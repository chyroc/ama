// The ama daemon's resident main loop: register → pick up tasks via some transport (WebSocket
// first, falling back to HTTP long polling on failure) → run them locally → return results in
// batches.
//
// It only ever makes outbound connections (the user's machine is behind NAT and has, and should
// have, no inbound ports):
//
//	register (user AMA_TOKEN, always HTTP) → get host_token, save to ~/.ama/host.json
//	transport (state machine in daemon_supervisor.go):
//	  WebSocket: GET /api/host-daemon/ws, 20s hello + proactive downstream push
//	  HTTP long polling: POST /heartbeat every 20s, POST /results
//
// This file only holds the transport-independent parts: local execution (dispatch), the three
// upstream buffers and batching, and the exec dedup ledger. The process is usually hosted by launchd
// (macOS) / systemd --user (Linux); see daemon_serve_*.go.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/chyroc/ama/server/shared/hostproto"
	"github.com/chyroc/ama/server/shared/hosttools"
)

const (
	// resultsFlushInterval: the interval for batching terminal output reports. exec results do not
	// wait for it (they go out with the flush right after a batch of calls is processed).
	resultsFlushInterval = 200 * time.Millisecond
	// httpHeartbeatRetry is the retry pace after one HTTP long polling tick fails (exactly as in the old version).
	httpHeartbeatRetry = 5 * time.Second
	// daemonHTTPTimeout is for ordinary POSTs (results); heartbeat long polling and WS get no overall
	// timeout (the server always replies within 20s).
	daemonHTTPTimeout = 30 * time.Second
)

// resultsRetry is the retry pace after sending a frame/batch of results fails.
var resultsRetry = 3 * time.Second

// resultRetryOverride: when 0, resultsRetry is used; tests inject a shorter value instead of
// changing the global variable (the flush goroutine reading it concurrently with tests would be a
// data race).
func (rt *daemonRuntime) resultRetry() time.Duration {
	if rt.resultRetryOverride > 0 {
		return rt.resultRetryOverride
	}
	return resultsRetry
}

// execFnT is the shape of the function that runs one exec locally; defaults to execLocal, replaceable in tests.
type execFnT func(ctx context.Context, spec hostproto.ExecSpec) hostproto.ExecResult

// toolFnT is the shape of the function that runs one KindTool locally; defaults to a local wrapper
// of hosttools.ExecuteTool (cwd anchored to $HOME), replaceable in tests.
type toolFnT func(ctx context.Context, cwd string, call hosttools.ToolCall) hosttools.ToolResult

// daemonRuntime is all of the daemon's runtime state.
//
// The three buffers (exec/frames/acks) and execution logic belong to no transport: when a WS ↔ HTTP
// switch happens in the middle of a 120s exec, execution is not interrupted, and the result stays in
// the buffer to be sent after the switch.
type daemonRuntime struct {
	baseURL string
	cfg     *daemonConfig
	http    *http.Client
	terms   *termManager
	ledger  *execLedger

	// execFn/toolFn run locally by default; tests inject fake executors.
	execFn execFnT
	toolFn toolFnT
	// resultRetryOverride: see resultRetry().
	resultRetryOverride time.Duration

	// The current active transport (WS or HTTP), read by submitResults.
	activeMu sync.RWMutex
	active   transport

	// Exec results and terminal frames/acks waiting to be reported; the flush loop sends them periodically.
	mu          sync.Mutex
	exec        []hostproto.ExecResultItem
	toolResults []hostproto.ToolResultItem
	frames      []hostproto.TermFrame
	acks        []int64
}

// daemonRun is the entry of `ama daemon run`.
func daemonRun(foregroundDebug bool) error {
	cfg, err := loadDaemonConfig()
	if err != nil && !isNotExist(err) {
		return err
	}
	if isNotExist(err) || cfg.Token == "" || cfg.HostID == "" {
		return errors.New("这台机器还没注册：请运行 `AMA_TOKEN=amak_xxx ama daemon install`（注册并装成系统服务），或 `ama daemon register` 后再 run")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}

	// Service mode (the launchd/systemd service set up by install) points AMA_DAEMON_LOG_FILE at
	// /tmp/ama-daemon.log; then the standard logger is connected to a size-rotated file, so the logs
	// of a long-running service do not grow forever. A foreground `ama daemon run` does not set this
	// variable, and logs go straight to the terminal/stdout.
	if logFile := strings.TrimSpace(os.Getenv(envDaemonLogFile)); logFile != "" {
		w, werr := newRotatingLogger(logFile, daemonLogMaxBytes, daemonLogKeep)
		if werr != nil {
			fmt.Fprintln(os.Stderr, "打开 daemon 日志文件失败，回退到标准输出: "+werr.Error())
		} else {
			log.SetOutput(w)
		}
	}

	rt := &daemonRuntime{
		baseURL: normalizeBaseURL(cfg.BaseURL),
		cfg:     &cfg,
		http:    newDaemonHTTPClient(),
		ledger:  newExecLedger(),
		execFn:  execLocal,
		toolFn:  executeToolLocal,
	}
	rt.terms = newTermManager(rt.enqueueFrame)

	// Adopt background jobs left by the previous daemon generation: their *.pid files are still in
	// /tmp/ama-bg and the processes may still be alive; add a reaping loop so exit codes still land in
	// status.
	if n := hosttools.AdoptBackgroundJobs(); n > 0 {
		log.Printf("收养了 %d 个上一代 daemon 留下的后台任务", n)
	}

	log.Printf("ama daemon 启动：主机 %s（%s/%s），服务端 %s",
		cfg.HostID, runtime.GOOS, runtime.GOARCH, rt.baseURL)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	inbound := make(chan inboundBatch, inboundQueueBatches)
	go rt.flushLoop(ctx)
	workerDone := make(chan struct{})
	go func() {
		rt.workerLoop(ctx, inbound)
		close(workerDone)
	}()

	supervisorErr := rt.runSupervisor(ctx, inbound)

	// Exit: wait for the worker to finish the last batch, close all terminals (readLoop produces the
	// last batch of closed frames), then try to send the buffers within a 10s budget independent of
	// the signal ctx.
	stop()
	<-workerDone
	rt.terms.closeAll()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rt.flushOnce(shutdownCtx)

	// Host revoked / token invalid: wipe the local credential and exit, leaving it to the user to
	// register again (HostUUID is deliberately kept, so re-registering still hits the same machine).
	if errors.Is(supervisorErr, errDaemonUnauthorized) {
		log.Printf("服务端拒绝凭据（401）：宿主机可能已被吊销。请重新运行 ama daemon register")
		_ = cfg.wipeToken()
	}
	log.Printf("ama daemon 已退出")
	return supervisorErr
}

var errDaemonUnauthorized = errors.New("unauthorized")

// heartbeatRequest builds the payload of one heartbeat tick / one hello frame (HTTP and WS share the same shape).
func (rt *daemonRuntime) heartbeatRequest() hostproto.HeartbeatRequest {
	return hostproto.HeartbeatRequest{
		HostID:        rt.cfg.HostID,
		LastSeq:       rt.cfg.LastSeq,
		Name:          hostname(),
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		DaemonVersion: version,
		// The new daemon always supports the native tool protocol (no longer depends on brew GNU tools).
		NativeTools: boolPtr(true),
	}
}

// handleBatch processes a batch of calls serially, advances LastSeq, and flushes results once right
// away. The worker is the only caller and the only one advancing the cursor, at the same pace as the
// old "fetch the next round only after processing".
func (rt *daemonRuntime) handleBatch(ctx context.Context, calls []hostproto.Dispatch) {
	var maxSeq int64 = rt.cfg.LastSeq
	for _, call := range calls {
		rt.dispatch(ctx, call)
		if call.Seq > maxSeq {
			maxSeq = call.Seq
		}
	}
	if maxSeq != rt.cfg.LastSeq {
		rt.cfg.LastSeq = maxSeq
		if err := rt.cfg.save(); err != nil {
			log.Printf("保存投递游标失败：%v", err)
		}
	}
	rt.flushOnce(ctx)
}

// dispatch handles one downstream action. exec runs synchronously (serially, matching the serial
// semantics of agent tools); terminal frames are handed to termManager to live asynchronously, and
// the seq is recorded as pending ack.
func (rt *daemonRuntime) dispatch(ctx context.Context, call hostproto.Dispatch) {
	switch call.Kind {
	case hostproto.KindExec:
		var spec hostproto.ExecSpec
		if err := json.Unmarshal(call.Payload, &spec); err != nil {
			rt.enqueueExec(call.ID, hostproto.ExecResult{Err: "bad_spec", Stderr: "下发的执行参数无法解析: " + err.Error()})
			return
		}
		// Redelivery after lease expiry: this call already finished on this machine and its result is
		// still in the ledger, so return the cache directly and never rerun a script with side effects
		// (rerunning after the ledger is lost on restart is the existing fallback semantics).
		if cached, ok := rt.ledger.lookup(call.ID); ok {
			rt.enqueueExecRaw(call.ID, cached)
			return
		}
		res := rt.execFn(ctx, spec)
		raw, _ := json.Marshal(res)
		rt.ledger.remember(call.ID, raw)
		rt.enqueueExec(call.ID, res)

	case hostproto.KindTool:
		var tc hosttools.ToolCall
		if err := json.Unmarshal(call.Payload, &tc); err != nil {
			rt.enqueueTool(call.ID, hosttools.ToolResult{
				IsError: true, Text: "下发的工具调用无法解析: " + err.Error()})
			return
		}
		if cached, ok := rt.ledger.lookup(call.ID); ok {
			rt.enqueueToolRaw(call.ID, cached)
			return
		}
		// cwd is anchored relative to $HOME (.ama/runs/<sid>), and the directory is ensured to exist.
		// First check sessionID has no path traversal, so cwd cannot escape ~/.ama/runs.
		if !validSessionID(call.SessionID) {
			rt.enqueueTool(call.ID, hosttools.ToolResult{
				IsError: true, Text: "非法的会话 id（含路径分隔符或越界片段），拒绝执行"})
			return
		}
		cwd := resolveCwd(hostWorkdirRel(call.SessionID))
		res := rt.toolFn(ctx, cwd, tc)
		raw, _ := hosttools.MarshalResult(res)
		rt.ledger.remember(call.ID, raw)
		rt.enqueueTool(call.ID, res)

	case hostproto.KindTermOpen:
		// Redelivered (30s term lease expired) and the PTY is still alive: only send the ack again, do not kill and reopen.
		if rt.terms.has(call.TerminalID) {
			rt.enqueueAck(call.Seq)
			return
		}
		rt.terms.open(call.TerminalID, call.Payload)
		rt.enqueueAck(call.Seq)
	case hostproto.KindTermInput:
		rt.terms.input(call.TerminalID, call.Payload)
		rt.enqueueAck(call.Seq)
	case hostproto.KindTermResize:
		rt.terms.resize(call.TerminalID, call.Payload)
		rt.enqueueAck(call.Seq)
	case hostproto.KindTermClose:
		rt.terms.closeID(call.TerminalID, "browser_closed")
		rt.enqueueAck(call.Seq)
	default:
		log.Printf("收到未知类型的任务 %q（seq=%d），已忽略", call.Kind, call.Seq)
	}
}

// -- Active transport ---------------------------------------------------------------

func (rt *daemonRuntime) setActive(t transport) {
	rt.activeMu.Lock()
	rt.active = t
	rt.activeMu.Unlock()
}

// clearActiveIf only clears when the current active transport is still t (cleanup after a disconnect must not knock out a new connection).
func (rt *daemonRuntime) clearActiveIf(t transport) {
	rt.activeMu.Lock()
	if rt.active == t {
		rt.active = nil
	}
	rt.activeMu.Unlock()
}

// -- Pending report buffers -------------------------------------------------------------

func (rt *daemonRuntime) enqueueExec(callID string, res hostproto.ExecResult) {
	raw, _ := json.Marshal(res)
	rt.enqueueExecRaw(callID, raw)
}

// enqueueExecRaw enqueues one already-serialized exec result (used for ledger redelivery).
func (rt *daemonRuntime) enqueueExecRaw(callID string, raw []byte) {
	var res hostproto.ExecResult
	_ = json.Unmarshal(raw, &res)
	rt.mu.Lock()
	rt.exec = append(rt.exec, hostproto.ExecResultItem{CallID: callID, Result: res})
	rt.mu.Unlock()
}

func (rt *daemonRuntime) enqueueTool(callID string, res hosttools.ToolResult) {
	raw, _ := json.Marshal(res)
	rt.enqueueToolRaw(callID, raw)
}

// enqueueToolRaw enqueues one already-serialized tool result (used for ledger redelivery).
func (rt *daemonRuntime) enqueueToolRaw(callID string, raw []byte) {
	var res hosttools.ToolResult
	_ = json.Unmarshal(raw, &res)
	rt.mu.Lock()
	rt.toolResults = append(rt.toolResults, hostproto.ToolResultItem{CallID: callID, Result: res})
	rt.mu.Unlock()
}
func (rt *daemonRuntime) enqueueFrame(f hostproto.TermFrame) {
	rt.mu.Lock()
	rt.frames = append(rt.frames, f)
	rt.mu.Unlock()
}
func (rt *daemonRuntime) enqueueAck(seq int64) {
	rt.mu.Lock()
	rt.acks = append(rt.acks, seq)
	rt.mu.Unlock()
}

func (rt *daemonRuntime) flushLoop(ctx context.Context) {
	t := time.NewTicker(resultsFlushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			rt.flushOnce(ctx)
		}
	}
}

// takePending takes and clears the four buffers.
func (rt *daemonRuntime) takePending() (
	exec []hostproto.ExecResultItem, tools []hostproto.ToolResultItem,
	frames []hostproto.TermFrame, acks []int64,
) {
	rt.mu.Lock()
	exec, tools, frames, acks = rt.exec, rt.toolResults, rt.frames, rt.acks
	rt.exec, rt.toolResults, rt.frames, rt.acks = nil, nil, nil, nil
	rt.mu.Unlock()
	return
}

// restorePending puts the data back at the head when sending fails (order matters: what failed first must be retried first).
func (rt *daemonRuntime) restorePending(
	exec []hostproto.ExecResultItem, tools []hostproto.ToolResultItem,
	frames []hostproto.TermFrame, acks []int64,
) {
	rt.mu.Lock()
	rt.exec = append(exec, rt.exec...)
	rt.toolResults = append(tools, rt.toolResults...)
	rt.frames = append(frames, rt.frames...)
	rt.acks = append(acks, rt.acks...)
	rt.mu.Unlock()
}

// flushOnce splits the current buffers into several frames/requests by 4MiB and sends them in order;
// the first failed block and all remaining blocks go back to the head of the queue for retry next
// round. exec is strongly reliable: only after a successful ACK is it forgotten in the dedup ledger.
func (rt *daemonRuntime) flushOnce(ctx context.Context) {
	exec, tools, frames, acks := rt.takePending()
	if len(exec) == 0 && len(tools) == 0 && len(frames) == 0 && len(acks) == 0 {
		return
	}
	chunks := splitResults(exec, tools, frames, acks, wsResultsMaxBytes)

	sent := 0
	for ; sent < len(chunks); sent++ {
		if err := rt.submitChunk(ctx, chunks[sent]); err != nil {
			break
		}
	}
	if sent == len(chunks) {
		return
	}

	// There is a failed block: the failed block + all unsent blocks go back.
	restoreExec := chunks[sent].ExecResults
	for i := sent + 1; i < len(chunks); i++ {
		restoreExec = append(restoreExec, chunks[i].ExecResults...)
	}
	restoreTools := chunks[sent].ToolResults
	for i := sent + 1; i < len(chunks); i++ {
		restoreTools = append(restoreTools, chunks[i].ToolResults...)
	}
	restoreFrames := chunks[sent].TermFrames
	for i := sent + 1; i < len(chunks); i++ {
		restoreFrames = append(restoreFrames, chunks[i].TermFrames...)
	}
	restoreAcks := chunks[sent].AckSeqs
	for i := sent + 1; i < len(chunks); i++ {
		restoreAcks = append(restoreAcks, chunks[i].AckSeqs...)
	}
	rt.restorePending(restoreExec, restoreTools, restoreFrames, restoreAcks)

	if ctx.Err() == nil {
		log.Printf("结果发送失败，%s 后重试", rt.resultRetry())
		time.Sleep(rt.resultRetry())
	}
}

// submitChunk sends one block over the current active transport; when WS is half-dead (write failed
// but the supervisor has not switched yet) or during shutdown cleanup, stateless HTTP is used once as
// a fallback. On success the callIDs are forgotten in the ledger.
func (rt *daemonRuntime) submitChunk(ctx context.Context, req hostproto.ResultsRequest) error {
	rt.activeMu.RLock()
	t := rt.active
	rt.activeMu.RUnlock()

	err := errNoActiveTransport
	if t != nil {
		err = t.submitResults(ctx, req)
	}
	// Only use stateless HTTP once as a fallback when "there is no active transport" or "WS failed at
	// the connection level (disconnected/half-dead dial)". An application-level ok=false
	// (errWSResultNack) means the connection is alive and the server only failed temporarily; then
	// retry over the same connection, and do not switch paths and send twice.
	needHTTPFallback := t == nil ||
		(t.name() == transportNameWS && !errors.Is(err, errWSResultNack))
	if err != nil && needHTTPFallback {
		if fbErr := newHTTPWire(rt).submitResults(ctx, req); fbErr == nil {
			err = nil
		}
	}
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(req.ExecResults)+len(req.ToolResults))
	for _, item := range req.ExecResults {
		ids = append(ids, item.CallID)
	}
	for _, item := range req.ToolResults {
		ids = append(ids, item.CallID)
	}
	rt.ledger.forget(ids...)
	return nil
}

var errNoActiveTransport = errors.New("当前没有活动传输")

// -- HTTP basics ---------------------------------------------------------------

func newDaemonHTTPClient() *http.Client {
	// Deliberately cloned from the default Transport (which comes with ProxyFromEnvironment; company
	// networks/local proxies rely on it): do not do `c := *http.DefaultTransport.(*http.Transport)`,
	// which would copy the internal Mutex too, and go vet would flag it. Clone is the official safe
	// way. Long polling sets no overall client timeout (the server always replies within 20s), only
	// fallbacks for connecting/TLS.
	return &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}
}

// postJSON sends JSON and receives JSON. With longPoll=true there is no overall request timeout (the
// heartbeat must hold for 20s). When out is nil the response body is not parsed.
func (rt *daemonRuntime) postJSON(
	ctx context.Context, method, path, token string, body any, out any, longPoll bool,
) (int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rdr = bytes.NewReader(b)
	}
	reqCtx := ctx
	if !longPoll {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(ctx, daemonHTTPTimeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(reqCtx, method, rt.baseURL+path, rdr)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := rt.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("解析响应失败: %w", err)
		}
	}
	return resp.StatusCode, nil
}

func hostname() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "unknown"
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// normalizeBaseURL tidies the server address (strip the trailing slash, add https).
func normalizeBaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = defaultBaseURL
	}
	raw = strings.TrimRight(raw, "/")
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		raw = "https://" + raw
	}
	return raw
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// newRawRequest builds a JSON request with Bearer, reused by registration (with the user's AMA_TOKEN).
func newRawRequest(ctx context.Context, method, url string, body []byte, token string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return req, nil
}

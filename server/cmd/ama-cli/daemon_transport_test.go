// The first unit tests of the daemon transport layer: pure functions (URL derivation/framing/
// ledger) + runs against httptest fake servers covering the WS happy path, 404 fallback to HTTP, 401
// exit, and results_ack failure retry.
//
// ⚠️ These tests must run on darwin/linux (termManager has a build tag); CI's go test ./... on linux
// covers them naturally; on other platforms this file is excluded by the build constraint, hence the
// same tag in the file header.
//
//go:build darwin || linux

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chyroc/ama/server/shared/hostproto"
	"github.com/chyroc/ama/server/shared/hosttools"
	"github.com/gorilla/websocket"
)

// -- Pure functions ------------------------------------------------------------------

func TestDeriveWSBaseURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://ama.example.com:8443/", "wss://ama.example.com:8443/api/host-daemon/ws"},
		{"http://127.0.0.1:5273", "ws://127.0.0.1:5273/api/host-daemon/ws"},
		{"https://ama.example.com", "wss://ama.example.com/api/host-daemon/ws"},
		{"ama.example.com", "wss://ama.example.com/api/host-daemon/ws"},
	}
	for _, tc := range cases {
		if got := deriveWSBaseURL(tc.in); got != tc.want {
			t.Errorf("deriveWSBaseURL(%q) = %q，want %q", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeTransport(t *testing.T) {
	cases := map[string]string{
		"": "auto", "AUTO": "auto", "ws": "ws", "HTTP": "http", "xxx": "auto",
	}
	for in, want := range cases {
		if got := normalizeTransport(in); got != want {
			t.Errorf("normalizeTransport(%q) = %q，want %q", in, got, want)
		}
	}
}

func TestSplitResultsBoundary(t *testing.T) {
	exec := []hostproto.ExecResultItem{
		{CallID: "a", Result: hostproto.ExecResult{Stdout: strings.Repeat("x", 3000)}},
		{CallID: "b", Result: hostproto.ExecResult{Stdout: strings.Repeat("y", 3000)}},
	}
	chunks := splitResults(exec, nil, nil, []int64{1, 2}, 4000)
	if len(chunks) < 2 {
		t.Fatalf("3000B 两条按 4000B 预算应切成至少 2 块，实际 %d", len(chunks))
	}
	// acks should only appear in the last block.
	for i, c := range chunks {
		if len(c.AckSeqs) != 0 && i != len(chunks)-1 {
			t.Fatalf("acks 只能在最后一块，第 %d 块却带了 acks", i)
		}
	}
	// A single item over budget also gets a block to itself (not split).
	big := []hostproto.ExecResultItem{{CallID: "big", Result: hostproto.ExecResult{Stdout: strings.Repeat("z", 9000)}}}
	if chunks := splitResults(big, nil, nil, nil, 4000); len(chunks) != 1 || chunks[0].ExecResults[0].CallID != "big" {
		t.Fatalf("超预算单条应独占一块，实际 %+v", chunks)
	}
}

func TestExecLedgerRememberLookupForget(t *testing.T) {
	l := newExecLedger()
	raw1, _ := json.Marshal(hostproto.ExecResult{ExitCode: intPtrTest(0), Stdout: "ok"})
	l.remember("c1", raw1)
	if got, ok := l.lookup("c1"); !ok || !strings.Contains(string(got), "ok") {
		t.Fatalf("lookup c1 失败: %s %v", got, ok)
	}
	raw2, _ := json.Marshal(hostproto.ExecResult{Stdout: "second"})
	l.remember("c1", raw2)
	if got, _ := l.lookup("c1"); strings.Contains(string(got), "second") {
		t.Fatal("重复 remember 不应覆盖第一次结果")
	}
	l.forget("c1")
	if _, ok := l.lookup("c1"); ok {
		t.Fatal("forget 后不应再查到")
	}
}

func TestExecLedgerEviction(t *testing.T) {
	old := ledgerMaxItems
	ledgerMaxItems = 3
	defer func() { ledgerMaxItems = old }()

	l := newExecLedger()
	// Four come in with capacity 3: the earliest, c1, is evicted when c4 is recorded.
	for _, id := range []string{"c1", "c2", "c3", "c4"} {
		raw, _ := json.Marshal(hostproto.ExecResult{Stdout: "x"})
		l.remember(id, raw)
	}
	if _, ok := l.lookup("c1"); ok {
		t.Fatal("超容量后最旧的 c1 应被淘汰")
	}
	if _, ok := l.lookup("c4"); !ok {
		t.Fatal("新记录 c4 应在")
	}
	// After forget leaves a hole in order, the next eviction still takes the real oldest (c2) instead of hitting the hole.
	l.forget("c4")
	raw, _ := json.Marshal(hostproto.ExecResult{Stdout: "x"})
	l.remember("c5", raw)
	l.remember("c6", raw)
	if _, ok := l.lookup("c2"); ok {
		t.Fatal("空洞压缩后继续超容量，c2 应被淘汰")
	}
	if _, ok := l.lookup("c6"); !ok {
		t.Fatal("新记录 c6 应在")
	}
}

func intPtrTest(i int) *int { return &i }

// -- Integration fixtures ----------------------------------------------------------------

// noProxyEnv keeps localhost dials from going through a company proxy the dev machine may have configured.
func noProxyEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
		t.Setenv(k, "")
	}
	t.Setenv("NO_PROXY", "127.0.0.1,localhost")
	t.Setenv("no_proxy", "127.0.0.1,localhost")
}

type fakeServers struct {
	http *httptest.Server
	ws   *httptest.Server

	mu            sync.Mutex
	httpHeartbeat int
	resultsFrames []hostproto.WSFrame
	connects      int
	conns         []*websocket.Conn
}

// dropFirstConn closes the first WS connection from the server side (simulating a server
// disconnect). Note httptest.Server.CloseClientConnections cannot be used: it has no effect on
// hijacked WebSocket connections (measured: the client did not notice after reading for 2s).
func (fs *fakeServers) dropFirstConn() {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.conns) > 0 {
		_ = fs.conns[0].Close()
	}
}

func newRuntimeForTransportTest(t *testing.T) (*daemonRuntime, *fakeServers) {
	t.Helper()
	noProxyEnv(t)
	t.Setenv("HOME", t.TempDir())

	fs := &fakeServers{}

	// HTTP fake server: heartbeats return empty immediately, results return 204.
	httpMux := http.NewServeMux()
	httpMux.HandleFunc("/api/host-daemon/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		fs.mu.Lock()
		fs.httpHeartbeat++
		fs.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(hostproto.HeartbeatResponse{})
	})
	httpMux.HandleFunc("/api/host-daemon/results", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	fs.http = httptest.NewServer(httpMux)
	t.Cleanup(fs.http.Close)

	cfg := daemonConfig{
		HostUUID: "uuid-test", HostID: "hst_test", Token: "good-token",
		BaseURL: fs.http.URL,
	}
	rt := &daemonRuntime{
		baseURL: normalizeBaseURL(fs.http.URL),
		cfg:     &cfg,
		http:    newDaemonHTTPClient(),
		ledger:  newExecLedger(),
		execFn: func(_ context.Context, spec hostproto.ExecSpec) hostproto.ExecResult {
			return hostproto.ExecResult{ExitCode: intPtrTest(0), Stdout: "executed:" + spec.Path}
		},
		toolFn: func(_ context.Context, _ string, call hosttools.ToolCall) hosttools.ToolResult {
			return hosttools.ToolResult{Text: "tool:" + call.Name}
		},
	}
	rt.terms = newTermManager(rt.enqueueFrame)
	return rt, fs
}

// startWSFake starts the WS fake server. firstCalls are the calls carried by the first hello_ack of
// each new connection; nackFirst makes the first N results frames reply ok=false (to test retry);
// with rejectStatus>0 it does not upgrade and replies with that HTTP status directly (to test the
// 401/404 handshake classification).
func (fs *fakeServers) startWSFake(t *testing.T, firstCalls []hostproto.Dispatch, nackFirst int, rejectStatus int) {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rejectStatus > 0 {
			w.WriteHeader(rejectStatus)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		fs.mu.Lock()
		fs.connects++
		fs.conns = append(fs.conns, conn)
		nack := nackFirst
		fs.mu.Unlock()

		var hello hostproto.WSFrame
		if err := conn.ReadJSON(&hello); err != nil || hello.Type != hostproto.WSFrameHello {
			return
		}
		_ = conn.WriteJSON(hostproto.WSFrame{Type: hostproto.WSFrameHelloAck, V: 1, Calls: firstCalls})
		for {
			var frame hostproto.WSFrame
			if err := conn.ReadJSON(&frame); err != nil {
				return
			}
			if frame.Type != hostproto.WSFrameResults {
				continue
			}
			fs.mu.Lock()
			fs.resultsFrames = append(fs.resultsFrames, frame)
			ok := true
			if nack > 0 {
				ok = false
				nack--
			}
			fs.mu.Unlock()
			ack := hostproto.WSFrame{Type: hostproto.WSFrameResultsAck, V: 1, ID: frame.ID, OK: ok}
			if !ok {
				ack.Error = "fake nack"
			}
			if err := conn.WriteJSON(ack); err != nil {
				return
			}
		}
	}))
	fs.ws = srv
	t.Cleanup(srv.Close)
}

func (fs *fakeServers) useWSURL(t *testing.T, rt *daemonRuntime) {
	t.Helper()
	t.Setenv(envWSBaseURL, "ws"+strings.TrimPrefix(fs.ws.URL, "http"))
}

func execDispatch(seq int64) hostproto.Dispatch {
	return hostproto.Dispatch{
		ID: "htc_" + strings.Repeat("z", int(seq)), Seq: seq, Kind: hostproto.KindExec,
		SessionID: "run_1", Payload: json.RawMessage(`{"path":"/bin/true"}`),
	}
}

// Wait until cond holds (polling, to avoid fine-grained synchronization in tests).
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("条件等待超时")
}

// startSupervisorWithWorker starts the supervisor and also the single dispatch worker (that is how
// the production path in daemonRun assembles it): without a worker, after WS receives calls nobody
// runs them, advances the cursor, or flushes.
func startSupervisorWithWorker(t *testing.T, rt *daemonRuntime) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	in := make(chan inboundBatch, inboundQueueBatches)
	go rt.flushLoop(ctx)
	go rt.workerLoop(ctx, in)
	go func() { _ = rt.runSupervisor(ctx, in) }()
	return cancel
}

// -- Integration cases ----------------------------------------------------------------

// WS happy path: the first frame carries an exec → it runs locally → the result comes back in a
// results frame, with zero HTTP heartbeat hits; LastSeq advances (asserted on the in-memory cfg).
func TestDaemonSupervisorWSHappyPath(t *testing.T) {
	rt, fs := newRuntimeForTransportTest(t)
	fs.startWSFake(t, []hostproto.Dispatch{execDispatch(1)}, 0, 0)
	fs.useWSURL(t, rt)

	cancel := startSupervisorWithWorker(t, rt)
	defer cancel()

	waitFor(t, 3*time.Second, func() bool {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return len(fs.resultsFrames) >= 1
	})
	cancel()

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.httpHeartbeat != 0 {
		t.Fatalf("WS 正常时不应有 HTTP 心跳，实际 %d 次", fs.httpHeartbeat)
	}
	frame := fs.resultsFrames[0]
	if frame.Results == nil || len(frame.Results.ExecResults) != 1 ||
		frame.Results.ExecResults[0].CallID != "htc_z" {
		t.Fatalf("results 帧内容不对: %+v", frame)
	}
	if got := rt.cfg.LastSeq; got != 1 {
		t.Fatalf("LastSeq 应为 1，实际 %d", got)
	}
}

// WS handshake 404: go straight into the HTTP fallback, with heartbeats hitting the HTTP fake server.
func TestDaemonSupervisorWS404FallsBackToHTTP(t *testing.T) {
	rt, fs := newRuntimeForTransportTest(t)
	fs.startWSFake(t, nil, 0, http.StatusNotFound)
	fs.useWSURL(t, rt)
	unsupportedReprobeWait = time.Hour // this case does not care about re-probing
	t.Cleanup(func() { unsupportedReprobeWait = 30 * time.Minute })

	cancel := startSupervisorWithWorker(t, rt)
	defer cancel()

	waitFor(t, 3*time.Second, func() bool {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return fs.httpHeartbeat > 0
	})
}

// WS handshake 401: the supervisor returns errDaemonUnauthorized (daemonRun wipes credentials based on it).
func TestDaemonSupervisorWS401Unauthorized(t *testing.T) {
	rt, fs := newRuntimeForTransportTest(t)
	fs.startWSFake(t, nil, 0, http.StatusUnauthorized)
	fs.useWSURL(t, rt)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := rt.runSupervisor(ctx, make(chan inboundBatch, 8))
	if err != errDaemonUnauthorized {
		t.Fatalf("401 应返回 errDaemonUnauthorized，实际 %v", err)
	}
}

// results_ack ok=false: the same exec result is resent, and the second ack succeeds.
func TestDaemonResultsNACKRetried(t *testing.T) {
	rt, fs := newRuntimeForTransportTest(t)
	rt.resultRetryOverride = 20 * time.Millisecond
	fs.startWSFake(t, []hostproto.Dispatch{execDispatch(1)}, 1, 0)
	fs.useWSURL(t, rt)

	cancel := startSupervisorWithWorker(t, rt)
	defer cancel()

	waitFor(t, 3*time.Second, func() bool {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return len(fs.resultsFrames) >= 2
	})
	cancel()

	fs.mu.Lock()
	defer fs.mu.Unlock()
	id0 := fs.resultsFrames[0].Results.ExecResults[0].CallID
	id1 := fs.resultsFrames[1].Results.ExecResults[0].CallID
	if id0 != id1 {
		t.Fatalf("重发应是同一个 callID：%q vs %q", id0, id1)
	}
}

// The server drops the WS connection after it is established: reconnect automatically within budget (the fake server accepts a second connection).
func TestDaemonWSReconnectsAfterDrop(t *testing.T) {
	oldBudget := wsFailBudget
	oldBackoff := wsBackoffInitial
	oldMax := wsBackoffMax
	wsFailBudget = 5
	wsBackoffInitial = 10 * time.Millisecond
	wsBackoffMax = 20 * time.Millisecond
	defer func() {
		wsFailBudget, wsBackoffInitial, wsBackoffMax = oldBudget, oldBackoff, oldMax
	}()

	rt, fs := newRuntimeForTransportTest(t)
	fs.startWSFake(t, nil, 0, 0)
	fs.useWSURL(t, rt)

	cancel := startSupervisorWithWorker(t, rt)
	defer cancel()

	waitFor(t, 3*time.Second, func() bool {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return fs.connects >= 1
	})
	fs.dropFirstConn() // the server drops the first connection from the inside
	waitFor(t, 3*time.Second, func() bool {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return fs.connects >= 2
	})
}

// HTTP-only escape hatch: with AMA_DAEMON_TRANSPORT=http not a single WS dial is made.
func TestDaemonHTTPOnlyNeverDialsWS(t *testing.T) {
	rt, fs := newRuntimeForTransportTest(t)
	fs.startWSFake(t, nil, 0, 0)
	fs.useWSURL(t, rt)
	t.Setenv(envDaemonTransport, "http")

	cancel := startSupervisorWithWorker(t, rt)
	defer cancel()

	waitFor(t, 3*time.Second, func() bool {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return fs.httpHeartbeat >= 2
	})
	cancel()
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.connects != 0 {
		t.Fatalf("http-only 模式不应拨 WS，实际连接 %d 次", fs.connects)
	}
}

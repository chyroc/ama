package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/shared/hostproto"
)

// Timeout model for waiting on the daemon's exec result:
//
//	daemon process timeout = spec.TimeoutMS (six-tool path ≤120s; the SDK tool ceiling is also 120s)
//	the outer wait adds execGrace: margin for enqueue → pulled by long polling → run → POST result, plus process group cleanup
//	hard cap execWaitCap: never exceeded in any case; it must be less than workerfailover's 180s
//	(that watchdog hands the whole work back to Ark for redispatch after 180s without a tool result, so we must return first).
const (
	execGrace   = 15 * time.Second
	execWaitCap = 130 * time.Second
	// execPollTick is the fallback database polling interval when NOTIFY is lost.
	execPollTick = 1 * time.Second
)

// Exec runs one tool process call on the given host, blocking until the daemon returns a result / the host goes offline / timeout.
//
// The returned ExecResult, even when it "did not run" (offline, missing GNU tools, wait timeout), is
// a normal result + an Err field, not a Go error: by protocol a tool failure is an IsError result, and a Go error
// is only for "the call itself is invalid" (host does not exist, bad arguments). The core layer translates based on this.
func (s *Service) Exec(
	ctx context.Context, hostID, sessionID string, spec hostproto.ExecSpec,
) (hostproto.ExecResult, error) {
	h, err := s.store.GetHost(ctx, hostID)
	if err != nil {
		return hostproto.ExecResult{}, fmt.Errorf("host: 取宿主机失败: %w", err)
	}

	// Pre-check 1: online. last_seen_at is the time-based check, not just the status column: status is
	// refreshed periodically by the watcher, so for a few seconds after heartbeats stop the column has not flipped, but past the grace period the host is unusable.
	if !hostSeenRecently(h, timeNow()) {
		return offlineResult("宿主机离线（心跳超时），工具未执行或执行结果未知；请确认 ama daemon 正在运行"), nil
	}
	// Pre-check 2 was removed: the old protocol relied on the GNU tools gate; the new protocol (KindTool) looks at NativeTools,
	// see tool.go. KindExec can only be sent by an old server; the current code no longer takes that path.

	payload, err := json.Marshal(spec)
	if err != nil {
		return hostproto.ExecResult{}, fmt.Errorf("host: 序列化执行参数失败: %w", err)
	}
	d := persistmodel.HostDispatch{
		ID:        newID(idPrefixCall),
		HostID:    hostID,
		Kind:      hostproto.KindExec,
		State:     "queued",
		SessionID: sessionID,
		Payload:   string(payload),
		CreatedAt: timeNow(),
		UpdatedAt: timeNow(),
	}
	if _, err := s.store.EnqueueDispatch(ctx, d); err != nil {
		return failedResult(hostproto.ExecErrEnqueue, "任务下发到宿主机失败（队列写入错误）"), nil
	}

	settled := s.awaitSettlement(ctx, h.ID, d.ID, spec.TimeoutMS)
	if !settled.done {
		return failedResult(settled.code, settled.message), nil
	}
	var res hostproto.ExecResult
	if jerr := json.Unmarshal([]byte(settled.result), &res); jerr != nil {
		return failedResult("bad_result", "宿主机回传的结果无法解析: "+jerr.Error()), nil
	}
	return res, nil
}

// EnqueueTermFrame sends one web terminal action (used by core/hostterm).
// Terminal frames are weakly reliable: the claimed lease is short (the Store gives term 30s), and no result is awaited.
func (s *Service) EnqueueTermFrame(
	ctx context.Context, hostID, sessionID, terminalID, kind string, payload []byte,
) error {
	d := persistmodel.HostDispatch{
		ID:         newID(idPrefixCall),
		HostID:     hostID,
		Kind:       kind,
		State:      "queued",
		SessionID:  sessionID,
		TerminalID: terminalID,
		Payload:    string(payload),
		CreatedAt:  timeNow(),
		UpdatedAt:  timeNow(),
	}
	if _, err := s.store.EnqueueDispatch(ctx, d); err != nil {
		return fmt.Errorf("host: 终端帧下发失败: %w", err)
	}
	return nil
}

// settlement is the outcome a dispatch waited for: when done=true, result is the raw result JSON returned by the daemon
// (Exec decodes it as ExecResult, ToolCall as hosttools.ToolResult);
// when done=false, code/message are the structured error of a server-side pre-flight failure.
type settlement struct {
	done    bool
	result  string
	code    string
	message string
}

// awaitSettlement polls the result of one dispatch; shared by Exec and ToolCall.
// NOTIFY gives low latency, the 1s ticker is the fallback, and ctx cancellation (work failover / request end) returns immediately.
//
// When timeoutMS>0 the wait budget is narrowed to "process timeout + execGrace"; 0 means the caller did not set a
// process timeout (true for native tool internal operations), so keep the execWaitCap hard cap, instead of degrading to waiting only 15s.
func (s *Service) awaitSettlement(ctx context.Context, hostID, dispatchID string, timeoutMS int64) settlement {
	waitBudget := execWaitCap
	if timeoutMS > 0 {
		if d := time.Duration(timeoutMS)*time.Millisecond + execGrace; d > 0 && d < waitBudget {
			waitBudget = d
		}
	}
	deadline := timeNow().Add(waitBudget)

	for {
		row, err := s.store.GetDispatch(ctx, dispatchID)
		if err != nil && !errors.Is(err, persistmodel.ErrNotFound) {
			return settlement{code: hostproto.ExecErrEnqueue,
				message: "读取宿主机任务状态失败: " + err.Error()}
		}
		if err == nil && row.State == "done" && row.Result != "" {
			return settlement{done: true, result: row.Result}
		}

		now := timeNow()
		if !now.Before(deadline) {
			// Do not mark done locally: a late result from the daemon still has value (the ledger covers it on the next reclaim),
			// but if the host is offline right now, leave it to the watcher's failure result to express it uniformly.
			if h, gerr := s.store.GetHost(ctx, hostID); gerr == nil && !hostSeenRecently(h, now) {
				return settlement{code: hostproto.ExecErrHostOffline, message: "等待宿主机执行结果超时且主机已离线"}
			}
			return settlement{code: "exec_timeout",
				message: "宿主机执行超时（等待超过 " + waitBudget.String() + "）；命令可能仍在宿主机上运行"}
		}

		// Each round of Wait uses its own waitCtx: cancel when select ends (timer/wake/cancel),
		// unregistering this round's waiter at once. Otherwise, when NOTIFY does not come and only the 1s ticker is the fallback,
		// one waiter goroutine would leak per second (only released when the outer ctx ends).
		waitCtx, cancelWait := context.WithCancel(ctx)
		timer := time.NewTimer(execPollTick)
		var wake <-chan struct{}
		if s.wait != nil {
			wake = s.wait.Wait(waitCtx, hostID)
		}
		select {
		case <-ctx.Done():
			timer.Stop()
			cancelWait()
			// The work was failed over / cancelled: do not judge success or failure; leave the row to redispatch or the ledger, and exit directly.
			return settlement{code: "canceled", message: "等待宿主机结果被中断"}
		case <-timer.C:
			cancelWait()
		case <-wake:
			cancelWait()
		}
	}
}

func hostSeenRecently(h persistmodel.Host, now time.Time) bool {
	if h.LastSeenAt.IsZero() {
		return false
	}
	return now.Sub(h.LastSeenAt) <= onlineGrace
}

func offlineResult(message string) hostproto.ExecResult {
	return failedResult(hostproto.ExecErrHostOffline, message)
}

func failedResult(code, message string) hostproto.ExecResult {
	return hostproto.ExecResult{Err: code, Stderr: message}
}

// HostOnline lets the web terminal check liveness before opening a connection: online if last_seen_at is within the 90s grace.
func (s *Service) HostOnline(ctx context.Context, hostID string) (bool, error) {
	h, err := s.store.GetHost(ctx, hostID)
	if err != nil {
		return false, err
	}
	return hostSeenRecently(h, timeNow()), nil
}

// WorkspaceHostBound re-checks that the host is still bound to the given workspace (used before snapshotting when creating a session).
func (s *Service) WorkspaceHostBound(ctx context.Context, workspaceID, hostID string) (bool, error) {
	_, err := s.store.GetWorkspaceHost(ctx, workspaceID, hostID)
	if err == nil {
		return true, nil
	}
	if isHostNotFound(err) {
		return false, nil
	}
	return false, err
}

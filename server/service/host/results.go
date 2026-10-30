package host

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/chyroc/ama/server/shared/hostproto"
)

// SubmitResults handles one batch upload from the daemon.
//
// The two kinds of content mean different things (see the header of host_dispatches.sql):
//
//	exec results     idempotent CAS into the database (only updated when state<>'done'), with a NOTIFY in the same transaction to wake
//	                 the worker waiting for the tool result; a repeated POST (network retry) does not overwrite the first result.
//	                 Clearing claimed_until on completion is not needed for correctness: with state=done the row is no longer
//	                 matched by the claim condition.
//	terminal frames  never stored (high frequency, large, useless when late): handed straight to TermUpSink to relay to the instance holding
//	                 the browser connection; when sink is missing they are counted, dropped and logged, without reporting an error to the daemon
//	                 (the terminal is a side feature; the daemon's results must not fail as a whole just because a web page was closed).
//
// ackSeqs is the consume ack for term_* frames: once the daemon has handled them (started/written to the PTY) they count as done,
// and after marking they can be purged. exec does not use ack; its completion is decided by the result.
func (s *Service) SubmitResults(ctx context.Context, req hostproto.ResultsRequest) error {
	now := timeNow()

	for _, item := range req.ExecResults {
		if item.CallID == "" {
			continue
		}
		result := item.Result
		// Scrub secrets before storing: the stdout/stderr returned by the daemon may contain plaintext tokens printed by commands
		// (env/echo $AMA_TOKEN). We look up the injected credentials of the session by the dispatch's session_id to
		// decide what to scrub: one row lookup (just claimed, in the PG cache, negligible cost). If the scrubber
		// is not installed or the lookup fails, keep it as is: a scrub failure must not lose the result.
		if s.resultScrubber != nil {
			if d, derr := s.store.GetDispatch(ctx, item.CallID); derr == nil && d.SessionID != "" {
				result = s.resultScrubber(ctx, d.SessionID, result)
			}
		}
		raw, err := json.Marshal(result)
		if err != nil {
			// One result failing to serialize should not drag down the whole batch of terminal frames; but this almost never happens
			// (ExecResult is a flat, encodable struct). Log it and skip.
			s.log.Error("宿主机 exec 结果序列化失败，已跳过", slogCallID(item.CallID), errAttr(err))
			continue
		}
		if err := s.store.CompleteDispatch(ctx, req.HostID, item.CallID, string(raw), now); err != nil {
			return fmt.Errorf("host: 回写工具结果失败 call=%s: %w", item.CallID, err)
		}
	}

	for _, item := range req.ToolResults {
		if item.CallID == "" {
			continue
		}
		result := item.Result
		// The same scrubbing as exec results: a native result is just one final text (read/grep may read
		// content with credentials; glob file names could in theory contain secret strings too).
		if s.toolResultScrubber != nil {
			if d, derr := s.store.GetDispatch(ctx, item.CallID); derr == nil && d.SessionID != "" {
				result = s.toolResultScrubber(ctx, d.SessionID, result)
			}
		}
		raw, err := json.Marshal(result)
		if err != nil {
			s.log.Error("宿主机 tool 结果序列化失败，已跳过", slogCallID(item.CallID), errAttr(err))
			continue
		}
		if err := s.store.CompleteDispatch(ctx, req.HostID, item.CallID, string(raw), now); err != nil {
			return fmt.Errorf("host: 回写工具结果失败 call=%s: %w", item.CallID, err)
		}
	}

	// term ack: mark done in batch. The Store only needs id/seq-based primitives; to avoid one result POST
	// dragging many small transactions, AckDispatches is agreed to update a batch; it is declared in the Store interface.
	if len(req.AckSeqs) > 0 {
		if err := s.store.AckDispatches(ctx, req.HostID, req.AckSeqs, now); err != nil {
			return fmt.Errorf("host: 确认终端帧失败: %w", err)
		}
	}

	for _, f := range req.TermFrames {
		if f.TerminalID == "" {
			continue
		}
		// Ownership check: the terminalID in the frame must belong to the latest term_open of **this** host.
		// host_token only proves "you are some valid host", not that this terminal was opened by you. Without the check,
		// machine A could use its own token to write forged output/close frames to a browser terminal machine B is using.
		// Not found (the terminal frame is past its 10-minute retention, the browser closed long ago) is dropped too: weakly reliable frames;
		// if the terminal is gone, delivery is pointless. exec's callID uses a CAS on both conditions with host_id, so it is not affected.
		ownerHost, err := s.store.TerminalHostID(ctx, f.TerminalID)
		if err != nil {
			s.log.Warn("终端帧归属主机查不到，已丢弃",
				slogTermID(f.TerminalID), slogFrameType(f.Type), errAttr(err))
			continue
		}
		if ownerHost != req.HostID {
			s.log.Warn("终端帧归属主机与令牌主机不符，已丢弃（可能是越权投递）",
				slogTermID(f.TerminalID), slog.String("frame_host", ownerHost),
				slog.String("token_host", req.HostID))
			continue
		}
		if s.sink == nil {
			s.log.Warn("宿主机终端帧无中继出口，已丢弃", slogTermID(f.TerminalID), slogFrameType(f.Type))
			continue
		}
		// The PTY bytes of an output frame are base64-encoded by the daemon (DataB64); here they are **passed through as is**
		// to the sink, never decoded into a Go string in between: marshaling again when building the browser JSON downstream would trigger
		// UTF-8 validation and replace invalid bytes with �, wasting the base64. Decoding + scrubbing happen on the core side holding the browser
		// connection (re-encoded after scrubbing). closed/error are text and use Data.
		if err := s.sink.DeliverTermFrame(ctx, f.TerminalID, f.Type, f.Data, f.DataB64); err != nil {
			// The browser may have just closed the page / the holding instance may be switching: losing terminal frames is acceptable;
			// do not report an error to the daemon (it would trigger pointless retries), just log.
			s.log.Debug("中继宿主机终端帧失败", slogTermID(f.TerminalID), errAttr(err))
		}
	}
	return nil
}

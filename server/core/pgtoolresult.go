// PG adapter for the tool result ledger: it adapts the PG ledger, stored by (session, call), to
// the SDK's selfhosted.ToolResultStore interface (used by SessionToolRunner).
//
// # Why a separate layer
//
// The SDK's selfhosted.ToolResultStore method signatures **have no sessionID**
// (Recover/Begin/SaveResult/MarkSent only take callID), because by default its ledger is a local
// directory for one session. Our ledger is in PG and must be isolated per session, so this adapter
// **binds the sessionID** when it is built: the newWorker factory of each work item builds a new
// one of this type and captures arkSessionID in it. That way the SDK methods with no session
// parameter all carry the right session_id when they reach SQL, which physically rules out
// recording session A's result into, or recovering it for, session B (the "poison pill crosstalk"
// of the local file ledger).
package core

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/volcengine/ark-runtime-go/arkruntime/selfhosted"
)

// ToolResultLedger is the narrow storage interface of the tool result ledger (with ctx and
// sessionID).
//
// Implemented by PGStore in repository/dao/coredao; coremem does not implement it
// (with AMA_STORE=mem the worker has no ledger and uses the SDK's "always execute"; only for
// local/test use). Same approach as dockertools.ActivityLedger and core.Leases: infra/dao
// implements, cmd wires and injects.
type ToolResultLedger interface {
	GetToolResultState(ctx context.Context, sessionID, callID string) (state string, resultEvent []byte, found bool, err error)
	BeginToolResult(ctx context.Context, sessionID, callID string) error
	MarkToolResultResult(ctx context.Context, sessionID, callID string, resultEvent []byte) error
	MarkToolResultSent(ctx context.Context, sessionID, callID string) error
	ListToolResultsForRecover(ctx context.Context, sessionID string) (pending map[string][]byte, sent map[string]bool, err error)
	DeleteOldToolResults(ctx context.Context, olderThan time.Time) (int64, error)
}

// Ledger state constants, matching the strings coredao writes to the database.
const (
	ledgerStateStarted = "started"
	ledgerStateResult  = "result"
	ledgerStateSent    = "sent"
)

// pgToolResultStore is bound to one session and implements selfhosted.ToolResultStore.
type pgToolResultStore struct {
	ledger    ToolResultLedger
	sessionID string
	ctx       context.Context
	log       *slog.Logger
}

func newPGToolResultStore(
	ledger ToolResultLedger, sessionID string, ctx context.Context, logger *slog.Logger,
) *pgToolResultStore {
	if logger == nil {
		logger = slog.Default()
	}
	return &pgToolResultStore{
		ledger:    ledger,
		sessionID: sessionID,
		ctx:       ctx,
		log:       logger.With(slog.String("ark_session", sessionID)),
	}
}

// Compile-time check that the SDK interface is implemented.
var _ selfhosted.ToolResultStore = (*pgToolResultStore)(nil)

// Recover only recovers calls of **this session** whose result was not confirmed as sent.
//
// The return values mean the same as SDK FileToolResultStore.Recover:
//   - pending: result state (finished but not delivered); the SDK resends the result and does not
//     rerun the tool;
//   - processed (map[call]true): started state (the process died halfway, unknown whether it
//     finished); when the SDK sees it in answered, it returns a "state unknown, not re-executed"
//     result instead of running it again.
func (p *pgToolResultStore) Recover() (map[string]selfhosted.Event, map[string]bool, error) {
	pendingRaw, answered, err := p.ledger.ListToolResultsForRecover(p.ctx, p.sessionID)
	if err != nil {
		// Same trade-off as Begin: if the ledger cannot be read, degrade to "nothing to recover"
		// and let this work item run normally. The SDK treats a Recover error as fatal; passing it
		// up would kill the whole work item. Recovery only prevents reruns, and failing to prevent
		// reruns should not mean being unable to run.
		p.log.Warn("恢复工具账本失败，本轮不做防重恢复直接执行（崩溃后可能重复执行）",
			slog.Any("err", err))
		return map[string]selfhosted.Event{}, map[string]bool{}, nil
	}
	out := make(map[string]selfhosted.Event, len(pendingRaw))
	for callID, raw := range pendingRaw {
		var ev selfhosted.Event
		if uerr := json.Unmarshal(raw, &ev); uerr != nil {
			// One bad JSON row should not stop the whole work item: skip it (the cleanup job deletes stale rows).
			p.log.Warn("工具结果账本里的结果事件无法解析，已跳过",
				slog.String("tool_use_id", callID), slog.Any("err", uerr))
			continue
		}
		out[callID] = ev
	}
	return out, answered, nil
}

// Begin marks started before a tool runs. The returned decision always lets the SDK go on
// executing; when a result/sent row already exists, the explicit query below short-circuits
// (SDK FileToolResultStore decides not to rerun by reading the existing row; here one Get
// reproduces that branch).
//
// ⚠️ **When ledger reads or writes fail, degrade to "run the tool as usual"; never return the
// error to the SDK.** The SDK's handleToolUse treats a Begin error as fatal (the whole work item
// exits), while the ledger is only a best-effort layer for "avoid repeated execution after a
// crash". When it is down, the correct fallback is "no duplicate protection", not "tools never
// run". statement_timeout already limits "hanging" to 10s; this adds "failure does not block".
// Both are required.
//
// To be clear about the cost: if the process crashes right when started was not recorded, the
// next owner may run a tool with side effects (bash) again. That is much milder than "never run,
// the whole chat is broken": crashes are rare, while keeping the chat going when the ledger is
// briefly unavailable is the normal need.
func (p *pgToolResultStore) Begin(callID string, event selfhosted.Event) (selfhosted.ToolCallStoreDecision, error) {
	state, raw, found, err := p.ledger.GetToolResultState(p.ctx, p.sessionID, callID)
	if err != nil {
		// Without the ledger state we cannot tell whether it ran; the safe choice is to run, same as having no ledger.
		p.log.Warn("查工具账本状态失败，跳过防重检查直接执行工具（崩溃后可能重复执行一次）",
			slog.String("tool_use_id", callID), slog.String("tool", event.Name),
			slog.Any("err", err))
		return selfhosted.ToolCallStoreDecision{}, nil
	}
	if found {
		switch state {
		case ledgerStateSent:
			// Already delivered: do not execute or send again.
			return selfhosted.ToolCallStoreDecision{Sent: true}, nil
		case ledgerStateResult:
			// Finished but not delivered: resend the result, do not rerun the tool.
			var ev selfhosted.Event
			if uerr := json.Unmarshal(raw, &ev); uerr == nil {
				return selfhosted.ToolCallStoreDecision{Result: ev}, nil
			}
			// Result is corrupted; fall through to the "treat as started" branch below that replays unknown.
		case ledgerStateStarted:
			// Last run died halfway, unknown whether it finished: do not rerun, return an unknown result.
			if err := p.markUnknown(callID, event); err != nil {
				// If upgrading the ledger fails, degrade too: better to really run it again than stall the tool.
				p.log.Warn("标记 unknown 结果失败，降级为直接执行工具",
					slog.String("tool_use_id", callID), slog.Any("err", err))
				return selfhosted.ToolCallStoreDecision{}, nil
			}
			ev := unknownToolResultEvent(callID, event)
			return selfhosted.ToolCallStoreDecision{Result: ev}, nil
		}
	}

	// Never seen: mark started and let the SDK run the tool normally.
	if err := p.ledger.BeginToolResult(p.ctx, p.sessionID, callID); err != nil {
		p.log.Warn("写工具开始状态失败，照常执行工具（崩溃后可能重复执行一次）",
			slog.String("tool_use_id", callID), slog.String("tool", event.Name),
			slog.Any("err", err))
		return selfhosted.ToolCallStoreDecision{}, nil
	}
	return selfhosted.ToolCallStoreDecision{}, nil
}

// SaveResult saves the result after the tool runs and before it is sent back.
func (p *pgToolResultStore) SaveResult(callID string, result selfhosted.Event) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return p.ledger.MarkToolResultResult(p.ctx, p.sessionID, callID, raw)
}

// MarkSent records that the result was sent back to Ark successfully.
func (p *pgToolResultStore) MarkSent(callID string) error {
	return p.ledger.MarkToolResultSent(p.ctx, p.sessionID, callID)
}

// markUnknown upgrades a started row to result, with a placeholder result saying "execution state unknown".
func (p *pgToolResultStore) markUnknown(callID string, event selfhosted.Event) error {
	ev := unknownToolResultEvent(callID, event)
	raw, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	return p.ledger.MarkToolResultResult(p.ctx, p.sessionID, callID, raw)
}

// unknownToolResultEvent has the same shape as SDK FileToolResultStore.unknownToolExecutionResult:
// it returns an error result saying "the execution environment restarted; not rerun to avoid
// repeated side effects". Custom tools use the custom field.
func unknownToolResultEvent(callID string, event selfhosted.Event) selfhosted.Event {
	text := "tool execution state is unknown after worker restart; refusing to re-execute this tool to avoid duplicate side effects"
	if event.Type == selfhosted.EventTypeAgentCustomToolUse {
		return selfhosted.NewUserCustomToolResultEvent(callID,
			[]selfhosted.ContentBlock{{Type: "text", Text: text}}, true, event.SessionThreadID)
	}
	return selfhosted.NewUserToolResultEvent(callID,
		[]selfhosted.ContentBlock{{Type: "text", Text: text}}, true, event.SessionThreadID)
}

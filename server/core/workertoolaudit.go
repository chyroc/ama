// Tool call reconciliation: when a work item wraps up, check that every tool_use Ark started got a result sent back,
// and post an error result for any that did not, so the whole conversation turn does not stay stuck in requires_action forever.
//
// # Which gap it covers
//
// Under self_hosted, `agent.tool_use` and `user.tool_result` must match one to one: that turn on the Ark side
// waits until **all** call ids have results before it continues inference. With one missing, the session stays at
// `idle{stop_reason: requires_action}` and does not move, with **zero errors**; both sides think they are fine:
// Ark is waiting for us, and we are no longer listening.
//
// The SDK's SessionToolRunner is reliable on the normal path (serial processing within one session,
// FileToolResultStore for resuming after restart, retrySendEvent using context.Background to guarantee
// one bounded delivery attempt for results already executed). But that whole chain rests on the premise **this runner is still alive**,
// and that premise breaks in several ways, none of which goes through postResult:
//
//	the runner already exited on idle timeout, and Ark then sent another tool_use (the production sample this time was this one)
//	the work lease was lost / heartbeat dropped → workCtx is canceled and runLoop returns right away
//	the event stream broke at the instant "tool_use delivered, not yet consumed"
//	the pod was replaced (rolling deploy, scale-down, health check failure)
//
// **None** of these paths posts a tool_result. So that turn is stuck forever, and 30 minutes later
// reapStaleRuns marks it terminated; what the user sees is "it stopped halfway through".
//
// # Why the fallback lives here instead of relying on the runner itself
//
// The test is "who holds both the session id and an API that still works". Once the runner exits it cannot
// do anything; while each work goroutine in pollLoop, after HandleItem returns, has **exactly**
// both. It is also the only point "always passed through, whatever the reason the runner exited".
//
// # ⚠️ This is a fallback, not a fix for the root cause
//
// To the agent the posted result is an **error**: it sees "this tool could not run", then decides by itself
// whether to retry or take another route. That beats being stuck forever, but **it is not the same as the tool actually running**. So:
//
//   - each time the fallback fires it logs a Warn with "started N / answered M / which ones were filled in",
//     so that "the root cause was fixed" and "the fallback is quietly cleaning up" **can be told apart** in the logs.
//     This repo's experience: if a fallback fix leaves no distinguishable trace, both make the alert go away,
//     and afterwards nobody can tell which one it was.
//   - the check is to search for the Warn log emitted below ("tool call returned no result"): 0 hits means this path was never reached.
package core

import (
	"context"
	"log/slog"
	"time"

	"github.com/volcengine/ark-runtime-go/arkruntime/selfhosted"
)

const (
	// toolAuditListLimit is the page size when listing events for reconciliation, matching the SDK's reconcileOnce.
	toolAuditListLimit = 1000
	// toolAuditMaxPages is the hard upper bound on paging, to stop "the server cursor does not advance" from becoming an infinite loop.
	//
	// This runs in the work goroutine and **holds a concurrency slot**; an infinite loop shows up as
	// "this environment's concurrency silently drops by one", with no error and nothing pointing here.
	//
	// 1000 × 1000 = one million events, far beyond any real session (the production sample had 87),
	// so the normal path never reaches it; reaching it means the server is misbehaving, and then filling in one less time is better.
	toolAuditMaxPages = 1000
	// toolAuditTimeout is the timeout for one whole reconciliation (list events + post results).
	//
	// The call site passes a ctx **without cancellation** (`context.WithoutCancel`) plus this timeout:
	// reconciliation must be able to finish even while the process is shutting down; it only needs to be bounded, not canceled along with upstream.
	//
	// ⚠️ Not because "HandleItem cancels the ctx passed in": it cancels its own derived workCtx,
	// and the outer one is unaffected. See the comment at the call site.
	//
	// 30 seconds is enough to list a few pages of events and post a few results; longer would hold up the worker's exit.
	toolAuditTimeout = 30 * time.Second
)

// orphanToolResultText is the body of the posted result.
//
// It is written for the **model**, so it must make three things clear: it did not run, it is not a problem with its call, it can retry.
// Do not write "service error": that makes the agent think its arguments were wrong and rewrite the same command over and over.
const orphanToolResultText = "这次工具调用没有被执行：执行环境在结果回传之前就已结束" +
	"（例如容器被回收、实例发布重启、或本轮会话已超时）。这不是调用参数的问题。" +
	"如果这一步仍然必要，可以重新发起一次同样的调用。"

// auditToolCalls checks whether a work item's session has tool calls "started but without a result",
// and if so posts an error result, so that Ark's turn can go on.
//
// Returns how many were posted, only for tests and logs.
//
// It serves two call sites, and in both the api must be built with **the key of the environment this session is in**;
// the only difference is whether a send gate is wrapped around it:
//
//   - worker wrap-up (worker.go): before the work goroutine exits, api is wrapped by sendGuard,
//     and after a permanent 4xx it stops posting;
//   - sweep rescue (rescueOrphanToolResults in runwatcher.go): the work was already stopped by the Ark server
//     and no worker will touch it again, so the sweep posts with the bare ClientAPI.
//
// # Why the parameter is sessionID and not the whole WorkItem
//
// Reconciliation only concerns the session: `tool_use` / `tool_result` both hang on the session, regardless of which work
// claimed it. Passing the work would make people think "a different work item needs its own reconciliation".
func auditToolCalls(
	ctx context.Context, api selfhosted.API, sessionID string, logger *slog.Logger,
) int {
	if api == nil || sessionID == "" {
		return 0
	}

	orphans, used, answered, err := listOrphanToolCalls(ctx, api, sessionID)
	if err != nil {
		// Without events there is no reconciliation. **Log it**: returning silently would make "the fallback did not work" and
		// "there were no orphans anyway" share one zero-log path, exactly the shape this repo keeps hitting.
		logger.Warn("工具调用对账失败，本轮不补结果（这条会话可能卡在 requires_action）",
			slog.String("ark_session", sessionID), slog.Any("err", err))
		return 0
	}
	if len(orphans) == 0 {
		// Normal case, no log: every work item takes this path, and logging would flood.
		return 0
	}

	// ⚠️ **This log line is the core of observability for the whole change.**
	//
	// The biggest obstacle when debugging this kind of failure is that "Ark started N, we answered M" does not add up,
	// and **those three call ids left no trace at all in the logs** (measured: searching all logs by call id found 0 entries).
	// So this must state all three at once: how many started, how many answered, which ones are missing.
	//
	// Only call ids and tool names are logged, **not the input body**: that is user data (bash commands,
	// file paths); see "log ids and enums, not bodies" in docs/guides/backend.md.
	logger.Warn("工具调用没有回传结果，补发错误结果避免整轮卡死",
		slog.String("ark_session", sessionID),
		slog.Int("发起", used),
		slog.Int("已回传", answered),
		slog.Int("补发", len(orphans)),
		slog.Any("孤儿调用", orphanDigest(orphans)))

	posted := 0
	for _, ev := range orphans {
		if ctx.Err() != nil {
			// ctx expired (reconciliation timeout). The rest cannot be posted this time; say so. Otherwise
			// the Warn above claimed "posting N" while only some were actually posted.
			logger.Warn("补发工具结果中途超时，剩下的这轮没补上",
				slog.String("ark_session", sessionID),
				slog.Int("已补", posted), slog.Int("计划补", len(orphans)))
			return posted
		}
		out := orphanToolResult(ev)
		if err := api.SendEvent(ctx, selfhosted.SendEventRequest{
			SessionID: sessionID,
			Event:     out,
		}); err != nil {
			// One failure does not affect the others: each call id is independent, and Ark wants results for **all** of them;
			// with one missing it stays stuck, so keep posting the rest.
			logger.Warn("补发工具结果失败",
				slog.String("ark_session", sessionID),
				slog.String("tool_use_id", toolCallID(ev)),
				slog.String("tool", ev.Name),
				slog.Any("err", err))
			continue
		}
		posted++
	}
	if posted > 0 {
		logger.Info("已补发工具结果，方舟那一轮可以继续",
			slog.String("ark_session", sessionID), slog.Int("条数", posted))
	}
	return posted
}

// listOrphanToolCalls lists all events and finds tool_use events "started but without a result".
//
// Returns (orphan events, total started, total answered, err). The last two numbers are only for logs: they are the only
// quantitative evidence that log line about "does not add up" can give.
//
// # The check is a set difference of call ids, not a count difference
//
// A count (15 vs 12) only says "3 are missing", not **which three**, while posting needs
// the specific call ids. The set difference also handles duplicate delivery naturally: the same call id appearing in two tool_use events
// (Ark re-pushing) counts once.
func listOrphanToolCalls(
	ctx context.Context, api selfhosted.API, sessionID string,
) (orphans []selfhosted.Event, used, answered int, err error) {
	// uses keeps order: posting follows the order Ark started them, so logs read in line with the event stream.
	var order []selfhosted.Event
	seen := map[string]bool{}
	done := map[string]bool{}

	page := ""
	// ⚠️ **Paging must have an upper bound.** `for { ... page = resp.NextPage }` loops forever when the server returns the same
	// cursor, and this runs in the work goroutine, **holding a concurrency slot**:
	// once it spins, "this environment's concurrency silently drops by one", with no error and nothing pointing here.
	// Found during self-review (the first version had no bound).
	//
	// Two gates: a page-count bound, and "the cursor must change". The latter is more precise: it directly rules out
	// the specific failure "the server keeps giving the same cursor".
	for pages := 0; pages < toolAuditMaxPages; pages++ {
		resp, err := api.ListEvents(ctx, selfhosted.ListEventsRequest{
			SessionID: sessionID,
			Page:      page,
			Limit:     toolAuditListLimit,
			Order:     selfhosted.EventListOrderAsc,
		})
		if err != nil {
			return nil, 0, 0, err
		}
		if resp == nil {
			break
		}
		for _, ev := range resp.Events {
			switch ev.Type {
			case selfhosted.EventTypeAgentToolUse, selfhosted.EventTypeAgentCustomToolUse:
				id := toolCallID(ev)
				if id == "" || seen[id] {
					continue
				}
				seen[id] = true
				order = append(order, ev)
			case selfhosted.EventTypeUserToolResult, selfhosted.EventTypeUserCustomToolResult:
				if id := toolResultCallID(ev); id != "" {
					done[id] = true
				}
			}
		}
		if resp.NextPage == "" || resp.NextPage == page {
			break
		}
		page = resp.NextPage
	}

	for _, ev := range order {
		if !done[toolCallID(ev)] {
			orphans = append(orphans, ev)
		}
	}
	return orphans, len(seen), len(done), nil
}

// orphanToolResult builds an error result for a tool_use that was not executed.
//
// **custom must be told apart**: for custom tools (wakeup) both the result event type and the id field differ
// (`user.custom_tool_result` + `custom_tool_use_id`). With the wrong type Ark does not accept it,
// the turn stays stuck, and reading the logs you would think "it was already posted".
//
// # ⚠️ The call id must go through toolCallID, not a bare field
//
// This once used `ev.ToolUseID` / `ev.CustomToolUseID`, while the checks, logs and
// digest in the same file **all** use the three-way fallback `toolCallID(ev)` (`:200 :144 :274`); only building the event
// diverged here. Both branches of the SDK's `postResult` pass `toolUseCallID(event)`
// (`session_tool_runner.go:858-864`), so the bare field was a deviation only on our side.
//
// The chain of consequences has **zero errors from start to end**, which is the worst part:
//
//  1. In sessions where `tool_use_id` is not sent, `ev.ToolUseID == ""` (measured records in
//     docs/guides/frontend.md);
//  2. `listOrphanToolCalls` uses `toolCallID`, falls back to `ev.ID`, and **correctly finds the orphan**;
//  3. the log also uses `toolCallID`, so **the logged call id is right and everything looks normal**;
//  4. but the empty string passed when building the event meets `omitempty`, and **that key disappears from the JSON entirely**;
//  5. SendEvent does not validate, the server returns 200 → `posted++` → logs "tool result posted";
//  6. Ark gets a tool_result pointing at no call, and **the turn stays stuck in
//     requires_action**, while the log says posting succeeded, so whoever debugs rules this path out.
//
// That is exactly the shape the file header warns about with "a fallback fix must leave a distinguishable trace": the fallback looks like it worked,
// but did not actually cover it. Using `toolCallID`, consistent with the SDK, breaks step 4 of the chain.
func orphanToolResult(ev selfhosted.Event) selfhosted.Event {
	content := []selfhosted.ContentBlock{{Type: "text", Text: orphanToolResultText}}
	callID := toolCallID(ev)
	if ev.Type == selfhosted.EventTypeAgentCustomToolUse {
		return selfhosted.NewUserCustomToolResultEvent(
			callID, content, true, ev.SessionThreadID)
	}
	return selfhosted.NewUserToolResultEvent(
		callID, content, true, ev.SessionThreadID)
}

// toolCallID gets the call id of a tool_use event.
//
// Three fields in order: normal tools use tool_use_id, custom tools use custom_tool_use_id,
// and with neither it falls back to the event's own id (consistent with the SDK's toolUseCallID; if the two diverged,
// "the SDK thinks it was answered, we think it is an orphan", and posting would be a duplicate delivery).
func toolCallID(ev selfhosted.Event) string {
	if ev.ToolUseID != "" {
		return ev.ToolUseID
	}
	if ev.CustomToolUseID != "" {
		return ev.CustomToolUseID
	}
	return ev.ID
}

// toolResultCallID gets the call id a result event points to.
//
// **It deliberately does not fall back to ev.ID**: a result event's own id is a different set (evt-xxx); using it as a call id
// would put a never-matching key into the done set, so a real orphan would instead count as answered.
func toolResultCallID(ev selfhosted.Event) string {
	if ev.ToolUseID != "" {
		return ev.ToolUseID
	}
	return ev.CustomToolUseID
}

// orphanDigest compresses the orphan list into a short "call id:tool name" list for logs.
//
// Only ids and tool names, **no inputs**. The tool name is needed: the call id alone does not show whether "what is stuck is
// bash or read", and that decides whether to check the container side.
func orphanDigest(orphans []selfhosted.Event) []string {
	out := make([]string, 0, len(orphans))
	for _, ev := range orphans {
		out = append(out, toolCallID(ev)+":"+ev.Name)
	}
	return out
}

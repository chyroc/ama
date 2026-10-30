// Recovery of unfinished turns after a service restart (Inflight Resume).
//
// # The problem it solves
//
// All the state of a Feishu conversation turn used to live only in cardUpdater in inbound.go (process memory).
// Once amad restarts (deploy, scaling, OOM, panic), that turn is lost completely, and the user sees a card
// stuck at "thinking" forever, and cannot even ask again (that message_id is left in pending,
// and the dedup logic blocks it as a duplicate delivery). Every deploy is a restart, so this is not an edge case.
//
// # Why not "redeliver the user message"
//
// If the agent were a local CLI process that died along with the service, after a restart the user message would have to be delivered again,
// or it would really be gone.
//
// **This repo is the opposite.** Here apimodel.Run.ID is the Ark session id, and inference runs on the Ark server:
// it keeps going when amad dies, and tool calls are picked up again by the self_hosted worker polling the work queue after it restarts.
// So that turn **has most likely already finished, or is still running normally**. Redelivering the user message would only make
// the agent answer the same question twice, and the user would get two replies.
//
// So recovery does this:
//
//	scan the table and find unfinished turns
//	  ↓
//	CAS claim (only one of multiple instances can take over; otherwise two instances write the same card and the content jumps back and forth)
//	  ↓
//	SyncRunEvents fills in the events Ark produced during the gap (and computes the final state along the way)
//	  ↓
//	already final → finalize; still running → reattach the stream and keep listening until end_turn
//
// # Why the limit is 1 hour
//
// For a turn interrupted for more than 1 hour, the user has most likely asked again or changed the subject, and forcing it to continue would only produce a very late
// answer. The workspace stays in the Docker volume longer (see shared/infra/dockertools/README.md),
// so this limit governs "is it still worth answering", not "is the container still there". The timeout branch does not continue;
// it only finalizes the card and clears the poisoning, so the user can ask again.
package feishu

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/chyroc/ama/server/shared/gopkg/arkframe"
)

const (
	// inflightMaxAge is the longest interruption we are still willing to continue after. See the file header for why.
	inflightMaxAge = time.Hour

	// inflightMaxRetries is the limit on automatic resumes.
	//
	// Beyond it, give up and let the user resend. Without this limit, a turn that "never finishes" would be run for nothing on every
	// restart, each time occupying a goroutine holding a stream.
	inflightMaxRetries = 3
)

// ResumeInflightTurns scans for unfinished turns left by the previous process and takes them over one by one.
//
// **Best-effort as a whole, returns no error**: the worst result of a failed recovery is going back to the state of "no such feature",
// and it should not break process startup. Each failure is logged in place.
//
// It is called at process startup, before the long connections are set up (see supervisor.go). With sessions reused by scope,
// the session being recovered **will** also receive new messages from that chat, but a new message must TryBeginTurn before running,
// and the inflight record stays during recovery, so it is blocked; no extra lock is needed.
func (in *Inbound) ResumeInflightTurns(ctx context.Context) {
	recs, err := in.cfg.Store.ListInflightTurns(ctx)
	if err != nil {
		in.log.Warn("扫描未结轮次失败，本次不做恢复", "error", err.Error())
		return
	}
	if len(recs) == 0 {
		return
	}
	in.log.Info("发现未结轮次，开始恢复", "count", len(recs))

	for _, rec := range recs {
		if ctx.Err() != nil {
			// The process is exiting; leave the rest to the next startup. The records are still in the store and will not be lost.
			return
		}
		in.resumeOne(ctx, rec)
	}
}

// resumeOne recovers one turn. It handles three kinds; see the table in the file header.
func (in *Inbound) resumeOne(ctx context.Context, rec InflightTurnRecord) {
	log := in.log.With("run", rec.RunID, "larkApp", rec.LarkAppID, "chat", rec.ChatID)

	// Claim first. Failing to grab it means another instance already took over (or it just finished and was deleted), so skip it.
	// This step also does retry_count +1.
	claimed, err := in.cfg.Store.ClaimInflightTurn(ctx, rec.RunID)
	if err != nil {
		log.Warn("认领未结轮次失败，跳过", "error", err.Error())
		return
	}
	if !claimed {
		log.Debug("这条轮次已被别的实例接管，跳过")
		return
	}
	// The claim succeeded and retry_count in the store has gone up; keep the local copy in step too, since the limit check and the text both use it.
	rec.RetryCount++

	age := in.now().Sub(rec.StartedAt)

	switch {
	case age > inflightMaxAge:
		// Hung too long: the container has been reclaimed, and continuing is pointless. Finalize + clear the poisoning, so the user can ask again.
		log.Info("未结轮次已超过可恢复时限，不再续跑",
			"age", age.Round(time.Second), "maxAge", inflightMaxAge)
		in.finalizeAbandoned(ctx, rec, fmt.Sprintf(
			"\n\n---\n⚠️ 服务重启导致这一轮中断，且已超过 %s 的可恢复时限（执行环境已被回收）。请重发一遍这条消息。",
			inflightMaxAge))

	case rec.RetryCount > inflightMaxRetries:
		// Still not finished after repeated resumes: stop trying automatically, and tell the user to resend.
		log.Warn("未结轮次已多次续跑仍未完成，放弃", "retryCount", rec.RetryCount)
		in.finalizeAbandoned(ctx, rec, fmt.Sprintf(
			"\n\n---\n🚫 服务多次重启，已自动续跑 %d 次仍未完成。请重发一遍这条消息。",
			rec.RetryCount-1))

	default:
		if err := in.resumeLive(ctx, rec, log); err != nil {
			// Keep the record: the next startup tries again with the increased retry_count, and gives up when it hits the limit.
			log.Warn("续跑未结轮次失败，留待下次启动重试", "error", err.Error())
			return
		}
	}
}

// resumeLive really takes a turn back: fill the gap → finalize or reattach the stream by state.
func (in *Inbound) resumeLive(ctx context.Context, rec InflightTurnRecord, log *slog.Logger) error {
	token, err := in.tokenFor(ctx, rec.LarkAppID)
	if err != nil {
		return err
	}

	// Fill back where we got to last time; new content continues after it.
	//
	// **It cannot be cleared and restarted**: the text already produced may no longer be sent by the Ark side
	// (frames consumed by the previous process are not replayed), and clearing would permanently lose the beginning of the answer.
	answer := &answerBuffer{persist: in.inflightPersister(ctx, rec)}
	answer.write(rec.LastText)

	// A recovered turn is subject to the same time limit; do not let it hang forever.
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runTimeout)
	defer cancel()

	// Take over this turn's chain of thought.
	//
	// The key is per turn, exactly what was computed when it started (see progressDeliveryKey), so what we get here is
	// **the same** delivery record: the remote cot_id is already in the store, so we can append to and close
	// that chain of thought directly, instead of opening a new one in the chat. If the lease cannot be grabbed (another instance is pushing) it returns nil,
	// and then we only send the answer.
	turnKey := ProgressDeliveryKey(rec.RunID, rec.SrcMessageID)
	progress := in.startTurnProgress(runCtx, token, rec, turnKey, rec.AgentID, log)

	// Fill in the events Ark produced during the gap. This step also computes the run's final state
	// (SyncRunEvents → persistArkEvent → applyEventToRun), which is why we can ask for the status below.
	if err := in.cfg.Conversations.SyncRunEvents(runCtx, rec.RunID); err != nil {
		// If filling fails, release the lease so the next startup (or another instance) can take over immediately,
		// instead of waiting 30 seconds for the lease to expire.
		progress.release(ctx, in.cfg.Progress)
		return fmt.Errorf("补齐方舟事件失败: %w", err)
	}

	status, err := in.cfg.Conversations.RunStatus(runCtx, rec.RunID)
	if err != nil {
		progress.release(ctx, in.cfg.Progress)
		return fmt.Errorf("读会话状态失败: %w", err)
	}

	if isTerminalRunStatus(status) {
		// It finished during the gap. Send what was collected and we are done; this is the most common case:
		// a restart only takes tens of seconds, and Ark finished producing the answer long ago.
		log.Info("未结轮次在断档期间已完成，定稿", "status", status)
		return in.finishAndClear(ctx, rec, token, answer, progress, log)
	}

	// Still running (including requires_action: the agent is waiting for a tool result, and the worker picks it up again after restarting).
	// Reattach and keep listening: **this is the "seamless" step**.
	log.Info("未结轮次仍在进行，重新挂流续跑", "status", status)
	var failedUpstream bool
	streamErr := in.cfg.Conversations.StreamRunEvents(runCtx, rec.RunID, func(raw []byte) error {
		if text, ok := arkframe.AgentText(raw); ok && text != "" {
			answer.write(text)
		}
		// Same as runAndReply: the part before a tool call is usually narration like "let me go check", not the reply.
		// See the type comment on answerBuffer: splitting **does not lose content**, and it falls back when the turn ends with tool_use.
		if arkframe.StartsToolCall(raw) {
			answer.reset()
		}
		// Same as runAndReply: a turn whose retries were exhausted on the Ark side gets a message that can state the cause.
		// See turnFailedUpstream.
		if arkframe.FailedUpstream(raw) {
			failedUpstream = true
		}
		progress.consume(runCtx, raw)
		return nil
	})
	if streamErr != nil {
		log.Warn("续跑的事件流异常结束", "error", streamErr.Error())
	}
	// However the stream ends, finish up: close the chain of thought and send what was collected.
	if failedUpstream && strings.TrimSpace(answer.text()) == "" {
		answer.write(replyRetriesExhausted)
	}
	return in.finishAndClear(ctx, rec, token, answer, progress, log)
}

// finishAndClear finishes a recovered turn: close the chain of thought → remove the reaction → send the answer → clear the record.
//
// **Cleanup must happen**, even if sending the answer fails: a record left in the store would make the next startup recover the same turn again,
// and the user would see a duplicate reply. So send errors are only reported, not allowed to block cleanup.
func (in *Inbound) finishAndClear(
	ctx context.Context, rec InflightTurnRecord, token string,
	answer *answerBuffer, progress *turnProgress, log *slog.Logger,
) error {
	defer in.clearInflight(ctx, rec)

	// Use a ctx without cancellation: the process may be exiting, and these steps **must be completed**:
	// without closing, a forever spinning chain of thought is left; without sending the answer, the user waited for nothing.
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), replyFinishTimeout)
	defer cancel()

	progress.finish(finishCtx, "done", "")
	if in.cfg.Progress != nil {
		waitForProgressTerminal(finishCtx, in.cfg.Progress, ProgressDeliveryKey(rec.RunID, rec.SrcMessageID), in.now, log)
	}
	sendErr := sendFinalReply(finishCtx, in.cfg.Sender, token, rec.SrcMessageID,
		ProgressDeliveryKey(rec.RunID, rec.SrcMessageID), answer.text(), rec.ReplyInThread, log)

	// After the answer is sent, add a DONE.
	//
	// **The recovery path cannot remove the original OnIt**: the reaction_id only lived in the previous process's memory, and after a restart
	// there is no anchor to remove it (removing it would need listing all reactions on this message and filtering by operator; two extra
	// remote calls for one reaction are not worth it). So an empty ID is passed here, and markDone skips the removal and only adds DONE:
	// that message then has both OnIt and DONE. This is a known small flaw, but **better than leaving only OnIt**:
	// the user can at least see this turn has been answered, instead of thinking it is still being processed.
	if sendErr == nil {
		markDone(finishCtx, in.cfg.Reactor, token, rec.SrcMessageID, "", log)
	}
	return sendErr
}

// finalizeAbandoned gives up resuming: sends the content already produced together with an explanation to the user, then clears the record.
//
// The explanation is **appended** after the existing content and does not overwrite the half reply: it is often valuable
// (the agent has already analyzed part of it), and dropping it would only make things more confusing.
func (in *Inbound) finalizeAbandoned(ctx context.Context, rec InflightTurnRecord, notice string) {
	defer in.clearInflight(ctx, rec)

	token, err := in.tokenFor(ctx, rec.LarkAppID)
	if err != nil {
		in.log.Warn("放弃续跑时换 token 失败，用户收不到说明",
			"run", rec.RunID, "error", err.Error())
		return
	}

	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), replyFinishTimeout)
	defer cancel()
	if err := sendFinalReply(writeCtx, in.cfg.Sender, token, rec.SrcMessageID,
		ProgressDeliveryKey(rec.RunID, rec.SrcMessageID),
		rec.LastText+notice, rec.ReplyInThread, in.log); err != nil {
		in.log.Warn("放弃续跑时发说明失败", "run", rec.RunID, "error", err.Error())
	}
}

// clearInflight deletes the turn record and gets the inbound message out of pending.
//
// # The unpoisoning step must never be missed
//
// If that row in feishu_inbound_messages stays in pending, ClaimInboundMessage treats it as
// a duplicate delivery and acks directly (see that method's truth table), so **even when the user resends the same sentence it is blocked**,
// that sentence can never be asked again, with no error at all. This is one of the three things this feature fixes.
//
// Mark it processed rather than failed: failed would make Feishu really run the agent again on the next redelivery,
// while this turn's answer is already on the card (or the user was clearly told to resend); running again would be consuming it twice.
func (in *Inbound) clearInflight(ctx context.Context, rec InflightTurnRecord) {
	// Use a ctx without cancellation: by now ctx may already be Done because the process is exiting,
	// and these two things must be done; missing them means "duplicate recovery on next startup" plus "the user can never ask this again".
	cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if rec.SrcMessageID != "" {
		if err := in.cfg.Store.MarkInboundMessage(cleanCtx, rec.SrcMessageID, inboundProcessed); err != nil {
			in.log.Warn("解除入站消息毒化失败，用户重发这句话可能被当成重复投递挡掉",
				"run", rec.RunID, "message", rec.SrcMessageID, "error", err.Error())
		}
	}
	if err := in.cfg.Store.DeleteInflightTurn(cleanCtx, rec.RunID); err != nil {
		in.log.Warn("清理未结轮次记录失败，下次启动可能重复恢复这一轮",
			"run", rec.RunID, "error", err.Error())
	}
}

// tokenFor exchanges a tenant_access_token for a Feishu app.
//
// The token expires in two hours, so it is **not stored**; on recovery a new one is exchanged per app.
func (in *Inbound) tokenFor(ctx context.Context, larkAppID string) (string, error) {
	secret, err := in.cfg.Store.AppSecret(ctx, larkAppID)
	if err != nil {
		return "", fmt.Errorf("取应用密钥失败: %w", err)
	}
	token, err := in.cfg.Tokens.TenantAccessToken(ctx, larkAppID, secret)
	if err != nil {
		return "", fmt.Errorf("换 token 失败（凭证可能已失效）: %w", err)
	}
	return token, nil
}

// isTerminalRunStatus decides whether this session has reached the end.
//
// The value range matches apimodel.RunStatus (a bare string is taken here; see the comment on Conversations.RunStatus):
//
//	idle / terminated        final, finalize and finish
//	running / rescheduling   still running, reattach the stream
//	requires_action          **not final**: the agent is waiting for a tool result, and the worker picks this work up again
//	                         after restarting, so it moves on by itself. Judging it final would freeze the card
//	                         right when the agent is about to continue, and the user gets half an answer.
//
// # A known ambiguity from session reuse
//
// With sessions reused by scope, one session has run many turns, and idle only says "this session is not running now",
// without telling whether this turn finished or an earlier turn finished long ago. So reading idle on recovery can mean two things.
//
// It is still treated as final here, because **the correct action happens to be the same under both meanings**: first SyncRunEvents fills in
// all events Ark produced during the gap (resumeLive does it before asking for the status), then finalize what was collected.
// If this turn really finished, after filling the card is the complete answer; if this turn never started (it crashed before the message
// got into Ark), there is no content to wait for either, and finalizing takes the "this turn produced no reply" fallback and clears the poisoning,
// so the user can ask again. Treating it as "still running" instead would leave recovery waiting on a silent stream until timeout.
func isTerminalRunStatus(status string) bool {
	switch status {
	case "idle", "terminated":
		return true
	default:
		return false
	}
}

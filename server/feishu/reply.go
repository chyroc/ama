// Delivery of the final answer: splitting, the ordering gate, the OnIt reaction.
//
// # The answer and the chain of thought are two separate messages
//
// The CoT message never becomes the answer. The whole orchestration is:
//
//	message received   add the OnIt reaction to the user's message
//	start running      create the CoT message and append events while running
//	finished           close unclosed events → close → remove OnIt
//	afterwards         the final answer is sent as 1~N separate messages
//
// # Why split
//
// Feishu limits the number of markdown tables in one card, and **over the limit the whole card is silently dropped**
// (error code 230099). What the user sees is "the bot thought for a long time and said nothing in the end". So over the limit it is
// split into several messages, each with its own idempotency key.
//
// # Why the ordering gate has a cap
//
// The answer waits for the CoT to close, otherwise the user sees the conclusion before the thinking, which is odd. But **it must not wait forever**:
// the CoT may never close for various reasons (uncertain result, giving up after consecutive failures), and then a slightly wrong order is better
// than the user never getting an answer. So it waits up to a limit and then lets it through.

package feishu

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/chyroc/ama/server/shared/infra/lark"
)

const (
	// maxCardTables is the limit on markdown tables in one card.
	//
	// Over it Feishu **silently drops the whole card** (230099), so better to send a few more messages.
	maxCardTables = 5

	// finalReplyWaitMax is the limit for the final answer waiting for the CoT to close. See the file header.
	finalReplyWaitMax = 10 * time.Second

	// finalReplyPollInterval is the polling interval while waiting.
	finalReplyPollInterval = 250 * time.Millisecond
)

// The two fallback texts for an empty reply.
//
// Both deliberately give a **next step**: saying only "something went wrong" leaves users nothing to do,
// while in these two cases what to do is actually known (resend once / wait a bit and resend).
const (
	// replyNoAnswer is the generic fallback: this turn really produced nothing, for an unknown reason.
	replyNoAnswer = "这一轮没有产生回复，可能是执行出了问题。把刚才那句话再发一次试试；" +
		"要是还不行，换个说法或稍后再问。"

	// replyRetriesExhausted is for a turn where "retries were exhausted on the Ark side", with a known cause (most likely rate limiting).
	//
	// It is separate from the generic one because **the next step differs**: here resending the same sentence is enough, no need to rephrase,
	// and no need to doubt whether the question was wrong. See turnFailedUpstream.
	replyRetriesExhausted = "这一轮没能完成：模型侧连续失败、重试次数已用尽（多半是被限流了）。" +
		"稍等一会儿把刚才那句话再发一次就好。"
)

// Reactor is the ability to add/remove reactions, provided by shared/infra/lark.
type Reactor interface {
	AddReaction(ctx context.Context, token, messageID, emojiType string) (string, error)
	RemoveReaction(ctx context.Context, token, messageID, reactionID string) error
}

// splitReplyByTables splits the reply into segments by table count.
//
// It only splits when really over the limit: most replies fit in one, and splitting needlessly would add several messages to the chat.
func splitReplyByTables(markdown string, maxTables int) []string {
	if maxTables <= 0 || countMarkdownTables(markdown) <= maxTables {
		return []string{markdown}
	}

	lines := strings.SplitAfter(markdown, "\n")
	var segments []string
	var cur []string
	tables, inFence, inTable := 0, false, false

	flush := func() {
		if seg := strings.TrimSpace(strings.Join(cur, "")); seg != "" {
			segments = append(segments, seg)
		}
		cur = cur[:0]
		tables = 0
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Pipes inside code blocks are not tables. Without this check, bash output that pastes an ASCII table
		// would be taken as a pile of tables, and cut into a string of baffling fragments.
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
		}
		isTableRow := !inFence && strings.HasPrefix(trimmed, "|")
		if isTableRow && !inTable {
			// A new table starts. If it does not fit, send what came before first.
			if tables+1 > maxTables && len(cur) > 0 {
				flush()
			}
			tables++
		}
		inTable = isTableRow
		cur = append(cur, line)
	}
	flush()
	if len(segments) == 0 {
		return []string{markdown}
	}
	return segments
}

// countMarkdownTables counts how many tables a piece of markdown has.
func countMarkdownTables(markdown string) int {
	count, inFence, inTable := 0, false, false
	for _, line := range strings.Split(markdown, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue
		}
		isTableRow := !inFence && strings.HasPrefix(trimmed, "|")
		if isTableRow && !inTable {
			count++
		}
		inTable = isTableRow
	}
	return count
}

// segmentIdempotencyKey builds a stable idempotency key for segment i.
//
// It includes the total segment count: if the split result changes when the same run retries (e.g. the content was changed), the key changes too,
// so a new segment is not dropped as a duplicate of an old one.
func segmentIdempotencyKey(runID string, index, total int) string {
	return fmt.Sprintf("ama-reply:%s:%d/%d", runID, index, total)
}

// waitForProgressTerminal waits for the CoT to close, at most finalReplyWaitMax.
//
// true means it got there, false means it timed out and let it through; **in both cases the answer is sent**,
// and the return value is only used for logging.
func waitForProgressTerminal(
	ctx context.Context, store ProgressStore, runID string, now func() time.Time, log *slog.Logger,
) bool {
	deadline := now().Add(finalReplyWaitMax)
	for {
		d, err := store.GetProgressDelivery(ctx, runID)
		if err != nil {
			// There is no progress record at all (e.g. the progress feature is not enabled): nothing to wait for.
			return true
		}
		if d.ReadyForFinalReply() {
			return true
		}
		if now().After(deadline) {
			log.Warn("等思维链封口超时，先发最终答案",
				"run", runID, "status", d.Status, "waited", finalReplyWaitMax.String())
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(finalReplyPollInterval):
		}
	}
}

// sendFinalReply sends the final answer as separate messages, split into several when needed.
//
// Sent in order, not concurrently: concurrently the arrival order on Feishu is not guaranteed, and the user would see a garbled answer.
//
// # ⚠️ An empty reply has two causes, and only one should show a notice
//
// This is the easiest place to get wrong in this function, and where a production bug happened. `answer == ""` can come from:
//
//	(a) execution really went wrong   Ark crashed, every tool failed, the turn never got going
//	                                  → **show a notice**, or the user waits at a blank for a reply that never comes
//	(b) this turn was interrupted     the user sent the next message, and the previous turn was redirected
//	                                  → **send no message at all**; an interrupted turn having no answer is normal
//
// This function **only handles (a)**: it treats every empty string it gets as "execution went wrong". Telling the two apart is the caller's job:
// case (b) never gets here (runAndReply returns directly when it recognizes user.interrupt;
// see the interrupted branch there and turnInterrupted).
//
// The old code did not make this distinction, so when sending "1, 2, 3" in a row the first two turns each got a pointless "something went wrong during execution",
// while they had only been interrupted by the user's own next message, and nothing had failed.
//
// Conversely, **the fallback must not be deleted entirely**: the recovery path in resume.go relies on it ("better to reply
// 'this turn produced no reply' than stay silent": after a process restart the turn may really have produced nothing,
// and then the user must receive something to know they can ask again). Deleting it would turn (a) into a silent failure.
//
// # ⚠️ Reaching the fallback must leave a trace
//
// This branch used to log nothing at all, so "the user received that sentence" could not be found in production at all:
// searching for the text gave 0 hits, and 0 could mean it never happened or it was never logged; the two could not be told apart
// (the repo rule "0 hits must be paired with a positive control" is exactly this shape). The inbound.go side logs a Warn with diagnostic counts
// (frame count / message count / stop_reason) when it computes an empty answer, and this Warn in this function is
// **the fallback for every caller**: the two paths in resume.go do not go through that inbound code,
// and without it those two paths would go back to "zero trace".
func sendFinalReply(
	ctx context.Context, sender MessageSender, token, srcMessageID, runID, answer string,
	inThread bool, log *slog.Logger,
) error {
	answer = strings.TrimSpace(answer)
	if answer == "" {
		// Reaching here is case (a) above: really nothing was produced. An interrupted turn does not get here.
		log.Warn("最终答案为空，改发兜底提示（这一轮什么都没产出）",
			"run", runID, "message", srcMessageID)
		answer = replyNoAnswer
	}
	segments := splitReplyByTables(answer, maxCardTables)
	if len(segments) > 1 {
		log.Info("回复表格过多，切成多条发送", "run", runID, "segments", len(segments))
	}

	for i, segment := range segments {
		_, err := sender.ReplyCard(ctx, token, srcMessageID, lark.TextCard(segment), lark.ReplyCardOption{
			// Only the first segment opens a topic; the later ones follow in the same place.
			ReplyInThread:  inThread && i == 0,
			IdempotencyKey: segmentIdempotencyKey(runID, i, len(segments)),
		})
		if err != nil {
			// Failure midway: segments already sent are not recalled (and cannot be). Return the error as a whole so the upper layer
			// logs it, and on retry the idempotency key lets Feishu dedupe by itself, so the chat is not flooded with duplicates.
			return fmt.Errorf("发第 %d/%d 段回复失败: %w", i+1, len(segments), err)
		}
	}
	return nil
}

// markReceived adds the OnIt reaction to the user's message and returns the reaction ID.
//
// Failure is not fatal: it only loses one "got it" hint, and must not stop the whole turn from running.
func markReceived(ctx context.Context, reactor Reactor, token, messageID string, log *slog.Logger) string {
	if reactor == nil {
		return ""
	}
	id, err := reactor.AddReaction(ctx, token, messageID, lark.ReactionOnIt)
	if err != nil {
		log.Debug("打 OnIt 表情失败，忽略", "message", messageID, "error", err.Error())
		return ""
	}
	return id
}

// markDone switches "processing" to "done": remove OnIt first, then add DONE.
//
// # Why switch instead of just removing
//
// Only removing would make the message look "ignored" again, and users looking back at the chat could not tell "the bot answered"
// from "the bot never saw it". With DONE the message **always has exactly one reaction**, and it is the current state.
//
// # Neither step failing is fatal, but the order matters
//
// Remove first, then add: the other way round there is a moment with two reactions on the message, and if the removal fails,
// both stay forever; users see "processing" and "done" side by side and do not know which to believe.
//
// If removing succeeds but adding fails, it degrades to the old behavior (no reaction at all), which is acceptable: the answer has been sent.
func markDone(ctx context.Context, reactor Reactor, token, messageID, reactionID string, log *slog.Logger) {
	if reactor == nil {
		return
	}
	// An empty reactionID means OnIt was never added (e.g. missing permission), so there is nothing to remove,
	// but DONE should still be tried: the two are independent permission/timing issues.
	if reactionID != "" {
		if err := reactor.RemoveReaction(ctx, token, messageID, reactionID); err != nil {
			// If it cannot be removed, **do not add** DONE: otherwise there would be two reactions side by side, more confusing than one fewer.
			log.Debug("撤 OnIt 表情失败，跳过打 DONE 以免两个表情并列",
				"message", messageID, "error", err.Error())
			return
		}
	}
	if _, err := reactor.AddReaction(ctx, token, messageID, lark.ReactionDone); err != nil {
		log.Debug("打 DONE 表情失败，忽略", "message", messageID, "error", err.Error())
	}
}

// clearReceived only removes OnIt, without adding DONE. For failure paths.
//
// When this turn did not run, **DONE must not be added**: that tells the user "answered", while they really got nothing.
// Removing OnIt makes the message look "unhandled" again, and together with the error explanation sent by dispatch,
// the user can understand what happened.
func clearReceived(ctx context.Context, reactor Reactor, token, messageID, reactionID string, log *slog.Logger) {
	if reactor == nil || reactionID == "" {
		return
	}
	if err := reactor.RemoveReaction(ctx, token, messageID, reactionID); err != nil {
		log.Debug("撤 OnIt 表情失败，忽略", "message", messageID, "error", err.Error())
	}
}

// markQueued adds an hourglass reaction to the message "queued behind the previous turn".
//
// # Why queueing must have its own reaction
//
// This is **the only visible feedback** of the "queue" mode: the bot does not answer right away, and people in the chat cannot tell
// whether it received it and is waiting, or never saw it. Together with the replyBusy sentence, the two say "received,
// waiting for the previous turn".
//
// Using a different reaction from redirect is deliberate; see the note on lark.ReactionRedirected: both mean
// "received", but with opposite consequences (one waits, one kills the previous turn).
//
// Failure is not fatal: one hint fewer, and the replyBusy sentence is still sent.
func markQueued(ctx context.Context, reactor Reactor, token, messageID string, log *slog.Logger) {
	if reactor == nil {
		return
	}
	if _, err := reactor.AddReaction(ctx, token, messageID, lark.ReactionQueued); err != nil {
		log.Debug("打排队表情失败，忽略", "message", messageID, "error", err.Error())
	}
}

// markRedirected adds a reaction to the message that "already interrupted the previous turn and is doing this one instead".
//
// **This one matters more than the queue one**: redirect is destructive (the previous turn really was killed), and what users
// directly perceive is "the bot suddenly stopped talking about the previous thing". Without this reaction, that interrupt looks like a failure.
func markRedirected(ctx context.Context, reactor Reactor, token, messageID string, log *slog.Logger) {
	if reactor == nil {
		return
	}
	if _, err := reactor.AddReaction(ctx, token, messageID, lark.ReactionRedirected); err != nil {
		log.Debug("打转向表情失败，忽略", "message", messageID, "error", err.Error())
	}
}

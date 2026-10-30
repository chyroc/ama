// Observability for "the message arrived, but did not trigger".
//
// # This file is a direct product of a production investigation
//
// On 2026-09-01 we debugged "several messages were sent in the alert chat, and the bot did not react at all", and got stuck for a long time,
// because the production logs **had only two lines about that app**:
//
//	lark/longconn.go:269 飞书长连接建立中
//	lark/longconn.go:308 飞书长连接已就绪，开始收消息
//
// And then nothing. Not a single line from inbound.go. And these two lines fit two cases that **call for opposite actions**:
//
//	A. Feishu never pushed the messages (permission not granted / the bot is not in the chat / the connection is actually dead)
//	B. The messages were pushed, but quietly rejected by the three "do not trigger" gates at the start of Handle
//
// **A means checking the Feishu side, B means checking our code**, and the logs could not tell them apart, because all three
// branches of B were `log.Debug`, while the production log level is Info (newLogger in `cmd/amad/main.go`,
// the source of truth for the level). Debug effectively does not exist in production, so B looked word for word identical to A.
//
// The comment in inbound.go already said "⚠️ every branch that does not trigger must state its reason",
// and the reason was right too, but it chose the Debug level, so **this defense never took effect in production**.
// This file makes it really visible in production.
//
// # Why not just change those three Debug lines to Info
//
// Because it would flood the log, and flooding is itself a failure. These branches cover "every ordinary message in a chat that does not @
// the bot": an active big chat has thousands a day, and logging all of them at Info would drown the log,
// so the lines that really matter could not be found, which costs more than now.
//
// So **rate limiting** is done here: at most one line per (app, reason) per dropWindow,
// carrying "how many were held back in this window". So:
//
//   - when debugging and wanting to know "did the message come in at all", one line is enough; that is the most important question;
//   - active chats flooding: blocked by rate limiting, capped at one a minute;
//   - "how many in total" is not lost: the suppressed count accounts for the ones held back,
//     so seeing one does not make you think only one came (which would be another kind of misleading).
//
// # Why by (app, reason) and not just by app
//
// One app hitting two reasons at once really happens: an alert chat has both bot notices (reason A)
// and pure images sent by people (reason B). Rate limiting by app alone would let whichever comes first cover the other reason
// for a whole minute, with no hint that "what was covered happens to be the one you are looking for".
// Counting separately means they do not hide each other, at the cost of a few more keys in the map.
//
// # Keeping the state in process is fine
//
// CLAUDE.md requires state where "losing it breaks functionality" to live outside the process. That deliberately does not apply here:
// it is **pure rate-limit counting**; losing it at worst means a few more log lines (with multiple instances in production each logs on its own anyway),
// and affects no behavior. It belongs to the "can be lost at any time, and losing it only makes things slower" kind that rule explicitly allows.
//
// The map does not grow without limit either: the key is (app, reason), there are only the three reason constants below, and the number of apps equals the connected Feishu
// apps (single digits in production), so the upper bound is "number of apps × 3", not growing with message volume. **No cleanup is needed**.
package feishu

import (
	"log/slog"
	"sync"
	"time"

	"github.com/chyroc/ama/server/shared/infra/lark"
)

// dropWindow is the minimum interval between two log lines for the same (app, reason).
//
// One minute is set for the use "a person is debugging": the person investigating usually asks the other side to send another message to try,
// and within a minute there will surely be one record that really made it into the log. Shorter risks flooding; longer would make
// the most natural check, "send another one and see", wait too long.
const dropWindow = time.Minute

// Reasons for not triggering. **These strings go into the log and are also the rate-limit grouping keys**, so they must be stable and searchable.
//
// Deliberately Chinese phrases rather than error codes: the logs are read by people, and "dropIneligible" would need another look at
// the code to know what it means.
const (
	// dropReasonIneligible: no @, and not in a topic.
	//
	// ⚠️ Before 2026-09-01 this also included "sent by a bot"; it no longer does: third-party bots
	// are treated the same as real people, and only messages from our own are blocked (those go to dropReasonSelfMessage).
	dropReasonIneligible = "没有 @ 机器人，且这条消息不在话题里"
	// dropReasonNotTopicRoot: in a topic, but **not the topic root** (a reply / descendant).
	//
	// ⚠️ **It is deliberately separate from dropReasonIneligible**: that one would send people to check "how did thread_id
	// get lost", while the truth is it is in a topic, just not the opening message, and by design it needs an @.
	// Added on 2026-09-02, together with the "no-@ only for topic roots" fix; see the trigger.go file header.
	dropReasonNotTopicRoot = "这条是话题内的跟帖（不是开题那条），免 @ 只给新话题，跟帖要 @ 机器人"
	// dropReasonEmptyText: @-mentioned (or qualified), but the body is empty.
	dropReasonEmptyText = "正文为空"
	// dropReasonModeOff: in a topic and qualified, but this app has not turned on the no-@ trigger.
	dropReasonModeOff = "话题内免 @ 触发未开启（应用的触发模式是 mention_only）"
	// dropReasonSelfMessage: sent by our own (this app's bot, or the bot of another AMA
	// app in this tenant). **Blocking it means self-trigger prevention is working, not a failure**, but it must be said,
	// otherwise "why does the bot ignore the message it sent itself" cannot be explained. See isSelfMessage in trigger.go.
	dropReasonSelfMessage = "这条是我们自己的机器人发的（防自触发）"
	// dropReasonAppLookupFailed: this Feishu app cannot be found, fail-closed and do not trigger.
	// It is separate because it is **a real failure** (a store hiccup or inconsistent data), unlike the ones above that are "just the rules".
	dropReasonAppLookupFailed = "查不到这个飞书应用（读库失败，保守不触发）"
)

// dropReporter writes rate-limited logs for messages that were "received but did not trigger".
//
// **Always create it with newDropReporter**; the zero value is not usable (`now` is nil). Safe for concurrent use.
//
// ⚠️ This deliberately does **not** make "the zero value also works". This is a type on the logging path, and the worst
// failure on the logging path is a panic, which drags down the whole turn, far worse than "one log line fewer". If a fallback is really needed,
// the right way is for admit to tolerate it itself (see the two lines about now below), not to add a pile of
// nil checks in report: those are easy to miss, and the missed one is exactly the one reached when things go wrong.
type dropReporter struct {
	now func() time.Time

	mu     sync.Mutex
	state  map[dropKey]*dropState
	window time.Duration
}

// dropKey is the rate-limit grouping key. See "Why by (app, reason)" in the file header.
type dropKey struct {
	appID  string
	reason string
}

type dropState struct {
	// lastLoggedAt is when this group last really logged a line.
	lastLoggedAt time.Time
	// suppressed is the number held back by rate limiting since then.
	suppressed int
}

// newDropReporter creates a rate limiter. When now is nil it uses time.Now.
func newDropReporter(now func() time.Time) *dropReporter {
	if now == nil {
		now = time.Now
	}
	return &dropReporter{now: now, state: make(map[dropKey]*dropState), window: dropWindow}
}

// report records one "received but did not trigger".
//
// **The level is Info**, which is exactly why this file exists: Debug effectively does not exist in production.
//
// ⚠️ **The body never goes into the log.** Everything carried here is metadata needed for diagnosis (whose app, which chat,
// what type, is there a thread_id, is the sender a person or a bot). To confirm "is it this one",
// messageID is enough; the body is user data (same as the assertion in progress_logging_test.go).
func (r *dropReporter) report(log *slog.Logger, msg *lark.InboundMessage, reason string) {
	if r == nil || msg == nil {
		return
	}
	suppressed, ok := r.admit(msg.AppID, reason)
	if !ok {
		return
	}
	window := r.window
	if window <= 0 {
		window = dropWindow
	}
	log.Info("收到飞书消息但没有触发 agent",
		"reason", reason,
		"chatType", msg.ChatType,
		"senderType", msg.SenderType,
		"mentioned", msg.Mentioned,
		"thread", msg.ThreadID,
		// How many more with the same reason were held back in this window. 0 means just this one, not "many more unsaid".
		"suppressed", suppressed,
		"window", window.String())
}

// admit decides whether this one should really be printed, and returns how many were held back in the previous window.
func (r *dropReporter) admit(appID, reason string) (suppressed int, ok bool) {
	key := dropKey{appID: appID, reason: reason}

	r.mu.Lock()
	defer r.mu.Unlock()

	// now / window / state all have their fallbacks here, instead of relying on the caller.
	// See the type comment for why: this is the logging path, and a panic costs the whole turn, far more than one log line fewer.
	now := time.Now()
	if r.now != nil {
		now = r.now()
	}
	window := r.window
	if window <= 0 {
		window = dropWindow
	}
	if r.state == nil {
		r.state = make(map[dropKey]*dropState)
	}

	st := r.state[key]
	if st == nil {
		// The first time this group appears: let it through. **The first one must be printed**: the most important
		// piece of information when debugging ("the message really came in") is in it.
		r.state[key] = &dropState{lastLoggedAt: now}
		return 0, true
	}
	if now.Sub(st.lastLoggedAt) < window {
		st.suppressed++
		return 0, false
	}
	suppressed = st.suppressed
	st.lastLoggedAt = now
	st.suppressed = 0
	return suppressed, true
}

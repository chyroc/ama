// **Event wakeups** for poll: when there is a new message, wake that environment's poll loop right away.
//
// # Why it is needed
//
// Stepped backoff (see workerbackoff.go) brings the idle poll interval all the way down to 60 seconds, which drops RPM from
// ~630 to single digits. **The cost is "when there is work, finding it can take up to 60 seconds"**, which a chat product
// cannot accept: the user sends a message and the agent takes a minute to start calling tools.
//
// So lowering the frequency and waking up are **a pair**; lowering the frequency without wakeups just "hides the problem behind longer intervals".
// Only together do they give "almost no requests when idle, instant response when there is work".
//
// # ⚠️ The wakeup signal must cross pods: this is the only hard part
//
// After leader election (see workerlease.go) an env has only **one** poll loop globally, running on the pod holding the lease.
// And the user's message HTTP request lands randomly through the load balancer, **usually not on that pod**:
//
//	user sends a message → lands on pod B → while the env's poll loop is on pod A
//
// So an in-process `chan struct{}` is **useless** here: pod B wakes its own loop that does not exist,
// and pod A keeps sleeping until the next interval. A single local process never finds this out, since then the receiver of the message and the poller
// are always the same process. This has the same shape as the production incident with terminal keystrokes.
//
// # ⚠️ Why not Redis pub/sub
//
// **This was researched; do not go down that road again** (full reasoning in repository/schema/mq_messages.sql): managed Redis
// is commonly a proxy-based cluster, **pub/sub does not cross replicas and the proxy layer does not route subscriptions**, and amad's instances may be spread over
// multiple data centers. The most damning point:
// "**The client library has Subscribe and it works. Precisely because it works it is more dangerous: it drops messages now and then, and only across data centers**".
// That failure looks like "fast most of the time, occasionally one message waits 60 seconds", nearly impossible to track down.
//
// It uses **shared/infra/pgmq** (PG LISTEN/NOTIFY, measured p50 0.08ms), the same path used to relay terminal keystrokes
// across instances. The payload is just one envID, far below NOTIFY's 8000-byte limit.
package core

import (
	"context"
	"log/slog"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
)

// WorkerWakeups abstracts "wake an environment's poll loop across pods".
//
// **Declared locally in core instead of importing shared/infra/pgmq**: core should not depend on a specific queue implementation.
// Same as TerminalRelay / Leases (cmd/amad is responsible for hooking pgmq up).
//
// Its shape is deliberately kept the same as TerminalRelay: both are backed by the same pgmq.Queue,
// only the meaning of topic and payload differs.
type WorkerWakeups interface {
	// Wake sends a signal "this environment may have new work".
	//
	// ⚠️ **It is a hint, not a command**: not received, received twice, or received late does not affect correctness
	// (the worst case falls back to "wait for the next backoff interval"). So callers **must not** fail the business operation because it failed,
	// and must not add retries for it either; the message-sending path should not be slowed down by a pure optimization.
	Wake(ctx context.Context, envID string) error

	// WatchWakeups keeps consuming an environment's wakeup signals until ctx ends.
	//
	// fn may be called any number of times (including duplicate deliveries); the implementation must be idempotent. See pollWaker.signal.
	WatchWakeups(ctx context.Context, envID string, fn func(context.Context) error) error
}

// WorkerWakeTTL is how long one wakeup signal stays valid.
//
// **It must be short.** The whole point of the signal is "there is work now, do not sleep", and old signals piling in after a pod rebuild
// only fire a series of useless polls, exactly what this change is meant to save. 60 seconds = the top of the backoff steps
// (workerBackoffMax): for a signal older than that, the poll loop has already woken once on its own,
// so that signal is surely redundant.
//
// Same reasoning as the ttl of TerminalRelay.Publish ("old keystrokes piling into the container after reconnect are harmful").
const WorkerWakeTTL = 60 * time.Second

// WorkerWakeTopic is the topic of an environment's wakeup signals.
//
// ⚠️ **Exporting it is deliberate**: the implementation lives in cmd/amad (the layer that hooks pgmq up), and it must build the topic
// **exactly the same way** as the subscriber. If each side wrote its own, a mismatch would mean
// "the wakeup was sent but nobody received it", zero errors, only showing as slower responses. Exporting makes them share one source at compile time.
//
// ⚠️ One topic per env, **not one global topic**: pgmq's Consume means "drain this topic",
// so with one global topic every pod would take other environments' signals and delete them (a consumed message is gone),
// which is "the wakeup signal got eaten by someone else", zero errors, showing as "sometimes wakeups do not work".
//
// ⚠️ pgmq hashes the topic internally into `ama_mq_` + hex as the channel name (LISTEN channel names are limited to
// 63 characters), so the length here is unrestricted, but keep it short for easier debugging.
func WorkerWakeTopic(envID string) string { return "workerwake:" + envID }

// pollWaker is the wakeup receiver on the poll loop side.
//
// # Coalescing is why it exists
//
// One conversation may send several messages in a row (a user typing fast, several people talking at once in a Feishu group), and each sends a
// wakeup signal. **Without coalescing, one conversation fires a string of extra polls**, and the requests saved by lowering frequency are
// spent again by wakeups.
//
// It uses a chan of capacity 1 + non-blocking send: when a signal is already waiting to be taken, later ones are just dropped.
// That is correct because the signal **carries no information**: it only says "wake up", and waking once or ten times has exactly the same effect.
// pgmq uses the same trick internally to wake its Consume loop (see the comment on its `woke` chan).
type pollWaker struct {
	// ch has capacity 1. **Do not make it larger**: capacity N means "drop only after N pile up", which just delays coalescing,
	// and each of the extra N-1 wakeups fires its own poll.
	ch chan struct{}
}

func newPollWaker() *pollWaker { return &pollWaker{ch: make(chan struct{}, 1)} }

// signal records "time to wake up". Non-blocking; may be called from any goroutine at any rate.
func (w *pollWaker) signal() {
	if w == nil {
		return
	}
	select {
	case w.ch <- struct{}{}:
	default:
		// A wakeup is already pending; merge this one into it. See the type comment.
	}
}

// wait waits until "someone wakes it" or "slept for d", and returns false if ctx has ended.
//
// ⚠️ **Wakeup and the timer are two parallel exits; the wakeup does not replace the timer**: a wakeup may never come
// (Wakeups not configured, pgmq down, signal lost during a pod rebuild), and then the timer must be the fallback.
// Waiting only for wakeups, one lost signal = this environment never polls again.
func (w *pollWaker) wait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	if w == nil {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return true
		}
	}
	select {
	case <-ctx.Done():
		return false
	case <-w.ch:
		return true
	case <-timer.C:
		return true
	}
}

// drain clears one pending wakeup signal.
//
// Used "right after getting work": that round goes straight back to poll anyway, and keeping the signal would make the next
// wait return at once and fire a useless poll. Non-blocking; does nothing when there is no signal.
func (w *pollWaker) drain() {
	if w == nil {
		return
	}
	select {
	case <-w.ch:
	default:
	}
}

// watchWakeups starts a goroutine subscribing to this environment's wakeup signals, and returns the receiver the poll loop waits on.
//
// When Wakeups is not configured it returns nil. `pollWaker`'s methods all check for nil, so callers need no branch
// (on a nil receiver wait degrades to a pure timer, that is, "stepped backoff only, no wakeups").
//
// ⚠️ **A failed subscription must not stop poll**: wakeups are a pure optimization, and when they fail it must fall back to "rely on the timer",
// not "this environment stops polling". So it only logs one Warn and moves on.
func (p *WorkerPool) watchWakeups(ctx context.Context, envID string, logger *slog.Logger) *pollWaker {
	if p.cfg.Wakeups == nil {
		return nil
	}
	waker := newPollWaker()
	go func() {
		// Consume normally only returns when ctx ends. An error midway (database unreachable) is degraded the same way:
		// leave a log line, and this environment is left with just the timer.
		err := p.cfg.Wakeups.WatchWakeups(ctx, envID, func(context.Context) error {
			waker.signal()
			return nil
		})
		if err != nil && ctx.Err() == nil {
			logger.Warn("订阅唤醒信号失败，本环境退化成只靠退避定时器（响应会变慢，但不影响正确性）",
				slog.Any("err", err))
		}
	}()
	return waker
}

// wakeEnvForRun wakes the poll loop of "the environment this run is in".
//
// # Which point it hangs on, and why not the others
//
// It hangs on `AppendRunEvents`, the moment **the user/system sends events into Ark**, which is
// **the single convergence point of the entry paths** named in the comment on sendEventsThroughWindow:
//
//	chat page HTTP / new Feishu conversation / Feishu follow-up / scheduled task / agent self-wake
//
// The three places the user named (api / web / Feishu) are all in there, and **only this one point needs a hook**:
// they all pass through here anyway (that is exactly why the "disabled" gate was put here originally).
// Hooking every outer HTTP handler would be wrong: it would miss Feishu follow-ups and scheduled tasks,
// and those two are exactly the paths "nobody is watching".
//
// ⚠️ **Pure interrupts do not wake**: the caller (AppendRunEvents) blocks them with `isInterruptOnly(raw)`.
// An interrupt stops work rather than producing it, so waking for it fires a useless poll; more importantly, `RedirectRun`
// calls AppendRunEvents twice in a row (first user.interrupt, then user.message),
// so without blocking, one redirect wakes twice. The check sits in the caller rather than here, to reuse the existing
// `isInterruptOnly` there; two separate checks drift apart sooner or later.
//
// ⚠️ **Deliberately not hooked on `CreateSession`.** Creating a session produces no work by itself;
// what really makes Ark dispatch work is the user.message that follows, and that goes through here. Hooking session creation
// would only fire one useless poll every time a new conversation starts.
//
// ⚠️ **A plain-text reply turn really is a wasted wakeup**, and that waste is accepted on purpose: at the moment the message is sent
// **nobody knows** whether the agent will call a tool (that is only known after the model finishes inference). Avoiding it would mean
// waiting for a tool_use event, but by then the work is already sitting in the queue and the wakeup is pointless.
// The cost is small too: one extra poll ≈ 1 request, and in exchange "when a tool really is called, there is no waiting for an interval".
//
// # Failures are always swallowed
//
// A wakeup is a hint, not a command (see WorkerWakeups.Wake). Failing to send it at worst means "wait for the next backoff interval";
// **it must never affect sending the message itself**, which would turn a pure optimization into a new failure point on the chat path.
func (s *Service) wakeEnvForRun(ctx context.Context, run apimodel.Run) {
	if s.workerWakeups == nil {
		return
	}
	binding, err := s.store.GetArkBinding(ctx, run.WorkspaceID, run.AgentID)
	if err != nil || binding.ArkEnvironmentID == "" {
		// Without a binding there is no way to know which environment to wake. Not an error (the binding may have just been deleted),
		// and **doing nothing on this path is safe**: the poll loop keeps waking on its steps.
		return
	}
	if err := s.workerWakeups.Wake(ctx, binding.ArkEnvironmentID); err != nil {
		s.log().Warn("发唤醒信号失败，这一轮要等退避定时器（不影响正确性）",
			"environment", binding.ArkEnvironmentID, "error", err.Error())
	}
}

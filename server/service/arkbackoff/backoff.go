// **Stepped backoff** for Ark polling: the idler, the slower, at most one request every 60 seconds.
//
// # What this package is for
//
// Two things, both "do not send requests too densely": this file is **idle slowdown** (the less work,
// the longer the wait),
// and ratelimit.go is **the whole environment backing off together after a 429**. The two are chained,
// not competing;
// for the rule see the file header of ratelimit.go.
//
// # ⚠️ Why it can be its own package (while the rest of the worker cannot)
//
// These two files have **zero references** to `*core.Service` and `WorkerPool`; the measured check
// was copying them
// alone into an empty package, where `go vet`'s undefined was only one `arkSleep` (now taken into this
// package,
// see Sleep). The other seven worker files all need `WorkerPool`, so they stay in core.
//
// # Why a fixed [1s,3s) is not enough
//
// Ark's block_ms is capped at 999 ms (arkPollBlockMS, a hard limit in the docs), so a "long poll" can
// hang at most
// 1 second; when idle that loop is essentially a timer. It used to be jitter(1s,3s), ~30 req/min per
// loop;
// in production 7 environments × 3 pods = 21 loops added up to ~630 RPM, **all spinning idle** (0
// works claimed in the same window).
// What gets hit is the account-level RPM quota, shared by all workers.
//
// Leader election (core/workerlease.go) removes the ×3 pods dimension, leaving ~210 RPM. But **the
// environment-count dimension remains**,
// and it only grows with the business: 7 environments is now, not a ceiling. So it also slows down
// by "how long it has been idle".
//
// # The ladder and the trigger count per level
//
//	0     1     2     3     4     5   ← level
//	1s   1s    10s   20s   30s   60s  ← interval (each with jitter)
//	      ↑ Step(=3) consecutive empty polls go up one level
//
// **Slowing down is "quick to back off, slow to climb"**: 3 consecutive empty polls go up one level.
// Why 3 rather than 1 or 10:
//
//   - 1 (every empty poll goes up) would let the normal gap "between two tool calls in one turn"
//     push the level up too, slowing down active sessions as well;
//   - 10 would need 10 idle spins per level, 50 requests to climb from 1s to 60s,
//     and most environments **are empty all day**, so those 50 times the number of environments is
//     quota burned for nothing;
//   - 3 reaches the top in only 15 requests (about 3 minutes), avoiding single jitters,
//     while letting "environments unused for a long time" sink to the lowest rate quickly.
//
// # ⚠️ Every level must have jitter
//
// Do not degrade to fixed values. Several loops in lockstep stack requests into periodic spikes, **and
// rate limits are hit even when the average RPM is fine**;
// that is exactly the old comment's line "21 loops in lockstep stack requests into spikes, which is no
// slowdown at all".
// After the ladder there are fewer loops, but the few at the same level still line up (they often
// start at the same time,
// and spin idle to the same level at the same time), so not a single jitter can be dropped.
//
// # ⚠️⚠️ Claiming work must return to the fastest level at once; missing it is a silent bug
//
// See Ladder.Reset. The symptom of missing it is "active environments respond slower and slower",
// **with zero errors**:
// an environment in frequent use would climb to the 60-second level after a few empty polls, and
// **never come back**.
// This is the easiest to miss and hardest to debug in this change, so a dedicated test locks it
// (core's TestWorkerBackoffResetsAfterWork).
package arkbackoff

import (
	"context"
	mrand "math/rand"
	"time"
)

const (
	// Step is "how many consecutive empty polls go up one level"; for why see the file header.
	Step = 3
	// Max is the ladder's cap; it goes no higher.
	//
	// The 60-second rule is **the fallback for responsiveness**: when wakeup (core/workerwake.go) is
	// broken, this is
	// "at worst how long until a user's message is noticed". Longer would make the degraded experience
	// unacceptable.
	Max = 60 * time.Second
	// JitterRatio is the jitter ratio added on top of each level's interval (±20%).
	//
	// A **ratio** rather than a fixed value: ±3 seconds on the 0.5-second level would go negative or
	// multiply several times,
	// while ±300 ms on the 60-second level would be no jitter at all.
	JitterRatio = 0.2
)

// Levels is the idle backoff ladder, from fastest to slowest.
//
// ⚠️ **It starts at 1 second rather than 0.5 seconds**, a deliberate tradeoff:
//
// Starting at 0.5 seconds only makes sense when "work was just claimed and another is coming soon",
// and that case is exactly
// **covered by the wakeup mechanism**: when the agent calls tools in a row in one turn, the new event
// each work produces
// triggers a wakeup (see core/workerwake.go), and the poll loop wakes at once, with no need for
// shorter polling to catch it.
//
// And the cost of starting at 0.5 seconds is certain: the RPM of this level during active periods
// **doubles** (120 → 60 req/min/environment).
// Trading a certain doubled quota use for a case already covered by wakeup is not worth it.
//
// Keeping 1 second has another benefit: it matches the lower bound of the SDK's own `jitter(1s, 3s)`,
// so "how fast can it be at most" does not get two answers in two places.
//
// ⚠️ **The requirement's ladder is 0.5s → 1s → 10s → …; there is only one 1-second level here, not a
// missing level.**
// With 0.5 seconds removed, the first two levels are both 1 second; keeping a duplicate level is not
// just redundant, it makes
// "went up one level" **unobservable** on the first climb (the interval did not change), so
// the `TestPollLoopResetsBackoffOnWork` test cannot tell "reset" from "not reset",
// becoming a fake test that is always green. We actually hit it: during mutation testing, deleting
// ladder.Reset() it stayed green.
//
// ⚠️ **This is a package-level slice; callers only read it, do not change it**. Changing it would
// affect the poll pace of all environments at once,
// and nothing would report an error about it.
var Levels = []time.Duration{
	1 * time.Second,
	10 * time.Second,
	20 * time.Second,
	30 * time.Second,
	Max,
}

// Ladder tracks "how long this environment has been idle", and from it gives how long to sleep
// before the next poll.
//
// **Not concurrency-safe**: each pollLoop holds its own, read and written only in that loop's own
// goroutine.
type Ladder struct {
	// level is the current level, an index into Levels.
	level int
	// idle is how many consecutive empty polls at the current level; at Step it goes up one level.
	idle int
}

// Next records one **empty poll**, and returns how long to sleep this time.
func (b *Ladder) Next() time.Duration {
	b.idle++
	if b.idle >= Step {
		b.idle = 0
		if b.level < len(Levels)-1 {
			b.level++
		}
	}
	return Jitter(Levels[b.level])
}

// Reset returns to the fastest level. **It must be called after claiming work**.
//
// ⚠️ Missing it is the silent bug from the file header: active environments climb all the way to the
// 60-second level and never come back,
// showing up as "the more it is used, the slower it gets" with no error. core's
// `TestWorkerBackoffResetsAfterWork` locks this in.
func (b *Ladder) Reset() {
	b.level = 0
	b.idle = 0
}

// Level is the current level, only for tests and logs.
func (b *Ladder) Level() int { return b.level }

// Jitter adds a ±JitterRatio jitter to an interval.
//
// See "every level must have jitter" in the file header: without it, several loops at the same level
// move in lockstep,
// stacking requests into periodic spikes, hitting rate limits even when the average is fine.
func Jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	span := float64(d) * JitterRatio
	// An offset in [-span, +span).
	delta := (mrand.Float64()*2 - 1) * span
	out := time.Duration(float64(d) + delta)
	if out <= 0 {
		return d
	}
	return out
}

// Sleep waits for a while, returning false early when ctx is cancelled.
//
// This package took it in because Gate.wait needs it (backoff must not sleep blindly; it must be
// interruptible by shutdown).
// core's pollLoop uses the same one, so core/workerbackoff.go keeps an alias forwarding here;
// **only one implementation is kept**: of two ctx-aware sleeps, one would sooner or later miss
// ctx.Done(),
// and that bug shows up as "the service will not shut down", not pointing at sleep.
func Sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

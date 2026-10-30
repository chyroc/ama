// Rate-limit backoff for the Ark API: on a 429 the **whole environment**'s concurrent runners back
// off together.
//
// Split with backoff.go: that one **slows down when idle**, this one **backs off after hitting the
// rate limit**.
// The two are chained, not competing; see "why this does not fight arkBackoff" below.
//
// # Why this layer is needed (a storm measured locally)
//
// In a local run, 15 concurrent works produced 6087 instances of
// `worker api status 429: The RPM (Requests Per Minute) limit ... exceeded` in 15 minutes,
// steady at ~400 requests/minute, stopping only when the process exited.
//
// ⚠️ The first diagnosis was "some work never exits and ran for 8 hours", which was **wrong**:
// the timestamp in the session id is UTC, while the log's `time=` is +08:00, exactly 8 hours apart.
// The "claimed work" times of the 15 works matched their ids' timestamps to the second; they only
// lived a dozen minutes or so.
// The real shape is **concurrency × fixed polling rate**, not a single one never exiting.
//
// # The gap lies between two quotas
//
// The concurrency cap (`core.WorkerConfig.Concurrency`, default 20) controls **how many containers
// the host can run at once**;
// it only answers "can the containers cope". But each concurrent work runs its own
// SDK session-tool-runner, each hitting ListEvents independently on a fixed ticker, so what really
// gets hit is
// **another quota: the Ark account's RPM**. The product "number of concurrent works × polling rate
// of each" is covered by nothing,
// and the gap between the two quotas is what this layer fills.
//
// # Why backoff must be shared per environment, rather than each runner backing off on its own
//
// The key is that **the rate limit is account-level**. With each runner doing its own exponential
// backoff, each looks polite,
// but N staggered retries still add up to a steady high-frequency stream: 15 each backing off to
// once per second
// still add up to 900 requests/minute. So the gate is shared per environment: when one runner hits a
// 429,
// the other runners in the same environment wait together before their next call, and the request
// volume really drops.
//
// ⚠️ **"Shared per environment" is in-process sharing, which is right; do not make it distributed**:
// it treats
// "these N runners in this process must not hammer together". The cross-pod dimension is handled by
// leader election (core/workerlease.go):
// one environment has only one poll loop globally. The two handle different dimensions; do not use
// this to replace leader election.
//
// # Why only back off on rate limits
//
// The 4xx classification in `arkFatalStatus` is deliberate: 4xx other than timeout/conflict/rate
// limit are configuration
// problems (environment does not exist, key has no permission), where retries do not help; they must
// surface to the upper layer to reconnect and log.
// This layer **only recognizes 429**; other errors pass through as is, without changing existing
// meaning.
//
// # Why it is added here rather than changing the SDK
//
// The SDK is in the module cache (read-only). And its own two other paths **already have backoff**:
// `reconcile` uses exponential backoff + jitter, `retrySendEvent` sleeps linearly between retries;
// only the fallback `consumeList` loop just logs a Warn and breaks when ListEvents fails,
// then goes back to the fixed ticker (`session_tool_runner.go:487-491`). Two of three paths have it,
// one does not, which says it is an oversight rather than design. What we can change is the
// `selfhosted.API` passed to it,
// and each work already gets our wrapper layer (see shutdownSafeAPI in
// core/workersendguard.go),
// so adding it here is cheapest.
package arkbackoff

import (
	"context"
	"log/slog"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/volcengine/ark-runtime-go/arkruntime/selfhosted"
)

const (
	// RateLimitStart / RateLimitCap are the start and cap of the rate-limit backoff.
	//
	// The values match the SDK's own path that has backoff (`sessionRunnerStreamBackoffStart` =
	// 500ms,
	// `sessionRunnerStreamBackoffCap` = 10s). **Deliberately not inventing our own**: two backoff
	// paces on the same API
	// would make "how often to retry, really" something nobody can explain.
	RateLimitStart = 500 * time.Millisecond
	RateLimitCap   = 10 * time.Second
	// RateLimitHealthyAfter is "how long it must stay quiet to count as really recovered": only when
	// more than this has passed since the last 429
	// does a success reset the backoff ladder to zero.
	//
	// The value is also copied from the SDK: `sessionRunnerStreamHealthyAfter` = 30s
	// (session_tool_runner.go:24, used at :347: after a stream reconnect it must be **healthy for 30
	// seconds in a row**
	// before backoff goes back to the start). We copied its Start/Cap back then, **but missed this
	// one**,
	// so "when does it count as recovered" got different answers on the two sides, and the missing
	// half is exactly the incident below.
	RateLimitHealthyAfter = 30 * time.Second
)

// Gate is a rate-limit gate shared by one environment.
//
// Concurrency-safe: several works in the same environment share one instance, touching it before and
// after every API call.
type Gate struct {
	log *slog.Logger

	mu sync.Mutex
	// until is the moment "do not send requests before this". Zero means not backing off.
	until time.Time
	// backoff is how long to wait on the next rate-limit hit. Each success steps down one level;
	// only after RateLimitHealthyAfter of quiet
	// is it reset to zero; see Observe.
	backoff time.Duration
	// lastHit is the time of the last 429; zero means none yet.
	// It is the only basis for "how long it has been quiet": successful calls are not logged, so it
	// can only be measured from this end.
	lastHit time.Time
	// hits counts the total rate-limit hits, only for logs (making "backoff is really working"
	// visible).
	hits int
}

// NewGate builds a rate-limit gate. When logger is nil it uses slog.Default().
func NewGate(logger *slog.Logger) *Gate {
	if logger == nil {
		logger = slog.Default()
	}
	return &Gate{log: logger}
}

// Wait waits until expiry while the gate is closed, or until ctx ends. Returns false when ctx is
// cancelled.
//
// **It does not sleep holding the lock**: the lock must be released while sleeping, otherwise other
// runners in the same environment could not even read the gate,
// and backoff would become serialization, turning "back off together" into "queue up one by one".
func (g *Gate) Wait(ctx context.Context) bool {
	for {
		g.mu.Lock()
		d := time.Until(g.until)
		g.mu.Unlock()
		if d <= 0 {
			return ctx.Err() == nil
		}
		if !Sleep(ctx, d) {
			return false
		}
	}
}

// Observe feeds the result of one API call to the gate: a rate-limit hit extends the backoff, a
// success **steps down one level**.
//
// Only 429 counts. Other errors (including network errors, 5xx) pass as is; their retry strategy is
// elsewhere,
// and this layer stays out; see "why only back off on rate limits" in the file header.
//
// # ⚠️ Why success "steps down one level" rather than "resets to zero" (a production P1)
//
// This used to be `backoff = 0; until = zero time`: **one success reset the whole environment's
// backoff**.
// With a single runner that is right; with several sharing one gate it is backwards:
//
//	9 runners hit together → some 429, some succeed
//	     ↓                        ↓
//	Observe(429) raises it to 500ms   Observe(nil) resets it, the gate opens
//	     └────── the latter wins, because it only needs one ──────┘
//
// And 429 is never a 100% hit (Ark's RPM is a leaky bucket; some requests always squeeze in), so the
// backoff
// **can never climb**. The fingerprint measured in production was solid: 947 backoff log lines in a
// 45-minute window, of which
// **73.1% of waits were in 250~500ms (the starting level), and only 0.6% ever reached the 10-second
// cap**;
// exponential backoff should "wait longer the more it hits, quickly reaching the cap", yet three
// quarters stayed at the start. The result was continuous 429s,
// one environment hitting the rate limit 48 times/minute, with a median interval of 3 seconds.
//
// **A shared gate shares the punishment, and also the pardon**, and the pardon only needs any one of
// the 9.
//
// The fix turns "recovery" from an event into a stretch of state, with both of these:
//
//   - **A success only steps down one level** (halving), and during the healthy period **not below
//     the start**. 1 success out of 9 only pushes
//     the ladder down one notch, and the other 8 429s raise it right back; under continuous rate
//     limiting the ladder steadily climbs to the cap.
//   - **The only way to really reset: RateLimitHealthyAfter (30s) of quiet since the last 429.**
//     This is the half missed when copying the SDK (see that constant's comment), and it also keeps
//     what the old test was really worried
//     about: occasional rate limits do not leave the environment stuck at the 10-second cap forever;
//     once rate limiting stops for 30 seconds, the next success
//     opens the gate fully.
//
// ⚠️ **The lower bound of "step down one level" must be the start, not 0**; this was nearly written
// wrong, and writing it wrong would mean no change at all:
// half of the starting level is 250ms; if it were allowed to fall to 0, then "429 → 500ms → one
// success → 0"
// would be **exactly the same** as before. And 73% of the production waits stayed exactly at the
// starting level, meaning the back-and-forth at the incident
// happened right on this notch. Pinning the lower bound at the start during the healthy period is
// what cuts that back-and-forth.
//
// ⚠️ When stepping down **`until` is not touched**: that is an already scheduled wait window, and
// wiping it early would make
// the other runners in Wait wake at once and hit together, exactly the spike being treated. Let it
// expire naturally.
func (g *Gate) Observe(err error) {
	if err == nil {
		g.observeSuccess()
		return
	}
	if !selfhosted.IsStatus(err, http.StatusTooManyRequests) {
		return
	}
	g.observeRateLimited()
}

// observeSuccess handles one success: if quiet long enough, reset fully, otherwise step down one
// level (not below the start).
func (g *Gate) observeSuccess() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.backoff <= 0 {
		return
	}
	// A full healthy period of quiet since the last 429: the rate limiting is really over, open it
	// fully.
	if !g.lastHit.IsZero() && time.Since(g.lastHit) >= RateLimitHealthyAfter {
		g.backoff = 0
		g.until = time.Time{}
		return
	}
	// Still in the healthy period: step down one level, with the start as the lower bound. See the ⚠️
	// above: allowing 0 would mean no change.
	next := g.backoff / 2
	if next < RateLimitStart {
		next = RateLimitStart
	}
	g.backoff = next
}

// observeRateLimited handles one 429: raises the ladder and closes the gate until a jittered moment.
func (g *Gate) observeRateLimited() {
	g.mu.Lock()
	next := g.backoff * 2
	if next < RateLimitStart {
		next = RateLimitStart
	}
	if next > RateLimitCap {
		next = RateLimitCap
	}
	g.backoff = next
	g.hits++
	hits := g.hits
	g.lastHit = time.Now()
	// jitter keeps the N runners in the same environment from waking in the same millisecond;
	// lockstep would stack the first wave after backoff
	// into another spike, as if there were no backoff. It takes [d/2, d), the same shape as the
	// SDK's jitterDuration.
	wait := RateLimitJitter(next)
	until := time.Now().Add(wait)
	// Only push it later when the backoff window needs extending, to avoid moving up a later window
	// someone else just set.
	if until.After(g.until) {
		g.until = until
	}
	g.mu.Unlock()

	g.log.Warn("方舟 API 限流，该环境的并发 work 一起退避",
		slog.Duration("等待", wait), slog.Int("累计限流次数", hits))
}

// RateLimitJitter takes a random value in [d/2, d), the same shape as the SDK's jitterDuration.
func RateLimitJitter(d time.Duration) time.Duration {
	if d <= 1 {
		return d
	}
	half := d / 2
	return half + time.Duration(rand.Int63n(int64(d-half)))
}

// rateLimitedAPI wraps ListEvents / SendEvent with the rate-limit gate.
//
// # Why only these two
//
// These two are **called at high frequency in the session event loop**: in fallback mode one
// ListEvents per round
// (fixed ticker), and one SendEvent per pending result in flushResults. All 6087 in the storm
// came from ListEvents.
//
// The other four are deliberately not wrapped:
//   - PollWork is a long poll and already has arkBackoff (that set in core's pollLoop);
//     another layer would fight it; it goes through core's pollWorkThroughGate separately;
//   - AckWork / StopWork / GetSession are once per work, not a source of frequency,
//     and adding delay to them would slow ack (holding the lease without working) and settlement.
//
// **The order is Wait first, then call, then Observe with the result**: the other way (call first,
// then wait) would let the storm
// fire one round before backoff starts.
type rateLimitedAPI struct {
	selfhosted.API
	gate *Gate
}

// WrapAPI adds a layer of rate-limit backoff. When api or gate is nil it returns api as is.
func WrapAPI(api selfhosted.API, gate *Gate) selfhosted.API {
	if api == nil || gate == nil {
		return api
	}
	return &rateLimitedAPI{API: api, gate: gate}
}

func (a *rateLimitedAPI) ListEvents(ctx context.Context, req selfhosted.ListEventsRequest) (*selfhosted.ListEventsResponse, error) {
	if !a.gate.Wait(ctx) {
		return nil, ctx.Err()
	}
	resp, err := a.API.ListEvents(ctx, req)
	a.gate.Observe(err)
	return resp, err
}

func (a *rateLimitedAPI) SendEvent(ctx context.Context, req selfhosted.SendEventRequest) error {
	// ⚠️ The ctx used by SendEvent cannot be the basis for backoff. The SDK's retrySendEvent
	// deliberately uses
	// `context.Background()` for this request (the tool already ran, and the result needs one bounded
	// final delivery chance), so here Wait uses the ctx passed in, but **does not wait forever just
	// because it is not cancelled**:
	// the gate waits at most RateLimitCap, never holding the result forever.
	if !a.gate.Wait(ctx) {
		return ctx.Err()
	}
	err := a.API.SendEvent(ctx, req)
	a.gate.Observe(err)
	return err
}

// **Environment-level long backoff** when the Ark credential is invalid (401/403).
//
// # What it fixes
//
// The incident: a user configured an Ark API Key in workspace settings that **had been
// deleted**, so every PollWork for that environment returned 401 "API key doesn't exist".
// The old handling was:
//
//	pollLoop  → arkFatalStatus(401) is true → return error
//	runOne    → log "worker exited abnormally, reconnecting in 5 seconds" → sleep reconnectGap() (fixed 5 seconds) → retry
//
// As long as the key stays the same, this loops forever. In 25 minutes of logs we measured
// 4448 lines of "API key doesn't exist" and 2224 lines of "worker exited abnormally", about one
// round every 3.5 seconds, across 4 environments in two workspaces. This is not a platform
// credential problem; **the user configured the wrong key**, and retrying ten thousand times
// gives exactly the same result: it wastes the Ark account's RPM quota (shared by all workers,
// see workerratelimit.go), floods the logs, and buries the errors that should be seen under
// those 4448 lines.
//
// # ⚠️ Why the gate is in ensureWorker, not in the reconnect loop of runLeasedPoll
//
// This is the only decision in this change where "putting it where intuition says" fails
// silently, so it is worth spelling out.
//
// The intuitive fix is "turn the 5 second sleep in runOne into a long backoff". **What that
// produces never recovers**: the Ark client is **built once** in `runOne` from `key.APIKey`
// (arkruntime.NewClientWithApiKey, **outside** the reconnect loop), so no matter how many times
// the loop retries, it still calls with that **already invalid key**. If the user puts a new
// one in workspace settings, this worker never sees it, unless the process restarts. And that is
// exactly what the requirement says we must not rely on.
//
// There is only one path that resolves the credential again:
//
//	refresh (one round every 30 seconds, refreshInterval)
//	  → ensureWorker → svc.arkKey(...)   ← only here is the DB read again and the three-level credential resolved again
//	  → runOne → builds the client with the **new** key
//
// So the backoff must be "**do not start** the worker", not "start it and then retry slowly":
// on an auth failure runOne simply exits (its entry in `p.running` is deleted with it), and this
// gate decides whether the next refresh round lets it back in.
//
// # Changing the key "recovers right away", without waiting for the backoff to expire
//
// The gate records `(environment id, key fingerprint)`. As soon as the fingerprint resolved by
// the next refresh round differs from the recorded one, it means **the user has changed the
// key**, and the gate lets it through and resets. The longest wait is one refresh period
// (≤30 seconds), not one backoff period. The fingerprint is ArkKeyFingerprint
// (`len=<n> sha=<hex8>`, not reversible), so putting it in logs or memory does not leak the key.
//
// # ⚠️ Why only 401/403, not 400/404
//
// The `arkFatalStatus` group also includes 400 and 404, but they **should not go through this
// gate**:
//
//   - 404 (environment does not exist) already has its own self-healing path: create a new
//     environment and repoint the sibling bindings in the same workspace (see
//     healMissingArkResource). Holding it behind the gate for 5 minutes would slow down a path
//     that **could have fixed itself**.
//   - 400 means the request itself is wrong. That is our bug, not something the user can change,
//     so "wait for the user to change the key" does not apply.
//
// The check uses `selfhosted.APIError.StatusCode` (errors.As), **no string matching**:
// "API key doesn't exist" is Ark's current wording and can change at any time; the status code
// will not.
//
// # ⚠️ Trade-off: the counter lives in the process, not in PG
//
// This breaks the default rule "a distributed service must not keep state in the process", and
// it is **deliberate**. The test is the one CLAUDE.md itself gives: "only things that can be lost
// at any time, and whose loss only makes things slower, may live in the process". This counter
// is exactly that kind of thing:
//
//   - What happens when it is lost (pod rebuilt, rolling release): this environment **tries
//     PollWork one more time**, gets 401 again, and starts over from the first 30 second level.
//     The cost is one extra request, not wrong behavior.
//   - Storing it in the DB has a real cost: one more query per refresh round (30 seconds ×
//     number of environments × number of pods), plus a table and cleanup logic, just to save
//     "at most one retry per pod per environment".
//
// With multiple instances the worst case is N pods each backing off on their own, so the total
// request count is N times. **That is still fine**: before it was N pods × once every 3.5
// seconds, now it is N pods × at most once every 5 minutes, two orders of magnitude apart, and
// leader election (workerlease.go) already means most pods never get the poll right at all.
//
// # But "when the consecutive failures started" is stored in the DB (ark_auth_failures)
//
// The trade-off above holds only for the **ladder**. "After 3 days of consecutive failures, drop
// to one try per day" (authPauseAfter) needs a start time that survives releases and pod
// rebuilds. Production ships a release every two or three days, so a process never accumulates
// 3 days, and the rule would never trigger, with zero errors. So the start time goes into PG,
// while the ladder and the counter stay in the process. DB writes happen only on failure (at most
// once every few minutes after backoff) and on the first success of each pollLoop, not on the
// hot path.
package core

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// authBackoffLevels is the backoff ladder for auth failures, starting from the first failure.
//
// It and the idle slow-down ladder (arkbackoff.Levels, 1s→60s) **are two different things, do
// not merge them**: that one handles "do not spin when there is no work", and starts at 1 second
// because it must react right away when work arrives; this one handles "the credential is wrong,
// retrying is pointless", so it starts at 30 seconds.
//
// ⚠️ **The first level must not be less than refreshInterval (30 seconds).** The gate is checked
// in ensureWorker, and ensureWorker is only called by refresh once every 30 seconds. Levels
// shorter than 30 seconds can never be observed, and only make people think "set it to 10
// seconds and it retries every 10 seconds".
//
// The 5 minute cap is based on **how long the user waits after fixing the config**: after
// someone fixes a wrong key, the worst case is waiting one backoff period to see recovery. In
// practice a changed key is let through right away by its fingerprint (see the file header), so
// the 5 minutes only applies when "nobody ever fixes it", and then being slower is what we want.
var authBackoffLevels = []time.Duration{
	30 * time.Second,
	1 * time.Minute,
	2 * time.Minute,
	5 * time.Minute,
}

// authPauseAfter is "how long of consecutive auth failures before we treat this key as abandoned".
//
// The backoff ladder caps at 5 minutes, designed for "the user will fix it soon". But in
// production a few bad keys stayed broken for over a month, hit once every 5 minutes, adding up
// to about 62,000 lines of the two kinds of Warn per week, and still eating the account-level RPM
// quota. If nobody has touched the key for 3 days in a row, we stop treating it as "about to be
// fixed" and drop to once per authPausedProbe.
//
// The start time must be stored in the DB (ark_auth_failures): production ships a release every
// two or three days, so a counter in the process never reaches 3 days.
const authPauseAfter = 72 * time.Hour

// authPausedProbe is how often to try once paused.
//
// ⚠️ **It is not "never try".** A 403 (this key has no permission for this resource) can fix
// itself after the permission is granted on the Ark side. The key itself does not change and
// neither does the fingerprint, so the gate's "a new key is let through right away" does not
// cover it. Without any tries such an environment would never recover, with zero errors. Once a
// day means recovery is at worst one day late.
// A changed key still recovers right away (blocked lets it through by fingerprint), not affected
// by this interval.
const authPausedProbe = 24 * time.Hour

// authBackoffEntry is the auth failure state of one environment.
type authBackoffEntry struct {
	// fingerprint is the fingerprint of the key used when it failed. If it changes, the user
	// has changed the key, and the gate lets it through right away (see blocked).
	fingerprint string
	// failures is the number of consecutive auth failures, which picks the level. Any successful
	// PollWork resets it to zero.
	failures int
	// until means "do not start this environment's worker again before this time".
	until time.Time
}

// authBackoff is the auth backoff gate shared by the whole process, keyed by environment id.
//
// **It must be safe for concurrent use**: writers are each environment's own worker goroutine
// (runOne), the reader is the refresh goroutine (ensureWorker), and the two naturally run at the
// same time. This differs from arkbackoff.Ladder, where each pollLoop owns its own copy, so no
// lock is needed there.
type authBackoff struct {
	mu sync.Mutex
	m  map[string]*authBackoffEntry
}

func newAuthBackoff() *authBackoff {
	return &authBackoff{m: map[string]*authBackoffEntry{}}
}

// fail records one auth failure, and returns how long to wait this time and which failure
// number this is.
//
// The returned wait already has ±20% jitter applied (jitterAround): several environments often
// share the same bad key (several environments in one workspace share a credential). Without
// jitter they would move in lockstep and all hit at the same moment, which is exactly what the
// file header of workerbackoff.go keeps stressing.
//
// Safe with a nil receiver: WorkerPools built by older unit tests do not have this field.
func (b *authBackoff) fail(envID, fingerprint string, now time.Time) (time.Duration, int) {
	if b == nil {
		return 0, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	e := b.m[envID]
	// A changed key starts counting from zero: it is a new key, whether it works is a separate
	// question, and it should not inherit the previous key's failure count.
	if e == nil || e.fingerprint != fingerprint {
		e = &authBackoffEntry{fingerprint: fingerprint}
		b.m[envID] = e
	}
	e.failures++

	idx := e.failures - 1
	if idx >= len(authBackoffLevels) {
		idx = len(authBackoffLevels) - 1
	}
	wait := jitterAround(authBackoffLevels[idx])
	e.until = now.Add(wait)
	return wait, e.failures
}

// holdUntil extends an environment's backoff to until (only extends, never shortens).
//
// Used by the "consecutive failures reached authPauseAfter" path: the ladder only goes up to 5
// minutes, and a pause needs a longer interval. The fingerprint must match the one recorded at
// fail time; a mismatch means the key changed in the meantime, and the new key must not be held
// back too.
func (b *authBackoff) holdUntil(envID, fingerprint string, until time.Time) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.m[envID]
	if e == nil || e.fingerprint != fingerprint {
		return
	}
	if until.After(e.until) {
		e.until = until
	}
}

// blocked decides whether this environment's worker should be skipped now, and how much longer
// to wait.
//
// When fingerprint differs from the recorded one, **always let it through and clear the
// record**: the user changed the key, and the new key must get a chance right away, otherwise
// "I clearly changed the config, why is it still broken".
func (b *authBackoff) blocked(envID, fingerprint string, now time.Time) (bool, time.Duration) {
	if b == nil {
		return false, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	e := b.m[envID]
	if e == nil {
		return false, 0
	}
	if e.fingerprint != fingerprint {
		delete(b.m, envID)
		return false, 0
	}
	if now.Before(e.until) {
		return true, e.until.Sub(now)
	}
	return false, 0
}

// clear removes an environment's auth failure record. **It must be called on every successful
// PollWork.**
//
// ⚠️ Missing it is a silent bug, the same shape as the one around arkbackoff.Ladder.Reset: an
// environment that occasionally hits a stray 401 would pile its count up to the 5 minute level and
// then **never come back down**: every next failure after recovering would start from the top
// level. `TestAuthBackoffResetsAfterSuccess` locks this in.
func (b *authBackoff) clear(envID string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.m, envID)
}

// arkAuthStatus decides "this error means the credential is invalid": 401 or 403.
//
// ⚠️ **401 and 403 are two different things, but handled the same**: 401 = the credential itself
// is invalid (key deleted, mistyped, expired), 403 = the credential is valid but has no permission
// for this resource (the key belongs to another project).
// Both need **a person to fix the config** before they recover, and retrying ten thousand times
// gives the same result, so they go through the same gate. Handling them together here does not
// mean they are the same thing; it means "the next step happens to be the same".
//
// The check uses the status code, not the message: "API key doesn't exist" is Ark's current
// wording and can change at any time.
func arkAuthStatus(err error) bool {
	switch arkStatusOf(err) {
	case 401, 403:
		return true
	default:
		return false
	}
}

// logAuthBackoff logs the "credential invalid, retry after backoff" Warn.
//
// **At most one line per backoff period**: this function is only called at the moment of fail,
// and after fail the worker exits and can only be started again once the backoff expires, so the
// rate naturally equals the backoff period. The old shape was one line every 5 seconds ×
// (number of environments), 2224 lines in 25 minutes.
//
// None of the four fields can be dropped; each answers one question during debugging: which
// environment (environment), whose key (key_fingerprint, not reversible, safe to log), which
// attempt (total failures), and how much longer to wait (next retry).
func logAuthBackoff(
	logger *slog.Logger, envID, fingerprint string,
	failures int, wait time.Duration, err error,
) {
	if logger == nil {
		return
	}
	logger.Warn("方舟凭据无效（401/403），本环境 worker 暂停，退避后自动重试；"+
		"到空间设置换一把有效的 API Key 可立即恢复",
		slog.String("environment", envID),
		slog.String("key_fingerprint", fingerprint),
		slog.Int("累计失败", failures),
		slog.Duration("下次重试", wait.Round(time.Second)),
		slog.Any("err", err))
}

// onAuthFailure is the full handling when an environment worker exits because the credential is
// invalid. It returns the earliest time until the next try.
//
// First it backs off by the in-process ladder; then it records the failure in the DB, and once
// consecutive failures reach authPauseAfter (3 days) the wait becomes about authPausedProbe (one
// day). If the DB write fails it keeps using the ladder (see recordAuthFailure).
func (p *WorkerPool) onAuthFailure(
	ctx context.Context, logger *slog.Logger, envID, workspaceID, fingerprint string, err error,
) time.Duration {
	wait, n := p.auth.fail(envID, fingerprint, time.Now())
	if since, ok := p.recordAuthFailure(ctx, envID, workspaceID, fingerprint); ok && since >= authPauseAfter {
		wait = jitterAround(authPausedProbe)
		p.auth.holdUntil(envID, fingerprint, time.Now().Add(wait))
		logAuthPaused(logger, envID, fingerprint, since, wait, err)
		return wait
	}
	logAuthBackoff(logger, envID, fingerprint, n, wait, err)
	return wait
}

// authStoreTimeout is the time budget for reading and writing ark_auth_failures. A slow DB
// should not hold up the worker's reconnect.
const authStoreTimeout = 3 * time.Second

// recordAuthFailure records this auth failure in the DB, and returns how long this run of
// consecutive failures has lasted.
//
// A false second return value means it was not recorded (no store wired up, or the DB write
// failed). The caller then backs off by the in-process ladder as before and **does not pause**:
// when we cannot tell whether 3 days have passed, we treat it as "not yet". The worst case is a
// few extra tries, while the opposite (pause when unsure) would stop a key that was just
// misconfigured and is about to be fixed for a whole day.
func (p *WorkerPool) recordAuthFailure(
	ctx context.Context, envID, workspaceID, fingerprint string,
) (time.Duration, bool) {
	if p == nil || p.svc == nil || p.svc.store == nil {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), authStoreTimeout)
	defer cancel()
	since, err := p.svc.store.RecordArkAuthFailure(ctx, envID, workspaceID, fingerprint)
	if err != nil {
		p.log.Warn("记方舟凭据失效起点失败，本次按普通退避处理（不会暂停）",
			slog.String("environment", envID), slog.Any("err", err))
		return 0, false
	}
	return since, true
}

// clearAuthFailureRecord deletes this environment's failure record from the DB (the key works
// now).
//
// Called only once, on the first successful poll of each pollLoop; see the comment at the call
// site in pollLoop. A failure is only logged at Debug: the cost of a stale row left behind is that
// "how long it has lasted" comes out too long at the next failure. That is only read at the next
// run of consecutive failures, and changing the key or one more success wipes it anyway.
func (p *WorkerPool) clearAuthFailureRecord(ctx context.Context, envID string) {
	if p == nil || p.svc == nil || p.svc.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), authStoreTimeout)
	defer cancel()
	if err := p.svc.store.ClearArkAuthFailure(ctx, envID); err != nil {
		p.log.Debug("清方舟凭据失效记录失败", slog.String("environment", envID), slog.Any("err", err))
	}
}

// logAuthPaused logs the "invalid for 3 days in a row, dropping to one try per day" Warn.
//
// Kept separate from logAuthBackoff: the two messages suggest different next steps. This one must
// make it clear that "this environment has in effect stopped"; otherwise people only see sparse
// backoff logs and think it is still retrying normally.
func logAuthPaused(
	logger *slog.Logger, envID, fingerprint string,
	since, wait time.Duration, err error,
) {
	if logger == nil {
		return
	}
	logger.Warn("方舟凭据已连续失效超过 3 天，本环境 worker 暂停，改为约每 24 小时试一次；"+
		"到空间设置换一把有效的 API Key 可立即恢复",
		slog.String("environment", envID),
		slog.String("key_fingerprint", fingerprint),
		slog.Duration("已连续失效", since.Round(time.Hour)),
		slog.Duration("下次重试", wait.Round(time.Minute)),
		slog.Any("err", err))
}

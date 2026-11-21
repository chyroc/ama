package core

import (
	"sync"
	"time"

	"github.com/chyroc/ama/server/service/arkbackoff"
)

const (
	// DefaultAuthPauseAfter is how long a key may fail before probing it once a day.
	DefaultAuthPauseAfter = 72 * time.Hour
	// DefaultAuthPausedProbe is the interval between probes after the pause threshold.
	DefaultAuthPausedProbe = 24 * time.Hour
)

// DefaultAuthBackoffLevels is the default ladder for invalid Ark credentials.
var DefaultAuthBackoffLevels = []time.Duration{
	30 * time.Second,
	time.Minute,
	2 * time.Minute,
	5 * time.Minute,
}

// AuthBackoff limits retries for invalid credentials independently per environment.
// Its state may be lost on restart: the cost is an extra request, not wrong behavior.
// A caller that pauses abandoned keys must keep their failure start time outside the process.
type AuthBackoff struct {
	mu     sync.Mutex
	m      map[string]*authBackoffEntry
	levels []time.Duration
	jitter func(time.Duration) time.Duration
}

// NewAuthBackoff returns a process-local gate using the supplied retry levels.
// A nil level slice uses DefaultAuthBackoffLevels. A nil jitter adds no jitter.
func NewAuthBackoff(levels []time.Duration, jitter func(time.Duration) time.Duration) *AuthBackoff {
	if levels == nil {
		levels = DefaultAuthBackoffLevels
	}
	if len(levels) == 0 {
		levels = []time.Duration{30 * time.Second}
	}
	if jitter == nil {
		jitter = func(d time.Duration) time.Duration { return d }
	}
	return &AuthBackoff{
		m:      make(map[string]*authBackoffEntry),
		levels: append([]time.Duration(nil), levels...),
		jitter: jitter,
	}
}

// Fail records one failure. Changing the key fingerprint starts a new count.
func (b *AuthBackoff) Fail(environmentID, fingerprint string, now time.Time) (time.Duration, int) {
	if b == nil {
		return 0, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.m[environmentID]
	if e == nil || e.fingerprint != fingerprint {
		e = &authBackoffEntry{fingerprint: fingerprint}
		b.m[environmentID] = e
	}
	e.failures++
	idx := e.failures - 1
	if idx >= len(b.levels) {
		idx = len(b.levels) - 1
	}
	wait := b.jitter(b.levels[idx])
	e.until = now.Add(wait)
	return wait, e.failures
}

// HoldUntil extends a gate, never shortening it or applying it to a different key.
func (b *AuthBackoff) HoldUntil(environmentID, fingerprint string, until time.Time) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.m[environmentID]
	if e == nil || e.fingerprint != fingerprint {
		return
	}
	if until.After(e.until) {
		e.until = until
	}
}

// Blocked reports whether a retry must wait. A new fingerprint clears the old gate immediately.
func (b *AuthBackoff) Blocked(environmentID, fingerprint string, now time.Time) (bool, time.Duration) {
	if b == nil {
		return false, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.m[environmentID]
	if e == nil {
		return false, 0
	}
	if e.fingerprint != fingerprint {
		delete(b.m, environmentID)
		return false, 0
	}
	if now.Before(e.until) {
		return true, e.until.Sub(now)
	}
	return false, 0
}

// Clear resets the retry count after a successful request.
func (b *AuthBackoff) Clear(environmentID string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.m, environmentID)
}

// DefaultAuthJitter is the jitter used by the standard worker configuration.
func DefaultAuthJitter(d time.Duration) time.Duration { return arkbackoff.Jitter(d) }

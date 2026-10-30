// Forwarding layer for Ark API rate-limit backoff: the implementation moved to service/arkbackoff,
// and only aliases stay here.
//
// The reason is word for word the same as in workerbackoff.go (option A: core keeps the forwarding
// it needs). The gate is used by core's pollLoop, pollWorkThroughGate and two worker test files.
// Keeping aliases is cheaper than adding a package name to some thirty call sites, and it does not
// change any behavior.
//
// What stays here is **type aliases / function forwarding**, so `arkRateLimitGate` and
// `arkbackoff.Gate` **are the same type**: the gate built in tests is the one used in production.
//
// ⚠️ **Do not build a second implementation here.** The three arguments "why share per
// environment", "why only react to 429" and "why not change the SDK" are all in the file header of
// service/arkbackoff/ratelimit.go.
//
// ⚠️ One more thing that is easy to mix up: the gate keeps "the N runners in this process from
// hammering at once"; **the cross-pod dimension is handled by leader election (workerlease.go)**.
// The two cover different dimensions.
package core

import (
	"log/slog"
	"time"

	"github.com/volcengine/ark-runtime-go/arkruntime/selfhosted"

	"github.com/chyroc/ama/server/service/arkbackoff"
)

// Start and cap of rate-limit backoff, forwarded as is.
const (
	arkRateLimitBackoffStart = arkbackoff.RateLimitStart
	arkRateLimitBackoffCap   = arkbackoff.RateLimitCap
)

// arkRateLimitGate is an alias of arkbackoff.Gate: **the same type**, not a wrapper.
type arkRateLimitGate = arkbackoff.Gate

// newArkRateLimitGate builds a rate-limit gate shared per environment.
func newArkRateLimitGate(logger *slog.Logger) *arkRateLimitGate { return arkbackoff.NewGate(logger) }

// newRateLimitedAPI wraps ListEvents / SendEvent with the rate-limit gate. If api or gate is nil,
// it returns api unchanged.
func newRateLimitedAPI(api selfhosted.API, gate *arkRateLimitGate) selfhosted.API {
	return arkbackoff.WrapAPI(api, gate)
}

// arkRateLimitJitter returns a random value in [d/2, d), same shape as the SDK's jitterDuration.
func arkRateLimitJitter(d time.Duration) time.Duration { return arkbackoff.RateLimitJitter(d) }

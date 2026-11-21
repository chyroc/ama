package core

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/volcengine/ark-runtime-go/arkruntime/selfhosted"
)

const DefaultArkBackoffCap = 60 * time.Second

// ArkFatalStatus reports permanent poll errors. Timeout, conflict, and rate limit
// remain retryable; invalid credentials and missing environment configuration do not
// benefit from retrying with the same client.
func ArkFatalStatus(err error) bool {
	var apiErr *selfhosted.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.StatusCode >= 400 && apiErr.StatusCode < 500 &&
		apiErr.StatusCode != http.StatusRequestTimeout &&
		apiErr.StatusCode != http.StatusConflict &&
		apiErr.StatusCode != http.StatusTooManyRequests
}

// ArkResolvedStatus reports that another worker already owns the work item.
func ArkResolvedStatus(err error) bool {
	return selfhosted.IsStatus(err, http.StatusNotFound) ||
		selfhosted.IsStatus(err, http.StatusConflict) ||
		selfhosted.IsStatus(err, http.StatusPreconditionFailed)
}

// ArkBackoff returns the exponential delay for one poll loop, capped at 64 seconds.
func ArkBackoff(n int) time.Duration {
	if n <= 0 {
		return time.Second
	}
	if n > 6 {
		return DefaultArkBackoffCap
	}
	if d := time.Duration(1<<n) * time.Second; d <= DefaultArkBackoffCap {
		return d
	}
	return DefaultArkBackoffCap
}

// PollWorkThroughGate applies the environment-wide rate-limit gate around PollWork.
// The gate controls account-level 429s; the caller's separate poll failure ladder
// controls this loop's repeated failures. They add delay rather than canceling each other.
func PollWorkThroughGate(
	ctx context.Context, api selfhosted.API, gate *arkRateLimitGate, req selfhosted.PollWorkRequest,
) (*selfhosted.WorkItem, error) {
	if gate != nil {
		if !gate.Wait(ctx) {
			return nil, ctx.Err()
		}
	}
	item, err := api.PollWork(ctx, req)
	if gate != nil {
		gate.Observe(err)
	}
	return item, err
}

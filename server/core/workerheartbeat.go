package core

import (
	"context"
	"errors"
	"time"

	"github.com/volcengine/ark-runtime-go/arkruntime/selfhosted"
)

const (
	// DefaultWorkerHeartbeatTTL is the longest interval without a successful heartbeat.
	DefaultWorkerHeartbeatTTL = 30 * time.Second
	// DefaultWorkerHeartbeatInterval is the initial interval, adjusted to half the server TTL.
	DefaultWorkerHeartbeatInterval = 15 * time.Second
)

const (
	HeartbeatCauseLeaseLost        = "lease_lost"
	HeartbeatCauseHeartbeatLost    = "heartbeat_lost"
	HeartbeatCausePermanentFailure = "permanent_failure"
	HeartbeatCauseStopRequested    = "stop_requested"
)

// RunWorkerHeartbeat sends one work item's heartbeats until cancellation, a lost
// lease, a permanent rejection, or a server stop request. The server-provided TTL
// replaces the default; failures are tolerated only until that TTL expires.
func RunWorkerHeartbeat(
	ctx context.Context, item selfhosted.WorkItem, api selfhosted.API,
	cancel context.CancelFunc, markCause func(string),
) {
	last := selfhosted.ExpectedLastHeartbeatNoHeartbeat
	ttl := DefaultWorkerHeartbeatTTL
	interval := DefaultWorkerHeartbeatInterval
	lastSuccess := time.Now()

	beat := func() bool {
		beatCtx, beatCancel := context.WithTimeout(ctx, interval)
		defer beatCancel()
		resp, err := api.HeartbeatWork(beatCtx, selfhosted.HeartbeatWorkRequest{
			EnvironmentID:         item.EnvironmentID,
			WorkID:                item.ID,
			ExpectedLastHeartbeat: last,
			DesiredTTLSeconds:     int(ttl / time.Second),
		})
		if err != nil {
			if selfhosted.IsStatus(err, 412) {
				markCause(HeartbeatCauseLeaseLost)
				cancel()
				return false
			}
			if ctx.Err() != nil {
				return false
			}
			if IsFatalWorker4xx(err) {
				markCause(HeartbeatCausePermanentFailure)
				cancel()
				return false
			}
			if time.Since(lastSuccess) > ttl {
				markCause(HeartbeatCauseHeartbeatLost)
				cancel()
				return false
			}
			return true
		}
		if resp == nil {
			return true
		}
		lastSuccess = time.Now()
		if resp.LastHeartbeat != "" {
			last = resp.LastHeartbeat
		}
		if resp.TTLSeconds > 0 {
			ttl = time.Duration(resp.TTLSeconds) * time.Second
			if half := ttl / 2; half >= time.Second {
				interval = half
			}
		}
		if resp.State == selfhosted.WorkStateStopping || resp.State == selfhosted.WorkStateStopped {
			markCause(HeartbeatCauseStopRequested)
			cancel()
			return false
		}
		return true
	}

	if !beat() {
		return
	}
	for {
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if !beat() {
			return
		}
	}
}

// IsFatalWorker4xx classifies permanent work request failures. Timeout, races,
// rate limits, and lost ownership remain retryable or have their own handling.
func IsFatalWorker4xx(err error) bool {
	var apiErr *selfhosted.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.StatusCode {
	case 408, 409, 429, 412:
		return false
	default:
		return apiErr.StatusCode >= 400 && apiErr.StatusCode < 500
	}
}

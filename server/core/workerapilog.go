// Redacted logging for every Ark call the worker makes.
//
// # Why this layer exists
//
// After the worker takes a work item, the steps that do the real work (GetSession to create the
// session, OpenSkill to download skills, ListEvents to poll events, SendEvent to return results,
// HeartbeatWork for heartbeats) are mostly called inside the SDK, and they log through the SDK's
// own stdlib log, which **production log collection does not pick up**. So when "tools hang" in
// production, none of these steps leave a single line, and debugging can only work backwards.
//
// This layer wraps the selfhosted.API used by the worker and logs, with our own slog, the **method
// name, duration, success/failure, and HTTP status code on failure** of every Ark call. So "which
// step it is stuck on, whether it is slow or failing, and with what status" is visible directly in
// the logs.
//
// # Redaction: only metadata, never content
//
// Request / response bodies hold user data (message text, bash commands, file paths, skill content,
// event JSON). We log **only** the method name, duration, status code, and ids the caller passes in
// via ctx/struct fields (session/work/environment; these ids are already printed in full in
// existing logs such as "took work"). No request or response field is printed. This follows
// backend.md: "log ids and enums, not content".
package core

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/volcengine/ark-runtime-go/arkruntime/selfhosted"
)

// workerAPISlowThreshold is how long a "should be fast" call may take before it is raised to Warn.
//
// Used only for event read/write and preparation steps (ListEvents/SendEvent/GetSession/
// OpenSkill). These hanging is the direct cause of "tools hang", so raising them to Warn shows at a
// glance "stuck in preparation / cannot read events". PollWork is a long poll (it hangs ~1s on
// purpose), so this threshold **does not apply**.
const workerAPISlowThreshold = 30 * time.Second

// instrumentedWorkerAPI wraps a selfhosted.API and adds redacted logs to every call.
type instrumentedWorkerAPI struct {
	selfhosted.API
	log *slog.Logger
}

// newInstrumentedWorkerAPI adds the wrapper. If inner is nil, it returns inner unchanged.
func newInstrumentedWorkerAPI(inner selfhosted.API, log *slog.Logger) selfhosted.API {
	if inner == nil {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	return &instrumentedWorkerAPI{API: inner, log: log}
}

// record logs one call: Debug on success, Warn on failure; with slowWarn=true slow calls are also
// raised to Warn. It takes only the method name, duration and error, never any req/resp.
func (a *instrumentedWorkerAPI) record(method string, start time.Time, err error, slowWarn bool) {
	took := time.Since(start)
	if err != nil {
		// ctx cancel/timeout is the normal result of shutdown and reconnect (the PollWork long poll
		// hangs until ctx ends, and heartbeats/event loops are cancelled when work finishes). It is
		// not a fault: logging it as Warn would spam noise per environment on every release.
		// Downgrade to Debug.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			a.log.Debug("worker 方舟 API 调用随 ctx 结束",
				slog.String("api", method),
				slog.Duration("耗时", took.Round(time.Millisecond)))
			return
		}
		a.log.Warn("worker 方舟 API 调用失败",
			slog.String("api", method),
			slog.Duration("耗时", took.Round(time.Millisecond)),
			slog.Int("status", arkStatusOf(err)),
			slog.Any("err", err))
		return
	}
	if slowWarn && took >= workerAPISlowThreshold {
		a.log.Warn("worker 方舟 API 调用过慢（这一步本该很快，卡住会导致工具不执行）",
			slog.String("api", method),
			slog.Duration("耗时", took.Round(time.Second)))
		return
	}
	a.log.Debug("worker 方舟 API 调用",
		slog.String("api", method),
		slog.Duration("耗时", took.Round(time.Millisecond)))
}

func (a *instrumentedWorkerAPI) PollWork(ctx context.Context, req selfhosted.PollWorkRequest) (*selfhosted.WorkItem, error) {
	start := time.Now()
	resp, err := a.API.PollWork(ctx, req)
	// PollWork is a long poll; hanging is normal, so no slow-call warning.
	a.record("PollWork", start, err, false)
	return resp, err
}

func (a *instrumentedWorkerAPI) AckWork(ctx context.Context, req selfhosted.AckWorkRequest) error {
	start := time.Now()
	err := a.API.AckWork(ctx, req)
	a.record("AckWork", start, err, false)
	return err
}

func (a *instrumentedWorkerAPI) HeartbeatWork(ctx context.Context, req selfhosted.HeartbeatWorkRequest) (*selfhosted.HeartbeatResponse, error) {
	start := time.Now()
	resp, err := a.API.HeartbeatWork(ctx, req)
	a.record("HeartbeatWork", start, err, false)
	return resp, err
}

func (a *instrumentedWorkerAPI) StopWork(ctx context.Context, req selfhosted.StopWorkRequest) error {
	start := time.Now()
	err := a.API.StopWork(ctx, req)
	a.record("StopWork", start, err, false)
	return err
}

func (a *instrumentedWorkerAPI) GetSession(ctx context.Context, req selfhosted.GetSessionRequest) (*selfhosted.Session, error) {
	start := time.Now()
	resp, err := a.API.GetSession(ctx, req)
	// GetSession is a preparation step before the runner starts. If it hangs, the work keeps
	// heartbeating but tools never run.
	a.record("GetSession", start, err, true)
	return resp, err
}

func (a *instrumentedWorkerAPI) ListEvents(ctx context.Context, req selfhosted.ListEventsRequest) (*selfhosted.ListEventsResponse, error) {
	start := time.Now()
	resp, err := a.API.ListEvents(ctx, req)
	// The event loop uses it to find tool calls; constant failure/slowness means tools never run.
	a.record("ListEvents", start, err, true)
	return resp, err
}

func (a *instrumentedWorkerAPI) SendEvent(ctx context.Context, req selfhosted.SendEventRequest) error {
	start := time.Now()
	err := a.API.SendEvent(ctx, req)
	// Tool results go back through it; if it cannot send, Ark waits on that turn forever.
	a.record("SendEvent", start, err, true)
	return err
}

func (a *instrumentedWorkerAPI) OpenSkill(ctx context.Context, req selfhosted.OpenSkillRequest) (*selfhosted.SkillContent, error) {
	start := time.Now()
	resp, err := a.API.OpenSkill(ctx, req)
	// Downloading skills is a preparation step before the runner starts; if it hangs, the whole
	// work hangs too.
	a.record("OpenSkill", start, err, true)
	return resp, err
}

// arkStatusOf gets the HTTP status code from an Ark error; returns 0 if there is none. Only used
// for logs; it does not change what the error means.
func arkStatusOf(err error) int {
	var apiErr *selfhosted.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode
	}
	// context cancel/timeout has no status code; use a readable placeholder so a 0 in the logs does
	// not look like a failed lookup.
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusRequestTimeout
	}
	return 0
}

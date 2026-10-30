// HTTP long polling transport: the only path before the daemon rework, behavior kept exactly; it is
// both the permanent fallback for WS and the fallback for old amad versions (no /ws route).
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"

	"github.com/chyroc/ama/server/shared/hostproto"
)

type httpWire struct {
	rt *daemonRuntime
}

func newHTTPWire(rt *daemonRuntime) *httpWire { return &httpWire{rt: rt} }

func (w *httpWire) name() string { return transportNameHTTP }

func (w *httpWire) close() error { return nil } // no connection

// serve long-polls tick by tick: POST /heartbeat (the server holds for at most 20s) → hand the calls
// to the worker (blocking until the worker finishes, reproducing the old "fetch the next tick only
// after processing") → go again right away.
func (w *httpWire) serve(ctx context.Context, in chan<- inboundBatch) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var resp hostproto.HeartbeatResponse
		status, err := w.rt.postJSON(ctx, http.MethodPost, "/api/host-daemon/heartbeat",
			w.rt.cfg.Token, w.rt.heartbeatRequest(), &resp, true)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("心跳失败：%v，%s 后重试", err, httpHeartbeatRetry)
			if !sleep(ctx, httpHeartbeatRetry) {
				return ctx.Err()
			}
			continue
		}
		if status == http.StatusUnauthorized || resp.Revoked {
			return errDaemonUnauthorized
		}
		if status != http.StatusOK {
			log.Printf("心跳返回非预期状态 %d，%s 后重试", status, httpHeartbeatRetry)
			if !sleep(ctx, httpHeartbeatRetry) {
				return ctx.Err()
			}
			continue
		}
		if len(resp.Calls) > 0 {
			select {
			case in <- inboundBatch{calls: resp.Calls}:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// submitResults does POST /results. A 401 does not wipe credentials here (the heartbeat side handles
// the exit uniformly, as in the old version); it only returns an error so the upper layer puts the
// data back and retries.
func (w *httpWire) submitResults(ctx context.Context, req hostproto.ResultsRequest) error {
	req.HostID = w.rt.cfg.HostID
	status, err := w.rt.postJSON(ctx, http.MethodPost, "/api/host-daemon/results",
		w.rt.cfg.Token, req, nil, false)
	if err != nil {
		return err
	}
	if status == http.StatusNoContent || status == http.StatusOK {
		return nil
	}
	if status == http.StatusUnauthorized {
		log.Printf("结果上报被拒（401），等心跳确认")
	}
	return fmt.Errorf("results 状态码 %d", status)
}

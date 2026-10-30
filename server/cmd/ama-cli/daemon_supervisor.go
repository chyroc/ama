// Transport supervisor: decides whether the daemon currently uses WebSocket or HTTP long polling,
// and when to reconnect, fall back, and probe again.
//
//	┌────────────────┐  3 short-lived failures in a row /  ┌────────────────────┐
//	│  WS phase      │  server does not support it          │ HTTP phase         │
//	│  reconnect w/  │ ───────────────────────────────────▶ │ long polling +     │
//	│  backoff       │                                      │ periodic re-probe  │
//	└────────────────┘ ◀─────────────────────────────────── └────────────────────┘
//	                     re-probe succeeds (101 + first frame hello_ack)
//
// Classification rules (errors are in daemon_wire_ws.go):
//
//   - 401 / revoked / close 4001: no reconnect, no fallback; wipe host_token and exit (the same path
//     as the old HTTP 401 exit, asking to register again);
//   - 404/405/426 / close 4000: an old amad or a gateway without WS rules; fall back to HTTP and
//     only probe again after 30min, no pointless hammering;
//   - dial failure / dropped soon after connecting: redial with backoff 1→2→5s (±20% jitter); 3
//     "short-lived" failures in a row fall back to HTTP; a connection that lived past 60s resets the
//     budget (one gateway blip should not push the user to HTTP forever);
//   - close 4002 (kicked out by a newer connection from the same host): wait 10s before trying
//     again, to avoid half-open zombie connections kicking each other in a loop.
//
// An explicit AMA_DAEMON_TRANSPORT=http is the escape hatch: HTTP only, never try WS.
package main

import (
	"context"
	"errors"
	"log"
	"math/rand"
	"time"

	"github.com/chyroc/ama/server/shared/hostproto"
)

const (
	transportModeAuto = "auto"
	transportModeHTTP = "http"
	transportModeWS   = "ws" // same meaning as auto (fallback allowed too; availability first)
)

// The timings below default to production values; tests change them to be shorter temporarily (for
// example shrinking the re-probe interval from 5 minutes to milliseconds), so they are var, not const.
var (
	wsFailBudget      = 3
	wsHealthySession  = 60 * time.Second
	wsBackoffInitial  = 1 * time.Second
	wsBackoffMax      = 5 * time.Second
	wsReplacedBackoff = 10 * time.Second
	// Re-probe timing in the HTTP phase: 5 minutes for an ordinary fallback; 30 minutes when the
	// server says outright it does not support it (404 and so on): an old amad will not be upgraded
	// within minutes.
	httpReprobeInterval    = 5 * time.Minute
	unsupportedReprobeWait = 30 * time.Minute
)

// errWSExhausted means the WS phase's reconnect budget is used up, and it is time for the HTTP phase.
var errWSExhausted = errors.New("ws: 连续短命失败，降级 HTTP")

// runSupervisor runs the transport state machine until ctx is cancelled or the credential becomes
// invalid (returning errDaemonUnauthorized; the caller is responsible for wiping credentials and
// exiting).
func (rt *daemonRuntime) runSupervisor(ctx context.Context, in chan inboundBatch) error {
	if rt.transportPreference() == transportModeHTTP {
		log.Printf("传输模式：仅 HTTP 长轮询（AMA_DAEMON_TRANSPORT=http）")
		return newHTTPWire(rt).serve(ctx, in)
	}

	for ctx.Err() == nil {
		// — WS phase: dial, stay connected, reconnect within budget, and give the exit reason on leaving —
		reason := rt.runWSPhase(ctx, in)
		if errors.Is(reason, errDaemonUnauthorized) {
			return errDaemonUnauthorized
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		wait := httpReprobeInterval
		if errors.Is(reason, errWSUnsupported) {
			wait = unsupportedReprobeWait
		}

		// — HTTP phase: long polling service, until a re-probe succeeds and hands back an established WS —
		w, firstCalls, err := rt.runHTTPWithReprobe(ctx, in, wait)
		if err != nil {
			// Only two cases are left: errDaemonUnauthorized and ctx ending.
			return err
		}
		// Re-probe upgrade: run this already-handshaken connection to the end, and after it dies go back
		// to the top of the loop into a new WS phase (dial once again, with the budget reset: a
		// successful re-probe means the network has recovered).
		rt.setActive(w)
		uerr := w.runConnected(ctx, in, firstCalls)
		rt.clearActiveIf(w)
		if errors.Is(uerr, errWSUnauthorized) {
			return errDaemonUnauthorized
		}
	}
	return ctx.Err()
}

// runWSPhase runs the WS phase until: the credential is invalid, the server does not support it, or the budget of short-lived failures in a row is used up.
func (rt *daemonRuntime) runWSPhase(ctx context.Context, in chan inboundBatch) error {
	budget := 0
	backoff := wsBackoffInitial

	for ctx.Err() == nil {
		w := newWSWire(rt, rt.wsURL())
		firstCalls, err := w.dialWS(ctx)
		if err == nil {
			log.Printf("传输模式：WebSocket（%s）", rt.wsURL())
			rt.setActive(w)
			err = w.runConnected(ctx, in, firstCalls)
			rt.clearActiveIf(w)
		}

		replaced := false
		switch {
		case errors.Is(err, errWSUnauthorized):
			return errDaemonUnauthorized
		case errors.Is(err, errWSUnsupported):
			return errWSUnsupported
		case errors.Is(err, errWSReplaced):
			budget++
			replaced = true
		case ctx.Err() != nil:
			return ctx.Err()
		default:
			// errWSDial (dial/first-frame failure, established is zero) or errWSDropped.
			if !w.established.IsZero() && time.Since(w.established) >= wsHealthySession {
				// Dropped only after living past 60s: an occasional blip, reset the budget.
				budget = 0
				backoff = wsBackoffInitial
			} else {
				budget++
			}
		}

		if budget >= wsFailBudget {
			log.Printf("WebSocket 连续 %d 次短命失败，降级 HTTP 长轮询", budget)
			return errWSExhausted
		}
		wait := backoff
		if replaced {
			wait = wsReplacedBackoff
		}
		log.Printf("WebSocket 将在 %s 后重连（连续短命 %d/%d）", wait, budget, wsFailBudget)
		if !sleepRng(ctx, wait) {
			return ctx.Err()
		}
		if !replaced {
			backoff = nextBackoff(backoff)
		}
	}
	return ctx.Err()
}

// runHTTPWithReprobe enters the HTTP fallback: start long polling, and try dialing WS every interval.
// When a trial dial succeeds (101 + first frame hello_ack), stop the HTTP path and return the
// established WS connection; when the credential is invalid, return errDaemonUnauthorized.
func (rt *daemonRuntime) runHTTPWithReprobe(
	ctx context.Context, in chan inboundBatch, interval time.Duration,
) (*wsWire, []hostproto.Dispatch, error) {
	httpCtx, cancelHTTP := context.WithCancel(ctx)
	defer cancelHTTP()

	hw := newHTTPWire(rt)
	rt.setActive(hw)
	httpErr := make(chan error, 1)
	go func() { httpErr <- hw.serve(httpCtx, in) }()

	t := time.NewTicker(interval)
	defer t.Stop()
	log.Printf("传输模式：HTTP 长轮询（每 %s 重探 WebSocket）", interval)

	for {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case err := <-httpErr:
			// httpWire already swallows and retries all temporary errors internally; the only things
			// that get here are a 401 (wipe credentials) or httpCtx being cancelled (cancelled by this
			// function itself, in the upgrade branch, which does not come through here).
			if errors.Is(err, errDaemonUnauthorized) {
				return nil, nil, errDaemonUnauthorized
			}
			return nil, nil, err
		case <-t.C:
			probeCtx, cancel := context.WithTimeout(ctx, wsDialTimeout+wsHelloAckWait)
			w, calls, err := probeWS(probeCtx, rt, rt.wsURL())
			cancel()
			if err == nil {
				// Success: stop HTTP and hand the connection back to the upper WS loop. In the very short
				// switch window both paths may each send one tick; SKIP LOCKED + the cursor naturally keep
				// claims from duplicating.
				cancelHTTP()
				<-httpErr
				log.Printf("WebSocket 重探成功，切回 WS")
				return w, calls, nil
			}
			if errors.Is(err, errWSUnauthorized) {
				return nil, nil, errDaemonUnauthorized
			}
			// Dial failure / not supported both stay on HTTP; try again on the next tick.
			log.Printf("WebSocket 重探失败，继续 HTTP：%v", err)
		}
	}
}

// sleepRng sleeps d±25% (jitter keeps all daemons from redialing at once after the gateway
// recovers/the service restarts; during a rolling deploy every WS drops at the same moment, and
// spreading them out depends entirely on this).
func sleepRng(ctx context.Context, d time.Duration) bool {
	span := int64(d) / 2 // [0, d/2)
	return sleep(ctx, d-time.Duration(span/2)+time.Duration(rand.Int63n(span)))
}

func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > wsBackoffMax {
		return wsBackoffMax
	}
	return d
}

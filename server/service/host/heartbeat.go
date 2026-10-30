package host

import (
	"context"
	"fmt"
	"time"

	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/shared/hostproto"
)

// longPollInterval is the plain database polling interval when there is no Waiter (LISTEN not configured), and also the
// long-poll wait step: when it fires, query the database again. The 20s cap comes from hostproto.HeartbeatLongPoll.
const longPollTick = 20 * time.Second

// streamClaimLimit is the cap of one claim: one exec + some terminal frames. A normal round never gets close;
// when things really pile up, the daemon keeps pulling until empty (after a non-empty response it sends another heartbeat/hello at once).
// HTTP long polling and the WS streaming session share this one number.
const streamClaimLimit = 64

// Heartbeat is the combined entry for daemon heartbeat + pulling new dispatches (HTTP long polling).
//
// What it does, in order:
//  1. TouchHost refreshes last_seen_at / version and the GNU probe snapshot (and also sets the status back to online);
//  2. Claim once immediately (queued after the cursor + delivered rows whose lease expired, for redelivery);
//  3. If nothing, wait on ctx for this host's Wake (PG NOTIFY relayed by cmd/amad),
//     and Claim again on tick/wake-up, until something is found or the longPoll cap is reached.
//
// Advancing the cursor is the daemon's own job: the returned calls are in ascending seq; after the daemon persists them
// the next heartbeat carries the new LastSeq. Pulling old delivered rows again (expired redelivery) is handled idempotently by the daemon
// by callID; this side does not do "return only once".
//
// The WS streaming session (stream.go) uses the same primitives touchFromHeartbeat / claimDispatches /
// waitForDispatch; the only difference is "after getting rows, write an HTTP response or push WS frames".
func (s *Service) Heartbeat(
	ctx context.Context, h persistmodel.Host, req hostproto.HeartbeatRequest, now time.Time,
) (hostproto.HeartbeatResponse, error) {
	if req.HostID == "" {
		req.HostID = h.ID
	}
	if err := s.touchFromHeartbeat(ctx, h.ID, req, now); err != nil {
		return hostproto.HeartbeatResponse{}, err
	}

	deadline := now.Add(time.Duration(hostproto.HeartbeatLongPoll) * time.Second)
	for {
		calls, err := s.claimDispatches(ctx, h.ID, req.LastSeq, time.Now())
		if err != nil {
			return hostproto.HeartbeatResponse{}, err
		}
		if len(calls) > 0 {
			return hostproto.HeartbeatResponse{Calls: calls}, nil
		}

		// No actions: wait for one of three things, "woken / ticker / long-poll cap".
		if time.Now().After(deadline) {
			return hostproto.HeartbeatResponse{}, nil
		}
		waitCtx, cancel := context.WithDeadline(ctx, deadline)
		err = s.waitForDispatch(waitCtx, h.ID)
		cancel()
		if err != nil && ctx.Err() != nil {
			// The outer request was cancelled: pass its err through; the handler sees r.Context() is done and exits quietly.
			// Note the other case: waitCtx itself hits the long-poll cap (DeadlineExceeded) but the outer
			// ctx is not done. Then err is non-nil but the loop continues; back at the top it claims once and after(deadline)
			// returns on empty. This matches exactly the old implementation's "driven by a 20s timer, deadline is not an error".
			return hostproto.HeartbeatResponse{}, ctx.Err()
		}
	}
}

// touchFromHeartbeat renews once from the snapshot in a heartbeat/hello frame: sets status back to online, refreshes
// last_seen_at, and only overwrites snapshot fields that are non-empty (the daemon sends every beat, and leaves out values that did not change).
// When the host has been deleted the lower layer returns ErrNotFound, and the caller uses it to exit with 401/revoked.
func (s *Service) touchFromHeartbeat(
	ctx context.Context, hostID string, req hostproto.HeartbeatRequest, now time.Time,
) error {
	snap := HostHeartbeatSnapshot{
		Name:          req.Name,
		OS:            req.OS,
		Arch:          req.Arch,
		DaemonVersion: req.DaemonVersion,
		GNUToolsOK:    req.GNUToolsOK,
		NativeTools:   req.NativeTools,
	}
	if req.Toolcheck != "" {
		tc := req.Toolcheck
		snap.Toolcheck = &tc
	}
	if err := s.store.TouchHost(ctx, hostID, snap, StatusOnline, now); err != nil {
		return fmt.Errorf("host: 心跳续租失败: %w", err)
	}
	return nil
}

// claimDispatches claims a batch without blocking and converts it to the wire shape. Both queued rows after the cursor and delivered rows
// whose lease expired are picked up (in SQL, SKIP LOCKED atomically sets delivered + renews the claim lease).
func (s *Service) claimDispatches(
	ctx context.Context, hostID string, afterSeq int64, now time.Time,
) ([]hostproto.Dispatch, error) {
	rows, err := s.store.ClaimPendingDispatches(ctx, hostID, afterSeq, now, streamClaimLimit)
	if err != nil {
		return nil, fmt.Errorf("host: 拉取待执行动作失败: %w", err)
	}
	return toDispatches(rows), nil
}

// waitForDispatch blocks until a cross-instance wake-up "this host has new actions", or one fallback tick.
// When s.wait is nil (mem tier / LISTEN not configured) the wake channel is nil, which is never readable in select,
// so it degrades to plain ticks, the same path as the fallback when NOTIFY is lost. If ctx ends first it returns ctx.Err().
//
// ⚠️ Wait registers with a waitCtx created inside this function, not the caller's ctx: the WS pump calls this function in a loop with a long
// connection-level ctx; after a timer/wake return the outer ctx does not end, so a waiter registered directly with it
// and its unregister goroutine would only be released when the whole connection drops: one leak every 20s. defer cancel
// makes sure "unregister as soon as this wait returns".
func (s *Service) waitForDispatch(ctx context.Context, hostID string) error {
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wake <-chan struct{}
	if s.wait != nil {
		wake = s.wait.Wait(waitCtx, hostID)
	}
	timer := time.NewTimer(longPollTick)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	case <-wake:
		return nil
	}
}

func toDispatches(rows []persistmodel.HostDispatch) []hostproto.Dispatch {
	out := make([]hostproto.Dispatch, 0, len(rows))
	for _, r := range rows {
		out = append(out, hostproto.Dispatch{
			ID:         r.ID,
			Seq:        r.Seq,
			Kind:       r.Kind,
			SessionID:  r.SessionID,
			TerminalID: r.TerminalID,
			Payload:    []byte(r.Payload),
		})
	}
	return out
}

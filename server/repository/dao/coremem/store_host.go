// In-memory implementation of the three host executor tables; behaves the same as coredao/store_pg_host.go case by case.
//
// This whole group runs under the single lock s.mu: seq assignment on enqueue and the claim CAS are serialized by it.
// On the PG side the matching pieces are an advisory lock + a single UPDATE with `for update skip locked`. NOTIFY is a no-op
// in the in-memory version (there is no cross-process consumer; the daemon long-poll runs in another process, and AMA_STORE=mem is
// a single-machine fallback anyway).
package coremem

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/chyroc/ama/server/model/persistmodel"
	// HostHeartbeatSnapshot is declared in the domain package, not persistmodel: coremem's rule is
	// not to import core (that would be a cycle), but service/host is a leaf just like model: it only depends on
	// model/* and shared/hostproto and does not depend back on core/repository, so importing it is no cycle.
	"github.com/chyroc/ama/server/service/host"
)

// execClaimLease / termClaimLease are the claim lease lengths, matching
// `interval '150 seconds'` / `'30 seconds'` in the PG claim SQL exactly. Keeping one constant on each side is deliberate:
// changing one but not the other would make the mem tests miss PG's redelivery timing difference.
const (
	execClaimLease = 150 * time.Second
	termClaimLease = 30 * time.Second
)

// UpsertRegisteredHost: see the host.Store contract. On an existing row only the registration snapshot columns are refreshed;
// id / token_hash / created_at / status / last_seen_at are kept.
func (s *MemStore) UpsertRegisteredHost(
	_ context.Context, row persistmodel.Host,
) (persistmodel.Host, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, old := range s.data.hosts {
		if old.OwnerEmail == row.OwnerEmail && old.HostUUID == row.HostUUID {
			old.Name = row.Name
			old.OS = row.OS
			old.Arch = row.Arch
			old.DaemonVersion = row.DaemonVersion
			old.GNUToolsOK = row.GNUToolsOK
			old.Toolcheck = row.Toolcheck
			old.NativeTools = row.NativeTools
			// ID / TokenHash / CreatedAt / Status / LastSeenAt are deliberately left alone.
			s.data.hosts[i] = old
			return cloneHost(old), false, nil
		}
	}
	s.data.hosts = append(s.data.hosts, row)
	return cloneHost(row), true, nil
}

func (s *MemStore) GetHost(_ context.Context, id string) (persistmodel.Host, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, h := range s.data.hosts {
		if h.ID == id {
			return cloneHost(h), nil
		}
	}
	return persistmodel.Host{}, persistmodel.ErrNotFound
}

func (s *MemStore) GetHostByOwnerUUID(
	_ context.Context, ownerEmail, hostUUID string,
) (persistmodel.Host, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, h := range s.data.hosts {
		if h.OwnerEmail == ownerEmail && h.HostUUID == hostUUID {
			return cloneHost(h), nil
		}
	}
	return persistmodel.Host{}, persistmodel.ErrNotFound
}

func (s *MemStore) GetHostByTokenHash(
	_ context.Context, tokenHash string,
) (persistmodel.Host, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Matches `token_hash <> ''` on the PG side: an empty hash must not match any machine.
	if tokenHash == "" {
		return persistmodel.Host{}, persistmodel.ErrNotFound
	}
	for _, h := range s.data.hosts {
		if h.TokenHash == tokenHash {
			return cloneHost(h), nil
		}
	}
	return persistmodel.Host{}, persistmodel.ErrNotFound
}

// TouchHost heartbeat: online status and last_seen_at are always refreshed; snapshot fields behave like the PG case when:
// string fields are refreshed only when non-empty, pointer fields only when non-nil, zero values keep the old value.
func (s *MemStore) TouchHost(
	_ context.Context, id string, snap host.HostHeartbeatSnapshot,
	onlineStatus string, now time.Time,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, h := range s.data.hosts {
		if h.ID == id {
			h.Status = onlineStatus
			h.LastSeenAt = now
			if snap.Name != "" {
				h.Name = snap.Name
			}
			if snap.OS != "" {
				h.OS = snap.OS
			}
			if snap.Arch != "" {
				h.Arch = snap.Arch
			}
			if snap.DaemonVersion != "" {
				h.DaemonVersion = snap.DaemonVersion
			}
			if snap.GNUToolsOK != nil {
				h.GNUToolsOK = *snap.GNUToolsOK
			}
			if snap.Toolcheck != nil {
				h.Toolcheck = *snap.Toolcheck
			}
			if snap.NativeTools != nil {
				h.NativeTools = *snap.NativeTools
			}
			s.data.hosts[i] = h
			return nil
		}
	}
	return persistmodel.ErrNotFound
}

func (s *MemStore) ListHostsByOwner(
	_ context.Context, ownerEmail string,
) ([]persistmodel.Host, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]persistmodel.Host, 0, len(s.data.hosts))
	for _, h := range s.data.hosts {
		if h.OwnerEmail == ownerEmail {
			out = append(out, cloneHost(h))
		}
	}
	sortHosts(out)
	return out, nil
}

// sortHosts sorts by (created_at, id) ascending, matching the PG order by exactly.
func sortHosts(hosts []persistmodel.Host) {
	sort.SliceStable(hosts, func(i, j int) bool {
		if !hosts[i].CreatedAt.Equal(hosts[j].CreatedAt) {
			return hosts[i].CreatedAt.Before(hosts[j].CreatedAt)
		}
		return hosts[i].ID < hosts[j].ID
	})
}

// DeleteHost is idempotent; matching the PG foreign key on delete cascade, it also deletes workspace bindings.
// host_dispatches is left alone on both sides (old commands kept for debugging).
func (s *MemStore) DeleteHost(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.hosts = filterSlice(s.data.hosts, func(h persistmodel.Host) bool { return h.ID != id })
	// Matching the PG foreign key on delete cascade, also delete workspace bindings. host_dispatches
	// is left alone on both sides (old commands kept for debugging).
	s.data.workspaceHosts = filterSlice(s.data.workspaceHosts,
		func(b persistmodel.WorkspaceHost) bool { return b.HostID != id })
	// The counter goes away with the hosts row (same as deleting the row in PG); leftover host_dispatches are left alone like in PG,
	// kept for debugging, and the old id will never be claimed again.
	delete(s.data.hostDispatchSeq, id)
	return nil
}

func (s *MemStore) ListHostsLastSeenBefore(
	_ context.Context, before time.Time, limit int,
) ([]persistmodel.Host, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]persistmodel.Host, 0)
	for _, h := range s.data.hosts {
		// Zero value = SQL NULL: rows that never sent a heartbeat are also in scope, matching `is null or <`.
		if h.LastSeenAt.IsZero() || h.LastSeenAt.Before(before) {
			out = append(out, cloneHost(h))
		}
	}
	sortHosts(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// MarkHostOffline is a CAS: it matches only if online and the heartbeat time is still before seenBefore,
// matching PG's `where status='online' and last_seen_at < seenBefore`.
// A zero LastSeenAt (= SQL NULL) does not meet the condition, same as PG's NULL comparison.
// now is unused (hosts has no offline-time column).
func (s *MemStore) MarkHostOffline(
	_ context.Context, id string, seenBefore, _ time.Time,
) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, h := range s.data.hosts {
		if h.ID == id {
			if h.Status != "online" || h.LastSeenAt.IsZero() ||
				!h.LastSeenAt.Before(seenBefore) {
				return false, nil
			}
			h.Status = "offline"
			s.data.hosts[i] = h
			return true, nil
		}
	}
	// Machine does not exist: UPDATE matches 0 rows, returns false without error, same shape as PG.
	return false, nil
}

// -- workspace_hosts --------------------------------------------------------

// BindWorkspaceHost is idempotent. The Service layer has already checked ownership before calling; this adds an existence check
// to match the PG foreign key: when the host does not exist it returns an error wrapping ErrNotFound, so the upper layer can use errors.Is.
func (s *MemStore) BindWorkspaceHost(_ context.Context, b persistmodel.WorkspaceHost) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	hostExists := false
	for _, h := range s.data.hosts {
		if h.ID == b.HostID {
			hostExists = true
			break
		}
	}
	if !hostExists {
		return fmt.Errorf("core: 绑定宿主机到空间失败: %w", persistmodel.ErrNotFound)
	}
	for _, old := range s.data.workspaceHosts {
		if old.WorkspaceID == b.WorkspaceID && old.HostID == b.HostID {
			// on conflict do nothing: a repeated binding is idempotent and returns nil.
			return nil
		}
	}
	s.data.workspaceHosts = append(s.data.workspaceHosts, b)
	return nil
}

func (s *MemStore) ListWorkspaceHosts(
	_ context.Context, workspaceID string,
) ([]persistmodel.BoundHost, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// First take this workspace's bindings and sort them in binding order, then join the host rows. The sort key is the binding table's
	// (created_at, id), matching order by wh.created_at, wh.id in the PG SQL.
	binds := make([]persistmodel.WorkspaceHost, 0)
	for _, b := range s.data.workspaceHosts {
		if b.WorkspaceID == workspaceID {
			binds = append(binds, b)
		}
	}
	sort.SliceStable(binds, func(i, j int) bool {
		if !binds[i].CreatedAt.Equal(binds[j].CreatedAt) {
			return binds[i].CreatedAt.Before(binds[j].CreatedAt)
		}
		return binds[i].ID < binds[j].ID
	})
	out := make([]persistmodel.BoundHost, 0, len(binds))
	for _, b := range binds {
		for _, h := range s.data.hosts {
			// Inner join: bindings whose host was deleted do not show up (DeleteHost already cascades the bindings;
			// this check is a second safety net).
			if h.ID == b.HostID {
				out = append(out, persistmodel.BoundHost{
					Host:      cloneHost(h),
					BindingID: b.ID,
					AddedBy:   b.AddedBy,
					BoundAt:   b.CreatedAt,
				})
				break
			}
		}
	}
	return out, nil
}

func (s *MemStore) GetWorkspaceHost(
	_ context.Context, workspaceID, hostID string,
) (persistmodel.WorkspaceHost, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, b := range s.data.workspaceHosts {
		if b.WorkspaceID == workspaceID && b.HostID == hostID {
			return b, nil
		}
	}
	return persistmodel.WorkspaceHost{}, persistmodel.ErrNotFound
}

func (s *MemStore) UnbindWorkspaceHost(
	_ context.Context, workspaceID, hostID string,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.workspaceHosts = filterSlice(s.data.workspaceHosts,
		func(b persistmodel.WorkspaceHost) bool {
			return !(b.WorkspaceID == workspaceID && b.HostID == hostID)
		})
	return nil
}

// -- host_dispatches --------------------------------------------------------

// EnqueueDispatch assigns the host's next number under the lock (the PG side serializes the same thing with an advisory lock),
// and hard-codes state to queued. The Seq / State / DeliveredAt / ClaimedUntil passed in are all ignored.
// NOTIFY is a no-op.
//
// Number = greatest(max(seq) of existing rows, counter)+1, then the counter is written back. The counter's lifecycle is
// separate from the rows: done rows of hostDispatches get purged, but the counter is not cleared. Otherwise
// the number wraps back to 1 and the daemon's persisted last_seq cursor can never fetch new commands again (same reason as
// hosts.last_dispatch_seq on the PG side). The existing-max branch is, like in PG, for rolling-deploy compatibility:
// when old code pushed a large number straight into the slice, numbers do not clash.
func (s *MemStore) EnqueueDispatch(
	_ context.Context, d persistmodel.HostDispatch,
) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var maxRow int64
	for _, x := range s.data.hostDispatches {
		if x.HostID == d.HostID && x.Seq > maxRow {
			maxRow = x.Seq
		}
	}
	if s.data.hostDispatchSeq == nil {
		s.data.hostDispatchSeq = map[string]int64{}
	}
	seq := maxRow
	if c := s.data.hostDispatchSeq[d.HostID]; c > seq {
		seq = c
	}
	seq++
	s.data.hostDispatchSeq[d.HostID] = seq
	// Sync to the hosts row: on the PG side the counter is hosts.last_dispatch_seq, which GetHost can read;
	// mem must keep the same behavior (the Store contract tests make the same assertions on both implementations).
	for i := range s.data.hosts {
		if s.data.hosts[i].ID == d.HostID {
			s.data.hosts[i].LastDispatchSeq = seq
			break
		}
	}
	d.Seq = seq
	d.State = "queued"
	d.DeliveredAt = time.Time{}
	d.ClaimedUntil = time.Time{}
	s.data.hostDispatches = append(s.data.hostDispatches, d)
	return seq, nil
}

// ClaimPendingDispatches selects rows and changes status under one write lock, equivalent to PG's single CTE UPDATE:
// new queued rows after the cursor, or delivered rows whose lease expired (redelivery).
//
// ⚠️ The redelivery branch only accepts ClaimedUntil non-zero and before now. The zero value stands for SQL NULL; in PG
// NULL < $now is not true, so do not treat the zero value as "the distant past", or it would claim too much.
func (s *MemStore) ClaimPendingDispatches(
	_ context.Context, hostID string, afterSeq int64, now time.Time, limit int,
) ([]persistmodel.HostDispatch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	picked := make([]int, 0)
	for i, d := range s.data.hostDispatches {
		if d.HostID != hostID {
			continue
		}
		claimable := false
		switch d.State {
		case "queued":
			claimable = d.Seq > afterSeq
		case "delivered":
			claimable = !d.ClaimedUntil.IsZero() && d.ClaimedUntil.Before(now)
		}
		if claimable {
			picked = append(picked, i)
		}
	}
	// Ascending by seq, matching PG's order by seq.
	sort.Ints(picked)
	if limit > 0 && len(picked) > limit {
		picked = picked[:limit]
	}
	out := make([]persistmodel.HostDispatch, 0, len(picked))
	for _, i := range picked {
		d := &s.data.hostDispatches[i]
		lease := termClaimLease
		if d.Kind == "exec" {
			lease = execClaimLease
		}
		d.State = "delivered"
		d.DeliveredAt = now
		d.ClaimedUntil = now.Add(lease)
		d.UpdatedAt = now
		out = append(out, *d)
	}
	return out, nil
}

// CompleteDispatch is a CAS: only a row matching id+hostID and not done is updated; repeated reports
// are ignored silently and **do not overwrite result**, matching PG's update with `state <> 'done'`.
func (s *MemStore) CompleteDispatch(
	_ context.Context, hostID, id, resultJSON string, now time.Time,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, d := range s.data.hostDispatches {
		if d.ID == id && d.HostID == hostID {
			if d.State == "done" {
				return nil
			}
			d.State = "done"
			d.Result = resultJSON
			d.UpdatedAt = now
			s.data.hostDispatches[i] = d
			return nil
		}
	}
	// Missing: UPDATE matches 0 rows, not an error.
	return nil
}

// AckDispatches acks term_* frames in bulk, matching that PG UPDATE: only term_* is accepted,
// only unfinished rows are updated, repeated acks are idempotent.
func (s *MemStore) AckDispatches(
	_ context.Context, hostID string, seqs []int64, now time.Time,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := make(map[int64]struct{}, len(seqs))
	for _, seq := range seqs {
		want[seq] = struct{}{}
	}
	for i, d := range s.data.hostDispatches {
		if d.HostID != hostID || d.State == "done" {
			continue
		}
		if !isTermKind(d.Kind) {
			continue
		}
		if _, ok := want[d.Seq]; !ok {
			continue
		}
		d.State = "done"
		d.UpdatedAt = now
		s.data.hostDispatches[i] = d
	}
	return nil
}

// isTermKind checks the four terminal frame kinds; the allowlist in (...) on the PG side matches it exactly.
func isTermKind(kind string) bool {
	return kind == "term_open" || kind == "term_input" ||
		kind == "term_resize" || kind == "term_close"
}

func (s *MemStore) GetDispatch(_ context.Context, id string) (persistmodel.HostDispatch, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, d := range s.data.hostDispatches {
		if d.ID == id {
			return d, nil
		}
	}
	return persistmodel.HostDispatch{}, persistmodel.ErrNotFound
}

func (s *MemStore) FailPendingExecsForHost(
	_ context.Context, hostID, resultJSON string, now time.Time,
) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for i, d := range s.data.hostDispatches {
		if d.HostID != hostID || (d.Kind != "exec" && d.Kind != "tool") {
			continue
		}
		if d.State == "queued" || d.State == "delivered" {
			d.State = "done"
			d.Result = resultJSON
			d.UpdatedAt = now
			s.data.hostDispatches[i] = d
			n++
		}
	}
	return n, nil
}

func (s *MemStore) PurgeDoneExecDispatches(
	_ context.Context, before time.Time,
) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.data.hostDispatches[:0]
	var n int64
	for _, d := range s.data.hostDispatches {
		if d.Kind == "exec" && d.State == "done" && d.CreatedAt.Before(before) {
			n++
			continue
		}
		kept = append(kept, d)
	}
	s.data.hostDispatches = kept
	return n, nil
}

func (s *MemStore) PurgeOldTermDispatches(
	_ context.Context, before time.Time,
) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.data.hostDispatches[:0]
	var n int64
	for _, d := range s.data.hostDispatches {
		if d.State == "done" && isTermKind(d.Kind) && d.CreatedAt.Before(before) {
			n++
			continue
		}
		kept = append(kept, d)
	}
	s.data.hostDispatches = kept
	return n, nil
}

// cloneHost returns an independent copy of a host row. Host has only scalar fields now, so a value copy is enough;
// the function stays so call sites need no change when a slice field is added later (the convention of other clones in this package).
func cloneHost(h persistmodel.Host) persistmodel.Host {
	return h
}

// TerminalHostID is the mem version of PG's TerminalHostID: search backwards for the latest frame of that terminal.
func (s *MemStore) TerminalHostID(_ context.Context, terminalID string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := len(s.data.hostDispatches) - 1; i >= 0; i-- {
		d := s.data.hostDispatches[i]
		if d.TerminalID == terminalID && strings.HasPrefix(d.Kind, "term_") {
			return d.HostID, nil
		}
	}
	return "", persistmodel.ErrNotFound
}

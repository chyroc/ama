// In-memory implementation of queued messages waiting to be sent.
//
// **The rules must match the PG version exactly** (status allowlist, first-in-first-out, an expired lease can be taken over).
// If the two drift apart, "unit tests on MemStore are all green while production on PG behaves differently", always with zero errors,
// the same situation as the AppendRunEvent dedup case.

package coremem

import (
	"context"
	"sort"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
)

// pendingRow is the in-memory form of one queued message.
//
// It has two more things than apimodel.PendingMessage: createdAt (the sort key; a timestamptz column on the PG side)
// and the lease. Neither goes into the HTTP contract, so they are not in the domain type.
type pendingRow struct {
	msg       apimodel.PendingMessage
	createdAt time.Time
	// ord orders messages queued at the same instant; it matches PG's bigserial.
	ord int64
	// claimedUntil is the claim lease; the zero value means nobody has claimed it.
	claimedUntil time.Time
}

func (s *MemStore) EnqueuePendingMessage(
	_ context.Context, m apimodel.PendingMessage, createdAt time.Time,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	m.Status = apimodel.NormalizePendingStatus(m.Status)
	m.Source = apimodel.NormalizePendingSource(m.Source)
	m.QueuedAt = apimodel.FormatISO(createdAt)

	s.data.pendingOrd++
	s.data.pending = append(s.data.pending, pendingRow{
		msg: m, createdAt: createdAt, ord: s.data.pendingOrd,
	})
	return nil
}

func (s *MemStore) ListPendingMessages(
	_ context.Context, runID string,
) ([]apimodel.PendingMessage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows := s.pendingFor(runID)
	out := make([]apimodel.PendingMessage, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.msg)
	}
	return out, nil
}

// ClaimNextPendingMessage claims the first pending message of a conversation. See the notes on the core.Store interface.
//
// **First-in-first-out** matches PG's `order by created_at, ord` exactly. If the order is wrong the user notices right away
// (the two lines they queued get swapped), so this is not an internal detail, it is the contract.
func (s *MemStore) ClaimNextPendingMessage(
	_ context.Context, runID, owner string, now time.Time, leaseFor time.Duration,
) (apimodel.PendingMessage, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	/*
	 * ⚠️ **Must use pendingFor to get the sorted list; do not scan s.data.pending directly.**
	 *
	 * The first version scanned directly, reasoning that "append order equals ord order". That is true,
	 * but **queue order is not send order**: send order follows createdAt (on the PG side,
	 * `order by created_at, ord`). The two diverge when "messages are queued with times going backwards",
	 * and that is not hypothetical: the Feishu and web paths queue independently, so arrival order is never guaranteed.
	 *
	 * The symptom is picking the wrong head: the user's second line is sent first. The second part of
	 * TestPendingMessagesAreFIFO (pick order) pins this.
	 */
	for _, sorted := range s.pendingFor(runID) {
		if apimodel.NormalizePendingStatus(sorted.msg.Status) != apimodel.PendingWaiting {
			continue
		}
		// Someone is sending and the lease has not expired → skip. If expired it can be taken over (the original owner may be dead).
		if sorted.claimedUntil.After(now) {
			continue
		}
		// pendingFor returns copies, so changes must go to the original slice.
		for i := range s.data.pending {
			r := &s.data.pending[i]
			if r.msg.ID != sorted.msg.ID {
				continue
			}
			r.msg.Status = apimodel.PendingSending
			r.msg.Attempts++
			r.claimedUntil = now.Add(leaseFor)
			return r.msg, true, nil
		}
	}
	// Nothing pending. **Not an error**.
	return apimodel.PendingMessage{}, false, nil
}

func (s *MemStore) DeletePendingMessage(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := s.data.pending[:0]
	for _, r := range s.data.pending {
		if r.msg.ID != id {
			out = append(out, r)
		}
	}
	s.data.pending = out
	return nil
}

func (s *MemStore) FailPendingMessage(_ context.Context, id, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.data.pending {
		if s.data.pending[i].msg.ID != id {
			continue
		}
		s.data.pending[i].msg.Status = apimodel.PendingFailed
		s.data.pending[i].msg.LastError = reason
		// Clear the lease, same as the PG version: otherwise "resend now" would have to claim it again.
		s.data.pending[i].claimedUntil = time.Time{}
		return nil
	}
	return nil
}

// pendingFor gets a conversation's queued messages in queue order. The caller must hold the lock.
func (s *MemStore) pendingFor(runID string) []pendingRow {
	out := make([]pendingRow, 0)
	for _, r := range s.data.pending {
		if r.msg.RunID == runID {
			out = append(out, r)
		}
	}
	// Matches PG's `order by created_at, ord` exactly.
	//
	// **Sorting by created_at alone is not enough**: two messages queued in the same millisecond would swap because the sort is not stable,
	// and that is exactly the error the user notices right away (same as the ordering contract in domain).
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].createdAt.Equal(out[j].createdAt) {
			return out[i].createdAt.Before(out[j].createdAt)
		}
		return out[i].ord < out[j].ord
	})
	return out
}

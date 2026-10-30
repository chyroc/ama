// In-memory implementation of the async task "sync the agent to Ark right after it is saved".
//
// ⚠️ **This one must match the coredao version exactly in behavior**, especially three checks:
// "an agent can have only one unfinished task", "a running task whose lease expired can be claimed again",
// "finishing must write finished_at". If the two drift apart nothing errors; the contract suite just passes falsely on one side,
// and this repo's rule is that the same test cases run **once each** against MemStore and PGStore.

package coremem

import (
	"context"
	"sort"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// EnqueueAgentSyncTask: see the notes on core.Store.
//
// The PG side relies on a partial unique index to decide "an unfinished task already exists"; here it is one linear scan.
// **The behavior must be the same**: on a clash, do not create a new one, and only mark the running one for resync.
func (s *MemStore) EnqueueAgentSyncTask(
	_ context.Context, task persistmodel.AgentSyncTaskRow,
) (persistmodel.AgentSyncTaskRow, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.data.agentSyncTasks {
		t := &s.data.agentSyncTasks[i]
		if t.WorkspaceID != task.WorkspaceID || t.AgentID != task.AgentID {
			continue
		}
		if !t.FinishedAt.IsZero() {
			// Finished tasks do not "hold the slot": the PG unique index predicate is
			// `where finished_at is null`, and this must match.
			continue
		}
		// ⚠️ Only mark the **running** one. The pending one has not been claimed yet; when claimed it will read
		// the latest agent anyway, so marking it would only make it run a useless second round (same as the case when on the PG side).
		if t.Status == string(apimodel.AgentSyncRunning) {
			t.ResyncRequested = true
		}
		t.UpdatedAt = task.UpdatedAt
		return *t, false, nil
	}

	s.data.agentSyncOrd++
	task.Ord = s.data.agentSyncOrd
	s.data.agentSyncTasks = append(s.data.agentSyncTasks, task)
	return task, true, nil
}

// ClaimDueAgentSyncTasks: see the notes on core.Store.
//
// The union of the two branches is exactly the same as on the PG side: pending ones, and running ones whose lease expired.
// The second branch is where the requirement "a task whose instance died must not stay running forever" is met.
func (s *MemStore) ClaimDueAgentSyncTasks(
	_ context.Context, owner string, now time.Time, leaseFor time.Duration, limit int,
) ([]persistmodel.AgentSyncTaskRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Claim in queue order, matching PG's `order by created_at, ord`.
	idx := make([]int, 0, len(s.data.agentSyncTasks))
	for i := range s.data.agentSyncTasks {
		idx = append(idx, i)
	}
	sort.SliceStable(idx, func(a, b int) bool {
		x, y := s.data.agentSyncTasks[idx[a]], s.data.agentSyncTasks[idx[b]]
		if !x.CreatedAt.Equal(y.CreatedAt) {
			return x.CreatedAt.Before(y.CreatedAt)
		}
		return x.Ord < y.Ord
	})

	out := make([]persistmodel.AgentSyncTaskRow, 0)
	for _, i := range idx {
		if limit > 0 && len(out) >= limit {
			break
		}
		t := &s.data.agentSyncTasks[i]
		if !t.FinishedAt.IsZero() {
			continue
		}
		claimable := t.Status == string(apimodel.AgentSyncPending) ||
			(t.Status == string(apimodel.AgentSyncRunning) &&
				!t.ClaimedUntil.IsZero() && t.ClaimedUntil.Before(now))
		if !claimable {
			continue
		}
		t.Status = string(apimodel.AgentSyncRunning)
		t.ClaimedBy = owner
		t.ClaimedUntil = now.Add(leaseFor)
		t.AttemptCount++
		t.UpdatedAt = now
		out = append(out, *t)
	}
	return out, nil
}

// FinishAgentSyncTask: see the notes on core.Store.
//
// ⚠️ finished_at must be written: it decides "can this agent queue another task".
// Without it the next save is treated as "an unfinished task already exists" and never creates a new one (zero errors).
func (s *MemStore) FinishAgentSyncTask(
	_ context.Context, id string, status apimodel.AgentSyncStatus,
	failureReason string, failureStage apimodel.AgentSyncStage, now time.Time,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.data.agentSyncTasks {
		t := &s.data.agentSyncTasks[i]
		if t.ID != id {
			continue
		}
		t.Status = string(status)
		t.FailureReason = failureReason
		t.FailureStage = string(failureStage)
		t.ClaimedBy = ""
		t.ClaimedUntil = time.Time{}
		t.ResyncRequested = false
		t.UpdatedAt = now
		t.FinishedAt = now
		return nil
	}
	// Missing is not an error (idempotent), same as the other delete/update methods in this repo.
	return nil
}

// RequeueAgentSyncTask: see the notes on core.Store.
//
// ⚠️ Clear the lease for the same reason as the PG side: otherwise the next claim tick is blocked by its own unexpired lease.
func (s *MemStore) RequeueAgentSyncTask(_ context.Context, id string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.data.agentSyncTasks {
		t := &s.data.agentSyncTasks[i]
		if t.ID != id {
			continue
		}
		t.Status = string(apimodel.AgentSyncPending)
		t.ResyncRequested = false
		t.ClaimedBy = ""
		t.ClaimedUntil = time.Time{}
		t.UpdatedAt = now
		return nil
	}
	return nil
}

// LatestAgentSyncTask: see the notes on core.Store.
func (s *MemStore) LatestAgentSyncTask(
	_ context.Context, agentID string,
) (persistmodel.AgentSyncTaskRow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var best persistmodel.AgentSyncTaskRow
	found := false
	for _, t := range s.data.agentSyncTasks {
		if t.AgentID != agentID {
			continue
		}
		// Take the newest one. **On ties compare ord**, matching PG's `order by created_at desc, ord desc`.
		// Two tasks queued in the same millisecond cannot be ordered by time alone, which would make the frontend sometimes read the previous one.
		if !found || t.CreatedAt.After(best.CreatedAt) ||
			(t.CreatedAt.Equal(best.CreatedAt) && t.Ord > best.Ord) {
			best = t
			found = true
		}
	}
	if !found {
		return persistmodel.AgentSyncTaskRow{}, persistmodel.ErrNotFound
	}
	return best, nil
}

// PurgeFinishedAgentSyncTasks: see the notes on core.Store.
func (s *MemStore) PurgeFinishedAgentSyncTasks(
	_ context.Context, before time.Time,
) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	kept := s.data.agentSyncTasks[:0]
	var removed int64
	for _, t := range s.data.agentSyncTasks {
		if !t.FinishedAt.IsZero() && t.FinishedAt.Before(before) {
			removed++
			continue
		}
		kept = append(kept, t)
	}
	s.data.agentSyncTasks = kept
	return removed, nil
}

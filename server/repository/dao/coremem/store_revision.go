// In-memory implementation of entity history versions (revisions).
//
// Must behave the same as the PG side (repository/dao/coredao/store_pg_revision.go);
// the contract tests (runStoreSuite) run on both. Two behaviors deliberately kept the same:
//
//   - **The sequence number is assigned by the implementation**; the No passed in by the caller is ignored;
//   - **Lists do not include the snapshot body**; use GetEntityRevision for the body.
//
// ⚠️ This package must not use a single core symbol (that would be a test cycle, see repository/schema/README.md),
// so it only depends on model/apimodel and model/persistmodel.
package coremem

import (
	"context"
	"sort"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

func (s *MemStore) AppendEntityRevision(
	_ context.Context, rev apimodel.Revision,
) (apimodel.Revision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Sequence number = current max for this entity + 1. **The rev.No passed in is ignored**, same as the PG side.
	next := 1
	for _, r := range s.data.revisions {
		if r.Kind == rev.Kind && r.EntityID == rev.EntityID && r.No >= next {
			next = r.No + 1
		}
	}

	out := persistmodel.CloneRevision(rev)
	out.No = next
	out.Reason = apimodel.NormalizeRevisionReason(rev.Reason)
	s.data.revisions = append(s.data.revisions, out)
	return persistmodel.CloneRevision(out), nil
}

func (s *MemStore) ListEntityRevisions(
	_ context.Context, kind apimodel.RevisionKind, entityID string, limit int,
) ([]apimodel.Revision, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]apimodel.Revision, 0)
	for _, r := range s.data.revisions {
		if r.Kind != kind || r.EntityID != entityID {
			continue
		}
		item := persistmodel.CloneRevision(r)
		// **Lists do not include the body**, matching the select column list on the PG side. Without clearing it the in-memory
		// implementation would return one more field than PG, so "the frontend gets the snapshot" would work locally
		// and vanish in production, and that kind of difference only shows up against a real database.
		item.Snapshot = nil
		out = append(out, item)
	}
	// Newest first.
	sort.Slice(out, func(i, j int) bool { return out[i].No > out[j].No })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *MemStore) GetEntityRevision(_ context.Context, id string) (apimodel.Revision, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.data.revisions {
		if r.ID == id {
			return persistmodel.CloneRevision(r), nil
		}
	}
	return apimodel.Revision{}, persistmodel.ErrNotFound
}

func (s *MemStore) DeleteEntityRevisions(
	_ context.Context, kind apimodel.RevisionKind, entityID string,
) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	kept := make([]apimodel.Revision, 0, len(s.data.revisions))
	var removed int64
	for _, r := range s.data.revisions {
		if r.Kind == kind && r.EntityID == entityID {
			removed++
			continue
		}
		kept = append(kept, r)
	}
	s.data.revisions = kept
	return removed, nil
}

func (s *MemStore) PurgeExcessRevisions(_ context.Context, keep int) (int64, error) {
	// ⚠️ keep <= 0 returns right away, same as the PG side: going on would wipe all history,
	// and misconfiguring it to 0 is possible. "Better not delete" is the safe side.
	if keep <= 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// Collect sequence numbers per entity and compute each one's "keep threshold".
	type entityKey struct {
		kind apimodel.RevisionKind
		id   string
	}
	nos := map[entityKey][]int{}
	for _, r := range s.data.revisions {
		k := entityKey{r.Kind, r.EntityID}
		nos[k] = append(nos[k], r.No)
	}
	// Threshold = the keep-th largest sequence number of the entity; with fewer than keep rows there is no threshold (keep all).
	floor := map[entityKey]int{}
	for k, list := range nos {
		if len(list) <= keep {
			continue
		}
		sort.Sort(sort.Reverse(sort.IntSlice(list)))
		floor[k] = list[keep-1]
	}

	kept := make([]apimodel.Revision, 0, len(s.data.revisions))
	var removed int64
	for _, r := range s.data.revisions {
		if f, ok := floor[entityKey{r.Kind, r.EntityID}]; ok && r.No < f {
			removed++
			continue
		}
		kept = append(kept, r)
	}
	s.data.revisions = kept
	return removed, nil
}

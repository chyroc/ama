// PostgreSQL implementation of entity history versions (revisions).
//
// A separate file instead of merging into store_pg.go: that one is already close to a thousand lines, and this domain has two things it lacks:
// **under concurrency the sequence number is assigned by retrying on a unique constraint**, and **cleanup with a window function grouped by entity**.
// Table structure and per-field reasons are in repository/schema/entity_revisions.sql.
//
// # The snapshot column is text, not jsonb
//
// The whole value of a snapshot is byte-for-byte fidelity (three months later, restore "what was filled in back then" exactly),
// and jsonb reorders keys and squeezes whitespace. It is exactly not faithful, and it would make the UI diff show a false
// "changed" out of nothing. The reasoning is spelled out in the header of that .sql file; the precedent is run_events.raw.
package coredao

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/shared/infra/pg"
)

// revisionSelectColumns is the column list for reading snapshots (**without snapshot**).
//
// The list endpoint deliberately does not fetch snapshot: one agent snapshot is a few KB, 20 of them tens of KB,
// and the list only renders names and times. Single reads use revisionSelectColumnsFull.
const revisionSelectColumns = `id, workspace_id, entity_kind, entity_id, revision_no,
	'' as snapshot, summary, created_at, created_by, reason`

// revisionSelectColumnsFull has one more column than the above: the snapshot body.
const revisionSelectColumnsFull = `id, workspace_id, entity_kind, entity_id, revision_no,
	snapshot, summary, created_at, created_by, reason`

// appendRevisionAttempts is how many times to retry after hitting the unique constraint.
//
// The sequence number is "current max + 1", a check-then-write: several instances saving the same entity at once
// compute the same number, and the later write hits entity_revisions_no_key. Retrying means querying the max again.
//
// 3, not 1: two instances clashing twice in a row is possible (both retry, at the same time again). Not more either:
// more retries on this path do not buy correctness, they only make an already abnormal case wait longer,
// and **failing to record a snapshot is allowed to degrade anyway** (see core/revision.go).
const appendRevisionAttempts = 3

// AppendEntityRevision appends a snapshot; this function assigns the sequence number.
//
// ⚠️ **The rev.No passed in is ignored**: the number must be decided on the database side; see the comment on the Store interface.
func (s *PGStore) AppendEntityRevision(
	ctx context.Context, rev apimodel.Revision,
) (apimodel.Revision, error) {
	// When snapshot is empty store the literal "null" instead of an empty string: an empty string is not valid JSON,
	// json.Unmarshal would fail when reading it back, and that error would show up at the moment of **rollback**,
	// days away from the real cause (it was already empty when written).
	snapshot := string(rev.Snapshot)
	if snapshot == "" {
		snapshot = "null"
	}

	var lastErr error
	for attempt := 0; attempt < appendRevisionAttempts; attempt++ {
		// Next number = current max + 1. coalesce covers "none exist yet".
		var next int
		err := s.client.QueryRow(ctx,
			`select coalesce(max(revision_no), 0) + 1 from entity_revisions
			  where entity_kind = $1 and entity_id = $2`,
			string(rev.Kind), rev.EntityID).Scan(&next)
		if err != nil {
			return apimodel.Revision{}, fmt.Errorf("core: 算历史版本序号失败: %w", err)
		}

		_, err = s.client.Exec(ctx,
			`insert into entity_revisions
			     (id, workspace_id, entity_kind, entity_id, revision_no,
			      snapshot, summary, created_at, created_by, reason)
			 values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			rev.ID, rev.WorkspaceID, string(rev.Kind), rev.EntityID, next,
			snapshot, rev.Summary, rev.CreatedAt, rev.CreatedBy,
			string(apimodel.NormalizeRevisionReason(rev.Reason)))
		if err == nil {
			out := rev
			out.No = next
			out.Reason = apimodel.NormalizeRevisionReason(rev.Reason)
			return out, nil
		}
		if !pg.IsUniqueViolation(err) {
			return apimodel.Revision{}, fmt.Errorf("core: 存历史版本失败: %w", err)
		}
		// Hit the unique constraint: **either** another instance just took this number (recomputing once is enough),
		// **or** rev.ID itself is a duplicate (then it clashes no matter how many retries).
		// Handling both the same way is fine: the latter still fails after the retries and finally returns an error.
		lastErr = err
	}
	return apimodel.Revision{}, fmt.Errorf(
		"core: 存历史版本连续 %d 次撞序号冲突: %w", appendRevisionAttempts, lastErr)
}

func (s *PGStore) ListEntityRevisions(
	ctx context.Context, kind apimodel.RevisionKind, entityID string, limit int,
) ([]apimodel.Revision, error) {
	// limit <= 0 means no limit. nil makes PG's `limit $3` receive NULL;
	// PG treats `limit NULL` as no limit, so there is no need to build two SQL variants.
	var lim any
	if limit > 0 {
		lim = limit
	}
	rows, err := s.client.Query(ctx,
		`select `+revisionSelectColumns+`
		   from entity_revisions
		  where entity_kind = $1 and entity_id = $2
		  order by revision_no desc
		  limit $3`,
		string(kind), entityID, lim)
	if err != nil {
		return nil, fmt.Errorf("core: 查历史版本失败: %w", err)
	}
	defer rows.Close()

	// **Empty slice, not nil**: for a list in the HTTP contract, nil encodes as null instead of [],
	// and the frontend would need to handle one more shape. Same as the other List methods in this package.
	out := make([]apimodel.Revision, 0)
	for rows.Next() {
		rev, err := scanRevisionRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("core: 遍历历史版本失败: %w", err)
	}
	return out, nil
}

func (s *PGStore) GetEntityRevision(ctx context.Context, id string) (apimodel.Revision, error) {
	rev, err := scanRevisionRow(s.client.QueryRow(ctx,
		`select `+revisionSelectColumnsFull+` from entity_revisions where id = $1`, id))
	if err != nil {
		if pg.IsNoRows(err) {
			return apimodel.Revision{}, persistmodel.ErrNotFound
		}
		return apimodel.Revision{}, err
	}
	return rev, nil
}

func (s *PGStore) DeleteEntityRevisions(
	ctx context.Context, kind apimodel.RevisionKind, entityID string,
) (int64, error) {
	res, err := s.client.Exec(ctx,
		`delete from entity_revisions where entity_kind = $1 and entity_id = $2`,
		string(kind), entityID)
	if err != nil {
		return 0, fmt.Errorf("core: 删历史版本失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("core: 删历史版本失败: %w", err)
	}
	return n, nil
}

// PurgeExcessRevisions keeps only the latest keep rows per entity.
//
// # Why a window function instead of "first query the entity list, then delete one by one"
//
// The latter is N+1 round trips, and **the list changes in between** (other instances are writing new snapshots).
// A window function does the whole thing in one statement under one snapshot isolation: number rows per entity,
// delete those numbered > keep. Several instances running it at once just repeat the same idempotent work.
func (s *PGStore) PurgeExcessRevisions(ctx context.Context, keep int) (int64, error) {
	// ⚠️ keep <= 0 returns right away, **do not go on**: that would make rn > 0 match every row
	// and wipe all history at once. Misconfiguring it to 0 is possible, and "better not delete" is the safe side.
	if keep <= 0 {
		return 0, nil
	}
	res, err := s.client.Exec(ctx,
		`delete from entity_revisions
		  where id in (
		        select id from (
		               select id,
		                      row_number() over (
		                          partition by entity_kind, entity_id
		                          order by revision_no desc
		                      ) as rn
		                 from entity_revisions
		        ) ranked
		         where rn > $1
		  )`, keep)
	if err != nil {
		return 0, fmt.Errorf("core: 清理超额历史版本失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("core: 清理超额历史版本失败: %w", err)
	}
	return n, nil
}

// rowScanner lets scanRevisionRow take both *sql.Row and *sql.Rows.
type rowScanner interface{ Scan(dest ...any) error }

func scanRevisionRow(sc rowScanner) (apimodel.Revision, error) {
	var (
		rev      apimodel.Revision
		kind     string
		reason   string
		snapshot string
	)
	err := sc.Scan(&rev.ID, &rev.WorkspaceID, &kind, &rev.EntityID, &rev.No,
		&snapshot, &rev.Summary, &rev.CreatedAt, &rev.CreatedBy, &reason)
	if err != nil {
		if err == sql.ErrNoRows || pg.IsNoRows(err) {
			return apimodel.Revision{}, err
		}
		return apimodel.Revision{}, fmt.Errorf("core: 读取历史版本失败: %w", err)
	}
	rev.Kind = apimodel.RevisionKind(kind)
	rev.Reason = apimodel.NormalizeRevisionReason(apimodel.RevisionReason(reason))
	// In list queries snapshot is selected as an empty string; then **keep it nil**.
	// Turning it into []byte("") would make json output `"snapshot":""`, which is not a valid JSON value
	// and would blow up the frontend parse. Only nil with omitempty makes the key disappear cleanly.
	if snapshot != "" {
		rev.Snapshot = []byte(snapshot)
	}
	return rev, nil
}

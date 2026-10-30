// PG implementation of the async task "sync the agent to Ark right after it is saved".
//
// ⚠️ The shape of the claim SQL (CTE + `for update skip locked` + repeating the business conditions in the outer query)
// **needs all three**; the reason is the hundred-line postmortem in store_pg_schedule.go. Both pitfalls hit there
// (the EPQ recheck makes `where id in (subquery)` always true, limit punched through by Nested Loop) apply here exactly.
// Do not rewrite it into something that "looks simpler".

package coredao

import (
	"context"
	"database/sql"

	"fmt"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/shared/infra/pg"
)

// syncTaskColumns is all the columns needed to read a sync task; several queries share it to avoid drift.
const syncTaskColumns = `id, workspace_id, agent_id, status, failure_reason, failure_stage, ` +
	`attempt_count, resync_requested, claimed_by, claimed_until, created_at, updated_at, ` +
	`finished_at, ord`

// scanAgentSyncTask reads one row into persistmodel.AgentSyncTaskRow.
//
// claimed_by / claimed_until / finished_at are nullable, so they go through sql.Null*:
// scanning directly into string / time.Time errors on NULL, and NULL is normal in all three columns
// (never claimed, not finished).
func scanAgentSyncTask(scan func(...any) error) (persistmodel.AgentSyncTaskRow, error) {
	var r persistmodel.AgentSyncTaskRow
	var claimedBy sql.NullString
	var claimedUntil, finishedAt sql.NullTime
	if err := scan(&r.ID, &r.WorkspaceID, &r.AgentID, &r.Status,
		&r.FailureReason, &r.FailureStage, &r.AttemptCount, &r.ResyncRequested,
		&claimedBy, &claimedUntil, &r.CreatedAt, &r.UpdatedAt, &finishedAt, &r.Ord); err != nil {
		return persistmodel.AgentSyncTaskRow{}, err
	}
	r.ClaimedBy = claimedBy.String
	r.ClaimedUntil = claimedUntil.Time
	r.FinishedAt = finishedAt.Time
	return r, nil
}

// EnqueueAgentSyncTask: see the notes on core.Store.
//
// # "Clicking twice runs only once" rests entirely on this SQL
//
// `on conflict ... do nothing` collides on `agent_sync_tasks_active_idx`
// (`unique (workspace_id, agent_id) where finished_at is null`).
// **The database decides**, not select-then-insert. The latter is TOCTOU: two instances would each find
// "none" and each insert one, so the same agent would be synced by two tasks at once.
//
// # Why one more UPDATE after a collision
//
// A collision means a round is running or queued, and the agent that round read may be the version from **before this save**.
// Doing nothing would make this change wait until the next conversation to take effect, exactly the symptom this mechanism exists to remove.
// So mark the running one with resync_requested, so it runs one more round after it finishes.
//
// ⚠️ **The pending one needs no mark** (the `status <> 'running'` condition): it has not been claimed yet,
// and will read the latest agent when claimed. Marking it would only make it run a useless second round.
func (s *PGStore) EnqueueAgentSyncTask(
	ctx context.Context, task persistmodel.AgentSyncTaskRow,
) (persistmodel.AgentSyncTaskRow, bool, error) {
	row := s.client.QueryRow(ctx, `
		insert into agent_sync_tasks
		    (id, workspace_id, agent_id, status, created_at, updated_at)
		values ($1, $2, $3, $4, $5, $5)
		on conflict do nothing
		returning `+syncTaskColumns,
		task.ID, task.WorkspaceID, task.AgentID, task.Status, task.CreatedAt)

	created, err := scanAgentSyncTask(row.Scan)
	if err == nil {
		return created, true, nil
	}
	if !pg.IsNoRows(err) {
		return persistmodel.AgentSyncTaskRow{}, false, fmt.Errorf("core: 入队 agent 同步任务失败: %w", err)
	}

	// Collided: an unfinished task already exists. Mark the running one "run another round after finishing", then return it.
	//
	// ⚠️ Use pg.IsNoRows instead of errors.Is(err, pgx.ErrNoRows): s.client wraps
	// database/sql and returns sql.ErrNoRows, so checking the pgx one **never matches**.
	// And this is the path always taken on a double click; a wrong check would report "task already exists" as an enqueue failure.
	existing := s.client.QueryRow(ctx, `
		update agent_sync_tasks
		   set resync_requested = case when status = 'running' then true else resync_requested end,
		       updated_at = $1
		 where workspace_id = $2 and agent_id = $3 and finished_at is null
		returning `+syncTaskColumns,
		task.UpdatedAt, task.WorkspaceID, task.AgentID)

	found, err := scanAgentSyncTask(existing.Scan)
	if pg.IsNoRows(err) {
		// A very narrow window: after the insert collided and before the update, that task just finished.
		// Then nothing was created and nothing is running. **Do not pretend success**; let the caller retry once.
		return persistmodel.AgentSyncTaskRow{}, false, nil
	}
	if err != nil {
		return persistmodel.AgentSyncTaskRow{}, false,
			fmt.Errorf("core: 标记 agent 同步任务需重跑失败: %w", err)
	}
	return found, false, nil
}

// ClaimDueAgentSyncTasks: see the notes on core.Store.
//
// # The union of two branches; the second cannot be dropped
//
//	pending                    nobody has claimed it yet
//	running + lease expired    the instance that claimed it was rebuilt (deploy / scale-down / health check failure),
//	                           so the write-back will never come
//
// Without the second branch those tasks **stay running forever**: the frontend polls until timeout, while the row in the database
// looks "syncing", exactly what the task spec explicitly said to avoid. It is the same thing as the
// "zombie reclaim" on the schedules side.
//
// # All three are required
//
//   - **CTE**: in a data-modifying query a CTE is materialized, so limit really caps the count.
//     Writing limit straight into a subquery makes it rerun row by row as the inner side (measured there: `limit 2` blocked nothing).
//   - **`for update skip locked`**: two concurrent statements get disjoint sets already at row selection.
//   - **Repeat the business conditions in the outer query**: under READ COMMITTED the EPQ recheck only looks at the outer WHERE,
//     and `id in (a precomputed set)` has nothing to do with the row's new content and is **always true**.
func (s *PGStore) ClaimDueAgentSyncTasks(
	ctx context.Context, owner string, now time.Time, leaseFor time.Duration, limit int,
) ([]persistmodel.AgentSyncTaskRow, error) {
	// limit can only go in the CTE: update itself does not support limit.
	limitClause := ""
	if limit > 0 {
		limitClause = fmt.Sprintf(" limit %d", limit)
	}
	query := `
		with due as (
		     select id from agent_sync_tasks
		      where finished_at is null
		        and (
		              status = 'pending'
		              -- Zombie reclaim: the process that claimed it died, so the write-back will never come.
		           or (status = 'running'
		               and claimed_until is not null
		               and claimed_until < $3)
		            )
		      order by created_at, ord` + limitClause + `
		      for update skip locked
		)
		update agent_sync_tasks
		   set status = 'running', claimed_by = $1, claimed_until = $2,
		       attempt_count = agent_sync_tasks.attempt_count + 1,
		       updated_at = $3
		  from due
		 where agent_sync_tasks.id = due.id
		   -- Repeat the business conditions: the EPQ recheck looks here, see the comment above.
		   and agent_sync_tasks.finished_at is null
		   and (
		         agent_sync_tasks.status = 'pending'
		      or (agent_sync_tasks.status = 'running'
		          and agent_sync_tasks.claimed_until is not null
		          and agent_sync_tasks.claimed_until < $3)
		       )
		returning ` + prefixed(syncTaskColumns, "agent_sync_tasks")

	rows, err := s.client.Query(ctx, query, owner, now.Add(leaseFor), now)
	if err != nil {
		return nil, fmt.Errorf("core: 认领 agent 同步任务失败: %w", err)
	}
	defer rows.Close()

	out := make([]persistmodel.AgentSyncTaskRow, 0)
	for rows.Next() {
		r, err := scanAgentSyncTask(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("core: 读取 agent 同步任务认领结果失败: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("core: 读取 agent 同步任务认领结果失败: %w", err)
	}
	return out, nil
}

// FinishAgentSyncTask: see the notes on core.Store.
//
// ⚠️ **finished_at must be written too**: it is the predicate of that partial unique index. Without it this agent
// can never queue a new task again (the unique index keeps blocking), the symptom is "no more syncing on any later save", with zero errors.
//
// Clear the lease as well: a final-state row keeping claimed_until would only mislead debugging into thinking someone is still running it.
func (s *PGStore) FinishAgentSyncTask(
	ctx context.Context, id string, status apimodel.AgentSyncStatus,
	failureReason string, failureStage apimodel.AgentSyncStage, now time.Time,
) error {
	if _, err := s.client.Exec(ctx, `
		update agent_sync_tasks
		   set status = $2, failure_reason = $3, failure_stage = $4,
		       claimed_by = null, claimed_until = null,
		       resync_requested = false,
		       updated_at = $5, finished_at = $5
		 where id = $1`,
		id, string(status), failureReason, string(failureStage), now); err != nil {
		return fmt.Errorf("core: 结算 agent 同步任务失败: %w", err)
	}
	return nil
}

// RequeueAgentSyncTask: see the notes on core.Store.
//
// ⚠️ **The lease must be cleared too**: otherwise the next claim tick is blocked by its own just-set, unexpired lease
// (the running branch of the claim condition requires `claimed_until < now`), wasting one lease period.
// This is the same reason FailPendingMessage of pending_messages clears the lease.
func (s *PGStore) RequeueAgentSyncTask(ctx context.Context, id string, now time.Time) error {
	if _, err := s.client.Exec(ctx, `
		update agent_sync_tasks
		   set status = 'pending', resync_requested = false,
		       claimed_by = null, claimed_until = null,
		       updated_at = $2
		 where id = $1`, id, now); err != nil {
		return fmt.Errorf("core: 重排 agent 同步任务失败: %w", err)
	}
	return nil
}

// LatestAgentSyncTask: see the notes on core.Store.
//
// Takes the first row by created_at desc, ord desc: **the ord half cannot be dropped**. Two tasks queued in the same millisecond
// (deleted and recreated, or double click) cannot be ordered by created_at alone, and PG may order ties arbitrarily,
// which would make the frontend sometimes read the previous, finished task and show "syncing" as "done".
func (s *PGStore) LatestAgentSyncTask(
	ctx context.Context, agentID string,
) (persistmodel.AgentSyncTaskRow, error) {
	row := s.client.QueryRow(ctx, `
		select `+syncTaskColumns+`
		  from agent_sync_tasks
		 where agent_id = $1
		 order by created_at desc, ord desc
		 limit 1`, agentID)

	r, err := scanAgentSyncTask(row.Scan)
	if pg.IsNoRows(err) {
		// This agent was never synced. **This is not an error**; it is a different thing from "sync failed".
		return persistmodel.AgentSyncTaskRow{}, persistmodel.ErrNotFound
	}
	if err != nil {
		return persistmodel.AgentSyncTaskRow{}, fmt.Errorf("core: 查 agent 同步任务失败: %w", err)
	}
	return r, nil
}

// PurgeFinishedAgentSyncTasks: see the notes on core.Store.
func (s *PGStore) PurgeFinishedAgentSyncTasks(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.client.Exec(ctx,
		`delete from agent_sync_tasks where finished_at is not null and finished_at < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("core: 清理已完成的 agent 同步任务失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		// The delete succeeded; the driver just did not give a count. **Do not report it as a delete failure**:
		// the caller would treat it as "cleanup failed" and retry, and the retry would delete nothing either (already deleted).
		return 0, nil
	}
	return n, nil
}

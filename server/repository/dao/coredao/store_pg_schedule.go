// PostgreSQL implementation of scheduled tasks.
//
// A separate file instead of merging into store_pg.go: that one is already close to a thousand lines, and this domain has two things it lacks:
// **time must be compared in the database** (claiming), and the boundary conversion between ISO strings and timestamptz.
// Table structure and reasons are in schema_schedule_pg.sql.
//
// # Timestamps have two forms here
//
// This repo's convention is "timestamps are always opaque ISO strings" (see the top of model/apimodel), because the frontend sorts them
// as strings. But claiming must let **the database be the only clock** (machine clocks differ across instances),
// so next_run_at is a timestamptz column. That makes this file the only place in the repo that converts both ways:
//
//	apimodel.Schedule.NextRunAt   ISO string, the shape the frontend wants, also stored in doc
//	schedules.next_run_at         timestamptz, the claim where condition is computed on it
//
// SaveSchedule writes both together, and **on read the column wins** (see the comment on scanScheduleRow).
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

// isoToTime converts a fixed-length ISO string into time.Time, and an empty string into NULL.
//
// It returns sql.NullTime instead of *time.Time: the driver accepts it directly, saving a nil-pointer check.
// **A parse failure counts as NULL instead of an error**: one broken next_run_at should not make the whole list unreadable,
// and the meaning of NULL (not claimed) happens to be the safe side: the worst case is that this task does not run,
// not that it runs every period or the whole scheduler goes down.
func isoToTime(iso string) sql.NullTime {
	if iso == "" {
		return sql.NullTime{}
	}
	t, err := time.Parse(isoLayoutPG, iso)
	if err != nil {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: t, Valid: true}
}

// isoLayoutPG is a local alias of persistmodel.ISOLayout (the core side uses the same constant).
const isoLayoutPG = persistmodel.ISOLayout

// timeToISO converts the timestamptz in the column back into a fixed-length ISO string.
func timeToISO(t sql.NullTime) string {
	if !t.Valid {
		return ""
	}
	return apimodel.FormatISO(t.Time)
}

// scanScheduleRow builds a Schedule from the three columns "doc, next_run_at, run_now_requested".
//
// **The columns win, not doc.** In theory they agree (SaveSchedule writes them together),
// but claiming updates the columns directly (`set next_run_at = null, run_now_requested = false`),
// and that step does not rewrite doc. Making doc follow would mean reading it, changing it and writing it back, a read-modify-write
// that loses updates under concurrency.
// So the convention: the source of truth for these two fields is the columns; the copy in doc is just serialized along with the entity.
// On read the columns overwrite them, so "after claiming NextRunAt is empty and the try-run mark has been consumed"
// is visible to the caller.
func scanScheduleRow(scan func(dest ...any) error) (apimodel.Schedule, error) {
	var blob []byte
	var nextRunAt sql.NullTime
	var runNow bool
	if err := scan(&blob, &nextRunAt, &runNow); err != nil {
		return apimodel.Schedule{}, err
	}
	var sch apimodel.Schedule
	if err := scanDoc(blob, &sch); err != nil {
		return apimodel.Schedule{}, err
	}
	sch.NextRunAt = timeToISO(nextRunAt)
	sch.RunNowRequested = runNow
	return persistmodel.CloneSchedule(sch), nil
}

// scheduleCols is the column list shared by the three read paths, in the same order as the scan in scanScheduleRow.
//
// It is a constant because **adding a column requires syncing four places** (three selects and one returning).
// Missing one shows as scan reporting "column count mismatch", which is at least loud; worse is getting the order wrong,
// when two columns with compatible types silently swap.
const scheduleCols = `doc, next_run_at, run_now_requested`

func (s *PGStore) ListSchedules(ctx context.Context, workspaceID string) ([]apimodel.Schedule, error) {
	rows, err := s.client.Query(ctx,
		`select `+scheduleCols+` from schedules where workspace_id = $1 order by ord`,
		workspaceID)
	if err != nil {
		return nil, fmt.Errorf("core: 查定时任务失败: %w", err)
	}
	defer rows.Close()

	out := make([]apimodel.Schedule, 0)
	for rows.Next() {
		sch, err := scanScheduleRow(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("core: 读取定时任务失败: %w", err)
		}
		out = append(out, sch)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("core: 遍历定时任务失败: %w", err)
	}
	return out, nil
}

func (s *PGStore) GetSchedule(ctx context.Context, id string) (apimodel.Schedule, error) {
	sch, err := scanScheduleRow(
		s.client.QueryRow(ctx,
			`select `+scheduleCols+` from schedules where id = $1`, id).Scan)
	if err != nil {
		if pg.IsNoRows(err) {
			return apimodel.Schedule{}, persistmodel.ErrNotFound
		}
		return apimodel.Schedule{}, fmt.Errorf("core: 查定时任务失败: %w", err)
	}
	return sch, nil
}

func (s *PGStore) CreateSchedule(ctx context.Context, sch apimodel.Schedule) (apimodel.Schedule, error) {
	blob, err := marshalDoc(sch)
	if err != nil {
		return apimodel.Schedule{}, err
	}
	_, err = s.client.Exec(ctx,
		`insert into schedules
		     (id, workspace_id, agent_id, status, next_run_at, run_now_requested, doc)
		 values ($1, $2, $3, $4, $5, $6, $7)`,
		sch.ID, sch.WorkspaceID, sch.AgentID, string(sch.Status),
		isoToTime(sch.NextRunAt), sch.RunNowRequested, blob)
	if err != nil {
		if pg.IsUniqueViolation(err) {
			// A primary key clash means a duplicate ID was generated, which is a bug and not a business conflict, but it should not be a 500 either;
			// let the caller decide with ErrConflict (same handling as CreateWorkspace).
			return apimodel.Schedule{}, persistmodel.ErrConflict
		}
		return apimodel.Schedule{}, fmt.Errorf("core: 建定时任务失败: %w", err)
	}
	return persistmodel.CloneSchedule(sch), nil
}

// SaveSchedule overwrites.
//
// **ord is not touched**: changing it would move this task to the end of the list, and every run writes back once
// (RunCount / LastRunAt), so the list order would keep reshuffling as tasks run. Same trade-off as SaveRun.
//
// **The two lease columns are not touched either**: they are claim state, and business updates (changing the prompt, enable/disable) must not release a lease
// someone else is holding. The lease is only taken by ClaimDueSchedules and released on expiry.
func (s *PGStore) SaveSchedule(ctx context.Context, sch apimodel.Schedule) (apimodel.Schedule, error) {
	blob, err := marshalDoc(sch)
	if err != nil {
		return apimodel.Schedule{}, err
	}
	res, err := s.client.Exec(ctx,
		`update schedules
		    set workspace_id = $2, agent_id = $3, status = $4, next_run_at = $5,
		        run_now_requested = $6, doc = $7
		  where id = $1`,
		sch.ID, sch.WorkspaceID, sch.AgentID, string(sch.Status),
		isoToTime(sch.NextRunAt), sch.RunNowRequested, blob)
	if err != nil {
		return apimodel.Schedule{}, fmt.Errorf("core: 存定时任务失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return apimodel.Schedule{}, fmt.Errorf("core: 存定时任务失败: %w", err)
	}
	if n == 0 {
		return apimodel.Schedule{}, persistmodel.ErrNotFound
	}
	return persistmodel.CloneSchedule(sch), nil
}

// DeleteSchedule deletes a task; missing is not an error (idempotent).
func (s *PGStore) DeleteSchedule(ctx context.Context, id string) error {
	if _, err := s.client.Exec(ctx, `delete from schedules where id = $1`, id); err != nil {
		return fmt.Errorf("core: 删定时任务失败: %w", err)
	}
	return nil
}

// ClaimDueSchedules atomically claims the tasks that are due.
//
// # The shape of this SQL came from hitting two pitfalls; do not "simplify" it
//
// **Pitfall one: `where id in (subquery)` does not stop concurrent double claims.**
// It was first written with all three filters in the subquery and only `where id in (...)` outside. Measured with 20 tasks
// and 8 concurrent instances, half the tasks were claimed by two instances at once. The cause is PostgreSQL's EvalPlanQual recheck under
// READ COMMITTED: when a row is locked by a concurrent transaction, it waits for the lock to be released, gets the new version and rechecks
// the **outer** WHERE, and the outer one is only `id in (a precomputed set)`, a condition that has nothing to do with the row's new content
// and is **always still true**, so the later instance updates successfully anyway. The claimed_until check in the subquery
// takes no part in the recheck at all.
//
// **Pitfall two: after repeating the conditions in the outer query, limit stopped working.**
// To let the EPQ recheck see the real business conditions, I wrote the three conditions in the outer query too. That did fix pitfall one,
// but `explain` showed the plan became a **Nested Loop Semi Join**: the outer scan drives all 5 rows,
// and the subquery with limit is treated as a repeatable inner side **rerun row by row**, so `limit 2` blocked nothing,
// and all 5 were claimed. (The same SQL in psql used HashAggregate and returned only 2 rows, so
// **verified in psql is not verified**; the two sides can have different plans.)
//
// The current shape uses a CTE to solve both pitfalls:
//
//		with due as (select ... limit N for update skip locked)
//		update schedules set ... from due where schedules.id = due.id and <business conditions repeated>
//
//	  - **The CTE guarantees it is computed once**: PostgreSQL materializes CTEs in queries with data-modifying statements,
//	    so it is not rerun as an inner side, and limit really caps the count.
//	  - **`for update skip locked`** makes two concurrent statements get disjoint sets already at row selection:
//	    rows locked by someone else are skipped instead of waiting for the lock and then walking into that broken recheck.
//	    This is the standard way to build a task queue on PostgreSQL.
//	  - **The outer query still repeats the business conditions**, as a second line of defense: even if the plan shape changes again someday,
//	    the EPQ recheck checks the real business conditions, not an always-true set of ids. This redundancy is deliberate.
//
// Only with these three does "whoever gets the row is the sole holder" really hold.
// The regression tests are ClaimIsAtomicUnderConcurrency and ClaimLimit in TestStoreSuite,
// and **both must run on real PG** (the MemStore run hits neither pitfall).
//
// # Why no explicit transaction is needed
//
// A single statement is a transaction by itself; row locks are held while it runs, and skip locked works within that window.
// Wrapping it in WithTx would only hold the locks longer.
//
// # What each of the three conditions blocks
//
//	status = 'active' or run_now_requested   paused ones do not run, **unless someone clicked try-run**
//	next_run_at <= $now                      not yet due does not run; NULL is naturally excluded
//	claimed_until is null or < $now          do not take one someone else is running with a valid lease
//
// # ⚠️ next_run_at = null is ambiguous: it means both "running" and "stuck"
//
// **The claim itself is `set next_run_at = null`** (see the update below). This is not a typo:
// one fire runs for ten-odd minutes, and without clearing it every scan tick would claim it again. After clearing,
// **the write-back after finishing** schedules the next time again (finish / rescheduleOnly).
//
// So the same "next_run_at is null + status = active" has two completely different meanings in the database:
//
//	running   just claimed, the lease (claimed_until) has not expired, the write-back is coming soon
//	stuck     the process that claimed it died (deploy/OOM/panic), **the write-back will never come**
//
// next_run_at alone cannot tell them apart; **you have to check whether claimed_until has expired**. That is exactly
// why this kind of failure is hard to notice: a sweep task stopped for nearly a day while the page showed
// "next run: not scheduled", which looked like the user had not configured it, but it was really the second case.
//
// # Why the zombie reclaim branch cannot be dropped
//
// This SQL first had only the "normally due" branch, so `next_run_at is not null` **excluded the second case above
// up front**: the lease expiry check (`claimed_until < $now`) looked like zombie reclaim
// but **could never be reached**; the record was already filtered out by the previous condition. The result was a self-lock:
//
//	claim → next_run_at cleared → process dies → nobody writes back → never meets "not null" → can never be claimed
//
// The symptom is that the task **never runs again**, with zero errors, zero logs, status still active,
// consecutiveFailures still 0 (the auto-pause-after-consecutive-failures gate does not fire either, because it did not "fail";
// it just never ran). One deploy interrupting a running turn is enough to create such a zombie.
//
// So it is now **the union of two branches**:
//
//	normally due     next_run_at is not null and next_run_at <= $now and the lease is not held by someone else
//	zombie reclaim   next_run_at is null and claimed_by is not null and claimed_until < $now
//
// **`claimed_by is not null` cannot be dropped**: among records with `next_run_at is null` there is also a kind that
// "was never claimed and never scheduled" (for example the in-between state after paused, where run-now set the mark but no time has been computed yet);
// missing this condition would claim those by mistake too.
//
// **Both branches must be written once each in the CTE and in the outer update**, for the same reason as "pitfall one": the EPQ recheck looks at the outer query.
//
// # Why "or try-run was clicked" cannot be dropped, nor loosened to looking only at next_run_at
//
// With only `status = 'active'`, calling run-now on a paused task returns 200 and sets
// next_run_at to now, **and the task is never claimed**: the symptom is "clicked try-run, nothing happened",
// with zero errors, and the nextRunAt on that row in the frontend even updated, looking exactly like success.
//
// Conversely, if the status condition were deleted entirely to save effort (only checking due time), **every task would run once
// the moment it is paused**: the next_run_at left over from before pausing is still in the past. So "clicked by a person" must have an explicit mark;
// see the run_now_requested section in schema_schedule_pg.sql.
//
// Claiming also clears this mark (**one-time**), so a try-run buys only one round. After running, finish sees
// status is still paused and does not schedule the next one, so a try-run does not turn a paused task into a periodic one.
//
// # Why now is a parameter and not now()
//
// In production time.Now() is passed, equivalent to now(). It is a parameter so **tests can pass a fixed time**:
// everything this path needs to assert (claim only when due, do not take an unexpired lease) is a function of time, and waiting on real
// time with sleep is slow and flaky. Same approach as "time passed in as a parameter" on the domain side.
func (s *PGStore) ClaimDueSchedules(
	ctx context.Context, owner string, now time.Time, leaseFor time.Duration, limit int,
) ([]apimodel.Schedule, error) {
	// limit can only go in the CTE: update itself does not support limit.
	limitClause := ""
	if limit > 0 {
		limitClause = fmt.Sprintf(" limit %d", limit)
	}
	query := `
		with due as (
		     select id from schedules
		      where (status = 'active' or run_now_requested)
		        and (
		              -- Normally due.
		              (next_run_at is not null
		               and next_run_at <= $3
		               and (claimed_until is null or claimed_until < $3))
		              -- Zombie reclaim: the process that claimed it died, so the write-back will never come. See the comment above.
		           or (next_run_at is null
		               and claimed_by is not null
		               and claimed_until is not null
		               and claimed_until < $3)
		            )
		      -- Zombies with a null next_run_at sort first (nulls first is not the default for asc,
		      -- so write it out): they are already a full lease late and should run before the ones just due.
		      order by next_run_at asc nulls first, ord` + limitClause + `
		      for update skip locked
		)
		update schedules
		   set claimed_by = $1, claimed_until = $2, next_run_at = null,
		       -- The run-now flag is **one-shot** and is consumed when claimed: if not cleared, this paused task
		       -- would be claimed again every time someone sets next_run_at back.
		       run_now_requested = false
		  from due
		 where schedules.id = due.id
		   -- Repeat the business conditions: the EPQ recheck looks here, see "pitfall one" above.
		   and (schedules.status = 'active' or schedules.run_now_requested)
		   and (
		         (schedules.next_run_at is not null
		          and schedules.next_run_at <= $3
		          and (schedules.claimed_until is null or schedules.claimed_until < $3))
		      or (schedules.next_run_at is null
		          and schedules.claimed_by is not null
		          and schedules.claimed_until is not null
		          and schedules.claimed_until < $3)
		       )
		returning schedules.doc, schedules.next_run_at, schedules.run_now_requested`

	rows, err := s.client.Query(ctx, query, owner, now.Add(leaseFor), now)
	if err != nil {
		return nil, fmt.Errorf("core: 认领定时任务失败: %w", err)
	}
	defer rows.Close()

	out := make([]apimodel.Schedule, 0)
	for rows.Next() {
		// The next_run_at returned is already NULL (just cleared by the same statement),
		// so scanScheduleRow overwrites NextRunAt with an empty string, which is exactly the state the caller should see.
		sch, err := scanScheduleRow(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("core: 读取认领结果失败: %w", err)
		}
		out = append(out, sch)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("core: 遍历认领结果失败: %w", err)
	}
	return out, nil
}

// ReleaseScheduleClaim clears the claim lease; see the "must be called after a round finishes" part of the interface comment.
func (s *PGStore) ReleaseScheduleClaim(ctx context.Context, id string) error {
	if _, err := s.client.Exec(ctx,
		`update schedules set claimed_by = null, claimed_until = null where id = $1`,
		id); err != nil {
		return fmt.Errorf("core: 释放定时任务租约失败: %w", err)
	}
	return nil
}

func (s *PGStore) SchedulesForAgent(ctx context.Context, agentID string) ([]string, error) {
	rows, err := s.client.Query(ctx,
		`select id from schedules where agent_id = $1 order by ord`, agentID)
	if err != nil {
		return nil, fmt.Errorf("core: 查 agent 的定时任务失败: %w", err)
	}
	defer rows.Close()

	out := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("core: 读取定时任务 ID 失败: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("core: 遍历定时任务 ID 失败: %w", err)
	}
	return out, nil
}

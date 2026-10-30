// PG implementation of queued messages waiting to be sent.
//
// ⚠️ This group handles **messages the user queues on purpose** (they know the agent is still running and will wait a whole turn), not
// "automatic resend after hitting the tool-execution window" (a race of tens of milliseconds, retried in place by
// sendEventsThroughWindow in core/chat.go). Do not merge the two; see the note on core/store.go.

package coredao

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/shared/infra/pg"
)

// pendingColumns is all the columns needed to read a queued message; both queries share it to avoid drift.
const pendingColumns = `id, run_id, body, status, source, actor, attempts, last_error, created_at`

// scanPendingMessage reads one row into apimodel.PendingMessage.
//
// created_at is timestamptz (for database sorting); it is converted to a fixed-length ISO string for the contract when read.
// The two are two shapes of the same instant; see apimodel.PendingMessage.QueuedAt.
func scanPendingMessage(scan func(...any) error) (apimodel.PendingMessage, error) {
	var m apimodel.PendingMessage
	var createdAt time.Time
	if err := scan(&m.ID, &m.RunID, &m.Text, &m.Status, &m.Source,
		&m.Actor, &m.Attempts, &m.LastError, &createdAt); err != nil {
		return apimodel.PendingMessage{}, err
	}
	m.Status = apimodel.NormalizePendingStatus(m.Status)
	m.Source = apimodel.NormalizePendingSource(m.Source)
	m.QueuedAt = apimodel.FormatISO(createdAt)
	return m, nil
}

func (s *PGStore) EnqueuePendingMessage(
	ctx context.Context, m apimodel.PendingMessage, createdAt time.Time,
) error {
	// workspace_id is taken from runs with a subquery; **the caller does not pass it**.
	//
	// It is purely derived data (a queued message's workspace always equals its run's workspace),
	// and letting the caller pass it adds one more thing to get wrong. Getting it wrong does not error; the message is just missed when "listing by workspace"
	// and not cleaned up when the workspace is deleted, both with zero errors.
	//
	// The subquery is in the same statement, so no extra round trip. If the run does not exist the insert writes NULL,
	// and that column is not null, so it fails right away, which is what we want (queuing into a conversation that does not exist
	// is a programming error and should not succeed silently).
	_, err := s.client.Exec(ctx, `
		insert into pending_messages
		    (id, run_id, workspace_id, body, status, source, actor, created_at)
		values ($1, $2, (select workspace_id from runs where id = $2),
		        $3, $4, $5, $6, $7)`,
		m.ID, m.RunID, m.Text,
		string(apimodel.NormalizePendingStatus(m.Status)),
		string(apimodel.NormalizePendingSource(m.Source)),
		m.Actor, createdAt)
	if err != nil {
		return fmt.Errorf("core: 排队消息入队失败: %w", err)
	}
	return nil
}

func (s *PGStore) ListPendingMessages(
	ctx context.Context, runID string,
) ([]apimodel.PendingMessage, error) {
	rows, err := s.client.Query(ctx,
		`select `+pendingColumns+` from pending_messages
		  where run_id = $1
		  order by created_at, ord`, runID)
	if err != nil {
		return nil, fmt.Errorf("core: 列排队消息失败: %w", err)
	}
	defer rows.Close()

	out := make([]apimodel.PendingMessage, 0)
	for rows.Next() {
		m, err := scanPendingMessage(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("core: 读排队消息失败: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("core: 遍历排队消息失败: %w", err)
	}
	return out, nil
}

// ClaimNextPendingMessage atomically claims the first pending message of a conversation. See the notes on the Store interface.
//
// # The shape is copied from ClaimDueSchedules; do not "simplify" it
//
// Both pitfalls from there apply here exactly (postmortem in the long comment in store_pg_schedule.go):
//
//   - **`where id in (subquery)` does not stop concurrent double claims.** Under READ COMMITTED, the EvalPlanQual
//     recheck only looks at the outer WHERE, and `id in (a precomputed set)` has nothing to do with the row's new content and is always true.
//     So the outer query must **repeat** the business conditions.
//   - **`for update skip locked`** makes two concurrent statements get disjoint sets already at row selection.
//
// It matters even more here than there: a double claim means **the same message is sent twice**, and the agent answers it twice.
// That is an error the user sees directly, unlike a duplicate subscription that only wastes a connection.
//
// limit 1 is inside the CTE (update itself does not support limit), and a CTE in a data-modifying query is
// materialized, so it really caps the count. The other place hit the pitfall of "limit punched through by Nested Loop".
func (s *PGStore) ClaimNextPendingMessage(
	ctx context.Context, runID, owner string, now time.Time, leaseFor time.Duration,
) (apimodel.PendingMessage, bool, error) {
	row := s.client.QueryRow(ctx, `
		with due as (
		     select id from pending_messages
		      where run_id = $1
		        and status = 'pending'
		        and (claimed_until is null or claimed_until < $4)
		      order by created_at, ord
		      limit 1
		      for update skip locked
		)
		update pending_messages
		   set status = 'sending', claimed_by = $2, claimed_until = $3,
		       attempts = pending_messages.attempts + 1
		  from due
		 where pending_messages.id = due.id
		   -- Repeat the business conditions: the EPQ recheck looks here, see the comment above.
		   and pending_messages.status = 'pending'
		   and (pending_messages.claimed_until is null or pending_messages.claimed_until < $4)
		returning `+prefixed(pendingColumns, "pending_messages")+``,
		runID, owner, now.Add(leaseFor), now)

	m, err := scanPendingMessage(row.Scan)
	if pg.IsNoRows(err) {
		// Nothing pending. **Not an error**: the queue is empty at the end of the vast majority of turns.
		//
		// ⚠️ Use pg.IsNoRows instead of errors.Is(err, pgx.ErrNoRows): s.client
		// wraps database/sql and returns sql.ErrNoRows, so **checking pgx.ErrNoRows never matches**.
		// And this is the most common path (the queue is empty in most turns), so a wrong check is not an occasional problem:
		// every turn end would report "no pending message" upward as a claim failure.
		return apimodel.PendingMessage{}, false, nil
	}
	if err != nil {
		return apimodel.PendingMessage{}, false, fmt.Errorf("core: 认领排队消息失败: %w", err)
	}
	return m, true, nil
}

func (s *PGStore) DeletePendingMessage(ctx context.Context, id string) error {
	if _, err := s.client.Exec(ctx,
		`delete from pending_messages where id = $1`, id); err != nil {
		return fmt.Errorf("core: 删排队消息失败: %w", err)
	}
	return nil
}

// FailPendingMessage marks a queued message as unsendable. See the notes on the Store interface.
//
// **Clear the lease**: otherwise this failed record still holds claimed_until,
// and the "resend now" path has to claim it again.
func (s *PGStore) FailPendingMessage(ctx context.Context, id, reason string) error {
	if _, err := s.client.Exec(ctx, `
		update pending_messages
		   set status = 'failed', last_error = $2,
		       claimed_by = null, claimed_until = null
		 where id = $1`, id, reason); err != nil {
		return fmt.Errorf("core: 标记排队消息失败: %w", err)
	}
	return nil
}

// prefixed adds the table name prefix to every column in the column list.
//
// `returning` inside `update ... from` must qualify the table name, otherwise columns like id / status are
// **ambiguous** between the update target and the CTE (PG errors out). Two hand-written column lists would drift,
// so both are generated from the same one.
func prefixed(columns, table string) string {
	parts := strings.Split(columns, ", ")
	for i, c := range parts {
		parts[i] = table + "." + c
	}
	return strings.Join(parts, ", ")
}

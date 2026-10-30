// PG implementation of the one-time entry ticket for the terminal WebSocket.
//
// The table structure and the full reasoning for "why this ticket is needed and why it must be in the database" are in
// the file header of repository/schema/terminal_tickets.sql. This file only covers implementation trade-offs.
package coredao

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/shared/infra/pg"
)

// IssueTerminalTicket: see the notes on core.Store.
//
// No `on conflict`: the ticket value is 32 bytes from crypto/rand, so a primary key clash is negligible,
// and if one really happens it should **error rather than silently overwrite**; overwriting would void someone else's unused ticket.
func (s *PGStore) IssueTerminalTicket(ctx context.Context, row persistmodel.TerminalTicketRow) error {
	const q = `insert into terminal_tickets (ticket, run_id, actor, expires_at, created_at)
	           values ($1, $2, $3, $4, $5)`
	if _, err := s.client.Exec(ctx, q,
		row.Ticket, row.RunID, row.Actor, row.ExpiresAt, row.CreatedAt); err != nil {
		return fmt.Errorf("core: 签发终端入场票失败: %w", err)
	}
	return nil
}

// RedeemTerminalTicket: see the notes on core.Store.
//
// # The "one-time" property rests entirely on this one SQL
//
// The three conditions `where ticket = $1 and used_at is null and expires_at > $2` and
// `set used_at = $2` are in **the same UPDATE**, so there is no window at all between "check" and
// "mark as used". PG serializes two concurrent UPDATEs on the same row; the later one re-evaluates the where,
// finds `used_at` already non-NULL, matches zero rows, and returns ErrNoRows.
//
// ⚠️ **Writing it as "select to check, then update" breaks it**: two instances can each select
// "not used yet" and then each update successfully, opening two terminals with one ticket. And this bug
// **never reproduces** in single-instance, single-request tests; it needs two concurrent requests hitting the same ticket.
// The check is the concurrency test in store_terminalticket_test.go (N goroutines race for one ticket,
// asserting exactly one wins).
//
// # Expiry also goes through the same where, instead of reading back and comparing time in Go
//
// Comparing in Go uses **this instance's clock**, and clocks drift between instances
// (NTP only guarantees about millisecond level). Letting the database be the only clock is an existing rule in this repo.
//
// # Three failures merged into ErrNotFound
//
// Ticket missing, already used, expired: the caller handles them exactly the same (return 401, do not open the connection),
// and telling them apart would tell an attacker "this ticket exists, it just expired". So it is always ErrNotFound.
func (s *PGStore) RedeemTerminalTicket(
	ctx context.Context, ticket string, now time.Time,
) (persistmodel.TerminalTicketRow, error) {
	const q = `update terminal_tickets
	              set used_at = $2
	            where ticket = $1 and used_at is null and expires_at > $2
	        returning ticket, run_id, actor, expires_at, used_at, created_at`
	var r persistmodel.TerminalTicketRow
	var usedAt sql.NullTime
	err := s.client.QueryRow(ctx, q, ticket, now).Scan(
		&r.Ticket, &r.RunID, &r.Actor, &r.ExpiresAt, &usedAt, &r.CreatedAt)
	if pg.IsNoRows(err) {
		// Missing / already used / expired, not told apart. See the section above.
		return persistmodel.TerminalTicketRow{}, persistmodel.ErrNotFound
	}
	if err != nil {
		return persistmodel.TerminalTicketRow{}, fmt.Errorf("core: 核销终端入场票失败: %w", err)
	}
	r.UsedAt = usedAt.Time
	return r, nil
}

// PurgeExpiredTerminalTickets: see the notes on core.Store.
//
// Delete by expires_at, not created_at: the expiry time is what decides "is this row still useful".
func (s *PGStore) PurgeExpiredTerminalTickets(
	ctx context.Context, before time.Time,
) (int64, error) {
	const q = `delete from terminal_tickets where expires_at < $1`
	res, err := s.client.Exec(ctx, q, before)
	if err != nil {
		return 0, fmt.Errorf("core: 清理过期终端入场票失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		// The delete succeeded; the driver just did not give a count. **Do not report it as a cleanup failure**:
		// same as the comment on PurgeFinishedAgentSyncTasks, the caller would retry because of it,
		// and retrying a delete that already succeeded is wasted work. Returning 0 means "unknown how many were deleted".
		return 0, nil
	}
	return n, nil
}

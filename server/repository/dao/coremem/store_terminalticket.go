// In-memory implementation of the one-time entry ticket for the terminal WebSocket.
//
// ⚠️ **This one must match the coredao version exactly in behavior**, especially the three redeem conditions:
// "ticket exists", "not used yet", "not expired" are all required, and the three failures are merged into one ErrNotFound.
// If the two drift apart nothing errors; the contract suite just passes falsely on one side. This repo's rule is that
// the same test cases run **once each** against MemStore and PGStore.
//
// The table structure and the full reasoning for "why this ticket is needed" are in
// the file header of repository/schema/terminal_tickets.sql.
package coremem

import (
	"context"
	"time"

	"github.com/chyroc/ama/server/model/persistmodel"
)

// IssueTerminalTicket: see the notes on core.Store.
func (s *MemStore) IssueTerminalTicket(
	_ context.Context, row persistmodel.TerminalTicketRow,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.terminalTickets[row.Ticket] = row
	return nil
}

// RedeemTerminalTicket: see the notes on core.Store.
//
// # "One-time" is guaranteed by a mutex here, and by a single UPDATE on the PG side
//
// Two mechanisms, same behavior: there must be no window between the check and the mark. Here the whole
// read-modify-write is inside `s.mu.Lock()`, so concurrent calls are serialized; when the second one gets in, `UsedAt` is already non-zero.
//
// ⚠️ **Do not split it into "read, unlock, check, lock again, write back"**: that opens a window,
// and MemStore is the default implementation for e2e and unit tests, so that bug would make the
// "a ticket can be used only once" assertion pass falsely in tests, then break in a different way on PG.
//
// The three failures are merged into ErrNotFound for exactly the same reason as on the PG side (callers handle them the same way,
// and telling them apart would tell an attacker "the ticket exists, it just expired").
func (s *MemStore) RedeemTerminalTicket(
	_ context.Context, ticket string, now time.Time,
) (persistmodel.TerminalTicketRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	row, ok := s.data.terminalTickets[ticket]
	switch {
	case !ok:
		// Ticket does not exist.
		return persistmodel.TerminalTicketRow{}, persistmodel.ErrNotFound
	case !row.UsedAt.IsZero():
		// Already redeemed. **This check is what makes it "one-time"**.
		return persistmodel.TerminalTicketRow{}, persistmodel.ErrNotFound
	case !row.ExpiresAt.After(now):
		// Expired. Use !After instead of Before so "exactly at the deadline" also counts as expired,
		// strictly matching `expires_at > $2` on the PG side (there too, "equal does not count").
		return persistmodel.TerminalTicketRow{}, persistmodel.ErrNotFound
	}

	row.UsedAt = now
	s.data.terminalTickets[ticket] = row
	return row, nil
}

// PurgeExpiredTerminalTickets: see the notes on core.Store.
func (s *MemStore) PurgeExpiredTerminalTickets(
	_ context.Context, before time.Time,
) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var n int64
	for k, v := range s.data.terminalTickets {
		if v.ExpiresAt.Before(before) {
			delete(s.data.terminalTickets, k)
			n++
		}
	}
	return n, nil
}

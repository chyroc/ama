// PostgreSQL implementation of ProgressStore.
//
// # Why the lease must be in the SQL where
//
// Almost every statement in this file has `lease_token = $n` in its where. That is not defensive code;
// it is **the only source of correctness**.
//
// Counterexample: SELECT first, compare the token in Go, then UPDATE. Between the two steps the lease may expire
// or be taken by someone else, so the expired holder overwrites the new holder's progress. The symptom is events in the CoT message
// out of order or going backwards, with no error. Putting the comparison in the UPDATE's where lets the database make it atomic.
//
// Likewise ClaimProgressDelivery uses one UPDATE ... RETURNING to do "check whether it can be claimed + claim",
// not "read then change".

package feishudao

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/chyroc/ama/server/feishu"
	"strings"
	"time"
)

// deliveryColumns is all the columns needed to read back a delivery, in the same order as scanDelivery.
const deliveryColumns = `
	run_id, lark_app_id, chat_id, origin_message_id, reply_in_thread,
	transport, status, coalesce(status_reason, ''),
	coalesce(remote_cot_id, ''), coalesce(remote_message_id, ''),
	desired_revision, applied_revision, applied_snapshot,
	terminal_requested, terminal_applied,
	coalesce(lease_token, ''), lease_until,
	create_attempted_at,
	attempt_count, consecutive_failure_count, next_retry_at, coalesce(last_error, ''),
	created_at, updated_at, finished_at`

// rowScanner lets rows from QueryRow and Query share one decoder.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanDelivery(row rowScanner) (*feishu.ProgressDelivery, error) {
	var d feishu.ProgressDelivery
	var snapshotRaw []byte
	var leaseUntil, createAttemptedAt, nextRetryAt, finishedAt sql.NullTime
	if err := row.Scan(
		&d.RunID, &d.LarkAppID, &d.ChatID, &d.OriginMessageID, &d.ReplyInThread,
		&d.Transport, &d.Status, &d.StatusReason,
		&d.RemoteCOTID, &d.RemoteMessageID,
		&d.DesiredRevision, &d.AppliedRevision, &snapshotRaw,
		&d.TerminalRequested, &d.TerminalApplied,
		&d.LeaseToken, &leaseUntil,
		&createAttemptedAt,
		&d.AttemptCount, &d.ConsecutiveFailureCount, &nextRetryAt, &d.LastError,
		&d.CreatedAt, &d.UpdatedAt, &finishedAt,
	); err != nil {
		return nil, err
	}
	snapshot, err := snapshotFromJSON(snapshotRaw)
	if err != nil {
		return nil, fmt.Errorf("解析进度快照失败: %w", err)
	}
	d.AppliedSnapshot = snapshot
	d.LeaseUntil = nullTime(leaseUntil)
	d.CreateAttemptedAt = nullTime(createAttemptedAt)
	d.NextRetryAt = nullTime(nextRetryAt)
	d.FinishedAt = nullTime(finishedAt)
	return &d, nil
}

func nullTime(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

// Compile-time assertion: PGStore must implement both Store and ProgressStore.
//
// **This assertion blocks a silent failure**: cmd/amad uses a type assertion to get ProgressStore from Store
// (see progressStoreOf). If the assertion fails it gets nil, so Feishu groups in production get **no chain of thought**,
// while every code path completes "normally": no error, no panic, the thinking process just vanishes.
// Missing one method falls into it, so let the compiler watch.
var (
	_ feishu.Store         = (*PGStore)(nil)
	_ feishu.ProgressStore = (*PGStore)(nil)
)

// EnsureProgressDelivery creates a delivery record, or returns the existing one if it exists.
//
// on conflict do nothing instead of "read then insert": when two messages of the same run come in concurrently,
// the later must not overwrite progress the earlier has already pushed halfway.
func (s *PGStore) EnsureProgressDelivery(ctx context.Context, d *feishu.ProgressDelivery) (*feishu.ProgressDelivery, error) {
	if d == nil || strings.TrimSpace(d.RunID) == "" {
		return nil, feishu.ErrInvalidArgument
	}
	now := time.Now()
	if _, err := s.client.Exec(ctx, `
		insert into feishu_progress_deliveries (
			run_id, lark_app_id, chat_id, origin_message_id, reply_in_thread,
			transport, status, desired_revision, applied_revision,
			created_at, updated_at
		) values ($1, $2, $3, $4, $5, $6, $7, 1, 0, $8, $8)
		on conflict (run_id) do nothing
	`, d.RunID, d.LarkAppID, d.ChatID, d.OriginMessageID, d.ReplyInThread,
		d.Transport, feishu.ProgressQueued, now,
	); err != nil {
		return nil, fmt.Errorf("建进度投递失败: %w", err)
	}
	return s.GetProgressDelivery(ctx, d.RunID)
}

// GetProgressDelivery gets by runID.
func (s *PGStore) GetProgressDelivery(ctx context.Context, runID string) (*feishu.ProgressDelivery, error) {
	row := s.client.QueryRow(ctx,
		`select `+deliveryColumns+` from feishu_progress_deliveries where run_id = $1`,
		strings.TrimSpace(runID))
	d, err := scanDelivery(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, feishu.ErrProgressNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("读进度投递失败: %w", err)
	}
	return d, nil
}

// ClaimProgressDelivery claims the lease of a delivery.
//
// One UPDATE does "check whether it can be claimed + claim". Three cases where it can be claimed:
//
//	nobody holds the lease (lease_until is null)
//	the lease has expired (lease_until < now)
//	it is already ours (reentry, e.g. crashed last round without releasing and came back)
//
// Plus the next_retry_at gate: do not claim before the retry time, to avoid spinning right after a failure.
func (s *PGStore) ClaimProgressDelivery(
	ctx context.Context, runID, token string, ttl time.Duration,
) (*feishu.ProgressDelivery, bool, error) {
	runID, token = strings.TrimSpace(runID), strings.TrimSpace(token)
	if runID == "" || token == "" {
		return nil, false, feishu.ErrInvalidArgument
	}
	if ttl <= 0 {
		ttl = feishu.ProgressLeaseTTL
	}
	now := time.Now()
	row := s.client.QueryRow(ctx, `
		update feishu_progress_deliveries set
			status = case when status = $4 then $4 else $5 end,
			lease_token = $2,
			lease_until = $3,
			attempt_count = attempt_count + 1,
			updated_at = $6
		where run_id = $1
		  and status in ($4, $5, $7)
		  and (lease_until is null or lease_until < $6 or lease_token = $2)
		  and (next_retry_at is null or next_retry_at <= $6)
		returning `+deliveryColumns,
		runID, token, now.Add(ttl), feishu.ProgressActive, feishu.ProgressProcessing, now, feishu.ProgressQueued,
	)
	d, err := scanDelivery(row)
	if errors.Is(err, sql.ErrNoRows) {
		// Not getting it is not an error: it may be held by someone else, not yet at the retry time, or already in a final state.
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("认领进度投递失败: %w", err)
	}
	return d, true, nil
}

// RenewProgressLease renews the lease. Returns false if the lease is no longer ours.
func (s *PGStore) RenewProgressLease(
	ctx context.Context, runID, token string, ttl time.Duration,
) (bool, error) {
	runID, token = strings.TrimSpace(runID), strings.TrimSpace(token)
	if runID == "" || token == "" {
		return false, feishu.ErrInvalidArgument
	}
	if ttl <= 0 {
		ttl = feishu.ProgressLeaseTTL
	}
	now := time.Now()
	res, err := s.client.Exec(ctx, `
		update feishu_progress_deliveries
		set lease_until = $3, updated_at = $4
		where run_id = $1 and lease_token = $2
	`, runID, token, now.Add(ttl), now)
	if err != nil {
		return false, fmt.Errorf("续租进度投递失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		// When it cannot be determined, **treat it as renewed**: treating it as not renewed would make this instance stop on its own,
		// and nobody may take over that CoT, leaving the user with a message that spins forever.
		return true, nil
	}
	return n > 0, nil
}

// SaveProgressDelivery writes a delivery back. Requires holding the lease.
//
// On entering a final state it also sets finished_at and clears the lease; keeping the lease would make the cleanup task think someone is still handling it.
func (s *PGStore) SaveProgressDelivery(ctx context.Context, d *feishu.ProgressDelivery, token string) error {
	if d == nil || strings.TrimSpace(d.RunID) == "" {
		return feishu.ErrInvalidArgument
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return feishu.ErrInvalidArgument
	}
	snapshot, err := snapshotJSON(d.AppliedSnapshot)
	if err != nil {
		return fmt.Errorf("序列化进度快照失败: %w", err)
	}
	now := time.Now()

	finished := d.FinishedAt
	if d.Finished() && finished == nil {
		finished = &now
	}
	// Clear the lease on final states, keep it otherwise.
	leaseToken, leaseUntil := any(token), any(d.LeaseUntil)
	if d.Finished() {
		leaseToken, leaseUntil = nil, nil
	}

	res, err := s.client.Exec(ctx, `
		update feishu_progress_deliveries set
			transport = $3,
			status = $4,
			status_reason = nullif($5, ''),
			remote_cot_id = nullif($6, ''),
			remote_message_id = nullif($7, ''),
			desired_revision = $8,
			applied_revision = $9,
			applied_snapshot = $10,
			terminal_requested = $11,
			terminal_applied = $12,
			create_attempted_at = $13,
			consecutive_failure_count = $14,
			next_retry_at = $15,
			last_error = nullif($16, ''),
			lease_token = $17,
			lease_until = $18,
			finished_at = $19,
			updated_at = $20
		where run_id = $1 and lease_token = $2
	`, d.RunID, token,
		d.Transport, d.Status, d.StatusReason,
		d.RemoteCOTID, d.RemoteMessageID,
		d.DesiredRevision, d.AppliedRevision, snapshot,
		d.TerminalRequested, d.TerminalApplied,
		d.CreateAttemptedAt,
		d.ConsecutiveFailureCount, d.NextRetryAt, d.LastError,
		leaseToken, leaseUntil, finished, now,
	)
	if err != nil {
		return fmt.Errorf("写回进度投递失败: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		// No row updated = the lease is no longer ours. See the comment on ErrProgressLeaseLost.
		return feishu.ErrProgressLeaseLost
	}
	return nil
}

// ReleaseProgressLease gives up the lease without changing the status, so someone else can take over at once.
//
// Pass a ctx that will not be canceled: it is usually called on the shutdown path, and with a canceled ctx it cannot release,
// leaving others to wait for the lease to expire.
func (s *PGStore) ReleaseProgressLease(ctx context.Context, runID, token string) error {
	runID, token = strings.TrimSpace(runID), strings.TrimSpace(token)
	if runID == "" || token == "" {
		return feishu.ErrInvalidArgument
	}
	if _, err := s.client.Exec(ctx, `
		update feishu_progress_deliveries
		set lease_token = null, lease_until = null, updated_at = $3
		where run_id = $1 and lease_token = $2
	`, runID, token, time.Now()); err != nil {
		return fmt.Errorf("释放进度投递租约失败: %w", err)
	}
	return nil
}

// ListClaimableProgressDeliveries lists deliveries that can be claimed, final states first.
//
// Final states first because the user is waiting for that last bit: a CoT pushed for a long time that never gets closed
// keeps the client showing "thinking", which is more frustrating than one that started later.
func (s *PGStore) ListClaimableProgressDeliveries(ctx context.Context, limit int) ([]*feishu.ProgressDelivery, error) {
	if limit <= 0 {
		limit = 32
	}
	now := time.Now()
	rows, err := s.client.Query(ctx, `
		select `+deliveryColumns+` from feishu_progress_deliveries
		where status in ($1, $2, $3)
		  and (lease_until is null or lease_until < $4)
		  and (next_retry_at is null or next_retry_at <= $4)
		order by terminal_requested desc, next_retry_at nulls first, created_at
		limit $5
	`, feishu.ProgressQueued, feishu.ProgressProcessing, feishu.ProgressActive, now, limit)
	if err != nil {
		return nil, fmt.Errorf("列可认领的进度投递失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]*feishu.ProgressDelivery, 0, limit)
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, fmt.Errorf("读进度投递失败: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("列可认领的进度投递失败: %w", err)
	}
	return out, nil
}

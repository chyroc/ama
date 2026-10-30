// PostgreSQL implementation of the three host executor tables: hosts / workspace_hosts /
// host_dispatches.
//
// A separate file instead of stuffing it into store_pg.go: that file is already over three thousand lines, and this domain has two concerns
// found nowhere else: **the reissue-token decision of the registration upsert**, and **serial seq assignment for the downstream queue,
// skip locked claiming and NOTIFY wakeups**. The table structures and all design reasons are in the file headers of
// repository/schema/{hosts,workspace_hosts,host_dispatches}.sql.
//
// Style matches store_pg_wsintegration.go case by case: column lists as constants, $N placeholders,
// fmt.Errorf("core: ...: %w"), sql.ErrNoRows → persistmodel.ErrNotFound,
// lists always return a non-nil empty slice.
package coredao

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/service/host"
)

// hostColumns is the read column list of the hosts table, matching the Scan arguments of scanHost one to one.
const hostColumns = `id, owner_email, name, os, arch, host_uuid, token_hash, status,
	        daemon_version, gnu_tools_ok, toolcheck, native_tools, created_at, last_seen_at,
	        last_dispatch_seq`

// workspaceHostColumns is the read column list of the workspace_hosts table.
const workspaceHostColumns = `id, workspace_id, host_id, added_by, created_at`

// hostDispatchColumns is the read column list of the host_dispatches table, in the same order as scanDispatch.
const hostDispatchColumns = `id, host_id, seq, kind, state, session_id, terminal_id,
	        payload, result, created_at, delivered_at, claimed_until, updated_at`

// scanHost scans one hosts row. **last_seen_at is nullable**: scanned with sql.NullTime, Invalid becomes the
// zero time.Time. Nullable timestamptz in row types always follows the convention zero value = NULL
// (see the comment on persistmodel.Host.LastSeenAt).
func scanHost(scan func(dest ...any) error) (persistmodel.Host, error) {
	var h persistmodel.Host
	var lastSeen sql.NullTime
	if err := scan(&h.ID, &h.OwnerEmail, &h.Name, &h.OS, &h.Arch, &h.HostUUID,
		&h.TokenHash, &h.Status, &h.DaemonVersion, &h.GNUToolsOK, &h.Toolcheck,
		&h.NativeTools, &h.CreatedAt, &lastSeen, &h.LastDispatchSeq); err != nil {
		return persistmodel.Host{}, err
	}
	if lastSeen.Valid {
		h.LastSeenAt = lastSeen.Time
	}
	return h, nil
}

func scanWorkspaceHost(scan func(dest ...any) error) (persistmodel.WorkspaceHost, error) {
	var b persistmodel.WorkspaceHost
	if err := scan(&b.ID, &b.WorkspaceID, &b.HostID, &b.AddedBy, &b.CreatedAt); err != nil {
		return persistmodel.WorkspaceHost{}, err
	}
	return b, nil
}

// scanDispatch scans one host_dispatches row. delivered_at / claimed_until are nullable;
// the zero time.Time stands for NULL (see the comments on those two fields of persistmodel.HostDispatch).
func scanDispatch(scan func(dest ...any) error) (persistmodel.HostDispatch, error) {
	var d persistmodel.HostDispatch
	var deliveredAt, claimedUntil sql.NullTime
	if err := scan(&d.ID, &d.HostID, &d.Seq, &d.Kind, &d.State, &d.SessionID,
		&d.TerminalID, &d.Payload, &d.Result, &d.CreatedAt,
		&deliveredAt, &claimedUntil, &d.UpdatedAt); err != nil {
		return persistmodel.HostDispatch{}, err
	}
	if deliveredAt.Valid {
		d.DeliveredAt = deliveredAt.Time
	}
	if claimedUntil.Valid {
		d.ClaimedUntil = claimedUntil.Time
	}
	return d, nil
}

// UpsertRegisteredHost upserts by (owner_email, host_uuid); see the host.Store contract.
//
// # One insert ... on conflict ... returning does it all, no select first
//
// The unique key (owner_email, host_uuid) lets the database decide concurrent registrations (TOCTOU): when two processes
// register the same machine at once, one of them always takes the conflict branch, and there are never two rows.
//
// On conflict only the registration snapshot columns are refreshed (name/os/arch/daemon_version/gnu_tools_ok/
// toolcheck), **id / token_hash / created_at / status / last_seen_at are not touched**:
// the first three are the existing identity, and the last two may only be maintained by heartbeats. A repeated registration must not push an online machine back to offline,
// or wipe its latest heartbeat time. The hosts table has no updated_at; the snapshot columns are "what was reported at the last registration".
//
// # How inserted is decided: check whether the returned token_hash equals the argument
//
// A new row's token_hash is the non-empty hash the Service generated this time, so it always equals the argument exactly; a conflicting row
// keeps the old hash (the token is a random sha256; two being equal is negligible), so they differ. This is
// more intuitive than tricks like `xmax = 0`: xmax has ambiguous meaning on different paths (triggers, locks), while comparing hashes
// is obvious from reading the code.
func (s *PGStore) UpsertRegisteredHost(
	ctx context.Context, row persistmodel.Host,
) (persistmodel.Host, bool, error) {
	saved, err := scanHost(s.client.QueryRow(ctx,
		`insert into hosts
		   (id, owner_email, name, os, arch, host_uuid, token_hash, status,
		    daemon_version, gnu_tools_ok, toolcheck, native_tools, created_at, last_seen_at)
		 values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		 on conflict (owner_email, host_uuid) do update
		   set name = excluded.name,
		       os = excluded.os,
		       arch = excluded.arch,
		       daemon_version = excluded.daemon_version,
		       gnu_tools_ok = excluded.gnu_tools_ok,
		       toolcheck = excluded.toolcheck,
		       native_tools = excluded.native_tools
		 returning `+hostColumns,
		row.ID, row.OwnerEmail, row.Name, row.OS, row.Arch, row.HostUUID,
		row.TokenHash, row.Status, row.DaemonVersion, row.GNUToolsOK, row.Toolcheck,
		row.NativeTools, row.CreatedAt, nullTime(row.LastSeenAt)).Scan)
	if err != nil {
		return persistmodel.Host{}, false, fmt.Errorf("core: 注册宿主机失败: %w", err)
	}
	return saved, saved.TokenHash == row.TokenHash, nil
}

func (s *PGStore) GetHost(ctx context.Context, id string) (persistmodel.Host, error) {
	h, err := scanHost(s.client.QueryRow(ctx,
		`select `+hostColumns+` from hosts where id = $1`, id).Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return persistmodel.Host{}, persistmodel.ErrNotFound
		}
		return persistmodel.Host{}, fmt.Errorf("core: 取宿主机失败: %w", err)
	}
	return h, nil
}

func (s *PGStore) GetHostByOwnerUUID(
	ctx context.Context, ownerEmail, hostUUID string,
) (persistmodel.Host, error) {
	h, err := scanHost(s.client.QueryRow(ctx,
		`select `+hostColumns+` from hosts
		  where owner_email = $1 and host_uuid = $2`, ownerEmail, hostUUID).Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return persistmodel.Host{}, persistmodel.ErrNotFound
		}
		return persistmodel.Host{}, fmt.Errorf("core: 按属主与 uuid 取宿主机失败: %w", err)
	}
	return h, nil
}

// GetHostByTokenHash deliberately includes `token_hash <> ”`: an empty string means "not issued yet", and must never let an
// empty Authorization match a machine row.
func (s *PGStore) GetHostByTokenHash(
	ctx context.Context, tokenHash string,
) (persistmodel.Host, error) {
	h, err := scanHost(s.client.QueryRow(ctx,
		`select `+hostColumns+` from hosts
		  where token_hash = $1 and token_hash <> ''`, tokenHash).Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return persistmodel.Host{}, persistmodel.ErrNotFound
		}
		return persistmodel.Host{}, fmt.Errorf("core: 按 token 取宿主机失败: %w", err)
	}
	return h, nil
}

// TouchHost is the heartbeat: it refreshes last_seen_at and the online status, and refreshes the host snapshot **when needed**.
//
// String snapshot fields (name/os/arch/daemon_version) are refreshed only when non-empty: the daemon sends a heartbeat every tick
// and omits unchanged values, so an empty string must keep the old value in the database; gnu_tools_ok / toolcheck are pointers,
// and nil means this tick did not include them. The SQL decides with case on the spot, avoiding the read-modify-write of "read then write back".
//
// ⚠️ **$8/$9 must be cast explicitly with ::boolean / ::text**: when the parameter is nil PG cannot infer the type from an untyped
// NULL literal and reports 42P08 `could not determine data type of parameter $8`
// (measured). With the cast, nil → SQL NULL and non-nil works normally; both sides are right.
// A missing machine (0 rows) returns ErrNotFound.
func (s *PGStore) TouchHost(
	ctx context.Context, id string, snap host.HostHeartbeatSnapshot,
	onlineStatus string, now time.Time,
) error {
	res, err := s.client.Exec(ctx,
		`update hosts
		    set status = $2,
		        last_seen_at = $3,
		        name = case when $4 <> '' then $4 else name end,
		        os = case when $5 <> '' then $5 else os end,
		        arch = case when $6 <> '' then $6 else arch end,
		        daemon_version = case when $7 <> '' then $7 else daemon_version end,
		        gnu_tools_ok = case when $8::boolean is not null then $8
		                            else gnu_tools_ok end,
		        toolcheck = case when $9::text is not null then $9
		                         else toolcheck end,
		        native_tools = case when $10::boolean is not null then $10
		                            else native_tools end
		  where id = $1`,
		id, onlineStatus, now,
		snap.Name, snap.OS, snap.Arch, snap.DaemonVersion,
		snap.GNUToolsOK, snap.Toolcheck, snap.NativeTools)
	if err != nil {
		return fmt.Errorf("core: 宿主机心跳失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return persistmodel.ErrNotFound
	}
	return nil
}

func (s *PGStore) ListHostsByOwner(
	ctx context.Context, ownerEmail string,
) ([]persistmodel.Host, error) {
	rows, err := s.client.Query(ctx,
		`select `+hostColumns+` from hosts
		  where owner_email = $1 order by created_at, id`, ownerEmail)
	if err != nil {
		return nil, fmt.Errorf("core: 列宿主机失败: %w", err)
	}
	defer rows.Close()
	out := make([]persistmodel.Host, 0, 4)
	for rows.Next() {
		h, err := scanHost(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("core: 读取宿主机失败: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("core: 遍历宿主机失败: %w", err)
	}
	return out, nil
}

// DeleteHost is idempotent: workspace_hosts bindings are deleted along by the foreign key on delete cascade;
// host_dispatches has no foreign key (old commands kept for debugging); neither is touched here.
func (s *PGStore) DeleteHost(ctx context.Context, id string) error {
	if _, err := s.client.Exec(ctx, `delete from hosts where id = $1`, id); err != nil {
		return fmt.Errorf("core: 删宿主机失败: %w", err)
	}
	return nil
}

func (s *PGStore) ListHostsLastSeenBefore(
	ctx context.Context, before time.Time, limit int,
) ([]persistmodel.Host, error) {
	// last_seen_at is null must be included explicitly: rows that never sent a heartbeat after registering should also be seen by the offline sweep,
	// and in SQL a NULL in a `<` comparison is always unknown, so where would silently exclude it.
	query := `select ` + hostColumns + ` from hosts
	  where last_seen_at is null or last_seen_at < $1
	  order by created_at, id`
	args := []any{before}
	if limit > 0 {
		query += ` limit $2`
		args = append(args, limit)
	}
	rows, err := s.client.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("core: 列待巡检宿主机失败: %w", err)
	}
	defer rows.Close()
	out := make([]persistmodel.Host, 0, 4)
	for rows.Next() {
		h, err := scanHost(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("core: 读取宿主机失败: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("core: 遍历宿主机失败: %w", err)
	}
	return out, nil
}

func (s *PGStore) MarkHostOffline(
	ctx context.Context, id string, seenBefore, _ time.Time,
) (bool, error) {
	// CAS: matches only if online and the heartbeat time is still before the sweep's threshold at the time. The second condition blocks
	// "heartbeat recovered right after listing and before marking": the heartbeat refreshed last_seen_at, this mark misses,
	// and the next round's ListHostsLastSeenBefore will not list it either. NULL (never sent a heartbeat) naturally fails
	// `<`; such rows will not have status online either, so both sides agree.
	// last_seen_at is not written: the heartbeat time is the evidence for the offline decision.
	res, err := s.client.Exec(ctx,
		`update hosts
		    set status = 'offline'
		  where id = $1 and status = 'online' and last_seen_at < $2`,
		id, seenBefore)
	if err != nil {
		return false, fmt.Errorf("core: 置宿主机离线失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("core: 取宿主机离线更新行数失败: %w", err)
	}
	return n > 0, nil
}

// -- workspace_hosts --------------------------------------------------------

// BindWorkspaceHost inserts idempotently. **Foreign key errors are not translated**: when host_id does not exist, 23503 is wrapped as is
// in "core: ..." and passed up, and the service/host layer turns it into an actionable error (contract in host.Store).
func (s *PGStore) BindWorkspaceHost(ctx context.Context, b persistmodel.WorkspaceHost) error {
	if _, err := s.client.Exec(ctx,
		`insert into workspace_hosts (id, workspace_id, host_id, added_by, created_at)
		 values ($1, $2, $3, $4, $5)
		 on conflict (workspace_id, host_id) do nothing`,
		b.ID, b.WorkspaceID, b.HostID, b.AddedBy, b.CreatedAt); err != nil {
		return fmt.Errorf("core: 绑定宿主机到空间失败: %w", err)
	}
	return nil
}

func (s *PGStore) ListWorkspaceHosts(
	ctx context.Context, workspaceID string,
) ([]persistmodel.BoundHost, error) {
	// Inner join: bindings whose host is gone are not returned (with the foreign key cascade they do not exist anyway).
	// Sort by the binding table's (created_at, id), not hosts': the contract says binding order.
	// Binding columns (wh.id/added_by/created_at) come after the host columns, in the same order as scanBoundHost.
	rows, err := s.client.Query(ctx,
		`select h.id, h.owner_email, h.name, h.os, h.arch, h.host_uuid,
		        h.token_hash, h.status, h.daemon_version, h.gnu_tools_ok, h.toolcheck,
		        h.native_tools, h.created_at, h.last_seen_at, h.last_dispatch_seq,
		        wh.id, wh.added_by, wh.created_at
		   from hosts h
		   join workspace_hosts wh on wh.host_id = h.id
		  where wh.workspace_id = $1
		  order by wh.created_at, wh.id`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("core: 列空间的宿主机绑定失败: %w", err)
	}
	defer rows.Close()
	out := make([]persistmodel.BoundHost, 0, 4)
	for rows.Next() {
		b, err := scanBoundHost(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("core: 读取绑定宿主机失败: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("core: 遍历绑定宿主机失败: %w", err)
	}
	return out, nil
}

// scanBoundHost scans the combined row "host row + three binding columns"; see ListWorkspaceHosts for the column order.
func scanBoundHost(scan func(dest ...any) error) (persistmodel.BoundHost, error) {
	var b persistmodel.BoundHost
	var lastSeen sql.NullTime
	if err := scan(&b.Host.ID, &b.Host.OwnerEmail, &b.Host.Name, &b.Host.OS, &b.Host.Arch,
		&b.Host.HostUUID, &b.Host.TokenHash, &b.Host.Status, &b.Host.DaemonVersion,
		&b.Host.GNUToolsOK, &b.Host.Toolcheck, &b.Host.NativeTools, &b.Host.CreatedAt,
		&lastSeen, &b.Host.LastDispatchSeq, &b.BindingID, &b.AddedBy, &b.BoundAt); err != nil {
		return persistmodel.BoundHost{}, err
	}
	if lastSeen.Valid {
		b.Host.LastSeenAt = lastSeen.Time
	}
	return b, nil
}

func (s *PGStore) GetWorkspaceHost(
	ctx context.Context, workspaceID, hostID string,
) (persistmodel.WorkspaceHost, error) {
	b, err := scanWorkspaceHost(s.client.QueryRow(ctx,
		`select `+workspaceHostColumns+` from workspace_hosts
		  where workspace_id = $1 and host_id = $2`, workspaceID, hostID).Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return persistmodel.WorkspaceHost{}, persistmodel.ErrNotFound
		}
		return persistmodel.WorkspaceHost{}, fmt.Errorf("core: 取空间宿主机绑定失败: %w", err)
	}
	return b, nil
}

func (s *PGStore) UnbindWorkspaceHost(
	ctx context.Context, workspaceID, hostID string,
) error {
	if _, err := s.client.Exec(ctx,
		`delete from workspace_hosts where workspace_id = $1 and host_id = $2`,
		workspaceID, hostID); err != nil {
		return fmt.Errorf("core: 解绑宿主机失败: %w", err)
	}
	return nil
}

// -- host_dispatches --------------------------------------------------------

// hostWakeChannel is the channel name for NOTIFY on enqueue/complete; it must match the host_dispatches.sql file header and
// the long-poll consumer exactly. It is a bare string, with no compiler watching renames.
const hostWakeChannel = "ama_host_wake"

// hostSeqLockSalt gives a namespace to the transaction-level advisory lock that serializes seq assignment per host, to avoid hash
// collisions with other advisory locks in the repo (if any are added later).
const hostSeqLockSalt int64 = 882911337

// EnqueueDispatch does "advisory lock → assign seq → insert → write back counter →
// NOTIFY" in one transaction.
//
// # Why seq is serialized with pg_advisory_xact_lock and not select ... for update
//
// The `select coalesce(max(seq),0)+1 ... for update` envisioned in the task spec does not work in PG:
// aggregate queries do not allow FOR UPDATE (`FOR UPDATE is not allowed with aggregate
// functions`). Instead it first takes a **transaction-level advisory lock** on the host_id text (the key is derived with
// hashtextextended; all enqueues for the same host are serialized, different hosts do not block each other), and taking a number inside the lock
// is an uncontended read. The unique constraint (host_id, seq) is still the last line of defense.
//
// The advisory lock is xact-level: released on commit, never leaked even if the instance crashes (only session-level locks have leak problems).
//
// # Why seq cannot just take max(seq) of this table
//
// done rows get wiped by PurgeOldTermDispatches / PurgeDoneExecDispatches, after which
// max(seq) is NULL and the next one starts from 1. The daemon's persisted last_seq is a large number,
// so `seq > last_seq` never holds, and new commands are silently never fetched. The counter lives in hosts.last_dispatch_seq
// (separate from the row lifecycle). Taking a number still uses greatest with the existing max of this table: during a rolling deploy old code
// (numbering only from this table) may already have inserted a larger number before the counter caught up; taking the max of both
// guarantees old and new instances running concurrently do not clash on the unique constraint, and the counter catches up on the next enqueue.
func (s *PGStore) EnqueueDispatch(
	ctx context.Context, d persistmodel.HostDispatch,
) (int64, error) {
	var seq int64
	err := s.client.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`select pg_advisory_xact_lock(hashtextextended($1, $2))`,
			d.HostID, hostSeqLockSalt); err != nil {
			return fmt.Errorf("core: 锁宿主机指令序号失败: %w", err)
		}
		if err := tx.QueryRowContext(ctx,
			`select greatest(
			          coalesce((select max(seq) from host_dispatches where host_id = $1), 0),
			          coalesce((select last_dispatch_seq from hosts where id = $1), 0)
			        ) + 1`,
			d.HostID).Scan(&seq); err != nil {
			return fmt.Errorf("core: 分配宿主机指令序号失败: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`insert into host_dispatches
			   (id, host_id, seq, kind, state, session_id, terminal_id,
			    payload, result, created_at, updated_at)
			 values ($1, $2, $3, $4, 'queued', $5, $6, $7, '', $8, $8)`,
			d.ID, d.HostID, seq, d.Kind, d.SessionID, d.TerminalID, d.Payload,
			d.CreatedAt); err != nil {
			return fmt.Errorf("core: 宿主机指令入队失败: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`update hosts set last_dispatch_seq = $2 where id = $1`,
			d.HostID, seq); err != nil {
			return fmt.Errorf("core: 回写宿主机指令序号失败: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`select pg_notify($1, $2)`, hostWakeChannel, d.HostID); err != nil {
			return fmt.Errorf("core: 唤醒宿主机长轮询失败: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return seq, nil
}

// ClaimPendingDispatches claims atomically with one SQL, its shape copied from the CTE of ClaimRunWatch /
// ClaimDueSchedules (the long comments in those two files record two concurrency pitfalls):
//
//   - the picked subquery uses `for update skip locked`: two concurrent claimers get disjoint
//     row sets, and order by seq + limit set the order and cap at this level;
//   - the outer WHERE of upd **repeats** the business conditions: the EPQ recheck under READ COMMITTED only looks at
//     the outer query, and writing only `id in picked` is always true after the row is changed concurrently, failing to stop double claims;
//   - finally select ... from upd order by seq: **UPDATE ... RETURNING does not support
//     ORDER BY** (PG syntax, measured 42601), so the order can only be ensured by this outermost select.
//
// The two branches match the two kinds of pending delivery in the contract: new queued rows after the cursor, and
// delivered rows whose lease expired (redelivery after a daemon crash / a lost result POST).
func (s *PGStore) ClaimPendingDispatches(
	ctx context.Context, hostID string, afterSeq int64, now time.Time, limit int,
) ([]persistmodel.HostDispatch, error) {
	// limit must be a positive integer: passing 0 for $4 means "take 0 rows" in SQL, not "no limit". When limit<=0
	// means no limit, the clause is simply not added (copied from ClaimDueSchedules; a positive integer has no injection risk).
	limitClause := ""
	if limit > 0 {
		limitClause = fmt.Sprintf(" limit %d", limit)
	}
	query := `
		with picked as (
		    select id
		      from host_dispatches
		     where host_id = $1
		       and ( (state = 'queued' and seq > $2)
		          or (state = 'delivered' and claimed_until < $3) )
		     order by seq` + limitClause + `
		     for update skip locked
		),
		upd as (
		    update host_dispatches
		       set state = 'delivered',
		           delivered_at = $3,
		           claimed_until = $3 + case when kind in ('exec', 'tool')
		                                    then interval '150 seconds'
		                                    else interval '30 seconds' end,
		           updated_at = $3
		      from picked
		     where host_dispatches.id = picked.id
		       and ( (state = 'queued' and seq > $2)
		          or (state = 'delivered' and claimed_until < $3) )
		    returning host_dispatches.id, host_dispatches.host_id,
		              host_dispatches.seq, host_dispatches.kind, host_dispatches.state,
		              host_dispatches.session_id, host_dispatches.terminal_id,
		              host_dispatches.payload, host_dispatches.result,
		              host_dispatches.created_at, host_dispatches.delivered_at,
		              host_dispatches.claimed_until, host_dispatches.updated_at
		)
		select id, host_id, seq, kind, state, session_id, terminal_id,
		       payload, result, created_at, delivered_at, claimed_until, updated_at
		  from upd
		 order by seq`
	rows, err := s.client.Query(ctx, query, hostID, afterSeq, now)
	if err != nil {
		return nil, fmt.Errorf("core: 认领宿主机指令失败: %w", err)
	}
	defer rows.Close()
	out := make([]persistmodel.HostDispatch, 0, 4)
	for rows.Next() {
		d, err := scanDispatch(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("core: 读取宿主机指令失败: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("core: 遍历宿主机指令失败: %w", err)
	}
	return out, nil
}

// CompleteDispatch completes with CAS. Only state<>'done' rows are updated, so repeated reports do not overwrite result;
// NOTIFY comes after the CTE in the same statement and commits with the implicit transaction of the update,
// so there is no in-between state of "result stored but no wakeup".
func (s *PGStore) CompleteDispatch(
	ctx context.Context, hostID, id, resultJSON string, now time.Time,
) error {
	if _, err := s.client.Exec(ctx, `
		with upd as (
		    update host_dispatches
		       set state = 'done', result = $3, updated_at = $4
		     where id = $2 and host_id = $1 and state <> 'done'
		    returning host_id
		)
		select pg_notify($5, host_id) from upd`,
		hostID, id, resultJSON, now, hostWakeChannel); err != nil {
		return fmt.Errorf("core: 完成宿主机指令失败: %w", err)
	}
	return nil
}

// AckDispatches sets the term_* rows matching (host_id, seq) to done in bulk.
//
// One UPDATE + expanded placeholders: seqs are the ack cursors carried in one result POST, single digits in number,
// far below the 65535 parameter limit; = any($1) is not used for the same reason as GetFiles in store_pg_file.go
// (database/sql does not turn []int64 into a PG array).
//
// Only term_* and unfinished rows are accepted: completing an exec must go through CompleteDispatch (it stores result),
// and state<>'done' makes repeated acks idempotent. No NOTIFY: ack goes daemon → amad,
// and nothing on the amad side waits for wakeups on these rows.
func (s *PGStore) AckDispatches(
	ctx context.Context, hostID string, seqs []int64, now time.Time,
) error {
	if len(seqs) == 0 {
		return nil
	}
	placeholders := make([]string, 0, len(seqs))
	args := make([]any, 0, len(seqs)+2)
	args = append(args, hostID)
	for _, seq := range seqs {
		args = append(args, seq)
		placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
	}
	args = append(args, now)
	query := `update host_dispatches
	    set state = 'done', updated_at = $` + fmt.Sprintf("%d", len(args)) + `
	  where host_id = $1
	    and kind in ('term_open', 'term_input', 'term_resize', 'term_close')
	    and state <> 'done'
	    and seq in (` + strings.Join(placeholders, ",") + `)`
	if _, err := s.client.Exec(ctx, query, args...); err != nil {
		return fmt.Errorf("core: 批量确认终端帧失败: %w", err)
	}
	return nil
}

func (s *PGStore) GetDispatch(ctx context.Context, id string) (persistmodel.HostDispatch, error) {
	d, err := scanDispatch(s.client.QueryRow(ctx,
		`select `+hostDispatchColumns+` from host_dispatches where id = $1`, id).Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return persistmodel.HostDispatch{}, persistmodel.ErrNotFound
		}
		return persistmodel.HostDispatch{}, fmt.Errorf("core: 取宿主机指令失败: %w", err)
	}
	return d, nil
}

// FailPendingExecsForHost first sets rows to done in bulk inside a transaction, then decides whether to NOTIFY by the affected row count:
// when nothing changed (the host has no in-flight exec) no empty wakeup is sent.
func (s *PGStore) FailPendingExecsForHost(
	ctx context.Context, hostID, resultJSON string, now time.Time,
) (int64, error) {
	var n int64
	err := s.client.WithTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`update host_dispatches
			    set state = 'done', result = $2, updated_at = $3
			  where host_id = $1
			    and kind in ('exec', 'tool')
			    and state in ('queued', 'delivered')`,
			hostID, resultJSON, now)
		if err != nil {
			return fmt.Errorf("core: 置失败宿主机 exec/tool 指令失败: %w", err)
		}
		n, _ = res.RowsAffected()
		if n > 0 {
			if _, err := tx.ExecContext(ctx,
				`select pg_notify($1, $2)`, hostWakeChannel, hostID); err != nil {
				return fmt.Errorf("core: 唤醒宿主机长轮询失败: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (s *PGStore) PurgeOldTermDispatches(
	ctx context.Context, before time.Time,
) (int64, error) {
	// List the four term_* explicitly instead of `like 'term_%'`: underscore is a single-character wildcard in LIKE,
	// and an allowlist will not wrongly delete future non-exec commands that might need to be kept longer.
	res, err := s.client.Exec(ctx,
		`delete from host_dispatches
		  where state = 'done'
		    and kind in ('term_open', 'term_input', 'term_resize', 'term_close')
		    and created_at < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("core: 清理旧终端帧失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// PurgeDoneExecDispatches deletes finished exec rows with created_at older than before.
//
// ⚠️ An exec row's payload (ExecSpec, including injected credential env) and result (stdout/stderr sent back by the daemon,
// which may contain plaintext secrets printed by commands) are both in this table and must not live forever. The retention period
// only covers the "redelivery after daemon crash + watcher debugging" window, and rows are deleted on expiry. Deletion only accepts state=done
// rows past the retention period; unfinished rows are left for the offline sweep to settle.
func (s *PGStore) PurgeDoneExecDispatches(
	ctx context.Context, before time.Time,
) (int64, error) {
	res, err := s.client.Exec(ctx,
		`delete from host_dispatches
		  where state = 'done'
		    and kind = 'exec'
		    and created_at < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("core: 清理旧 exec 调用失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// TerminalHostID returns the host of the latest terminal frame for a terminalID, used to check ownership when the daemon reports
// terminal output frames (machine A's token must not write frames into machine B's browser terminal).
// Records only exist within the retention window of term_* rows (10 minutes); missing returns ErrNotFound.
// Terminal frames are best-effort; if ownership cannot be found the frame is dropped, without affecting exec.
func (s *PGStore) TerminalHostID(ctx context.Context, terminalID string) (string, error) {
	var hostID string
	err := s.client.QueryRow(ctx,
		`select host_id from host_dispatches
		  where terminal_id = $1
		    and kind like 'term_%'
		  order by seq desc limit 1`, terminalID).Scan(&hostID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", persistmodel.ErrNotFound
		}
		return "", fmt.Errorf("core: 取终端归属主机失败: %w", err)
	}
	return hostID, nil
}

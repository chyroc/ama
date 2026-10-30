// Persistence contract of the host executor domain: machine registration, workspace binding, and the downstream command queue.
//
// # The declarations follow the domain, not left in core/store.go
//
// The reasons are exactly the same as for the earlier domains (workspace / issue / suggestion / revision / schedule):
// core/store.go is the shared file every resource domain must pass through, and a dozen concurrent sessions touch it. Piling one domain's
// method declarations there costs not "a big file" but **conflict surface**. Now the declarations live here,
// and core/store.go only embeds one line, `hostsvc.Store`.
//
// # Why the three groups of methods are in one interface
//
// They describe three tables of the same thing, "the host executor" (hosts / workspace_hosts /
// host_dispatches; the DDL and all design reasons are in the file headers of the same-named files under repository/schema/):
// registration auth reads hosts, picking a machine for a workspace joins the first two, and sending commands writes the third. Splitting them into three packages would require
// packages to import each other (zero imports between domain packages is a hard rule), or splitting the chain "look up host → check binding → enqueue"
// out to the callers, and missing one step of that chain is not a compile error; it is an incident where commands are sent to an unbound machine.
package host

import (
	"context"
	"time"

	"github.com/chyroc/ama/server/model/persistmodel"
)

// Store is all the persistence methods this domain needs, **only the ones it needs**.
//
// The two implementations (coredao PGStore / coremem MemStore) must match in behavior method by method; the contract cases
// run against both in coredao's TestStoreSuite.
type Store interface {
	// -- hosts (machine registration and heartbeat) --------------------------------------------

	// UpsertRegisteredHost upserts one machine row by (owner_email, host_uuid).
	// **The token is generated in the Service layer**: the plaintext token is returned to the daemon once there, and its sha256 goes into
	// row.TokenHash passed in here; the storage layer never touches the plaintext.
	//
	//   - New row: inserted as is (id / token_hash / created_at all from the input),
	//     inserted=true;
	//   - Conflict (the same machine registers again): only refresh the registration snapshot columns
	//     name/os/arch/daemon_version/gnu_tools_ok/toolcheck, **keeping the existing
	//     id, token_hash, created_at, status, last_seen_at** (online status and heartbeat time
	//     can only be maintained by heartbeats; re-registering should not flip an online machine back to offline), inserted=false.
	//
	// The returned saved is the whole row after storing; Service uses inserted to decide whether to put **this time's** plaintext
	// token into the response; when false it returns "machine already registered", with no token.
	UpsertRegisteredHost(ctx context.Context, row persistmodel.Host) (saved persistmodel.Host, inserted bool, err error)
	// GetHost gets a machine by id; returns ErrNotFound if it does not exist.
	GetHost(ctx context.Context, id string) (persistmodel.Host, error)
	// GetHostByOwnerUUID gets by (owner, machine uuid); returns ErrNotFound if it does not exist.
	// Both registration idempotency and "is this machine yours" go through it.
	GetHostByOwnerUUID(ctx context.Context, ownerEmail, hostUUID string) (persistmodel.Host, error)
	// GetHostByTokenHash gets a machine by token hash, used for auth of daemon heartbeat/result long polling;
	// returns ErrNotFound if it does not exist (= invalid token or the machine was revoked).
	GetHostByTokenHash(ctx context.Context, tokenHash string) (persistmodel.Host, error)
	// TouchHost is the heartbeat: last_seen_at=now, status=onlineStatus, and refreshes the host snapshot from the non-empty/non-nil fields
	// in snapshot (daemon version, GNU probe, os/arch/name;
	// empty fields untouched; the daemon sends every beat and leaves out values that did not change). Returns ErrNotFound if the machine does not exist.
	TouchHost(ctx context.Context, id string, snapshot HostHeartbeatSnapshot, onlineStatus string, now time.Time) error
	// ListHostsByOwner lists all machines of an owner, **in ascending (created_at, id) order**.
	ListHostsByOwner(ctx context.Context, ownerEmail string) ([]persistmodel.Host, error)
	// DeleteHost deletes a machine (revoking the token); not existing is not an error (idempotent). workspace_hosts
	// bindings are deleted by foreign key cascade.
	DeleteHost(ctx context.Context, id string) error
	// ListHostsLastSeenBefore lists machines whose last_seen_at is before before (**including NULL: rows that never sent a heartbeat
	// after registering**), in ascending (created_at, id) order; limit<=0 means no limit;
	// used by the offline sweep to scan machines that "may be offline".
	ListHostsLastSeenBefore(ctx context.Context, before time.Time, limit int) ([]persistmodel.Host, error)
	// MarkHostOffline is a CAS: sets offline only when status='online' now **and last_seen_at is still before
	// seenBefore**, and returns whether it matched. Offline sweeps on several instances run concurrently; this where clause
	// makes sure only one instance reports "offline"; the second condition blocks the race "the host resumed heartbeats right after listing
	// and before marking" (the heartbeat refreshed last_seen_at, so this mark must miss, and the host is not listed next round either).
	// It does not touch last_seen_at: that is the heartbeat time, and the offline decision should not rewrite the evidence.
	MarkHostOffline(ctx context.Context, id string, seenBefore, now time.Time) (bool, error)

	// -- workspace_hosts (machines bound into workspaces) ------------------------------------

	// BindWorkspaceHost adds a workspace binding, on conflict (workspace_id, host_id)
	// do nothing: idempotent; a repeated bind returns nil. If host_id does not exist in hosts the foreign key errors,
	// and **the storage layer does not translate it** (wrapped as "core: ..." as is); the Service layer turns it into an actionable
	// user-facing error: the same database may have other foreign key failures, and the translation rules should be written only once.
	BindWorkspaceHost(ctx context.Context, row persistmodel.WorkspaceHost) error
	// ListWorkspaceHosts lists the machines bound to a workspace (join hosts, returning host row + binding row:
	// who registered it, who bound it, when it was bound), **in ascending workspace_hosts.(created_at, id)
	// order**. Rows whose host was deleted do not appear (inner join + foreign key cascade; both implementations agree).
	ListWorkspaceHosts(ctx context.Context, workspaceID string) ([]persistmodel.BoundHost, error)
	// GetWorkspaceHost gets one binding itself (for audit/existence checks); ErrNotFound if it does not exist.
	GetWorkspaceHost(ctx context.Context, workspaceID, hostID string) (persistmodel.WorkspaceHost, error)
	// UnbindWorkspaceHost unbinds; not existing is not an error (idempotent).
	UnbindWorkspaceHost(ctx context.Context, workspaceID, hostID string) error

	// -- host_dispatches (amad → daemon downstream queue) -------------------------

	// EnqueueDispatch enqueues one command: in a transaction it assigns a serial seq for that host (max(seq)+1
	// within the same host), hard-codes state 'queued', and pg_notify('ama_host_wake', host_id)
	// to wake long polling. Returns the assigned seq. A lost notification is not fatal (the daemon still has heartbeats as fallback).
	EnqueueDispatch(ctx context.Context, d persistmodel.HostDispatch) (int64, error)
	// ClaimPendingDispatches atomically claims up to limit (<=0 no limit) commands waiting for delivery, in one
	// SQL statement: (state='queued' and seq>afterSeq) or (state='delivered' and
	// claimed_until<now, i.e. redelivery after lease expiry), in ascending seq order, for update skip locked,
	// updated to delivered, delivered_at=now, claimed_until=now+lease (exec 150s,
	// others 30s), updated_at=now, and returns the whole updated rows.
	//
	// afterSeq is the daemon's persistent cursor: queued rows within the cursor are no longer delivered. Redelivered rows
	// may have a seq below the cursor but must still be returned; the daemon side deduplicates idempotently by call id.
	ClaimPendingDispatches(ctx context.Context, hostID string, afterSeq int64, now time.Time, limit int) ([]persistmodel.HostDispatch, error)
	// CompleteDispatch is a CAS completion: only when state<>'done' it writes state='done' and
	// result, and pg_notify; if already done (repeated report) it returns nil and **does not overwrite result**.
	// Both id and host_id are conditions, so a mixed-up host cannot report on someone else's command.
	CompleteDispatch(ctx context.Context, hostID, id, resultJSON string, now time.Time) error
	// AckDispatches marks the **term_*** rows matched by (host_id, seq) as done in batch
	// (terminal frames have no execution result; they are acked once the daemon has started/written to the PTY). exec rows do not use it.
	AckDispatches(ctx context.Context, hostID string, seqs []int64, now time.Time) error
	// GetDispatch gets one command by id; returns ErrNotFound if it does not exist.
	GetDispatch(ctx context.Context, id string) (persistmodel.HostDispatch, error)
	// FailPendingExecsForHost is the fallback when a host goes offline: it sets all of that host's **exec** commands whose state is in
	// (queued, delivered) to done with a failure result,
	// pg_notify, and returns the number of affected rows. term_* frames are untouched (weakly reliable; late keystrokes are handled harmlessly).
	FailPendingExecsForHost(ctx context.Context, hostID, resultJSON string, now time.Time) (int64, error)
	// PurgeOldTermDispatches deletes old term_* rows with created_at before before and state='done'
	// (terminal frames keep no results and are only cleaned up periodically), and returns the number of deleted rows.
	PurgeOldTermDispatches(ctx context.Context, before time.Time) (int64, error)
	// PurgeDoneExecDispatches deletes completed exec rows past the retention period (payload/result contain
	// injected credentials and output that may print plaintext, so they must not be kept long).
	PurgeDoneExecDispatches(ctx context.Context, before time.Time) (int64, error)
	// TerminalHostID returns the host of the latest term_* frame for a terminalID, used to check ownership when the daemon
	// reports terminal output frames. Returns ErrNotFound if not found.
	TerminalHostID(ctx context.Context, terminalID string) (string, error)
}

// HostHeartbeatSnapshot is an optional host snapshot refresh in a heartbeat; zero-value fields mean no update.
type HostHeartbeatSnapshot struct {
	Name          string
	OS            string
	Arch          string
	DaemonVersion string
	// GNUToolsOK uses a pointer to tell "not sent" (nil) from "sent as false".
	GNUToolsOK *bool
	Toolcheck  *string
	// NativeTools has the same pointer meaning: nil means this beat did not carry it.
	NativeTools *bool
}

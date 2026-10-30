// One-time entry tickets for the terminal WebSocket: issuing and redeeming.
//
// # Why the terminal moved to WebSocket, and why that move requires this ticket
//
// The browser side used to be "SSE down + POST up". That shape was forced by the gateway of the
// main domain: the gateway (e.g. `ama.example.com`) negotiates HTTP/2 via ALPN, and h2 has no
// `Connection: Upgrade` mechanism (RFC 7540 forbids hop-by-hop headers), so the WS handshake
// never reaches amad.
//
// With a separate entry that **only negotiates HTTP/1.1** (e.g. `ama-ws.example.com`), that
// obstacle is gone. Tested: got 101 + two-way traffic + 20 frames in order. So the terminal can
// become one real WebSocket, which also fixes two problems users reported:
//
//   - **Out-of-order echo**: each keystroke used to be its own POST, with several fetches in
//     flight at once, so arrival order was set by the network, not by send order. A single WS
//     keeps order by nature, so this bug can no longer happen.
//   - **Lag**: each character used to wait a full RTT. WS just pushes one way, no waiting for a
//     response.
//
// # ⚠️ But the separate domain breaks every credential channel; that is why this ticket exists
//
// Both existing paths stop working at once:
//
//   - **The Cookie is not sent.** `ama_session` is set without a `Domain` attribute, so per
//     RFC 6265 it is host-only and the browser only sends it to `ama.example.com`. (Verified
//     with the standard library cookiejar.)
//   - **The `Authorization` header cannot be used either.** The browser's `new WebSocket()`
//     does not support custom request headers. This is a WHATWG spec limit with no switch.
//
// What remains is the URL query, and **long-lived credentials must never go in the query**
// (they end up in browser history, gateway access logs, Referer). This repo has the same rule
// for the `ama` CLI: it has no `--token` flag on purpose, because the command line is stored
// as is in RunEvent forever.
//
// So we use a "short-lived one-time ticket": a same-origin endpoint (with Cookie, passing authz)
// exchanges for a ticket that can be redeemed only once, is valid for one run only, and expires
// in tens of seconds. Finding a used ticket gets you nothing.
//
// **The Cookie's Domain was not widened.** That is a one-line change, but it would send
// `ama_session` to every service under `example.com`; any compromised subdomain would leak the
// AMA login state. It is also a one-way door (narrowing it back logs out every live session).
// See repository/schema/terminal_tickets.sql.
package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// isHostNotFoundErr recognizes "host does not exist". The host domain's ErrNotFound wraps
// persistmodel.ErrNotFound, and the storage layer may also return the bare latter, so checking
// only the persistmodel sentinel is enough (it matches both forms); core does not import
// service/host for this.
func isHostNotFoundErr(err error) bool {
	return errors.Is(err, persistmodel.ErrNotFound)
}

// terminalTicketTTL is how long a ticket is valid.
//
// 30 seconds: it only needs to cover one round trip, "frontend gets the ticket → starts the WS
// handshake", which takes tens of milliseconds. Minutes would bring no benefit, only a larger
// leak window.
//
// ⚠️ **Do not raise it for "users with slow networks".** If it is slow, the frontend just asks
// for a new ticket (the issuing endpoint is same-origin, has the Cookie, and can be called any
// time), and the shorter a ticket lives the safer it is.
const terminalTicketTTL = 30 * time.Second

// terminalTicketPurgeAge is the margin kept when purging expired tickets.
//
// Keep an hour instead of deleting right at expiry: `used_at` helps debugging ("who used this
// ticket, and when"), and deleting right at expiry would make exactly the ticket that just
// caused trouble impossible to find.
const terminalTicketPurgeAge = time.Hour

// IssueTerminalTicket issues the current logged-in user a ticket to open this run's terminal.
//
// ⚠️ **The ticket route (POST .../terminal/ticket) is ActionRead in the authz matrix, not
// ActionManage**, because there is a row-level relation here that the matrix cannot check per
// route: the terminal of a personally connected host may only be opened by **the machine
// owner** (who may be just a member in the workspace, or even demoted to viewer).
// This is the same kind of row-level check as "only agent.CreatedBy can change personal
// authorization" in service/agent (the matrix can only map the runId in the URL back to a
// workspace role; it cannot tell whose machine is in the run snapshot), so the real tiered
// authorization lives in this function, **fail-closed**:
//
//	container run                 workspace owner/admin (ActionManage, same as before the change)
//	workspace-assigned host run   workspace owner/admin (v1 behavior: admins may run on the owner's machine)
//	personally connected host run the machine owner (looked up from run.HostID via hosts.owner_email)
//
// Identity still comes only from context, never from caller parameters; only the server can
// write into context.
func (s *Service) IssueTerminalTicket(ctx context.Context, runID string) (string, error) {
	actor := strings.TrimSpace(auth.ActorFrom(ctx))
	if actor == "" {
		// Getting here means the wiring is wrong (this route is not wrapped by Protect).
		// **Return an error instead of issuing an anonymous ticket**: an anonymous ticket opens
		// the terminal to the whole world.
		return "", fmt.Errorf("%w: 缺少身份，无法签发终端入场票", ErrInvalidArgument)
	}

	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return "", err
	}

	// Authorization + availability checks split into three tiers by executor. A host run has no
	// container, so it must not go through the four-state container check (that would always
	// return None, never issue a ticket, and make host terminals unusable); a host run only
	// needs to be online.
	switch {
	case run.HostID == "":
		// Container: keep the workspace admin bar (before the change the whole route was
		// ActionManage).
		if !s.actorCanManageTerminal(ctx, actor, run.WorkspaceID) {
			return "", fmt.Errorf("%w: 只有空间管理员能打开容器终端", ErrAccessDenied)
		}
		// First confirm this run really has a usable container, **before issuing the ticket**.
		//
		// We could issue without checking, but then the user gets a ticket, starts the
		// handshake, and only fails on the WS side. A WS failure can only say "cannot connect",
		// so the frontend cannot explain the real cause, "the container was reclaimed".
		// Failing here returns a structured {code,message}, which the frontend can turn into a
		// message for each case.
		state, err := s.RunSandboxState(ctx, runID)
		if err != nil {
			return "", err
		}
		if state.Status != SandboxStatusRunning {
			return "", fmt.Errorf("%w: 容器当前不可用（状态：%s），请先恢复容器",
				ErrPrecondition, state.Status)
		}
	case apimodel.EffectiveHostSource("host", run.HostSource) == string(apimodel.HostSourcePersonal):
		// Personally connected: only the machine owner. The run is a snapshot taken when the
		// session was created; the agent changing its source later does not change this past
		// run's tier. So check against the owner found from run.HostID, not the agent.
		if s.hostTerms == nil {
			return "", fmt.Errorf("%w: 服务端未启用宿主机终端", ErrPrecondition)
		}
		owner, oerr := s.hostTerms.HostOwnerEmail(ctx, run.HostID)
		if oerr != nil {
			if isHostNotFoundErr(oerr) {
				return "", fmt.Errorf("%w: 宿主机已被属主删除，无法打开终端", ErrPrecondition)
			}
			return "", oerr
		}
		if !strings.EqualFold(owner, actor) {
			return "", fmt.Errorf("%w: 个人直连的宿主机终端只有机器属主本人（%s）能打开",
				ErrAccessDenied, owner)
		}
		if err := s.ensureHostOnlineForTicket(ctx, run.HostID); err != nil {
			return "", err
		}
	default:
		// Workspace-assigned (including existing rows with an empty source; unknown
		// EffectiveHostSource values also fall here): keep the workspace admin bar.
		if !s.actorCanManageTerminal(ctx, actor, run.WorkspaceID) {
			return "", fmt.Errorf("%w: 只有空间管理员能打开空间宿主机的终端", ErrAccessDenied)
		}
		if err := s.ensureHostOnlineForTicket(ctx, run.HostID); err != nil {
			return "", err
		}
	}

	ticket, err := newTerminalTicket()
	if err != nil {
		return "", err
	}
	now := time.Now()
	if err := s.store.IssueTerminalTicket(ctx, persistmodel.TerminalTicketRow{
		Ticket:    ticket,
		RunID:     runID,
		Actor:     actor,
		ExpiresAt: now.Add(terminalTicketTTL),
		CreatedAt: now,
	}); err != nil {
		return "", err
	}

	// Audit: who asked for a ticket for which run. **Do not log the ticket value**: logging it
	// means writing the credential into the log, and log retention and access are far wider than
	// this ticket's 30-second lifetime.
	s.log().Info("签发终端入场票",
		slog.String("actor", actor), slog.String("run", runID))
	return ticket, nil
}

// ensureHostOnlineForTicket confirms before issuing a ticket that the host daemon's heartbeat
// is fresh.
//
// The nil check on hostTerms lives here, not in each calling branch: the workspace-assigned /
// default branch also reaches this function. If only the personal branch checked early, a
// workspace host with hostTerms not wired would panic here on a nil call, turning the
// fail-closed ticket path into a 500 / crash.
func (s *Service) ensureHostOnlineForTicket(ctx context.Context, hostID string) error {
	if s.hostTerms == nil {
		return fmt.Errorf("%w: 服务端未启用宿主机终端", ErrPrecondition)
	}
	online, err := s.hostTerms.HostOnline(ctx, hostID)
	if err != nil {
		return err
	}
	if !online {
		return fmt.Errorf("%w: 宿主机当前离线（ama daemon 心跳超时），请确认它正在运行",
			ErrPrecondition)
	}
	return nil
}

// actorCanManageTerminal checks whether actor is an owner/admin of the workspace.
//
// ⚠️ **fail-closed**: unlike service/agent.actorIsAdmin, there is no "allow when ctx has no
// identity" branch here. After the ticket route was lowered to ActionRead, this function is the
// only admin bar for container / workspace host terminals, so any case where the role cannot be
// found (DB hiccup, non-member, dependency not wired) must be treated as "cannot open". Treating
// a database failure as allow would be the real incident. The actor here is always non-empty
// (anonymous callers are already stopped at the start of IssueTerminalTicket).
func (s *Service) actorCanManageTerminal(ctx context.Context, actor, workspaceID string) bool {
	if s.workspaces == nil {
		return false
	}
	role, ok, err := s.RoleOf(ctx, workspaceID, actor)
	if err != nil || !ok {
		return false
	}
	return role == apimodel.RoleOwner || role == apimodel.RoleAdmin
}

// RedeemTerminalTicket redeems a ticket and returns the run and identity bound to it.
//
// ⚠️ **This is the only auth point for the WS connection**, because that connection has
// neither a Cookie nor an Authorization header. So:
//
//   - the returned actor is the identity of that connection, and everything after relies on
//     it; **never accept what the client claims**;
//   - the returned runID must be compared with the one in the URL (the caller does this),
//     otherwise one valid ticket could open the terminal of any run. "One-time" would be right
//     but the authorization scope lost, and the latter is the real boundary.
//
// Atomicity is guaranteed by the Store (PG is a single UPDATE, mem holds the lock for the whole
// section); see the comments in those two places.
func (s *Service) RedeemTerminalTicket(
	ctx context.Context, ticket string,
) (persistmodel.TerminalTicketRow, error) {
	ticket = strings.TrimSpace(ticket)
	if ticket == "" {
		return persistmodel.TerminalTicketRow{}, ErrNotFound
	}
	row, err := s.store.RedeemTerminalTicket(ctx, ticket, time.Now())
	if err != nil {
		// Missing / already used / expired: the Store always returns ErrNotFound; pass it up as
		// is.
		return persistmodel.TerminalTicketRow{}, err
	}
	return row, nil
}

// PurgeExpiredTerminalTickets purges expired tickets; called by a scheduled task.
//
// ⚠️ **A purge function that nobody calls gives zero errors** (the pgmq table grew for months
// that way before a call was added). This function's call site is in the sweep in
// core/scheduler.go, wired up when it was added.
func (s *Service) PurgeExpiredTerminalTickets(ctx context.Context) (int64, error) {
	return s.store.PurgeExpiredTerminalTickets(ctx, time.Now().Add(-terminalTicketPurgeAge))
}

// newTerminalTicket generates the ticket value: 32 bytes of crypto/rand, hex encoded.
//
// ⚠️ **Do not use s.nextID** (in-process counter). This ticket is a **cross-instance**
// credential: the instance that issues it and the instance that redeems it are almost never
// the same (issuing goes through the ama domain, redeeming through the ama-ws domain, plus
// traffic shifting across data centers). Two instances each counting up from a timestamp will
// collide sooner or later, and a collision means one user can redeem another user's ticket,
// which is privilege escalation.
//
// Same reasoning as the comment on newTerminalID, only the consequence here is worse (a
// collision there mixes up terminals, a collision here mixes up identities).
func newTerminalTicket() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// A crypto/rand failure is a system-level fault. **Never fall back to math/rand**:
		// a predictable ticket is the same as no auth.
		return "", fmt.Errorf("生成终端入场票失败: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

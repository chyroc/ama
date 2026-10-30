// The Service of the Agent domain: what it needs to do its work.
//
// # Six dependencies
//
//	Store         the agents' own table, plus reading workspaces, someone else's table; see store.go
//	NextID        issues ids. The agent prefix is decided by this package, the sequence by the wiring
//	Revisions     records a version before saving, clears history after deleting. **This package
//	              declares the narrow interface**, same as schedule
//	Members       decides "is this person an admin in this workspace". Narrowed to the single
//	              method RoleOf
//	Integrations  looks up display names of authorization sources. The real one is still in core
//	Syncs         schedules one "sync to Ark" after saving. The real one is still in core (see below)
//	Logger        may be nil
//
// The first three have exactly the same shape as in the schedule domain; the last three are unique
// to this domain, each with its reason written
// on the interface declarations below.
//
// # ⚠️ This step only moves the agent body; the enqueue side of agentsync was **not** moved
//
// The `EnqueueAgentSync` family (enqueue / retry / fetch latest / kick the local syncer)
// is still in core/agentsync.go, and this domain calls it through the single-method interface Syncs.
// The reason is that side
// shares a pair of in-process state fields (syncKickMu / syncKick) with `AgentSyncer` (the
// long-running executor),
// and **the executor itself cannot be moved**: the three methods it calls, arkKey / ensureBinding /
// arkSkillsFor,
// belong to the ark / chat domains that have not been split into packages yet. Moving it by force
// would need interfaces extracted for those two domains now, and those interfaces
// would almost certainly be rewritten when chat is really split; word for word the same argument
// as the file header of service/schedule/service.go,
// "why the scheduler was not moved".
//
// So the enqueue side is left for a later step to move on its own; that step touches the lifecycle
// of `AgentSyncer.Run`
// and that pair of fields on core.Service, a different kind of change; mixing it in would make this
// step's diff impossible to self-review.
//
// # ⚠️ The Skill domain is not in this package; it is another domain
//
// core/agent.go used to be **two domains stitched in one file** (with a `// skills` separator
// comment around line 1094).
// This step only moves the agent half; the skill half (ListSkills / CreateSkill /
// SkillFiles …) has been `git mv`ed into core/skill.go, to be moved in a later step.
// We tripped over "domain ≠ file" once, so that half now has a file name that says where the
// domain lives.
package agent

import (
	"context"

	"errors"
	"github.com/chyroc/ama/server/model/persistmodel"
	"log/slog"

	"github.com/chyroc/ama/server/model/apimodel"
)

// Revisions is the version history capability this domain needs, **listing only the two used**.
//
// ⚠️ **Neither method returns an error, on purpose**: failing to record history should not fail the
// user's save,
// and failing to clear history should not report "the delete already succeeded" as a failure. The
// full argument is on
// revisionsvc.Service.Record / Drop; getting the direction wrong gives no error, and tests stay
// green.
//
// Exactly the same shape as schedulesvc.Revisions. **Deliberately not sharing one declaration**: zero
// imports between domain packages
// is a hard rule, and sharing would make one import the other. Two three-line interfaces will not
// drift
// (a signature mismatch is a compile error), while a package dependency is permanent.
type Revisions interface {
	// Record stores a snapshot from before the change. payload is the result of Snapshot(current
	// entity).
	Record(
		ctx context.Context,
		kind apimodel.RevisionKind,
		workspaceID, entityID, summary string,
		payload any,
		reason apimodel.RevisionReason,
	)

	// Drop deletes all history of an entity. Called when an agent is deleted.
	Drop(ctx context.Context, kind apimodel.RevisionKind, entityID string)
}

// Members is the permission capability this domain needs, **with only one method**.
//
// Used to answer "is the current person an admin/owner in this workspace" (see actorIsAdmin).
//
// ⚠️ **Must be the same function authz.Guard uses for permission checks** (wiring fills in
// *workspacesvc.Service, which satisfies this interface naturally). Comparing against the members
// array yourself,
// the rules would sooner or later diverge from Guard, so "Guard let it through but this blocks it",
// with an error saying "you have no permission",
// a sentence the user cannot do anything about.
type Members interface {
	// RoleOf returns the actor's role in a workspace. The second return value false means not a
	// member.
	//
	// When both a user row and a department row match, **the one with the highest permission
	// wins**; that rule is in
	// workspacesvc.RoleOf, and this domain does not check it again.
	RoleOf(ctx context.Context, workspaceID, actor string) (apimodel.WorkspaceRole, bool, error)
}

// Integrations is the "authorization source" capability this domain needs: looking up display names.
//
// The real one is core's authorization source registry (not split into a package yet), so the
// interface is declared here and filled in by core.
type Integrations interface {
	// DisplayName looks up an authorization source's display name, falling back to the id if not
	// found.
	//
	// Only used for error text. **Fall back to the id rather than error**: when a source is retired
	// while bindings remain, the admin
	// must be able to see this row and delete it; hiding it would leave them unable to delete a
	// binding they cannot see.
	DisplayName(providerID string) string
}

// Syncs is the capability "schedule one sync of this agent to Ark", **with only one method**.
//
// The real one is still in core/agentsync.go; for why see "the enqueue side of agentsync was not
// moved" in this file's header.
//
// ⚠️ **No error return value, on purpose** (the same shape as Revisions): a failed enqueue should not
// make the user's save report an error; the agent is already stored, and an error now would make the
// user think it was not saved. The cost of not
// being queued is only falling back to the old behavior: the next conversation's ensureBinding
// catches it up. The real one internally
// degrades failures into a Warn log.
type Syncs interface {
	// EnqueueAgentSync schedules one sync task for an agent. This domain does not use the return
	// value (frontend polling
	// uses another endpoint), so it is deliberately not taken here: **interfaces list only what is
	// used**.
	EnqueueAgentSync(ctx context.Context, a apimodel.Agent)
}

// Config is what is needed to build a Service.
type Config struct {
	// Store is this domain's persistence. Required.
	Store Store

	// NextID issues a new id with a prefix. Required.
	//
	// ⚠️ **The prefix is an observable contract**: agent_ appears in URLs, logs and user
	// screenshots,
	// and is also written into bindings on the Ark side. Changing the prefix means a new set of ids;
	// all old links break with zero errors.
	NextID func(prefix string) string

	// Revisions is the version history domain. Required.
	//
	// **It must not be nil, and there is no "skip if nil" degrade**: that degrade gives zero errors,
	// showing up as
	// "changed the agent but no history to roll back", found only the day the user really wants to
	// roll back.
	Revisions Revisions

	// Members is the role lookup for permission checks. Required.
	//
	// ⚠️ **Also must not be nil**. "Let through if nil" is the most dangerous degrade here: it would
	// make
	// the two gates "changing authorization injection needs an admin" and "only the creator can touch
	// personal authorizations" vanish entirely,
	// while all tests stay green: with no gate, the positive cases are supposed to pass anyway.
	Members Members

	// Integrations is the authorization source registry and workspace bindings. Required.
	//
	// ⚠️ Same reason as Members: skipping when nil would silently disable the
	// `ensureServiceCredentialBound`
	// gate "a service account must be bound in the workspace first", so the authorization list in
	// workspace settings
	// becomes decoration (anyone could hang any credential directly on an agent).
	Integrations Integrations

	// Syncs is "schedule one sync to Ark". Required.
	//
	// ⚠️ Skipping when nil would show up as: after changing the prompt, save only writes the
	// database without touching Ark, and the user waits until
	// someone opens a new conversation for it to take effect; **exactly the symptom this mechanism
	// was built to remove**, with zero errors.
	Syncs Syncs

	// Console is the locating capability the Ark console jump needs; see the declaration in
	// console.go.
	// **May be nil**: then an all-empty ref is returned, and the frontend does not render the link.
	Console Console

	// HostBindings validates host executor bindings; see that interface's comment. **May be nil**:
	// when the host domain is not wired,
	// saves carrying executorType=host are rejected explicitly.
	HostBindings HostBindings

	// Logger may be nil; when empty slog.Default() is used.
	//
	// ⚠️ This domain has one **fail-open** degrade (when the AgentDisabled lookup fails it is not
	// treated as disabled),
	// and the log is the only trace of that failure. Pass in a real logger.
	Logger *slog.Logger
}

// Service is the business logic of the agent domain.
type Service struct {
	store        Store
	nextID       func(prefix string) string
	revisions    Revisions
	members      Members
	integrations Integrations
	syncs        Syncs
	console      Console
	hosts        HostBindings
	logger       *slog.Logger
}

// New builds a Service. None of the five dependencies can be missing; see each one's comment on
// Config:
// what they share is that "degrade if nil" always gives zero errors, and what degrades is exactly
// the gates and syncing.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, errors.New("agent: Store 不能为 nil")
	}
	if cfg.NextID == nil {
		return nil, errors.New("agent: NextID 不能为 nil")
	}
	if cfg.Revisions == nil {
		return nil, errors.New("agent: Revisions 不能为 nil")
	}
	if cfg.Members == nil {
		return nil, errors.New("agent: Members 不能为 nil")
	}
	if cfg.Integrations == nil {
		return nil, errors.New("agent: Integrations 不能为 nil")
	}
	if cfg.Syncs == nil {
		return nil, errors.New("agent: Syncs 不能为 nil")
	}
	return &Service{
		store:        cfg.Store,
		nextID:       cfg.NextID,
		revisions:    cfg.Revisions,
		members:      cfg.Members,
		integrations: cfg.Integrations,
		syncs:        cfg.Syncs,
		console:      cfg.Console,
		hosts:        cfg.HostBindings,
		logger:       cfg.Logger,
	}, nil
}

func (s *Service) log() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

// nonNil makes sure a slice is non-nil. nil would be encoded as null, while the frontend contract
// requires [].
//
// ⚠️ **core still keeps one with the same name** (core/helpers.go); that is not an incomplete move:
// the skill half of the domain
// and sameSkillDraft in core/revisiondiff.go both use it, and domain packages have zero imports
// between them.
// A three-line `if in == nil` fallback; the two copies will not drift.
func nonNil(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// filterSlice filters in place, keeping elements where keep is true, with a non-nil result.
//
// **Non-nil is the key**: a nil slice encodes as null, and the frontend's .map() would blow up.
//
// ⚠️ Same as nonNil, core/helpers.go still keeps a copy (used by the skill half). That file's header
// already says the rule for "do not sink it into a leaf package" is "shared by several resource
// domains and substantial enough"; for ten lines,
// owning a copy each beats creating a package dependency for it.
func filterSlice[T any](in []T, keep func(T) bool) []T {
	out := make([]T, 0, len(in))
	for _, v := range in {
		if keep(v) {
			out = append(out, v)
		}
	}
	return out
}

// HostBindings validates "is this executor choice allowed". Two-level model (workspace assignment /
// personal direct connect).
//
// The real one is the host domain service (service/host). Domain packages do not import each other
// (the hard rule in this file's header),
// so the narrow interface is declared in this domain and injected by cmd/amad at wiring time. **May
// be nil**: when the host domain is not wired,
// any save carrying executorType=host is rejected (rather than let through with tools silently
// running in a container).
type HostBindings interface {
	// ResolveBoundHost validates that hostID exists and is bound into workspaceID (does not check
	// the owner; owner/role
	// are checked by this domain's gate: admins can assign machines bound in by others).
	ResolveBoundHost(ctx context.Context, workspaceID, hostID string) (persistmodel.Host, error)
	// ResolvePersonalHost validates that hostID exists and its owner is actorEmail (does not check
	// workspace binding).
	ResolvePersonalHost(ctx context.Context, actorEmail, hostID string) (persistmodel.Host, error)
}

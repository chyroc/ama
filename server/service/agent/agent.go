// Business logic of Agents.
//
// Most invariants of this domain **cross aggregates**, which is the main reason it needs a Service
// and not just a Store:
//
//   - writing an agent must sync the reverse reference on the Feishu app side (bindings are visible
//     from both sides)
//   - deleting an agent must clear all its version history (otherwise those snapshots can never be
//     rolled back to anywhere)
//
// The reverse reference's **source of truth is on the agent side** (Agent.FeishuAppIDs). The
// BoundAgentIDs on a Feishu app
// is derived, computed live by Store.AgentsBoundTo; so this package does not need to write
// the feishu package's data, and the two modules are coupled only by an ID string. This is also why
// the two-way sync in
// LocalApiClient's syncFeishuBindings **does not need** to exist on the server:
// it is there so the two copies in localStorage do not conflict, while here there is only one.
//
// # Feishu apps are 1:1 exclusive
//
// A Feishu app can be bound by at most one agent. Skills are not like this (one skill can be reused
// by several agents),
// so this constraint is unique to Feishu; for the reason see the comment on ensureFeishuAppsFree:
// an inbound message can have only one answerer. The write side is guaranteed by that function + a
// partial unique index in the database.
//
// # ⚠️ The Skill half is not in this file; it is another domain
//
// This code used to live in core/agent.go, and that file was **two domains stitched together**:
// after a `// skills` separator comment around line 1094, everything was the skill domain
// (including SkillFiles).
// When this domain moved over, that half stayed in core and was git mv'ed into core/skill.go,
// so the next person gets a file name that says where the domain lives. **Do not add skill code
// here again.**

package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/shared/infra/shelltools"
)

// ListAgents lists the agents of a workspace. When the workspace does not exist it returns an empty
// list rather than 404:
// the frontend's listAgents return type is an array, and a 404 would make the page throw.
func (s *Service) ListAgents(ctx context.Context, workspaceID string) ([]apimodel.Agent, error) {
	return s.store.ListAgents(ctx, workspaceID)
}

// GetAgent fetches an agent; returns persistmodel.ErrNotFound when it does not exist.
//
// The text matches LocalApiClient's updateAgent. On this GET path the frontend turns 404 into null
// and
// shows no message, but the same error also surfaces from update / delete, where it is shown to the
// user.
func (s *Service) GetAgent(ctx context.Context, id string) (apimodel.Agent, error) {
	a, err := s.store.GetAgent(ctx, id)
	if err != nil {
		if errors.Is(err, persistmodel.ErrNotFound) {
			return apimodel.Agent{}, fmt.Errorf("%w: Agent 不存在", persistmodel.ErrNotFound)
		}
		return apimodel.Agent{}, err
	}
	return a, nil
}

// CreateAgent creates an agent. version starts from 1.
func (s *Service) CreateAgent(ctx context.Context, workspaceID string, draft apimodel.AgentDraft) (apimodel.Agent, error) {
	if err := validateAgentDraft(draft); err != nil {
		return apimodel.Agent{}, err
	}
	// Carrying authorizations at creation must also pass the gate.
	//
	// ⚠️ **This is the easiest one to miss**, because it is not in the "edit" mental model: POST to
	// create an agent is
	// ActionWrite (members can do it), and the create form can also check authorizations; without
	// blocking, simply
	// "create a new one" would bypass the whole restriction, however strictly the PATCH path blocks.
	//
	// **Reject explicitly, do not ignore silently.** Silently ignoring would make people think it is
	// set up, then the agent in the container
	// fails as soon as it calls the matching platform command, and that failure **does not point at
	// permissions** (it points at missing credentials, or even
	// at the CLI itself). Better to say it clearly at the moment of saving.
	//
	// ⚠️ The current passed in is **an empty agent with only CreatedBy and WorkspaceID filled**,
	// and both are required:
	//
	//	CreatedBy = actor   so "the creator checks their own personal authorization on their new
	//	                    agent"
	//	                    hits the "is the creator → let through" branch. Without it, it would
	//	                    fall into
	//	                    "no creator recorded → needs admin", and members could not create
	//	                    agents with their own
	//	                    personal authorizations, which item 1 of the requirements explicitly
	//	                    says must succeed.
	//	empty IntegrationIDs  so every item in the draft counts as "added", each one validated.
	actor := auth.ActorFrom(ctx)
	if err := s.ensureIntegrationsAllowed(ctx,
		apimodel.Agent{WorkspaceID: workspaceID, CreatedBy: actor}, workspaceID,
		draft.IntegrationIDs); err != nil {
		return apimodel.Agent{}, err
	}
	// Aliases are on the same level as authorizations: carried at creation they also need admin. The
	// reason is word for word as above.
	if err := s.ensureCanSetEnvAliases(ctx, workspaceID, draft.EnvAliases); err != nil {
		return apimodel.Agent{}, err
	}
	if _, err := s.store.GetWorkspace(ctx, workspaceID); err != nil {
		return apimodel.Agent{}, fmt.Errorf("%w: 空间不存在", persistmodel.ErrNotFound)
	}
	tri, err := s.ensureExecutorAllowed(ctx,
		apimodel.Agent{WorkspaceID: workspaceID, CreatedBy: actor}, draft)
	if err != nil {
		return apimodel.Agent{}, err
	}

	// Feishu apps are 1:1 exclusive, so taking someone else's app must be blocked at creation too.
	// An empty id is passed down,
	// because this agent has no ID yet; it cannot be the current holder of any app.
	if err := s.ensureFeishuAppsFree(ctx, "", draft.FeishuAppIDs); err != nil {
		return apimodel.Agent{}, err
	}

	now := apimodel.NowISO()
	a := apimodel.Agent{
		ID:          s.nextID("agent"),
		WorkspaceID: workspaceID,
		Name:        strings.TrimSpace(draft.Name),
		Description: draft.Description,
		System:      draft.System,
		Model:       draft.Model,
		Version:     1,
		// Slices are always non-nil, otherwise they encode as null and the frontend blows up
		BuiltinTools: nonNil(draft.BuiltinTools),
		SkillIDs:     nonNil(draft.SkillIDs),
		FeishuAppIDs: nonNil(draft.FeishuAppIDs),
		Status:       draft.Status,
		// Authorization source list: **an empty list is a valid value, meaning "inject none"**.
		//
		// ⚠️ **No default is filled in here on purpose**. New agents default to no authorizations
		// selected (the requirement says so explicitly),
		// so both nil and [] become [].
		IntegrationIDs: normalizeIntegrationIDs(draft.IntegrationIDs),
		Envs:           normalizeAgentEnvs(draft.Envs),
		// Credential aliases (inject a platform credential once more under another name). Also made
		// non-nil.
		EnvAliases: normalizeAgentEnvAliases(draft.EnvAliases),
		// Redirect or queue when sending another message. **An empty string normalizes to redirect**;
		// see
		// apimodel.FollowUpMode: that enum's default side is the opposite of other enums in this
		// repo, on purpose.
		FollowUp: apimodel.NormalizeFollowUpMode(draft.FollowUp),
		// Whether to show detailed work content (chain of thought) in Feishu. **An empty string
		// normalizes to full (show)**,
		// same reason as above; see apimodel.ProgressDetail.
		ProgressDetail: apimodel.NormalizeProgressDetail(draft.ProgressDetail),
		ExecutorType:   tri.ExecType,
		HostID:         tri.HostID,
		HostSource:     tri.Source,
		CreatedAt:      now,
		UpdatedAt:      now,
		// Who created it / who last changed it. **Taken from context, not from draft**;
		// see that part on apimodel.Agent.CreatedBy.
		//
		// An empty string is possible (identity-less system paths); then omitempty makes the key
		// disappear,
		// and the frontend reads "we do not know who created it", the same display as existing
		// agents.
		// **Do not fill in a fake value here** (like the current workspace's owner); that would be
		// guessing.
		CreatedBy: actor,
		UpdatedBy: actor,
	}
	// No stated status means enabled. **It used to default to draft**, which gave every new agent a
	// status
	// nobody looked at; with draft removed, the only reasonable default is "usable":
	// the user just filled the form and clicked save, and clearly did not intend "create one that
	// cannot be used".
	if a.Status == "" {
		a.Status = apimodel.StatusActive
	}
	created, err := s.store.CreateAgent(ctx, a)
	if err != nil {
		return apimodel.Agent{}, err
	}
	// Schedule a sync right after creation, rather than waiting for someone to open a conversation.
	//
	// **Failure does not affect this return** (EnqueueAgentSync degrades to a Warn internally): the
	// agent is already created,
	// and an error now would make the user think it was not created. The cost of not being queued is
	// only falling back to the old behavior:
	// the next conversation's ensureBinding catches it up.
	s.syncs.EnqueueAgentSync(ctx, created)
	return created, nil
}

// UpdateAgent updates an agent.
//
// **version is incremented and updatedAt refreshed every time, even for an empty patch** (copied
// from api.ts:369).
// Ark uses version for optimistic locking, and the frontend sends it back, so this increment is part
// of the contract.
//
// The input is a full draft rather than per-field pointers: although the frontend's updateAgent
// signature is Partial,
// the actual call (AgentDetailPage) always passes the full object. Pointers would fall into
// ModelConfig's
// null trap (see the comment on apimodel.ModelConfig).
func (s *Service) UpdateAgent(ctx context.Context, id string, draft apimodel.AgentDraft) (apimodel.Agent, error) {
	return s.UpdateAgentWithReason(ctx, id, draft, apimodel.RevisionReasonEdit)
}

// UpdateAgentWithReason is the implementation of UpdateAgent, with one more "what reason this change
// counts as when recorded in history".
//
// **It is exported because the rollback slot needs it**: the agent slot in core/revision.go uses it
// to write the snapshot back,
// with reason rollback. Same as schedulesvc.UpdateScheduleWithReason.
//
// ⚠️ **Rollback must go through here rather than its own recordRevision + UpdateAgent**: that would
// record two entries
// (one by the rollback itself, another inside UpdateAgent), with identical content and different
// reasons.
// We tested it: TestRollbackAlsoRecordsRevision then reported "expected 2, got 3".
//
// A parameter rather than stuffing a value into ctx: passing through ctx is implicit, so readers of
// UpdateAgent cannot see
// "this reason may not be edit", while an extra parameter is explicit and watched by the compiler.
func (s *Service) UpdateAgentWithReason(
	ctx context.Context, id string, draft apimodel.AgentDraft, reason apimodel.RevisionReason,
) (apimodel.Agent, error) {
	if err := validateAgentDraft(draft); err != nil {
		return apimodel.Agent{}, err
	}
	a, err := s.store.GetAgent(ctx, id)
	if err != nil {
		return apimodel.Agent{}, fmt.Errorf("%w: Agent 不存在", persistmodel.ErrNotFound)
	}
	// Feishu apps are 1:1 exclusive. **Exclude itself**: otherwise the most common save, "only rename,
	// send the bindings back unchanged",
	// would be blocked by its own existing binding, showing up as "this agent cannot be saved
	// anymore".
	if err := s.ensureFeishuAppsFree(ctx, a.ID, draft.FeishuAppIDs); err != nil {
		return apimodel.Agent{}, err
	}
	tri, err := s.ensureExecutorAllowed(ctx, a, draft)
	if err != nil {
		return apimodel.Agent{}, err
	}

	// Record a snapshot from **before** the change, for rollback after mistakes.
	//
	// **The position here is deliberate**: before the assignments below (otherwise what is stored is
	// the changed version),
	// but after validation and the exclusivity check (a save doomed to fail should not leave an entry
	// in history).
	//
	// ⚠️ Do not record when the content did not change; otherwise a user clicking save twice would
	// store two identical snapshots,
	// using up the 20-entry cap for nothing.
	if before := Snapshot(a); !sameAgentDraft(before, draft) {
		s.revisions.Record(ctx, apimodel.RevisionKindAgent, a.WorkspaceID, a.ID,
			a.Name, before, reason)
	}

	a.Name = strings.TrimSpace(draft.Name)
	a.Description = draft.Description
	a.System = draft.System
	a.Model = draft.Model
	a.BuiltinTools = nonNil(draft.BuiltinTools)
	a.SkillIDs = nonNil(draft.SkillIDs)
	a.FeishuAppIDs = nonNil(draft.FeishuAppIDs)
	if draft.Status != "" {
		a.Status = draft.Status
	}
	// IntegrationIDs **is assigned unconditionally, with no "skip if empty" check** (the opposite of
	// Status).
	//
	// An empty list here is a meaningful value (= cancel all authorizations), not "not sent".
	// Writing it like Status would make users **unable to cancel the last authorization**: the
	// frontend sends an empty array,
	// and if empty arrays were skipped, the stored copy would stay forever, showing up as "cancelled
	// but it came back by itself", with no error.
	// This is the same kind of symptom that the defaultsApplied gate in AgentsPage guards against.
	//
	// ⚠️ **But changing this field must pass a gate**: personal authorizations can only be touched by
	// the creator, service accounts need admin
	// and must already be bound in the workspace. Checking "did it change" rather than "was it sent"
	// is deliberate: the frontend's
	// toAgentDraft **always** sends back the full draft (including integrationIds), and rejecting
	// unconditionally would stop
	// members from even saving a rename. See the comment on ensureIntegrationsAllowed.
	if err := s.ensureIntegrationsAllowed(
		ctx, a, a.WorkspaceID, draft.IntegrationIDs); err != nil {
		return apimodel.Agent{}, err
	}
	a.IntegrationIDs = normalizeIntegrationIDs(draft.IntegrationIDs)
	// Envs is assigned unconditionally: an empty list means "delete all custom variables", not "not
	// sent".
	a.Envs = normalizeAgentEnvs(draft.Envs)
	// EnvAliases likewise is assigned unconditionally, and **also must pass an admin gate first**
	// (word for word the same reason as integrationIds:
	// check "did it change" rather than "was it sent", otherwise members could not even save a
	// rename).
	if err := s.ensureEnvAliasesUnchanged(ctx, a, draft.EnvAliases); err != nil {
		return apimodel.Agent{}, err
	}
	a.EnvAliases = normalizeAgentEnvAliases(draft.EnvAliases)
	// FollowUp is also assigned unconditionally. **Do not write it like Status, `assign only if !=
	// ""`**:
	// an empty string here is a meaningful value (= the default, redirect); skipping it would make a
	// user's change from "queue" back to
	// "redirect" never stick: the stored queue would stay, showing up as "selected it but it bounced
	// back".
	a.FollowUp = apimodel.NormalizeFollowUpMode(draft.FollowUp)
	// ProgressDetail likewise is assigned unconditionally: an empty string is a meaningful value (= the
	// default, "show"),
	// and skipping it would make a change from "do not show" back to "show" never stick.
	a.ProgressDetail = apimodel.NormalizeProgressDetail(draft.ProgressDetail)
	// Executor: validation already passed in ensureExecutorAllowed above (personal direct connect only
	// for the creator, workspace assignment for
	// admins or the machine owner). Assigned unconditionally: the empty triple is the valid value for
	// "switch back to Docker container".
	a.ExecutorType = tri.ExecType
	a.HostID = tri.HostID
	a.HostSource = tri.Source
	// Ark increments version on every update; mirror that behavior here
	a.Version++
	a.UpdatedAt = apimodel.NowISO()
	// Who changed it. **Only overwrite UpdatedBy, never touch CreatedBy**: the latter is kept as read
	// back,
	// it records "who created it", and should not be replaced by later editors.
	a.UpdatedBy = auth.ActorFrom(ctx)

	saved, err := s.store.SaveAgent(ctx, a)
	if err != nil {
		return apimodel.Agent{}, err
	}
	// Schedule a sync after a successful save. **This line is the point of this change**: before it,
	// after changing the prompt, save
	// only wrote the database without touching Ark, and the user waited until someone opened a new
	// conversation for it to take effect, with zero errors,
	// showing up as "I changed it but it acts as if nothing happened". See the file header of
	// core/agentsync.go.
	//
	// The sync is async, so **we do not wait for it**: waiting would block the save for seconds, and
	// a failed sync would make
	// a successful save show as failed. The frontend polls GET /api/agents/{id}/sync-task for the
	// result.
	s.syncs.EnqueueAgentSync(ctx, saved)
	return saved, nil
}

// DeleteAgent deletes an agent. Deleting a nonexistent id is not an error (idempotent).
//
// The Feishu app side's BoundAgentIDs need no cleanup: that data is derived, and once the agent is
// gone it simply cannot be found.
func (s *Service) DeleteAgent(ctx context.Context, id string) error {
	if err := s.store.DeleteAgent(ctx, id); err != nil {
		return err
	}
	// The agent is gone, so its history can never be rolled back to anywhere; clear it too.
	// **Failure is only logged**, so it does not turn an already successful delete into a failure;
	// see dropRevisions.
	s.revisions.Drop(ctx, apimodel.RevisionKindAgent, id)
	return nil
}

// AgentDisabled reports whether an agent has been disabled.
//
// Used by the Feishu side to block **before running**. The Feishu path only has the agentID (looked
// up in reverse by resolveBoundAgent),
// and although the AppendRunEvents gate can block it, it can only throw the error upward:
// the Feishu side sees an error and only logs it, and **people in the group receive nothing**.
// That is exactly the hardest kind of fault, "messages silently unanswered". So ask one step
// earlier, to be able to reply with a human sentence.
//
// **When the agent does not exist it returns false rather than an error**: that case is handled
// elsewhere (carry on, and creating the session
// is caught by "Agent does not exist"); this only answers "is it disabled". A database failure
// likewise
// fails open: a database hiccup should not silence all Feishu bots at once, same rule as
// markAgentDeleted.
func (s *Service) AgentDisabled(ctx context.Context, agentID string) bool {
	a, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		if !errors.Is(err, persistmodel.ErrNotFound) {
			s.log().Warn("查 agent 失败，本次不按停用处理", "agent", agentID, "error", err.Error())
		}
		return false
	}
	return a.Status.IsDisabled()
}

// ---------------------------------------------------------------------------
// Authorization injection: who can change "which credentials this agent may inject"
// ---------------------------------------------------------------------------
//
// # Why this gate is in the Service and not in authz
//
// authz decides permissions **per route**, and cannot judge "did some field in the request body
// change". And
// integrationIds is exactly one field in the request body of `PATCH /api/agents/{id}`,
// and that route as a whole must stay ActionWrite (changing name/prompt/model is a member's daily
// work).
//
// So this is the kind of gate the README describes as "Guard cannot judge it, it can only live in
// the Service",
// the same shape as "built-in skills cannot be changed" (UpdateSkill).
//
// ⚠️ **This does not break the rule "authorization does not move down into the Service layer".**
// That rule forbids moving **route-level** role
// checks into the Service, which would break the four identity-less system-driven paths. This is
// different:
//
//   - CreateAgent / UpdateAgent **have only two production callers in the whole repo, the HTTP
//     handlers**
//     (the worker pool, Feishu inbound and schedules never touch them), so there is no
//     risk of "blocking system paths";
//   - and when ctx has no identity it **lets through** (see actorLevel), so unit tests and future
//     system paths
//     can still write this field. All production HTTP endpoints are behind the auth middleware and
//     never reach that branch.
//
// The rule is "does this path have an identity", not "which package the code is in".

// ErrIntegrationsNeedAdmin is the user text for "changing authorization injection needs an admin".
//
// It is a constant so handler-layer tests can pin the exact string: **it goes straight onto the
// screen**,
// and the sentence must point at "whom to ask", not just say "no permission".
const ErrIntegrationsNeedAdmin = "修改授权注入需要管理员权限。注入的凭据代表一个真人，" +
	"agent 拿到后能做的事等于那个员工能做的事，所以只有空间管理员或拥有者可以改。"

// ErrEnvAliasesNeedAdmin is the user text for "changing credential aliases needs an admin".
//
// It is kept apart from ErrIntegrationsNeedAdmin because **the two sentences must point at different
// things**: that one is about
// "which authorizations are checked", this one is about "under what name credentials appear in the
// container". What users see
// are two blocks, and the errors should be distinguishable too. It is a constant so handler-layer
// tests can also pin the exact text.
const ErrEnvAliasesNeedAdmin = "修改凭据别名需要管理员权限。别名会把平台凭据以另一个" +
	"变量名再注入一份容器，能拿到的东西与原凭据完全相同，所以与「授权」同一个档位。"

// ---------------------------------------------------------------------------
// Personal authorization vs service accounts: who can attach which kind to an agent
// ---------------------------------------------------------------------------
//
// # The hole this group of gates closes
//
// After "checking some authorization", whose credential was used was previously always **the
// workspace owner's**
// (those firstOwnerEmail spots in core/sessionenv.go). So a member checking some "personal
// account authorization" made their agent act as the owner's personal identity, and the owner got no
// notice at all.
//
// The fix splits into two kinds by apimodel.IntegrationKind; the rules come from the requirements,
// each landing in the functions below:
//
//	personal authorization  only **the agent's creator** can add or remove it. Nobody else may
//	                        touch it
//	                        (edits not touching authorizations, like changing the title, still
//	                        pass; see "check did it change" below)
//	service account         must **first be bound in the workspace** (workspace settings → service
//	                        account authorization),
//	                        then admins can attach it to an agent
//
// # ⚠️ Why the creator check cannot be expressed with authz role levels
//
// "Is it the creator" is not a role; it is **the relation between this row of data and the actor**.
// authz decides by route
// (it cannot even judge fields, see above), let alone "is this agent's created_by you".
// So it can only live in the Service, the same shape as ensureIntegrationsUnchanged.
//
// # ⚠️ Existing agents: what to do when CreatedBy is empty
//
// `Agent.CreatedBy` is a field added later, so **existing agents may lack the key entirely**
// (the comment on apimodel.Agent.CreatedBy says this is a long-lasting valid state). Then:
//
//	nobody is "the creator"  → if it were "reject anyone who is not the creator", personal
//	                           authorizations on existing agents
//	                           **could be changed by nobody**, including admins who could before,
//	                           and the error would say
//	                           "only the creator can change it", pointing at a person the UI does
//	                           not even show
//
// So when CreatedBy is empty it **falls back to the admin level** (the old behavior). This is not a
// loosening: the old behavior was already
// admin-only, and falling back means "this agent has no creator concept yet, use the old rules".
// And it is diagnosable: the error says clearly "this agent has no creator recorded".
// See that branch in ensureIntegrationsAllowed.

// ErrPersonalIntegrationNeedsCreator is the user text template for "only the creator can touch a
// personal authorization".
//
// ⚠️ The requirement explicitly asks for "a friendly hint", so the sentence must do three things;
// missing one leaves the user stuck:
//
//	say why it is not allowed     the personal credential stands for the creator, not something the
//	                              workspace shares
//	say whom to ask               **name the creator** (%s), do not just say "please contact the
//	                              creator"
//	give an actionable alternative  the workspace's service account authorization is the proper way
//
// It is a constant so tests can pin the exact text: it goes straight onto the screen.
const ErrPersonalIntegrationNeedsCreator = "「%s」是个人授权，只有这个 Agent 的创建者" +
	"（%s）能给它添加或移除。它的凭据代表创建者本人，agent 拿到后能做的事等于那个人" +
	"能做的事，所以不能由别人代为注入或撤销。你可以请他来操作；" +
	"如果是空间里多人共用的场景，请改用空间设置里绑定的服务账号授权。"

// ErrPersonalIntegrationNoCreator is the text for the "existing agent has no creator recorded"
// branch.
//
// A separate sentence rather than reusing the one above: that one must name "whom to ask", while
// here **there is no such person**;
// reusing it would render "only this Agent's creator () can change it", an empty pair of
// parentheses,
// leaving the user knowing neither whom to ask nor why.
const ErrPersonalIntegrationNoCreator = "这个 Agent 没有记录创建者（它建于本功能上线之前），" +
	"所以「个人授权」按管理员权限处理：请空间管理员或拥有者来改。" +
	"若要长期由某个人管理它的个人授权，可以让管理员重建一个 Agent。"

// integrationChange is the result of "which sources changed" in one authorization change.
//
// Currently there is only the personal authorization kind (see apimodel.IntegrationKindOf), so only
// one list.
type integrationChange struct {
	// personal holds the ids of **changed** personal authorizations (both added and removed count;
	// see diffIntegrationIDs).
	personal []string
}

// changed reports whether this change touched authorizations at all.
func (c integrationChange) changed() bool {
	return len(c.personal) > 0
}

// diffIntegrationIDs computes the ids **that changed** between two authorization sets, split by kind.
//
// ⚠️ **Both added and removed count as "changed"**, as the requirement explicitly says:
//
//	a non-creator **adding** a personal authorization   → error (they are borrowing someone else's
//	                                                       identity)
//	a non-creator **removing** a personal authorization → also an error (it is someone else's;
//	                                                       revoking should also be their decision;
//	                                                       and silently allowing removal would make the
//	                                                       agent quietly lose abilities)
//
// So this takes the **symmetric difference**, not "only additions". Only additions would miss
// scenario 5
// (pinned by the test TestNonCreatorCannotRemovePersonalIntegration).
func diffIntegrationIDs(want, have []string) integrationChange {
	inWant := make(map[string]bool, len(want))
	for _, id := range want {
		inWant[id] = true
	}
	inHave := make(map[string]bool, len(have))
	for _, id := range have {
		inHave[id] = true
	}

	var out integrationChange
	// Symmetric difference: what appears on only one side has changed. **Order is want then
	// have**,
	// so the order in the error text is stable (tests assert the exact text).
	for _, id := range want {
		if !inHave[id] {
			out.add(id)
		}
	}
	for _, id := range have {
		if !inWant[id] {
			out.add(id)
		}
	}
	return out
}

// add records one changed id.
func (c *integrationChange) add(id string) {
	c.personal = append(c.personal, id)
}

// ensureIntegrationsAllowed is the single rule for "is this authorization change allowed".
//
// It serves both PATCH (edit) and POST (create); the two paths differ only in "what is used as
// current":
// on edit it is the stored one, on create an empty agent (so every id counts as "added").
// **Sharing one function is deliberate**: two copies would surely drift, and the drift goes "create a
// new one to bypass it";
// exactly what the warning above ensureCanSetIntegrations is about.
//
// ⚠️ **Check "did it change", not "was it sent"** (following ensureIntegrationsUnchanged's rule):
// the frontend's toAgentDraft always sends back the full draft, and checking "was it sent" would
// mean people **could not even save a rename**,
// with an error saying "authorization needs permission", not pointing at what they actually did at
// all.
// This is pinned by a mutation test (swap the diff for "reject if non-empty" → test case 3 must go
// red).
func (s *Service) ensureIntegrationsAllowed(
	ctx context.Context, current apimodel.Agent, workspaceID string,
	incoming []string,
) error {
	change := diffIntegrationIDs(
		normalizeIntegrationIDs(incoming),
		normalizeIntegrationIDs(current.IntegrationIDs),
	)
	// Nothing changed, so let it through. **This branch is the implementation of "a non-creator
	// changing the title gets no error"**.
	if !change.changed() {
		return nil
	}

	// Let it through when ctx has no identity, same reason as actorIsAdmin (unit tests and future
	// system-driven paths).
	// ⚠️ This branch must come first: both checks below compare with the actor, and an empty actor
	// would misjudge
	// "no identity" as "not the creator", so a batch of tests unrelated to authorization would fail
	// here.
	actor := strings.TrimSpace(auth.ActorFrom(ctx))
	if actor == "" {
		return nil
	}

	// -- 1. Personal authorization: only the creator ----------------------------------------
	if len(change.personal) > 0 {
		creator := strings.TrimSpace(current.CreatedBy)
		switch {
		case creator == "":
			// An existing agent with no creator recorded. **Fall back to the admin level** (the old
			// behavior);
			// for why see the part at the start of this section.
			//
			// ⚠️ New agents never get here: in CreateAgent the current is filled with actor as
			// CreatedBy (see the call site there), so "checking your own personal authorization at
			// creation"
			// hits the EqualFold branch below.
			ok, err := s.actorIsAdmin(ctx, workspaceID)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%w: %s", persistmodel.ErrAccessDenied, ErrPersonalIntegrationNoCreator)
			}
		case !strings.EqualFold(creator, actor):
			// ⚠️ Compare with EqualFold: Actor is lowercased at the entry, but the stored CreatedBy was
			// written
			// in the past and its case may differ. A byte comparison would report the same person as
			// "not the creator".
			return fmt.Errorf("%w: "+ErrPersonalIntegrationNeedsCreator, persistmodel.ErrAccessDenied,
				s.integrations.DisplayName(change.personal[0]), creator)
		}
		// It is the creator → let through. **Deliberately no longer requiring admin**: this is the
		// core of the requirement
		// ("personal authorizations must belong to the agent's creator"); a member injecting their
		// own credential into an agent they built
		// is their legitimate right; that credential is theirs to begin with.
	}

	return nil
}

// SetAgentIntegrations changes "which authorizations this agent may inject".
//
// A separate method (rather than going through UpdateAgent) to have its own route and its own
// permission level.
// Permission is checked by authz.Guard at the HTTP boundary (ActionManage), **not checked again
// here**:
// checking twice means two sources of truth, and this method has no other caller.
//
// Like UpdateAgent it bumps version and refreshes updatedAt: this really is a change to the agent,
// and not bumping would make the version the frontend holds differ from the stored one, hitting
// the optimistic lock on the next save.
func (s *Service) SetAgentIntegrations(
	ctx context.Context, id string, ids []string,
) (apimodel.Agent, error) {
	a, err := s.store.GetAgent(ctx, id)
	if err != nil {
		return apimodel.Agent{}, fmt.Errorf("%w: Agent 不存在", persistmodel.ErrNotFound)
	}
	// ⚠️⚠️ **This gate cannot be skipped here, even though Guard already checked ActionManage.**
	//
	// Guard checks "are you an admin of this workspace", while it cannot check the two new rules:
	//
	//	personal authorizations only by the creator  not even admins: the credential stands for the
	//	                                             creator, not something the workspace shares
	//	service accounts must be bound first         otherwise the list in workspace settings is
	//	                                             decoration (admins would hang
	//	                                             any credential here directly, bypassing it)
	//
	// Missing this line would show up as: the PATCH path is blocked, while this route **made
	// specifically for changing authorizations**
	// is wide open; a zero-error hole, worse than before (before, both paths were equally strict).
	// The wiring test TestSetAgentIntegrationsRouteEnforcesCreator pins it.
	if err := s.ensureIntegrationsAllowed(ctx, a, a.WorkspaceID, ids); err != nil {
		return apimodel.Agent{}, err
	}
	a.IntegrationIDs = normalizeIntegrationIDs(ids)
	a.Version++
	a.UpdatedAt = apimodel.NowISO()
	// Same as UpdateAgent: changing authorizations is also a real change, and must leave a trail.
	// CreatedBy stays untouched.
	a.UpdatedBy = auth.ActorFrom(ctx)
	return s.store.SaveAgent(ctx, a)
}

// ensureEnvAliasesUnchanged blocks "sneaking credential alias changes through the request body of
// PATCH /api/agents/{id}".
//
// # ⚠️ Why it **still only checks admin**, rather than switching to "check the creator" like
// authorizations
//
// During self-review you may suspect a gap here: aliases can point at **personal** credentials
// (`AliasableEnvKeys` has EnvAMAToken / EnvLarkUserAccessToken), so would a non-creator adding an
// alias bypass the creator check?
//
// **No.** An alias only "gives another name to a value already injected"; it **naturally inherits
// the authorization decision of integrationIds**:
// the value is taken from the **already merged** values on the shelltools side; if that
// authorization is not checked
// the key is not in merged at all, and the alias produces nothing
// (see the three hard rules in the "credential aliases" section of README-integrations.md).
//
// That is: to make an alias really emit a personal credential, that authorization must be checked
// first, and **that step is already
// blocked by ensureIntegrationsAllowed by creator**. There is no second entry on the alias path.
//
// ⚠️ So do not "casually" change this to check the creator too: that would stop non-creators from
// even adding aliases for **service account** credentials,
// which they already have the right to do (service accounts are shared by the workspace).
//
// **The same shape and the same reason** as ensureIntegrationsAllowed (see that function's comment):
// check "**did it change**" rather than "was it sent", because the frontend's toAgentDraft always
// sends back the full draft;
// checking "was it sent" would stop members from even saving a rename, with an error saying "aliases
// need an admin".
//
// ⚠️ **No sorting** before comparing (the opposite of integrationIds): aliases are an ordered,
// editable list,
// and the order itself is the user's intent. So "only swapping the order of two aliases" also counts
// as a change, needing admin.
// That is right: order does not affect the result here, but it is still a write, and no special case
// is needed for it.
func (s *Service) ensureEnvAliasesUnchanged(
	ctx context.Context, current apimodel.Agent, incoming []apimodel.AgentEnvAlias,
) error {
	if slices.Equal(
		normalizeAgentEnvAliases(incoming),
		normalizeAgentEnvAliases(current.EnvAliases),
	) {
		return nil
	}
	ok, err := s.actorIsAdmin(ctx, current.WorkspaceID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s", persistmodel.ErrAccessDenied, ErrEnvAliasesNeedAdmin)
	}
	return nil
}

// ensureCanSetEnvAliases blocks "carrying credential aliases directly when creating an agent".
//
// The same shape as ensureCanSetIntegrations: **an empty list is let through** (that is the create
// form's default),
// and only non-empty needs admin. ⚠️ This is the easiest one to miss: it is not in the "edit" mental
// model,
// and without blocking, members could bypass the whole restriction by simply "creating a new one".
func (s *Service) ensureCanSetEnvAliases(
	ctx context.Context, workspaceID string, aliases []apimodel.AgentEnvAlias,
) error {
	if len(aliases) == 0 {
		return nil
	}
	ok, err := s.actorIsAdmin(ctx, workspaceID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s", persistmodel.ErrAccessDenied, ErrEnvAliasesNeedAdmin)
	}
	return nil
}

// actorIsAdmin reports whether the current identity is admin/owner in a workspace.
//
// ⚠️ **When ctx has no identity it returns true (let through)**, for the same reason as that part in
// GetWorkspaceByKey:
// this path is also called directly by unit tests (not via HTTP), and blocking would make a batch of
// tests unrelated to authorization fail here;
// while all production endpoints are behind the auth middleware, so ctx always has an identity. If
// a system-driven path later needs to create
// agents with authorizations, it should be let through too: those paths have no "who".
//
// The role goes through RoleOf, **the same function** authz.Guard uses for permission checks: there
// will be no
// self-contradiction like "Guard let it through but this blocks it".
func (s *Service) actorIsAdmin(ctx context.Context, workspaceID string) (bool, error) {
	actor := auth.ActorFrom(ctx)
	if actor == "" {
		return true, nil
	}
	role, ok, err := s.members.RoleOf(ctx, workspaceID, actor)
	if err != nil {
		// A database hiccup must be thrown upward. Treating it as "not admin" would describe a
		// database fault as "you have no permission",
		// a false statement the user cannot do anything about (same as that part in RoleOf).
		return false, err
	}
	if !ok {
		return false, nil
	}
	return role == apimodel.RoleOwner || role == apimodel.RoleAdmin, nil
}

// AgentsBoundToFeishuApp lists the agent IDs bound to a Feishu app, in binding order.
//
// Used to fill App.BoundAgentIDs on the Feishu side. The order is the agents' storage order, not a
// set:
// the frontend's isDirty is a bare JSON.stringify comparison, and order jitter would make the page
// show "unsaved changes".
func (s *Service) AgentsBoundToFeishuApp(ctx context.Context, feishuAppID string) ([]string, error) {
	return s.store.AgentsBoundTo(ctx, feishuAppID)
}

// UnbindFeishuApp removes a Feishu app from the bindings of all agents.
//
// Called by the feishu module after deleting an app, to avoid dangling references. **Does not bump
// version, does not touch
// updatedAt**: it copies the behavior of deleteFeishuApp in LocalApiClient (api.ts:501-503
// changes feishuAppIds directly, not through updateAgent). The difference is observable: bumping
// would make
// the frontend think someone edited the agent.
//
// ⚠️ **Likewise UpdatedBy is not written**, and the reason here is even stronger: this path **has no
// identity at all** (it is triggered by the feishu module after deleting an app, not by anyone's
// click), and auth.ActorFrom here is always
// an empty string. Writing it would only **wipe the existing trail to empty**, so "who changed this
// agent" would be lost out of nowhere,
// with no error. Cascade cleanup is not a user edit; this guards the same thing as "no bump" above.
//
// The cleanup is **across all workspaces**, not filtered by workspace (also copied).
func (s *Service) UnbindFeishuApp(ctx context.Context, feishuAppID string) error {
	ids, err := s.store.AgentsBoundTo(ctx, feishuAppID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		a, err := s.store.GetAgent(ctx, id)
		if err != nil {
			continue
		}
		a.FeishuAppIDs = filterSlice(a.FeishuAppIDs, func(x string) bool { return x != feishuAppID })
		if _, err := s.store.SaveAgent(ctx, a); err != nil {
			return err
		}
	}
	return nil
}

// ensureFeishuAppsFree checks that these Feishu apps are not held by **another** agent.
//
// # Why Feishu apps must be 1:1 exclusive
//
// An inbound message can have only one answerer. When bound 1:N, the inbound side can only pick one
// of several agents
// (the historical implementation took the first query result, i.e. the one first by ord), so "who
// answers"
// is decided by row order: the user sees two agents both showing connected on the page, yet only one
// really replies,
// and no UI can tell them which one. This is not a tradeoff, it is a bug.
//
// selfID is the agent being saved; an empty string means "no ID yet" (new). It must be excluded;
// for why see the call site in UpdateAgent.
//
// # This cannot stop concurrency, and the database cannot help either
//
// When two instances bind the same app to different agents at the same time, both find "nobody holds
// it" and each writes successfully:
// the classic TOCTOU. By this repo's rules (CLAUDE.md) such things should be "decided by a unique
// constraint", but **that cannot be done here**:
// the bindings live in the jsonb array feishuAppIds in agents.doc, and PostgreSQL
//
//   - does not allow indexes on set-returning functions (jsonb_array_elements_text fails directly with
//     "set-returning functions are not allowed in index expressions");
//   - text[] has no default GiST operator class, so `exclude using gist (arr with &&)`
//     does not work either (btree_gist only covers scalars).
//
// Really letting the database decide would need the relation pulled out into a separate "one row
// per binding" table, with feishu_app_id as primary key.
// That is the right direction, but it would give the same relation two storage locations (doc and
// that table),
// which must be kept in sync in SaveAgent / DeleteAgent / Reset, a separate cost.
//
// So the current state is: **with one instance this check is reliable; with multiple instances there
// is a very narrow race window**
// (two people saving bindings of the same app almost at the same time). The consequence in the
// window is not data corruption, just another
// 1:N, and the inbound side already tolerates it and alerts (see resolveBoundAgent in inbound.go).
func (s *Service) ensureFeishuAppsFree(ctx context.Context, selfID string, appIDs []string) error {
	for _, appID := range appIDs {
		holders, err := s.store.AgentsBoundTo(ctx, appID)
		if err != nil {
			// If we cannot look it up we dare not let it through: letting through means allowing a
			// takeover, and the failure here is a database hiccup that a retry can fix.
			return err
		}
		for _, holder := range holders {
			if holder == selfID {
				continue
			}
			// The error must carry the holder's **name**, not just the ID: users see names in the UI,
			// and an agent_xxx would make them dig for which one it is. Fall back to the ID if the name
			// cannot be found.
			name := holder
			if other, err := s.store.GetAgent(ctx, holder); err == nil &&
				strings.TrimSpace(other.Name) != "" {
				name = other.Name
			}
			return fmt.Errorf("%w: 该飞书应用已被智能体「%s」绑定，请先在那边解绑", persistmodel.ErrConflict, name)
		}
	}
	return nil
}

func validateAgentDraft(d apimodel.AgentDraft) error {
	if strings.TrimSpace(d.Name) == "" {
		return fmt.Errorf("%w: Agent 名称不能为空", persistmodel.ErrInvalidArgument)
	}
	if strings.TrimSpace(d.Model.ID) == "" {
		return fmt.Errorf("%w: 必须指定模型", persistmodel.ErrInvalidArgument)
	}
	// Ark limits system to at most 100000 characters
	if len([]rune(d.System)) > 100000 {
		return fmt.Errorf("%w: 系统提示词超过 100000 字符", persistmodel.ErrInvalidArgument)
	}
	// Status can only be those two values. **This used to not be validated at all**, so any string
	// could be stored,
	// while IsDisabled only knows "disabled", and a misspelled "disable" would silently count as
	// enabled.
	//
	// An empty string must pass: it means "not sent"; CreateAgent fills in active, and UpdateAgent
	// keeps the old value
	// (see that `!= ""` below). Older frontends and internal calls may not carry this key.
	if d.Status != "" && d.Status != apimodel.StatusActive && d.Status != apimodel.StatusDisabled {
		return fmt.Errorf("%w: 状态只能是 active 或 disabled", persistmodel.ErrInvalidArgument)
	}
	if err := validateAgentEnvs(d.Envs); err != nil {
		return err
	}
	if err := validateAgentEnvAliases(d.EnvAliases, d.Envs); err != nil {
		return err
	}
	return nil
}

// validateAgentEnvs validates custom environment variables.
//
// **Blocked here and not only in the frontend**: the API is public (external agents can call it with
// an API Key),
// so frontend validation is only experience, and core validation is the contract. And none of these
// are purism:
//
//   - invalid keys have different consequences, none pointing at this form (see the comment on
//     apimodel.IsValidEnvKey);
//   - duplicate keys would make the injection result **depend on map iteration order**, i.e.
//     nondeterministic;
//   - exceeding the limit would make **every command** in the container fail with E2BIG, with an
//     error not pointing at env at all.
func validateAgentEnvs(envs []apimodel.AgentEnv) error {
	if len(envs) > apimodel.MaxAgentEnvs {
		return fmt.Errorf("%w: 环境变量最多 %d 条，当前 %d 条",
			persistmodel.ErrInvalidArgument, apimodel.MaxAgentEnvs, len(envs))
	}
	total := 0
	seen := make(map[string]bool, len(envs))
	for _, e := range envs {
		key := strings.TrimSpace(e.Key)
		if !apimodel.IsValidEnvKey(key) {
			// The error must say "what the valid shape is", otherwise users can only try one by one.
			return fmt.Errorf(
				"%w: 环境变量名 %q 不合法。只能用字母、数字、下划线，且不能以数字开头",
				persistmodel.ErrInvalidArgument, e.Key)
		}
		// **Duplicate check is case-sensitive**: environment variables are case-sensitive on Linux,
		// FOO and foo are two different variables and should not be blocked.
		if seen[key] {
			return fmt.Errorf("%w: 环境变量 %q 重复了。同名变量只能有一条——"+
				"留两条的话最终生效哪个取决于遍历顺序，不确定", persistmodel.ErrInvalidArgument, key)
		}
		seen[key] = true
		// ⚠️⚠️ **Platform-reserved variable names are always rejected; this is a security boundary,
		// not purism.**
		//
		// Without it a user could enter `AMA_TOKEN=<a stolen key>`, so the container would run as
		// **the identity they chose**;
		// trading "fill in a form" permission for "call the platform as any identity", a classic
		// privilege escalation.
		//
		// The injection point (shelltools) has another filter; having both is deliberate: this one
		// gives the user **a clear
		// error** ("entered it but it did not take effect" is much harder to debug than "rejected
		// when entering"), and that one covers dirty data
		// and cases bypassing the write path. Either one failing alone should not immediately become a
		// hole.
		if shelltools.IsPlatformReservedEnv(key) {
			return fmt.Errorf("%w: %q 是平台保留的环境变量，不能自定义。"+
				"它由平台按当前会话自动注入（见 Agent 的「授权」设置）",
				persistmodel.ErrInvalidArgument, key)
		}
		if len(e.Value) > apimodel.MaxAgentEnvValueBytes {
			return fmt.Errorf("%w: 环境变量 %q 的值有 %d 字节，超过上限 %d 字节",
				persistmodel.ErrInvalidArgument, key, len(e.Value), apimodel.MaxAgentEnvValueBytes)
		}
		total += len(key) + len(e.Value)
	}
	if total > apimodel.MaxAgentEnvTotalBytes {
		return fmt.Errorf("%w: 环境变量总量 %d 字节，超过上限 %d 字节",
			persistmodel.ErrInvalidArgument, total, apimodel.MaxAgentEnvTotalBytes)
	}
	return nil
}

// normalizeAgentEnvs puts the env list into a storable shape.
//
// It does only two things: trim whitespace around keys, and ensure non-nil.
//
// ⚠️ **No sorting on purpose** (the opposite of normalizeIntegrationIDs): this is a user-editable
// list,
// and reordering would make the row they are editing jump away. The order itself is the user's
// intent.
func normalizeAgentEnvs(in []apimodel.AgentEnv) []apimodel.AgentEnv {
	out := make([]apimodel.AgentEnv, 0, len(in))
	for _, e := range in {
		out = append(out, apimodel.AgentEnv{
			Key:    strings.TrimSpace(e.Key),
			Value:  e.Value,
			Secret: e.Secret,
		})
	}
	return out
}

// validateAgentEnvAliases validates credential aliases, and checks their conflicts with custom env.
//
// The same basis as validateAgentEnvs: **blocked in core, not only in the frontend**, because the API
// is public.
//
// ⚠️ It takes envs to check "the target name collides with some custom env": in that case which one
// wins
// depends on merge order, a nondeterministic behavior, which must be rejected at write time rather
// than left for the user to guess.
func validateAgentEnvAliases(aliases []apimodel.AgentEnvAlias, envs []apimodel.AgentEnv) error {
	if len(aliases) > apimodel.MaxAgentEnvs {
		return fmt.Errorf("%w: 凭据别名最多 %d 条，当前 %d 条",
			persistmodel.ErrInvalidArgument, apimodel.MaxAgentEnvs, len(aliases))
	}
	envKeys := make(map[string]bool, len(envs))
	for _, e := range envs {
		envKeys[strings.TrimSpace(e.Key)] = true
	}
	seen := make(map[string]bool, len(aliases))
	for _, a := range aliases {
		key, from := strings.TrimSpace(a.Key), strings.TrimSpace(a.From)
		if !apimodel.IsValidEnvKey(key) {
			return fmt.Errorf(
				"%w: 别名的变量名 %q 不合法。只能用字母、数字、下划线，且不能以数字开头",
				persistmodel.ErrInvalidArgument, a.Key)
		}
		// ⚠️⚠️ **The target name must not be a platform-reserved key; this is a security boundary,
		// not purism.**
		//
		// Without it a user could declare `<some platform credential variable> ← AMA_TOKEN`,
		// swapping the platform identity in the container for another credential; the same escalation
		// hole as bypassing platformReservedEnvs,
		// just through a different entry. The injection point (shelltools) has a second guard.
		if shelltools.IsPlatformReservedEnv(key) {
			return fmt.Errorf("%w: %q 是平台保留的环境变量，不能作为别名的名字。"+
				"别名是「给平台凭据起个新名字」，不能反过来覆盖平台自己的变量",
				persistmodel.ErrInvalidArgument, key)
		}
		// The source must be in the fixed list. **The error must list the options**, otherwise users
		// can only try one by one.
		if !shelltools.IsAliasableEnv(from) {
			return fmt.Errorf("%w: %q 不是可引用的平台凭据。可选：%s",
				persistmodel.ErrInvalidArgument, a.From, strings.Join(shelltools.AliasableEnvKeys(), " / "))
		}
		if seen[key] {
			return fmt.Errorf("%w: 别名 %q 重复了。同名只能有一条——"+
				"留两条的话最终生效哪个取决于遍历顺序，不确定", persistmodel.ErrInvalidArgument, key)
		}
		seen[key] = true
		// Name collision with a custom env: likewise "which one wins is nondeterministic", reject
		// directly.
		if envKeys[key] {
			return fmt.Errorf("%w: %q 既是别名又是自定义环境变量，只能留一个——"+
				"两者都在时最终生效哪个不确定", persistmodel.ErrInvalidArgument, key)
		}
	}
	return nil
}

// normalizeAgentEnvAliases puts the alias list into a storable shape.
//
// The same basis as normalizeAgentEnvs: only trim whitespace and ensure non-nil, **no sorting**
// (a user-editable list; reordering would make the row being edited jump away).
func normalizeAgentEnvAliases(in []apimodel.AgentEnvAlias) []apimodel.AgentEnvAlias {
	out := make([]apimodel.AgentEnvAlias, 0, len(in))
	for _, a := range in {
		out = append(out, apimodel.AgentEnvAlias{
			Key:  strings.TrimSpace(a.Key),
			From: strings.TrimSpace(a.From),
		})
	}
	return out
}

// normalizeIntegrationIDs puts the authorization id list into a storable shape:
// trim whitespace, dedupe, **sort**, and never nil.
//
// # Why it must be sorted
//
// The frontend's isDirty is a bare `JSON.stringify` comparison, and `["a","b"]` and `["b","a"]` are
// not equal.
// The order users check depends on the order they click; without sorting, "checked A then B" and
// "checked B then A"
// would be stored in two byte shapes, so after saving, reading back would not equal the draft:
// **the save button stays lit forever**,
// and does not go away after clicking. Such symptoms give no error, only making people think the
// save did not take effect.
//
// # Why dedupe
//
// Duplicate ids do not break injection (the injection side treats them as a set), but they add
// another unstable shape to that byte comparison,
// and the list page would show "3 authorizations" when there are only 2.
//
// **Deliberately not checking whether ids really exist**: the provider list is registered at
// runtime, and this is the write path.
// Checking would reject the normal release order "save the config first, ship that provider
// later";
// while a dangling id only means "not found at injection, skipped", and the injection side logs it.
// Same basis as how SkillIDs handles dangling references.
func normalizeIntegrationIDs(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, id := range in {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

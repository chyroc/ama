// Store interface: the storage contract of this module.
//
// # Neither implementation lives in this package
//
//   - PG     → repository/dao/coredao (PGStore)
//   - memory → repository/dao/coremem (MemStore, the production fallback with AMA_STORE=mem)
//
// **Keeping the interface in core is deliberate**: declaring the interface on the consumer side is
// the direction dependency inversion asks for (same rule as infra/ not importing core). Moving it
// into repository/ would fall back to a classic three-layer setup.
//
// ⚠️ coremem uses no symbol from core, so that core's own internal tests can build a MemStore
// without an import cycle. Read that package's file header before changing it.
//
// The interface is deliberately designed as if "it will be SQL":
//
//   - write methods return ErrConflict on a unique key collision, matching a unique index conflict;
//   - read methods always return **deep copies**, to avoid behavior differences where the memory
//     implementation can be changed in place but the SQL implementation cannot;
//   - no entry point exposes "get a pointer and change it directly".
//
// Every "list" returns a non-nil slice (possibly empty), because the frontend calls .map()
// directly and null would crash.

package core

import (
	"context"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	agentsvc "github.com/chyroc/ama/server/service/agent"
	hostsvc "github.com/chyroc/ama/server/service/host"
	issuesvc "github.com/chyroc/ama/server/service/issue"
	revisionsvc "github.com/chyroc/ama/server/service/revision"
	runsvc "github.com/chyroc/ama/server/service/run"
	schedulesvc "github.com/chyroc/ama/server/service/schedule"
	suggestionsvc "github.com/chyroc/ama/server/service/suggestion"
	userfilesvc "github.com/chyroc/ama/server/service/userfile"
	workspacesvc "github.com/chyroc/ama/server/service/workspace"
)

// RunQuery / UsageQuery / EventQuery have moved to model/persistmodel.
//
// They are Store inputs and do not go into HTTP responses. Putting them in a leaf package keeps
// the Store interface signatures from depending on core; see that package's comment for why.

// Store is the storage contract of this module.
//
// Methods are grouped by resource domain. Every "list" returns a non-nil slice (possibly empty),
// because the frontend calls .map() directly and null would crash.
type Store interface {
	// -- Workspaces / members --------------------------------------------------
	//
	// Declarations follow the domain: those methods are in service/workspace/store.go, only embedded here.
	workspacesvc.Store

	// -- Agent --------------------------------------------------------------
	//
	// **The declarations have moved with the domain** (same as issue / suggestion / revision /
	// schedule / workspace / run): those 5 methods plus AgentsBoundTo now live in
	// service/agent/store.go (`agent.Store`), and this file only embeds one line. The reason to embed
	// instead of a dynamic assertion is word for word the same as the issuesvc.Store section below:
	// decorators missing a method still compile, the failure is delayed until wiring time, and the
	// error does not point at the real cause at all.
	//
	// ⚠️ **agentsvc.Store also has GetWorkspace, overlapping with the workspacesvc.Store group above**.
	// That is not a duplicate declaration error: Go allows methods of embedded interfaces to share a
	// name and signature (the signatures must match exactly, and changing one is a compile error,
	// which is exactly the gate we want). The agent domain reads workspaces only to confirm the
	// workspace exists when creating an agent; see the file header of service/agent/store.go. In the
	// same way its ListAgents / GetAgent overlap with the runsvc.Store / schedulesvc.Store groups.
	//
	// ⚠️ **The 6 agentsync methods did not move** (they are still in the "Async tasks that sync agents
	// to Ark" section below): the enqueue side and the resident runner AgentSyncer share a pair of
	// in-process state fields, and the runner depends on two domains not yet split out, ark and chat,
	// so that whole block is left for the next step.
	agentsvc.Store

	// -- Host executor (host) ------------------------------------------------
	//
	// Declarations follow the domain: those 17 methods live in service/host/store.go (`host.Store`),
	// and this file only embeds one line. The reason it must stay embedded instead of a dynamic
	// assertion is word for word the same as the issuesvc.Store section above: core.Store is the
	// embedded type of a pile of decorators; if a method disappears from the interface the decorators
	// still compile, and the failure is delayed until wiring time and does not point at the real cause.
	hostsvc.Store

	// -- Account-level personal files (userfile) ------------------------------
	//
	// Those 5 methods live in service/userfile/store.go (`userfile.Store`), and this file only embeds
	// one line; a dynamic assertion instead of embedding would let decorators silently lose methods,
	// same reason as the hostsvc.Store section above.
	userfilesvc.Store

	// -- Skills ---------------------------------------------------------------

	ListSkills(ctx context.Context, workspaceID string) ([]apimodel.Skill, error)
	GetSkill(ctx context.Context, id string) (apimodel.Skill, error)
	// SkillNameTaken checks whether a skill name already exists **within this workspace**.
	SkillNameTaken(ctx context.Context, workspaceID, name string) (bool, error)
	CreateSkill(ctx context.Context, s apimodel.Skill) (apimodel.Skill, error)
	SaveSkill(ctx context.Context, s apimodel.Skill) (apimodel.Skill, error)
	// DeleteSkill deletes a skill; not existing is not an error (idempotent). **Attached files must be
	// deleted with it**, otherwise orphan attachments keep taking space in the DB, and get mixed in
	// when a later skill reuses the same id.
	DeleteSkill(ctx context.Context, id string) error

	// PutSkillFiles replaces all attached files of a skill.
	//
	// **Full replace, not incremental append**: an upload means "this version of the skill bundle is
	// exactly these files". With incremental behavior, re-uploading a bundle that dropped a script
	// would leave that script in the DB and keep injecting it into containers: the agent sees a file
	// that should be gone, while the UI does not show it.
	//
	// Empty files means clear. The implementation must do "delete then insert" in one transaction,
	// with no in-between state.
	PutSkillFiles(ctx context.Context, skillID, workspaceID string, files []apimodel.SkillFile) error

	// SkillFiles gets all attached files of a skill, **including content bytes**, sorted by path
	// ascending.
	//
	// Keeping it separate from GetSkill is deliberate: GetSkill reads doc, and the Files in doc only
	// have path + size (see the comment on apimodel.SkillFile). Only the container inject path needs
	// the bytes; making it do one explicit extra query is better than making every list carry
	// attachment bytes.
	SkillFiles(ctx context.Context, skillID string) ([]apimodel.SkillFile, error)

	// -- Issues (workspace level) ----------------------------------------------
	//
	// **The declarations have moved with the domain**: those 20 methods now live in
	// service/issue/store.go (`issue.Store`), and this file only embeds it in one line. Adding a Store
	// method for issue **no longer requires changing this file**, which is exactly the conflict
	// surface this split was meant to remove (this file is the path every resource domain goes through).
	//
	// ⚠️ **It must stay embedded; a dynamic assertion in New alone is not enough.** core.Store is the
	// embedded type of a pile of decorators (flakyRunStore and others in tests, `struct { Store }`).
	// If a method disappears from this interface, the decorators lose it too, and **they still
	// compile**. The failure is delayed until wiring time, and shows up as "some test says Store does
	// not implement the issue domain", a message that does not point at the real cause at all.
	// Embedding keeps the compiler in charge, and the `var _ core.Store` line for PGStore keeps
	// proving it implements issue.
	issuesvc.Store

	// -- Product suggestions (Suggestion, platform level) --------------------------
	//
	// ⚠️ **The third kind of "something is wrong" thing**; it is not the same as issue above, nor the
	// "problem feedback" group below:
	//
	//	feedback group      collected by machines, cross-workspace, deduplicated, only the dev team can read
	//	issuesvc.Store      written by people, a **workspace-level** entity, workspace members read by role
	//	suggestionsvc.Store written by people, **platform-level** opinions, the author sees their own + the platform team sees all
	//
	// The easiest mistake: **no method in this group takes a workspaceID parameter**. Suggestions do
	// not belong to any workspace, and their table does not even have a workspace_id column.
	// See core/README-suggestion.md for why.
	//
	// **The declarations have moved with the domain** (same as issue): those 16 methods now live in
	// service/suggestion/store.go, and this file only embeds one line. The reason to embed instead of
	// a dynamic assertion is word for word the same as the issuesvc.Store section above: decorators
	// missing a method still compile, the failure is delayed until wiring time, and the error does
	// not point at the real cause at all.
	suggestionsvc.Store

	// -- Files (metadata after attachment bytes moved to object storage) ---------------
	//
	// ⚠️ **Only metadata, not bytes**: the bytes are in object storage, handled by BlobStore
	// (core/blob.go). This group stores "which bucket and key those bytes are at".
	//
	// Its relation to the two groups above is **being referenced**: issue_attachments /
	// suggestion_attachments each have a file_id column pointing here. Deleting an attachment only
	// deletes the business table row, and the files row stays. That is not an oversight but a
	// deliberate trade-off; see the file header of repository/schema/files.sql.
	//
	// The methods are declared in store_file.go, only embedded here.
	fileStore

	// -- Ark credentials -------------------------------------------------------

	// GetArkSecret gets the credential with the plain text key; returns ErrNotFound if not configured.
	GetArkSecret(ctx context.Context, workspaceID string) (apimodel.ArkSecret, error)
	PutArkSecret(ctx context.Context, sec apimodel.ArkSecret) error
	// DeleteArkSecret deletes the credential; not existing is not an error (idempotent).
	DeleteArkSecret(ctx context.Context, workspaceID string) error

	// -- Feishu user OAuth credentials -------------------------------------------
	//
	// ⚠️ **This is the only credential source whose "authorization state is stored in the DB"**. It
	// looks like it conflicts with the rule in the integration.go file header, "state is always
	// queried live, never stored", but it does not: that rule is about **another platform's state**,
	// while what is stored here is **the credential itself**. refresh_token is something Feishu gives
	// us only once; without storing it we can never renew (the user would rescan the code every two
	// hours). Same test as integration_ama.go: "who holds the authority over this thing".
	//
	// ⚠️ Tokens going in and out of these methods are all **plain text**; encryption and decryption are
	// inside the Store implementation (sealKey/openKey in coredao), same as ArkSecret.

	// GetLarkUserToken gets a person's credential for a Feishu app; returns ErrNotFound if never authorized.
	GetLarkUserToken(ctx context.Context, actor, larkAppID string) (persistmodel.LarkUserToken, error)
	// PutLarkUserToken overwrites the credential (upsert).
	PutLarkUserToken(ctx context.Context, tok persistmodel.LarkUserToken) error
	// DeleteLarkUserToken deletes the credential; not existing is not an error (idempotent).
	//
	// **This is how "revoke authorization" is implemented**: after deleting, the user must scan the
	// code again, so it is a real revocation, not "delete a row and then automatically recreate the
	// same one next time". The latter would not be what its name says; see that lesson in
	// integration_ama.go.
	DeleteLarkUserToken(ctx context.Context, actor, larkAppID string) error
	// ListLarkUserTokens lists all credentials of a person (they may have authorized several Feishu apps).
	//
	// Ordered by lark_app_id, stable. An empty result is not an error.
	ListLarkUserTokens(ctx context.Context, actor string) ([]persistmodel.LarkUserToken, error)

	// -- "Which Feishu app do I use by default" ------------------------------------
	//
	// ⚠️ **Account level, the primary key is only actor** (see that table's comment). It answers "which
	// one to use when there is no workspace context": showing the card, starting authorization, and
	// revoking authorization.
	//
	// **Injection does not look at it**: that path is pinned to the run's workspace
	// (feishu.ResolveApp takes LarkAppQuery.WorkspaceID). Making injection look at this table too
	// would bring the "cross-workspace mismatch" bug back in a different place.

	// GetLarkUserAppPref gets the Feishu app id this person chose; returns ErrNotFound if never chosen.
	GetLarkUserAppPref(ctx context.Context, actor string) (string, error)
	// PutLarkUserAppPref records the Feishu app this person chose (upsert).
	PutLarkUserAppPref(ctx context.Context, actor, larkAppID string) error

	// -- In-between state of the device code for Feishu user authorization ---------------
	//
	// ⚠️ **It must be stored in the DB, not in process memory**: production runs multiple instances,
	// and the two requests "start authorization" and "poll the result" often land in different
	// processes. In memory it shows up as "I clicked authorize but the page keeps spinning", and only
	// sometimes (it works when you are lucky and land on the same instance), the hardest kind to debug.
	// See the comment on that table in repository/schema/core_larkuser_pg.sql.

	// PutLarkAuthSession records a device code waiting for authorization (replaces the old one for the same (actor, app)).
	PutLarkAuthSession(ctx context.Context, sess persistmodel.LarkAuthSession) error
	// GetLarkAuthSession gets the device code waiting for authorization; returns ErrNotFound if none or
	// expired.
	//
	// **Expired rows must count as missing** (the implementation filters by expires_at), otherwise the
	// user keeps polling with a device_code Feishu has long since voided, and the error does not point
	// at "start over".
	GetLarkAuthSession(ctx context.Context, actor, larkAppID string) (persistmodel.LarkAuthSession, error)
	// DeleteLarkAuthSession deletes the pending authorization record; not existing is not an error (idempotent).
	DeleteLarkAuthSession(ctx context.Context, actor, larkAppID string) error

	// -- Account-level Ark credential (the one "configured once globally, referenced by workspaces") ----------------

	// GetGlobalArkSecret gets a person's account-level credential; returns ErrNotFound if never configured.
	//
	// **The key is the employee email**. When the workspace's Source is inherit, reads fall back to
	// here (resolveArkSecret in core/ark.go). Workspaces keep no copy, so changing this one takes
	// effect right away for every workspace referencing it.
	GetGlobalArkSecret(ctx context.Context, email string) (apimodel.GlobalArkSecret, error)

	// PutGlobalArkSecret stores/overwrites a person's account-level credential.
	//
	// **One upsert, not check then write**: with multiple instances check then write is TOCTOU (see
	// PutOnboarding).
	PutGlobalArkSecret(ctx context.Context, sec apimodel.GlobalArkSecret) error

	// DeleteGlobalArkSecret deletes the account-level credential; not existing is not an error (idempotent).
	//
	// ⚠️ **It does not change any workspace's source**. Workspaces referencing it end up "referencing a
	// credential that does not exist", which reads as not configured. This is deliberate; see
	// DeleteGlobalArkCredential in core/ark.go.
	DeleteGlobalArkSecret(ctx context.Context, email string) error

	// -- Agent-level Ark credential (the most specific of the three levels) ----------------------------

	// GetAgentArkSecret gets an agent's own key; returns ErrNotFound if never configured.
	//
	// **"No row" means "follow the workspace setting"**, not "not configured": the agent level has no
	// inherit storage state (a row = custom, no row = follow); see apimodel.AgentArkCredential for why.
	//
	// The fallback chain is agent → workspace → account level, and the only entry point is
	// resolveArkSecretForAgent in core/ark.go. **Lower levels keep no copy of upper levels**, so
	// changing an upper level takes effect right away.
	GetAgentArkSecret(ctx context.Context, agentID string) (apimodel.AgentArkSecret, error)

	// PutAgentArkSecret stores/overwrites an agent's credential.
	//
	// **One upsert, not check then write**: with multiple instances check then write is TOCTOU (see
	// PutOnboarding).
	PutAgentArkSecret(ctx context.Context, sec apimodel.AgentArkSecret) error

	// DeleteAgentArkSecret deletes the agent-level credential; not existing is not an error (idempotent).
	//
	// After deleting, this agent falls back to the workspace level. That is the "use workspace
	// settings" action in the UI.
	DeleteAgentArkSecret(ctx context.Context, agentID string) error

	// -- Onboarding (account level, the only group not under a workspace) -------------------------

	// GetOnboarding gets a person's onboarding state; returns ErrNotFound if there is no record.
	//
	// **The key is the employee email**, not the workspace id: the onboarding state belongs to the
	// person and has nothing to do with workspaces (see the onboarding.go file header). The caller
	// (Service) translates ErrNotFound into the zero value of a "brand new user" and does not pass it up.
	GetOnboarding(ctx context.Context, email string) (persistmodel.OnboardingRecord, error)

	// PutOnboarding records the onboarding state: merge if present, insert if not.
	//
	// **It must be a single upsert, not check then write.** Production runs multiple instances, and
	// the same person's two clicks may land in two processes. Check then write is TOCTOU: both sides
	// see "no record" and insert together, and the slower one hits the unique constraint and returns
	// 500, while the user did nothing wrong.
	//
	// **Merge behavior**: a zero-value timestamp in the input **does not overwrite** the stored value.
	// That way both moments of "skipped first, then finished it later on purpose" are kept (the PG
	// implementation uses coalesce, the memory implementation checks zero values explicitly).
	PutOnboarding(ctx context.Context, rec persistmodel.OnboardingRecord) error

	// -- User preferences (account level, same as the previous group) ------------

	// GetUserPreferences gets a person's language and appearance; returns ErrNotFound if there is no
	// record.
	//
	// **The key is the employee email**, same as the two onboarding methods. The caller (Service)
	// translates ErrNotFound into the default (zh-CN + system) and does not pass it up: "never set"
	// is the most common normal case, and going through the error channel would leave the frontend
	// stuck in the loading state forever.
	GetUserPreferences(ctx context.Context, email string) (persistmodel.UserPreferencesRecord, error)

	// PutUserPreferences stores a person's preferences: overwrite the whole row if present, insert if
	// not.
	//
	// **It must be a single upsert, not check then write**, for the same reason as PutOnboarding
	// (multi-instance TOCTOU).
	//
	// ⚠️ **Unlike PutOnboarding, this overwrites the whole row with no merging.** That one needs
	// coalesce because completed_at / skipped_at each record a separate thing that happened; here the
	// two fields are always submitted together (the frontend sends the complete preferences object),
	// and merging would drop a change like "from dark back to follow system" as "no new value given".
	PutUserPreferences(ctx context.Context, rec persistmodel.UserPreferencesRecord) error

	// -- Problem feedback (cross-workspace, unrelated to runs) ---------------------

	// PutFeedback stores one feedback record.
	//
	// **Level one (auto) is deduplicated by DedupKey**: when it hits an existing one, no new row is
	// added; instead occurrences +1 and last_seen_at moves to now. This is how rate limiting is
	// implemented; see schema_feedback_pg.sql and the top of core/feedback.go.
	//
	// **Level two (user) is not deduplicated**: DedupKey is the empty string, which PG writes as NULL
	// (NULLs are never equal in a unique index), so each one adds a new row. Two clicks by the user
	// should give two rows: losing something the user submitted on purpose is much worse than storing
	// one extra.
	//
	// Like PutOnboarding, it **must be a single upsert, not check then write**: with multiple
	// instances check then write is TOCTOU, two processes both see "this bucket is empty" and insert
	// together, and the slower one hits the unique constraint and returns 500. And that happens right
	// during an outage (when concurrency is highest).
	PutFeedback(ctx context.Context, rec persistmodel.FeedbackRecord) error

	// ListFeedback lists feedback by conditions, and returns this page and **the total count matching
	// the conditions**.
	//
	// The total is returned separately from the page because the caller (an automation script) needs
	// to know "is there a next page", and judging by "does this page have Limit rows" is wrong when
	// the last page happens to be exactly full.
	//
	// Order is fixed: **created_at descending** (the newest problems matter most), with id as the tie
	// breaker so rows at the same moment have a stable order. An unstable order would repeat or skip
	// rows when paging, and that bug is never visible with small data.
	ListFeedback(ctx context.Context, q persistmodel.FeedbackQuery) ([]persistmodel.FeedbackRecord, int, error)

	// -- Ark resource bindings -------------------------------------------------

	// GetArkBinding gets the Ark-side counterpart of an AMA agent; returns ErrNotFound if none.
	GetArkBinding(ctx context.Context, workspaceID, agentID string) (persistmodel.ArkBinding, error)
	// PutArkBinding overwrites the binding (overwrite if present, insert if not; there is no "does not exist" error).
	PutArkBinding(ctx context.Context, b persistmodel.ArkBinding) error
	// ListArkBindings lists all bindings in a workspace, in creation order.
	//
	// It is used to **reuse the Environment**: a workspace shares one Ark environment, and when a new
	// agent creates a binding it takes the environment ID from any existing binding instead of
	// creating another one.
	ListArkBindings(ctx context.Context, workspaceID string) ([]persistmodel.ArkBinding, error)
	// DeleteArkBinding deletes one binding; not existing is not an error (idempotent).
	//
	// Used to self-heal after the Ark-side resource is deleted: when a binding points at an Agent that
	// no longer exists, delete it, and the next conversation recreates one.
	DeleteArkBinding(ctx context.Context, workspaceID, agentID string) error
	// RepointArkEnvironment **specifically** repoints bindings still pointing at oldEnvID to newEnvID
	// (column and doc changed together), and returns how many rows were changed. The where clause
	// carries oldEnvID as a CAS guard: rows already on a different environment are left alone.
	//
	// Used to self-heal when "the Ark Environment was deleted": environments are reused per workspace,
	// so every sibling binding in the same workspace stores that dead environment ID. Deleting only
	// your own row would let the rebuild pick the dead environment back up from a sibling row (the
	// root cause of a production scheduled task failing once an hour, see chat.go).
	//
	// ⚠️ **It must be a targeted UPDATE, not read the whole row and Put it back**: a sibling agent's own
	// conversation may be catching up skills/prompts at the same time (which also calls
	// PutArkBinding), and read-modify-write would overwrite each other.
	RepointArkEnvironment(ctx context.Context, workspaceID, oldEnvID, newEnvID string) (int, error)

	// -- Start of consecutive Ark credential failures ------------------------------

	// RecordArkAuthFailure records one "this environment failed to authenticate with this key", and
	// returns how long this run of consecutive failures has lasted (computed by the database as
	// now() - first_failed_at).
	//
	// When the fingerprint differs from the recorded one (the user changed the key), **timing starts
	// over**. See schema/ark_auth_failures.sql.
	RecordArkAuthFailure(ctx context.Context, environmentID, workspaceID, fingerprint string) (time.Duration, error)

	// ClearArkAuthFailure deletes an environment's failure record (the key works again). Not existing is not an error.
	ClearArkAuthFailure(ctx context.Context, environmentID string) error

	// -- Session containers (Docker) ---------------------------------------------

	// GetRunSandbox gets the container used by a Run; returns ErrNotFound if none.
	//
	// **"None" is a normal result, not a failure**: plain text conversations never create a container
	// (the container is created lazily the first time a tool is really called, see
	// shared/infra/dockertools/session.go), so many Runs simply have no record. Callers must tell this
	// apart from "had a container but it is gone": the latter has a record, but container inspect
	// returns 404.
	GetRunSandbox(ctx context.Context, runID string) (persistmodel.RunSandbox, error)

	// PutRunSandbox overwrites "which container this Run uses" (overwrite if present, insert if not).
	//
	// **It must overwrite, not insert**: after a container is deleted it gets rebuilt, possibly on
	// another Docker daemon (an instance connected to a different daemon took over the session).
	// Insert behavior would hit the primary key, and after that the table would keep the id from the
	// **old daemon**, so the page would look for the container on the wrong daemon.
	PutRunSandbox(ctx context.Context, s persistmodel.RunSandbox) error

	// DeleteRunSandbox deletes one record; not existing is not an error (idempotent).
	DeleteRunSandbox(ctx context.Context, runID string) error

	// TouchRunSandboxTerminal pushes the "this container's terminal is still in use" deadline to
	// now+ttl.
	//
	// Purpose: do not pause the container while a webshell is open. Pausing is triggered by "a
	// conversation turn has been idle for 60 seconds", and that check cannot see someone typing
	// commands in the terminal (see the terminal_active_until part of schema/run_sandbox_sessions.sql).
	//
	// ⚠️ **It is not read-modify-write; it is one UPDATE, and the time is computed by the database**
	// (`now() + $2`). Two reasons, neither of them an optimization:
	//   - This runs on **every keystroke**; reading back then writing would let concurrent requests
	//     overwrite each other (same as TouchAPIKey in authdao);
	//   - wall clocks of multiple instances drift, and letting the database be the only clock is the
	//     only choice that makes sense in a distributed setup (same as TouchRunSandboxActivity).
	//
	// **A missing record is not an error** (idempotent): plain text conversations have no container
	// record, and this path should not fail because of that.
	TouchRunSandboxTerminal(ctx context.Context, runID string, ttl time.Duration) error

	// TerminalActiveSessions returns the container names (session_id column) whose "terminal is still
	// in use".
	//
	// The check is `terminal_active_until > now()`, **compared by the database** (same as above, do not
	// pull the time back into Go to compare). It returns a whole set: the caller (dockertools idle
	// cleanup) holds the handle lock, getting the full set once and checking it in memory is least
	// likely to go wrong, and the set itself is small.
	TerminalActiveSessions(ctx context.Context) ([]string, error)

	// TouchRunSandboxActivity sets the "last activity time" of this Run's container to the
	// database's now().
	//
	// The reclaim sweep uses it to decide how long the container has been idle. One UPDATE, with the
	// time computed by the database (clocks drift across instances); a missing record is not an error.
	TouchRunSandboxActivity(ctx context.Context, runID string) error

	// RunSandboxActivity gets the last activity times in bulk: coalesce(last_active_at, created_at).
	// Runs not in the DB do not appear in the result.
	RunSandboxActivity(ctx context.Context, runIDs []string) (map[string]time.Time, error)

	// -- Ark skill upload records -----------------------------------------------

	// GetArkSkillUpload checks "has this version of this skill been uploaded with this key", and
	// returns ErrNotFound if not.
	//
	// contentHash is computed by SkillFingerprint. Ark has no edit-skill API, so changed content means
	// a new Ark skill, and the key must include the version; see the comment in schema_pg.sql for
	// details.
	//
	// keyFingerprint is computed by ArkKeyFingerprint. **It is part of the key too**: an Ark skill
	// belongs to the project of the key used at upload time, so switching to another key in the same
	// workspace means uploading again (otherwise the skill_id we get does not exist in the new
	// project, and the error does not point at the real cause). The empty string marks older rows.
	GetArkSkillUpload(
		ctx context.Context, workspaceID, skillID, contentHash, keyFingerprint string,
	) (persistmodel.ArkSkillUpload, error)

	// PutArkSkillUpload records one upload result and returns **the one that actually took effect**.
	//
	// The return value is not an echo of the input: under concurrency two instances may each have
	// uploaded a copy, whoever stores first wins, and here we read the winner back so every instance in
	// the same workspace ends up pointing at the same Ark skill. The implementation must be `on
	// conflict do nothing` + read back, not an overwrite: overwriting would make two instances keep
	// changing it back and forth, and the skill_id would keep flipping.
	PutArkSkillUpload(ctx context.Context, u persistmodel.ArkSkillUpload) (persistmodel.ArkSkillUpload, error)

	// -- Run history -----------------------------------------------------------
	//
	// ⚠️ **The declarations of the three read-only methods have moved with the domain**: ListRuns /
	// GetRun / ListRunEvents now live in service/run/store.go (`run.Store`), and this file only embeds
	// it in one line. The reason to embed instead of a dynamic assertion is word for word the same as
	// the issuesvc.Store section above: core.Store is the embedded type of a pile of decorators
	// (flakyRunStore and others in tests). If a method disappears from this interface, the decorators
	// lose it too and **still compile**, and the failure is delayed until wiring time.
	//
	// ⚠️ **runsvc.Store also has ListAgents / GetAgent, overlapping with the "Agent" group above**.
	// That is not a duplicate declaration error: Go allows explicit methods and methods of embedded
	// interfaces to share a name and signature (the two signatures must match exactly, and changing one
	// is a compile error, which is exactly the gate we want). The run domain reads agents only to
	// compute two derived fields; see the file header of service/run/store.go.
	runsvc.Store

	// The methods below **did not move**; they do not belong to the read-only "run history" domain:
	//
	//	ListRunsForUsage                       usage aggregation
	//	ListActiveRuns / ClaimRunWatch /       resident subscription sweep (core/runwatcher.go)
	//	ReleaseRunWatch / ListStaleActiveRuns /
	//	RetireStaleRun
	//	CreateRun / SaveRun / AppendRunEvent   write path, belongs to the conversation domain (step 9b)
	//
	// ListRunsForUsage gets runs by workspace + time window, for usage aggregation.
	//
	// Kept separate from ListRuns instead of adding two fields to RunQuery: this path does not filter
	// by agent / status / keyword, it only wants "all runs in this workspace during this period", so PG
	// can use runs_workspace_started_idx directly. Mixing it into ListRuns would add two more branches
	// to a query that already has three, and the time window would be empty for every existing
	// caller, extra complexity for nothing.
	//
	// The window is **half-open** [Since, Until): when bucketing by day, an open right end keeps the
	// last millisecond of a day from falling into two buckets. Both ends are fixed-length ISO strings
	// (.000Z); empty means that side is unbounded.
	ListRunsForUsage(ctx context.Context, workspaceID string, q persistmodel.UsageQuery) ([]apimodel.Run, error)

	// ListActiveRuns lists sessions still running in **all workspaces**, for the server's resident
	// subscription sweep.
	//
	// # What counts as "still running"
	//
	// running / rescheduling / requires_action count as alive, idle / terminated as final. The value
	// set matches isTerminalRunStatus in feishu word for word. **requires_action in particular is not
	// final** (the agent is waiting for a tool result and will continue after the worker hands it
	// back). Missing it would make the resident subscription disconnect just as the agent is about to
	// keep talking, and that part of the reply would never be stored.
	//
	// # Why it is cross-workspace
	//
	// Same reason as ListAutoUpdateSkills: the sweep has no concept of a "current user"; it must find
	// every session that should be subscribed within the whole process's view. **Only call it from
	// inside the process, never wire it to HTTP**.
	//
	// **The order must be stable** (started_at ascending + id as tie breaker): without order by, a
	// sequential scan of a small table happens to return insert order, so a missing sort is never
	// caught in tests (see the CLAUDE.md rule "small tables do not expose a missing order by").
	// Subscribing to the ones that started first also makes the logs line up.
	ListActiveRuns(ctx context.Context) ([]apimodel.Run, error)

	// ClaimRunWatch atomically claims "this session's resident subscription is mine", and returns true
	// if claimed.
	//
	// # This is the correctness guard of resident subscriptions (gate two)
	//
	// With multiple instances, every pod scans the same batch of active sessions. Without this CAS,
	// 5 pods would each hold a subscription, using 5 times the Ark long connections for nothing.
	// **The data does not get dirty** (events are deduplicated by id), but the connections do. So this
	// is resource correctness, not data correctness.
	//
	// Redis leader election is only an optimization and can be left out entirely; correctness must
	// rest on this SQL, same as the double gate reasoning of ClaimDueSchedules.
	//
	// leaseFor is the lease length: when the holder's instance is rebuilt nobody releases it, and only
	// expiry lets someone else take over. After a successful claim you **must** call ReleaseRunWatch
	// when the subscription ends; otherwise that session waits a full lease before being subscribed
	// again (which shows up as "for a while after closing the page, events are still not stored").
	ClaimRunWatch(ctx context.Context, runID, owner string, now time.Time, leaseFor time.Duration) (bool, error)

	// ReleaseRunWatch clears the resident subscription claim lease; not existing is not an error
	// (idempotent).
	//
	// **It must be called when a subscription round ends**, same reason as ReleaseScheduleClaim:
	// without releasing, that session waits a whole lease period before it can be subscribed again.
	ReleaseRunWatch(ctx context.Context, runID string) error

	// IncrRunRescueCount adds 1 to this run's sweep rescue count, and returns the new value.
	//
	// The count is **across instances**: the pod that runs failover and the pod that finally resends
	// the result are usually different, so an in-process count would not match. The caller
	// (runrescue) picks the alert level by the return value: the 1st time is a normal fallback; from
	// the 2nd time on it means the failover→reclaim self-healing chain is broken, and it must be raised
	// to Error so a person steps in. When the run does not exist it returns 0 and ErrNotFound.
	IncrRunRescueCount(ctx context.Context, runID string) (int, error)

	// ListStaleActiveRuns lists sessions that **have had no activity for a long time but whose status
	// is still "alive"**.
	//
	// # It covers a gap nobody else owns
	//
	// Each of the three active states has a normal way out: running / rescheduling wait for Ark to
	// push the next frame, requires_action waits for a worker to hand back `user.tool_result`. **But
	// that tool_result may never come**: the worker's pod is replaced halfway through a tool run
	// (rolling release, failed health check, scale-in), the work on the Ark side is settled early by
	// StopWork, or container quota is tight and the container cannot be created. **None** of these
	// paths moves the run to a final state, so it stays at requires_action forever.
	//
	// That is not "it looks a bit ugly"; it has three real costs:
	//
	//  1. Zombies pile up in run history, and the UI and status aggregation think the session is still waiting for someone;
	//  2. **The resident subscription re-subscribes to it forever**: ListActiveRuns picks it up every 30 seconds, each
	//     subscription hangs for the full watchTurnTimeout (10 minutes) before timing out, and then the next round starts.
	//     One zombie means one Ark long connection that never stops;
	//  3. The user cannot continue the chat either: the Ark side is also stuck at requires_action, and sending
	//     user.message gets 400 RequiresActionRejected.
	//
	// # staleBefore has a lower bound, do not pick it on a whim
	//
	// The rule is "**it must be clearly longer than the longest possible silence of a live work**",
	// otherwise a session that is working normally, just with a slow step, is taken for a zombie.
	// That is a double loss: the user's answer is cut off, and **the symptom looks exactly like a
	// real zombie**, which in turn makes people think the fallback did not work.
	// The lower bound is the sum of three parts: the reclaim threshold (45 seconds) + the per-tool
	// run limit (120 seconds) + one subscription period (10 minutes). The value is runStaleAfter in
	// core/runwatcher.go.
	ListStaleActiveRuns(ctx context.Context, staleBefore string) ([]apimodel.Run, error)

	// RetireStaleRun moves a stuck session to a final state, and returns true if it really changed it
	// this time.
	//
	// # Why not "read it, change it, then SaveRun"
	//
	// That is TOCTOU: between listing candidates and writing back, the late `user.tool_result` may
	// arrive at just that moment (once tool_result arrives, applyEventToRun moves the run back to
	// running and refreshes lastActivityAt). Overwriting with SaveRun would **kill the session that
	// just came back to life**, with zero errors: the DB just shows a terminated row, and nobody knows
	// it had actually recovered.
	//
	// So the conditions are **written again** in the UPDATE's WHERE, making it a CAS:
	//
	//	status still in the active allow list       already moved to a final state by another path → not handled again
	//	lastActivityAt still before staleBefore     any new event in between makes it newer → not changed
	//
	// This also makes multiple instances exclude each other naturally: once the first instance
	// changes status, it is terminated, and the second instance's WHERE matches no row and returns
	// false. So Redis leader election here is only an **optimization** that saves useless work;
	// correctness rests on this SQL, same as the double gate of ClaimDueSchedules.
	//
	// run is the **fully computed final state** (including endedAt / durationMs / errorSummary),
	// built by the caller with apimodel.FormatISO, so timestamps have the fixed three-decimal shape.
	RetireStaleRun(ctx context.Context, run apimodel.Run, staleBefore string) (bool, error)

	// -- Queued messages waiting to be sent ------------------------------------
	//
	// ⚠️ This group handles **messages the user queued on purpose** (they know they must wait a whole
	// turn), not "automatic resend after hitting the tool run window" (a race of a few tens of
	// milliseconds, retried in place by sendEventsThroughWindow in core/chat.go). Do not merge the
	// two; see the comment on apimodel.PendingMessage.

	// EnqueuePendingMessage appends a message to the tail of a session's queue.
	//
	// Order is set by created_at + ord; the implementation must not reorder: **first in, first out is
	// the contract**. The "2nd/3rd" the user sees is this order, and they would notice right away if
	// it changed when sent.
	EnqueuePendingMessage(ctx context.Context, m apimodel.PendingMessage, createdAt time.Time) error

	// ListPendingMessages lists the messages on a session that have not been sent yet, in queue order.
	//
	// Includes failed ones: those must be shown to the user ("this one was not sent, here is the
	// original text"), they are just not sent automatically anymore. Callers tell them apart by Status.
	ListPendingMessages(ctx context.Context, runID string) ([]apimodel.PendingMessage, error)

	// ClaimNextPendingMessage atomically claims the **first** pending message on a session and
	// returns it.
	//
	// # This is the correctness guard for queue extraction
	//
	// With multiple instances, each pod's watcher comes to pull one when a turn ends. Without this
	// CAS the same message would be sent twice. **That is an error the user can see** (the agent
	// answers the same sentence twice), much worse than a duplicate subscription.
	//
	// A false second return value means "no pending message on this session", **not an error**.
	//
	// A successful claim sets status to sending and adds 1 to attempts. After sending, call
	// DeletePendingMessage; on failure call FailPendingMessage (or put it back to pending to retry).
	ClaimNextPendingMessage(ctx context.Context, runID, owner string, now time.Time, leaseFor time.Duration) (apimodel.PendingMessage, bool, error)

	// DeletePendingMessage deletes a queued message; not existing is not an error (idempotent).
	//
	// **Delete once sent, no sent state kept**: keeping it would only make the query "how many are
	// still queued on this session" slower and slower, and the fact "this was sent" is already in
	// run_events.
	DeletePendingMessage(ctx context.Context, id string) error

	// FailPendingMessage marks a queued message as unsendable and records the reason.
	//
	// The original text is kept: the user must be able to see "this one was not sent" and decide to
	// resend or rewrite. Silently dropping it is the worst way for this feature to fail: they think
	// it was sent and then wait for a reply that never comes.
	FailPendingMessage(ctx context.Context, id, reason string) error

	// CreateRun stores a new run.
	//
	// A conversation is a run (see chat.go), so this domain went from "read only" to writable.
	// ⚠️ **The write path deliberately did not move with runsvc.Store**; see the "Run history" section above.
	CreateRun(ctx context.Context, r apimodel.Run) (apimodel.Run, error)
	// SaveRun overwrites; returns ErrNotFound if it does not exist.
	SaveRun(ctx context.Context, r apimodel.Run) (apimodel.Run, error)
	// AppendRunEvent appends an event. sequence is assigned by Store (current max + 1), and it returns
	// the event with sequence filled in.
	//
	// **The sequence number is handed out by Store, not passed by the caller**: under concurrency,
	// two requests that each read the max and add one compute the same number, and on PG they hit
	// the primary key (run_id, sequence). Only inside Store can this happen under the same lock / in
	// the same SQL as the write.
	//
	// **Idempotent by Ark event id**: when the same event arrives again, no new row is appended and
	// the existing one is returned (with the sequence it got the first time). This is not an
	// optimization: an Ark session can have several subscribers at the same time, and each calls
	// this method for every frame it receives (see persistArkEvent in core/chat.go). Since this
	// method hands out the sequence itself, without idempotency the same event would take two
	// numbers, be stored twice, and the same sentence would show up several times in the UI. Both
	// implementations must keep this behavior: PG through the unique index run_events_event_id_idx,
	// MemStore by scanning before writing.
	//
	// Frames with an empty id (the kind that cannot be parsed) are not deduplicated and are appended
	// one by one as before.
	//
	// A missing run **is not an error**: Ark's event stream is async, the run may have just been
	// deleted, and failing to write an event that already happened serves no purpose.
	//
	// created reports whether **a new row was really written** this time. On a dedup hit it returns
	// false and reads back the existing row.
	//
	// # Why this return value is needed
	//
	// After dedup, "newly written" and "already there" have exactly the same shape in the return
	// value, and the caller cannot tell them apart. That does not matter for storage itself
	// (idempotency is the point), but it is fatal for **downstream code with external side effects**:
	// observability reporting hangs on this chain (see persistArkEvent in core/chat.go), and
	// production runs multiple instances, so the same session may be subscribed by several
	// instances, and the same event passes through here N times. Without this bool the event would
	// be reported N times, and the usage and call counts on the observability platform would all be
	// inflated. And it **reports no error**; the data is just quietly wrong.
	AppendRunEvent(ctx context.Context, runID string, raw []byte) (ev apimodel.RunEvent, created bool, err error)

	// -- Scheduled tasks ------------------------------------------------------
	//
	// **The declarations have moved with the domain** (same as issue / suggestion / revision): those
	// 8 methods now live in service/schedule/store.go, and this file only embeds one line. The reason
	// to embed instead of a dynamic assertion is word for word the same as the issuesvc.Store section
	// above: decorators missing a method still compile, the failure is delayed until wiring time, and
	// the error does not point at the real cause at all.
	//
	// ⚠️ Three methods in that group (ClaimDueSchedules / ReleaseScheduleClaim / SchedulesForAgent)
	// **are still used directly in this package**: the first two in core/scheduler.go (the scheduler
	// has not moved yet, see the file header of service/schedule/service.go for why), and the third
	// in core/agent.go for cascade cleanup when deleting an agent. Embedding means none of those call
	// sites need to change.
	schedulesvc.Store

	// -- Async tasks that sync agents to Ark ------------------------------------

	// EnqueueAgentSyncTask enqueues a sync task; **if the agent already has an unfinished task, no new
	// one is created**. Instead that task is marked "run one more round when done" and returned as is.
	//
	// # The whole difficulty of this method is "two quick clicks run only once"
	//
	// The decision must be made by **the database** (`insert ... on conflict do nothing` hitting the
	// partial unique index `agent_sync_tasks_active_idx`). **Do not fall back to "check then create"**:
	// that is TOCTOU. Two instances (or a user's double click landing on two instances) would each
	// see "none" and each create one, so the same agent would be synced by two tasks at once, both
	// changing the same Ark Agent.
	//
	// # Why mark it on a hit instead of just returning that task
	//
	// A hit means "a round is running or queued", and the agent that round read is very likely the
	// version **before this save**. If we did nothing, the changes from this save would only take
	// effect at the next conversation, which is exactly the symptom this mechanism exists to remove.
	// So record resync_requested, and run one more round when done.
	//
	// created true means a new task was really created (the caller uses this to decide whether to
	// Kick the local syncer).
	EnqueueAgentSyncTask(
		ctx context.Context, task persistmodel.AgentSyncTaskRow,
	) (row persistmodel.AgentSyncTaskRow, created bool, err error)

	// ClaimDueAgentSyncTasks atomically claims tasks waiting to be synced, with the same behavior as
	// ClaimDueSchedules.
	//
	// **It is also a correctness pivot, not a query method**: N instances see the same pending task
	// at the same time, and exactly one must win. The implementation must use the shape `CTE + for
	// update skip locked + business conditions rewritten in the outer query`. All three are required;
	// see the hundred-line post-mortem in store_pg_schedule.go (`where id in (subquery)` cannot stop
	// concurrency under EPQ recheck, and limit stops working).
	//
	// It is the union of two branches: pending ones (nobody has claimed them yet), and running ones
	// whose **lease has expired** (the instance that claimed it was rebuilt, and the write-back will
	// never come). The latter is where the requirement "tasks of a dead instance must not stay running
	// forever" lands.
	//
	// Claiming sets status to running, adds one to attempt_count, and sets the lease to now+leaseFor.
	ClaimDueAgentSyncTasks(
		ctx context.Context, owner string, now time.Time, leaseFor time.Duration, limit int,
	) ([]persistmodel.AgentSyncTaskRow, error)

	// FinishAgentSyncTask moves a task to a final state (succeeded / failed) and clears the lease.
	//
	// ⚠️ **It must also set finished_at**: that is the predicate of the partial unique index. Without
	// it this agent can never enqueue a new task (the unique index keeps blocking), which shows up as
	// "from now on no save is ever synced".
	FinishAgentSyncTask(
		ctx context.Context, id string, status apimodel.AgentSyncStatus,
		failureReason string, failureStage apimodel.AgentSyncStage, now time.Time,
	) error

	// RequeueAgentSyncTask puts a task back to pending so it runs another round, and clears the
	// resync mark.
	//
	// Used in two places: the user saved again while it was running (resync_requested), and a failure
	// that has not reached the retry limit yet. **It must clear the lease at the same time**,
	// otherwise the next claim is blocked by its own lease and waits a whole lease period for nothing.
	RequeueAgentSyncTask(ctx context.Context, id string, now time.Time) error

	// LatestAgentSyncTask gets an agent's most recent task (including finished ones), for frontend
	// polling.
	//
	// Returns ErrNotFound when there are no tasks at all. **That and "sync failed" are two different
	// things**, and the handler must treat them differently (the former means this agent was never
	// saved and should not be shown as an error).
	LatestAgentSyncTask(ctx context.Context, agentID string) (persistmodel.AgentSyncTaskRow, error)

	// PurgeFinishedAgentSyncTasks deletes tasks finished before before, and returns how many were
	// deleted.
	//
	// Without cleanup this table only grows. Same lesson as pgmq: **a cleanup function that nobody
	// calls** produces zero errors, and the cost is index entries that keep growing.
	PurgeFinishedAgentSyncTasks(ctx context.Context, before time.Time) (int64, error)

	// -- Entity revision history (rollback after mistakes) ------------------------
	//
	// **The declarations have moved with the domain** (same as issue / suggestion): those 5 methods
	// now live in service/revision/store.go, and this file only embeds one line. The reason to embed
	// instead of a dynamic assertion is word for word the same as the issuesvc.Store section above:
	// decorators missing a method still compile, the failure is delayed until wiring time, and the
	// error does not point at the real cause at all.
	//
	// ⚠️ That group **only has this domain's own table**, no GetAgent / GetSkill / GetSchedule.
	// Revision history touches three entities (the read API checks for 404, rollback writes back),
	// but it gets them through the three slots of revisionsvc.Entity, not through Store. **Reading
	// another domain's table** may go into your own Store; **going through another domain's UpdateX
	// gate** may not. See service/revision/entity.go.
	revisionsvc.Store

	// -- One-time entry tickets for the terminal WebSocket --------------------------
	//
	// Why it is needed: the WS connects to another domain (ama-ws), the Cookie is host-only and is
	// not sent there, and the browser's WebSocket does not support custom request headers, so the
	// credential can only go in the URL query. That means it must be a short-lived, one-time ticket
	// valid for only one run. The full reasoning is in the file header of
	// repository/schema/terminal_tickets.sql.

	// IssueTerminalTicket issues a ticket.
	//
	// The caller (the issue-ticket API) has already done authentication and authorization, so this
	// only stores it and does not check permissions again.
	IssueTerminalTicket(ctx context.Context, row persistmodel.TerminalTicketRow) error

	// RedeemTerminalTicket redeems a ticket and returns the run and identity bound to it.
	//
	// ⚠️ **It must be done atomically in one SQL statement** (`update ... where ticket=$1 and used_at
	// is null returning ...`). Select then update would let the same ticket be redeemed twice under
	// concurrency, which defeats the whole reason this table exists: the "one-time" property is
	// guaranteed by this one statement only.
	//
	// Ticket missing / already used / expired **all return ErrNotFound**: not telling them apart is
	// deliberate. Telling them apart would tell an attacker "this ticket exists but expired", and the
	// caller handles all three the same way.
	RedeemTerminalTicket(
		ctx context.Context, ticket string, now time.Time,
	) (persistmodel.TerminalTicketRow, error)

	// PurgeExpiredTerminalTickets deletes tickets that expired before before, and returns how many
	// were deleted.
	//
	// Tickets are short-lived rows created often and useless after a few tens of seconds; without
	// cleanup this table grows forever. Same lesson as PurgeFinishedAgentSyncTasks above: **written
	// but never called** produces zero errors.
	PurgeExpiredTerminalTickets(ctx context.Context, before time.Time) (int64, error)

	// -- self_hosted worker tool result ledger (PG; the memory Store uses a map) ----------
	//
	// Records how far each tool_use got, so that after a worker restart / reclaim, tools with side
	// effects are not run again. **It must be isolated per session**: see core/pgtoolresult.go and
	// repository/schema/selfhost_tool_results.sql (the incident where the local file ledger's poison
	// entries leaked across sessions).
	BeginToolResult(ctx context.Context, sessionID, callID string) error
	MarkToolResultResult(ctx context.Context, sessionID, callID string, resultEvent []byte) error
	MarkToolResultSent(ctx context.Context, sessionID, callID string) error
	GetToolResultState(ctx context.Context, sessionID, callID string) (state string, resultEvent []byte, found bool, err error)
	ListToolResultsForRecover(ctx context.Context, sessionID string) (pending map[string][]byte, sent map[string]bool, err error)
	DeleteOldToolResults(ctx context.Context, olderThan time.Time) (int64, error)

	// -- For e2e --------------------------------------------------------------

	// Reset restores the seed data. Matches window.__amaApi.reset() in the frontend.
	Reset(ctx context.Context) error
	// Clear deletes all data. Matches window.__amaApi.clear() in the frontend.
	Clear(ctx context.Context) error
}

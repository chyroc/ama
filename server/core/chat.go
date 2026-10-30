// Chat: run one session on the real Ark Managed Agents.
//
// # One chat is one Run
//
// We deliberately add no Conversation / Message entity. The comment at the top of the
// frontend ChatPage decided this, so chats show up in the run history automatically, can be
// reopened from the history to continue, and share one copy of the tool pairing logic with
// RunDetailPage. On the Ark side this maps to:
//
//	AMA Run  ←→  Ark Session
//	AMA RunEvent ←→ Ark SessionEvent
//
// # Use the cloud environment, not self_hosted
//
// **This is a tested result, not a preference.** In a self_hosted environment Ark rejects
// agent-side event writes (`agent.message` / `session.status_idle` both return
// InternalServiceError), and there is no worker receipt endpoint either: after the worker
// finishes, not a single field of the Session changes, and the UI never shows a result. Full
// evidence is in the "ceiling" section of cmd/ama-worker/README.md.
//
// In the cloud environment Ark runs the agent loop itself and the result lands on the
// Session directly; this path works. When creating a cloud environment **networking is
// required**; leaving it out returns MissingParameter.
//
// # Mapping and reuse of Ark resources
//
// One AMA Agent maps to one Ark Agent, and one workspace maps to one Ark Environment. Their
// IDs are stored in ark_bindings (a small table in the Store), **not created once per
// message**: creating an Ark Agent is a resource operation with a quota, and creating one per
// chat would quickly flood the console.
//
// **The binding must be stored in the database, not only in memory.** Ark Environment names
// are unique within a project, and creating a duplicate name returns 409. If it were only in
// memory, after a process restart we would create the same name again, and that workspace
// could never open a new chat. See the comment on ensureBinding; this problem only showed up
// after restarting the service.
//
// # The two destinations of an event
//
// Every event Ark pushes must do two things **at the same time**: be forwarded to the browser
// (live) and be written to the Store (history). Without the latter, refreshing the page leaves
// the chat empty, because the frontend useConversationStream "loads history first, then
// subscribes".
//
// The database write happens in the SSE handler rather than in a separate background
// goroutine watching Ark: the latter would have to handle "who writes when nobody is
// watching", and in the current setup a chat always has someone watching. **Events are not
// stored when nobody is subscribed**; this is a known gap of this trade-off, see the comment
// on subscribe at the end of this file.

package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/shared/gopkg/arkframe"
	"github.com/chyroc/ama/server/shared/infra/ma"
	"github.com/chyroc/ama/server/shared/infra/secret"
)

// The timeout of a single synchronous Ark call (create Agent / create Session / send events)
// **has moved to service/arkrun** (its callTimeout there, still 30 seconds).
//
// It moved because only LiveRunner uses it; on the core side only comments mention its value
// (one each in runbackfill.go and arkbackfill_test.go, explaining "why back-filling needs its
// own limit").

// slowBackfillThreshold is the line for "this back-fill deserves a higher log level"; see
// SyncRunEvents.
//
// Above it we log Warn instead of Info: there is an Info line every time a detail page is
// opened, so the really slow one would be buried (same idea as slowToolThreshold in
// shared/infra/shelltools/timing.go).
//
// 500ms was chosen because apart from the Ark step everything is local PG: measured, reading
// back one session with 582 events takes 1.77ms and 3000 events takes 8.1ms, so a healthy
// back-fill should finish within tens of milliseconds. Over half a second means the Ark round
// trip is dragging, and that is exactly what must be visible at a glance. **It is smaller than
// backfillTimeout**; otherwise the ones cut off by the timeout would never reach this check.
const slowBackfillThreshold = 500 * time.Millisecond

// ArkBinding has moved to model/persistmodel.
//
// ⚠️ Its 8 json tags are the **stored format** of ark_bindings.doc (not the HTTP contract).
// Renaming one key is a silent data incompatibility: existing rows read back with all fields
// empty, the fingerprint does not match, so re-creating the Environment hits 409, and that
// workspace can never open a new chat again.

// ArkKeyFingerprint computes the fingerprint of a key, used to decide "do two places mean the
// same key".
//
// It uses secret.Fingerprint directly: its shape is `len=<n> sha=<hex8>`, it is **not
// reversible**, and it is safe to put in doc and in logs.
//
// An empty key returns an empty string, not "the fingerprint of the empty string": callers
// must be able to tell "no key" from "a key", and Fingerprint("") gives a value that looks
// like a real fingerprint (`len=0`).
func ArkKeyFingerprint(apiKey string) string {
	if strings.TrimSpace(apiKey) == "" {
		return ""
	}
	return secret.Fingerprint(apiKey)
}

// sameArkKey reports whether an existing binding was created with the same key.
//
// # Existing bindings (with no fingerprint) always count as "the same key"
//
// This is **deliberate backward compatibility**: bindings created before the upgrade do not
// have this field, and they were certainly created with that workspace's key at the time.
// Judging them "different" would make every existing workspace rebuild its Ark resources
// after the upgrade. That is not just waste: the duplicate Environment name would hit 409,
// and **every existing workspace would immediately be unable to open a new chat**.
//
// The cost: a binding created before the upgrade still reuses the old environment even if
// someone later gave an agent a different key. In that case, the next time the user changes
// the agent's credentials a new fingerprint is written, and the next round corrects itself.
// Better to correct one step late than to break everyone's chats at the moment of upgrade.
func sameArkKey(b persistmodel.ArkBinding, fingerprint string) bool {
	if b.KeyFingerprint == "" {
		return true
	}
	return b.KeyFingerprint == fingerprint
}

// ---------------------------------------------------------------------------
// Service side
// ---------------------------------------------------------------------------

// CreateConversation opens a new chat: creates an Ark Session and stores a Run with
// trigger=manual.
//
// **The Session is created before the Run is stored**: if the Session cannot be created
// (invalid key, Ark failure), we should not leave an empty chat that can never send a
// message. Storing the Run first and then creating the Session would mean either rolling back
// or leaving garbage on failure, both more complex than doing it in order.
//
// The title is left empty and filled in from the start of the first message, same as
// LocalApiClient.createConversation (api.ts:869); Codex does the same.
func (s *Service) CreateConversation(ctx context.Context, workspaceID, agentID string) (apimodel.Run, error) {
	return s.createConversation(ctx, workspaceID, agentID, triggerManual, "")
}

// CreateFeishuConversation creates a session that **comes from Feishu**. Apart from
// trigger/triggerDetail it is exactly the same as CreateConversation.
//
// Why a separate method instead of a new parameter on CreateConversation: that method has a
// dozen or so call sites (including tests), all of which would need changes, while what they
// express has not changed.
//
// ⚠️ After tagging trigger=feishu, the frontend session list filter **must** also be widened
// to accept feishu (CONVERSATION_TRIGGERS in ChatPage.tsx). Changing only this side makes
// Feishu sessions disappear from the left column entirely, which is exactly the bug this
// change fixes.
func (s *Service) CreateFeishuConversation(
	ctx context.Context, workspaceID, agentID, detail string,
) (apimodel.Run, error) {
	return s.createConversation(ctx, workspaceID, agentID, apimodel.TriggerFeishu, detail)
}

func (s *Service) createConversation(
	ctx context.Context, workspaceID, agentID string,
	trigger apimodel.RunTrigger, detail string,
) (apimodel.Run, error) {
	if s.runner == nil {
		return apimodel.Run{}, fmt.Errorf("%w: 服务端未接入方舟，无法发起对话", ErrPrecondition)
	}

	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		// The message matches createConversation in LocalApiClient (api.ts:866).
		return apimodel.Run{}, fmt.Errorf("%w: Agent 不存在", ErrNotFound)
	}
	if agent.WorkspaceID != workspaceID {
		return apimodel.Run{}, fmt.Errorf("%w: Agent 不属于该空间", ErrInvalidArgument)
	}
	// A disabled agent cannot open a new session.
	//
	// **This is not a duplicate of the gate in AppendRunEvents.** Without this one, creating
	// the session would succeed (really calling Ark CreateSession, really storing a Run), and
	// then the first message would be blocked with 422: the user gets an empty session that can
	// never send anything, and the run history gets a record where nothing happened. Meanwhile
	// the Ark session and container have already been created and waste resources.
	//
	// Blocking here also makes scheduled tasks and self-wakeups fail one step earlier: the first
	// step of the scheduler's runOnce is CreateConversation, so LastError records "disabled"
	// instead of some vague downstream error.
	if agent.Status.IsDisabled() {
		return apimodel.Run{}, fmt.Errorf("%w: Agent 已停用，无法发起对话；要用它请先到 Agent 详情页启用", ErrPrecondition)
	}

	key, err := s.arkKey(ctx, workspaceID, agentID)
	if err != nil {
		return apimodel.Run{}, err
	}

	binding, err := s.ensureBinding(ctx, key, agent)
	if err != nil {
		return apimodel.Run{}, err
	}
	sessionID, err := s.runner.CreateSession(ctx, key, binding, "")
	if err != nil && ma.IsNotFound(err) && !ma.IsManagedAgentNotOpen(err) {
		// The Ark resource the binding points to is gone (someone deleted it in the console, or the
		// key was swapped for one pointing at another project). **We must self-heal here**: once a
		// binding is stored it never expires by itself; if we do not clear it, this agent can never
		// open a new chat, and the error is an unexplained 404.
		//
		// A 404 on the environment and a 404 on the Agent heal different scopes (see the comment on
		// healMissingArkResource), so we cannot always "delete our own binding and rebuild". That was
		// exactly the code that made a scheduled task fail once an hour in production: when
		// rebuilding, it picked the same dead environment back up from a sibling binding.
		//
		// ⚠️ **A NotOpen 404 must not self-heal**: it means the account has not enabled Managed
		// Agents, and the binding itself is fine. Deleting and rebuilding would only create an orphan
		// Environment/Agent pair and hit the exact same 404 again. The same 404 is told apart by its
		// code; see the comment on ma.IsManagedAgentNotOpen.
		binding, sessionID, err = s.healMissingArkResource(ctx, key, agent, binding, err)
	}
	if err != nil {
		return apimodel.Run{}, arkFailure("创建方舟会话失败", err)
	}

	// Executor re-check: when the agent picked a host machine, confirm the machine is still
	// usable before creating the session. It was checked once when the agent was saved, but the
	// state may have changed since; relying only on the save-time check would let stale existing
	// agents keep dispatching. The two sources have different invalidation events, so there must
	// be two branches:
	//   workspace (assigned to the workspace): unbound from the workspace by an admin/owner →
	//                                          blocked by WorkspaceHostBound;
	//   personal (direct personal connection): the machine was revoked by its owner (hosts row
	//                                          deleted) → blocked by HostExists; workspace
	//                                          unbinding does not matter for it.
	// A brief DB outage does not block the chat (degrade and allow; the HostOnline and host
	// deletion checks on the executor side are still there).
	hostIDForRun, hostSourceForRun := hostOnRun(agent)
	if hostIDForRun != "" && s.hostTerms != nil {
		if hostSourceForRun == string(apimodel.HostSourcePersonal) {
			exists, herr := s.hostTerms.HostExists(ctx, hostIDForRun)
			if herr != nil {
				s.log().Warn("复查个人直连宿主机是否存在失败，本次放行",
					"host", hostIDForRun, "err", herr)
			} else if !exists {
				return apimodel.Run{}, fmt.Errorf(
					"%w: 这台宿主机已被属主删除，请在 Agent 设置里改回云容器或联系创建者更换机器",
					ErrPrecondition)
			}
		} else {
			if bound, berr := s.hostTerms.WorkspaceHostBound(ctx, workspaceID, hostIDForRun); berr != nil {
				s.log().Warn("复查宿主机空间绑定失败，本次放行",
					"host", hostIDForRun, "workspace", workspaceID, "err", berr)
			} else if !bound {
				return apimodel.Run{}, fmt.Errorf(
					"%w: 宿主机已从该空间解绑，请在 Agent 设置里改回云容器或重新绑定",
					ErrPrecondition)
			}
		}
	}

	now := apimodel.NowISO()
	run := apimodel.Run{
		// **The ID is the Ark session id.** RunDetailPage shows run.id as "Session ID"
		// (RunDetailPage.tsx:220), so the two should be the same thing anyway; issuing one each and
		// storing a mapping would invent an "AMA-side run id" that nobody needs.
		ID:          sessionID,
		WorkspaceID: workspaceID,
		AgentID:     agentID,
		AgentName:   agent.Name,
		Title:       "",
		// A new session has not started running yet. It turns running when the first message arrives.
		Status:        apimodel.RunIdle,
		Trigger:       trigger,
		TriggerDetail: detail,
		StartedAt:     now,
		// Aligned with StartedAt on creation, so list sorting works for new sessions right away without waiting for the first event.
		LastActivityAt: now,
		Usage:          apimodel.TokenUsage{},
		// Threads are created by Ark: it issues the thread id (sthr-...); one made up here would never match the real events.
		Threads: []apimodel.RunThread{},
		// Snapshot the executor: for an agent that picked a host machine, the run records host_id
		// and its source. Tool routing (toolRouter) sends tools to the host executor by host_id, and
		// terminal tickets pick their permission level by host_source; later executor changes on the
		// agent do not rewrite past runs. Empty = Docker container.
		HostID:     hostIDForRun,
		HostSource: hostSourceForRun,
		EventCount: 0,
	}
	return s.store.CreateRun(ctx, run)
}

// healMissingArkResource self-heals when CreateSession hits 404 (the Ark resource the
// binding points to is gone), and returns a binding to retry with plus the new session ID.
// The caller has already ruled out the ManagedAgentNotOpen kind of 404.
//
// # A 404 on the environment and a 404 on the Agent heal different scopes
//
// A workspace shares one Ark Environment, and **all sibling bindings** in the same workspace
// store the same environment ID. A real failure caught by a production scheduled task: the
// environment was reclaimed by the platform (404 InvalidEnvironment.NotFound). The old code
// only deleted this agent's own binding, and when rebuilding, ensureBinding picked **the same
// dead environment** back up from a sibling binding, so the second CreateSession got the
// same 404. After that it repeated every hour, and each round also created a useless Ark
// Agent.
//
// So there are two branches:
//
// The environment is gone (ma.IsEnvironmentNotFound):
// create a new environment → RepointArkEnvironment points all bindings of the workspace at
// it **in a targeted way** (including our own row; the Ark Agent is a separate resource and
// keeps working once attached to the new environment, no need to rebuild it) → retry right
// away. If the retry still gets 404, the Agent happens to be gone too, so fall through to the
// branch below.
// The Agent is gone (any other 404):
// delete our own binding → ensureBinding rebuilds it (the environment is reused from a
// sibling row, which is alive).
func (s *Service) healMissingArkResource(
	ctx context.Context, key ArkBindingKey, agent apimodel.Agent,
	binding persistmodel.ArkBinding, firstErr error,
) (persistmodel.ArkBinding, string, error) {
	log := s.log().With(
		slog.String("workspace", key.WorkspaceID), slog.String("agent", key.AgentID))

	if ma.IsEnvironmentNotFound(firstErr) {
		deadEnv := binding.ArkEnvironmentID
		log.Warn("方舟运行环境已不存在，建新环境并把同空间绑定一起改指",
			slog.String("deadEnv", deadEnv),
			slog.String("arkAgent", binding.ArkAgentID),
			slog.String("code", ma.ErrorCode(firstErr)),
			slog.String("request_id", ma.RequestID(firstErr)))

		// Create the new environment first; if that fails, touch nothing: the binding still points at the old environment, and the next retry still has a chance.
		envName := fmt.Sprintf("ama-%s-%d", key.WorkspaceID, time.Now().UnixNano())
		newEnv, err := s.runner.EnsureEnvironment(ctx, key, envName)
		if err != nil {
			return binding, "", fmt.Errorf("替换已失效的方舟环境 %s 失败: %w", deadEnv, err)
		}
		n, err := s.store.RepointArkEnvironment(ctx, key.WorkspaceID, deadEnv, newEnv)
		if err != nil {
			return binding, "", fmt.Errorf("把兄弟绑定改指到新环境 %s 失败: %w", newEnv, err)
		}
		log.Info("死环境替换完成",
			slog.String("deadEnv", deadEnv), slog.String("newEnv", newEnv),
			slog.Int("repointed_bindings", n))
		binding.ArkEnvironmentID = newEnv

		sessionID, err := s.runner.CreateSession(ctx, key, binding, "")
		if err == nil {
			log.Info("环境改指后建会话成功", slog.String("session", sessionID))
			return binding, sessionID, nil
		}
		// Still failing on the new environment: it is either not a 404 or some other failure
		// (auth/rate limit/parameters); let the caller translate it, and do not delete the binding
		// again, since the environment was just created and cannot already be gone.
		if !ma.IsNotFound(err) || ma.IsManagedAgentNotOpen(err) {
			return binding, "", err
		}
		// Narrow case: the environment and the Agent were deleted at the same time. Carry this error
		// into the Agent rebuild branch.
		// In the extreme case (this workspace has only this one agent's binding), after deleting our
		// own binding ensureBinding finds no sibling environment to reuse and creates another
		// environment, and the newEnv just created becomes an empty orphan environment.
		// Acceptable: such a double deletion is very rare, and the cost is only one unreferenced
		// empty environment on the Ark side.
		log.Warn("新环境上建会话仍 404，方舟 Agent 也已不存在，连 Agent 一起重建",
			slog.String("arkAgent", binding.ArkAgentID),
			slog.String("code", ma.ErrorCode(err)),
			slog.String("request_id", ma.RequestID(err)))
		firstErr = err
	}

	log.Warn("方舟 Agent 已不存在，清掉这个 agent 的绑定重建",
		slog.String("arkAgent", binding.ArkAgentID),
		slog.String("arkEnv", binding.ArkEnvironmentID),
		slog.String("code", ma.ErrorCode(firstErr)),
		slog.String("request_id", ma.RequestID(firstErr)))
	if err := s.store.DeleteArkBinding(ctx, key.WorkspaceID, key.AgentID); err != nil {
		return binding, "", fmt.Errorf("清理失效的方舟绑定失败: %w", err)
	}
	b, err := s.ensureBinding(ctx, key, agent)
	if err != nil {
		return b, "", err
	}
	sessionID, err := s.runner.CreateSession(ctx, key, b, "")
	if err == nil {
		log.Info("绑定重建后建会话成功",
			slog.String("arkAgent", b.ArkAgentID),
			slog.String("arkEnv", b.ArkEnvironmentID),
			slog.String("session", sessionID))
	}
	return b, sessionID, err
}

// ensureBinding gets (or creates) the Ark-side counterpart of an AMA agent and **stores** the
// result in the database.
//
// # Storing it is not an optimization, it is correctness
//
// Ark Environment names are unique within a project, and creating a duplicate name returns
// 409 ResourceConflict. If bindings were only kept in memory, after a process restart the
// cache is empty, creating the same name again returns 409 right away, and that workspace can
// never open a new chat. **The first run works fine, and after a restart it fails 100% of
// the time**, while the error (ResourceConflict) points at Ark, not at the caching strategy
// here.
//
// It also leaks resources: Agents may share names, so every restart creates one more, and
// they pile up.
//
// # Environments are reused per workspace, Agents per AMA agent
//
// A workspace shares one Environment (the same network policy; no need for one per agent),
// so when creating the binding for a new agent, we first look at any existing binding of this
// workspace and take the environment ID from it.
func (s *Service) ensureBinding(
	ctx context.Context, key ArkBindingKey, agent apimodel.Agent,
) (persistmodel.ArkBinding, error) {
	existing, err := s.store.GetArkBinding(ctx, key.WorkspaceID, key.AgentID)
	if err == nil {
		// ⚠️ **First check whether the key changed**: the Ark Agent and Environment in this binding
		// both belong to the project of the key used to create it. After the user switches this
		// agent to another key, those resources **do not exist** for the new key. Keep using them and
		// Ark reports "resource not found" or an auth failure, and since the error points at
		// credentials, the user will think the newly configured key is broken.
		//
		// So if the key changed, treat it as having no binding and take the "build a new set" path.
		// The old set becomes an orphan (it cannot be deleted on the Ark side; same known leak as
		// skill uploads). The cost is that the first chat after switching keys is a few seconds
		// slower, far smaller than "after switching keys you can never open a chat again".
		keyFP := ArkKeyFingerprint(key.APIKey)
		if sameArkKey(existing, keyFP) {
			// The binding exists and the Ark Agent is there too, but **its content may be out of
			// date**.
			//
			// This step is all of what "config changes take effect immediately" means: an Ark Agent is
			// created once and cached forever (the CreateAgent below is never called again), so if the
			// user changes something after the first chat and we do not actively bring it up to date,
			// the Ark side never knows, and it looks like "I changed it but it acts as if I did not".
			//
			// **The two catch-ups are independent** (one for skills, one for spec), because their
			// fingerprints are computed separately: merging them would make "changed a skill" also
			// resend the system prompt, and the other way round.
			b := s.syncBindingSkills(ctx, key, agent, existing)
			return s.syncBindingSpec(ctx, key, agent, b), nil
		}
		s.log().Info("方舟凭证已更换，为这个 agent 重建方舟资源",
			"workspace", key.WorkspaceID, "agent", key.AgentID)
	} else if !errors.Is(err, ErrNotFound) {
		return persistmodel.ArkBinding{}, err
	}

	// If another agent in the same workspace **and with the same key** has already created an
	// environment, reuse it, to avoid the duplicate-name 409 and save a resource.
	//
	// ⚠️ **The key fingerprint must be compared; do not reuse by workspace alone**: an
	// Environment belongs to the project of the key used to create it. Once agent-level
	// credentials exist, one workspace may have two different keys, and reusing the wrong one
	// means "using key2 to access key1's project". Ark's error says the credentials are wrong,
	// while each of the two keys is fine. See the comment on ArkBinding.KeyFingerprint.
	keyFP := ArkKeyFingerprint(key.APIKey)
	envID := ""
	var envFromAgent string
	if siblings, err := s.store.ListArkBindings(ctx, key.WorkspaceID); err == nil {
		for _, b := range siblings {
			if b.ArkEnvironmentID != "" && sameArkKey(b, keyFP) {
				envID = b.ArkEnvironmentID
				envFromAgent = b.AgentID
				break
			}
		}
	} else {
		// A listing failure does not block (a new one is created below, at the cost of one more
		// environment), but it must leave a trace: silent reuse / silent creation is exactly the kind
		// of information most missing when debugging.
		s.log().Warn("列同空间方舟绑定失败，本次不复用环境、直接新建",
			slog.String("workspace", key.WorkspaceID), slog.Any("err", err))
	}
	if envID != "" {
		s.log().Info("复用同空间兄弟 agent 的方舟环境",
			slog.String("workspace", key.WorkspaceID),
			slog.String("agent", key.AgentID),
			slog.String("env", envID),
			slog.String("env_from_agent", envFromAgent))
	} else {
		// The name has a unique suffix; **do not use a fixed name**. A fixed name would be "claimed"
		// on a duplicate-name 409, and what gets claimed may be an environment of the old type (this
		// happened before: after switching to self_hosted, a cloud environment was claimed; the type
		// did not change, and nothing in the whole chain reported an error). See the comment in
		// arkrun.go.
		//
		// The suffix is a timestamp, not an in-process counter: the latter starts over on restart,
		// collides across restarts, and leads back to the 409 path.
		envName := fmt.Sprintf("ama-%s-%d", key.WorkspaceID, time.Now().UnixNano())
		envID, err = s.runner.EnsureEnvironment(ctx, key, envName)
		if err != nil {
			return persistmodel.ArkBinding{}, arkFailure("准备方舟运行环境失败", err)
		}
		s.log().Info("新建方舟运行环境",
			slog.String("workspace", key.WorkspaceID),
			slog.String("agent", key.AgentID),
			slog.String("env", envID))
	}

	// Sync skills to Ark first so they are included when the Agent is created: one round trip
	// fewer than updating after creation, and no window of "the Agent exists but has no skills
	// yet".
	//
	// Failure does not block: empty refs just means "the model does not know about skills this
	// round", and the chat opens as usual (reason in the file header of skillark.go).
	refs, fingerprint, err := s.arkSkillsFor(ctx, key, agent)
	if err != nil {
		// Only "too many bound, over the limit" gets here (that is ErrPrecondition, and the user
		// must see it). A single skill failing to sync is already downgraded to Warn in arkSkillsFor.
		return persistmodel.ArkBinding{}, err
	}

	spec := arkSpecFor(agent)
	spec.Skills = refs
	created, err := s.runner.CreateAgent(ctx, key, spec)
	if err != nil {
		return persistmodel.ArkBinding{}, arkFailure("准备方舟 Agent 失败", err)
	}

	// The fingerprint is written back only when **Ark confirms the skills really got attached**.
	//
	// ⚠️ This used to be written unconditionally, a design that blocked its own repair path:
	// the fingerprint means "what is attached on the Ark side now is exactly this set", and
	// writing it before Ark confirms makes the record false. The next chat hits the existing
	// binding → syncBindingSkills compares fingerprints → equal → returns right away, and the
	// UpdateAgentSkills that should correct it is never called.
	// So "skills not attached" becomes **permanent**, with zero errors.
	//
	// This really happened: the translation in the ma layer forgot to pass Skills (CreateAgent in
	// arkrun.go), so no newly created agent got its skills attached, and adding that field back
	// could not rescue the agents already affected.
	//
	// The cost of leaving the fingerprint empty is only one extra UpdateAgent on the next chat
	// (that path writes it correctly and only writes the fingerprint back after success): **one
	// extra round trip buys a path that can always heal itself**.
	// This is the same rule as "if the update did not succeed, do not write the fingerprint
	// back" in syncBindingSkills.
	fingerprintToStore := fingerprint
	arkSkillIDs := arkSkillIDsOf(refs)
	if created.SkillCount != len(refs) {
		s.log().Warn("方舟建 Agent 时挂上的技能条数与预期不符，不回写指纹（下次开对话会补挂）",
			slog.String("workspace", key.WorkspaceID), slog.String("agent", key.AgentID),
			slog.String("arkAgent", created.ID),
			slog.Int("want", len(refs)), slog.Int("got", created.SkillCount))
		fingerprintToStore = ""
		arkSkillIDs = nil
	}

	binding := persistmodel.ArkBinding{
		WorkspaceID:       key.WorkspaceID,
		AgentID:           key.AgentID,
		ArkAgentID:        created.ID,
		ArkEnvironmentID:  envID,
		CreatedAt:         apimodel.NowISO(),
		SkillsFingerprint: fingerprintToStore,
		ArkSkillIDs:       arkSkillIDs,
		// The spec fingerprint can be written back unconditionally; it differs from the skills one:
		// **this is exactly the spec that was just sent**, and CreateAgent succeeding means Ark
		// accepted it.
		//
		// The skills side has to wait for confirmation because it goes through a **field-by-field
		// hand-written translation layer** (the ma.AgentSpec literal), where a missing field raises no
		// error; here the fingerprint input is the same spec variable, so both come from the same
		// source. Guarding against missing fields in the translation layer is the job of the
		// reflection test TestArkAgentSpecMapsEveryFieldToMA, not this code.
		SpecFingerprint: ArkSpecFingerprint(spec),
		// Record which key this set of resources was created with, so next time we can decide whether the environment can be reused.
		KeyFingerprint: keyFP,
	}
	if err := s.store.PutArkBinding(ctx, binding); err != nil {
		// A failed store is the same as never having created it: next time another Ark Agent is
		// created (one leaked), but we must not pretend success; that way the binding would never be
		// stored and every chat would leak one.
		return persistmodel.ArkBinding{}, fmt.Errorf("保存方舟资源绑定失败: %w", err)
	}
	return binding, nil
}

// syncBindingSkills brings the skills attached to the Ark Agent up to date with the current
// state on the AMA side.
//
// # Why it is needed
//
// When ensureBinding hits an existing binding it reuses that Ark Agent directly, and
// `CreateAgent` is never called again. So "binding a skill after the first chat" is
// something the Ark side never learns about. That is exactly the symptom this fixes: the user
// binds a skill, the agent acts as if nothing happened, with zero errors.
//
// # Why UpdateAgent instead of deleting the binding and rebuilding
//
// Rebuilding **leaks one more Ark Agent** (Ark allows duplicate names; the old one cannot be
// deleted and nobody deletes it) and changes ArkAgentID, after which past sessions point at
// an Agent that is no longer updated. UpdateAgent only changes the content, the id stays the
// same, and old sessions benefit too.
//
// # Every failure degrades
//
// The return value is always a usable binding (on sync failure the original one is
// returned), and only a WARN is logged.
// Today chats work without skills, and dragging "cannot open a chat" in for an add-on
// feature is a bad trade. **But the log must be kept**, otherwise "why does the model not use
// skills" cannot be investigated.
func (s *Service) syncBindingSkills(
	ctx context.Context, key ArkBindingKey, agent apimodel.Agent, b persistmodel.ArkBinding,
) persistmodel.ArkBinding {
	log := s.log().With(
		slog.String("workspace", key.WorkspaceID), slog.String("agent", key.AgentID))

	refs, fingerprint, err := s.arkSkillsFor(ctx, key, agent)
	if err != nil {
		// The over-the-limit kind. **Do not return it upward** here: doing so would mean "after
		// binding too many skills you cannot even open a chat". The binding creation path does return
		// it (the user is configuring at that time, so seeing the error is useful), but this path runs
		// every time a chat is opened, and blocking here would only make people think chat is broken.
		log.Warn("技能同步到方舟失败，沿用方舟 Agent 上现有的那组技能", slog.Any("err", err))
		return b
	}

	if fingerprint == b.SkillsFingerprint {
		// Almost every case goes here: **zero extra network calls**.
		return b
	}

	log.Info("agent 绑定的技能变了，更新方舟 Agent",
		slog.String("was", b.SkillsFingerprint), slog.String("now", fingerprint),
		slog.Int("skills", len(refs)))

	if err := s.runner.UpdateAgentSkills(ctx, key, b.ArkAgentID, refs); err != nil {
		// If the update did not succeed, **do not write the fingerprint back**: doing so would make
		// the next chat hit the fingerprint directly with no retry at all, and that skill would never
		// take effect (with zero errors). Keeping the old fingerprint means it will be tried again
		// next time.
		log.Warn("更新方舟 Agent 的技能失败，下次开对话会重试", slog.Any("err", err))
		return b
	}

	b.SkillsFingerprint = fingerprint
	b.ArkSkillIDs = arkSkillIDsOf(refs)
	if err := s.store.PutArkBinding(ctx, b); err != nil {
		// The Ark side was updated successfully; we just failed to record it. The next chat will
		// update again; UpdateAgent is idempotent (it replaces everything with the same set), so
		// repeating is harmless.
		log.Warn("方舟技能已更新但绑定回写失败，下次会重复更新一次", slog.Any("err", err))
	}
	return b
}

// syncBindingSpec brings the Ark Agent's system / model / description / tools up to date with
// the current state on the AMA side.
//
// # Why it is needed: this is the half syncBindingSkills missed
//
// An Ark Agent is created once and cached forever, and ensureBinding reuses it directly when
// it hits an existing binding. A catch-up was added for skills back then
// (syncBindingSkills), but **the same statement holds word for word for system / model**, and
// that half was never done: if the user changes the system prompt from A to B, or switches
// models, the Ark side stays on the old one forever.
//
// It shows up as "I changed the prompt, but the agent still talks the old way", and it is
// harder to track down than the skills case:
//
// - **Zero errors**: PATCH returns 200 and the database really changed; only the Ark side did
// not move;
// - **Permanent**: on the skills side any add or remove changes the fingerprint and so heals
// it along the way, but nothing else ever touches system; changing it a hundred times gives
// the same result;
// - **The error does not point at the real cause**: the user will think the model is not
// listening or the prompt is badly written, and keep changing the prompt, which fails to
// take effect every single time.
//
// Confirmed by testing (against real Ark): create the agent with system="V1" → open a chat
// → PATCH to "V2" → open another new chat → Get from Ark still returns "V1", and the Agent
// version stays at 1.
//
// # Why separate from skills instead of merged into one UpdateAgent
//
// The two fingerprints are independent (skills are computed by content, spec by the
// assembled text). Merging them would make "changed one skill" resend system too, and the
// other way round. The extra round trip does not matter, but **one failed call would drag
// both down together**, and "skill sync failed" could no longer be told apart from "prompt
// sync failed".
//
// The cost is two UpdateAgent calls in a round where both changed. Acceptable: that only
// happens on the first chat right after the user changed the config; after that both
// fingerprints match, with zero extra round trips.
//
// # Every failure degrades
//
// Same trade-off as syncBindingSkills: the return value is always a usable binding, and only
// a WARN is logged. Chats still work when the prompt is not caught up (they just use the old
// one), and dragging "cannot open a chat" in for it is a bad trade. **But the log must be
// kept**, otherwise "why did changing the prompt do nothing" cannot be investigated.
func (s *Service) syncBindingSpec(
	ctx context.Context, key ArkBindingKey, agent apimodel.Agent, b persistmodel.ArkBinding,
) persistmodel.ArkBinding {
	// ⚠️ **Use the full value computed by arkSpecFor**; do not send only agent.System.
	//
	// The system sent to Ark is assembled: the user's text + the self-wakeup instructions + the
	// `<harness:env>` block (see arkSpecFor). Sending only the user's text would **erase** the
	// last two parts, a regression with zero errors: the model would no longer know it can wake
	// itself up (so when it has to wait for something, it sleeps in place until it is cut off),
	// and it would not get the runtime facts in env. This is the easiest place to get wrong on
	// this path.
	//
	// Skills is deliberately left empty: that is the job of syncBindingSkills, and the ma layer
	// treats Skills as "nil means do not change", so leaving it empty says exactly "do not touch
	// skills this time".
	spec := arkSpecFor(agent)
	fingerprint := ArkSpecFingerprint(spec)
	if fingerprint == b.SpecFingerprint {
		// Almost every case goes here: **zero extra network calls**.
		return b
	}

	log := s.log().With(
		slog.String("workspace", key.WorkspaceID), slog.String("agent", key.AgentID))
	log.Info("agent 的提示词/模型变了，更新方舟 Agent",
		slog.String("was", b.SpecFingerprint), slog.String("now", fingerprint),
		slog.String("model", spec.Model))

	if err := s.runner.UpdateAgentSpec(ctx, key, b.ArkAgentID, spec); err != nil {
		// If the update did not succeed, **do not write the fingerprint back**; same rule as in
		// syncBindingSkills: doing so would make the next chat hit the fingerprint directly with no
		// retry at all, and the change would never take effect (with zero errors). Keeping the old
		// fingerprint means the next chat will try again.
		log.Warn("更新方舟 Agent 的提示词/模型失败，下次开对话会重试", slog.Any("err", err))
		return b
	}

	b.SpecFingerprint = fingerprint
	if err := s.store.PutArkBinding(ctx, b); err != nil {
		// The Ark side was updated successfully; we just failed to record it. The next chat will
		// update again; UpdateAgent is idempotent (it sends the same content again), so repeating is
		// harmless.
		log.Warn("方舟 Agent 已更新但绑定回写失败，下次会重复更新一次", slog.Any("err", err))
	}
	return b
}

// arkSpecFingerprintVersion is the version prefix of the spec fingerprint.
//
// Same technique as skillsFingerprintVersion (see that part of skillzip.go): when the meaning
// of the fingerprint changes, change this prefix, so every existing binding mismatches once
// and each catches up once on its next chat.
//
// It starts at v1 because **existing bindings do not have this key at all** (it unmarshals to
// an empty string), and the empty string never equals any real fingerprint, so it naturally
// triggers one catch-up. That is exactly the self-healing we want.
const arkSpecFingerprintVersion = "v1:"

// ArkSpecFingerprint computes the fingerprint of "which spec the Ark Agent should have".
//
// # The input must be the return value of arkSpecFor
//
// That is, the spec **after assembly** (system includes the self-wakeup instructions and the
// `<harness:env>` block). Computing it from the raw agent.System is wrong: then "changed an
// env variable" would not trigger a catch-up, and that block is the only place the model
// gets runtime facts from.
//
// # Skills are deliberately not included
//
// Skills are tracked separately by SkillsFingerprint. Including them would make "swapped a
// skill" also trigger a spec update, which changes nothing in system: a wasted round trip.
//
// # Always returns a non-empty string
//
// The empty string is reserved for "we do not know which spec is on the Ark side" (existing
// bindings do not have this key), and it must never equal any real fingerprint. So there is
// no "empty spec returns empty string" shortcut here: that would make an agent whose fields
// are all empty look "already caught up", so it would never sync. This is the opposite of
// SkillsFingerprint (there, returning an empty string for an empty set is correct: the
// desired state it expresses, "no skills on the Ark side", was also true in the old version).
func ArkSpecFingerprint(spec ArkAgentSpec) string {
	h := sha256.New()
	// Write each field with a **length prefix** instead of concatenating directly.
	//
	// Without lengths, ("ab","c") and ("a","bc") give the same fingerprint, so a change like
	// "move the last sentence of the prompt to the start of the description" would not trigger a
	// catch-up. Length prefixes are an existing practice in this repo (see SkillsFingerprint
	// and skillContentFingerprint).
	write := func(parts ...string) {
		for _, p := range parts {
			_, _ = fmt.Fprintf(h, "%d:%s", len(p), p)
		}
	}
	write(spec.Name, spec.Description, spec.System, spec.Model, spec.Speed)
	// Tools and CustomTools must be in the fingerprint too: they are also sent by
	// UpdateAgentSpec (tools are replaced as a whole), and leaving them out would make "turn a
	// built-in tool on/off for the agent" not take effect.
	write(spec.Tools...)
	for _, ct := range spec.CustomTools {
		write(ct.Name, ct.Description)
		// InputSchema is a map with random iteration order. **It must be sorted**, otherwise the same
		// spec gives a different fingerprint each time, and every chat wastes one UpdateAgent call.
		keys := make([]string, 0, len(ct.InputSchema))
		for k := range ct.InputSchema {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			// Encode the value as JSON before adding it to the fingerprint: it is any, and %v output for maps is unstable too.
			blob, err := json.Marshal(ct.InputSchema[k])
			if err != nil {
				// If it cannot be encoded, use only the key name. Such input is a programming error (see how
				// ma.toolItems handles it: it skips that property), and should not make fingerprinting fail.
				blob = nil
			}
			write(k, string(blob))
		}
		write(ct.Required...)
	}
	return arkSpecFingerprintVersion + hex.EncodeToString(h.Sum(nil))
}

// triggerManual is the trigger source of the chat path.
//
// The value must be "manual": ChatPage filters the session list in the left column by
// `r.trigger === "manual"` (ChatPage.tsx:84); get it wrong and new chats will not show up
// there.
//
// This package-level alias is kept instead of replacing it everywhere with
// apimodel.TriggerManual: the package already refers to it in many places (including tests),
// and it means "the chat path level"; a name is clearer than repeating the qualifier at every
// call site.
const triggerManual = apimodel.TriggerManual

// AppendRunEvents forwards events sent up by the client to Ark and returns Ark's receipt.
//
// It backs POST /api/runs/{runId}/events; the frontend uses it to send messages
// (user.message) and interrupts (user.interrupt). The receipt is the list of accepted events
// **given by Ark**, one for each event in the request; the frontend sendMessage takes the
// first one as "the message just sent" and draws it in the UI.
//
// # User messages are not stored here
//
// Ark pushes user.message back into the event stream unchanged (confirmed by testing), so
// the subscription path writes it. Writing it here too would duplicate it. The cost is that it
// is not stored when "a message was sent but nobody subscribed", same as other events; see
// the file header.
func (s *Service) AppendRunEvents(ctx context.Context, runID string, raw []json.RawMessage) ([]json.RawMessage, error) {
	if s.runner == nil {
		return nil, fmt.Errorf("%w: 服务端未接入方舟，无法发送消息", ErrPrecondition)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: 至少要发一个事件", ErrInvalidArgument)
	}

	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		// The message matches sendMessage in LocalApiClient (api.ts:898).
		return nil, fmt.Errorf("%w: 这次运行不存在", ErrNotFound)
	}

	// A session whose agent was deleted or disabled is frozen: no more messages can be sent.
	//
	// **But interrupts are allowed.** The Ark session may still be running (run_4 in the seed is
	// manual + running); blocking everything would mean the user can never stop it: Ark keeps
	// burning tokens, and the container is only reclaimed after an idle pause or even its TTL.
	// An interrupt creates no new work; it stops work. While streaming, the frontend turns "send"
	// into "interrupt" and that button is not disabled; blocking it would put a button on a
	// frozen session that always fails.
	//
	// # This is the only gate for "disabled" with no blind spots
	//
	// All five entry points (chat page HTTP, new Feishu session, **Feishu follow-up**, scheduled
	// task, self-wakeup) go through this method. Adding it only to createConversation is not
	// enough: once a Feishu group has bound a scope, follow-ups reuse the runID directly (the hit
	// branch of ensureScopedRun in feishu/inbound.go) and **never create a new session**, and the
	// vast majority of group messages are follow-ups. That would only block the first message of
	// each group.
	if !isInterruptOnly(raw) {
		agent, aerr := s.store.GetAgent(ctx, run.AgentID)
		switch {
		case aerr == nil && agent.Status.IsDisabled():
			// Disabled and "deleted" share 422 and the "can only view history" wording: for the user the
			// action is the same for both (go to the agent detail page and change something), and the
			// history is still there.
			//
			// Empty/unknown values do not get here; see the comment on apimodel.AgentStatus.IsDisabled.
			// That check is the only guard against "existing agents getting stopped by mistake".
			return nil, fmt.Errorf("%w: 关联的 Agent 已停用，这个会话只能查看历史；要继续对话请先到 Agent 详情页启用它", ErrPrecondition)
		case aerr == nil:
			// The agent still exists and is enabled; continue as usual.
		case errors.Is(aerr, ErrNotFound):
			// **Never use ErrNotFound.** NOT_FOUND_CODES in http.ts turns not_found into null, and the
			// page shows a silent empty state whose cause cannot be found from the symptom. The run does
			// exist here; this is "this operation is not allowed right now", which is exactly what
			// ErrPrecondition (422) means.
			//
			// This Chinese sentence is part of the contract: LocalApiClient.sendMessage copies it word for
			// word, and e2e asserts on it.
			return nil, fmt.Errorf("%w: 关联的 Agent 已被删除，这个会话只能查看历史，不能再发消息", ErrPrecondition)
		default:
			// fail-open, same reason as markAgentDeleted: a database hiccup should not look like "your agent was deleted".
			s.log().Warn("查 agent 失败，本次发送不做冻结判断", "run", runID, "error", aerr.Error())
		}
	}

	key, err := s.arkKey(ctx, run.WorkspaceID, run.AgentID)
	if err != nil {
		return nil, err
	}

	accepted, err := s.sendEventsThroughWindow(ctx, key, run.ID, raw)
	if err != nil {
		return nil, arkFailure("发送消息到方舟失败", err)
	}

	// Wake up this environment's poll loop: the events are already in Ark, and work will very
	// likely be dispatched next.
	//
	// **It must come after a successful send**: if sending failed there is no work, and waking up
	// only wastes a poll. Why it hangs on this method (and not on each HTTP handler), and why not
	// on session creation, see wakeEnvForRun in core/workerwake.go. Failures are always swallowed;
	// it is a hint, not a command.
	//
	// ⚠️ **A pure interrupt does not wake up**: an interrupt creates no new work, it stops work
	// (same wording as the frozen gate above). This is not only about saving a request:
	// `RedirectRun` **calls AppendRunEvents twice in a row** (first user.interrupt, then
	// user.message); without skipping, one redirect would wake up twice, and the first one would
	// always be idle. The check reuses the existing isInterruptOnly instead of inventing another.
	if !isInterruptOnly(raw) {
		s.wakeEnvForRun(ctx, run)
	}

	// The title and status follow this round. The title is only filled in on the first message (later messages should not change it).
	if title := TitleFromEvents(raw); title != "" && run.Title == "" {
		run.Title = title
	}
	if !isInterruptOnly(raw) {
		run.Status = apimodel.RunRunning
	}
	// The user just said something, so this session counts as active right away, without waiting
	// for Ark to send frames back.
	//
	// applyEventToRun also updates this field, but it is only called **when there is a
	// subscriber** (events are stored by persistArkEvent). The Feishu path subscribes every round,
	// but the web page may switch away right after sending, and then it all depends on this;
	// without it, refreshing after sending a message drops the session back down the list.
	run.LastActivityAt = apimodel.NowISO()
	if _, err := s.store.SaveRun(ctx, run); err != nil {
		// A failed store should not make "the message was sent" look like a failure: Ark has already accepted it.
		s.log().Warn("更新对话状态失败", "run", run.ID, "error", err.Error())
	}
	return accepted, nil
}

// ---------------------------------------------------------------------------
// Tool execution window: short retries on the server
// ---------------------------------------------------------------------------

// windowRetryBudget is the total budget for the server's own retries when it "hits the tool
// execution window".
//
// Measured, the window is only tens of milliseconds (48ms was seen in production; see
// arkFailure and shared/infra/ma/README.md), and 1 second covers nearly all cases. **It is
// deliberately small**: this is a synchronous request a user is waiting on, and making it
// longer only makes "send message" look slow. Long waits for a whole round of tools to finish
// (possibly over a hundred seconds) **are not handled here**; they belong to queuing. See the
// comment on sendEventsThroughWindow for the difference.
const windowRetryBudget = time.Second

// windowRetryStep is the interval between two retries.
//
// A fixed 40ms instead of exponential backoff: the window itself is on the order of tens of
// milliseconds, and backoff is for "do not hammer an overloaded service", while the other side
// here is fully healthy and only briefly rejecting. A small fixed interval sends the moment
// the window closes; within the budget it is at most about 25 tries, no load at all for Ark.
const windowRetryStep = 40 * time.Millisecond

// sendEventsThroughWindow sends events and retries within the budget when it hits the "tool
// execution window".
//
// # This layer and the frontend queue do **not** solve the same problem
//
// The two look alike, and mixing them up leads to the wrong implementation, so it is spelled
// out here:
//
//	this function (server)  a **race** of tens of milliseconds: between Ark sending tool_use
//	                        and the worker returning tool_result, user.message is rejected.
//	                        It has nothing to do with user intent: the user had no "send
//	                        later" in mind, they were just quick. One or two retries fix
//	                        it, and the user never notices.
//	frontend queue          the user **deliberately** sends a second message while the agent
//	                        is still running. That waits for a whole round (possibly tens of
//	                        seconds to minutes), and the user must see "which position in
//	                        line" and be able to cancel or edit. It **cannot** be solved by
//	                        sleeping here: that would hang an HTTP request for minutes.
//
// So the budget is deliberately only 1 second: past it, ErrConflict is handed back, and the
// caller (the frontend queue) handles it as "must wait a whole round". **The cross-layer
// contract of 409 + code=conflict does not change**; see isSendConflict in
// web/src/lib/useSendQueue.ts.
//
// # Why push it down to the server instead of keeping it only in the frontend
//
// The browser is not the only one hitting the window: Feishu inbound (feishu/inbound.go) and
// scheduled tasks (core/scheduler.go) also go through AppendRunEvents, and **neither of those
// paths retries**: hitting it is a hard failure, showing up as "the bot does not reply to this
// message" and "this scheduled task run was wasted", both with zero errors. Putting it here
// covers all four entry points (including a future server-side queue consumer) at once.
func (s *Service) sendEventsThroughWindow(
	ctx context.Context, key ArkBindingKey, sessionID string, raw []json.RawMessage,
) ([]json.RawMessage, error) {
	budget := s.windowRetry
	if budget <= 0 {
		budget = windowRetryBudget
	}
	deadline := time.Now().Add(budget)
	for attempt := 0; ; attempt++ {
		accepted, err := s.runner.SendEvents(ctx, key, sessionID, raw)
		if err == nil {
			if attempt > 0 {
				// A successful retry must leave a trace: without it, "this message actually waited 200ms
				// before being sent" is completely invisible in the logs, and if the window gets longer later
				// (for example tools get slower) there is no way to notice.
				s.log().Info("撞上工具执行窗口，重试后发送成功",
					"session", sessionID, "attempts", attempt+1)
			}
			return accepted, nil
		}
		// Only retry the "a different moment would work" kind (currently two codes, see
		// ma.IsTransientSendRejection). Other errors (invalid format, auth failure, Ark down) will not
		// get better however many times we retry, and waiting would only make the user stare at a
		// request that is bound to fail for one more second.
		if !ma.IsTransientSendRejection(err) {
			return nil, err
		}
		// If ctx is gone (user closed the page / upstream timeout), stop trying and hand back the last error.
		if ctx.Err() != nil {
			return nil, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			// Budget used up: hand back RequiresActionRejected, which arkFailure turns into 409.
			// Getting here means the tool really ran for a long time; that is for the queue to handle,
			// not this layer.
			s.log().Info("工具执行窗口在重试预算内没有关闭，交给调用方排队",
				"session", sessionID, "attempts", attempt+1)
			return nil, err
		}
		// Sleep until "the next step" or "budget used up", whichever comes first.
		//
		// This clamp is needed: tests squeeze the budget to milliseconds, and when the fixed step is
		// larger than the budget, "wait one full step" would jump past the deadline. Then not even one
		// retry happens, and the test looks like "retry did not work", while the real reason is just
		// that the interval is larger than the budget.
		wait := windowRetryStep
		if remaining < wait {
			wait = remaining
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(wait):
		}
	}
}

// ---------------------------------------------------------------------------
// Redirect: interrupt this round and say this instead
// ---------------------------------------------------------------------------

// InterruptRun only interrupts this round and sends no new message.
//
// # How it splits work with RedirectRun
//
// RedirectRun is "interrupt + send a new message right away", for cases where **the person
// sending the message and the one receiving the answer are the same** (the chat page: the
// browser keeps the SSE open, and it is the one listening before and after the interrupt).
//
// The Feishu path is different: each round has one goroutine that subscribes and is
// responsible for sending the answer back to the group, and **as soon as the interrupt takes
// effect, the running goroutine loses its stream and leaves**. With RedirectRun the new
// message would reach Ark with nobody waiting for its answer: that answer would land in the
// buffer of the message after it (two rounds of replies mixed up), or never be sent. So that
// side needs "only interrupt first, then send along the normal path once it holds the lock",
// which is this method. See interruptForRedirect in feishu/inbound.go.
//
// The real result of the interrupt comes back on the SSE stream (user.interrupt followed by
// session.status_idle); here we only make sure Ark accepted the interrupt.
func (s *Service) InterruptRun(ctx context.Context, runID string) error {
	interrupt := json.RawMessage(`{"type":"user.interrupt"}`)
	if _, err := s.AppendRunEvents(ctx, runID, []json.RawMessage{interrupt}); err != nil {
		return err
	}
	return nil
}

// RedirectRun interrupts this round, then immediately sends a new message: "stop doing that,
// do this instead".
//
// # Why it must be one primitive on the server, not two calls from the frontend
//
// If the frontend sends interrupt and then message, there is a full network round trip in
// between, and **that is again a rejection window**: the interrupt makes Ark stop the tool in
// hand, and while stopping it still only accepts tool_result / interrupt, so user.message is
// rejected (in production a tool result was seen coming back 48ms after the error). So
// "redirect" would turn into "only interrupted, the new message was not sent" about half the
// time: the user sees the agent stop and then nothing happens, with zero errors.
//
// On the server, the window in between is covered by the retries of
// sendEventsThroughWindow, and there is no network round trip between the two steps.
//
// # Why not put both events in one batch
//
// `AppendRunEvents(ctx, runID, []{interrupt, message})` looks simpler, and Ark does accept an
// array. But then **nothing guarantees the message is processed after the interrupt takes
// effect**: Ark accepts by batch and the interrupt takes effect asynchronously, so the message
// in the same batch may well be consumed before the agent has stopped. That is not "redirect"
// but "cutting in". Sending twice, retrying the second one if it hits the window, is what
// makes the meaning certain.
//
// # Return value
//
// Only the receipt of the new message is returned (the frontend needs it to draw the message).
// The interrupt receipt carries no information: it is just echoed back, see acceptedFrom in
// arkrun.go. The **real** result of the interrupt comes back on the SSE stream
// (user.interrupt followed by session.status_idle); see the comment on StreamRunEvents.
func (s *Service) RedirectRun(
	ctx context.Context, runID, text string,
) ([]json.RawMessage, error) {
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("%w: 消息不能为空", ErrInvalidArgument)
	}

	// Interrupt first. **If it fails, the whole thing fails; do not "treat it as a normal send if
	// the interrupt did not work"**: that would turn the user's "send now" into "go to the back of
	// the queue", while they see the message sent and the agent still busy with the previous task.
	// The two outcomes are too far apart to be decided silently by one failure.
	//
	// Go through AppendRunEvents instead of calling the runner directly: it has the frozen check,
	// the status write-back and the LastActivityAt refresh; bypassing it would mean copying those
	// rules (and slowly drifting).
	interrupt := json.RawMessage(`{"type":"user.interrupt"}`)
	if _, err := s.AppendRunEvents(ctx, runID, []json.RawMessage{interrupt}); err != nil {
		return nil, err
	}

	message, err := json.Marshal(map[string]any{
		"type": "user.message",
		"content": []map[string]any{
			{"type": "text", "text": text},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("拼消息事件失败: %w", err)
	}
	return s.AppendRunEvents(ctx, runID, []json.RawMessage{json.RawMessage(message)})
}

// extractEventID quickly extracts the id from the raw event JSON bytes.
func extractEventID(raw []byte) string {
	var env struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return ""
	}
	return env.ID
}

// SyncRunEvents actively pulls a session's event history from Ark, stores the events not yet
// stored, and updates the final run status.
//
// It is for when the frontend reopens the detail page or chat page after its stream broke
// during a chat (for example the browser switched tabs / the network flickered), to fill in
// the trailing events Ark produced after the disconnect (thinking, text replies, usage, end
// status, etc.).
//
// # ⚠️ This method makes external network calls; callers must think about who waits on it
//
// Step 4, `runner.ListEvents`, is a **cross-network** Ark call; when Ark is slow, so is this.
// It was once called directly inside the "open run detail page" request, and production
// measurements (44 deduplicated samples) showed:
//
//	/api/runs/{runId}         P50 320ms  P95 1398ms   ← slowest endpoint
//	/api/runs/{runId}/events  P50 412ms  P95 1157ms   ← second slowest
//	static files, same logs   P50 0~3ms  max 8ms      ← control group, proves it is not network/proxy
//
// And `isSyncableRunStatus` back-fills exactly when running / rescheduling / requires_action,
// so **sessions that are running pay this cost every time their detail page is opened**, and
// those are the ones users open most.
//
// So callers on the HTTP request path **must give it a short timeout**; see backfillFromArk in
// ark.go. No timeout is set here: the two Feishu recovery paths (feishu/resume.go,
// feishu/inbound.go) want "fill in as much as possible", not "return as fast as possible", and
// hard-coding a limit here would change their meaning too.
//
// # Per-step timings are logged here, not computed at call sites
//
// "Which step is slow" used to be guessable only by reading code: the log only had one total
// cost=, and you could not tell whether those 1.4 seconds were Ark being slow, fetching
// credentials being slow, or storing events one by one being slow. The five steps below are
// timed separately, and one Info line is logged at the end (raised to Warn when slower than
// slowBackfillThreshold, same idea as shared/infra/shelltools/timing.go).
// **Not Debug**: production log level does not show Debug, which would be the same as not
// adding it.
func (s *Service) SyncRunEvents(ctx context.Context, runID string) error {
	if s.runner == nil {
		return nil
	}

	start := time.Now()

	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("%w: 这次运行不存在", ErrNotFound)
	}

	key, err := s.arkKey(ctx, run.WorkspaceID, run.AgentID)
	if err != nil {
		return err
	}
	keyTook := time.Since(start)

	existing, err := s.store.ListRunEvents(ctx, runID, persistmodel.EventQuery{})
	if err != nil {
		return fmt.Errorf("读取本地事件失败: %w", err)
	}

	seenIDs := make(map[string]struct{}, len(existing))
	for _, ev := range existing {
		if id := extractEventID(ev.Raw); id != "" {
			seenIDs[id] = struct{}{}
		}
	}
	localTook := time.Since(start) - keyTook

	// **This call is the big one, timed on its own.** The production numbers table above exists because of it, so no more guessing.
	arkStart := time.Now()
	arkEvents, err := s.runner.ListEvents(ctx, key, run.ID)
	arkTook := time.Since(arkStart)
	if err != nil {
		// Report the time on failure too: "Ark took 3 seconds and then timed out" and "Ark returned a
		// 400 in 20ms" are completely different things, and if only the error is reported they look
		// identical in the logs.
		s.log().Warn("补拉方舟事件失败",
			"run", runID,
			slog.Duration("ark_took", arkTook),
			slog.Duration("total", time.Since(start)),
			"error", err.Error())
		return fmt.Errorf("拉取方舟历史事件失败: %w", err)
	}

	persisted := 0
	for _, raw := range arkEvents {
		if isDeltaFrame(raw) || isDoneFrame(raw) {
			continue
		}
		id := extractEventID(raw)
		if id != "" {
			if _, ok := seenIDs[id]; ok {
				continue
			}
			seenIDs[id] = struct{}{}
		}
		// ⚠️ **Store even when ctx has expired**: persistArkEvent internally uses
		// `context.WithoutCancel(ctx)` with its own 5-second budget (see the comment there).
		// When a back-fill is cut off by the timeout, the batch of events already pulled back should
		// not be lost with it: they are the only output of this round trip, and losing them means the
		// latency was paid for nothing.
		s.persistArkEvent(ctx, run.ID, raw)
		persisted++
	}
	persistTook := time.Since(arkStart) - arkTook

	total := time.Since(start)
	attrs := []any{
		"run", runID,
		// Five steps broken out. The ark one is the big one; the other three are local PG, normally single-digit milliseconds.
		slog.Duration("total", total),
		slog.Duration("ark_took", arkTook),
		slog.Duration("key_took", keyTook),
		slog.Duration("local_took", localTook),
		slog.Duration("persist_took", persistTook),
		slog.Int("ark_events", len(arkEvents)),
		slog.Int("persisted", persisted),
	}
	if total >= slowBackfillThreshold {
		s.log().Warn("补拉方舟事件偏慢", attrs...)
		return nil
	}
	s.log().Info("补拉方舟事件完成", attrs...)

	return nil
}

// RunExists only asks "does this run exist", with no side effects.
//
// **Do not use GetRun as the pre-check instead.** When the run is running and there is no
// active stream, GetRun also calls SyncRunEvents to pull events from Ark (see ark.go). That
// is meant for "opening the run detail page", while the SSE pre-check is about to open a
// stream right after, so syncing first is a wasted remote trip.
// This reads the Store once and returns ErrNotFound if it does not exist.
func (s *Service) RunExists(ctx context.Context, runID string) error {
	if _, err := s.store.GetRun(ctx, runID); err != nil {
		return fmt.Errorf("%w: 这次运行不存在", ErrNotFound)
	}
	return nil
}

// StreamRunEvents subscribes to the events of a run and calls onFrame for each frame.
//
// It blocks until Ark's stream ends or ctx is canceled (the browser disconnected). **Events
// are stored here**: forwarding and persisting are two things on the same path; see the file
// header.
//
// # The "agent deleted" freeze check is deliberately not done here
//
// The freeze only blocks writes (AppendRunEvents), not reads. There are reasons; do not
// "add it back" on a whim:
//
// 1. Users must be able to browse the history of frozen sessions; this feature explicitly
// requires it;
// 2. AppendRunEvents lets user.interrupt through, and the interrupt's receipt (user.interrupt
// followed by session.status_idle) comes back on exactly this stream; blocking the stream
// would mean the interrupt has no result.
//
// # "Run does not exist" is the opposite: it is now pre-checked before the handler writes headers
//
// There used to be a third reason here, saying "really blocking it would need a pre-check
// before the handler writes headers, not worth it". **Production logs proved that wrong**: a
// missing run is a permanent error that a retry can never fix, and the 200 + session.error
// frame shape cannot say "stop coming back". The measured cost was the same dead runId being
// subscribed 3 times in 6 seconds, each logging an ERROR (see the post-mortem in
// streamRunEvents in handler.go). So the handler now pre-checks with RunExists and returns a
// real 404, and the ErrNotFound in this method is only a fallback (for the narrow window
// where the run is deleted between the pre-check and opening the stream).
func (s *Service) StreamRunEvents(
	ctx context.Context, runID string, onFrame func(raw []byte) error,
) error {
	if s.runner == nil {
		return fmt.Errorf("%w: 服务端未接入方舟，无法订阅对话", ErrPrecondition)
	}

	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("%w: 这次运行不存在", ErrNotFound)
	}
	key, err := s.arkKey(ctx, run.WorkspaceID, run.AgentID)
	if err != nil {
		return err
	}

	// Before opening the stream, check whether events were missed by earlier broken streams; if so and there is no current stream, sync to fill them in first
	if run.Status == apimodel.RunRunning && !s.hasActiveStream(runID) {
		if err := s.SyncRunEvents(ctx, runID); err != nil {
			s.log().Warn("开流前同步方舟事件失败", "run", runID, "error", err.Error())
		}
	}

	cleanup := s.markStreamActive(runID)
	defer cleanup()

	return s.runner.Stream(ctx, key, run.ID, func(raw []byte) error {
		// Store first, then forward, and forward **the bytes after storing**. Both points matter:
		//
		// 1. Store first: if forwarding fails (the browser disconnected), the event is already saved
		// and can be seen next time the page is opened. The other way round, the event being
		// processed at the moment of disconnect would be lost forever.
		//
		// 2. Forward the stored bytes: Ark's original **has no sequence** (tested), and storing adds
		// it. Forwarding the original would give the SSE copy no sequence while the listRunEvents copy
		// has one, so the same event has two shapes in the frontend. timeline.ts sorts by sequence,
		// undefined makes `a.sequence - b.sequence` NaN, and the order of the whole timeline becomes
		// unpredictable.
		// This problem only showed up when running end to end against real Ark; reading the code,
		// both paths look "right".
		return onFrame(s.persistArkEvent(ctx, run.ID, raw))
	})
}

// persistArkEvent writes one frame pushed by Ark into the Store and returns **the bytes that
// should be forwarded to the browser**.
//
// Three kinds of frames are handled differently:
//
//   - event_start / event_delta: **not stored**, forwarded unchanged. Ark states clearly that
//     they are best-effort and do not enter the event history (see the Ark docs page on
//     streaming session events). Storing them would fill the history with types that do not
//     exist in the frontend RunEvent union at all.
//   - [DONE]: not JSON, not an event, forwarded unchanged.
//   - Everything else: stored, and the copy with sequence filled in is forwarded.
//
// When the write fails, fall back to forwarding the original, and only log without breaking
// the stream: the event is a fact that already happened on Ark, and cutting off the reply the
// user is watching because of one failed write costs far more.
func (s *Service) persistArkEvent(ctx context.Context, runID string, raw []byte) []byte {
	if isDeltaFrame(raw) || isDoneFrame(raw) {
		return raw
	}

	// Use a context without cancellation: when the browser disconnects ctx is Done immediately,
	// but this event should still be stored.
	// The timeout is short: storing is local PG, and taking 5 seconds means something is wrong.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	ev, created, err := s.store.AppendRunEvent(writeCtx, runID, raw)
	if err != nil {
		s.log().Warn("事件落库失败", "run", runID, "error", err.Error())
		return raw
	}
	s.applyEventToRun(writeCtx, runID, ev, created)
	return ev.Raw
}

// applyEventToRun writes back the effect of one event on the run: status, usage, threads,
// event count.
//
// These fields are what RunsPage / RunDetailPage display; without updating them, chats in the
// run history would stay at "running / 0 events / 0 tokens" forever.
func (s *Service) applyEventToRun(
	ctx context.Context, runID string, ev apimodel.RunEvent, created bool,
) {
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		// The run was deleted: the event is already stored and there is nothing to update; this is
		// the normal path.
		//
		// ⚠️ **But "cannot read it" is not "it was deleted".** The two used to share this one silent
		// return, with completely different results: a database hiccup (or this 5-second writeCtx
		// timing out) would leave status / eventCount / lastActivityAt / usage **all not updated**:
		// the session stays at running, 0 events, 0 tokens forever, and drops to the bottom of the
		// session list because lastActivityAt was not refreshed. The user sees "the chat is running,
		// but the history looks like nothing happened".
		//
		// The upstream "failed to store event" Warn only covers the AppendRunEvent step; this step
		// comes after it and is completely silent, so from the logs you cannot tell this layer exists.
		if !errors.Is(err, ErrNotFound) {
			s.log().Warn("回写 run 前重读失败，本条事件对 run 的影响全部丢失（状态/事件数/用量都不会更新）",
				slog.String("run", runID),
				slog.String("event", ev.Type),
				slog.Any("err", err))
		}
		return
	}

	run.EventCount = ev.Sequence

	// Every event refreshes "last active", which the session list is sorted by. It is here and
	// not in AppendRunEvents because this is the single funnel for all stored events: those
	// pushed by Ark (persistArkEvent), those sent by the user, and those back-filled by
	// SyncRunEvents all pass through it, so one place covers every source.
	run.LastActivityAt = apimodel.NowISO()

	switch ev.Type {
	case "session.status_running":
		run.Status = apimodel.RunRunning
	case "session.status_idle":
		// When stop_reason is requires_action the Ark side is still idle, but "stuck waiting for
		// confirmation" and "normal idle" are different things to the user; the frontend treats it as
		// a separate status (see apimodel.RunStatus).
		if arkframe.StopReason(ev.Raw) == "requires_action" {
			run.Status = apimodel.RunRequiresAction
			// ⚠️ **This branch deliberately does not call closeRun.**
			//
			// requires_action is not the end; it means "the agent is waiting for a tool result". Under
			// self_hosted, Ark pushes one such idle frame for every tool call, many times in one round of
			// chat. Writing endedAt / durationMs for it would give the run both "an end time" and "a
			// not-ended status", while the contract of those two fields is exactly "missing while not
			// ended" (see the comment on apimodel.Run; DurationMs is a pointer precisely to tell "0
			// milliseconds" from "not ended").
			//
			// This contradictory shape is exactly what was seen in a production incident, and it has two
			// real costs: the detail page shows the "end time" as a moment when the agent was actually
			// still running; and when investigating zombie sessions, an `endedAt` value leads people to
			// "everything ended, why did the status not settle", which is **the wrong direction**: the
			// real problem is that tool_result never came back (see reapStaleRuns in runwatcher.go).
			//
			// The next frame (tool_result comes back → status_running) pushes the status back; on a real
			// end, that idle frame has stop_reason end_turn and goes through the else branch to finish
			// normally.
			break
		}
		run.Status = apimodel.RunIdle
		s.closeRun(&run)
	case "session.status_terminated":
		run.Status = apimodel.RunTerminated
		s.closeRun(&run)
	case "session.status_rescheduled":
		run.Status = apimodel.RunRescheduling
	case "session.error":
		if msg := sessionErrorMessage(ev.Raw); msg != "" {
			run.ErrorSummary = msg
		}
	case "span.model_request_end":
		// ⚠️ **Only add up the one that was really newly written**, same reason as the reportEvent
		// gate below.
		//
		// All other writes in this function are **assignments** (status, event count, last active,
		// threads), so running them again gives the same result; only this one is `+=`. One Ark
		// session having several subscribers at once is normal (browser SSE / Feishu inbound / Feishu
		// recovery / scheduled task / server-side long-running subscription), and the same
		// span.model_request_end passes through here N times. Without checking created, **the row
		// count is right but the token count is multiplied by N**, and both the usage dashboard and
		// billing read it.
		//
		// Zero errors: events in the database are not duplicated (the unique index blocks that); only
		// those few numbers are quietly wrong. TestConcurrentSubscribersDoNotDoubleCountUsage pins it.
		if created {
			addUsage(&run.Usage, ev.Raw)
		}
	case "session.thread_created", "session.thread_status_running":
		s.touchThread(&run, ev)
	case "session.thread_status_idle", "session.thread_status_terminated":
		s.touchThread(&run, ev)
	}

	if _, err := s.store.SaveRun(ctx, run); err != nil {
		s.log().Warn("更新运行状态失败", "run", runID, "error", err.Error())
	}
}

// closeRun fills in the end time and duration of a run, settled **at this moment**.
//
// endedAt / durationMs are **missing** (not zero values) on runs that have not ended, so they
// are only written here. DurationMs is a pointer precisely to tell "0 milliseconds" from "not
// ended".
//
// ⚠️ Only paths that "are finishing up at the moment the final frame arrives" should use it:
// then `now` is when the session really ended. **Fallback reclaiming must not use it** (see
// closeRunAt).
func (s *Service) closeRun(run *apimodel.Run) {
	s.closeRunAt(run, time.Now().UTC())
}

// closeRunAt settles the end time and duration of a run **at the given moment**.
//
// # Why the caller must be able to choose the end moment
//
// Fallback reclaiming (reapStaleRuns / retireMissingRun in runwatcher) happens **long after**
// the session really stopped: the sweep ticks every 30 seconds, judging a zombie needs 30
// minutes without activity, and existing backlog may even have piled up for days. Settling with
// `now` at that point records not "how long this run ran" but "how long until someone noticed
// it was stuck".
//
// The reclaim batch of a production incident had exactly this shape: a session that was
// really active for 13 seconds was recorded as **36.7 hours**, plus several of 39.0h / 26.8h /
// 24.5h. This is not a display issue: `durationMs` is a stored field, and the "duration"
// column of RunsPage and OverviewPage renders it directly (formatDuration), so the run history
// showed a row of sweep tasks lasting tens of hours.
//
// So the reclaim path must pass `lastActivityAt`: that is the moment this session **last
// really had activity**, which is also when it actually stopped. Zero errors is the mark of
// this problem: the numbers were always there, just all wrong, and wrong in a way that looks
// like "these tasks really ran for a long time".
func (s *Service) closeRunAt(run *apimodel.Run, at time.Time) {
	run.EndedAt = apimodel.FormatISO(at)
	if started, err := time.Parse("2006-01-02T15:04:05.000Z", run.StartedAt); err == nil {
		ms := at.Sub(started).Milliseconds()
		if ms < 0 {
			ms = 0
		}
		run.DurationMs = &ms
	}
}

// touchThread maintains run.Threads from thread events.
//
// Threads are issued by Ark (thread ids look like sthr-...); here we only collect them for
// the threads section of RunDetailPage. When the same thread appears again, its status is
// updated rather than a new one added.
func (s *Service) touchThread(run *apimodel.Run, ev apimodel.RunEvent) {
	if ev.ThreadID == nil || *ev.ThreadID == "" {
		return
	}
	status := "running"
	switch ev.Type {
	case "session.thread_status_idle":
		status = "idle"
	case "session.thread_status_terminated":
		status = "terminated"
	}

	for i := range run.Threads {
		if run.Threads[i].ID == *ev.ThreadID {
			run.Threads[i].Status = status
			if status != "running" && run.Threads[i].EndedAt == "" {
				run.Threads[i].EndedAt = apimodel.NowISO()
			}
			return
		}
	}
	run.Threads = append(run.Threads, apimodel.RunThread{
		ID:        *ev.ThreadID,
		AgentName: agentNameOf(ev.Raw, run.AgentName),
		Status:    status,
		// The main thread's parent is null: **the key must be present**, so it is a pointer without omitempty.
		ParentThreadID: nil,
		StartedAt:      apimodel.NowISO(),
	})
}

// arkKey gets a workspace's Ark credentials, and gives an actionable Chinese hint when they are not configured.
func (s *Service) arkKey(ctx context.Context, workspaceID, agentID string) (ArkBindingKey, error) {
	// ⚠️ **Go through resolveArkSecretForAgent, do not read the store directly**: otherwise a
	// workspace with source=inherit would be judged unconfigured, showing up as "the settings
	// page clearly shows a masked key, but opening a chat says none is configured".
	//
	// This is the easiest place to miss in the reference mechanism, because it is not on the
	// settings page path: the settings page reads GetArkCredential, chats read this, and the two
	// paths are separate.
	//
	// **Use the resolver that takes an agent** (three levels: agent → workspace → account level):
	// this path needs "which key does this agent actually use", not "which key is this
	// workspace's default". With the one that takes no agent, agent-level credentials would be
	// silently ignored: that agent's chats would run on the workspace key, which works but bills
	// someone else, and nothing in the UI shows it.
	sec, err := s.resolveArkSecretForAgent(ctx, workspaceID, agentID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// This sentence is shown to the user as is, so it must say clearly what to do next.
			//
			// **The two kinds of "none" need different guidance**: never configured → go to workspace
			// settings and paste one; refers to the global one but that is gone (deleted, or never
			// configured) → configure it in global settings, or switch this workspace to a custom one.
			// The cost of the wrong guidance is that the user follows it and then finds it does not help.
			if row, rerr := s.store.GetArkSecret(ctx, workspaceID); rerr == nil &&
				apimodel.NormalizeArkSource(row.Source) == apimodel.SourceInherit {
				return ArkBindingKey{}, fmt.Errorf(
					"%w: 本空间引用的全局方舟 API Key 已不存在，请到全局设置重新配置，"+
						"或在空间设置里改用本空间自定义的 key", ErrPrecondition)
			}
			return ArkBindingKey{}, fmt.Errorf(
				"%w: 请先在空间设置里配置方舟 API Key（也可以只给这个 Agent 单独配一把）",
				ErrPrecondition)
		}
		return ArkBindingKey{}, err
	}
	region := sec.Region
	if region == "" {
		region = apimodel.RegionCNBeijing
	}
	return ArkBindingKey{
		WorkspaceID: workspaceID,
		AgentID:     agentID,
		APIKey:      sec.APIKey,
		Region:      region,
	}, nil
}

// arkFailure translates failures on the Ark side into Chinese the user can understand.
//
// 401/403 are recognized separately: that means "the key is invalid or not enabled", which
// the user can fix themselves; everything else counts as a service failure.
// The SDK's raw English error is not shown to the user, but it goes into the logs (logged by
// the handler layer).
//
// There are currently three kinds of account configuration errors, each recognized by a
// structured check, and all translated to 422 rather than 500:
//   - 401/403: invalid API Key;
//   - 404 ManagedAgentNotOpen: the account has not enabled the Managed Agents service;
//   - 400 InvalidParameter + service not open: the account has not enabled inference for some
//     model (ma.ModelEndpointNotOpen).
//
// The last two both really broke in production after the global credentials were switched,
// and the fix could only be found in the server logs.
//
// # The "a different moment would work" rejection has **two** codes, not one
//
// Under self_hosted, tools run in our own containers, and between Ark sending tool_use and
// the worker returning `user.tool_result` there is a window of tens of milliseconds in which
// Ark rejects `user.message`. If it fell into the default branch it would land in the
// handler's default → **500 + "internal service error, please retry later"**, while it is
// neither a server fault nor something that should make the user think the service is down:
// sending again is enough. Seen once in production: the tool result came back 48 milliseconds
// after the error.
//
// ⚠️ **That code is only one of them.** A burst-send stress test against the real service
// caught a second one: **HTTP 409 + code=RuntimeBusy**, with the text `retry later`: another
// exclusive command is running on the same session on the Ark side. Back then it fell into
// default and became 500, meaning that after the "fix" an identical problem was still there,
// just rarer. So this asks `IsTransientSendRejection` (checks both codes together); **do not
// change it back to asking one by one**: missing one shows up as users occasionally seeing a
// 500 that points at no real failure.
//
// Both could only be found by stress testing against the real service; the docs do not list
// them all. Before adding a new code, read the comments on those two functions in
// shared/infra/ma/errors.go.
func arkFailure(what string, err error) error {
	if ma.IsAuthError(err) {
		return fmt.Errorf("%w: 方舟拒绝了这把 API Key，请到空间设置里检查凭证是否有效",
			ErrPrecondition)
	}
	if ma.IsManagedAgentNotOpen(err) {
		// The account has not enabled Managed Agents: it can only be fixed by enabling it in the Ark
		// console; retrying will not help. It goes to ErrPrecondition (422, user can fix) instead of
		// default (500, service failure).
		//
		// Both the account id and request_id must be passed through: the former tells the user
		// **which account** is not enabled (which account a key belongs to cannot be seen in the AMA
		// UI), and the latter is what you give the Ark side when reporting a problem.
		// When the global credentials were once switched to a key from an account that had not
		// enabled it, creating sessions returned 500 for every workspace inheriting the global
		// credentials; the real reason was only in the logs, and users only saw "internal service
		// error".
		account := ma.NotOpenAccountID(err)
		if account != "" {
			account = "（账号 " + account + "）"
		}
		msg := fmt.Sprintf("这把 API Key 所属的方舟账号%s还没开通 Managed Agents 服务，请到方舟控制台开通后再试",
			account)
		// request_id is what you give the Ark side when reporting a problem; errors of this kind from Ark usually carry it, and without it we do not add an empty half sentence.
		if rid := ma.RequestID(err); rid != "" {
			msg += "；若已开通仍报此错，把 request_id " + rid + " 提供给平台管理员报障"
		}
		// Do not use fmt.Errorf("%w: message", ErrPrecondition): then only the sentinel is in the
		// chain and the original *ma.APIError is dropped, so later code (such as arkSyncStageOf)
		// asking ma.IsManagedAgentNotOpen / ma.RequestID can never get it.
		return &arkUserError{sentinel: ErrPrecondition, msg: msg, cause: err}
	}
	if modelID, ok := ma.ModelEndpointNotOpen(err); ok {
		// The model is valid, but **this key's account has not enabled it** (Ark models are enabled
		// per account, one by one). Most common after switching global credentials: the new account
		// has not enabled models the old account had. It goes to ErrPrecondition (422, user can fix)
		// instead of default (500, service failure). Once in production doubao-seed-evolving hit this
		// 400, users only saw "internal service error", and both fixes ("enable the model in the
		// console / switch to another model") were only in the server logs.
		msg := fmt.Sprintf(
			"这把 API Key 所属的方舟账号还没开通模型「%s」的推理服务："+
				"请到方舟控制台开通这个模型，或在 Agent 设置里换成一个已开通的模型",
			modelID)
		if rid := ma.RequestID(err); rid != "" {
			msg += "；若确认已开通仍报此错，把 request_id " + rid + " 提供给平台管理员报障"
		}
		return &arkUserError{sentinel: ErrPrecondition, msg: msg, cause: err}
	}
	if ma.IsTransientSendRejection(err) {
		// Use ErrConflict (409) rather than 422: it means exactly "a different moment would work",
		// while ErrPrecondition already means "this session is frozen" in this file (agent deleted /
		// disabled), which needs the user to change config to fix, the opposite of this case.
		return fmt.Errorf("%w: 上一个工具还在执行，请等它跑完再发", ErrConflict)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// arkUserError carries two things at once: the sentinel for errors.Is (decides which HTTP
// status the handler returns) and Ark's original error (so structured checks like
// ma.IsManagedAgentNotOpen / ma.RequestID still work after arkFailure translates it).
//
// It is equivalent to `fmt.Errorf("%w: %s", sentinel, msg)` plus another %w for the cause.
// Go's fmt.Errorf lets only one %w per format string take part in the Is chain (several %w
// do return a multierror, but errors.As then behaves differently from the single-chain
// assumption made everywhere else), so the type is written out explicitly.
//
// The shape of Error() is deliberately the same as fmt.Errorf("%w: message") ("sentinel
// text: message"): apihandler.userMessage cuts the user-facing message at the last ": ", and
// a different shape would make it fall back to "the request could not be completed".
type arkUserError struct {
	sentinel error
	msg      string
	cause    error
}

func (e *arkUserError) Error() string { return e.sentinel.Error() + ": " + e.msg }
func (e *arkUserError) Is(target error) bool {
	return target == e.sentinel
}
func (e *arkUserError) Unwrap() error { return e.cause }

// arkSpecFor translates an AMA agent into the input for creating an Ark Agent.
//
// # Tools use the whole built-in toolset, not the names in builtin_tools
//
// AMA's builtin_tools stores **single tool names** like read / grep / bash, while the tools
// of an Ark Agent take **toolset types** (agent_toolset_20260401). They are not the same
// granularity, and putting the former in directly gets rejected by Ark. Per-tool switches go
// through the configs field of the toolset, which is another layer of structure; for now the
// whole set is enabled, to be refined when there is a real need.
//
// An empty builtin_tools means this agent needs no tools, so it really gets none: attaching a
// full set of file system tools to a chat-only agent has side effects.
func arkSpecFor(a apimodel.Agent) ArkAgentSpec {
	spec := ArkAgentSpec{
		Name:        arkAgentName(a),
		Description: a.Description,
		// Append the <harness:env> block after the system prompt the user wrote.
		//
		// **Append, do not replace**: that prompt belongs to the user. It goes at the end because the
		// later an instruction is, the more likely the model follows it.
		//
		// The env block goes last: it is **data**, not instructions, and putting it between
		// instructions would blur the boundary. When there are no non-sensitive env values,
		// renderEnvPrompt returns an empty string and nothing is added here; never output an empty
		// <harness:env></harness:env>, see envprompt.go.
		System: a.System + renderEnvPrompt(a.Envs, nil),
		Model:  a.Model.ID,
	}
	// service_tier "fast" maps to Ark's model.speed fast; default / auto use the model default.
	if a.Model.ServiceTier == "fast" {
		spec.Speed = "fast"
	}
	if len(a.BuiltinTools) > 0 {
		spec.Tools = []string{ma.ToolsetDefault}
	}
	// ⚠️ **spec.CustomTools is always empty now, but that does not mean this path can be dropped.**
	//
	// Self-wakeup (schedule_wakeup) used to be the only custom tool, and it was removed with
	// that feature. When custom tools are added again, **their declarations must come back
	// here**: the built-in toolset is a fixed 8 tools, and Ark does not tell the model about a
	// tool that was never declared, so the model never calls it, and no error is reported. The
	// executor side is EnvironmentWorkerOptions.CustomTools in core/worker.go; both sides are
	// needed.
	return spec
}

// arkAgentName names the Agent on the Ark side.
//
// It includes the AMA agent ID, so in the Ark console you can see at a glance whom it belongs
// to and who created it; otherwise a pile of agents with the same name like "data analyst"
// cannot be matched up. Ark names are limited to 1–100 characters, and Chinese works
// (tested with a probe).
func arkAgentName(a apimodel.Agent) string {
	name := strings.TrimSpace(a.Name)
	if name == "" {
		name = "ama-agent"
	}
	full := fmt.Sprintf("%s (%s)", name, a.ID)
	if len([]rune(full)) > 100 {
		full = string([]rune(full)[:100])
	}
	return full
}

// ---------------------------------------------------------------------------
// Small helpers for frames and events
// ---------------------------------------------------------------------------

// isDoneFrame reports whether this is Ark's stream end frame `[DONE]`. It is not JSON.
func isDoneFrame(raw []byte) bool {
	return strings.TrimSpace(string(raw)) == "[DONE]"
}

// isDeltaFrame reports whether this is an incremental preview frame.
//
// Measured, Ark's incremental frames look like this (**they use the same type field as final
// events**, told apart by value):
//
//	{"type":"event_start","event":{"id":"sevt-...","type":"agent.message"}}
//	{"type":"event_delta","event_id":"sevt-...","delta":{"type":"content_delta",...}}
func isDeltaFrame(raw []byte) bool {
	switch arkframe.Type(raw) {
	case "event_start", "event_delta":
		return true
	}
	return false
}

// sessionErrorMessage gets the error message from a session.error event.
func sessionErrorMessage(raw []byte) string {
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return ""
	}
	return env.Error.Message
}

// agentNameOf gets agent_name from a thread event, falling back to the name on the run when missing.
func agentNameOf(raw []byte, fallback string) string {
	var env struct {
		AgentName string `json:"agent_name"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && env.AgentName != "" {
		return env.AgentName
	}
	return fallback
}

// addUsage adds the model_usage of one span.model_request_end into the total.
//
// All four fields must be added: the frontend RunsPage adds them up, and missing one gives NaN.
func addUsage(sum *apimodel.TokenUsage, raw []byte) {
	var env struct {
		ModelUsage *apimodel.TokenUsage `json:"model_usage"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.ModelUsage == nil {
		return
	}
	sum.InputTokens += env.ModelUsage.InputTokens
	sum.OutputTokens += env.ModelUsage.OutputTokens
	sum.CacheReadInputTokens += env.ModelUsage.CacheReadInputTokens
	sum.CacheCreationInputTokens += env.ModelUsage.CacheCreationInputTokens
}

// TitleFromEvents computes the session title a batch of upstream events should fill in: take
// the text of the first user.message, flatten it to one line, and truncate. Returns an empty
// string when there is no usable text (the caller then does not fill it in).
//
// # Why this one is exported instead of the two private functions
//
// The title is a **cross-package contract**: the feishu side attaches a second content block
// to user.message (`<harness:lark>`), and "that block must not get into the title" can only
// be verified by running both steps together. If feishu's tests copied firstUserText +
// truncateTitle, they would verify "is the copy right": if core removed the filter, the copy
// would still pass, so nothing would be verified.
//
// So what is exported is **the combined step** (exactly the combination AppendRunEvents
// really uses), and the two private functions stay private.
func TitleFromEvents(raw []json.RawMessage) string {
	text := firstUserText(raw)
	if text == "" {
		return ""
	}
	return truncateTitle(text)
}

// firstUserText gets the text of the first user.message from upstream events, used to fill in
// the title.
//
// ⚠️ **Skip the `<harness:*>` blocks injected by the platform**; reason in HarnessTagPrefix in
// envprompt.go: those blocks are not what the user said, and putting them in the title would
// turn the whole session title into XML, with no way to recover. The check uses the prefix,
// not the full tag name, so a future third harness block **needs no change here**; forgetting
// to change it would show up as a wrong title with zero errors.
func firstUserText(raw []json.RawMessage) string {
	for _, item := range raw {
		var ev struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(item, &ev); err != nil || ev.Type != "user.message" {
			continue
		}
		var sb strings.Builder
		for _, block := range ev.Content {
			if block.Type != "text" {
				continue
			}
			// Blocks injected by the platform are skipped whole: they are not what the user said.
			if strings.HasPrefix(strings.TrimSpace(block.Text), HarnessTagPrefix) {
				continue
			}
			sb.WriteString(block.Text)
		}
		if text := strings.TrimSpace(sb.String()); text != "" {
			return text
		}
	}
	return ""
}

// isInterruptOnly reports whether this batch of upstream events is only interrupts (then the run should not be set back to running).
func isInterruptOnly(raw []json.RawMessage) bool {
	for _, item := range raw {
		if arkframe.Type(item) != "user.interrupt" {
			return false
		}
	}
	return len(raw) > 0
}

// truncateTitle uses the start of the message as the title, behaving the same as the frontend
// truncateTitle (api.ts:309).
//
// Length is cut by **rune** count, not bytes: 30 bytes is only 10 Chinese characters, far from
// the UTF-16 meaning of `oneLine.length > 30` in the frontend. When Chinese and ASCII are all
// in the BMP, the rune count equals the number of UTF-16 code units, so both sides give the
// same result.
func truncateTitle(text string) string {
	oneLine := strings.Join(strings.Fields(text), " ")
	runes := []rune(oneLine)
	if len(runes) > 30 {
		return string(runes[:30]) + "…"
	}
	return oneLine
}

// hostOnRun returns the host machine (id, source) this run should snapshot: it has a value
// only when the agent explicitly chose the host executor; an empty executorType (default
// Docker container) always gives empty strings, even if the host_id field has stale data.
// source goes through EffectiveHostSource, and existing NULL rows are read as workspace
// assignment.
func hostOnRun(a apimodel.Agent) (string, string) {
	if a.ExecutorType == "host" {
		return a.HostID, apimodel.EffectiveHostSource(a.ExecutorType, a.HostSource)
	}
	return "", ""
}

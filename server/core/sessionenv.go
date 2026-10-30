// The implementation of "which environment variables to inject for this session", called back by
// shared/infra/dockertools when it injects into the container.
//
// It handles two kinds of things. They share one source because both are looked up by
// arkSessionID and both go through the same per-exec injection channel:
//
//	AMA CLI credentials   AMA_TOKEN / AMA_BASE_URL / AMA_WORKSPACE_ID / AMA_SESSION_ID
//	user-entered env      the kv pairs configured on the agent (see apimodel.AgentEnv)
//
// # This file fixes a bug that "was claimed to exist but was never implemented"
//
// The body of the built-in ama skill tells the model "credentials are already set up in the
// container, just use them", and the CLI and cli/README.md also say "injected automatically by the
// platform", but before this **nothing in the whole repo injected AMA_TOKEN**. So an agent with that
// skill checked was sure to fail when running `ama` in the container, and the failure message made
// the model turn around and tell the user "the platform side has not configured injection", a
// statement the user can do nothing about. See the file header of integration_ama.go for details.
//
// # Why the per-exec channel
//
// It uses per-exec ExecSpec.Envs; for the reason see the file header of
// shared/infra/shelltools/credential.go (SessionSpec.Envs can't be changed after creation, and the
// container TTL is longer than the lifetime of the Feishu user token).
//
// The AMA token is signed by us and **never expires**, but it still goes through the same per-exec
// channel, because it is "derived + row created lazily": the row is only really written to the store
// on the first execution.
//
// # Whose permissions: the agent creator (existing agents fall back to the workspace owner)
//
// See personalCredentialSubject.
//
// ⚠️ **This key is sensitive, and that must be stated clearly**: it can operate on AMA's own data;
// the agent in the container can, as that person, modify workspaces, delete agents, and read all run
// history. Narrowing it would need a scope field in auth (today the meaning is "calling the API with
// the key is fully equivalent to that person logging in", see auth/apikey.go), which is a separate
// piece of work. Until then, **checking AMA authorization means handing that person's platform
// permissions to this agent**.
package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/shared/infra/shelltools"
)

// SessionEnvSource builds the environment variables to inject into the container for an Ark session.
//
// It implements shelltools.SessionEnvSource. It is a small separate type instead of having Service
// implement it directly, so core's main type does not depend on shared/infra/dockertools at the type
// level.
type SessionEnvSource struct {
	svc *Service
	// keys is the entry point for deriving the AMA token. When nil, those three variables are not injected (user-entered env still is).
	keys ManagedKeyIssuer
	// baseURL is AMA's own public address, injected for the CLI.
	//
	// **When empty, AMA_BASE_URL is not injected**, and the CLI falls back to its built-in default
	// (the production address).
	// ⚠️ It must be set for local development, or the ama in the container will operate on
	// **production** data: a trap of "runs fine locally, but actually changes production".
	baseURL string
	// larkUser fetches the workspace owner's Feishu user token (so lark-cli in the container can read
	// group history).
	//
	// When nil, "Feishu user authorization checked" gives an explicit error instead of silently not
	// injecting; see addLarkUserEnvs.
	//
	// ⚠️ Unlike the fields above, the type is *LarkUserProvider rather than a narrow interface. The
	// reason is that it is also the provider for that card: **fetching credentials and checking status
	// must use the same object**, or "the row written back after renewal" and "the row the card reads"
	// would go two different ways, leading to "the card says OK, injection says there is none".
	larkUser *LarkUserProvider
	log      *slog.Logger
}

// ManagedKeyIssuer is the ability to "get the plaintext of this person's AMA managed key".
//
// It has only one method, implemented by auth.Service. It is declared as an interface so it can be
// tested offline, and so the dependency surface here is small enough to read at a glance.
type ManagedKeyIssuer interface {
	// EnsureManagedKey gets (creating if needed) the plaintext of this person's managed key.
	//
	// **Idempotent**: repeated calls return the same string, and the store has only one row. See the
	// deterministic derivation in auth/managedkey.go; that is what allows this feature to be called
	// over and over on the hot path of "every command executed".
	EnsureManagedKey(ctx context.Context, email string) (string, error)
}

// NewSessionEnvSource builds a session env source.
//
// svc is required. When keys is nil (or reports itself as not configured) it **still returns a
// usable object**: user-entered env and AMA credentials are two independent things, and the former
// should not stop working just because the latter is not configured.
// larkUser can likewise be nil (the server has no Feishu integration): then an agent with that
// authorization checked gets an explicit error instead of silent non-injection.
func NewSessionEnvSource(
	svc *Service, keys ManagedKeyIssuer,
	larkUser *LarkUserProvider, baseURL string, logger *slog.Logger,
) *SessionEnvSource {
	if svc == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &SessionEnvSource{
		svc:      svc,
		keys:     keys,
		larkUser: larkUser,
		baseURL:  strings.TrimSpace(baseURL),
		log:      logger,
	}
}

// Compile-time check that it satisfies the interface on the shelltools side.
//
// **This line can't be dropped**: if the signatures don't match, the error is delayed until the
// wiring line, or isn't reported at all and shows up as "configured but never took effect once".
var _ shelltools.SessionEnvSource = (*SessionEnvSource)(nil)

// SessionEnvs builds the environment variables to inject for this session.
//
// An empty map with a nil err is a **normal result** (this agent has neither AMA authorization
// checked nor any env configured). An error is only returned when "it should have been obtainable
// but failed".
//
// ⚠️ **envs may still be non-empty when err is non-nil**; this is a deliberate "partial success":
// the user-entered env has already been computed and should not be thrown away just because the AMA
// credentials can't be fetched. Callers must handle it as "record the error + inject what was
// obtained as usual", not by the Go habit of "discard everything if there is an err".
func (s *SessionEnvSource) SessionEnvs(
	ctx context.Context, arkSessionID string,
) (map[string]string, []string, []shelltools.EnvAlias, error) {
	// The Ark sessionID is the Run.ID. Not finding it is normal: probes and paths that don't store a Run all behave this way.
	run, err := s.svc.store.GetRun(ctx, arkSessionID)
	if err != nil {
		return nil, nil, nil, nil
	}
	if strings.TrimSpace(run.WorkspaceID) == "" || run.AgentID == "" {
		return nil, nil, nil, nil
	}
	agent, err := s.svc.store.GetAgent(ctx, run.AgentID)
	if err != nil {
		// The agent was deleted while an old session continues the conversation. No config to go on → inject nothing.
		// **Don't fall back to "inject"**, for the same reason as in that section of credentialinject.go.
		return nil, nil, nil, nil
	}

	envs := make(map[string]string)
	var secretKeys []string

	// -- Zero: credential aliases (declarations only, no values) ------------------------------
	//
	// "Inject a platform credential once more under a user-chosen variable name", for compatibility
	// with third-party CLIs that don't read our default variable names. ⚠️ **Only the declarations
	// are passed down here; values are never fetched here**, for two reasons:
	//
	//  1. **This function can't see those values.** Platform variables are only all visible at the
	//     merge point in shelltools, so aliases can only be resolved there.
	//  2. **Taking the value there from "the merged result" makes aliases inherit the authorization
	//     check naturally**: when the agent hasn't checked the matching authorization, that key isn't
	//     in the merged result at all, so the alias naturally produces nothing. Fetching it here
	//     ourselves would bypass the integrationIds checkbox, with zero errors.
	//
	// Dirty data gets filtered here (the write path already validates, but the store may hold data
	// saved before validation went live): entries with an invalid name or a source not in the list
	// are dropped and logged.
	var aliases []shelltools.EnvAlias
	for _, a := range agent.EnvAliases {
		key, from := strings.TrimSpace(a.Key), strings.TrimSpace(a.From)
		if key == "" || from == "" {
			continue
		}
		if shelltools.IsPlatformReservedEnv(key) || !shelltools.IsAliasableEnv(from) {
			s.log.WarnContext(ctx, "凭据别名不合法，已忽略这一条",
				"agent", agent.ID, "key", key, "from", from)
			continue
		}
		aliases = append(aliases, shelltools.EnvAlias{Key: key, From: from})
	}

	// -- One: user-entered env --------------------------------------------------------------
	//
	// ⚠️ **Reserved keys are blocked right here, not left to downstream.**
	//
	// It used to be "put them in as is, and rely on downstream to filter by platformReservedEnvs",
	// and that had a real trap: the map downstream receives **already mixes both kinds of env**
	// (user-entered + platform-injected), so filtering by name also ate the AMA_TOKEN /
	// AMA_WORKSPACE_ID the platform had just computed. Then `ama` in the container always reported
	// "no credentials", while the log said "a custom environment variable tried to override", which
	// **points in completely the wrong direction** (it looks like the user filled in junk, but
	// agent.Envs was actually empty).
	//
	// With them blocked at the source, any reserved key seen downstream **must be platform-injected**,
	// with no more guessing. The write path (validation in core/agent.go) is the first gate, this is
	// the second, and the shelltools merge order (platform wins last) is the third. With all three in
	// place, missing one doesn't create a hole.
	for _, e := range agent.Envs {
		key := strings.TrimSpace(e.Key)
		if key == "" {
			continue
		}
		if shelltools.IsPlatformReservedEnv(key) {
			// Log instead of dropping silently: silence would make the user think their setting took effect,
			// and then spend a long time on "why isn't my token being used".
			s.log.WarnContext(ctx, "自定义环境变量用了平台保留名，已忽略这一条",
				"agent", agent.ID, "key", key)
			continue
		}
		envs[key] = e.Value
		if e.Secret {
			// Ones marked sensitive must be scrubbed from command output; see shelltools/scrub.go.
			secretKeys = append(secretKeys, key)
		}
	}

	// -- Two: AMA CLI credentials -----------------------------------------------------------
	//
	// Only injected when AMA authorization is checked. **This step can't be skipped, or that checkbox
	// is dead code**: the user thinks they didn't grant it, but the token still goes into the
	// container. Same lesson as that section in credentialinject.go.
	if agentAllowsIntegration(agent, apimodel.IntegrationAMA) {
		if err := s.addAMAEnvs(ctx, agent, run.WorkspaceID, arkSessionID, envs, &secretKeys); err != nil {
			// ⚠️ **Checked but unobtainable is an error; it must be reported upward, not silently downgraded.**
			//
			// This and "the user didn't check it" are two completely different cases, handled in opposite ways:
			//
			//	not checked → silently don't inject. That is what the user wants; a reminder on every
			//	              conversation turn is pure noise.
			//	checked     → **error**. They explicitly asked for this and the platform can't provide it;
			//	              silently not injecting would turn the symptom into "running ama in the
			//	              container says there are no credentials", and that points at the CLI, not
			//	              at "the server has no managedKeySecret configured".
			//
			// This used to just log a Warn and continue, which was wrong: the log is on the server,
			// while the person seeing the symptom is in the conversation. The two never meet, turning
			// a fixable config problem into a strange symptom whose root cause can't be found.
			//
			// The workspace is included so the error itself says "which workspace". The upper layer
			// (withCredentials) records it as a Warn and downgrades to not injecting; that downgrade is
			// correct (the whole session must not become unusable), but **the error message must not
			// be lost here**.
			//
			// ⚠️ **Still return the env computed so far** (not nil): user-entered env and AMA
			// credentials are two independent things, and failing to get AMA should not throw away
			// their env too. Callers handle it as "record one entry if there is an err, but inject as
			// usual if envs is non-empty".
			return envs, secretKeys, aliases, fmt.Errorf(
				"这个 agent 勾了「AMA」授权，但取不到凭据（空间 %s）: %w",
				run.WorkspaceID, err)
		}
	}

	// -- Three: Feishu user credentials (so lark-cli in the container can read group chat and thread history) --
	//
	// ⚠️ **The key difference from the sections above: the credential is read from our own store and
	// may need renewing on the spot.** The other sources are deterministic derivations (ama), while
	// this token was saved when the user did OAuth authorization and expires in about 2 hours.
	//
	// Renewal happens on this very path (UserAccessToken refreshes internally as needed), which is
	// exactly why **injection must go through per-exec ExecSpec.Envs**: SessionSpec.Envs can't be
	// changed after creation, and the container lives longer than this token (see the decision
	// record in shelltools/credential.go). It is fetched again on every execution, so "renew naturally
	// on expiry" comes for free.
	if agentAllowsIntegration(agent, apimodel.IntegrationLarkUser) {
		if err := s.addLarkUserEnvs(ctx, agent, run.WorkspaceID, envs, &secretKeys); err != nil {
			// Same rule as AMA: **checked but unobtainable must be reported as an error**, not silently
			// downgraded. If silent, the symptom is "the agent says lark-cli reports not_configured",
			// which points at the CLI config, not at "the workspace owner hasn't done Feishu user
			// authorization yet".
			//
			// ⚠️ Still return the env computed so far (not nil), for the same reason as the two sections above.
			return envs, secretKeys, aliases, fmt.Errorf(
				"这个 agent 勾了「飞书用户授权」，但取不到凭据（空间 %s）: %w",
				run.WorkspaceID, err)
		}
	}

	if len(envs) == 0 {
		// ⚠️ **Aliases are still returned**, not swallowed along with it: they are **derived**, and
		// their values are only resolved at the merge point in shelltools, so "no env computed here"
		// and "are there aliases" are two unrelated things.
		return nil, nil, aliases, nil
	}
	return envs, secretKeys, aliases, nil
}

// personalCredentialSubject answers "whose credentials should this agent's **personal**
// authorizations use".
//
// # ⚠️⚠️ This is the core of this security change; read this whole section before changing it
//
// The answer used to always be the **workspace owner** (firstOwnerEmail written out in four places).
// That rule had a zero-error hole: anyone who could edit an agent could check some personal
// authorization, and their agent would act **as the owner personally**, with no notice to the owner.
// The credentials injected into the container can be read by the agent with a single `env`, and
// what it can do equals what that employee can do.
//
// The rule now is **by agent.CreatedBy**, paired with the gate on the write path
// (ensureIntegrationsAllowed: only the creator can add or remove personal authorizations): **whoever
// is allowed to inject is whose credentials are used**. The two places must use the same rule; if
// the write path checks A and runtime uses B, the gate is just decoration.
//
// # Existing agents: fall back to the owner when CreatedBy is empty
//
// `CreatedBy` is a field added later, and existing agents may not have the key at all
// (apimodel.Agent states this is a long-lived valid state). Then it **falls back to the workspace
// owner**, i.e. the behavior before the change.
//
// ⚠️ This is an **explicit** migration decision, not "forgot to handle it":
//
//	no fallback (return empty → don't inject)  existing agents' Feishu/AMA abilities **silently
//	                                           disappear**; the symptom is "it was fine yesterday,
//	                                           today the agent says there are no credentials", and
//	                                           nothing points at this change. This repo explicitly
//	                                           forbids this kind of "flip the default direction by
//	                                           relying on missing values".
//	fall back to the owner (as now)            existing agents behave **exactly as before**; new ones
//	                                           follow the new rule.
//
// The cost is that the hole remains on existing agents. This is a deliberately accepted trade-off:
// **it has a floor** (the set of existing agents won't grow any more, and every new one carries
// CreatedBy), while the opposite would break production features on the spot. The log states that the
// fallback rule was used, so whoever is debugging can see it at a glance.
//
// # Non-interactive triggers (scheduled tasks / Feishu messages)
//
// Those paths **have no "current user"**, so this deliberately ignores the actor in ctx and only
// looks at the creator recorded on the agent. So a scheduled task and a person clicking "run once"
// use **the same credentials**, and the behavior is predictable.
//
// ⚠️ **What happens after the creator leaves or revokes authorization**: fetching the credentials
// fails, and the caller treats it as "checked but unobtainable" → **error** (not silent
// non-injection), with the error message saying whose credentials they were. That is deliberate:
// silently downgrading would make a scheduled task fail once a day with no one knowing why, while an
// error goes into RunEvent and is visible in the run history. See those sections in each addXxxEnvs.
func (s *SessionEnvSource) personalCredentialSubject(
	ctx context.Context, agent apimodel.Agent, workspaceID string,
) (string, error) {
	return s.svc.personalCredentialSubjectFor(ctx, agent, workspaceID, s.log)
}

// personalCredentialSubjectFor is the implementation of the one above, attached to Service.
//
// It is attached to Service so other injection sources (added later) share the same rule: if each
// wrote its own copy, drift like "A uses the owner, B uses the creator" would not be reported. The
// logger is passed in by the caller.
func (s *Service) personalCredentialSubjectFor(
	ctx context.Context, agent apimodel.Agent, workspaceID string, log *slog.Logger,
) (string, error) {
	if creator := strings.TrimSpace(agent.CreatedBy); creator != "" {
		return creator, nil
	}
	ws, err := s.store.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return "", fmt.Errorf("空间不存在: %w", err)
	}
	owner := firstOwnerEmail(ws.Members)
	if owner == "" {
		return "", fmt.Errorf("%w: 空间没有 owner，取不到可用身份", ErrPrecondition)
	}
	// State that the fallback rule was used. **This log line can't be dropped**: it is the only
	// traceable clue to "why this agent uses the owner's identity", and that is the first question to
	// ask when investigating a concern about privilege overreach.
	if log == nil {
		log = slog.Default()
	}
	log.InfoContext(ctx, "agent 没有记录创建者，个人授权回落到空间 owner（存量行为）",
		"agent", agent.ID, "workspace", workspaceID, "owner", owner)
	return owner, nil
}

// addLarkUserEnvs merges the two variables lark-cli recognizes into envs.
//
// The identity rule is **the agent creator** (existing agents fall back to the workspace owner), see
// personalCredentialSubject. Feishu has one more trade-off: a user identity can only see the
// conversations that person can see, so after switching the rule to the creator, "can't read it if
// the creator isn't in that group" replaces the old "can't read it if the owner isn't in it". Both
// the feature surface and the overreach surface narrow to "the person allowed to inject", matching
// the gate on the write path.
//
// ⚠️ **The two variables must be given together**: when lark-cli lacks the app id it reports
// "LARKSUITE_CLI_USER_ACCESS_TOKEN is set but LARKSUITE_CLI_APP_ID is missing", which points at
// something the user can't fix. So shelltools.LarkUserEnvs builds both at once, instead of writing
// two assignments by hand here.
func (s *SessionEnvSource) addLarkUserEnvs(
	ctx context.Context, agent apimodel.Agent, workspaceID string,
	envs map[string]string, secretKeys *[]string,
) error {
	if s.larkUser == nil {
		return fmt.Errorf("%w: 服务端没接入飞书用户授权", ErrPrecondition)
	}

	// Pick the person by **the same rule** as the other three; see personalCredentialSubject.
	subject, err := s.personalCredentialSubject(ctx, agent, workspaceID)
	if err != nil {
		return err
	}

	// This step refreshes internally as needed and writes the new token back to the store.
	//
	// ⚠️⚠️ **workspaceID must be passed down**; this is the easiest line in this function to get wrong.
	// A Feishu app is a workspace-level resource, and the token is bound to the app: if it isn't passed,
	// downstream guesses an app among **all** of this person's workspaces, so "an agent running in
	// workspace C gets the token for workspace A's app"; lark-cli returns 99991668 "token invalid", or
	// it happens to be valid but can't read that group. **Neither failure points at "the wrong app was
	// picked"**, so runtime errors can't be relied on to catch this.
	//
	// This used to pass only the owner (workspaceID was right there in the arguments but got dropped).
	// One user was measured to have three apps belonging to three workspaces, so this is not a
	// theoretical problem.
	token, appID, err := s.larkUser.UserAccessToken(ctx, subject, workspaceID)
	if err != nil {
		// The most common case is "they haven't done Feishu user authorization yet", which the user can
		// fix themselves, but it must still be reported upward: if silent, the symptom points at
		// lark-cli instead of the authorization.
		// **The err does not contain the token.**
		//
		// ⚠️ Include **whose** credentials: after switching the rule to the creator, this message is the
		// only clue to "who needs to go authorize" (even more so for non-interactive triggers, when there
		// is no "current user" to ask).
		return fmt.Errorf("取 %s 的飞书用户凭据失败（多半是他还没做飞书用户授权）: %w",
			subject, err)
	}

	for k, v := range shelltools.LarkUserEnvs(token, appID) {
		envs[k] = v
	}
	// ⚠️ **Only the token counts as a secret**, not the app id (cli_xxx is a public identifier, and
	// scrubbing it would hide "which app this credential belongs to" during debugging). See the table
	// in shelltools/scrub.go.
	*secretKeys = append(*secretKeys, shelltools.EnvLarkUserAccessToken)
	return nil
}

// ⚠️ The three variable names use the constants on the shelltools side, **not literals**: they must
// match cmd/ama-cli/main.go exactly, and a name mismatch fails with zero errors (the CLI says "the
// platform has not configured injection", our log says injection succeeded). With the constants
// defined in only one place, the two sides can't drift.
func (s *SessionEnvSource) addAMAEnvs(
	ctx context.Context, agent apimodel.Agent, workspaceID, arkSessionID string,
	envs map[string]string, secretKeys *[]string,
) error {
	if s.keys == nil {
		return fmt.Errorf("%w: 服务端没配 auth.managedKeySecret", ErrPrecondition)
	}

	// Pick the person by **the same rule** as the others (the agent creator, existing agents fall back
	// to the owner); see personalCredentialSubject.
	//
	// ⚠️ This credential is **the platform's own** (it can change workspaces, change agents, start
	// runs), so after switching the rule to the creator, what is injected into the container is "the
	// creator's permissions on AMA", not the owner's. That is exactly the intended effect: the person
	// checking this authorization must be the creator, so their own permissions are what get used.
	subject, err := s.personalCredentialSubject(ctx, agent, workspaceID)
	if err != nil {
		return err
	}

	token, err := s.keys.EnsureManagedKey(ctx, subject)
	if err != nil {
		if errors.Is(err, auth.ErrManagedKeyUnavailable) {
			// The server has no secret configured. An admin has to configure it; this is not a runtime failure.
			return fmt.Errorf("%w: %v", ErrPrecondition, err)
		}
		// **The err does not contain the token**: the auth side only returns diagnostic text.
		return fmt.Errorf("取 %s 的 AMA 凭据失败: %w", subject, err)
	}

	envs[shelltools.EnvAMAToken] = token
	// AMA_TOKEN is a credential and must be scrubbed from command output: **otherwise a single env
	// from the agent would put it into the Ark context, into the permanent RunEvent record, and into
	// the user's browser**. See the file header of shelltools/scrub.go.
	*secretKeys = append(*secretKeys, shelltools.EnvAMAToken)

	// Workspace id (ws_xxx). ⚠️ **Not the key in the URL**: the CLI uses it directly to build
	// /api/workspaces/{id}/...; passing the key gives a 404 that doesn't point at the mixed-up identifiers.
	envs[shelltools.EnvAMAWorkspaceID] = workspaceID

	// The current session id (which is the Ark session id). The agent in the container uses it to
	// build the per-session passthrough APIs under /api/runs/{id}/ark/... (list/attach files, threads,
	// and so on), without having to guess the current session. Not a secret, but still reserved in
	// case the user forges it.
	envs[shelltools.EnvAMASessionID] = arkSessionID

	// If baseURL is empty, don't provide it; let the CLI use its built-in production default.
	if s.baseURL != "" {
		envs[shelltools.EnvAMABaseURL] = s.baseURL
	}
	return nil
}

// This file implements "Feishu user authorization" as an IntegrationProvider.
//
// It fills one specific gap: the `<harness:lark>` block (server/feishu/larkprompt.go) already
// tells the agent "fetch context yourself" and gives it a few `lark-cli` commands, **but the
// CLI in the container has no credentials**, and every run returns
// `{"ok":false,"error":{"subtype":"not_configured"}}`. So those commands are just decoration.
// This card is the half that supplies the credentials.
//
// # ⚠️⚠️ Why this provider **must** store data, while other authorization sources deliberately do not
//
// The header of integration.go says "state is always looked up live, never stored", because
// "authorization is clicked by the user **on the platform side**, the platform is the only
// authority; keeping a copy gets out of sync with the platform, and the symptom of that is
// 'the page says authorized, but tool calls still fail'".
//
// **That rule is about "another platform's state", while what is stored here is "the
// credential itself".** The difference is decisive:
//
//	sources that can swap live   when needed, swap app credentials for a user token on the spot;
//	                              the platform is the source of truth, we keep nothing
//	ama                           derived deterministically, no need to store
//	**Feishu user OAuth**          the access_token expires in about 2 hours, and **the only way to
//	                              renew it is the refresh_token we store ourselves**. Feishu gives it
//	                              to us only once, at the moment authorization succeeds, and never
//	                              again; without storing it we can never renew.
//
// So this is not "caching platform state to save a network request"; it is "without storing
// it, the feature does not exist". The rule is still the one in integration_ama.go: **who
// holds the authority over this thing?** The refresh_token is something Feishu issued to us
// and only we have → the authority is in our database → it may be stored.
//
// ⚠️ **Do not take this file as permission to "cache other platforms' authorization state".**
// That is still forbidden. What the two exceptions (ama and this one) share is "the source of
// truth is with us", not "keeping a copy is more convenient".
//
// # ⚠️ What is stored is decryptable ciphertext, not a hash
//
// Every other credential in the repo is either stored as sha256 (`auth_api_keys.key_hash`,
// which only needs to verify "is what you gave correct") or not stored at all. Neither fits
// this table: **renewal must send the refresh_token back to Feishu as-is**, so it must be
// recoverable. Storage goes through shared/infra/cryptobox (AES-256-GCM), with encryption
// and decryption kept in the Store layer (coredao's sealKey/openKey), exactly the same as
// the Ark credential set.
// See the file header of repository/schema/core_larkuser_pg.sql for details.
//
// # Whose identity: the workspace owner, same as the ama injection path
//
// **This is a choice with a cost, not a default.** Intuitively it should be "the person who
// @-mentioned the bot in the group", because a Feishu user identity **only sees the chats
// that person can see**. Using the owner's token to read the group where A asked could read
// content A cannot see (overreach), or fail to read anything because the owner is not in
// that group (broken feature).
//
// We still choose the owner, for three reasons:
//
//  1. **The injection interface does not know "who is speaking"**: SessionEnvs and Credential
//     only take arkSessionID (see shelltools/credential.go). Injecting per speaker would mean
//     changing the interface signature, adding an actor field to Run, and building a
//     "Feishu open_id → employee email" mapping table. But inbound events **contain no
//     email at all** (contact/v3/users was observed to return email:None; the app only has
//     contact:contact.base:readonly), so that mapping table cannot be built today.
//  2. **Consistent with the existing injection approach**: the ama path also uses the owner.
//     Several paths with several identities would make "whose credential was used this
//     time" something you can only know by reading code.
//  3. **The overreach surface is smaller than it looks**: the agent is deployed by the
//     workspace owner for everyone anyway, and groups the owner cannot read, it cannot read
//     either; it does not widen the owner's own visibility out of nowhere.
//
// ⚠️ **The cost must be stated clearly in the UI** (both Description and FixHint say it): the
// person who authorizes vouches for Feishu reads by every agent in the workspace. If this is
// ever changed to "per speaker", first remove the three prerequisites in point 1; do not
// only change this file.
//
// # How the four states are used: refresh failure must separate warning from error
//
// This provider is where the four-state model matters most, because its credential **breaks
// on its own**:
//
//	missing  the server/user has no usable Feishu app yet → connect a Feishu app first;
//	         clicking authorize will not help
//	ok       has a token that still works (about to expire is not a problem; it auto-renews)
//	warning  **the last renewal did not get through** (network blip, Feishu 5xx) → usually
//	         nothing to do, the next try will work
//	error    the refresh_token is invalid or expired → **must re-authorize**
//
// ⚠️ The line between warning and error follows the lesson that credential validity needs
// an "unknown" state: with two states, a single network blip is treated as "authorization
// has expired", so the user is asked again and again to rescan, and after each scan it
// "works again for a while", which is the hardest kind to self-diagnose.
// The rule sits on PollStatus.Terminal() in shared/infra/lark: only a terminal state is error.
//
// # ⭐⭐ How four things relate: who authorizes / stored under whom / state judged by whom / used for injection
//
// **These four once disagreed, and it was the most expensive failure in this file.** The
// conclusion table first, then why:
//
//	                     identity (actor)        app (app_id)                   entry point
//	who authorizes       **the person clicking**  pinned by workspaceID          AuthorizeURLForWorkspace
//	stored under whom    same, exactly            same, exactly                  PutLarkUserToken
//	state judged by whom same, exactly            same, exactly                  StateForWorkspace
//	used for injection   **workspace owner**      pinned by the run's workspace  UserAccessToken
//
// Read it **as two separate axes**; mixing them leads to wrong conclusions:
//
//	identity axis  the first three rows are "the operator", the fourth is "the workspace owner".
//	               **This is not a bug**; see the "Whose identity" section above: the injection
//	               interface does not know "who is speaking", so the runtime always uses the
//	               owner. So "a non-owner authorizes" is not a no-op (their own card will truly
//	               turn 'authorized'), but **for this workspace's agents to really work, the
//	               owner must also authorize once**. The table's primary key is
//	               `(actor, lark_app_id)`, so it naturally stores each of N people's credentials.
//	app axis       **all four rows must be exactly the same**: all pinned by workspace (falling
//	               back to the account-level default only without workspace context). A token
//	               is bound to an app; if any row uses a different rule, that row is answering
//	               a different question.
//
// ⚠️ **Historically, one row on the app axis was missed**: after start / poll / list apps
// were changed to go by workspace, **the state check stayed on the account-level default**.
// Three of four right, one wrong. The symptom was "authorization succeeds, the card always
// says not authorized": the user authorizes again and again, each time it really succeeds,
// each time the card says not authorized, while the whole chain returns HTTP 200 with clean
// logs. See StateForWorkspace for details.
//
// So the fixed routine when editing this file: **touch any one path, recount all four rows
// together.** The test is "does the app_id this row resolves match the other three rows",
// not "is this row correct on its own".
//
// # ⚠️ UI text must match this table
//
// The card's sentence "Feishu reads by every agent in the workspace use the **workspace
// owner's** authorization" is about the **injection axis** (row four), while the card's own
// state is about **you** (row three). The two sentences sit side by side with different
// subjects, and a non-owner would read it as "my authorization is useless". So the state
// area must say on the spot "this is your own authorization state" and "whose authorization
// actually applies in this workspace"; see ownerHintFor.
package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/shared/infra/lark"
)

// LarkUserIntegrationID is the stable identifier of this source.
//
// It is exported because **the injection path uses it to decide "did this agent check Feishu
// user authorization"** (core/sessionenv.go), and apimodel.IntegrationLarkUser must equal it
// exactly. If it were a literal scattered in several places, missing one on rename would
// not error; it would only show up as "checked but not injected".
const LarkUserIntegrationID = "lark-user"

// LarkAppResolver tells this provider "which Feishu app this person should authorize with".
//
// # Why an interface instead of taking feishu.Store directly
//
// **core does not import feishu** (header of core/core.go: Feishu apps belong to
// server/feishu; this package does not touch them). So the consumer interface is declared
// locally here, and cmd/amad plugs in the feishu-side implementation, the same dependency
// inversion as SkillLoader / ManagedAPIKeys.
type LarkAppResolver interface {
	// ResolveApp returns the Feishu app credentials this person should use.
	//
	// The returned appSecret is **plaintext**, used only briefly when swapping tokens, and
	// must never be logged. It returns an error when no usable app is found, which maps to
	// the missing state (connect a Feishu app first).
	//
	// ⚠️ **q.WorkspaceID decides whether this path is correct**: injection must fill it
	// (which workspace that run belongs to), otherwise it falls back to "guess one among all
	// of this person's workspaces". A token is bound to an app, and a wrong guess shows up
	// as 401 or unreadable groups, **neither pointing at "the wrong app was chosen"**.
	// Authorize / revoke have no workspace context, so leaving it empty is right. See
	// apimodel.LarkAppQuery.
	ResolveApp(ctx context.Context, q apimodel.LarkAppQuery) (appID, appSecret string, err error)

	// ListUserApps lists every Feishu app this person can pick, for the "switch app" picker.
	//
	// ⚠️ The returned struct has **no** app_secret field (apimodel.LarkUserApp), so the hard
	// rule "credentials never go into responses" holds at the type level, not by relying
	// on callers to remember.
	ListUserApps(ctx context.Context, actor string) ([]apimodel.LarkUserApp, error)

	// WorkspaceIDsFor returns the ids of the workspaces this person is in.
	//
	// # ⚠️ It is here for one thing only: checking "are you a member of the workspace the client named"
	//
	// The two authorization routes (authorize / authorization) now take an optional
	// `?workspaceId=`, and it **comes from the client**. The branch of ResolveApp that pins a
	// workspace (appInWorkspace/firstUsableApp in feishu/apps.go) **queries the database
	// directly without checking membership**. That is safe today because the only caller
	// passing WorkspaceID is the trusted injection path (core/sessionenv.go). Once the value
	// can be set by a request, not checking it would let someone use another person's
	// workspaceId to probe "has that workspace connected a Feishu app".
	//
	// ⚠️ **Keep the check in this package (the provider); do not push it down into
	// ResolveApp**: that function is on the injection hot path of **every container command**
	// (it deliberately uses the store instead of the Service to save round trips). Adding an
	// auth query there would tax every bash command.
	//
	// ⚠️ Guard cannot decide this: the workspace comes from a **query parameter**, and authz
	// patterns cannot express queries (same as `GET /api/workspace-by-key`; see
	// authz/matrix.go). So this is another place where "authorization is written in the
	// Service layer", and it must be pinned by tests.
	WorkspaceIDsFor(ctx context.Context, actor string) ([]string, error)
}

// LarkUserTokenStore is the storage this provider needs.
//
// It is narrowed from core.Store for the same reason as ManagedAPIKeys: this provider should
// only touch these four methods. Giving it the whole Store would make "what else does it
// change here" something you only know by reading code.
type LarkUserTokenStore interface {
	GetLarkUserToken(ctx context.Context, actor, larkAppID string) (persistmodel.LarkUserToken, error)
	PutLarkUserToken(ctx context.Context, tok persistmodel.LarkUserToken) error
	DeleteLarkUserToken(ctx context.Context, actor, larkAppID string) error
	ListLarkUserTokens(ctx context.Context, actor string) ([]persistmodel.LarkUserToken, error)

	// "Which Feishu app I use by default". **Account-level**; see the comments on that group of methods in core.Store.
	GetLarkUserAppPref(ctx context.Context, actor string) (string, error)
	PutLarkUserAppPref(ctx context.Context, actor, larkAppID string) error

	// Device-code intermediate state. Stored in the database, not memory; see persistmodel.LarkAuthSession.
	PutLarkAuthSession(ctx context.Context, sess persistmodel.LarkAuthSession) error
	GetLarkAuthSession(ctx context.Context, actor, larkAppID string) (persistmodel.LarkAuthSession, error)
	DeleteLarkAuthSession(ctx context.Context, actor, larkAppID string) error
}

// WorkspaceOwnerLookup answers "whose authorization will this workspace's agents actually use".
//
// # Why this is a separate dependency instead of being folded into LarkAppResolver
//
// LarkAppResolver answers the **app axis** (which cli_xxx to use); this answers the
// **identity axis** (whose token is used for injection). The two axes in the file header
// table are deliberately separate, and so are the interfaces. Merging them would let
// someone "changing app resolution" also change identity semantics by accident.
//
// It is implemented by *Service (which owns membership), and the rule must come from **the
// same source** as the injection path: addLarkUserEnvs in core/sessionenv.go takes
// firstOwnerEmail, so this must be that too. If the two places each wrote their own,
// "the UI says A applies but the runtime uses B" would appear with zero errors.
//
// When nil, that hint is not shown (state is exactly as before); it is not a fatal gap.
type WorkspaceOwnerLookup interface {
	// WorkspaceOwnerOf returns the email of this workspace's "first owner", the same rule as the injection path.
	//
	// When no owner is found it returns an empty string and a nil err: that is an abnormal
	// state, but it should not make the whole card error out.
	WorkspaceOwnerOf(ctx context.Context, workspaceID string) (string, error)
}

// LarkUserProvider is the "Feishu user authorization" source.
type LarkUserProvider struct {
	store LarkUserTokenStore
	apps  LarkAppResolver
	// owners answers "whose authorization this workspace uses for injection", used only to
	// explain the identity axis on the card. When nil the hint is not shown; see WorkspaceOwnerLookup.
	owners WorkspaceOwnerLookup
	// client is the client that calls Feishu. nil means not wired (state is always missing).
	client *lark.Client
	log    *slog.Logger
	// now can be injected so tests can pin the expiry check. When nil, time.Now is used.
	//
	// **Do not drop this hook**: the expiry check is this provider's core logic, and testing
	// "about to expire" with a real clock needs either sleep or "roughly this number"
	// assertions, and the latter would not turn red if broken.
	now func() time.Time
}

// NewLarkUserProvider builds a provider.
//
// Even if any dependency is nil it still returns a usable object (state shows missing),
// **never nil**: the card must appear in the list, otherwise users do not know it exists and
// will never configure it (same as the cmd/amad rule "a provider missing config must still
// be registered").
//
// owners may be nil (then the card does not show the "whose authorization applies in this
// workspace" hint). It **does not take part** in the configured() check: one missing hint
// should not turn the whole source into "not connected".
func NewLarkUserProvider(
	store LarkUserTokenStore, apps LarkAppResolver, client *lark.Client,
	owners WorkspaceOwnerLookup, logger *slog.Logger,
) *LarkUserProvider {
	if logger == nil {
		logger = slog.Default()
	}
	return &LarkUserProvider{
		store: store, apps: apps, client: client, owners: owners, log: logger,
	}
}

func (p *LarkUserProvider) ID() string   { return LarkUserIntegrationID }
func (p *LarkUserProvider) Name() string { return "飞书用户授权" }
func (p *LarkUserProvider) Description() string {
	// ⚠️ Say plainly "under your identity" and "this workspace's agents will all use it"; see
	// the cost in the "Whose identity" section of the file header. Without saying it, users
	// would think the authorization only affects their own chats.
	//
	// ⚠️⚠️ **The subject must be kept separate from the state column.** This sentence is about
	// the **injection axis** (the runtime uses the workspace owner's), while the card state
	// is about **your own** (the table's primary key is `(actor, app_id)`). This sentence used
	// to be "Feishu reads by every agent in the workspace use the workspace owner's
	// authorization", and "**this** authorization" glued the two subjects into one, so a
	// non-owner would conclude "my authorization is useless", while in fact they stored their
	// own and the card would truly turn green. Whose it is exactly is written on the spot by
	// ownerHintFor in Details and Summary (only there is the workspace known).
	return "让容器里的 agent 能用 lark-cli 以你的飞书身份读群聊与话题历史。" +
		"每个人各自授权、互不覆盖；而 agent 运行时统一用空间负责人的那份"
}

// configured reports whether this source is wired up.
func (p *LarkUserProvider) configured() bool {
	return p != nil && p.store != nil && p.apps != nil && p.client != nil
}

// nowTime gets the current time; tests can inject it.
func (p *LarkUserProvider) nowTime() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// State reports this person's Feishu user authorization status (**account-level**).
//
// This is the method on the IntegrationProvider interface, kept as a thin wrapper with an
// empty workspaceID, so every flow has a single implementation body, same as AuthorizeURL /
// PollAuthorization / ListApps. The entry with a workspace is StateForWorkspace, and
// **the agent page must use that one**.
//
// Convention: **never returns an error**; failures are encoded as states (see the comment on
// IntegrationProvider.State).
func (p *LarkUserProvider) State(ctx context.Context, actor string) IntegrationState {
	return p.StateForWorkspace(ctx, actor, "")
}

// StateForWorkspace reports the state, picking the app by workspaceID.
//
// # ⚠️⚠️ Why state must also be computed per workspace: this was the fourth "half-wired" place
//
// After start authorization / poll / list apps all took `?workspaceId=`, **the state check
// was left out**, so three of the four paths went by workspace and one by the account
// default. The result was a complete dead end, with every step returning 200:
//
//	card's "will authorize" line   resolved by workspace → workspace B's app (right)
//	user clicks "authorize"        resolved by workspace → token stored under
//	                               (actor, workspace B's app) (right, it is in the database)
//	the card's state               resolved by **account default** → workspace A's app →
//	                               no token found → "not authorized yet, click 'authorize'"
//
// So **authorization succeeded, but the card still says not authorized**. The user follows
// that FixHint and clicks again and again; each time authorization really succeeds and each
// time the card says not authorized. This contrast was observed in production (same account):
//
//	GET /api/integrations/lark-user                     → cli_example_app_a, not authorized
//	GET /api/integrations/lark-user/apps?workspaceId=…  → cli_example_app_b, authorized:true
//
// The two appIDs differ, and the latter is the one this workspace really uses: **the
// credential was sitting in the database all along**. Worse, the injection path
// (UserAccessToken) is also pinned by workspace, so **the agent actually worked**; only the
// UI was lying, and the user had no reason to suspect "the display is wrong".
func (p *LarkUserProvider) StateForWorkspace(
	ctx context.Context, actor, workspaceID string,
) IntegrationState {
	st := IntegrationState{
		ID:          p.ID(),
		Name:        p.Name(),
		Description: p.Description(),
		CheckedAt:   nowISO(),
		// ⭐ **The opposite of the AMA provider**: this source requires the user to click
		// approve in Feishu themselves; OAuth has no other way. So the "authorize" button is
		// its main path, not optional.
		CanAuthorize: true,
		// Injection type: LARKSUITE_CLI_USER_ACCESS_TOKEN / _APP_ID in the container come from it.
		//
		// ⚠️ It is true even when the state is missing, for the same reason as the AMA
		// provider: it should still appear in the agent's authorization checkbox area
		// (disabled + one hint), not disappear entirely.
		Injectable: true,
		// ⚠️ **Must be true**: this source uses device codes and has no callback. Only when
		// the frontend polls GET /api/integrations/lark-user/authorization do we swap the
		// token back and store it. If false, the user clicks approve in Feishu and this side
		// shows "not authorized" forever.
		PollsAuthorization: true,
	}

	if !p.configured() {
		st.Status = IntegrationMissing
		st.Summary = "服务端还没接入飞书，容器里的 lark-cli 不可用"
		st.FixHint = "请管理员先接入一个飞书应用：在空间的侧栏点「飞书」→「接入应用」"
		// No authorize button when missing: clicking would only get a 422, and the user has no way to know the server needs configuring.
		st.CanAuthorize = false
		return st
	}

	// ⚠️ **Must use the same entry as starting authorization** (resolveAppScoped), otherwise
	// it is the dead end described in this function's comment: authorization lands on the
	// workspace's app while the state checks the account default.
	appID, _, err := p.resolveAppScoped(ctx, actor, workspaceID)
	if err != nil {
		// Non-member (ErrNotFound) and "this workspace has no app connected" both end up
		// here, but what to tell the user differs: for the former nothing they click helps;
		// for the latter they need to connect one.
		//
		// ⚠️ But **neither is "not authorized yet"**: that sentence makes people click the
		// authorize button, and the button is bound to fail in both cases (422 / 404).
		if errors.Is(err, ErrNotFound) {
			st.Status = IntegrationError
			st.Summary = "看不到这个空间的飞书应用"
			st.FixHint = "你可能不是这个空间的成员，请找空间负责人确认"
			st.CanAuthorize = false
			return st
		}
		// Reaching here means "no app could be resolved". ⚠️ **The account-level and
		// per-workspace paths must give different answers**, because "can the user fix it
		// themselves" is opposite in the two cases:
		//
		//	account-level (authorization page)  they have no usable Feishu app at all → missing;
		//	                                    an admin must connect one first, and the authorize
		//	                                    button does nothing
		//	per-workspace (agent page)          **this workspace** has no app connected yet, and they
		//	                                    are likely the owner/admin of it and can connect one
		//	                                    with two clicks in the sidebar → something they can fix
		//
		// ⚠️⚠️ The per-workspace path **must keep CanAuthorize true**, otherwise the agent form
		// hides the whole card (the listed filter in `AgentRuntimeSections` only keeps
		// `status === "ok" || canAuthorize`). That is exactly the dead end from the lesson about
		// hiding a control by default: card not visible → no one knows the capability exists →
		// even less that an app needs to be connected.
		//
		// The UI line "this workspace has not connected a Feishu app → 'Feishu'" and **hiding
		// the authorize button** are derived by the frontend from its own app list
		// (`missingAppInWorkspace`), so returning true here does not actually render a button
		// that is bound to 422. Even if another caller rendered it, the raw text of that 422
		// itself says "this workspace has not connected a Feishu app; connect one under
		// 'Feishu' in the sidebar first": clear and actionable, not a meaningless failure.
		if strings.TrimSpace(workspaceID) != "" {
			st.Status = IntegrationError
			st.Summary = "这个空间还没接入飞书应用，无法授权"
			st.FixHint = "先在这个空间的侧栏点「飞书」→「接入应用」，然后回来授权"
			return st
		}
		// No usable Feishu app. This is missing, not error: an app must be connected first,
		// and "start authorization" cannot even be sent without one (it needs client_id).
		st.Status = IntegrationMissing
		st.Summary = "还没有可用的飞书应用，无法发起授权"
		// ⚠️ Point to **"Feishu" in the sidebar**, not "workspace settings": workspace
		// settings has no such item, and people following the old text would end up on the
		// "authorization management" page (this page) and conclude "I can't switch the Feishu
		// bot here". See the paragraph above noAppInScopeError.
		st.FixHint = "先接入一个飞书应用：在空间的侧栏点「飞书」→「接入应用」，然后回来授权"
		st.CanAuthorize = false
		return st
	}
	st.Details = append(st.Details, IntegrationDetail{Label: "飞书应用", Value: appID})
	// Identity axis: whose authorization this workspace's agents actually use. See the table in the file header.
	ownerHint, ownerIsMe := p.ownerHintFor(ctx, actor, workspaceID)
	if ownerHint != "" {
		st.Details = append(st.Details, IntegrationDetail{
			Label: "本空间生效的授权", Value: ownerHint})
	}

	tok, err := p.store.GetLarkUserToken(ctx, actor, appID)
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, persistmodel.ErrNotFound) {
			// Never authorized. **This is error, not missing**: the user can fix it with one
			// click, while missing means "clicking won't help, an admin must configure it".
			st.Status = IntegrationError
			st.Summary = "还没有授权，容器里的 lark-cli 取不到群聊与话题历史"
			st.FixHint = "点「发起授权」，在飞书里确认一次即可（之后由平台自动续期）"
			// ⚠️ A non-owner must also be told "your authorization will not make this workspace
			// work"; otherwise they authorize successfully, the card turns green, the agent
			// still cannot read groups, and nothing points at "it uses the owner's".
			if !ownerIsMe && ownerHint != "" {
				st.FixHint += "。⚠️ 但这个空间的 agent 用的是" + ownerHint +
					"，要让 agent 真的能读群聊，需要那个人也授权一次"
			}
			return st
		}
		// The database query failed. **This is not "not authorized"**; do not show it as
		// asking the user to authorize. Clicking won't fix it; the real cause is on our side.
		p.log.ErrorContext(ctx, "查飞书用户凭据失败",
			slog.String("actor", actor), slog.String("larkAppID", appID),
			slog.Any("err", err))
		st.Status = IntegrationError
		st.Summary = "读取授权状态失败"
		st.FixHint = "这是服务端的问题，请把这条反馈给维护者"
		st.CanAuthorize = false
		return st
	}

	now := p.nowTime()
	if tok.UserName != "" {
		st.Details = append(st.Details, IntegrationDetail{Label: "授权用户", Value: tok.UserName})
	}
	// ⚠️ **The "Feishu open_id" detail is deliberately no longer shown.**
	//
	// The old reason was "open_id is not a secret, and showing it lets you reconcile with
	// the Feishu side". The first half is still true (so this is not a security issue), but
	// "reconciling" is something **maintainers** do while debugging, and the "authorized
	// user" line above already says the only thing users can tell apart (who authorized).
	// The `ou_` string is neither readable nor actionable for users; the test "would seeing
	// it lead to a different decision?" says no.
	//
	// ⚠️ **Only "re-authorize by" is shown; "credential valid until" is no longer shown.**
	//
	// This is not about saving space; that line **is fake actionable information on this
	// card**: whether the access_token is about to expire or already expired, it is renewed
	// automatically at injection time (see the comment on the default branch below: "the
	// user does not notice, so it is still ok"). Showing a moment the user does not notice
	// and need not act on only makes people think "it's about to expire, should I do
	// something?", when they need to do nothing.
	//
	// The refresh one is the opposite: **when it expires, re-authorization is really
	// required**, and auto-renewal cannot save it (see the first case below). So it is the
	// only time on this card worth the user remembering.
	//
	// ⚠️⚠️ And it is rendered with `expiryDetail`, **no longer with
	// `tok.*.Format(time.RFC3339)`**. Two reasons; the second is a hard rule:
	//
	//	1. A bare ISO string (2026-09-11T03:41:26Z) makes the user compute "how long from
	//	   now", which is exactly the only thing they want to know. expiryDetail renders
	//	   "local time (N days left)"; the e2e case "validity shows 'how long left', not a bare
	//	   timestamp for the user to compute" tests exactly this shape.
	//	2. The repo rule says **do not use time.RFC3339**: it drops `.000`, so the same
	//	   concept appears in two forms in the database/API. Here it is only a display value,
	//	   but copying the pattern passes the trap on. expiryDetail also guards against the
	//	   "units mixed up" kind of bug (see plausibleExpiry).
	if d, ok := expiryDetail("需重新授权于", tok.RefreshExpiresAt); ok {
		st.Details = append(st.Details, d)
	}
	// Show the scope: the answer to "why can't it read group history" is often a missing item here.
	if tok.Scope != "" {
		st.Details = append(st.Details, IntegrationDetail{
			Label: "已授权限", Value: tok.Scope})
	}
	// With a credential it can be truly revoked (the row is deleted, the user must rescan),
	// unlike the AMA provider's fake revoke that "rebuilds the same key next time", so this
	// button does what it says.
	st.CanRevoke = true

	switch {
	case !tok.RefreshExpiresAt.After(now):
		// refresh has also expired: **re-authorization is required**; auto-renewal cannot save it.
		st.Status = IntegrationError
		st.Summary = "授权已过期，需要重新授权"
		st.FixHint = "点「发起授权」，在飞书里重新确认一次"

	case tok.LastRefreshError != "":
		// ⭐ The last renewal did not get through, but the refresh_token has not expired →
		// **warning, not error**. This means "usually nothing to do, it will fix itself next
		// time"; asking the user to act would be the wrong response.
		// See the file header and the lesson that credential validity needs an unknown state.
		st.Status = IntegrationWarning
		st.Summary = "上次自动续期没成功，平台会重试"
		st.FixHint = "通常不用管。如果长时间一直是这个状态，再点一次「发起授权」"
		st.Details = append(st.Details, IntegrationDetail{
			// ⚠️ LastRefreshError **contains no token** (guaranteed where it is written), so it can be shown.
			Label: "上次续期失败原因", Value: tok.LastRefreshError})

	default:
		// Valid, or only the access_token is about to expire / already expired. The latter
		// is renewed automatically at injection time and the user does not notice, so it is
		// still ok, not warning.
		st.Status = IntegrationOK
		st.Summary = "已授权，容器里的 lark-cli 可以读群聊与话题历史"
		// ⚠️ **A non-owner's "authorized" is a half-truth**: their own credential is indeed
		// fine, but this workspace's agents use the owner's. Without saying so, they would
		// take "the card is green" as "the agent can read groups now", and those two are not
		// the same here.
		//
		// It is still ok, not warning: **they have done everything they should**, and no
		// action is needed from them. Lowering it to warning would make people think they
		// are still one step short, and they would re-authorize again and again.
		if !ownerIsMe && ownerHint != "" {
			st.Summary = "你这份授权已生效；但这个空间的 agent 用的是" + ownerHint + "那份"
			st.FixHint = "如果 agent 仍读不到群聊，说明那个人还没授权——请找他授权一次"
		}
	}
	return st
}

// ownerHintFor explains "whose Feishu authorization this workspace's agents actually use".
//
// It returns (a short phrase for people, whether that person is the actor). When it cannot
// tell, it returns ("", true). **The second value being true is deliberate**: callers only
// append the hint when `!ownerIsMe`, and false would turn "owner not found" into "show
// everyone a warning that points nowhere".
//
// # Why this function exists: the subject of the text and of the implementation must match
//
// The card description says "Feishu reads by every agent in the workspace use the
// **workspace owner's** authorization", while the card state is computed for **the current
// logged-in user** (the table's primary key is `(actor, app_id)`). With the two subjects
// side by side and unexplained, a non-owner reaches one of two wrong conclusions: "my
// authorization is useless" (so they don't authorize), or "a green card means the agent
// works" (so they debug in entirely the wrong direction).
//
// ⚠️ **Never shown without workspace context** (the authorization management page path):
// that page is account-level and has no idea which workspace the user cares about;
// guessing an owner and naming them would only mislead.
func (p *LarkUserProvider) ownerHintFor(
	ctx context.Context, actor, workspaceID string,
) (hint string, ownerIsMe bool) {
	if p.owners == nil || strings.TrimSpace(workspaceID) == "" {
		return "", true
	}
	owner, err := p.owners.WorkspaceOwnerOf(ctx, workspaceID)
	if err != nil {
		// Not found means not shown. **Do not show it as an error**: this hint is a bonus,
		// and letting it turn the whole card into error would blow a display issue up into
		// "feature unavailable".
		p.log.WarnContext(ctx, "查空间 owner 失败（卡片少一条提示，不影响授权）",
			slog.String("workspaceID", workspaceID), slog.Any("err", err))
		return "", true
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return "", true
	}
	// Emails are case-insensitive (repo-wide rule: the lowercase email is the user's unique ID).
	if strings.EqualFold(owner, strings.TrimSpace(actor)) {
		return "你本人（空间负责人）的授权", true
	}
	return "空间负责人 " + owner + " 的授权", false
}

// AuthorizeURL starts a device-code authorization (**account-level**) and returns the URL
// for the user to open.
//
// This is the method on the IntegrationProvider interface, kept as a thin wrapper with an
// empty workspaceID so every flow has a single implementation body. The entry with a
// workspace is AuthorizeURLForWorkspace.
func (p *LarkUserProvider) AuthorizeURL(ctx context.Context, actor string) (string, error) {
	return p.AuthorizeURLForWorkspace(ctx, actor, "")
}

// AuthorizeURLForWorkspace starts a device-code authorization, picking the app by workspaceID.
//
// ⚠️ **It has side effects**: it requests a device_code from Feishu and records it (see
// pendingAuth), because polling needs it. This differs from other providers that "just
// build a URL", but the interface shape is the same, and callers need not know the
// difference.
//
// # A non-empty workspaceID pins that workspace, never falling back
//
// This is where the bug "authorization started from the agent page authorized the
// account-level default app instead" was fixed. An agent belongs to a workspace, and the
// group messages it reads are received by **that workspace's** Feishu app. A token is bound
// to an app, and authorizing the wrong one shows up at runtime as "no user authorization
// has been done for Feishu app cli_xxx yet", **a message that does not point at "you
// authorized the wrong one"**. So pick by workspace here, and fail clearly if none is found.
//
// When the workspace has no usable app, **falling back to the account-level default is not
// allowed**: that is exactly the kind of silent mismatch fixed before (pairing workspace A's
// token with workspace B's app_id, lark-cli returns 99991668).
//
// # Why device code instead of an authorization code callback
//
// An authorization code needs a redirect_uri, which must first be registered in the open
// platform's allowlist, **and this repo cannot change the app config** ("known limits" in
// shared/infra/lark/README.md). Feishu apps are self-registered by users, so that would
// require every user to configure it by hand on the open platform first, or the
// authorization page errors out. Device code has no such prerequisite.
func (p *LarkUserProvider) AuthorizeURLForWorkspace(
	ctx context.Context, actor, workspaceID string,
) (string, error) {
	if !p.configured() {
		return "", fmt.Errorf("%w: 服务端还没接入飞书", ErrPrecondition)
	}
	appID, appSecret, err := p.resolveAppScoped(ctx, actor, workspaceID)
	if err != nil {
		// ⚠️ A membership check failure (ErrNotFound) must be **passed up as-is**, not covered
		// by the "no usable Feishu app yet" 422 below; that would turn "you are not a member of
		// this workspace" into "go connect an app", pointing to a completely wrong action.
		if errors.Is(err, ErrNotFound) {
			return "", err
		}
		return "", noAppInScopeError(workspaceID)
	}

	ch, err := p.client.BeginUserAuth(ctx, appID, appSecret)
	if err != nil {
		return "", fmt.Errorf("发起飞书授权失败: %w", err)
	}
	// Record the device_code, used for polling when the frontend comes back asking "is
	// authorization done".
	//
	// ⚠️ **Stored in the database, not process memory**: production runs multiple instances,
	// and the polling request will likely land on another instance.
	// See persistmodel.LarkAuthSession and the comments on that table.
	now := p.nowTime()
	// Feishu's expires_in is occasionally 0 (unreadable when the field name differs); then
	// use a conservative default, otherwise this row would be stored as "expired", and the
	// user would be told the flow is void right after getting the link.
	ttl := time.Duration(ch.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = defaultAuthSessionTTL
	}
	interval := ch.Interval
	if interval <= 0 {
		interval = defaultAuthPollInterval
	}
	if err := p.store.PutLarkAuthSession(ctx, persistmodel.LarkAuthSession{
		Actor:        actor,
		LarkAppID:    appID,
		DeviceCode:   ch.DeviceCode,
		PollInterval: interval,
		CreatedAt:    now,
		ExpiresAt:    now.Add(ttl),
	}); err != nil {
		// If it cannot be stored, polling cannot get the device_code, and this authorization
		// is doomed. Error clearly instead of handing out a link the user clicks for nothing.
		return "", fmt.Errorf("记录飞书授权会话失败: %w", err)
	}
	return ch.VerificationURL, nil
}

// defaultAuthSessionTTL / defaultAuthPollInterval are fallbacks when Feishu does not give these two values.
//
// Feishu was observed to give expires_in=3600 / interval=5; the defaults here only apply
// when the fields cannot be read (e.g. if it renames them one day). **They must not be 0**:
// 0 would store this row as "expired", the user would be told the flow is void right after
// getting the authorization link, and the error would not point at the real cause.
const (
	defaultAuthSessionTTL   = 10 * time.Minute
	defaultAuthPollInterval = 5
)

// The definition of AuthorizationStatus moved to service/integration along with the
// registry; on the core side it is a **true alias** (see core/integrationalias.go).
//
// ⚠️ It must be an alias, not a copied struct with the same name: the provider in this file
// must be recognized by that registry **via a structural assertion**:
//
//	p.(interface{ PollAuthorization(ctx, actor string) (AuthorizationStatus, error) })
//
// With a copy, the two types are not the same, and the assertion **fails silently** (not a
// compile error). At runtime it returns ErrPollUnsupported → 400, and the frontend stops
// polling on its first tick. Device code has no callback, and **only the poll swaps the
// token back and stores it**, so a broken assertion means Feishu user authorization does
// not work at all, while the whole chain returns HTTP 200.

// noAppInScopeError says "no usable Feishu app" and points to the **real** place to connect one.
//
// ⚠️ These three messages once all said "workspace settings → Feishu", but **workspace
// settings has no Feishu item at all** (SettingsPage only has the tabs general / members /
// ark / danger). Feishu app management is a **top-level sidebar entry** (BUILD_NAV in
// web/src/app/AppShell.tsx, route /w/:key/feishu). Users following the old text would dig
// around workspace settings, fail to find it, and then look on the "authorization
// management" page, which is **account-level authorization** and has no bot management
// either. That is where "users say they can't switch the Feishu bot in global settings"
// came from: **the feature was there all along; we pointed them to the wrong place.**
//
// So the text only names the sidebar entry; never mention "workspace settings" again.
func noAppInScopeError(workspaceID string) error {
	if strings.TrimSpace(workspaceID) == "" {
		return fmt.Errorf(
			"%w: 还没有可用的飞书应用，先在侧栏的「飞书」里接入一个", ErrPrecondition)
	}
	// With a workspace, say clearly "this workspace has none": the account-level default app
	// may be fine, and just saying "no usable app" would make users think nothing was ever set up.
	return fmt.Errorf(
		"%w: 这个空间还没接入飞书应用，先在侧栏的「飞书」里接入一个（每个空间各自接入）",
		ErrPrecondition)
}

// PollAuthorization checks once "has the user clicked approve" (**account-level**), and stores
// the credential on success.
//
// Same as AuthorizeURL: this is the interface method, kept as a thin wrapper with an empty
// workspaceID. The entry with a workspace is PollAuthorizationForWorkspace.
//
// # Why it is needed: device code has no callback
//
// With the authorization code flow, Feishu calls our callback URL itself, but with device
// code we have to ask. So after the frontend clicks "authorize", it polls this endpoint at
// PollInterval until Done or an error.
//
// **Returning Pending is not an error**; see the PollStatus convention in shared/infra/lark:
// mixing "still waiting" into error would make the frontend stop the first time, and the
// user could never finish authorizing.
func (p *LarkUserProvider) PollAuthorization(
	ctx context.Context, actor string,
) (AuthorizationStatus, error) {
	return p.PollAuthorizationForWorkspace(ctx, actor, "")
}

// PollAuthorizationForWorkspace polls the authorization result, picking the app by workspaceID.
//
// ⚠️⚠️ **It must use the same rule as AuthorizeURLForWorkspace** (both go through
// resolveAppScoped), otherwise the whole flow dies: starting records the device_code under
// `(actor, workspace app B)`, while polling resolves app A from the account default →
// GetLarkAuthSession finds nothing → returns "this authorization flow is no longer valid".
// That response has Pending false, the frontend treats it as **terminal** and stops polling
// on the first tick, so the user sees "no longer valid" within 3 seconds of clicking
// authorize, **while the start itself succeeded**.
// See the comment on resolveAppScoped; this is the most typical shape of "half-wired".
func (p *LarkUserProvider) PollAuthorizationForWorkspace(
	ctx context.Context, actor, workspaceID string,
) (AuthorizationStatus, error) {
	if !p.configured() {
		return AuthorizationStatus{}, fmt.Errorf("%w: 服务端还没接入飞书", ErrPrecondition)
	}
	appID, appSecret, err := p.resolveAppScoped(ctx, actor, workspaceID)
	if err != nil {
		// Same as AuthorizeURLForWorkspace: pass the membership check's 404 up as-is; do not cover it with 422.
		if errors.Is(err, ErrNotFound) {
			return AuthorizationStatus{}, err
		}
		return AuthorizationStatus{}, noAppInScopeError(workspaceID)
	}
	sess, err := p.store.GetLarkAuthSession(ctx, actor, appID)
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, persistmodel.ErrNotFound) {
			// No pending authorization flow, or it has expired (Store treats expired as missing).
			// Say "start again" clearly; that is the only thing the user can do.
			return AuthorizationStatus{
				Message: "这次授权流程已失效，请重新点「发起授权」",
			}, nil
		}
		return AuthorizationStatus{}, fmt.Errorf("查飞书授权会话失败: %w", err)
	}

	tok, status, err := p.client.PollUserAuth(ctx, appID, appSecret, sess.DeviceCode)
	switch {
	case status == lark.PollPending || status == lark.PollSlowDown:
		// Still waiting for the user to click. On slow_down, double the interval, as the protocol intends.
		interval := sess.PollInterval
		if status == lark.PollSlowDown {
			interval *= 2
		}
		return AuthorizationStatus{
			Pending: true, PollInterval: interval,
			Message: "还在等你在飞书里确认授权",
		}, nil

	case err != nil, tok == nil:
		// Terminal failure (denied, expired, app deleted) or a transport failure.
		// **Delete this flow**: if kept, the frontend would keep polling a device_code that is bound to fail.
		if delErr := p.store.DeleteLarkAuthSession(ctx, actor, appID); delErr != nil {
			p.log.WarnContext(ctx, "清理失败的飞书授权会话时出错", slog.Any("err", delErr))
		}
		if err != nil {
			return AuthorizationStatus{}, fmt.Errorf("飞书授权未完成: %w", err)
		}
		return AuthorizationStatus{}, fmt.Errorf("%w: 飞书授权未完成", ErrPrecondition)
	}

	// Got it. Convert to absolute times and store.
	//
	// ⚠️ **Convert using "the moment just now"**: expires_in is relative, and every delay
	// shifts it further.
	now := p.nowTime()

	// Also look up "whose token is this".
	//
	// ⚠️ **The token swap response has no name or open_id** (authen/v2/oauth/token was
	// observed to return only access/refresh/expires/scope), so without this extra call
	// those two columns stay empty: the "authorized user" line on the card would be blank,
	// and operators could not match this credential to a person on the Feishu side.
	//
	// **Failure is not fatal**: the authorization itself already succeeded; a missing name is
	// just less display info. Treating it as failure would make the user scan for nothing,
	// which is worse.
	if info, infoErr := p.client.FetchUserInfo(ctx, tok.AccessToken); infoErr != nil {
		p.log.WarnContext(ctx, "查飞书用户信息失败（授权仍然成功，只是卡片上没有名字）",
			slog.String("actor", actor), slog.Any("err", infoErr))
	} else if info != nil {
		tok.Name = info.Name
		tok.OpenID = info.OpenID
	}
	rec := persistmodel.LarkUserToken{
		Actor:            actor,
		LarkAppID:        appID,
		AccessToken:      tok.AccessToken,
		RefreshToken:     tok.RefreshToken,
		ExpiresAt:        lark.ExpiryFrom(now, tok.ExpiresIn),
		RefreshExpiresAt: lark.ExpiryFrom(now, tok.RefreshTokenExpiresIn),
		Scope:            tok.Scope,
		OpenID:           tok.OpenID,
		UserName:         tok.Name,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := p.store.PutLarkUserToken(ctx, rec); err != nil {
		return AuthorizationStatus{}, fmt.Errorf("保存飞书凭据失败: %w", err)
	}
	// Authorization done; the intermediate state is no longer needed. A failed delete only leaves a junk row that expires in minutes; the result is unaffected.
	if delErr := p.store.DeleteLarkAuthSession(ctx, actor, appID); delErr != nil {
		p.log.WarnContext(ctx, "清理已完成的飞书授权会话时出错", slog.Any("err", delErr))
	}
	p.log.InfoContext(ctx, "飞书用户授权完成",
		slog.String("actor", actor), slog.String("larkAppID", appID),
		// ⚠️ Log only scope and open_id, **never the token**.
		slog.String("scope", tok.Scope), slog.String("openID", tok.OpenID))

	msg := "授权成功，容器里的 lark-cli 现在可以读群聊与话题历史了"
	// ⚠️ Authorization success ≠ can read group history: the user or an admin may have
	// removed that item. Point it out right here; otherwise the agent gets rejected on its
	// first read, and that rejection points at permissions, not at "an item was left out
	// during authorization".
	if !strings.Contains(tok.Scope, lark.ScopeUserReadGroupMessage) {
		msg = "授权成功，但没拿到「读群消息」权限，agent 仍然读不到群历史——" +
			"请重新授权并在飞书的授权页上勾全"
	}
	return AuthorizationStatus{Done: true, Message: msg}, nil
}

// Revoke removes the authorization: it deletes that row in the database.
//
// **This is a real revoke**: afterwards the user must go through OAuth again, unlike the AMA
// provider's fake revoke that "rebuilds the same key next time after deletion" (see the
// lesson in integration_ama.go).
//
// ⚠️ **It only deletes our side.** The entry in Feishu's "authorized apps" list has to be
// revoked by the user in Feishu; we have no API for that. So the error message must say so,
// otherwise users would think the Feishu side was cleared too.
func (p *LarkUserProvider) Revoke(ctx context.Context, actor string) error {
	if !p.configured() {
		return fmt.Errorf("%w: 服务端还没接入飞书", ErrPrecondition)
	}
	appID, _, err := p.resolveApp(ctx, actor)
	if err != nil {
		return fmt.Errorf("%w: 还没有可用的飞书应用", ErrPrecondition)
	}
	// Deletion is idempotent (a Store convention), so "was never authorized" is not an error.
	if err := p.store.DeleteLarkUserToken(ctx, actor, appID); err != nil {
		return fmt.Errorf("删除飞书授权失败: %w", err)
	}
	// Also clear any leftover pending authorization flow (the user started one and came to revoke before finishing).
	if err := p.store.DeleteLarkAuthSession(ctx, actor, appID); err != nil {
		p.log.WarnContext(ctx, "清理飞书授权会话时出错", slog.Any("err", err))
	}
	p.log.InfoContext(ctx, "已解除飞书用户授权",
		slog.String("actor", actor), slog.String("larkAppID", appID))
	return nil
}

// ---------------------------------------------------------------------------
// Which Feishu app to use: list them, pick one
// ---------------------------------------------------------------------------

// resolveApp gets this person's **account-level default** Feishu app (app_id + plaintext secret).
//
// ⚠️ **This is the rule when there is no workspace context**: card display, starting
// authorization, and revoking authorization. Injection **does not go through here**: that
// path is pinned by the run's workspace (UserAccessTokenForWorkspace), because a token is
// bound to an app, and a wrong pick shows up as 401 or unreadable groups, neither pointing
// at "the wrong app was chosen".
//
// The app the user explicitly picked comes first (lark_user_app_prefs); if none was picked,
// the first usable one in default order is taken. A failed preference read is always
// treated as "not picked": that only falls back to the default app, which is much better
// than making the whole card error out.
func (p *LarkUserProvider) resolveApp(
	ctx context.Context, actor string,
) (appID, appSecret string, err error) {
	return p.apps.ResolveApp(ctx, apimodel.LarkAppQuery{
		Actor:       actor,
		PreferAppID: p.preferredAppID(ctx, actor),
	})
}

// preferredAppID reads the app this person picked; returns an empty string if none was
// picked or the read failed.
//
// **Failures are not reported up**: this is purely a preference, and not reading it only
// means falling back to the default order. Letting it turn the whole card into error would
// blow an unimportant problem up into "feature unavailable".
func (p *LarkUserProvider) preferredAppID(ctx context.Context, actor string) string {
	appID, err := p.store.GetLarkUserAppPref(ctx, actor)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(appID)
}

// resolveAppScoped is the shared app-picking entry for the two paths "start authorization /
// poll authorization".
//
// Empty workspaceID → exactly equivalent to resolveApp (account-level default; the
// authorization management page path is unchanged).
// Non-empty → **pinned to that workspace**, after first checking "are you a member of this
// workspace".
//
// # Why these two paths must use the same function
//
// Starting authorization records the device code under `(actor, app_id)`
// (PutLarkAuthSession), and polling needs **the same app_id** to find that record again. If
// each side resolved the app on its own with different rules: start pins workspace app B,
// poll takes app A from the account default → session not found → returns "this
// authorization flow is no longer valid, please click 'authorize' again". And Pending is
// false, so the frontend treats it as **terminal** and stops polling on the first tick. The
// user sees "no longer valid" within 3 seconds of clicking authorize, **while the start
// itself succeeded and the Feishu authorization page is open in another tab**. It
// reproduced the same way every time. Worse: if they had authorized the account-default
// app before, the card still says "ready", with no sign of anything wrong.
//
// So this function is **the only** resolution entry; do not write the lookup again at each
// call site.
//
// # ⚠️ The check order is security-critical
//
// Check membership first; **if not a member, return immediately and never carry this id
// into ResolveApp**. The reason is the same as using ErrNotFound (404) instead of
// ErrAccessDenied (403): do not offer an oracle. 403 would let a non-member tell apart
// "this workspace does not exist" from "it exists but has no Feishu app connected yet",
// while 404 makes both look the same.
func (p *LarkUserProvider) resolveAppScoped(
	ctx context.Context, actor, workspaceID string,
) (appID, appSecret string, err error) {
	wsID := strings.TrimSpace(workspaceID)
	if wsID == "" {
		return p.resolveApp(ctx, actor)
	}
	if err := p.ensureWorkspaceMember(ctx, actor, wsID); err != nil {
		return "", "", err
	}
	// The preference is still passed, but it is **soft**: ignored if not in this workspace
	// (see apimodel.LarkAppQuery). Without it, when a workspace has several apps, the one
	// the user picked on the authorization management page would be ignored.
	return p.apps.ResolveApp(ctx, apimodel.LarkAppQuery{
		Actor:       actor,
		WorkspaceID: wsID,
		PreferAppID: p.preferredAppID(ctx, actor),
	})
}

// ensureWorkspaceMember confirms actor is a member of workspaceID, returning ErrNotFound if not.
//
// If the lookup fails (database blip) it always rejects: letting it through would hand an
// unchecked workspaceID downstream, and on this path "rather fail" is right; the user just
// retries once.
func (p *LarkUserProvider) ensureWorkspaceMember(ctx context.Context, actor, workspaceID string) error {
	ids, err := p.apps.WorkspaceIDsFor(ctx, actor)
	if err != nil {
		return fmt.Errorf("查这个人所在的空间失败: %w", err)
	}
	if slices.Contains(ids, workspaceID) {
		return nil
	}
	// ⚠️ The message must **not** reveal whether this workspace exists or has a Feishu app. See the function comment.
	return fmt.Errorf("%w: 空间不存在，或你不是它的成员", ErrNotFound)
}

// ListApps lists the Feishu apps this person can pick, marking "authorized or not" and
// "which one is current".
//
// # ⚠️ Authorization state is computed per app, not a single global boolean
//
// The table's primary key is `(actor, lark_app_id)`, which **naturally supports one person
// with several apps and several tokens**. So "authorized or not" must be checked per app.
// With one global boolean, after the user switches to an app they never authorized, the
// card still shows "authorized", and the agent fails as soon as it calls lark-cli.
//
// # Credentials never go into responses
//
// The returned apimodel.LarkUserApp has no token field at all (the type does not define
// one), so this hard rule holds at the type level. This code also **does not read token
// values**; it only checks "does this row exist".
func (p *LarkUserProvider) ListApps(
	ctx context.Context, actor string,
) ([]apimodel.LarkUserApp, error) {
	return p.ListAppsForWorkspace(ctx, actor, "")
}

// ListAppsForWorkspace is the same as ListApps, but `Current` is computed by workspaceID.
//
// # Why the per-workspace version is needed
//
// The agent form must show **which app is about to be authorized** next to "start
// authorization", otherwise users cannot confirm whether they authorized the wrong one
// (exactly the bug fixed this time). And the answer to "which one" depends on the
// workspace: the account-level default (Current's original meaning) is precisely the
// wrong value.
//
// ⚠️ **Do not match by workspaceName in the frontend**: apimodel.LarkUserApp only has the
// workspace **name**, not the id, and matching by name silently goes wrong with workspaces
// of the same name. Here the backend answers, using the same resolveAppScoped as the start
// authorization path: **one rule, one implementation**, so "the one the UI says will be
// authorized" and "the one that really gets authorized" cannot drift apart.
func (p *LarkUserProvider) ListAppsForWorkspace(
	ctx context.Context, actor, workspaceID string,
) ([]apimodel.LarkUserApp, error) {
	if !p.configured() {
		return nil, fmt.Errorf("%w: 服务端还没接入飞书", ErrPrecondition)
	}
	// When a workspace is named, check membership first, for the same reason as
	// resolveAppScoped: this route is also ActionNone, and not checking would let someone
	// use another person's workspaceId to probe "which Feishu apps that workspace has".
	if ws := strings.TrimSpace(workspaceID); ws != "" {
		if err := p.ensureWorkspaceMember(ctx, actor, ws); err != nil {
			return nil, err
		}
	}
	apps, err := p.apps.ListUserApps(ctx, actor)
	if err != nil {
		return nil, fmt.Errorf("列飞书应用失败: %w", err)
	}

	// List all apps this person has authorized in one go instead of Get per app: the latter
	// would hit the database N times when they have N apps, and this path runs every time
	// the authorization management page opens.
	authorized := map[string]bool{}
	if toks, listErr := p.store.ListLarkUserTokens(ctx, actor); listErr == nil {
		for _, tok := range toks {
			authorized[tok.LarkAppID] = true
		}
	} else {
		// If the lookup fails, treat it as "none authorized". **No error**: the app list itself
		// is right, and one missing status column is far better than the whole picker failing
		// to open (then the user could not even switch apps).
		p.log.WarnContext(ctx, "列飞书用户凭据失败（应用列表照常返回，只是标不出授权状态）",
			slog.String("actor", actor), slog.Any("err", listErr))
	}

	// "Which one is current" must **match exactly** what resolution concludes, so ask it
	// directly instead of reimplementing the priority rules here. Two implementations are
	// bound to drift, and the drift shows up as "the one checked in the UI" not being "the
	// one actually used".
	//
	// ⚠️ With a workspace it goes through resolveAppScoped (the same path as starting
	// authorization), so Current means "which one would be authorized if you start
	// authorization **in this workspace**". When the workspace has no app it is an empty
	// string; then the frontend should show "this workspace has not connected a Feishu app",
	// not fall back to the account default.
	current, _, curErr := p.resolveAppScoped(ctx, actor, workspaceID)
	if curErr != nil {
		current = ""
	}
	for i := range apps {
		apps[i].Authorized = authorized[apps[i].AppID]
		apps[i].Current = apps[i].AppID == current
	}
	return apps, nil
}

// SelectApp switches this person's default Feishu app to appID.
//
// # Only accept apps "they can actually see"
//
// Without the check, any logged-in user could set the preference to any cli_xxx. That
// leaks nothing by itself (the preference only affects them), but it would leave them stuck
// in a state where no app can ever be resolved, and the error would say "no usable Feishu
// app", not pointing at "you picked one that doesn't exist".
//
// ⚠️ **Switching apps is not switching authorization.** The new app may not be authorized
// yet; then the card shows "not authorized" + "start authorization". That is correct, and
// one of the reasons this feature exists (a token is bound to an app, so after switching
// apps you must authorize the new app once more).
func (p *LarkUserProvider) SelectApp(ctx context.Context, actor, appID string) error {
	if !p.configured() {
		return fmt.Errorf("%w: 服务端还没接入飞书", ErrPrecondition)
	}
	want := strings.TrimSpace(appID)
	if want == "" {
		return fmt.Errorf("%w: 要选哪个飞书应用", ErrInvalidArgument)
	}
	apps, err := p.apps.ListUserApps(ctx, actor)
	if err != nil {
		return fmt.Errorf("列飞书应用失败: %w", err)
	}
	for _, app := range apps {
		if app.AppID == want {
			if err := p.store.PutLarkUserAppPref(ctx, actor, want); err != nil {
				return fmt.Errorf("保存飞书应用选择失败: %w", err)
			}
			p.log.InfoContext(ctx, "切换飞书用户授权使用的应用",
				slog.String("actor", actor), slog.String("larkAppID", want))
			return nil
		}
	}
	// Return ErrNotFound (→ 404) rather than 400: this means "this app is not on the list you
	// can pick from", which is not the same as a malformed parameter.
	return fmt.Errorf("%w: 飞书应用 %s 不在你能选的名单里", ErrNotFound, want)
}

// ---------------------------------------------------------------------------
// Get a usable token (for the injection path)
// ---------------------------------------------------------------------------

// UserAccessToken gets this person's usable Feishu access_token for a **given workspace**,
// renewing it automatically when needed.
//
// It returns (token, appID, error). **Both are needed**: lark-cli requires
// LARKSUITE_CLI_USER_ACCESS_TOKEN and LARKSUITE_CLI_APP_ID to come as a pair, and errors
// clearly if one is missing (observed: "…is set but LARKSUITE_CLI_APP_ID is missing").
//
// # ⚠️⚠️ workspaceID is the most important parameter of this function, not optional context
//
// It decides which Feishu app is chosen, and **a token is bound to an app**: pairing the
// token of workspace A's app with the app_id of workspace B's app makes lark-cli return
// 99991668 "token invalid"; even if it happens to be valid, the two apps have different
// visibility (that group is unreadable). **Neither failure points at "the wrong app was
// chosen"**: one looks like an expired token, the other like missing permissions, and
// debugging heads in a completely wrong direction.
//
// This used to take only actor, so the downstream (feishu.ResolveApp) guessed one among
// **all** of this person's workspaces. The injection point clearly had the workspaceID (an
// argument of addLarkUserEnvs in core/sessionenv.go), but dropped it. One user was observed
// with three apps in three workspaces, so this is not a theoretical problem.
//
// An empty workspaceID falls back to the account-level default. That is for callers with
// no workspace context (probes, manual debugging); **the injection path must pass it**.
//
// # "This workspace's app is not authorized yet" must error clearly, never fall back
//
// Falling back to another workspace's token is the most tempting and the most wrong thing:
// it turns a problem the user can fix with one click (go authorize) into a silent mismatch.
// So here it would rather fail, and the error message says clearly "authorize this app once".
//
// # Renewal happens on this path, not in a separate background task
//
// Injection runs on **every command execution** (per-exec Envs, see
// shelltools/credential.go), so "check whether it needs renewal right before use" is
// naturally the most timely moment, with no scheduled task and no distributed lock. This is
// also why injection must use ExecSpec.Envs instead of SessionSpec.Envs: the latter cannot
// change after creation, and this token will expire while the container is alive.
//
// ⚠️ **Renew refreshSkew early**; do not wait until it actually expires: after injection the
// agent still runs a series of commands with this token, and sitting on the boundary would
// make "valid at injection, expired when used" happen now and then. That failure shows up
// as 401, not pointing at a time boundary.
func (p *LarkUserProvider) UserAccessToken(
	ctx context.Context, actor, workspaceID string,
) (token, appID string, err error) {
	if !p.configured() {
		return "", "", fmt.Errorf("%w: 服务端还没接入飞书", ErrPrecondition)
	}
	// ⚠️ **Pick by workspace, no fallback.** The preference is still passed, but it is soft:
	// ignored if not in this workspace (see apimodel.LarkAppQuery).
	appID, appSecret, err := p.apps.ResolveApp(ctx, apimodel.LarkAppQuery{
		Actor:       actor,
		WorkspaceID: workspaceID,
		PreferAppID: p.preferredAppID(ctx, actor),
	})
	if err != nil {
		return "", "", fmt.Errorf("%w: 空间 %s 里还没有可用的飞书应用", ErrPrecondition, workspaceID)
	}
	tok, err := p.store.GetLarkUserToken(ctx, actor, appID)
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, persistmodel.ErrNotFound) {
			// **This app** was never authorized.
			//
			// ⚠️ The error must include the app_id: this person may already have authorized
			// **another** app, and just saying "Feishu user authorization not done yet" would
			// leave them puzzled in front of a card that says "authorized". That is exactly
			// where cross-workspace mismatches fool people most easily.
			return "", "", fmt.Errorf(
				"%w: %s 还没对飞书应用 %s 做过用户授权（去「授权管理」里选中它再授权一次）",
				ErrPrecondition, actor, appID)
		}
		return "", "", fmt.Errorf("取飞书用户凭据失败: %w", err)
	}

	now := p.nowTime()
	if tok.ExpiresAt.After(now.Add(refreshSkew)) {
		// Still good enough; give it directly.
		return tok.AccessToken, appID, nil
	}

	// Time to renew. If refresh itself has expired too, nothing can be done; tell the user clearly to re-authorize.
	if !tok.RefreshExpiresAt.After(now) {
		return "", "", fmt.Errorf("%w: %s 的飞书授权已过期，需要重新授权",
			ErrPrecondition, actor)
	}

	fresh, status, refreshErr := p.client.RefreshUserToken(ctx, appID, appSecret, tok.RefreshToken)
	if refreshErr != nil {
		// ⭐ This is where the warning / error line is drawn.
		//
		// Terminal (invalid_grant etc.) = the authorization is really gone → clear that row so
		// the user re-authorizes.
		// Non-terminal (network, 5xx) = this attempt did not get through → **keep the
		// credential**, record only a failure reason; the card shows warning, and the next
		// injection retries.
		//
		// ⚠️ Deleting on non-terminal errors too is the worst form from the lesson that
		// credential validity needs an unknown state: one blip destroys a good credential and
		// forces the user to re-authorize.
		if status.Terminal() {
			p.log.WarnContext(ctx, "飞书授权已失效，清掉本地凭据",
				slog.String("actor", actor), slog.String("larkAppID", appID),
				slog.String("status", string(status)))
			if delErr := p.store.DeleteLarkUserToken(ctx, actor, appID); delErr != nil {
				p.log.ErrorContext(ctx, "清理失效的飞书凭据失败", slog.Any("err", delErr))
			}
			return "", "", fmt.Errorf("%w: %s 的飞书授权已失效，需要重新授权",
				ErrPrecondition, actor)
		}
		// Non-terminal: record the reason for the card's warning. **Do not write the full err
		// text**: it may carry the upstream response body, which may contain unexpected
		// sensitive content. Keep only a short description.
		p.noteRefreshFailure(ctx, tok, "续期请求没打通，稍后会自动重试")
		return "", "", fmt.Errorf("续期飞书凭据失败（稍后自动重试）: %w", refreshErr)
	}
	if fresh == nil {
		p.noteRefreshFailure(ctx, tok, "续期返回了空凭据")
		return "", "", fmt.Errorf("%w: 续期飞书凭据返回空", ErrPrecondition)
	}

	// Renewal succeeded; write it back.
	//
	// ⚠️ **The refresh_token must be written back too**: Feishu rotates it. Storing only the
	// access token would make the next renewal use the old one, which may already be
	// invalid; the symptom is "after renewing for a while, suddenly everything needs
	// re-authorization".
	updated := tok
	updated.AccessToken = fresh.AccessToken
	if strings.TrimSpace(fresh.RefreshToken) != "" {
		updated.RefreshToken = fresh.RefreshToken
		updated.RefreshExpiresAt = lark.ExpiryFrom(now, fresh.RefreshTokenExpiresIn)
	}
	updated.ExpiresAt = lark.ExpiryFrom(now, fresh.ExpiresIn)
	if strings.TrimSpace(fresh.Scope) != "" {
		updated.Scope = fresh.Scope
	}
	updated.UpdatedAt = now
	// After a successful renewal, clear the last failure record, otherwise the card keeps showing warning.
	updated.LastRefreshError = ""
	if err := p.store.PutLarkUserToken(ctx, updated); err != nil {
		// The write failed but the token is good: **use it first**; do not let one failed
		// write take down this round of chat. The only cost is renewing once more next time.
		p.log.ErrorContext(ctx, "写回续期后的飞书凭据失败（本轮仍使用新 token）",
			slog.String("actor", actor), slog.Any("err", err))
	}
	return updated.AccessToken, appID, nil
}

// refreshSkew is how early to renew.
//
// 30 minutes: the access_token was observed to be valid for 2 hours, and within one chat
// round the agent may run tool calls for several minutes in a row. Leaving enough margin
// makes the occasional "valid at injection, expired when used" go away; that failure shows
// up as 401 and would not point at a time boundary when debugging.
const refreshSkew = 30 * time.Minute

// noteRefreshFailure records a renewal failure reason for the card's warning.
//
// ⚠️ **Write only a fixed short description, never the err text**: upstream errors may
// carry a response body, and this column goes into HTTP responses through IntegrationState.
func (p *LarkUserProvider) noteRefreshFailure(
	ctx context.Context, tok persistmodel.LarkUserToken, reason string,
) {
	tok.LastRefreshError = reason
	tok.UpdatedAt = p.nowTime()
	if err := p.store.PutLarkUserToken(ctx, tok); err != nil {
		p.log.ErrorContext(ctx, "记录续期失败原因时写库失败", slog.Any("err", err))
	}
}

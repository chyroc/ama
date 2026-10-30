// This file is the **framework** of integration management: a provider registry + a four-state
// status model.
//
// Originally there was only one hard-coded integration source here (a few dedicated routes + a
// dedicated service). Adding a second source would mean changing routes, the authz matrix, and the
// frontend's card list; all three must be right for it to work, and missing any one shows up
// differently, never as an error. So it became a registry: **adding a provider means registering it
// once here; routes, permissions and frontend cards need no change.**
//
// # Four states instead of two
//
// The states are ok / warning / error / missing, not "authorized / unauthorized". Two states cannot
// express these real situations, which are handled completely differently:
//
//	missing  the server has not configured it → an **admin** must configure it; clicking a button
//	         does nothing for the user
//	warning  works but needs attention (about to expire, the platform renews automatically) →
//	         usually nothing to do
//	error    does not work at all (credential revoked, platform unreachable) → look at FixHint
//	ok       fine
//
// The worst part of two states is showing missing as "unauthorized": the user clicks the authorize
// button again and again on "unauthorized", while the problem is in the server config, and a
// hundred clicks would not help.
//
// # Every state must say "how to fix it"
//
// Summary says what is going on, FixHint says what to do, FixURL says where to do it. A state that
// only says "broken" pushes all the debugging onto the user.
//
// # ⚠️ Credentials never go into State
//
// State goes into the HTTP response and the frontend unchanged. So **no token / secret /
// access_token may appear in any of its fields**, including the free-text Summary and FixHint.
// Credential-related information can only be shown as a masked value (see maskCredential) or
// secret-free metadata like "valid until".
//
// # Status is always queried live, never stored
//
// Reason: authorization is granted by the user **on the platform side**, and the platform is the
// only authority. Storing our own "authorized" copy would disagree with the platform, and the
// disagreement shows up as "the page says authorized, but tool calls still fail", the hardest kind
// to diagnose yourself. With multiple instances it gets worse: instance A stored it and B did not,
// so two refreshes show two states. So there is no Store here; the cost is one platform call per
// status query.
package integration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/shared/infra/secret"
)

// IntegrationStatus is the current status of an integration source.
//
// The four values are **handled differently**, which is exactly why two states are not used. The
// frontend picks the color and whether to offer the "authorize" button from it, so values cannot be
// added casually: an added value would fall into the frontend's default branch.
type IntegrationStatus string

const (
	// IntegrationOK means authorized and usable.
	IntegrationOK IntegrationStatus = "ok"
	// IntegrationWarning means usable, but with something to know (about to expire, the platform
	// renews automatically, degraded but usable).
	//
	// The line between it and error is "does the user need to act now": warning no, error yes.
	IntegrationWarning IntegrationStatus = "warning"
	// IntegrationError means unusable, and **a user or admin must act** to recover it (credential
	// invalid, platform unreachable).
	IntegrationError IntegrationStatus = "error"
	// IntegrationMissing means the server has not configured this integration source.
	//
	// ⚠️ Not the same as "the user has not authorized". missing needs an admin to configure it, and
	// clicking the authorize button does nothing for the user, so this frontend branch **offers no
	// "authorize" button**, only an entry to the console.
	IntegrationMissing IntegrationStatus = "missing"
)

// IntegrationDetail is a name/value pair; the frontend renders them in order as two columns.
//
// ⚠️ Value is shown to the user as is; **no plain-text credentials**. Credentials must be masked with
// maskCredential.
type IntegrationDetail struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// IntegrationState is all the information an integration source shows.
//
// The field shape follows that frontend card: it must answer "what is this", "what is going on",
// "what should I do". These land in Name/Description, Status/Summary and FixHint/FixURL.
type IntegrationState struct {
	// ID is the provider's stable identifier (ama / lark-user); the frontend uses it for keys and testids.
	ID string `json:"id"`

	// Name / Description are the card's title and subtitle.
	Name        string `json:"name"`
	Description string `json:"description"`

	// Status is one of the four states.
	Status IntegrationStatus `json:"status"`

	// Summary is one sentence explaining the current situation, given for all four states.
	Summary string `json:"summary"`

	// Details are extra name/value pairs (authorized user, platform-side id, valid until...); may be empty.
	Details []IntegrationDetail `json:"details,omitempty"`

	// FixHint is "what to do", and should be given when not ok.
	FixHint string `json:"fixHint,omitempty"`

	// FixURL is "where to do it"; may be empty. It points to external addresses like a platform
	// console, and the frontend opens it in a new tab.
	FixURL string `json:"fixUrl,omitempty"`

	// CanAuthorize reports whether the "authorize" action makes sense right now.
	//
	// **This is where the difference between missing and unauthorized lands in the UI.** When not
	// wired it is false, and the frontend does not render the button: if it did, clicking it would
	// only give a 422, and the user would have no way to know the server needs configuring.
	CanAuthorize bool `json:"canAuthorize"`

	// CanRevoke reports whether the "remove authorization" action makes sense (authorized + the
	// platform supports removing).
	//
	// It is false when the platform does not support removing: better no button than one that is
	// sure to fail. See ErrRevokeUnsupported.
	CanRevoke bool `json:"canRevoke"`

	// CheckedAt is when this status was checked, RFC3339.
	CheckedAt string `json:"checkedAt"`

	// Injectable reports whether this integration source **injects credentials into the agent
	// container**.
	//
	// # Why this must come from the backend instead of the frontend guessing from the status
	//
	// "Ready" and "can inject something" are two different things, and guessing from status is wrong
	// in both directions at once:
	//
	//	a source only for the server itself  status ok, but the credential does not go into the container
	//	ama                                  status is missing when the server has no secret configured,
	//	                                     but it **is** an injecting source; it should be shown to
	//	                                     tell the user "an admin must configure it", not disappear
	//
	// The "integrations" checkbox area on the agent form should only list items with Injectable
	// true: ticking a source that injects nothing would make the user think that platform works in
	// the container, and they would spend ages on **a switch that never takes effect**, with nothing
	// in the UI to tell them that switch never did anything.
	//
	// ⚠️ **When adding a provider, think about this value explicitly.** The zero value false is the
	// safe side (not shown → at most one option missing), but forgetting true shows up as "configured
	// but cannot be ticked".
	Injectable bool `json:"injectable"`

	// PollsAuthorization reports "after clicking 'authorize', should the result be polled".
	//
	// # ⚠️ Why this must come from the backend instead of the frontend always or never polling
	//
	// The two kinds of providers finish authorization in **fundamentally different ways**:
	//
	//	OAuth redirect          the user finishes on the platform page, and **the platform records
	//	                        it itself**. The next status query sees it, so not polling is also
	//	                        right (the user clicks "check again").
	//	Feishu user auth        uses a **device code**, with no callback. **Only when we actively
	//	                        poll is the token exchanged and stored**; without polling, the user
	//	                        clicks consent in Feishu and we never know, and a hundred clicks on
	//	                        "check again" still show "unauthorized".
	//
	// So this is not a UX preference but **whether the feature works at all**. A frontend that never
	// polls would break the device code path completely, while always polling would spray 400s at
	// sources that do not support it.
	//
	// ⚠️ When adding a provider think about this value explicitly: the zero value false is the "old
	// behavior", right for sources that do not use a device code.
	PollsAuthorization bool `json:"pollsAuthorization"`

	// NeedsCredential reports "this source needs the user to **fill in a set of credentials by hand**".
	//
	// # ⚠️ Why this must come from the backend instead of the frontend checking the provider id
	//
	// Same reason as Injectable / PollsAuthorization: hard-coding `state.id === "service-account"` in
	// the frontend means keeping a provider list in the frontend; if some other source also needs
	// hand-entered credentials later, forgetting to change the frontend **gives no error**: that card
	// just has no form, and the user finds nowhere to fill anything in for a "not configured" status.
	//
	// It is different from CanAuthorize; do not merge them:
	//
	//	CanAuthorize=true, NeedsCredential=false  one click goes to the platform's authorization page (e.g. Feishu)
	//	CanAuthorize=true, NeedsCredential=true   fill in a set of credentials on this page by hand
	//
	// Merged, the frontend could not decide "render a button or a form" and would have to guess from
	// the provider id again.
	//
	// ⚠️ The zero value false is the "old behavior" (redirect authorization), right for existing
	// sources. No source uses true at the moment.
	NeedsCredential bool `json:"needsCredential"`

	// CredentialCollection reports "this source manages **a set of named credentials**, not just one".
	//
	// # ⚠️ Why this must be its own bit and cannot be covered by NeedsCredential
	//
	// Both are true, but they answer different questions:
	//
	//	NeedsCredential=true                              the user fills in credentials by hand
	//	NeedsCredential=true, CredentialCollection=false  fill in **one**; refilling overwrites it
	//	NeedsCredential=true, CredentialCollection=true   fill in **several**, each named, can add/delete
	//
	// The frontend decides which form to render from it. **The consequence of missing this bit was
	// observed**: looking only at NeedsCredential, a "several" card would render both an "overwrite
	// form" and a "multiple credentials" area: two forms, two identical security notices, and that
	// AK/SK form is sure to fail when submitted (the backend route does not accept this source).
	//
	// ⚠️ That bug **passed all tests with correct types**; only screenshots showed it. So the rule must
	// live in the backend; do not let the frontend guess "what shape this source is".
	//
	// The zero value false is the "old behavior" (one, overwritten), right for the existing source.
	CredentialCollection bool `json:"credentialCollection,omitempty"`

	// Kind is the **identity type** of this source: does it stand for a real person, or an app.
	//
	// # ⚠️ Why this must be declared by the backend (same reason as the four bits above, but costlier)
	//
	// Hard-coding `state.id === "service-account"` in the frontend means keeping a second provider
	// list there. Forgetting to change the four bits above costs "one form less / one switch more",
	// while forgetting this bit costs **in the security direction**: a new **personal** source would be
	// treated as a service account, and the UI would offer "add it to the workspace for everyone to
	// share", while its credential stands for a real person.
	//
	// So the rule lives in apimodel.IntegrationKindOf (an allow list; anything unknown counts as
	// personal), and this just copies it into the contract. **Providers do not implement this
	// themselves**, so the two places cannot give different answers.
	//
	// ⚠️ **No omitempty**: this bit must always appear in the response. If omitted, the frontend gets
	// undefined, and its default (personal) could not be told apart from "the backend is not upgraded
	// yet".
	Kind apimodel.IntegrationKind `json:"kind"`
}

// IntegrationProvider is what an integration source implements.
//
// The three actions map to three routes. **Not all of them are required**: if the platform has no
// remove endpoint, Revoke returns ErrRevokeUnsupported, the framework sets CanRevoke to false and
// tells the user the reason truthfully, much better than pretending to succeed ("clicked remove,
// the page says removed, the credential is still in the vault" is the worst outcome).
type IntegrationProvider interface {
	// ID is the stable identifier, used in URLs and frontend keys; only [a-z0-9-] is allowed.
	ID() string

	// Name / Description are the human-readable name and a one-sentence description.
	Name() string
	Description() string

	// State queries the current logged-in user's status on this source.
	//
	// **Rule: State does not return an error; it encodes failures as the IntegrationError status.**
	// An unreachable platform should not make the whole list 500: then the status of other providers
	// would also be invisible, although they are fine. The case where no actor can be taken from ctx
	// is stopped outside by the framework (see Registry.List).
	State(ctx context.Context, actor string) IntegrationState

	// AuthorizeURL gets an authorization page address for the user to click. Returns
	// ErrAuthorizeUnsupported if not supported.
	AuthorizeURL(ctx context.Context, actor string) (string, error)

	// Revoke removes the authorization. When the platform cannot do it, it returns
	// ErrRevokeUnsupported and says in the error message where to revoke it by hand.
	Revoke(ctx context.Context, actor string) error
}

// Sentinel errors of the framework.
var (
	// ErrAuthorizeUnsupported means this source is not connected by "click to authorize" (e.g. it
	// relies on server config).
	ErrAuthorizeUnsupported = errors.New("这个授权源不需要用户授权")

	// ErrRevokeUnsupported means the platform has no endpoint to remove authorization.
	//
	// This is not "not implemented yet"; **the platform really has no** remove endpoint. So report a
	// clear error and tell the user where to revoke it, instead of pretending to succeed.
	ErrRevokeUnsupported = errors.New("平台没有提供解除授权的接口")
)

// ProviderTimeout is the limit for one provider's status query.
//
// **It must be set per item**, not left to the outer request timeout: this is a synchronous path a
// user waits on a page for, and one stuck provider would drag the whole integration page to a
// browser timeout, while the other providers' statuses were all already fetched.
//
// The item that timed out is shown as warning, not error: "probe timed out" does not mean "this
// source is broken"; see the passage in List.
const ProviderTimeout = 10 * time.Second

// IntegrationRegistry is the provider registry; safe for concurrent use.
//
// The order is kept stable as registration order: the frontend's card order should not change on
// every refresh (that looks like a loading problem).
type IntegrationRegistry struct {
	mu        sync.RWMutex
	providers []IntegrationProvider
	byID      map[string]IntegrationProvider
	log       *slog.Logger
}

// NewIntegrationRegistry creates an empty registry.
func NewIntegrationRegistry(logger *slog.Logger) *IntegrationRegistry {
	if logger == nil {
		logger = slog.Default()
	}
	return &IntegrationRegistry{byID: map[string]IntegrationProvider{}, log: logger}
}

// Register registers a provider.
//
// **A duplicate ID panics right away**, the same choice as a missing declaration in the authz
// matrix: a duplicate registration means the later one silently overwrites the earlier, and the
// page then has one card less with no error. Things like authorization should fail loudly. This is
// a wiring mistake at startup and should not be left until run time.
func (r *IntegrationRegistry) Register(p IntegrationProvider) {
	if p == nil {
		panic("core: 注册的 IntegrationProvider 不能为 nil")
	}
	id := strings.TrimSpace(p.ID())
	if id == "" {
		panic("core: IntegrationProvider 的 ID 不能为空")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byID[id]; dup {
		panic(fmt.Sprintf("core: 授权源 %q 重复注册", id))
	}
	r.byID[id] = p
	r.providers = append(r.providers, p)
}

// Get gets a provider by id; returns persistmodel.ErrNotFound if not found.
//
// It returns persistmodel.ErrNotFound (→ 404) instead of 400: this means "this integration source
// does not exist", which is different from a badly formatted parameter. The frontend's provider list
// comes from the backend, so normally a missing id is never hit; if it is, the frontend has an old
// list, and 404 is the right signal.
func (r *IntegrationRegistry) Get(id string) (IntegrationProvider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.byID[strings.TrimSpace(id)]
	if !ok {
		return nil, fmt.Errorf("%w: 授权源 %q 不存在", persistmodel.ErrNotFound, id)
	}
	return p, nil
}

// List queries the status of all providers for the current user.
//
// It queries concurrently (each provider calls a platform once), but **returns in registration
// order**. An unstable order would make the cards jump around, which looks like the page broke.
//
// One provider's failure **does not affect the others**: State's rule is to encode failures as the
// error status rather than returning an error, and here a panic is also caught: a bug in one provider
// should not blank the whole integration page.
//
// # ⚠️ workspaceID may be empty, but the agent page must pass it
//
// Passed = computed for that workspace (the Feishu card's status and "authorize" resolve the same
// app). Not passed = account-level, the path of the integration management page, behaving exactly
// as before.
//
// **Forgetting it shows up as the bug fixed this time**: the agent page gets the status computed
// from the account default, while the "authorize" button next to it authorizes per workspace; the
// authorization succeeds, and the card still says "unauthorized". See stateOf.
func (r *IntegrationRegistry) List(
	ctx context.Context, workspaceID string,
) ([]IntegrationState, error) {
	actor := strings.TrimSpace(auth.ActorFrom(ctx))
	if actor == "" {
		return nil, fmt.Errorf("%w: 需要先登录", persistmodel.ErrInvalidArgument)
	}

	r.mu.RLock()
	providers := make([]IntegrationProvider, len(r.providers))
	copy(providers, r.providers)
	r.mu.RUnlock()

	states := make([]IntegrationState, len(providers))
	var wg sync.WaitGroup
	for i, p := range providers {
		wg.Add(1)
		go func(i int, p IntegrationProvider) {
			defer wg.Done()
			// Safety net: one provider panicking must not take the whole list down; one item
			// failing should not hide the whole page.
			defer func() {
				if rec := recover(); rec != nil {
					r.log.ErrorContext(ctx, "查授权状态时 panic",
						slog.String("provider", p.ID()), slog.Any("panic", rec))
					states[i] = IntegrationState{
						ID:        p.ID(),
						Name:      p.Name(),
						Status:    IntegrationError,
						Summary:   "检查这个授权源时发生内部错误",
						FixHint:   "这是服务端的问题，请把这条反馈给维护者",
						CheckedAt: nowISO(),
					}
				}
			}()
			// Per-item timeout: slow ones do not hold back fast ones; see ProviderTimeout.
			pctx, cancel := context.WithTimeout(ctx, ProviderTimeout)
			defer cancel()
			st := r.stateOf(pctx, p, actor, workspaceID)
			// ID/Name are filled in by the framework: if a provider forgot them the frontend gets no
			// key, showing up as an anonymous card in the list, and React reports duplicate keys.
			if st.ID == "" {
				st.ID = p.ID()
			}
			if st.Name == "" {
				st.Name = p.Name()
			}
			if st.Description == "" {
				st.Description = p.Description()
			}
			if st.CheckedAt == "" {
				st.CheckedAt = nowISO()
			}
			// ⚠️ **Kind is always overwritten by the framework, whatever the provider filled in.**
			// See the comment on backfillKind: the classification can have only one source of truth.
			backfillKind(&st)
			states[i] = st
		}(i, p)
	}
	wg.Wait()
	return states, nil
}

// State queries one provider's status.
//
// workspaceID may be empty. Non-empty means "compute for this workspace"; see the passage on stateOf.
func (r *IntegrationRegistry) State(
	ctx context.Context, id, workspaceID string,
) (IntegrationState, error) {
	actor := strings.TrimSpace(auth.ActorFrom(ctx))
	if actor == "" {
		return IntegrationState{}, fmt.Errorf("%w: 需要先登录", persistmodel.ErrInvalidArgument)
	}
	p, err := r.Get(id)
	if err != nil {
		return IntegrationState{}, err
	}
	st := r.stateOf(ctx, p, actor, workspaceID)
	if st.ID == "" {
		st.ID = p.ID()
	}
	if st.Name == "" {
		st.Name = p.Name()
	}
	if st.Description == "" {
		st.Description = p.Description()
	}
	if st.CheckedAt == "" {
		st.CheckedAt = nowISO()
	}
	// ⚠️ **Must match** the place in List, so both use backfillKind.
	backfillKind(&st)
	return st, nil
}

// backfillKind writes the identity classification into the status, **overwriting whatever the
// provider filled in**.
//
// # ⚠️ Why overwrite instead of "fill in only if the provider left it empty"
//
// The classification can have only one source of truth (the allow list in
// apimodel.IntegrationKindOf). Leaving room for "the provider can say it itself" would allow "some
// source calls itself a service account while the allow list says it is personal", and when the two
// disagree **nothing reports an error**; the UI just renders the wrong one, so a credential that
// stands for a real person is shown as "can be added to the workspace for everyone to share".
//
// That is the shape of the hole being prevented here, so providers get no say: they should not
// care about this at all, and the IntegrationProvider interface deliberately has no Kind() method.
//
// ⚠️ Both call sites (List and State) must call it. Missing one shows up as "right in the list, but
// the card changes after clicking 'check again'", fixing itself after one refresh, the hardest kind
// to track down.
func backfillKind(st *IntegrationState) {
	st.Kind = apimodel.IntegrationKindOf(st.ID)
}

// stateOf queries a provider's status, preferring its per-workspace version when a workspace is
// given.
//
// # ⚠️ Why the status must also be per workspace, not just "authorize"
//
// Because **checking the status and starting authorization must resolve the same Feishu app**. After
// the three paths (start / poll / list apps) all took `?workspaceId=`, the status check was left out,
// so three of the four were per workspace and one used the account default; the symptom was "the
// authorization succeeded, the card always says unauthorized", while the whole chain returned HTTP
// 200. See the comment on LarkUserProvider.StateForWorkspace and the production comparison there.
//
// # Here falling back silently **is allowed**, unlike AuthorizeURL
//
// That one does not fall back because "asked for workspace W but authorized another app" is a
// **mismatch with side effects** (the token lands on the wrong app). State is read-only: a provider
// that does not know workspaces (the other four sources have no notion of "app" at all) answering at
// account level is the right answer, and making it return 400 would only make the agent page's whole
// list fail to load: **four good cards would disappear together**, while only Feishu really needs to
// be computed per workspace.
func (r *IntegrationRegistry) stateOf(
	ctx context.Context, p IntegrationProvider, actor, workspaceID string,
) IntegrationState {
	if ws := strings.TrimSpace(workspaceID); ws != "" {
		if scoped, ok := p.(interface {
			StateForWorkspace(ctx context.Context, actor, workspaceID string) IntegrationState
		}); ok {
			return scoped.StateForWorkspace(ctx, actor, ws)
		}
	}
	return p.State(ctx, actor)
}

// AuthorizeURL gets the authorization page address of the given provider.
//
// workspaceID may be empty. Non-empty means "pick the app by this workspace"; see the passage on
// authorizeScoped below.
func (r *IntegrationRegistry) AuthorizeURL(
	ctx context.Context, id, workspaceID string,
) (string, error) {
	actor := strings.TrimSpace(auth.ActorFrom(ctx))
	if actor == "" {
		return "", fmt.Errorf("%w: 需要先登录", persistmodel.ErrInvalidArgument)
	}
	p, err := r.Get(id)
	if err != nil {
		return "", err
	}
	if ws := strings.TrimSpace(workspaceID); ws != "" {
		scoped, ok := p.(interface {
			AuthorizeURLForWorkspace(ctx context.Context, actor, workspaceID string) (string, error)
		})
		if !ok {
			// ⚠️⚠️ **Never silently fall back to AuthorizeURL.**
			//
			// Falling back means "the caller asked for workspace W and got the account default app,
			// with HTTP 200"; **that is exactly the defect being fixed this time, growing back as
			// 'graceful degradation'**: authorizing from the agent page authorizes another
			// workspace's app, then at run time it reports "you have not done user authorization for
			// Feishu app cli_xxx yet", and that sentence does not point to "you authorized the wrong
			// one".
			//
			// So fail explicitly here. A silent mismatch is much worse than a 400.
			return "", fmt.Errorf("%w: 授权源 %q 不按空间区分应用", ErrWorkspaceScopeUnsupported, id)
		}
		return scoped.AuthorizeURLForWorkspace(ctx, actor, ws)
	}
	return p.AuthorizeURL(ctx, actor)
}

// Revoke removes the authorization of the given provider.
func (r *IntegrationRegistry) Revoke(ctx context.Context, id string) error {
	actor := strings.TrimSpace(auth.ActorFrom(ctx))
	if actor == "" {
		return fmt.Errorf("%w: 需要先登录", persistmodel.ErrInvalidArgument)
	}
	p, err := r.Get(id)
	if err != nil {
		return err
	}
	return p.Revoke(ctx, actor)
}

// PollAuthorization checks once "has the user clicked consent yet".
//
// # Why it is needed: the device code flow has no callback
//
// With the authorization code flow the platform calls our callback address itself, while **with a
// device code we have to ask ourselves**. This repository's Feishu app cannot configure a
// redirect_uri allow list ("Known limits" in shared/infra/lark/README.md: this package cannot change
// the app config; only the app owner can do that step), so Feishu user authorization can only use a
// device code, and after clicking "authorize" the frontend must poll this route at pollInterval.
//
// # Only some providers support it
//
// Most sources do not need polling (refreshing the page after authorization is enough). **Those
// that do not implement it return ErrPollUnsupported → 400**, instead of pretending done: pretending
// done would make the frontend show "not authorized yet" as "authorized", exactly the kind of lie
// this four-state model most wants to avoid.
//
// An optional interface assertion instead of a method on IntegrationProvider: a method would mean
// changing every provider, and most of them could only write an "unsupported" empty shell, which is
// pure noise.
//
// # ⚠️ workspaceID must be exactly the same as when authorization was started
//
// Starting records the device_code under `(actor, app_id)`, and polling must use **the same app_id**
// to find it again. If the two disagree (start pinned to app B by workspace, poll resolved app A from
// the account default), GetLarkAuthSession finds nothing and returns "this authorization flow has
// expired", whose Pending is false; the frontend treats it as final and stops polling on the first
// tick. So **the user sees "expired" within 3 seconds of clicking authorize, while the start itself
// succeeded and the Feishu authorization page is open in another tab**, the same however many times
// they retry. This is the most typical shape of "only half wired", so this and AuthorizeURL must pass
// the same value.
func (r *IntegrationRegistry) PollAuthorization(
	ctx context.Context, id, workspaceID string,
) (AuthorizationStatus, error) {
	actor := strings.TrimSpace(auth.ActorFrom(ctx))
	if actor == "" {
		return AuthorizationStatus{}, fmt.Errorf("%w: 需要先登录", persistmodel.ErrInvalidArgument)
	}
	p, err := r.Get(id)
	if err != nil {
		return AuthorizationStatus{}, err
	}
	if ws := strings.TrimSpace(workspaceID); ws != "" {
		scoped, ok := p.(interface {
			PollAuthorizationForWorkspace(
				ctx context.Context, actor, workspaceID string,
			) (AuthorizationStatus, error)
		})
		if !ok {
			// Same as AuthorizeURL: **no fallback**. Falling back would make polling resolve a
			// different app from the start, exactly the dead end described above.
			return AuthorizationStatus{}, fmt.Errorf("%w: 授权源 %q 不按空间区分应用",
				ErrWorkspaceScopeUnsupported, id)
		}
		return scoped.PollAuthorizationForWorkspace(ctx, actor, ws)
	}
	poller, ok := p.(interface {
		PollAuthorization(ctx context.Context, actor string) (AuthorizationStatus, error)
	})
	if !ok {
		return AuthorizationStatus{}, fmt.Errorf("%w: 授权源 %q 不需要轮询授权结果",
			ErrPollUnsupported, id)
	}
	return poller.PollAuthorization(ctx, actor)
}

// ListApps lists the "sub-accounts / apps" this source can choose from, with each one's
// authorization status.
//
// # Why an optional interface instead of a method on IntegrationProvider
//
// Among existing providers **only Feishu user authorization has this notion** (one person may have
// registered several Feishu apps, and tokens are bound to apps). Adding it to the main interface
// would mean an "unsupported" empty shell for all the others, pure noise; same reasoning as
// PollAuthorization.
//
// # Why not put it into IntegrationState.Details
//
// Details are `{label, value}` name/value pairs; they can hold "currently using cli_xxx" but not
// "which three can be chosen and whether each is authorized". Forcing it into a string would make the
// frontend parse it, **and that parsing will drift**: if the backend changes a separator, the
// frontend silently misaligns, with nothing reporting an error.
//
// Sources that do not support it return ErrAppsUnsupported → 400.
func (r *IntegrationRegistry) ListApps(
	ctx context.Context, id, workspaceID string,
) ([]apimodel.LarkUserApp, error) {
	actor := strings.TrimSpace(auth.ActorFrom(ctx))
	if actor == "" {
		return nil, fmt.Errorf("%w: 需要先登录", persistmodel.ErrInvalidArgument)
	}
	p, err := r.Get(id)
	if err != nil {
		return nil, err
	}
	if ws := strings.TrimSpace(workspaceID); ws != "" {
		// With a workspace, Current means "which one would be authorized if authorization started
		// in this workspace"; the agent form uses it to show "the app about to be authorized".
		// Same as AuthorizeURL: if the assertion fails, fail explicitly, **do not silently fall back
		// to the account-level path**: falling back would make the app shown in the UI differ from
		// the one that would really be authorized.
		scoped, ok := p.(interface {
			ListAppsForWorkspace(
				ctx context.Context, actor, workspaceID string,
			) ([]apimodel.LarkUserApp, error)
		})
		if !ok {
			return nil, fmt.Errorf("%w: 授权源 %q 不按空间区分应用",
				ErrWorkspaceScopeUnsupported, id)
		}
		return scoped.ListAppsForWorkspace(ctx, actor, ws)
	}
	lister, ok := p.(interface {
		ListApps(ctx context.Context, actor string) ([]apimodel.LarkUserApp, error)
	})
	if !ok {
		return nil, fmt.Errorf("%w: 授权源 %q 没有可选的应用", ErrAppsUnsupported, id)
	}
	return lister.ListApps(ctx, actor)
}

// SelectApp switches the app this source uses by default to appID.
//
// ⚠️ **Switching apps is not switching authorization**: the new app may never have been authorized,
// and then the card becomes "unauthorized" + "authorize". That is right: tokens are bound to apps,
// so switching means authorizing the new one once more.
func (r *IntegrationRegistry) SelectApp(ctx context.Context, id, appID string) error {
	actor := strings.TrimSpace(auth.ActorFrom(ctx))
	if actor == "" {
		return fmt.Errorf("%w: 需要先登录", persistmodel.ErrInvalidArgument)
	}
	p, err := r.Get(id)
	if err != nil {
		return err
	}
	selector, ok := p.(interface {
		SelectApp(ctx context.Context, actor, appID string) error
	})
	if !ok {
		return fmt.Errorf("%w: 授权源 %q 没有可选的应用", ErrAppsUnsupported, id)
	}
	return selector.SelectApp(ctx, actor, appID)
}

// ErrAppsUnsupported means this source has no notion of "apps to choose from".
//
// Its own sentinel instead of reusing another: its meaning is narrow; not "no authorization needed",
// not "no polling", but "this source has only one identity, there is nothing to choose". All four
// sources are like this.
var ErrAppsUnsupported = errors.New("这个授权源没有可选的应用")

// ErrWorkspaceScopeUnsupported means the caller named a workspace, but this source does not tell
// apps apart by workspace.
//
// # Why a separate sentinel instead of reusing ErrAppsUnsupported
//
// Following this file's convention: **one meaning, one sentinel**. This one means "I cannot do the
// workspace restriction you asked for", unlike "I have no apps to choose": the former is **the caller
// passing a parameter it should not**, the latter is an inherent property of the source.
//
// ⚠️ More importantly it **must be an error and cannot degrade**. Allowing a silent fallback to the
// account-level path would mean "the caller asked for workspace W, got the account default app, HTTP
// 200", which is exactly the defect being fixed (authorizing from the agent page authorized another
// workspace's app, and the error did not point to the real cause).
var ErrWorkspaceScopeUnsupported = errors.New("这个授权源不按空间区分应用")

// ErrPollUnsupported means this source does not use "poll the authorization result" (no device code
// flow).
//
// Its own sentinel instead of reusing ErrAuthorizeUnsupported: that one means "this source needs no
// user authorization at all" (e.g. AMA itself), while this one means "needs authorization, but no
// polling afterwards". They map to different UI behavior; mixing them would make the frontend spin
// forever on a source that needs no polling.
var ErrPollUnsupported = errors.New("这个授权源不需要轮询授权结果")

// isTransient recognizes "this query found nothing, but that does not mean the authorization is
// broken": network hiccups, timeouts, cancellation.
//
// ⚠️ **Deliberately kept in core instead of moved down into a leaf package.** The 5 files that use it
// are all core/integration*.go: it only serves the single "integration source" domain and does not
// cross domains. The rule for moving down is "shared by several resource domains", not "looks like a
// generic tool"; moving by the latter is building a junk room.
//
// # Why "unknown" and "broken" must be kept apart
//
// The defensive intent of making "network error / failure not related to the session" a first-class
// state of its own is: **leave the authorization state unchanged in that case and let the existing
// flow keep running, so a network hiccup does not force a re-scan**.
//
// For AMA: showing a timeout as "unavailable / unauthorized" would make the user authorize again,
// while their authorization is actually fine, and **that wasted authorization would also overwrite
// the credential that was fine on the platform side**. So these are always degraded to warning,
// saying plainly "this check found nothing, which does not mean the authorization is invalid", and
// guiding to "check again later" instead of "authorize again".
//
// The check only accepts the ctx and net kinds, never the message text: text is given by upstream
// and can change any time.
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var nerr net.Error
	return errors.As(err, &nerr)
}

// maskCredential masks a credential-like value as "ellipsis + a few trailing characters", for
// display only.
//
// **The implementation is in `shared/infra/secret`** (the only copy in the repository); this just
// forwards to it. There used to be a copy here, another one in the tool layer, and the log
// layer wanted a third; after the three drifted apart separately, "which one is right" had no answer,
// and a missed mask gives no error.
//
// ⚠️ For the three reasons (keep only the tail, not the prefix; mask short values entirely; fixed
// mask width so the length does not leak), see the comment on secret.Mask, which also has the
// matching tests.
func maskCredential(s string, keepTail int) string {
	return secret.Mask(s, keepTail)
}

// ExpiryDetail builds a "valid until" detail; returns false for a zero value.
//
// It is pulled out because two providers need it, and both must avoid formatting the zero time as
// "0001-01-01", which the frontend would show as a real expiry time.
//
// The value is rendered as "2026-09-11 10:19:05 (14 days left)" rather than bare RFC3339: details are
// shown as plain text in the frontend, and a bare timestamp makes the user work out "how far away is
// that", while "14 days left" is what they really want to know.
func ExpiryDetail(label string, at time.Time) (IntegrationDetail, bool) {
	if !PlausibleExpiry(at) {
		return IntegrationDetail{}, false
	}
	return IntegrationDetail{Label: label, Value: fmtExpiry(time.Now(), at)}, true
}

// PlausibleExpiry decides whether an expiry time "looks real", catching the class of mixed-up units.
//
// # Why this seemingly redundant check is needed
//
// A typical failure: some platform's `expire_at` is in **milliseconds**; parsed as seconds it gives
// the year 58624, so that token never expires in our eyes, the cache never refreshes, and after it
// really expires every call gets 401. The opposite mistake gives 1970.
//
// Both would be rendered faithfully by fmtExpiry: the page would show "20889000 days left" or
// "expired 20586 days ago". So anything outside the range is **not shown**, sending the caller to the
// "no expiry" branch instead of presenting a lie to the user.
//
// The range is 2020–2100: this service cannot be handling credentials from before 2020, and nothing
// will expire only in 2100.
func PlausibleExpiry(at time.Time) bool {
	if at.IsZero() {
		return false
	}
	y := at.UTC().Year()
	return y >= 2020 && y <= 2100
}

// fmtExpiry renders an expiry time as "local time + (time left / expired how long ago)".
func fmtExpiry(now, at time.Time) string {
	local := at.Local().Format("2006-01-02 15:04:05")
	if d := at.Sub(now); d <= 0 {
		return local + "（已过期 " + humanDur(-d) + "）"
	}
	return local + "（还有 " + humanDur(at.Sub(now)) + "）"
}

// humanDur is the duration text (in Chinese) for fmtExpiry.
func humanDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d 小时", int(d.Hours()))
	default:
		return fmt.Sprintf("%d 天", int(d.Hours()/24))
	}
}

// FmtPastTime renders a **past** time as "local time (how long ago)".
//
// It pairs with fmtExpiry; do not mix them up: that one renders a **future** expiry time ("14 days
// left"), while this one renders "when it was configured / when it was last verified". Rendering a
// past time with fmtExpiry would give "expired 3 days ago", and saying that about something that
// never expires (service account AK/SK) would make the user think the credential is invalid.
//
// A zero or implausible time returns an empty string, and the caller then does not show the line;
// same handling as ExpiryDetail, so 0001-01-01 is not shown as a real time.
func FmtPastTime(at time.Time) string {
	if !PlausibleExpiry(at) {
		return ""
	}
	local := at.Local().Format("2006-01-02 15:04:05")
	if d := time.Since(at); d > 0 {
		return local + "（" + humanDur(d) + "前）"
	}
	return local
}

// AuthorizationStatus is the result of one "is it authorized yet" query.
//
// ⚠️ **It has no device_code**, which is as sensitive as a secret (holding it lets you exchange it
// for a token). This struct goes into the HTTP response.
//
// # ⚠️ Why it is here while its only implementation is still in core
//
// It is the return value of IntegrationRegistry.PollAuthorization, so it must go with the registry.
// The implementation (Feishu user authorization, whose credentials are stored) stays in core, and a
// **true alias** (`type AuthorizationStatus = integrationsvc.AuthorizationStatus` on the core side)
// keeps them the same type: the registry recognizes implementations **by structural assertion**, and
// two different types with the same name would make that assertion fail silently; see the passage
// in core/integration_larkuser.go.
type AuthorizationStatus struct {
	// Done true means the credential has been obtained and stored; the frontend can refresh the card.
	Done bool `json:"done"`
	// Pending true means still waiting for the user to click consent in Feishu.
	Pending bool `json:"pending"`
	// PollInterval is the suggested number of seconds until the next poll (given by Feishu).
	PollInterval int `json:"pollInterval,omitempty"`
	// Message is one sentence for the user. When not pending it states the result.
	Message string `json:"message,omitempty"`
}

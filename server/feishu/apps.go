// Reading and disconnecting apps.
//

package feishu

import (
	"context"
	"fmt"
	"strings"

	"github.com/chyroc/ama/server/model/apimodel"
)

// ListOption describes one list query.
type ListOption struct {
	// ActorID is the caller, required.
	ActorID string
	// WorkspaceID is the target workspace, required.
	WorkspaceID string
	// RefreshReadiness bypasses the check cache and checks again when true.
	//
	// The frontend sends it when the user comes back from the Feishu authorization page; at that moment the cached conclusion is almost certainly stale.
	RefreshReadiness bool
}

// ListApps lists the Feishu apps connected in a workspace, along with readiness.
// ChatOriginOf returns where an Ark session originally came from: Feishu app, chat, topic, anchor message.
//
// Used for agent self-wakeup: the task it schedules must send the result back to where it came from. The core side only declares what it needs
// (the core.ChatOrigins interface); the implementation is here and wired up by cmd/amad. The two modules are deliberately coupled only by ID
// strings; see the comment in core/agent.go.
//
// # Why read inflight_turns and not something else
//
// That table records "run → chat / source message", and **its lifetime exactly covers the moment the agent calls a tool**:
// written before a turn starts and deleted after it ends (see TryBeginTurn / DeleteInflightTurn in inbound.go).
// The agent calling the self-wakeup tool always happens within this turn, so it can be found.
//
// # Why this reasoning still holds after session reuse
//
// With sessions reused by scope, one runID runs many turns, which looks like "one run maps to several records,
// and which turn is found is uncertain". But turns on the same session are **serial** (see the comment on
// TryBeginTurn in runAndReply): at any moment the session has at most one inflight record,
// the turn that is running. So what is found is always the current turn, and the anchor is the current user message.
//
// Not found returns ok=false, and **that is not an error**: sessions opened from the chat page have no Feishu source to begin with.
// Not returning an error is deliberate: using error to express "none" would make the caller check each time whether it is a real error or just none.
func (s *Service) ChatOriginOf(ctx context.Context, runID string) (ChatOrigin, bool) {
	turns, err := s.store.ListInflightTurns(ctx)
	if err != nil {
		return ChatOrigin{}, false
	}
	for _, t := range turns {
		if t.RunID != runID {
			continue
		}
		app, err := s.store.GetAppByAppID(ctx, t.LarkAppID)
		if err != nil {
			// The app was disconnected, so delivery cannot be started.
			return ChatOrigin{}, false
		}
		return ChatOrigin{
			// **This is the AMA-side app ID**, not Feishu's cli_xxx: delivery uses it to call
			// Store.GetApp. These two IDs were mixed up once (see the comment on handler.listChats),
			// and the symptom was that configuring worked fine and delivery only failed when the time came.
			FeishuAppID: app.ID,
			ChatID:      t.ChatID,
			// SrcMessageID is the user's message; it is the anchor for "going back to the topic",
			// and also makes the result show up as a reply to that message.
			AnchorMessageID: t.SrcMessageID,
			// ReplyInThread records whether it was a topic reply originally; if so, this turn came from a topic.
			InThread: t.ReplyInThread,
		}, true
	}
	return ChatOrigin{}, false
}

// ChatOrigin is the Feishu source of a session.
type ChatOrigin struct {
	// FeishuAppID is the **AMA-side** app ID (feishu.App.ID).
	FeishuAppID string
	// ChatID is the Feishu chat ID.
	ChatID string
	// AnchorMessageID is the user message that triggered this turn, used for replying when delivering.
	AnchorMessageID string
	// InThread means this turn came from a topic, and the result should go back to that topic.
	InThread bool
}

// ListChats lists the "chats the bot has seen" in a workspace, for scheduled tasks to pick a delivery target.
//
// Auth is the same as ListApps (first check whether the actor can access this workspace), because it exposes the same level of
// information: which chats have this workspace's bot.
//
// When no chat has been recorded yet it returns an empty slice, **not an error**: that is the normal state of "the bot has not been @-mentioned in a chat yet",
// and the UI should show a hint (go @-mention the bot in a chat) instead of an error.
func (s *Service) ListChats(ctx context.Context, opt ListOption) ([]ChatRecord, error) {
	actorID := strings.TrimSpace(opt.ActorID)
	workspaceID := strings.TrimSpace(opt.WorkspaceID)
	if actorID == "" || workspaceID == "" {
		return nil, ErrInvalidArgument
	}
	ok, err := s.auth.CanAccessWorkspace(ctx, actorID, workspaceID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrAccessDenied
	}
	chats, err := s.store.ListChats(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	// Chats without a name get their name fetched on the spot, otherwise the dropdown only has a list of oc_xxx; see chatnames.go.
	s.fillChatNames(ctx, workspaceID, chats)
	return chats, nil
}

func (s *Service) ListApps(ctx context.Context, opt ListOption) ([]*App, error) {
	actorID := strings.TrimSpace(opt.ActorID)
	workspaceID := strings.TrimSpace(opt.WorkspaceID)
	if actorID == "" || workspaceID == "" {
		return nil, ErrInvalidArgument
	}
	ok, err := s.auth.CanAccessWorkspace(ctx, actorID, workspaceID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrAccessDenied
	}

	apps, err := s.store.ListApps(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	for i, app := range apps {
		s.fillBoundAgents(ctx, apps[i])
		secret, err := s.store.AppSecret(ctx, app.AppID)
		if err != nil {
			// If the secret is gone (should not happen in theory), skip the check and keep the existing conclusion.
			continue
		}
		if opt.RefreshReadiness {
			s.inspection.invalidate(app.AppID)
		}
		// A check failure does not affect returning the list: better one diagnosis fewer than a list page that cannot open.
		if refreshed, err := s.refreshReadiness(ctx, app, secret); err == nil {
			// refreshReadiness returns a different instance, so derived fields must be filled again.
			s.fillBoundAgents(ctx, refreshed)
			apps[i] = refreshed
		}
	}
	return apps, nil
}

// fillBoundAgents computes App.BoundAgentIDs on the fly.
//
// # This field is derived and must never be stored
//
// The source of truth for bindings is on the agent side (core's Agent.FeishuAppIDs); this is only a reverse view.
// Yet it lives in the App struct and is written back along with doc by SaveApp, so **it is only filled on the read
// path**; do not SaveApp after filling it. The same pitfall was noted in core/ark.go:237 (Run.AgentDeleted).
//
// # What happens if it is not filled
//
// Before the change this field was always [] in every response (Service had no dependency on core at all),
// so the delete confirmation of the frontend FeishuPage always showed "N agents are bound to it" as 0,
// and users thought it was safe to delete; while the list display bypassed it and computed from agents itself (the same on the api.ts side).
// One field with two algorithms, one of them always wrong, is what this change removes.
//
// When Unbinder is nil it stays as is (always an empty list): many tests do not wire up core,
// and should not lose the app list because of that.
func (s *Service) fillBoundAgents(ctx context.Context, app *App) {
	if app == nil || s.cfg.Unbinder == nil {
		return
	}
	ids, err := s.cfg.Unbinder.AgentsBoundToFeishuApp(ctx, app.ID)
	if err != nil {
		// If not found, keep the original value. Missing one reverse reference should not stop the whole list page from opening;
		// this matches how readiness check failures are handled.
		return
	}
	app.BoundAgentIDs = cloneStrings(ids)
}

// GetApp gets an app by AMA-side ID.
func (s *Service) GetApp(ctx context.Context, actorID, id string) (*App, error) {
	app, err := s.store.GetApp(ctx, strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	ok, err := s.auth.CanAccessWorkspace(ctx, strings.TrimSpace(actorID), app.WorkspaceID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrAccessDenied
	}
	s.fillBoundAgents(ctx, app)
	return app, nil
}

// SetTriggerMode changes an app's trigger mode (when it speaks in chats); values are listed in TriggerMode.
//
// Needs manage permission, consistent with other single-app writes (connect, disconnect, refresh readiness, request admin approval):
// "whether the bot replies in chats without an @" changes this app's external behavior, and members should not change it casually.
//
// **Unknown values fall back to the default mode instead of erroring** (handled by NormalizeTriggerMode): the frontend only sends these
// two values; if it really sends something else, the versions do not match, and then we let the bot fall back to the most conservative behavior rather than
// rejecting the whole save, so the user at least is not stuck on a page that cannot be changed.
//
// The readiness cache is not touched: the trigger mode is AMA-side configuration, unrelated to permissions/publish state on the Feishu side,
// and invalidating it in passing would only make the next list call the Feishu APIs once more.
func (s *Service) SetTriggerMode(ctx context.Context, actorID, id string, mode TriggerMode) (*App, error) {
	app, err := s.store.GetApp(ctx, strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	if err := s.requireManage(ctx, actorID, app.WorkspaceID); err != nil {
		return nil, err
	}
	app.TriggerMode = NormalizeTriggerMode(mode)
	app.UpdatedAt = s.now()
	saved, err := s.store.SaveApp(ctx, app)
	if err != nil {
		return nil, err
	}
	// Like GetApp, fill the reverse references on the read path: this response directly replaces the App held by the frontend,
	// and without filling, "N agents bound" on the page would suddenly become 0 after saving.
	s.fillBoundAgents(ctx, saved)
	return saved, nil
}

// DeleteApp disconnects an app.
//
// It only removes the connection on the AMA side, and does not delete the app on the Feishu side: it is the user's own app, the platform has no reason
// to delete it for them, and it may be in use elsewhere.
//
// After deleting, the bindings on the agent side must be removed too, or dangling feishuAppIds are left: the app is gone,
// but the agent detail page still shows "1 bound", and that binding points to an ID that cannot be found.
// The frontend LocalApiClient always did this cleanup (deleteFeishuApp in api.ts); the server missed it.
func (s *Service) DeleteApp(ctx context.Context, actorID, id string) error {
	app, err := s.store.GetApp(ctx, strings.TrimSpace(id))
	if err != nil {
		return err
	}
	if err := s.requireManage(ctx, actorID, app.WorkspaceID); err != nil {
		return err
	}
	s.inspection.invalidate(app.AppID)
	if err := s.store.DeleteApp(ctx, app.ID); err != nil {
		return err
	}
	if s.cfg.Unbinder != nil {
		// **Delete the app first, then unbind**, not the other way round: if unbinding fails the app is already disconnected (the main effect
		// the user sees is achieved), and the dangling reference shows up on the agent page as an invalid binding
		// that the user can remove. The other way round, unbinding succeeds but deleting fails, and the user sees the app still there
		// while the binding is gone, though they never touched the binding.
		if err := s.cfg.Unbinder.UnbindFeishuApp(ctx, app.ID); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// App resolution for the "Feishu user authorization" authorization source
// ---------------------------------------------------------------------------

// UserAppResolver answers "which Feishu app should this person use for user authorization".
//
// # Why it lives in this package
//
// Feishu apps (including app_secret) belong to server/feishu, and **core does not import feishu**
// (the core/core.go file header). So core only declares a consumer interface
// (core.LarkAppResolver), the implementation sits here, and cmd/amad assembles it:
// the same dependency inversion as SkillLoader / ManagedAPIKeys.
//
// # ⚠️ Why look up by "apps in the workspaces this person belongs to" and not "apps they registered themselves"
//
// Feishu apps are **workspace-level** resources (feishu_apps.workspace_id), while user authorization is **account-level**
// (one token per person). The two have different dimensions, and one rule must be picked:
//
// Here we pick "apps connected in the workspaces they belong to", because that is what the injection path needs: the agent in the container
// belongs to some workspace, and the chat messages it reads are received by that workspace's Feishu app. A token exchanged with an app
// unrelated to that workspace is useless: **the token is bound to the app** (lark-cli requires app_id and
// token in pairs), and with the wrong app lark-cli gets 99991668 "token invalid".
//
// # ⚠️⚠️ "Authorization is account-level, app choice is workspace-level": these two must not be merged
//
// There used to be only one `ResolveApp(ctx, actor)` here: walk through **all** of this person's workspaces and take the first app with
// a secret. That was a **zero-error mismatch**: the injection point (core/sessionenv.go) knew which workspace this
// run belongs to, but only passed the owner down, so this place guessed again. The user runs an
// agent in workspace C, the Feishu message is received by C's app, and the injected token may be from workspace A's app.
// In practice one user had three apps in three workspaces, so this is not a theoretical problem.
//
// Now the selection rule is gathered into **one** entry point + one query struct (apimodel.LarkAppQuery):
//
//	ResolveApp(ctx, LarkAppQuery{Actor, WorkspaceID, PreferAppID})
//
// A non-empty WorkspaceID pins it to that workspace (the injection path must fill it); empty means the account-level default
// (authorize, revoke authorization, card display). There is only one copy of the rule, so it cannot spread.
//
// ⚠️ **Do not make authorization per workspace too**. Authorization is one person clicking agree once for one Feishu app;
// the workspace only decides "which token to use this time". Merging them would make users scan a code again for every new workspace,
// while the table in the store (primary key `(actor, lark_app_id)`) naturally supports one person with multiple apps and multiple tokens.
type UserAppResolver struct {
	svc *Service
	// workspaces answers "which workspaces is this person in". Implemented by core.Service (it manages members).
	workspaces UserWorkspaceLister
}

// UserWorkspaceLister answers "which workspaces is this person a member of" and "what is a workspace called".
//
// Implemented on the core side (membership belongs to it). It is declared here instead of taking *core.Service directly,
// for the reason above: **this package must not import core**, or it would form a cycle with the core side.
type UserWorkspaceLister interface {
	// WorkspaceIDsFor returns the ids of the workspaces this person is in, in a stable order.
	WorkspaceIDsFor(ctx context.Context, actor string) ([]string, error)

	// WorkspaceNameOf returns a workspace's display name, for display only.
	//
	// Only used by the picker ("which workspace does this app belong to"), **not on the injection path**:
	// injection only needs app_id and secret, and looking up the name too would add a round trip to every bash command.
	WorkspaceNameOf(ctx context.Context, workspaceID string) (string, error)
}

// NewUserAppResolver creates a resolver. Returns nil (meaning not wired up) when any dependency is nil.
func NewUserAppResolver(svc *Service, workspaces UserWorkspaceLister) *UserAppResolver {
	if svc == nil || workspaces == nil {
		return nil
	}
	return &UserAppResolver{svc: svc, workspaces: workspaces}
}

// ResolveApp picks a Feishu app according to q, returning its app_id and **plaintext** secret.
//
// ⚠️ **The secret is plaintext**; it may only be used to exchange a token, never logged, never put in a response.
//
// # Priority: the workspace is hard, the preference is soft
//
// The rule lives only in this one function (see the comment on apimodel.LarkAppQuery), shared by three callers:
//
//	injection (core/sessionenv.go)  q.WorkspaceID = the workspace of this run  → pinned
//	authorize / revoke              q.WorkspaceID empty                       → account-level default
//	card display                    same as above
//
// When nothing is found it returns an error, matching the missing state on the card ("connect a Feishu app first"),
// instead of an empty appID: an empty value would make downstream send a request bound to fail,
// and that failure (invalid_client) does not point to "there is no app at all".
func (r *UserAppResolver) ResolveApp(
	ctx context.Context, q apimodel.LarkAppQuery,
) (appID, appSecret string, err error) {
	if r == nil || r.svc == nil {
		return "", "", fmt.Errorf("%w: 没接入飞书", ErrAppNotFound)
	}
	actor := strings.TrimSpace(q.Actor)
	if actor == "" {
		return "", "", fmt.Errorf("%w: 需要先登录", ErrAppNotFound)
	}
	want := strings.TrimSpace(q.PreferAppID)

	// ---- Hard constraint: pinned to one workspace ----
	//
	// **Do not fall back to other workspaces**; that is exactly the bug this change fixes. Falling back turns "this workspace has not connected
	// a Feishu app yet" into a silent mismatch, and the symptoms of a mismatch (401 / cannot read the chat) send people to check Feishu
	// permissions; the error does not point to "the wrong app was picked". Better to fail clearly.
	if wsID := strings.TrimSpace(q.WorkspaceID); wsID != "" {
		if want != "" {
			if id, secret, ok := r.appInWorkspace(ctx, wsID, want); ok {
				return id, secret, nil
			}
			// The preference is **soft**: if it is not in this workspace, ignore it and take the first one in this workspace.
			// Failing the whole query would be wrong: after a user picked an app of workspace A in settings,
			// every agent in workspace B would stop working.
		}
		if id, secret, ok := r.firstUsableApp(ctx, wsID); ok {
			return id, secret, nil
		}
		return "", "", fmt.Errorf("%w: 空间 %s 里没有可用的飞书应用", ErrAppNotFound, wsID)
	}

	// ---- No workspace context: account-level default ----
	wsIDs, err := r.workspaces.WorkspaceIDsFor(ctx, actor)
	if err != nil {
		return "", "", fmt.Errorf("查这个人所在的空间失败: %w", err)
	}
	// The one the user explicitly picked comes first. **It only counts if they can still see it**:
	// they may have been removed from that workspace after picking, and then this preference should quietly stop applying instead of blocking them.
	if want != "" {
		for _, wsID := range wsIDs {
			if id, secret, ok := r.appInWorkspace(ctx, wsID, want); ok {
				return id, secret, nil
			}
		}
	}
	for _, wsID := range wsIDs {
		if id, secret, ok := r.firstUsableApp(ctx, wsID); ok {
			return id, secret, nil
		}
	}
	return "", "", fmt.Errorf("%w: 这个人所在的空间里没有可用的飞书应用", ErrAppNotFound)
}

// WorkspaceIDsFor returns the ids of the workspaces this person is in.
//
// It just forwards to the workspaces it already holds (the one implemented by core.Service). It exists because
// **the core side needs it to check "the workspaceId named by the client: are you a member"**:
// the two authorization routes now take an optional `?workspaceId=`, and the workspace-pinned branch of ResolveApp
// queries the store directly without checking membership (that is deliberate for the trusted injection path; see the comment on firstUsableApp).
//
// ⚠️ The check is deliberately placed in core's provider layer, not here: ResolveApp is on the injection hot path of **every container command
// execution**, and adding an auth query inside it would add a round trip to every bash command.
// See the comment on this method on core.LarkAppResolver.
func (r *UserAppResolver) WorkspaceIDsFor(ctx context.Context, actor string) ([]string, error) {
	if r == nil || r.workspaces == nil {
		return nil, fmt.Errorf("%w: 没接入飞书", ErrAppNotFound)
	}
	return r.workspaces.WorkspaceIDsFor(ctx, actor)
}

// ListUserApps lists all Feishu apps this person can choose, for the "switch app" picker.
//
// # ⚠️ Not a single field here may be a credential
//
// It returns apimodel.LarkUserApp, which **has no** app_secret field (see the comment on that type):
// not "remember not to fill it", but it cannot be filled at all. app_id and the app name are not secrets.
//
// # Why dedupe by workspace rather than by app
//
// A person may see **the same** Feishu app in several workspaces at once (connecting the same cli_xxx to multiple
// workspaces is allowed). Two identical options in the picker make no sense, so dedupe by app_id,
// keeping the workspace name seen first.
//
// Order: by workspace order, and within a workspace by connection time ascending (the convention of Store.ListApps).
// **A stable order is required**: if it changed on every refresh, users would think the options are changing.
func (r *UserAppResolver) ListUserApps(
	ctx context.Context, actor string,
) ([]apimodel.LarkUserApp, error) {
	if r == nil || r.svc == nil {
		return nil, fmt.Errorf("%w: 没接入飞书", ErrAppNotFound)
	}
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return nil, fmt.Errorf("%w: 需要先登录", ErrAppNotFound)
	}
	wsIDs, err := r.workspaces.WorkspaceIDsFor(ctx, actor)
	if err != nil {
		return nil, fmt.Errorf("查这个人所在的空间失败: %w", err)
	}

	// **A non-nil empty slice**: this value goes into the HTTP response, nil encodes as null,
	// and the frontend would crash at .map on null (iron rule one of JSON tags in apimodel.go).
	out := make([]apimodel.LarkUserApp, 0, len(wsIDs))
	seen := map[string]bool{}
	for _, wsID := range wsIDs {
		apps, listErr := r.svc.store.ListApps(ctx, wsID)
		if listErr != nil {
			// One workspace failing to query should not fail the whole list: they may have other workspaces.
			continue
		}
		// The workspace display name. If it cannot be fetched, fall back to the id; **do not skip this app because of it**:
		// missing one column of display info is far better than an option disappearing entirely.
		wsName := wsID
		if ws, wsErr := r.workspaces.WorkspaceNameOf(ctx, wsID); wsErr == nil && ws != "" {
			wsName = ws
		}
		for _, app := range apps {
			if app == nil || strings.TrimSpace(app.AppID) == "" || seen[app.AppID] {
				continue
			}
			// An app without a secret cannot exchange a token; listing it would only give users
			// a baffling failure after they pick it. The test matches firstUsableApp word for word.
			secret, secErr := r.svc.store.AppSecret(ctx, app.AppID)
			if secErr != nil || strings.TrimSpace(secret) == "" {
				continue
			}
			seen[app.AppID] = true
			out = append(out, apimodel.LarkUserApp{
				AppID:         app.AppID,
				Name:          strings.TrimSpace(app.Name),
				WorkspaceName: wsName,
			})
		}
	}
	return out, nil
}

// firstUsableApp takes the first app in a workspace that "has an app_id and a secret".
//
// ⚠️ It goes through Store instead of Service.ListApps: the latter does auth and readiness checks,
// while this path **runs on every container command execution** (the injection path),
// and the check would add a round trip to Feishu to every bash command.
func (r *UserAppResolver) firstUsableApp(
	ctx context.Context, workspaceID string,
) (appID, appSecret string, ok bool) {
	apps, err := r.svc.store.ListApps(ctx, workspaceID)
	if err != nil {
		// One workspace failing to query should not fail the whole thing: they may have other workspaces.
		return "", "", false
	}
	for _, app := range apps {
		if app == nil || strings.TrimSpace(app.AppID) == "" {
			continue
		}
		secret, secErr := r.svc.store.AppSecret(ctx, app.AppID)
		if secErr != nil || strings.TrimSpace(secret) == "" {
			// There is an app but no secret (NULL and the empty string are deliberately not equivalent in the store; see feishu_pg.sql).
			// Exchanging a token needs a secret, so this app is unusable; keep looking for the next one.
			continue
		}
		return app.AppID, secret, true
	}
	return "", "", false
}

// appInWorkspace finds the app with the given app_id in a workspace and fetches its secret.
//
// Not found, or found but without a secret, both return ok=false: the two are the same thing to the caller
// (this preference cannot be used here), and telling them apart would give the call site one more branch nobody handles.
func (r *UserAppResolver) appInWorkspace(
	ctx context.Context, workspaceID, wantAppID string,
) (appID, appSecret string, ok bool) {
	apps, err := r.svc.store.ListApps(ctx, workspaceID)
	if err != nil {
		return "", "", false
	}
	for _, app := range apps {
		if app == nil || app.AppID != wantAppID {
			continue
		}
		secret, secErr := r.svc.store.AppSecret(ctx, app.AppID)
		if secErr != nil || strings.TrimSpace(secret) == "" {
			return "", "", false
		}
		return app.AppID, secret, true
	}
	return "", "", false
}

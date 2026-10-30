// Readiness diagnosis: after installing, can it really be used, and if not, which step is missing.
//
// # Why not a boolean
//
// "Not usable" has at least seven or eight causes: permission not requested, permission requested but not approved, events not subscribed, callbacks not subscribed,
// bot capability not enabled, version not published, credentials invalid. Each needs the user to do something completely different; returning only
// false would leave the user trying them one by one. So each one is reported separately here.
//
// The two most easily merged, and the two that must never be merged, are:
//
//	MissingScopes     never requested → the user adds them on the open platform
//	UnapprovedScopes  requested, waiting for approval → can only nudge the admin; submitting again does nothing
//
// Merged into one "missing permissions" list, users would keep clicking request on permissions they already submitted.
//
// # Why "missing permissions" and "missing event subscriptions" are no longer reported
//
// We use a **long connection** (see shared/infra/lark/longconn.go). In long-connection mode, permission grants and event subscriptions
// both **take effect at runtime**, and all we can read is "a snapshot of the published version"
// (events[] of GET /application/v6/applications/me/app_versions/{id}),
// and that snapshot cannot show the actual grants.
//
// Comparing against the snapshot **misreports working apps as missing a bunch of things**, forcing users onto the open platform
// to repeatedly request permissions that are already in effect. That is exactly what users saw before the change: "missing 1 permission im:chat",
// "missing 8 event subscriptions", while the app was actually fine.
//
// So now:
//
//	MissingScopes     no longer filled. A real missing permission errors **when calling the API**, and that is when it is a real problem
//	MissingEvents     no longer filled. Long-connection events do not need to be ticked one by one in the console
//	UnapprovedScopes  still filled. It comes from grant_status of /application/v6/scopes, real-time and trustworthy
//	MissingCallbacks  still filled, with the extra check that callback_type must be websocket
//
// Ready or not **is decided by the liveness probe**: being able to read the bot info means the path really works.
package feishu

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chyroc/ama/server/shared/infra/lark"
)

// inspectionTTL is how long check results are cached.
//
// The list page shows readiness on every refresh, and a check calls 3~4 Feishu APIs. 45 seconds is enough to absorb
// repeated refreshes, yet short enough that users coming back after changing config on the open platform see the new result.
const inspectionTTL = 45 * time.Second

// inspectionCache caches check results per app. Safe for concurrent use.
type inspectionCache struct {
	mu      sync.Mutex
	entries map[string]inspectionEntry
	now     func() time.Time
}

type inspectionEntry struct {
	config    *lark.AppConfiguration
	checkErr  string
	expiresAt time.Time
	// secretDigest makes sure old results are not reused after the secret changes.
	secretDigest string
}

func newInspectionCache(now func() time.Time) *inspectionCache {
	return &inspectionCache{entries: make(map[string]inspectionEntry), now: now}
}

func (c *inspectionCache) get(appID, digest string) (*lark.AppConfiguration, string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[appID]
	if !ok || entry.secretDigest != digest || c.now().After(entry.expiresAt) {
		return nil, "", false
	}
	return entry.config, entry.checkErr, true
}

func (c *inspectionCache) put(appID, digest string, config *lark.AppConfiguration, checkErr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[appID] = inspectionEntry{
		config:       config,
		checkErr:     checkErr,
		expiresAt:    c.now().Add(inspectionTTL),
		secretDigest: digest,
	}
}

func (c *inspectionCache) invalidate(appID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, appID)
}

// RefreshReadiness checks an app again and writes back the result.
//
// It returns the whole App rather than just Readiness: the frontend can update with one setState, without fetching the list again.
// This matches the shape of verifyArkCredential.
func (s *Service) RefreshReadiness(ctx context.Context, actorID, appID string) (*App, error) {
	app, err := s.store.GetApp(ctx, strings.TrimSpace(appID))
	if err != nil {
		return nil, err
	}
	if err := s.requireManage(ctx, actorID, app.WorkspaceID); err != nil {
		return nil, err
	}
	secret, err := s.store.AppSecret(ctx, app.AppID)
	if err != nil {
		return nil, err
	}
	// When the user clicks "check again" we should not fob them off with the cache: they likely just changed config on the open platform.
	s.inspection.invalidate(app.AppID)
	return s.refreshReadiness(ctx, app, secret)
}

// RequestAdminAuthorization submits an admin approval request, then checks again.
func (s *Service) RequestAdminAuthorization(ctx context.Context, actorID, appID string) (*App, error) {
	app, err := s.store.GetApp(ctx, strings.TrimSpace(appID))
	if err != nil {
		return nil, err
	}
	if err := s.requireManage(ctx, actorID, app.WorkspaceID); err != nil {
		return nil, err
	}
	secret, err := s.store.AppSecret(ctx, app.AppID)
	if err != nil {
		return nil, err
	}
	client, err := s.larkClient(lark.NormalizeRegion(app.Region))
	if err != nil {
		return nil, err
	}
	token, err := client.TenantAccessToken(ctx, app.AppID, secret)
	if err != nil {
		return nil, err
	}
	if err := client.RequestAdminAuthorization(ctx, token); err != nil {
		return nil, err
	}
	// The request state changed, so the cached check result is void immediately.
	s.inspection.invalidate(app.AppID)
	app.AdminAuthorizationRequestedAt = ptrTime(s.now())
	if _, err := s.store.SaveApp(ctx, app); err != nil {
		return nil, err
	}
	return s.refreshReadiness(ctx, app, secret)
}

// refreshReadiness checks and writes the result back to App.
func (s *Service) refreshReadiness(ctx context.Context, app *App, secret string) (*App, error) {
	readiness := s.inspect(ctx, app, secret)
	// The authorization page URL is added here, not in inspect: inspect has several early return paths (client cannot be built,
	// check failed), and **those are exactly the times users most need a link to click**. Putting it at the single meeting point
	// means not having to remember to fill it on each early return path.
	//
	// Left empty when granted: empty/non-empty is itself the frontend's "show a hint or not" switch.
	if !readiness.GroupMessageScopeGranted {
		readiness.GroupMessageAuthURL = lark.ScopeApplyURL(
			lark.NormalizeRegion(app.Region), app.AppID, lark.ScopeGroupMessage)
	}
	// The level that includes bots gets its own link: it and the level above **may be one present, one missing** (existing apps
	// originally only requested group_msg), and one combined link would make users think adding one is enough.
	// Both IncludeBot permissions are requested together; see onboarding.go for why.
	if !readiness.BotMessageScopeGranted {
		readiness.BotMessageAuthURL = lark.ScopeApplyURL(
			lark.NormalizeRegion(app.Region), app.AppID,
			lark.ScopeGroupMessageIncludeBot, lark.ScopeGroupAtMessageIncludeBot)
	}
	app.Readiness = readiness
	app.Status = statusFor(readiness)
	// When the check can read the app name and avatar, refresh them along the way, saving a separate "sync display info".
	if readiness.ConfigurationInspectable {
		if name := strings.TrimSpace(readiness.inspectedName); name != "" {
			app.Name = name
		}
		if avatar := strings.TrimSpace(readiness.inspectedAvatar); avatar != "" {
			app.AvatarURL = avatar
		}
	}
	if readiness.inspectedBotOpenID != "" {
		app.BotOpenID = readiness.inspectedBotOpenID
	}
	app.UpdatedAt = s.now()
	return s.store.SaveApp(ctx, app)
}

// inspect calls Feishu APIs and derives a readiness.
func (s *Service) inspect(ctx context.Context, app *App, secret string) *Readiness {
	now := s.now()
	r := &Readiness{
		Status:           ReadinessSetupRequired,
		CheckedAt:        now,
		MissingScopes:    []string{},
		UnapprovedScopes: []string{},
		MissingEvents:    []string{},
		MissingCallbacks: []string{},
		// Permission names are always sent, regardless of "is it granted": the UI uses them to build hint text, and the string
		// must come from the same source as the backend constant, otherwise the frontend misses a change to the constant, and users following the hint cannot find it on the open platform.
		GroupMessageScope: lark.ScopeGroupMessage,
		BotMessageScope:   lark.ScopeGroupMessageIncludeBot,
	}
	if app.AdminAuthorizationRequestedAt != nil {
		r.AdminAuthorizationRequested = true
	}

	client, err := s.larkClient(lark.NormalizeRegion(app.Region))
	if err != nil {
		r.LastCheckError = "无法为该应用创建飞书客户端"
		return r
	}

	digest := secretDigest(secret)
	config, checkErr, cached := s.inspection.get(app.AppID, digest)
	if !cached {
		token, tokenErr := client.TenantAccessToken(ctx, app.AppID, secret)
		if tokenErr != nil {
			// Failing to get a token means the credentials are no longer usable, and the rest of the check is meaningless.
			r.LastRuntimeError = "无法用当前凭证连上这个应用，凭证可能已失效"
			r.Status = ReadinessRuntimeUnavailable
			s.inspection.put(app.AppID, digest, nil, r.LastRuntimeError)
			return r
		}
		config, err = client.InspectApp(ctx, token)
		if err != nil {
			checkErr = "无法读取该应用在飞书侧的配置，请检查凭证后重试"
		}
		s.inspection.put(app.AppID, digest, config, checkErr)

		// Liveness probe: being able to read the bot info counts as connected.
		if bot, botErr := client.BotInfo(ctx, token); botErr == nil && bot != nil {
			r.RuntimeConnected = true
			ts := now
			r.LastConnectedAt = &ts
			r.inspectedBotOpenID = bot.OpenID
		} else {
			r.LastRuntimeError = "无法用当前凭证连上这个应用，凭证可能已失效"
		}
	} else {
		// On a cache hit do not probe again; keep the conclusion "the config was readable, so it could connect then".
		if config != nil {
			r.RuntimeConnected = true
			ts := now
			r.LastConnectedAt = &ts
		}
	}
	r.LastCheckError = checkErr

	if config != nil {
		r.ConfigurationInspectable = config.Inspectable
		r.BotEnabled = config.BotEnabled
		r.Published = config.Published
		r.GroupMessageScopeGranted = lark.ContainsValue(config.GrantedScopes, lark.ScopeGroupMessage)
		// The level that includes bots: the no-@ scenario only accepts group_msg.include_bot:read.
		// **group_at_msg.include_bot is not counted**: that only covers messages "that @ me",
		// while this field answers "among chat messages without @, can what bots say be received".
		// Mixing the two would show an app that only requested the @ level as "ready", and it would still not receive alerts.
		r.BotMessageScopeGranted = lark.ContainsValue(config.GrantedScopes, lark.ScopeGroupMessageIncludeBot)
		r.inspectedName = config.Name
		r.inspectedAvatar = config.AvatarURL

		// Permissions requested and waiting for admin approval **are still reported**: this comes from grant_status of
		// /application/v6/scopes, which is real-time and trustworthy with or without a long connection.
		//
		// Note it only takes **required** permissions: the pending list Feishu returns may include permissions the user requested for other purposes,
		// and they should not block AMA's readiness. The optional chat message permission is expressed separately with
		// GroupMessageScopeGranted.
		for _, scope := range lark.MissingValues(lark.BaseScopes, config.GrantedScopes) {
			if lark.ContainsValue(config.UnapprovedScopes, scope) {
				r.UnapprovedScopes = append(r.UnapprovedScopes, scope)
			}
			// With the long connection **MissingScopes is no longer reported**; see the comment below for why.
		}

		// Events and callbacks are only trustworthy when the app details could be read; if not, do not report them missing, or it is a groundless accusation.
		if config.Inspectable {
			r.MissingCallbacks = lark.MissingValues(lark.BaseCallbacks, config.SubscribedCallbacks)
			// In long-connection mode, card callbacks must be delivered over websocket. With the wrong callback type,
			// clicking buttons on cards does nothing; then a missing callback should be reported.
			//
			// This is also the only consumer of AppConfiguration.CallbackType. It used to be parsed and
			// assigned, and then nobody in the whole repo read it.
			if config.CallbackType != lark.CallbackTypeWebSocket {
				r.MissingCallbacks = lark.MissingValues(lark.BaseCallbacks, nil)
			}
		}
		sort.Strings(r.MissingScopes)
		sort.Strings(r.UnapprovedScopes)
		sort.Strings(r.MissingEvents)
		sort.Strings(r.MissingCallbacks)
	}

	r.Status = DeriveStatus(r)
	return r
}

// deriveStatus derives the overall status from the individual facts.
func DeriveStatus(r *Readiness) string {
	// "Waiting for admin" is checked first: when required permissions are stuck in approval, whether the rest of the config is complete does not change the conclusion;
	// the only thing the user can do right now is nudge the admin.
	//
	// This branch must come before "all complete": pending permissions also count as not granted, and if "all complete" were checked first, an app purely stuck
	// in approval would be judged ready or runtime_unavailable because all other conditions are met,
	// swallowing the only useful hint, "go nudge the admin".
	if len(r.UnapprovedScopes) > 0 {
		if len(r.MissingScopes) > 0 {
			// Both never-requested and pending ones: have the user add the never-requested ones first, otherwise they would wait for
			// an approval that is never enough.
			return ReadinessSetupRequired
		}
		return ReadinessWaitingForAdmin
	}
	// All complete: check whether it can connect.
	if r.LastCheckError == "" && r.ConfigurationInspectable && r.BotEnabled && r.Published &&
		len(r.MissingScopes) == 0 && len(r.MissingEvents) == 0 && len(r.MissingCallbacks) == 0 {
		if r.RuntimeConnected {
			return ReadinessReady
		}
		return ReadinessRuntimeUnavailable
	}
	return ReadinessSetupRequired
}

// statusFor folds readiness into the frontend's three-valued status.
func statusFor(r *Readiness) AppStatus {
	switch r.Status {
	case ReadinessReady:
		return StatusConnected
	case ReadinessRuntimeUnavailable:
		return StatusError
	default:
		return StatusUnverified
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

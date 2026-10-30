// Registration orchestration: start, poll, store.
//
// This file has four places that must not be changed on assumption; each is described in its function's comment:
//
//  1. The order "authenticate first, then look up the session" in Poll (a security boundary)
//  2. The session is bound to its initiator (prevents others in the same workspace from taking over someone else's registration)
//  3. Automatic dual-domain correction (do not make the user start over when the domain was guessed wrong)
//  4. Idempotent registration (read back on a duplicate; if it is the same person, treat it as a reconnect)

package feishu

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/chyroc/ama/server/shared/infra/lark"
)

// BeginOption describes one registration start.
type BeginOption struct {
	// ActorID is the initiator, required.
	ActorID string
	// WorkspaceID is the target workspace, required.
	WorkspaceID string
	// Region is the account domain; empty means mainland China. A wrong value from the user is fine; it is corrected automatically during poll.
	Region string
	// A non-empty AppID means adding permissions to an existing app, not creating a new one.
	AppID string
	// When RequestGroupMessage is true, also request the permission "receive chat messages without @".
	RequestGroupMessage bool
}

// PollOption describes one poll.
type PollOption struct {
	// ActorID is the caller, required. It must be the initiator themselves.
	ActorID string
	// WorkspaceID is the workspace at the time of starting, required.
	WorkspaceID string
	// SessionID is the session ID returned at start, required.
	SessionID string
}

// Begin starts a device code registration and returns the URL for the user to authorize and the polling credential.
func (s *Service) Begin(ctx context.Context, opt BeginOption) (*Challenge, error) {
	if err := s.requireManage(ctx, opt.ActorID, opt.WorkspaceID); err != nil {
		return nil, err
	}
	region := lark.NormalizeRegion(opt.Region)
	client, err := s.larkClient(region)
	if err != nil {
		return nil, err
	}

	// Extra permissions are only requested when the user really wants "no @ in chats": asking for more permissions makes admins more hesitant to approve.
	//
	// ⚠️ **Request all three together, not just ScopeGroupMessage.** The latter only covers chat messages "sent by users",
	// and not a single bot message is pushed, while alert chats are exactly where only bots talk (the 2026-09-02 failure
	// came from this; see the comment on the IncludeBot constant in lark/scopes.go).
	//
	// Requesting them separately has no value for users, only a real cost: **once the authorization page URL is generated it is fixed**,
	// and a missed permission **can only be fixed by making the user go through the whole authorization again** (the addons parameter is encoded in the URL and
	// cannot be changed after the page opens). So "no @ ticked" requests this whole level at once.
	var tenantScopes []string
	if opt.RequestGroupMessage {
		tenantScopes = append(tenantScopes,
			lark.ScopeGroupMessage,
			lark.ScopeGroupMessageIncludeBot,
			lark.ScopeGroupAtMessageIncludeBot,
		)
	}

	challenge, err := client.Begin(ctx, lark.BeginSpec{
		AppID:        strings.TrimSpace(opt.AppID),
		AppName:      defaultAppName,
		Source:       registrationSource,
		TenantScopes: tenantScopes,
		Callbacks:    lark.BaseCallbacks,
	})
	if err != nil {
		return nil, err
	}

	sessionID, err := newSessionID()
	if err != nil {
		return nil, err
	}
	interval := challenge.Interval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	// The validity takes the shorter of "what Feishu gives" and "this platform's limit"; see the comment on sessionTTL.
	ttl := sessionTTL
	if challenge.ExpiresIn > 0 {
		if fromLark := time.Duration(challenge.ExpiresIn) * time.Second; fromLark < ttl {
			ttl = fromLark
		}
	}
	expiresAt := s.now().Add(ttl)

	session := Session{
		WorkspaceID:         strings.TrimSpace(opt.WorkspaceID),
		ActorID:             strings.TrimSpace(opt.ActorID),
		DeviceCode:          challenge.DeviceCode,
		Region:              string(region),
		PollIntervalSeconds: interval,
		ExpiresAt:           expiresAt,
	}
	if err := s.store.PutSession(ctx, sessionID, session, ttl); err != nil {
		return nil, err
	}
	return &Challenge{
		SessionID:           sessionID,
		VerificationURL:     challenge.VerificationURL,
		ExpiresAt:           expiresAt,
		PollIntervalSeconds: interval,
	}, nil
}

// Poll checks the authorization result once, and stores the app along the way when authorization is complete.
//
// # Authentication must come before the session lookup
//
// The first line is requireManage, and only **after** it is the SessionID taken to Store. In the reverse order,
// sessionID would become a de facto credential: whoever gets it could advance the registration flow and get the registration result.
// The session ID is high-entropy (see session.go), but "unguessable" should not be the only defense.
func (s *Service) Poll(ctx context.Context, opt PollOption) (*PollResult, error) {
	if err := s.requireManage(ctx, opt.ActorID, opt.WorkspaceID); err != nil {
		return nil, err
	}
	sessionID := strings.TrimSpace(opt.SessionID)
	if sessionID == "" {
		return nil, ErrInvalidArgument
	}
	session, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	// The session is bound to the workspace and the initiator. Even other admins in the same workspace cannot take over someone else's registration in progress:
	// that would let A's authorization result land in B's hands. Both mismatches return ErrAccessDenied, without distinguishing
	// "the session does not exist" from "it is not yours", so as not to reveal whether the session exists.
	if session.WorkspaceID != strings.TrimSpace(opt.WorkspaceID) {
		return nil, ErrAccessDenied
	}
	if session.ActorID != strings.TrimSpace(opt.ActorID) {
		return nil, ErrAccessDenied
	}

	if s.now().After(session.ExpiresAt) {
		_ = s.store.DeleteSession(ctx, sessionID)
		return &PollResult{Status: RegistrationExpired, Reason: "注册会话已过期，请重新发起接入"}, nil
	}

	region := lark.NormalizeRegion(session.Region)
	client, err := s.larkClient(region)
	if err != nil {
		return nil, err
	}
	ticket, err := client.Poll(ctx, session.DeviceCode)
	if err != nil {
		return nil, err
	}

	switch ticket.Status {
	case lark.PollOK:
		// The protocol says no error, but the credentials may still be on their way. Try correcting the account domain first, then keep waiting:
		// a wrongly guessed domain shows up as "always OK but never gets credentials".
		if ticket.AppID == "" || ticket.AppSecret == "" {
			s.CorrectRegion(ctx, sessionID, &session, ticket.TenantBrand)
			return s.pendingResult(session), nil
		}
		app, err := s.completeRegistration(ctx, opt, session, ticket)
		if err != nil {
			return nil, err
		}
		_ = s.store.DeleteSession(ctx, sessionID)
		return &PollResult{Status: RegistrationCompleted, App: app}, nil

	case lark.PollPending:
		return s.pendingResult(session), nil

	case lark.PollSlowDown:
		// Feishu thinks we are too fast; increase the interval and write it back to the session, so later polls use the new interval.
		session.PollIntervalSeconds += SlowDownIncrement
		_ = s.store.PutSession(ctx, sessionID, session, time.Until(session.ExpiresAt))
		return s.pendingResult(session), nil

	case lark.PollExpired:
		_ = s.store.DeleteSession(ctx, sessionID)
		return &PollResult{Status: RegistrationExpired, Reason: "飞书侧的授权码已过期，请重新发起接入"}, nil

	case lark.PollDenied:
		_ = s.store.DeleteSession(ctx, sessionID)
		return &PollResult{Status: RegistrationDenied, Reason: "授权被拒绝。如果这不是你的操作，请重新发起接入"}, nil

	case lark.PollInvalid:
		// An invalid device code is final and unrelated to the account domain; do not try switching domains and retrying here.
		_ = s.store.DeleteSession(ctx, sessionID)
		return &PollResult{Status: RegistrationInvalid, Reason: "授权码无效，请重新发起接入"}, nil

	default:
		// An error code the protocol does not define. A wrongly guessed domain often lands here, so try correcting first; only fail if it cannot be corrected.
		if s.CorrectRegion(ctx, sessionID, &session, ticket.TenantBrand) {
			return s.pendingResult(session), nil
		}
		_ = s.store.DeleteSession(ctx, sessionID)
		reason := strings.TrimSpace(ticket.ErrorDescription)
		if reason == "" {
			reason = "飞书返回了未知的授权状态，请重新发起接入"
		}
		return &PollResult{Status: RegistrationInvalid, Reason: reason}, nil
	}
}

// pendingResult builds a "still waiting" result.
func (s *Service) pendingResult(session Session) *PollResult {
	return &PollResult{Status: RegistrationPending, PollIntervalSeconds: session.PollIntervalSeconds}
}

// correctRegion changes the session's domain in place and keeps polling when the tenant's real home differs from the current account domain.
//
// Mainland China / international are two separate tenant systems, and users often cannot tell which side they are on. The
// tenant_brand returned by poll reveals the real home, and changing the domain from it saves the step "make the user start over",
// and starting over the user would likely choose wrong again, since they did not know in the first place.
//
// Returns whether it really changed.
func (s *Service) CorrectRegion(ctx context.Context, sessionID string, session *Session, tenantBrand string) bool {
	brand := strings.ToLower(strings.TrimSpace(tenantBrand))
	// Only these two values are accepted. An empty string or anything else means Feishu did not tell us the home; if we cannot tell, do not change it.
	if brand != string(lark.RegionFeishu) && brand != string(lark.RegionLark) {
		return false
	}
	next := lark.NormalizeRegion(brand)
	if next == lark.NormalizeRegion(session.Region) {
		return false
	}
	session.Region = string(next)
	_ = s.store.PutSession(ctx, sessionID, *session, time.Until(session.ExpiresAt))
	return true
}

// completeRegistration stores the registration result.
//
// # Idempotency
//
// On hitting the appID unique key, read back the existing row:
//
//   - still owned by the same person → treat it as **a successful reconnect**, refresh the secret and return. Reinstalling, switching machines, or
//     simply clicking connect again all end up here, and a 409 would only confuse people.
//   - owned by someone else → only then really error. The same Feishu app connected by two people would make message routing undecidable.
func (s *Service) completeRegistration(ctx context.Context, opt PollOption, session Session, ticket *lark.Ticket) (*App, error) {
	id, err := newAppID()
	if err != nil {
		return nil, err
	}
	now := s.now()
	region := session.Region
	// The home reported by Feishu on successful registration is the most authoritative, so it overwrites the possibly wrongly guessed domain in the session.
	if brand := strings.ToLower(strings.TrimSpace(ticket.TenantBrand)); brand == string(lark.RegionFeishu) || brand == string(lark.RegionLark) {
		region = brand
	}
	candidate := &App{
		ID:            id,
		WorkspaceID:   session.WorkspaceID,
		Name:          defaultAppName,
		AppID:         ticket.AppID,
		Region:        region,
		TenantBrand:   ticket.TenantBrand,
		Status:        StatusUnverified,
		BoundAgentIDs: []string{},
		CreatedByID:   session.ActorID,
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	app, err := s.store.CreateApp(ctx, candidate, ticket.AppSecret)
	if errors.Is(err, ErrAppExists) {
		existing, readErr := s.store.GetAppByAppID(ctx, ticket.AppID)
		if readErr != nil {
			return nil, readErr
		}
		if existing.CreatedByID != session.ActorID {
			return nil, ErrAppOwnedByOther
		}
		// Reconnect by the same person: switch to the new secret (the old one may have been rotated by Feishu), and refresh the home info.
		if err := s.store.SetAppSecret(ctx, existing.AppID, ticket.AppSecret); err != nil {
			return nil, err
		}
		existing.WorkspaceID = session.WorkspaceID
		existing.Region = region
		existing.TenantBrand = ticket.TenantBrand
		existing.UpdatedAt = now
		// The secret changed, so the old check conclusion is void immediately: the same app with new credentials, the earlier "can connect"
		// conclusion cannot vouch for the new credentials.
		existing.Readiness = nil
		existing.Status = StatusUnverified
		s.inspection.invalidate(existing.AppID)
		app, err = s.store.SaveApp(ctx, existing)
		if err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}

	// Check right away: the user just finished authorizing, and what they most want to know is "can it be used now".
	// A check failure does not change the fact that registration itself succeeded, so errors here are ignored.
	if refreshed, err := s.refreshReadiness(ctx, app, ticket.AppSecret); err == nil {
		app = refreshed
	}
	return app, nil
}

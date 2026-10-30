// Service is the business entry point of auth: login (local account / OIDC), issuing sessions,
// checking identities.
//
// # Several login methods, one exit
//
//	Local account  LoginWithPassword / Signup        see local.go
//	OIDC           StartOIDCLogin / CompleteOIDCLogin see oidc.go
//	GitHub         StartGitHubLogin / PollGitHubLogin see github.go (device flow)
//
// All three paths end in finishLogin: issue a session, write the users profile, call the
// post-login hook. So "what happens after a successful login" exists in one piece of code only,
// and "people who log in with OIDC have a profile, people who use a password do not" cannot happen.
//
// The methods can be on at the same time, or just one; **with none on the service still starts**
// (the fixed API Key and user API Keys still work), but people cannot log in; cmd/amad says this
// loudly in the startup log.
//
// # The unique user ID is still the lower-cased email
//
// A local account's email is the one entered at sign-up; the OIDC email is the email claim of the
// ID token (email_verified is required by default). Two paths that yield the same email are the
// same Actor: a person who first registered with a password and later switched to the company SSO
// does not need their workspace memberships set up again.
//
// ⚠️ This also means **an IdP that does not verify emails can impersonate anyone** (register an
// IdP account with someone else's email). So email_verified must be true by default; to relax it,
// set auth.oidc.allowUnverifiedEmail explicitly.

package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Default durations of sessions and login flows.
const (
	// defaultSessionTTL is how long one login lasts. 7 days: covers a work week without leaving a
	// session valid for half a year on a forgotten machine.
	defaultSessionTTL = 7 * 24 * time.Hour

	// oidcStateTTL is how long one OIDC login flow (the state row) lives.
	// 10 minutes: enough for "open the IdP login page, enter the password, pass two-factor auth",
	// without keeping state valid for long.
	oidcStateTTL = 10 * time.Minute
)

// APIKey is the config of the fixed API Key channel.
//
// **For callers that are not people**: e2e / CI / scripts / probes have no browser.
// Leave it empty (Key or Email empty) and the whole channel does not exist; that is the
// production default.
type APIKey struct {
	// Key is the fixed secret. **It is a credential**; do not put it in the repository, pass it
	// through an environment variable.
	Key string

	// Email is the identity this key maps to. One key maps to exactly one email.
	Email string

	// Name is the display name; the email prefix if empty.
	Name string
}

// Enabled reports whether this channel is fully configured.
func (k APIKey) Enabled() bool {
	return strings.TrimSpace(k.Key) != "" && strings.TrimSpace(k.Email) != ""
}

// Config is the configuration needed to build a Service.
type Config struct {
	// Store stores sessions, login flows and local accounts. Required.
	Store Store

	// Local is the switch and rate-limit settings of local accounts (email + password). Zero value = disabled.
	Local LocalConfig

	// OIDC is OIDC login, **may be nil** (= disabled). The production implementation is
	// *oidc.Client (shared/infra/oidc).
	//
	// ⚠️ The wiring code must not assign a nil concrete pointer to it: that gives a "non-nil
	// interface value", so s.oidc == nil cannot detect it and the method panics. cmd/amad returns
	// a literal nil explicitly.
	OIDC OIDCClient

	// OIDCDisplayName is the name on the login page button ("Log in with <it>"); "SSO" if empty.
	OIDCDisplayName string

	// OIDCAllowUnverifiedEmail relaxes the email_verified requirement. **Default false**; see the
	// package comment for why.
	OIDCAllowUnverifiedEmail bool

	// GitHub is GitHub device flow login; Client nil means disabled. See github.go.
	GitHub GitHubConfig

	// Admins is the list of platform admin emails (config). It is ORed with is_admin on local
	// accounts: people logging in with OIDC have no local account row, so this is the only way
	// for them to be admins.
	Admins []string

	// APIKey is the fixed API Key channel; disabled if empty.
	APIKey APIKey

	// ManagedKeySecret is the derivation secret of "platform-managed API Keys"; see managedkey.go.
	// **If empty, this feature is entirely unavailable** (the ama command in containers gets no
	// credential); the service still starts.
	ManagedKeySecret string

	// OnLogin is called once **after every successful login**; may be nil.
	//
	// ⚠️ Its failure must never make the login fail: the session has already been issued at this
	// point, so a returned error is only logged.
	OnLogin func(ctx context.Context, email string) error

	// SessionTTL is the session lifetime; 7 days if empty.
	SessionTTL time.Duration

	// Logger defaults to the slog default logger.
	Logger *slog.Logger
}

// Service is the business entry point of this module. Safe to share between goroutines.
type Service struct {
	store      Store
	local      LocalConfig
	apiKey     APIKey
	sessionTTL time.Duration
	log        *slog.Logger

	oidc                OIDCClient
	oidcDisplayName     string
	oidcAllowUnverified bool

	// github nil means GitHub login is not enabled.
	github *githubAuth

	// admins are the platform admins from config, a set of lower-cased emails. Read-only, never
	// changed after construction.
	admins map[string]bool

	// managedKeySecret is the derivation secret of platform-managed API Keys; see managedkey.go.
	managedKeySecret string

	onLogin func(ctx context.Context, email string) error

	// bcryptCost is only lowered by tests (bcrypt.MinCost); otherwise defaultBcryptCost.
	bcryptCost int

	// now is only overridden by tests.
	now func() time.Time
}

// New builds a Service from the config.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("%w: Store 不能为 nil", ErrInvalidArgument)
	}
	ttl := cfg.SessionTTL
	if ttl <= 0 {
		ttl = defaultSessionTTL
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	key := cfg.APIKey
	key.Key = strings.TrimSpace(key.Key)
	key.Email = normalizeEmail(key.Email)
	key.Name = strings.TrimSpace(key.Name)
	if key.Enabled() && key.Name == "" {
		key.Name = emailPrefix(key.Email)
	}
	admins := make(map[string]bool, len(cfg.Admins))
	for _, a := range cfg.Admins {
		if e := normalizeEmail(a); e != "" {
			admins[e] = true
		}
	}
	display := strings.TrimSpace(cfg.OIDCDisplayName)
	if display == "" {
		display = "SSO"
	}

	return &Service{
		store:               cfg.Store,
		local:               cfg.Local.withDefaults(),
		apiKey:              key,
		sessionTTL:          ttl,
		log:                 log,
		oidc:                cfg.OIDC,
		oidcDisplayName:     display,
		oidcAllowUnverified: cfg.OIDCAllowUnverifiedEmail,
		github:              newGitHubAuth(cfg.GitHub),
		admins:              admins,
		managedKeySecret:    strings.TrimSpace(cfg.ManagedKeySecret),
		onLogin:             cfg.OnLogin,
		bcryptCost:          defaultBcryptCost,
		now:                 time.Now,
	}, nil
}

// APIKeyEnabled reports whether the fixed API Key channel is enabled. cmd/amad uses it to decide
// whether to log a warning.
func (s *Service) APIKeyEnabled() bool { return s.apiKey.Enabled() }

// LocalEnabled reports whether local account login is enabled.
func (s *Service) LocalEnabled() bool { return s.local.Enabled }

// OIDCEnabled reports whether OIDC login is enabled.
func (s *Service) OIDCEnabled() bool { return s.oidc != nil }

// SessionTTL returns the session lifetime; the handler uses it for the cookie's Max-Age.
func (s *Service) SessionTTL() time.Duration { return s.sessionTTL }

// Providers is what the login page needs to know about "how this deployment can log in".
type Providers struct {
	Local struct {
		Enabled     bool `json:"enabled"`
		AllowSignup bool `json:"allowSignup"`
		// SignupOpen is "should the sign-up entry be shown right now": sign-up is open, or there
		// are no users at all in the database yet (a fresh deployment, registering the first
		// account is allowed).
		SignupOpen bool `json:"signupOpen"`
	} `json:"local"`
	OIDC struct {
		Enabled     bool   `json:"enabled"`
		DisplayName string `json:"displayName"`
	} `json:"oidc"`
	GitHub struct {
		Enabled     bool   `json:"enabled"`
		DisplayName string `json:"displayName"`
	} `json:"github"`
	APIKeyEnabled bool `json:"apiKeyEnabled"`
}

// Providers reports which login methods are enabled.
//
// SignupOpen queries the database once (HasAnyUser); if that fails it reports "not open". Better
// to make the first person refresh once more than to show a sign-up button that is sure to fail.
// The real decision is made in Signup.
func (s *Service) Providers(ctx context.Context) Providers {
	var p Providers
	p.Local.Enabled = s.local.Enabled
	p.Local.AllowSignup = s.local.Enabled && s.local.AllowSignup
	if p.Local.AllowSignup {
		p.Local.SignupOpen = true
	} else if s.local.Enabled {
		any, err := s.store.HasAnyUser(ctx)
		if err != nil {
			s.log.WarnContext(ctx, "查是否已有用户失败，登录页先不显示注册入口", slog.String("error", err.Error()))
		}
		p.Local.SignupOpen = err == nil && !any
	}
	p.OIDC.Enabled = s.oidc != nil
	if p.OIDC.Enabled {
		p.OIDC.DisplayName = s.oidcDisplayName
	}
	p.GitHub.Enabled = s.github != nil
	if p.GitHub.Enabled {
		p.GitHub.DisplayName = s.github.displayName
	}
	p.APIKeyEnabled = s.apiKey.Enabled()
	return p
}

// ---------------------------------------------------------------------------
// After a successful login (shared by all methods)
// ---------------------------------------------------------------------------

// LoginResult is what a successful login produces.
type LoginResult struct {
	// SessionToken is the **plain token, which exists only at this moment**: the database stores
	// its hash. The handler writes it into the cookie and should forget it right after.
	SessionToken string

	// Identity is the identity settled by this login.
	Identity Identity

	// ReturnTo is the in-site relative path recorded before login; may be empty (then go to the
	// home page). Only OIDC has it.
	ReturnTo string
}

// finishLogin issues the session, writes the profile and calls the hook. The single exit of all
// login methods.
func (s *Service) finishLogin(ctx context.Context, identity Identity, method string) (LoginResult, error) {
	token, err := s.issueSession(ctx, identity)
	if err != nil {
		return LoginResult{}, err
	}

	// Record the profile in the users table. **A failure must not make the login fail**: this is a
	// profile for display, and the session has already been issued. It comes after issueSession:
	// the other way round, if the profile write succeeds but issuing the session fails, the
	// database keeps a profile row of someone who "never really logged in" (and it would also make
	// the "first account" check think the database has users).
	now := s.now()
	if err := s.store.PutUser(ctx, User{
		Email:     identity.Email,
		Name:      identity.Name,
		AvatarURL: identity.AvatarURL,
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		s.log.WarnContext(ctx, "保存用户档案失败", slog.String("error", err.Error()))
	}

	// Post-login hook. **A failure is only logged**. WithoutCancel: the OIDC callback is a
	// top-level browser navigation; closing the tab cancels the original ctx, and following it
	// would show up as "occasionally not finished".
	if s.onLogin != nil {
		if err := s.onLogin(context.WithoutCancel(ctx), identity.Email); err != nil {
			s.log.WarnContext(ctx, "登录后钩子失败（不影响本次登录）",
				slog.String("email", identity.Email), slog.String("error", err.Error()))
		}
	}

	s.log.InfoContext(ctx, "登录成功", slog.String("email", identity.Email), slog.String("method", method))
	return LoginResult{SessionToken: token, Identity: identity}, nil
}

// issueSession issues a new session and returns the **plain token** (it exists only at this moment).
func (s *Service) issueSession(ctx context.Context, identity Identity) (string, error) {
	token, err := newToken()
	if err != nil {
		return "", err
	}
	now := s.now()
	session := Session{
		TokenHash:  hashToken(token),
		Email:      identity.Email,
		Name:       identity.Name,
		AvatarURL:  identity.AvatarURL,
		EmployeeID: identity.EmployeeID,
		CreatedAt:  now,
		ExpiresAt:  now.Add(s.sessionTTL),
	}
	if err := s.store.PutSession(ctx, session); err != nil {
		return "", err
	}
	return token, nil
}

// IsPlatformAdmin reports whether this email is a platform admin: it is on the config list, or
// the local account has is_admin.
func (s *Service) IsPlatformAdmin(ctx context.Context, email string) (bool, error) {
	e := normalizeEmail(email)
	if e == "" {
		return false, nil
	}
	if s.admins[e] {
		return true, nil
	}
	acct, err := s.store.GetLocalAccount(ctx, e)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return acct.Admin, nil
}

// ---------------------------------------------------------------------------
// Checking identities
// ---------------------------------------------------------------------------

// Resolve turns a credential into an identity. The middleware's entry point.
//
// Three channels, tried in order:
//
//  1. Fixed API Key (constant-time compare, no database query): an operator side door for
//     e2e / CI / probes
//  2. User-created API Key (amak_ prefix, looked up in the api_keys table): for external agents /
//     scripts
//  3. Session token (looked up in the auth_sessions table): browsers
//
// The value spaces of the three do not overlap, so the order does not affect correctness, only
// whether a database query is saved.
//
// When no identity can be found it always returns ErrUnauthenticated, **without telling "not
// sent" apart from "sent but wrong"**.
func (s *Service) Resolve(ctx context.Context, credential string) (Identity, error) {
	cred := strings.TrimSpace(credential)
	if cred == "" {
		return Identity{}, ErrUnauthenticated
	}

	if s.apiKey.Enabled() && constantTimeEqual(cred, s.apiKey.Key) {
		return Identity{
			Email:     s.apiKey.Email,
			Name:      s.apiKey.Name,
			ViaAPIKey: true,
		}, nil
	}

	if looksLikeAPIKey(cred) {
		return s.resolveUserAPIKey(ctx, cred)
	}

	session, err := s.store.GetSession(ctx, hashToken(cred))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Identity{}, ErrUnauthenticated
		}
		// A database hiccup is a 500, not a 401: calling it "not logged in" would make users log
		// in again and again.
		return Identity{}, err
	}
	return session.Identity(), nil
}

// resolveUserAPIKey turns a user-created key into an identity and records one use on the way.
//
// **The identity is the creator**: the key inherits the creator's permissions; see "A key is
// bound to a person, not to a workspace" in apikey.go.
func (s *Service) resolveUserAPIKey(ctx context.Context, cred string) (Identity, error) {
	rec, err := s.store.GetAPIKeyByHash(ctx, hashAPIKey(cred))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Identity{}, ErrUnauthenticated
		}
		return Identity{}, err
	}

	// Record one use. **A failure is only logged**: it is a pure statistics field, not worth
	// rejecting a request that would otherwise pass.
	if err := s.store.TouchAPIKey(ctx, rec.KeyHash, s.now()); err != nil {
		s.log.WarnContext(ctx, "记录 API Key 使用时间失败",
			slog.String("keyId", rec.ID), slog.String("error", err.Error()))
	}

	return Identity{
		Email:     rec.Email,
		Name:      emailPrefix(rec.Email),
		ViaAPIKey: true,
	}, nil
}

// Logout ends a session.
//
// Safe to repeat: an unknown token is not an error. **Neither kind of API Key can be logged
// out**; passing one in silently does nothing: the fixed key can only be changed by config, and
// user keys use "revoke". Letting logout delete a key on the way would be dangerous, since that
// key may be in use by CI on another machine.
func (s *Service) Logout(ctx context.Context, credential string) error {
	cred := strings.TrimSpace(credential)
	if cred == "" {
		return nil
	}
	if s.apiKey.Enabled() && constantTimeEqual(cred, s.apiKey.Key) {
		return nil
	}
	if looksLikeAPIKey(cred) {
		return nil
	}
	return s.store.DeleteSession(ctx, hashToken(cred))
}

// LogoutAll kicks out all sessions of a person. Returns how many were deleted.
func (s *Service) LogoutAll(ctx context.Context, email string) (int, error) {
	return s.store.DeleteSessionsByEmail(ctx, email)
}

// emailPrefix returns the part of the email before @, used as the default display name.
func emailPrefix(email string) string {
	if i := strings.Index(email, "@"); i > 0 {
		return email[:i]
	}
	return email
}

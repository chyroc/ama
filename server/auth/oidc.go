// OIDC login: Authorization Code + PKCE + state + nonce.
//
// # Two steps
//
//	StartOIDCLogin     generate state / nonce / PKCE verifier, store them, return the IdP authorization URL
//	CompleteOIDCLogin  consume state (one-time), exchange code + verifier for tokens, verify the ID token
//	                   (signature, iss, aud, exp, nonce), take the email, issue a session
//
// # All three values must live outside the process
//
// state, nonce and verifier are stored in auth_oidc_states. Starting the login on instance A and
// receiving the IdP callback on instance B is normal (the load balancer spreads requests at
// random). With an in-process map, B cannot find them, and the symptom is an occasional "login
// has expired".
//
// # The order must not be swapped
//
// **Consume state first, then exchange the token.** The other way round lets anyone trigger a
// call to the IdP with a forged code; and state is the CSRF gate: if it is not in the database,
// this callback was not started by us.
//
// # How far the email can be trusted
//
// The email is the Actor. By default email_verified=true is required: an IdP that does not verify
// emails lets anyone enter any email, which means they could pose as any member of a workspace.
// To relax this, set auth.oidc.allowUnverifiedEmail explicitly.

package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/chyroc/ama/server/shared/infra/oidc"
)

// Errors of OIDC login.
var (
	// ErrOIDCDisabled means OIDC is not configured. → 404 (or a 302 with unavailable in the callback)
	ErrOIDCDisabled = errors.New("auth: 未启用 OIDC 登录")

	// ErrLoginUnavailable means something is wrong on the IdP side (discovery failed, token
	// exchange got 5xx / network error). → 502
	ErrLoginUnavailable = errors.New("auth: 登录服务暂时不可用")

	// ErrLoginExpired means the credentials of this login can no longer be used: state not found /
	// already used / expired, or the IdP rejected the code, the ID token failed verification, or
	// the nonce does not match. → a 302 with expired in the callback
	//
	// **They are merged into one**: for the user it is the same thing (log in again).
	// Telling them apart would tell an attacker "this state exists, it just expired".
	ErrLoginExpired = errors.New("auth: 本次登录已失效，请重新登录")

	// ErrLoginNoIdentity means the IdP's ID token has no email. → a 302 with no_identity in the callback
	//
	// Usually email is missing from the scopes, or the IdP has no email for this user. **The user
	// cannot fix this alone**, so the message should point them to an administrator. The email is
	// the Actor; if we cannot get it we must never fall back to sub or an empty string.
	ErrLoginNoIdentity = errors.New("auth: 没能从登录凭证里确定你的邮箱")

	// ErrEmailUnverified means the IdP says this email is not verified. → a 302 with
	// email_unverified in the callback
	ErrEmailUnverified = errors.New("auth: 登录服务没有确认你的邮箱已验证")
)

// OIDCClient is the OIDC capability Service depends on; the production implementation is
// *oidc.Client.
//
// **It is an interface for testing**: service-layer unit tests must be able to simulate the
// branches "IdP is down", "code is invalid", "no email". The full real code flow (including
// signature verification) is run end to end by oidc_flow_test.go against a fake IdP started
// with httptest.
type OIDCClient interface {
	AuthCodeURL(ctx context.Context, redirectURI, state, nonce, codeVerifier string) (string, error)
	Exchange(ctx context.Context, redirectURI, code, codeVerifier, nonce string) (oidc.Claims, error)
}

// StartOIDCLogin starts an OIDC login and returns the authorization URL to send the browser to.
//
// redirectURI is our own callback URL (computed by the handler with selfurl.Callback; **do not
// trust request headers**); returnTo is the in-site relative path to return to after a
// successful login (already passed through sanitizeReturnTo); it may be empty.
func (s *Service) StartOIDCLogin(ctx context.Context, redirectURI, returnTo string) (string, error) {
	if s.oidc == nil {
		return "", ErrOIDCDisabled
	}
	if strings.TrimSpace(redirectURI) == "" {
		return "", fmt.Errorf("%w: redirectURI 不能为空", ErrInvalidArgument)
	}
	state, err := newToken()
	if err != nil {
		return "", err
	}
	nonce, err := newToken()
	if err != nil {
		return "", err
	}
	// PKCE verifier: RFC 7636 requires 43 to 128 unreserved characters. 32 bytes in base64url
	// without padding is exactly 43, and the character set is within unreserved.
	verifier, err := newToken()
	if err != nil {
		return "", err
	}

	// **Store first, then get the authorization URL**: the other way round, if storing fails the
	// link has already been handed out; the user finishes at the IdP, comes back, the state is not
	// found, and they see "login has expired" although they did nothing wrong.
	now := s.now()
	if err := s.store.PutOIDCState(ctx, OIDCState{
		State:        state,
		Nonce:        nonce,
		CodeVerifier: verifier,
		ReturnTo:     returnTo,
		CreatedAt:    now,
		ExpiresAt:    now.Add(oidcStateTTL),
	}); err != nil {
		return "", err
	}

	url, err := s.oidc.AuthCodeURL(ctx, redirectURI, state, nonce, verifier)
	if err != nil {
		s.log.ErrorContext(ctx, "取 OIDC 授权地址失败", slog.String("error", err.Error()))
		return "", fmt.Errorf("%w: %v", ErrLoginUnavailable, err)
	}
	return url, nil
}

// CompleteOIDCLogin finishes the login with the state and code from the callback. redirectURI
// must be the same as when the login was started.
func (s *Service) CompleteOIDCLogin(ctx context.Context, redirectURI, state, code string) (LoginResult, error) {
	if s.oidc == nil {
		return LoginResult{}, ErrOIDCDisabled
	}
	if strings.TrimSpace(state) == "" || strings.TrimSpace(code) == "" {
		return LoginResult{}, fmt.Errorf("%w: state 与 code 都不能为空", ErrInvalidArgument)
	}

	st, err := s.store.ConsumeOIDCState(ctx, state)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return LoginResult{}, ErrLoginExpired
		}
		return LoginResult{}, err
	}

	claims, err := s.oidc.Exchange(ctx, redirectURI, code, st.CodeVerifier, st.Nonce)
	if err != nil {
		if errors.Is(err, oidc.ErrRejected) {
			// Log a Warn with the reason: config problems such as "wrong aud" or "wrong client
			// secret" also land here. The user sees "log in again"; operators need this log line
			// to find the real cause.
			s.log.WarnContext(ctx, "OIDC 登录被拒绝", slog.String("error", err.Error()))
			return LoginResult{}, ErrLoginExpired
		}
		s.log.ErrorContext(ctx, "OIDC 换 token 失败", slog.String("error", err.Error()))
		return LoginResult{}, fmt.Errorf("%w: %v", ErrLoginUnavailable, err)
	}

	email := normalizeEmail(claims.Email)
	if email == "" || !strings.Contains(email, "@") {
		s.log.ErrorContext(ctx, "OIDC 的 ID token 里没有可用的邮箱（scopes 里有 email 吗）",
			slog.String("sub", claims.Subject))
		return LoginResult{}, ErrLoginNoIdentity
	}
	if !claims.EmailVerified && !s.oidcAllowUnverified {
		s.log.WarnContext(ctx, "OIDC 邮箱未验证，拒绝登录（要放宽配 auth.oidc.allowUnverifiedEmail）",
			slog.String("email", email))
		return LoginResult{}, ErrEmailUnverified
	}

	name := claims.Name
	if name == "" {
		name = emailPrefix(email)
	}
	res, err := s.finishLogin(ctx, Identity{Email: email, Name: name, AvatarURL: claims.Picture}, "oidc")
	if err != nil {
		return LoginResult{}, err
	}
	res.ReturnTo = st.ReturnTo
	return res, nil
}

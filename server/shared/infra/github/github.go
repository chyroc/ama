// Package github is a client for the GitHub OAuth device flow (RFC 8628) and a few read-only user
// endpoints.
//
// # Standard library only
//
// Four HTTP calls in total (request device code, exchange token, get user, get emails) plus an
// optional org membership check. Their shapes are all simple, not worth pulling in an SDK.
//
// # Why no client secret is needed
//
// Once "Enable Device Flow" is turned on for a GitHub OAuth App, both steps of the device flow only
// need the client_id (GitHub's docs say plainly that device flow does not use a secret: it is meant
// for devices like CLIs / TVs that cannot keep a secret). So the deployer only configures a public
// client_id; when it is not set, DefaultClientID (the GitHub CLI's) is used, with the costs
// described in that constant's comment.
//
// # The proxy only applies to this client
//
// Config.Proxy is only installed into this package's own http.Transport and **does not touch the
// process-wide HTTPS_PROXY**: that would route other outbound traffic such as Ark and object
// storage through the proxy too. When Proxy is empty, the standard library's ProxyFromEnvironment
// is used (same as other outbound traffic).
//
// # The access token is not stored here
//
// This package only "gets the token and uses it to ask a few questions"; it does not keep it. The
// caller (auth) discards it after use; see auth/github.go for why.
package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Default addresses. Tests point Config.WebURL / APIURL at an httptest server.
const (
	DefaultWebURL = "https://github.com"
	DefaultAPIURL = "https://api.github.com"
)

// DefaultClientID is the default used when no client id is configured: the public OAuth App client
// id of GitHub CLI (see oauthClientID in internal/authflow/flow.go in the github.com/cli/cli
// source).
//
// It can be used directly because device flow only needs the client id and **does not need a client
// secret**, and the client id is public anyway. So GitHub login works out of the box, without
// creating an OAuth App first.
//
// ⚠️ The costs; for real deployments, switch to your own OAuth App (auth.github.clientId):
//   - On the GitHub authorization page users see "GitHub CLI" asking for access, not your platform.
//     That is misleading, and it also makes device code phishing more convincing;
//   - We do not control this App; its owner may revoke it or change settings at any time, and then
//     GitHub login stops working entirely;
//   - The user's GitHub "Authorized OAuth Apps" will record it as an authorization for GitHub CLI.
const DefaultClientID = "178c6fc778ccc68e1d6a"

// DefaultScopes are the default requested scopes: read profile + read emails (including private
// emails and the verified flag).
var DefaultScopes = []string{"read:user", "user:email"}

// maxBody is the limit for reading GitHub response bodies. Normal responses are a few KB; 1MB
// guards against huge payloads.
const maxBody = 1 << 20

// Error codes GitHub returns when exchanging a token in device flow (RFC 8628 3.5 + GitHub docs).
const (
	ErrCodeAuthorizationPending = "authorization_pending"
	ErrCodeSlowDown             = "slow_down"
	ErrCodeExpiredToken         = "expired_token"
	ErrCodeAccessDenied         = "access_denied"
)

// ErrUnavailable means something is wrong on the GitHub side: network down, 5xx, an unrecognized
// response, or a config problem (wrong client_id, OAuth App without device flow enabled). None of
// these can be fixed by the user.
var ErrUnavailable = errors.New("github: 服务暂时不可用")

// Config is the client configuration.
type Config struct {
	// ClientID is the GitHub OAuth App client id; empty uses DefaultClientID.
	ClientID string

	// Scopes defaults to DefaultScopes when empty.
	Scopes []string

	// Proxy is an HTTP proxy used only by this client (like http://127.0.0.1:8080). Empty uses
	// environment variables.
	Proxy string

	// WebURL / APIURL default to github.com / api.github.com when empty. Only tests change them.
	WebURL string
	APIURL string

	// Timeout is the per-request timeout; empty means 15 seconds.
	Timeout time.Duration
}

// Client is a GitHub client. It is safe for concurrent use.
type Client struct {
	clientID string
	scopes   []string
	webURL   string
	apiURL   string
	http     *http.Client
}

// New creates a client. It only validates the config and sends no requests.
func New(cfg Config) (*Client, error) {
	id := strings.TrimSpace(cfg.ClientID)
	if id == "" {
		id = DefaultClientID
	}
	scopes := cleanScopes(cfg.Scopes)
	if len(scopes) == 0 {
		scopes = append([]string(nil), DefaultScopes...)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if p := strings.TrimSpace(cfg.Proxy); p != "" {
		u, err := ParseProxy(p)
		if err != nil {
			return nil, err
		}
		transport.Proxy = http.ProxyURL(u)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &Client{
		clientID: id,
		scopes:   scopes,
		webURL:   trimURL(cfg.WebURL, DefaultWebURL),
		apiURL:   trimURL(cfg.APIURL, DefaultAPIURL),
		http:     &http.Client{Transport: transport, Timeout: timeout},
	}, nil
}

// ParseProxy validates a proxy address: it must have an http / https / socks5 scheme and a host.
//
// Exported so the config package can validate it at load time. A wrong proxy (missing `http://`)
// shows up as "GitHub login never works", with an error that does not point at the proxy, so it
// must be caught at startup.
func ParseProxy(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("github: proxy %q 不是合法的 URL（要带 scheme，如 http://127.0.0.1:8080）", raw)
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("github: proxy 的 scheme 只能是 http / https / socks5，实际 %q", u.Scheme)
	}
	return u, nil
}

// ClientID returns the client id actually used (DefaultClientID when not configured).
func (c *Client) ClientID() string { return c.clientID }

// Scopes returns the scopes actually requested (deduplicated, empty ones removed).
func (c *Client) Scopes() []string { return append([]string(nil), c.scopes...) }

// DeviceCode is the result of requesting a device code.
type DeviceCode struct {
	// DeviceCode is a **secret**: whoever holds it can exchange it for a token once the user
	// authorizes. It must stay on the server.
	DeviceCode string
	// UserCode is the short code shown to the person, to be typed on the GitHub page, like
	// ABCD-1234.
	UserCode        string
	VerificationURI string
	ExpiresIn       time.Duration
	Interval        time.Duration
}

// RequestDeviceCode asks GitHub for a device code / user code pair.
func (c *Client) RequestDeviceCode(ctx context.Context) (DeviceCode, error) {
	form := url.Values{"client_id": {c.clientID}, "scope": {strings.Join(c.scopes, " ")}}
	var out struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
		oauthError
	}
	if err := c.postForm(ctx, c.webURL+"/login/device/code", form, &out); err != nil {
		return DeviceCode{}, err
	}
	if out.Error != "" {
		// device_flow_disabled / unauthorized_client / incorrect_client_credentials：
		// All of these are deployment config problems.
		return DeviceCode{}, fmt.Errorf("%w: 申请 device code 被拒（%s: %s）", ErrUnavailable, out.Error, out.ErrorDescription)
	}
	if out.DeviceCode == "" || out.UserCode == "" || out.VerificationURI == "" {
		return DeviceCode{}, fmt.Errorf("%w: device code 响应缺字段", ErrUnavailable)
	}
	if out.Interval <= 0 {
		out.Interval = 5 // RFC 8628 3.2: defaults to 5 seconds
	}
	if out.ExpiresIn <= 0 {
		out.ExpiresIn = 900
	}
	return DeviceCode{
		DeviceCode:      out.DeviceCode,
		UserCode:        out.UserCode,
		VerificationURI: out.VerificationURI,
		ExpiresIn:       time.Duration(out.ExpiresIn) * time.Second,
		Interval:        time.Duration(out.Interval) * time.Second,
	}, nil
}

// TokenResult is the result of one token exchange: either AccessToken is non-empty or Error is
// non-empty.
type TokenResult struct {
	AccessToken string
	// Error is the error code GitHub returned (authorization_pending / slow_down / ...).
	Error string
	// Interval is the new interval GitHub gives with slow_down (may be 0, meaning none given).
	Interval time.Duration
}

// PollToken exchanges the device code for a token once.
//
// "Still waiting for the user", "slow down", "expired" and "denied" are all **normal results**, so
// they go in TokenResult.Error, not in error; error only means GitHub is unavailable or returned an
// unrecognized error code.
func (c *Client) PollToken(ctx context.Context, deviceCode string) (TokenResult, error) {
	form := url.Values{
		"client_id":   {c.clientID},
		"device_code": {deviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}
	var out struct {
		AccessToken string `json:"access_token"`
		Interval    int    `json:"interval"`
		oauthError
	}
	if err := c.postForm(ctx, c.webURL+"/login/oauth/access_token", form, &out); err != nil {
		return TokenResult{}, err
	}
	switch out.Error {
	case "":
		if out.AccessToken == "" {
			return TokenResult{}, fmt.Errorf("%w: token 响应里既没有 access_token 也没有 error", ErrUnavailable)
		}
		return TokenResult{AccessToken: out.AccessToken}, nil
	case ErrCodeAuthorizationPending, ErrCodeSlowDown, ErrCodeExpiredToken, ErrCodeAccessDenied:
		return TokenResult{Error: out.Error, Interval: time.Duration(out.Interval) * time.Second}, nil
	default:
		// incorrect_device_code / unsupported_grant_type / incorrect_client_credentials /
		// device_flow_disabled: a config or protocol problem; retrying will not help the user.
		return TokenResult{}, fmt.Errorf("%w: 换 token 被拒（%s: %s）", ErrUnavailable, out.Error, out.ErrorDescription)
	}
}

// User holds the fields of GET /user that we use.
type User struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url"`
}

// Email is one item of GET /user/emails.
type Email struct {
	Email    string `json:"email"`
	Primary  bool   `json:"primary"`
	Verified bool   `json:"verified"`
}

// GetUser fetches the user for the current token.
func (c *Client) GetUser(ctx context.Context, token string) (User, error) {
	var u User
	if _, err := c.getAPI(ctx, token, "/user", &u); err != nil {
		return User{}, err
	}
	if u.Login == "" {
		return User{}, fmt.Errorf("%w: /user 响应里没有 login", ErrUnavailable)
	}
	return u, nil
}

// ListEmails fetches all emails of the current user (needs the user:email scope).
func (c *Client) ListEmails(ctx context.Context, token string) ([]Email, error) {
	var out []Email
	if _, err := c.getAPI(ctx, token, "/user/emails?per_page=100", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// IsOrgMember reports whether the current user is an **active** member of org (needs the read:org
// scope).
//
// It uses GET /user/memberships/orgs/{org} instead of listing /user/orgs: the latter needs paging
// and only lists orgs "this OAuth App is allowed to see". The result is the same, and this call is
// more direct.
// 404 / 403 = not a member (or the org restricts third-party app access); not treated as an error.
func (c *Client) IsOrgMember(ctx context.Context, token, org string) (bool, error) {
	var m struct {
		State string `json:"state"`
	}
	status, err := c.getAPI(ctx, token, "/user/memberships/orgs/"+url.PathEscape(org), &m)
	if status == http.StatusNotFound || status == http.StatusForbidden {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return m.State == "active", nil
}

// ---------------------------------------------------------------------------

type oauthError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// postForm sends a form POST and decodes the JSON response into dst.
//
// ⚠️ It must send `Accept: application/json`: without it GitHub answers with
// application/x-www-form-urlencoded, JSON decoding fails, and it shows up as "GitHub unavailable".
func (c *Client) postForm(ctx context.Context, endpoint string, form url.Values, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ama")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("%w: 读响应失败: %v", ErrUnavailable, err)
	}
	// GitHub often returns 200 for OAuth errors too (the error is in the body), so try to decode
	// first, then look at the status code.
	if err := json.Unmarshal(body, dst); err != nil || resp.StatusCode >= 500 {
		return fmt.Errorf("%w: %s 回了 HTTP %d", ErrUnavailable, endpoint, resp.StatusCode)
	}
	return nil
}

// getAPI calls a REST API with the token and returns the status code (error is non-nil for
// non-2xx).
func (c *Client) getAPI(ctx context.Context, token, path string, dst any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiURL+path, nil)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "ama")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("%w: 读响应失败: %v", ErrUnavailable, err)
	}
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, fmt.Errorf("%w: GET %s 回了 HTTP %d", ErrUnavailable, path, resp.StatusCode)
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return resp.StatusCode, fmt.Errorf("%w: 解析 %s 响应失败: %v", ErrUnavailable, path, err)
	}
	return resp.StatusCode, nil
}

func cleanScopes(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
			if !seen[part] {
				seen[part] = true
				out = append(out, part)
			}
		}
	}
	return out
}

func trimURL(v, def string) string {
	v = strings.TrimRight(strings.TrimSpace(v), "/")
	if v == "" {
		return def
	}
	return v
}

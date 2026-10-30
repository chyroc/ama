// Package oidc is the access layer for generic OpenID Connect login: Authorization Code +
// PKCE + nonce.
//
// It does only the two things that need io. Everything else (storing state, issuing the
// session, deciding whether the email can be trusted) lives in the auth package:
//
//	AuthCodeURL  builds the authorization URL that sends the browser to the IdP (with state,
//	             nonce and the PKCE S256 digest)
//	Exchange     trades the code from the callback + the PKCE original for a token,
//	             **verifies the ID token** (signature, iss, aud, exp, nonce), and returns the
//	             identity claims in it
//
// Under the hood it uses github.com/coreos/go-oidc/v3 + golang.org/x/oauth2. No hand-written
// JWT verification.
//
// # discovery is lazy, and failures are not cached
//
// go-oidc's NewProvider fetches `<issuer>/.well-known/openid-configuration`. If we did this at
// startup, a short IdP outage would keep **the whole service from starting** (even the fixed
// API Key and local accounts would stop working), and the error would point at "cannot reach
// some URL", not at "this is the login config". So we fetch it the first time it is really
// needed. On success it is cached in the process (pure cache: losing it only means fetching
// again next time). On failure it is **not cached**, and the next call tries again.
//
// # Caching in the process is allowed
//
// What we cache is the IdP's public metadata and JWKS (go-oidc's RemoteKeySet refreshes keys
// by kid on its own). Each instance fetches its own copy and they do not affect each other.
// This fits the rule "only keep things in the process that can be lost at any time, where
// losing them only makes things slower".
package oidc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// ErrRejected means the IdP or the ID token check **rejected this login**: the code is expired
// or replayed (invalid_grant), the ID token signature or aud is wrong, or the nonce does not
// match. For the user all of these mean "log in again". Keep them apart from "cannot reach
// the IdP" (all other errors), which is an outage.
var ErrRejected = errors.New("oidc: 登录凭证被拒绝")

// Config is the configuration of one OIDC client.
type Config struct {
	// Issuer is the IdP's issuer URL, like https://accounts.google.com.
	// The iss of the ID token must equal it exactly (checked by go-oidc).
	Issuer string

	// ClientID / ClientSecret are the client registered at the IdP. ClientSecret may be empty
	// (public client, relying only on PKCE).
	ClientID     string
	ClientSecret string

	// Scopes defaults to openid email profile when empty. **openid is always added**: without
	// it the IdP does not issue an ID token.
	Scopes []string

	// HTTPClient defaults to a client with a 15 second timeout when empty. Tests inject the
	// httptest one.
	HTTPClient *http.Client
}

// Claims are the identity fields taken from the ID token.
type Claims struct {
	// Subject is the stable user id on the IdP side (sub). Only used for logs; the AMA Actor
	// is still the email.
	Subject string

	// Email is returned as is (not lowercased). The caller normalizes it.
	Email string

	// EmailVerified is the email_verified claim. Missing means false.
	EmailVerified bool

	Name    string
	Picture string
}

// Client is an OIDC login client. Safe for concurrent use.
type Client struct {
	cfg    Config
	scopes []string
	http   *http.Client

	mu       sync.Mutex
	provider *gooidc.Provider
}

// New validates the config and builds a Client. **It makes no network request** (discovery is
// lazy, see the package comment).
func New(cfg Config) (*Client, error) {
	cfg.Issuer = strings.TrimSpace(cfg.Issuer)
	cfg.ClientID = strings.TrimSpace(cfg.ClientID)
	if cfg.Issuer == "" || cfg.ClientID == "" {
		return nil, fmt.Errorf("oidc: issuer 与 clientId 都必须配置")
	}
	if !strings.HasPrefix(cfg.Issuer, "https://") && !strings.HasPrefix(cfg.Issuer, "http://") {
		return nil, fmt.Errorf("oidc: issuer 必须是 http(s) URL，实际 %q", cfg.Issuer)
	}
	scopes := []string{gooidc.ScopeOpenID}
	seen := map[string]bool{gooidc.ScopeOpenID: true}
	in := cfg.Scopes
	if len(in) == 0 {
		in = []string{"email", "profile"}
	}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			scopes = append(scopes, s)
		}
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{cfg: cfg, scopes: scopes, http: hc}, nil
}

// getProvider fetches the discovery document lazily. It caches only on success; on failure
// the next call retries.
func (c *Client) getProvider(ctx context.Context) (*gooidc.Provider, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.provider != nil {
		return c.provider, nil
	}
	// ⚠️ Use WithoutCancel: the RemoteKeySet inside the provider **remembers this ctx** and uses
	// it to refresh the JWKS later. If we passed the request ctx, it would be canceled when that
	// request ends, and every later key refresh would fail. The symptom is "after a while nobody
	// can log in". The client still has a timeout, so it will not hang.
	p, err := gooidc.NewProvider(gooidc.ClientContext(context.WithoutCancel(ctx), c.http), c.cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc: 取 IdP 的 discovery 文档失败（%s）: %w", c.cfg.Issuer, err)
	}
	c.provider = p
	return p, nil
}

func (c *Client) oauth2Config(p *gooidc.Provider, redirectURI string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     c.cfg.ClientID,
		ClientSecret: c.cfg.ClientSecret,
		Endpoint:     p.Endpoint(),
		RedirectURL:  redirectURI,
		Scopes:       c.scopes,
	}
}

// AuthCodeURL builds the authorization URL. codeVerifier appears in the URL only as its S256
// digest.
func (c *Client) AuthCodeURL(ctx context.Context, redirectURI, state, nonce, codeVerifier string) (string, error) {
	p, err := c.getProvider(ctx)
	if err != nil {
		return "", err
	}
	return c.oauth2Config(p, redirectURI).AuthCodeURL(state,
		gooidc.Nonce(nonce),
		oauth2.S256ChallengeOption(codeVerifier),
	), nil
}

// Exchange trades the code for a token, verifies the ID token, and returns the identity claims.
//
// redirectURI must be exactly the same as the one used to start the login (the IdP compares
// them). nonce is the one written into the authorization request; if the nonce in the ID
// token differs, the result is ErrRejected.
func (c *Client) Exchange(ctx context.Context, redirectURI, code, codeVerifier, nonce string) (Claims, error) {
	p, err := c.getProvider(ctx)
	if err != nil {
		return Claims{}, err
	}
	ctx = gooidc.ClientContext(ctx, c.http)
	tok, err := c.oauth2Config(p, redirectURI).Exchange(ctx, code, oauth2.VerifierOption(codeVerifier))
	if err != nil {
		var re *oauth2.RetrieveError
		// 4xx means the IdP clearly rejected it (code expired, replayed, wrong verifier,
		// wrong client credentials). Only 5xx and network errors are outages.
		if errors.As(err, &re) && re.Response != nil && re.Response.StatusCode >= 400 && re.Response.StatusCode < 500 {
			return Claims{}, fmt.Errorf("%w: 换 token 被拒（%s）: %v", ErrRejected, re.ErrorCode, err)
		}
		return Claims{}, fmt.Errorf("oidc: 换 token 失败: %w", err)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return Claims{}, fmt.Errorf("%w: token 响应里没有 id_token（scopes 里有 openid 吗）", ErrRejected)
	}
	idt, err := p.Verifier(&gooidc.Config{ClientID: c.cfg.ClientID}).Verify(ctx, raw)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: ID token 校验失败: %v", ErrRejected, err)
	}
	// The caller compares the nonce instead of go-oidc: its Config has no nonce option, it only
	// exposes the value on IDToken.
	if nonce == "" || idt.Nonce != nonce {
		return Claims{}, fmt.Errorf("%w: ID token 的 nonce 与本次登录不符", ErrRejected)
	}
	var cl struct {
		Email         string `json:"email"`
		EmailVerified any    `json:"email_verified"`
		Name          string `json:"name"`
		Picture       string `json:"picture"`
	}
	if err := idt.Claims(&cl); err != nil {
		return Claims{}, fmt.Errorf("%w: 解析 ID token claims 失败: %v", ErrRejected, err)
	}
	return Claims{
		Subject:       idt.Subject,
		Email:         strings.TrimSpace(cl.Email),
		EmailVerified: truthy(cl.EmailVerified),
		Name:          strings.TrimSpace(cl.Name),
		Picture:       strings.TrimSpace(cl.Picture),
	}, nil
}

// truthy accepts the two forms of email_verified: JSON true, or the string "true" (some IdPs
// do this, for example early Cognito). Everything else is false: **when unsure, treat it as
// not verified**.
func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return strings.EqualFold(strings.TrimSpace(x), "true")
	}
	return false
}

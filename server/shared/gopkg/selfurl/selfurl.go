// Package selfurl answers one question: **what is this service's public address?**
//
// Two places need it, with different requirements:
//
//   - the llms.txt / spec links in `openapi`: these want "the origin the caller itself sees",
//     so links point to local when developing locally and to production in production, and the
//     agent is never sent to the wrong environment.
//   - the OAuth `redirect_uri` in `auth`: **this one must not trust the request**, see below.
//
// # ⚠️ Deriving the origin from the request has an attack surface; do not use FromRequest directly in security-sensitive places
//
// Both `Host` and `X-Forwarded-Host` **can be changed by the caller**. If you build the OAuth
// redirect_uri from them, an attacker can use `Host: evil.com` to get an authorization link and
// trick a victim into clicking through; the one-time code then lands on evil.com and he exchanges
// it for a token. **This is account takeover**, and it happens with zero errors.
//
// So there are two separate functions, and the names say whether they can be trusted:
//
//	FromRequest(r)              // the origin the caller claims. **Not trusted**; only for display/doc links
//	Callback(publicURL, r, path) // trusted: prefers config; fallback only allowed for local requests
//
// Using the wrong domain in a doc link costs "the link points to the wrong environment" (argued on
// the openapi side, acceptable); using the wrong domain in redirect_uri costs account takeover.
// The two cannot share one function.
package selfurl

import (
	"errors"
	"net"
	"net/http"
	"strings"
)

// ErrUnknown means there is no config and the request is not local, so no trusted self address can be derived.
var ErrUnknown = errors.New("selfurl: 推不出服务自身的对外地址")

// FromRequest builds the origin **the caller itself sees**, such as `https://ama.example.com`.
//
// ⚠️ **The return value is not trusted** (it comes from Host / X-Forwarded-Host, which the caller
// can change). Only use it for display links in docs and similar cases where "pointing wrong only
// means pointing at the wrong environment". Use Callback in security-sensitive places.
//
// # Why not hard-code the production domain, or use a config item
//
// Hard-coding `https://ama.example.com` would make **that link point to production during local
// development**: the agent reads the local docs but calls the production API, with zero errors.
// The responses it gets look completely normal; it just acts on the wrong environment. The same
// goes for changing the domain or starting a staging environment.
//
// When Host is missing it returns an empty string, and links in the text fall back to same-origin
// relative paths. That is the last fallback, not the normal case, and a relative path at least
// never points to the wrong environment.
func FromRequest(r *http.Request) string {
	host := hostOf(r)
	if host == "" {
		return ""
	}
	return Scheme(r, host) + "://" + host
}

// Callback builds a **trusted** callback address, such as `https://ama.example.com/api/auth/oidc/callback`.
//
// path must start with `/`. Order of preference:
//
//  1. publicURL is not empty -> use it. This is the only correct source in production, and it is
//     **not affected by the request**.
//  2. publicURL is empty and the request comes from the local machine -> derive it from the
//     request. An attacker who sets a loopback Host gets nothing (the code is sent to his own
//     machine), so this gap is safe; in return, local development and worktrees (each on a
//     different port) work without configuring publicURL.
//  3. Neither holds -> ErrUnknown. **Better to fail here than to build a wrong redirect_uri**:
//     that error happens at the callback step after the user clicks consent at the IdP (the IdP
//     reports redirect_uri mismatch), and it does not point at the config.
func Callback(publicURL string, r *http.Request, path string) (string, error) {
	if base := strings.TrimRight(strings.TrimSpace(publicURL), "/"); base != "" {
		return base + path, nil
	}
	host := hostOf(r)
	if host == "" || !IsLoopback(host) {
		return "", ErrUnknown
	}
	return Scheme(r, host) + "://" + host + path, nil
}

// hostOf gets the host the caller sees (may include a port).
func hostOf(r *http.Request) string {
	if r == nil {
		return ""
	}
	host := r.Host
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
		// It may be a comma-separated list; take the first one (what the original client saw).
		host = strings.TrimSpace(strings.Split(fwd, ",")[0])
	}
	return host
}

// Scheme decides whether the caller used http or https.
//
// In production TLS terminates at the gateway and the process sees plain HTTP, so **do not look at
// r.TLS**: production would then build `http://`, and a caller using it would get a 301 (it works,
// but adds a hop, and some clients that do not follow redirects fail outright). The signal is
// X-Forwarded-Proto, which the gateway sets.
func Scheme(r *http.Request, host string) string {
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		return strings.TrimSpace(strings.Split(proto, ",")[0])
	}
	if r.TLS != nil {
		return "https"
	}
	// Fallback when the gateway did not set X-Forwarded-Proto. **Default to https**: in
	// production the hop to the process is always plain, so guessing plain would make the
	// production docs emit http. Only local addresses get http.
	if IsLoopback(host) {
		return "http"
	}
	return "https"
}

// IsLoopback reports whether host is a local/private address (may include a port).
func IsLoopback(host string) bool {
	h := host
	if name, _, err := net.SplitHostPort(host); err == nil {
		h = name
	}
	h = strings.Trim(h, "[]")
	switch h {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0":
		return true
	}
	// Dev machines often connect directly by private IP too (`http://10.x.x.x:5273`).
	// Only explicit private prefixes are accepted here; do not guess from shapes like "has no dot".
	return strings.HasPrefix(h, "127.") || strings.HasPrefix(h, "192.168.") ||
		strings.HasPrefix(h, "10.")
}

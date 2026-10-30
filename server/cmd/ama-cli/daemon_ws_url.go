// Deriving the WebSocket entry address and parsing the transport preference.
//
// By default it is derived from the HTTP BaseURL (same host, http→ws / https→wss). When the gateway
// in front only speaks HTTP/2 and swallows the Upgrade header, the WS handshake fails (see the
// header of server/httpd/wsprobe.go); then point AMA_WS_BASE_URL at an entry that speaks HTTP/1.1;
// when it cannot connect, the supervisor falls back to HTTP.
package main

import (
	"net/url"
	"os"
	"strings"
)

const (
	envWSBaseURL       = "AMA_WS_BASE_URL"
	envDaemonTransport = "AMA_DAEMON_TRANSPORT"
	wsPath             = "/api/host-daemon/ws"
)

// transportPreference parses the transport preference: environment variable > host.json > default
// auto. Invalid values are treated as auto and shown in the caller's log (no logging here, to keep it
// a pure function that is easy to test).
func (rt *daemonRuntime) transportPreference() string {
	if v := strings.TrimSpace(os.Getenv(envDaemonTransport)); v != "" {
		return normalizeTransport(v)
	}
	return normalizeTransport(rt.cfg.Transport)
}

func normalizeTransport(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case transportModeHTTP:
		return transportModeHTTP
	case transportModeWS:
		return transportModeWS
	default:
		return transportModeAuto
	}
}

// wsURL returns the full WS URL to use this time: explicit config (env > host.json) first, otherwise
// derived from the HTTP BaseURL.
func (rt *daemonRuntime) wsURL() string {
	base := strings.TrimSpace(os.Getenv(envWSBaseURL))
	if base == "" {
		base = strings.TrimSpace(rt.cfg.WSBaseURL)
	}
	if base == "" {
		return deriveWSBaseURL(rt.baseURL)
	}
	return joinWSURL(normalizeWSOrigin(base))
}

// deriveWSBaseURL derives the WS address from the HTTP BaseURL:
//
//	host unchanged, scheme http→ws / https→wss, with /api/host-daemon/ws appended.
func deriveWSBaseURL(httpBase string) string {
	u, err := url.Parse(normalizeBaseURL(httpBase))
	if err != nil || u.Host == "" {
		// When it cannot be parsed, give the most likely shape as is; a dial failure is classified and falls back, not fatal.
		return "wss://" + strings.TrimRight(strings.TrimPrefix(normalizeBaseURL(httpBase), "https://"), "/") + wsPath
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		u.Scheme = "wss"
	}
	return joinWSURL(u.String())
}

// normalizeWSOrigin tidies up a WS origin given explicitly by the user: strip the trailing slash, add a scheme.
func normalizeWSOrigin(raw string) string {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	switch {
	case strings.HasPrefix(raw, "ws://"), strings.HasPrefix(raw, "wss://"):
		return raw
	case strings.HasPrefix(raw, "http://"):
		return "ws" + strings.TrimPrefix(raw, "http")
	case strings.HasPrefix(raw, "https://"):
		return "wss" + strings.TrimPrefix(raw, "https")
	default:
		return "wss://" + raw
	}
}

func joinWSURL(origin string) string {
	return strings.TrimRight(origin, "/") + wsPath
}

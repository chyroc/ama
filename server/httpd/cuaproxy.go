// CUA cloud desktop reverse proxy (`/desktops/` and `/cua-app/`).
//
// # Why a same-origin proxy is required
//
// When AMA Web is accessed over an IP or a different domain (e.g. `http://ama.example.com:9999`)
// and embeds the Volcengine Ark CUA web client (`https://cua.ark.cn-beijing.volces.com/desktops/<id>/cua-app/?ticket=dt_...`)
// directly inside an `<iframe>`, browser security and transport constraints break the embedded desktop:
//
//  1. **Third-party cookie blocking (`401 MissingTicket`)**: CUA redeems `?ticket=dt_...` via a
//     `303 See Other` redirect to `/desktops/<id>/cua-app/` and sets `Set-Cookie: cua_desktop_ticket=dt_...; SameSite=Lax`.
//     Inside a cross-site `<iframe>`, modern browsers refuse to store or send a `SameSite=Lax`
//     cookie, so the redirected request and subsequent `/api/session` / WebSocket calls fail with `401 MissingTicket`.
//  2. **`crypto.randomUUID` on plain HTTP**: CUA's RustDesk web viewer (`viewer.js`) calls
//     `crypto.randomUUID()`, which browsers only expose in Secure Contexts (`https://` or `localhost`).
//  3. **8-second `viewer-lease` expiration over HTTP/1.1 (`409 viewer_lease_expired`)**:
//     Upstream `GET /relay/rustdesk/<id>/session` takes 5–7 seconds to prepare the RustDesk relay
//     session while enforcing an 8-second `viewer-lease` TTL (`ttlMs: 8000`). Over plain HTTP/1.1,
//     browser connection pool contention (6 connections per origin shared with SSE streams and
//     `viewer.js` downloads) delays the browser's 2-second `POST /viewer-lease` heartbeats, causing
//     `GET /session` to fail with `409 viewer_lease_expired` and triggering infinite iframe reloads.
//
// Proxying `/desktops/` and `/cua-app/` under AMA's own origin makes `cua_desktop_ticket` a
// first-party cookie, maintains a server-side 1.5-second `viewer-lease` keepalive loop while the
// desktop viewer is active, caches static JS/CSS/WASM assets, tunnels WebSockets transparently, and
// injects a `crypto.randomUUID` + standalone viewer lease bootstrap when serving HTML over HTTP.
package httpd

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// PathCUAProxyPrefix proxies `/desktops/{desktopId}/...` (HTML shell, `/api/*`, `/ws/rtc`, `/cua-app/ws/events`).
	PathCUAProxyPrefix = "/desktops/"
	// PathCUAAppPrefix proxies `/cua-app/...` static assets (`app.js`, `viewer.js`, CSS, fonts, WASM).
	PathCUAAppPrefix = "/cua-app/"

	defaultCUAUpstream = "https://cua.ark.cn-beijing.volces.com"
)

type cuaTicketEntry struct {
	ticket    string
	desktopID string
	expiresAt time.Time
}

type cuaTicketManager struct {
	mu         sync.Mutex
	entries    map[string]cuaTicketEntry
	apiKey     string
	gatewayURL string
	httpClient *http.Client
	log        *slog.Logger
}

func newCUATicketManager(log *slog.Logger, gatewayURL, apiKey string) *cuaTicketManager {
	return &cuaTicketManager{
		entries:    make(map[string]cuaTicketEntry),
		apiKey:     strings.TrimSpace(apiKey),
		gatewayURL: strings.TrimRight(strings.TrimSpace(gatewayURL), "/"),
		httpClient: &http.Client{Timeout: 15 * time.Second},
		log:        log,
	}
}

func (m *cuaTicketManager) getOrMint(ctx context.Context, desktopID string, forceRefresh bool) (string, string) {
	if desktopID == "" || m.gatewayURL == "" || m.apiKey == "" {
		return "", ""
	}
	now := time.Now()
	m.mu.Lock()
	for k, e := range m.entries {
		if !now.Before(e.expiresAt) {
			delete(m.entries, k)
		}
	}
	if !forceRefresh {
		if entry, ok := m.entries[desktopID]; ok && entry.ticket != "" && now.Before(entry.expiresAt) {
			m.mu.Unlock()
			return entry.ticket, entry.desktopID
		}
	}
	m.mu.Unlock()

	ticket, actualID, ttl := m.requestTicket(ctx, m.apiKey, desktopID)
	if ticket != "" {
		if actualID == "" {
			actualID = desktopID
		}
		entry := cuaTicketEntry{
			ticket:    ticket,
			desktopID: actualID,
			expiresAt: time.Now().Add(ttl),
		}
		m.mu.Lock()
		m.entries[desktopID] = entry
		m.entries[actualID] = entry
		m.mu.Unlock()
		return ticket, actualID
	}
	return "", ""
}

func (m *cuaTicketManager) requestTicket(ctx context.Context, apiKey, desktopID string) (string, string, time.Duration) {
	endpoint := m.gatewayURL + "/v1/desktop/access"
	if desktopID != "" {
		endpoint += "?desktop=" + url.QueryEscape(desktopID)
	}
	reqCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", "", 0
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return "", "", 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", 0
	}

	var payload struct {
		OK   bool `json:"ok"`
		Data struct {
			Ticket     string `json:"ticket"`
			DesktopID  string `json:"desktop_id"`
			TTLSeconds int    `json:"ttl_seconds"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil || !payload.OK || payload.Data.Ticket == "" {
		return "", "", 0
	}
	ttl := 20 * time.Minute
	if payload.Data.TTLSeconds > 180 {
		d := time.Duration(payload.Data.TTLSeconds-120) * time.Second
		if d < ttl {
			ttl = d
		}
	}
	return payload.Data.Ticket, payload.Data.DesktopID, ttl
}

func newCUAUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("ama-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]),
	)
}

type cuaLeaseState struct {
	mu           sync.Mutex
	desktopID    string
	ticket       string
	ownerID      string
	pageID       string
	leaseID      string
	path         string
	lastRespJSON []byte
	lastRenewed  time.Time
	lastSeen     time.Time
	running      bool
}

type cuaLeaseManager struct {
	mu             sync.Mutex
	states         map[string]*cuaLeaseState
	upstreamOrigin string
	ticketMgr      *cuaTicketManager
	httpClient     *http.Client
	log            *slog.Logger
}

func newCUALeaseManager(upstreamOrigin string, ticketMgr *cuaTicketManager, log *slog.Logger) *cuaLeaseManager {
	return &cuaLeaseManager{
		states:         make(map[string]*cuaLeaseState),
		upstreamOrigin: strings.TrimRight(upstreamOrigin, "/"),
		ticketMgr:      ticketMgr,
		httpClient:     &http.Client{Timeout: 10 * time.Second},
		log:            log,
	}
}

func (m *cuaLeaseManager) getState(desktopID string) *cuaLeaseState {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.states[desktopID]
	if !ok {
		st = &cuaLeaseState{desktopID: desktopID}
		m.states[desktopID] = st
	}
	return st
}

func (m *cuaLeaseManager) Touch(desktopID, ticket string) {
	if desktopID == "" {
		return
	}
	st := m.getState(desktopID)
	st.mu.Lock()
	st.lastSeen = time.Now()
	if ticket != "" {
		st.ticket = ticket
	}
	st.mu.Unlock()
}

func (m *cuaLeaseManager) Release(desktopID string) {
	if desktopID == "" {
		return
	}
	st := m.getState(desktopID)
	st.mu.Lock()
	st.ownerID = ""
	st.pageID = ""
	st.leaseID = ""
	st.lastRespJSON = nil
	st.lastRenewed = time.Time{}
	st.mu.Unlock()
}

type cuaUpstreamLeaseResp struct {
	OK    bool `json:"ok"`
	Owner bool `json:"owner"`
	Lease struct {
		OwnerID string `json:"ownerId"`
		PageID  string `json:"pageId"`
		LeaseID string `json:"leaseId"`
	} `json:"lease"`
}

func (m *cuaLeaseManager) postLeaseUpstream(
	ctx context.Context,
	desktopID, ticket, ownerID, pageID, leaseID, path string,
	force, takeover bool,
) ([]byte, *cuaUpstreamLeaseResp, int, error) {
	endpoint := fmt.Sprintf("%s/desktops/%s/api/desktops/%s/viewer-lease",
		m.upstreamOrigin, url.PathEscape(desktopID), url.PathEscape(desktopID))
	if path == "" {
		path = fmt.Sprintf("/desktops/%s/cua-app/", desktopID)
	}
	reqPayload := map[string]any{
		"ownerId":  ownerID,
		"pageId":   pageID,
		"force":    force,
		"takeover": takeover,
		"path":     path,
	}
	if leaseID != "" {
		reqPayload["leaseId"] = leaseID
	}
	bodyBytes, err := json.Marshal(reqPayload)
	if err != nil {
		return nil, nil, 0, err
	}

	reqCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", m.upstreamOrigin)
	req.Header.Set("Referer", m.upstreamOrigin+path)
	if ticket != "" {
		req.Header.Set("Cookie", "cua_desktop_ticket="+ticket)
	}

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, nil, 0, err
	}
	defer resp.Body.Close()
	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, resp.StatusCode, err
	}
	var parsed cuaUpstreamLeaseResp
	_ = json.Unmarshal(respBytes, &parsed)
	return respBytes, &parsed, resp.StatusCode, nil
}

func (m *cuaLeaseManager) EnsureLease(
	ctx context.Context,
	desktopID, ticket, reqOwnerID, reqPageID, reqLeaseID, reqPath string,
	forceTakeover, forceSync bool,
) (ownerID, pageID, leaseID string, respJSON []byte, ok bool) {
	if desktopID == "" {
		return "", "", "", nil, false
	}
	st := m.getState(desktopID)
	now := time.Now()

	st.mu.Lock()
	st.lastSeen = now
	if ticket != "" {
		st.ticket = ticket
	} else {
		ticket = st.ticket
	}
	if reqPath != "" {
		st.path = reqPath
	}
	path := st.path

	if reqOwnerID != "" {
		ownerID = reqOwnerID
		pageID = reqPageID
		if pageID == "" {
			pageID = ownerID
		}
		if reqLeaseID != "" {
			leaseID = reqLeaseID
		} else if st.ownerID == ownerID && st.pageID == pageID {
			leaseID = st.leaseID
		}
	} else if st.ownerID != "" && st.leaseID != "" {
		ownerID = st.ownerID
		pageID = st.pageID
		leaseID = st.leaseID
	} else {
		uid := newCUAUUID()
		ownerID = uid
		pageID = uid
	}

	// Fast path: if the server-side keepalive loop already renewed this exact lease within the last 2.5s,
	// return the cached response immediately (< 1ms) so browser HTTP/1.1 connections never stall.
	if !forceSync && !forceTakeover &&
		st.ownerID == ownerID && st.pageID == pageID && st.leaseID != "" &&
		len(st.lastRespJSON) > 0 && now.Sub(st.lastRenewed) < 2500*time.Millisecond {
		if !st.running {
			st.running = true
			go m.keepAliveLoop(st)
		}
		resOwner, resPage, resLease, resJSON := st.ownerID, st.pageID, st.leaseID, st.lastRespJSON
		st.mu.Unlock()
		return resOwner, resPage, resLease, resJSON, true
	}
	st.mu.Unlock()

	force := forceTakeover || leaseID == ""
	rawBytes, parsed, status, err := m.postLeaseUpstream(ctx, desktopID, ticket, ownerID, pageID, leaseID, path, force, force)
	if status == http.StatusUnauthorized && m.ticketMgr != nil {
		if freshTicket, _ := m.ticketMgr.getOrMint(ctx, desktopID, true); freshTicket != "" {
			ticket = freshTicket
			rawBytes, parsed, status, err = m.postLeaseUpstream(ctx, desktopID, ticket, ownerID, pageID, leaseID, path, force, force)
		}
	}
	if err == nil && (status != http.StatusOK || parsed == nil || !parsed.Owner || parsed.Lease.LeaseID == "") {
		// Retry once with force=true, takeover=true, and empty leaseId to recover from expired/held leases.
		rawBytes, parsed, status, err = m.postLeaseUpstream(ctx, desktopID, ticket, ownerID, pageID, "", path, true, true)
	}
	if err != nil || status != http.StatusOK || parsed == nil || !parsed.Owner || parsed.Lease.LeaseID == "" {
		return ownerID, pageID, leaseID, nil, false
	}

	st.mu.Lock()
	st.ticket = ticket
	st.ownerID = parsed.Lease.OwnerID
	st.pageID = parsed.Lease.PageID
	if st.pageID == "" {
		st.pageID = st.ownerID
	}
	st.leaseID = parsed.Lease.LeaseID
	st.lastRespJSON = rawBytes
	st.lastRenewed = time.Now()
	st.lastSeen = time.Now()
	if !st.running {
		st.running = true
		go m.keepAliveLoop(st)
	}
	resOwner, resPage, resLease, resJSON := st.ownerID, st.pageID, st.leaseID, st.lastRespJSON
	st.mu.Unlock()
	return resOwner, resPage, resLease, resJSON, true
}

func (m *cuaLeaseManager) keepAliveLoop(st *cuaLeaseState) {
	ticker := time.NewTicker(1500 * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		st.mu.Lock()
		if time.Since(st.lastSeen) > 90*time.Second || st.ownerID == "" || st.leaseID == "" {
			st.running = false
			st.mu.Unlock()
			return
		}
		desktopID := st.desktopID
		ticket := st.ticket
		ownerID := st.ownerID
		pageID := st.pageID
		leaseID := st.leaseID
		path := st.path
		st.mu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		rawBytes, parsed, status, err := m.postLeaseUpstream(ctx, desktopID, ticket, ownerID, pageID, leaseID, path, false, false)
		if status == http.StatusUnauthorized && m.ticketMgr != nil {
			if freshTicket, _ := m.ticketMgr.getOrMint(ctx, desktopID, true); freshTicket != "" {
				ticket = freshTicket
				rawBytes, parsed, status, err = m.postLeaseUpstream(ctx, desktopID, ticket, ownerID, pageID, leaseID, path, false, false)
			}
		}
		cancel()

		if err == nil && status == http.StatusOK && parsed != nil && parsed.Owner && parsed.Lease.LeaseID != "" {
			st.mu.Lock()
			if st.ownerID == ownerID && st.pageID == pageID {
				st.ticket = ticket
				st.leaseID = parsed.Lease.LeaseID
				st.lastRespJSON = rawBytes
				st.lastRenewed = time.Now()
			}
			st.mu.Unlock()
			continue
		}
		// Another instance or tab took over the lease (or the lease expired); stop renewing on this
		// pod instead of fighting with force=true/takeover=true across pods.
		st.mu.Lock()
		st.running = false
		st.mu.Unlock()
		return
	}
}

func extractCUADesktopID(path string) string {
	if !strings.HasPrefix(path, PathCUAProxyPrefix) {
		return ""
	}
	rest := strings.TrimPrefix(path, PathCUAProxyPrefix)
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) > 0 && strings.HasPrefix(parts[0], "desk_") {
		return parts[0]
	}
	return ""
}

func upsertCookieHeader(h http.Header, name, value string) {
	prefix := name + "="
	var kept []string
	for _, raw := range h.Values("Cookie") {
		for _, part := range strings.Split(raw, ";") {
			part = strings.TrimSpace(part)
			if part == "" || strings.HasPrefix(part, prefix) {
				continue
			}
			kept = append(kept, part)
		}
	}
	kept = append(kept, prefix+value)
	h.Set("Cookie", strings.Join(kept, "; "))
}

func isStaticCUAAsset(path string) bool {
	lower := strings.ToLower(path)
	for _, ext := range []string{".js", ".css", ".wasm", ".woff", ".woff2", ".ttf", ".svg", ".png", ".jpg", ".webp"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

var (
	cookieSecureRe = regexp.MustCompile(`(?i);\s*secure\b`)
	cookieDomainRe = regexp.MustCompile(`(?i);\s*domain=[^;]+`)
)

type cuaInjectLeaseCtxKey struct{}

type cuaInjectLeaseInfo struct {
	DesktopID string
	OwnerID   string
	PageID    string
	LeaseID   string
}

func buildCUAHeadInjection(leaseInfo *cuaInjectLeaseInfo) string {
	var leaseScript string
	if leaseInfo != nil && leaseInfo.LeaseID != "" {
		leaseScript = fmt.Sprintf(`
  try {
    var u = new URL(window.location.href);
    if (!u.searchParams.get('lease_id')) {
      u.searchParams.set('lease_owner_id', %q);
      u.searchParams.set('lease_page_id', %q);
      u.searchParams.set('lease_id', %q);
      window.history.replaceState(null, '', u.pathname + u.search + u.hash);
    }
    var leaseUrl = '/desktops/' + encodeURIComponent(%q) + '/api/desktops/' + encodeURIComponent(%q) + '/viewer-lease';
    var ownerId = u.searchParams.get('lease_owner_id') || %q;
    var pageId = u.searchParams.get('lease_page_id') || %q;
    var leaseId = u.searchParams.get('lease_id') || %q;
    window.setInterval(function() {
      fetch(leaseUrl, {
        method: 'POST',
        credentials: 'same-origin',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({ownerId: ownerId, pageId: pageId, leaseId: leaseId, path: window.location.pathname})
      }).catch(function(){});
    }, 5000);
  } catch (e) {}`,
			leaseInfo.OwnerID, leaseInfo.PageID, leaseInfo.LeaseID,
			leaseInfo.DesktopID, leaseInfo.DesktopID,
			leaseInfo.OwnerID, leaseInfo.PageID, leaseInfo.LeaseID,
		)
	}

	return `<script>
(function(){
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID !== 'function') {
    crypto.randomUUID = function() {
      return '10000000-1000-4000-8000-100000000000'.replace(/[018]/g, function(c) {
        var n = Number(c);
        var r = crypto.getRandomValues(new Uint8Array(1))[0];
        return (n ^ (r & (15 >> (n / 4)))).toString(16);
      });
    };
  }
  try {
    var params = new URLSearchParams(window.location.search);
    var mode = params.get('ama_mode') || window.sessionStorage.getItem('ama_cua_mode');
    if (params.get('ama_mode')) {
      window.sessionStorage.setItem('ama_cua_mode', params.get('ama_mode'));
    }
    if (mode === 'desktop') {
      document.documentElement.setAttribute('data-ama-mode', 'desktop');
    } else if (mode === 'full') {
      document.documentElement.removeAttribute('data-ama-mode');
    }
  } catch (e) {}` + leaseScript + `
})();
</script>
<style>
html[data-ama-mode="desktop"] .cua-sidebar-shell,
html[data-ama-mode="desktop"] #cua-rail-expand-button {
  display: none !important;
}
html[data-ama-mode="desktop"] .cua-workspace-shell,
html[data-ama-mode="desktop"] .cua-main-shell {
  display: flex !important;
  width: 100vw !important;
  max-width: 100vw !important;
  height: 100dvh !important;
  min-height: 100dvh !important;
  max-height: 100dvh !important;
  flex: 1 1 100% !important;
}
.orientation-vertical .cua-workspace-shell {
  height: min(56dvh, calc(56.25vw + 2.5rem)) !important;
  min-height: 300px !important;
  max-height: 58dvh !important;
}
</style>`
}

func isHTTPSRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}

// registerCUAProxy keeps the in-package test and embedding entry point stable.
func registerCUAProxy(
	mux *http.ServeMux, log *slog.Logger, cfg CUAConfig, protect func(http.Handler) http.Handler,
) {
	RegisterCUAProxy(mux, log, cfg, protect)
}

// RegisterCUAProxy mounts the optional authenticated CUA reverse proxy on mux.
func RegisterCUAProxy(
	mux *http.ServeMux,
	log *slog.Logger,
	cfg CUAConfig,
	protect func(http.Handler) http.Handler,
	authorize ...CUADesktopAuthorizer,
) {
	if !cfg.Enabled {
		return
	}
	rawUpstream := strings.TrimSpace(cfg.UpstreamURL)
	if rawUpstream == "" {
		rawUpstream = defaultCUAUpstream
	}
	target, err := url.Parse(rawUpstream)
	if err != nil || target.Scheme == "" || target.Host == "" {
		target, _ = url.Parse(defaultCUAUpstream)
	}
	originBase := target.Scheme + "://" + target.Host
	ticketMgr := newCUATicketManager(log, cfg.GatewayURL, cfg.APIKey)
	leaseMgr := newCUALeaseManager(originBase, ticketMgr, log)

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
			pr.SetXForwarded()

			// Rewrite Origin & Referer so upstream WebSocket/CSRF checks see its own origin.
			pr.Out.Header.Set("Origin", originBase)
			if ref := pr.In.Header.Get("Referer"); ref != "" {
				if refURL, err := url.Parse(ref); err == nil {
					refURL.Scheme = target.Scheme
					refURL.Host = target.Host
					pr.Out.Header.Set("Referer", refURL.String())
				}
			} else {
				pr.Out.Header.Set("Referer", originBase+"/")
			}

			// Request uncompressed HTML on entry pages so ModifyResponse can inject the polyfill cleanly.
			if !strings.EqualFold(pr.In.Header.Get("Upgrade"), "websocket") &&
				(strings.HasSuffix(pr.In.URL.Path, "/") || strings.HasSuffix(pr.In.URL.Path, ".html")) {
				pr.Out.Header.Del("Accept-Encoding")
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode == http.StatusSwitchingProtocols {
				return nil
			}

			// Allow same-origin iframe embedding and keep cookies working on plain HTTP deployments.
			resp.Header.Del("X-Frame-Options")
			resp.Header.Del("Content-Security-Policy")

			// Allow browser caching of heavy static assets (viewer.js 1.24MB, ogv.js, wasm, css)
			// instead of upstream's default `Cache-Control: private, no-store` so HTTP/1.1
			// connection slots are freed immediately on subsequent loads.
			if resp.StatusCode == http.StatusOK && resp.Request != nil && isStaticCUAAsset(resp.Request.URL.Path) {
				resp.Header.Set("Cache-Control", "public, max-age=3600")
			}

			// If upstream rejects the ticket (401 InvalidTicket / MissingTicket), force-refresh the
			// cached ticket for this desktop and redirect entry pages immediately.
			if resp.StatusCode == http.StatusUnauthorized && resp.Request != nil {
				if desktopID := extractCUADesktopID(resp.Request.URL.Path); desktopID != "" {
					if freshTicket, actualID := ticketMgr.getOrMint(resp.Request.Context(), desktopID, true); freshTicket != "" {
						targetID := desktopID
						if actualID != "" {
							targetID = actualID
						}
						leaseMgr.Touch(targetID, freshTicket)
						resp.Header.Add("Set-Cookie", "cua_desktop_ticket="+freshTicket+"; Path=/desktops/"+targetID+"/; HttpOnly; SameSite=Lax")
						if !strings.Contains(resp.Request.URL.Path, "/api/") &&
							!strings.EqualFold(resp.Request.Header.Get("Upgrade"), "websocket") {
							if resp.Body != nil {
								_, _ = io.Copy(io.Discard, resp.Body)
								_ = resp.Body.Close()
							}
							q := resp.Request.URL.Query()
							q.Set("ticket", freshTicket)
							q.Del("ama_reload")
							redirectPath := resp.Request.URL.Path
							if actualID != "" && actualID != desktopID {
								redirectPath = PathCUAProxyPrefix + actualID + strings.TrimPrefix(redirectPath, PathCUAProxyPrefix+desktopID)
							}
							resp.StatusCode = http.StatusFound
							resp.Status = "302 Found"
							resp.Header.Del("Content-Type")
							resp.Header.Set("Location", redirectPath+"?"+q.Encode())
							resp.Header.Set("Content-Length", "0")
							resp.ContentLength = 0
							resp.Body = io.NopCloser(strings.NewReader(""))
							return nil
						}
					}
				}
			}

			if cookies := resp.Header.Values("Set-Cookie"); len(cookies) > 0 {
				resp.Header.Del("Set-Cookie")
				stripSecure := !isHTTPSRequest(resp.Request)
				for _, c := range cookies {
					cleaned := cookieDomainRe.ReplaceAllString(c, "")
					if stripSecure {
						cleaned = cookieSecureRe.ReplaceAllString(cleaned, "")
					}
					resp.Header.Add("Set-Cookie", cleaned)
				}
			}

			// Preserve ama_mode=desktop across the 303 ticket-redemption redirect.
			if resp.Request != nil && resp.Request.URL.Query().Get("ama_mode") == "desktop" {
				if loc := resp.Header.Get("Location"); loc != "" && !strings.Contains(loc, "ama_mode=") {
					sep := "?"
					if strings.Contains(loc, "?") {
						sep = "&"
					}
					resp.Header.Set("Location", loc+sep+"ama_mode=desktop")
				}
			}

			// Inject crypto.randomUUID polyfill, lease bootstrap, and ama_mode=desktop CSS into HTML responses.
			ct := strings.ToLower(resp.Header.Get("Content-Type"))
			if resp.StatusCode == http.StatusOK && strings.Contains(ct, "text/html") && resp.Body != nil {
				bodyBytes, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if err != nil {
					return err
				}
				var leaseInfo *cuaInjectLeaseInfo
				if resp.Request != nil {
					if v, ok := resp.Request.Context().Value(cuaInjectLeaseCtxKey{}).(*cuaInjectLeaseInfo); ok {
						leaseInfo = v
					}
				}
				injection := buildCUAHeadInjection(leaseInfo)
				lower := bytes.ToLower(bodyBytes)
				var modified []byte
				if idx := bytes.Index(lower, []byte("<head>")); idx >= 0 {
					insertPos := idx + len("<head>")
					modified = make([]byte, 0, len(bodyBytes)+len(injection))
					modified = append(modified, bodyBytes[:insertPos]...)
					modified = append(modified, []byte(injection)...)
					modified = append(modified, bodyBytes[insertPos:]...)
				} else {
					modified = append([]byte(injection), bodyBytes...)
				}
				resp.Body = io.NopCloser(bytes.NewReader(modified))
				resp.ContentLength = int64(len(modified))
				resp.Header.Set("Content-Length", strconv.Itoa(len(modified)))
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.WarnContext(r.Context(), "CUA 反向代理请求上游失败",
				slog.String("path", r.URL.Path),
				slog.String("err", err.Error()),
			)
			http.Error(w, "CUA 云桌面连接失败: "+err.Error(), http.StatusBadGateway)
		},
	}

	desktopHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if desktopID := extractCUADesktopID(r.URL.Path); desktopID != "" {
			q := r.URL.Query()
			forceRefresh := q.Get("ama_reload") != ""
			targetID := desktopID
			freshTicket, actualID := ticketMgr.getOrMint(r.Context(), desktopID, forceRefresh)
			if freshTicket != "" {
				if actualID != "" && actualID != desktopID {
					targetID = actualID
					r.URL.Path = PathCUAProxyPrefix + actualID + strings.TrimPrefix(r.URL.Path, PathCUAProxyPrefix+desktopID)
					r.URL.RawPath = ""
				}
				// Strip one-time `ticket` and `ama_reload` query params when forwarding to upstream
				// since `cua_desktop_ticket` is injected directly as a Cookie header. This avoids
				// upstream's 303 redirect roundtrip and keeps `/relay/rustdesk/` URLs intact.
				if q.Get("ticket") != "" || q.Get("ama_reload") != "" {
					q.Del("ticket")
					q.Del("ama_reload")
					r.URL.RawQuery = q.Encode()
				}
				upsertCookieHeader(r.Header, "cua_desktop_ticket", freshTicket)
				if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") && strings.HasSuffix(r.URL.Path, "/") {
					w.Header().Add("Set-Cookie", "cua_desktop_ticket="+freshTicket+"; Path=/desktops/"+targetID+"/; HttpOnly; SameSite=Lax")
				}
			}

			leaseMgr.Touch(targetID, freshTicket)

			// 1. Intercept `/api/desktops/{id}/viewer-lease` so browser heartbeats return in <1ms
			//    and our server-side 1.5s renewal loop keeps the 8s upstream lease alive continuously.
			if strings.HasSuffix(r.URL.Path, "/viewer-lease") && strings.Contains(r.URL.Path, "/api/desktops/") {
				if r.Method == http.MethodGet {
					if _, _, _, respJSON, ok := leaseMgr.EnsureLease(r.Context(), targetID, freshTicket, "", "", "", "", false, false); ok && len(respJSON) > 0 {
						w.Header().Set("Content-Type", "application/json; charset=utf-8")
						w.Header().Set("Cache-Control", "private, no-store")
						w.WriteHeader(http.StatusOK)
						_, _ = w.Write(respJSON)
						return
					}
				} else if r.Method == http.MethodPost {
					bodyBytes, _ := io.ReadAll(r.Body)
					_ = r.Body.Close()
					r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

					var reqBody struct {
						OwnerID          string `json:"ownerId"`
						PageID           string `json:"pageId"`
						LeaseID          string `json:"leaseId"`
						Path             string `json:"path"`
						Force            bool   `json:"force"`
						Takeover         bool   `json:"takeover"`
						Release          bool   `json:"release"`
						ManualInputRunID any    `json:"manualInputRunId"`
					}
					if err := json.Unmarshal(bodyBytes, &reqBody); err == nil {
						if reqBody.Release {
							leaseMgr.Release(targetID)
						} else if reqBody.ManualInputRunID == nil && reqBody.OwnerID != "" {
							st := leaseMgr.getState(targetID)
							st.mu.Lock()
							ownerChanged := st.ownerID != "" && st.ownerID != reqBody.OwnerID
							st.mu.Unlock()
							forceTakeover := reqBody.Force || reqBody.Takeover || ownerChanged
							if _, _, _, respJSON, ok := leaseMgr.EnsureLease(
								r.Context(),
								targetID,
								freshTicket,
								reqBody.OwnerID,
								reqBody.PageID,
								reqBody.LeaseID,
								reqBody.Path,
								forceTakeover,
								false,
							); ok && len(respJSON) > 0 {
								w.Header().Set("Content-Type", "application/json; charset=utf-8")
								w.Header().Set("Cache-Control", "private, no-store")
								w.WriteHeader(http.StatusOK)
								_, _ = w.Write(respJSON)
								return
							}
						}
					}
				} else if r.Method == http.MethodDelete {
					leaseMgr.Release(targetID)
				}
			}

			// 2. When serving the RustDesk viewer HTML shell (`/relay/rustdesk/{id}/`), ensure a
			//    server-maintained viewer lease is active and inject `lease_owner_id`, `lease_page_id`,
			//    and `lease_id` into `<head>` so `/relay/rustdesk/{id}/` works standalone in an iframe.
			if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/relay/rustdesk/") && strings.HasSuffix(r.URL.Path, "/") {
				ownerID, pageID, leaseID, _, ok := leaseMgr.EnsureLease(
					r.Context(),
					targetID,
					freshTicket,
					q.Get("lease_owner_id"),
					q.Get("lease_page_id"),
					q.Get("lease_id"),
					r.URL.Path,
					false,
					false,
				)
				if ok && leaseID != "" {
					r = r.WithContext(context.WithValue(r.Context(), cuaInjectLeaseCtxKey{}, &cuaInjectLeaseInfo{
						DesktopID: targetID,
						OwnerID:   ownerID,
						PageID:    pageID,
						LeaseID:   leaseID,
					}))
				}
			}

			// 3. Before forwarding RustDesk `/session` or `/display-resolution`, ensure the upstream
			//    viewer lease is freshly renewed (and kept alive every 1.5s while `/session` executes)
			//    and attach the active `X-CUA-Viewer-*` headers so `/session` never fails with 409.
			if (strings.Contains(r.URL.Path, "/relay/rustdesk/") && strings.HasSuffix(r.URL.Path, "/session") && r.Method == http.MethodGet) ||
				(strings.Contains(r.URL.Path, "/api/desktops/") && strings.HasSuffix(r.URL.Path, "/display-resolution") && r.Method == http.MethodPost) {
				isSession := strings.HasSuffix(r.URL.Path, "/session")
				reqOwner := strings.TrimSpace(r.Header.Get("X-CUA-Viewer-Owner"))
				reqPage := strings.TrimSpace(r.Header.Get("X-CUA-Viewer-Page"))
				reqLease := strings.TrimSpace(r.Header.Get("X-CUA-Viewer-Lease"))
				ownerID, pageID, leaseID, _, ok := leaseMgr.EnsureLease(
					r.Context(),
					targetID,
					freshTicket,
					reqOwner,
					reqPage,
					reqLease,
					"",
					false,
					isSession,
				)
				if ok && leaseID != "" {
					r.Header.Set("X-CUA-Viewer-Owner", ownerID)
					r.Header.Set("X-CUA-Viewer-Page", pageID)
					r.Header.Set("X-CUA-Viewer-Lease", leaseID)
				}
			}
		}
		proxy.ServeHTTP(w, r)
	})

	var desktopProtected, appProtected http.Handler = desktopHandler, proxy
	if protect != nil {
		desktopProtected = protect(desktopHandler)
		appProtected = protect(proxy)
	}
	if len(authorize) > 0 && authorize[0] != nil {
		desktopProtected = CUAAuthorizerMiddleware(authorize[0], desktopProtected)
		appProtected = CUAAuthorizerMiddleware(authorize[0], appProtected)
	}
	mux.Handle(PathCUAProxyPrefix, desktopProtected)
	mux.Handle(PathCUAAppPrefix, appProtected)
}

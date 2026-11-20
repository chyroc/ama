package httpd

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCUAProxyAutoRefreshesExpiredTicketAndEnforcesAuth(t *testing.T) {
	// Mock CUA gateway that mints a fresh ticket when called with Bearer ark-test-key.
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/desktop/access" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer ark-test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("desktop") != "desk_abc123" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"data":{"desktop_id":"desk_abc123","ticket":"dt_fresh_999","ttl_seconds":1800}}`))
	}))
	defer gateway.Close()

	// Mock CUA upstream that only accepts dt_fresh_999.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		qTicket := r.URL.Query().Get("ticket")
		cookie, _ := r.Cookie("cua_desktop_ticket")
		cookieVal := ""
		if cookie != nil {
			cookieVal = cookie.Value
		}
		if qTicket != "dt_fresh_999" && cookieVal != "dt_fresh_999" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"InvalidTicket","message":"ticket is invalid or expired"}`))
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><head><title>CUA</title></head><body>ok</body></html>`))
	}))
	defer upstream.Close()

	mux := http.NewServeMux()
	protect := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Test-Auth") != "allow" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	registerCUAProxy(mux, slog.Default(), CUAConfig{
		Enabled:     true,
		UpstreamURL: upstream.URL,
		GatewayURL:  gateway.URL,
		APIKey:      "ark-test-key",
	}, protect)
	proxySrv := httptest.NewServer(mux)
	defer proxySrv.Close()

	// Unauthenticated requests must be rejected before minting any desktop ticket.
	unauthResp, err := http.Get(proxySrv.URL + "/desktops/desk_abc123/cua-app/?ticket=dt_expired_old")
	if err != nil {
		t.Fatalf("GET unauth failed: %v", err)
	}
	_ = unauthResp.Body.Close()
	if unauthResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated caller, got %d", unauthResp.StatusCode)
	}

	req, err := http.NewRequest(http.MethodGet, proxySrv.URL+"/desktops/desk_abc123/cua-app/?ticket=dt_expired_old", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("X-Test-Auth", "allow")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET proxy failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK with auto-refreshed ticket, got %d: %s", resp.StatusCode, string(body))
	}
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	body := string(bodyBytes)
	if !strings.Contains(body, "crypto.randomUUID") {
		t.Errorf("expected injected crypto.randomUUID polyfill in HTML body, got: %s", body)
	}
}

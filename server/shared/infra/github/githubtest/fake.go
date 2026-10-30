// Package githubtest is a fake GitHub (httptest) that lets the auth and authdao tests run the full
// device flow: request a device code, return scripted token exchange results in order, /user,
// /user/emails, org membership. Only imported by tests.
package githubtest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/chyroc/ama/server/shared/infra/github"
)

// Fixed values the fake GitHub hands out. DeviceCode and AccessToken are probe strings tests use to
// assert "the secret does not appear in any response".
const (
	ClientID    = "fake-client-id"
	DeviceCode  = "dc-SECRET-device-code-must-not-leak"
	UserCode    = "ABCD-1234"
	AccessToken = "gho_SECRET_access_token_must_not_leak"
)

// A script item for token exchange.
const (
	Pending  = github.ErrCodeAuthorizationPending
	SlowDown = github.ErrCodeSlowDown
	Expired  = github.ErrCodeExpiredToken
	Denied   = github.ErrCodeAccessDenied
	OK       = "ok"
)

// Server is the fake GitHub. Change fields before Start; after that, read results through the
// counters guarded by mu.
type Server struct {
	t   *testing.T
	srv *httptest.Server

	// Interval / ExpiresIn are the values returned when requesting a device code (seconds).
	Interval  int
	ExpiresIn int
	// SlowDownInterval is the interval returned with slow_down (seconds); 0 = do not return the
	// field.
	SlowDownInterval int

	// Script is the sequence of token exchange results; after it runs out, the last item repeats.
	Script []string

	Login     string
	Name      string
	AvatarURL string
	Emails    []github.Email
	// Orgs are the orgs the user is an active member of (lowercase).
	Orgs map[string]bool

	mu         sync.Mutex
	tokenCalls int
	scope      string
}

// New starts a fake GitHub with defaults: 5-second interval, 15-minute expiry, immediate success,
// one verified primary email.
func New(t *testing.T) *Server {
	t.Helper()
	s := &Server{
		t: t, Interval: 5, ExpiresIn: 900, Script: []string{OK},
		Login: "Octo-Cat", Name: "Octo Cat", AvatarURL: "https://avatars.example/octo.png",
		Emails: []github.Email{
			{Email: "octo@users.noreply.github.com", Primary: false, Verified: true},
			{Email: "Octo@Example.com", Primary: true, Verified: true},
		},
		Orgs: map[string]bool{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/device/code", s.deviceCode)
	mux.HandleFunc("POST /login/oauth/access_token", s.accessToken)
	mux.HandleFunc("GET /user", s.api(s.user))
	mux.HandleFunc("GET /user/emails", s.api(s.emails))
	mux.HandleFunc("GET /user/memberships/orgs/{org}", s.api(s.membership))
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

// URL is the fake server's address, used as both WebURL and APIURL.
func (s *Server) URL() string { return s.srv.URL }

// Client builds a real github.Client pointing at the fake server.
func (s *Server) Client(t *testing.T) *github.Client {
	t.Helper()
	c, err := github.New(github.Config{ClientID: ClientID, WebURL: s.srv.URL, APIURL: s.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TokenCalls is how many times token exchange was called.
func (s *Server) TokenCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokenCalls
}

// Scope is the scope sent with the most recent device code request.
func (s *Server) Scope() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scope
}

func (s *Server) deviceCode(w http.ResponseWriter, r *http.Request) {
	if !s.checkForm(w, r) {
		return
	}
	s.mu.Lock()
	s.scope = r.PostForm.Get("scope")
	s.mu.Unlock()
	writeJSON(w, map[string]any{
		"device_code":      DeviceCode,
		"user_code":        UserCode,
		"verification_uri": "https://github.com/login/device",
		"expires_in":       s.ExpiresIn,
		"interval":         s.Interval,
	})
}

func (s *Server) accessToken(w http.ResponseWriter, r *http.Request) {
	if !s.checkForm(w, r) {
		return
	}
	if r.PostForm.Get("device_code") != DeviceCode ||
		r.PostForm.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" {
		writeJSON(w, map[string]any{"error": "incorrect_device_code"})
		return
	}
	s.mu.Lock()
	i := s.tokenCalls
	s.tokenCalls++
	s.mu.Unlock()
	step := s.Script[len(s.Script)-1]
	if i < len(s.Script) {
		step = s.Script[i]
	}
	switch step {
	case OK:
		writeJSON(w, map[string]any{"access_token": AccessToken, "token_type": "bearer", "scope": "read:user,user:email"})
	case SlowDown:
		body := map[string]any{"error": SlowDown}
		if s.SlowDownInterval > 0 {
			body["interval"] = s.SlowDownInterval
		}
		writeJSON(w, body)
	default:
		writeJSON(w, map[string]any{"error": step})
	}
}

// checkForm checks the common requirements of GitHub's two OAuth endpoints: form encoding, Accept
// JSON, correct client_id.
func (s *Server) checkForm(w http.ResponseWriter, r *http.Request) bool {
	if err := r.ParseForm(); err != nil || r.Header.Get("Accept") != "application/json" ||
		r.PostForm.Get("client_id") != ClientID {
		http.Error(w, "bad request", http.StatusBadRequest)
		return false
	}
	return true
}

func (s *Server) api(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+AccessToken {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func (s *Server) user(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"id": 1, "login": s.Login, "name": s.Name, "avatar_url": s.AvatarURL})
}

func (s *Server) emails(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.Emails)
}

func (s *Server) membership(w http.ResponseWriter, r *http.Request) {
	if !s.Orgs[strings.ToLower(r.PathValue("org"))] {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"state": "active", "role": "member"})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

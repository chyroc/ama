// Auth tests for the host daemon routes on the bare mux.
//
// Heartbeat/results deliberately bypass the authz matrix gate (see the file header of
// handler_hostdaemon.go). The security boundary lost by bypassing the gate must be covered by
// tests, in the same shape as the terminal WS ticket tests:
// missing token / malformed / revoked all get 401; a hostId claimed in the body cannot replace the
// host in the token.
package apihandler

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chyroc/ama/server/model/persistmodel"
	hostsvc "github.com/chyroc/ama/server/service/host"
	"github.com/chyroc/ama/server/shared/hostproto"
)

// fakeHostDaemon is a controllable stand-in for HostDaemonServices.
type fakeHostDaemon struct {
	authedHostID string // host returned when AuthenticateToken matches; empty = recognize nobody
	results      []hostproto.ExecResultItem
	frames       []hostproto.TermFrame
	// Stream-related: beginErr makes BeginStream return ErrNotFound (the revoked path);
	// replacedStream puts a new session into the "kicked out by a newer connection" state as soon as
	// it is created (the 4002 path); resultsErr makes result handling fail once (the ack ok=false path).
	beginCalls     []hostproto.HeartbeatRequest
	beginErr       error
	replacedStream bool
	resultsErr     error
}

func (f *fakeHostDaemon) Register(context.Context, string, hostproto.RegisterRequest, time.Time,
) (hostproto.RegisterResponse, error) {
	return hostproto.RegisterResponse{HostID: "hst_new"}, nil
}

func (f *fakeHostDaemon) AuthenticateToken(_ context.Context, token string) (persistmodel.Host, error) {
	if f.authedHostID != "" && token == "good-token" {
		return persistmodel.Host{ID: f.authedHostID, OwnerEmail: "a@example.com", Status: "online"}, nil
	}
	return persistmodel.Host{}, persistmodel.ErrNotFound
}

func (f *fakeHostDaemon) Heartbeat(_ context.Context, h persistmodel.Host, req hostproto.HeartbeatRequest, _ time.Time,
) (hostproto.HeartbeatResponse, error) {
	return hostproto.HeartbeatResponse{Calls: []hostproto.Dispatch{{ID: "htc_1", Seq: 1, Kind: "exec"}}}, nil
}

func (f *fakeHostDaemon) SubmitResults(_ context.Context, req hostproto.ResultsRequest) error {
	if f.resultsErr != nil {
		return f.resultsErr
	}
	f.results = append(f.results, req.ExecResults...)
	f.frames = append(f.frames, req.TermFrames...)
	return nil
}

// fakeHostStream is a minimal stand-in for a WS session: Wait blocks until ctx ends by default
// (the pump spins but does not exit, so the "kicked" test is driven only by Done); wake can let
// one tick through by hand.
type fakeHostStream struct {
	revoked  bool
	replaced bool
	done     chan struct{}
	closeOne sync.Once

	wake chan struct{}
}

func newFakeHostStream(replaced bool) *fakeHostStream {
	st := &fakeHostStream{replaced: replaced, done: make(chan struct{}), wake: make(chan struct{})}
	if replaced {
		close(st.done)
	}
	return st
}

func (s *fakeHostStream) Hello(_ context.Context, _ hostproto.HeartbeatRequest, _ time.Time,
) (hostproto.HeartbeatResponse, error) {
	if s.revoked {
		return hostproto.HeartbeatResponse{}, persistmodel.ErrNotFound
	}
	return hostproto.HeartbeatResponse{}, nil
}
func (s *fakeHostStream) Wait(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-s.wake:
		return true
	}
}
func (s *fakeHostStream) Claim(context.Context, time.Time) ([]hostproto.Dispatch, error) {
	return nil, nil
}
func (s *fakeHostStream) Done() <-chan struct{} { return s.done }
func (s *fakeHostStream) Replaced() bool        { return s.replaced }
func (s *fakeHostStream) Close() {
	s.closeOne.Do(func() {})
}

func (f *fakeHostDaemon) BeginStream(
	_ context.Context, _ persistmodel.Host, req hostproto.HeartbeatRequest, _ time.Time,
) (hostsvc.StreamSession, hostproto.HeartbeatResponse, error) {
	f.beginCalls = append(f.beginCalls, req)
	if f.beginErr != nil {
		return nil, hostproto.HeartbeatResponse{}, f.beginErr
	}
	return newFakeHostStream(f.replacedStream), hostproto.HeartbeatResponse{
		Calls: []hostproto.Dispatch{{ID: "htc_ws1", Seq: req.LastSeq + 1, Kind: hostproto.KindExec}},
	}, nil
}

func (f *fakeHostDaemon) ListMyHosts(context.Context, string) ([]persistmodel.Host, error) {
	return nil, nil
}
func (f *fakeHostDaemon) DeleteHost(context.Context, string, string) error { return nil }
func (f *fakeHostDaemon) BindHost(context.Context, string, string, string, time.Time) error {
	return nil
}
func (f *fakeHostDaemon) ListWorkspaceHosts(context.Context, string) ([]persistmodel.Host, error) {
	return nil, nil
}
func (f *fakeHostDaemon) UnbindHost(context.Context, string, string) error { return nil }

func newHostDaemonHandler(t *testing.T, daemon HostDaemonServices) http.Handler {
	t.Helper()
	h := &Handler{
		hostDaemon: daemon,
		// The WS path also logs on normal branches such as handshake failure / result handling
		// failure, so building Handler directly needs a discard logger (production wiring goes
		// through NewHandler, which always sets one).
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	mux := http.NewServeMux()
	// Same as the httpd wiring: register goes on the normal mux (registered directly here without
	// auth, since auth is not what this group tests); heartbeat/results/ws use the self-authenticating
	// methods on the bare mux.
	h.registerHostDaemonRegisterRoute(mux)
	h.RegisterHostDaemon(mux)
	return mux
}

func TestHostDaemonHeartbeatAuth(t *testing.T) {
	daemon := &fakeHostDaemon{authedHostID: "hst_a"}
	mux := newHostDaemonHandler(t, daemon)

	cases := []struct {
		name   string
		auth   string
		status int
	}{
		{"无 Authorization 头", "", http.StatusUnauthorized},
		{"不是 Bearer", "Token abc", http.StatusUnauthorized},
		{"Bearer 空 token", "Bearer ", http.StatusUnauthorized},
		{"错误 token", "Bearer nope", http.StatusUnauthorized},
		{"正确 token", "Bearer good-token", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/host-daemon/heartbeat", strings.NewReader(`{"lastSeq":0}`))
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("心跳鉴权状态 = %d，期望 %d", rec.Code, tc.status)
			}
			if tc.status == http.StatusOK {
				var resp hostproto.HeartbeatResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Fatalf("解析心跳响应失败: %v", err)
				}
				if len(resp.Calls) != 1 || resp.Calls[0].ID != "htc_1" {
					t.Fatalf("心跳响应内容不对: %+v", resp.Calls)
				}
			}
		})
	}
}

// Result reports must use the host authenticated from the token; a different hostId claimed in
// the body must be overwritten. This stops someone from using host A's token to return host B's
// call results (cross-host mix-up).
func TestHostDaemonResultsIgnoreBodyHostID(t *testing.T) {
	daemon := &fakeHostDaemon{authedHostID: "hst_a"}
	mux := newHostDaemonHandler(t, daemon)

	body := `{"hostId":"hst_OTHER","execResults":[{"callId":"htc_x","result":{"exitCode":0}}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/host-daemon/results", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer good-token")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("结果上报状态 = %d，期望 204", rec.Code)
	}
	if len(daemon.results) != 1 {
		t.Fatalf("应收到 1 条结果，实际 %d", len(daemon.results))
	}
}

func TestHostDaemonResultsRejectsBadToken(t *testing.T) {
	daemon := &fakeHostDaemon{authedHostID: "hst_a"}
	mux := newHostDaemonHandler(t, daemon)
	req := httptest.NewRequest(http.MethodPost, "/api/host-daemon/results", strings.NewReader(`{}`))
	// Deliberately no Authorization.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无令牌结果上报应 401，实际 %d", rec.Code)
	}
}

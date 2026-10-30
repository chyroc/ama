// Tests for the incoming logid.
//
// They lock down "the **source** of the pass-through really has a value". It is worth testing on its
// own because the logid chain has a particularly hidden failure mode: **the outgoing pass-through is
// written perfectly, but nobody ever puts the logid into ctx**. Then the pass-through code never
// gets a value, while the whole test suite can be green (the outgoing tests put it into ctx
// themselves). This is exactly the kind of "new code all green but never took effect" this
// repository has hit again and again.

package httpd

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/shared/gopkg/requestid"
)

// ⭐ Every request must get a logid, written back into the response header.
//
// It is written back so users/the frontend can give this id directly when reporting a problem; a
// past case of "a user reported a logid" was hard to track down partly because we never generated
// one on our side.
func TestEveryRequestGetsALogID(t *testing.T) {
	srv := newTestServer(t)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	got := rec.Header().Get(requestid.Header)
	if got == "" {
		t.Fatal("响应头里应带上 logid，否则用户报障时没有可给的 id")
	}
	if got == "-" {
		t.Error(`不能是 "-"：那是「取不到」的占位，不是真 logid`)
	}
	if len(got) < 8 {
		t.Errorf("logid 太短，不像真的：%q", got)
	}
}

// ⭐ It can really be read from ctx: this is the **precondition** of the outgoing pass-through.
//
// Checking the response header is not enough: the header can be right while ctx is missing it, and
// the outgoing side still gets nothing.
func TestLogIDReachesHandlerContext(t *testing.T) {
	var fromCtx string
	var ok bool
	h := withLogID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fromCtx, ok = requestid.From(r.Context())
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if !ok || fromCtx == "" {
		t.Fatal("handler 的 ctx 里应当能取到 logid——取不到则出站透传永远是空的")
	}
	// Must be the same id as the one written back, otherwise each side looks up a different id.
	if h := rec.Header().Get(requestid.Header); h != fromCtx {
		t.Errorf("响应头与 ctx 里的应当一致：header=%q ctx=%q", h, fromCtx)
	}
}

// When upstream (gateway/caller) sent one, reuse it instead of making another.
//
// Making one on each side is as good as none: the same call has two ids on the two sides, which is
// exactly the problem it is meant to solve.
func TestUpstreamLogIDIsReused(t *testing.T) {
	const upstream = "0217879120240-from-gateway"

	var fromCtx string
	h := withLogID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fromCtx, _ = requestid.From(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(requestid.Header, upstream)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if fromCtx != upstream {
		t.Errorf("应复用上游的 logid：want %q, got %q", upstream, fromCtx)
	}
}

// Each request's logid is different; otherwise you cannot filter out "this one request" by it.
func TestLogIDsAreUnitPerRequest(t *testing.T) {
	h := withLogID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
		id := rec.Header().Get(requestid.Header)
		if seen[id] {
			t.Fatalf("两个请求拿到了同一个 logid %q——那就没法筛出单个请求了", id)
		}
		seen[id] = true
	}
}

// ⭐ The "request finished" log line must carry the logid.
//
// This pins **the middleware order**: `withLogID` must be further out than `logRequest`, otherwise
// ctx has no logid yet when `logRequest` logs with `InfoContext`.
//
// ⚠️ Getting the order backwards is extremely hidden: **the response header still has the logid**
// (withLogID still ran), only the "request finished" line lacks it, and that line is exactly the
// first one searched when debugging with a logid. So "the logid the user gave matches no request",
// which looks like the logs were not collected. This hole was found by mutation testing (the tests
// that only check the response header could not catch it).
func TestRequestCompletedLogCarriesLogID(t *testing.T) {
	var buf bytes.Buffer
	// Observe with a handler that copies the logid from ctx into an attribute: on the real path
	// xlog.NewHandler reads it from ctx; here an equivalent check is used: does ctx actually have
	// a value when logRequest is called.
	logger := slog.New(&ctxLogIDHandler{w: &buf})

	srv := newTestServer(t)
	srv.log = logger

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/feishu/apps", nil))

	out := buf.String()
	if !strings.Contains(out, "请求完成") {
		t.Fatalf("应当打出「请求完成」，实际 %q", out)
	}
	if strings.Contains(out, "logid=<无>") {
		t.Errorf("「请求完成」那行没有 logid —— withLogID 必须比 logRequest 更靠外。\n实际：%s", out)
	}
	// The same id as the response header.
	if id := rec.Header().Get(requestid.Header); id != "" && !strings.Contains(out, id) {
		t.Errorf("日志里的 logid 与响应头不一致：header=%q 日志=%s", id, out)
	}
}

// ⭐ The "request finished" line must carry host: it is the only way to tell "which port the
// request came in on".
//
// This is not about pretty logs. In that 260-second crash loop, `/healthz` was never hit, but
// "request finished" appeared once (`path=/debug/pprof/dynconf status=200`), proving **the process
// was reachable**. But it **could not be settled** then: the line had no `Host`, so "which port the
// incoming request used" was unknown, and "is that the port the probe should use" could not be
// answered.
//
// On that platform each pod listened on a different port (assigned by the platform), while the
// service declaration said 8080; that mismatch was the main suspect of that incident, and `Host` is
// exactly the field that tells at a glance. Removing it would bring the blind spot back, so it is
// pinned.
func TestRequestCompletedLogCarriesHost(t *testing.T) {
	var buf bytes.Buffer
	srv := newTestServerWithLogger(t, slog.New(slog.NewTextHandler(&buf, nil)))

	req := httptest.NewRequest(http.MethodGet, "/api/feishu/apps", nil)
	// Simulate a probe/inspection hitting a pod-specific port.
	req.Host = "10.1.2.3:10125"
	srv.Handler().ServeHTTP(httptest.NewRecorder(), req)

	out := buf.String()
	if !strings.Contains(out, "请求完成") {
		t.Fatalf("应当打出「请求完成」，实际 %q", out)
	}
	if !strings.Contains(out, "10125") {
		t.Errorf("「请求完成」那行没带 host —— 那样就判不出请求从哪个端口进来，\n"+
			"而那正是 260 秒崩溃循环当时卡住的地方。实际：%s", out)
	}
}

// ctxLogIDHandler writes out the message of each log line and the logid in ctx.
type ctxLogIDHandler struct{ w *bytes.Buffer }

func (h *ctxLogIDHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *ctxLogIDHandler) Handle(ctx context.Context, r slog.Record) error {
	id, ok := requestid.From(ctx)
	if !ok || id == "" {
		id = "<无>"
	}
	h.w.WriteString(r.Message + " logid=" + id + "\n")
	return nil
}
func (h *ctxLogIDHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *ctxLogIDHandler) WithGroup(string) slog.Handler      { return h }

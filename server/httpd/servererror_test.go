// Tests for the invariant "5xx ⇒ there is always an Error line in the log".
//
// It deserves its own file because this invariant fails with **zero signal**: the endpoint returns
// 500 as usual, the frontend shows the error as usual, and only the person debugging searches the
// logs for `_level=Error` and gets 0 lines. Then they think "there is no error, so it is not a
// server problem" and look in completely the wrong direction. That is what happened once with a
// page in production.
//
// ⚠️ What is asserted here is **the safety net at the middleware layer**, not some handler
// remembering to log. Testing only "core's fail logs" is not enough: that path does not cover call
// sites that call failWith(500) directly, nor any 5xx that does not go through core.

package httpd

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// errorLogCapture collects log text for assertions.
func errorLogCapture() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return log, &buf
}

// runThroughLogRequest runs a handler once wrapped in the real logRequest middleware.
//
// **It deliberately goes through the real middleware instead of calling logServerError
// directly**: the whole value of this invariant is "every response goes through it"; calling that
// function directly, the test would stay green if someone removed the middleware from the chain.
func runThroughLogRequest(t *testing.T, log *slog.Logger, h http.HandlerFunc, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	srv := &Server{cfg: Config{Addr: ":0"}, log: log}
	rec := httptest.NewRecorder()
	srv.logRequest(h).ServeHTTP(rec, req)
	return rec
}

// ⭐ Every 5xx must leave one Error log line.
//
// This is the core assertion of this file; the others are its boundaries.
func TestServerErrorAlwaysLogsError(t *testing.T) {
	log, buf := errorLogCapture()

	runThroughLogRequest(t, log, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"服务内部错误，请稍后重试","code":"internal"}`))
	}, httptest.NewRequest(http.MethodGet, "/api/workspaces/ws_x/schedules", nil))

	out := buf.String()
	if !strings.Contains(out, "level=ERROR") {
		t.Fatalf("500 必须留一条 Error 日志，否则排查时 _level=Error 搜出 0 条，实际输出：%s", out)
	}
	// All four things needed to find the cause must be there.
	for _, want := range []string{
		"status=500",
		"/api/workspaces/ws_x/schedules",
		"code=internal", // the error code is worth more for finding the cause than the status code
		"method=GET",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Error 日志里缺 %q，排查时就要多猜一步。实际输出：%s", want, out)
		}
	}
	if !strings.Contains(out, "cost=") {
		t.Errorf("缺耗时，分不出「快速失败」与「超时后失败」。实际输出：%s", out)
	}
}

// ⭐ 4xx is deliberately not logged as Error.
//
// 403/404 are daily traffic (permission boundaries, frontend probing, scanners). Logging them as
// Error would make "search for Error" meaningless, and that is exactly what this log line is for.
func TestClientErrorDoesNotLogError(t *testing.T) {
	log, buf := errorLogCapture()

	runThroughLogRequest(t, log, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"没有权限","code":"access_denied"}`))
	}, httptest.NewRequest(http.MethodGet, "/api/workspaces/ws_x", nil))

	if strings.Contains(buf.String(), "level=ERROR") {
		t.Errorf("403 不该打 Error，否则日常流量把真事故淹掉。实际输出：%s", buf.String())
	}
	// But it must still leave a trace in the access log.
	if !strings.Contains(buf.String(), "status=403") {
		t.Errorf("403 仍应出现在「请求完成」那条 Info 里。实际输出：%s", buf.String())
	}
}

// ⭐ /healthz returning 5xx must also log an Error.
//
// The access log has a `return` for /healthz (the probe hits every 10 seconds; logging all of them
// floods the logs). **That return must only apply to successful probes**: a failing probe is
// exactly what most needs a trace. When pods are killed again and again, "did the probe fail or
// not" is the first question to answer.
//
// The easy mistake is putting logServerError after that return, which makes a failing health check
// the only kind of 5xx in the whole chain with zero logs.
func TestHealthzServerErrorStillLogs(t *testing.T) {
	log, buf := errorLogCapture()

	runThroughLogRequest(t, log, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if !strings.Contains(buf.String(), "level=ERROR") {
		t.Fatalf("/healthz 回 503 必须留 Error——探针失败是排查 pod 反复重启的第一个判据。实际输出：%s", buf.String())
	}
}

// ⭐ **The query never goes into the log.**
//
// This is a security assertion, not a format assertion. The frontend once really put the identity
// into `?actor=` (see the comment in web/src/data/http.ts), and any "log the full URL" approach
// would carry it away. Using r.URL.Path here excludes the query **by structure**, not by
// "remembering to strip it".
//
// Mutation check: replace `r.URL.Path` with `r.URL.String()` in logServerError and this test turns red.
func TestServerErrorLogOmitsQuery(t *testing.T) {
	log, buf := errorLogCapture()

	runThroughLogRequest(t, log, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}, httptest.NewRequest(http.MethodGet,
		"/api/runs?actor=someone%40example.org&token=amak_live_do_not_log_me&access_token=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.abcd", nil))

	out := buf.String()
	for _, leaked := range []string{
		"someone", "example.org", // identity
		"amak_live_do_not_log_me", // this repository's own API Key prefix
		"eyJhbGciOiJIUzI1NiJ9",    // JWT
		"actor=", "token=",        // not even the parameter names should appear; that would mean the whole query was logged
	} {
		if strings.Contains(out, leaked) {
			t.Errorf("query 内容 %q 泄漏进日志了，实际输出：%s", leaked, out)
		}
	}
	// The path itself must stay, otherwise this log line is useless.
	if !strings.Contains(out, "/api/runs") {
		t.Errorf("路径应保留，实际输出：%s", out)
	}
}

// ⭐ **The response body does not go into the log; only the code key is picked out.**
//
// The message in an error response is a sentence for users (in Chinese), sometimes with entity
// names, and it is "what the endpoint returned", one of the things README-feedback.md says is never
// collected at either level. The read here is an allow list (decoding into a struct with only a Code
// field), so a field someone adds to error responses in the future does not flow into the log
// automatically.
//
// Mutation check: change parseErrorCode to `return string(body)` and this test turns red.
func TestServerErrorLogOmitsResponseBody(t *testing.T) {
	log, buf := errorLogCapture()

	runThroughLogRequest(t, log, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		// The message deliberately carries something that looks like sensitive data, plus "a field
		// added in the future".
		_, _ = w.Write([]byte(`{"message":"保存失败：用户 zhangsan@example.com 的密钥无效","code":"internal","debugDump":"Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.sig"}`))
	}, httptest.NewRequest(http.MethodPost, "/api/workspaces/ws_x/ark-credential", nil))

	out := buf.String()
	for _, leaked := range []string{
		"zhangsan", "密钥无效", "保存失败", // message text
		"debugDump", "eyJhbGciOiJIUzI1NiJ9", // keys not on the allow list
	} {
		if strings.Contains(out, leaked) {
			t.Errorf("响应体内容 %q 泄漏进日志了——只该挑出 code。实际输出：%s", leaked, out)
		}
	}
	if !strings.Contains(out, "code=internal") {
		t.Errorf("错误码应被记下（判因价值高于状态码），实际输出：%s", out)
	}
}

// ⭐ When code cannot be decoded, no fake value is made up.
//
// The gateway outputs HTML, the response is truncated, it is not a 5xx we wrote at all: in these
// cases the field should be absent entirely, not set to "unknown". Same reason core/observe.go
// forbids fake logids: a fake value is a valid string and would merge with "the error code really
// is called unknown".
func TestServerErrorLogSkipsUnparseableCode(t *testing.T) {
	log, buf := errorLogCapture()

	runThroughLogRequest(t, log, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html><head><title>502 Bad Gateway</title></head></html>"))
	}, httptest.NewRequest(http.MethodGet, "/api/agents", nil))

	out := buf.String()
	if !strings.Contains(out, "level=ERROR") {
		t.Fatalf("502 也是 5xx，必须留 Error。实际输出：%s", out)
	}
	if strings.Contains(out, "code=") {
		t.Errorf("解不出错误码时该整个不出现这个字段，不能兜假值。实际输出：%s", out)
	}
	// HTML from the gateway should not be logged as content either.
	if strings.Contains(out, "Bad Gateway</title>") {
		t.Errorf("响应体不该进日志。实际输出：%s", out)
	}
}

// ⭐ Normal responses are not copied by statusRecorder.
//
// errBody only collects on 5xx. This guards against "adding copy overhead to all normal traffic
// (including every SSE frame) for a field that is only used during failures".
func TestSuccessResponseIsNotBuffered(t *testing.T) {
	srv := &Server{cfg: Config{Addr: ":0"}, log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))}
	rec := httptest.NewRecorder()

	var captured *statusRecorder
	srv.logRequest(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = w.(*statusRecorder)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[{"id":"run_1"}]}`))
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runs", nil))

	if captured == nil {
		t.Fatal("没拿到 recorder")
	}
	if len(captured.errBody) != 0 {
		t.Errorf("2xx 不该攒响应体，实际攒了 %d 字节", len(captured.errBody))
	}
}

// ⭐ The collected bytes have a limit.
//
// A 5xx body is not always our small JSON; collecting without a limit would copy a whole large
// response into memory.
func TestErrorBodySniffIsBounded(t *testing.T) {
	srv := &Server{cfg: Config{Addr: ":0"}, log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))}
	rec := httptest.NewRecorder()

	var captured *statusRecorder
	srv.logRequest(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = w.(*statusRecorder)
		w.WriteHeader(http.StatusInternalServerError)
		for i := 0; i < 100; i++ {
			_, _ = w.Write(bytes.Repeat([]byte("x"), 1024))
		}
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runs", nil))

	if got := len(captured.errBody); got > errBodySniffLimit {
		t.Errorf("攒了 %d 字节，超过上限 %d", got, errBodySniffLimit)
	}
	// The pass-through must not be truncated: the response the user gets must be complete.
	if rec.Body.Len() != 100*1024 {
		t.Errorf("响应体被截断了：期望 %d 字节，实际 %d", 100*1024, rec.Body.Len())
	}
}

// ⭐ A panic takes a different path, but the invariant still holds.
//
// recoverPanic is the **outermost** layer of the whole chain (further out than logRequest), so on a
// panic the stack unwinds all the way to it, and the code after next.ServeHTTP in logRequest never
// runs. In other words logServerError **does not see** the 500 caused by a panic.
//
// This invariant is covered by recoverPanic's own Error log line. This test exists so that "why the
// panic path does not go through logServerError" is clearly recorded, instead of the next person
// thinking they found a hole.
func TestPanicStillLogsError(t *testing.T) {
	log, buf := errorLogCapture()
	srv := &Server{cfg: Config{Addr: ":0"}, log: log}

	rec := httptest.NewRecorder()
	srv.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runs", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("panic 应回 500，实际 %d", rec.Code)
	}
	if !strings.Contains(buf.String(), "level=ERROR") {
		t.Errorf("panic 必须留 Error 日志。实际输出：%s", buf.String())
	}
}

// Unit assertions for parseErrorCode, including a few malformed inputs that really occur.
func TestParseErrorCode(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"正常错误响应", `{"message":"服务内部错误","code":"internal"}`, "internal"},
		{"字段顺序无关", `{"code":"sso_unavailable","message":"x"}`, "sso_unavailable"},
		{"空体", ``, ""},
		{"网关的 HTML", `<html>502</html>`, ""},
		{"被截断的 JSON", `{"message":"很长很长的一段`, ""},
		{"没有 code 键", `{"message":"x"}`, ""},
		{"code 不是字符串", `{"code":123}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseErrorCode([]byte(tc.body)); got != tc.want {
				t.Errorf("parseErrorCode(%q) = %q，期望 %q", tc.body, got, tc.want)
			}
		})
	}
}

// The "code is not a string" case above relies on encoding/json failing as a whole on a type
// mismatch. Pin that precondition too, so it does not silently become "123" if the JSON library is
// ever changed.
func TestJSONTypeMismatchYieldsNoCode(t *testing.T) {
	var payload struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(`{"code":123}`), &payload); err == nil {
		t.Error("期望类型不匹配时报错，这样 parseErrorCode 才会返回空串")
	}
}

// Make sure the duration field in this file is not always zero (otherwise "fast failure vs failure
// after a timeout" cannot be told apart).
func TestServerErrorLogRecordsRealCost(t *testing.T) {
	log, buf := errorLogCapture()

	runThroughLogRequest(t, log, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Millisecond)
		w.WriteHeader(http.StatusInternalServerError)
	}, httptest.NewRequest(http.MethodGet, "/api/runs", nil))

	if strings.Contains(buf.String(), "cost=0s") {
		t.Errorf("耗时不该是 0，实际输出：%s", buf.String())
	}
}

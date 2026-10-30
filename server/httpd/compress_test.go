// Tests for gzip compression.
//
// # The only reason this file exists
//
// The only production failure the gzip middleware could cause is **compressing SSE**, and that
// failure gives no error: the event stream arrives late or hangs entirely, the chat page looks like
// "the agent does not reply", and the logs have nothing. So the focus here is not "does compression
// work", but "is SSE dragged into it".
//
// # ⚠️ The check cannot only look at content-encoding
//
// The intuitive check is "no content-encoding in the SSE response means safe". **Measured, that
// check gives a false green**: after excluding SSE with a content-type filter, the response headers
// really have no content-encoding, but **the stream still hangs for 3 seconds**, because gzhttp's
// WriteHeader only stores the status code without sending it, and its Flush() does not even call
// the underlying Flush when the buffer is empty, so the Flush at the start of the handler that
// "sends the headers out" is swallowed.
//
// Measurements of two filter styles (details in the file header of compress.go):
//
//	allow list (SSE not on it)   headers arrive after=3s   content-encoding=""
//	ExceptContentTypes           headers arrive after=3s   content-encoding=""
//
// So the core case below, TestSSEResponseIsNeverCompressed, asserts **how long the headers take to
// arrive**, and must use **a real socket** (httptest.NewServer) instead of httptest.NewRecorder: a
// Recorder has no notion of "when the headers reach the client", and a test written with it would
// always be green.

package httpd

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sseHoldDuration is how long the test SSE handler waits "after sending the headers before the
// first frame".
//
// This value gives the whole test its resolving power: if the middleware buffers the headers, the
// client waits this much longer. 2 seconds is far above local scheduling jitter while keeping the
// whole case within seconds.
const sseHoldDuration = 2 * time.Second

// newSSEHandler reproduces the **timing** of this repository's real SSE handlers: first
// WriteHeader + Flush to send the headers (the frontend relies on this to fire
// EventSource.onopen), then wait for events. core/handler.go and core/sandboxterm.go both have this
// shape.
//
// The timing is the very thing under test, so it cannot be simplified to "write everything at
// once": then buffered and unbuffered would look exactly the same and the case would be useless.
func newSSEHandler(started chan<- struct{}) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "不支持流式响应", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		close(started)
		time.Sleep(sseHoldDuration)
		_, _ = w.Write([]byte("data: frame\n\n"))
		flusher.Flush()
	})
}

// TestSSEResponseIsNeverCompressed pins "SSE is never compressed and never buffered".
//
// This is the only place this gzip change could cause a production failure, and it gives no error.
//
// There are two checks; **the second is the key one**:
//
//  1. No content-encoding in the response headers;
//  2. **The headers must arrive before the first frame**. Check 1 alone misses the case "the
//     filter decided correctly, but the headers are held in the middleware"; see the file header.
func TestSSEResponseIsNeverCompressed(t *testing.T) {
	started := make(chan struct{})
	// Hits an /api/ path, the same branch (backend branch) as the two SSE routes in production.
	srv := httptest.NewServer(routeFrontend(newSSEHandler(started), compressStatic(placeholderFrontend())))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/runs/run_x/events/stream", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	// Say explicitly "I support gzip": without this header the middleware does not step in at all,
	// and the case is always green.
	req.Header.Set("Accept-Encoding", "gzip")

	// DisableCompression stops the transport from adding the header and decompressing
	// automatically; otherwise it would quietly strip content-encoding and the assertion would not
	// see the truth.
	tr := &http.Transport{DisableCompression: true}
	defer tr.CloseIdleConnections()

	start := time.Now()
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	headerLatency := time.Since(start)

	if ce := resp.Header.Get("Content-Encoding"); ce != "" {
		t.Errorf("SSE 响应被压缩了，Content-Encoding=%q，应当为空", ce)
	}

	// Key assertion: the headers must arrive the moment the handler starts sleeping, not at the
	// first frame. Half the hold time (sseHoldDuration/2) is left as margin for scheduling jitter,
	// while still clearly failing a full 2-second wait.
	if headerLatency >= sseHoldDuration/2 {
		t.Errorf("SSE 响应头被缓冲了：等了 %v 才拿到（handler 在发头之后才 sleep %v）。"+
			"这说明压缩中间件吞掉了 handler 的 Flush，前端的 EventSource.onopen 会迟迟不触发，"+
			"且不报任何错", headerLatency.Round(time.Millisecond), sseHoldDuration)
	}

	<-started
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil {
		t.Fatalf("读取事件帧失败: %v", err)
	}
	// The frame must be plain text. If gzipped, it would be binary starting with \x1f\x8b.
	if !strings.HasPrefix(line, "data: ") {
		t.Errorf("事件帧不是明文，实际 %q", line)
	}
}

// TestStaticAssetsAreCompressed checks "what should be compressed really is".
//
// It is the **positive control** of the case above: without it, deleting the middleware entirely
// would keep TestSSEResponseIsNeverCompressed green. An implementation that "compresses nothing"
// passes all SSE assertions, so that case would prove nothing.
func TestStaticAssetsAreCompressed(t *testing.T) {
	// Must be larger than gzhttp's 1024-byte minimum, otherwise it is not compressed. Use
	// compressible repeated content.
	body := strings.Repeat("console.log('hello world');\n", 200)
	js := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		_, _ = w.Write([]byte(body))
	})
	srv := httptest.NewServer(compressStatic(js))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/assets/index-abc.js", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Accept-Encoding", "gzip")
	tr := &http.Transport{DisableCompression: true}
	defer tr.CloseIdleConnections()

	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	if ce := resp.Header.Get("Content-Encoding"); ce != "gzip" {
		t.Errorf("静态 js 没被压缩，Content-Encoding=%q，期望 gzip", ce)
	}
	// Without Vary, a cache in the middle could send the compressed version to clients without gzip
	// support.
	if v := resp.Header.Get("Vary"); !strings.Contains(v, "Accept-Encoding") {
		t.Errorf("缺 Vary: Accept-Encoding，实际 %q", v)
	}
}

// TestAlreadyCompressedAssetsAreNotRecompressed pins "already compressed binaries are not
// compressed again".
//
// woff2 / png are compressed formats themselves; compressing them again only burns CPU and sometimes
// makes them larger. ⚠️ gzhttp's **default** filter does not exclude them (it only blocks video/
// audio/ image/jp and archive-like keywords), so this is blocked by the allow list in compress.go,
// not by the library's default behavior: switch the allow list back to the default filter and this
// case turns red.
func TestAlreadyCompressedAssetsAreNotRecompressed(t *testing.T) {
	// Each type must be larger than 1024 bytes, otherwise "not compressed" might only mean "too
	// small", and the case would give a false green.
	for _, tc := range []struct {
		name        string
		contentType string
	}{
		{"woff2 字体", "font/woff2"},
		{"woff 字体", "font/woff"},
		{"ttf 字体", "font/ttf"},
		{"png 图片", "image/png"},
		{"ico 图标", "image/vnd.microsoft.icon"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				// Use highly compressible content: if it were compressed the difference would be
				// obvious, so the assertion does not lose meaning because "the content does not
				// compress anyway".
				_, _ = w.Write([]byte(strings.Repeat("A", 4096)))
			})
			srv := httptest.NewServer(compressStatic(h))
			defer srv.Close()

			req, err := http.NewRequest(http.MethodGet, srv.URL+"/assets/x", nil)
			if err != nil {
				t.Fatalf("构造请求失败: %v", err)
			}
			req.Header.Set("Accept-Encoding", "gzip")
			tr := &http.Transport{DisableCompression: true}
			defer tr.CloseIdleConnections()

			resp, err := tr.RoundTrip(req)
			if err != nil {
				t.Fatalf("请求失败: %v", err)
			}
			defer resp.Body.Close()

			if ce := resp.Header.Get("Content-Encoding"); ce != "" {
				t.Errorf("%s 被重复压缩了，Content-Encoding=%q", tc.contentType, ce)
			}
		})
	}
}

// TestBackendRoutesBypassCompression pins "the backend branch does not go through the compression
// middleware at all".
//
// This is the **structural guarantee** of SSE safety: not "the filter let SSE through", but "the
// branch SSE lives on has no middleware at all". Move compressStatic from the frontend branch to
// around the whole Handler and this case turns red.
func TestBackendRoutesBypassCompression(t *testing.T) {
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		// Far above the 1024 minimum and highly compressible: if this branch had the middleware, it
		// would certainly be compressed.
		_, _ = w.Write([]byte(`{"items":[` + strings.Repeat(`"x",`, 500) + `"y"]}`))
	})
	srv := httptest.NewServer(routeFrontend(backend, compressStatic(placeholderFrontend())))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/workspaces", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Accept-Encoding", "gzip")
	tr := &http.Transport{DisableCompression: true}
	defer tr.CloseIdleConnections()

	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	if ce := resp.Header.Get("Content-Encoding"); ce != "" {
		t.Errorf("后端路由被压缩了，Content-Encoding=%q。"+
			"压缩中间件应当只挂在前端静态资源那一支上（SSE 全在 /api/ 下）", ce)
	}
}

// TestCompressionPreservesFlusherAndHijacker pins "Flush / Hijack are still there after wrapping".
//
// The same kind of problem as the two statusRecorder methods: any middleware that wraps the
// ResponseWriter may hide the underlying http.Flusher / http.Hijacker, and the symptoms do not
// point to the wrapper at all (SSE starts sending in lumps, WebSocket reports "Hijack not
// supported").
func TestCompressionPreservesFlusherAndHijacker(t *testing.T) {
	var gotFlusher, gotHijacker bool
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, gotFlusher = w.(http.Flusher)
		_, gotHijacker = w.(http.Hijacker)
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
	})
	srv := httptest.NewServer(compressStatic(h))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/assets/x.js", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	_, _ = bufio.NewReader(resp.Body).ReadString('\n')

	if !gotFlusher {
		t.Error("包了压缩中间件之后 http.Flusher 断言失败，SSE 会攒一坨才吐")
	}
	if !gotHijacker {
		t.Error("包了压缩中间件之后 http.Hijacker 断言失败，WebSocket 升级不了")
	}
}

// placeholderFrontend is a frontend stub that is never called, only there to wire up routeFrontend.
func placeholderFrontend() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!doctype html><title>stub</title>"))
	})
}

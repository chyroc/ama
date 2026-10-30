// Regression tests for "a long-lived SSE connection must not be cut off after a fixed duration".
//
// # What these tests guard
//
// The SDK's NewClientConfig comes with defaultTimeout = 10 minutes, and StreamSessionEvents uses
// config.HTTPClient. Without an explicit override, any chat longer than 10 minutes gets cut off
// by http.Client.Timeout, reporting
// `context deadline exceeded (Client.Timeout or ... while reading body)`.
// This really fired in production many times, every one lasting exactly 10m0.00x s.
//
// A unit test cannot wait 10 minutes, so here the scale is shrunk while the mechanism stays the
// same: give ordinary requests a very short timeout, then have the stand-in server keep sending
// frames over a window far longer than that timeout. The mechanism is identical: in both cases
// http.Client.Timeout covers the total time of "reading the body", regardless of the value.
//
// TestStreamOutlivesNonStreamTimeout is the positive proof,
// TestStreamDiesWhenClientTimeoutApplies is the negative control: it runs the same server with
// the old "only one client" shape and must fail. Without the control, the positive test might
// pass falsely after the bug comes back, just because it ran too fast.

package ma

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/volcengine/ark-runtime-go/arkruntime"
)

const (
	// slowFrameCount / slowFrameGap set the total stream duration (about 8×60ms≈480ms).
	// It must be clearly larger than shortTimeout, or cut-off vs not-cut-off cannot be told apart.
	slowFrameCount = 8
	slowFrameGap   = 60 * time.Millisecond

	// shortTimeout plays the role of the 10-minute default in production: short enough to wait
	// for in a unit test, yet far smaller than the total stream duration.
	shortTimeout = 120 * time.Millisecond
)

// newSlowSSEServer starts a slow SSE stand-in: one frame every slowFrameGap,
// ending with a session.status_idle.
//
// It stops at once when the client disconnects (via r.Context()); otherwise the handler would
// still be writing after the test ends, and -race would report concurrent use of
// http.ResponseWriter.
func newSlowSSEServer(t *testing.T) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/sessions/s1/events/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		for i := 0; i < slowFrameCount; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(slowFrameGap):
			}
			_, _ = fmt.Fprintf(w,
				"data: {\"type\":\"agent.message\",\"content\":[{\"type\":\"text\",\"text\":\"块%d\"}]}\n\n", i)
			if flusher != nil {
				flusher.Flush()
			}
		}
		_, _ = fmt.Fprint(w, "data: {\"type\":\"session.status_idle\"}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// drainStream reads until the stream ends and returns the number of frames received and the
// error.
func drainStream(ctx context.Context, ins *Instance, sessionID string) (int, error) {
	dec, err := ins.Sessions().StreamEvents(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	defer func() { _ = dec.Close() }()

	n := 0
	for dec.Next() {
		n++
	}
	return n, dec.Err()
}

// Positive: with an instance configured WithTimeout, the stream can still read the whole window,
// which is far longer than that timeout.
//
// This is the minimal reproduction of the production bug: before the fix the stream broke at
// shortTimeout.
func TestStreamOutlivesNonStreamTimeout(t *testing.T) {
	srv := newSlowSSEServer(t)

	ins, err := New("dummy-key", WithBaseURL(srv.URL), WithTimeout(shortTimeout))
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}

	// Give ctx plenty of room: this test must prove "it is not http.Client.Timeout cutting it",
	// so ctx must not become the new bottleneck.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	got, err := drainStream(ctx, ins, "s1")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("流不该出错，实际: %v（读了 %d 帧，耗时 %v）", err, got, elapsed)
	}
	// slowFrameCount content frames + 1 status_idle.
	if want := slowFrameCount + 1; got != want {
		t.Fatalf("应收到 %d 帧，实际 %d 帧（耗时 %v）", want, got, elapsed)
	}
	// It really lived past the timeout point, rather than passing falsely because the server ran
	// too fast.
	if elapsed <= shortTimeout {
		t.Fatalf("流只跑了 %v，没有超过 shortTimeout=%v，测不出被切断与否；"+
			"请调大 slowFrameCount/slowFrameGap", elapsed, shortTimeout)
	}
}

// Negative control: run the old shape, "the stream and ordinary requests share one client with a
// Timeout", and it must break at the timeout point.
//
// This test proves the one above is not a false pass: same stand-in server, same reading logic,
// only the client's Timeout differs, so the result must differ. It also covers the warning in
// the NewWithClient comment (on the single-client path, Timeout becomes the stream's limit).
func TestStreamDiesWhenClientTimeoutApplies(t *testing.T) {
	srv := newSlowSSEServer(t)

	// NewWithClient has only one client, and SSE uses it too: exactly the shape before the fix.
	client := arkruntime.NewClientWithApiKey("dummy-key",
		arkruntime.WithBaseUrl(srv.URL),
		arkruntime.WithTimeout(shortTimeout))
	ins, err := NewWithClient(client)
	if err != nil {
		t.Fatalf("NewWithClient 失败: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	got, err := drainStream(ctx, ins, "s1")
	if err == nil {
		t.Fatalf("带 Timeout 的单 client 应当把流切断，却读满了 %d 帧——"+
			"说明这组测试已经测不到 http.Client.Timeout 的效果了", got)
	}
	// This is exactly the production error: Client.Timeout while reading body.
	if !errors.Is(err, os.ErrDeadlineExceeded) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("错误应当是超时类，实际: %v", err)
	}
}

// streamClient's http.Client must really have no Timeout, while the ordinary client keeps it.
//
// This guards the option order in streamConfigOptions: the SDK's WithTimeout writes a field on
// config.HTTPClient, so if it ever comes after WithHTTPClient, it writes the timeout back onto
// the streaming client and the fix silently stops working.
func TestStreamClientHasNoTimeoutButNormalClientDoes(t *testing.T) {
	ins, err := New("dummy-key", WithTimeout(shortTimeout))
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}

	if got := ins.streamClient.HTTPClient().Timeout; got != 0 {
		t.Fatalf("流式 client 的 Timeout 必须是 0，实际 %v", got)
	}
	if got := ins.client.HTTPClient().Timeout; got != shortTimeout {
		t.Fatalf("普通 client 应保留 %v，实际 %v", shortTimeout, got)
	}
}

// Without an explicit WithTimeout, the streaming client must also have no Timeout.
//
// This is exactly what the production failure looked like: core/arkrun.go just calls
// ma.New(key, WithBaseURL(...)) with no WithTimeout at all, and still got the SDK's 10-minute
// default.
func TestStreamClientHasNoTimeoutByDefault(t *testing.T) {
	ins, err := New("dummy-key", WithBaseURL("https://example.invalid"))
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}

	if got := ins.streamClient.HTTPClient().Timeout; got != 0 {
		t.Fatalf("流式 client 的 Timeout 必须是 0，实际 %v", got)
	}
	// Conversely, confirm the SDK default really exists; otherwise the trap this file guards no
	// longer holds, meaning the SDK changed its behavior and the comments and tests must be
	// updated too.
	if got := ins.client.HTTPClient().Timeout; got == 0 {
		t.Fatal("普通 client 竟然没有默认超时：SDK 的 defaultTimeout 行为变了，" +
			"请复核 newStreamHTTPClient 的注释与本文件的前提")
	}
}

// When the caller explicitly injects an httpClient, the streaming client uses it (instead of it
// being silently swapped out).
func TestStreamClientHonorsInjectedHTTPClient(t *testing.T) {
	injected := &http.Client{Timeout: 3 * time.Second}

	ins, err := New("dummy-key", WithHTTPClient(injected))
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}

	if ins.streamClient.HTTPClient() != injected {
		t.Fatal("显式注入的 http.Client 应当也用于流式 client")
	}
	if ins.client.HTTPClient() != injected {
		t.Fatal("显式注入的 http.Client 应当用于普通 client")
	}
}

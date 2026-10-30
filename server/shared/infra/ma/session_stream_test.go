package ma

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Run is the only logic in this package with concurrent timing (open the stream first, send the
// message asynchronously, leave once a terminal event is seen), which compile checks alone cannot
// cover. Here a fake SSE server runs it to verify its behavior.

// newFakeArk starts a minimal Ark stand-in that only implements the two endpoints Run uses.
// frames are the SSE data lines sent on the stream, in order.
func newFakeArk(t *testing.T, frames []string) (*Instance, *int32, *int32) {
	t.Helper()

	var sendCount, streamCount int32

	mux := http.NewServeMux()

	// POST /sessions/{id}/events: receives the user message
	//
	// The response body key is `data`, not `events` (measured: real Ark returns {"data":[...]}).
	// The old SDK did not validate this field, so a wrong key still passed; the self-hosted
	// commit on the SDK main branch made it required, so as soon as the stand-in returned
	// {"events":[]} it failed with `invalid: data (field required)`. The error happens at the
	// send step but looks like an SSE stream problem, which makes it easy to debug in the wrong
	// direction.
	mux.HandleFunc("/sessions/s1/events", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&sendCount, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	})

	// GET /sessions/{id}/events/stream: the long-lived SSE connection
	mux.HandleFunc("/sessions/s1/events/stream", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&streamCount, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, f := range frames {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", f)
			if flusher != nil {
				flusher.Flush()
			}
		}
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	ins, err := New("dummy-key", WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	return ins, &sendCount, &streamCount
}

func TestRunCollectsTextAndStopsOnIdle(t *testing.T) {
	ins, sendCount, streamCount := newFakeArk(t, []string{
		`{"type":"agent.message","content":[{"type":"text","text":"珠穆朗玛峰"}]}`,
		`{"type":"agent.message","content":[{"type":"text","text":"，8848 米。"}]}`,
		`{"type":"session.status_idle"}`,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	res, err := ins.Sessions().Run(ctx, "s1", "最高的山？")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}

	if res.Text != "珠穆朗玛峰，8848 米。" {
		t.Errorf("拼接的回复 = %q", res.Text)
	}
	if res.TerminalType != "session.status_idle" {
		t.Errorf("TerminalType = %q", res.TerminalType)
	}
	if !res.Idle() {
		t.Error("Idle() 应为 true")
	}
	// The event trace should record all 3 frames.
	if len(res.Events) != 3 {
		t.Errorf("Events = %v", res.Events)
	}
	// The stream must be opened before sending the message, and each done only once.
	if got := atomic.LoadInt32(streamCount); got != 1 {
		t.Errorf("开流次数 = %d", got)
	}
	if got := atomic.LoadInt32(sendCount); got != 1 {
		t.Errorf("发送次数 = %d", got)
	}
}

// When terminated or failing, Run must also leave the loop instead of hanging until the context
// times out.
func TestRunStopsOnTerminalVariants(t *testing.T) {
	for _, tc := range []struct {
		frame string
		want  string
	}{
		{`{"type":"session.status_terminated"}`, "session.status_terminated"},
		{`{"type":"session.error"}`, "session.error"},
	} {
		ins, _, _ := newFakeArk(t, []string{tc.frame})
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)

		res, err := ins.Sessions().Run(ctx, "s1", "x")
		cancel()
		if err != nil {
			t.Fatalf("%s: Run 失败: %v", tc.want, err)
		}
		if res.TerminalType != tc.want {
			t.Errorf("TerminalType = %q，期望 %q", res.TerminalType, tc.want)
		}
		if res.Idle() {
			t.Errorf("%s 不该判为 Idle", tc.want)
		}
	}
}

// When the stream ends naturally without a terminal event, Run must not block forever.
func TestRunReturnsWhenStreamEndsWithoutTerminal(t *testing.T) {
	ins, _, _ := newFakeArk(t, []string{
		`{"type":"agent.message","content":[{"type":"text","text":"部分输出"}]}`,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	done := make(chan struct{})
	var res *RunResult
	var err error
	go func() {
		res, err = ins.Sessions().Run(ctx, "s1", "x")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("流结束后 Run 未返回，可能死锁")
	}

	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	// Text already received should be kept; an empty terminal type means no terminal event
	// arrived.
	if res.Text != "部分输出" {
		t.Errorf("Text = %q", res.Text)
	}
	if res.TerminalType != "" {
		t.Errorf("TerminalType 应为空，实际 %q", res.TerminalType)
	}
}

// A send failure must be reported, or "no reply" gets misread as an Agent problem.
func TestRunSurfacesSendError(t *testing.T) {
	var streamOpened int32
	mux := http.NewServeMux()
	mux.HandleFunc("/sessions/s1/events/stream", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&streamOpened, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Keep the connection open until the send side fails and Run wraps up.
		time.Sleep(3 * time.Second)
	})
	// The send side returns 500 to simulate a failed message send.
	mux.HandleFunc("/sessions/s1/events", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom","type":"server_error"}}`))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	ins, err := New("dummy-key", WithBaseURL(srv.URL), WithRetryTimes(0))
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	_, runErr := ins.Sessions().Run(ctx, "s1", "x")
	if runErr == nil {
		t.Fatal("发送失败时 Run 应返回错误")
	}
	if atomic.LoadInt32(&streamOpened) != 1 {
		t.Errorf("开流次数 = %d", atomic.LoadInt32(&streamOpened))
	}
	// The error message should show it is a server 500, not a vague timeout.
	if !strings.Contains(strings.ToLower(runErr.Error()), "boom") &&
		StatusCode(runErr) != http.StatusInternalServerError {
		t.Errorf("错误信息未体现服务端故障: %v", runErr)
	}
}

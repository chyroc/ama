package core

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chyroc/ama/server/repository/dao/coremem"
	"github.com/gorilla/websocket"
)

// This group tests the **dispatch and authorization** of WriteRunSandboxTerminal, which is
// where that earlier failure lived: when a keystroke frame lands on an instance that does not have
// this terminal, does it get sent where it should go.
//
// ⚠️ **Why these tests are a must**: bugs on this path can never be reproduced with a single
// instance (local ./dev.sh has only one process, so every POST always hits), so "all green
// locally" proves nothing for it. Only tests that "explicitly build two instances" can expose the
// problem, which is what the tests below do: the hub not having this terminal == another instance
// received this keystroke.
//
// The two fast-path tests use a **real** WebSocket (an echo server started by httptest):
// terminalSession.write calls gorilla's WriteMessage directly, and a nil placeholder would panic
// inside the library, so the test would fail with a SIGSEGV unrelated to what it is testing. See
// dialTestTerminal.

// fakeRelay records every delivered frame, to assert that "the cross-instance path was really
// taken".
type fakeRelay struct {
	mu        sync.Mutex
	published []relayPublish
	err       error
}

type relayPublish struct {
	topic   string
	payload []byte
	ttl     time.Duration
}

func (f *fakeRelay) Publish(_ context.Context, topic string, payload []byte, ttl time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	// Copy it: if the caller reuses the buffer, keeping the original slice would let the assertion
	// see modified content.
	cp := append([]byte(nil), payload...)
	f.published = append(f.published, relayPublish{topic: topic, payload: cp, ttl: ttl})
	return nil
}

func (f *fakeRelay) Consume(ctx context.Context, _ string, _ func(context.Context, RelayMessage) error) error {
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeRelay) snapshot() []relayPublish {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]relayPublish(nil), f.published...)
}

// newRelayService builds a Service with a relay.
func newRelayService(t *testing.T) (*Service, *fakeRelay) {
	t.Helper()
	store, err := coremem.NewSeededMemStore()
	if err != nil {
		t.Fatalf("装载种子失败：%v", err)
	}
	relay := &fakeRelay{}
	svc, err := New(Config{Store: store, TerminalRelay: relay})
	if err != nil {
		t.Fatalf("装配 Service 失败：%v", err)
	}
	return svc, relay
}

// TestWriteTerminalRelaysWhenNotLocal: when this instance does not have the terminal, the
// keystroke must go into the queue.
//
// **This is exactly the case seen in production**: before the fix these all returned 404 and
// users were typing blind.
func TestWriteTerminalRelaysWhenNotLocal(t *testing.T) {
	svc, relay := newRelayService(t)

	frame := []byte(`{"type":"input","data":"ls"}`)
	if err := svc.WriteRunSandboxTerminal(
		context.Background(), "run_1", "term_notlocal", frame,
	); err != nil {
		t.Fatalf("本实例没有这条终端时应当转发而不是报错，却报了：%v", err)
	}

	got := relay.snapshot()
	if len(got) != 1 {
		t.Fatalf("应当投递 1 帧，实际 %d 帧", len(got))
	}
	if want := "term:term_notlocal"; got[0].topic != want {
		t.Errorf("topic 错了：要 %q，得到 %q", want, got[0].topic)
	}

	// The envelope must carry the runID: **the ownership check relies on it**; see the section in
	// sandboxterm.go.
	var env relayFrame
	if err := json.Unmarshal(got[0].payload, &env); err != nil {
		t.Fatalf("投出去的不是一个合法信封：%v", err)
	}
	if env.RunID != "run_1" {
		t.Errorf("信封里的 runId 错了：要 run_1，得到 %q", env.RunID)
	}
	if string(env.Payload) != string(frame) {
		t.Errorf("按键内容被改了：要 %q，得到 %q", frame, env.Payload)
	}
}

// TestWriteTerminalUsesLocalPathWhenPresent: when this instance holds the terminal it must
// **not** go through the queue.
//
// Without this assertion, "send every keystroke to the queue" would also make the test above
// fully green, and that would make every local keystroke take an extra trip through the database,
// wasting the fast path (p50 0.00ms).
//
// ⚠️ A **real** WebSocket must be attached here: terminalSession.write calls gorilla's
// WriteMessage directly, and a nil upstream panics inside the library (it does not return an
// error). With a nil placeholder, the test would fail with a SIGSEGV unrelated to its purpose.
func TestWriteTerminalUsesLocalPathWhenPresent(t *testing.T) {
	svc, relay := newRelayService(t)

	conn, got := dialTestTerminal(t)

	const termID = "term_local"
	svc.terminals().add(termID, &terminalSession{upstream: conn, done: make(chan struct{})})
	svc.terminals().bind(termID, "run_1")

	frame := []byte(`{"type":"input","data":"ls"}`)
	if err := svc.WriteRunSandboxTerminal(context.Background(), "run_1", termID, frame); err != nil {
		t.Fatalf("本实例持有这条终端时应当直接写，却报了：%v", err)
	}

	// It was really written to that connection; this also blocks "doing nothing is still all green".
	select {
	case data := <-got:
		if string(data) != string(frame) {
			t.Errorf("写进连接的内容不对：要 %q，得到 %q", frame, data)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("等不到写进 WebSocket 的那一帧")
	}

	if got := relay.snapshot(); len(got) != 0 {
		t.Fatalf("本实例持有这条终端时不该投队列，却投了 %d 帧", len(got))
	}
}

// dialTestTerminal starts an echo-only WebSocket server and returns the client connection and
// "what it received".
//
// It uses a real connection instead of stubbing terminalSession: write calls gorilla directly,
// and only a real connection can verify that "the frame was really written out".
func dialTestTerminal(t *testing.T) (*websocket.Conn, <-chan []byte) {
	t.Helper()

	got := make(chan []byte, 4)
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			got <- data
		}
	}))
	t.Cleanup(srv.Close)

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("连测试用 WebSocket 失败：%v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, got
}

// TestWriteTerminalRejectsForeignTerminalLocally: the fast path must block "borrowing someone
// else's terminalId".
//
// Auth checks the runId in the URL, but what actually decides which container gets written is
// the terminalId in the body. Without comparing the two, someone could pass auth with a run they
// have access to, fill in another person's terminalId, and write commands into a container in
// another workspace.
func TestWriteTerminalRejectsForeignTerminalLocally(t *testing.T) {
	svc, relay := newRelayService(t)

	conn, got := dialTestTerminal(t)

	const termID = "term_victim"
	svc.terminals().add(termID, &terminalSession{upstream: conn, done: make(chan struct{})})
	svc.terminals().bind(termID, "run_victim")

	err := svc.WriteRunSandboxTerminal(
		context.Background(), "run_attacker", termID, []byte(`{"type":"input","data":"whoami"}`))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("借用别人的 terminalId 应当回 ErrNotFound，得到：%v", err)
	}

	// ⚠️ It must not be sent to the queue either; that would just bypass the check on another
	// instance.
	if got := relay.snapshot(); len(got) != 0 {
		t.Fatalf("越权的按键不该被转发，却投了 %d 帧", len(got))
	}

	// More importantly: that frame must **never** actually reach the victim's container.
	select {
	case data := <-got:
		t.Fatalf("越权的按键被写进了别人的终端：%q", data)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestWriteTerminalWithoutRelayStillNotFound: with no relay configured, keep the pre-fix
// behavior.
//
// With a single instance (local, unit tests) relay being nil is normal. Then a miss should
// return 404, not panic or pretend to succeed.
func TestWriteTerminalWithoutRelayStillNotFound(t *testing.T) {
	svc, _ := newService(t)

	err := svc.WriteRunSandboxTerminal(
		context.Background(), "run_1", "term_missing", []byte(`{"type":"input"}`))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("没配中转且本实例没有这条终端时应回 ErrNotFound，得到：%v", err)
	}
}

// TestWriteTerminalRejectsEmptyTerminalID: an empty terminalId must be rejected as an argument
// error.
//
// Without that it would become the topic "term:", **a public channel shared by every empty id**,
// and keystrokes anyone sends there would be consumed by whatever terminal happens to read it.
func TestWriteTerminalRejectsEmptyTerminalID(t *testing.T) {
	svc, relay := newRelayService(t)

	for _, id := range []string{"", "   "} {
		err := svc.WriteRunSandboxTerminal(context.Background(), "run_1", id, []byte(`{}`))
		if !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("terminalId=%q 应回 ErrInvalidArgument，得到：%v", id, err)
		}
	}
	if got := relay.snapshot(); len(got) != 0 {
		t.Fatalf("空 terminalId 不该投队列，却投了 %d 帧", len(got))
	}
}

// TestTerminalInputTTLIsShort: keystrokes must have a short time-to-live.
//
// Old queued keystrokes flooding into the container after a reconnect are harmful: the user
// would see a string of characters typed half a minute ago suddenly executed. This pins the
// trade-off itself: "better dropped than late".
func TestTerminalInputTTLIsShort(t *testing.T) {
	if terminalInputTTL > time.Minute {
		t.Fatalf("按键 TTL 取到了 %v——积压的按键迟到执行比丢掉更糟", terminalInputTTL)
	}
}

// TestNewTerminalIDIsNotSequential: terminal ids must be random, not an in-process counter.
//
// ⚠️ termID is now a **cross-instance** queue key. If two instances each counted up from a
// millisecond timestamp, a collision would only be a matter of time, and a collision means sending
// one person's keystrokes into another person's terminal.
// This test pins down "do not go back to nextID / timestamps".
//
// ⚠️ **The check is "entropy", not "no duplicates" or "how many characters adjacent ids differ
// by"**. Mutation testing forced this: the earlier version asserted "200 unique + adjacent ids differ
// by at least 4 characters", and after swapping the implementation for
// `fmt.Sprintf("term_%d", time.Now().UnixNano())` it **was still all green**: nanosecond timestamps
// never repeat in a tight loop anyway, and the decimal forms of two adjacent nanosecond values
// easily differ by more than 4 digits.
// That version tested "no collisions", but what we really need here is "cannot be guessed"; the two
// are not the same thing.
//
// The current check: after the fixed prefix it must be hex, and **each position must have shown
// several different characters**. The high digits of a monotonic timestamp barely change (within
// one second only the last few digits move), so "every position changes" is a property only a
// random source has.
func TestNewTerminalIDIsNotSequential(t *testing.T) {
	const n = 200
	seen := make(map[string]struct{}, n)
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id := newTerminalID()
		if _, dup := seen[id]; dup {
			t.Fatalf("第 %d 个 id 撞了：%s", i, id)
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}

	const prefix = "term_"
	body := make([]string, 0, len(ids))
	for _, id := range ids {
		rest, ok := strings.CutPrefix(id, prefix)
		if !ok {
			t.Fatalf("id 少了 %q 前缀：%q", prefix, id)
		}
		if len(rest) != len(body0(ids[0], prefix)) {
			t.Fatalf("id 长度不稳定（时间戳会随位数变长）：%q vs %q", ids[0], id)
		}
		for _, c := range rest {
			if !strings.ContainsRune("0123456789abcdef", c) {
				t.Fatalf("id 里出现了非 hex 字符 %q：%q——时间戳实现会走到这里", c, id)
			}
		}
		body = append(body, rest)
	}

	// Every position must "have changed". The high digits of a monotonic timestamp are constant over
	// the time span of one test, so this blocks it; a random source is uniform at every position.
	for pos := 0; pos < len(body[0]); pos++ {
		distinct := map[byte]struct{}{}
		for _, s := range body {
			distinct[s[pos]] = struct{}{}
		}
		if len(distinct) < 4 {
			t.Fatalf("第 %d 位在 %d 个 id 里只出现了 %d 种字符——这不像随机，像自增/时间戳",
				pos, n, len(distinct))
		}
	}
}

// body0 strips the prefix, only to save writing it again when comparing lengths above.
func body0(id, prefix string) string {
	rest, _ := strings.CutPrefix(id, prefix)
	return rest
}

// TestRelayHandlerRejectsForeignRun: the ownership check on the cross-instance path.
//
// ⚠️ **This one was added later, and mutation testing forced it**: the consumer-side logic used
// to be inlined in StreamRunSandboxTerminal (which needs a real container + a real WebSocket to
// run), so it **had no tests at all**: deleting the whole ownership check left every case green.
//
// It covers the half the fast path cannot: the publishing side does not have the binding, so it
// cannot check; and which instance a request lands on is decided randomly by the load balancer.
// Checking only on the fast path == an attacker can get around it by trying a few more times.
func TestRelayHandlerRejectsForeignRun(t *testing.T) {
	svc, _ := newRelayService(t)

	var got [][]byte
	write := func(p []byte) error {
		got = append(got, append([]byte(nil), p...))
		return nil
	}
	handle := svc.relayHandler("run_victim", write)

	envelope := func(runID string, payload string) RelayMessage {
		b, err := json.Marshal(relayFrame{RunID: runID, Payload: []byte(payload)})
		if err != nil {
			t.Fatalf("造信封失败：%v", err)
		}
		return RelayMessage{Payload: b}
	}

	// Someone else's run: must be dropped.
	if err := handle(context.Background(), envelope("run_attacker", "whoami")); err != nil {
		t.Fatalf("丢弃越权帧不该报错（报错会让它反复重投）：%v", err)
	}
	if len(got) != 0 {
		t.Fatalf("越权的按键被写进了容器：%q", got)
	}

	// Your own run: must be delivered as usual. **This half must be asserted too**; otherwise "drop
	// everything" would also pass the check above, and then the whole relay would be pointless.
	if err := handle(context.Background(), envelope("run_victim", "ls")); err != nil {
		t.Fatalf("正常帧应当被写入：%v", err)
	}
	if len(got) != 1 || string(got[0]) != "ls" {
		t.Fatalf("正常帧应当原样写入，实际 %q", got)
	}
}

// TestRelayHandlerDropsUndecodableFrame: frames that cannot be decoded are dropped and **not
// redelivered**.
//
// Returning an error would leave it in the queue to be redelivered again and again, but a frame of
// bad JSON stays bad no matter how many times it is redelivered. It would become a zombie message
// that never clears and floods the log every time its lease expires.
func TestRelayHandlerDropsUndecodableFrame(t *testing.T) {
	svc, _ := newRelayService(t)

	called := false
	handle := svc.relayHandler("run_1", func([]byte) error {
		called = true
		return nil
	})

	if err := handle(context.Background(), RelayMessage{Payload: []byte("这不是 JSON")}); err != nil {
		t.Fatalf("解不开的帧应当被丢弃而不是报错（报错=反复重投）：%v", err)
	}
	if called {
		t.Fatal("解不开的帧不该被写进容器")
	}
}

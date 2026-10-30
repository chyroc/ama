package channel

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeConversations replays a fixed list of frames when subscribed.
type fakeConversations struct {
	frames    []string
	appendErr error

	mu       sync.Mutex
	appended []json.RawMessage
}

func (f *fakeConversations) CreateChannelConversation(context.Context, string, string, Kind, string) (Run, error) {
	return Run{ID: "run_1"}, nil
}

func (f *fakeConversations) AppendRunEvents(_ context.Context, _ string, raw []json.RawMessage) ([]json.RawMessage, error) {
	if f.appendErr != nil {
		return nil, f.appendErr
	}
	f.mu.Lock()
	f.appended = append(f.appended, raw...)
	f.mu.Unlock()
	return raw, nil
}

func (f *fakeConversations) StreamRunEvents(ctx context.Context, _ string, onFrame func([]byte) error) error {
	for _, fr := range f.frames {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := onFrame([]byte(fr)); err != nil {
			return err
		}
	}
	<-ctx.Done() // Ark keeps the stream open between turns.
	return ctx.Err()
}

func (f *fakeConversations) AgentDisabled(context.Context, string) bool { return false }

func userMsg(context.Context) (json.RawMessage, error) {
	return json.RawMessage(`{"type":"user.message","content":[{"type":"text","text":"hi"}]}`), nil
}

func TestRunTurnCollectsLastSegment(t *testing.T) {
	conv := &fakeConversations{frames: []string{
		`{"type":"agent.message","content":[{"type":"text","text":"let me check"}]}`,
		`{"type":"agent.tool_use","name":"bash"}`,
		`{"type":"session.status_idle","stop_reason":{"type":"requires_action"}}`,
		`{"type":"agent.message","content":[{"type":"text","text":"the answer"}]}`,
		`{"type":"session.status_idle","stop_reason":{"type":"end_turn"}}`,
	}}
	var seen atomic.Int64
	res, err := RunTurn(context.Background(), TurnConfig{
		Conversations: conv, RunID: "run_1", BuildEvent: userMsg,
		OnFrame: func(context.Context, []byte) { seen.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Answer != "the answer" {
		t.Fatalf("answer = %q, want the last segment", res.Answer)
	}
	if res.StreamErr != nil || res.Interrupted || res.FailedUpstream {
		t.Fatalf("unexpected result %+v", res)
	}
	if res.StopReason != "end_turn" || res.Frames != 5 || res.Messages != 2 || seen.Load() != 5 {
		t.Fatalf("unexpected counters %+v seen=%d", res, seen.Load())
	}
	if len(conv.appended) != 1 {
		t.Fatalf("the user message must be sent once, got %d", len(conv.appended))
	}
}

func TestRunTurnFallsBackToTextBeforeToolCall(t *testing.T) {
	conv := &fakeConversations{frames: []string{
		`{"type":"agent.message","content":[{"type":"text","text":"real answer"}]}`,
		`{"type":"agent.tool_use","name":"bash"}`,
		`{"type":"session.status_idle","stop_reason":{"type":"end_turn"}}`,
	}}
	res, err := RunTurn(context.Background(), TurnConfig{Conversations: conv, RunID: "r", BuildEvent: userMsg})
	if err != nil {
		t.Fatal(err)
	}
	if res.Answer != "real answer" {
		t.Fatalf("a turn ending on a tool call must keep the earlier text, got %q", res.Answer)
	}
}

func TestRunTurnReportsInterruptAndUpstreamFailure(t *testing.T) {
	conv := &fakeConversations{frames: []string{
		`{"type":"user.interrupt"}`,
		`{"type":"session.status_idle","stop_reason":{"type":"end_turn"}}`,
	}}
	res, _ := RunTurn(context.Background(), TurnConfig{Conversations: conv, RunID: "r", BuildEvent: userMsg})
	if !res.Interrupted {
		t.Fatal("interrupt not reported")
	}
	conv = &fakeConversations{frames: []string{`{"type":"session.status_idle","stop_reason":{"type":"retries_exhausted"}}`}}
	res, _ = RunTurn(context.Background(), TurnConfig{Conversations: conv, RunID: "r", BuildEvent: userMsg})
	if !res.FailedUpstream {
		t.Fatal("retries_exhausted not reported")
	}
}

func TestRunTurnSendErrors(t *testing.T) {
	conv := &fakeConversations{appendErr: errors.New("boom")}
	_, err := RunTurn(context.Background(), TurnConfig{Conversations: conv, RunID: "r", BuildEvent: userMsg})
	var se *SendError
	if !errors.As(err, &se) {
		t.Fatalf("append failure must be a SendError, got %v", err)
	}
	_, err = RunTurn(context.Background(), TurnConfig{Conversations: &fakeConversations{}, RunID: "r",
		BuildEvent: func(context.Context) (json.RawMessage, error) { return nil, errors.New("bad") }})
	if !errors.As(err, &se) {
		t.Fatalf("build failure must be a SendError, got %v", err)
	}
}

func TestAnswerBufferPersistsCurrentSegment(t *testing.T) {
	var last string
	a := &AnswerBuffer{Persist: func(s string) { last = s }}
	a.Write("a")
	a.Write("b")
	if last != "ab" {
		t.Fatalf("persisted %q", last)
	}
	a.Reset()
	if last != "" {
		t.Fatal("a reset must persist the empty new segment, not the backup")
	}
	if a.Text() != "ab" {
		t.Fatal("the backup must be returned while the new segment is empty")
	}
	a.Write("c")
	if a.Text() != "c" {
		t.Fatal("the new segment wins once it has text")
	}
}

// ---- Supervisor ----

type fakeSource struct {
	mu       sync.Mutex
	accounts []Account
	started  map[string]int
}

func (f *fakeSource) Name() string { return "fake" }

func (f *fakeSource) ListAccounts(context.Context) ([]Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Account(nil), f.accounts...), nil
}

func (f *fakeSource) RunAccount(ctx context.Context, a Account) error {
	f.mu.Lock()
	f.started[a.Key+"@"+a.Version]++
	f.mu.Unlock()
	<-ctx.Done()
	return nil
}

func (f *fakeSource) set(accounts ...Account) {
	f.mu.Lock()
	f.accounts = accounts
	f.mu.Unlock()
}

func (f *fakeSource) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.started[key]
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSupervisorStartsStopsAndRestartsOnVersion(t *testing.T) {
	src := &fakeSource{started: map[string]int{}}
	src.set(Account{Key: "a", Version: "1"}, Account{Key: "b", Version: "1"})
	sup := NewSupervisor(src, time.Hour, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sup.Reconcile(ctx)
	waitFor(t, func() bool { return src.count("a@1") == 1 && src.count("b@1") == 1 })

	// Same version: nothing restarts.
	sup.Reconcile(ctx)
	if sup.Running() != 2 || src.count("a@1") != 1 {
		t.Fatal("an unchanged account must keep its loop")
	}

	// b removed, a changed.
	src.set(Account{Key: "a", Version: "2"})
	sup.Reconcile(ctx)
	waitFor(t, func() bool { return src.count("a@2") == 1 })
	if sup.Running() != 1 {
		t.Fatalf("running = %d, want 1", sup.Running())
	}

	cancel()
	sup.wg.Wait()
}

// ---- RunWithLease ----

type fakeLeases struct {
	mu   sync.Mutex
	held map[string]bool
}

type fakeLease struct {
	store *fakeLeases
	key   string
}

func (f *fakeLeases) Acquire(_ context.Context, key string, _ time.Duration) (Lease, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.held[key] {
		return nil, false, nil
	}
	f.held[key] = true
	return &fakeLease{store: f, key: key}, true, nil
}

func (l *fakeLease) Renew(context.Context, time.Duration) (bool, error) { return true, nil }

func (l *fakeLease) Release(context.Context) error {
	l.store.mu.Lock()
	delete(l.store.held, l.key)
	l.store.mu.Unlock()
	return nil
}

func TestRunWithLeaseStopReturnsAndReleases(t *testing.T) {
	leases := &fakeLeases{held: map[string]bool{}}
	want := errors.New("credentials rejected")
	err := RunWithLease(context.Background(), leases, "k", nil, func(context.Context) error {
		return Stop(want)
	})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v", err)
	}
	if leases.held["k"] {
		t.Fatal("the lease must be released")
	}
}

func TestRunWithLeaseOnlyOneHolder(t *testing.T) {
	leases := &fakeLeases{held: map[string]bool{}}
	ctx, cancel := context.WithCancel(context.Background())
	var running, peak atomic.Int64
	fn := func(ctx context.Context) error {
		n := running.Add(1)
		if n > peak.Load() {
			peak.Store(n)
		}
		<-ctx.Done()
		running.Add(-1)
		return nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = RunWithLease(ctx, leases, "k", nil, fn) }()
	}
	waitFor(t, func() bool { return running.Load() == 1 })
	time.Sleep(20 * time.Millisecond)
	cancel()
	wg.Wait()
	if peak.Load() != 1 {
		t.Fatalf("peak holders = %d, want 1", peak.Load())
	}
}

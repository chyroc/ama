package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestRunStartsTasksAfterSetupAndClosesInReverseOrder(t *testing.T) {
	var mu sync.Mutex
	var events []string
	add := func(event string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := New(Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	for _, name := range []string{"first", "second"} {
		a.AddModule(ModuleFunc(name, func(ctx context.Context, runtime *Runtime) error {
			add("setup-" + name)
			runtime.OnClose(name, func(context.Context) error { add("close-" + name); return nil })
			runtime.Go(func(ctx context.Context) error {
				mu.Lock()
				if events[0] != "setup-first" || events[1] != "setup-second" || len(events) > 3 {
					t.Errorf("task started before setup completed: %v", events)
				}
				mu.Unlock()
				add("run-" + name)
				<-ctx.Done()
				return nil
			})
			return nil
		}))
	}
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return slices.Contains(events, "run-second") })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"setup-first", "setup-second", "run-first", "run-second", "close-second", "close-first"}
	if !slices.Equal(events[:2], want[:2]) || !slices.Equal(events[4:], want[4:]) ||
		!slices.Contains(events[2:4], "run-first") || !slices.Contains(events[2:4], "run-second") {
		t.Fatalf("events = %v, want setup in order, concurrent tasks, then reverse cleanup", events)
	}
}

func TestSetupFailureCleansOnlyCompletedModules(t *testing.T) {
	var closed []string
	want := errors.New("bad configuration")
	a := New(Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	a.AddModule(ModuleFunc("database", func(context.Context, *Runtime) error {
		return nil
	}))
	// Register the resource in a separate successful module to make the ownership boundary clear.
	a.AddModule(ModuleFunc("pool", func(_ context.Context, runtime *Runtime) error {
		runtime.OnClose("pool", func(context.Context) error { closed = append(closed, "pool"); return nil })
		return nil
	}))
	a.AddModule(ModuleFunc("invalid", func(_ context.Context, runtime *Runtime) error {
		runtime.OnClose("must-not-close", func(context.Context) error {
			closed = append(closed, "must-not-close")
			return nil
		})
		return want
	}))
	if err := a.Run(context.Background()); !errors.Is(err, want) {
		t.Fatalf("got %v, want setup error", err)
	}
	if !slices.Equal(closed, []string{"pool"}) {
		t.Fatalf("closed = %v, want only resources of completed modules", closed)
	}
}

func TestTaskFailureCancelsOthersAndStillCloses(t *testing.T) {
	want := errors.New("worker failed")
	otherStopped := make(chan struct{})
	closed := false
	a := New(Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	a.AddModule(ModuleFunc("test", func(_ context.Context, runtime *Runtime) error {
		runtime.OnClose("resource", func(context.Context) error { closed = true; return nil })
		runtime.Go(func(context.Context) error { return want })
		runtime.Go(func(ctx context.Context) error {
			<-ctx.Done()
			close(otherStopped)
			return nil
		})
		return nil
	}))
	if err := a.Run(context.Background()); !errors.Is(err, want) {
		t.Fatalf("got %v, want task error", err)
	}
	select {
	case <-otherStopped:
	default:
		t.Fatal("task failure did not cancel the other task")
	}
	if !closed {
		t.Fatal("resources were not closed after task failure")
	}
}

func TestCloseFailureIsReturnedAfterAllResourcesClose(t *testing.T) {
	want := errors.New("close failed")
	var closed []string
	a := New(Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	a.AddModule(ModuleFunc("test", func(_ context.Context, runtime *Runtime) error {
		runtime.OnClose("first", func(context.Context) error { closed = append(closed, "first"); return want })
		runtime.OnClose("second", func(context.Context) error { closed = append(closed, "second"); return nil })
		return nil
	}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	cancel()
	if err := <-done; !errors.Is(err, want) {
		t.Fatalf("got %v, want close error", err)
	}
	if !slices.Equal(closed, []string{"second", "first"}) {
		t.Fatalf("closed = %v, want reverse order", closed)
	}
}

func TestRunRejectsSecondInvocation(t *testing.T) {
	a := New(Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.Run(context.Background()); err == nil {
		t.Fatal("second Run must fail")
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition was not met before timeout")
		}
		time.Sleep(time.Millisecond)
	}
}

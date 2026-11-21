package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
)

func TestStoreModuleDefaultsToSharedAndClosesOnShutdown(t *testing.T) {
	closed := false
	ctx, cancel := context.WithCancel(context.Background())
	process := New(Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	process.AddModule(NewStoreModule(StoreModuleConfig{
		OpenShared: func(context.Context) (StoreSelection, error) {
			return StoreSelection{Value: "shared", DSN: "private-dsn", Close: func() { closed = true }}, nil
		},
		OpenMemory: func(context.Context) (StoreSelection, error) {
			t.Fatal("default must not use memory")
			return StoreSelection{}, nil
		},
	}))
	done := make(chan error, 1)
	go func() { done <- process.Run(ctx) }()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !closed {
		t.Fatal("shared store was not closed")
	}
}

func TestStoreModuleFailureDoesNotFallBackToMemory(t *testing.T) {
	want := errors.New("database unavailable")
	process := New(Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	process.AddModule(NewStoreModule(StoreModuleConfig{
		OpenShared: func(context.Context) (StoreSelection, error) { return StoreSelection{}, want },
		OpenMemory: func(context.Context) (StoreSelection, error) {
			t.Fatal("shared failure must not fall back to memory")
			return StoreSelection{}, nil
		},
	}))
	if err := process.Run(context.Background()); !errors.Is(err, want) {
		t.Fatalf("Run error = %v, want shared open error", err)
	}
}

func TestStoreModuleMemoryUsesExplicitFactory(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	process := New(Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	process.AddModule(NewStoreModule(StoreModuleConfig{Memory: true,
		OpenMemory: func(context.Context) (StoreSelection, error) {
			called = true
			return StoreSelection{Value: "memory"}, nil
		},
	}))
	if err := process.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("explicit memory factory was not called")
	}
}

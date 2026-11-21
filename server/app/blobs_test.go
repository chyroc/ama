package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
)

func TestBlobStoreModuleRequiredRejectsMissingStorage(t *testing.T) {
	process := New(Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	process.AddModule(NewBlobStoreModule(BlobStoreModuleConfig{Required: true,
		Open: func(context.Context) (BlobStoreSelection, error) { return BlobStoreSelection{}, nil },
	}))
	if err := process.Run(context.Background()); err == nil {
		t.Fatal("required storage must not silently start without a client")
	}
}

func TestBlobStoreModuleOptionalPreservesFallbackAndCloses(t *testing.T) {
	closed := false
	ctx, cancel := context.WithCancel(context.Background())
	process := New(Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	process.AddModule(NewBlobStoreModule(BlobStoreModuleConfig{
		Open: func(context.Context) (BlobStoreSelection, error) {
			return BlobStoreSelection{Close: func(context.Context) error { closed = true; return nil }}, nil
		},
	}))
	done := make(chan error, 1)
	go func() { done <- process.Run(ctx) }()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !closed {
		t.Fatal("storage cleanup was not called")
	}
}

func TestBlobStoreModuleReturnsFactoryError(t *testing.T) {
	want := errors.New("storage unavailable")
	process := New(Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	process.AddModule(NewBlobStoreModule(BlobStoreModuleConfig{
		Open: func(context.Context) (BlobStoreSelection, error) { return BlobStoreSelection{}, want },
	}))
	if err := process.Run(context.Background()); !errors.Is(err, want) {
		t.Fatalf("Run error = %v, want factory error", err)
	}
}

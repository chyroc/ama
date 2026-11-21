package app

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

type testTask struct{ run func(context.Context) error }

func (t testTask) Run(ctx context.Context) error { return t.run(ctx) }

func TestTaskModuleStartsOnlyAfterEveryModuleIsReady(t *testing.T) {
	ready := false
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	process := New(Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	process.AddModule(NewTaskModule("shared-tasks", nil, testTask{run: func(ctx context.Context) error {
		if !ready {
			t.Error("background task started before later module setup")
		}
		close(started)
		<-ctx.Done()
		return nil
	}}))
	process.AddModule(ModuleFunc("late-wiring", func(context.Context, *Runtime) error {
		ready = true
		return nil
	}))
	done := make(chan error, 1)
	go func() { done <- process.Run(ctx) }()
	<-started
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestTaskModuleRejectsMissingRuntime(t *testing.T) {
	if err := NewTaskModule("test").Setup(context.Background(), nil); err == nil {
		t.Fatal("nil runtime must be rejected instead of panicking")
	}
}

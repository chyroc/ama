package main

import (
	"context"
	"log/slog"
	"testing"

	"github.com/chyroc/ama/server/app"
)

func TestRunUsesAppLifecycle(t *testing.T) {
	oldMain := runProcessMain
	t.Cleanup(func() { runProcessMain = oldMain })
	called := false
	runProcessMain = func(build func(*app.App) error) {
		called = true
		process := app.New(app.Config{Logger: slog.Default()})
		if err := build(process); err != nil {
			t.Error(err)
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := process.Run(ctx); err == nil {
			t.Error("missing config must fail module setup")
		}
	}
	main()
	if !called {
		t.Fatal("main must use the public app entry point")
	}
}

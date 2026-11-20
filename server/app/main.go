package app

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

// Main is the shared entry point of an amad process. build receives an App and adds modules to
// it. The caller's main only needs to pass its module factory to Main; signal handling, exit
// status, startup logging, and cleanup stay consistent across distributions.
//
// Example:
//
//	func main() {
//	    app.Main(func(process *app.App) error {
//	        process.AddModule(app.ModuleFunc("company-runtime", setupCompanyRuntime))
//	        return nil
//	    })
//	}
func Main(build func(*App) error) {
	os.Exit(runMain(build))
}

func runMain(build func(*App) error) int {
	log := slog.Default()
	if build == nil {
		log.Error("app: module factory is required")
		return 1
	}
	a := New(Config{Logger: log})
	if err := build(a); err != nil {
		log.Error("app: build modules", slog.Any("error", err))
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := a.Run(ctx); err != nil {
		log.Error("app: process stopped", slog.Any("error", err))
		return 1
	}
	return 0
}

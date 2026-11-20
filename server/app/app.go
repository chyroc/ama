// Package app assembles an AMA process from explicit modules. It owns startup order, cleanup,
// cancellation of background tasks, and graceful shutdown; it does not choose company-specific
// implementations. A downstream module can use the same lifecycle with different providers.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"golang.org/x/sync/errgroup"
)

// Config supplies the process context and log. Modules are set with AddModule before Run.
type Config struct {
	Logger *slog.Logger
	// ShutdownTimeout bounds cleanup after cancellation. Zero means 10 seconds. Resource Close
	// methods receive this context so a broken dependency cannot keep shutdown blocked forever.
	ShutdownTimeout time.Duration
}

// Module wires one capability. Setup runs sequentially in registration order. A module can call
// OnClose for acquired resources and Go for long-running tasks; both take effect only after Setup
// returns successfully. Returning an error aborts startup and cleans up earlier modules.
type Module interface {
	Name() string
	Setup(ctx context.Context, runtime *Runtime) error
}

// ModuleFunc adapts a setup function to Module. Empty name means "module".
func ModuleFunc(name string, setup func(context.Context, *Runtime) error) Module {
	return moduleFunc{name: name, setup: setup}
}

type moduleFunc struct {
	name  string
	setup func(context.Context, *Runtime) error
}

func (m moduleFunc) Name() string {
	if m.name == "" {
		return "module"
	}
	return m.name
}

func (m moduleFunc) Setup(ctx context.Context, runtime *Runtime) error {
	if m.setup == nil {
		return fmt.Errorf("app: module %s has no setup function", m.Name())
	}
	return m.setup(ctx, runtime)
}

type closeFunc struct {
	name string
	fn   func(context.Context) error
}

// Runtime is valid only during a module's Setup. It collects resources and tasks without running
// them concurrently with startup. It is not a shared mutable registry and must not be retained.
type Runtime struct {
	log      *slog.Logger
	closers  []closeFunc
	services []func(context.Context) error
}

// Logger returns the logger shared by all modules.
func (r *Runtime) Logger() *slog.Logger { return r.log }

// OnClose registers resource cleanup. Resources are closed in reverse registration order.
func (r *Runtime) OnClose(name string, close func(context.Context) error) {
	if close != nil {
		r.closers = append(r.closers, closeFunc{name: name, fn: close})
	}
}

// CloseFunc adapts an io.Closer that does not need a context.
func CloseFunc(name string, closer io.Closer) func(context.Context) error {
	if closer == nil {
		return nil
	}
	return func(context.Context) error { return closer.Close() }
}

// Go registers a long-running task. All tasks start after every module has been set up. If any
// task returns a non-nil error, the others are canceled. A task should return nil after its ctx
// is canceled as part of a normal shutdown.
func (r *Runtime) Go(run func(context.Context) error) {
	if run != nil {
		r.services = append(r.services, run)
	}
}

// App is a process assembled from explicit modules. Build a new App for each process; Run must be
// called at most once.
type App struct {
	log             *slog.Logger
	shutdownTimeout time.Duration
	modules         []Module
	ran             bool
}

// New creates an empty process.
func New(cfg Config) *App {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	timeout := cfg.ShutdownTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &App{log: log, shutdownTimeout: timeout}
}

// AddModule appends a module to the startup sequence.
func (a *App) AddModule(module Module) *App {
	if module != nil {
		a.modules = append(a.modules, module)
	}
	return a
}

// Run sets up modules sequentially, starts their tasks, and waits for cancellation or a task
// failure. It closes all acquired resources on every exit path.
func (a *App) Run(ctx context.Context) error {
	if a.ran {
		return fmt.Errorf("app: Run may only be called once")
	}
	a.ran = true
	if ctx == nil {
		ctx = context.Background()
	}
	var resources []closeFunc
	var services []func(context.Context) error
	for _, module := range a.modules {
		runtime := &Runtime{log: a.log}
		a.log.Info("app module setup", slog.String("module", module.Name()))
		if err := module.Setup(ctx, runtime); err != nil {
			closeErr := closeAll(context.Background(), a.shutdownTimeout, resources)
			return errors.Join(fmt.Errorf("app: setup %s: %w", module.Name(), err), closeErr)
		}
		resources = append(resources, runtime.closers...)
		services = append(services, runtime.services...)
	}

	g, runCtx := errgroup.WithContext(ctx)
	for _, service := range services {
		g.Go(func() error { return service(runCtx) })
	}
	waitErr := g.Wait()
	closeCtx, cancel := context.WithTimeout(context.Background(), a.shutdownTimeout)
	defer cancel()
	closeErr := closeAll(closeCtx, a.shutdownTimeout, resources)
	return errors.Join(waitErr, closeErr)
}

func closeAll(ctx context.Context, timeout time.Duration, resources []closeFunc) error {
	if len(resources) == 0 {
		return nil
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var errs []error
	for i := len(resources) - 1; i >= 0; i-- {
		resource := resources[i]
		if err := resource.fn(ctx); err != nil {
			errs = append(errs, fmt.Errorf("app: close %s: %w", resource.name, err))
		}
	}
	return errors.Join(errs...)
}

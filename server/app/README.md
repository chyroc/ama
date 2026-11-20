# Process modules

`app` is the importable lifecycle used by `cmd/amad`. It lets a downstream distribution keep AMA's
startup and shutdown behavior while supplying its own configuration source, storage, credentials,
or execution environment.

A module has two phases:

1. `Setup` runs sequentially. Acquire resources and register them with `Runtime.OnClose`; register
   long-running tasks with `Runtime.Go`.
2. After **all** modules have been set up, tasks start concurrently with a shared cancellation
   context. Resources close in reverse order on success, setup failure, or task failure.

```go
app.Main(func(process *app.App) error {
    process.AddModule(app.ModuleFunc("company-runtime", func(
        ctx context.Context, runtime *app.Runtime,
    ) error {
        pool, err := openCompanyPool(ctx)
        if err != nil {
            return err
        }
        runtime.OnClose("database", app.CloseFunc("database", pool))
        runtime.Go(func(ctx context.Context) error {
            return serveCompanyHTTP(ctx, pool)
        })
        return nil
    }))
    return nil
})
```

There is no process-global plugin registry: the downstream entry point explicitly chooses and orders
its modules. This matters when multiple teams or multiple deployments share the same executable.

`app.Main` handles SIGINT/SIGTERM and the exit code. `app.New` is the testable form; pass a canceled
context to stop a process without sending a signal to the test runner. A task should return nil
when its context is canceled for normal shutdown.

See `examples/company` for the corresponding configuration and database adapters. That example is
sanitized; private SDKs and deployment-specific configuration stay in the downstream module.

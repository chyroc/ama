package app

import (
	"context"
	"fmt"
	"log/slog"
)

// StoreSelection chooses an explicit in-memory implementation or a shared persistent one.
// Value and DSN are owned by the downstream factory, so private DAO types never enter app.
type StoreSelection struct {
	Value any
	DSN   string
	Close func()
}

// StoreModuleConfig wires the common storage lifecycle. Persistent is used by default;
// a connection or migration failure must not silently turn into process-local memory.
type StoreModuleConfig struct {
	Name       string
	Memory     bool
	OpenMemory func(context.Context) (StoreSelection, error)
	OpenShared func(context.Context) (StoreSelection, error)
	Logger     *slog.Logger
}

// StoreModule opens exactly one configured data layer and registers cleanup on success.
// A failed factory is not registered for cleanup: its own error path must release partial resources.
type StoreModule struct{ cfg StoreModuleConfig }

func NewStoreModule(cfg StoreModuleConfig) *StoreModule { return &StoreModule{cfg: cfg} }

func (m *StoreModule) Name() string {
	if m == nil || m.cfg.Name == "" {
		return "stores"
	}
	return m.cfg.Name
}

func (m *StoreModule) Setup(ctx context.Context, runtime *Runtime) error {
	if m == nil {
		return fmt.Errorf("app: nil store module")
	}
	logger := m.cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	open := m.cfg.OpenShared
	if m.cfg.Memory {
		open = m.cfg.OpenMemory
		logger.Warn("用内存 Store，进程重启数据就没了；多实例下各实例的会话互不相通")
	}
	if open == nil {
		return fmt.Errorf("app: store module %s has no factory for the selected tier", m.Name())
	}
	selection, err := open(ctx)
	if err != nil {
		return fmt.Errorf("app: open stores: %w", err)
	}
	if selection.Value == nil {
		return fmt.Errorf("app: store module %s returned no stores", m.Name())
	}
	if selection.Close != nil {
		runtime.OnClose("stores", func(context.Context) error {
			selection.Close()
			return nil
		})
	}
	return nil
}

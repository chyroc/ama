package app

import (
	"context"
	"fmt"
)

// Task is one resident process loop. A typed nil pointer behind this interface is a
// programming error: an optional implementation must be returned as a literal nil interface,
// not as a non-nil interface containing nil.
type Task interface {
	Run(context.Context) error
}

// TaskModule registers already assembled background tasks. Setup does not run them: App
// starts every task only after all modules finish setup, so a late setter is never missed.
// Nil tasks are skipped deliberately; the downstream keeps its own missing-feature logging.
type TaskModule struct {
	name  string
	tasks []Task
}

func NewTaskModule(name string, tasks ...Task) *TaskModule {
	return &TaskModule{name: name, tasks: tasks}
}

func (m *TaskModule) Name() string {
	if m == nil || m.name == "" {
		return "background-tasks"
	}
	return m.name
}

func (m *TaskModule) Setup(_ context.Context, runtime *Runtime) error {
	if m == nil {
		return nil
	}
	if runtime == nil {
		return fmt.Errorf("app: task module %s has no runtime", m.Name())
	}
	for _, task := range m.tasks {
		if task == nil {
			continue
		}
		// Capture the current entry rather than a later loop variable.
		current := task
		runtime.Go(current.Run)
	}
	return nil
}

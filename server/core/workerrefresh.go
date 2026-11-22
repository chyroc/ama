package core

import (
	"context"
	"log/slog"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// RefreshWorkersHooks supplies workspace and binding reads plus per-environment startup.
type RefreshWorkersHooks struct {
	ListWorkspaces func(context.Context) ([]apimodel.Workspace, error)
	ListBindings   func(context.Context, string) ([]persistmodel.ArkBinding, error)
	Ensure         func(context.Context, string, string, string)
	Logger         *slog.Logger
}

// RefreshWorkers starts one worker per distinct environment, using the agent ID
// from that binding so three-level credentials resolve to the key that created it.
// A binding read failure is logged, not confused with an empty workspace.
func RefreshWorkers(ctx context.Context, hooks RefreshWorkersHooks) {
	logger := hooks.Logger
	if logger == nil {
		logger = slog.Default()
	}
	spaces, err := hooks.ListWorkspaces(ctx)
	if err != nil {
		logger.Warn("worker 池扫描空间失败，下个周期重试", slog.Any("err", err))
		return
	}
	for _, workspace := range spaces {
		bindings, err := hooks.ListBindings(ctx, workspace.ID)
		if err != nil {
			logger.Warn("列空间的方舟绑定失败，跳过该空间，下个周期重试",
				slog.String("workspace", workspace.ID), slog.Any("err", err))
			continue
		}
		seen := map[string]bool{}
		for _, binding := range bindings {
			if binding.ArkEnvironmentID == "" || seen[binding.ArkEnvironmentID] ||
				apimodel.EffectiveArkEnvironment("", binding.EnvironmentType) == apimodel.ArkEnvironmentCloud {
				// Cloud tools run in Ark, so no AMA worker may poll this environment.
				continue
			}
			seen[binding.ArkEnvironmentID] = true
			if hooks.Ensure != nil {
				hooks.Ensure(ctx, workspace.ID, binding.AgentID, binding.ArkEnvironmentID)
			}
		}
	}
}

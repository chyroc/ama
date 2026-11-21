package core

import (
	"context"
	"log/slog"
	"time"

	"github.com/volcengine/ark-runtime-go/arkruntime"
	"github.com/volcengine/ark-runtime-go/arkruntime/selfhosted"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

func (w *RunWatcher) rescueStoppedWork(ctx context.Context, run apimodel.Run, age time.Duration) {
	RunStoppedWorkRescue(ctx, run, age, w.log, RunRescueHooks{
		GetBinding: func(ctx context.Context, run apimodel.Run) (persistmodel.ArkBinding, error) {
			return w.cfg.Service.store.GetArkBinding(ctx, run.WorkspaceID, run.AgentID)
		},
		GetKey: func(ctx context.Context, run apimodel.Run) (ArkBindingKey, error) {
			return w.cfg.Service.arkKey(ctx, run.WorkspaceID, run.AgentID)
		},
		BaseURL:        arkBaseURL,
		FetchWorkState: fetchArkWorkState,
		Audit: func(ctx context.Context, baseURL, apiKey string, logger *slog.Logger) int {
			client := arkruntime.NewClientWithApiKey(apiKey, arkruntime.WithBaseUrl(baseURL))
			rescueCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), toolAuditTimeout)
			defer cancel()
			return auditToolCalls(rescueCtx, selfhosted.NewClientAPI(client), run.ID, logger)
		},
		IncrementCount: func(ctx context.Context, runID string) (int, error) {
			return w.cfg.Service.store.IncrRunRescueCount(ctx, runID)
		},
	})
}

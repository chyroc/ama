package core

import (
	"context"
	"log/slog"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// RescueAfter is the silence required before considering a last-resort rescue.
const RescueAfter = arkWorkServerStopAfter + 10*time.Second

// ShouldRescueStoppedWork permits a missing tool result only after the run has been
// stuck past Ark's stop line and the work itself is confirmed stopped.
func ShouldRescueStoppedWork(status apimodel.RunStatus, age time.Duration, workState string, found bool) bool {
	return shouldRescue(status, age, workState, found)
}

// RescueNeedsHuman reports whether repeated rescues indicate a failed re-dispatch chain.
// An unavailable counter must not manufacture a repeated-rescue incident.
func RescueNeedsHuman(n int, countFailed bool) bool {
	return rescueNeedsHuman(n, countFailed)
}

// RunRescueHooks provides the downstream binding, credential, and audit operations.
// The caller must already own the run's database claim before invoking RunStoppedWorkRescue.
type RunRescueHooks struct {
	GetBinding     func(context.Context, apimodel.Run) (persistmodel.ArkBinding, error)
	GetKey         func(context.Context, apimodel.Run) (ArkBindingKey, error)
	BaseURL        func(apimodel.ArkRegion) (string, error)
	FetchWorkState func(context.Context, string, string, string, string) (string, bool, error)
	Audit          func(context.Context, string, string, *slog.Logger) int
	IncrementCount func(context.Context, string) (int, error)
}

// RunStoppedWorkRescue checks the remote work before posting any missing result.
// Live, queued, and not-found work are left to the normal worker path. A repeated
// successful rescue is logged as an error because it indicates a broken re-dispatch chain.
func RunStoppedWorkRescue(
	ctx context.Context, run apimodel.Run, age time.Duration, logger *slog.Logger, hooks RunRescueHooks,
) {
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With(slog.String("run", run.ID), slog.String("workspace", run.WorkspaceID))
	if hooks.GetBinding == nil || hooks.GetKey == nil || hooks.BaseURL == nil ||
		hooks.FetchWorkState == nil || hooks.Audit == nil {
		logger.Error("work 救援缺少必要装配，本轮不补发工具结果")
		return
	}
	binding, err := hooks.GetBinding(ctx, run)
	if err != nil {
		return
	}
	key, err := hooks.GetKey(ctx, run)
	if err != nil {
		logger.Debug("救援时取不到方舟凭证，跳过", slog.Any("err", err))
		return
	}
	baseURL, err := hooks.BaseURL(key.Region)
	if err != nil {
		logger.Warn("救援时解析方舟地址失败，跳过", slog.Any("err", err))
		return
	}
	state, found, err := hooks.FetchWorkState(ctx, baseURL, key.APIKey, binding.ArkEnvironmentID, run.ID)
	if err != nil {
		logger.Warn("查询 work 状态失败，下一轮再试", slog.Any("err", err))
		return
	}
	if !shouldRescue(run.Status, age, state, found) {
		return
	}
	logger.Warn("work 已被方舟停止却仍卡 requires_action，巡检补发工具结果（最后一道救援）",
		slog.Duration("静默", age.Round(time.Second)),
		slog.String("environment", binding.ArkEnvironmentID))

	rescueN, countErr := 0, error(nil)
	if hooks.IncrementCount != nil {
		rescueN, countErr = hooks.IncrementCount(ctx, run.ID)
	}
	if countErr != nil {
		logger.Warn("自增救援计数失败，本轮仍照常补发", slog.Any("err", countErr))
	}
	posted := hooks.Audit(ctx, baseURL, key.APIKey, logger)
	if posted <= 0 {
		return
	}
	if rescueNeedsHuman(rescueN, countErr != nil) {
		logger.Error("同一条会话已连续多次靠巡检兜底，failover 重派自愈链失效，需要人介入",
			slog.Int("第几次救援", rescueN), slog.Int("本轮补发", posted),
			slog.String("environment", binding.ArkEnvironmentID),
			slog.String("排查方向", "查该环境执行者 pod 在工具准备段（账本/凭据/建容器）是否卡死、是否需要重建实例"))
	} else {
		logger.Warn("巡检补发完成，方舟那一轮可以继续；agent 若重试工具会作为新 work 被真执行",
			slog.Int("补发", posted), slog.Int("第几次救援", rescueN))
	}
}

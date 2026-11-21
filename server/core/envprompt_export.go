package core

import (
	"log/slog"

	"github.com/chyroc/ama/server/model/apimodel"
)

// RenderEnvPrompt renders non-secret environment entries as data inside the
// harness environment block. Secret values stay out of model context, entries that
// could close the block early are skipped, and no empty block is emitted.
func RenderEnvPrompt(envs []apimodel.AgentEnv, logger *slog.Logger) string {
	return renderEnvPrompt(envs, logger)
}

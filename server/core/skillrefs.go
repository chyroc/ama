package core

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

const MaxArkSkillsPerAgent = 20

// SyncArkSkillRefs syncs the agent's skills in order, skipping a failed skill
// without discarding the others. The fingerprint covers only successful uploads,
// so a failed skill is retried instead of being cached as complete.
func SyncArkSkillRefs(
	ctx context.Context, agent apimodel.Agent, precondition error, logger *slog.Logger,
	ensure func(context.Context, string) (persistmodel.ArkSkillUpload, error),
	fingerprint func(map[string]string) string,
) ([]persistmodel.ArkSkillRef, string, error) {
	if len(agent.SkillIDs) == 0 {
		return nil, "", nil
	}
	if len(agent.SkillIDs) > MaxArkSkillsPerAgent {
		return nil, "", fmt.Errorf(
			"%w: 这个 Agent 绑定了 %d 个技能，方舟侧最多 %d 个，请先解绑一些",
			precondition, len(agent.SkillIDs), MaxArkSkillsPerAgent)
	}
	if logger == nil {
		logger = slog.Default()
	}
	refs := make([]persistmodel.ArkSkillRef, 0, len(agent.SkillIDs))
	perSkill := make(map[string]string, len(agent.SkillIDs))
	for _, id := range agent.SkillIDs {
		upload, err := ensure(ctx, id)
		if err != nil {
			logger.Warn("技能同步到方舟失败，这一轮模型不会知道它",
				slog.String("skill", id), slog.Any("err", err))
			continue
		}
		refs = append(refs, upload.Ref())
		perSkill[id] = upload.ContentHash
	}
	return refs, fingerprint(perSkill), nil
}

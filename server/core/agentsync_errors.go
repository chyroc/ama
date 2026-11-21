package core

import (
	"strings"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/shared/infra/ma"
)

// ArkSyncStageOf maps an Ark binding error to the setup step the user can act on.
// Unknown errors stay at the general agent stage instead of pointing to the wrong fix.
func ArkSyncStageOf(err error) apimodel.AgentSyncStage {
	if ma.IsManagedAgentNotOpen(err) {
		return apimodel.AgentSyncStageCredential
	}
	message := err.Error()
	switch {
	case strings.Contains(message, "方舟运行环境"):
		return apimodel.AgentSyncStageEnvironment
	case strings.Contains(message, "API Key"), strings.Contains(message, "方舟 API Key"):
		return apimodel.AgentSyncStageCredential
	case strings.Contains(message, "技能"):
		return apimodel.AgentSyncStageSkills
	default:
		return apimodel.AgentSyncStageAgent
	}
}

// UserMessageOf returns the display part after the last sentinel separator.
func UserMessageOf(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	if i := strings.LastIndex(message, ": "); i >= 0 && i+2 < len(message) {
		return message[i+2:]
	}
	return message
}

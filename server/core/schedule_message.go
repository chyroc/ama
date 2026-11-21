package core

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/chyroc/ama/server/model/apimodel"
)

// ScheduleUserMessage builds the user.message sent for one scheduled fire.
// ContextSummary is included before the prompt because every fire starts a new session
// without history. The caller decides the actual prompt for this round.
func ScheduleUserMessage(sch apimodel.Schedule, prompt string) (json.RawMessage, error) {
	if summary := strings.TrimSpace(sch.ContextSummary); summary != "" {
		prompt = "# 这条任务创建时的上下文\n\n" + summary +
			"\n\n---\n\n# 本次要做的事\n\n" + prompt
	}
	raw, err := json.Marshal(map[string]any{
		"type":    "user.message",
		"content": []map[string]any{{"type": "text", "text": prompt}},
	})
	if err != nil {
		return nil, fmt.Errorf("拼指令失败: %w", err)
	}
	return raw, nil
}

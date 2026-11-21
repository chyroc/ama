package core

import (
	"errors"
	"fmt"

	"github.com/chyroc/ama/server/model/apimodel"
)

// AgentMessageFreezeError returns the user-facing precondition for a disabled or
// deleted agent. A missing agent is not a not-found response: the run still exists,
// but this operation is not allowed. Other storage failures return nil so a database
// blip cannot masquerade as a deleted agent. Interrupts bypass this check at the caller.
func AgentMessageFreezeError(agent apimodel.Agent, err error, notFound, precondition error) error {
	if err == nil && agent.Status.IsDisabled() {
		return fmt.Errorf("%w: 关联的 Agent 已停用，这个会话只能查看历史；要继续对话请先到 Agent 详情页启用它", precondition)
	}
	if err == nil {
		return nil
	}
	if notFound != nil && errors.Is(err, notFound) {
		return fmt.Errorf("%w: 关联的 Agent 已被删除，这个会话只能查看历史，不能再发消息", precondition)
	}
	return nil
}

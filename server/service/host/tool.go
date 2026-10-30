package host

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/shared/hostproto"
	"github.com/chyroc/ama/server/shared/hosttools"
)

// ToolResultScrubber removes plaintext secrets from the tool result text before it is stored. Same meaning as
// ExecResultScrubber (see the comment in service.go), except a native result is just one piece of text.
type ToolResultScrubber func(ctx context.Context, sessionID string, res hosttools.ToolResult) hosttools.ToolResult

// ToolCall runs one native tool call on the given host, blocking until the daemon returns a result / the host goes offline / timeout.
//
// Same shape as Exec: pre-check, enqueue, wait. Failure is also a normal ToolResult{IsError:true},
// not a Go error. A Go error is only for "the call itself is invalid" (host does not exist, bad arguments).
func (s *Service) ToolCall(
	ctx context.Context, hostID, sessionID string, call hosttools.ToolCall,
) (hosttools.ToolResult, error) {
	h, err := s.store.GetHost(ctx, hostID)
	if err != nil {
		return hosttools.ToolResult{}, fmt.Errorf("host: 取宿主机失败: %w", err)
	}
	if !hostSeenRecently(h, timeNow()) {
		return failedToolResult(
			"宿主机离线（心跳超时），工具未执行或执行结果未知；请确认 ama daemon 正在运行"), nil
	}
	// Capability gate: an old daemon only understands the shell-script protocol and cannot run a KindTool.
	// An old daemon that never reported (default false) is also stopped before enqueueing; otherwise it would just wait until timeout.
	if !h.NativeTools {
		return failedToolResult(
			"这台机器上的 ama daemon 版本过旧，不支持原生工具协议；" +
				"请在宿主机升级新版 ama daemon（新版不再需要 brew install coreutils findutils util-linux）后重试"), nil
	}

	payload, err := json.Marshal(call)
	if err != nil {
		return hosttools.ToolResult{}, fmt.Errorf("host: 序列化工具调用失败: %w", err)
	}
	d := persistmodel.HostDispatch{
		ID:        newID(idPrefixCall),
		HostID:    hostID,
		Kind:      hostproto.KindTool,
		State:     "queued",
		SessionID: sessionID,
		Payload:   string(payload),
		CreatedAt: timeNow(),
		UpdatedAt: timeNow(),
	}
	if _, err := s.store.EnqueueDispatch(ctx, d); err != nil {
		return failedToolResult("任务下发到宿主机失败（队列写入错误）"), nil
	}

	settled := s.awaitSettlement(ctx, h.ID, d.ID, 0)
	if !settled.done {
		return failedToolResult(settled.message), nil
	}
	var res hosttools.ToolResult
	if jerr := json.Unmarshal([]byte(settled.result), &res); jerr != nil {
		return failedToolResult("宿主机回传的工具结果无法解析: " + jerr.Error()), nil
	}
	return res, nil
}

func failedToolResult(message string) hosttools.ToolResult {
	return hosttools.ToolResult{IsError: true, Text: message}
}

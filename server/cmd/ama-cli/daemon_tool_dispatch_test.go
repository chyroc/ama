package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/shared/hostproto"
	"github.com/chyroc/ama/server/shared/hosttools"
)

// A KindTool call must go through toolFn, with the result in the ToolResults buffer (not ExecResults).
func TestDispatchKindTool(t *testing.T) {
	rt := &daemonRuntime{
		ledger: newExecLedger(),
		toolFn: func(_ context.Context, cwd string, call hosttools.ToolCall) hosttools.ToolResult {
			if !strings.HasSuffix(cwd, ".ama/runs/ses-1") {
				t.Errorf("cwd 应锚到会话目录，实际 %q", cwd)
			}
			if call.Name != hosttools.NameRead {
				t.Errorf("工具名 = %q", call.Name)
			}
			return hosttools.ToolResult{Text: "native-ok"}
		},
	}
	payload, _ := json.Marshal(hosttools.ToolCall{Name: hosttools.NameRead, Input: json.RawMessage(`{"file_path":"a.txt"}`)})
	rt.dispatch(context.Background(), hostproto.Dispatch{
		ID: "c1", Kind: hostproto.KindTool, SessionID: "ses-1", Payload: payload,
	})

	rt.mu.Lock()
	tools := rt.toolResults
	exec := rt.exec
	rt.mu.Unlock()
	if len(exec) != 0 {
		t.Fatalf("KindTool 结果不该进 exec 缓冲: %+v", exec)
	}
	if len(tools) != 1 || tools[0].CallID != "c1" || tools[0].Result.Text != "native-ok" {
		t.Fatalf("tool 结果不对: %+v", tools)
	}

	// Redelivering the same callID: the ledger hit only returns the cached result, and toolFn must not be called again (verified with a counter).
	calls := 0
	rt.toolFn = func(context.Context, string, hosttools.ToolCall) hosttools.ToolResult {
		calls++
		return hosttools.ToolResult{Text: "should-not-run"}
	}
	rt.dispatch(context.Background(), hostproto.Dispatch{
		ID: "c1", Kind: hostproto.KindTool, SessionID: "ses-1", Payload: payload,
	})
	if calls != 0 {
		t.Fatalf("重投不应再执行 toolFn，实际执行 %d 次", calls)
	}
	rt.mu.Lock()
	tools = rt.toolResults
	rt.mu.Unlock()
	if len(tools) != 2 || tools[1].Result.Text != "native-ok" {
		t.Fatalf("重投应回缓存结果，实际 %+v", tools)
	}
}

// A bad payload must return an IsError tool result instead of crashing.
func TestDispatchKindToolBadPayload(t *testing.T) {
	rt := &daemonRuntime{
		ledger: newExecLedger(),
		toolFn: func(context.Context, string, hosttools.ToolCall) hosttools.ToolResult {
			t.Fatal("坏 payload 不该调 toolFn")
			return hosttools.ToolResult{}
		},
	}
	rt.dispatch(context.Background(), hostproto.Dispatch{
		ID: "c2", Kind: hostproto.KindTool, Payload: json.RawMessage(`{not json`),
	})
	rt.mu.Lock()
	tools := rt.toolResults
	rt.mu.Unlock()
	if len(tools) != 1 || !tools[0].Result.IsError {
		t.Fatalf("坏 payload 应回 IsError 结果: %+v", tools)
	}
}

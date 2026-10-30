// Unit tests for the timedTool timing decorator.
package shelltools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/volcengine/ark-runtime-go/arkruntime/toolset"
)

// The decorator must forward Name as-is: Set uses it as the key, and if the name changes Ark
// gets unknown tool.
func TestTimedToolForwardsName(t *testing.T) {
	for _, tc := range []struct {
		inner toolset.Tool
		want  string
	}{
		{NewBashTool(nil, ""), "bash"},
		{NewReadTool(nil, ""), "read"},
		{NewWriteTool(nil, ""), "write"},
		{NewEditTool(nil, ""), "edit"},
		{NewGlobTool(nil, ""), "glob"},
		{NewGrepTool(nil, ""), "grep"},
	} {
		if got := newTimedTool(tc.inner, quietLog()).Name(); got != tc.want {
			t.Errorf("装饰后的名字 = %q，期望 %q", got, tc.want)
		}
	}
}

// The decorator must hand the result back as-is, including IsError: it decides whether the
// agent sees success or failure.
func TestTimedToolPassesResultThrough(t *testing.T) {
	fe := &fakeExec{reply: []*Result{ok("hi\n")}}
	inner := NewBashTool(fe, "")
	timed := newTimedTool(inner, quietLog())

	raw := json.RawMessage(`{"command":"echo hi"}`)
	direct := inner.Execute(context.Background(), raw)
	viaTimed := timed.Execute(context.Background(), raw)

	if viaTimed.IsError != direct.IsError {
		t.Errorf("IsError 被改写了: 直接调 %v，经装饰器 %v", direct.IsError, viaTimed.IsError)
	}
	if len(viaTimed.Content) != len(direct.Content) {
		t.Fatalf("Content 段数不一致: 直接调 %d，经装饰器 %d",
			len(direct.Content), len(viaTimed.Content))
	}
	for i := range direct.Content {
		if viaTimed.Content[i].Text != direct.Content[i].Text {
			t.Errorf("第 %d 段内容被改写了", i)
		}
	}
}

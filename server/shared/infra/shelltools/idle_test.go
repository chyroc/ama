// Unit tests for the idleHook idle sentinel and NewSet assembly.
package shelltools

import (
	"context"
	"errors"
	"testing"
)

// The sentinel's name must not collide with a tool name Ark really sends.
func TestIdleHookNameIsReserved(t *testing.T) {
	for _, real := range []string{"bash", "read", "write", "edit", "glob", "grep"} {
		if idleHookName == real {
			t.Fatalf("哨兵名字撞上了真实工具名 %q", real)
		}
	}
}

// With no callback, Close must be a no-op without error: not every executor needs to do
// something when idle.
func TestIdleHookNilCallbackIsNoop(t *testing.T) {
	h := newIdleHook(nil)
	if err := h.Close(); err != nil {
		t.Fatalf("nil 回调时 Close 不该报错: %v", err)
	}
}

func TestIdleHookCallsCallback(t *testing.T) {
	called := 0
	h := newIdleHook(func() error { called++; return nil })
	if err := h.Close(); err != nil {
		t.Fatalf("Close 不该报错: %v", err)
	}
	if called != 1 {
		t.Errorf("回调应当被调一次，实际 %d", called)
	}
}

// The error returned by the callback must be passed up as-is: the SDK logs it, and the executor
// uses it to decide how to express an idle failure.
func TestIdleHookPropagatesError(t *testing.T) {
	want := errors.New("暂停失败")
	h := newIdleHook(func() error { return want })
	if got := h.Close(); got != want {
		t.Errorf("Close 应返回回调的错误，got %v want %v", got, want)
	}
}

// Execute should never be called. If it is, the name collided, and it must return an error
// instead of an empty result.
func TestIdleHookExecuteRejectsCall(t *testing.T) {
	h := newIdleHook(nil)
	r := h.Execute(context.Background(), nil)
	if !r.IsError {
		t.Error("哨兵被调用时应当回错误结果")
	}
}

// NewSet must register the six real tools (every name exactly right) and the idle sentinel;
// set.Close() must trigger onIdle once.
//
// ⚠️ This is the one most likely to break silently: the six real tools are all wrapped with the
// timing decorator, but the sentinel alone must not be. The decorator hides Close, and onIdle
// would never be called again.
func TestNewSetRegistersToolsAndFiresOnIdle(t *testing.T) {
	calls := 0
	fe := &fakeExec{reply: []*Result{ok("")}}
	set, err := NewSet(fe, "/work", t.TempDir(), func() error {
		calls++
		return nil
	}, quietLog())
	if err != nil {
		t.Fatalf("NewSet 失败: %v", err)
	}
	for _, name := range []string{"bash", "read", "write", "edit", "glob", "grep"} {
		if !set.Has(name) {
			t.Errorf("工具 %q 不在 Set 里", name)
		}
	}
	if !set.Has(idleHookName) {
		t.Errorf("空闲哨兵 %q 不在 Set 里", idleHookName)
	}

	if err := set.Close(); err != nil {
		t.Fatalf("Set.Close 不该报错: %v", err)
	}
	if calls != 1 {
		t.Errorf("Set.Close 应触发一次 onIdle，实际 %d 次", calls)
	}
}

// When onIdle is nil, Set.Close must also return cleanly.
func TestNewSetNilOnIdleClosesCleanly(t *testing.T) {
	set, err := NewSet(&fakeExec{}, "/work", t.TempDir(), nil, quietLog())
	if err != nil {
		t.Fatalf("NewSet 失败: %v", err)
	}
	if err := set.Close(); err != nil {
		t.Fatalf("nil onIdle 时 Set.Close 不该报错: %v", err)
	}
}

// Tests for error classification.
//
// They watch for "a classifier silently stops working": a wrong classification raises no error;
// it just makes upper layers treat one failure as another, and the user sees a message that has
// nothing to do with what happened.

package ma

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

// TestIsRequiresActionRejected matches Ark's own code, not the HTTP status code.
//
// # Why it must match by code and not by 400
//
// With self_hosted, tools run in our own containers, and there is a window of tens of
// milliseconds between Ark sending tool_use and the worker returning `user.tool_result`;
// messages sent in that window are rejected. That failure **is fixed by resending once**, while
// "invalid event format", also a 400, will never succeed no matter how many resends. Mixing
// them up means the user either waits for nothing or thinks the service is down.
func TestIsRequiresActionRejected(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			// The real shape of the error caught in production (the session is waiting for a
			// tool's result).
			name: "方舟拒收：正等工具结果",
			err: &APIError{
				Code:           "RequiresActionRejected",
				Message:        "waiting on responses to events [call_yt9jlkp7piumx86t34i2owi8]",
				Type:           "Bad Request",
				HTTPStatusCode: http.StatusBadRequest,
			},
			want: true,
		},
		{
			// Also a 400, but retrying will not help. **This case is the point of this test.**
			// An implementation that classifies by status code returns true here, so a parameter
			// error is shown as "wait for the tool to finish and resend", and the user resends
			// again and again without ever succeeding.
			name: "同为 400 但语义不同：参数错误",
			err: &APIError{
				Code:           "InvalidParameter",
				Message:        "events[0].type is not recognized",
				HTTPStatusCode: http.StatusBadRequest,
			},
			want: false,
		},
		{
			name: "鉴权失败",
			err: &APIError{
				Code:           "AuthenticationError",
				HTTPStatusCode: http.StatusUnauthorized,
			},
			want: false,
		},
		{
			// Must still be recognized after one layer of wrapping: core passes it up with
			// fmt.Errorf("%w").
			name: "被包装过",
			err: fmt.Errorf("发送消息到方舟失败: %w", &APIError{
				Code:           "RequiresActionRejected",
				HTTPStatusCode: http.StatusBadRequest,
			}),
			want: true,
		},
		{
			// Network errors and the like have no structured code and must not be misread as
			// "wait a moment and resend".
			name: "不是方舟结构化错误",
			err:  errors.New("connection reset by peer"),
			want: false,
		},
		{
			name: "nil",
			err:  nil,
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRequiresActionRejected(tc.err); got != tc.want {
				t.Errorf("IsRequiresActionRejected() = %v，期望 %v", got, tc.want)
			}
		})
	}
}

// TestErrorCode takes Ark's code field, and returns an empty string instead of panicking when
// there is none.
func TestErrorCode(t *testing.T) {
	if got := ErrorCode(&APIError{Code: "RequiresActionRejected"}); got != "RequiresActionRejected" {
		t.Errorf("应取出方舟的 code，实际 %q", got)
	}
	if got := ErrorCode(errors.New("普通错误")); got != "" {
		t.Errorf("非结构化错误应回空串，实际 %q", got)
	}
	if got := ErrorCode(nil); got != "" {
		t.Errorf("nil 应回空串，实际 %q", got)
	}
}

// TestIsRuntimeBusy matches Ark's second "just try again at another moment" code.
//
// # This one comes from real calls, not from the docs
//
// Caught in a rapid-send stress test: when the agent calls tools in a row, besides the
// tens-of-milliseconds RequiresActionRejected window, it also hits **HTTP 409 +
// code=RuntimeBusy** (text: `Session is currently busy with another exclusive command; retry
// later`). At the time it fell into the handler's default and became a 500 "internal server
// error": exactly the problem RequiresActionRejected was fixed for, only rarer, so it was not
// caught back then.
func TestIsRuntimeBusy(t *testing.T) {
	busy := &APIError{
		Code:           "RuntimeBusy",
		Message:        "Session is currently busy with another exclusive command; retry later.",
		Type:           "Conflict",
		HTTPStatusCode: 409,
	}
	if !IsRuntimeBusy(busy) {
		t.Error("应当认出 RuntimeBusy")
	}
	// **Do not classify by status code**: our own ErrConflict is also 409, and 409 has other
	// meanings on the Ark side too.
	other409 := &APIError{Code: "SomethingElse", HTTPStatusCode: 409}
	if IsRuntimeBusy(other409) {
		t.Error("同为 409 但 code 不同，不该被认成 RuntimeBusy")
	}
	// The two codes are independent; neither should be mistaken for the other.
	if IsRequiresActionRejected(busy) {
		t.Error("RuntimeBusy 不该被认成 RequiresActionRejected")
	}
}

// TestIsTransientSendRejection: both codes must be caught by this one check.
//
// Callers (core's retry and 409 translation) only ask this one function. Missing either one
// shows up as **the user occasionally seeing a 500** that points to no real fault; that is the
// regression this test guards against.
func TestIsTransientSendRejection(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"工具执行窗口", &APIError{Code: "RequiresActionRejected", HTTPStatusCode: 400}, true},
		{"会话忙", &APIError{Code: "RuntimeBusy", HTTPStatusCode: 409}, true},
		// The ones below never succeed no matter how many retries and must be excluded; mixing
		// them in would make the user wait out a whole retry budget on an operation that can
		// never succeed.
		{"参数非法", &APIError{Code: "InvalidParameter", HTTPStatusCode: 400}, false},
		{"鉴权失败", &APIError{Code: "Unauthorized", HTTPStatusCode: 401}, false},
		{"非方舟错误", errors.New("connection refused"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTransientSendRejection(tc.err); got != tc.want {
				t.Errorf("IsTransientSendRejection() = %v，期望 %v", got, tc.want)
			}
		})
	}
}

// TestIsManagedAgentNotOpen matches code=ManagedAgentNotOpen, not "404".
//
// The same 404 has two conflicting meanings: the resource was deleted (chat.go relies on it to
// clear stale bindings and self-heal), and the account has not activated Managed Agents (someone
// must activate it in the console). Classifying by status code would treat the latter as "stale
// binding" too, pointlessly deleting and rebuilding the binding, only to get 404 again.
func TestIsManagedAgentNotOpen(t *testing.T) {
	notOpen := &APIError{
		Code:           "ManagedAgentNotOpen",
		Message:        "Your account 2100000001 has not activated the managed agent. Please activate the service in the Ark Console.",
		Type:           "Not Found",
		HTTPStatusCode: http.StatusNotFound,
		RequestId:      "021789045093086923718d11",
	}
	if !IsManagedAgentNotOpen(notOpen) {
		t.Error("应当认出 ManagedAgentNotOpen")
	}
	// Note: IsNotFound(notOpen) is still true; it only looks at the HTTP status code.
	// So the "404 → delete binding and self-heal" of CreateSession in chat.go must not swallow
	// this meaning: when not activated, deleting and rebuilding still gives 404 and only wastes
	// a round. Only the code can tell the two apart.
	// An ordinary 404 for a deleted resource must not be mistaken for it: that is a different
	// self-heal path.
	gone := &APIError{HTTPStatusCode: http.StatusNotFound}
	if IsManagedAgentNotOpen(gone) {
		t.Error("普通 404 不该被认成未开通")
	}
	if IsManagedAgentNotOpen(errors.New("connection refused")) {
		t.Error("非方舟错误不该被认成未开通")
	}
}

// TestNotOpenAccountID must extract the account id from the message, and return an empty
// string instead of guessing when it cannot.
func TestNotOpenAccountID(t *testing.T) {
	notOpen := &APIError{
		Code:    "ManagedAgentNotOpen",
		Message: "Your account 2100000001 has not activated the managed agent.",
	}
	if got := NotOpenAccountID(notOpen); got != "2100000001" {
		t.Errorf("账号 id = %q，期望 2100000001", got)
	}
	if got := NotOpenAccountID(&APIError{Code: "ManagedAgentNotOpen", Message: "no account here"}); got != "" {
		t.Errorf("原话里没账号 id 时该给空串，实际 %q", got)
	}
	if got := NotOpenAccountID(errors.New("other")); got != "" {
		t.Errorf("非方舟错误该给空串，实际 %q", got)
	}
}

// TestModelEndpointNotOpen matches `endpoint "x" is invalid ... service not open` in a 400
// InvalidParameter message; it only counts when all three are present.
//
// Real shape seen in production (Agent creation rejected after the global key was switched to an
// account that had not activated this model):
//
//	model.id: endpoint "doubao-seed-evolving" is invalid: get endpoint meta
//	doubao-seed-evolving: service not open Request id: 02178919838572...
//
// Looking only at code=InvalidParameter would also report "our own request body is wrong" as a
// user configuration problem, so `service not open` and the endpoint name in the message are the
// required second and third pieces of evidence.
func TestModelEndpointNotOpen(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		wantID  string
		wantHit bool
	}{
		{
			name: "线上真实形状",
			err: &APIError{
				Code: "InvalidParameter",
				Message: `model.id: endpoint "doubao-seed-evolving" is invalid: ` +
					`get endpoint meta doubao-seed-evolving: service not open Request id: 0217...`,
				Type:           "Bad Request",
				HTTPStatusCode: http.StatusBadRequest,
			},
			wantID:  "doubao-seed-evolving",
			wantHit: true,
		},
		{
			// Also code=InvalidParameter, also 400, but this is our own parameter mistake.
			// It should be handled as a service fault and not mistaken for "account has not
			// activated the model".
			name: "普通参数错误不冒认",
			err: &APIError{
				Code:           "InvalidParameter",
				Message:        `events[0].type is not recognized`,
				HTTPStatusCode: http.StatusBadRequest,
			},
			wantHit: false,
		},
		{
			// Has `endpoint is invalid` but no `service not open` (e.g. a misspelled model id);
			// must not be mistaken for it either.
			name: "endpoint 无效但非未开通",
			err: &APIError{
				Code:           "InvalidParameter",
				Message:        `model.id: endpoint "totally-bogus" is invalid: not found`,
				HTTPStatusCode: http.StatusBadRequest,
			},
			wantHit: false,
		},
		{
			name:    "非方舟错误",
			err:     errors.New("connection reset by peer"),
			wantHit: false,
		},
		{
			name:    "nil",
			err:     nil,
			wantHit: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotID, gotHit := ModelEndpointNotOpen(tc.err)
			if gotHit != tc.wantHit {
				t.Errorf("命中 = %v，期望 %v", gotHit, tc.wantHit)
			}
			if gotID != tc.wantID {
				t.Errorf("模型 id = %q，期望 %q", gotID, tc.wantID)
			}
		})
	}

	// Must still be recognized after one layer of wrapping: the runner wraps it with
	// fmt.Errorf("<create Ark Agent failed>: %w", ...).
	wrapped := fmt.Errorf("准备方舟 Agent 失败: %w", &APIError{
		Code:           "InvalidParameter",
		Message:        `endpoint "m-1" is invalid: service not open`,
		HTTPStatusCode: http.StatusBadRequest,
	})
	if id, ok := ModelEndpointNotOpen(wrapped); !ok || id != "m-1" {
		t.Errorf("包装后应仍可识别，got=%q ok=%v", id, ok)
	}
}

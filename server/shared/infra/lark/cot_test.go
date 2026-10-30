// Tests for the CoT message API. All use httptest to run a fake Feishu, so they run offline.
//
// They pin down two things:
//
//   - **Classifying "confirmed rejected" vs "outcome unknown"**. This is the most important
//     meaning in this file: a 4xx means the other side clearly refused, so retrying is safe;
//     408/5xx/unreachable may have already taken effect, and a retry would show the user two
//     chains of thought. Getting it wrong does not raise an error; it puts a duplicate CoT in
//     the group.
//   - **Local event validation**. The platform validates the whole batch: one bad event and the
//     whole batch is rejected, without saying which event or which field. So we report it
//     clearly locally first.
//
// One more finding from a real run: calling
// `POST open.feishu.cn/open-apis/im/v1/message_cot` with a real app's tenant token and a
// chat_id that does not exist returns `10001 invalid receive_id`. That shows the request shape
// (field names, query parameters) is right; the platform is already checking the values.

package lark

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// cotServer starts a fake Feishu; handler decides what each path returns.
func cotServer(t *testing.T, handler func(r *http.Request, body map[string]any) (int, string)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		status, resp := handler(r, body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	client, err := New(Config{OpenURL: srv.URL})
	if err != nil {
		t.Fatalf("建客户端失败: %v", err)
	}
	return client
}

// evAt builds a valid event with a caller-given timestamp, to make "must strictly increase"
// easy to test.
func evAt(ms int64) COTEvent {
	return COTEvent{
		EventType: COTEventRunStarted,
		Content:   `{"runId":"r1","threadId":"t1"}`,
		Timestamp: strconv.FormatInt(ms, 10),
	}
}

func TestCreateCOTSendsExpectedShape(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]any
	client := cotServer(t, func(r *http.Request, body map[string]any) (int, string) {
		gotPath, gotMethod, gotBody = r.URL.Path+"?"+r.URL.RawQuery, r.Method, body
		return 200, `{"code":0,"data":{"cot_id":"cot_1","message_id":"om_1"}}`
	})

	msg, err := client.CreateCOT(context.Background(), "t", "chat_id", "oc_1", CreateCOTOption{
		OriginMessageID: "om_src",
		ReplyInThread:   true,
	})
	if err != nil {
		t.Fatalf("建 CoT 失败: %v", err)
	}
	if msg.COTID != "cot_1" || msg.MessageID != "om_1" {
		t.Errorf("返回值不对: %+v", msg)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("该用 POST，得到 %s", gotMethod)
	}
	if !strings.Contains(gotPath, "/im/v1/message_cot") || !strings.Contains(gotPath, "receive_id_type=chat_id") {
		t.Errorf("路径不对: %s", gotPath)
	}
	if gotBody["receive_id"] != "oc_1" || gotBody["origin_message_id"] != "om_src" || gotBody["reply_in_thread"] != true {
		t.Errorf("请求体不对: %+v", gotBody)
	}
}

// Create succeeded but returned no IDs → treat as "unknown", not failure: the remote side very
// likely did create it, and a blind retry would add another chain of thought to the group.
func TestCreateCOTTreatsMissingIDsAsUnknown(t *testing.T) {
	client := cotServer(t, func(*http.Request, map[string]any) (int, string) {
		return 200, `{"code":0,"data":{}}`
	})
	_, err := client.CreateCOT(context.Background(), "t", "chat_id", "oc_1", CreateCOTOption{})
	if !IsCOTOutcomeUnknown(err) {
		t.Fatalf("该判为结果不确定，得到 %v", err)
	}
}

// When the chat does not support thread replies, retry once without that parameter. Without
// this fallback the whole chain of thought never shows up.
func TestCreateCOTFallsBackWhenThreadUnsupported(t *testing.T) {
	var attempts []map[string]any
	client := cotServer(t, func(_ *http.Request, body map[string]any) (int, string) {
		attempts = append(attempts, body)
		if _, inThread := body["reply_in_thread"]; inThread {
			return 400, `{"code":10001,"msg":"reply_in_thread is not supported in this chat"}`
		}
		return 200, `{"code":0,"data":{"cot_id":"cot_2","message_id":"om_2"}}`
	})

	msg, err := client.CreateCOT(context.Background(), "t", "chat_id", "oc_1", CreateCOTOption{
		ReplyInThread:              true,
		AllowReplyInThreadFallback: true,
	})
	if err != nil {
		t.Fatalf("该降级成功: %v", err)
	}
	if msg.COTID != "cot_2" {
		t.Errorf("该拿到降级后的结果，得到 %+v", msg)
	}
	if len(attempts) != 2 {
		t.Fatalf("该发两次，实际 %d 次", len(attempts))
	}
	if _, inThread := attempts[1]["reply_in_thread"]; inThread {
		t.Error("第二次不该再带 reply_in_thread")
	}
}

// Without the fallback switch on, it must not retry on its own.
func TestCreateCOTDoesNotFallBackWithoutOptIn(t *testing.T) {
	calls := 0
	client := cotServer(t, func(*http.Request, map[string]any) (int, string) {
		calls++
		return 400, `{"code":10001,"msg":"reply_in_thread is not supported in this chat"}`
	})
	if _, err := client.CreateCOT(context.Background(), "t", "chat_id", "oc_1", CreateCOTOption{
		ReplyInThread: true,
	}); err == nil {
		t.Fatal("该报错")
	}
	if calls != 1 {
		t.Errorf("没开降级就不该重试，实际发了 %d 次", calls)
	}
}

// **The most important test in this file**: 4xx is confirmed rejected (safe to retry);
// 408/5xx/unreachable is outcome unknown.
func TestCOTErrorClassification(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		wantUnkwn bool
	}{
		{"400 确认被拒", 400, `{"code":10001,"msg":"bad request"}`, false},
		{"403 确认被拒", 403, `{"code":99991672,"msg":"no permission"}`, false},
		{"404 确认被拒", 404, `{"code":230002,"msg":"not found"}`, false},
		{"408 结果不确定", 408, `{"code":1,"msg":"timeout"}`, true},
		{"500 结果不确定", 500, `{"code":1,"msg":"internal"}`, true},
		{"502 结果不确定", 502, `{"code":1,"msg":"bad gateway"}`, true},
		{"503 结果不确定", 503, `{"code":1,"msg":"unavailable"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := cotServer(t, func(*http.Request, map[string]any) (int, string) {
				return tc.status, tc.body
			})
			_, err := client.CreateCOT(context.Background(), "t", "chat_id", "oc_1", CreateCOTOption{})
			if err == nil {
				t.Fatal("该报错")
			}
			if got := IsCOTOutcomeUnknown(err); got != tc.wantUnkwn {
				t.Errorf("结果不确定判定错了：期望 %v，得到 %v（err=%v）", tc.wantUnkwn, got, err)
			}
		})
	}
}

// Not even connecting (no response at all) is also outcome unknown: the request may already
// have reached the other side.
func TestCOTUnreachableIsUnknown(t *testing.T) {
	client, err := New(Config{OpenURL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatalf("建客户端失败: %v", err)
	}
	_, err = client.CreateCOT(context.Background(), "t", "chat_id", "oc_1", CreateCOTOption{})
	if !IsCOTOutcomeUnknown(err) {
		t.Fatalf("连不上该判为结果不确定，得到 %v", err)
	}
}

func TestAppendCOTUsesPut(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	client := cotServer(t, func(r *http.Request, body map[string]any) (int, string) {
		gotMethod, gotPath, gotBody = r.Method, r.URL.Path, body
		return 200, `{"code":0}`
	})

	events := []COTEvent{evAt(1000), evAt(1001)}
	if err := client.AppendCOT(context.Background(), "t", "cot_1", "om_1", events); err != nil {
		t.Fatalf("追加失败: %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("追加该用 PUT，得到 %s", gotMethod)
	}
	if gotPath != "/im/v1/message_cot" {
		t.Errorf("路径不对: %s", gotPath)
	}
	if gotBody["cot_id"] != "cot_1" || gotBody["message_id"] != "om_1" {
		t.Errorf("请求体缺标识: %+v", gotBody)
	}
	if got, _ := gotBody["events"].([]any); len(got) != 2 {
		t.Errorf("该带两条事件，得到 %v", gotBody["events"])
	}
}

func TestCompleteCOTPostsReasonInQuery(t *testing.T) {
	var gotPath string
	client := cotServer(t, func(r *http.Request, _ map[string]any) (int, string) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		return 200, `{"code":0}`
	})
	if err := client.CompleteCOT(context.Background(), "t", "cot_1", "om_1", COTCompleteDone); err != nil {
		t.Fatalf("封口失败: %v", err)
	}
	if !strings.Contains(gotPath, "/im/v1/message_cot/complete/cot_1") {
		t.Errorf("路径该带 cotID: %s", gotPath)
	}
	if !strings.Contains(gotPath, "reason=done") || !strings.Contains(gotPath, "message_id=om_1") {
		t.Errorf("query 该带 reason 与 message_id: %s", gotPath)
	}

	if err := client.CompleteCOT(context.Background(), "t", "cot_1", "om_1", COTCompleteReason("bogus")); err == nil {
		t.Error("非法 reason 该被拦住")
	}
}

// A non-zero business error code is a failure; HTTP 200 alone does not mean success.
func TestCOTSurfacesBusinessError(t *testing.T) {
	client := cotServer(t, func(*http.Request, map[string]any) (int, string) {
		return 200, `{"code":230002,"msg":"cot not found"}`
	})
	err := client.AppendCOT(context.Background(), "t", "cot_x", "om_x", []COTEvent{evAt(1)})
	if err == nil {
		t.Fatal("业务错误码该报错")
	}
	if IsCOTOutcomeUnknown(err) {
		t.Error("明确的业务错误不该判成结果不确定")
	}
}

// ---------------------------------------------------------------------------
// Event validation
// ---------------------------------------------------------------------------

// Within one batch, timestamps must **strictly** increase; equal is not allowed either.
//
// Events produced in the same millisecond are common, so the side that builds events must
// spread the timestamps apart. This test is a reminder of that.
func TestValidateCOTEventsRequiresStrictlyIncreasingTimestamps(t *testing.T) {
	if err := ValidateCOTEvents([]COTEvent{evAt(100), evAt(101)}); err != nil {
		t.Errorf("递增的该通过: %v", err)
	}
	if err := ValidateCOTEvents([]COTEvent{evAt(100), evAt(100)}); err == nil {
		t.Error("时间戳相等该被拦住（平台要求严格递增）")
	}
	if err := ValidateCOTEvents([]COTEvent{evAt(100), evAt(99)}); err == nil {
		t.Error("时间戳倒退该被拦住")
	}
}

func TestValidateCOTEventsEnforcesBatchLimits(t *testing.T) {
	if err := ValidateCOTEvents(nil); err == nil {
		t.Error("空批该被拦住")
	}

	tooMany := make([]COTEvent, COTMaxEvents+1)
	for i := range tooMany {
		tooMany[i] = evAt(int64(i + 1))
	}
	if err := ValidateCOTEvents(tooMany); err == nil {
		t.Errorf("超过 %d 条该被拦住", COTMaxEvents)
	}

	// Exactly 50 events must pass.
	exact := make([]COTEvent, COTMaxEvents)
	for i := range exact {
		exact[i] = evAt(int64(i + 1))
	}
	if err := ValidateCOTEvents(exact); err != nil {
		t.Errorf("刚好 %d 条该通过: %v", COTMaxEvents, err)
	}
}

func TestValidateCOTEventsEnforcesContentRules(t *testing.T) {
	base := evAt(1)

	notObject := base
	notObject.Content = `"just a string"`
	if err := ValidateCOTEvents([]COTEvent{notObject}); err == nil {
		t.Error("content 不是对象该被拦住")
	}

	badJSON := base
	badJSON.Content = `{oops}`
	if err := ValidateCOTEvents([]COTEvent{badJSON}); err == nil {
		t.Error("content 不是合法 JSON 该被拦住")
	}

	tooBig := base
	tooBig.Content = `{"runId":"` + strings.Repeat("x", COTMaxContentBytes) + `","threadId":"t"}`
	if err := ValidateCOTEvents([]COTEvent{tooBig}); err == nil {
		t.Errorf("content 超过 %d 字节该被拦住", COTMaxContentBytes)
	}

	badType := base
	badType.EventType = "NOT_A_REAL_EVENT"
	if err := ValidateCOTEvents([]COTEvent{badType}); err == nil {
		t.Error("未知事件类型该被拦住")
	}

	badTS := base
	badTS.Timestamp = "不是数字"
	if err := ValidateCOTEvents([]COTEvent{badTS}); err == nil {
		t.Error("时间戳不是整数该被拦住")
	}
}

// Missing any required field of any event type gets the whole batch rejected, so we must
// report locally which field it is.
func TestValidateCOTEventsChecksRequiredFieldsPerType(t *testing.T) {
	cases := []struct {
		eventType string
		ok        string
		bad       string
		wantField string
	}{
		{COTEventRunStarted, `{"runId":"r","threadId":"t"}`, `{"runId":"r"}`, "threadId"},
		{COTEventRunFinished, `{"runId":"r","threadId":"t","status":"ok"}`, `{"runId":"r","threadId":"t"}`, "status"},
		{COTEventRunError, `{"message":"m","code":"c"}`, `{"message":"m"}`, "code"},
		{COTEventStepStarted, `{"stepName":"n","stepId":"i"}`, `{"stepName":"n"}`, "stepId"},
		{COTEventReasoningMessageContent, `{"messageId":"m","delta":"d"}`, `{"messageId":"m"}`, "delta"},
		{COTEventToolCallStart, `{"toolCallId":"t","toolCallName":"n"}`, `{"toolCallId":"t"}`, "toolCallName"},
		{COTEventToolCallResult, `{"messageId":"m","toolCallId":"t","content":"c","role":"tool"}`,
			`{"messageId":"m","toolCallId":"t","content":"c"}`, "role"},
	}
	for _, tc := range cases {
		t.Run(tc.eventType, func(t *testing.T) {
			good := COTEvent{EventType: tc.eventType, Content: tc.ok, Timestamp: "1"}
			if err := ValidateCOTEvents([]COTEvent{good}); err != nil {
				t.Errorf("齐全的该通过: %v", err)
			}
			bad := COTEvent{EventType: tc.eventType, Content: tc.bad, Timestamp: "1"}
			err := ValidateCOTEvents([]COTEvent{bad})
			if err == nil {
				t.Fatalf("缺 %s 该被拦住", tc.wantField)
			}
			// The error must name the field; otherwise we just pass on the platform's vague 400.
			if !strings.Contains(err.Error(), tc.wantField) {
				t.Errorf("错误信息该点名 %q，实际: %v", tc.wantField, err)
			}
		})
	}

	// An empty string counts as missing.
	empty := COTEvent{EventType: COTEventRunStarted, Content: `{"runId":"","threadId":"t"}`, Timestamp: "1"}
	if err := ValidateCOTEvents([]COTEvent{empty}); err == nil {
		t.Error("必填字段是空串该被拦住")
	}
}

func TestCOTRejectsBadArguments(t *testing.T) {
	client := cotServer(t, func(*http.Request, map[string]any) (int, string) {
		return 200, `{"code":0,"data":{"cot_id":"c","message_id":"m"}}`
	})
	ctx := context.Background()

	if _, err := client.CreateCOT(ctx, "t", "bogus_type", "oc_1", CreateCOTOption{}); err == nil {
		t.Error("非法 receiveIDType 该报错")
	}
	if _, err := client.CreateCOT(ctx, "t", "chat_id", "  ", CreateCOTOption{}); err == nil {
		t.Error("空 receiveID 该报错")
	}
	if err := client.AppendCOT(ctx, "t", "", "om_1", []COTEvent{evAt(1)}); err == nil {
		t.Error("空 cotID 该报错")
	}
	if err := client.CompleteCOT(ctx, "t", "cot_1", "", COTCompleteDone); err == nil {
		t.Error("空 messageID 该报错")
	}
}

// Timestamps are in milliseconds. This test also pins the rule "the timestamps we generate are
// milliseconds, not nanoseconds or seconds".
func TestCOTTimestampIsMilliseconds(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	ms := now.UnixMilli()
	ev := COTEvent{
		EventType: COTEventRunStarted,
		Content:   `{"runId":"r","threadId":"t"}`,
		Timestamp: strconv.FormatInt(ms, 10),
	}
	if err := ValidateCOTEvents([]COTEvent{ev}); err != nil {
		t.Fatalf("毫秒时间戳该通过: %v", err)
	}
	// 13 digits marks milliseconds (seconds have 10 digits, nanoseconds 19).
	if len(ev.Timestamp) != 13 {
		t.Errorf("毫秒时间戳该是 13 位，得到 %d 位: %s", len(ev.Timestamp), ev.Timestamp)
	}
}

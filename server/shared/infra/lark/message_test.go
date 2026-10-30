// Tests for outgoing messages. All use httptest to run a fake Feishu, so they run offline.
//
// They pin down two things:
//
//   - **230019 fallback**: when a thread reply is not accepted, resend without that parameter
//     instead of dropping the reply. This bug shows up as "the bot read it but never replied";
//     in the logs it is one failed request, easy to dismiss as a random network issue.
//   - **Shortening the idempotency key**: a key that is too long must be folded with sha256,
//     not truncated. Truncation makes two different uses collide into one key when their first
//     50 characters match; Feishu drops the second as a duplicate and the user misses a message.

package lark

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeOpen starts a fake open platform; handler gets (path, request body) and decides what to
// return.
func fakeOpen(t *testing.T, handler func(path string, body map[string]any) (int, string)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		status, resp := handler(r.URL.Path+queryOf(r), body)
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

func queryOf(r *http.Request) string {
	if r.URL.RawQuery == "" {
		return ""
	}
	return "?" + r.URL.RawQuery
}

func TestReplyCardSendsInteractiveContent(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	client := fakeOpen(t, func(path string, body map[string]any) (int, string) {
		gotPath, gotBody = path, body
		return 200, `{"code":0,"data":{"message_id":"om_new"}}`
	})

	sent, err := client.ReplyCard(context.Background(), "t", "om_src", TextCard("你好"), ReplyCardOption{
		ReplyInThread:  true,
		IdempotencyKey: "run-1:card",
	})
	if err != nil {
		t.Fatalf("回复失败: %v", err)
	}
	if sent.MessageID != "om_new" {
		t.Errorf("该拿到新消息 id，得到 %q", sent.MessageID)
	}
	if gotPath != "/im/v1/messages/om_src/reply" {
		t.Errorf("路径不对: %s", gotPath)
	}
	if gotBody["msg_type"] != "interactive" {
		t.Errorf("该发 interactive，得到 %v", gotBody["msg_type"])
	}
	if gotBody["reply_in_thread"] != true {
		t.Error("该带 reply_in_thread")
	}
	if gotBody["uuid"] != "run-1:card" {
		t.Errorf("该带幂等键，得到 %v", gotBody["uuid"])
	}
	// Pitfall 1: content must be a serialized string, not an object.
	content, ok := gotBody["content"].(string)
	if !ok {
		t.Fatalf("content 该是字符串，得到 %T", gotBody["content"])
	}
	if !strings.Contains(content, "你好") {
		t.Errorf("content 里该有正文: %s", content)
	}
}

// Pitfall 2: on 230019, resend without reply_in_thread instead of dropping the reply.
func TestReplyCardFallsBackWhenThreadUnsupported(t *testing.T) {
	var attempts []map[string]any
	client := fakeOpen(t, func(_ string, body map[string]any) (int, string) {
		attempts = append(attempts, body)
		if _, inThread := body["reply_in_thread"]; inThread {
			return 200, `{"code":230019,"msg":"reply_in_thread is not supported in this chat"}`
		}
		return 200, `{"code":0,"data":{"message_id":"om_fallback"}}`
	})

	sent, err := client.ReplyCard(context.Background(), "t", "om_src", TextCard("你好"), ReplyCardOption{
		ReplyInThread: true,
	})
	if err != nil {
		t.Fatalf("该降级成功而不是报错: %v", err)
	}
	if sent.MessageID != "om_fallback" {
		t.Errorf("该拿到降级后的消息 id，得到 %q", sent.MessageID)
	}
	if len(attempts) != 2 {
		t.Fatalf("该发两次（原始 + 降级），实际 %d 次", len(attempts))
	}
	if _, inThread := attempts[1]["reply_in_thread"]; inThread {
		t.Error("第二次不该再带 reply_in_thread")
	}
}

// Other errors must not trigger a resend; only the thread-reply one does.
func TestReplyCardDoesNotRetryOtherErrors(t *testing.T) {
	calls := 0
	client := fakeOpen(t, func(string, map[string]any) (int, string) {
		calls++
		return 200, `{"code":230001,"msg":"别的错"}`
	})

	_, err := client.ReplyCard(context.Background(), "t", "om_src", TextCard("x"), ReplyCardOption{
		ReplyInThread: true,
	})
	if err == nil {
		t.Fatal("该报错")
	}
	if calls != 1 {
		t.Errorf("别的错误不该重发，实际发了 %d 次", calls)
	}
}

func TestUpdateCardPatches(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("更新卡片该用 PATCH，得到 %s", r.Method)
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer srv.Close()
	client, err := New(Config{OpenURL: srv.URL})
	if err != nil {
		t.Fatalf("建客户端失败: %v", err)
	}

	if err := client.UpdateCard(context.Background(), "t", "om_1", TextCard("更新后")); err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	if gotPath != "/im/v1/messages/om_1" {
		t.Errorf("路径不对: %s", gotPath)
	}
	if content, _ := gotBody["content"].(string); !strings.Contains(content, "更新后") {
		t.Errorf("该带新内容: %v", gotBody["content"])
	}
}

func TestUpdateCardSurfacesBusinessError(t *testing.T) {
	client := fakeOpen(t, func(string, map[string]any) (int, string) {
		return 200, `{"code":230002,"msg":"消息不存在"}`
	})
	err := client.UpdateCard(context.Background(), "t", "om_gone", TextCard("x"))
	if err == nil {
		t.Fatal("业务错误码该报错，不能当成功")
	}
	if !strings.Contains(err.Error(), "230002") {
		t.Errorf("错误信息该带错误码: %v", err)
	}
}

func TestSendCardTargetsChat(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	client := fakeOpen(t, func(path string, body map[string]any) (int, string) {
		gotPath, gotBody = path, body
		return 200, `{"code":0,"data":{"message_id":"om_direct"}}`
	})

	sent, err := client.SendCard(context.Background(), "t", "oc_1", TextCard("你好"), "k1")
	if err != nil {
		t.Fatalf("直发失败: %v", err)
	}
	if sent.MessageID != "om_direct" {
		t.Errorf("该拿到消息 id，得到 %q", sent.MessageID)
	}
	if !strings.Contains(gotPath, "receive_id_type=chat_id") {
		t.Errorf("该按 chat_id 投递: %s", gotPath)
	}
	if gotBody["receive_id"] != "oc_1" {
		t.Errorf("该带 chat id，得到 %v", gotBody["receive_id"])
	}
}

// A too-long idempotency key is folded, not truncated; see the file header.
func TestIdempotencyKeyFoldsLongValues(t *testing.T) {
	short := "run-1:card"
	if got := idempotencyKey(short); got != short {
		t.Errorf("短键该原样用，得到 %q", got)
	}

	// Two long keys with the same prefix, differing only at the end: they must still differ
	// after folding. A truncating version would collide them, and Feishu would drop the second
	// message as a duplicate.
	prefix := strings.Repeat("x", uuidMaxLength)
	a := idempotencyKey(prefix + ":thinking")
	b := idempotencyKey(prefix + ":answer")
	if a == b {
		t.Fatal("前缀相同的长键折叠后撞了——用了截断而不是哈希")
	}
	if len(a) > uuidMaxLength {
		t.Errorf("折叠后仍超长: %d", len(a))
	}

	// The same input must give a stable result; if the value changed on retry, idempotency
	// would be pointless.
	if idempotencyKey(prefix+":answer") != b {
		t.Error("同一输入该得到同一个键")
	}

	if idempotencyKey("  ") != "" {
		t.Error("空键该返回空串")
	}
}

func TestCardHelpersRejectEmptyInput(t *testing.T) {
	client := fakeOpen(t, func(string, map[string]any) (int, string) {
		return 200, `{"code":0,"data":{"message_id":"om"}}`
	})
	ctx := context.Background()

	if _, err := client.ReplyCard(ctx, "t", "  ", TextCard("x"), ReplyCardOption{}); err == nil {
		t.Error("messageID 为空该报错")
	}
	if _, err := client.SendCard(ctx, "t", "", TextCard("x"), ""); err == nil {
		t.Error("chatID 为空该报错")
	}
	if err := client.UpdateCard(ctx, "t", "", TextCard("x")); err == nil {
		t.Error("messageID 为空该报错")
	}
	if _, err := client.ReplyCard(ctx, "t", "om_1", nil, ReplyCardOption{}); err == nil {
		t.Error("空卡片该报错")
	}
}

// Empty text gets a placeholder, not an empty card: Feishu rejects empty content, and the user
// would then see an error card.
func TestTextCardHandlesEmptyText(t *testing.T) {
	card := TextCard("   ")
	raw, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if strings.Contains(string(raw), `"content":""`) {
		t.Error("空文本该给占位，不能是空串")
	}
	// update_multi must be true, otherwise the card cannot be changed after sending and
	// streaming updates stop working entirely.
	cfg, _ := card["config"].(map[string]any)
	if cfg["update_multi"] != true {
		t.Error("update_multi 必须为真，否则卡片不能再更新")
	}
}

// TestReactionEmojiTypesAreLegal checks that all four reaction values are in Feishu's list of
// legal emoji_type values.
//
// # This guards against a zero-error pitfall
//
// A wrong emoji_type **raises no error**: `/im/v1/messages/{id}/reactions` still returns
// success, the reaction just never shows up. So "HTTP 200" proves nothing, and faking a
// Reactor in unit tests stays all green too. **The only real check is whether the value is in
// the list.**
//
// This really happened: the first version of the queued and redirected ones used `Hourglass`
// and `Fast`, both made up by guess and neither in the list. On a real device it looked like
// "the reaction feature does not seem to work".
//
// The list comes from Feishu's official emoji text reference (185 values). We only pin the
// ones we use: copying the full list would make a copy that goes stale as soon as Feishu adds
// new emoji, and we do not need it.
func TestReactionEmojiTypesAreLegal(t *testing.T) {
	// The ones we use, taken from the official list. Check the official docs before
	// changing this.
	legal := map[string]bool{
		"OnIt": true, "DONE": true, "OneSecond": true, "GoGoGo": true,
	}
	for _, tc := range []struct{ name, value string }{
		{"收到了", ReactionOnIt},
		{"处理完了", ReactionDone},
		{"排队中", ReactionQueued},
		{"已转向", ReactionRedirected},
	} {
		if !legal[tc.value] {
			t.Errorf("%s 用的 %q 不是合法 emoji_type——接口会返回成功但表情不出现",
				tc.name, tc.value)
		}
	}

	// **All four must be different.** Each stands for one state; a collision means that state
	// has no feedback. Queued and redirected especially must not share one (both mean
	// "received", but one waits and the other killed the previous turn).
	seen := map[string]string{}
	for _, tc := range []struct{ name, value string }{
		{"收到了", ReactionOnIt},
		{"处理完了", ReactionDone},
		{"排队中", ReactionQueued},
		{"已转向", ReactionRedirected},
	} {
		if prev, dup := seen[tc.value]; dup {
			t.Errorf("%s 与 %s 用了同一个表情 %q，那个状态就没有可见反馈了",
				tc.name, prev, tc.value)
		}
		seen[tc.value] = tc.name
	}
}

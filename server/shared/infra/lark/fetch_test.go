// Tests for fetching messages / chats. Uses httptest to run a fake Feishu, so they run offline.
//
// # Both things pinned here are "HTTP 200 but the content is fake"
//
//  1. **The card_msg_content_type switch must really be sent.** Without it the API still
//     returns 200 + code 0, but the card body becomes "请升级至最新版本客户端" ("please
//     upgrade to the latest client"). This was found by testing for real (see the header of
//     expand.go), and it is the core gap this change fixes. **No error ever tells you this
//     parameter is missing**, so only a test can pin it.
//  2. **The children of a merge-forward are in the items of the same response.** The shape is
//     a flat array of "1 parent + N children", not nested; skip parsing items[1:] and all you
//     get is the English sentinel line.
package lark

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeGet starts a fake open platform; handler gets "path+query" and decides what to return.
//
// ⚠️ The query is passed to handler on purpose: the most important assertion in this file is
// "does card_msg_content_type really appear in the URL".
func fakeGet(t *testing.T, handler func(pathWithQuery string) (int, string)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		full := r.URL.Path
		if r.URL.RawQuery != "" {
			full += "?" + r.URL.RawQuery
		}
		status, resp := handler(full)
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

// TestGetMessage_SendsUserCardContentFlag pins that the switch really goes into the URL.
//
// This is the main reason this file exists; see point 1 of the file header.
func TestGetMessage_SendsUserCardContentFlag(t *testing.T) {
	var seen string
	client := fakeGet(t, func(pathWithQuery string) (int, string) {
		seen = pathWithQuery
		return 200, `{"code":0,"msg":"success","data":{"items":[
			{"message_id":"om_1","msg_type":"interactive","body":{"content":"{}"}}]}}`
	})

	if _, err := client.GetMessage(context.Background(), "t-token", "om_1", true); err != nil {
		t.Fatalf("取消息失败: %v", err)
	}
	if !strings.Contains(seen, "card_msg_content_type=user_card_content") {
		t.Errorf("withUserCard=true 时必须带这个 query，否则拿到的是降级后的假内容。实际 URL：%s", seen)
	}

	// The reverse: do not send it when not wanted (the extra parameter would push normal
	// messages through the card-restore path too).
	if _, err := client.GetMessage(context.Background(), "t-token", "om_1", false); err != nil {
		t.Fatalf("取消息失败: %v", err)
	}
	if strings.Contains(seen, "card_msg_content_type") {
		t.Errorf("withUserCard=false 时不该带这个 query，实际 URL：%s", seen)
	}
}

// TestGetMessage_ParsesMergeForwardChildren pins that both parent and children are parsed.
//
// The shape follows a real response: items is flat, and children carry upper_message_id
// pointing back to the parent.
func TestGetMessage_ParsesMergeForwardChildren(t *testing.T) {
	client := fakeGet(t, func(string) (int, string) {
		return 200, `{"code":0,"msg":"success","data":{"items":[
			{"message_id":"om_parent","msg_type":"merge_forward",
			 "body":{"content":"Merged and Forwarded Message"},
			 "sender":{"id":"ou_alice","sender_type":"user"},"create_time":"1788250740665"},
			{"message_id":"om_kid1","msg_type":"text","body":{"content":"{\"text\":\"子消息甲\"}"},
			 "upper_message_id":"om_parent","sender":{"id":"cli_bot","sender_type":"app"}},
			{"message_id":"om_kid2","msg_type":"image","body":{"content":"{\"image_key\":\"img_zzz\"}"},
			 "upper_message_id":"om_parent","sender":{"id":"ou_bob","sender_type":"user"},"deleted":true}]}}`
	})

	items, err := client.GetMessage(context.Background(), "t-token", "om_parent", true)
	if err != nil {
		t.Fatalf("取消息失败: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("要解出 1 父 + 2 子共 3 条，实际 %d 条", len(items))
	}

	// Check the first item (parent) field by field. Every field has a distinct value, so a
	// value in the wrong place shows up.
	root := items[0]
	if root.MessageID != "om_parent" || root.MessageType != "merge_forward" {
		t.Errorf("父消息字段不对：%+v", root)
	}
	if root.UpperMessageID != "" {
		t.Errorf("父消息不该有 upper_message_id，实际 %q", root.UpperMessageID)
	}
	if root.SenderID != "ou_alice" || root.SenderType != "user" {
		t.Errorf("父消息发送者不对：%+v", root)
	}

	// Children must show the parent link. **This is the only basis for rebuilding the structure.**
	if items[1].UpperMessageID != "om_parent" || items[2].UpperMessageID != "om_parent" {
		t.Errorf("子消息要带 upper_message_id：%+v / %+v", items[1], items[2])
	}
	if items[1].MessageID != "om_kid1" || items[1].SenderType != "app" {
		t.Errorf("第一条子消息串味了：%+v", items[1])
	}
	// The recalled flag must come through, otherwise expansion prints an empty line.
	if !items[2].Deleted {
		t.Errorf("撤回标记要解出来：%+v", items[2])
	}
	if items[1].Deleted {
		t.Errorf("没撤回的不该被标成撤回：%+v", items[1])
	}
}

// TestGetMessage_BusinessErrorIsError pins that code != 0 is treated as an error.
//
// These Feishu APIs follow "HTTP 200 + business code". Looking only at the HTTP status turns a
// failure into success, and then expands an empty items list, which silently drops content.
func TestGetMessage_BusinessErrorIsError(t *testing.T) {
	client := fakeGet(t, func(string) (int, string) {
		return 200, `{"code":230002,"msg":"no permission","data":{}}`
	})
	if _, err := client.GetMessage(context.Background(), "t-token", "om_1", true); err == nil {
		t.Fatal("code != 0 必须当错误，否则调用方会把「没权限」当成「这条消息没内容」")
	}
}

// TestGetChat_ParsesName pins that the group name is parsed.
func TestGetChat_ParsesName(t *testing.T) {
	client := fakeGet(t, func(string) (int, string) {
		return 200, `{"code":0,"msg":"success","data":{"name":"告警值班群",
			"description":"线上告警在这里处理","chat_mode":"topic"}}`
	})
	info, err := client.GetChat(context.Background(), "t-token", "oc_x")
	if err != nil {
		t.Fatalf("取会话失败: %v", err)
	}
	// The three fields have distinct values, so a mix-up is visible.
	if info.Name != "告警值班群" {
		t.Errorf("群名不对：%q", info.Name)
	}
	if info.Description != "线上告警在这里处理" {
		t.Errorf("群描述不对：%q", info.Description)
	}
	if info.ChatMode != "topic" {
		t.Errorf("chat_mode 不对：%q", info.ChatMode)
	}
}

// TestGetMessage_RejectsEmptyArgs pins that empty arguments send no request.
func TestGetMessage_RejectsEmptyArgs(t *testing.T) {
	called := false
	client := fakeGet(t, func(string) (int, string) {
		called = true
		return 200, `{"code":0,"data":{"items":[]}}`
	})
	if _, err := client.GetMessage(context.Background(), "t", "", true); err == nil {
		t.Error("messageID 为空要报错")
	}
	if _, err := client.GetMessage(context.Background(), "", "om_1", true); err == nil {
		t.Error("token 为空要报错")
	}
	if called {
		t.Error("参数不合法时不该真发请求")
	}
}

// TestListMessages_FollowsPagination pins that it **really pages through**.
//
// # This is a real bug caught in code review
//
// The old code fetched only one page, while claiming "fetch the whole thread". Tested for real:
// with page_size=2 Feishu returns `has_more=true` + a `page_token`. So when a thread is longer
// than one page it **silently gives less context**, while the scope note still says "all".
// That tells the agent it has read the whole thread, and it draws conclusions from that.
// Zero errors.
func TestListMessages_FollowsPagination(t *testing.T) {
	var seen []string
	client := fakeGet(t, func(pathWithQuery string) (int, string) {
		seen = append(seen, pathWithQuery)
		switch len(seen) {
		case 1:
			return 200, `{"code":0,"data":{"has_more":true,"page_token":"tok_page2","items":[
				{"message_id":"om_p1","msg_type":"text","body":{"content":"{\"text\":\"第一页的消息\"}"}}]}}`
		case 2:
			return 200, `{"code":0,"data":{"has_more":true,"page_token":"tok_page3","items":[
				{"message_id":"om_p2","msg_type":"text","body":{"content":"{\"text\":\"第二页的消息\"}"}}]}}`
		default:
			return 200, `{"code":0,"data":{"has_more":false,"items":[
				{"message_id":"om_p3","msg_type":"text","body":{"content":"{\"text\":\"第三页的消息\"}"}}]}}`
		}
	})

	items, err := client.ListMessages(context.Background(), "tok", ContainerThread, "omt_x", 50, 5, false)
	if err != nil {
		t.Fatalf("取历史失败: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("三页共 3 条都要拿回来，实际 %d 条", len(items))
	}
	// The 2nd and 3rd requests must carry the token from the previous page, otherwise they just
	// fetch the first page again.
	if !strings.Contains(seen[1], "page_token=tok_page2") {
		t.Errorf("第二次请求要带上一页的 token，实际 URL：%s", seen[1])
	}
	if !strings.Contains(seen[2], "page_token=tok_page3") {
		t.Errorf("第三次请求要带第二页的 token，实际 URL：%s", seen[2])
	}
	// The first request must not carry a token.
	if strings.Contains(seen[0], "page_token") {
		t.Errorf("第一次请求不该带 page_token，实际 URL：%s", seen[0])
	}
	// It must stop after has_more=false.
	if len(seen) != 3 {
		t.Errorf("has_more=false 之后要停，实际请求了 %d 次", len(seen))
	}
}

// TestListMessages_RespectsMaxPages pins that paging has a hard limit.
//
// Without a limit, a group with thousands of messages would eat memory, time and the context
// budget all at once, and this code runs **on the path where the user waits for a reply**.
func TestListMessages_RespectsMaxPages(t *testing.T) {
	calls := 0
	client := fakeGet(t, func(string) (int, string) {
		calls++
		// Always says there is more: without a limit this would loop forever.
		return 200, `{"code":0,"data":{"has_more":true,"page_token":"more","items":[
			{"message_id":"om_x","msg_type":"text","body":{"content":"{\"text\":\"一条\"}"}}]}}`
	})

	items, err := client.ListMessages(context.Background(), "tok", ContainerThread, "omt_x", 50, 3, false)
	if err != nil {
		t.Fatalf("取历史失败: %v", err)
	}
	if calls != 3 {
		t.Errorf("maxPages=3 就最多请求 3 次，实际 %d 次", calls)
	}
	if len(items) != 3 {
		t.Errorf("应拿回 3 条，实际 %d", len(items))
	}
}

// TestListMessages_KeepsEarlierPagesOnLaterFailure pins "if page 2 fails, keep page 1".
//
// A bit less context is far better than no context.
func TestListMessages_KeepsEarlierPagesOnLaterFailure(t *testing.T) {
	n := 0
	client := fakeGet(t, func(string) (int, string) {
		n++
		if n == 1 {
			return 200, `{"code":0,"data":{"has_more":true,"page_token":"t2","items":[
				{"message_id":"om_ok","msg_type":"text","body":{"content":"{\"text\":\"拿到了的那条\"}"}}]}}`
		}
		return 200, `{"code":230027,"msg":"no permission","data":{}}`
	})

	items, err := client.ListMessages(context.Background(), "tok", ContainerThread, "omt_x", 50, 5, false)
	if err != nil {
		t.Fatalf("后续页失败不该让整次调用失败：%v", err)
	}
	if len(items) != 1 || items[0].MessageID != "om_ok" {
		t.Errorf("已经拿到的那页要留着，实际 %+v", items)
	}
}

// TestListMessages_SinglePageWhenMaxPagesOne pins that "latest N messages" calls the API once.
//
// "Latest 50 messages" is one page by definition; paging would break that meaning and cost an
// extra round trip.
func TestListMessages_SinglePageWhenMaxPagesOne(t *testing.T) {
	calls := 0
	client := fakeGet(t, func(string) (int, string) {
		calls++
		return 200, `{"code":0,"data":{"has_more":true,"page_token":"more","items":[
			{"message_id":"om_a","msg_type":"text","body":{"content":"{\"text\":\"一条\"}"}}]}}`
	})
	if _, err := client.ListMessages(context.Background(), "tok", ContainerChat, "oc_x", 50, 1, true); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("maxPages=1 只该请求 1 次（哪怕 has_more=true），实际 %d 次", calls)
	}
}

// Tests for expanding cards and merged forwards.
//
// # All fixtures come from **real data**, not invented from the docs
//
// The content strings below were copied verbatim from the real Feishu API (using message ids
// found through real events in the local run_events). This matters: the shapes of these two
// message types are far from what you would expect, and fixtures guessed from the docs would
// give an implementation that "passes all tests but fails to decode in production":
//
//   - The content of a merged forward is **not JSON**; it is a fixed English sentence;
//   - By default a card comes back in its **degraded form** (the body is "please upgrade to the
//     latest client"); the real body is only returned with card_msg_content_type.
//
// Both were found only by testing; the docs do not mention either.
//
// # ⚠️ Fixture fields are deliberately all different
//
// Each child message has a different id, sender and body (the lesson that a mutation fixture
// must differ on every axis): if values share a source, a bug like "printing message 2's body
// as message 1's" still passes.
package lark

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Real fixtures
// ---------------------------------------------------------------------------

// realCard20 is a 2.0 card **returned by real Feishu** (with
// card_msg_content_type=user_card_content). Copied verbatim from a real message.
const realCard20 = `{"body":{"elements":[{"content":"收到，我在的 👋\n\n测试成功——我是 ARK Agent 报警助手，可以正常响应群里的消息。","element_id":"_2","tag":"markdown"}]},"config":{"enable_forward_interaction":false,"streaming_mode":false},"schema":"2.0"}`

// realCardDegraded is the degraded form returned for **the same message** without that flag.
//
// ⚠️ This is the core problem being fixed: the body becomes "please upgrade to the latest
// client", while the API returns HTTP 200 + code 0, with nothing telling you the content is fake.
const realCardDegraded = `{"title":null,"elements":[[{"tag":"img","image_key":"img_v3_02154_dab3764d-439e-4090-824a-33be5df5052g"},{"tag":"text","text":"请升级至最新版本客户端，以查看内容"},{"tag":"text","text":""}]]}`

// realMergeForwardSentinel is the content of a merged forward: a fixed English sentence,
// **not JSON**. Copied verbatim from a real message.
const realMergeForwardSentinel = "Merged and Forwarded Message"

// TestExpandMessage_RealCard20 checks with a real card that the body can be decoded.
func TestExpandMessage_RealCard20(t *testing.T) {
	got := ExpandMessage([]FetchedMessage{{
		MessageID:   "om_card1",
		MessageType: MsgTypeInteractive,
		Content:     realCard20,
	}})

	if !strings.Contains(got, "收到，我在的") {
		t.Errorf("2.0 卡片的 markdown 正文要解出来，实际：%q", got)
	}
	if !strings.Contains(got, "ARK Agent 报警助手") {
		t.Errorf("正文第二段也要在，实际：%q", got)
	}
	// The decoded result must be the body; structural fields like schema / element_id must not be printed.
	for _, unwanted := range []string{"element_id", "schema", "streaming_mode"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("结构字段 %q 不该出现在给 agent 的文本里：%q", unwanted, got)
		}
	}
}

// TestExpandMessage_DegradedCardStillReadable pins that the degraded form also yields **its
// own** text.
//
// This does not encourage the degraded form: the real path always carries user_card_content.
// It guards against "the degraded form decodes to an empty string": then the agent would not
// even know "there is a card here".
func TestExpandMessage_DegradedCardStillReadable(t *testing.T) {
	got := ExpandMessage([]FetchedMessage{{
		MessageID:   "om_card2",
		MessageType: MsgTypeInteractive,
		Content:     realCardDegraded,
	}})
	if strings.TrimSpace(got) == "" {
		t.Fatal("降级态卡片也要解出文本，不能是空串（空串会让调用方丢掉占位符）")
	}
	if !strings.Contains(got, "请升级至最新版本客户端") {
		t.Errorf("降级态的行×段结构要能解，实际：%q", got)
	}
}

// TestExpandMessage_RealMergeForward checks merged-forward expansion with the real shape.
//
// Structure follows the real response: items[0] is the parent (sentinel string), the rest are
// child messages, which point to the parent through upper_message_id.
func TestExpandMessage_RealMergeForward(t *testing.T) {
	items := []FetchedMessage{
		{
			MessageID:   "om_parent",
			MessageType: MsgTypeMergeForward,
			Content:     realMergeForwardSentinel,
			SenderType:  "user",
		},
		{
			MessageID:      "om_child_card",
			MessageType:    MsgTypeInteractive,
			Content:        `{"schema":"2.0","body":{"elements":[{"tag":"markdown","content":"TLS告警：错误数超阈值"}]}}`,
			UpperMessageID: "om_parent",
			SenderType:     "app",
		},
		{
			MessageID:      "om_child_text",
			MessageType:    "text",
			Content:        `{"text":"这个昨天也报过一次"}`,
			UpperMessageID: "om_parent",
			SenderType:     "user",
		},
	}

	got := ExpandMessage(items)

	// Both child bodies must be present and **different from each other** (fixture fields differ exactly so this assertion is meaningful).
	if !strings.Contains(got, "TLS告警：错误数超阈值") {
		t.Errorf("卡片子消息的正文要展开，实际：%q", got)
	}
	if !strings.Contains(got, "这个昨天也报过一次") {
		t.Errorf("文本子消息的正文要展开，实际：%q", got)
	}
	// The uninformative English sentinel must not appear in the agent's text: it is exactly what this change removes.
	if strings.Contains(got, realMergeForwardSentinel) {
		t.Errorf("哨兵串不该出现在展开结果里：%q", got)
	}
	// Tell the agent this is a forward and how many messages it has, or it does not know what it is reading.
	if !strings.Contains(got, "合并转发") {
		t.Errorf("要标明这是合并转发，实际：%q", got)
	}
}

// TestDecodeContent_MergeForwardSentinel pins that the sentinel string is not treated as
// something the user said.
//
// # This guards against exactly the behavior before the fix
//
// The content of a merged forward is not JSON, so the second decoding step in decodeContent
// fails. The old implementation took the "cannot decode an object, so it was plain text"
// branch and passed `Merged and Forwarded Message` to the agent as the user's words.
// Zero errors, and the agent guessed from one English sentence.
func TestDecodeContent_MergeForwardSentinel(t *testing.T) {
	got := decodeContent(realMergeForwardSentinel, MsgTypeMergeForward)
	if strings.Contains(got, "Merged and Forwarded") {
		t.Errorf("哨兵串必须换成中文占位符，不能原样透给 agent，实际：%q", got)
	}
	if strings.TrimSpace(got) == "" {
		t.Error("也不能变成空串——那样 agent 连「这里有条转发」都不知道")
	}
}

// ---------------------------------------------------------------------------
// Truncation
// ---------------------------------------------------------------------------

// TestExpandMessage_TruncatesManyChildren pins the count limit and the "truncated" note.
//
// One merged forward can hold hundreds of messages; putting all of them into the prompt
// overflows the context and is **stored forever in RunEvent**. Truncating is not enough: we
// must **say** it was truncated, or the agent treats the partial content as complete.
func TestExpandMessage_TruncatesManyChildren(t *testing.T) {
	items := []FetchedMessage{{
		MessageID:   "om_parent",
		MessageType: MsgTypeMergeForward,
		Content:     realMergeForwardSentinel,
	}}
	// Build far more child messages than the limit, each with a different body (so we can assert exactly which ones remain).
	const total = maxForwardChildren + 15
	for i := 0; i < total; i++ {
		items = append(items, FetchedMessage{
			MessageID:      "om_child" + itoa(i),
			MessageType:    "text",
			Content:        `{"text":"消息内容编号` + itoa(i) + `"}`,
			UpperMessageID: "om_parent",
		})
	}

	got := ExpandMessage(items)

	if !strings.Contains(got, truncatedNotice) {
		t.Error("超上限必须带「已截断」说明，否则 agent 会以为看到的是全部")
	}
	// The last one is surely not included.
	if strings.Contains(got, "消息内容编号"+itoa(total-1)) {
		t.Error("超出上限的子消息不该出现")
	}
	// The first one must be there: truncation cuts the tail, it does not drop everything.
	if !strings.Contains(got, "消息内容编号0") {
		t.Error("上限之内的子消息要保留")
	}
}

// TestExpandMessage_TruncatesLongSingleChild pins that a **single** overly long message is
// also truncated.
//
// In a real log alert seen in testing, a single child message body was 3KB+ (mostly a base64
// log search link). Limiting only the count and not the length of each message would let one
// message eat the whole budget.
func TestExpandMessage_TruncatesLongSingleChild(t *testing.T) {
	long := strings.Repeat("很长的告警正文", 500) // far over maxChildTextLen
	got := ExpandMessage([]FetchedMessage{{
		MessageID:   "om_card3",
		MessageType: MsgTypeInteractive,
		Content:     `{"schema":"2.0","body":{"elements":[{"tag":"markdown","content":"` + long + `"}]}}`,
	}})

	if len([]rune(got)) > maxChildTextLen+len([]rune(truncatedNotice))+10 {
		t.Errorf("单条正文要截到 %d 字以内，实际 %d 字", maxChildTextLen, len([]rune(got)))
	}
	if !strings.Contains(got, truncatedNotice) {
		t.Error("截断了就要说")
	}
}

// TestExpandMessage_EmptyWhenNothingRenderable pins "return an empty string when nothing can
// be rendered".
//
// The caller relies on the empty string to decide "keep the original placeholder". If this
// returned a plausible-looking empty shell, `[卡片]` would be replaced by text with no content,
// and the agent would be even more confused.
func TestExpandMessage_EmptyWhenNothingRenderable(t *testing.T) {
	if got := ExpandMessage(nil); got != "" {
		t.Errorf("没有 items 要回空串，实际 %q", got)
	}
	// All child messages were recalled.
	got := ExpandMessage([]FetchedMessage{
		{MessageID: "om_p", MessageType: MsgTypeMergeForward, Content: realMergeForwardSentinel},
		{MessageID: "om_c1", MessageType: "text", UpperMessageID: "om_p", Deleted: true},
		{MessageID: "om_c2", MessageType: "text", UpperMessageID: "om_p", Deleted: true},
	})
	if got != "" {
		t.Errorf("子消息全撤回时要回空串（让调用方保留占位符），实际 %q", got)
	}
}

// TestExpandMessage_NoTruncationNoticeWhenComplete pins **do not say truncated when nothing was
// truncated**.
//
// If every forward carried "truncated", the agent would think the content is always incomplete,
// and would then fetch history it does not need, or add needless disclaimers to its answer.
func TestExpandMessage_NoTruncationNoticeWhenComplete(t *testing.T) {
	got := ExpandMessage([]FetchedMessage{
		{MessageID: "om_p", MessageType: MsgTypeMergeForward, Content: realMergeForwardSentinel},
		{MessageID: "om_c1", MessageType: "text", UpperMessageID: "om_p", Content: `{"text":"短消息甲"}`},
		{MessageID: "om_c2", MessageType: "text", UpperMessageID: "om_p", Content: `{"text":"短消息乙"}`},
	})
	if strings.Contains(got, truncatedNotice) {
		t.Errorf("没截断就不该有截断说明：%q", got)
	}
}

// TestCardText_OneOhSchema pins 1.0 cards (body in text.content, not content).
func TestCardText_OneOhSchema(t *testing.T) {
	got := cardText(`{"header":{"title":{"content":"发布通知","tag":"plain_text"}},` +
		`"elements":[{"tag":"div","text":{"content":"今晚十点发版","tag":"lark_md"}},` +
		`{"tag":"hr"},{"tag":"note","elements":[{"tag":"plain_text","content":"影响范围有限"}]}]}`)

	for _, want := range []string{"发布通知", "今晚十点发版", "影响范围有限"} {
		if !strings.Contains(got, want) {
			t.Errorf("1.0 卡片要解出 %q，实际：%q", want, got)
		}
	}
}

// itoa is a small helper that avoids importing strconv (used only in tests in this file).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// Group event (system) messages
// ---------------------------------------------------------------------------

// realSystemRename / realSystemInvite are group event message contents **returned by real
// Feishu**. Copied verbatim from a group's history (oc_example_chat2).
//
// Note they **have no text field**, only a {template, from_user, to_chatters} struct, so
// decodeContent does not recognize them and can only give a placeholder.
const (
	realSystemRename = `{"template":"{from_user} updated the group name from \"{old_group_name}\" to \"{group_name}\".","from_user":["张三"],"to_chatters":[],"divider_text":{}}`
	realSystemInvite = `{"template":"{from_user} invited {to_chatters} to the group. New members can view all chat history.","from_user":["张三"],"to_chatters":["AMA 助手"],"divider_text":{}}`
)

// TestExpandHistory_SkipsSystemMessages pins that group events do not go into history.
//
// # The real rendered result before the fix
//
//	用户：[system]
//	用户：[system]
//	用户：<the real user question>
//
// **Both parts are wrong**: the content is a placeholder (system content has no text field),
// and sender_type is an **empty string**, which was defaulted to "user". So the model saw "some
// user said [system]": pure noise, and it may think it failed to decode something and ask.
//
// ⚠️ The test uses msg_type, not "does the body look like a placeholder": the latter is string
// matching, and a user typing `[system]` themselves would get deleted by mistake.
func TestExpandHistory_SkipsSystemMessages(t *testing.T) {
	items := []FetchedMessage{
		{MessageID: "om_s1", MessageType: MsgTypeSystem, SenderType: "", Content: realSystemRename},
		{MessageID: "om_s2", MessageType: MsgTypeSystem, SenderType: "", Content: realSystemInvite},
		{MessageID: "om_real", MessageType: "text", SenderType: "user", Content: `{"text":"真正的用户提问"}`},
	}

	got := ExpandHistory(items, false, "")

	// Not a single character of the group events should appear: no placeholder, no template string, no renamed group name.
	for _, unwanted := range []string{"[system]", "system", "updated the group name", "invited"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("群事件不该进历史，但出现了 %q：\n%s", unwanted, got)
		}
	}
	// The real message must stay; otherwise "skip everything" would also make the assertion above pass.
	if !strings.Contains(got, "真正的用户提问") {
		t.Errorf("真消息要保留：%q", got)
	}
	// Only that one line remains.
	if n := strings.Count(got, "\n"); n != 0 {
		t.Errorf("应当只剩 1 行（两条群事件都被跳过），实际 %d 行：\n%s", n+1, got)
	}
}

// TestExpandHistory_AllSystemYieldsEmpty pins "return an empty string when everything is a
// group event".
//
// The caller relies on the empty string to decide "this history is not worth injecting". If
// this returned a string of only blank lines, the block would get an empty "context already
// fetched for you" section.
func TestExpandHistory_AllSystemYieldsEmpty(t *testing.T) {
	got := ExpandHistory([]FetchedMessage{
		{MessageID: "om_s1", MessageType: MsgTypeSystem, Content: realSystemRename},
		{MessageID: "om_s2", MessageType: MsgTypeSystem, Content: realSystemInvite},
	}, false, "")
	if got != "" {
		t.Errorf("全是群事件时要回空串，实际 %q", got)
	}
}

// TestHistorySpeaker_UnknownSenderIsNotClaimedAsUser pins that an empty sender is not falsely
// called "user".
//
// The old default branch turned "we do not know who said it" into "a real person said it",
// and the model cannot tell it is a guess. Group events are already skipped entirely, but
// **other types may also have an empty sender in the future** (anonymous messages, messages
// sent by the platform on someone's behalf), and saying "user" then would be making up facts.
func TestHistorySpeaker_UnknownSenderIsNotClaimedAsUser(t *testing.T) {
	if got := historySpeaker(FetchedMessage{SenderType: ""}); got == "用户" {
		t.Error("sender_type 为空时不能说成「用户」——那是把「不知道」断言成「真人」")
	}
	// Positive case: when the identity is known, mark it as usual.
	if got := historySpeaker(FetchedMessage{SenderType: "user"}); got != "用户" {
		t.Errorf("真人要标「用户」，实际 %q", got)
	}
	if got := historySpeaker(FetchedMessage{SenderType: "app"}); got != "机器人" {
		t.Errorf("机器人要标「机器人」，实际 %q", got)
	}
}

// TestDecodeContent_CardAndForwardNeverEmpty pins that **the placeholders for cards and merged
// forwards are never empty**.
//
// # Why this test exists on its own
//
// feishu/inbound.go has two places with `if strings.TrimSpace(msg.Text) == "" { do not trigger }`
// (the trigger check and the redirect check). They filter out "only an image/file, or only an
// @ with no words": such messages really have nothing to reply to.
//
// Cards and merged forwards **must get past these two places**: their body is in content, not
// in text. Once decodeContent returns an empty string for these two types, the whole message is
// **silently dropped**: a user posts a card in the group and @s the bot, the bot says nothing,
// and the log only says "body is empty".
//
// This invariant is also a **safety precondition** for feishu/larkUserText: after the expanded
// body improves the user message, the placeholder marks where that body came from (the `[卡片]`
// prefix); if the placeholder is gone, so is the marker.
//
// ⚠️ The values deliberately cover three shapes: with a key, without a key, and content that
// cannot be decoded into an object at all.
func TestDecodeContent_CardAndForwardNeverEmpty(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		messageType string
	}{
		{"合并转发（哨兵串，不是 JSON）", realMergeForwardSentinel, MsgTypeMergeForward},
		{"卡片（2.0 schema，正文在 body 里）",
			`{"schema":"2.0","body":{"elements":[{"tag":"markdown","content":"线上告警"}]}}`,
			MsgTypeInteractive},
		{"卡片（认不出的形状）", `{"config":{"wide_screen_mode":true}}`, MsgTypeInteractive},
		{"卡片（content 是空对象）", `{}`, MsgTypeInteractive},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeContent(tc.raw, tc.messageType)
			if strings.TrimSpace(got) == "" {
				t.Errorf("卡片/合并转发的占位符不能为空——空串会让 inbound.go 那两处"+
					"「正文为空就不触发」把整条消息静默丢掉。raw=%q", tc.raw)
			}
		})
	}
}

// Tests for inbound content parsing and the long connection. All offline; real Feishu is never touched.
//
// They lock in three pitfalls (see the file header of content.go): content must be decoded
// twice, @mentions come in three forms, and the @ check must use the structured identity, not
// the text. All three are the "no error when wrong, just wrong behavior" kind: decoding content
// one level short feeds a pile of escape characters to the agent, not stripping @ fully makes
// every sentence start with @, and checking @ by text gets fooled by renames and hand-typed
// names that match.

package lark

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

func ptr(s string) *string { return &s }

func mention(key, openID, name string) *larkim.MentionEvent {
	return &larkim.MentionEvent{
		Key:  ptr(key),
		Id:   &larkim.UserId{OpenId: ptr(openID)},
		Name: ptr(name),
	}
}

// content is "a JSON string that contains JSON"; it must be decoded twice.
func TestDecodeContentUnwrapsDoubleEncodedJSON(t *testing.T) {
	// This is exactly the shape Feishu pushes: the outer layer is a string, the object is inside.
	raw := `"{\"text\":\"你好\"}"`
	if got := decodeContent(raw, "text"); got != "你好" {
		t.Errorf("双层编码没解开，得到 %q", got)
	}

	// A single-level shape must also be handled (other sources / future changes).
	if got := decodeContent(`{"text":"你好"}`, "text"); got != "你好" {
		t.Errorf("单层也该能解，得到 %q", got)
	}

	// Empty and null must not blow up.
	for _, raw := range []string{"", "  ", "null"} {
		if got := decodeContent(raw, "text"); got != "" {
			t.Errorf("decodeContent(%q) = %q，该是空串", raw, got)
		}
	}
}

func TestDecodeContentHandlesPostAndUnknownTypes(t *testing.T) {
	post := `"{\"title\":\"周报\",\"content\":[[{\"tag\":\"text\",\"text\":\"第一行\"}],` +
		`[{\"tag\":\"text\",\"text\":\"第二行\"},{\"tag\":\"a\",\"text\":\"链接\"}]]}"`
	got := decodeContent(post, "post")
	want := "周报\n第一行\n第二行链接"
	if got != want {
		t.Errorf("富文本拍平不对\n得到 %q\n期望 %q", got, want)
	}

	// Unknown types get a placeholder, not an empty string: an empty string makes the agent think
	// the user sent an empty message.
	//
	// ⚠️ **The placeholder must carry the resource key.** This assertion used to be
	// `== "[image 消息]"`, which pinned a real gap as "expected behavior": the agent sees "there is
	// an image but I know nothing about it" and has no way to fetch it (fetching a resource needs the
	// message_id + file_key pair).
	// Now the key must appear in the placeholder; together with the download command given in the
	// <harness:lark> block, it can be fetched.
	if got := decodeContent(`"{\"image_key\":\"img_x\"}"`, "image"); got != "[图片 img_x]" {
		t.Errorf("图片占位符要带 image_key，得到 %q", got)
	}
}

// TestResourcePlaceholderCarriesKeys checks each attachment type using **really captured**
// content shapes.
//
// Background: someone reported "AMA cannot get attachment bytes, only an [image 消息]
// placeholder". The root cause was in this layer: the default branch of textFromObject only
// output the type name, dropping image_key / file_key during inbound parsing. This test pins the
// shape after the fix.
//
// **Bytes are still not downloaded**: the placeholder gives the key, and the agent fetches it with
// lark-cli when needed. The reasons are prompt length cost + those bytes would be stored forever
// in RunEvent along with the conversation.
func TestResourcePlaceholderCarriesKeys(t *testing.T) {
	cases := []struct {
		name    string
		msgType string
		content string
		want    []string // all of these substrings must appear
	}{
		{
			// Real shape: an image message fetched from open.feishu.cn.
			name:    "图片带 image_key",
			msgType: "image",
			content: `"{\"image_key\":\"img_v3_02ad_e19fca1f-912a-450e-95de-3c229091b53g\"}"`,
			want:    []string{"图片", "img_v3_02ad_e19fca1f-912a-450e-95de-3c229091b53g"},
		},
		{
			name:    "文件带文件名与 file_key",
			msgType: "file",
			content: `"{\"file_key\":\"file_v3_abc\",\"file_name\":\"报表.xlsx\"}"`,
			want:    []string{"文件", "报表.xlsx", "file_v3_abc"},
		},
		{
			name:    "语音带时长",
			msgType: "audio",
			content: `"{\"file_key\":\"file_v3_voice\",\"duration\":3000}"`,
			want:    []string{"语音", "file_v3_voice"},
		},
		{
			name:    "视频",
			msgType: "media",
			content: `"{\"file_key\":\"file_v3_mp4\",\"file_name\":\"录屏.mp4\"}"`,
			want:    []string{"视频", "录屏.mp4", "file_v3_mp4"},
		},
		{
			// Images embedded in rich text go through **another** path, postSegment, so test them separately.
			name:    "富文本内嵌图片",
			msgType: "post",
			content: `"{\"content\":[[{\"tag\":\"img\",\"image_key\":\"img_inpost\"}]]}"`,
			want:    []string{"图片", "img_inpost"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeContent(tc.content, tc.msgType)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("占位符里应含 %q，实际 %q", w, got)
				}
			}
			// It must never degrade to that "knows nothing" shape.
			if got == "["+tc.msgType+" 消息]" {
				t.Errorf("退化成无信息占位符了：%q", got)
			}
		})
	}
}

// Unknown types keep the original msg_type instead of making up a Chinese name.
func TestResourcePlaceholderUnknownTypeKeepsRawName(t *testing.T) {
	got := decodeContent(`"{}"`, "brand_new_type")
	if !strings.Contains(got, "brand_new_type") {
		t.Errorf("认不出的类型要保留原始 msg_type，实际 %q", got)
	}
}

// All three forms of @mention must be stripped.
func TestStripMentionsRemovesAllThreeForms(t *testing.T) {
	m := mention("@_user_1", "ou_bot", "小助手")

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"占位符形态", "@_user_1 帮我查一下", "帮我查一下"},
		{"展开成名字", "@小助手 帮我查一下", "帮我查一下"},
		{"名字前有空格", "@ 小助手 帮我查一下", "帮我查一下"},
		{"@ 在句中", "麻烦 @_user_1 看看这个", "麻烦  看看这个"},
		{"没有 @", "帮我查一下", "帮我查一下"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stripMentions(tc.in, []*larkim.MentionEvent{m})
			// The mid-sentence case leaves a double space; we only require that the @ and the name are gone and both ends are clean.
			if strings.Contains(got, "@") || strings.Contains(got, "小助手") {
				t.Errorf("没剥干净: %q", got)
			}
			if got != strings.TrimSpace(got) {
				t.Errorf("首尾该 trim 掉: %q", got)
			}
		})
	}
}

// After stripping @ there must be no leading blank line, or the first character the agent gets is a newline.
func TestStripMentionsCollapsesLeftoverBlankLines(t *testing.T) {
	m := mention("@_user_1", "ou_bot", "小助手")
	got := stripMentions("@_user_1\n\n帮我查一下", []*larkim.MentionEvent{m})
	if got != "帮我查一下" {
		t.Errorf("剥完该收拾空行，得到 %q", got)
	}
}

// Whether the bot is @-mentioned must be judged by the structured identity, not the text.
func TestMentionsBotComparesIdentityNotText(t *testing.T) {
	bot := BotIdentity{OpenID: "ou_bot", AppID: "cli_app"}

	if !mentionsBot([]*larkim.MentionEvent{mention("@_user_1", "ou_bot", "小助手")}, bot) {
		t.Error("open_id 对上了，该判为 @ 到机器人")
	}

	// Someone else is @-mentioned: does not count.
	if mentionsBot([]*larkim.MentionEvent{mention("@_user_1", "ou_someone", "别人")}, bot) {
		t.Error("@ 的是别人，不该判为 @ 到机器人")
	}

	// The user hand-typed a matching "@小助手" without a real @: mentions has no matching identity,
	// so it must not count. This is exactly the "fooled by text" case.
	if mentionsBot(nil, bot) {
		t.Error("没有 mentions 就不该判为 @ 到机器人")
	}

	// When the bot identity is empty, it must not be misjudged as @-mentioned.
	if mentionsBot([]*larkim.MentionEvent{mention("@_user_1", "ou_bot", "小助手")}, BotIdentity{}) {
		t.Error("不知道自己是谁时不该判为 @ 到机器人")
	}
}

func TestParseMessageTextCombinesDecodeAndStrip(t *testing.T) {
	bot := BotIdentity{OpenID: "ou_bot"}
	raw := `"{\"text\":\"@_user_1 帮我查一下\"}"`
	text, mentioned := parseMessageText(raw, "text", []*larkim.MentionEvent{
		mention("@_user_1", "ou_bot", "小助手"),
	}, bot)
	if text != "帮我查一下" {
		t.Errorf("文本该解码并剥掉 @，得到 %q", text)
	}
	if !mentioned {
		t.Error("该判为 @ 到机器人")
	}
}

// ---------------------------------------------------------------------------
// Long connection
// ---------------------------------------------------------------------------

// fakeLeases is a fake leader-election implementation: an in-memory lock, enough to test "no lock, no connection".
type fakeLeases struct {
	held     bool
	acquires int32
	err      error
	// When heldAfterRelease is true, the lock stays taken after Release, simulating "as soon as we
	// release, another instance grabs it". Used to test that reconnecting after death must not
	// connect when the lock cannot be obtained.
	heldAfterRelease bool
}

type fakeLease struct{ store *fakeLeases }

func (f *fakeLeases) Acquire(_ context.Context, _ string, _ time.Duration) (Lease, bool, error) {
	atomic.AddInt32(&f.acquires, 1)
	if f.err != nil {
		return nil, false, f.err
	}
	if f.held {
		return nil, false, nil
	}
	f.held = true
	return &fakeLease{store: f}, true, nil
}

func (l *fakeLease) Renew(context.Context, time.Duration) (bool, error) { return true, nil }
func (l *fakeLease) Release(context.Context) error {
	l.store.held = l.store.heldAfterRelease
	return nil
}

func TestNewReceiverRequiresLeases(t *testing.T) {
	// Without leader election two instances might connect at once, which silently loses messages
	// and is harder to debug than failing to start. So this must fail, not let it through with a
	// default.
	_, err := NewReceiver(ReceiverConfig{
		AppID:     "cli_x",
		AppSecret: "s",
		Handler:   func(context.Context, *InboundMessage) error { return nil },
	})
	if err == nil {
		t.Fatal("没给 Leases 该报错")
	}
	if !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("该是参数错误，得到 %v", err)
	}
}

func TestNewReceiverValidatesCredentials(t *testing.T) {
	base := ReceiverConfig{
		AppID:     "cli_x",
		AppSecret: "s",
		Handler:   func(context.Context, *InboundMessage) error { return nil },
		Leases:    &fakeLeases{},
	}
	if _, err := NewReceiver(base); err != nil {
		t.Fatalf("完整配置该能建: %v", err)
	}

	noID := base
	noID.AppID = "  "
	if _, err := NewReceiver(noID); err == nil {
		t.Error("appID 为空该报错")
	}

	noSecret := base
	noSecret.AppSecret = ""
	if _, err := NewReceiver(noSecret); err == nil {
		t.Error("appSecret 为空该报错")
	}

	noHandler := base
	noHandler.Handler = nil
	if _, err := NewReceiver(noHandler); err == nil {
		t.Error("Handler 为空该报错")
	}
}

// When someone else holds the lock we can only wait, never connect: this is the guarantee of "one long connection per app".
func TestRunWaitsWhenLeaseHeldElsewhere(t *testing.T) {
	leases := &fakeLeases{held: true} // the lock is already held by another instance
	r, err := NewReceiver(ReceiverConfig{
		AppID:     "cli_x",
		AppSecret: "s",
		Handler:   func(context.Context, *InboundMessage) error { return nil },
		Leases:    leases,
	})
	if err != nil {
		t.Fatalf("建接收器失败: %v", err)
	}

	// Give it a moment to try, then cancel. It must never connect (a real connection would call Feishu).
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := r.Run(ctx); err != nil {
		t.Fatalf("ctx 取消该干净退出，得到 %v", err)
	}
	if leases.acquires == 0 {
		t.Error("至少该尝试抢一次锁")
	}
	if leases.held != true {
		t.Error("不该动别人持有的锁")
	}
}

func TestLeaseKeyIsPerApp(t *testing.T) {
	if LeaseKey("cli_a") == LeaseKey("cli_b") {
		t.Fatal("不同应用必须是不同的锁，否则两个应用会互相排斥")
	}
	if !strings.Contains(LeaseKey("cli_a"), "cli_a") {
		t.Error("key 里该带 appID，方便在 Redis 里排查")
	}
}

func TestBackoffGrowsAndCaps(t *testing.T) {
	d := backoffMin
	for i := 0; i < 20; i++ {
		d = nextBackoff(d)
	}
	if d != backoffMax {
		t.Errorf("退避该封顶在 %v，得到 %v", backoffMax, d)
	}
}

// Invalid credentials must be recognized: reconnecting ten thousand times will not help, so we
// must stop and let the user reconnect the app.
//
// Both codes must be recognized: 1000040346 is the code **real Feishu returned in testing** for an
// app_id that does not exist (see the comment in longconn.go). Recognizing only 1000040345 would
// send the most common typo into endless reconnects.
func TestIsInvalidCredential(t *testing.T) {
	for _, code := range []int{codeInvalidCredential, codeInvalidAppID} {
		if !isInvalidCredential(&larkws.ClientError{Code: code, Msg: "invalid"}) {
			t.Errorf("该认出凭证失效（code=%d）", code)
		}
	}
	if isInvalidCredential(&larkws.ClientError{Code: 1, Msg: "别的错"}) {
		t.Error("别的客户端错误不该当成凭证失效")
	}
	if isInvalidCredential(errors.New("网络抖动")) {
		t.Error("普通错误不该当成凭证失效")
	}
	if isInvalidCredential(nil) {
		t.Error("nil 不该当成凭证失效")
	}
}

// Messages the bot sent itself must be filtered out, or it treats its own replies as new messages and keeps chatting with itself.
func TestToInboundDropsBotOwnMessages(t *testing.T) {
	r, err := NewReceiver(ReceiverConfig{
		AppID:     "cli_x",
		AppSecret: "s",
		Bot:       BotIdentity{OpenID: "ou_bot"},
		Handler:   func(context.Context, *InboundMessage) error { return nil },
		Leases:    &fakeLeases{},
	})
	if err != nil {
		t.Fatalf("建接收器失败: %v", err)
	}

	own := &larkim.P2MessageReceiveV1{Event: &larkim.P2MessageReceiveV1Data{
		Sender:  &larkim.EventSender{SenderId: &larkim.UserId{OpenId: ptr("ou_bot")}},
		Message: &larkim.EventMessage{MessageId: ptr("om_1"), Content: ptr(`"{\"text\":\"hi\"}"`)},
	}}
	if got := r.toInbound(own); got != nil {
		t.Error("机器人自己发的消息该被丢掉")
	}

	// Messages from others must stay.
	other := &larkim.P2MessageReceiveV1{Event: &larkim.P2MessageReceiveV1Data{
		Sender: &larkim.EventSender{SenderId: &larkim.UserId{OpenId: ptr("ou_human")}},
		Message: &larkim.EventMessage{
			MessageId: ptr("om_2"), ChatId: ptr("oc_1"), ChatType: ptr("group"),
			MessageType: ptr("text"), Content: ptr(`"{\"text\":\"hi\"}"`),
		},
	}}
	got := r.toInbound(other)
	if got == nil {
		t.Fatal("别人发的消息该留下")
	}
	if got.MessageID != "om_2" || got.ChatID != "oc_1" || got.Text != "hi" {
		t.Errorf("字段没收敛对: %+v", got)
	}
	if got.AppID != "cli_x" {
		t.Errorf("该带上 appID 方便反查空间，得到 %q", got.AppID)
	}

	// Incomplete events must not panic.
	if r.toInbound(nil) != nil {
		t.Error("nil 事件该返回 nil")
	}
	if r.toInbound(&larkim.P2MessageReceiveV1{}) != nil {
		t.Error("空事件该返回 nil")
	}
}

// A direct chat counts as @-mentioning the bot: the user came to talk to it specifically, so no @ should be required.
func TestToInboundTreatsP2PAsMentioned(t *testing.T) {
	r, err := NewReceiver(ReceiverConfig{
		AppID:     "cli_x",
		AppSecret: "s",
		Bot:       BotIdentity{OpenID: "ou_bot"},
		Handler:   func(context.Context, *InboundMessage) error { return nil },
		Leases:    &fakeLeases{},
	})
	if err != nil {
		t.Fatalf("建接收器失败: %v", err)
	}

	p2p := &larkim.P2MessageReceiveV1{Event: &larkim.P2MessageReceiveV1Data{
		Sender: &larkim.EventSender{SenderId: &larkim.UserId{OpenId: ptr("ou_human")}},
		Message: &larkim.EventMessage{
			MessageId: ptr("om_3"), ChatId: ptr("oc_p2p"), ChatType: ptr("p2p"),
			MessageType: ptr("text"), Content: ptr(`"{\"text\":\"在吗\"}"`),
		},
	}}
	got := r.toInbound(p2p)
	if got == nil || !got.Mentioned {
		t.Error("单聊该天然算 @ 到机器人")
	}

	// In a group, no @ means it does not count.
	group := &larkim.P2MessageReceiveV1{Event: &larkim.P2MessageReceiveV1Data{
		Sender: &larkim.EventSender{SenderId: &larkim.UserId{OpenId: ptr("ou_human")}},
		Message: &larkim.EventMessage{
			MessageId: ptr("om_4"), ChatId: ptr("oc_g"), ChatType: ptr("group"),
			MessageType: ptr("text"), Content: ptr(`"{\"text\":\"随便聊聊\"}"`),
		},
	}}
	got = r.toInbound(group)
	if got == nil {
		t.Fatal("群消息该留下（要不要触发由业务层判）")
	}
	if got.Mentioned {
		t.Error("群里没 @ 不该算 @ 到机器人")
	}
}

// fakeWSConn is a fake wsConn: it records the Close count, and its behavior can be scripted.
//
// When behavior is nil it blocks until ctx is canceled (simulating a healthy connection); when it
// is errConnDead it returns it (after an optional delay), simulating a dead connection.
type fakeWSConn struct {
	runStarted chan struct{}
	closed     int32
	behavior   error
	delay      time.Duration
	onceStart  sync.Once
}

func (f *fakeWSConn) Run(ctx context.Context) error {
	f.onceStart.Do(func() { close(f.runStarted) })
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil
		}
	}
	if f.behavior != nil {
		return f.behavior
	}
	<-ctx.Done()
	return nil
}

func (f *fakeWSConn) Close() { atomic.AddInt32(&f.closed, 1) }

func (f *fakeWSConn) closeCount() int { return int(atomic.LoadInt32(&f.closed)) }

// scriptedDialer returns scripted connections in order, and counts dials and started connections.
type scriptedDialer struct {
	mu    sync.Mutex
	conns []*fakeWSConn
	next  int
}

func (d *scriptedDialer) dial() wsConn {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.next >= len(d.conns) {
		// Script used up: return a healthy connection that blocks until ctx is canceled, so we never really dial Feishu.
		c := &fakeWSConn{runStarted: make(chan struct{})}
		d.conns = append(d.conns, c)
	}
	c := d.conns[d.next]
	d.next++
	return c
}

func (d *scriptedDialer) startedCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, c := range d.conns {
		select {
		case <-c.runStarted:
			n++
		default:
		}
	}
	return n
}

func newTestReceiver(t *testing.T, leases LeaseStore) (*Receiver, *scriptedDialer) {
	t.Helper()
	r, err := NewReceiver(ReceiverConfig{
		AppID:     "cli_x",
		AppSecret: "s",
		Handler:   func(context.Context, *InboundMessage) error { return nil },
		Leases:    leases,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("建接收器失败: %v", err)
	}
	d := &scriptedDialer{}
	r.dial = d.dial
	return r, d
}

// ctx cancel (shutdown / lease taken over) must really Close the connection.
// In the old implementation the SDK did not watch ctx, and Close came after the never-returning
// Start, so cancel did nothing.
func TestRunClosesConnWhenContextCanceled(t *testing.T) {
	leases := &fakeLeases{}
	r, d := newTestReceiver(t, leases)
	c1 := &fakeWSConn{runStarted: make(chan struct{})}
	d.conns = []*fakeWSConn{c1}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	select {
	case <-c1.runStarted:
	case <-time.After(time.Second):
		t.Fatal("连接始终没被建立")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ctx 取消该干净退出，得到 %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ctx 取消后 Run 没退出——连接没被 Close")
	}
	if c1.closeCount() == 0 {
		t.Error("ctx 取消后连接必须被 Close")
	}
}

// After the connection dies (half-open / kicked) it must reconnect automatically: a second one is
// dialed and the first is Closed.
// This is the core of this fix: in the old implementation Start never returned, and a dead
// connection could only be recovered by restarting the process.
func TestRunReconnectsAfterConnDies(t *testing.T) {
	leases := &fakeLeases{}
	r, d := newTestReceiver(t, leases)
	c1 := &fakeWSConn{runStarted: make(chan struct{}), behavior: errConnDead}
	c2 := &fakeWSConn{runStarted: make(chan struct{})}
	d.conns = []*fakeWSConn{c1, c2}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	// Wait for the second connection to start (proof the reconnect worked). There is a backoff before reconnecting, so allow enough time.
	deadline := time.After(5 * time.Second)
	for d.startedCount() < 2 {
		select {
		case <-deadline:
			t.Fatalf("失活后没有重连：只建了 %d 条连接", d.startedCount())
		case <-time.After(20 * time.Millisecond):
		}
	}
	if c1.closeCount() == 0 {
		t.Error("失活的旧连接必须被 Close")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("退出该无错，得到 %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel 后 Run 没退出")
	}
}

// Reconnecting after death is still bound by leader election: when someone else took the lock, never connect again (only one per app).
func TestRunDoesNotReconnectWhileLeaseHeldElsewhere(t *testing.T) {
	// As soon as the lock is released, someone else takes it.
	leases := &fakeLeases{heldAfterRelease: true}
	r, d := newTestReceiver(t, leases)
	c1 := &fakeWSConn{runStarted: make(chan struct{}), behavior: errConnDead}
	d.conns = []*fakeWSConn{c1}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	select {
	case <-c1.runStarted:
	case <-time.After(time.Second):
		t.Fatal("第一条连接没建立")
	}
	// Give it time to go through "dead → release → backoff → fail to get lock".
	time.Sleep(300 * time.Millisecond)
	if n := d.startedCount(); n != 1 {
		t.Errorf("抢不到锁不该再建连，期望 1 条，实际 %d 条", n)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancel 后 Run 没退出")
	}
}

// The only dead signal of a half-open connection is a failed heartbeat ping write (the SDK logs a Warn).
// Harmless Warns (receive unknown message) must not mark it dead, or it reconnects for no reason.
func TestSDKLoggerMarksDeadOnlyOnPingFailure(t *testing.T) {
	var deadReason string
	var deadMu sync.Mutex
	markDead := func(reason string) {
		deadMu.Lock()
		defer deadMu.Unlock()
		deadReason = reason
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	l := &sdkLogger{log: log, markDead: markDead}
	ctx := context.Background()

	l.Warn(ctx, "receive unknown message, message_type: %d, message: %s", 9, "x")
	if deadReason != "" {
		t.Errorf("良性 Warn 不该判死，得到 %q", deadReason)
	}

	l.Warn(ctx, "ping failed, err: %v", errors.New("write: broken pipe"))
	if !strings.Contains(deadReason, "心跳") {
		t.Errorf("ping 失败必须判死，得到 %q", deadReason)
	}

	deadReason = ""
	l.Error(ctx, "receive message failed, err: %v", errors.New("connection reset"))
	if deadReason == "" {
		t.Error("SDK Error 必须判死")
	}
}

// Emoji reaction events must have a handler, or the long connection gets broken.
//
// This locks in a real production incident, iss_example2: every time a user added an emoji
// reaction, the SDK's dispatcher.Do found no handler and returned NotFoundEventHandlerErr, the
// ws layer then replied HTTP 500 to Feishu, Feishu closed the connection, and the bot went
// offline and reconnected. **All messages during the reconnect window were lost.**
//
// ⚠️ The test must go through dispatcher.Do (the SDK's real dispatch entry), not just assert "I
// registered a function": the latter only checks our own implementation against itself, while
// this bug is entirely "Do cannot find a handler".
// The check is err == nil: that is exactly the value ws/client.go uses to decide between 200 and 500.
func TestDispatcherHandlesReactionEventsWithoutError(t *testing.T) {
	r, err := NewReceiver(ReceiverConfig{
		AppID:     "cli_x",
		AppSecret: "s",
		Bot:       BotIdentity{OpenID: "ou_bot"},
		Handler: func(context.Context, *InboundMessage) error {
			t.Error("表情回应不该触发业务 Handler")
			return nil
		},
		Leases: &fakeLeases{},
	})
	if err != nil {
		t.Fatalf("建接收器失败: %v", err)
	}
	d := r.dispatcher()

	// A schema 2.0 event looks like this: the event type is in header.event_type (see the SDK's parse).
	payload := func(eventType string) []byte {
		return []byte(`{
			"schema": "2.0",
			"header": {
				"event_id": "e1",
				"event_type": "` + eventType + `",
				"create_time": "1700000000000",
				"token": "t",
				"app_id": "cli_x",
				"tenant_key": "tk"
			},
			"event": {
				"message_id": "om_1",
				"reaction_type": {"emoji_type": "SMILE"},
				"operator_type": "user",
				"user_id": {"open_id": "ou_human"},
				"action_time": "1700000000000"
			}
		}`)
	}

	for _, eventType := range []string{
		"im.message.reaction.created_v1",
		"im.message.reaction.deleted_v1",
	} {
		// A non-nil err from Do is exactly that production 500.
		if _, err := d.Do(context.Background(), payload(eventType)); err != nil {
			t.Errorf("%s 没有处理器会让飞书断连，err = %v", eventType, err)
		}
	}
}

// Positive control: confirm the check above really catches "not registered".
//
// Without this, if Do actually returned nil for unregistered events, the test above would always
// pass: it would pass both when "registered" and when "not registered", testing nothing at all.
func TestDispatcherRejectsUnregisteredEvent(t *testing.T) {
	d := dispatcher.NewEventDispatcher("", "")
	payload := []byte(`{"schema":"2.0","header":{"event_type":"im.message.reaction.created_v1"},"event":{}}`)
	if _, err := d.Do(context.Background(), payload); err == nil {
		t.Fatal("未注册事件必须报错，否则上面那条测试是恒绿的")
	}
}

// Bot added to / removed from group events must have a handler, or the long connection gets
// broken (iss_example3).
//
// Same mechanism as the reaction one: every time a user adds the bot to a group, dispatcher.Do
// finds no handler → the ws layer replies 500 → Feishu closes the connection → messages are lost
// during the reconnect window. In production this broke the connection 3 times within 72h.
//
// The registered handlers just swallow the event (logging one Info saying it is deliberately
// ignored). Under the current design, message routing uses AppID, not the group, and there is no
// group-level binding state (reasoning in the dispatcher() comment), so joining and being removed
// have no business action. This is the final shape, not a placeholder waiting to be wired up.
func TestDispatcherHandlesBotMembershipEventsWithoutError(t *testing.T) {
	r, err := NewReceiver(ReceiverConfig{
		AppID:     "cli_x",
		AppSecret: "s",
		Bot:       BotIdentity{OpenID: "ou_bot"},
		Handler: func(context.Context, *InboundMessage) error {
			t.Error("入群/被移出事件不该触发业务 Handler")
			return nil
		},
		Leases: &fakeLeases{},
	})
	if err != nil {
		t.Fatalf("建接收器失败: %v", err)
	}
	d := r.dispatcher()

	payload := func(eventType string) []byte {
		return []byte(`{
			"schema": "2.0",
			"header": {
				"event_id": "e2",
				"event_type": "` + eventType + `",
				"create_time": "1700000000000",
				"token": "t",
				"app_id": "cli_x",
				"tenant_key": "tk"
			},
			"event": {
				"chat_id": "oc_1",
				"operator_id": {"open_id": "ou_human"},
				"timestamp": "1700000000000"
			}
		}`)
	}

	for _, eventType := range []string{
		"im.chat.member.bot.added_v1",
		"im.chat.member.bot.deleted_v1",
	} {
		// A non-nil err from Do is exactly that production 500.
		if _, err := d.Do(context.Background(), payload(eventType)); err != nil {
			t.Errorf("%s 没有处理器会让飞书断连，err = %v", eventType, err)
		}
	}
}

// The consistency between the subscribedEvents constant (the source of the "which events are
// subscribed" line in the ready log) and what dispatcher() really registers is pinned here from
// the other direction: run Do for each event in the list; if the constant lists a name the
// dispatcher did not register, it fails with the same error as production
// (not found handler → 500 → disconnect).
//
// ⚠️ This only covers one direction (constant ⊆ registered). The other direction (registered but
// missing from the constant, so the log lies) cannot be caught because the SDK cannot list the
// registered events; the comment next to the constant guards it.
func TestSubscribedEventsAllHaveHandlers(t *testing.T) {
	r, err := NewReceiver(ReceiverConfig{
		AppID:     "cli_x",
		AppSecret: "s",
		Handler:   func(context.Context, *InboundMessage) error { return nil },
		Leases:    &fakeLeases{},
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("建接收器失败: %v", err)
	}
	d := r.dispatcher()

	for _, name := range strings.Split(subscribedEvents, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			t.Fatalf("subscribedEvents 里有多余的分隔: %q", subscribedEvents)
		}
		// An empty object as the event body is enough: the swallowing handlers do not look at fields; the
		// receive handler returns nil for empty events (the nil guard in toInbound, pinned by another test).
		payload := []byte(`{"schema":"2.0","header":{"event_type":"` + name + `"},"event":{}}`)
		if _, err := d.Do(context.Background(), payload); err != nil {
			t.Errorf("subscribedEvents 列了 %s 但 dispatcher 没注册（就绪日志会说谎，且线上会断连）: %v", name, err)
		}
	}
}

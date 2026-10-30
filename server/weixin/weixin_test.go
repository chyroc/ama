package weixin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chyroc/ama/server/channel"
	"github.com/chyroc/ama/server/model/apimodel"
	wx "github.com/chyroc/ama/server/shared/infra/weixin"
)

// fakeILink is a scripted iLink server.
type fakeILink struct {
	t  *testing.T
	mu sync.Mutex

	qrCount    int
	statuses   []string // next statuses of get_qrcode_status
	verifySeen []string
	updates    []string // next getupdates bodies; when empty the request hangs until cancelled
	sent       []sentMsg
	typing     []int
}

type sentMsg struct {
	To, Context, Text string
}

func newFakeILink(t *testing.T) (*fakeILink, *httptest.Server) {
	f := &fakeILink{t: t}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeILink) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	switch r.URL.Path {
	case "/ilink/bot/get_bot_qrcode":
		f.qrCount++
		n := f.qrCount
		f.mu.Unlock()
		fmt.Fprintf(w, `{"qrcode":"qr%d","qrcode_img_content":"https://example.com/q/%d"}`, n, n)
		return
	case "/ilink/bot/get_qrcode_status":
		f.verifySeen = append(f.verifySeen, r.URL.Query().Get("verify_code"))
		st := "wait"
		if len(f.statuses) > 0 {
			st, f.statuses = f.statuses[0], f.statuses[1:]
		}
		f.mu.Unlock()
		if st == "confirmed" {
			fmt.Fprint(w, `{"status":"confirmed","bot_token":"tok-1","ilink_bot_id":"bot1@im.bot","baseurl":"`+
				"http://"+r.Host+`","ilink_user_id":"owner@im.wechat"}`)
			return
		}
		fmt.Fprintf(w, `{"status":%q}`, st)
		return
	case "/ilink/bot/getupdates":
		if len(f.updates) == 0 {
			f.mu.Unlock()
			<-r.Context().Done()
			return
		}
		next := f.updates[0]
		f.updates = f.updates[1:]
		f.mu.Unlock()
		fmt.Fprint(w, next)
		return
	case "/ilink/bot/sendmessage":
		msg, _ := body["msg"].(map[string]any)
		items, _ := msg["item_list"].([]any)
		text := items[0].(map[string]any)["text_item"].(map[string]any)["text"].(string)
		ctxTok, _ := msg["context_token"].(string)
		f.sent = append(f.sent, sentMsg{To: msg["to_user_id"].(string), Context: ctxTok, Text: text})
		f.mu.Unlock()
		fmt.Fprint(w, `{"ret":0}`)
		return
	case "/ilink/bot/getconfig":
		f.mu.Unlock()
		fmt.Fprint(w, `{"ret":0,"typing_ticket":"tt"}`)
		return
	case "/ilink/bot/sendtyping":
		f.typing = append(f.typing, int(body["status"].(float64)))
	}
	f.mu.Unlock()
	fmt.Fprint(w, `{"ret":0}`)
}

func (f *fakeILink) sentTexts() []sentMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentMsg(nil), f.sent...)
}

type fakeAgents map[string]string

func (f fakeAgents) AgentWorkspace(_ context.Context, id string) (string, error) {
	ws, ok := f[id]
	if !ok {
		return "", ErrAgentNotFound
	}
	return ws, nil
}

func newTestService(t *testing.T, srv *httptest.Server, store Store) *Service {
	t.Helper()
	svc, err := NewService(Config{
		Store:    store,
		Client:   wx.New(wx.Config{LoginBaseURL: srv.URL}),
		Agents:   fakeAgents{"agent_a": "ws1", "agent_x": "ws2"},
		PollWait: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestLoginFlowWithVerifyCode(t *testing.T) {
	f, srv := newFakeILink(t)
	store := NewMemStore()
	svc := newTestService(t, srv, store)
	ctx := context.Background()

	login, err := svc.StartLogin(ctx, "ws1", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if login.Status != apimodel.WeixinLoginWaiting || !strings.HasPrefix(login.QRCodeDataURL, "data:image/png;base64,") {
		t.Fatalf("start: %+v", login)
	}

	f.statuses = []string{"wait", "scaned", "need_verifycode", "need_verifycode", "confirmed"}
	poll := func() apimodel.WeixinLogin {
		t.Helper()
		l, err := svc.PollLogin(ctx, "ws1", "alice@example.com", login.ID)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	if l := poll(); l.Status != apimodel.WeixinLoginWaiting {
		t.Fatalf("wait: %+v", l)
	}
	if l := poll(); l.Status != apimodel.WeixinLoginScanned {
		t.Fatalf("scanned: %+v", l)
	}
	if l := poll(); l.Status != apimodel.WeixinLoginNeedVerifyCode {
		t.Fatalf("need code: %+v", l)
	}
	if _, err := svc.SubmitVerifyCode(ctx, "ws1", "alice@example.com", login.ID, "12"); err != nil {
		t.Fatal(err)
	}
	// Wrong code: asked again, and the wrong code is not sent a second time.
	if l := poll(); l.Status != apimodel.WeixinLoginNeedVerifyCode || !strings.Contains(l.Message, "不对") {
		t.Fatalf("wrong code: %+v", l)
	}
	if _, err := svc.SubmitVerifyCode(ctx, "ws1", "alice@example.com", login.ID, "34"); err != nil {
		t.Fatal(err)
	}
	l := poll()
	if l.Status != apimodel.WeixinLoginConfirmed || l.AccountID == "" || l.QRCodeDataURL != "" {
		t.Fatalf("confirmed: %+v", l)
	}
	if got := f.verifySeen; got[3] != "12" || got[4] != "34" {
		t.Fatalf("verify codes sent: %q", got)
	}

	acc, err := store.GetAccount(ctx, l.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if acc.BotID != "bot1@im.bot" || acc.BotToken != "tok-1" || acc.OwnerUserID != "owner@im.wechat" ||
		acc.WorkspaceID != "ws1" || acc.CreatedBy != "alice@example.com" || acc.BaseURL != srv.URL {
		t.Fatalf("account: %+v", acc)
	}
	// A done login stays done.
	if l := poll(); l.Status != apimodel.WeixinLoginConfirmed {
		t.Fatalf("after done: %+v", l)
	}
}

func TestLoginBelongsToItsStarter(t *testing.T) {
	_, srv := newFakeILink(t)
	svc := newTestService(t, srv, NewMemStore())
	ctx := context.Background()
	login, _ := svc.StartLogin(ctx, "ws1", "alice@example.com")
	if _, err := svc.PollLogin(ctx, "ws1", "bob@example.com", login.ID); !errors.Is(err, ErrLoginNotFound) {
		t.Fatalf("another person: %v", err)
	}
	if _, err := svc.PollLogin(ctx, "ws2", "alice@example.com", login.ID); !errors.Is(err, ErrLoginNotFound) {
		t.Fatalf("another workspace: %v", err)
	}
	if _, err := svc.SubmitVerifyCode(ctx, "ws1", "alice@example.com", login.ID, "1"); !errors.Is(err, ErrVerifyCodeNotAsked) {
		t.Fatalf("code not asked: %v", err)
	}
}

func TestLoginRefreshesExpiredQRCodeThenGivesUp(t *testing.T) {
	f, srv := newFakeILink(t)
	svc := newTestService(t, srv, NewMemStore())
	ctx := context.Background()
	login, _ := svc.StartLogin(ctx, "ws1", "a@example.com")
	f.statuses = []string{"expired", "expired", "expired"}
	l1, _ := svc.PollLogin(ctx, "ws1", "a@example.com", login.ID)
	if l1.Status != apimodel.WeixinLoginWaiting || l1.QRCodeContent != "https://example.com/q/2" {
		t.Fatalf("first refresh: %+v", l1)
	}
	l2, _ := svc.PollLogin(ctx, "ws1", "a@example.com", login.ID)
	if l2.QRCodeContent != "https://example.com/q/3" {
		t.Fatalf("second refresh: %+v", l2)
	}
	l3, _ := svc.PollLogin(ctx, "ws1", "a@example.com", login.ID)
	if l3.Status != apimodel.WeixinLoginExpired {
		t.Fatalf("after three codes: %+v", l3)
	}
}

func TestLoginBotInAnotherWorkspace(t *testing.T) {
	f, srv := newFakeILink(t)
	store := NewMemStore()
	_, _ = store.UpsertAccount(context.Background(), AccountRecord{ID: "wxa_old", WorkspaceID: "ws2", BotID: "bot1@im.bot", BotToken: "x"})
	svc := newTestService(t, srv, store)
	login, _ := svc.StartLogin(context.Background(), "ws1", "a@example.com")
	f.statuses = []string{"confirmed"}
	l, _ := svc.PollLogin(context.Background(), "ws1", "a@example.com", login.ID)
	if l.Status != apimodel.WeixinLoginFailed || !strings.Contains(l.Message, "别的空间") {
		t.Fatalf("got %+v", l)
	}
}

func TestBindAgentChecksWorkspace(t *testing.T) {
	_, srv := newFakeILink(t)
	store := NewMemStore()
	_, _ = store.UpsertAccount(context.Background(), AccountRecord{ID: "wxa_1", WorkspaceID: "ws1", BotID: "b", BotToken: "t"})
	svc := newTestService(t, srv, store)
	ctx := context.Background()
	if _, err := svc.BindAgent(ctx, "ws1", "wxa_1", "agent_x"); !errors.Is(err, ErrAgentInOtherSpace) {
		t.Fatalf("agent of another workspace: %v", err)
	}
	if _, err := svc.BindAgent(ctx, "ws1", "wxa_1", "nope"); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("missing agent: %v", err)
	}
	if _, err := svc.BindAgent(ctx, "ws2", "wxa_1", ""); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("account of another workspace must look missing: %v", err)
	}
	got, err := svc.BindAgent(ctx, "ws1", "wxa_1", "agent_a")
	if err != nil || got.AgentID != "agent_a" {
		t.Fatalf("bind: %+v %v", got, err)
	}
	list, _ := svc.ListAccounts(ctx, "ws1")
	if len(list) != 1 || list[0].AgentID != "agent_a" {
		t.Fatalf("list: %+v", list)
	}
	if err := svc.DeleteAccount(ctx, "ws1", "wxa_1"); err != nil {
		t.Fatal(err)
	}
	if list, _ := svc.ListAccounts(ctx, "ws1"); len(list) != 0 {
		t.Fatal("deleted")
	}
}

// ---- Poller ----

// fakeConv answers every turn with one agent.message echoing the user's text.
type fakeConv struct {
	mu       sync.Mutex
	created  int
	disabled bool
	runs     []string
	pending  map[string]chan string
}

func (c *fakeConv) CreateChannelConversation(_ context.Context, _, _ string, kind channel.Kind, _ string) (channel.Run, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if kind != channel.Weixin {
		return channel.Run{}, errors.New("wrong channel kind")
	}
	c.created++
	return channel.Run{ID: fmt.Sprintf("run_%d", c.created)}, nil
}

func (c *fakeConv) ch(runID string) chan string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending == nil {
		c.pending = map[string]chan string{}
	}
	if c.pending[runID] == nil {
		c.pending[runID] = make(chan string, 1)
	}
	return c.pending[runID]
}

func (c *fakeConv) AppendRunEvents(_ context.Context, runID string, raw []json.RawMessage) ([]json.RawMessage, error) {
	var ev struct {
		Content []struct{ Text string } `json:"content"`
	}
	_ = json.Unmarshal(raw[0], &ev)
	c.mu.Lock()
	c.runs = append(c.runs, runID)
	c.mu.Unlock()
	c.ch(runID) <- ev.Content[0].Text
	return raw, nil
}

func (c *fakeConv) StreamRunEvents(ctx context.Context, runID string, onFrame func([]byte) error) error {
	select {
	case text := <-c.ch(runID):
		b, _ := json.Marshal(map[string]any{"type": "agent.message", "content": []map[string]any{{"type": "text", "text": "echo: " + text}}})
		_ = onFrame(b)
		_ = onFrame([]byte(`{"type":"session.status_idle","stop_reason":{"type":"end_turn"}}`))
	case <-ctx.Done():
	}
	<-ctx.Done()
	return ctx.Err()
}

func (c *fakeConv) AgentDisabled(context.Context, string) bool { return c.disabled }

type memLeases struct {
	mu   sync.Mutex
	held map[string]bool
}

type memLease struct {
	l   *memLeases
	key string
}

func (m *memLeases) Acquire(_ context.Context, key string, _ time.Duration) (channel.Lease, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.held == nil {
		m.held = map[string]bool{}
	}
	if m.held[key] {
		return nil, false, nil
	}
	m.held[key] = true
	return memLease{m, key}, true, nil
}

func (l memLease) Renew(context.Context, time.Duration) (bool, error) { return true, nil }
func (l memLease) Release(context.Context) error {
	l.l.mu.Lock()
	delete(l.l.held, l.key)
	l.l.mu.Unlock()
	return nil
}

type pollerEnv struct {
	f      *fakeILink
	store  *MemStore
	chs    *channel.MemStore
	conv   *fakeConv
	poller *Poller
}

func newPollerEnv(t *testing.T, agentID string) *pollerEnv {
	f, srv := newFakeILink(t)
	store := NewMemStore()
	_, _ = store.UpsertAccount(context.Background(), AccountRecord{
		ID: "wxa_1", WorkspaceID: "ws1", BotID: "bot1", BotToken: "tok", BaseURL: srv.URL, AgentID: agentID,
		CreatedAt: "2026-10-01T00:00:00.000Z",
	})
	env := &pollerEnv{f: f, store: store, chs: channel.NewMemStore(), conv: &fakeConv{}}
	p, err := NewPoller(PollerConfig{
		Store: store, Channel: env.chs, Conversations: env.conv,
		Client: wx.New(wx.Config{}), Leases: &memLeases{},
	})
	if err != nil {
		t.Fatal(err)
	}
	env.poller = p
	return env
}

func userMessage(id, from, text string) wx.Message {
	return wx.Message{
		MessageID: wx.ID(id), FromUserID: from, MessageType: wx.MessageTypeUser, ContextToken: "ctx-" + id,
		Items: []wx.Item{{Type: wx.ItemText, Text: &wx.TextItem{Text: text}}},
	}
}

func TestHandleAnswersAndReusesSession(t *testing.T) {
	env := newPollerEnv(t, "agent_a")
	ctx := context.Background()
	env.poller.handle(ctx, "wxa_1", userMessage("1", "u1", "hello"))
	env.poller.handle(ctx, "wxa_1", userMessage("2", "u1", "again"))
	// Duplicate delivery: ignored.
	env.poller.handle(ctx, "wxa_1", userMessage("2", "u1", "again"))

	sent := env.f.sentTexts()
	if len(sent) != 2 {
		t.Fatalf("sent %+v", sent)
	}
	if sent[0].Text != "echo: hello" || sent[0].To != "u1" || sent[0].Context != "ctx-1" {
		t.Fatalf("first reply %+v", sent[0])
	}
	if sent[1].Text != "echo: again" || sent[1].Context != "ctx-2" {
		t.Fatalf("second reply %+v", sent[1])
	}
	if env.conv.created != 1 || env.conv.runs[0] != env.conv.runs[1] {
		t.Fatalf("one user is one session: created=%d runs=%v", env.conv.created, env.conv.runs)
	}
	if typing := env.f.typing; len(typing) < 2 || typing[len(typing)-1] != wx.TypingCancel {
		t.Fatalf("typing must start and be cleared: %v", typing)
	}

	// /new forgets the session; the next message starts another one.
	env.poller.handle(ctx, "wxa_1", userMessage("3", "u1", "/new"))
	env.poller.handle(ctx, "wxa_1", userMessage("4", "u1", "fresh"))
	if env.conv.created != 2 {
		t.Fatalf("after /new: created=%d", env.conv.created)
	}
	sent = env.f.sentTexts()
	if sent[2].Text != replyNewChat || sent[3].Text != "echo: fresh" {
		t.Fatalf("replies %+v", sent[2:])
	}
}

func TestHandleHintsWithoutAgentAndIgnoresGroupsAndBots(t *testing.T) {
	env := newPollerEnv(t, "")
	ctx := context.Background()
	env.poller.handle(ctx, "wxa_1", userMessage("1", "u1", "hi"))
	group := userMessage("2", "u1", "hi")
	group.GroupID = "g1"
	env.poller.handle(ctx, "wxa_1", group)
	bot := userMessage("3", "u1", "hi")
	bot.MessageType = wx.MessageTypeBot
	env.poller.handle(ctx, "wxa_1", bot)

	sent := env.f.sentTexts()
	if len(sent) != 1 || sent[0].Text != replyNotBound {
		t.Fatalf("sent %+v", sent)
	}
	if env.conv.created != 0 {
		t.Fatal("no session without an agent")
	}

	env2 := newPollerEnv(t, "agent_a")
	env2.conv.disabled = true
	env2.poller.handle(ctx, "wxa_1", userMessage("1", "u1", "hi"))
	if s := env2.f.sentTexts(); len(s) != 1 || s[0].Text != replyDisabled {
		t.Fatalf("disabled: %+v", s)
	}
}

func TestPollLoopSavesCursorAndStopsOnStaleToken(t *testing.T) {
	env := newPollerEnv(t, "agent_a")
	env.f.updates = []string{
		`{"ret":0,"get_updates_buf":"cur-1","msgs":[{"message_id":1,"from_user_id":"u1","message_type":1,"context_token":"c1","item_list":[{"type":1,"text_item":{"text":"ping"}}]}]}`,
		`{"ret":-14,"errmsg":"session timeout"}`,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	accounts, _ := env.poller.ListAccounts(ctx)
	if len(accounts) != 1 {
		t.Fatalf("accounts %+v", accounts)
	}
	err := env.poller.RunAccount(ctx, accounts[0])
	if !wx.IsStaleToken(err) {
		t.Fatalf("a stale token must stop the loop, got %v", err)
	}
	acc, _ := env.store.GetAccount(ctx, "wxa_1")
	if acc.SyncCursor != "cur-1" {
		t.Fatalf("cursor = %q", acc.SyncCursor)
	}
	if acc.Status != apimodel.WeixinAccountExpired || acc.LastError == "" {
		t.Fatalf("account must be marked expired: %+v", acc)
	}
	if acc.LastInboundAt == "" {
		t.Fatal("last inbound time")
	}
	if sent := env.f.sentTexts(); len(sent) != 1 || sent[0].Text != "echo: ping" {
		t.Fatalf("the message received before the stale token must still be answered: %+v", sent)
	}
	if list, _ := env.poller.ListAccounts(ctx); len(list) != 0 {
		t.Fatal("an expired account must not be polled again")
	}
}

// ---- Pure helpers ----

func TestMessageText(t *testing.T) {
	msg := wx.Message{Items: []wx.Item{
		{Type: wx.ItemText, Text: &wx.TextItem{Text: "look"}, RefMsg: &wx.RefMessage{Item: &wx.Item{Type: wx.ItemText, Text: &wx.TextItem{Text: "line1\nline2"}}}},
		{Type: wx.ItemImage, Image: &wx.MediaItem{}},
		{Type: wx.ItemVoice, Voice: &wx.VoiceItem{Text: "spoken words"}},
		{Type: wx.ItemFile, File: &wx.FileItem{FileName: "a.pdf"}},
	}}
	want := "> line1\n> line2\nlook\n[图片]\nspoken words\n[文件] a.pdf"
	if got := messageText(msg); got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if command(" /Reset ") != cmdNew || command("/help") != cmdHelp || command("hello /new") != "" {
		t.Fatal("commands")
	}
	h := harnessBlock(userMessage("9", "u1@im.wechat", "x"))
	if !strings.HasPrefix(h, "<harness:weixin>\n") || !strings.Contains(h, "user_id: u1@im.wechat\nmessage_id: 9\n\n## ") {
		t.Fatalf("harness block:\n%s", h)
	}
}

func TestReplyCleanAndSplit(t *testing.T) {
	if got := cleanReply("a ![x](http://img)\n\n\n\n###### small\n**bold**"); got != "a \n\nsmall\n**bold**" {
		t.Fatalf("clean %q", got)
	}
	long := strings.Repeat("字", 30) + "\n\n" + strings.Repeat("b", 30)
	parts := splitReply(long, 40)
	if len(parts) != 2 || parts[0] != strings.Repeat("字", 30) || parts[1] != strings.Repeat("b", 30) {
		t.Fatalf("split %q", parts)
	}
	hard := splitReply(strings.Repeat("x", 25), 10)
	if len(hard) != 3 || hard[2] != "xxxxx" {
		t.Fatalf("hard split %q", hard)
	}
}

package feishu

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chyroc/ama/server/shared/infra/lark"
)

// newChatNameService starts a fake Feishu: oc_named has a chat name, oc_gone cannot be fetched (the bot was removed).
func newChatNameService(t *testing.T) (*Service, Store, *atomic.Int32) {
	t.Helper()
	var getChatCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/open-apis/auth/v3/tenant_access_token/internal", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"t","expire":7200}`))
	})
	mux.HandleFunc("/open-apis/im/v1/chats/", func(w http.ResponseWriter, r *http.Request) {
		getChatCalls.Add(1)
		switch strings.TrimPrefix(r.URL.Path, "/open-apis/im/v1/chats/") {
		case "oc_named":
			_, _ = w.Write([]byte(`{"code":0,"data":{"name":"增长数据日常","chat_mode":"group"}}`))
		default:
			_, _ = w.Write([]byte(`{"code":232011,"msg":"bot is not in the chat"}`))
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	ctx := context.Background()
	store := NewMemStore()
	if _, err := store.CreateApp(ctx, &App{ID: "fsa_1", WorkspaceID: "ws_1", AppID: "cli_1"}, "secret"); err != nil {
		t.Fatalf("预置应用失败: %v", err)
	}
	seen := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	for _, id := range []string{"oc_named", "oc_gone"} {
		if err := store.RememberChat(ctx, ChatRecord{
			LarkAppID: "cli_1", ChatID: id, WorkspaceID: "ws_1", ChatType: "group", LastSeenAt: seen,
		}); err != nil {
			t.Fatalf("预置群失败: %v", err)
		}
	}
	svc, err := New(Config{
		Store: store, Authorizer: allowAuthorizer{},
		AccountsURL: srv.URL, OpenURL: srv.URL + "/open-apis",
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	return svc, store, &getChatCalls
}

// TestListChatsFillsMissingNames fills in chat names for chats that have none when listing chats, and writes them back to the store.
//
// This is the issue a user reported: the chat dropdown for scheduled tasks only showed a list of oc_xxx, and nobody could tell which chat was which.
// A chat whose name cannot be fetched (the bot was removed) must not make the whole list fail; list it anyway and show the chat_id.
func TestListChatsFillsMissingNames(t *testing.T) {
	ctx := context.Background()
	svc, store, calls := newChatNameService(t)

	chats, err := svc.ListChats(ctx, ListOption{ActorID: "a@example.com", WorkspaceID: "ws_1"})
	if err != nil {
		t.Fatalf("取不到某个群名不该让列表失败: %v", err)
	}
	names := map[string]string{}
	for _, c := range chats {
		names[c.ChatID] = c.Name
	}
	if len(names) != 2 {
		t.Fatalf("两个群都该列出来，实际 %v", names)
	}
	if names["oc_named"] != "增长数据日常" {
		t.Errorf("oc_named 该补上群名，实际 %q", names["oc_named"])
	}
	if names["oc_gone"] != "" {
		t.Errorf("取不到的群名该留空（界面显示 chat_id），实际 %q", names["oc_gone"])
	}

	// It was written back to the store: listing again does not call Feishu for chats that already have a name.
	stored, _ := store.ListChats(ctx, "ws_1")
	for _, c := range stored {
		if c.ChatID == "oc_named" && c.Name != "增长数据日常" {
			t.Errorf("群名该写回库，库里是 %q", c.Name)
		}
		if !c.LastSeenAt.Equal(time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)) {
			t.Errorf("补群名不该改 lastSeenAt，实际 %v", c.LastSeenAt)
		}
	}
	before := calls.Load()
	if _, err := svc.ListChats(ctx, ListOption{ActorID: "a@example.com", WorkspaceID: "ws_1"}); err != nil {
		t.Fatalf("第二次列群失败: %v", err)
	}
	if got := calls.Load() - before; got != 1 {
		t.Errorf("第二次列群只该为还没名字的 oc_gone 打一次飞书，实际 %d 次", got)
	}
}

// TestInboundRecordsPrefetchedChatName: the chat name fetched by the inbound prefetch must be written into the chat list.
//
// This is how a renamed chat catches up: the list-chats side only fills chats that "have no name yet" and does not refresh existing names.
func TestInboundRecordsPrefetchedChatName(t *testing.T) {
	conv := &fakeConversations{agentIDs: []string{"agent_1"}, frames: [][]byte{agentFrame("好")}}
	store := NewMemStore()
	if _, err := store.CreateApp(context.Background(), &App{
		ID: "fsa_1", WorkspaceID: "ws_1", AppID: "cli_1", BotOpenID: selfBotOpenID,
	}, "secret"); err != nil {
		t.Fatalf("预置应用失败: %v", err)
	}
	in, err := NewInbound(InboundConfig{
		Store:         store,
		Conversations: conv,
		Sender:        &fakeSender{},
		Tokens:        fakeTokens{},
		Fetcher:       &fakeFetcher{chat: &lark.ChatInfo{Name: "告警值班群"}},
	})
	if err != nil {
		t.Fatalf("建入站处理器失败: %v", err)
	}

	if err := in.Handle(context.Background(), inboundMsg("om_1", "看一下")); err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	chats, err := store.ListChats(context.Background(), "ws_1")
	if err != nil {
		t.Fatalf("列群失败: %v", err)
	}
	if len(chats) != 1 || chats[0].Name != "告警值班群" {
		t.Errorf("预取到的群名该写进群列表，实际 %+v", chats)
	}
}

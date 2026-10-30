// Tests for the session scope mapping: which Ark session an inbound message belongs to.
//
// # Why these tests are worth writing
//
// This whole class of defects **gives no errors**; the behavior is just wrong:
//
//   - created new when it should reuse → the bot has no context, and "what about tomorrow" gets a baffling answer
//   - reused when it should separate → the contexts of two threads pollute each other
//   - idempotency key clash      → Feishu drops the second turn's card as a duplicate, and the first turn's answer gets overwritten
//   - two turns concurrently     → both cards show the answers to two questions mixed together
//
// All of them "look fine when running", so only assertions can lock them down.
//
// scopeForMessage is a pure function, so the mapping rules can be enumerated; reuse and serialization go through the full Inbound path,
// because they also need to verify those Store methods are really called right.

package feishudao

import (
	"context"
	"errors"
	"fmt"
	"github.com/chyroc/ama/server/feishu"
	"github.com/chyroc/ama/server/feishu/feishumodel"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chyroc/ama/server/shared/infra/lark"
)

// ---------------------------------------------------------------------------
// One. Mapping rules (pure function, enumerated)
// ---------------------------------------------------------------------------

// TestScopeForMessage pins the three mapping rules one by one.
func TestScopeForMessage(t *testing.T) {
	cases := []struct {
		name     string
		msg      *lark.InboundMessage
		wantType string
		wantKey  string
		wantHint string // expected thread anchor
	}{
		{
			name:     "普通群：整个群一个会话",
			msg:      &lark.InboundMessage{ChatID: "oc_g", ChatType: "group"},
			wantType: feishu.ScopeChat,
			wantKey:  "chat:oc_g",
		},
		{
			name: "话题群：一个话题一个会话",
			msg: &lark.InboundMessage{
				ChatID: "oc_g", ChatType: "topic_group", ThreadID: "omt_a",
			},
			wantType: feishu.ScopeThread,
			wantKey:  "thread:omt_a",
			wantHint: "omt_a",
		},
		{
			name: "同一个群的另一个话题：必须是另一个会话",
			msg: &lark.InboundMessage{
				ChatID: "oc_g", ChatType: "topic_group", ThreadID: "omt_b",
			},
			wantType: feishu.ScopeThread,
			wantKey:  "thread:omt_b",
			wantHint: "omt_b",
		},
		{
			name: "单聊新话头：每条消息一个独立会话，键取自己的 message_id",
			msg: &lark.InboundMessage{
				MessageID: "om_p1", ChatID: "oc_p", ChatType: feishu.ChatTypeP2P,
			},
			// DMs go by message; see the scope.go file header.
			wantType: feishu.ScopeMessage,
			wantKey:  "message:om_p1",
		},
		{
			name: "单聊话题内的后续消息：键取 root_id，与根消息那条完全一致",
			msg: &lark.InboundMessage{
				MessageID: "om_p2", ChatID: "oc_p", ChatType: feishu.ChatTypeP2P,
				RootID: "om_root", ParentID: "om_parent",
			},
			// The root message was stored as "message:om_root", so this must compute the same string to reuse it.
			wantType: feishu.ScopeMessage,
			wantKey:  "message:om_root",
		},
		{
			name: "单聊话题内：thread_id 不参与，仍取 root_id",
			msg: &lark.InboundMessage{
				MessageID: "om_p3", ChatID: "oc_p", ChatType: feishu.ChatTypeP2P,
				ThreadID: "omt_x", RootID: "om_root",
			},
			// Accepting thread_id would build "message:omt_x", which the root message never bound; that would fix nothing.
			wantType: feishu.ScopeMessage,
			wantKey:  "message:om_root",
		},
		{
			name: "单聊只有 parent_id 时拿它兜底",
			msg: &lark.InboundMessage{
				MessageID: "om_p4", ChatID: "oc_p", ChatType: feishu.ChatTypeP2P,
				ParentID: "om_parent",
			},
			wantType: feishu.ScopeMessage,
			wantKey:  "message:om_parent",
		},
		{
			name: "群里没有 thread_id 但有 root_id：按回复链复用",
			msg: &lark.InboundMessage{
				ChatID: "oc_g", ChatType: "group", RootID: "om_root",
			},
			wantType: feishu.ScopeThread,
			wantKey:  "thread:om_root",
			wantHint: "om_root",
		},
		{
			name: "thread_id 优先于 root_id",
			msg: &lark.InboundMessage{
				ChatID: "oc_g", ChatType: "topic_group",
				ThreadID: "omt_a", RootID: "om_root", ParentID: "om_parent",
			},
			wantType: feishu.ScopeThread,
			wantKey:  "thread:omt_a",
			wantHint: "omt_a",
		},
		{
			name: "只有 parent_id 时拿它兜底",
			msg: &lark.InboundMessage{
				ChatID: "oc_g", ChatType: "group", ParentID: "om_parent",
			},
			wantType: feishu.ScopeThread,
			wantKey:  "thread:om_parent",
			wantHint: "om_parent",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := feishu.ScopeForMessage(tc.msg)
			if got.Type != tc.wantType {
				t.Errorf("作用域类型：期望 %q 实际 %q", tc.wantType, got.Type)
			}
			if got.Key != tc.wantKey {
				t.Errorf("作用域键：期望 %q 实际 %q", tc.wantKey, got.Key)
			}
			if got.ThreadID != tc.wantHint {
				t.Errorf("话题锚点：期望 %q 实际 %q", tc.wantHint, got.ThreadID)
			}
			if !got.Valid() {
				t.Error("这个作用域应当可用")
			}
		})
	}
}

// TestScopeForMessageP2PIsPerMessage: two different messages in a DM must compute two different scope keys.
//
// # This test locks down the core of this change
//
// DMs used to be reused by chat_id (the key was always "chat:<chat_id>"), so the same person typing two lines would land on
// the same session. After switching to message_id each DM message gets its own key, and therefore its own session.
//
// The reverse half must be pinned too: **recomputing for the same message must give the same key**. Feishu redelivers the same
// message (see the inbound.go file header), and with an unstable key a redelivery would create another session for the same line
// and answer the same question twice.
func TestScopeForMessageP2PIsPerMessage(t *testing.T) {
	first := feishu.ScopeForMessage(&lark.InboundMessage{
		MessageID: "om_1", ChatID: "oc_p", ChatType: feishu.ChatTypeP2P,
	})
	second := feishu.ScopeForMessage(&lark.InboundMessage{
		MessageID: "om_2", ChatID: "oc_p", ChatType: feishu.ChatTypeP2P,
	})

	if first.Key == second.Key {
		t.Fatalf("同一个单聊的两条消息不该共用作用域键，都是 %q——那样又变回复用一条会话了", first.Key)
	}
	if !first.Valid() || !second.Valid() {
		t.Fatalf("两个单聊作用域都该可用: %+v / %+v", first, second)
	}
	if first.Type != feishu.ScopeMessage || second.Type != feishu.ScopeMessage {
		t.Errorf("单聊作用域类型应当是 %q，实际 %q / %q", feishu.ScopeMessage, first.Type, second.Type)
	}
	// A DM should not carry a thread anchor: it is not grouped by anchor, and storing one would only mislead debugging into thinking there is a thread.
	if first.ThreadID != "" {
		t.Errorf("单聊作用域不该带话题锚点，实际 %q", first.ThreadID)
	}

	// Recomputing for the same message must be stable, otherwise redelivery would create a second session.
	again := feishu.ScopeForMessage(&lark.InboundMessage{
		MessageID: "om_1", ChatID: "oc_p", ChatType: feishu.ChatTypeP2P,
	})
	if again.Key != first.Key {
		t.Errorf("同一条消息的键必须稳定: %q vs %q", again.Key, first.Key)
	}
}

// TestScopeForMessageP2PKeyIgnoresChat: the key of a new DM conversation start depends only on message_id, not chat_id.
//
// A different chat_id must not change the result; otherwise "one session per message" would quietly degrade back to
// reuse by chat for some message shapes.
//
// **Note this only covers "new conversation starts".** Carrying a reply anchor is a different matter: then the key becomes the anchor, to match the root
// message, covered by TestScopeForMessageP2PThreadReusesRootKey.
func TestScopeForMessageP2PKeyIgnoresChat(t *testing.T) {
	base := feishu.ScopeForMessage(&lark.InboundMessage{
		MessageID: "om_x", ChatID: "oc_a", ChatType: feishu.ChatTypeP2P,
	})
	for _, tc := range []struct {
		name string
		msg  *lark.InboundMessage
	}{
		{"换一个单聊", &lark.InboundMessage{
			MessageID: "om_x", ChatID: "oc_b", ChatType: feishu.ChatTypeP2P,
		}},
		{"带上 thread_id 但没有回复锚点", &lark.InboundMessage{
			// thread_id does not take part in the key in DMs (it is in the omt_ namespace and does not match the root message),
			// so with only it set, it is still treated as a new conversation start.
			MessageID: "om_x", ChatID: "oc_a", ChatType: feishu.ChatTypeP2P,
			ThreadID: "omt_z",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := feishu.ScopeForMessage(tc.msg); got.Key != base.Key {
				t.Errorf("单聊新话头的键只该由 message_id 决定：期望 %q 实际 %q", base.Key, got.Key)
			}
		})
	}
}

// TestScopeForMessageP2PThreadReusesRootKey: follow-up messages inside a DM thread must compute **exactly the same**
// scope key as the root message.
//
// # This test locks down the core of this fix
//
// The user sends "hello" in a DM → the bot replies → Feishu folds this question and answer into a thread → the user **asks in that
// thread** "do you have the lark command". What they mean is "I am talking about that same thing", so the root message's session
// must be reused; otherwise the agent remembers nothing of what was said before.
//
// Before the fix, the p2p short-circuit in scope.go came before the thread check, so the thread branch was never reached, and follow-up
// messages in the thread opened a brand-new session by their own message_id again.
//
// The keys must be **exactly equal**, not just "both reusable": the root message was stored with
// `message:<its own message_id>`, and one prefix off or switching to thread_id would miss that row.
func TestScopeForMessageP2PThreadReusesRootKey(t *testing.T) {
	// The two real message_ids from production are used, which also writes the measured fact "root_id == the root message's message_id"
	// into code (see the scope.go file header).
	const (
		rootMsgID   = "om_x100b0000cfc8d27fde49fabb5d57bc50"
		secondMsgID = "om_x100b0000cd476a726b1e27ba11eeba3"
		threadID    = "omt_0000fb76f7d5ca5d"
	)

	// The root message: a new conversation start the user sent on the DM main line, with no anchors.
	root := feishu.ScopeForMessage(&lark.InboundMessage{
		MessageID: rootMsgID, ChatID: "oc_p", ChatType: feishu.ChatTypeP2P,
	})
	// The second message in the thread: measured, both root_id and parent_id point to the root message, and thread_id starts with omt_,
	// a different namespace.
	second := feishu.ScopeForMessage(&lark.InboundMessage{
		MessageID: secondMsgID, ChatID: "oc_p", ChatType: feishu.ChatTypeP2P,
		RootID: rootMsgID, ParentID: rootMsgID, ThreadID: threadID,
	})

	if second.Key != root.Key {
		t.Fatalf("话题内的后续消息必须复用根消息那个键：根 %q，实际 %q——"+
			"不一致就会又开一条新 session，agent 不记得前面聊过什么", root.Key, second.Key)
	}
	if want := feishu.ScopeMessage + ":" + rootMsgID; second.Key != want {
		t.Errorf("键应当是 %q，实际 %q", want, second.Key)
	}
	// Mixing thread_id into the key is the easiest mistake: that computes "message:omt_...", which the root message never bound.
	if strings.Contains(second.Key, threadID) {
		t.Errorf("键里不该出现 thread_id（%s 是 omt_ 命名空间，对不上根消息）：%q",
			threadID, second.Key)
	}
	if !second.Valid() {
		t.Errorf("话题内消息的作用域应当可用: %+v", second)
	}
	// The type must not change to thread: the key must have exactly the same shape as the root message's to be reused.
	if second.Type != feishu.ScopeMessage {
		t.Errorf("作用域类型应当仍是 %q，实际 %q", feishu.ScopeMessage, second.Type)
	}

	// Follow-ups in another thread must land on a different key; otherwise all threads blur into one context.
	other := feishu.ScopeForMessage(&lark.InboundMessage{
		MessageID: "om_other2", ChatID: "oc_p", ChatType: feishu.ChatTypeP2P,
		RootID: "om_other_root",
	})
	if other.Key == second.Key {
		t.Errorf("两个不同话题共用了键 %q，上下文会互相污染", other.Key)
	}
}

// TestScopeForMessageRejectsEmpty: when the key degrades to only the prefix, it must be judged unusable.
//
// Without blocking it, a bunch of messages with empty chat_id would be blurred into the same "chat:" scope,
// with unrelated people sharing one context. The DM half is the same: with an empty message_id all DM messages would share
// one "message:" key, turning "one session per message" exactly into "everyone shares one session",
// far worse than no context.
func TestScopeForMessageRejectsEmpty(t *testing.T) {
	if feishu.ScopeForMessage(nil).Valid() {
		t.Error("nil 消息不该算出可用作用域")
	}
	empty := feishu.ScopeForMessage(&lark.InboundMessage{ChatType: "group"})
	if empty.Valid() {
		t.Errorf("chat_id 与锚点全空时不该可用，得到 %+v", empty)
	}
	// DM missing message_id: fall back to a one-off session (the degraded branch of ensureScopedRun),
	// instead of letting all DMs share one empty key.
	noMsgID := feishu.ScopeForMessage(&lark.InboundMessage{ChatID: "oc_p", ChatType: feishu.ChatTypeP2P})
	if noMsgID.Valid() {
		t.Errorf("单聊缺 message_id 时不该可用，得到 %+v", noMsgID)
	}
}

// TestScopeKeysDoNotCollideAcrossTypes: when a group ID / thread ID / message ID have the same literal value, they must not count as
// the same scope.
//
// That is why keys carry a type prefix: without it, a group called "x", a thread called "x",
// and a DM message with ID "x" would share context.
func TestScopeKeysDoNotCollideAcrossTypes(t *testing.T) {
	asChat := feishu.ScopeForMessage(&lark.InboundMessage{ChatID: "x", ChatType: "group"})
	asThread := feishu.ScopeForMessage(&lark.InboundMessage{
		ChatID: "other", ChatType: "topic_group", ThreadID: "x",
	})
	asMessage := feishu.ScopeForMessage(&lark.InboundMessage{
		MessageID: "x", ChatID: "another", ChatType: feishu.ChatTypeP2P,
	})

	keys := map[string]string{
		"群":      asChat.Key,
		"话题":     asThread.Key,
		"私聊单条消息": asMessage.Key,
	}
	seen := make(map[string]string, len(keys))
	for name, key := range keys {
		if other, dup := seen[key]; dup {
			t.Fatalf("%s 与 %s 的键撞了: %q", name, other, key)
		}
		seen[key] = name
	}
}

// ---------------------------------------------------------------------------
// Two. Store layer: concurrent binding must converge on the same session
// ---------------------------------------------------------------------------

// TestBindSessionScopeFirstWriterWins: with concurrent binding only one wins, and the losers get the winner's.
//
// This is the core of multi-instance correctness: two processes each created an Ark session, and **both must use the same one**,
// otherwise one group has two contexts. Run with -race.
func TestBindSessionScopeFirstWriterWins(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ctx := context.Background()

		const racers = 8
		var wg sync.WaitGroup
		results := make([]feishu.SessionScopeRecord, racers)
		bounds := make([]bool, racers)
		errs := make([]error, racers)

		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				// Each "instance" created its own session, then races for the binding.
				results[i], bounds[i], errs[i] = store.BindSessionScope(ctx, feishu.SessionScopeRecord{
					LarkAppID:   "cli_1",
					ScopeKey:    "chat:oc_race",
					AgentID:     "agent_1",
					RunID:       fmt.Sprintf("sesn-%d", i),
					WorkspaceID: testWorkspace,
					ScopeType:   feishu.ScopeChat,
					ChatID:      "oc_race",
				})
			}(i)
		}
		wg.Wait()

		winners := 0
		var runID string
		for i := 0; i < racers; i++ {
			if errs[i] != nil {
				t.Fatalf("第 %d 个绑定报错: %v", i, errs[i])
			}
			if bounds[i] {
				winners++
				runID = results[i].RunID
			}
		}
		if winners != 1 {
			t.Fatalf("应当恰好一个赢家，实际 %d 个", winners)
		}
		// Key assertion: everyone got the same session.
		for i := 0; i < racers; i++ {
			if results[i].RunID != runID {
				t.Errorf("第 %d 个拿到 %q，应当与赢家一致 %q",
					i, results[i].RunID, runID)
			}
		}

		// And so must the stored one.
		got, err := store.GetSessionScope(ctx, "cli_1", "chat:oc_race", "agent_1")
		if err != nil {
			t.Fatalf("回读绑定失败: %v", err)
		}
		if got.RunID != runID {
			t.Errorf("库里存的是 %q，期望 %q", got.RunID, runID)
		}
	})
}

// TestGetSessionScopeUnbound: never bound must return ErrScopeNotBound, not some other error.
//
// The inbound path relies on this sentinel to route "first time speaking"; mistaking it would treat it as a failure and drop the message.
func TestGetSessionScopeUnbound(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		_, err := store.GetSessionScope(context.Background(), "cli_1", "chat:nope", "agent_1")
		if !errors.Is(err, feishu.ErrScopeNotBound) {
			t.Fatalf("期望 ErrScopeNotBound，实际 %v", err)
		}
	})
}

// TestSessionScopeIsolation: different scopes / different agents must be independent.
func TestSessionScopeIsolation(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ctx := context.Background()
		bind := func(scopeKey, agentID, runID string) {
			t.Helper()
			if _, ok, err := store.BindSessionScope(ctx, feishu.SessionScopeRecord{
				LarkAppID: "cli_1", ScopeKey: scopeKey, AgentID: agentID,
				RunID: runID, WorkspaceID: testWorkspace,
				ScopeType: feishu.ScopeChat, ChatID: "oc_1",
			}); err != nil || !ok {
				t.Fatalf("绑定 %s/%s 失败: ok=%v err=%v", scopeKey, agentID, ok, err)
			}
		}
		bind("chat:oc_1", "agent_1", "sesn-a")
		bind("thread:omt_1", "agent_1", "sesn-b")
		// Same scope with a different agent: it should be another conversation, not inheriting the old agent's history.
		bind("chat:oc_1", "agent_2", "sesn-c")

		for _, tc := range []struct{ scopeKey, agentID, want string }{
			{"chat:oc_1", "agent_1", "sesn-a"},
			{"thread:omt_1", "agent_1", "sesn-b"},
			{"chat:oc_1", "agent_2", "sesn-c"},
		} {
			got, err := store.GetSessionScope(ctx, "cli_1", tc.scopeKey, tc.agentID)
			if err != nil {
				t.Fatalf("查 %s/%s 失败: %v", tc.scopeKey, tc.agentID, err)
			}
			if got.RunID != tc.want {
				t.Errorf("%s/%s 期望 %q 实际 %q",
					tc.scopeKey, tc.agentID, tc.want, got.RunID)
			}
		}
	})
}

// TestRebindSessionScopeCAS: rebinding must use CAS: change only if it is still bound to that invalid session.
func TestRebindSessionScopeCAS(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ctx := context.Background()
		rec := feishu.SessionScopeRecord{
			LarkAppID: "cli_1", ScopeKey: "chat:oc_1", AgentID: "agent_1",
			RunID: "sesn-old", WorkspaceID: testWorkspace,
			ScopeType: feishu.ScopeChat, ChatID: "oc_1",
		}
		if _, ok, err := store.BindSessionScope(ctx, rec); err != nil || !ok {
			t.Fatalf("初次绑定失败: ok=%v err=%v", ok, err)
		}

		// Expected value matches: the change succeeds.
		rec.RunID = "sesn-new"
		ok, err := store.RebindSessionScope(ctx, rec, "sesn-old")
		if err != nil {
			t.Fatalf("改绑失败: %v", err)
		}
		if !ok {
			t.Fatal("期望值对得上时应当改绑成功")
		}

		// Changing again with the old value: must be rejected (someone already changed it).
		rec.RunID = "sesn-third"
		ok, err = store.RebindSessionScope(ctx, rec, "sesn-old")
		if err != nil {
			t.Fatalf("第二次改绑报错: %v", err)
		}
		if ok {
			t.Error("期望值已过期时不该改绑成功——这正是防覆盖的那道 CAS")
		}

		got, err := store.GetSessionScope(ctx, "cli_1", "chat:oc_1", "agent_1")
		if err != nil {
			t.Fatalf("回读失败: %v", err)
		}
		if got.RunID != "sesn-new" {
			t.Errorf("最终应当是 sesn-new，实际 %q", got.RunID)
		}
	})
}

// TestTouchSessionScopeCountsTurns: the turn count must accumulate, without losing updates under concurrency.
func TestTouchSessionScopeCountsTurns(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ctx := context.Background()
		if _, ok, err := store.BindSessionScope(ctx, feishu.SessionScopeRecord{
			LarkAppID: "cli_1", ScopeKey: "chat:oc_1", AgentID: "agent_1",
			RunID: "sesn-1", WorkspaceID: testWorkspace,
			ScopeType: feishu.ScopeChat, ChatID: "oc_1",
		}); err != nil || !ok {
			t.Fatalf("绑定失败: ok=%v err=%v", ok, err)
		}

		const bumps = 5
		var wg sync.WaitGroup
		for i := 0; i < bumps; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := store.TouchSessionScope(
					ctx, "cli_1", "chat:oc_1", "agent_1", time.Time{},
				); err != nil {
					t.Errorf("touch 失败: %v", err)
				}
			}()
		}
		wg.Wait()

		got, err := store.GetSessionScope(ctx, "cli_1", "chat:oc_1", "agent_1")
		if err != nil {
			t.Fatalf("回读失败: %v", err)
		}
		// Starting 1 turn + bumps. On the SQL side it is turn_count + 1, not read then write, so nothing should be lost.
		if want := 1 + bumps; got.TurnCount != want {
			t.Errorf("轮次计数期望 %d 实际 %d（丢更新说明用了先读再写）", want, got.TurnCount)
		}
	})
}

// TestTouchSessionScopeMissingIsNotError: touching a scope that does not exist is not an error.
//
// It is only operational information; erroring on it would fail a message that should have been answered.
func TestTouchSessionScopeMissingIsNotError(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		if err := store.TouchSessionScope(
			context.Background(), "cli_1", "chat:nope", "agent_1", time.Now(),
		); err != nil {
			t.Fatalf("不存在的作用域 touch 不该报错: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Three. Serial guard: only one turn at a time on a session
// ---------------------------------------------------------------------------

// TestTryBeginTurnSerializesOneRun: the second turn on the same session must be blocked.
//
// Without blocking, two cardUpdaters would subscribe to the same session's event stream at once and write each other's replies
// onto their own cards: both cards would show the answers to two questions mixed together.
func TestTryBeginTurnSerializesOneRun(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ctx := context.Background()
		rec := feishu.InflightTurnRecord{
			RunID: "sesn-1", LarkAppID: "cli_1", ChatID: "oc_1",
			SrcMessageID: "om_1", Status: feishumodel.InflightOpen, StartedAt: time.Now(),
		}
		ok, err := store.TryBeginTurn(ctx, rec, time.Hour)
		if err != nil || !ok {
			t.Fatalf("第一轮应当开得起来: ok=%v err=%v", ok, err)
		}

		second := rec
		second.SrcMessageID = "om_2"
		second.StartedAt = time.Now()
		ok, err = store.TryBeginTurn(ctx, second, time.Hour)
		if err != nil {
			t.Fatalf("第二轮报错: %v", err)
		}
		if ok {
			t.Fatal("同一条会话上第二轮必须被挡住，否则两轮会把卡片写乱")
		}

		// The first turn finishes → the lock is released → the next line can start.
		if err := store.DeleteInflightTurn(ctx, "sesn-1"); err != nil {
			t.Fatalf("清理失败: %v", err)
		}
		ok, err = store.TryBeginTurn(ctx, second, time.Hour)
		if err != nil || !ok {
			t.Fatalf("前一轮结束后应当能开新轮: ok=%v err=%v", ok, err)
		}
	})
}

// TestTryBeginTurnOnlyOneWinnerUnderRace: with concurrent starts only one may succeed.
func TestTryBeginTurnOnlyOneWinnerUnderRace(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ctx := context.Background()

		const racers = 8
		var wg sync.WaitGroup
		var mu sync.Mutex
		winners := 0
		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				ok, err := store.TryBeginTurn(ctx, feishu.InflightTurnRecord{
					RunID: "sesn-race", LarkAppID: "cli_1", ChatID: "oc_1",
					SrcMessageID: fmt.Sprintf("om_%d", i),
					Status:       feishumodel.InflightOpen, StartedAt: time.Now(),
				}, time.Hour)
				if err != nil {
					t.Errorf("开轮报错: %v", err)
					return
				}
				if ok {
					mu.Lock()
					winners++
					mu.Unlock()
				}
			}(i)
		}
		wg.Wait()

		if winners != 1 {
			t.Fatalf("并发开轮应当恰好一个成功，实际 %d 个", winners)
		}
	})
}

// TestTryBeginTurnPreemptsStaleRecord: a stale record must be preemptible.
//
// The holder's instance may be gone before it could delete the record (OOM, kill -9). Without preemption this group would be
// blocked until the next restart scan cleans it up, and meanwhile whatever the user asks only gets "the previous message is still being processed".
//
// # Why "a tiny staleness window + really waiting a bit" instead of faking a very old StartedAt
//
// Because TryBeginTurn overwrites UpdatedAt with the current time, and the liveness rule is
// max(StartedAt, UpdatedAt). That is deliberate: a long task's StartedAt can be very old, but as long as it keeps
// refreshing the card it is not dead and must not be taken over. So changing only StartedAt cannot fake "stale";
// time really has to move forward.
func TestTryBeginTurnPreemptsStaleRecord(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ctx := context.Background()
		if ok, err := store.TryBeginTurn(ctx, feishu.InflightTurnRecord{
			RunID: "sesn-1", LarkAppID: "cli_1", ChatID: "oc_1",
			SrcMessageID: "om_old", Status: feishumodel.InflightOpen, StartedAt: time.Now(),
		}, time.Hour); err != nil || !ok {
			t.Fatalf("预置占用记录失败: ok=%v err=%v", ok, err)
		}

		// While fresh it must not be taken over; this half matters just as much, otherwise "preemption" would degrade into "anyone can take it",
		// and the serial guard would be useless.
		if ok, err := store.TryBeginTurn(ctx, feishu.InflightTurnRecord{
			RunID: "sesn-1", LarkAppID: "cli_1", ChatID: "oc_1",
			SrcMessageID: "om_new", Status: feishumodel.InflightOpen, StartedAt: time.Now(),
		}, time.Hour); err != nil || ok {
			t.Fatalf("新鲜记录不该被抢占: ok=%v err=%v", ok, err)
		}

		// Let a little time really pass, then take it with a tiny staleness window.
		time.Sleep(5 * time.Millisecond)
		ok, err := store.TryBeginTurn(ctx, feishu.InflightTurnRecord{
			RunID: "sesn-1", LarkAppID: "cli_1", ChatID: "oc_1",
			SrcMessageID: "om_new", Status: feishumodel.InflightOpen, StartedAt: time.Now(),
		}, time.Millisecond)
		if err != nil {
			t.Fatalf("抢占报错: %v", err)
		}
		if !ok {
			t.Fatal("陈旧记录应当能被抢占，否则这个群会一直被挡住")
		}

		// After preemption the record must become the new turn's, otherwise recovery would rescue the wrong message.
		turns, err := store.ListInflightTurns(ctx)
		if err != nil {
			t.Fatalf("列轮次失败: %v", err)
		}
		if len(turns) != 1 {
			t.Fatalf("应当仍只有 1 条，实际 %d", len(turns))
		}
		if turns[0].SrcMessageID != "om_new" {
			t.Errorf("抢占后源消息应当是 om_new，实际 %q", turns[0].SrcMessageID)
		}
		// retry_count must reset to zero: this is a new turn and should not carry the previous turn's resume count,
		// otherwise long-chatting groups would be misjudged as "resumed too many times" and recovery refused.
		if turns[0].RetryCount != 0 {
			t.Errorf("抢占后续跑次数应当归零，实际 %d", turns[0].RetryCount)
		}
	})
}

// ---------------------------------------------------------------------------
// Four. Full path: reuse and isolation
// ---------------------------------------------------------------------------

// scopedMsg builds an inbound message with thread information.
func scopedMsg(id, chatID, chatType, threadID, text string) *lark.InboundMessage {
	return &lark.InboundMessage{
		AppID:     "cli_1",
		MessageID: id,
		ChatID:    chatID,
		ChatType:  chatType,
		ThreadID:  threadID,
		Text:      text,
		Mentioned: true,
	}
}

// TestInboundReusesSessionInSameGroup: two messages in the same group must land on the same session.
//
// **This is the main defect this change fixes**: before it, each message created its own session, and the agent had no context at all.
func TestInboundReusesSessionInSameGroup(t *testing.T) {
	conv := &fakeConversations{
		agentIDs: []string{"agent_1"},
		frames:   [][]byte{agentFrame("好")},
	}
	in, store := inboundFixture(t, conv, &fakeSender{})
	ctx := context.Background()

	if err := in.Handle(ctx, scopedMsg("om_1", "oc_1", "group", "", "杭州天气怎么样")); err != nil {
		t.Fatalf("第一条失败: %v", err)
	}
	if err := in.Handle(ctx, scopedMsg("om_2", "oc_1", "group", "", "那明天呢")); err != nil {
		t.Fatalf("第二条失败: %v", err)
	}

	if conv.createCount() != 1 {
		t.Fatalf("同一个群应当只建一次会话，实际建了 %d 次——上下文丢了", conv.createCount())
	}
	bound, err := store.GetSessionScope(ctx, "cli_1", "chat:oc_1", "agent_1")
	if err != nil {
		t.Fatalf("查绑定失败: %v", err)
	}
	if bound.TurnCount != 2 {
		t.Errorf("应当记下 2 轮，实际 %d", bound.TurnCount)
	}
}

// TestInboundSeparatesTopicsInSameGroup: two threads in the same group must each get their own session.
func TestInboundSeparatesTopicsInSameGroup(t *testing.T) {
	conv := &fakeConversations{
		agentIDs: []string{"agent_1"},
		frames:   [][]byte{agentFrame("好")},
	}
	in, store := inboundFixture(t, conv, &fakeSender{})
	ctx := context.Background()

	if err := in.Handle(ctx, scopedMsg("om_1", "oc_1", "topic_group", "omt_a", "话题一")); err != nil {
		t.Fatalf("话题一失败: %v", err)
	}
	if err := in.Handle(ctx, scopedMsg("om_2", "oc_1", "topic_group", "omt_b", "话题二")); err != nil {
		t.Fatalf("话题二失败: %v", err)
	}
	// Back to thread one: it must reuse its session, not create another.
	if err := in.Handle(ctx, scopedMsg("om_3", "oc_1", "topic_group", "omt_a", "接着说")); err != nil {
		t.Fatalf("话题一续聊失败: %v", err)
	}

	if conv.createCount() != 2 {
		t.Fatalf("两个话题应当建两条会话，实际 %d 次", conv.createCount())
	}
	a, err := store.GetSessionScope(ctx, "cli_1", "thread:omt_a", "agent_1")
	if err != nil {
		t.Fatalf("查话题一失败: %v", err)
	}
	b, err := store.GetSessionScope(ctx, "cli_1", "thread:omt_b", "agent_1")
	if err != nil {
		t.Fatalf("查话题二失败: %v", err)
	}
	if a.RunID == b.RunID {
		t.Fatalf("两个话题共用了会话 %q，上下文会互相污染", a.RunID)
	}
	if a.TurnCount != 2 {
		t.Errorf("话题一应当记下 2 轮，实际 %d", a.TurnCount)
	}
}

// TestInboundCreatesNewSessionPerDirectMessage: each **new conversation start** on the DM main line must land on a
// brand-new, independent session.
//
// # This and "reuse inside a thread" are two different user actions
//
// Back when DMs reused the whole session by chat_id, the same person typing two lines shared context. Now each message on the main line
// gets its own session; the benefit is that however the previous turn got stuck, it never drags down the next line (the serial lock is on run_id,
// see the comment on TryBeginTurn in runAndReply).
//
// **The cost falls only on the main line**: two lines the user types in a row on the main line share no memory. To follow up continuously, reply in the thread
// formed by the bot's reply; that path is covered by TestInboundReusesSessionInDirectThread.
//
// The assertion is on **the session IDs themselves being different**, not just "created twice": the fake's
// CreateConversation returns a different ID each time exactly to make this assertable.
func TestInboundCreatesNewSessionPerDirectMessage(t *testing.T) {
	conv := &fakeConversations{
		agentIDs: []string{"agent_1"},
		frames:   [][]byte{agentFrame("好")},
	}
	in, store := inboundFixture(t, conv, &fakeSender{})
	ctx := context.Background()

	// Both are new conversation starts on the main line, with no reply anchors. With one they would be follow-ups in a thread,
	// which should reuse rather than create; that is another test's job.
	first := scopedMsg("om_1", "oc_p", feishu.ChatTypeP2P, "", "在吗")
	second := scopedMsg("om_2", "oc_p", feishu.ChatTypeP2P, "", "另起一个不相干的问题")

	if err := in.Handle(ctx, first); err != nil {
		t.Fatalf("第一条失败: %v", err)
	}
	if err := in.Handle(ctx, second); err != nil {
		t.Fatalf("第二条失败: %v", err)
	}

	if conv.createCount() != 2 {
		t.Fatalf("私聊两条新话头应当各建一条会话，实际建了 %d 次", conv.createCount())
	}

	// Each message's scope must be stored, pointing to **different** sessions.
	a, err := store.GetSessionScope(ctx, "cli_1", "message:om_1", "agent_1")
	if err != nil {
		t.Fatalf("第一条应当按 message 作用域绑定: %v", err)
	}
	b, err := store.GetSessionScope(ctx, "cli_1", "message:om_2", "agent_1")
	if err != nil {
		t.Fatalf("第二条应当按 message 作用域绑定: %v", err)
	}
	if a.RunID == b.RunID {
		t.Fatalf("两条私聊新话头共用了会话 %q——上下文没有隔开", a.RunID)
	}
	// Each ran only one turn: with reuse this would be 2.
	if a.TurnCount != 1 || b.TurnCount != 1 {
		t.Errorf("每条私聊新话头应当各自只有 1 轮，实际 %d / %d", a.TurnCount, b.TurnCount)
	}
	if a.ScopeType != feishu.ScopeMessage || b.ScopeType != feishu.ScopeMessage {
		t.Errorf("作用域类型应当是 %q，实际 %q / %q", feishu.ScopeMessage, a.ScopeType, b.ScopeType)
	}
	// The old per-chat reuse key must not appear again, otherwise the change was only half done.
	if _, err := store.GetSessionScope(ctx, "cli_1", "chat:oc_p", "agent_1"); !errors.Is(err, feishu.ErrScopeNotBound) {
		t.Errorf("私聊不该再按 chat 作用域绑定，实际 err=%v", err)
	}

	// Both messages really sent their question into their own session; creating a session without sending the message means no answer.
	if got := len(conv.appendedEvents()); got != 2 {
		t.Errorf("两条私聊消息都该发进会话，实际发了 %d 次", got)
	}
}

// TestInboundReusesSessionInDirectThread: in a DM, when the user keeps talking in the thread formed by the bot's reply,
// the session of **the thread's root message** must be reused.
//
// # This is exactly what the user reported
//
// Send "hello" in a DM → the bot replies → Feishu folds this question and answer into a thread → the user asks in that thread
// "do you have the lark command", expecting the bot to remember what was just said. Before the fix it opened another brand-new
// session (two sessions each with turn_count=1), and the agent forgot everything.
//
// The root cause was that the p2p short-circuit in scope.go came before the thread check, so threadAnchor never
// got a chance to be called in DMs. **This test is the regression guard for that order.**
func TestInboundReusesSessionInDirectThread(t *testing.T) {
	conv := &fakeConversations{
		agentIDs: []string{"agent_1"},
		frames:   [][]byte{agentFrame("好")},
	}
	in, store := inboundFixture(t, conv, &fakeSender{})
	ctx := context.Background()

	// First: a new conversation start on the DM main line; after the bot replies, Feishu folds it into a thread.
	if err := in.Handle(ctx, scopedMsg("om_root", "oc_p", feishu.ChatTypeP2P, "", "你好")); err != nil {
		t.Fatalf("根消息失败: %v", err)
	}
	// Second: sent by the user **inside that thread**. The measured shape is root_id / parent_id both pointing to the root message,
	// plus a thread_id starting with omt_.
	second := scopedMsg("om_second", "oc_p", feishu.ChatTypeP2P, "omt_1", "你有 lark 命令吗")
	second.RootID = "om_root"
	second.ParentID = "om_root"
	if err := in.Handle(ctx, second); err != nil {
		t.Fatalf("话题内第二条失败: %v", err)
	}

	// Core assertion: only one session was created, and the second reused it.
	if conv.createCount() != 1 {
		t.Fatalf("话题内的后续消息应当复用根消息那条会话，实际建了 %d 条——"+
			"agent 会完全不记得前面聊过什么", conv.createCount())
	}
	bound, err := store.GetSessionScope(ctx, "cli_1", "message:om_root", "agent_1")
	if err != nil {
		t.Fatalf("根消息那条作用域应当还在: %v", err)
	}
	if bound.TurnCount != 2 {
		t.Errorf("根消息那条会话应当记下 2 轮，实际 %d", bound.TurnCount)
	}
	// The second must not bind another one with its own message_id: that was exactly the behavior before the fix.
	if _, err := store.GetSessionScope(ctx, "cli_1", "message:om_second", "agent_1"); !errors.Is(err, feishu.ErrScopeNotBound) {
		t.Errorf("话题内的后续消息不该按自己的 message_id 另绑一条，实际 err=%v", err)
	}
	// thread_id must not become the key either: then the root message's session still could not be reused.
	if _, err := store.GetSessionScope(ctx, "cli_1", "message:omt_1", "agent_1"); !errors.Is(err, feishu.ErrScopeNotBound) {
		t.Errorf("键不该用 thread_id 拼，实际 err=%v", err)
	}

	// Both messages really went into **the same** session, so the agent can see the context.
	if got := len(conv.appendedEvents()); got != 2 {
		t.Errorf("两条消息都该发进会话，实际发了 %d 次", got)
	}

	// A new line on the main line (no anchor) is still a new session: reuse inside a thread must not reuse the main line too.
	if err := in.Handle(ctx, scopedMsg("om_fresh", "oc_p", feishu.ChatTypeP2P, "", "另起一个话头")); err != nil {
		t.Fatalf("主线新话头失败: %v", err)
	}
	if conv.createCount() != 2 {
		t.Fatalf("主线上的新话头应当另建一条会话，实际共建了 %d 条", conv.createCount())
	}
}

// TestInboundDirectMessageNotBlockedByStuckTurn: the next DM line must not be blocked by a leftover lock from the previous turn.
//
// # This is exactly what "one session per message" buys
//
// The serial lock is on run_id (inflight_turns uses run_id as the primary key). With session reuse, a previous turn
// that failed to wrap up normally (Ark not closing the stream, the process killed, deleting the record failed; all have happened in production) would make
// every following line the user asks get "the previous message is still being processed", until runTimeout (10 minutes) runs out.
// DMs are one-on-one rapid typing, so this pitfall is hardest to bear in DMs.
//
// Once each DM message has its own session, the new message's run_id differs from the stuck one, and the locks naturally never meet.
func TestInboundDirectMessageNotBlockedByStuckTurn(t *testing.T) {
	conv := &fakeConversations{
		agentIDs: []string{"agent_1"},
		frames:   [][]byte{agentFrame("好")},
	}
	sender := &fakeSender{}
	in, store := inboundFixture(t, conv, sender)
	ctx := context.Background()

	if err := in.Handle(ctx, scopedMsg("om_1", "oc_p", feishu.ChatTypeP2P, "", "第一个问题")); err != nil {
		t.Fatalf("第一条失败: %v", err)
	}
	bound, err := store.GetSessionScope(ctx, "cli_1", "message:om_1", "agent_1")
	if err != nil {
		t.Fatalf("查第一条的绑定失败: %v", err)
	}
	// Simulate "the previous turn's lock is left over": the record is kept, and RunStatus says it is still running,
	// so reclaimFinishedTurn cannot rescue it either. With session reuse this would be a deadlock.
	conv.status = "running"
	if ok, err := store.TryBeginTurn(ctx, feishu.InflightTurnRecord{
		RunID: bound.RunID, LarkAppID: "cli_1", ChatID: "oc_p",
		SrcMessageID: "om_stuck", Status: feishumodel.InflightOpen, StartedAt: time.Now(),
	}, time.Hour); err != nil || !ok {
		t.Fatalf("预置残留锁失败: ok=%v err=%v", ok, err)
	}

	before := len(conv.appendedEvents())
	if err := in.Handle(ctx, scopedMsg("om_2", "oc_p", feishu.ChatTypeP2P, "", "第二个问题")); err != nil {
		t.Fatalf("第二条失败: %v", err)
	}

	// Key point: the second must really run, instead of getting "the previous message is still being processed".
	if got := len(conv.appendedEvents()); got != before+1 {
		t.Fatalf("第二条私聊消息应当照常开跑，实际往会话里发了 %d 次", got-before)
	}
	for _, c := range sender.sent() {
		if strings.Contains(c.text, "上一句还在处理") {
			t.Fatal("私聊不该被上一轮的残留锁挡住——这正是每条消息一个会话要解决的问题")
		}
	}
}

// TestInboundReusesSessionInGroupDespiteP2PChange: the reuse rule for group chats is unaffected by the DM change.
//
// This test is kept separately because the two rules live in the same function: after switching DMs to message_id,
// it is easy to casually send group chats down the same branch too, and then groups have no context, with no error.
func TestInboundReusesSessionInGroupDespiteP2PChange(t *testing.T) {
	conv := &fakeConversations{
		agentIDs: []string{"agent_1"},
		frames:   [][]byte{agentFrame("好")},
	}
	in, store := inboundFixture(t, conv, &fakeSender{})
	ctx := context.Background()

	// Two messages in the same normal group with different message_ids: group chats must ignore it and reuse by group.
	if err := in.Handle(ctx, scopedMsg("om_1", "oc_g", "group", "", "杭州天气怎么样")); err != nil {
		t.Fatalf("第一条失败: %v", err)
	}
	if err := in.Handle(ctx, scopedMsg("om_2", "oc_g", "group", "", "那明天呢")); err != nil {
		t.Fatalf("第二条失败: %v", err)
	}
	if conv.createCount() != 1 {
		t.Fatalf("普通群应当只建一次会话，实际 %d 次——群聊的上下文丢了", conv.createCount())
	}
	if _, err := store.GetSessionScope(ctx, "cli_1", "message:om_1", "agent_1"); !errors.Is(err, feishu.ErrScopeNotBound) {
		t.Errorf("群消息不该按 message 作用域绑定，实际 err=%v", err)
	}

	// Topic group: reuse within the same thread too, and separate between two threads.
	if err := in.Handle(ctx, scopedMsg("om_3", "oc_t", "topic_group", "omt_a", "话题一")); err != nil {
		t.Fatalf("话题一失败: %v", err)
	}
	if err := in.Handle(ctx, scopedMsg("om_4", "oc_t", "topic_group", "omt_a", "话题一接着说")); err != nil {
		t.Fatalf("话题一续聊失败: %v", err)
	}
	if err := in.Handle(ctx, scopedMsg("om_5", "oc_t", "topic_group", "omt_b", "话题二")); err != nil {
		t.Fatalf("话题二失败: %v", err)
	}

	// group(1) + thread a(1) + thread b(1) = 3 sessions.
	if conv.createCount() != 3 {
		t.Fatalf("应当共建 3 条会话（群 / 话题 a / 话题 b），实际 %d 次", conv.createCount())
	}
	topicA, err := store.GetSessionScope(ctx, "cli_1", "thread:omt_a", "agent_1")
	if err != nil {
		t.Fatalf("查话题一失败: %v", err)
	}
	topicB, err := store.GetSessionScope(ctx, "cli_1", "thread:omt_b", "agent_1")
	if err != nil {
		t.Fatalf("查话题二失败: %v", err)
	}
	if topicA.RunID == topicB.RunID {
		t.Fatalf("两个话题共用了会话 %q，上下文会互相污染", topicA.RunID)
	}
	if topicA.TurnCount != 2 {
		t.Errorf("话题一应当记下 2 轮（话题内复用），实际 %d", topicA.TurnCount)
	}
}

// TestInboundDirectMessageRedeliveryDoesNotCreateSecondSession: when the same DM message is delivered again
// it must not create a second session.
//
// # Why this one must be pinned separately
//
// "One session per message" has a trap in itself: Feishu redelivers the same message (network jitter,
// slow handling on our side, an error we returned). If the implementation were "DM creates a session directly" instead of "DM goes through
// the message_id scope", a redelivery would create a second session for the same line and answer the same question twice.
//
// This covers two lines of defense at once: the inbound dedup table (within one process) and the message scope
// (when two instances receive a redelivery at once, the database decides; see the comment on ensureScopedRun).
func TestInboundDirectMessageRedeliveryDoesNotCreateSecondSession(t *testing.T) {
	conv := &fakeConversations{
		agentIDs: []string{"agent_1"},
		frames:   [][]byte{agentFrame("好")},
	}
	in, store := inboundFixture(t, conv, &fakeSender{})
	ctx := context.Background()

	msg := scopedMsg("om_dup", "oc_p", feishu.ChatTypeP2P, "", "同一个问题")
	if err := in.Handle(ctx, msg); err != nil {
		t.Fatalf("第一次投递失败: %v", err)
	}
	// Feishu delivers the same message once more: it must be quietly ignored and return no error (otherwise it would be delivered again).
	if err := in.Handle(ctx, msg); err != nil {
		t.Fatalf("重复投递不该返回错误（会让飞书继续重投）: %v", err)
	}

	if conv.createCount() != 1 {
		t.Fatalf("同一条私聊消息重投不该建出第二个会话，实际建了 %d 次", conv.createCount())
	}
	if got := len(conv.appendedEvents()); got != 1 {
		t.Errorf("同一个问题不该被答两遍，实际发进会话 %d 次", got)
	}
	if _, err := store.GetSessionScope(ctx, "cli_1", "message:om_dup", "agent_1"); err != nil {
		t.Errorf("重投后作用域仍应绑着那条会话: %v", err)
	}
}

// TestInboundDirectThreadRedeliveryDoesNotCreateSecondSession: when that message inside the thread is delivered again,
// it likewise must not create a second session or answer the same question twice.
//
// # Why the in-thread half must be pinned separately
//
// The previous test takes the "new conversation start" branch, whose key is the message's own message_id; messages inside a thread take
// **another branch**, whose key is root_id. The two branches compute keys separately, so whether dedup still holds must be checked for each; testing only one
// lets the other slip.
//
// And the in-thread one is easier to break: once someone changes the key into a combination like "root_id + its own message_id"
// (looks more "precise"), the key is still stable on redelivery and this test stays green, but **reuse** breaks on the spot. So
// the reuse half is guarded by TestInboundReusesSessionInDirectThread, and this one only handles dedup.
func TestInboundDirectThreadRedeliveryDoesNotCreateSecondSession(t *testing.T) {
	conv := &fakeConversations{
		agentIDs: []string{"agent_1"},
		frames:   [][]byte{agentFrame("好")},
	}
	in, store := inboundFixture(t, conv, &fakeSender{})
	ctx := context.Background()

	if err := in.Handle(ctx, scopedMsg("om_root", "oc_p", feishu.ChatTypeP2P, "", "你好")); err != nil {
		t.Fatalf("根消息失败: %v", err)
	}
	// The second message in the thread, delivered twice.
	second := scopedMsg("om_second", "oc_p", feishu.ChatTypeP2P, "omt_1", "你有 lark 命令吗")
	second.RootID = "om_root"
	second.ParentID = "om_root"

	if err := in.Handle(ctx, second); err != nil {
		t.Fatalf("话题内第一次投递失败: %v", err)
	}
	if err := in.Handle(ctx, second); err != nil {
		t.Fatalf("重复投递不该返回错误（会让飞书继续重投）: %v", err)
	}

	// Only the root message's session exists throughout: the in-thread message reuses it, and the redelivery is blocked by dedup.
	if conv.createCount() != 1 {
		t.Fatalf("话题内消息重投不该建出第二个会话，实际建了 %d 次", conv.createCount())
	}
	// Root message once + in-thread once = 2 times; the redelivered one must not be sent again.
	if got := len(conv.appendedEvents()); got != 2 {
		t.Errorf("同一个问题不该被答两遍，实际发进会话 %d 次", got)
	}
	bound, err := store.GetSessionScope(ctx, "cli_1", "message:om_root", "agent_1")
	if err != nil {
		t.Fatalf("重投后作用域仍应绑着根消息那条会话: %v", err)
	}
	// The redelivery is blocked before creating a session, so the turn count should only go up once (root message 1 + in-thread 1).
	if bound.TurnCount != 2 {
		t.Errorf("轮次应当是 2（重投不计），实际 %d", bound.TurnCount)
	}
}

// TestInboundSeparatesDifferentChats: two different groups must not share a session.
func TestInboundSeparatesDifferentChats(t *testing.T) {
	conv := &fakeConversations{
		agentIDs: []string{"agent_1"},
		frames:   [][]byte{agentFrame("好")},
	}
	in, _ := inboundFixture(t, conv, &fakeSender{})
	ctx := context.Background()

	if err := in.Handle(ctx, scopedMsg("om_1", "oc_a", "group", "", "甲群")); err != nil {
		t.Fatalf("甲群失败: %v", err)
	}
	if err := in.Handle(ctx, scopedMsg("om_2", "oc_b", "group", "", "乙群")); err != nil {
		t.Fatalf("乙群失败: %v", err)
	}
	if conv.createCount() != 2 {
		t.Fatalf("两个群应当各建一条会话，实际 %d 次", conv.createCount())
	}
}

// TestInboundCardIdempotencyKeyIsPerTurn: when reusing a session, the card creation idempotency keys of two turns must differ.
//
// # This test locks down a silent data loss
//
// With only runID as the idempotency key, Feishu would treat the second turn's card creation as a duplicate delivery of the first turn's and return the same
// message_id, so the second turn's answer PATCHes over the first turn's card: the user's first answer vanishes out of nowhere,
// while both calls return success. The idempotencyKey comment and tests in shared/infra/lark say the same.
func TestInboundCardIdempotencyKeyIsPerTurn(t *testing.T) {
	conv := &fakeConversations{
		agentIDs: []string{"agent_1"},
		frames:   [][]byte{agentFrame("答案")},
	}
	sender := &fakeSender{}
	in, _ := inboundFixture(t, conv, sender)
	ctx := context.Background()

	if err := in.Handle(ctx, scopedMsg("om_1", "oc_1", "group", "", "第一个问题")); err != nil {
		t.Fatalf("第一轮失败: %v", err)
	}
	if err := in.Handle(ctx, scopedMsg("om_2", "oc_1", "group", "", "第二个问题")); err != nil {
		t.Fatalf("第二轮失败: %v", err)
	}

	var keys []string
	for _, c := range sender.sent() {
		if c.kind == "reply" {
			keys = append(keys, c.idem)
		}
	}
	if len(keys) != 2 {
		t.Fatalf("应当建两张卡，实际 %d 张（键: %v）", len(keys), keys)
	}
	if keys[0] == keys[1] {
		t.Fatalf("两轮的建卡幂等键撞了（都是 %q）——第二轮会覆盖第一轮的卡片", keys[0])
	}
	// The key must carry a turn identifier (the source message ID), the one that is both stable and unique.
	for i, want := range []string{"om_1", "om_2"} {
		if !strings.Contains(keys[i], want) {
			t.Errorf("第 %d 个键 %q 里应当含源消息 ID %q", i+1, keys[i], want)
		}
	}
}

// TestProgressDeliveryKeyStableAndDistinct: the chain-of-thought delivery key must be "stable within one turn, different across turns".
//
// Stable is the precondition for idempotency (the value does not change on retry); different is the precondition for **having a chain of thought from the second turn on**:
// the delivery table uses this key as its primary key, and with only runID the second turn would hit the first turn's record that is already final,
// and the driver, seeing Finished(), pushes nothing: the group never shows a thinking process again, with zero errors throughout.
func TestProgressDeliveryKeyStableAndDistinct(t *testing.T) {
	a1 := feishu.ProgressDeliveryKey("sesn-1", "om_1")
	a2 := feishu.ProgressDeliveryKey("sesn-1", "om_1")
	if a1 != a2 {
		t.Errorf("同一轮的键必须稳定: %q vs %q", a1, a2)
	}
	if b := feishu.ProgressDeliveryKey("sesn-1", "om_2"); b == a1 {
		t.Errorf("同一会话不同轮的键必须不同，都是 %q——否则第二轮起没有思维链", b)
	}
	if c := feishu.ProgressDeliveryKey("sesn-2", "om_1"); c == a1 {
		t.Errorf("不同会话的键必须不同，都是 %q", c)
	}
	// Without a source message ID fall back to runID, so retries within one turn are at least still idempotent.
	if got := feishu.ProgressDeliveryKey("sesn-1", ""); got != "sesn-1" {
		t.Errorf("缺源消息 ID 时应退回 runID，实际 %q", got)
	}
}

// TestInboundRejectsConcurrentTurnInSameScope: when the previous turn in the same group has not finished answering,
// **an agent configured for "queue"** must give the second message a clear notice, instead of silently dropping it,
// or running two turns concurrently and messing up the cards.
//
// ⚠️ Queue mode must be turned on explicitly: **the default is redirect** (the empty string of apimodel.FollowUpMode is redirect),
// and the redirect path never goes through replyBusy: it interrupts the previous turn and uses this message instead.
// Without setting it explicitly this test would test another branch, while the error says "the blocked message should get an explanation",
// not pointing at the real cause at all. The redirect branch is covered by feishu/inbound_followup_test.go.
func TestInboundRejectsConcurrentTurnInSameScope(t *testing.T) {
	conv := &fakeConversations{
		agentIDs:       []string{"agent_1"},
		frames:         [][]byte{agentFrame("在想")},
		queueingAgents: map[string]bool{"agent_1": true},
	}
	sender := &fakeSender{}
	in, store := inboundFixture(t, conv, sender)
	ctx := context.Background()

	// Manually hold this session: simulate "the previous turn is still running".
	// Run one turn first to get the runID, then keep the inflight record without deleting it.
	if err := in.Handle(ctx, scopedMsg("om_1", "oc_1", "group", "", "第一个问题")); err != nil {
		t.Fatalf("第一轮失败: %v", err)
	}
	bound, err := store.GetSessionScope(ctx, "cli_1", "chat:oc_1", "agent_1")
	if err != nil {
		t.Fatalf("查绑定失败: %v", err)
	}
	if ok, err := store.TryBeginTurn(ctx, feishu.InflightTurnRecord{
		RunID: bound.RunID, LarkAppID: "cli_1", ChatID: "oc_1",
		SrcMessageID: "om_stuck", Status: feishumodel.InflightOpen, StartedAt: time.Now(),
	}, time.Hour); err != nil || !ok {
		t.Fatalf("预置占用失败: ok=%v err=%v", ok, err)
	}

	before := len(conv.appendedEvents())
	if err := in.Handle(ctx, scopedMsg("om_2", "oc_1", "group", "", "第二个问题")); err != nil {
		t.Fatalf("被挡住的那条不该返回错误（会让飞书重投）: %v", err)
	}
	if got := len(conv.appendedEvents()); got != before {
		t.Errorf("被挡住时不该往会话里发消息，发了 %d 条", got-before)
	}

	// The user must get an explanation, otherwise they think the bot is broken.
	var busy bool
	for _, c := range sender.sent() {
		if strings.Contains(c.text, "上一句还在处理") {
			busy = true
		}
	}
	if !busy {
		t.Error("被挡住的消息应当回一句说明，不能静默丢掉")
	}
}

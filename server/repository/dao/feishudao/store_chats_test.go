// Tests for the conversation (group chat) registry.
//
// They go through eachStore, so **every assertion runs once each against memory and PG** (t.Skip when PG is unreachable).
// The two implementations must match case by case, otherwise "works fine in local memory, wrong on PG".
//
// This table exists because scheduled tasks need to pick delivery targets; before it, the system had no
// "workspace → conversation" mapping at all.

package feishudao

import (
	"context"
	"errors"
	"github.com/chyroc/ama/server/feishu"
	"testing"
	"time"
)

// TestStoreRememberChatRoundTrip: remember a group and list it by workspace.
func TestStoreRememberChatRoundTrip(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ctx := context.Background()
		seen := time.Date(2026, 8, 27, 9, 30, 0, 0, time.UTC)

		if err := store.RememberChat(ctx, feishu.ChatRecord{
			LarkAppID:   "cli_1",
			ChatID:      "oc_alpha",
			WorkspaceID: testWorkspace,
			ChatType:    "group",
			LastSeenAt:  seen,
		}); err != nil {
			t.Fatalf("记录会话失败: %v", err)
		}

		list, err := store.ListChats(ctx, testWorkspace)
		if err != nil {
			t.Fatalf("列会话失败: %v", err)
		}
		if len(list) != 1 {
			t.Fatalf("应有 1 条，实际 %d", len(list))
		}
		got := list[0]
		if got.ChatID != "oc_alpha" || got.LarkAppID != "cli_1" {
			t.Errorf("键不对: %+v", got)
		}
		if got.ChatType != "group" {
			t.Errorf("chatType 应往返: %q", got.ChatType)
		}
		if !got.LastSeenAt.Equal(seen) {
			t.Errorf("lastSeenAt 应往返，期望 %v 实际 %v", seen, got.LastSeenAt)
		}
	})
}

// TestStoreRememberChatIsIdempotent: remembering the same group again only updates the time; it never becomes several rows.
//
// The inbound path calls it for every message; if it were not idempotent an active group would pile up thousands of records.
func TestStoreRememberChatIsIdempotent(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ctx := context.Background()
		first := time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC)
		second := first.Add(time.Hour)

		for _, at := range []time.Time{first, second} {
			if err := store.RememberChat(ctx, feishu.ChatRecord{
				LarkAppID:   "cli_1",
				ChatID:      "oc_same",
				WorkspaceID: testWorkspace,
				ChatType:    "group",
				LastSeenAt:  at,
			}); err != nil {
				t.Fatalf("记录会话失败: %v", err)
			}
		}

		list, err := store.ListChats(ctx, testWorkspace)
		if err != nil {
			t.Fatalf("列会话失败: %v", err)
		}
		if len(list) != 1 {
			t.Fatalf("同一个群应只有 1 条，实际 %d —— 不幂等的话活跃群会攒出海量记录", len(list))
		}
		if !list[0].LastSeenAt.Equal(second) {
			t.Errorf("lastSeenAt 应被更新成最近一次，期望 %v 实际 %v", second, list[0].LastSeenAt)
		}
	})
}

// TestStoreRememberChatKeepsExistingName: an existing name must not be overwritten by an empty name.
//
// Inbound messages **usually do not carry the group name** (lark.InboundMessage has no such field). If one day a message carries
// the name and every later message without a name wipes it, the symptom is "the group name comes and goes",
// which looks like a random failure in the UI.
func TestStoreRememberChatKeepsExistingName(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ctx := context.Background()
		base := feishu.ChatRecord{
			LarkAppID:   "cli_1",
			ChatID:      "oc_named",
			WorkspaceID: testWorkspace,
			ChatType:    "group",
			LastSeenAt:  time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC),
		}

		named := base
		named.Name = "增长数据日常"
		if err := store.RememberChat(ctx, named); err != nil {
			t.Fatalf("记录会话失败: %v", err)
		}

		// Then one without a name (this is the normal case).
		later := base
		later.LastSeenAt = base.LastSeenAt.Add(time.Hour)
		if err := store.RememberChat(ctx, later); err != nil {
			t.Fatalf("记录会话失败: %v", err)
		}

		list, err := store.ListChats(ctx, testWorkspace)
		if err != nil {
			t.Fatalf("列会话失败: %v", err)
		}
		if len(list) != 1 {
			t.Fatalf("应只有 1 条，实际 %d", len(list))
		}
		if list[0].Name != "增长数据日常" {
			t.Errorf("已有名字不该被空名字覆盖，实际 %q —— "+
				"否则群名会一会儿有一会儿没有", list[0].Name)
		}
		// But the time must be updated.
		if !list[0].LastSeenAt.Equal(later.LastSeenAt) {
			t.Errorf("lastSeenAt 仍应更新，实际 %v", list[0].LastSeenAt)
		}
	})
}

// TestStoreListChatsOrderAndScope: ordered by last seen descending, and only this workspace.
func TestStoreListChatsOrderAndScope(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ctx := context.Background()
		base := time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC)

		// Insert out of order on purpose, mixed with one from another workspace.
		for _, rec := range []feishu.ChatRecord{
			{LarkAppID: "cli_1", ChatID: "oc_mid", WorkspaceID: testWorkspace,
				ChatType: "group", LastSeenAt: base.Add(time.Hour)},
			{LarkAppID: "cli_1", ChatID: "oc_old", WorkspaceID: testWorkspace,
				ChatType: "group", LastSeenAt: base},
			{LarkAppID: "cli_1", ChatID: "oc_new", WorkspaceID: testWorkspace,
				ChatType: "p2p", LastSeenAt: base.Add(2 * time.Hour)},
			{LarkAppID: "cli_2", ChatID: "oc_other", WorkspaceID: "ws_other",
				ChatType: "group", LastSeenAt: base.Add(3 * time.Hour)},
		} {
			if err := store.RememberChat(ctx, rec); err != nil {
				t.Fatalf("记录会话失败: %v", err)
			}
		}

		list, err := store.ListChats(ctx, testWorkspace)
		if err != nil {
			t.Fatalf("列会话失败: %v", err)
		}
		want := []string{"oc_new", "oc_mid", "oc_old"}
		if len(list) != len(want) {
			t.Fatalf("应有 %d 条（别的空间那条不该出现），实际 %d", len(want), len(list))
		}
		for i, id := range want {
			if list[i].ChatID != id {
				t.Errorf("第 %d 条应是 %s，实际 %s —— 最近见过的要排在前面", i, id, list[i].ChatID)
			}
		}
	})
}

// TestStoreListChatsNeverNil: with no groups remembered, return an empty slice instead of nil.
//
// "Nobody has @-ed the bot in a group yet" is a normal state, not an error. Returning nil would encode as null,
// and the frontend's .map() blows up right away.
func TestStoreListChatsNeverNil(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ctx := context.Background()
		list, err := store.ListChats(ctx, "ws_nobody")
		if err != nil {
			t.Fatalf("列空结果不该报错: %v", err)
		}
		if list == nil {
			t.Error("应返回空切片而不是 nil")
		}
		if len(list) != 0 {
			t.Errorf("应为空，实际 %d 条", len(list))
		}
	})
}

// TestStoreRememberChatRejectsEmptyKeys: neither part of the primary key may be empty.
func TestStoreRememberChatRejectsEmptyKeys(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ctx := context.Background()
		for _, rec := range []feishu.ChatRecord{
			{LarkAppID: "", ChatID: "oc_1", WorkspaceID: testWorkspace},
			{LarkAppID: "cli_1", ChatID: "", WorkspaceID: testWorkspace},
			{LarkAppID: "  ", ChatID: "oc_1", WorkspaceID: testWorkspace},
		} {
			if err := store.RememberChat(ctx, rec); !errors.Is(err, feishu.ErrInvalidArgument) {
				t.Errorf("空主键应返回 ErrInvalidArgument，实际 %v（rec=%+v）", err, rec)
			}
		}
	})
}

// TestStoreRememberChatSameChatDifferentApps: the same group seen by two apps gets one record each.
//
// The primary key is (lark_app_id, chat_id) rather than chat_id alone: a group may have two bots,
// and each delivers with its own credentials; they must not mix.
func TestStoreRememberChatSameChatDifferentApps(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ctx := context.Background()
		at := time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC)
		for _, app := range []string{"cli_1", "cli_2"} {
			if err := store.RememberChat(ctx, feishu.ChatRecord{
				LarkAppID:   app,
				ChatID:      "oc_shared",
				WorkspaceID: testWorkspace,
				ChatType:    "group",
				LastSeenAt:  at,
			}); err != nil {
				t.Fatalf("记录会话失败: %v", err)
			}
		}
		list, err := store.ListChats(ctx, testWorkspace)
		if err != nil {
			t.Fatalf("列会话失败: %v", err)
		}
		if len(list) != 2 {
			t.Errorf("同一个群被两个应用看见应有 2 条，实际 %d", len(list))
		}
	})
}

// TestStoreSetChatName only changes the name, not lastSeenAt; an empty name does not wipe an existing name; a missing group is not created.
//
// Filling in group names happens on the "list groups" read path, where the lastSeenAt at hand is an old value just read.
// Writing it back would overwrite the newer time a concurrent inbound message just wrote, so only the name column may change.
func TestStoreSetChatName(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ctx := context.Background()
		seen := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
		if err := store.RememberChat(ctx, feishu.ChatRecord{
			LarkAppID: "cli_1", ChatID: "oc_rename", WorkspaceID: testWorkspace,
			ChatType: "group", LastSeenAt: seen,
		}); err != nil {
			t.Fatalf("记录会话失败: %v", err)
		}

		if err := store.SetChatName(ctx, "cli_1", "oc_rename", "旧群名"); err != nil {
			t.Fatalf("设群名失败: %v", err)
		}
		if err := store.SetChatName(ctx, "cli_1", "oc_rename", "新群名"); err != nil {
			t.Fatalf("改群名失败: %v", err)
		}
		if err := store.SetChatName(ctx, "cli_1", "oc_rename", "  "); err != nil {
			t.Fatalf("空名字该静默忽略: %v", err)
		}
		if err := store.SetChatName(ctx, "cli_1", "oc_missing", "不存在的群"); err != nil {
			t.Fatalf("群不存在不该报错: %v", err)
		}

		list, err := store.ListChats(ctx, testWorkspace)
		if err != nil {
			t.Fatalf("列会话失败: %v", err)
		}
		if len(list) != 1 {
			t.Fatalf("群不存在时不该新建，应只有 1 条，实际 %d", len(list))
		}
		if list[0].Name != "新群名" {
			t.Errorf("群改名后该跟着变、空名字不该抹掉它，实际 %q", list[0].Name)
		}
		if !list[0].LastSeenAt.Equal(seen) {
			t.Errorf("改名字不该动 lastSeenAt，实际 %v", list[0].LastSeenAt)
		}
	})
}

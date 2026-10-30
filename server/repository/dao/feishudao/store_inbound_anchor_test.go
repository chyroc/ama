// Persistence tests for the thread anchors of inbound messages.
//
// # Why these tests are worth writing
//
// These columns **have no business consumer at all**; they are only read by humans when diagnosing after the fact. So "not stored"
// breaks no feature, gives no error, and turns no test red on its own, until a real
// "replying in the thread opened a new session" problem shows up, you check the database, and the values you need are not there,
// while the logs have long rolled over and the user does not want to reproduce it again.
//
// That is exactly the situation of a real investigation: four messages left only scope_type=message, turn_count=1,
// **with no way to tell whether Feishu gave root_id at the time**, and "the platform gave no anchor" vs "we computed the key wrong"
// have completely unrelated fixes. So these assertions are this field's only line of defense.
//
// # Two properties that pull against each other, and must be checked separately
//
// Adding columns easily breaks dedup by accident (for example dropping the on conflict where to "update the anchor"),
// and broken dedup is far worse than a missing anchor: the same message runs the agent twice and the user gets two cards.
// So "the anchor is stored" and "dedup is not broken" are two independent groups of assertions, both run under eachStore.

package feishudao

import (
	"context"
	"errors"
	"github.com/chyroc/ama/server/feishu"
	"github.com/chyroc/ama/server/feishu/feishumodel"
	"testing"
	"time"

	"github.com/chyroc/ama/server/shared/infra/lark"
)

// anchorFixture builds a record whose **four fields all differ**.
//
// The values are deliberately all different: with the same source for all four (say all "om_x"), a wrong implementation that
// stores root_id into parent_id would still pass; you could not assert "this column holds what it should".
func anchorFixture() feishu.InboundMessageRecord {
	return feishu.InboundMessageRecord{
		LarkAppID: "cli_anchor",
		MessageID: "om_self_0001",
		ChatID:    "oc_chat_0002",
		Status:    feishumodel.InboundPending,
		CreatedAt: time.Now(),
		ChatType:  feishu.ChatTypeP2P,
		ThreadID:  "omt_thread_0003",
		RootID:    "om_root_0004",
		ParentID:  "om_parent_0005",
	}
}

// All four anchors must really be stored, each in its own place.
func TestClaimInboundMessagePersistsThreadAnchors(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ctx := context.Background()
		rec := anchorFixture()
		if ok, err := store.ClaimInboundMessage(ctx, rec); err != nil || !ok {
			t.Fatalf("第一次该认领到: ok=%v err=%v", ok, err)
		}

		got, err := store.GetInboundMessage(ctx, rec.MessageID)
		if err != nil {
			t.Fatalf("读回去重记录失败: %v", err)
		}
		// Assert each field separately: combining them into one would make the error unclear about which column is wrong, and swapping
		// (root stored into parent) is exactly the most likely mistake.
		for _, c := range []struct {
			name, got, want string
		}{
			{"chat_type", got.ChatType, rec.ChatType},
			{"thread_id", got.ThreadID, rec.ThreadID},
			{"root_id", got.RootID, rec.RootID},
			{"parent_id", got.ParentID, rec.ParentID},
			{"chat_id", got.ChatID, rec.ChatID},
			{"lark_app_id", got.LarkAppID, rec.LarkAppID},
		} {
			if c.got != c.want {
				t.Errorf("%s 存错了：期望 %q，实际 %q", c.name, c.want, c.got)
			}
		}
	})
}

// When Feishu gives no anchor, store an empty value; **it must not be filled with another field's value**.
//
// "The platform gave nothing" is the most important signal when diagnosing: it points the problem at the platform or SDK parsing, not at
// scopeForMessage. If some column got filled with chat_id or message_id along the way, that signal is gone,
// and the diagnosis would be led in a completely wrong direction.
func TestClaimInboundMessageKeepsMissingAnchorsEmpty(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ctx := context.Background()
		rec := anchorFixture()
		rec.MessageID = "om_no_anchor"
		rec.ThreadID, rec.RootID, rec.ParentID = "", "", ""

		if ok, err := store.ClaimInboundMessage(ctx, rec); err != nil || !ok {
			t.Fatalf("第一次该认领到: ok=%v err=%v", ok, err)
		}
		got, err := store.GetInboundMessage(ctx, rec.MessageID)
		if err != nil {
			t.Fatalf("读回去重记录失败: %v", err)
		}
		for _, c := range []struct{ name, got string }{
			{"thread_id", got.ThreadID},
			{"root_id", got.RootID},
			{"parent_id", got.ParentID},
		} {
			if c.got != "" {
				t.Errorf("飞书没给 %s 时该留空，实际填成了 %q", c.name, c.got)
			}
		}
		// If chat_type was given it must be there; otherwise the three empty values above might just mean "the whole row was not stored".
		if got.ChatType != feishu.ChatTypeP2P {
			t.Errorf("chat_type 该照常存下来，实际 %q", got.ChatType)
		}
	})
}

// **Adding four columns must not break dedup**: redelivering the same message still leaves only one record.
//
// This is the easiest thing to break when adding columns: loosening the on conflict where
// to "update the anchor" removes dedup, and the same message runs the agent twice and the user gets two cards.
func TestClaimInboundMessageStillDedupesWithAnchors(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ctx := context.Background()
		rec := anchorFixture()
		rec.MessageID = "om_dedupe"

		if ok, err := store.ClaimInboundMessage(ctx, rec); err != nil || !ok {
			t.Fatalf("首投该认领到: ok=%v err=%v", ok, err)
		}
		// Feishu redelivers the same message: must be blocked.
		if ok, err := store.ClaimInboundMessage(ctx, rec); err != nil || ok {
			t.Errorf("pending 期间重投该被挡掉: ok=%v err=%v", ok, err)
		}
		// Redelivered after processing: still blocked.
		if err := store.MarkInboundMessage(ctx, rec.MessageID, feishumodel.InboundProcessed); err != nil {
			t.Fatalf("标记 processed 失败: %v", err)
		}
		if ok, err := store.ClaimInboundMessage(ctx, rec); err != nil || ok {
			t.Errorf("processed 之后重投该被挡掉: ok=%v err=%v", ok, err)
		}
		// And the anchor was not changed by the redelivered frames.
		got, err := store.GetInboundMessage(ctx, rec.MessageID)
		if err != nil {
			t.Fatalf("读回去重记录失败: %v", err)
		}
		if got.RootID != rec.RootID {
			t.Errorf("重投不该改掉锚点：期望 %q，实际 %q", rec.RootID, got.RootID)
		}
	})
}

// When failed is let through for retry, **the anchors from the first delivery must be kept**.
//
// Only this branch reaches do update, and the key was computed from the first delivery's input, so only that one counts as evidence when diagnosing.
// Frames redelivered by Feishu are not guaranteed to carry the same fields; letting them overwrite would swap the evidence for something of unknown origin.
func TestClaimInboundMessageRetryKeepsOriginalAnchors(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ctx := context.Background()
		rec := anchorFixture()
		rec.MessageID = "om_retry"

		if ok, err := store.ClaimInboundMessage(ctx, rec); err != nil || !ok {
			t.Fatalf("首投该认领到: ok=%v err=%v", ok, err)
		}
		if err := store.MarkInboundMessage(ctx, rec.MessageID, feishumodel.InboundFailed); err != nil {
			t.Fatalf("标记 failed 失败: %v", err)
		}

		// The anchors in this redelivered frame are incomplete (Feishu redelivery really can look like this).
		stripped := rec
		stripped.ThreadID, stripped.RootID, stripped.ParentID = "", "", ""
		if ok, err := store.ClaimInboundMessage(ctx, stripped); err != nil || !ok {
			t.Fatalf("failed 之后该放行重试: ok=%v err=%v", ok, err)
		}

		got, err := store.GetInboundMessage(ctx, rec.MessageID)
		if err != nil {
			t.Fatalf("读回去重记录失败: %v", err)
		}
		for _, c := range []struct{ name, got, want string }{
			{"thread_id", got.ThreadID, rec.ThreadID},
			{"root_id", got.RootID, rec.RootID},
			{"parent_id", got.ParentID, rec.ParentID},
		} {
			if c.got != c.want {
				t.Errorf("重试不该丢掉首投的 %s：期望 %q，实际 %q", c.name, c.want, c.got)
			}
		}
	})
}

func TestGetInboundMessageReportsMissing(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		_, err := store.GetInboundMessage(context.Background(), "om_never_seen")
		if !errors.Is(err, feishu.ErrInboundMessageNotFound) {
			t.Errorf("期望 %v，实际 %v", feishu.ErrInboundMessageNotFound, err)
		}
	})
}

// Through the full Inbound path: the anchors of a **real inbound message** are stored.
//
// The tests above check that the Store stores correctly; this one checks that **the call site really passes the values down**.
// Passing one field less does not fail compilation, does not change behavior, and the Store unit tests stay all green. This repo was bitten once by
// "brand-new code all green but with no production caller" (see that entry in the README); this test guards against it.
func TestInboundPersistsThreadAnchorsFromRealMessage(t *testing.T) {
	conv := &fakeConversations{
		agentIDs: []string{"agent_1"},
		frames:   [][]byte{agentFrame("好的。")},
	}
	in, store := inboundFixture(t, conv, &fakeSender{})

	// A follow-up message inside a DM thread: exactly the shape from the user's retest.
	msg := &lark.InboundMessage{
		AppID:     "cli_1",
		MessageID: "om_followup_9001",
		ChatID:    "oc_p2p_9002",
		ChatType:  feishu.ChatTypeP2P,
		ThreadID:  "omt_thread_9003",
		RootID:    "om_root_9004",
		ParentID:  "om_parent_9005",
		Text:      "那明天呢",
		Mentioned: true,
	}
	if err := in.Handle(context.Background(), msg); err != nil {
		t.Fatalf("处理入站消息失败: %v", err)
	}

	got, err := store.GetInboundMessage(context.Background(), msg.MessageID)
	if err != nil {
		t.Fatalf("读回去重记录失败: %v", err)
	}
	for _, c := range []struct{ name, got, want string }{
		{"chat_type", got.ChatType, msg.ChatType},
		{"thread_id", got.ThreadID, msg.ThreadID},
		{"root_id", got.RootID, msg.RootID},
		{"parent_id", got.ParentID, msg.ParentID},
	} {
		if c.got != c.want {
			t.Errorf("入站链路没把 %s 传下去：期望 %q，实际 %q", c.name, c.want, c.got)
		}
	}
}

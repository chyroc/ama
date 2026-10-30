// Tests for final answer delivery. All offline.
//
// They pin down three things:
//
//   - Table splitting: split when over the limit; **pipes inside code blocks are not tables** (otherwise bash output gets chopped up)
//   - Ordering gate has a cap: send the answer even if the CoT never gets closed
//   - Stable idempotency key: the value does not change on retry, so Feishu can dedupe

package feishu

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestSplitReplyKeepsShortAnswerWhole(t *testing.T) {
	answer := "就一句话，没有表格。"
	got := splitReplyByTables(answer, maxCardTables)
	if len(got) != 1 || got[0] != answer {
		t.Errorf("没超限不该切，得到 %d 段: %v", len(got), got)
	}

	// Exactly at the limit does not split either.
	var b strings.Builder
	for i := 0; i < maxCardTables; i++ {
		b.WriteString("| a | b |\n|---|---|\n| 1 | 2 |\n\n文字\n\n")
	}
	if got := splitReplyByTables(b.String(), maxCardTables); len(got) != 1 {
		t.Errorf("刚好 %d 个表格不该切，得到 %d 段", maxCardTables, len(got))
	}
}

func TestSplitReplySplitsWhenTooManyTables(t *testing.T) {
	var b strings.Builder
	for i := 0; i < maxCardTables*2+1; i++ {
		b.WriteString("| a | b |\n|---|---|\n| 1 | 2 |\n\n")
	}
	segments := splitReplyByTables(b.String(), maxCardTables)
	if len(segments) < 2 {
		t.Fatalf("超限该切开，得到 %d 段", len(segments))
	}
	for i, seg := range segments {
		if n := countMarkdownTables(seg); n > maxCardTables {
			t.Errorf("第 %d 段有 %d 个表格，仍然超限", i, n)
		}
	}
	// Content must not be lost: all segments together must cover the table count of the original.
	total := 0
	for _, seg := range segments {
		total += countMarkdownTables(seg)
	}
	if total != maxCardTables*2+1 {
		t.Errorf("切分丢了表格：原 %d 个，切后 %d 个", maxCardTables*2+1, total)
	}
}

// **Pipes inside code blocks are not tables.** Without this check, bash output that pastes an ASCII table
// would be taken as a pile of tables, and the answer would be cut into a string of baffling fragments.
func TestCountTablesIgnoresCodeFences(t *testing.T) {
	withFence := "这是输出：\n\n```\n| 不是 | 表格 |\n| 只是 | 输出 |\n| a | b |\n```\n\n完毕。"
	if n := countMarkdownTables(withFence); n != 0 {
		t.Errorf("代码块里的竖线不该算表格，数出了 %d 个", n)
	}
	if got := splitReplyByTables(withFence, maxCardTables); len(got) != 1 {
		t.Errorf("不该被切开，得到 %d 段", len(got))
	}

	// Real tables must still be counted.
	real := "| a | b |\n|---|---|\n| 1 | 2 |"
	if n := countMarkdownTables(real); n != 1 {
		t.Errorf("真表格该数出 1 个，得到 %d", n)
	}

	// Mixed: tables inside code blocks do not count, ones outside do.
	mixed := real + "\n\n```\n| x | y |\n```\n\n" + real
	if n := countMarkdownTables(mixed); n != 2 {
		t.Errorf("该只数外面那两个表格，得到 %d", n)
	}
}

// The idempotency key must be stable: the value does not change on retry, so Feishu can tell it is the same message.
func TestSegmentIdempotencyKeyIsStable(t *testing.T) {
	a := segmentIdempotencyKey("sesn-1", 0, 3)
	if a != segmentIdempotencyKey("sesn-1", 0, 3) {
		t.Error("同样输入该得到同样的键")
	}
	// Different segments must not collide.
	if a == segmentIdempotencyKey("sesn-1", 1, 3) {
		t.Error("不同段的键不能相同，否则第二段会被当重复丢掉")
	}
	// Different runs must not collide.
	if a == segmentIdempotencyKey("sesn-2", 0, 3) {
		t.Error("不同 run 的键不能相同")
	}
	// If the split result changes, the key must change too, or the new segment would be treated as a duplicate of the old one.
	if a == segmentIdempotencyKey("sesn-1", 0, 5) {
		t.Error("总段数变了键该变")
	}
}

func TestSendFinalReplySendsSegmentsInOrder(t *testing.T) {
	sender := &fakeSender{}
	var b strings.Builder
	for i := 0; i < maxCardTables*2+1; i++ {
		b.WriteString("| a | b |\n|---|---|\n| 1 | 2 |\n\n")
	}

	err := sendFinalReply(context.Background(), sender, "t", "om_src", "sesn-1",
		b.String(), true, slog.Default())
	if err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	cards := sender.sent()
	if len(cards) < 2 {
		t.Fatalf("该切成多条发，实际 %d 条", len(cards))
	}
	// All of them are replies (separate messages), not updates: the answer must not overwrite the chain-of-thought card.
	for i, c := range cards {
		if c.kind != "reply" {
			t.Errorf("第 %d 条该是独立回复，得到 %q", i, c.kind)
		}
	}
}

func TestSendFinalReplyHandlesEmptyAnswer(t *testing.T) {
	sender := &fakeSender{}
	if err := sendFinalReply(context.Background(), sender, "t", "om_src", "sesn-1",
		"   ", false, slog.Default()); err != nil {
		t.Fatalf("空答案不该报错: %v", err)
	}
	cards := sender.sent()
	if len(cards) != 1 {
		t.Fatalf("空答案也该回一条，实际 %d 条", len(cards))
	}
	if strings.TrimSpace(cards[0].text) == "" {
		t.Error("兜底文案不能是空的——用户会以为机器人坏了")
	}
}

func TestSendFinalReplySurfacesFailure(t *testing.T) {
	sender := &fakeSender{replyErr: errors.New("网络炸了")}
	err := sendFinalReply(context.Background(), sender, "t", "om_src", "sesn-1",
		"答案", false, slog.Default())
	if err == nil {
		t.Fatal("发送失败该报错")
	}
}

// ---------------------------------------------------------------------------
// Ordering gate
// ---------------------------------------------------------------------------

func TestWaitForProgressTerminalReturnsWhenReady(t *testing.T) {
	store := NewMemProgressStore()
	ctx := context.Background()
	if _, err := store.EnsureProgressDelivery(ctx, &ProgressDelivery{
		RunID: "sesn-1", LarkAppID: "cli_1", ChatID: "oc_1", Transport: TransportCOT,
	}); err != nil {
		t.Fatalf("建投递失败: %v", err)
	}
	// Push it to the final state first.
	d, _, _ := store.ClaimProgressDelivery(ctx, "sesn-1", "tok", time.Hour)
	d.Status = ProgressCompleted
	d.TerminalApplied = true
	d.RemoteMessageID = "om_1"
	_ = store.SaveProgressDelivery(ctx, d, "tok")

	start := time.Now()
	if !waitForProgressTerminal(ctx, store, "sesn-1", time.Now, slog.Default()) {
		t.Error("已经封口了该立刻返回 true")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("不该等这么久: %v", elapsed)
	}
}

// With no progress record it should not wait in vain: the progress feature may not be enabled at all.
func TestWaitForProgressTerminalSkipsWhenNoRecord(t *testing.T) {
	store := NewMemProgressStore()
	start := time.Now()
	if !waitForProgressTerminal(context.Background(), store, "nope", time.Now, slog.Default()) {
		t.Error("没有记录该立刻放行")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("不该等: %v", elapsed)
	}
}

// **The ordering gate must have a cap.** Send the answer even if the CoT can never be closed,
// or the user never gets the result.
func TestWaitForProgressTerminalGivesUpAfterMax(t *testing.T) {
	store := NewMemProgressStore()
	ctx := context.Background()
	if _, err := store.EnsureProgressDelivery(ctx, &ProgressDelivery{
		RunID: "sesn-stuck", LarkAppID: "cli_1", ChatID: "oc_1", Transport: TransportCOT,
	}); err != nil {
		t.Fatalf("建投递失败: %v", err)
	}
	// Stuck in active, never closed.
	d, _, _ := store.ClaimProgressDelivery(ctx, "sesn-stuck", "tok", time.Hour)
	d.Status = ProgressActive
	d.RemoteCOTID, d.RemoteMessageID = "cot_1", "om_1"
	_ = store.SaveProgressDelivery(ctx, d, "tok")

	// Use a fake clock to set up "already waited a long time" directly, instead of really sleeping 10 seconds.
	base := time.Now()
	calls := 0
	fakeNow := func() time.Time {
		calls++
		if calls == 1 {
			return base
		}
		return base.Add(finalReplyWaitMax + time.Second)
	}

	if waitForProgressTerminal(ctx, store, "sesn-stuck", fakeNow, slog.Default()) {
		t.Error("一直不封口该超时放行（返回 false），而不是无限等")
	}
}

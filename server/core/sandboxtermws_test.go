package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/repository/dao/coremem"
	"github.com/gorilla/websocket"
)

// **Auth** cases for the terminal WS path.
//
// ⚠️ **This group adds back a gate that is bypassed; it is not optional coverage.**
//
// Every HTTP route in this repo must declare its permission in authz/matrix.go, or startup
// panics. But the terminal WS route is **deliberately not in the table**: it connects to a
// different domain, that connection has no Cookie (ama_session is host-only), and it cannot carry
// custom request headers (a spec limit of browser WS), so the auth middleware does not work here
// and auth is done instead by "redeeming a one-time ticket".
//
// The cost is that the machine gate no longer covers it. So this group takes over: **one test
// for each of the four ways to be denied**, and breaking any of them turns it red.
//
// The check is always the **HTTP status code**, because during auth this path has not upgraded the
// protocol yet. That is exactly why StreamRunSandboxTerminalWS does "auth first, upgrade after":
// after the upgrade you can no longer return a status code, rejection can only close the
// connection, and the browser cannot tell why.

// newTicketService builds a Service with an in-memory store and returns it.
//
// No TerminalRelay is configured: the WS path **does not need a cross-instance relay** (upstream
// and downstream are on the same connection, in the same process), which is exactly where it is
// simpler than the SSE+POST setup.
func newTicketService(t *testing.T) *Service {
	t.Helper()
	store, err := coremem.NewSeededMemStore()
	if err != nil {
		t.Fatalf("装载种子失败：%v", err)
	}
	svc, err := New(Config{Store: store})
	if err != nil {
		t.Fatalf("装配 Service 失败：%v", err)
	}
	return svc
}

// wsRequest builds a terminal WS handshake request.
//
// All four headers are required (without them gorilla rejects it as "not a valid handshake",
// which would get mixed up with auth failures). But **most cases in this group are rejected before
// the upgrade**, so these headers only exist to make the premise "if auth passes, it should be able
// to upgrade" hold.
func wsRequest(runID, ticket string) *http.Request {
	url := "/api/runs/" + runID + "/sandbox/terminal/ws"
	if ticket != "" {
		url += "?ticket=" + ticket
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.SetPathValue("runId", runID)
	return req
}

// issueFor puts a ticket straight into the store, bypassing IssueTerminalTicket's container check.
//
// Why bypass it: issuing requires the container to be running, and the in-memory store has no
// real container. That check is pinned separately by
// TestIssueTerminalTicketRequiresRunningSandbox; what we test here is the **redeem** side.
func issueFor(t *testing.T, svc *Service, ticket, runID, actor string, expires time.Time) {
	t.Helper()
	err := svc.store.IssueTerminalTicket(context.Background(), persistmodel.TerminalTicketRow{
		Ticket:    ticket,
		RunID:     runID,
		Actor:     actor,
		ExpiresAt: expires,
		CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("塞入测试票失败：%v", err)
	}
}

// TestTerminalWSRejectsBadTickets pins one test for each of the four ways to be denied.
//
// **The check is "must not be 101"**, and the status code must point to the right cause. Any of
// them becoming 101 means a privilege-escalation hole.
func TestTerminalWSRejectsBadTickets(t *testing.T) {
	const (
		myRun    = "run_1"
		otherRun = "run_2"
		actor    = "zhangsan@example.com"
	)

	cases := []struct {
		name   string
		setup  func(t *testing.T, svc *Service) (runID, ticket string)
		want   int
		reason string
	}{
		{
			name: "完全没带票",
			setup: func(_ *testing.T, _ *Service) (string, string) {
				return myRun, ""
			},
			want:   http.StatusUnauthorized,
			reason: "没有凭据就该 401。放过去等于终端对全网开放。",
		},
		{
			name: "票是编的",
			setup: func(_ *testing.T, _ *Service) (string, string) {
				return myRun, "deadbeef-not-a-real-ticket"
			},
			want:   http.StatusUnauthorized,
			reason: "伪造的票必须挡住——这条是猜测攻击的第一道门。",
		},
		{
			name: "票已过期",
			setup: func(t *testing.T, svc *Service) (string, string) {
				const tk = "expired-ticket"
				// The expiry time is in the **past**.
				issueFor(t, svc, tk, myRun, actor, time.Now().Add(-time.Second))
				return myRun, tk
			},
			want: http.StatusUnauthorized,
			reason: "过期票必须挡住。⚠️ 过期判定要在 SQL/持锁段里做，" +
				"在 Go 里比时间会用本实例的时钟，而多实例的时钟是偏的。",
		},
		{
			name: "票是别的运行的（越权）",
			setup: func(t *testing.T, svc *Service) (string, string) {
				const tk = "ticket-for-another-run"
				// The ticket itself is fully valid; it is just bound to otherRun.
				issueFor(t, svc, tk, otherRun, actor, time.Now().Add(time.Minute))
				// But it is used to open myRun's terminal.
				return myRun, tk
			},
			want: http.StatusForbidden,
			reason: "⚠️ 这条是最容易漏的：票合法、没过期、没用过，只有 runID 对不上。" +
				"少了那个比对，任何一张票都能开任意运行的终端——" +
				"「一次性」做对了而授权范围丢了。",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTicketService(t)
			runID, ticket := tc.setup(t, svc)

			rec := httptest.NewRecorder()
			svc.StreamRunSandboxTerminalWS(rec, wsRequest(runID, ticket), runID)

			if rec.Code == http.StatusSwitchingProtocols {
				t.Fatalf("握手竟然成功了（101）—— 这是一个越权口子。%s", tc.reason)
			}
			if rec.Code != tc.want {
				t.Errorf("状态码 = %d，期望 %d。%s", rec.Code, tc.want, tc.reason)
			}
		})
	}
}

// TestTerminalTicketIsSingleUse pins down "a ticket can be redeemed only once".
//
// ⚠️ **This is the core property of the mechanism**. The ticket appears in the URL (browser WS
// cannot carry request headers, so the query is the only place left), so it may end up in browser
// history, gateway access logs, or Referer. "Single use" is the only way to shrink that leak
// surface to an acceptable level: picking up a used ticket lets you do nothing.
func TestTerminalTicketIsSingleUse(t *testing.T) {
	svc := newTicketService(t)
	const tk = "single-use-ticket"
	issueFor(t, svc, tk, "run_1", "zhangsan@example.com", time.Now().Add(time.Minute))

	// First redeem: succeeds, and the content matches.
	row, err := svc.RedeemTerminalTicket(context.Background(), tk)
	if err != nil {
		t.Fatalf("第一次核销就失败了：%v", err)
	}
	if row.RunID != "run_1" || row.Actor != "zhangsan@example.com" {
		t.Errorf("核销回来的绑定不对：run=%q actor=%q", row.RunID, row.Actor)
	}

	// Second: must fail.
	if _, err := svc.RedeemTerminalTicket(context.Background(), tk); err == nil {
		t.Fatal("同一张票被核销了两次 —— 「一次性」这个性质没了，" +
			"而它是这套机制存在的全部理由（票会进 URL、日志、历史记录）")
	}
}

// TestTerminalTicketRedeemIsAtomic: concurrent attempts on the same ticket, **exactly one**
// succeeds.
//
// # Why this test must exist
//
// "Single use" is guaranteed on the PG side by a single `update ... where used_at is null`, and on
// the in-memory side by holding the lock for the whole section. Both could be changed into "read,
// check, then write". That shape is perfectly fine in **single-request** tests (the
// TestTerminalTicketIsSingleUse above stays green); only concurrency exposes it: two goroutines
// each read "not used yet" and then each write successfully.
//
// So this test and the one above are **not redundant**: they pin the same property at different
// levels of concurrency, and the one missing is exactly what happens in production and what tests
// most easily let through.
//
// # ⚠️ The parameters were tuned so that "the mutation gets caught", not picked at random
//
// The first version used 16 goroutines and a single starting gun, and **during mutation testing
// 20 rounds without -race never caught it once** (with -race it was caught every time, because
// instrumentation widens the race window). A security assertion that only works under -race is
// not enough: `go test` without -race is also a common way to run.
//
// The second version added `runtime.Gosched()` spinning + 40 rounds, and the catch rate was only
// 2/10, still not enough. The root cause is that after `Gosched` yields, the goroutines still come
// back at scattered times.
//
// The final shape is a **real barrier**:
//
//   - `sync.WaitGroup` makes all 64 goroutines get in place first (ready.Done + ready.Wait),
//     then a channel releases them at once, so they "rush together" instead of "one after another";
//   - concurrency on the order of `runtime.GOMAXPROCS(0)` + 60 rounds turns the per-round
//     probability into a certainty;
//   - the assertion is checked in **every round**; a second winner in any round fails right away.
//
// Measured (with the mem store changed into a TOCTOU version that "reads, unlocks, then writes"):
// 10 runs in a row without -race, all 10 red. With -race also all red.
//
// Do not shrink rounds/workers to "run faster". After shrinking, it goes back to being a
// decoration that only works under -race, and this assertion guards a privilege-escalation hole.
func TestTerminalTicketRedeemIsAtomic(t *testing.T) {
	const (
		rounds  = 60
		workers = 64
	)
	svc := newTicketService(t)

	for round := 0; round < rounds; round++ {
		ticket := "race-ticket-" + strconv.Itoa(round)
		issueFor(t, svc, ticket, "run_1", "zhangsan@example.com", time.Now().Add(time.Minute))

		var (
			wg       sync.WaitGroup
			ready    sync.WaitGroup
			mu       sync.Mutex
			wins     int
			firstErr error
		)
		// The go statement itself has overhead, so first get all goroutines in place (ready), then
		// release them together (start). Without this barrier, the early goroutines would finish before the
		// later ones even start, so there would be no concurrency at all, which is exactly why the first
		// two versions could not catch the mutation.
		start := make(chan struct{})
		ready.Add(workers)
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ready.Done()
				<-start
				_, err := svc.RedeemTerminalTicket(context.Background(), ticket)
				mu.Lock()
				defer mu.Unlock()
				if err == nil {
					wins++
				} else if firstErr == nil {
					firstErr = err
				}
			}()
		}
		ready.Wait() // all in place
		close(start) // go together
		wg.Wait()

		if wins != 1 {
			t.Fatalf("第 %d 轮：%d 个并发核销里有 %d 个成功，期望恰好 1 个。\n"+
				"大于 1 = 核销不是原子的（很可能被改成了「先读再写」），"+
				"于是同一张票能开出多条终端；\n"+
				"等于 0 = 连合法的第一次都失败了，票机制整体不可用。\n"+
				"其中一个失败原因：%v", round, workers, wins, firstErr)
		}
	}
}

// TestIssueTerminalTicketNeedsIdentity pins down "no ticket can be issued without an identity".
//
// When IssueTerminalTicket is reached, context always has an identity (that route is wrapped by
// Protect), so this guards against **wiring mistakes**: if someone ever moves the issue endpoint
// onto a bare mux, this turns red.
//
// ⚠️ Issuing an anonymous ticket is much worse than "the endpoint returns 500": that ticket can
// open a terminal, and the terminal has personal credentials injected by the workspace owner. So
// this must be a hard failure and must not degrade.
func TestIssueTerminalTicketNeedsIdentity(t *testing.T) {
	svc := newTicketService(t)

	// **Deliberately do not put an identity into context.**
	_, err := svc.IssueTerminalTicket(context.Background(), "run_1")
	if err == nil {
		t.Fatal("没有身份也签出了票 —— 那是一张匿名的、能开终端的凭据")
	}
	if !strings.Contains(err.Error(), "缺少身份") {
		t.Errorf("错误信息没指向真实原因：%v", err)
	}
}

// TestTerminalTicketCarriesIssuerIdentity pins down "the identity got back on redeem is the one
// from issue time".
//
// This connection has no Cookie, so the actor in the ticket is the **only** source of identity. If
// it is lost or mixed up, downstream (audit logs, credential resolution in the container) gets the
// wrong person, and that does not report an error.
func TestTerminalTicketCarriesIssuerIdentity(t *testing.T) {
	svc := newTicketService(t)
	const want = "lisi@example.com"
	const tk = "identity-ticket"
	issueFor(t, svc, tk, "run_1", want, time.Now().Add(time.Minute))

	row, err := svc.RedeemTerminalTicket(context.Background(), tk)
	if err != nil {
		t.Fatalf("核销失败：%v", err)
	}
	if row.Actor != want {
		t.Fatalf("核销回来的身份 = %q，期望 %q —— 这是那条连接唯一的身份来源", row.Actor, want)
	}

	// That identity must be readable by auth.ActorFrom (everything downstream uses it).
	//
	// ⚠️ This step cannot be skipped: putting it under some other context key would also pass the
	// assertion above, but downstream would read an empty string. The symptom is "the terminal works,
	// but the audit log has no operator and the container gets no credentials", and neither reports an
	// error.
	ctx := svc.withTicketActor(context.Background(), row.Actor)
	if got := auth.ActorFrom(ctx); got != want {
		t.Fatalf("auth.ActorFrom 读到 %q，期望 %q —— "+
			"身份没按标准形状放进 context，下游（审计、凭据解析）会读到空串", got, want)
	}
}

// TestPurgeExpiredTerminalTickets pins down that purge really deletes expired tickets and leaves
// unexpired ones alone.
//
// ⚠️ "A cleanup function was written but nobody calls it" has already happened once in this repo
// (the pgmq table), and that kind of gap has zero errors. This test at least makes sure the
// function itself is correct.
func TestPurgeExpiredTerminalTickets(t *testing.T) {
	svc := newTicketService(t)
	now := time.Now()
	// One ticket long expired (beyond the retention margin), one not expired yet.
	issueFor(t, svc, "old", "run_1", "a@example.com", now.Add(-2*terminalTicketPurgeAge))
	issueFor(t, svc, "fresh", "run_1", "a@example.com", now.Add(time.Minute))

	n, err := svc.PurgeExpiredTerminalTickets(context.Background())
	if err != nil {
		t.Fatalf("清理失败：%v", err)
	}
	if n != 1 {
		t.Errorf("删了 %d 条，期望 1 条（只该删那张远期过期的）", n)
	}

	// The unexpired one must still be redeemable: **this is the counter-example, to prevent purge
	// being written as "delete everything"**.
	if _, err := svc.RedeemTerminalTicket(context.Background(), "fresh"); err != nil {
		t.Errorf("没过期的票被清理掉了：%v —— 清理的 where 条件写宽了", err)
	}
}

// TestPurgeIsActuallyWired pins down "the purge function is really called by someone".
//
// # ⚠️ This guards against a pure wiring gap, which has zero errors
//
// Found during self-review: the comment on PurgeExpiredTerminalTickets said "called by a scheduled
// job", but at the time there was **no call site at all**. The function was written and its unit
// test passed (calling the Service method directly), but in production it would never run, and
// that high-traffic table would only grow.
//
// This repo has already hit the same shape (cleanup of pgmq's mq_messages).
// "The function is written but nobody calls it" is green at compile time, in unit tests, and in e2e.
//
// The check is to **go through the real call chain** (AgentSyncer.purge), not call the Service
// method directly: the latter cannot prove the wiring exists, which is exactly what this case
// guards against.
//
// # ⚠️ The assertion must look at "how many are left", not "redeem fails"
//
// The first version asserted "redeeming the expired ticket should fail", and **under mutation
// testing it was always true**: deleting the whole wiring in purge kept the case green. The reason
// is that RedeemTerminalTicket also returns ErrNotFound for **expired** tickets (that is
// deliberate; see its comment), so "redeem fails" also holds when "the ticket is still in the DB,
// just expired", and the assertion proved nothing.
//
// Now it calls Purge again and looks at the returned count: wiring present -> the first run
// already deleted it -> the second returns 0; wiring missing -> the ticket is still there -> the
// second returns 1. This signal only shows up when the wiring exists.
func TestPurgeIsActuallyWired(t *testing.T) {
	svc := newTicketService(t)
	// A ticket expired long ago (beyond the retention margin); purge should delete it.
	issueFor(t, svc, "stale", "run_1", "a@example.com",
		time.Now().Add(-2*terminalTicketPurgeAge))

	syncer := NewAgentSyncer(AgentSyncerConfig{Service: svc})
	if syncer == nil {
		t.Fatal("构造 AgentSyncer 失败")
	}
	// ⚠️ Call purge, not svc.PurgeExpiredTerminalTickets: this step is what verifies the wiring.
	syncer.purge(context.Background())

	// Purge again: if the ticket was already deleted above, this time it should be 0 rows.
	left, err := svc.PurgeExpiredTerminalTickets(context.Background())
	if err != nil {
		t.Fatalf("二次清理失败：%v", err)
	}
	if left != 0 {
		t.Fatalf("二次清理还删掉了 %d 条 —— 说明 AgentSyncer.purge 根本没清终端入场票。"+
			"「写了清理函数但没人调它」是零报错的，只有这条用例挡得住。", left)
	}
}

// TestClientWriterSerializesWrites pins down "writes to the browser connection are serialized".
//
// # ⚠️ This is a real bug caught in code review, not a defensive test
//
// In relayTerminalWS there are **three sources** that write to the browser connection:
//
//	① the container->browser goroutine forwarding PTY output
//	② the main loop sending a ping frame every 20 seconds (to beat the gateway's 30-second idle
//	   disconnect)
//	③ an error frame sent when either side fails
//
// ① is in a goroutine, ② and ③ are in the main loop, and **they really do run concurrently**.
// gorilla's Conn explicitly does not support concurrent writes; the result is interleaved frames
// (bytes of two frames mixed together) -> the browser cannot decode them and disconnects.
//
// My first version's comment said "ping and writes both happen in this goroutine, so no write
// lock is needed". That is right if you only look at the main loop, but it missed ①. **And this
// bug cannot be caught locally or in e2e**: it is a race and does not show up at all under low
// load.
//
// So this case locks it down with concurrent writes under `-race`: N goroutines write into
// clientWriter at the same time, and if the race detector reports DATA RACE, serialization is gone.
//
// ⚠️ **The check depends on `go test -race`** (this repo's standard way to run includes it).
// Without -race, interleaved frames do not necessarily show up as failures, so this case is weak
// evidence when run that way. That is inherent to how Go race detection works, not a broken test.
func TestClientWriterSerializesWrites(t *testing.T) {
	// Start a WS server that only reads and never replies: we only care whether the write side races.
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("连测试用 WebSocket 失败：%v", err)
	}
	defer func() { _ = conn.Close() }()

	cw := &clientWriter{conn: conn}

	// The three sources each write concurrently, mimicking the real shape in relay.
	const writers = 8
	var wg, ready sync.WaitGroup
	start := make(chan struct{})
	ready.Add(writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ready.Done()
			<-start
			for j := 0; j < 20; j++ {
				if i%3 == 0 {
					_ = cw.write(websocket.PingMessage, nil)
				} else {
					_ = cw.write(websocket.TextMessage, []byte("frame-"+strconv.Itoa(i)))
				}
			}
		}(i)
	}
	ready.Wait()
	close(start)
	wg.Wait()

	// Reaching here without -race reporting anything means writes are serialized.
	//
	// ⚠️ After removing clientWriter's lock (calling conn.WriteMessage directly),
	// `go test -race` reports DATA RACE or gorilla itself panics
	// （"concurrent write to websocket connection"）。
}

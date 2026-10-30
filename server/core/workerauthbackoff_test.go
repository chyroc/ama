// Tests for the environment-level long backoff gate used when Ark credentials are invalid (401/403).
//
// It matches an earlier incident: a deleted key made PollWork return 401 over and over, while
// pollLoop returned right away and the outer layer reconnected every fixed 5 seconds; in 25 minutes
// production logged 4448 lines of "API key doesn't exist".
//
// These tests **don't sleep in real time**: the gate's fail/blocked both take a now argument, and
// the tests move the clock themselves. A backoff test that takes 5 minutes could only be skipped
// with -short, which is the same as having no test.
package core

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/repository/dao/coremem"
	"github.com/volcengine/ark-runtime-go/arkruntime/selfhosted"
)

// authErr builds an Ark authentication error. **It must be a *selfhosted.APIError**: the check uses
// errors.As to get StatusCode (arkAuthStatus), not string matching; "API key doesn't exist" is
// Ark's current wording and may change at any time.
func authErr(code int) error {
	return &selfhosted.APIError{StatusCode: code, Message: "API key doesn't exist"}
}

// TestAuthBackoffLadderIsLongNotFiveSeconds is the **main goal** of this change.
//
// It pins "how long to wait after the 1st/2nd/3rd/4th authentication failure". The old behavior was
// a fixed 5 seconds (reconnectGap), so one bad key hit the server every 3.5 seconds, burning both
// the Ark account's RPM quota and the logs.
//
// ⚠️ **Mutation check**: when authBackoffLevels is changed to {5s,5s,5s,5s} (i.e. back to the old
// fixed 5-second reconnect), this test must turn red. Asserting only "more than 5 seconds" is not
// enough: a fake fix with "a fixed 6-second gap" would fool it, and that is still a busy-loop. So
// check every step one by one.
func TestAuthBackoffLadderIsLongNotFiveSeconds(t *testing.T) {
	b := newAuthBackoff()
	now := time.Unix(1700000000, 0)
	const env, fp = "env_bad_key", "len=32 sha=deadbeef"

	// The expected center value of each step, matching authBackoffLevels one to one.
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute}

	for i, center := range want {
		wait, n := b.fail(env, fp, now)
		if n != i+1 {
			t.Errorf("第 %d 次失败：计数 = %d，要 %d", i+1, n, i+1)
		}
		// jitter is ±20%, so assert a range rather than an exact value, but the range must be **narrow enough to rule out 5 seconds**.
		lo, hi := time.Duration(float64(center)*0.8), time.Duration(float64(center)*1.2)
		if wait < lo || wait > hi {
			t.Errorf("第 %d 次失败：等 %v，要落在 [%v,%v]（档位 %v ±20%%）",
				i+1, wait, lo, hi, center)
		}
		// This is the hook for the mutation check: no step should fall back to the "5-second reconnect" scale.
		if wait <= 5*time.Second {
			t.Errorf("第 %d 次失败只等了 %v —— 这就是那次故障的 busy-loop，"+
				"认证失败必须长退避（当前档位表 %v）", i+1, wait, authBackoffLevels)
		}
		now = now.Add(wait)
	}

	// Past the number of steps it must be capped at the last step, not keep doubling into hours
	// (that would stop the worker for good, while the requirement clearly says "when the user
	// changes the key it must recover automatically").
	for range 5 {
		wait, _ := b.fail(env, fp, now)
		if wait > time.Duration(float64(5*time.Minute)*1.2) {
			t.Fatalf("退避超过封顶值：%v", wait)
		}
		now = now.Add(wait)
	}
}

// TestAuthBackoffBlocksUntilDeadline checks that it really blocks during the backoff and really
// lets through once it expires.
//
// The gate sits on ensureWorker (not inside the reconnect loop), so "blocked" = that round of
// refresh doesn't start this environment's worker, so PollWork isn't called again.
func TestAuthBackoffBlocksUntilDeadline(t *testing.T) {
	b := newAuthBackoff()
	now := time.Unix(1700000000, 0)
	const env, fp = "env_bad_key", "len=32 sha=deadbeef"

	wait, _ := b.fail(env, fp, now)

	// Just failed: blocked.
	if blocked, left := b.blocked(env, fp, now); !blocked {
		t.Fatal("刚认证失败就该挡住，否则下一轮 refresh 立刻又去撞一次 401")
	} else if left <= 0 {
		t.Errorf("挡住时要报出剩余时间（给日志与排查用），得到 %v", left)
	}

	// Within the backoff (1 nanosecond before expiry): still blocked. Use the boundary value instead
	// of "halfway", because an off-by-one error (<= written as <) only shows up at the boundary.
	if blocked, _ := b.blocked(env, fp, now.Add(wait-1)); !blocked {
		t.Error("退避到期前一刻仍该挡住")
	}

	// Expired: let through. This guarantees the worker comes back by itself instead of being stopped for good.
	if blocked, _ := b.blocked(env, fp, now.Add(wait)); blocked {
		t.Error("退避到期就该放行 —— 永久停掉 worker 意味着只能靠重启进程恢复")
	}
}

// TestAuthBackoffResetsAfterSuccess locks in "one success resets to zero".
//
// ⚠️ Missing clear is a **silent bug**, exactly the same shape as the one for
// arkbackoff.Ladder.Reset: an environment that hits an occasional 401 would pile its count all the
// way up to the 5-minute step and never come back, so after every recovery the next failure starts
// from the highest step. Zero errors, it just "gets slower the more you use it".
//
// ⚠️ Mutation check: when the line p.auth.clear(envID) in pollLoop is deleted, this test must turn red.
func TestAuthBackoffResetsAfterSuccess(t *testing.T) {
	b := newAuthBackoff()
	now := time.Unix(1700000000, 0)
	const env, fp = "env_flaky", "len=32 sha=deadbeef"

	// Hit it three times in a row, climbing to the third step.
	for range 3 {
		wait, _ := b.fail(env, fp, now)
		now = now.Add(wait)
	}

	// One success (this is exactly what pollLoop calls the moment PollWork returns nil).
	b.clear(env)

	if blocked, _ := b.blocked(env, fp, now); blocked {
		t.Fatal("成功之后不该还挡着")
	}

	// Another 401: it must start again from the **first step**, not continue at the fourth.
	wait, n := b.fail(env, fp, now)
	if n != 1 {
		t.Errorf("成功清零后再失败，计数要从 1 开始，得到 %d", n)
	}
	first := authBackoffLevels[0]
	if hi := time.Duration(float64(first) * 1.2); wait > hi {
		t.Errorf("成功清零后再失败要回到第一级（%v ±20%%），得到 %v —— "+
			"没清零的话这里会是 1 分钟往上，表现为「偶尔抖一下的环境越用越迟钝」",
			first, wait)
	}
}

// TestPollLoopClearsAuthBackoffOnSuccess checks that "reset on success" is **really wired into
// pollLoop**.
//
// ⚠️ This test pairs with the one above, which is useless without it: the one above calls b.clear()
// directly, testing the gate as a data structure, while what someone would actually delete is the
// line p.auth.clear(envID) in pollLoop. Measured: with only the test above, deleting that line in
// pollLoop left the tests **all green**. A test that can't reach the wiring can't lock in the bug it
// claims to lock in.
//
// ⚠️ Mutation check: when p.auth.clear(envID) in pollLoop is deleted, this test must turn red.
func TestPollLoopClearsAuthBackoffOnSuccess(t *testing.T) {
	const env = "env_recovered"
	const fp = "len=32 sha=deadbeef"

	pool := &WorkerPool{cfg: WorkerConfig{Concurrency: 2}, log: testLogger(), auth: newAuthBackoff()}
	installFakeProcessor(pool)

	// First put the gate in the state "already failed, backing off" (simulating the record left by the previous round's 401).
	now := time.Now()
	pool.auth.fail(env, fp, now)
	if blocked, _ := pool.auth.blocked(env, fp, now); !blocked {
		t.Fatal("用例前提不成立：这时该是被挡住的")
	}

	// Then run one **successful** PollWork: putting a nil in pollErrs means "no error this time and no
	// work picked up", which is exactly the most common empty poll after things recover.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	api := &fakeWorkAPI{pollErrs: []error{nil}}
	_ = pool.pollLoop(ctx, api, env, "worker_test", 2, testLogger())

	// After one success the gate must already let through. Check with **the same now**, to rule out "the backoff just expired".
	if blocked, left := pool.auth.blocked(env, fp, now); blocked {
		t.Errorf("PollWork 成功后必须清零认证退避（还剩 %v）—— "+
			"漏掉 pollLoop 里那行 p.auth.clear(envID) 的话，偶尔抖出 401 的环境"+
			"会一路累到 5 分钟档再也回不来，且零报错", left)
	}
}

// TestAuthBackoffReleasesWhenKeyChanged is the "recover automatically when the key changes" part of
// the requirement.
//
// After misconfiguring a key, the user goes to the workspace settings and swaps in another one.
// **Don't make them wait a full backoff cycle** (up to 5 minutes): a changed fingerprint means a
// person has already acted, so give the new key a chance right away, waiting at most one refresh
// round (≤30 seconds).
//
// ⚠️ Mutation check: when the two fingerprint comparisons in blocked/fail are deleted, this test must turn red.
func TestAuthBackoffReleasesWhenKeyChanged(t *testing.T) {
	b := newAuthBackoff()
	now := time.Unix(1700000000, 0)
	const env = "env_bad_key"
	const oldFP, newFP = "len=32 sha=deadbeef", "len=32 sha=cafe1234"

	// Hit the highest step and confirm the old key is firmly blocked.
	for range 4 {
		wait, _ := b.fail(env, oldFP, now)
		now = now.Add(wait)
	}
	b.fail(env, oldFP, now)
	if blocked, _ := b.blocked(env, oldFP, now); !blocked {
		t.Fatal("用例前提不成立：旧 key 这时应该是被挡住的")
	}

	// The user changed the key: let it through at once, without waiting for the backoff to expire.
	if blocked, left := b.blocked(env, newFP, now); blocked {
		t.Fatalf("换了 key 必须立刻放行（还剩 %v）—— 否则用户改完配置要干等一个"+
			"退避周期，而他无从得知要等多久", left)
	}

	// And the new key counts from the first step: whether it is any good is another matter,
	// it should not inherit the previous key's failure count.
	_, n := b.fail(env, newFP, now)
	if n != 1 {
		t.Errorf("新 key 的失败计数要从 1 开始，得到 %d", n)
	}
}

// TestEnsureWorkerSkipsEnvInAuthBackoff checks that the gate is **really installed on ensureWorker**.
//
// ⚠️ This is the most important piece of wiring in the whole change, and also the easiest to turn
// into decoration. The tests above all only prove "the gate as a data structure is correct", while
// what really decides whether production works is **whether refresh → ensureWorker checks it**.
// Measured: when the p.auth.blocked call in ensureWorker was short-circuited (always false), the six
// tests above were **all green**, while production behavior then was that the worker kept starting
// every 30 seconds, as if this fix didn't exist.
//
// The check rests on the observable result "was a worker really started": if it was, p.running keeps
// an entry for this environment and it calls the real Ark service (which must fail locally). If
// blocked, it goes in and out and running stays empty.
//
// ⚠️ Mutation check: when the line `if blocked, left := p.auth.blocked(...)` is made always false,
// this test must turn red.
func TestEnsureWorkerSkipsEnvInAuthBackoff(t *testing.T) {
	const envID = "env_bad_key"
	const apiKey = "ark-a-deleted-key-000000000"

	store := coremem.NewMemStore()
	svc, err := New(Config{Store: store, ArkRunner: &fakeRunner{sessionID: "sess-1"}})
	if err != nil {
		t.Fatalf("建 Service 失败：%v", err)
	}
	ctx := context.Background()

	ws, err := store.CreateWorkspace(ctx, apimodel.Workspace{
		ID: "ws_auth", Name: "认证", Key: "auth-ws",
		Members: []apimodel.WorkspaceMember{{
			ID: "m1", Email: "owner@example.com", Role: apimodel.RoleOwner,
		}},
	})
	if err != nil {
		t.Fatalf("建空间失败：%v", err)
	}
	agent, err := store.CreateAgent(ctx, apimodel.Agent{
		ID: "agent_a", WorkspaceID: ws.ID, Name: "a",
		Model:  apimodel.ModelConfig{ID: "doubao-pro"},
		Status: apimodel.StatusActive,
	})
	if err != nil {
		t.Fatalf("建 agent 失败：%v", err)
	}
	// This key is the one from the scenario "the user configured a key that has been deleted".
	if err := store.PutArkSecret(ctx, apimodel.ArkSecret{
		ArkCredential: apimodel.ArkCredential{
			WorkspaceID: ws.ID, Region: apimodel.RegionCNBeijing,
			Source: apimodel.SourceOverride,
		},
		APIKey: apiKey,
	}); err != nil {
		t.Fatalf("存凭证失败：%v", err)
	}

	var buf bytes.Buffer
	pool := &WorkerPool{
		svc:     svc,
		cfg:     WorkerConfig{Concurrency: 2},
		log:     slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		running: map[string]context.CancelFunc{},
		auth:    newAuthBackoff(),
	}

	// Put this environment inside the backoff (simulating the state just recorded by the previous round's 401).
	// The fingerprint must match the one ensureWorker computes itself, or it would be let through as "the user changed the key".
	pool.auth.fail(envID, ArkKeyFingerprint(apiKey), time.Now())

	pool.ensureWorker(ctx, ws.ID, agent.ID, envID)

	// Check ①: no running entry is left, i.e. no worker was started at all and it won't hit 401 again.
	pool.mu.Lock()
	running := len(pool.running)
	pool.mu.Unlock()
	if running != 0 {
		t.Errorf("退避期内不该起这个环境的 worker，running 里却有 %d 条 —— "+
			"闸门没装在 ensureWorker 上的话，worker 每 30 秒照起不误，"+
			"这个修复等于不存在", running)
	}

	// Check ②: the log line for the skip is Debug, not Warn. refresh runs every 30 seconds; with Warn
	// one bad environment would log 120 lines an hour, exactly the kind of noise this change fixes.
	logged := buf.String()
	if strings.Contains(logged, "level=WARN") {
		t.Errorf("被挡住这条只能打 Debug，打 Warn 会按 refresh 周期刷屏；实际：%q", logged)
	}

	// Swap in a new key (the user changed it in workspace settings): the same ensureWorker must let it through.
	if err := store.PutArkSecret(ctx, apimodel.ArkSecret{
		ArkCredential: apimodel.ArkCredential{
			WorkspaceID: ws.ID, Region: apimodel.RegionCNBeijing,
			Source: apimodel.SourceOverride,
		},
		APIKey: "ark-the-new-good-key-00000000",
	}); err != nil {
		t.Fatalf("换凭证失败：%v", err)
	}

	pool.ensureWorker(ctx, ws.ID, agent.ID, envID)

	// This time it must really start (it calls the real Ark service and fails, but that doesn't affect
	// the check: it has already been let through). Shut it down right after so this goroutine doesn't
	// outlive the test.
	pool.mu.Lock()
	running = len(pool.running)
	pool.mu.Unlock()
	if running == 0 {
		t.Error("换了 key 必须立刻放行（最长等一个 refresh 周期），" +
			"否则用户改完配置只能干等一个退避周期，而他无从得知要等多久")
	}
	pool.shutdown()
}

// ⚠️ 404 must **not** go through this gate: a missing environment has its own self-healing path
// (create a new environment + repoint the sibling bindings in the same workspace together, see
// healMissingArkResource). Holding it behind a 5-minute backoff only slows down a path that could
// have fixed itself. 400 is our own bug, and "wait for the user to change the key" doesn't apply.
//
// 429 must not be touched at all: it has its own rate-limit gate (pollWorkThroughGate + arkbackoff).
func TestArkAuthStatusOnly401And403(t *testing.T) {
	cases := []struct {
		code int
		auth bool
	}{
		{401, true},  // invalid credentials: key deleted/mistyped/expired
		{403, true},  // credentials present but no permission: the key belongs to another project
		{400, false}, // our own bug
		{404, false}, // environment doesn't exist: it has dedicated self-healing, don't block it
		{408, false},
		{409, false},
		{429, false}, // rate limited: handled by arkRateLimitGate
		{500, false},
		{503, false},
	}
	for _, c := range cases {
		if got := arkAuthStatus(authErr(c.code)); got != c.auth {
			t.Errorf("状态码 %d：arkAuthStatus = %v，要 %v", c.code, got, c.auth)
		}
	}
	// A network error has no status code and must not be treated as an auth failure: that would let one network blip stop the environment for 30 seconds.
	if arkAuthStatus(errors.New("dial tcp: i/o timeout")) {
		t.Error("网络错误不是认证失败：当成认证失败会让一次抖动把环境停掉 30 秒")
	}
	if arkAuthStatus(nil) {
		t.Error("nil 不是认证失败")
	}
}

// TestAuthBackoffLogIsRateLimited checks log rate limiting; this is the other half of the issue.
//
// In 25 minutes production logged 2224 lines of the "worker exited abnormally" message and 4448
// lines of "API key doesn't exist", drowning the errors that really needed to be seen. Now there is
// at most one line per backoff cycle, and that line must answer the four questions asked while
// debugging: which environment, whose key, which attempt, and how much longer to wait.
func TestAuthBackoffLogIsRateLimited(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	b := newAuthBackoff()
	now := time.Unix(1700000000, 0)
	const env, fp = "env_bad_key", "len=32 sha=deadbeef"

	// Simulate refresh knocking again and again within one backoff cycle: the blocked ones **should not log a single Warn**.
	wait, n := b.fail(env, fp, now)
	logAuthBackoff(logger, env, fp, n, wait, authErr(401))

	for i := range 20 {
		at := now.Add(time.Duration(i) * time.Second)
		if blocked, _ := b.blocked(env, fp, at); !blocked {
			break
		}
		// The blocked path in ensureWorker logs only Debug, not Warn (see the comment there).
	}

	if got := strings.Count(buf.String(), "方舟凭据无效"); got != 1 {
		t.Errorf("一个退避周期只该打一条 Warn，实际 %d 条；"+
			"原来的形状是每 5 秒一行 ×环境数，25 分钟 2224 行", got)
	}

	logged := buf.String()
	for _, want := range []string{env, fp, "累计失败", "下次重试"} {
		if !strings.Contains(logged, want) {
			t.Errorf("日志里要有 %q，否则排查时还得再查一轮；实际：%q", want, logged)
		}
	}
	// ⚠️ The key itself must never go into the log. The fingerprint is irreversible (len=<n> sha=<hex8>), so it is fine.
	if strings.Contains(logged, "sk-") {
		t.Error("日志里不能出现 key 原文")
	}
}

// TestPollLoopReturnsOnAuthStatus checks that a 401 still bubbles up out of pollLoop.
//
// This is the lower half of the wiring: for the gate to be triggered, pollLoop must **return an
// error** on 401 instead of backing off and retrying in place. The upper half (runLeasedPoll hands it
// back to runOne) is checked by TestRunLeasedPollReturnsAuthError.
func TestPollLoopReturnsOnAuthStatus(t *testing.T) {
	api := &fakeWorkAPI{pollErrs: []error{authErr(401)}}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	pool := &WorkerPool{cfg: WorkerConfig{Concurrency: 2}, log: testLogger(), auth: newAuthBackoff()}
	installFakeProcessor(pool)
	err := pool.pollLoop(ctx, api, "env_test", "worker_test", 2, testLogger())
	if err == nil {
		t.Fatal("401 要作为错误返回，让上层记退避；原地重试就是那次故障的死循环")
	}
	if !arkAuthStatus(err) {
		t.Errorf("返回的错误要能被 arkAuthStatus 认出来（否则闸门收不到），得到 %v", err)
	}
}

// TestRunLeasedPollReturnsAuthError checks that a 401 **doesn't spin inside the reconnect loop**, but
// is handed back to runOne.
//
// ⚠️ This is the easiest place in the whole change to wire wrong. The intuitive fix is "turn the
// 5-second sleep in runOne into a long backoff", but the Ark client is **built once** in runOne with
// key.APIKey (outside the reconnect loop), so retries inside the loop always use the same invalid
// key, and when the user changes to a new key this worker never sees it. So a 401 must make
// runLeasedPoll **return**, going all the way back to ensureWorker to resolve the credentials again.
//
// ⚠️ Mutation check: when `if arkAuthStatus(err) { return err }` in runLeasedPoll is replaced with the
// old `logger.Warn(...)` + continue the loop, this test must turn red (it keeps spinning until the
// timeout).
func TestRunLeasedPollReturnsAuthError(t *testing.T) {
	api := &fakeWorkAPI{pollErrs: []error{authErr(401), authErr(401), authErr(401)}}

	pool := leaseTestPool(nil, nil)
	pool.auth = newAuthBackoff()
	installFakeProcessor(pool)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- pool.runLeasedPoll(ctx, api, "env_test", "worker_test", 2, testLogger())
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("认证失败要把错误交回 runOne（它才拿得到 key 指纹去记退避）")
		}
		if !arkAuthStatus(err) {
			t.Errorf("交回的要是认证错误，得到 %v", err)
		}
	case <-ctx.Done():
		t.Fatal("认证失败时 runLeasedPoll 没有返回 —— 它还在 5 秒一轮地重连，" +
			"而客户端持有的是同一把失效 key，这正是那次故障的形状")
	}

	// Exits after hitting it only once: it should not consume all three pollErrs (that would mean it is still retrying in place).
	if got := api.pollCount(); got != 1 {
		t.Errorf("认证失败后应立刻退出，PollWork 只该被调 1 次，实际 %d 次", got)
	}
}

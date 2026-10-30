// Storage contract tests for the GitHub device flow (run once each on mem and pg), plus one end-to-end
// concurrency test on PG: when one flow is polled concurrently, only one session is created.

package authdao

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/shared/infra/github/githubtest"
)

func TestGitHubFlowSuite(t *testing.T) {
	for _, impl := range []struct {
		name    string
		factory storeFactory
	}{{"mem", memFactory}, {"pg", pgFactory}} {
		t.Run(impl.name, func(t *testing.T) {
			t.Run("抢轮询与间隔", func(t *testing.T) { testGitHubClaim(t, impl.factory) })
			t.Run("slow_down 持久化", func(t *testing.T) { testGitHubSlowDown(t, impl.factory) })
			t.Run("并发消费只成功一次", func(t *testing.T) { testGitHubConsumeOnce(t, impl.factory) })
			t.Run("并发抢轮询只成功一次", func(t *testing.T) { testGitHubClaimConcurrent(t, impl.factory) })
			t.Run("过期即不存在", func(t *testing.T) { testGitHubExpired(t, impl.factory) })
		})
	}
}

func sampleGitHubFlow(id string, now time.Time) auth.GitHubFlow {
	return auth.GitHubFlow{
		FlowID: id, BindingHash: "bind-" + id, DeviceCode: "dc-" + id,
		UserCode: "ABCD-1234", VerificationURI: "https://github.com/login/device",
		IntervalSeconds: 5, NextPollAt: now.Add(5 * time.Second),
		CreatedAt: now, ExpiresAt: now.Add(15 * time.Minute),
	}
}

func testGitHubClaim(t *testing.T, factory storeFactory) {
	store := factory(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	if err := store.PutGitHubFlow(ctx, sampleGitHubFlow("f1", now)); err != nil {
		t.Fatal(err)
	}
	// Not time yet.
	f, err := store.ClaimGitHubPoll(ctx, "f1", "bind-f1", now)
	if !errors.Is(err, auth.ErrPollTooSoon) || f.IntervalSeconds != 5 || f.DeviceCode != "" {
		t.Fatalf("没到时间应 ErrPollTooSoon 且不给 device code: %+v %v", f, err)
	}
	// Wrong binding = does not exist.
	if _, err := store.ClaimGitHubPoll(ctx, "f1", "bind-wrong", now.Add(5*time.Second)); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("binding 不对应 ErrNotFound: %v", err)
	}
	if _, err := store.ClaimGitHubPoll(ctx, "nope", "bind-f1", now.Add(5*time.Second)); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("flow 不存在应 ErrNotFound: %v", err)
	}
	// Time reached: claimed, got the device code, and next_poll_at was pushed back.
	at := now.Add(5 * time.Second)
	f, err = store.ClaimGitHubPoll(ctx, "f1", "bind-f1", at)
	if err != nil || f.DeviceCode != "dc-f1" {
		t.Fatalf("到点应抢到: %+v %v", f, err)
	}
	if !f.NextPollAt.Equal(at.Add(5 * time.Second)) {
		t.Errorf("next_poll_at 应推到 %v，得到 %v", at.Add(5*time.Second), f.NextPollAt)
	}
	// Claim again at the same instant: already pushed back.
	if _, err := store.ClaimGitHubPoll(ctx, "f1", "bind-f1", at); !errors.Is(err, auth.ErrPollTooSoon) {
		t.Fatalf("刚抢过应 ErrPollTooSoon: %v", err)
	}
}

func testGitHubSlowDown(t *testing.T, factory storeFactory) {
	store := factory(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	if err := store.PutGitHubFlow(ctx, sampleGitHubFlow("f1", now)); err != nil {
		t.Fatal(err)
	}
	n, err := store.SlowDownGitHubFlow(ctx, "f1", 0, now)
	if err != nil || n != 10 {
		t.Fatalf("slow_down 后应为 10 秒: %d %v", n, err)
	}
	// If GitHub gives a larger value, use it.
	if n, _ = store.SlowDownGitHubFlow(ctx, "f1", 40, now); n != 40 {
		t.Fatalf("应取 GitHub 给的 40，得到 %d", n)
	}
	// Persisted: at the old interval it still cannot be claimed, only at the new interval, and the interval is still 40.
	if _, err := store.ClaimGitHubPoll(ctx, "f1", "bind-f1", now.Add(10*time.Second)); !errors.Is(err, auth.ErrPollTooSoon) {
		t.Fatalf("没满新间隔应 ErrPollTooSoon: %v", err)
	}
	f, err := store.ClaimGitHubPoll(ctx, "f1", "bind-f1", now.Add(40*time.Second))
	if err != nil || f.IntervalSeconds != 40 {
		t.Fatalf("满新间隔应抢到且间隔为 40: %+v %v", f, err)
	}
	if _, err := store.SlowDownGitHubFlow(ctx, "nope", 0, now); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("不存在应 ErrNotFound: %v", err)
	}
}

func testGitHubConsumeOnce(t *testing.T, factory storeFactory) {
	store := factory(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	if err := store.PutGitHubFlow(ctx, sampleGitHubFlow("f1", now)); err != nil {
		t.Fatal(err)
	}
	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := store.ConsumeGitHubFlow(ctx, "f1", now)
			if err != nil {
				t.Error(err)
				return
			}
			if ok {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("并发消费成功了 %d 次，期望 1", won)
	}
	if _, err := store.ClaimGitHubPoll(ctx, "f1", "bind-f1", now.Add(time.Minute)); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("消费后应不存在: %v", err)
	}
}

func testGitHubClaimConcurrent(t *testing.T, factory storeFactory) {
	store := factory(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	if err := store.PutGitHubFlow(ctx, sampleGitHubFlow("f1", now)); err != nil {
		t.Fatal(err)
	}
	at := now.Add(5 * time.Second)
	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.ClaimGitHubPoll(ctx, "f1", "bind-f1", at)
			if err == nil {
				mu.Lock()
				got++
				mu.Unlock()
			} else if !errors.Is(err, auth.ErrPollTooSoon) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got != 1 {
		t.Fatalf("同一时刻并发抢轮询成功了 %d 次，期望 1", got)
	}
}

func testGitHubExpired(t *testing.T, factory storeFactory) {
	store := factory(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	if err := store.PutGitHubFlow(ctx, sampleGitHubFlow("f1", now)); err != nil {
		t.Fatal(err)
	}
	later := now.Add(15 * time.Minute)
	if _, err := store.ClaimGitHubPoll(ctx, "f1", "bind-f1", later); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("过期应 ErrNotFound: %v", err)
	}
	if ok, err := store.ConsumeGitHubFlow(ctx, "f1", later); err != nil || ok {
		t.Fatalf("过期的行不能被消费成功: %v %v", ok, err)
	}
	if err := store.DeleteGitHubFlow(ctx, "f1"); err != nil {
		t.Errorf("删不存在的行不算错: %v", err)
	}
}

// End-to-end on PG: real Service + real github.Client + fake GitHub; the same flow is polled concurrently
// 16 times (simulating several instances receiving the poll at once); only one session may be created and GitHub asked only once.
func TestGitHubConcurrentPollPG(t *testing.T) {
	store := pgFactory(t).(*PGStore)
	gh := githubtest.New(t)
	gh.Interval = 1 // real clock; wait 1 second and it can be polled
	svc, err := auth.New(auth.Config{Store: store, GitHub: auth.GitHubConfig{
		Client: gh.Client(t), AllowedUsers: []string{"octo-cat"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, err := svc.StartGitHubLogin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)

	const n = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, pending := 0, 0
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := svc.PollGitHubLogin(ctx, st.FlowID, st.BindingToken)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				if !errors.Is(err, auth.ErrGitHubFlowExpired) {
					t.Errorf("意外错误: %v", err)
				}
			case res.Pending:
				pending++
			default:
				ok++
			}
		}()
	}
	close(start)
	wg.Wait()
	if ok != 1 {
		t.Fatalf("并发 %d 次轮询建了 %d 个会话（pending %d），期望 1", n, ok, pending)
	}
	if c := gh.TokenCalls(); c != 1 {
		t.Errorf("应只问 GitHub 一次，实际 %d 次", c)
	}
	var sessions int
	if err := pgEnv.client.QueryRow(ctx, `select count(*) from auth_sessions where email = 'octo@example.com'`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 1 {
		t.Errorf("库里有 %d 个会话，期望 1", sessions)
	}
	var flows int
	if err := pgEnv.client.QueryRow(ctx, `select count(*) from auth_github_flows`).Scan(&flows); err != nil {
		t.Fatal(err)
	}
	if flows != 0 {
		t.Errorf("成功后 flow 行应已删除，还剩 %d 行", flows)
	}
	// The raw device_code only lives in the database until consumed; the GitHub token is never stored.
}

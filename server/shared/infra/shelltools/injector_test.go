// Tests for **which log level** is used when SessionEnvs fails to fetch.
//
// A separate file instead of merging into scrub_test.go / envalias_test.go: those two are about
// "credentials must not leak out" and "credentials get injected", while this one is about
// observability: one branch mixes two kinds of errors, and the wrong level makes the inspection
// list meaningless.
//
// # Why this is worth pinning down
//
// The level of this branch affects no feature: injection degrades as usual, tools run as usual,
// and getting it wrong gives **zero errors**. The only symptom shows up far away: each production
// inspection round that pulls `_level=Error` gets one more noise entry to rule out by hand (this
// came up again and again in an issue's comments). Without a test holding it, such a thing slowly
// drifts back, and nobody notices the day it does.

package shelltools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/model/persistmodel"
)

// failingSessionEnvs is a stub for "SessionEnvs returns an error".
//
// ⚠️ Deliberately separate from fakeSessionEnvs in scrub_test.go: that one's err is always nil,
// while this file tests exactly the err != nil path. More importantly, it must be able to return
// envs **and** err together: the `SessionEnvs` contract is "partial success" (see the interface
// comment in credential.go), and a stub that can only error without giving values could never
// test "are the parts already computed still injected on error".
type failingSessionEnvs struct {
	envs       map[string]string
	secretKeys []string
	err        error
}

func (f failingSessionEnvs) SessionEnvs(
	_ context.Context, _ string,
) (map[string]string, []string, []EnvAlias, error) {
	return f.envs, f.secretKeys, nil, f.err
}

// runWithFailingEnvs runs n executions and returns the whole log and the last env the executor
// received.
//
// The log level is set to Debug: **not the default Info**, otherwise things other than Warn/Error
// are filtered out, and "is it still logged after degrading" cannot be tested (it would be a false
// green as "not logged").
func runWithFailingEnvs(t *testing.T, src SessionEnvSource, n int) (string, map[string]string) {
	t.Helper()
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	echo := &echoEnvExecer{}
	ex := NewEnrichingExecer(echo, "sesn-envfail", src, logger)

	for i := 0; i < n; i++ {
		if _, err := ex.Exec(context.Background(), ExecSpec{
			Path: "bash", Args: []string{"-lc", "env"},
		}); err != nil {
			t.Fatalf("执行失败（注入失败绝不能让工具失败）：%v", err)
		}
	}
	return buf.String(), echo.gotEnvs
}

// ⭐ 1. Precondition not met (the user has not authorized yet) logs Warn, not Error.
//
// This is the very thing being fixed: the agent enabled "Feishu user authorization" while some
// user has not authorized the Feishu app yet. The platform is fine, and the error text says where
// to click, but it used to appear as Error in the inspection list and had to be ruled out by hand
// on every round.
//
// Mutation check: delete the errors.Is(err, persistmodel.ErrPrecondition) branch in injector.go
// (both kinds go to Error), and this test turns red right away.
func TestPreconditionEnvErrorLogsWarnNotError(t *testing.T) {
	// Build an error with the same shape as in production: addLarkUserEnvs in core/sessionenv.go
	// wraps the LarkUserProvider error carrying ErrPrecondition one more time, so wrap once here too.
	// **It must be tested wrapped with %w**, otherwise it tests "the top level is the sentinel", a case
	// that does not exist, while what can really break is "does errors.Is still match after two layers
	// of wrapping".
	inner := fmt.Errorf("%w: alice@example.com 还没对飞书应用 cli_xxx 做过用户授权"+
		"（去「授权管理」里选中它再授权一次）", persistmodel.ErrPrecondition)
	err := fmt.Errorf("这个 agent 勾了「飞书用户授权」，但取不到凭据（空间 ws_demo）: %w", inner)

	logs, _ := runWithFailingEnvs(t, failingSessionEnvs{err: err}, 1)

	if strings.Contains(logs, "level=ERROR") {
		t.Errorf("用户还没授权是他自己能修的配置问题，不该进 Error 清单伪装成平台故障：\n%s", logs)
	}
	if !strings.Contains(logs, "level=WARN") {
		t.Errorf("降级不等于不说——调试时仍要看得见，实际日志：\n%s", logs)
	}
}

// ⭐ 2. Degrading must not swallow "what to do".
//
// Just lowering the level is not enough: whoever sees this Warn must know at a glance **which
// authorization, and who fixes it**. The guidance in the error chain (the "select it in
// authorization management and authorize again" text) is the only actionable clue for the user.
// If it is lost, this log line becomes "some credential was not obtained", which says nothing.
func TestPreconditionEnvErrorKeepsGuidanceText(t *testing.T) {
	inner := fmt.Errorf("%w: alice@example.com 还没对飞书应用 cli_xxx 做过用户授权"+
		"（去「授权管理」里选中它再授权一次）", persistmodel.ErrPrecondition)
	err := fmt.Errorf("这个 agent 勾了「飞书用户授权」，但取不到凭据（空间 ws_demo）: %w", inner)

	logs, _ := runWithFailingEnvs(t, failingSessionEnvs{err: err}, 1)

	for _, want := range []string{
		"飞书用户授权",            // which authorization
		"alice@example.com", // who fixes it
		"再授权一次",             // what to do
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("错误链里的关键信息 %q 被吞了，日志：\n%s", want, logs)
		}
	}
}

// ⭐ 3. Real platform failures are still Error.
//
// This is the control group for the one above, and **more important than it**: lowering too far
// (everything Warn) would make real failures like network down or platform 5xx disappear from the
// inspection list. That trades a noisy problem for a silent one, which is strictly worse.
//
// Mutation check: change the else branch in injector.go to i.log.Warn too, and this test turns
// red right away.
func TestPlatformEnvErrorStaysError(t *testing.T) {
	err := fmt.Errorf("取飞书用户凭据失败: %w", errors.New("dial tcp 10.0.0.1:443: connect: connection refused"))

	logs, _ := runWithFailingEnvs(t, failingSessionEnvs{err: err}, 1)

	if !strings.Contains(logs, "level=ERROR") {
		t.Errorf("网络不通是真故障，必须留在 Error 清单里（值班的人要看）：\n%s", logs)
	}
}

// ⭐ 4. Both kinds are said only once.
//
// The existing first-only rate limit must not break because of the level split: this branch runs
// on **every tool call**, a conversation round has dozens of commands, and without dedup the real
// failure would be scrolled off the screen.
func TestEnvErrorLoggedOncePerInjector(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"前置条件", fmt.Errorf("勾了但拿不到: %w",
			fmt.Errorf("%w: 还没授权", persistmodel.ErrPrecondition))},
		{"真故障", errors.New("平台不通")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs, _ := runWithFailingEnvs(t, failingSessionEnvs{err: tc.err}, 3)
			// Count **log lines**, not keyword occurrences: the word for "credential" appears a different
			// number of times in the two messages (the Error one has two), so counting words would misjudge
			// "said once" as twice. Line count is what this test really pins; the rate limit is about the
			// number of lines.
			if n := strings.Count(logs, "level="); n != 1 {
				t.Errorf("执行 3 次期望只说一次，实际 %d 条：\n%s", n, logs)
			}
		})
	}
}

// ⭐ 5. The env already computed when the error happens is still injected.
//
// The `SessionEnvs` contract is "partial success" (see the interface comment in credential.go and
// the implementation in core/sessionenv.go): the custom env the user filled in is already computed
// and should not be lost just because one authorization could not be fetched.
//
// ⚠️ This has nothing to do with levels, but it must sit with the ones above: when changing levels
// it is very easy to write `if err != nil { return }` along the way, which silently wipes all of
// the user's custom env, and the symptom ("the env I set is not in the container") does not point
// to that change at all.
func TestEnvErrorStillInjectsWhatSucceeded(t *testing.T) {
	src := failingSessionEnvs{
		envs: map[string]string{"MY_OWN_VAR": "kept"},
		err: fmt.Errorf("勾了但拿不到: %w",
			fmt.Errorf("%w: 还没授权", persistmodel.ErrPrecondition)),
	}

	_, gotEnvs := runWithFailingEnvs(t, src, 1)

	if got := gotEnvs["MY_OWN_VAR"]; got != "kept" {
		t.Errorf("一个授权取不到，不该把用户自己填的 env 也丢掉（部分成功语义），实际 %q", got)
	}
}

// 6. Errors without ErrPrecondition always go to Error.
//
// The only rule is "is the sentinel there": real failures like network down or platform 5xx while
// fetching credentials return bare errors that need the on-call person, so they must be Error.
// Even if the error text says "not authorized yet", without the sentinel it does not go into the
// Warn branch: guessing the level from text is unreliable.
func TestNonSentinelErrorStillLogsError(t *testing.T) {
	err := fmt.Errorf("取 alice 的会话凭据失败（多半是他还没授权）: %w",
		errors.New("upstream: 该用户尚未授权"))

	logs, _ := runWithFailingEnvs(t, failingSessionEnvs{err: err}, 1)

	if !strings.Contains(logs, "level=ERROR") {
		t.Errorf("不带 ErrPrecondition 的错误应当打 ERROR，实际日志：\n%s", logs)
	}
}

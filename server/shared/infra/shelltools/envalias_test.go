// Tests for credential aliases (injecting a platform credential once more under a variable name
// the user picks).
//
// A separate file instead of merging into scrub_test.go: that file is about "injected credentials
// must not leak out", this one is about "is the alias feature itself correct". The two fail for
// completely different reasons.
// The only overlap is redaction: alias values are credentials too, so those assertions are
// pinned again here.

package shelltools

import (
	"context"
	"strings"
	"testing"
)

// aliasEnricher builds an injection decorator wrapping echoEnvExecer, so each test case does not
// copy five lines.
//
// When amaToken is non-empty, session env gets an extra AMA_TOKEN (reported as a secret); an
// empty string means this session has no AMA_TOKEN, which is exactly what "authorization not
// enabled" looks like in the executor.
func aliasEnricher(t *testing.T, amaToken string, envs fakeSessionEnvs) (Execer, *echoEnvExecer) {
	t.Helper()
	echo := &echoEnvExecer{}
	if amaToken != "" {
		merged := make(map[string]string, len(envs.envs)+1)
		for k, v := range envs.envs {
			merged[k] = v
		}
		merged[EnvAMAToken] = amaToken
		envs.envs = merged
		envs.secretKeys = append(append([]string(nil), envs.secretKeys...), EnvAMAToken)
	}
	return NewEnrichingExecer(echo, "sesn-alias", envs, quietLog()), echo
}

func aliasExec(t *testing.T, ex Execer) *Result {
	t.Helper()
	res, err := ex.Exec(context.Background(), ExecSpec{
		Path: "bash", Args: []string{"-lc", "env"},
	})
	if err != nil {
		t.Fatalf("执行失败：%v", err)
	}
	return res
}

// ⭐ 1. The alias really gets the value of the source credential.
//
// This is the feature itself. ⚠️ The assertion is on **the env the executor received**
// (echo.gotEnvs), not on command output: the output is scrubbed, so asserting "has a value" on
// it could never hold.
func TestEnvAliasGetsCredentialValue(t *testing.T) {
	const tok = "amak_alias-should-see-this-value-0123456789"
	ex, echo := aliasEnricher(t, tok, fakeSessionEnvs{
		aliases: []EnvAlias{{Key: "MY_CLI_TOKEN", From: EnvAMAToken}},
	})
	aliasExec(t, ex)

	if got := echo.gotEnvs[EnvAMAToken]; got != tok {
		t.Fatalf("源变量本身应当照常注入，实际 %q", got)
	}
	if got := echo.gotEnvs["MY_CLI_TOKEN"]; got != tok {
		t.Errorf("❌ 别名没拿到源凭据的值。\n"+
			"这是本功能的全部意义：只认自己那个变量名的第三方 CLI 靠它才能跑起来。\n"+
			"期望 %q，实际 %q", tok, got)
	}
}

// ⭐⭐ 2. Alias values must be redacted.
//
// # Why this matters more than "does the feature work"
//
// Missing it has a **permanent** effect: one `env` from the agent → plain text goes into
// tool_result → stored in RunEvent → into the model context → pushed to the user's browser over
// SSE. RunEvent is a permanent record and cannot be deleted.
//
// An alias is scrubbed "automatically" because its value is **identical** to the source, and
// shared/infra/secret replaces **by value**. This test pins that this reasoning really holds,
// rather than us just believing it does.
func TestEnvAliasValueIsScrubbed(t *testing.T) {
	// Use the Feishu token as the source and **deliberately do not report secretKeys**: then the only
	// path left that can scrub the alias value is the alias path (it checks secretEnvNames), so the
	// test really pins alias scrubbing.
	const tok = "u-lark-alias-value-must-be-scrubbed-0123"
	ex, _ := aliasEnricher(t, "", fakeSessionEnvs{
		envs:    map[string]string{EnvLarkUserAccessToken: tok},
		aliases: []EnvAlias{{Key: "MY_LARK_TOKEN", From: EnvLarkUserAccessToken}},
	})
	res := aliasExec(t, ex)

	// Check all three fields: stdout (env), stderr (set -x), Err (the executor echoes the spec).
	for name, field := range map[string]string{
		"Stdout": res.Stdout, "Stderr": res.Stderr, "Err": res.Err,
	} {
		if strings.Contains(field, tok) {
			t.Errorf("❌ %s 里有明文凭据——别名把它原样带出执行端了。\n"+
				"泄漏是永久的（tool_result → RunEvent → 模型上下文 → 浏览器）。\n"+
				"→ 检查 enrich.go 里别名那段有没有把值并进 toScrub：%s", name, field)
		}
	}
	// The variable name itself **should** remain: it is not a secret, and scrubbing it would hide
	// "is this alias configured" when debugging.
	if !strings.Contains(res.Stdout, "MY_LARK_TOKEN=") {
		t.Error("别名的变量名不该被抹——排查时要看得见它确实设了")
	}
}

// ⭐⭐⭐ 3. An alias must not become a way to bypass the "authorization checkbox".
//
// # The most dangerous failure mode of this feature
//
// When the agent has not enabled an authorization, the matching credential (for example
// AMA_TOKEN) should not enter the executor at all. If the alias were implemented as "fetch the
// credential value itself", an agent **with no authorization enabled** could get that credential
// just by declaring an alias, with **zero errors**, while the checkbox in the UI still looks fine.
//
// The correct implementation takes the value from **the merged result**: if the source is not
// there, the alias naturally has no value. This test pins exactly that.
func TestEnvAliasDoesNotBypassIntegrationGrant(t *testing.T) {
	// Empty amaToken = this authorization not enabled; the source is not injected.
	//
	// ⚠️ **A plain env must be given too**, otherwise injection takes the early-return "nothing at
	// all" branch and alias resolution never runs. Then this test would go green **for the wrong reason**.
	ex, echo := aliasEnricher(t, "", fakeSessionEnvs{
		envs:    map[string]string{"DEPLOY_REGION": "cn-east"},
		aliases: []EnvAlias{{Key: "MY_CLI_TOKEN", From: EnvAMAToken}},
	})
	aliasExec(t, ex)

	if _, ok := echo.gotEnvs[EnvAMAToken]; ok {
		t.Fatal("前提不成立：没勾授权时源变量不该被注入（这条用例白测了）")
	}
	if got, ok := echo.gotEnvs["MY_CLI_TOKEN"]; ok {
		t.Errorf("❌❌ 别名绕过了授权勾选：agent 没勾这个授权，却拿到了 MY_CLI_TOKEN=%q。\n"+
			"这是提权——「填个表单」换到了「拿到别人的凭据」。\n"+
			"→ 别名必须从 merged 里取值，不能自己去取凭据。", got)
	}
}

// ⭐⭐ 4. An alias cannot change platform reserved variables.
//
// If allowed, a user declaring `AMA_TOKEN ← LARKSUITE_CLI_USER_ACCESS_TOKEN` could replace the
// platform identity in the executor. It is the same escalation hole as "custom env sets a
// reserved key", just through another entrance.
// The write path (core/agent.go) is the first gate; this is the second (bad data and callers that
// bypass the write path).
func TestEnvAliasCannotOverridePlatformReservedKey(t *testing.T) {
	const amaToken = "amak_the-real-platform-identity-0123456789"
	const larkToken = "u-lark-a-different-credential-entirely"
	ex, echo := aliasEnricher(t, amaToken, fakeSessionEnvs{
		envs:       map[string]string{EnvLarkUserAccessToken: larkToken},
		secretKeys: []string{EnvLarkUserAccessToken},
		// Malicious declaration: point the platform's own AMA_TOKEN at another credential.
		aliases: []EnvAlias{{Key: EnvAMAToken, From: EnvLarkUserAccessToken}},
	})
	aliasExec(t, ex)

	if got := echo.gotEnvs[EnvAMAToken]; got != amaToken {
		t.Errorf("❌ 别名覆盖掉了平台注入的身份：期望仍是平台算出来的那把，实际 %q。\n"+
			"这等于「填个表单换掉执行端里的身份」——权限提升。", got)
	}
}

// 5. Nothing is produced when the source is not in the aliasable list.
//
// The list only holds the ones whose "value itself is a credential". Aliasing a public id like
// AMA_WORKSPACE_ID is pointless.
func TestEnvAliasIgnoresNonAliasableSource(t *testing.T) {
	ex, echo := aliasEnricher(t, "", fakeSessionEnvs{
		envs: map[string]string{EnvAMAWorkspaceID: "ws_demo"},
		// AMA_WORKSPACE_ID is a public id and is not in AliasableEnvKeys.
		aliases: []EnvAlias{{Key: "MY_WS", From: EnvAMAWorkspaceID}},
	})
	aliasExec(t, ex)

	// Note: the injection side (enrich.go) only checks "is the source in merged"; what really blocks
	// invalid sources is core/sessionenv.go and the write-path validation. This asserts the scope of
	// **the list itself**.
	if IsAliasableEnv(EnvAMAWorkspaceID) {
		t.Error("AMA_WORKSPACE_ID 不该是可别名的源：它是公开标识，不是凭据")
	}
	_ = echo
}

// ⭐ 6. AliasableEnvKeys and secretEnvNames must match.
//
// # What happens if one is missed
//
// Here but not there = the alias value **is not scrubbed**; plain text leaves the executor, permanently.
// There but not here = the user cannot alias a credential (a missing feature, but not a security issue).
//
// Both tables decide the same thing ("is this variable's value a credential"), so they must match
// item by item. When adding a new injected credential, **add it in both places**; forgetting one
// is not a compile error, only this test goes red.
func TestAliasableKeysMatchSecretEnvNames(t *testing.T) {
	for _, k := range AliasableEnvKeys() {
		if !secretEnvNames[k] {
			t.Errorf("❌ %s 可以被别名，但不在 secretEnvNames 里——"+
				"别名的值不会被抹，明文流出执行端且泄漏是永久的。\n"+
				"→ 往 scrub.go 的 secretEnvNames 里加上它", k)
		}
	}
	for k := range secretEnvNames {
		if !IsAliasableEnv(k) {
			t.Errorf("❌ %s 是凭据却不能被别名——用户没法给它起第三方 CLI 认的名字。\n"+
				"→ 往 credential.go 的 AliasableEnvKeys 里加上它（若确有理由排除，"+
				"在那个函数的注释里写明）", k)
		}
	}
}

// ⭐⭐ 7. Values the user marked "sensitive" on custom env must be scrubbed.
//
// # This pins a bug that really existed (found by testing with a probe)
//
// enrich.go computed the set to scrub by **actual value** (including those the user marked
// Secret), but newScrubber then **filtered the key names a second time** by `secretEnvNames`.
// User-defined key names are of course not in that platform variable table, so **none were scrubbed**:
//
//	MY_TOKEN=user-marked-secret-...   goes into stdout as-is, scrubber.active() == false
//
// The effect is exactly the same as other credential leaks, and harder to notice: the
// "sensitive" switch in the UI looks fine, and the user thinks he has already handled it.
//
// The fix is "do not decide the same thing twice": the decision stays with the caller, and
// newScrubber only does the replacing.
func TestUserSecretEnvIsScrubbed(t *testing.T) {
	const userSecret = "glsa_user_marked_this_one_as_secret_1234"
	ex, echo := aliasEnricher(t, "", fakeSessionEnvs{
		envs: map[string]string{
			"GRAFANA_TOKEN": userSecret,
			"DEPLOY_REGION": "cn-east", // plain entry, **should not** be scrubbed
		},
		secretKeys: []string{"GRAFANA_TOKEN"},
	})
	res := aliasExec(t, ex)

	// Precondition: the executor really received the plain text (otherwise this is a false green:
	// nothing was injected, so of course there is nothing to scrub).
	if got := echo.gotEnvs["GRAFANA_TOKEN"]; got != userSecret {
		t.Fatalf("执行端应当收到明文（否则注入没生效），实际 %q", got)
	}
	for name, field := range map[string]string{
		"Stdout": res.Stdout, "Stderr": res.Stderr, "Err": res.Err,
	} {
		if strings.Contains(field, userSecret) {
			t.Errorf("❌ %s 里有明文——用户勾了「敏感」但值根本没被抹。\n"+
				"泄漏是永久的（tool_result → RunEvent → 模型上下文 → 浏览器），\n"+
				"而界面上那个开关看起来是生效的。\n"+
				"→ 检查 newScrubber 有没有又按 secretEnvNames 过滤一遍键名：%s",
				name, field)
		}
	}
	// The reverse pair: plain env **should not** be scrubbed; scrubbing it would make command
	// output confusing to the user.
	if !strings.Contains(res.Stdout, "cn-east") {
		t.Error("用户的普通 env 不该被抹——那是他自己写的配置，抹了会让他看不懂输出")
	}
}

// Tests for credential scrubbing.
//
// They lock down one thing: **an injected token must not flow out of the execution backend
// through any exit**.
//
// Why this deserves a whole test file: once that token gets back to our process, it goes into
// Ark's context (so it may show up in the agent's thinking and replies), into RunEvent storage
// (**permanent** plain text), and into SSE pushed to the browser, all at once. One careless
// `env` is enough. And none of these paths **raise an error**: the leak is silent.

package shelltools

import (
	"context"
	"strings"
	"testing"
)

// theToken is the fake AMA_TOKEN shared by most cases.
//
// It is deliberately long and has an easy-to-spot tail: a short string would let bugs like
// "truncated into something that looks safe" slip past the assertions, and the tail is used to
// check for "a recognizable fragment was left behind".
const theToken = "amak_0123456789abcdef_s1gn4tur3-do-not-log-me"

// theBaseURL / theWorkspaceID are the two **non-secret** variables injected along with theToken.
const (
	theBaseURL     = "https://ama.example.com"
	theWorkspaceID = "ws_demo"
)

// scrubEnvs builds an env where credentials "were really injected".
func scrubEnvs() map[string]string {
	return map[string]string{
		EnvAMAToken:       theToken,
		EnvAMABaseURL:     theBaseURL,
		EnvAMAWorkspaceID: theWorkspaceID,
	}
}

// tokenSessionEnvs is a SessionEnvSource stub where "the session injected AMA_TOKEN".
func tokenSessionEnvs() fakeSessionEnvs {
	return fakeSessionEnvs{envs: scrubEnvs(), secretKeys: []string{EnvAMAToken}}
}

// ⭐ **None** of the three output fields may be skipped.
//
// Each one has a real leak scenario:
//   - stdout: `env` / `echo $VAR`, the most common;
//   - stderr: a script with `set -x` prints the expanded command line, token included;
//   - Err: some backend failures echo the whole ExecSpec back.
func TestScrubberCleansAllThreeOutputFields(t *testing.T) {
	s := scrubberFor(scrubEnvs())
	if !s.active() {
		t.Fatal("注入了凭据，抹除器应当生效")
	}

	res := &Result{
		Stdout: "AMA_TOKEN=" + theToken + "\nPATH=/usr/bin",
		Stderr: "+ curl -H 'Authorization: Bearer " + theToken + "' https://x",
		Err:    "exec failed with env: " + theToken,
	}
	got := s.result(res)

	for name, field := range map[string]string{
		"Stdout": got.Stdout, "Stderr": got.Stderr, "Err": got.Err,
	} {
		if strings.Contains(field, theToken) {
			t.Errorf("%s 里仍有明文 token：%s", name, field)
		}
		// No recognizable fragment of the original value may remain either.
		if strings.Contains(field, "s1gn4tur3-do-not-log-me") {
			t.Errorf("%s 里留下了 token 片段：%s", name, field)
		}
		if !strings.Contains(field, credentialPlaceholder) {
			t.Errorf("%s 应留下占位符（让人知道这里被抹了），实际：%s", name, field)
		}
	}

	// The normal content around it must stay; scrubbing too hard makes the output useless.
	if !strings.Contains(got.Stdout, "PATH=/usr/bin") {
		t.Error("不该动无关内容")
	}
	// The variable **name** must stay: when debugging you need to see "this variable is really set".
	if !strings.Contains(got.Stdout, EnvAMAToken) {
		t.Error("变量名不是秘密，不该抹掉")
	}
}

// **Do not modify the input.**
//
// Changing it in place causes the hardest kind of mismatch to debug: "some places see the
// scrubbed version, some see plain text".
func TestScrubberDoesNotMutateInput(t *testing.T) {
	s := scrubberFor(scrubEnvs())
	res := &Result{Stdout: "token=" + theToken}
	got := s.result(res)

	if !strings.Contains(res.Stdout, theToken) {
		t.Error("入参被就地改了——调用方可能还在引用它")
	}
	if got == res {
		t.Error("应当返回新对象")
	}
}

// When the token appears **several times** in one output, all of them must be scrubbed, not
// just the first.
//
// Things like `env | grep JWT` repeat it; scrubbing only the first is the same as not scrubbing.
func TestScrubberReplacesEveryOccurrence(t *testing.T) {
	s := scrubberFor(scrubEnvs())
	res := &Result{
		Stdout: theToken + " middle " + theToken + " end " + theToken,
	}
	got := s.result(res)
	if strings.Contains(got.Stdout, theToken) {
		t.Errorf("应抹掉全部出现，实际：%s", got.Stdout)
	}
	if n := strings.Count(got.Stdout, credentialPlaceholder); n != 3 {
		t.Errorf("应有 3 个占位符，实际 %d：%s", n, got.Stdout)
	}
}

// **Only credentials are scrubbed, not the service address or workspace id.**
//
// Those two are not secrets. Scrubbing them would hide "which environment the backend hit and
// which workspace it belongs to" while debugging.
func TestScrubberKeepsNonSecretEnvs(t *testing.T) {
	s := scrubberFor(scrubEnvs())
	res := &Result{
		Stdout: "AMA_BASE_URL=" + theBaseURL + "\nAMA_WORKSPACE_ID=" + theWorkspaceID,
	}
	got := s.result(res)
	if !strings.Contains(got.Stdout, theBaseURL) {
		t.Error("服务地址不是秘密，不该抹")
	}
	if !strings.Contains(got.Stdout, theWorkspaceID) {
		t.Error("空间 id 不是秘密，不该抹")
	}
}

// Without injected credentials, the scrubber is inactive and output is returned unchanged.
//
// This guarantees zero cost and zero effect on the "user without authorization" path.
func TestScrubberInactiveWithoutCredentials(t *testing.T) {
	for name, envs := range map[string]map[string]string{
		"nil":     nil,
		"空 map":   {},
		"只有非秘密变量": {EnvAMAWorkspaceID: theWorkspaceID},
	} {
		s := scrubberFor(envs)
		if s.active() {
			t.Errorf("%s：不该生效", name)
		}
		res := &Result{Stdout: "hello"}
		if got := s.result(res); got != res {
			t.Errorf("%s：不生效时应原样返回同一个对象", name)
		}
	}
}

// Absurdly short tokens are **deliberately not scrubbed**.
//
// Globally replacing a 3-character value would turn every chance match in the output into a
// placeholder and ruin normal output. Real credentials are all far longer than the threshold,
// so it does not affect real cases.
func TestScrubberIgnoresAbsurdlyShortSecrets(t *testing.T) {
	s := scrubberFor(map[string]string{EnvAMAToken: "abc"})
	if s.active() {
		t.Error("3 字符的值不该参与抹除——会把正常输出毁掉")
	}
	res := &Result{Stdout: "abcdef abc"}
	if got := s.result(res); got.Stdout != "abcdef abc" {
		t.Errorf("不该动，实际：%s", got.Stdout)
	}
}

// A nil Result must not panic.
func TestScrubberHandlesNilResult(t *testing.T) {
	s := scrubberFor(scrubEnvs())
	if got := s.result(nil); got != nil {
		t.Error("nil 应返回 nil")
	}
}

// ⭐ **End to end: go through the real injecting decorator; the credential must not appear in
// the return value.**
//
// The earlier cases test the scrubber itself. This one tests that **it is really wired into that
// path**. "Wrote the scrubber but did not wire it into Exec" is a zero-error failure, exactly
// the shape of the incidents in this repo.
//
// We use an Execer that echoes the env: only that can simulate real leaks like `env` / `set -x`.
func TestEnrichingExecerScrubsInjectedCredential(t *testing.T) {
	echo := &echoEnvExecer{}
	ex := NewEnrichingExecer(echo, "sesn-scrub", tokenSessionEnvs(), quietLog())

	res, err := ex.Exec(context.Background(), ExecSpec{
		Path: "bash", Args: []string{"-lc", "env"},
	})
	if err != nil {
		t.Fatalf("执行失败：%v", err)
	}

	// The stub really got the plain text. Otherwise this test is a false green (injection never
	// happened, so of course there is nothing to scrub).
	if got := echo.gotEnvs[EnvAMAToken]; got != theToken {
		t.Fatalf("执行端应当收到明文凭据（否则注入没生效），实际 %q", got)
	}
	// Non-secret session envs must also be injected as usual.
	if got := echo.gotEnvs[EnvAMAWorkspaceID]; got != theWorkspaceID {
		t.Errorf("非秘密的会话 env 也要注入，实际 %q", got)
	}

	// But none of the three fields returned to the upper layer may contain plain text.
	for name, field := range map[string]string{
		"Stdout": res.Stdout, "Stderr": res.Stderr, "Err": res.Err,
	} {
		if strings.Contains(field, theToken) {
			t.Errorf("Exec 返回的 %s 里有明文凭据——它会进方舟上下文与 RunEvent：%s", name, field)
		}
		if !strings.Contains(field, credentialPlaceholder) {
			t.Errorf("%s 应留下占位符，实际：%s", name, field)
		}
	}
}

// With no session env source configured, Exec works as usual and does not touch the output.
func TestEnrichingExecerWithoutCredentialsIsUnaffected(t *testing.T) {
	echo := &echoEnvExecer{}
	ex := NewEnrichingExecer(echo, "sesn-nocred", nil, quietLog())

	res, err := ex.Exec(context.Background(), ExecSpec{
		Path: "bash", Envs: map[string]string{"HARMLESS": "value"},
	})
	if err != nil {
		t.Fatalf("执行失败：%v", err)
	}
	if !strings.Contains(res.Stdout, "HARMLESS=value") {
		t.Errorf("没凭据时不该动输出，实际：%s", res.Stdout)
	}
}

// ⭐ **When a CLI caches the token on disk, reading it back via read/grep must also be scrubbed.**
//
// This covers a less obvious leak path: we inject the env → some CLI in the backend writes the
// token into its own cache file → the agent reads that file with the **read tool** → the plain
// text gets back to our process anyway.
//
// It is covered automatically because all six tools go through runShell → Execer, with no side
// path. This test pins that fact: if someone ever opens a shortcut from a tool straight to the
// session, it turns red.
func TestScrubCoversFileReadOfCachedToken(t *testing.T) {
	// Stub: simulate `cat` of a CLI cache file whose content contains the token.
	cat := &fileDumpExecer{content: `{"token":"` + theToken + `","site":"cn"}`}
	ex := NewEnrichingExecer(cat, "sesn-read", tokenSessionEnvs(), quietLog())

	// Go through the real path the read tool takes (runShell → Execer).
	res, err := runShell(context.Background(), ex, "/workspace",
		"cat /root/.cache/some-cli/token.json")
	if err != nil {
		t.Fatalf("执行失败：%v", err)
	}
	if strings.Contains(res.Stdout, theToken) {
		t.Errorf("read 读出来的缓存文件里有明文凭据：%s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, credentialPlaceholder) {
		t.Errorf("应留下占位符，实际：%s", res.Stdout)
	}
	// Non-secret fields in the file must stay, otherwise the agent cannot tell what the file is.
	if !strings.Contains(res.Stdout, `"site":"cn"`) {
		t.Errorf("不该动无关内容，实际：%s", res.Stdout)
	}
}

// fileDumpExecer simulates "reading a file" with fixed content.
type fileDumpExecer struct {
	content string
}

func (e *fileDumpExecer) Exec(_ context.Context, _ ExecSpec) (*Result, error) {
	zero := 0
	return &Result{ExitCode: &zero, Stdout: e.content}, nil
}

// scrubberFor builds a scrubber from a "platform variable name → value" map, taking only the
// ones in secretEnvNames.
//
// ⚠️ This is a **test-only helper**, not production logic. The production decision lives in
// enrich.go (it has to handle three sources: platform credentials, user custom envs marked
// Secret, and aliases). newScrubber itself only takes "values already judged", which is the key
// to fixing the "user Secret env not scrubbed" bug; see the newScrubber comment in scrub.go.
func scrubberFor(envs map[string]string) scrubber {
	var values []string
	for k, v := range envs {
		if secretEnvNames[k] {
			values = append(values, v)
		}
	}
	return newScrubber(values...)
}

// echoEnvExecer echoes the env it receives into stdout / stderr / Err.
//
// It simulates three real leaks: `env` (stdout), a script with `set -x` (stderr), and the
// backend echoing ExecSpec into an error (Err).
type echoEnvExecer struct {
	gotEnvs map[string]string
}

func (e *echoEnvExecer) Exec(_ context.Context, spec ExecSpec) (*Result, error) {
	e.gotEnvs = spec.Envs
	var b strings.Builder
	for k, v := range spec.Envs {
		b.WriteString(k + "=" + v + "\n")
	}
	dump := b.String()
	zero := 0
	return &Result{
		ExitCode: &zero,
		Stdout:   dump,
		Stderr:   "+ exported " + dump,
		Err:      "spec dump: " + dump,
	}, nil
}

// ⭐ Every kind of injected credential must be scrubbed. **Add a case here for each new one.**
//
// # This case came from a real bug
//
// When `AMA_TOKEN` (the platform-managed API Key used by the `ama` command in the backend) was
// first wired into injection, the check in `newScrubber` was still hard-coded to accept only
// another variable name, so **it flowed out of the backend unchanged**:
//
//	the agent runs env
//	  → AMA_TOKEN goes in plain text into tool_result → stored in RunEvent (**permanent**)
//	  → into the model context → pushed to the user's browser via SSE
//
// And that key can call AMA APIs as its creator. **All existing cases were green at the time**:
// the `scrubEnvs()` fixture had no AMA_TOKEN at all, so no case ever touched it.
//
// So the shape of this case is deliberate: it **iterates over secretEnvNames** instead of
// writing assertions one by one. That way, when a new credential is added to secretEnvNames,
// it is covered here automatically. And if someone adds a new credential to envs but forgets to
// add it to secretEnvNames, the check below that "every value in the fixture must be scrubbed"
// turns red right away.
func TestScrubberCoversEverySecretEnv(t *testing.T) {
	envs := allSecretEnvsFixture()
	s := scrubberFor(envs)

	for name := range secretEnvNames {
		val, ok := envs[name]
		if !ok {
			t.Fatalf("fixture 里缺 %s —— 加了新凭据就要在 allSecretEnvsFixture 里补上，"+
				"否则这条用例覆盖不到它（那正是 AMA_TOKEN 漏掉时的情形）", name)
		}
		got := s.text("prefix " + val + " suffix")
		if strings.Contains(got, val) {
			t.Errorf("❌ %s 的值原样流出了：%s\n"+
				"→ 往 scrub.go 的 secretEnvNames 里加上它", name, got)
		}
	}
}

// allSecretEnvsFixture gives each **secret** env a distinct, long enough value.
//
// ⚠️ The values must be distinct: if they all used the same string, a bug like "only one of
// them was scrubbed" could not be detected (scrubbing that one scrubs every occurrence). The
// length must pass secret.MinLen, otherwise the "too short to scrub" rule skips it, and the
// assertion would fail for a reason other than the one we want to test.
func allSecretEnvsFixture() map[string]string {
	return map[string]string{
		EnvAMAToken: "amak_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		// Feishu user token. The real shape is a `u-` prefix plus an opaque string.
		EnvLarkUserAccessToken: "u-lark-user-token-dddddddddddddddddddd",
	}
}

// The non-secret ones are still not scrubbed. This pairs with the case above, to guard against
// "scrubbing everything for safety".
//
// Scrubbing AMA_BASE_URL would hide which environment the backend hit (production or local)
// while debugging, and scrubbing AMA_WORKSPACE_ID would hide "which workspace this session
// belongs to". Both already appear in URLs, logs and pages; they are not secrets at all.
func TestScrubberKeepsAMANonSecrets(t *testing.T) {
	envs := allSecretEnvsFixture()
	envs[EnvAMABaseURL] = "https://ama.example.com"
	envs[EnvAMAWorkspaceID] = "ws_demo"

	s := scrubberFor(envs)
	got := s.text("AMA_BASE_URL=https://ama.example.com AMA_WORKSPACE_ID=ws_demo")
	if !strings.Contains(got, "https://ama.example.com") {
		t.Error("服务地址不是秘密，不该抹——抹了就看不出打的是哪个环境")
	}
	if !strings.Contains(got, "ws_demo") {
		t.Error("空间 id 不是秘密，不该抹")
	}
}

// ---------------------------------------------------------------------------
// Scrubbing the Feishu user token (via the SessionEnvSource source)
// ---------------------------------------------------------------------------

// fakeSessionEnvs is a SessionEnvSource stub that directly returns a set of envs and "which
// ones are secrets".
type fakeSessionEnvs struct {
	envs       map[string]string
	secretKeys []string
	aliases    []EnvAlias
}

func (f fakeSessionEnvs) SessionEnvs(
	_ context.Context, _ string,
) (map[string]string, []string, []EnvAlias, error) {
	return f.envs, f.secretKeys, f.aliases, nil
}

// theLarkToken is the Feishu user token used in these cases.
//
// It must be long enough (pass secret.MinLen) and differ from the other stub values. Otherwise a
// bug like "only one of them was scrubbed" would be hidden by the scrubbing of another value.
const theLarkToken = "u-lark-user-token-eeeeeeeeeeeeeeeeeeee"

// ⭐ The injected Feishu user token must not flow out of the backend unchanged.
//
// This has the same shape as TestEnrichingExecerScrubsInjectedCredential, using the Feishu token
// instead (also injected by SessionEnvSource, and scrubbed via the secretKeys it reports).
//
// # ⚠️ Why this one must exist separately
//
// The main guard TestScrubberCoversEverySecretEnv **iterates over secretEnvNames**: it only
// turns red when "registered but no value in the fixture". When **both tables forget it**, it
// stays all green, which is exactly how AMA_TOKEN was missed back then. This one works the other
// way: it starts from "this variable really got injected", so it catches "injected but not
// registered".
//
// Mutation check: empty the secretKeys of fakeSessionEnvs below (meaning the core side forgot
// to report it as a secret), and this case must turn red.
func TestEnrichingExecerScrubsLarkUserToken(t *testing.T) {
	echo := &echoEnvExecer{}
	ex := NewEnrichingExecer(echo, "sesn-lark-scrub", fakeSessionEnvs{
		envs: map[string]string{
			EnvLarkUserAccessToken: theLarkToken,
			// Give the app id too: real injection always comes in pairs (lark-cli errors if one is missing).
			EnvLarkAppID: "cli_example_d",
		},
		secretKeys: []string{EnvLarkUserAccessToken},
	}, quietLog())

	res, err := ex.Exec(context.Background(), ExecSpec{
		Path: "bash", Args: []string{"-lc", "env"},
	})
	if err != nil {
		t.Fatalf("执行失败：%v", err)
	}

	// First confirm the injection really happened. Otherwise this test is a false green (nothing
	// to scrub, so of course it scrubs clean).
	if got := echo.gotEnvs[EnvLarkUserAccessToken]; got != theLarkToken {
		t.Fatalf("执行端应当收到明文飞书 token（否则注入没生效），实际 %q", got)
	}
	// The app id must also really reach the backend: without it lark-cli reports "…is set but
	// LARKSUITE_CLI_APP_ID is missing", and the user cannot do anything about that message.
	if got := echo.gotEnvs[EnvLarkAppID]; got == "" {
		t.Error("执行端没收到 LARKSUITE_CLI_APP_ID —— 两个变量必须成对注入")
	}

	// But none of the three fields returned to the upper layer may contain plain text.
	for name, field := range map[string]string{
		"Stdout": res.Stdout, "Stderr": res.Stderr, "Err": res.Err,
	} {
		if strings.Contains(field, theLarkToken) {
			t.Errorf("❌ Exec 返回的 %s 里有明文飞书 token——它会进 tool_result → "+
				"RunEvent → 模型上下文 → 浏览器，且泄漏是永久的：%s\n"+
				"→ 往 scrub.go 的 secretEnvNames 里加上 EnvLarkUserAccessToken", name, field)
		}
	}
}

// The app id must **not** be scrubbed: it is a public identifier (cli_xxx appears in pages and
// logs), and scrubbing it would hide "which app this credential belongs to" while debugging.
//
// This is the reverse pair of the case above, to guard against "scrubbing everything for safety".
func TestScrubberKeepsLarkAppID(t *testing.T) {
	const appID = "cli_example_d"
	s := scrubberFor(map[string]string{
		EnvLarkUserAccessToken: theLarkToken,
		EnvLarkAppID:           appID,
	})
	got := s.text("LARKSUITE_CLI_APP_ID=" + appID)
	if !strings.Contains(got, appID) {
		t.Errorf("app id 不该被抹掉（它不是秘密，抹了排查时看不见是哪个应用）：%s", got)
	}
}

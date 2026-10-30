// Unit tests for the six tools.
//
// A fake Execer replaces the real executor: running for real needs an executor container, while
// what we check here is **whether the scripts we generate are right and whether output parsing is
// right**, not whether the executor is reachable.
// Real executor-side behavior is covered by e2e.
package shelltools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// fakeExec records the scripts it receives and returns preset replies.
type fakeExec struct {
	scripts []string
	// reply gives results in call order; after the last one is used, it keeps using it.
	reply []*Result
	err   error
	// lastTimeout is the ExecSpec.Timeout received by the last Exec.
	//
	// It is recorded because "which timeout layer was used" is itself something to check: background
	// start wrongly using execTimeout (120s) has no visible symptom, and only drags the whole
	// conversation round to death when the executor hangs.
	lastTimeout time.Duration
}

func (f *fakeExec) Exec(_ context.Context, spec ExecSpec) (*Result, error) {
	// Record the script really run (the last element of Args, i.e. the string after bash -lc).
	if n := len(spec.Args); n > 0 {
		f.scripts = append(f.scripts, spec.Args[n-1])
	}
	f.lastTimeout = spec.Timeout
	if f.err != nil {
		return nil, f.err
	}
	i := len(f.scripts) - 1
	if i >= len(f.reply) {
		i = len(f.reply) - 1
	}
	if i < 0 {
		return &Result{}, nil
	}
	return f.reply[i], nil
}

func ok(stdout string) *Result {
	code := 0
	return &Result{ExitCode: &code, Stdout: stdout}
}

func fail(code int, stderr string) *Result {
	return &Result{ExitCode: &code, Stderr: stderr}
}

func b64(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func raw(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// -- shQuote: injection protection is the foundation of this layer ------------

func TestShQuoteEscapesEverything(t *testing.T) {
	cases := map[string]string{
		"plain":       `'plain'`,
		"a b":         `'a b'`,
		"$(rm -rf /)": `'$(rm -rf /)'`,
		"`id`":        "'`id`'",
		"it's":        `'it'\''s'`,
		"a;b":         `'a;b'`,
	}
	for in, want := range cases {
		if got := shQuote(in); got != want {
			t.Errorf("shQuote(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

// -- resolvePath ---------------------------------------------------------

func TestResolvePath(t *testing.T) {
	// Empty workdir: relative paths resolve from root.
	if got, err := resolvePath("", "a/b.txt"); err != nil || got != "/a/b.txt" {
		t.Errorf("空 workdir: got=%q err=%v", got, err)
	}
	// With workdir: relative paths go under it.
	if got, err := resolvePath("/w", "a.txt"); err != nil || got != "/w/a.txt" {
		t.Errorf("相对路径: got=%q err=%v", got, err)
	}
	// Absolute paths are just cleaned.
	if got, err := resolvePath("/w", "/w/x/../y.txt"); err != nil || got != "/w/y.txt" {
		t.Errorf("绝对路径: got=%q err=%v", got, err)
	}
	// Absolute paths outside workdir **no longer error**. This is deliberate; see the resolvePath comment.
	//
	// This used to assert "going out of bounds must be blocked". That check never fired while workdir
	// was empty for a long time, and once workdir was pinned to /workspace it would lock the five file
	// tools inside home (bash does not go through resolvePath and can read any path anyway). Keeping
	// it would only make the tools disagree with each other.
	if got, err := resolvePath("/w", "/etc/passwd"); err != nil || got != "/etc/passwd" {
		t.Errorf("workdir 外的绝对路径应当原样返回: got=%q err=%v", got, err)
	}
	// `..` traversal likewise: after path.Clean folds it, it is a plain absolute path, no longer rejected.
	if got, err := resolvePath("/w", "../../etc/passwd"); err != nil || got != "/etc/passwd" {
		t.Errorf(".. 穿越应当折叠成绝对路径: got=%q err=%v", got, err)
	}
	// An empty path still errors.
	if _, err := resolvePath("/w", "  "); err == nil {
		t.Error("空路径应当报错")
	}
}

// defaultTestWorkdir is the default workdir of the Docker executor container. Here it is only a
// test value for "where skills land must match the agent's relative paths" (the real source is
// dockertools.DefaultWorkdir).
const defaultTestWorkdir = "/workspace"

// TestSkillsLandWhereAgentLooks pins the whole chain: **wherever injection writes, the agent can
// read from there**.
//
// The self_hosted side has the agent look for skills under `$workdir/skills` (it sends the
// relative path `./skills/<name>/SKILL.md`). So the key is not "the landing spot equals some
// literal", but that **the absolute path built by injection** and **the result of resolvePath on
// the agent's relative path** must be the same file.
//
// This failure has **zero errors**: the injection log still says "skill files injected into the
// container" (true, they were just written where the agent does not look), and the agent first
// fails one read, then searches the whole disk with `find /`. Slow, costly, and the error does not
// point to the landing spot at all. This is exactly the sequence users reported.
//
// ⚠️ **It must hold for every workdir, not just the default.** workdir can be overridden. When the
// landing spot used to be a hard-coded constant, the two only matched under the default workdir,
// and the bug came back as soon as it was overridden.
func TestSkillsLandWhereAgentLooks(t *testing.T) {
	// The default, an overridden value, a value with a trailing slash, and the empty string (the
	// fallback outside NewProvider) must all hold.
	for _, workdir := range []string{
		defaultTestWorkdir,
		"/tmp/custom-workdir",
		"/workspace/",
		"",
	} {
		dir, ok := safeSkillDir(SkillsRootFor(workdir), "ark-tls")
		if !ok {
			t.Fatalf("workdir=%q: ark-tls 应当是合法技能名", workdir)
		}
		// The relative path the agent sends must resolve to the file injection wrote.
		got, err := resolvePath(workdir, "./skills/ark-tls/SKILL.md")
		if err != nil {
			t.Fatalf("workdir=%q: 解析 agent 的相对路径失败: %v", workdir, err)
		}
		if got != dir+"/SKILL.md" {
			t.Errorf("workdir=%q: agent 的 ./skills/ark-tls/SKILL.md 解析成 %q，"+
				"但注入写到 %q —— 这正是用户报的那个错位",
				workdir, got, dir+"/SKILL.md")
		}
	}
}

// TestSkillsRootFollowsWorkdir pins that the landing spot is **derived from workdir**, not hard-coded.
//
// One version hard-coded it (`/workspace/skills`): fine under the default workdir, but as soon as
// containerWorkdir was overridden it no longer matched the agent's lookup path, with zero errors.
// This test watches "change the workdir, and the landing spot changes with it".
func TestSkillsRootFollowsWorkdir(t *testing.T) {
	if got, want := SkillsRootFor("/opt/x"), "/opt/x/skills"; got != want {
		t.Errorf("SkillsRootFor(/opt/x) = %q，期望 %q", got, want)
	}
	// The trailing slash must be cleaned, otherwise we get `//skills`.
	if got, want := SkillsRootFor("/opt/x/"), "/opt/x/skills"; got != want {
		t.Errorf("尾斜杠没规整：%q，期望 %q", got, want)
	}
	// The empty string counts as root, the same as resolvePath with an empty workdir.
	if got, want := SkillsRootFor(""), "/skills"; got != want {
		t.Errorf("空 workdir: %q，期望 %q", got, want)
	}
	// Under the default workdir it is the familiar path.
	if got, want := SkillsRootFor(defaultTestWorkdir), "/workspace/skills"; got != want {
		t.Errorf("默认 workdir 下落点 = %q，期望 %q", got, want)
	}
}

// -- read ----------------------------------------------------------------

func TestReadWholeFile(t *testing.T) {
	fe := &fakeExec{reply: []*Result{ok(b64("hello\nworld\n"))}}
	r := NewReadTool(fe, "/w").Execute(context.Background(), raw(map[string]any{"file_path": "a.txt"}))
	if r.IsError {
		t.Fatalf("不该失败: %+v", r.Content)
	}
	if got := r.Content[0].Text; got != "hello\nworld\n" {
		t.Errorf("整份读取应原样返回，got %q", got)
	}
	// The script must contain the path properly quoted.
	if !strings.Contains(fe.scripts[0], `'/w/a.txt'`) {
		t.Errorf("脚本里没有引号包好的路径: %s", fe.scripts[0])
	}
}

func TestReadWithOffsetAddsLineNumbers(t *testing.T) {
	fe := &fakeExec{reply: []*Result{ok(b64("l1\nl2\nl3\n"))}}
	r := NewReadTool(fe, "/w").Execute(context.Background(),
		raw(map[string]any{"file_path": "a.txt", "offset": 1, "limit": 1}))
	// Same as the SDK: %6d\t line numbers, and they are **the real line numbers in the file**
	// (starting at offset+1).
	want := "     2\tl2\n\n[truncated: showing lines 2-2 of 3]\n"
	if got := r.Content[0].Text; got != want {
		t.Errorf("带 offset 的输出 = %q，期望 %q", got, want)
	}
}

func TestReadViewRange(t *testing.T) {
	fe := &fakeExec{reply: []*Result{ok(b64("a\nb\nc\nd\n"))}}
	r := NewReadTool(fe, "/w").Execute(context.Background(),
		raw(map[string]any{"file_path": "a.txt", "view_range": []int{2, 3}}))
	// view_range adds no line numbers.
	if got := r.Content[0].Text; got != "b\nc" {
		t.Errorf("view_range 输出 = %q", got)
	}
}

func TestReadErrorsMapToSDKWording(t *testing.T) {
	for _, tc := range []struct{ stderr, want string }{
		{"__NOENT__", "no such file or directory"},
		{"__NOTFILE__:directory", "path is not a regular file"},
		{"__TOOBIG__:99999999", "file too large"},
	} {
		fe := &fakeExec{reply: []*Result{fail(2, tc.stderr)}}
		r := NewReadTool(fe, "/w").Execute(context.Background(), raw(map[string]any{"file_path": "a"}))
		if !r.IsError {
			t.Errorf("%s 应当是错误", tc.stderr)
			continue
		}
		if !strings.Contains(r.Content[0].Text, tc.want) {
			t.Errorf("%s → %q，期望含 %q", tc.stderr, r.Content[0].Text, tc.want)
		}
	}
}

func TestReadRejectsBinary(t *testing.T) {
	// Non-UTF-8 bytes.
	fe := &fakeExec{reply: []*Result{ok(base64.StdEncoding.EncodeToString([]byte{0xff, 0xfe}))}}
	r := NewReadTool(fe, "/w").Execute(context.Background(), raw(map[string]any{"file_path": "a"}))
	if !r.IsError || !strings.Contains(r.Content[0].Text, "binary file") {
		t.Errorf("二进制文件应被拒: %+v", r.Content)
	}
}

// -- write ---------------------------------------------------------------

func TestWriteUsesBase64AndMkdir(t *testing.T) {
	fe := &fakeExec{reply: []*Result{ok("")}}
	content := "第一行\n$(whoami) `id` \"'\n"
	r := NewWriteTool(fe, "/w").Execute(context.Background(),
		raw(map[string]any{"file_path": "d/a.txt", "content": content}))
	if r.IsError {
		t.Fatalf("不该失败: %+v", r.Content)
	}
	// Output format matches the SDK, and uses **the path from the input**, not the absolute path.
	if got, want := r.Content[0].Text, "wrote 39 bytes to d/a.txt"; !strings.HasPrefix(got, "wrote ") || !strings.HasSuffix(got, " to d/a.txt") {
		t.Errorf("write 输出 = %q，期望形如 %q", got, want)
	}
	s := fe.scripts[0]
	// Content must appear as base64, never as raw text: the $(whoami) in the raw text would be run
	// by the shell.
	if !strings.Contains(s, b64(content)) {
		t.Errorf("脚本里没有 base64 内容: %s", s)
	}
	if strings.Contains(s, "$(whoami)") {
		t.Errorf("脚本里出现了未编码的原文，存在注入风险: %s", s)
	}
	if !strings.Contains(s, "mkdir -p") {
		t.Errorf("脚本应当先建父目录: %s", s)
	}
}

func TestWriteRejectsOversize(t *testing.T) {
	fe := &fakeExec{reply: []*Result{ok("")}}
	big := strings.Repeat("x", maxWriteBytes+1)
	r := NewWriteTool(fe, "/w").Execute(context.Background(),
		raw(map[string]any{"file_path": "a", "content": big}))
	if !r.IsError || !strings.Contains(r.Content[0].Text, "content too large") {
		t.Errorf("超大内容应被拒: %+v", r.Content)
	}
	if len(fe.scripts) != 0 {
		t.Error("超限时不该发起执行端调用")
	}
}

// -- edit ----------------------------------------------------------------

func TestEditReplacesOnce(t *testing.T) {
	fe := &fakeExec{reply: []*Result{ok(b64("aaa OLD bbb\n")), ok("")}}
	r := NewEditTool(fe, "/w").Execute(context.Background(), raw(map[string]any{
		"file_path": "a.txt", "old_string": "OLD", "new_string": "NEW",
	}))
	if r.IsError {
		t.Fatalf("不该失败: %+v", r.Content)
	}
	// The second script is the write-back; its content should be the base64 of the replaced text.
	if !strings.Contains(fe.scripts[1], b64("aaa NEW bbb\n")) {
		t.Errorf("写回内容不对: %s", fe.scripts[1])
	}
}

func TestEditRefusesAmbiguous(t *testing.T) {
	fe := &fakeExec{reply: []*Result{ok(b64("X\nX\n"))}}
	r := NewEditTool(fe, "/w").Execute(context.Background(), raw(map[string]any{
		"file_path": "a.txt", "old_string": "X", "new_string": "Y",
	}))
	// Same as the SDK: error if not unique, so the agent gives a longer context instead of us guessing.
	if !r.IsError || !strings.Contains(r.Content[0].Text, "appears 2 times") {
		t.Errorf("多处匹配应当报错: %+v", r.Content)
	}
	if len(fe.scripts) != 1 {
		t.Error("报错时不该写回")
	}
}

func TestEditReplaceAll(t *testing.T) {
	fe := &fakeExec{reply: []*Result{ok(b64("X\nX\n")), ok("")}}
	r := NewEditTool(fe, "/w").Execute(context.Background(), raw(map[string]any{
		"file_path": "a.txt", "old_string": "X", "new_string": "Y", "replace_all": true,
	}))
	if r.IsError {
		t.Fatalf("不该失败: %+v", r.Content)
	}
	if !strings.Contains(fe.scripts[1], b64("Y\nY\n")) {
		t.Errorf("replace_all 没全换: %s", fe.scripts[1])
	}
	if !strings.Contains(r.Content[0].Text, "2 occurrences") {
		t.Errorf("应当报告替换了 2 处: %q", r.Content[0].Text)
	}
}

func TestEditNotFound(t *testing.T) {
	fe := &fakeExec{reply: []*Result{ok(b64("hello\n"))}}
	r := NewEditTool(fe, "/w").Execute(context.Background(), raw(map[string]any{
		"file_path": "a.txt", "old_string": "ZZZ", "new_string": "Y",
	}))
	if !r.IsError || !strings.Contains(r.Content[0].Text, "not found") {
		t.Errorf("找不到应当报错: %+v", r.Content)
	}
}

func TestEditRejectsEmptyOldString(t *testing.T) {
	fe := &fakeExec{}
	r := NewEditTool(fe, "/w").Execute(context.Background(), raw(map[string]any{
		"file_path": "a.txt", "old_string": "", "new_string": "Y",
	}))
	if !r.IsError {
		t.Error("空 old_string 应当报错")
	}
	if len(fe.scripts) != 0 {
		t.Error("参数不合法时不该发起执行端调用")
	}
}

// -- glob ----------------------------------------------------------------

func TestGlobSortsByMtimeDesc(t *testing.T) {
	// Output of find -printf '%T@ %p\n': older first; the expected result has newer first.
	fe := &fakeExec{reply: []*Result{ok(
		"100.5 ./old.sql\n300.5 ./new.sql\n200.5 ./mid.sql\n")}}
	r := NewGlobTool(fe, "/w").Execute(context.Background(),
		raw(map[string]any{"pattern": "*.sql"}))
	if r.IsError {
		t.Fatalf("不该失败: %+v", r.Content)
	}
	want := "new.sql\nmid.sql\nold.sql\n"
	if got := r.Content[0].Text; got != want {
		t.Errorf("glob 输出 = %q，期望 %q（mtime 倒序、去掉 ./ 前缀）", got, want)
	}
}

func TestGlobRequiresPattern(t *testing.T) {
	fe := &fakeExec{}
	r := NewGlobTool(fe, "/w").Execute(context.Background(), raw(map[string]any{"pattern": " "}))
	if !r.IsError || !strings.Contains(r.Content[0].Text, "pattern must not be empty") {
		t.Errorf("空 pattern 应当报错: %+v", r.Content)
	}
}

func TestGlobQuotesPattern(t *testing.T) {
	fe := &fakeExec{reply: []*Result{ok("")}}
	NewGlobTool(fe, "/w").Execute(context.Background(),
		raw(map[string]any{"pattern": "*.go; rm -rf /"}))
	if strings.Contains(fe.scripts[0], "; rm -rf /'") == false &&
		!strings.Contains(fe.scripts[0], `'*.go; rm -rf /'`) {
		t.Errorf("pattern 必须被引号包住: %s", fe.scripts[0])
	}
}

// -- grep ----------------------------------------------------------------

func TestGrepContentMode(t *testing.T) {
	fe := &fakeExec{reply: []*Result{ok(
		"./a/b.go:12:func main() {\n./c.go:3:// TODO: x:y\n")}}
	r := NewGrepTool(fe, "/w").Execute(context.Background(),
		raw(map[string]any{"pattern": "func"}))
	if r.IsError {
		t.Fatalf("不该失败: %+v", r.Content)
	}
	want := "a/b.go:12:func main() {\nc.go:3:// TODO: x:y\n"
	if got := r.Content[0].Text; got != want {
		t.Errorf("grep content = %q，期望 %q（正文里的冒号不能被截断）", got, want)
	}
}

func TestGrepFilesWithMatches(t *testing.T) {
	fe := &fakeExec{reply: []*Result{ok(
		"./z.go:1:x\n./a.go:2:x\n./a.go:5:x\n")}}
	r := NewGrepTool(fe, "/w").Execute(context.Background(),
		raw(map[string]any{"pattern": "x", "output_mode": "files_with_matches"}))
	// Dedup + sort by path ascending.
	if got, want := r.Content[0].Text, "a.go\nz.go\n"; got != want {
		t.Errorf("files_with_matches = %q，期望 %q", got, want)
	}
}

func TestGrepCount(t *testing.T) {
	fe := &fakeExec{reply: []*Result{ok("./a:1:x\n./a:2:x\n./b:1:x\n")}}
	r := NewGrepTool(fe, "/w").Execute(context.Background(),
		raw(map[string]any{"pattern": "x", "output_mode": "count"}))
	if got := r.Content[0].Text; got != "3\n" {
		t.Errorf("count = %q，期望 \"3\\n\"", got)
	}
}

func TestGrepNoMatchIsNotError(t *testing.T) {
	// grep exits with 1 when nothing matches: that is not an error.
	fe := &fakeExec{reply: []*Result{fail(1, "")}}
	r := NewGrepTool(fe, "/w").Execute(context.Background(), raw(map[string]any{"pattern": "zzz"}))
	if r.IsError {
		t.Errorf("没匹配到不该算错误: %+v", r.Content)
	}
	if r.Content[0].Text != "" {
		t.Errorf("没匹配到应当返回空，got %q", r.Content[0].Text)
	}
}

func TestGrepCaseInsensitiveAndFilter(t *testing.T) {
	fe := &fakeExec{reply: []*Result{ok("")}}
	NewGrepTool(fe, "/w").Execute(context.Background(), raw(map[string]any{
		"pattern": "x", "case_insensitive": true, "glob_filter": "*.go",
	}))
	s := fe.scripts[0]
	if !strings.Contains(s, "-rnEi") {
		t.Errorf("case_insensitive 应当加 i 标志: %s", s)
	}
	if !strings.Contains(s, `--include='*.go'`) {
		t.Errorf("glob_filter 应当变成 --include: %s", s)
	}
}

// -- Execution failures and command failures must be told apart ---------------

func TestExecTransportErrorIsDistinct(t *testing.T) {
	fe := &fakeExec{err: context.DeadlineExceeded}
	r := NewReadTool(fe, "/w").Execute(context.Background(), raw(map[string]any{"file_path": "a"}))
	if !r.IsError || !strings.Contains(r.Content[0].Text, "在容器里执行失败") {
		t.Errorf("传输失败应当有别于命令失败: %+v", r.Content)
	}
}

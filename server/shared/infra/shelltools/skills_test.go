// Tests for injecting skill files into the container.
//
// # What is tested here is "what the command looks like", not "the file really landed on disk"
//
// The latter needs a real execution container. This file uses a recording execer to capture the
// scripts sent out, and asserts three things: batching works, paths and payloads are both quoted,
// and recreating the container injects again. These three are the parts that can be tested
// offline, and also exactly the parts most likely to be written wrong.
package shelltools

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingExecer records every script sent out.
type recordingExecer struct {
	scripts []string
	// timeouts records the ExecSpec.Timeout of each command.
	//
	// **It must be recorded**: that field is the limit on the backend side, and only widening the
	// context cannot control it (see the runShellTimeout comment). Without recording it, "what the
	// timeout really is" cannot be observed, and setting it wrong shows up as "large skills
	// randomly fail to install on slow links", with the error coming from the backend.
	timeouts []time.Duration
	// When fail is true, every command returns a non-zero exit code.
	fail bool
}

func (e *recordingExecer) Exec(_ context.Context, spec ExecSpec) (*Result, error) {
	// Same shape as runShell: bash -lc <script>
	if len(spec.Args) == 2 {
		e.scripts = append(e.scripts, spec.Args[1])
		e.timeouts = append(e.timeouts, spec.Timeout)
	}
	code := 0
	if e.fail {
		code = 1
	}
	return &Result{ExitCode: &code, Stderr: "boom"}, nil
}

func TestInjectSkillsWritesEachFile(t *testing.T) {
	sh := &recordingExecer{}
	bundles := []SkillBundle{{
		Name: "ticket-triage",
		Files: []SkillFile{
			{Path: "scripts/init.sh", Content: []byte("echo one")},
			{Path: "references/rules.md", Content: []byte("# 规则")},
		},
	}}

	if err := InjectSkills(context.Background(), sh, "", bundles, quietLog()); err != nil {
		t.Fatalf("注入应当成功：%v", err)
	}

	// The two small files should be merged into **one** command: the SDK handles tool calls
	// serially, and one command per file would stack N round trips on top of the 20 seconds of
	// container creation.
	if len(sh.scripts) != 1 {
		t.Fatalf("两个小文件应当合并成 1 条命令，实际 %d 条", len(sh.scripts))
	}
	script := sh.scripts[0]

	// Paths must be **absolute**: injection goes through `bash -lc`, and Cwd and the tool-side
	// workdir are two entry points. Only absolute paths give "where does the file actually land"
	// one answer.
	// The workdir passed here is an empty string, so the location is SkillsRootFor(""). It is
	// computed with the function instead of a hard-coded literal, so this does not quietly stop
	// matching when the default changes.
	for _, want := range []string{
		SkillsRootFor("") + "/ticket-triage/scripts/init.sh",
		SkillsRootFor("") + "/ticket-triage/references/rules.md",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("脚本里应当出现绝对路径 %q，实际脚本：%s", want, script)
		}
	}
	// Content must appear as base64, not as raw text: raw text gets rewritten by the shell and HTTP
	// layers (trailing newlines eaten, CRLF normalized, $(...) executed).
	for _, content := range []string{"echo one", "# 规则"} {
		enc := base64.StdEncoding.EncodeToString([]byte(content))
		if !strings.Contains(script, enc) {
			t.Errorf("内容 %q 应当以 base64(%s) 出现", content, enc)
		}
	}
	// set -e: any failing line in the batch must stop right away, otherwise the failure is swallowed
	// by the exit code of the last line.
	if !strings.HasPrefix(script, "set -e\n") {
		t.Errorf("脚本应当以 set -e 开头，实际：%s", script)
	}
	// mkdir -p: the redirect fails if the parent directory does not exist.
	if !strings.Contains(script, "mkdir -p") {
		t.Errorf("脚本应当建父目录，实际：%s", script)
	}
}

// TestInjectSkillsBatchesBySize pins "split into batches once over the batch budget".
//
// Without batching, the base64 payload would exceed maxWriteBytes, and the error would be
// "argument list too long", which does not point to skill injection at all.
func TestInjectSkillsBatchesBySize(t *testing.T) {
	sh := &recordingExecer{}
	// Each file is about 200 KiB after base64; two of them exceed maxInjectBatchBytes (384 KiB).
	chunk := strings.Repeat("x", 150<<10)
	bundles := []SkillBundle{{
		Name: "big-skill",
		Files: []SkillFile{
			{Path: "a.bin", Content: []byte(chunk)},
			{Path: "b.bin", Content: []byte(chunk)},
			{Path: "c.bin", Content: []byte(chunk)},
		},
	}}

	if err := InjectSkills(context.Background(), sh, "", bundles, quietLog()); err != nil {
		t.Fatalf("注入应当成功：%v", err)
	}
	if len(sh.scripts) < 2 {
		t.Fatalf("超过一批预算应当分批，实际只发了 %d 条命令", len(sh.scripts))
	}
	// No batch may exceed maxWriteBytes, otherwise the split was for nothing.
	for i, s := range sh.scripts {
		if len(s) > maxWriteBytes {
			t.Errorf("第 %d 批长度 %d 超过 maxWriteBytes(%d)", i, len(s), maxWriteBytes)
		}
	}
	// None of the three files may be lost.
	for _, name := range []string{"a.bin", "b.bin", "c.bin"} {
		found := false
		for _, s := range sh.scripts {
			if strings.Contains(s, name) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("分批后 %s 丢了", name)
		}
	}
}

// TestInjectSkillsRejectsUnsafePaths is a defense-in-depth case.
//
// core/skillpkg.go already does the full check, but injection is the step that "turns untrusted
// data into real files in the container", so it must be able to judge safety on its own.
// Otherwise, when a future caller skips the parser, the symptom is an extra file under /etc in the
// container, with no error at all.
//
// Mutation check: make safeSkillPath always return true → all subcases of this case turn red.
func TestInjectSkillsRejectsUnsafePaths(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"逃出目录", "../../etc/cron.d/evil"},
		{"绝对路径", "/etc/passwd"},
		{"反斜杠", `..\evil`},
		{"换行注入", "a\necho pwned"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sh := &recordingExecer{}
			bundles := []SkillBundle{{
				Name:  "ok-skill",
				Files: []SkillFile{{Path: tc.path, Content: []byte("evil")}},
			}}
			err := InjectSkills(context.Background(), sh, "", bundles, quietLog())
			if err == nil {
				t.Error("不安全的路径应当被拒（返回错误让上层记日志）")
			}
			// Key assertion: **not a single command should be sent**.
			if len(sh.scripts) != 0 {
				t.Errorf("不安全路径不该产生任何命令，实际发了 %d 条：%v", len(sh.scripts), sh.scripts)
			}
		})
	}
}

// TestInjectSkillsRejectsUnsafeSkillName is the same as above, but for the skill name (which
// becomes a directory name).
func TestInjectSkillsRejectsUnsafeSkillName(t *testing.T) {
	for _, name := range []string{"../evil", "Has Upper", "semi;colon", ""} {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			sh := &recordingExecer{}
			bundles := []SkillBundle{{
				Name:  name,
				Files: []SkillFile{{Path: "a.sh", Content: []byte("x")}},
			}}
			if err := InjectSkills(context.Background(), sh, "", bundles, quietLog()); err == nil {
				t.Error("不安全的技能名应当被拒")
			}
			if len(sh.scripts) != 0 {
				t.Errorf("不安全技能名不该产生任何命令，实际 %d 条", len(sh.scripts))
			}
		})
	}
}

func TestInjectSkillsSkipsEmpty(t *testing.T) {
	sh := &recordingExecer{}
	// No skills, and skills without attachments, should both send no command at all. Sending one
	// would make every session without skills pay a container round trip for nothing.
	if err := InjectSkills(context.Background(), sh, "", nil, quietLog()); err != nil {
		t.Errorf("空输入不该报错：%v", err)
	}
	if err := InjectSkills(context.Background(), sh, "",
		[]SkillBundle{{Name: "no-files"}}, quietLog()); err != nil {
		t.Errorf("没有附件不该报错：%v", err)
	}
	if len(sh.scripts) != 0 {
		t.Errorf("空输入不该发命令，实际 %d 条", len(sh.scripts))
	}
}

// maxArgStrLen is the Linux length limit for a **single** argv element (32 pages).
//
// **This is the most important number in this file**, and the real constraint behind that
// production incident. Do not use `ARG_MAX` (2 MiB) as the test: that is the **total** of all
// arguments plus environment variables and does not limit a single element. Reasoning from it
// leads to "a 148 KiB script is fine", while execve actually returns E2BIG.
const maxArgStrLen = 128 << 10

// TestInjectSkillsKeepsEachCommandUnderKernelLimit reproduces and pins down the whole-batch loss
// seen in production.
//
// # What happened in production
//
// An agent bound 10 skills with **no attachments** (3 built-in + 7 imported from a code repo),
// with bodies totaling 108 KiB, 144 KiB after base64. The old maxInjectBatchBytes was 384 KiB
// (derived from maxWriteBytes, the wrong reference), so all 10 SKILL.md files were collected into
// **one** 148 KiB command, and the backend returned 400:
//
//	fork/exec /bin/bash: argument list too long
//
// And `set -e` made this batch fail **as a whole**: **not one of the 10 skills landed on disk**,
// and the agent reading /mnt/skills/some-cli/SKILL.md got exit=2.
//
// # Why "several medium files" instead of one large file
//
// The shape of this bug is going over the limit **cumulatively**, not one file over the limit. The
// existing TestInjectSkillsBatchesBySize uses large 150 KiB files, which also get batched under
// the old code (3 files × 200 KiB > 384 KiB), so it **cannot catch this bug**. In the real case,
// each SKILL.md was only a dozen KB or so, small on its own.
func TestInjectSkillsKeepsEachCommandUnderKernelLimit(t *testing.T) {
	// 10 skills, each body 12 KiB. Each alone is far below any limit; only the total goes over.
	names := []string{
		"some-cli", "lark-cli", "ama", "skill-a", "skill-b",
		"skill-c", "skill-d", "skill-e", "skill-f", "skill-g",
	}
	bundles := make([]SkillBundle, 0, len(names))
	for i, n := range names {
		// Every content is different: if they all used the same content, "some skill is lost" could
		// be masked by another skill's payload (existence is asserted by content below).
		body := fmt.Sprintf("# %s\n%s", n, strings.Repeat(fmt.Sprintf("正文%d ", i), 2<<10))
		bundles = append(bundles, SkillBundle{
			Name:  n,
			Files: []SkillFile{{Path: "SKILL.md", Content: []byte(body)}},
		})
	}

	sh := &recordingExecer{}
	if err := InjectSkills(context.Background(), sh, "", bundles, quietLog()); err != nil {
		t.Fatalf("注入应当成功：%v", err)
	}

	// ① Core assertion: **every** command must be under the kernel's per-argument limit.
	// The old code sent only 1 command of about 148 KiB here, directly over the limit.
	for i, s := range sh.scripts {
		if len(s) > maxArgStrLen {
			t.Errorf("第 %d 条命令 %d 字节，超过 MAX_ARG_STRLEN(%d)——"+
				"线上表现是 执行端回 400 argument list too long，"+
				"且 set -e 让整批失败、所有技能一个都不落盘",
				i, len(s), maxArgStrLen)
		}
	}

	// ② No skill may be lost. Batching only splits the commands; all content must arrive.
	joined := strings.Join(sh.scripts, "\n")
	for i, n := range names {
		if !strings.Contains(joined, SkillsRootFor("")+"/"+n+"/SKILL.md") {
			t.Errorf("技能 %s 的 SKILL.md 没被写出——容器里 %s/%s/ 会不存在，"+
				"模型据此认为没有这个技能", n, SkillsRootFor(""), n)
		}
		// The payload must be there too (a path that appears with lost content is just as broken).
		enc := base64.StdEncoding.EncodeToString(
			[]byte(fmt.Sprintf("# %s\n%s", n, strings.Repeat(fmt.Sprintf("正文%d ", i), 2<<10))))
		if !strings.Contains(joined, enc[:64]) {
			t.Errorf("技能 %s 的正文载荷没出现在任何一条命令里", n)
		}
	}
}

// TestInjectSkillsChunksSingleOversizeFile pins the path where "a single file is larger than one
// batch budget".
//
// The upload side allows 256 KiB per file (maxSkillFileBytes in core/skillpkg.go), about 341 KiB
// after base64, much larger than MAX_ARG_STRLEN. A comment in the old code said "a single file
// over the budget is safe in a batch of its own". **That does not hold**: a batch is one command,
// so a batch of its own is still one 341 KiB command, and still gets E2BIG. So large files must be
// written in chunks.
func TestInjectSkillsChunksSingleOversizeFile(t *testing.T) {
	// 250 KiB of raw text → about 333 KiB after base64, far over one batch budget.
	content := []byte(strings.Repeat("A", 250<<10))
	sh := &recordingExecer{}
	bundles := []SkillBundle{{
		Name:  "big-skill",
		Files: []SkillFile{{Path: "references/huge.md", Content: content}},
	}}

	if err := InjectSkills(context.Background(), sh, "", bundles, quietLog()); err != nil {
		t.Fatalf("注入应当成功：%v", err)
	}

	// Every command must be under the kernel limit.
	for i, s := range sh.scripts {
		if len(s) > maxArgStrLen {
			t.Errorf("第 %d 条命令 %d 字节，超过 MAX_ARG_STRLEN(%d)", i, len(s), maxArgStrLen)
		}
	}

	// The first chunk must use `>` (truncate), later ones must use `>>` (append).
	//
	// **The wrong order is silent content corruption**: with `>` everywhere only the last chunk
	// remains at the end, the file exists with the wrong size and no error; with `>>` everywhere,
	// after the container is recreated it appends on top of the old content.
	if len(sh.scripts) < 2 {
		t.Fatalf("333 KiB 的载荷必须切成多条命令，实际 %d 条", len(sh.scripts))
	}
	if !strings.Contains(sh.scripts[0], `base64 -d > "$p"`) {
		t.Errorf("第一片必须用 > 截断，否则容器重建时会在旧内容上追加：%.120s", sh.scripts[0])
	}
	for i, s := range sh.scripts[1:] {
		if !strings.Contains(s, `base64 -d >> "$p"`) {
			t.Errorf("第 %d 片必须用 >> 追加，用 > 会把前面写的覆盖掉、只剩最后一片：%.120s",
				i+1, s)
		}
	}

	// Join the base64 of all chunks back together; it must equal the original byte for byte.
	//
	// **This is the only test for cut-point alignment**: base64 decodes 3 bytes from every 4
	// characters, and cutting at a point that is not a multiple of 4 makes chunks decode into
	// shifted bytes, which neither raises an error nor changes whether the file exists.
	//
	// ⚠️ One script may hold **several** write commands (batching groups the last few chunks
	// together), so we must extract command by command, not take one payload per script. That would
	// miss later chunks, showing up as "the joined result is shorter than the original" (this was
	// hit once while writing this case).
	var b64 strings.Builder
	for _, s := range sh.scripts {
		for _, p := range payloads(s) {
			b64.WriteString(p)
		}
	}
	got, err := base64.StdEncoding.DecodeString(b64.String())
	if err != nil {
		t.Fatalf("各片拼回来必须是合法 base64（切点没对齐 4 的倍数就会这样）：%v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("拼回来的内容与原文不一致：原文 %d 字节，实际 %d 字节", len(content), len(got))
	}
}

// payloads extracts **all** base64 payloads from an injection script (the ones wrapped in single
// quotes).
func payloads(script string) []string {
	const marker = "printf '%s' '"
	var out []string
	for rest := script; ; {
		i := strings.Index(rest, marker)
		if i < 0 {
			return out
		}
		rest = rest[i+len(marker):]
		j := strings.Index(rest, "'")
		if j < 0 {
			return out
		}
		out = append(out, rest[:j])
		rest = rest[j:]
	}
}

func TestInjectSkillsReportsExecFailure(t *testing.T) {
	sh := &recordingExecer{fail: true}
	bundles := []SkillBundle{{
		Name:  "ticket-triage",
		Files: []SkillFile{{Path: "a.sh", Content: []byte("x")}},
	}}
	err := InjectSkills(context.Background(), sh, "", bundles, quietLog())
	if err == nil {
		t.Fatal("容器里执行失败必须报上来（上层据此记 Warn）")
	}
	if !strings.Contains(err.Error(), "写技能文件失败") {
		t.Errorf("报错应当指出是写技能文件失败：%v", err)
	}
}

// ---------------------------------------------------------------------------
// Direct archive download (the container curls the presigned URL itself)
// ---------------------------------------------------------------------------

// TestInjectSkillsPullsArchiveInsteadOfBase64 pins "skills with ArchiveURL are downloaded, not
// sent via base64".
//
// The base64 path is limited by MAX_ARG_STRLEN (128 KiB), and packages of tens of MB cannot fit.
// That is exactly what this round works around. So with a package we **must** see the download
// command, and **must not** see that skill's base64 payload.
func TestInjectSkillsPullsArchiveInsteadOfBase64(t *testing.T) {
	const url = "https://objects.example.com/ama/skill-archives/2026/09/abc.zip?signature=xyz"
	bundles := []SkillBundle{{
		Name: "big-skill",
		// Fallback SKILL.md: on a successful download it should **not** be written (the one in the
		// package is the real one).
		Files:        []SkillFile{{Path: "SKILL.md", Content: []byte("兜底正文")}},
		ArchiveURL:   url,
		ArchiveBytes: 4096,
	}}

	sh := &recordingExecer{}
	if err := InjectSkills(context.Background(), sh, "/workspace", bundles, quietLog()); err != nil {
		t.Fatalf("注入应当成功：%v", err)
	}

	joined := strings.Join(sh.scripts, "\n")
	if !strings.Contains(joined, "curl") || !strings.Contains(joined, url) {
		t.Error("带 ArchiveURL 的技能应当走 curl 下载")
	}
	if !strings.Contains(joined, "unzip") {
		t.Error("下完要解压")
	}
	// The fallback body must not be written via base64: the one in the package is the real one,
	// and writing it would overwrite that.
	if strings.Contains(joined, base64.StdEncoding.EncodeToString([]byte("兜底正文"))) {
		t.Error("下载成功时不该再逐文件写兜底 SKILL.md——那会覆盖包里真的那份")
	}
}

// TestInjectSkillsArchiveVerifiesByteCount pins "compare the byte count after download".
//
// ⚠️ When the connection drops, curl leaves an **incomplete but existing** file and exits 0 (as
// long as the server already replied 200). Without the check unzip may extract only half the
// files, and **a broken skill is worse than a missing one**: the agent reads a skill whose
// directory looks complete but is missing half its content, with zero errors.
func TestInjectSkillsArchiveVerifiesByteCount(t *testing.T) {
	bundles := []SkillBundle{{
		Name:         "big-skill",
		ArchiveURL:   "https://example.invalid/x.zip",
		ArchiveBytes: 123456,
	}}
	sh := &recordingExecer{}
	_ = InjectSkills(context.Background(), sh, "/workspace", bundles, quietLog())

	joined := strings.Join(sh.scripts, "\n")
	if !strings.Contains(joined, "123456") {
		t.Error("脚本里要带期望字节数，否则截断的下载会被当成成功")
	}
	if !strings.Contains(joined, "wc -c") {
		t.Error("要真的去数字节，不能只把数字写进注释")
	}
	// curl must have -f: without it, the 403 error page is written into the file with exit 0, then
	// unzip says "not a zip file". That message points to a broken package, not to an expired
	// signature.
	if !strings.Contains(joined, "curl -fsS") {
		t.Error("curl 要带 -f，否则 HTTP 错误会被当成下载成功")
	}
}

// TestInjectSkillsArchiveClearsDirBeforeUnzip pins "clear the directory before unpacking".
//
// The container may be restored from a snapshot and may contain leftovers from a previous skill
// version. Without clearing, old and new files mix, and the agent reads a skill where "some files
// are the old version". That mismatch raises no error.
func TestInjectSkillsArchiveClearsDirBeforeUnzip(t *testing.T) {
	bundles := []SkillBundle{{
		Name:         "big-skill",
		ArchiveURL:   "https://example.invalid/x.zip",
		ArchiveBytes: 10,
	}}
	sh := &recordingExecer{}
	_ = InjectSkills(context.Background(), sh, "/workspace", bundles, quietLog())

	joined := strings.Join(sh.scripts, "\n")
	if !strings.Contains(joined, "rm -rf") {
		t.Error("解压前要清掉技能目录，否则会与上一版残留混在一起")
	}
	// Unpack into the **parent directory**: the zip top level is <skill name>/, and unpacking into
	// the skill directory would give skills/big-skill/big-skill/SKILL.md.
	if !strings.Contains(joined, "unzip -qo \"$tmp\" -d '/workspace/skills'") {
		t.Errorf("应当解压到父目录（zip 顶层已经是技能名那层），实际脚本：\n%s", joined)
	}
}

// TestInjectSkillsFallsBackToFilesWhenArchiveFails pins "the fallback SKILL.md is still written
// when the download fails".
//
// This guards the invariant "a bound skill always has a visible directory": no directory = the
// model thinks the skill does not exist (see the skillsRootDir part and the
// `No such file or directory` a user reported). A failed download is bad enough; the directory must
// not be missing too.
func TestInjectSkillsFallsBackToFilesWhenArchiveFails(t *testing.T) {
	bundles := []SkillBundle{{
		Name:         "big-skill",
		Files:        []SkillFile{{Path: "SKILL.md", Content: []byte("兜底正文")}},
		ArchiveURL:   "https://example.invalid/x.zip",
		ArchiveBytes: 10,
	}}
	// fail=true makes every command return a non-zero exit code, simulating a failed download.
	sh := &recordingExecer{fail: true}
	err := InjectSkills(context.Background(), sh, "/workspace", bundles, quietLog())
	if err == nil {
		t.Error("下载失败要往上报（上层记 Warn），否则查不出「技能为什么不全」")
	}

	joined := strings.Join(sh.scripts, "\n")
	if !strings.Contains(joined, base64.StdEncoding.EncodeToString([]byte("兜底正文"))) {
		t.Error("下载失败时必须回落到写兜底 SKILL.md——" +
			"目录不存在的话模型会认为压根没这个技能")
	}
}

// TestInjectSkillsArchiveFailureDoesNotBlockOtherSkills pins "one package failing to download does
// not affect other skills".
//
// That is exactly why it is one command per skill: in a single batch, `set -e` would let one
// failure take down the whole batch.
func TestInjectSkillsArchiveFailureDoesNotBlockOtherSkills(t *testing.T) {
	bundles := []SkillBundle{
		{Name: "with-archive", ArchiveURL: "https://example.invalid/x.zip", ArchiveBytes: 10},
		{Name: "plain-skill", Files: []SkillFile{{Path: "SKILL.md", Content: []byte("正常技能")}}},
	}
	sh := &failFirstExecer{}
	_ = InjectSkills(context.Background(), sh, "/workspace", bundles, quietLog())

	joined := strings.Join(sh.scripts, "\n")
	if !strings.Contains(joined, base64.StdEncoding.EncodeToString([]byte("正常技能"))) {
		t.Error("一个技能的包下载失败，不该妨碍另一个技能落盘")
	}
}

// failFirstExecer makes only the first command fail; the rest succeed.
type failFirstExecer struct {
	recordingExecer
	n int
}

func (e *failFirstExecer) Exec(ctx context.Context, spec ExecSpec) (*Result, error) {
	if len(spec.Args) == 2 {
		e.scripts = append(e.scripts, spec.Args[1])
	}
	e.n++
	code := 0
	if e.n == 1 {
		code = 1
	}
	return &Result{ExitCode: &code, Stderr: "boom"}, nil
}

// The ExecSpec.Timeout of the skill package download command must be archiveDownloadTimeout, not
// the default toolExecTimeout (60 seconds).
//
// ⚠️ **Only widening the context does not help**: ExecSpec.Timeout is the limit on the backend
// side. We hit this: the comment said "5 minutes", but it was actually killed at 60 seconds,
// showing up as "large skills randomly fail to install on slow links", with the error coming from
// the backend and not pointing at all to "we set the timeout to 60 seconds ourselves".
func TestArchiveDownloadUsesItsOwnExecTimeout(t *testing.T) {
	bundles := []SkillBundle{{
		Name:         "big-skill",
		ArchiveURL:   "https://example.invalid/x.zip",
		ArchiveBytes: 100 << 20,
	}}
	sh := &recordingExecer{}
	_ = InjectSkills(context.Background(), sh, "/workspace", bundles, quietLog())

	if len(sh.timeouts) == 0 {
		t.Fatal("一条命令都没发")
	}
	if got := sh.timeouts[0]; got != archiveDownloadTimeout {
		t.Errorf("下载命令的 ExecSpec.Timeout = %s，要 %s——"+
			"用默认的 %s 会让超过它的下载被执行端掐掉，"+
			"而注释里承诺的是前者",
			got, archiveDownloadTimeout, toolExecTimeout)
	}
}

// The other way around: **per-file injection** must not be switched to the long timeout.
//
// Those commands are short operations (writing files of a few KB). Giving them 5 minutes would
// only add 4 extra minutes of waiting when something really hangs.
func TestBase64InjectKeepsDefaultExecTimeout(t *testing.T) {
	bundles := []SkillBundle{{
		Name:  "plain-skill",
		Files: []SkillFile{{Path: "SKILL.md", Content: []byte("正文")}},
	}}
	sh := &recordingExecer{}
	if err := InjectSkills(context.Background(), sh, "/workspace", bundles, quietLog()); err != nil {
		t.Fatalf("注入失败：%v", err)
	}
	if len(sh.timeouts) == 0 {
		t.Fatal("一条命令都没发")
	}
	if got := sh.timeouts[0]; got != toolExecTimeout {
		t.Errorf("逐文件注入应当沿用默认超时 %s，实际 %s", toolExecTimeout, got)
	}
}

// concurrencyProbeExecer records the peak of "how many commands run at the same time".
type concurrencyProbeExecer struct {
	mu      sync.Mutex
	cur     int
	peak    int
	scripts []string
	delay   time.Duration
}

func (e *concurrencyProbeExecer) Exec(_ context.Context, spec ExecSpec) (*Result, error) {
	e.mu.Lock()
	e.cur++
	if e.cur > e.peak {
		e.peak = e.cur
	}
	if len(spec.Args) == 2 {
		e.scripts = append(e.scripts, spec.Args[1])
	}
	e.mu.Unlock()

	time.Sleep(e.delay) // simulate download time so the concurrency windows really overlap

	e.mu.Lock()
	e.cur--
	e.mu.Unlock()
	code := 0
	return &Result{ExitCode: &code}, nil
}

// TestArchivePullsRunConcurrently pins "multiple skill packages are downloaded concurrently, not
// serially".
//
// # Why this one matters
//
// Each download command pays a fixed ~0.9 second container round trip (measured), while the command
// itself spends 99% of its time waiting on the network. Serially, N skills mean N round trips:
//
//	5 skills with attachments    serial 5.3s → concurrent 1.7s
//	10 skills with attachments   serial 10.6s → concurrent 2.5s
//
// **This is the most cost-effective step for "the user does not feel it is slow"**, and it is still
// synchronous: no background goroutine, no waiting gate, no race with idle/pause.
func TestArchivePullsRunConcurrently(t *testing.T) {
	const n = 8
	bundles := make([]SkillBundle, 0, n)
	for i := 0; i < n; i++ {
		bundles = append(bundles, SkillBundle{
			Name:         fmt.Sprintf("skill-%d", i),
			ArchiveURL:   fmt.Sprintf("https://storage.example.test/obj/%d.zip", i),
			ArchiveBytes: 1024,
		})
	}

	sh := &concurrencyProbeExecer{delay: 50 * time.Millisecond}
	start := time.Now()
	if err := InjectSkills(context.Background(), sh, "/workspace", bundles, quietLog()); err != nil {
		t.Fatalf("注入失败：%v", err)
	}
	elapsed := time.Since(start)

	if sh.peak < 2 {
		t.Errorf("峰值并发只有 %d——下载还是串行的，N 个技能要付 N 次容器往返", sh.peak)
	}
	if sh.peak > archivePullConcurrency {
		t.Errorf("峰值并发 %d 超过上限 %d——会把 执行端的连接打满",
			sh.peak, archivePullConcurrency)
	}
	// Serially it takes 8×50ms=400ms; with concurrency 4 it should be a bit over 100ms.
	if elapsed > 300*time.Millisecond {
		t.Errorf("耗时 %s，看着仍是串行（串行约 %s，并发应当约 %s）",
			elapsed, time.Duration(n)*sh.delay, time.Duration(n/archivePullConcurrency)*sh.delay)
	}
	t.Logf("%d 个技能包：峰值并发 %d，耗时 %s（串行需 %s）",
		n, sh.peak, elapsed.Round(time.Millisecond), time.Duration(n)*sh.delay)

	// None may be missed.
	if len(sh.scripts) != n {
		t.Errorf("应当发出 %d 条下载命令，实际 %d 条", n, len(sh.scripts))
	}
}

// deadlineProbeExecer records the **remaining context budget each command sees**.
type deadlineProbeExecer struct {
	mu        sync.Mutex
	remaining []time.Duration
	delay     time.Duration
}

func (e *deadlineProbeExecer) Exec(ctx context.Context, _ ExecSpec) (*Result, error) {
	var left time.Duration
	if dl, ok := ctx.Deadline(); ok {
		left = time.Until(dl)
	}
	e.mu.Lock()
	e.remaining = append(e.remaining, left)
	e.mu.Unlock()

	time.Sleep(e.delay)
	code := 0
	return &Result{ExitCode: &code}, nil
}

// TestQueuedDownloadsGetFullTimeoutBudget pins "time spent queuing for a token does not count
// toward the download timeout".
//
// # Why this needs its own test
//
// Concurrency is 4, and an agent can bind up to 20 skills, so the later ones must queue. If
// `context.WithTimeout` were written **before** `sem <- struct{}{}`, queue time would eat the
// budget: by the time the last batch gets a token there may be only a few seconds left, so they
// time out for "waiting too long".
//
// That failure has nothing to do with the skill or the network; it is purely caused by queuing,
// **and the error says "download timed out"**, not pointing at all to the real cause. This is
// exactly the kind of trap this repo keeps warning about.
//
// The test is "every command sees a remaining budget close to full", not "total time"; the latter
// is unstable in concurrency tests.
func TestQueuedDownloadsGetFullTimeoutBudget(t *testing.T) {
	const n = 12 // 3 batches (concurrency 4)
	bundles := make([]SkillBundle, 0, n)
	for i := 0; i < n; i++ {
		bundles = append(bundles, SkillBundle{
			Name:         fmt.Sprintf("skill-%d", i),
			ArchiveURL:   fmt.Sprintf("https://storage.example.test/obj/%d.zip", i),
			ArchiveBytes: 1024,
		})
	}

	// Each command takes 80ms, so the third batch waits about 160ms to get a token.
	sh := &deadlineProbeExecer{delay: 80 * time.Millisecond}
	if err := InjectSkills(context.Background(), sh, "/workspace", bundles, quietLog()); err != nil {
		t.Fatalf("注入失败：%v", err)
	}

	if len(sh.remaining) != n {
		t.Fatalf("应当发出 %d 条命令，实际 %d 条", n, len(sh.remaining))
	}
	// Every command should see a budget close to full.
	//
	// ⚠️ **The tolerance must be much smaller than the queue time, otherwise this test is always
	// green.** This was hit during a mutation check: with a 1 second tolerance and the third batch
	// queuing only 160ms (2×80ms), the wrong version that moves the timeout before queuing still
	// passed. The tolerance is 30ms: smaller than one batch's delay (80ms), so "waited one extra
	// batch" is always detected.
	full := archiveDownloadTimeout + 30*time.Second
	const tolerance = 30 * time.Millisecond
	for i, left := range sh.remaining {
		if left < full-tolerance {
			t.Errorf("第 %d 条命令只剩 %s 预算（满额 %s，容差 30ms）——"+
				"说明排队时间被算进了超时里，排在后面的技能会因为等太久而失败，"+
				"而报错会说「下载超时」，完全不指向排队",
				i, left.Round(time.Millisecond), full)
		}
	}
}

// Tests for skill package parsing, focused on **security boundaries**.
//
// # Every security case was mutation-tested
//
// The nature of security checks is "remove one and everything still works": normal packages still
// upload, only attack packages get through. So asserting only "the attack package is rejected" is not
// enough: that assertion might happen to be true for another reason (for example the package itself
// was built wrong and got stopped by "SKILL.md not found", not by the escape check).
//
// The approach is to assert **the specific error content** in every case, and during development
// comment out the matching check and confirm the case turns red. The mutation results are recorded
// in each case's comment. Asserting the error content rather than just err != nil is exactly what
// lets "stopped by some other check in passing" and "stopped by the target check" be told apart.
package skill

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/model/persistmodel"
)

// zipEntry is one entry for building a test zip.
type zipEntry struct {
	name string
	body string
	// When mode is non-zero it is set in the header, used to build symlink entries.
	mode uint32
	// When raw is true, skip Deflate and Store directly; not needed for building zip bombs, but
	// kept to observe how the compression ratio affects parsing.
	raw bool
}

// buildZip assembles a zip.
//
// Uses archive/zip's CreateRaw/CreateHeader rather than prepared binary files: the tests need to
// express shapes like "the entry name is ../../x" that normal packaging tools **cannot produce**.
func buildZip(t *testing.T, entries []zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		hdr := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		if e.raw {
			hdr.Method = zip.Store
		}
		if e.mode != 0 {
			// Write the high 16 bits of ExternalAttrs directly; that is where the Unix mode lives in a
			// zip. This rather than SetMode: SetMode takes an fs.FileMode (Go's abstract bits), while we
			// want to express the **system** bit S_IFLNK, and writing it directly is closer to a real
			// zip.
			hdr.ExternalAttrs = e.mode << 16
		}
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatalf("造 zip 条目 %q 失败：%v", e.name, err)
		}
		if _, err := io.WriteString(w, e.body); err != nil {
			t.Fatalf("写 zip 条目 %q 失败：%v", e.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("收尾 zip 失败：%v", err)
	}
	return buf.Bytes()
}

// goodSkillMD is a valid SKILL.md.
//
// **Every field's value differs** (name / description / body are all different): with shared
// values, an implementation bug like "filling description with name" could also pass the
// assertions.
const goodSkillMD = `---
name: ticket-triage
description: 当用户反馈问题、报障或提交工单时使用。
---

# 工单分诊

1. 读完整条反馈
2. 判定严重级别
`

func TestParseSkillMarkdown(t *testing.T) {
	pkg, err := ParseSkillPackage("SKILL.md", []byte(goodSkillMD))
	if err != nil {
		t.Fatalf("合法 SKILL.md 应当解析成功：%v", err)
	}
	if pkg.Name != "ticket-triage" {
		t.Errorf("name 解析错了：%q", pkg.Name)
	}
	if pkg.Description != "当用户反馈问题、报障或提交工单时使用。" {
		t.Errorf("description 解析错了：%q", pkg.Description)
	}
	// DisplayTitle falls back to name when missing.
	if pkg.DisplayTitle != "ticket-triage" {
		t.Errorf("displayTitle 应当回落到 name，实际 %q", pkg.DisplayTitle)
	}
	if !strings.HasPrefix(pkg.Instructions, "# 工单分诊") {
		t.Errorf("正文应当从标题开始（frontmatter 要被剥掉）：%q", pkg.Instructions)
	}
	if strings.Contains(pkg.Instructions, "name:") {
		t.Error("正文里不该残留 frontmatter")
	}
	// The slice must be non-nil: it goes into JSON, and nil serializes as null, making the frontend's .map() throw.
	if pkg.Files == nil {
		t.Error("Files 必须是非 nil 的空切片")
	}
}

// TestParseSkillZipKeepsAttachedFiles is the core positive case of this task: files attached in the
// zip must be kept, and the top-level directory stripped.
func TestParseSkillZipKeepsAttachedFiles(t *testing.T) {
	data := buildZip(t, []zipEntry{
		{name: "ticket-triage/"},
		{name: "ticket-triage/SKILL.md", body: goodSkillMD},
		{name: "ticket-triage/scripts/init.sh", body: "#!/bin/sh\necho hi\n"},
		{name: "ticket-triage/references/rules.md", body: "# 规则\n"},
	})

	pkg, err := ParseSkillPackage("ticket-triage.zip", data)
	if err != nil {
		t.Fatalf("合法 zip 应当解析成功：%v", err)
	}
	if len(pkg.Files) != 2 {
		t.Fatalf("应当保留 2 个附件，实际 %d 个：%+v", len(pkg.Files), pkg.Files)
	}
	// The top-level directory must be stripped; otherwise the same skill uploaded as a bare md and as a zip would get two sets of paths.
	byPath := map[string]string{}
	for _, f := range pkg.Files {
		byPath[f.Path] = string(f.Content)
	}
	if got, ok := byPath["scripts/init.sh"]; !ok {
		t.Errorf("附件路径应当剥掉顶层目录，实际有这些：%v", keysOf(byPath))
	} else if got != "#!/bin/sh\necho hi\n" {
		t.Errorf("附件内容不对：%q", got)
	}
	if _, ok := byPath["references/rules.md"]; !ok {
		t.Errorf("references 下的附件丢了：%v", keysOf(byPath))
	}
	// SKILL.md itself should not appear among the attachments: its content is in Instructions, and
	// storing it twice would give two sources of truth.
	for _, f := range pkg.Files {
		if f.Path == "SKILL.md" {
			t.Error("SKILL.md 不该同时出现在 Files 里")
		}
	}
	// Size must match the content length: the UI uses it to show the size, and a mismatch shows the wrong size.
	for _, f := range pkg.Files {
		if f.Size != len(f.Content) {
			t.Errorf("%s 的 Size(%d) 与内容长度(%d) 不一致", f.Path, f.Size, len(f.Content))
		}
	}
}

// TestParseSkillZipRejectsZipSlip is the Zip Slip case.
//
// Mutation testing: comment out the two lines `cleaned == ".." ||
// strings.HasPrefix(cleaned, "../")` in sanitizeZipEntry → all four subcases of this case fail (the
// escaping entry is accepted as an ordinary attachment).
func TestParseSkillZipRejectsZipSlip(t *testing.T) {
	cases := []struct {
		name  string
		entry string
	}{
		// The most direct form.
		{"直接跳出", "../evil.sh"},
		// Jumping up two levels, with legal path segments before it; implementations beyond Contains("..") easily miss it.
		{"绕一圈再跳出", "a/b/../../../evil.sh"},
		// Backslash separators: path.Clean does not recognize them, and without converting first the check is bypassed.
		{"反斜杠分隔符", `..\evil.sh`},
		// An escape that only shows up after the top-level directory is stripped.
		{"剥前缀后逃逸", "ticket-triage/../../evil.sh"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := buildZip(t, []zipEntry{
				{name: "ticket-triage/SKILL.md", body: goodSkillMD},
				{name: tc.entry, body: "rm -rf /"},
			})
			_, err := ParseSkillPackage("x.zip", data)
			if err == nil {
				t.Fatal("越出目录的条目必须被拒")
			}
			if !errors.Is(err, persistmodel.ErrInvalidArgument) {
				t.Errorf("应当是 persistmodel.ErrInvalidArgument：%v", err)
			}
			// Assert that **this** check stopped it, not some other check in passing.
			if !strings.Contains(err.Error(), "越出目录") &&
				!strings.Contains(err.Error(), "绝对路径") {
				t.Errorf("报错应当指出是路径逃逸，实际：%v", err)
			}
		})
	}
}

// TestParseSkillZipRejectsAbsolutePaths is the absolute path and drive letter case.
//
// Mutation testing: comment out the check `path.IsAbs(name) || hasDriveLetter(name)` → both
// subcases fail.
func TestParseSkillZipRejectsAbsolutePaths(t *testing.T) {
	for _, entry := range []string{"/etc/cron.d/evil", `C:\windows\evil.bat`} {
		t.Run(entry, func(t *testing.T) {
			data := buildZip(t, []zipEntry{
				{name: "SKILL.md", body: goodSkillMD},
				{name: entry, body: "evil"},
			})
			_, err := ParseSkillPackage("x.zip", data)
			if err == nil {
				t.Fatal("绝对路径条目必须被拒")
			}
			if !strings.Contains(err.Error(), "绝对路径") {
				t.Errorf("报错应当指出是绝对路径，实际：%v", err)
			}
		})
	}
}

// TestParseSkillZipRejectsSymlink is the case for "a symlink escaping the package must be rejected".
//
// Symlinks themselves are **no longer always rejected** (see TestParseSkillZipDereferencesSymlink:
// ones that can be resolved inside the package are dereferenced and accepted), but once the target
// escapes the package it is still rejected; that is exactly what the original defense guards
// against, and dereferencing must not become a way around it. This uses an absolute path target; for
// the `../` form see TestParseSkillZipRejectsSymlinkEscape.
//
// Mutation testing: comment out the check `path.IsAbs(target) || hasDriveLetter(target)` in
// resolveSymlinkTarget → this case fails (the target is looked up inside the package as
// "/etc/passwd", reporting "not in the archive" instead of "points to an absolute path", so the
// assertion fails).
//
// ⚠️ This case is **not enough** to pin the symlink detection; it must be read together with
// TestParseSkillZipAcceptsRealWorldFileModes: here regular entries keep mode: 0, so the bug "wrong
// type mask, treating regular files as symlinks too" cannot be caught by it.
func TestParseSkillZipRejectsSymlink(t *testing.T) {
	// 0o120000 is S_IFLNK. SetMode converts it to fs.ModeSymlink.
	data := buildZip(t, []zipEntry{
		{name: "SKILL.md", body: goodSkillMD},
		{name: "link", body: "/etc/passwd", mode: 0o120777},
	})
	_, err := ParseSkillPackage("x.zip", data)
	if err == nil {
		t.Fatal("指向宿主机绝对路径的符号链接必须被拒")
	}
	if !strings.Contains(err.Error(), "符号链接") {
		t.Errorf("报错应当指出是符号链接，实际：%v", err)
	}
	// Assert that the "absolute path" check stopped it, not "the target is not in the package" in
	// passing; the latter would mean the code really looked up /etc/passwd as a path inside the
	// package, which is a different implementation.
	if !strings.Contains(err.Error(), "绝对路径") {
		t.Errorf("报错应当指出目标是绝对路径，实际：%v", err)
	}
}

// TestParseSkillZipDereferencesSymlink is the core positive case for symlink dereferencing: a
// symlink pointing to a file that exists in the package must be replaced by the target's **real
// content** and accepted.
//
// The path is the symlink's own (`docs/alias.md`), the content is the target's; that is the
// definition of "dereference". Accepted rather than rejected, because in real skill packages
// symlinks are a common way to organize (one piece of content referenced in several places), and
// since we never unzip to disk, dereferencing inside the package has no escape risk.
//
// Mutation testing: comment out the `derefSkillSymlinks` call in parseSkillZip → all three subcases
// fail (the symlink entry's content is empty: it was neither read as a regular file nor
// dereferenced, so alias.md is empty / the SKILL.md one reports "SKILL.md not found").
func TestParseSkillZipDereferencesSymlink(t *testing.T) {
	const targetBody = "# 共享参考\n\n这份内容被软链引用。\n"

	t.Run("同目录", func(t *testing.T) {
		data := buildZip(t, []zipEntry{
			{name: "pack/SKILL.md", body: goodSkillMD, mode: 0o100644},
			{name: "pack/real.md", body: targetBody, mode: 0o100644},
			{name: "pack/alias.md", body: "real.md", mode: 0o120777},
		})
		pkg, err := ParseSkillPackage("pack.zip", data)
		if err != nil {
			t.Fatalf("指向包内文件的软链应当被解引用收下，实际：%v", err)
		}
		byPath := map[string]string{}
		for _, f := range pkg.Files {
			byPath[f.Path] = string(f.Content)
		}
		// The path is the symlink's own, the content is the target's.
		if got := byPath["alias.md"]; got != targetBody {
			t.Errorf("alias.md 的内容应当等于目标文件的内容，实际 %q（有这些：%v）",
				got, keysOf(byPath))
		}
		// The target file itself is accepted as usual; both are there.
		if got := byPath["real.md"]; got != targetBody {
			t.Errorf("目标文件本身也该在附件里，实际 %q", got)
		}
		// Size must follow the content after dereferencing, not the length of the symlink's target
		// string: the UI uses it to show the size, and len("real.md") would show 7 bytes.
		for _, f := range pkg.Files {
			if f.Size != len(f.Content) {
				t.Errorf("%s 的 Size(%d) 与内容长度(%d) 不一致", f.Path, f.Size, len(f.Content))
			}
		}
	})

	t.Run("相对上级目录", func(t *testing.T) {
		// The target is resolved "relative to the symlink's directory": ../shared/a.md in
		// refs/alias.md resolves to shared/a.md. An implementation resolving from the package root
		// would not find it here and error.
		data := buildZip(t, []zipEntry{
			{name: "pack/SKILL.md", body: goodSkillMD, mode: 0o100644},
			{name: "pack/shared/a.md", body: targetBody, mode: 0o100644},
			{name: "pack/refs/alias.md", body: "../shared/a.md", mode: 0o120777},
		})
		pkg, err := ParseSkillPackage("pack.zip", data)
		if err != nil {
			t.Fatalf("相对上级目录的软链应当能解开，实际：%v", err)
		}
		byPath := map[string]string{}
		for _, f := range pkg.Files {
			byPath[f.Path] = string(f.Content)
		}
		if got := byPath["refs/alias.md"]; got != targetBody {
			t.Errorf("refs/alias.md 应当解到 shared/a.md 的内容，实际 %q（有这些：%v）",
				got, keysOf(byPath))
		}
	})

	t.Run("SKILL.md 自己是软链", func(t *testing.T) {
		// Dereferencing is deliberately done before "picking SKILL.md", so the body can also be a
		// symlink. Picking the body first, this package would report "SKILL.md not found".
		data := buildZip(t, []zipEntry{
			{name: "pack/SKILL.md", body: "doc/main.md", mode: 0o120777},
			{name: "pack/doc/main.md", body: goodSkillMD, mode: 0o100644},
		})
		pkg, err := ParseSkillPackage("pack.zip", data)
		if err != nil {
			t.Fatalf("SKILL.md 是软链时也应当能解析，实际：%v", err)
		}
		if pkg.Name != "ticket-triage" {
			t.Errorf("正文应当取自软链目标，name 实际是 %q", pkg.Name)
		}
		if !strings.HasPrefix(pkg.Instructions, "# 工单分诊") {
			t.Errorf("正文应当是目标文件的内容，实际：%q", pkg.Instructions)
		}
	})

	t.Run("软链套软链", func(t *testing.T) {
		// a → b → real file. At least one level is the requirement; this pins that several levels work too.
		data := buildZip(t, []zipEntry{
			{name: "pack/SKILL.md", body: goodSkillMD, mode: 0o100644},
			{name: "pack/real.md", body: targetBody, mode: 0o100644},
			{name: "pack/mid.md", body: "real.md", mode: 0o120777},
			{name: "pack/top.md", body: "mid.md", mode: 0o120777},
		})
		pkg, err := ParseSkillPackage("pack.zip", data)
		if err != nil {
			t.Fatalf("软链套软链应当能解开，实际：%v", err)
		}
		byPath := map[string]string{}
		for _, f := range pkg.Files {
			byPath[f.Path] = string(f.Content)
		}
		for _, p := range []string{"top.md", "mid.md", "real.md"} {
			if got := byPath[p]; got != targetBody {
				t.Errorf("%s 的内容应当是目标文件的内容，实际 %q", p, got)
			}
		}
	})
}

// TestParseSkillZipRejectsMissingSymlinkTarget pins "a missing target is an error, and it must say
// which symlink".
//
// Silently dropping the entry is not OK: a symlink in the package means the author thought that file
// was useful, and dropping it silently would make the agent, after the skill is installed, read a
// file that does not exist per SKILL.md, then retry a few paths and give up.
//
// Mutation testing: change `if !ok { return nil, ... }` in resolveSymlink to
// `if !ok { return nil, nil }` (accept it as an empty file) → both subcases fail.
func TestParseSkillZipRejectsMissingSymlinkTarget(t *testing.T) {
	t.Run("目标不在包里", func(t *testing.T) {
		data := buildZip(t, []zipEntry{
			{name: "pack/SKILL.md", body: goodSkillMD, mode: 0o100644},
			{name: "pack/alias.md", body: "nowhere.md", mode: 0o120777},
		})
		_, err := ParseSkillPackage("pack.zip", data)
		if err == nil {
			t.Fatal("软链目标不存在必须报错")
		}
		if !errors.Is(err, persistmodel.ErrInvalidArgument) {
			t.Errorf("应当是 persistmodel.ErrInvalidArgument：%v", err)
		}
		// The error must say **which** symlink points to **which** missing target, otherwise the user
		// has no way in, in a package of dozens of files.
		if !strings.Contains(err.Error(), "alias.md") {
			t.Errorf("报错应当带上软链自己的路径，实际：%v", err)
		}
		if !strings.Contains(err.Error(), "nowhere.md") {
			t.Errorf("报错应当带上缺失的目标路径，实际：%v", err)
		}
	})

	t.Run("目标是个目录", func(t *testing.T) {
		// Directory entries are skipped in sanitizeZipEntry, so they cannot be found; grouped with
		// "does not exist" in one sentence; both are "cannot get the file content".
		data := buildZip(t, []zipEntry{
			{name: "pack/SKILL.md", body: goodSkillMD, mode: 0o100644},
			{name: "pack/sub/", mode: 0o040755},
			{name: "pack/sub/a.md", body: "x", mode: 0o100644},
			{name: "pack/alias", body: "sub", mode: 0o120777},
		})
		_, err := ParseSkillPackage("pack.zip", data)
		if err == nil {
			t.Fatal("软链指向目录必须报错，不能静默收下")
		}
		if !strings.Contains(err.Error(), "符号链接") {
			t.Errorf("报错应当指出是符号链接，实际：%v", err)
		}
	})
}

// TestParseSkillZipRejectsSymlinkEscape pins "a symlink target escaping the package must be
// rejected".
//
// This extends the Zip Slip defense: the symlink's **entry name** has passed sanitizeZipEntry, but
// its **target** is another, completely separate path equally controlled by the attacker. An
// implementation that only checks entry names would follow the target here to look up
// ../../etc/passwd.
//
// Mutation testing: comment out the check `cleaned == ".." || strings.HasPrefix(cleaned, "../")` in
// resolveSymlinkTarget → all four subcases fail (it reports "the target is not in the archive"
// instead, so the "escapes" assertion fails).
func TestParseSkillZipRejectsSymlinkEscape(t *testing.T) {
	cases := []struct {
		name   string
		link   string
		target string
	}{
		// The most direct form.
		{"直接跳出", "pack/alias", "../../etc/passwd"},
		// Jumping up just one level counts too: outside the package root is outside the package.
		{"跳一层", "pack/alias", "../outside.md"},
		// Jumping around and then out; it only shows up after normalizing, and implementations beyond Contains("..") easily miss it.
		{"绕一圈再跳出", "pack/refs/alias", "sub/../../../outside.md"},
		// Backslash separators: path.Clean does not recognize them, and without normalizing first the check is bypassed.
		{"反斜杠分隔符", "pack/alias", `..\..\etc\passwd`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := buildZip(t, []zipEntry{
				{name: "pack/SKILL.md", body: goodSkillMD, mode: 0o100644},
				{name: tc.link, body: tc.target, mode: 0o120777},
			})
			_, err := ParseSkillPackage("pack.zip", data)
			if err == nil {
				t.Fatal("软链目标越出压缩包必须被拒")
			}
			if !errors.Is(err, persistmodel.ErrInvalidArgument) {
				t.Errorf("应当是 persistmodel.ErrInvalidArgument：%v", err)
			}
			// Assert that the "escapes" check stopped it, not "the target is not in the package" in
			// passing: the latter means the code looked up an outside path as an inside one, and that
			// path could easily be changed later into really reading it.
			if !strings.Contains(err.Error(), "越出") {
				t.Errorf("报错应当指出目标越出了压缩包，实际：%v", err)
			}
		})
	}
}

// TestParseSkillZipRejectsSymlinkCycle pins "cycles terminate".
//
// **The point here is not hanging, not which error is reported.** Self-reference and mutual
// reference can both be built in a zip, and following them is an endless loop; that is not an error
// but a hung parsing goroutine, a request timeout, and no log explaining why. So this case should be
// run with `-timeout` (verified that `-timeout 30s` passes: with the cycle-safe implementation all
// three subcases return an error within milliseconds).
//
// Cycles have **two** defenses, each pinned by different subcases; missing either one turns red:
//
//	seen set      → self-reference/mutual reference reported at once (asserted below: "must not rely on walking the full depth")
//	depth limit   → catches long chains, and any shape seen might miss
//
// Mutation testing (three times, all compile):
//   - remove both (depth limit changed to `depth < 1<<62` + comment out the two seen lines) → all
//     three subcases **hang until timeout** (not red but `panic: test timed out`, with resolveSymlink
//     at the top of the stack), exactly what this guards against.
//   - remove only the depth limit → "exceeds nesting depth" fails.
//   - comment out only seen → self-reference/mutual reference fail. ⚠️ At first this one was
//     **green**: the depth-limit message also contained the words "circular reference", so asserting
//     just that word let the two defenses stand in for each other. So "must not contain nesting
//     depth" was added below: a 2-node cycle must be recognized on the spot, not guessed after
//     walking all 8 levels.
func TestParseSkillZipRejectsSymlinkCycle(t *testing.T) {
	// assertCaughtByCycleDetector asserts the seen set caught it on the spot, not the depth limit after walking all levels.
	assertCaughtByCycleDetector := func(t *testing.T, err error) {
		t.Helper()
		if !strings.Contains(err.Error(), "循环引用") {
			t.Errorf("报错应当指出循环引用，实际：%v", err)
		}
		if strings.Contains(err.Error(), "嵌套层数") {
			t.Errorf("环应当被当场认出来，而不是靠走满层数上限兜住，实际：%v", err)
		}
	}

	t.Run("自指", func(t *testing.T) {
		data := buildZip(t, []zipEntry{
			{name: "pack/SKILL.md", body: goodSkillMD, mode: 0o100644},
			{name: "pack/loop", body: "loop", mode: 0o120777},
		})
		_, err := ParseSkillPackage("pack.zip", data)
		if err == nil {
			t.Fatal("自指软链必须报错")
		}
		assertCaughtByCycleDetector(t, err)
	})

	t.Run("互指", func(t *testing.T) {
		data := buildZip(t, []zipEntry{
			{name: "pack/SKILL.md", body: goodSkillMD, mode: 0o100644},
			{name: "pack/a", body: "b", mode: 0o120777},
			{name: "pack/b", body: "a", mode: 0o120777},
		})
		_, err := ParseSkillPackage("pack.zip", data)
		if err == nil {
			t.Fatal("互指软链必须报错")
		}
		assertCaughtByCycleDetector(t, err)
	})

	t.Run("超过嵌套层数", func(t *testing.T) {
		// A chain with no cycle but longer than the limit: when "really nested this deep" and "there
		// is a cycle" cannot be told apart, an error is better than hanging, so the depth limit itself
		// needs a readable error too.
		entries := []zipEntry{{name: "pack/SKILL.md", body: goodSkillMD, mode: 0o100644}}
		for i := 0; i <= maxSymlinkDepth+1; i++ {
			entries = append(entries, zipEntry{
				name: fmt.Sprintf("pack/l%02d", i),
				body: fmt.Sprintf("l%02d", i+1),
				mode: 0o120777,
			})
		}
		// The end of the chain is a real file.
		entries = append(entries, zipEntry{
			name: fmt.Sprintf("pack/l%02d", maxSymlinkDepth+2),
			body: "终点", mode: 0o100644,
		})
		_, err := ParseSkillPackage("pack.zip", buildZip(t, entries))
		if err == nil {
			t.Fatal("超过嵌套层数上限必须报错")
		}
		if !strings.Contains(err.Error(), "嵌套层数") {
			t.Errorf("报错应当指出嵌套层数超限，实际：%v", err)
		}
	})
}

// TestParseSkillZipCountsDereferencedBytes pins "bytes swapped in by dereferencing count toward the
// total limit".
//
// Dereferencing **amplifies**: N symlinks pointing to the same big file add N copies of real
// content. Without adding them up, "symlink + big file" is a way around MaxSkillTotalBytes: the
// package looks small, and only when injecting into the container does it turn out to need several
// MB written, giving a string of timeouts instead of one error.
//
// Mutation testing: comment out the `if budget > MaxSkillTotalBytes` check after
// `budget += len(content)` in derefSkillSymlinks → this case fails (after the symlinks expand, the
// total is over the limit but accepted).
func TestParseSkillZipCountsDereferencedBytes(t *testing.T) {
	// Real files filling up close to the total limit but **leaving one slot**, none over the per-file
	// limit. Deliberately not filling all 8 slots: SKILL.md itself takes bytes too, and filling them
	// all would already be over the limit; then the assertion below would be true because "the real
	// files themselves are over", unrelated to symlinks (the first version was written that way, and
	// the precondition assertion below caught it).
	chunk := strings.Repeat("C", MaxSkillFileBytes)
	n := MaxSkillTotalBytes/MaxSkillFileBytes - 1
	base := []zipEntry{{name: "SKILL.md", body: goodSkillMD}}
	for i := 0; i < n; i++ {
		base = append(base, zipEntry{name: fmt.Sprintf("chunk-%02d.bin", i), body: chunk})
	}

	// First prove these real files can be accepted on their own (**not** already over the limit).
	// Without this step, the assertion below might be true only because the real files themselves
	// are over, unrelated to symlinks.
	if _, err := ParseSkillPackage("x.zip", buildZip(t, base)); err != nil {
		t.Fatalf("留一格的包应当收得下（否则下面的断言证明不了是软链导致的）：%v", err)
	}

	// Then add two symlinks pointing to the same big file, which goes over after expanding: one copy of
	// real content becomes three, which is what "dereferencing amplifies" means.
	withLinks := append(base,
		zipEntry{name: "alias-1.bin", body: "chunk-00.bin", mode: 0o120777},
		zipEntry{name: "alias-2.bin", body: "chunk-00.bin", mode: 0o120777},
	)
	_, err := ParseSkillPackage("x.zip", buildZip(t, withLinks))
	if err == nil {
		t.Fatal("软链展开后总量超限必须被拒")
	}
	// Assert that the **expansion** check stopped it, not the regular-file accumulation; the two
	// messages are deliberately different, precisely so "were the symlinks counted" can be told apart
	// in the test.
	if !strings.Contains(err.Error(), "符号链接展开后总大小超过上限") {
		t.Errorf("报错应当指出是软链展开后超限，实际：%v", err)
	}
}

// TestParseSkillZipAcceptsRealWorldFileModes pins "zips produced by real packaging tools are
// accepted".
//
// # Why this is needed
//
// Symlink detection must recognize S_IFLNK in the high 16 bits of ExternalAttrs, and the file type
// is an **enum value** over those 4 bits, not independent flag bits. Writing `attrs & S_IFLNK != 0`
// would judge **every regular file** a symlink (`0o100644 & 0o120000 == 0o100000`), so every package
// produced by a packaging tool that records the Unix mode would be rejected; zip(1), macOS
// right-click compress, and Python's zipfile.write all record 0o100644.
//
// This bug was hit exactly this way on a real machine: package a skill with python zipfile and upload
// it, and get "the archive contains a symlink entry "xxx/SKILL.md", which is not supported"; the error
// pointed nowhere near the real cause, and following the error you would first suspect your file
// really was a symlink. (Now that symlinks are dereferenced, the same bug would show up as "symlink
// "xxx/SKILL.md" points to a target ... that is not in the archive": the target being that file's own
// content treated as a path, equally far from the real cause.)
//
// The neighboring TestParseSkillZipRejectsSymlink cannot catch it: there regular entries keep
// mode: 0 (so ExternalAttrs is 0 too), which happens to skip this check. **So the two must exist as
// a pair**: one guards against letting things through (a symlink escaping the package must be
// rejected), the other against false kills (regular files must be accepted).
//
// Mutation testing: change the check in isSymlinkEntry back to
// `(entry.ExternalAttrs>>16)&unixModeSymlink != 0`
// → this case fails, RejectsSymlink stays green; change the mask to `== unixModeIFMT`
// → RejectsSymlink fails, this case stays green.
func TestParseSkillZipAcceptsRealWorldFileModes(t *testing.T) {
	// 0o100644 is a regular file + rw-r--r--, which is what zip(1) / Finder / zipfile.write write
	// into ExternalAttrs for an ordinary .md. 0o040755 is for directory entries.
	data := buildZip(t, []zipEntry{
		{name: "pack/SKILL.md", body: goodSkillMD, mode: 0o100644},
		{name: "pack/references/", mode: 0o040755},
		{name: "pack/references/notes.md", body: "# 参考\n", mode: 0o100644},
		{name: "pack/scripts/run.sh", body: "#!/bin/sh\necho hi\n", mode: 0o100755},
	})
	pkg, err := ParseSkillPackage("pack.zip", data)
	if err != nil {
		t.Fatalf("真实打包工具产出的 zip 必须收得下，实际：%v", err)
	}
	if len(pkg.Files) != 2 {
		t.Fatalf("应当收下 2 个附件（目录项与 SKILL.md 不算），实际 %d 个：%+v",
			len(pkg.Files), pkg.Files)
	}
	// The top-level directory must be stripped; attachment paths are relative to the skill directory.
	got := map[string]bool{}
	for _, f := range pkg.Files {
		got[f.Path] = true
	}
	for _, want := range []string{"references/notes.md", "scripts/run.sh"} {
		if !got[want] {
			t.Errorf("附件里缺 %q，实际：%v", want, got)
		}
	}
}

// TestParseSkillZipRejectsBomb is the zip bomb case.
//
// Highly compressible content (all zeros) has a huge compression ratio: a zip of a few KB unpacks
// to several MB. The key is **never to trust the size declared in the header**, and to count while
// unpacking.
//
// Mutation testing: replace the io.LimitReader in readZipEntry with a direct io.ReadAll(rc) and
// delete the two length checks → both subcases fail.
func TestParseSkillZipRejectsBomb(t *testing.T) {
	t.Run("单文件超限", func(t *testing.T) {
		// Just slightly over the per-file limit is enough; no need to really build several GB; what is tested is "does it count".
		big := strings.Repeat("A", MaxSkillFileBytes+1)
		data := buildZip(t, []zipEntry{
			{name: "SKILL.md", body: goodSkillMD},
			{name: "big.txt", body: big},
		})
		_, err := ParseSkillPackage("x.zip", data)
		if err == nil {
			t.Fatal("解压后超过单文件上限必须被拒")
		}
		if !strings.Contains(err.Error(), "单文件上限") {
			t.Errorf("报错应当指出单文件超限，实际：%v", err)
		}
	})

	t.Run("总量超限", func(t *testing.T) {
		// None is over the per-file limit, but together they exceed the total limit.
		entries := []zipEntry{{name: "SKILL.md", body: goodSkillMD}}
		chunk := strings.Repeat("B", MaxSkillFileBytes)
		for i := 0; i*MaxSkillFileBytes <= MaxSkillTotalBytes; i++ {
			entries = append(entries, zipEntry{
				name: fmt.Sprintf("chunk-%02d.bin", i),
				body: chunk,
			})
		}
		data := buildZip(t, entries)
		_, err := ParseSkillPackage("x.zip", data)
		if err == nil {
			t.Fatal("解压后总量超限必须被拒")
		}
		if !strings.Contains(err.Error(), "总大小") {
			t.Errorf("报错应当指出总量超限，实际：%v", err)
		}
	})
}

// TestParseSkillZipRejectsTooManyEntries is the entry count limit case.
//
// Mutation testing: comment out the check `len(zr.File) > MaxSkillFiles` → this case fails.
func TestParseSkillZipRejectsTooManyEntries(t *testing.T) {
	entries := []zipEntry{{name: "SKILL.md", body: goodSkillMD}}
	for i := 0; i <= MaxSkillFiles; i++ {
		entries = append(entries, zipEntry{name: fmt.Sprintf("f-%03d.txt", i), body: "x"})
	}
	data := buildZip(t, entries)
	_, err := ParseSkillPackage("x.zip", data)
	if err == nil {
		t.Fatal("条目数超限必须被拒")
	}
	if !strings.Contains(err.Error(), "超过上限") {
		t.Errorf("报错应当指出条目数超限，实际：%v", err)
	}
}

// TestParseSkillZipRejectsLongPath is the overly long path case.
//
// Mutation testing: comment out the MaxSkillPathBytes check → this case fails.
func TestParseSkillZipRejectsLongPath(t *testing.T) {
	long := strings.Repeat("a", MaxSkillPathBytes+1) + ".txt"
	data := buildZip(t, []zipEntry{
		{name: "SKILL.md", body: goodSkillMD},
		{name: long, body: "x"},
	})
	_, err := ParseSkillPackage("x.zip", data)
	if err == nil {
		t.Fatal("超长路径必须被拒")
	}
	if !strings.Contains(err.Error(), "路径太长") {
		t.Errorf("报错应当指出路径太长，实际：%v", err)
	}
}

// TestParseSkillZipRejectsDuplicatePaths is the duplicate paths within one package case.
//
// The zip format allows two records with the same name. Without blocking, saving would collide on
// the skill_files primary key, giving a 500.
//
// Mutation testing: comment out the ensureUniquePaths call → this case fails.
func TestParseSkillZipRejectsDuplicatePaths(t *testing.T) {
	data := buildZip(t, []zipEntry{
		{name: "SKILL.md", body: goodSkillMD},
		{name: "scripts/a.sh", body: "first"},
		{name: "scripts/a.sh", body: "second"},
	})
	_, err := ParseSkillPackage("x.zip", data)
	if err == nil {
		t.Fatal("重复路径必须被拒")
	}
	if !strings.Contains(err.Error(), "重复的路径") {
		t.Errorf("报错应当指出路径重复，实际：%v", err)
	}
}

// TestParseSkillZipRequiresExactlyOneSkillMD pins "exactly one SKILL.md".
func TestParseSkillZipRequiresExactlyOneSkillMD(t *testing.T) {
	t.Run("一个都没有", func(t *testing.T) {
		data := buildZip(t, []zipEntry{{name: "readme.md", body: "# 不是技能"}})
		_, err := ParseSkillPackage("x.zip", data)
		if err == nil || !strings.Contains(err.Error(), "找不到 SKILL.md") {
			t.Errorf("缺 SKILL.md 应当明确报错，实际：%v", err)
		}
	})

	t.Run("有两个", func(t *testing.T) {
		// Two sibling top-level directories → the prefix is not stripped → both SKILL.md are at the
		// root… Directly building two paths at the root that differ other than in case does not work
		// (different paths are not duplicates), so build the shape "both become SKILL.md after
		// stripping the prefix".
		data := buildZip(t, []zipEntry{
			{name: "SKILL.md", body: goodSkillMD},
			{name: "./SKILL.md", body: goodSkillMD},
		})
		_, err := ParseSkillPackage("x.zip", data)
		if err == nil {
			t.Fatal("多个 SKILL.md 应当被拒")
		}
		if !strings.Contains(err.Error(), "多个 SKILL.md") {
			t.Errorf("报错应当指出有多个 SKILL.md，实际：%v", err)
		}
	})
}

// TestParseSkillZipIgnoresPackagingMetadata pins "Mac/Windows packaging metadata is ignored".
//
// This is not a security case but a usability one: right-click compress on a Mac adds __MACOSX/, so
// rejecting the whole package would make the most common way of packaging unusable.
func TestParseSkillZipIgnoresPackagingMetadata(t *testing.T) {
	data := buildZip(t, []zipEntry{
		{name: "ticket-triage/SKILL.md", body: goodSkillMD},
		{name: "ticket-triage/scripts/init.sh", body: "echo hi"},
		{name: "__MACOSX/._SKILL.md", body: "垃圾"},
		{name: "ticket-triage/.DS_Store", body: "垃圾"},
	})
	pkg, err := ParseSkillPackage("x.zip", data)
	if err != nil {
		t.Fatalf("带打包元数据的 zip 应当能解析：%v", err)
	}
	// Metadata should not be accepted as attachments: it is useless to the agent and takes injection budget.
	for _, f := range pkg.Files {
		if strings.Contains(f.Path, "__MACOSX") || strings.Contains(f.Path, ".DS_Store") {
			t.Errorf("打包元数据不该进附件：%s", f.Path)
		}
	}
	// And the top-level directory must still be stripped correctly (metadata directories must not interfere with prefix detection).
	if len(pkg.Files) != 1 || pkg.Files[0].Path != "scripts/init.sh" {
		t.Errorf("附件应当只有 scripts/init.sh，实际：%+v", pkg.Files)
	}
}

// TestParseFrontmatterRejectsInvalid pins every invalid form of the frontmatter.
//
// **The point is "never silently create with empty values"**: the silent result is a skill created
// with an empty name that only blows up when uploaded to Ark, when the error cannot point back to
// this upload.
//
// Mutation testing: change any return in parseFrontmatter to "log a warning and continue" → the
// matching subcase fails.
func TestParseFrontmatterRejectsInvalid(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantMsg string
	}{
		{
			name:    "完全没有 frontmatter",
			content: "# 只有正文\n\n没有 YAML 头。\n",
			wantMsg: "缺少开头的 YAML frontmatter",
		},
		{
			name:    "frontmatter 没有结束标记",
			content: "---\nname: foo\ndescription: bar\n\n# 正文\n",
			wantMsg: "没有结束标记",
		},
		{
			name:    "缺 name",
			content: "---\ndescription: 有描述没名字\n---\n\n# 正文\n",
			wantMsg: "缺少 name",
		},
		{
			name:    "缺 description",
			content: "---\nname: foo-bar\n---\n\n# 正文\n",
			wantMsg: "缺少 description",
		},
		{
			// The Ark side only accepts [a-z0-9-]; uppercase and underscores are both rejected.
			name:    "name 有大写",
			content: "---\nname: Ticket_Triage\ndescription: 描述\n---\n\n# 正文\n",
			wantMsg: "不合法",
		},
		{
			name:    "name 以连字符开头",
			content: "---\nname: -leading\ndescription: 描述\n---\n\n# 正文\n",
			wantMsg: "不合法",
		},
		{
			name:    "YAML 语法错",
			content: "---\nname: [unclosed\ndescription: 描述\n---\n\n# 正文\n",
			wantMsg: "不是合法的 YAML",
		},
		{
			name:    "正文为空",
			content: "---\nname: foo-bar\ndescription: 描述\n---\n\n   \n",
			wantMsg: "正文是空的",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseSkillPackage("SKILL.md", []byte(tc.content))
			if err == nil {
				t.Fatal("非法 frontmatter 必须被拒，不能静默用空值创建")
			}
			if !errors.Is(err, persistmodel.ErrInvalidArgument) {
				t.Errorf("应当是 persistmodel.ErrInvalidArgument：%v", err)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("报错应当含 %q，实际：%v", tc.wantMsg, err)
			}
		})
	}
}

// TestParseFrontmatterToleratesBOMAndCRLF pins two kinds of "invisible bytes".
//
// Without tolerating them, the error would be "missing frontmatter", while the file the user sees
// clearly has frontmatter; the error points nowhere near the real cause (three bytes at the start /
// an extra \r at line ends).
func TestParseFrontmatterToleratesBOMAndCRLF(t *testing.T) {
	t.Run("UTF-8 BOM", func(t *testing.T) {
		pkg, err := ParseSkillPackage("SKILL.md", []byte("\ufeff"+goodSkillMD))
		if err != nil {
			t.Fatalf("带 BOM 的 SKILL.md 应当能解析：%v", err)
		}
		if pkg.Name != "ticket-triage" {
			t.Errorf("name 解析错了：%q", pkg.Name)
		}
	})

	t.Run("CRLF 换行", func(t *testing.T) {
		crlf := strings.ReplaceAll(goodSkillMD, "\n", "\r\n")
		pkg, err := ParseSkillPackage("SKILL.md", []byte(crlf))
		if err != nil {
			t.Fatalf("CRLF 的 SKILL.md 应当能解析：%v", err)
		}
		if pkg.Name != "ticket-triage" {
			t.Errorf("name 解析错了：%q", pkg.Name)
		}
		if pkg.Description != "当用户反馈问题、报障或提交工单时使用。" {
			t.Errorf("description 解析错了（\\r 没清掉？）：%q", pkg.Description)
		}
	})
}

// TestParseSkillPackageRejectsGarbage pins the error when non-zip content goes down the zip branch.
func TestParseSkillPackageRejectsGarbage(t *testing.T) {
	_, err := ParseSkillPackage("x.zip", []byte("这不是一个 zip 文件"))
	if err == nil {
		t.Fatal("坏 zip 必须被拒")
	}
	// The library's raw text is the English "zip: not a valid zip file", which must not appear in the
	// message shown to users: the handler's userMessage takes the part after the last ": ", which would
	// expose an English half-sentence.
	if strings.Contains(err.Error(), "not a valid zip") {
		t.Errorf("不该把库的英文原文暴露给用户：%v", err)
	}
	if !strings.Contains(err.Error(), "zip") {
		t.Errorf("报错应当提到 zip：%v", err)
	}
}

func TestParseSkillPackageRejectsEmpty(t *testing.T) {
	if _, err := ParseSkillPackage("SKILL.md", nil); err == nil {
		t.Error("空文件必须被拒")
	}
}

// TestParseSkillMarkdownRejectsOversize pins that a bare md is also bound by the per-file limit.
//
// Without blocking, a 5 MB SKILL.md could get into the database and then **silently fail to be
// written** when injecting into the container (over shelltools' maxWriteBytes), showing up as a skill
// that is installed but that the agent cannot read.
func TestParseSkillMarkdownRejectsOversize(t *testing.T) {
	body := goodSkillMD + strings.Repeat("x", MaxSkillFileBytes)
	_, err := ParseSkillPackage("SKILL.md", []byte(body))
	if err == nil {
		t.Fatal("超过单文件上限的 SKILL.md 必须被拒")
	}
	if !strings.Contains(err.Error(), "太大") {
		t.Errorf("报错应当指出文件太大，实际：%v", err)
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

package core

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/repository/dao/coremem"

	"github.com/chyroc/ama/server/model/apimodel"
)

// Reading skill attachment content (the Service method behind the GET /api/skills/{id}/file route).
//
// These cases guard three things that show up as odd problems in the UI without any error:
//
//  1. binary files must not spit random bytes into JSON (the frontend renders garbage);
//  2. "the skill is gone" and "the skill exists but the file is gone" must be reported
//     separately, otherwise the user does not know whether to recreate the skill
//     or upload the attachment again;
//  3. built-in skills must not go down the database lookup path: they are not in the skills
//     table, so that path would say "skill does not exist",
//     and that is wrong (it is clearly visible in the list).

// putTestSkillFiles creates a skill with attachments and returns its id.
//
// It goes through store instead of CreateSkillFromPackage: that path needs a zip and also runs the
// parser, while what we test here is reading, not parsing.
//
// ⚠️ **But the bytes must go through object storage** (putSkillFilesViaBlobs). After 4a, rows with
// an empty file_id cannot be read (see hydrateSkillFiles). Calling store.PutSkillFiles directly
// would make every case in this file die on "refusing to hand out an incomplete skill", while they
// test attachment reading, not object storage.
func putTestSkillFiles(
	t *testing.T, svc *Service, store *coremem.MemStore, files []apimodel.SkillFile,
) string {
	t.Helper()
	ctx := context.Background()
	sk, err := store.CreateSkill(ctx, apimodel.Skill{
		ID:          "skill-files-test",
		WorkspaceID: seedWS,
		Name:        "files-test",
		Instructions: "# 正文\n\n这份内容在 instructions 里，" +
			"**不在 files 里**（见 skillpkg.go 那条注释）。",
		Source: apimodel.SkillCustom,
	})
	if err != nil {
		t.Fatalf("建技能失败：%v", err)
	}
	putSkillFilesViaBlobs(t, ctx, svc, sk.ID, files)
	return sk.ID
}

func TestSkillFileContentReadsText(t *testing.T) {
	ctx := context.Background()
	svc, store := newService(t)
	id := putTestSkillFiles(t, svc, store, []apimodel.SkillFile{
		{Path: "scripts/init.sh", Content: []byte("#!/bin/sh\necho hi\n")},
	})

	got, err := svc.SkillFileContent(ctx, id, "scripts/init.sh")
	if err != nil {
		t.Fatalf("读附件失败：%v", err)
	}
	if got.Content != "#!/bin/sh\necho hi\n" {
		t.Errorf("内容不对：%q", got.Content)
	}
	// Size is the number of bytes returned this time. MemStore's PutSkillFiles does not trust the
	// Size passed by the caller and overwrites it with len(content) (PG does not store Size at
	// all), so the real length is expected here.
	if got.Size != 18 {
		t.Errorf("Size = %d，期望 18", got.Size)
	}
	if got.Binary || got.Truncated {
		t.Errorf("文本文件不该被标成 binary/truncated：%+v", got)
	}
}

// TestSkillFileContentBinaryGivesNoContent: binary files give no content.
//
// **The test is whether it contains a NUL byte** (reuses isBinary, the same function as the sandbox
// path). Without it, png bytes would be stuffed into a JSON string and the frontend would render a
// screen of garbage. No error, it just looks like the page is broken.
func TestSkillFileContentBinaryGivesNoContent(t *testing.T) {
	ctx := context.Background()
	svc, store := newService(t)
	// The first 8 bytes of a PNG; there is a NUL right after byte 4.
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00}
	id := putTestSkillFiles(t, svc, store, []apimodel.SkillFile{
		{Path: "references/logo.png", Content: png},
	})

	got, err := svc.SkillFileContent(ctx, id, "references/logo.png")
	if err != nil {
		t.Fatalf("读附件失败：%v", err)
	}
	if !got.Binary {
		t.Error("含 NUL 字节的文件必须标成 binary")
	}
	if got.Content != "" {
		t.Errorf("binary 时 Content 必须是空串，实际 %q", got.Content)
	}
	// The size must still be given: the UI shows "this is a 10-byte binary file".
	if got.Size != len(png) {
		t.Errorf("Size = %d，期望 %d", got.Size, len(png))
	}
}

// TestSkillFileContentTruncatesOversize: an oversized attachment is truncated and marked, not
// rejected.
//
// Normal attachments never reach this (the upload side limits a single file to 256 KiB). This
// guards against **oversized rows already in the existing database**: if those rows errored out,
// the user would see a file that cannot be opened with no explanation.
func TestSkillFileContentTruncatesOversize(t *testing.T) {
	ctx := context.Background()
	svc, store := newService(t)
	big := strings.Repeat("x", skillFileMaxBytes+100)
	id := putTestSkillFiles(t, svc, store, []apimodel.SkillFile{
		{Path: "big.txt", Content: []byte(big)},
	})

	got, err := svc.SkillFileContent(ctx, id, "big.txt")
	if err != nil {
		t.Fatalf("读附件失败：%v", err)
	}
	if !got.Truncated {
		t.Error("超过上限必须标 Truncated")
	}
	if len(got.Content) != skillFileMaxBytes {
		t.Errorf("截断后长度 = %d，期望 %d", len(got.Content), skillFileMaxBytes)
	}
	// Size reports the bytes **returned this time**, not the original file size: the UI uses it to
	// compute "how much is shown", and only together with Truncated is the information complete.
	if got.Size != skillFileMaxBytes {
		t.Errorf("Size = %d，期望 %d", got.Size, skillFileMaxBytes)
	}
}

// TestSkillFileContentBuiltinHasNoFiles: built-in skills always give 404, and **do not query the
// database**.
//
// They have zero attachments (see the top of skillbuiltin.go). Going down the database path would
// give "skill does not exist", which is wrong: it is clearly visible in the skill list.
func TestSkillFileContentBuiltinHasNoFiles(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)

	_, err := svc.SkillFileContent(ctx, BuiltinSkillIDLarkCLI, "anything.md")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("内置技能读附件应回 ErrNotFound，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "内置") {
		t.Errorf("报错要说清是内置技能，实际：%v", err)
	}
}

// TestSkillFileContentSeparatesMissingSkillFromMissingFile
// "The skill is gone" and "the skill exists but this file does not" must be reported separately.
//
// If merged into one "does not exist", the user cannot tell whether to recreate the skill or upload
// the attachment again.
func TestSkillFileContentSeparatesMissingSkillFromMissingFile(t *testing.T) {
	ctx := context.Background()
	svc, store := newService(t)
	id := putTestSkillFiles(t, svc, store, []apimodel.SkillFile{
		{Path: "a.md", Content: []byte("a")},
	})

	_, err := svc.SkillFileContent(ctx, "skill-does-not-exist", "a.md")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("技能不存在应回 ErrNotFound，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "技能不存在") {
		t.Errorf("这条要说「技能不存在」，实际：%v", err)
	}

	_, err = svc.SkillFileContent(ctx, id, "not-there.md")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("文件不存在应回 ErrNotFound，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "没有这个文件") {
		t.Errorf("这条要说「没有这个文件」，实际：%v", err)
	}
}

// TestSkillFileContentRejectsBadPaths: invalid paths are always rejected and must not degrade into
// 404.
//
// Degrading into 404 would mix up "the path is wrong" with "the file does not exist". More
// importantly, this method will later get a write version (editing attachments), and then path
// validation becomes a security boundary, not a UX issue. So the rule lives in one place,
// CleanSkillPath, already now.
func TestSkillFileContentRejectsBadPaths(t *testing.T) {
	ctx := context.Background()
	svc, store := newService(t)
	id := putTestSkillFiles(t, svc, store, []apimodel.SkillFile{
		{Path: "a.md", Content: []byte("a")},
	})

	for _, bad := range []string{
		"",                                       // not given at all
		"   ",                                    // whitespace only
		"../../etc/passwd",                       // Zip Slip
		"a/../../b",                              // only shows up after normalizing
		"/etc/passwd",                            // absolute path
		`C:/Windows/x`,                           // Windows drive letter
		`..\..\x`,                                // escape using backslashes
		"a\x00b",                                 // NUL truncation
		"a\nrm -rf /",                            // newline, starts a new line when spliced into a script
		strings.Repeat("a", maxSkillPathBytes+1), // too long
	} {
		t.Run(bad, func(t *testing.T) {
			_, err := svc.SkillFileContent(ctx, id, bad)
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("路径 %q 应被拒（ErrInvalidArgument），实际 %v", bad, err)
			}
		})
	}
}

// TestSkillFileContentNormalizesPath: `./scripts/x.sh` and `scripts/x.sh` are the same file.
//
// Without normalizing, the frontend gets a 404 just for adding a `./`, and that 404 does not point
// to the real cause at all.
func TestSkillFileContentNormalizesPath(t *testing.T) {
	ctx := context.Background()
	svc, store := newService(t)
	id := putTestSkillFiles(t, svc, store, []apimodel.SkillFile{
		{Path: "scripts/x.sh", Content: []byte("echo x")},
	})

	got, err := svc.SkillFileContent(ctx, id, "./scripts/x.sh")
	if err != nil {
		t.Fatalf("带 ./ 的路径应当能读到：%v", err)
	}
	if got.Path != "scripts/x.sh" {
		t.Errorf("返回的 Path 要是规范化后的，实际 %q", got.Path)
	}
}

// TestCleanSkillPathMatchesZipEntryJudgement
// The rules of CleanSkillPath must match sanitizeZipEntry.
//
// Both guard the same invariant: a path stored in skill_files is always a normalized relative path.
// If they drift, "a shape rejected by upload can be pushed in from another entry point": a silent
// bypass. This case pins the match, so a change on either side shows up here.
func TestCleanSkillPathMatchesZipEntryJudgement(t *testing.T) {
	// Valid ones: both sides should accept them, with the same normalized result.
	for _, ok := range []string{"SKILL.md", "scripts/init.sh", "a..b.md", "a/b/c.txt"} {
		if _, valid := CleanSkillPath(ok); !valid {
			t.Errorf("%q 应当合法", ok)
		}
	}
	// `a..b.md` is the most important one here: checking escape with strings.Contains("..") would
	// wrongly reject it.
	if got, _ := CleanSkillPath("a..b.md"); got != "a..b.md" {
		t.Errorf("合法的 a..b.md 被改成了 %q", got)
	}
	// Invalid ones: one-to-one with sanitizeZipEntry.
	for _, bad := range []string{
		"..", "../x", "a/../../b", "/abs", "C:/x", `..\x`, ".", "",
	} {
		if _, valid := CleanSkillPath(bad); valid {
			t.Errorf("%q 应当非法", bad)
		}
	}
}

// ---------------------------------------------------------------------------
// Editing attachments (UpdateSkillFile)
// ---------------------------------------------------------------------------
//
// The most important thing in this group is the **read-only gate**: this function goes through
// PutSkillFiles and never passes UpdateSkill, so the IsBuiltinSkill check there never runs on this
// path. Missing it means "built-in skill text cannot be edited, but attachments can": asymmetric
// and with zero errors. It has a test case, and mutation testing was done.

func TestUpdateSkillFileReplacesContent(t *testing.T) {
	ctx := context.Background()
	svc, store := newService(t)
	id := putTestSkillFiles(t, svc, store, []apimodel.SkillFile{
		{Path: "scripts/init.sh", Content: []byte("echo old")},
		{Path: "a.md", Content: []byte("keep me")},
	})

	before, err := store.GetSkill(ctx, id)
	if err != nil {
		t.Fatalf("读技能失败：%v", err)
	}

	sk, err := svc.UpdateSkillFile(ctx, id, "scripts/init.sh", []byte("echo new and longer"))
	if err != nil {
		t.Fatalf("改附件失败：%v", err)
	}

	// The bytes really changed.
	got, err := svc.SkillFileContent(ctx, id, "scripts/init.sh")
	if err != nil {
		t.Fatalf("回读失败：%v", err)
	}
	if got.Content != "echo new and longer" {
		t.Errorf("内容没换：%q", got.Content)
	}
	// **The file that was not edited must not be affected** (writing everything back is where this
	// most easily goes wrong).
	other, err := svc.SkillFileContent(ctx, id, "a.md")
	if err != nil || other.Content != "keep me" {
		t.Errorf("同技能的其它附件被动了：%q err=%v", other.Content, err)
	}

	// The size in the list in doc must change too. **If not kept in sync, the UI always shows the
	// old size**, while the "N attachments" count is right, so everything looks fine and only the
	// size is silently wrong.
	var size int
	for _, f := range sk.Files {
		if f.Path == "scripts/init.sh" {
			size = f.Size
		}
		// doc must not carry bytes (it is the return value of ListSkills).
		if len(f.Content) != 0 {
			t.Errorf("doc.Files 不该带 content：%s", f.Path)
		}
	}
	if size != len("echo new and longer") {
		t.Errorf("doc.Files 里的 size = %d，期望 %d", size, len("echo new and longer"))
	}
	// The version must be bumped, same as editing the text.
	if sk.LatestVersion == before.LatestVersion {
		t.Errorf("LatestVersion 应当 bump，仍是 %q", sk.LatestVersion)
	}
}

// TestUpdateSkillFileRejectsBuiltin: built-in skills cannot be edited.
//
// **Mutation check**: comment out the IsBuiltinSkill check in UpdateSkillFile, and this case must
// turn red. It also blocks a real piece of dirty data: writing with skill-builtin-* would insert an
// orphan row into skill_files with an empty workspace_id.
func TestUpdateSkillFileRejectsBuiltin(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)

	_, err := svc.UpdateSkillFile(ctx, BuiltinSkillIDLarkCLI, "a.md", []byte("x"))
	if !errors.Is(err, ErrPrecondition) {
		t.Fatalf("内置技能改附件应回 ErrPrecondition，实际 %v", err)
	}
}

// TestUpdateSkillFileRejectsSkillMD: SKILL.md is not an attachment; editing it must go through
// UpdateSkill.
//
// If let through: skill_files would get an extra row named SKILL.md, creating a second source of
// truth competing with Instructions, and it would clash with Ark's copy when injected into the
// container. Zero errors.
func TestUpdateSkillFileRejectsSkillMD(t *testing.T) {
	ctx := context.Background()
	svc, store := newService(t)
	id := putTestSkillFiles(t, svc, store, []apimodel.SkillFile{{Path: "a.md", Content: []byte("a")}})

	_, err := svc.UpdateSkillFile(ctx, id, "SKILL.md", []byte("# 新正文"))
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("改 SKILL.md 应回 ErrInvalidArgument，实际 %v", err)
	}
	// The database must not really get this extra row.
	if _, err := svc.SkillFileContent(ctx, id, "SKILL.md"); !errors.Is(err, ErrNotFound) {
		t.Error("SKILL.md 不该作为附件存在")
	}
}

// TestUpdateSkillFileOnlyExistingPaths: only existing files are edited, nothing new is created.
//
// This also makes the "entry count limit" impossible to exceed by design: nothing new, so nothing
// grows.
func TestUpdateSkillFileOnlyExistingPaths(t *testing.T) {
	ctx := context.Background()
	svc, store := newService(t)
	id := putTestSkillFiles(t, svc, store, []apimodel.SkillFile{{Path: "a.md", Content: []byte("a")}})

	_, err := svc.UpdateSkillFile(ctx, id, "brand-new.md", []byte("x"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的路径应回 ErrNotFound，实际 %v", err)
	}
}

// TestUpdateSkillFileEnforcesLimits: per-file and total limits.
//
// **Never raise these two values**: 256 KiB is worked back from the injection channel (about 341
// KiB after base64, still below shelltools' maxWriteBytes of 512 KiB). Raising it would make a file
// just over the limit **silently fail to be written into the container**: the skill is installed,
// one attachment is missing, and the agent cannot read it and just keeps retrying.
func TestUpdateSkillFileEnforcesLimits(t *testing.T) {
	ctx := context.Background()
	svc, store := newService(t)
	id := putTestSkillFiles(t, svc, store, []apimodel.SkillFile{
		{Path: "a.md", Content: []byte("a")},
		// First fill space close to the total limit, so the case below hits the total limit rather
		// than the per-file limit.
		//
		// ⚠️ **It needs +1**: without it, big + 256KiB is exactly **equal** to the total limit, and
		// the check is `> maxSkillTotalBytes` (equal is allowed), so this case would think "it was
		// not blocked". That is the test's own math error, not a gap in the implementation. The
		// first version fell into exactly this.
		{Path: "big.bin", Content: []byte(strings.Repeat("x", maxSkillTotalBytes-maxSkillFileBytes+1))},
	})

	t.Run("单文件超限", func(t *testing.T) {
		_, err := svc.UpdateSkillFile(ctx, id, "a.md",
			[]byte(strings.Repeat("x", maxSkillFileBytes+1)))
		if !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("单文件超限应被拒，实际 %v", err)
		}
	})

	t.Run("总量超限", func(t *testing.T) {
		// A single file within the limit, but together with the existing big.bin it exceeds the
		// total.
		_, err := svc.UpdateSkillFile(ctx, id, "a.md",
			[]byte(strings.Repeat("x", maxSkillFileBytes)))
		if !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("总量超限应被拒，实际 %v", err)
		}
	})
}

// TestUpdateSkillFileRejectsBadPaths: invalid paths are always rejected.
//
// This is a **security boundary**, not a UX issue: the path gets spliced into the shell script that
// injects into the container (shared/infra/shelltools/skills.go), so the rule must match the upload
// path (CleanSkillPath).
func TestUpdateSkillFileRejectsBadPaths(t *testing.T) {
	ctx := context.Background()
	svc, store := newService(t)
	id := putTestSkillFiles(t, svc, store, []apimodel.SkillFile{{Path: "a.md", Content: []byte("a")}})

	for _, bad := range []string{
		"", "   ", "../../etc/passwd", "a/../../b", "/etc/passwd",
		`C:/Windows/x`, `..\..\x`, "a\x00b", "a\nrm -rf /",
	} {
		t.Run(bad, func(t *testing.T) {
			_, err := svc.UpdateSkillFile(ctx, id, bad, []byte("x"))
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("路径 %q 应被拒，实际 %v", bad, err)
			}
		})
	}
}

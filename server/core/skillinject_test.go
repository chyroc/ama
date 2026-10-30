// Pins one invariant: **skills bound to an agent must be visible under the skills directory**.
//
// # Why this needs its own test
//
// SkillsFor used to have `if len(sk.Files) == 0 { continue }`, so skills with no attachments
// (hand-written ones, and the three built-in skills) **had no directory at all** in the container.
// Whether the skill directory (`<workdir>/skills/<skill name>/`, see shelltools/skills.go) exists
// is what the model uses to decide "do I have this skill": no directory means no skill. The user
// report of `ls -la /mnt/skills/ama/` returning `No such file or directory` was exactly this
// (back then the location was still `/mnt/skills`): `ama` is a built-in skill with no attachments.
//
// This gap had all three traits of "hardest to debug":
//
//	① zero errors: the inject log said "skill files injected into container" (true for the ones with attachments);
//	② skills with attachments work fine, so a quick test passes;
//	③ the symptom shows up at the far end (the model says "I don't have this skill"), not pointing at the inject logic.
//
// So every assertion checks "does the directory/file exist", not "did inject report an error".
package core

import (
	"context"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// skillsForAgent binds skills to the agent, starts a conversation, and returns the skill bundles
// to inject for that session.
//
// It goes through the real chain (Run → Agent → SkillIDs) instead of building bundles directly:
// the bug was a continue on that chain, so testing around it would test nothing.
func skillsForAgent(t *testing.T, skillIDs ...string) []struct {
	Name  string
	Files map[string]string
} {
	t.Helper()
	ctx := context.Background()
	svc, store, _ := newChatService(t)
	bindSkills(t, store, seedChatAgent, skillIDs...)

	run, err := svc.CreateConversation(ctx, seedWS, seedChatAgent)
	if err != nil {
		t.Fatalf("开对话失败：%v", err)
	}

	loader := NewSkillFileLoader(svc, nil)
	bundles, err := loader.SkillsFor(ctx, run.ID)
	if err != nil {
		t.Fatalf("SkillsFor 失败：%v", err)
	}

	out := make([]struct {
		Name  string
		Files map[string]string
	}, 0, len(bundles))
	for _, b := range bundles {
		files := map[string]string{}
		for _, f := range b.Files {
			files[f.Path] = string(f.Content)
		}
		out = append(out, struct {
			Name  string
			Files map[string]string
		}{Name: b.Name, Files: files})
	}
	return out
}

// TestBuiltinSkillLandsInContainer is the core of this fix: built-in skills have no attachments,
// but **must** have a directory + SKILL.md in the container.
//
// Uses the ama built-in skill, because that is the one users reported (`ls /mnt/skills/ama/` →
// does not exist).
func TestBuiltinSkillLandsInContainer(t *testing.T) {
	got := skillsForAgent(t, BuiltinSkillIDAMA)

	if len(got) != 1 {
		t.Fatalf("绑了 1 个内置技能就应当注入 1 个包，实际 %d 个 —— "+
			"0 个意味着容器里技能目录下没有 ama/，模型会说没这个技能", len(got))
	}
	if got[0].Name != "ama" {
		t.Errorf("目录名要等于技能名（落点是 <workdir>/skills/<技能名>/），实际 %q", got[0].Name)
	}
	body, ok := got[0].Files["SKILL.md"]
	if !ok {
		t.Fatalf("必须写出 SKILL.md，否则目录是空的、等于没有这个技能；实际有 %v",
			skillMDKeys(got[0].Files))
	}
	// The body of a built-in skill is a code constant (skillbuiltin.go). What lands on disk must
	// be that real body, not a placeholder: a placeholder would make the model read a skill
	// document with no content.
	if len(strings.TrimSpace(body)) < 200 {
		t.Errorf("SKILL.md 应当是内置技能的真正文（几 KB），实际只有 %d 字节：%q",
			len(body), truncateForMsg(body))
	}
}

// TestHandwrittenSkillWithoutFilesLandsInContainer covers hand-written skills (source=custom, no
// attachments). They take a different branch from built-in skills (one reads the DB, the other
// uses code constants).
func TestHandwrittenSkillWithoutFilesLandsInContainer(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newChatService(t)

	// Hand-write a skill: it has a body and no attachments.
	// Use CreateSkill rather than SaveSkill: the latter "updates an existing row" and returns
	// ErrNotFound for a new id (I hit this once, see the error message below).
	sk, err := store.CreateSkill(ctx, apimodel.Skill{
		ID:           "skill_handwritten",
		WorkspaceID:  seedWS,
		Name:         "handwritten-demo",
		DisplayTitle: "Handwritten Demo",
		Description:  "一个手填的技能",
		Instructions: "# 手填技能\n\n这是正文，应当原样落进 SKILL.md。\n",
		Source:       apimodel.SkillCustom,
		CreatedAt:    apimodel.NowISO(),
		UpdatedAt:    apimodel.NowISO(),
	})
	if err != nil {
		t.Fatalf("建技能失败：%v", err)
	}
	bindSkills(t, store, seedChatAgent, sk.ID)

	run, err := svc.CreateConversation(ctx, seedWS, seedChatAgent)
	if err != nil {
		t.Fatalf("开对话失败：%v", err)
	}
	bundles, err := NewSkillFileLoader(svc, nil).SkillsFor(ctx, run.ID)
	if err != nil {
		t.Fatalf("SkillsFor 失败：%v", err)
	}
	if len(bundles) != 1 {
		t.Fatalf("手填技能也要落盘，实际注入 %d 个包", len(bundles))
	}
	if bundles[0].Name != "handwritten-demo" {
		t.Errorf("目录名应当是技能名，实际 %q", bundles[0].Name)
	}
	if len(bundles[0].Files) != 1 || bundles[0].Files[0].Path != "SKILL.md" {
		t.Fatalf("应当只有一个 SKILL.md，实际 %+v", bundles[0].Files)
	}
	// **The body lands on disk as is**: any wrapping would break things like the user's
	// front-matter, with zero errors.
	if got := string(bundles[0].Files[0].Content); got != sk.Instructions {
		t.Errorf("SKILL.md 必须是 Instructions 原文。\n期望 %q\n实际 %q",
			sk.Instructions, got)
	}
}

// TestUploadedSkillMDWinsOverGenerated pins the priority: when the uploaded bundle has its own
// SKILL.md, use **the user's copy**, and do not let the one we generate from Instructions overwrite it.
//
// Why this matters: both have the same path (SKILL.md). If both go into the same batch of
// commands, the final content depends on write order, which is effectively random and reports no error.
func TestUploadedSkillMDWinsOverGenerated(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newChatService(t)

	skills, err := store.ListSkills(ctx, seedWS)
	if err != nil || len(skills) == 0 {
		t.Fatalf("种子里应当有技能：%v", err)
	}
	sk := skills[0]
	sk.Instructions = "# 这是库里的 instructions，应当被上传的那份顶掉\n"

	const uploaded = "# 上传包里自带的 SKILL.md\n\nsentinel: UPLOADED-WINS\n"
	files := []apimodel.SkillFile{
		{Path: "SKILL.md", Content: []byte(uploaded)},
		{Path: "references/x.md", Content: []byte("# x\n")},
	}
	// Bytes go through object storage (after 4a, rows with an empty file_id cannot be read, see
	// hydrateSkillFiles).
	putSkillFilesViaBlobs(t, ctx, svc, sk.ID, files)
	sk.Files = []apimodel.SkillFile{
		{Path: "SKILL.md", Size: len(uploaded)},
		{Path: "references/x.md", Size: 4},
	}
	if _, err := store.SaveSkill(ctx, sk); err != nil {
		t.Fatalf("建技能失败：%v", err)
	}
	bindSkills(t, store, seedChatAgent, sk.ID)

	run, err := svc.CreateConversation(ctx, seedWS, seedChatAgent)
	if err != nil {
		t.Fatalf("开对话失败：%v", err)
	}
	bundles, err := NewSkillFileLoader(svc, nil).SkillsFor(ctx, run.ID)
	if err != nil {
		t.Fatalf("SkillsFor 失败：%v", err)
	}
	if len(bundles) != 1 {
		t.Fatalf("应当注入 1 个包，实际 %d", len(bundles))
	}

	var seenSkillMD int
	var body string
	for _, f := range bundles[0].Files {
		if f.Path == "SKILL.md" {
			seenSkillMD++
			body = string(f.Content)
		}
	}
	// The same path may appear only once; otherwise two write commands overwrite each other and the
	// result depends on order.
	if seenSkillMD != 1 {
		t.Errorf("SKILL.md 只能出现一次（重复会让最终内容取决于写入顺序），实际 %d 次", seenSkillMD)
	}
	if body != uploaded {
		t.Errorf("上传包自带的 SKILL.md 优先。\n期望 %q\n实际 %q", uploaded, truncateForMsg(body))
	}
	// The attachment must be there too.
	if len(bundles[0].Files) != 2 {
		t.Errorf("SKILL.md + 1 个附件 = 2 个文件，实际 %d 个", len(bundles[0].Files))
	}
}

// TestMixedSkillsAllLand covers a real setup: skills with attachments and built-in skills with no
// attachments bound together.
//
// The old continue only dropped the latter, so with a mix it showed up as **partly visible**.
// That kind of partial failure is harder to debug than a full failure, because "some skills work"
// leads people to suspect the skill itself.
func TestMixedSkillsAllLand(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newChatService(t)
	withFiles := seedSkillWithFiles(t, svc, store)
	bindSkills(t, store, seedChatAgent, withFiles, BuiltinSkillIDAMA, BuiltinSkillIDLarkCLI)

	run, err := svc.CreateConversation(ctx, seedWS, seedChatAgent)
	if err != nil {
		t.Fatalf("开对话失败：%v", err)
	}
	bundles, err := NewSkillFileLoader(svc, nil).SkillsFor(ctx, run.ID)
	if err != nil {
		t.Fatalf("SkillsFor 失败：%v", err)
	}
	if len(bundles) != 3 {
		names := make([]string, 0, len(bundles))
		for _, b := range bundles {
			names = append(names, b.Name)
		}
		t.Fatalf("绑了 3 个技能就应当有 3 个目录，实际 %d 个：%v —— "+
			"少的那些在技能目录下不存在", len(bundles), names)
	}
	// Every bundle has at least SKILL.md, otherwise that directory is empty.
	for _, b := range bundles {
		var has bool
		for _, f := range b.Files {
			if f.Path == "SKILL.md" {
				has = true
			}
		}
		if !has {
			t.Errorf("技能 %q 没有 SKILL.md，容器里那个目录会是空的", b.Name)
		}
	}
}

// skillMDKeys only serves error messages (the name keysOf is already taken by skillpkg_test.go).
func skillMDKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func truncateForMsg(s string) string {
	if len(s) <= 200 {
		return s
	}
	return s[:200] + "…"
}

// ⭐⭐ **The hot path that injects into the container must get the bytes from object storage.**
//
// This is the call site that "switching the read path to object storage" really serves:
// SkillFileContent is used by the page, while this one runs on every session. Both use the same
// fill-in function, but **the entry points differ**: skillinject.go used to call
// `l.svc.store.SkillFiles` directly (bypassing Service), so the switch did nothing there, with
// zero errors: the container got the old bytes from the content column.
//
// So this test checks the bytes in bundles, not "was the fill-in function called".
func TestSkillsForReadsBytesFromBlobStore(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newChatService(t)
	blobs := newFakeBlobStore()
	svc.blobs = blobs

	skills, err := store.ListSkills(ctx, seedWS)
	if err != nil || len(skills) == 0 {
		t.Fatalf("种子里应当有技能：%v", err)
	}
	sk := skills[0]

	// Build an attachment "already moved to object storage": the DB has a file_id, object storage
	// has the bytes, and the content column copy is **deliberately different from object
	// storage**, so "which side was read" is observable.
	const inBlob = "FROM-BLOB-HOTPATH"
	const inDB = "STALE-DB-COPY"
	const fileID = "file_hotpath"
	const key = "skill-files/2026/09/file_hotpath"

	if err := blobs.Put(ctx, key, []byte(inBlob), ""); err != nil {
		t.Fatalf("写假对象失败：%v", err)
	}
	if err := store.CreateFile(ctx, persistmodel.FileRecord{
		ID: fileID, Bucket: blobs.Bucket(), ObjectKey: key,
		Filename: "references/x.md", Size: len(inBlob), CreatedAt: apimodel.NowISO(),
	}); err != nil {
		t.Fatalf("写 files 记录失败：%v", err)
	}
	if err := store.PutSkillFiles(ctx, sk.ID, seedWS, []apimodel.SkillFile{
		{Path: "references/x.md", Content: []byte(inDB), FileID: fileID},
	}); err != nil {
		t.Fatalf("写附件失败：%v", err)
	}
	sk.Files = []apimodel.SkillFile{{Path: "references/x.md", Size: len(inDB)}}
	if _, err := store.SaveSkill(ctx, sk); err != nil {
		t.Fatalf("存技能失败：%v", err)
	}
	bindSkills(t, store, seedChatAgent, sk.ID)

	run, err := svc.CreateConversation(ctx, seedWS, seedChatAgent)
	if err != nil {
		t.Fatalf("开对话失败：%v", err)
	}
	bundles, err := NewSkillFileLoader(svc, nil).SkillsFor(ctx, run.ID)
	if err != nil {
		t.Fatalf("SkillsFor 失败：%v", err)
	}

	var got string
	var found bool
	for _, b := range bundles {
		for _, f := range b.Files {
			if f.Path == "references/x.md" {
				got, found = string(f.Content), true
			}
		}
	}
	if !found {
		t.Fatalf("注入的文件里没有 references/x.md")
	}
	if got == inDB {
		t.Fatalf("注入容器的是 content 列那份旧字节——热路径绕过了对象存储补齐" +
			"（skillinject.go 别直接调 store.SkillFiles）")
	}
	if got != inBlob {
		t.Fatalf("注入的字节既不是对象存储那份也不是库里那份：%q", got)
	}
}

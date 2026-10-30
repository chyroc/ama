// Tests for packaging and fingerprints.
//
// # The core assertion is "a package built back out can be read back by our own parser"
//
// BuildSkillZip and ParseSkillPackage (skillpkg.go) are inverse operations: the former turns a Skill
// into a zip, the latter turns a zip back into a SkillPackage. Running them end to end is much
// stronger than asserting the zip's internal structure field by field, because a mistake in any of
// the three (frontmatter assembly, YAML escaping, stripping the top-level directory) means it
// cannot be read back to the original.
//
// It also covers "parsing on the Ark side": Ark itself parses name and description out of
// SKILL.md, and our parser follows the same convention (YAML frontmatter starting with `---`).
package skill

import (
	"archive/zip"
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
)

// zipSkill is a skill for testing, **with every field's value different from the others**.
//
// If values were shared, an implementation bug like "writing description as name" could also pass
// the assertions (the rule that every field of a mutation fixture must differ). The description
// deliberately has colons and quotes, specifically to exercise YAML escaping.
func zipSkill() apimodel.Skill {
	return apimodel.Skill{
		ID:            "skill_1",
		WorkspaceID:   "ws_demo",
		Name:          "ticket-triage",
		DisplayTitle:  "工单分诊",
		Description:   `当用户反馈问题时使用：判定级别、归类模块，含"引号"与 \ 反斜杠`,
		Instructions:  "# 工单分诊\n\n1. 读完整条反馈\n2. 判定严重级别\n",
		Source:        apimodel.SkillCustom,
		LatestVersion: "3",
		Tags:          []string{"客服"},
	}
}

func zipFiles() []apimodel.SkillFile {
	return []apimodel.SkillFile{
		// Passed in deliberately out of order, to verify packaging sorts them.
		{Path: "scripts/init.sh", Size: 8, Content: []byte("echo one")},
		{Path: "references/rules.md", Size: 10, Content: []byte("# 规则\n")},
	}
}

// TestBuildSkillZipRoundTrip is the core case of this file: the built package can be read back as
// is by our own parser.
//
// Mutation testing:
//   - remove the frontmatter in skillMarkdown → parsing reports "missing frontmatter", the case fails.
//   - change yamlScalar to write bare (no quotes) → the description has a colon, YAML parsing fails.
//   - remove the top-level directory (write SKILL.md and attachments at the root) → the attachment
//     path assertions fail.
func TestBuildSkillZipRoundTrip(t *testing.T) {
	sk := zipSkill()
	blob, err := BuildSkillZip(sk, zipFiles())
	if err != nil {
		t.Fatalf("打包应当成功：%v", err)
	}

	// Read it back with our own parser; it follows the same SKILL.md convention as Ark.
	pkg, err := ParseSkillPackage("ticket-triage.zip", blob.Zip)
	if err != nil {
		t.Fatalf("打出来的包必须能被自己的解析器读回来：%v", err)
	}

	if pkg.Name != sk.Name {
		t.Errorf("name 变了：打包前 %q，读回 %q", sk.Name, pkg.Name)
	}
	// The description has a colon, double quotes and a backslash; this one is specifically for YAML escaping.
	if pkg.Description != sk.Description {
		t.Errorf("description 变了（YAML 转义写错了？）：\n打包前 %q\n读回   %q",
			sk.Description, pkg.Description)
	}
	if pkg.Instructions != sk.Instructions {
		t.Errorf("正文变了：\n打包前 %q\n读回   %q", sk.Instructions, pkg.Instructions)
	}

	// Attachments: the top-level directory must be stripped, and contents must match byte for byte.
	if len(pkg.Files) != 2 {
		t.Fatalf("应当读回 2 个附件，实际 %d 个：%+v", len(pkg.Files), pkg.Files)
	}
	got := map[string]string{}
	for _, f := range pkg.Files {
		got[f.Path] = string(f.Content)
	}
	for _, want := range zipFiles() {
		if got[want.Path] != string(want.Content) {
			t.Errorf("附件 %s 对不上：期望 %q，实际 %q",
				want.Path, want.Content, got[want.Path])
		}
	}
}

// TestBuildSkillZipHasSingleTopLevelDir pins "exactly one top-level directory".
//
// This is not about looks: on the SDK worker side, envinit.installSourceDir only strips that level
// when **there is exactly one top-level entry after unzipping and it is a directory**; otherwise it
// lays the flat contents out as is. With more than one top-level entry, the installed directory
// structure differs from what is expected, **with no error**.
func TestBuildSkillZipHasSingleTopLevelDir(t *testing.T) {
	blob, err := BuildSkillZip(zipSkill(), zipFiles())
	if err != nil {
		t.Fatalf("打包失败：%v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(blob.Zip), int64(len(blob.Zip)))
	if err != nil {
		t.Fatalf("打出来的东西不是合法 zip：%v", err)
	}

	tops := map[string]struct{}{}
	for _, f := range zr.File {
		name := f.Name
		idx := strings.Index(name, "/")
		if idx < 0 {
			t.Errorf("条目 %q 直接在根上，破坏了「单一顶层目录」", name)
			continue
		}
		tops[name[:idx]] = struct{}{}
	}
	if len(tops) != 1 {
		t.Errorf("顶层目录应当恰好一个，实际 %d 个：%v", len(tops), tops)
	}
	if _, ok := tops["ticket-triage"]; !ok {
		t.Errorf("顶层目录应当是技能名，实际 %v", tops)
	}

	// SKILL.md must be a direct child of the top-level directory (Ark's convention).
	var found bool
	for _, f := range zr.File {
		if f.Name == "ticket-triage/SKILL.md" {
			found = true
		}
	}
	if !found {
		t.Error("SKILL.md 应当在 ticket-triage/ 的直接子级")
	}
}

// TestBuildSkillZipIsDeterministic pins "the same input produces the same bytes".
//
// The fingerprint and "whether to re-upload" both rest on this: if packaging were nondeterministic
// (for example attachment order changing with map iteration), the same skill would compute
// something different every time, so every conversation would re-upload to Ark, and each re-upload
// leaves a skill on the Ark side that can never be cleaned up.
func TestBuildSkillZipIsDeterministic(t *testing.T) {
	sk := zipSkill()
	a, err := BuildSkillZip(sk, zipFiles())
	if err != nil {
		t.Fatalf("打包失败：%v", err)
	}
	// Pass the attachments in reverse once; the result must be the same.
	reversed := zipFiles()
	reversed[0], reversed[1] = reversed[1], reversed[0]
	b, err := BuildSkillZip(sk, reversed)
	if err != nil {
		t.Fatalf("打包失败：%v", err)
	}
	if !bytes.Equal(a.Zip, b.Zip) {
		t.Error("附件顺序不该影响打包结果（否则指纹会白白变化，每次都重传）")
	}
}

// TestBuildSkillZipRejectsUnsafe is a defense-in-depth case.
//
// Mutation testing: make safeArkZipPath always true → every subcase fails.
func TestBuildSkillZipRejectsUnsafe(t *testing.T) {
	t.Run("技能名不合法", func(t *testing.T) {
		for _, name := range []string{"", "../evil", "Has Upper", "semi;colon"} {
			sk := zipSkill()
			sk.Name = name
			if _, err := BuildSkillZip(sk, nil); err == nil {
				t.Errorf("技能名 %q 应当被拒", name)
			}
		}
	})

	t.Run("附件路径不安全", func(t *testing.T) {
		for _, p := range []string{"../../etc/passwd", "/etc/passwd", `..\evil`, "a\necho x"} {
			files := []apimodel.SkillFile{{Path: p, Content: []byte("x")}}
			if _, err := BuildSkillZip(zipSkill(), files); err == nil {
				t.Errorf("附件路径 %q 应当被拒", p)
			}
		}
	})
}

// TestSkillFingerprintSensitivity pins that the fingerprint "changes when it should and does not
// when it should not".
//
// Both halves matter:
//   - should change but does not → the body changed but is not re-uploaded, the model works from old
//     content, a **silent error**.
//   - should not change but does → every unrelated edit re-uploads, piling up skills on the Ark side
//     that can never be cleaned up.
//
// Mutation testing: remove Instructions from SkillFingerprint → the "change the body" subcase fails.
func TestSkillFingerprintSensitivity(t *testing.T) {
	sk := zipSkill()
	files := zipFiles()
	base := SkillFingerprint(sk, files)

	t.Run("同样的输入指纹相同", func(t *testing.T) {
		if again := SkillFingerprint(zipSkill(), zipFiles()); again != base {
			t.Error("同样的输入必须得到同样的指纹")
		}
	})

	t.Run("附件顺序不影响指纹", func(t *testing.T) {
		rev := zipFiles()
		rev[0], rev[1] = rev[1], rev[0]
		if SkillFingerprint(sk, rev) != base {
			t.Error("附件顺序不该影响指纹")
		}
	})

	// Should change: these four all change what the Ark side sees.
	changed := []struct {
		name  string
		mutut func(*apimodel.Skill, *[]apimodel.SkillFile)
	}{
		{"改正文", func(s *apimodel.Skill, _ *[]apimodel.SkillFile) {
			s.Instructions += "\n3. 新增一步\n"
		}},
		{"改描述", func(s *apimodel.Skill, _ *[]apimodel.SkillFile) {
			s.Description = "换了个完全不同的描述"
		}},
		{"改标识", func(s *apimodel.Skill, _ *[]apimodel.SkillFile) {
			s.Name = "another-name"
		}},
		{"改附件内容", func(_ *apimodel.Skill, f *[]apimodel.SkillFile) {
			(*f)[0].Content = []byte("echo changed")
		}},
		{"加附件", func(_ *apimodel.Skill, f *[]apimodel.SkillFile) {
			*f = append(*f, apimodel.SkillFile{Path: "extra.md", Content: []byte("x")})
		}},
		{"删附件", func(_ *apimodel.Skill, f *[]apimodel.SkillFile) {
			*f = (*f)[:1]
		}},
		{"改附件路径", func(_ *apimodel.Skill, f *[]apimodel.SkillFile) {
			(*f)[0].Path = "scripts/renamed.sh"
		}},
	}
	for _, tc := range changed {
		t.Run(tc.name+"→指纹要变", func(t *testing.T) {
			s, f := zipSkill(), zipFiles()
			tc.mutut(&s, &f)
			if SkillFingerprint(s, f) == base {
				t.Errorf("%s 之后指纹必须变（否则改了内容却不重传，模型按旧的干活）", tc.name)
			}
		})
	}

	// Should not change: these are invisible on the Ark side or do not affect model behavior.
	unchanged := []struct {
		name  string
		mutut func(*apimodel.Skill)
	}{
		{"改 tags", func(s *apimodel.Skill) { s.Tags = []string{"完全不同的标签"} }},
		{"改 updatedAt", func(s *apimodel.Skill) { s.UpdatedAt = "2099-01-01T00:00:00.000Z" }},
		{"改 latestVersion", func(s *apimodel.Skill) { s.LatestVersion = "999" }},
		{"改 displayTitle", func(s *apimodel.Skill) { s.DisplayTitle = "换个展示名" }},
	}
	for _, tc := range unchanged {
		t.Run(tc.name+"→指纹不该变", func(t *testing.T) {
			s := zipSkill()
			tc.mutut(&s)
			if SkillFingerprint(s, zipFiles()) != base {
				t.Errorf("%s 不该改变指纹（否则每次无关编辑都往方舟重传一份）", tc.name)
			}
		})
	}
}

// TestSkillFingerprintNoConcatAmbiguity pins "write the length of each segment".
//
// Concatenating directly without lengths, moving the last character of the description to the start
// of the body would give the same hash; that is a bug that can silently lose changes.
//
// Mutation testing: remove the `%d:` prefix in writeChunk → this case fails.
func TestSkillFingerprintNoConcatAmbiguity(t *testing.T) {
	a := apimodel.Skill{Name: "n", Description: "ab", Instructions: "c"}
	b := apimodel.Skill{Name: "n", Description: "a", Instructions: "bc"}
	if SkillFingerprint(a, nil) == SkillFingerprint(b, nil) {
		t.Error("拼接歧义：('ab','c') 与 ('a','bc') 算出了同一个指纹")
	}
}

// TestSkillsFingerprint pins the fingerprint semantics of a whole set of skills.
func TestSkillsFingerprint(t *testing.T) {
	t.Run("空集合是空串", func(t *testing.T) {
		// "Never bound" and "bound then all unbound" expect the same Ark-side state (empty skills),
		// so the fingerprints should be the same too.
		if got := SkillsFingerprint(nil); got != "" {
			t.Errorf("空集合应当是空串，实际 %q", got)
		}
		if got := SkillsFingerprint(map[string]string{}); got != "" {
			t.Errorf("空 map 应当是空串，实际 %q", got)
		}
	})

	t.Run("顺序无关", func(t *testing.T) {
		// A map itself is unordered, but we need to confirm the implementation really sorts; without
		// sorting the same bindings could compute different fingerprints each time, so every
		// conversation would call Ark UpdateAgent for nothing.
		a := SkillsFingerprint(map[string]string{"s1": "h1", "s2": "h2"})
		b := SkillsFingerprint(map[string]string{"s2": "h2", "s1": "h1"})
		if a != b {
			t.Error("绑定顺序不该影响整组指纹")
		}
	})

	t.Run("成员或内容变了指纹要变", func(t *testing.T) {
		base := SkillsFingerprint(map[string]string{"s1": "h1", "s2": "h2"})
		cases := map[string]map[string]string{
			"少一个":    {"s1": "h1"},
			"多一个":    {"s1": "h1", "s2": "h2", "s3": "h3"},
			"某个内容变了": {"s1": "h1", "s2": "CHANGED"},
			"换了个技能":  {"s1": "h1", "s9": "h2"},
		}
		for name, m := range cases {
			if SkillsFingerprint(m) == base {
				t.Errorf("%s 之后整组指纹必须变", name)
			}
		}
	})
}

// TestSkillMarkdownEscapesYAML pins escaping on its own, because it is the easiest thing to
// "simplify" away.
//
// Writing `description: <user text>` bare makes YAML parsing fail or produce an unexpected
// structure when the description has a colon, and that failure happens **on the Ark side**, in
// English, and does not point back to this upload.
func TestSkillMarkdownEscapesYAML(t *testing.T) {
	sk := apimodel.Skill{
		Name:         "x-y",
		Description:  "含冒号：还有 #井号 与 \"引号\"",
		Instructions: "# 正文\n",
	}
	md := string(skillMarkdown(sk))
	if !strings.HasPrefix(md, "---\n") {
		t.Fatalf("必须以 frontmatter 起始，实际：%q", md)
	}
	// The description line must be in the quoted form.
	if !strings.Contains(md, `description: "`) {
		t.Errorf("description 必须用引号包起来，实际：%q", md)
	}
	// It must decode back exactly the same.
	pkg, err := ParseSkillPackage("SKILL.md", []byte(md))
	if err != nil {
		t.Fatalf("带特殊字符的 SKILL.md 必须能解析：%v", err)
	}
	if pkg.Description != sk.Description {
		t.Errorf("描述往回解不一致：\n期望 %q\n实际 %q", sk.Description, pkg.Description)
	}
}

// ---------------------------------------------------------------------------
// Degrading when Ark's limits are exceeded
// ---------------------------------------------------------------------------

// TestBuildSkillZipDegradesWhenTooManyFiles pins "only package SKILL.md when the file count exceeds
// Ark's limit".
//
// # Why degrade instead of erroring
//
// Our own limit (10000 files) is looser than Ark's (500), on purpose: the container side downloads
// directly from object storage and is not bound by Ark. So "fits for us but cannot be uploaded to
// Ark" is **an expected normal case**.
//
// This used to fail the whole package, and arkSkillsFor only logs one Warn when a single skill
// fails; the symptom was "the skill is checked, the model knows nothing about it", with no error.
// Degrading is strictly better: the model at least knows the skill exists and what it does, and
// the attachments still go into the container.
func TestBuildSkillZipDegradesWhenTooManyFiles(t *testing.T) {
	files := make([]apimodel.SkillFile, arkSkillMaxFiles+1)
	for i := range files {
		files[i] = apimodel.SkillFile{
			Path:    fmt.Sprintf("references/doc-%04d.md", i),
			Content: []byte("x"),
		}
	}

	got, err := BuildSkillZip(zipSkill(), files)
	if err != nil {
		t.Fatalf("超上限应当降级而不是报错：%v", err)
	}
	if !got.Degraded {
		t.Fatal("应当标记为已降级——不标的话「控制台上怎么没附件」查不出原因")
	}
	if !strings.Contains(got.Reason, "超过方舟上限") {
		t.Errorf("降级原因要说清是什么超了，实际 %q", got.Reason)
	}
	assertOnlySkillMD(t, got.Zip)
}

// TestBuildSkillZipDegradesWhenSingleFileTooBig pins that a single file over 25 MiB also degrades.
//
// **This needs its own test**: neither the total nor the file count is over, only one file is.
// Checking only the total would let this package through, and then Ark returns an English
// InvalidParameter, showing up as "this agent cannot open a conversation", not pointing at "one
// attachment is too big" at all.
func TestBuildSkillZipDegradesWhenSingleFileTooBig(t *testing.T) {
	files := []apimodel.SkillFile{
		{Path: "small.md", Content: []byte("x")},
		{Path: "huge.bin", Content: make([]byte, arkSkillMaxFileBytes+1)},
	}

	got, err := BuildSkillZip(zipSkill(), files)
	if err != nil {
		t.Fatalf("超上限应当降级而不是报错：%v", err)
	}
	if !got.Degraded {
		t.Fatal("单文件超方舟上限也要降级")
	}
	if !strings.Contains(got.Reason, "huge.bin") {
		t.Errorf("降级原因要点名是哪个文件，实际 %q", got.Reason)
	}
	assertOnlySkillMD(t, got.Zip)
}

// TestBuildSkillZipDegradesAllOrNothing pins that degrading means "not a single attachment".
//
// **Not "pick a few small ones"**: that would give the Ark side a package that looks complete but is
// missing files, and when the model reads an attachment per SKILL.md, that file does not exist on the
// Ark side. Either all of them or only the body; the same reasoning as hydrateSkillFiles' "never hand
// out an incomplete skill".
func TestBuildSkillZipDegradesAllOrNothing(t *testing.T) {
	files := []apimodel.SkillFile{
		{Path: "tiny.md", Content: []byte("小到不能再小")},
		{Path: "huge.bin", Content: make([]byte, arkSkillMaxFileBytes+1)},
	}

	got, err := BuildSkillZip(zipSkill(), files)
	if err != nil {
		t.Fatalf("打包失败：%v", err)
	}
	// Even the "as small as it gets" file must not be in the package.
	if bytes.Contains(got.Zip, []byte("tiny.md")) {
		t.Error("降级时连小附件都不该带——半截的技能比只有正文更坏")
	}
}

// TestBuildSkillZipDoesNotDegradeNormalSkill is the **negative control**: a normal skill must not be
// degraded.
//
// Without it, "always set Degraded to true" would also make the three above all green.
func TestBuildSkillZipDoesNotDegradeNormalSkill(t *testing.T) {
	got, err := BuildSkillZip(zipSkill(), zipFiles())
	if err != nil {
		t.Fatalf("打包失败：%v", err)
	}
	if got.Degraded {
		t.Errorf("正常技能不该降级，原因说的是 %q", got.Reason)
	}
	if got.Reason != "" {
		t.Errorf("没降级时 Reason 应当为空，实际 %q", got.Reason)
	}
	// The attachments must really be there.
	if !bytes.Contains(got.Zip, []byte("scripts/init.sh")) {
		t.Error("正常技能的附件必须进包")
	}
}

// assertOnlySkillMD asserts the zip has **exactly** one SKILL.md and nothing else.
func assertOnlySkillMD(t *testing.T, blob []byte) {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(blob), int64(len(blob)))
	if err != nil {
		t.Fatalf("降级产出的不是合法 zip：%v", err)
	}
	if len(zr.File) != 1 {
		names := make([]string, 0, len(zr.File))
		for _, f := range zr.File {
			names = append(names, f.Name)
		}
		t.Fatalf("降级后应当只有 1 个条目，实际 %d 个：%v", len(zr.File), names)
	}
	if !strings.HasSuffix(zr.File[0].Name, "/"+SkillFileName) {
		t.Errorf("唯一那个条目应当是 SKILL.md，实际 %q", zr.File[0].Name)
	}
}

// TestSkillsFingerprintCarriesVersionPrefix pins that the fingerprint carries the version prefix.
//
// # Why this needs its own case
//
// The prefix is for **curing existing data**: the version that forgot to pass skills left a batch of
// fingerprints that are "computed correctly, but claim something that did not happen" (the Ark side
// has empty skills, while the record says N are attached). Changing the prefix makes those existing
// fingerprints mismatch once, so each binding goes through UpdateAgentSkills once on its next
// conversation: it heals exactly once.
//
// Without this case, someone "cleaning up" this useless-looking prefix would meet no resistance, and
// the result would be that the affected agents in production never heal, with no error.
func TestSkillsFingerprintCarriesVersionPrefix(t *testing.T) {
	fp := SkillsFingerprint(map[string]string{"s1": "h1"})
	if !strings.HasPrefix(fp, skillsFingerprintVersion) {
		t.Errorf("指纹要带版本前缀 %q（治存量毒化指纹用的，见 skillzip.go），实际 %q",
			skillsFingerprintVersion, fp)
	}
	// The old format (bare hex, no prefix) must not equal the new format; that is exactly what "existing fingerprints will mismatch" means.
	bare := strings.TrimPrefix(fp, skillsFingerprintVersion)
	if bare == fp {
		t.Fatal("前缀没起作用")
	}
	if SkillsFingerprint(map[string]string{"s1": "h1"}) == bare {
		t.Error("新指纹不能等于旧格式的裸 hex，否则存量毒化指纹不会失配、永远不自愈")
	}

	// The empty set is still an empty string (no prefix): it says "the Ark side should have no
	// skills", and that state was true in the old version too, so it needs no healing.
	if got := SkillsFingerprint(nil); got != "" {
		t.Errorf("空集合应当是空串（不带前缀），实际 %q", got)
	}
}

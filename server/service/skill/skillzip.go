// Packages an AMA skill into a zip Ark accepts, and computes a content fingerprint for it.
//
// # Why the frontmatter is rebuilt
//
// When an uploaded package is parsed, the frontmatter is stripped (parseFrontmatter in
// `skillpkg.go`: name and description go into fields, the body into Instructions). But Ark expects
// **the complete SKILL.md**: it parses name and description out of it itself. So when packaging back,
// those two lines must be written back, or Ark gets a body with no frontmatter and parses an empty
// name.
//
// Hand-filled skills never had frontmatter (users filled the fields in a form), and they take this
// path too; for them it is not "restoring" but "generating for the first time". There is no
// difference between the two here, which is exactly the benefit of treating Instructions as "pure
// body".
//
// # Why there must be a top-level directory
//
// Ark recommends `foo.zip → foo/SKILL.md` (see the Skills article in docs/ark-managed-agents). The
// harder reason is on the SDK worker side: `envinit.installSourceDir` only strips that level when
// **there is exactly one top-level entry after unzipping and it is a directory**
// (`initializer.go:385-395`); otherwise it lays the flat contents out as is. So there must be exactly
// one top-level directory, or the installed directory structure differs from what we expect, and
// that difference gives no error.
//
// # The fingerprint decides "whether to upload again"
//
// Ark **has no "update skill" API**: every `CreateSkill` call produces a new skill_id
// (`api/06-Skills/` only has two articles, create and get details). So "the skill changed" must be
// judged by us, and the only valid basis is **what really affects the content on the Ark side**; see
// the SkillFingerprint comment.
package skill

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/shared/gopkg/humanize"
)

// arkSkillZipLimit is Ark's size limit for a zip package (50 MiB; see the official Skills docs).
//
// ⚠️ **This went from "a second gate never hit" to a check that really triggers.** Our own limits
// used to be much stricter (256 KiB per file, 2 MiB total), so normally it was never hit. Now ours
// are 100 MiB total / 25 MiB per file (skillpkg.go; after the injection channel changed, no longer
// limited by the shell), **looser than Ark's**; so exceeding this is an expected normal case, not
// an anomaly.
//
// Hitting it **no longer fails the whole package**: it takes the "only upload SKILL.md to Ark"
// fallback (see skillark.go), and attachments still go into the container. So today this constant
// decides "whether to degrade", not "reject".
const arkSkillZipLimit = 50 << 20

// Ark's three hard limits on skill packages; see the Skills article in docs/ark-managed-agents.
//
// ⚠️ **These three are external and do not follow our limits.** Ours are already 100 MiB / 10000
// files / 25 MiB per file (skillpkg.go), looser than Ark's; the part beyond is handled by degrading
// (uploading only SKILL.md), not by rejecting.
const (
	// arkSkillMaxFiles is the file count limit for one version.
	arkSkillMaxFiles = 500
	// arkSkillMaxFileBytes is the limit for a single file after unzipping.
	arkSkillMaxFileBytes = 25 << 20
)

// SkillZipResult is the result of packaging, including whether it was degraded.
//
// # Why degrading is reported instead of quietly building a smaller package
//
// Degrading means **the Ark side and the container side disagree**: the model only sees SKILL.md,
// while the container has all attachments. That is a deliberately accepted trade-off, but **the
// caller must know**: it logs based on this. Degrading quietly would make "why does this skill have
// no attachments in the Ark console" impossible to trace.
type SkillZipResult struct {
	// Zip is the built package.
	Zip []byte
	// Degraded true means **only SKILL.md was packaged**; attachments are not in this package.
	Degraded bool
	// Reason is why it was degraded, in plain words, for logs. Empty when Degraded is false.
	Reason string
}

// arkDegradeReason decides whether a skill should be degraded and returns the reason (an empty
// string when it should not).
//
// **Decided before packaging, not after**: attachments may total tens of MB, finding out it is over
// the limit after packaging would mean compressing for nothing, and this path may run on every
// conversation.
//
// ⚠️ All three checks must be made, not just the total: a package of 500 small files may total only
// a few MB and still fail Ark's file count limit, and then Ark returns an English
// InvalidParameter, showing up as "this agent cannot open a conversation".
func arkDegradeReason(files []apimodel.SkillFile) string {
	if len(files) > arkSkillMaxFiles {
		return fmt.Sprintf("附件 %d 个，超过方舟上限 %d 个", len(files), arkSkillMaxFiles)
	}
	total := 0
	for _, f := range files {
		if len(f.Content) > arkSkillMaxFileBytes {
			return fmt.Sprintf("附件 %s 有 %s，超过方舟单文件上限 %s",
				f.Path, humanize.HumanBytes(len(f.Content)), humanize.HumanBytes(arkSkillMaxFileBytes))
		}
		total += len(f.Content)
	}
	// Comparing the uncompressed total against the zip limit is **deliberately conservative**:
	// compression usually shrinks it a lot, so some packages that would fit might be degraded here.
	// The other way (package first, then check) costs compressing tens of MB for nothing, and
	// degrading itself is lossless (attachments still go into the container), so conservative it is.
	if total > arkSkillZipLimit {
		return fmt.Sprintf("附件总量 %s，超过方舟包上限 %s",
			humanize.HumanBytes(total), humanize.HumanBytes(arkSkillZipLimit))
	}
	return ""
}

// BuildSkillZip packages a skill into a zip Ark accepts.
//
// files must carry content bytes (the kind `Store.SkillFiles` returns), not the path+size-only
// metadata in doc: with that, every attachment in the package would be 0 bytes, and since Ark does
// not validate the format, **there would be no error**; the agent would just read a pile of empty
// files.
//
// # Beyond Ark's limits it **degrades instead of failing**
//
// Our own limits are looser than Ark's (100 MiB vs 50 MiB, 10000 files vs 500), so "fits for us but
// cannot be uploaded to Ark" is an expected normal case. Then only SKILL.md is packaged and
// uploaded:
//
//	Ark side        only SKILL.md       → the model **knows** the skill exists and what it does
//	container side  full attachments    → tools **can read** those bytes (downloaded directly via an object storage presigned URL)
//
// The two paths are separate to begin with (see the top of skillark.go), so degrading only loses
// one thing: the skill looks incomplete in the Ark console. **This is not a bug**, and the model can
// still use it normally: the paths written in SKILL.md all really exist in the container.
//
// ⚠️ This used to **reject the whole package** (returning persistmodel.ErrInvalidArgument), and
// arkSkillsFor only logs one Warn when a single skill fails; the symptom was "the skill is checked,
// the model knows nothing about it", with no error. Degrading is strictly better than that.
//
// ⚠️⚠️ **This function only serves Ark. The container side must use BuildFullSkillZip.** They were
// mixed up once; see that function's comment for the consequences.
func BuildSkillZip(sk apimodel.Skill, files []apimodel.SkillFile) (SkillZipResult, error) {
	return buildSkillZip(sk, files, true)
}

// BuildFullSkillZip builds a **complete** skill package, ignoring Ark's three limits.
//
// # Why this must exist instead of reusing BuildSkillZip
//
// The two consumers have **fundamentally different** constraints, and they once shared a function,
// so Ark's limits hurt the container side along the call chain:
//
//	Ark        50 MiB / 500 files / 25 MiB per file: an external hard limit; beyond it the only option is degrading
//	container  **no limit**: it is exactly the end that needs the complete skill (downloaded directly
//	           from object storage, independent of command line length; that is the whole point of
//	           this round's channel change)
//
// ⚠️ **This bug has already happened, with no error.** `ensureSkillArchive` called BuildSkillZip to
// get the container package and errored when it saw `Degraded` (the intent, "the container should
// not use a degraded package", was right), so **any skill beyond Ark's limits could not get a
// container package**, and fell back to base64 per-file injection. And base64 is exactly what cannot
// fit large packages: estimated from measurements, 60 MiB takes 853 commands, about 12.8 minutes,
// and 100 MiB takes 21 minutes.
//
// Net effect: **"raising the skill package limit to 100 MiB" was completely empty for everything
// above 25 MiB**; those skills could get into neither Ark (degraded, correct) nor the container (fell
// back to base64, sure to time out). The only symptom was one Warn saying "falling back to per-file
// injection", and then the agent waited until timeout.
//
// So from now on the two checks are separate: **packaging** always builds the complete package, and
// **whether to degrade** is only asked when giving it to Ark.
func BuildFullSkillZip(sk apimodel.Skill, files []apimodel.SkillFile) ([]byte, error) {
	res, err := buildSkillZip(sk, files, false)
	if err != nil {
		return nil, err
	}
	if res.Degraded {
		// Getting here means the applyArkLimits=false path degraded anyway; impossible unless
		// someone broke buildSkillZip. Better to error than to hand out an incomplete container
		// package: an incomplete skill is worse than a missing one (the same rule as
		// hydrateSkillFiles).
		return nil, fmt.Errorf("core: 内部错误：完整包不该被降级（%s）", res.Reason)
	}
	return res.Zip, nil
}

// buildSkillZip is the shared implementation of the two entry points.
//
// With applyArkLimits true it checks Ark's three limits for degrading (only SKILL.md is packaged);
// with false it always builds the complete package.
func buildSkillZip(
	sk apimodel.Skill, files []apimodel.SkillFile, applyArkLimits bool,
) (SkillZipResult, error) {
	root := strings.TrimSpace(sk.Name)
	if root == "" {
		return SkillZipResult{}, fmt.Errorf("%w: 技能标识为空，无法打包", persistmodel.ErrInvalidArgument)
	}
	// The skill name becomes the directory name in the zip, so path traversal must be blocked.
	// Upstream (apimodel.ValidSkillName) has already checked, and this is defense in depth: packaging
	// is the step that turns data into a file structure, and it should not rely on "the caller must
	// have validated".
	if !apimodel.ValidSkillName(root) {
		return SkillZipResult{}, fmt.Errorf(
			"%w: 技能标识 %q 不合法，无法作为目录名", persistmodel.ErrInvalidArgument, root)
	}

	var reason string
	if applyArkLimits {
		reason = arkDegradeReason(files)
	}
	if reason != "" {
		// Degraded: not a single attachment. **Not "pick a few small ones"**: that would give the Ark
		// side a skill that looks complete but is missing files, and when the model reads an
		// attachment per SKILL.md, that file would not exist in the Ark-side package. Either all of
		// them, or only the body.
		files = nil
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	// SKILL.md first: Ark requires exactly one, and having it as the first entry also makes it obvious when unzipped by hand.
	if err := writeZipFile(zw, path.Join(root, SkillFileName), skillMarkdown(sk)); err != nil {
		return SkillZipResult{}, err
	}

	// Attachments are written sorted by path, so the same input produces a **byte-for-byte identical**
	// zip. Unsorted, map iteration order would change the fingerprint for nothing, causing a re-upload
	// every time.
	sorted := make([]apimodel.SkillFile, len(files))
	copy(sorted, files)
	persistmodel.SortSkillFiles(sorted)

	for _, f := range sorted {
		rel, ok := safeArkZipPath(f.Path)
		if !ok {
			// The same defense in depth as on the shelltools side: the path has passed the Zip Slip
			// check, but it is checked again independently here. Unsafe ones **reject the whole
			// package** instead of being skipped: skipping would allow smuggling an entry we cannot see
			// inside a valid package.
			return SkillZipResult{}, fmt.Errorf(
				"%w: 附件路径 %q 不安全，无法打包", persistmodel.ErrInvalidArgument, f.Path)
		}
		if err := writeZipFile(zw, path.Join(root, rel), f.Content); err != nil {
			return SkillZipResult{}, err
		}
	}

	if err := zw.Close(); err != nil {
		return SkillZipResult{}, fmt.Errorf("core: 打技能包失败: %w", err)
	}
	if applyArkLimits && buf.Len() > arkSkillZipLimit {
		// Getting here means arkDegradeReason let it through but it is still over the limit after
		// compression. The uncompressed total was within the limit yet the compressed one is
		// larger, which can only mean the content itself does not compress (already compressed
		// images/binaries). **Degrade once more** instead of erroring: an error would make this skill
		// completely invisible to the model, a silent failure with no error.
		//
		// ⚠️ **This one must also respect applyArkLimits**: the container side has no 50 MiB limit,
		// and without this check the "complete package" would be degraded here again, exactly like
		// the bug already fixed.
		md, err := buildSkillMDOnlyZip(sk, root)
		if err != nil {
			return SkillZipResult{}, err
		}
		return SkillZipResult{
			Zip:      md,
			Degraded: true,
			Reason: fmt.Sprintf("打包后 %s 仍超过方舟上限 %s（附件多为不可压缩内容）",
				humanize.HumanBytes(buf.Len()), humanize.HumanBytes(arkSkillZipLimit)),
		}, nil
	}
	return SkillZipResult{Zip: buf.Bytes(), Degraded: reason != "", Reason: reason}, nil
}

// buildSkillMDOnlyZip builds a package with only SKILL.md, for the degrade path.
func buildSkillMDOnlyZip(sk apimodel.Skill, root string) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	if err := writeZipFile(zw, path.Join(root, SkillFileName), skillMarkdown(sk)); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("core: 打技能包失败: %w", err)
	}
	return buf.Bytes(), nil
}

// writeZipFile writes one file entry into the zip.
//
// Deflate rather than Store: skill packages are mostly text, which compresses very well, and it
// goes to Ark over HTTP.
func writeZipFile(zw *zip.Writer, name string, content []byte) error {
	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
	if err != nil {
		return fmt.Errorf("core: 建 zip 条目 %q 失败: %w", name, err)
	}
	if _, err := w.Write(content); err != nil {
		return fmt.Errorf("core: 写 zip 条目 %q 失败: %w", name, err)
	}
	return nil
}

// skillMarkdown restores a skill into a complete SKILL.md (frontmatter + body).
func skillMarkdown(sk apimodel.Skill) []byte {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("name: ")
	b.WriteString(yamlScalar(sk.Name))
	b.WriteString("\n")
	b.WriteString("description: ")
	b.WriteString(yamlScalar(sk.Description))
	b.WriteString("\n")
	b.WriteString("---\n\n")
	b.WriteString(sk.Instructions)
	// Add a newline at the end of the body: Markdown files end with a newline by convention, and
	// Instructions may have been typed by the user in a TextArea without a trailing newline.
	if !strings.HasSuffix(sk.Instructions, "\n") {
		b.WriteString("\n")
	}
	return []byte(b.String())
}

// yamlScalar encodes a string as a safe YAML scalar.
//
// **It must be quoted**, not written bare: description is free text from the user and very likely
// contains a colon (like "Use when a user reports a problem: decide the level…"). Written bare,
// `description: Use when…: decide` would be parsed by YAML as a nested map or be a plain syntax
// error, and that error happens **on the Ark side** when it parses SKILL.md, in English, and does
// not point back to this upload.
//
// Double quotes + escaping rather than single quotes: in single-quote form a newline cannot be
// expressed, and description could in theory contain newlines (the frontend uses a TextArea).
func yamlScalar(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// safeArkZipPath checks whether an attachment path can be safely put into a zip.
//
// Same rule as shelltools' safeSkillPath, and the duplicate implementation is on purpose: that one
// is for "writing into the container", this one for "packaging into a zip for Ark"; each path is the
// step that turns untrusted data into a file structure, and neither should rely on upstream.
func safeArkZipPath(p string) (string, bool) {
	if p == "" || len(p) > MaxSkillPathBytes {
		return "", false
	}
	// A backslash is treated as invalid rather than converted: the zip spec requires `/`, and we are
	// the **generating** side, with no reason to produce an entry name with a backslash.
	if strings.ContainsRune(p, '\\') {
		return "", false
	}
	if path.IsAbs(p) || hasDriveLetter(p) {
		return "", false
	}
	cleaned := path.Clean(p)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", false
	}
	if strings.ContainsAny(cleaned, "\x00\n") {
		return "", false
	}
	return cleaned, true
}

// SkillFingerprint computes a skill's content fingerprint, used to decide "whether to upload to Ark
// again".
//
// # Only what really affects the Ark-side content is included
//
// What is included is Name, Description, Instructions, and each attachment's path + content bytes:
// exactly what BuildSkillZip puts into the package.
//
// **UpdatedAt or LatestVersion are deliberately not used**: those two change on every save, even if
// the user only changed tags. Using them would make every unrelated edit re-upload for nothing, and
// each re-upload leaves one more skill on the Ark side that can never be cleaned up (there is no
// delete API).
//
// **Tags and DisplayTitle are deliberately excluded too**: tags are only used in the AMA UI and
// invisible on the Ark side; display_title is sent to Ark but only affects the display name in the
// console, not model behavior, and re-uploading for it is not worth it. The cost of this trade-off
// is "changed the display name, the Ark console still shows the old name", which is acceptable.
func SkillFingerprint(sk apimodel.Skill, files []apimodel.SkillFile) string {
	h := sha256.New()
	// Write the length before each segment, then the content, to avoid concatenation ambiguity:
	// without the length, ("ab", "c") and ("a", "bc") would give the same hash, so a change like
	// "move the last character of the description to the start of the body" would not be detected.
	writeChunk := func(s string) {
		_, _ = fmt.Fprintf(h, "%d:", len(s))
		_, _ = h.Write([]byte(s))
	}
	writeChunk(sk.Name)
	writeChunk(sk.Description)
	writeChunk(sk.Instructions)

	// Attachments sorted by path, so the same content always gets the same fingerprint (order should not affect the result).
	sorted := make([]apimodel.SkillFile, len(files))
	copy(sorted, files)
	persistmodel.SortSkillFiles(sorted)
	for _, f := range sorted {
		writeChunk(f.Path)
		_, _ = fmt.Fprintf(h, "%d:", len(f.Content))
		_, _ = h.Write(f.Content)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// skillsFingerprintVersion is the version prefix of the skills fingerprint.
//
// # Why it exists: to cure a batch of existing fingerprints "whose content is right but whose
// meaning is false"
//
// The fingerprint means "this set of skills is what is currently attached to the Ark Agent". In the
// version where CreateAgent forgot to pass Skills (see CreateAgent in arkrun.go), the binding record
// **unconditionally** wrote back the fingerprint after creating the Agent; so every binding created
// then stores a fingerprint that is "computed correctly, but claims something that did not happen":
// the Ark side has empty skills, while the record says N are attached.
//
// This false record blocks the repair path: syncBindingSkills returns right away when the
// fingerprints match, with zero network calls, so **just filling in the missing field would never
// heal agents already affected**.
//
// Changing the prefix makes every existing binding's fingerprint mismatch once, so the next
// conversation goes through UpdateAgentSkills once each (that path is correct, and writes back only
// after success): **it heals exactly once**, and afterwards hits the new fingerprint as usual with
// zero extra round trips.
//
// # Why not clear that column with an SQL migration
//
// The table creation script runs on every start (production has multiple instances and rolling
// deploys; every process runs it once), and an unconditional `update ark_bindings set doc = ...`
// would make **every agent call UpdateAgent for nothing after every deploy**; making it run only once
// would need yet another migration record table. The version prefix encodes "has it run" into the
// data itself, naturally idempotent, and does not touch DDL at all; it also avoids two migration
// traps this repo has recorded (information_schema DDL checks leaking across schemas, and
// jsonb_array_elements hitting JSON null and erroring with 22023).
//
// ⚠️ **Next time "the meaning of the fingerprint changes", change this prefix** (v2 → v3). The cost
// is one extra Ark round trip per agent on its next conversation, just once.
const skillsFingerprintVersion = "v2:"

// SkillsFingerprint computes the overall fingerprint of a set of skill references, used to decide
// whether the skills attached to the Ark Agent need updating.
//
// The input is (AMA skillID → that skill's content fingerprint). **Order does not matter**: it is
// sorted internally, so "reordering bindings" does not trigger a pointless Ark update.
//
// An empty set returns an empty string rather than some hash: that way "never bound any skill" and
// "bound then unbound all" are the same value in the binding record, and they really do expect the
// same Ark-side state (empty skills). **The empty string has no version prefix**: it says "the Ark
// side should have no skills", and that state was true in the old version too (no skill to forget),
// so it needs no healing.
func SkillsFingerprint(perSkill map[string]string) string {
	if len(perSkill) == 0 {
		return ""
	}
	ids := make([]string, 0, len(perSkill))
	for id := range perSkill {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	h := sha256.New()
	for _, id := range ids {
		_, _ = fmt.Fprintf(h, "%d:%s%d:%s", len(id), id, len(perSkill[id]), perSkill[id])
	}
	return skillsFingerprintVersion + hex.EncodeToString(h.Sum(nil))
}

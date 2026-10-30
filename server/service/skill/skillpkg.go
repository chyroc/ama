// Skill package parsing: turns a SKILL.md or zip uploaded by a user into an
// apimodel.SkillPackage waiting for confirmation.
//
// # This file handles untrusted input
//
// zip is the input format with the largest attack surface. Every limit and every path check in this
// file maps to a known class of attack, and deleting any of them would alert nothing except a test
// turning red; so every security case in skillpkg_test.go was mutation-tested (comment out the
// check, confirm the matching case fails). Four classes are guarded against:
//
//  1. **Zip Slip**: entry names like `../../etc/cron.d/x`. We do not unzip to disk, but the parsed
//     paths are eventually written into the container (see shared/infra/shelltools/skills.go), so
//     escaping works just the same. The rule is "after normalizing it is still a relative path and
//     does not start with ..", not a simple strings.Contains(".."); the latter would also reject the
//     legal `a..b.md`, and miss things like `a/../../b` that only show up after normalizing.
//  2. **zip bomb**: the header declares 1 KB and it actually unpacks 10 GB. **Never trust
//     FileHeader.UncompressedSize64** (a number written by the attacker); always count while
//     unpacking with io.LimitReader, and stop one byte over.
//  3. **Symlink entries**: zip can store symlinks, where the entry's "content" is the target path
//     string. We **dereference inside the package**: replace the symlink with the real bytes of the
//     target file, instead of following it to read the host. The target is resolved "relative to the
//     symlink's own directory", and after normalizing it must still land inside the package: escaping
//     the package, pointing to an absolute path, a missing target, or a cycle all reject the whole
//     package (this extends the Zip Slip defense; it is not an exception to it). Dereferencing makes
//     the total bigger (one symlink becomes a real copy of content), so the bytes swapped in are
//     counted again toward the total limit; see derefSkillSymlinks. "Is it a symlink" is always
//     decided by masking out the file type with S_IFMT first and then comparing for equality; see
//     isSymlinkEntry.
//  4. **Absolute path entries**: like `/etc/passwd`. Windows drive letters (`C:\`) count too,
//     because zip allows backslash separators, and path.Clean does not recognize them.
//
// # Where the limits come from
//
// ⚠️ **This section was rewritten entirely; do not reason from the old version.**
//
// The four limits used to be (10 MiB upload / 256 KiB per file / 2 MiB total / 64 entries), all
// derived from the **injection channel**: writing into the container had only one path, base64 +
// shell redirection, a single command limited by `MAX_ARG_STRLEN` (128 KiB), and base64 inflating by
// 4/3. That reasoning was right at the time.
//
// **Injection now works as "the container curls an object storage presigned URL directly"** (see
// shared/infra/shelltools/skills.go): bytes go straight from object storage to the container,
// no longer through the shell command line. So that constraint disappeared entirely, and the limits
// are now decided by two other things:
//
//   - **Ark's hard limits** (50 MiB package / 500 files / 25 MiB per file; see the Skills article in
//     docs/ark-managed-agents). ⚠️ They are external and **will not loosen with ours**. Exceeding
//     them no longer rejects the whole package; it degrades to "only upload SKILL.md to Ark"
//     (skillark.go), and attachments still go into the container.
//   - **How much we can move ourselves**: on upload the bytes pass through amad's memory once, plus
//     the cost of object storage.
//
// So today it is 100 MiB / 25 MiB per file / 10000 entries: looser than Ark's, on purpose: the
// container side gets the complete skill, and the Ark side is just missing the attachments.
package skill

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/shared/gopkg/humanize"
)

const (
	// MaxUploadBytes is the limit on the upload request body.
	//
	// **Must be set with http.MaxBytesReader before reading**, not checked after reading into
	// memory: then a 100 MiB limit means nothing for a 10 GiB request; the process would OOM first.
	//
	// ⚠️ It used to be 10 MiB. It was raised to 100 MiB because the injection channel changed (see
	// "where the limits come from" above); skill packages of 8 MiB+ unpacked / thousands of files
	// really exist and could not fit before.
	MaxUploadBytes = 100 << 20

	// MaxSkillFileBytes is the limit for a single file after unzipping.
	//
	// ⚠️ **This value is no longer decided by the injection channel.** It used to be 256 KiB, because
	// "after base64 it is about 341 KiB, still under shelltools' maxWriteBytes"; that reasoning fell
	// apart once injection changed to "the container downloads a presigned URL directly": bytes go
	// straight from object storage to the container, no longer through the shell command line, so
	// MAX_ARG_STRLEN does not apply.
	//
	// The current basis is **Ark's hard limit**: no more than 25 MiB per file after unzipping (the
	// Skills article in docs/ark-managed-agents). It is chosen because beyond it Ark cannot accept the
	// file, and then the skill can only degrade to "only upload SKILL.md" (see skillark.go): the
	// attachment is in the container, but the model does not know about it; one fewer such split is
	// one fewer.
	MaxSkillFileBytes = 25 << 20

	// MaxSkillTotalBytes is the limit on the total of all files after unzipping.
	//
	// ⚠️ It used to be 2 MiB, because "injection is serial, and split into 512 KiB batches it is a
	// single-digit number of execs". That also fell apart with the injection channel change: now it
	// is **one download + one unzip**, independent of the file count.
	//
	// 100 MiB matches MaxUploadBytes. It is no longer "afraid of slow injection" but about two things:
	// the bytes pass through amad's memory once (on upload), and the cost of object storage.
	MaxSkillTotalBytes = 100 << 20

	// MaxSkillFiles is the entry count limit.
	//
	// ⚠️ It used to be 64, because "each file takes part of the injection script's budget";
	// injection no longer builds a script per file, so that reason is gone. Skill packages with
	// thousands of files really exist; they could not fit before, and now they can.
	//
	// **A limit is still kept**, because it stops something else: zip-bomb-like mistakes ("packed the
	// whole node_modules in"). 10000 is far beyond any normal skill, and it gives "parsing a malicious
	// zip" a predictable upper bound.
	// ⚠️ The Ark side only accepts 500 files; beyond that it takes the "only upload SKILL.md"
	// fallback path.
	MaxSkillFiles = 10000

	// MaxSkillPathBytes is the length limit of a single path, against overly long paths.
	//
	// File systems in the container usually limit a segment to 255 bytes and a full path to 4096,
	// but our paths are also put into shell scripts, so shorter is safer. 200 is enough for normal
	// structures like "scripts/foo/bar.sh".
	MaxSkillPathBytes = 200

	// SkillFileName is the fixed file name of the skill body. It is the name Ark's convention uses, case-sensitive.
	SkillFileName = "SKILL.md"
)

// NewPkgError wraps parse failures uniformly as persistmodel.ErrInvalidArgument.
//
// ⚠️ **The name is deliberately not ErrSkillPkg**: in this repo that shape is how sentinel error
// variables are written (ErrUploadTooLarge and so on), and this is a constructor. A name starting
// with Err would make the next person write `errors.Is(err, ErrSkillPkg)`; that does not compile,
// but the worse case is they change it to an `==` comparison that is always false.
//
// **The message must not end with the library's raw error**: the handler's userMessage takes the part
// after the last ": " (see core/handler.go), and appending `zip: not a valid zip file` at the end
// would show the user a broken English half-sentence. So msg here is always a complete Chinese
// sentence; library errors only go into logs, never into the message.
func NewPkgError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", persistmodel.ErrInvalidArgument, fmt.Sprintf(format, args...))
}

// ParseSkillPackage parses an uploaded skill package.
//
// filename decides the path: `.zip` goes through zip parsing, anything else is treated as a single
// SKILL.md body. No MIME sniffing: the Content-Type from the browser can be changed by the caller and
// is no more trustworthy than the extension, and zip's magic number check is done by archive/zip
// itself (a non-zip errors out directly).
//
// **Nothing touches disk.** Everything stays in memory, because production runs multiple instances,
// each with its own disk that is lost on rebuild (see CLAUDE.md); writing the unzipped result to
// local disk would make "inject into the container" unable to find the files on another instance.
func ParseSkillPackage(filename string, data []byte) (apimodel.SkillPackage, error) {
	if len(data) == 0 {
		return apimodel.SkillPackage{}, NewPkgError("上传的文件是空的")
	}
	if strings.EqualFold(path.Ext(filename), ".zip") {
		return parseSkillZip(data)
	}
	return parseSkillMarkdown(data)
}

// parseSkillMarkdown parses a bare SKILL.md: only a body, no attachments.
func parseSkillMarkdown(data []byte) (apimodel.SkillPackage, error) {
	if len(data) > MaxSkillFileBytes {
		return apimodel.SkillPackage{}, NewPkgError(
			"SKILL.md 太大了（%s），单个文件不能超过 %s",
			humanize.HumanBytes(len(data)), humanize.HumanBytes(MaxSkillFileBytes))
	}
	meta, body, err := parseFrontmatter(data)
	if err != nil {
		return apimodel.SkillPackage{}, err
	}
	return apimodel.SkillPackage{
		Name:         meta.Name,
		DisplayTitle: meta.displayTitle(),
		Description:  meta.Description,
		Instructions: body,
		Files:        []apimodel.SkillFile{},
	}, nil
}

// parseSkillZip parses a zip skill package.
//
// The top-level directory is stripped: the structure Ark recommends is `foo.zip → foo/SKILL.md` (see
// the official Skills docs), and after stripping SKILL.md is at the root, the same shape as a bare md
// upload; without stripping, the same skill uploaded two ways would get two sets of paths, and after
// injection into the container the agent would have to guess where files are.
func parseSkillZip(data []byte) (apimodel.SkillPackage, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		// The library's raw text ("zip: not a valid zip file") does not go into the message; see NewPkgError.
		return apimodel.SkillPackage{}, NewPkgError("压缩包打不开，请确认是有效的 zip 文件")
	}

	// First block by entry count. **Before unzipping**, because the entry count is in the header and
	// trustworthy (the number of directory entries cannot lie; writing more means storing more), and
	// unzipping is the step that costs.
	if len(zr.File) > MaxSkillFiles {
		return apimodel.SkillPackage{}, NewPkgError(
			"压缩包里有 %d 个条目，超过上限 %d 个", len(zr.File), MaxSkillFiles)
	}

	prefix := commonTopDir(zr.File)

	var (
		items []zipItem
		// index is path → items index, used to look up targets when dereferencing symlinks.
		//
		// **On duplicate paths keep the first, and do not error here.** Erroring would make "two
		// SKILL.md" hit "duplicate path" first, and the user would no longer see the more accurate
		// "there can only be one SKILL.md". Duplicates are still caught below by the SKILL.md check and
		// ensureUniquePaths.
		index = make(map[string]int, len(zr.File))
		total int
	)
	for _, entry := range zr.File {
		rel, skip, err := sanitizeZipEntry(entry, prefix)
		if err != nil {
			return apimodel.SkillPackage{}, err
		}
		if skip {
			continue
		}

		if isSymlinkEntry(entry) {
			// A symlink's content is the target path, not file bytes, so it **does not count toward
			// the total**; what counts is the real content swapped in after dereferencing (see
			// derefSkillSymlinks). Only the target string is collected here; dereferencing waits until
			// all entries are read: a symlink can point to an entry after it, and resolving while
			// reading would misjudge "the target has not appeared yet" as "the target does not exist".
			target, err := readZipSymlinkTarget(entry)
			if err != nil {
				return apimodel.SkillPackage{}, err
			}
			items = append(items, zipItem{path: rel, link: target})
		} else {
			content, err := readZipEntry(entry, total)
			if err != nil {
				return apimodel.SkillPackage{}, err
			}
			total += len(content)
			if total > MaxSkillTotalBytes {
				return apimodel.SkillPackage{}, NewPkgError(
					"解压后总大小超过上限 %s", humanize.HumanBytes(MaxSkillTotalBytes))
			}
			items = append(items, zipItem{path: rel, content: content})
		}
		if _, dup := index[rel]; !dup {
			index[rel] = len(items) - 1
		}
	}

	// Replace symlinks with the real content of their targets. **Done before picking SKILL.md**, so a
	// symlink like `SKILL.md → real.md` can also serve as the body; picking the body first would treat
	// it as "SKILL.md not found".
	if err := derefSkillSymlinks(items, index, total); err != nil {
		return apimodel.SkillPackage{}, err
	}

	var (
		files []apimodel.SkillFile
		body  []byte
		found bool
	)
	for _, it := range items {
		if it.path == SkillFileName {
			if found {
				// Ark requires exactly one SKILL.md. With two, there is no way to know which is the body,
				// and silently taking the first would show the user, after upload, a body that is not the
				// one they thought.
				return apimodel.SkillPackage{}, NewPkgError(
					"压缩包里有多个 %s，只能有一个", SkillFileName)
			}
			found = true
			body = it.content
			// The SKILL.md body goes into Instructions and is not stored again in Files: otherwise the
			// same content would have two sources of truth, and after changing the body the attachment
			// would still be the old one.
			continue
		}
		files = append(files, apimodel.SkillFile{Path: it.path, Size: len(it.content), Content: it.content})
	}

	if !found {
		return apimodel.SkillPackage{}, NewPkgError(
			"压缩包里找不到 %s，技能正文必须放在这个文件里", SkillFileName)
	}

	meta, instructions, err := parseFrontmatter(body)
	if err != nil {
		return apimodel.SkillPackage{}, err
	}
	if files == nil {
		// Must not be nil: this slice goes into JSON, and nil serializes as null, making the
		// frontend's .map() throw (see the three JSON conventions at the start of
		// model/apimodel/apimodel.go).
		files = []apimodel.SkillFile{}
	}
	if err := ensureUniquePaths(files); err != nil {
		return apimodel.SkillPackage{}, err
	}
	return apimodel.SkillPackage{
		Name:         meta.Name,
		DisplayTitle: meta.displayTitle(),
		Description:  meta.Description,
		Instructions: instructions,
		Files:        files,
	}, nil
}

// sanitizeZipEntry validates the name of a zip entry and turns it into a relative path.
//
// Returning skip=true means the entry should be ignored (directory entry, packaging metadata, empty
// after stripping the prefix). Returning an error means the entry has attack features, and **the whole
// package is rejected**, not just this entry skipped. Skipping would let an attacker smuggle a
// malicious entry inside a valid package, uploaded successfully with no hint at all.
func sanitizeZipEntry(entry *zip.File, prefix string) (string, bool, error) {
	name := entry.Name

	if len(name) > MaxSkillPathBytes {
		return "", false, NewPkgError("条目路径太长（超过 %d 字节）", MaxSkillPathBytes)
	}
	if !utf8.ValidString(name) {
		// Non-UTF-8 names cannot go into a PG text column, and cannot be safely put into a shell either.
		return "", false, NewPkgError("条目路径不是合法的 UTF-8 编码")
	}

	// Turn backslashes into slashes first: the zip spec requires `/`, but Windows packaging tools
	// write `\`, and path.Clean **does not recognize backslashes**; `..\..\x` is an ordinary file name
	// to it. Without converting, the escape check below would be bypassed.
	name = strings.ReplaceAll(name, `\`, "/")

	// Absolute paths and drive letters. path.IsAbs recognizes the former but not the latter (`C:/x`), checked separately.
	if path.IsAbs(name) || hasDriveLetter(name) {
		return "", false, NewPkgError("压缩包含绝对路径条目 %q，不支持", name)
	}

	// Packaging metadata is skipped here too. **Must be checked before stripping the prefix**:
	// __MACOSX/ is a top-level directory and cannot be recognized after stripping. Without skipping,
	// they would be accepted as attachments, taking injection budget, and names like ._SKILL.md would
	// make people think the skill really has that file.
	if isZipMetadata(name) {
		return "", true, nil
	}

	// Strip the top-level directory.
	if prefix != "" {
		name = strings.TrimPrefix(name, prefix)
	}

	cleaned := path.Clean(name)
	if cleaned == "." || cleaned == "/" || cleaned == "" {
		return "", true, nil // the top-level directory itself; nothing left after stripping
	}
	// **The Zip Slip rule.** Still starting with .. after normalizing means it wants to jump up.
	// Check after Clean rather than with Contains(".."): the former catches `a/../../b` (cleaned to
	// `../b`), the latter would wrongly reject legal names like `a..b.md`.
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", false, NewPkgError("压缩包含越出目录的条目 %q，不支持", entry.Name)
	}
	if entry.FileInfo().IsDir() {
		return "", true, nil
	}
	// After stripping the prefix a metadata file may show up again (for example foo/.DS_Store → .DS_Store).
	if isZipMetadata(cleaned) {
		return "", true, nil
	}
	return cleaned, false, nil
}

// zipItem is an entry that has passed the security checks, waiting to be assembled into a
// SkillFile.
//
// Regular files carry content, symlinks carry link (the raw target path). The two are mutually
// exclusive: after dereferencing, every entry with a link gets content filled in; see
// derefSkillSymlinks.
//
// This intermediate form is needed because **a symlink can point to an entry after it** (entry order
// in a zip is decided by the packer and not guaranteed to be topological). Resolving while reading
// would misjudge "the target is further on" as "the target does not exist", so the whole package is
// collected first and then resolved in one go.
type zipItem struct {
	path    string
	content []byte
	// A non-empty link means this is a symlink; the value is the raw target path stored in the zip (not normalized).
	link string
}

// isSymlinkEntry tells whether a zip entry is a symbolic link.
//
// **Both sources must be checked.** entry.Mode() only decodes the high 16 bits of ExternalAttrs when
// the high byte of CreatorVersion is marked Unix; some packers (and Go's own zip.Writer when only
// ExternalAttrs is set) do not write that marker, so Mode() returns 0, and checking it alone would
// miss them; missing one means accepting the target path string as file content, silently producing
// a broken file.
//
// ⚠️ **On the ExternalAttrs side the file type must be masked out with S_IFMT first and then
// compared; never just & S_IFLNK.** The file type is an **enum value** over those 4 bits, not
// independent flag bits: a regular file S_IFREG is 0o100000, and `0o100644 & 0o120000 == 0o100000 !=
// 0`; a plain bitwise AND would judge **every regular file** to be a symlink. The symptom is every
// file in a zip produced by any real packaging tool being dereferenced as a symlink, reporting
// "symlink %q points to a target ... that does not exist" (the target being that file's own content
// treated as a path), pointing nowhere near the real cause (zip(1), macOS right-click compress, and
// Python zipfile.write all record 0o100644). The old tests could not catch this: they left regular
// entries at mode: 0, so ExternalAttrs was 0.
func isSymlinkEntry(entry *zip.File) bool {
	return entry.Mode()&fsModeSymlink != 0 ||
		(entry.ExternalAttrs>>16)&unixModeIFMT == unixModeSymlink
}

// readZipSymlinkTarget reads the target path of a symlink entry.
//
// A symlink's "content" is the target path string. **It is untrusted input too**, so it is still
// length-limited (borrowing MaxSkillPathBytes; a path should not be longer than the entry name
// limit) rather than read without limit: an entry declared as a symlink whose content is several GB
// is still a zip bomb.
func readZipSymlinkTarget(entry *zip.File) (string, error) {
	rc, err := entry.Open()
	if err != nil {
		return "", NewPkgError("解压条目 %q 失败", entry.Name)
	}
	defer func() { _ = rc.Close() }()

	// +1 is what tells "exactly at the limit" apart from "over".
	raw, err := io.ReadAll(io.LimitReader(rc, MaxSkillPathBytes+1))
	if err != nil {
		return "", NewPkgError("读取条目 %q 失败", entry.Name)
	}
	if len(raw) > MaxSkillPathBytes {
		return "", NewPkgError(
			"符号链接 %q 的目标路径太长（超过 %d 字节）", entry.Name, MaxSkillPathBytes)
	}
	target := strings.TrimSpace(string(raw))
	if target == "" {
		return "", NewPkgError("符号链接 %q 没有目标路径", entry.Name)
	}
	if !utf8.ValidString(target) {
		return "", NewPkgError("符号链接 %q 的目标路径不是合法的 UTF-8 编码", entry.Name)
	}
	return target, nil
}

// maxSymlinkDepth is the depth limit for dereferencing symlinks.
//
// Symlinks to symlinks are supported (`a → b → real file`), but **there must be an end**:
// self-reference (`a → a`) and mutual reference (`a → b`, `b → a`) can both be built in a zip, and
// following them is an endless loop; that is not an error but a hung parsing thread, with the request
// timing out and no log explaining why.
//
// 8 levels is far more than real skill packages need (symlinks to symlinks are rare to begin with);
// anything beyond is reported as a cycle: when "really nested 9 levels" and "there is a cycle" cannot
// be told apart, an error is better than hanging.
const maxSymlinkDepth = 8

// derefSkillSymlinks replaces the symlinks in items in place with the real content of their
// targets.
//
// # Resolution rules
//
// The target is resolved **relative to the directory the symlink is in**: `ref/link.md →
// ../shared/a.md` resolves to `shared/a.md`, consistent with the OS semantics of symlinks. After
// resolving it is normalized, and it must still land inside the package: starting with `..`
// (escaping the package) or an absolute path/drive letter is always rejected. **This is an
// extension of the Zip Slip defense in sanitizeZipEntry, not an exception to it**: the symlink
// entry's own name has already been checked, but its **target** is another, completely separate path
// that is equally controlled by the attacker.
//
// A missing target is also an error, rather than silently dropping the entry: a symlink in the
// package means the author thought that file was useful, and dropping it silently would make the
// agent, after the skill is installed, read a file that does not exist per SKILL.md.
//
// # Total size limit
//
// budget is the number of bytes already used by regular files. Dereferencing **amplifies** (N
// symlinks pointing to the same 1 MB file add N MB), so every piece of content swapped in must be
// added to budget and compared against the limit, otherwise "symlink + big file" is a way around
// MaxSkillTotalBytes.
func derefSkillSymlinks(items []zipItem, index map[string]int, budget int) error {
	for i := range items {
		if items[i].link == "" {
			continue
		}
		content, err := resolveSymlink(items, index, i)
		if err != nil {
			return err
		}
		// The per-file limit is already guaranteed by the target file's own readZipEntry; only the
		// total is added here: the target's bytes were counted once in budget (as the target file
		// itself), and the symlink swapping it in is another copy, so add it again.
		budget += len(content)
		if budget > MaxSkillTotalBytes {
			return NewPkgError(
				"符号链接展开后总大小超过上限 %s", humanize.HumanBytes(MaxSkillTotalBytes))
		}
		items[i].content = content
		items[i].link = ""
	}
	return nil
}

// resolveSymlink follows the symlink chain from items[start] to a real file and returns its
// content.
//
// It dereferences level by level, redoing the "resolve → normalize → still inside the package"
// check at every level: a middle hop could also point outside the package, and checking only the
// first hop would let all later hops through. Besides the depth limit there is a seen set; both can
// stop a cycle: the limit catches long chains, and seen reports self-reference/mutual reference
// right away instead of walking all 8 levels.
func resolveSymlink(items []zipItem, index map[string]int, start int) ([]byte, error) {
	origin := items[start].path
	seen := map[string]struct{}{origin: {}}
	cur := start

	for depth := 0; depth < maxSymlinkDepth; depth++ {
		target, err := resolveSymlinkTarget(items[cur].path, items[cur].link)
		if err != nil {
			return nil, err
		}
		next, ok := index[target]
		if !ok {
			// The target may be a directory (directory entries in the zip are skipped, so it cannot
			// be found); both cases are "cannot get the file content", merged into one sentence
			// without telling them apart.
			return nil, NewPkgError(
				"符号链接 %q 指向的目标 %q 不在压缩包里", origin, target)
		}
		if items[next].link == "" {
			return items[next].content, nil
		}
		if _, loop := seen[target]; loop {
			return nil, NewPkgError(
				"符号链接 %q 指向的目标构成了循环引用（%q）", origin, target)
		}
		seen[target] = struct{}{}
		cur = next
	}
	return nil, NewPkgError(
		"符号链接 %q 的嵌套层数超过 %d 层，可能存在循环引用", origin, maxSymlinkDepth)
}

// resolveSymlinkTarget resolves a symlink's target path into a normalized path inside the package.
//
// from is the symlink's own path (like `ref/link.md`), target is the raw target stored in the zip.
// A relative target is based on the **directory** of from, consistent with OS symlink semantics.
func resolveSymlinkTarget(from, target string) (string, error) {
	// Normalize backslashes first, as in sanitizeZipEntry, otherwise `..\..\etc\passwd` would bypass
	// the checks below (path.Clean does not recognize backslashes; the whole string is an ordinary
	// file name to it).
	target = strings.ReplaceAll(target, `\`, "/")

	// Reject absolute paths and drive letters directly. **Never follow it to read a host file**: this
	// does not touch the file system at all, but letting it through would look it up as a path inside
	// the package, and when not found report "the target is not in the archive", which would hide the
	// fact that "this package tried to read /etc/passwd".
	if path.IsAbs(target) || hasDriveLetter(target) {
		return "", NewPkgError(
			"符号链接 %q 指向绝对路径 %q，不支持", from, target)
	}

	cleaned := path.Clean(path.Join(path.Dir(from), target))
	// The same rule as sanitizeZipEntry: still starting with .. after normalizing means it escaped the package.
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", NewPkgError(
			"符号链接 %q 指向的目标 %q 越出了压缩包，不支持", from, target)
	}
	if cleaned == "." || cleaned == "/" || cleaned == "" {
		return "", NewPkgError("符号链接 %q 的目标 %q 不是一个文件", from, target)
	}
	return cleaned, nil
}

// readZipEntry unpacks one entry, counting bytes as it goes.
//
// budget is the total already used, used to compute how much this entry may still read: exceeding
// the total must be discovered **while reading**, not compared after reading.
//
// **Never look at entry.UncompressedSize64.** That is the declared value in the header, written by
// the packer, and zip bombs lie with exactly that: declare 1 KB, unpack several GB. The only
// reliable way is to count ourselves.
func readZipEntry(entry *zip.File, budget int) ([]byte, error) {
	rc, err := entry.Open()
	if err != nil {
		return nil, NewPkgError("解压条目 %q 失败", entry.Name)
	}
	defer func() { _ = rc.Close() }()

	// Take the smaller of the per-file limit and the remaining total, then +1: reading one extra byte
	// is how to tell "exactly at the limit" from "over", otherwise a file exactly at the limit would
	// be misjudged as over.
	limit := MaxSkillFileBytes
	if remain := MaxSkillTotalBytes - budget; remain < limit {
		limit = remain
	}
	content, err := io.ReadAll(io.LimitReader(rc, int64(limit)+1))
	if err != nil {
		return nil, NewPkgError("读取条目 %q 失败", entry.Name)
	}
	if len(content) > MaxSkillFileBytes {
		return nil, NewPkgError(
			"条目 %q 解压后超过单文件上限 %s", entry.Name, humanize.HumanBytes(MaxSkillFileBytes))
	}
	if len(content) > limit {
		return nil, NewPkgError("解压后总大小超过上限 %s", humanize.HumanBytes(MaxSkillTotalBytes))
	}
	return content, nil
}

// ensureUniquePaths checks for duplicate paths in the same package.
//
// zip allows two entries with the same name (in the format they are just two records), and when
// stored they would collide on the skill_files primary key (skill_id, path), giving a 500. Block it
// here with a sentence people can understand.
func ensureUniquePaths(files []apimodel.SkillFile) error {
	seen := make(map[string]struct{}, len(files))
	for _, f := range files {
		if _, dup := seen[f.Path]; dup {
			return NewPkgError("压缩包里有重复的路径 %q", f.Path)
		}
		seen[f.Path] = struct{}{}
	}
	return nil
}

// commonTopDir finds the top-level directory shared by all entries, like "foo/".
//
// Returns an empty string when there is no shared top-level directory (SKILL.md is directly at the
// root, or there are several sibling directories). Only strips when there is **exactly one**
// top-level directory, otherwise files from `a/` and `b/` would be mixed together.
func commonTopDir(entries []*zip.File) string {
	top := ""
	for _, e := range entries {
		name := strings.ReplaceAll(e.Name, `\`, "/")
		// Ignore the metadata directories zip tools add, otherwise they would break "only one
		// top-level directory", the prefix would not be stripped, and SKILL.md's path would become
		// `foo/SKILL.md` and not be found.
		if isZipMetadata(name) {
			continue
		}
		idx := strings.Index(name, "/")
		if idx < 0 {
			return "" // an entry is directly at the root, so there is no shared prefix
		}
		dir := name[:idx+1]
		if top == "" {
			top = dir
			continue
		}
		if top != dir {
			return "" // several sibling top-level directories
		}
	}
	return top
}

// isZipMetadata tells metadata entries added by macOS / Windows packaging tools.
//
// These entries are **only ignored, not errors**: packaging tools add them automatically, and users
// neither know about nor control them; rejecting the whole package because of __MACOSX would make
// "right-click compress on a Mac", the most common way to package, unusable.
func isZipMetadata(name string) bool {
	base := path.Base(name)
	return strings.HasPrefix(name, "__MACOSX/") ||
		strings.Contains(name, "/__MACOSX/") ||
		base == ".DS_Store" ||
		base == "Thumbs.db" ||
		strings.HasPrefix(base, "._")
}

// hasDriveLetter tells whether a name starts with a Windows drive letter (`C:/x`, `c:x`).
//
// path.IsAbs only recognizes a leading `/`; it treats drive letter forms as relative paths, so they
// could bypass the absolute path check.
func hasDriveLetter(name string) bool {
	if len(name) < 2 || name[1] != ':' {
		return false
	}
	c := name[0]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// CleanSkillPath validates and normalizes a relative path inside a skill; the second return value
// tells whether it is valid.
//
// # Why an independent pure function instead of reusing sanitizeZipEntry
//
// That function takes a *zip.File and also does "strip the top-level directory" and "skip directory
// entries and packaging metadata", which only the upload path needs. The entry points that read/write
// a single attachment by path (an HTTP query parameter) get a bare string, with no zip entry and no
// prefix to strip.
//
// ⚠️ **The rules must match sanitizeZipEntry one for one** (each one below maps to one there),
// because they guard the same invariant: a path stored in skill_files is always a normalized relative
// path. If the two drifted, "a shape rejected on upload could be pushed in through the edit API": a
// silent bypass. The two extra things done there (strip prefix, skip directories/metadata) make no
// sense here and are deliberately left out.
//
// Its relation to safeSkillPath in shared/infra/shelltools is **defense in depth**, not
// duplication: that layer is the last step that turns untrusted data into real files in the
// container, and it must be able to judge safety on its own without relying on "the caller
// validated".
func CleanSkillPath(name string) (string, bool) {
	if strings.TrimSpace(name) == "" {
		return "", false
	}
	// Length first: like sanitizeZipEntry it counts the **raw byte count**, not after normalizing.
	if len(name) > MaxSkillPathBytes {
		return "", false
	}
	// Non-UTF-8 cannot go into a PG text column, and cannot be safely put into a shell either.
	if !utf8.ValidString(name) {
		return "", false
	}
	// Turn backslashes into slashes first: path.Clean **does not recognize backslashes**, `..\..\x`
	// is an ordinary file name to it, and without converting the escape check below would be bypassed.
	name = strings.ReplaceAll(name, `\`, "/")
	// Absolute paths and Windows drive letters (path.IsAbs does not recognize the latter).
	if path.IsAbs(name) || hasDriveLetter(name) {
		return "", false
	}
	cleaned := path.Clean(name)
	// After normalizing, "the current directory" or still jumping up are not valid files inside a
	// skill. Check after Clean rather than with Contains(".."): the former catches `a/../../b`
	// (cleaned to `../b`), the latter would wrongly reject legal names like `a..b.md`.
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", false
	}
	// NUL and newline: the former can truncate C strings, the latter can start a new command line when
	// put into a script. Same as safeSkillPath in shared/infra/shelltools.
	if strings.ContainsAny(cleaned, "\x00\n") {
		return "", false
	}
	return cleaned, true
}

// fsModeSymlink is the value of io/fs.ModeSymlink.
//
// Written as a constant instead of importing io/fs: zip.File.Mode() returns fs.FileMode, but this
// file only needs to check this one bit, which is not worth importing a package for.
const fsModeSymlink = 1 << 27

// unixModeSymlink is S_IFLNK, the "symbolic link" value among Unix file types.
//
// It and fsModeSymlink are at two different levels: the latter is Go's abstract bit, the former is
// the raw OS value stored in the high 16 bits of the zip's ExternalAttrs. **Both must be checked**,
// because zip.File.Mode() only decodes ExternalAttrs when CreatorVersion is marked Unix; otherwise
// it returns 0, and checking Mode() alone misses symlinks (measured: this file's tests build
// symlinks with ExternalAttrs, and checking only Mode() makes the cases red).
const unixModeSymlink = 0o120000

// unixModeIFMT is S_IFMT, the mask of the 4 bits in a Unix mode that identify the **file type**.
//
// The type must be masked out with it first and then compared for equality with S_IFLNK. The file
// type is an enum over these 4 bits (regular 0o100000 / dir 0o040000 / symlink 0o120000 / …), not
// flags that can be ANDed independently: `0o100644 & 0o120000` is non-zero, but that is a regular
// file.
const unixModeIFMT = 0o170000

// skillFrontmatter is the shape of the YAML at the top of SKILL.md.
//
// Matches .claude/skills/*/SKILL.md (name + description), which is also the Ark skill package
// convention. Extra keys **are not errors**: skill packages may carry fields we do not know, like
// license or version, and failing because of them makes no sense.
type skillFrontmatter struct {
	Name         string `yaml:"name"`
	Description  string `yaml:"description"`
	DisplayTitle string `yaml:"display_title"`
}

// displayTitle gets the display name, falling back to name when missing.
//
// Fall back rather than error: display_title is not a required field in the convention (none of the
// examples under .claude/skills have it), and the UI has to show something.
func (f skillFrontmatter) displayTitle() string {
	if t := strings.TrimSpace(f.DisplayTitle); t != "" {
		return t
	}
	return strings.TrimSpace(f.Name)
}

// parseFrontmatter splits the YAML frontmatter and body of SKILL.md.
//
// **Missing or invalid fields are always errors; a skill is never silently created with empty
// values.** The silent result would be: the skill is created, name is an empty string, and Ark's
// [a-z0-9-] constraint only blows up **when uploading to Ark**, by which time the user has long left
// this page, and the error cannot point back to this upload.
func parseFrontmatter(data []byte) (skillFrontmatter, string, error) {
	text := string(data)
	// Tolerate a UTF-8 BOM: Windows editors add it, and with it `---` is not at the start, the
	// frontmatter is treated as body, and then an empty name reports "missing frontmatter"; that error
	// points nowhere near the real cause (three invisible bytes at the start of the file).
	text = strings.TrimPrefix(text, "\ufeff")
	// Normalize newlines: in a CRLF file the separator line is "---\r", which does not match "---" exactly.
	text = strings.ReplaceAll(text, "\r\n", "\n")

	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return skillFrontmatter{}, "", NewPkgError(
			"%s 缺少开头的 YAML frontmatter，需要以 --- 起始并声明 name 与 description",
			SkillFileName)
	}
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return skillFrontmatter{}, "", NewPkgError(
			"%s 的 frontmatter 没有结束标记 ---", SkillFileName)
	}
	head := rest[:idx]
	body := rest[idx+len("\n---"):]
	// The rest of the closing marker line (maybe "\n" or "---" followed by something) must be consumed.
	if nl := strings.IndexByte(body, '\n'); nl >= 0 {
		body = body[nl+1:]
	} else {
		body = ""
	}

	var meta skillFrontmatter
	if err := yaml.Unmarshal([]byte(head), &meta); err != nil {
		// yaml's raw error (with line and column) is useful for locating, but must not go into the user message (see NewPkgError).
		return skillFrontmatter{}, "", NewPkgError("%s 的 frontmatter 不是合法的 YAML", SkillFileName)
	}

	meta.Name = strings.TrimSpace(meta.Name)
	meta.Description = strings.TrimSpace(meta.Description)

	if meta.Name == "" {
		return skillFrontmatter{}, "", NewPkgError(
			"%s 的 frontmatter 缺少 name 字段", SkillFileName)
	}
	// Ark-side skill names only accept [a-z0-9-] and must start with a letter or digit.
	// **Blocked here instead of waiting for Ark to reject**: that error is in English and happens on
	// another path.
	if !apimodel.ValidSkillName(meta.Name) {
		return skillFrontmatter{}, "", NewPkgError(
			"%s 里的 name %q 不合法，只能用小写字母、数字和连字符", SkillFileName, meta.Name)
	}
	if meta.Description == "" {
		return skillFrontmatter{}, "", NewPkgError(
			"%s 的 frontmatter 缺少 description 字段，模型靠它决定何时使用该技能",
			SkillFileName)
	}

	body = strings.TrimLeft(body, "\n")
	if strings.TrimSpace(body) == "" {
		return skillFrontmatter{}, "", NewPkgError("%s 的正文是空的", SkillFileName)
	}
	return meta, body, nil
}

// The real humanBytes is in shared/gopkg/humanize (`humanize.HumanBytes`).
//
// It moved because 6 files use it, spanning the skill / issue / suggestion / devblob domains, and
// core is being split into packages by resource domain; leaving it here would make the first domain
// to move take it along.

// HumanSkillFileLimit / HumanSkillTotalLimit are the wordings of the limits for **docs and user
// messages**.
//
// # Why export these two instead of letting others write literals
//
// The OpenAPI descriptions need to say "per-file limit X, total limit Y", and the spec is for
// agents: **a stale spec is worse than no spec** (agents follow it, call APIs wrongly again and
// again, and the errors do not point at the real cause; see server/openapi/README.md). With
// literals this loosening would have been missed: measured, those two places kept saying
// "256 KiB / 2 MiB" long after the constants changed.
//
// Computed from the constants, the wording follows automatically when the limits change, without
// relying on anyone remembering.
var (
	HumanSkillFileLimit  = humanize.HumanBytes(MaxSkillFileBytes)
	HumanSkillTotalLimit = humanize.HumanBytes(MaxSkillTotalBytes)
)

// ErrUploadTooLarge is the error when the request body exceeds MaxUploadBytes.
//
// A separate sentinel because http.MaxBytesReader's error carries no type information (just the
// string "http: request body too large"), and handlers rely on this to tell 413 from an ordinary
// 400.
var ErrUploadTooLarge = fmt.Errorf(
	"%w: 上传的文件超过 %s 上限", persistmodel.ErrInvalidArgument, humanize.HumanBytes(MaxUploadBytes))

// IsUploadTooLarge tells whether an error is a request body over the limit.
//
// MaxBytesReader returns *http.MaxBytesError when over the limit, but that only exists in Go 1.19+
// and errors.As needs to import net/http; the core package imports net/http anyway (handler.go), so
// here it also falls back to matching the error text, accepting both forms.
func IsUploadTooLarge(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrUploadTooLarge) {
		return true
	}
	return strings.Contains(err.Error(), "request body too large")
}

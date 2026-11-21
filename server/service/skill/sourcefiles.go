package skill

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/chyroc/ama/server/model/apimodel"
)

// SourceFile is one file read from an external skill directory. Path is relative to that
// directory. Truncated content must never be imported as a complete skill.
type SourceFile struct {
	Path      string
	Content   []byte
	Truncated bool
}

// PackageSourceFiles converts untrusted directory files to a zip and runs the same parser
// used for uploads. Reusing that path retains Zip Slip, symlink, duplicate-path, size and
// SKILL.md checks instead of maintaining a second, weaker repository-only parser.
func PackageSourceFiles(dir string, files []SourceFile) (apimodel.SkillPackage, error) {
	if len(files) == 0 {
		return apimodel.SkillPackage{}, NewPkgError("技能目录 %s 是空的，至少要有一个 %s", dir, SkillFileName)
	}
	for _, file := range files {
		if file.Truncated {
			return apimodel.SkillPackage{}, NewPkgError("文件 %s 被上游截断了，内容不完整，不能导入", file.Path)
		}
	}
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	sorted := append([]SourceFile(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	for _, file := range sorted {
		entry, err := writer.Create(file.Path)
		if err != nil {
			return apimodel.SkillPackage{}, NewPkgError("打包 %s 失败", file.Path)
		}
		if _, err := entry.Write(file.Content); err != nil {
			return apimodel.SkillPackage{}, NewPkgError("打包 %s 失败", file.Path)
		}
	}
	if err := writer.Close(); err != nil {
		return apimodel.SkillPackage{}, NewPkgError("打包技能 %s 失败", dir)
	}
	return ParseSkillPackage(dir+".zip", buffer.Bytes())
}

// SourceContentChecksum hashes content, not zip bytes: zip timestamps change between builds
// and would falsely report a new upstream version on every background check. Metadata fields
// participate because frontmatter is removed from Instructions by the parser.
func SourceContentChecksum(pkg apimodel.SkillPackage) string {
	hash := sha256.New()
	write := func(path string, content []byte) {
		_, _ = hash.Write([]byte(path))
		_, _ = hash.Write([]byte{0})
		_, _ = fmt.Fprintf(hash, "%d", len(content))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(content)
		_, _ = hash.Write([]byte{0})
	}
	write(SkillFileName, []byte(pkg.Instructions))
	write("\x01name", []byte(pkg.Name))
	write("\x01title", []byte(pkg.DisplayTitle))
	write("\x01desc", []byte(pkg.Description))
	files := append([]apimodel.SkillFile(nil), pkg.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	for _, file := range files {
		write(file.Path, file.Content)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

// SourceFilesBytes is the total content size shown by repository scans.
func SourceFilesBytes(files []SourceFile) int {
	total := 0
	for _, file := range files {
		total += len(file.Content)
	}
	return total
}

// ShortSourceVersion is a display-only abbreviation. Empty input falls back to "1" so
// a UI that prefixes versions with "v" never shows a bare "v".
func ShortSourceVersion(version string) string {
	version = strings.TrimSpace(version)
	if version == "" {
		return "1"
	}
	if len(version) > 8 {
		return version[:8]
	}
	return version
}

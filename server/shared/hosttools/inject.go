// Host-native version of skill injection, replacing the base64+shell approach in shelltools.
//
// Two internal operations (only started by amad's InjectSkills; Ark never sends them):
//
//	OpWriteFiles:    write several files at once, content goes as JSON. This fully avoids the
//	                 shell argv MAX_ARG_STRLEN (128KiB) limit, so no chunked writes are needed.
//	OpFetchArchive:  net/http downloads a presigned zip → byte count check → clear then unpack,
//	                 and supports caching on this machine by content fingerprint (same cache
//	                 behavior as the shell version).
package hosttools

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SkillFile is one skill file to write to the host. Its shape matches shelltools.SkillFile.
type SkillFile struct {
	// Path is relative to the skill directory. The caller must already have done the Zip Slip
	// check; this side blocks it once more.
	Path string `json:"path"`
	// Content is the file content, written as is (sent as JSON, not through a shell).
	Content string `json:"content"`
}

// SkillBundle is one skill with its files and an optional archive.
type SkillBundle struct {
	// Name is the skill name ([a-z0-9-]), used as the directory name.
	Name string
	// Files are written one by one; if the archive download fails, there is at least a fallback
	// SKILL.md.
	Files []SkillFile
	// ArchiveURL is the presigned zip download link, with a signature: **never put it in logs or
	// error messages**.
	ArchiveURL string
	// ArchiveBytes is the expected byte count, used to catch truncated downloads.
	ArchiveBytes int
	// When CacheKey is not empty, the local content-addressed cache (sha256 fingerprint) is
	// enabled.
	CacheKey string
}

// writeFilesInput is the input of OpWriteFiles.
type writeFilesInput struct {
	// Dir is the skill root directory (absolute, or relative to the daemon cwd); files land at
	// Dir/<f.Path>.
	Dir   string      `json:"dir"`
	Files []SkillFile `json:"files"`
}

func runWriteFiles(_ context.Context, workdir string, raw json.RawMessage) ToolResult {
	var req writeFilesInput
	if err := json.Unmarshal(raw, &req); err != nil {
		return ErrorResult("解析 write_files 入参失败: %v", err)
	}
	dir, err := ResolvePath(workdir, req.Dir)
	if err != nil {
		return ErrorResult("%v", err)
	}
	total := 0
	for _, f := range req.Files {
		rel, ok := safeSkillPath(f.Path)
		if !ok {
			return ErrorResult("技能文件路径不安全: %q", f.Path)
		}
		target := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return ErrorResult("建技能目录失败: %v", err)
		}
		if err := os.WriteFile(target, []byte(f.Content), 0o644); err != nil {
			return ErrorResult("写技能文件失败: %v", err)
		}
		total += len(f.Content)
	}
	return TextResult(fmt.Sprintf("wrote %d files (%d bytes)", len(req.Files), total))
}

// fetchArchiveInput is the input of OpFetchArchive.
type fetchArchiveInput struct {
	// Dir is the skill directory (the zip top level has the same name; it is unpacked into its
	// parent directory).
	Dir string `json:"dir"`
	// URL is the presigned link.
	URL string `json:"url"`
	// WantBytes is the expected byte count.
	WantBytes int `json:"wantBytes"`
	// When CacheDir is not empty and CacheKey is valid, the content-addressed cache is enabled. The
	// daemon anchors absolute paths at $HOME (same mechanism as the tool cwd); here it is treated
	// as a working file.
	CacheDir string `json:"cacheDir,omitempty"`
	CacheKey string `json:"cacheKey,omitempty"`
}

// archiveDownloadTimeout matches shelltools.archiveDownloadTimeout.
const archiveDownloadTimeout = 5 * time.Minute

func runFetchArchive(ctx context.Context, workdir string, raw json.RawMessage) ToolResult {
	var req fetchArchiveInput
	if err := json.Unmarshal(raw, &req); err != nil {
		return ErrorResult("解析 fetch_archive 入参失败: %v", err)
	}
	dir, err := ResolvePath(workdir, req.Dir)
	if err != nil {
		return ErrorResult("%v", err)
	}
	parent := filepath.Dir(dir)

	cachePath, useCache := resolveCachePath(anchorHome(req.CacheDir), req.CacheKey)

	var data []byte
	cached := false
	if useCache {
		if b, ok := readCacheValid(cachePath, req.WantBytes); ok {
			data = b
			cached = true
			_ = os.Chtimes(cachePath, time.Now(), time.Now())
		}
	}
	if data == nil {
		b, fail := downloadArchive(ctx, req.URL, req.WantBytes)
		if fail != nil {
			return *fail
		}
		data = b
		if useCache {
			// Write the cache atomically: write a temp file first, then rename.
			_ = os.MkdirAll(filepath.Dir(cachePath), 0o755)
			if tf, err := os.CreateTemp(filepath.Dir(cachePath), ".dl-*"); err == nil {
				_, _ = tf.Write(data)
				_ = tf.Chmod(0o600)
				_ = tf.Close()
				_ = os.Rename(tf.Name(), cachePath)
			}
		}
	}

	// Clear then unpack: the host may still have leftovers from the previous skill version.
	if err := os.RemoveAll(dir); err != nil {
		return ErrorResult("清理旧技能目录失败: %v", err)
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return ErrorResult("建技能目录失败: %v", err)
	}
	// Unpack and force every entry to land under the target skill directory name: the zip top level
	// must be the skill name, otherwise it could pollute sibling skill directories, or the target
	// directory may not exist at all after unpacking.
	skillName := filepath.Base(dir)
	if err := unzipSafe(data, parent, skillName); err != nil {
		return ErrorResult("解压技能包失败: %v", err)
	}
	// Check again that the target directory was really unpacked (unzipSafe already blocks a
	// mismatched top level; this is a second guard).
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return ErrorResult("解压技能包失败: 压缩包缺少技能目录 %s", skillName)
	}
	if cached {
		return TextResult(cacheHitMarker)
	}
	return TextResult("ok")
}

// cacheHitMarker has the same value as shelltools.cacheHitMarker; the caller uses it to log "cache
// hit".
const cacheHitMarker = "__ama_skill_cache_hit__"

// resolveCachePath validates the cache directory and key, and when valid returns the zip's
// absolute/working path.
func resolveCachePath(cacheDir, cacheKey string) (string, bool) {
	if !safeCacheDir(cacheDir) || !safeCacheKey(cacheKey) {
		return "", false
	}
	return filepath.Join(cacheDir, cacheKey+".zip"), true
}

// anchorHome anchors a relative cache directory at the daemon user's $HOME.
//
// ⚠️ The cache directory (hostproto.SkillArchiveCacheDir=".ama/cache/skill-archives") is meant to
// be relative to **$HOME**, while tool file paths are relative to the session cwd: two different
// bases. The old shell script used "$HOME/..." with double-quote expansion, and daemon_cache.go
// cleanup also locates it by $HOME. If this were wrongly anchored to the session cwd, it would land
// at $HOME/.ama/runs/<sid>/.ama/cache, and both cache hits and age-based cleanup would break with
// zero errors. Absolute paths are returned unchanged.
func anchorHome(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	return filepath.Join(home, p)
}

// readCacheValid returns the content on a hit with a matching byte count. Content addressing is
// itself a hash commitment, so only the size is checked.
func readCacheValid(cachePath string, wantBytes int) ([]byte, bool) {
	info, err := os.Stat(cachePath)
	if err != nil || info.IsDir() {
		return nil, false
	}
	if wantBytes > 0 && info.Size() != int64(wantBytes) {
		return nil, false
	}
	b, err := os.ReadFile(cachePath)
	if err != nil {
		return nil, false
	}
	if wantBytes > 0 && len(b) != wantBytes {
		return nil, false
	}
	return b, true
}

// downloadArchive downloads and checks the byte count. No error message ever **contains the URL**
// (it carries a signature). When fail is not nil, data is meaningless.
func downloadArchive(ctx context.Context, url string, wantBytes int) ([]byte, *ToolResult) {
	cctx, cancel := context.WithTimeout(ctx, archiveDownloadTimeout)
	defer cancel()
	client := &http.Client{Timeout: archiveDownloadTimeout}

	var resp *http.Response
	// Same as curl --retry 3: retry network errors and 5xx (brief storage faults); fail at once on
	// 4xx (retrying an expired signature/403/404 is pointless).
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
		if err != nil {
			// The input URL is signed by us, so a construction failure is an internal error; err
			// has no query parameters.
			return nil, ptrError("下载技能包失败：%v", err)
		}
		resp, err = client.Do(req)
		if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			break
		}
		if err == nil {
			// Got a response but not 2xx: fail at once on 4xx; on 5xx close the body and retry.
			if resp.StatusCode >= 400 && resp.StatusCode < 500 {
				_ = resp.Body.Close()
				return nil, ptrError("下载技能包失败：服务端返回错误（签名可能已过期）")
			}
			_ = resp.Body.Close()
			resp = nil
		}
		if attempt == 2 {
			return nil, ptrError("下载技能包失败：服务暂时不可用，请稍后重试")
		}
		select {
		case <-cctx.Done():
			return nil, ptrError("下载技能包失败：超时")
		case <-time.After(time.Duration(attempt+1) * time.Second):
		}
	}
	if resp == nil {
		// Fallback after three network errors (normally we already returned at attempt==2); never
		// echo the URL.
		return nil, ptrError("下载技能包失败：网络不可达")
	}
	defer func() { _ = resp.Body.Close() }()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, io.LimitReader(resp.Body, 110<<20)); err != nil {
		return nil, ptrError("下载技能包失败：读取中断")
	}
	if wantBytes > 0 && buf.Len() != wantBytes {
		return nil, ptrError("技能包字节数不对：下到 %d，应当是 %d（下载被截断了）",
			buf.Len(), wantBytes)
	}
	return buf.Bytes(), nil
}

func ptrError(format string, args ...any) *ToolResult {
	r := ErrorResult(format, args...)
	return &r
}

// unzipSafe unpacks a zip into parent, blocking path traversal and unusual entries one by one.
// unzipSafe unpacks a zip into parent, and forces the top-level directory of every entry to be
// expectedTop (the skill name): BuildSkillZip guarantees the package is `<skill name>/...`. This
// blocks Zip Slip, and also blocks "other top-level directories in the package" polluting sibling
// skills, and a wrong top level leaving the target skill directory missing.
func unzipSafe(data []byte, parent, expectedTop string) error {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	for _, zf := range zr.File {
		name := filepath.FromSlash(zf.Name)
		// Block Zip Slip: after cleaning, the path must still be inside parent.
		clean := filepath.Clean(name)
		if clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) ||
			filepath.IsAbs(clean) {
			return fmt.Errorf("压缩包内路径越界: %q", zf.Name)
		}
		// The top-level directory must be the skill name: take the first segment with slash
		// semantics, so nested directories are not misjudged.
		top := strings.SplitN(filepath.ToSlash(filepath.Clean(name)), "/", 2)[0]
		if top != expectedTop {
			return fmt.Errorf("压缩包顶层目录必须是 %q，实际条目 %q", expectedTop, zf.Name)
		}
		target := filepath.Join(parent, clean)
		rel, err := filepath.Rel(parent, target)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("压缩包内路径越界: %q", zf.Name)
		}

		mode := zf.Mode()
		switch {
		case mode.IsDir():
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case mode.IsRegular():
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			rc, err := zf.Open()
			if err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode.Perm())
			if err != nil {
				_ = rc.Close()
				return err
			}
			if _, err := io.Copy(out, rc); err != nil {
				_ = out.Close()
				_ = rc.Close()
				return err
			}
			_ = out.Close()
			_ = rc.Close()
		default:
			// Symlinks/device files and so on: reject. A skill package should not contain these,
			// and letting them through is an escape hole.
			return fmt.Errorf("压缩包内不支持的条目类型: %q", zf.Name)
		}
	}
	return nil
}

// safeCacheDir only allows the safe character set of relative paths like "$HOME/..." built by the
// daemon.
//
// Same rule as the shelltools version: cache paths spliced into a shell must be guarded against
// injection. The native version does not go into a shell, but keeps the same rule so callers cannot
// stuff an arbitrary directory in.
func safeCacheDir(d string) bool {
	if d == "" || len(d) > 256 {
		return false
	}
	if strings.ContainsAny(d, "\x00\n\r'\"`$\\ ") {
		return false
	}
	return true
}

// safeCacheKey only accepts a lowercase hex sha256 fingerprint.
func safeCacheKey(k string) bool {
	if len(k) != 64 {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// SkillsDirName is the name of the skills directory under the working directory, same value as
// shelltools.skillsDirName.
const SkillsDirName = "skills"

// SkillsRootFor means the same as shelltools.SkillsRootFor: always <workdir>/skills.
func SkillsRootFor(workdir string) string {
	if strings.TrimSpace(workdir) == "" {
		return "/" + SkillsDirName
	}
	if path.IsAbs(workdir) {
		return path.Join(workdir, SkillsDirName)
	}
	return SkillsDirName
}

// safeSkillDir / safeSkillPath follow the same rules as their shelltools namesakes (defense in
// depth).
func safeSkillDir(root, name string) (string, bool) {
	if name == "" || len(name) > 64 {
		return "", false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0:
		default:
			return "", false
		}
	}
	return path.Join(root, name), true
}

func safeSkillPath(p string) (string, bool) {
	if p == "" || len(p) > 512 {
		return "", false
	}
	if strings.ContainsRune(p, '\\') {
		return "", false
	}
	if path.IsAbs(p) {
		return "", false
	}
	cleaned := path.Clean(p)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", false
	}
	return cleaned, true
}

// InjectSkills injects skills on the host executor side, orchestrated the same way as
// shelltools.InjectSkills: archives in parallel (concurrency 4, timing starts only after getting a
// token, failures are warn-only and fall back), then files are written one by one.
func InjectSkills(
	ctx context.Context, doer Doer, workdir, cacheDir string,
	bundles []SkillBundle, log *slog.Logger,
) error {
	if len(bundles) == 0 {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	root := SkillsRootFor(workdir)

	// Pull archives first: skills that fail still have the file writes below as a fallback (at
	// least SKILL.md).
	archived, archiveErr := pullArchives(ctx, doer, root, cacheDir, bundles, log)

	var (
		batch      []SkillFile
		batchBytes int
		writes     int
		skipped    int
	)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		input, _ := json.Marshal(writeFilesInput{Dir: root, Files: batch})
		res, err := doer.Do(ctx, ToolCall{Name: OpWriteFiles, Input: input})
		if err != nil {
			return fmt.Errorf("写技能文件失败: %w", err)
		}
		if res.IsError {
			return fmt.Errorf("写技能文件失败: %s", res.Text)
		}
		batch = batch[:0]
		batchBytes = 0
		return nil
	}

	for _, b := range bundles {
		dir, ok := safeSkillDir(root, b.Name)
		if !ok {
			skipped++
			log.Warn("技能名不安全，跳过", slog.String("skill", b.Name))
			continue
		}
		if archived[b.Name] {
			// dir was already validated and used in the archive branch; the archive has all files,
			// so we do not write them one by one here.
			_ = dir
			continue
		}
		for _, f := range b.Files {
			rel, ok := safeSkillPath(f.Path)
			if !ok {
				skipped++
				log.Warn("附件路径不安全，跳过",
					slog.String("skill", b.Name), slog.String("path", f.Path))
				continue
			}
			if batchBytes > 0 && batchBytes+len(f.Content) > WriteFilesBatchBytes {
				if err := flush(); err != nil {
					return err
				}
			}
			batch = append(batch, SkillFile{Path: path.Join(b.Name, rel), Content: f.Content})
			batchBytes += len(f.Content)
			writes++
		}
	}
	if err := flush(); err != nil {
		return err
	}
	log.Info("技能文件已注入宿主机",
		slog.Int("files", writes), slog.Int("archives", len(archived)),
		slog.Int("skipped", skipped))
	if skipped > 0 {
		archiveErr = fmt.Errorf("hosttools: %d 个附件的路径不安全，已跳过", skipped)
	}
	return archiveErr
}

// pullArchives downloads/unpacks archives concurrently and returns the set of skill names that
// landed via archive.
func pullArchives(
	ctx context.Context, doer Doer, root, cacheDir string,
	bundles []SkillBundle, log *slog.Logger,
) (map[string]bool, error) {
	type job struct {
		name string
		dir  string
		b    SkillBundle
	}
	var jobs []job
	for _, b := range bundles {
		if b.ArchiveURL == "" {
			continue
		}
		dir, ok := safeSkillDir(root, b.Name)
		if !ok {
			continue
		}
		jobs = append(jobs, job{name: b.Name, dir: dir, b: b})
	}
	if len(jobs) == 0 {
		return map[string]bool{}, nil
	}

	type outcome struct {
		ok  bool
		err error
	}
	results := make([]outcome, len(jobs))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// Timing starts only after getting a token (queueing does not count against the
			// budget); same reason as the shelltools version.
			cctx, cancel := context.WithTimeout(ctx, archiveDownloadTimeout+30*time.Second)
			defer cancel()
			input, _ := json.Marshal(fetchArchiveInput{
				Dir: j.dir, URL: j.b.ArchiveURL, WantBytes: j.b.ArchiveBytes,
				CacheDir: cacheDir, CacheKey: j.b.CacheKey,
			})
			start := time.Now()
			res, err := doer.Do(cctx, ToolCall{Name: OpFetchArchive, Input: input})
			switch {
			case err != nil:
				results[i] = outcome{err: err}
				log.Warn("技能包下载失败，回落到只写 SKILL.md",
					slog.String("skill", j.name), slog.Any("err", err))
			case res.IsError:
				results[i] = outcome{err: fmt.Errorf("%s", res.Text)}
				log.Warn("技能包下载失败，回落到只写 SKILL.md",
					slog.String("skill", j.name), slog.String("detail", res.Text))
			default:
				results[i] = outcome{ok: true}
				if strings.TrimSpace(res.Text) == cacheHitMarker {
					log.Info("技能包命中宿主机本地缓存，跳过下载",
						slog.String("skill", j.name), slog.Int("bytes", j.b.ArchiveBytes))
				} else {
					log.Info("技能包已下载并解压",
						slog.String("skill", j.name), slog.Int("bytes", j.b.ArchiveBytes),
						slog.Duration("took", time.Since(start).Round(time.Millisecond)))
				}
			}
		}(i, j)
	}
	wg.Wait()

	done := map[string]bool{}
	var failed []string
	for i, r := range results {
		if r.ok {
			done[jobs[i].name] = true
		} else {
			failed = append(failed, jobs[i].name)
		}
	}
	if len(failed) > 0 {
		return done, fmt.Errorf("hosttools: %d 个技能包没下成（%s）",
			len(failed), strings.Join(failed, ", "))
	}
	return done, nil
}

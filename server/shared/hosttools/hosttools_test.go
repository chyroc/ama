package hosttools

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// call builds a ToolCall.
func call(t *testing.T, name string, v any) ToolCall {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return ToolCall{Name: name, Input: b}
}

func chdirTemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// Tools use slash paths internally; TempDir starts with / on both darwin and linux.
	t.Chdir(dir)
	return dir
}

func TestReadWriteEdit(t *testing.T) {
	wd := chdirTemp(t)
	ctx := context.Background()

	// write: relative path + parent directories created automatically.
	res := ExecuteTool(ctx, wd, call(t, NameWrite, map[string]any{
		"file_path": "sub/a.txt", "content": "hello\nworld\n",
	}))
	if res.IsError {
		t.Fatalf("write: %s", res.Text)
	}
	if !strings.Contains(res.Text, "wrote 12 bytes to sub/a.txt") {
		t.Fatalf("write 输出不符: %q", res.Text)
	}
	if got, _ := os.ReadFile(filepath.Join(wd, "sub/a.txt")); string(got) != "hello\nworld\n" {
		t.Fatalf("落盘内容不符: %q", got)
	}

	// read of the whole file has no line numbers.
	res = ExecuteTool(ctx, wd, call(t, NameRead, map[string]any{"file_path": "sub/a.txt"}))
	if res.IsError || res.Text != "hello\nworld\n" {
		t.Fatalf("read 全文不符: %+v", res)
	}

	// Paged read has line numbers.
	res = ExecuteTool(ctx, wd, call(t, NameRead, map[string]any{
		"file_path": "sub/a.txt", "offset": 0, "limit": 1,
	}))
	if res.IsError || !strings.Contains(res.Text, "     1\thello") {
		t.Fatalf("read 分页不符: %+v", res)
	}

	// read with view_range has no line numbers.
	res = ExecuteTool(ctx, wd, call(t, NameRead, map[string]any{
		"file_path": "sub/a.txt", "view_range": []int{1, 1},
	}))
	if res.IsError || res.Text != "hello" {
		t.Fatalf("view_range 不符: %+v", res)
	}

	// edit with a unique replacement.
	res = ExecuteTool(ctx, wd, call(t, NameEdit, map[string]any{
		"file_path": "sub/a.txt", "old_string": "world", "new_string": "WORLD",
	}))
	if res.IsError || !strings.Contains(res.Text, "1 occurrence replaced") {
		t.Fatalf("edit 不符: %+v", res)
	}

	// edit errors when the match is not unique.
	os.WriteFile(filepath.Join(wd, "dup.txt"), []byte("x x"), 0o644)
	res = ExecuteTool(ctx, wd, call(t, NameEdit, map[string]any{
		"file_path": "dup.txt", "old_string": "x", "new_string": "y",
	}))
	if !res.IsError || !strings.Contains(res.Text, "appears 2 times") {
		t.Fatalf("edit 不唯一未报错: %+v", res)
	}

	// replace_all replaces every match.
	res = ExecuteTool(ctx, wd, call(t, NameEdit, map[string]any{
		"file_path": "dup.txt", "old_string": "x", "new_string": "y", "replace_all": true,
	}))
	if res.IsError {
		t.Fatalf("replace_all: %s", res.Text)
	}
	if got, _ := os.ReadFile(filepath.Join(wd, "dup.txt")); string(got) != "y y" {
		t.Fatalf("replace_all 落盘不符: %q", got)
	}

	// Missing file / directory / missing old_string.
	res = ExecuteTool(ctx, wd, call(t, NameRead, map[string]any{"file_path": "nope"}))
	if !res.IsError || !strings.Contains(res.Text, "no such file or directory") {
		t.Fatalf("read 不存在未报错: %+v", res)
	}
	res = ExecuteTool(ctx, wd, call(t, NameRead, map[string]any{"file_path": "sub"}))
	if !res.IsError || !strings.Contains(res.Text, "not a regular file") {
		t.Fatalf("read 目录未报错: %+v", res)
	}
	res = ExecuteTool(ctx, wd, call(t, NameEdit, map[string]any{
		"file_path": "sub/a.txt", "old_string": "不存在的串", "new_string": "z",
	}))
	if !res.IsError || !strings.Contains(res.Text, "old_string not found") {
		t.Fatalf("edit 未命中未报错: %+v", res)
	}
}

func TestWriteTooLarge(t *testing.T) {
	wd := chdirTemp(t)
	big := strings.Repeat("a", MaxWriteBytes+1)
	res := ExecuteTool(context.Background(), wd, call(t, NameWrite, map[string]any{
		"file_path": "big", "content": big,
	}))
	if !res.IsError || !strings.Contains(res.Text, "content too large") {
		t.Fatalf("超大 write 未拒绝: %+v", res)
	}
}

func TestReadBinaryRejected(t *testing.T) {
	wd := chdirTemp(t)
	p := filepath.Join(wd, "bin.dat")
	// Control characters (including NUL) are still valid UTF-8; only an invalid byte sequence
	// counts as binary.
	os.WriteFile(p, []byte{0xff, 0xfe, 0x00}, 0o644)
	res := ExecuteTool(context.Background(), wd, call(t, NameRead, map[string]any{"file_path": "bin.dat"}))
	if !res.IsError || !strings.Contains(res.Text, "binary file") {
		t.Fatalf("二进制 read 未拒绝: %+v", res)
	}
}

func TestGlob(t *testing.T) {
	wd := chdirTemp(t)
	// Put one go file at each of two depths, plus a txt.
	os.MkdirAll(filepath.Join(wd, "deep/nest"), 0o755)
	os.WriteFile(filepath.Join(wd, "a.go"), []byte("1"), 0o644)
	os.WriteFile(filepath.Join(wd, "deep/b.go"), []byte("12"), 0o644)
	os.WriteFile(filepath.Join(wd, "deep/nest/c.txt"), []byte("123"), 0o644)
	time.Sleep(20 * time.Millisecond)
	// Make a.go's mtime the newest to verify descending order: the later write comes first.
	os.WriteFile(filepath.Join(wd, "a.go"), []byte("updated"), 0o644)

	res := ExecuteTool(context.Background(), wd, call(t, NameGlob, map[string]any{"pattern": "*.go"}))
	if res.IsError {
		t.Fatalf("glob: %s", res.Text)
	}
	lines := strings.Split(strings.TrimSpace(res.Text), "\n")
	if len(lines) != 2 {
		t.Fatalf("*.go 应命中 2 个，实际 %v", lines)
	}
	if lines[0] != "a.go" || lines[1] != "deep/b.go" {
		t.Fatalf("glob 顺序应 mtime 倒序，实际 %v", lines)
	}

	// The ** pattern crosses directories.
	res = ExecuteTool(context.Background(), wd, call(t, NameGlob, map[string]any{"pattern": "**/*.txt"}))
	if res.IsError || strings.TrimSpace(res.Text) != "deep/nest/c.txt" {
		t.Fatalf("**/*.txt 不符: %+v", res)
	}

	// A root directory that does not exist.
	res = ExecuteTool(context.Background(), wd, call(t, NameGlob, map[string]any{
		"pattern": "*.go", "path": "nope",
	}))
	if !res.IsError || !strings.Contains(res.Text, "no such directory") {
		t.Fatalf("glob 坏根未报错: %+v", res)
	}
}

func TestGrep(t *testing.T) {
	wd := chdirTemp(t)
	os.MkdirAll(filepath.Join(wd, "d"), 0o755)
	os.WriteFile(filepath.Join(wd, "a.txt"), []byte("foo\nbar\nFOOagain\n"), 0o644)
	os.WriteFile(filepath.Join(wd, "d/b.txt"), []byte("foo2\nnothing\n"), 0o644)

	// content mode: foo matches line 1 of a.txt and foo2 in b.txt, but not uppercase FOO.
	res := ExecuteTool(context.Background(), wd, call(t, NameGrep, map[string]any{"pattern": "foo"}))
	if res.IsError {
		t.Fatalf("grep: %s", res.Text)
	}
	if !strings.Contains(res.Text, "a.txt:1:foo") ||
		!strings.Contains(res.Text, "d/b.txt:1:foo2") ||
		strings.Contains(res.Text, "FOOagain") {
		t.Fatalf("grep content 不符:\n%s", res.Text)
	}

	// Case-insensitive: a.txt has 2 matches (foo/FOO), b.txt has 1.
	res = ExecuteTool(context.Background(), wd, call(t, NameGrep, map[string]any{
		"pattern": "foo", "case_insensitive": true,
	}))
	if res.IsError || !strings.Contains(res.Text, "FOOagain") || !strings.Contains(res.Text, "b.txt:1:foo2") {
		t.Fatalf("grep -i 不符:\n%s", res.Text)
	}

	// files_with_matches。
	res = ExecuteTool(context.Background(), wd, call(t, NameGrep, map[string]any{
		"pattern": "foo", "output_mode": "files_with_matches",
	}))
	if res.IsError {
		t.Fatalf("grep files: %s", res.Text)
	}
	lines := strings.Split(strings.TrimSpace(res.Text), "\n")
	if len(lines) != 2 {
		t.Fatalf("files_with_matches 应有 2 个文件，实际 %v", lines)
	}

	// count。
	res = ExecuteTool(context.Background(), wd, call(t, NameGrep, map[string]any{
		"pattern": "foo", "case_insensitive": true, "output_mode": "count",
	}))
	if res.IsError || strings.TrimSpace(res.Text) != "3" {
		t.Fatalf("grep count 应 3，实际 %+v", res)
	}

	// glob_filter。
	res = ExecuteTool(context.Background(), wd, call(t, NameGrep, map[string]any{
		"pattern": "foo", "glob_filter": "a.txt",
	}))
	if res.IsError || strings.Contains(res.Text, "b.txt") || !strings.Contains(res.Text, "a.txt") {
		t.Fatalf("grep glob_filter 不符:\n%s", res.Text)
	}

	// No match is not an error.
	res = ExecuteTool(context.Background(), wd, call(t, NameGrep, map[string]any{"pattern": "zzzz"}))
	if res.IsError || res.Text != "" {
		t.Fatalf("grep 无匹配应为空成功，实际 %+v", res)
	}

	// Binary files are skipped (a short prefix containing NUL is enough to detect it).
	os.WriteFile(filepath.Join(wd, "bin.dat"), []byte{'f', 'o', 'o', 0, 0xff}, 0o644)
	res = ExecuteTool(context.Background(), wd, call(t, NameGrep, map[string]any{"pattern": "foo"}))
	if res.IsError || strings.Contains(res.Text, "bin.dat") {
		t.Fatalf("grep 不应搜二进制，实际 %+v", res)
	}

	// A bad regex reports a clear error (RE2).
	res = ExecuteTool(context.Background(), wd, call(t, NameGrep, map[string]any{"pattern": "("}))
	if !res.IsError {
		t.Fatalf("坏正则未报错: %+v", res)
	}
}

func TestBashSync(t *testing.T) {
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("无 /bin/bash")
	}
	wd := chdirTemp(t)

	// Basic three parts + exit code.
	res := ExecuteTool(context.Background(), wd, call(t, NameBash, map[string]any{
		"command": "echo hi; echo bad >&2; exit 7",
	}))
	if res.IsError {
		t.Fatalf("非零退出不该是 IsError（agent 要看到结果）: %+v", res)
	}
	if !strings.Contains(res.Text, "exit_code: 7") ||
		!strings.Contains(res.Text, "--- stdout ---\nhi") ||
		!strings.Contains(res.Text, "--- stderr ---\nbad") {
		t.Fatalf("bash 三段式不符:\n%s", res.Text)
	}

	// Pipes are left to the shell to interpret.
	res = ExecuteTool(context.Background(), wd, call(t, NameBash, map[string]any{
		"command": "echo abc | tr a-z A-Z",
	}))
	if res.IsError || !strings.Contains(res.Text, "ABC") {
		t.Fatalf("bash 管道不符: %+v", res)
	}

	// env injection.
	res = ExecuteTool(context.Background(), wd, ToolCall{
		Name:  NameBash,
		Input: json.RawMessage(`{"command":"echo $MY_TEST_VAR"}`),
		Envs:  map[string]string{"MY_TEST_VAR": "secret123"},
	})
	if res.IsError || !strings.Contains(res.Text, "secret123") {
		t.Fatalf("bash env 注入不符: %+v", res)
	}

	// Missing command reports an error.
	res = ExecuteTool(context.Background(), wd, call(t, NameBash, map[string]any{}))
	if !res.IsError {
		t.Fatalf("空 command 未报错: %+v", res)
	}
}

func TestBashTimeoutKillsGroup(t *testing.T) {
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("无 /bin/bash")
	}
	if runtime.GOOS == "windows" {
		t.Skip("信号语义仅 unix")
	}
	wd := chdirTemp(t)
	// Derive a unique fractional number of seconds from this test's pid: other tests or system
	// processes running in parallel may also have a sleep, but this value is globally unique (in
	// practice, when run in parallel with the cmd/ama-cli package, that package has its own sleep 30,
	// and pgrep "sleep 30" would match it by mistake). sleep accepts fractions (both GNU and macOS BSD
	// do), and 19.x seconds is long enough (the 200ms timeout fires first).
	secs := fmt.Sprintf("19.%04d", os.Getpid()%10000)
	start := time.Now()
	res := ExecuteTool(context.Background(), wd, call(t, NameBash, map[string]any{
		"command": "sleep " + secs, "timeout_ms": 200,
	}))
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Fatalf("超时没有在 ~200ms 触发，实际 %s", elapsed)
	}
	if !strings.Contains(res.Text, "超时") {
		t.Fatalf("超时结果应说明被终止: %+v", res)
	}
	// Leftover check: the command line builds the seconds from a variable, so pgrep -f sees the
	// unexpanded "sleep $s" and does not match the checker itself; only the target sleep's argv has the
	// expanded unique value.
	check := ExecuteTool(context.Background(), wd, call(t, NameBash, map[string]any{
		"command": fmt.Sprintf(`s=%q; pgrep -f "sleep $s" | wc -l`, secs),
	}))
	if strings.TrimSpace(lastLine(check.Text)) != "0" {
		t.Fatalf("超时后仍有 sleep %s 残留:\n%s", secs, check.Text)
	}
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

func TestBashTruncation(t *testing.T) {
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("无 /bin/bash")
	}
	wd := chdirTemp(t)
	res := ExecuteTool(context.Background(), wd, call(t, NameBash, map[string]any{
		// About 200KB, above the 64KiB business truncation.
		"command": fmt.Sprintf("head -c %d /dev/zero | tr '\\0' 'a'", 200<<10),
	}))
	if !strings.Contains(res.Text, "已截断") {
		t.Fatalf("长输出应截断并说明，长度 %d", len(res.Text))
	}
}

func TestBackground(t *testing.T) {
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("无 /bin/bash")
	}
	if runtime.GOOS == "windows" {
		t.Skip("信号语义仅 unix")
	}
	wd := chdirTemp(t)
	res := ExecuteTool(context.Background(), wd, call(t, NameBash, map[string]any{
		"command": "sleep 3; echo done", "run_in_background": true,
	}))
	if res.IsError {
		t.Fatalf("后台启动失败: %+v", res)
	}
	// Parse the pid and the file paths.
	var pid, logPath, statusPath string
	for _, line := range strings.Split(res.Text, "\n") {
		line = strings.TrimSpace(line)
		// Strip parenthetical notes like "(appears only after the task ends)"; keep only the path.
		if i := strings.IndexAny(line, "（("); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		switch {
		case strings.HasPrefix(line, "pid: "):
			pid = strings.TrimPrefix(line, "pid: ")
		case strings.HasPrefix(line, "输出文件: "):
			logPath = strings.TrimPrefix(line, "输出文件: ")
		case strings.HasPrefix(line, "退出码文件: "):
			statusPath = strings.TrimPrefix(line, "退出码文件: ")
		}
	}
	if pid == "" || logPath == "" || statusPath == "" {
		t.Fatalf("后台提示缺字段:\n%s", res.Text)
	}
	if _, err := strconv.Atoi(pid); err != nil {
		t.Fatalf("pid 非数字: %s", pid)
	}

	// Wait for the task to finish (3 seconds); the status file appears and is 0.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(statusPath); err == nil && strings.TrimSpace(string(b)) == "0" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	b, err := os.ReadFile(statusPath)
	if err != nil || strings.TrimSpace(string(b)) != "0" {
		t.Fatalf("status 未落 0: %v %q", err, b)
	}
	if out, _ := os.ReadFile(logPath); !strings.Contains(string(out), "done") {
		t.Fatalf("日志缺 done: %q", out)
	}
}

func TestWriteFilesOp(t *testing.T) {
	wd := chdirTemp(t)
	input, _ := json.Marshal(writeFilesInput{
		Dir: "skills/demo",
		Files: []SkillFile{
			{Path: "SKILL.md", Content: "# demo"},
			{Path: "scripts/a.sh", Content: "echo hi"},
		},
	})
	res := ExecuteTool(context.Background(), wd, ToolCall{Name: OpWriteFiles, Input: input})
	if res.IsError {
		t.Fatalf("write_files: %s", res.Text)
	}
	for _, rel := range []string{"skills/demo/SKILL.md", "skills/demo/scripts/a.sh"} {
		if _, err := os.Stat(filepath.Join(wd, rel)); err != nil {
			t.Fatalf("文件未落地 %s: %v", rel, err)
		}
	}

	// Zip Slip is blocked.
	bad, _ := json.Marshal(writeFilesInput{
		Dir:   "skills/demo",
		Files: []SkillFile{{Path: "../evil", Content: "x"}},
	})
	res = ExecuteTool(context.Background(), wd, ToolCall{Name: OpWriteFiles, Input: bad})
	if !res.IsError {
		t.Fatalf("路径穿越未被拦: %+v", res)
	}
}

func TestFetchArchive(t *testing.T) {
	wd := chdirTemp(t)

	// Build a zip: top-level demo/ containing SKILL.md.
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	w, _ := zw.Create("demo/SKILL.md")
	w.Write([]byte("# demo skill"))
	w2, _ := zw.Create("demo/scripts/run.sh")
	w2.Write([]byte("echo ok"))
	zw.Close()
	payload := zb.Bytes()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		w.Write(payload)
	}))
	defer srv.Close()

	target := filepath.Join(wd, "skills", "demo")
	input, _ := json.Marshal(fetchArchiveInput{
		Dir: target, URL: srv.URL, WantBytes: len(payload),
	})
	res := ExecuteTool(context.Background(), wd, ToolCall{Name: OpFetchArchive, Input: input})
	if res.IsError {
		t.Fatalf("fetch_archive: %s", res.Text)
	}
	if got, err := os.ReadFile(filepath.Join(target, "SKILL.md")); err != nil || string(got) != "# demo skill" {
		t.Fatalf("解压内容不符: %q %v", got, err)
	}

	// Byte count mismatch (truncated) -> error.
	input2, _ := json.Marshal(fetchArchiveInput{
		Dir: filepath.Join(wd, "skills2", "demo"), URL: srv.URL, WantBytes: len(payload) + 10,
	})
	res = ExecuteTool(context.Background(), wd, ToolCall{Name: OpFetchArchive, Input: input2})
	if !res.IsError || !strings.Contains(res.Text, "字节数不对") {
		t.Fatalf("截断下载未报错: %+v", res)
	}

	// 403 -> error, and the message does not contain the URL.
	srv403 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv403.Close()
	input3, _ := json.Marshal(fetchArchiveInput{
		Dir: filepath.Join(wd, "skills3", "demo"), URL: srv403.URL, WantBytes: 1,
	})
	res = ExecuteTool(context.Background(), wd, ToolCall{Name: OpFetchArchive, Input: input3})
	if !res.IsError {
		t.Fatalf("403 未报错: %+v", res)
	}
	if strings.Contains(res.Text, "127.0.0.1") || strings.Contains(res.Text, srv403.URL) {
		t.Fatalf("错误文案泄漏了 URL: %s", res.Text)
	}

	// 500 fails the first time, succeeds the second: it should retry and then write to disk
	// (verifies that 5xx and 4xx take different branches).
	flakyHits := 0
	srv500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flakyHits++
		if flakyHits == 1 {
			http.Error(w, "busy", http.StatusInternalServerError)
			return
		}
		w.Write(payload)
	}))
	defer srv500.Close()
	input5xx, _ := json.Marshal(fetchArchiveInput{
		Dir: filepath.Join(wd, "skills5xx", "demo"), URL: srv500.URL, WantBytes: len(payload),
	})
	res = ExecuteTool(context.Background(), wd, ToolCall{Name: OpFetchArchive, Input: input5xx})
	if res.IsError {
		t.Fatalf("5xx 后重试应成功: %+v", res)
	}
	if flakyHits != 2 {
		t.Fatalf("应请求 2 次（首 500 后重试），实际 %d", flakyHits)
	}
	if _, err := os.Stat(filepath.Join(wd, "skills5xx", "demo", "SKILL.md")); err != nil {
		t.Fatalf("5xx 重试成功后文件未落盘: %v", err)
	}

	// A Zip Slip package is rejected.
	var evil bytes.Buffer
	ezw := zip.NewWriter(&evil)
	ew, _ := ezw.Create("../escape.txt")
	ew.Write([]byte("x"))
	ezw.Close()
	srvEvil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(evil.Bytes())
	}))
	defer srvEvil.Close()
	input4, _ := json.Marshal(fetchArchiveInput{
		Dir: filepath.Join(wd, "skills4", "demo"), URL: srvEvil.URL, WantBytes: evil.Len(),
	})
	res = ExecuteTool(context.Background(), wd, ToolCall{Name: OpFetchArchive, Input: input4})
	// The top level of ../escape.txt is not the skill name demo, so it is caught by "the top level
	// must be the skill name" (also a safe rejection).
	if !res.IsError ||
		(!strings.Contains(res.Text, "越界") && !strings.Contains(res.Text, "顶层目录必须是")) {
		t.Fatalf("Zip Slip 未被拦: %+v", res)
	}

	// The package's top level is another skill's directory (other/...): it must not pollute
	// siblings and must report an error.
	var wrong bytes.Buffer
	wzw := zip.NewWriter(&wrong)
	wf, _ := wzw.Create("other/x.txt")
	wf.Write([]byte("x"))
	wzw.Close()
	srvWrong := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(wrong.Bytes())
	}))
	defer srvWrong.Close()
	input5, _ := json.Marshal(fetchArchiveInput{
		Dir: filepath.Join(wd, "skills5", "demo"), URL: srvWrong.URL, WantBytes: wrong.Len(),
	})
	res = ExecuteTool(context.Background(), wd, ToolCall{Name: OpFetchArchive, Input: input5})
	if !res.IsError || !strings.Contains(res.Text, "顶层目录必须是") {
		t.Fatalf("错误顶层目录未被拦: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(wd, "skills5", "other")); !os.IsNotExist(err) {
		t.Fatalf("错误顶层目录不应被解出来")
	}
}

func TestFetchArchiveCache(t *testing.T) {
	wd := chdirTemp(t)
	hits := 0
	var payload []byte
	{
		var zb bytes.Buffer
		zw := zip.NewWriter(&zb)
		w, _ := zw.Create("c/SKILL.md")
		w.Write([]byte("# c"))
		zw.Close()
		payload = zb.Bytes()
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write(payload)
	}))
	defer srv.Close()

	// CacheDir is relative to $HOME (same meaning as hostproto.SkillArchiveCacheDir): pin HOME to a
	// temp directory to verify the cache lands in $HOME/.ama/cache rather than the session cwd, and
	// does not pollute the real home directory.
	home := t.TempDir()
	t.Setenv("HOME", home)
	cacheDir := ".ama/cache/skill-archives"
	key := strings.Repeat("a", 64)
	mk := func(dir string) json.RawMessage {
		b, _ := json.Marshal(fetchArchiveInput{
			Dir: dir, URL: srv.URL, WantBytes: len(payload),
			CacheDir: cacheDir, CacheKey: key,
		})
		return b
	}
	r1 := ExecuteTool(context.Background(), wd, ToolCall{Name: OpFetchArchive, Input: mk(filepath.Join(wd, "s1", "c"))})
	if r1.IsError || r1.Text == cacheHitMarker {
		t.Fatalf("首次应下载且非缓存: %+v", r1)
	}
	// The cache file must land under $HOME.
	if _, err := os.Stat(filepath.Join(home, cacheDir, key+".zip")); err != nil {
		t.Fatalf("缓存应落在 $HOME/%s/<key>.zip: %v", cacheDir, err)
	}
	r2 := ExecuteTool(context.Background(), wd, ToolCall{Name: OpFetchArchive, Input: mk(filepath.Join(wd, "s2", "c"))})
	if r2.IsError || r2.Text != cacheHitMarker {
		t.Fatalf("二次应命中缓存: %+v", r2)
	}
	if hits != 1 {
		t.Fatalf("缓存命中不应触网，实际请求 %d 次", hits)
	}
}

func TestUnknownTool(t *testing.T) {
	res := ExecuteTool(context.Background(), chdirTemp(t), ToolCall{Name: "nope"})
	if !res.IsError || !strings.Contains(res.Text, "不认识的工具") {
		t.Fatalf("未知工具未报错: %+v", res)
	}
}

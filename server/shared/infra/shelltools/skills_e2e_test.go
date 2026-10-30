package shelltools

import (
	"archive/zip"
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Runs ArchiveScript end to end: a real zip, a real HTTP server, real bash + curl + unzip.
//
// # Why this is needed when unit tests already assert the script text
//
// Those assertions check "does some string appear in the script". They **cannot prove the script
// really runs**: wrong quoting, wrong unpack level, unexpanded variables all pass string
// assertions and then fail in the container. And failures on the container side give zero errors
// (the agent just cannot read the files).
//
// Here we really run it with the local bash/curl/unzip. The only difference from a real container
// is "kernel and image", while the script logic itself (levels, checks, clearing the directory)
// is the same.
func TestArchiveScriptEndToEndWithRealTools(t *testing.T) {
	for _, bin := range []string{"bash", "curl", "unzip"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("本机没有 %s，跳过（容器里有）", bin)
		}
	}

	// Build a package with the same shape as BuildSkillZip: exactly one directory at the top level.
	const skillName = "probe-skill"
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	want := map[string]string{
		skillName + "/SKILL.md":            "# 探针技能\n正文\n",
		skillName + "/references/notes.md": "参考资料\n",
		skillName + "/scripts/run.sh":      "#!/bin/sh\necho hi\n",
	}
	for name, body := range want {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	blob := buf.Bytes()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(blob)
	}))
	defer srv.Close()

	root := t.TempDir()
	dir := filepath.Join(root, "skills", skillName)

	// First put in a leftover file from a previous version, to check that "clear the directory
	// before unpacking" really works.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "STALE-FROM-OLD-VERSION.md")
	if err := os.WriteFile(stale, []byte("上一版残留"), 0o644); err != nil {
		t.Fatal(err)
	}

	runScript(t, ArchiveScript(dir, srv.URL, len(blob)))

	// ① All three files are at the **right level**.
	for rel, body := range want {
		p := filepath.Join(root, "skills", rel)
		got, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("%s 不在对的位置：%v", rel, err)
			continue
		}
		if string(got) != body {
			t.Errorf("%s 内容不对：%q", rel, got)
		}
	}

	// ② **No extra level.** The zip top level is already <skill name>/; unpacking into the skill
	// directory would give skills/x/x/SKILL.md, while the agent looks for ./skills/x/SKILL.md. It
	// cannot read it, with zero errors.
	if _, err := os.Stat(filepath.Join(dir, skillName)); err == nil {
		t.Errorf("多套了一层：%s 下面又出现了一个 %s 目录", dir, skillName)
	}

	// ③ Leftovers from the previous version must be cleared. Otherwise old and new files mix,
	// and the agent reads a skill where "some files are the old version".
	if _, err := os.Stat(stale); err == nil {
		t.Error("解压前没清目录——上一版的残留还在，会与新版混在一起")
	}
}

// It must fail when the byte count does not match.
//
// ⚠️ When the connection drops, curl leaves an **incomplete but existing** file and exits 0 (as
// long as the server already replied 200). Without the check unzip may extract only half the
// files, and a broken skill is worse than a missing one.
func TestArchiveScriptFailsOnTruncatedDownload(t *testing.T) {
	for _, bin := range []string{"bash", "curl"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("本机没有 %s，跳过", bin)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("短了一截"))
	}))
	defer srv.Close()

	dir := filepath.Join(t.TempDir(), "skills", "probe-skill")
	// Declare 99999 expected bytes, but only serve a few.
	out, err := exec.Command("bash", "-lc", ArchiveScript(dir, srv.URL, 99999)).CombinedOutput()
	if err == nil {
		t.Fatal("字节数对不上时脚本必须以非零退出——否则截断的下载会被当成成功")
	}
	if !strings.Contains(string(out), "字节数不对") {
		t.Errorf("报错要说清是字节数不对，实际：%s", out)
	}
}

// HTTP errors (403/404) must make the script fail.
//
// ⚠️ Without `-f`, curl **writes the 403 error page into the file** and exits 0, then unzip says
// "not a zip file". That message points to a broken package, not to an expired signature.
func TestArchiveScriptFailsOnHTTPError(t *testing.T) {
	for _, bin := range []string{"bash", "curl"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("本机没有 %s，跳过", bin)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"auth failed"}`))
	}))
	defer srv.Close()

	dir := filepath.Join(t.TempDir(), "skills", "probe-skill")
	out, err := exec.Command("bash", "-lc", ArchiveScript(dir, srv.URL, 100)).CombinedOutput()
	if err == nil {
		t.Fatalf("HTTP 403 必须让脚本失败，实际成功了。输出：%s", out)
	}
	// The error page must not be unpacked as a zip.
	if strings.Contains(string(out), "cannot find or open") {
		t.Error("走到 unzip 才失败，说明 curl 没带 -f——报错会指向「包坏了」而不是「没权限」")
	}
}

func runScript(t *testing.T, script string) {
	t.Helper()
	out, err := exec.Command("bash", "-lc", script).CombinedOutput()
	if err != nil {
		t.Fatalf("脚本执行失败：%v\n输出：%s", err, out)
	}
}

// When a download fails, **the signed URL must never appear in stderr**.
//
// stderr is logged by pullSkillArchives (slog → log platform), and the URL carries a signature,
// so it is a time-limited pass: logs may be pasted into issues and seen by others.
//
// ⚠️ **This test is "always green" with the local curl.** A mutation check confirmed it: replacing
// the sed line in the script with cat (no filtering at all) still passes, because this local curl
// version does not echo the URL on 403.
//
// So why keep it, and keep that sed line? Because **"curl does not echo it" is not something we
// can guarantee**: it changes with the version, the error type, and the --retry retry messages,
// and the curl in the container image is not the same as the local one. An always-green test here
// is not about "catching a bug now" but "if one day we switch to a curl that echoes, it starts
// catching". For that reason another assertion that **does not depend on curl behavior** was
// added below (see TestArchiveScriptRedactsURLInErrorOutput), and that one can really fail.
func TestArchiveScriptNeverLeaksURLToStderr(t *testing.T) {
	for _, bin := range []string{"bash", "curl", "sed"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("本机没有 %s，跳过", bin)
		}
	}

	const secret = "SUPER-SECRET-SIGNATURE-abc123"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("denied"))
	}))
	defer srv.Close()
	url := srv.URL + "/obj/x.zip?X-Signature=" + secret

	dir := filepath.Join(t.TempDir(), "skills", "probe-skill")
	out, err := exec.Command("bash", "-lc", ArchiveScript(dir, url, 100)).CombinedOutput()
	if err == nil {
		t.Fatal("403 应当让脚本失败")
	}
	if strings.Contains(string(out), secret) {
		t.Errorf("签名泄漏进了输出：%s", out)
	}
	// Also confirm it **really reported an error**, rather than being filtered into blank output
	// (that would leave no clue for debugging).
	if strings.TrimSpace(string(out)) == "" {
		t.Error("滤掉 URL 之后不该什么都不剩——那样线上失败时无从查起")
	}
}

// Directly checks that the sed line really replaces, **without depending on whether curl echoes
// the URL**.
//
// The one above is "end to end but always green"; this one is "not end to end but can fail". Only
// the two together make sense: the first makes sure the real path does not leak, the second makes
// sure the filtering is really being done.
func TestArchiveScriptRedactsURLInErrorOutput(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("本机没有 bash")
	}
	const secret = "SUPER-SECRET-SIGNATURE-abc123"
	url := "https://storage.example.test/obj/x.zip?X-Signature=" + secret

	script := ArchiveScript("/tmp/nonexistent-dir/skills/x", url, 100)
	// Replace curl in the script with a stub that "pretends to fail and dumps the URL into stderr
	// unchanged", so this case checks **our sed line**, not curl's behavior.
	stub := strings.Replace(script,
		"curl -fsS --retry 3 --retry-delay 1 -m 300 -o \"$tmp\" \"$url\" 2>\"$curl_err\"",
		"{ echo \"curl: (22) failed for $url\" >\"$curl_err\"; false; }", 1)
	if stub == script {
		t.Fatal("没能替换掉 curl 那行——脚本形状变了，这条用例要跟着改")
	}

	out, err := exec.Command("bash", "-lc", stub).CombinedOutput()
	if err == nil {
		t.Fatal("桩让 curl 失败了，脚本应当非零退出")
	}
	if strings.Contains(string(out), secret) {
		t.Errorf("签名没被滤掉：%s", out)
	}
	if !strings.Contains(string(out), "已隐去") {
		t.Errorf("应当留下一个「已隐去」的占位，否则排查时看不出这里有过一个 URL：%s", out)
	}
}

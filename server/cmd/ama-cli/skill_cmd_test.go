// Unit tests for `ama skill upload`: httptest starts a fake amad to verify the shape of the
// multipart form (field name / file name / bytes / auth header) and the --dry-run query. No real
// network.
//
// This only verifies that **the request we send** is right; parsing the skill package (zip or bare
// md, how frontmatter is parsed) is the server's job, and the fake server deliberately does not copy
// that logic: a copy would mean two sets of parsing rules that can drift, and nothing would report
// the drift.
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSkillAMA records the one upload it receives and only implements the one route the CLI uses.
type fakeSkillAMA struct {
	gotWorkspaceID string
	gotAuth        string
	gotDryRun      string
	gotFilename    string
	gotBytes       []byte
	formErr        error
}

func (f *fakeSkillAMA) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/workspaces/{workspaceId}/skills/upload", func(w http.ResponseWriter, r *http.Request) {
		f.gotWorkspaceID = r.PathValue("workspaceId")
		f.gotAuth = r.Header.Get("Authorization")
		f.gotDryRun = r.URL.Query().Get("dryRun")

		file, header, err := r.FormFile(skillUploadFieldName)
		if err != nil {
			f.formErr = err
			http.Error(w, "表单里没有 "+skillUploadFieldName+" 字段："+err.Error(), http.StatusBadRequest)
			return
		}
		defer func() { _ = file.Close() }()
		b, err := io.ReadAll(file)
		if err != nil {
			f.formErr = err
			http.Error(w, "读表单失败："+err.Error(), http.StatusBadRequest)
			return
		}
		f.gotFilename = header.Filename
		f.gotBytes = b

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "skill-1", "name": "demo", "workspaceId": r.PathValue("workspaceId"),
		})
	})
	return mux
}

func newSkillTestClient(srv *httptest.Server) *client {
	return &client{
		baseURL:     srv.URL,
		token:       "test-token",
		workspaceID: "ws_test",
		http:        srv.Client(),
		httpLong:    srv.Client(),
	}
}

// newSkillTestClientOffline is for cases that "should fail locally and never send a request".
func newSkillTestClientOffline(t *testing.T) *client {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	return newSkillTestClient(srv)
}

func writeSkillFile(t *testing.T, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSkillUploadSendsMultipartFile(t *testing.T) {
	fake := &fakeSkillAMA{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	c := newSkillTestClient(srv)

	content := []byte("---\nname: demo\ndescription: 演示\n---\n正文\n")
	path := writeSkillFile(t, "SKILL.md", content)

	if err := c.skill([]string{"upload", path}); err != nil {
		t.Fatal(err)
	}
	if fake.formErr != nil {
		t.Fatalf("假服务没解出表单：%v", fake.formErr)
	}
	if fake.gotAuth != "Bearer test-token" {
		t.Errorf("Authorization = %q", fake.gotAuth)
	}
	if fake.gotWorkspaceID != "ws_test" {
		t.Errorf("workspaceId = %q，应回落到注入的 ws_test", fake.gotWorkspaceID)
	}
	// The file name must be sent as is: the server decides **by extension** whether it is zip or bare md, and renaming it changes the parsing.
	if fake.gotFilename != "SKILL.md" {
		t.Errorf("表单里的文件名 = %q，应为 SKILL.md", fake.gotFilename)
	}
	if !bytes.Equal(fake.gotBytes, content) {
		t.Errorf("上传字节与源文件不一致：%q", fake.gotBytes)
	}
	if fake.gotDryRun != "" {
		t.Errorf("没给 --dry-run 时不该带 dryRun，实际 %q", fake.gotDryRun)
	}
}

// TestSkillUploadDryRunBeforePath watches that parseFlags trap: in `upload --dry-run foo.zip`,
// parseFlags would take foo.zip as the value of --dry-run. Going through hasFlag must both
// recognize the switch and keep the path.
func TestSkillUploadDryRunBeforePath(t *testing.T) {
	fake := &fakeSkillAMA{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	c := newSkillTestClient(srv)

	content := []byte("假装是个 zip\n")
	path := writeSkillFile(t, "foo.zip", content)

	if err := c.skill([]string{"upload", "--dry-run", path}); err != nil {
		t.Fatal(err)
	}
	if fake.gotDryRun != "true" {
		t.Errorf("dryRun = %q，应为 true", fake.gotDryRun)
	}
	if fake.gotFilename != "foo.zip" {
		t.Errorf("文件名 = %q，应为 foo.zip（开关不能把路径吃掉）", fake.gotFilename)
	}
	if !bytes.Equal(fake.gotBytes, content) {
		t.Errorf("上传字节 = %q", fake.gotBytes)
	}
}

// TestSkillUploadDryRunValueForm watches the miss in the other direction: if `--dry-run=true` (the
// --k=v form parseFlags supports) were treated as "no dry-run given", it would **silently create the
// skill for real**; an operation that should only preview would have a side effect. Conversely,
// `--dry-run=false` must really mean off.
func TestSkillUploadDryRunValueForm(t *testing.T) {
	cases := []struct {
		flag    string
		wantDry string
	}{
		{"--dry-run=true", "true"},
		{"--dry-run=1", "true"},
		{"--dry-run=false", ""},
		{"--dry-run=0", ""},
	}
	for _, tc := range cases {
		t.Run(tc.flag, func(t *testing.T) {
			fake := &fakeSkillAMA{}
			srv := httptest.NewServer(fake.handler())
			defer srv.Close()
			c := newSkillTestClient(srv)

			path := writeSkillFile(t, "SKILL.md", []byte("x"))
			if err := c.skill([]string{"upload", path, tc.flag}); err != nil {
				t.Fatal(err)
			}
			if fake.gotDryRun != tc.wantDry {
				t.Errorf("%s 时 dryRun = %q，应为 %q", tc.flag, fake.gotDryRun, tc.wantDry)
			}
			if fake.gotFilename != "SKILL.md" {
				t.Errorf("文件名 = %q", fake.gotFilename)
			}
		})
	}
}
func TestSkillUploadWorkspaceFlag(t *testing.T) {
	fake := &fakeSkillAMA{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	c := newSkillTestClient(srv)

	path := writeSkillFile(t, "SKILL.md", []byte("x"))
	if err := c.skill([]string{"upload", path, "--workspace", "ws_other"}); err != nil {
		t.Fatal(err)
	}
	if fake.gotWorkspaceID != "ws_other" {
		t.Errorf("workspaceId = %q，--workspace 应优先于注入值", fake.gotWorkspaceID)
	}
}

func TestSkillUploadRequiresPath(t *testing.T) {
	c := newSkillTestClientOffline(t)
	err := c.skill([]string{"upload"})
	if err == nil {
		t.Fatal("没给路径应当报错")
	}
	if !strings.Contains(err.Error(), "路径") {
		t.Errorf("报错要说清缺什么，实际：%v", err)
	}
}

func TestSkillUploadRejectsDirectory(t *testing.T) {
	c := newSkillTestClientOffline(t)
	err := c.skill([]string{"upload", t.TempDir()})
	if err == nil {
		t.Fatal("目录应当报错")
	}
	if !strings.Contains(err.Error(), "目录") {
		t.Errorf("报错要说清是目录，实际：%v", err)
	}
}

func TestSkillUploadRejectsTooLarge(t *testing.T) {
	c := newSkillTestClientOffline(t)
	path := filepath.Join(t.TempDir(), "huge.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// A sparse file: only stretch the logical size to the limit+1, taking no real disk and not really read (the size check comes before reading).
	if err := f.Truncate(maxSkillUploadBytes + 1); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	err = c.skill([]string{"upload", path})
	if err == nil {
		t.Fatal("超过上限应当报错")
	}
	if !strings.Contains(err.Error(), "太大") {
		t.Errorf("报错要说清太大，实际：%v", err)
	}
}

func TestSkillUploadMissingFile(t *testing.T) {
	c := newSkillTestClientOffline(t)
	err := c.skill([]string{"upload", filepath.Join(t.TempDir(), "不存在.md")})
	if err == nil {
		t.Fatal("文件不存在应当报错")
	}
	if !strings.Contains(err.Error(), "打不开") {
		t.Errorf("报错要说清打不开，实际：%v", err)
	}
}

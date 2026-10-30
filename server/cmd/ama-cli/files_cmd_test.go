// Unit tests for ama files: httptest starts a fake amad to verify the upload protocol (simple /
// multipart) and the shape of the download/delete commands. No real network and no real storage: the
// signed URL in the fake server is just a byte path it hosts itself (verifying "no AMA header");
// download goes through the AMA relay (verifying "with token").
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeAMA records uploads in progress and simulates amad's 8 /api/files routes (only the ones the CLI uses).
type fakeAMA struct {
	mu        sync.Mutex
	files     map[string]cliUserFile
	objects   map[string][]byte // completed object id -> bytes
	parts     map[string]map[int][]byte
	tokenSeen string // download requests should not carry Authorization; verified via a separate host outside the test
}

func newFakeAMA() *fakeAMA {
	return &fakeAMA{
		files:   map[string]cliUserFile{},
		objects: map[string][]byte{},
		parts:   map[string]map[int][]byte{},
	}
}

func (f *fakeAMA) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/files", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Filename string `json:"filename"`
			Size     int64  `json:"size"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		id := "uf_" + strconv.Itoa(len(f.files)+1)
		mode := "simple"
		var partSize int64
		if in.Size > 8*1024*1024 || in.Size == 0 {
			mode = "multipart"
			partSize = 8 * 1024 * 1024
		}
		uf := cliUserFile{
			ID: id, Filename: in.Filename, Size: 0,
			Status: "uploading", UploadMode: mode, PartSize: partSize,
		}
		f.mu.Lock()
		f.files[id] = uf
		if mode == "multipart" {
			f.parts[id] = map[int][]byte{}
		}
		f.mu.Unlock()
		writeJSON2(w, 201, uf)
	})

	mux.HandleFunc("PUT /api/files/{id}/content", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		uf := f.files[id]
		uf.Status, uf.Size = "completed", int64(len(b))
		f.files[id] = uf
		f.objects[id] = b
		f.mu.Unlock()
		writeJSON2(w, 200, uf)
	})

	mux.HandleFunc("PUT /api/files/{id}/parts/{n}", func(w http.ResponseWriter, r *http.Request) {
		id, nStr := r.PathValue("id"), r.PathValue("n")
		n, _ := strconv.Atoi(nStr)
		b, _ := io.ReadAll(r.Body)
		// Check Content-Length matches the actual size (the CLI uses SectionReader + req.ContentLength).
		if r.ContentLength != int64(len(b)) {
			http.Error(w, fmt.Sprintf("length mismatch %d vs %d", r.ContentLength, len(b)), 400)
			return
		}
		f.mu.Lock()
		f.parts[id][n] = append([]byte(nil), b...)
		f.mu.Unlock()
		writeJSON2(w, 200, map[string]any{"partNumber": n})
	})

	mux.HandleFunc("POST /api/files/{id}/complete", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		f.mu.Lock()
		var all []byte
		for n := 1; n <= len(f.parts[id]); n++ {
			all = append(all, f.parts[id][n]...)
		}
		uf := f.files[id]
		uf.Status, uf.Size = "completed", int64(len(all))
		f.files[id] = uf
		f.objects[id] = all
		f.mu.Unlock()
		writeJSON2(w, 200, uf)
	})

	mux.HandleFunc("POST /api/files/{id}/abort", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		delete(f.files, r.PathValue("id"))
		f.mu.Unlock()
		w.WriteHeader(204)
	})

	mux.HandleFunc("GET /api/files/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		uf, ok := f.files[r.PathValue("id")]
		f.mu.Unlock()
		if !ok {
			http.Error(w, `{"code":"not_found","message":"不存在"}`, 404)
			return
		}
		if uf.Status == "completed" {
			// Points to the same fake server but without the /api prefix, to verify download carries
			// no Authorization. httptest's r.URL.Scheme is empty, so the scheme is fixed to http.
			uf.DownloadURL = "http://" + r.Host + "/download/" + uf.ID
			uf.ExpiresInSeconds = 3600
		}
		writeJSON2(w, 200, uf)
	})

	mux.HandleFunc("DELETE /api/files/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		delete(f.files, r.PathValue("id"))
		delete(f.objects, r.PathValue("id"))
		f.mu.Unlock()
		w.WriteHeader(204)
	})

	// Relayed download: ama files download uses this (object storage may not be reachable from the
	// container); it calls AMA itself, so it **must** carry Authorization.
	mux.HandleFunc("GET /api/files/{id}/content", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, `{"code":"unauthenticated","message":"没带 token"}`, 401)
			return
		}
		f.mu.Lock()
		b := f.objects[r.PathValue("id")]
		f.mu.Unlock()
		if b == nil {
			http.Error(w, `{"code":"not_found","message":"不存在"}`, 404)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.Write(b)
	})

	// The presigned download endpoint (the one ama files url prints): assert there is no Authorization header.
	mux.HandleFunc("GET /download/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			http.Error(w, "download 不该带 Authorization", 400)
			return
		}
		f.mu.Lock()
		b := f.objects[r.PathValue("id")]
		f.mu.Unlock()
		if b == nil {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	})

	return mux
}

func writeJSON2(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func newCLIClientFor(t *testing.T, srv *httptest.Server) *client {
	t.Helper()
	c := &client{
		baseURL:  srv.URL,
		token:    "test-token",
		http:     srv.Client(),
		httpLong: srv.Client(),
	}
	return c
}

func TestFilesUploadSimple(t *testing.T) {
	fake := newFakeAMA()
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	c := newCLIClientFor(t, srv)

	path := filepath.Join(t.TempDir(), "hello.txt")
	os.WriteFile(path, []byte("hello world"), 0o644)

	if err := c.filesCmd([]string{"upload", path}); err != nil {
		t.Fatal(err)
	}
	if len(fake.objects) != 1 {
		t.Fatalf("应有 1 个完成对象，实际 %d", len(fake.objects))
	}
	for _, b := range fake.objects {
		if string(b) != "hello world" {
			t.Errorf("对象字节 = %q", b)
		}
	}
}

func TestFilesUploadMultipart(t *testing.T) {
	fake := newFakeAMA()
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	c := newCLIClientFor(t, srv)

	// 17 MiB: 8 + 8 + 1, three parts.
	path := filepath.Join(t.TempDir(), "big.bin")
	content := bytes.Repeat([]byte("Z"), 17*1024*1024+1)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.filesCmd([]string{"upload", path}); err != nil {
		t.Fatal(err)
	}
	if len(fake.objects) != 1 {
		t.Fatalf("合并后应有 1 个对象，实际 %d", len(fake.objects))
	}
	for _, b := range fake.objects {
		if int64(len(b)) != int64(17*1024*1024+1) {
			t.Errorf("对象大小 = %d", len(b))
		}
		if !bytes.Equal(b, content) {
			t.Error("合并字节与源文件不一致")
		}
	}
	// All three parts must arrive (on a length mismatch the fake returns 400, and the CLI fails after retries run out).
	if len(fake.parts[firstID(fake)]) != 3 {
		t.Errorf("应传 3 片，实际 %d", len(fake.parts[firstID(fake)]))
	}
}

func firstID(f *fakeAMA) string {
	for id := range f.parts {
		return id
	}
	return ""
}

func TestFilesDeleteRequiresConfirm(t *testing.T) {
	fake := newFakeAMA()
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	c := newCLIClientFor(t, srv)

	fake.files["uf_x"] = cliUserFile{ID: "uf_x", Status: "completed"}
	if err := c.filesCmd([]string{"delete", "uf_x"}); err == nil {
		t.Fatal("不带 --yes 应被拒")
	}
	if _, ok := fake.files["uf_x"]; !ok {
		t.Error("被拒时不能真删")
	}
	if err := c.filesCmd([]string{"delete", "uf_x", "--yes"}); err != nil {
		t.Fatalf("--yes 后应删除：%v", err)
	}
	if _, ok := fake.files["uf_x"]; ok {
		t.Error("文件没被删")
	}
}

func TestFilesURLPrintsBareURL(t *testing.T) {
	fake := newFakeAMA()
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	c := newCLIClientFor(t, srv)
	fake.files["uf_x"] = cliUserFile{ID: "uf_x", Status: "completed"}

	var out bytes.Buffer
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	err := c.filesCmd([]string{"url", "uf_x"})
	w.Close()
	os.Stdout = old
	io.Copy(&out, r)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(out.String())
	if !strings.HasPrefix(got, srv.URL+"/download/uf_x") {
		t.Errorf("url 应只输出下载链接，得到 %q", got)
	}
}

func TestFilesDownloadWithoutToken(t *testing.T) {
	fake := newFakeAMA()
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	c := newCLIClientFor(t, srv)
	fake.files["uf_x"] = cliUserFile{ID: "uf_x", Filename: "d.bin", Status: "completed"}
	fake.objects["uf_x"] = []byte("payload")

	dir := t.TempDir()
	dest := filepath.Join(dir, "out.bin")
	// Switch to a temp directory and use an explicit dest, to avoid polluting the repo.
	if err := c.filesCmd([]string{"download", "uf_x", dest}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "payload" {
		t.Errorf("下载字节 = %q", got)
	}
}

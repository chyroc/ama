package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// frame builds one multiplexed output frame.
func frame(stream byte, payload string) []byte {
	var hdr [8]byte
	hdr[0] = stream
	binary.BigEndian.PutUint32(hdr[4:], uint32(len(payload)))
	return append(hdr[:], payload...)
}

func TestDemuxSplitsInterleavedStreams(t *testing.T) {
	var raw bytes.Buffer
	raw.Write(frame(1, "hello "))
	raw.Write(frame(2, "oops\n"))
	raw.Write(frame(1, "world\n"))
	raw.Write(frame(2, ""))

	var out, errb bytes.Buffer
	if err := Demux(&raw, &out, &errb, 0); err != nil {
		t.Fatalf("Demux: %v", err)
	}
	if out.String() != "hello world\n" {
		t.Errorf("stdout = %q", out.String())
	}
	if errb.String() != "oops\n" {
		t.Errorf("stderr = %q", errb.String())
	}
}

// Over the limit it must truncate, report ErrOutputTooLarge, and **drain the stream** (otherwise
// the daemon side gets stuck on back pressure).
func TestDemuxTruncatesButDrains(t *testing.T) {
	var raw bytes.Buffer
	raw.Write(frame(1, "0123456789"))
	raw.Write(frame(1, "abcdef"))
	raw.Write(frame(2, "e"))
	r := bytes.NewReader(raw.Bytes())

	var out, errb bytes.Buffer
	err := Demux(r, &out, &errb, 12)
	if !errors.Is(err, ErrOutputTooLarge) {
		t.Fatalf("要 ErrOutputTooLarge，实际 %v", err)
	}
	if out.String() != "0123456789ab" {
		t.Errorf("stdout 应保留前 12 字节，实际 %q", out.String())
	}
	if errb.String() != "e" {
		t.Errorf("stderr 不受 stdout 的上限影响，实际 %q", errb.String())
	}
	if r.Len() != 0 {
		t.Errorf("流没读完，还剩 %d 字节", r.Len())
	}
}

// Decoding raw TTY output as multiplexed frames must fail, not produce garbage.
func TestDemuxRejectsRawTTYOutput(t *testing.T) {
	var out bytes.Buffer
	err := Demux(strings.NewReader("root@ama:/workspace# ls\r\n"), &out, &out, 0)
	if err == nil {
		t.Fatal("原始字节流应当被认出来")
	}
}

func TestDemuxTruncatedHeader(t *testing.T) {
	var out bytes.Buffer
	if err := Demux(bytes.NewReader([]byte{1, 0, 0}), &out, &out, 0); err == nil {
		t.Fatal("帧头只有半截要报错")
	}
}

// ---------------------------------------------------------------------------
// Fake Engine API
// ---------------------------------------------------------------------------

type fakeEngine struct {
	t       *testing.T
	mu      sync.Mutex
	calls   []string
	stdin   bytes.Buffer
	execEnv []string
}

func (f *fakeEngine) record(r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
	f.mu.Unlock()
}

func (f *fakeEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.record(r)
	if !strings.HasPrefix(r.URL.Path, "/v1.41/") {
		http.Error(w, `{"message":"bad version"}`, http.StatusBadRequest)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1.41")
	writeJSON := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case path == "/_ping":
		_, _ = w.Write([]byte("OK"))
	case path == "/info":
		writeJSON(200, map[string]string{"ID": "daemon-1", "Name": "host", "ServerVersion": "27.0"})
	case path == "/containers/create":
		if r.URL.Query().Get("name") == "taken" {
			writeJSON(409, map[string]string{"message": "Conflict. The container name is already in use"})
			return
		}
		var cfg ContainerConfig
		_ = json.NewDecoder(r.Body).Decode(&cfg)
		if cfg.Image != "img" {
			writeJSON(404, map[string]string{"message": "No such image: " + cfg.Image})
			return
		}
		writeJSON(201, map[string]string{"Id": "cid-1"})
	case path == "/containers/missing/json":
		writeJSON(404, map[string]string{"message": "No such container: missing"})
	case path == "/containers/c1/json":
		writeJSON(200, map[string]any{"Id": "c1", "Name": "/c1", "State": map[string]any{"Status": "paused", "Paused": true}})
	case path == "/containers/c1/start":
		w.WriteHeader(http.StatusNotModified)
	case path == "/containers/missing" && r.Method == http.MethodDelete:
		writeJSON(404, map[string]string{"message": "No such container"})
	case path == "/containers/json":
		writeJSON(200, []map[string]any{{"Id": "c1", "Names": []string{"/ama-sess-a"}, "Labels": map[string]string{"ama.managed": "true"}}})
	case path == "/images/create":
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"status":"Pulling from library/x"}` + "\n"))
		if r.URL.Query().Get("fromImage") == "bad" {
			_, _ = w.Write([]byte(`{"errorDetail":{"message":"pull access denied"},"error":"pull access denied"}` + "\n"))
		}
	case path == "/containers/c1/exec":
		var cfg ExecConfig
		_ = json.NewDecoder(r.Body).Decode(&cfg)
		f.mu.Lock()
		f.execEnv = cfg.Env
		f.mu.Unlock()
		id := "e1"
		if cfg.AttachStdin {
			id = "e2"
		}
		writeJSON(201, map[string]string{"Id": id})
	case path == "/exec/e1/start":
		w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
		w.WriteHeader(200)
		_, _ = w.Write(frame(1, "out"))
		_, _ = w.Write(frame(2, "err"))
	case path == "/exec/e2/start":
		// The stdin call goes through hijack: after reading to EOF, echo what was received back as stdout.
		if r.Header.Get("Upgrade") != "tcp" {
			http.Error(w, "need upgrade", 400)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body) // the request body ({"Detach":false,...}) is not stdin
		hj := w.(http.Hijacker)
		conn, rw, err := hj.Hijack()
		if err != nil {
			f.t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		_ = rw.Flush()
		data, _ := io.ReadAll(rw.Reader)
		f.mu.Lock()
		f.stdin.Write(data)
		f.mu.Unlock()
		_, _ = conn.Write(frame(1, "got:"+string(data)))
	case strings.HasPrefix(path, "/exec/") && strings.HasSuffix(path, "/json"):
		writeJSON(200, map[string]any{"Running": false, "ExitCode": 3})
	default:
		writeJSON(500, map[string]string{"message": "unexpected " + r.Method + " " + path})
	}
}

func newTestClient(t *testing.T) (*Client, *fakeEngine) {
	t.Helper()
	fe := &fakeEngine{t: t}
	srv := httptest.NewServer(fe)
	t.Cleanup(srv.Close)
	c, err := New(Config{Host: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, fe
}

func TestClientBasics(t *testing.T) {
	c, _ := newTestClient(t)
	ctx := context.Background()

	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	info, err := c.Info(ctx)
	if err != nil || info.ID != "daemon-1" {
		t.Fatalf("Info = %+v, %v", info, err)
	}
	if _, err := c.ContainerCreate(ctx, "taken", ContainerConfig{Image: "img"}); !IsConflict(err) {
		t.Errorf("名字被占用要报 409，实际 %v", err)
	}
	if _, err := c.ContainerCreate(ctx, "x", ContainerConfig{Image: "nope"}); !IsNotFound(err) {
		t.Errorf("镜像不存在要报 404，实际 %v", err)
	}
	id, err := c.ContainerCreate(ctx, "x", ContainerConfig{Image: "img"})
	if err != nil || id != "cid-1" {
		t.Errorf("ContainerCreate = %q, %v", id, err)
	}
	if _, err := c.ContainerInspect(ctx, "missing"); !IsNotFound(err) {
		t.Errorf("容器不存在要报 404，实际 %v", err)
	}
	ci, err := c.ContainerInspect(ctx, "c1")
	if err != nil || ci.State.Status != "paused" {
		t.Errorf("ContainerInspect = %+v, %v", ci, err)
	}
	if err := c.ContainerStart(ctx, "c1"); err != nil {
		t.Errorf("已经在跑（304）不算错，实际 %v", err)
	}
	if err := c.ContainerRemove(ctx, "missing", true); err != nil {
		t.Errorf("删一个不存在的容器不算错，实际 %v", err)
	}
	list, err := c.ContainerList(ctx, []string{"ama.managed=true"})
	if err != nil || len(list) != 1 || list[0].Labels["ama.managed"] != "true" {
		t.Errorf("ContainerList = %+v, %v", list, err)
	}
}

// A failed image pull is also HTTP 200 with the error hidden in the progress stream; looking only
// at the status code treats failure as success.
func TestImagePullDetectsErrorInStream(t *testing.T) {
	c, _ := newTestClient(t)
	if err := c.ImagePull(context.Background(), "bad"); err == nil || !strings.Contains(err.Error(), "pull access denied") {
		t.Fatalf("要报出进度流里的错误，实际 %v", err)
	}
	if err := c.ImagePull(context.Background(), "good:1"); err != nil {
		t.Fatalf("正常拉取不该报错: %v", err)
	}
}

func TestRunCollectsOutputAndExitCode(t *testing.T) {
	c, fe := newTestClient(t)
	res, err := c.Run(context.Background(), "c1", RunOptions{Cmd: []string{"true"}, Env: []string{"A=1"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(res.Stdout) != "out" || string(res.Stderr) != "err" || res.ExitCode != 3 {
		t.Errorf("Run = stdout %q stderr %q exit %d", res.Stdout, res.Stderr, res.ExitCode)
	}
	if len(fe.execEnv) != 1 || fe.execEnv[0] != "A=1" {
		t.Errorf("env 没带上: %v", fe.execEnv)
	}
}

func TestRunWithStdinUsesHijack(t *testing.T) {
	c, fe := newTestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := c.Run(ctx, "c1", RunOptions{Cmd: []string{"cat"}, Stdin: []byte("hi there")})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(res.Stdout) != "got:hi there" {
		t.Errorf("stdout = %q", res.Stdout)
	}
	fe.mu.Lock()
	defer fe.mu.Unlock()
	if fe.stdin.String() != "hi there" {
		t.Errorf("daemon 收到的 stdin = %q", fe.stdin.String())
	}
}

func TestNewParsesHosts(t *testing.T) {
	for _, tc := range []struct {
		host, base string
		ok         bool
	}{
		{"unix:///var/run/docker.sock", "http://docker", true},
		{"tcp://10.0.0.1:2375", "http://10.0.0.1:2375", true},
		{"http://127.0.0.1:2375", "http://127.0.0.1:2375", true},
		{"https://example.com", "", false},
		{"ssh://user@host", "", false},
	} {
		c, err := New(Config{Host: tc.host})
		if (err == nil) != tc.ok {
			t.Errorf("%s: err=%v", tc.host, err)
			continue
		}
		if tc.ok && c.base != tc.base {
			t.Errorf("%s: base=%q，要 %q", tc.host, c.base, tc.base)
		}
	}
}

func TestSplitImageRef(t *testing.T) {
	for _, tc := range []struct{ in, name, tag string }{
		{"debian", "debian", "latest"},
		{"debian:bookworm", "debian", "bookworm"},
		{"localhost:5000/x/y:1", "localhost:5000/x/y", "1"},
		{"localhost:5000/x/y", "localhost:5000/x/y", "latest"},
		{"x@sha256:abc", "x@sha256:abc", ""},
	} {
		n, tg := splitImageRef(tc.in)
		if n != tc.name || tg != tc.tag {
			t.Errorf("%s → %s %s", tc.in, n, tg)
		}
	}
}

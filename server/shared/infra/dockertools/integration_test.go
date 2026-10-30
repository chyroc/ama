package dockertools

// Integration tests against a real Docker daemon. Without a daemon (cannot reach DOCKER_HOST /
// /var/run/docker.sock) they are all skipped, so on CI they are skipped by default. **Skipped means
// green, not verified.**
//
// The image defaults to debian:bookworm-slim (has bash, coreutils, GNU find, grep); override it
// with AMA_DOCKER_TEST_IMAGE. If the image is not local it gets pulled; if the pull fails the tests
// are skipped.
//
// Each run uses a random prefix and cleans up the containers and volumes it created afterwards.
// **It does not touch other containers.**

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chyroc/ama/server/shared/infra/docker"
	"github.com/chyroc/ama/server/shared/infra/shelltools"
)

func realProvider(t *testing.T, cfg Config) (*Provider, *docker.Client) {
	t.Helper()
	cli, err := docker.New(docker.Config{})
	if err != nil {
		t.Skipf("建 Docker 客户端失败，跳过: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cli.Ping(ctx); err != nil {
		t.Skipf("连不上 Docker daemon，跳过: %v", err)
	}
	image := os.Getenv("AMA_DOCKER_TEST_IMAGE")
	if image == "" {
		image = "debian:bookworm-slim"
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	cfg.Image = image
	cfg.Prefix = "amatest" + hex.EncodeToString(b[:])
	p := New(cli, cfg, quietLog())
	if err := p.Init(context.Background()); err != nil {
		t.Skipf("Init 失败，跳过: %v", err)
	}
	pctx, pcancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer pcancel()
	if err := p.PrepareImage(pctx); err != nil {
		t.Skipf("镜像 %s 不在本地也拉不到，跳过: %v", image, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		list, _ := cli.ContainerList(ctx, []string{p.labelManaged() + "=true"})
		for _, c := range list {
			_ = cli.ContainerRemove(ctx, c.ID, true)
		}
		vols, _ := cli.VolumeList(ctx, []string{p.labelManaged() + "=true"})
		for _, v := range vols {
			_ = cli.VolumeRemove(ctx, v.Name)
		}
	})
	return p, cli
}

func execTool(t *testing.T, p *Provider, sessionID, tool string, input map[string]any) string {
	t.Helper()
	set, err := p.ToolsFor(context.Background(), sessionID, t.TempDir())
	if err != nil {
		t.Fatalf("ToolsFor: %v", err)
	}
	raw, _ := json.Marshal(input)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res := set.Execute(ctx, tool, raw)
	var sb strings.Builder
	for _, c := range res.Content {
		sb.WriteString(c.Text)
	}
	if res.IsError {
		t.Fatalf("%s 失败: %s", tool, sb.String())
	}
	// Do not call set.Close(): that would trigger idle pause, and here we want to control when to
	// pause.
	return sb.String()
}

// create container → bash writes file → read sees it → pause → resume → reads it again → remove
// container → file still there after rebuild (volume) → delete.
func TestRealDockerLifecycle(t *testing.T) {
	p, cli := realProvider(t, Config{})
	ctx := context.Background()
	const sid = "sesn-it-1"
	name := p.ContainerName(sid)

	// 1. bash writes a file (first tool call, container created lazily).
	out := execTool(t, p, sid, "bash", map[string]any{"command": "echo hello-docker > note.txt && pwd"})
	if !strings.Contains(out, DefaultWorkdir) {
		t.Errorf("bash 的 cwd 应当是 %s，输出: %s", DefaultWorkdir, out)
	}
	info, err := cli.ContainerInspect(ctx, name)
	if err != nil || info.State.Status != "running" {
		t.Fatalf("容器应当在跑: %+v %v", info, err)
	}

	// 2. The read tool sees the file bash wrote: the six tools share one container file system.
	if out := execTool(t, p, sid, "read", map[string]any{"file_path": "note.txt"}); !strings.Contains(out, "hello-docker") {
		t.Fatalf("read 读不到 bash 写的文件: %s", out)
	}

	// 3. Idle pause.
	s := p.session(sid)
	if _, err := s.ensureRunning(ctx); err != nil {
		t.Fatal(err)
	}
	s.idle(ctx)
	if info, _ := cli.ContainerInspect(ctx, name); info.State.Status != "paused" {
		t.Fatalf("空闲后应当暂停，实际 %s", info.State.Status)
	}

	// 4. The next message calls a tool: resumes automatically, file still there.
	if out := execTool(t, p, sid, "read", map[string]any{"file_path": "note.txt"}); !strings.Contains(out, "hello-docker") {
		t.Fatalf("恢复后读不到文件: %s", out)
	}
	if info, _ := cli.ContainerInspect(ctx, name); info.State.Status != "running" {
		t.Fatalf("调工具后应当恢复运行，实际 %s", info.State.Status)
	}

	// 5. Container removed (reaper / manual docker rm): rebuilt, workspace volume still there.
	if err := cli.ContainerRemove(ctx, name, true); err != nil {
		t.Fatal(err)
	}
	if out := execTool(t, p, sid, "bash", map[string]any{"command": "cat note.txt"}); !strings.Contains(out, "hello-docker") {
		t.Fatalf("容器重建后工作区应当还在: %s", out)
	}

	// 6. The page side: list a directory, run a webshell command (cd must persist between two
	// commands).
	a, err := p.Attach(ctx, sid, p.DaemonID())
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	entries, err := a.List(ctx, DefaultWorkdir, 1)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Name == "note.txt" && e.Type == "file" && e.Size == int64(len("hello-docker\n")) {
			found = true
		}
	}
	if !found {
		t.Errorf("列目录里没有 note.txt: %+v", entries)
	}
	if _, err := a.Shell(ctx, "sh1", "mkdir -p sub && cd sub", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	res, err := a.Shell(ctx, "sh1", "pwd", 10*time.Second)
	if err != nil || strings.TrimSpace(res.Output) != DefaultWorkdir+"/sub" {
		t.Errorf("webshell 的 cwd 应当保留，实际 %+v %v", res, err)
	}

	// 7. Terminal: open an interactive shell, type a command, read the output.
	term, err := a.OpenTerminal(ctx)
	if err != nil {
		t.Fatalf("OpenTerminal: %v", err)
	}
	in, _ := json.Marshal(map[string]any{"type": "input", "data": "echo term-$((40+2))\r"})
	if err := term.WriteMessage(textMessage, in); err != nil {
		t.Fatal(err)
	}
	_ = term.SetReadDeadline(time.Now().Add(15 * time.Second))
	var got strings.Builder
	for !strings.Contains(got.String(), "term-42") {
		_, frame, err := term.ReadMessage()
		if err != nil {
			t.Fatalf("终端读到 %q 之后出错: %v", got.String(), err)
		}
		var f struct{ DataB64 string }
		_ = json.Unmarshal(frame, &f)
		got.WriteString(decodeB64(f.DataB64))
	}
	_ = term.Close()

	// 8. Delete.
	if err := cli.ContainerRemove(ctx, name, true); err != nil {
		t.Fatal(err)
	}
	if err := cli.VolumeRemove(ctx, p.VolumeName(sid)); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Attach(ctx, sid, ""); !IsNotFound(err) {
		t.Errorf("容器与 volume 都删了之后应当是不存在，实际 %v", err)
	}
}

// Command timeout: killed inside the container on time, not just that we stop waiting.
func TestRealDockerTimeoutKillsProcess(t *testing.T) {
	p, _ := realProvider(t, Config{})
	s := p.session("sesn-it-2")
	ctx := context.Background()
	start := time.Now()
	res, err := s.Exec(ctx, shellSpec("sleep 30", 2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 20*time.Second {
		t.Errorf("超时没生效，等了 %s", time.Since(start))
	}
	if res.Err == "" {
		t.Errorf("超时应当在 Err 里说清楚，实际 %+v", res)
	}
	// The image may not have procps, so scan /proc directly. The container's keep-alive process
	// also runs sleep 3600, so match by arguments.
	check, err := s.Exec(ctx, shellSpec(`n=0; for f in /proc/[0-9]*/cmdline; do tr '\0' ' ' < "$f" 2>/dev/null | grep -q '^sleep 30 ' && n=$((n+1)); done; echo "count=$n"`, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(check.Stdout, "count=0") {
		t.Errorf("超时的进程应当已经被杀掉，实际: %q %q", check.Stdout, check.Stderr)
	}
}

func shellSpec(cmd string, timeout time.Duration) shelltools.ExecSpec {
	return shelltools.ExecSpec{Path: "bash", Args: []string{"-lc", cmd}, Timeout: timeout}
}

func decodeB64(s string) string {
	b, _ := base64.StdEncoding.DecodeString(s)
	return string(b)
}

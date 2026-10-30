package main

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/chyroc/ama/server/shared/hostproto"
)

func TestExecLocalBasic(t *testing.T) {
	res := execLocal(context.Background(), hostproto.ExecSpec{
		Path: "/bin/sh",
		Args: []string{"-lc", "echo hello; exit 7"},
	})
	if res.ExitCode == nil || *res.ExitCode != 7 {
		t.Fatalf("退出码 = %v，期望 7", res.ExitCode)
	}
	if strings.TrimSpace(res.Stdout) != "hello" {
		t.Fatalf("stdout = %q，期望 hello", res.Stdout)
	}
}

// A timeout must return Err=timeout and kill the process group (the wait must not run far past the given timeout).
func TestExecLocalTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh 语义")
	}
	start := time.Now()
	res := execLocal(context.Background(), hostproto.ExecSpec{
		Path:      "/bin/sh",
		Args:      []string{"-lc", "sleep 30"},
		TimeoutMS: 300,
	})
	elapsed := time.Since(start)
	if res.Err != "timeout" {
		t.Fatalf("Err = %q，期望 timeout；stderr=%s", res.Err, res.Stderr)
	}
	// 300ms timeout + 3s SIGTERM grace (sleep should exit immediately on TERM and not need the full
	// grace). Loosened to 10s so CI machine jitter does not cause false failures, but it must never run
	// the full 30s.
	if elapsed > 10*time.Second {
		t.Fatalf("超时返回耗时 %v，进程组可能没被杀掉", elapsed)
	}
}

// Cancelling the parent ctx must also clean up the process, but it does not count as timeout.
func TestExecLocalCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	res := execLocal(ctx, hostproto.ExecSpec{
		Path: "/bin/sh", Args: []string{"-lc", "sleep 30"},
	})
	// Cancelling kills the process; when sh is terminated by a signal ExitCode is usually nil or non-zero; the key is it must not be timeout.
	if res.Err == "timeout" {
		t.Fatal("ctx 取消不应被标记为 timeout")
	}
}

func TestResolveCwd(t *testing.T) {
	if got := resolveCwd(""); got != "" {
		t.Fatalf("空 cwd 应原样返回，得到 %q", got)
	}
	if got := resolveCwd("/abs/path"); got != "/abs/path" {
		t.Fatalf("绝对路径应原样返回，得到 %q", got)
	}
	got := resolveCwd(filepath.Join(".ama", "runs", "x"))
	if !filepath.IsAbs(got) || !strings.HasSuffix(got, filepath.Join(".ama", "runs", "x")) {
		t.Fatalf("相对路径应锚到 $HOME 绝对路径，得到 %q", got)
	}
}

// Run many short commands repeatedly, together with -race: verifies the timeout goroutine has no
// data race with the main goroutine and does not leak over time (every command returns normally).
func TestExecLocalNoRaceOverManyCalls(t *testing.T) {
	for i := 0; i < 30; i++ {
		res := execLocal(context.Background(), hostproto.ExecSpec{
			Path: "/bin/sh", Args: []string{"-lc", "true"}, TimeoutMS: 5000,
		})
		if res.ExitCode == nil || *res.ExitCode != 0 {
			t.Fatalf("第 %d 次执行失败: %+v", i, res)
		}
	}
}

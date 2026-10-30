//go:build linux

// Hosting the daemon as a service on Linux: systemd --user.
//
// --user rather than system-level:
//   - no sudo (works right after a global npm install);
//   - runs in the user's own session, so $HOME, credentials and terminal identity are all that
//     person's;
//   - needs lingering (keeps running after a reboot without logging in); install automatically
//     tries `loginctl enable-linger`, and without permission (containers/minimal machines without
//     systemd/polkit) it tells the user to handle it by hand or use nohup.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const systemdUnit = "ama-daemon.service"

func serviceSupported() bool { return true }

func systemdUnitPath() (string, error) {
	// Ignore XDG_RUNTIME_DIR and the like; the unit goes in the static user config directory.
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user", systemdUnit), nil
}

func systemdUnitContent(exePath string) string {
	return strings.Join([]string{
		"[Unit]",
		"Description=AMA host daemon (runs agent tools on this machine)",
		"",
		"[Service]",
		"ExecStart=" + exePath + " daemon run",
		"Restart=always",
		"RestartSec=3",
		"# host_token 在 ~/.ama/host.json，不需要 AMA_TOKEN；代理等变量随用户环境。",
		"Environment=AMA_BASE_URL=" + os.Getenv("AMA_BASE_URL"),
		// Transport preference and WS entry (an environment snapshot at install time; empty = derive automatically + prefer WS, fall back to HTTP).
		"Environment=AMA_DAEMON_TRANSPORT=" + os.Getenv("AMA_DAEMON_TRANSPORT"),
		"Environment=AMA_WS_BASE_URL=" + os.Getenv("AMA_WS_BASE_URL"),
		// Logs go through the daemon's own size-based rotating writer (10MiB×2; see
		// daemon_log.go), no longer systemd's append (which never rotates). stdout/stderr still go
		// to the journal, so output that bypasses the logger, like a panic, is still visible in
		// journalctl.
		"Environment=" + envDaemonLogFile + "=" + daemonLogPath,
		"",
		"[Install]",
		"WantedBy=default.target",
		""}, "\n")
}

func installService() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("找不到当前二进制路径: %w", err)
	}
	p, err := systemdUnitPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p, []byte(systemdUnitContent(exe)), 0o644); err != nil {
		return fmt.Errorf("写 systemd unit 失败: %w", err)
	}
	for _, args := range [][]string{
		{"--user", "daemon-reload"},
		{"--user", "enable", "--now", systemdUnit},
	} {
		if err := runCmd("systemctl", args...); err != nil {
			// Distros differ in where systemctl accepts the --user prefix; try once more with it at the end.
			return fmt.Errorf("执行 systemctl %s 失败: %w", strings.Join(args, " "), err)
		}
	}
	fmt.Println("已安装并启动 systemd --user 服务（登录后自动运行，崩溃自动重启）")
	// linger: lacking permission is not fatal (it only affects "keep running after a reboot without logging in").
	if err := runCmd("loginctl", "enable-linger", os.Getenv("USER")); err != nil {
		fmt.Fprintln(os.Stderr, "提示：未能开启 linger（机器重启后需登录一次才会启动 daemon）；",
			"需要的话请管理员执行 `loginctl enable-linger "+os.Getenv("USER")+"`")
	}
	return nil
}

func uninstallService() error {
	p, err := systemdUnitPath()
	if err != nil {
		return err
	}
	_ = runCmd("systemctl", "--user", "disable", "--now", systemdUnit)
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = runCmd("systemctl", "--user", "daemon-reload")
	fmt.Println("已停止并移除 systemd --user 服务")
	return nil
}

func startService() error   { return runCmd("systemctl", "--user", "start", systemdUnit) }
func stopService() error    { return runCmd("systemctl", "--user", "stop", systemdUnit) }
func restartService() error { return runCmd("systemctl", "--user", "restart", systemdUnit) }

func serviceStatus() error {
	p, _ := systemdUnitPath()
	if _, err := os.Stat(p); err != nil {
		fmt.Println("未安装（跑 ama daemon install）")
		return nil
	}
	return runCmd("systemctl", "--user", "status", "--no-pager", systemdUnit)
}

func serviceLogs() error { return tailFile("/tmp/ama-daemon.log") }

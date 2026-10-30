//go:build darwin

// Hosting the daemon as a service on macOS: a launchd user-level Agent (~/Library/LaunchAgents).
//
// User-level rather than /Library/LaunchDaemon:
//   - no sudo; after a global npm install, one `ama daemon install` command does it;
//   - the daemon runs in the logged-in user's session, so the PTY terminal, $HOME and keychain are
//     all that person's, which is exactly the identity "the agent works on my Mac" needs;
//   - the limit is it does not run when the user has no GUI session; for "the user's own Mac" that
//     is not a problem.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const launchdLabel = "io.github.chyroc.ama.daemon"

func serviceSupported() bool { return true }

func launchdPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"), nil
}

// launchdPlist generates the plist. Key entries:
//   - ProgramArguments uses the **current real binary path** (os.Executable): npm's node shell
//     spawns the cross-compiled ama-<os>-<arch>, and os.Executable resolves to it;
//   - RunAtLoad + KeepAlive: start on login, restart automatically after a crash/exit;
//   - AMA_* environment variables are inherited naturally in the user's login session (launchd
//     carries the GUI environment, beyond ~/.zprofile); host_token is stored in ~/.ama/host.json, so
//     the daemon does not need AMA_TOKEN.
func launchdPlist(exePath string) string {
	return strings.Join([]string{
		`<?xml version="1.0" encoding="UTF-8"?>`,
		`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">`,
		`<plist version="1.0"><dict>`,
		`  <key>Label</key><string>` + launchdLabel + `</string>`,
		`  <key>ProgramArguments</key>`,
		`  <array>`,
		`    <string>` + xmlEscape(exePath) + `</string>`,
		`    <string>daemon</string><string>run</string>`,
		`  </array>`,
		`  <key>RunAtLoad</key><true/>`,
		`  <key>KeepAlive</key><true/>`,
		// Business logs go through the daemon's own size-based rotating writer (10MiB×2; see
		// daemon_log.go). launchd's StandardOutPath does not rotate, so the business logger no longer
		// writes to it; but it is kept as a fallback for panics/non-logger output (tiny volume).
		`  <key>EnvironmentVariables</key><dict>`,
		`    <key>` + envDaemonLogFile + `</key><string>` + daemonLogPath + `</string>`,
		`  </dict>`,
		`  <key>StandardOutPath</key><string>/tmp/ama-daemon.out.log</string>`,
		`  <key>StandardErrorPath</key><string>/tmp/ama-daemon.out.log</string>`,
		`</dict></plist>`,
		""}, "\n")
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;")
	return r.Replace(s)
}

func installService() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("找不到当前二进制路径: %w", err)
	}
	p, err := launchdPlistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p, []byte(launchdPlist(exe)), 0o644); err != nil {
		return fmt.Errorf("写 launchd 配置失败: %w", err)
	}
	// bootstrap is the modern launchctl subcommand; on failure, give a hint with the old command.
	if err := runCmd("launchctl", "bootstrap", "gui/"+strconv.Itoa(os.Getuid()), p); err != nil {
		// When already loaded, bootstrap exits with code 5; use kickstart as a fallback to start/restart it.
		_ = runCmd("launchctl", "kickstart", "-k", "gui/"+strconv.Itoa(os.Getuid())+"/"+launchdLabel)
	}
	fmt.Println("已安装并启动 launchd 服务（开机登录后自动运行，崩溃自动重启）")
	return nil
}

func uninstallService() error {
	p, err := launchdPlistPath()
	if err != nil {
		return err
	}
	_ = runCmd("launchctl", "bootout", "gui/"+strconv.Itoa(os.Getuid())+"/"+launchdLabel)
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Println("已停止并移除 launchd 服务")
	return nil
}

func startService() error {
	return runCmd("launchctl", "kickstart", "gui/"+strconv.Itoa(os.Getuid())+"/"+launchdLabel)
}
func stopService() error {
	return runCmd("launchctl", "kill", "SIGTERM", "gui/"+strconv.Itoa(os.Getuid())+"/"+launchdLabel)
}
func restartService() error {
	return runCmd("launchctl", "kickstart", "-k", "gui/"+strconv.Itoa(os.Getuid())+"/"+launchdLabel)
}

func serviceStatus() error {
	p, _ := launchdPlistPath()
	if _, err := os.Stat(p); err != nil {
		fmt.Println("未安装（跑 ama daemon install）")
		return nil
	}
	out, err := cmdOutput("launchctl", "print", "gui/"+strconv.Itoa(os.Getuid())+"/"+launchdLabel)
	if err != nil {
		fmt.Println("已安装但当前未运行（跑 ama daemon start）")
		return nil
	}
	running := strings.Contains(out, "state = running") || strings.Contains(out, "pid =")
	if running {
		fmt.Println("运行中；日志：/tmp/ama-daemon.log")
	} else {
		fmt.Println("已安装但当前未运行（跑 ama daemon start）")
	}
	return nil
}

func serviceLogs() error { return tailFile("/tmp/ama-daemon.log") }

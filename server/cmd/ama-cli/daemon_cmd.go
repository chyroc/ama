// Dispatch of daemon subcommands and small shared helpers for service management (run/register/service install live in the per-platform files).
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// daemonCmd handles `ama daemon <sub>`.
//
// Note: in main.run it is dispatched **before newClient**: register reads the user token from env
// itself, and run only needs host_token (in ~/.ama/host.json); neither should be stopped by the
// ordinary client's "AMA_TOKEN is required" check.
func daemonCmd(args []string) error {
	sub := "run"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "register":
		return daemonRegister()
	case "run", "start-foreground":
		return daemonRun(false)
	case "install":
		// One command takes over setup: if the machine is not registered yet, register with AMA_TOKEN
		// first, then install the service. If already registered (reinstalling the service after a CLI
		// upgrade, etc.), install directly; no AMA_TOKEN needed.
		return daemonInstall()
	case "uninstall":
		return uninstallService()
	case "start":
		return startService()
	case "stop":
		return stopService()
	case "restart":
		return restartService()
	case "status":
		return serviceStatus()
	case "logs":
		return serviceLogs()
	case "reset", "unregister":
		// Only wipe the local credential (the machine on the server is still in global settings, and the owner can revoke it on the page).
		cfg, err := loadDaemonConfig()
		if err != nil {
			if isNotExist(err) {
				fmt.Println("本机没有 daemon 配置")
				return nil
			}
			return err
		}
		_ = stopService()
		cfg.Token = ""
		cfg.HostID = ""
		cfg.HostUUID = ""
		if err := cfg.save(); err != nil {
			return err
		}
		fmt.Println("已清除本机 daemon 凭据；重新启用请 AMA_TOKEN=amak_xxx ama daemon install")
		return nil
	case "-h", "--help", "help":
		fmt.Print(daemonUsage)
		return nil
	default:
		return fmt.Errorf("未知 daemon 子命令 %q。跑 `ama daemon help` 看用法", sub)
	}
}

// daemonInstall is the entry of `ama daemon install`: when the machine is not registered yet,
// register first (AMA_TOKEN is used only once, in this step), then install as a resident system
// service. If already registered (reinstalling the service after a CLI upgrade), install directly
// without needing AMA_TOKEN.
func daemonInstall() error {
	if !serviceSupported() {
		return installService()
	}
	if !hostConfigReady() {
		fmt.Println("本机还没注册，先完成注册……")
		if err := registerHost(); err != nil {
			return err
		}
	}
	return installService()
}

func hostConfigReady() bool {
	cfg, err := loadDaemonConfig()
	return err == nil && cfg.Token != "" && cfg.HostID != ""
}

// runCmd / cmdOutput are small wrappers for service management commands (inheriting stdio, so
// systemctl/launchctl errors show up directly in the user's terminal).
func runCmd(name string, args ...string) error {
	c := exec.Command(name, args...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

func cmdOutput(name string, args ...string) (string, error) {
	c := exec.Command(name, args...)
	b, err := c.Output()
	return string(b), err
}

// tailFile prints logs with the system tail (-n 200 -f). When there is no log file, it gives a hint with the path.
func tailFile(path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("日志文件还不存在：%s（先 ama daemon run 前台跑一次看输出）", path)
	}
	return runCmd("tail", "-n", "200", "-f", path)
}

const daemonUsage = `ama daemon —— 在这台机器上常驻，让 AMA agent 的工具在这里执行

用法：
  ama daemon install     一键接入：首次运行先自动注册（需要 AMA_TOKEN），再装成
                         系统服务并启动（macOS: launchd，Linux: systemd --user）；
                         已注册过时直接装服务，不需要 AMA_TOKEN
  ama daemon run         前台运行（调试用；日常由系统服务托管；需先注册）
  ama daemon register    只注册不装服务（想只用 run 前台跑时才需要它）
  ama daemon status      查看服务状态
  ama daemon restart     重启服务（升级 CLI 后执行）
  ama daemon logs        跟踪日志（/tmp/ama-daemon.log）
  ama daemon stop        停止服务
  ama daemon uninstall   停止并移除系统服务（不删注册凭据）
  ama daemon reset       清除本机注册凭据

环境：
  AMA_TOKEN            install 首次注册 / register 时需要（全局设置创建 API Key）；
                       run/服务运行时不需要
  AMA_BASE_URL         服务端地址，默认 http://localhost:9999
  AMA_DAEMON_TRANSPORT 传输偏好：auto（默认，WebSocket 优先失败降级 HTTP）或 http（只走长轮询）
  AMA_WS_BASE_URL      WebSocket 入口（默认按 AMA_BASE_URL 推导：同 host，http→ws，一般不用填）

工具由 daemon 用 Go 原生执行（bash 用系统自带 /bin/bash），Mac 无需安装 brew 依赖。
`

var _ = strings.TrimSpace

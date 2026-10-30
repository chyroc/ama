//go:build !darwin && !linux

// Platforms other than macOS/Linux (Windows and so on): hosting the daemon as a service is not
// supported yet. run/register can still be used in the foreground (exec/pty have other limits), and
// the install family gives clear guidance.
package main

import "errors"

func serviceSupported() bool { return false }

func installService() error {
	return errors.New("当前平台暂不支持自动安装服务；请在 macOS/Linux 上使用，或用 `ama daemon run` 前台运行")
}
func uninstallService() error { return installService() }
func startService() error     { return installService() }
func stopService() error      { return installService() }
func restartService() error   { return installService() }

func serviceStatus() error {
	println("当前平台不支持服务托管；ama daemon run 可前台运行")
	return nil
}
func serviceLogs() error { return installService() }

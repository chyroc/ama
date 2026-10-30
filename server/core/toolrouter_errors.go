package core

import "errors"

// Explicit errors for when an executor Provider is not wired up (the WorkerPool has Docker
// disabled but a container work arrives, or the host service is not wired up but a host work arrives).
var (
	errContainerProviderNotConfigured = errors.New("Docker 执行端未启用或连不上 daemon（见 amad 启动日志里「Docker 执行端」那几行），这条会话的工具无法执行")
	errHostProviderNotConfigured      = errors.New("宿主机执行端未配置（host 服务未装配），这条会话的工具无法执行")
)

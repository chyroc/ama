#!/usr/bin/env node
// Entry shell for `ama`. The real work is done by the Go binary in the same directory.
//
// # Why package.json's bin points to this JS and not directly to the binary
//
// npm's `bin` generates a .cmd wrapper on Windows and a symlink on Unix-like systems; it expects a
// script that node can run. Pointing directly to a platform-specific binary breaks on another platform.
// `@larksuite/cli` does the same (scripts/run.js); this copies its shape.
//
// # This layer does only three things, not one more
//
//  1. Pick which binary to use by platform/architecture;
//  2. Forward argv and stdio as is;
//  3. **Forward the exit code as is**.
//
// The third is the easiest to forget, and forgetting it has the most hidden cost: the agent in the container judges success by the exit code;
// if a failed command is reported as exit 0, it treats the error message as a result and keeps going.
// That kind of failure reports no error; only the conclusion is wrong.
"use strict";

const { spawnSync } = require("child_process");
const fs = require("fs");
const path = require("path");

// Go's GOOS/GOARCH and node's process.platform/arch use different names, so they need mapping.
const PLATFORM = { darwin: "darwin", linux: "linux", win32: "windows" };
const ARCH = { x64: "amd64", arm64: "arm64" };

function main() {
  const goos = PLATFORM[process.platform];
  const goarch = ARCH[process.arch];

  if (!goos || !goarch) {
    fail(
      `ama 不支持当前平台：${process.platform}-${process.arch}\n` +
        `支持的组合：linux-x64 / linux-arm64 / darwin-x64 / darwin-arm64 / win32-x64`
    );
  }

  const ext = goos === "windows" ? ".exe" : "";
  const bin = path.join(__dirname, `ama-${goos}-${goarch}${ext}`);

  if (!fs.existsSync(bin)) {
    // Reaching here means the package is incomplete (this platform's binary was left out when publishing, or the install was cut off).
    //
    // ⚠️ **It must really fail**, not quietly exit 0. A silent exit 0 makes `npm ci` report success
    // while the package is empty, and the error only shows up when actually run, and that error does not point to "the dependency was not installed" at all.
    fail(
      `找不到 ama 的二进制：${bin}\n` +
        `这个包可能没有包含当前平台（${goos}-${goarch}）的二进制。\n` +
        `修复：重新安装这个包（或从源码 go build ./cmd/ama-cli）；仍然失败请反馈这条完整信息。`
    );
  }

  // spawnSync + stdio:inherit: stdout/stderr connect straight to the parent process,
  // so output streams (long responses show before the command ends) and uses no extra memory.
  const r = spawnSync(bin, process.argv.slice(2), { stdio: "inherit" });

  if (r.error) {
    // The binary exists but cannot start (permission bit lost, architecture mismatch, file corrupted).
    // ENOEXEC gets its own message, because it most often means "installed the package for the wrong architecture".
    const extra =
      r.error.code === "ENOEXEC"
        ? "\n（Exec format error：这个二进制不是当前架构的，包装可能有问题）"
        : "";
    fail(`启动 ama 失败：${r.error.message}${extra}`);
  }

  // When killed by a signal, status is null. We map it to 128+signo, same as the shell convention;
  // a plain exit(null) is treated as 0, which would report "killed" as success.
  if (r.status === null) {
    process.exit(r.signal ? 128 : 1);
  }
  process.exit(r.status);
}

function fail(msg) {
  process.stderr.write(msg + "\n");
  process.exit(1);
}

main();

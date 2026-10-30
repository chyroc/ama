import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { fileURLToPath } from "node:url";
import { execSync } from "node:child_process";

/**
 * Frontend version: the short sha of the current commit, injected at build time.
 *
 * It has only one use: issue feedback must be able to say "which frontend version this error happened on" (see src/lib/feedback.ts).
 * Without it, a feedback can only tell you "someone hit an error", not whether that person was running a version that is already fixed.
 *
 * ⚠️ **When it cannot be obtained it must fall back to an empty string, and must never fail the build.** It cannot be obtained in three cases:
 * the SCM build container may have no .git, no git installed, or the source was unpacked from a tarball.
 * Then the build goes on as usual with this field empty: feedback missing one field is far better than the whole release flow breaking.
 *
 * Do not fall back to "unknown" either: that would make "not injected" and "injected a version called unknown"
 * look exactly the same in the database. The backend drops an empty string directly via putIfNotEmpty, which is a cleaner shape.
 */
function gitSha(): string {
  try {
    return execSync("git rev-parse --short HEAD", {
      stdio: ["ignore", "pipe", "ignore"],
    })
      .toString()
      .trim();
  } catch {
    return "";
  }
}

export default defineConfig({
  plugins: [react(), tailwindcss()],
  // JSON.stringify cannot be dropped: define does **literal replacement**, and a bare string would be taken as an identifier,
  // so the output would contain an undefined variable name instead of that string value.
  define: {
    __APP_VERSION__: JSON.stringify(gitSha()),
  },
  resolve: {
    alias: {
      "@": fileURLToPath(new URL("./src", import.meta.url)),
    },
  },
  server: {
    port: 5273,
    strictPort: true,
    // Listen on all interfaces so other machines on the same internal network can access it; the actual binding is decided by --host in scripts/dev.sh
    host: "0.0.0.0",
    // Vite blocks domains not in the allowlist by the Host header (DNS rebinding protection). It is opened here,
    // otherwise access by hostname or internal IP would be refused. Only for internal development; do not copy this to environments exposed to the public internet.
    allowedHosts: true,
    // Forward /api to the local backend (cmd/amad defaults to 127.0.0.1:8080).
    //
    // This way the frontend requests same-origin /api/**, and **cross-origin problems do not exist at all**: no need for the backend to configure
    // AMA_ALLOWED_ORIGIN, and the login cookie does not need to deal with all the cross-site restrictions.
    //
    // When the backend changes port, override with AMA_PROXY_TARGET (8080 on this machine is often taken by other processes).
    // To connect to a completely different address without the proxy, set VITE_API_BASE=http://host:port/api for the frontend,
    // but then the backend has to allow cross-origin itself.
    proxy: {
      "/api": {
        target: process.env.AMA_PROXY_TARGET || "http://127.0.0.1:8080",
        changeOrigin: true,
      },
    },
    // On this machine fs.inotify.max_user_instances is only 128 and already used up by other processes, so vite's default
    // file watching throws EMFILE right away. Use polling to bypass inotify, and ignore node_modules to keep the cost down.
    watch: {
      usePolling: true,
      interval: 400,
      ignored: ["**/node_modules/**", "**/dist/**", "**/.git/**", "**/test-results/**"],
    },
  },
  preview: {
    port: 5273,
    strictPort: true,
    host: "0.0.0.0",
  },
});

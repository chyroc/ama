/**
 * A temporary config for running the whole e2e suite: baseURL points to an already running `vite preview`, without webServer.
 *
 * Why it is needed (see "e2e runs in local mode" in docs/guides/frontend.md):
 * running the 67 tests with the dev server makes it die midway: on this machine fs.inotify.max_user_instances
 * is only 128 and often used up to 124, and even with usePolling configured in vite.config.ts it is still not enough.
 * After that all tests fail in a row with ERR_CONNECTION_REFUSED, **which looks like a widespread regression but is really the environment**.
 *
 * preview serves static output with zero file watching, so it can run through in one go. Usage:
 *
 *   VITE_API_MODE=local npx vite build --mode development
 *   npx vite preview --port 5274 --host 0.0.0.0 --strictPort &
 *   npx playwright test -c playwright.preview.config.ts
 *
 * ⚠️ **A taken port means someone else is running; use `E2E_PORT` to switch, do not kill that process**
 * (this workspace has concurrent sessions). Failures from fighting over the port do not point to the port: they show up as
 * the whole suite failing in a row with ERR_CONNECTION_REFUSED, or worse: connecting to **someone else's build output**,
 * so what you verify is not your own code at all. Likewise, `--outDir` of the build must be a private directory.
 *
 * ⚠️ This file **must be in web/**; in /tmp it gives ERR_MODULE_NOT_FOUND (cannot resolve
 * @playwright/test).
 */
import { defineConfig, devices } from "@playwright/test";

const E2E_PORT = Number(process.env.E2E_PORT ?? 5274);

export default defineConfig({
  testDir: "./e2e",
  fullyParallel: false,
  workers: 1,
  reporter: [["list"]],
  timeout: 60_000,
  expect: { timeout: 10_000 },
  use: {
    baseURL: `http://127.0.0.1:${E2E_PORT}`,
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
    launchOptions: { args: ["--no-sandbox", "--disable-dev-shm-usage"] },
  },
  projects: [
    {
      name: "chrome",
      use: { ...devices["Desktop Chrome"], channel: "chrome" },
      // ⚠️ **The mobile group must be excluded**, exactly as in playwright.config.ts.
      // Without it, the 9 tests in `e2e/mobile/` would run at 1280×720, while what they assert is exactly
      // the narrow-screen layout (top bar visible below the md breakpoint, drawer, vertical stacking): **they turn red in a row, with errors that
      // do not point to the viewport at all**, looking like the mobile implementation is broken.
      testIgnore: /mobile\/.*\.spec\.ts/,
    },
    {
      // The mobile group needs its own project, otherwise the testIgnore above would make them not run at all,
      // which is worse than running wrong (9 fewer run while all green, and nobody can tell).
      //
      // ⚠️ Both `browserName` and `channel` must be written: the `iPhone 16 Pro` preset comes with
      // `defaultBrowserType: "webkit"`, and writing only channel reports
      // `Unsupported webkit channel "chrome"`. See playwright.config.ts for details.
      name: "iphone",
      use: { ...devices["iPhone 16 Pro"], browserName: "chromium", channel: "chrome" },
      testMatch: /mobile\/.*\.spec\.ts/,
    },
  ],
});

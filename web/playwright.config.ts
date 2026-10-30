import { defineConfig, devices } from "@playwright/test";

/**
 * e2e config.
 *
 * Uses the system Chrome (channel: "chrome") instead of downloading Playwright's own browser: this machine has no
 * browser cache, downloading may not get through, and /usr/bin/google-chrome is already there.
 *
 * webServer goes through scripts/dev.sh instead of `vite` directly: inotify instances on this machine are used up, and the
 * ulimit and polling config in the script are needed; see the comments in scripts/dev.sh and vite.config.ts.
 *
 * # Tests run in local mode
 *
 * `VITE_API_MODE=local` makes `data/api.ts` choose LocalApiClient instead of the default HttpApiClient.
 * This is not for convenience: 59 tests depend on the determinism of the mock: before each test `window.__amaApi.reset()` goes back to
 * seed data, and the Feishu ones also need `completeFeishuAuth()` to get past device code authorization, and **a real backend cannot
 * have this hook** (authorization can only be done by the user clicking agree in Feishu). Running over HTTP would mean starting the backend and the database, and accepting
 * state shared between tests. What is tested is frontend interaction, so let it depend only on the frontend.
 *
 * # Port separate from the dev server
 *
 * e2e uses 5274 with `reuseExistingServer: false`, not touching 5273 used for daily development.
 * It used to reuse the existing service on 5273; after switching the default to HTTP this would **silently** run tests on a page in HTTP mode
 * (reset() hits a backend that is not running, everything times out), and the error does not point to "reused the wrong server" at all.
 * Starting its own one with the right environment variables is cheaper than guessing afterwards why it failed.
 *
 * The port can be overridden with `E2E_PORT`: **this workspace has several sessions working at the same time**, and when another session is running
 * e2e, 5274 is taken and reports `is already used`. That process is not yours; do not kill it;
 * switch to another port to run your own: `E2E_PORT=5284 npx playwright test`.
 */
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
      // `testDir` is the whole e2e/, so the mobile group must be excluded explicitly, otherwise they would **also** run at
      // 1280×720, while what they assert is exactly the narrow-screen layout (e.g. "the top bar is visible below the md breakpoint"),
      // which is bound to be red in a desktop viewport, with errors that look like the mobile implementation has problems.
      testIgnore: /mobile\/.*\.spec\.ts/,
    },
    /*
     * Mobile. The device preset is Playwright's built-in `iPhone 16 Pro` (viewport 402×681,
     * isMobile, hasTouch, deviceScaleFactor 3).
     *
     * ⚠️ **Both `browserName` + `channel` must be written; neither can be left out.** That device preset carries
     * `defaultBrowserType: "webkit"`, while this machine's `~/.cache/ms-playwright/` only has chromium
     * (and downloading browsers may not get through; see the file header). Writing only `channel: "chrome"` is not enough: in practice it reports
     * **`browserType.launch: Unsupported webkit channel "chrome"`**, because channel is interpreted on
     * webkit. `browserName: "chromium"` must be set explicitly to switch the browser before channel makes sense.

     *
     * ⚠️ **So this project cannot test the real behavior of iOS Safari.** Chromium simulates viewport / UA /
     * touch, but `env(safe-area-inset-*)` is **always 0** in it, the soft keyboard never really pops up,
     * `visualViewport` behaves differently from iOS, and standalone mode cannot be simulated. What it can guard is
     * **the layout contract** (fixed top bar, no horizontal overflow, the content area scrolls on its own); safe areas and the keyboard **must be checked on a real device**.
     * Do not take this project being all green as "mobile is fine".
     *
     * `testMatch` limits it to `e2e/mobile/`: the 273 desktop tests are written for the desktop layout
     * (the sidebar always present, `nav()` finding navigation links directly), and on narrow screens the sidebar becomes a drawer, so large numbers would fail.
     * Rather than change mobile back to the desktop layout to make them green (that would put the cart before the horse), let mobile
     * have its own group of tests, and leave the 273 desktop ones untouched.
     */
    {
      name: "iphone",
      use: { ...devices["iPhone 16 Pro"], browserName: "chromium", channel: "chrome" },
      testMatch: /mobile\/.*\.spec\.ts/,
    },
  ],

  webServer: {
    command: "./scripts/dev.sh",
    url: `http://127.0.0.1:${E2E_PORT}`,
    reuseExistingServer: false,
    timeout: 60_000,
    env: {
      PORT: String(E2E_PORT),
      VITE_API_MODE: "local",
    },
  },
});

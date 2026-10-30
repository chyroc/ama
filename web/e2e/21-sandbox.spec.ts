import type { Page } from "@playwright/test";

import {
  clearFeedbackMock,
  clearSandboxStatusMock,
  clearTerminalInputMock,
  expect,
  failTerminalInputMock,
  feedbackReports,
  setSandboxStatusMock,
  test,
} from "./fixtures";

/**
 * The container panel (file browser + terminal).
 *
 * The panel renders under **its own view** (`?view=files`, the fourth tab next to chat / events /
 * call tree), so always `goto` the address with the parameter directly; do not click the view switch,
 * which waits for one more route tick.
 *
 * # What this set watches is "each of the four states says its own thing"
 *
 * The container has four states, and **two pairs are the most likely to get merged**; once merged
 * there is no error at all, only users taking the wrong action:
 *
 * | Merge | Consequence |
 * | --- | --- |
 * | `none` said as `gone` | users think the files were lost and ask "where is my workspace", while there never was one |
 * | `paused` said as `gone` | users start over from scratch for nothing, while the workspace is actually still there |
 *
 * So each state gets its own case, and **explicitly asserts the sentence that should not appear**
 * (asserting only "A appeared" cannot stop "A and B both appeared").
 *
 * # It also watches one security thing
 *
 * **The file list is read-only**: no delete / upload entry should appear.
 *
 * The terminal side's line of defense is the **permission threshold** (`ActionManage`; member and
 * viewer cannot get in), locked in by the table-driven cases in authz, not here.
 *
 * The seed's run_1 is the run shared by the chat page and the run detail page.
 */
test.describe("容器面板", () => {
  // ⚠️ **It must be restored**: workers=1, localStorage shared serially, and a leftover state marker would
  // make all later specs run on the wrong branch, with failure messages not pointing to this at all.
  test.afterEach(async ({ page }) => {
    await clearSandboxStatusMock(page);
  });

  test("默认是「还没用到容器」，且不能说成「已回收」", async ({ app }) => {
    await app.goto("/w/growth/runs/run_1?view=files");

    // First wait for a positive anchor that certainly exists, then assert the text: asserting directly
    // would run during the skeleton stage.
    const panel = app.getByTestId("sandbox-panel");
    await expect(panel).toBeVisible();
    await expect(panel).toHaveAttribute("data-status", "none");

    await expect(app.getByText("这次对话还没用到容器")).toBeVisible();
    // ⚠️ This one is the focus: **"never created" is not "recycled"**.
    await expect(app.getByText("容器已被回收")).toHaveCount(0);
    // Without a container there is nothing to resume, so no button should be given.
    await expect(app.getByTestId("sandbox-resume")).toHaveCount(0);
  });

  test("运行中：列出文件、可以点进目录", async ({ app }) => {
    await app.goto("/w/growth/runs/run_1");
    await setSandboxStatusMock(app, "running");
    await app.goto("/w/growth/runs/run_1?view=files");

    await expect(app.getByTestId("sandbox-panel")).toHaveAttribute("data-status", "running");
    await expect(app.getByTestId("sandbox-status")).toHaveText("运行中");

    // Root directory: one directory + one file + one symlink.
    const files = app.getByTestId("sandbox-file");
    await expect(files).toHaveCount(3);
    await expect(app.getByText("output.log")).toBeVisible();

    // A symlink must show where it points: **the type comes from the platform's type field**; computing it
    // from the mode bits would treat every regular file as a symlink.
    await expect(app.getByText("→ /workspace/workspace")).toBeVisible();

    // Click into the directory; the list switches along.
    await app.getByText("workspace", { exact: true }).click();
    await expect(app.getByText("main.go")).toBeVisible();
    // "Up one level" only exists inside a subdirectory.
    await expect(app.getByTestId("sandbox-file-parent")).toBeVisible();
  });

  test("文件列表是只读的：没有删除或上传入口", async ({ app }) => {
    await app.goto("/w/growth/runs/run_1");
    await setSandboxStatusMock(app, "running");
    await app.goto("/w/growth/runs/run_1?view=files");

    await expect(app.getByTestId("sandbox-files")).toBeVisible();
    await expect(app.getByText("只读浏览")).toBeVisible();
    // Read-only is the boundary of this feature: writing to the container needs its own design and its own permissions.
    await expect(app.getByRole("button", { name: /删除|上传/ })).toHaveCount(0);
  });

  test("已暂停：自动恢复，不需要用户点按钮", async ({ app }) => {
    await app.goto("/w/growth/runs/run_1");
    await setSandboxStatusMock(app, "paused");
    await app.goto("/w/growth/runs/run_1?view=files");

    // ⚠️ **There should be no "resume container" button**: the user deliberately clicked into the "Files"
    // tab, so the intent to use this container is already clear; making them click again asks once more
    // what they already said.
    await expect(app.getByTestId("sandbox-resume")).toHaveCount(0);

    // Instead it starts by itself: the mock's resume pushes the state to running.
    await expect(app.getByTestId("sandbox-panel")).toHaveAttribute("data-status", "running");
    await expect(app.getByTestId("sandbox-status")).toHaveText("运行中");
  });


  test("已回收：说清文件没了，且不去自动恢复", async ({ app }) => {
    await app.goto("/w/growth/runs/run_1");
    await setSandboxStatusMock(app, "gone");
    await app.goto("/w/growth/runs/run_1?view=files");

    await expect(app.getByTestId("sandbox-panel")).toHaveAttribute("data-status", "gone");
    await expect(app.getByTestId("sandbox-status")).toHaveText("已回收");
    await expect(app.getByText("容器已被回收")).toBeVisible();

    // A recycled container cannot be resumed (canResume=false), so there is no button, and no automatic
    // resume either: otherwise it would keep sending requests at a container that does not exist.
    await expect(app.getByTestId("sandbox-resume")).toHaveCount(0);
    // Conversely it must not be described as "paused" either: that would make users think the files are still there.
    await expect(app.getByText("容器已暂停")).toHaveCount(0);
  });

  test("终端：打开后挂上 xterm 画布", async ({ app }) => {
    await app.goto("/w/growth/runs/run_1");
    await setSandboxStatusMock(app, "running");
    await app.goto("/w/growth/runs/run_1?view=terminal");

    // **No need to click "open terminal" again**: entering this tab connects directly.
    await expect(app.getByTestId("sandbox-terminal")).toBeVisible();
    // xterm is lazy-loaded; wait for it to mount.
    // **Only assert the canvas mounted, not what is inside**: in mock mode there is no real backend, and
    // the WebSocket failing to connect is expected; that path is verified end to end on a real machine,
    // not here.
    const host = app.getByTestId("sandbox-terminal-host");
    await expect(host).toBeVisible();
    await expect(host.locator(".xterm")).toBeVisible();
  });

  test("终端：容器已回收时显示说明而不是消失", async ({ app }) => {
    await app.goto("/w/growth/runs/run_1");
    // Use gone rather than paused: paused now **resumes automatically** and cannot be held.
    await setSandboxStatusMock(app, "gone");
    await app.goto("/w/growth/runs/run_1?view=terminal");

    // ⚠️ The terminal **always renders** (it does not unmount with the state; that was a fixed bug),
    // and when the container is unavailable it shows an explanation instead of disappearing.
    await expect(app.getByTestId("sandbox-terminal-closed")).toBeVisible();
    await expect(app.getByText("容器运行中才能打开终端。")).toBeVisible();
  });

  test("终端：只连一次，且状态只显示一句", async ({ app }) => {
    await app.goto("/w/growth/runs/run_1");
    await setSandboxStatusMock(app, "running");

    // Count how many SSE connections are opened. ⚠️ **The listener must be attached before goto**, or
    // the first one is missed.
    let sse = 0;
    app.on("request", (r) => {
      if (r.url().includes("/sandbox/terminal") && !r.url().includes("/input")) sse++;
    });
    await app.goto("/w/growth/runs/run_1?view=terminal");
    await expect(app.getByTestId("sandbox-terminal-host")).toBeVisible();
    await app.waitForTimeout(1500);

    // ⚠️ This guards against a real bug: under StrictMode effects run twice, and cleanup did not fully
    // clear the previous round, so **two terminals and two SSE connections** were created: two prompt
    // lines on the page, and the status bar saying both "connected" and "disconnected" (two variables
    // each saying their own thing).
    expect(sse, "开了不止一条 SSE —— 清理没收干净，StrictMode 第二遍又建了一个").toBe(1);

    // The status is a **single enum**, so "connected" and "disconnected" can never appear together.
    //
    // In mock mode there is no real container, so SSE cannot connect -> it lands on closed, which is right.
    // What must be locked in is "not contradicting itself": when closed there should be no "connected".
    const state = app.getByTestId("sandbox-terminal-state");
    const phase = await state.getAttribute("data-phase");
    if (phase === "closed") {
      await expect(app.getByText("已连接", { exact: true })).toHaveCount(0);
    } else {
      await expect(app.getByText("连接已断开")).toHaveCount(0);
    }
  });

  test("终端那一档不给输入框", async ({ app }) => {
    await app.goto("/w/growth/chat/run_1");
    await setSandboxStatusMock(app, "running");

    // The chat view has an input box.
    await app.goto("/w/growth/chat/run_1");
    await expect(app.getByTestId("chat-input")).toBeVisible();

    // The terminal view does not: there is already a real terminal waiting for the keyboard, and another
    // input box would make people unsure whether a keystroke goes to the agent or into the container, two
    // things with very different consequences.
    await app.goto("/w/growth/chat/run_1?view=terminal");
    await expect(app.getByTestId("sandbox-terminal-host")).toBeVisible();
    await expect(app.getByTestId("chat-input")).toHaveCount(0);
  });

  test("文件详情：点文件看内容，点目录进目录", async ({ app }) => {
    await app.goto("/w/growth/runs/run_1");
    await setSandboxStatusMock(app, "running");
    await app.goto("/w/growth/runs/run_1?view=files");

    // First go into the subdirectory, then click a file inside.
    await app.getByText("workspace", { exact: true }).click();
    await app.getByText("main.go", { exact: true }).click();

    const detail = app.getByTestId("sandbox-file-detail");
    await expect(detail).toBeVisible();
    await expect(app.getByTestId("sandbox-file-content")).toContainText("package main");

    // After closing, the detail disappears and the list stays.
    await app.getByTestId("sandbox-file-close").click();
    await expect(detail).toHaveCount(0);
    await expect(app.getByTestId("sandbox-files")).toBeVisible();
  });

  test("对话页也有这两档", async ({ app }) => {
    await app.goto("/w/growth/chat/run_1");
    await setSandboxStatusMock(app, "running");
    await app.goto("/w/growth/chat/run_1?view=files");

    await expect(app.getByTestId("sandbox-panel")).toHaveAttribute("data-status", "running");
    await expect(app.getByTestId("sandbox-files")).toBeVisible();
  });

  /**
   * Should a terminal connection failure be reported? **The whole value of this group is "do not report
   * when it should not be reported"**.
   *
   * # Why this needs its own lock
   *
   * The terminal uses SSE + POST, **not on the `useAsyncData` / `useWriteError` paths**, so originally
   * none of its failures were recorded (a zero-error collection blind spot). Once wired in, the opposite
   * risk appeared at once: after a session ends the container is paused when idle and recycled past its
   * TTL, and a user opening an old session days later will certainly fail to connect the terminal:
   * **that is a normal ending**. Reporting unconditionally would add a feedback entry for every old
   * session opened, drowning out real problems. So the test is "is the container still there".
   *
   * # ⚠️ How to give "the panel sees running" and "the container is gone at failure time" a definite order
   *
   * The mock's container state is **read fresh from localStorage on every call**, so a single marker
   * cannot build this scenario (with the marker at gone the panel does not render the terminal at all).
   * Here `page.route` **holds that SSE**, and lets it fail only after the marker has been changed to
   * gone: the order is then definite, with no sleep racing.
   *
   * ⚠️ **This is one of the very few requests `page.route` can really intercept in local mode**:
   * `EventSource` is created directly in the component, not through `LocalApiClient`, so it is a real
   * same-origin GET. Do not generalize this elsewhere: other APIs send no network requests at all in
   * local mode.
   *
   * ⚠️ **It must use `fulfill(500)`, not `abort()`**: `EventSource` **reconnects automatically** on
   * network-level failures (readyState goes back to CONNECTING); only an HTTP error code or a wrong
   * content-type makes it give up for good (CLOSED), and our reporting only hangs on the CLOSED branch.
   * With abort neither case would trigger the code under test, and **both would be false greens**.
   */
  test.describe("终端连接失败的上报", () => {
    test.beforeEach(async ({ app }) => {
      await clearFeedbackMock(app);
    });
    test.afterEach(async ({ page }) => {
      await clearFeedbackMock(page);
    });

    /**
     * Hold the SSE and hand back a "make it fail" switch. Only after the returned function is called does
     * that connection get 500.
     */
    async function holdTerminalSSE(app: Page): Promise<() => void> {
      let release: () => void = () => {};
      const held = new Promise<void>((r) => {
        release = r;
      });
      await app.route("**/sandbox/terminal", async (route) => {
        await held;
        // 500 + text/plain: makes EventSource **give up for good** (CLOSED), with no automatic reconnect.
        await route.fulfill({ status: 500, contentType: "text/plain", body: "boom" });
      });
      return release;
    }

    test("容器已回收：连不上是预期结果，不上报", async ({ app }) => {
      const fail = await holdTerminalSSE(app);

      await app.goto("/w/growth/runs/run_1");
      await setSandboxStatusMock(app, "running");
      await app.goto("/w/growth/runs/run_1?view=terminal");
      // Wait for the terminal to really mount (xterm is lazy-loaded); by now the SSE has been sent and is held by us.
      await expect(app.getByTestId("sandbox-terminal-host")).toBeVisible();

      // The container was recycled meanwhile: the real scenario is exactly this order: still there when
      // entering the page, gone by the time the connection fails.
      await setSandboxStatusMock(app, "gone");
      fail();

      // First wait for it to finish handling the failure: phase becoming recycled means the "fresh check" came back.
      // **This step also locks in the text**: saying "disconnected" here would call a normal ending a fault.
      const state = app.getByTestId("sandbox-terminal-state");
      await expect(state).toHaveAttribute("data-phase", "recycled");
      await expect(app.getByText("连接已断开")).toHaveCount(0);

      // ⚠️ **This wait is required, not just for safety.**
      // After phase becomes recycled, the "report" path does one more fresh check (90ms) before reaching
      // submitFeedback. Asserting 0 entries right away, **it would still be 0 with the check removed**: the
      // case would be a false green, and the whole point of this case is to prevent exactly that.
      // 1500ms is far more than 90+90+submit, and mutation testing confirmed: with the check removed this goes red.
      await app.waitForTimeout(1500);
      expect(
        await feedbackReports(app),
        "容器已回收时连不上是预期结果，上报它会把真问题淹在噪音里",
      ).toHaveLength(0);
    });

    test("容器还活着却连不上：这是真故障，上报且错误码可自解释", async ({ app }) => {
      const fail = await holdTerminalSSE(app);

      await app.goto("/w/growth/runs/run_1");
      await setSandboxStatusMock(app, "running");
      await app.goto("/w/growth/runs/run_1?view=terminal");
      await expect(app.getByTestId("sandbox-terminal-host")).toBeVisible();

      // The container is fine all along, it just cannot connect: **this is what should be collected**.
      fail();

      await expect
        .poll(async () => (await feedbackReports(app)).length, {
          message: "容器活着却连不上，这是真故障，必须被记录",
        })
        .toBe(1);

      const [report] = await feedbackReports(app);
      expect(report.level).toBe("auto");
      // ⚠️ `network_error` cannot be reused: that would mix it into the "the whole backend is not up"
      // kind, and the two need completely different debugging directions.
      expect(report.errorCode).toBe("terminal_disconnected");
      // The EventSource error event has no status code; **0 means "no HTTP response was received"**,
      // matching the meaning of network_error. Making up a fake status code is worse than leaving it empty.
      expect(report.httpStatus).toBe(0);
      // Level one carries no full path and no agent (the user clicked nothing); this boundary holds for the new path too.
      expect(report.path).toBeUndefined();
      expect(report.agentId).toBeUndefined();
      // The session id must already be erased: it is in the URL, and level one has no user consent.
      expect(report.routePattern).toBe("/w/growth/runs/{id}");

      // The UI says "disconnected", not "recycled": the container is still there, and the two must not be mixed up.
      await expect(app.getByTestId("sandbox-terminal-state")).toHaveAttribute(
        "data-phase",
        "closed",
      );
    });
  });

  /**
   * The two entries at the top right of the terminal: fullscreen and "open in new tab".
   *
   * # This group watches three things, each with zero errors
   *
   * | Way to break | Symptom |
   * | --- | --- |
   * | URL parameter not restored | drops out of fullscreen after refresh; users think "it was not saved" |
   * | no exit after going fullscreen | users get stuck and can only delete the query by hand (the repo rule "hiding controls by default is dangerous") |
   * | refit not triggered | "fullscreen but the terminal is still a small block": purely visual, no error at all |
   *
   * # ⚠️ Why the refit case measures xterm's rows instead of the container box
   *
   * The container box's size is given directly by CSS (`flex-1`), so **it always changes**: measuring it
   * stays all green with refit removed (a false green). What really stays unchanged without refit is
   * **xterm's own cols/rows**, which FitAddon computes from the box and writes into the Terminal instance.
   * So the assertion reads `__term` (a read-only reference SandboxTerminal deliberately attaches, existing
   * exactly for this: xterm v6 renders to canvas, so there is no text in the DOM to assert on).
   * Mutation-verified: removing that effect turns this red.
   */
  test.describe("终端的全屏与新标签页入口", () => {
    /** Enter the terminal view and wait for xterm to really mount (it is lazy-loaded). */
    async function openTerminal(app: Page, path = "/w/growth/runs/run_1") {
      await app.goto(path);
      await setSandboxStatusMock(app, "running");
      await app.goto(`${path}?view=terminal`);
      await expect(app.getByTestId("sandbox-terminal-host")).toBeVisible();
    }

    /** Read the current cols/rows of the xterm instance. Attached at `__term` on the host node. */
    async function termSize(app: Page): Promise<{ cols: number; rows: number }> {
      return app.evaluate(() => {
        const host = document.querySelector('[data-testid="sandbox-terminal-host"]') as
          | (HTMLElement & { __term?: { cols: number; rows: number } })
          | null;
        if (!host?.__term) throw new Error("拿不到 xterm 实例 —— __term 那个测试口子没了");
        return { cols: host.__term.cols, rows: host.__term.rows };
      });
    }

    test("点全屏铺满视口，URL 上留下 full=1，刷新后仍是全屏", async ({ app }) => {
      await openTerminal(app);

      // First confirm it is **not fullscreen by default**: without confirming, an "always fullscreen"
      // implementation would also make everything below green.
      await expect(app.getByTestId("sandbox-terminal-fullscreen")).toHaveCount(0);

      await app.getByTestId("sandbox-terminal-fullscreen-toggle").click();

      const shell = app.getByTestId("sandbox-terminal-fullscreen");
      await expect(shell).toBeVisible();
      // It really fills the viewport: the measured box equals the window size.
      const box = await shell.boundingBox();
      const vp = app.viewportSize();
      expect(box?.width).toBeCloseTo(vp!.width, 0);
      expect(box?.height).toBeCloseTo(vp!.height, 0);

      // The URL has full=1, **and view=terminal is still there**: both parameters coexist without
      // overriding each other. Without the latter, "fullscreen kicks the user back to the chat view", with zero errors.
      await expect(app).toHaveURL(/[?&]view=terminal/);
      await expect(app).toHaveURL(/[?&]full=1/);

      // Still fullscreen after a refresh: that is the whole point of "the parameter goes into the URL".
      await app.reload();
      await expect(app.getByTestId("sandbox-terminal-fullscreen")).toBeVisible();
      await expect(app.getByTestId("sandbox-terminal-host")).toHaveAttribute("data-full", "1");
    });

    test("全屏后终端跟着变大：xterm 的 rows 真的增加了", async ({ app }) => {
      await openTerminal(app);
      const before = await termSize(app);

      await app.getByTestId("sandbox-terminal-fullscreen-toggle").click();
      await expect(app.getByTestId("sandbox-terminal-fullscreen")).toBeVisible();

      /*
        ⚠️ It must poll rather than read once: refit is deliberately pushed two frames later (a synchronous
        call always gets the old size, see the comment on that effect in SandboxTerminal), so reading right
        away gets the old value.

        What is asserted is **rows increased**, not "changed": fullscreen is taller than 60vh, so the
        direction is certain. Written as "!==", an implementation that shrinks the terminal would also pass.
      */
      await expect
        .poll(async () => (await termSize(app)).rows, {
          message: "全屏后 xterm 的 rows 没变多 —— refit 没触发，终端字符区还是原来那么小",
        })
        .toBeGreaterThan(before.rows);

      // Exiting fullscreen must **shrink it back**; it cannot only grow.
      await app.getByTestId("sandbox-terminal-fullscreen-toggle").click();
      await expect(app.getByTestId("sandbox-terminal-fullscreen")).toHaveCount(0);
      await expect
        .poll(async () => (await termSize(app)).rows, {
          message: "退出全屏后 rows 没收回去 —— 终端内容会被裁掉",
        })
        .toBeLessThanOrEqual(before.rows);
    });

    test("全屏时退出入口始终可见，点它就能回来", async ({ app }) => {
      await openTerminal(app);
      await app.getByTestId("sandbox-terminal-fullscreen-toggle").click();
      await expect(app.getByTestId("sandbox-terminal-fullscreen")).toBeVisible();

      /*
        ⚠️ **This is where the "do not trap people" lesson lands.**

        After going fullscreen the URL carries full=1 and was replaced in, so the browser's back button pops
        the user out of this page instead of exiting fullscreen. The UI must have a **visible** exit.

        Assert `toBeVisible`, not `toBeAttached`: a hidden button (opacity-0 / only on hover) is as good as
        none, and that is exactly the implementation to guard against.
      */
      const toggle = app.getByTestId("sandbox-terminal-fullscreen-toggle");
      await expect(toggle).toBeVisible();
      // The icon direction switches too: in fullscreen it is "collapse", not "expand".
      await expect(toggle).toHaveAttribute("data-full", "1");

      await toggle.click();
      await expect(app.getByTestId("sandbox-terminal-fullscreen")).toHaveCount(0);
      // After coming back the terminal is still there (the whole component was not unmounted: that would
      // lose the history output).
      await expect(app.getByTestId("sandbox-terminal-host")).toBeVisible();
      await expect(app).not.toHaveURL(/[?&]full=1/);
    });

    test("终端里的 Esc 不会退出全屏（要留给 vim）", async ({ app }) => {
      await openTerminal(app);
      await app.getByTestId("sandbox-terminal-fullscreen-toggle").click();
      await expect(app.getByTestId("sandbox-terminal-fullscreen")).toBeVisible();

      /*
        ⚠️ **This is a reverse assertion; write it carefully.**

        Esc in the terminal must pass through to the shell (vim uses it to leave insert mode). A keydown
        listener that exits fullscreen would pop vim users out on every press.

        Assert "**still** fullscreen after pressing Esc": this is "the element should keep existing", not
        "the element should not appear", so `toBeVisible` does not fall into the "satisfied before rendering"
        trap. But a render window is still needed: exiting fullscreen is a React re-render, and asserting right
        away might run before it.
      */
      await app.getByTestId("sandbox-terminal-host").click();
      await app.keyboard.press("Escape");
      await app.waitForTimeout(400);

      await expect(app.getByTestId("sandbox-terminal-fullscreen")).toBeVisible();
      await expect(app).toHaveURL(/[?&]full=1/);
    });

    test("新标签页入口是真锚点，指向可直接用的全屏终端", async ({ app }) => {
      await openTerminal(app);

      /*
        ⚠️ What is asserted is **the shape of the href**, not "what is in the new tab after clicking".

        Two reasons: (1) opening a new tab in local mode means starting another page and resetting the mock
        again, which tests Playwright rather than this code; (2) what really breaks is the href: missing one
        parameter means "after opening you still need to click twice", which the requirement explicitly ruled out.

        Using `<a target="_blank">` instead of window.open is deliberate: middle-click, Cmd/Ctrl+click and the
        right-click menu are behaviors browsers give anchors for free. So this also locks in "it really is
        an anchor".
      */
      const link = app.getByTestId("sandbox-terminal-newtab");
      await expect(link).toBeVisible();
      // It is an anchor, not a button: otherwise all those browser behaviors above are lost.
      expect(await link.evaluate((el) => el.tagName)).toBe("A");
      await expect(link).toHaveAttribute("target", "_blank");
      // rel is required: without it, the opened page could control this page back through window.opener.
      await expect(link).toHaveAttribute("rel", /noopener/);

      const href = await link.getAttribute("href");
      // Usable on opening: terminal view + fullscreen, both parameters present.
      expect(href).toContain("/w/growth/runs/run_1");
      expect(href).toContain("view=terminal");
      expect(href).toContain("full=1");
    });

    test("对话页的终端也有这两个入口", async ({ app }) => {
      // ⚠️ **This repo has hit "a multi-level feature wired into only one level"**: the panel is reused by
      // two pages, and checking only the run detail page lets half of it slip through.
      await openTerminal(app, "/w/growth/chat/run_1");

      await expect(app.getByTestId("sandbox-terminal-newtab")).toBeVisible();
      await app.getByTestId("sandbox-terminal-fullscreen-toggle").click();
      await expect(app.getByTestId("sandbox-terminal-fullscreen")).toBeVisible();

      // The chat page's href points to **the run detail page** (a new tab needs neither the session list
      // nor the input area), but it is still the same run.
      const href = await app.getByTestId("sandbox-terminal-newtab").getAttribute("href");
      expect(href).toContain("/w/growth/runs/run_1");
    });

    test("状态与按钮在同一行，且不再显示创建/到期时间", async ({ app }) => {
      await openTerminal(app);

      /*
        ⚠️ **Judging "the same row" needs geometric positions; asserting both exist is not enough.**

        Before the change they each took a row (status row on top, button row below), and in that version
        both elements were also `toBeVisible`: asserting only existence, changing back to two rows would
        still be all green. So compare the y-axis centers.
      */
      const chip = app.getByTestId("sandbox-status");
      const toggle = app.getByTestId("sandbox-terminal-fullscreen-toggle");
      await expect(chip).toBeVisible();
      await expect(toggle).toBeVisible();

      const cb = await chip.boundingBox();
      const tb = await toggle.boundingBox();
      expect(cb).not.toBeNull();
      expect(tb).not.toBeNull();

      /*
        Compare center points rather than top: the two have different heights (the Chip is shorter than the
        icon button), so their tops should not be equal anyway.

        ⚠️ The threshold is 12px rather than 0: in the same flex row, after `items-center` alignment the
        center difference is a few pixels (rounding from different child heights). But the difference in a
        two-row layout is over 40px: a threshold well below "the height of one row" separates the two. The
        reverse of the repo rule "do not judge wrapping by the row's center line": here it compares the
        centers of two **sibling elements**, not a row stretched by its children.
      */
      const chipMid = cb!.y + cb!.height / 2;
      const toggleMid = tb!.y + tb!.height / 2;
      expect(
        Math.abs(chipMid - toggleMid),
        `状态徽标与全屏按钮不在同一行（中心相差 ${Math.abs(chipMid - toggleMid)}px）——两行会白占约 60px 高度`,
      ).toBeLessThan(12);

      // Button on the right, status on the left: the two ends of the same toolbar.
      expect(tb!.x, "按钮应该在状态徽标右边").toBeGreaterThan(cb!.x);

      /*
        Creation/expiry times are no longer shown.

        ⚠️ toHaveCount(0) is fine here, because **a positive anchor in the same tree was already waited for**
        (both chip and toggle are visible), so the render window has passed: the repo trap "toHaveCount(0)
        is satisfied in the frame before rendering" does not apply at this point.
      */
      await expect(app.getByText(/创建于/)).toHaveCount(0);
      await expect(app.getByText(/到期于/)).toHaveCount(0);
    });

    test("容器不可用时也给得出退出全屏的入口", async ({ app }) => {
      /*
        ⚠️ This guards a specific trapped scenario: the container gets recycled while fullscreen (it will,
        when idle past its TTL), and at that moment the "terminal only opens while the container runs"
        branch replaces the terminal tree. If the two branches each rendered their own toolbar, this one would
        certainly be the one missed, and it is exactly the one that needs an exit most.
      */
      await app.goto("/w/growth/runs/run_1");
      await setSandboxStatusMock(app, "gone");
      await app.goto("/w/growth/runs/run_1?view=terminal&full=1");

      await expect(app.getByTestId("sandbox-terminal-closed")).toBeVisible();
      // The fullscreen shell is still there, and the exit button is visible.
      await expect(app.getByTestId("sandbox-terminal-fullscreen")).toBeVisible();
      const toggle = app.getByTestId("sandbox-terminal-fullscreen-toggle");
      await expect(toggle).toBeVisible();

      await toggle.click();
      await expect(app.getByTestId("sandbox-terminal-fullscreen")).toHaveCount(0);
    });
  });
  /**
   * Upstream failure (keystrokes cannot get into the container): the frontend side of a real production
   * incident.
   *
   * Measured in production: one report reached **25 occurrences**: someone failed on every keystroke
   * for a minute, with no hint in the UI, typing blind. This group watches two things: failures must be
   * visible (with reconnect), and **do not send 25 reports**.
   */
  test.describe("终端上行失败", () => {
    test.beforeEach(async ({ app }) => {
      await clearFeedbackMock(app);
    });
    test.afterEach(async ({ page }) => {
      await clearFeedbackMock(page);
      // reset() cannot wipe this key; without clearing it leaks into the next case.
      await clearTerminalInputMock(page);
    });

    /**
     * Make the terminal really connect: the mock has no backend, so SSE must fake a ready frame itself.
     *
     * ⚠️ EventSource is created directly in the **transport layer** (lib/terminalTransport.ts), not through
     * LocalApiClient, so page.route works on it in local mode (one of the very few places).
     *
     * ⚠️ This stub ends the response after one frame, which means **the stream "ends normally" right away**.
     * That is not a disconnect: the browser goes to CONNECTING on its own when a stream ends, and the
     * transport layer deliberately only judges it disconnected at `readyState === CLOSED` (see the comment
     * there: otherwise two reconnect mechanisms stack, doubling the connections, measured 1 -> 3 -> 5).
     * So this stub does not need to hold the stream open.
     */
    async function connectTerminal(app: Page): Promise<void> {
      await app.route("**/sandbox/terminal", async (route) => {
        await route.fulfill({
          status: 200,
          headers: { "content-type": "text/event-stream; charset=utf-8" },
          // The first frame gives terminalId; the frontend only sends keystrokes after getting it.
          body: 'event: ready\ndata: {"terminalId":"term_e2e"}\n\n',
        });
      });
      await app.goto("/w/growth/runs/run_1");
      await setSandboxStatusMock(app, "running");
      await failTerminalInputMock(app);
      await app.goto("/w/growth/runs/run_1?view=terminal");
      await expect(app.getByTestId("sandbox-terminal-host")).toBeVisible();
      await expect(app.getByTestId("sandbox-terminal-state")).toHaveAttribute(
        "data-phase",
        "connected",
      );
    }

    /** Type n characters into xterm, through the __term reference the component deliberately leaves for testing. */
    async function type(app: Page, n: number): Promise<void> {
      await app.evaluate((count) => {
        const host = document.querySelector('[data-testid="sandbox-terminal-host"]');
        const term = (host as (HTMLElement & { __term?: { input(d: string): void } }) | null)?.__term;
        for (let i = 0; i < count; i++) term?.input("x");
      }, n);
    }

    test("连打很多下也只上报一次，且看得见失败", async ({ app }) => {
      await connectTerminal(app);

      // That production record was exactly 25 times.
      await type(app, 25);

      // Failures must be visible: the status turns red + "reconnect" appears.
      await expect(app.getByTestId("sandbox-terminal-state")).toHaveAttribute(
        "data-phase",
        "closed",
      );
      await expect(app.getByTestId("sandbox-terminal-reconnect")).toBeVisible();

      // Let all async reports settle, then count.
      await app.waitForTimeout(1500);

      const reports = await feedbackReports(app);
      expect(reports.length, "上行失败应当只产生一条记录").toBe(1);
      // ⚠️ **The test is occurrences, not the number of entries**: both the mock and the backend dedupe by
      // routePattern|errorCode|httpStatus and only accumulate occurrences, so `toHaveLength(1)` is green with
      // or without the latch: that would make this case as good as unwritten.
      expect(
        reports[0]?.occurrences ?? 1,
        "闩没生效：25 次按键各报了一次（线上就是这么攒出 25 的）",
      ).toBe(1);
      expect(reports[0]?.errorCode).toBe("terminal_input_failed");
    });

    test("点重连会重建连接", async ({ app }) => {
      await connectTerminal(app);
      await type(app, 3);
      await expect(app.getByTestId("sandbox-terminal-reconnect")).toBeVisible();

      // Remove the fault before reconnecting, simulating "that was just a hiccup".
      await clearTerminalInputMock(app);
      await app.getByTestId("sandbox-terminal-reconnect").click();

      // Reconnected; the button disappears.
      await expect(app.getByTestId("sandbox-terminal-state")).toHaveAttribute(
        "data-phase",
        "connected",
      );
      await expect(app.getByTestId("sandbox-terminal-reconnect")).toHaveCount(0);

      // ⚠️ **This part is the half that locks in failedRef being reset.**
      //
      // The assertions above **stay all green** when "dispose forgets to reset the latch" (verified: deleting
      // that line, both cases pass), because reconnecting itself does not depend on the latch. What really
      // breaks is **the next failure**: the latch is still locked, so the failure hint and the report never
      // appear again, with zero errors.
      //
      // So fail once more and see whether the hint comes back.
      await failTerminalInputMock(app);
      await type(app, 3);
      await expect(
        app.getByTestId("sandbox-terminal-reconnect"),
        "重连之后再失败，提示没有回来——dispose 里漏了重置 failedRef，" +
          "这个退化是静默的：用户从此再也看不到失败提示",
      ).toBeVisible();
    });
  });
});

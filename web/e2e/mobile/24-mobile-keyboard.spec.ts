/**
 * Mobile input area and keyboard avoidance (iPhone 16 Pro, 402×681).
 *
 * # What these tests guard and what they do not
 *
 * They **do not guard** "is it usable once the keyboard is up": in Chromium the soft keyboard never opens,
 * `env(safe-area-inset-*)` is always 0, and `visualViewport` behaves differently from iOS.
 * Those can only be checked on a real device; the list is at the end of the file.
 *
 * They guard the **wiring contract "the variables are really hooked up"**: once `--kb` / `--safe-bottom` have values,
 * the input area must move out of the way as expected. This is measured accurately in Chromium because it only concerns CSS evaluation,
 * and it happens to be the link most likely to break with zero errors: misspelling `var(--kb)`, using a variable name that does not exist,
 * or being overridden by the inline style of `py-3`, **the page is completely fine on desktop**, and only on a real device does it show up as
 * "the keyboard covers the input box". So here `page.evaluate` sets `--kb` to a value by hand,
 * then asserts the input area really moved.
 *
 * # Why the assertions are on "height difference" and not "a specific pixel value"
 *
 * Asserting `paddingBottom === "312px"` would also pin down the `0.75rem` baseline,
 * so tweaking the padding later would go red, which has nothing to do with keyboard avoidance. Here it is always **measure a baseline first,
 * measure again after the change, compare the difference**; the difference is the definition of "avoidance works".
 */

import { test, expect } from "../fixtures";
import type { Page } from "@playwright/test";

/** Keyboard height injected by hand. 300 is roughly the height of an iPhone Chinese keyboard; the exact value does not matter. */
const FAKE_KB_PX = 300;
/** Safe-area height injected by hand. 34px is the real value on an iPhone with a Home Indicator. */
const FAKE_SAFE_BOTTOM_PX = 34;

/**
 * Open the chat page and wait until the input area has really rendered.
 *
 * ⚠️ This gate is required, for the same reason as `gotoShell` in 23-mobile-shell: when `AuthGate` mounts,
 * `chat-composer` is not yet in the DOM, and any "element does not exist / size is 0" assertion would be a false green.
 * **And during that time it is now a blank page**: the startup gate became "a text-less spinner that only appears after 300ms"
 * (see StartupSpinner); on the fast path not even a spinner shows, yet assertions are still satisfied.
 */
async function gotoChat(page: Page, path: string) {
  await page.goto(path);
  await expect(page.getByTestId("chat-composer")).toBeVisible();
  await expect(page.getByTestId("chat-input")).toBeVisible();
}

/** Read the input area's bottom padding and the position of its bottom edge on screen. */
async function measureComposer(page: Page) {
  return page.evaluate(() => {
    const el = document.querySelector('[data-testid="chat-composer"]');
    if (!el) throw new Error("找不到 chat-composer —— 输入区不在了");
    const input = document.querySelector('[data-testid="chat-input"]')!;
    const r = el.getBoundingClientRect();
    const ir = input.getBoundingClientRect();
    return {
      paddingBottom: parseFloat(getComputedStyle(el).paddingBottom),
      height: r.height,
      /** The input box's own bottom edge. The end goal of keyboard avoidance is that it is not covered. */
      inputBottom: ir.bottom,
      viewportHeight: window.innerHeight,
    };
  });
}

/** Write CSS variables onto `<html>` by hand, simulating "keyboard up / has safe area". */
async function setRootVars(page: Page, vars: Record<string, string>) {
  await page.evaluate((v) => {
    for (const [k, val] of Object.entries(v)) {
      document.documentElement.style.setProperty(k, val);
    }
  }, vars);
}

test("--kb 有值时输入区上移，且消息流跟着变矮", async ({ app }) => {
  // Use an existing conversation: it has a message list, so the "message list moves out of the way" half can be checked too.
  await gotoChat(app, "/w/growth/chat/run_1");

  const before = await measureComposer(app);
  const streamBefore = await app.evaluate(
    () => document.querySelector('[data-slot="chat-conversation"]')!.clientHeight,
  );

  await setRootVars(app, { "--kb": `${FAKE_KB_PX}px` });

  const after = await measureComposer(app);
  const streamAfter = await app.evaluate(
    () => document.querySelector('[data-slot="chat-conversation"]')!.clientHeight,
  );

  /*
    The bottom padding must grow by exactly one keyboard height.

    ⚠️ This must assert **the difference equals FAKE_KB_PX**; `toBeGreaterThan(before)` alone is not enough.
    The loose version stays green when `max(var(--safe-bottom), var(--kb))` is written as
    `calc(var(--safe-bottom) + var(--kb))`, and that is exactly the bug to guard against
    (the keyboard already covers the bottom edge of the screen; adding the safe area on top leaves an extra 34px gap).
    In Chromium `--safe-bottom` is always 0, so max and + happen to give the same number here;
    what really separates them is the test below, "safe-bottom and kb are one or the other".
  */
  expect(after.paddingBottom - before.paddingBottom).toBe(FAKE_KB_PX);

  /*
    **This line is the dividing line of "padding instead of transform", and the point of this test.**

    If the implementation used `transform: translateY(calc(-1 * var(--kb)))`, the padding assertion above
    would go red, sure. But if someone changed it to something like "transform + a placeholder element of the same height",
    padding might change too, while the message list height **would not change at all**, and the last message would still be covered.
    The message list is a flex sibling of the input area (`flex-1`); only when the input area really grows and really triggers relayout
    does it get shorter. This is the physical precondition for requirement 4 (the message list scrolls to the bottom along with it).
  */
  expect(streamBefore - streamAfter).toBe(FAKE_KB_PX);
});

test("safe-bottom 与 kb 是二选一，不叠加", async ({ app }) => {
  await gotoChat(app, "/w/growth/chat/run_1");

  /*
    ⚠️ In Chromium `env(safe-area-inset-bottom)` is always 0, so **`--safe-bottom` must also be set
    by hand**, otherwise this test tests thin air: when both are 0,
    `max(a, b)` and `a + b` give the same answer, and a wrong implementation stays green.
    This is exactly the concrete form of the limitation that this project cannot test real-device safe areas.
  */
  await setRootVars(app, { "--safe-bottom": `${FAKE_SAFE_BOTTOM_PX}px`, "--kb": "0px" });
  const keyboardClosed = await measureComposer(app);

  await setRootVars(app, { "--kb": `${FAKE_KB_PX}px` });
  const keyboardOpen = await measureComposer(app);

  /*
    With no keyboard up, room for the safe area must be left (otherwise the Home Indicator covers it).
    The baseline is the padding with "both 0", so measure 0/0 once more here as reference.
  */
  await setRootVars(app, { "--safe-bottom": "0px", "--kb": "0px" });
  const bare = await measureComposer(app);
  expect(keyboardClosed.paddingBottom - bare.paddingBottom).toBe(FAKE_SAFE_BOTTOM_PX);

  /*
    With the keyboard up, the safe area **gives way entirely**; it is not added on top of the keyboard.

    Check: the padding is exactly one keyboard height more than the bare baseline, not kb + safe-bottom.
    An implementation that adds them gets 334, and this line goes red right there. On a real device those 34px are an extra gap
    between the input box and the keyboard that appears out of nowhere.
  */
  expect(keyboardOpen.paddingBottom - bare.paddingBottom).toBe(FAKE_KB_PX);
  expect(keyboardOpen.paddingBottom - bare.paddingBottom).not.toBe(
    FAKE_KB_PX + FAKE_SAFE_BOTTOM_PX,
  );
});

test("输入框字号 >= 16px，横屏也是", async ({ app }) => {
  await gotoChat(app, "/w/growth/chat");

  const portrait = await app.evaluate(() => {
    const ta = document.querySelector('[data-testid="chat-input"]')!;
    return {
      fontSize: parseFloat(getComputedStyle(ta).fontSize),
      width: window.innerWidth,
      coarse: window.matchMedia("(pointer: coarse)").matches,
    };
  });

  /*
    First pin the precondition: this project really emulates a touch device. Without this, if the device preset got overwritten,
    the test would run on a `pointer: fine` desktop viewport, where `pointer-coarse:` does not apply,
    and portrait 402px is already 16px, so the assertion stays green while verifying nothing.
  */
  expect(portrait.coarse).toBe(true);
  expect(portrait.width).toBe(402);
  // Below 16px, iOS zooms the whole page on focus, and does not zoom back by itself.
  expect(portrait.fontSize).toBeGreaterThanOrEqual(16);

  /*
    ⚠️ **Landscape is the case that really drops to 14px; the whole value of this test is here.**

    HeroUI's `.textarea` is `text-base sm:text-sm`, and `sm` is 640px:
    iPhone portrait at 402px never reaches that media query and is already 16px. In other words, testing only portrait,
    **an implementation doing nothing would be all green**. Landscape at 874px crosses 640, `sm:text-sm` applies,
    and it really drops to 14px, while it is still a touch device and still zooms the page.
    This is also why the implementation checks `pointer: coarse` and not the viewport width.
  */
  await app.setViewportSize({ width: 874, height: 402 });
  const landscape = await app.evaluate(() => {
    const ta = document.querySelector('[data-testid="chat-input"]')!;
    return {
      fontSize: parseFloat(getComputedStyle(ta).fontSize),
      width: window.innerWidth,
      coarse: window.matchMedia("(pointer: coarse)").matches,
    };
  });
  expect(landscape.width).toBe(874);
  expect(landscape.coarse).toBe(true);
  expect(landscape.fontSize).toBeGreaterThanOrEqual(16);
});

test("聚焦输入框时消息流滚到底", async ({ app }) => {
  await gotoChat(app, "/w/growth/chat/run_1");

  /*
    ⚠️ **First wait for Pro's own opening scroll to stop, then push it to the top.**

    When `ChatConversation` mounts it does `scrollTo({behavior: "smooth"})` to the bottom (see its
    `useLayoutEffect`). That is an animation spanning several frames. Writing `scrollTop = 0` without waiting for it,
    later frames of the animation move the position back, so the `scrollTop` read in the next line is an in-between value
    (2 was observed), and the test fails now and then with "Expected 0, Received 2", a failure message that does not
    point at "an animation is fighting you" at all. This pitfall showed up during mutation testing: an unrelated mutation
    turned this test red too.

    The check is "position unchanged for N frames in a row", more reliable than sleeping a fixed number of milliseconds.

    ⚠️ **N must be greater than 2.** "Unchanged for two frames in a row" does not stop this: other effects on the page trigger
    another render after data comes back (for example the refresh of the queue list), and the smooth animation **starts again**
    after that render. So the position often holds still for two frames and then gets pushed away; the test reads the in-between value 2 again,
    and the failure message still says "Expected 0" rather than "someone restarted the animation".
    8 unchanged frames in a row is enough to get past one React commit and the animation start after it.
  */
  await app.evaluate(
    () =>
      new Promise<void>((resolve) => {
        const el = document.querySelector('[data-slot="chat-conversation"]')!;
        let last = -1;
        let stable = 0;
        const tick = () => {
          if (el.scrollTop === last) {
            stable += 1;
            if (stable >= 8) {
              resolve();
              return;
            }
          } else {
            stable = 0;
            last = el.scrollTop;
          }
          requestAnimationFrame(tick);
        };
        requestAnimationFrame(tick);
      }),
  );

  // Create the state "the user scrolled up to read history", and wait for it to settle.
  const before = await app.evaluate(
    () =>
      new Promise<{ top: number; max: number }>((resolve) => {
        const el = document.querySelector('[data-slot="chat-conversation"]')!;
        el.scrollTop = 0;
        // One more frame to confirm nobody changed it back, then read both values at once.
        // Reading them in two evaluate calls would leave a window for the animation to slip in.
        requestAnimationFrame(() =>
          resolve({ top: el.scrollTop, max: el.scrollHeight - el.clientHeight }),
        );
      }),
  );

  /*
    ⚠️ Avoid a false green: with less than one screen of content there is nothing to scroll, `scrollTop` is always 0,
    and the "scrolled to bottom" assertion below becomes `0 >= 0`, always true.
    First confirm there really is room to scroll.
  */
  expect(before.max).toBeGreaterThan(0);
  expect(before.top).toBe(0);

  await app.getByTestId("chat-input").focus();

  /*
    Use poll instead of a bare read: the scroll triggered by focus needs a render tick,
    and the ChatConversation in ui/Chat.tsx also has its own ResizeObserver involved.
    A bare `evaluate` reads the previous frame, which shows up as an occasional failure.
  */
  await expect
    .poll(async () =>
      app.evaluate(() => {
        const el = document.querySelector('[data-slot="chat-conversation"]')!;
        // Allow 4px tolerance: ChatConversation also uses 4px for its own "at bottom" check.
        return el.scrollHeight - el.scrollTop - el.clientHeight <= 4;
      }),
    )
    .toBe(true);
});

test("在消息流上拖动会收起键盘（blur 输入框）", async ({ app }) => {
  await gotoChat(app, "/w/growth/chat/run_1");

  const input = app.getByTestId("chat-input");
  await input.focus();
  await expect(input).toBeFocused();

  const box = await app
    .locator('[data-slot="chat-conversation"]')
    .boundingBox();
  if (!box) throw new Error("消息流拿不到 boundingBox");

  /*
    Use a real touch event sequence, not `mouse.wheel` or changing scrollTop directly.

    The implementation deliberately listens to `touchstart` / `touchmove` and not `scroll`, because `scroll`
    cannot tell "the user is swiping" from "code is scrolling", and the input area itself scrolls the message list once on focus;
    listening to scroll would **dismiss the keyboard right away on every focus**. So real touches are required here;
    changing scrollTop cannot reach this path (nor catch the self-triggering bug above).
  */
  const cx = Math.round(box.x + box.width / 2);
  const cy = Math.round(box.y + box.height / 2);
  await app.touchscreen.tap(cx, cy).catch(() => {
    /* the tap only makes sure the touch subsystem is ready; failure does not matter */
  });
  await input.focus();
  await expect(input).toBeFocused();

  // Dispatch a drag by hand: starting on the message list, moving vertically past the threshold (8px).
  await app.evaluate(
    ({ x, y }) => {
      const el = document.querySelector('[data-slot="chat-conversation"]')!;
      const mk = (type: string, clientY: number) =>
        new TouchEvent(type, {
          bubbles: true,
          cancelable: true,
          touches:
            type === "touchend"
              ? []
              : [
                  new Touch({
                    identifier: 1,
                    target: el,
                    clientX: x,
                    clientY,
                  }),
                ],
        });
      el.dispatchEvent(mk("touchstart", y));
      el.dispatchEvent(mk("touchmove", y - 40));
      el.dispatchEvent(mk("touchend", y - 40));
    },
    { x: cx, y: cy },
  );

  // Dragged across the message list → the keyboard should close, that is, the input box loses focus.
  await expect(input).not.toBeFocused();
});

test("点一下消息流不收键盘（阈值以内的抖动要忽略）", async ({ app }) => {
  await gotoChat(app, "/w/growth/chat/run_1");

  const input = app.getByTestId("chat-input");
  await input.focus();
  await expect(input).toBeFocused();

  const box = await app.locator('[data-slot="chat-conversation"]').boundingBox();
  if (!box) throw new Error("消息流拿不到 boundingBox");
  const cx = Math.round(box.x + box.width / 2);
  const cy = Math.round(box.y + box.height / 2);

  /*
    A 3px jitter: the real size of a finger "tapping" on a list, below the 8px threshold in the implementation.

    Without this test, setting the threshold to 0 would also be all green, and that would make "tapping a link in a message"
    dismiss the keyboard too; to the user it looks like "what I typed is still there, but the keyboard vanished for no reason".
  */
  await app.evaluate(
    ({ x, y }) => {
      const el = document.querySelector('[data-slot="chat-conversation"]')!;
      const mk = (type: string, clientY: number) =>
        new TouchEvent(type, {
          bubbles: true,
          cancelable: true,
          touches:
            type === "touchend"
              ? []
              : [new Touch({ identifier: 1, target: el, clientX: x, clientY })],
        });
      el.dispatchEvent(mk("touchstart", y));
      el.dispatchEvent(mk("touchmove", y - 3));
      el.dispatchEvent(mk("touchend", y - 3));
    },
    { x: cx, y: cy },
  );

  /*
    ⚠️ Leave a time window here for a "wrong implementation to really get a chance to blur", otherwise the assertion
    passes before the blur happens. This repo fell into the same pitfall (e2e toHaveCount(0) /
    "element should not appear" style assertions satisfied in the frame before rendering).
    Wait a little first, then assert focus is still there.
  */
  await app.waitForTimeout(200);
  await expect(input).toBeFocused();
});

test("agent 选择器在窄屏不被压到截断", async ({ app }) => {
  await gotoChat(app, "/w/growth/chat");

  const m = await app.evaluate(() => {
    const sel = document.querySelector('[data-testid="chat-agent-select"]')!;
    const r = sel.getBoundingClientRect();
    return {
      width: r.width,
      /** The actual content width of the text inside the trigger; wider than the container means it is truncated. */
      scrollWidth: sel.scrollWidth,
      clientWidth: sel.clientWidth,
      innerWidth: window.innerWidth,
    };
  });

  expect(m.innerWidth).toBe(402);
  /*
    It used to be a fixed `w-52` (208px), with the `ml-auto` send button on the same row, so at 402px the agent name
    got squeezed into truncation. With `flex-1` on narrow screens it can take up that 100px of empty space.
    Assert "the content fits" rather than "the width equals some value"; the latter would pin layout details.
  */
  expect(m.scrollWidth).toBeLessThanOrEqual(m.clientWidth);
  // Also pin "it really got wider": 208 is the old fixed value and must not appear again.
  expect(m.width).toBeGreaterThan(208);
});

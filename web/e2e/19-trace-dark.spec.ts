import { expect, setServerPrefs, test } from "./fixtures";

/**
 * Switch appearance.
 *
 * ⚠️ **Both the local cache and "the server copy" must be written; writing only one does not work**,
 * with a trap in each direction:
 *
 * - writing only the server (`setServerPrefs`): that copy only takes effect after AuthGate calls
 *   `syncFromServer`, and the first frame still renders from the cache;
 * - writing only the local cache (`ama.prefs.v1`): `syncFromServer` **trusts the database**
 *   (the line in `PrefsContext.tsx:154`, "setPrefs even if it equals the cache"),
 *   so the mock's default `system` **overwrites** the dark just written.
 *
 * The second is exactly what was hit the first time this spec was written: the bodyBg read in dark
 * and light was exactly the same (both the light oklch), and the error only said "expected them to
 * differ", not pointing at all to "the preference was overwritten by the server copy".
 */
async function setAppearance(
  page: import("@playwright/test").Page,
  appearance: "light" | "dark",
) {
  await page.goto("/workspaces");
  await page.evaluate((a) => {
    localStorage.setItem("ama.prefs.v1", JSON.stringify({ language: "zh-CN", appearance: a }));
  }, appearance);
  await setServerPrefs(page, { language: "zh-CN", appearance });
}

/**
 * **Dark mode** checks for the call tree panel, measured for real.
 *
 * # Why this deserves its own spec
 *
 * This repo has twice hit "right in light, wrong in dark, with zero errors":
 *
 * 1. **Unlayered CSS beats `@layer`** (when md-box was integrated): light was fine, and everything dark
 *    was overridden wholesale by `[theme-mode="dark"]` back to GitHub's #0d1117. Noted near
 *    `index.css:352`.
 * 2. **Tokens that do not exist were used** (`text-on-accent` / `bg-danger-subtle`): Tailwind silently
 *    outputs nothing for unknown class names, `tsc` and `build` were all green, light looked right too,
 *    and **only dark exposed it** (white text on white, button text vanished entirely).
 *
 * What the two have in common is that **neither typecheck nor build can stop them**. So what is
 * asserted here is **the color the browser really computes**, not "looks right".
 *
 * # What these assertions can and cannot catch (measured; do not overestimate them)
 *
 * Mutation testing ran two kinds of "bad token":
 *
 * | Mutation | Result | Why |
 * | --- | --- | --- |
 * | `text-background` (exists but misused) | **caught**, reports 1.00:1 with identical colors | that is the real shape of "white on white in dark" |
 * | `text-on-accent` (a token that **does not exist**) | **not caught** | Tailwind silently outputs no rule, the color falls back to the inherited value, which happens to still be readable |
 *
 * So these assertions watch **"the computed color is unreadable"**, not "the class name is spelled
 * right". When a token is misspelled into one that does not exist, the contrast is still fine; that has
 * to be checked with the `grep -c '\.text-xxx{' dist/assets/*.css` rule in `frontend.md` (output 0
 * means the class does not exist), which is a separate matter. It is written here so the next person
 * does not think "dark mode has tests, so it is safe".
 *
 * # Also locks in "the official component's styles were not pulled in"
 *
 * The panel's other option was to embed CozeLoop's official observability component directly.
 * It **puts styles on `:root`**, uses bare RGB triples `--coze-up-*`, and is full of `!important`:
 * exactly the shape of point 1 above, only worse. It was not chosen in the end (see the file header of
 * TracePanel), so this asserts `--coze-up-*` / `--semi-*` **should not appear** on `:root`: if someone
 * brings it back someday, this goes red.
 */
test.describe("调用树 · 深色模式", () => {
  /** The computed colors of all "should be visible" text in the panel + their own backgrounds. */
  async function probeColors(page: import("@playwright/test").Page) {
    return page.evaluate(() => {
      const rootStyle = getComputedStyle(document.documentElement);

      /** Walk up the ancestors to find the first non-transparent background: the text's actual contrast background. */
      const effectiveBg = (el: Element | null): string => {
        let cur: Element | null = el;
        while (cur) {
          const bg = getComputedStyle(cur).backgroundColor;
          if (bg && bg !== "rgba(0, 0, 0, 0)" && bg !== "transparent") return bg;
          cur = cur.parentElement;
        }
        return getComputedStyle(document.body).backgroundColor;
      };

      /**
       * Get the **title** span of a certain kind of node.
       *
       * ⚠️ `node.querySelector("span")` cannot be used: leaf nodes put an **empty placeholder span** where
       * the toggle would be (with no children they still need the same width, or the titles of each level
       * would shift left and right), and it comes before the title, so `querySelector` grabs that empty
       * shell: the color read is the inherited value and `textContent` is an empty string, so the assertion
       * looks like it "passes" while testing nothing. So filter by the one that has text.
       */
      const titleOf = (sel: string) => {
        const node = document.querySelector(sel);
        if (!node) return null;
        // ⚠️ **The title must be found by testid, not "the first span with text".**
        // After following the official structure, the first span with text in a row is the **type color
        // block** (containing the two letters `Mo` / `To`), not the title. Grabbing the wrong one makes this
        // assertion measure the color block's foreground/background contrast instead of title readability:
        // the name says one thing and measures another.
        const holder = node.querySelector('[data-testid="trace-span-name"]');
        const span = holder?.querySelector("span") ?? holder;
        if (!span) return null;
        return {
          text: (span.textContent ?? "").trim(),
          color: getComputedStyle(span).color,
          bg: effectiveBg(span),
        };
      };

      // The official node row's identifying symbol is "color block + two-letter abbreviation" (CustomIconWrapper).
      //
      // ⚠️ **Do not find it by `span[aria-hidden]`**: leaf nodes put an **empty aria-hidden placeholder
      // span** where the collapse toggle would be (with no children they still need the same width, or the
      // titles of each level would shift left and right), and it comes before the color block. Grabbing it
      // gives a backgroundColor of `rgba(0,0,0,0)`, and the assertion reports "should not be transparent":
      // **it looks like a misspelled token name**, while the real cause is just picking the wrong element.
      // So filter by "has Mo/To text".
      const badge = Array.from(
        document.querySelectorAll<HTMLElement>(
          '[data-testid="trace-node-model"] span, [data-testid="trace-node-tool"] span',
        ),
      ).find((el) => /^(Mo|To|Ag)$/.test((el.textContent ?? '').trim()));

      return {
        // Did the theme really switch (read both forms; the implementation may use a class or an attribute)
        htmlClass: document.documentElement.className,
        themeAttr: document.documentElement.getAttribute("theme-mode"),
        bodyBg: getComputedStyle(document.body).backgroundColor,
        // The structure follows the official SpanNode: root (agent) + child spans (model / tool), two levels.
        // **There is no turn level anymore** (the old self-invented three levels were changed).
        root: titleOf('[data-testid="trace-node-agent"]'),
        model: titleOf('[data-testid="trace-node-model"]'),
        tool: titleOf('[data-testid="trace-node-tool"]'),
        // The color block's background comes from a token and must really be computed (when a token name is
        // misspelled Tailwind silently outputs nothing, and the block becomes transparent: looks like it did not render).
        badgeBg: badge ? getComputedStyle(badge).backgroundColor : null,
        badgeText: badge ? (badge.textContent ?? '').trim() : null,
        // The official component's fingerprint. Having a value means its styles leaked in.
        cozeVar: rootStyle.getPropertyValue("--coze-up-brand-7").trim(),
        semiVar: rootStyle.getPropertyValue("--semi-color-white").trim(),
      };
    });
  }

  /**
   * Color string -> relative luminance (WCAG).
   *
   * ⚠️ **Do not grab numbers with a regex as RGB.** HeroUI v3's tokens are all **oklch**, and
   * `getComputedStyle` returns `oklch(0.1798 0.0043 264.36)` as-is (Chrome no longer converts to rgb).
   * Grabbing the first three `\d+` numbers gives L=0.17->0, C=0.004->0, so both the dark and light colors
   * compute to nearly 0, with contrast always 1.09:1: **looking exactly like the real "white on white"
   * failure**, while the colors are actually right. This was hit on the very first run, and the error
   * pointed in completely the wrong direction.
   *
   * So the browser does the conversion: put it into an element and read back `color`, and Chrome gives a
   * parseable form; if it is still oklch, take the L component directly (oklch's L is itself perceived
   * lightness, good enough to judge "which is darker").
   */
  function luminance(color: string): number {
    const oklch = color.match(/^oklch\(\s*([\d.]+)/i);
    if (oklch) {
      // oklch's L is already a perceived lightness from 0 to 1. Convert to an approximate relative luminance:
      // L^3 is closer to WCAG's linear luminance scale, comparable in magnitude to the sRGB one when used for
      // contrast ratios.
      const L = Number(oklch[1]);
      return Math.pow(L, 3);
    }
    const m = color.match(/[\d.]+/g);
    if (!m) return 0;
    const [r, g, b] = m.slice(0, 3).map((v) => {
      const c = Number(v) / 255;
      return c <= 0.03928 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4);
    });
    return 0.2126 * r + 0.7152 * g + 0.0722 * b;
  }

  function contrast(fg: string, bg: string): number {
    const a = luminance(fg);
    const b = luminance(bg);
    return (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);
  }

  for (const appearance of ["light", "dark"] as const) {
    test(`${appearance}：span 树各层标题都可读，类型色块有颜色`, async ({ app }) => {
      await setAppearance(app, appearance);
      await app.goto("/w/growth/runs/run_1?view=trace");
      await expect(app.getByTestId("trace-tree")).toBeVisible();

      const probe = await probeColors(app);
      // Attached to the report, so the computed colors are visible directly on failure.
      test.info().annotations.push({
        type: `${appearance} 实测颜色`,
        description: JSON.stringify(probe, null, 2),
      });

      // The theme really did switch.
      if (appearance === "dark") {
        expect(probe.htmlClass).toContain("dark");
      } else {
        expect(probe.htmlClass).not.toContain("dark");
      }

      // All three levels of titles must exist, with **enough contrast against their own background**.
      //
      // 3:1 is a deliberately chosen floor: body text should reach 4.5:1, but secondary text like
      // `text-muted` is a step lower by design, and requiring 4.5 would give false alarms. Real failures
      // like "white on white" compute to nearly 1:1, so 3:1 is enough to catch them without judging
      // deliberate design weakening as failure.
      for (const [name, v] of Object.entries({
        root: probe.root,
        model: probe.model,
        tool: probe.tool,
      })) {
        expect(v, `${name} 节点的标题应该存在`).not.toBeNull();
        // The title must really have text: see the empty placeholder span trap in titleOf.
        expect(v!.text.length, `${name} 节点的标题不该是空串`).toBeGreaterThan(0);
        const ratio = contrast(v!.color, v!.bg);
        expect(
          ratio,
          `${appearance} 下 ${name} 标题对比度只有 ${ratio.toFixed(2)}:1` +
            `（文字 ${v!.color} / 背景 ${v!.bg}）——这正是「用了不存在的 token」的形状`,
        ).toBeGreaterThan(3);
      }

      // The official color block must have a real background and a two-letter abbreviation.
      expect(probe.badgeBg, "类型色块应该有背景色").not.toBeNull();
      expect(probe.badgeBg).not.toBe("rgba(0, 0, 0, 0)");
      expect(probe.badgeText, "色块里是官方那套两字母缩写（Mo / To）").toMatch(/^(Mo|To)$/);

      // Not one of the official component's style fingerprints should be present.
      expect(probe.cozeVar, "不该出现 --coze-up-* （那说明官方组件被装回来了）").toBe("");
      expect(probe.semiVar, "不该出现 --semi-* （semi-ui 的指纹）").toBe("");
    });
  }

  test("深色与浅色的底色确实不同", async ({ app }) => {
    // A safety net: if the theme switch did not take effect at all, the two cases above would each pass
    // "self-consistently" (the dark one actually reads light values, and the contrast is still fine). So
    // cross-compare once.

    await setAppearance(app, "light");
    await app.goto("/w/growth/runs/run_1?view=trace");
    await expect(app.getByTestId("trace-tree")).toBeVisible();
    const light = await probeColors(app);

    await setAppearance(app, "dark");
    await app.goto("/w/growth/runs/run_1?view=trace");
    await expect(app.getByTestId("trace-tree")).toBeVisible();
    const dark = await probeColors(app);

    expect(dark.bodyBg).not.toBe(light.bodyBg);
    // The dark background should be darker than the light one.
    expect(luminance(dark.bodyBg)).toBeLessThan(luminance(light.bodyBg));
    // The title color must flip too: only the background changing while the text does not is the really
    // dangerous half-applied case.
    expect(dark.root?.color).not.toBe(light.root?.color);
  });
});

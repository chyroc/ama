/**
 * Writes the height taken by the soft keyboard into the CSS variable `--kb`, so elements fixed to
 * the bottom can move out of the way.
 *
 * # Why this must be JS, not pure CSS
 *
 * On iOS Safari, the keyboard opening **does not change any vh unit**: `100vh` / `100dvh` /
 * `100svh` all
 * stay put, and the `resize` event may not fire. The only thing that notices is
 * `window.visualViewport`:
 * the layout viewport does not change, but the visual viewport is shrunk by the keyboard.
 *
 * The `interactive-widget=resizes-content` in the viewport meta is **for Android Chrome**;
 * iOS Safari still does not implement it (the title of MDN browser-compat-data PR #29064 is
 * "Safari iOS does not support interactive-widget viewport feature"; WebKit Bugzilla
 * 259770 is still "Implement …"). So that attribute does nothing on iOS, and keyboard avoidance
 * can only live here.
 *
 * # Only write the variable, never touch layout
 *
 * This hook deliberately **only exposes the number**: it does not scroll and does not change any
 * element's style.
 * Whoever needs to move uses `var(--kb)` themselves (e.g. the composer's `padding-bottom`). This
 * way keyboard logic lives in
 * one place, while the avoidance strategy can differ by component.
 */

import { useEffect } from "react";

/**
 * Listen to the visual viewport and sync the keyboard height into `--kb` (set on `<html>`).
 *
 * Should be called only once on the whole site; the call site is `AppShell` (the only app shell).
 */
export function useVisualViewport(): void {
  useEffect(() => {
    const vv = window.visualViewport;
    // If unsupported, do nothing: `--kb` keeps the CSS default 0px, i.e. "there is never a keyboard".
    // This is safer than polyfilling a fake value: wrong avoidance is harder to debug than none.
    if (!vv) return;

    const root = document.documentElement;

    function sync() {
      // Keyboard height = layout viewport height − visual viewport height − the visual viewport's
      // vertical offset.
      //
      // offsetTop is subtracted because Safari sometimes does not shrink the visual viewport but
      // **pushes it up as a whole**
      // (the page is panned up); then the height difference is 0 but offsetTop is positive. Both
      // forms must come out right.
      //
      // ⚠️ Math.max(0, …) is not a casual defensive clamp; it fixes a specific bug:
      // **on iOS 26 `visualViewport.offsetTop` does not go back to zero after the keyboard closes**
      // (Apple Developer
      // Forums thread 800125 / 800154, WebKit Bugzilla 311821 / 317749).
      // Without the clamp the result is negative, and the bottom fixed area is pushed off screen
      // **and never comes back**.
      // It looks like "after using the input once the bottom bar is gone", with not a single error.
      const raw = window.innerHeight - vv!.height - vv!.offsetTop;
      const kb = Math.max(0, Math.round(raw));
      root.style.setProperty("--kb", `${kb}px`);
    }

    // Sync once up front: the keyboard may already be open when entering the page (focus carried
    // over from another page).
    sync();

    // resize is the keyboard opening/closing; scroll is the "visual viewport pushed up" form above.
    // We need both; listening only to resize misses the pan.
    vv.addEventListener("resize", sync);
    vv.addEventListener("scroll", sync);

    return () => {
      vv.removeEventListener("resize", sync);
      vv.removeEventListener("scroll", sync);
      // On unmount remove the variable and go back to the CSS default, so no stale keyboard height
      // is left behind.
      root.style.removeProperty("--kb");
    };
  }, []);
}

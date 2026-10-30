/**
 * A flag that "only counts if it is still not done after a while".
 *
 * Used to avoid **a loading indicator flashing on the fast path**: most requests come back within
 * one or two hundred ms, and
 * showing a spinner/skeleton then only makes the user feel "the page twitched". That twitch is
 * more jarring than the wait itself,
 * because moving things grab the eye, and by the time they do, the information is already gone.
 *
 * On the other hand, when it is slow we must show something, or "nothing shown" reads as
 * "clicked and nothing happened".
 * So this is delayed display, not no display.
 *
 * # Why a separate hook instead of putting it in LoadingBlock
 *
 * `LoadingBlock` has 7 callers, 5 of which are **in-page** loads (agent detail, run detail,
 * schedule detail, chat page...). In those places the shell is already there and the user clearly
 * clicked something;
 * a loading block with text is exactly the feedback they want, and a delay would only make the
 * page look stuck.
 * Only the two **full-page startup gates** need a quiet period, so the delay belongs to the caller,
 * not the shared loading block.
 */

import { useEffect, useState } from "react";

/**
 * Turns true `ms` milliseconds after mount; false before that.
 *
 * ⚠️ **`ms <= 0` starts as true right away**, with no timer. Two uses: callers that want to turn
 * off the quiet period
 * need no branch; and mutation testing (set the delay to 0; the test asserting "not shown in the
 * first 300ms" must go red)
 * takes this path. If 0 still waited one frame, that mutation would catch nothing.
 *
 * ⚠️ Timing starts from **this component's mount**, not from page load. The two full-page gates
 * run in series
 * (AuthGate checks identity → WorkspaceGate fetches the workspace), so on a slow network the
 * worst case is two quiet periods,
 * with the spinner disappearing and reappearing in between. This is accepted on purpose: if it
 * counted "from page load",
 * then when clicking into another workspace inside the app `performance.now()` is long past
 * 300ms, and the spinner would pop up **immediately**.
 * That is exactly what this is meant to remove, and it would happen every day, while chained slow
 * loads only happen on a bad network.
 */
export function useDelayedFlag(ms: number): boolean {
  const [on, setOn] = useState(ms <= 0);

  useEffect(() => {
    if (ms <= 0) {
      setOn(true);
      return;
    }
    // When ms changes we must restart the timer, so reset first. On first mount it is already
    // false, and React skips the update itself.
    setOn(false);
    const id = window.setTimeout(() => setOn(true), ms);
    // Unmount means clear: the gate finishing inside the quiet period is **the main path** (that is
    // exactly the result this hook wants);
    // without clearing, every fast load leaves a timer that later calls setState on an unmounted
    // component.
    return () => window.clearTimeout(id);
  }, [ms]);

  return on;
}

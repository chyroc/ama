/**
 * Terminal fullscreen state, the one and only copy.
 *
 * # Why in the URL and not component state
 *
 * It follows the site-wide rule "all state goes in the URL, links can be shared, refresh keeps
 * context" (see
 * `lib/useTimelineFilter.ts` and `components/ViewSwitch.tsx`). There is a harder reason here
 * too:
 * **the requirement is "still fullscreen after refresh"**. Component state is always lost on
 * refresh, so it cannot do this.
 *
 * The parameter is `?full=1`, **alongside** the existing `?view=terminal`: fullscreen is "how to
 * show",
 * view is "what to show"; two independent axes each get a parameter. Sharing one (e.g.
 * `?view=terminal-full`) would make `useRunView` not recognize it and fall back to the chat view,
 * so "fullscreen" would kick the user out of the terminal, with zero errors.
 *
 * # ⚠️ This is not the Fullscreen API
 *
 * This is CSS-level "fill the browser viewport" (`fixed inset-0`), not
 * `element.requestFullscreen()`. The difference is not just looks:
 *
 *   - The Fullscreen API **must be triggered by a user gesture**, so "restore fullscreen after
 *     refresh" is
 *     impossible with it (there is no gesture on page load; the browser refuses and only logs one
 *     line in the console).
 *     That is the core point of this requirement.
 *   - The browser manages exit itself (Esc, UI hints), which would fight with our own Esc handling.
 *
 * So we use CSS. The cost is that the browser's own address bar/tab bar is still there, which is
 * expected:
 * the user wants "the terminal not squeezed into a panel", not "hide the browser".
 */

import { useCallback } from "react";
import { useSearchParams } from "react-router-dom";

/** Fullscreen parameter name. `1` means on; missing/any other value means off. */
export const FULLSCREEN_PARAM = "full";

/**
 * Read whether fullscreen is on, and provide a toggle.
 *
 * The check is `=== "1"`, not "the parameter exists": `?full=0` should be off, and checking
 * existence would treat it as on;
 * such a link, once shared, would behave opposite to what it says.
 */
export function useTerminalFullscreen(): [boolean, (next: boolean) => void] {
  const [params, setParams] = useSearchParams();
  const full = params.get(FULLSCREEN_PARAM) === "1";

  const setFull = useCallback(
    (next: boolean) => {
      /*
       * ⚠️ **The base must be `window.location.search`, not `params` from the closure.**
       *
       * This trap was hit and written down in `useTimelineFilter`, and applies here word for word:
       * when two URL
       * states change in a row (e.g. switching to the terminal view then clicking fullscreen right
       * after), the second call still gets `params`
       * from the render snapshot before the first, so the first change is silently overwritten.
       * It shows up as "clicked fullscreen and it went back to
       * the chat view", with no error at all.
       *
       * The functional overload of `setSearchParams` **does not fix this**: react-router 7
       * implements it as
       * `nextInit(new URLSearchParams(searchParams))`, and the argument is still that memoized
       * snapshot.
       */
      const updated = new URLSearchParams(window.location.search);
      // Off is the default and is not written to the URL, to avoid leaving a query with no
      // information.
      if (next) updated.set(FULLSCREEN_PARAM, "1");
      else updated.delete(FULLSCREEN_PARAM);
      // replace: toggling fullscreen should not push a browser history entry, or "back" would
      // bounce between two sizes.
      // Same tradeoff as useRunView / useTimelineFilter.
      setParams(updated, { replace: true });
    },
    [setParams],
  );

  return [full, setFull];
}

/**
 * Build a "terminal only" address, for "open in new tab".
 *
 * **Reuse the existing route + parameters, do not add a new route.** The reason is that what this
 * page needs
 * (run, container state, the SSE and POST channels, the four-state display) all lives on
 * `runs/:runId`;
 * a new route would have to wire them up again, a second implementation of the same logic, and
 * changing one would surely miss the other.
 * With `?view=terminal&full=1` it opens as a usable fullscreen terminal, no extra click needed.
 *
 * ⚠️ Use the **run detail page**, not the chat page, even when the caller is the chat page: the
 * chat page also fetches the conversation list,
 * creates the SSE stream and renders the composer, none of which the new tab needs. The terminal
 * view on both pages is the same
 * component, so the experience is no different.
 */
export function terminalTabPath(workspaceKey: string, runId: string): string {
  return `/w/${encodeURIComponent(workspaceKey)}/runs/${encodeURIComponent(runId)}?view=terminal&${FULLSCREEN_PARAM}=1`;
}

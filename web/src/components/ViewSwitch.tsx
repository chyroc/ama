/**
 * View switch: chat / events / call tree / files / terminal.
 *
 * The same RunEvent can be read three ways (DESIGN.md 3.6.3): the chat view collapses by turn, the event view is a filterable
 * vertical timeline, and the call tree view is a span tree (root + model requests/tool calls).
 * **Chat is the default**; the others are reached through this switch. The run detail page and the chat page share it.
 *
 * # Why the call tree was moved from "the bottom of the event view" into its own option
 *
 * It used to hang at the **very bottom** of the event view, so to see the call tree you had to open the event view and scroll past the whole timeline
 * (long runs have hundreds of entries). The two are **read differently by nature**: the event view answers "what happened",
 * the call tree answers "where did the time go". Burying the latter at the end of the former turns an independent view into an appendix of another.
 * With the options side by side, `?view=trace` can also be shared / opened directly.
 *
 * The state lives in the URL's `?view=` rather than component state: this matches the site-wide rule "links are shareable, refresh keeps context"
 * and the existing `?tab=` convention on the Agent detail page; e2e can also goto the event view directly,
 * without clicking the switch and waiting for animations.
 *
 * Uses components/ui/Segment: it hard-codes `disallowEmptySelection` and only calls back when the key is non-null,
 * so the "nothing selected" empty state cannot happen and no extra fallback is needed.
 */

import { useSearchParams } from "react-router-dom";
import { Segment } from "@/components/ui/Segment";
import { useT } from "@/prefs/PrefsContext";

export type RunView = "conversation" | "events" | "trace" | "files" | "terminal";

const PARAM = "view";

/**
 * Reads the current view from the URL and returns a switch function.
 *
 * Only the four values `events` / `trace` / `files` / `terminal` are accepted; anything else (missing, misspelled, old links) falls back to the chat view;
 * the default view should not change because of one unrecognized parameter.
 */
export function useRunView(): [RunView, (next: RunView) => void] {
  const [params, setParams] = useSearchParams();
  const raw = params.get(PARAM);
  const view: RunView =
    raw === "events"
      ? "events"
      : raw === "trace"
        ? "trace"
        : raw === "files"
          ? "files"
          : raw === "terminal"
            ? "terminal"
            : "conversation";

  const setView = (next: RunView) => {
    const updated = new URLSearchParams(params);
    // Chat is the default; it is not written into the URL, to avoid leaving a useless query.
    if (next === "conversation") updated.delete(PARAM);
    else updated.set(PARAM, next);
    // replace: switching views should not push a history entry, otherwise "back" bounces between two views.
    setParams(updated, { replace: true });
  };

  return [view, setView];
}

export function ViewSwitch({
  view,
  onChange,
}: {
  view: RunView;
  onChange: (next: RunView) => void;
}) {
  const t = useT();
  return (
    <Segment
      size="sm"
      aria-label={t("view.switchLabel")}
      selectedKey={view}
      onSelectionChange={(key) => onChange(key as RunView)}
      data-testid="view-switch"
    >
      <Segment.Item id="conversation" data-testid="view-conversation">
        {t("view.conversation")}
      </Segment.Item>
      <Segment.Item id="events" data-testid="view-events">
        {t("view.events")}
      </Segment.Item>
      <Segment.Item id="trace" data-testid="view-trace">
        {t("view.trace")}
      </Segment.Item>
      <Segment.Item id="files" data-testid="view-files">
        {t("view.files")}
      </Segment.Item>
      <Segment.Item id="terminal" data-testid="view-terminal">
        {t("view.terminal")}
      </Segment.Item>
    </Segment>
  );
}

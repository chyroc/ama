/**
 * "Report this problem" entry: a lightweight button next to error states.
 *
 * # Three interaction requirements (the whole reason this component exists)
 *
 * 1. **The user does not need to type anything.** There is no "please describe your problem" form: that would
 *    push the work of diagnosing back to the user, who is often stuck here exactly because they do not know what happened.
 *    All the information we need is collected automatically.
 * 2. **There must be a place for "report or not".** Level one is already recorded silently, but that is for us;
 *    the user needs an action to actively say "this problem is annoying, please look", and that click is also
 *    their consent to more collection.
 * 3. **Before reporting, the user must see what will be reported.** One-click reporting without the user knowing what is sent is
 *    unacceptable. So there is an expandable "view the information to be reported",
 *    and it renders **the actual object to be sent** (`describePayload(payload)`),
 *    not text describing it: text drifts from the implementation, the object does not.
 *
 * # The receipt must show on the button
 *
 * The copy button (`CopyButton` in `RunTimeline.tsx`) already set this pattern:
 * without a global toast, feedback on the button itself is the only place it can be seen.
 * And **the state must also go into the `data-feedback-state` attribute**: the tooltip only renders on hover,
 * so relying on text alone, "received" is not observable at all without a mouse (including e2e).
 */

import { Button, Disclosure } from "@heroui/react";
import { Icon } from "@iconify/react";
import { useEffect, useRef, useState } from "react";
import { api } from "@/data/api";
import { buildFeedbackPayload, describePayload, type FeedbackContext } from "@/lib/feedback";
import type { AsyncError } from "@/lib/useAsyncData";
import { useT } from "@/prefs/PrefsContext";

/** How long the receipt shows (ms). Same as CopyButton / the save notice. */
const ACK_MS = 2400;

export function FeedbackButton({
  error,
  context,
  compact,
}: {
  error: AsyncError;
  /** Context the page can provide (workspace id / agent id). **Only level two reports them.** */
  context?: FeedbackContext;
  /**
   * Compact layout: for inline errors (`InlineError`).
   *
   * In a full error state the button is stacked and centered, which next to a single line of red text leaves a big blank,
   * and "Report this problem" would stand out more than the error itself. The compact mode switches to a row + sm size,
   * **but the collection and reporting logic does not change at all**: both modes share the same payload.
   */
  compact?: boolean;
}) {
  const t = useT();
  const [sent, setSent] = useState(false);
  const [showDetails, setShowDetails] = useState(false);
  // The timer must be cleared on unmount, otherwise it would setState on an unmounted component (same as CopyButton).
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(() => () => {
    if (timer.current) clearTimeout(timer.current);
  }, []);

  // Build the payload during render: **the "information to be reported" panel and what is actually sent must be the same object**.
  // If built twice, the panel shows A and B is sent, and that mismatch happens exactly on
  // the path "the user looked at the panel before clicking", so they would think they know what was sent.
  const payload = buildFeedbackPayload("user", error, context);
  const rows = describePayload(payload, t as (k: string) => string);

  const submit = (): void => {
    // No await: the button gives the receipt immediately. Whether the report succeeds is nothing the user can deal with
    // (submitFeedback itself never rejects; see data/http.ts).
    void api.submitFeedback(payload);
    setSent(true);
    if (timer.current) clearTimeout(timer.current);
    timer.current = setTimeout(() => setSent(false), ACK_MS);
  };

  return (
    <div
      className={
        compact
          ? "flex flex-wrap items-center gap-x-2 gap-y-1"
          : "flex flex-col items-center gap-2"
      }
      data-feedback-state={sent ? "sent" : "idle"}
    >
      <div className="flex items-center gap-2">
        <Button
          variant="outline"
          size={compact ? "sm" : undefined}
          onPress={submit}
          isDisabled={sent}
          data-testid="feedback-submit"
        >
          {sent ? (
            <>
              <Icon icon="gravity-ui:circle-check" className="size-4 text-success" />
              {t("feedback.sent")}
            </>
          ) : (
            t("feedback.report")
          )}
        </Button>
      </div>

      {/* "View the information to be reported". Collapsed by default: it is for those who want to look;
          expanded by default would turn the error state into a wall, and then nobody reads it. */}
      <Disclosure isExpanded={showDetails} onExpandedChange={setShowDetails}>
        <Disclosure.Heading>
          <Button slot="trigger" variant="ghost" size="sm" data-testid="feedback-details-toggle">
            {t("feedback.viewPayload")}
            <Disclosure.Indicator />
          </Button>
        </Disclosure.Heading>
        <Disclosure.Content>
          <Disclosure.Body className="mt-1 flex flex-col gap-2 r-container bg-surface-secondary p-3 text-left">
            <p className="text-xs text-muted">{t("feedback.payloadHint")}</p>
            <dl className="flex flex-col gap-1" data-testid="feedback-payload">
              {rows.map((row) => (
                <div key={row.label} className="flex gap-2 text-xs">
                  <dt className="shrink-0 text-muted">{row.label}</dt>
                  {/* break-all instead of truncate: these values (paths, error text) must be fully readable,
                      and truncation would make the user think we only reported the visible part. */}
                  <dd className="break-all">{row.value}</dd>
                </div>
              ))}
            </dl>
            <p className="text-xs text-muted">{t("feedback.neverCollected")}</p>
          </Disclosure.Body>
        </Disclosure.Content>
      </Disclosure>
    </div>
  );
}

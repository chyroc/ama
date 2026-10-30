/**
 * Copy button. The repo had no "copy" feature before; `RunTimeline` built a minimal one itself.
 * The event view needs it too, so it was pulled out to be shared (two copies of the same interaction will drift).
 *
 * Two points that must be handled:
 *
 * 1. **`navigator.clipboard` may not exist.** It only exists in a secure context (https or localhost);
 *    when accessed via `http://<hostname or intranet IP>` it is undefined. So we use copyText from `lib/clipboard`
 *    (it falls back to execCommand), and when both fail we show visible feedback instead of failing silently.
 * 2. **Feedback must show on the button.** A successful copy has no system notice, so the user cannot tell if it worked;
 *    so the icon briefly turns into a check mark. The timer is cleared on unmount, otherwise it would setState on an unmounted component.
 */

import { useEffect, useRef, useState } from "react";
import { Button, Tooltip } from "@heroui/react";
import { Icon } from "@iconify/react";
import { usePrefs } from "@/prefs/PrefsContext";
import { copyText } from "@/lib/clipboard";

export function CopyButton({ text, label }: { text: string; label?: string }) {
  const { t } = usePrefs();
  const [state, setState] = useState<"idle" | "done" | "failed">("idle");
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => () => {
    if (timer.current) clearTimeout(timer.current);
  }, []);

  const copy = async () => {
    // In a non-secure context (http + not localhost) there is no navigator.clipboard, and copyText falls back to
    // execCommand; it only fails when both fail.
    setState((await copyText(text)) ? "done" : "failed");
    if (timer.current) clearTimeout(timer.current);
    timer.current = setTimeout(() => setState("idle"), 2000);
  };

  const hint =
    state === "done"
      ? t("timeline.copied")
      : state === "failed"
        ? t("timeline.copyFailed")
        : (label ?? t("timeline.copy"));

  return (
    <Tooltip>
      <Tooltip.Trigger>
        <Button
          size="sm"
          variant="ghost"
          isIconOnly
          aria-label={hint}
          onPress={() => void copy()}
          data-testid="copy-button"
          // The copy result also goes into a data attribute: the tooltip only renders on hover,
          // so relying on it alone, "copied" is not observable at all without a mouse (including e2e).
          data-copy-state={state}
        >
          <Icon
            icon={state === "done" ? "gravity-ui:copy-check" : "gravity-ui:copy"}
            className={state === "failed" ? "size-3.5 text-danger" : "size-3.5"}
          />
        </Button>
      </Tooltip.Trigger>
      <Tooltip.Content>{hint}</Tooltip.Content>
    </Tooltip>
  );
}

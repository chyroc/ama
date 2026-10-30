/**
 * List of queued messages: they wait here to be resent after hitting the 409 "the previous tool is still running".
 *
 * It sits right above the input area (the same band as the freeze notice and send errors), because that is where users notice "why was it not sent".
 *
 * # Why it is not drawn as message bubbles
 *
 * The first idea was to draw queued messages as `ChatMessage.User` too, with a "sending" badge. That is the least work,
 * but **it looks almost the same as sent messages**, while the two mean very different things: one is already in the agent's hands,
 * the other has not left yet and can be cancelled or edited at any time. Users must tell them apart at a glance, so it gets a clearly different form
 * (dashed box + queue number + two actions), instead of a small difference on the bubble.
 *
 * # The number is "which one goes out next", not the array index
 *
 * The queue is first-in first-out, so item 1 is the next to be sent. Showing the form `1/3` with the total
 * tells users where they are and how many are behind; showing only "queued" would, with 5 piled up, put
 * 5 identical boxes on screen.
 *
 * # "Send now" is on every item, not only the head
 *
 * What it does is **steer**: interrupt the agent's current turn and handle this message right away. So it has nothing to do with "which
 * position" it is in: the line the user wants to insert may well be the 3rd one just typed (the first two no longer matter).
 * If only the head had it, the user would have to cancel the first two to jump the queue, and the text of those two would be lost.
 *
 * ⚠️ **State the cost.** The words "Send now" do not tell you "it interrupts the current turn", and that is exactly what
 * users need to know before clicking, so the button has a Tooltip. For the same reason it is not called "jump the queue":
 * that phrase suggests the previous turn finishes normally, the opposite of what happens.
 */

import { Button, Tooltip } from "@heroui/react";
import { Icon } from "@iconify/react";
import { useT } from "@/prefs/PrefsContext";
import type { PendingMessage } from "@/types/domain";

interface SendQueueListProps {
  /**
   * Queued messages of the current session, first-in first-out.
   *
   * **A snapshot of the server-side queue** (the pending_messages table), not temporary state in the browser,
   * so they survive a page refresh, and ones queued from Feishu also show up here.
   */
  items: PendingMessage[];
  /** Cancel one; it is simply discarded. */
  onCancel: (id: string) => void;
  /** Put one's text back into the input box for editing (and remove it from the queue). */
  onEdit: (id: string) => void;
  /**
   * Send this one now: interrupt the current turn and talk about it instead (steer).
   *
   * Every item gets it; see the file header for why.
   */
  onSendNow: (id: string) => void;
}

export function SendQueueList({
  items,
  onCancel,
  onEdit,
  onSendNow,
}: SendQueueListProps) {
  const t = useT();
  if (items.length === 0) return null;

  return (
    <div className="px-4 pb-2" data-testid="chat-queue">
      <div className="mx-auto flex w-full max-w-3xl flex-col gap-1.5">
        {/* The note for the queue as a whole is said once, not repeated on each item. */}
        <p className="text-xs text-muted" data-testid="chat-queue-hint">
          {t("chat.queueHint", { count: items.length })}
        </p>

        {items.map((item, i) => (
          <div
            key={item.id}
            /*
              Dashed border = "not sent yet". Deliberately different from the solid bubbles of sent messages;
              see that part of the file header.
            */
            className="flex items-center gap-2 r-inset border border-dashed border-border bg-surface-secondary px-3 py-2"
            data-testid="chat-queue-item"
          >
            <Icon icon="gravity-ui:clock" className="size-3.5 shrink-0 text-muted" />
            {/* Number: `item n / m total`, so users know where they are. */}
            <span
              className="shrink-0 text-xs tabular-nums text-muted"
              data-testid="chat-queue-position"
            >
              {t("chat.queuePosition", { index: i + 1, total: items.length })}
            </span>
            {/*
              The text should show which message it is, but must not push the input area up. clamp-2 is a custom class in the repo
              (Tailwind's line-clamp-* does not work here, see docs/guides/heroui.md);
              the clipping happens at the padding edge, so the clipped element has no py-*.
            */}
            <span className="clamp-2 min-w-0 flex-1 text-xs" data-testid="chat-queue-text">
              {item.text}
            </span>
            <div className="flex shrink-0 items-center gap-1">
              {/*
                "Send now" is the only one of the three actions with an outside effect (it interrupts the agent),
                so it comes first and has a Tooltip explaining the cost. outline instead of solid:
                there is no "primary action" in this row; all three are things the user may want to do,
                and solid would render it as the recommended action, while interrupting the agent should not be recommended.
              */}
              <Tooltip>
                <Button
                  size="sm"
                  variant="outline"
                  onPress={() => onSendNow(item.id)}
                  data-testid="chat-queue-send-now"
                >
                  {t("chat.queueSendNow")}
                </Button>
                <Tooltip.Content>{t("chat.queueSendNowHint")}</Tooltip.Content>
              </Tooltip>
              <Button
                size="sm"
                variant="ghost"
                onPress={() => onEdit(item.id)}
                data-testid="chat-queue-edit"
              >
                {t("chat.queueEdit")}
              </Button>
              <Button
                size="sm"
                variant="ghost"
                onPress={() => onCancel(item.id)}
                data-testid="chat-queue-cancel"
              >
                {t("common.cancel")}
              </Button>
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}

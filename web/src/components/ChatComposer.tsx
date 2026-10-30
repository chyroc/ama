/**
 * Chat input area: agent picker + multi-line input + send/interrupt.
 *
 * Built from the OSS TextArea / Select / Button, without pulling in an extra input component.
 */

import { Button, ListBox, Select, TextArea, Tooltip } from "@heroui/react";
import { Icon } from "@iconify/react";
import { useEffect, useRef, useState, type KeyboardEvent } from "react";
import { useT } from "@/prefs/PrefsContext";
import type { Agent } from "@/types/domain";

/**
 * How many pixels of touch drag count as "the user is scrolling the message stream", which dismisses the keyboard.
 *
 * It cannot be 0: a finger tapping on the list also jitters 1~3px, and then every tap on a message would dismiss the keyboard.
 */
const DISMISS_DRAG_THRESHOLD_PX = 8;

/**
 * Selector for the message stream scroll container.
 *
 * This is the root node of `ChatConversation` in `ui/Chat.tsx`, rendered by `ChatStream`.
 * The input area deals with it twice: scroll it to the bottom on focus, and dismiss the keyboard when the user drags it.
 *
 * Why a global attribute selector instead of passing a ref all the way up from `ChatStream`: the two are sibling nodes
 * in `ChatPage`, with the conditional rendering of view switching in between (in the events / call tree / files / terminal modes it does not
 * exist at all); passing a ref would make `ChatPage` keep a "is there a message stream now" state, and that state's
 * only use would be these two things. The global query is safe here: `ChatConversation` is used in only one place in the repo,
 * `ChatStream` (the run detail page deliberately does not use this shell; see the comment in `ConversationView`),
 * so there is at most one on the page at any moment.
 */
const STREAM_SELECTOR = '[data-slot="chat-conversation"]';

interface ChatComposerProps {
  agents: Agent[];
  /** The selected agent; changeable in a new session, locked in an existing one. */
  agentId: string;
  onAgentChange: (id: string) => void;
  /** Once the session has started the agent cannot be changed (a different agent would not be the same run). */
  agentLocked: boolean;
  onSend: (text: string) => void;
  onInterrupt: () => void;
  /** This turn is still running: the send button becomes interrupt. */
  streaming: boolean;
  isDisabled?: boolean;
  /**
   * Fallback display name when the selected agent is not in `agents` (pass the agentName snapshot from the Run).
   *
   * The case is an existing session whose agent was deleted: `agentId` points to an id no longer in the collection,
   * and then `<Select.Value />` **renders blank**: the session looks like it belongs to nobody.
   */
  agentFallbackName?: string;
  /**
   * Text pushed into the input box from outside, for "edit a queued message and resend it".
   *
   * The shape is `{text, token}` instead of a bare string: **the same text may be put back twice in a row**
   * (the user is halfway through editing and clicks "edit" on another queued message, or clicks the same one, cancels, and clicks back);
   * with a bare string the `useEffect` dependency would not change, and the second time would silently do nothing. token increments each time,
   * so the **action** of "putting back" itself can be observed, not just its result.
   *
   * `null` means no text waiting to be put back (initial state).
   */
  refill?: { text: string; token: number } | null;
}

export function ChatComposer({
  agents,
  agentId,
  onAgentChange,
  agentLocked,
  onSend,
  onInterrupt,
  streaming,
  isDisabled,
  agentFallbackName,
  refill,
}: ChatComposerProps) {
  const t = useT();
  const [text, setText] = useState("");
  const inputRef = useRef<HTMLTextAreaElement>(null);
  const rootRef = useRef<HTMLDivElement>(null);

  /*
   * Puts text handed back from outside into the input box ("edit" a queued message, or return the original text after the queue is full/timed out).
   *
   * **The dependency is only token, not text.** With text, when the user edits after it was put back
   * (setText changes text), this effect would run again and overwrite what they just typed with the original:
   * a zero-error failure that feels like "the input box changes back by itself". token only changes when
   * the "put back" action really happens, which is exactly the moment we want to match.
   */
  useEffect(() => {
    if (!refill) return;
    setText(refill.text);
    // Filling it in without focus would make the user click once more to edit, while what they just clicked was "edit".
    inputRef.current?.focus();
    // eslint-disable-next-line react-hooks/exhaustive-deps -- see above: deliberately follows only token
  }, [refill?.token]);

  /*
   * Whether we can send. **Note this deliberately does not look at `streaming`.**
   *
   * It used to be `... && !streaming`, so once the agent started running, the input box became a read-only decoration:
   * when the user wanted to say a second thing in a row **there was no "send" to click at all** (that spot was taken by "interrupt"),
   * while what they really meant was "note it down now, send it when it finishes". That was the most common complaint about this input area:
   * the UI looked like you could type, but after typing you could not send, with no explanation.
   *
   * Now sending works during streaming too; `ChatPage` just puts it into the queue (see lib/useSendQueue.ts),
   * and sends it automatically when this turn finishes. So only two real gates remain here: is there content, and is the session frozen.
   */
  const canSend = text.trim().length > 0 && !isDisabled;

  function submit() {
    if (!canSend) return;
    onSend(text.trim());
    setText("");
  }

  function onKeyDown(e: KeyboardEvent<HTMLTextAreaElement>) {
    // Enter sends, Shift+Enter is a newline. This is the common expectation for chat boxes; not Cmd+Enter.
    // Enter during IME composition is "pick a word" and must not send; otherwise Chinese users would send on every word they type.
    if (e.key === "Enter" && !e.shiftKey && !e.nativeEvent.isComposing) {
      e.preventDefault();
      submit();
    }
  }

  /**
   * Scrolls the message stream to the bottom.
   *
   * Does nothing if not found: in the events / call tree / files / terminal modes `ChatStream` is not rendered at all,
   * while the input area is still there in the first three.
   */
  function scrollStreamToBottom() {
    const stream = document.querySelector(STREAM_SELECTOR);
    if (stream) stream.scrollTop = stream.scrollHeight;
  }

  /**
   * On focus, pulls both the last message and the input box into view.
   *
   * The keyboard takes half the screen, so this one "focus" must take care of two things at once:
   *
   *   1. **Scroll the message stream to the bottom**: unconditionally, regardless of where it was. `ChatConversation` has its own
   *      "stick to bottom" logic that pushes content back to the bottom when the container gets shorter, but **only when `isAtBottom` is
   *      true**. When the user scrolled up to read history and then taps the input box to type, it does not move, and the last
   *      message stays behind the keyboard. Here we first push it to the bottom unconditionally, which also makes that sticking logic
   *      take the "stay at bottom" branch next: the shrink caused when the keyboard really opens (~300ms after focus)
   *      is therefore covered by it, with no need to guess a delay here.
   *   2. **Scroll the input box itself into view**, `block: "nearest"`: not `center`, which would yank the message stream
   *      away as a whole; here we only require "not covered". In the current layout there is usually no scrollable ancestor, so this
   *      does nothing; it is kept so we do not have to come back and add it when some layer becomes scrollable in the future.
   */
  function onFocus() {
    scrollStreamToBottom();
    rootRef.current?.scrollIntoView({ block: "nearest" });
  }

  /**
   * Dismisses the keyboard when **dragging with a finger** on the message stream.
   *
   * # Why listen to touch instead of scroll
   *
   * `scroll` cannot tell "the user is swiping" from "code is scrolling". The `onFocus` above scrolls the message stream itself,
   * and the message stream's stick-to-bottom scrolls once more when the keyboard opens: listening to `scroll`,
   * **every focus would immediately dismiss the keyboard that just opened**, and the user could not type at all.
   * `touchstart` / `touchmove` are only produced by real fingers, so this self-triggering cannot happen.
   *
   * # Why attach to document instead of the message stream element
   *
   * The message stream is a sibling node in `ChatPage`, and is rebuilt entirely when switching views / sessions. Attached to it, we would have to
   * rebind on every rebuild (missing once means "swiping this session does not dismiss the keyboard", with no error). Attaching to document +
   * checking ownership with `closest()` works no matter how many rebuilds. All three listeners are `passive`: we never
   * `preventDefault`, and without declaring passive the browser would wait for the callback every time before continuing to scroll.
   */
  useEffect(() => {
    /** The starting Y of this touch. null = this touch did not land on the message stream; ignore it. */
    let startY: number | null = null;

    function inStream(target: EventTarget | null): boolean {
      return target instanceof Element && target.closest(STREAM_SELECTOR) !== null;
    }

    function onTouchStart(e: TouchEvent) {
      const touch = e.touches[0];
      startY = touch && inStream(e.target) ? touch.clientY : null;
    }

    function onTouchMove(e: TouchEvent) {
      if (startY === null) return;
      const touch = e.touches[0];
      if (!touch) return;
      // The threshold cannot be 0: a finger "tapping" on the list also jitters 1~3px, and then tapping a message would dismiss the keyboard.
      if (Math.abs(touch.clientY - startY) < DISMISS_DRAG_THRESHOLD_PX) return;
      // Only judge once per touch, then clear it: otherwise every following touchmove would blur again.
      startY = null;
      if (document.activeElement === inputRef.current) inputRef.current?.blur();
    }

    function onTouchEnd() {
      startY = null;
    }

    document.addEventListener("touchstart", onTouchStart, { passive: true });
    document.addEventListener("touchmove", onTouchMove, { passive: true });
    document.addEventListener("touchend", onTouchEnd, { passive: true });
    return () => {
      document.removeEventListener("touchstart", onTouchStart);
      document.removeEventListener("touchmove", onTouchMove);
      document.removeEventListener("touchend", onTouchEnd);
    };
  }, []);

  return (
    <div
      ref={rootRef}
      className="flex flex-col gap-2 border-t border-border bg-background px-4 pt-3"
      /*
        The bottom padding handles two things at once: avoid the Home Indicator without the keyboard, avoid the keyboard when it is open.

        # Why max() instead of adding the two

        When the keyboard opens it **already covers the bottom edge of the screen**, and the Home Indicator gap is under the keyboard;
        adding both would only leave an extra 34px gap between the input box and the keyboard. So it is "whichever is larger",
        not a sum: once `--kb` is non-zero, the safe area part should step aside entirely.

        # Why padding instead of transform

        `translateY(-var(--kb))` only uses the compositing layer and triggers no reflow, which looks cheaper, **but it is wrong**:
        the input area floats up, leaving an empty background strip `--kb` tall below, and the height of the message stream **does not change at all**:
        the last message is still covered by the input area, and requirement 4 (the message stream scrolls to the bottom along with it) cannot be met.

        padding causes reflow, and here **reflow is exactly what we want**: the input area gets taller → the message stream
        (`flex-1`), a flex sibling, gets shorter → content is laid out again above the keyboard. The cost is one
        reflow per keyboard open/close, not one per frame, which does not matter in scale.

        The base 0.75rem is the original `py-3` (the top is still given by `pt-3` in className).
        Written as calc instead of keeping `py-3`: an inline style always beats a class, so the bottom half of `py-3` has actually
        stopped working, and keeping it would only make people think it decides the bottom padding.
      */
      style={{
        paddingBottom: "calc(0.75rem + max(var(--safe-bottom), var(--kb)))",
      }}
      data-testid="chat-composer"
    >
      <div className="mx-auto flex w-full max-w-3xl flex-col gap-2">
        {/*
          The input box uses the secondary variant: it sits on background, and the default variant would blend into
          the background color.

          The disabled state uses native disabled instead of isDisabled: TextArea passes through the native textarea
          props, not the react-aria set (writing isDisabled is a compile error).

          # pointer-coarse:text-base fixes "iOS zooms the whole page on focus"

          iOS Safari **automatically zooms the whole page** when focusing an input with font size < 16px, and after zooming
          it does not zoom back by itself. This repo deliberately does not set `user-scalable=no` (that would also disable the user's own
          pinch zoom, a step back for accessibility), so the only option is to raise the font size to 16px.

          ⚠️ **The condition must be `pointer: coarse`, not viewport width.** HeroUI's `.textarea`
          says `text-base sm:text-sm`, and `sm` is **640px**: an iPhone in portrait at 402px never
          reaches that media query and is 16px already; **what really drops to 14px is landscape** (an iPhone 16
          Pro Max turned sideways is 874px, tested at 14px), and then it is still a touch device and still zooms the whole page.
          Writing it as "raise the font only on narrow screens" would miss exactly the landscape case, and landscape is exactly the posture with the most typing.

          This utility is in `@layer utilities`, and HeroUI's `.textarea` is in
          `@layer components`; the layer order puts the former after the latter, so it wins (tested 14px → 16px).
        */}
        <TextArea
          ref={inputRef}
          variant="secondary"
          className="pointer-coarse:text-base"
          aria-label={t("chat.placeholder")}
          placeholder={t("chat.placeholder")}
          value={text}
          rows={3}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={onKeyDown}
          onFocus={onFocus}
          disabled={isDisabled}
          data-testid="chat-input"
        />

        <div className="flex items-center gap-2">
          {/*
            On narrow screens the picker stretches itself; from sm up it goes back to a fixed 208px.

            At 402px the old `w-52` was tested to squeeze the agent name into truncation (the same row also has the `ml-auto` send
            button, so the parent container had to squeeze it). `min-w-0` is required: a flex child's default
            `min-width:auto` makes it refuse to shrink below its content width; `flex-1` alone does not help.
            On desktop from sm up it is still `w-52`, with zero visual change.
          */}
          <Select
            className="min-w-0 flex-1 sm:w-52 sm:flex-none"
            aria-label={t("chat.pickAgent")}
            selectedKey={agentId || null}
            isDisabled={agentLocked || isDisabled}
            /*
              Disabled agents **stay in the list but cannot be selected**.
              Hiding them would make people think the agent was deleted and go create a new one, while it was only disabled,
              with bindings and skills all still there. Keeping it grayed out, together with the "disabled" suffix below, lets users know
              they should enable it again. The check is the opposite of `!== "active"`: only explicit disabled is blocked,
              unknown values pass (same discipline as the backend's IsDisabled).
            */
            disabledKeys={agents
              .filter((a) => a.status === "disabled")
              .map((a) => a.id)}
            onSelectionChange={(k) => onAgentChange(String(k))}
          >
            <Select.Trigger data-testid="chat-agent-select">
              {/*
                When selectedKey points to an item not in the collection (an existing session whose agent was deleted),
                the default rendering is **blank**: no error, no placeholder, and you cannot tell whom that session belongs to.
                Fall back to the agentName snapshot from the Run.
              */}
              <Select.Value>
                {({ defaultChildren, state }) =>
                  state.selectedItems.length === 0 && agentFallbackName
                    ? agentFallbackName
                    : defaultChildren
                }
              </Select.Value>
              <Select.Indicator />
            </Select.Trigger>
            <Select.Popover>
              <ListBox>
                {agents.map((a) => (
                  <ListBox.Item key={a.id} id={a.id} textValue={a.name}>
                    {/* Write out the reason for graying out, otherwise users only see "it does not click". */}
                    {a.status === "disabled"
                      ? `${a.name}（${t("enum.agentStatus.disabled")}）`
                      : a.name}
                  </ListBox.Item>
                ))}
              </ListBox>
            </Select.Popover>
          </Select>

          <span className="hidden text-xs text-muted sm:inline">{t("chat.hint")}</span>

          {/*
            **Both buttons exist together; it is not one or the other.**

            Before, this was `streaming ? interrupt : send`, so while the agent was running "send" disappeared from the DOM
            entirely: when the user wanted to say a second thing there was nowhere to click, and the typed text got stuck in the box.
            And "interrupt" and "send" express two things that do not conflict (stop this turn / say one more thing),
            so making them exclusive makes no sense.

            Layout: interrupt first, send after: send is the main action, and the bottom-right spot is the easiest to reach,
            which also matches where it is when not streaming (the button does not jump left and right when running starts).
            Interrupt is icon-only + Tooltip: it is a secondary action, and a full-width text button would
            compete with the main CTA for visual weight (icon-only must come with a Tooltip; see the design rules).
          */}
          <div className="ml-auto flex items-center gap-2">
            {streaming ? (
              <Tooltip>
                <Tooltip.Trigger>
                  <Button
                    variant="outline"
                    isIconOnly
                    aria-label={t("chat.interrupt")}
                    onPress={onInterrupt}
                    data-testid="chat-interrupt"
                  >
                    <Icon icon="gravity-ui:stop" className="size-4" />
                  </Button>
                </Tooltip.Trigger>
                <Tooltip.Content>{t("chat.interrupt")}</Tooltip.Content>
              </Tooltip>
            ) : null}
            <Button onPress={submit} isDisabled={!canSend} data-testid="chat-send">
              {t("chat.send")}
            </Button>
          </div>
        </div>
      </div>
    </div>
  );
}

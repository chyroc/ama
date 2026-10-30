/**
 * Session list: a permanent left column on desktop + a drawer on narrow screens; **the same list JSX is rendered only once**.
 *
 * # Why it must exist
 *
 * The chat page's left column used to be `hidden w-64 … lg:flex`: hidden on narrow screens, and nowhere else on the page was there **any
 * alternative entry** (the title bar only has the title, ViewSwitch and the "view run" link). So opening the chat page on a phone
 * only showed the current one: **you could not start a new chat or see past sessions**; a dead end, not just ugly.
 *
 * The breakpoint is still `lg:` (1024px), so **tablets and small laptops were hit too**, and there are more devices in that range than phones.
 *
 * # One list, two shells: this is deliberate, not laziness
 *
 * The two testids `chat-new` and `chat-list` **are relied on by two cases in e2e 09-chat**, and
 * Playwright's strict mode requires them to be globally unique. AppShell's drawer hit this trap: rendering the same
 * sidebar JSX in both "desktop permanent" and "narrow-screen drawer" made the testid count 2,
 * and those two cases turned permanently red with a strict mode violation; **and the error said "found 2 elements",
 * not pointing to "you rendered the same tree twice"**.
 *
 * So here `useMediaQuery` renders one of the two, instead of relying on `hidden` / `lg:flex` to let CSS hide one:
 * the copy hidden by CSS **is still in the DOM**, and there are still 2 testids. The rule is "how many are in the DOM",
 * not "how many are visible on screen".
 *
 * The cost is that the list remounts once when the viewport switches (`useMediaQuery` changed). Acceptable: real users do not
 * drag back and forth across the 1024px boundary, and e2e's two projects each have a fixed viewport.
 */

import { Button, Chip, Drawer, Tooltip, useMediaQuery, useOverlayState } from "@heroui/react";
import { Icon } from "@iconify/react";
import { useEffect } from "react";
import { AgentDeletedChip, AgentDisabledChip } from "@/components/Page";
import { RunStatusChip } from "@/components/RunStatusChip";
import { formatRelative } from "@/lib/format";
import { usePrefs, useT } from "@/prefs/PrefsContext";
import type { Run } from "@/types/domain";

/**
 * Breakpoint for the permanent desktop column.
 *
 * Matches the old left column's `lg:flex` exactly (Tailwind's `lg` is 1024px). **This value was not picked
 * at random**: the drawer must appear on every viewport where "the list used to be invisible"; if the two do not line up there is a band
 * with neither the left column nor a drawer entry, a dead zone that reports no error.
 */
const DESKTOP_QUERY = "(min-width: 1024px)";

/** One session in the list. */
function ConversationRow({
  run,
  isActive,
  onSelect,
}: {
  run: Run;
  isActive: boolean;
  onSelect: () => void;
}) {
  const { t, language } = usePrefs();
  /*
    **One row says only one state.**

    When the agent is deleted/disabled, this session is **frozen**: no messages can be sent, only history can be read. That is the fact the user
    needs right now, while `run.status` (idle / finished / running…) says "which stage the session on the Ark side
    is in". The two side by side on the same row read as a contradiction:
    "deleted" + "idle" sounds like "it is gone, but it is idle". That combination is exactly what users reported.

    ⚠️ **The shape of this trap is worth remembering: priority only works within one row.**
    The old "deleted wins" check was written in the second row's `agentDeleted ? … : agentDisabled ? …`,
    and it did make the two agent markers mutually exclusive, but the status chip in the title row is **another ternary expression**,
    and neither knows about the other. So a cross-row combination like "deleted + idle" slipped through between the two checks,
    and **reported no error at all**: each chip rendered correctly on its own.
    So the decision must be computed in one place (the frozen below), not checked separately per row.

    Disabled is treated the same as deleted: both make the session unable to send messages, and "disabled + idle" is just as awkward.
    The only difference is **what the user should do** (one goes to create new / rebind, the other switches the state back to enabled),
    and that is carried by the chip's own text; no extra status label is needed.
  */
  const frozen = run.agentDeleted || run.agentDisabled;
  return (
    <button
      type="button"
      onClick={onSelect}
      className={[
        "flex w-full flex-col items-start gap-0.5 r-row px-3 py-2 text-left cursor-[var(--cursor-interactive)]",
        // Selected uses surface + a thin shadow; unselected only gets a neutral background on hover (same as the sidebar nav)
        isActive ? "bg-surface shadow-surface" : "hover:bg-surface-secondary",
      ].join(" ")}
      data-testid={`chat-row-${run.id}`}
    >
      <span className="flex w-full items-center gap-2">
        {/*
          `min-w-0` is required for truncate to work in a flex child: a flex item's default
          `min-width:auto` makes it refuse to shrink below its content width, so the title not only gets no ellipsis,
          it also pushes the status chip on the right out of the container: in a 402px drawer that is a horizontal overflow,
          while the three `truncate` classes clearly look right.
        */}
        <span className="min-w-0 flex-1 truncate text-sm font-medium">
          {run.title || t("chat.untitled")}
        </span>
        {/*
          The status goes in the title row, not crammed together with the three chips below.

          The row below already has "source" + "agent deleted/disabled"; a fourth chip would squeeze out the time in 232px
          (w-64 minus padding). And the two rows mean different things: **this row says "what it is
          and how it is now", the next row says "where it came from"**; status and source side by side would look like the same kind of
          label. Separated, the status is right-aligned at the same position in each row, so scanning one column shows which is running and which crashed.

          The failed state is decided by `hasError`, not status: a session that errored also has status terminated,
          and rendering by status alone would show "failed" as "finished". See the RunStatusChip file header.

          ⚠️ **Not rendered at all when frozen** (see the frozen part above): "deleted" is the more final fact,
          so it replaces the status label instead of sitting beside it. With one chip fewer here, the agent marker in the second row is
          the only source of state for this row: the width only gets looser, no overflow is introduced.
        */}
        {frozen ? null : (
          <RunStatusChip status={run.status} hasError={Boolean(run.errorSummary)} />
        )}
      </span>
      <span className="flex w-full items-center gap-1.5 truncate text-xs text-muted">
        <span className="truncate">
          {run.agentName} ·{" "}
          {formatRelative(run.lastActivityAt ?? run.startedAt, language)}
        </span>
        {/* Only mark non-web sources: manual is the default case, and marking it would fill the screen with "manual", adding no information. */}
        {run.trigger !== "manual" ? (
          <Chip size="sm" variant="soft" className="shrink-0">
            {/*
              ⚠️ The testid is `row-trigger-*` and **not** `chat-row-trigger-*`.

              The latter was used before; the trap is that **it also starts with `chat-row-`**: e2e uses
              `[data-testid^="chat-row-"]` to count session rows (the session-switching case in `e2e/mobile/25`
              is written that way), so every row with a source mark was counted one extra time: tested, 7
              sessions were counted as 11 "rows". The symptom was "clicking row 2" actually clicked a chip,
              and the error said "element not clickable", not pointing to naming at all.

              The same trap happened once in the event view (the `event-` prefix counted `event-duration` as
              an event), recorded in docs/guides/frontend.md: inline helper markers must use a different prefix.
            */}
            <Chip.Label data-testid={`row-trigger-${run.trigger}`}>
              {t(`enum.trigger.${run.trigger}`)}
            </Chip.Label>
          </Chip>
        ) : null}
        {/*
          Deleted wins: when both are true, "deleted" is the more final fact.

          These two markers and the status chip in the title row are **mutually exclusive** (the frozen part above), so a row
          has at most the two chips "source + frozen reason", never crowded.
        */}
        {run.agentDeleted ? (
          <AgentDeletedChip />
        ) : run.agentDisabled ? (
          <AgentDisabledChip />
        ) : null}
      </span>
    </button>
  );
}

interface ChatSessionsProps {
  conversations: Run[];
  /** Id of the currently open session; null means none selected yet. */
  activeRunId: string | null;
  /** Clicked a session. */
  onSelect: (runId: string) => void;
  /** Clicked "new chat". */
  onNew: () => void;
}

/**
 * The list itself (`chat-new` + `chat-list`). **Desktop and drawer share this one, and it is rendered only once.**
 *
 * `onNavigate` is the closing hook for the drawer: after clicking one, the drawer must close, otherwise the list covers the session just opened,
 * and the user has to click a blank area again to see what they chose. The desktop copy passes undefined.
 */
function SessionList({
  conversations,
  activeRunId,
  onSelect,
  onNew,
  onNavigate,
}: ChatSessionsProps & { onNavigate?: () => void }) {
  const t = useT();
  return (
    <>
      <div className="flex items-center justify-between px-1">
        <p className="text-xs text-muted">{t("chat.recent")}</p>
        <Tooltip>
          <Tooltip.Trigger>
            <Button
              variant="ghost"
              isIconOnly
              aria-label={t("chat.new")}
              onPress={() => {
                onNew();
                onNavigate?.();
              }}
              data-testid="chat-new"
            >
              <Icon icon="gravity-ui:plus" className="size-4" />
            </Button>
          </Tooltip.Trigger>
          <Tooltip.Content>{t("chat.new")}</Tooltip.Content>
        </Tooltip>
      </div>

      <div className="flex min-h-0 flex-1 flex-col gap-1 overflow-y-auto" data-testid="chat-list">
        {conversations.length === 0 ? (
          <p className="px-3 py-2 text-xs text-muted">{t("chat.emptyTitle")}</p>
        ) : (
          conversations.map((r) => (
            <ConversationRow
              key={r.id}
              run={r}
              isActive={r.id === activeRunId}
              onSelect={() => {
                onSelect(r.id);
                onNavigate?.();
              }}
            />
          ))
        )}
      </div>
    </>
  );
}

/**
 * Entry button on narrow screens. Placed in the chat page title bar; opens the drawer.
 *
 * Exported separately because it and the drawer itself are in **two different places**: the button must fit in the title bar row,
 * while the drawer is a `fixed` overlay. As one component, the button would have to be pushed outside the title bar with the drawer,
 * or the other way, the overlay would be put inside a container limited by `overflow`, and the latter would clip the drawer.
 *
 * ⚠️ **It carries `lg:hidden` itself**: on desktop the left column is permanent, and another "sessions" button in the title bar would be two entries
 * to the same place (a replay of the members issue; see the MANAGE_NAV comment in AppShell).
 */
export function ChatSessionsTrigger(props: ChatSessionsProps) {
  const t = useT();
  const isDesktop = useMediaQuery(DESKTOP_QUERY);
  const state = useOverlayState();

  /*
    Reset the drawer state when crossing the desktop breakpoint.

    ⚠️ **It is not to "avoid both lists appearing at once"**: the `if (isDesktop) return null` below
    already unmounts the whole drawer tree, so on desktop it cannot still be on screen. What it really guards against is **the way back**:

      open the drawer on a narrow screen → widen/rotate across 1024px (the drawer unmounts with return null and is gone from screen)
      → narrow back again → `isOpen` is still true, and the drawer **pops up by itself** covering the chat.

    Because `useOverlayState`'s state lives on this component, `return null` only stops rendering;
    it does not reset it. An overlay appears without the user clicking anything, and they could never guess it was caused by widening the window.

    `state.close` is a stable reference wrapped by useCallback (see use-overlay-state.js),
    so putting it in the dependency array does not rerun this effect every frame.
  */
  useEffect(() => {
    if (isDesktop) state.close();
  }, [isDesktop, state.close]);

  // On desktop neither this button nor the drawer should exist (the list is in the permanent left column).
  // **It must not be rendered at the DOM level; hiding it with `lg:hidden` is not enough**: the hidden copy still
  // carries the chat-new / chat-list testids, and Playwright counts 2 and errors in strict mode.
  if (isDesktop) return null;

  return (
    <>
      <Tooltip>
        <Tooltip.Trigger>
          <Button
            variant="ghost"
            isIconOnly
            aria-label={t("chat.mobileOpenSessions")}
            onPress={state.open}
            data-testid="chat-sessions-open"
          >
            <Icon icon="gravity-ui:list-ul" className="size-4" />
          </Button>
        </Tooltip.Trigger>
        <Tooltip.Content>{t("chat.mobileOpenSessions")}</Tooltip.Content>
      </Tooltip>

      {/*
        Controlled drawer: pass isOpen/onOpenChange only to Drawer.Backdrop, **without the <Drawer> root**.
        The root `<Drawer>` renders RAC's DialogTrigger (the uncontrolled path); the official controlled example
        also starts directly from Backdrop (see the "Controlled State" section of the drawer docs).

        placement="left" is on the same side as the desktop left column: the same thing comes from the same side; do not let it
        slide in from different directions on the two viewports.
      */}
      <Drawer.Backdrop isOpen={state.isOpen} onOpenChange={state.setOpen}>
        <Drawer.Content placement="left">
          {/*
            Safe area: the drawer is `fixed inset-0`, so its top and bottom edges are the physical edges of the screen. Without safe-area
            the top would be covered by the Dynamic Island, and the last session at the bottom by the Home Indicator
            (same as AppShell's drawer, where the same thing is noted).

            `p-6` comes with drawer__dialog, and is too wide left and right for a list (on a 402px screen both sides
            eat 48px); reduced to px-3 to match the desktop left column; top and bottom use inline padding styles for the safe area.

            The left side also takes `--safe-left`: this drawer slides out from the left, and in landscape the notch is on that side,
            where the 12px of `px-3` cannot cover 59px. In portrait the variable is 0, so this only takes effect in landscape.
          */}
          <Drawer.Dialog
            className="w-72 max-w-[85vw] gap-3 px-3 py-4"
            style={{
              paddingTop: "calc(var(--safe-top) + 1rem)",
              paddingBottom: "calc(var(--safe-bottom) + 1rem)",
              paddingLeft: "calc(var(--safe-left) + 0.75rem)",
            }}
            aria-label={t("chat.mobileSessionsTitle")}
            data-testid="chat-sessions-drawer"
          >
            <SessionList {...props} onNavigate={state.close} />
          </Drawer.Dialog>
        </Drawer.Content>
      </Drawer.Backdrop>
    </>
  );
}

/**
 * Permanent left column on desktop. Not rendered at all on narrow screens (the drawer above handles the list).
 *
 * This is the `hidden w-64 … lg:flex` that used to be in ChatPage; moving it out also solved
 * "what about narrow screens". The two must be in the same file, otherwise the conditions "hide one on desktop, hide the other
 * on narrow screens" would sooner or later get out of line, and that results in **two lists in the DOM at once** (testid becomes
 * 2, strict mode red) or **none at all** (narrow screens back to a dead end).
 */
export function ChatSessionsSidebar(props: ChatSessionsProps) {
  const isDesktop = useMediaQuery(DESKTOP_QUERY);
  if (!isDesktop) return null;
  return (
    <div className="flex w-64 shrink-0 flex-col gap-3 border-r border-border px-3 py-4">
      <SessionList {...props} />
    </div>
  );
}

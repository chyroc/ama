/**
 * Shared components for page-level layout and state display.
 *
 * Why they were pulled out: the list/detail pages of the six modules share one structure (title + description + action area, loading, empty state,
 * error); without pulling it out it would be copied everywhere.
 */

import { Button, Chip, EmptyState, Skeleton, Spinner } from "@heroui/react";
import { Icon } from "@iconify/react";
import { createContext, useContext, type ReactNode } from "react";
import { FeedbackButton } from "@/components/FeedbackButton";
import type { FeedbackContext } from "@/lib/feedback";
import type { AsyncError } from "@/lib/useAsyncData";
import { useDelayedFlag } from "@/lib/useDelayedFlag";
import { useT } from "@/prefs/PrefsContext";

interface PageHeaderProps {
  title: string;
  /**
   * ReactNode instead of string: the run detail page puts a "deleted" Chip here.
   * A Chip renders as a `<span>`, which is valid inside the `<p>` below.
   */
  description?: ReactNode;
  /** Action area on the right, usually the main CTA. */
  actions?: ReactNode;
  /** Back link and the like above the title. */
  above?: ReactNode;
}

export function PageHeader({ title, description, actions, above }: PageHeaderProps) {
  return (
    <header className="flex flex-col gap-2">
      {above}
      <div className="flex items-start justify-between gap-4">
        <div className="flex flex-col gap-1 min-w-0">
          <h1 className="text-2xl font-semibold tracking-tight truncate">{title}</h1>
          {description ? <p className="text-sm text-muted">{description}</p> : null}
        </div>
        {actions ? <div className="flex items-center gap-2 shrink-0">{actions}</div> : null}
      </div>
    </header>
  );
}

/**
 * Switch for "is there a shell above". AppShell sets it to true outside its own `<main>`,
 * so all PageBody inside the shell stay as they are; **if nobody declares it, there is no shell**.
 *
 * The default is deliberately false (= I handle the safe area myself). The opposite, defaulting to true and making shell-less routes declare it each,
 * would be a zero-error design: a missing declaration shows as "content covered by the Dynamic Island", which is exactly the bug fixed this time,
 * and it is only visible on real devices. The safe default side at most adds a blank strip, which is visible and fixable.
 */
const InAppShell = createContext(false);

/** For AppShell only: declares "this subtree already has a shell; the safe area is mine". */
export function AppShellScope({ children }: { children: ReactNode }) {
  return <InAppShell.Provider value={true}>{children}</InAppShell.Provider>;
}

/**
 * Main content container. `md:pt-8` is a design rule: the page header needs breathing room and must not touch the top.
 *
 * Padding has two levels: narrow screens `px-4`, desktop `md:px-8`. The desktop level is the original value, unchanged; narrow screens are tighter because
 * `px-8` on a 402px-wide phone eats 64px on both sides (**16% of the whole viewport**), squeezing lists and tables until
 * they are unreadable. The bottom `pb-16` is the same for both: it leaves space at the end of the page, unrelated to viewport width.
 *
 * # ⚠️ This container is in two situations, and only in one of them is the safe area someone else's job
 *
 * **Inside AppShell** (the `/w/:key/*` routes): the top edge is the top bar, which already takes `--safe-top`
 * (the `calc(var(--safe-top) + 0.75rem)` in AppShell); the bottom `pb-16` (64px) is also well above the
 * Home Indicator. So inside the shell we **add nothing more**; adding would double it, showing as the page header dropping down for no reason.
 *
 * **Outside AppShell**: `/workspaces`, `/preferences` without `?from=`, and
 * the loading and 403 pages of AuthGate/WorkspaceGate are all whole pages **mounted directly under `#root`**
 * (in router.tsx they are outside AppShell). Then the top edge of this container **is the physical edge of the screen**,
 * and `pt-6` (24px) cannot cover the Dynamic Island's 59px: the title and two buttons on the "choose workspace" screen
 * fell entirely under the island and could not be tapped, which is exactly the bug fixed this time.
 *
 * The old comment here only described the first situation ("this container sits in AppShell's scroll area"),
 * which is wrong for `/workspaces`, so the first screen after login was missed.
 *
 * # Left and right must be handled **in both situations**, unlike top and bottom
 *
 * Top and bottom are split into levels because the top bar/`pb-16` inside the shell already shield this container there.
 * Left and right **have no such stand-in**: AppShell's top bar only handles its own row, and `<main>` is a pure scroll container
 * with zero padding, so the left and right edges of pages inside the shell **are still the physical edges of the screen**. In landscape the notch is on the side,
 * and the 16px of `px-4` cannot cover 59px: tested (iPhone 16 Pro landscape), the page header title and card text
 * sat right under the notch, while the top bar row was fine, because it handles it itself.
 *
 * So horizontally there are no levels; both share the same `pl/pr`.
 *
 * On non-notch devices and in portrait all four variables are `0px`, so this has zero effect on other models; and precisely because of that
 * we should not use `@media` to detect models (it cannot be accurate, and new iPhone models keep coming).
 *
 * ⚠️ Everything is written as Tailwind arbitrary values instead of an inline `style`: an inline style always beats a class,
 * and would knock out `md:px-8` / `md:pt-8` too, making desktop narrower instead. It also must not be mixed with `px-4`:
 * `px-4` generates `padding-inline` while `pl-[…]` generates `padding-left`;
 * two different properties decided by source order is a hidden trap that can change with the Tailwind version.
 */
const BODY_BASE = "mx-auto flex w-full max-w-6xl flex-col gap-6";
/** Horizontal: shared by both levels. Base 1rem / md 2rem, equal item by item to the old `px-4 md:px-8`. */
const BODY_X =
  "pl-[calc(1rem_+_var(--safe-left))] pr-[calc(1rem_+_var(--safe-right))] " +
  "md:pl-[calc(2rem_+_var(--safe-left))] md:pr-[calc(2rem_+_var(--safe-right))]";
/** Vertical, inside the shell: the top bar already took `--safe-top`, and `pb-16` (64px) is above the Home Indicator; keep the original values. */
const BODY_Y_IN_SHELL = "pt-6 pb-16 md:pt-8";
/** Vertical, no shell: the top edge is the physical edge of the screen; handle it ourselves. The base matches the level above item by item. */
const BODY_Y_STANDALONE =
  "pt-[calc(1.5rem_+_var(--safe-top))] pb-[calc(4rem_+_var(--safe-bottom))] " +
  "md:pt-[calc(2rem_+_var(--safe-top))]";

export function PageBody({ children }: { children: ReactNode }) {
  const inShell = useContext(InAppShell);
  const y = inShell ? BODY_Y_IN_SHELL : BODY_Y_STANDALONE;
  return <div className={`${BODY_BASE} ${BODY_X} ${y}`}>{children}</div>;
}

/** When label is not given, use the current language's version of "loading"; the default must not be hard-coded Chinese. */
export function LoadingBlock({ label }: { label?: string }) {
  const t = useT();
  const text = label ?? t("common.loading");
  return (
    <div
      className="flex items-center justify-center gap-3 py-16 text-muted"
      role="status"
      aria-label={text}
    >
      <Spinner />
      <span className="text-sm">{text}</span>
    </div>
  );
}

/**
 * The quiet period is 300ms: waits shorter than that show nothing at all.
 *
 * The number is not arbitrary: it is the lower edge of "the user starts to feel something is off" (the range between 0.1s and 1s in Nielsen's
 * 0.1s / 1s / 10s rule); locally and on an intranet both gates mostly finish within 100~200ms,
 * so **normally nothing flashes at all**; and when it is really slow, 0.3 seconds is far from making anyone think the page died.
 */
const STARTUP_SPINNER_DELAY_MS = 300;

/**
 * Loading state of the whole-page startup gates: **nothing is shown for the first 300ms**; only after that a centered spinner appears.
 *
 * # Why it and LoadingBlock are two different things
 *
 * `LoadingBlock` is loading **inside a page** (the user clicked something, the shell is still there, and they wait for this block of content);
 * there a line "loading XX" is useful feedback. The startup gates are different: AuthGate and WorkspaceGate are on
 * **the path every full-page refresh must go through**, and run on every refresh. The "checking login status"
 * and "opening workspace" texts they used to carry had two problems:
 *
 *   1. **When fast, they are pure noise.** The two gates run in series, so one refresh flashes two different texts in turn,
 *      often under half a second in total: too short to read, and nothing worth reading (the user is not waiting for anything specific;
 *      they just refreshed the page); all that is left is "the page twitched twice".
 *   2. **The text pins attention in the wrong place.** It describes what the system is busy with, not what the user can do.
 *
 * So here it is both delayed and text-free. **When slow there is still a spinner**, so it never degrades into a blank unresponsive screen.
 *
 * # Accessibility must not disappear along with the text
 *
 * No visible text, but screen readers must still get something: `role="status"` (aria-live=polite) + `aria-label`.
 * It uses the generic key `common.loading`, which is also LoadingBlock's default text,
 * so the screen reader experience is the same in both places, and nothing is lost from announcements by removing those two dedicated texts.
 * During the quiet period the element is **not in the DOM**, so screen reader users are not bothered by a "loading… done" either.
 *
 * # No PageBody wrapper
 *
 * The max-w / padding / safe area of `PageBody` are for pages **with content**; a centered spinner
 * needs none of it, and wrapping it would hang it too high (`py-16` from the top). Here it takes the full screen for true centering:
 * the center of the viewport is far from every screen edge, so the safe area is naturally not a problem.
 */
export function StartupSpinner() {
  const t = useT();
  const show = useDelayedFlag(STARTUP_SPINNER_DELAY_MS);
  // During the quiet period **render null**, not a transparent spinner. A placeholder element would be
  // seen by e2e assertions like "X should not be on the page", and would leave an unexplainable empty box in the DOM.
  if (!show) return null;
  return (
    <div
      className="flex min-h-dvh items-center justify-center"
      role="status"
      aria-label={t("common.loading")}
      data-testid="startup-spinner"
    >
      <Spinner />
    </div>
  );
}

export function ListSkeleton({ rows = 4 }: { rows?: number }) {
  return (
    <div className="flex flex-col gap-3" aria-hidden>
      {Array.from({ length: rows }, (_, i) => (
        <Skeleton key={i} className="h-20 w-full r-container" />
      ))}
    </div>
  );
}

interface EmptyProps {
  icon: string;
  title: string;
  description?: string;
  action?: ReactNode;
}

export function Empty({ icon, title, description, action }: EmptyProps) {
  return (
    <EmptyState className="py-16">
      <div className="flex flex-col items-center gap-4 text-center">
        <div className="flex size-12 items-center justify-center r-container bg-surface-secondary">
          <Icon icon={icon} className="size-6 text-muted" />
        </div>
        <div className="flex flex-col gap-1">
          {/* Use a real heading element: the empty state title is the main information of this area; screen readers and tests both locate by it */}
          <h2 className="text-base font-medium">{title}</h2>
          {description ? (
            <p className="text-sm text-muted max-w-md text-balance">{description}</p>
          ) : null}
        </div>
        {action}
      </div>
    </EmptyState>
  );
}

export function ErrorBlock({ message, onRetry }: { message: string; onRetry?: () => void }) {
  const t = useT();
  return (
    <Empty
      icon="gravity-ui:triangle-exclamation"
      title={t("common.loadFailed")}
      description={message}
      action={
        onRetry ? (
          <Button variant="outline" onPress={onRetry}>
            {t("common.retry")}
          </Button>
        ) : undefined
      }
    />
  );
}

/**
 * Unified display of load failures. **403 is kept apart from other errors**, because what the user should do is completely different:
 *
 * - `denied` (403): they are not in this workspace, or their role is not enough. **Deliberately no "retry"**:
 *   retrying ten thousand times is still 403, and that button would only make people click again and again.
 * - `other` (network / 5xx): offers "retry", because it really might work next time.
 *
 * Used with `useAsyncData`: `error` is passed straight through here, so pages do not classify it themselves.
 * The title is split too: 403 says "no access" instead of "failed to load": the latter makes people think it is an outage,
 * so they refresh, clear caches, look for the frontend team, while what they should do is ask the workspace admin to add them.
 *
 * # The feedback entry lives here; one change applies site-wide
 *
 * The error states of 13 pages render this component, so putting "Report this problem" here covers the whole site automatically,
 * without adding it page by page (page by page always misses some, and the missed page reports no error).
 *
 * **403 also gets the feedback entry**, which is worth explaining: it looks like "the user's own problem" (go ask an admin),
 * but 403 is exactly the most common source of **false reports** in this repo: the 12 blank screens fixed in `useAsyncData` that time were
 * caused by 403, and part of them really came from data problems like "the role string is not one of the four enums".
 * Letting users report it in one click is more useful than sending them to an admin who does not know what to do either.
 */
export function AsyncErrorBlock({
  error,
  onRetry,
  feedbackContext,
}: {
  error: AsyncError;
  onRetry?: () => void;
  /**
   * Context the page can provide (workspace id / agent id); **reported only when the user actively gives feedback**.
   *
   * May be omitted: without it two fewer fields are collected, and feedback can still be submitted. It is deliberately not required:
   * changing all 13 call sites would bring 13 changes and 13 chances to miss one, for two optional fields.
   */
  feedbackContext?: FeedbackContext;
}) {
  const t = useT();
  const denied = error.kind === "denied";
  return (
    <div data-testid={denied ? "load-denied" : "load-error"} role="alert">
      <Empty
        icon={denied ? "gravity-ui:lock" : "gravity-ui:triangle-exclamation"}
        title={denied ? t("common.accessDenied") : t("common.loadFailed")}
        description={error.message}
        action={
          // Retry and feedback are stacked: feedback is a secondary action, and side by side the two buttons would look equally important,
          // while "retry" is the first thing the user should do when it can be retried.
          <div className="flex flex-col items-center gap-3">
            {denied || !onRetry ? null : (
              <Button variant="outline" onPress={onRetry} data-testid="load-retry">
                {t("common.retry")}
              </Button>
            )}
            <FeedbackButton error={error} context={feedbackContext} />
          </div>
        }
      />
    </div>
  );
}

/**
 * Inline display of a failed write: one line of red text + a level-two feedback entry.
 *
 * # Why not reuse AsyncErrorBlock
 *
 * That one is a **whole-block** error state (`EmptyState`, filling the content area), used for "this page's data did not load,
 * and there is nothing else to see". A failed write is completely different: the page is fine, only the user's last action did not work;
 * slapping a whole-block error state on it would cover the form they already filled in and the chat they already typed.
 * So inline it stays one line of red text as before, with only the feedback entry added.
 *
 * # What it fixes
 *
 * After the feedback feature launched, the table was empty, and half the reason was here: failed writes used to render each page's own
 * `<p class="text-danger">{e.message}</p>`, which had **no feedback entry at all**,
 * so users "could not see a dialog". The other half was in `useWriteError` (level one recorded nothing at all).
 * Both must be fixed together to help: fixing only level one, users still cannot actively report;
 * fixing only level two, the database still has no automatic records.
 *
 * Used with `useWriteError`: `error` is passed straight in. `compact` is for tight places like the chat page
 * (one line above the input box); it shrinks the font to xs and lays the feedback entry out in a row.
 */
export function InlineError({
  error,
  compact,
  feedbackContext,
  prefix,
  testId,
}: {
  error: AsyncError;
  /** Compact layout: xs font + row layout. Used above the chat page input box. */
  compact?: boolean;
  feedbackContext?: FeedbackContext;
  /** A qualifier before the error, like "send failed". When not given, only message is shown. */
  prefix?: string;
  /** Keeps each page's existing testid, so existing e2e assertions do not break. */
  testId?: string;
}) {
  return (
    <div
      className={compact ? "flex flex-wrap items-center gap-x-3 gap-y-1" : "flex flex-col gap-2"}
      role="alert"
      data-testid={testId}
    >
      <p className={compact ? "text-xs text-danger" : "text-sm text-danger"}>
        {prefix ? `${prefix}：` : ""}
        {error.message}
      </p>
      {/* The feedback entry appears together with the error. **This is the only level-two entry for failed writes**:
          without it, when users hit "failed to send message" there is nothing clickable in the UI. */}
      <FeedbackButton error={error} context={feedbackContext} compact />
    </div>
  );
}

/** A definition list of "label + value" pairs, used all over the detail pages. */
export function DescList({
  items,
}: {
  items: { label: string; value: ReactNode }[];
}) {
  return (
    <dl className="grid gap-x-8 gap-y-4 sm:grid-cols-2">
      {items.map((it) => (
        <div key={it.label} className="flex flex-col gap-1 min-w-0">
          <dt className="text-xs text-muted">{it.label}</dt>
          <dd className="text-sm break-words">{it.value}</dd>
        </div>
      ))}
    </dl>
  );
}

/** Section heading. Used to split long forms/detail pages into blocks. */
export function SectionTitle({
  title,
  description,
  actions,
}: {
  title: string;
  description?: string;
  actions?: ReactNode;
}) {
  return (
    <div className="flex items-start justify-between gap-4">
      <div className="flex flex-col gap-1">
        <h2 className="text-base font-medium">{title}</h2>
        {description ? <p className="text-sm text-muted">{description}</p> : null}
      </div>
      {actions}
    </div>
  );
}

/**
 * Inline warning bar.
 *
 * The site does not use HeroUI's Alert (OSS has none); this shape was originally a private
 * `Issue` in FeishuReadiness, hand-copied in six places. New warning bars all use this; **those six existing ones are still to be unified**;
 * they were not changed along the way this time because it would be pure churn across six files.
 *
 * `tone` has `info` for the four states of "authorization management": `ok` and `missing` are **not**
 * "something is wrong"; warning yellow would make users think something is broken and go to an admin to put out a fire,
 * while it is really just "all fine" or "not configured yet". A neutral color matches the meaning of those two states.
 *
 * # `action` was added later; the line "deliberately only five props" no longer holds
 *
 * The original argument was "`action` (the button to nudge approval) is specific to FeishuReadiness; putting it in a shared component only makes
 * callers face one more hole they do not use". A second caller overturned it: when "this workspace has no Ark
 * API Key configured", the chat page needs an entry in the warning bar that goes straight to workspace settings (otherwise it can only say "go to workspace settings"
 * and let users find it themselves, which is exactly the missing guidance fixed this time; see chat.noArkTitle in messages.ts).
 *
 * It is **optional**, so the six existing call sites did not change a word. Adding a hole instead of letting the second caller copy the shape again,
 * because "hand-copied six times" is exactly why this component exists.
 *
 * ⚠️ The button is **on its own line, below the text**, not on the same row as the title: on the same row "explanation" and "action" are squeezed into
 * a lump, and on narrow screens the button would also push the title into two lines.
 */
export function Notice({
  tone = "warning",
  icon,
  title,
  hint,
  action,
  "data-testid": testId,
}: {
  tone?: "info" | "warning" | "danger";
  icon: string;
  title: string;
  hint?: string;
  /** Optional next-step entry (button / link). Rendered on its own line below the text. */
  action?: ReactNode;
  "data-testid"?: string;
}) {
  const color =
    tone === "danger" ? "text-danger" : tone === "info" ? "text-foreground" : "text-warning";
  const bg =
    tone === "danger" ? "bg-danger/5" : tone === "info" ? "bg-surface-secondary" : "bg-warning/5";
  // The info icon uses muted: in a neutral tone, making the icon as heavy as the title would look like emphasis.
  const iconColor = tone === "info" ? "text-muted" : color;
  return (
    <div className={`flex items-start gap-2 r-inset ${bg} px-3 py-2.5`} data-testid={testId}>
      <Icon icon={icon} className={`mt-0.5 size-4 shrink-0 ${iconColor}`} />
      <div className="flex min-w-0 flex-1 flex-col gap-1.5">
        <p className={`text-sm ${color}`}>{title}</p>
        {hint ? <p className="text-xs text-muted">{hint}</p> : null}
        {action ? <div className="mt-1 flex flex-wrap items-center gap-2">{action}</div> : null}
      </div>
    </div>
  );
}

/**
 * The "deleted" marker next to an agent name.
 *
 * Agents are hard-deleted, but `agentName` on a Run is a snapshot taken when the session was created, still there after deletion:
 * without this marker the page looks perfectly normal, and you cannot tell that this record's agent is gone.
 *
 * The color is always `warning`, not varying by page: `danger` is too alarming (RunStatusChip keeps it for real failures),
 * and a neutral color next to the name is too invisible. The frozen warning bar on the chat page is also warning: one concept, one color.
 */
export function AgentDeletedChip() {
  const t = useT();
  return (
    <Chip size="sm" color="warning" variant="soft">
      <Chip.Label data-testid="agent-deleted-chip">{t("agent.deleted")}</Chip.Label>
    </Chip>
  );
}

/**
 * The "disabled" marker next to an agent name.
 *
 * **Kept as a separate component from "deleted" instead of sharing one "frozen" marker**: the two need different user actions
 * (deleted can only be recreated; disabled just needs the state switched back), and this marker is often the only thing users notice.
 * Mixing them leads directly to "it was only disabled, but the UI says deleted": users would go create a new agent,
 * while the old one's skills and Feishu bindings are all still there.
 *
 * Also warning: one concept, one color; see the comment on AgentDeletedChip.
 */
export function AgentDisabledChip() {
  const t = useT();
  return (
    <Chip size="sm" color="warning" variant="soft">
      <Chip.Label data-testid="agent-disabled-chip">
        {t("enum.agentStatus.disabled")}
      </Chip.Label>
    </Chip>
  );
}

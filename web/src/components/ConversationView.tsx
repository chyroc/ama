/**
 * Chat view: events shown collapsed by turn.
 *
 * The form follows Codex: **after a turn ends only the input and output stay, and the process in between collapses into one summary line**;
 * while a turn is still running the process is laid out, and with many tools only the latest few steps stay, the earlier ones go into an expandable group.
 * RunTimeline is the other way of showing the same events (that one is the debugging view; see DESIGN.md 3.6.3).
 *
 * **It deliberately does not include the ChatConversation scroll shell.** The run detail page scrolls with normal page flow; nesting ChatConversation's
 * `overflow-y:auto` container inside PageBody would give nested scroll areas (the outer scrolls a bit,
 * the inner scrolls again, and the wheel behaves chaotically). So this only outputs content, and the scroll shell is decided by the caller:
 * the chat page wraps it with ChatStream, and the run detail page uses it directly.
 *
 * ⚠ Likewise the ready-made ChatTool / CodeBlock components are **not used**: they pull in shiki and marked respectively;
 * for the reason see the top of RunTimeline.tsx and docs/guides/heroui.md. Tool results are still <pre>.
 */

import { Fragment, useEffect, useState } from "react";
import { Chip, Disclosure } from "@heroui/react";
import { ChainOfThought, ChatLoader, ChatMessage } from "@/components/ui/Chat";
import { Icon } from "@iconify/react";
import { MarkdownMessage } from "@/components/MarkdownMessage";
import { parseHarnessBlock } from "@/lib/harnessBlock";
import {
  groupTurns,
  processStats,
  type ProcessStats,
  type TimelineItem,
  type Turn,
  type TurnSegment,
} from "@/lib/timeline";
import { formatDuration } from "@/lib/format";
import type { RunEventType } from "@/types/events";
import { useT, type TFunc } from "@/prefs/PrefsContext";

/**
 * At most how many steps of the in-progress part are shown in full.
 *
 * The excess goes into "N earlier steps", i.e. "dynamically fold the old, dynamically show the newest". 3 because
 * one tool call together with thinking/thread is often two or three entries, so 3 steps just cover "what it is doing now"
 * without pushing the input out of the viewport.
 */
const LIVE_WINDOW = 3;

/**
 * Lifecycle churn that is **never shown** in the chat view.
 *
 * Under self_hosted Ark only does model inference, and tools are picked up and run by amad's worker, so **for every tool call**
 * the session and thread flip back and forth between running ⇄ idle once:
 *
 *   agent.tool_use → status_idle + thread_status_idle (hand back control, wait for tool_result)
 *   ← the worker returns user.tool_result
 *   → status_running + thread_status_running (Ark starts again and continues inference)
 *
 * In other words, calling tools 5 times in a turn gives 5 sets of "session started running / thread started running / thread idle".
 * **That is self_hosted's heartbeat, not chat content**: what users see is their own question washed away by a string of status announcements,
 * none of which tells them anything (that the tool is running is already said by the tool row's own "running";
 * saying the same information twice should be removed).
 *
 * They are still fully kept in the **event view** (`?view=events` is the debugging view, with all 33 kinds);
 * what is removed here is only noise in the "chat" way of reading, not data. Same treatment as `span.*`.
 *
 * ⚠ **It must be decided by type, not by category.** The category of `user.interrupt` is also
 * `status`, but it is **an action the user really took**; removing it would make "I interrupted this turn" vanish out of nowhere.
 * Likewise `*_rescheduled` (recovered from a transient error) and `*_terminated` (really ended) must stay:
 * they are turning points, not heartbeats.
 */
const LIFECYCLE_CHURN = new Set<RunEventType>([
  "session.status_running",
  "session.thread_status_running",
  // Thread-level idle is always noise: the session-level end_turn is the end-of-turn marker (eaten by groupTurns,
  // never entering a segment), while the thread-level one is eaten by nobody, and it used to hang at the end of every turn in the form
  // "thread idle · ended normally". The requires_action kind is already merged into the tool row in buildTimeline.
  "session.thread_status_idle",
  // The only session.status_idle left in segments is the "requires_action whose event_ids cannot be attached" kind
  // (see lib/timeline.ts). The event view still shows it; the chat view does not need it.
  "session.status_idle",
]);

/**
 * "Informative" entries in a process segment.
 *
 * In finished turns, if a segment only has status/thread markers left (`status_running` + `thread_created`
 * and the like), collapsing it would add a summary line with no content, so the whole segment is not rendered. The check is here instead of inline
 * because "what counts as substance" is tied to the noise-reduction policy, and changing one means changing both.
 */
function isSubstantive(item: TimelineItem): boolean {
  switch (item.category) {
    case "tool":
    case "error":
    case "outcome":
    case "conversation": // the only conversation entries that reach a process segment are system.message
      return true;
    case "thinking":
      // Thinking with a body is real content: users explicitly want to look back at it, so a finished turn must not
      // drop the whole segment just because "only thinking is left". Thinking with an empty body still counts as noise.
      return Boolean(item.body);
    case "thread":
      // Inter-thread communication and context compaction have content; pure status markers do not.
      return Boolean(item.body);
    case "status":
      /*
       * status as a whole is noise (self_hosted's running/idle heartbeat), **but `user.interrupt`
       * is an action the user really took**, and must not be dropped with the whole segment just because it also falls under status.
       *
       * This is the other half of the same discipline as LIFECYCLE_CHURN: that one lists types to decide
       * "what is not shown", and this one decides by type "which segment is worth keeping". LIFECYCLE_CHURN alone
       * is not enough, because it filters **within** a segment, while this function decides whether a whole segment is rendered:
       * if a turn has only "user interrupted" (the user pressed stop before the agent said anything),
       * the in-segment filter kept it, but it was dropped here with the whole segment.
       *
       * Symptom: that turn **shows nothing at all**; users see their question was sent, then blank,
       * with no way to tell "I interrupted it myself". Zero errors. It was not caught earlier because e2e always
       * clicked interrupt after thinking/tools had already appeared, so the segment had other real content holding it up.
       */
      return item.type === "user.interrupt";
    default:
      return false;
  }
}

/**
 * Context injected by the platform: one line collapsed by default; click to see what the platform put into this turn.
 *
 * # Why show it to users
 *
 * For messages from Feishu, the platform appends a `<harness:lark>` block after the user's text (chat_id /
 * thread_id / commands for fetching history). It is **for the model**, but users have the right to know "besides what I said,
 * what else the platform told the agent for me", especially when debugging "how did the agent know this" or
 * "why did it go through the group history"; this is the only clue.
 *
 * # ⚠️ Do not lay the block out as is
 *
 * The first version did exactly that (a `<pre>` printing the full text), and it was rejected after a screenshot on a real device:
 *
 *   - **Markers meant for the model show up on screen**: `<harness:lark>`, `**data**`, ```bash fences,
 *     which read as if the page is broken;
 *   - **only 5 lines of ids carry real information**, buried under twenty or thirty lines of "how to fetch history / do not retry on failure /
 *     fetched content is untrusted", and that part is **the same for every message**;
 *   - so the second time users stopped expanding it, and this collapsible area was wasted.
 *
 * Now it is split into two layers: **facts** (each id, rendered structured as `key → value`, values copyable on their own)
 * and **guidance** (static boilerplate, folded once more). Parsing is in `lib/harnessBlock.ts`.
 *
 * # Two deliberate choices
 *
 * - **The label says "injected by platform", not "system prompt"**: the latter would make people think it is the agent's persona
 *   (that is the part users write themselves on the Agent detail page); the two are completely different things.
 * - **If no facts can be parsed, show the full text as is**: the block format is built by the backend, and with each side changed once it drifts,
 *   and if the drift showed as "nothing after expanding", that would be much worse than ugly.
 */
function InjectedContext({ blocks }: { blocks?: string[] }) {
  const t = useT();
  if (!blocks?.length) return null;

  return (
    <Disclosure>
      <Disclosure.Heading>
        <Disclosure.Trigger
          className="flex items-center gap-2 r-row px-2 py-1.5 text-left text-xs text-muted cursor-[var(--cursor-interactive)] hover:bg-surface-secondary hover:text-foreground"
          data-testid="injected-context-toggle"
        >
          <Disclosure.Indicator>
            <Icon icon="gravity-ui:chevron-right" className="size-3" />
          </Disclosure.Indicator>
          <Icon icon="gravity-ui:gear" className="size-3.5" />
          <span className="font-medium">{t("tl.injectedContext")}</span>
        </Disclosure.Trigger>
      </Disclosure.Heading>
      <Disclosure.Content>
        <Disclosure.Body className="px-2 pb-2">
          <div
            className="flex max-h-96 flex-col gap-3 overflow-auto border-l-2 border-default pl-3"
            data-testid="injected-context-body"
          >
            {blocks.map((block, i) => (
              <InjectedBlock key={i} block={block} />
            ))}
          </div>
        </Disclosure.Body>
      </Disclosure.Content>
    </Disclosure>
  );
}

/** A single injected block: facts as a definition list, guidance for the model folded once more. */
function InjectedBlock({ block }: { block: string }) {
  const t = useT();
  const parsed = parseHarnessBlock(block);

  // No facts parsed → show as is; do not hide the content (see the comment on InjectedContext).
  if (parsed.facts.length === 0) {
    return (
      <pre className="whitespace-pre-wrap break-words font-mono text-xs leading-relaxed text-muted">
        {parsed.raw}
      </pre>
    );
  }

  return (
    <div className="flex flex-col gap-2">
      {/*
        Source label. **It must not be just a bare `lark`**: that reads like a line of
        ownerless text floating above (confirmed by a screenshot on a real device). Add a line saying what it is; the Chip carries only the meaning "source".
      */}
      {parsed.kind ? (
        <p className="text-xs text-muted">
          {t("tl.injectedFrom")}
          <Chip size="sm" variant="soft" className="ml-1.5 align-middle">
            {parsed.kind}
          </Chip>
        </p>
      ) : null}

      {/*
        <dl> instead of joining strings: this is "field → value" by nature, and with two columns
        values can be selected and copied on their own: nine times out of ten users expand this block to grab some id to look up.
        Values use tabular-nums + font-mono: a hex string only lines up in monospace.
      */}
      <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs">
        {parsed.facts.map((f) => (
          <Fragment key={f.key}>
            <dt className="font-mono text-muted">{f.key}</dt>
            <dd className="min-w-0 break-all font-mono text-foreground tabular-nums">
              {f.value}
              {f.note ? (
                // The note is **not part of the value**: calling an API with `om_xxx (this very message)` will not work,
                // and users copy what they see on screen. So it is pushed to a secondary color, and not monospace.
                <span className="ml-1.5 font-sans text-muted">{f.note}</span>
              ) : null}
            </dd>
          </Fragment>
        ))}
      </dl>

      {parsed.guidance ? (
        <Disclosure>
          <Disclosure.Heading>
            <Disclosure.Trigger
              className="flex items-center gap-1.5 self-start text-xs text-muted cursor-[var(--cursor-interactive)] hover:text-foreground"
              data-testid="injected-guidance-toggle"
            >
              <Disclosure.Indicator>
                <Icon icon="gravity-ui:chevron-right" className="size-3" />
              </Disclosure.Indicator>
              {t("tl.injectedGuidance")}
            </Disclosure.Trigger>
          </Disclosure.Heading>
          <Disclosure.Content>
            <Disclosure.Body>
              {/*
                The guidance contains markdown (`**emphasis**`, ```bash fences) and command lines, handed to
                MarkdownMessage to render: it already isolates the Markdown renderer into its own chunk,
                and code blocks get proper monospace + horizontal scrolling, easier to read than <pre> printing raw text.

                ⚠️ The testid is on the outer div: MarkdownMessage only takes the three props
                `children/streaming/className`, and passing data-testid would be
                **silently dropped** (e2e then cannot find the element, and the error does not point to the real cause).
              */}
              <div data-testid="injected-guidance-body">
                <MarkdownMessage className="text-xs leading-relaxed text-muted">
                  {parsed.guidance}
                </MarkdownMessage>
              </div>
            </Disclosure.Body>
          </Disclosure.Content>
        </Disclosure>
      ) : null}
    </div>
  );
}


/**
 * Thinking process: collapsed into one line "thought deeply"; click to see the full chain of thought.
 *
 * Kept separate from ToolRow instead of reusing it: the tool row shows confirmation state, pending, and two pre blocks for input/result,
 * while thinking has only one body, and the body is **natural language**, not code: rendering thinking with ToolRow's monospace pre
 * would be hard to read (a monospace font + a non-wrapping <pre> is especially bad for long Chinese paragraphs).
 *
 * **The collapsed state deliberately shows no body summary** (the tool row does; this one does not). Two reasons:
 * first, when the thinking body is short (tested: often around 44 characters) the summary is the whole text, and expanding only writes the same sentence again,
 * wasting the collapse; second, thinking is "process", not "conclusion", and a summary laid out there would compete with the real reply
 * for attention: when users scan this screen they want to see the answer first. To see it, click to expand; that is an explicit action.
 */
export function ThinkingRow({ item }: { item: TimelineItem }) {
  // Without a body (tested: 3 of 72) it degrades into a thin line, without an expandable empty shell.
  if (!item.body) return <MetaRow item={item} />;

  return (
    <Disclosure>
      <Disclosure.Heading>
        <Disclosure.Trigger
          className="flex items-center gap-2 r-row px-2 py-1.5 text-left text-xs text-muted cursor-[var(--cursor-interactive)] hover:bg-surface-secondary hover:text-foreground"
          data-testid="chat-thinking-row"
        >
          <Disclosure.Indicator>
            <Icon icon="gravity-ui:chevron-right" className="size-3" />
          </Disclosure.Indicator>
          <Icon icon={item.icon} className="size-3.5" />
          <span className="font-medium">{item.title}</span>
        </Disclosure.Trigger>
      </Disclosure.Heading>
      <Disclosure.Content>
        <Disclosure.Body className="px-2 pb-2">
          {/*
            The body goes through Markdown: in the thinking process the model also uses lists, code blocks and bold (tested: present in the production
            database), and as plain text `**` would show with the asterisks. Reuse MarkdownMessage instead of
            rendering ourselves: it already isolates the weight of the Markdown renderer into its own chunk (see the top of that file).

            border-l gives a vertical line + text-muted lowers the contrast: visually separates "the model's inner activity"
            from the formal reply, so users do not mistake thinking for the answer.
          */}
          <div className="max-h-96 overflow-auto border-l-2 border-default pl-3">
            <MarkdownMessage className="text-xs leading-relaxed text-muted">
              {item.body}
            </MarkdownMessage>
          </div>
        </Disclosure.Body>
      </Disclosure.Content>
    </Disclosure>
  );
}

/** Tool call: collapsed into one line; click to see input and result. */
export function ToolRow({ item }: { item: TimelineItem }) {
  const t = useT();
  const failed = item.result?.isError === true;
  /**
   * Really waiting for the user to click. **This is the only check** (`permission === "ask"` and no confirmation).
   *
   * Do not replace it with `stop_reason === "requires_action"`: under self_hosted every tool call
   * produces that idle, meaning "Ark hands back control and waits for our worker to return a result", with nobody waiting for the user.
   * Mixing them would prompt "waiting for your confirmation" on every tool run, with nothing for the user to click.
   */
  const awaitingUser = item.permission === "ask" && !item.confirmation;

  return (
    <Disclosure>
      <Disclosure.Heading>
        <Disclosure.Trigger
          className="flex w-full items-center gap-2 r-row px-2 py-1.5 text-left text-xs text-muted cursor-[var(--cursor-interactive)] hover:bg-surface-secondary hover:text-foreground"
          data-testid="chat-tool-row"
        >
          <Disclosure.Indicator>
            <Icon icon="gravity-ui:chevron-right" className="size-3" />
          </Disclosure.Indicator>
          <Icon icon={item.icon} className={failed ? "size-3.5 text-danger" : "size-3.5"} />
          <span className={failed ? "font-medium text-danger" : "font-medium"}>{item.title}</span>

          {/*
            The confirmation state must be visible in the collapsed state, and stand out more than "running":
            "waiting for your confirmation" is the reason the run **really stops there** (the whole of run_5 is just this one thing),
            and if the user does not click it, it never moves. Hiding it in the expanded area means users never know they need to click.
          */}
          {awaitingUser ? (
            <Chip size="sm" color="warning" variant="primary" data-testid="tool-needs-confirm">
              <Chip.Label>{t("timeline.needsConfirm")}</Chip.Label>
            </Chip>
          ) : null}
          {item.confirmation ? (
            <Chip
              size="sm"
              color={item.confirmation.result === "allow" ? "success" : "danger"}
              variant="soft"
            >
              <Chip.Label>
                {item.confirmation.result === "allow"
                  ? t("timeline.approved")
                  : t("timeline.denied")}
              </Chip.Label>
            </Chip>
          ) : null}
          {/*
            Running: the tool runs in our own container. This is an **in-progress state**, so it uses a neutral
            default Chip + spinner, not warning: that would clash with "waiting for your confirmation" above,
            while the two mean exactly opposite things (one is moving, one is stuck).
          */}
          {item.executing ? (
            <Chip size="sm" variant="soft" data-testid="tool-executing">
              <Chip.Label>{t("timeline.executing")}</Chip.Label>
            </Chip>
          ) : null}

          <span className="min-w-0 flex-1 truncate">{item.summary}</span>
          {/*
            While still running, give an "it is moving" signal; shrink-0 keeps it from squeezing out the icon in front.
            **Not shown while waiting for user confirmation**: nothing is running then, and a spinner would make people think waiting a bit more will do.
          */}
          {item.executing ? (
            <ChatLoader.Dots size="sm" label={t("timeline.executing")} className="shrink-0" />
          ) : null}
        </Disclosure.Trigger>
      </Disclosure.Heading>
      <Disclosure.Content>
        <Disclosure.Body className="flex flex-col gap-2 px-2 pb-2">
          {/* While waiting for confirmation, spell out "what you need to do": the Chip in the collapsed state is only enough to hint it exists. */}
          {awaitingUser ? (
            <p className="flex items-center gap-1.5 text-xs text-warning">
              <Icon icon="gravity-ui:triangle-exclamation" className="size-3.5 shrink-0" />
              {t("timeline.awaitingYou")}
            </p>
          ) : null}
          {item.body ? (
            <pre className="max-h-48 overflow-auto whitespace-pre-wrap r-inset bg-surface-secondary p-3 text-xs leading-relaxed">
              {item.body}
            </pre>
          ) : null}
          {item.result ? (
            <pre
              className={[
                "max-h-64 overflow-auto whitespace-pre-wrap r-inset p-3 text-xs leading-relaxed",
                item.result.isError ? "bg-danger/5 text-danger" : "bg-surface-secondary",
              ].join(" ")}
            >
              {item.result.text || t("timeline.emptyResult")}
            </pre>
          ) : null}
        </Disclosure.Body>
      </Disclosure.Content>
    </Disclosure>
  );
}

/** Structural entries like errors and status: one thin line, taking no visual weight from the chat. */
export function MetaRow({ item }: { item: TimelineItem }) {
  const danger = item.tone === "danger";
  return (
    <div
      className={[
        "flex items-center gap-2 px-2 text-xs",
        danger ? "text-danger" : "text-muted",
      ].join(" ")}
      data-testid={danger ? "chat-error-row" : "chat-meta-row"}
    >
      <Icon icon={item.icon} className="size-3.5 shrink-0" />
      <span className="font-medium">{item.title}</span>
      {item.summary ? <span className="min-w-0 truncate">{item.summary}</span> : null}
    </div>
  );
}

function StepRow({ item }: { item: TimelineItem }) {
  if (item.category === "tool") return <ToolRow item={item} />;
  // thinking goes through its own row: MetaRow only draws icon + title + summary, **with no body area**;
  // putting thinking on it would throw away the chain of thought (exactly why "the page only shows the final reply").
  if (item.category === "thinking") return <ThinkingRow item={item} />;
  return <MetaRow item={item} />;
}

/**
 * The summary line when collapsed: `12 steps · 7 tool calls · 38.2s`.
 *
 * With failures, a red "N failed" part is inserted: in the collapsed state this is the only place to see "something went wrong in this turn";
 * not marking it would hide the failure.
 */
function ProcessSummary({ stats, t }: { stats: ProcessStats; t: TFunc }) {
  const parts = [t("turn.steps", { count: stats.steps })];
  if (stats.toolCalls) parts.push(t("turn.toolCalls", { count: stats.toolCalls }));
  // When processedAt is all missing the duration cannot be computed; drop this part instead of showing a placeholder.
  if (stats.durationMs != null) parts.push(formatDuration(stats.durationMs));

  return (
    <span className="flex items-center gap-1.5">
      <span>{parts.join(" · ")}</span>
      {stats.toolErrors ? (
        <span className="text-danger">
          · {t("turn.toolErrors", { count: stats.toolErrors })}
        </span>
      ) : null}
    </span>
  );
}

/**
 * One process segment.
 *
 * Three states:
 * - `live` (this turn is still running, and it is the last segment) → expanded + shimmer, showing only the last LIVE_WINDOW steps in full;
 * - this turn has no output at all → expanded. **When there is no answer, the process is the answer**:
 *   terminated by failure (run_3), still running (run_4), stuck waiting for confirmation (run_5) all have this shape,
 *   and collapsing them would leave the page with only the user's question;
 * - otherwise → collapsed, keeping only the summary line.
 */
function ProcessBlock({
  items,
  live,
  expanded,
}: {
  items: TimelineItem[];
  live: boolean;
  expanded: boolean;
}) {
  const t = useT();
  const stats = processStats(items);

  /**
   * The expanded state must be **controlled**.
   *
   * `defaultExpanded` hits a trap with no error: it is an uncontrolled prop, read only once on mount.
   * The real course of a chat turn is "laid out with live=true while running → should collapse with live=false after it ends",
   * while the component mounted long ago, so changing `defaultExpanded` **has no effect at all**:
   * the symptom is the answer came out, the turn ended, and the pile of tools in between stays there without folding,
   * which kills exactly the requirement "only input and output stay after a turn ends".
   *
   * So we keep the state ourselves and sync it when shouldExpand flips (even if the user expanded it by hand, this flip wins:
   * a turn that ended should fold up, which is exactly Codex's behavior).
   */
  const shouldExpand = live || expanded;
  const [isExpanded, setIsExpanded] = useState(shouldExpand);
  useEffect(() => {
    setIsExpanded(shouldExpand);
  }, [shouldExpand]);

  const windowed = live && items.length > LIVE_WINDOW;
  const earlier = windowed ? items.slice(0, items.length - LIVE_WINDOW) : [];
  const latest = windowed ? items.slice(items.length - LIVE_WINDOW) : items;

  return (
    <ChainOfThought
      isExpanded={isExpanded}
      onExpandedChange={setIsExpanded}
      isStreaming={live}
      data-testid="turn-process"
    >
      <ChainOfThought.Trigger data-testid="turn-process-trigger">
        {live ? t("turn.running") : <ProcessSummary stats={stats} t={t} />}
      </ChainOfThought.Trigger>
      <ChainOfThought.Content>
        <ChainOfThought.Steps>
          {/* The earlier steps are folded into a nested expandable group: a nested Disclosure is safe:
              RAC's DisclosurePanel sets ButtonContext to null before passing it to the subtree,
              so the inner trigger does not toggle the outer one by mistake. See docs/guides/heroui.md. */}
          {earlier.length ? (
            <ChainOfThought.Step>
              <Disclosure>
                <Disclosure.Heading>
                  <Disclosure.Trigger
                    className="flex items-center gap-1.5 text-xs text-muted cursor-[var(--cursor-interactive)] hover:text-foreground"
                    data-testid="turn-earlier"
                  >
                    <Disclosure.Indicator>
                      <Icon icon="gravity-ui:chevron-right" className="size-3" />
                    </Disclosure.Indicator>
                    {t("turn.earlierSteps", { count: earlier.length })}
                  </Disclosure.Trigger>
                </Disclosure.Heading>
                <Disclosure.Content>
                  <Disclosure.Body className="flex flex-col gap-2 pt-2">
                    {earlier.map((it) => (
                      <StepRow key={it.key} item={it} />
                    ))}
                  </Disclosure.Body>
                </Disclosure.Content>
              </Disclosure>
            </ChainOfThought.Step>
          ) : null}

          {latest.map((it) => (
            <ChainOfThought.Step key={it.key}>
              <StepRow item={it} />
            </ChainOfThought.Step>
          ))}
        </ChainOfThought.Steps>
      </ChainOfThought.Content>
    </ChainOfThought>
  );
}

function TurnBlock({
  turn,
  isLast,
  agentName,
}: {
  turn: Turn;
  isLast: boolean;
  agentName: string;
}) {
  // The two noise filters of the chat view: span.* (see the event view, DESIGN.md 3.6.3) and self_hosted's
  // lifecycle churn (every tool call has a set of running/idle; see LIFECYCLE_CHURN).
  // Both only affect the "chat" way of reading; the event view still gives all 33 kinds.
  const segments: TurnSegment[] = turn.segments
    .map((seg) =>
      seg.kind === "process"
        ? {
            kind: "process" as const,
            items: seg.items.filter(
              (i) => i.category !== "trace" && !LIFECYCLE_CHURN.has(i.type),
            ),
          }
        : seg,
    )
    .filter((seg) => seg.kind === "output" || seg.items.length > 0);

  const hasOutput = segments.some((seg) => seg.kind === "output");
  const lastProcessIdx = segments.reduce(
    (acc, seg, i) => (seg.kind === "process" ? i : acc),
    -1,
  );

  return (
    <>
      {turn.input ? (
        <>
          <ChatMessage.User data-testid="chat-user-message">
            <ChatMessage.Bubble>
              {/*
                ⚠️ **Neither whitespace-pre-wrap nor max-h may be dropped.**

                The user's text is **plain text** (not rendered through Markdown like agent replies), and
                `ChatMessage.Content` only sets font size/line height/color, **with no white-space**
                (see components/ui/Chat.tsx). Without it all line breaks are swallowed:
                an expanded Feishu merged forward is multi-line text like "1. …\n2. …", which would mush into one paragraph.

                max-h guards the other end: a merged forward has up to 20 messages and 4000 characters (maxExpandedTotalLen in
                shared/infra/lark), so one message could push the whole page down, and the agent replies below
                would be out of sight. The max-h-96 here is the same value as the InjectedContext block next to it.

                break-words keeps long URLs / long hashes from breaking the bubble.
              */}
              <ChatMessage.Content className="max-h-96 overflow-auto break-words whitespace-pre-wrap">
                {turn.input.body || turn.input.summary}
              </ChatMessage.Content>
            </ChatMessage.Bubble>
          </ChatMessage.User>
          <InjectedContext blocks={turn.input.injected} />
        </>
      ) : null}

      {segments.map((seg, i) => {
        if (seg.kind === "output") {
          // Agent replies have no bubble: a long reply reads more smoothly aligned left than stuffed into a bubble.
          return (
            <ChatMessage.Assistant key={seg.item.key} data-testid="chat-agent-message">
              <ChatMessage.Avatar alt={agentName} fallback={agentName.slice(0, 1) || "A"} />
              <ChatMessage.Body>
                {/*
                  The agent's reply is Markdown, rendered by the Markdown renderer (headings/code blocks/tables/lists).
                  **Do not** add whitespace-pre-wrap here: the Markdown renderer has already laid it out as block-level elements,
                  and keeping the raw line breaks too would double the blank lines between paragraphs.
                */}
                <ChatMessage.Content>
                  <MarkdownMessage>{seg.item.body || seg.item.summary}</MarkdownMessage>
                </ChatMessage.Content>
              </ChatMessage.Body>
            </ChatMessage.Assistant>
          );
        }

        // This turn is still running and this is the last segment → it is "what is happening right now".
        const live = !turn.done && isLast && i === lastProcessIdx;
        // In finished turns, pure-noise segments are not rendered at all; this does not apply to the last segment of an unfinished turn,
        // otherwise the "thinking" segment would disappear.
        if (turn.done && !seg.items.some(isSubstantive)) return null;

        return (
          <ProcessBlock
            key={`${turn.key}-p${i}`}
            items={seg.items}
            live={live}
            expanded={live || !hasOutput}
          />
        );
      })}
    </>
  );
}

/**
 * Event stream → chat collapsed by turn.
 *
 * items must be the output of buildTimeline() (already paired, already sorted by sequence).
 *
 * `runStatus` is the fallback for closing the last turn (used to fold it when the final event was not stored);
 * for the check and reasons see `groupTurns` in `lib/timeline.ts`. Not passed while the chat page is streaming.
 */
export function ConversationView({
  items,
  agentName,
  runStatus,
}: {
  items: TimelineItem[];
  agentName: string;
  runStatus?: string;
}) {
  const turns = groupTurns(items, runStatus);

  return (
    <>
      {turns.map((turn, i) => (
        <TurnBlock
          key={turn.key}
          turn={turn}
          isLast={i === turns.length - 1}
          agentName={agentName}
        />
      ))}
    </>
  );
}

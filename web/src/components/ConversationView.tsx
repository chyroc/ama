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

import { Fragment, useEffect, useRef, useState } from "react";
import { useParams } from "react-router-dom";
import { Chip, Disclosure } from "@heroui/react";
import { ChainOfThought, ChatLoader, ChatMessage } from "@/components/ui/Chat";
import { Icon } from "@iconify/react";
import { MarkdownMessage } from "@/components/MarkdownMessage";
import { SeedanceImagePreview } from "@/components/SeedanceImagePreview";
import { CuaDesktopPanel } from "@/components/CuaDesktopPanel";
import {
  extractCuaSessionsFromTimelineItems,
  stripCuaUrlNoise,
  toEmbeddableCuaUrl,
  type CuaDesktopSession,
} from "@/lib/cua";
import {
  buildSeedancePlayableUrls,
  extractSeedanceTasksFromTimelineItems,
  type SeedanceVideoTask,
} from "@/lib/seedance";
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
import type { MessageKey } from "@/prefs/messages";

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
const MEMORY_TYPE_KEYS: Record<string, MessageKey> = {
  preference: "tl.memoryType.preference",
  profile: "tl.memoryType.profile",
  entity: "tl.memoryType.entity",
  event: "tl.memoryType.event",
  case: "tl.memoryType.case",
  pattern: "tl.memoryType.pattern",
};

function fallbackCopyText(text: string): boolean {
  if (typeof document === "undefined") return false;
  try {
    const el = document.createElement("textarea");
    el.value = text;
    el.style.position = "fixed";
    el.style.opacity = "0";
    document.body.appendChild(el);
    el.select();
    const ok = document.execCommand("copy");
    document.body.removeChild(el);
    return ok;
  } catch {
    return false;
  }
}

function CopyMessageButton({ text, className = "" }: { text: string; className?: string }) {
  const t = useT();
  const [copied, setCopied] = useState(false);
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => {
    return () => {
      if (timerRef.current !== null) {
        clearTimeout(timerRef.current);
      }
    };
  }, []);

  if (!text) return null;

  const markCopied = () => {
    setCopied(true);
    if (timerRef.current !== null) {
      clearTimeout(timerRef.current);
    }
    timerRef.current = setTimeout(() => setCopied(false), 1800);
  };

  return (
    <button
      type="button"
      onClick={() => {
        if (typeof navigator !== "undefined" && navigator.clipboard?.writeText) {
          navigator.clipboard
            .writeText(text)
            .then(markCopied)
            .catch(() => {
              if (fallbackCopyText(text)) markCopied();
            });
        } else if (fallbackCopyText(text)) {
          markCopied();
        }
      }}
      className={[
        "inline-flex items-center gap-1 r-row px-1.5 py-0.5 text-xs text-muted transition-colors hover:bg-surface-secondary hover:text-foreground",
        className,
      ].join(" ")}
      aria-label={copied ? t("chat.copied") : t("chat.copyMessage")}
      data-testid="chat-copy-message"
    >
      <Icon icon={copied ? "gravity-ui:check" : "gravity-ui:copy"} className="size-3.5" />
      <span>{copied ? t("chat.copied") : t("chat.copy")}</span>
    </button>
  );
}

function InjectedContext({ blocks }: { blocks?: string[] }) {
  const t = useT();
  if (!blocks?.length) return null;

  const totalMemories = blocks.reduce(
    (sum, b) => sum + parseHarnessBlock(b).memories.length,
    0,
  );

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
          <Icon
            icon={totalMemories > 0 ? "gravity-ui:database" : "gravity-ui:gear"}
            className="size-3.5"
          />
          <span className="font-medium">{t("tl.injectedContext")}</span>
          {totalMemories > 0 ? (
            <Chip size="sm" color="accent" variant="soft">
              <Chip.Label>{t("tl.recalledMemories", { count: totalMemories })}</Chip.Label>
            </Chip>
          ) : null}
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

  // No facts or memories parsed → show as is; do not hide the content (see the comment on InjectedContext).
  if (parsed.facts.length === 0 && parsed.memories.length === 0) {
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
            {parsed.kind === "openviking" ? t("tl.injectedOpenViking") : parsed.kind}
          </Chip>
        </p>
      ) : null}

      {/*
        <dl> instead of joining strings: this is "field → value" by nature, and with two columns
        values can be selected and copied on their own: nine times out of ten users expand this block to grab some id to look up.
        Values use tabular-nums + font-mono: a hex string only lines up in monospace.
      */}
      {parsed.facts.length > 0 ? (
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
      ) : null}

      {parsed.memories.length > 0 ? (
        <div className="flex flex-col gap-2" data-testid="injected-openviking-memories">
          {parsed.memories.map((m, idx) => {
            const scoreNum = m.score ? Number.parseFloat(m.score) : NaN;
            const scorePct = Number.isFinite(scoreNum)
              ? `${Math.round(scoreNum * 100)}%`
              : m.score;
            const typeKey = MEMORY_TYPE_KEYS[m.type];
            return (
              <div
                key={`${m.uri}-${idx}`}
                className="r-inset border border-border bg-surface-secondary/50 p-2.5 text-xs"
              >
                <div className="mb-1.5 flex flex-wrap items-center justify-between gap-1.5">
                  <div className="flex min-w-0 items-center gap-1.5">
                    {m.type ? (
                      <Chip size="sm" color="accent" variant="soft">
                        <Chip.Label>{typeKey ? t(typeKey) : m.type}</Chip.Label>
                      </Chip>
                    ) : null}
                    {scorePct ? (
                      <span className="r-chip bg-surface px-1.5 py-0.5 font-mono text-[11px] text-muted tabular-nums">
                        {t("tl.memoryRelevance", { score: scorePct })}
                      </span>
                    ) : null}
                  </div>
                  {m.uri ? (
                    <span className="truncate font-mono text-[11px] text-muted">{m.uri}</span>
                  ) : null}
                </div>
                <MarkdownMessage className="text-xs leading-relaxed text-foreground">
                  {m.content}
                </MarkdownMessage>
              </div>
            );
          })}
        </div>
      ) : null}

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

          {item.vaultProtected ? (
            <Chip size="sm" color="success" variant="soft" data-testid="tool-vault-badge">
              <Icon icon="gravity-ui:lock" className="size-3" />
              <Chip.Label>{t("tl.vaultBadge")}</Chip.Label>
            </Chip>
          ) : null}

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
          {item.vaultProtected && item.vaultAudit ? (
            <div
              className="flex flex-wrap items-center gap-x-4 gap-y-1.5 r-inset border border-success/30 bg-success/10 px-3 py-2 text-xs text-foreground"
              data-testid="tool-vault-audit-card"
            >
              <span className="inline-flex items-center gap-1.5 font-semibold text-success">
                <Icon icon="gravity-ui:shield-check" className="size-3.5" />
                {t("tl.vaultAuditTitle")}
              </span>
              {item.vaultAudit.vaultId ? (
                <span className="font-mono text-[11px] text-muted">
                  Vault: {item.vaultAudit.vaultId}
                </span>
              ) : null}
              {item.vaultAudit.credentialId ? (
                <span className="font-mono text-[11px] text-muted">
                  Credential: {item.vaultAudit.credentialId}
                </span>
              ) : null}
              {item.vaultAudit.authMode ? (
                <span className="text-[11px] text-muted">
                  {t("tl.vaultAuthMode", { mode: item.vaultAudit.authMode })}
                </span>
              ) : null}
              {item.vaultAudit.mcpEndpoint ? (
                <span className="font-mono text-[11px] text-muted">
                  MCP: {item.vaultAudit.mcpEndpoint}
                </span>
              ) : null}
            </div>
          ) : null}
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
  const vaultCallCount = items.filter((it) => it.vaultProtected).length;

  return (
    <ChainOfThought
      isExpanded={isExpanded}
      onExpandedChange={setIsExpanded}
      isStreaming={live}
      data-testid="turn-process"
    >
      <ChainOfThought.Trigger data-testid="turn-process-trigger">
        <span className="inline-flex flex-wrap items-center gap-2">
          {live ? t("turn.running") : <ProcessSummary stats={stats} t={t} />}
          {vaultCallCount > 0 ? (
            <Chip size="sm" color="success" variant="soft" data-testid="turn-process-vault-badge">
              <Icon icon="gravity-ui:lock" className="size-3" />
              <Chip.Label>{t("tl.vaultProcessBadge", { count: vaultCallCount })}</Chip.Label>
            </Chip>
          ) : null}
        </span>
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

function CuaTurnCard({
  sessions,
  defaultExpanded,
  onOpenCuaDesktop,
}: {
  sessions: CuaDesktopSession[];
  defaultExpanded: boolean;
  onOpenCuaDesktop?: (desktopId: string) => void;
}) {
  const t = useT();
  const [expanded, setExpanded] = useState(defaultExpanded);
  useEffect(() => {
    if (defaultExpanded) {
      setExpanded(true);
    }
  }, [defaultExpanded]);
  const primary = sessions[0];
  if (!primary) return null;
  const needsHuman = primary.outcome === "needs_human";
  const activeUrl = toEmbeddableCuaUrl(primary.fullInterfaceUrl || primary.desktopViewUrl);

  return (
    <div
      className="my-2 overflow-hidden r-container border border-border bg-surface shadow-2xs"
      data-testid="chat-cua-card"
    >
      <div className="flex flex-wrap items-center justify-between gap-2 border-b border-border px-3.5 py-2.5">
        <div className="flex min-w-0 items-center gap-2">
          <Icon icon="gravity-ui:display" className="size-4 shrink-0 text-accent" />
          <span className="text-xs font-semibold text-foreground">{t("cua.title")}</span>
          <Chip
            size="sm"
            color={needsHuman ? "warning" : "success"}
            variant="soft"
          >
            <Chip.Label>
              {needsHuman ? t("cua.badgeNeedsHuman") : t("cua.badgeLive")}
            </Chip.Label>
          </Chip>
          <span className="truncate font-mono text-xs text-muted">
            {t("cua.desktopLabel", { id: primary.desktopId })}
          </span>
        </div>

        <div className="flex items-center gap-1.5">
          {onOpenCuaDesktop && (
            <button
              type="button"
              onClick={() => onOpenCuaDesktop(primary.desktopId)}
              className="inline-flex h-7 items-center gap-1 rounded-md border border-default bg-surface-secondary px-2.5 text-xs font-medium text-foreground transition-colors hover:bg-surface-tertiary"
              data-testid="chat-cua-open-panel"
            >
              <Icon icon="gravity-ui:display" className="size-3.5" />
              <span>{t("cua.openInPanel")}</span>
            </button>
          )}
          <button
            type="button"
            onClick={() => setExpanded((v) => !v)}
            className="inline-flex h-7 items-center gap-1 rounded-md border border-default bg-surface px-2.5 text-xs text-muted transition-colors hover:bg-surface-secondary hover:text-foreground"
            data-testid="chat-cua-toggle-inline"
          >
            <Icon
              icon={
                expanded
                  ? "gravity-ui:chevrons-collapse-up-right"
                  : "gravity-ui:chevrons-expand-up-right"
              }
              className="size-3.5"
            />
            <span>{expanded ? t("cua.collapseInline") : t("cua.expandInline")}</span>
          </button>
          <a
            href={activeUrl}
            target="_blank"
            rel="noopener noreferrer"
            className="inline-flex h-7 items-center gap-1 rounded-md border border-default bg-surface px-2 text-xs text-muted transition-colors hover:bg-surface-secondary hover:text-foreground"
          >
            <span>{t("cua.openExternal")}</span>
            <Icon icon="gravity-ui:arrow-up-right-from-square" className="size-3" />
          </a>
        </div>
      </div>

      {!expanded ? (
        <div className="px-3.5 py-2 text-xs text-muted">{t("cua.cardSubtitle")}</div>
      ) : (
        <div className="h-[520px] w-full">
          <CuaDesktopPanel sessions={sessions} selectedDesktopId={primary.desktopId} />
        </div>
      )}
    </div>
  );
}

function SeedanceTaskCardItem({
  task,
  runId,
}: {
  task: SeedanceVideoTask;
  runId?: string;
}) {
  const t = useT();
  const [showStoryboard, setShowStoryboard] = useState(false);
  const [sourceIdx, setSourceIdx] = useState(0);
  const [videoLoadFailed, setVideoLoadFailed] = useState(false);

  const { sources, downloadUrl, externalUrl } = buildSeedancePlayableUrls(task, runId);

  const primarySourceKey = `${task.sessionId}|${sources[0] ?? ""}`;
  useEffect(() => {
    setSourceIdx(0);
    setVideoLoadFailed(false);
  }, [primarySourceKey]);

  const activeVideoUrl = !videoLoadFailed ? (sources[sourceIdx] ?? sources[0]) : undefined;
  const isImage = task.kind === "image";
  const isCompleted = task.outcome === "completed";
  const isFailed = task.outcome === "failed";
  const isPreviewUnavailable = isCompleted && !isImage && !activeVideoUrl;
  const isPortrait = task.ratio === "9:16" || task.ratio === "3:4";
  const storyboardText = task.storyboardPrompt || task.userPrompt || "";

  return (
    <div
      className="my-2.5 overflow-hidden r-container border border-border bg-surface shadow-2xs"
      data-testid="chat-seedance-card"
    >
      <div className="flex flex-wrap items-center justify-between gap-2 border-b border-border px-3.5 py-2.5">
        <div className="flex min-w-0 flex-wrap items-center gap-2">
          <Icon icon={isImage ? "gravity-ui:picture" : "gravity-ui:play"} className="size-4 shrink-0 text-accent" />
          <span className="text-xs font-semibold text-foreground">
            {task.title || t(isImage ? "seedance.imageDefaultTitle" : "seedance.defaultTitle")}
          </span>
          <Chip
            size="sm"
            color={isCompleted ? "success" : isFailed ? "danger" : "accent"}
            variant="soft"
          >
            <Chip.Label>
              {isCompleted
                ? t(isImage ? "seedance.imageStatusCompleted" : "seedance.statusCompleted")
                : isFailed
                  ? t("seedance.statusFailed")
                  : t(isImage ? "seedance.imageStatusRendering" : "seedance.statusRendering")}
            </Chip.Label>
          </Chip>
          <span className="r-chip bg-surface-secondary px-1.5 py-0.5 font-mono text-[11px] text-muted">
            {isImage ? (task.imageModel || t("seedance.imageModelLabel")) : (task.videoModel || t("seedance.modelLabel"))}
          </span>
          {task.ratio ? (
            <span className="r-chip bg-surface-secondary px-1.5 py-0.5 font-mono text-[11px] text-muted">
              {task.ratio}
            </span>
          ) : null}
          {task.duration ? (
            <span className="r-chip bg-surface-secondary px-1.5 py-0.5 font-mono text-[11px] text-muted">
              {task.duration}s
            </span>
          ) : null}
        </div>

        <div className="flex items-center gap-1.5">
          {storyboardText ? (
            <button
              type="button"
              onClick={() => setShowStoryboard((v) => !v)}
              className="inline-flex h-7 items-center gap-1 r-row border border-border bg-surface px-2.5 text-xs text-muted transition-colors hover:bg-surface-secondary hover:text-foreground"
              data-testid="chat-seedance-toggle-storyboard"
            >
              <Icon icon="gravity-ui:file-text" className="size-3.5" />
              <span>
                {showStoryboard
                  ? t("seedance.hideStoryboard")
                  : t("seedance.showStoryboard")}
              </span>
            </button>
          ) : null}
          {activeVideoUrl ? (
            <>
              <a
                href={downloadUrl || activeVideoUrl}
                download
                target="_blank"
                rel="noopener noreferrer"
                className="inline-flex h-7 items-center gap-1 r-row border border-border bg-surface-secondary px-2.5 text-xs font-medium text-foreground transition-colors hover:bg-surface-tertiary"
                data-testid="chat-seedance-download"
              >
                <Icon icon="gravity-ui:arrow-down" className="size-3.5" />
                <span>{t("seedance.downloadMp4")}</span>
              </a>
              <a
                href={externalUrl || activeVideoUrl}
                target="_blank"
                rel="noopener noreferrer"
                className="inline-flex h-7 items-center gap-1 r-row border border-border bg-surface px-2 text-xs text-muted transition-colors hover:bg-surface-secondary hover:text-foreground"
                data-testid="chat-seedance-open-external"
              >
                <span>{t("seedance.openExternal")}</span>
                <Icon icon="gravity-ui:arrow-up-right-from-square" className="size-3" />
              </a>
            </>
          ) : null}
        </div>
      </div>

      {isImage && isCompleted ? (
        <SeedanceImagePreview images={task.images || []} runId={runId} title={task.title || t("seedance.imageDefaultTitle")} />
      ) : activeVideoUrl ? (
        <div className="flex items-center justify-center bg-black/95 p-2">
          <video
            src={activeVideoUrl}
            controls
            playsInline
            preload="metadata"
            onError={() => {
              if (sourceIdx + 1 < sources.length) {
                setSourceIdx((idx) => idx + 1);
              } else {
                setVideoLoadFailed(true);
              }
            }}
            className={
              isPortrait
                ? "mx-auto max-h-[520px] w-auto r-inset"
                : "max-h-[460px] w-full r-inset object-contain"
            }
            data-testid="chat-seedance-video"
          />
        </div>
      ) : isPreviewUnavailable ? (
        <div className="flex items-center gap-2 px-3.5 py-3 text-xs text-muted">
          <Icon icon="gravity-ui:picture" className="size-4 shrink-0" />
          <span>{task.stageLabel || t("seedance.previewUnavailable")}</span>
        </div>
      ) : isFailed ? (
        <div className="flex items-center gap-2 bg-danger/5 px-3.5 py-3 text-xs text-danger">
          <Icon icon="gravity-ui:circle-exclamation" className="size-4 shrink-0" />
          <span>{task.errorMessage || task.stageLabel || t("seedance.failedFallback")}</span>
        </div>
      ) : (
        <div className="flex flex-col gap-2 px-3.5 py-3">
          <div className="flex items-center justify-between gap-2">
            <div className="flex items-center gap-2 text-xs font-medium text-foreground">
              <ChatLoader.Dots size="sm" label={t("seedance.statusRendering")} className="shrink-0" />
              <span>{task.stageLabel || t("seedance.renderingFallback")}</span>
            </div>
            {task.sessionId && !task.sessionId.startsWith("pending-") ? (
              <span className="font-mono text-[11px] text-muted">{task.sessionId}</span>
            ) : null}
          </div>
          {task.storyboardPrompt ? (
            <div className="r-inset border border-border bg-surface-secondary/60 p-2.5 text-xs leading-relaxed text-muted">
              <div className="mb-1 font-medium text-foreground">
                {t("seedance.storyboardHeading")}
              </div>
              <div className="clamp-3 whitespace-pre-wrap">
                {task.storyboardPrompt}
              </div>
            </div>
          ) : null}
        </div>
      )}

      {showStoryboard && storyboardText ? (
        <div className="border-t border-border bg-surface-secondary/50 px-3.5 py-2.5 text-xs">
          <div className="mb-1 flex items-center justify-between text-[11px] font-medium text-muted">
            <span>{t("seedance.storyboardPromptTitle")}</span>
            {task.cgtTaskId ? (
              <span className="font-mono">{task.cgtTaskId}</span>
            ) : null}
          </div>
          <pre className="max-h-56 overflow-auto whitespace-pre-wrap font-sans text-xs leading-relaxed text-foreground">
            {storyboardText}
          </pre>
        </div>
      ) : null}
    </div>
  );
}

function SeedanceTurnCard({
  tasks,
  runId,
}: {
  tasks: SeedanceVideoTask[];
  runId?: string;
}) {
  if (tasks.length === 0) return null;
  return (
    <>
      {tasks.map((task) => (
        <SeedanceTaskCardItem key={task.sessionId} task={task} runId={runId} />
      ))}
    </>
  );
}

function TurnBlock({
  turn,
  isLast,
  agentName,
  runId,
  onOpenCuaDesktop,
}: {
  turn: Turn;
  isLast: boolean;
  agentName: string;
  runId?: string;
  onOpenCuaDesktop?: (desktopId: string) => void;
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
  const lastOutputIdx = segments.reduce(
    (acc, seg, i) => (seg.kind === "output" ? i : acc),
    -1,
  );

  // Extract any CUA cloud desktop sessions opened or updated during this turn.
  const turnItems: TimelineItem[] = [];
  for (const seg of turn.segments) {
    if (seg.kind === "process") {
      turnItems.push(...seg.items);
    } else {
      turnItems.push(seg.item);
    }
  }
  const cuaSessions = extractCuaSessionsFromTimelineItems(turnItems);
  const cuaNeedsHuman = cuaSessions.some((s) => s.outcome === "needs_human");
  // Show the inline CUA card only while this latest turn is actively running or when CUA
  // explicitly paused for human takeover (`needs_human`). Once a normal CUA step finishes,
  // do not leave a permanent card in the chat stream.
  const showCuaTurnCard =
    cuaSessions.length > 0 && isLast && (!turn.done || cuaNeedsHuman);
  const seedanceTasks = extractSeedanceTasksFromTimelineItems(turnItems);

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
          const cleanReply = stripCuaUrlNoise(seg.item.body || seg.item.summary);
          // Agent replies have no bubble: a long reply reads more smoothly aligned left than stuffed into a bubble.
          return (
            <Fragment key={seg.item.key}>
              {seedanceTasks.length > 0 && i === lastOutputIdx ? (
                <SeedanceTurnCard tasks={seedanceTasks} runId={runId} />
              ) : null}
              <ChatMessage.Assistant className="group" data-testid="chat-agent-message">
                <ChatMessage.Avatar alt={agentName} fallback={agentName.slice(0, 1) || "A"} />
                <ChatMessage.Body>
                  {/*
                    The agent's reply is Markdown, rendered by the Markdown renderer (headings/code blocks/tables/lists).
                    **Do not** add whitespace-pre-wrap here: the Markdown renderer has already laid it out as block-level elements,
                    and keeping the raw line breaks too would double the blank lines between paragraphs.
                  */}
                  <ChatMessage.Content>
                    <MarkdownMessage>{cleanReply}</MarkdownMessage>
                  </ChatMessage.Content>
                  <div className="mt-1 flex items-center gap-2 opacity-60 transition-opacity group-hover:opacity-100">
                    <CopyMessageButton text={cleanReply} />
                  </div>
                </ChatMessage.Body>
              </ChatMessage.Assistant>
            </Fragment>
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

      {seedanceTasks.length > 0 && lastOutputIdx < 0 ? (
        <SeedanceTurnCard tasks={seedanceTasks} runId={runId} />
      ) : null}

      {showCuaTurnCard ? (
        <CuaTurnCard
          sessions={cuaSessions}
          defaultExpanded={isLast && !onOpenCuaDesktop}
          onOpenCuaDesktop={onOpenCuaDesktop}
        />
      ) : null}
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
  runId,
  runStatus,
  onOpenCuaDesktop,
}: {
  items: TimelineItem[];
  agentName: string;
  runId?: string;
  runStatus?: string;
  onOpenCuaDesktop?: (desktopId: string) => void;
}) {
  const params = useParams<{ runId?: string }>();
  const effectiveRunId = runId || params.runId;
  const turns = groupTurns(items, runStatus);

  return (
    <>
      {turns.map((turn, i) => (
        <TurnBlock
          key={turn.key}
          turn={turn}
          isLast={i === turns.length - 1}
          agentName={agentName}
          runId={effectiveRunId}
          onOpenCuaDesktop={onOpenCuaDesktop}
        />
      ))}
    </>
  );
}

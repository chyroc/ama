/**
 * Chat page.
 *
 * Recent conversations on the left + the main conversation area on the right, shaped after Codex:
 * the conversation list can be switched, the body is the message stream,
 * the composer is fixed at the bottom, and the agent picker sits next to the input.
 *
 * **One conversation is one Run**, and messages are RunEvents; we deliberately add no
 * Conversation / Message entities. So conversations automatically show up in run history, can be
 * reopened from history to continue,
 * and tool pairing logic is shared with RunDetailPage (see ChatStream.tsx).
 *
 * The left column takes two triggers, manual (started on the web) and feishu (started in Feishu);
 * see
 * CONVERSATION_TRIGGERS. The order comes from the backend by lastActivityAt, so a conversation just
 * spoken in on Feishu
 * floats to the top; it reuses the same Run, and startedAt does not change.
 *
 * runId lives in the URL rather than component state, matching the site-wide rule "links can be
 * shared, refresh keeps context".
 *
 * # Sending several messages in a row: three paths, do not mix them up
 *
 * "The previous message is still running and I sent another" looks like one thing, but it is
 * really three, each with its own right response.
 * They were mixed once; the cost is at the end of each item below.
 *
 * **1. Hitting the tool-execution window (a race of tens of milliseconds, unrelated to user
 * intent).**
 * Under self_hosted tools run in our own container; between Ark sending tool_use and the worker
 * returning
 * tool_result there is a window of tens of milliseconds, and messages sent inside it are rejected
 * by Ark (the backend turns it into
 * **409 + code=conflict**). **That is normal, not a fault**: it happens again and again when the
 * agent calls tools in a row,
 * and every tool call pushes a session.status_idle, so the UI looks idle and the user hits it as
 * soon as they send.
 * The user has no intent to "wait"; they are just quick.
 * ⚠️ **This is now mainly retried by the server itself** (sendEventsThroughWindow in core/chat.go,
 * with a 1-second budget), because not only the browser hits the window: Feishu inbound and
 * schedules use the same endpoint too.
 * The frontend copy (`send()` recognizes the code, then queues and resends) **is kept as a
 * fallback**; do not delete it: it is also
 * what takes over after "the server used up its retry budget".
 *
 * **2. The user queues on purpose (waiting a whole turn, tens of seconds to minutes).**
 * The user says a second thing while the agent is clearly still running: they know they must wait,
 * they just do not want to hold it in.
 * This **cannot** be solved by retrying (that would hang an HTTP request for minutes); they must
 * see their position in line,
 * and be able to cancel and edit. It belongs to `lib/useSendQueue.ts`.
 *
 * **3. Redirect (stop waiting, interrupt the previous turn).**
 * "Send now" on a queued message. The opposite of item 2: not waiting, but stopping.
 * It uses the server primitive `api.redirectRun` (interrupt + message done back to back on the
 * server);
 * ⚠️ **it is not calling interrupt and then sendMessage from the frontend**: the network round trip
 * between those two steps is exactly
 * the rejection window of item 1, and would sometimes become "only interrupted, the new message was
 * not sent", with zero errors.
 *
 * The check is code, not HTTP 409, and certainly not that Chinese text; see the header of the
 * useSendQueue file.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  Link,
  useNavigate,
  useParams,
  useSearchParams,
} from "react-router-dom";
import { Button } from "@heroui/react";
import { api } from "@/data/api";
import { useWorkspace } from "@/app/WorkspaceGate";
import { ChatComposer } from "@/components/ChatComposer";
import {
  ChatSessionsSidebar,
  ChatSessionsTrigger,
} from "@/components/ChatSessions";
import { ChatStream } from "@/components/ChatStream";
import { SendQueueList } from "@/components/SendQueueList";
import { RawEventList } from "@/components/RawEventList";
import { TracePanel } from "@/components/TracePanel";
import { SandboxPanel } from "@/components/SandboxPanel";
import { TimelineToolbar, type ToolbarCounts } from "@/components/TimelineToolbar";
import { ViewSwitch, useRunView } from "@/components/ViewSwitch";
import {
  AgentDeletedChip,
  AgentDisabledChip,
  AsyncErrorBlock,
  Empty,
  InlineError,
  LoadingBlock,
  Notice,
} from "@/components/Page";
import {
  STREAM_GONE,
  useConversationStream,
} from "@/lib/useConversationStream";
import { buildTimeline } from "@/lib/timeline";
import {
  applyRawFilter,
  buildRawHaystack,
  useTimelineFilter,
} from "@/lib/useTimelineFilter";
import { buildRawRows, rawEventStats } from "@/lib/rawEvents";
import {
  isSendConflict,
  useSendQueue,
  SEND_QUEUE_LIMIT,
  type GiveUpReason,
  type QueuedSend,
} from "@/lib/useSendQueue";
import {
  classifyError,
  useAsyncData,
  useWriteError,
  type AsyncError,
} from "@/lib/useAsyncData";
import { useCanManage } from "@/lib/useCanManage";
import { useT } from "@/prefs/PrefsContext";
import type { RunEventType } from "@/types/events";
import type { Agent, Run, RunTrigger } from "@/types/domain";

/**
 * Which Runs the left conversation list takes.
 *
 * `manual` is started by clicking on the web, `feishu` comes from chatting in Feishu; both are "a
 * person talking with an agent",
 * so they are listed together, and what the user said on one side can be opened and continued from
 * the other.
 *
 * **Cannot keep only manual**: Runs from the Feishu path carry trigger=feishu
 * (CreateFeishuConversation in core/chat.go); leaving it out hides all Feishu conversations from
 * the left column.
 *
 * schedule / wakeup / api / webhook are deliberately excluded: those run automatically and belong
 * to run history, not conversations.
 *
 * ⚠️ **This allowlist is now pushed down to the backend** (`api.listRuns(ws, { trigger })`), no
 * longer
 * `.filter()`ed in the browser. The meaning is exactly the same: the backend does `doc->>'trigger' =
 * any($n)`, and an empty array means no filter,
 * so this constant being non-empty is itself a precondition (passing it empty would make the left
 * column "all runs" rather than empty).
 *
 * ⚠️ **`limit` is deliberately not added.** The left column is the conversation list, and users
 * rely on it to "continue from history"; with a limit
 * and no "load more" entry, older conversations would vanish from the UI for good; worse than
 * slow, and with zero errors.
 * Paging needs a paging UI first; see the `RunQuery.limit` note in api.ts.
 */
const CONVERSATION_TRIGGERS: readonly RunTrigger[] = ["manual", "feishu"];

/**
 * "Does this agent have usable Ark credentials right now": **if not, stop the user before they
 * type**.
 *
 * # Why this hook is needed
 *
 * Before, the chat page knew nothing about credentials: the input rendered as usual, and the user
 * typed a whole message and clicked send, only to have the backend
 * push back a 422 (`arkKey` in `core/chat.go`). The message was written for nothing, and had to be
 * typed
 * again. In production this kind of feedback came from many different workspaces, 1–2 times each,
 * all with the shape
 * "the first message in a new workspace hits it once, and then the person goes to configure it";
 * so it is an **onboarding** problem,
 * not a fault: every new workspace will hit it again, and that is the reason to fix it.
 *
 * # ⚠️ The check must use the agent-level endpoint, not just the workspace level
 *
 * Credentials **fall back across three levels** (agent → workspace → account, the more specific the
 * higher priority; see
 * `resolveArkSecretForAgent` in `server/core/ark.go`). Checking only
 * `getArkCredential(workspaceId)` would judge **an agent that configured its own key** as
 * "unusable" when the workspace has none,
 * so the input would be disabled while it can clearly run. That is a wrong decision with zero
 * errors:
 * the backend lets it through as usual, only the UI blocks it, and the user has no way forward.
 *
 * The `effective` returned by `getAgentArkCredential` is the server's computed "which key this
 * agent actually uses"
 * (`arkEffective` in `core/agentark.go`), the same fallback order as the conversation path.
 * **Do not assemble the three levels in the frontend**: it would surely drift from the server, and
 * drift shows up as the wrong decision above.
 *
 * # Three states, not two
 *
 * `null` = not known yet. Then **nothing is rendered and the input is not disabled**, for the same
 * reason as `useCanManage`'s
 * three states, word for word: two states would make every normal workspace flash "no key
 * configured yet", gone 90ms later.
 * That is more alarming than no hint, and gets reported as a bug in screenshots/recordings.
 *
 * # Failure always counts as "usable" (let through), the opposite of useCanManage's conservative
 * direction
 *
 * This is deliberate: what this hook produces is a **gate**, and the two wrong guesses cost very
 * different amounts.
 *
 *   guess "unusable" → a workspace with perfectly good credentials gets its input disabled for
 *                      good; the user cannot send a word,
 *                      and the page says "no key configured yet" (false). One read hiccup can
 *                      cause it.
 *   guess "usable"   → back to today's behavior: they type and click send, and get the 422 that
 *                      explains the next step.
 *
 * The latter at worst just fails to stop them early; the former blocks the feature entirely with a
 * wrong explanation. So read failures let it through.
 */
type ArkGate =
  | { state: "loading" }
  | { state: "ready" }
  /** None of the three levels configured → go to workspace settings and paste one. */
  | { state: "missing" }
  /** References the account-level one, but it no longer exists → reconfigure in global settings, or switch to this workspace's custom key. */
  | { state: "inheritGone" };

function useArkGate(workspaceId: string, agentId: string): ArkGate {
  /*
   * Both must be queried, and **neither can be skipped**:
   *
   *   the agent-level one      gives "which key actually takes effect" (the conclusion of the
   *                            three-level fallback) → decides whether to block
   *   the workspace-level one  gives source=inherit or override → decides what to say after
   *                            blocking
   *
   * `effective` has no source (it says "which level was used", not "how the workspace is
   * configured";
   * see the "do not infer from sec" part in core/agentark.go), so the two kinds of "none" cannot be
   * told apart,
   * and their next steps are completely different; giving the wrong guide makes users follow it and
   * then find it does nothing.
   */
  /*
   * ⚠️ **When no agent is selected, that request must not really be sent; short-circuit it inside
   * the fetcher.**
   *
   * Hooks cannot be called conditionally, so "no agent selected yet" cannot be guarded outside
   * useAsyncData.
   * At first this was `useAsyncData(...)` first, then `if (!agentId) return ready` below,
   * so **the request was still sent**: `getAgentArkCredential("")` queried with an empty id, and
   * the backend/mock returned
   * "agent does not exist". And `useAsyncData`'s catch **silently reports one first-level feedback**
   * for every read failure
   * (see lib/useAsyncData.ts), so that fake fault went into the feedback store:
   * in production it showed as "every time the chat page opens before an agent is picked, one more
   * agent-does-not-exist",
   * with zero errors, pure pollution, and the error rate metric inflated too.
   *
   * What actually caught it was the 16-feedback test: it asserts that the "failed to send message"
   * report's
   * `httpStatus` is 500, but got 0, because this fake fault landed **first**.
   * That test had nothing to do with credentials; it pushed this bug out for us.
   *
   * Returns null rather than throwing: null means "nothing to judge", and below it lets things
   * through.
   */
  const fetchAgentCred = useCallback(
    () => (agentId ? api.getAgentArkCredential(agentId) : Promise.resolve(null)),
    [agentId],
  );
  const fetchWsCred = useCallback(
    () => api.getArkCredential(workspaceId),
    [workspaceId],
  );
  // The text is not used (this hook renders no error state; failure always lets through), but
  // useAsyncData requires one.
  const agentCred = useAsyncData(fetchAgentCred, "");
  const wsCred = useAsyncData(fetchWsCred, "");

  // With no agent selected nothing can be judged (the dropdown guides picking one), so let it
  // through.
  if (!agentId) return { state: "ready" };
  // Either one fails → let through (see above: the costs of a wrong gate guess are asymmetric).
  if (agentCred.error || wsCred.error) return { state: "ready" };
  if (agentCred.data === null || wsCred.data === null) return { state: "loading" };

  /*
   * ⚠️ The check is `effective.apiKeyMasked` non-empty, **not** `custom`, and not `check` either.
   *
   *   using custom → only "the agent configured its own key" counts, and every agent following the
   *                  workspace is judged unusable;
   *   using check  → "configured but not verified yet" would be judged unusable, while it can
   *                  actually run (the backend's arkKey ignores
   *                  the verification result, only whether a key exists). That would block a usable
   *                  workspace for good.
   */
  if (agentCred.data.effective.apiKeyMasked) return { state: "ready" };

  // No usable credentials. The two kinds of "none" are told apart by the workspace's source, the
  // same origin as the branches of the backend's arkKey.
  return wsCred.data.source === "inherit"
    ? { state: "inheritGone" }
    : { state: "missing" };
}

export function ChatPage() {
  const t = useT();
  const { workspace } = useWorkspace();
  const { runId } = useParams<{ runId?: string }>();
  const [params] = useSearchParams();
  const navigate = useNavigate();

  const [agents, setAgents] = useState<Agent[] | null>(null);
  // Why the first load failed. null means it did not fail.
  const [loadError, setLoadError] = useState<AsyncError | null>(null);
  const [conversations, setConversations] = useState<Run[]>([]);
  // Empty string while no agent is selected; ?agent= can be carried over from the Agent detail page.
  const [agentId, setAgentId] = useState(params.get("agent") ?? "");
  // Error state for send/interrupt failures. **Use useWriteError rather than a bare
  // useState<string>**:
  // it also wires up first-level automatic records, and gives an AsyncError for InlineError to hang
  // the second-level entry on.
  // This used to be `useState<string | null>`, so for "failed to send message" both feedback levels
  // failed at once
  // (first level recorded nothing, second level had no entry); see the useWriteError docs in
  // lib/useAsyncData.ts.
  const sendFail = useWriteError(t("chat.sendFailedUnknown"));
  const [view, setView] = useRunView();

  /*
   * Up-front guide when no Ark credentials are configured (see useArkGate).
   *
   * `canManage` decides which sentence to give: people who can configure get a button straight to
   * the settings page, people who cannot get
   * "please contact an admin"; giving the latter a button sure to 403 is worse than none (same as
   * the AgentRuntimeSections
   * rule "a button that can never be clicked is more confusing than no button").
   */
  const arkGate = useArkGate(workspace.id, agentId);
  const canManage = useCanManage(workspace.id);
  /**
   * Credentials unusable → input disabled.
   *
   * ⚠️ **Neither `loading` nor `ready` disables it.** "Not known yet" counts as usable, otherwise
   * every normal
   * workspace would flash a disabled state (see the three-states part of useArkGate).
   */
  const arkBlocked = arkGate.state === "missing" || arkGate.state === "inheritGone";

  /**
   * Text to put back into the input (clicking "edit" on a queued message, or handing back the text
   * after a full queue/timeout).
   *
   * Carries a token rather than a bare string: the same sentence may be put back twice in a row,
   * and with a bare string
   * the dependency of that ChatComposer effect would not change, so the second time would silently
   * do nothing. See the comment there.
   */
  const [refill, setRefill] = useState<{ text: string; token: number } | null>(null);
  /**
   * Queue-related **non-fault** notes (full / timed out in line).
   *
   * Kept apart from `sendFail` on purpose: that one is red text + one-click feedback + silently
   * reporting a first-level record,
   * while neither of these cases is a server problem; using sendFail would pollute error rates and
   * the feedback store,
   * and make users think the system is broken (all they really need to do is wait or cancel a few).
   */
  const [queueNotice, setQueueNotice] = useState<string | null>(null);
  /**
   * The send currently in flight. Used by "interrupt" so it does not run ahead of the send (see the
   * comment on interrupt).
   *
   * A ref rather than state: it is only read by async flows and should not trigger renders; also
   * `send` is a plain function rebuilt on every render,
   * and state would give it the previous frame's snapshot.
   */
  const inflightSend = useRef<Promise<void> | null>(null);

  const stream = useConversationStream(runId ?? null);

  // Load the agent list and past conversations.
  //
  // The conversation list's trigger allowlist is **pushed down to the backend** (see
  // CONVERSATION_TRIGGERS): fetching everything and filtering
  // in the browser would also transfer the schedule, API and webhook runs that are never shown.
  const reload = useCallback(async () => {
    const [agentList, runs] = await Promise.all([
      api.listAgents(workspace.id),
      api.listRuns(workspace.id, { trigger: [...CONVERSATION_TRIGGERS] }),
    ]);
    setAgents(agentList);
    setConversations(runs);
    return agentList;
  }, [workspace.id]);

  /*
   * **Must catch**: it used to be `void reload().then(...)`, with no .catch. Any reject
   * (after RBAC, non-members / insufficient roles get 403) would leave agents at null forever, and
   * the page stuck
   * loading forever, while the error hints for send/interrupt below were written but never reached.
   * See the file header of lib/useAsyncData.ts.
   */
  useEffect(() => {
    setLoadError(null);
    reload()
      .then((agentList) => {
        // Without ?agent=, pick the first usable one by default, saving a click.
        //
        // ⚠️ **The fallback must also skip disabled ones**. It used to be `find(active) ??
        // agentList[0]`,
        // and that fallback would, when "all agents are disabled", pick an agent that can never
        // send a message;
        // the user types a sentence only to be pushed back by a 422, while the dropdown clearly
        // shows a name.
        // Disabling really blocks now, so better to pick none (the dropdown stays on its empty
        // state, guiding the user to pick).
        setAgentId((prev) => {
          if (prev) return prev;
          const usable = agentList.filter((a) => a.status !== "disabled");
          const preferred = usable.find((a) => a.status === "active") ?? usable[0];
          return preferred?.id ?? "";
        });
      })
      .catch((e: unknown) => {
        console.error("加载对话页数据失败", e);
        setLoadError(classifyError(e, t("common.loadFailedUnknown")));
      });
  }, [reload, t]);

  // When opening an existing conversation, the agent follows that conversation (it cannot be
  // switched midway).
  const activeRun = useMemo(
    () => conversations.find((r) => r.id === runId) ?? null,
    [conversations, runId],
  );
  useEffect(() => {
    if (activeRun) setAgentId(activeRun.agentId);
  }, [activeRun]);

  const agentName = useMemo(() => {
    if (activeRun) return activeRun.agentName;
    return agents?.find((a) => a.id === agentId)?.name ?? "";
  }, [activeRun, agents, agentId]);

  /**
   * This conversation is frozen: view only, no sending. Two causes: the linked agent was
   * **deleted**, or **disabled**.
   *
   * The check only uses the server-computed `agentDeleted` / `agentDisabled`, **not inferred from
   * the local agents list**:
   * a single-source check also avoids first-frame issues like "flashing deleted before agents
   * finish loading", and uses the same fact as the backend gate
   * (AppendRunEvents returns 422).
   *
   * ⚠️ **Disabling is reversible**, so this value goes back and forth with the agent's state. Do
   * not cache it in state:
   * when the user re-enables the agent and refreshes, this must follow at once.
   */
  const frozen = Boolean(activeRun?.agentDeleted || activeRun?.agentDisabled);

  // What the conversation view and call tree need: through the buildTimeline pipeline (tool calls
  // paired into one).
  // Only needed after switching there, but hooks cannot be called conditionally, so compute it
  // anyway (a pure function; one conversation's volume does not matter).
  const allItems = useMemo(
    () => buildTimeline(stream.events, t),
    [stream.events, t],
  );
  /*
   * What the event view needs: **skip buildTimeline and use the raw events directly**, one per
   * row, nothing merged.
   * Its meaning is "faithfully show the raw events Ark returned", and going through the processing
   * pipeline is itself a conceptual mismatch
   * (see the top of lib/rawEvents.ts). The conversation view still uses allItems above.
   */
  const rawRows = useMemo(() => buildRawRows(stream.events, t), [stream.events, t]);
  const rawStats = useMemo(() => rawEventStats(rawRows), [rawRows]);
  // The type filter only lists types that really appeared in this run. Taken from **raw events**:
  // the event view is its host,
  // and pairing would make absorbed types like user.tool_result vanish from the dropdown entirely.
  const presentTypes = useMemo(
    () => Object.keys(rawStats.byType) as RunEventType[],
    [rawStats.byType],
  );
  const { filter, isDirty, ...filterActions } = useTimelineFilter(presentTypes);
  // Every event must be JSON.stringify'd, so cache it; otherwise every keystroke in the keyword
  // re-serializes all events.
  const rawHaystack = useMemo(() => buildRawHaystack(rawRows), [rawRows]);
  const eventRows = useMemo(
    () => applyRawFilter(rawRows, filter, rawHaystack),
    [rawRows, filter, rawHaystack],
  );
  /*
   * The toolbar counts come from **raw events**, on the same basis as what it filters.
   *
   * Using the computeStats() copy would give "the filter says tool has 8, clicking shows 15";
   * that inconsistency gives no error, it only makes people suspect the filter is broken. See the
   * comment on rawEventStats.
   */
  const toolbarCounts = useMemo<ToolbarCounts>(
    () => ({
      byCategory: rawStats.byCategory,
      byDomain: rawStats.byDomain,
      byType: rawStats.byType,
      slowCount: rawStats.slow,
      failedCount: rawStats.failed,
    }),
    [rawStats],
  );

  /**
   * Really send one message (the conversation already exists).
   *
   * Split out of `send()` because **queued resends must take the same path**: subscribe, send,
   * refresh the left column; none of the three
   * steps can be skipped (especially subscribe: Ark does not replay history; see the comment in
   * useConversationStream).
   * A copy would surely drift, and drift shows up as "the resent one gets no reply", with zero
   * errors.
   *
   * **Deliberately does not catch**: success or failure is for the caller to judge (`send` uses it
   * to decide queueing, the queue uses it to decide retries).
   * But **on failure markStreaming must be undone**: that flag is optimistic, and this turn never
   * started,
   * so no event will ever come to clear streaming. Without undoing it, the composer stays in the
   * "interrupt" state forever,
   * and the user cannot send a single message while queued (see clearStreaming in
   * useConversationStream).
   */
  const deliver = useCallback(
    async (targetId: string, text: string) => {
      stream.markStreaming(targetId);
      try {
        // **Wait for the subscription first, then send.** An Ark subscription only pushes events
        // "produced after it" and does not replay,
        // so in the wrong order this turn's reply is lost for good (it shows as sent but not a
        // single word comes back,
        // until a refresh fills it in from history). whenSubscribed has its own timeout fallback,
        // so it never blocks sending.
        await stream.whenSubscribed(targetId);
        const event = await api.sendMessage(targetId, text);
        stream.appendLocal(event);
      } catch (e) {
        stream.clearStreaming();
        throw e;
      }
      // The title is backfilled by the data layer from the first message, so the left column must
      // be refetched to keep up.
      // await rather than void here: without waiting, "jump to run history right after sending"
      // would see the title still empty.
      await reload();
    },
    // stream is a new object on every render (the hook returns a literal), but its methods are
    // useCallback /
    // ref stable, so rebuilding along with it is harmless; reload is already memoized.
    [stream, reload],
  );

  /**
   * Wrap-up when a queued resend is given up: hand the text back to the input, and explain why it
   * was not sent.
   *
   * **The two causes need two sets of wording, and only one counts as a fault**:
   *
   *   - `error`   hit some other error (session frozen, 5xx…). This is a real fault; use
   *               `sendFail.fail(e)`,
   *               the existing two-level feedback (red text + one-click feedback entry + silent
   *               first-level report).
   *   - `timeout` waited 3 minutes and the tool still has not finished. **This is not a fault**;
   *               sendFail would paint it as
   *               red "send failed" text and report a fake fault, polluting error rates and the
   *               feedback store. So it uses the notice path.
   */
  const onQueueGiveUp = useCallback(
    (item: QueuedSend, reason: GiveUpReason, error: unknown) => {
      // Always hand the text back to the input: the user's typed words must not vanish just
      // because we did not send them.
      setRefill((prev) => ({ text: item.text, token: (prev?.token ?? 0) + 1 }));
      if (reason === "error") {
        sendFail.fail(error);
        return;
      }
      setQueueNotice(t("chat.queueTimeout"));
    },
    [sendFail, t],
  );

  const queue = useSendQueue(deliver, onQueueGiveUp);

  /*
   * The queue state is refetched following streaming and runId.
   *
   * **The frontend no longer decides when to send**: the server drains the queue head itself when
   * a turn ends
   * (server/core/pendingmessage.go). But after draining it notifies nobody, so without refetching
   * the message already sent would keep hanging on the queue UI, while it is long since in the
   * conversation.
   *
   * Both directions must refetch:
   *   - streaming false → true: the server may have just drained one, so the queue is one shorter;
   *   - streaming true → false: this turn ended, and the server will drain the next one right away.
   * `runId` is in the deps: switching conversations must switch to that conversation's queue
   * (otherwise it shows the previous one's).
   */
  useEffect(() => {
    if (!runId) return;
    queue.refresh(runId);
    // queue is a new object on every render (the hook returns a literal), but refresh is
    // useCallback-stable;
    // putting the whole queue in deps would run this effect every frame, so only that one method is
    // taken.
    //
    // ⚠️ `stream.events.length` in the deps must not be removed. After the server drains one, it
    // pushes it down as
    // a user.message, and **that is the only signal the frontend can observe for "the queue is one
    // shorter"**;
    // the server sends no other notice. Depending only on streaming, with two queued in a row the
    // second would keep hanging on
    // the queue UI (streaming did not change when the first was drained), while it was actually
    // sent.
  }, [stream.streaming, stream.events.length, runId, queue.refresh]);

  async function send(text: string) {
    sendFail.clear();
    setQueueNotice(null);
    let targetId = runId;
    try {
      // No conversation yet: create one first, then switch the URL over.
      if (!targetId) {
        const created = await api.createConversation(workspace.id, agentId);
        targetId = created.id;
        setConversations((prev) => [created, ...prev]);
        navigate(`/w/${workspace.key}/chat/${created.id}`, { replace: true });
      }

      /*
       * When to queue rather than send directly. Two conditions; **the first is the normal case**:
       *
       *   1. **This turn is still running** (`stream.streaming`). That is the case when the user
       *      says a second thing in a row,
       *      and Ark cannot accept a new user.message while the agent holds the session anyway.
       *      Earlier this condition was missing, relying on "send directly first, queue after
       *      hitting a 409"; but that path required
       *      the composer to be usable during streaming, and it was disabled by `!streaming` back
       *      then, so
       *      **neither end worked: it could neither send nor queue**. The user's text was stuck in
       *      the box with no explanation.
       *   2. Someone is already queued in this conversation → go to the tail, no jumping ahead
       *      (otherwise the two messages would swap order).
       *
       * Why keep both: `streaming` is inferred by the frontend from the event stream, and it lags
       * behind the truth by nature
       * (during tool execution Ark pushes status_idle, and the frontend thinks it is idle; that is
       * exactly what causes the 409 window).
       * So the streaming check is the fast path for "no need to wait for a 409 most of the time",
       * and the 409 in catch is still the fallback; the two do not overlap.
       */
      if (stream.streaming || queue.pendingFor(targetId) > 0) {
        await enqueueOrWarn(targetId, text);
        return;
      }

      const flight = deliver(targetId, text);
      inflightSend.current = flight;
      try {
        await flight;
      } finally {
        // Only clear it if it is still our own send, otherwise we would wipe a later send's record.
        if (inflightSend.current === flight) inflightSend.current = null;
      }
    } catch (e) {
      /*
       * "The previous tool is still running" is not a fault, but a collision with a window of tens of
       * milliseconds (backend 409 + code=conflict;
       * see arkFailure in server/core/chat.go). **Queue and resend automatically; do not show it to
       * the user.**
       *
       * ⚠️ The check is code, not HTTP 409, and not matching that Chinese text; see isSendConflict in
       * lib/useSendQueue.ts.
       *
       * targetId is always non-empty (getting here it either already existed or was just created),
       * but its type is
       * `string | undefined`; when the create step itself fails targetId is still empty, and such
       * errors
       * have nothing to do with the tool window, so falling into sendFail below is right.
       */
      if (targetId && isSendConflict(e)) {
        /*
         * ⚠️ **This takes (A) local resend, not (B) queueing**: both used to land on enqueue,
         * and after the split they must be separate, otherwise users would see a "queued" message
         * vanish by itself tens of milliseconds later.
         *
         * The reason is that this window is only tens of milliseconds (measured 48ms in production),
         * and the user expects "right now";
         * while the queue waits for **a whole turn** to end, three orders of magnitude apart. Putting
         * it into the queue would turn an
         * invisible retry into a visible wait.
         */
        queue.sendThroughWindow(targetId, text);
        return;
      }
      // fail() does three things at once: classify, silently report first level, store in state for
      // InlineError to render.
      sendFail.fail(e);
    }
  }

  /**
   * Enqueue; when full, give a clear hint and hand the text back to the input.
   *
   * **Must not drop silently**: that is the worst way this feature can fail; the user thinks it was
   * sent,
   * then waits for a reply that will never come.
   */
  async function enqueueOrWarn(targetId: string, text: string) {
    // Enqueueing is now a network round trip (the server queue). Failures come in two kinds: full is
    // an **expected refusal**,
    // hand back the text + hint; the rest are real faults, using the existing two-level feedback.
    try {
      if (await queue.enqueue(targetId, text)) return;
    } catch (e) {
      setRefill((prev) => ({ text, token: (prev?.token ?? 0) + 1 }));
      sendFail.fail(e);
      return;
    }
    setRefill((prev) => ({ text, token: (prev?.token ?? 0) + 1 }));
    setQueueNotice(t("chat.queueFull", { limit: SEND_QUEUE_LIMIT }));
  }

  /**
   * Interrupt this turn.
   *
   * **Must wait for the in-flight send to land first.** `markStreaming()` is optimistic: it
   * lights the "interrupt" button before `whenSubscribed` + `sendMessage`, so there is a window;
   * if the user (or e2e) clicks interrupt in that window, `interruptRun` **runs before
   * `sendMessage`**,
   * and `user.interrupt` lands before that user.message: the turn looks "interrupted first,
   * then started", while the sentence meant to be interrupted runs to the end as usual.
   *
   * That window was always there (create conversation → change URL → fetch history, measured
   * ~80ms), but the interrupt button used to wait
   * for streaming to really be set before appearing, so it was hard to hit; now both buttons
   * coexist and light up earlier, so it became
   * a reliably reproducible ordering bug. Awaiting the in-flight send before interrupting always
   * gives the right order.
   */
  async function interrupt() {
    if (!runId) return;
    try {
      // Let the in-flight send land first, so the interrupt does not run ahead of it (see above).
      // When the send itself fails this should not also report an error:
      // that error was already handled by send()'s catch, so swallow it.
      await inflightSend.current?.catch(() => {});
      await api.interruptRun(runId);
    } catch (e) {
      sendFail.fail(e);
    }
  }

  /**
   * Send a queued message now: interrupt the current turn and say this instead (**redirect**).
   *
   * # The opposite of "queue"
   *
   * Queue = wait for the previous turn to finish then send; redirect = stop waiting and interrupt
   * the previous turn. So this **cannot** reuse
   * the queue's deliver: that path holds back while `canSendNow` is false, and "the current turn is
   * still running"
   * is exactly the premise of the user clicking this button, so the message would stay put and the
   * button would seem to do nothing.
   *
   * # Dequeue first; on failure put it back in the input
   *
   * Dequeue before sending: otherwise after a successful redirect the same message would still be
   * in the queue, and be **sent again** when the next turn ends.
   * And on failure the user's words are not silently dropped: the text goes back into the input,
   * the same handling as when the queue gives up.
   *
   * ⚠️ **The optimistic streaming flag must be reset on the failure path.** markStreaming turns the
   * composer into
   * "interrupt" at once, while this turn never started, and no event will ever come to clear it;
   * without the reset the composer
   * stays stuck on "interrupt" forever, and the user cannot send any message. This is the same rule
   * as the clearStreaming in deliver
   * (the event that would clear it does not exist, precisely because of the failure).
   */
  async function sendNow(id: string) {
    if (!runId) return;
    const text = await queue.take(id);
    // take returns null = this one was just drained and sent by the server. Then there is nothing
    // to redirect,
    // and the text **must not** be sent again, which would give two identical messages.
    if (text === null) return;

    sendFail.clear();
    setQueueNotice(null);
    /*
     * ⚠️ The guard "do not let the queue head slip in during the interrupt's idle gap" **now lives
     * on the server**
     * (drainAllowed in core/pendingmessage.go excludes user_interrupt).
     *
     * There used to be a redirecting ref here for this; once the queue moved to the server it could
     * no longer control it:
     * the one deciding "when to send the next" is no longer the browser. Keeping it would only make
     * people think the frontend still guards it.
     */
    stream.markStreaming(runId);
    try {
      // Wait for the subscription before sending, for exactly the same reason as in deliver: Ark
      // subscriptions do not replay,
      // and in the wrong order this turn's reply is lost for good.
      await stream.whenSubscribed(runId);
      const event = await api.redirectRun(runId, text);
      stream.appendLocal(event);
      await reload();
    } catch (e) {
      // See above: the optimistic flag must be reset by hand.
      stream.clearStreaming();
      // Hand the text back to the input; do not let the user's typed words vanish because we did not
      // send them.
      setRefill((prev) => ({ text, token: (prev?.token ?? 0) + 1 }));
      sendFail.fail(e);
    }
  }

  if (loadError) {
    return (
      <div className="mx-auto w-full max-w-6xl px-8 pt-8">
        <AsyncErrorBlock error={loadError} onRetry={() => void reload()} />
      </div>
    );
  }

  if (!agents) {
    return (
      <div className="flex h-full flex-col">
        <LoadingBlock />
      </div>
    );
  }

  // With no agents at all, the chat page has nothing to do: send the person straight to create one.
  if (agents.length === 0) {
    return (
      <div className="mx-auto w-full max-w-6xl px-8 pt-8">
        <Empty
          icon="gravity-ui:comments"
          title={t("chat.noAgents")}
          description={t("chat.noAgentsDesc")}
          action={
            <Button onPress={() => navigate(`/w/${workspace.key}/agents/new`)}>
              {t("chat.createAgent")}
            </Button>
          }
        />
      </div>
    );
  }

  const sidebar = (
    <ChatSessionsSidebar
      conversations={conversations}
      activeRunId={runId ?? null}
      onSelect={(id) => navigate(`/w/${workspace.key}/chat/${id}`)}
      onNew={() => navigate(`/w/${workspace.key}/chat`)}
    />
  );

  return (
    <div className="flex h-full">
      {sidebar}

      <div className="flex min-w-0 flex-1 flex-col">
        {/* Conversation title bar: only context and the entry to run detail, no repeated app-level info. */}
        <div className="flex items-center gap-3 border-b border-border px-4 py-3">
          <ChatSessionsTrigger
            conversations={conversations}
            activeRunId={runId ?? null}
            onSelect={(id) => navigate(`/w/${workspace.key}/chat/${id}`)}
            onNew={() => navigate(`/w/${workspace.key}/chat`)}
          />
          <div className="flex min-w-0 flex-col">
            <span className="truncate text-sm font-medium">
              {activeRun?.title || t("chat.title")}
            </span>
            {agentName ? (
              <span className="flex items-center gap-1.5 truncate text-xs text-muted">
                {t("chat.startedWith", { name: agentName })}
                {/* **The two freeze causes need two markers.** This used to be `frozen ?
                    <AgentDeletedChip />`, so disabled conversations were labeled "deleted";
                    users would go create a new agent, while the original's skills and Feishu bindings were all still there.
                    Deleted wins: when both are true, "deleted" is the more final fact. */}
                {activeRun?.agentDeleted ? (
                  <AgentDeletedChip />
                ) : activeRun?.agentDisabled ? (
                  <AgentDisabledChip />
                ) : null}
              </span>
            ) : null}
          </div>
          {runId ? (
            <div className="ml-auto flex shrink-0 items-center gap-3">
              <ViewSwitch view={view} onChange={setView} />
              <Link
                to={`/w/${workspace.key}/runs/${runId}`}
                className="text-xs text-muted no-underline hover:text-foreground"
              >
                {t("chat.viewRun")}
              </Link>
            </div>
          ) : null}
        </div>

        {runId ? (
          view === "events" ? (
            // The event view is its own scroll area (the chat page is a fixed-height layout as a
            // whole, unlike the run detail page's page-level scrolling).
            // Wide screens show list + detail in two columns, so it widens to max-w-6xl here (the
            // conversation view stays 3xl).
            <div className="flex-1 overflow-y-auto px-4 pt-4 pb-6">
              <div className="mx-auto flex w-full max-w-6xl flex-col gap-4">
                <TimelineToolbar
                  filter={filter}
                  stats={toolbarCounts}
                  threads={activeRun?.threads ?? []}
                  isDirty={isDirty}
                  onToggleCategory={filterActions.toggleCategory}
                  onToggleDomain={filterActions.toggleDomain}
                  onSetTypes={filterActions.setTypes}
                  onApplyPreset={filterActions.applyPreset}
                  onReset={filterActions.reset}
                  onQueryChange={filterActions.setQuery}
                  onSlowOnlyChange={filterActions.setSlowOnly}
                  onErrorsOnlyChange={filterActions.setErrorsOnly}
                  onThreadChange={filterActions.setThreadId}
                />
                <RawEventList
                  rows={eventRows}
                  showThread={
                    (activeRun?.threads.length ?? 0) > 1 &&
                    filter.threadId === null
                  }
                />
              </div>
            </div>
          ) : view === "trace" ? (
            /* The call tree is its own view, no longer hung at the end of the event view (see the comment on ViewSwitch).
               This layer is the scroll area (the chat page is fixed height as a whole, unlike the run detail page's page-level scrolling),
               and the panel has no scroll wrapper of its own, so this layer carries it (see the comment on TracePanel).
               Use allItems rather than eventRows: the call tree should not be affected by the event view's filter. */
            <div className="flex-1 overflow-y-auto px-4 pt-4 pb-6">
              <div className="mx-auto flex w-full max-w-3xl flex-col gap-4">
                <TracePanel items={allItems} />
              </div>
            </div>
          ) : view === "files" || view === "terminal" ? (
            /* Files and terminal share one panel. Same scrolling arrangement as the call tree:
               this layer is the scroll area, and the panel has no wrapper of its own. */
            <div className="flex-1 overflow-y-auto px-4 pt-4 pb-6">
              <div className="mx-auto flex w-full max-w-3xl flex-col gap-4">
                <SandboxPanel runId={runId} mode={view} hostId={activeRun?.hostId} />
              </div>
            </div>
          ) : (
            <ChatStream
              events={stream.events}
              draft={stream.draft}
              thinkingDraft={stream.thinkingDraft}
              thinking={stream.thinking}
              agentName={agentName}
            />
          )
        ) : (
          <div className="flex flex-1 items-center justify-center px-4">
            <Empty
              icon="gravity-ui:comments"
              title={t("chat.pickAgentTitle")}
              description={t("chat.pickAgentDesc")}
            />
          </div>
        )}

        {/* The freeze note sits right above the composer: that is where the user finds "I cannot type".
            **Two causes, two sets of wording**: deleted can only be recreated, disabled just needs the status switched back (see messages.ts).
            agentDeleted takes priority: when both are true (the agent was disabled and then deleted)
            "deleted" is the more final fact. */}
        {frozen ? (
          <div className="px-4 pb-2" data-testid="chat-frozen">
            <Notice
              icon="gravity-ui:triangle-exclamation"
              title={
                activeRun?.agentDeleted
                  ? t("chat.frozenTitle")
                  : t("chat.frozenDisabledTitle")
              }
              hint={
                activeRun?.agentDeleted
                  ? t("chat.frozenHint")
                  : t("chat.frozenDisabledHint")
              }
            />
          </div>
        ) : null}

        {/* Up-front guide when no Ark credentials are configured. **Placed right above the composer**, the same place as the freeze note:
            that is where the user finds "I cannot type" (see the in-file comments of useArkGate).
            Two cases, two sets of wording, switched by permission: people who can configure get a button, people who cannot get "whom to ask". */}
        {arkBlocked ? (
          <div className="px-4 pb-2" data-testid="chat-no-ark">
            <Notice
              icon="gravity-ui:key"
              title={
                arkGate.state === "inheritGone"
                  ? t("chat.noArkInheritTitle")
                  : t("chat.noArkTitle")
              }
              hint={
                // People without permission: do not say "where to configure", only "whom to ask";
                // going there would just 403.
                canManage === false
                  ? t("chat.noArkNeedAdmin")
                  : arkGate.state === "inheritGone"
                    ? t("chat.noArkInheritHint")
                    : t("chat.noArkHint")
              }
              action={
                /* ⚠️ When `canManage === null` (not known yet) **no button is given**, the same as
                   `canManage === false`. Otherwise a viewer would first see a clickable
                   button that disappears 90ms later; a flashing entry is more confusing than no entry,
                   and the cost of this choice is only an admin waiting one extra network round trip. */
                canManage === true ? (
                  arkGate.state === "inheritGone" ? (
                    /* The referenced global key is gone → go to **global settings**.
                       ⚠️ Must carry `?from=<key>`: without it is the shell-less form, and the whole sidebar disappears
                       (see the comments on the two redirects in SettingsPage). */
                    <Button
                      size="sm"
                      variant="outline"
                      data-testid="chat-no-ark-action"
                      onPress={() =>
                        navigate(`/preferences?tab=ark&from=${workspace.key}`)
                      }
                    >
                      {t("chat.noArkInheritAction")}
                    </Button>
                  ) : (
                    <Button
                      size="sm"
                      variant="outline"
                      data-testid="chat-no-ark-action"
                      onPress={() =>
                        navigate(`/w/${workspace.key}/settings?tab=ark`)
                      }
                    >
                      {t("chat.noArkAction")}
                    </Button>
                  )
                ) : undefined
              }
            />
          </div>
        ) : null}

        {/* The conversation no longer exists (the URL is stale). Placed above the composer, the same place as the freeze note. */}
        {stream.error === STREAM_GONE ? (
          <div className="px-4 pb-2" data-testid="chat-gone">
            <Notice
              icon="gravity-ui:triangle-exclamation"
              title={t("chat.goneTitle")}
              hint={t("chat.goneHint")}
            />
          </div>
        ) : null}

        {sendFail.error ? (
          <div className="px-4 pb-1">
            <InlineError
              error={sendFail.error}
              compact
              prefix={t("chat.sendFailed")}
              testId="chat-error"
              feedbackContext={{ workspaceId: workspace.id, agentId: agentId || undefined }}
            />
          </div>
        ) : null}

        {/* Note for a full queue / timing out in line. **Not a fault**, so it uses a neutral info tone rather than red text;
            see where queueNotice is declared. */}
        {queueNotice ? (
          <div className="px-4 pb-2">
            <Notice
              tone="info"
              icon="gravity-ui:circle-info"
              title={queueNotice}
              data-testid="chat-queue-notice"
            />
          </div>
        ) : null}

        {/* Queued messages. Only those of the **current conversation** are shown: the queue is grouped by runId,
            and switching to another conversation should not show this one's pending messages (that would look like it was sent to the wrong place). */}
        <SendQueueList
          items={queue.items.filter((it) => it.runId === runId)}
          onCancel={(id) => void queue.cancel(id)}
          onSendNow={(id) => void sendNow(id)}
          onEdit={(id) => {
            void queue.take(id).then((text) => {
              // take returns null = this one was just drained and sent by the server. Then the text
              // **must not** be put back into
              // the input, otherwise the user thinks it was not sent and sends it again.
              if (text === null) return;
              setRefill((prev) => ({ text, token: (prev?.token ?? 0) + 1 }));
            });
          }}
        />

        {/* The terminal view gets no composer.
            There is already a real terminal waiting for the keyboard; another "say something…" below would make people unsure
            whether what they type goes to the agent or into the container, and the two have very different consequences.
            Same for the files view: it is read-only browsing, with nothing to send. */}
        {view !== "terminal" && view !== "files" && (
          <ChatComposer
            agents={agents}
            agentId={agentId}
            onAgentChange={setAgentId}
            agentLocked={Boolean(runId)}
            onSend={(text) => void send(text)}
            onInterrupt={() => void interrupt()}
            streaming={stream.streaming}
            // Also disabled when credentials are not configured: **do not let people type a whole
            // message only to be pushed back by a 422**, which is exactly what this fixes
            // (see useArkGate). Independent from freezing; either one disables it, hence ||.
            isDisabled={frozen || arkBlocked}
            agentFallbackName={agentName}
            refill={refill}
          />
        )}
      </div>
    </div>
  );
}

import { test as base, expect, type Page } from "@playwright/test";

/**
 * Before each case, reset the mock store to the seed data so cases do not affect each other.
 *
 * The reset must happen after the page loads (the store hangs on window), so visit the start page
 * first and then reset.
 */
export const test = base.extend<{ app: Page }>({
  app: async ({ page }, use) => {
    await page.goto("/workspaces");
    await page.evaluate(() => window.__amaApi?.reset());
    await use(page);
  },
});

/**
 * Make read APIs never return, then reload: used to check the **loading** branch (skeleton).
 *
 * Reload is required: just setting the marker does not bring an already loaded page back to loading.
 *
 * Why it is needed: `isLoading` and "no data" are two things, and pages must handle them separately.
 * But the mock's delay is only 90ms, and without pinning it the assertions always run on "loaded", so a
 * mistake like "treating isLoading as the empty state" stays all green: verified.
 */
export async function hangReadsMock(page: Page): Promise<void> {
  await page.evaluate(() => window.__amaApi?.hangReads?.());
}

/**
 * Slow down each of the two full-page startup gates (AuthGate fetching identity, WorkspaceGate
 * fetching the workspace) by `ms` milliseconds.
 *
 * Used to check "when waiting exceeds the quiet period, the spinner really appears". The mock defaults
 * to 90ms, **below the 300ms quiet period**, so without slowing it down the spinner never appears: that
 * is how the fast path should look, but it also means the slow path cannot be tested at all (deleting
 * the spinner stays all green).
 *
 * ⚠️ **clearSlowGatesMock must be called in afterEach to restore it**, same as clearSandboxStatusMock:
 * workers=1 with localStorage shared serially, and leaving it would make every later spec's startup wait
 * that much longer, showing up as scattered timeouts with errors that do not point to this.
 */
export async function slowGatesMock(page: Page, ms: number): Promise<void> {
  await page.evaluate((n) => window.__amaApi?.slowGates?.(n), ms);
}

/** Undo slowGatesMock. */
export async function clearSlowGatesMock(page: Page): Promise<void> {
  await page.evaluate(() => window.__amaApi?.clearSlowGates?.());
}

/**
 * Make the mock pretend the Ark-side agent / skill has already been created (so "open in the Ark
 * console" has an entry).
 *
 * Why it is required: Ark resources are **created lazily** (only when a chat is really started), and the
 * mock has no Ark, so the default state is "not created yet": that is how the default should be, and
 * what most users see. But then "the entry should appear when there is an id" could never be tested, and
 * a missing entry would never be noticed.
 *
 * ⚠️ **disableArkConsoleMock must be called in afterEach to restore it**, same as clearSandboxStatusMock:
 * workers=1 with localStorage shared serially, and leaving it would make later specs run on the
 * "already created" branch.
 */
export async function enableArkConsoleMock(
  page: Page,
  region: "cn-beijing" | "ap-southeast-1" = "cn-beijing",
): Promise<void> {
  await page.evaluate((r) => window.__amaApi?.enableArkConsoleMock?.(r), region);
}

/** Undo enableArkConsoleMock. */
export async function disableArkConsoleMock(page: Page): Promise<void> {
  await page.evaluate(() => window.__amaApi?.disableArkConsoleMock?.());
}

/**
 * Make the mock pretend the container is in some state (none / running / paused / gone).
 *
 * Why it is required: of the four states only `none` is the natural default (the mock has no Docker).
 * The other three can only be reproduced in a real environment by **waiting 60 seconds for a pause, or
 * an hour for recycling**, and their UIs are exactly the easiest to get wrong (showing "paused" as
 * "recycled" and the like).
 *
 * ⚠️ **clearSandboxStatusMock must be called in afterEach to restore it**, same as clearSlowGatesMock.
 */
export async function setSandboxStatusMock(
  page: Page,
  status: "none" | "running" | "paused" | "gone",
): Promise<void> {
  await page.evaluate((s) => window.__amaApi?.setSandboxStatusMock?.(s), status);
}

/** Undo setSandboxStatusMock. */
export async function clearSandboxStatusMock(page: Page): Promise<void> {
  await page.evaluate(() => window.__amaApi?.clearSandboxStatusMock?.());
}

/**
 * Make terminal upstream (the POST for each keystroke) always fail.
 *
 * ⚠️ failWritesMock cannot be used instead: the mock's writeRunSandboxTerminal does not go through
 * guardWrite, so that switch has no effect on it, and using it gives an always-green case. See
 * FAIL_TERMINAL_INPUT_KEY in api.ts.
 */
export async function failTerminalInputMock(page: Page): Promise<void> {
  await page.evaluate(() => window.__amaApi?.failTerminalInputMock?.());
}

/** Undo failTerminalInputMock. **Cases must call it in afterEach** (reset cannot wipe it). */
export async function clearTerminalInputMock(page: Page): Promise<void> {
  await page.evaluate(() => window.__amaApi?.clearTerminalInputMock?.());
}

/**
 * Make the mock pretend "fetch from Volcengine account" is in some state (ready / unavailable /
 * unauthorized / error).
 *
 * Why it is required: the default is `ready` (connected and authorized, so local development can walk
 * the full flow); the other three states **can never be reached naturally** in the mock: they need an
 * admin who did not configure app_key, a person who never authorized, or an upstream that happens to be
 * down. And their UIs are exactly the easiest to get wrong:
 *
 *   unavailable  **no "authorize" button** (clicking gives only a 422, and users cannot know to find an admin)
 *   unauthorized gives the authorization entry
 *   error        retryable, and the error must point to the real cause
 *   loading      these three APIs never return -> skeleton (**not** an empty list)
 *
 * "Not connected != not authorized" was hit once on the authorization page (canAuthorize in IntegrationsTab).
 *
 * `loading` cannot be replaced by hangReadsMock: that switch also pins getArkCredential, so the whole Ark
 * card stays on the skeleton and **the dialog cannot open at all**, while the picker's loading state is
 * inside the dialog.
 *
 * ⚠️ **clearArkFetchStateMock must be called in afterEach to restore it**, same as setSandboxStatusMock:
 * workers=1 with localStorage shared serially, and a leftover marker would make all later specs run on
 * that branch, with failure messages that do not point to this.
 */
export async function setArkFetchStateMock(
  page: Page,
  state: "ready" | "unavailable" | "unauthorized" | "error" | "loading",
): Promise<void> {
  await page.evaluate((s) => window.__amaApi?.setArkFetchStateMock?.(s), state);
}

/** Undo setArkFetchStateMock. */
export async function clearArkFetchStateMock(page: Page): Promise<void> {
  await page.evaluate(() => window.__amaApi?.clearArkFetchStateMock?.());
}

/** Clear all data, to check the forced guidance "a workspace must be created first". */
export async function clearAll(page: Page): Promise<void> {
  await page.goto("/workspaces");
  await page.evaluate(() => window.__amaApi?.clear());
}

/**
 * Read all problem feedback the mock received.
 *
 * ⚠️ **Reporting is not awaited** (level one does `void` in useAsyncData's catch, level two gives a
 * receipt on click), so reading right after triggering may get an empty array. Wait with `expect.poll`,
 * do not call it bare: a bare call's failure message is "expected 1, got 0", which cannot point to the
 * real cause "it has not landed yet".
 */
export async function feedbackReports(page: Page): Promise<
  {
    level: string;
    routePattern: string;
    errorCode: string;
    httpStatus: number;
    occurrences?: number;
    path?: string;
    agentId?: string;
    recent?: { logId?: string }[];
    consoleErrors?: unknown[];
    logId?: string;
  }[]
> {
  return (await page.evaluate(() => window.__amaApi?.feedbackReports?.() ?? [])) ?? [];
}

/**
 * Clear the feedback the mock received.
 *
 * **Every feedback-related case must call it at the start.** Feedback lives in a separate localStorage
 * key (so it can be cleared on its own), and the fixture's `reset()` only rebuilds the STORAGE_KEY store,
 * which cannot wipe it. Without clearing, the previous case's reports would turn count assertions like
 * "only 1 was produced this time" red on the second run.
 */
export async function clearFeedbackMock(page: Page): Promise<void> {
  await page.evaluate(() => window.__amaApi?.clearFeedback?.());
}

/**
 * Switch the mock to logged out, then reload.
 *
 * Both are required:
 *
 * 1. the mock is always logged in by default (see the "login" part of data/api.ts); without this marker
 *    you can never reach the landing page;
 * 2. **reload is required**: AuthGate only probes the login state once on mount, so just setting the
 *    marker does not change the current page.
 *
 * The marker lives in a separate localStorage key, so reset() / clear() do not wipe it.
 */
export async function signOutMock(page: Page): Promise<void> {
  await page.goto("/workspaces");
  await page.evaluate(() => window.__amaApi?.signOut?.());
  await page.reload();
}

/**
 * Make the mock's current user "have no avatar", then reload.
 *
 * Same shape as signOutMock (including the required reload: AuthGate only probes the login state once
 * on mount). Why not use page.route to intercept /api/auth/me: e2e runs with VITE_API_MODE=local, and
 * LocalApiClient sends no network requests, so there is nothing to intercept: the case would quietly run
 * on the "has avatar" path and then fail at an assertion, with the failure far from the real cause.
 */
export async function hideAvatarMock(page: Page): Promise<void> {
  await page.goto("/workspaces");
  await page.evaluate(() => window.__amaApi?.hideAvatar?.());
  await page.reload();
}

/**
 * Make list read APIs start failing, to check the page's error state (instead of a permanent skeleton).
 *
 * Passing `code` as `access_denied` simulates 403 (no access, no "retry" should be given); other values
 * simulate retryable faults. Same shape as hideAvatarMock, including the **required reload**: pages only
 * fetch data once on mount, so just setting the marker does not change the current page.
 *
 * Why not intercept with page.route: e2e runs with VITE_API_MODE=local, and LocalApiClient sends no
 * network requests, so there is nothing to intercept: the case would quietly run on the success path and
 * then fail at an assertion, with the failure far from the real cause. The marker lives in a separate
 * localStorage key that reset() cannot wipe.
 */
export async function failReadsMock(page: Page, code?: string): Promise<void> {
  await page.evaluate((c) => window.__amaApi?.failReads?.(c), code);
}

/** Undo failReadsMock, so read APIs work normally again. */
export async function restoreReadsMock(page: Page): Promise<void> {
  await page.evaluate(() => window.__amaApi?.restoreReads?.());
}

/**
 * Make **write** APIs fail (send message, create agent, save settings…), 500 by default.
 *
 * # Why it is separate from failReadsMock
 *
 * `failReadsMock` keeps the page from opening at all (list loading fails, and a whole error block
 * renders), while write failures need to check **a completely different path**: the page is fine, and
 * an action the user clicked did not succeed. With one merged switch, write-failure cases would stop at
 * the read-failure step and test nothing.
 *
 * # The regression it guards against
 *
 * After one-click feedback launched, the `feedback_reports` table was empty for a whole day: level one
 * only hung off read failures (the catch in `useAsyncData`), and **write failures recorded nothing and
 * had no level-two entry either**. It was not caught back then exactly because this switch did not
 * exist: the 7 cases in 16-feedback all used failReadsMock, so the question "should a write failure be
 * reported" was never asked in tests.
 *
 * `restoreWritesMock` must be called after use (the marker lives in a separate key that `reset()` cannot wipe).
 */
export async function failWritesMock(page: Page, code?: string): Promise<void> {
  await page.evaluate((c) => window.__amaApi?.failWrites?.(c), code);
}

/** Undo failWritesMock, so write APIs work normally again. */
export async function restoreWritesMock(page: Page): Promise<void> {
  await page.evaluate(() => window.__amaApi?.restoreWrites?.());
}

/**
 * Make only **redirect** ("send now" on a queued message) fail; other writes work as usual.
 *
 * # Why not failWritesMock
 *
 * That switch breaks all writes together, **including the queue's own resend**. So at the moment of
 * clicking "send now", the message being retried in the queue also fails, is judged a real fault and
 * leaves the queue, and the queue box disappears from the DOM together with the button: Playwright
 * reports `element was detached from the DOM`, which looks like UI jitter but is really the mock hitting
 * too wide (measured: two runs, one red one green).
 *
 * More importantly, it makes assertions **pass for the wrong reason**: the text really is back in the
 * input box, but that was done by the queue's fill-back when giving up, not by the redirect failure
 * path: even with sendNow's catch deleted entirely, the case might still be green.
 *
 * `restoreRedirectMock` must be called after use (the marker lives in a separate key that `reset()` cannot wipe).
 */
export async function failRedirectMock(page: Page, code?: string): Promise<void> {
  await page.evaluate((c) => window.__amaApi?.failRedirect?.(c), code);
}

/** Undo failRedirectMock. */
export async function restoreRedirectMock(page: Page): Promise<void> {
  await page.evaluate(() => window.__amaApi?.restoreRedirect?.());
}

/**
 * Seed a few attachments with content for a skill, so the detail page's file list and content area are testable.
 *
 * In mock mode this is the **only** way to create attachments: none of the 4 skills in the seed has
 * `files` (the Go-side TestSeedRoundTrip compares seed.json byte for byte, and one extra `"files":[]`
 * turns it red), the upload path deliberately drops content, and the mock does not unpack zips at all.
 *
 * To create a binary file: put a `"\u0000"` in the content (the same test as the backend core.isBinary).
 *
 * ⚠️ **`clearSkillFilesMock` must be called after use**: the content half lives in a separate key, and
 * the `reset()` before each case **cannot wipe it** (on purpose, or it would be cleared right after
 * seeding); the cost is that without clearing it leaks into the next case.
 */
export async function seedSkillFilesMock(
  page: Page,
  skillId: string,
  files: { path: string; content: string }[],
): Promise<void> {
  await page.evaluate(
    ([id, fs]) =>
      window.__amaApi?.seedSkillFilesMock?.(
        id as string,
        fs as { path: string; content: string }[],
      ),
    [skillId, files] as const,
  );
}

/** Undo seedSkillFilesMock. */
export async function clearSkillFilesMock(page: Page): Promise<void> {
  await page.evaluate(() => window.__amaApi?.clearSkillFilesMock?.());
}

/**
 * Create "this schedule has already run a few rounds", so the detail page's run history and counters are testable.
 *
 * The mock **has no scheduler**; `runScheduleNow` only sets nextRunAt to now and returns, while real
 * execution is taken over by the background Scheduler in production. So those detail page branches can
 * only be created with this hook.
 *
 * ⚠️ The data lives in the store that `reset()` wipes (unlike seedSkillFilesMock), so it **does not
 * need** cleanup in afterEach: the reset before the next case clears it along the way.
 *
 * Passing `counters.runCount` larger than `runs.length` creates the "earlier history cannot be found" gap.
 */
export async function seedScheduleHistoryMock(
  page: Page,
  scheduleId: string,
  runs: { status?: string; startedAt?: string; errorSummary?: string }[],
  counters?: { runCount?: number; failCount?: number; consecutiveFailures?: number },
): Promise<void> {
  await page.evaluate(
    ([id, rs, cs]) =>
      window.__amaApi?.seedScheduleHistoryMock?.(
        id as string,
        rs as { status?: string; startedAt?: string; errorSummary?: string }[],
        cs as { runCount?: number; failCount?: number; consecutiveFailures?: number },
      ),
    [scheduleId, runs, counters] as const,
  );
}

/**
 * Append a platform-injected block to the first `user.message` of a run, so the "platform injected"
 * collapsible is testable.
 *
 * The real source is Feishu inbound (`server/feishu/larkprompt.go`), and the mock has no Feishu; it
 * cannot be added to the seed either (`seed.json` is compared byte for byte on the Go side).
 *
 * ⚠️ The data lives in the store that `reset()` wipes, so it **does not need** afterEach cleanup.
 */
export async function seedInjectedContextMock(
  page: Page,
  runId: string,
  text?: string,
): Promise<void> {
  await page.evaluate(
    ([id, txt]) => window.__amaApi?.seedInjectedContextMock?.(id as string, txt as string),
    [runId, text] as const,
  );
}

/**
 * Make the next `times` sends hit "the previous tool is still running" (409 + code=conflict), then
 * succeed naturally. Pass `-1` to always hit it.
 *
 * # Why it is separate from failWritesMock
 *
 * That one creates a **permanent** failure (500 / 403), which the frontend should show to the user; this
 * one creates the exact opposite: a race that fixes itself within a few dozen milliseconds (under
 * self_hosted, the window between Ark issuing tool_use and the worker returning tool_result, measured at
 * 48 milliseconds), which the frontend should quietly queue and resend. The two reactions are completely
 * different.
 *
 * With one merged switch, the "resend succeeds" half could not be tested at all: with an always-failing
 * mock the queue would just keep retrying until timeout, which is the give-up branch, not the success branch.
 *
 * `clearConflictSendsMock` must be called after use (the marker lives in a separate key that `reset()`
 * cannot wipe): missing it once makes every later send case hit the window first.
 */
export async function conflictSendsMock(page: Page, times = 1): Promise<void> {
  await page.evaluate((n) => window.__amaApi?.conflictSends?.(n), times);
}

/** Undo conflictSendsMock. */
export async function clearConflictSendsMock(page: Page): Promise<void> {
  await page.evaluate(() => window.__amaApi?.clearConflictSends?.());
}

/**
 * Make this turn **stay "in progress"** (the agent is still answering), to test the main path of the send queue.
 *
 * # Why it is needed
 *
 * The most common trigger for queueing is `streaming` being true. And in e2e it **can neither be waited
 * for nor created**:
 *
 *   - the mock replay is a dozen-odd frames x 70ms ≈ 1.4 seconds, and by the second click on send it has
 *     likely already ended, so the message is sent directly and the queue is empty (measured: that flaky,
 *     with the failure message "expected 1, got 0" looking like queueing is broken);
 *   - seed data does not work either: `streaming` is derived from **the event tail** (`isStillRunning`),
 *     and the tails of all six seed runs are `session.status_idle`, even run_4 with `status: "running"`,
 *     which is not streaming when opened.
 *
 * With it set, the replay stops after pushing `session.status_running`, and this turn stays stably at
 * "still answering". `flushConversation()` and clicking interrupt can still end the turn, so the half
 * "the queue sends automatically after the turn ends" is still testable.
 *
 * ⚠️ **releaseTurnMock must be called after use** (the marker lives in a separate key that `reset()`
 * cannot wipe): missing it once makes every later send case stuck at "answering", with failure messages
 * that do not point to this.
 */
export async function holdTurnMock(page: Page): Promise<void> {
  await page.evaluate(() => window.__amaApi?.holdTurn?.());
}

/**
 * Put one item straight into the server queue, **without going through the send flow**.
 *
 * Used to create "one item in the queue, but this turn is not running", a state `holdTurnMock` cannot
 * create (what it holds is exactly "running"), and the test of the case "a redirect failure does not
 * get stuck in the interrupt state" depends on it. See the notes on LocalApiClient.seedPendingMessage.
 */
export async function seedPendingMessageMock(
  page: Page,
  runId: string,
  text: string,
): Promise<void> {
  await page.evaluate(
    ([id, t]) => window.__amaApi?.seedPendingMessage?.(id as string, t as string),
    [runId, text],
  );
}

/** Undo holdTurnMock, and let the held replay finish. */
export async function releaseTurnMock(page: Page): Promise<void> {
  await page.evaluate(() => window.__amaApi?.releaseTurn?.());
}

/**
 * Hold the chat replay at **the thinking stage**: the word-by-word thinking panel is on the page with the
 * full body, but not settled yet.
 *
 * # Why it cannot be done without this (nor replaced by waiting some milliseconds)
 *
 * That panel naturally lives only ~300ms (4 chunks x 70ms), while **Playwright's `expect` retries on a
 * backoff grid** (about 0 / 100 / 350 / 850ms…). Measured, this window falls at 465->764ms after the
 * click, **right between two ticks**:
 *
 *   synthetic comparison (the same assertion, only changing when the element exists, 3 runs each):
 *     465->764ms (299ms wide)   MISSED MISSED MISSED
 *     300->600ms (300ms wide)   CAUGHT CAUGHT CAUGHT
 *     500->850ms (350ms wide)   CAUGHT CAUGHT CAUGHT
 *     850->1150ms (300ms wide)  MISSED MISSED MISSED
 *
 * So it is not even flaky, it is **a consistent miss** (5/5 failures): the window's width does not decide
 * success; which two ticks it falls between does. `waitForTimeout` is no fix either: that ties the
 * assertion to a number of milliseconds that drifts with machine speed, green today and red tomorrow.
 *
 * ⚠️ **releaseStageMock must be called after use** (a module-level marker that `reset()` cannot wipe):
 * missing it once makes every later send case stop at the thinking stage, with failure messages that
 * do not point here.
 */
export async function holdThinkingMock(page: Page): Promise<void> {
  await page.evaluate(() => window.__amaApi?.holdStage?.("thinking"));
}

/** Undo holdThinkingMock, and let the held replay finish. */
export async function releaseStageMock(page: Page): Promise<void> {
  await page.evaluate(() => window.__amaApi?.releaseStage?.());
}

/**
 * Make a workspace 403 for the current user (omit key = all workspaces), simulating a **non-member**.
 *
 * # Why it is separate from failReadsMock
 *
 * `failReadsMock` hits the list APIs (the workspace still opens, testing the page's error state); this
 * one hits `getWorkspaceByKey` (the whole workspace cannot open, testing the gate and the fallback
 * paths). With one merged switch, the 7 cases in 13-load-errors would be stopped by the gate outside the
 * page, while what they check is exactly the error state after the page renders.
 *
 * **Since RBAC launched this is the normal case**: a non-member accessing `workspace-by-key` gets 403
 * directly. It is used to test "someone shares a link with ?from=": without this switch that case would
 * test nothing (the mock always succeeds, and the case quietly runs on the success path).
 *
 * The marker lives in a separate localStorage key that `reset()` cannot wipe, **so it must be cleared
 * after use** (see allowWorkspacesMock): missing it once makes the whole later batch unable to open workspaces.
 */
export async function denyWorkspaceMock(page: Page, key?: string): Promise<void> {
  await page.evaluate((k) => window.__amaApi?.denyWorkspace?.(k), key);
}

/** Undo denyWorkspaceMock. */
export async function allowWorkspacesMock(page: Page): Promise<void> {
  await page.evaluate(() => window.__amaApi?.allowWorkspaces?.());
}

/**
 * Lower "my role in the workspace" to some level, to test the UI **non-admins** see.
 *
 * In the mock the person is always the workspace owner, so the "read-only + admin permission required"
 * branch **never runs even once**: its cases would quietly run on the admin path, and nobody would know
 * if it broke.
 *
 * It is a different switch from denyWorkspaceMock: that one makes the whole workspace unopenable (the
 * gate stops it), while this needs "the page is fine, only the authorization block is read-only".
 *
 * The marker lives in a separate localStorage key that `reset()` cannot wipe, **so it must be cleared
 * after use** (see resetMyRoleMock): missing it once makes the whole later batch think they are not admins.
 */
export async function setMyRoleMock(
  page: Page,
  role: "owner" | "admin" | "member" | "viewer",
): Promise<void> {
  await page.evaluate((r) => window.__amaApi?.setMyRole?.(r), role);
}

/** Undo setMyRoleMock, back to owner. */
export async function resetMyRoleMock(page: Page): Promise<void> {
  await page.evaluate(() => window.__amaApi?.resetMyRole?.());
}

/**
 * Clear all three levels of Ark credentials, back to the initial state "this workspace never configured a key".
 *
 * # Why it is needed
 *
 * The seed now **configures a key by default** (see seedArkCredentials in data/api.ts): otherwise the
 * 30-odd chat cases that do not care about credentials at all would all run into the new pre-check
 * guidance on the chat page (the input box disabled), with failure messages that only say "the input box
 * is disabled", not pointing at all to "the seed has no key".
 *
 * The cost is that the "not configured" branch is unreachable in the seed state, so cases testing it
 * must go back explicitly: the empty state of the workspace settings page, "none of the three levels
 * configured" on the agent detail page, and the pre-check guidance on the chat page all use this one switch.
 *
 * ⚠️ **It must be called before `goto()`, or followed by a reload.** Pages fetch credentials once on
 * mount; clearing after rendering does not change that frame's UI by itself.
 *
 * ⚠️ **The data lives in the store that `reset()` wipes, so no afterEach cleanup is needed**: the
 * opposite of switches like setMyRoleMock that live in separate localStorage keys.
 */
export async function clearArkMock(page: Page): Promise<void> {
  await page.evaluate(() => window.__amaApi?.clearArk?.());
}

/**
 * Turn the mock into "a brand new user" so the onboarding overlay appears, then reload.
 *
 * Both are required, for the same reasons as signOutMock:
 *
 * 1. **The mock defaults to "onboarding already seen"** (see ONBOARDING_KEY in data/api.ts). Without this
 *    marker the overlay never shows. The default is "seen" so as not to disturb the other 100+ cases:
 *    onboarding is a bottom-right overlay, and popping up by default would cover buttons in that area,
 *    making a batch of unrelated cases start "unable to click things".
 * 2. **Reload is required**: OnboardingProvider only fetches the state once on mount, so just setting the
 *    marker does not change the current page.
 *
 * The marker lives in a separate localStorage key, so reset() / clear() do not wipe it.
 * Why not intercept /api/user-onboarding with page.route: e2e runs with VITE_API_MODE=local, and
 * LocalApiClient sends no network requests, so there is nothing to intercept: the case would quietly run
 * on the "seen" path and then fail at an assertion, with the failure far from the real cause.
 */
export async function resetOnboardingMock(page: Page): Promise<void> {
  await page.goto("/workspaces");
  await page.evaluate(() => window.__amaApi?.resetOnboarding?.());
  await page.reload();
}

/**
 * Undo resetOnboardingMock, back to the default "onboarding already seen".
 *
 * **Every case using resetOnboardingMock must call it in afterEach** (workers=1, localStorage shared
 * across cases, and reset() cannot clear this key): otherwise all later cases would run with an
 * onboarding overlay on top.
 */
export async function restoreOnboardingMock(page: Page): Promise<void> {
  await page.evaluate(() => window.__amaApi?.restoreOnboarding?.());
}

/**
 * Set "the server copy" of preferences to some value, or clear it (null). **Does not touch the local cache**.
 *
 * # Why it is needed
 *
 * Preferences now have two storage places: the local cache (`ama.prefs.v1`, prevents first-frame
 * flicker) and the server (authoritative, cross-device). The two things really worth checking are both
 * about **who wins when the two disagree**:
 *
 *   cache has it, server does not  ->  migrate (move this machine's copy up; do not lose it on upgrade)
 *   both have it and differ        ->  the server wins (you changed it on another device)
 *
 * These two scenarios cannot be built with page.route in local mode (LocalApiClient sends no requests),
 * so the mock provides its own switch.
 */
export async function setServerPrefs(
  page: Page,
  prefs: { language: string; appearance: string } | null,
): Promise<void> {
  await page.evaluate((p) => window.__amaApi?.setServerPreferences?.(p), prefs);
}

/** Read "the server copy" of preferences, to assert whether migration really wrote it up. */
export async function readServerPrefs(
  page: Page,
): Promise<{ language: string; appearance: string } | null> {
  return page.evaluate(() => window.__amaApi?.serverPreferences?.() ?? null);
}

/**
 * The number of **platform built-in skills** in this workspace.
 *
 * # Why it exists: do not write the number of built-in skills as a literal
 *
 * Built-in skills are **checked by default** when creating an agent, so any assertion of "create an agent
 * and bind N skills" expects `built-in count + N`. And the built-in count changes: when it went from 2 to
 * 3 (adding `ama`), the hard-coded `toHaveText("1")` in `06-journey` went red, **and that red did not
 * point to the real cause at all**: it reported "the skills count in the summary bar is wrong", which
 * reads like the summary bar is broken, while the product behavior was completely right and the
 * assertion had just pinned a changing quantity as a literal.
 *
 * So the expected value is fetched from the data layer and the case adds its own bound ones, so it
 * follows along the next time built-in skills are added or removed.
 *
 * **Filter by `source === "builtin"`, not by id prefix**: the same test as the frontend `isBuiltinSkill`
 * (source is a contract field; the id shape is a backend implementation detail).
 *
 * ⚠️ **Count the data layer, not the DOM.** Counting the `bind-skill-builtin-*` badges on the page would
 * use the rendering of the code under test as the expected value: the assertion would move together with
 * the bug, always green.
 */
export async function builtinSkillCount(page: Page, workspaceKey: string): Promise<number> {
  return page.evaluate(async (key) => {
    const api = window.__amaApi;
    if (!api) throw new Error("window.__amaApi 不存在：这条用例没跑在 local 模式的产物上");
    const ws = await api.getWorkspaceByKey(key);
    if (!ws) throw new Error(`空间 ${key} 不存在，取不到它的技能清单`);
    const skills = await api.listSkills(ws.id);
    return skills.filter((s) => s.source === "builtin").length;
  }, workspaceKey);
}

/**
 * Simulate the user clicking consent on the SSO authorization page, so the next poll gets succeeded.
 *
 * Same intent as completeFeishuAuth: real authorization can only be clicked by the user on another site,
 * and tests need a deterministic way to move forward.
 */
export async function completeLogin(page: Page): Promise<void> {
  await page.evaluate(() => window.__amaApi?.completeLogin?.());
}

/**
 * Push an in-progress Feishu device code connection straight to "authorized".
 *
 * The real flow needs the user to click consent in Feishu, which e2e can neither do nor should really do.
 * With this hook, tests can deterministically get past the authorization step, instead of sleeping until
 * the mock's poll counter naturally runs out.
 */
export async function completeFeishuAuth(page: Page): Promise<void> {
  await page.evaluate(() => window.__amaApi?.completeFeishuAuth());
}

/**
 * Push a not-yet-finished chat replay all the way to the end at once.
 *
 * Same intent as completeFeishuAuth: word-by-word output is a feature for users and nondeterminism for
 * e2e. With it, cases can assert both the intermediate state (do not call) and the final result (call).
 *
 * **Wait for the replay to exist before pushing.** Between clicking send and the mock setting up the
 * replay there are several awaits (create session -> wait for the subscription -> sendMessage); flushing
 * right after the click pushes an empty replay table and quietly does nothing, and then the case times
 * out on "the agent reply does not appear", far from the real cause. So this polls until there is a
 * replay (or it has already finished).
 */
export async function flushConversation(page: Page): Promise<void> {
  await page.waitForFunction(
    () => {
      const api = window.__amaApi;
      if (!api?.flushConversation) return false;
      // Replay is set up -> it can be pushed; there is already an agent reply -> this turn has finished, no need to push.
      return (
        (api.pendingReplays?.() ?? 0) > 0 ||
        document.querySelector('[data-testid="chat-agent-message"]') !== null
      );
    },
    undefined,
    { timeout: 10_000 },
  );
  /*
   * Use the deep version: a queued message being sent **starts another replay round**, which does not exist
   * yet during a synchronous flush (taking the queue head goes through sendMessage, which is async with a
   * 90ms delay). Pushing only one round, with two queued the second would stay in the queue forever,
   * showing up as "one was sent, the other does not move", with zero errors.
   *
   * Falling back to the synchronous version is for compatibility with old builds that lack deep (e2e
   * sometimes runs against a different dist).
   */
  await page.evaluate(async () => {
    const api = window.__amaApi;
    if (api?.flushConversationDeep) await api.flushConversationDeep();
    else api?.flushConversation?.();
  });
}

export { expect };

/**
 * Sidebar navigation links.
 *
 * The detail page's back link has the same name as the sidebar navigation (such as "run history"), and a
 * direct getByRole('link') would hit strict mode, so nav is always used to scope it.
 */
export function nav(page: Page, name: string) {
  return page.getByRole("navigation").getByRole("link", { name });
}

/**
 * Open the event view's filter popover and return it, for chaining selection controls.
 *
 * The filter axes (quick filters / kind / domain / concrete type / slow / failed / thread) were moved from
 * six laid-out rows into a Popover: laid out, they took over half the screen **above** the event list,
 * while the list is what is read all the time.
 *
 * **Call it before every click on a control**: a click in the popover changes the filter -> the URL
 * changes -> a re-render, and RAC's Popover may have closed by then. To click two options in a row call
 * it twice; do not write two clicks after one open (the second would land on an element that is gone,
 * with the error "element not visible" instead of "the popover closed", not pointing to the real cause at all).
 */
export async function openFilters(page: Page) {
  const panel = page.getByTestId("timeline-filter-panel");
  /*
    **Idempotent**: if already open, return directly without clicking the trigger again.

    This was found the hard way: with the popover open it covers the trigger itself, and clicking again is
    blocked by "some ToggleButton in the popover intercepts pointer events", then a 60-second timeout.
    The error points to the type button that happens to sit over the trigger (such as
    `session.thread_created`), not at all to "you clicked the trigger again while the popover was open".

    Clicking two filter options in a row is easily written as open->click->open->click, so open handles it itself.
  */
  if (await panel.isVisible().catch(() => false)) return panel;
  await page.getByTestId("timeline-filter-open").click();
  // The popover renders through a portal; wait for it to really mount before returning, or later clicks would miss.
  await panel.waitFor({ state: "visible" });
  return panel;
}

/**
 * Close the filter popover, and wait for it to really leave the DOM.
 *
 * ⚠️ **After clicking filter controls, to click a row in the list you must call this first.**
 * RAC's Popover has a dismiss layer covering the whole viewport; while the popover is open, clicks on the
 * list are intercepted by it, and Playwright reports `<body>…</body> intercepts pointer events` + a
 * 60-second timeout, not pointing at all to "the popover is still open". Hit once, with three cases
 * failing on it at the same time.
 *
 * Use Escape rather than clicking empty space: the coordinates of empty space might land right on another
 * interactive element.
 */
export async function closeFilters(page: Page) {
  await page.keyboard.press("Escape");
  await page.getByTestId("timeline-filter-panel").waitFor({ state: "hidden" });
}

/**
 * Expand the card of some source on the authorization management page.
 *
 * # Why it is needed
 *
 * Cards on that page are **collapsible**, and only the first "needs your action" one is expanded by
 * default (see `initialExpandedIds` in `pages/IntegrationsTab.tsx`). So before asserting "is something in
 * the card", it must be expanded first: in the collapsed state that content is in a `hidden="until-found"`
 * subtree, and `toBeVisible()` judges it hidden.
 *
 * ⚠️ **Idempotent**: already expanded ones are not clicked. Which one is expanded by default depends on
 * the providers' states, and several cases change states (lark-user becomes ok after authorizing, the
 * first todo changes), so clicking unconditionally would **collapse** one that was open, a failure whose
 * cause is hard to see. The test uses `aria-expanded` (which RAC puts on the Trigger, see useDisclosure's
 * buttonProps).
 *
 * ⚠️ After expanding, **wait for the collapsible area to really be visible** before returning. The
 * `hidden` attribute is only removed after the animation ends (see react-aria's useDisclosure:
 * `Promise.all(getAnimations()).then(...)`), and without waiting the next assertion would run on the
 * frame before it opens.
 *
 * ⚠️⚠️ **What is waited for is "the card description", not `integration-summary-*`.** That colored Notice
 * is now **not on every card**: the two sources of `credentialCollection` do not render it when there is no
 * fixHint (that is, when everything is fine): its body is word for word the same as the collapsed row,
 * and right below it is the credential list with buttons, so the same sentence would appear twice on one
 * screen. Waiting for it would make `expandIntegration` on those two cards **wait until timeout**, with an
 * error saying "cannot find integration-summary-xxx", not pointing to "this card never has it".
 *
 * The description (`integration-desc-*`) is **the only thing rendered unconditionally** in the
 * collapsible area, so using it as the test of "is it open" is the most reliable.
 */
export async function expandIntegration(page: Page, provider: string): Promise<void> {
  const trigger = page.getByTestId(`integration-toggle-${provider}`);
  await trigger.waitFor();
  if ((await trigger.getAttribute("aria-expanded")) !== "true") {
    await trigger.click();
  }
  await page.getByTestId(`integration-desc-${provider}`).waitFor({ state: "visible" });
}

declare global {
  interface Window {
    __amaApi?: {
      reset: () => void;
      clear: () => void;
      completeFeishuAuth: () => void;
      /**
       * Mark the current Feishu app directly as authorized. Only LocalApiClient has it.
       *
       * Checking "switching to a never-authorized app makes it not authorized" needs an authorized
       * starting state first; otherwise the initial value is already not authorized and the assertion is always true.
       */
      completeLarkUserAuth?: () => void;
      /** Cut the selectable Feishu apps down to the first N, to reach the "only one app shows no picker" branch. */
      setLarkUserAppCount?: (n: number) => void;
      /**
       * Make "resolve Feishu app by workspace" always fail, to reach the "this workspace has not connected
       * a Feishu app yet" branch.
       *
       * Same reason as setLarkUserAppCount: that branch has no other way in under local mode
       * (the list is a module constant, no requests are sent, and page.route cannot intercept anything).
       */
      setLarkUserAppsEmpty?: (empty: boolean) => void;
      /** Only LocalApiClient has it (undefined on HttpApiClient), so it is optional. */
      flushConversation?: () => void;
      /** Number of replays in progress. Used by flushConversation to judge "is there anything to push". */
      pendingReplays?: () => number;
      /** Switch the mock to logged out. Likewise only LocalApiClient has it. */
      signOut?: () => void;
      /** Make the current user have no avatar, to check the initial fallback. Same as above, only LocalApiClient has it. */
      hideAvatar?: () => void;
      /** Clear the logged-out marker, simulating the user clicking consent on the SSO page. */
      completeLogin?: () => void;
      /** Make list read APIs fail, to check the error state. Same as above, only LocalApiClient has it. */
      failReads?: (code?: string) => void;
      /** Undo failReads. */
      restoreReads?: () => void;
      /**
       * Make **write** APIs fail (send message, save…), to check the error state of write actions and the two feedback levels.
       *
       * It is a different switch from failReads: that one keeps the page from opening, this one keeps the
       * page fine while the action fails.
       */
      failWrites?: (code?: string) => void;
      /** Undo failWrites. */
      restoreWrites?: () => void;
      /**
       * Make only **redirect** ("send now" on a queued message) fail; other writes work as usual.
       *
       * Separating it from failWrites is required: that one also breaks the queue's own resend, so the queue box
       * disappears from the DOM at the moment of the click (Playwright reports detached), and assertions **pass for
       * the wrong reason**: the text really is back in the input box, but that was done by the queue's fill-back
       * when giving up. See the notes on failRedirectMock.
       */
      failRedirect?: (code?: string) => void;
      /** Undo failRedirect. */
      restoreRedirect?: () => void;
      /**
       * Make the next N sends hit the tool execution window (409 + code=conflict), then succeed naturally.
       * `-1` = always hit it. Only LocalApiClient has it.
       *
       * It is a different switch from failWrites: that one is a permanent failure (to be shown to the user), this
       * one is a race of a few dozen milliseconds (to be queued and resent automatically). See the notes on conflictSendsMock.
       */
      conflictSends?: (times?: number) => void;
      /** Undo conflictSends. */
      clearConflictSends?: () => void;
      /**
       * Make this turn stay "in progress", so the queue's main path with `streaming` true is testable.
       * Only LocalApiClient has it. See the notes on holdTurnMock.
       */
      holdTurn?: () => void;
      /** Undo holdTurn, and let the held replay finish. */
      releaseTurn?: () => void;
      /**
       * Make the replay stop at some **intermediate stage** (currently only `thinking`). Only LocalApiClient has it.
       *
       * Why it is required: that intermediate state only lives ~300ms, and Playwright's expect backoff grid
       * steps right over it; see the notes and synthetic comparison data on holdThinkingMock.
       */
      holdStage?: (stage: "thinking") => void;
      /** Undo holdStage, and let the held replay finish. */
      releaseStage?: () => void;
      /**
       * Push all replays to the end, **including follow-up rounds caused by queued messages**. Only LocalApiClient has it.
       * See the notes in flushConversation.
       */
      flushConversationDeep?: () => Promise<void>;
      /**
       * Put one item straight into the server queue (not through the send flow). Only LocalApiClient has it.
       * Used to create "the queue has an item but this turn is not running"; see the notes on seedPendingMessage.
       */
      seedPendingMessage?: (runId: string, text: string) => void;
      /**
       * Make a workspace 403 for the current user (omit key = all workspaces). Only LocalApiClient has it.
       *
       * It is a **different** switch from failReads: that one hits the list APIs (the workspace still opens),
       * this one hits "this workspace cannot open". 13-load-errors needs the former to hold while the latter does not.
       */
      denyWorkspace?: (key?: string) => void;
      /** Undo denyWorkspace. */
      allowWorkspaces?: () => void;
      /**
       * Lower "my role in the workspace" to some level. Only LocalApiClient has it.
       *
       * The mock's person is always owner; without lowering it, the "non-admins see a read-only view" branch
       * never runs even once.
       */
      setMyRole?: (role: "owner" | "admin" | "member" | "viewer") => void;
      /** Undo setMyRole. */
      resetMyRole?: () => void;
      /**
       * Clear all three levels of Ark credentials, back to the initial "never configured a key" state. Only LocalApiClient has it.
       *
       * The seed is **configured** by default, so the "not configured" branch can only be reached with this.
       */
      clearArk?: () => void;
      /** Make read APIs never return, pinning the page in loading. Only LocalApiClient has it. */
      hangReads?: () => void;
      /**
       * Slow down each of the two full-page startup gates by N milliseconds, so "past the quiet period a
       * spinner must appear" is testable. Only LocalApiClient has it.
       */
      slowGates?: (ms: number) => void;
      /** Undo slowGates. */
      clearSlowGates?: () => void;
      // Only LocalApiClient has it (a mock-only switch), so it is optional.
      /** Pretend the Ark-side resources were created, so the "has id -> has entry" branch is testable. Only LocalApiClient has it. */
      enableArkConsoleMock?: (region?: "cn-beijing" | "ap-southeast-1") => void;
      /** Undo enableArkConsoleMock. */
      disableArkConsoleMock?: () => void;
      /** Pretend the container is in some state, so each of the four states' UI is testable. Only LocalApiClient has it. */
      setSandboxStatusMock?: (status: "none" | "running" | "paused" | "gone") => void;
      /** Undo setSandboxStatusMock. */
      clearSandboxStatusMock?: () => void;
      /**
       * Make terminal upstream (the POST for each keystroke) always fail. Only LocalApiClient has it.
       *
       * ⚠️ A separate switch rather than reusing failWritesMock: the mock's
       * writeRunSandboxTerminal does not go through guardWrite, so that switch has no effect on it.
       */
      failTerminalInputMock?: () => void;
      /** Undo failTerminalInputMock. */
      clearTerminalInputMock?: () => void;
      /**
       * Swap the id of some built-in skill, simulating "the backend changed a built-in skill's id".
       *
       * Used to check that the built-in skill ids checked by default when creating an agent are **fetched
       * dynamically**, not hard-coded literals: a hard-coded one fails silently (the box is still checked, but
       * it checks an id that does not exist), and only making the id really change once exposes it. Only LocalApiClient has it.
       */
      renameBuiltinSkillMock?: (from: string, to: string) => void;
      /** Undo renameBuiltinSkillMock. */
      clearBuiltinSkillRenameMock?: () => void;
      /**
       * Seed a few attachments with **content** for a skill, so the "view files inside a skill" path is testable.
       *
       * In mock mode a skill with attachment content cannot be created (none of the 4 in the seed has `files`, and
       * they cannot be added: the Go-side TestSeedRoundTrip compares seed.json byte for byte; the upload path
       * deliberately drops content; the mock does not unpack zips either), so this hook is the only way. Only LocalApiClient has it.
       *
       * Putting a `"\u0000"` in the content makes it judged binary (the same test as the backend core.isBinary).
       */
      seedSkillFilesMock?: (skillId: string, files: { path: string; content: string }[]) => void;
      /** Undo seedSkillFilesMock. */
      clearSkillFilesMock?: () => void;
      /**
       * Create the established fact "this schedule has already run N rounds".
       *
       * The mock has no scheduler (runScheduleNow only changes nextRunAt and returns), so the detail page's
       * run history, three counters, and "how many more until auto-pause" cannot be created in e2e. Only
       * LocalApiClient has it.
       *
       * counters.runCount may be **larger** than the number of runs: that is exactly the shape of the
       * existing-data gap "earlier history cannot be found".
       */
      seedScheduleHistoryMock?: (
        scheduleId: string,
        runs: { status?: string; startedAt?: string; errorSummary?: string }[],
        counters?: { runCount?: number; failCount?: number; consecutiveFailures?: number },
      ) => void;
      /**
       * Append a platform-injected block (`<harness:lark>`) to the first user.message of a run.
       *
       * Only LocalApiClient has it. This block is added by **the Feishu inbound** path, and the mock has no Feishu;
       * it cannot be added to the seed data either (seed.json is compared byte for byte).
       */
      seedInjectedContextMock?: (runId: string, text?: string) => void;
      /**
       * Pretend "fetch from Volcengine account" is in some state, so each of the five states' UI is testable.
       *
       * Only LocalApiClient has it. The default is `ready`; the other four states cannot be reached naturally in the mock.
       */
      setArkFetchStateMock?: (
        state: "ready" | "unavailable" | "unauthorized" | "error" | "loading",
      ) => void;
      /** Undo setArkFetchStateMock. */
      clearArkFetchStateMock?: () => void;
      /**
       * List the selectable Ark keys under a Volcengine account. **Both implementations have it**, so it is not optional.
       *
       * Why e2e calls it directly: this is the only way to assert "**the plaintext is not in the response body**".
       * Not shown in the UI does not mean not in the response: once plaintext reaches frontend memory, it is
       * already in the devtools network panel and may be read by browser extensions. That security premise can
       * only be checked at the data layer.
       */
      listVolcArkKeys: (accountId: string) => Promise<
        {
          id: string;
          name: string;
          masked?: string;
          status?: string;
          projectName?: string;
          createTime?: string;
          lastAccessTime?: string;
        }[]
      >;
      /** Undo hangReads. */
      resumeReads?: () => void;
      /** Turn the mock into "a brand new user" so the onboarding overlay appears. Only LocalApiClient has it. */
      resetOnboarding?: () => void;
      /** Undo resetOnboarding, back to the default "onboarding already seen". */
      restoreOnboarding?: () => void;
      /**
       * Set "the server copy" of preferences directly, **without touching the local cache**. Only LocalApiClient has it.
       *
       * It simulates "you changed preferences on another device": the only way to create data for the
       * cross-device assertions. Passing null means the server has no record (the precondition of the migration branch).
       */
      setServerPreferences?: (
        prefs: { language: string; appearance: string } | null,
      ) => void;
      /** Read "the server copy" of preferences; null means no record. Used to assert the migration result. */
      serverPreferences?: () => { language: string; appearance: string } | null;
      /**
       * Problem feedback the mock received. Only LocalApiClient has it.
       *
       * This is the observation window for the "two-level collection boundary" assertions: cases read it, then
       * check that the level-one entry **does not have** the keys path / agentId / recent / consoleErrors
       * (logId is the exception; both levels should have it).
       * Without it, nobody would watch the half where the frontend builds the payload, and that is exactly
       * where the boundary between the two levels is decided.
       */
      feedbackReports?: () => {
        level: string;
        routePattern: string;
        httpStatus: number;
        errorCode: string;
        errorKind: string;
        message: string;
        occurrences?: number;
        // Server logid. **Both levels have it** (it needs no user consent), but it only exists when the request
        // really reached the server: under network_error this key should not appear.
        logId?: string;
        // The following four **should only appear on records with level === "user"**: exactly what is asserted.
        path?: string;
        agentId?: string;
        recent?: { logId?: string }[];
        consoleErrors?: unknown[];
      }[];
      /** Clear the feedback the mock received. **Call it before every related case**, or count assertions get polluted by the previous case. */
      clearFeedback?: () => void;
      /**
       * Revoke an authorization source. **Both implementations have it**, so it is not optional.
       *
       * Why e2e calls it directly: that button is **deliberately not rendered** when "the platform does not support
       * revoking" (canRevoke is false), but the backend path must still reject and must not pretend to succeed.
       * With no button to click, the only way to lock this in is calling the API directly.
       */
      revokeIntegration: (provider: string) => Promise<void>;
      /**
       * Get a workspace by key. **Both implementations have it**, so it is not optional.
       *
       * Used by `builtinSkillCount` to get the workspaceId (`listSkills` takes an id, not a key).
       */
      getWorkspaceByKey: (key: string) => Promise<{ id: string } | null>;
      /**
       * List a workspace's skills, **including platform built-in ones** (both implementations list them first).
       *
       * Used by `builtinSkillCount` to compute "how many built-in skills there are": that number must not be a
       * literal; see that function's notes. Only the two fields really read here are declared.
       */
      listSkills: (workspaceId: string) => Promise<{ id: string; source: string }[]>;
    };
  }
}

/**
 * "Open in the Ark console": three entry points, for agent / skill / session.
 *
 * # What this group of cases guards
 *
 * The failure modes of this feature are **all zero-error**: the link still renders, clicking it
 * still opens a page, only that page is about something else, or is just a 404. So the point of the
 * assertions is not "is there a link" but **these three things**:
 *
 *   1. **with no id, the whole entry point is not rendered** (Ark resources are created lazily, so
 *      this is the most common state);
 *   2. **the URL is exactly right**: a wrong one opens a 404, which looks like "Ark deleted the resource";
 *   3. **region follows the credentials**, not hard-coded.
 *
 * ⚠️ "Element should not appear" assertions have a trap: `toHaveCount(0)` is already satisfied in the
 * frame **before the page has finished rendering**, so it turns green even if the implementation is
 * "always render". So before every negative assertion, first wait for an anchor that **must appear**
 * (the page title or similar) to make sure the render window has passed.
 * This was mutation-checked: changing the implementation to "always render" turns the two negative
 * cases below red.
 */
import { expect, test } from "./fixtures";
import { clearArkMock, disableArkConsoleMock, enableArkConsoleMock } from "./fixtures";

const AGENT_PATH = "/w/growth/agents/agent_triage";
const RUN_PATH = "/w/growth/runs/run_1";

/** URL prefix of the Ark console, shared by three cases. */
const CONSOLE = "https://console.volcengine.com/ark/region:cn-beijing/managed-agents";

test.afterEach(async ({ app }) => {
  // ⚠️ workers=1, localStorage shared serially: leaving the marker would make all later specs run on
  // the "already created on the Ark side" branch, with failure messages that do not point to this at all.
  await disableArkConsoleMock(app);
});

test("方舟侧还没建出来时，agent 详情页不渲染方舟控制台入口", async ({ app }) => {
  // ⚠️ This case assumes "this workspace **has never configured** Ark credentials" (see the comment
  // below), but the seed now configures one by default (seedArkCredentials in data/api.ts), so undo it
  // explicitly.
  await clearArkMock(app);
  await app.goto(AGENT_PATH);

  // First wait for the page to really render; otherwise the negative assertion below is satisfied in
  // the "nothing drawn yet" frame, and is still green if the implementation is changed to "always
  // render" (a false green).
  await expect(app.getByTestId("agent-save")).toBeVisible();

  await expect(app.getByTestId("agent-ark-console")).toHaveCount(0);

  /*
   * ⚠️ The seed **has no Ark credentials**, so at this step even the note "not created on Ark yet"
   * should not be there: that note assumes "you already use Ark, this agent just hasn't chatted yet".
   * For a workspace that never configured Ark, explaining when Ark resources get created only causes
   * confusion.
   *
   * The "credentials configured but not created yet" branch is checked in a separate case below.
   */
  await expect(app.getByTestId("agent-ark-console-absent")).toHaveCount(0);
});

test("配了方舟凭据但还没开过对话时，要解释为什么没有入口", async ({ app }) => {
  /*
   * This guards a state **users will come asking about**: they saw "view in the Ark console" on another
   * agent, cannot find it on this newly created agent, and think it is broken.
   *
   * Building the scenario: first configure credentials (enableArkConsoleMock does this along the way),
   * then remove the "already created" marker, which gives region but no arkAgentId: exactly that state.
   */
  await app.goto(AGENT_PATH);
  await enableArkConsoleMock(app);
  await disableArkConsoleMock(app);
  await app.reload();

  await expect(app.getByTestId("agent-save")).toBeVisible();
  // No id: the entry point is still not rendered (**not** rendered as a disabled button).
  await expect(app.getByTestId("agent-ark-console")).toHaveCount(0);
  // But it must explain this is not a fault, and how to make it appear.
  await expect(app.getByTestId("agent-ark-console-absent")).toBeVisible();
});

test("方舟侧建出来之后，agent 详情页的入口指向正确的控制台地址", async ({ app }) => {
  await app.goto(AGENT_PATH);
  await enableArkConsoleMock(app);
  await app.reload();

  const link = app.getByTestId("agent-ark-console");
  await expect(link).toBeVisible();

  // It is an anchor, not a button; otherwise middle-click / Cmd+click / right-click "open in new tab" are all lost.
  expect(await link.evaluate((el) => el.tagName)).toBe("A");
  await expect(link).toHaveAttribute("target", "_blank");
  // rel is required: without it, the opened page could control this page back through window.opener.
  await expect(link).toHaveAttribute("rel", /noopener/);
  await expect(link).toHaveAttribute("rel", /noreferrer/);

  /*
   * Compare the URL **exactly**, not with toContain.
   *
   * This id has the shape of a real Ark agent id, so this assertion is also evidence that "the
   * address we build matches the real console address".
   */
  await expect(link).toHaveAttribute(
    "href",
    `${CONSOLE}/agents/agent-20260831113008-frhdh`,
  );

  // When the entry point exists, the "not created yet" note must disappear, or it contradicts itself.
  await expect(app.getByTestId("agent-ark-console-absent")).toHaveCount(0);
});

test("会话详情页的入口指向方舟 session，且 id 就是 run.id", async ({ app }) => {
  /*
   * ⚠️ enableArkConsoleMock is called here **only to configure Ark credentials** (the seed has none,
   * and without credentials there is no region, so no URL can be built), **not** to "make the session
   * id appear".
   *
   * Sessions differ from the other two kinds: run.id is itself the Ark session id (the backend uses
   * the id returned by CreateSession directly when creating a Run), so as long as the session exists
   * it can always be given, with no lazy creation involved. The href assertion below uses `run_1`, the
   * id of that run in the seed itself, not the made-up agent/skill id in the switch; that is the evidence.
   */
  await app.goto(RUN_PATH);
  await enableArkConsoleMock(app);
  await app.reload();

  const link = app.getByTestId("run-ark-console");
  await expect(link).toBeVisible();
  expect(await link.evaluate((el) => el.tagName)).toBe("A");
  await expect(link).toHaveAttribute("rel", /noopener/);

  await expect(link).toHaveAttribute("href", `${CONSOLE}/sessions/run_1`);
});

test("技能详情页：没传到方舟时无入口，传了之后指向方舟技能", async ({ app }) => {
  // Same as the agent case: the first half of this case assumes "Ark credentials never configured",
  // while the seed configures them by default.
  await clearArkMock(app);
  // The user-created skill in the seed.
  await app.goto("/w/growth/skills/skill-20260801-triage");

  // Same as above: first wait for an anchor that must appear, then make the negative assertion.
  await expect(app.getByTestId("skill-detail-name")).toBeVisible();
  await expect(app.getByTestId("skill-ark-console")).toHaveCount(0);
  // The seed has no Ark credentials, so at this step even that note should not be there (same reason as the agent case).
  await expect(app.getByTestId("skill-ark-console-absent")).toHaveCount(0);

  await enableArkConsoleMock(app);
  await app.reload();

  const link = app.getByTestId("skill-ark-console");
  await expect(link).toBeVisible();
  await expect(link).toHaveAttribute("rel", /noopener/);
  await expect(link).toHaveAttribute(
    "href",
    `${CONSOLE}/skills/skill-20260901120830-px59g`,
  );
  // When the entry point exists, that note must disappear, or it contradicts itself.
  await expect(app.getByTestId("skill-ark-console-absent")).toHaveCount(0);
});

test("技能没传到方舟时，要解释为什么没有入口", async ({ app }) => {
  /*
   * # This one came from real user feedback
   *
   * "I don't see a link from the skill to MA." On inspection the feature was right: a skill is only
   * sent to Ark after some Agent bound to it has really had a chat; until then there is no id and no
   * entry point. But **most of the local skills were in that state**: users seeing "some have it, some
   * don't" naturally conclude the feature is broken.
   *
   * So "no entry point" must be explained on the skill page, for the same reason as the agent page.
   * The scenario is built the same way: configure credentials first, then remove the "already created" marker.
   */
  await app.goto("/w/growth/skills/skill-20260801-triage");
  await enableArkConsoleMock(app);
  await disableArkConsoleMock(app);
  await app.reload();

  await expect(app.getByTestId("skill-detail-name")).toBeVisible();
  // No id: the entry point is not rendered.
  await expect(app.getByTestId("skill-ark-console")).toHaveCount(0);
  // But it must explain this is not a fault, and how to make it appear.
  await expect(app.getByTestId("skill-ark-console-absent")).toBeVisible();
});

test("控制台地址未知的区域不给入口，而不是拼一个指向错区域的链接", async ({ app }) => {
  /*
   * # This guards "region must not be hard-coded"
   *
   * `ap-southeast-1` is BytePlus, and its console is **not** on console.volcengine.com, and we have no
   * real BytePlus managed-agents address to copy (see the CONSOLE_HOSTS part of lib/arkConsole.ts). So
   * **no entry point is given** in that region.
   *
   * ⚠️ Without this case, the change "hard-code region to cn-beijing" **would not turn any case red**:
   * verified, with only one region to choose from, hard-coding and not hard-coding behave exactly the
   * same. That is why this case exists: it makes the "other region" path testable.
   */
  await app.goto(AGENT_PATH);
  await enableArkConsoleMock(app, "ap-southeast-1");
  await app.reload();

  // Wait for the page to finish rendering before the negative assertion (otherwise it is a false green).
  await expect(app.getByTestId("agent-save")).toBeVisible();
  await expect(app.getByTestId("agent-ark-console")).toHaveCount(0);
});

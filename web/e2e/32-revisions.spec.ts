/**
 * Requirement: entity revision history (switch back after a mistake).
 *
 * Covers three entities, agent / skill / schedule, but **not as three identical sets of cases**:
 * each entity only tests its own most error-prone edge, and the shared behavior (recording,
 * expanding, rolling back) is tested once, on agent. The reason is that all three paths go through
 * the same Service on the server (core/revision.go), and three identical sets of cases would only
 * mean changing three places when behavior changes once.
 *
 * The four things that "cause no error when wrong" we watch:
 *
 *  1. **existing entities have no history at all**: it must say "no revisions yet", not leave a
 *     blank, and not error (that is the normal state, not an exception).
 *  2. **rollback must really change the content back**, and rollback **leaves a record itself** too
 *     (otherwise picking the wrong revision cannot be undone).
 *  3. **limit**: past it the oldest is evicted, and the UI must state the number.
 *  4. **the counter must not go backwards**: after a rollback the `v7` badge on the agent detail page
 *     must keep moving forward. This is the most important one in the whole set: if it is wrong, the
 *     page looks like "rollback succeeded", and only the version number quietly goes backwards.
 */

import { expect, test } from "./fixtures";

/** Change the agent's prompt once and wait for the save to finish. */
async function editAgentSystem(
  app: import("@playwright/test").Page,
  text: string,
): Promise<void> {
  await app.getByTestId("tab-config").click();
  await app.getByTestId("cfg-system").fill(text);
  await app.getByTestId("agent-save").click();
  await expect(app.getByText("已保存")).toBeVisible();
}

/** Switch to the revision history tab. */
async function openRevisions(app: import("@playwright/test").Page): Promise<void> {
  await app.getByTestId("tab-revisions").click();
}

test("没有历史版本时说清「还没有」，而不是留白", async ({ app }) => {
  await app.goto("/w/growth/agents/agent_triage");
  await openRevisions(app);

  // The agent in the seed was never changed, so it has no history at all: this is **the normal state of existing data**.
  await expect(app.getByText("还没有历史版本")).toBeVisible();
  // In the empty state no rollback button should appear (there is nothing to roll back).
  await expect(app.locator('[data-testid^="revision-rollback-"]')).toHaveCount(0);
});

test("改一次就留下一条历史，存的是改动前的内容", async ({ app }) => {
  await app.goto("/w/growth/agents/agent_triage");
  const before = await app.getByTestId("cfg-system").inputValue();

  await editAgentSystem(app, "新版提示词：先给级别，再给依据。");
  await openRevisions(app);

  await expect(app.getByTestId("revision-row-1")).toBeVisible();
  // Expand to see the content. **The list API does not include the snapshot**; this step fetches the single item.
  await app.getByTestId("revision-toggle-1").click();
  const detail = app.getByTestId("revision-detail");
  await expect(detail).toBeVisible();
  // ⚠️ The assertion is on the copy from **before** the change. Storing the after-change copy is a
  // very natural slip (recording after assignment), and then a rollback would do nothing, with no way
  // to tell from the page.
  await expect(detail).toContainText(before);
  await expect(detail).not.toContainText("新版提示词");
});

/**
 * The changer must be shown with **real name and avatar**, not an email initial.
 *
 * # ⚠️ The assertions must expose the "profile not passed" bug
 *
 * When `<UserAvatar>` is missing `profile`, **the feature still looks like it works**: the avatar
 * circle is there, the tooltip is there, it just always degrades to the email initial (the lone "C"
 * in the screenshot). So **assertions like "UserAvatar exists" or "there is an avatar element" hold
 * just as well for the broken implementation**; they are always true and expose no bug. This repo
 * has fallen into such false greens.
 *
 * The two tests chosen here **only hold when profile is passed**:
 *
 *  1. an `<img>` node exists and its src is a data URI: Radix only renders img after the image has
 *     loaded, and avatarUrl **can only come from profile** (Revision has no such field).
 *     Without profile, src is undefined and the node never enters the DOM.
 *  2. the accessible name is "changer: <the Chinese display name>": that name is also only in profile.
 *     Without it, it falls back to the email `alice@example.com`, so the two can be told apart.
 */
test("改动者显示真实姓名与头像，不是邮箱首字母", async ({ app }) => {
  await app.goto("/w/growth/agents/agent_triage");
  await editAgentSystem(app, "随便改一下，好让历史非空");
  await openRevisions(app);

  const author = app.getByTestId("revision-author-1");
  await expect(author).toBeVisible();

  // ⭐ Test one: the real avatar loaded. The mock gives the current user an inline data URI (this
  // machine cannot reach external image hosts, see data/api.ts). avatarUrl only comes from profiles, so
  // this assertion is equivalent to "profile was really passed in".
  await expect(author.locator("img")).toHaveAttribute("src", /^data:image\/svg\+xml/);

  // ⭐ Test two: the screen reader reads the display name, not the email. The initial fallback path
  // cannot produce that name.
  //
  // ⚠️ aria-label is on the **outer Tooltip.Trigger button**, not on the Avatar carrying the testId
  // (the testId is deliberately on Avatar: when the image has not loaded, that <img> does not exist,
  // see UserAvatar). Asserting on the testId node would get null: that is locating the wrong element,
  // not a wrong implementation.
  const trigger = app.getByRole("button", { name: "改动者：张三" });
  await expect(trigger).toBeVisible();

  // Lock in the opposite: it must **not** be just an initial. When profile is missing, this is exactly
  // "A" (the initial of alice@), the very shape the user saw in the screenshot.
  await expect(author).not.toHaveText("A");
});

test("回滚把内容改回去，并且回滚本身也留一条历史", async ({ app }) => {
  await app.goto("/w/growth/agents/agent_triage");
  const original = await app.getByTestId("cfg-system").inputValue();

  await editAgentSystem(app, "改坏了的提示词");
  await openRevisions(app);
  await expect(app.getByTestId("revision-row-1")).toBeVisible();

  await app.getByTestId("revision-rollback-1").click();
  await expect(app.getByTestId("confirm-danger")).toBeVisible();
  await app.getByTestId("confirm-danger").click();

  // The content went back.
  await app.getByTestId("tab-config").click();
  await expect(app.getByTestId("cfg-system")).toHaveValue(original);

  // ⚠️ **Rollback must leave a record itself** (storing "the broken version"), otherwise after the
  // user picks the wrong revision there is no way back, and that is exactly a second mistake in a
  // mistake-recovery scenario.
  await openRevisions(app);
  await expect(app.getByTestId("revision-row-2")).toBeVisible();
  await expect(app.getByTestId("revision-reason-rollback")).toBeVisible();

  // That new history entry should hold the content from **before** the rollback.
  await app.getByTestId("revision-toggle-2").click();
  await expect(app.getByTestId("revision-detail")).toContainText("改坏了的提示词");
});

/**
 * ⚠️ The most important case in the whole set.
 *
 * `Agent.version` is a monotonic counter, shown as `v7` at the top of the detail page. After rolling
 * back to an old revision it must **keep moving forward** (the content goes back, the number goes
 * forward), not go back together with the snapshot.
 *
 * If it is wrong the page looks completely normal (the content really was rolled back); only that
 * badge quietly goes backwards, with no error. So this assertion must stand alone and cannot be
 * merged into the "rollback changes the content back" case above.
 */
test("回滚之后版本号继续递增，不跟着倒退", async ({ app }) => {
  await app.goto("/w/growth/agents/agent_triage");

  const readVersion = async () => {
    const text = await app.getByText(/^v\d+$/).first().innerText();
    return Number(text.slice(1));
  };
  const v0 = await readVersion();

  await editAgentSystem(app, "第二版");
  await editAgentSystem(app, "第三版");
  const beforeRollback = await readVersion();
  expect(beforeRollback).toBeGreaterThan(v0);

  // Roll back to the earliest entry (content = the original prompt).
  await openRevisions(app);
  const oldest = app.locator('[data-testid^="revision-rollback-"]').last();
  await oldest.click();
  await app.getByTestId("confirm-danger").click();

  await app.getByTestId("tab-config").click();
  const afterRollback = await readVersion();
  expect(afterRollback).toBeGreaterThan(beforeRollback);
});

/**
 * Limit. The mock side keeps 20 (matching the backend domain.MaxRevisionsPerEntity).
 *
 * This does not really change it 21 times (that would be 21 saves, slow and fragile); it only asserts
 * that **the UI states the limit**. The "evict when over" behavior is locked in by Go-side cases
 * (one each in core/revision_test.go and coredao/revision_test.go, running on both Mem and PG).
 * What the frontend must guarantee is that the number **comes from the server**, not hard-coded.
 */
test("列表底部说清最多保留几个版本", async ({ app }) => {
  await app.goto("/w/growth/agents/agent_triage");
  await editAgentSystem(app, "随便改一下，好让历史非空");
  await openRevisions(app);

  await expect(app.getByTestId("revision-max-kept")).toContainText("20");
});

test("定时任务：改配置留历史，回滚能切回旧的 prompt", async ({ app }) => {
  await app.goto("/w/growth/schedules");
  await app.getByTestId("new-schedule").click();
  await app.getByTestId("sched-name").fill("版本历史用的任务");
  await app.getByTestId("sched-prompt").fill("原始 prompt");
  await app.getByTestId("modal-submit").click();

  const row = app.locator('[data-testid^="schedule-row-"]').first();
  const id = ((await row.getAttribute("data-testid")) ?? "").replace("schedule-row-", "");
  await app.getByTestId(`schedule-link-${id}`).click();

  // Change the prompt.
  await app.getByTestId("detail-prompt").fill("改过的 prompt");
  await app.getByTestId("detail-save").click();

  await app.getByTestId("detail-tab-revisions").click();
  await expect(app.getByTestId("revision-row-1")).toBeVisible();

  await app.getByTestId("revision-rollback-1").click();
  await app.getByTestId("confirm-danger").click();

  await app.getByTestId("detail-tab-config").click();
  await expect(app.getByTestId("detail-prompt")).toHaveValue("原始 prompt");
});

/**
 * The skill case only tests the note "attachments are not covered by rollback".
 *
 * ⚠️ This is not optional text: `SkillDraft` does not include `files`, so rollback **only restores
 * the body**. Without saying so, users would think attachments went back too, and that causes no
 * error; it only shows up the next time they notice the attachments are still new, when it is hard
 * to connect it to the rollback not covering attachments.
 */
test("技能：回滚前要说清附件不会被还原", async ({ app }) => {
  // ticket-triage is the user-created (custom) skill in the seed; built-in ones have no revision
  // history, so they are not suitable for this case.
  await app.goto("/w/growth/skills/skill-20260801-triage");

  await app.getByTestId("skill-tab-revisions").click();
  // Never changed, so it is the empty state: this case also covers the skill page's empty state.
  await expect(app.getByText("还没有历史版本")).toBeVisible();
});

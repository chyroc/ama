/**
 * Requirement 1: agents can be created, each bound to its own prompt and model.
 */

import { expect, test } from "./fixtures";
import type { Page } from "@playwright/test";

test("创建 agent 并指定独立的提示词与模型", async ({ app }) => {
  await app.goto("/w/growth/agents");
  await app.getByTestId("new-agent").click();
  await expect(app).toHaveURL(/\/agents\/new$/);

  await app.getByTestId("agent-name").fill("竞品情报官");
  await app.getByTestId("agent-desc").fill("每日汇总竞品动态");
  await app
    .getByTestId("agent-system")
    .fill("你负责跟踪竞品动态。每条结论必须给出信息来源，不确定的信息要标注出来。");

  await app.getByTestId("agent-model").click();
  await app.getByRole("option", { name: /GLM 5\.2/ }).click();

  await app.getByTestId("agent-submit").click();
  await expect(app).toHaveURL(/\/agents\/agent_/);
  await expect(app.getByRole("heading", { name: "竞品情报官" })).toBeVisible();

  // The prompt and model really landed on this agent
  await expect(app.getByTestId("cfg-system")).toHaveValue(/你负责跟踪竞品动态/);
  await expect(app.getByText("GLM 5.2").first()).toBeVisible();
});

test("必填项缺失时不创建", async ({ app }) => {
  await app.goto("/w/growth/agents/new");
  await app.getByTestId("agent-submit").click();
  await expect(app.getByText("请填写名称")).toBeVisible();
  await expect(app.getByText("请填写系统提示词")).toBeVisible();
  await expect(app).toHaveURL(/\/agents\/new$/);
});

test("不同 agent 各自持有不同的提示词与模型", async ({ app }) => {
  await app.goto("/w/growth/agents");

  // The two agents in the seed use different models
  await app.getByTestId("agent-card-agent_triage").click();
  const triageSystem = await app.getByTestId("cfg-system").inputValue();
  const triageModel = await app.getByTestId("agent-model").innerText();

  await app.goto("/w/growth/agents");
  await app.getByTestId("agent-card-agent_reviewer").click();
  const reviewerSystem = await app.getByTestId("cfg-system").inputValue();
  const reviewerModel = await app.getByTestId("agent-model").innerText();

  expect(triageSystem).not.toBe(reviewerSystem);
  expect(triageModel).not.toBe(reviewerModel);
});

test("修改提示词与模型后能保存，版本号递增", async ({ app }) => {
  await app.goto("/w/growth/agents/agent_triage");

  const before = await app.getByText(/^v\d+$/).first().innerText();

  await app.getByTestId("cfg-system").fill("你是分诊助手。新版提示词：先给级别，再给依据。");
  await app.getByTestId("agent-save").click();
  await expect(app.getByText("已保存")).toBeVisible();

  await app.reload();
  await expect(app.getByTestId("cfg-system")).toHaveValue(/新版提示词/);

  // Ark uses version for optimistic locking; every update adds 1
  const after = await app.getByText(/^v\d+$/).first().innerText();
  expect(Number(after.slice(1))).toBe(Number(before.slice(1)) + 1);
});

test("换模型时把不支持的推理档位收敛掉", async ({ app }) => {
  await app.goto("/w/growth/agents/agent_analyst");

  // turbo supports the fast tier
  await app.getByTestId("agent-tier").click();
  await expect(app.getByRole("option", { name: "fast" })).toBeVisible();
  await app.keyboard.press("Escape");

  // Switch to pro (the docs say it does not support fast); the fast option should disappear.
  // Locate by model ID, not label: Doubao Seed 2.1 Pro has two versions, 260628 / 260915, with exactly the
  // same label, so `/Doubao Seed 2\.1 Pro/` would hit both (strict mode violation).
  await app.getByTestId("agent-model").click();
  await app.getByRole("option", { name: /doubao-seed-2-1-pro-260628/ }).click();
  await app.getByTestId("agent-tier").click();
  await expect(app.getByRole("option", { name: "fast" })).toHaveCount(0);
});

/**
 * The reasoning_effort axis interplay is a separate adjustment logic from service_tier above (each
 * clamped separately in AgentsPage's changeModel); testing only tier would miss the effort one.
 *
 * The pro <-> glm-5.2 pair is chosen: their level sets **overlap very little**: pro is
 * [minimal, low, medium, high], glm is [none, minimal, high, max], so medium/low are only in pro and
 * none/max only in glm, and switching there and back shows the adjustment. All taken from measurements
 * (server/cmd/probe-models).
 */
test("换模型时推理档位按新模型的支持集收敛", async ({ app }) => {
  await app.goto("/w/growth/agents/agent_analyst");

  // First land on pro: it supports medium, not none / max.
  // Also locate by ID: the two Doubao Seed 2.1 Pro have identical labels.
  await app.getByTestId("agent-model").click();
  await app.getByRole("option", { name: /doubao-seed-2-1-pro-260628/ }).click();
  await app.getByTestId("agent-effort").click();
  await expect(app.getByRole("option", { name: "中" })).toBeVisible();
  await expect(app.getByRole("option", { name: "最高" })).toHaveCount(0);
  await expect(app.getByRole("option", { name: "关闭" })).toHaveCount(0);
  await app.keyboard.press("Escape");

  // Switch to GLM 5.2: medium should disappear, none / max appear
  await app.getByTestId("agent-model").click();
  await app.getByRole("option", { name: /GLM 5\.2/ }).click();
  await app.getByTestId("agent-effort").click();
  await expect(app.getByRole("option", { name: "中" })).toHaveCount(0);
  await expect(app.getByRole("option", { name: "最高" })).toBeVisible();
  await expect(app.getByRole("option", { name: "关闭" })).toBeVisible();
});

/**
 * Locks in that the two deepseek models that "differ only by -ga-" are **two separate entries**.
 *
 * This was added because deepseek-v4-pro-260425 was once missing from MODEL_SPECS: the IDs differ only
 * by `-ga-`, very easy to read as the same one in review. Their measured levels really differ too: the
 * non-GA one does not support `none` and the GA one does, so picking the wrong one gives a different
 * set of valid levels.
 */
test("deepseek 的 GA 与非 GA 是两个可选模型，档位不同", async ({ app }) => {
  await app.goto("/w/growth/agents/agent_analyst");

  // Both entries are in the list at the same time, told apart by ID (the labels deliberately carry versions too)
  await app.getByTestId("agent-model").click();
  await expect(app.getByRole("option", { name: /deepseek-v4-pro-260425/ })).toBeVisible();
  await expect(app.getByRole("option", { name: /deepseek-v4-pro-ga-260813/ })).toBeVisible();

  // Non-GA: does not support none (UI text "off")
  await app.getByRole("option", { name: /deepseek-v4-pro-260425/ }).click();
  await app.getByTestId("agent-effort").click();
  await expect(app.getByRole("option", { name: "关闭" })).toHaveCount(0);
  await app.keyboard.press("Escape");

  // GA: supports none
  await app.getByTestId("agent-model").click();
  await app.getByRole("option", { name: /deepseek-v4-pro-ga-260813/ }).click();
  await app.getByTestId("agent-effort").click();
  await expect(app.getByRole("option", { name: "关闭" })).toBeVisible();
});

/**
 * Every model in the model picker has a vendor logo in front, and **only one in the collapsed state**.
 *
 * The second half is a regression: `Select.Value` renders the whole content of the selected
 * ListBox.Item, which already contains the icon; the first version drew another ModelIcon in the
 * Trigger, so the collapsed state had two icons stacked. Such a defect is all green in typecheck /
 * build and only noticed by looking at the UI, so it is locked in with a count.
 */
test("模型选择器每项都有厂商标，收起时只显示一个", async ({ app }) => {
  await app.goto("/w/growth/agents/new");

  // Collapsed state: exactly one icon in the Trigger (one more means the bug above is back)
  const trigger = app.getByTestId("agent-model");
  await expect(trigger.locator("[data-testid^='model-icon-']")).toHaveCount(1);

  // After expanding: each of the 10 models has an icon, none missing
  await trigger.click();
  const options = app.getByRole("option");
  await expect(options).toHaveCount(10);
  await expect(app.getByRole("listbox").locator("[data-testid^='model-icon-']")).toHaveCount(10);

  // Each of the three vendors uses the right logo (Doubao 5, DeepSeek 4, Zhipu 1)
  const listbox = app.getByRole("listbox");
  await expect(listbox.getByTestId("model-icon-豆包")).toHaveCount(5);
  await expect(listbox.getByTestId("model-icon-DeepSeek")).toHaveCount(4);
  await expect(listbox.getByTestId("model-icon-智谱")).toHaveCount(1);
});

/**
 * Locks in the new model deepseek-v4-1-flash-261017: it is not in the Ark docs mirror yet (see
 * KNOWN_DOC_LAG in web/scripts/check-models.mjs for the basis), but it works on the platform in
 * practice, so it must really appear in the picker, with levels adjusted per measurement to
 * [none, low, high, max].
 *
 * "low" / "high" use exact: without it, "high" would also match "very high" and "highest", making the
 * assertion meaningless.
 */
test("DeepSeek V4.1 Flash 可选，且档位按平台实测收敛", async ({ app }) => {
  await app.goto("/w/growth/agents/agent_analyst");

  await app.getByTestId("agent-model").click();
  await app.getByRole("option", { name: /deepseek-v4-1-flash-261017/ }).click();

  await app.getByTestId("agent-effort").click();
  await expect(app.getByRole("option", { name: "关闭", exact: true })).toBeVisible();
  await expect(app.getByRole("option", { name: "低", exact: true })).toBeVisible();
  await expect(app.getByRole("option", { name: "高", exact: true })).toBeVisible();
  await expect(app.getByRole("option", { name: "最高", exact: true })).toBeVisible();
  // Measured on the platform: minimal / medium not supported
  await expect(app.getByRole("option", { name: "极简", exact: true })).toHaveCount(0);
  await expect(app.getByRole("option", { name: "中", exact: true })).toHaveCount(0);
});

test("能删除 agent", async ({ app }) => {
  await app.goto("/w/growth/agents/agent_draft");
  await app.getByLabel("删除 Agent").click();
  await app.getByTestId("confirm-danger").click();

  await expect(app).toHaveURL(/\/agents$/);
  await expect(app.getByTestId("agent-card-agent_draft")).toHaveCount(0);
});

test("授权勾选能关能开，且保存后不残留「未保存」状态", async ({ app }) => {
  // This case used to test a single credentials Switch. After authorization became multi-select it was
  // replaced by a list (see 31-agent-runtime.spec.ts), but **this case itself must stay**,
  // because what it really watches is something else:
  //
  // ⚠️ isDirty is a bare JSON.stringify comparison; if toAgentDraft misses integrationIds or envs, the
  // page is "dirty" as soon as it opens, the save button stays lit forever and does not go away after
  // clicking. typecheck cannot catch that failure; it only shows when running.
  //
  // agent_triage in the seed has lark-user checked.
  await app.goto("/w/growth/agents/agent_triage");

  const toggle = app.getByTestId("bind-integration-ama").locator("label");
  const risk = app.getByTestId("agent-integration-risk");
  const save = app.getByTestId("agent-save");

  // lark-user is already checked, so the risk warning is there.
  await expect(risk).toBeVisible();
  // Right after entering the page there are no changes, so the save button should be disabled (this is
  // the real purpose mentioned above).
  await expect(save).toBeDisabled();

  // Check one more: the save button lights up.
  await toggle.click();
  await expect(save).toBeEnabled();
  await save.click();
  await expect(app.getByText("已保存")).toBeVisible();

  // It saves, and **after reading it back it should no longer show "unsaved"**.
  await app.reload();
  await expect(app.getByTestId("bind-integration-ama").locator("input")).toBeChecked();
  await expect(app.getByTestId("agent-save")).toBeDisabled();

  // Uncheck it again: this checks the frontend side of the backend rule "an empty array can be stored":
  // the backend assigns integrationIds unconditionally, and an empty array means "uncheck all".
  // If it were written like Status, "skip when empty", this step would show up as "unchecked, then it came back by itself".
  await app.getByTestId("bind-integration-ama").locator("label").click();
  await app.getByTestId("agent-save").click();
  await expect(app.getByText("已保存")).toBeVisible();
  await app.reload();
  await expect(app.getByTestId("bind-integration-ama").locator("input")).not.toBeChecked();
});

// ---------------------------------------------------------------------------
// Default checks for a new agent
// ---------------------------------------------------------------------------

/*
 * How to assert the checked state: two traps found by measurement:
 *
 * One, **`toBeChecked()` cannot be used directly on the testid.** HeroUI's Checkbox puts the testid on the
 *     outer `<div data-slot="checkbox">`, while the real `<input type="checkbox">` is hidden inside (in a
 *     visually hidden container with `clip-path: inset(50%)`). Asserting on the outer one reports
 *     "Not a checkbox or radio button", which sounds like a wrong testid but is really a nesting problem.
 *
 * Two, **clicks must also land on the `label`.** Clicking the outer div does not reach that hidden input,
 *     `data-selected` does not move, so the "uncheck" step fails silently while later assertions check
 *     "the effect after unchecking": a failure that does not point to the real cause. The authorization
 *     checkboxes (bind-integration-*) also use `.locator("label").click()`, for the same reason.
 *
 * When unchecked, `data-selected` **does not exist** (it is not "false"), so use the input's
 * toBeChecked rather than an attribute assertion: the attribute form must remember this asymmetry and
 * is easily written to be always true.
 */
const checkbox = (app: Page, testId: string) =>
  app.getByTestId(testId).locator("input");
const clickCheckbox = (app: Page, testId: string) =>
  app.getByTestId(testId).locator("label").click();

// The defaults only affect new agents: when the form opens, all built-in tools are checked, and the two
// built-in skills too.
//
// Why this is worth testing: the symptom of a missed check is "the model says it has no tools/skills",
// a sentence that does not point to this form, and debugging would first dig through the Ark side and
// worker logs (see the warning at shared/infra/ma/agents.go:52).
test("新建 agent 表单默认勾上全部内置工具与内置技能", async ({ app }) => {
  await app.goto("/w/growth/agents/new");

  // Assert the eight built-in tools one by one, not "checked count === 8": equal counts with the wrong
  // ones checked would still pass.
  for (const tool of [
    "bash",
    "read",
    "write",
    "edit",
    "glob",
    "grep",
    "web_fetch",
    "web_search",
  ]) {
    await expect(checkbox(app, `tool-${tool}`)).toBeChecked();
  }

  // The two built-in skills. They are fetched asynchronously, and toBeChecked waits by itself, so no
  // waitFor first is needed; but **they must come after the tools in order**, letting the tools' round
  // absorb the time.
  await expect(checkbox(app, "bind-skill-ama")).toBeChecked();
  await expect(checkbox(app, "bind-skill-lark-cli")).toBeChecked();
});

// Defaults that are checked must be uncheckable, and unchecking must **really be saved**.
//
// This guards the classic failure of "defaults in the wrong place": if defaults were written on the
// render path (say, refilled when skillIds is empty), the render right after the user unchecks would
// fill them back, showing up as "cannot uncheck". So the assertion must go as far as "read back after
// saving"; looking at the checkboxes in the UI alone cannot test it.
test("默认勾上的工具与内置技能可以取消，且取消后保存生效", async ({ app }) => {
  await app.goto("/w/growth/agents/new");
  await expect(checkbox(app, "bind-skill-ama")).toBeChecked();

  // Uncheck one tool, and **both** built-in skills.
  //
  // ⚠️ All built-in skills must be unchecked, not just one. The wrong "refill when empty" implementation
  // (`if (skillIds.length === 0) add the defaults`) **does not trigger** while one is left:
  // verified: unchecking only lark-cli kept the case all green; only unchecking down to zero brought both back.
  // "Unchecking the last one" is exactly what users do to build a pure chat agent, and the only way into that bug.
  await clickCheckbox(app, "tool-bash");
  await clickCheckbox(app, "bind-skill-lark-cli");
  await clickCheckbox(app, "bind-skill-ama");
  await expect(checkbox(app, "tool-bash")).not.toBeChecked();
  await expect(checkbox(app, "bind-skill-lark-cli")).not.toBeChecked();
  await expect(checkbox(app, "bind-skill-ama")).not.toBeChecked();

  await app.getByTestId("agent-name").fill("只读助手");
  await app.getByTestId("agent-system").fill("你只读不写，遇到要改文件的请求先说明原因。");
  await app.getByTestId("agent-submit").click();
  await expect(app).toHaveURL(/\/agents\/agent_/);

  // Detail page: the two unchecked ones must really be gone, and the others still there.
  await expect(checkbox(app, "tool-bash")).not.toBeChecked();
  await expect(checkbox(app, "tool-read")).toBeChecked();

  // The tab is stored in the URL (?tab=); entering with the parameter directly is more deterministic than clicking the switch.
  await app.goto(new URL(app.url()).pathname + "?tab=skills");
  await expect(checkbox(app, "bind-skill-lark-cli")).not.toBeChecked();
  await expect(checkbox(app, "bind-skill-ama")).not.toBeChecked();
});

// ⚠️ Existing agents must not be polluted by defaults: this one is the easiest to miss.
//
// emptyAgentDraft() should only be the initial value of the create page. If someday someone used it as a
// general "fill in the draft" function, the **unchecked** tools of existing agents would be quietly
// filled in and stored on the next save: tools the user never checked would just grow out of nowhere.
//
// Mutation record: the form `{...emptyAgentDraft(), ...toDraft(a)}` **cannot and should not be caught**:
// the later spread always overrides the earlier one, so behavior does not change. What really breaks is
// taking the union
// (`[...new Set([...emptyAgentDraft().builtin_tools, ...d.builtin_tools])]`),
// and that mutation turned this case red.
test("编辑已有 agent 不会被塞进默认勾选", async ({ app }) => {
  // agent_reviewer in the seed has read / grep / glob / bash checked (seed.ts:407),
  // and **not** write / edit / web_fetch / web_search: those last four are the test.
  await app.goto("/w/growth/agents/agent_reviewer");

  for (const tool of ["read", "grep", "glob", "bash"]) {
    await expect(checkbox(app, `tool-${tool}`)).toBeChecked();
  }
  // The unchecked ones must still be unchecked: if filled by defaults they would all become checked.
  for (const tool of ["write", "edit", "web_fetch", "web_search"]) {
    await expect(checkbox(app, `tool-${tool}`)).not.toBeChecked();
  }

  // Same for built-in skills: in the seed this agent is only bound to s-hub-code-review.
  await app.goto("/w/growth/agents/agent_reviewer?tab=skills");
  await expect(checkbox(app, "bind-skill-ama")).not.toBeChecked();
  await expect(checkbox(app, "bind-skill-lark-cli")).not.toBeChecked();

  // And **opening the page should not make it dirty**: with defaults stuffed in, the save button would be
  // lit, and a casual save by the user would store those tools.
  await expect(app.getByTestId("agent-save")).toBeDisabled();
});

// The ids of built-in skills are filtered dynamically from the skill list by source, not literals
// hard-coded in the frontend.
//
// Hard-coded ids fail **silently**: the checkbox stays checked, but it checks an id that does not exist,
// and a dangling skill id only logs a Warn in core/skillark.go, showing up as "skill checked, the model
// has no idea". So this swaps the id given by the backend, and the checks must follow.
//
// ⚠️ **afterEach must be wrapped in describe**: workers=1 with localStorage shared serially, so the
// rename marker must be cleared after use; but a file-level afterEach would run an `app.evaluate`
// **after every case in this file**, adding a page interaction out of nowhere to 13 unrelated cases.
// describe limits it to the one case that needs it.
test.describe("内置技能 id 动态取", () => {
  test.afterEach(async ({ app }) => {
    await app.evaluate(() => window.__amaApi?.clearBuiltinSkillRenameMock?.());
  });

  test("内置技能的默认勾选跟着后端的 id 走，不是写死的", async ({ app }) => {
    // Simulate the backend changing a built-in skill's id (the name stays, so the testid is still bind-skill-ama).
    await app.evaluate(() =>
      window.__amaApi?.renameBuiltinSkillMock?.("skill-builtin-ama", "skill-builtin-ama2"),
    );

    await app.goto("/w/growth/agents/new");

    // The checkbox's isSelected test is `skillIds.includes(s.id)`, and s.id is already the **new** id.
    // An implementation with the old literal hard-coded would show it as unchecked here.
    await expect(checkbox(app, "bind-skill-ama")).toBeChecked();

    await app.getByTestId("agent-name").fill("动态取 id");
    await app.getByTestId("agent-system").fill("验证内置技能 id 不是写死的。");
    await app.getByTestId("agent-submit").click();
    await expect(app).toHaveURL(/\/agents\/agent_/);

    // What is stored must be the new id too: the detail page also judges checks by s.id, and storing the
    // old id would show it as unbound.
    await app.goto(new URL(app.url()).pathname + "?tab=skills");
    await expect(checkbox(app, "bind-skill-ama")).toBeChecked();
  });
});

// ---------------------------------------------------------------------------
// Pick workspace skills from the skill library when creating
// ---------------------------------------------------------------------------

/*
 * This group watches a **correction** of a design decision in `AgentsPage`: the create page used to
 * list only built-in skills, and workspace skills could only be bound on the detail page after
 * creating. Nothing was missing functionally (the detail page's SkillsTab could always bind
 * everything); what was missing was "choosing at the create step". Now there is a "pick from skill
 * library" dialog, and **the whole skill library is still not inlined into the form** (which is what the
 * original comment really guarded against).
 *
 * Two cases, one end each: one walks the full path (what is picked must really be stored in skillIds),
 * one guards the limit of 20.
 */

/** Open the skill library dialog on the create page, and wait for it to really open. */
async function openSkillPicker(app: Page) {
  await app.getByTestId("open-skill-picker").click();
  await expect(app.getByTestId("skill-picker")).toBeVisible();
}

// ⭐ Full path: check a **user-created** skill when creating -> save -> the detail page's SkillsTab also
// shows it as bound.
//
// **It must go to the detail page to assert, not just check the create page.** Inconsistency between the
// two is the most likely problem of this kind of change (checked in the dialog, but the id lost when
// handed back to the draft), and that inconsistency **causes no frontend error**: the create page looks
// fine, and only going to the detail page afterwards shows it was not bound.
test("新建 agent 时能从技能库选空间技能，且详情页看得到", async ({ app }) => {
  await app.goto("/w/growth/agents/new");

  // Before the dialog opens, the form says "no workspace skills bound yet": the three built-ins do not
  // count in this summary (they are visible in their own block above; counting them would give the false
  // statement "2 workspace skills bound").
  await expect(app.getByTestId("picked-skill-summary")).toHaveText("还没有绑定空间技能");

  await openSkillPicker(app);

  // The dialog **should not show built-in skills**: they already have checkboxes in the form body, and
  // listing them in both places would give the same skill two identical testids, which Playwright's
  // strict mode rejects outright.
  await expect(app.getByTestId("skill-picker").getByTestId("bind-skill-ama")).toHaveCount(0);

  // All the user-created skills in the seed are there.
  await expect(app.getByTestId("bind-skill-ticket-triage")).toBeVisible();
  await expect(app.getByTestId("bind-skill-code-review")).toBeVisible();

  // The count includes the 2 built-in skills checked by default: they take slots of Ark's 20 just the same.
  await expect(app.getByTestId("skill-picker-count")).toHaveText("已选 2 / 20");
  await clickCheckbox(app, "bind-skill-ticket-triage");
  await expect(app.getByTestId("skill-picker-count")).toHaveText("已选 3 / 20");

  await app.getByTestId("modal-submit").click();
  await expect(app.getByTestId("skill-picker")).toHaveCount(0);
  // ⭐ The summary area must show **the skill names**, not just a count.
  //
  // This is the core test of this change: this used to assert "1 workspace skill bound", which cannot say
  // which one: after closing the dialog users had no way to confirm they picked the right one.
  // Use `toHaveText` exact match rather than `toContainText`: exact match also guards "no extra skills",
  // while contains stays green under the wrong implementation "lay out every skill".
  await expect(app.getByTestId("picked-skill-summary")).toHaveText("工单分诊");
  // The unpicked one must not appear. Without this line, "the summary area lays out the whole skill
  // library" could not be caught, and that is exactly the shape that was rejected.
  await expect(app.getByTestId("picked-skill-code-review")).toHaveCount(0);

  await app.getByTestId("agent-name").fill("分诊助理");
  await app.getByTestId("agent-system").fill("你按工单分诊技能里的口径判定级别。");
  await app.getByTestId("agent-submit").click();
  await expect(app).toHaveURL(/\/agents\/agent_/);

  // ⭐ The test is here: the detail page's SkillsTab (another render path, another data read) must also
  // consider it bound. Asserting only on the create page, "the id was lost on submit" cannot be caught.
  await app.goto(new URL(app.url()).pathname + "?tab=skills");
  await expect(checkbox(app, "bind-skill-ticket-triage")).toBeChecked();
  // The default checks of built-in skills were not wiped by this change (the dialog hands back the
  // **full** list; written as "only hand back its own", the two built-ins would be erased, a zero-error regression).
  await expect(checkbox(app, "bind-skill-ama")).toBeChecked();
  // The unchecked one must stay unchecked too: otherwise "selected all" and "selected right" cannot be told apart.
  await expect(checkbox(app, "bind-skill-code-review")).not.toBeChecked();
});

// Canceling the dialog must really cancel, without quietly putting the staged checks into the draft.
//
// This guards against the implementation that "edits the draft directly instead of a copy": then the
// changes take effect as soon as the dialog closes, and the "cancel" below is a lie, which users cannot
// see unless they go back and count the summary.
test("技能库弹窗点取消不会落下改动", async ({ app }) => {
  await app.goto("/w/growth/agents/new");
  await openSkillPicker(app);
  await clickCheckbox(app, "bind-skill-ticket-triage");
  await app.getByTestId("skill-picker").getByText("取消").click();

  await expect(app.getByTestId("picked-skill-summary")).toHaveText("还没有绑定空间技能");

  // Open it again: the staged value must resync from the current draft, not stay as it was when the dialog last closed.
  await openSkillPicker(app);
  await expect(checkbox(app, "bind-skill-ticket-triage")).not.toBeChecked();
});

// ⭐ Chips in the summary area can be removed in place, and fold when there are too many.
//
// Two things in one case, because they guard the same thing: **the summary area must faithfully mirror
// "what the user picked"**: things can be added, removed, and too many do not overflow.
//
// ⚠️ The folding half must create its own skills: the seed has only 4 non-built-in skills, not enough
// for SUMMARY_CHIP_LIMIT (6) + 1, so that branch would never be reached, and "the limit is wrong" is all
// green with 4 items.
test("摘要区的技能 chip 能就地取消，超过 6 个折叠成「等 N 个」", async ({ app }) => {
  await app.goto("/w/growth/agents/new");

  // Create enough first: 4 seed + 4 = 8; after selecting all, 8 > 6, so it must fold.
  await app.evaluate(async () => {
    const api = window.__amaApi as unknown as {
      getWorkspaceByKey(key: string): Promise<{ id: string } | null>;
      createSkill(ws: string, draft: Record<string, unknown>): Promise<unknown>;
    };
    const ws = await api.getWorkspaceByKey("growth");
    for (let i = 0; i < 4; i++) {
      await api.createSkill(ws!.id, {
        name: `extra-${i}`,
        displayTitle: `附加技能 ${i}`,
        description: "为了把摘要区堆到折叠阈值用的",
        instructions: "# 附加\n",
        tags: [],
      });
    }
  });
  await app.reload();

  // Check just one, testing the basics first: the name is visible and the x can be clicked.
  await openSkillPicker(app);
  await clickCheckbox(app, "bind-skill-ticket-triage");
  await app.getByTestId("modal-submit").click();
  await expect(app.getByTestId("picked-skill-ticket-triage")).toHaveText("工单分诊");

  // ⭐ Remove in place. The test is not only that the chip is gone: **the check in the dialog must go too**.
  // With an implementation that only changes the summary area without writing back to the draft, the chip
  // disappears while the skill is actually still bound: a zero-error illusion (users think it was removed,
  // and the saved agent still has it).
  await app.getByTestId("unbind-skill-ticket-triage").click();
  await expect(app.getByTestId("picked-skill-summary")).toHaveText("还没有绑定空间技能");
  await openSkillPicker(app);
  await expect(checkbox(app, "bind-skill-ticket-triage")).not.toBeChecked();

  // Select all 8 -> 6 chips + a line "and 2 more".
  for (const name of ["ticket-triage", "code-review", "metrics-sql", "weekly-report"]) {
    await clickCheckbox(app, `bind-skill-${name}`);
  }
  for (let i = 0; i < 4; i++) await clickCheckbox(app, `bind-skill-extra-${i}`);
  await app.getByTestId("modal-submit").click();

  // Count chips rather than only looking at the "and 2 more" line: **only counting chips guards the limit
  // itself** (with an implementation that changed SUMMARY_CHIP_LIMIT to 8 and the overflow text to "and 0
  // more", asserting only the overflow text could not catch it). Count with unbind-skill-*: each laid-out
  // chip carries one, and the folded ones do not.
  await expect(app.locator('[data-testid^="unbind-skill-"]')).toHaveCount(6);
  await expect(app.getByTestId("picked-skill-overflow")).toHaveText("等 2 个");
});

// ⭐ The limit of 20 must be enforced **at the checkbox**.
//
// The backend gate does not fire when creating an Agent but when **starting a chat** (arkSkillsFor in
// core/skillark.go). So relying only on the backend, users could create an agent with 21 skills just
// fine, then hit "this Agent cannot start a chat" somewhere completely unrelated, a sentence that does
// not point to "you picked too many".
//
// ⚠️ The case creates 19 skills itself (2 built-in + 18 = 20, and the 19th created one is exactly over).
// **It must not create fewer**: the seed has only 4 skills, not enough for 20, so that branch would
// never be reached.
test("技能选够 20 个之后不让再选，并说清已达上限", async ({ app }) => {
  await app.goto("/w/growth/agents/new");

  // Create skills by calling the mock directly: much faster and more stable than clicking 19 times in the UI.
  // ⚠️ Look up the workspace id from the key in the URL; do not write `ws_growth`: the URL has the key
  // (growth), the API wants the id (ws_demo in the seed), and hard-coding it would silently create them in
  // a workspace that does not exist.
  await app.evaluate(async () => {
    const api = window.__amaApi as unknown as {
      getWorkspaceByKey(key: string): Promise<{ id: string } | null>;
      createSkill(ws: string, draft: Record<string, unknown>): Promise<unknown>;
    };
    const ws = await api.getWorkspaceByKey("growth");
    for (let i = 0; i < 19; i++) {
      const n = String(i).padStart(2, "0");
      await api.createSkill(ws!.id, {
        name: `bulk-${n}`,
        displayTitle: `凑数技能 ${n}`,
        description: "为了把技能数堆到上限用的",
        instructions: "# 凑数\n",
        tags: [],
      });
    }
  });
  await app.reload();
  await openSkillPicker(app);

  // The 2 built-ins already take slots, so 18 more can be picked.
  await expect(app.getByTestId("skill-picker-count")).toHaveText("已选 2 / 20");
  for (let i = 0; i < 18; i++) {
    await clickCheckbox(app, `bind-skill-bulk-${String(i).padStart(2, "0")}`);
  }

  // At the top: the count must say so, and the unchecked ones can no longer be checked.
  await expect(app.getByTestId("skill-picker-count")).toContainText("已选 20 / 20");
  await expect(app.getByTestId("skill-picker-count")).toContainText("已达上限");
  await expect(checkbox(app, "bind-skill-bulk-18")).toBeDisabled();

  // ⚠️ **Already checked ones must still be uncheckable**: otherwise users at the top could not even swap
  // one and would have to start over. The right scope of "disabled" is "adding", not "every checkbox".
  await expect(checkbox(app, "bind-skill-bulk-00")).toBeEnabled();
  await clickCheckbox(app, "bind-skill-bulk-00");
  await expect(app.getByTestId("skill-picker-count")).toHaveText("已选 19 / 20");
  // After freeing one, the one that could not be clicked can be picked again.
  await expect(checkbox(app, "bind-skill-bulk-18")).toBeEnabled();
});

// Empty state: when there are no non-built-in skills at all, do not give a dialog that opens empty;
// say clearly where to get one.
//
// ⚠️ The entry **must not be hidden entirely** here: if hidden, users would not even know "workspace
// skills can be bound", which is exactly the problem this change solves (see the discipline in
// hiding-a-control-by-default: when the main path is unavailable, give a way out instead of deleting
// the control).
test("技能库为空时，弹窗给的是「去技能库」的引导", async ({ app }) => {
  await app.evaluate(async () => {
    const api = window.__amaApi as unknown as {
      getWorkspaceByKey(key: string): Promise<{ id: string } | null>;
      listSkills(ws: string): Promise<{ id: string; source: string }[]>;
      deleteSkill(id: string): Promise<void>;
    };
    const ws = await api.getWorkspaceByKey("growth");
    const skills = await api.listSkills(ws!.id);
    // Only delete non-built-in ones: built-in skills cannot be deleted (and should not be; they are code constants).
    for (const s of skills.filter((x) => x.source !== "builtin")) {
      await api.deleteSkill(s.id);
    }
  });

  await app.goto("/w/growth/agents/new");
  // The entry is still there: one of the main points of this case.
  await expect(app.getByTestId("open-skill-picker")).toBeVisible();
  await openSkillPicker(app);

  await expect(app.getByTestId("skill-picker-empty")).toBeVisible();
  await expect(app.getByRole("heading", { name: "技能库里还没有技能" })).toBeVisible();
  // ⚠️ It must be located by role: the empty-state text also contains the words "go to skill library",
  // and getByText would hit both that <p> and this button, running into Playwright's strict mode.
  await expect(
    app.getByTestId("skill-picker").getByRole("button", { name: "去技能库" }),
  ).toBeVisible();
});

// ⭐ The "created by me" filter: after checking it the list must get shorter, and what remains must all be mine.
//
// # Why "shorter" is asserted too
//
// Asserting only "the remaining ones are all mine" is **a false green**: a wrong implementation that
// filters out every agent still satisfies it (every item of an empty set meets any condition). So both
// the upper bound (fewer than before) and the lower bound (not 0) must be locked in.
//
// # ⚠️ Existing agents have no createdBy; they must be filtered out
//
// The two in the seed (agent_triage / agent_draft) do not have the field at all: that is the long-term
// state this change leaves behind, not dirty data. Counting them as "created by me" would be wrong (not
// knowing who created it != I created it), so this case also locks in that meaning.
test("筛「我创建的」会让列表变短，且存量 agent 不算我的", async ({ app }) => {
  await app.goto("/w/growth/agents");

  const cards = app.locator('[data-testid^="agent-card-"]');
  // ⚠️ **First wait for the list to really render before counting.** A bare count() does not retry, and
  // the page is still stuck at the startup gate at this point (`AuthGate` is confirming the login state,
  // and **the whole page is empty** during that time: the loading hint became "a text-less spinner that
  // only appears after 300ms", see StartupSpinner), so it reads 0, which would make this case fail with
  // "before not greater than 1", an error that does not point to timing at all.
  // First use a self-retrying assertion as the sync point (agent_reviewer is certainly in the seed).
  await expect(app.getByTestId("agent-card-agent_reviewer")).toBeVisible();
  const before = await cards.count();
  expect(before).toBeGreaterThan(1);

  // In the seed agent_analyst was created by the user named Li Wei, agent_reviewer by the current user,
  // and the other two have no createdBy. So after filtering only agent_reviewer should remain.
  await app.getByTestId("filter-mine-only").click();
  // Same as above: first wait for "the filtered one really disappears" as the sync point, then count. A
  // direct count() would read the old value before re-render, so the real bug "the filter had no effect"
  // could not be caught.
  await expect(app.getByTestId("agent-card-agent_analyst")).toHaveCount(0);

  const after = await cards.count();
  expect(after).toBeGreaterThan(0); // not "everything filtered out"
  expect(after).toBeLessThan(before); // really got shorter
  await expect(app.getByTestId("agent-card-agent_reviewer")).toBeVisible();
  // Those created by others, and those without a recorded creator, should not be there
  await expect(app.getByTestId("agent-card-agent_analyst")).toHaveCount(0);
  await expect(app.getByTestId("agent-card-agent_triage")).toHaveCount(0);
});

// All three states of the creator avatar must be drawn, **especially "unknown creator"**.
//
// Existing agents cannot get a creator backfilled, and the UI must truthfully say "not recorded" rather
// than leave a blank: one missing in a column of avatars looks like broken rendering, misreporting "this
// information does not exist" as "the page has a bug".
test("每行都有创建者头像，存量 agent 显示成「未记录」而不是空白", async ({ app }) => {
  await app.goto("/w/growth/agents");

  // Ones with a real avatar (created by the current user): Radix only renders <img> after the image has loaded.
  const mine = app.getByTestId("agent-creator-agent_reviewer");
  await expect(mine).toBeVisible();

  // Name only, no avatar (Li Wei): falls back to the initial, **not an empty circle**.
  await expect(app.getByTestId("agent-creator-agent_analyst")).toHaveText("李");

  // ⭐ Existing ones: a question mark, not a blank, nor an error.
  const unknown = app.getByTestId("agent-creator-agent_triage");
  await expect(unknown).toBeVisible();
  await expect(unknown).toHaveText("?");
});

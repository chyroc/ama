/**
 * How platform-injected context (`<harness:lark>`) is shown in the UI.
 *
 * Background: messages from Feishu get a structured block appended by the platform (chat_id /
 * thread_id / the command to fetch history), attached as the **second content block** of
 * `user.message`. Users have a right to know what the platform told the agent on their behalf, but
 * it is something you only look at when debugging, so it is a collapsible section, collapsed by default.
 *
 * The backend half (how the block is rendered, how malicious content is blocked) is in
 * `server/feishu/larkprompt_test.go`. This only tests the frontend: **is it pulled out, is it
 * collapsed by default, can you see it when opened**.
 */

import { expect, seedInjectedContextMock, test } from "./fixtures";

/** The first user.message of run_1 in the seed; that sentence is visible word for word in the bubble. */
const USER_TEXT = "帮我看下上周华东区的 GMV 表现";

test("平台注入块不进聊天气泡，折叠区默认收起、可展开", async ({ app }) => {
  await app.goto("/w/growth/runs/run_1");
  await seedInjectedContextMock(app, "run_1");
  // After changing the seed, re-enter the page: the hook writes to the store, and the page has already rendered once.
  await app.goto("/w/growth/runs/run_1");

  const bubble = app.getByTestId("chat-user-message").first();
  await expect(bubble).toBeVisible();

  // -- 1. The user's sentence shows as usual, and the injected block is **not in the bubble** --
  //
  // This is the main value of the feature: without pulling it out, the bubble would be "the user's
  // words + a big chunk of XML meant for the model".
  await expect(bubble).toContainText(USER_TEXT);
  await expect(bubble).not.toContainText("harness:lark");
  await expect(bubble).not.toContainText("chat_id");

  // -- 2. The collapsible section is collapsed by default --
  //
  // ⚠️ **The test is "the body is not visible", not `toHaveCount(0)`.** When HeroUI's Disclosure is
  // collapsed, the content **is still in the DOM** (hidden with aria-hidden; see its Styling
  // Reference), so a count assertion is **always true and tests nothing**. This comes from the same
  // source as the two e2e lessons in the repo (toHaveCount(0) is satisfied first in the
  // skeleton/not-yet-rendered frame).
  const toggle = app.getByTestId("injected-context-toggle");
  await expect(toggle).toBeVisible();
  await expect(app.getByTestId("injected-context-body")).toBeHidden();

  // -- 3. Opening it shows the content, and it is **structured**, not a lump of raw text --
  await toggle.click();
  const body = app.getByTestId("injected-context-body");
  await expect(body).toBeVisible();
  // Facts are rendered one by one as key -> value (<dl>), and each value can be selected and copied on its own.
  await expect(body).toContainText("chat_id");
  await expect(body).toContainText("oc_seedchat0001");

  // ⚠️ **Markup meant for the model should not reach the screen.** This is the main reason for this
  // rework: the first version dumped the whole raw block into a <pre>, so `<harness:lark>`, `**data**`
  // and ```bash fences all showed up, and it read like a broken page. Now the tags are parsed away and
  // markdown goes to the renderer.
  await expect(body).not.toContainText("<harness:lark>");
  await expect(body).not.toContainText("**数据**");

  // The static guidance for the model is folded one more level: collapsed by default (it is the same
  // for every message, pure boilerplate).
  const guidance = app.getByTestId("injected-guidance-toggle");
  await expect(guidance).toBeVisible();
  await expect(app.getByTestId("injected-guidance-body")).toBeHidden();
  await guidance.click();
  await expect(app.getByTestId("injected-guidance-body")).toBeVisible();

  // Click again to collapse: the section works both ways, and testing only expand would miss "clicked
  // and it won't collapse".
  await toggle.click();
  await expect(body).toBeHidden();
});

test("没有平台注入时不出现折叠区", async ({ app }) => {
  // No hook call: the runs in the seed have no injected block to begin with (they did not come from Feishu).
  await app.goto("/w/growth/runs/run_1");

  await expect(app.getByTestId("chat-user-message").first()).toBeVisible();

  // ⚠️ Before asserting "should not appear", **you must first wait out the render window**; the
  // toBeVisible above is that gate. Without it, `toHaveCount(0)` is satisfied in the frame before the
  // chat renders, so this case is green whether or not the implementation is right (the repo note
  // e2e-absence-assertion-needs-render-delay).
  //
  // Using count is right here: with no injected block **the whole component returns null**, and the
  // trigger never enters the DOM (a different thing from the "collapsed but in the DOM" case above).
  await expect(app.getByTestId("injected-context-toggle")).toHaveCount(0);
});

test("事件视图的摘要只显示用户原话，详情仍是完整原始 content", async ({ app }) => {
  await app.goto("/w/growth/runs/run_1");
  await seedInjectedContextMock(app, "run_1");
  await app.goto("/w/growth/runs/run_1?view=events");

  // The summary of that row in the event list must be readable text, not XML; otherwise every message
  // from Feishu looks identical in the list, and you cannot tell them apart at a glance.
  //
  // ⚠️ Locate by `data-evt-type`, not by text: the summary is truncated to 80 characters, so matching
  // hasText on the whole sentence fails because of the truncation (the first version fell into this).
  // Row testids in the event view use the `evt-row-*` prefix, **not** `event-*` (those are the paired
  // entries of the chat view; the two have different meanings, see the comment in RawEventList.tsx).
  const row = app.locator('[data-evt-type="user.message"]').first();
  await expect(row).toBeVisible();
  await expect(row).toContainText("帮我看下上周");
  await expect(row).not.toContainText("harness:lark");

  // But **the detail panel must still give the full raw content**: the event view means "faithfully
  // show the raw event Ark returned", it must match run_events in the database one by one, and the
  // injected block is part of that event anyway.
  await row.click();
  await expect(app.getByTestId("evt-content").first()).toContainText("harness:lark");
});

/**
 * After Feishu cards / merged forwards are expanded into the user message, the bubble must show them
 * **line by line**.
 *
 * # What this guards against
 *
 * The backend moved the expanded body from the `<harness:lark>` block into the user message (see
 * `server/feishu/larkUserText`), so run history finally shows the real content instead of "[card]".
 * But that body is **multi-line plain text**, and the user bubble does not go through md-box like
 * agent replies do: `.chat-message__content` only sets font size/line height/color, **no
 * white-space** (see ui/Chat.tsx). Without `whitespace-pre-wrap`, all newlines are swallowed and
 * "1. …\n2. …" runs together into one paragraph.
 *
 * ⚠️ **The test uses the computed CSS value, not textContent.** `toContainText` is not sensitive to
 * newlines: it still passes when everything runs into one line, so it tests nothing.
 */
test("卡片/合并转发展开后的多行正文，在气泡里保留换行且不会撑爆页面", async ({ app }) => {
  // Built in the real shape: the placeholder as the first-line marker, followed by the expanded multi-line body.
  const forwarded = [
    "[合并转发，共 3 条]",
    "1. TLS告警：错误数超阈值",
    "2. 这个昨天也报过",
    "3. 已经在跟进了",
  ].join("\n");

  await app.goto("/w/growth/runs/run_1");
  await seedInjectedContextMock(app, "run_1", forwarded);
  await app.goto("/w/growth/runs/run_1");

  const bubble = app.getByTestId("chat-user-message").first();
  await expect(bubble).toBeVisible();

  // -- 1. The content really reaches the screen (the user's original request: stop showing only "[card]") --
  await expect(bubble).toContainText("TLS告警：错误数超阈值");
  await expect(bubble).toContainText("已经在跟进了");
  // The fact that "this was originally a forward" must be kept.
  await expect(bubble).toContainText("[合并转发，共 3 条]");

  // -- 2. Newlines are **really** kept --
  //
  // The test is the computed value of white-space: only pre-wrap / pre-line render \n as a line break.
  const content = bubble.locator('[data-slot="chat-message-content"]');
  const whiteSpace = await content.evaluate((el) => getComputedStyle(el).whiteSpace);
  expect(["pre-wrap", "pre-line"]).toContain(whiteSpace);

  // -- 3. Height is capped, so a long forward does not push the reply below out of view --
  //
  // A merged forward is at most 20 items / 4000 characters (maxExpandedTotalLen in shared/infra/lark);
  // without a cap, one message could fill the whole page.
  const maxHeight = await content.evaluate((el) => getComputedStyle(el).maxHeight);
  expect(maxHeight).not.toBe("none");
});

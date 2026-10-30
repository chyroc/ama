/**
 * e2e for product suggestions (Suggestion).
 *
 * ⚠️ **It is the third kind of "something is wrong" thing in this repo; do not call it feedback, and
 * it is not issue either.** feedback is the machine-collected error stream (the user types nothing),
 * issue is a workspace-level human-written to-do, and this page is **platform-level** human-written
 * opinion: it has no workspace dimension, so it lives in global settings
 * (`/preferences?tab=suggestions`). See the table in server/core/README-suggestion.md for how the
 * three differ; the UI text deliberately avoids the word "feedback".
 *
 * # Why this group was added (this page had no e2e at all before)
 *
 * The frontend main-bundle cleanup changed the Markdown rendering path to dynamic loading with
 * `React.lazy` (`MarkdownMessage` -> `MarkdownBody` -> md-box), and **this page is one of the places
 * that change landed**. Back then it was only checked by hand in a real browser ("renders
 * h1/strong/code/li"), with no automated check to catch it: the next person touching that path would
 * not know what they broke.
 *
 * So the most important cases in this group are the two "the Markdown body really rendered into
 * elements" ones: **they assert that `strong` / `h2` / `li` / `pre code` really exist**, not that the
 * container exists. The container (`.md-message`) is **also** on the Suspense fallback: the fallback
 * is the same text plus `whitespace-pre-wrap`, made to look the same on purpose (see note 2 in
 * MarkdownMessage.tsx). Asserting only the container, the dynamic import could break completely and
 * stay on the plain-text fallback forever, while the cases stay all green.
 *
 * # It runs in local mode, so what is asserted is LocalApiClient's behavior
 *
 * `page.route` **never fires** here (the mock sends no network requests at all), so failures can only
 * be injected through the mock hooks in fixtures. Same as 14-api-keys / 17-issues; see the notes in
 * fixtures.ts.
 *
 * # ⚠️ Two things **cannot be tested** with the mock, so no cases are written for them on purpose
 * # (writing them would give always-true false greens)
 *
 * 1. **The non-platform-staff view**. `viewerIsStaff` is the module constant
 *    `MOCK_SUGGESTION_STAFF = true` in the mock (the comment in api.ts explains why: demo mode must
 *    show the whole flow), and no hook can flip it.
 *    So the three branches "regular users don't see the author column / have no status control /
 *    don't see others' suggestions" **never run even once in local mode**. Forcing a case would only
 *    give an always-true assertion. What really covers them are the real-wiring cases in the backend
 *    `httpd/suggestion_test.go`. Covering them on the frontend too would need a
 *    `setSuggestionStaffMock` hook in the mock first; that is a change to api.ts and should not be
 *    mixed into this "add e2e" round.
 * 2. **The error state for failed writes** (the `suggestion-write-error` / `suggestion-detail-error`
 *    boxes). **None** of the suggestion write methods go through `guardWrite` (only the three read
 *    methods go through `guardRead`), so `failWritesMock` has no effect on them at all: a case would
 *    quietly run on the success path and then fail on "the error box did not appear", a failure that
 *    does not point to the real cause at all. Same trap as the one noted for `failTerminalInputMock`
 *    in fixtures.ts (that method does not go through guardWrite either).
 *
 * The read-failure one **can be tested** (`listSuggestions` goes through `guardRead`), so it is written.
 */

import { expect, failReadsMock, hangReadsMock, restoreReadsMock, test } from "./fixtures";

type App = import("@playwright/test").Page;

/** Open the product suggestions tab (in global settings, unrelated to workspaces). */
async function openSuggestions(app: App) {
  await app.goto("/preferences?tab=suggestions");
  await expect(app.getByTestId("create-suggestion")).toBeVisible();
}

/**
 * Submit a suggestion. **After submitting, the page automatically opens the detail** (that is product
 * behavior: attachments must hang off an existing suggestion, so "submit first, then upload"), so
 * this waits for the detail dialog to appear before returning.
 *
 * ⚠️ **The submit button must be scoped to `create-suggestion-modal`**; the global
 * `getByTestId("modal-submit")` cannot be used: between the create dialog closing and the detail
 * dialog opening, both FormModals are mounted in the DOM at the same time (RAC's exit animation waits
 * for animationend), so a global locator hits a strict mode violation. This is not a product bug; it
 * is the inherent shape of "two FormModals on the same page" (same as 14-api-keys).
 */
async function createSuggestion(app: App, title: string, body?: string) {
  await app.getByTestId("create-suggestion").click();
  if (title) await app.getByTestId("suggestion-title").fill(title);
  if (body) await app.getByTestId("suggestion-body").fill(body);
  await app.getByTestId("create-suggestion-modal").getByTestId("modal-submit").click();
}

/** Close the detail dialog (the bottom primary button is "Back"), and wait for it to really leave the DOM. */
async function closeDetail(app: App) {
  await app.getByTestId("suggestion-detail-modal").getByTestId("modal-submit").click();
  await expect(app.getByTestId("suggestion-detail-modal")).toHaveCount(0);
}

test.describe("产品建议", () => {
  test("空态说清这是干什么的，且不叫它「反馈」", async ({ app }) => {
    await openSuggestions(app);

    // The seed has no suggestions (same as issues / apiKeys; they are things users write).
    await expect(app.getByText("还没有提过建议")).toBeVisible();
    await expect(app.getByTestId("suggestion-list")).toHaveCount(0);

    // The footer line is the precondition for this feature daring to require real names; it must be on the page.
    await expect(app.getByText(/只有平台团队看得到/)).toBeVisible();

    /*
      ⚠️ **The UI text must not contain the word "feedback"** (a hard rule in README-suggestion.md).
      That word is already taken in this repo by "automatic error collection", and using it again here
      would pull the user's mental model back to a different thing; and this is a pure wording problem
      that code review cannot see, only an assertion can catch.

      The scope is limited to the cards on this page, not the whole body: global settings also has a
      "report a problem" button (FeedbackButton) and the left tab bar, which are **a different thing**
      and are supposed to use that name.

      ⚠️ Locate "the innermost element containing both the create button and the footer line"; **do not
      use `xpath=ancestor::*[3]`**: that hard-codes "go up three levels" as the expected value, while the
      number of levels of Card / Card.Content is a HeroUI implementation detail, and one component
      release could silently point it at another element: too narrow and the assertion is always true
      (false green), too wide and it pulls in FeedbackButton and is always false.

      `.last()` takes the innermost one (Playwright locators return in document order, and among nested
      matches the last is the deepest), so the scope does not grow to include the page shell.
    */
    const card = app
      .locator("div")
      .filter({ has: app.getByTestId("create-suggestion") })
      .filter({ hasText: "你的建议只有平台团队看得到" });
    // First prove this scope really frames something; otherwise the not.toContainText below would be
    // always true because "no element matched", exactly the kind of false green this repo keeps hitting.
    await expect(card.last()).toContainText("提一条建议");
    await expect(card.last()).not.toContainText("反馈");
  });

  test("提一条建议：只填标题就能建，默认落 open，并自动进详情", async ({ app }) => {
    await openSuggestions(app);
    await createSuggestion(app, "希望运行历史能按 agent 筛");

    /*
      After submitting it **opens the detail automatically**: deliberate product behavior, not
      incidental. The attachment table has a suggestion_id foreign key, images can only hang off an
      existing suggestion, and without jumping there the user would have no place to paste images after
      submitting. It is an assertion because "not jumping to the detail after submitting" is a zero-error
      regression: the list still gains a row, only that person can never find where to paste images.
    */
    const detail = app.getByTestId("suggestion-detail-modal");
    await expect(detail).toBeVisible();
    await expect(detail).toContainText("希望运行历史能按 agent 筛");
    // The default status is "unresolved", in a neutral color rather than danger (marking every new
    // suggestion red would make them all look like incidents).
    await expect(detail.getByTestId("suggestion-status-open")).toBeVisible();

    await closeDetail(app);

    // Also in the list, with a status Chip.
    const list = app.getByTestId("suggestion-list");
    await expect(list).toContainText("希望运行历史能按 agent 筛");
    await expect(list.getByTestId("suggestion-status-open")).toBeVisible();
  });

  test("只填正文时标题从第一行提取，Markdown 标记被剥掉", async ({ app }) => {
    await openSuggestions(app);

    // Shares the same deriveIssueTitle with issues (the backend core/suggestion.go also calls it directly).
    // The first line is **an image with alt**: without stripping image syntax, the link rule would turn
    // it into `!<alt text>`, which has readable characters, so the title would really be wrong. An image
    // without alt cannot test this (the comments in the two 17-issues cases record this comparison).
    await createSuggestion(app, "", "![建议截图](https://example.com/a.png)\n\n# 侧栏希望能收起");

    await closeDetail(app);
    const list = app.getByTestId("suggestion-list");
    await expect(list).toContainText("建议截图");
    // The exclamation mark is the test: it says "the image syntax was only treated as a normal link".
    await expect(list).not.toContainText("!建议截图");
    await expect(list).not.toContainText("![");
  });

  test("标题与正文都为空 → 拒绝，且弹窗保持打开", async ({ app }) => {
    await openSuggestions(app);
    await app.getByTestId("create-suggestion").click();
    await app.getByTestId("create-suggestion-modal").getByTestId("modal-submit").click();

    /*
      ⚠️ **The error must appear inside the dialog (`modal-error`).**

      The first time this case ran it exposed a real bug (fixed in this round, see the comment in
      SuggestionsTab): the catch used to be `fail(e); return false;`, and `fail` only wrote the error into
      `suggestion-write-error` on the card. On failure the dialog deliberately stays open, so that line
      landed right under the overlay, and **the user could not see a single word**: it showed up as
      "clicking submit does nothing".

      The test is pinned on `modal-error`. The box on the card will still be there (it carries the
      second-level "report this problem" entry, which the dialog does not have), so **it does not assert
      that box is absent**: that would make a correct behavior the expected failure. What matters is the
      copy inside the dialog.

      The text matches the backend errSuggestionEmpty() / LocalApiClient word for word.
    */
    const modalError = app.getByTestId("create-suggestion-modal").getByTestId("modal-error");
    await expect(modalError).toHaveText("请至少填写标题或正文");
    // The dialog must still be open: closing it would lose what the user just wrote.
    await expect(app.getByTestId("suggestion-title")).toBeVisible();

    // Adding a body makes it pass.
    await app.getByTestId("suggestion-body").fill("补上正文之后就能提交了");
    await app.getByTestId("create-suggestion-modal").getByTestId("modal-submit").click();
    await expect(app.getByTestId("suggestion-detail-modal")).toBeVisible();

    /*
      ⚠️ **After success, the red failure box on the card must go away.**

      The second fix in the same round: nobody cleared the error written by `fail()`, so after "fail
      first, edit, then succeed", the user closing the detail would see a no-longer-true error hanging on
      the card, and it carries the "report this problem" entry, effectively guiding the user to report a
      fault that does not exist.

      The assertion comes **after** closing the detail: that box is under the detail dialog, and without
      closing it `toBeHidden` / `toHaveCount(0)` would be always true because it is covered: a false green.
    */
    await closeDetail(app);
    await expect(app.getByTestId("suggestion-write-error")).toHaveCount(0);
  });

  /**
   * ⚠️ **The most important case in this group: the body really went through md-box, not staying on
   * the Suspense plain-text fallback.**
   *
   * The test must be "elements were rendered", not "the container exists": the fallback uses the same
   * `.md-message` class name (on purpose, see MarkdownMessage.tsx), so asserting only the container is
   * still green when the dynamic import is completely broken. Four structures are checked, one each,
   * because they come from different processing stages of md-box (inline emphasis / block heading /
   * list / code highlighting); checking only strong would not notice code blocks collapsing.
   */
  test("详情页正文走 Markdown 渲染：标题/加粗/列表/代码块都成了真元素", async ({ app }) => {
    await openSuggestions(app);
    await createSuggestion(
      app,
      "对话页希望支持代码块折叠",
      [
        "## 现在的问题",
        "",
        "长代码块把整个对话**撑得很长**。",
        "",
        "- 翻历史要滚很久",
        "- 手机上尤其明显",
        "",
        "```bash",
        "ama run --agent demo",
        "```",
      ].join("\n"),
    );

    const md = app.getByTestId("suggestion-detail-modal").locator(".md-message");
    await expect(md).toBeVisible();

    /*
      md-box is lazy-loaded: the fallback comes first (a plain-text version of the same text), and it
      switches to the real render once the chunk arrives. So every assertion here must be one that
      **retries**, like `toBeVisible` / `toContainText`, to wait for the chunk to land; you cannot grab
      innerHTML once and compare.
    */
    await expect(md.locator("h2")).toContainText("现在的问题");
    await expect(md.locator("strong")).toHaveText("撑得很长");
    await expect(md.locator("li").first()).toContainText("翻历史要滚很久");
    await expect(md.locator("pre code")).toContainText("ama run --agent demo");

    /*
      Reverse test: Markdown markers in the source **should not reach the screen together with their symbols**.

      This one exists specifically to catch the "back to plain text" regression: the four assertions above
      go red when md-box breaks because the elements are missing, but if someone changed the fallback into
      "a hand-rolled simple renderer", the elements might exist while `##` and `**` still remain in the
      text. Only this one can expose that half-baked implementation.
    */
    await expect(md).not.toContainText("## 现在的问题");
    await expect(md).not.toContainText("**撑得很长**");
  });

  test("回复也走 Markdown 渲染，且平台团队的回复带徽章", async ({ app }) => {
    await openSuggestions(app);
    await createSuggestion(app, "希望能导出运行历史");

    const detail = app.getByTestId("suggestion-detail-modal");
    // With no replies yet, give a sentence, not an empty ul.
    await expect(detail.getByText("还没有回复")).toBeVisible();
    await expect(detail.getByTestId("suggestion-reply-list")).toHaveCount(0);

    // An empty reply cannot be sent: the button should be disabled (not erroring only after clicking).
    await expect(detail.getByTestId("suggestion-reply-submit")).toBeDisabled();

    await detail.getByTestId("suggestion-reply-body").fill("已经在排期了，`v1.3` 会带上**导出**");
    await detail.getByTestId("suggestion-reply-submit").click();

    const replies = detail.getByTestId("suggestion-reply-list");
    await expect(replies).toBeVisible();

    /*
      Reply bodies and the suggestion body use **the same** MarkdownMessage, but in a different mount
      position (inside li, and this part is rendered from a list). They are checked separately because
      "a lazy component rendered in a list" is not the same as "rendered in a fixed position": once the
      Suspense boundary is written inside the map, each item waits for the chunk separately, and getting
      it wrong shows up as "some items rendered and some did not".
    */
    await expect(replies.locator("code")).toHaveText("v1.3");
    await expect(replies.locator("strong")).toHaveText("导出");
    await expect(replies).not.toContainText("**导出**");

    // In the mock the current user is platform staff, so this reply carries the official badge: that is
    // what users most want to find.
    await expect(detail.getByTestId("reply-staff")).toBeVisible();

    /*
      A freshly posted reply should not show "edited" (createdAt === updatedAt).

      ⚠️ This `toHaveCount(0)` is safe: the `toBeVisible` calls above already waited for the reply to
      really render, so this is not satisfied on the "not yet rendered" frame (this repo has fallen into
      that false green before). The test is that **there must be a positive assertion before it**.
    */
    await expect(detail.getByText("已编辑")).toHaveCount(0);

    // Close the detail; the reply count appears on the list (that number is computed, not stored).
    // ⚠️ Same as the attachment case: pin the test on the badge's own element, do not count the whole
    // row text: in the staff view the author email contains digits, which would make the assertion always true.
    await closeDetail(app);
    await expect(app.getByTestId("suggestion-reply-count")).toHaveText("1");
  });

  test("平台方能改状态，列表跟着变", async ({ app }) => {
    await openSuggestions(app);
    await createSuggestion(app, "搜索希望支持拼音");

    const detail = app.getByTestId("suggestion-detail-modal");
    // In the mock the current user is platform staff, so the status control is there.
    // ⚠️ On the real backend this control is **not rendered** for the person who submitted the
    // suggestion (the backend also blocks it; an author changing it gets 403); that branch cannot be
    // tested in local mode, see the file header.
    await detail.getByTestId("suggestion-status-select").click();
    await app.getByRole("option", { name: "已解决" }).click();

    await expect(detail.getByTestId("suggestion-status-resolved")).toBeVisible();
    await closeDetail(app);

    // The list changed too: not refreshing the list after the change is a zero-error regression (the
    // detail is right, the list is still old).
    const list = app.getByTestId("suggestion-list");
    await expect(list.getByTestId("suggestion-status-resolved")).toBeVisible();
    await expect(list.getByTestId("suggestion-status-open")).toHaveCount(0);

    // Still resolved after a refresh (really stored, not just an optimistic update in memory).
    await app.reload();
    await expect(app.getByTestId("suggestion-list").getByTestId("suggestion-status-resolved")).toBeVisible();
  });

  test("改状态不会把正文清空", async ({ app }) => {
    /*
      ⚠️ This case stands alone; it checks a heuristic that is **easy to lose in a refactor**.

      The status-change PATCH sends `{title:"", body:"", status}`, relying on the backend rule "both
      empty = leave them alone this time" to keep the body. If someone changes those two empty strings to
      not being sent, or that backend heuristic gets "cleaned up", the result is **the whole body
      disappears after one status change**, with no error in the UI; the user only finds out days later
      that what they wrote is gone.

      The test is on Markdown elements rather than plain text: if the body is gone, `strong` simply does not exist.
    */
    await openSuggestions(app);
    await createSuggestion(app, "", "希望**批量删除**运行历史");

    const detail = app.getByTestId("suggestion-detail-modal");
    await expect(detail.locator(".md-message strong")).toHaveText("批量删除");

    await detail.getByTestId("suggestion-status-select").click();
    await app.getByRole("option", { name: "已解决" }).click();
    await expect(detail.getByTestId("suggestion-status-resolved")).toBeVisible();

    // The body is still there.
    await expect(detail.locator(".md-message strong")).toHaveText("批量删除");

    // Still there after closing and reopening; this step cannot be skipped: the assertion above might
    // just mean the component has not re-rendered yet.
    await closeDetail(app);
    await app.reload();
    await app.getByTestId("suggestion-list").getByText("批量删除").click();
    await expect(
      app.getByTestId("suggestion-detail-modal").locator(".md-message strong"),
    ).toHaveText("批量删除");
  });

  test("列表标题可点开详情", async ({ app }) => {
    await openSuggestions(app);
    await createSuggestion(app, "希望侧栏能记住折叠状态");
    await closeDetail(app);

    // The title itself is a button (not only the "view" button is clickable): the most natural click
    // target in a list is the title, and offering only the right-side button would make people think
    // this one cannot be opened.
    await app.getByTestId("suggestion-list").getByText("希望侧栏能记住折叠状态").click();
    await expect(app.getByTestId("suggestion-detail-modal")).toBeVisible();
  });

  test("附件：非法类型被挡下，图片能传上去", async ({ app }) => {
    await openSuggestions(app);
    await createSuggestion(app, "贴张图说明问题");

    const detail = app.getByTestId("suggestion-detail-modal");

    // .exe is not on the allowlist.
    //
    // ⚠️ This only checks that **the frontend can show the backend's rejection to the user**; it is not
    // the security boundary. The real decision is in the backend (sniffing + cross-check with the
    // extension), locked in by Go cases.
    //
    // Unlike issues: the suggestion side has no drop zone, only a button + hidden input, and no
    // validation on the frontend either (SuggestionAttachmentPicker hands the file straight to the api,
    // and LocalApiClient / the backend rejects it), so the error lands on the InlineError in the detail page.
    await detail.getByTestId("suggestion-attachment-input").setInputFiles({
      name: "payload.exe",
      mimeType: "application/octet-stream",
      buffer: Buffer.from("MZ\x90\x00fake"),
    });
    await expect(app.getByTestId("suggestion-detail-error")).toContainText("不支持的文件类型");

    // A real image uploads fine.
    await detail.getByTestId("suggestion-attachment-input").setInputFiles({
      name: "shot.png",
      mimeType: "image/png",
      buffer: Buffer.from(
        "89504e470d0a1a0a0000000d49484452000000010000000108060000001f15c489",
        "hex",
      ),
    });
    // AttachmentList is a component shared with issues, so the testid prefix is issue- (reused on
    // purpose; see the comment in SuggestionsTab about "pass an empty workspaceId to reuse it").
    await expect(app.getByTestId("issue-attachment-shot.png")).toBeVisible();

    // Close the detail; the attachment count appears on the list (that badge is deliberately not rendered at 0).
    //
    // ⚠️ **Do not use `list.toContainText("1")`**: in the mock the current user is platform staff, and
    // the list row renders the author email `alice@example.com`, which contains a "1", so that assertion
    // is green even when the count is not rendered at all (verified). So the test is pinned on the
    // badge's own element, with exact text.
    await closeDetail(app);
    const attachmentBadge = app.getByTestId("suggestion-attachment-count");
    await expect(attachmentBadge).toHaveText("1");
  });

  test("删除建议：二次确认后从列表消失", async ({ app }) => {
    await openSuggestions(app);
    await createSuggestion(app, "待删除的建议");
    await closeDetail(app);

    await app.getByTestId("suggestion-list").getByText("待删除的建议").click();
    await closeDetail(app);

    // The delete button is on the list row (not in the detail).
    //
    // ⚠️ Use the `suggestion-delete-*` testid, **not `getByRole("button", {name: <the "delete" label>})`**: the title
    // in the list row is also a button, and accessible name matching is substring-based, so a title
    // containing the word for "delete" -> strict mode violation. That failure reads like "there are two
    // delete buttons", not pointing at all to "the title happens to contain that word".
    const list = app.getByTestId("suggestion-list");
    await list.locator('[data-testid^="suggestion-delete-"]').click();

    // Irreversible actions need a second confirmation, and the description must include the title:
    // without it, deleting two in a row makes mistakes easy.
    await expect(app.getByText(/「待删除的建议」/)).toBeVisible();
    await app.getByTestId("confirm-danger").click();

    // There was only one; after deleting, back to the empty state.
    await expect(app.getByText("还没有提过建议")).toBeVisible();
    await expect(app.getByTestId("suggestion-list")).toHaveCount(0);

    /*
      ⚠️ **Assert again after a refresh**: the empty state above might be the gap during the rerender
      after deletion (this repo has seen `toHaveCount(0)` satisfied in the gap after a save).
      A refresh reads what was really stored.
    */
    await app.reload();
    await expect(app.getByText("还没有提过建议")).toBeVisible();
  });

  test("建议是账号级的：一个空间都没有时也能提", async ({ app }) => {
    /*
      Clear out all workspaces: this is the hardest check of "platform-level". If this page still
      depended on some workspace (say, listWorkspaces and put the first one into the request), it would
      go empty or error here.

      This is also one reason the feature exists: **newcomers who have not joined any workspace yet are
      exactly the people with the most to say** (the SuggestionsTab file header says so). Putting it
      under a workspace would stop them from giving feedback.
    */
    await app.goto("/workspaces");
    await app.evaluate(() => window.__amaApi?.clear());

    await openSuggestions(app);
    await createSuggestion(app, "新人第一天就想说的话");
    await closeDetail(app);
    await expect(app.getByTestId("suggestion-list")).toContainText("新人第一天就想说的话");

    // Still there after a refresh: it should not follow any workspace's lifecycle.
    await app.reload();
    await expect(app.getByTestId("suggestion-list")).toContainText("新人第一天就想说的话");
  });

  test("读取失败时给重试入口，不是空列表", async ({ app }) => {
    // **"internal" must be passed explicitly**: without code it takes the access_denied branch (no retry
    // button, see 13-load-errors.spec.ts). What this checks is the retryable branch.
    await failReadsMock(app, "internal");
    await app.goto("/preferences?tab=suggestions");

    /*
      A read failure must show itself and offer retry: degrading into an empty list would make the user
      think "I never submitted a suggestion" and submit the same one again. This page matters especially:
      its empty state looks almost the same as "read failed" (both are a card with nothing in it).
    */
    await expect(app.getByTestId("load-error")).toBeVisible();
    await expect(app.getByTestId("load-retry")).toBeVisible();
    // The empty-state text should **not** appear: that is exactly the shape of "showing a failure as no data".
    await expect(app.getByText("还没有提过建议")).toHaveCount(0);

    await restoreReadsMock(app);
    await app.getByTestId("load-retry").click();
    await expect(app.getByTestId("create-suggestion")).toBeVisible();
  });

  test("加载中显示骨架屏，不是空列表", async ({ app }) => {
    await hangReadsMock(app);
    // hangReadsMock only sets a marker; you must navigate/reload yourself for it to take effect (see fixtures.ts).
    await app.goto("/preferences?tab=suggestions");

    // Stays on the isLoading branch: the create button has not appeared, nor the empty-state text.
    // "Treating isLoading as the empty state" is a very common mistake, and it causes zero errors.
    await expect(app.getByTestId("create-suggestion")).toHaveCount(0);
    await expect(app.getByText("还没有提过建议")).toHaveCount(0);
  });

  test("从全局设置的 tab 栏能进这一页", async ({ app }) => {
    await app.goto("/preferences");
    await app.getByTestId("prefs-tab-suggestions").click();

    await expect(app).toHaveURL(/\/preferences\?tab=suggestions$/);
    await expect(app.getByTestId("create-suggestion")).toBeVisible();
    // ⚠️ An unrecognized tab falls back to the default page (the condition in PreferencesPage); a missing
    // condition shows up as "clicking that tab shows general settings", so assert that this page really
    // rendered, not just that the URL changed.
    await expect(app.getByText(/只有平台团队看得到/)).toBeVisible();
  });
});

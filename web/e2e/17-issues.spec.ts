import { expect, test } from "./fixtures";

/**
 * e2e for issues (Issue).
 *
 * # It runs in local mode, so what is asserted is LocalApiClient's behavior
 *
 * `page.route` **never fires** here (the mock sends no network requests at all), so failure injection
 * relies on the mock hooks in fixtures. See docs/guides/frontend.md for this.
 *
 * # This group focuses on three things
 *
 * 1. **Automatic title extraction really is wired into the create path**: adding a function that is
 *    never called on the main path is the kind of "all green but the feature has no effect" this repo
 *    has seen before;
 * 2. **"Both title and body empty" is rejected**, with error text matching the backend word for word;
 * 3. **Type validation for attachment uploads** (this frontend check is only instant feedback; the real
 *    boundary is in the backend).
 */

test.describe("问题", () => {
  test("提一个问题：只填标题就能建，默认落 open / P2", async ({ app }) => {
    await app.goto("/w/growth/issues");

    // The empty state shows first (the seed deliberately has no issues).
    await expect(app.getByText("还没有问题")).toBeVisible();

    await app.getByTestId("new-issue").click();
    await app.getByTestId("issue-title").fill("登录后偶发白屏");
    await app.getByTestId("modal-submit").click();

    // It shows in the list, with the default status and priority Chips.
    const list = app.getByTestId("issue-list");
    await expect(list).toContainText("登录后偶发白屏");
    await expect(list.getByTestId("issue-status-open")).toBeVisible();
    await expect(list.getByTestId("issue-priority-p2")).toBeVisible();
  });

  test("只填正文时标题从第一行提取，Markdown 标记被剥掉", async ({ app }) => {
    await app.goto("/w/growth/issues");
    await app.getByTestId("new-issue").click();

    // The first line of the body is **an image without alt**, and the real text is on the second line.
    // Without stripping, the title would become a string of `![](...)`, exactly what this case guards against.
    await app
      .getByTestId("issue-body")
      .fill("![](https://example.com/shot.png)\n\n# 点了登录按钮之后整页白掉");
    await app.getByTestId("modal-submit").click();

    const list = app.getByTestId("issue-list");
    await expect(list).toContainText("点了登录按钮之后整页白掉");
    // Neither hash signs nor image syntax should appear in the title.
    await expect(list).not.toContainText("![](");
    await expect(list).not.toContainText("# 点了登录");
  });

  test("正文第一行是带 alt 的图片时，标题取 alt 而不是整段语法", async ({ app }) => {
    // ⚠️ **This one cannot be merged with the previous one; it checks a different rule.**
    //
    // The previous one uses an image **without alt** (`![](url)`); in that case, even with no image-syntax
    // stripping at all, only a `!` is left after stripping: no readable characters, so it gets skipped by
    // the "this line has no content" rule, and the title is still right. **Confirmed during mutation
    // testing: commenting out the image-stripping step leaves the previous case green.**
    //
    // Only an image with alt separates the two rules: without stripping, the link rule turns it into
    // `!<alt text>`, which **does** have readable characters, so the title really becomes `!<alt text>`.
    await app.goto("/w/growth/issues");
    await app.getByTestId("new-issue").click();
    await app.getByTestId("issue-body").fill("![登录页截图](https://example.com/a.png)");
    await app.getByTestId("modal-submit").click();

    const list = app.getByTestId("issue-list");
    await expect(list).toContainText("登录页截图");
    // The exclamation mark is the test: it says "the image syntax was not stripped properly, only treated
    // as a normal link".
    await expect(list).not.toContainText("!登录页截图");
  });

  test("标题与正文都为空 → 拒绝，且弹窗保持打开", async ({ app }) => {
    await app.goto("/w/growth/issues");
    await app.getByTestId("new-issue").click();

    // Submit without filling in either.
    await app.getByTestId("modal-submit").click();

    // The error text matches the backend errIssueEmpty() / LocalApiClient word for word.
    await expect(app.getByTestId("issue-form-error")).toHaveText("请至少填写标题或正文");
    // The dialog must still be open: closing it would lose what the user just wrote.
    await expect(app.getByTestId("issue-title")).toBeVisible();

    // Adding a body makes it pass.
    await app.getByTestId("issue-body").fill("补上正文之后就能提交了");
    await app.getByTestId("modal-submit").click();
    await expect(app.getByTestId("issue-list")).toContainText("补上正文之后就能提交了");
  });

  test("详情页：Markdown 渲染正文、改状态、加评论、改评论", async ({ app }) => {
    await app.goto("/w/growth/issues");
    await app.getByTestId("new-issue").click();
    await app.getByTestId("issue-title").fill("方舟凭证过期");
    await app
      .getByTestId("issue-body")
      .fill("## 现象\n\n所有对话都报 **401**。\n\n- 空间 A 复现\n- 空间 B 也复现");
    await app.getByTestId("modal-submit").click();

    // Go to the detail page.
    await app.getByTestId("issue-list").getByText("方舟凭证过期").click();

    // Markdown really was rendered: `**401**` became <strong>, not literal asterisks.
    // md-box is lazy-loaded; wait for it to arrive (the fallback is the same plain text, so assert strong first).
    await expect(app.locator(".md-message strong").filter({ hasText: "401" })).toBeVisible();
    await expect(app.locator(".md-message h2")).toContainText("现象");
    // List items should be real <li> too.
    await expect(app.locator(".md-message li").first()).toContainText("空间 A 复现");

    // Change status in place: no dialog.
    await app.getByTestId("issue-status-inline").click();
    await app.getByRole("option", { name: "处理中" }).click();
    await expect(app.getByTestId("issue-status-in_progress").first()).toBeVisible();

    // Add a comment.
    await expect(app.getByTestId("issue-comment-empty")).toBeVisible();
    await app.getByTestId("issue-comment-input").fill("我这边也复现了，key 是上周换的");
    await app.getByTestId("issue-comment-submit").click();

    const comments = app.getByTestId("issue-comment-list");
    await expect(comments).toContainText("我这边也复现了");
    // A freshly posted comment should not show "edited" (createdAt === updatedAt).
    await expect(app.getByTestId("issue-comment-edited")).toHaveCount(0);

    // Edit this comment -> the "edited" marker appears.
    const item = comments.locator("li").first();
    const id = await item.getAttribute("data-testid");
    const commentId = (id ?? "").replace("issue-comment-", "");
    await app.getByTestId(`issue-comment-edit-${commentId}`).click();
    await app.getByTestId(`issue-comment-editor-${commentId}`).fill("更正：key 是这周换的");
    await app.getByTestId(`issue-comment-save-${commentId}`).click();

    await expect(comments).toContainText("更正：key 是这周换的");
    await expect(app.getByTestId("issue-comment-edited")).toBeVisible();
  });

  test("改标题之后列表跟着变", async ({ app }) => {
    await app.goto("/w/growth/issues");
    await app.getByTestId("new-issue").click();
    await app.getByTestId("issue-title").fill("原来的标题");
    await app.getByTestId("modal-submit").click();

    await app.getByTestId("issue-list").getByText("原来的标题").click();
    await app.getByTestId("issue-edit").click();
    await app.getByTestId("issue-title").fill("改过之后的标题");
    await app.getByTestId("modal-submit").click();

    // The detail page title follows.
    await expect(app.getByRole("heading", { name: "改过之后的标题" })).toBeVisible();

    // Back on the list it is the new title too.
    await app.getByTestId("issue-back").click();
    const list = app.getByTestId("issue-list");
    await expect(list).toContainText("改过之后的标题");
    await expect(list).not.toContainText("原来的标题");
  });

  test("按状态筛选", async ({ app }) => {
    await app.goto("/w/growth/issues");

    for (const title of ["第一个问题", "第二个问题"]) {
      await app.getByTestId("new-issue").click();
      await app.getByTestId("issue-title").fill(title);
      await app.getByTestId("modal-submit").click();
    }

    // Close the second one.
    await app.getByTestId("issue-list").getByText("第二个问题").click();
    await app.getByTestId("issue-status-inline").click();
    await app.getByRole("option", { name: "已关闭" }).click();
    await expect(app.getByTestId("issue-status-closed").first()).toBeVisible();
    await app.getByTestId("issue-back").click();

    // Only open ones: the second should disappear.
    await app.getByTestId("issue-filter-status").click();
    await app.getByRole("option", { name: "待处理" }).click();

    const list = app.getByTestId("issue-list");
    await expect(list).toContainText("第一个问题");
    await expect(list).not.toContainText("第二个问题");

    // Switch back to all; both are there.
    await app.getByTestId("issue-filter-status").click();
    await app.getByRole("option", { name: "全部状态" }).click();
    await expect(list).toContainText("第一个问题");
    await expect(list).toContainText("第二个问题");
  });

  test("附件：非法类型被前端挡下", async ({ app }) => {
    await app.goto("/w/growth/issues");
    await app.getByTestId("new-issue").click();
    await app.getByTestId("issue-title").fill("要传附件的问题");
    await app.getByTestId("modal-submit").click();
    await app.getByTestId("issue-list").getByText("要传附件的问题").click();

    // .exe is not on the allowlist.
    //
    // ⚠️ This **only checks the frontend's instant feedback**; it is not the security boundary. The real
    // decision is in the backend (sniffing + cross-check with the extension), locked in by Go cases.
    await app.getByTestId("issue-attachment-input").setInputFiles({
      name: "payload.exe",
      mimeType: "application/octet-stream",
      buffer: Buffer.from("MZ\x90\x00fake"),
    });
    await expect(app.getByTestId("issue-attachment-error")).toHaveText("不支持的文件类型");

    // A real image uploads fine.
    await app.getByTestId("issue-attachment-input").setInputFiles({
      name: "shot.png",
      mimeType: "image/png",
      buffer: Buffer.from(
        "89504e470d0a1a0a0000000d49484452000000010000000108060000001f15c489",
        "hex",
      ),
    });
    await expect(app.getByTestId("issue-attachment-shot.png")).toBeVisible();
    await expect(app.getByTestId("issue-attachment-error")).toHaveCount(0);
  });

  test("删除问题回到列表", async ({ app }) => {
    await app.goto("/w/growth/issues");
    await app.getByTestId("new-issue").click();
    await app.getByTestId("issue-title").fill("待删除的问题");
    await app.getByTestId("modal-submit").click();

    await app.getByTestId("issue-list").getByText("待删除的问题").click();
    await app.getByTestId("issue-delete").click();
    await app.getByTestId("confirm-danger").click();

    // Back on the list, and that one is gone (it was the only one, so back to the empty state).
    await expect(app.getByText("还没有问题")).toBeVisible();
  });

  test("侧栏能进问题页", async ({ app }) => {
    await app.goto("/w/growth");
    await app.getByRole("link", { name: "问题" }).click();
    await expect(app).toHaveURL(/\/w\/growth\/issues$/);
    await expect(app.getByRole("heading", { name: "问题" })).toBeVisible();
  });

  /**
   * The author must be shown with **real name and avatar**, not an email initial.
   *
   * # ⚠️ The assertions must expose two bugs: "profile not passed" and "key not lowercased"
   *
   * When `<UserAvatar>` is missing `profile` (or looks up `profiles` with an email that was not
   * lowercased, which silently misses), **the feature still looks like it works**: the avatar circle is
   * there, the tooltip is there, it just always degrades to the email initial. So **assertions like
   * "UserAvatar exists" or "there is an avatar element" hold just as well for the broken
   * implementation**; they are always true and expose no bug. This repo has fallen into such false greens
   * (same as the one in 32-revisions.spec.ts, for exactly the same reason).
   *
   * The tests chosen here **only hold when profile really is passed in**:
   *
   *  1. `<img>` exists and its src is a data URI: Radix only renders img after the image has loaded, and
   *     avatarUrl **can only come from profile** (Issue has no such field).
   *     Without it, src is undefined and the node never enters the DOM.
   *  2. The accessible name is "creator: <the Chinese display name>": that name is also only in profile.
   *     Without it, it falls back to the email `alice@example.com`, so the two can be told apart.
   */
  test("列表页作者显示真实头像，不是邮箱首字母", async ({ app }) => {
    await app.goto("/w/growth/issues");
    await app.getByTestId("new-issue").click();
    await app.getByTestId("issue-title").fill("列表页要有作者头像");
    await app.getByTestId("modal-submit").click();

    const row = app.getByTestId("issue-list").locator("li").first();
    const id = (await row.getByTestId(/^issue-row-/).getAttribute("data-testid")) ?? "";
    const issueId = id.replace("issue-row-", "");

    const author = app.getByTestId(`issue-author-${issueId}`);
    await expect(author).toBeVisible();

    // ⭐ Test one: the real avatar loaded (the mock gives the current user an inline data URI; this machine
    // cannot reach external image hosts, see data/api.ts). avatarUrl only comes from profiles.
    await expect(author.locator("img")).toHaveAttribute("src", /^data:image\/svg\+xml/);

    // ⭐ Test two: the screen reader reads the display name, not the email.
    //
    // ⚠️ aria-label is on the **outer Tooltip.Trigger button**, not on the Avatar carrying the testId
    // (the testId is deliberately on Avatar: when the image has not loaded, <img> does not exist).
    await expect(app.getByRole("button", { name: "创建者：张三" })).toBeVisible();

    // Lock in the opposite: it must not be just an initial. When profile is missing, this is exactly "C".
    await expect(author).not.toHaveText("C");

    // The list page **only gives the avatar**, with the name in the tooltip: the line "created by xxx"
    // should no longer appear.
    await expect(app.getByTestId("issue-list")).not.toContainText("由 ");
  });

  test("详情页作者与评论者都显示真实姓名与头像", async ({ app }) => {
    await app.goto("/w/growth/issues");
    await app.getByTestId("new-issue").click();
    await app.getByTestId("issue-title").fill("详情页要有作者头像");
    await app.getByTestId("modal-submit").click();
    await app.getByTestId("issue-list").getByText("详情页要有作者头像").click();

    // ---- issue author ----
    const author = app.getByTestId("issue-author");
    await expect(author.locator("img")).toHaveAttribute("src", /^data:image\/svg\+xml/);
    await expect(app.getByRole("button", { name: "创建者：张三" })).toBeVisible();
    await expect(author).not.toHaveText("C");

    // The detail page **also needs a visible name next to the avatar** (the list page deliberately has none).
    // This also locks in "the name goes through displayNameOf, not the bare email": the display name is shown.
    //
    // ⚠️ Use a separate testId instead of walking up from the avatar node with `xpath=..`: in practice that
    // gets **the Tooltip.Trigger div** (`tooltip__trigger`, containing only the avatar), while the name is on
    // its sibling, so the assertion gets an empty string: that is locating the wrong element, not a wrong
    // implementation.
    await expect(app.getByTestId("issue-author-name")).toHaveText("张三");

    // ---- commenter ----
    await app.getByTestId("issue-comment-input").fill("评论者也要有头像");
    await app.getByTestId("issue-comment-submit").click();

    const comments = app.getByTestId("issue-comment-list");
    const item = comments.locator("li").first();
    const commentId = ((await item.getAttribute("data-testid")) ?? "").replace(
      "issue-comment-",
      "",
    );

    const commentAuthor = app.getByTestId(`issue-comment-author-${commentId}`);
    await expect(commentAuthor.locator("img")).toHaveAttribute(
      "src",
      /^data:image\/svg\+xml/,
    );
    await expect(commentAuthor).not.toHaveText("C");
    // The role prefix in comments differs from the issue author ("commenter" instead of "creator").
    await expect(app.getByRole("button", { name: "评论者：张三" })).toBeVisible();

    // The name is still visible (the old font-medium email line was replaced by the display name),
    // and **it is no longer a bare email**: exactly what this change set out to fix.
    await expect(item).toContainText("张三");
    await expect(item).not.toContainText("alice@example.com");
  });
});

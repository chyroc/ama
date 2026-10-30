/**
 * The **information architecture and five states** of the "pick an Ark API Key from a Volcengine
 * account" picker.
 *
 * # What this group guards is "can users understand it", not "does it work"
 *
 * The functional side is locked in by 20-agent-ark (all three levels can pick, plaintext never goes
 * through the frontend). This locks in the reasons for the redesign:
 *
 * 1. **Accounts and keys are two levels**. The first version laid both out as equally weighted button
 *    lists: 10 account squares followed by 10 key squares, so users saw 20 identical-looking items and
 *    could not tell which were accounts and which were keys, nor that "keys only appear after picking an
 *    account". So this locks in: **with no account picked, the key section does not render at all**,
 *    rather than rendering as an empty list (which would say "there are no keys here" when it means
 *    "you have not picked yet").
 *
 * 2. **20 keys with similar names must be distinguishable**. In practice one account had 10 keys, 6 named
 *    `api-key-2026082…`, and **the masks were all identical** (Volcengine gives 32 asterisks). So the only
 *    way to tell them apart is the creation time: it must really be rendered, and search must find it.
 *
 * 3. **Each of the five states is presented on its own**, especially these two pairs:
 *    - **not connected != not authorized**: giving an "authorize" button when not connected, a user
 *      clicking it only gets a 422, with no way to know they need an admin to configure the server (the
 *      same trap was hit on the authorization page).
 *    - **loading != empty list**: the same mistake as that batch of blank screens in useAsyncData.
 *
 * 4. **Plaintext never appears in any frontend response**. This matters most, and is the premise this
 *    path exists on.
 *
 * # ⚠️ Two hooks must be restored in afterEach
 *
 * The markers of `setArkFetchStateMock` and `hangReadsMock` both live in **separate** localStorage keys
 * (the `reset()` before each case cannot wipe them, on purpose). workers=1, shared serially, so leftover
 * markers would make all later specs run on that branch, with failure messages not pointing to this at all.
 */

import type { Page } from "@playwright/test";
import {
  clearArkFetchStateMock,
  clearArkMock,
  expect,
  setArkFetchStateMock,
  test,
} from "./fixtures";

/** The account in the seed that can list keys (10 keys, 6 with the same prefix -> only time tells them apart). */
const ACCOUNT_OK = "2128853309";
/** The account in the seed that **deliberately returns an empty array** -> takes "this account has no keys". */
const ACCOUNT_EMPTY = "2101887744";
/** The account in the seed that is **deliberately missing** -> takes AccessDenied (a different thing from the previous one). */
const ACCOUNT_DENIED = "2100444922";

/** `api-key-20260822165212`, used to assert selection. */
const KEY_OK = "4169105";
/** Another key with the same prefix as KEY_OK, differing by a few digits (`api-key-20260827175443`). */
const KEY_SIMILAR = "4173882";

/** Open the workspace-level dialog: the shortest path of the three. */
async function openPicker(app: Page) {
  // ⚠️ First go back to "never configured": the seed configures one by default (see
  // seedArkCredentials in data/api.ts), and then the empty-state `ark-setup` does not render, `ark-replace`
  // is in its place, and the next line would time out with "element not found", an error that does not
  // point to "the seed already configured one".
  await clearArkMock(app);
  await app.goto("/w/growth/settings?tab=ark");
  await app.getByTestId("ark-setup").click();
}

/**
 * Pick an account in the picker.
 *
 * The account level is a **searchable dropdown** (a precondition that folds away after selection to
 * make room for step two), so the popover must be opened before clicking an item; you cannot just
 * `getByText().click()` as when it was laid out flat.
 *
 * ⚠️ **Wait for the account list to finish loading before opening the popover.**
 *
 * StrictMode runs effects twice in dev, and the second run first sets `accounts` to null and then to a
 * new array; RAC's ListBox then rebuilds its collection, and **the items in an already open popover are
 * removed from the DOM**. The symptom is an occasional `element was detached from the DOM` (measured: in
 * 6 batch runs, 3 had one failure each, and not even the same one each time), which looks like flakiness,
 * an animation, or pollution from another case, and never points to the real cause.
 *
 * Tricks like `hover()` to "wait for it to stop moving" **do not work**: the element is not moving, it
 * is being replaced. The right way is to wait for the stable state "the list is in place": the account
 * count text appearing means accounts has settled (it only renders when accounts is non-empty).
 */
async function pickAccount(app: Page, id: string) {
  // See the function header: wait for accounts to settle (the count text appears) before opening, or
  // it is occasionally detached.
  await expect(app.getByTestId("ark-account-combo")).toContainText("个账号");
  await app.getByTestId("ark-account-combo").getByRole("button").click();
  await app.getByTestId(`ark-account-${id}`).click();
}

/** The key level is also a dropdown; open it first too. */
async function openKeyList(app: Page) {
  await app.getByTestId("ark-key-combo").getByRole("button").click();
}

/** Pick a key. Same as pickAccount: wait for the popover animation to stop before clicking. */
async function pickKeyItem(app: Page, id: string) {
  await openKeyList(app);
  await app.getByTestId(`ark-key-${id}`).click();
}

/** Switch the source to "fill in manually". */
async function chooseManual(app: Page) {
  await app.getByTestId("ark-source-select").click();
  await app.getByRole("option", { name: "手动填写" }).click();
}

test.afterEach(async ({ page }) => {
  // See the file header: not restoring would pollute all later specs.
  await clearArkFetchStateMock(page);
});

// ---------------------------------------------------------------------------
// 1. The two-level relationship between accounts and keys
// ---------------------------------------------------------------------------

test("两级关系：没选账号时 key 那一段压根不出现，选了才出现", async ({ app }) => {
  await openPicker(app);
  await expect(app.getByTestId("ark-fetch-picker")).toBeVisible();

  // The first level is there (the account is a precondition), and the second level **does not exist at all**.
  await expect(app.getByTestId("ark-fetch-step-account")).toBeVisible();
  await expect(app.getByTestId("ark-fetch-step-key")).toHaveCount(0);
  // ⚠️ Not "rendered as an empty list": that would say "this account has no keys" when it means
  // "you have not picked an account yet".
  await expect(app.getByTestId("ark-key-combo")).toHaveCount(0);
  await expect(app.getByTestId("ark-key-empty")).toHaveCount(0);
  // A sentence explains what it is waiting for, instead of leaving a blank (a blank looks like broken loading).
  await expect(app.getByTestId("ark-key-waiting")).toBeVisible();

  // Only after an account is picked does the second level appear.
  await pickAccount(app, ACCOUNT_OK);
  await expect(app.getByTestId("ark-fetch-step-key")).toBeVisible();
  await expect(app.getByTestId("ark-key-combo")).toBeVisible();
  // The "pick an account first" line goes away with it.
  await expect(app.getByTestId("ark-key-waiting")).toHaveCount(0);
});

test("账号多到要搜：10 个账号，搜得到也筛得掉", async ({ app }) => {
  await openPicker(app);

  // The count itself must be stated: users need to know how big the set they are picking from is.
  await expect(app.getByTestId("ark-account-combo")).toContainText("10");

  await app.getByTestId("ark-account-combo").getByRole("button").click();
  // All 10 are there.
  await expect(app.getByTestId(`ark-account-${ACCOUNT_OK}`)).toBeVisible();
  await expect(app.getByTestId(`ark-account-${ACCOUNT_DENIED}`)).toBeVisible();

  // Search coze -> only that one remains; the others are filtered out.
  await app.getByTestId("ark-account-search").fill("coze");
  await expect(app.getByTestId("ark-account-2113029408")).toBeVisible();
  await expect(app.getByTestId(`ark-account-${ACCOUNT_OK}`)).toHaveCount(0);

  // The account ID is searchable too: users often only have that string of digits.
  await app.getByTestId("ark-account-search").fill(ACCOUNT_OK);
  await expect(app.getByTestId(`ark-account-${ACCOUNT_OK}`)).toBeVisible();
});

// ---------------------------------------------------------------------------
// 2. How to tell apart 20 keys with similar names
// ---------------------------------------------------------------------------

test("名字相近的 key 靠创建时间区分：时间真的渲染出来了", async ({ app }) => {
  await openPicker(app);
  await pickAccount(app, ACCOUNT_OK);
  await openKeyList(app);

  // These two names differ by only a few digits, and the masks are identical.
  const a = app.getByTestId(`ark-key-${KEY_OK}`);
  const b = app.getByTestId(`ark-key-${KEY_SIMILAR}`);
  await expect(a).toContainText("api-key-2026082");
  await expect(b).toContainText("api-key-2026082");

  // ⚠️ **The mask does not help**: Volcengine gives both the same string of asterisks. This assertion
  // pins down the wrong design "the mask is enough to tell them apart": if someone ever uses the mask to
  // tell them apart, this reminds them the masks are identical.
  const maskedA = await a.locator("span.font-mono").nth(1).innerText();
  const maskedB = await b.locator("span.font-mono").nth(1).innerText();
  expect(maskedA).toBe(maskedB);

  // So the creation time must be there, and **different** for the two: it is the only way to tell them apart.
  const timeA = app.getByTestId(`ark-key-created-${KEY_OK}`);
  const timeB = app.getByTestId(`ark-key-created-${KEY_SIMILAR}`);
  await expect(timeA).toBeVisible();
  await expect(timeB).toBeVisible();
  await expect(timeA).not.toHaveText(await timeB.innerText());
});

test("key 多到要搜：能按名称搜，也能按创建时间搜", async ({ app }) => {
  await openPicker(app);
  await pickAccount(app, ACCOUNT_OK);

  // 10 keys, and the hint says "look at the creation time when names are similar".
  // The count follows the mock (11 after adding the "same prefix, different domain" mutation case).
  await expect(app.getByTestId("ark-key-count")).toContainText("11");

  // By name: the dog one is the only one not named with a timestamp.
  await app.getByTestId("ark-key-search").fill("dog");
  await expect(app.getByTestId("ark-key-4165330")).toBeVisible();
  await expect(app.getByTestId(`ark-key-${KEY_OK}`)).toHaveCount(0);

  // ⚠️ **Search by creation time**. This is what the requirement "time must be searchable" is tested on:
  // users often remember "the one created on Aug 27", and 8-27 is only in the time, not in the name
  // (the name is a UTC timestamp 20260827…, while what is searched here is the locally formatted 2026/08/27).
  //
  // It also locks in that ListBox.Item's textValue must include the time: RAC's built-in filter does a
  // contains on that string, and with only name this would go red right away.
  await app.getByTestId("ark-key-search").fill("2026/08/27");
  await expect(app.getByTestId(`ark-key-${KEY_SIMILAR}`)).toBeVisible();
  await expect(app.getByTestId("ark-key-4165330")).toHaveCount(0);

  // No match != this account has no keys (the latter is a different branch with different text).
  await app.getByTestId("ark-key-search").fill("绝不存在的关键词");
  await expect(app.getByTestId("ark-key-no-match")).toBeVisible();
  await expect(app.getByTestId("ark-key-empty")).toHaveCount(0);
});

// ---------------------------------------------------------------------------
// 2.5 Source dropdown: automatic by default; the manual input does not appear by default
// ---------------------------------------------------------------------------

test("默认从火山选，输入框不出现；切到手动才出现", async ({ app }) => {
  await openPicker(app);

  // The default option is "pick from Volcengine account", so the two-level picker is there and **the manual input is not**.
  await expect(app.getByTestId("ark-source-select")).toContainText("从火山账号选");
  await expect(app.getByTestId("ark-fetch-step-account")).toBeVisible();
  // ⚠️ This is what the requirement "no input box by default" is tested on.
  await expect(app.getByTestId("ark-key-input")).toHaveCount(0);

  // Switch to manual -> the input box appears, and the two-level picker folds away entirely (the user
  // has said they are not taking that path).
  await chooseManual(app);
  await expect(app.getByTestId("ark-key-input")).toBeVisible();
  await expect(app.getByTestId("ark-fetch-step-account")).toHaveCount(0);
  await expect(app.getByTestId("ark-key-combo")).toHaveCount(0);

  /*
    ⚠️ The "fetch from Volcengine account · recommended / plaintext never goes through the browser" title
    must fold away too.

    Two reasons: it says "recommended" while the user just clearly chose something else, a contradiction;
    more importantly, **"plaintext never goes through the browser" is false on the manual path** (manually
    entered plaintext does go through the frontend, it just is not stored). Leaving a false security
    promise on screen is far worse than looking bad.
  */
  await expect(app.getByTestId("ark-fetch-picker")).not.toContainText("明文不经过浏览器");
  await expect(app.getByTestId("ark-fetch-picker")).not.toContainText("推荐");
});

test("走自动却没挑 key 就保存：报「请先选一把」，不报「API Key 不能为空」", async ({ app }) => {
  await openPicker(app);
  await app.getByTestId("modal-submit").click();

  /*
    ⚠️ The error must point to a control the user can see.

    At this moment the manual input **is not in the UI**, so "API Key cannot be empty" is a sentence
    users cannot act on: they would look for an input box that does not exist. And that error used to
    hang on the manual input's FieldError; when the manual input is not rendered it has nowhere to show
    at all, appearing as "clicked save and nothing happened".
  */
  const err = app.getByTestId("modal-error");
  await expect(err).toBeVisible();
  await expect(err).toContainText("选一把");
  await expect(err).not.toContainText("不能为空");
});

test("手动填写这条路仍然通（切来源之后）", async ({ app }) => {
  await openPicker(app);
  await chooseManual(app);
  await app.getByTestId("ark-key-input").fill("ARK00000000000000000000000000001");
  await app.getByTestId("modal-submit").click();

  await expect(app.getByTestId("ark-masked")).toContainText("*");
  // The plaintext does not stay on the page.
  await expect(app.getByText("ARK00000000000000000000000000001")).toHaveCount(0);
});

// ---------------------------------------------------------------------------
// 2.6 Creator: which key did I create
// ---------------------------------------------------------------------------
//
// ⚠️ This column is **the only answer to "which one is mine"**. In practice the 10 keys under one
// Volcengine account belonged to 10 different people, and neither the names (`api-key-2026082…`) nor
// the masks (all identical) show ownership: without it, picking a key means guessing among other
// people's credentials.
//
// Volcengine puts the creator in `sys:ark:createdBy` inside `Tags`, with values like
// `IAMUser/<UserId>/<username>`, and **the username part has two shapes** (both seen in real data):
// a bare username `bob`, and a full email `heidi@example.com`.
// AMA's identity is the email, so both must be compared: comparing only one would leave people with
// the other shape never recognizing their own keys.

test("创建人：自己建的标出来，别人建的显示名字", async ({ app }) => {
  await openPicker(app);
  await pickAccount(app, ACCOUNT_OK);
  await openKeyList(app);

  // Shape one: a bare username, exactly the @ prefix of my email -> judged "created by me".
  await expect(app.getByTestId(`ark-key-${KEY_OK}`)).toContainText("我创建的");
  // Shape two: **a full email** -> must also be judged "created by me".
  // An implementation comparing only bare usernames would miss this, and those people would never see their own keys marked.
  await expect(app.getByTestId(`ark-key-${KEY_SIMILAR}`)).toContainText("我创建的");

  // Created by someone else: show the name, and it must **not** be marked as mine.
  const other = app.getByTestId("ark-key-4173851");
  await expect(other).toContainText("bob");
  await expect(other).not.toContainText("我创建的");

  /*
    ⚠️ Someone else's **full email**. This blocks two kinds of wrong implementations:
      · "treat anything with @ as me"
      · "compare only the @ prefix, ignore the domain"
    Both would mark a colleague's key as mine, and users would pick a key thinking they have permission.
  */
  const otherEmail = app.getByTestId("ark-key-4174906");
  await expect(otherEmail).toContainText("heidi@example.com");
  await expect(otherEmail).not.toContainText("我创建的");

  /*
    ⚠️⚠️ **Same prefix as mine, different domain** (`alice@other.example.org`).

    This one came out of mutation testing: only it separates the two implementations "compare the full
    email string" and "compare only the @ prefix"; the latter would mark this key as mine. And this is
    not a made-up input: the same prefix under different domains may well be different people, and
    Volcengine's createdBy has shown the full email shape, so together that means "same name, different person".
  */
  const sameName = app.getByTestId("ark-key-3155903");
  await expect(sameName).toContainText("alice@other.example.org");
  await expect(sameName).not.toContainText("我创建的");

  // An old key without createdBy: show nothing, and **do not default to treating it as mine**.
  const noTag = app.getByTestId("ark-key-4170233");
  await expect(noTag).not.toContainText("我创建的");
  await expect(noTag).not.toContainText("创建人");
});

test("能按创建人搜：搜同事的名字，找出他建的那几把", async ({ app }) => {
  await openPicker(app);
  await pickAccount(app, ACCOUNT_OK);

  /*
    ⚠️ This locks in that createdBy must go into `textValue`: RAC's built-in filter only looks at that
    string, not the rendered JSX. With the creator column added, "search a colleague's name to find the
    one they created" is the most natural use, and if the creator is only rendered into the row without
    going into textValue, it cannot be searched, with no error.
  */
  await app.getByTestId("ark-key-search").fill("bob");
  await expect(app.getByTestId("ark-key-4173851")).toBeVisible();
  await expect(app.getByTestId(`ark-key-${KEY_OK}`)).toHaveCount(0);
});

// ---------------------------------------------------------------------------
// 3. The five states
// ---------------------------------------------------------------------------

test("态·未接入：只给去控制台的入口，**不给「去授权」按钮**", async ({ app }) => {
  await setArkFetchStateMock(app, "unavailable");
  await openPicker(app);

  await expect(app.getByTestId("ark-fetch-unavailable")).toBeVisible();
  // The reason given by the server must be shown (it is more specific than the fallback text).
  await expect(app.getByTestId("ark-fetch-unavailable")).toContainText("app_key");

  // ⚠️⚠️ This is the assertion this spec most needs to guard: **not connected != not authorized**.
  // With an authorize button, a user clicking it only gets a 422, with no way to know they need an admin
  // to configure the server.
  await expect(app.getByTestId("ark-fetch-authorize-action")).toHaveCount(0);
  await expect(app.getByTestId("ark-fetch-authorize")).toHaveCount(0);

  // Going to the console is the only thing they can do right now, so that entry must be there.
  await expect(app.getByTestId("ark-fetch-console-link")).toBeVisible();

  // ⚠️⚠️ **The manual input must be there, without the user switching the source first**.
  //
  // This is the only really dangerous part of the "hide the input box by default" change: if, when
  // automatic fetching is unavailable, it still faithfully rendered by "the user chose automatic", users
  // would see a picker that cannot be used with nothing below it, and be completely stuck, not even
  // knowing they could type it in. The forced fallback in the effectiveSource line exists for this.
  await expect(app.getByTestId("ark-key-input")).toBeVisible();
  // The source dropdown must also show "fill in manually" and be locked: staying on an unselectable
  // "pick from Volcengine" would be a false statement.
  await expect(app.getByTestId("ark-source-select")).toContainText("手动填写");
});

test("态·未授权：给授权入口（与未接入的呈现不同）", async ({ app }) => {
  await setArkFetchStateMock(app, "unauthorized");
  await openPicker(app);

  await expect(app.getByTestId("ark-fetch-authorize")).toBeVisible();
  // This state **does give** the button: exactly the opposite of the previous case, and only the two
  // together lock in "the two states must not be mixed".
  await expect(app.getByTestId("ark-fetch-authorize-action")).toBeVisible();
  // And the "not connected" branch should not appear.
  await expect(app.getByTestId("ark-fetch-unavailable")).toHaveCount(0);

  // When not authorized there should be no account or key list (nothing can be fetched then).
  await expect(app.getByTestId("ark-account-combo")).toHaveCount(0);
  await expect(app.getByTestId("ark-key-combo")).toHaveCount(0);
  // But manual input is still available: authorizing means a trip elsewhere, so people must not be stuck here.
  await expect(app.getByTestId("ark-key-input")).toBeVisible();
});

test("态·加载中：骨架，**不是**空列表也不是「没有账号」", async ({ app }) => {
  /*
    ⚠️ Use `loading` instead of hangReadsMock: the latter also pins getArkCredential, so the whole Ark
    card stays on the skeleton and **the dialog cannot open at all**, while the picker's loading state is
    inside the dialog.

    Why it must be pinned: the mock's delay is only 90ms, and without pinning the assertions always run on
    "loaded", so an implementation mistake like "treating isLoading as the empty state" stays all green
    (that is how that batch of blank screens in useAsyncData slipped through).
  */
  await setArkFetchStateMock(app, "loading");
  await openPicker(app);

  // The loading state has its own testid, and right now **none of the four conclusive branches may
  // appear**: each of them means "there is already an answer", and there is not one yet.
  await expect(app.getByTestId("ark-fetch-loading")).toBeVisible();
  await expect(app.getByTestId("ark-account-empty")).toHaveCount(0);
  await expect(app.getByTestId("ark-key-empty")).toHaveCount(0);
  await expect(app.getByTestId("ark-fetch-unavailable")).toHaveCount(0);
  await expect(app.getByTestId("ark-account-error")).toHaveCount(0);

  // Manual input works as usual: loading should not block the user.
  await expect(app.getByTestId("ark-key-input")).toBeVisible();
});

test("态·加载中（第二级）：选了账号、key 还在飞时也是骨架", async ({ app }) => {
  await openPicker(app);
  // First load and select the account list under ready, then switch to loading: only then does the second level fetch.
  await pickAccount(app, ACCOUNT_OK);
  await expect(app.getByTestId("ark-key-combo")).toBeVisible();

  await setArkFetchStateMock(app, "loading");
  // Switch to another account to make the second level fetch again; this time it never returns.
  await pickAccount(app, "2113029408");

  await expect(app.getByTestId("ark-key-loading")).toBeVisible();
  // ⚠️ Loading **must not** be shown as "this account has no keys": the same mistake as that batch of
  // blank screens in useAsyncData (treating "no data yet" as "no data").
  await expect(app.getByTestId("ark-key-empty")).toHaveCount(0);
  await expect(app.getByTestId("ark-key-no-match")).toHaveCount(0);
});

test("态·空列表：说清该去哪建，且与「没权限」不是同一句话", async ({ app }) => {
  await openPicker(app);
  await pickAccount(app, ACCOUNT_EMPTY);

  // This account really has no keys (the mock gives an empty array).
  await expect(app.getByTestId("ark-key-empty")).toBeVisible();
  await expect(app.getByTestId("ark-key-empty")).toContainText("还没有");
  // Where to create one must be said, or the user gets stuck here.
  await expect(app.getByTestId("ark-fetch-console-link")).toBeVisible();

  // ⚠️ **Must not be mixed with AccessDenied**: that one must show which permission is missing,
  // and calling "no permission" "no keys" would make users create a new key (which would also be rejected).
  await expect(app.getByTestId("ark-fetch-error")).toHaveCount(0);

  // Switch to the account without permission; it looks completely different.
  await pickAccount(app, ACCOUNT_DENIED);
  await expect(app.getByTestId("ark-fetch-error")).toBeVisible();
  await expect(app.getByTestId("ark-fetch-error")).toContainText("ark:ListApiKeys");
  await expect(app.getByTestId("ark-key-empty")).toHaveCount(0);
});

test("态·失败：列账号失败能重试，且报错指向真实原因", async ({ app }) => {
  await setArkFetchStateMock(app, "error");
  await openPicker(app);

  const err = app.getByTestId("ark-account-error");
  await expect(err).toBeVisible();
  // The error must say "listing accounts failed", not "you have no accounts": the two ask the user
  // to do different things.
  await expect(err).toContainText("503");

  // Retryable (unlike 403, where retrying ten thousand times does nothing).
  await expect(app.getByTestId("ark-account-retry")).toBeVisible();

  /*
    ⚠️ These two lock in a check that **was once written wrong**: `fetchUsable` once included
    accountError, so when listing accounts failed it forced a fallback to manual input and the whole fetch
    branch folded away: **the error and the "retry" button disappeared together**, and users neither knew
    what happened nor could retry.

    The right behavior keeps both paths open: the error and retry are there (the two above), and the
    source dropdown stays usable, ready to switch to manual input at any time (the one below).
  */
  await expect(app.getByTestId("ark-source-select")).toBeEnabled();
  await expect(app.getByTestId("ark-source-select")).toContainText("从火山账号选");

  // Fixed after retry: first switch the mock back to ready, then click.
  await setArkFetchStateMock(app, "ready");
  await app.getByTestId("ark-account-retry").click();
  await expect(app.getByTestId("ark-account-combo")).toBeVisible();
  await expect(app.getByTestId("ark-account-error")).toHaveCount(0);
});

test("列 key 失败也能重试当前账号", async ({ app }) => {
  await openPicker(app);
  await pickAccount(app, ACCOUNT_DENIED);

  await expect(app.getByTestId("ark-fetch-error")).toBeVisible();
  await expect(app.getByTestId("ark-key-retry")).toBeVisible();
});

// ---------------------------------------------------------------------------
// 4. Plaintext never goes through the frontend (this matters most)
// ---------------------------------------------------------------------------

test("明文不经过前端：列表里只有掩码，且选中只交两个 id", async ({ app }) => {
  await openPicker(app);
  await pickAccount(app, ACCOUNT_OK);

  /*
    ⚠️ Ask the data layer directly, and assert **the response body has not a single plaintext field**.

    Stronger than only looking at the UI: not shown in the UI does not mean not in the response; once
    plaintext reaches frontend memory it is already in the devtools network panel and may be read by
    browser extensions. This assertion locks in that security premise itself.
  */
  const keys = await app.evaluate(async (acct) => {
    const list = await window.__amaApi?.listVolcArkKeys?.(acct);
    return list ?? [];
  }, ACCOUNT_OK);

  expect(keys.length).toBeGreaterThan(0);
  for (const k of keys) {
    const fields = Object.keys(k);
    // None of the possible names for a plaintext field may be present.
    expect(fields).not.toContain("apiKey");
    expect(fields).not.toContain("key");
    expect(fields).not.toContain("plaintext");
    expect(fields).not.toContain("secret");
    expect(fields).not.toContain("value");
    // There is a mask, and it is **only asterisks**: no real characters at all.
    expect(k.masked).toMatch(/^\*+$/);
  }

  // After selecting and saving, the UI also only has the mask.
  await pickKeyItem(app, KEY_OK);
  await expect(app.getByTestId("ark-fetch-picked")).toBeVisible();
  await app.getByTestId("modal-submit").click();
  await expect(app.getByTestId("ark-masked")).toContainText("*");
});

// ---------------------------------------------------------------------------
// 5. "Changing the key affects all referencing workspaces" must also be said in the picker
// ---------------------------------------------------------------------------

test("账号级：选中之后就地提示「所有引用它的空间会立即改用新 key」", async ({ app }) => {
  await app.goto("/preferences?tab=ark");
  await app.getByTestId("global-ark-setup").click();
  await pickAccount(app, ACCOUNT_OK);
  await pickKeyItem(app, KEY_OK);

  // ⚠️ By the time the user scrolls here, the same sentence in the dialog title is no longer visible,
  // while pressing save affects **a batch of workspaces**. So after selecting, state the impact again right here.
  const cascade = app.getByTestId("ark-fetch-cascade");
  await expect(cascade).toBeVisible();
  await expect(cascade).toContainText("所有引用");
  await expect(cascade).toContainText("立即");
});

test("空间级不提示联动：它只影响这一个空间，说了反而是假话", async ({ app }) => {
  await openPicker(app);
  await pickAccount(app, ACCOUNT_OK);
  await pickKeyItem(app, KEY_OK);

  await expect(app.getByTestId("ark-fetch-picked")).toBeVisible();
  // The impact is just the current workspace; it should not borrow the account-level sentence.
  await expect(app.getByTestId("ark-fetch-cascade")).toHaveCount(0);
});

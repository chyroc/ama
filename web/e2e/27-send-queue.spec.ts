/**
 * Send queue: when a send hits "the previous tool is still running" (409 + code=conflict), it is
 * queued and resent automatically.
 *
 * # Why this has to be simulated
 *
 * Under self_hosted there is a window of a few dozen milliseconds between Ark issuing
 * `agent.tool_use` and the worker returning `user.tool_result` (measured 48ms in production), during
 * which Ark rejects `user.message`. **It is normal, not a fault**: it keeps happening while the agent
 * calls tools in a row, a quick user hits it, and one resend fixes it.
 *
 * And this window **can never be reached naturally** in the mock (LocalApiClient has no Ark and no
 * requires_action state machine), and e2e runs in local mode where `page.route` intercepts nothing,
 * so the mock provides its own switch: `conflictSendsMock(n)` = the next n sends hit the window, then
 * succeed.
 *
 * # Every case must clear the switch
 *
 * The `conflictSends` marker lives in a **separate** localStorage key (otherwise `reset()` before
 * each case would wipe it), and the cost is that `reset()` cannot clear it either: workers=1 with
 * localStorage shared serially, so missing it once makes every later send case hit the window first,
 * with failure messages that do not point to this.
 */

import {
  clearConflictSendsMock,
  conflictSendsMock,
  seedPendingMessageMock,
  expect,
  failRedirectMock,
  failWritesMock,
  flushConversation,
  holdTurnMock,
  releaseTurnMock,
  restoreRedirectMock,
  restoreWritesMock,
  test,
} from "./fixtures";

test.afterEach(async ({ page }) => {
  await clearConflictSendsMock(page);
  await restoreWritesMock(page);
  await restoreRedirectMock(page);
  await releaseTurnMock(page);
});

test("只撞一次窗口时自动重发成功，用户看不到任何报错", async ({ app }) => {
  await app.goto("/w/growth/chat");

  // First send a normal sentence to create the session (queueing needs the "session already exists"
  // path, and creating a session does not go through sendMessage, so it never hits the window).
  await app.getByTestId("chat-input").fill("第一句正常发");
  await app.getByTestId("chat-send").click();
  await flushConversation(app);
  await expect(app.getByTestId("chat-agent-message")).toBeVisible();

  // Hit once: this is the most common shape in production (the window measured 48ms; one resend fixes it).
  await conflictSendsMock(app, 1);

  await app.getByTestId("chat-input").fill("这条会撞上工具窗口");
  await app.getByTestId("chat-send").click();

  /*
   * 1. **The automatic resend must really send the message.** This is the core of the requirement:
   *    hitting the window used to be a 500 "internal service error" and the user had to click again;
   *    now the frontend resends for them, and the message lands as usual.
   *
   * ⚠️ This case **deliberately does not assert the intermediate queued state**. With one hit,
   * enqueuing and the successful resend happen in the same React batch (enqueue pumps right away, and
   * this time the mock no longer conflicts), so the queue box **is never drawn at all**: that is the
   * ideal user experience (if the window closes fast it is completely unnoticed).
   * Asserting the queued state needs the window to stay open; see the next case.
   */
  await expect(
    app.getByTestId("chat-user-message").filter({ hasText: "这条会撞上工具窗口" }),
  ).toHaveCount(1);

  /*
   * 2. **It must not become an error.** Hitting the window is not a fault; the red text (with its
   *    one-click feedback entry and level-one reporting) should not appear at all. This is asserted
   *    after the message has landed: if the implementation took the old path, the red text would
   *    already be there by then.
   */
  await expect(app.getByTestId("chat-error")).toHaveCount(0);
  // 3. The queue should also be clean, with no empty box or "0 queued" line left behind
  await expect(app.getByTestId("chat-queue")).toHaveCount(0);
});

test("工具还在执行时消息停在队列里，看得见排第几；跑完自动发出", async ({ app }) => {
  await app.goto("/w/growth/chat");
  await app.getByTestId("chat-input").fill("第一句正常发");
  await app.getByTestId("chat-send").click();
  await flushConversation(app);
  await expect(app.getByTestId("chat-agent-message")).toBeVisible();

  /*
   * Hold this turn, simulating "the agent is still answering / a tool really is still running". The
   * queued state is therefore stable and assertable.
   *
   * ⚠️ **conflictSendsMock can no longer be used to create a queue.** That switch simulates a 409
   * window hit, and a 409 now goes through **silent local resend** (sendThroughWindow in useSendQueue),
   * never entering the queue UI: it is a race of a few dozen milliseconds that users should not see as
   * "queued". The queue only serves "the user sends several in a row and has to wait for a whole turn".
   * With the two paths separated, holdTurn is the only way to create a queue.
   */
  await holdTurnMock(app);

  /*
   * First send one sentence to **really start this turn** (holdTurn holds "the replay that is
   * running"; without a replay there is nothing to hold). This one stays at "still answering", and
   * only the next one can enter the queue.
   */
  await app.getByTestId("chat-input").fill("这句会一直在回答");
  await app.getByTestId("chat-send").click();
  await expect(app.getByTestId("chat-interrupt")).toBeVisible();

  await app.getByTestId("chat-input").fill("这条要等 agent 答完");
  await app.getByTestId("chat-send").click();

  // 1. Queued messages are visible, and you can tell which one it is
  await expect(app.getByTestId("chat-queue-item")).toHaveCount(1);
  await expect(app.getByTestId("chat-queue-text")).toContainText("这条要等 agent 答完");
  // 2. The queue position must be shown: with several stacked up, users know where they are
  await expect(app.getByTestId("chat-queue-position")).toHaveText("第 1/1 条");
  // 3. It must not become an error
  await expect(app.getByTestId("chat-error")).toHaveCount(0);
  // 4. The wording is not "failed": that window is only a few dozen milliseconds, and calling it a
  // failure would make users retry by hand
  const hint = app.getByTestId("chat-queue-hint");
  await expect(hint).toContainText("排队");
  await expect(hint).not.toContainText("失败");

  /*
   * 5. **After the window closes it must send by itself, leave the queue, and render as a normal sent
   *    message.**
   *
   * This is the line between "queued" and "stuck": without it, an implementation that "never sends
   * after enqueuing" would still turn all the assertions above green.
   */
  await releaseTurnMock(app);
  await flushConversation(app);
  await expect(app.getByTestId("chat-queue-item")).toHaveCount(0, { timeout: 15_000 });
  await expect(
    app.getByTestId("chat-user-message").filter({ hasText: "这条要等 agent 答完" }),
  ).toHaveCount(1);
  // After the queue is empty the note should go away too, not leave a "0 messages queued" line
  await expect(app.getByTestId("chat-queue")).toHaveCount(0);
});

test("排队中的消息可以单独取消，取消即丢弃", async ({ app }) => {
  await app.goto("/w/growth/chat");
  await app.getByTestId("chat-input").fill("先建会话");
  await app.getByTestId("chat-send").click();
  await flushConversation(app);
  await expect(app.getByTestId("chat-agent-message")).toBeVisible();

  // Hold this turn: cancel must be clicked while it is "still queued", so it cannot be allowed to send by itself.
  // (conflictSendsMock cannot be used: a 409 now goes through silent local resend and does not enter
  // the queue; see this file's header.)
  await holdTurnMock(app);
  await app.getByTestId("chat-input").fill("这句会一直在回答");
  await app.getByTestId("chat-send").click();
  await expect(app.getByTestId("chat-interrupt")).toBeVisible();

  await app.getByTestId("chat-input").fill("这条要被取消掉");
  await app.getByTestId("chat-send").click();
  await expect(app.getByTestId("chat-queue-item")).toHaveCount(1);

  await app.getByTestId("chat-queue-cancel").click();

  // 1. The queue is empty
  await expect(app.getByTestId("chat-queue-item")).toHaveCount(0);
  /*
   * 2. **Cancel = discard, not "send later".**
   *
   * Turn off the window switch and wait long enough (backoff starts at 400ms, capped at 5s):
   * if the implementation sends the cancelled one anyway, it shows up here.
   * This assertion guards against the implementation where "cancel only removes it from the list while
   * the timer keeps running": that mistake has zero errors and only shows up as "a cancelled message
   * sent itself a while later".
   */
  await releaseTurnMock(app);
  await flushConversation(app);
  await app.waitForTimeout(1500);
  await expect(
    app.getByTestId("chat-user-message").filter({ hasText: "这条要被取消掉" }),
  ).toHaveCount(0);
  // The input box should not get the original text back either: cancel discards; filling it back is
  // the job of the "edit" action
  await expect(app.getByTestId("chat-input")).toHaveValue("");
});

test("点「修改」把排队消息放回输入框，同时从队列里移除", async ({ app }) => {
  await app.goto("/w/growth/chat");
  await app.getByTestId("chat-input").fill("先建会话");
  await app.getByTestId("chat-send").click();
  await flushConversation(app);
  await expect(app.getByTestId("chat-agent-message")).toBeVisible();

  // Hold this turn to create a queue (409 now goes through silent local resend and does not enter the
  // queue; see this file's header).
  await holdTurnMock(app);
  await app.getByTestId("chat-input").fill("这句会一直在回答");
  await app.getByTestId("chat-send").click();
  await expect(app.getByTestId("chat-interrupt")).toBeVisible();

  await app.getByTestId("chat-input").fill("这条要改一改再发");
  await app.getByTestId("chat-send").click();
  await expect(app.getByTestId("chat-queue-item")).toHaveCount(1);
  // After sending, the input box is cleared first; that is the precondition for the fill-back
  // assertion (otherwise it might read an old value that was never cleared)
  await expect(app.getByTestId("chat-input")).toHaveValue("");

  await app.getByTestId("chat-queue-edit").click();

  // 1. The text goes back to the input box and can be edited further
  await expect(app.getByTestId("chat-input")).toHaveValue("这条要改一改再发");
  // 2. **It is also removed from the queue**: otherwise editing and sending again would make two messages
  await expect(app.getByTestId("chat-queue-item")).toHaveCount(0);

  /*
   * 3. Same reasoning as cancel: after edit takes it away, the original should not be sent anymore.
   *    Otherwise the user would see both "the version before editing" and "the version after" sent.
   */
  await releaseTurnMock(app);
  await flushConversation(app);
  await app.waitForTimeout(1500);
  await expect(
    app.getByTestId("chat-user-message").filter({ hasText: "这条要改一改再发" }),
  ).toHaveCount(0);
});

test("队列满 20 条时第 21 条被拒，给明确提示且原文不丢", async ({ app }) => {
  await app.goto("/w/growth/chat");
  await app.getByTestId("chat-input").fill("先建会话");
  await app.getByTestId("chat-send").click();
  await flushConversation(app);
  await expect(app.getByTestId("chat-agent-message")).toBeVisible();

  // Hold this turn to create a queue (409 now goes through silent local resend and does not enter the
  // queue; see this file's header).
  await holdTurnMock(app);
  await app.getByTestId("chat-input").fill("这句会一直在回答");
  await app.getByTestId("chat-send").click();
  await expect(app.getByTestId("chat-interrupt")).toBeVisible();

  // Fill up 20: this turn is held, so each one takes the "streaming -> end of queue" path
  // (even with a non-empty queue it may not jump ahead and send directly; see the third point in the
  // header of lib/useSendQueue.ts).
  for (let i = 1; i <= 20; i++) {
    await app.getByTestId("chat-input").fill(`排队消息 ${i}`);
    await app.getByTestId("chat-send").click();
    // Wait one by one: without waiting, a click can run before the previous setState, so fewer than 20
    // actually get queued, and the failure message points in the wrong direction, "the limit check is wrong".
    await expect(app.getByTestId("chat-queue-item")).toHaveCount(i);
  }

  // Once full, send the 21st
  await app.getByTestId("chat-input").fill("这是第 21 条");
  await app.getByTestId("chat-send").click();

  /*
   * 1. **There must be a clear message.** Silent dropping is the worst failure of this feature: the
   *    user thinks it was sent, then waits for a reply that will never come.
   */
  const notice = app.getByTestId("chat-queue-notice");
  await expect(notice).toBeVisible();
  await expect(notice).toContainText("20");

  // 2. The queue did **not** grow to 21: the limit really blocks
  await expect(app.getByTestId("chat-queue-item")).toHaveCount(20);

  // 3. The text goes back to the input box; what the user typed must not vanish because we did not accept it
  await expect(app.getByTestId("chat-input")).toHaveValue("这是第 21 条");

  // 4. The 21st really was not sent
  await expect(
    app.getByTestId("chat-user-message").filter({ hasText: "这是第 21 条" }),
  ).toHaveCount(0);
});

test("排队消息按先进先出发出，位置随之更新", async ({ app }) => {
  await app.goto("/w/growth/chat");
  await app.getByTestId("chat-input").fill("先建会话");
  await app.getByTestId("chat-send").click();
  await flushConversation(app);
  await expect(app.getByTestId("chat-agent-message")).toBeVisible();

  // Hold this turn to create a queue (409 now goes through silent local resend and does not enter the
  // queue; see this file's header).
  await holdTurnMock(app);
  await app.getByTestId("chat-input").fill("这句会一直在回答");
  await app.getByTestId("chat-send").click();
  await expect(app.getByTestId("chat-interrupt")).toBeVisible();

  for (const text of ["排队第一条", "排队第二条", "排队第三条"]) {
    await app.getByTestId("chat-input").fill(text);
    await app.getByTestId("chat-send").click();
    await expect(app.getByTestId("chat-input")).toHaveValue("");
  }
  await expect(app.getByTestId("chat-queue-item")).toHaveCount(3);

  /*
   * 1. Order and position: **the one sent first comes first**, and each shows its own position in the queue.
   *
   * Asserting "there are 3 boxes" is not enough: an implementation using the queue as a stack (last in,
   * first out) still shows 3 boxes, and it would send the user's three sentences in reverse order.
   */
  await expect(app.getByTestId("chat-queue-text")).toHaveText([
    "排队第一条",
    "排队第二条",
    "排队第三条",
  ]);
  await expect(app.getByTestId("chat-queue-position")).toHaveText([
    "第 1/3 条",
    "第 2/3 条",
    "第 3/3 条",
  ]);

  // 2. After this turn ends the whole queue is sent in order, and everything leaves the queue
  await releaseTurnMock(app);
  await flushConversation(app);
  await expect(app.getByTestId("chat-queue-item")).toHaveCount(0, { timeout: 15_000 });
  for (const text of ["排队第一条", "排队第二条", "排队第三条"]) {
    await expect(app.getByTestId("chat-user-message").filter({ hasText: text })).toHaveCount(1);
  }
});

test("判据是错误码不是状态码：500 + code=conflict 走无感重发而不是报错", async ({ app }) => {
  /*
   * This locks in the contract "decide by code, not by HTTP 409".
   *
   * `failWritesMock("conflict")` creates an error with **code=conflict but status 500** (guardWrite
   * only maps access_denied to 403, everything else is 500). That shape cannot appear on the real
   * backend, but it is the **only** way to tell the two implementations apart:
   *
   *   - decide by code (correct) -> goes to (A) silent local resend; the user sees no red text
   *   - decide by status === 409 -> falls into the "real fault" branch, red text
   *
   * Without this case, an implementation writing isSendConflict as `e.status === 409` would be all
   * green, and the real consequence of that is the reverse: other 409 meanings in the future (such as
   * "skill name already exists") would get pulled into automatic resend, and the user would wait three
   * minutes for a message that can never be sent.
   *
   * ⚠️ **What is asserted is "no red text + eventually sent", not "enters the queue".** Hitting the
   * window is a race of a few dozen milliseconds, while the queue is "wait for a whole turn"; with the
   * two layered apart, a 409 no longer enters the queue UI (see (A)/(B) in this file's header).
   */
  await app.goto("/w/growth/chat");
  await app.getByTestId("chat-input").fill("先建会话");
  await app.getByTestId("chat-send").click();
  await flushConversation(app);
  await expect(app.getByTestId("chat-agent-message")).toBeVisible();

  await failWritesMock(app, "conflict");

  await app.getByTestId("chat-input").fill("状态码不是 409 也该无感重发");
  await app.getByTestId("chat-send").click();

  // 1. It must not become red text: hitting the window is not a fault.
  await expect(app.getByTestId("chat-error")).toHaveCount(0);
  // 2. It should not enter the queue UI either: that spot is only for "wait for a whole turn".
  await expect(app.getByTestId("chat-queue-item")).toHaveCount(0);

  /*
   * 3. **After the switch is removed it must really send it.**
   *
   * This cannot be skipped: asserting only the first two, an implementation that "silently drops the
   * message on a hit" would still be all green: no red text, no queue, and the user's words just vanish.
   */
  await restoreWritesMock(app);
  await expect(
    app.getByTestId("chat-user-message").filter({ hasText: "状态码不是 409 也该无感重发" }),
  ).toHaveCount(1, { timeout: 15_000 });
  await expect(app.getByTestId("chat-error")).toHaveCount(0);
});

test("真故障不排队：500 立刻报错，不让用户干等", async ({ app }) => {
  /*
   * The flip side of queueing must be locked in too. **Only that window conflict is queued**; other
   * errors must be shown to the user right away. An implementation that "queues and retries every
   * failure" would turn all the earlier cases green, and its consequence is: real faults (invalid
   * credentials, service down) are hidden, the user waits three minutes for a message doomed never to
   * be sent, and during those three minutes the frontend keeps hammering the backend.
   */
  await app.goto("/w/growth/chat");
  await app.getByTestId("chat-input").fill("先建会话");
  await app.getByTestId("chat-send").click();
  await flushConversation(app);
  await expect(app.getByTestId("chat-agent-message")).toBeVisible();

  await failWritesMock(app);

  await app.getByTestId("chat-input").fill("这条会真的失败");
  await app.getByTestId("chat-send").click();

  // 1. Red text appears right away (with its one-click feedback entry)
  await expect(app.getByTestId("chat-error")).toBeVisible();
  // 2. **Not** queued
  await expect(app.getByTestId("chat-queue-item")).toHaveCount(0);
});

test("agent 还在回答时照样能连着发，后续消息排队而不是发不出去", async ({ app }) => {
  /*
   * **This is the real main path of the feature**; the 409 cases before it are fallback paths.
   *
   * Users really use it as "say a few things in a row": the first is sent, the agent starts answering,
   * and then they think of a second one and keep typing. This path used to be **completely blocked**:
   * `ChatComposer`'s canSend included `!streaming`, and the "Send" button was replaced entirely by
   * "Interrupt" while streaming, so:
   *
   *   the UI looks like you can type -> you finish and cannot send -> no explanation at all
   *
   * And at the time queueing only hung off the "sent and hit 409" path, which this could never reach.
   * Zero errors.
   *
   * # Why holdTurnMock
   *
   * The precondition this case checks is `streaming` being true, and in e2e it can neither be waited
   * for nor created: the mock replay finishes in 1.4 seconds (by the second click on send it has often
   * already ended, so the message is sent directly and the queue is empty), and the event tails of all
   * six seed runs are `status_idle`. See the notes in fixtures.
   */
  await holdTurnMock(app);

  await app.goto("/w/growth/chat");
  await app.getByTestId("chat-input").fill("第一句：帮我查一下");
  await app.getByTestId("chat-send").click();

  // This turn stays stably at "still answering": the interrupt button being present is the test
  await expect(app.getByTestId("chat-interrupt")).toBeVisible();

  /*
   * 1. **The send button must still be present.** This is the positive assertion for that bug:
   *    "Interrupt" and "Send" mean two non-conflicting things (stop this turn / say one more thing);
   *    they used to be either-or, so "Send" disappeared from the DOM entirely while streaming.
   */
  await expect(app.getByTestId("chat-send")).toBeVisible();

  // 2. After typing content it is **really clickable** (empty content being unclickable is an existing
  // rule, unrelated to this bug)
  await app.getByTestId("chat-input").fill("第二句：顺便看下上周");
  await expect(app.getByTestId("chat-send")).toBeEnabled();
  await app.getByTestId("chat-send").click();
  await expect(app.getByTestId("chat-queue-item")).toHaveCount(1);

  // 3. Send another one right after; both get queued
  await app.getByTestId("chat-input").fill("第三句：再对比上月");
  await app.getByTestId("chat-send").click();
  await expect(app.getByTestId("chat-queue-item")).toHaveCount(2);

  // 4. Queued above the input box, showing the position; the wording is "still answering", not "running a tool"
  //    (most queueing has that cause, and saying "tool" would make users look for a tool that does not exist)
  await expect(app.getByTestId("chat-queue-position")).toHaveText([
    "第 1/2 条",
    "第 2/2 条",
  ]);
  await expect(app.getByTestId("chat-queue-hint")).toContainText("还在回答");

  // 5. Queued != sent: these two should not appear in the message stream yet
  await expect(
    app.getByTestId("chat-user-message").filter({ hasText: "第二句" }),
  ).toHaveCount(0);

  /*
   * 6. **After this turn ends they must be sent automatically.** The queue is completely still during
   *    streaming (no timers, no polling), relying on an effect in ChatPage to nudge it when streaming
   *    drops. Without that effect the messages would stay in the queue forever, also with zero errors.
   */
  await releaseTurnMock(app);
  await flushConversation(app);
  await expect(app.getByTestId("chat-queue-item")).toHaveCount(0, { timeout: 20_000 });

  for (const text of ["第二句：顺便看下上周", "第三句：再对比上月"]) {
    await expect(app.getByTestId("chat-user-message").filter({ hasText: text })).toHaveCount(1);
  }
});

test("排队中点「立即发送」= 转向：打断当前这一轮，这条立刻发出", async ({ app }) => {
  /*
   * "Send now" and queueing are **opposite** things: queueing waits for the previous turn to finish;
   * redirecting does not wait and interrupts directly.
   *
   * So the core of this case is "the current turn really was interrupted". Checking only "the message
   * was sent" is not enough: an implementation that "moves this one to the head of the queue but still
   * waits for the previous turn to end" would still get the message into the stream eventually, and
   * that is completely different from what the user wants by clicking this button (they want to change
   * the topic **now**).
   *
   * The test uses holdTurnMock: that turn is held and will not end by itself, so if the message shows
   * up in the stream, it can only have been sent by redirecting.
   */
  await holdTurnMock(app);

  await app.goto("/w/growth/chat");
  await app.getByTestId("chat-input").fill("第一句：帮我查一下");
  await app.getByTestId("chat-send").click();
  // This turn stays stably at "still answering": the interrupt button being present is the test
  await expect(app.getByTestId("chat-interrupt")).toBeVisible();

  await app.getByTestId("chat-input").fill("不查了，改看 CI");
  await app.getByTestId("chat-send").click();
  await expect(app.getByTestId("chat-queue-item")).toHaveCount(1);

  // 1. Every queued item has "Send now"
  await expect(app.getByTestId("chat-queue-send-now")).toHaveCount(1);

  await app.getByTestId("chat-queue-send-now").click();

  /*
   * 2. **This turn is still held by holdTurn, yet the message was sent**: only possible via redirect
   *    (interrupt first, then send). This is the core assertion of this case.
   */
  await expect(
    app.getByTestId("chat-user-message").filter({ hasText: "不查了，改看 CI" }),
  ).toHaveCount(1, { timeout: 15_000 });

  // 3. It leaves the queue. Otherwise it would **be sent again** when this turn ends, and the user
  // would see two identical messages.
  await expect(app.getByTestId("chat-queue-item")).toHaveCount(0);

  // 4. Redirecting is not a fault; there should be no red text
  await expect(app.getByTestId("chat-error")).toHaveCount(0);

  await releaseTurnMock(app);
});

test("多条排队时每条都有「立即发送」，点第二条只发第二条", async ({ app }) => {
  /*
   * **Every item** must have the button, not just the head of the queue: the one the user wants to
   * jump in may well be the 3rd they just typed (the first two no longer matter). If only the head had
   * it, they would have to cancel the first two to jump the queue, losing those two texts.
   *
   * The half "clicking the second sends only the second" must be locked in too: an implementation that
   * passes the wrong id and always sends the head (say, using items[0] in onSendNow) is **completely
   * invisible** when only one is queued.
   */
  await holdTurnMock(app);

  await app.goto("/w/growth/chat");
  await app.getByTestId("chat-input").fill("第一句：帮我查一下");
  await app.getByTestId("chat-send").click();
  await expect(app.getByTestId("chat-interrupt")).toBeVisible();

  for (const text of ["排队第一条", "排队第二条", "排队第三条"]) {
    await app.getByTestId("chat-input").fill(text);
    await app.getByTestId("chat-send").click();
    await expect(app.getByTestId("chat-input")).toHaveValue("");
  }
  await expect(app.getByTestId("chat-queue-item")).toHaveCount(3);

  // 1. **Each** of the three has a button
  await expect(app.getByTestId("chat-queue-send-now")).toHaveCount(3);

  // 2. Click the second one
  await app.getByTestId("chat-queue-send-now").nth(1).click();

  // 3. The second one is what got sent
  await expect(
    app.getByTestId("chat-user-message").filter({ hasText: "排队第二条" }),
  ).toHaveCount(1, { timeout: 15_000 });

  /*
   * 4. **The other two are still in the queue**, and the first was not sent along the way.
   *    This half is the real test of "click which, send which".
   */
  await expect(app.getByTestId("chat-queue-text")).toHaveText([
    "排队第一条",
    "排队第三条",
  ]);
  await expect(
    app.getByTestId("chat-user-message").filter({ hasText: "排队第一条" }),
  ).toHaveCount(0);

  await releaseTurnMock(app);
});

test("转向失败时原文回到输入框，且输入区不会卡在「中断」态", async ({ app }) => {
  /*
   * Locks in two things very easy to miss on the failure path:
   *
   * One, **the text must not be lost.** The user's typing vanishing because we failed to send it is
   *     the worst way to fail.
   * Two, **the optimistic streaming flag must be reset by hand.** markStreaming switches the input area
   *     to the "Interrupt" state right away, while this turn never started: the event that would clear
   *     it does not exist precisely because of the failure. Without a reset, the "Interrupt" button
   *     **stays there forever**, and streaming stuck at true freezes the queue from then on (canSendNow
   *     is always false), so nothing queued afterwards can be sent.
   *     That is exactly the known trap "an optimistic flag must be reset by hand on the failure path".
   *
   * # Why seedPendingMessageMock instead of holdTurnMock to create the queue
   *
   * The test for point two is "the interrupt button should not remain", and on the holdTurn path **this
   * turn really is running**, so the interrupt button should be there regardless, and whether it was
   * reset cannot be seen (verified: deleting clearStreaming entirely keeps the case green). So what is
   * needed here is "one item in the queue, but this turn **not running**": then streaming is really
   * **false**, the interrupt button can only come from the optimistic flag, and a missed reset is
   * obvious at a glance.
   *
   * ⚠️ This state used to be created with conflictSendsMock (a 409 kept the message stuck in the
   * queue). After the queue moved to the server, a 409 no longer enters the queue (it goes through
   * silent local resend; see (A)/(B) in this file's header), so now one item is put straight into the
   * server queue.
   */
  await app.goto("/w/growth/chat");
  await app.getByTestId("chat-input").fill("先建会话");
  await app.getByTestId("chat-send").click();
  await flushConversation(app);
  await expect(app.getByTestId("chat-agent-message")).toBeVisible();

  /*
   * Put one item straight into the server queue. At this point this turn **has already ended**, so the
   * "Interrupt" button is not present: that is the precondition for the assertion below.
   *
   * runId is taken from the URL: the earlier send already created the session and replaced the address.
   */
  const runId = new URL(app.url()).pathname.split("/").pop() ?? "";
  expect(runId).not.toBe("");
  await seedPendingMessageMock(app, runId, "这条转向会失败");
  // After putting it in, make the page fetch the queue again (it was put in on the server; the frontend does not know yet).
  await app.reload();
  await expect(app.getByTestId("chat-queue-item")).toHaveCount(1);
  await expect(app.getByTestId("chat-interrupt")).toHaveCount(0);

  // Only make **redirect** fail, other writes work as usual: failWritesMock would also break the queue's
  // own resend, the queue box would disappear at the moment of the click (detached), and the
  // assertions would pass for the wrong reason. See the notes on failRedirectMock in fixtures.
  await failRedirectMock(app);
  await app.getByTestId("chat-queue-send-now").click();

  // 1. The error must be shown to the user: a redirect failure is a real fault, unlike "hitting the
  // window", and must not be silent
  await expect(app.getByTestId("chat-error")).toBeVisible();

  // 2. The text goes back to the input box
  await expect(app.getByTestId("chat-input")).toHaveValue("这条转向会失败");

  /*
   * 3. **Not stuck in the "Interrupt" state.** markStreaming is optimistic, and this turn never
   *    started, so no event will come to clear it; without a manual reset the button would stay here forever.
   */
  await expect(app.getByTestId("chat-interrupt")).toHaveCount(0);

  /*
   * 4. **The send channel is not frozen.**
   *
   * This is the downstream consequence of the trap in point two: with streaming stuck at true, the
   * input area stays in the "Interrupt" state forever, and the user can never send anything again,
   * with zero errors.
   *
   * ⚠️ **The test is "the text in the input box can really be sent", not "it is still in the queue".**
   * This used to assert that `chat-queue-item` still had 1 item, which was **wrong**, and subtly so:
   * `queue.take()` in `sendNow` first calls `cancelPendingMessage`, which **really deletes** it on the
   * server, and then redirects; when the redirect fails the text is handed back to the input box
   * (point 2 above), and **nobody re-queues it**. So after a failure the server queue is always empty:
   * verified, `listPendingMessages` reliably returns 0, and the DOM shows 0 after reload too.
   *
   * That assertion passed ~80% of the time thanks to a **phantom**: if a refresh started before take and
   * landed after take, it wrote the old "before cancel" snapshot back into state, leaving a queue box
   * in the DOM that the server no longer had (`dom=1` while `server=0`; forcing this interleaving
   * reproduces it 100%). In other words it asserted the product of a bug; that bug has been fixed in
   * useSendQueue's mutationSeq, so the assertion was bound to go red, and **fixing it was the right thing**.
   *
   * The current test does not depend on how many are left in the queue: once the text is back in the
   * input box and can be sent normally, that proves streaming was reset and the send channel is alive.
   * The queue is empty at this point, so sending directly has no "jumping ahead of the queue head"
   * problem (that rule is the third point in the header of lib/useSendQueue.ts).
   */
  await restoreRedirectMock(app);
  await expect(app.getByTestId("chat-queue-item")).toHaveCount(0);
  await app.getByTestId("chat-send").click();
  await expect(
    app.getByTestId("chat-user-message").filter({ hasText: "这条转向会失败" }),
  ).toHaveCount(1, { timeout: 15_000 });
  // After sending the input box must be cleared; otherwise the user would think it failed and send again.
  await expect(app.getByTestId("chat-input")).toHaveValue("");
});

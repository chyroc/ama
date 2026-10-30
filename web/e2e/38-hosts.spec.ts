/**
 * UI cases for the host executor.
 *
 * The mock has no daemon registration API (that is what the CLI uses), so hosts are put directly
 * into the mock store (same technique as building events in 09-chat.spec.ts: the private store +
 * commit). Binding/unbinding goes through public ApiClient methods or the UI, because those two are
 * exactly the main paths to check.
 *
 * Covers:
 *   1. the two-step onboarding guide in the global settings empty state (API Key link)
 *   2. inline info for registered machines (online dot / native protocol capability warning) and delete
 *   3. workspace settings: bind / dropdown candidate filtering / unbind
 *   4. Agent form: the two executor cards, the workspace-assigned/personal-direct source groups,
 *      guidance when there are no candidates, offline warning, fields shown again after saving
 */

import { expect, test } from "./fixtures";
import type { Page } from "@playwright/test";

const WS_ID = "ws_demo";

interface SeedHost {
  id: string;
  name: string;
  os: string;
  arch: string;
  status: string;
  online: boolean;
  gnuToolsOk: boolean;
  nativeToolsOk: boolean;
  createdAt: string;
  lastSeenAt: string;
  daemonVersion?: string;
}

function host(over: Partial<SeedHost> & { id: string; name: string }): SeedHost {
  const now = new Date().toISOString();
  return {
    os: "darwin",
    arch: "arm64",
    status: "online",
    online: true,
    gnuToolsOk: true,
    nativeToolsOk: true,
    createdAt: now,
    lastSeenAt: now,
    daemonVersion: "0.1.0",
    ...over,
  };
}

async function seedHosts(page: Page, hosts: SeedHost[]): Promise<void> {
  await page.evaluate((rows) => {
    const client = window.__amaApi as unknown as {
      store: { hosts: SeedHost[] };
      commit: () => void;
    };
    client.store.hosts.push(...rows);
    client.commit();
  }, hosts);
}

async function bindViaApi(page: Page, hostId: string): Promise<void> {
  await page.evaluate(
    ([wsId, id]) => {
      const client = window.__amaApi as unknown as {
        bindWorkspaceHost: (ws: string, h: string) => Promise<void>;
      };
      return client.bindWorkspaceHost(wsId, id);
    },
    [WS_ID, hostId],
  );
}

test("全局设置空态给出两步接入引导", async ({ app }) => {
  await app.goto("/preferences?tab=hosts");

  await expect(app.getByTestId("hosts-empty")).toBeVisible();
  // Both step commands are there, each with a copyable code block.
  await expect(app.getByText('git clone https://github.com/chyroc/ama && cd ama/server && go build -o "$(go env GOPATH)/bin/ama" ./cmd/ama-cli', { exact: true })).toBeVisible();
  await expect(app.getByText("AMA_TOKEN=amak_xxx ama daemon install", { exact: true })).toBeVisible();
  // Step 2 points to the API Key tab on the same page (with from, so you can come back).
  await expect(app.getByTestId("hosts-apikeys-link")).toHaveAttribute(
    "href",
    /tab=apikeys/,
  );
});

test("已注册机器显示在线状态，不支持原生协议给升级提示，删除后消失", async ({ app }) => {
  await seedHosts(app, [
    host({
      id: "hst_mac1",
      name: "我的 MacBook",
      // No native tool protocol support: show the hint to upgrade the ama CLI (the new protocol does not need brew).
      nativeToolsOk: false,
    }),
  ]);

  await app.goto("/preferences?tab=hosts");

  const row = app.getByTestId("host-row-hst_mac1");
  await expect(row).toBeVisible();
  await expect(row).toContainText("我的 MacBook");
  await expect(row).toContainText("darwin/arm64");
  await expect(row).toContainText("在线");
  await expect(app.getByTestId("host-native-bad-hst_mac1")).toBeVisible();
  await expect(app.getByTestId("host-native-bad-detail-hst_mac1")).toContainText(
    "升级新版 ama CLI",
  );
  // The hint must say clearly that the new version does not need brew; do not point people down the
  // old path of installing GNU tools.
  await expect(app.getByTestId("host-native-bad-detail-hst_mac1")).toContainText(
    "不需要 brew install",
  );

  // Delete goes through the dangerous-action confirmation dialog, then back to the empty-state guide.
  await app.getByTestId("host-delete-hst_mac1").click();
  await app.getByTestId("confirm-danger").click();
  await expect(app.getByTestId("host-row-hst_mac1")).toHaveCount(0);
  await expect(app.getByTestId("hosts-empty")).toBeVisible();
});

test("空间设置：绑定一台机器后出现在列表，再解绑", async ({ app }) => {
  await seedHosts(app, [
    host({ id: "hst_bind1", name: "台式机-Linux", os: "linux", arch: "amd64" }),
  ]);
  await app.goto("/w/growth/settings?tab=hosts");

  // None bound: empty state + add area visible, without showing "you have not registered any host".
  await expect(app.getByText("这个空间还没有绑定宿主机")).toBeVisible();
  await expect(app.getByTestId("ws-hosts-none")).toHaveCount(0);

  // Pick a candidate + bind.
  await app.getByTestId("ws-host-trigger").click();
  await app.getByRole("option", { name: /台式机-Linux/ }).click();
  await app.getByTestId("ws-host-bind").click();

  const boundRow = app.getByTestId("ws-host-hst_bind1");
  await expect(boundRow).toBeVisible();
  await expect(boundRow).toContainText("linux/amd64");

  // The row must show "who registered it, who bound it" (the mock is single-user, so both are the current user).
  await expect(app.getByTestId("ws-host-owner-hst_bind1")).toContainText("注册者");
  await expect(app.getByTestId("ws-host-binder-hst_bind1")).toContainText("绑定者");

  // Once the only candidate is bound, the dropdown folds away completely (showing "all bound"),
  // instead of leaving an empty dropdown.
  await expect(app.getByTestId("ws-hosts-all-bound")).toBeVisible();
  await expect(app.getByTestId("ws-host-select")).toHaveCount(0);

  // Unbind (only deletes the binding, not the machine: it is still in global settings).
  await boundRow.getByRole("button", { name: "解绑" }).click();
  await app.getByTestId("confirm-danger").click();
  await expect(app.getByTestId("ws-host-hst_bind1")).toHaveCount(0);

  // The machine still belongs to this user: the candidate shows up again.
  await expect(app.getByTestId("ws-hosts-all-bound")).toHaveCount(0);
  await app.getByTestId("ws-host-trigger").click();
  await expect(app.getByRole("option", { name: /台式机-Linux/ })).toBeVisible();
});

test("没有任何主机时引导去全局设置注册", async ({ app }) => {
  await app.goto("/w/growth/settings?tab=hosts");
  await expect(app.getByTestId("ws-hosts-none")).toBeVisible();
  await expect(app.getByTestId("ws-host-select")).toHaveCount(0);
});

test("Agent 表单：默认云容器；选宿主机时展开机器选择并随保存回显", async ({ app }) => {
  await seedHosts(app, [
    host({ id: "hst_pick1", name: "我的办公机", os: "linux", arch: "amd64", online: false, status: "offline" }),
  ]);
  await bindViaApi(app, "hst_pick1");

  await app.goto("/w/growth/agents/new");

  // Default cloud container: the machine picker does not appear.
  await expect(app.getByTestId("agent-executor-host-picker")).toHaveCount(0);

  // Switch to host: the picker appears; offline machines can be picked but get a yellow warning.
  await app.getByTestId("executor-host").click();
  await app.getByTestId("agent-executor-host-trigger").click();
  await app.getByRole("option", { name: /我的办公机/ }).click();
  await expect(app.getByTestId("agent-executor-host-offline")).toContainText("离线");

  // Fill in required fields and save.
  await app.getByTestId("agent-name").fill("宿主机执行助手");
  await app.getByTestId("agent-system").fill("在我的办公机上执行任务。");
  await app.getByTestId("agent-submit").click();
  await app.waitForURL(/\/agents\/agent_/);

  // Shown again: the host card on the config page is still selected, and the collapsed dropdown shows
  // the same machine.
  await expect(app.getByTestId("executor-host")).toHaveAttribute("data-selected", "true");
  await expect(app.getByTestId("agent-executor-host-trigger")).toContainText("我的办公机");
  await expect(app.getByTestId("agent-executor-host-offline")).toContainText("离线");

  // Switch back to cloud container: the picker disappears and hostId is cleared (no preselection when
  // switching back to host).
  await app.getByTestId("executor-docker").click();
  await expect(app.getByTestId("agent-executor-host-picker")).toHaveCount(0);
  await app.getByTestId("executor-host").click();
  await expect(app.getByTestId("agent-executor-host-trigger")).not.toContainText("我的办公机");
});

test("Agent 表单：空间一台主机都没绑时给出空间设置引导", async ({ app }) => {
  await app.goto("/w/growth/agents/new");
  await app.getByTestId("executor-host").click();
  // The default source is workspace-assigned; the empty state should be visible right away.
  await expect(app.getByTestId("executor-source-workspace")).toHaveAttribute(
    "data-selected",
    "true",
  );
  await expect(app.getByTestId("agent-executor-no-hosts")).toBeVisible();
  await expect(app.getByTestId("agent-executor-no-hosts")).toContainText("去空间设置绑定宿主机");
});

test("Agent 表单：个人直连可以选一台没绑进空间的机器，保存并回显", async ({ app }) => {
  // Register a machine but do **not** bind it to the workspace: the workspace-assigned group cannot see
  // it, only the personal group can pick it.
  await seedHosts(app, [
    host({ id: "hst_personal1", name: "我的私人 Mac", os: "darwin", arch: "arm64" }),
  ]);

  await app.goto("/w/growth/agents/new");
  await app.getByTestId("executor-host").click();

  // The workspace group is empty (never bound); switch to personal direct: the personal group's empty
  // state disappears and the machine appears.
  await expect(app.getByTestId("agent-executor-no-hosts")).toBeVisible();
  await app.getByTestId("executor-source-personal").click();
  await expect(app.getByTestId("agent-executor-personal-no-hosts")).toHaveCount(0);
  await app.getByTestId("agent-executor-personal-select-trigger").click();
  await app.getByRole("option", { name: /我的私人 Mac/ }).click();

  // Fill in required fields and save.
  await app.getByTestId("agent-name").fill("个人直连助手");
  await app.getByTestId("agent-system").fill("在我自己的机器上跑。");
  await app.getByTestId("agent-submit").click();
  await app.waitForURL(/\/agents\/agent_/);

  // Shown again: host selected, source stays on personal direct, same machine.
  await expect(app.getByTestId("executor-host")).toHaveAttribute("data-selected", "true");
  await expect(app.getByTestId("executor-source-personal")).toHaveAttribute(
    "data-selected",
    "true",
  );
  await expect(app.getByTestId("agent-executor-personal-select-trigger")).toContainText(
    "我的私人 Mac",
  );
});

test("Agent 表单：切换来源会清空上一组的机器选择", async ({ app }) => {
  await seedHosts(app, [
    host({ id: "hst_a", name: "空间机", os: "linux", arch: "amd64" }),
    host({ id: "hst_b", name: "私人机", os: "darwin", arch: "arm64" }),
  ]);
  await bindViaApi(app, "hst_a");

  await app.goto("/w/growth/agents/new");
  await app.getByTestId("executor-host").click();

  // Pick one in the workspace group.
  await app.getByTestId("agent-executor-host-trigger").click();
  await app.getByRole("option", { name: /空间机/ }).click();
  await expect(app.getByTestId("agent-executor-host-trigger")).toContainText("空间机");

  // Switch to the personal group: the workspace machine must be cleared and the trigger goes back to
  // the placeholder; pick another one in the personal group.
  await app.getByTestId("executor-source-personal").click();
  await expect(app.getByTestId("agent-executor-host-trigger")).toHaveCount(0);
  await app.getByTestId("agent-executor-personal-select-trigger").click();
  await app.getByRole("option", { name: /私人机/ }).click();
  await expect(app.getByTestId("agent-executor-personal-select-trigger")).toContainText(
    "私人机",
  );
});

test("Agent 表单：非创建者停在空间指派时，个人直连卡片点不动并给提示", async ({ app }) => {
  // One machine bound to the workspace + a host agent "created by someone else, currently using workspace assignment".
  await seedHosts(app, [
    host({ id: "hst_ws1", name: "空间办公机", os: "linux", arch: "amd64" }),
  ]);
  await bindViaApi(app, "hst_ws1");
  await app.evaluate(() => {
    const client = window.__amaApi as unknown as {
      store: { agents: Array<Record<string, unknown>> };
      commit: () => void;
    };
    const base = client.store.agents.find((a) => a.workspaceId === "ws_demo");
    if (!base) throw new Error("种子里没有 ws_demo 的 agent 可克隆");
    client.store.agents.push({
      ...structuredClone(base),
      id: "agent_other_host",
      name: "别人的宿主机助手",
      createdBy: "bob@example.com",
      executorType: "host",
      hostSource: "workspace",
      hostId: "hst_ws1",
    });
    client.commit();
  });

  await app.goto("/w/growth/agents/agent_other_host");
  await app.getByTestId("executor-host").click();

  // Not the creator: the personal-direct card is clearly disabled, with a note on whom to ask; the
  // workspace-assigned card can still be edited.
  const personal = app.getByTestId("executor-source-personal");
  await expect(personal).toHaveAttribute("data-disabled", "true");
  await expect(app.getByTestId("agent-executor-personal-forbidden")).toContainText(
    "bob@example.com",
  );
  await expect(app.getByTestId("executor-source-workspace")).toHaveAttribute(
    "data-selected",
    "true",
  );

  // Even when clicked by force (force through the disabled styling), the source must stay on workspace:
  // do not let the user pick a machine, fill in the form, and only hit the backend 403 on save.
  await personal.click({ force: true });
  await expect(app.getByTestId("executor-source-workspace")).toHaveAttribute(
    "data-selected",
    "true",
  );
  await expect(app.getByTestId("agent-executor-personal-select")).toHaveCount(0);
});

/**
 * Role permission table. **This file is generated. Do not edit it by hand.**
 *
 * To regenerate:
 *
 *     cd server && go run ./cmd/gen-authz-capabilities
 *
 * The source of truth is `server/authz/matrix.go` (each route declares the level it
 * needs) plus `server/authz/capability.go` (groups routes by feature area into
 * one human-readable row).
 *
 * # Why generated instead of hand-written
 *
 * A hand-written permission table **will drift**: the next time someone adds a route, this
 * table silently goes stale. And a stale permission table is worse than none: users trust it,
 * think they can do something, get a 403, then ask an admin for permission, and the admin looks
 * at the same table and also thinks they should be able to. Two people fooled by the same page.
 *
 * Drift is blocked by a machine: `go test ./shared/gopkg/authzdoc/` derives it
 * again and compares it with this file byte by byte, and fails on any difference; and "added a
 * route but did not categorize it" already errors on the Go side.
 *
 * ⚠️ **owner and admin have exactly the same permissions.** This is not a missing check mark;
 * see ROLE_NOTE below.
 */

import type { WorkspaceRole } from "@/types/domain";

/** One row of the permission table. */
export interface RoleCapability {
  /** Stable id, used by e2e and as the React key. */
  id: string;
  /** Feature area name, shown on screen as is. */
  label: string;
  /** Extra explanation; may be an empty string. */
  detail: string;
  /** The lowest role that can do this. */
  minRole: WorkspaceRole;
  /**
   * All roles that can do this.
   *
   * ⚠️ **owner and admin are always both in or both out**: on the server, levelOf puts them
   * at the same level, and this array is computed from it, not copied.
   */
  roles: readonly WorkspaceRole[];
  /** Action level: read / write / manage. */
  action: "read" | "write" | "manage";
  /**
   * The routes this row covers, like `PATCH /api/agents/{id}`.
   *
   * **The UI does not show it.** It is here so every row can be traced back to matrix.go,
   * and so that "someone added a route" is visible in this file's diff.
   */
  routes: readonly string[];
}

/** Role order in the table header, from most to least permission. Matches authz.RoleOrder() on the server. */
export const ROLE_COLUMNS: readonly WorkspaceRole[] = ["owner", "admin", "member", "viewer"];

/**
 * The sentence that must be shown under the table.
 *
 * ⚠️ **It is not an optional note.** "owner has more permission than admin" is a strong
 * intuition; when those two columns look identical, the user's first thought is "this table is
 * wrong / unfinished". So the text must state that "identical is correct", plus the one extra
 * thing owner has.
 */
export const ROLE_NOTE =
  "拥有者与管理员的权限完全相同；拥有者只多一条保护：不能被移除，且空间至少要有一位。";

/**
 * Second sentence under the table: non-members.
 *
 * Deliberately **not drawn as a column of all crosses**: a non-member is not "the role with
 * the least permission", they **cannot see this workspace at all** (the workspace list does
 * not return workspaces they are not in). A column would suggest they can get in but cannot do
 * anything.
 */
export const NON_MEMBER_NOTE = "不在成员名单里的人无法访问这个空间，也不会在空间列表里看到它。";

/**
 * Third sentence under the table: account-level features.
 *
 * ⚠️ **It must be shown**, otherwise users wonder "why is authorization management not in the
 * table". Those features (authorization management, API Key, UI preferences, issue reports)
 * are account-level: the server sets ActionNone, they have nothing to do with workspace roles,
 * and any logged-in user can use them. They have moved to global settings.
 */
export const ACCOUNT_SCOPE_NOTE =
  "授权管理、API Key、界面偏好等属于账号级设置，与空间角色无关，在「全局设置」里。";

/** All rows, sorted by lowest role from low to high (viewer → member → admin). */
export const ROLE_CAPABILITIES: readonly RoleCapability[] = [
  {
    id: "view-content",
    label: "查看空间内容",
    detail: "Agent、技能、运行历史与对话记录、定时任务、问题、飞书接入状态，以及空间可用的宿主机列表（Agent 编辑页选执行端要用）",
    minRole: "viewer",
    roles: ["owner", "admin", "member", "viewer"],
    action: "read",
    routes: ["GET /api/agents/{id}", "GET /api/agents/{id}/ark-console", "GET /api/agents/{id}/revisions", "GET /api/agents/{id}/revisions/{revisionId}", "GET /api/agents/{id}/sync-task", "GET /api/feishu/apps/{id}", "GET /api/issue-attachments/{id}", "GET /api/issues/{id}", "GET /api/issues/{id}/attachments", "GET /api/issues/{id}/comments", "GET /api/runs/{id}", "GET /api/runs/{runId}/ark-console", "GET /api/runs/{runId}/events", "GET /api/runs/{runId}/events/stream", "GET /api/runs/{runId}/pending", "GET /api/schedules/{id}", "GET /api/schedules/{id}/revisions", "GET /api/schedules/{id}/revisions/{revisionId}", "GET /api/skills/{id}", "GET /api/skills/{id}/ark-console", "GET /api/skills/{id}/file", "GET /api/skills/{id}/revisions", "GET /api/skills/{id}/revisions/{revisionId}", "GET /api/workspaces/{workspaceId}/agents", "GET /api/workspaces/{workspaceId}/feishu/apps", "GET /api/workspaces/{workspaceId}/feishu/chats", "GET /api/workspaces/{workspaceId}/hosts", "GET /api/workspaces/{workspaceId}/issues", "GET /api/workspaces/{workspaceId}/runs", "GET /api/workspaces/{workspaceId}/schedules", "GET /api/workspaces/{workspaceId}/skills", "GET /api/workspaces/{workspaceId}/usage"],
  },
  {
    id: "view-sandbox",
    label: "查看会话容器/宿主机状态，并换取网页终端入场票",
    detail: "查看是只读的；终端票能换到的终端仍按运行分权限：容器/空间宿主机要管理员，个人直连宿主机只有机器属主本人",
    minRole: "viewer",
    roles: ["owner", "admin", "member", "viewer"],
    action: "read",
    routes: ["GET /api/runs/{runId}/sandbox", "GET /api/runs/{runId}/sandbox/file", "GET /api/runs/{runId}/sandbox/files", "POST /api/runs/{runId}/sandbox/terminal/ticket"],
  },
  {
    id: "view-ark",
    label: "查看方舟凭证",
    detail: "只有掩码，明文永远不出服务端；也能点「校验」",
    minRole: "viewer",
    roles: ["owner", "admin", "member", "viewer"],
    action: "read",
    routes: ["GET /api/agents/{id}/ark-credential", "GET /api/workspaces/{workspaceId}/ark-credential", "POST /api/agents/{id}/ark-credential/verify", "POST /api/workspaces/{workspaceId}/ark-credential/verify"],
  },
  {
    id: "view-ark-proxy",
    label: "读取方舟侧资源",
    detail: "列出/查询会话挂载的文件与线程、Files、保管库 Vault、记忆库与记忆条目；这些是方舟 Managed Agents 的原始接口，字段与方舟一致",
    minRole: "viewer",
    roles: ["owner", "admin", "member", "viewer"],
    action: "read",
    routes: ["GET /api/runs/{runId}/ark/files", "GET /api/runs/{runId}/ark/files/{fileId}", "GET /api/runs/{runId}/ark/resources", "GET /api/runs/{runId}/ark/resources/{resourceId}", "GET /api/runs/{runId}/ark/threads", "GET /api/runs/{runId}/ark/threads/{threadId}", "GET /api/runs/{runId}/ark/threads/{threadId}/events", "GET /api/runs/{runId}/ark/threads/{threadId}/stream", "GET /api/workspaces/{workspaceId}/ark/memory-stores", "GET /api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}", "GET /api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}/memories", "GET /api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}/memories/{memoryId}", "GET /api/workspaces/{workspaceId}/ark/vaults", "GET /api/workspaces/{workspaceId}/ark/vaults/{vaultId}", "GET /api/workspaces/{workspaceId}/ark/vaults/{vaultId}/credentials", "GET /api/workspaces/{workspaceId}/ark/vaults/{vaultId}/credentials/{credentialId}"],
  },
  {
    id: "create-api-key",
    label: "创建 API Key",
    detail: "key 严格继承你本人的权限，不是一套独立的作用域",
    minRole: "viewer",
    roles: ["owner", "admin", "member", "viewer"],
    action: "read",
    routes: ["GET /api/workspaces/{workspaceId}/api-keys", "POST /api/workspaces/{workspaceId}/api-keys"],
  },
  {
    id: "view-own-role",
    label: "查看自己在本空间的角色",
    detail: "任何成员都能查，界面靠它决定哪些操作画成只读",
    minRole: "viewer",
    roles: ["owner", "admin", "member", "viewer"],
    action: "read",
    routes: ["GET /api/workspaces/{workspaceId}/my-role"],
  },
  {
    id: "converse",
    label: "发起对话、发消息",
    detail: "一次对话就是一条运行记录，所以它同时进运行历史",
    minRole: "member",
    roles: ["owner", "admin", "member"],
    action: "write",
    routes: ["DELETE /api/runs/{runId}/pending/{messageId}", "POST /api/runs/{runId}/events", "POST /api/runs/{runId}/pending", "POST /api/runs/{runId}/redirect", "POST /api/workspaces/{workspaceId}/conversations"],
  },
  {
    id: "edit-agent",
    label: "创建与修改 Agent",
    detail: "",
    minRole: "member",
    roles: ["owner", "admin", "member"],
    action: "write",
    routes: ["DELETE /api/agents/{id}", "PATCH /api/agents/{id}", "POST /api/agents/{id}/revisions/{revisionId}/rollback", "POST /api/agents/{id}/sync-task", "POST /api/workspaces/{workspaceId}/agents"],
  },
  {
    id: "edit-skill",
    label: "创建与修改技能",
    detail: "含上传技能包",
    minRole: "member",
    roles: ["owner", "admin", "member"],
    action: "write",
    routes: ["DELETE /api/skills/{id}", "PATCH /api/skills/{id}", "POST /api/skills/{id}/revisions/{revisionId}/rollback", "POST /api/workspaces/{workspaceId}/skills", "POST /api/workspaces/{workspaceId}/skills/fork", "POST /api/workspaces/{workspaceId}/skills/upload", "PUT /api/skills/{id}/file"],
  },
  {
    id: "ark-proxy-session-data",
    label: "操作会话相关方舟资源",
    detail: "给会话挂文件（含上传 Files）、升级会话运行配置、写记忆库与记忆条目；消耗这个空间方舟账号的配额",
    minRole: "member",
    roles: ["owner", "admin", "member"],
    action: "write",
    routes: ["DELETE /api/runs/{runId}/ark/files/{fileId}", "DELETE /api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}", "DELETE /api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}/memories/{memoryId}", "POST /api/runs/{runId}/ark/files", "POST /api/runs/{runId}/ark/resources", "POST /api/runs/{runId}/ark/upgrade", "POST /api/workspaces/{workspaceId}/ark/memory-stores", "POST /api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}", "POST /api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}/memories", "POST /api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}/memories/batch-create", "POST /api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}/memories/{memoryId}"],
  },
  {
    id: "edit-schedule",
    label: "管理定时任务",
    detail: "",
    minRole: "member",
    roles: ["owner", "admin", "member"],
    action: "write",
    routes: ["DELETE /api/schedules/{id}", "PATCH /api/schedules/{id}", "PATCH /api/schedules/{id}/status", "POST /api/schedules/{id}/revisions/{revisionId}/rollback", "POST /api/schedules/{id}/run-now", "POST /api/workspaces/{workspaceId}/schedules"],
  },
  {
    id: "edit-issue",
    label: "提问题、评论与附件",
    detail: "",
    minRole: "member",
    roles: ["owner", "admin", "member"],
    action: "write",
    routes: ["DELETE /api/issue-attachments/{id}", "DELETE /api/issue-comments/{id}", "DELETE /api/issues/{id}", "PATCH /api/issue-comments/{id}", "PATCH /api/issues/{id}", "POST /api/issues/{id}/attachments", "POST /api/issues/{id}/comments", "POST /api/workspaces/{workspaceId}/issues"],
  },
  {
    id: "resume-sandbox",
    label: "恢复已暂停的会话容器",
    detail: "会占用计算配额，所以不给只读角色",
    minRole: "member",
    roles: ["owner", "admin", "member"],
    action: "write",
    routes: ["POST /api/runs/{runId}/sandbox/resume"],
  },
  {
    id: "manage-workspace",
    label: "空间设置与删除空间",
    detail: "改名、改 key（等于改所有链接）、删空间",
    minRole: "admin",
    roles: ["owner", "admin"],
    action: "manage",
    routes: ["DELETE /api/workspaces/{workspaceId}", "PATCH /api/workspaces/{workspaceId}"],
  },
  {
    id: "manage-member",
    label: "成员管理",
    detail: "邀请、改角色、移除",
    minRole: "admin",
    roles: ["owner", "admin"],
    action: "manage",
    routes: ["DELETE /api/workspaces/{workspaceId}/members/{memberId}", "PATCH /api/workspaces/{workspaceId}/members/{memberId}", "POST /api/workspaces/{workspaceId}/members"],
  },
  {
    id: "manage-ark",
    label: "配置方舟凭证",
    detail: "换掉或删掉会影响这个空间所有 Agent 的对话，账也记在那把 key 上",
    minRole: "admin",
    roles: ["owner", "admin"],
    action: "manage",
    routes: ["DELETE /api/agents/{id}/ark-credential", "DELETE /api/workspaces/{workspaceId}/ark-credential", "PATCH /api/agents/{id}/ark-credential/region", "PATCH /api/workspaces/{workspaceId}/ark-credential/region", "POST /api/workspaces/{workspaceId}/ark-credential/use-global", "PUT /api/agents/{id}/ark-credential", "PUT /api/workspaces/{workspaceId}/ark-credential"],
  },
  {
    id: "manage-ark-proxy-vault",
    label: "方舟保管库 Vault 与凭证",
    detail: "Vault 里装的是第三方凭据（MCP OAuth / bearer token），Agent 运行时会以凭证主人的身份行动；让普通成员自助写等于让他决定 Agent 以谁的身份访问外部系统",
    minRole: "admin",
    roles: ["owner", "admin"],
    action: "manage",
    routes: ["DELETE /api/workspaces/{workspaceId}/ark/vaults/{vaultId}", "DELETE /api/workspaces/{workspaceId}/ark/vaults/{vaultId}/credentials/{credentialId}", "POST /api/workspaces/{workspaceId}/ark/vaults", "POST /api/workspaces/{workspaceId}/ark/vaults/{vaultId}", "POST /api/workspaces/{workspaceId}/ark/vaults/{vaultId}/credentials", "POST /api/workspaces/{workspaceId}/ark/vaults/{vaultId}/credentials/{credentialId}"],
  },
  {
    id: "manage-feishu",
    label: "飞书接入",
    detail: "注册应用、解除接入、改免 @ 触发",
    minRole: "admin",
    roles: ["owner", "admin"],
    action: "manage",
    routes: ["DELETE /api/feishu/apps/{id}", "GET /api/workspaces/{workspaceId}/feishu/registrations/{sessionId}", "PATCH /api/feishu/apps/{id}/trigger-mode", "POST /api/feishu/apps/{id}/admin-authorization", "POST /api/feishu/apps/{id}/readiness/refresh", "POST /api/workspaces/{workspaceId}/feishu/registrations"],
  },
  {
    id: "manage-integration",
    label: "把授权注入给 Agent",
    detail: "个人授权代表一个真人，**只有 Agent 的创建者本人能加减，管理员也不行**",
    minRole: "admin",
    roles: ["owner", "admin"],
    action: "manage",
    routes: ["PUT /api/agents/{id}/integrations"],
  },
  {
    id: "manage-host",
    label: "宿主机绑定",
    detail: "把成员自己注册的宿主机绑进空间，Agent 才能选它做执行端（v1 只能绑本人的机器）",
    minRole: "admin",
    roles: ["owner", "admin"],
    action: "manage",
    routes: ["DELETE /api/workspaces/{workspaceId}/hosts/{hostId}", "POST /api/workspaces/{workspaceId}/hosts"],
  },
  {
    id: "manage-sandbox-shell",
    label: "会话容器的终端与命令执行",
    detail: "等于空间数据的完整读写权，所以只给管理员",
    minRole: "admin",
    roles: ["owner", "admin"],
    action: "manage",
    routes: ["GET /api/runs/{runId}/sandbox/terminal", "POST /api/runs/{runId}/sandbox/shell", "POST /api/runs/{runId}/sandbox/terminal/input"],
  },
];

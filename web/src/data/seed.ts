/**
 * Seed data.
 *
 * Run history is the most complex view on this platform, so the seed must contain one truly
 * complex run: multi-thread collaboration, built-in tools, MCP tools, tools that need user
 * confirmation, error retries, context compaction, outcome evaluation. Otherwise the timeline's
 * classification / pairing / filtering logic cannot be verified.
 */

import type {
  Agent,
  FeishuApp,
  Run,
  Skill,
  Workspace,
} from "@/types/domain";
import type { RunEvent } from "@/types/events";

export interface SeedData {
  workspaces: Workspace[];
  agents: Agent[];
  skills: Skill[];
  feishuApps: FeishuApp[];
  runs: Run[];
  events: Record<string, RunEvent[]>;
}

const WS = "ws_demo";
const T0 = Date.parse("2026-08-25T10:00:00Z");

/** ISO timestamp a number of seconds after T0, so the seed timeline reads as coherent. */
function at(offsetSec: number): string {
  return new Date(T0 + offsetSec * 1000).toISOString();
}

/**
 * Timestamp with **nanosecond** fraction and a `+08:00` offset: the real shape of Ark's
 * `processed_at`.
 *
 * Its only purpose is to make sure event-view sorting is really tested. Measured on production
 * `run_events`: the fraction width varies (9 digits 2360 rows / 4 digits 76 rows / 6 digits 2
 * rows), two time zone forms (`+08:00` and literal `Z`) coexist, and **several rows in the same
 * millisecond is normal** (662 events had only 400 distinct milliseconds). JS `Date` only goes to
 * milliseconds and drops nanoseconds, so within the same millisecond `sequence` has to break the
 * tie.
 *
 * `subMicros` is **the 6 digits after the milliseconds** (micro + nano). It only affects the
 * literal, not the milliseconds `Date` parses, which is exactly how we build "same instant but
 * ordered" shapes: in the seed, sevt-0603/0604/0605 share one millisecond and differ only in
 * these 6 digits.
 *
 * ⚠️ **The fraction must be exactly 9 digits** (3 millisecond digits + 6 subMicros), so pass 6
 * digits as `subMicros`. The first version appended 9 nanosecond digits to the first 23 chars of
 * `toISOString()` (which already include `.400`), giving **12** fraction digits, so `Date` only
 * took the first 3 as milliseconds and dropped the rest: `.000778899001` parsed as **778
 * milliseconds**, and row order got scrambled (in the sort assertion, seq 100 ran ahead of 99).
 * Cut to **seconds** and build the rest ourselves, and the length check below catches this kind
 * of slip at runtime.
 */
function atNano(offsetSec: number, millis: number, subMicros: string): string {
  if (subMicros.length !== 6) {
    throw new Error(`atNano: subMicros 必须是 6 位，收到 ${subMicros.length} 位`);
  }
  const d = new Date(T0 + offsetSec * 1000 + millis);
  // Build +08:00 by hand: toISOString only gives Z, and having both time zone forms is itself a shape we need to cover.
  const shifted = new Date(d.getTime() + 8 * 3600 * 1000);
  const secs = shifted.toISOString().slice(0, 19);
  const ms3 = String(millis).padStart(3, "0");
  return `${secs}.${ms3}${subMicros}+08:00`;
}

let seq = 0;

/**
 * Assign increasing sequence numbers.
 *
 * Uses a distributive conditional type rather than `Omit<T, "sequence">`: the latter stops TS
 * from inferring T from the argument and flags every concrete event field as "unknown property".
 * Distributing gives a union of Omit for each event type, and literals match normally.
 */
type EventInput = RunEvent extends infer U ? (U extends RunEvent ? Omit<U, "sequence"> : never) : never;

function ev(e: EventInput): RunEvent {
  return { ...e, sequence: ++seq } as RunEvent;
}

/**
 * Reserve n consecutive sequence numbers and return them.
 *
 * Used in only one place: in run_6, three **same-millisecond** events are deliberately out of
 * order in the array, and reserved numbers pin their sequence to the logical order. That makes
 * "array order" and "sequence order" differ, so `return a.sequence - b.sequence` in the event
 * view sort really runs.
 *
 * ⚠️ **Do not change this to "hard-code a few numbers".** The first version used literals like
 * `withSeq(91, ...)`, and `ev()` later handed out 91 anyway, so one run had 3 pairs of duplicate
 * sequences (measured: seq 91/92/93 each collided once). sequence is Ark's total order within a
 * session; duplicates make "fall back to sequence" pointless and may also collide React keys.
 * Taking numbers from the same counter is the only safe way.
 */
function reserveSeq(n: number): number[] {
  return Array.from({ length: n }, () => ++seq);
}

// ---------------------------------------------------------------------------
// Workspaces
// ---------------------------------------------------------------------------

const workspaces: Workspace[] = [
  {
    id: WS,
    name: "增长中台",
    key: "growth",
    description: "增长业务线的 agent 工作空间",
    createdAt: at(-86400 * 30),
    // Equal to createdAt = "never changed". Renaming / changing key / adding, removing or editing members all refresh it.
    updatedAt: at(-86400 * 30),
    members: [
      // The owner is the current user in mock (MOCK_USER); the two emails must match: the
      // workspace list filters by membership, and a mismatch gives "logged in but no workspaces",
      // with no error at all.
      //
      // ⚠️ The unique key of workspace_members is (workspace_id, lowercase email); do not add the same person twice.
      {
        id: "m_1",
        name: "张三",
        email: "alice@example.com",
        role: "owner",
        joinedAt: at(-86400 * 30),
      },
      {
        id: "m_2",
        name: "李维",
        email: "bob@example.com",
        role: "admin",
        joinedAt: at(-86400 * 21),
      },
      {
        id: "m_3",
        name: "王朗",
        email: "carol@example.com",
        role: "member",
        joinedAt: at(-86400 * 14),
      },
      {
        id: "m_4",
        name: "赵晴",
        email: "dave@example.com",
        role: "viewer",
        joinedAt: at(-86400 * 3),
      },
    ],
  },
];

// ---------------------------------------------------------------------------
// Skills
// ---------------------------------------------------------------------------

const skills: Skill[] = [
  {
    id: "skill-20260801-triage",
    workspaceId: WS,
    name: "ticket-triage",
    displayTitle: "工单分诊",
    description:
      "当用户反馈问题、报障或提交工单时使用。判定严重级别、归类到业务模块、给出下一步处理建议。",
    instructions: `# 工单分诊

## 分诊步骤

1. 读完整条反馈，先判断是否可复现。
2. 按严重级别归类：P0 全站不可用、P1 核心链路受损、P2 单点功能异常、P3 体验问题。
3. 归类到业务模块：投放、结算、报表、账号。
4. 输出结论时必须包含：级别、模块、判定依据、建议处理人。

## 判定依据要写清楚

不要只给结论。「P1，因为下单接口 500 且影响所有商家」比「P1」有用得多。`,
    source: "custom",
    latestVersion: "3",
    tags: ["客服", "分诊"],
    createdAt: at(-86400 * 20),
    updatedAt: at(-86400 * 2),
  },
  {
    id: "skill-20260805-sql",
    workspaceId: WS,
    name: "metrics-sql",
    displayTitle: "指标 SQL 编写",
    description:
      "当需要查询业务指标、拉取数据看板数字、做同环比分析时使用。包含数仓表结构与口径约定。",
    instructions: `# 指标 SQL 编写

## 常用表

- \`dwd_order_detail\`：订单明细，分区字段 \`date\`，必须带分区过滤。
- \`dws_merchant_daily\`：商家日汇总。
- \`dim_merchant\`：商家维表，注意有 \`is_test\` 标记，正式统计要排除。

## 口径约定

- GMV 一律用 \`pay_amount\` 而非 \`order_amount\`（后者含未支付）。
- 同比看去年同期**同一周的对应日**，不是同一日期。
- 任何查询都要排除 \`is_test = 1\` 的测试商家。`,
    source: "custom",
    latestVersion: "5",
    tags: ["数据", "SQL"],
    createdAt: at(-86400 * 16),
    updatedAt: at(-86400 * 5),
  },
  {
    id: "skill-20260810-report",
    workspaceId: WS,
    name: "weekly-report",
    displayTitle: "周报撰写",
    description: "当需要产出周报、汇总本周进展、写业务复盘时使用。规定了周报结构与写法。",
    instructions: `# 周报撰写

## 结构

1. **核心结论**（3 条以内，每条一句话）
2. **数据表现**（带同环比）
3. **风险与阻塞**
4. **下周计划**

## 写法

结论先行。不要按时间顺序罗列做过的事，要按重要性排序讲影响。
数字必须带对比基准，「GMV 1200 万」没有信息量，「GMV 1200 万，环比 +8%」才有。`,
    source: "custom",
    latestVersion: "2",
    tags: ["文档"],
    createdAt: at(-86400 * 11),
    updatedAt: at(-86400 * 11),
  },
  {
    id: "s-hub-code-review",
    workspaceId: WS,
    name: "code-review",
    displayTitle: "代码审查",
    description:
      "当需要审查代码变更、评估 MR 风险、或检查安全与可维护性问题时使用。",
    instructions: `# 代码审查

## 何时用

用户贴出 diff、MR 链接，或问「这段代码有什么问题」时。

## 审查顺序

先看正确性，再看安全，最后看可维护性——顺序不能反：
一段会崩的代码，命名再好也没有意义。

1. **正确性**：边界条件、空值、并发。特别注意「单实例跑对、多实例跑错」的写法
   （进程内状态、先查再建、读改写）。
2. **安全**：注入、越权、凭据进日志。
3. **可维护性**：命名、重复、注释是否解释了「为什么」而不是「做了什么」。

## 输出格式

按严重级别倒序列出，每条给：问题在哪一行、会怎么坏、怎么改。

**不要罗列「建议」而不说后果**——说不出后果的问题通常不是问题。`,
    source: "custom",
    latestVersion: "1.2.0",
    tags: ["研发"],
    createdAt: at(-86400 * 9),
    updatedAt: at(-86400 * 9),
  },
];

// ---------------------------------------------------------------------------
// Feishu apps
// ---------------------------------------------------------------------------

const feishuApps: FeishuApp[] = [
  {
    id: "fs_1",
    workspaceId: WS,
    name: "增长助手",
    appId: "cli_a1b2c3d4e5f60011",
    region: "feishu",
    tenantBrand: "feishu",
    status: "connected",
    // The two seed apps deliberately use **different** trigger modes: if both used the default, a
    // selector rendering the wrong option (say, always the first) would not show in the UI.
    triggerMode: "mention_only",
    // One Feishu app can only be bound to one agent (1:1 exclusive; see ensureFeishuAppsFree in
    // server/core/agent.go). This used to be ["agent_triage", "agent_analyst"], which broke that
    // rule, so inbound messages could only pick a responder by row order.
    boundAgentIds: ["agent_triage"],
    readiness: {
      status: "ready",
      checkedAt: at(-600),
      missingScopes: [],
      unapprovedScopes: [],
      missingEvents: [],
      missingCallbacks: [],
      configurationInspectable: true,
      botEnabled: true,
      published: true,
      groupMessageScopeGranted: true,
      groupMessageScope: "im:message.group_msg",
      botMessageScopeGranted: true,
      botMessageScope: "im:message.group_msg.include_bot:read",
      adminAuthorizationRequested: false,
      runtimeConnected: true,
      lastConnectedAt: at(-600),
    },
    createdById: "u_1",
    createdAt: at(-86400 * 18),
    updatedAt: at(-600),
  },
  {
    // This one deliberately models "waiting for an admin to approve permissions": the seed must
    // cover the not-ready branch of readiness, or that part of the UI can only be verified in
    // imagination.
    id: "fs_2",
    workspaceId: WS,
    name: "数据播报机器人",
    appId: "cli_9f8e7d6c5b4a0022",
    region: "feishu",
    tenantBrand: "feishu",
    status: "unverified",
    triggerMode: "topic_and_mention",
    // Matches agent_analyst.feishuAppIds. The two directions must agree, so LocalApiClient's
    // syncFeishuBindings does not conflict.
    boundAgentIds: ["agent_analyst"],
    readiness: {
      status: "waiting_for_admin",
      checkedAt: at(-1800),
      missingScopes: [],
      unapprovedScopes: ["im:message.group_msg", "im:message:send_as_bot"],
      missingEvents: [],
      missingCallbacks: [],
      configurationInspectable: true,
      botEnabled: true,
      published: true,
      groupMessageScopeGranted: false,
      groupMessageScope: "im:message.group_msg",
      // Not authorized, so include the authorization entry: this fixture is exactly "chose no-@
      // but Feishu did not grant the permission", and e2e uses it to check whether the warning
      // appears.
      groupMessageAuthURL:
        "https://open.feishu.cn/page/scope-apply?clientID=cli_9f8e7d6c5b4a0022&scopes=im:message.group_msg",
      // The level that includes bots was not granted either. **Both are deliberately false**:
      // existing apps only requested group_msg originally, so "both missing" is the most common
      // case in production.
      botMessageScopeGranted: false,
      botMessageScope: "im:message.group_msg.include_bot:read",
      botMessageAuthURL:
        "https://open.feishu.cn/page/scope-apply?clientID=cli_9f8e7d6c5b4a0022&scopes=im:message.group_msg.include_bot:read,im:message.group_at_msg.include_bot:readonly",
      adminAuthorizationRequested: true,
      runtimeConnected: true,
      lastConnectedAt: at(-1800),
    },
    adminAuthorizationRequestedAt: at(-3600),
    createdById: "u_1",
    createdAt: at(-86400 * 4),
    updatedAt: at(-1800),
  },
];

// ---------------------------------------------------------------------------
// Agent
// ---------------------------------------------------------------------------

const agents: Agent[] = [
  {
    id: "agent_triage",
    workspaceId: WS,
    name: "工单分诊助手",
    description: "接飞书群消息，自动分诊用户反馈并给出处理建议",
    system: `你是增长业务线的工单分诊助手。

用户会把各种反馈丢给你，你的职责是快速判定严重级别、归到业务模块、并给出下一步建议。

原则：
- 结论先行，不要复述用户的话。
- 判定依据必须写出来，让接手的人能验证你的判断。
- 信息不足时明确说缺什么，不要凭空猜。`,
    model: {
      id: "doubao-seed-2-1-pro-260628",
      thinking: "enabled",
      reasoning_effort: "high",
      service_tier: "default",
    },
    version: 4,
    builtin_tools: ["read", "grep", "web_search"],
    skillIds: ["skill-20260801-triage"],
    feishuAppIds: ["fs_1"],
    status: "active",
    // Feishu user authorization is checked: the agent in the container can read and write Feishu as the current user.
    integrationIds: ["lark-user"],
    envs: [],
    envAliases: [],
    // Empty string = default level (redirect). Matches the Go-side seed.json exactly; that file
    // is exported from this one (scripts/export-seed), and the two must agree.
    followUp: "",
    progressDetail: "",
    createdAt: at(-86400 * 19),
    updatedAt: at(-86400 * 2),
  },
  {
    id: "agent_analyst",
    workspaceId: WS,
    name: "数据分析师",
    description: "按自然语言问题查数仓、算指标、产出结论",
    system: `你是增长业务线的数据分析师。

用户会用自然语言问你业务指标，你需要：
1. 先确认口径（时间范围、指标定义、过滤条件），有歧义就问清楚。
2. 写 SQL 查数。
3. 给结论，附上数字和对比基准。

不要把 SQL 直接丢给用户当答案 —— 用户要的是结论，SQL 是过程。`,
    model: {
      id: "doubao-seed-2-1-turbo-260628",
      thinking: "enabled",
      reasoning_effort: "medium",
      service_tier: "fast",
    },
    version: 7,
    builtin_tools: ["bash", "read", "write", "grep"],
    skillIds: ["skill-20260805-sql", "skill-20260810-report"],
    // fs_2 rather than fs_1: one Feishu app can only be bound to one agent (1:1 exclusive), and
    // fs_1 belongs to agent_triage. The seed used to give fs_1 to both agents; that data would now
    // be rejected by ensureFeishuAppsFree in core.Service.
    feishuAppIds: ["fs_2"],
    status: "active",
    // Both integrations checked, plus one plain env and one sensitive env, covering both paths:
    // "plain entries go into the <harness:env> system prompt, sensitive ones are only injected
    // into the container".
    integrationIds: ["ama", "lark-user"],
    envs: [
      { key: "DEPLOY_REGION", value: "cn-east", secret: false },
      { key: "GRAFANA_TOKEN", value: "glsa_seed_only_not_real", secret: true },
    ],
    envAliases: [],
    // Empty string = default level (redirect), same as above.
    followUp: "",
    progressDetail: "",
    // Created by Li Wei: the users table **has a name but no avatar**, so the list takes the initial-letter fallback.
    createdBy: "bob@example.com",
    updatedBy: "bob@example.com",
    createdAt: at(-86400 * 15),
    updatedAt: at(-86400 * 1),
  },
  {
    id: "agent_reviewer",
    workspaceId: WS,
    name: "代码审查员",
    description: "审查 MR 变更，输出问题清单",
    system: `你是代码审查员。审查给定的变更，找出正确性、安全性与可维护性问题。

只报真问题。风格偏好、个人口味不要报。每条问题要能说出「什么输入会导致什么错误结果」。`,
    model: {
      id: "deepseek-v4-pro-ga-260813",
      thinking: "enabled",
      reasoning_effort: "max",
      service_tier: "default",
    },
    version: 2,
    builtin_tools: ["read", "grep", "glob", "bash"],
    skillIds: ["s-hub-code-review"],
    feishuAppIds: [],
    status: "active",
    // No integration checked: this is **the default state of a new agent** (the requirement is that none is selected by default).
    integrationIds: [],
    envs: [],
    envAliases: [],
    // Empty string = default level (redirect). Matches the Go-side seed.json exactly; that file
    // is exported from this one (scripts/export-seed), and the two must agree.
    followUp: "",
    progressDetail: "",
    // Created by the current user (has a real avatar), but **last edited by Li Wei**: this one
    // specifically guards "createdBy and updatedBy are two fields". If an implementation merged
    // them into one, the "created by me" filter would miss or over-count this one, while the UI
    // alone would look normal.
    createdBy: "alice@example.com",
    updatedBy: "bob@example.com",
    createdAt: at(-86400 * 8),
    updatedAt: at(-86400 * 6),
  },
  {
    id: "agent_draft",
    workspaceId: WS,
    name: "周报生成器",
    description: "汇总本周数据与进展，产出周报草稿",
    system: "你负责产出周报草稿。结论先行，数字带对比基准。",
    model: {
      id: "glm-5-2-260617",
      thinking: "enabled",
      reasoning_effort: "max",
      service_tier: "auto",
    },
    version: 1,
    builtin_tools: ["read", "write"],
    skillIds: ["skill-20260810-report"],
    feishuAppIds: [],
    // This is **the disabled-state sample**; do not change it to active in passing.
    //
    // Disabling now really blocks things (no new sessions, no sending messages), so the seed needs
    // a disabled agent to show that path locally and in e2e. The id is still agent_draft so as not
    // to touch the two assertions in 02-agent.spec.ts that use it for the delete test.
    status: "disabled",
    integrationIds: [],
    envs: [],
    envAliases: [],
    // Empty string = default level (redirect). Matches the Go-side seed.json exactly; that file
    // is exported from this one (scripts/export-seed), and the two must agree.
    followUp: "",
    progressDetail: "",
    createdAt: at(-86400 * 1),
    updatedAt: at(-86400 * 1),
  },
];

// ---------------------------------------------------------------------------
// Run 1: complex multi-thread data analysis, covering as many event types as possible
// ---------------------------------------------------------------------------

const MAIN = "sth_main_01";
const SUB = "sth_sub_sql_01";

const run1Events: RunEvent[] = [
  ev({
    id: "sevt-0001",
    type: "session.status_running",
    processed_at: at(0),
    session_thread_id: null,
  }),
  ev({
    id: "sevt-0002",
    type: "session.thread_created",
    processed_at: at(0),
    session_thread_id: MAIN,
    agent_name: "数据分析师",
    parent_thread_id: null,
  }),
  ev({
    id: "sevt-0003",
    type: "user.message",
    processed_at: at(1),
    session_thread_id: MAIN,
    content: [
      {
        type: "text",
        text: "帮我看下上周华东区的 GMV 表现，跟前一周和去年同期都对比一下，有异常的话说明原因。",
      },
    ],
  }),
  ev({
    id: "sevt-0004",
    type: "system.message",
    processed_at: at(1),
    session_thread_id: MAIN,
    content: [
      {
        type: "text",
        text: "当前对话来自飞书群「增长数据日常」，提问者是华东区运营负责人，回答请偏业务视角。",
      },
    ],
  }),
  ev({
    id: "sevt-0005",
    type: "session.thread_status_running",
    processed_at: at(2),
    session_thread_id: MAIN,
    agent_name: "数据分析师",
  }),
  ev({
    id: "sevt-0006",
    type: "span.model_request_start",
    processed_at: at(2),
    session_thread_id: MAIN,
  }),
  ev({
    id: "sevt-0007",
    type: "agent.thinking",
    processed_at: at(3),
    session_thread_id: MAIN,
    // With content: measured, Ark does send thinking content (the docs say it does not; they are
    // wrong; see types/events.ts). The seed keeps one with content so the chain-of-thought
    // collapsible panels on the run detail and chat pages have something to show.
    content: [
      {
        type: "text",
        text: "用户要找 src/ 下遗留的 TODO。\n\n先用 grep 扫一遍，再逐个看上下文判断哪些已经做完了。",
      },
    ],
  }),
  ev({
    id: "sevt-0008",
    type: "span.model_request_end",
    processed_at: at(9),
    session_thread_id: MAIN,
    model_request_start_id: "sevt-0006",
    model_usage: {
      input_tokens: 4820,
      output_tokens: 312,
      cache_read_input_tokens: 3968,
      cache_creation_input_tokens: 852,
      speed: "fast",
    },
  }),
  ev({
    id: "sevt-0009",
    type: "agent.message",
    processed_at: at(9),
    session_thread_id: MAIN,
    content: [
      {
        type: "text",
        text: "我先确认一下口径：上周指 8/17–8/23，华东区按 `dim_merchant.region = 'east'` 圈定，GMV 用 `pay_amount`。同期对比取去年同一周的对应日。开始查数。",
      },
    ],
  }),
  // Built-in tool: read the definitions file in a skill
  ev({
    id: "sevt-0010",
    type: "agent.tool_use",
    processed_at: at(10),
    session_thread_id: MAIN,
    tool_use_id: "toolu_read_01",
    name: "read",
    input: JSON.stringify({ file_path: "/workspace/metrics/gmv_definition.md" }),
    evaluated_permission: "allow",
  }),
  ev({
    id: "sevt-0011",
    type: "agent.tool_result",
    processed_at: at(11),
    session_thread_id: MAIN,
    tool_use_id: "toolu_read_01",
    content: [
      {
        type: "text",
        text: "# GMV 口径\n\nGMV = SUM(pay_amount)，仅统计 pay_status IN (2, 3)。\n排除 is_test = 1 的测试商家。\n退款不从 GMV 里扣减，单独看退款率。",
      },
    ],
  }),
  // Create a sub-thread to run SQL
  ev({
    id: "sevt-0012",
    type: "session.thread_created",
    processed_at: at(12),
    session_thread_id: SUB,
    agent_name: "SQL 执行子智能体",
    parent_thread_id: MAIN,
  }),
  ev({
    id: "sevt-0013",
    type: "agent.thread_message_sent",
    processed_at: at(12),
    session_thread_id: MAIN,
    to_session_thread_id: SUB,
    to_agent_name: "SQL 执行子智能体",
    content: [
      {
        type: "text",
        text: "查询华东区 2026-08-17 至 2026-08-23 的日粒度 GMV，同时给出前一周与去年同期同一周的对应日数据。排除测试商家。",
      },
    ],
  }),
  ev({
    id: "sevt-0014",
    type: "agent.thread_message_received",
    processed_at: at(13),
    session_thread_id: SUB,
    from_session_thread_id: MAIN,
    content: [
      {
        type: "text",
        text: "查询华东区 2026-08-17 至 2026-08-23 的日粒度 GMV，同时给出前一周与去年同期同一周的对应日数据。排除测试商家。",
      },
    ],
  }),
  ev({
    id: "sevt-0015",
    type: "session.thread_status_running",
    processed_at: at(13),
    session_thread_id: SUB,
    agent_name: "SQL 执行子智能体",
  }),
  // MCP tool: query the data warehouse
  ev({
    id: "sevt-0016",
    type: "agent.mcp_tool_use",
    processed_at: at(14),
    session_thread_id: SUB,
    tool_use_id: "toolu_mcp_01",
    mcp_server_name: "data-warehouse",
    name: "run_query",
    input: JSON.stringify({
      sql: "SELECT date, SUM(pay_amount) AS gmv FROM dwd_order_detail o JOIN dim_merchant m ON o.merchant_id = m.id WHERE m.region = 'east' AND m.is_test = 0 AND o.pay_status IN (2,3) AND date BETWEEN '2026-08-17' AND '2026-08-23' GROUP BY date ORDER BY date",
      timeout_ms: 30000,
    }),
    evaluated_permission: "allow",
  }),
  // First query is rate limited, triggering a recoverable error + rescheduling
  ev({
    id: "sevt-0017",
    type: "session.error",
    processed_at: at(16),
    session_thread_id: SUB,
    error: {
      type: "model_rate_limited_error",
      message: "模型请求触发限流（TPM 超限），正在重试",
      retry_status: { type: "retrying" },
    },
  }),
  ev({
    id: "sevt-0018",
    type: "session.thread_status_rescheduled",
    processed_at: at(17),
    session_thread_id: SUB,
    agent_name: "SQL 执行子智能体",
  }),
  ev({
    id: "sevt-0019",
    type: "agent.mcp_tool_result",
    processed_at: at(22),
    session_thread_id: SUB,
    mcp_tool_use_id: "toolu_mcp_01",
    mcp_server_name: "data-warehouse",
    content: [
      {
        type: "text",
        text: "date | gmv\n2026-08-17 | 12480321.55\n2026-08-18 | 13102887.10\n2026-08-19 | 12995044.02\n2026-08-20 | 13488190.73\n2026-08-21 | 14201776.88\n2026-08-22 | 9877402.31\n2026-08-23 | 9120558.64\n\n(7 rows, 1.82s)",
      },
    ],
  }),
  // A tool that needs user confirmation (a write)
  ev({
    id: "sevt-0020",
    type: "agent.tool_use",
    processed_at: at(23),
    session_thread_id: SUB,
    tool_use_id: "toolu_write_01",
    name: "write",
    input: JSON.stringify({
      file_path: "/workspace/out/east_gmv_week34.csv",
      content: "date,gmv\n2026-08-17,12480321.55\n...",
    }),
    evaluated_permission: "ask",
  }),
  ev({
    id: "sevt-0021",
    type: "session.thread_status_idle",
    processed_at: at(23),
    session_thread_id: SUB,
    agent_name: "SQL 执行子智能体",
    stop_reason: { type: "requires_action", event_ids: ["sevt-0020"] },
  }),
  ev({
    id: "sevt-0022",
    type: "user.tool_confirmation",
    processed_at: at(41),
    session_thread_id: SUB,
    result: "allow",
    tool_use_id: "toolu_write_01",
  }),
  ev({
    id: "sevt-0023",
    type: "agent.tool_result",
    processed_at: at(42),
    session_thread_id: SUB,
    tool_use_id: "toolu_write_01",
    content: [{ type: "text", text: "已写入 /workspace/out/east_gmv_week34.csv（7 行）" }],
  }),
  // A failed tool call
  ev({
    id: "sevt-0024",
    type: "agent.mcp_tool_use",
    processed_at: at(43),
    session_thread_id: SUB,
    tool_use_id: "toolu_mcp_02",
    mcp_server_name: "data-warehouse",
    name: "run_query",
    input: JSON.stringify({
      sql: "SELECT date, SUM(pay_amount) AS gmv FROM dwd_order_detail_2025 WHERE region = 'east' GROUP BY date",
    }),
    evaluated_permission: "allow",
  }),
  ev({
    id: "sevt-0025",
    type: "agent.mcp_tool_result",
    processed_at: at(45),
    session_thread_id: SUB,
    mcp_tool_use_id: "toolu_mcp_02",
    is_error: true,
    content: [
      {
        type: "text",
        text: "SemanticException: Table 'dwd_order_detail_2025' not found. 历史数据请查 dwd_order_detail 并带 date 分区过滤。",
      },
    ],
  }),
  // ⚠️ **Starting at 45 = exactly when mcp_02 ends** (it is [43,45]).
  // This is the "touching endpoints" edge case: two spans **meet end to start without
  // overlapping**, one after the other rather than concurrent. This shape pins that the
  // concurrency check uses **strict** inequality (`a<b'` && `a'<b`); with `<=` this pair would be
  // wrongly judged concurrent, and that error would not show anywhere else.
  ev({
    id: "sevt-0026",
    type: "agent.mcp_tool_use",
    processed_at: at(45),
    session_thread_id: SUB,
    tool_use_id: "toolu_mcp_03",
    mcp_server_name: "data-warehouse",
    name: "run_query",
    input: JSON.stringify({
      sql: "SELECT date, SUM(pay_amount) AS gmv FROM dwd_order_detail o JOIN dim_merchant m ON o.merchant_id = m.id WHERE m.region='east' AND m.is_test=0 AND date BETWEEN '2025-08-18' AND '2025-08-24' GROUP BY date ORDER BY date",
    }),
    evaluated_permission: "allow",
  }),
  ev({
    id: "sevt-0027",
    type: "agent.mcp_tool_result",
    processed_at: at(49),
    session_thread_id: SUB,
    mcp_tool_use_id: "toolu_mcp_03",
    content: [
      {
        type: "text",
        text: "date | gmv\n2025-08-18 | 10233871.20\n2025-08-19 | 10884120.44\n2025-08-20 | 10790233.90\n2025-08-21 | 11002448.15\n2025-08-22 | 11845690.02\n2025-08-23 | 8422019.77\n2025-08-24 | 7988301.36\n\n(7 rows, 2.04s)",
      },
    ],
  }),
  ev({
    id: "sevt-0028",
    type: "agent.thread_message_sent",
    processed_at: at(50),
    session_thread_id: SUB,
    to_session_thread_id: MAIN,
    content: [
      {
        type: "text",
        text: "查询完成。本周华东 GMV 合计 8527.6 万，前一周 8102.3 万（环比 +5.2%），去年同期 7116.7 万（同比 +19.8%）。注意 8/22–8/23 两天明显低于前五日。",
      },
    ],
  }),
  ev({
    id: "sevt-0029",
    type: "session.thread_status_terminated",
    processed_at: at(50),
    session_thread_id: SUB,
    agent_name: "SQL 执行子智能体",
  }),
  ev({
    id: "sevt-0030",
    type: "agent.thread_message_received",
    processed_at: at(51),
    session_thread_id: MAIN,
    from_session_thread_id: SUB,
    from_agent_name: "SQL 执行子智能体",
    content: [
      {
        type: "text",
        text: "查询完成。本周华东 GMV 合计 8527.6 万，前一周 8102.3 万（环比 +5.2%），去年同期 7116.7 万（同比 +19.8%）。注意 8/22–8/23 两天明显低于前五日。",
      },
    ],
  }),
  // Context compaction
  ev({
    id: "sevt-0031",
    type: "agent.thread_context_compacted",
    processed_at: at(52),
    session_thread_id: MAIN,
    summary:
      "已压缩前 18 轮工具调用记录，保留：GMV 口径定义、本周/上周/去年同期三组日粒度数据、8/22–8/23 异常线索。",
  }),
  // ── Concurrent calls: **two tools started at the same moment**, one returns first, one later. ──────────────
  //
  // This pair is for the call tree's **timeline axis**: duration alone cannot tell "concurrent
  // or serial"; both ends [start, end] must be shown to tell. Real production data has exactly
  // this shape (measured: in one run, two bash calls started 16 microseconds apart and ended 0.5
  // seconds apart).
  //
  // ⚠️ **The processed_at of the two tool_use events must be equal or very close**, otherwise
  // they are offset on the axis and this seed no longer covers the "overlap" branch.
  ev({
    id: "sevt-0032a",
    type: "agent.tool_use",
    processed_at: at(53),
    session_thread_id: MAIN,
    tool_use_id: "toolu_par_01",
    name: "bash",
    input: JSON.stringify({ command: "python3 scripts/export_gmv.py --week 34" }),
    evaluated_permission: "allow",
  }),
  ev({
    id: "sevt-0032b",
    type: "agent.tool_use",
    processed_at: at(53),
    session_thread_id: MAIN,
    tool_use_id: "toolu_par_02",
    name: "bash",
    input: JSON.stringify({ command: "python3 scripts/export_refund.py --week 34" }),
    evaluated_permission: "allow",
  }),
  ev({
    id: "sevt-0032c",
    type: "agent.tool_result",
    processed_at: at(61),
    session_thread_id: MAIN,
    tool_use_id: "toolu_par_01",
    content: [{ type: "text", text: "已导出 /workspace/out/gmv_w34.csv（7 行）" }],
  }),
  // The later one ends 3 seconds later, so the two overlap on the axis but have different lengths.
  ev({
    id: "sevt-0032d",
    type: "agent.tool_result",
    processed_at: at(64),
    session_thread_id: MAIN,
    tool_use_id: "toolu_par_02",
    content: [{ type: "text", text: "已导出 /workspace/out/refund_w34.csv（7 行）" }],
  }),
  // Web search for the cause of the anomaly
  ev({
    id: "sevt-0032",
    type: "agent.tool_use",
    processed_at: at(53),
    session_thread_id: MAIN,
    tool_use_id: "toolu_search_01",
    name: "web_search",
    input: JSON.stringify({ query: "2026年8月22日 华东 台风 物流影响" }),
    evaluated_permission: "allow",
  }),
  ev({
    id: "sevt-0033",
    type: "agent.tool_result",
    processed_at: at(56),
    session_thread_id: MAIN,
    tool_use_id: "toolu_search_01",
    content: [
      {
        type: "search_result",
        source: "https://weather.example.com/news/20260822",
        title: "台风「海棠」8月22日登陆浙江，江浙沪快递大面积延误",
        content: [
          {
            type: "text",
            text: "8月22日至23日，受台风影响，江浙沪多地快递网点暂停揽收，电商物流时效普遍延后 1–2 天。",
          },
        ],
        citations: { enabled: true },
      },
    ],
  }),
  // ── Tools called **inside a model request span** ────────────────────────────────────
  //
  // Real Ark has exactly this shape (measured on one production run):
  //     seq 31  span.model_request_start   13.1s   ← the model starts first
  //     seq 33  agent.tool_use             17.0s   ← the tool is called in the middle
  //     seq 35  span.model_request_end     17.0s   ← then the model ends
  //
  // ⚠️ **This pair is for the "row order by start time" test case.** The model span's `item`
  // is the `_end` event, so its sequence is **larger** than the tool in the middle; sorting by
  // sequence would put the model after the tool, while its bar is drawn to the left of the tool's;
  // on the page this looks like "the bar on the upper row is further right". Without this shape
  // that bug cannot be reproduced on seed data, and the test case would be idle.
  ev({
    id: "sevt-0034",
    type: "span.model_request_start",
    processed_at: at(57),
    session_thread_id: MAIN,
  }),
  ev({
    id: "sevt-0034a",
    type: "agent.tool_use",
    processed_at: at(59),
    session_thread_id: MAIN,
    tool_use_id: "toolu_mid_01",
    name: "read",
    input: JSON.stringify({ file_path: "/workspace/out/refund_w34.csv" }),
    evaluated_permission: "allow",
  }),
  ev({
    id: "sevt-0034b",
    type: "agent.tool_result",
    processed_at: at(60),
    session_thread_id: MAIN,
    tool_use_id: "toolu_mid_01",
    content: [{ type: "text", text: "date,refund_amount\n2026-08-22,182300.11" }],
  }),
  ev({
    id: "sevt-0035",
    type: "span.model_request_end",
    processed_at: at(64),
    session_thread_id: MAIN,
    model_request_start_id: "sevt-0034",
    model_usage: {
      input_tokens: 9932,
      output_tokens: 806,
      cache_read_input_tokens: 8104,
      cache_creation_input_tokens: 1828,
      speed: "fast",
    },
  }),
  ev({
    id: "sevt-0036",
    type: "agent.message",
    processed_at: at(64),
    session_thread_id: MAIN,
    content: [
      {
        type: "text",
        text: `## 结论

上周（8/17–8/23）华东区 GMV **8527.6 万**，环比 **+5.2%**，同比 **+19.8%**，整体健康。

**但 8/22–8/23 两天掉得厉害**：日均从前五日的 1325 万降到 950 万，跌幅 28%。原因是台风「海棠」8/22 登陆浙江，江浙沪快递大面积暂停揽收，不是需求端问题。

## 数据

| 日期 | GMV（万） | 去年同期（万） |
| --- | --- | --- |
| 8/17 | 1248.0 | 1023.4 |
| 8/18 | 1310.3 | 1088.4 |
| 8/19 | 1299.5 | 1079.0 |
| 8/20 | 1348.8 | 1100.2 |
| 8/21 | 1420.2 | 1184.6 |
| 8/22 | 987.7 | 842.2 |
| 8/23 | 912.1 | 798.8 |

## 建议

台风属一次性外部因素，不建议据此调整投放预算。可关注 8/24 之后是否有补偿性回补 —— 如果没有，说明这部分需求是真流失而非延后。`,
      },
    ],
  }),
  ev({
    id: "sevt-0037",
    type: "session.thread_status_idle",
    processed_at: at(65),
    session_thread_id: MAIN,
    agent_name: "数据分析师",
    stop_reason: { type: "end_turn" },
  }),
  ev({
    id: "sevt-0038",
    type: "session.status_idle",
    processed_at: at(65),
    session_thread_id: null,
    stop_reason: { type: "end_turn" },
  }),
];

// ---------------------------------------------------------------------------
// Run 2: ticket triage with an outcome evaluation loop
// ---------------------------------------------------------------------------

const T2 = "sth_triage_01";

const run2Events: RunEvent[] = [
  ev({
    id: "sevt-0101",
    type: "session.status_running",
    processed_at: at(3600),
    session_thread_id: null,
  }),
  ev({
    id: "sevt-0102",
    type: "session.thread_created",
    processed_at: at(3600),
    session_thread_id: T2,
    agent_name: "工单分诊助手",
    parent_thread_id: null,
  }),
  ev({
    id: "sevt-0103",
    type: "user.message",
    processed_at: at(3601),
    session_thread_id: T2,
    content: [
      {
        type: "text",
        text: "有商家反馈昨天下午开始结算页面打不开，点进去一直转圈。已经有 30 多个商家在群里问了。",
      },
      {
        type: "image",
        source: { type: "url", url: "https://internal.example.com/uploads/screenshot-8f2a.png" },
      },
    ],
  }),
  ev({
    id: "sevt-0104",
    type: "user.define_outcome",
    processed_at: at(3601),
    session_thread_id: T2,
    outcome_id: "outc-20260825-001",
    description: "产出一份可直接转派的分诊结论",
    rubric: {
      type: "text",
      content:
        "必须包含：严重级别（P0-P3）、业务模块、判定依据、建议处理人。判定依据要能被第三方验证，不能只有结论。",
    },
    max_iterations: 3,
  }),
  ev({
    id: "sevt-0105",
    type: "session.thread_status_running",
    processed_at: at(3602),
    session_thread_id: T2,
    agent_name: "工单分诊助手",
  }),
  ev({
    id: "sevt-0106",
    type: "agent.thinking",
    processed_at: at(3603),
    session_thread_id: T2,
  }),
  ev({
    id: "sevt-0107",
    type: "span.model_request_start",
    processed_at: at(3603),
    session_thread_id: T2,
  }),
  ev({
    id: "sevt-0108",
    type: "span.model_request_end",
    processed_at: at(3611),
    session_thread_id: T2,
    model_request_start_id: "sevt-0107",
    model_usage: {
      input_tokens: 3211,
      output_tokens: 264,
      cache_read_input_tokens: 2560,
      cache_creation_input_tokens: 651,
      speed: "standard",
    },
  }),
  ev({
    id: "sevt-0109",
    type: "agent.tool_use",
    processed_at: at(3612),
    session_thread_id: T2,
    tool_use_id: "toolu_grep_01",
    name: "grep",
    input: JSON.stringify({
      pattern: "settlement.*(timeout|500)",
      path: "/logs/gateway/2026-08-24",
    }),
    evaluated_permission: "allow",
  }),
  ev({
    id: "sevt-0110",
    type: "agent.tool_result",
    processed_at: at(3614),
    session_thread_id: T2,
    tool_use_id: "toolu_grep_01",
    content: [
      {
        type: "text",
        text: "/logs/gateway/2026-08-24/access.log:88213: GET /api/settlement/summary 504 upstream timeout (14.9s)\n/logs/gateway/2026-08-24/access.log:88547: GET /api/settlement/summary 504 upstream timeout (15.0s)\n… 共 4127 条匹配，集中在 14:10 之后",
      },
    ],
  }),
  ev({
    id: "sevt-0111",
    type: "agent.message",
    processed_at: at(3620),
    session_thread_id: T2,
    content: [
      {
        type: "text",
        text: "初步判定：P1，结算模块，`/api/settlement/summary` 接口 504 超时。建议转派结算后端。",
      },
    ],
  }),
  // First evaluation round: not good enough
  ev({
    id: "sevt-0112",
    type: "span.outcome_evaluation_start",
    processed_at: at(3621),
    session_thread_id: T2,
    outcome_id: "outc-20260825-001",
    iteration: 1,
  }),
  ev({
    id: "sevt-0113",
    type: "span.outcome_evaluation_ongoing",
    processed_at: at(3624),
    session_thread_id: T2,
    outcome_id: "outc-20260825-001",
    iteration: 1,
  }),
  ev({
    id: "sevt-0114",
    type: "span.outcome_evaluation_end",
    processed_at: at(3628),
    session_thread_id: T2,
    outcome_id: "outc-20260825-001",
    outcome_evaluation_start_id: "sevt-0112",
    iteration: 1,
    result: "needs_revision",
    explanation:
      "判定依据不充分：只说了接口 504，没有给出影响面（多少商家、多少请求）与起始时间，接手人无法评估紧急度。建议处理人也过于笼统。",
    usage: {
      input_tokens: 1840,
      output_tokens: 132,
      cache_read_input_tokens: 1520,
      cache_creation_input_tokens: 320,
      speed: "standard",
    },
  }),
  // Second round: passes after filling the gaps
  ev({
    id: "sevt-0115",
    type: "span.outcome_evaluation_start",
    processed_at: at(3629),
    session_thread_id: T2,
    outcome_id: "outc-20260825-001",
    iteration: 2,
  }),
  ev({
    id: "sevt-0116",
    type: "agent.message",
    processed_at: at(3640),
    session_thread_id: T2,
    content: [
      {
        type: "text",
        text: `## 分诊结论

**级别**：P1
**模块**：结算
**建议处理人**：结算后端 @李维（值班），同步告知商家运营准备对外话术

### 判定依据

1. 网关日志 \`/logs/gateway/2026-08-24/access.log\` 中 \`/api/settlement/summary\` 返回 504 共 **4127 条**，全部集中在 **14:10 之后**，与商家反馈的「昨天下午开始」吻合。
2. 超时时长稳定在 14.9–15.0s，等于网关上游超时阈值，说明是后端处理不完而非网络抖动。
3. 影响面：群内 30+ 商家主动反馈，日志显示受影响请求 4127 条，覆盖商家数需结算侧确认，但已可判定为核心链路受损。

### 为什么不是 P0

结算页面不可用影响商家查账，但**不阻塞交易主链路**（下单、支付正常），故定 P1 而非 P0。`,
      },
    ],
  }),
  ev({
    id: "sevt-0117",
    type: "span.outcome_evaluation_end",
    processed_at: at(3648),
    session_thread_id: T2,
    outcome_id: "outc-20260825-001",
    outcome_evaluation_start_id: "sevt-0115",
    iteration: 2,
    result: "satisfied",
    explanation: "四要素齐全，判定依据可被第三方通过日志验证，并解释了级别边界。达标。",
    usage: {
      input_tokens: 2410,
      output_tokens: 96,
      cache_read_input_tokens: 2048,
      cache_creation_input_tokens: 362,
      speed: "standard",
    },
  }),
  ev({
    id: "sevt-0118",
    type: "session.thread_status_idle",
    processed_at: at(3649),
    session_thread_id: T2,
    agent_name: "工单分诊助手",
    stop_reason: { type: "end_turn" },
  }),
  ev({
    id: "sevt-0119",
    type: "session.status_idle",
    processed_at: at(3649),
    session_thread_id: null,
    stop_reason: { type: "end_turn" },
  }),
];

// ---------------------------------------------------------------------------
// Run 3: terminated by an unrecoverable error
// ---------------------------------------------------------------------------

const T3 = "sth_review_01";

const run3Events: RunEvent[] = [
  ev({
    id: "sevt-0201",
    type: "session.status_running",
    processed_at: at(7200),
    session_thread_id: null,
  }),
  ev({
    id: "sevt-0202",
    type: "session.thread_created",
    processed_at: at(7200),
    session_thread_id: T3,
    agent_name: "代码审查员",
    parent_thread_id: null,
  }),
  ev({
    id: "sevt-0203",
    type: "user.message",
    processed_at: at(7201),
    session_thread_id: T3,
    content: [{ type: "text", text: "审一下 MR !3821，重点看并发安全。" }],
  }),
  ev({
    id: "sevt-0204",
    type: "session.thread_status_running",
    processed_at: at(7202),
    session_thread_id: T3,
    agent_name: "代码审查员",
  }),
  ev({
    id: "sevt-0205",
    type: "agent.mcp_tool_use",
    processed_at: at(7203),
    session_thread_id: T3,
    tool_use_id: "toolu_mcp_mr_01",
    mcp_server_name: "gitlab",
    name: "get_merge_request_diff",
    input: JSON.stringify({ project: "growth/settlement", mr_iid: 3821 }),
    evaluated_permission: "allow",
  }),
  ev({
    id: "sevt-0206",
    type: "session.error",
    processed_at: at(7208),
    session_thread_id: T3,
    error: {
      type: "mcp_auth_failed_error",
      message:
        "MCP 服务 gitlab 认证失败：凭证已过期（401 Unauthorized）。请在保管库中更新该凭证。",
      retry_status: { type: "retries_exhausted" },
      mcp_server_name: "gitlab",
      vault_id: "vault_growth_01",
      credential_id: "cred_gitlab_pat",
    },
  }),
  ev({
    id: "sevt-0207",
    type: "session.thread_status_terminated",
    processed_at: at(7209),
    session_thread_id: T3,
    agent_name: "代码审查员",
  }),
  ev({
    id: "sevt-0208",
    type: "session.status_terminated",
    processed_at: at(7209),
    session_thread_id: null,
    stop_reason: { type: "retries_exhausted" },
  }),
];

// ---------------------------------------------------------------------------
// Run 4: still running (to verify how the running state is shown)
// ---------------------------------------------------------------------------

const T4 = "sth_live_01";

const run4Events: RunEvent[] = [
  ev({
    id: "sevt-0301",
    type: "session.status_running",
    processed_at: at(10800),
    session_thread_id: null,
  }),
  ev({
    id: "sevt-0302",
    type: "session.thread_created",
    processed_at: at(10800),
    session_thread_id: T4,
    agent_name: "数据分析师",
    parent_thread_id: null,
  }),
  ev({
    id: "sevt-0303",
    type: "user.message",
    processed_at: at(10801),
    session_thread_id: T4,
    content: [{ type: "text", text: "跑一下这周的商家留存，按新老商家拆开看。" }],
  }),
  ev({
    id: "sevt-0304",
    type: "agent.thinking",
    processed_at: at(10803),
    session_thread_id: T4,
  }),
  ev({
    id: "sevt-0305",
    type: "agent.mcp_tool_use",
    processed_at: at(10805),
    session_thread_id: T4,
    tool_use_id: "toolu_mcp_live_01",
    mcp_server_name: "data-warehouse",
    name: "run_query",
    input: JSON.stringify({
      sql: "SELECT cohort, retention_d7 FROM dws_merchant_retention WHERE week = '2026W34'",
    }),
    evaluated_permission: "allow",
  }),
  // Deliberately no result: this run is still in progress, to verify the "waiting for result" UI
  //
  // The two idle events below are **the real shape of every tool call under the self_hosted
  // architecture**: Ark only does model inference, tools are put on the work queue and picked up
  // by amad's worker, so on the Ark side there is always tool_use first, then idle with
  // `stop_reason: requires_action` (meaning "control is handed back to you, waiting for
  // `user.tool_result`"). One at the thread level and one at the session level, so two.
  //
  // ⚠ **This is not "waiting for user confirmation"** (this call's evaluated_permission is allow;
  // nobody needs to click anything). The frontend used to render each as "thread idle / session
  // idle · waiting for user confirmation", so during the longest part of a tool run the user saw
  // "it already stopped, and I need to click something"; neither was true. Now they are merged
  // into the call above and shown as "running"; see TimelineItem.executing in lib/timeline.ts.
  // The seed keeps this shape so that path has something to test.
  ev({
    id: "sevt-0306",
    type: "session.thread_status_idle",
    processed_at: at(10806),
    session_thread_id: T4,
    agent_name: "数据分析师",
    stop_reason: { type: "requires_action", event_ids: ["sevt-0305"] },
  }),
  // The session-level one **deliberately has empty event_ids**, covering the degraded branch
  // "cannot attach to a specific call". This shape is not made up: `event_ids` is only a hint from
  // Ark, and **the backend does not look at it at all when deciding "is it finished"**
  // (`server/feishu/inbound.go:931` and `server/core/scheduler.go:602` only check
  // `stop_reason.type`). The frontend cannot rely on it being non-empty either: when it cannot
  // attach, it must still show a "control handed back" entry (swallowing it would lose
  // information), and it **still must not treat this turn as finished**; that gate is `endsTurn()`
  // in `lib/timeline.ts`. Without this seed, that gate would be dead code no test reaches.
  ev({
    id: "sevt-0307",
    type: "session.status_idle",
    processed_at: at(10806),
    session_thread_id: null,
    stop_reason: { type: "requires_action", event_ids: [] },
  }),
  ev({
    id: "sevt-0308",
    type: "span.model_request_start",
    processed_at: null,
    session_thread_id: T4,
  }),
];

// ---------------------------------------------------------------------------
// Run summaries
// ---------------------------------------------------------------------------

const runs: Run[] = [
  {
    id: "run_1",
    workspaceId: WS,
    agentId: "agent_analyst",
    agentName: "数据分析师",
    title: "华东区上周 GMV 表现分析",
    status: "idle",
    trigger: "feishu",
    triggerDetail: "增长数据日常",
    startedAt: at(0),
    endedAt: at(65),
    durationMs: 65_000,
    usage: {
      input_tokens: 14_752,
      output_tokens: 1_118,
      cache_read_input_tokens: 12_072,
      cache_creation_input_tokens: 2_680,
    },
    threads: [
      {
        id: MAIN,
        agent_name: "数据分析师",
        status: "idle",
        parent_thread_id: null,
        startedAt: at(0),
        endedAt: at(65),
      },
      {
        id: SUB,
        agent_name: "SQL 执行子智能体",
        status: "terminated",
        parent_thread_id: MAIN,
        startedAt: at(12),
        endedAt: at(50),
      },
    ],
    eventCount: run1Events.length,
  },
  {
    id: "run_2",
    workspaceId: WS,
    agentId: "agent_triage",
    agentName: "工单分诊助手",
    title: "结算页面无法打开（30+ 商家反馈）",
    status: "idle",
    trigger: "feishu",
    triggerDetail: "增长客服值班群",
    startedAt: at(3600),
    endedAt: at(3649),
    durationMs: 49_000,
    usage: {
      input_tokens: 7_461,
      output_tokens: 492,
      cache_read_input_tokens: 6_128,
      cache_creation_input_tokens: 1_333,
    },
    threads: [
      {
        id: T2,
        agent_name: "工单分诊助手",
        status: "idle",
        parent_thread_id: null,
        startedAt: at(3600),
        endedAt: at(3649),
      },
    ],
    eventCount: run2Events.length,
  },
  {
    id: "run_3",
    workspaceId: WS,
    agentId: "agent_reviewer",
    agentName: "代码审查员",
    title: "审查 MR !3821 并发安全",
    status: "terminated",
    trigger: "api",
    triggerDetail: "ci-bot",
    startedAt: at(7200),
    endedAt: at(7209),
    durationMs: 9_000,
    usage: {
      input_tokens: 1_204,
      output_tokens: 86,
      cache_read_input_tokens: 0,
      cache_creation_input_tokens: 1_204,
    },
    threads: [
      {
        id: T3,
        agent_name: "代码审查员",
        status: "terminated",
        parent_thread_id: null,
        startedAt: at(7200),
        endedAt: at(7209),
      },
    ],
    eventCount: run3Events.length,
    errorSummary: "MCP 服务 gitlab 凭证过期，重试耗尽后终止",
  },
  {
    id: "run_4",
    workspaceId: WS,
    agentId: "agent_analyst",
    agentName: "数据分析师",
    title: "本周商家留存（新老拆分）",
    status: "running",
    trigger: "manual",
    triggerDetail: "张三",
    startedAt: at(10800),
    usage: {
      input_tokens: 2_048,
      output_tokens: 0,
      cache_read_input_tokens: 1_536,
      cache_creation_input_tokens: 512,
    },
    threads: [
      {
        id: T4,
        agent_name: "数据分析师",
        status: "running",
        parent_thread_id: null,
        startedAt: at(10800),
      },
    ],
    eventCount: run4Events.length,
  },
  {
    id: "run_5",
    workspaceId: WS,
    agentId: "agent_triage",
    agentName: "工单分诊助手",
    title: "投放后台数据延迟",
    status: "requires_action",
    trigger: "feishu",
    triggerDetail: "增长客服值班群",
    startedAt: at(12_600),
    usage: {
      input_tokens: 3_120,
      output_tokens: 210,
      cache_read_input_tokens: 2_560,
      cache_creation_input_tokens: 560,
    },
    threads: [
      {
        id: "sth_pending_01",
        agent_name: "工单分诊助手",
        status: "idle",
        parent_thread_id: null,
        startedAt: at(12_600),
      },
    ],
    eventCount: 6,
  },
  /*
   * run_7 / run_8: samples needed for **status display in the conversation list**; see
   * components/ChatSessions.tsx.
   *
   * Why they are needed: the chat page's left column only takes `CONVERSATION_TRIGGERS` (manual +
   * feishu), and before this **the only run with an errorSummary was run_3, which is `api`
   * triggered**; so the "failed" level could not render in the conversation list at all, and e2e
   * had nothing to test with. And failure is exactly the state that should be seen at a glance: a
   * failed session's `status` is also `terminated`, and without checking errorSummary separately
   * it would show as "ended".
   *
   * Same for `rescheduling`: of the five status levels it had no sample at all (run_1..run_6 use
   * idle / terminated / running / requires_action).
   *
   * Both are placed before run_6 (earlier startedAt); **run_6 is still the latest**: `06-journey`
   * asserts that the first item in the overview is run_6, and moving it would fail that case with
   * "element not found".
   */
  {
    id: "run_7",
    workspaceId: WS,
    agentId: "agent_analyst",
    agentName: "数据分析师",
    /*
     * The title is **deliberately long**: the narrow-screen drawer is only 264px, and the status
     * chip relies on the title's `truncate` to give way. A short title does not fill the row, so
     * removing `min-w-0 flex-1 truncate` would still not overflow, and the "chip is not pushed
     * out" e2e case would pass falsely (measured: with truncate removed, the case still passed).
     * This long title is that case's stress source; shortening it quietly turns the guard off.
     */
    title: "对账脚本跑不动，昨晚那批商家结算数据到现在还没出，帮我看下是不是上游表又延迟了",
    // ⚠️ terminated rather than some "failed" state: **RunStatus has no failed level**.
    // The check is a non-empty errorSummary, the same rule as UsageSummary / OverviewPage.
    status: "terminated",
    trigger: "manual",
    triggerDetail: "张三",
    startedAt: at(15_000),
    endedAt: at(15_034),
    durationMs: 34_000,
    usage: {
      input_tokens: 4_302,
      output_tokens: 265,
      cache_read_input_tokens: 3_584,
      cache_creation_input_tokens: 718,
    },
    threads: [
      {
        id: "sth_fail_01",
        agent_name: "数据分析师",
        status: "terminated",
        parent_thread_id: null,
        startedAt: at(15_000),
        endedAt: at(15_034),
      },
    ],
    eventCount: 0,
    errorSummary: "容器执行超时：bash 工具 120 秒未返回，本轮终止",
  },
  {
    id: "run_8",
    workspaceId: WS,
    agentId: "agent_triage",
    agentName: "工单分诊助手",
    title: "值班群夜间告警汇总",
    status: "rescheduling",
    trigger: "feishu",
    triggerDetail: "增长客服值班群",
    startedAt: at(16_000),
    usage: {
      input_tokens: 1_106,
      output_tokens: 48,
      cache_read_input_tokens: 1_024,
      cache_creation_input_tokens: 82,
    },
    threads: [
      {
        id: "sth_resched_01",
        agent_name: "工单分诊助手",
        status: "rescheduling",
        parent_thread_id: null,
        startedAt: at(16_000),
      },
    ],
    eventCount: 0,
  },
  /*
   * run_9: a session that **finished answering but whose closing event was never stored**; see
   * the long comment on run9Events.
   *
   * ⚠️ `status: "idle"` and its event stream are **deliberately inconsistent**: the last event is
   * an idle with `requires_action`, while the run itself finished long ago. That is exactly the
   * shape of the failing production session (the catch-up fetch gate blocked the trailing
   * events), and the only test material for the fallback in `groupTurns(items, runStatus)`;
   * changing status to requires_action would turn the guard off.
   *
   * Like run_7 / run_8: placed before run_6, and run_6 is still the latest in run history.
   */
  {
    id: "run_9",
    workspaceId: WS,
    agentId: "agent_triage",
    agentName: "工单分诊助手",
    title: "昨天超时订单的时段分布",
    // The backend run has finished (idle), but the event stream has no end_turn.
    status: "idle",
    trigger: "manual",
    triggerDetail: "张三",
    startedAt: at(17_000),
    endedAt: at(17_012),
    durationMs: 12_000,
    usage: {
      input_tokens: 1_820,
      output_tokens: 96,
      cache_read_input_tokens: 1_536,
      cache_creation_input_tokens: 284,
    },
    threads: [
      {
        id: "sth_orphan_01",
        agent_name: "工单分诊助手",
        status: "idle",
        parent_thread_id: null,
        startedAt: at(17_000),
        endedAt: at(17_012),
      },
    ],
    // Hard-coded instead of run9Events.length: the event array is defined after runs (same as run_6's 20).
    eventCount: 9,
  },
  /*
   * run_6: the debugging sample covering every event type; see the comment on run6Events.
   * Placed last with the latest startedAt, so it is first in run history; no scrolling needed to
   * see all events.
   */
  {
    id: "run_6",
    workspaceId: WS,
    agentId: "agent_triage",
    agentName: "工单分诊助手",
    title: "事件类型全覆盖自检",
    // The last event is session.deleted, so the session is over; terminated is the matching status.
    status: "terminated",
    trigger: "manual",
    startedAt: at(20_000),
    durationMs: 20_000,
    usage: {
      input_tokens: 1_820,
      output_tokens: 96,
      cache_read_input_tokens: 1_536,
      cache_creation_input_tokens: 128,
    },
    threads: [
      {
        id: "sth_full_01",
        agent_name: "全量事件探针",
        status: "idle",
        parent_thread_id: null,
        startedAt: at(20_000),
      },
    ],
    eventCount: 20,
  },
];

const run5Events: RunEvent[] = [
  ev({
    id: "sevt-0401",
    type: "session.status_running",
    processed_at: at(12_600),
    session_thread_id: null,
  }),
  ev({
    id: "sevt-0402",
    type: "session.thread_created",
    processed_at: at(12_600),
    session_thread_id: "sth_pending_01",
    agent_name: "工单分诊助手",
    parent_thread_id: null,
  }),
  ev({
    id: "sevt-0403",
    type: "user.message",
    processed_at: at(12_601),
    session_thread_id: "sth_pending_01",
    content: [
      { type: "text", text: "投放后台的实时数据好像延迟了快一个小时，是不是又出问题了？" },
    ],
  }),
  ev({
    id: "sevt-0404",
    type: "agent.tool_use",
    processed_at: at(12_604),
    session_thread_id: "sth_pending_01",
    tool_use_id: "toolu_bash_pending",
    name: "bash",
    input: JSON.stringify({
      command: "curl -s http://monitor.internal/api/lag?topic=ad_realtime | jq .",
    }),
    evaluated_permission: "ask",
  }),
  ev({
    id: "sevt-0405",
    type: "session.thread_status_idle",
    processed_at: at(12_605),
    session_thread_id: "sth_pending_01",
    agent_name: "工单分诊助手",
    stop_reason: { type: "requires_action", event_ids: ["sevt-0404"] },
  }),
  ev({
    id: "sevt-0406",
    type: "session.status_idle",
    processed_at: at(12_605),
    session_thread_id: null,
    stop_reason: { type: "requires_action", event_ids: ["sevt-0404"] },
  }),
];

/** Thread for run_9. */
const T9 = "sth_orphan_01";

/**
 * run_9: a session that **finished answering, but whose closing event was never stored**.
 *
 * # This seed guards against a real production bug
 *
 * In production, one conversation turn had long finished (with a complete answer), yet the
 * intermediate steps stayed expanded and never collapsed. The root cause was not the collapse
 * logic (`endsTurn()`'s check is correct, matching `turnEnded` in the backend's `scheduler.go`
 * exactly); it was that **the closing event never reached the frontend**.
 *
 * There were two reasons the event stream was missing its closing event, both on the server and
 * both silent:
 *
 *   1. **The catch-up fetch only accepted `running`** (`ListRunEvents` in `core/ark.go`, fixed
 *      along with this). Under self_hosted, every tool call turns the run into
 *      `requires_action`, so "SSE dropped while a tool was running", the most common break
 *      point, was exactly what that gate blocked, and the trailing events could never be fetched.
 *   2. **`retireMissingRun` only changes the status and appends no event**
 *      (`core/runwatcher.go`). After the Ark-side session is reclaimed, the run is set to
 *      `terminated`, but the event stream has no `session.status_terminated`.
 *
 * # Shape: **deliberately stops at an idle with `requires_action`**, with nothing after
 *
 * That is exactly how it looks at the moment the stream broke: the tool result came back and the
 * agent finished its answer, but the final `session.status_idle (end_turn)` was never stored.
 *
 * ⚠️ **Do not "helpfully add" that end_turn event**: with it, this seed degrades into run_1, and
 * the fallback it guards (the runStatus parameter of `groupTurns`) becomes dead code no test
 * reaches. This run's `status` is `idle` (the backend run finished long ago), and the frontend
 * relies on it to collapse the last turn.
 */
const run9Events: RunEvent[] = [
  ev({
    id: "sevt-0901",
    type: "session.status_running",
    processed_at: at(17_000),
    session_thread_id: null,
  }),
  ev({
    id: "sevt-0902",
    type: "session.thread_created",
    processed_at: at(17_000),
    session_thread_id: T9,
    agent_name: "工单分诊助手",
    parent_thread_id: null,
  }),
  ev({
    id: "sevt-0903",
    type: "user.message",
    processed_at: at(17_001),
    session_thread_id: T9,
    content: [{ type: "text", text: "帮我查下昨天那批超时订单的分布" }],
  }),
  ev({
    id: "sevt-0904",
    type: "agent.thinking",
    processed_at: at(17_002),
    session_thread_id: T9,
    content: [{ type: "text", text: "先按小时聚合看看峰值落在哪一段。" }],
  }),
  ev({
    id: "sevt-0905",
    type: "agent.tool_use",
    processed_at: at(17_003),
    session_thread_id: T9,
    name: "bash",
    input: { command: "psql -c 'select hour, count(*) from orders_timeout group by 1'" },
    evaluated_permission: "allow",
  }),
  // The tool hands back control. **This does not count as finished** (requires_action), same shape as run_4.
  ev({
    id: "sevt-0906",
    type: "session.status_idle",
    processed_at: at(17_003),
    session_thread_id: null,
    stop_reason: { type: "requires_action", event_ids: ["sevt-0905"] },
  }),
  ev({
    id: "sevt-0907",
    type: "user.tool_result",
    processed_at: at(17_009),
    session_thread_id: T9,
    tool_use_id: "sevt-0905",
    content: [{ type: "text", text: "09:00 | 12\n10:00 | 47\n11:00 | 213\n12:00 | 38" }],
    is_error: false,
  }),
  ev({
    id: "sevt-0908",
    type: "session.status_running",
    processed_at: at(17_010),
    session_thread_id: null,
  }),
  // The answer is given: in the user's eyes this turn **has ended**.
  ev({
    id: "sevt-0909",
    type: "agent.message",
    processed_at: at(17_012),
    session_thread_id: T9,
    content: [
      {
        type: "text",
        text: "超时集中在 **11:00–12:00**，那一小时 213 单，是相邻时段的 4~5 倍。建议先查该时段上游批处理任务的排队情况。",
      },
    ],
  }),
  // ⚠️ It ends here. In the real bug, that `session.status_idle (end_turn)` was simply missing.
];

/**
 * run_6: a debugging run that **covers all 34 event types**.
 *
 * It exists because the event view's promise is "show all 33 types", while the first five runs
 * together only contain 27. The other 7 (`user.interrupt` / `user.tool_result` /
 * `user.custom_tool_result` / `agent.custom_tool_use` / `session.status_rescheduled` /
 * `session.updated` / `session.deleted`) **never render once** in local mode, so their detail
 * rendering is code that was "written but never run".
 *
 * Two more things only this run can test:
 *
 * 1. **Same-millisecond ordering**: sevt-0603/0604/0605 have `processed_at` in the same
 *    millisecond (different nanoseconds, but JS only has milliseconds), so only `sequence` can
 *    order them.
 * 2. **Mixed time zone forms**: this run uses `+08:00`, others use literal `Z`; comparing as
 *    strings gives the wrong order, so they must be parsed into absolute instants first.
 */
/**
 * The three **same-millisecond** events in run_6 are **deliberately out of order in the array**
 * (0605 → 0603 → 0604), while their sequence numbers are reserved from the global counter in
 * logical order (0603 → 0604 → 0605).
 *
 * # Why they must be out of order
 *
 * `Array.prototype.sort` is **stable**, and `ev()` assigns increasing sequence by array position.
 * Together, "array order" happens to equal "sequence order", so changing the sort's
 * `return a.sequence - b.sequence` to `return 0` **still passes every test** (measured in the
 * first round of mutation testing). That assertion was always true; it looked covered but
 * verified nothing.
 *
 * Only when array order differs from sequence order does the same-millisecond fallback really
 * run. This is the same kind of problem as "a small table cannot reveal a missing order by":
 * **seed data that lacks the shape to verify is more hidden than a wrong assertion**.
 */
function shuffledSameMillis(): RunEvent[] {
  // The three numbers are reserved from the same counter; their order is the logical order. Do not hard-code literals; see reserveSeq.
  const [s3, s4, s5] = reserveSeq(3);
  const running: RunEvent = {
    id: "sevt-0605",
    type: "session.thread_status_running",
    processed_at: atNano(20_001, 3, "999999"),
    session_thread_id: "sth_full_01",
    agent_name: "全量事件探针",
    sequence: s5,
  };
  const userMsg: RunEvent = {
    id: "sevt-0603",
    type: "user.message",
    processed_at: atNano(20_001, 3, "111111"),
    session_thread_id: "sth_full_01",
    content: [{ type: "text", text: "把所有事件类型都跑一遍，我要对着排查视图逐条核对。" }],
    sequence: s3,
  };
  const sysMsg: RunEvent = {
    id: "sevt-0604",
    type: "system.message",
    processed_at: atNano(20_001, 3, "555555"),
    session_thread_id: "sth_full_01",
    content: [{ type: "text", text: "当前处于事件覆盖自检模式，允许调用全部内置工具。" }],
    sequence: s4,
  };
  // Return order ≠ sequence order; that is the whole reason this function exists.
  return [running, userMsg, sysMsg];
}

const run6Events: RunEvent[] = [
  ev({
    id: "sevt-0601",
    type: "session.status_running",
    processed_at: atNano(20_000, 0, "100200"),
    session_thread_id: null,
  }),
  ev({
    id: "sevt-0602",
    type: "session.thread_created",
    processed_at: atNano(20_000, 2, "400500"),
    session_thread_id: "sth_full_01",
    agent_name: "全量事件探针",
    parent_thread_id: null,
  }),
  /*
   * The next three share **the same millisecond** (20_001 seconds + 3 ms); nanoseconds increase
   * but JS cannot parse them. Their order can only come from sequence; they are what the
   * "same millisecond falls back to sequence" rule is tested on. Logical order: user question →
   * system prompt → session starts running.
   *
   * ⚠️ **These three are deliberately out of order in the array**; see the comment on
   * shuffledSameMillis().
   */
  ...shuffledSameMillis(),
  ev({
    id: "sevt-0606",
    type: "span.model_request_start",
    processed_at: atNano(20_002, 0, "000000"),
    session_thread_id: "sth_full_01",
  }),
  ev({
    id: "sevt-0607",
    type: "agent.thinking",
    processed_at: atNano(20_003, 120, "246813"),
    session_thread_id: "sth_full_01",
    content: [
      {
        type: "text",
        text: "先列一下要覆盖的类型：\n\n1. 工具类（含 MCP 与自定义）\n2. 线程间通信\n3. 产出评估\n\n**先跑工具。**",
      },
    ],
  }),
  ev({
    id: "sevt-0608",
    type: "span.model_request_end",
    processed_at: atNano(20_006, 500, "864213"),
    session_thread_id: "sth_full_01",
    model_request_start_id: "sevt-0606",
    model_usage: {
      input_tokens: 1_820,
      output_tokens: 96,
      cache_read_input_tokens: 1_536,
      cache_creation_input_tokens: 128,
      speed: "standard",
    },
  }),
  // Tool call: this pair is **not merged into one**; in the event view they are two rows.
  ev({
    id: "sevt-0609",
    type: "agent.tool_use",
    processed_at: atNano(20_007, 0, "135792"),
    session_thread_id: "sth_full_01",
    // Deliberately no tool_use_id: real Ark does not send it, so the pairing key falls back to the event's own id.
    name: "bash",
    input: { command: "ls -la /workspace && echo done" },
    evaluated_permission: "allow",
  }),
  ev({
    id: "sevt-0610",
    type: "user.tool_result",
    processed_at: atNano(20_009, 300, "975318"),
    session_thread_id: "sth_full_01",
    tool_use_id: "sevt-0609",
    content: [
      {
        type: "text",
        text: "exit_code: 0\n--- stdout ---\ntotal 12\ndrwxr-xr-x 3 root root 4096 Aug 25 18:00 .\n-rw-r--r-- 1 root root   42 Aug 25 18:00 notes.md\ndone\n--- stderr ---\n",
      },
    ],
  }),
  // Custom tool: one start and one result, both types found nowhere else.
  ev({
    id: "sevt-0611",
    type: "agent.custom_tool_use",
    processed_at: atNano(20_011, 0, "112233"),
    session_thread_id: "sth_full_01",
    name: "internal_ticket_lookup",
    input: { ticket_id: "GROWTH-4821", include_history: true },
    evaluated_permission: "allow",
  }),
  ev({
    id: "sevt-0612",
    type: "user.custom_tool_result",
    processed_at: atNano(20_013, 0, "556677"),
    session_thread_id: "sth_full_01",
    custom_tool_use_id: "sevt-0611",
    content: [{ type: "text", text: '{"ticket":"GROWTH-4821","status":"open","owner":"数据平台"}' }],
  }),
  // User interrupt: found nowhere else, and although it is in the status category it is a **real user action**.
  ev({
    id: "sevt-0613",
    type: "user.interrupt",
    processed_at: atNano(20_015, 0, "667788"),
    session_thread_id: "sth_full_01",
    reason: "先别查历史了，只要当前状态",
  }),
  ev({
    id: "sevt-0614",
    type: "session.status_rescheduled",
    processed_at: atNano(20_016, 0, "778899"),
    session_thread_id: null,
  }),
  ev({
    id: "sevt-0615",
    type: "session.thread_status_rescheduled",
    processed_at: atNano(20_016, 400, "889900"),
    session_thread_id: "sth_full_01",
    agent_name: "全量事件探针",
  }),
  // Session renamed / deleted: only this run has these two types.
  ev({
    id: "sevt-0616",
    type: "session.updated",
    processed_at: atNano(20_017, 0, "990011"),
    session_thread_id: null,
    name: "事件类型全覆盖自检",
  }),
  ev({
    id: "sevt-0617",
    type: "agent.message",
    processed_at: atNano(20_018, 0, "101112"),
    session_thread_id: "sth_full_01",
    content: [
      {
        type: "text",
        text: "## 覆盖结果\n\n已跑过 **34** 类事件。要点：\n\n- `agent.tool_use` 与 `user.tool_result` 是**两条**独立事件\n- `requires_action` 表示工具在我们容器里执行\n\n| 域 | 条数 |\n| --- | --- |\n| user | 5 |\n| agent | 6 |\n\n```bash\nls -la /workspace\n```",
      },
    ],
  }),
  ev({
    id: "sevt-0618",
    type: "session.thread_status_idle",
    processed_at: atNano(20_019, 0, "121314"),
    session_thread_id: "sth_full_01",
    agent_name: "全量事件探针",
    stop_reason: { type: "end_turn" },
  }),
  ev({
    id: "sevt-0619",
    type: "session.status_idle",
    processed_at: atNano(20_019, 200, "131415"),
    session_thread_id: null,
    stop_reason: { type: "end_turn" },
  }),
  ev({
    id: "sevt-0620",
    type: "session.deleted",
    processed_at: atNano(20_020, 0, "141516"),
    session_thread_id: null,
  }),
];

export const SEED: SeedData = {
  workspaces,
  agents,
  skills,
  feishuApps,
  runs,
  events: {
    run_1: run1Events,
    run_2: run2Events,
    run_3: run3Events,
    run_4: run4Events,
    run_5: run5Events,
    run_6: run6Events,
    run_9: run9Events,
  },
};

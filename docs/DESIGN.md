# AMA Agent 平台 · 设计文档

> 版本：v0.1（2026-08-26）
> 范围：本文档描述 **AMA 前端（web/）** 的信息架构、数据模型、路由与交互设计。后端已有第一个业务模块「接入飞书应用」（`server/feishu/` + `server/httpd/` + `server/cmd/amad/`，其契约见 3.4），其余模块的数据层仍是**契约优先**：页面只依赖可替换的 `ApiClient` 接口，当前实现是 mock。

---

## 1. 设计出发点

平台是**方舟 Managed Agents 之上的一层多租户管控面**。方舟提供 Agent / Session / Skill 等原语，但它本身没有「空间（workspace）」「成员」「飞书应用绑定」这些组织侧概念。AMA 要补的正是这一层：

| 关注点 | 归属 |
| --- | --- |
| Agent 定义（提示词、模型、工具、技能） | 方舟原语，AMA 代理管理 |
| Session / Event（运行历史） | 方舟原语，AMA 只读聚合与可视化 |
| Skill 定义 | 方舟原语，但**归属由 AMA 定义为空间级资产** |
| Workspace / 成员 / Key | **AMA 自有概念**，方舟侧不存在 |
| 飞书应用与 Agent 的绑定 | **AMA 自有概念**，方舟侧不存在 |

因此数据模型分两类：**镜像方舟的**（字段命名尽量对齐方舟 API，方便直接透传）和 **AMA 自有的**（自由设计）。

### 1.1 核心约束：workspace 优先

所有资源都挂在 workspace 下，没有「全局 agent」。这条约束落到实现上是三件事：

1. **路由层**：除 `/workspaces/*` 引导页与 `/preferences` 全局设置（账号级）外，所有业务路由都在 `/w/:workspaceKey/*` 之下。workspace key 出现在 URL 里，而不是藏在全局状态里 —— 这样链接可分享、刷新不丢上下文。全局设置虽然在 workspace 之外，仍可用 `?from=<key>` 记住来源空间以带上外壳（见下面「一个路由 + 一个 query 参数」）。
2. **网关层**：`WorkspaceGate` 在渲染任何子路由前校验 key 是否存在；不存在则跳转引导页。它同时是「上次访问过哪个空间」这条本机记忆的**唯一写入点**（进入/切换空间时写一次，打不开时忘掉），根路径 `/` 据此直接跳回去。⚠️ 同样是 403，**自动跳来的静静回列表页、手输的仍要说清原因**——理由与那条记忆为什么用 localStorage 而不落库，见 `web/src/lib/lastWorkspace.ts` 与 `docs/guides/frontend.md`。
3. **空态**：一个新用户没有任何 workspace 时，不展示空的列表页，而是**强制**进入创建流程（EmptyState + 创建对话框），无法跳过。

---

## 2. 信息架构

```
/                                    → 上次访问过的空间；没有记录（或它已失效）才去 /workspaces
/workspaces                          workspace 列表 / 引导创建（**手输永远是这一页**）
/preferences                         全局设置（账号级）：外观与语言 / 方舟凭证 / API Key
  └── ?from=<workspaceKey>           带外壳形态：侧栏在、当前空间高亮、返回回那个空间
/w/:workspaceKey
  ├── /                              概览：agent 数、近期运行、成功率、token 消耗
  ├── /agents                        Agent 列表
  ├── /agents/new                    创建 Agent（表单）
  ├── /agents/:agentId               Agent 详情
  │     ├── ?tab=config              提示词 + 模型 + 参数
  │     ├── ?tab=skills              绑定空间技能
  │     ├── ?tab=feishu              绑定飞书应用
  │     └── ?tab=runs                该 Agent 的运行历史
  ├── /chat                          对话：选 agent 发起（?agent= 可预选）
  ├── /chat/:runId                   某次对话（刷新可续、链接可分享）
  ├── /runs                          全空间运行历史列表（可筛 agent/状态/来源/时间）
  ├── /runs/:runId                   运行详情 —— 事件时间线（本平台最复杂的视图）
  ├── /usage                         用量与洞察：调用频次 / 耗时分位 / Token 趋势 / 成功率
  ├── /skills                        空间技能库
  ├── /skills/:skillId               技能编辑
  ├── /feishu                        飞书应用列表（含就绪度诊断）
  ├── /feishu/connect                接入飞书应用（设备码流程，?session= 可续）
  └── /settings                      空间设置（四个 tab **全是空间级的**）
        ├── ?tab=general             name / key
        ├── ?tab=members             成员与角色
        ├── ?tab=ark                 方舟凭证（API Key + 区域）
        └── ?tab=danger              删除空间
```

导航用侧边栏，分三组：**构建**（Agents、对话、Skills、定时任务、飞书应用）、**观测**（运行历史、用量与洞察）与**管理**（空间设置）。分组只靠 spacing + 小号 section label，不用分割线。

「用量与洞察」与「概览」的分工是刻意的：**概览回答「现在怎么样」**（几个汇总数字 + 最近运行，所以它不放图表），**用量页回答「一段时间里谁在花钱、谁在变慢、谁在失败」**。两者都放图表会让概览变成一个更差的用量页；所以图表全在用量页，概览保持一屏可扫完。

**侧栏顶部是 logo + 当前空间名合成的一行**，整行就是空间切换器（点开列出全部空间，末尾一项「管理所有空间」去 `/workspaces`）；再往下才是「概览」与三个导航组。**AMA 字标让位给空间名**——logo 本身已经承担品牌识别，而侧栏顶部真正要回答的是「我在哪个空间」（Slack / Linear / Notion 的通行做法）。这一行**曾是不可点的品牌位**，理由是「它是标识不是导航，去处已由紧下方的『概览』承担，两处高亮是重复反馈」；改成切换器之后那条顾虑不成立了——它不再通往某个地址，而是打开一个菜单，与「概览」不是同一个去处。

**左下角只剩个人信息那一行**（头像 + 用户名，点开出账号菜单）。两处都只显示一个名字，空间的 key 不在侧栏出现（要看去空间设置）。两个身份维度各占侧栏一头：顶部回答「我在哪个空间」，底部回答「我是谁」。

**「空间设置」是一级导航项**，在「管理」组里。它一度被收进账号菜单，理由是「配置不是目的地」；但实践下来它恰恰是常去的目的地，藏在头像后面既不好找、也没法从侧栏看出「我正在设置里」。所以账号菜单只留**个人维度**的东西（个人信息、偏好设置、退出登录），空间维度的一律回到导航。账号菜单里也没有「切换空间」——那是侧栏最顶上那一行，同一个目的地不留两个入口。

**成员是设置页的一个 tab**（`?tab=members`），排在基本信息之后、方舟凭证之前。它曾短暂地独立成页（`/settings/members`）并进侧栏与「空间设置」并列，但那样同一个目的地就有两个入口、点进去侧栏高亮还在两行之间跳。只能留一个的话该留在设置里：成员回答的是「这个空间是什么」，与基本信息同类，而方舟凭证与授权管理是「凭证那一片」，不该被插在中间拆开。旧地址 `/settings/members` 保留一条重定向到 `?tab=members`——那个地址真的在外面跑过（文档、书签、e2e），直接删会让它落到兜底路由、表现成「收藏夹点开回到了空间列表」。

「对话」归**构建**而非观测：它是调 agent 的手段（改完提示词立刻试一句），不是事后复盘。所以 Agent 详情页的操作区也有一个「试一下」直达 `/chat?agent=:id`。

### 「全局设置」是一个路由 + 一个 query 参数，不是两个挂载点

它在侧栏的**「账号」组**（与「管理」组分开：那一组是空间维度的配置，改了影响所有人；这一页是账号级的，只影响自己。两者相邻、图标相近、名字都叫「xx 设置」，分组标题是唯一说清范围的地方）。

它的**内容**是账号级的（在哪个空间看到的都是同一份），但**导航上下文**分两种，所以路由只有一个、外壳靠 `?from=<workspaceKey>` 切：

```
/preferences                无壳 —— 空间列表页进，或还没加入任何空间
/preferences?from=growth    带壳 —— 侧栏「账号」组进：侧栏在、当前空间高亮、返回回那个空间
```

它有四个 tab，**全部是账号级的**：外观与语言 / 方舟凭证 / 授权管理 / API Key。

```
/preferences?tab=general        外观与语言（含重看新手引导）
/preferences?tab=ark            账号级方舟凭证：配一份，各空间引用
/preferences?tab=integrations   授权管理（给 agent 的 AMA 凭据、飞书用户授权等）
/preferences?tab=apikeys        API Key（别人来调我们）
```

**方舟凭证与授权管理紧挨着**：两者是同一件事的两种形态——那边贴一把 key，这边点一次授权。API Key 排最后，因为前两个是「我们代表你去调别人」，它是反方向的。

**API Key 与授权管理都是从空间设置搬过来的。** 两者一直是账号级的（`auth_api_keys` 没有 workspace_id 列；`/api/integrations` 那四条按 actor 取，路径里根本没有 workspaceId，authz 里刻意定成 `ActionNone` 好让还没加入任何空间的人也能授权），住在空间设置里只是历史原因，代价是要靠一枚「个人」Chip 加一段说明去解释范围——**用文案补偿位置放错**。搬完之后位置本身说明了范围，那枚 Chip 已随之删除：空间设置四个 tab 全是空间级的，标记没有要区分的对象了。

⚠️ 曾有一条结论说「授权管理与方舟凭证是同一件事的两种形态，拆开更难理解，所以要留在空间设置」——**已废止**。那是把一个账号级 tab 混在空间级之间的理由，而那种「相邻」的理解收益抵不过范围混淆造成的误解（误解真实发生过，收益只是推测）；何况那对形态在新家照样相邻，且两者范围一致，相邻不再是陷阱。空间设置里剩下的方舟凭证不会因此变孤立——它自己的卡片就写着「空间级的：这个空间的所有成员共用同一把，换掉会影响所有人」，那句话从不依赖邻居。

两个旧地址各留一条重定向（`?tab=apikeys`、`?tab=integrations` → `/preferences?tab=…&from=:key`，**带 `from` 才不会把人送出空间**）。它们真的在外面跑过，后者还被 9 处后端错误提示间接指着（那些提示写「设置 → 授权管理」）。

**这里翻过两次车，方向相反、症状相同**，都是「用户只想改个配色，结果被踢出了正在干活的空间」：

1. 侧栏那一项最初指向无壳的 `/preferences` —— 点一下整个侧栏消失、左侧空间设置不见了，「返回」还把人送回空间列表；
2. 改成**两个挂载点**（`/preferences` + `/w/:key/preferences`）能work，但复杂度落在了每一个指向它的链接上：侧栏、空间设置页那两条「去全局设置」、`?tab=apikeys` 那条重定向，四处都要先判断「我现在在不在空间里」，漏一处就复现现象 1，且零报错。

单一路由把判断收到**一处**（`app/PreferencesRoute.tsx`），四处链接统一走 `preferencesPath()`。参数名取 `from` 而不是 `ws`：这一页不属于任何空间，`ws=` 会让人以为偏好是按空间存的，而那正是这一页反复要向用户解释清楚的事；`from` 与「返回」的语义也对得上。值用 key 不用 id——URL 里 key 可读，且 `workspace-by-key` 接口本来就存在。

**query 里的 key 不可信，取不到就降级成无壳，而不是报错**。key 可能不存在，也可能是别人的空间（RBAC 上线后非成员访问直接 403）。两种都不能崩页：页面内容是账号级的、本来就该看得到，为了一个纯导航性质的外壳把人挡在自己的设置外面是本末倒置——尤其**不属于任何空间的新人恰恰最需要先进这一页**配方舟凭证与 API Key（后端那几条路由特意定成 `ActionNone` 就是为了他）。降级路径与「直接访问 `/preferences`」是同一条，不是第二套分支。

旧地址 `/w/:key/preferences` 保留一条重定向到 `/preferences?from=:key`（含原样带上 `tab`），理由与 `/settings/members` 那条逐字相同：那个地址真的在外面跑过。

---

## 3. 数据模型

以下 TypeScript 类型即前端 `src/types/` 的真实定义。**镜像方舟的字段用 snake_case**（与方舟 API 一致，便于透传与对照文档），**AMA 自有实体用 camelCase**。这个区分是刻意的：看到 snake_case 就知道这条数据的权威来源在方舟。

### 3.1 Workspace（AMA 自有）

```ts
type WorkspaceRole = "owner" | "admin" | "member" | "viewer";

interface WorkspaceMember {
  id: string;
  name: string;
  email: string;
  role: WorkspaceRole;
  avatarUrl?: string;    // 只是缓存、不刷新，缺失时前端落回首字母
  joinedAt: string;      // ISO 8601，固定三位小数（2026-07-26T10:00:00.000Z）
}

interface Workspace {
  id: string;
  name: string;          // 展示名，可改
  key: string;           // URL 标识，全局唯一，[a-z0-9-]{2,32}
  description?: string;
  createdAt: string;
  updatedAt: string;     // 与 createdAt 不等即表示编辑过
  members: WorkspaceMember[];
}
```

`key` 是 URL 的一部分，改 key 等于改所有链接，所以设置页对它单独做「改动会失效旧链接」的提示与唯一性校验。

**时间字段一律是 ISO 8601 字符串、固定三位小数**（不是 `Date`，也不是时间戳数字）。
这条不只是格式偏好：`listRuns` 按字符串排序，两种写法混用会直接改变顺序。
详见 [CLAUDE.md 的「时间戳：单位与格式」](../CLAUDE.md)。

**`members` 在存储上已经不在空间的 jsonb 里了**，它是独立的 `workspace_members` 表
（`id / workspace_id / member_email / role / created_at / updated_at`）。
**HTTP 契约没变**——响应里仍然带完整的 `members` 数组，由服务端读的时候填回去。

抽出来的理由是并发：原先增删改成员是「读整个空间 → 在内存里改数组 → 整份写回」，
两个管理员同时操作会**后写的静默盖掉先写的**。拆成真表之后由数据库裁决，
并且 `(workspace_id, lower(email))` 的唯一索引让「同一个人两行」不可能出现
（查角色是大小写不敏感的，两行会让「改了角色不生效」）。

⚠️ **成员顺序有语义，不是可有可无的**：服务端取「第一个 owner」来决定注入谁的
授权凭据，所以列成员一律按 `(created_at, id)` 排。关联表没有固有行序，
不显式排序的话多 owner 的空间会随机换人——且零报错。

### 3.2 Agent（镜像方舟 + AMA 扩展）

方舟的 Agent 定义包含 name / description / `system`（系统提示词）/ `model` / `tools` / `skills`。AMA 在此之上加 `workspaceId` 和飞书绑定。

模型 ID 与参数取自方舟官方文档「定义 Agent → 模型支持的参数列表」，
**不是猜的** —— 该表还规定了每个模型支持哪些 `reasoning_effort` 档位、以及是否支持
`service_tier: "fast"`（只有 lite 与 turbo 支持），前端的模型选择器据此联动。
清单的代码真源是 `web/src/types/domain.ts` 的 `MODEL_SPECS`；文档还没收录、
但平台实测可用的模型（如 `deepseek-v4-1-flash-261017`）记在
`web/scripts/check-models.mjs` 的 `KNOWN_DOC_LAG`。

```ts
type ModelId =
  | "doubao-seed-2-1-pro-260628"      // 文档示例里的事实默认值
  | "doubao-seed-2-1-pro-260915"
  | "doubao-seed-2-1-turbo-260628"
  | "doubao-seed-2-0-lite-260428"
  | "doubao-seed-evolving"
  | "deepseek-v4-pro-260425"
  | "deepseek-v4-pro-ga-260813"
  | "deepseek-v4-flash-ga-260731"
  | "deepseek-v4-1-flash-261017"
  | "glm-5-2-260617";

type ReasoningEffort = "none" | "minimal" | "low" | "medium" | "high" | "xhigh" | "max";

interface ModelConfig {
  id: ModelId;                                   // 方舟字段名是 id，不是 model
  thinking?: "enabled" | "disabled" | "auto";
  reasoning_effort?: ReasoningEffort;
  service_tier?: "auto" | "default" | "fast";
  speed?: "standard" | "fast";
}
```

注意方舟的 Agent **没有** `temperature` / `max_tokens` / `top_p` —— 推理强度由
`reasoning_effort` 与 `thinking` 表达。前端不要凭 OpenAI 的习惯造这几个字段。

```ts
type BuiltinTool =
  | "bash" | "read" | "write" | "edit"
  | "glob" | "grep" | "web_fetch" | "web_search";

interface Agent {
  id: string;
  workspaceId: string;
  name: string;
  description: string;
  system: string;             // 每个 agent 绑定不同提示词，≤ 100000 字符
  model: ModelConfig;         // 每个 agent 绑定不同模型
  version: number;            // 方舟用它做乐观锁：更新必须回传当前 version
  builtin_tools: BuiltinTool[];
  skillIds: string[];         // 绑定空间技能（引用 Skill.id）
  feishuAppIds: string[];     // 绑定飞书应用
  status: "active" | "disabled";   // 停用是真的会拦住，见下
  createdAt: string;
  updatedAt: string;
}
```

**技能绑定是引用而非拷贝**：agent 存 `skillIds`，技能内容改了所有绑定它的 agent 立即生效。这是把技能设计成「空间级资产」的意义 —— 否则不如直接写进 system prompt。

**`status` 只有两个值，且 `disabled` 是真的会拦住**：停用的 agent 开不了新会话、
已有会话发不出消息（422）、飞书里回一句提示而不是回答、定时任务到点失败并记下原因。
**但历史照常可看、正在跑的照常可中断**——「能看不能发」，因为停用是可逆的。

前端要点三条：

- 冻结判据只用服务端派生的 `Run.agentDeleted` / `Run.agentDisabled`，**不拿本地
  agents 列表反推**（单源判据顺带免掉「agents 还没加载完时闪一下」那类首帧问题）；
- **「已删除」与「已停用」是两个标记、两套文案**，不能合成一个「已冻结」：要用户做的
  事不同（一个去改绑或新建，一个把状态改回「已启用」）。混用的直接后果是用户跑去重新
  建一个 agent，而原来那个的技能与飞书绑定都还在；
- 选择器里停用项**留着但置灰并标出原因**，藏起来会让人以为 agent 被删了。

曾经还有第三个值 `draft`，删掉了：没有任何一处读它做判断，而它想表达的「还没配好」
是一份可诊断的报告而不是用户手拨的开关（同下面 FeishuReadiness 那段「装好不等于能用」）。
坑与判据细节见 [server/core/README.md](../server/core/README.md) 的「Agent 状态」。

方舟侧的对应结构是 `skills: AgentSkillRef[]`，其中
`AgentSkillRef = { type: "anthropic" | "custom" | "skill_hub"; skill_id?: string; version?: string }`
（`version` 是**字符串**），每个 agent 最多 20 个技能。AMA 的 `skillIds` 在提交给方舟时展开为这个形状。


### 3.3 Skill（空间级）

```ts
interface Skill {
  id: string;                // 方舟形如 skill-20260812080348-xxxxx
  workspaceId: string;
  name: string;              // 方舟侧技能名，[a-z0-9-]
  displayTitle: string;      // 方舟字段 display_title
  description: string;       // 决定模型何时调用它，很关键
  instructions: string;      // 技能正文（即 SKILL.md 内容）
  source: "custom" | "skill_hub";
  latestVersion: string;     // 方舟 latest_version，字符串
  tags: string[];
  createdAt: string;
  updatedAt: string;
}
```

> 方舟真实的技能创建走 `multipart/form-data` 上传 ZIP（内含恰好一个 `SKILL.md`），
> 而非 JSON。AMA 在 UI 上让用户直接编辑 `instructions` 正文，提交时由后端打包成
> ZIP 再调方舟 —— 这个转换属于后端职责，前端只管正文。


### 3.4 飞书应用（AMA 自有）

**接入方式是 OAuth 2.0 device flow 自助注册，用户不手填任何凭证。**
`accounts.feishu.cn/oauth/v1/app/registration` 不需要 tenant token，所以在「手上还没有
任何飞书应用」时也能自举 —— 这一点与方舟形成对照：方舟没有 OAuth 通路（见 3.5），
只能手填长效 API Key；飞书有，所以就不该让用户去抄 AppID 和密钥。

流程在独立路由 `/w/:key/feishu/connect`，三步：去飞书授权 → 轮询等待 → 完成并给出
就绪度诊断。`sessionId` 落在 URL query 上，刷新可续。**AppSecret 只在服务端短暂停留，
绝不回传浏览器**（后端的 `App` 结构压根没有该字段，见 `server/feishu/README.md`）。

```ts
export type FeishuAppStatus = "unverified" | "connected" | "error";

interface FeishuApp {
  id: string;
  workspaceId: string;
  name: string;
  appId: string;             // cli_xxx，由飞书生成
  avatarUrl?: string;
  botOpenId?: string;
  region: string;            // feishu（国内）/ lark（国际）
  tenantBrand?: string;      // 注册时飞书回报的租户归属
  status: FeishuAppStatus;   // 粗粒度，细节看 readiness
  eventSubscriptionUrl: string;
  boundAgentIds: string[];   // 反向引用（派生，不落库）。1:1 独占，最多一个元素
  readiness?: FeishuReadiness;
  createdById: string;
  createdAt: string;
  updatedAt: string;
}
```

**「装好」不等于「能用」，所以就绪度是一份可诊断的报告而不是布尔值。**
不能用至少有七八种原因（权限没申请、权限等审批、事件没订阅、回调没订阅、机器人
没启用、版本没发布、凭证失效），每种要用户做的事都不同，只回 `false` 用户只能一个个试。

```ts
type FeishuReadinessStatus =
  | "setup_required"       // 要用户自己去开放平台补配置
  | "waiting_for_admin"    // 已申请，只能等管理员批
  | "ready"
  | "runtime_unavailable"; // 配置齐了但连不上，通常是凭证失效

interface FeishuReadiness {
  status: FeishuReadinessStatus;
  checkedAt: string;
  missingScopes: string[];      // 压根没申请 → 用户自己去加
  unapprovedScopes: string[];   // 已申请等批 → 只能催管理员
  missingEvents: string[];
  missingCallbacks: string[];
  configurationInspectable: boolean;  // 为假时下面几个布尔不可信
  botEnabled: boolean;
  published: boolean;
  groupMessageScopeGranted: boolean;
  adminAuthorizationRequested: boolean;
  runtimeConnected: boolean;
  lastConnectedAt?: string;
  lastRuntimeError?: string;
  lastCheckError?: string;
}
```

`missingScopes` 与 `unapprovedScopes` **必须分开**：前者要用户去改配置，后者只能等
审批。合成一个「缺权限」列表，用户就会对着已经提交过的权限反复点申请。

粗状态 `status` 保持三值不变、把粒度放进 `readiness`，与 3.5 的 `ArkCredential`
把粗粒度 `check` 和细节 `checkError`/`checkedAt` 分开是同一手法：粗状态给列表页当徽标，
细节给诊断用，两者变化频率不同。

绑定是**双向可见**的：Agent 详情页能选飞书应用，飞书应用列表也能看到挂了哪个 agent。

**一个飞书应用只能绑一个 agent（1:1 独占）。** 早前设计成多对多（想按群/按场景路由），
但那条路由规则从来没有实现——一条入站消息进来时，`inbound` 只能在绑着的几个 agent
里取第一个，于是「谁来回答」由数据库行序决定，而界面上两个 agent 都显示连着同一个
应用、看不出哪个真的会回话。要恢复多对多得先答一个产品问题（按 @ 的名字选？让用户
选？轮流？），不是放开约束就能解决的。

约束在 `core.Service` 的写入路径上拦（409 + 占用者名字），前端在 Agent 详情页的
飞书 Tab 上**提前**表达：被别人占着的应用禁用开关并直接写出占用者是谁，而不是等
用户点了保存才报错。那一行还额外标了「已绑定 / 未绑定」徽标——它与右边那个应用
健康度徽标（已连接 / 待验证 / 连接异常）是两件事，早前只有后者，于是一个健康的应用
在任何 agent 的这一页上都显示绿色「已连接」，用户据此以为当前 agent 已经接上了。

`FeishuApp.boundAgentIds` 因此实际最多一个元素，但**字段形状仍是数组**：改成标量
要同时动前后端与所有存量数据，而存量里可能还有违规的多绑记录——那种数据要能被读
出来、被看见，才修得掉。它是**派生字段**，服务端读的时候现算，绝不落库。

### 3.5 方舟凭证（AMA 自有，按空间维度）

```ts
type ArkRegion = "cn-beijing" | "ap-southeast-1";
type ArkCredentialCheck = "unchecked" | "valid" | "invalid";

interface ArkCredential {
  workspaceId: string;
  apiKeyMasked: string;      // 只展示掩码，明文不回传前端
  region: ArkRegion;
  check: ArkCredentialCheck;
  source: ArkCredentialSource;  // override 自己存了 / inherit 引用账号级那份
  inheritFrom?: string;         // inherit 时被引用者的邮箱
  checkedAt?: string;
  checkError?: string;
  updatedAt?: string;
  updatedBy?: string;        // 凭证类配置要留痕
}

// agent 级（三级里最具体的那一级）。**有记录 = 自定义，没记录 = 跟随空间。**
interface AgentArkCredential {
  agentId: string;
  apiKeyMasked: string;
  region: ArkRegion;
  check: ArkCredentialCheck;
  custom: boolean;           // false = 跟随空间。不可选：键缺席会让界面两个分支都不命中
  checkedAt?: string;
  checkError?: string;
  updatedAt?: string;
  updatedBy?: string;
  effective: ArkEffective;   // 当前实际生效那份的摘要，见下
}

// 「到底用了哪个」的答案，只用于展示。
interface ArkEffective {
  level: "agent" | "workspace" | "global" | "";  // 空串 = 三级都没配
  apiKeyMasked: string;
  region: ArkRegion;
  check: ArkCredentialCheck;
  ownerEmail?: string;       // 只在 level 为 global 时有值
}
```

**为什么没有 OAuth 换 key 这条路。** 调研过，方舟没有：

- 方舟官方文档里，60 篇 API 参考的「鉴权」段落**只有一种方式** ——
  「API Key 鉴权，请在 API Key 管理页面，获取**长效** API Key」，即
  `Authorization: Bearer $ARK_API_KEY`。**没有任何创建 API Key 的 OpenAPI**
  （全仓 grep `CreateApiKey` 零命中）。
- 文档里近 50 处 `OAuth` 命中**全是 `mcp_oauth`** —— 那是 agent 拿去访问第三方
  MCP 服务（Slack、Google Drive）的凭证，存在 Vaults 里，与「获取方舟 API Key」
  是反方向的东西。`STS` / `临时凭证` / `AssumeRole` 命中数为 0。
- 唯一沾 SSO 的是 `arkcli auth login volc-sso`（浏览器授权），但它服务于 arkcli
  走的 OpenTOP 管控面，文档明确说这与「`curl` 直调 REST API（全程仅需 API Key）」
  不同，且没有任何「换出一把 ARK_API_KEY」的说法。
- 最接近 STS 的是管控面的 `GetApiKey` 动作（`POST /?Action=GetApiKey`，
  `DurationSeconds ≤ 2592000`，返回临时 `ApiKey`）—— 但它**只接受 AK/SK 签名鉴权**
  （HMAC-SHA256，service `ark`），也就是说仍要用户先交出更高权限的 AK/SK；且 SDK
  v0.4.0 里这条路径只为 `endpoint` / `presetendpoint` 资源接了线，Managed Agents
  的 agents / sessions 等接口是否走得通未经验证。用它换不来「更少的用户输入」。

所以开源版只支持手填（「列出账号里已有的 key」留了扩展点，见下面「方舟凭证」）。

手填仍要做得不难受 —— 给控制台直达链接、存下只回显掩码、提供一次真实校验
（方舟没有「测 key」接口，实现是拿这把 key 调一次只读接口，按 401/403 判失效），
避免用户配错了却要等到跑 agent 时才发现。

**三级配置：个人（账号级）→ 空间 → Agent，越具体越优先。**

```
agent 级   agent_ark_secrets（主键 agent_id）    ← 最优先
空间级     ark_secrets（source=override/inherit）
账号级     global_ark_secrets（主键 email）      ← 兜底
```

起点是「按空间维度」——空间是计费与隔离的边界，同空间的 agent / session 共用一把 key。
但两头都不够：一个人有五个空间要把同一把 key 贴五遍（于是有了账号级 + 空间引用），
而同一个空间里的 agent 常常属于不同业务线、账要分开（于是有了 agent 级）。

**核心不变量是「下级不存任何上级的副本」**：跟随时下级根本没有行，每次读都现取，
所以改上级立刻联动，不需要同步逻辑、也没有「忘了同步」的可能。
⚠️ 绝不能改成「跟随时复制一份到下级」——那样改上级不会联动，而联动正是这套机制
存在的理由，且它**零报错**（界面仍显示「跟随空间设置」，只是那把 key 已经旧了）。

**agent 级「没有记录」就是「跟随空间」**，与空间级刻意不同：空间引用账号级那份要求
一次显式点击（那是「改用别人的 key」，不该让人猜），而 agent 默认跟随空间是它一直
以来的行为——要求点一下才「跟随」会让所有既有 agent 在升级后突然没凭证可用。

**界面上必须能看出当前生效的是哪一级。** 三级配置最容易让人困惑的就是「我配了三个
地方，到底用了哪个」，所以 agent 级的对外视图带一个 `effective` 摘要
（`level` 为 `agent`/`workspace`/`global`，`global` 时还给 `ownerEmail`）。
来源条的视觉三处统一：中性底 `bg-surface-secondary` + `gravity-ui:link`——
**不用 badge、不用 warning 色**，它陈述事实不报告好坏（warning 会让人以为「跟随」
是个需要修的问题）。

换 key 或换区域都会把 `check` 重置为 `unchecked` —— 同一把 key 在另一个区域未必开通了
服务，留着旧结论等于虚报可用。

**明文加密落库**（AES-256-GCM，密钥走配置）。方舟 key **不能只存 hash**：它要被原样
发给方舟做推理，存 hash 就再也拿不回可用的 key——与 `auth_api_keys`（别人来调我们，
只需比对）方向相反。密文带 `v1:` 前缀，所以存量明文零迁移继续可用。
详见 `server/shared/infra/cryptobox` 与 `server/core/README.md`。

**目前只支持手动填写。** 服务端留了一个「从账号里列出已有 key」的扩展点
（`service/arkcred` 的 `KeyFetcher` 接口，`cmd/amad` 里传 nil），未接入时只能手填。
⚠️ 若以后接入，**明文不经过前端**：只读接口只回掩码与 id，用户选定后前端只回传 id，
由服务端现取明文、算掩码、加密落库。

### 3.6 运行历史 —— 本平台最复杂的部分

一次「运行」对应方舟的一个 **Session**。Session 里可以有多个 **Thread**（多 Agent 协作时子 agent 各占一个线程），每个 Thread 下是一串 **Event**。

方舟的 `SessionStatus` 只有 4 个值：**`idle` / `running` / `terminated` / `rescheduling`**。
注意 `rescheduling` 是**状态**值，而对应的**事件**类型叫 `session.status_rescheduled`（过去分词），
两者不要混用。AMA 的 `RunStatus` 直接复用这 4 个值，再加一个前端派生的 `requires_action`
（当 `session.status_idle` 的 `stop_reason.type === "requires_action"` 时展示为此状态）。

#### ⚠ `requires_action` 不是「等用户确认」，而是「工具正在我们容器里执行」

**这条最容易搞错，且错了之后界面会对用户说谎。** self_hosted 架构下方舟只做模型推理，
工具调用被扔进 work 队列、由 amad 里常驻的 worker 领走下发到会话容器执行
（`server/core/worker.go`）。所以**每次工具调用**方舟侧都必然先 `agent.tool_use`、
然后转 idle 并给出 `stop_reason: requires_action` —— 含义是「我把控制权交还给你们了，
等 `user.tool_result` 回传」。**根本没有人在等用户点任何东西。**

后端一直是这么判的（`server/feishu/resume.go:302`、`server/core/scheduler.go:602`、
`server/feishu/inbound.go:931` 都写着「requires_action 不是终态，agent 在等工具结果」），
前端曾经不是：把这个 idle 无条件渲染成独立条目「线程空闲 / 会话空闲 ·
**等待用户确认**（N 项待确认）」。用户在工具跑得最久的那段时间里看到的是
「已经停下来了、还要我去点什么」—— 两件事都不成立，而且**点无可点**。

现在的呈现（`lib/timeline.ts`）：

| 信号 | 判据 | 呈现 |
| --- | --- | --- |
| **工具执行中** | idle 的 `stop_reason.event_ids` 指向某次 `tool_use` | 那条 idle **并进该次工具调用**，工具行显示「执行中」+ spinner；不再单独成条 |
| **真的等你确认** | `evaluated_permission === "ask"` **且** 没有 `user.tool_confirmation` | warning 的醒目 Chip「待你确认」+ 展开后一句「需要你批准后才会执行」 |

三条纪律：

- **判「等用户」只能用 `ask` + 无 confirmation**，绝不能用 `stop_reason`。也不能只判
  `ask`（那会让已批准的调用一直挂着待办——`RunTimeline` 真踩过这一版）。
- **`event_ids` 挂不上时照实单独显示一条「交还控制权」，但仍然不算一轮结束。**
  吞掉是丢信息；当成结束会让那次调用被折叠，而它恰恰是用户唯一想看的东西。
  闸门在 `endsTurn()`，种子 `run_4` 里那条 `event_ids: []` 的 idle 专为覆盖它而留 ——
  **没有它这道闸门就是测不到的死代码**（能挂上的 idle 已在配对阶段被吸收，走不到那里）。
- **被吸收的事件仍进 `raw`**，「查看原始 payload」看得到——UI 折叠可以，藏数据不行。
- **执行中刻意不显示「已耗时」。** 唯一多出来的时间戳是那条 idle 的 `processed_at`，
  但它是「方舟交还控制权」的时刻（种子里距 `tool_use` 只有 1 秒），当已耗时会在一条
  跑了几分钟的命令上显示「1.0s」；拿墙钟现在时刻减则会让三天前的历史显示「3 天」。
  **算不出来就不显示**，同「durationMs 不能当 0」那条纪律。

`RunStatus` 那一层**分不出这两条路**（两者在方舟侧都是 `idle` + `requires_action`），
所以运行列表只说通用的进行中态「工具执行中」；**运行详情页有事件，就把话说准**
（`runAwaitsUser()`），否则顶部写「工具执行中」、底下那条工具行写「待你确认」，自相矛盾。

```ts
type RunStatus = "running" | "idle" | "requires_action" | "rescheduling" | "terminated";
type RunTrigger = "manual" | "feishu" | "api" | "schedule";

interface TokenUsage {
  input_tokens: number;
  output_tokens: number;
  cache_read_input_tokens: number;
  cache_creation_input_tokens: number;
}

interface RunThread {
  id: string;                      // session_thread_id
  agent_name: string;
  status: "idle" | "running" | "terminated" | "rescheduling";
  parent_thread_id: string | null; // null = 主线程
  startedAt: string;
  endedAt?: string;
}

interface Run {
  id: string;
  workspaceId: string;
  agentId: string;
  agentName: string;
  title: string;             // session title
  status: RunStatus;
  trigger: RunTrigger;
  triggerDetail?: string;    // 如飞书群名 / 调用方
  startedAt: string;
  endedAt?: string;
  durationMs?: number;
  usage: TokenUsage;
  threads: RunThread[];
  eventCount: number;
  errorSummary?: string;
  agentDeleted?: boolean;    // 服务端派生：关联 agent 已删 → 会话冻结。见 3.6.4
}
```


#### 3.6.1 事件模型

方舟定义了 7 类客户端上行事件 + 26 类服务端推送事件，共 **33 类**（见方舟官方文档
「Sessions → 会话事件 → 会话事件结构参考」）。
前端建模为**判别联合**，`type` 为判别字段，公共字段抽到 `RunEventBase`：

```ts
interface RunEventBase {
  id: string;                    // 形如 sevt-<ts>-<rand>
  sequence: number;              // 会话内单调递增序列号，列表与 SSE 都带
  processed_at: string | null;   // null = 已入队未处理
  session_thread_id: string | null;
}
```

按 domain 分四组（对齐方舟 `{domain}.{action}` 约定）：

**user 域（上行，7）**：`user.message`、`system.message`、`user.interrupt`、`user.tool_confirmation`、`user.tool_result`、`user.custom_tool_result`、`user.define_outcome`

**agent 域（10）**：`agent.message`、`agent.thinking`、`agent.tool_use`、`agent.tool_result`、`agent.mcp_tool_use`、`agent.mcp_tool_result`、`agent.custom_tool_use`、`agent.thread_message_sent`、`agent.thread_message_received`、`agent.thread_context_compacted`

**session 域（12）**：`session.status_running`、`session.status_idle`、`session.status_rescheduled`、`session.status_terminated`、`session.error`、`session.updated`、`session.deleted`、`session.thread_created`、`session.thread_status_running`、`session.thread_status_idle`、`session.thread_status_rescheduled`、`session.thread_status_terminated`

**span 域（5）**：`span.model_request_start`、`span.model_request_end`、`span.outcome_evaluation_start`、`span.outcome_evaluation_ongoing`、`span.outcome_evaluation_end`

关键的多态字段：

- `content: ContentBlock[]` —— `text` / `image` / `document` / `search_result` 四种块，图片的 `source` 分 `base64` / `url` / `file`，文档的 `source` 多一种 `text`（内联纯文本）。
- `session.error.error` —— 按 `type` 分 `model_overloaded_error`、`model_rate_limited_error`、`model_request_failed_error`、`mcp_connection_failed_error`、`mcp_auth_failed_error`、`billing_error`、`credential_unreachable_error`、`unknown_error`，都带 `retry_status`。收到该事件**不代表运行结束**：可恢复错误（如限流）会自动重排继续跑，只有不可恢复的才转 `terminated` —— 所以 UI 上错误条目要显示重试状态，不能一律标成「失败」。
- `session.status_idle.stop_reason` —— `end_turn` / `requires_action`（带 `event_ids`，指向在等**工具结果回传**的那几条 `tool_use` 事件，**不是**在等用户确认，见 3.6 开头那节）/ `retries_exhausted` / `user_interrupt`。
- `span.model_request_end.model_usage` —— token 用量，运行详情的 token 汇总由这些事件累加得出。
- `agent.custom_tool_use.input` 是 **JSON 编码的字符串**，展示前需 `JSON.parse`，不能直接当对象渲染。
- `agent.thinking` **不携带思考内容**，只是「正在思考」的信号 —— UI 上不要留空正文区。

**事件关联全靠 ID 配对**（方舟没有 turn 概念，事件信封上也没有 `turn_id`）：
`tool_use_id` 串起 `agent.tool_use` ↔ `agent.tool_result`；`custom_tool_use_id` 指向
`agent.custom_tool_use` 的 `id`；`model_request_start_id` 与 `outcome_evaluation_start_id`
闭合各自的 span；`from/to_session_thread_id` 串起多 agent 的线程间通信。
一「轮」由 `session.status_running` … `session.status_idle` 括起来。


#### 3.6.2 事件的呈现策略

33 类事件平铺成一条流会完全不可读。设计上做三件事：

**一、按语义归类，给每类一个视觉身份**（图标 + 语义色）：

| 类别 | 包含 | 视觉 |
| --- | --- | --- |
| 对话 | `user.message`、`agent.message`、`system.message` | 主体内容，最大视觉权重 |
| 思考 | `agent.thinking` | 弱化，muted |
| 工具 | `*_tool_use` + `*_tool_result` **配对折叠** | 单行摘要，可展开看入参/结果 |
| 线程 | `thread_*`、`session.thread_created` | 弱化的结构性标记 |
| 状态 | `session.status_*` | 细线状态标记 |
| 追踪 | `span.*` | 默认**折叠**，token 用量汇总到顶部 |
| 错误 | `session.error` | danger 色，始终展开 |

**二、tool_use 与 tool_result 配对**。方舟把一次工具调用拆成两个事件（用 `tool_use_id` / `mcp_tool_use_id` 关联）。时间线上把它们合成**一个**条目：标题是工具名，副标题是入参摘要，展开后上半是入参、下半是结果，结果 `is_error` 时整条标红。这一步把事件数量差不多砍掉三分之一，也更符合用户心智（「调用了一次工具」而不是「发生了两件事」）。

**三、给出降噪控制**（事件视图 = Trace 视图，控件在 `components/TimelineToolbar.tsx`，
状态在 `lib/useTimelineFilter.ts`，全部进 URL）。筛选建在**三条真实存在**的轴上，
而不是新造一套分桶 —— 前两条是交叉关系，所以都得给：

| 轴 | 值域 | 出处 |
| --- | --- | --- |
| **域** | `user` / `agent` / `session` / `span` | 方舟 `{domain}.{action}` 约定（`system.message` 归 User 域是方舟明确的例外，判据是方向而非前缀） |
| **语义类别** | 上表那 8 类 | AMA 自己的呈现分组，默认隐藏 `trace` |
| **具体类型** | 34 类，只列本次运行出现过的 | `RunEventType`，默认收起 |

「工具 / 模型 / 错误 / 系统」做成**预设**，一键落到前两条轴上。另有三个正交条件与一个排序：

- 关键词即时搜索：工具入参、stdout/stderr、返回值、**原始 payload JSON**
- 只看慢调用（> 5s）、只看失败（错误事件 + 工具回执 `is_error`）
- 按发生顺序 / 按耗时降序（耗时未知的一律排最后，不能当 0）
- 多线程时按线程分组/切换
- 每条事件可展开原始 JSON payload 并复制（调试兜底 —— 无论 UI 是否覆盖了某个字段，
  用户总能看到真实数据）

**三处必须在 UI 上说实话的地方**（都是「数据本身有局限」，藏起来就是误导）：

- **耗时是派生的墙钟时间**：方舟不给 duration 字段，只能用 `processed_at` 差值（秒级），
  含排队与容器从暂停恢复的 16~29 秒。「> 5s」的语义是「让用户等了 5 秒以上」。
- **退出码 ≠ 成败**：`grep` 无匹配退出 1。成败以事件的 `is_error` 为准，退出码是中性 Chip。
- **「系统」这一档覆盖不到环境准备**：容器 create/pause/resume 只写服务端日志，不产生事件。

**没有 SSE 这一档** —— 增量帧是 `StreamFrame`，方舟明确 best-effort 且不进 event history，
历史里没有「增量」可筛。能筛的连接与状态迁移已在 `status` / `thread` 类里。

**退出码 / stdout / stderr 也不是结构化字段**，是 `lib/toolResult.ts` 从结果文本里按
`shared/infra/shelltools/bash.go` 的格式解析出来的，解不出就整段原样显示。

顶部固定一个运行摘要条：状态、耗时、token 汇总（input/output/cache）、触发来源、线程数。

### 3.6.3 对话页 —— 与运行历史共用一份数据

**一次对话就是一条 `trigger: "manual"` 的 `Run`，消息就是 `RunEvent`。** 刻意**不新增**
Conversation / Message 实体。

理由是：新增一套模型，「同一件事」在系统里就有两种表示，后端要实现两遍、事件的分类与
配对逻辑要写两遍，而对话与运行历史本来就是同一批数据的两种读法。复用之后白拿三件事：
对话自动出现在运行历史里、能从历史点回来续聊、`tool_use ↔ tool_result` 的配对直接复用
`lib/timeline.ts` 的 `buildTimeline()`（配对涉及三套关联字段，抄一遍必然走偏）。

`RunTrigger` 里那个 `"manual"` 档位本来就是为此留的。

**同一份运行，五种呈现，两个页面都能切**（`?view=` 决定，缺省是对话）。

前三档看的是**事件**（对话读法 / 事件读法 / 调用树），后两档看的是另一样东西
——**agent 干活的那个容器**：`?view=files` 浏览它的文件、`?view=terminal` 进它的
终端（见 3.6.5）：

| | 对话视图（`ConversationView`） | 事件视图（`RunTimeline`） |
| --- | --- | --- |
| 视角 | 对话 | 排查 |
| 默认 | **是** | 手动切 |
| 布局 | 按轮折叠的消息 | 竖向轨道 + 圆点 |
| 事件 | 按轮聚合，过程可折叠 | 33 类全给，域/类别/类型三轴 + 关键词筛选 |
| `span.*` | 一律不显示（去事件视图看） | 可手动打开（「模型」预设会连带打开） |
| `session.status_*` / `thread_status_*` 的 running / idle | **一律不显示**，见下 | 全给 |
| 工具调用 | 轮内一行，随轮次折叠 | 时间线条目，可展开看命令行/退出码/stdout/stderr |

早先运行详情页默认是事件视图、另给一个「只看对话」开关，那个开关**已删掉**：
它只是按 category 过滤，滤完连「中间干了什么」的入口都没了，是丢信息而不是折叠信息。

后两档（调用树 / 容器）都是从「埋在别的视图里」提上来的独立一档：一个视角被塞进
另一个视角的末尾，等于让它变成附录，而 `?view=` 也就没法直接分享/直达。

#### 对话视角不播报 self_hosted 的生命周期抖动

上表那条「running / idle 一律不显示」值得单独说，因为它的**量**是随工具调用次数涨的。
self_hosted 下每次工具调用，session 与 thread 都要在 running ⇄ idle 之间翻一个来回：

```
agent.tool_use → status_idle + thread_status_idle   （交还控制权，等 tool_result）
   ← worker 把结果回给方舟（user.tool_result）
             → status_running + thread_status_running（方舟重新起跑，接着推理）
```

一轮里调 5 次工具就有 5 组「会话开始运行 / 线程开始运行 / 线程空闲」。**那是 self_hosted
的心跳，不是对话内容**——用户那句问话会被一串状态播报冲走，而每一条都没告诉他任何事
（「工具在跑」已经由工具行自己的「执行中」说了，重复表达同一信息就该删掉）。
名单在 `ConversationView.tsx` 的 `LIFECYCLE_CHURN`，两条纪律：

- **必须按 `type` 列白名单，不能按 `category` 一刀切。** `user.interrupt` 的 category
  也是 `status`，但它是**用户真做过的动作** —— 滤掉它等于让「我中断过这一轮」凭空消失，
  用户只看到回答半截没了、没有任何解释。`*_rescheduled`（从瞬时错误恢复）与
  `*_terminated`（真的结束了）同理都要留：它们是转折点，不是心跳。
  这道边界由 `e2e/09-chat.spec.ts` 的「中断正在跑的对话」守着。
- **事件视图仍然 33 类全给。** 删的是「对话」这一种读法下的噪音，不是数据 ——
  同 `span.*` 的处理方式。e2e 里那条用例特意两边都断言（对话视图 0 条、事件视图可见），
  否则「把噪音从 `buildTimeline` 里删掉」也能让测试变绿，而那是真的丢数据。

顺带一个早先漏掉的：**thread 级的 `end_turn` idle 也是噪音**。session 级那条是轮次结束
标记（被 `groupTurns()` 吃掉、压根不进段），thread 级的没人吃，于是一直以
「线程空闲 · 正常结束」的形态挂在每一轮末尾。

#### turn 是前端派生的

**方舟没有 turn 概念**（信封上没有 `turn_id`），所以「一轮」由前端从事件流推出来，
逻辑在 `lib/timeline.ts` 的 `groupTurns()`：

- 以 `user.message` 为界切开；首条用户消息之前的事件归入一个无输入的前导段。
- 段内按「是不是 agent 的答复」交替成 `output` / `process`。**只有 `agent.message` 算
  output**，`system.message` 是系统提示词、归进过程。一轮里 agent 可以先说「我先查一下」
  再去调工具再给结论，所以 output 与 process 会交替多次 —— 中间那句也是答复，
  不能折进过程里。
- **`done` 的判据有个坑**：`session.status_idle` 通常表示这轮结束，但
  `stop_reason.type === "requires_action"` 那种**不算**。方舟侧它确实是 idle，可
  self_hosted 下那只是「控制权交还给我们的 worker、等 `user.tool_result`」——**工具还在
  我们容器里跑**，这一轮远没结束（详见 3.6 开头那节）。把它当结束有两个后果：那次仍在
  执行/待确认的调用被折叠起来（而它恰恰是用户唯一想看的东西），以及后面真正的
  `end_turn` 被算成下一轮，一轮对话被切成两半。

呈现规则（`components/ConversationView.tsx`）：

- 轮次**已结束** → 过程折叠成一行摘要（步骤数 · 工具数 · 耗时，有失败补一段标红的
  「N 个失败」），点开看每一步。
- 轮次**进行中** → 过程展开并 shimmer；步数超过 3 时只完整展示最后 3 步，
  更早的收进「更早的 N 步」可展开组。
- **一轮没有任何 output 时过程默认展开** —— 没有答案时，过程就是答案。
  失败终止、还在跑、卡在待确认都是这种形状，折叠它们会让页面只剩一句用户问话。


#### 实时性：逐字增量

走方舟 SSE 的 `event_deltas` opt-in（`GET /sessions/{id}/events/stream?event_deltas=agent.message,agent.thinking`）。
不传这个参数方舟**不下发任何增量帧**，回复就会整段蹦出来。

**增量帧刻意不进 `RunEvent` 联合**，单独建 `StreamFrame`（`types/events.ts`）。
方舟明确 `event_start` / `event_delta` 是 best-effort 且**不进入 event history** ——
事后查历史永远查不到它们。混进那个镜像历史的 33 类联合里，会让
`buildTimeline()` / `computeStats()` 这些按 `RunEvent` 穷举的地方，全都要处理一种永不
落库的东西。

它的定位只是「最终事件到达前的临时显示脚手架」：

```
event_start → 开一条空草稿
event_delta → 逐字追加        ← 「实时」的来源
event       → buffered 的最终事件，**它才是真源**，到达时按 id 整体替换掉草稿
```

这样即便中途丢帧（best-effort 允许丢），最终结果仍然是对的。

**两条顺序上的坑**：

1. **先补历史再订阅。** 方舟不支持历史回放，订阅只收到此后产生的事件。反过来写
   （先订阅再拉历史）会让两步之间的事件被历史覆盖掉。
2. **`agent.thinking` 不带内容**，所以它只用来点亮「思考中」，不留空正文区。

轮询/订阅逻辑收敛在 `lib/useConversationStream.ts`，纪律照 `useRegistrationPoll.ts`
那一套（卸载即取消、终态即停）。

#### 3.6.4 agent 被删除后：会话冻结但仍标明归属

Agent 是**硬删除**，没有软删字段，删的时候也不级联动 Run。于是有两个问题要一起解：

**一、不能再对话。** 删之前只有「建会话」查了 agent，「发消息」与「订阅」拿到 run 后
直接算 `arkKey(workspaceId, agentId)` —— 而那个 key 是**空间级**的，压根不查 agent 表。
所以 agent 没了对话照常跑。闸门加在 `AppendRunEvents`，返 **422
`precondition_failed`**（不是 404：`http.ts` 会把 `not_found` 转成 null，页面变静默空态）。

**冻结只挡写，两处刻意放行**：

- **`user.interrupt` 放行。** 方舟侧 session 可能还在跑，挡掉用户就永远停不下来它
  （方舟继续烧 token，容器要等空闲暂停甚至 TTL 才收）。中断不产生新工作，它停止工作。
- **SSE 订阅不挡。** 用户要能翻历史；而且上面放行的中断，其回执正是从这条流回来的。

**二、还得知道这条会话归属谁。** 靠 `Run.agentName` —— 它是建会话时的**快照**，
agent 删了仍然在，所以名字天然还看得见。缺的只是一个标记：`Run.agentDeleted`
由服务端在读 Run 时现算（`core/ark.go`），四个渲染 `agentName` 的地方都挂一个
「已删除」Chip（对话页头部与左栏、运行历史列表、运行详情、空间概览），
运行详情那个必然 404 的「查看 Agent」按钮同时隐掉。

`agentDeleted` 是**派生字段、不落库**（同 `feishu.App.BoundAgentIDs`）。它在 Go 侧是
三条 JSON tag 铁律的第一个书面例外——带了 `omitempty`，理由见 `model/apimodel/apimodel.go` 该字段的注释。


---

### 3.6.5 容器视图 —— agent 在哪儿干活

对话页与运行详情页的第四、五档。看的不是事件，是**执行工具的那个会话容器**
（默认执行端是每个方舟会话一个 Docker 容器）：`?view=files` 浏览它的文件，
`?view=terminal` 进它的终端。

**拆成两档而不是一个「容器」档**：两种用法完全不同——文件是「扫一眼有什么」，
终端是「进去干活」且要占满高度。挤在一起终端永远只能分到下半屏。
容器状态（四态那一段）两边各显示一份，它是两者共同的前提。

#### 四态，两对不能合并

容器的状态**不缓存**，每次进面板都现查一次（后端 inspect 一次容器）。理由与
「授权状态不落库」（3.5）同源，但更迫切：容器会**自己**变——空闲 60 秒自动暂停、
闲置超过 TTL 被回收巡检删掉，两件事都不经过这次请求，没有任何人来更新页面上的记录。
存快照必然出现「页面说运行中、实际早没了」。

| 状态 | 什么意思 | 页面上给什么 |
| --- | --- | --- |
| `none` | 这次对话还没调过工具，**压根没建过容器** | 「还没用到容器」，不给任何按钮 |
| `running` | 容器活着 | 文件列表 + 终端 |
| `paused` | 空闲自动暂停（或停止），**工作区还在** | 「已暂停」+ 恢复按钮 |
| `gone` | 工作区已被回收，文件真没了 | 「已回收，工作区已重置」 |

⚠️ **两对绝不能合并，合并之后零报错，只有用户会做出错误的行动**：

- `none` 说成 `gone` → 用户以为文件丢了，来问「我的工作区呢」，而根本没有过文件；
- `paused` 说成 `gone` → 用户白白从头再来一遍，而工作区其实还在。

**查看状态与列文件都不自动恢复容器。** 恢复会重新占用宿主机资源，让一个「看一眼」的
只读动作产生那种代价、而用户不知道自己触发了什么，是不对的——所以恢复是一个显式按钮。

#### 文件列表只读、逐层拉

后端 depth 硬上限 1，点进一个目录才发一次请求；条目超过 500 会截断，
并**在页面上明说**（不说的话「目录里就这些」是个错觉）。

**没有用现成的 FileTree 组件**：那类组件通常要求一次给出整棵树，而这里是按需
逐层拉的。硬套就得先把整棵树拉下来，那正是后端刻意不做的事。

**只读**——不做删除/改写/上传。那是另一件事，风险完全不同（一个误点就能毁掉
agent 正在用的工作区），要做也该单独设计与单独定权限。

#### 终端是真 PTY，经 amad 中转

xterm.js 连 amad，amad 在会话容器里 `docker exec -it bash -l` 起一个真 PTY。
于是 `vim` / `top` / `Ctrl-C` / 彩色输出 / Tab 补全全都能用。
浏览器侧优先走 WebSocket（一次性票鉴权，见 `server/core/sandboxtermws.go`），
网关不支持 WS 升级时退回「SSE 下行 + POST 上行」（`server/core/sandboxterm.go`），
跨实例的按键经 PG 队列转给持有 PTY 的那个实例。

**为什么必须经 amad，不能让浏览器直连容器**：直连等于把 Docker daemon 的访问能力交给
浏览器——**绕过 AMA 自己的授权层**、无法审计。经 amad 之后每次握手都过一遍 authz。

⚠️ 这条路依赖 `httpd` 的 `statusRecorder` **透传 `Hijack`**。日志中间件包了一层
ResponseWriter，不透传的话 WebSocket 升级会失败，而报错指向 websocket 库、
完全不指向中间件（与 `Flush` 之于 SSE 是同一类问题，两个都有测试钉住）。

xterm 走 `React.lazy` 懒加载：包约 1 MB（零传递依赖），构建后是一个独立 chunk
（83 KB gzip），不进主包。

⚠️ **终端组件不能挂在 `status === "running"` 下面。** 容器空闲 60 秒就自动暂停，
那一刻整个组件被卸载、React 状态跟着销毁，用户已经敲出来的历史输出全没——
而接口一直是好的，只查后端永远查不到。现在终端常驻、由它自己显示禁用态，
21-sandbox 里有一条回归用例钉住。

#### 终端：owner/admin 才能进，且必须告知风险

⚠️ 这一档是 `ActionManage`（owner/admin），**member 与 viewer 都进不去**。理由不是
「执行命令危险」这种泛泛之谈，而是具体的：**工具调用时容器里会注入授权凭据**
（AMA token、飞书用户 token 等，见 `shared/infra/shelltools/credential.go`），
进容器的人可以读到工作区里 agent 留下的东西，并在那台机器上以平台身份执行命令。
那是**身份借用**，不是「多一点权限」。

所以页面上有两道：权限门槛，以及一条**在打开终端之前就可见**的常驻警示
（不折叠、不是一次性提示）。工具输出出口会把注入的凭据抹成占位符——但那只挡
「顺着输出原样流出来」，挡不住刻意外带（base64、curl），权限门槛才是真正的防线。

除了终端那条，还保留一条 `POST .../sandbox/shell`（一条命令一次请求）：
它不需要长连接，适合脚本化调用与 e2e。

多实例下天然安全：shell 状态活在**容器里**，既不在浏览器也不在某个 amad 进程里。

### 3.7 定时任务 —— 主动式能力

在此之前 agent 只会被动应答（对话页发消息、飞书群里 @ 它）。这一节给它主动性。

```ts
type ScheduleKind = "cron" | "interval" | "oneshot";
type ScheduleOrigin = "user";
type DeliveryKind = "run_only" | "feishu";
```

**一次触发就是一条 Run**，`trigger` 为 `schedule`。与 3.6.3 对话页同一个手法：刻意
**不新增执行记录实体**，于是白拿三件事——自动进运行历史、能看完整的 33 类事件时间线、
能从历史点回来续聊。`Schedule` 上只留 `lastRunId` 指过去。

`RunTrigger` 因此从 4 值扩到 5。

⚠️ **`ScheduleOrigin` 现在只剩一个值**：曾经还有 `agent_self`（agent 自唤醒排的），
配套 `trigger` 的第 6 个值 `wakeup`。**那个功能已整个删除**（见下面「自唤醒（已删除）」）。

#### 三种触发方式的语义差别

| kind | 下一次什么时候 | 备注 |
| --- | --- | --- |
| `cron` | 按表达式在**指定时区**里算 | 默认 `Asia/Shanghai`——「工作日 9:30」写下来就是本地时间 |
| `interval` | 从**上一轮跑完**起算 | 不是固定网格：按网格排会在慢的那轮之后立刻追一次，把间隔吃掉 |
| `oneshot` | 就是 `fireAt` | 过期也照跑一次（停机期间的提醒该补发），**跑完即删** |

#### 投递按来源走

`DeliveryTarget` 在建任务时**冻结**下来：到点时是一轮全新会话，没有「当时那条消息从哪来」
的上下文，不冻结就只能让 agent 猜投递地址——而它猜错不报错，只是把报表发到了别的群。

「从飞书话题来的发回话题」的真正落点是 `anchorMessageId`：飞书的发消息接口只收
`chat_id`，要落进话题必须**回复话题里的某条消息**。光有 `threadId` 发不进去。

群从哪来：**入站消息经过时顺手记一笔**（`feishu_chats`）。让用户手填 `oc_xxx` 的话他要
自己去飞书翻 ID，且填错了不报错、只是报表一直发不出去。

#### 自唤醒（已删除）

曾经有一个 `schedule_wakeup` 自定义工具：agent 启动长跑作业（CI、部署）之后不原地
sleep，而是给自己排一个一次性任务、干净结束当前 turn，到点被唤醒再看结果。

**已整个删除**，因为 agent 把它当轮询用了：一个只配了「每小时一次」的定时任务，
近 200 条运行里出现 **84 次自唤醒**，最密时段稳定 3.5 分钟排一轮。每次自唤醒
= 一个新会话 + 一条新 work + 一个新 runner，于是它成了方舟 429 限流的业务侧放大器。

**别照着「加个开关/加个频率上限」把它加回来**：真正的问题不是频率没上限，而是
「让模型自己决定什么时候再跑一轮」这件事没有反馈闸门——模型每一次都觉得「再等 3 分钟
看一眼」是合理的，而那个判断在它自己的上下文里永远成立。要等长跑作业，现在的做法是
把 task_id 与查看方式告诉用户，由用户下次再问（见 `shared/infra/shelltools/bash.go` 的后台任务
回执）。

#### 多实例下「谁来跑」

两道闸门：Redis 选主（**优化**，省掉 N-1 个实例的无用扫描，可选）+ PostgreSQL 原子认领
（**正确性**，必须）。只靠前者不行——Redis 整段可以留空，那时「没配 Redis」就直接等于
N 倍重复投递，而那个故障不报错。

## 4. 数据层设计

```
ApiClient (interface)
   ├── MockApiClient    ← 现在：localStorage 持久化 + 种子数据
   └── HttpApiClient    ← 将来：对接 server/ 的 Go HTTP 服务
```

前端只依赖 `ApiClient` 接口，所有方法返回 Promise 并带人为延迟，这样 loading 态是真的被走到过、而不是理论上存在。Mock 数据落 localStorage，刷新不丢，e2e 里可以清库重置。

种子数据必须包含**一条覆盖多种事件类型的运行**（含多线程、工具调用、MCP 调用、错误重试、outcome 评估），否则运行历史这块的复杂度无法验证。

---

## 5. 视觉与组件

基于 HeroUI v3（`@heroui/react`）+ Tailwind CSS v4。遵循 HeroUI 设计原则：语义化 variant 而非裸颜色类、克制的边框、surface token 建立层级、慷慨留白（`gap-4/6`、`p-6`、主内容区 `pt-8`）、数字用 `tabular-nums`、图标按钮必配 Tooltip、不用 emoji 当图标。

> **组件库**：只用免费的 `@heroui/react`（OSS）。事件时间线、对话流等复合组件在 `web/src/components/ui/` 下自行实现，不依赖付费的 HeroUI Pro。
>
>
> 对话页的消息流、气泡、加载态在 `web/src/components/ui/Chat.tsx`；输入区是 OSS `TextArea` + `Button` 自己搭的。

---

## 6. 验收路径

e2e 覆盖需求里的每一条：

1. 无 workspace → 被强制引导创建 → 创建后进入空间
2. 创建 agent，设置独立的提示词与模型 → 列表可见 → 详情可改
3. 创建技能 → 在 agent 上绑定 → 绑定关系持久化
4. 走设备码流程接入飞书应用（不手填凭证）→ 就绪度诊断可见 → 在 agent 上绑定 → 双向可见
5. 打开运行历史 → 事件时间线渲染多类事件 → 筛选/展开/token 汇总可用
6. 空间设置改 name、改 key、增删成员
7. 选 agent 发起对话 → 逐字看到结果 → 工具调用可展开 → 刷新可续 → 该对话出现在运行历史里

对话那条的确定性靠 `window.__amaApi.flushConversation()`：逐字出字对用户是功能，对 e2e
是不确定性，有这个钩子后用例既能断言中间态（不调）也能断言最终结果（调）。
与 `completeFeishuAuth()` 是同一个用意，见 `guides/frontend.md`。

# MA 约定基线

**这份文件是 MA（方舟 Managed Agents）OpenAPI 约定的一份快照**，记着它怎么写 spec、
AMA 采纳了哪些、以及**哪些刻意不采纳、为什么**。`conventions_test.go` 读它做对账。

读它的时机：要动 `server/openapi/` 的表达形状之前，或者有人问「为什么 AMA 这里
和 MA 不一样」。

## 为什么是一份手工快照，而不是实时拉取

MA 的 spec 是方舟那边的产物，**不随本仓库提供**，CI 里也不保证拉得到：

| | |
| --- | --- |
| 产物 | `openapi/<api>/openapi.yaml`，按服务分 14 份，OpenAPI **3.1.0** |
| 真源 | `typespec/<api>/`，`make openapi` 生成（**那边的 yaml 不手写**） |
| 快照日期 | 2026-08-13 |

照着「本机能拉到」设计出来的检查会在 CI 上失败，**而失败信息看着像网络问题**——
那种报错不指向真实原因。所以基线固化成这份文件。

### 代价：这份基线会过期，而且不会有东西提醒你

**MA 改了约定，这边不会自动变红。** 这是接受的取舍（拉不到就只能如此），
但必须写清楚，否则下一个人会以为它自动跟着 MA 走，然后基于三个月前的约定做判断。

所以：**要更新它得有人去重读 MA 的 spec**，对着上面那个快照日期看 diff，再改这份文件。
`conventions_test.go` 只保证「AMA 的 spec 与**这份基线**一致」，
不保证「这份基线与**今天的 MA**一致」。

⚠️ 这里刻意**没有**做成「拿不到 MA 的 spec 就 `t.Skip`」：跳过时是绿的，
日志里没有任何东西提醒你它没跑。所以本包的检查只读进程内的东西——
磁盘上的 spec 与这份文件，没有一条会因为环境不同而跳过。

## MA 的约定（逐条，附证据）

证据是 MA spec 里的路径与行号，按上面那个快照。

| 维度 | MA 的做法 | 证据 |
| --- | --- | --- |
| operationId | `Service_verb`：`Agents_create` / `Agents_list` / `Agents_retrieve` / `Agents_update` / `Agents_remove` / `Agents_listVersions` | `openapi/agent/openapi.yaml:9,27,45,61,84,102` |
| 更新用什么方法 | **POST**，`POST /agents/{agentId}` 就是 `Agents_update` | `openapi/agent/openapi.yaml:60-61` |
| 全仓的方法分布 | **29 get / 26 post / 10 delete，零 PATCH、零 PUT** | 14 份 yaml 数出来的 |
| 字段命名 | snake_case 全局统一：`created_at`、`updated_at`、`mcp_servers`、`skill_id`、`input_schema` | `openapi/agent/openapi.yaml:213,237,240,279,512` |
| 列表响应 | 包装 schema `ListAgentsResponse`：`data` 数组 + `next_page` + `total` | `openapi/agent/openapi.yaml:343-360` |
| 删除响应 | 包装 schema `DeleteAgentResponse`（只有 `id`） | `openapi/agent/openapi.yaml:334-342` |
| 请求体 | 独立 schema `CreateAgentRequest` / `UpdateAgentRequest` | `openapi/agent/openapi.yaml:285,526` |
| query 参数 | 抽到 `components.parameters`，用 `$ref` 引；**按操作分开命名**（`ListAgentsQuery.limit` 与 `ListAgentVersionsQuery.limit` 是两条） | `openapi/agent/openapi.yaml:31-34,122-171` |
| 分页参数 | `limit` / `page` / `created_at_gte` / `created_at_lte` | `openapi/agent/openapi.yaml:139-171` |
| 资源自描述 | 每个资源有 `type` 字段，固定单值 enum（`type: agent`） | `openapi/agent/openapi.yaml:186-190` |
| id 格式 | 写进 description：`agent_<opaque>`、`skill_01XJ5...` | `openapi/agent/openapi.yaml:185,280` |
| 通用字段 | `metadata`（map，≤16 对，key ≤64、value ≤512）、`tags` 数组 | `openapi/agent/openapi.yaml:227-236` |
| 文案 | `summary` 英文（`Create an Agent`）、`description` 中文（`创建一个新的 Agent。`） | `openapi/agent/openapi.yaml:10-11` |

MA 的保鲜机制是一条 CI 检查：改动碰到 `typespec/**`、
`openapi/**`、`Makefile` 就重新生成再 `git diff --exit-code`，有 diff 就红并给出本地
修复命令。形状是「**重新生成 → 与提交的比 → 不一致就拦 MR**」——AMA 这边等价的东西
是 `TestSpecFileIsUpToDate`，早就有了。

## AMA 采纳了哪些

已经落地（`conventions_test.go` 逐条钉住）：

| 约定 | AMA 现在长什么样 |
| --- | --- |
| operationId `Service_verb` | `Agents_retrieve` / `WorkspacesArkCredential_verify` / `SkillsRevisions_rollback` |
| verb 用 MA 的词表 | create / list / retrieve / update / remove |
| query 参数抽成 `$ref` | `#/components/parameters/Suggestions_listQuery.limit`，27 条，按操作命名（跟 MA 一样不合并） |
| 每个路径参数都有说明 | 含 id 格式（`ws_example` / `agent_example` / `run_example`）与「取值从哪来」 |
| summary 不是占位 | 此前 55 条 summary 是「方法 + 路径」，等于把路径重复一遍 |
| OpenAPI 版本 | 3.1.0，与 MA 同 |

## AMA 刻意不采纳哪些，以及为什么

**这一节是重点。** 差异分两种：**想清楚了才留下的**，和**没注意到才留下的**——
这两种在 spec 里长得一模一样，而后者会被下一个人当成前者（「既然一直是这样，
想必是有原因的」）。所以每条都写下理由。

### 1. 字段名保持 camelCase（MA 是 snake_case）

**这是本仓库最大的一颗雷。** **9 张表**把整个实体序列化进 `doc jsonb`
（`agents`、`ark_bindings`、`ark_secrets`、`feishu_apps`、`feishu_sessions`、
`run_events`、`runs`、`schedules`、`skills`），
所以 **`apimodel` 的 json tag 就是数据库里的键名**——
`repository/schema/agents.sql:27` 那行注释写得很直白：

> `Keys of the doc column (jsonb) (= json.Marshal(Agent))`

把 `createdAt` 改成 `created_at` 会**同时**坏三件事，**三件都零报错**：

1. **存量行读出空串**，而**全部测试照样绿**——测试写完就读，键是一致的，
   所以测试根本碰不到「旧键写进去、新键读出来」这个组合。
2. **GIN 索引永不命中**：`agents.sql:77` 是
   `create index agents_feishu_idx on agents using gin ((doc -> 'feishuAppIds'))`。
   改键名之后查询**照样返回正确结果**，只是全表扫——**慢而不错**，
   没有任何东西会报警。
3. **存量数据迁移静默变成空操作**：`agents.sql` 里那批
   `update agents set created_at = doc ->> 'createdAt' where ...`
   会一行都匹配不到，于是迁移「跑成功了」但什么也没搬。

`doc->'键'` 这样的引用全仓 **117 处**（`.go` + `.sql`），散布在三类地方：
DDL 索引与生成列、生产 SQL（`repository/dao/coredao/store_pg.go` 里的
`doc->>'lastActivityAt'` / `doc->>'trigger'` / `doc->>'status'`）、以及存量迁移。

**结论：任何 json tag 重命名都必须配一套数据迁移 + 索引重建 + 「缺键 ≠ 空串」的校验。
拿不出方案就别改。**

⚠️ **数 `doc jsonb` 表时别裸 grep。** `grep 'doc jsonb'` 给 16 个文件，但其中 7 个是
「本表**没有** doc jsonb」的说明性注释——真有那一列的是 9 张。判据要看
`create table` 语句体里有没有 `^\s*doc\s+jsonb`。

⚠️ **`cmd/ama-cli` 不受编译保护，这是重命名最阴的一面。** 它是单文件 969 行、
**一次都没有 import `apimodel`**（`grep 'apimodel\.'` = 0），字段名全部以
`putIf(draft, "status", …)` 这样的**字符串字面量**出现（约 22 处）。
所以 Go 侧改 tag 时它**编译全绿**，然后在容器里静默失效——
而那是 agent 正在用的命令行工具。

### 2. 保留 PATCH（15 条）与 PUT（7 条）

MA 全仓零 PATCH 零 PUT，更新一律 POST。不跟的两个理由：

- 那是 **wire format**，改它要动全部前端调用方与 e2e，而收益只是「方法名跟 MA 一样」；
- **PATCH 携带「部分更新」这个信息**，换成 POST 就表达不出来了。

spec 里用两个 verb 把这个区别保留下来：`update`（PATCH，部分更新）与
`replace`（PUT，整体替换）。两者合成一个词会让「传了一半字段会不会清空另一半」
这件事无从表达。

⚠️ 有一条值得知道的例外：`PATCH /api/agents/{id}` 的 description 里明写着
它**按全量更新处理**（前端总是回传完整对象）。所以那条的方法名与语义本来就有点错位，
但那是既成事实，不是这次要动的。

### 3. 时间格式保持固定三位小数的 ISO 8601，不采纳「RFC 3339」

MA 的 `created_at` description 写的是「RFC 3339 时间」。

⚠️ **对齐命名（`created_at`）与对齐格式（RFC3339）是两件事，别一起做。**

Go 的 `time.RFC3339` 与 `RFC3339Nano` **都会把 `.000` 抹掉**，于是同一时刻在库里出现
两种写法。这不是好看不好看的问题：`listRuns` 是按 `startedAt` 做**字符串**降序排的，
而 `.`(0x2E) < `Z`(0x5A)，**两种形状混在一起会直接改变排序结果**。

AMA 的规矩是固定三位小数的 ISO 8601（`domain.NowISO()`，`2026-07-26T10:00:00.000Z`），
CLAUDE.md 里明确禁止 `time.RFC3339`。**这一条 AMA 是对的，MA 的描述只是更宽松**
（RFC 3339 允许有没有小数位，两种都合法）。

### 4. 不给 spec 加 `type` 字段、不加 `ListXxxResponse` 包装

⚠️ **判据在 wire 上，不在好看上。**

- 列表接口返回的是**裸数组**：`apihandler/handler_agent.go:50` 是
  `writeJSON(w, http.StatusOK, list)`，不是 `{"data": [...]}`；
- 资源上**没有** `type` 判别字段（`apimodel.Agent` 里没有这个字段）。

**只在 spec 里补这两样，spec 就成了谎言。** agent 照着读会去取 `resp.data`
拿到 `undefined`，然后去查自己的代码——**而那个错误不指向真实原因**
（「文档描述的形状和服务端实际吐的不是一回事」）。

**宁可保留一个诚实的差异，也不要一个好看的假描述。**
要真对齐它们就得改 wire format，那是破坏性改动：动 `apimodel`、动全部前端读取点、
动全部 e2e。这次没做。

### 5. 方舟透传字段保持方舟的 snake_case 名

**这是一条规则，不是待清理的不一致。而且它已经被文档化了两处**，
所以「AMA 的字段名风格不统一」这个判断本身就是错的：

> `model/apimodel/apimodel.go:4-9`（包注释）：
> 镜像方舟的字段用 snake_case（与方舟 API 一致，便于透传与对照文档），AMA 自有概念用
> camelCase。看到 snake_case 就知道这条数据的权威来源在方舟。**改字段名前先读
> `docs/DESIGN.md` 第 3 节。**

`docs/DESIGN.md:121` 逐字同款，并写明「这个区分是刻意的」。

**符合率 240/241**：241 个 distinct 线上字段名里 10 个是 snake_case，其中 9 个有方舟出处。

| 字段 | 位置 | 方舟出处 |
| --- | --- | --- |
| `session_thread_id` | `apimodel/events.go:63` | 注释明写「对应方舟的 session_thread_id」 |
| `input_tokens` | `apimodel/apimodel.go:1864` | 由方舟的 `model_usage` 累加 |
| `output_tokens` | `apimodel/apimodel.go:1865` | 同上 |
| `cache_read_input_tokens` | `apimodel/apimodel.go:1866` | 同上 |
| `cache_creation_input_tokens` | `apimodel/apimodel.go:1867` | 同上 |
| `agent_name` | `apimodel/apimodel.go:1874` | 方舟的线程字段 |
| `parent_thread_id` | `apimodel/apimodel.go:1879` | 同上 |
| `reasoning_effort` | `apimodel/apimodel.go:528` | `shared/infra/ma/` 7 处、ark 文档 7 个文件 |
| `service_tier` | `apimodel/apimodel.go:529` | 同上 |
| `builtin_tools` | `apimodel/apimodel.go:1253`、`:1412` | ⚠️ **没有**，见下 |

⚠️ **`builtin_tools` 是这条规则里唯一的真偏差，别把它当成「方舟字段」照抄。**
实测：`docs/ark-managed-agents/` 里 `built` 这个词根**一个文件都搜不到**，
`shared/infra/ma/` 里也没有任何 builtin 相关字段——**它从不发给方舟**，
是 AMA 自有概念，按上面那条规则本该叫 `builtinTools`。

**但它恰好是代价最大的那个，所以不在这轮里改**：
`agents.doc` 里的存量行带的是旧键，改名后读出空切片，而
`apimodel` 的第一条 JSON tag 铁律要求这个切片**必须非 nil**——
前端 `.length` 会直接 TypeError。要改就得带 doc 回填迁移，单独立项。

⚠️ **`RunThread`（`apimodel.go:1870-1883`）一个 struct 里两种风格并存，且各有理由**：
`agent_name` / `parent_thread_id` 来自方舟，`startedAt` / `endedAt` 是 AMA 自己的。
所以「把这个结构体的风格统一」没有正确答案——**统一了反而两边都不对**。

**把方舟那 9 个改成 camelCase 才是引入 bug**——那会让透传字段与上游对不上，
而这类错位通常表现为「某个值一直是空的」，不报错。

前端那侧的分域是同一条规则的实体化，可以当机械判据用：
`web/src/types/events.ts`（方舟事件镜像，29 个 distinct snake 字段）
vs `web/src/types/domain.ts`（AMA 自有，630 处 camelCase 声明）。

判据与 CLAUDE.md 那条「外部系统的既有名字照原样用」一致：
能在方舟文档里搜到它，就别换成「自己的说法」。

## 路径集合本身无法相等

「一毛一样」不可能字面成立，所以它只能指「在两边都有的概念上用同一套约定」。

| | 有什么 |
| --- | --- |
| **AMA 有、MA 没有** | workspaces / members / issues / suggestions / feedback / api-keys / feishu / integrations / schedules / user-preferences / user-onboarding / ark-fetch |
| **MA 有、AMA 不暴露** | chat / embedding / responses / tokenization / images / content_generation / multimodal_embedding / file |
| **两边都有（真正该对齐的）** | agent、session↔run、skill、environment、vault/credential、memory |

AMA 那些是平台自己的能力（空间、成员、问题、建议这些 MA 压根没有的概念）；
MA 那些是模型推理接口，AMA 不转发它们。所以对齐的对象只能是**表达约定**，
不是路径清单。

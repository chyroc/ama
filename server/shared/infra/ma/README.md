# ma —— 方舟 Managed Agents 接入层

基于官方 SDK [`github.com/volcengine/ark-runtime-go`](https://github.com/volcengine/ark-runtime-go)
（v0.4.0）封装，把 Managed Agents 的能力组织成「一个实例 + 六个子服务」。

## 用法

传一个 API Key new 出实例，之后所有操作都挂在这个实例上：

```go
ins, err := ma.New(os.Getenv("ARK_API_KEY"))
if err != nil {
    return err
}

// 1. Agent：智能体配置
ag, err := ins.Agents().Create(ctx, ma.AgentSpec{
    Name:   "triage",
    Model:  "doubao-seed-2-1-pro-260628",
    System: "你是一个乐于助人的助手。",
    Tools:  []string{ma.ToolsetDefault},
})

// 2. Environment：运行环境（默认托管沙箱 + 放开出网）
env, err := ins.Environments().Create(ctx, ma.EnvironmentSpec{Name: "default"})

// 3. Session：把 Agent 绑到环境上
sess, err := ins.Sessions().Create(ctx, ma.SessionSpec{
    AgentID:       ag.ID,
    EnvironmentID: env.ID,
})

// 4. 发消息，等 Agent 跑完
res, err := ins.Sessions().Run(ctx, sess.ID, "最高的山是哪座？")
fmt.Println(res.Text)
```

可运行的完整示例见 [`example/main.go`](example/main.go)。

## 子服务

| 入口 | 职责 | 文件 |
| --- | --- | --- |
| `ins.Agents()` | Agent 定义、版本 | `agents.go` |
| `ins.Environments()` | 运行环境 | `environments.go` |
| `ins.Sessions()` | 会话生命周期 | `sessions.go` |
| | 事件收发、历史回放 | `session_events.go` |
| | SSE 流、`Run` 组合操作 | `session_stream.go` |
| | 挂载资源 | `session_resources.go` |
| | 线程 | `session_threads.go` |
| `ins.Vaults()` | 保管库 | `vaults.go` |
| | 凭证 | `credentials.go` |
| `ins.MemoryStores()` | 记忆库 | `memory_stores.go` |
| | 记忆条目 | `memories.go` |
| `ins.Skills()` | 技能包上传 | `skills.go` |
| `ins.RawDo` / `ins.RawStream` | 字节级原样透传（任意方舟 HTTP 接口，含 multipart 与 SSE） | `raw.go` |

## 字节级透传（RawDo / RawStream）

六个子服务之外还有一个**不做类型映射**的通道：给它什么字节就发什么字节、
响应读到什么就回什么，不经过 SDK 的 struct 反序列化。用于 AMA 要把方舟接口
原样转给上层调用方的场合——多一次 JSON 重编码，方舟新加的字段就会被静默丢掉。

```go
resp, err := ins.RawDo(ctx, ma.RawRequest{
    Method: http.MethodPost,
    Path:   "/sessions/sesn-xxx/resources",
    Query:  url.Values{},                 // 含 created_at[gt] 这类带方括号的参数也原样拼
    Body:   []byte(`{"type":"file","file_id":"file-1"}`),
})
// resp.StatusCode / resp.Header / resp.Body 全是方舟原样；
// 方舟回 4xx/5xx 时 resp 与 *ma.APIError 一起返回（错误体也在 resp.Body 里）。
```

multipart 上传（Files API）也走 RawDo：自己用 `mime/multipart` 拼好整段报文，
把 `writer.FormDataContentType()`（boundary 在里头）放进 `RawRequest.ContentType`。
SSE 用 RawStream，返回未读取的流、由调用方 Close。

只有 `ma.New` 造的实例支持透传；`ma.NewWithClient` 收自造 client、拿不到
baseURL，调透传方法返 `ErrRawUnsupported`。

## 设计取舍

**薄封装。** 便捷入口（`AgentSpec`、`SendText`、`Run`）覆盖常见路径；每个方法都有
配套的 `*Raw` 版本直收 SDK 原生请求体，所以封装没覆盖到的字段永远不会挡路。
返回值一律是 SDK 的原生 struct，不做二次映射——SDK 升级新增的字段自动可见，
也省掉维护一整套平行 DTO 的成本。

**空值即未设置。** SDK 用 ogen 生成的 `OptString` 区分「没传」和「传了空串」，
两者在服务端语义不同。便捷入参里留空表示不下传，需要显式清空时走 `*Raw`。

**`Run` 封掉了两个时序坑。** 一是必须先开流再发消息，否则会漏掉 Agent 最早的
事件；二是必须识别终止事件（`session.status_idle` / `status_terminated` / `error`）
才能退出循环，否则会一直阻塞到 context 超时。这两点在
`session_stream_test.go` 里用假 SSE 服务端做了验证。

**`WithTimeout` 不作用于流式接口。** SSE 长连接的生命周期由传入的 context 控制，
用 `context.WithTimeout` / `WithCancel`。

## 模型清单

**真源是方舟平台本身，不是文档、也不是 SDK。** 拿一把真的方舟 API Key，
对每个模型 × 每个档位真建一次 Agent 问出来（必要时再真发一轮消息，验模型确实能推理）。

为什么只有「建 Agent」算数——三个来源实测下来的结论：

| 来源 | 能回答什么 | 为什么不够 |
| --- | --- | --- |
| `GET /api/v3/models` | 模型 ID 存不存在 | 是方舟**全量**目录（实测 526 条，含视频/图片/ASR/内部测试模型），`features` 里**没有** `reasoning_effort` / `service_tier` 元数据 |
| SDK | —— | **没有列模型的接口**；`agent.ModelConfig` 里甚至没有 `ServiceTier` 字段（只有 `Speed`） |
| `POST /api/v3/agents` | **档位到底收不收** | 就是它。方舟按模型元数据校验，不支持就 400 `InvalidParameter` |

建 Agent 成功时方舟会把**解析后的默认值**回显在 `model` 里
（`{"service_tier":"default","thinking":"enabled","reasoning_effort":"high"}`），
`MODEL_SPECS.defaultReasoningEffort` 就是那个回显值——不用猜。

### 当前 10 个模型：9 个与文档镜像逐格一致，1 个文档尚未收录

`deepseek-v4-1-flash-261017` 是后者：实测可建 Agent、真推理正常，档位 `reasoning_effort=[none,low,high,max]`（默认 high）、
`service_tier=[auto,default]`，塞伪造值会被拒；而官方文档当时还没收录。
平台目录里同族还有 260901 / 260910，档位完全一样，清单只收最新的 261017。
依据同时记在 `web/scripts/check-models.mjs` 的 `KNOWN_DOC_LAG`。

一致是**结论不是前提**：文档正文自己写着「模型支持范围可能随模型版本发布而变化」，
而且方舟文档被实测证伪过（`thinking` 那篇说不返回思考内容，实测 69/72 条带 content）。
所以清单要改时先对真平台实测，别照抄文档。

### 坑：`-ga-` 差一个连字符是两个模型

`deepseek-v4-pro-260425` 与 `deepseek-v4-pro-ga-260813` 是**两个不同模型**，
实测档位也不同（前者不支持 `none`，后者支持）。前者曾经漏在 `MODEL_SPECS` 外面
——review 时极容易看成同一个。

### 坑：「方舟收下这份配置」不等于「这个模型能用」

`-catalog` 扫出 5 个文档没列但能建出 Agent 的模型，`-infer` 真发消息后
**3 个卡住超时**（`deepseek-v4-pro-ga-260801`、`deepseek-v4-flash-260731`、
`doubao-seed-2-0-lite-260415`，跨两次运行稳定复现）。建 Agent 只校验配置，
endpoint 有没有推理能力要真发一轮才知道。**所以它们没有进清单。**

### 坑：没有元数据的模型「支持全部档位」是假象

`glm-5-3-260814`（比文档里任何模型都新）接受全部 7 个 effort + 全部 3 个 tier，
看着像最宽松的模型。**其实是它根本没注册元数据，方舟跳过了校验**——
决定性的判别手段是塞一个不存在的值：

```
reasoning_effort=totally-bogus-value  →  glm-5-3-260814 接受（无元数据）
                                         glm-5-2-260617 拒绝（有元数据，会校验）
```

它能推理（回复正常），但档位全无保护、任何值都能提交，等于把校验责任推给前端。
**不进清单**，等方舟补上元数据再说。

### 坑：模型在清单里，不等于每个方舟账号都开通了它

模型是**按账号逐个开通**的：同一个模型 id，这个账号能建 Agent，别的账号没在方舟
控制台开通就被 **400 `InvalidParameter`** 拒，原话：

```
model.id: endpoint "doubao-seed-evolving" is invalid:
get endpoint meta doubao-seed-evolving: service not open
```

典型场景：方舟 key 换成另一个账号的，某 agent 用的模型新账号没开通，换 key 触发
绑定重建 → CreateAgent 吃这个 400；没人认的话会落进 handler default → 用户只看到
500「服务内部错误」。

所以**清单不能按「哪个账号建失败了」删**——`doubao-seed-evolving` 在官方文档
清单里、开通过的账号也能用，它就是合法条目。这个 400 由 `ma.ModelEndpointNotOpen`
识别（code + message 里的 `service not open` + endpoint 名三者凑齐），
`arkFailure` 翻成 422「去控制台开通模型 / 换个已开通的模型」。
**换全局凭证后要想到这一层**：新账号开过的模型集合可能和旧账号不一样。

### 防漏同步：对账脚本，不是 `GET /api/models`

`web/npm run check-models`（`web/scripts/check-models.mjs`，已进 `npm run build`）
拿 `MODEL_SPECS` 与文档镜像那张表**逐格**比：模型集合、档位集合、默认值、
`service_tier`、`thinking`，外加两条自洽性检查（默认档位必须在支持集里）与
一条种子检查（`core/seed.json` / `web/src/data/seed.ts` 里的模型 ID 必须在清单里）。

为什么不做 `GET /api/models`：**后端根本不认识模型。** `model.id` 从 HTTP 请求体
到 domain 到 SDK 全程是自由字符串，`core/agent.go` 的 `validateAgentDraft` 只校验
非空，PG 里存 jsonb 没有 CHECK。加那条路由等于把手写清单从 TS 搬到 Go：维护量没少，
多一条路由要在 `authz/matrix.go` 声明，还会让前端丢掉 `ModelId` 联合类型
——而 AgentsPage 的档位联动正是靠它在**编译期**挡住非法组合的。搬家不解决漂移。

为什么不「从文档生成」：文档不是真源（见上），把生成的输入定成一个会过期的快照
等于把错误固化。文档镜像在对账脚本里的角色是**跳闸器**——它一变脚本就红，
逼人去对真平台确认。

### ⚠️ `reasoning_effort` 与 `thinking` 当前没有下发给方舟

前端存了、也回显了，但 `core/chat.go` 的 `arkSpecFor` 只翻译 `service_tier`
（`"fast"` → 方舟的 `model.speed`），**`reasoning_effort` 与 `thinking` 两个字段
从未进入建 Agent 的请求**——`ma.AgentSpec` 里没有这两个字段，尽管 SDK 的
`agent.ModelConfig` 有 `ReasoningEffort`。表现是「界面上调了档位，实际推理没变」，
不报任何错。本次改动没动这条链路（补清单是另一件事），记在这里免得下次又当新问题查。

## 踩过的坑

**SSE 长连接必须显式覆盖 SDK 的默认超时，「不设 WithTimeout」不等于「没有超时」。**
SDK 的 `NewClientConfig` 自带 `defaultTimeout = 10 * time.Minute`
（`arkruntime/config.go`），而 `StreamSessionEvents` 用的就是同一个
`config.HTTPClient`（`arkruntime/sessions.go`）。`http.Client.Timeout` 管的是
「建连到读完 body」的总时长，SSE 的 body 永远读不完，于是**任何超过 10 分钟的对话
必定断流**，报 `context deadline exceeded (Client.Timeout or context cancellation
while reading body)`，前端收到 `session.error` 渲染成错误条目。

线上真炸过：一晚 13 次「对话流中断」，`cost` 全部精确卡在 `10m0.00x s`——**误差毫秒级
是这个坑的指纹**，用户关页面导致的断开是随机时长。当时代码里那条「不给 WithTimeout
是因为同一个 client 也用于 SSE」的注释把机制说对了、结论下反了，反而挡住了排查。

所以 `New` 造**两个** client：普通请求一个（保留超时），SSE 一个
（`newStreamHTTPClient`，`Timeout: 0`，生命周期交给 context）。
`streamConfigOptions` 里 `WithHTTPClient` 必须排在最后且不下传 `WithTimeout`——
SDK 的 `WithTimeout` 是往 `config.HTTPClient` 上写字段，顺序反了会把超时写回流式
client，修复静默失效。走 `NewWithClient` 只有一个 client，要跑长流得自己把
`Timeout` 设成 0。

**方舟把语义完全不同的失败都归到 HTTP 400，判别必须看 `code` 字段而不是状态码。**
`APIError.Code` 才是那个稳定标识（`errors.go` 的 `ErrorCode` / `IsRequiresActionRejected`）。

真出过一次：self_hosted 下工具跑在我们自己的容器里，方舟发出 `tool_use` 到 worker
回传 `user.tool_result` 之间有个窗口，窗口内发 `user.message` 会被拒——
HTTP 400 + `code=RequiresActionRejected`。`core.arkFailure` 当时只特判了 401/403，
于是它落进 `handler.fail` 的 default 分支，用户看到 **500「服务内部错误，请稍后重试」**。
三重误导：说得像服务挂了、没说该怎么办、500 还污染错误率告警。
而日志里三行相隔 155 毫秒就把真相说尽了——工具结果在报错 **48 毫秒**后就回传了：

```
20:35:32.287  工具执行完成 tool=bash took=116ms
20:35:32.394  ERROR 接口处理失败 → 400 RequiresActionRejected
20:35:32.442  tool event handled tool_use_id=call_yt9... posted=true
```

agent 连续调工具时这个窗口反复出现，用户手快就会撞上，**所以它是常态而不是故障**，
现在翻成 409 +「上一个工具还在执行，请等它跑完再发」。

⚠️ **别图省事按 400 判**：「事件格式非法」同样是 400，但它重试多少次都不会好。
混为一谈会让用户对着一个永远不会成功的操作反复重发。这条由
`TestIsRequiresActionRejected` 里那个「同为 400 但语义不同」的用例钉住。

**404 也有两个互相打架的含义，靠 `code` 区分：资源被删 vs 账号没开通 Managed Agents。**

全局方舟凭证被换成一把属于另一个账号的 key，而该账号没在方舟控制台开通
Managed Agents。所有继承全局凭证的空间建环境时撞：

```
HTTP 404 code=ManagedAgentNotOpen
Your account 2100000001 has not activated the managed agent.
Please activate the service in the Ark Console.
```

它与「绑定指向的 Agent/环境被人在控制台删了」**同为 404**，而正确反应相反：

| 含义 | code | 正确反应 |
| --- | --- | --- |
| 资源被删 | 空 | 清掉失效绑定重建（`chat.go` 的自愈分支，问 `ma.IsNotFound`） |
| 账号没开通 | `ManagedAgentNotOpen` | 422 告诉用户去控制台开通，**不能**删绑定重建 |

当时 `arkFailure` 不认识这个 code，落进 `handler.fail` default 又是 **500「服务内部错误」**
（与 RequiresActionRejected 那次同款三重误导）；更隐蔽的是 `CreateSession` 的
「404→删绑定自愈」也会吞掉它：绑定没坏，删了重建造一对孤儿资源，再撞同一个 404。
现在 `ma.IsManagedAgentNotOpen` + `ma.NotOpenAccountID`（从 message 提账号 id）
识别它，`arkFailure` 翻成 422 并带上账号 id 与 request_id，自愈分支显式排除它。

⚠️ 换全局/空间凭证会触发 `chat.go` 的「凭证已更换→重建方舟资源」，所以**一把未开通
账号的 key 配到全局，所有空间（含绑好好久的老 agent）会在下一次开对话时一起挂**，
不是只有新建 agent 受影响。

**逐字段手写的 `Spec` 翻译层是个漏字段陷阱，漏了不报错、方舟照样 200。**

`AgentSpec` / `EnvironmentSpec` 这类便捷入参的价值是「不必让调用方拼 SDK 原生请求体」，
代价是**多了一处必须手工同步的字段清单**：`toRequest()` 里少写一行，那个字段就在
wire 上消失了，而三层都不会喊：编译器不管（结构体字段本来可以是零值）、
SDK 不管（少个可选字段而已）、方舟不管（返回 200 建成功）。

真出过：调用方 `core.LiveArkRunner.CreateAgent` 构造 `ma.AgentSpec` 时列了 7 个字段却
**漏掉 `Skills`**。本包这一侧完全无辜（`req.Skills = toSkillRefs(s.Skills)` 一直在），
但表现是「用户给 agent 配了 3 个技能，问它有什么 skill，它说没有」——
技能包上传成功、绑定记录完整，只有方舟 Agent 的 `skills` 是空的，
于是模型压根不知道有技能、**永远不会用**，且零报错。整段复盘在
`server/core/README.md` 的「配了技能，问 agent 有什么 skill 它说没有」。

这与 `AgentSpec.CustomTools` 和 `EnvironmentSpec.Type` 是**同一类**：
`Type` 留空会默认成 `cloud`，于是工具悄悄跑在方舟托管沙箱里而全链路无报错
（`core.EnsureEnvironment` 因此建完必须回读校验类型）。
共同点是**「少传一个字段」与「功能静默失效」之间没有任何报错**。

两条防线，缺一条都不够：

- 本包侧：`TestAgentSpecToRequest` 必须覆盖**每一个** `AgentSpec` 字段。
  它当年只断言 6 个、既没设 `Skills` 也没断言 `req.Skills`，所以那次漏传本包看不出来。
- 调用方侧：断言**实际发出的 HTTP 请求体**，而不是中间那层结构体。
  `core/arkrun_test.go` 用 httptest 起替身 + 反射遍历 `ArkAgentSpec` 的字段
  （`TestArkAgentSpecMapsEveryFieldToMA`），加了新字段却没接线会红并点出字段名。
  只断言 `ma.AgentSpec` 是不够的——那正是当年全绿的那一层。

顺带一条实测：**建 Agent 的响应体会回显 `skills`**，
所以「确认技能真的挂上了」不需要额外 `Get` 一次。

## 已知边界

- **Skill** 当前 SDK 只提供创建（multipart 上传）与查询详情，没有列表/删除接口。
- **凭证的 `Auth`** 是 union（API Key / OAuth 等多种形态），构造差异大，故不做
  便捷封装，统一走原生请求体。
- **自定义工具** 方舟本期暂不支持，只支持 MCP 工具与内置工具集。

## 测试

```bash
go test -race ./shared/infra/ma/
```

全部离线：参数校验、选项处理、入参翻译、错误判别，以及用 `httptest` 起假 SSE
服务端验证 `Run` 的并发时序。

`session_stream_timeout_test.go` 守上面那个超时坑：把 10 分钟的量级缩小成毫秒级
（机制不变，都是 `http.Client.Timeout` 覆盖读 body 的总时长），正向证明流能活过
普通请求的超时点，**反向再用「单 client」的旧形状跑同一个服务端确认它一定断**——
少了这条对照，正向那条测试在 bug 回归后可能因为跑得太快而假绿。

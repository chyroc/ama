# feishu —— 接入飞书应用

AMA 后端第一个业务模块。把「接入一个飞书应用」从「用户自己去开放平台建应用、抄
AppID、抄 AppSecret、再贴回来」变成「点个链接去授权」。

协议层在
[`shared/infra/lark`](../shared/infra/lark/)，本包只做业务编排。

> **Feishu is one channel of the channel layer** ([`server/channel`](../channel/README.md)).
> What is not specific to Feishu now lives there and is shared with the WeChat channel:
> session creation (`CreateChannelConversation`), running a turn (`channel.RunTurn`, used by
> `runAndReply`), the answer buffer (`channel.AnswerBuffer`), the long-connection supervisor
> (`Supervisor` here is a `channel.Source`) and the fixed fallback replies. Ark frame parsing moved to
> `shared/gopkg/arkframe`. Feishu still keeps its own dedup / scope / inflight tables. The rest of this
> README predates the move and is still in Chinese; function names like `agentText` / `turnEnded`
> below now refer to their `arkframe` equivalents.

## 分层

```
cmd/amad  →  httpd  →  feishu  →  shared/infra/lark  →  飞书开放平台
  进程        路由      业务         协议
```

依赖严格单向。本包不认识 HTTP（除 `handler.go`），也不认识飞书的协议细节。

| 文件 | 职责 |
| --- | --- |
| `feishu.go` | `Service` 装配、`Authorizer` 接口 |
| `onboarding.go` | 发起 / 轮询编排：鉴权、会话、双域纠正、幂等落库 |
| `readiness.go` | 就绪度诊断 + 巡检缓存 |
| `apps.go` | 列表、详情、断开 |
| `store.go` | 持久化契约 + 内存实现（PG 实现在 `repository/dao/feishudao`，见下文「两个 Store 实现」） |
| `app.go` | 领域模型 |
| `handler.go` | HTTP 边界：解码、错误映射 |
| `inbound.go` | 入站消息编排：去重、会话复用、「表情 → 思维链 → 独立答案」三部曲 |
| `trigger.go` | 触发判定：这条消息该不该让 agent 开口（含防自触发） |
| `reply.go` | 最终答案的投递：OnIt/DONE 表情、按表格数切段、等思维链封口的顺序门 |
| `turnprogress.go` | 一轮对话的思维链：开投递、抢租约、边跑边推、终态封口 |
| `progress.go` | 思维链投递的状态机模型与内存实现 |
| `progress_driver.go` | 把一条投递从 queued 推到 completed |
| `transport.go` | 两条投递路径：CoT 消息（主）与卡片摘要（降级） |
| `cotevents.go` | 方舟事件 → 快照 → AG-UI 事件 |
| `resume.go` | 服务重启后未完成轮次的恢复 |

## HTTP 接口

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/api/workspaces/{workspaceId}/feishu/apps` | 列表（含 readiness），`?refreshReadiness=true` 绕过缓存 |
| `POST` | `/api/workspaces/{workspaceId}/feishu/registrations` | 发起接入 |
| `GET` | `/api/workspaces/{workspaceId}/feishu/registrations/{sessionId}` | 轮询 |
| `GET` | `/api/feishu/apps/{id}` | 详情 |
| `POST` | `/api/feishu/apps/{id}/readiness/refresh` | 重新检测，返回整个应用 |
| `POST` | `/api/feishu/apps/{id}/admin-authorization` | 申请管理员授权 |
| `PATCH` | `/api/feishu/apps/{id}/trigger-mode` | 改触发方式，返回整个应用 |
| `DELETE` | `/api/feishu/apps/{id}` | 断开 |

错误体是 `{"message": "中文句子", "code": "machine_readable"}`：`message` 给用户看
（前端直接展示），`code` 给程序判断（将来做 i18n 时按它映射翻译键，不必解析中文）。

## 四个不能想当然改的地方

**一、轮询必须先鉴权，再查会话。** `Poll` 的第一句是 `requireManage`，**之后**才拿
sessionID 去 Store。顺序反了 sessionID 就成了事实凭证——谁拿到它谁就能推进注册流程、
拿到注册结果。会话 ID 虽然是 crypto/rand 生成的 187 位熵（见 `session.go`），但
「不可猜」不该是唯一防线。`TestPollAuthorizesBeforeTouchingSession` 锁住了这条。

**二、会话绑发起人。** 同空间的其他管理员也不能接管别人正在进行的注册，否则 A 的
授权结果会落到 B 手里。空间不匹配和发起人不匹配都返回 `ErrAccessDenied`，不区分
「会话不存在」和「不是你的」——避免把会话是否存在这件事透露出去。

**三、feishu / lark 双域自动纠正。** 国内与国际是两套独立租户体系，用户往往说不清
自己在哪边，而猜错域的表现不是报错，是**一直 pending**。poll 返回的 `tenant_brand`
会暴露真实归属，此时就地改会话的 region 继续轮询，不让用户重来一遍（他很可能再选错）。

**四、注册幂等。** 撞 appID 唯一键时回读已存在的行：归属仍是本人就当**重连成功**
（刷新密钥后返回），只有归属他人才报 409。用户重装、换机器、或者单纯又点了一次接入
都会走到这里，报冲突只会让人困惑。

## AppSecret 不会出现在响应里

不是靠 `json:"-"` 标签，是 **`App` 结构压根没有这个字段**。密钥只在 `Store` 内部和
巡检调用之间流动，序列化路径上取不到它。这样「密钥泄进响应」不是一个需要靠自觉避免
的错误，而是编译不过的事。`TestAppJSONNeverContainsSecret` 会在有人加回这个字段时炸掉。

## readiness 是诊断，不是布尔

「不能用」至少有七八种原因，每种要用户做的事完全不同。只回一个 `false`，用户只能
一个个试。所以 `Readiness` 把每条都单独报出来，其中最关键的是这两个**必须分开**：

| 字段 | 含义 | 用户该做什么 |
| --- | --- | --- |
| `MissingScopes` | 压根没申请 | 自己去开放平台加 |
| `UnapprovedScopes` | 已申请，等审批 | 催管理员，再提交也没用 |

合成一个「缺权限」列表，用户就会对着已经提交过的权限反复点申请。

四个状态：`setup_required`（要自己动手）、`waiting_for_admin`（只能等）、`ready`、
`runtime_unavailable`（配置齐了但连不上，通常是凭证失效）。

推导顺序上有个坑：**`waiting_for_admin` 必须在「全齐」之前判**。待批权限也算没授权，
要是先判全齐，一个纯粹卡在审批上的应用会因为其他条件都满足而被判成 `ready` 或
`runtime_unavailable`，把「去催管理员」这条唯一有用的提示吞掉。这个 bug 是
`TestDeriveStatus` 抓出来的。

### 走长连接之后，不再按快照报「缺权限 / 缺事件」

**这是踩过的坑，改回去会让所有正常应用都被误报。**

我们从「已发布版本的快照」读事件订阅
（`GET /application/v6/applications/me/app_versions/{id}` 的 `events[]`）。
但长连接模式下，**权限授权与事件订阅都是运行时生效的**，那份快照根本读不到。

拿快照去比对的后果是把工作正常的应用误报成缺一堆东西，然后逼用户到开放平台反复申请
已经生效的权限。实际报出来长这样——而那个应用其实好好的：

```
缺少 1 项权限        im:chat
缺少 8 项事件订阅     im.message.receive_v1, im.chat.disbanded_v1, ...
```

所以现在：

| 字段 | 还填吗 | 为什么 |
| --- | --- | --- |
| `MissingScopes` | **不填** | 快照读不到运行时授权。真缺权限会在调接口时报错，那时才是真问题 |
| `MissingEvents` | **不填** | 长连接的事件不需要在控制台逐个勾 |
| `UnapprovedScopes` | 填 | 来自 `/application/v6/scopes` 的 `grant_status`，实时可信 |
| `MissingCallbacks` | 填 | 再加判 `callback_type` 必须是 `websocket` |

就绪与否**以探活为准**：能读到机器人信息（`/bot/v3/info/`）就说明这条链路真的通。

回归防线是 `TestReadinessDoesNotReportMissingEventsOnLongConnection`——它构造一个
快照里一个事件都没有的应用，断言 readiness 仍然判 `ready`。

顺带一提，`callback_type` 那一判让 `AppConfiguration.CallbackType` 终于有了消费者。
它以前解析了、赋值了，然后全仓库没人读。

### ⚠️ 排查时别把 readiness 全绿当成「消息能进来」

上一节的取舍是对的，但它有个**代价没写在界面上**：`ready` 这个绿灯**不覆盖入站链路**。
2026-09-01 排查「报警群里发了多条消息，机器人完全没反应」时，这件事让判断整整偏了一轮
——页面上那个应用是绿的，`missingScopes` / `missingEvents` / `missingCallbacks` 全空，
`runtimeConnected: true`，看着像「配置侧没问题，那一定是代码 bug」。

实际上那几项里有两项**根本不可能报错**：

| 字段 | 看起来在说 | 实际上 |
| --- | --- | --- |
| `missingEvents: []` | 事件订阅齐了 | **恒为空**，代码里写死 `[]string{}`，从不填（见上一节） |
| `runtimeConnected: true` | 长连接是通的 | 只是**用 REST 调了一次 `/bot/v3/info/` 成功**，与 websocket 无关 |

所以 `runtimeConnected` 真正的含义是「**这对 app_id/app_secret 还能换 token**」，
它在「长连接没建起来」「机器人被踢出群」「飞书没推消息下来」时**照样是 true**。
把它读成「消息进得来」是这次排查最贵的一步。

**判「入站到底通不通」只有两个可信判据**，都不在 readiness 里：

1. **日志里有没有 `收到飞书消息但没有触发 agent`**（本包 `dropped.go`，Info 级）——
   有，就说明消息**确实进来了**，只是被触发判定否掉了，接着看它的 `reason`；
2. **`GET /api/workspaces/{ws}/feishu/chats` 里有没有这个群**——`rememberChat` 在
   `dispatch` 里调，所以群一旦出现在这张表里，就证明至少有一条消息走完过整条链路。

两个都没有，才轮到怀疑飞书侧（权限、机器人在不在群里、长连接是不是哑的）。

⚠️ **2026-09-02 这条判据又被用错了一次，值得补两句。** 那次「两个都没有」，
于是结论正确地指向飞书侧——但**「长连接没建起来」是个诱人的错误方向**：查日志能看到
`飞书长连接已就绪`，看着像连上了，可谁也说不准它是不是哑的。真正把方向锁死的是
**对照**，不是单看这个应用：

1. **先证明「消息真的存在」**（否则「零日志」可能只是没人说话）：
   `lark-cli im +chat-messages-list --chat-id oc_xxx --order desc`。
2. **再找一个同期收得到消息的 app 当阳性对照**，最好**落在同一个 pod** 上——
   那次两个 app 的 websocket 都在 `w9qzt` 上，一个收得到一个收不到，
   于是连接层、租约、网络**一次性全被排除**。
3. **最后按 `sender_type` 分组看**：那次全平台 `senderType=user` 有、
   `senderType=app` **一条都没有**，根因（缺 include_bot）当场浮出水面。

单看一个应用的日志永远得不出这三步的结论——**「它没日志」有太多种解释，
而「同一条连接上真人进得来、机器人进不来」只有一种。**

## 事件走长连接，没有回调地址

**不需要公网回调地址、不需要 verification token、不需要 encrypt key。**
只要 AppID + AppSecret，SDK 拨一条 websocket 到飞书，事件从那条连接推下来。
实现在 `shared/infra/lark/longconn.go`，入站编排在 `inbound.go`。

以前这里有个 `App.EventSubscriptionURL`，拼出来是
`{publicURL}/api/feishu/webhook/{appID}`，前端还让用户复制去粘到开放平台——
**但那个路由从来没注册过**（`handler.go` 只有 7 条路由）。也就是说即便用户把 8 个
事件全订阅了，消息也进不来，指向的是一个 404。改长连接时一并删掉了它，
以及随之失去用途的 `Config.PublicURL` 与 `AMA_PUBLIC_URL` 环境变量。

一个平台约束要记住：**一个飞书应用只允许一条长连接**。多实例裸连会互相驱逐，
且**不报错**，表现为入站消息随机丢失。所以建连前要抢 Redis 锁、抢不到就不建连
（fail-closed），见 `shared/infra/redis/README.md`。

### ⚠️ 空闲连接会静默死掉，且不会自己重连——重连不靠 SDK 的 Start

2026-09-09 线上踩到：某个小群机器人被 @ 后完全不响应，ready 绿灯、零报错、
**重启即恢复**。同机另一个高频机器人却一直正常。根因是一条「半开连接」：
那个群消息很少，websocket 空闲了一两天，被飞书/NAT 静默回收（本地不知道），
事件推到死连接上；活跃机器人靠流量保活所以没事。

真正的坑在 `shared/infra/lark/longconn.go`：飞书 SDK 的 `larkws.Client.Start()`
连上、起完心跳后最后一句是 **`select {}`——零 case，永久阻塞，连接死了也不返回**。
而我们又显式 `WithAutoReconnect(false)`（重连必须受 Redis 选主约束）。于是：

- SDK 不自己重连，我们的「重连循环」要等 `Start()` 返回才走到——它永不返回，
  **重连是死代码**，死连接只能靠进程重启（发布/扩容）重建；
- 半开时 `read` 一直阻塞、`OnDisconnected` 不触发；唯一冒出来的信号是 SDK 心跳
  ping 写失败（`ping failed`，Warn 级，进 SDK 自己的 logger）；
- 看门狗 `keepLease` 丢锁时调 `cancel()`，但 SDK 不监听 ctx、`Close()` 又排在
  永不返回的 `Start()` 之后——**cancel 是哑的**，存在「两实例同时连」窗口。

修法（`wsConn` 抽象 + `sdkConn`）：把 `Start` 放进 goroutine，外层在
`ctx.Done()` / 失活信号 / 建连致命错误之间等；`OnDisconnected`/`OnError` 与
`WithLogger`（只认 `ping failed` 这个 Warn、良性 Warn 不判死）任一触发就
`markDead` → `client.Close()` → 返回 `errConnDead`，回到抢锁循环**重新 dial
一条全新连接**。ctx 取消时同样 `Close()`（看门狗现在真能掐断连接）。

⚠️ `Close()` 叫不回 `Start`/`pingLoop` 那两个 goroutine（`select{}` 没有 case
能解除），每次失活会泄漏两个极小的阻塞 goroutine——失活是数天一次的低频事件，
可接受，SDK 形状决定无法从外部回收。判「现在到底有没有活连接」看
Redis 租约 key `lark:longconn:<appID>`（TTL 20s、持锁方 5s 续约），不看日志里
那条一次性的「已就绪」。回归用例在 `longconn_test.go` 的
`TestRunReconnectsAfterConnDies` / `TestRunClosesConnWhenContextCanceled`。

## 两个 Store 实现

`Store` 是接口，有两个实现，**接口本来就是按「将来是 SQL」设计的**，所以加 PG 那版
业务代码一行没动（`CreateApp` 撞 appID 返回 `ErrAppExists` 直接对应唯一索引冲突；
读方法一律返回副本，避免「内存实现能改、SQL 实现改不动」的行为差异）。

| 实现 | 位置 | 用在哪 |
| --- | --- | --- |
| `memStore` | 本包 `store.go` | 测试、以及还没配库的本地开发（`AMA_STORE=mem`） |
| `PGStore` | `repository/dao/feishudao`（`store_pg.go` + `schema_pg.sql`） | 要持久化的场合 |

⚠️ **两个实现分居两地是量过代价的选择。** `memStore` 的签名带着本包的 `App` /
`Session` 等类型，所以它只能 `import feishu`，而 5 个 `package feishu` 的测试直接
构造它 → `import cycle not allowed in test`（而 `go build` 照过，只有 `go vet` 报）。
搬走要把 12 个测试文件全迁成 `feishu_test` 并导出 32 个内部标识符，不划算。
`feishudao` 没这个约束，所以它可以 `import feishu`——判据是「谁的内部测试构造它」。
完整数字见 `store.go` 的「内存实现」一节。

```go
client, _ := pg.NewStandard(ctx, cfg.PG.ToStdConfig())
store, err := feishudao.NewPGStore(ctx, client)   // 顺手把表建好，幂等
```

`cmd/amad` 默认用 `feishudao.NewPGStore`（`AMA_STORE=mem` 回退到 `feishu.NewMemStore`）。

### schema：两张存量表把整个实体塞进 `doc`（新表别照抄）

⚠️ **现行规则是「字段展开成列、一对多用关联表」**，见
[repository/schema/README.md](../repository/schema/README.md) 的「表结构设计的默认
规则」。本包后来加的五张表（`feishu_inbound_messages` / `feishu_inflight_turns` /
`feishu_chats` / `feishu_session_scopes` / `feishu_progress_deliveries`）**全是真列**，
**那五张才是样板**。下面这段讲的是 `feishu_apps` / `feishu_sessions` 当初为什么选 doc。

两张表 `feishu_apps` / `feishu_sessions`，每张表把整个 Go 结构体序列化进一个 `jsonb`
列，只把**要用来过滤、排序或做唯一约束**的标量拎成真列（`workspace_id` / `app_id` /
`created_at` / `expires_at`）。

这么选是因为本仓库的 HTTP 契约对字段很敏感（真源是前端的 `LocalApiClient`），而 `App`
有 17 个字段、里面还嵌着 15 个字段的 `Readiness`。手写一整套列映射漏掉一个，编译不报错、
测试也未必发现，只有前端某个角落突然不显示了才知道。`TestStoreRoundTripsFullApp`
钉住这条。`jsonb` 会重排键，但响应里的字段顺序由 Go 结构体在 marshal 时决定，无影响。

⚠️ **「字段多」在新规则下不再构成用 doc 的理由**（17 个字段就建 17 列）：漏字段的
防线是编译器与逐字段往返用例，不是字段数量，而 doc 这边两道防线都没有——改一个
json tag 全部测试照样绿，但存量行读出空串、线上零报错。

### 三处 PG 与内存实现不完全一致

都写在 `store_pg.go` 的文件头注释里，这里只列结论：

1. **`Readiness` 的三个不导出字段落库后归零**（不过 JSON）。目前无影响——它们只在
   `refreshReadiness` 里刚 inspect 完就被消费掉，没有任何代码在「从 Store 读回来之后」
   再读它们。要是将来有了，得给 `Readiness` 补 `MarshalJSON`。
2. **`SaveApp` 会一并更新 `app_id` 列**；内存实现不更新它的 `byAppID` 反向索引。
   这里不复刻那个 bug。当前没有调用方会改 `AppID`。
3. **过期会话在写路径清，不在读路径清。** 对外行为一样（过期就是读不到），
   但读路径不产生写——注册期间前端是 5 秒一轮的。

另外两处**刻意对齐**、很容易写错的地方：`app_secret` 为空时存 **NULL 而不是空串**
（否则 `AppSecret()` 会返回 `("", nil)`，内存实现返回的是 `ErrAppNotFound`）；
`SaveApp` **不碰 `app_secret` 列**（内存实现里密钥在另一张 map 上，`SaveApp` 够不着它，
PG 侧要是顺手整行覆盖，一次普通的 readiness 刷新就会把密钥清掉）。

### 空切片必须是 `[]`，不能是 `null`

`App.Clone()` 除了深拷贝，还负责兜住**「切片字段非 nil」**这条契约，两个 Store 的
读方法都要让返回值过一遍它——包括 PG 的 `scanApp` / `ListApps`：从 `jsonb` 解出来的
`null` 或缺席字段就是 `nil`。

**别把 `cloneStrings(x)` 图省事写成 `append([]string(nil), x...)`**：后者在 `x` 为空时
返回 `nil`，编码成 JSON `null`。前端契约要求 `[]`（`ReadinessCard` 直接读
`r.missingScopes.length`），拿到 `null` 是整页白屏。

这个 bug 真发生过，且特别难从现象定位：只有**四个缺失列表都为空**的应用才触发，也就是
「配置最齐」的那些反而先炸；`readiness` 一旦落库带了 `null`，修了写路径也救不回已有的
行——所以读路径也过 `Clone()`。`TestEmptySlicesMarshalAsArraysNotNull` 钉住这条，
mem 与 pg 两个实现都跑。

## 一轮对话给用户的三样东西

群里 @ 一次机器人，用户会依次看到：

```
收到消息   →  用户那条消息上出现 OnIt 表情（马上）
开跑       →  一条思维链消息，边跑边长出思考与工具调用
跑完       →  封口思维链 → 发出最终答案（独立消息）→ 表情换成 DONE
```

编排在 `runAndReply`（`inbound.go`），三部分各自的实现在 `reply.go`（表情、切段、
顺序门）与 `turnprogress.go`（思维链的一轮生命周期）。

飞书接入层等待一轮回复的上限是 `runTimeout`（60 分钟），轮次锁的过期判断和重启恢复
也使用这个值。收到终态帧就立即收尾；到期仍未结束时，停止订阅、封口思维链，再发送
已收到的正文，没有正文则发送兜底提示。收尾另有 30 秒预算，发送成功与否仍取决于
飞书接口。超时不会向方舟发送中断，底层 turn 可能继续运行，但这一轮收尾后不会自动
补发后续结果。

### ⚠️ 一轮 = 一条回复，而「没有回复」有两种原因

这一节全部来自一次真实故障（现场 `sesn-example-06`）：用户在单聊里连发
「1、2、3」（前两条被转向）又发「4」，**期望恰好两条回复**，实际收到三条、且内容
串味。四个坑各自零报错，凑齐了就是这个现象。

**一、空回复必须分清是「出错」还是「被中断」。** `sendFinalReply` 在 `answer == ""`
时兜一句「这一轮没有产生回复，可能是执行出了问题」。但空回复有两种来源：

| 原因 | 该怎么办 |
| --- | --- |
| 真的执行出错（方舟崩了、工具全失败） | **该提示**，否则用户对着空白等一个永远不来的回复 |
| 这一轮被后一条消息中断了（连发转向） | **压根不该发消息**，被中断没有答案是正常结果 |

判据是有没有 `user.interrupt`（`turnInterrupted`）。**不能拿「有没有内容」当判据**，
也**不能把兜底整个删掉**——`resume.go` 仍然依赖它。

**二、中断在事件流里和正常结束长得一模一样。** `user.interrupt` 之后紧跟一帧
`session.status_idle + stop_reason=end_turn`（**不是** `requires_action`）。所以
`turnEnded` 照常判「结束」——那是对的，流确实该断；只是「为什么结束」要另外记。
⚠️ 别改成「让 `turnEnded` 对中断返回 false」：那样流永不关闭，串行锁要握满
`runTimeout` 的等待上限，用户每问一句都被挡。那是另一个已修的线上事故。

**三、转向必须让新消息自己有 owner。** 曾经的实现调 `RedirectRun` 把新消息塞进正在
跑的那条会话，注释写「由正在跑的那一轮自己收尾」——**那个假设是错的**：它一看到
中断带来的终态帧就断流走人了，新消息的答案没有任何人在等，于是落进再下一条消息的
缓冲里（两轮串味）。现在是「只发中断（`InterruptRun`）→ 等锁（`awaitTurn`）→
拿到锁再按正常路径跑」。顺带修掉一个隐性缺陷：`RedirectRun` 送进去的 `user.message`
**不带 `<harness:lark>` 块**，agent 看不到 chat_id / thread_id，实测它因此去乱调工具
找上下文。合并原语留给对话页——那条路上发消息的人和收答案的人是同一个。

**四、答复只取最后一段。** 调工具前那句「让我先拉一下聊天历史」也是 `agent.message`，
把一轮里所有 `agent.message` 拼起来就会把旁白当答复发出去。分界线是 `agent.tool_use`
（`answerBuffer.reset`），落库要跟着清——否则恢复时会把作废的旁白当成答案开头。

**五、`reclaim` 不能回收本进程还握着的锁。** `reclaimFinishedTurn` 用「run 是不是
终态」判断锁是否残留，而**一轮的生命周期比一次推理长**：agent 答完（run 变 `idle`）
之后，持锁的 goroutine 还要封口思维链、等顺序门、发答案，那期间 run 正是 `idle`。
只看它就会把锁发给第二个 goroutine，**两个订阅者同时消费同一条事件流、各自攒到全量
文本**——真机上表现为 9 条消息只中断 3 次、飞书里 3 条一模一样且含两轮内容的回复。
闸门是进程内的 `localTurns`：本进程持锁期间一律不回收；跨实例的真残留锁仍回收得到
（那才是 `reclaim` 要解决的问题）。刻意只放进程内——它表达「本进程有 goroutine 在
跑」，跨实例互斥仍归 `inflight_turns` 表 + `staleAfter`。

这五条的回归用例在 `inbound_realseq_test.go`，fixture 是**那条 run 的真实事件序列**
（脱敏）。用真实序列是刻意的：这几个 bug 都不是逻辑写错，而是对事件流形状的假设错了
——自己编的 fixture 会顺着当初那个错假设编，照不出来。

#### 表情是「换」不是「撤」

一轮结束时先撤 OnIt、再打 DONE（`markDone`），于是那条消息上**始终只有一个表情，
且它就是当前状态**。三条规则都是踩过或想清楚了才这么写的：

- **只撤不加不行。** 消息回到「没人理」的样子，用户翻聊天记录时分不出
  「机器人答过了」和「机器人压根没看见」。
- **撤 OnIt 失败就不要打 DONE。** 否则消息上并列「正在处理」与「已完成」两个表情，
  用户不知道该信哪个——比少一个表情更让人困惑。
- **答案没发出去时不能打 DONE。** 那是在谎报「答完了」，而用户手里什么都没有。
  这条路径只撤 OnIt（`clearReceived`），失败说明由 `dispatch` 那侧发出。

所以打 DONE **放在发答案之后**：表情是这一轮的状态指示，得等答案真的送出去才能变成
「完成」。顺序反了的话发送失败时消息上会留一个 DONE。
`TestInboundDoesNotClaimDoneWhenSendFails` 与 `TestMarkDoneSkipsDoneWhenRemoveFails`
分别钉住后两条。

另外 `emoji_type` 的取值由平台定义（`OK` / `THUMBSUP` / `DONE` / ...），
**写错不报错**：接口返回成功，但那个表情不会出现。

**只有第三步是必须成功的。** 表情打不上（缺 `im:message.reactions:write_only` 权限）、
思维链推不动（租户没开 CoT 预览、租约被别的实例抢了）都只让体验退化，绝不能让用户
拿不到回答。所以前两步的失败一律只记日志，`TestInboundAnswersEvenWhenCOTFails` 与
`TestInboundAnswersWhenReactionFails` 钉住这条。

### 这条链路曾经整段没生效

值得单独记一笔，因为它是个**零报错**的故障，而且原因不在任何一行业务逻辑里。

`reply.go`（表情 + 独立答案 + 切段）与 `progress_driver.go` / `transport.go`
（思维链的两条投递路径）都是**纯新增的提交**——它们自带单测、全部通过、
`go vet` 干净，但**没有任何一处改动 `inbound.go`**。于是主路径照旧走「一张不断
PATCH 的卡片」，那三个文件全是没有调用方的库代码。

表现是「群里只有一张卡片」，而日志、返回值、测试**全都正常**——因为每个部件确实都对，
错的是没人调它们。查这种故障时对着组件的单测看会一直看不出问题；`grep` 一下
「这个函数有没有生产调用方」才是最快的路径。

现在由 `TestInboundRunsFullTrio` 钉住整条编排，它断言的是「三步都真的发生了」，
而不是各部件内部对不对——后者已经有各自的单测了。

### 投递记录的键是轮次，不是会话

`feishu_progress_deliveries` 主键那一列叫 `run_id`，但存进去的是
`progressDeliveryKey(runID, srcMessageID)`。**这不是随手拼的**：会话按作用域复用之后
一个 `runID` 会跑很多轮，只用 `runID` 的话第二轮 `EnsureProgressDelivery` 会撞主键、
回读到第一轮那条已经 `completed` 的记录，driver 一看 `Finished()` 就直接返回——
**第二轮起再也没有思维链**，而且日志干净、调用全部返回 nil、答案照常发出。

和 `turnIdempotencyKey` 当年要挡的是同一类坑，区别只是那边毁掉的是卡片、这边是思维链。
不改 DDL 是因为本仓库的 schema 是「`go:embed` + 每次启动跑一遍」，全是
`create ... if not exists`，没有 `alter table` 也没有版本表——改主键在已经建过表的库上
不会生效，于是本地新库和线上老库结构分裂，那比换个键值危险得多。
`TestInboundSecondTurnAlsoGetsCOT` 钉住这条。

### ⚠️ `requires_action` 不是这一轮的结束（思维链「永远停在思考中」的根因）

现场 `sesn-example-08`：方舟侧那一轮**正常跑完了**（582 个事件、
末帧 `end_turn`、答案完整），可用户在飞书里看到的思维链**永远停在「思考中」**。

根因在 `snapshotBuilder.Consume`：它原本对 `session.status_idle` **一律**
`markTerminal("done")`。而 agent 每调一次工具，方舟就下发一帧
`session.status_idle + stop_reason=requires_action`——那是「我停下来等工具结果」，
worker 交回结果后这一轮还会继续。那一轮 61 次工具调用、58 帧这样的 idle，
于是**第一次工具调用（跑了几秒）就把快照标成终态了**。

后果有三个，全都零报错：

| 症状 | 为什么 |
| --- | --- |
| 工具全显示「（未完成）」 | `Terminal()` 把 `Done=false` 的工具补成「未完成」，而它跑在真实结果回来之前 |
| 一轮真失败却被说成成功 | `markTerminal` 幂等（`if b.terminal` 直接返回），后面真出的错**改不动它** |
| 降级卡片中途就写「已完成」 | 卡片文案直接由 `snapshot.Terminal` 决定 |

**这条链路失败时走的全是成功分支**：封口成功、`CompleteCOT` 回 `code:0`、
答案照常发出、`Handle` 返回 nil。所以线上按 `事件流异常结束` / `思维链封口失败`
这些告警去搜，**全时段零条**——很容易误判成「投递协程压根没起来」，
而真相是它跑了、而且自认为成功了。查这类问题别把「没有告警」当成「没有执行」。

判据与 `turnEnded`（inbound.go）、`core/scheduler.go` 的 `turnEnded`、
`core/chat.go` 的 `applyEventToRun` **完全一致，四处必须同步**。
`cotevents_requires_action_test.go` 里四条用例钉住它。

### 封口之前必须补推一次

节流（`progressPushInterval`）意味着**最后那个窗口里的进度还没推上去**，而
`cotEventBuilder.Terminal` 只负责收尾：它给未闭合的工具补 `TOOL_CALL_END` + `RESULT`，
却不会补 `TOOL_CALL_START`——那是 `Diff` 的活。

于是一个在最后一秒才出现的工具调用，客户端只收到它的结束、从没收到它的开始，
按 AG-UI 的语义那条事件无处挂载，**整个工具就凭空消失了**；同理最后一段思考文本
也会被丢掉。两者都不报错：封口成功、日志干净，只是内容缺一截。
所以 `turnProgress.finish` 先无条件 `push` 一次再封口。

### 每个 agent 可以关掉「展示详细工作内容」

`domain.Agent.ProgressDetail`：`full`（展示，**默认**）/ `brief`（不展示）。
关掉之后这个 agent 在飞书里只有「表情 + 最终答案」，群里**不会出现思维链消息**。

**实现是压根不建 CoT**（`startTurnProgress` 提前 `return nil`），走的是本包早就有的
那条降级路径。三个坑：

**一、不能用 `cot_hidden=true` 来「隐藏」。** 建 CoT 的接口有这个参数，文档说
「任务完成后隐藏 COT 消息」，听起来正好。**实测完全不是这个效果**——消息封口后
永远停在「思考中」，既没隐藏也没完成，而服务端一路 `code: 0`。详见
`shared/infra/lark/cot.go` 那段实测记录。照文档做的结果是：用户关掉了「展示详细内容」，
却换来一条永远转圈的思维链，比不关更糟。

**二、这个字段是字符串枚举，不是 bool。** 默认档是「展示」，而 Go 的 `bool` 零值是
`false`——方向正好反。用 bool 会有两个零报错故障：存量 agent 的 jsonb `doc` 里没这个键
（解出 `false`）→ 全站思维链一夜消失；老版本前端 PATCH 不发这个键（同样解成 `false`）
→ 用户没动过却被关掉。做成枚举后空串天然等于默认档，**不需要数据迁移**，
理由与 `FollowUpMode` 逐字相同（见 `domain.ProgressDetail` 的注释）。

**三、恢复路径要 `feishu_inflight_turns.agent_id`。** `resume.go` 重启后手上只有库里
那条记录，没有 agentID 就问不出这个开关。该列是**后加的、可空、无 default**：
存量行读出空串 → 反查不到 agent → `AgentShowsProgress` fail-open 回「展示」，
与加这个开关之前的行为一致。

⚠️ **fail-open 的方向是「回落到各自的默认档」，不是「都回 false」**：
`AgentFollowUpQueues` 查不到回 `false`（转向是它的默认档），
`AgentShowsProgress` 查不到回 `true`（展示是它的默认档）。回错方向的后果是
库抖一下就让这个 agent 的思维链整轮消失，而用户那边零报错。

`TestInboundSkipsCOTWhenProgressDetailBrief` 断言的是 **`CreateCOT` 一次都没调**，
而不是「事件里没有思考文本」——后者在「建了但没推内容」时也成立，
照不出「群里多了一条空思维链」。配对的 `TestInboundBuildsCOTByDefault` 守住默认档，
少了它把开关判反也能全绿。

### 工具结果走 `user.tool_result`，不是 `agent.tool_result`

**只认 `agent.*` 会让线上每一个工具都显示「（未完成）」。**

self_hosted 下工具跑在我们自己的容器里，worker 执行完是用 **`user.tool_result`**
把结果交回方舟的（见 `core/worker.go` 与 `shared/infra/dockertools/`）——那正是方舟接受的
7 种用户侧事件之一。`agent.tool_result` 只在方舟托管沙箱（`cloud` 环境）下才出现，
而本仓库**禁用 cloud**，所以那个分支线上一次都不会命中。

现场 `sesn-example-09`：21 次工具调用、21 条 `user.tool_result` 全都在
事件流里，而快照里 `done` 的有 **0 个**。

**这个坑零报错，而且症状会骗人：**

- 事件流本身是完整的，配对失败只是静默地什么都不做，日志里查不到
- 终止补齐还会给每个未闭合的工具补上 `TOOL_CALL_END` + `RESULT`，所以连
  「一直转圈」都不会——它们显示成「已完成但没有输出」，**看起来像工具真的失败了**，
  于是排查方向会被引向工具执行本身，而不是事件配对

`TestSnapshotBuilderPairsUserToolResult` 钉住这条，用的是真实事件形状：
自己的 `id` 是 `evt-*`、配对键在 `tool_use_id`、`content` 项**没有 `type` 字段**。

### 工具调用要穿插在思考文本里，而顺序信息在快照层就会丢

用户看到的曾经是「一大段思考 + 底部堆着一坨工具」，和 agent 实际的节奏
（想一下、查一下、再想一下）完全对不上，工具越多越离谱。

**根因在建模，不在渲染。** `ProgressSnapshot` 原本只有 `Thinking string` 和
`Tools []ProgressToolCall` 两个平行字段：方舟事件流本来是**按真实顺序到达**的，
但一进 `snapshotBuilder` 就被拍成两条独立轨道——快照只知道「有哪些工具」，
不知道第 3 个工具发生在哪段文本之后。顺序信息在落库之前就已经不存在了，
所以 `Diff` 除了「先把文本发完、再遍历工具」之外别无选择。

修法是给每次工具调用记一个 `AfterTextLen`：它发生时 `Thinking` 积累到的字节数。
渲染时按它把文本切开，工具事件就落回真实位置。**不需要任何平台协议支持**——
客户端按时间戳排，而时间戳由 `cotClock` 按产出顺序 +1，所以
`Diff` 里 `append` 的顺序就是用户看到的顺序。

三个坑：

**`0` 是有歧义的，所以必须另有一个 `Ordered` 标记。** 一个在任何文本之前就发生的
工具，`AfterTextLen` 本来就是 0；而存量行压根没这个字段，解出来也是 0。两者要的
渲染**恰好相反**（「工具在最前面」vs「工具在全部文本之后」），靠 0 去猜必然错一半，
而错的那一半正是**存量数据**——滚动发布期间正在跑的轮次会把工具全甩到消息最顶上，
比修复前更乱。所以由写入方显式声明一次。这个 bug 在实现时真的写出来了，
被 `TestCOTDiffLegacySnapshotWithoutOffsets` 当场照出来。

**不能改成一个 `items` 有序数组。** 快照是**带 json tag 落 jsonb 的**
（`feishu_progress_deliveries.applied_snapshot`），换字段名会让存量行读出来是空，
于是滚动发布期间正在跑的轮次把已推过的内容整个重推一遍，用户看到两份。
加可选字段则两个方向都安全：老实例读新行拿到的 `thinking` / `tools` 逐字不变。

**「只发增量」要按段维护，别退化成重发。** 单段时 `TrimPrefix` 就够了；切成多段
之后靠一个只增不减的字节水位 `sent`，每段从 `sent` 切起、发完推到段尾，
整轮下来每个字节恰好发一次。水位算错就会重发已发过的段，客户端显示两遍。

`TestCOTDiffInterleavesToolsWithText` 等五条钉住顺序，断言的是**整条事件序列**
而不是「某个事件在不在」——在不在这件事修复前后都成立，照不出顺序错乱。

## 服务重启后的未完成轮次恢复

发布即重启，所以「重启那一刻正在跑的对话怎么办」不是边角情况。实现见 `resume.go`，
写路径在 `inbound.go`，表是 `feishu_inflight_turns`。

**恢复时不能「重投用户消息」。** 那是 agent 为本地 CLI 进程时的做法（跟着服务
一起死，不重投就真没了）。本仓库反过来：`domain.Run.ID` 就是方舟的 session id，推理跑在
方舟服务端，工具调用由 self_hosted worker 重启后重新 poll 领回去——那一轮**很可能已经
答完了，或者还在正常往下跑**。重投只会让 agent 把同一个问题答两遍。所以恢复的动作是
`SyncRunEvents` 补齐断档 + 按状态定稿或重新挂流，不碰用户消息。

三个「错了不报错、只是行为不对」的坑：

**`requires_action` 不是终态。** worker 侧重启留下的恰恰是这个状态（agent 在等工具
结果），而 worker 重启后会重新领那条 work，所以它会自己往下走。把它判成终态就会在
agent 正要继续时就把答案定死，用户拿到半截回答。终态只有 `idle` 与 `terminated`。

**pending 会永久毒化那条消息。** 轮次记录之外，`feishu_inbound_messages` 那行留在
`pending` 的话，`ClaimInboundMessage` 会把它当重复投递直接 ack——于是用户重发同一句话
也被挡掉，这句话再也问不出去，且没有任何报错。所以每条终态分支都必须
`MarkInboundMessage(..., processed)`，见 `clearInflight`。

**清理只能有一个出口。** 清理若只挂在失败路径上，成功的
轮次会把记录漏在库里，下次重启把已经答过的问题又答一遍。所以调用点收敛在 `runAndReply`
的 `defer` 里，成功 / 流异常 / 超时 / 发消息失败全从那里收口，且用
`context.WithoutCancel`——走到那儿 `runCtx` 往往已经超时，拿它去删会失败，
记录就又漏了。`TestSuccessfulTurnClearsInflight` 钉住这条。

两处次要但容易写反的地方：

- **续跑上限 1 小时**（`resume.go` 的 `inflightMaxAge`）当初是按旧容器平台的 TTL 定的。
  换成 Docker 后容器闲置 `docker.containerTTL`（默认 24h）才删、工作区 volume 闲置
  `docker.workspaceTTL`（默认 7 天）才删，所以 1 小时现在偏保守。要放宽时**别超过工作区的寿命**：
  过了它工作区已被回收，硬续只会得到一个在空目录里困惑的 agent。
- **常规刷卡不能覆盖 `status` / `retry_count` / `started_at`。** 写路径会因为刷卡反复调
  `PutInflightTurn`，顺手把 `status` 冲回 `inflight` 的话，一次刷卡就能让另一个实例再
  认领一次，同一张卡两个写者、内容来回跳；`started_at` 每次刷新则会让超时分支永远不
  触发。PG 侧靠 `on conflict` 只更新三列，内存侧靠显式保留旧值——两边都由
  `TestPutInflightTurnKeepsClaimState` 钉住（这条用例真的抓出过内存实现的这个 bug）。

多实例安全靠库里的 CAS 认领（`ClaimInflightTurn`），不靠调用时序：每个实例启动都会扫，
`update ... where status = 'inflight'` 让数据库裁决唯一的接管者。

## 触发方式：群里什么时候开口

判定在 `trigger.go`（`shouldTrigger` 是纯函数），配置是 `App.TriggerMode`，两档：

| 取值 | 行为 |
| --- | --- |
| `mention_only` | 只有 @ 了才触发。**默认档**，也是这个功能加进来之前的行为 |
| `topic_and_mention` | **话题的根**（开题那条，判据是有 `thread_id` 且没有 `root_id`）免 @ 自动触发；**话题内的跟帖与其余群消息仍要 @** |

单聊两档都不受影响：入站层把单聊一律当成「天然 @」（`longconn.go`），用户专门找机器人
说话不该还要求打个 @。

### ⚠️ 免 @ 只给「新话题」，不给话题内的跟帖

**一个话题是一棵树，只有 root 能免 @ 触发，子孙节点一律要 @。**

判定曾经只看「有没有 `thread_id`」，于是**话题里的每一条消息都算**。2026-09-02 的线上
后果：ManagedAgents P1/P2 报警群里一个**非 AMA 托管的第三方 bot** 在话题内回了一条，
AMA bot 立刻给它打表情、还自动回复了一轮——那条消息既不是新话题、也没 @ 我们。

语义上开一个新话题是「我有件新事要说」，而话题内的跟帖是别人在自己那摊事里继续讨论，
AMA 只是恰好也在群里。**前者是找我们，后者不是。** 代价还被话题群的形态放大了：
报警群里每条报警各开一个话题，而跟帖（补充播报、状态更新）**比根消息多得多**。

判据是 **`root_id` / `parent_id` 都为空**（`isTopicRootMessage`）：

| | `thread_id` | `root_id` |
| --- | --- | --- |
| 话题根（开题那条） | `omt_xxx` | **空** |
| 话题内的跟帖 | `omt_xxx` | 根消息的 `message_id`（`om_xxx`） |

⚠️ **这两个值只有「事件通道」才给，飞书的读接口是拿不到的。**
`lark-cli im +threads-messages-list` / `+chat-messages-list` 对话题群里的消息**一律回
`root_id: null`**，连真正的跟帖也是（实测 `omt_0000c624372b1cb7` 那个话题的 6 条全是
null，读接口里区分根与跟帖的字段叫 `thread_message_position`）。**照着读接口的形状写
判据，会得出「所有消息都是根」的错误结论，等于没修。** 判据取自线上 run 的入站 payload
（`sesn-example-11` 是根、`sesn-example-10` 是跟帖，同一个报警群）。

⚠️ **两道闸都要改**：`mentionFreeEligible`（不读库的第一道）与 `shouldTrigger`（第二道）。
只改第二道的话第一道会继续放行、白读一次库；只改第一道则第二道形同虚设。

「这个群是不是话题群」**入站消息里就带着**，不用额外调接口——飞书的 `chat_type` 有
`topic_group` 这个取值。注意它与 `thread_id` 是两件事：后者说「这条消息在不在某个话题里」
（作用域用它，见 `scope.go`），前者说「这个群本身是不是话题群」（触发判定用它）。

### 为什么是枚举而不是布尔

「话题群免 @」看着就是个开关，但它不是。将来大概率还有第三档（「全群消息都触发」），
布尔就得再加一个字段，而**两个布尔有四种组合，其中「不免 @ 但全群触发」是无意义状态**，
还得靠约定去挡。枚举天然只有列出来的那几个值，加一档就是加一个常量。

### 为什么默认值必须是现状

**这是硬要求，不是保守。** 存量应用的 `doc` 里压根没有 `triggerMode` 这个键，
反序列化出来是空串。空串要是落到免 @ 那一侧，**升级后所有话题群会突然开始自动回复，
而没有任何人点过任何开关**——用户只会看到机器人在几十个群里同时变成话痨。

所以 `NormalizeTriggerMode` 把空串、以及任何不认识的值都收敛到 `mention_only`。
读路径（`App.Clone`）与判定路径（`shouldTrigger`）**各自归一化一次**，不互相依赖：
前者保证响应里不出现空串，后者保证即便有人绕过 Clone 塞进来一个脏值，行为也退回
最保守的那一侧。

### 加字段不需要 DDL

`feishu_apps` 是「一个聚合一张表，整个实体塞进 `doc` jsonb」（见上面的 schema 一节），
真列只有 `id / workspace_id / app_id / app_secret / created_at / doc`。所以给 `App`
加字段**没有 `alter table`**：老行读出来缺这个键就是零值，由归一化兜住；新行写进去自然
带上。上线顺序两个方向都活——老代码读新行会忽略这个键，新代码读老行拿到默认档。

### 防自触发：免 @ 之后必须挡住「我们自己人」

**这是整件事里最危险的一处。**

机器人的回复本身就是群里的一条新消息，飞书会把它推回来。@ 触发模式下这不成问题——
机器人不会 @ 自己，`Mentioned` 天然为 false，那条一刀切的闸顺手挡住了自触发。
**免 @ 一开，那道兜底就没了**：自己的回复落进话题群 → 判定说该触发 → 读到自己刚说的话 →
再答一遍 → 再推回来。**无限循环，且每一轮都真的在烧模型和容器。**

#### ⚠️ 判据 2026-09-01 从 `sender_type` 换成了 open_id 比对

原先是「`sender_type == "app"` 一律挡」。那条判据**太宽**，把一整类真实用途也挡死了：

```
「ManagedAgents P1/P2 报警群」44 条消息里 43 条是第三方监控机器人发的
（TLS 告警机器人 42 条、ARK机器人 1 条），真人发的 0 条。
用户开了话题免 @ 想让 agent 自动处理报警——而这一句让整档功能形同虚设。
```

这恰恰是免 @ **最有价值**的场景：报警群里本来就没有真人说话，要的就是
「机器人播报 → agent 接手」。所以现在第三方机器人与真人**同等对待**，
只挡「我们自己人」（`trigger.go` 的 `isSelfMessage`）：

| 发送者 | 免 @ 路上 | 为什么 |
| --- | --- | --- |
| 本应用自己的 bot | **挡** | 自问自答死循环 |
| 本租户下另一个 AMA 应用的 bot | **挡** | 见下面「多 bot 互触发」 |
| 第三方机器人（报警 bot） | **放行** | 与真人同等，这是本次要修的 |
| 真人 | 放行 | — |

⚠️ **不知道自己是谁时退回旧行为**（open_id 集合为空 → 仍按 `sender_type` 一律挡）。
这个兜底不能省：`botIdentity` 读不到身份时集合就是空的，精确比对恒不命中，
**没有兜底就是无限自触发**。宁可漏触发一次，不可让一个群自问自答烧到天荒地老。
`isBotMessage` 现在**只剩这一个调用点**，别因为看着像死代码就删。

#### ⚠️ 多 bot 互触发：只挡自己是不够的

线上不止一个应用开着免 @（实测 test 群里「AMA 助手」与「报警助手（MA 版）」同处一群）。
两者 open_id 不同，**各自都不认为对方是「自己」**，于是 A 回 B、B 回 A，
同样是死循环，只是多绕一个人。

所以 `shouldTrigger` 收的是**一个集合**：`inbound.selfBotOpenIDs` 从
`Store.ListAllApps` 现算出本租户下所有 AMA 应用的 bot open_id 一并排除。
读库失败时**至少保住本应用自己那一个**（返回空集合会让第三方机器人被连坐挡掉，
那正是本次要修的东西）。

#### ⚠️ 这条链路依赖 `app.BotOpenID` 是对的，而它曾经是错的

实测 `cli_example_main` 库里存 `ou_aaaa…`，飞书侧实际是 `ou_bbbb…`——
**对不上**。`shared/infra/lark` 的自我过滤与这里的 `isSelfMessage` 都拿它当判据，
比错了就是恒不命中，**防线整个失效**。以前没炸是因为「机器人一律挡」兜着，
那道闸撤掉之后这里就是唯一防线了。

所以 `supervisor.botIdentity` 改成**每次建连都问飞书、以飞书为准**，
不一致就回写库并打一条 Warn（原先是「库里非空就直接用」，存错了永远不自愈）。

⚠️ **这道判断刻意只加在免 @ 那条路上。** @ 触发那条保持原样：人 @ 机器人是显式意图，
不该被这里的任何判据挡掉——包括我们自己 @ 自己（`trigger_test.go` 有一条钉住这个边界）。

### 判定分两段，别合并

`Handle` 里先做不花 I/O 的那半（@ 了没有、是不是话题群、是不是机器人），再读库取模式。
因为触发模式配在应用上，合并成一段的话**群里每一条没 @ 的闲聊都要读一次库**。
两段的判据来自同一个 `trigger.go`，`mentionFreeEligible` 与 `shouldTrigger` 必须一致——
前者放宽、后者收紧最多是多读一次库；前者错否了，后者再对也没用。

⚠️ 这也意味着**改判据要同时改两处**。`trigger_test.go` 里 Handle 级的那几条用例是防线，
但**只变异一层时它们照样绿**（另一层兜住了），所以那几条做过双层变异验证。

**取不到应用时 fail-closed**（不触发）：免 @ 是用户显式开的增强档，判不出来时退回默认档
才是安全的一侧。反过来「读不到就当开了」会让一个查不到的应用对所有消息自动回复。

### ⚠️ 免 @ 还依赖一个飞书权限，缺了整档静默失效

**`topic_and_mention` 光在我们这边配好是不够的**：飞书要有
`im:message.group_msg`（敏感权限，需管理员审批）才会把**未 @ 机器人的群消息**推下来。

⚠️⚠️ **而它只覆盖「真人发的」消息——机器人发的还要另一档权限**，见下一节。
报警群里全是机器人在说话，所以那种群**光有这一档等于没有**。

没有它的表现极难查——**事件根本不到 amad**：

| | |
| --- | --- |
| `feishu_inbound_messages` | **一行都没有** |
| 日志 | 搜不到任何入站痕迹 |
| `shouldTrigger` | 再正确也没有输入 |
| 界面 | 就绪度照样是绿色 `ready` |

2026-08-28 线上真踩到：用户把 agent 设成「话题群自动回复」，在话题群里发消息，
机器人一声不吭。排查依次否掉了服务健康、崩溃循环、版本没上线、触发判定逻辑，
**根因在飞书侧的权限**。

**判据（最快 30 秒）**：查那条消息有没有进来。连入站记录都没有 = 飞书没推 = 查权限，
别再查我们的代码。

```sql
select message_id, chat_type, thread_id, created_at
from feishu_inbound_messages order by created_at desc limit 5;
```

⚠️ **线上够不到那张表时，改看日志——但那条判据 2026-09-01 之前是不成立的**，
见下一节。上面这句「日志搜不到任何入站痕迹 = 飞书没推」当时**只对了一半**：
消息进来后被触发判定否掉，日志里同样什么都没有。

### ⚠️ 「日志里没痕迹」曾经无法区分两种相反的原因

2026-09-01 又踩了一次，形状与上面那次几乎一样，但**根因方向相反**，
而当时的日志**分不出这两者**：

```
A. 飞书压根没推下来        → 去飞书侧查（权限 / 机器人在不在群里）
B. 推下来了，被我们否掉了   → 去代码里查（触发模式 / 发送者是不是机器人）
```

`Handle` 开头那三道闸原本就各有一条「说出理由」的日志，注释也写着
「⚠️ 每条不触发的分支都要说出理由」——**但它们是 `log.Debug`，而线上级别是 Info**
（`cmd/amad/main.go` 的 `newLogger`，注释里明写「级别的真源在这里」）。
于是这条防线**在线上从来没生效过**，B 看起来和 A 逐字相同：

```
lark/longconn.go:269 飞书长连接建立中
lark/longconn.go:308 飞书长连接已就绪，开始收消息
（然后就没有了，inbound.go 一条都没有）
```

**现在怎么防**：`dropped.go` 给这三条分支各补一条 **Info 级**日志。

```
收到飞书消息但没有触发 agent  reason=... chatType=... senderType=... thread=... suppressed=N
```

`reason` 与各道闸一一对应，**看 reason 就知道往哪查**：

| reason | 意思 | 往哪查 |
| --- | --- | --- |
| `没有 @ 机器人，且这条消息不在话题里` | 不在话题里（判据是 `thread_id` 非空） | 看 `thread` 字段；群里让人 @ 一下就能触发 |
| `这条是话题内的跟帖（不是开题那条），免 @ 只给新话题，跟帖要 @ 机器人` | 在话题里，但**不是话题根**（有 `root_id`） | **这是设计如此**：免 @ 只给新话题。要它回就 @ 一下 |
| `话题内免 @ 触发未开启（应用的触发模式是 mention_only）` | 在话题里、够资格，但模式没开 | 去改这个应用的触发模式 |
| `这条是我们自己的机器人发的（防自触发）` | 本应用、或另一个 AMA 应用的 bot 发的 | **这是防线在正常工作**，不要去改配置 |
| `正文为空` | 纯图片/文件，或只 @ 了没说话 | 多半是消息形态没解析出文本，是我们要跟进的 |
| `查不到这个飞书应用（读库失败，保守不触发）` | GetAppByAppID 失败 | **真故障**：库抖动或数据不一致 |

⚠️ 后两条与前几条性质不同：`防自触发` 是**正常**的（别去改那个明明开着的开关），
`查不到应用` 是**真故障**。混成一条会让排查的人往错方向走一整圈。

⚠️ **`sender_type=app` 不再是不触发的理由**（2026-09-01 起第三方机器人与真人同等），
所以看到报警群没反应时，别再拿「是机器人发的」当解释——去看 reason。

**这条日志一出现，就证明消息确实进来了**——A 当场被排除，不用再去查飞书权限。
反过来，长连接说「已就绪」却连一条 drop 日志都没有，才该怀疑 A。

**它是限流的**：同一个 `(应用, 原因)` 每分钟最多一条，`suppressed=N` 交代这个窗口里
被挡掉多少条。不限流的话一个活跃大群会把日志刷爆，而刷屏会把真正要紧的行淹掉——
那是另一种「查不到」。⚠️ 所以**看到一条不等于只来了一条**，要看 `suppressed`。

⚠️ **正文不进日志**（用户数据，长期留存且全团队可检索），
`TestDroppedMessageLogOmitsUserContent` 钉着这条。

#### 现在怎么防

`Readiness` 下发三个字段（`readiness.go`）：

| 字段 | 含义 |
| --- | --- |
| `GroupMessageScopeGranted` | 授权了没有 |
| `GroupMessageScope` | 权限名，**由服务端下发**，免得前端写死一份改常量时漏改 |
| `GroupMessageAuthURL` | 去开放平台申请的地址，**已授权时为空**，空/非空即「要不要提示」 |

前端在触发方式选择器**正下方**告警（`AgentDetailPage.tsx` 的 `GroupScopeNotice`）——
紧挨着刚做出的那个选择，用户才会把两件事联系起来；塞进就绪度卡片会变成一条与当前
动作无关的通用诊断，还会让只用默认档的人看到一条不该管的告警。

**刻意不拦保存**：这个权限由飞书侧异步变化（勾选 → 发版 → 生效），拦截会把保存动作
和飞书可用性耦合起来。代价是「存下了但不生效」这个状态仍然存在，所以**那条提示是唯一
的防线**，位置和措辞承担全部重量。

#### 补权限只能重走授权，「催审批」按钮没用

`TenantScopes` 只影响**授权页 URL**（`BeginSpec` → `encodeAddons`），
页面一打开就定死了，事后改不了。所以：

- **新接入**：接入页那个勾选框（`requestGroupMessage`）一并申请。
  ⚠️ 在这个功能加进来之前，**前端硬编码送空 body，这个权限从未被申请过**。
- **存量应用**：只能重走一次授权（`BeginSpec.AppID` 非空 = 升级已有应用）。
- ❌ **「提交管理员审批」补不了它**：`/application/v6/scopes/apply` 的 body 是空对象，
  只能催**已声明**的权限，对压根没申请过的无效。

`deriveStatus` **刻意不看这个权限**：它是**条件必需**的（只有免 @ 才要），而
`deriveStatus(r *Readiness)` 拿不到 `TriggerMode`；把它算进就绪度会重演本文件开头那个
「把工作正常的应用误报成缺一堆东西」的坑。

### ⚠️⚠️ 收群消息有**两档**权限，只有第一档时「机器人发的消息一条都不推」

**这是 2026-09-02 那次故障的根因，也是上一轮修错方向的原因。**

飞书把「收群消息」拆成两档，而 `im:message.group_msg` **明确不含机器人发的消息**。
官方文档「权限要求」节的原文
（<https://open.feishu.cn/document/server-docs/im-v1/message/events/receive>）：

> - 接收群聊中所有用户和其他机器人发送的消息：需具备 **`im:message.group_msg.include_bot:read`** 权限，不包含当前机器人自己发送的消息
> - 仅接收群聊中所有用户消息，不包含机器人消息：需具备 **`im:message.group_msg`** 权限

四档对照（`shared/infra/lark/scopes.go` 里有同一张表）：

| 权限 | 免 @ | 含机器人 |
| --- | --- | --- |
| `im:message.group_at_msg:readonly` | ❌ 只收 @ 我的 | ❌ |
| `im:message.group_at_msg.include_bot:readonly` | ❌ 只收 @ 我的 | ✅ |
| `im:message.group_msg` | ✅ | ❌ ← **我们原来只有这个** |
| `im:message.group_msg.include_bot:read` | ✅ | ✅ |

⚠️ **后缀 `:read` 与 `:readonly` 不一致是官方原文如此**，两个域核对过，不是笔误。

#### 为什么上一轮没修对（这条最值得记）

上一轮用 `lark-cli im +chat-messages-list` 看到报警群里 43 条
`sender_type=app` 的消息，据此判定「消息进得来、是被 `trigger.go` 挡了」，
于是把防自触发从「挡所有机器人」收窄成「只挡自己人」。改完上线，**问题依旧**。

**因为那是历史消息 API，不是事件推送。** 两条链路权限语义不同：

| | 用 `im:message.group_msg` |
| --- | --- |
| 历史消息 API（`GET /open-apis/im/v1/messages`） | **返回**机器人消息（官方示例响应里就有 `"sender_type": "app"`） |
| 事件推送（`im.message.receive_v1`） | **不推**机器人消息 |

**「历史里读得到」推不出「事件会推下来」。** 上一轮的改动是修复的**前置条件**
（必要不充分），不是错的——但缺了权限，`shouldTrigger` 再对也没有任何输入。

#### 决定性证据：同一条 websocket 上的天然对照

2026-09-02 查的，同一个 app、同一个进程（pod `w9qzt`）：

| 消息来源 | 事件到 amad 了吗 |
| --- | --- |
| 两个 test 群里**真人**发的 | ✅ 6 条，`飞书消息进来了，开跑` |
| **同一个话题群**里 bot 发的 12 条 | ❌ 逐条按 `message_id` grep 全为 0 |
| 报警群 8+ 条告警（`sender_type=app`） | ❌ 20 小时内该 `chat_id` 在日志里出现 **0 次** |

真人进得来、机器人进不来 ⇒ 排除连接、租约、代码过滤，锁定飞书侧权限档位。

#### 现在怎么防

- **申请时一次要齐**：勾了「群消息」就同时申请 `group_msg` + 两个 `include_bot`
  （`onboarding.go`）。⚠️ **`group_msg` 不能被替代掉**——读群历史点名要它本身。
- **巡检分开报**：`BotMessageScopeGranted` 与 `GroupMessageScopeGranted` **两个字段**。
  合成一个的话，「第一档有、第二档没有」这个状态在界面上根本没法表达——
  而那正是存量应用的常态（include_bot 是后来才有的档位）。
- **建连时打专属 Warn**（`supervisor.go` 的 `warnIfBotMessagesUnreachable`）。
  ⚠️ 这条防线**不能放进 `dropped.go`**：缺权限时飞书根本不推事件，`Handle` 一次都不会
  被调用，放那儿是一条**永远打不出来的日志**。建连是唯一不依赖消息到达的时机。

#### ⚠️ 事件侧的 `sender_type` 到底给什么，至今无法证实

三处说法互相矛盾，且官方**没有任何一段机器人发消息的事件示例**：

| 出处 | 说法 |
| --- | --- |
| SDK `im/v1/model.go:2909` 的 `EventSender`（事件用的就是它） | 「目前只支持用户(user)发送的消息」 |
| 事件文档的字段定义 | `"user"（用户）或"bot"（机器人）` |
| 历史 API 的 `Sender`（`model.go:6539`） | `user / app / anonymous / unknown` |

仓库里 `senderTypeApp = "app"` 的来源是**历史 API 的采样**。所以
`trigger.go` 的 `isBotMessage` 现在 **`app` 与 `bot` 两个都认**——它是
`selfOpenIDs` 为空时防自触发的最后一道闸，**比错就是恒不命中 = 无限自问自答**。
以前不出事只因机器人事件压根不来；include_bot 一生效它就成了真会被走到的防线。
拿到真报文后可以收窄，在那之前别删任何一个。



入站消息按「作用域」决定归到哪条方舟会话。规则在 `scope.go`
（`scopeForMessage` 是纯函数）：

| 消息形态 | 作用域键 | 效果 |
| --- | --- | --- |
| 群里的话题（有 `thread_id` / `root_id`） | `thread:<锚点>` | 一个话题一条会话 |
| 普通群（没有话题锚点） | `chat:<chat_id>` | 整个群共享一条会话 |
| 单聊的**新话头**（`p2p` 且无锚点） | `message:<自己的 message_id>` | **每条新私聊消息一条新会话** |
| 单聊**话题内的后续消息**（`p2p` 且有 `root_id`） | `message:<root_id>` | **复用话题根消息那条会话** |

映射落在 `feishu_session_scopes`，主键 `(lark_app_id, scope_key, agent_id)`。

**单聊的新话头为什么反过来。** 它曾经和普通群一样按 `chat_id` 复用，后来按产品要求改成
每条消息一个独立会话，因为「整个单聊共用一条会话」代价大于收益：

- **上一轮卡住会连坐下一句。** 串行锁是按 `run_id` 上的（一条会话同时只允许跑一轮），
  上一轮没能正常收尾时（方舟不关流、进程被 kill、删记录失败——三种线上都发生过），
  用户接着问的每一句都只会收到「上一句还在处理中」，得等满 `runTimeout` 的等待上限。
  群里有别人说话作缓冲，私聊是一对一连打的，同一个坑在私聊里表现得最难忍。
- **历史越长越贵越慢。** 复用意味着每轮都把整段历史重新喂给模型，而私聊里连续追问
  攒出来的那些问题彼此大多无关。

`TestInboundCreatesNewSessionPerDirectMessage` 与 `TestScopeForMessageP2PIsPerMessage`
钉住这件事，别顺手改回按 `chat_id` 复用。

### 「每条消息一个会话」与「话题内复用」并不矛盾

**这两条管的是两种不同的用户动作，别把它们看成互相打架、然后删掉一条。**

上面那两个理由针对的是「用户在单聊主线里连打互不相关的问题」。但用户**特意点进机器人
回复形成的那个话题**里接着说，表达的恰恰是相反的意思：「我说的就是刚才那件事」——
此时不复用，agent 就完全不记得前面聊过什么。两个理由在话题里也都不成立：话题是用户
显式开的、是一段有界的子对话（不会无限攒历史）；上一轮卡住时用户随时能回主线另开一条。

这个 bug 真发生过，**根因是 p2p 的短路判断排在话题判断之前**：`scopeForMessage` 里
p2p 分支直接 return，`threadAnchor` 在单聊里永远没机会被调用，于是话题内的后续消息
又按自己的 `message_id` 开了一条全新 session（线上实测根消息 `sesn-...jkl75`、
话题内第二条 `sesn-...j25b7`，两条 `turn_count` 都是 1）。
`TestInboundReusesSessionInDirectThread` 与 `TestScopeForMessageP2PThreadReusesRootKey`
是那条顺序的回归防线。

**单聊的锚点只认 `root_id` / `parent_id`，刻意不认 `thread_id`。** 因为要算出与根消息
**完全相同**的键：根消息当初是按「新话头」落的 `message:<它自己的 message_id>`，而
`root_id` 与 `message_id` 是同一个命名空间（`om_xxx`，SDK 注释写的是 `open_message_id`），
`thread_id` 是另一个（`omt_xxx`）。用 `thread_id` 拼出的 `message:omt_xxx` 根消息从没
绑过，等于没修。实测那两条消息：话题内第二条的 `root_id` == 根消息的 `message_id`
（`om_x100b0000cfc8d27fde49fabb5d57bc50`），而 `thread_id` 是 `omt_0000fb76f7d5ca5d`。

顺带一个好处：**存量数据不用迁移**。库里那些 `message:<message_id>` 的老记录，正是老
话题里后续消息现在会算出来的键，所以在已存在的老话题里接着说也能直接复用上。

### 「单聊连发两条，第二条被挡」——这是预期行为，不是 bug

**有人会拿着上面那张表来报 bug**：既然「单聊主线每条消息一条独立会话」，那连发两条
就不该互相挡，可实测第二条确实收到「上一句还在处理中」。代码和现象对不上，于是很容易
去 `scope.go` 或串行锁里找一个并不存在的缺陷。

**中间少的那一环是 `reply_in_thread`。** 机器人回复主线消息时会开一个话题
（`runAndReply` 里 `inThread := msg.ThreadID == ""`，主线消息没有 `thread_id`，所以
恒为真），飞书把这一问一答折叠成话题之后，**客户端的输入焦点就在那个话题里**——
用户以为自己在主线接着打第二句，实际发出来的消息带着 `root_id`：

```
主线第一句 om_A（无锚点） → 键 message:om_A → 建会话 sesn-1
  → 机器人带 reply_in_thread 回复，飞书折叠成话题
  → 用户"接着"打的第二句 om_B 带上 root_id=om_A
  → directThreadAnchor 命中 → 键 message:om_A（与第一句逐字相同）
  → 同一条会话 sesn-1 → 撞上「一条会话只跑一轮」→「上一句还在处理中」
```

所以它是上面两条设计规则**叠加**出来的结果，两条单看都对：一、机器人回复要开话题；
二、话题内的后续消息要复用根消息那条会话（否则 agent 不记得刚才聊了什么）。
**不要为了「能并发」去放宽会话级互斥**——那条注释里的失败模式是真的（两轮并发时
`agentText` 分不出哪句回给谁，两条答案会混在一起），而且它零报错。

实测证据（本地库 `feishu_inbound_messages`，两次独立发生）：用户在单聊里连打两句，
第二句的 `root_id` 都等于第一句的 `message_id`（`om_...623c83` → `om_...edd387`、
`om_...f3e56b` → `om_...68d0fa`），`root_id` 为空的「真主线」消息则各自建了独立会话
（`oc_6134...` 那个单聊连着四条，四条 `scopeType=message` 各一条会话，一次都没被挡）。

**真正的主线消息之间确实互不相干**，这一点没有退化：要造出「两条都是主线」，得在
第一轮回复**之前**把第二句发出去，或者在客户端回到主线输入框再发。

⚠️ **群聊与话题群里撞车是预期的，且与本节无关**：`chat:<chat_id>` 和
`thread:<锚点>` 本来就是「一个群/一个话题一条会话」，同一个群里连问两句必然共用一条
会话、必然串行。那不是缺陷，是「群里的人共享同一个上下文」的直接后果。

这条因果链由 `inbound_p2p_thread_test.go` 整组钉住，其中
`TestP2PMainlineReplyOpensThread` 锁的是**第一环**（回复带 `reply_in_thread`）——
以前 `fakeSender` 直接丢掉了这个字段，于是"锚点是我们自己造成的"这件事在单测里
根本不存在，只能靠读代码猜。

**私聊仍然走作用域这条路，没有绕过去直接建会话。** 新话头的键取 `message_id` 之后
`GetSessionScope` 几乎必然 `ErrScopeNotBound`，于是每条主线消息都会走到建会话那一步
——效果就是「每个话头一个会话」。但绕过作用域会丢掉一道防线：飞书会重复派发同一条
消息，而重投时算出的键不变，所以这套「先查再由数据库裁决」顺带把重复投递挡在了建会话
之前（两个实例同时收到同一条重投消息时，抢输的那个改用赢家那条会话，而不是给同一句话
建出第二个会话、把同一个问题答两遍）。两条分支各自算键，所以去重要分别验：
`TestInboundDirectMessageRedeliveryDoesNotCreateSecondSession`（新话头）与
`TestInboundDirectThreadRedeliveryDoesNotCreateSecondSession`（话题内）锁住这一条。

**为什么不能是进程内 map**：线上多实例，两条相邻消息通常不在同一个进程里，
表现会是「有时候记得、有时候不记得」。**为什么不能用确定性 ID 省掉这张表**：
`run_id` 就是方舟发的 session id，我们无权决定它长什么样。

**并发靠唯一约束，不靠先查再建。** 两个实例同时收到同一个群的消息时都会查到「没绑过」、
各建一个会话，然后由数据库裁决：`BindSessionScope` 用
`insert ... on conflict do nothing` + 回读，抢输的那个**必须改用赢家的 run_id**
（返回值 `bound=false`）。用「先查再建」就是 TOCTOU，一个群从此两条上下文。

### 复用带来的几个坑，都不报错

**一、幂等键必须按轮次取，不能只用 `run_id`。** 一个 `run_id` 现在会跑很多轮，
而飞书对重复的 `uuid` 是**把这条消息丢掉并返回上一条的 message_id**
（见 `shared/infra/lark/message.go` 的 `idempotencyKey`）。只用 `run_id` 的话第二轮建卡会拿到
第一轮那张卡，于是第二轮的答案 PATCH 掉第一轮的答案——用户的第一个回答凭空消失，
两次调用都返回成功。所以键是 `ama-turn:<run>:<源消息>`（`turnIdempotencyKey`）。

**二、同一条会话同时只能跑一轮。** 一条会话的事件流是共享的，两轮并发时两个
`cardUpdater` 都会把对方的回复写到自己的卡上（`agentText` 分不出哪句是回给谁的），
两张卡都是两个问题的答案混在一起。所以开跑前要过 `Store.TryBeginTurn`——
它拿 `feishu_inflight_turns` 那行当互斥量（存活窗口正好是一轮），抢不到就回一句
「上一句还在处理」。**被挡住不能静默丢掉**，否则用户以为机器人坏了。

`TryBeginTurn` 带 `staleAfter`：持有者所在实例可能没来得及删记录就没了（OOM、kill -9），
比这个窗口还老的记录直接抢过来，等价一把 TTL 租约。活性看
`max(started_at, updated_at)`——长任务的 `started_at` 很老，但只要还在刷卡就不算死。

**三、方舟不会自己关流，订阅方必须自己认终态帧。** 这条是线上事故复盘出来的，
症状是「上一轮明明答完了，下一句还被回『上一句还在处理中』，一挡十分钟」。

`StreamRunEvents` **不会自己返回**：方舟按会话维持 SSE 长连接、`end_turn` 之后继续等
这条会话的下一条用户消息。而释放串行锁的 `defer` 排在 `wg.Wait()` 后面，于是锁一直
握到 `runTimeout` 的等待上限。所以 `runAndReply` 在 `onFrame` 里判 `turnEnded(raw)`，
认出终态就 cancel 掉专用的 `streamCtx`。判据与 `core/scheduler.go` 的 `turnEnded`、
`core/chat.go` 的 `applyEventToRun` 是同一套：

```
session.status_idle + stop_reason=end_turn         → 结束
session.status_idle + stop_reason=requires_action  → **不是结束**，agent 在等工具结果
session.status_terminated                          → 结束
```

`requires_action` 那条最要紧：真实的一轮里它出现在**中间**（实测 `sesn-...-kvz6t`
那条会话 34 个事件里 `status_idle` 出现 4 次，前两次都是它）。当成结束会在 agent
正要接着说时断流，用户拿到半截答案，而且不报任何错。

**这个坑单测原本照不出来**：老的 `fakeConversations.StreamRunEvents` 把帧发完就
`return`，「流会自己结束」在测试里是天然成立的。所以回归用例用的是
`inbound_turnlock_test.go` 里的 `stickyConversations`——**帧发完不返回**，
只有 ctx 取消才结束，这才是方舟的真实形状。

**四、复用会话在网页列表里「消失」，因为 `startedAt` 不会变。** 用户报的是
「飞书发的消息在网页对话页看不到」，但消息一直都在库里——第 N 轮追加进第一轮那条
Run，而列表原先按 `startedAt` 降序排，于是刚说完话的会话还躺在十小时前的位置上，
标题也冻结在第一句（`core/chat.go` 只在 `run.Title == ""` 时回填）。用户按「新会话
应该在最上面」的直觉去找，自然找不到。

修法是 `domain.Run` 加 `lastActivityAt`，列表按它排（PG 侧
`coalesce(nullif(doc->>'lastActivityAt',''), started_at)`，`nullif` 不能省——缺键给
NULL 但空串给空串，而空串排在所有时间戳之前，会把老会话全顶到最前）。
两处刷新：`applyEventToRun`（每条落库事件）与 `AppendRunEvents`（用户刚发的那句，
**不能只靠前者**——它要有订阅者才被调到，网页发完就切走时没人订阅）。

顺带一件事：飞书建会话此前走的是 `CreateConversation`，Run 一律标成
`trigger=manual`，运行历史里分不出来源（`domain.TriggerFeishu` 定义了却从没人用，
是死代码）。现在走 `CreateFeishuConversation`。⚠️ **改这个的同时必须放宽前端筛选**
（`ChatPage.tsx` 的 `CONVERSATION_TRIGGERS`）——那里原本只收 `manual`，只改后端会让
飞书会话从左栏**整体消失**，比原来的故障更严重。

**四、抢锁失败时要回头核对 run 的真实状态。** 那张表只能证明「有人开过一轮且没删
记录」，证明不了「那一轮还在跑」。删不掉的原因有好几种（上面第三条、删除本身失败、
`kill -9`），而光靠 `staleAfter` 兜底要等满整个 `runTimeout`，期间用户每问一句都被挡。
所以 `TryBeginTurn` 返回 false 之后走 `reclaimFinishedTurn`：问一次 `RunStatus`，
是终态（`idle` / `terminated`）就把残留记录删掉重试一次。**只在抢锁失败这条冷路径上
多一次远端调用**，正常路径一次都不多。问不出真相（`RunStatus` 报错）时维持原判挡住
——猜错的代价是两轮并发把回复写乱，比让用户重发一次糟得多。

## 给 agent 的飞书上下文：`<harness:lark>` 块

入站消息发给方舟时，**用户正文之后会追加一个 `<harness:lark>` 块**
（`larkprompt.go`），里面是这条消息的来源信息：chat_id / chat_type / thread_id /
root_id / message_id / sender_open_id，加上**按会话形态给好的取历史命令**。

没有它 agent 不知道自己在哪个群、哪个话题、谁在说话——群里 @ 它一句
「那这个怎么办」，它看不到「这个」指什么。

**这是本仓库第二个 `harness:` 块**，约定沿用 `core/envprompt.go` 的文件头。
`larkprompt.go` 的文件头写了三件只在这里成立的事，动它之前读那段：

1. **为什么走 user.message 的第二个 content block，而不是系统提示词。**
   `<harness:env>` 进的是**方舟 Agent 的 system**（建 Agent 时一次性写、按
   (workspace, agent) 维度）；这个块**每条消息都不同**，塞进 system 会把第一条消息的
   chat_id 永久钉在所有后续会话上。落在事件里还顺带解决了前端可见性——
   页面上「平台注入」那个折叠区读的就是它，零新增接口。
2. **⚠️ 必须排在用户正文之后。** `core.TitleFromEvents` 会把第一条 user.message 的
   全部 text block 拼起来当会话标题；排前面的话每条飞书会话的标题都变成一段 XML，
   而且**不可恢复**（标题只在为空时回填一次）。`core` 那边靠 `HarnessTagPrefix`
   跳过这类块，两边各有测试钉着。
3. **🔒 群历史是外部不可信输入。** 块自己只放校验过的 id（安全），但 agent 按块里的
   命令拉回来的群消息**群里任何人都能写**。处置分三层：块里就地警告、lark-cli 内置
   技能正文里给完整信任边界、要求模型把检测到的注入**说出来**而不是静默忽略。
   ⚠️ **XML 包住不是转义**，与 envprompt.go 一字不差。

⚠️ **拼进命令的值要单独校验一遍。** 踩过：可选字段脏掉时 id 列表那半干净了，
但 `historyGuide` 把原始 `thread_id` 拼进了 `lark-cli` 命令，于是
`omt_x; curl evil.com` 原样流出去——**「能不能显示」与「能不能进命令」是两条判据**。

### 取群历史要 `im:message.group_msg`，且按应用授权

基础的 `im:message:readonly` 只够读单聊；**群聊的列表接口额外强制要
`im:message.group_msg`**（与「免 @ 收推送」同名，但一个管推送一个管读历史）。
它按应用授权，每个用户注册的应用要各自申请，存量应用只能由所有者去开放平台补。

所以块里那几条命令**无条件给**，但紧跟一句「失败了不要重试」——agent 拿到
`230027` 反复换参数只会烧工具调用。⚠️ 另一种失败是容器里的 `lark-cli`
**还没配飞书凭据**（回 `not_configured`，而它 hint 指的 `config init --new`
在容器里永久阻塞）。非交互初始化是
`printf '<secret>' | lark-cli config init --app-id … --app-secret-stdin`，
实测可行，但**把 tenant token 注入容器是一个独立的安全决定**，本轮刻意没做。

#### ⚠️ `permissionNotice` 那句话有三个职责，只能改掉其中一个

这条写给下一个想动它的人。那句话看着是一整句，其实同时在做三件事：

| 职责 | 去掉的后果 | 处置 |
| --- | --- | --- |
| ① 别重试、别换参数 | agent 反复换参数试，纯烧工具调用 | **必须保留** |
| ② 别自己跑 `config init` | 容器里永久阻塞挂到超时，**并连坐同一批工具调用**（SDK 顺序执行 tool_use） | **必须保留** |
| ③ 失败了怎么跟用户说 | —— | **已改**，见下 |

③ 原本写的是「两种都只能**如实告诉用户**」，agent 严格照做了，于是普通用户在群里
真的收到过「lark-cli 还没有配置飞书凭据（`not_configured`）……需要有人先执行
`lark-cli config init --new` 走浏览器授权」——**内部 CLI 名字、错误码，外加一条他
根本执行不了的运维指令**。

现在改成**静默降级**：当作本来就没有这段历史、自然作答；缺上文听不懂就**像正常聊天
那样直接反问**（「你说的『2』是指哪一项？」）。分界线是——「我不知道你在说什么，
请补充」是正常对话，「我读不到历史因为 CLI 没配凭据」是泄漏实现细节。用户明确要求
回顾上文时才说一句「我看不到之前的消息」，**同样不解释原因**。

⚠️ **只写「不要告诉用户」不够**，模型会自己发挥成「我遇到了一些问题」那种含糊其辞，
所以给的是**正面行为**。⚠️ 提示词里也**刻意不再出现 `not_configured` / `230027`
这些字面量**——那条真实回复几乎是逐字抄提示词的，**给了什么词模型就可能吐出什么词**；
判「该不该重试」也用不着错误码，任何取历史失败都不该重试。

**两处配套，改一处要看另一处**：`larkprompt.go` 的 `permissionNotice`（每条消息注入，
块被丢弃时就没了）与 `core/skillbuiltin.go` 的 lark-cli 技能正文（静态，勾了技能就一直在）。
`larkprompt_test.go` 与 `core/skillbuiltin_test.go` 各有一组**否定断言**钉着，都做过变异验证。

⚠️ **用户不该感知 ≠ 我们也不用知道。** 静默之后这条失败原本会变成完全无声的事件
（会话照常回复、run 照常成功、只是答得没头没脑），所以配了一条服务端留痕：
`shared/infra/shelltools/larkdegrade.go` 在工具装饰器上认出「取历史失败」并打一条 Warn，
两种失败**归因分开**（一个要配凭据、一个要补权限，找的人不是同一个）。

### 入站附件带 resource key，不带字节

非文本消息（图片 / 文件 / 语音 / 视频）在 `shared/infra/lark/content.go` 里解析成
**带 key 的占位符**：

```
[图片 img_v3_02ad_e19f…]        [文件 报表.xlsx file_v3_abc…]
```

⚠️ 早先这里只输出 `[image 消息]`，**key 在解析层就被丢了**，于是 agent 连去取都
无从下手（取资源要 `(message_id, file_key)` 这一对）。那是有人真反馈过的缺口，
而且当时有一条测试把它断言成了预期行为。改的时候**两条路径都要动**：
`textFromObject`（独立的附件消息）与 `postSegment`（富文本里嵌的图）。

**刻意不下载字节**：那会让每条带图的消息往提示词里塞一坨 base64，而且会随对话
**永久落进 RunEvent**。已端到端验过拿占位符里的 key 能取到逐字节一致的原图。

## 已知边界

**身份与授权都已经是真的了。** 调用者身份来自登录会话（`ActorFrom(r)`
读的是鉴权中间件写进 context 的 `auth.Identity`，值是用户邮箱），
未登录的请求在 `httpd` 那层就被 401 挡掉了，进不到本包。

授权走 `server/authz`：`Config.Authorizer` 的实现是 `authz.FeishuAuthorizer`，
按**空间成员角色**判——`CanAccessWorkspace` 要 viewer 起，`CanManageWorkspace`
（接入应用、断开应用、刷新就绪度、请管理员授权）要 admin 起。装配在 `cmd/amad`。

⚠️ **`Config.Authorizer` 留空是一律拒绝**（`denyAllAuthorizer`）。早前那个
`AllowAllAuthorizer` 已经删了：兜底放行时「忘了装配」等于授权形同不存在，
而且零报错、测试全绿；兜底拒绝则第一次点就发现。所以本包的测试要显式传一个
放行的授权器（见 `feishu_test.go` 的 `allowAuthorizer`），否则全部停在 403。

本包 Service 内层那几处 `requireManage` **在 HTTP 边界的 Guard 之外仍然保留**，
因为它们保证的是「先鉴权再查会话」这条不变量（顺序反了 sessionID 就成了事实凭证）。
两层共用同一张角色表，判据一致——不一致会出现「Guard 放过、Service 又拒」。

**入站消息这条路径上没有身份，这是设计不是缺口。** `inbound.go` 由飞书长连接驱动，
ctx 派生自进程级 ctx，不经过 HTTP、不经过 Guard、也不经过 `Authorizer`——
飞书机器人不代表任何一个「人」，归属靠 `App.WorkspaceID` 直接带出来。
**不要"顺手"给它加鉴权**：那会让所有飞书消息静默失效（机器人不回话，且不报错）。
同理 `Store.ListAllApps` / `ListInflightTurns` 是跨空间的进程内查询，不要接到 HTTP 上。

**删掉了手填凭证的入口。** 设备码是唯一路径。如果某个租户禁止成员自建应用，
授权会被管理员拦下——此时 readiness 的 `waiting_for_admin` 能让用户看清卡在哪，
但没有反向的手填逃生口，需要另开一轮。

**不去飞书侧删应用。** `DeleteApp` 只解除 AMA 侧的接入关系。那是用户自己的应用，
平台没有替他删除的道理，别处可能还在用。**但 AMA 侧的绑定要一起清掉**——
`Config.Unbinder` 就是干这个的（实现是 `core.Service.UnbindFeishuApp`）。
早前漏了这一步：应用删了，agent 上的 `feishuAppIds` 还留着一个查不到的 ID，
于是详情页显示「已绑 1 个」而飞书页面上找不到它，用户没法自己解绑。

**一个应用只能绑一个 agent，但数据库挡不住。** 约束由 `core` 的
`ensureFeishuAppsFree` 在写入时拦（409 + 占用者名字），PostgreSQL 表达不了它
（不能对集合返回函数建索引，`text[]` 没有 GiST opclass，实测报错记在
`repository/schema/core_pg.sql`）。所以多实例下写入侧有一个很窄的 TOCTOU 窗口，加上约束是
后加的、库里可能有更早的 1:N 存量数据——`resolveBoundAgent` 因此**容错而不断言**：
多绑时仍取第一个（按 `ord`，与改造前一致，存量用户不会突然收不到回复），
但打一条 ERROR 把应用和全部候选 agent 点名报出来。存量修复 SQL 见
`scripts/fix-feishu-1to1/repair.sql`。

选「照旧回答 + 告警」而不是「拒绝回答」，是因为后者会把一个**配置问题**变成一次
**线上故障**：用户只会看到机器人不理人了，而那条脏数据可能已经躺了很久。

**会话不会过期回收。** `feishu_session_scopes` 只增不减，一个群聊上下文会一直攒下去。
`last_used_at` 已经记着了，将来要加「多久没说话就重开一条」时按它扫即可。
会话容器空闲会自己暂停、闲置久了被回收巡检删掉（见 `shared/infra/dockertools/README.md`），所以这不是资源泄漏，
只是上下文越来越长。

**恢复路径撤不掉 OnIt，于是会并列两个表情。** `reaction_id` 只活在上一条命的内存里，
重启后没有锚点能撤它（撤它要先列出这条消息的全部回应再按操作者筛，为一个表情多两次
远端调用不值得）。所以恢复时只补打 DONE，那条消息上 OnIt 与 DONE 并列。
**这比只留 OnIt 好**：用户至少能看出这一轮已经答完了，而不是以为还在处理。

## 测试

```bash
go test -race ./feishu/ ./httpd/
```

**没有数据库照样全绿**：碰持久化的用例会 `t.Skip`。想连着库跑就先起本地 PG：

```bash
docker compose up -d           # 仓库根，PG 17，端口 15432
go test -race ./feishu/        # 26 个 PG 用例从 skip 变成真跑
```

换个库用 `AMA_TEST_PG_DSN`。**每个用例建一个随机命名的 schema、结束时 drop cascade**，
所以跑测试不会碰你手动灌进 `public` 的数据。

除 PG 外全部离线：假飞书用 `httptest` 起，时间靠 `Config.Now` 注入（所以过期逻辑不用
真的等）。

**两个 Store 实现跑的是同一批断言。** `feishu_test.go` 里凡是碰持久化的用例都套了
`eachStore`，`mem` 和 `pg` 各跑一遍——断言只有一份，不然两个实现迟早会各绿各的。
注意 `eachStore` 注入的时钟**只给 Service、不给 Store**：两个 Store 内部都用真实时间，
去改它会让「时间往前拨」的用例在 Store 层就先把会话弄没了，Service 那句
`s.now().After(session.ExpiresAt)` 根本轮不到执行。Store 自己的过期语义由
`TestStoreExpiredSessionIsGone` 单独覆盖。

假飞书会**照抄真实的 HTTP 状态码**：poll 的协议状态返回 400 而不是 200。这一点很重要
——最早假服务器一律回 200，测试全绿，但真连飞书时轮询立刻挂掉。详见
`shared/infra/lark/README.md` 的「踩过的坑」。

**`dropped_test.go` 断言的是日志级别，不只是「有没有日志」。** 那组用例每条都验
`level=INFO`——只断言「有一行」的话，把 `log.Info` 改回 `log.Debug` 测试照样绿，
而线上会退回到「查不出消息有没有进来」的状态（那正是这组用例要防的东西）。
另有两条反向对照：正常触发的消息**不**打这条日志、以及限流真的在限
（删掉限流会让一个活跃大群刷爆日志）。四种变异都验过会变红。

真实连通性验证：起 `cmd/amad` 然后打真接口，见下。

## 本地跑

```bash
AMA_ADDR=127.0.0.1:8080 \
AMA_ALLOWED_ORIGIN=http://localhost:5273 \
AMA_API_KEY=dev-key AMA_API_KEY_EMAIL=你@example.com \
go run ./cmd/amad

# 下面的 curl 要带上这把 key（浏览器走登录页，curl 走 API Key）：
#   curl -H "Authorization: Bearer dev-key" ...

# 发起接入（会真的打飞书，不需要凭证）
curl -s -X POST http://127.0.0.1:8080/api/workspaces/ws-1/feishu/registrations | python3 -m json.tool
# 把返回的 verificationUrl 贴到浏览器里授权，然后拿 sessionId 轮询：
curl -s "http://127.0.0.1:8080/api/workspaces/ws-1/feishu/registrations/<sessionId>"
```

# core —— 六个资源域的业务模块

空间 / 成员 / Agent / 技能 / 方舟凭证 / 运行历史。飞书应用**不在本包**，在
`server/feishu`（设备码自助注册那条链路）；两者的交集只有一个 ID 字符串
（`Agent.FeishuAppIDs` 引用 `feishu.App.ID`）。

另有三个不属于那六个域的东西，各自有单独的文档：

| 域 | 文档 | 一句话 |
| --- | --- | --- |
| 授权管理（多 provider） | [README-integrations.md](README-integrations.md) | 账号级，与空间无关 |
| 新手引导 | `onboarding.go` 文件头 | 账号级，同一个人不被引导两次 |
| **问题反馈** | **[README-feedback.md](README-feedback.md)** | **两级采集边界 + 只给开发团队的读取口** |
| **产品建议** | **[README-suggestion.md](README-suggestion.md)** | **平台级（没有空间维度），人手写的意见 + 回复；授权全在 Service 层** |

⚠️ **改反馈的采集范围之前必须读 README-feedback.md**：那里有「一级采什么、二级采什么」
的确切清单，而一级是在用户不知情时发生的——多采一个字段就是一次未经同意的采集，
且界面上什么都看不出来。

## 契约的真源是前端

本包实现的是 `web/src/data/api.ts` 里 `ApiClient` 接口的服务端一侧。那个文件里的
`LocalApiClient`（localStorage 版）是**行为基准**——它的每条副作用、每句报错文案、
每个字段的有无，都是页面真实依赖的东西。

所以本包大量注释在解释「为什么照抄一个看起来奇怪的行为」。照抄是为了让前端从
`LocalApiClient` 换成 `HttpApiClient` 时**零改动**；偏离基准的地方都写明了理由。
改任何行为前先去读 `api.ts` 里对应的那几行。

## 分层

```
httpd 注册路由 → Handler（协议转换 + 错误映射） → Service（业务不变量） → Store（存取）
```

不变量放 `Service` 而不是 `Store`，因为它们大多**跨聚合**：删技能要清所有 agent 的
悬空引用、删空间要级联清五处。放 `Store` 里的话换 PG 实现得原样重写一遍，那样
`Store` 接口就是错的缝。`Store` 只做存取，不懂业务。

**授权不在本包**，在 `server/authz`（按空间成员角色，HTTP 边界的装饰器）。本包只
提供它需要的两样数据：`Service.RoleOf`（查角色，大小写不敏感）与 `Service.WorkspaceOf`
（把资源 id 反解成归属空间，给 `/api/agents/{id}` 那类扁平路径用）。
动手前先读 [authz/README.md](../authz/README.md)。

### 列空间有两个方法，分工不能合并

```go
ListWorkspaces(ctx)            // 全部空间，不过滤
ListWorkspacesFor(ctx, actor)  // 只给 actor 所在的空间
```

面向用户的接口（`GET /api/workspaces`）走**后者**，接错就把别人的空间列给用户看了，
而页面上完全看不出异常——只是多了几个不该出现的空间。

前者**必须保留且不过滤**：`core/worker.go` 的 worker 池巡检要扫全部空间的方舟绑定，
那条路径是进程级 ctx、**没有任何身份**。给它加上过滤会让它扫到 0 个空间，
于是每个 agent 一调工具就卡在 `requires_action`，且日志里什么都看不出来。
同类的无身份路径还有飞书入站与定时任务触发，见 authz 的 README。

另有 `GetWorkspaceByKey` **自己判成员**（非成员返回 `ErrNotFound`）——那是整个授权
方案里唯一写在 Service 层的一处，因为它的空间来自 query 参数，Guard 的 pattern
表达不了。回 404 而不是 403 是为了让前端 `nullable()` 照旧转成 `null`。

## 数据层有两份实现

**两份实现都不在本包**，接口留在这里（`store.go`）：

| 实现 | 位置 |
| --- | --- |
| `PGStore`（默认） | `repository/dao/coredao` |
| `MemStore`（`AMA_STORE=mem` 回退口，也是行为基准） | `repository/dao/coremem` |

⚠️ **`coremem` 刻意不 import `core`**——本包自己的内部测试要构造 `MemStore`，
它一旦反向依赖 `core` 就成环（`import cycle not allowed in test`，而 `go build`
照过、只有 `go vet` 报）。`coredao` 没这个约束，所以它可以 import `core`。
改那两个包之前先读各自的文件头。

**内存版是行为基准**，PG 版逐方法对齐它——
`store_test.go` 的每条断言都由 `TestStoreSuite` 对两者各跑一遍（各 64 条），
PG 那一遍在库连不上时整体 `t.Skip`，所以 `go test ./...` 对没起容器的人仍是离线可跑的。

`NewSeededPGStore` 只在**空库**时装种子（1 空间 / 4 agent / 4 技能 / 5 run / 77 事件），
非空时保留既有数据；`NewSeededMemStore` 每次起来都是种子，因为它本来也没有上一次的数据。

种子文件 `seed.json` **不要手改**——它由 `scripts/export-seed/export.sh` 生成，
改了下次导出就被覆盖。要调整种子内容请改 `web/src/data/seed.ts` 再跑导出脚本。

## 对话跑在真实的方舟上

对话页的三个接口（`POST .../conversations`、`POST /runs/{id}/events`、
`GET /runs/{id}/events/stream`）不是 mock，它们真的在驱动方舟 Managed Agents：

```
AMA Run  ←→  方舟 Session          （run.id 就是 session id）
RunEvent ←→  方舟 SessionEvent
```

**用 `cloud` 环境，不用 `self_hosted`。** 这是实测结论：`self_hosted` 下方舟拒收
agent 侧事件写入，worker 跑完 Session 一个字段都不会变，界面上永远出不来结果。
证据见 [`cmd/ama-worker/README.md`](../cmd/ama-worker/README.md) 的「天花板」一节。

代码分三处：`chat.go`（业务）、`arkrun.go`（打方舟）、`handler.go` 的三个 handler。
方舟资源的映射存在 `ark_bindings` 表里，见下面那条坑。

## 踩过的坑

以下每条都不会编译报错，只会在页面上表现成难查的怪问题。

**空闲的 worker 也在满速轮询，21 条循环就能把方舟账号的 RPM 打满。**
线上实测：7 个 workspace 的 worker 持续 429，连查四轮都复现，
每轮 70 分钟窗口 4574~5372 行 Warn。**看着像配额不够，其实是自伤。**

判据是把两个数放在同一个窗口里看：

```
poll work 失败 ... status 429   4159 条 / 59.6 分钟
领到 work / 工具执行 / tool event handled   全部 0 次
```

一条活都没干却把配额打满了，所以提额没有意义——它只会把同一份空转抬到更高水位。

算术：方舟的 `block_ms` 上限是 **999**（文档《长轮询拉取 work》明写「取值
1-999 毫秒」），所以「长轮询」最多挂住 1 秒。队列为空时若立刻再发一次，
空闲 worker 就是个 ~60 req/min 的定时器；线上 **7 个 environment × 3 个实例
= 21 条 poll 循环**，合计 ~1200 RPM。SDK 自己在队列为空时还会
`sleep(ctx, jitter(1s, 3s))`（`lib/environments/poller.go`），
我们照抄它的 poll 循环时**漏了这一句**。

**⚠️「退避了为什么还能刷出每小时 5000 行」——因为退避管的是错的东西。**
`failures` 成功一次就清零，而空转的 poll 大多数是成功的（返回空队列也算成功）。
实测退避档位分布 `2s×3561 / 4s×505 / 8s×79 / 16s×11 / 32s×3`，**86% 停在第一档**，
形状是「撞一次 → 等 2s → 成功 → 恢复满速 → 再撞」，相邻 429 中位数间隔 8.6 秒。
退避回答的是「失败之后重试多快」，而这里的问题是「没失败的时候也在满速发」，
所以**把退避上限调大治不了它**。

另一半是 `workerratelimit.go` 的闸门**当初刻意没包 PollWork**，理由写着
「PollWork 有 arkBackoff，再叠一层会打架」——**那个理由只在单进程下成立**：
`failures` 是每条 pollLoop 各一份的局部变量，21 条各退各的仍然合成一条稳定
高频流，正是那个文件头自己写下的形状。实证：线上「一起退避」这条日志 **0 条**，
即原有闸门从未生效过，4159 条 429 全部来自 PollWork。

判「是自伤还是配额不够」的通用手法：**在同一个窗口里数一次「有效工作」**
（领到 work / 工具执行）。分母为零的限流永远是自伤。

**终端按键随机丢一半，因为「哪个实例收到它」和「哪个实例握着 PTY」是两件事。**
线上实测：前端自动上报 `terminal_input_failed`，24 次 / 3 人，
`routePattern=unknown`、`httpStatus` 为空。根因是终端的下行 SSE 与上行
**每一次按键**是两条独立的 HTTP 请求，负载均衡各自随机分发，而 PTY 的 WebSocket
只握在开 SSE 的那个实例手里（`sandboxterm.go` 的进程内 `terminalHub`）。
同一条会话的日志摊开就是答案——**同一个 terminalId，两个实例各收到一半**：

```
实例 A（持有 SSE）  POST → 204 × 7
实例 B（没有）      POST → 404 × 8
```

命中率约等于 1/实例数。这个坑当年是**被明确「刻意接受」的**（那段注释还在，
说「丢了只是重开一次终端」）——线上数据证明那个判断是错的：用户根本不知道要重开，
他只看到自己敲的字不出现，一分钟报了 25 次失败。

**⚠️ 更要紧的是它在本地永远复现不出来。** `./dev.sh` 只有一个进程，每条 POST 必然
命中，所以「本地全绿」对这条路零证明力——与 CLAUDE.md 开篇那条「单实例跑测试全绿、
线上偶发」是同一类。能照出问题的只有显式构造「hub 里没有这条终端」的测试。

修法只有一个形状，因为另外两条更简单的路都被平台堵死了（真机实测，探针留在仓库里）：

| 想法 | 平台的回答 | 探针 |
| --- | --- | --- |
| 每个实例各连各的 PTY | `Session already has an active WebSocket connection`，随后写入即 close | `cmd/probe-term-reattach` |
| 上行改走无状态 `/v1/shell/write` | `404 Session not found`——它与 `/v1/shell/ws` 的 **id 空间不通用** | `cmd/probe-term-write` |

两条都堵死，才逼出「按键必须送到**持有那条 WS 的那个实例**」这个唯一形状：
本实例有就直接写（快路径），没有就投进 PG 队列（`shared/infra/pgmq`），由持有连接的那端
消费。延迟是量过才定的（`cmd/probe-term-relay`）：LISTEN/NOTIFY p50 0.08ms，
轮询 @20ms p50 10.9ms——**推比拉快两个数量级**，终端是逐字符交互，这个差别是手感。

⚠️ **顺带堵掉的一个越权口子**：鉴权判的是 URL 里的 `runId`，而真正决定「写进哪个
容器」的是 body 里的 `terminalId`——两者原先毫无关系。拿一个自己有权限的 run 过鉴权、
body 里填别人的 `terminalId`，就能把命令写进别的空间的容器。所以**两条路上都要校验
归属**：只在快路径上比的话，越权请求换个实例落地就绕过去了，而落到哪个实例是负载
均衡随机决定的。跨实例那端的校验落在消费侧（`relayHandler`）——**只有那一端知道这条
终端属于谁**。

⚠️ **`termID` 不能再用 `nextID`**：它现在是跨实例的队列键，而 `nextID` 是「进程内
自增」。两个实例各自从毫秒时间戳起头自增，撞上只是时间问题，撞了就是把一个人的按键
投进另一个人的终端。改成 `crypto/rand`（手法同 `arkWorkerID`）。

⚠️ **「不重复」不等于「猜不到」，这是变异验证逼出来的。** 给随机 id 写的第一版断言是
「200 个不重复 + 相邻 id 至少差 4 个字符」，把实现换回 `time.Now().UnixNano()` 之后
**照样全绿**——纳秒时间戳在紧循环里本来就不重复，相邻值的十进制表示也轻松差 4 位
以上。改成**按位统计熵**（每一位在 200 个样本里至少出现 4 种字符）才变红：单调
时间戳的高位在一次测试的时间跨度内是常量。用例见 `sandboxterm_test.go`。

**`if err != nil || len(x) == 0 { continue }` 会把故障伪装成「没数据」。**
`WorkerPool.refresh` 里那圈扫空间的代码从前就是这么写的：查 `ListArkBindings`
出错与「这个空间本来就没接方舟」走同一条静默 `continue`。两件事的语义完全相反——
后者是常态，前者是故障——合写之后**库抖一下就把那个空间整个跳过**：worker 起不来，
那个空间里每一次工具调用都卡在 `requires_action`，而**全过程一条日志都没有**。
巡检看到的是「agent 不干活」，完全不指向数据库。

判据很简单：**`err` 和「空集合」不能共用一个分支。** 出错要么往上抛、要么至少留一条
带上下文（是哪个空间、底层错误是什么）的日志；「空集合」才是那条安静的 `continue`。
用 `Warn` 不用 `Error`：下个周期会重试，单次失败不算故障，但一定要留痕。

这条是排查一次线上 500 时顺手挖出来的——那次线上 500 的唯一线索，正是它上面
`ListWorkspaces` 失败时打的那句 `Warn`。要是那句也被合并掉，整条链路就彻底不可查了。
用例见 `worker_refresh_test.go`（配对钉住两侧：失败要打、没绑定不许打，
只写前者很容易顺手把后者也打上，那条日志每周期每空间都会刷，真故障反而被淹掉）。

**会话会永久停在 `requires_action`（僵尸 run），因为没人负责给它收尾。**
线上实测：110 条 run 里 11 条卡在 `requires_action` 且在扩大，
最早一条卡了两天。三个活跃状态各有一条正常出路，但**没有一条保证走到**——
`requires_action` 等 worker 把 `user.tool_result` 交回来，而那条结果可能永远不来
（pod 在工具跑到一半时被换掉、work 被提前 `StopWork` 结算、容器配额吃紧建不出来）。

⚠️ **别按「哪个工具特殊」去查。** 当时的第一反应是「`schedule_wakeup`（那个已删除的
自唤醒工具）排完任务就结束 turn，它的 `tool_result` 回不来」——**实测证伪**：把 11 条的
`stop_reason.event_ids` 逐个对回 `agent.tool_use`，pending 的**全是普通的
`bash` / `read`**，一条 `schedule_wakeup` 都没有。真因与工具种类无关，
是「结果没回来时没有兜底」。判据是拉 `GET /api/runs/{id}/events` 把
pending 的 call id 与 `agent.tool_use` 对表，别只看最后一帧的类型。

代价有三层，第二层最贵：运行历史里僵尸越积越多；**`runwatcher` 每 30 秒把它重订
一次、每次白挂满 `watchTurnTimeout`（10 分钟）**，所以积得越多白占的方舟连接越多；
用户想接着聊也聊不了（方舟侧同样停在 requires_action，发消息回 400
`RequiresActionRejected`）。

修法是 `runwatcher.go` 的 `reapStaleRuns`：超过 `runStaleAfter`（30 分钟）没有任何
动静的活跃会话推到 `terminated`。**阈值取小了比不做还糟**——会把正在正常干活、
只是这一步慢的会话腰斩，而症状与真僵尸一模一样（都是一条 terminated），
反过来让人以为兜底没生效。下界是三段叠加：`arkReclaimOlderThan`（90 秒）+
工具执行天花板（120 秒）+ `watchTurnTimeout`（10 分钟）≈ 13 分钟，取 30 分钟留一倍余量。

⚠️ **回收必须是 CAS，不能「读出来改一改再 SaveRun」。** 列出候选与写回之间，
那条迟到的 `tool_result` 完全可能刚好到达并把 run 推回 running——覆盖写会把
**刚复活的会话重新按死**，且零报错（库里只看到一条 terminated，没人知道它活过来过）。
判据写进 `UPDATE` 的 `WHERE`（`RetireStaleRun`），谁先改谁赢；这同时让多实例
天然互斥，所以 Redis 选主在这里只是优化，正确性押在那条 SQL 上。

**存量不需要写迁移 SQL**：巡检启动先扫一拍，30 分钟阈值对存量僵尸天然成立，
第一拍就全收掉（幂等，重复跑无副作用）。

**`reapStaleRuns` 是 30 分钟后的「收尸」，不是救活；救活靠 `workerfailover.go`。**
2026-09-09 线上同一环境（fanzq）一小时内 ≥6 条 run 卡 `requires_action`、与成功 run
交错，暴露了 `reapStaleRuns` 兜不住的那一半：它只把僵尸**终止**，用户还是白等 10 分钟
收到「没回复」。这批卡死的形状是——worker **领了** work、心跳一直正常，但 SDK 的
工具事件循环（`SessionToolRunner`）在开始消费事件**之前**就挂住了（`GetSession` /
`envinit` 下技能 / `ListEvents` 持续失败）。单次工具执行有 120s 超时兜底会回结果，
所以「零 tool_result 挂 30 分钟」只可能是没走到执行；而心跳是 `HandleItem` 里独立的
goroutine，主流程卡住它照跳 → 方舟 90s reclaim（只抢「心跳过期」的 work）被心跳骗过去
→ 健康 worker 看着「有人在干」也不接手。

`workerfailover.go` 给每条 work 配一个看门狗：**工具调出去后超过
`workToolProgressBound`（180s，刻意 > 工具超时 120s + 容器冷启动 ~30s）还没有
`tool_result`，就判定执行者卡死**，然后 ① 拦住 SDK 收尾的 `StopWork`（workFailover
包装层，`abandon()` 后 StopWork 直接吞掉、Heartbeat 回错）② 取消这条 work。方舟于是
收不到终止、只看到心跳停了，90s 后把同一条 work **reclaim 重派给健康 worker 真跑一遍**
——这正是 pod 被 kill -9 时本来就走的自愈路径，只是从「靠进程死掉」变成主动触发。
阈值大于「工具最坏正常耗时」，所以触发时仍是 orphan 的 tool_use 必然连 `ResultStore.
Begin` 都没走到（没产生副作用），重跑安全，不会把一条命令执行两遍。

⚠️ **failover 的 work 收尾时不补错误 `tool_result`**（`worker.go` 收尾对账多了
`&& !failover.isAbandoned()`）：那些 call 要留给重派来的 worker **真执行**，补发错误
结果反而把「等重派」变成「收到一条假结果」。重派后的 worker 收尾时自己会再走对账。

### 工具结果账本必须放 PG、按 session 隔离（2026-09-10 毒丸串台）

SDK 执行有副作用的工具（bash 等）时，要记每个 tool_use 的执行状态（started/result/
sent），避免 worker 重启后**把工具再执行一遍**。SDK 默认的 `FileToolResultStore` 把账本
写在 worker **本地盘**，而且 `HandleItem` 固定按 `useWorkdirAsSession=true` 算工作目录
——所有并发 work 共用同一个根 `/tmp/ama-ss`，账本目录 `.ma_self_host_worker/tool_ledger`
也是**全局一份**。

这在我们的部署里两个层面都错（**这是分布式服务，pod 本地盘即抛、不共享**）：

1. **跨 pod 防不住**：work 被别的实例 reclaim 接手时读不到这个 pod 的 `/tmp`；
2. **更致命的是本地共享目录会「毒丸串台」**（线上 2026-09-10，fanzq 空间）：某条
   work 在工具结果**回传前被杀**，共享账本里留下一条 `state=result` 的固定 call
   （`call_qy982…`）。此后该 pod 每领到一条**别的 session** 的 work，runner 启动
   `Recover()` 都把这条外来结果抢先 POST 给新 session——新 session 没有这个 call，
   方舟回 **400 `Request payload is invalid`**（param 为空，看着像 body 格式错，
   实测是 call id 不属于该 session）。发送闸见 400 在 84ms 内判死整条 work，残留又
   永远不会被 MarkSent，于是**同 pod 后续所有会话接连中毒**，5 次 400 全来自一个
   pod、同一个 call id、跨 4 个 session。重建 pod（清 `/tmp`）才止血。

修法（`core/workerhandle.go` 自编排 HandleItem，替代 SDK 那个写死本地文件账本的
`HandleItem`）：

- **账本走 PG**（表 `selfhost_tool_results`，主键 `(session_id, call_id)`），
  `core/pgtoolresult.go` 把按 `(session,call)` 的 PG 账本适配成 SDK
  `selfhosted.ToolResultStore`；内存 Store 给一份 map 实现。跨实例可读（真正防重复
  执行）、pod 重建不丢。
- **Recover 只取当前 session 的行**——外来结果物理上不可能串台（这是毒丸的根治）。
- 工作目录也按 session 分（`<root>/<sessionID>`，给 envinit 下载技能用）。
- worker 池每 10 分钟清一次超过 1 小时的陈年账本行。

**排查这个故障靠的是 `workerapilog.go` 的全链路日志**（同一批改动）：第一次直接
照出了「worker 领到 work、装技能、SendEvent 400」——此前这一步是 SDK 黑盒。
诊断要点：多个不同 session 的 SendEvent **回传同一个 tool_use_id、且该 id 在这些
session 的事件里都不存在**，就是账本串台（而不是 body 内容问题）。


**worker 调方舟的全链路日志在 `workerapilog.go`**：领活/ack/建会话/下技能/拉事件/交结果/
心跳每个调用的**方法名、耗时、失败状态码**都用我们自己的 slog 打（SDK 内部那几步走
标准库 log，发版后在线上日志里查不到——这正是上面那批卡死「零报错」、排查只能反推的原因）。
只记元信息、**绝不记请求/返回正文**（消息、命令、路径、事件 JSON），失败打 Warn、
本该很快的调用超过 30s 也升 Warn（直接照出「卡在准备环节 / 事件读不动」）。

**`requires_action` 不是结束，给它写 `endedAt` 会把排查引向反方向。**
self_hosted 下**每次工具调用**方舟都推一帧 `idle + requires_action`，一轮对话里
出现很多次。老代码在 `applyEventToRun` 的 `status_idle` 分支里**无条件**调
`closeRun`，于是 run 同时带着「已结束的时间」和「没结束的状态」——而那两个字段的
契约恰恰是「未结束时缺失」（`DurationMs` 用指针正是为了区分「0 毫秒」与「没结束」）。

两处真实代价：详情页把「结束时间」显示成一个 agent 其实还在跑的时刻；而排查僵尸时
`endedAt` 有值会让人问「都结束了状态怎么没收敛」——**那是反方向**，真正的问题是
`tool_result` 没回来。上面那条线上 issue 的报告就是这么写的，绕了一圈。
现在只有 `end_turn` 那支才 `closeRun`。

**兜底回收要按「最后一次有动静」结算时长，不能按此刻。**
这是同一条 issue 的第二半，在回收上线**之后**才暴露出来：`closeRun` 内部写死
`time.Now()`，那对「收到终态帧就地收尾」是对的，但回收发生在会话真正停下来
**之后很久**（判僵尸要静默满 30 分钟，存量还积了几天）。于是记下的不是
「这条 run 跑了多久」，而是**「隔了多久才有人发现它卡住」**。

线上那批回收的真实形状：一条实际活跃 **13 秒**的会话被记成 **36.7 小时**，
另有 39.0h / 26.8h / 24.5h 若干，11 条全中。`durationMs` 是落库字段，
`RunsPage` 与 `OverviewPage` 的「时长」列直接渲染它，所以运行历史里就出现
一排「跑了几十小时」的巡检任务。

**零报错是这个坑的特征**：数字一直都在，只是全错，而且错得很像
「这些任务真的跑了很久」——没有人会去质疑一个看起来合理的数字。
现在 `closeRun` 拆成 `closeRun`（按此刻，给终态帧路径）与 `closeRunAt`
（按指定时刻），回收路径传 `lastActivityAt`（`retireEndedAt`，口径与
`storemodel.LastActivityOf` 一致，解析不出来才退回此刻——宁可偏大也不能丢
`endedAt`，缺了会让前端把已终止的 run 显示成「还没结束」）。

⚠️ **写这条的测试时，`startedAt` 与 `lastActivityAt` 必须取不同的值。**
既有的回收用例都写成 `StartedAt: tc.lastActive, LastActivityAt: tc.lastActive`，
真实时长≈0，于是「按 now 结算」与「按活跃时刻结算」都能过——这个 bug 正是
这样从一组全绿的用例底下溜过去的。

**存量那 11 条不自愈，但也不值得写迁移**：它们已经是 `terminated`，回收谓词
只认活跃状态，不会再被扫到。而 `durationMs` 没有任何聚合消费方（全站只有
两处按行渲染），影响面是「历史列表里 11 行数字偏大」，改它要写一次性 SQL
去动已终结的历史记录，代价大于收益。**新回收的不再有这个问题**，随着列表
往前滚它们自然沉底。

**超过 10 分钟的对话会断流，因为方舟 SDK 的 http.Client 自带 10 分钟超时。**
`arkrun.go` 的 `instance()` 只给了 `WithBaseURL`，看着「没设超时」，但 SDK 的
`NewClientConfig` 自带 `defaultTimeout = 10 分钟`，而 SSE 用的就是同一个
`http.Client`——于是长对话必断，日志里是「对话流中断」+
`Client.Timeout ... while reading body`，且 `cost` **精确卡在 `10m0.00x s`**
（毫秒级误差是指纹；用户关页面是随机时长）。修法在 `shared/infra/ma`：那边造两个 client，
SSE 走不设 `Timeout` 的那个，见 `shared/infra/ma/README.md` 的「踩过的坑」。
`arkCallTimeout`（30s）管的是同步调用，靠 context，与这条无关。

**方舟不发 `sequence`，得服务端补。** 实测方舟的事件（SSE 与 `GET /sessions/{id}/events`
两条路径都是）只有 `id` / `type` / `processed_at` / `session_thread_id`，**没有 sequence**。
但前端把它当必填：`timeline.ts` 按它排序，`undefined` 会让 `a.sequence - b.sequence`
算出 `NaN`，整条时间线的顺序变成不确定的；PG 那边主键还是 `(run_id, sequence)`。
所以由 `domain.NewRunEventFromArk` 按到达顺序补一个，且是**在原文字节里插一个键**
而不是解成 map 再 marshal——后者会重排键序、丢掉方舟新增的字段、触发 HTML 转义。

**SSE 转发的必须是「落库后」的字节，不是方舟原文。** 承上：补 sequence 发生在落库时，
转发原文的话，同一条事件在 SSE 里没有 sequence、在 `listRunEvents` 里有，前端按 id
对齐草稿时对不上，同一条回复会被画两遍。**这个坑单看代码两条路径都「对」**，
只有端到端跑真方舟才暴露。

**一条会话可以同时有多个订阅者，事件必须按 id 幂等落库，否则同一句话存好几行。**
`persistArkEvent` 是「收一帧 → 落一行」，而 `sequence` 由 `AppendRunEvent` 自己发
（当前最大值 +1）。订阅者却不止一个：飞书 inbound（`feishu/inbound.go`）、飞书 resume
（`feishu/resume.go`）、浏览器 SSE（`handler.go` 的 `streamRunEvents`）、定时任务
（`scheduler.go`）——**它们互不排斥**，`hasActiveStream` 只用来决定「开流前要不要补一次
历史」，从来不阻止第二个订阅者接上来。于是两条流各收到同一条事件、各拿一个新号、
各写一行，主键 `(run_id, sequence)` 一点都不冲突，两行安然入库。

实测现场 `sesn-20260101000002-ex002`：飞书起的会话，浏览器打开对话页那一刻起，
后续每条事件都存了 2 行——**同 id、同 `processed_at` 到纳秒、`raw` 除注入的 sequence
外逐字节相同**，界面上那句回复显示了好几遍。这类故障的指纹是「前 N 条正常、从某一
时刻起全部翻倍」，那个时刻就是第二个订阅者接入的时刻。

修法是让数据库裁决（CLAUDE.md 那张表里的「唯一约束 + `ON CONFLICT`」）：`run_events`
有 `event_id` 列 + 唯一索引 `run_events_event_id_idx`，`AppendRunEvent` 撞它时
**回读并返回已存在的那一行**。三个别顺手改坏的点：

- **两种唯一冲突要分开处理**：撞 `event_id` 是「这条事件已经有了」，返回那一行、
  重试没有意义；撞 `(run_id, sequence)` 才是「号被人抢了」，退避重读 max 再来。
  都当成撞号的话会白重试 50 次然后报一个不相干的错。
- **返回值必须是已存在那一行的 sequence**，不能是本次算出来的新号：转发给浏览器的
  字节里带着它，两个订阅者转发的号不一致，前端按 sequence 排序会把同一条消息
  排到两个位置去。
- **`MemStore` 要同步这条语义**。两边不一致更糟：修好 PG 却让内存版继续攒重复，
  `AMA_STORE=mem` 一回退问题就复活，而**单测用的正是内存版**——那等于测试永远
  看不见这个 bug。

`id` 为空的帧（`NewRunEventFromArk` 解不动的兜底分支）存 NULL、不参与去重：NULL 在
唯一索引里互不相等，那种帧无从判重，逐条存下来比吞掉好。

老库迁移的四步**顺序不能换**：补列 → 回填 → 删重复 → 建索引。`create table if not
exists` 对已存在的表什么都不做（所以要显式 `ALTER`），而老库里已经攒了重复行，
直上唯一索引会撞 23505，而 `migrate` 只咽 42P07/42710——表现是**升完这版 amad 起不来**，
报错指向建表、不指向脏数据。`cmd/probe-migrate-dedupe` 专验这条路径：单测的
`pgFactory` 每次都先 `Reset`/`Clear`，拿到的永远是新库，覆盖不到带脏数据的老库。

前端也有一份自己的责任：`useConversationStream.ts` 的 `mergeEvents` 曾经有两行
「一侧为空就直接返回另一侧」的短路，而首屏进对话页时本地事件必然为空，于是历史整份
直穿、它下半段的按 id 去重压根没执行。**后端推什么形状，前端都得自己收敛到「一个 id
只出现一次」**，否则重复 id 还会变成 React 同 key 的兄弟节点（`timeline.ts` 的 key
取自事件 id）。

**方舟的增量帧形状与前端要的不一样，必须翻译。** 方舟发的是顶层带 `type`：

```
{"type":"event_start","event":{"id":"sevt-x","type":"agent.message"}}
{"type":"event_delta","event_id":"sevt-x","delta":{"content":{"text":"我"}}}
```

而 `http.ts` 的 `parseFrame` 认的是顶层带 `kind`、id 在顶层、文本在顶层。直接透传的话
它会走兜底分支把每个增量帧当成一条完整事件，界面上出现几十条空消息、真正的回复反而
不显示。翻译在 `handler.go` 的 `clientFrames`。**最终事件则要逐字节原样透传。**

**方舟的 Environment 名字在一个 project 里唯一，绑定必须入库。**
重名建返回 409 `ResourceConflict`。「AMA agent → 方舟 Agent + 环境」的映射早前只放
进程内存，表现是**第一次跑得好好的、重启后 100% 失败**——缓存空了就拿同名再建一次，
那个空间从此开不了新对话，而错误信息指向方舟、不指向缓存策略。顺带每次重启还会多建
一个方舟 Agent（Agent 允许重名），越攒越多。所以有了 `ark_bindings` 表。
反过来，方舟侧资源被人删掉时绑定会失效，`CreateConversation` 认 404 后会清掉绑定重建
（自愈），否则那个 agent 永远开不了新对话。

⚠️ **环境 404 的自愈范围比 Agent 404 大：要连兄弟绑定一起改指。** 环境按空间复用，
同空间每个 agent 的绑定里都存着同一个环境 ID。旧版自愈只删自己那行，重建时
`ensureBinding` 会从兄弟行把**同一个死环境**捡回来，第二次建会话还是同一个 404——
2026-09-22 线上定时任务因此每小时失败一次，每轮还白建一个方舟 Agent。现在
环境 404（`InvalidEnvironment.NotFound`）走 `healMissingArkResource`：建新环境、
`RepointArkEnvironment` 定向 UPDATE 同空间所有绑定、原方舟 Agent 挂到新环境继续用；
Agent 404 才是删自己那行重建。两者是 CreateSession 上同一个 HTTP 404 的两种含义，
靠错误码区分（与 `ManagedAgentNotOpen` 同一个教训：只看状态码必然误判）。

**`writeTimeout` 会掐断 SSE。** `http.Server.WriteTimeout` 是**整个响应**的期限而不是
空闲期限，设 60 秒的话任何超过一分钟的对话都会在第 60 秒被硬切断，表现成「回复说到
一半就没了」，服务端日志里什么都看不到。`httpd` 因此把它设成 0。

**包一层 `ResponseWriter` 会藏掉 `Flush`。** `httpd` 的访问日志中间件包了
`statusRecorder`，不显式实现 `Flush()` 转发的话，`w.(http.Flusher)` 断言失败，
每一帧都攒在缓冲区里——页面表现为「一直转圈，回复整段才蹦出来」，不报任何错。

**序号要由 Store 发，不能调用方读一次最大值再加一。** 方舟的事件是并发推下来的，
分两步的话两个 goroutine 会算出同一个号。PG 那版早先写成 `on conflict do nothing`
看着「安全」，实际是**静默丢事件**：并发追加 16 条只活下来 1 条。现在是撞号重试。

**Go 1.22 的 ServeMux 拒绝有歧义的路由，比 Hertz 严格。**
`/api/workspaces/by-key/{key}` 与 `/api/workspaces/{workspaceId}/agents` 会让它在
**启动时 panic**：两者都能匹配 `/api/workspaces/by-key/agents`，而"谁也不比谁更具体"。
所以按 key 查空间走 `GET /api/workspace-by-key?key=`、查占用走
`/api/workspace-key-taken?key=`，用 query 参数从根上消掉这一类冲突。
（早前拿 Hertz 探过同样的形状、可以共存，于是误以为标准库也行——两个路由器的
冲突策略不同，别拿一个的结论套另一个。）

**时间戳必须固定三位小数（`.000Z`）。** 种子全部来自 JS 的 `toISOString()`。Go 的
`time.RFC3339` 与 `RFC3339Nano` 都会把 `.000` 抹掉。这不只是好看的问题：`listRuns`
按 `startedAt` 做**字符串**降序排，而 `.`(0x2E) < `Z`(0x5A)，两种形状混用会直接改变
排序结果。所以时间戳一律当不透明字符串存取，生成新的用 `domain.NowISO()`。

**nil 切片编码成 `null`，但契约要求 `[]`。** `FeishuPage.tsx` 读
`deleting.boundAgentIds.length`，拿到 null 直接 TypeError。切片字段一律不加
`omitempty`——加了更糟，会让 `members` / `skillIds` / `threads` 这些必填字段整个键消失。
数字与布尔同理不加：`TokenUsage` 的 0 是合法值（缺键会让前端算出 NaN），
`verificationTokenSet: false` 也是有意义的状态。

**agent 响应是字节敏感的。** `AgentDetailPage.tsx` 的 `isDirty` 是裸
`JSON.stringify` 字符串比较，所以 `ModelConfig` 的三个可选字段必须用
`string` + `omitempty` 而不是 `*string`：吐出 `"thinking":null` 会让保存成功后
「未保存改动」永久亮着。

**事件响应要手工拼原始字节。** 标准库（以及 Hertz 的 `c.JSON`）会把 `<` `>` `&`
转义成 `<` 之类，而 JS 的 `JSON.stringify` 不转义。事件里嵌着技能正文与工具入参
（用户可编辑的 Markdown / JSON 字符串），转义了就与方舟原始数据不一致。
`domain.AppendJSONArray` 因此绕开了 `encoding/json`；其余响应走 `Encoder` 并显式
`SetEscapeHTML(false)`。

**「404 → null」不能只看状态码。** 前端有五个 getter 返回 `T | null`，`HttpApiClient`
要把 404 转成 null。但路由写错时标准库返的 404 是 text/plain 的
`404 page not found`，按状态码判会把「路由挂错了」当成「资源不存在」，页面上表现为
静默的空状态。所以给 `/api/` 注册了兜底路由，未命中也吐同一个信封，且用
`code:"route_not_found"` 与真正的 `not_found` 区分开。**判断要认 code，不是状态码。**

**列表接口永不 404。** `listAgents` / `listSkills` / `listRuns` / `listRunEvents`
的返回类型都是数组不是可空，空间或 run 不存在时一律 200 + `[]`。同理
`getArkCredential` 未配置时回 200 + 空壳（前端靠空掩码判断「未配置」、靠 null
判断「加载中」，返 404 会让它永远停在骨架屏）。

**方舟凭证：key 无效不是 HTTP 错误。** 前端 `verifyArkCredential` 没有 try/catch，
返 401 会变成未处理的 rejection、页面上什么都不显示。所以 key 无效要以 200 +
`check:"invalid"` + 中文 `checkError` 返回；只有「凭证没配」和真正的传输故障才算
请求失败，且**传输故障时不改结论**——没验成不等于验失败了。

## 方舟凭证：全局配一次、空间引用

一把 key 可以配在**账号级**（`global_ark_secrets`，主键 email），空间选择引用它。

```
source = override   空间自己存了 key（默认档，也是这个功能之前的唯一行为）
source = inherit    空间**没存**，读时回落到 inheritFrom 那个人的账号级凭证
```

**联动是天然的，因为 inherit 时空间侧根本没有副本。** 改全局那一行，所有引用它的
空间下次解析立刻拿到新值——不需要同步逻辑，也没有「忘了同步」的可能。

⚠️ **绝不能改成「引用时复制一份到空间」**。那个改法看着更直接（读少一次查询），
但它让改全局永远不生效，而且**零报错**：空间设置页仍然显示「来自全局设置」，
只是那把 key 已经是旧的了。守住这条的不变量是「inherit 的空间行里不能有明文」，
由 `TestVerifyInheritDoesNotCopyKeyIntoWorkspace` 钉住。

解析的唯一入口是 `resolveArkSecret`。**三处写路径容易漏，漏了都不报错**：

| 位置 | 漏了会怎样 |
| --- | --- |
| `arkKey`（在 **chat.go**，不在 ark.go） | 设置页显示着好 key，一开对话拿到空串发给方舟，回 401 |
| `VerifyArkCredential` 写回 | 把全局明文复制进空间行，联动从此失效且界面看不出来 |
| `UpdateArkRegion` | 写出一行「有 region 没有 key」的记录，用户改了区域毫无效果 |

第一条真的漏过——`arkKey` 不在 `ark.go` 里，改的时候没搜到。

**存量数据不需要 DDL**：`source` 进 `doc` jsonb（不参与筛选排序），老行读出来缺这个
键就是空串，`domain.NormalizeArkSource` 收敛到 `override`（= 现状）。同飞书
`triggerMode` 的先例。⚠️ 空串**必须**落到 override 那一侧——落到 inherit 的话，
升级后所有已配置的空间会突然改用全局那把 key，而没有任何人点过任何开关。

**删全局不级联**改引用它的空间：它们变成「引用了一个不存在的凭证」，读出来是未配置
但 `source` 仍是 inherit，界面据此给指向全局设置的引导。级联是跨聚合批量写
（多实例下没有事务边界），而且用户多半只是想换一把新的。

**`ma.WithRegion` 对 BytePlus 无效。** SDK 的 `NewClientWithApiKey` 硬编码火山的
`volces.com`，`WithRegion` 只改 region 字段不改域名。拿合法的 `ap-southeast-1` key
去验会打到火山端点收 401、被误判成 key 无效。所以按区域显式给 `ma.WithBaseURL`，
URL 取自前端 `ARK_REGIONS`，两边必须一致。

**掩码与 key 校验按 UTF-16 码元数算长度**，因为 JS 的 `String.length` 是 UTF-16
语义。密钥都是 ASCII 时三种算法一致，但按 UTF-16 实现能保证万一贴进中文或 emoji
时两端算出同样结果。JS 正则的 `\s` 还比 Go 的 `unicode.IsSpace` 多认 U+FEFF（BOM），
而复制粘贴 key 恰恰容易带上它。

顺带一个 Go 本身的坑：**注释里写 BOM 的字面量会让编译失败**（`illegal byte order
mark`），得写成 `U+FEFF`。

**派生字段的两个位置陷阱（`Run.AgentDeleted`）。** 「关联 agent 已被删除」这件事没法
存——Agent 是硬删除、没有 `deleted_at`，所以它在 Service 读 Run 时现算
（`ark.go` 的 `markAgentDeleted`）。两个都不会报错的坑：

一、**`GetRun` 里必须标在最后。** 那个函数在 run 是 running 时会 `SyncRunEvents` 完
**重新** `s.store.GetRun` 覆盖掉本地变量，而 Store 直出的 run 上这个字段恒为 false。
标早了就被静默覆盖，表现成「有时标得出、有时标不出」（取决于 run 的状态）。

二、**enrich 过的 Run 绝不能交给 `SaveRun`。** `runs` 表是「标量列 + `doc jsonb` 存整个
实体」，`SaveRun` 把整个 Run marshal 进 `doc`——派生值一旦落库就成了永不更新的陈旧
快照（agent 后来重建也还写着「已删除」），而且**没有任何症状**。`AppendRunEvents`
回写的是 `s.store.GetRun` 的原始 run，不是 Service 那份，`chat_test.go` 有一条专门
钉住这件事。

**冻结要用 422，不能用 404。** agent 被删掉的会话不许再发消息，但那不是「资源不存在」
（run 明明在），而是「这个操作此刻不允许」，所以是 `ErrPrecondition`。用 `ErrNotFound`
的话前端 `http.ts` 的 `NOT_FOUND_CODES` 会把它转成 `null`，页面表现成静默的空状态。
另外**中断要放行**（`isInterruptOnly`）：方舟侧 session 可能还在跑，一律挡掉用户就永远
停不下来它。SSE 订阅同样不挡——只读是合理的，而且中断的回执就是从那条流回来的。

**SSE 的永久性错误必须在写头之前挡下来，回真 404。** 订阅一条不存在的 run，原先的做法
是照常 `WriteHeader(200)`，再把错误当成一帧 `session.error` 发出去、随后补 `[DONE]`。
当时留的结论是「真要挡得在 handler 写头之前预检——不值得」。**那条结论是错的，被线上
日志推翻了**：同一个失效 runId 在 6 秒内被订阅 3 次，每次 cost 不到 1ms、每次刷一条
ERROR（`.run/amad.log` 23:12:56~23:13:02）。

错在哪：`run 不存在`是**永久性**错误，重试永远不会成功；而 `200 + 流内错误`在 HTTP 层
与「连上了然后中途断线」**完全同形**，前端区分不出该不该重试。

关键事实是拿真 Chromium 实测出来的（别照文档推，也别凭直觉）：

| 服务端响应 | `onerror` 时 `readyState` | 浏览器行为 |
| --- | --- | --- |
| 404 / 非 `text/event-stream` | `2` CLOSED | **放弃，不再重连**（服务端只被打 1 次） |
| 连上后中途断开 | `0` CONNECTING | 自己按退避重连 |

所以**「无限重连」根本不可能是浏览器干的**——`[DONE]` 当时确实发出去了，前端也确实
`close()` 了。真凶在应用层：日志里每条 `/events/stream` 前面 40ms 都有一条 `/events`，
那是 `useConversationStream` 的 effect 整个重跑（切页签、重渲染、点回失效会话），
每跑一次就 `new EventSource` 一个。这一点在日志里可以直接分辨：**前面跟着 `/events`
的是应用层重挂，孤立出现的才是浏览器自己重连。**

改法是两侧各一半，缺一不可：

- 服务端 `streamRunEvents` 在 `WriteHeader` **之前** `RunExists` 预检，回 `404 +
  code:"not_found"`（预检用 `RunExists` 不用 `GetRun`——后者在 running 时会顺手
  `SyncRunEvents` 打一次远端，纯属白跑）；
- 前端 `subscribeRunEvents` 用 `readyState === CLOSED` 判出永久失败，回调
  `onPermanentError`，调用方收到就停手、不再重挂。

预检**挡不住全部**：预检通过之后、开流之前 run 被删的窄窗口仍会走流内错误那条，
所以那条分支保留着，不是冗余。日志分级也跟着分开——这两条都是「客户端拿着失效 URL
来订阅」，属于预期内的用户输入问题，记 WARN；**真正的流中断仍然是 ERROR**，
降级会把方舟断流、落库炸了这些真故障一起藏掉。

### 明文 key 加密落库（`shared/infra/cryptobox`）

两处存储（`global_ark_secrets.api_key` 与 `ark_secrets.doc` 里那个字段）的明文 key
落库前用 **AES-256-GCM** 加密，读出后解密。密钥是 `ark.secretKey`（base64 的 32 字节，
走配置文件或环境变量）。

**加密收在 `PGStore` 而不是 `Service`**：Service 里那些 secret 结构会在内存里流转、
被拼视图、被比较（`resolveArkSecret` 就拼了一个「看起来像空间凭证」的视图），
任何一处忘了解密都会把密文当 key 发给方舟。收在「进出库」这唯一的转换点上漏不掉。

密文格式带版本前缀，**这是能兼容存量明文的全部原因**：

```
v1:<base64(nonce || ciphertext+tag)>   加密过的
<其它任何形状>                          存量明文，Open 原样返回
```

于是**存量行零迁移即可继续工作**，写入一律加密——渐进迁移自然发生，上线顺序两个
方向都活。⚠️ 别改成「另加一列 `encrypted bool`」：那要改 DDL，且任何一处漏读那一列
都会把密文原样发给方舟，而方舟回的 401 说「API Key 无效」，完全不指向真因。

三条容易做错的地方：

- **`PutArkSecret` 不能改坏调用方手上那份明文**。加密写回入参的话，Service 存完之后
  拿同一个结构去 Verify 就会把密文发出去（`TestPutArkSecretDoesNotMutateCallerPlaintext`）。
- **没配密钥的实例读到密文行必须报错**，不能把密文当 key 返回。多实例下这是真实场景
  （滚动发布、漏配某个实例）——返回密文的话 401 指向「凭证无效」，而真因是少配了密钥。
- **`ark.secretKey` 不是 `ark.apiKey`**。后者是我们自己那把方舟钥匙，前者是用来加密
  *用户配的那些 key* 的。配串了不报错，只让加密静默不生效。

**换掉密钥不是「轮换」**，是让所有已加密的行解不开（症状：所有人的方舟凭证同时失效）。
真要轮换得加 `v2:` 前缀并保留 v1 的解密能力。

⚠️ 验「加密生效了没」**必须直查库**。用 Store 的读方法验不出来——它会解密，于是库里
是明文还是密文，读出来都对。这正是「忘了加密」能零报错通过全部功能测试的原因。
启动日志那行（`方舟凭证加密落库已启用` / 未配置时的 warn）是运维唯一的判据。

### 方舟派生资源归属于建它时那把 key

Environment / 方舟 Agent / 技能上传**都属于建它时那把 key 的 project**。所以复用的
边界不是「同一个空间」，而是「同一个空间 + 同一把 key」：

- `ArkBinding.KeyFingerprint` 记下这套资源是哪把 key 建的（`ArkKeyFingerprint`，
  走 `secret.Fingerprint`，**不是明文也不是掩码**——掩码只有 8 个有效字符，
  换一把前后四位相同的 key 会碰撞）；
- `ensureBinding` 复用兄弟绑定的环境时**只认同指纹的**；
- key 变了就当没有绑定，重建一套（旧的成为孤儿，同技能上传那个已知泄漏）；
- `ark_skill_uploads` 主键加了 `key_fingerprint` 一维。

这条不变量在 key 只有空间级的年代**自动成立**（同空间必然同一把 key），所以原来
的代码是对的。agent 级凭证一进来那个「必然」就没了：agent A 用 key1 建了 env-1，
agent B 用 key2 复用 env-1 → **拿 key2 去访问 key1 的 project**。方舟报凭证问题，
而两把 key 各自都是好的——报错完全不指向真因。`ark_skill_uploads` 的表注释早就记过
同类问题（「不按空间分会让 B 空间拿到一个自己 project 里根本不存在的 skill_id」）。

⚠️ **存量绑定（没有指纹）必须一律算「同一把」**（`sameArkKey` 的第一个分支）。
判成「不同」会让所有存量空间在升级瞬间重建方舟资源，而 Environment 重名撞 409 ——
表现是**全部既有空间当场开不了新对话**。这是本次改动最危险的破法，
由 `TestLegacyBindingWithoutFingerprintIsReused` 钉住。

⚠️ `worker.go` 里那句「agentID 只是为了复用 arkKey 的签名，worker 与具体 agent 无关」
**已经不成立**：三级凭证下不同 agent 解析出不同的 key，而 worker 要用的正是建出那个
环境的那把。传错 agent 会让 worker 起得来但 poll 一直失败、工具调用全卡在
`requires_action`。

#### 老库迁移的那个坑：`information_schema` 会跨 schema 串味

判「主键现在是几列、该不该迁移」时**不能**用不带 `table_schema` 的
`information_schema.key_column_usage`：集成测试的 schema（`ama_test_core`）与
`public` 里有同名表与同名约束，count 会把两边加在一起（3+4=7），于是
`if cols = 3` 永远不成立，**迁移静默跳过**——而 `ALTER TABLE` / `DO` 都返回成功。

症状离真因很远：老库主键没换，表现成「升级后换了 key 仍然复用旧技能」。
所以判定走 `pg_index` + `to_regclass('...')`（只认 search_path 里那一个表）。

发现它靠的是**专门造一个老库 schema 跑一遍迁移**再回查，而不是「本地没报错就算过」
——新库的表本来就是四列建出来的，在它上面永远看不出这个问题。

## 上传技能包（SKILL.md 或 zip）

`skillpkg.go`（解析 + 全部安全校验）+ `skillinject.go`（查会话该装哪些技能）
+ `shared/infra/shelltools/skills.go`（写进容器）。手填技能那条路一个字没改，上传是**并存**的
第二个入口。

### 技能包附件：`skill_files` 只存元信息，字节在 BlobStore（**这一节说的是 skill_files，不是问题/建议的附件**）

`skill_files` 表主键 `(skill_id, path)`，只有 path、大小与 `file_id`；**`content` 列已经删了**，
字节由 `file_id` 指到 `core.BlobStore` 里那一份（S3 兼容对象存储，`shared/infra/s3`；PG 不存字节）。`SkillFiles()` 读的就是那一份，
**没有回落**：`file_id` 为空是错误状态，读路径整批报错（绝不交出残缺的技能）。

⚠️ **没装配 `BlobStore` 时带附件的技能包上传会回 422**
（`service/skill/blobs.go` 的 `PutSkillFileBlobs`：没有地方放字节就当场失败，
不留半成品技能）。cmd/amad 总会装一个（S3 客户端；mem 档没配时内存实现），
所以这条只在单测里会碰到。

两张附件表（issue / suggestion）也是同一个形状：只有 `file_id`，字节在对象存储里。

注入容器（`skillinject.go`）每次会话要读一个技能的全部文件，读法是：

- **并发拉取**（`core/blob.go` 的 `hydrateSkillFiles`，上限 8）。实测 15 个文件
  串行 4.70s、并发 1.06s——往返次数不随文件数线性叠加。
- **元信息一次查完**（`store.GetFiles` 批量版），不是每个文件查一次 PG。

⚠️ **别绕过 `Service.SkillFiles` 直接调 `store.SkillFiles`**：那一层压根
不返回字节（只有 path 与 file_id），绕过去拿到的是一批零字节文件，而注入容器那条
路径是静默降级、不会有人报错。

**加列而不是并进 files 表**：这张表的主键 `(skill_id, path)` 就是它存在的头号理由
（jsonb 数组元素建不了唯一约束）。并进去要给 files 加 path 列与
`unique(owner_id, path)` 部分索引，而 files 是「一份文件一行、id 不透明」的形状。

另外两个决定：

- **不存本地盘**：线上多实例，每个实例的盘各自独立且重建即丢（见 CLAUDE.md）。
  注入容器发生在处理某条 work 的那个实例上，而上传发生在另一个实例——存本地盘等于
  「注入时找不到文件」，且不报错。
- **独立表而不是塞进 `skills.doc`**：一是要保证同一技能内路径唯一，而**jsonb 数组
  元素建不了唯一约束**（在 doc 里做只能「读出来在 Go 里查一遍」，那是 check-then-write，
  多实例并发下必然漏）；二是 doc 是 `ListSkills` 的返回值，字节塞进去会让列表响应
  随附件线性膨胀，而列表页只需要文件名。所以 `doc` 里的 `Files` 只有 path + size
  （`Content` 带 `omitempty`），字节不在 doc 里。

⚠️ **删列要跨两次发布**（`content` 列就是这么删的）：删列不向后兼容，删的那一刻若还有
旧实例在 select/insert 那一列就是一批 42703，而滚动发布期间新旧实例必然共存。
详见 `repository/schema/skill_files.sql`。

### 附件内容怎么给前端看：`GET /api/skills/{id}/file?path=`

技能详情页要能看技能里的每个文件，所以附件内容有一条 HTTP 出口
（`Service.SkillFileContent`）。**它与 `Service.SkillFiles` 是两个东西，别混**：

| | `SkillFiles` | `SkillFileContent` |
| --- | --- | --- |
| 给谁 | 注入容器那条链路 | HTTP / 详情页 |
| 范围 | 一次全部附件的全部字节（上限 2 MiB） | 按 path 取**一个** |
| 二进制 | 原样给 | 不给内容，回 `binary: true` |

四条不能改错的地方：

- **路径走 query 参数，不是路径段。** 附件路径含 `/`，做成 `{path...}` 会是本仓库
  第一个 wildcard 路由，而 authz 的 Guard 是按**单个** `PathValue(idParam)` 反解资源
  归属的。先例是 `GET /api/runs/{runId}/sandbox/file?path=`，照它抄。
- **`SkillFileContent.Content` 是 `string`（明文），而 `domain.SkillFile.Content` 是
  `[]byte`（序列化成 base64）。** 同一个概念两种编码，前端对前者再 decode 一遍会得到
  乱码。两处的字段注释都写了这件事。
- **二进制单独一个字段，不是往 content 里塞一句话**：那样调用方分不出「文件内容恰好
  是这句话」和「我们判定它是二进制」。判据是含 NUL 字节（复用 `isBinary`，与
  `ReadRunSandboxFile` 同一个函数——**别另写一份**）。
- **内置技能在查库之前短路回 404。** 它们零附件、也不在 `skills` 表里，走下去拿到的是
  「技能不存在」，而那句话是错的（它明明在技能列表里看得见）。

路径校验走 `CleanSkillPath`（`skillpkg.go`），判据与 `sanitizeZipEntry` 逐条对齐——
两者守同一个不变量「落进 `skill_files` 的 path 一定是规范化后的相对路径」，
漂移的后果是「上传拒掉的形状能从另一个入口塞进去」。
`TestCleanSkillPathMatchesZipEntryJudgement` 钉住这个对齐关系。

### 改附件内容：`PUT /api/skills/{id}/file`

`Service.UpdateSkillFile`。**只改已经存在的附件**，不新建 / 不删除 / 不重命名——
路径不在这个技能里回 404。这个限制顺带让条目数上限天然不可能被突破。

五条不能改错的地方：

- **内置技能的只读闸门要自己拦**（见下面「内置技能只读，要改走 Fork」）。这是本功能最容易漏的一处。
- **`SKILL.md` 必须显式拒（400）。** `skill_files` 里压根没有这一行
  （正文在 `Instructions` 里），放过去会插一条叫 `SKILL.md` 的附件，于是出现一份与
  `Instructions` 竞争的第二真源，而注入容器时它会与方舟侧那份打架。零报错。
- **落库是三步，顺序不能换**：写 `skill_files` → 重算 `doc.Files` 的 size →
  `SaveSkill`。**附件先写、doc 后写**：反过来的话中间窗口里
  agent 会按新清单去读还没落地的内容，而读不到不报错、只会让它反复重试。
- **第 2 步不能省。** `doc.Files[i].Size` 是上传那一刻算出来的，改了内容不同步的话
  界面上那个文件的大小**永远显示旧值**——而「N 个附件」这个数字是对的，所以看着
  一切正常，只有大小静默错。
- **上限一个都不能放大**（单文件 256 KiB、总量 2 MiB），理由见下一节。
  ⚠️ 写这两条的测试时注意边界：判据是 `>` 上限，所以「恰好等于」是允许的——
  第一版用例把 fixture 凑成正好等于，于是误以为实现没拦住。

改完内容指纹（`SkillFingerprint`）会变，于是下次开对话会重传方舟并 `UpdateAgent`
换掉 skills——**这是期望行为，别绕过**。版本号跟着 bump，与改正文对称（指纹刻意不含
`LatestVersion`，所以 bump 本身不引起无意义重传）。

⚠️ **已经在跑的容器里那份不会更新**：技能文件只在容器新建出来那一刻注入，
所以「改完立刻生效」只对**新建的会话**成立。这件事零报错（agent 读到旧内容只是
行为不对），所以界面上必须说出来。

### 上限是被注入通道倒推出来的，不是拍的

写进容器只有一条路：base64 + shell 重定向（`shared/infra/shelltools/file.go` 的 write 就是
这么干的），而单条命令有 512 KiB 上限（`maxWriteBytes`，因为 base64 跟着命令行走、
受 `ARG_MAX` 限）。base64 膨胀 4/3，于是：

| 限额 | 数值 | 依据 |
| --- | --- | --- |
| 请求体 | 10 MiB | `MaxBytesReader` 在**读之前**设好；请求体本身不是危险源，解压后的量才是 |
| 解压后单文件 | 256 KiB | ×4/3 ≈ 341 KiB < 512 KiB，给 `mkdir -p` 那段脚本留余量 |
| 解压后总量 | 2 MiB | 注入是串行的（SDK 逐条处理 tool_use），2 MiB 分批后是个位数次 exec |
| 条目数 | 64 | 每个文件都占注入脚本预算；条目多常常是「把 node_modules 打进来了」 |
| 单条路径 | 200 字节 | 路径要拼进 shell，短一点更安全 |

方舟侧宽得多（50 MB / 500 文件 / 单文件 25 MB）。**我们刻意收更紧**：方舟自己解压
自己用，我们要落 PG 再逐条注入容器。宽上限换来的不是能力，是超时。

放大单文件上限最危险：恰好超限的那个文件会**静默写不进容器**，表现成技能装上了、
附件缺一个，而 agent 读不到只会反复重试。

### 安全边界（zip 是不可信输入）

`skillpkg.go` 防四类攻击，每条都在 `skillpkg_test.go` 里做过**变异验证**
（把校验注掉、确认对应用例变红）——安全校验没有变异验证等于没验：

- **Zip Slip**：判据是 `path.Clean` 之后仍以 `..` 开头，**不是** `strings.Contains("..")`
  ——后者会误伤 `a..b.md`，且漏掉 `a/../../b` 这种规范化后才暴露的。反斜杠要先转成
  正斜杠：`path.Clean` 不认 `\`，`..\x` 在它眼里是个普通文件名。
- **zip bomb**：**绝不看 `FileHeader.UncompressedSize64`**（那是攻击者写的数字），
  一律 `io.LimitReader` 边解边数。
- **符号链接**：`entry.Mode()` **与** `ExternalAttrs` 高 16 位都要判。只判前者会漏——
  `Mode()` 仅在 `CreatorVersion` 标着 Unix 时才去解 `ExternalAttrs`，不标就返回 0。
  这个漏洞是测试抓出来的。
- **绝对路径**：`path.IsAbs` 只认 `/` 开头，Windows 盘符（`C:/x`）要单独判。

**有攻击特征的条目一律拒掉整个包，不是跳过它**——跳过的话攻击者可以把恶意条目夹带
在合法包里，上传成功且无任何提示。例外只有打包元数据（`__MACOSX/`、`.DS_Store`），
那是打包工具自动加的、用户控制不了，拒掉会让「在 Mac 上右键压缩」直接不可用。

`shared/infra/shelltools/skills.go` 的 `safeSkillPath` 是**纵深防御**，重复了一遍校验：注入是
把不可信数据变成容器里真实文件的那一步，它必须能独立判定安全，不能依赖「调用方一定
校验过」——那个假设一旦被新调用方打破，症状是容器里多出个 `/etc` 下的文件且零报错。

### frontmatter 缺失或非法一律报错，绝不静默用空值

静默的后果是：技能建出来了、`name` 是空串，方舟侧那个 `[a-z0-9-]` 约束在**上传到
方舟时**才炸，那时用户早就离开这个页面了。BOM 与 CRLF 要容忍——不容忍的话报错是
「缺少 frontmatter」，而用户看到的文件明明有（问题是开头三个看不见的字节）。

### 注入时机：容器建出来那一刻，且**重建也要**

钩子是 `session.initFresh`（`shared/infra/dockertools/session.go`），`ensureRunning` 每次
真建出一个新容器都调它：首次懒建、以及容器被删（回收巡检 / 手动 `docker rm`）后重建。

- **不能挂在 `ToolsFor` 上**：SDK 每领一条 work 都会调一次它，而纯文本回复的轮次
  根本不碰容器——那样会把「懒建容器」省下的时间又还回去。
- **重建那条最容易漏**：volume 也没了时工作区是重置过的。漏了的话首次对话一切正常，
  只有「隔很久再回来」的会话缺文件，而那个条件在测试里几乎不会自然出现。
- unpause / start 一个已有容器**不**重复注入：同一个容器醒过来，工作区还在。

注入失败**不阻断**容器可用，只记 Warn：技能正文走方舟的 system prompt、不依赖文件，
所以「读不到附件」比「整条会话不能执行命令」轻得多。但那条日志必须留着，否则
「技能文件为什么不在」无从查起。

注入用的是直接打容器 id 的 `rawExecer`，不走 `session.Exec`：后者开头会再取 `s.mu`，
而此刻正持有它，`sync.Mutex` 不可重入——走 `Exec` 会死锁。

### 「上传」没有成为一个 `SkillSource`

上传的技能 `source` 仍是 `custom`。加 `upload` 要同步改前端那个判别联合、seed 与
所有 switch，而「上传」描述的是**创建方式**、不是**来源**。带不带附件用 `Files`
是否为空判断。

⚠️ **这条判据不是「一律不加来源」。** 若将来有一种技能有自己的更新源、自己的
identifier 形状，就该加值——因为**只读闸门按 `Source` 判**，填 `custom` 会让它变成可编辑。

### 技能怎么到达模型：两条链路，缺一条都不完整

这一点极易被误解成重复劳动，所以先把分工写清楚：

| 链路 | 作用 | 实现 |
| --- | --- | --- |
| **方舟 Agent 的 `skills`** | 让模型**知道**有这个技能、何时该用它 | `skillark.go` + `skillzip.go` |
| **会话容器的文件系统** | 让工具**读到**附件字节 | `skillinject.go` + `shared/infra/shelltools/skills.go` |

只做后者，模型压根不知道技能存在，于是永远不会去读那些文件——文件在容器里躺着
没人碰，且零报错。只做前者，模型按 SKILL.md 的指引去 read 一个脚本，容器里没有，
read 回「文件不存在」，它换个路径试几次然后放弃。

#### ⚠️ 两条链路的**完整程度刻意允许不一致**

我们自己的技能包上限（100 MiB / 10000 文件，`skillpkg.go`）**比方舟宽**
（50 MiB / 500 文件 / 单文件 25 MiB，**外部硬限制、不会跟着我们变**）。
超出方舟那三条时**不再整包失败，而是只把 SKILL.md 传上去**（`BuildSkillZip` 判的）：

    方舟侧   只有 SKILL.md  → 模型仍然知道有这个技能、知道它干什么
    容器侧   完整的附件      → 工具仍然读得到那些字节

于是**「方舟控制台上这个技能没有附件」是预期行为，不是 bug**。模型照样能用它，
因为 SKILL.md 里写的那些路径在容器里都是真的。降级时打一条 Info 日志说明原因
（只记 Info 不记 Warn：这是正常路径，记成 Warn 会淹掉真正的异常）。

这个取舍换来的是「上千个文件的技能包也能装进来」——从前那种包直接被拒。

⚠️ **降级是全有全无**，不「挑几个小的带上」：半截的包会让模型按 SKILL.md 去读一个
方舟侧不存在的附件，那比只有正文更坏（同 `hydrateSkillFiles` 那条 starved 纪律）。

#### 附件字节怎么进容器：base64 逐文件，或打包 + 预签名 URL

默认逐个 base64 塞进 shell 命令行——那条路受 `MAX_ARG_STRLEN`（128 KiB）限，
几十 MB 的包塞不进去。所以 `BlobStore` 实现**同时**有签名能力（`BlobPresigner`）时，
改走：

    core 打包 → 传对象存储（key 是内容指纹，天然幂等、可缓存）
              → 签一条 30 分钟的预签名 URL
              → 容器自己 curl 下来 unzip

零附件的技能永远走 base64（几 KB 的正文，绕一圈对象存储反而慢且多两个失败点）。
`ensureSkillArchive`（`skillarchive.go`）负责打包与签名，失败**只降级不阻断**：
签不出来就回落到 base64，小技能照常可用。S3 适配层（`cmd/amad/s3store.go`）实现了
`BlobPresigner`，有编译期断言钉着；内存实现（mem 档）签不出，只有 base64 这条。

⚠️ **链接签给谁，决定了它打不打得通**：SigV4 把 Host 签进去了。Docker 会话容器拿的链接
签 `s3.sandboxEndpoint`（compose 里是 ama-net 上的 `http://ama-s3:9000`），用户自带机器
（`HostSkillsFor`）拿的签 `s3.publicEndpoint`——见 `core.LinkReader`。签错了对方下载 403 /
连不上，然后**静默回落成只写 SKILL.md**（附件全缺），只有一条 Warn。

⚠️ **容器侧用的必须是完整包，不能是降级包**：降级是给方舟侧用的，
容器恰恰是要拿到完整技能的那一端。用错了「容器里有完整技能」整个落空且零报错，
`ensureSkillArchive` 里有一道显式断言挡住这件事。

容器侧那四条「错了不报错」的细节（`curl -f`、字节数校验、解压层级、先清目录）
见 `shared/infra/shelltools/skills.go` 的注释。

**同一份技能会被装两次，这不是 bug。** SDK 的 `EnvironmentWorker` 每处理一条 work
都会调 `envinit.Setup`（`arkruntime/lib/environments/worker.go`），把方舟侧的技能包
下载解压。但它落在 **worker 本机**：`SkillsDir` 留空时默认 `<Workdir>/skills`，
而我们传的 `Workdir` 是 `workerRootDir`（`/tmp/ama-worker-sessions`）。
六个工具全是容器版，agent 看的是会话容器的文件系统，与那份毫无关系；
`envinit` 失败还是 warn-only，**全程零报错**。所以 worker 本机那一份是没人读的浪费
（几十 KB，暂时不管），容器里那一份才是 agent 真正用的——**别把其中一个"优化"掉**。

**注入的落点必须等于「容器 cwd + `/skills`」**，今天是 `/workspace/skills/<技能名>/`
（`docker.workdir` 默认 `/workspace`，工作区 volume 也挂在这里）。
不是随便挑的常量：**agent 按 `$workdir/skills` 找技能**，它发的是相对路径
`./skills/<技能名>/SKILL.md`，所以落点与 `ExecSpec.Cwd` 必须配套
（`dockertools` 拿同一个 `Workdir` 既当 cwd 又传给 `shelltools.InjectSkills`）。
self_hosted 的三篇文档一个字都没提 skills，所以注入这件事必须我们自己做——
但「自己做」不等于可以自己挑路径：**挑错了是零报错故障**。踩过两次：
落在 `/root/.ama/skills` 时模型按先验去读 `/mnt/skills/…`；改成 `/mnt/skills` 之后
平台又改成按 `$workdir/skills` 找，于是 agent 先 read `./skills/…` 失败、
再 `find /` 全盘搜才兜到旧路径（用户 2026-09-02 报的就是这个）。
两次的共同点都是**注入日志说成功、文件也确实在容器里，只是 agent 不去那儿看**。
⚠️ **改 `docker.workdir` 时两边一起变**，别在别处另钉一个 cwd 或落点，只改一半等于没做。

### ⚠️ 「配了技能，问 agent 有什么 skill 它说没有」——真出过，两重伤

上面那张表说的「只做后者」不是假设，它真的发生了整整一版，而且**零报错**：
技能包上传成功、`ark_skill_uploads` 有记录、绑定表里 `arkSkillIds` 三条齐全、
容器里文件也躺着——只有方舟 Agent 的 `skills` 字段是空的，于是模型压根不知道。

复盘的价值在于它有**两重独立的伤**，只修第一重线上仍然是坏的：

**第一重：字段在 `core → ma` 边界被静默丢弃。** `LiveArkRunner.CreateAgent` 里那个
`ma.AgentSpec{...}` 是逐字段手写的，漏了 `Skills`。上游 `chat.go` 算好了 refs、
下游 `ma` 的 `req.Skills = toSkillRefs(...)` 一直在，转换函数 `toMASkillRefs` 就摆在
同一个文件里——**只被 `UpdateAgentSkills` 用了，`CreateAgent` 从没调它**。
漏一个字段不会编译报错，方舟照样返回 200。

**第二重（更要紧）：指纹被无条件回写，把修复通道堵死。** `ensureBinding` 在建完
Agent 后无条件写 `SkillsFingerprint`,于是绑定声称「这一组已经挂上了」而方舟侧是空的
——**假账**。下次开对话命中既有绑定 → `syncBindingSkills` 比指纹 → 一样 → 直接返回、
零网络调用，那条真正会写对的 `UpdateAgentSkills` **永远不会被调到**。
所以光把漏掉的字段补上，已中招的 agent 永远不会自愈。

讽刺的是这个失败模式在代码里被写死了两遍警告，只是没人想到它对自己成立：
`skillark.go` 的「按『应该有的』算指纹会让失败被缓存下来，连重试都不会有」，
以及 `syncBindingSkills` 的「没更新成就不要回写指纹」——当年只在 update 路径上
贯彻了，create 路径漏了。

修法对应三处：

1. `CreateAgent` 把 `Skills` 传下去，并**返回方舟确认挂上的条数**
   （`ArkCreatedAgent.SkillCount`；实测建 Agent 的响应体带 `skills`，所以零额外往返）；
2. 只有 `SkillCount == len(refs)` 才回写指纹，否则留空 + 记 WARN——留空意味着
   下次开对话必然重试，**一次多余的往返换一条永远能自愈的路**；
3. 存量那批毒化指纹靠 `SkillsFingerprint` 的**版本前缀**（`v2:`）一次性失配，
   每条绑定下次开对话各自补挂一次。不走 SQL 迁移是因为建表脚本每次启动都跑，
   无条件 `update` 会让每个 agent 每次发布后都白打一次 `UpdateAgent`。

**为什么测试一条都没拦住**：断言全在 bug 上游那一层。`fakeRunner.CreateAgent` 收的是
core 层的 `ArkAgentSpec`，而它**确实被填对了**；`ma` 侧 `TestAgentSpecToRequest` 当时
只断言 6 个字段、既没设 `Skills` 也没断言 `req.Skills`。两侧各自都对，
中间那条缝没人测——`grep LiveArkRunner *_test.go` 是**零结果**。
现在 `core/arkrun_test.go` 用 httptest 起方舟替身、读**实际发出的 HTTP 请求体**，
并用反射盯着 `ArkAgentSpec` 的每个字段（`TestArkAgentSpecMapsEveryFieldToMA`）——
以后往那个 literal 加字段却漏传，会红并直接点出字段名。

查这类问题**别只读代码**：`go run ./cmd/probe-agent-skills -agent <id>` 直接把方舟侧
那个 Agent 拉回来数 `skills`，一步给出答案。它顺带打印 `tools` 做对照——
`tools` 有值而 `skills` 为空，正好说明「不是整体没传，是单独漏了一个字段」。

### 方舟侧技能的上传与去重

方舟**没有改技能/列表/删除接口**（`api/06-Skills/` 只有创建与查详情两篇），
`CreateSkill` 每次调用都产生一个新 `skill_id`。所以：

- 映射记在 `ark_skill_uploads` 表，主键 `(workspace_id, skill_id, content_hash)`。
  `content_hash` 由 `SkillFingerprint` 算（Name + Description + Instructions +
  每个附件的 path 与字节），**不用 `UpdatedAt` / `LatestVersion`**——那两个改个 tags
  也会变，用它们做键会让每次无关编辑都白传一份、在方舟侧堆一份清不掉的垃圾。
- 落库是 `on conflict do nothing` + **回读**。并发首次使用时两个实例各传一份，
  先落库的赢，输的那份在方舟侧成为孤儿。**这是刻意接受的**：方舟技能不占运行时配额，
  一份几十 KB；替代方案（先占位再上传）会让上传失败的占位行变成永久挡住重试的毒行。
- `type` 用 **`custom`**。API 参考里那句「一期仅支持 `skill_hub`」是**过时的**：
  官方指南与两篇 best-practice 都在用 `custom`，SDK 的枚举与 validator 都接受它，
  CreateSkill 的响应 `source` 本身就有这个值。与「方舟 thinking 文档说不返回思考
  内容、实测 69/72 条都带」同一类问题——**以 SDK 与实测为准**。
- **`source == skill_hub` 的技能直接跳过**（记 Warn）。它们的 id 是方舟 SkillHub 的，
  不是我们传出来的；而种子里那个 `s-hub-code-review` 是不是真实 id 没核实过，
  塞一个假 id 会让**建 Agent 就 400**、整条会话开不出来。代价只是预置技能暂时不生效
  （它今天也不生效）。

### 绑了技能立刻生效：靠 `ArkBinding.SkillsFingerprint`

**这一节是本功能最容易写错的地方。** `ensureBinding` 命中既有绑定就复用那个方舟
Agent，`CreateAgent` 再也不会被调——所以「第一次对话之后再绑技能」方舟侧压根不知道，
表现成「我绑了技能，agent 却当没这回事」，且零报错。

做法是在绑定里记一个指纹（`SkillsFingerprint`，整组技能的 hash），每次开对话比一下：

- 相同 → 什么都不做，**零额外网络调用**（绝大多数情况）。
- 不同 → 上传缺的技能 → 打一次方舟 `UpdateAgent` 换掉 `skills` → 回写指纹。

几条不能改的细节：

- **走 `UpdateAgent` 而不是删绑定重建**。重建会多泄漏一个方舟 Agent（方舟允许重名、
  删不掉），而且换掉 `ArkAgentID`——那之后历史会话指向一个不再更新的 Agent。
- **更新失败绝不回写指纹**。回写了会让下次对话直接命中指纹、连重试都没有，
  那个技能就永久不生效（零报错）。留着旧指纹意味着下次还会再试。
- **解绑最后一个技能要显式清空**（`ma.AgentUpdate.ClearSkills`）。方舟的 `skills`
  是整体替换语义，而「不传」是保留原有——传 nil 会让「解绑了」静默无效，
  模型还在用那个技能而界面上已经看不到它。
- `UpdateAgent` 带乐观锁，**版本冲突要重新取版本重试一次，不能当成功**。
  ⚠️ 这里原先写的是「冲突不算错，放过去就是对的」，理由是「别的实例刚写过，
  而它写的内容与我们要写的一样（同一份 AMA 数据算出来的）」。**那个理由只在
  「只有一种更新」的年代成立**——有了 spec 追平（见下一节）之后就不成立了：
  A 推 spec 时 B 推 skills 先写成功把 version 顶掉，A 撞 409 当成功 → 回写指纹 →
  方舟侧还是旧 system 而绑定声称是新的，又是一次「假账把修复通道堵死」。
  线上多实例，两个人同时开对话就会撞上。现在两条更新共用 `updateAgent`：
  撞 409 就重新 `Get` 拿新版本再来一次，仍冲突则**返回错误**（那时调用方不回写指纹，
  下次开对话还会再试，自愈通道留着）。
- **跨空间引用要拦住**。`DeleteWorkspace` 刻意不清理其它空间 agent 的悬空引用，
  所以 `SkillIDs` 里确实可能有别空间的技能 id。不拦的话会拿 A 空间的 key 把 B 空间
  技能的内容传上去，B 的内容出现在 A 的 project 里——那是静默越权泄露，不是脏数据。
- 上限 20 个：**建绑定时超限报错**（用户正在配置，看到错误有用），
  **绑定已存在时只降级**（每次开对话都走，拦下来会让人以为对话坏了）。

### 改了 system / model 也要追平：靠 `ArkBinding.SpecFingerprint`

**上一节那套推理对 `system` / `model` / 描述 / 工具逐字成立，而那半边一直没做**
（2026-08-31 修）。用户在界面上把系统提示词从 A 改成 B，方舟侧永远是 A。

它比技能那次更难查：

- **零报错**：PATCH 返回 200、库里真的改了，只有方舟侧没动；
- **永久**：技能那边任何一次增删都会让指纹变化从而顺带自愈，而 `system` 没有任何
  别的动作会碰到它——改一百次都是同一个结果；
- **报错不指向真因**：用户以为模型不听话、提示词写得不好，于是反复改提示词，
  每一次都同样不生效。

**先判定过这不是「刻意不做」**：`feishu/larkprompt.go` 里有句「Agent 的 system 是建
Agent 那一刻写死的……连更新路径都不碰 System」，读着像既定设计，但它是**把现状当障碍
陈述**——那段话真正的理由是「chat_id/thread_id 每条消息都不一样，所以不能进 system」，
与「system 能不能更新」无关。另三条依据：引入技能追平的那个 commit 通篇只讲技能；
`server/` 下没有任何注释给出「不同步 system」的理由；而 `ma.AgentUpdate` 早就有
`System` 字段并接到了 SDK 上，**一直没有调用方**——能力是齐的，只是没人接。

做法与技能同构：`SpecFingerprint` 存在绑定的 jsonb doc 里，版本前缀 `v1:`，
`ensureBinding` 命中既有绑定时在 `syncBindingSkills` 之后再走一次 `syncBindingSpec`。
存量绑定没有这个键（反序列化出空串，与任何真实指纹都不相等）→ 下次开对话自愈一次、
恰好一次，**不碰 DDL**（理由同 `skillsFingerprintVersion` 那段）。

三条不能改的细节：

- ⚠️ **必须发 `arkSpecFor` 算出来的那份完整 system，不是裸 `agent.System`。**
  进方舟的 system 是拼过的（用户正文 + clone 提示 + `<harness:env>` 块）。
  只发用户正文会把后两段**抹掉**——模型拿不到 env 里的运行时事实，也不知道该用
  部分克隆。**这是这条路上最容易写错的地方。**
  指纹同样按拼好的那份算，否则「只改了一个 env 变量」不会触发追平。
- ⚠️ **`Tools` 与 `CustomTools` 必须一起发。** 方舟的 `tools` 是整体替换，而内置
  工具集与自定义工具**挤在同一个数组里**，只发前者等于把自定义工具删掉，
  而方舟照样返回 200。`model` 同理是个对象（`{id,speed}`）
  整体替换，只填 `id` 会让 fast 档悄悄退回默认。`shared/infra/ma` 里已抽出共享的
  `modelConfig` / `toolItems`，让建与更新两条路**不可能漂移**。
  ⚠️ **当前一个自定义工具都没有**（唯一那个自唤醒已删除），所以这条约束眼下没有
  实际载体——但 `CustomTools` 的整条链路留着，再加自定义工具时它立刻重新生效。
- **两条追平各自独立，不要合并成一次 `UpdateAgent`**。指纹分开算，合并会让「改技能」
  顺带重发一遍 system；更要紧的是一次调用失败会把两件事一起拖下水，
  于是「技能没同步」与「提示词没同步」再也分不开。

**真机验证过**（`cmd/probe-agent-system`，打真方舟）：建 agent(system=V1) → 开对话
→ PATCH 成 V2 → 再开对话 → 从方舟 `Get` 读回。修复前 system 仍是 V1、version 停在 1；
修复后 version 1→2、V1 消失 V2 出现，再开一次对话 version 不再涨（指纹命中）。
（当时 tools 是 `[agent_toolset_20260401] [custom]` 两条；自唤醒删除后 `custom`
那条没有了，探针改成**与建 Agent 那一刻的基线比**而不是写死「必须有 custom」。）

⚠️ 写那个探针时踩了两个自己的坑，都表现成「看着像产品缺陷」：
`AgentDraft` 的 `builtin_tools` 是**唯一一个下划线命名**的字段（其余 camelCase），
拼成 `builtinTools` 会被静默忽略 → agent 没有内置工具 → 方舟侧 tools 里只剩
`custom`，看着像追平把工具集弄丢了；以及判断字符串写错（当时是把
`schedule_wakeup` 写成 `ama_wakeup`）会让「平台注入段落还在不在」永远是 false。
**判 tools 有没有丢，判据必须是「与建 Agent 那一刻的基线比」**，不是与想象中的应有值比。

### 失败一律降级，但必须留日志

上传或更新失败只记 WARN，对话照常开——只是这一轮模型不知道有技能。今天没有技能
对话也能跑，为一个附加能力把主功能拖下水是错的等价交换。与容器注入侧一致
（`injectSkills` 失败也只 Warn）。**日志必须留**，否则「为什么模型不用技能」无从查起。

### 还没验的那一段

以上全部是**离线验证**的（`fakeRunner` + MemStore/PGStore，含变异验证）。
**真链路没跑过**——要真 key + 真 project。最小验证路径：写一个 `cmd/probe-ark-skill`，
上传一个两文件的包 → 建带 skills 的 Agent → 建 Session → 发一条明确要求用该技能的
消息 → 看模型有没有引用技能里的内容。

一个已知的待确认点：SDK 的 `OpenSkillContent` 要求 `version` 非空
（`selfhosted/skill_hub.go` 显式报错），我们传的是 `CreateSkill` 返回的
`LatestVersion` 实值，所以应该没问题；但那条路径此前**从未被执行过**
（`SkillRefs()` 一直是空），第一次真跑时要盯一下 worker 日志。

## 内置技能只读，要改走 Fork

`builtin` 技能的正文**不可改**（`UpdateSkill` 回 `ErrPrecondition`）。要改走 `ForkSkill`
（`service/skill/fork.go`）：副本是 `custom`，可以随意编辑。

只读闸门放在 `Service.UpdateSkill` **一处就够**，已核实：`store.SaveSkill` 全仓库
只有一个生产调用方（就是 `UpdateSkill` 末尾那行），没有别的路径能改到 `skills` 的 doc。

⚠️ **「一处就够」只对正文（`skills` 的 doc）成立，附件那条路是独立的第二条写路径。**
`store.PutSkillFiles` 不经过 `UpdateSkill`，所以它**绕开上面那条检查**。
**`Service.UpdateSkillFile`（改附件内容）因此把检查重写了一遍**，不是冗余：
那边拦的是 doc，这边拦的是 `skill_files`。由 `TestUpdateSkillFileRejectsBuiltin` 钉住
（做过变异验证）。

判据很简单：**凡是能改到 `skills` 或 `skill_files` 的新入口，都要自己拦一遍
builtin。** 当前一共两个：`UpdateSkill`（正文）与 `UpdateSkillFile`（附件）。

### ⚠️ Fork 的路由形状：挂空间下，源 id 走 body

```
POST /api/workspaces/{workspaceId}/skills/fork   {"sourceId": "..."}   ← 对
POST /api/skills/{id}/fork                                            ← 错，会 403
```

后者的 authz Guard 要按技能 id 反解所属空间，而 `resource.go` 对内置技能前缀返回
`ErrPlatformResource`（authz 翻成「读放行、写拒绝」）——于是**fork 一个内置技能
永远 403**，而那个 403 完全不指向「路由形状选错了」，只会让人去查权限矩阵。

挂在空间路由下就绕开了反解：`workspaceId` 在路径里，Guard 不需要查库。

### `skill_hub` 那个枚举被删了

它曾表示「方舟技能市场」，但从来没有任何写入路径能产生它，库里唯一那条是种子里
手打的假数据（`instructions` 是句占位文案），而唯一读它的业务逻辑作用是**拒绝**它
——勾了它模型压根不知道。完整生命周期是「种子造一条假的 → 前端渲染一个『官方』
徽标 → 后端拿到就跳过」。

⚠️ `shared/infra/ma/agents.go` 里方舟自己的协议值 `skill_hub` **保留不动**，那是另一个轴。

## 定时任务（主动式能力）

`schedule.go`（业务）+ `scheduler.go`（常驻调度）+ `store_pg_schedule.go`（认领那条
SQL）。一次触发就是一条 `trigger` 为 `schedule` 的 Run，**刻意不新增「执行记录」
实体**——同对话页的手法，于是自动进运行历史、能看完整事件时间线。

### 认领：三条坑，全都只在真 PG 上暴露

线上 amad 是多实例的，N 个进程会在同一秒发现同一条任务到期。认领必须让**恰好一个**
赢，否则用户收到 N 份重复报表——**而单实例跑测试永远全绿**。

**一、`where id in (子查询)` 挡不住并发双领。** 三个筛选条件全放子查询、外层只剩
`id in (...)` 时，20 条任务 8 个实例并发，一半被两个实例同时领到。根因是 READ
COMMITTED 下的 EvalPlanQual 重检只看**外层** WHERE，而 `id in (已经算好的集合)`
与行的新内容无关、恒为真，于是后到者照样更新成功。

**二、把条件在外层重写一遍之后 `limit` 失效了。** 修坑一后 `explain` 显示计划变成
**Nested Loop Semi Join**：外层扫描驱动全部行，带 limit 的子查询被当成内层**逐行重跑**，
`limit 2` 一条没挡住，5 条全被领走。

**三、同一条 SQL 在 psql 里走 HashAggregate、只出 2 行——拿 psql 验过不等于验过。**
两边的执行计划可以不一样。这条最费时间：raw SQL 一直是对的，问题只在 Go 那侧复现。

现在的形状：CTE（materialized，只算一次、limit 真封顶）+ `for update skip locked`
（并发选行就互不相交）+ 外层重写业务条件（计划再变时 EPQ 重检看的是真条件，
刻意的冗余）。回归用例 `ClaimIsAtomicUnderConcurrency` / `ClaimLimit` **必须在真 PG 上跑**
（MemStore 那遍两个坑都碰不到），改坏的版本 1~10 轮内必现。

**租约跑完要显式释放，顺序是先 SaveSchedule 再 Release。** 不释放的话租约要 15 分钟才
过期，任何间隔短于它的任务（「每 5 分钟」）在那之前都领不到——表现是**间隔被悄悄拉长到
15 分钟，日志里什么都看不出来，界面上 `nextRunAt` 还是对的**。顺序反了则中间那一瞬
`next_run_at` 还是旧的（已到期）而租约已没，另一个实例会立刻领走再跑一遍。

### 方舟不会在 end_turn 之后关闭 SSE，得自己判结束并断流

`StreamRunEvents` **不会自己返回**：方舟按会话维持长连接，等下一条用户消息。
所以 fire 那条 goroutine 要自己认出「这一轮跑完了」（`session.status_idle` 且
`stop_reason` 不是 `requires_action`）然后取消 ctx。

不做的症状：agent 5 秒就回完了，而 fire 一直挂到 `fireTimeout`（10 分钟）才醒——
那期间租约一直被持有，于是「每 5 分钟」的任务实际变成「每 10 分钟」，
**而且日志里只看到「认领到任务」、永远看不到「执行完成」**。
实测修完之后同一条任务从 10 分钟降到 3.4 秒闭合。

**这个坑只有对真方舟跑才暴露**：`fakeRunner.Stream` 把帧发完就返回，
单测里 `streamDone` 立刻就闭合了，所有用例都是绿的。

### `next_run_at` 是 timestamptz，与本包其它时间戳的约定相反

本包的约定是「时间戳一律当不透明 ISO 串」（前端按字符串排序它们），但认领要
**让数据库当唯一时钟**：多实例的机器时钟有偏差，「谁是现在」不能由应用决定。
所以那一列是真 timestamptz，边界上与 ISO 串互转（`isoToTime` / `timeToISO`），
`doc` 里那份只是副本、**读回来以列为准**。

### ⚠️ agent 自唤醒已删除，别照着「加个上限」把它加回来

曾经有一个 `schedule_wakeup` 自定义工具（`core/wakeup.go`）：agent 遇到要等的事
（CI、部署、远端任务）时给自己排一条 `origin: agent_self` 的 oneshot 任务、
结束当前 turn，到点开新会话看结果。**连同 `trigger=wakeup` / `origin=agent_self`
两个枚举值一起删干净了**，存量数据由 `runs.sql` / `schedules.sql` 末尾的迁移改写。

**删的原因**：agent 把它当轮询用了。线上一个只配了「每小时一次」的定时任务，
近 200 条运行里出现 **84 次自唤醒**，最密时段稳定 3.5 分钟排一轮。每次自唤醒
= 一个新会话 + 一条新 work + 一个新 runner，于是它成了 429 限流的业务侧放大器。

⚠️ **别把它读成「频率没上限」而去加一个上限。** 那个工具本来就有下限（最短 1 分钟）
与上限（最长 7 天），实际用出来的 3.5 分钟远在合法区间内。真正的问题是
「让模型自己决定什么时候再跑一轮」没有反馈闸门——**模型每一次都觉得「再等几分钟看
一眼」是合理的，而那个判断在它自己的上下文里永远成立**，它看不到自己已经排了 84 次。

### 自定义工具：声明与实现是两件事

**当前一个自定义工具都没有**（唯一那个自唤醒已删除），但整条链路留着，
再加时这条约束立刻重新生效：

**内置工具集是固定的 8 个。** 在 worker 侧的 `toolset.Set` 里 Register 一个新名字，
方舟**根本不会把它告诉模型**——模型不知道有这个工具，永远不会调，**且不报任何错**。
必须两步：建 Agent 时声明 `{type:"custom",...}`（`arkSpecFor` →
`ma.AgentSpec.CustomTools`），worker 侧按同名注册实现（`worker.go` 的 `CustomTools`）。

**工具拿不到 session id**：SDK 给 custom tool 的签名里没有它，而 `CustomTools`
是进程级共享的 map（不像 `ToolsFunc` 按 session 建）。那六个容器工具能知道自己属于哪个
会话，靠的是 `ToolsFunc` 回调带的 workdir，这条路在 custom tool 上不存在。
自唤醒当年的解法是让 agent 把 `run_id` 当必填入参自己报出来。

**试过「每轮注入一条带 run id 的 system.message」，撤掉了**：`chat_test.go` 有一条
「上行事件要原样转给方舟，不做二次加工」的断言，那不是过时的测试——前端 `sendMessage`
取 `accepted[0]` 当作刚发出的用户消息画到界面上，多一条就会把那句提示显示成用户自己
说的话。当时改成了写进 Agent 的 system 提示词。

### 错误文案别把库的原始错误放在末尾

`handler.go` 的 `userMessage` 取的是**最后一个** `": "` 之后那一段。robfig 的解析错误
自带冒号（`failed to parse int from 99: strconv.Atoi: ...`），拼在末尾会让用户看到
「99）——五段格式是…」这种半句话。这条是 handler 用例抓出来的。

## 怎么跑

```bash
cd server
go build ./... && go vet ./... && gofmt -l .   # 最后一条输出为空才算过
go test -race ./core/                          # 离线，不需要任何凭证

# 起服务（AMA_DEV_ROUTES=1 才注册 _debug/reset|clear，别在线上开）
AMA_ADDR=:8477 AMA_DEV_ROUTES=1 AMA_ALLOWED_ORIGIN=http://localhost:5273 go run ./cmd/amad
```

已实测跑通的关键路径：

```bash
B=localhost:8477/api
curl -s $B/workspaces                                    # 列空间
curl -s "$B/workspace-by-key?key=growth"                 # 按 key 查
curl -s "$B/workspace-key-taken?key=growth&exceptId=ws_demo"   # {"taken":false}
curl -s $B/workspaces/ws_demo/ark-credential             # 未配置也 200，是空壳
curl -s "$B/runs/run_1/events?types=agent.message&types=agent.tool_use"  # 重复参数
curl -s "$B/runs/不存在/events"                           # 200 []
curl -s $B/agents/不存在                                  # 404 + code:not_found
curl -s $B/乱路径                                         # 404 + code:route_not_found
curl -s -X POST $B/_debug/reset                          # 恢复种子（dev 才有）
```

**事件响应已与前端导出的种子逐字节比对通过**（5 条 run 共 77 个事件全部一致），
`workspaces` / `agents` / `skills` / `runs` 四个域同样一致。这个断言在
`model/apimodel` 的测试里固化成了 golden test。

方舟校验用真实 key 打通过：真 key 判 `valid`、假 key 正确归类为
`ErrArkKeyInvalid`（而不是网络故障那一档）。

**对话链路已对真方舟端到端跑通**（2026-08-26）。完整一轮：

```bash
B=localhost:8099/api
curl -s -X PUT $B/workspaces/ws_demo/ark-credential -H 'Content-Type: application/json' \
     -d "{\"apiKey\":\"$ARK_API_KEY\",\"region\":\"cn-beijing\"}"
RUN=$(curl -s -X POST $B/workspaces/ws_demo/conversations \
      -H 'Content-Type: application/json' -d '{"agentId":"agent_triage"}' | jq -r .id)
# 先订阅再发消息（前端也是这个顺序）
curl -N "$B/runs/$RUN/events/stream?event_deltas=agent.message&event_deltas=agent.thinking&actor=u-dev" &
curl -s -X POST "$B/runs/$RUN/events" -H 'Content-Type: application/json' \
     -d '{"events":[{"type":"user.message","content":[{"type":"text","text":"..."}]}]}'
```

实测一轮 338 帧（2 个 `event_start` + 325 个 `event_delta` + 9 个最终事件 + 心跳 +
`[DONE]`），agent 给出了真实回复。收尾后：

- `GET /workspaces/ws_demo/runs` 里能看到这条，`status: idle`、`durationMs: 43743`、
  `usage` 四个字段都累加对了、`eventCount: 9`、标题由首条消息回填；
- `GET /runs/$RUN/events` 回放出 9 条事件，`sequence` 1–9 连续；
- **SSE 里的 9 条最终事件与回放的那 9 条逐字节相同**（这条是刷新还原能对上的前提）。

自愈也验过：手工在方舟侧删掉绑定的 Agent 后再开对话，服务端认出 404、清掉绑定重建，
对话照常开出来。

## worker 并发处理 work：不能用 SDK 的 `Run`，也不能用它的 `WorkPoller`

**一条 work 就是一轮对话。** 方舟的 poll 接口**每次最多返回一条**
（`selfhosted.PollWorkRequest.MaxItems` 已标 Deprecated，注释原文「MA poll 每次最多
返回一个 work」），所以并发只能靠「领一条 → 扔出去 → 立刻回去领下一条」。

### 为什么不用 `EnvironmentWorker.Run`

它是**串行**的：`lib/environments/worker.go` 里 `for poller.Next() { w.runClaimedWork(...) }`
——没有 `go`。而一条 work 要等到会话空闲满 `MaxIdle`（默认 **60 秒**）才退出
（`SessionToolRunner.Next()` 阻塞读 channel，channel 由 idle 计时器关闭）。

于是**同一个 environment 下第二个人发消息要干等第一个人空闲**。而环境是按空间建的
（同空间 agent 复用一个），所以症状是「同事在聊，我发消息半分钟没反应」——
零报错，本地一个人测永远碰不到。

### ⚠️ 为什么也不能用 `WorkPoller`（这条是静默故障）

`WorkPoller.Next()` 第一行就是 `p.runPendingStop()`，它会对**上一条** work 发
`StopWork`（`poller.go:81` 与 `:177-186`）。串行模型下那是对的（上一条已经跑完），
但并发下第二次 `Next()` 会把**还在跑的**第一条标记成终态：

```go
for poller.Next() {           // ← 这里给上一条发 StopWork
    go worker.HandleItem(...) // ← 而上一条还在这个 goroutine 里跑
}
```

那条 work 在方舟侧结束了，我们跑完再 `postResult` 就是往一个已结束的 work 回传结果
——**agent 卡在 `requires_action`，而 `StopWork` 与 `postResult` 都返回成功**。
这个 `pendingStop` 没有开关能关掉。

所以 `pollLoop` 自己调 `PollWork` / `AckWork`（都在公开的 `selfhosted.API` 上），
`StopWork` 交给 `HandleItem` 内部的 defer 按每条 work 各自结算。
`TestPollLoopNeverStopsWorkStillRunning` 钉住这条，**做过变异验证**：换回
`WorkPoller` 时它会报「有 work 在仍然运行时被 StopWork」。

### ⚠️ 进程重启会打死在途对话（两个坑，缺一个都救不回来）

**症状**：对话跑到一半永久停住。库里 `agent.tool_use` 比 `user.tool_result`
多一条，最后一条事件是 `session.status_idle{stop_reason: requires_action}`。
页面上就是「说到一半没了」，**全程零报错**。

真实案例 `sesn-20260101000006-ex006`（46 个工具调用都正常返回，第 47 个卡死）：

```
13:07:45.799  worker 停止       ← ./dev.sh 重启，ctx 被取消
13:07:48.929  agent.tool_use    ← 模型又要调工具，已经没人在听
13:07:50.970  worker 池启动      ← 新进程起来了，却永远领不回这条
```

**坑一：优雅退出被 SDK 当成「这条 work 干完了」。**

`handleItem` 的 defer 会判断要不要发 `StopWork`，而判据是 heartbeat 的停止原因：

| cause | 发 StopWork？ | 什么时候 |
| --- | --- | --- |
| `lease_lost` / `heartbeat_lost` / ... | **否** | 所有权不确定，别乱动 |
| `""`（空） | **是**，且 `Force: true` | ← **ctx 被取消走的就是这条** |

`ctx` 一取消，`heartbeatLoop` 直接 `return`，那条路**不调 `markStopped`**，
于是 cause 是空串，`shouldStopItem("")` 落到 default 返回 true。
**最该保留 work 的路径，被归进了「正常结束」**——方舟侧那条 work 进终态，
新进程再也领不到它。

`EnvironmentWorkerOptions` 里没有开关能关掉这个 defer（11 个字段没一个与 stop
有关）。唯一的接缝是 `NewEnvironmentWorker(api, ...)` 吃的那个接口，所以
`shutdownSafeAPI` 包在 API 层拦掉退出途中的 `StopWork`。

⚠️ **判据必须是 worker 的 ctx，不能是传进来的 `stopCtx`**——SDK 那边用的是
`context.Background()`（`stopItem` 里现造的），它在退出时照样能发出去。

**坑二：poll 从不带 `reclaim_older_than_ms`，抢不回没人管的 work。**

护栏只覆盖「走了 defer」的优雅退出。进程被 `kill -9` 时 defer 根本不跑，
那条 work 停在「已认领但没心跳」，**只有带 reclaim 的 poll 才领得回来**：

```
优雅退出（SIGTERM，走 defer）→ 护栏不发 StopWork → 新进程 reclaim 领回
硬杀（kill -9，不走 defer）  → 本来就没发       → 新进程 reclaim 领回
```

阈值取 90 秒 = SDK 心跳 TTL（30 秒）的 3 倍。**下界不能再小**：取小了会把
正在跑、只是心跳慢了一拍的 work 抢走，那是双跑（两个容器、两份 `tool_result`），
比卡死更糟。

三条用例钉住这两件事，**都做过变异验证**：
`TestPollLoopDoesNotStopWorkOnShutdown`（护栏没接上就红）、
`TestPollLoopStillStopsWorkWhenRunning`（护栏拦过头就红，正常结束必须照常结算）、
`TestPollWorkCarriesReclaim`（漏了 reclaim 就红）。

前两条走的是**真的 `pollLoop` + 真的 `EnvironmentWorker`**，让 SDK 自己那个
defer 跑起来——只测 `shutdownSafeAPI.StopWork` 这个函数是不够的，那正是
workdir 那次的教训：两侧各自都对、接线错了，中间没人看。

### 坑三：`tool_use` 与 `tool_result` 对不上，而没有任何一处负责补

**症状**：一轮对话永久停在 `requires_action`，库里 `agent.tool_use` 比
`user.tool_result` 多几条，最后一条事件是
`session.status_idle{stop_reason: requires_action}`，`event_ids` 里正是那几条缺结果的
call id。30 分钟后被 `reapStaleRuns` 收成 `terminated`，用户看到「说到一半就没了」。

真实案例 `sesn-20260101000007-ex007`（`trigger=schedule`，87 条事件）：
`agent.tool_use` **15** 条、`user.tool_result` 只有 **12** 条，缺的是最后同一瞬间
（`01:33:20.409`）并发发出的三个 bash（seq 82/83/84）。

⚠️ **别把这个归给「并发」——同一条 run 里有反例。** 更早的三批并发
（2 条、5 条、5 条）**全部正常回传**：

```
01:31:24.774  并发 x2   answered=2/2
01:31:41.060  并发 x5   answered=5/5
01:32:32.022  并发 x5   answered=5/5
01:33:20.409  并发 x3   answered=0/3   ← 只有这一批全丢
```

`SessionToolRunner` 对同一 session 是**串行**处理 `tool_use` 的（`processListedEvents`
里顺序调 `handleToolUse`，`executeWithTimeout` 阻塞等结果），`pollLoop` 的并发是
**work 之间**的、不是「一个 turn 内多个 tool_use 之间」的。所以「并发把结果冲掉了」
这个方向是错的，照它查会绕远路。

**真因是那个 runner 已经不在听了。** SDK 的正常路径本身是可靠的（串行处理、
`FileToolResultStore` 做重启续跑、`retrySendEvent` 用 `context.Background()` 保证
已执行的结果有一次有界投递机会），但整条链路都挂在「runner 还活着」这个前提上，
而它有好几种断法，**每一种都不经过 `postResult`**：

- runner 因 idle 超时（`MaxIdle`，默认 60 秒）退出后，方舟又发了 `tool_use`；
- work 的 lease 丢了 / 心跳掉了 → `workCtx` 被 cancel，`runLoop` 直接返回；
- 事件流断在「`tool_use` 已下发、还没被消费」那一瞬；
- pod 被换掉（滚动发布、缩容、健康检查失败）。

**修法：每条 work 收尾时对账一次**（`workertoolaudit.go` 的 `auditToolCalls`，
挂在 `pollLoop` 里 `HandleItem` 返回之后）。拉一遍该 session 的事件，用
**call id 的集合差**（不是条数差——条数说不出是哪几条）找出孤儿，给每个补一条
`is_error=true` 的 `user.tool_result`。

放在这里的判据是「谁手上同时有 session id 和一个还能用的 API」：runner 退出后它
自己什么都做不了，而这个点是**无论 runner 因为哪种原因退出都一定会经过**的唯一位置。

⚠️ **三个不能改错的细节**：

1. **对账要用不带取消的 ctx**（`context.WithoutCancel` + 自己的超时）：它要在进程
   关停途中也能把话说完，只需要有界，不需要跟着上游一起被取消。
   ⚠️ **不是因为「`HandleItem` 会取消传进去的 ctx」**——它取消的是自己派生的
   `workCtx`，外层那个不受影响。这条注释第一版就写错了这个理由，记下来免得下次
   有人照那个错理由改动。
2. **进程退出时不要补**，判据是 `workerCtx`（`pollLoop` 顶部那个别名）。那时那条
   work 是**故意留着等新实例 reclaim** 的（见坑一），补发会抢在 reclaim 之前把那一轮
   推进成「没能执行」——把一次本来能自愈的重启变成一轮真的失败。
   `TestPollLoopSkipsAuditOnShutdown` 钉住这条。
   ⚠️ **判据用 `workerCtx` 而不是裸 `ctx` 是刻意的。** 每条 work 的 goroutine 里
   很可能出现一个 per-work 的 ctx（比如给 `HandleItem` 单独派生一个，好在 work 收尾后
   掐断它自己的重试），而**那种 ctx 正常结束时也会被取消**。判据一旦被它遮蔽就恒为假，
   兜底静默变成死代码——**零报错**，日志里一条「工具调用没有回传结果」都不会出现，
   看起来就像「线上很健康」。`TestAuditSurvivesPerWorkCtxCancellation` 专门拦这个
   （已做变异验证：模拟遮蔽后它与接线用例一起变红）。
3. **自定义工具要用 `user.custom_tool_result` + `custom_tool_use_id`**。
   用错类型方舟不认，那一轮照样卡着，**而日志会说「已补发」**——排查的人会据此把这条
   路径排除掉。

⚠️ **这是兜底，不是根因修好了。** 补发的是一条错误结果，agent 会看到「这个工具没能
执行」然后自己决定重试还是换路——比永久卡死好，但**不等于工具真的跑了**。所以兜底
每次触发都打 Warn 并带上「发起 N / 已回传 M / 补了哪几条 call id」，让「根因修好了」
与「兜底在默默打扫」在日志里**分得开**（本仓库的经验：兜底型修复不留可区分的痕迹，
两者都让告警消失，事后没人说得清是哪一个）。

**判据是搜 `工具调用没有回传结果` 这条日志：0 条说明这条路从没走到过。**
排查这次故障时最大的障碍正是这个——那三个 call id 在日志里**一条痕迹都没有**，
于是「方舟发起了几个 / 我们回传了几个」这笔账根本对不起来。

⚠️ 日志只记 call id 与工具名，**不记 `input` 正文**（bash 命令、文件路径都是用户
数据）。`TestOrphanDigestCarriesNoToolInput` 用一条带可识别入参的 fixture 钉住它
——「不泄漏 X」这类断言极容易恒真，fixture 里入参是空的话，把它打进日志的变异照样全绿。

九条用例覆盖这件事，**四个变异逐一验过**（都能编译、都变红、还原成修复后那份）：
把 `auditToolCalls` 改成直接 `return 0`（7 条红，含接线用例）、只补第一条孤儿其余静默
丢弃（3 条红）、把 `is_error` 改成 `false`（假成功结果，1 条红）、
绕过退出护栏（2 条红）。

#### ⚠️ 这个对账**覆盖不到** `sesn-20260101000007-ex007` 那次，别以为已经修好了

线上日志把那次的时间线钉死了（变更记录里查到一次滚动发布，
17:28:54Z–17:34:48Z，**那 3 条 call 就发在这个发布窗口里**）：

```
17:31:07Z  w9qzt  领到 work
17:31:26Z–17:32:44Z  w9qzt  12 条「tool event handled」，全部 posted=true
17:32:48Z  w9qzt  收到退出信号 → 进程退出中，保留在途 work 不结算（等新实例 reclaim）
17:33:20Z  ★ 模型发出那 3 个并发 bash —— 此时 w9qzt 已经没了，无人在听
17:33:56Z  m6lqz  开始常驻订阅（只是 SSE，不是 worker 领 work）
18:03:26Z  m6lqz  回收卡死的会话（stale_after=30m）
```

三条判据说明**执行侧压根没碰过那 3 条**（所以不是「领了没执行」也不是「回传失败」）：
`tool event handled` 恰好 12 条且全部 `posted=true`、那 3 个 call id 全库 0 条
（阳性对照 `call_lo8qha4rvu9v7zhgs2oqh8u4` 搜得到 1 条，所以「0 条」是有效证据）、
`工具执行完成` 也只有 12 次。heartbeat / lease / `event stream` / `worker 异常退出`
相关**全 0 条**——是干净的 SIGTERM，不是出错退的。

**所以那次的缺口在 reclaim：这条 work 一次都没被重新领回来。**
8 小时窗口内 `领到 work` 全量 34 条，`work_id=…lmzsm` **只出现 1 次**（就是 17:31:07
那次）；而同期别的 work 被领 3 次、5 次，**说明 reclaim 机制本身是好的**。

而本节这个对账**刻意在进程退出时跳过**（上面护栏 2），因为那时补发会抢在 reclaim
之前把那一轮推成「没能执行」。于是这条路径**天然在覆盖范围之外**——
它兜的是「runner 因别的原因退出」，不是「pod 走了且 work 再没被领回」。

⚠️ **「为什么 reclaim 没接手」目前没有直接证据，别在这上面编故事。**
`PollWork` 返回空**不打日志**，所以方舟侧为什么不把它派回来，日志里查不到。
两个待验的方向（**要打方舟 API 才能定，读日志定不了**）：

- 那条 work 可能已经不在可派范围。文档《长轮询拉取 work》只说
  `reclaim_older_than_ms` 是「超过该时间未 heartbeat 的 work 可被其它 worker
  重新认领」，**没有说明护栏跳过 `StopWork` 之后那条 work 处在什么状态、
  以及那个状态还算不算可派**。我们 90 秒的阈值按文档看是合规的
  （> SDK 心跳 TTL 30 秒），所以「阈值取小了」这个方向缺证据支持。
- 也可能 17:33:20 那 3 条 `tool_use` 到达时，方舟侧压根没有为它们**新建/重排**一条
  work（原来那条已被我们认领且在它看来仍属 `w9qzt`）。这条最符合观测：
  新 pod 一直在 poll 同一个 environment，却一次都没领到东西。

⚠️ **查这类问题时的方法坑**：日志查询如果默认只覆盖一部分机房 / 区域，
另一部分的日志会静默丢掉。**任何「0 条」结论都必须把每个区域各查一遍**才成立。

### 几个不能改错的细节

**先占槽位再 poll，顺序不能反。** 反了的话领到的 work 会卡在等槽位上，而方舟那侧
已经把它派给我们了——lease 在手却不干活，直到超时被别的 worker 抢走
（`ReclaimOlderThanMS`）。那是一次白白的重试，日志上看不出原因。

**闭包要捕获 work 的副本**（`it := *item`）。下一轮 poll 会写 `item` 指向的内存，
持有指针会让 goroutine 读到别人的 work。

**ack 失败就不能处理这条 work。** ack 是 queued→starting 的竞争操作，失败意味着无法
证明它归我们，硬跑下去会出现两个实例处理同一轮对话——两个容器、两份 `tool_result`。
404/409/412 是正常竞争（别人抢到了），不该打 warn。

**并发上限的判据是 Docker 宿主机扛得住几个同时干活的容器，不只是 amad 的 CPU。**
每条在跑的 work 可能各持一个会话容器。默认 20，
配置项是 `worker.concurrency`（环境变量 `AMA_WORKER_CONCURRENCY`）。
**配 1 退回串行**，是并发调度出问题时不发版就能止血的手段。

### 附带查清的一件事：`MaxIdle` 可配，但不该改大

`EnvironmentWorkerOptions.MaxIdle` 是 `*time.Duration`，nil 时落
`selfhosted.DefaultMaxIdle`（60 秒）。所以它**能配**——但把它改大是两头变差：

容器暂停发生在 work 收尾的 `defer tools.Close()`，`MaxIdle` 只决定「多久之后才走到
那个 defer」。改大买到的好处几乎为零（Docker 的 unpause 是毫秒级，`stop` 模式下重新 start 也就一两秒），
代价却是那条 work 的 goroutine 与 heartbeat 循环多占同样长的时间。
拿实测的工具耗时分布看（P50 4.3 秒），不划算。所以保持默认。

### 还有一个改不了的天花板：SDK 的 `ToolTimeout` 是 120 秒

`executeWithTimeout`（`selfhosted/session_tool_runner.go:823`）在**每次工具调用外面**
包一层 `context.WithTimeout`，取 `SessionToolRunnerOptions.ToolTimeout`，
留空落 `DefaultToolTimeout` = **120 秒**。而 `EnvironmentWorkerOptions` **没有这个字段**
——`worker.go` 构造 runner options 时不传它，所以我们**配不了**。

这意味着单条工具调用的上限就是 120 秒，`shared/infra/shelltools` 那边把超时改到 5 分钟也没用
（SDK 那层会先掐掉，报错是 `tool execution timed out after 2m0s`）。要突破得改 SDK。

## 还差什么

**没人订阅时事件不落库。** 事件的持久化挂在 SSE 那条链路上（转发与写库是同一处），
所以「发完消息就关掉页面」的那一轮，agent 的回复不会进历史。当前形态下对话总是有人
看着的，所以先这样；要补的话得起一个后台订阅者，那就要先回答「谁来负责、跑在哪个
实例上」。

**中断只是转发给方舟，不等它停。** `POST /runs/{id}/events` 带 `user.interrupt` 会
原样转给方舟，返回 200 就完了——真正的停止由方舟异步执行，前端靠事件流里的
`session.status_idle`（`stop_reason: user_interrupt`）知道它停了。

**种子里的 `fs_1` / `fs_2` 在 feishu 那侧不存在。** core 的种子（来自前端 `seed.ts`）
带着两个飞书应用的引用，而 `feishu` 包的库里没有它们，所以
`GET /api/feishu/apps/fs_1` 返回 404，`boundAgentIds` 也就无从填起。
这不影响 core 这六个域，但**前端 agent 详情页的「绑定飞书应用」那一块会看到空列表**。
两个模块各自的种子怎么对齐，需要和飞书那条链路一起定。

**身份是占位的。** 建空间时自动写入的 owner 来自 `Config.DefaultOwner`，默认值是
`LocalApiClient` 里写死的那个人（e2e 的 05-settings 断言了它）；方舟凭证的
`updatedBy` 取的是空间里第一个 owner 的邮箱。SSE 那条路径更弱一档——`EventSource`
不能带自定义头，身份只能走 `?actor=`。真实认证落地后这两处要一起改。

**方舟侧的资源不会被回收。** 删空间时只删本地的 `ark_bindings`，方舟那边的 Agent 与
Environment 留着（这里没有 key，也不该在删空间时去打外部服务）。攒多了要手工清，
或者另做一个带凭证的回收任务。

## 三级方舟凭证：agent → 空间 → 账号级

一把 key 可以配在三个层级，**越具体越优先**：

```
agent 级   agent_ark_secrets（主键 agent_id）    ← 最优先
空间级     ark_secrets（source=override/inherit）
账号级     global_ark_secrets（主键 email）      ← 兜底
```

解析的唯一入口是 **`resolveArkSecretForAgent`**。它与 `resolveArkSecret` 刻意是
**两个函数**而不是一个带可选 agentID 的函数：

```
设置页要展示「本空间的默认是什么」        → resolveArkSecret(ws)
对话与 worker 要用「这个 agent 用哪把」   → resolveArkSecretForAgent(ws, agentID)
```

合成一个的话调用方会分不清自己拿到的是哪一级——而有 agent 级凭证时那是两把不同的
key，用错的表现是「设置页显示一切正常，某个 agent 的对话却报凭证问题」。

### agent 级「没有记录」就是「跟随空间」

这一点与空间级**刻意不同**：空间引用账号级那份要求一次显式点击
（`UseGlobalArkCredential`），因为那是「改用别人的 key」，不该让人猜。而 agent 默认
跟随所在空间是它一直以来的行为——要求点一下才「跟随」会让所有既有 agent 在升级后
突然没凭证可用。

所以 agent 级**没有 inherit 这个存储态**：有行 = 自定义，没有行 = 跟随。
界面上「改用空间设置」就是 `DELETE`。

### 不变量扩展成「下级不存任何上级的副本」

跟随时下级根本没有行，每次读都现取——所以改上级立刻联动。这条在三级下有**三处**
容易破，且破了都不报错：

| 位置 | 漏了会怎样 |
| --- | --- |
| `arkKey`（chat.go） | 走了不带 agent 的解析入口 → agent 级凭证被静默忽略，账记在别人头上 |
| `VerifyAgentArkCredential` 回写 | 把上级明文抄进 agent 行 → 改上级从此不联动，界面仍显示「跟随空间设置」 |
| `UpdateAgentArkRegion` | 跟随时不拒绝 → 写出一行「有区域没有 key」，用户改了毫无效果 |

三条各有测试钉住，其中两条做过变异验证（见 `core/agentark_test.go`）。

### `ArkEffective`：把「到底用了哪个」直接说出来

三级配置最容易让人困惑的就是「我配了三个地方，到底用了哪个」。所以 agent 级的对外
视图里带一个 `effective` 摘要（`level` 为 `agent` / `workspace` / `global`，
`global` 时还给 `ownerEmail`）。

⚠️ **判 level 不能从 `sec` 反推**：`resolveArkSecretForAgent` 拼出来的视图里
`Source` 表达的是「相对它自己那一级」，空间级 override 与 agent 级 override 在那个
字段上长得一样。要从最具体的那一级依次问起，与解析顺序一致。

### 删 agent 要删它那把 key

agent 是硬删除、没有 `deleted_at`，所以那一行没有别的机会被回收。更要紧的是**串味**：
agent id 由进程内自增生成，删掉再建有可能撞上同一个 id，那时新 agent 会**静默继承**
上一个 agent 的凭证——能跑，只是账记在别人头上。两个 Store 实现都要做。

## 谁建的 / 谁改的：`CreatedBy` + `UpdatedBy`

**四个实体有这两个字段：Agent / Skill / Workspace / Schedule。** 下面先讲 Agent
（参照实现），另外三个照它抄，差异记在后面那一节。

⚠️ **Issue / Suggestion / IssueComment / SuggestionReply 刻意没有这两个字段**——
它们早就有 `Author`，语义与来源（只从 `auth.ActorFrom(ctx)` 取）完全一样。
再加一个 `CreatedBy` 会变成两个字段说同一件事，然后必然有一天两者不一致。
`feishu.App` 同理已有 `CreatedByID`。**加字段之前先 grep 一遍它有没有别名。**

`domain.Agent` 上有这两个字段（邮箱），命名与语义照 `ArkCredential.UpdatedBy` 来。

**只从 `auth.ActorFrom(ctx)` 取，`AgentDraft` 里刻意没有这两个字段**——请求体是
调用方能改的，让它指定「谁建的」等于允许冒名。

### ⚠️ 存量 agent 永远补不上创建者

这两个字段是后加的，在那之前建的 agent 当时没记，而且**无从考据**（jsonb doc 里
没有，方舟侧也没有）。所以：

- 两个字段都带 `omitempty`，**「键不存在」是一种会长期存在的合法状态**，
  含义是「不知道是谁建的」，与「空字符串」必须分得开；
- 前端据此渲染成问号头像并明说「未记录创建者」，不是空白也不是错误；
- **不要为了整齐给存量行补一个猜出来的值**（比如空间 owner）——猜错的归属
  会被当成事实读，而它不报错。

不加 `omitempty` 还有个立刻会红的后果：`TestSeedRoundTrip` 拿 seed.json
反序列化再序列化做**逐字节**比对，而种子里这两个键不存在。

### 哪些写入路径要盖章，哪些**刻意不盖**

|                        | 盖 `UpdatedBy`？ | 为什么 |
| ---------------------- | --- | --- |
| `CreateAgent`          | ✅ 连 `CreatedBy` 一起 | 用户建的 |
| `UpdateAgent`          | ✅ **只动 UpdatedBy** | `CreatedBy` 是读回来那份，不能被后来的改动者顶掉 |
| `SetAgentIntegrations` | ✅ | 改授权也是一次真实修改 |
| `UnbindFeishuApp`      | ❌ | **级联清理，且根本没有身份**（见下） |
| `DeleteSkill`          | ❌ | 同上，级联清理不是用户编辑 |

后两条与它们「不 bump version、不动 updatedAt」守的是同一件事。
`UnbindFeishuApp` 的理由更硬：它由飞书模块在删应用后触发，`auth.ActorFrom`
在那里**恒为空串**，写了只会把已有的留痕**抹成空**，且不报错。
`core/agentactor_test.go` 里那四条钉着这些边界。

### 另外三个实体也有这两个字段（Skill / Workspace / Schedule）

同一套语义、同一套 omitempty 理由，照 Agent 抄的，**没有另发明一套**。
盖章边界见下表，全部由 `core/entityactor_test.go` 钉住（12 条，做过 7 次变异验证）；
存储层的往返由 `repository/dao/coredao/store_actor_test.go` 钉住（两个 Store 各跑一遍）。

| 实体 | 盖章的路径 | 刻意不盖的路径 |
| --- | --- | --- |
| **Skill** | `CreateSkill`、`CreateSkillFromPackage`（上传/Fork 共用）、`UpdateSkill` | `DeleteSkill` 的级联 |
| **Workspace** | `CreateWorkspace`、`UpdateWorkspace`、`AddMember`、`UpdateMemberRole`、`RemoveMember` | —— |
| **Schedule** | `CreateSchedule`、`UpdateSchedule`、`SetScheduleStatus`、`RunScheduleNow` | `Scheduler` 的到点重排 |

四条踩过 / 想清楚的：

- **`CreateSkillFromPackage` 必须单独盖一次。** 它不是 `CreateSkill`——只在后者盖章
  的话，上传与 Fork 出来的技能**全都没有创建者**，而技能列表看起来
  完全正常。两条各有一个用例，变异验证时删掉一个另一个不红，证明它们真的独立。
- **成员那三条写路径各自 `SaveWorkspace`。** 只盖 `UpdateWorkspace` 的话，管理员改了
  一屋子人的权限、留痕里一个字都没有。这与「增删改成员也算这个空间被改动过」
  （`UpdatedAt` 那条既有注释）是同一个判断，只是当时只落到了时间上。
- **空间的「谁建的」不能用「第一个 owner」代替。** 建空间时两者同值，所以
  `owner.Email` 能过测试——但 owner 可以转让、可以有多个、也可以把自己移除。
  所以取 `auth.ActorFrom(ctx)`。
- **存量任务这两个字段可能是空的，那不是漏盖。** 早期的任务本来就没记；已删除的
  agent 自唤醒任务更是绕过 Service 直接落库的（那条链路上没有人类身份）。
  **别拿「`CreatedBy` 空不空」当判据**去推断任务的来源。

### ⚠️ 为什么没有 `DeletedAt`（软删除）——这是一次显式的取舍

「创建者 / 创建时间 / 更新时间 / 删除时间」听起来是一组四件套，**但第四个与前三个不是
一个量级**：前三个是纯增量（加字段、写入时盖章，读路径一行不用改），而 `DeletedAt`
一旦引入，**每一条读路径都要跟着加 `where deleted_at is null`**，漏一处的表现是
「删掉的东西又出现了」或「删了还占着唯一约束」，零报错。

实测量化过，这里记下来，免得下一个人重新算一遍：

- **约 124 处机械改动点**：`coredao` 生产代码 65 条 `select` + `coremem` 约 59 处
  guard，跨 11 个文件，**零集中化杠杆**——那 8 个共享常量全是列清单，一个都不带
  `WHERE`，`queryDocs` / `getDoc` 是扫描器而不是查询构建器。
- **2 条 `ON CONFLICT` 会直接 42P10 硬报错**（`workspace_members` / `feedback_reports`）。
- **3 条 CTE 认领 SQL 要把谓词写两遍**（`ClaimDueSchedules` / `ClaimRunWatch` /
  `ClaimNextPendingMessage`）。漏了的话软删除的定时任务**照样到点触发并投递**。
- **两个「删了再建同名」会变成看着合理的 409**，不是 23505：`workspaces.key` 撞在 Go
  的 `KeyTaken` 预检查上，技能撞在 `SkillNameTaken` 上（`skills_name_idx`
  **刻意不是** UNIQUE）。一个正确的报错形状比一个错误的更容易查——这个更坏。
- **全仓有 8 处注释把「硬删除、没有 deleted_at」当成设计不变量在依赖**，
  `Run.AgentDeleted` / `Schedule.AgentDeleted` 那套**派生标记**正是因此而存在
  （见 `domain.go` 与 `schedule.go` 上那两段）。引入软删除等于推翻它们的前提。

而收益侧是空的：**这个仓库没有任何回收站 / 恢复 / 「已删除」入口**，也没有人需要看
已删除的东西。所以结论是**先把前三个补齐，软删除单独立项**——真要做的时候，判据不是
「哪个更规范」，而是先有一个明确的「谁要看已删除的什么」。

## 用户档案（头像与名字）：一条批量接口，不挂在实体上

`POST /api/user-profiles` 收一批邮箱、回一张查找表（`domain.UserProfile`）。

**为什么不把 name/avatar 挂到 `domain.Agent` 上**：`agents` 是 `doc jsonb`
全量落库，挂上去它们就会跟着写进库，于是「建 agent 那一刻的名字」被**永久冻结**
——那个人改名换头像之后列表上还是旧的，且没有任何报错。
`WorkspaceMember` 自带那两个字段是被上游通讯录逼的（只有 `?search=`、
没有按邮箱批量查的入口），**users 表没有那个限制**，别照抄那份陈旧。

三个实现细节：

- **`where email = any($1)` 一条 SQL**，不是循环 `GetUser`——调用方是「给一列人
  渲染头像」，逐个查就是 N+1；
- **查不到的邮箱不进 map、不算错**：「这个人没登录过」是常态（被邀请没登录、
  固定 API Key 的机器身份、离职同事），当失败处理会让整个列表页打不开；
- **查失败一律降级成空 map**（`ResolveUserProfiles`）：头像是纯装饰信息，
  不该把「头像服务抖一下」升级成「agent 列表打不开」。

⚠️ `core` 不 import `authdao`（那会把六个资源域与认证存储绑死）。走 `core` 侧
声明的 `UserProfileLookup` 接口 + `cmd/amad` 里一层适配，同 `DepartmentLookup`
那条「在消费方声明接口」的先例。

## Agent 状态：只有 active / disabled，且「停用」是真的会拦住

曾经有三个值（`draft` / `active` / `disabled`），其中两个是装饰性的：`draft` 在整个
生产代码里**没有任何一处读它做判断**，`disabled` 除了定义之外零引用。用户在下拉里
选「已停用」，界面给出确认，然后绑着的飞书 bot 继续回话、定时任务继续到点跑。

**一个会骗人的状态比没有这个状态差**，所以 `draft` 删掉、`disabled` 补上真实拦截。
`draft` 不该「实现」——它想表达的「还没配好」在本仓库里是一份可诊断的报告而不是
用户手拨的开关（见 `docs/DESIGN.md` 里 FeishuReadiness 那段「装好不等于能用」）。

### 闸门在 `AppendRunEvents`，不在 `createConversation`

这条最容易做错。**直觉上建会话是共同漏斗，实际不是**：

```
飞书群第一句话  → resolveBoundAgent → CreateFeishuConversation → AppendRunEvents
飞书群第二句起  → resolveBoundAgent →      （复用 runID）      → AppendRunEvents
                                            ↑ 压根不建会话
```

`feishu/inbound.go` 的 `ensureScopedRun` 命中 `session_scopes` 就直接
`return bound.RunID`，而群聊绝大多数消息都走这条。**只在建会话处拦，等于只挡住每个群
的第一句话，第二句起照常回答**——而那个漏洞在「停用之后我发了一句，它没回」这种手工
验证里恰好看不出来（第一句真的被挡了）。

四条入口（对话页 / 飞书新会话 / **飞书续聊** / 定时任务）全部汇聚在
`AppendRunEvents`，且它为「agent 被删」已经查过一次 `GetAgent`，Agent 对象就在手里。
`createConversation` 那道是补充：少了它会建出一条永远发不出话的空会话，方舟侧的
session 与容器白占资源，运行历史里还多一条什么都没发生的记录。

### 空串必须等价于「启用」

判据是 `status == "disabled"`，**不是 `!= "active"`**。这不是风格问题：

- 线上存量 agent 的 doc 里压根没有 `status` 键，反序列化出来是空串。写成
  `!= "active"` 会让**全部存量 agent 一夜之间不回话**，而报错说的是「已停用」——
  用户根本没停过，从现象查不到原因；
- 单测里多处 `store.CreateAgent(ctx, domain.Agent{ID: "a1"})` 不设 Status，同样是空串；
- 存量的 `draft`（已删掉的枚举值）也落到这里：放行，行为与删枚举之前**逐字一致**。
  **所以删掉 draft 不需要数据迁移就已经正确**——迁移在多实例滚动发布时的时序不可控，
  能不依赖它就别依赖。

判据收在 `domain.AgentStatus.IsDisabled()` 一处，别在别的地方另写一份。
未知值一律放行，同配置里未知字段不能当致命错误那条纪律：**拦错是全站故障，漏拦只是
维持现状**。

### 中断（`user.interrupt`）必须放行

方舟侧 session 可能正在跑。一律挡掉的话，停用一个 agent 之后就**再也停不下那一轮**
——方舟继续烧 token，容器要等空闲暂停甚至 TTL 才收。而前端 streaming 时把「发送」
换成「中断」且那个按钮不禁用，挡掉它等于摆一个必然报错的按钮。

停用比删除更容易踩这个坑：删 agent 是个显式的破坏性动作，停用看着很轻（「我就先停
一下」），然后正在跑的那轮就失控了。

### 飞书要在跑之前拦，并且回一句人话 + 打日志

`core` 挡得住，但那是一个 error——`feishu` 那侧只会记一行日志然后 return，
**群里的人什么都收不到**，表现成「机器人不理我了」。所以 `dispatch` 在
`resolveBoundAgent` 之后就问 `AgentDisabled`，走已有的 `replyStatic` 回一句提示。

**回复与日志都要有**：回复给群里的人（否则用户以为机器人坏了），日志给运维（否则
「用户报机器人不回话」时查不到是有人停用了它）。测试里这两条分别断言，且做过
「只删日志一行」的变异——那时只有日志相关的断言变红，证明它不是被回复那条顺带盖住的。

### 派生字段有**两条** enrich 路径，都要标

`Run.AgentDisabled` 与 `AgentDeleted` 同源、同一次查库（`markAgentDeleted`），但
`GetRun` 与 `ListRuns` 是**两条独立的代码路径**。只改一条的表现是「列表里看不出冻结、
点进去才看出来」，而对话页左栏读的正是列表。`ListRuns` 那边为了避 N+1 存的是
`map[id]status` 而不是 id 集合。

两个字段并列而不是合成一个 `frozen`：要用户做的事不同（一个去改绑或新建，一个把状态
改回「已启用」）。前端同理要两个标记与两套文案——早前 `ChatPage` 那个标记按 `frozen`
渲染，停用被显示成「已删除」，用户会跑去重新建一个 agent，而原来那个的技能与飞书
绑定都还在。

### 顺带补的：`validateAgentDraft` 此前不校验 status

任意字符串都能存进库，而 `IsDisabled` 只认 `"disabled"`——一个拼错的 `"disable"` 会被
静默当成启用（用户以为停了，它照跑）。现在只接受两个合法值与空串（空串是「没传」）。

## 连发多条消息：三条路径，别混为一谈（真机验证记录）

「上一句还在跑，我又发了一句」看着是一件事，实际是三件，各有各的正确反应。
下面每条都在**真方舟**上验过（2026-08-31，agent 连着调 bash 的压测）。

| | 是什么 | 要等多久 | 落在哪 |
| --- | --- | --- | --- |
| (A) 撞工具执行窗口 | 几十毫秒**竞态**，与用户意图无关 | 48ms 量级 | 服务端 `sendEventsThroughWindow` |
| (B) 用户主动排队 | 明知要等，只是不想憋着 | 几十秒到几分钟 | 前端 `useSendQueue` |
| (C) 转向 | 不等了，打断前一轮 | 不等 | 服务端 `RedirectRun` |

**(A) 为什么必须在服务端**：撞窗口的不止浏览器——飞书入站与定时任务也走
`AppendRunEvents`，而那两条路径此前**完全没有重试**，撞上就是硬失败
（症状：机器人对这句话不回话 / 这次定时任务白跑一轮，都零报错）。

**(A) 有两个 code，不是一个。** 真机压测抓出来的：

	RequiresActionRejected  HTTP 400  工具执行窗口
	RuntimeBusy             HTTP 409  会话正忙于另一条独占命令（原文写着 retry later）

第二个当时还落进 handler 的 default → 500「服务内部错误」。判据一律用
`ma.IsTransientSendRejection`（两个一起判），**别改回逐个问**——漏一个的表现是
用户偶发看到 500，而那个 500 不指向任何真实故障。

真机对照（同一套压测手法，密集打点 60~70 次）：

	修复前   200×59  409×4   **500×1**
	修复后   200×75  409×19   500×0

同一轮日志里「撞上工具执行窗口，重试后发送成功」出现 3 次（各重试 16 / 12 次）
——那 3 条是用户**没有看到**的 409。预算耗尽的 19 次如实回 409 交给前端排队。

**(C) 为什么是服务端原语而不是前端连打两个接口**：interrupt 与 message 之间的
网络往返正好又是 (A) 那个窗口，会以一定概率变成「只中断了、新消息没发出去」，
且零报错。真机验证（`sleep 25` 跑到一半时转向）：

	事件序列  agent.tool_use → user.interrupt → agent.message
	tool_result 里**没有** LONGTASK_DONE   ← 长任务真的被杀掉了，不是等它跑完
	agent 最终回答「1 + 1 = 2」            ← 改答了新问题

⚠️ 判据要用「tool_result 里有没有那个标记」，不能用「日志里搜得到吗」——
prompt 回显与 `agent.tool_use` 的 input 里都带着它，光搜关键词会得出相反结论。

## 「打开运行详情页」为什么慢，以及补拉的两道闸门

线上实测最慢的两条接口都在这条路径上（从日志捞 `cost=`、按 `__logid` 去重后
44 条样本）：

| path | n | P50 | P95 |
| --- | --- | --- | --- |
| `/api/runs/{runId}` | 3 | 320ms | **1398ms** |
| `/api/runs/{runId}/events` | 4 | 412ms | **1157ms** |
| `/api/workspaces/{ws}/runs` | 2 | 339ms | 339ms |
| 静态文件（favicon / `/`） | 14 | 0~3ms | 8ms |

样本量小，所以 P95 只是「见过这么慢」而不是稳定分位数。但两件事是确定的：
**静态文件 0~3ms 证明这是真的服务端处理**（不是网络或 proxy），而这两条恰好是
唯一会在请求内**打外部网络**的路径。

**根因不是 SQL，是请求内同步打方舟。** `GetRun` / `ListRunEvents` 在
`isSyncableRunStatus`（running / rescheduling / requires_action）时补拉尾部事件，
而 `SyncRunEvents` 第 4 步 `runner.ListEvents` 是一次跨网络调用。也就是
**正在跑的会话每次打开详情页都付这个代价**——而那恰恰是用户最常打开的那些。

⚠️ **「没显式设超时」不等于「没超时」，而是 30 秒**（`LiveArkRunner` 的
`arkCallTimeout`；更早的形状是 SDK 自带的 10 分钟 `defaultTimeout`）。对「点开一个
页面」来说 30 秒和 10 分钟没有区别，两者都是「这页打不开」。

所以 HTTP 路径上的补拉一律走 `backfillFromArk`（`core/ark.go`），它有两道闸门：

- **`backfillTimeout = 3s`**——除方舟那步外全是本地 PG（582 事件读回 1.77ms、
  3000 事件 8.1ms），健康的一次补拉是几十毫秒级，3 秒是它的几十倍，正常请求碰不到。
  **不压到 1 秒**：超时等于这次补拉白付了延迟，压太狠会让「方舟稍慢」也被切掉，
  于是详情页老显示「运行中」而实际已结束——那是把性能问题换成了正确性问题。
- **`backfillCooldown = 1s`**——详情页一次打开会**串行发两条请求**（先 `getRun`、
  拿到之后再 `listRunEvents`），走同一条判据，于是一次打开付两趟跨网络往返，
  而第二趟拿回同一批事件、逐条命中去重，纯属白跑。冷却掐掉第二趟，
  **读的仍然是库、数据一条不少**。取 1 秒是因为要盖的只是那一个本地 RTT，
  同时真人手动刷新（> 1 秒）不会被挡——挡了就成了「刷了也不动」。
  ⚠️ **只在补拉成功后才记时间**：失败也记的话，方舟抖一下就会让接下来一整个
  窗口内的补拉全被跳过，那道本该自愈的通道被这个「优化」堵死，且零报错。

⚠️ **别在 HTTP 路径上直接调 `SyncRunEvents`**。它自己刻意不设上限——飞书那两条恢复
路径（`feishu/resume.go`、`feishu/inbound.go`）要的是「尽量补齐」而不是「尽快返回」，
把上限写死在它里面会连坐改掉那边的语义。

### 为什么是短超时兜底，不是改成后台异步

异步（请求立刻返回库里的数据、补拉扔后台）看着更彻底，但在当前前端是
**用户可见的退化，且没有自愈路径**：运行详情页**既没有 SSE、也没有轮询**
（`RunDetailPage` 用 `useAsyncData` 一次性拉取，只在 `runId` 变化时重跑；全站唯一的
run 事件流在 `ChatPage`，而我们的 SSE 端点是纯代理、**不回放库**）。异步之后首屏
会拿到还没补拉的旧值：状态 chip 显示「运行中」并挂着呼吸灯、结束时间显示「进行中」、
耗时显示「—」、对话视图最后一轮的 `done` 停在 false 整段摊开不折叠。

而这些**纠正不了**——没有流、没有轮询、没有 focus refetch，只能等用户自己手动刷新。
等于把「页面慢 1 秒」换成了「页面显示错的状态直到用户重新点一次」，后者更坏且静默。
真要走异步，得先给详情页一条推送或轮询通道，那是另一件事。

### 慢在哪一步现在日志里直接有

`SyncRunEvents` 把五步拆开计时（`ark_took` / `key_took` / `local_took` /
`persist_took` / `total`），慢于 `slowBackfillThreshold`（500ms）升 Warn，
同 `shared/infra/shelltools/timing.go` 的取向。以前这件事只能靠读代码推——日志里只有一个
总的 `cost=`，看不出那 1.4 秒是方舟慢、取凭证慢、还是逐条落库慢。

⚠️ **打 Info 不打 Debug**：线上日志级别看不到 Debug，打成 Debug 等于没加。

⚠️ **超时不回滚已经落库的事件。** `persistArkEvent` 用 `context.WithoutCancel` 另起
5 秒预算，所以补拉被切掉时，已经拉回来的那批照常入库——它们是这次往返唯一的产出，
丢了就等于白付了那笔延迟。补拉是幂等的（`AppendRunEvent` 靠
`run_events_event_id_idx` 去重），所以「拉到一半」是安全的中间态，下次接着补。

⚠️ 同理 **`GetRun` 里的重读必须无条件做**，不能挂在 `err == nil` 上：补拉失败属于
「拉到一半」，库可能已经被改过（我们自己落的库，或线上别的 pod 落的），
只在成功时重读的表现是「第一次打开状态不对，刷新一下才对」。
`TestGetRunRereadsAfterFailedBackfill` 盯着这条。

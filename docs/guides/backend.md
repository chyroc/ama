# 后端 Go

**读它的时机**：改 `server/` 下任何 Go 代码之前。碰 `server/shared/infra/` 的外部接入层
还要另读 [infra.md](infra.md)。

## 现状

两个业务模块已落地并装配进 `cmd/amad`：

- **`feishu/`** —— 接入飞书应用，走 OAuth 2.0 设备码自助注册。
- **`core/`** —— 六个资源域：空间 / 成员 / Agent / 技能 / 方舟凭证 / 运行历史。

`httpd/` 是 HTTP 骨架（路由、CORS、panic 兜底、认证与授权的装配），`cmd/amad/` 是唯一的
main 包。配置由 `shared/infra/config` 统一加载，数据落 PostgreSQL。

**身份是真的了**：登录是本地账号（邮箱 + 密码）与通用 OIDC 两种（`auth` + `shared/infra/oidc`，
由配置 `auth.local` / `auth.oidc` 决定开不开），
**小写邮箱就是用户唯一 ID（Actor）**。除 `/api/auth/` 那七条与 `/healthz` 之外，
所有接口都要求已登录，未登录一律 401 + `code=unauthenticated`。

**授权也是真的了**：`authz` 包按**空间成员角色**判权限（owner/admin = 全部，
member = 读写，viewer = 只读，非成员一律 403），空间列表按成员过滤。
`AllowAllAuthorizer` 已经删了。**新增路由必须在 `authz/matrix.go` 里声明权限，
否则启动 panic**——动手前先读 [server/authz/README.md](../../server/authz/README.md)。

四条动手前要知道的：

- **身份走 context，不走请求头。** `auth.ActorFrom(ctx)` 取邮箱，
  `auth.IdentityFrom(ctx)` 取完整身份。原先的 `X-AMA-Actor` 头**已经删了**——
  请求头是调用方能控制的东西，漏覆盖一处就能冒充别人；context 只有服务端写得进去。
  `feishu.ActorFrom(r)` 签名没变，内部改成读 context，所以它那 11 个调用点一行没动。
- **鉴权中间件按路由包，不能兜在整个 mux 外面。** `httpd` 用一个 `protectedMux`
  装饰器把每个 handler 单独套上 `auth.Protect`。兜在外面的话方法不对的请求会先被
  401 挡下而不是回 405——`TestMethodRouting` 当场就挂，且报错不指向真实原因。
- **凭据在 HttpOnly Cookie 里**，随机 token，库里只存 sha256。选它不选 JWT 是为了
  **能真正吊销**（登出就是删那行会话），也省掉给多实例分发/轮换签名密钥。
  SSE 因此白拿鉴权：`EventSource` 不能带自定义头，但同源时自动带 Cookie。
- **固定 API Key 是给「不是人」的调用方**（e2e / CI / 脚本 / 探针）：
  `AMA_API_KEY` + `AMA_API_KEY_EMAIL` 两个都配齐才启用，线上默认不配即不存在。
  **本地开发的前端仍然走真实登录**（本地账号或 OIDC），旁路只给机器。


## 加了 / 改了接口，必须重新生成 OpenAPI

```bash
cd server && go run ./cmd/gen-openapi     # 产物 server/openapi/openapi.json，入库
```

`GET /api/openapi.json` 与 `GET /llms.txt` 是给**外部 agent** 读的自描述接口，
两条都**免鉴权**（agent 得先读懂平台才可能去认证）。

spec 的路由部分**从 `authz/matrix.go` 生成，一条也不手写**——手写必然漂移，
而 agent 拿着过时的 spec 会反复调错接口、且报错不指向真实原因。

**忘了重新生成会怎样**：`go test ./openapi/` 直接红，报错里带修复命令。整条闭环是

```
新增路由 → 必须写 matrix（漏了服务启动 panic）
        → spec 自动跟着变（从 matrix 生成）
        → 对账保证 spec 已重新生成并提交（TestSpecFileIsUpToDate）
```

**不依赖任何人记得。** 两个坑记在 `server/openapi/README.md`：
断言 spec 内容的测试必须读**磁盘那份**（读现场生成的那份会让「手塞一条虚构路由」
测不出来），以及 `/llms.txt` 不在 `/api/` 下面所以 `isBackendPath` 要单独放行它
（漏了会被 SPA 吃成 404，而 handler 明明注册了）。

**动手前读 [server/openapi/README.md](../../server/openapi/README.md)。**

## 两类 API Key，别混

```
config.APIKey（AMA_API_KEY）  进程级单例，环境变量配，给 e2e / CI / 探针。不入库。
auth.UserAPIKey（api_keys 表） 用户自助创建的，多把，可吊销、记最后使用时间。
```

两条通道并存是刻意的：前者是运维配的旁路（改配置才能动），后者是产品功能。
合并的话「轮换 CI 的 key」就要走一遍产品界面，而 CI 恰恰是在没有人的时候跑的。

用户级 key 的三条硬约束：

- **只存 sha256 + prefix**，明文只在创建那一次的响应里出现。与会话 token 同一个做法，
  所以「数据库泄露也换不出一把能用的 key」。用户丢了只能吊销重建——能再看一遍的凭据
  等于存了明文。
- **key 绑「人」，不绑空间。** 权限已经由 `authz` 按「这个人在这个空间的角色」判定，
  key 只是换一种方式证明「我是谁」。做成空间级就有两套并行的权限模型
  （key 作用域 vs 成员角色），迟早对不上，而不一致的表现是「某个接口有时 403 有时 200」。
  让它继承创建者的角色，那几十条路由声明就自动复用了。
- **界面在空间设置里，数据模型是账号级**——这两件事不矛盾。路径里那个 `{workspaceId}`
  只让 Guard 能判「你至少是这个空间的成员」，**不参与过滤**。做成全局
  `/api/api-keys` 的话 pattern 里没有空间，Guard 只能声明 `ActionNone`，
  于是任何登录用户都能打——校验就下移到「handler 记得取对当事人」这一个约定上了。

## 飞书模块

`server/feishu/` 走飞书的 **OAuth 2.0 device flow 自助注册**：用户不手填
AppID/AppSecret，点链接授权即可。accounts 域的注册接口**不需要任何 token**，
这是能在「手上还没有任何飞书应用」时自举的关键。

四条不能想当然改的约束（先鉴权再查 session、session 绑发起人、feishu/lark 双域纠正、
注册幂等）与踩过的坑都写在 `server/feishu/README.md`，**动手前先读**。
已起 `cmd/amad` 对真飞书跑通发起与轮询。

## 契约的真源是前端

`core` 实现的是 `web/src/data/api.ts` 里 `ApiClient` 接口的服务端一侧。那个文件里的
`LocalApiClient`（localStorage 版）是**行为基准**——它的每条副作用、每句报错文案、
每个字段的有无，都是页面真实依赖的东西。照抄是为了让前端换成 `HttpApiClient` 时
**零改动**。改任何行为前先去读 `api.ts` 里对应的那几行。

`server/core/README.md` 里逐条写了那些「看起来奇怪但必须照抄」的行为，都是**不会编译
报错、只在页面上表现成难查的怪问题**的类型：

- 时间戳必须固定三位小数（`.000Z`），因为 `listRuns` 按字符串排序
- nil 切片编码成 `null` 但契约要求 `[]`，切片字段一律不加 `omitempty`
- agent 响应**字节敏感**（前端 `isDirty` 是裸 `JSON.stringify` 比较）
- 事件响应要手工拼原始字节，绕开 `encoding/json` 的 HTML 转义
- 「404 → null」要认 `code` 不是状态码
- 列表接口永不 404，方舟凭证 key 无效也不是 HTTP 错误

**动手改 `core` 前先读那份 README。**

种子文件 `core/seed.json` **不要手改**——它由 `scripts/export-seed/export.sh` 生成，
改了下次导出就被覆盖。要调整种子内容请改 `web/src/data/seed.ts` 再跑导出脚本。

## 路由：Go 1.22 的 ServeMux 拒绝歧义路由

**冲突会在启动时 panic，比 Hertz 严格。** `/api/workspaces/by-key/{key}` 与
`/api/workspaces/{workspaceId}/agents` 会让它直接挂：两者都能匹配
`/api/workspaces/by-key/agents`，而「谁也不比谁更具体」。

所以按 key 查空间走 `GET /api/workspace-by-key?key=`、查占用走
`/api/workspace-key-taken?key=`，**用 query 参数从根上消掉这一类冲突**。

早前拿 Hertz 探过同样的形状、可以共存，于是误以为标准库也行——**两个路由器的冲突
策略不同，别拿一个的结论套另一个。**

## 派生字段放 Service，不放 Store

「关联 agent 是否已被删除」（`Run.AgentDeleted`）这类**跨聚合、算得出来所以不该存**的
字段，一律在 Service 读的时候现算，Store 只管存取。`core/ark.go` 的 `ListRuns` /
`GetRun` 是范例，另一个先例是 `feishu.App.BoundAgentIDs`。

这个缝之所以干净：读 Run 只有两个 handler 经过 Service，而**所有内部调用者都直接走
`s.store.GetRun`**，所以 Service 层加工不会污染业务逻辑，也不会顺着 `SaveRun` 落库。
列表路径记得先建一次 id 集合再逐条判，别逐条查（N+1）。

两条纪律：**判存在性只认 `ErrNotFound`，其它错误 fail-open**（库抖动不该表现成
「你的 agent 被删了」——那是一句用户无法处理的假话，还会连带冻结整个空间的对话）；
**「资源在、但这个操作此刻不允许」用 `ErrPrecondition`（422），不要用 404**——
前端把 `not_found` 转成 `null`，会让页面变成静默的空状态。

`feishu.App.BoundAgentIDs` 是这条纪律的另一个实例，而且它**曾经忘了现算**：
`feishu.Service` 原本没有 core 的依赖，于是那个字段在所有响应里恒为 `[]`。
症状不是报错，是删除确认框永远显示「有 0 个 Agent 绑定该应用」，而列表展示那处
自己绕过它、用 `agents` 反算了一遍——**一个字段两套算法、其中一套恒错**。
现在由 `Config.Unbinder` 在读路径上填（`feishu/apps.go` 的 `fillBoundAgents`），
`TestBoundAgentsNeverPersisted` 钉住「填了不能落库」这一半。

## 飞书应用与 agent 是 1:1 独占

**一个飞书应用最多只能绑一个 agent。** 技能不是这样（一个技能可被多个 agent 复用），
所以这条约束是飞书独有的，理由只有一条：**一条入站消息只能有一个回答者**。

绑成 1:N 时 `inbound.go` 只能在几个 agent 里挑一个（取 `AgentsBoundTo` 的第一个，
即按 `ord` 排最前的那个），于是「谁来回答」由行序决定——用户在两个 agent 的详情页
上都看到那个应用，却只有一个真的会回话，且**哪个会回话没有任何界面能告诉他**。

写入侧由 `core/agent.go` 的 `ensureFeishuAppsFree` 拦截（`ErrConflict` → 409，
文案带上占用者的名字）。**校验时必须排除自己**，否则「只改名字、绑定原样回传」这个
最常见的保存动作会被自己已有的绑定挡住——前端每次保存都是全量回传 draft，必经此路。

**数据库层挡不住这件事，这是个已知的缺口。** 按本仓库的规矩该「让唯一约束裁决」，
但 PostgreSQL 两条路都走不通：不能对集合返回
函数建索引，`text[]` 也没有默认的 GiST 操作符类。真要数据库裁决就得把关系抽成
「一行一绑定」的独立表，代价是同一份关系有两个存储位置。所以现状是**写时校验 +
读时容错**：`inbound.go` 的 `resolveBoundAgent` 遇到多绑仍取第一个，
但打一条 ERROR 把应用和所有候选 agent 点名报出来。

## 聚合放 Go，不写进 SQL

`core/usage.go` 的用量聚合是**取回来在 Go 里算**，不是 `sum()` / `percentile_cont`。
不是偷懒，是三件事叠起来：

- `usage` / `trigger` / `durationMs` 都只在 `runs.doc` 这个 jsonb 里，**没有标量列也
  没有索引**，`doc->>'…'` 强转既用不上索引，又要在「SQL 的 NULL、`sum()` 空集回
  NULL」与「Go 的零值」之间逐个对齐语义；
- 分位数在 PG 里是 `percentile_cont`（线性插值），与 MemStore 的 nearest-rank
  **不是同一个定义**，两个实现会给出不同的数；
- Store 契约用例对两个实现各跑一遍，口径分叉当场就挂。

所以 Store 只加了一个 `ListRunsForUsage`（按空间 + 时间窗取），聚合在 Service。
时间窗用**定长 ISO 串做字符串比较**（`started_at >= $2 and started_at < $3`）——
这样才吃得上 `runs_workspace_started_idx`；`started_at::timestamptz` 会让它失效。
按天分桶也在 Go 里切字符串前 10 位，不用 `date_trunc`。

窗口一律**左闭右开** `[since, until)`：闭区间会让某天最后一毫秒的 run 同时落进
相邻两个桶，于是「每天的数加起来」比总数多，而两个数都长得合理，从现象查不出来。

## 日志：什么该记、什么绝不记、级别怎么定

**判据只有一条：这条日志能让下一次排查少走一步吗。** 不能就别加——
日志刷屏本身是故障（有过一小时刷 5000 行的 429），而噪音会把真事故淹掉。

### 故障分支与常态分支必须分开

这是本仓库反复踩的那个形状，已经付出过四次代价：

```go
if err != nil || len(bindings) == 0 { continue }   // ❌ 库抖一下 = 整个空间跳过，零日志
if err != nil { return }                           // ❌ 「查不动」与「本来就没有」共用一支
```

后果的共同点是**故障伪装成常态、全程零日志**，而且症状与真因方向相反：

| 位置 | 静默 return 的后果 | 排查时看到的现象 |
| --- | --- | --- |
| `core/worker.go` 绑定查询（已修） | 那个空间的 worker 不启动 | 每次工具调用卡在 `requires_action` |
| `core/scheduler.go` `rescheduleOnly`（已修） | `next_run_at` 恒空，任务**再也不跑** | 界面上 status 还是 active、失败计数还是 0 |
| `core/chat.go` `applyEventToRun`（已修） | run 的状态/事件数/用量全不更新 | 「对话在跑，但历史里像是没动过」 |
| `feishu/progress_driver.go`（已修） | 思维链永远停在「思考中」 | 日志里**一条都查不到** |

写法：**能 `errors.Is(err, ErrNotFound)` 就分开**，常态那支静默、故障那支 Warn
并写清**后果**（不是只写「失败了」）。`core/scheduler.go` 的 `finish` 是范例。

⚠️ **比「少一条日志」更坏的是「日志说谎」**：`rescheduleOnly` 的上游打的是
「跳过本次触发，已排回去」，而它可能一步都没排回去。看日志的人会据此排除掉这条路径。

### 每条防线/降级/兜底都要有专属日志

**判据是「搜它的专属日志，0 条就是从没生效过」**——今天就靠这招确认了 worker 的
限流闸门从未触发。没有专属日志的防线**无法被验证**，等于薛定谔的防线。

反例（本轮修掉的）：`transport.ensure` 里「开场事件失败，下一次 apply 会带完整差异
重来」——那句兜底注释写了很久，但它到底有没有生效过，谁也答不上来。

### 级别

| 级别 | 用在哪 | 频率注意 |
| --- | --- | --- |
| **Error** | **全部 5xx**（`httpd` 的 `logServerError` 统一兜底）、panic | 异常路径，正常量为零，不必限频 |
| **Warn** | 可恢复的失败、降级、退避重试、防线触发 | 只在失败时打；正常流量为零才行 |
| **Info** | 状态变化、启动装配、认领到活 | ⚠️ 高频路径要限频或**只在状态变化时打** |
| **Debug** | 线上看不见（生产 logger 是 Info） | 别把关键判据放这儿 |

⚠️ **4xx 不打 Error**：403/404 是日常流量（权限边界、前端探测、扫描器），
打了会让「搜 Error」这个动作失去意义。

⚠️ **别用「只判对象非 nil」当装配日志**——那类判据恒为真（依赖注入里那个对象总是
非 nil），于是日志说「已装配」而功能其实不可用。踩过一次：同一个实例在启动时
先打「未配置」再打「已装配」，**前者才是对的**。问「这个能力自己报告可用吗」
（`XxxEnabled()`），别问「指针在不在」。

### 记 id 与枚举，不记正文

| 记 | 不记 |
| --- | --- |
| run / workspace / chat / work id、错误码、状态枚举、耗时、重试次数、走了哪一支 | 用户消息正文、CoT 思考文本、工具入参与输出、接口响应体、请求头、原始 UA |
| **logid**（服务端自己生成的排查信息，**不该被脱敏**） | **URL 的 query**（历史上身份真的进过 `?actor=`） |

- 人名/邮箱用 `secret.Mask`；用户数据要留痕就用 `secret.Fingerprint`（长度 + 短哈希，
  够判断「是不是同一条」，不足以反推原文）。
- **query 要靠结构排除，不靠「记得去掉」**：用 `r.URL.Path` 而不是 `r.URL.String()`。
- 要从响应体里取东西就**白名单式地取**（解进一个只有目标字段的 struct），
  别整段记下来——`httpd` 的 `parseErrorCode` 是范例，它只挑 `code` 那一个键。

脱敏还有一层兜底在 `shared/gopkg/xlog`（`cmd/amad` 启动时 `xlog.SetRedactor(secret.Redact)`，
每条日志的 message 与字符串值都过一遍，按形状抹 JWT /
`xxxToken=` / `Bearer xxx`），但**那不替代调用点的责任**：它认不出编码过的、
拆开拼接的、以及形状不特殊的随机串。⚠️ **新注入的凭据要同时进脱敏名单**——
漏了会原样流出且零报错，而 fixture 里没有它、所以现有用例照样全绿。

### 安全断言必须做变异验证

「不泄漏 X」这类断言**很容易恒真**。本轮踩到一次真的：
「投递失败日志不含用户内容」用的场景是**首推即失败**，而那时快照还是 nil——
用户内容根本不在被测函数手边，把它打进日志的变异**照样全绿**。
改成「先成功推一次、再失败」才真的验到。

变异要**能编译**才算数，还原时还原成**修复后**那份。

## 多实例：写之前先问「两个进程同时跑还对吗」

线上 amad 是 **N 个实例**，请求经负载均衡随机落地，滚动发布期间新旧实例并存
（详见 [CLAUDE.md 的「这是分布式服务」](../../CLAUDE.md)）。本地 `./dev.sh` 是单进程，
所以**进程内状态在本地永远是对的**——这类 bug 测不出来，只能靠写的时候不引入。

具体到本仓库的几处，动手前对齐一下：

- **状态一律落 PG**。`core.Store` / `feishu.Store` 是唯一合法的跨请求记忆。
  想加个 `map` 缓存前先确认：丢了只是变慢（可以），还是丢了功能就错（不行）。
- **要互斥就用 Redis 租约**（`shared/infra/redis`），不要 `sync.Mutex`。飞书长连接是范例：
  一个应用只能有一条连接，抢不到锁就**不建连**（fail-closed），宁可这个实例不收消息。
- **有远端副作用的创建要幂等**。方舟建 Environment / Agent 都是配额资源且名字唯一，
  两个实例同时「先查再建」会撞 409，认领同名资源还可能领回类型不对的旧环境。
  让唯一约束裁决，别在 Go 里判存在性。
- **SSE 的订阅者和事件源可能不在同一个进程**。浏览器连在 A，方舟事件或 worker 结果
  到了 B，A 上的用户就什么都看不到。加实时链路时先想清楚落库与送达两件事。
- **`time.Ticker` 起的巡检会在每个实例上各跑一遍**。要单例就先抢锁。

## 约定

标准 `gofmt`。注释与提交信息用中文（沿用现有 commit 风格）。

`shared/infra/ma/` 的既有约定见其 README「设计取舍」一节，扩展该包时请沿用：一个文件只放
一类 API；便捷入参配 `*Raw` 逃生口；返回值直接用 SDK 原生 struct，不做二次 DTO 映射。

## 定时任务与主动式能力

`core/schedule.go` + `scheduler.go`。一次触发就是一条 `trigger` 为 `schedule` 的 Run，
所以它自动进运行历史、能看完整事件时间线。

**动手前必读 `server/core/README.md` 的「定时任务」一节**，那里有三条只在真 PG
上暴露的认领坑（`in (子查询)` 挡不住并发双领、外层重写条件后 `limit` 失效、
psql 与 pgx 的执行计划可以不一样）。

两处与本文件既有约定相反的地方，都有理由：

- **`schedules.next_run_at` 是真的 timestamptz**，不是那套不透明 ISO 串。认领要让
  数据库当唯一时钟——多实例的机器时钟有偏差，「谁是现在」不能由应用决定。
- **时间作为参数传入**（`ComputeNextRun(spec, after)`、`Scheduler.Now`），这是 core 包里
  第一次这么做。这一域要断言的东西全是时间的函数，给个固定时刻就能测，
  不需要 sleep 也不需要 mock 库。

## 测试

`go test -race ./...`，**全部离线、无需凭证**（下表是各包测试函数数，会随代码漂移）：

| 包 | 数 | 包 | 数 |
| --- | --- | --- | --- |
| `feishu` | 185 | `shared/infra/lark` | 58 |
| `core` | 136 | `shared/infra/shelltools` | 97 |
| `auth` | 59 | `shared/infra/dockertools` | 22 |
| `shared/infra/config` | 30 | `shared/infra/pg` | 32 |
| `httpd` | 25 | `shared/infra/docker` | 10 |
| `authz` | 19 | `shared/infra/ma` | 28 |
| `model/apimodel` | 9 | `shared/infra/redis` | 9 |

`authz` 那 19 个覆盖角色矩阵、邮箱大小写、路由表双向完整性（漏声明 panic / 声明了
没注册）、以及「资源不存在回 404 不回 403」。真正会出错的地方在缝上（Guard 装反位置、
pattern 差一个字、反解用错参数名），那些在单元测试里都是绿的——所以 `httpd` 里另有
一层端到端：真 core + 真 Guard + 种子数据，以四种角色 + 一个非成员打真实路由。

飞书那 185 个里有一组专门盯 1:1 绑定：`core` 侧 4 条（抢占被拒、新建也拦、保存自己
的绑定不被自己挡、解绑后可重绑），`feishu` 侧 3 条入站寻址（正常只派一个、多绑取
第一个并打 ERROR、没绑时回引导语）+ 3 条派生字段与级联清理。

`shared/infra/pg` 那 32 个覆盖配置校验、DSN 拼装（含 IPv6 方括号、易错键拦截）、池参数、
错误判别和事务控制流。碰库的包默认连本地 `127.0.0.1:15432` 的 PG（`AMA_TEST_PG_*` 可改），连不上就整组跳过。

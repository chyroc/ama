# 问题反馈 —— 两级采集与「只给开发团队」的读取口

**读它的时机**：改反馈的采集范围之前，或者要给「只给开发团队」这类接口设计权限之前。

> ⚠️ **本文说的「反馈」是机器自动采集的报错流，不是用户手写的意见。**
> 后者是另一个实体（`suggestions`，产品建议），它**复用了这里的 `feedback.readers`
> 白名单**当「平台方」名单——所以改那份配置会同时影响两个功能。
> 见 [README-suggestion.md](README-suggestion.md)。

代码：`core/feedback.go`（Service + 采集边界）、`repository/schema/core_feedback_pg.sql`（表与限流）、
`core/handler.go` 的两个 handler、前端 `web/src/lib/feedback.ts` 与
`web/src/components/FeedbackButton.tsx`。

## 这个功能解决什么

用户遇到问题时，**点一下就把必要信息报上来，不需要输入任何文字**。

不要「请描述你遇到的问题」表单，那等于把判断问题的工作推回给用户——而他往往正是因为
不知道发生了什么才卡在那里。要采的信息全部自动收集。

挂载点是 `AsyncErrorBlock`（`web/src/components/Page.tsx`）：13 个页面的错误态都渲染它，
所以入口放在那里**一处改动全站生效**，不用逐页加。逐页加必然漏，而漏掉的那页零报错。

## 两级：区别在「采什么」，不只在「谁触发」

| | 一级 `auto` | 二级 `user` |
| --- | --- | --- |
| 谁触发 | 错误发生时**自动**，用户没点任何东西 | 用户点了「反馈这个问题」 |
| 依据 | 没有用户的同意 | **那一下就是他的同意** |
| 去重 | **是**（见下面「限流」） | 否——点两次就该有两条 |

一级是在用户不知情时发生的，所以它采的每一个字段都必须「就算全世界看见也无所谓」。
**把两级采同样的东西，等于用二级的授权去合法化一级的采集。**

### 确切的字段清单

| 字段 | 一级 | 二级 | 说明 |
| --- | --- | --- | --- |
| `routePattern` | ✅ | ✅ | **页面**路由模式，**id 已抹成 `{id}`** |
| `httpStatus` | ✅ | ✅ | HTTP 状态码，拿不到是 0 |
| `errorCode` | ✅ | ✅ | 后端错误码（判因价值高于状态码） |
| `errorKind` | ✅ | ✅ | 前端分类 `denied` / `other` |
| `payload.message` | ✅ | ✅ | 那句错误文案（后端给的中文句子，不是用户输入） |
| `payload.apiRoute` / `payload.apiMethod` | ✅ | ✅ | **出错的那条接口**（模式串 + 方法），见下面一节 |
| `userAgent` | ✅ | ✅ | 浏览器**概要**（`Chrome 120 / macOS`） |
| `payload.appVersion` | ✅ | ✅ | 前端版本（构建时注入的 git sha） |
| `workspaceId` | ✅ | ✅ | 空间 **id**，可空；不像 id 的值会被丢掉 |
| `payload.path` | ❌ | ✅ | **完整路径（含 id）** |
| `payload.agentId` | ❌ | ✅ | 当前 agent |
| `payload.recent` | ❌ | ✅ | 最近失败的接口调用（方法 + 路径模式 + 状态 + code + **logid**） |
| `payload.consoleErrors` | ❌ | ✅ | 最近几条控制台错误的**首行** |
| `logId`（顶层真列） | ✅ | ✅ | 服务端那次请求的 logid，见下面一整节 |

**两级都绝不采**：token、cookie、API Key、Authorization 头、接口响应体、用户消息正文、
原始 UA 串、URL 的 query 部分。

### `apiRoute` / `apiMethod`：一级此前看不出「是哪条接口挂了」

**`routePattern` 是页面路由，不是接口路由**，而这两者经常不同：`/w/{id}/chat`
一个页面会打十几条接口。二级还能靠 `recent` 反推，**一级不采 `recent`**——
于是一级记录里根本没有「哪条接口失败了」这个信息，而它恰恰是排查时第一个要问的。

补上之后 `group by api_route` 才有意义（「这个接口今天错了多少次」），
而那是自动化改进的第一个查询。

为什么两级都采（逐条对照上面那张「绝不采」的表）：

- **是模式串不是真实路径**——前端在 `data/http.ts` 的唯一错误出口调
  `routePatternForReport()` 抹 id，后端 `sanitizeAuto` **再抹一遍**
  （`stripRouteIDs(stripQuery(...))`，判据与 `route_pattern` 完全相同）。
  所以它定位不到具体的会话或 agent，一级那条「不采完整路径」的边界没有被绕过。
- **没有 query**——`routePatternForReport` 先 `stripQuery` 再 `stripIDs`，
  顺序写死在那个函数里，**调用方没有机会写错**。
- 不含用户输入、不含响应体、不是凭据。它就是我们自己那份接口清单里的一项，
  而 `GET /api/openapi.json` 本来就公开这份清单。
- 低基数，所以聚合有意义。

⚠️ **它不进 `dedup_key`**。不是因为危险（低基数，不会像 logid 那样让限流失效），
而是因为**没必要**：`route_pattern + error_code + http_status` 已经框住了「同一类问题」，
多一个维度只会把本该合并的行拆开。`TestFeedbackAPIRouteIsNotInDedupKey` 钉住这条
（刻意让 20 条的 `apiRoute` 各不相同，断言仍然合成 1 行、`occurrences` 是 20）。

⚠️ **e2e 覆盖不到这个字段，别把 16-feedback.spec.ts 全绿当成它被测过了。**
`LocalApiClient` 不走 `data/http.ts` 的 `request()`，mock 的错误由 `mockApiError`
直接 new 出来，那里没有「刚才在打哪条接口」这个信息——所以 mock 模式下
`apiRoute` 恒为空，面板上那一行**永远不渲染**，任何断言它的用例都会恒真。
（同本文最后一节那个坑：**同一个字段在测试环境与线上是两种形状**。）
钉住这条链的是后端那两条 Go 用例 + TS 类型检查；要在 e2e 里覆盖，
得先让 mock 也带上路由。

⚠️ **`network_error` 下它仍然有值，与 logid 相反**。这两个字段的可得性来源不同：
logid 是**服务端生成的**（请求没到达就不存在），而 method/route 是**我们自己发起时
就知道的**，与对面有没有回应无关。所以那个场景下它是唯一能说清「当时在打哪条接口」
的东西——别看到「`network_error` 没有 logid」就顺手把它也判成空。


几条值得单独说的：

- **原始 UA 串在前端就被概括掉**，不是发给后端再截。原始 UA 是准指纹（版本号 + 平台 +
  渲染引擎的组合能定位到很小的人群），而判因只需要「什么浏览器」。
- **用户消息正文两级都不采。** 对话内容是用户数据，而「反馈一个加载失败」不需要它。
  要它得有单独的、说得清的理由，并且让用户看见。
- **控制台错误只留首行。** 完整堆栈里有源码路径、变量名，偶尔还有内联的数据。
  首行（`TypeError: x is not a function`）已经足够判因。
- **`recent` 里没有 query。** 历史上前端真的把身份塞进过 `?actor=`（见 `data/http.ts`
  的注释），任何「把 URL 报上来」的功能都会把那种参数一起带走。

### 用户能看见将要上报什么

反馈按钮旁边有一个可展开的「查看将上报的信息」。**它渲染的是真正要发出去的那个对象**
（`describePayload(payload)`），不是一段描述它的文案——文案会与实现漂移，对象不会。

一键上报而用户不知道传了什么是不可接受的。i18n 里那句 `feedback.neverCollected`
（「绝不上报：登录凭据、Cookie、API Key、接口返回的内容、你在对话里说过的话」）
**是承诺不是装饰**：改采集范围时必须同步改它。写着「绝不上报」而实际上报了，
比不写这句话糟得多。

> 加 logid 那次**核对过这句话、结论是不用改**：它列的八项是「登录凭据 / Cookie /
> API Key / 接口返回的内容 / 你在对话里说过的话」，logid 一项都不沾——它是我们
> 服务端自己生成的随机串，不含用户输入、不含响应体、不是凭据。这个结论要留在这里，
> 因为上面那句「改采集范围必须同步改它」会让下一个人以为漏了一步。

## 服务端 logid：两级都采，但只有请求到达过服务端时才有

**两级都会带上服务端那次请求的 logid**（顶层真列 `log_id`，不在 payload 里）。
它来自响应头 `X-Request-Id`，由 `httpd` 的 `withLogID` 生成（id 本身见
`shared/gopkg/requestid`）；同一串 id 进了那个请求的**全部**日志行（`xlog.NewHandler`
从 ctx 取，填 `request_id` 字段）。所以拿着它能直接搜到服务端日志里
那一次请求的现场，不必按「大概几点 + 哪个接口」去猜——**并发下时间戳会撞**，
那种猜法本来就不可靠。

前端读它的地方只有一处：`web/src/data/http.ts` 的 `errorFrom`。**只能是那里**——
它是唯一同时手上有 `Response` 与要抛的错误的地方，再往后（`classifyError` /
`buildFeedbackPayload`）只剩一个 Error 对象，响应早就没了。

### ⚠️ `network_error` 下不存在服务端 logid，因为请求没到达、中间件没跑

**这条反直觉，而且下一个人很可能会以为是「漏读了响应头」，所以单独写一节。**

用户在 `/w/ama/usage` 看到「连不上服务端，请确认后端已启动」（`errorCode:
network_error`）时点反馈，采集清单里**不会有 logid，而且不该有**。原因不是前端忘了读：

```
fetch 只在网络层失败时 reject（DNS 失败、连接被拒、TLS 失败、CORS 预检失败）
        ↓
请求从未到达服务端
        ↓
withLogID 这个 HTTP 中间件压根没执行过
        ↓
没有 logid 被生成，服务端日志里不存在这次请求的任何一行
```

`http.ts` 里那个 `catch` 分支连 `res` 变量都还没被赋值（`let res: Response` 在上一行
才声明）——**不是「忘了读响应头」，是结构上没有响应对象可读**。`httpStatus: 0` 就是
同一件事在数据模型里的表达（见 `schema_feedback_pg.sql` 那一列的注释）。

**全部错误码里只有 `network_error` 是这样**：`access_denied` / `not_found` /
`route_not_found` / `invalid_argument` / `invalid_payload` / `conflict` /
`precondition_failed` / `internal` / `unauthenticated` / `app_not_found` / `unknown`
**全都有响应对象、全都有 logid 可读**——那才是它真正有价值的场景（403/404/409/500
这些需要跨端排查的）。

`unknown` 是个中间态，值得单独想清楚：那是网关吐 HTML 那种（`readBody` 解不出 JSON）。
请求**可能**到了网关但没到 amad，于是 logid 可能是网关生成的（`withLogID` 会复用上游
传来的头）、也可能压根没有。所以那条路径上「有就带、没有就空」，不假装成两端之一。

### 绝不编一个假 logid

空就让它空着。**不要**在前端兜一个 `"-"` / `"unknown"` / 随机生成的 id：

- 那是个**合法字符串**，会被当成真 logid 上报出去，排查的人以为串上了、
  实际搜不到任何东西（以前的容器客户端就犯过同一个错误：取不到时回落成 `"-"`）。
- **也不要改成「前端发请求前自己生成一个塞进 `X-Request-Id`」**。那个 id 在服务端日志里
  依然查不到（请求没到达），只是把「空值」变成「有值但查不到」，更糟；而且会让**所有
  正常请求**的日志关联键由不可信输入决定。

实现上这件事表现为：空值时那个键**整个不出现**（Go 侧 `omitempty` + `putIfNotEmpty`，
前端 `if (error.logId)`），而不是落一个空串。「这次没有服务端 logid」与「有一个空的
logid」在排查时是两件事。

### 那这个场景就没救了吗——「最近失败的接口调用」能救

`payload.recent` 里每一条也带自己的 logid，**这恰恰是 `network_error` 场景下唯一能
拿到 logid 的途径**：如果用户是先正常用了一会儿、后端才挂的，那前面几次**到达过**
服务端的失败各自带着真 logid，顺着它们能定位到后端挂掉的时刻。

同样地，那几次没到达的调用**不落空串键**——混一堆空串进去会让「哪几次真到过服务端」
看不出来，而那正是这份轨迹在这个场景下的全部价值。

⚠️ `sanitizeUser` 里 `recent` 是**逐字段重建**的（注释写明了理由：不逐字段重建的话
「将来给 FeedbackCall 加字段就会自动落库」）。给它加字段要同步改那里，这次就是。

### ⚠️ 三件必须一起做对的事

**一、logid 两级都采，它不需要用户同意。** 这一串是**我们服务端自己生成的**
（`httpd` 的 `withLogID`），不含用户的任何输入，也不是凭据；`shared/infra/secret` 刻意不抹
裸 hex 时还点名过「logid 是不该被脱敏的排查信息」。

而且**一级本来就知道这条反馈是谁报的**（`actor` 列存着员工邮箱），所以带上 logid
并没有多暴露一个人的身份。一级恰恰是最需要它的那一级：那些记录是在用户不知情时
留下的，没有人在旁边解释「我当时在干嘛」，只有一串 logid 能把它接回现场。

> 这里曾经判错过一次，判据值得留下：当时的理由是「它是准会话标识——顺着它搜日志能
> 拿到带 `path` 的日志行，等于绕过一级不采完整路径那条边界」。这个推理把它抬得太高
> 了：**一级已经采了 `actor`**，「这是谁」本来就知道；而 logid 本身不含用户的任何
> 输入。真正需要拦的是**用户数据与身份标识**，不是「能不能顺藤查到我们自己的日志」
> ——我们的日志本来就是我们自己的。

**二、logid 绝不能进 `dedup_key`。** logid 每请求唯一，进了键就每条都算新行，
**限流完全失效**——那正是这个功能 DoS 掉自己的方式（见上面那节）。它不报错、
界面上也完全正常（一条能存、一百条也能存），只有真实故障期间才暴露。

⚠️ **「一级也采」之后这条从假想变成了真实风险**：走去重的正是一级，而一级的每一行
现在都带着一个各不相同的 logid。所以 `TestFeedbackLogIDIsNotInDedupKey`
**走真实路径**（连发 20 条带不同 logid 的同类错误，断言仍然合成 1 行），
而不只是单测那个纯函数——前者能证明整条链上没有任何一处把它混进键里。

**三、CORS 要 expose 这个头。** 浏览器默认只让 JS 读 7 个 CORS-safelisted 响应头，
其余**静默返回 null**。同源下（默认 `API_BASE = "/api"`、生产 go:embed 同源）读得到，
所以现在不加也不出事；但一旦有人用 `VITE_API_BASE` 把前端指到别的 host——那正是
`cors` 中间件存在的场景——logid 就会在那种部署下静默消失，表现是
**「logid 有时有有时没有」**，而现象完全不指向 CORS。`httpd/server.go` 的 `cors`
已经补上 `Access-Control-Expose-Headers`，`TestCORS` 钉住它（**钉死字面量**：
拿常量比常量的话改错了两边仍然一致）。

### 冲突时不更新 log_id，这是对的

`store_pg.go` 的 `on conflict do update` **只动 `occurrences` 与 `last_seen_at`**，
`log_id` 不在其中。走到 `on conflict` 的只可能是一级（二级的 `dedup_key` 是 NULL，
永不冲突），而一级**是采 logid 的**——所以这个选择有实际后果：那一行留着的是
**第一次**那个现场的 logid，后面 299 次的都丢掉了。

这是对的，与 `payload` / `created_at` 保持首次同一个取向：第一次出现的现场比第 300 次
有价值（往往还带着触发它的上下文），而 `occurrences` 已经说清了「发生了几次」。
反过来「每次都覆盖成最新的」会让唯一那串能查的 logid 一直在变，且指向的是最没
信息量的那一次。`TestFeedbackLogIDIsNotInDedupKey` 顺带钉住了这个语义。

## ⚠️ 过滤是白名单式的，不是黑名单式的

**这是本功能最重要的一条。** 请求体不被原样存进 `payload`——它先被解成一个字段固定的
struct（`FeedbackInput`），然后由 `sanitizeAuto` / `sanitizeUser` **重新构造**出要落库的
那个 map。前端多发的任何字段在解码那一刻就已经不存在了。

黑名单（「删掉 token / cookie / authorization 这几个键」）一定会漏，两种漏法都不报错：

- 漏掉一个键名——`Authorization` / `X-Auth-Token` / `apiKey` / `secret` / `bearer` /
  `set-cookie` / `x-jwt-assertion`…穷举不完，而且前端加一个新字段就多一个；
- 值里**嵌着**凭据——一条 message 里贴了半个 JWT、一个 URL 带着 `?token=`。

白名单挡住第一种：**没在名单上的键根本不会被读取**。第二种由 `scrubText` 按形状抹一层
（JWT、`amak_` key、敏感 query 参数、`Bearer xxx`），但那**只是纵深防御**——
真正的保证来自「我们只存自己列举出来的东西」。

`scrubText` 刻意不复用 `shared/infra/shelltools/scrub.go`：那一层抹的是「我们自己注入的那把 token」
（值已知，精确替换），这一层面对的是「不知道具体值、只能按形状猜」。判据完全不同，
硬合成一个函数会让两边都变模糊。

## ⚠️ 一级必须限流，否则这个功能会 DoS 掉自己

一个坏接口能让**一个用户**在一分钟里刷出上百条错误（页面重挂、SSE 重连、轮询重试），
而线上是多实例、用户是几百个。不限流的话第一个真实故障就把表刷爆了，而那时
**读取接口也一起失效**——几十万条同一个错误里找不出别的问题。

做法：`dedup_key` 的**唯一索引** + `on conflict (dedup_key) do update set
occurrences = occurrences + 1, last_seen_at = excluded.last_seen_at`。

```
dedup_key = sha256(actor | route_pattern | error_code | http_status | 十分钟桶)
```

四个决定各有理由：

- **由数据库裁决，不在 Go 里先查再写。** 后者是 TOCTOU，多实例下两个进程都查到
  「这个桶还没有」然后一起 insert，慢的那个撞唯一约束报 500——而那**正好发生在故障期间**
  （并发最高、最需要这条路径可靠的时候）。
- **时间桶靠取整（`unix / 600`），不用滑动窗口。** 取整是无状态的：两个实例对同一时刻
  算出同一个桶，不需要商量。滑动窗口要先读「上一条是什么时候」再决定，那又回到了先查再写。
  代价是跨桶边界的一次爆发会产生 2 行——有界，且对「这个错误在发生」这个判断无影响。
- **`http_status` 也进键。** 同一条路由上的 403 与 500 是两个不同的问题；不放进键的话
  故障期间先出的那个会把后出的吃掉，而后出的可能才是真正的原因。
- **hash 而不是明文键。** 明文会把邮箱写进一个带唯一索引的列（`actor` 列已经有了），
  而它还会出现在唯一约束冲突的错误信息与慢查询日志里。

**冲突时只动两列**（`occurrences` 与 `last_seen_at`），其余一列都不改：保留首次那份是
刻意的，第一次出现的现场比第 300 次有价值（往往还带着触发它的上下文）。

**量级不能丢**：合并成一行但 `occurrences` 要说得出发生了几次。「发生了 312 次」与
「发生过」对判断优先级完全不是一回事，而按行数统计在去重之后必然低报。

### 二级刻意不去重

`dedup_key` 为空串 → Store 写成 **SQL NULL** → NULL 在唯一索引里互不相等 → 每条都进得去。
不需要为它写任何分支。

⚠️ **这里最容易写错、且错了完全不报错**：`dedup_key` 存空串（而不是 NULL）的话，
唯一索引会认为「第二条空串与第一条重复」，于是**所有用户主动提交的反馈只有第一条能存进去**，
后面的全被 `on conflict` 吃掉、变成给第一条 +1——用户看到「已收到，谢谢」而库里没有。
`nullText()` 就是为这件事存在的，`testFeedbackKeepsUserLevel` 钉住它（对两个 Store 都跑）。

## 读取接口的权限：配置里的邮箱白名单

`GET /api/feedback` **只有开发团队能读**，判据是 `feedback.readers`
（`shared/infra/config`，配置文件可配、环境变量 `AMA_FEEDBACK_READERS` 可覆盖）。

**「不在界面上放入口」不是权限控制**——任何人拿 curl 都能打，而且它的失败是静默的：
没有任何报错，只是数据被谁读了没人知道。

### 为什么选白名单，不给 API Key 加 admin 标记

给 key 加标记（那个看着更「正规」的方案）会撞上一条**已经写在三个地方**的不变量：
key 严格继承创建者的权限（`auth/apikey.go` 顶部、`repository/schema/auth_pg.sql`、
以及 `authz/matrix.go` 里 api-keys 那两条的定级理由）。那两条路由之所以能定 `ActionRead`，
靠的正是「一个 viewer 建出来的 key 也只能读」。

一旦 key 能带 admin 标记，**viewer 就能自己造一把管理员 key**——那是一条实打实的提权路径，
而且会让 api-keys 那两条的定级立刻不成立（那两处注释明确写了「哪天加了 scope，
这两条就必须重新定级」）。为一个内部读取接口去动整个 key 模型的地基，代价与收益不成比例。

邮箱白名单则**与 key 机制正交且自动兼容**：key 换出来的身份就是创建者的邮箱
（`auth.Identity.Email`），所以开发团队成员用自己的 key 调这条接口天然就能过，
不在名单里的人拿任何一把 key 都过不了。一条判断，两种凭据都覆盖。

平台级角色那个方案（现有 `authz` 的角色都是空间级的）代价最大，而这条接口是**跨空间**的
——引入一个新的权限维度只为一条内部接口，不值得。

### 留空即全部拒绝（fail-closed）

名单为空时**谁都读不到**，包括你自己。这与 `feishu.Config.Authorizer` 留空一律拒绝、
以及 `AllowAllAuthorizer` 被删掉是同一个取向：兜底放行时「忘了配」等于这道门根本不存在
且零报错，兜底拒绝则第一次点就发现。**授权要错在吵闹的那一边。**

启动日志两个方向都会说：名单为空时打 WARN 并给出配置方法（否则表现是「开发团队自己也
403」，而报错只说「只有开发团队可以查看」——看着像代码写错了）；配了就打出人数与名单。

### ⚠️ 这是第二处「授权写在 Service 层」

`authz/matrix.go` 里两条都是 `ActionNone`，但**理由完全不同，别把它们看成一组**：

- `POST`（上报）—— `ActionNone` 是**真的不需要授权**。对象不是空间资源，当事人从 context
  取，请求体里没有 email，所以「以别人的名义反馈」表达不出来。而且遇到问题的人本来就该
  能反馈——挂到空间下会让「还没加入任何空间的新人打不开页面」这个最需要反馈的场景反馈不了。
- `GET`（读列表）—— `ActionNone` **不代表它是公开的**，而是「Guard 管不了这条」：反馈是
  跨空间的，`authz` 的模型是「actor + 空间 → 角色」，这里根本没有空间可判。
  **门在 Service**（`core.ListFeedback` 开头）。

第一处是 `GET /api/workspace-by-key`（空间来自 query 参数）。两处都在代码里写明了理由，
且都有测试钉住——`httpd/feedback_test.go` 用**真装配**打真路由，证明普通用户
（**刻意挑空间 owner**）拿 403、只有白名单里的人拿 200。挑 owner 是为了证明
「空间角色与这条接口无关」；拿 viewer 测的话「因为角色不够」与「因为不在白名单」
两种解释都成立。

## 表结构与索引

`feedback_reports`，两级共存、靠 `level` 区分。**采集边界的差异在写入侧**（Service 的
白名单），不在表结构——分两张表的话读取接口要 union 两个不同形状再排序，而 `level`
只是一个枚举列。

`payload` 是 jsonb，其余是真列：**能查询/能聚合的拎成真列，剩下的进 doc**（同 `runs`
表的判据）。四个索引服务四条真实查询：按时间翻页、按 level + 时间、按 error_code + 时间、
以及 `dedup_key` 的唯一约束（那是限流的实现，**去掉它不报错，只是限流静默失效**）。

`log_id` 按同一条判据拎成了真列而不是塞进 payload：它的两个用途
（「拿用户给的 logid 反查那条反馈」「拿一条反馈去服务端日志里搜那次请求」）都是等值查找，
而 jsonb 里的键做不到（要 `->>` 取出来再比，用不上索引）。**可空、无 default**——
两级碰上 `network_error` 都是 NULL（请求没到达服务端），那是正确状态，
给它 `not null` 会逼出一个假值。加列照 `run_events.event_id` 那个先例
（`alter table ... add column if not exists`），但只要一条：**纯新增、不参与任何约束、
也不需要回填**（历史反馈本来就没有 logid，回填不出来也不该编）。

`route_pattern` 存模式串而不是真实路径，两个好处：低基数所以 `group by` 才有意义
（「哪个页面出错最多」是自动化改进的第一个查询），以及去重键要用它，而含 id 的路径
永远不会重复、去重就完全失效了。

**后端要再抹一遍 id**（`stripRouteIDs`），尽管前端已经抹过：这条接口是公开的 HTTP 接口、
调用方不一定是我们的前端，而含 id 的 `route_pattern` 会静默破坏限流与聚合两件事。

## 前端的三条

- **一级挂在两个 hook 上，覆盖读与写两条路径。** 读失败在 `useAsyncData` 的 catch 里，
  写失败在 `useWriteError` 的 `fail()` 里，两者都调 `lib/feedback.ts` 的
  `reportAutoFeedback`。三条不能改：不 `await`（否则错误态的渲染多等一个网络往返）、
  必须有 catch（unhandled rejection 会盖掉 `console.error` 那条真正有用的记录）、
  只采不敏感元信息。
- **`submitFeedback` 永远 resolve，从不 reject。** 上报失败是用户无法处理的事，
  而在一个已经出错的页面上再叠一个「反馈失败」是最难看的失败。所以它的返回值
  **不表示「上报成功」**，只表示「我们试过了」。
- **`reportAutoFeedback` 用动态 `import()`。** `api.ts → http.ts → feedback.ts →
  useAsyncData.ts` 已经是一条链，静态 import 会成环，而 ESM 的循环导入**不报错**——
  它让某个模块在初始化时拿到 `undefined`，表现是「某个页面偶尔白屏」且报错指向别处。

## ⚠️ 上线后表是空的：一级只覆盖了读失败

功能上线一整天，`feedback_reports` **一条记录都没有**。排查时先怀疑限流太严、
再怀疑后端，都不是——`curl` 打 `POST /api/feedback` 回 204、行也立刻落库。
真正的原因是**当初那次实现漏了一整条路径**：

- 一级只挂在 `useAsyncData` 的 catch 里，而那个 hook **只管「加载数据」**；
- 写操作（发消息、建 agent、保存设置）各自 `try/catch` 后 `setXxxError(e.message)`，
  渲染一句行内红字，**既不上报，也没有反馈入口**（入口挂在 `AsyncErrorBlock` 上）。

于是用户遇到「发送消息失败」时，**两级同时失效**：库里没有自动记录，界面上也没有
可点的东西。而这件事零报错——功能代码全在、`go test` 全绿、e2e 168 条全过、
构建通过，只是那段代码**从没被触发过**。

这不是 bug，是设计不完整：一级的语义是「出错就记」，不是「加载出错就记」。

修法是 `useWriteError`（`lib/useAsyncData.ts`）+ `InlineError`（`components/Page.tsx`）：
把写失败的 `setXxxError(e.message)` 换成 `fail(e)`，「分类」与「上报」变成同一个动作
——想拿到可渲染的错误态，就必然已经报过了。20 处写操作的 catch 各自记一次
是错的：那是 20 个漏掉的机会，而漏掉零报错。

**当初测不出来的原因值得单独记**：mock 里没有任何办法造一次写失败，
`16-feedback.spec.ts` 那 7 条全用 `failReadsMock`，而那个开关会让页面压根进不去。
于是「写失败要不要上报」这个问题在测试里**从没被问过**。`failWritesMock` 就是为
这件事加的——它与 `failReadsMock` 必须是两个开关，理由同
`failReadsMock` 与 `denyWorkspaceMock` 的关系。

## ⚠️ 长连接（SSE / WebSocket）整类都在采集盲区外

上面那条修完之后，一级覆盖了「读失败」与「写失败」。**但它们覆盖的都是 fetch 走的
请求**——`useAsyncData` 与 `useWriteError` 的原料是一个被 reject 的 Promise。
而 SSE 与 WebSocket 两样都没有：

- 不经过 `api` 那层（容器终端的 `EventSource` 是在组件里直接 `new` 的）；
- **没有可分类的异常**。`EventSource.onerror` 拿到的事件对象里没有状态码、
  没有响应体、没有错误消息（规范如此，为了不泄漏跨域信息），
  所以 `classifyError` 那条路根本走不通。

于是容器终端连不上时**一条记录都没有**，与上面那次「表是空的」是同一类漏，
只不过这次漏的是**一整种连接方式**，而且零报错。发现它的方式也一样：
用户报了一次「点进容器提示连接已断开」，回头查 `feedback_reports`，
`total=1` 且那一条与本次无关。

修法是第三条入口 `reportConnectionFailure`（`web/src/lib/feedback.ts`），
它自己拼载荷（`httpStatus` 填 0 = 没有拿到 HTTP 响应，语义同 `network_error`）。

### ⚠️ 但接上之后，风险立刻反转成「刷噪音」

**这一条比漏采更值得当心。** 会话结束后容器会空闲暂停，闲置超过 `docker.containerTTL`
（默认 24 小时）被回收巡检删掉。用户过几天点进一条历史会话，终端必然连不上——**那是符合预期的结果，
不是故障**。无条件上报的话，每一条被点开的历史会话都会刷一条反馈，
把真问题淹掉，而巡检 agent 会被喂一堆「正常现象」。

判据是**失败那一刻现查容器还在不在**（`GET /api/runs/{runId}/sandbox`）：
`running` 才报，`none` / `paused` / `gone` 一律闭嘴；**现查本身失败也闭嘴**
（宁可漏一条，不可刷一片——真故障通常可复现，用户会再点一次）。

这个判据可信，是因为后端已经把两件事分开了（`core/sandbox.go` 的 `RunSandboxState`）：
接不上时**404 才算 `gone`**，其它错误一律冒成 500 而不是谎称「已回收」。

⚠️ **不能拿组件手上那个 `status` 当判据**：`SandboxTerminal` 顶部就写着
「非 running 不建连接」，所以走到失败处理时它恒为 `running`——那道闸等于没设，
而它会安静地恒真。容器恰恰是在「进页面」与「连接失败」这两个时刻之间没的。

### 两条失败路径要分开标记

`terminal_disconnected`（SSE 断，收不到输出）与 `terminal_input_failed`
（上行 POST 失败，发不出输入）**是两种故障**，成因完全不同：前者是容器那侧断了，
后者常是 POST 落到了另一个实例（终端的 hub 是进程内状态，见 `sandboxterm.go` 文件头）。
用同一个码会让排查时分不出「屏幕不刷新」与「敲了没反应」。

**也不要复用 `network_error`**：那会让它混进「后端整个没起来」那一类里。

### 上报只能挂在 `readyState === CLOSED` 上

`EventSource` 会**自动重连**，`onerror` 在每次重连过程中都触发。挂在 `onerror`
入口的话，一次网络抖动就会上报十几条。只有它彻底放弃（`CLOSED`）才算真断开。

⚠️ 这条同时决定了 e2e 怎么写：**造失败必须用 `route.fulfill({status: 500})`，
不能用 `route.abort()`**。网络层失败会让 `EventSource` 回到 `CONNECTING` 并重连，
永远到不了 `CLOSED`，于是被测代码根本没跑——两条用例**双双假绿**。

### e2e 怎么造出「面板看到 running、失败时容器已没」

mock 的容器状态是每次调用现读 localStorage，单靠一个标记造不出这个先后
（标记是 `gone` 的话终端压根不渲染）。做法是用 `page.route` **拦住那条 SSE**，
把标记改成 `gone` 之后再放它失败——先后确定，不靠 sleep 抢跑。

⚠️ **这是 local 模式下极少数 `page.route` 真拦得到的请求**：`EventSource` 不走
`LocalApiClient`，是一个真的同源 GET。别把这条经验推广到别处。

⚠️ 「不上报」那条用例**必须先等一个渲染窗口再断言 0 条**：phase 变成 `recycled`
之后，上报那条路还要再走一次现查才轮到 `submitFeedback`。立刻断言的话，
**去掉判据它照样是 0**——用例假绿，而那条用例的全部意义就是防这件事。
两条都做过变异验证（去掉判据 / 换成 `network_error`），各自变红。

## ⚠️ 真实会话 id 是连字符式的，`{id}` 抹不掉它

`stripRouteIDs` / `looksLikeID` 原先只认四种形状：带下划线（`run_abc`）、纯数字、
长十六进制串、UUID。而**真实会话 id 是 `sesn-20260101000003-ex003`**——
一个下划线都没有，也不是纯数字/hex/UUID，**四条规则全不命中**。
于是一级上报的 `route_pattern` 里带着真实会话 id 落库，两个后果都不报错：

- **一级泄漏了「这个人刚才在哪次对话里」**，而一级恰恰是没有用户同意的那一级；
- 去重键每条都不同（**限流失效**），`group by` 变成一张每行计数 1 的表（聚合无意义）。

单测与 e2e 全绿，因为 `TestFeedbackStripsRouteIDs` 用的是 `run_abc123`（带下划线），
mock 种子也用 `run_1`。**同一个字段在测试环境与线上是两种形状**——这是这类漏洞的
共同成因，也是为什么这个洞只有「打真后端 + 查真库」才照得出来。

判据是「含连字符**且**有一段 ≥8 位纯数字」（`hasLongDigitRun`），不是「含连字符就抹」：
后者会把 `ark-agent`（真实空间 key）、`skill-hub` 这类路由字面量一起抹成 `{id}`，
那等于把这个字段搞废。两个方向各有一条测试钉住
（`TestFeedbackStripsHyphenatedSessionIDs` 与 `TestFeedbackKeepsRouteLiterals`），
**防漏放与防误杀必须配对**，只写一条会让人把判据改松或改紧都不报错。

## 测试

```bash
cd server && go test ./core/ ./httpd/ ./shared/infra/config/ -run 'Feedback'
cd web && npx playwright test e2e/16-feedback.spec.ts
```

Store 契约那 4 条**对 MemStore 与 PGStore 各跑一遍**（`runStoreSuite`）。限流的真正实现
是 PG 的 `on conflict`，而单测跑的是内存版——只在一边验会漏掉两个方向的分叉：
修好 PG 却让内存版继续攒重复（`AMA_STORE=mem` 一回退问题就复活，而单测用的正是内存版），
或者内存版对了 PG 却没建唯一索引（线上限流静默失效）。

**每条安全与限流断言都做过变异验证**（把实现注掉、确认用例真的变红）——
安全校验没有变异验证等于没验：

| 变异 | 变红的用例 |
| --- | --- |
| 去掉读取权限判断（两处都放行） | `TestListFeedbackDeniesOrdinaryUser` + `...WhenReadersUnconfigured` |
| 一级改用 `sanitizeUser`（边界破坏） | `TestAutoAndUserCollectDifferentFields` |
| 不算去重键（限流失效） | `TestAutoFeedbackIsRateLimited`（20 条而非 1 条） |
| `scrubText` 变恒等函数 | `TestFeedbackScrubsEmbeddedCredentials` |
| MemStore 对空 DedupKey 也去重 | `TestUserFeedbackIsNotDeduped`（1 条而非 3 条） |
| matrix 里 `GET` 误定成 `ActionRead` | `httpd` 那 4 条（500，报「路径里没有 workspaceId」） |
| 前端一级也采二级字段 | e2e「一级不采完整路径…」 |
| **退回「只有二级采 logid」** | `TestAutoAndUserCollectDifferentFields` + e2e 两条 |
| **logid 进 `dedup_key`**（限流静默失效） | `TestFeedbackLogIDIsNotInDedupKey` |
| **`network_error` 兜底成假 logid** | e2e「network_error 下不显示…」+「最近失败的接口调用…」 |
| **`select` 漏掉 `log_id`**（写进去读不出来） | `TestStoreSuite/PGStore/FeedbackLogIDRoundTrips` |
| **CORS 不 expose logid 头** | `httpd.TestCORS` |
| **`apiRoute` 不抹 id / 不砍 query** | `TestFeedbackAPIRouteIsSanitized` + `TestAutoAndUserCollectDifferentFields` |

### e2e 踩过的两个坑

**`guardRead` 只包了列表方法**（`listAgents` / `listSkills` / `listFeishuApps` /
`listApiKeys` / `listRuns`），**没包 `getRun`**——所以运行详情页在 `failReads` 下压根不会
失败。要测「URL 里有 id 的页面」用 agent 详情页。

**`Disclosure.Content` 折叠时内容仍在 DOM 里**（靠 `height` + `opacity: 0` +
`overflow: clip`，见其 BEM 样式），所以断言收起态要用 `toBeHidden()` 而不是
`toHaveCount(0)`。后者会红，而失败信息（「期望 0 个实际 1 个」）看着像组件没渲染对。

**断言「面板里没有某一行」之前必须先展开面板。** 收起态下**任何**文本都不可见，
于是 `not.toContainText(...)` 在那时是**恒真**的——那条用例会全绿，但它什么都没验到。
「network_error 下不显示假 logid」那条踩的就是这里。

**「面板不显示」与「不上报」要各钉一次。** 只验前者的话，一个「悄悄上报但界面不显示」
的实现照样绿，而那恰恰是这个功能最不能出的错（用户看到的清单 ≠ 真正发出去的东西）。

### ⚠️ 跑整套要用 `playwright.preview.config.ts`，而它必须带两个 project

dev server 跑不完整套（`fs.inotify.max_user_instances` 不够，中途死掉后连片
`ERR_CONNECTION_REFUSED`，**看着像大面积回归其实是环境**），所以整套走 preview。

⚠️ **那份 config 必须与 `playwright.config.ts` 一样有 `chrome` + `iphone` 两个
project**，且 `chrome` 要 `testIgnore: /mobile\/.*\.spec\.ts/`。少了 iphone 那个，
`e2e/mobile/` 那 9 条会在 1280×720 桌面视口下跑一遍——它们断言的恰恰是窄屏布局
（抽屉、竖排、顶栏在 md 断点以下可见），于是**连片变红且报错完全不指向视口**。
这次就是这么被绊了一下：13 条失败里 9 条是这个，补上 project 之后 16 条全绿。

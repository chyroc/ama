# 平台自巡检闭环 —— 定时任务发现问题、agent 自己记成 Issue

**读它的时机**：要改巡检 prompt、要加一种新的「素材」、或者想知道
「为什么判重靠标题前缀而不是服务端指纹」的时候。

代码：`core/scheduler.go`（认领、起会话、`deliver` 走 run_only 直接返回 nil）。
**巡检的全部业务逻辑不在代码里，而在 agent 的系统提示词里**——即「平台巡检员」
那个 agent 的 `system` 字段，改它走 `PATCH /api/agents/{agentId}`。
参考版本在 `testdata/inspector-agent-system-prompt.md`。

## 这条链路长什么样

```
定时任务（interval，1 小时）
  ↓ Scheduler 认领（PG 原子认领，多实例下恰好一个赢）
  ↓ 起一轮真 agent 对话（trigger=schedule，自动进运行历史）
  ↓ prompt 只有一句「开始本轮平台巡检，按你的系统提示词执行。」
  ↓ agent 自己查三类素材（feedback / 日志 / run 历史）
  ↓ agent 自己 `ama issue list` 比对标题前缀
  ↓
没见过 → `ama issue create`      见过 → `ama issue comment`（已关闭的自己重新打开）
  ↓ delivery=run_only，服务端**不解析**它的输出，只要它说了话就算成功
```

**零新增 HTTP 路由**，所以 `authz/matrix.go` 一个字都没改。

## ⚠️ 为什么是「agent 自己建」而不是「服务端代建」

另一种做法是 agent 输出一个 JSON 数组（`[{key, title, body, priority}]`），
服务端解析它、按 `sha256(空间 + key)` 算指纹、决定建还是评论。

不这么做的理由只有一条，但够硬：**那个 JSON 契约太脆，解析失败会让整轮巡检白跑，
而失败原因与「平台有没有问题」毫无关系。** 模型少写一个括号、把数组包进了对象、
在正文里多贴了一个代码块——任何一种都会把一轮体检变成一条
「解析巡检结果失败」，而平台到底健不健康没人知道。要写「多个围栏里只有一个是合法 JSON」这种兜底，本身就是这个契约站不住的证据。

### 判重责任跟着搬到了 agent 身上

代码算指纹换成了**agent 自己看列表比对**。
后者天生不如前者稳，所以提示词里用三条硬约束把它钉住：

1. **标题格式固定**：`[巡检] <标识>: <一句话>`。前缀 `[巡检] ` 把机器产出与人报的
   分开，`<标识>` 是跨轮次比对的唯一判据，冒号后面那句允许每轮不同；
2. **`<标识>` 从一张固定清单里选**（`amad-error-logs` / `log-fetch-failed`
   / `config-missing` …），清单里没有的才允许自造；
3. **建之前必须先 `ama issue list`**，比对 `[巡检] <标识>:` 前缀，
   **连已关闭的一起看**（复发要在原条目下追评论并重开，不是建新的）。

⚠️ 典型的失败模式：`log-unreachable` 与 `inspector-log-fetch-auth-fail` 指同一件事，
两轮写出两个不同的标识 → agent 自己认不出报过了，建了两条重复 issue。
**可推广的判据**：凡是要求 agent 产出一个「跨轮次必须逐字相同」的标识符，
都要给枚举而不是给命名规则。

### 第一轮真跑就违反了标题规范

实测第一轮 agent 建出的标题是
`前端请求了未实现的 GET /api/schedules（应走 ...）` 这样的——描述得很准，
**但没有前缀也没有标识**，于是它下一轮自己都认不出。
所以提示词里补了一段「建每一条之前先把标题念一遍，确认它以 `[巡检] ` 开头、
紧跟清单里的某个标识、然后是一个冒号」，并把这次失败原样写进去当反例。
补完之后第二轮正确地追了评论，没有建出第二条。

## ⚠️ feedback 现在由 agent 自己查，但那条采集边界一步都不能让

agent 自己 `ama api GET /api/feedback`，**拿到的是带 `actor` 邮箱和
`payload` 的原始记录**——边界靠的是「提示词要求」而不是「代码保证」，所以这条
必须写得很重。

feedback 一级（auto）是在用户**不知情**时采的，`README-feedback.md` 把
「采的每个字段都必须就算全世界看见也无所谓」写成了核心不变量。而 agent 写进
issue 正文的东西会流进四个可见范围宽得多的地方：模型上下文、SSE 流、
**RunEvent 永久记录**、以及问题列表本身。

所以提示词里的要求是：**只写聚合后的判因信息**（哪条路由、什么错误码、
什么状态码、多少次、影响几个人），**`actor` 一个都不许出现**（只出「几个人」，
哪怕只有 1 个人也不写他是谁），**`payload` 整个不读不引不抄**。
「有 3 个人在 /w/{key}/chat 上撞到 500」足够开一条问题，而它不泄漏任何一个人是谁。

### 白名单：巡检空间的 owner 必须在 `feedback.readers` 里

`GET /api/feedback` 是 fail-closed 的邮箱白名单（`feedback.readers`）。
容器里注入的 `AMA_TOKEN` 按**空间 owner**推导（`core/sessionenv.go` 的
`firstOwnerEmail`），所以巡检能不能读 feedback 取决于那个空间的 owner 在不在白名单里。
部署后在容器里跑 `ama api GET /api/feedback?limit=3`，拿到 **200** 才算通。

⚠️ **这个前提是数据决定的，不是代码保证的。** 换个 owner 不在白名单的空间去建巡检，
它会 403。那时正确的动作是把那个人加进 `feedback.readers`，
**不是**去放宽白名单机制本身（`authz/matrix.go` 里已经否决过「给 API Key 加 admin 标记」
那条路——viewer 就能自己造一把管理员 key）。

## 「没发现问题」算成功这条语义没变

| 这一轮的结果 | 算什么 | 为什么 |
| --- | --- | --- |
| agent 说「未发现问题」 | **成功** | 巡检最常见的结果就是一切正常。当失败的话，一个**健康**的平台会在连续 5 轮后被 `MaxConsecutiveFailures` 自动暂停——「平台越健康、巡检越早停」，而且停了没人知道 |
| agent 一个字都没输出 | **失败** | 那是卡死或超时（`fire` 里那条 `text == ""` 判断），我们**不知道**平台有没有问题 |

⚠️ **「解析不出来」这个失败模式不存在**——服务端不再解析任何东西，
`deliver` 在 run_only 分支直接 `return nil`。这正是要的：巡检的成败不再取决于
模型的输出格式。提示词里那条「一个字都不输出会被判为失败」是为了兜住剩下那个
真失败模式。

## Prompt 的门槛要写成硬条件

第一版写的是「错误次数明显偏高、或影响人数 ≥ 2」，实测 agent 面对
「5 次 500、影响 1 人」**判定为不值得报**——理由讲得还挺合理。但那是错的：
一个 500 就说明服务端真的出错了，而**上报量天然远小于实际发生量**
（只有点开那个页面、又恰好触发的人才会产生一条）。

改成硬门槛之后（`HTTP >= 500` 或 `次数 >= 3` 或 `影响 >= 2`，满足任意一条就报，
并明说「不要再自行加码」），同样的素材立刻建出了 issue。

**教训**：给 agent 的判据要写成可机械执行的条件。留有余地的措辞
（「明显偏高」）会被它用来论证不做事，而那个方向的错误是静默的。

## ⚠️ 日志查询参数写错会静默假阴性

**这是所有坑里最危险的一个**，因为它的失败方式是「报告一切正常」。

查错区域、服务名或时间窗返回 0 条，**和「服务真的没有日志」长得一模一样**。
agent 拿到 0 条会得出「平台正常」，于是这次巡检不仅没发现问题，还主动给了一个虚假的安心。

所以提示词里写了兜底：**确认查询参数无误还是 0 条，那本身就值得报一个问题**
（标识 `log-fetch-failed`），绝对不要据此说平台正常。另一个同类的坑：按条数截断的查询
是按**时间**截断不按实例采样，取小了会造成「某些实例零日志」的假象。

## ⚠️ 404 几乎都不是平台故障——第一轮就在这上面报错了

第一轮真跑，巡检在日志里看到
`请求了不存在的接口 method=GET path=/api/schedules` 与 `path=/api/workspaces/{id}`，
于是建了两条「前端请求了未实现的接口，需要前端修正路径」，还写了具体改法。

**两条都是错的。** 那是有人在开发机上用 curl 手工试接口打出来的 404，
前端一次都没请求过它们。结论完全反了，而它读起来毫无破绽——日志原话确实在，
路由确实不存在，推理链看着很紧。

所以提示词里加了一节：看到 404 先排除**你自己这一轮的探测**、
**别人手工 curl**（判据是只出现一两次且没有前后文）、**扫描器**三种来源。
并且**不许从「路由不存在」直接推出「前端拼错了路径」**——那两件事之间少了一步，
得先证明真的是前端发的，判据是 feedback 里有没有配套的自动上报记录。
改完之后第二轮正确地把同样那 3 次 404 归类为「工具探测类偶发，不计」。

**可推广的教训**：给 agent 的排除规则要按**来源**列举，不能只说「排除你自己的探测」
——共享环境里还有别人的探测，而它分不出那是谁的。

## `ama` 命令与凭据（历史四层，现已全部可用）

「容器里 `ama` 不可用」查下来是四层叠在一起，每修掉一层症状就换一个说法。
留档是因为自己部署时仍会撞到第 3、4 层：

| # | 症状 | 真因 | 状态 |
| --- | --- | --- | --- |
| 1 | `没有凭据：环境变量 AMA_TOKEN 是空的` | agent 的 `integrationIds` 没勾 `ama`（**空 = 不注入**） | ✅ 勾上 |
| 2 | 同上，勾了也没用 | **平台把自己注入的凭据过滤掉了**——见下 | ✅ 已修 |
| 3 | `HTTP 401 请先登录` / 连不上 | 没配 `server.publicURL` → 不注入 `AMA_BASE_URL` → CLI 回落到默认的 `http://localhost:9999` | ⚙️ 配上 `publicURL` |
| 4 | `connection refused` / `network is unreachable` | **会话容器回连不到 amad**：容器里的 `localhost` 指容器自己 | ⚙️ 见 [docker-executor.md](../../docs/guides/docker-executor.md) 的「容器里的 `ama` 命令要能打回 amad」 |

### 第 2 层：平台把自己注入的凭据过滤掉了（最隐蔽的一个）

`SessionEnvs` 返回的 map 里**两类 env 混在一起**：用户填的自定义 env，以及平台注入的
`AMA_TOKEN` / `AMA_WORKSPACE_ID` / `AMA_BASE_URL`。而当时容器工具那层按
`platformReservedEnvs` 过滤整个 map，于是**把平台刚算出来的值也吃掉了**。
最坑的是日志把人指向反方向（`自定义环境变量试图覆盖平台注入的凭据` ——
而 `agent.Envs` 其实是空的）。

⚠️ 第一版修法只放行「在 `secretKeys` 里的键」仍然不够：只有 `AMA_TOKEN` 是秘密，
另两个不进 `secretKeys`，于是 `AMA_WORKSPACE_ID` 照样被挡。
**那一层从原理上就分不清两类来源**——能分清的只有源头。所以过滤挪到了
`core/sessionenv.go`（在拼平台变量**之前**过滤用户 env）。

### ⚠️ `ama issue list` 不支持 `--status`

CLI 的 `issue` 子命令**把 `--status` 静默忽略**（它直接 GET
`/api/workspaces/{id}/issues`，不拼 query），所以 `ama issue list --status open`
拿回来的是**全部**问题。而 HTTP 接口本身是支持 `?status=` 的（认不出的值回 400）。
提示词里已写明这件事，并给了 `ama api GET ".../issues?status=open"` 这条替代。

`ListIssues` 没有标题搜索。这里接受了这个限制：agent 拉全量列表在本地比对前缀，问题量级小，够用。

## 排查时的几个坑

### 两个假象（都不是代码问题）

| 现象 | 真因 |
| --- | --- |
| `run-now` 返回 200，`nextRunAt` 却一直不动、也没有认领日志 | **实例正在被滚动替换**。认领发生在被替换掉的那个进程上，日志与计数都跟着没了。判据：当前在跑的实例与日志里打出认领的实例对不上 |
| 轮次跑了很久没结果 | 那一轮**还在跑**（`status=requires_action`，agent 在执行工具）。`runCount` 只在 `finish` 时才 +1，所以「计数没涨」≠「没在跑」。判据：`GET /api/workspaces/{id}/runs` 看有没有 `trigger=schedule` 且非 idle 的 run |

⚠️ 第二条尤其容易误判成「投递分支没生效」。**判「这一轮到底跑没跑」要看 run 的状态，
不要看 schedule 的 `runCount`。** 一轮通常要几分钟。

### ⚠️ PATCH 整条任务会清掉 `nextRunAt`

改 prompt 走 `PATCH /api/schedules/{id}`，而 `UpdateSchedule` 在 `specChanged ||
NextRunAt == ""` 时才重算——传的 spec 没变、原来的 `nextRunAt` 又被认领置空过，
于是它保持为空，**这条任务就再也不会被自动触发**（要再点一次 run-now）。
改完 prompt 记得补一次 run-now，或者顺手改一下 spec 让它重算。

### ⚠️ run-now 对 paused 的任务也要生效

`RunScheduleNow` 承诺「暂停的任务也允许试跑」，而如果认领条件只有 `status = 'active'`，
那条任务**永远不会被认领**，表现是「点了试跑，什么都没发生」——返回 200、`nextRunAt`
还更新了，看着像成功。

所以 `schedules` 有一列 `run_now_requested`，认领条件是
`(status = 'active' or run_now_requested)`，标记在认领那一刻被消费掉（一次点击只换
一轮），跑完 `finish` 看到仍是 paused 就不排下一次。

那一列不是可以省的：**「人工点了试跑」必须与「暂停前遗留的 next_run_at」区分开**，
否则把 status 条件一删，每条任务都会在**暂停那一刻**被立刻跑一次。

## 怎么建一套

1. **建 agent，绑 `skill-builtin-ama`**，
   `integrationIds` 要含 `ama`（**空 = 不注入凭据**）。
   ⚠️ **`skill-builtin-ama` 是必须的**：agent 要自己建 issue；
2. **把巡检逻辑写进 agent 的 `system`**，不要写进 schedule 的 prompt。
   拿 `testdata/inspector-agent-system-prompt.md` 改（日志查询命令按你的部署方式填）；
3. **确认那个空间的 owner 在 `feedback.readers` 里**，否则素材一会 403；
4. 建 schedule：`spec.kind=interval`、`everyMs >= 60000`、
   **`delivery.kind=run_only`**、
   prompt 只写一句「开始本轮平台巡检，按你的系统提示词执行。」；
5. `POST /api/schedules/{id}/run-now` 触发，别等一小时。
   **第一轮跑完必须去看它建出来的 issue 标题合不合规范**——这是最容易坏的地方。

巡检每轮都真的会跑并烧 token。**不想让它一直跑就去暂停**
（`PATCH /api/schedules/{id}/status` 传 `{"status":"paused"}`）。

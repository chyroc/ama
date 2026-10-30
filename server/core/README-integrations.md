# 授权管理（Integrations）—— provider 框架与四态模型

**读它的时机**：要加一个新的授权源之前，或者要改「授权状态怎么显示」之前。

代码：`service/integration/integration.go`（框架）+ `service/integration/ama.go` +
`core/integration_larkuser.go`（两个 provider）。

## 加一个授权源要改几处

**一处**：在 `cmd/amad/main.go` 的 `newIntegrationRegistry` 里 `Register` 一下。
路由、权限、前端卡片都不用动。

这是抽这个框架的全部理由。没有框架的话，每加一个源都要改路由、改 `authz/matrix.go`、改前端的卡片名单——
三处都改对才算接上，而**漏掉任何一处都不报错**：

| 漏改 | 症状 |
| --- | --- |
| 路由 | 前端拿到 404，页面显示「这版后端没这功能」 |
| authz 矩阵 | 启动 panic（这个还好，是唯一会吵的） |
| 前端名单 | **页面上少一张卡片，零报错**，没有任何人会收到通知 |

### ⚠️ 但「这个源要往容器注入凭据」是另一件事，那个不止一处

上面那句「一处」只覆盖**展示**（卡片、状态、授权按钮）。如果新的源还要把凭据注入
agent 容器（`Injectable: true`），那就要多改四处，**每一处漏了都零报错**：

| 位置 | 漏了会怎样 |
| --- | --- |
| `model/apimodel/apimodel.go` 的 `Integration*` 常量 | 勾选存不进去（前端存的 id 与后端判的对不上） |
| `core/sessionenv.go` 走一遍 `agentAllowsIntegration` | 那个勾选框成**死代码**：不勾也注入 |
| `shared/infra/shelltools/scrub.go` 的 `secretEnvNames` | **凭据原样流出容器**，后果是永久的（进 RunEvent） |
| `credential.go` 的 `platformReservedEnvs` + 前端 `RESERVED_ENV_KEYS` | 用户能用自定义 env 换掉平台注入的身份（权限提升） |
| `credential.go` 的 `AliasableEnvKeys()` | 用户没法给这把凭据起别名（第三方 CLI 认不了它）——见下面「凭据别名」 |

还有第五处，**只在 webshell 那条路上**：`core/sandbox.go` 的 `scrubShellOutput`。
走 `SessionEnvSource` + `secretKeys` 的源自动覆盖；自己搞一条新通道的就要单独加。

### 凭据别名：为什么是「映射」而不是让用户填值

⚠️ **下一个人极可能「顺手」把它改成让用户填一个值。那会开一个提权口子。**

需求是「兼容不读我们默认变量名的第三方 CLI」（例子：某个 CLI 只认 `GH_TOKEN`，
不读我们注入的那个变量名）。实现成**映射**：用户只声明「引用哪个平台凭据」，
**值由服务端在注入那一刻现填**（`domain.AgentEnvAlias`，只有 `key` 与 `from`，没有 value）。

这样绕开了那条安全边界而不是拆掉它：

- 用户**永远碰不到凭据原文**——不进请求体、不进库、不进 HTTP 响应、不进前端；
- `platformReservedEnvs` 与「平台凭据最后赢」两道防线一条都不用动；
- **天然继承脱敏**——别名的值与源逐字相同，而 `shared/infra/secret` 是**按值**替换的。

反过来做成「用户填值」就得回答「值存哪、怎么加密、谁能看到」，而本仓库**没有
at-rest 加密设施**。用户要填自己的值（比如平台根本没有的 ArgoCD 密码）走的是
**已有的自定义 env**（`AgentEnv`，需要的话勾 `Secret`）——两种需求，两套机制。

三条实现上的硬约束（改之前先读）：

| 约束 | 为什么 |
| --- | --- |
| 别名解析**只能在 `shelltools/injector.go` 的合并之后**（`prepareWithBase`） | 工具自身 env 与会话 env 只在那里同时可见 |
| 取值必须从**已合并的 `merged`** 里拿，不能自己再取一遍凭据 | 这样别名**天然继承 `integrationIds` 的授权判定**：没勾那个授权 → 源不在 merged 里 → 别名不产出。自己取值就成了绕过勾选框的口子，且零报错 |
| 别名是 `Agent` 上的**独立字段**，不能并进 `Envs` | `core/envprompt.go` 的 `renderEnvPrompt` 会把非敏感的 `Envs` 渲染进系统提示词。并进去等于让**真凭据**进模型上下文 → 落 RunEvent → 永久 |

权限与授权注入**同档位**（member 改不了），两个旧口子都堵了：
`ensureEnvAliasesUnchanged`（PATCH，判「值变没变」）与 `ensureCanSetEnvAliases`（POST，
判「非空」）。装配层用例在 `httpd/authz_envalias_test.go`，做过变异验证。

离线用例在 `core/envalias_test.go` 与 `shared/infra/shelltools/envalias_test.go`。

### ⚠️ 「谁能勾这个勾选框」是 admin/owner，不是 member

授权管理页那四条接口是 `ActionNone`（管的是「我自己的授权」，见下面「路由与权限」），
但**把授权注入给某个 agent** 是完全不同的一件事，档位是 **`ActionManage`**：

```
PUT /api/agents/{id}/integrations        ActionManage   改「这个 agent 注入哪些源」
```

理由是安全边界：注进容器的个人授权凭据（比如飞书用户 token）
**代表一个真人**，agent 一句 `env` 就能读到，
于是它能做的事等于那个员工能做的事。让 member 自己勾上，等于让他把别人的身份
借给一个程序。

**⚠️ 为什么不是把 `PATCH /api/agents/{id}` 整条提到 Manage** —— 这是下一个人
最容易做错、也一定会重新纠结的地方：

> 改名字、改提示词、换模型、绑技能**本来就该是 member 的日常**（ActionWrite），
> 而 `integrationIds` 只是那条 PATCH 请求体里的一个字段。把整条路由提上去，
> 等于为了锁一个字段把 agent 的日常编辑全锁给 admin——功能倒退，而且倒退得很隐蔽：
> member 只会发现「我改不了提示词了」，不会想到是权限矩阵。
>
> **矩阵是按路由判权的，判不了字段。** 所以字段要收紧就得先变成路由。这正是
> `ark-credential` 那组已经用过的范式（同一个 agent，PATCH 是 Write，
> 而 `PUT .../ark-credential` 是 Manage）。

**拆出路由只是一半，两个旧口子必须同时堵**，否则 member 走剩下那条就绕过去了，
且零报错。两道闸都在 `service/agent/agent.go`（Guard 判不了字段）：

| 口子 | 闸门 | 判据 |
| --- | --- | --- |
| `PATCH /api/agents/{id}` 带 `integrationIds` | `ensureIntegrationsUnchanged` | **值与库里不同**才拒 |
| `POST .../agents` 建 agent 时带 | `ensureCanSetIntegrations` | **非空**才拒（空是新建表单默认值） |

⚠️ **PATCH 那条判「变没变」而不是「传没传」**，这个区别是必须的：前端的
`toAgentDraft` **总是**回传完整草稿（isDirty 是裸 `JSON.stringify` 比较，少一个
字段会让保存按钮永久亮着）。按「传没传」判会让 member **连改名字都保存不了**，
而那个报错说的是「授权需要管理员」，完全不指向他实际做的事。

⚠️ 建 agent 那条**明确拒绝而不是静默忽略**：静默忽略会让 member 以为配上了，
然后 agent 在容器里一调对应的 CLI 就失败，而那个失败**不指向权限**。

这**不违反**「授权不下移到 Service 层」（`authz/README.md` 的系统驱动路径那节）：
那条禁的是**路由级**角色判断，而 `CreateAgent`/`UpdateAgent` 全仓库只有 HTTP
handler 两个生产调用方，且 ctx 无身份时放行（`actorIsAdmin`）。判据是
「这条链路有没有身份」，不是「代码在哪个包」。

前端对应的是**只读清单**而不是隐藏、也不是禁用的勾选框，理由见
`web/src/components/AgentRuntimeSections.tsx` 的文件头。角色走
`GET /api/workspaces/{id}/my-role`。

装配层用例在 `httpd/authz_integrations_test.go`（三条口子 + 对照组，做过变异验证）。

## ⚠️⚠️ 两类授权：个人 vs 服务账号（**这条决定了谁能配、用谁的凭据**）

**判据是 `domain.IntegrationKind`，一份白名单（`serviceAccountIntegrations`），
未知 id 一律算个人**——保守的那一侧。

| | 个人授权 | 服务账号 |
| --- | --- | --- |
| 例子 | 飞书用户 / AMA | 目前没有（`serviceAccountIntegrations` 是空的） |
| 凭据代表 | **一个真人** | 一个应用 |
| 谁能给 agent 加减 | **只有 agent 的创建者本人**（admin 也不行） | admin |
| 运行时取谁的凭据 | `agent.CreatedBy`（存量回落到空间 owner） | — |

将来加「代表一个应用」的源时，在 `model/apimodel` 的 `serviceAccountIntegrations` 里登记。

### 它修的那个洞

在此之前「勾了某个授权」之后取谁的凭据**一律是空间 owner 的**
（`core/sessionenv.go` 那几处 `firstOwnerEmail`）。于是任何能编辑 agent 的人勾一下
某个个人授权，他的 agent 就以 **owner 的个人身份**去调外部平台，
而 owner 收不到任何通知——注进容器的是代表真人的 token，agent 一句 `env`
就读得到，它能做的事等于那个员工能做的事。

修法是让**写路径的闸门**与**运行时的口径**配成一对：

```
写路径   ensureIntegrationsAllowed   个人授权只有创建者能加减
运行时   personalCredentialSubjectFor 个人授权就用创建者的凭据
```

⚠️ **两处口径必须一致。** 写路径判 A、运行时用 B 的话，闸门就是摆设。所以那个
resolver 是**一个函数**（挂在 Service 上），两个注入来源共用它——各写一份的话，
「AMA 用 owner、飞书用创建者」这种漂移不报错，而它恰好就是这个洞的形状。

### ⚠️ 存量 agent（`CreatedBy` 为空）回落到空间 owner

`CreatedBy` 是后来加的字段，存量 agent 上可能整个键都没有。那时**回落到改动前的
行为**，这是一次显式的迁移决定：

- 不回落（返回空 → 不注入）会让存量 agent 的能力**静默消失**，症状是「昨天还好好的，
  今天 agent 说 clone 不了」，而没有任何东西指向这次改动；
- 回落则存量行为逐字不变，新建的 agent 走新口径。

代价是存量 agent 上那个洞还在。**它有下限**（存量集合不会再增长），而反过来会当场
打挂线上功能。日志里会说清用的是回落口径。

写路径同理：`CreatedBy` 为空时回落到 admin 档位，报错明说「这个 Agent 没有记录
创建者」——不这么做的话存量 agent 上的个人授权**谁都改不了**，而报错指向一个
界面上根本不显示的人。

### 非交互式触发（定时任务 / 飞书消息）

那些链路**没有「当前用户」**，所以 resolver 刻意不看 ctx 里的 actor，只看
`agent.CreatedBy`。于是定时任务与人手点「跑一次」用的是同一把凭据，行为可预测。

⚠️ **创建者离职或撤销授权之后**：取凭据失败 → 当成「勾了却拿不到」**报错**
（不是静默不注入），错误信息里带上是谁的凭据。静默降级会让定时任务每天失败一次
而没人知道原因；报错会进 RunEvent，在运行历史里看得见。

⚠️ **但服务端那条日志的级别按错误类型分**（`shelltools.SessionInjector.sessionEnvs`）：
带 `ErrPrecondition` 的（他还没授权、服务端没接入某个源）打 **Warn**，
真故障（网络不通、平台 5xx）才打 **Error**。上面说的「报错」指的是**返回 error**
那件事，两者是独立的——降级的只是日志级别，错误照样往上报、照样进 RunEvent。

分级的理由：`_level=Error` 清单的价值来自「出现即有事」，而「某个用户还没授权」
是他自己就能修的配置问题，混进去会让每轮巡检都要人工排除一次，
排到最后整个清单会被当噪音跳过——真故障也跟着被跳过。

### 前端怎么判这两类

**读后端给的 `IntegrationState.kind`，绝不硬编码 provider id。** 那一位由框架的
`backfillKind` 统一覆写（`List` 与 `State` 两处都调），**provider 自己说的不算**——
留那个口子就会出现「某个源自称服务账号，而白名单说它是个人」，而两处不一致时
零报错，只是界面按错的那份渲染。

`IntegrationProvider` 接口上刻意**没有** `Kind()` 方法，就是为了不给这个余地。

## 接口

```go
type IntegrationProvider interface {
    ID() string          // 稳定标识，进 URL 与前端 key
    Name() string
    Description() string
    State(ctx, actor) IntegrationState              // 注意：不返回 error
    AuthorizeURL(ctx, actor) (string, error)
    Revoke(ctx, actor) error
}
```

**`State` 不返回 error，把失败编码成 `error` 状态。** 平台不通不该让整张列表 500——
那样其它 provider 的状态也一起看不见了，而它们是好的。`List` 并发查全部 provider，
单个 panic 由框架兜住。

**返回顺序 = 注册顺序。** 并发查但按序返回：顺序不稳的话卡片每次刷新都跳，
看起来像页面出了问题。

## 四态模型（ok / warning / error / missing）

这套模型最值钱的部分是每条状态都带 **`Summary` + `FixHint` + `FixURL`**——
不只说「坏了」，还说**怎么修、去哪修**。

| 状态 | 含义 | 谁该动手 |
| --- | --- | --- |
| `ok` | 好的 | — |
| `warning` | 能用，但要知道（快过期、平台会自动续） | 通常不用管 |
| `error` | 不可用，要动手（还没授权、凭证失效、平台不通） | 用户或管理员 |
| `missing` | 服务端还没配 | **管理员**，用户点按钮没有任何用 |

### 为什么不是二态

**二态最要命的是把 `missing` 显示成「未授权」**：用户对着「未授权」反复点授权按钮，
而问题在服务端配置上，他点一百次也没用。四态把「没配」与「没授权」分成两个状态，变成模型的一部分。

`expired` 归 `warning` 而不是 `error`：平台侧的刷新是自动的，绝大多数「过期」在下一次
代调时就自己好了；显示成 error 会让用户白跑一趟授权页。更完整的做法是
**三态过期判定**——access 活着 → ok；access 死了但 refresh 活着
→ warning +「无需人工干预」；两个都死 → error + 重新授权的链接。AMA 这边拿不到
refresh 的状态（平台只回 access token 与它的过期时刻），所以退化成两档，
但**分界线是同一条**：会不会自愈。

「有效期至」渲染成「2026-09-11 11:42:24（还有 14 天）」而不是裸 RFC3339
（`fmtExpiry`）：detail 在前端是原样显示的纯文本，裸时间戳要
用户自己算「这是多久之后」，而那恰好是他唯一想知道的事。

### 界面按 `canAuthorize` / `canRevoke`，不按状态猜

```go
CanAuthorize bool   // 「发起授权」现在有不有意义
CanRevoke    bool   // 「解除授权」有不有意义
```

**这两个字段是「未接入 ≠ 未授权」落到界面上的地方。** 前端据此决定渲不渲染按钮——
拿 `status` 去猜的话，`missing` 那张卡片会长出一个点了只能拿到 422 的按钮，
而用户无从得知要去配服务端。

同理 `canRevoke`：平台没有解除接口时为 false，**即便已授权也不给按钮**。
给一个必然失败的按钮不如不给。

#### ⚠️ `canRevoke = false` 只是不渲染按钮，不等于撤不掉

**`DELETE /api/integrations/{provider}` 是一条通用路由**（matrix.go 里那四条之一），
任何人都能直接打。所以把 `CanRevoke` 置 false **只影响界面**，provider 的 `Revoke`
如果照旧能删东西，那就是一个**没有界面、也没人会想到去看的隐藏口子**。

`ama` 不支持解除：

| provider | 为什么不支持 | 怎么落 |
| --- | --- | --- |
| `ama` | **需求明确不支持**（凭据由平台托管、自动创建自动续） | `Revoke` 返回 `ErrPrecondition` + `ErrRevokeUnsupported`；且 `ManagedAPIKeys` 接口上**刻意没有 Revoke 方法**，于是它在类型层面就拿不到删那一行的手段 |

⚠️ 两个哨兵都要用 `%w` 包：写成 `%s` 的话 `errors.Is(err, ErrRevokeUnsupported)` 是
**false**，判据只能靠 `strings.Contains`，文案一改就失效。

**撤掉一个撤销入口时，要同时回答「用户现在从哪儿处置」。** `ama` 那把 key 就是
`auth_api_keys` 里的普通一行，全局设置的「API Key」页面列得出来也吊销得掉，
所以它的 `FixHint` 指过去。⚠️ 但那条路的语义是**重置而不是轮换**（吊销完下次注入
会推导出同一把明文），所以文案不能说「吊销即作废」——那是假话，而用户会据此以为
自己已经处置完一把泄漏的凭据。真要换只能让管理员换 `auth.managedKeySecret`。

## 「不知道」是独立的一等状态，不能当成「坏了」

**这是最有价值的一条**：网络错误时**不改动授权状态**，让原有流程继续跑，
网络抖动不会强制重扫码。

落到 AMA 上：把一次超时显示成「不可用 / 未授权」会让用户去重新授权一遍，而他的授权
其实好着呢——**那次白跑的授权还会覆盖掉平台侧本来正常的凭证**。所以 `isTransient`
认出的错误（`context.DeadlineExceeded` / `Canceled` / `net.Error`）一律降级成
`warning`，Summary 明说「不代表授权已失效」，FixHint 明说「**不用重新授权**」，
且 `CanAuthorize` 为 false（点了只是白跑）。

判据只认 ctx 与 net 两类，**不看文案**——文案是上游给的，随时会变。
反过来业务失败（`credential grant not found` 这类）**不能**当成 transient：
那会把「真的没授权」说成「稍后再试」，于是用户永远等不到那个该他点的授权按钮。

## 单项超时，慢的不拖累快的

`providerTimeout`（10s）逐项设，不能只靠外层请求超时：这是一个用户在等页面的同步路径，
一个卡住的 provider 会把整张页拖到浏览器超时，而那时其它 provider 的状态明明都拿到了。

## 上游没给原因时不留空话

`Summary` 兜一句「平台报错，但没给具体原因」而不是空串——
让用户知道是**上游没说**，而不是我们把话丢了。空 Summary 在页面上表现成一张
「不可用」但什么也不解释的卡片，那比说「没给原因」更让人无从下手。

## ⚠️ 凭据绝不进 `IntegrationState`

`State` 会原样进 HTTP 响应、进前端。所以**任何 token / secret 都不许出现在它的任何
字段里**，包括 `Summary` 与 `FixHint` 这两个自由文本——最容易漏的正是「顺手把上游
错误拼进 Summary」，而上游错误里可能带着凭据。

`TestIntegrationResponseNeverLeaksCredentials` 逐字节搜整个响应体来锁这条。

要展示凭据相关信息只能过 `maskCredential`，它**只留尾部、不留前缀**
。露前缀是很常见的写法，但前缀往往
正是最不该露的部分：`sa_g_` 点出账号类型、`app-` 点出租户，而尾部几位已经足够让人
分辨「这是不是我配的那一把」。短到无法脱敏时**整个打码**，不是原样返回。

**两处容易写错的地方**：

- **短值打码定宽三星**，不用 `strings.Repeat("*", len(s))`。输出 `len(s)` 个星号等于
  透露了长度，且对短 secret 恰好最有害：
  长度把暴破空间直接缩小一个量级。
- **过期时刻过一道区间 sanity check**（`plausibleExpiry`，2020–2100）。外部平台的
  `expire_at` 可能是毫秒，当秒解析得 58624 年，那把 token 永远「没过期」、
  缓存再也不刷新，过期后每次调用都 401。反向搞错得 1970 年。两种都会被 `fmtExpiry` 老老实实渲染成「还有 106751 天」/「已过期 20672 天」
  ——区间外的一律不显示，而不是把假话摊给用户。

⚠️ 这个函数**目前没有调用方**（现有的源的卡片都不展示凭据片段）。
将来要展示时用它，别另写一个。

## Details 放什么：判据是「用户看了能做出不同决定吗」

`Details` 是卡片上那串灰色名值对。它最容易长成一个垃圾抽屉——每个 provider 的作者
都顺手把自己排查时想看的东西塞进去，于是用户面对一堆他既读不懂、也改不动的串。

**判据只有一句：这条信息能让用户做出不同的决定吗？** 不能就别放。按这条清过一轮，
删掉的是（每条都在对应源的代码里留了论证）：

| 删掉的 | 为什么它不合格 |
| --- | --- |
| 飞书的「飞书 open_id」 | `ou_` 开头的串，对用户不可读、不可操作。「授权用户」已经答了「是谁」 |
| 飞书的「凭据有效期至」 | access_token 到期会在注入那一刻自动续，**用户无感也无需处置**；把它摆出来只会让人以为该做点什么。`需重新授权于` 相反——那条到期就真的要重新授权，所以留着 |
**留下来的两类**（别为了「更短」把这些也删了）：

1. **可行动的引导**——`missing` / `error` 的 `FixHint` / `FixCommand` / `FixURL`；
2. **身份归属**——「当前用的是哪个 app / 哪个账号」（飞书的 `飞书应用`、
   `本空间生效的授权`）。它们有**可选项**，所以「用的是哪一个」是真问题。

⚠️ 时间值一律过 `expiryDetail` / `fmtPastTime`，**不要 `t.Format(time.RFC3339)`**：
裸 ISO 串要用户自己算「这是多久之后」，而那恰好是他唯一想知道的事；且 CLAUDE.md
明令别用 `RFC3339`（它抹掉 `.000`）。

## 卡片顺序 = 注册顺序

页面按后端返回的顺序渲染，而那就是 `newIntegrationRegistry` 里的注册顺序
（`IntegrationRegistry.List` 并发查但按注册顺序返回，
`TestIntegrationListKeepsRegistrationOrder` 锁住这条）。

页面按 `kind` 分成「个人授权 / 服务账号」两组，分组只按 `kind` 切一刀、`filter` 保序，
**组内仍然严格是注册顺序**。

⚠️ **不要在前端补一个排序数组。** 页面刻意不硬编码 provider 名单，
补数组的话后端将来加的源会掉到数组外面——漏改零报错，只表现成顺序莫名其妙。
（分组本身不算：它读的是后端给的 `kind`，前端只定「个人组在前」这一个常量顺序。）

⚠️ 一个 provider 能不能提前注册，判据是**它依赖 `coreStore` 还是 `coreSvc`**：
前者在 `run()` 开头就建好了，
后者建在 registry 之后（飞书用户授权是这一类，只能在 `run()` 里注册）。

## 路由与权限

```
GET    /api/integrations                        列全部
GET    /api/integrations/{provider}             查单个（「重新检查」只刷一张卡片）
POST   /api/integrations/{provider}/authorize   发起授权
DELETE /api/integrations/{provider}             解除授权
```

**`/api/integrations` 与 `/api/integrations/{provider}` 并存不冲突**：Go 1.22 的
ServeMux 只在两个 pattern「谁也不比谁更具体」时才拒绝，而这里前者是后者的父路径、
段数不同。真正会 panic 的是 `{provider}` 与某个字面段同层并存——所以 `authorize`
挂在 `{provider}` 之下而不是与它同层。

**四条全是 `ActionNone`**（`authz/matrix.go`）。这不是忘了定级：

- 对象**不是空间资源**。查的是「我在某个外部平台上的授权状态」，pattern 里没有
  workspaceId，也不该有；
- **当事人从 context 取、不是请求参数**（每个方法开头都 `auth.ActorFrom(ctx)`）。
  用户 A 打这四条接口，看到与操作的永远是 A 自己的授权——**越权在这里表达不出来**；
- 定 `ActionRead` 会**更糟**：Read 要求空间成员身份，于是一个还没加入任何空间的新人
  打不开「授权管理」，而他恰恰最需要先在这里授权一次。

⚠️ **如果哪天加了「按 actor 查别人授权」的能力，这四条必须重新定级**——那时越权才
第一次成为可能。

## 状态一律实时查，不落库

理由：授权是用户在**平台侧**点出来的，平台是唯一权威。
我们存一份「已授权」就会与平台不一致，而不一致的表现是**「页面显示已授权、工具调用
照样失败」**——最难自查的一种。多实例下更糟：A 实例存了 B 没存，刷新两次看到两种状态。

所以这里没有任何 `Store`，代价是每次查状态要打一次平台（并发查，所以总耗时取最慢那个）。

## 测试

**全部离线**（`go test -race ./...` 必须离线全绿）：两个 provider 的上游都要真凭据，
打不了。所以离线锁的是「不打网络也能出错的判断」：

- **路由分发**——用**两个** provider 逐个断言拿到的是自己。只注册一个的话，
  「路径参数没接对、永远返回第一个」与「接对了」表现完全一样；
- 未知 provider 回 404 + `code=not_found`（静默返回第一个是最坏的：用户在
  「AMA」卡片上看到飞书用户授权的状态）；
- 空注册表回 `[]` 而不是 `null`（`null` 会让前端 `.map` 炸掉，整个 tab 白屏）；
- 四态各自的序列化、`missing` 不给 `canAuthorize`、凭据不进响应。

前端那 8 个 e2e 用例在 `web/e2e/11-integrations.spec.ts`，靠 `LocalApiClient` 里
**四个假 provider** 覆盖四态——尤其 `missing`，它在真实配置下几乎测不到，
而它恰好是最容易写错的那条。

### 两条测试自己的坑（都实测踩过）

1. **`toHaveCount(0)` 会在骨架屏阶段就先满足。** 断言「没有那个按钮」之前必须先等
   一个肯定存在的元素，否则那条断言是一句永远为真的废话——把实现改成「按 status 猜
   按钮」时，本该抓住它的用例照样绿。
2. **`isLoading` 那条分支测不到**，因为 mock 的 `delay` 只有 90ms，断言还没跑到就
   加载完了。所以加了 `hangReads()` 钩子把页面钉在加载中——不这么做的话，
   「把 isLoading 当成空状态」这种错误一个用例都不会红（实测：8 个用例全绿）。
   local 模式下 `page.route` 拦不到任何东西（一个网络请求都不发），只能由 mock 给开关。

## 飞书用户授权（`lark-user`）

它与另一个源（AMA）不一样，三处不同都是**实现前就该知道**的：

| 维度 | AMA | `lark-user` |
| --- | --- | --- |
| 凭据落库 | 一律不落（平台是权威） | **必须落**，否则无法续期 |
| 授权动作 | 平台页面点完即生效 | **设备码，要轮询**才拿得到 token |
| 解除授权 | 假撤销 | **真撤销**（删了必须重走 OAuth） |

### 为什么它必须落库（而 AMA 刻意不落）

本文件与 `integration.go` 反复强调「状态一律实时查」。**那条纪律管的是别人家平台的
状态，而这里存的是凭据本身**：access_token 约 2 小时过期，而**续期的唯一手段是我们
自己保存的 refresh_token**——飞书只在授权成功那一刻给一次。不存就没有这个功能，
不是「存一份更方便」。判据仍是那句：**这个东西的权威在谁手里。**

它因此是全仓**第一份必须可解密**的凭据（`auth_api_keys` 存 sha256 是够的，这里不行）。
落库走 `shared/infra/cryptobox`，加解密收在 Store 层，见 `schema/core_larkuser_pg.sql`。

### 用谁的身份：空间 owner（代价已知）

与 AMA 那条注入路径一致。**代价是实测出来的**：飞书的用户身份只看得到那个人自己可见的
会话，所以拿 owner 的 token 去读一个 owner 不在的群，回的是
`230002 Bot/User can NOT be out of the chat`——**功能直接失败**，而不只是理论上的
越权风险。反过来，owner 在的群里 agent 能读到提问者可能看不到的内容，那是越权面。

仍然选 owner 是因为**注入接口拿不到「谁在说话」**（`SessionEnvs` 只收 arkSessionID），
而要按说话人就得改接口 + 建「飞书 open_id → 员工邮箱」映射表，
而入站事件里压根没有邮箱。完整论证见 `core/integration_larkuser.go` 的文件头。

### 四个真机踩到的坑（本地单测一个都照不出来）

1. **发起设备码少传 `client_secret` → `20140 The auth method is not supported.`**
   那句话听起来像「这个应用不支持设备码」，于是会去查应用类型 / archetype /
   开放平台开关。实测拿 `lark-cli` 自己那个**已知可用**的 app_id 也回同一个错，
   补上 secret 立刻成功。**判据：20140 先怀疑少参数，别怀疑应用能力。**
2. **open 域的路径不能带 `/open-apis` 前缀。** `DefaultOpenURLFeishu` 本身就以它结尾，
   写了就拼成 `/open-apis/open-apis/…` → **404**，而那个 404 看起来像「飞书没这个
   接口」。⚠️ accounts 域那个常量恰好**不带**前缀，两个域形状不一致。
3. **换 token 的响应里没有名字与 open_id**，要另调一次 `/authen/v1/user_info`。
   不补的话库里那两列是空的，卡片上「授权用户」空着。**失败不该判成授权失败**——
   那会让用户白扫一次码。
4. **`+chat-messages-list` 返回的字段叫 `messages`，不是 `items`。**
   按「飞书列表接口一律 items」的直觉去数会永远得到 0，看起来像「没取到消息」。

### scope：用户身份能绕开那个卡住 bot 的租户权限

读群历史有**两个几乎同名、归属不同**的 scope，别混：

	im:message.group_msg              租户/bot。敏感权限、要管理员审批，**存量应用加不上**
	im:message.group_msg:get_as_user  **用户**身份，跟着用户授权走，不需要审批

所以这条路不受「应用没申请到租户权限」的阻塞。判据是错误码：
`230027` = 缺权限，`230001` = **权限过了**只是 chat_id 不对。

### 怎么验（四段任何一段坏了都表现成同一个 401）

```bash
cd server && go run ./cmd/probe-lark-user        # 要本地库里已有授权行
# 可选：AMA_PROBE_CHAT_ID=oc_xxx 读一个真实群，那才是「真取到东西」的证据
```

它刻意在**隔离 HOME** 下跑 `lark-cli`——不这么做的话「成功」可能来自本机既有的
`~/.lark-cli/config.json` 而不是我们注入的 env，那是个假绿。

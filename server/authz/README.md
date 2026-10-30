# authz —— 基于空间成员角色的授权

**读它的时机**：新增 HTTP 路由之前（**必须**在这里声明权限，否则启动 panic），
或者要改「谁能做什么」之前。

## 这一层负责什么

```
auth  回答「你是谁」 —— SSO 会话 / API Key → Identity（邮箱即 Actor ID），不通过 401
authz 回答「你能不能」—— Actor + 空间 → 角色 → 够不够这个动作的等级，不通过 403
```

认证在前、授权在后。装配链条在 `httpd.New`：

```
mux 路由 → auth.Protect（认证）→ authz.Guard（授权）→ 业务 handler
```

## 角色权限矩阵

四个角色（`domain.WorkspaceRole`）映射到三个等级，**owner 与 admin 同级**：

| 角色 | 等级 | 能做什么 |
| --- | --- | --- |
| `owner` | Admin | 全部 |
| `admin` | Admin | 全部（与 owner 权限面相同，只是不受「不能移除唯一 owner」保护） |
| `member` | Member | 读 + 写：发起对话、触发 Run、增删改 Agent 与技能、改定时任务 |
| `viewer` | Viewer | 只读：看空间、浏览 Agent / 技能 / 运行历史、看对话 |
| 非成员 | None | 什么都不能，一律 403 |

三档动作与最低等级：

| 动作 | 最低等级 | 覆盖 |
| --- | --- | --- |
| `ActionRead` | viewer | 列表与详情、用量、看方舟凭证（只回掩码） |
| `ActionWrite` | member | 建改删 Agent / 技能 / 定时任务、发起对话、追加事件 |
| `ActionManage` | admin | 空间设置、成员增删改角色、删空间、方舟凭证写操作、飞书接入 |

**owner 与 admin 不分级**是刻意的：分了之后每处判断都要想一遍「这件事 admin 能不能做」，
而答案永远是「能」。owner 的特殊性只体现在数据约束上（`core/workspace.go` 的 `countOwners`）。

**账号级的那几组是 ActionNone，不是「忘了定级」。** `/api/integrations`、
`/api/user-onboarding`、`/api/user-preferences`、`/api/global-ark-credential`、
`/api/api-keys` 这几组的对象**不是空间资源**，pattern 里没有 workspaceId 也不该有。
越权在它们身上**表达不出来**：当事人只从 `auth.ActorFrom(ctx)` 取，路径与请求体里
都不出现 email——不是「校验了当事人」，是「说不出别人来」。

⚠️ 定 ActionRead 会**更糟而不是更安全**：Read 要求空间成员身份，于是还没加入任何
空间的人打不开引导、配不了自己的全局凭证、管不了自己的 key——而他恰恰最需要这些。

⚠️ 反过来说，哪天给它们加了「按 email 查别人的」能力，或者给 API Key 加了 scope
（「建一把管理员 key」），**这几组就必须立刻重新定级**：那一刻越权才第一次成为可能。

**方舟凭证的写操作是 Manage 而不是 Write**：那是一把空间级的付费凭证，换掉或删掉会影响
这个空间所有 agent 的对话，与「改一个 agent 的提示词」不是一个量级。`GET` 与 `verify`
是 Read——前者只回掩码，后者不写任何东西。

**成员管理全部是 Manage**：能改成员就能把自己提成 owner。

## 新增路由必须在矩阵里声明，否则启动 panic

权限按 **method + pattern** 声明在 `matrix.go` 的 `routePermissions` 里。Guard 在注册
路由时**双向**比对那张表：

| 方向 | 症状 | 何时报 |
| --- | --- | --- |
| 注册了但没声明 | 那条路由**静默放行**，任何登录用户都能打 | `HandleFunc` 里 panic |
| 声明了但没注册 | pattern 打错字（`workspaceld` vs `workspaceId`），声明永远匹配不上 | `Verify()` 返回错误 |

选 panic 而不是记日志，因为授权属于**不生效也看不出来**的东西：所有接口照常响应、
测试全绿，只是权限没了。本仓库有过「纯新增模块但没接到主路径」的事故，同一类问题。

只查一个方向查不出打错字那种：真路由会因为「没声明」而 panic，报错指向那条真路由，
让人以为是漏了声明，而实际是声明里有个错字。

⚠️ `/api/auth/` 那七条与 `/healthz` **不要写进表**：它们注册在裸 mux 上，不经过
`protectedMux`，也就不在 Guard 视野里，写了会触发反向 panic。
`_debug` 那两条在 `optionalRoutes` 里豁免（只在 `DevRoutes` 开时注册，线上不开）。

## 新增路由还要归进「角色权限说明表」，否则测试红

除了上面那条 panic，新增路由还有**第二道门**：`capability.go` 把全部要权限的路由按
「功能域 × 动作档位」归纳成一张给人看的表，前端在空间设置的成员页底部展示它
（`web/src/components/RolePermissionTable.tsx`，默认收起）。

```
matrix.go（真源）→ capability.go（归纳）→ gen-authz-capabilities（渲染）
                                        → web/src/data/roleCapabilities.ts（入库产物）
```

**为什么不让前端手写那张表**：手写的权限说明必然漂移。而过时的权限说明**比没有说明
更坏**——用户照着它以为自己能做某事，实际被 403，然后去找管理员要权限，而管理员看
同一张表也觉得他应该能做。两个人被同一张纸骗了。做法照抄 OpenAPI 那套
（`openapi/routes.go` 文件头），连对账形状都一样。

漏了会怎样：`go test ./shared/gopkg/authzdoc/` **直接红**，两个方向都查——

| 方向 | 报错说什么 |
| --- | --- |
| 路由归不进任何功能域 | 去 `capability.go` 的 `domainOf` 加一条 case |
| 某个 (域, 档位) 没人描述 | 去 `capabilityDecls` 加一行，或并进语义相同的已有行 |
| 某行描述没有对应的路由 | 那个能力已经没了，删掉那条 pair |
| 产物没重新生成 | `cd server && go run ./cmd/gen-authz-capabilities` |

**已做变异验证**：往 matrix 加一条落在新域的路由、加一条落在已有域但新档位的路由、
以及把 admin 从 LevelAdmin 降下来，三种都真的变红。

⚠️ **表里 owner 与 admin 那两列必然完全相同**，这是画这张表最容易画错的地方——
直觉会给 owner 多打几个勾，而那是错的。所以 `Roles` 是**逐个角色问 `levelOf` 算出来
的**，不是抄的结论；`TestOwnerAndAdminAreIdentical` 钉住它，免得被谁「顺手修正」。
真要给 owner 更高权限，改的是 `levelOf`，不是那张表。

⚠️ **ActionNone 的路由不进这张表**（账号级那几组）。它们不是「权限最低」，是
「与空间角色无关」，混进去会让用户以为只读角色也能改别人的东西。表下有一句话专门
交代它们在「全局设置」里——否则用户找不到「授权管理」会怀疑表漏了。

## 三条系统驱动路径必须绕过授权

**这是本包最容易被「顺手改对」而搞坏的地方。** 下面三条路径的 ctx 里**没有身份**，
它们不该被授权拦：

| 路径 | 位置 | 拦下去的后果 |
| --- | --- | --- |
| worker 池巡检 | `core/worker.go` 的 `refresh` | 扫到 0 个空间 → agent 一调工具卡在 `requires_action`，**不报错** |
| 飞书入站消息 | `feishu/inbound.go` 的 `dispatch` | 机器人静默不回话 |
| 定时任务触发 | `core/scheduler.go` 的 `fire` | 定时任务全失败 |

它们直接调 `core.Service`，不经过 HTTP，所以天然不经过 Guard。**不要为了「更彻底」
把检查下移到 Service 层**——那不是更安全，是把这三条链路弄坏，而且三种故障都不产生
指向真实原因的报错。

同理，`core.Service` 上有两个列空间的方法，**分工不能合并**：

```go
ListWorkspaces(ctx)            // 全部空间，给 worker 池那类系统路径
ListWorkspacesFor(ctx, actor)  // 只给 actor 所在的空间，给 HTTP
```

## 邮箱比较大小写不敏感，但 AddMember 的查重不是

`auth.Identity.Email` 在入口被小写化（`auth/normalizeEmail`），而成员的 email 是
**管理员手输**的，可能写成 `Bob@example.com`。所以查角色用 `strings.EqualFold`
（`core/workspace.go` 的 `memberOf`）。

但 `AddMember` 的唯一性校验**仍然是大小写敏感**的精确匹配——那条照抄 LocalApiClient，
改了会拦掉前端允许的操作。两者刻意不一致，问的不是同一件事：前者是「这是不是同一个人」，
后者是「前端允许不允许这次添加」。

## 资源不存在回 404，不回 403

扁平路径（`/api/agents/{id}` 之类）的空间要按资源 id 反解。反解不到时**必须回 404 +
`code=not_found`**，不能为了「不泄露存在性」统一回 403：前端有六个可空 getter 靠那个
code 把响应转成 `null`（`web/src/data/http.ts` 的 `nullable`）。回 403 会让
「这个 agent 已经被删了」变成「无权访问」——用户以为是权限问题去找管理员，
而那个 agent 早就没了。

顺序因此是：**先反解（不存在 → 404）→ 再判成员（非成员 → 403）→ 再判等级（不足 → 403）**。
查库出错走 500，不是 403——数据库抖动表现成「你没有权限」会让用户去找管理员要权限，
而管理员看他明明是 owner。

**一个例外**：`GET /api/runs/{runId}/events` 反解不到 run 时要**放行给 handler**。
它属于「列表接口永不 404」那一类（见 `core/handler.go` 文件头），handler 自己回 200 + `[]`。
Guard 抢着回 404 会把前端的空列表变成抛异常。这是全表唯一一条 `allowMissing`。

## Guard 盖不住的那一条

`GET /api/workspace-by-key` 的空间来自 **query 参数**而不是路径参数，按 pattern
声明权限表达不了它。所以那条声明为豁免，**成员校验落在 Service 里**
（`core.GetWorkspaceByKey`，非成员返回 `ErrNotFound`）。

这是整个方案里唯一「授权写在 Service 层」的地方，代码里有注释说明，
别以为是漏了。回 404 而不是 403 也是刻意的：前端 `WorkspaceGate` 靠 `nullable()`
把它转成 `null` → 跳回空间列表，是既有行为；顺带也不泄露「这个 key 存在但你进不去」。

`GET /api/workspace-key-taken` 则**必须保持全局查重**，不能按成员过滤——否则用户会
建出一个 key 已被别人占用的空间，然后在保存时撞唯一约束。

## ⚠️ 矩阵判得了路由，判不了字段——要收紧字段就先拆路由

**这是最容易做错的一类改动**，而做错的方式很有诱惑力：需求说「XX 这件事要收紧到
admin」，而 XX 是某条 PATCH 请求体里的一个字段，于是顺手把那条路由的档位提上去。

**不要这么做。** 那条路由上的**其它**字段往往本来就该是 member 的日常，提级等于
为了锁一个字段把整个资源的编辑权收走——功能倒退，而且倒退得很隐蔽：
member 只会发现「我改不了提示词了」，不会想到是权限矩阵。

正确的做法是**把那个字段拆成独立路由，单独定级**。仓库里有两组先例：

| 资源 | 日常编辑（Write） | 敏感字段（Manage） |
| --- | --- | --- |
| Agent 的方舟凭证 | `PATCH /api/agents/{id}` | `PUT /api/agents/{id}/ark-credential` |
| Agent 的授权注入 | `PATCH /api/agents/{id}` | `PUT /api/agents/{id}/integrations` |

**⚠️ 拆路由只是一半。** 旧路由必须同时拒绝在请求体里改那个字段，否则档位定得再高
也没用——member 走旧口子就绕过去了，**且零报错**。那道闸只能落在 Service
（Guard 看不见请求体），与下面「Guard 盖不住的那一条」同一类豁免。

两条实现纪律，都是踩出来的：

- **判「值变没变」，不是「传没传」。** 前端通常全量回传草稿（本仓库的 `toAgentDraft`
  就是，因为 isDirty 是裸 `JSON.stringify` 比较）。按「传没传」判会让 member
  连改名字都保存不了，而报错说的是「这个字段需要管理员」，完全不指向他做的事。
- **创建接口是第三个口子，最容易漏**——它不在「修改」这个心智模型里。
  `POST` 建资源时带上那个字段同样要拦，而且要**明确拒绝而不是静默忽略**：
  静默忽略会让人以为配上了，然后在很远的地方失败（容器里、下一次运行时），
  那个失败不指向权限。

### ⚠️⚠️ 还有一类闸门连「档位」都表达不了：判据不是角色，而是**这一行与当事人的关系**

上面那节讲的是「字段要收紧到某个角色」。但有一类规则**没有对应的角色**：

> 「个人授权只有 **agent 的创建者本人**能加减」——admin 也不行。

`admin` 不是它的答案，`owner` 也不是。判据是 `agent.created_by == actor`，
那是**数据与当事人的关系**，矩阵里压根没有这个维度（矩阵只知道「你在这个空间是什么
角色」）。所以它只能落在 Service，而且**不能**靠「提到 Manage」来近似——
那会让 admin 能代人注入别人的个人凭据，而那正是要防的事。

⚠️ **这类规则的存在，意味着「Guard 放行」不等于「这件事允许」。** 于是那条专门
用来改该字段的路由（`PUT /api/agents/{id}/integrations`，已经是 ActionManage）
**仍然要在 Service 里再过一次闸**——很容易觉得「Guard 判过了这里不用判」，
而漏掉的表现是：PATCH 那条被堵住了，这条**专门改授权**的路大开。
`TestSetAgentIntegrationsRouteAlsoEnforcesCreator` 钉住它。

⚠️ **错误码要选对**：这类拒绝是「**有**权限但不该做这件事」。
`ErrAccessDenied`（403）适用于「不是创建者」；「前提条件不满足」那类拒绝
要用 `ErrPrecondition`（422）——回 403 会让管理员以为自己权限不够，
去找 owner 白跑一趟。用例见 `core/authscope_test.go` 与
`httpd/authz_authscope_test.go`。

⚠️ **存量数据没有那个关系时要回落，且回落方向要显式**：`created_by` 是后加的字段，
存量 agent 上可能整个键都没有。那时回落到 admin 档位（改动前的行为），
并在报错里说清「这个 Agent 没有记录创建者」——否则那些 agent 上的字段**谁都改不了**，
而报错指向一个界面上根本不显示的人。

现成例子见 `core/agent.go` 的「授权注入」一节与 `httpd/authz_integrations_test.go`
（三条口子各一条用例 + 对照组，做过变异验证）。

## 飞书那两层都要判，且判据必须一致

`feishu.Service` 内层已有几处 `requireManage`（`onboarding.go` / `readiness.go` /
`apps.go`），它们保证「先鉴权再查会话」这条不变量（否则 sessionID 就成了事实凭证）。
Guard 在 HTTP 边界拦了之后**那几处仍然保留**，由 `authz.FeishuAuthorizer` 提供实现——
两层共用同一张角色表。

判据不一致会出现「Guard 放过、Service 又拒」，报错指向不了真实原因。所以
`CanAccessWorkspace` → `ActionRead`、`CanManageWorkspace` → `ActionManage`，
与矩阵里飞书那 8 条的档位对齐。

`feishu.Config.Authorizer` **留空一律拒绝**（`denyAllAuthorizer`）。早前那个
`AllowAllAuthorizer` 已经删了：兜底 allow 时「忘了装配」等于授权形同不存在且零报错，
兜底 deny 则第一次点就发现。授权要错在吵闹的那一边。

## 测试

`go test ./authz/` 覆盖矩阵、大小写、双向完整性、404 与那条例外、以及飞书适配器。

真正会出错的地方在缝上（Guard 装反位置、pattern 差一个字、反解用错参数名），
那些在单元测试里都是绿的——所以另有一层端到端：`httpd/authz_test.go` 用**真装配**
（真 core + 真 Guard + 种子数据）以四种角色 + 一个非成员打真实路由。

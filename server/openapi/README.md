# openapi —— 面向外部 agent 的自描述

**读它的时机**：加了 / 改了 HTTP 接口之后（**必须重新生成 spec，否则测试红**），
或者要改「给 agent 看的说明」之前。

## 这一层提供什么

两条**免鉴权**的接口，分工互补：

| 接口 | 讲什么 | 形式 |
| --- | --- | --- |
| `GET /api/openapi.json` | **怎么调**——路径、方法、参数、字段、权限档位 | OpenAPI 3.1 JSON |
| `GET /llms.txt` | **调之前要知道什么**——光看 spec 猜不出来的约定 | Markdown（[llmstxt.org](https://llmstxt.org/) 的惯例） |

两者都不是可选的。只给 spec，agent 会把 `requires_action` 当成「等用户确认」然后
卡在那里等一个永远不会来的输入；只给 llms.txt，它不知道字段叫什么。

## spec 从 matrix.go 生成，一条也不手写

**这是本包最重要的一条。** 路由部分全部来自 `authz.Routes()`，也就是
`authz/matrix.go` 的那张路由权限表。

理由不是省事，是**手写的 spec 必然漂移**：路由改了名、换了方法、多了一条，
没有任何机制提醒你去改 yaml。而 agent 拿着过时的 spec 会反复调错接口，
**且报错不指向真实原因**——它看到 404 / 403，真实原因却是「这条接口上个月改了路径」。

从 matrix 生成则永不漂移，因为那张表已经被启动时的双向校验钉死了
（见 [authz/README.md](../authz/README.md)）：

```
漏一条 → 那条路由注册时 panic（服务起不来）
多一条 → Verify() 返回错误（服务起不来）
```

也就是说「表 == 真实路由」这件事已经有人管了，spec 只要跟着表走就自动正确。

**顺带一个手写给不了的好东西**：matrix 里存着每条路由的权限档位，生成时会写进每条
接口的 description。agent 读到它才知道「我这把 key 能干什么」，而不是撞一堆 403
才试出来——403 只说「不行」，不说「你需要 admin」。

## 改了接口要跑这个

```bash
cd server && go run ./cmd/gen-openapi          # 重新生成，产物入库
cd server && go run ./cmd/gen-openapi -check   # 只检查是否最新（CI 用）
cd server && go generate ./openapi             # 同第一条
```

**忘了跑会怎样**：`go test ./openapi/` 直接红，报错里带修复命令。

## 「必须同步」是机器强制的，不靠自觉

`openapi_test.go` 里那几条串起来是一个闭环：

| 测试 | 钉住什么 | 失败时说明 |
| --- | --- | --- |
| `TestSpecCoversEveryDeclaredRoute` | spec 与 matrix 逐条一致（**两个方向都查**） | 多了 / 少了哪条 |
| `TestSpecFileIsUpToDate` | git 里那份 `openapi.json` 是最新的 | 直接给修复命令 |
| `TestSpecIsDeterministic` | 生成是确定性的（跑三遍字节相同） | 某处 map 没排序 |
| `TestLLMsTxtMentionsOnlyRealRoutes` | `/llms.txt` 提到的接口都真实存在 | 哪条路径不存在 |
| `TestEveryRouteHasPermissionInDescription` | 每条接口都标了权限档位 | 哪条漏了 |
| `TestEveryRouteHasOperationID` | operationId 存在且唯一 | 重复会让 client 生成器静默覆盖方法 |
| `TestEveryPathParamIsDocumented` | 每个 `{xxx}` 都在 parameters 里 | 漏了 agent 会把它当字面量拼进 URL |
| `TestEveryRefResolves` | 所有 `$ref` 都指向真实 schema | 悬空 ref 让 spec 不合法 |

整条链：

```
新增路由 → 必须写 matrix（漏了服务起不来）
        → spec 自动跟着变（本包从 authz.Routes() 生成）
        → 对账保证 spec 已重新生成并提交（TestSpecFileIsUpToDate）
```

**不依赖任何人记得。**

### 两条要注意的实现细节

**断言 spec 内容的测试读磁盘那份，不是现场生成那份。** 这个区别是变异验证发现的：
早先比的是 `Generate()` 的结果，于是「往 `openapi.json` 里手塞一条虚构路由」这个
变异**测不出来**——现场生成的当然不含手塞的东西，两边自然一致。而线上吐给 agent 的
正是磁盘（`go:embed`）那一份，所以断言对象必须是它。

**确定性是对账的前提。** 如果某处 map 没排序输出，每次生成的字节都不同，
`TestSpecFileIsUpToDate` 会永远失败，而失败信息说的是「spec 过期了」——完全不指向
真实原因。`orderedMap` 和 `sortedKeys` 就是为这件事存在的，
`TestSpecIsDeterministic` 让真实原因自己显形。

## 为什么 spec 落成文件入库，而不是请求期生成

它本可以在请求里现算。选择落成文件并入库，是为了让「接口契约变了」这件事
**在提交历史里看得见**：能 diff、能 code review。请求期生成的话，一条路由改了名，
没有任何痕迹。

代价是它可能过期——那个代价由上面那张表兜住。

## 为什么路由生成、实体 schema 手写

分工是刻意的：**凡是会漂移的东西都生成**（路径、方法、权限档位、反解规则），
**凡是机器猜不出来的东西才手写**（这条接口是干什么的、body 长什么样、字段的坑）。

实体 schema 不用反射 `domain` 的 struct tag，两个理由：

1. **反射表达不出那三条 JSON tag 铁律**（见 `model/apimodel` 包注释）——
   「这个键必须在且必须是 `[]`」与「这个键可以不在」的区别，恰恰是本仓库前端契约的核心。
2. **schema 要带中文说明才对 agent 有用**，而那些说明（「`requires_action` 不是等用户
   确认」这种）只有人能从 domain 的注释里搬过来。

代价是 domain 加字段时要手动同步。这个代价是**有界的**：漏了只是 spec 少描述一个字段，
不会让 agent 调错接口；而路由漂移会。两者严重程度差一个量级，所以只对后者上机器约束。

手写部分漏一条也不报错——那是刻意的。逼着「新增路由必须同时补文案」会让人在
写不出好文案时随便写一句凑数，而路由本身漏不掉（它来自 matrix）。

## 那两条免鉴权，安全边界在哪

**必须免鉴权**：agent 得先读懂这个平台才可能去认证，要求「认证之后才能知道怎么认证」
是个死锁（同 `/api/auth/` 那五条的理由）。它们注册在 **裸 mux** 上
（`httpd/server.go`），不经过 `protectedMux`。

⚠️ 因此**它们不能写进 `authz/matrix.go`**——那会触发「声明了但没注册」的反向 panic。
它们的声明放在 `openapi.go` 的 `allRoutes()` 里，是本包唯一一处「路由不来自 matrix」
的地方。

那么放开的是什么？**只有结构，没有任何内容**：

```
spec      字段名、类型、路径、权限档位 —— 来自 authz.Routes() 与手写的 schema
llms.txt  一篇散文 + 唯一一个可变量：调用方自己请求时用的 origin
```

⚠️ 那个可变量是 2026-09-01 加的（此前是一段纯常量）。它**不构成泄露**：origin 来自
请求的 Host 头，是调用方刚刚自己用过的地址。**但这条判据只对「调用方自己给的东西」
成立**——往里塞任何来自 Store 或配置的值，都要重新过一遍本节。

两个 handler **不带 Store、不认识任何空间、不读任何凭据**（`Handler` 是个空结构体，
这不是巧合）。三条测试钉住这件事，都在 `httpd/openapi_test.go`：

| 测试 | 钉住什么 |
| --- | --- |
| `TestPublicRoutesRegistered` | 不带凭据能访问（装配位置对） |
| `TestPublicRoutesExposeNoTenantData` | 响应里没有种子里的空间 id / key / 成员邮箱 |
| `TestPublicRoutesLeakNoCredentials` | 没有凭据形状的东西（含「把真 key 当例子写进文档」） |

**第二条必须用带种子的服务跑**（`newRBACServer`）：空 Store 的话「响应里不含 ws_demo」
会因为压根没有 ws_demo 而假通过，那时测试是绿的但什么都没验。所以那个文件里还有一个
元测试 `assertSeedDataReachable`，先确认靶子真的在库里。

**这条边界立刻就发挥了作用**：写文档时拿种子里真实的 `ws_demo` / `growth` 当示例，
测试当场变红。现在文档里用的是 `ws_example` / `my-team` 这种明显的占位符。

⚠️ 将来若想往 spec 里塞「这个部署实际启用了哪些 provider」之类的动态信息，
**那一刻就要重新评估这条边界**——那种信息是实例数据，会让免鉴权变成信息泄露。

## 向 MA（方舟 Managed Agents）的约定收敛

**AMA 可以理解成「一个固定 env 的 MA」**，两边经常被同一批人对着读。所以在**两边都有
的概念**上（agent、session↔run、skill、credential）用同一套表达，读的人不用切换心智。

MA 的 spec 不随本仓库提供（`openapi/<api>/openapi.yaml`，
从 TypeSpec 生成）。它的约定与我们采纳/不采纳的部分，逐条固化在
[ma_conventions.md](ma_conventions.md) 里，并由 `conventions_test.go` 对账。

### 「一毛一样」不可能字面成立，所以它指的是约定

两边的**路径集合本身就无法相等**：

| | 有什么 |
| --- | --- |
| AMA 有、MA 没有 | workspaces / members / issues / suggestions / feedback / api-keys / feishu / integrations / schedules / user-preferences / ark-fetch |
| MA 有、AMA 不暴露 | chat / embedding / responses / tokenization / images / content_generation / multimodal_embedding / file |
| 两边都有（真正该对齐的） | agent、session↔run、skill、environment、vault/credential、memory |

所以「一毛一样」= **在两边都有的概念上用 MA 的命名与形状**，加上 MA 那套
「生成 + 漂移守卫」的保鲜机制。不是把路径抄成一样——那既做不到也没有意义。

### 采纳了什么

| 约定 | 现在长什么样 |
| --- | --- |
| operationId 是 `Service_verb` | `Agents_retrieve` / `WorkspacesArkCredential_verify`（此前是 `getAgentsById` / `patchAgentsByIdArkCredentialRegion`） |
| verb 用 MA 的词表 | create / list / retrieve / update / remove，另加 `replace`（PUT）与非 CRUD 接口的动作名 |
| query 参数抽成 `$ref` | `#/components/parameters/Suggestions_listQuery.limit`，27 条 |
| 每个路径参数都有说明 | 含 id 格式（`ws_example` / `agent_example`）与「取值从哪来」 |
| 没有占位 summary | 此前 55 条 summary 是「方法 + 路径」，等于没写 |

⚠️ **改 operationId 是安全的，因为它只出现在 spec 里**——仓库里没有任何代码引用它
（前端 14 处调用写的是路径字面量，`cmd/ama-cli` 也是）。这次改动**不碰 wire format**。

⚠️ **写 summary 要去读 handler，不要照路径猜。** 补那 55 条时踩过一次：
`POST /api/workspaces/{workspaceId}/skills/fork` 被想当然写成「把别的空间的技能复制
一份过来」，而 `service/skill/market.go` 里明写着**跨空间 fork 是被拦掉的**
（`got.WorkspaceID != workspaceID` 直接回 404，注释说那等于一次静默的越权读取）。
一句写反的 summary 比没有 summary 更坏——**它看起来像是已经写过说明了**，
下一个人不会再去核。所以 `TestNoPlaceholderSummaries` 只能保证「写了」，
「写对」仍然只能靠读代码。

### 刻意保留的差异，以及为什么

**这一节是本次任务的主要产出。** 差异分两种：想清楚了才留下的，和没注意到才留下的
——**这两种在 spec 里长得一模一样**，而后者会被下一个人当成前者（「既然一直是这样，
想必是有原因的」）。所以每条都写下理由，且由 `TestDeliberateDivergencesAreDocumented`
钉住「基线文件里必须有交代」。

**1. 字段名保持 camelCase（MA 是 snake_case 全局统一）。**
本仓库 **9 张表**把整个实体序列化进 `doc jsonb`，所以 **`apimodel` 的 json tag 就是库里的
键名**（`repository/schema/agents.sql` 顶部那行注释：`Keys of the doc column (jsonb) (= json.Marshal(Agent))`）。
改名会**同时**坏三件事，且三件都零报错：

- **存量行读出空串**，而**全部测试照样绿**（测试写完就读，键是一致的）；
- **GIN 索引 `agents_feishu_idx`（挂在 `doc -> 'feishuAppIds'`）永不命中**——
  查询照样返回正确结果，只是全表扫，**慢而不错**；
- **存量数据迁移静默变成空操作**：`agents.sql` 里那批 `update ... set created_at = doc ->> 'createdAt'`
  会一行都匹配不到。

`doc->'键'` 这样的引用全仓约 120 处，散布在 DDL 索引、生产 SQL
（`store_pg.go` 的 `doc->>'lastActivityAt'` / `'trigger'` / `'status'`）与迁移里。
**所以重命名必须配一套数据迁移 + 索引重建 + 「缺键 ≠ 空串」的校验；拿不出方案就别改。**

**2. 保留 PATCH（15 条）与 PUT（7 条）。** MA 全仓**零 PATCH 零 PUT**，更新一律 POST。
不跟的理由：那是 wire format，改它要动全部前端调用方与 e2e；而 PATCH 本身还携带
**「部分更新」**这个信息，换成 POST 就表达不出来了。spec 里用 `update`（PATCH）与
`replace`（PUT）两个 verb 把这个区别保留下来。

**3. 时间戳保持固定三位小数的 ISO 8601（`domain.NowISO()`），不采纳 MA 那句「RFC 3339」。**
⚠️ **对齐命名（`created_at`）与对齐格式（RFC3339）是两件事，别一起做。**
Go 的 `time.RFC3339` / `RFC3339Nano` 都会把 `.000` 抹掉，于是同一时刻库里出现两种写法；
而 `listRuns` 是按 `startedAt` 做**字符串**降序排的，`.`(0x2E) < `Z`(0x5A)，
**两种形状混在一起会直接改变排序结果**。这一条 AMA 是对的，MA 的描述只是更宽松。

**4. 不给 spec 加 `type` 字段、不加 `ListXxxResponse` 包装。**
⚠️ 判据在 wire 上：列表接口返回的是**裸数组**（`apihandler/handler_agent.go` 的
`writeJSON(w, http.StatusOK, list)`），资源上也没有 `type` 判别字段。
**只在 spec 里补这两样，spec 就成了谎言**——agent 照着读会去取 `resp.data` 拿到
`undefined`，而那个错误不指向真实原因。**宁可保留一个诚实的差异，也不要一个好看的
假描述。** 要真对齐就得改 wire，那是破坏性路线。

**5. 方舟透传字段保持方舟的 snake_case 名。这是规则，不是待清理的不一致。**
AMA 里那几个 snake_case 的 json tag 全部是**方舟原样透传的字段**：
`session_thread_id`（`apimodel/events.go` 的注释明写「对应方舟的 session_thread_id」）、
`builtin_tools`、以及 `TokenUsage` 的 `input_tokens` / `output_tokens` /
`cache_read_input_tokens` / `cache_creation_input_tokens`（由方舟的 `model_usage` 累加）、
`agent_name`、`parent_thread_id`。**把它们改成 camelCase 才是引入 bug**——那会让透传
字段与上游对不上。

### 保鲜：为什么基线是一份文件，而不是实时拉 MA 的 spec

用户明确要「保持鲜活度的自动化措施」。这里要补的洞**不是**「spec 有没有重新生成」
——那个上面那张表已经管住了。真正缺的是两件：**(a) 没有任何东西检查约定本身**，
**(b) MA 改了约定这边无人知晓**。

(a) 由 `conventions_test.go` 补上（7 条，跟着 `go test ./...` 跑，CI 现成）。

(b) **做不到实时拉取**：MA 的 spec 不随本仓库提供，CI 里也不保证拉得到。

所以退化成「基线文件 + 对账测试」，代价写在 `ma_conventions.md` 里：MA 改了约定
这边不会自动变红，要人去重读 MA 的 spec 并更新基线。基线里记着快照日期，
据此能判断它有多旧。

⚠️ **这些检查刻意都不做「拿不到东西就 `t.Skip`」的降级。** 本仓库吃过那个亏：碰库的
四个包连不上库时整组 Skip，**而跳过时是绿的**，日志里没有任何东西提醒你它没跑。
本包的检查只读进程内的东西——磁盘上的 spec 与仓库里的基线文件，没有一条会因为环境不同而跳过。

## /llms.txt 写什么


**读者是 agent，判据只有一条：照着它能不能把接口调通。**

价值全在**反直觉**的事上。一条内容如果 agent 看着 spec 就能推出来，它就不该占篇幅；
凡是「不写下来就会被猜错、而猜错了不报错」的，都必须写进来。目前那几条：

1. **一次对话就是一条 `trigger: "manual"` 的 Run，消息是 Run 上的事件**——
   对话与运行历史是同一份数据的两种读法，不看文档绝对猜不到。
2. **`requires_action` 不是「等用户确认」**，而是「工具正在我们的容器里执行」。
   猜错的后果是 agent 卡在那里等一个永远不会来的输入。
3. **空间有 `id`（`ws_xxx`）和 `key`（短名）两个标识**，接口要 id，前端 URL 里是 key。
   搞混会得到 404 / 403，而报错不会告诉你是标识搞混了。
4. **Actor 就是员工邮箱**，不是 user_id 也不是工号。
5. **有一批「看起来是失败其实是正常」的响应**：列表永不 404、方舟凭证未配置回
   200 + 空壳、校验 key 无效也回 200（结论在 `check` 字段里）。
6. **`endedAt` / `durationMs` 在 Run 未结束时整个键不存在**，是缺失不是 0。

⚠️ **不要写实现细节**（我们用什么语言、内部包怎么分层、当初为什么这么取舍）。
那些 agent 用不上，占的却是它上下文里的真实位置。同理，**别用「我们没有 X」来解释
一件事**——那句话不告诉读者该做什么。写正面陈述（不是「没有 Conversation 实体」，
而是「一次对话就是一条 Run」）。

⚠️ **正文里不用 Markdown 加粗。** 曾经有 44 处 `**`，占正文相当大的比例——
通篇加粗等于没有重点，信息量与全不加粗相同，只多了噪音。强调改由句式承载：
自成一段、「注意：」、「常见误读：」、「判据：」。`TestLLMsTxtHasNoBoldMarkup`
钉住这件事，因为加粗很容易在后续编辑里一处一处偷偷长回来。
（本 README 不受这条约束——它的读者是人。）

三条测试钉住这份手写散文（它没有任何编译期约束）：

| 测试 | 钉住什么 |
| --- | --- |
| `TestLLMsTxtMentionsOnlyRealRoutes` | 提到的每条路径都真实存在 |
| `TestLLMsTxtSpecLinkFollowsOrigin` | spec 链接跟着请求的 origin，没写死域名 |
| `TestLLMsTxtHasNoBoldMarkup` | 没有 `**` |

## spec 链接为什么不写死域名

正文里那两处 OpenAPI 链接是**全路径**，host 来自请求自己的 Host 头
（`handler.go` 的 `requestOrigin`）。

写死 `https://ama.example.com` 会让**本地开发时那个链接指向线上**：agent 读的是
本机文档、调的却是生产接口，**而这件事零报错**——它拿到的响应完全正常，只是操作错了
环境。换域名、起预发环境同理。

也没用配置项（`server.publicURL`）：那要给 `Handler` 加一个依赖，而「它不带任何依赖」
正是这两条路由能免鉴权的论据。请求里已经有 Host 了。

⚠️ 协议判定用 `X-Forwarded-Proto`，**不能看 `r.TLS`**：线上 TLS 终止在网关，
到进程这里是明文 HTTP，看 `r.TLS` 会让线上那份文档吐 `http://`。
网关没带那个头时**默认 https**，只有本机地址才认 http。

⚠️ 响应带 `Vary: Host`：正文随 origin 变，缓存必须按 Host 分桶。漏了它，
中间缓存可能把本地那份（指向 localhost）发给线上调用方——**或者反过来**，
而反过来那半会让 agent 照着文档动生产数据。

## /llms.txt 不在 /api/ 下面

那是 llmstxt.org 的惯例（agent 按约定去站点根目录找）。代价是 `httpd` 的
`isBackendPath` 要**单独放行它**：漏了的话请求会落到前端 SPA，而那条路径带 `.txt`
扩展名，`spaHandler` 的「带扩展名就 404」规则会把它拦成 404——**而后端明明注册了
这条路由**。那个症状极难查（curl 报 404，代码里搜得到 handler）。

## 测试

```bash
cd server && go test ./openapi/ ./httpd/
```

分两组，钉的是**两个不同方向**的事：

| 文件 | 钉什么 |
| --- | --- |
| `openapi_test.go` | spec 与本仓库的**真实路由**一致（matrix → 生成 → 对账） |
| `conventions_test.go` | spec 的**表达**符合 MA 的约定（见上面那节） |

两者不互相覆盖：一份 spec 可以既与 matrix 完全一致、又写成一套谁也不认识的形状。

`httpd` 那几条是缝上的事（装配位置、不泄露数据）。
每条都做过变异验证：故意漏一条路由 / 故意让 spec 过期 / 故意往免鉴权响应里塞实例数据
或凭据 / 故意把 spec 链接写死成线上域名 / 故意加回一处 `**` / 故意漏替换占位符 /
故意把一条 operationId 改回旧形状 / 故意删掉基线文件，确认测试真的变红。
⚠️ **变异必须能编译才算数**——一个编译不过的变异只是编译错误，它不证明测试有效。


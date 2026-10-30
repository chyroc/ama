// The body of /llms.txt: the project description for agents to read.
//
// # How it divides the work with OpenAPI
//
//	openapi.json  how to call —— paths, methods, parameters, fields, permission levels
//	llms.txt      what to know before calling —— conventions that cannot be guessed from the spec alone
//
// So the whole value here is in the counterintuitive facts. If an agent could infer an item from the spec,
// it should not take space here; conversely, anything that "will be guessed wrong if not written down, and guessing wrong gives no error"
// must be written in.
//
// The reader is an agent, not a colleague. There is only one test: can it get the API calls working by following this.
// So this does not cover what language we wrote it in, how internal packages are layered, or why the trade-offs were made back then:
// agents cannot use those, and they take up real space in its context.
//
// The format follows the llmstxt.org convention: Markdown, an H1 title + introduction, sections.
//
// # The only variable is the origin the caller sees
//
// The two OpenAPI links in the body are full paths (`https://host/api/openapi.json`), with host taken from
// the request's own Host header; see requestOrigin in handler.go. There are no other parameters.
//
// Why `https://ama.example.com` is not hard-coded: during local development that link would point agents to production,
// so they would read the local docs but call production endpoints, with no error at all. The same goes for changing domains.
//
// Why not a config item (server.publicURL): that would give Handler a dependency, and
// "it has no dependencies at all" is exactly the argument for these two routes needing no auth (see the security boundary at the top of handler.go).
// The request's Host is the address the caller itself just used; returning it to the caller leaks nothing.
//
// # It still contains no instance data
//
// No workspace names, no agents, no credentials, only models and semantics. A template that "fills in some workspace's data"
// would turn this no-auth endpoint into an information leak, and that leak has no symptoms.
// A test pins this; see TestPublicRoutesExposeNoTenantData.

package openapi

import "strings"

// specURLPlaceholder is the placeholder of the OpenAPI document address in the body.
//
// On rendering it is replaced with origin + PathSpec. A placeholder is used instead of concatenating in the raw string,
// so the body still reads as one whole Markdown document.
//
// A missed replacement gives no error; it only shows agents a fake link starting with `{{`,
// so TestLLMsTxtLeavesNoPlaceholder asserts the rendered result no longer has `{{`.
const specURLPlaceholder = "{{specURL}}"

// LLMsTxt renders the body of /llms.txt for the origin the caller sees.
//
// origin looks like `https://ama.example.com` (no trailing slash). When empty the OpenAPI link in the body
// degrades to a same-origin relative path; that is the fallback for "Host not available", not the normal case.
//
// ⚠️ Every endpoint path mentioned in it must really exist: `TestLLMsTxtMentionsOnlyRealRoutes`
// checks them one by one against the matrix. Otherwise this document slowly turns into lies: a path gets renamed,
// the old one is still written here, and agents calling by it get 404.
func LLMsTxt(origin string) string {
	return strings.ReplaceAll(llmsTxtBody, specURLPlaceholder, origin+PathSpec)
}

// llmsTxtBody is the body template. The only placeholder is specURLPlaceholder.
//
// # No Markdown bold here
//
// `**` everywhere means no emphasis: a document with a third of its sentences in bold reads exactly like one with no bold at all,
// just with more noise. So emphasis is carried by sentence form:
//
//	a paragraph of its own       this sentence is the conclusion of the section
//	"注意：" (Note:)              calls will go wrong if you do not know this
//	"常见误读：" (Common misreading:)  the thing most people get wrong on first reaction
//	"判据：" (Test:)              how to confirm it yourself
//
// Before adding `**` back, first think through why it is better than another sentence form. TestLLMsTxtHasNoBoldMarkup pins this.
const llmsTxtBody = `# AMA Platform

一个 agent 平台：在一个空间（workspace）里配置 agent、给它挂技能、
跟它对话或让它定时跑，对话过程逐条留痕。

本文件讲的是「调这些接口之前要知道什么」。参数、字段、每条接口要什么权限，
看 [OpenAPI 文档](` + specURLPlaceholder + `)——那份由路由权限表自动生成，
不会与真实接口漂移。

## 三分钟跑通一条最短路径

从一把 token 到写下一条记录，只需要三个请求。

    # 1. 确认这把 token 有效。这条不需要任何业务权限，所以它只回答一件事：钥匙对不对
    GET /api/auth/me
    #    → {"user": {"email": "you@example.com", ...}}

    # 2. 列出我能进的空间。返回的就是我是成员的那些，不需要再过滤
    GET /api/workspaces
    #    → [{"id": "ws_example", "key": "my-team", "name": "...", ...}]

    # 3. 在其中一个空间里建一条问题记录（要 member 以上）
    POST /api/workspaces/ws_example/issues
    {"title": "线上偶发 500", "body": "复现步骤……", "priority": "p1"}
    #    → {"id": "iss_...", "status": "open", ...}

判据：第 1 步回 ` + "`unauthenticated`" + ` 就是钥匙不对，别拿业务接口试——
那会把「钥匙不对」和「这个空间没权限」两件事混在一起。

## 认证

凭据放 ` + "`Authorization: Bearer <token>`" + `。两种：

| 凭据 | 给谁用 | 怎么拿 |
| --- | --- | --- |
| API Key（` + "`amak_`" + ` 开头） | agent / 脚本 / CI | 在设置页创建 |
| 会话 Cookie | 浏览器 | 邮箱密码登录（` + "`POST /api/auth/login`" + `）或 OIDC 登录（` + "`GET /api/auth/oidc/login`" + `） |

API Key 的明文只在创建那一刻的响应里出现一次，之后再也读不到（库里只存它的
sha256）。丢了就吊销重建。

一把 key 继承创建者本人的权限：用它调接口，与那个人自己登录调完全等价。
所以它能进的空间就是那个人能进的空间，不是一套独立的作用域。

管理自己的 key：

    GET    /api/api-keys         列出我的 key（只有前几位，没有明文）
    POST   /api/api-keys         创建（明文只回这一次）
    DELETE /api/api-keys/{id}    吊销，立即生效

注意：key 是账号级的。空间维度那两条（` + "`/api/workspaces/{workspaceId}/api-keys`" + `）
只是把管理入口放在空间设置页，返回的仍是调用者本人的全部 key，换个空间打开是同一批。

SSE 有一个例外：` + "`GET /api/runs/{runId}/events/stream`" + ` 额外接受
` + "`?api_key=<key>`" + ` 查询参数，因为浏览器的 ` + "`EventSource`" + ` 不能带自定义请求头。
这个参数只认 API Key——查询参数会进访问日志与浏览器历史，会话 token 不该出现在那里。

## 授权

权限只看一件事：你在这个空间里的角色。用哪种凭据不影响判定。

| 角色 | 能做什么 |
| --- | --- |
| ` + "`owner`" + ` / ` + "`admin`" + ` | 全部，含空间设置、成员、凭证、外部应用接入 |
| ` + "`member`" + ` | 读 + 写：发消息、增删改 agent 与技能、改定时任务 |
| ` + "`viewer`" + ` | 只读 |
| 非成员 | 一律 403 |

OpenAPI 文档里每条接口都标了它要求的档位（` + "`none`" + ` / ` + "`read`" + ` /
` + "`write`" + ` / ` + "`manage`" + `）和最低角色。先看那个再调，能省掉一堆 403。

想知道自己在某个空间是什么角色，问 ` + "`GET /api/workspaces/{workspaceId}/my-role`" + `。

注意：这个角色要从上面那条接口拿，不要从空间详情的 ` + "`members`" + ` 数组里自己算。
判权规则只有服务端那一份，自己算迟早会与它不一致。

## 数据模型

空间是一切资源的归属边界：agent、技能、运行、问题记录都属于某一个空间，
没有跨空间的全局资源。

    空间 workspace
      ├─ 成员        角色决定权限
      ├─ agent       提示词 + 模型 + 绑定的技能
      ├─ 技能 skill  空间级资产，可被多个 agent 引用
      ├─ 运行 run    一次执行，里面是一串事件
      │    └─ 事件 RunEvent
      ├─ 定时任务    到点起一条运行
      └─ 问题 issue  含评论与附件

技能挂给 agent 是引用而不是拷贝：改了技能内容，绑它的 agent 下一轮立即用上新的。

### 一次对话就是一条 Run，消息是 Run 上的事件

一次对话是一条 ` + "`trigger: \"manual\"`" + ` 的 Run，里面每条消息（你发的和 agent 回的）
都是这条 Run 上的一个 RunEvent。对话与运行历史因此是同一份数据的两种读法。

| 你想做的事 | 实际要调的接口 |
| --- | --- |
| 开一轮新对话 | ` + "`POST /api/workspaces/{workspaceId}/conversations`" + ` → 返回一条 Run |
| 发一句话 | ` + "`POST /api/runs/{runId}/events`" + `（追加一个 ` + "`user.message`" + ` 事件） |
| 读 agent 的回复 | ` + "`GET /api/runs/{runId}/events`" + `，或订阅 ` + "`GET /api/runs/{runId}/events/stream`" + ` |
| 看历史对话 | ` + "`GET /api/workspaces/{workspaceId}/runs`" + ` |

### 空间有两个标识，接口一律用 id

| | 形状 | 用在哪 |
| --- | --- | --- |
| ` + "`id`" + ` | ` + "`ws_example`" + ` | 所有接口参数（路径里的 ` + "`{workspaceId}`" + `） |
| ` + "`key`" + ` | ` + "`my-team`" + `（短名） | 前端 URL（` + "`/w/my-team/chat`" + `） |

手上只有短名时先换 id：` + "`GET /api/workspace-by-key?key=my-team`" + `。

常见误读：把 key 直接填进 ` + "`{workspaceId}`" + `。结果是 404 或 403，
而报错不会告诉你是两个标识搞混了。

### 身份就是员工邮箱

空间成员、凭证的修改者、API Key 的归属，全都以员工邮箱标识人。比较时大小写不敏感。

### 方舟原始接口走 /ark/ 透传，字段与方舟逐字一致

除了 AMA 自己建模的对话接口，方舟 Managed Agents 的其余资源接口以原样透传的形式提供：
路径里带一段 ` + "`ark/`" + `，后面就是方舟 ` + "`/api/v3`" + ` 之后的路径（会话 id 由 runId
自动补上），请求体、query、响应体字节级原样转发，字段命名与分页包裹（` + "`data` / `next_page`" + `）
全部与方舟文档一致。拿方舟 Managed Agents API 文档对照这些路径即可。

| 你想做的事 | 接口（` + "`{runId}`" + ` 就是 AMA 注入的 ` + "`AMA_SESSION_ID`" + `） |
| --- | --- |
| 看这条会话挂了哪些文件 | ` + "`GET /api/runs/{runId}/ark/resources`" + ` |
| 给会话挂一个文件 | 先 ` + "`POST /api/runs/{runId}/ark/files`" + `（multipart，字段 ` + "`file`" + ` + ` + "`purpose=agent`" + `）拿 file_id，再 ` + "`POST .../ark/resources`" + ` |
| 看会话有哪些线程 / 线程事件 | ` + "`GET .../ark/threads`" + `、` + "`.../threads/{threadId}/events`" + ` |
| 热升级会话运行配置 | ` + "`POST .../ark/upgrade`" + ` |
| 管 Vault 保管库与凭证（写要 admin） | ` + "`/api/workspaces/{workspaceId}/ark/vaults...`" + ` |
| 管记忆库与记忆条目 | ` + "`/api/workspaces/{workspaceId}/ark/memory-stores...`" + ` |

两个要点：

- 不用传方舟 key：服务端按 run（会话所属空间/agent）或 workspace 反解该用哪把方舟 key。
- 方舟侧的错误回方舟形状：方舟返回 4xx/5xx 时是方舟自己的 ` + "`{\"error\":{...}}`" + ` 与方舟状态码，
  不是 AMA 的 ` + "`{message,code}`" + ` 信封；只有 run 不存在、空间没配 key、网络不通这类 AMA 自身问题才走信封。

## 一轮对话怎么走

    # 1. 开一轮，拿到一条 Run
    POST /api/workspaces/ws_example/conversations
    {"agentId": "agent_x"}
    #    → {"id": "run_y", "trigger": "manual", "status": "idle", ...}

    # 2. 发一句话
    POST /api/runs/run_y/events
    {"events": [{"type": "user.message",
                 "content": [{"type": "text", "text": "帮我看看当前目录"}]}]}

    # 3. 读回复。推荐 SSE，它还会推逐字增量帧
    GET  /api/runs/run_y/events/stream
    #    或轮询 GET /api/runs/run_y/events

发消息的请求发完立刻返回，agent 的回复是异步的。

SSE 里的逐字增量帧只用来渲染打字效果，它们不进事件历史。要拿一轮对话的完整内容，
读事件列表。

### status 是 requires_action 时，工具正在执行

这条 Run 的工具调用正在平台自己的容器里跑，通常几秒到几十秒。正确的反应是等。

常见误读：把它当成「等用户确认」。那会让你停下来等一个永远不会来的输入，
而 Run 其实一直在正常推进。

同理，一轮对话里 ` + "`session.status_idle`" + ` 出现好几次是正常的：每次工具调用都要在
running 与 idle 之间翻一个来回。

判断一轮到底结束没有，看 ` + "`stop_reason`" + `：它是 ` + "`requires_action`" + ` 时这一轮还在继续。

### 第一次调工具可能要多等十几秒

一轮对话结束后容器会空闲暂停，交还计算资源，同时整个工作区原样保留。
下一条消息真的调到工具那一刻才恢复，约 16 秒；纯文本回复不会白等这段时间。

空闲超过一小时容器被回收，之后自动重建，那时工作区回到初始状态（日志里会明说）。

### agent 还在跑时想再说一句，用待发队列

    POST /api/runs/{runId}/pending      排一条，等这一轮结束自动发出
    GET  /api/runs/{runId}/pending      看队列

直接往正在跑的 Run 里追加消息会撞上工具执行窗口。队列由服务端在轮次结束那一刻发出，
一次一条，顺序保持。

## 响应约定

这几条不知道就会把正常响应当成故障处理。

列表接口回的总是 ` + "`200`" + ` + 数组，空的时候是 ` + "`[]`" + `。挂在 Run 下面的那些
（events / pending）在 Run 压根不存在时也照此办理，不会 404。

注意：空间下面的列表不一样。空间 id 不存在时得到的是 ` + "`403`" + `
（` + "`access_denied`" + `）而不是空数组——判权先跑，而「不存在的空间」当然也不是你的空间。
所以拿到 403 不一定是权限配少了，也可能是那个 id 根本是错的。判据：
用 ` + "`GET /api/workspace-by-key`" + ` 或 ` + "`GET /api/workspaces`" + ` 核一下这个 id 真的存在。

取方舟凭证时，未配置的空间回 ` + "`200`" + ` + 空壳（` + "`apiKeyMasked`" + ` 是空串）。

校验凭证时，key 无效也是 ` + "`200`" + `，结论在 ` + "`check`" + ` 字段里
（` + "`valid`" + ` / ` + "`invalid`" + `）。把它当成 HTTP 错误会漏掉真正的结论。

## 错误

失败一律是这个信封：

    {"message": "一句中文", "code": "稳定标识"}

判断要看 ` + "`code`" + ` 不看状态码。路由写错时标准库返回的 404 是 text/plain 且没有
` + "`code`" + `，只按状态码判会把「路由挂错了」当成「这条资源不存在」。

常见 ` + "`code`" + `：` + "`unauthenticated`" + `（401）、` + "`access_denied`" + `（403）、
` + "`not_found`" + `（404）、` + "`invalid_argument`" + `（400）、
` + "`conflict`" + `（409）、` + "`precondition_failed`" + `（422）。

资源不存在时回 404 而不是 403，这是刻意的：把「已经被删了」说成「无权访问」
会让你去申请一个早就不存在的东西的权限。

## 字段约定

id 一律当成不透明字符串。本文档里的 ` + "`ws_example`" + ` / ` + "`agent_x`" + ` /
` + "`run_y`" + ` 都是占位符，不是真实形状——真实的 Run id 长得像
` + "`sesn-20260101000000-abcde`" + `。按前缀猜类型、或者拿正则校验 id，
会在某一类 id 上静默失配。原样回传就对了。

时间戳是固定三位小数的 ISO 8601 串（` + "`2026-08-25T10:00:00.000Z`" + `）。

` + "`endedAt`" + ` 与 ` + "`durationMs`" + ` 只在 Run 结束后出现。未结束时这两个键整个不存在，
读它们要按「键可能缺失」处理，不是按 0 处理。

` + "`TokenUsage`" + ` 的四个字段总是存在，` + "`0`" + ` 就是真的 0。

` + "`Agent.version`" + ` 是数字（乐观锁，更新时必须回传当前值），
而 ` + "`Skill.latestVersion`" + ` 是字符串。两者形状相反，容易写反。

` + "`PATCH`" + ` 按全量更新处理：改一个字段也要回传完整对象，缺的字段会被当成「清空」。

运行列表按 ` + "`lastActivityAt`" + ` 降序，不是按 ` + "`startedAt`" + `。飞书会话会被复用，
按开始时间排的话刚说完话的会话反而躺在列表底部。

一个飞书应用最多绑一个 agent（一条入站消息只能有一个回答者）。抢占别人已绑的
应用会得到 409。

## 全部接口

见 [` + specURLPlaceholder + `](` + specURLPlaceholder + `)。

那份文档由路由权限表自动生成，所以它列出的路由与线上真实注册的路由必然一致：
少一条会让服务启动失败，多一条会让启动校验报错。它不会漂移，可以照着调。
`

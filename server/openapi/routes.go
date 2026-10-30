// The "human-written part" of routes: groups, a one-line description per endpoint, parameters and request/response bodies.
//
// # Hand-written here, generated in openapi.go
//
// The split is deliberate: **everything that can drift is generated** (paths, methods, permission levels, resolution rules),
// **only what machines cannot guess is hand-written** (what this endpoint does, what the body looks like).
//
// Missing one in the hand-written part has a bounded cost: that endpoint's summary becomes the fallback line,
// permissions and paths stay accurate, and agents can still call it. So there is **no** "error if missing" constraint here:
// that would make "add a route" require writing text at the same time, and when no good text comes to mind people write a filler sentence.
//
// Conversely, missing a route itself is impossible: it comes from the matrix, and if the matrix misses one the service will not start.

package openapi

import (
	"strings"

	"github.com/chyroc/ama/server/authz"
	"github.com/chyroc/ama/server/core"
)

// Endpoint group names. Constants instead of literals: tagsArray and each route's tagFor must reference the same word,
// and one misspelled letter would add an empty group to the spec, with no error.
const (
	tagWorkspace     = "空间"
	tagMember        = "成员"
	tagAgent         = "Agent"
	tagSkill         = "技能"
	tagArkCredential = "方舟凭证"
	tagIssue         = "问题"
	tagSuggestion    = "产品建议"
	tagAPIKey        = "API Key"
	tagIntegration   = "授权管理"
	tagHost          = "宿主机"
	tagUserFile      = "文件"
	tagConversation  = "对话"
	tagRun           = "运行历史"
	tagSchedule      = "定时任务"
	tagUsage         = "用量"
	tagFeishu        = "飞书"
	tagMeta          = "自描述"
	tagDebug         = "开发用"
	tagArkProxy      = "方舟透传"
)

// tagFor decides by path which group a route belongs to.
//
// Order matters: `/api/workspaces/{id}/skills` contains both workspaces and skills,
// and must go to the "skills" group rather than "workspaces", so **more specific sub-paths are checked first**.
func tagFor(r authz.RouteDecl) string {
	p := r.Path
	// The Ark passthrough /ark/ segment must be checked first: otherwise /api/runs/{id}/ark/... would fall into
	// "run history", and /api/workspaces/{id}/ark/... into some other specific group.
	if strings.Contains(p, "/ark/") {
		return tagArkProxy
	}
	switch {
	case p == PathSpec || p == PathLLMsTxt:
		return tagMeta
	case strings.Contains(p, "/_debug/"):
		return tagDebug
	case strings.Contains(p, "/feishu"):
		return tagFeishu
	case strings.Contains(p, "/members"):
		return tagMember
	case strings.Contains(p, "/agents"):
		return tagAgent
	case strings.Contains(p, "/skills"):
		return tagSkill
	// All three issue-domain paths go here: /issues, /issue-comments, /issue-attachments.
	//
	// ⚠️ Check the **prefix fragment** "/issue" rather than "/issues": the latter does not match
	// /api/issue-comments and /api/issue-attachments, and those two groups would fall into default
	// and become the "workspaces" group; no error, just wrong grouping in the spec, and agents cannot find them.
	case strings.Contains(p, "/issue"):
		return tagIssue
	// The three product suggestion paths: /suggestions, /suggestion-replies, /suggestion-attachments.
	//
	// ⚠️ Check the prefix fragment "/suggestion" rather than "/suggestions", for exactly the same reason as the issue one above
	// (the latter does not match the -replies and -attachments groups).
	case strings.Contains(p, "/suggestion"):
		return tagSuggestion
	// ⚠️ Check "/ark-" rather than "/ark-credential": the latter does not match /api/ark-fetch/*
	// (the three that fetch automatically from the Volcano account), and they would fall into default and become the "workspaces" group;
	// no error, just wrong grouping in the spec, and agents looking for endpoints by group cannot find them.
	case strings.Contains(p, "/ark-credential"), strings.HasPrefix(p, "/api/ark-fetch"):
		return tagArkCredential
	case strings.Contains(p, "/api-keys"):
		return tagAPIKey
	case strings.HasPrefix(p, "/api/integrations"):
		return tagIntegration
	case strings.Contains(p, "/conversations"):
		return tagConversation
	case strings.Contains(p, "/schedules"):
		return tagSchedule
	case strings.Contains(p, "/usage"):
		return tagUsage
	case strings.Contains(p, "/runs"):
		return tagRun
	case strings.Contains(p, "/hosts") || strings.Contains(p, "/host-daemon"):
		return tagHost
	case strings.Contains(p, "/files"):
		return tagUserFile
	default:
		return tagWorkspace
	}
}

// routeDocs is the "what it does" paragraph of each endpoint, with keys exactly the same as the matrix patterns.
//
// The content first covers **what cannot be guessed from the path**: defaults, side effects, differences from another endpoint,
// and behaviors that "look odd but must be so". Purely repeating the path ("list workspaces")
// is worse than writing nothing; the summary already says it.
// arkProxyCommon is the shared description of the Ark passthrough endpoints (/ark/...): shape, credential resolution, error shape.
// The field tables are deliberately not repeated in this file: the Ark Managed Agents docs are the single source of truth, avoiding drift between the two.
const arkProxyCommon = "这是方舟 Managed Agents 接口的**原样透传**：`ark/` 之后就是方舟 " +
	"`/api/v3` 之后的路径（会话 id 由 runId 自动补上，不用你传），请求体、query、响应体 " +
	"**字节级原样转发**，字段名与方舟文档完全一致（蛇形命名、`data` / `next_page` 包裹、" +
	"RFC3339 时间）。拿方舟 Managed Agents API 文档对照即可，这里不重复字段表。\n\n" +
	"**鉴权与凭据**：仍走 AMA 的登录与空间权限；方舟 API Key 由服务端按 run（会话所属 " +
	"空间/agent 的三级凭证）或 workspace（空间级凭证，未配则回落该空间引用的全局凭证）" +
	"反解，请求里不要也不能带方舟 key。\n\n" +
	"**错误形状**：方舟侧的 4xx/5xx 回的是**方舟自己的错误 JSON**（`{\"error\":{...}}`）" +
	"与方舟状态码，不是 AMA 的 `{message,code}`；只有 AMA 自身的问题（run 不存在、" +
	"空间没配 key、网络不通）才走 AMA 信封。"

var routeDocs = map[string]string{
	"GET /api/workspaces": "列出**当前调用者所在的**空间。不是全部空间——" +
		"非成员的空间不会出现在这里，所以这条接口天然不需要额外过滤。",
	"POST /api/workspaces": "建一个空间。**创建者自动成为 owner**。" +
		"`key` 是 URL 标识，全局唯一，`[a-z0-9-]{2,32}`。",
	"GET /api/workspace-by-key": "按 `key`（短名，出现在 URL 里）查空间。\n\n" +
		"⚠️ **非成员会得到 404 而不是 403**，这是刻意的：前端据此跳回空间列表，" +
		"顺带也不泄露「这个 key 存在但你进不去」。",
	"GET /api/workspace-key-taken": "查一个 `key` 有没有被占用。\n\n" +
		"**这条是全局查重，不按成员过滤**——否则你会建出一个 key 已被别人占用的空间，" +
		"然后在保存时撞唯一约束。",
	"PATCH /api/workspaces/{workspaceId}": "改空间的名字 / key / 描述。\n\n" +
		"⚠️ **改 key 等于改所有链接**。`description` 传空或不传表示清空。",
	"DELETE /api/workspaces/{workspaceId}": "删空间。**级联删掉里面的 Agent、技能、运行历史与方舟凭证**，不可恢复。",

	"POST /api/workspaces/{workspaceId}/members":              "加一个成员。`email` 就是对方的身份（员工邮箱）。",
	"PATCH /api/workspaces/{workspaceId}/members/{memberId}":  "改成员角色。**不能把唯一的 owner 降级**。",
	"DELETE /api/workspaces/{workspaceId}/members/{memberId}": "移除成员。**不能移除唯一的 owner**。",

	"GET /api/workspaces/{workspaceId}/agents":  "列出空间里的 Agent。",
	"POST /api/workspaces/{workspaceId}/agents": "建一个 Agent。",
	"GET /api/agents/{id}":                      "取一个 Agent 的完整定义。",
	"PATCH /api/agents/{id}": "改一个 Agent。**前端总是回传完整对象**，所以这里按全量更新处理。\n\n" +
		"⚠️ 一个飞书应用**最多只能绑一个 Agent**（一条入站消息只能有一个回答者），" +
		"抢占别人已绑的应用会得到 409。",
	"DELETE /api/agents/{id}": "删一个 Agent。**已有的会话不会被删**，只是被标记为 `agentDeleted` 并冻结（不能再发消息）。",

	"GET /api/workspaces/{workspaceId}/skills":  "列出空间里的技能。**响应里不含附件内容**（`files[].content` 缺失），只有文件名与大小。",
	"POST /api/workspaces/{workspaceId}/skills": "手填一个技能。`name` 必须是 `[a-z0-9-]` 且首字符非连字符。",
	"POST /api/workspaces/{workspaceId}/skills/upload": "上传 `SKILL.md` 或技能 zip 包（`multipart/form-data`，字段名 `file`）。\n\n" +
		"建出来的东西与手填技能**完全等价**（同一张表、同一个 `source: custom`），" +
		"区别只是带附件。所以档位是 `write` 而不是 `manage`。",
	"GET /api/skills/{id}": "取一个技能。",
	"GET /api/skills/{id}/file": "读一个附件的内容（`?path=` 指定哪个）。\n\n" +
		"**`SKILL.md` 不是附件**：技能正文在 `instructions` 字段里，`files` 只有除它以外的文件。\n\n" +
		"二进制文件不给内容（`binary: true`、`content` 为空串）——判据是含 NUL 字节。\n\n" +
		"⚠️ 响应里的 `content` 是**明文**，不是 base64（与 `Skill.files[].content` 相反，那个是 `[]byte`）。",
	"PATCH /api/skills/{id}": "改一个技能。**技能是被引用而不是被拷贝的**——改了内容，所有绑定它的 Agent 立即生效。\n\n" +
		"⚠️ **内置技能不可改**。要在它基础上改，先用 " +
		"`POST /api/workspaces/{workspaceId}/skills/fork` 复制一份（副本是 `custom`，可以随意改）。",
	"PUT /api/skills/{id}/file": "改一个附件的内容（`path` 与 `content` 都在 body 里）。\n\n" +
		"**只能改已经存在的附件**，不能新建 / 删除 / 重命名——路径不在这个技能里回 404。\n\n" +
		"⚠️ **`SKILL.md` 会被拒（400）**：技能正文是 `instructions` 字段，改它走 " +
		"`PATCH /api/skills/{id}`。放过去会在库里留下一份与 `instructions` 竞争的第二真源。\n\n" +
		"⚠️ **内置技能不可改**。要在它基础上改先 Fork 一份。\n\n" +
		"`content` 是**明文**不是 base64。单文件上限 " + core.HumanSkillFileLimit +
		"、该技能附件总量上限 " + core.HumanSkillTotalLimit + "。\n\n" +
		"改动会让技能的内容指纹变化，所以下次开对话会把新版本重传给方舟——" +
		"但**已经在跑的容器里那份不会更新**（技能文件是建容器时注入的）。",
	"DELETE /api/skills/{id}": "删一个技能。绑定它的 Agent 会自动解绑。",

	"GET /api/workspaces/{workspaceId}/issues": "列出这个空间的问题，**最新的在前**。\n\n" +
		"可用 `?status=` 筛（`open` / `in_progress` / `closed`）。**永远是数组**，空的时候是 `[]`。\n\n" +
		"⚠️ 这不是 `/api/feedback`。那条是错误发生时机器自动上报的流（跨空间、会去重、" +
		"只有开发团队能读）；这里是人手写的问题记录（有标题正文、能讨论、有状态流转）。",
	"POST /api/workspaces/{workspaceId}/issues": "提一个问题。\n\n" +
		"**`title` 与 `body` 至少要有一个**，两个都空会被拒（400）。" +
		"只填正文时标题会自动从第一行提取——Markdown 标记会被剥掉、图片取 `alt`、" +
		"超过 80 字截断，实在提不出来才用「未命名问题」兜底。\n\n" +
		"不填 `status` 落 `open`，不填 `priority` 落 **`p2`**（不是 `p0`）。" +
		"创建者取自登录身份，**请求体里没有 `author` 字段**。",
	"GET /api/issues/{id}": "取一条问题（含评论数与附件数）。",
	"PATCH /api/issues/{id}": "改一条问题：标题、正文、状态、优先级都能改，服务端会刷新 `updatedAt`。\n\n" +
		"⚠️ 两种「空」的含义不同：`title`/`body` 传空串是**真的清空**" +
		"（清空标题会重新从正文提取，两者都空则拒）；" +
		"`status`/`priority` 传空串表示**这一项不改**。",
	"DELETE /api/issues/{id}": "删一条问题。**它的评论与附件一并删掉**（附件字节也删）。",

	"GET /api/issues/{id}/comments": "列出一条问题下的评论，**按发表顺序正序**" +
		"（与问题列表的倒序相反——讨论要从上往下读）。\n\n" +
		"问题不存在时回 `200 + []`，不是 404（「列表接口永不 404」）。",
	"POST /api/issues/{id}/comments": "发一条评论。作者取自登录身份，空正文会被拒。",
	"PATCH /api/issue-comments/{id}": "改一条评论，服务端刷新它的 `updatedAt`" +
		"（前端据此显示「已编辑」）。",
	"DELETE /api/issue-comments/{id}": "删一条评论。",

	"GET /api/issues/{id}/attachments": "列出一条问题的附件**元信息**。\n\n" +
		"**不含字节**：要内容去打每条记录的 `url`。",
	"POST /api/issues/{id}/attachments": "上传附件（`multipart/form-data`，字段名 `file`）。\n\n" +
		"**类型由服务端嗅探判定，不信你声明的 Content-Type**：内容与扩展名对不上一律拒" +
		"（那是「HTML 伪装成 png」的形状）。允许图片、pdf、纯文本与 zip，**svg 不收**。\n\n" +
		"单文件 5 MiB，单个问题最多 20 个。返回体里的 `url` 可以直接写进正文的 Markdown 图片语法。",

	"GET /api/issue-attachments/{id}": "取附件的**原始字节**。这是唯一一条不回 JSON 的业务接口。\n\n" +
		"**能直接塞进 `<img src>`**：浏览器发子资源请求不带自定义头，但同源会自动带上会话 Cookie。\n\n" +
		"⚠️ **它不是公开的。** 尽管看起来像静态资源，访问仍然要求你是该附件所属空间的成员" +
		"——拿到别的空间的 URL 会得到 403。\n\n" +
		"响应固定带三个安全头：真实的 `Content-Type`（嗅探结果）、`X-Content-Type-Options: nosniff`、" +
		"以及 `Content-Disposition`（**只有位图是 `inline`，其余一律 `attachment`**）。",
	"DELETE /api/issue-attachments/{id}": "删一个附件。正文里引用它的图片会变成裂图，删之前想清楚。",

	// -- Product suggestions (platform level) --------------------------------------------------
	//
	// ⚠️ The text of this group must say again and again "it is not an issue, nor feedback": the mistake agents holding
	// the spec most easily make when looking for endpoints is treating the three as the same thing, and writing data into the wrong table.
	"GET /api/suggestions": "列产品建议——用户对 **AMA 平台本身**提的意见。\n\n" +
		"⚠️ **这是第三种「有问题」的东西，别与另外两个搞混**：\n\n" +
		"- `/api/feedback` —— 机器采集的**报错流**（前端 catch 自动落库，用户没打字）\n" +
		"- `/api/workspaces/{id}/issues` —— **空间级**的待办，属于某一个空间\n" +
		"- 本接口 —— 人手写的产品意见，**平台级，没有空间维度**（那张表连 workspace_id 列都没有）\n\n" +
		"**返回集合由你的身份决定**：普通用户只拿得到自己提的，平台方拿得到全部。" +
		"响应里的 `viewerIsStaff` 告诉你走的是哪一支——**别自己猜**，" +
		"平台方名单是服务端配置（feedback.readers），调用方看不到。\n\n" +
		"⚠️ `author` 这个参数**只对平台方有效**：普通用户传了会被服务端无条件覆盖成自己。" +
		"这不是 bug，是这条路径的授权实现——采纳它等于让任何人读任何人的建议。",
	"POST /api/suggestions": "提一条建议。**任何登录用户都能提**，不需要任何空间权限" +
		"（还没加入任何空间的新人也能提——那恰恰是最有话要说的一批人）。\n\n" +
		"`title` 可以不填，那时服务端会从 `body` 第一个有内容的行提取一个" +
		"（剥掉 Markdown 标记、图片取 alt、按字符截断到 80）。" +
		"**两者都空会被拒**（400）——那是一条什么都没有的记录。\n\n" +
		"⚠️ 请求体里**没有 author 字段**：提建议的人从登录身份取，收下它等于允许冒名。" +
		"`status` 不填落 `open`。",
	"GET /api/suggestions/{id}": "看一条建议。**只有提的人本人与平台方读得到**，" +
		"别人拿到 id 也是 403（不是 404——「不存在」与「不是你的」是两件事，混在一起会让" +
		"用户在自己那条被删掉时以为是权限问题）。",
	"PATCH /api/suggestions/{id}": "改一条建议。\n\n" +
		"**两种人能改的东西不一样**：作者本人只能改标题与正文，**改不了状态**；" +
		"平台方两者都能改。状态表达的是「平台方处理到哪一步了」，" +
		"让提的人自己点「已解决」会让这个字段失去意义。\n\n" +
		"⚠️ **收的是完整 draft，不是增量**：`title` 与 `body` 传空串是**真的清空**。" +
		"但**两者同时为空**时是「这次不动它们」——那是为了让 `{\"status\":\"resolved\"}` " +
		"这种只改状态的请求走得通（同 PATCH /api/issues/{id} 的规则）。\n\n" +
		"`status` 只认 `open` 与 `resolved`，**认不出一律 400，绝不默默改成 open**。",
	"DELETE /api/suggestions/{id}": "删一条建议，**回复与附件一起删**。作者本人或平台方。",

	"GET /api/suggestions/{id}/replies": "列一条建议下的回复，**正序**（对话从上往下读）。\n\n" +
		"每条回复带 `isStaff`，表示它是不是平台方写的。那个值是**写入时定下来落库的**，" +
		"不是读的时候拿 author 现比白名单——否则有人调岗移出名单后，" +
		"他当年那条官方答复会追溯性地变成用户留言。",
	"POST /api/suggestions/{id}/replies": "回一条。**作者与平台方都能回**，这就是「支持对话」" +
		"（平台方答复、用户追问走同一条路径）。\n\n" +
		"⚠️ 请求体里只有 `body`：`author` 从登录身份取，`isStaff` 由服务端按名单判——" +
		"收下后者等于让任何人把自己标成官方答复。",
	"PATCH /api/suggestion-replies/{id}": "改一条回复。**只有作者本人能改自己那条**。\n\n" +
		"⚠️ 平台方在这里**没有**特权，与「平台方能改建议状态」刻意相反：" +
		"改状态是处理流程的一部分，改别人写下的话是篡改发言。要纠正只能再回一条。",
	"DELETE /api/suggestion-replies/{id}": "删一条回复。作者本人**或平台方**。\n\n" +
		"平台方在这里**有**特权（与改不同）：删是管理动作（清垃圾内容），改是篡改发言。",

	"GET /api/suggestions/{id}/attachments": "列附件的**元信息**（不含字节）。" +
		"要内容去打每条的 `url`。",
	"POST /api/suggestions/{id}/attachments": "上传一个附件，`multipart/form-data`，字段名 `file`。\n\n" +
		"类型由**服务端嗅探**决定（不信客户端声明的那个），且要与扩展名对得上。" +
		"白名单：png / jpeg / gif / webp / pdf / txt / md / log / csv / json / zip。" +
		"**svg 明确不收**（它能内嵌脚本，跑在我们的同源里）。单文件 5 MiB，单条建议 20 个。",

	"GET /api/suggestion-attachments/{id}": "取附件的**原始字节**。不回 JSON。\n\n" +
		"**能直接塞进 `<img src>`**：浏览器发子资源请求不带自定义头，但同源会自动带上会话 Cookie。\n\n" +
		"⚠️ **它不是公开的。** 尽管看起来像静态资源，读权限与所属建议一致" +
		"（提的人本人 + 平台方）——拿到别人的 URL 会得到 403。\n\n" +
		"⚠️ 与 `/api/issue-attachments/{id}` 的**鉴权机制不同**：那条靠 authz 按空间反解，" +
		"这条没有空间可反解，鉴权在 Service 层回查所属建议。行为一样，实现不一样。\n\n" +
		"响应固定带三个安全头：嗅探出的真实 `Content-Type`、`X-Content-Type-Options: nosniff`、" +
		"以及 `Content-Disposition`（图片 inline，其余一律 attachment）。",
	"DELETE /api/suggestion-attachments/{id}": "删一个附件。正文里引用它的图片会变成裂图。",

	"GET /api/workspaces/{workspaceId}/ark-credential": "取空间的方舟凭证。**只回掩码，永远拿不到明文**。\n\n" +
		"⚠️ 未配置时返回 **200 + 空壳**（`apiKeyMasked` 为空串），不是 404。",
	"PUT /api/workspaces/{workspaceId}/ark-credential": "存方舟凭证（明文 key 只在这个请求里出现一次）。\n\n" +
		"**档位是 `manage` 而不是 `write`**：它是一把空间级的付费凭证，换掉会影响这个空间所有 Agent 的对话。",
	"DELETE /api/workspaces/{workspaceId}/ark-credential":       "删掉方舟凭证。删完这个空间的 Agent 就跑不了对话了。",
	"PATCH /api/workspaces/{workspaceId}/ark-credential/region": "只改区域，不动 key。会把上次的校验结果清掉。",
	"POST /api/workspaces/{workspaceId}/ark-credential/verify": "拿已存的 key 打一次方舟的只读接口，回写校验结果。\n\n" +
		"**key 无效不是 HTTP 错误**：仍然回 200，结果在 `check` 字段里（`valid` / `invalid`）。",

	"GET /api/agents/{id}/ark-credential": "取这个 Agent 的方舟凭证，**并告诉你当前实际生效的是哪一级**。\n\n" +
		"三级配置的回落顺序是 **Agent → 空间 → 账号（全局）**，越具体越优先。\n\n" +
		"`custom: false` 表示这个 Agent 没有自己的 key、跟随空间设置；" +
		"`effective` 里是实际生效那份的摘要（`level` 为 `agent` / `workspace` / `global`），" +
		"`level` 为 `global` 时 `ownerEmail` 说明那是谁的账号级凭证。\n\n" +
		"⚠️ 未配置时回 **200 + 空壳**，不是 404。",
	"PUT /api/agents/{id}/ark-credential": "给这个 Agent 贴一把**自己的** key（此后它不再跟随空间）。\n\n" +
		"key 二选一：手填 `apiKey`，或用 `fetchRef` 指向火山账号里已有的一把。\n\n" +
		"**档位是 `manage` 而不是 `write`**：改 Agent 的提示词/模型/技能都是 `write`，" +
		"但那些不涉及钱。换 key 改变的是「这个 Agent 的对话账记在谁头上」。",
	"DELETE /api/agents/{id}/ark-credential": "让这个 Agent **改回跟随空间设置**。\n\n" +
		"⚠️ 它不是「删掉一把凭证」——Agent 级没有「继承」这个存储态，**没有记录就是跟随**。" +
		"删完之后解析会自然回落到空间级（空间自己可能又引用了账号级，继续回落）。",
	"PATCH /api/agents/{id}/ark-credential/region": "只改这个 Agent 那把 key 的区域，不动 key。会把上次的校验结果清掉。\n\n" +
		"⚠️ **跟随空间的 Agent 会被拒绝**：区域是那把 key 的属性，而它用的是上一级那份。" +
		"允许改的话会写出一条「有区域没有 key」的记录，而解析仍然回落——用户改了却毫无效果。",
	"POST /api/agents/{id}/ark-credential/verify": "拿这个 Agent **实际生效**的 key 打一次方舟的只读接口。\n\n" +
		"跟随空间时校验结果写回**空间那一行**（不在 Agent 侧留副本，否则改上级就不联动了）。\n\n" +
		"**key 无效不是 HTTP 错误**：仍然回 200，结果在 `check` 字段里。",

	"PUT /api/agents/{id}/integrations": "改「这个 Agent 允许注入哪些授权源的凭据」。\n\n" +
		"收 `{\"integrationIds\": [...]}`，**整份覆盖**（不是增量）。空数组是合法值，" +
		"含义是「一个都不注入」。\n\n" +
		"**档位是 `manage` 而不是 `write`**，理由比方舟凭证那条更重：注进容器的是" +
		"**代表一个真人**的凭据（空间 owner 的 person token），Agent 一句 `env` 就能读到，" +
		"于是它能做的事等于那个员工能做的事。\n\n" +
		"⚠️ **为什么单独一条路由**：`PATCH /api/agents/{id}` 整体必须留在 `write`" +
		"（改名字/提示词/模型是 member 的日常），而权限是按路由判的、判不了字段。" +
		"所以要收紧的字段得先变成路由。\n\n" +
		"⚠️ 旧口子同时堵上了：`PATCH /api/agents/{id}` 带一个**与现状不同**的 " +
		"`integrationIds` 会回 403（相同则放行——前端总是回传完整草稿）；" +
		"`POST .../agents` 建 Agent 时带非空 `integrationIds` 也要 `manage`。",

	"GET /api/agents/{id}/sync-task": "查这个 Agent 最近一次「同步到方舟」的结果。\n\n" +
		"保存 Agent（POST / PATCH）会**立刻排一个异步任务**去追平方舟侧的 " +
		"system / model / 工具 / 技能，前端轮询这条接口拿结果。\n\n" +
		"⚠️ **没有同步记录时回 200 + `{\"status\":\"none\"}`，不是 404**：" +
		"存量 Agent 从没保存过、或记录已过保留期被清掉，那都是正常状态，" +
		"回 404 会让前端把它渲染成错误。\n\n" +
		"`status` 取 `none` / `pending` / `running` / `succeeded` / `failed`。" +
		"失败时 `failureReason` 是可直接展示的中文句子，`failureStage` 说明卡在哪一步" +
		"（`credential` / `environment` / `agent` / `skills` / `spec`），前端据此给不同引导。\n\n" +
		"**轮询要有终止条件**：`succeeded` / `failed` 都是终态，之后不会再变。",

	"POST /api/agents/{id}/sync-task": "重新排一次「同步到方舟」，对应失败提示旁边的「重试」。\n\n" +
		"已经有一条没跑完的任务时**不会新建**，而是复用那一条（连点两次只跑一次，" +
		"由数据库的部分唯一索引裁决）。\n\n" +
		"⚠️ **档位是 `write` 而不是 `read`**：它真的会去改方舟侧的 Agent，viewer 不该能触发。",

	"GET /api/workspaces/{workspaceId}/my-role": "查**我自己**在这个空间是什么角色。\n\n" +
		"回 `{role, canManage}`。`canManage` 由服务端算（owner 与 admin 都为 true），" +
		"不要在客户端比字符串——那份判断与服务端不一致时没有任何报错。\n\n" +
		"⚠️ **别从空间详情的 `members` 数组里自己算**：" +
		"这条接口走的是判权用的同一个函数，自己算迟早会与它不一致。",

	"GET /api/ark-fetch/status": "查「从火山账号自动获取方舟 API Key」这个功能可不可用、" +
		"以及**我自己**授权了没有。\n\n" +
		"三种状态要分开看，因为要做的事完全不同：`available:false` → 用不了，手动填；" +
		"`available:true` + `authorized:false` → 拿 `authorizeUrl` 去点一次授权（600 秒有效，别缓存）；" +
		"两者都为 true → 可以直接列账号。",
	"GET /api/ark-fetch/accounts": "列出**我自己**名下已授权的火山账号，供选一个。\n\n" +
		"一个人常常有多个（实测有 10 个），所以必须让用户选——猜一个的话，" +
		"猜错时表现是「列出来的 key 不是我要的那些」，而他无从判断为什么。",
	"GET /api/ark-fetch/accounts/{accountId}/keys": "列出某个火山账号下的方舟 API Key。\n\n" +
		"⚠️ **只回掩码与 id，没有明文**。用户选定之后走 `PUT .../ark-credential` 或 " +
		"`PUT /api/global-ark-credential` 并带上 `fetchRef: {accountId, keyId}`，" +
		"由服务端现取明文——**明文不经过前端**。\n\n" +
		"⚠️ 路径上的 `accountId` 是**火山账号 id**，不是本系统的资源 id。\n\n" +
		"火山没有「创建 API Key」的接口，所以这里是「列出已有的让用户选」——" +
		"顺带避开了「每点一次就建一把、堆一堆废 key」。",

	"GET /api/integrations":                       "列出全部授权源及**我自己**在每个源上的授权状态。",
	"GET /api/integrations/{provider}":            "查我在某个源上的授权状态。",
	"POST /api/integrations/{provider}/authorize": "发起一次授权（**给我自己**）。",
	"DELETE /api/integrations/{provider}":         "解除我自己在某个源上的授权。",
	"GET /api/integrations/{provider}/apps": "列出这个授权源可以选哪些「应用」，以及各自授权了没、当前用的是哪个。\n\n" +
		"**只有 `lark-user` 有这个概念**（一个人可能注册了好几个飞书应用），" +
		"其余源回 400。\n\n" +
		"⚠️ **响应里没有任何凭据**：只有 app_id、应用名、所属空间名与两个布尔。",
	"PUT /api/integrations/{provider}/app": "换一个应用（把默认使用的那个设成 `appId`），返回换完之后的完整授权状态。\n\n" +
		"⚠️ **换应用不等于换授权**：新应用可能还没授权过，那时返回的状态是 `error` + " +
		"`canAuthorize: true`——token 与飞书应用是绑定的，换了就得对新的再授权一次。\n\n" +
		"⚠️ 这只改**账号级默认值**（卡片展示、发起/解除授权用）。" +
		"agent 跑起来时注入哪个应用**按运行所属空间定**，不看这个值。",

	"GET /api/user-onboarding": "查**我自己**有没有看过新手引导。\n\n" +
		"没有记录时回 200 + `{\"version\": 0}`，**不是 404**——「还没看过」是正常状态，不是资源不存在。",
	"POST /api/user-onboarding": "记下**我自己**看完（`action: \"complete\"`）或跳过（`\"skip\"`）了引导。\n\n" +
		"`version` 是引导内容的版本号，原样存下；以后内容改版时据它判断「这个人看过的是哪一版」。\n\n" +
		"重复调用是幂等的（upsert）。两个时间戳各自保留，所以「先跳过、后来又看完」两个时刻都在。",

	"POST /api/feedback": "上报一条问题反馈。**任何登录用户都能写。**\n\n" +
		"分两级，区别在**采集内容的敏感度**而不只是谁触发：\n\n" +
		"- `level: \"auto\"` —— 前端在错误发生时自动上报，用户没点任何东西。" +
		"因为没有用户的同意，服务端**只保留不敏感的元信息**：路由模式（id 已抹掉）、" +
		"HTTP 状态码、错误码、错误分类、浏览器概要、前端版本、那句错误文案。\n" +
		"- `level: \"user\"` —— 用户点了「反馈这个问题」，那一下就是他的同意，" +
		"所以还会保留完整路径（含 id）、当前 agent、最近几次接口调用、最近几条控制台错误。\n\n" +
		"⚠️ **服务端按白名单重建 payload，不存请求体原文。** 本 schema 没列的字段" +
		"（token / cookie / Authorization / 接口响应体 / 用户消息正文）发过来也会被丢掉，" +
		"而且值里嵌着的凭据形状（JWT、`amak_` key、`?token=`、`Bearer xxx`）会被抹成 `[已抹除]`。\n\n" +
		"⚠️ **一级有限流**：同一个人 + 同一条路由 + 同一个错误码 + 同一个状态码，" +
		"在同一个十分钟窗口内**只落一条**，重复上报累加 `occurrences` 而不新增行。" +
		"所以刷 100 次得到的是一条 `occurrences: 100` 的记录，不是 100 条。" +
		"二级刻意不去重——用户点两次就该有两条。\n\n" +
		"回 **204 无响应体**：上报没有什么可回的，也刻意不回记录 id（调用方对它没有后续操作）。",

	"GET /api/feedback": "列出问题反馈，**只有开发团队能读**。\n\n" +
		"⚠️ **不在名单里一律 403 `access_denied`**，即便你是某个空间的 owner。" +
		"判据是服务端配置里的邮箱白名单（`feedback.readers`），" +
		"**不是空间角色**——反馈是跨空间的，没有空间可判。名单为空时谁都读不到。\n\n" +
		"用 API Key 调也一样：key 换出来的身份就是创建者的邮箱，所以开发团队成员" +
		"用自己的 key 能读，别人拿任何 key 都读不了。\n\n" +
		"支持按 `level` / `errorCode` / 时间窗过滤与分页。时间窗**左闭右开** `[since, until)`" +
		"——闭区间会让边界那条同时落进相邻两页，按时间翻页时重复处理。\n\n" +
		"响应带 `total`：靠「这页够不够 `limit` 条」判断有没有下一页，在最后一页恰好满时是错的。",

	"GET /api/workspaces/{workspaceId}/runs": "列出运行历史。\n\n" +
		"⚠️ **按 `lastActivityAt` 降序**，不是 `startedAt`：飞书按作用域复用会话，" +
		"一个群的第 N 轮仍然追加进第一轮那条 Run；只按 startedAt 排的话刚说完话的会话还躺在列表底部。",
	"GET /api/workspaces/{workspaceId}/usage": "token 用量聚合（按天分桶）。窗口**左闭右开** `[since, until)`。",
	"GET /api/runs/{id}":                      "取一条 Run 的详情（含线程、用量、事件数）。",
	"GET /api/runs/{runId}/events": "取一条 Run 的全部事件，按 `sequence` 升序。\n\n" +
		"**这是读对话内容的地方**——用户的话与 agent 的回复都是事件。事件有 33 类，" +
		"结构直接来自方舟，本接口原样透传。",

	"POST /api/workspaces/{workspaceId}/conversations": "开一轮新对话。\n\n" +
		"**返回的是一条 Run**（`trigger: \"manual\"`），不是什么 Conversation 对象——" +
		"对话与运行历史是同一份数据的两种读法。拿到 `id` 之后用它发消息与订阅事件。",
	"POST /api/runs/{runId}/events": "往一条 Run 里追加事件，也就是**发一条消息**。\n\n" +
		"发完立刻返回，agent 的回复是异步的——去 `GET .../events/stream` 或轮询 " +
		"`GET .../events` 拿结果。",
	"GET /api/runs/{runId}/events/stream": "订阅一条 Run 的事件流（**SSE**，`text/event-stream`）。\n\n" +
		"这是看 agent 实时回复的推荐方式。除了落库的事件，还会推 `event_deltas` 逐字增量帧——" +
		"**那种帧不进事件历史**，方舟明确它不落库，所以别拿它当数据源，只用来渲染打字效果。\n\n" +
		"非浏览器客户端不能带自定义头时，可以用 `?api_key=<key>` 传凭据。",

	"GET /api/workspaces/{workspaceId}/schedules":  "列出定时任务。",
	"POST /api/workspaces/{workspaceId}/schedules": "建一条定时任务。三种触发方式：`cron` / `interval` / `oneshot`。",
	"GET /api/schedules/{id}":                      "取一条定时任务。",
	"PATCH /api/schedules/{id}":                    "改一条定时任务。",
	"PATCH /api/schedules/{id}/status": "启用 / 暂停一条定时任务。\n\n" +
		"单独一条接口而不是并进 `PATCH {id}`：列表页那个开关是一次独立操作，手上并没有完整草稿。",
	"DELETE /api/schedules/{id}":       "删一条定时任务。",
	"POST /api/schedules/{id}/run-now": "立刻跑一次，不等到点。**不影响下次触发时刻**。",

	"GET /api/workspaces/{workspaceId}/feishu/apps":                      "列出空间里已接入的飞书应用。",
	"GET /api/workspaces/{workspaceId}/feishu/chats":                     "列出机器人见过的飞书群（入站消息经过时顺手记下的）。",
	"POST /api/workspaces/{workspaceId}/feishu/registrations":            "发起一次飞书应用自助注册（设备码流程）。",
	"GET /api/workspaces/{workspaceId}/feishu/registrations/{sessionId}": "轮询注册进度。",
	"GET /api/feishu/apps/{id}":                                          "取一个飞书应用（含权限巡检结果）。",
	"POST /api/feishu/apps/{id}/readiness/refresh":                       "重跑一次权限与配置巡检。",
	"POST /api/feishu/apps/{id}/admin-authorization":                     "标记管理员已授权。",
	"DELETE /api/feishu/apps/{id}":                                       "解除飞书应用接入。",

	"GET /api/workspaces/{workspaceId}/api-keys": "列出**我自己**在这个空间页面里创建的 API Key。\n\n" +
		"⚠️ **key 是账号级的，不是空间级的**：这里只是管理入口放在空间设置页，" +
		"返回的是当前调用者自己的全部 key。换个空间打开会看到同一批。\n\n" +
		"**永远不返回明文**，只有 `prefix`（前几位，用于分辨）。",
	"POST /api/workspaces/{workspaceId}/api-keys": "创建一把 API Key。\n\n" +
		"⚠️ **明文只在这个响应里出现一次，之后再也拿不到**（库里只存 sha256）。" +
		"丢了只能吊销重建。\n\n" +
		"这把 key **继承你本人的权限**：它能访问的空间、能做的操作，与你自己登录时完全一样。" +
		"它不是一套独立的作用域。",
	"POST /api/admin/users": "平台管理员代建一个本地账号（邮箱 + 密码）。**不会登录成那个人**，只回它的身份。\n\n" +
		"只有平台管理员能调（配置里的 `auth.admins`，或带管理员标记的本地账号——第一个注册的账号就是），" +
		"其他人回 403 `access_denied`。没启用本地账号回 404 `local_disabled`，邮箱已存在回 409 `email_taken`，" +
		"密码不在 8~72 字节回 400 `weak_password`。",
	"DELETE /api/api-keys/{id}": "吊销一把 API Key，立即生效。\n\n" +
		"**只能吊销自己的**：别人的 key 会返回 404（不是 403——不泄露它的存在性）。",

	"GET " + PathSpec: "本文档自己（OpenAPI 3.1 JSON）。**免鉴权**。\n\n" +
		"由路由权限表自动生成，所以它不会与真实注册的路由漂移。",
	"GET " + PathLLMsTxt: "给 agent 读的项目说明（Markdown，见 llmstxt.org 的惯例）。**免鉴权**。\n\n" +
		"讲的是「调这些接口之前要知道什么」——那些光看 OpenAPI 猜不出来的约定。",

	"POST /api/_debug/reset": "把数据重置成种子数据。**只在 DevRoutes 开时存在**。",
	"POST /api/_debug/clear": "清空全部数据。**只在 DevRoutes 开时存在**。",

	// -- Passthrough of the raw Ark Managed Agents API (/ark/...); field tables are in the Ark docs ----
	"POST /api/runs/{runId}/ark/upgrade": "**升级会话**（方舟 `POST /sessions/{id}/upgrades`）：" +
		"在保持 session id 与历史事件不变的前提下热改 Agent / Environment 运行配置" +
		"（换模型、切 Agent 版本、改 system/tools/skills、改沙箱依赖/环境变量）。" +
		"升级期间会话状态为 `upgrading`，要轮询会话状态回到 `idle`。请求体与方舟逐字一致。\n\n" +
		arkProxyCommon,
	"GET /api/runs/{runId}/ark/resources": "**列出会话资源**（方舟 `GET /sessions/{id}/resources`）：" +
		"已挂载的 file / memory_store / tos 资源。不分页，全量返回（方舟上限 1000 条）。\n\n" +
		arkProxyCommon,
	"POST /api/runs/{runId}/ark/resources": "**添加会话资源**（方舟 `POST /sessions/{id}/resources`）：" +
		"把一个文件挂到会话沙箱里。当前方舟只支持 `type` 为 file，需要先用 `POST .../ark/files` " +
		"拿到 `file_id`，`mount_path` 可省略（默认挂到 `/mnt/session/uploads/`）。\n\n" +
		arkProxyCommon,
	"GET /api/runs/{runId}/ark/resources/{resourceId}": "**查询单个会话资源**" +
		"（方舟 `GET /sessions/{id}/resources/{resourceId}`）。\n\n" + arkProxyCommon,
	"GET /api/runs/{runId}/ark/threads": "**列出会话线程**（方舟 `GET /sessions/{id}/threads`）：" +
		"多 Agent 协作时每个子 Agent 一条线程。query 支持 `limit`（默认/上限 1000）与 `page`。\n\n" +
		arkProxyCommon,
	"GET /api/runs/{runId}/ark/threads/{threadId}": "**查询线程详情**" +
		"（方舟 `GET /sessions/{id}/threads/{threadId}`），threadId 形如 `sthr-...`。\n\n" +
		arkProxyCommon,
	"GET /api/runs/{runId}/ark/threads/{threadId}/events": "**查询线程事件列表**" +
		"（方舟 `GET /sessions/{id}/threads/{threadId}/events`）：按时间正序回放该线程事件，" +
		"query 支持 `created_at[gt/gte/lt/lte]`、`limit`（默认 50、上限 200）、`order`、" +
		"`page`、`types`。\n\n" + arkProxyCommon,
	"GET /api/runs/{runId}/ark/threads/{threadId}/stream": "**流式订阅线程事件**" +
		"（方舟 SSE `GET /sessions/{id}/threads/{threadId}/stream`）：`text/event-stream`，" +
		"每帧是一个事件 JSON。线程 idle 也不关流。\n\n" + arkProxyCommon,
	"GET /api/runs/{runId}/ark/files": "**列出文件**（方舟 Files API `GET /files`）：" +
		"文件属于该会话所在的火山账号（按 run 反解 key，保证同账号）。\n\n" + arkProxyCommon,
	"POST /api/runs/{runId}/ark/files": "**上传文件**（方舟 Files API `POST /files`）：" +
		"`multipart/form-data`，文件字段名 **`file`**，另带文本字段 `purpose`（挂给 agent " +
		"用 `agent`）。响应里的 `id` 就是后续添加会话资源 / 在消息里引用用的 `file_id`。" +
		"请求体上限 64 MiB。\n\n" + arkProxyCommon,
	"GET /api/runs/{runId}/ark/files/{fileId}": "**查询文件元信息**（方舟 `GET /files/{fileId}`）。\n\n" +
		arkProxyCommon,
	"DELETE /api/runs/{runId}/ark/files/{fileId}": "**删除文件**（方舟 `DELETE /files/{fileId}`）。\n\n" +
		arkProxyCommon,
	"GET /api/workspaces/{workspaceId}/ark/vaults": "**列出保管库 Vault**（方舟 `GET /vaults`），" +
		"响应 `data` + `next_page` + `total`。\n\n" + arkProxyCommon,
	"POST /api/workspaces/{workspaceId}/ark/vaults": "**创建保管库**（方舟 `POST /vaults`）：" +
		"body 为 `display_name` 与 `metadata` 两个字段。\n\n" + arkProxyCommon,
	"GET /api/workspaces/{workspaceId}/ark/vaults/{vaultId}": "**查询单个保管库**" +
		"（方舟 `GET /vaults/{vaultId}`）。\n\n" + arkProxyCommon,
	"POST /api/workspaces/{workspaceId}/ark/vaults/{vaultId}": "**更新保管库**" +
		"（方舟更新一律 POST：`POST /vaults/{vaultId}`），`display_name` / `metadata` 可选。\n\n" +
		arkProxyCommon,
	"DELETE /api/workspaces/{workspaceId}/ark/vaults/{vaultId}": "**删除保管库**" +
		"（方舟 `DELETE /vaults/{vaultId}`）：**级联永久删除里面的全部凭证**，不可恢复。\n\n" +
		arkProxyCommon,
	"GET /api/workspaces/{workspaceId}/ark/vaults/{vaultId}/credentials": "**列出保管库凭证**" +
		"（方舟 `GET /vaults/{vaultId}/credentials`）。响应不含任何密文（token 全是 write-only）。\n\n" +
		arkProxyCommon,
	"POST /api/workspaces/{workspaceId}/ark/vaults/{vaultId}/credentials": "**创建凭证**" +
		"（方舟 `POST /vaults/{vaultId}/credentials`）：`auth.type` 取 `mcp_oauth` 或 " +
		"`static_bearer`，字段与方舟一致（密文只写不读）。\n\n" + arkProxyCommon,
	"GET /api/workspaces/{workspaceId}/ark/vaults/{vaultId}/credentials/{credentialId}": "**查询单个凭证**" +
		"（方舟 `GET /vaults/{vaultId}/credentials/{credentialId}`），不含密文。\n\n" + arkProxyCommon,
	"POST /api/workspaces/{workspaceId}/ark/vaults/{vaultId}/credentials/{credentialId}": "**更新凭证**" +
		"（方舟更新一律 POST：`POST /vaults/{vaultId}/credentials/{credentialId}`）；" +
		"`mcp_server_url` / `secret_name` 创建后不可改。\n\n" + arkProxyCommon,
	"DELETE /api/workspaces/{workspaceId}/ark/vaults/{vaultId}/credentials/{credentialId}": "**删除凭证**" +
		"（方舟 `DELETE /vaults/{vaultId}/credentials/{credentialId}`）。\n\n" + arkProxyCommon,
	"GET /api/workspaces/{workspaceId}/ark/memory-stores": "**列出记忆库**" +
		"（方舟 `GET /memory_stores`）：支持 `created_by` / `name` 模糊过滤，`data` + `next_page`。\n\n" +
		arkProxyCommon,
	"POST /api/workspaces/{workspaceId}/ark/memory-stores": "**创建记忆库**" +
		"（方舟 `POST /memory_stores`）：字段为 name / description / metadata，" +
		"每账号上限 100 个。\n\n" + arkProxyCommon,
	"GET /api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}": "**查询单个记忆库**" +
		"（方舟 `GET /memory_stores/{id}`，含 memory_count / session_count / storage_bytes）。\n\n" +
		arkProxyCommon,
	"POST /api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}": "**更新记忆库**" +
		"（方舟更新一律 POST：`POST /memory_stores/{id}`）；`metadata` 为**整体覆盖**。\n\n" +
		arkProxyCommon,
	"DELETE /api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}": "**删除记忆库**" +
		"（方舟 `DELETE /memory_stores/{id}`）：连同全部记忆条目与内部版本删除。\n\n" +
		arkProxyCommon,
	"GET /api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}/memories": "**列出记忆条目**" +
		"（方舟 `GET /memory_stores/{id}/memories`）：以 `path` 为层次化键；" +
		"支持 `path_prefix` / `depth` / `order_by` / `limit` / `page`；列表项不含正文，" +
		"另有 `memory_prefix` 折叠目录项与 `prefixes`。\n\n" + arkProxyCommon,
	"POST /api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}/memories": "**写一条记忆**" +
		"（方舟 `POST /memory_stores/{id}/memories`）：字段为 path 与 content，" +
		"content ≤100KB，同 path 已存在方舟返回 409（要覆盖走批量接口的 `on_conflict:overwrite`）。\n\n" +
		arkProxyCommon,
	"POST /api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}/memories/batch-create": "**批量写记忆**" +
		"（方舟路径是 `memories/batch_create`）：items 数组每项含 path 与 content，" +
		"另带 on_conflict（overwrite / fail），≤100 条；响应是 `results` 数组（不是 `data`），" +
		"每条带 memory 或 error。\n\n" + arkProxyCommon,
	"GET /api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}/memories/{memoryId}": "**查询一条记忆**" +
		"（方舟 `GET /memory_stores/{id}/memories/{memoryId}`）：**含 content 正文**（列表不含）。\n\n" +
		arkProxyCommon,
	"POST /api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}/memories/{memoryId}": "**更新一条记忆**" +
		"（方舟更新一律 POST：`POST /memory_stores/{id}/memories/{memoryId}`）：可改 `content` / `path`，" +
		"last-write-wins。\n\n" + arkProxyCommon,
	"DELETE /api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}/memories/{memoryId}": "**删除一条记忆**" +
		"（方舟 `DELETE /memory_stores/{id}/memories/{memoryId}`）。\n\n" + arkProxyCommon,
}

// summaryFor is a one-line short title. Routes without one get a fallback built from method + resource.
func summaryFor(r authz.RouteDecl) string {
	if s, ok := routeSummaries[r.Method+" "+r.Path]; ok {
		return s
	}
	return r.Method + " " + r.Path
}

var routeSummaries = map[string]string{
	"GET /api/hosts":                                      "列出我注册的宿主机",
	"DELETE /api/hosts/{hostId}":                          "吊销一台宿主机",
	"GET /api/workspaces/{workspaceId}/hosts":             "列出空间已绑定的宿主机",
	"POST /api/workspaces/{workspaceId}/hosts":            "把宿主机绑进空间",
	"DELETE /api/workspaces/{workspaceId}/hosts/{hostId}": "把宿主机移出空间",
	"POST /api/host-daemon/register":                      "注册本机为宿主机",

	// Account-level personal files (object storage upload/download).
	"POST /api/files":                        "发起文件上传",
	"GET /api/files":                         "列出我的文件",
	"GET /api/files/{id}":                    "查看文件详情",
	"GET /api/files/{id}/content":            "下载文件内容（经 AMA 中转）",
	"DELETE /api/files/{id}":                 "删除文件",
	"PUT /api/files/{id}/content":            "上传文件内容",
	"PUT /api/files/{id}/parts/{partNumber}": "上传一个分片",
	"POST /api/files/{id}/complete":          "完成分片上传",
	"POST /api/files/{id}/abort":             "取消文件上传",
	"GET /api/workspaces":                    "列出我的空间",
	"POST /api/workspaces":                   "建空间",
	"GET /api/workspace-by-key":              "按 key 查空间",
	"GET /api/workspace-key-taken":           "查 key 是否被占用",
	"PATCH /api/workspaces/{workspaceId}":    "改空间",
	"DELETE /api/workspaces/{workspaceId}":   "删空间",

	"POST /api/workspaces/{workspaceId}/members":              "加成员",
	"PATCH /api/workspaces/{workspaceId}/members/{memberId}":  "改成员角色",
	"DELETE /api/workspaces/{workspaceId}/members/{memberId}": "移除成员",

	"GET /api/workspaces/{workspaceId}/agents":              "列出 Agent",
	"POST /api/workspaces/{workspaceId}/agents":             "建 Agent",
	"GET /api/agents/{id}":                                  "取 Agent",
	"PATCH /api/agents/{id}":                                "改 Agent",
	"DELETE /api/agents/{id}":                               "删 Agent",
	"PUT /api/agents/{id}/integrations":                     "改 Agent 的授权注入（需管理员）",
	"GET /api/agents/{id}/sync-task":                        "查 Agent 同步到方舟的结果",
	"POST /api/agents/{id}/sync-task":                       "重新同步 Agent 到方舟",
	"GET /api/agents/{id}/ark-console":                      "查 Agent 在方舟控制台的位置",
	"GET /api/agents/{id}/revisions":                        "列 Agent 历史版本",
	"GET /api/agents/{id}/revisions/{revisionId}":           "看 Agent 某个历史版本",
	"POST /api/agents/{id}/revisions/{revisionId}/rollback": "回滚 Agent 到某个版本",

	"GET /api/workspaces/{workspaceId}/my-role": "查我在本空间的角色",

	"GET /api/workspaces/{workspaceId}/skills":              "列出技能",
	"POST /api/workspaces/{workspaceId}/skills":             "建技能",
	"POST /api/workspaces/{workspaceId}/skills/upload":      "上传技能包",
	"POST /api/workspaces/{workspaceId}/skills/fork":        "在本空间复制一份技能副本",
	"GET /api/skills/{id}":                                  "取技能",
	"GET /api/skills/{id}/file":                             "读技能附件",
	"PUT /api/skills/{id}/file":                             "改技能附件",
	"PATCH /api/skills/{id}":                                "改技能",
	"DELETE /api/skills/{id}":                               "删技能",
	"GET /api/skills/{id}/ark-console":                      "查技能在方舟控制台的位置",
	"GET /api/skills/{id}/revisions":                        "列技能历史版本",
	"GET /api/skills/{id}/revisions/{revisionId}":           "看技能某个历史版本",
	"POST /api/skills/{id}/revisions/{revisionId}/rollback": "回滚技能到某个版本",

	"GET /api/workspaces/{workspaceId}/issues":  "列问题",
	"POST /api/workspaces/{workspaceId}/issues": "提问题",
	"GET /api/issues/{id}":                      "取问题",
	"PATCH /api/issues/{id}":                    "改问题",
	"DELETE /api/issues/{id}":                   "删问题",
	"GET /api/issues/{id}/comments":             "列评论",
	"POST /api/issues/{id}/comments":            "发评论",
	"PATCH /api/issue-comments/{id}":            "改评论",
	"DELETE /api/issue-comments/{id}":           "删评论",
	"GET /api/issues/{id}/attachments":          "列附件",
	"POST /api/issues/{id}/attachments":         "传附件",
	"GET /api/issue-attachments/{id}":           "下载附件",
	"DELETE /api/issue-attachments/{id}":        "删附件",

	"GET /api/suggestions":                    "列产品建议",
	"POST /api/suggestions":                   "提一条建议",
	"GET /api/suggestions/{id}":               "看一条建议",
	"PATCH /api/suggestions/{id}":             "改建议（状态只有平台方能改）",
	"DELETE /api/suggestions/{id}":            "删建议",
	"GET /api/suggestions/{id}/replies":       "列回复",
	"POST /api/suggestions/{id}/replies":      "回一条",
	"PATCH /api/suggestion-replies/{id}":      "改自己的回复",
	"DELETE /api/suggestion-replies/{id}":     "删回复",
	"GET /api/suggestions/{id}/attachments":   "列建议附件",
	"POST /api/suggestions/{id}/attachments":  "上传建议附件",
	"GET /api/suggestion-attachments/{id}":    "下载建议附件",
	"DELETE /api/suggestion-attachments/{id}": "删建议附件",

	"GET /api/workspaces/{workspaceId}/ark-credential":          "取方舟凭证（掩码）",
	"PUT /api/workspaces/{workspaceId}/ark-credential":          "存方舟凭证",
	"DELETE /api/workspaces/{workspaceId}/ark-credential":       "删方舟凭证",
	"PATCH /api/workspaces/{workspaceId}/ark-credential/region": "改方舟区域",
	"POST /api/workspaces/{workspaceId}/ark-credential/verify":  "校验方舟凭证",

	"GET /api/agents/{id}/ark-credential":          "取 Agent 级方舟凭证（掩码 + 生效来源）",
	"PUT /api/agents/{id}/ark-credential":          "给 Agent 配独立方舟凭证",
	"DELETE /api/agents/{id}/ark-credential":       "改回跟随空间设置",
	"PATCH /api/agents/{id}/ark-credential/region": "改 Agent 级方舟区域",
	"POST /api/agents/{id}/ark-credential/verify":  "校验 Agent 生效的方舟凭证",

	"GET /api/ark-fetch/status":                    "查自动获取是否可用",
	"GET /api/ark-fetch/accounts":                  "列我的火山账号",
	"GET /api/ark-fetch/accounts/{accountId}/keys": "列该账号下的方舟 Key（掩码）",

	// The platform-level fallback credential (used when no workspace credential is configured), not the same as the workspace-level group above.
	"GET /api/global-ark-credential":                               "取平台兜底方舟凭证（掩码）",
	"PUT /api/global-ark-credential":                               "存平台兜底方舟凭证",
	"DELETE /api/global-ark-credential":                            "删平台兜底方舟凭证",
	"POST /api/global-ark-credential/verify":                       "校验平台兜底方舟凭证",
	"POST /api/workspaces/{workspaceId}/ark-credential/use-global": "让本空间改用平台兜底凭证",

	"GET /api/integrations":                          "列出授权状态",
	"GET /api/integrations/{provider}":               "查单个授权状态",
	"POST /api/integrations/{provider}/authorize":    "发起授权",
	"GET /api/integrations/{provider}/authorization": "轮询这次授权好了没",
	"DELETE /api/integrations/{provider}":            "解除授权",
	"GET /api/integrations/{provider}/apps":          "列可选的应用",
	"PUT /api/integrations/{provider}/app":           "换一个应用",
	// Manually entered credentials (the AK/SK kind), a different path from the OAuth authorization above.
	// Workspace-level authorization binding: whose credentials this workspace uses.

	"GET /api/user-onboarding":  "查新手引导状态",
	"POST /api/user-onboarding": "标记引导已看完或已跳过",

	"GET /api/user-preferences": "取我的偏好设置",
	"PUT /api/user-preferences": "存我的偏好设置",
	// Batch-fetch "email → name/avatar", for list pages to display people.
	"POST /api/user-profiles": "按邮箱批量查用户资料",

	"POST /api/feedback": "上报问题反馈",
	"GET /api/feedback":  "列出问题反馈（仅开发团队）",

	"GET /api/workspaces/{workspaceId}/runs":  "列出运行历史",
	"GET /api/workspaces/{workspaceId}/usage": "取用量聚合",
	"GET /api/runs/{id}":                      "取运行详情",
	"GET /api/runs/{runId}/events":            "取运行事件",
	"GET /api/runs/{runId}/ark-console":       "查这次运行在方舟控制台的位置",

	// The server-side queue of pending messages (queued while the agent is busy).
	"GET /api/runs/{runId}/pending":                "列排队中的待发消息",
	"POST /api/runs/{runId}/pending":               "把一条消息排进队列",
	"DELETE /api/runs/{runId}/pending/{messageId}": "撤掉一条排队消息",

	// Session containers (Docker): read-only viewing, plus two execution entries that need an admin.
	"GET /api/runs/{runId}/sandbox":                  "查容器状态",
	"POST /api/runs/{runId}/sandbox/resume":          "恢复已暂停的容器",
	"GET /api/runs/{runId}/sandbox/files":            "列容器里的文件",
	"GET /api/runs/{runId}/sandbox/file":             "看容器里一个文件的内容",
	"POST /api/runs/{runId}/sandbox/shell":           "在容器里执行一条命令（需管理员）",
	"GET /api/runs/{runId}/sandbox/terminal":         "开容器终端（WebSocket）",
	"POST /api/runs/{runId}/sandbox/terminal/input":  "把按键送进容器终端",
	"POST /api/runs/{runId}/sandbox/terminal/ticket": "签一张开终端用的一次性票",

	"POST /api/workspaces/{workspaceId}/conversations": "开新对话",
	"POST /api/runs/{runId}/events":                    "发消息",
	"GET /api/runs/{runId}/events/stream":              "订阅事件流（SSE）",
	"POST /api/runs/{runId}/redirect":                  "打断这一轮并立刻发新消息",

	"GET /api/workspaces/{workspaceId}/schedules":              "列出定时任务",
	"POST /api/workspaces/{workspaceId}/schedules":             "建定时任务",
	"GET /api/schedules/{id}":                                  "取定时任务",
	"PATCH /api/schedules/{id}":                                "改定时任务",
	"PATCH /api/schedules/{id}/status":                         "启停定时任务",
	"DELETE /api/schedules/{id}":                               "删定时任务",
	"POST /api/schedules/{id}/run-now":                         "立刻跑一次",
	"GET /api/schedules/{id}/revisions":                        "列定时任务历史版本",
	"GET /api/schedules/{id}/revisions/{revisionId}":           "看定时任务某个历史版本",
	"POST /api/schedules/{id}/revisions/{revisionId}/rollback": "回滚定时任务到某个版本",

	"GET /api/workspaces/{workspaceId}/feishu/apps":                      "列出飞书应用",
	"GET /api/workspaces/{workspaceId}/feishu/chats":                     "列出飞书群",
	"POST /api/workspaces/{workspaceId}/feishu/registrations":            "发起飞书注册",
	"GET /api/workspaces/{workspaceId}/feishu/registrations/{sessionId}": "轮询飞书注册",
	"GET /api/feishu/apps/{id}":                                          "取飞书应用",
	"POST /api/feishu/apps/{id}/readiness/refresh":                       "重跑巡检",
	"POST /api/feishu/apps/{id}/admin-authorization":                     "标记管理员已授权",
	"PATCH /api/feishu/apps/{id}/trigger-mode":                           "改机器人在群里什么时候开口",
	"DELETE /api/feishu/apps/{id}":                                       "解除飞书接入",

	"GET /api/workspaces/{workspaceId}/api-keys":  "列出我的 API Key",
	"POST /api/workspaces/{workspaceId}/api-keys": "创建 API Key",
	"DELETE /api/api-keys/{id}":                   "吊销 API Key",
	// The two account-level ones: the global settings page is outside any workspace and has no workspaceId.
	"GET /api/api-keys":  "列出我的 API Key（账号级）",
	"POST /api/api-keys": "创建 API Key（账号级）",

	"POST /api/admin/users": "创建本地账号（平台管理员）",

	"GET " + PathSpec:    "本 OpenAPI 文档",
	"GET " + PathLLMsTxt: "给 agent 读的项目说明",

	"POST /api/_debug/reset": "重置成种子数据",
	"POST /api/_debug/clear": "清空数据",

	// -- Passthrough of the raw Ark Managed Agents API (/ark/...) ----------------------
	// Fields match Ark exactly, so these paths can be used against the Ark docs; see each group's detailed description.
	"POST /api/runs/{runId}/ark/upgrade": "升级这条会话的运行配置（方舟 sessions/{id}/upgrades）",

	"GET /api/runs/{runId}/ark/resources":                 "列出会话挂载的资源（文件等）",
	"POST /api/runs/{runId}/ark/resources":                "给会话添加一个资源（挂文件）",
	"GET /api/runs/{runId}/ark/resources/{resourceId}":    "查询一个会话资源",
	"GET /api/runs/{runId}/ark/threads":                   "列出会话的线程",
	"GET /api/runs/{runId}/ark/threads/{threadId}":        "查询一个线程",
	"GET /api/runs/{runId}/ark/threads/{threadId}/events": "查询某条线程的事件",
	"GET /api/runs/{runId}/ark/threads/{threadId}/stream": "流式订阅某条线程的事件（SSE）",
	"GET /api/runs/{runId}/ark/files":                     "列出 Files（方舟账号下的文件）",
	"POST /api/runs/{runId}/ark/files":                    "上传一个文件（multipart，拿 file_id）",
	"GET /api/runs/{runId}/ark/files/{fileId}":            "查询文件元信息",
	"DELETE /api/runs/{runId}/ark/files/{fileId}":         "删除一个文件",

	"GET /api/workspaces/{workspaceId}/ark/vaults":                                         "列出保管库 Vault",
	"POST /api/workspaces/{workspaceId}/ark/vaults":                                        "创建保管库",
	"GET /api/workspaces/{workspaceId}/ark/vaults/{vaultId}":                               "查询一个保管库",
	"POST /api/workspaces/{workspaceId}/ark/vaults/{vaultId}":                              "更新保管库",
	"DELETE /api/workspaces/{workspaceId}/ark/vaults/{vaultId}":                            "删除保管库（连同全部凭证）",
	"GET /api/workspaces/{workspaceId}/ark/vaults/{vaultId}/credentials":                   "列出保管库里的凭证",
	"POST /api/workspaces/{workspaceId}/ark/vaults/{vaultId}/credentials":                  "在保管库里创建凭证",
	"GET /api/workspaces/{workspaceId}/ark/vaults/{vaultId}/credentials/{credentialId}":    "查询一个凭证",
	"POST /api/workspaces/{workspaceId}/ark/vaults/{vaultId}/credentials/{credentialId}":   "更新凭证",
	"DELETE /api/workspaces/{workspaceId}/ark/vaults/{vaultId}/credentials/{credentialId}": "删除凭证",

	"GET /api/workspaces/{workspaceId}/ark/memory-stores":                                        "列出记忆库",
	"POST /api/workspaces/{workspaceId}/ark/memory-stores":                                       "创建记忆库",
	"GET /api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}":                        "查询一个记忆库",
	"POST /api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}":                       "更新记忆库",
	"DELETE /api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}":                     "删除记忆库（连同全部记忆）",
	"GET /api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}/memories":               "列出记忆条目",
	"POST /api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}/memories":              "写一条记忆",
	"POST /api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}/memories/batch-create": "批量写记忆",
	"GET /api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}/memories/{memoryId}":    "查询一条记忆（含正文）",
	"POST /api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}/memories/{memoryId}":   "更新一条记忆",
	"DELETE /api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}/memories/{memoryId}": "删除一条记忆",
}

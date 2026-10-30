// Parameters, request bodies, response bodies, and entity schemas.
//
// Entity schemas are hand-written instead of reflecting domain struct tags; see the package comment in openapi.go for why:
// reflection cannot express the difference between "this key must be present and must be []" and "this key may be absent", and that difference
// is the core of this repo's frontend contract (see the three JSON tag iron rules in model/apimodel).
//
// Field descriptions are carried over from the domain comments as much as possible, especially the "counterintuitive but must be so" ones:
// `apiKeyMasked` is an empty string rather than a missing key when not configured, the real meaning of `requires_action`,
// and 0 is a valid value for the four `usage` fields. Only by reading these will agents not misjudge.

package openapi

import (
	"net/http"
	"strings"

	"github.com/chyroc/ama/server/authz"
	"github.com/chyroc/ama/server/core"
)

// ---------------------------------------------------------------------------
// Parameters
// ---------------------------------------------------------------------------

// pathParamDocs are the descriptions of path parameters, keyed by parameter name.
//
// Indexed by parameter name rather than by "route + parameter name": a parameter with the same name means the same thing in different routes
// (`{id}` is the exception, it changes with the path, so pathParameters below special-cases it by path prefix).
var pathParamDocs = map[string]string{
	"workspaceId": "空间 id，形如 `ws_example`。\n\n" +
		"⚠️ **不是 `key`**：空间有两个标识——`id`（`ws_` 开头，接口里用）与 " +
		"`key`（短名，出现在前端 URL 里）。拿到 key 要先用 `GET /api/workspace-by-key` 换 id。",
	"memberId":  "成员 id（不是邮箱）。",
	"runId":     "Run 的 id，形如 `run_example`。一次对话就是一条 Run。",
	"provider":  "授权源标识，取值见 `GET /api/integrations` 返回的 `provider` 字段。",
	"sessionId": "飞书注册流程的 sessionId，由发起注册的接口返回。",
	// The ones below had no description before. **Missing one gives no error**, but agents cannot tell what to put in,
	// so they fill it with the parent resource's id, which gives 404, and the error does not point to "filled in the wrong kind of id".
	"revisionId": "版本 id，取值来自对应资源的 `GET .../revisions` 列表。\n\n" +
		"⚠️ **不是版本号**（`version` 那个整数），是列表里那条记录自己的 id。",
	"accountId": "火山账号 id，取值来自 `GET /api/ark-fetch/accounts`。",
	"messageId": "排队消息的 id，取值来自 `GET /api/runs/{runId}/pending`。",
	"hostId": "宿主机 id（`hst_` 开头）。取值来自 `GET /api/hosts` 或" +
		" `GET /api/workspaces/{workspaceId}/hosts`；是用 `ama daemon register` 注册机器时服务端返回的。",

	"partNumber": "分片号，**从 1 开始的整数**，上限 10000。\n\n" +
		"同一个片号可以重传（后传的覆盖前传的）；合并顺序由片号决定，与上传顺序无关。" +
		"每片固定 8 MiB（最后一片可更小）。",

	// Path parameters of the Ark passthrough endpoints (/ark/...). Field names match the Ark docs exactly
	// (snake_case is written as camelCase in URL segments, and converted back to Ark's {memory_store_id} on passthrough).
	"resourceId": "会话资源 id，取值来自 `GET .../ark/resources` 返回的 `id`。",
	"threadId":   "会话线程 id，形如 `sthr-...`，取值来自 `GET .../ark/threads`。",
	"fileId":     "文件 id，由 `POST .../ark/files` 上传后返回（方舟 Files API 的 file id）。",
	"vaultId":    "保管库 id，形如 `vault-...`，取值来自 `GET .../ark/vaults`。",
	"memoryStoreId": "记忆库 id，形如 `memstore-...`，取值来自 " +
		"`GET .../ark/memory-stores`。",
	"memoryId": "记忆条目 id，形如 `mem-...`，取值来自 " +
		"`GET .../ark/memory-stores/{memoryStoreId}/memories`。",
}

// pathParamDocsByPath handles parameters with the same name meaning different things under different routes; the key is "path parameterName".
//
// Right now there is only the Ark passthrough {credentialId}: the same-named parameter in pathParamDocs belongs to
// authorization management (integrations), whose description says "the value comes from /api/integrations/...",
// and used on Ark Vault credentials it would send agents to a completely unrelated endpoint.
var pathParamDocsByPath = map[string]string{
	"/api/workspaces/{workspaceId}/ark/vaults/{vaultId}/credentials credentialId": "方舟保管库凭证 id，取值来自 `GET .../ark/vaults/{vaultId}/credentials`。\n\n" +
		"⚠️ 不是授权管理里那个 integrations credential id，两者是不同系统的东西。",
	"/api/workspaces/{workspaceId}/ark/vaults/{vaultId}/credentials/{credentialId} credentialId": "方舟保管库凭证 id，取值来自 `GET .../ark/vaults/{vaultId}/credentials`。\n\n" +
		"⚠️ 不是授权管理里那个 integrations credential id，两者是不同系统的东西。",
}

// idParamDocs describes, by path prefix, what the generic `{id}` is the id of.
//
// ⚠️ **The prefix must be written up to the end of that segment** (with a trailing slash): `/api/issues/` and `/api/issue-comments/`
// are two different resources, and one missing slash would make the latter match the former's description.
var idParamDocs = map[string]string{
	"/api/agents/":                 "Agent 的 id，形如 `agent_example`。",
	"/api/skills/":                 "技能的 id，形如 `skill_example`。",
	"/api/runs/":                   "Run 的 id，形如 `run_example`。",
	"/api/schedules/":              "定时任务的 id，形如 `sch_example`。",
	"/api/feishu/apps/":            "飞书应用的 id。",
	"/api/api-keys/":               "API Key 的 id（**不是 key 本身**，是列表接口返回的那个 id）。",
	"/api/issues/":                 "问题的 id。",
	"/api/issue-comments/":         "问题评论的 id（评论自己的 id，不是它所属问题的 id）。",
	"/api/issue-attachments/":      "问题附件的 id（附件自己的 id，不是它所属问题的 id）。",
	"/api/suggestions/":            "产品建议的 id。",
	"/api/suggestion-replies/":     "建议回复的 id（回复自己的 id，不是它所属建议的 id）。",
	"/api/suggestion-attachments/": "建议附件的 id（附件自己的 id，不是它所属建议的 id）。",
	"/api/files/":                  "个人文件的 id，`uf_` 开头，由 `POST /api/files` 发起上传时返回。",
}

// pathParameters extracts {xxx} from the path and generates parameter objects.
//
// Generated rather than hand-written: hand-written, every new route with parameters would need remembering to add them, and a miss would make that route's
// parameters disappear entirely from the spec, and agents following the spec would splice `{workspaceId}` into the URL as a literal.
func pathParameters(path string) []any {
	var out []any
	for _, seg := range strings.Split(path, "/") {
		if !strings.HasPrefix(seg, "{") || !strings.HasSuffix(seg, "}") {
			continue
		}
		name := strings.Trim(seg, "{}")
		desc := pathParamDocs[name]
		if d, ok := pathParamDocsByPath[path+" "+name]; ok {
			desc = d
		}
		if name == "id" {
			for prefix, d := range idParamDocs {
				if strings.HasPrefix(path, prefix) {
					desc = d
					break
				}
			}
		}
		p := newOrderedMap()
		p.set("name", name)
		p.set("in", "path")
		p.set("required", true)
		if desc != "" {
			p.set("description", desc)
		}
		p.set("schema", typeSchema("string"))
		out = append(out, p)
	}
	return out
}

// queryParameters are query parameters; only a few routes have them, so they are hand-written by pattern.
func queryParameters(r authz.RouteDecl) []any {
	switch r.Method + " " + r.Path {
	case "GET /api/workspace-by-key":
		return []any{queryParam("key", true, "空间的短名（URL 标识），形如 `my-team`。")}
	case "GET /api/workspace-key-taken":
		return []any{
			queryParam("key", true, "要检查的短名。"),
			queryParam("exceptId", false, "排除这个空间自己（改 key 时用，避免撞自己）。缺失与空串等价。"),
		}
	case "GET /api/skills/{id}/file":
		return []any{
			queryParam("path", true,
				"要读哪个附件，相对技能根目录的路径，形如 `scripts/init.sh`。\n\n"+
					"取值来自 `GET /api/skills/{id}` 响应里的 `files[].path`。"+
					"**`SKILL.md` 不在其中**——技能正文是 `instructions` 字段，不是附件。"),
		}
	case "GET /api/files":
		return []any{queryParam("limit", false,
			"返回条数上限。缺省 100，最大 1000；按创建时间倒序。只返回已上传完成的文件。")}
	case "GET /api/integrations/{provider}/apps":
		return []any{
			queryParam("workspaceId", false,
				"把 `current` 的含义换成「**在这个空间**发起授权会授权哪一个应用」。\n\n"+
					"缺省时 `current` 是**账号级默认**那个（授权管理页显示的那一个）。\n\n"+
					"⚠️ agent 表单要显示「即将授权哪个应用」**必须传它**："+
					"账号级默认那个恰恰可能不是这个 agent 该用的。\n\n"+
					"⚠️ 别在前端按 `workspaceName` 自己匹配——`LarkUserApp` 只有空间**名**、"+
					"没有 id，同名空间会静默错位。这个参数让后端用与发起授权**同一条**"+
					"解析路径回答，两者因此不可能漂移。\n\n"+
					"不是这个空间的成员会拿到 404。"),
		}
	case "GET /api/integrations",
		"GET /api/integrations/{provider}":
		// "Which workspace to judge the state by". **It must be the same value as the authorize one**: if they differ
		// it is that dead end of "authorization succeeded, but the card always says not authorized"; see the part below.
		return []any{
			queryParam("workspaceId", false,
				"按这个空间判飞书用户授权的状态（该用哪个飞书应用）。"+
					"缺省用**账号级默认**那个（授权管理页就是这么用的）。\n\n"+
					"⚠️⚠️ **从 agent 页读状态必须传，且要与 `authorize` 那次传的一致。** "+
					"授权是按空间授权的（token 记在 `(你, 那个空间的 app_id)` 上），"+
					"状态漏传就会按账号级默认那个应用查——于是 token 明明已经落库，"+
					"卡片仍然显示「还没有授权」。那条 FixHint 又让用户去点「发起授权」，"+
					"他每点一次都真的授权成功、每次卡片都说没授权，**全链路 HTTP 200**。\n\n"+
					"不认识这个参数的授权源（另外四个）会**静默按账号级回答**，"+
					"不像 `authorize` 那样回 400：这条是只读的，"+
					"让它整条失败会连带把那四张好卡片一起拉不出来。\n\n"+
					"不是这个空间的成员时，飞书那张卡片的状态会说「看不到这个空间的飞书应用」"+
					"并收起授权按钮（**不会**说成「还没有授权」——那会让人白点）。"),
		}
	case "POST /api/integrations/{provider}/authorize",
		"GET /api/integrations/{provider}/authorization":
		// "Which workspace to pick the app by". Only Feishu user authorization has this concept; other sources passing it are rejected (400
		// `workspace_scope_unsupported`). **Deliberately not ignored silently**: ignoring would make the caller
		// think the restriction took effect, while it actually gets the account-level default app.
		return []any{
			queryParam("workspaceId", false,
				"按这个空间选飞书应用。缺省用**账号级默认**那个（授权管理页就是这么用的）。\n\n"+
					"⚠️ **从 agent 页发起授权必须传**：agent 属于某个空间，"+
					"它要读的群消息是那个空间的飞书应用收到的，而 token 与应用是绑定的。"+
					"不传会授权账号级默认那个应用，然后运行时报"+
					"「还没对飞书应用 cli_xxx 做过用户授权」——那句话不指向「授权错了一个」。\n\n"+
					"⚠️⚠️ **发起（authorize）与轮询（authorization）必须传同一个值。** "+
					"发起把 device_code 记在 `(你, app_id)` 上，轮询要拿同一个 app_id 才找得回；"+
					"不一致时会回「这次授权流程已失效」，而那条的 `pending` 是 false，"+
					"前端会当成终态停止轮询——表现是刚点完授权就被告知已失效。\n\n"+
					"不是这个空间的成员会拿到 404（不区分「空间不存在」与「没接入飞书应用」，"+
					"刻意不给探测口）。"),
		}
	case "GET /api/workspaces/{workspaceId}/issues":
		return []any{
			queryParam("status", false,
				"只看这个状态的问题（`open` / `in_progress` / `closed`）。缺省给全部。\n\n"+
					"⚠️ 认不出的值会被**拒绝**（400），不是静默忽略——"+
					"忽略的话你会拿到全部而以为筛过了。"),
		}
	case "GET /api/suggestions":
		return []any{
			queryParam("status", false,
				"只看这个状态的建议（`open` / `resolved`）。缺省给两态。\n\n"+
					"⚠️ 认不出的值会被**拒绝**（400），不是静默忽略。"),
			queryParam("author", false,
				"只看这个人提的。\n\n"+
					"⚠️ **只对平台方有效**：普通用户传了会被服务端无条件覆盖成自己。"+
					"这不是 bug，是这条路径的授权实现——采纳它等于让任何人读任何人的建议。"),
			queryParam("limit", false, "每页条数，缺省 50，上限 200。超了截到上限而不是报错。"),
			queryParam("offset", false, "跳过多少条。"),
		}
	case "GET /api/workspaces/{workspaceId}/runs":
		return []any{
			queryParam("agentId", false, "只看这个 Agent 的运行。"),
			queryParam("status", false,
				"只看这些状态的运行（`running` / `idle` / `requires_action` / "+
					"`rescheduling` / `terminated`）。缺省给全部。\n\n"+
					"可以重复给（`?status=a&status=b`），也可以逗号分隔——两种都收。"),
			queryParam("search", false, "在标题 / Agent 名 / run id 三处做大小写不敏感的子串匹配。"),
			queryParam("scheduleId", false,
				"只看这条定时任务触发的运行，用来回答「这条任务历史上跑得怎么样」。\n\n"+
					"⚠️ **它查不到 2026-08-30 之前的历史运行**：那时 run 上还没记这个关联，"+
					"而已产生的数据补不上。所以查到的条数可能少于 `schedule.runCount`，"+
					"差额就是更早的那些——别把它读成「这条任务只跑过这么多次」。\n\n"+
					"另外 `trigger=schedule` **不能替代它**：那只筛出「所有定时任务的运行」，"+
					"一个空间有多条任务时它们混在一起。"),
			queryParam("trigger", false,
				"只看这些触发方式的运行（`manual` / `feishu` / `schedule` / "+
					"`api` / `webhook`）。缺省给全部。\n\n"+
					"可以重复给（`?trigger=a&trigger=b`），也可以逗号分隔——两种都收。\n\n"+
					"「人跟 agent 对话」的会话是 `manual,feishu` 两种"+
					"（网页发起的与飞书里聊出来的），对话页左栏取的就是这个组合。"),
			queryParam("limit", false,
				"这一页最多给多少条，上限 200（超了截到上限而不是报错）。\n\n"+
					"⚠️ **缺省是不限**（把整个空间的运行全给），"+
					"与 `/api/feedback`、`/api/suggestions` 那两条缺省 50 的**相反**。"+
					"这条接口先有一批全量口径的调用方，缺省分页会静默地把它们的统计截断。\n\n"+
					"⚠️ 拿它做统计（总数、token 汇总）时**别只取第一页再求和**——"+
					"那个数字看着正常，实际只统计了一页。要总量走 "+
					"`GET /api/workspaces/{workspaceId}/usage`，它在服务端聚合全量。"),
			queryParam("offset", false,
				"跳过多少条，用于翻页。可以与 `limit` 分开用："+
					"只给 `offset` 表示「跳过前 N 条、要剩下全部」。"),
		}
	case "GET /api/workspaces/{workspaceId}/usage":
		return []any{
			queryParam("since", false, "窗口起点（含），ISO 8601。缺省是 30 天前。"),
			queryParam("until", false, "窗口终点（**不含**），ISO 8601。缺省是现在。"),
		}
	case "GET /api/runs/{runId}/events":
		return []any{
			queryParam("types", false, "只要这些事件类型，逗号分隔。缺省全给。"),
			queryParam("threadId", false, "只要这个线程的事件。"),
		}
	case "GET /api/runs/{runId}/events/stream":
		return []any{
			queryParam("api_key", false,
				"凭据的逃生口，给**不能带自定义请求头**的 SSE 客户端用（浏览器的 `EventSource` 就是）。\n\n"+
					"⚠️ 只接受 API Key，**不接受会话 token**：query 参数会进访问日志与浏览器历史。"),
		}
	default:
		return nil
	}
}

func queryParam(name string, required bool, desc string) *orderedMap {
	p := newOrderedMap()
	p.set("name", name)
	p.set("in", "query")
	p.set("required", required)
	p.set("description", desc)
	p.set("schema", typeSchema("string"))
	return p
}

// ---------------------------------------------------------------------------
// Query parameters extracted to components.parameters
// ---------------------------------------------------------------------------
//
// MA puts query parameters under components.parameters and references them with $ref:
//
//	- $ref: '#/components/parameters/ListAgentsQuery.limit'
//
// The benefit of extracting them is that **the same parameter is written only once**, and client generators can generate shared parameter types.
// This follows it, and its shape too: the key is `<operation>Query.<parameterName>`.
//
// ⚠️⚠️ **Deliberately not merged by parameter name.** It looks like the two `limit`s could be merged into one
// `ListQuery.limit`, but their meanings are **opposite**:
//
//	GET /api/suggestions                        limit defaults to 50
//	GET /api/workspaces/{workspaceId}/runs      limit defaults to **unlimited** (gives everything)
//
// The latter's description says explicitly "the **opposite** of /api/suggestions, which defaults to 50".
// Merging would feed one's description to the other's callers, and that mistake **gives no error**:
// it only makes people think paging behaves the same, and then statistics get silently truncated.
// MA itself also splits by operation (`ListAgentsQuery.limit` and `ListAgentVersionsQuery.limit`
// are two), so this is not departing from MA but following it.
//
// So the keys here are named by operationId prefix, and naturally never merge across operations.

// queryParamRefs replaces a route's query parameters with $ref, and registers the definitions into defs at the same time.
//
// It returns the $ref array; defs are collected by componentsObject and output to components.parameters.
//
// ⚠️ **Both call sites must share this one function, not each write their own.** Generating an operation needs the $ref array,
// collecting components needs the definitions, and the two are two halves of the same walk. Written separately, "the referenced keys" and
// "the defined keys" would each be computed once, and once either side's naming rule changes there are dangling $refs or orphan definitions,
// while the spec itself is still valid JSON, and it only blows up when generating a client.
// TestQueryParamsAreReusableRefs checks both directions exactly to cover this.
func queryParamRefs(r authz.RouteDecl, defs *orderedMap) []any {
	params := queryParameters(r)
	if len(params) == 0 {
		return nil
	}
	prefix := operationID(r) + "Query."
	out := make([]any, 0, len(params))
	for _, p := range params {
		param, ok := p.(*orderedMap)
		if !ok {
			out = append(out, p)
			continue
		}
		nameAny, _ := param.get("name")
		name, _ := nameAny.(string)
		if name == "" {
			out = append(out, p)
			continue
		}
		key := prefix + name
		defs.set(key, param)
		out = append(out, schemaParamRef(key))
	}
	return out
}

// schemaParamRef references one entry under components.parameters.
func schemaParamRef(key string) *orderedMap {
	m := newOrderedMap()
	m.set("$ref", "#/components/parameters/"+key)
	return m
}

// collectQueryParams walks all routes and collects the content of components.parameters.
//
// It runs a separate walk instead of accumulating while generating paths: the output order of paths and components is separate,
// and orderedMap keeps order, so collecting in allRoutes() order here gives stable output;
// an unstable order would make TestSpecFileIsUpToDate fail forever, with an error saying "the spec is outdated".
func collectQueryParams() *orderedMap {
	defs := newOrderedMap()
	for _, r := range allRoutes() {
		if r.Method == "" {
			continue
		}
		queryParamRefs(r, defs)
	}
	return defs
}

// ---------------------------------------------------------------------------
// Request bodies
// ---------------------------------------------------------------------------

// requestBodyFor returns a route's request body, or nil when there is no body.
//
// GET / DELETE never have a body. The rest are looked up by pattern; those not in the table return nil instead of
// making up an empty body: an empty body would make client generators create a required empty object parameter.
func requestBodyFor(r authz.RouteDecl) *orderedMap {
	if r.Method == "GET" || r.Method == "DELETE" {
		return nil
	}
	// Ark passthrough endpoints: the request body matches Ark exactly, and the schema is not repeated here (that would drift from the Ark docs).
	// They all get "any JSON object", and the file upload one gets a multipart wildcard.
	if isArkProxyPath(r.Path) {
		content := newOrderedMap()
		media := newOrderedMap()
		if strings.HasSuffix(r.Path, "/ark/files") {
			media.set("schema", multipartArkFileSchema())
			content.set("multipart/form-data", media)
		} else {
			media.set("schema", arbitrarySchema("方舟该接口的请求体原文，字段见方舟 Managed Agents API 文档。"))
			content.set("application/json", media)
		}
		body := newOrderedMap()
		body.set("required", true)
		body.set("content", content)
		return body
	}
	ref, ok := requestSchemas[r.Method+" "+r.Path]
	if !ok {
		return nil
	}
	content := newOrderedMap()
	media := newOrderedMap()
	media.set("schema", ref)
	content.set(contentTypeFor(r), media)

	body := newOrderedMap()
	body.set("required", true)
	body.set("content", content)
	return body
}

// isArkProxyPath decides whether this is a passthrough route of the raw Ark API (/runs/{id}/ark/... or
// /workspaces/{id}/ark/...).
func isArkProxyPath(path string) bool {
	return strings.Contains(path, "/ark/") || strings.HasSuffix(path, "/ark")
}

// arbitrarySchema is the schema of "any JSON" ({} accepts any type; here it is constrained to object,
// since the bodies of this batch of Ark write endpoints are all objects).
func arbitrarySchema(desc string) *orderedMap {
	m := newOrderedMap()
	m.set("type", "object")
	m.set("description", desc)
	m.set("additionalProperties", true)
	return m
}

// multipartArkFileSchema describes the multipart form of Files upload: a file + purpose text.
func multipartArkFileSchema() *orderedMap {
	m := newOrderedMap()
	m.set("type", "object")
	m.set("description", "multipart 表单：`file` 是文件部分（字段名必须为 file），"+
		"`purpose` 等文本字段与方舟 Files API 一致（挂给 agent 用 purpose=agent）。")
	props := newOrderedMap()
	fileField := newOrderedMap()
	fileField.set("type", "string")
	fileField.set("format", "binary")
	fileField.set("description", "要上传的文件本身。")
	purposeField := stringField("文件用途，挂给 agent 用 `agent`。")
	props.set("file", fileField)
	props.set("purpose", purposeField)
	m.set("properties", props)
	m.set("required", []string{"file"})
	return m
}

func contentTypeFor(r authz.RouteDecl) string {
	// Uploading a skill package takes multipart; everything else is JSON.
	if strings.HasSuffix(r.Path, "/skills/upload") {
		return "multipart/form-data"
	}
	// The simple content of personal files and every chunk are raw bytes, not JSON.
	if r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/api/files/") {
		return "application/octet-stream"
	}
	return "application/json"
}

// binaryBodySchema is the schema of a raw byte request body (application/octet-stream).
func binaryBodySchema(desc string) *orderedMap {
	m := newOrderedMap()
	m.set("type", "string")
	m.set("format", "binary")
	m.set("description", desc)
	return m
}

// requestSchemas are the input schemas of every write endpoint, with keys exactly the same as the matrix patterns.
var requestSchemas = map[string]any{
	"POST /api/workspaces":                objectSchema("建空间的入参", requiredFields("name", "key"), workspaceInputProps()),
	"PATCH /api/workspaces/{workspaceId}": objectSchema("改空间的入参。三个字段都可选，只传要改的。", nil, workspaceInputProps()),

	"POST /api/workspaces/{workspaceId}/members": objectSchema("加成员的入参", requiredFields("email", "role"), map[string]any{
		"email": stringField("对方的员工邮箱，**这就是他的身份**。"),
		"name":  stringField("展示名，留空则用邮箱前缀。"),
		"role":  enumField("角色", "owner", "admin", "member", "viewer"),
	}),
	"PATCH /api/workspaces/{workspaceId}/members/{memberId}": objectSchema("改角色的入参", requiredFields("role"), map[string]any{
		"role": enumField("新角色", "owner", "admin", "member", "viewer"),
	}),

	"POST /api/workspaces/{workspaceId}/agents": schemaRef("AgentDraft"),
	"PATCH /api/agents/{id}":                    schemaRef("AgentDraft"),

	// Onboarding. **There is no email in the input**, and that is not an omission: the subject is taken from the session by the server.
	// Passing email in has no effect at all (the server does not read it); see core/onboarding.go.
	"POST /api/user-onboarding": objectSchema(
		"标记引导已看完或已跳过", requiredFields("action", "version"), map[string]any{
			"action":  enumField("看完了还是跳过了", "complete", "skip"),
			"version": intField("引导内容的版本号，原样存下，供以后改版时比较。不能为负。"),
		}),

	// A platform admin creates a local account on someone's behalf.
	"POST /api/admin/users": objectSchema(
		"要建的本地账号。",
		requiredFields("email", "password"), map[string]any{
			"email":    stringField("邮箱，就是这个人在 AMA 里的唯一 ID（存成小写）。"),
			"password": stringField("初始密码，8~72 字节。只存 bcrypt 哈希，之后谁也读不回来。"),
			"name":     stringField("展示名，可空（空时用邮箱 @ 前面那段）。"),
			"admin":    boolField("要不要让这个人也成为平台管理员，默认 false。"),
		}),

	// Start a personal file upload.
	"POST /api/files": objectSchema(
		"发起一次文件上传。服务端按 size 决定走哪条路：\n\n"+
			"- `size` 给了且 **≤ 8 MiB**：`uploadMode=simple`，随后 `PUT .../content` 一次传完；\n"+
			"- `size` 更大或**不给**：`uploadMode=multipart`，随后按响应里的 `partSize`（8 MiB）"+
			"逐片 `PUT .../parts/{partNumber}`，最后 `POST .../complete`。\n\n"+
			"大文件上限 2 GiB。filename 只用于展示，不会出现在对象路径里。",
		requiredFields("filename"), map[string]any{
			"filename":    stringField("原始文件名，只用于展示与下载，必填，最长 255 字节，不能含控制字符。"),
			"contentType": stringField("文件的 MIME 类型，如 `application/zip`，可空。"),
			"size": intField("文件总字节数，可空。给了且 ≤8 MiB 走 simple，否则走分片；" +
				"最终大小以服务端回读为准，上限 2 GiB（2147483648）。"),
		}),

	// Both PUTs are raw bytes (contentTypeFor returns application/octet-stream for them).
	"PUT /api/files/{id}/content": binaryBodySchema(
		"simple 模式的整个文件内容。必须用 `Content-Length` 给准字节数，" +
			"长度与声明一致，且不超过 8 MiB。"),
	"PUT /api/files/{id}/parts/{partNumber}": binaryBodySchema(
		"这一片的字节。必须用 `Content-Length` 给准字节数；固定 8 MiB 一片" +
			"（最后一片可更小），不接受 chunked 请求。同片号可重传覆盖。"),

	// The report body of issue feedback.
	//
	// ⚠️ **This schema is the allowlist itself**, not "suggested fields to send". The server decodes the request body into a
	// struct with fixed fields (core.FeedbackInput), and unlisted keys are dropped at the moment of decoding.
	// So "could a sensitive field be missed" has a definite answer: impossible; we are not deleting fields,
	// we are picking them.
	//
	// Fields deliberately **absent**, written here so nobody thinks they were missed:
	// token / cookie / authorization / API response bodies / user message bodies / the raw UA string.
	"POST /api/feedback": objectSchema(
		"一条问题反馈。**服务端按本 schema 重建 payload，不存请求体原文。**",
		requiredFields("level"), map[string]any{
			"level": enumField("`auto` 自动记录（只采不敏感元信息）／"+
				"`user` 用户主动提交（可多采）。认不出的值回 400，"+
				"**服务端不会挑一个默认值**。", "auto", "user"),
			"routePattern": stringField("出错的路由，**id 要抹成占位符**，" +
				"如 `/w/{key}/chat/{runId}`。服务端会再抹一遍——含真实 id 的路径会让限流" +
				"失效（每条都是新键）并把聚合查询变成一张每行计数 1 的表。"),
			"httpStatus": intField("HTTP 状态码。拿不到（网络故障根本没有响应）传 0。"),
			"errorCode":  stringField("后端错误码，如 `access_denied` / `not_found` / `network_error`。"),
			"errorKind":  enumField("前端错误分类", "denied", "other"),
			"message":    stringField("给用户看过的那句错误文案。**两级都采**（它是后端给的中文句子，不是用户输入）。"),
			"workspaceId": stringField("出问题时所在空间的 **id**（`ws_` 开头），不是 key。" +
				"可空——错误可能发生在空间之外。不像 id 的值会被丢掉。"),
			"browser": stringField("浏览器**概要**，如 `Chrome 120`。" +
				"⚠️ 刻意不收原始 UA 串：那有指纹价值，而排查只要「什么浏览器」。"),
			"os":         stringField("操作系统概要，如 `macOS`。"),
			"appVersion": stringField("前端版本（构建时注入的 git sha）。"),
			"path": stringField("**完整路径（含 id）。只有 `level: \"user\"` 会采用**，" +
				"一级传了也会被丢掉——路径里的 id 能定位到具体一次对话，而一级没有用户同意。" +
				"query 部分一律砍掉（那里可能有 token）。"),
			"agentId": stringField("当前 agent 的 id。**只有 `level: \"user\"` 会采用。**"),
			"logId": stringField("服务端那次请求的 logid，来自响应头 `X-TT-Logid`。" +
				"**两级都会采用。**\n\n" +
				"它不需要用户同意：这一串是服务端自己生成的，不含用户的任何输入、也不是凭据，" +
				"而 `actor`（谁报的）两级本来就采。一级恰恰最需要它——那些记录是在用户不知情时" +
				"留下的，没有人在旁边解释「我当时在干嘛」。\n\n" +
				"⚠️ **`errorCode: \"network_error\"` 时它必然为空，那是正确的**：" +
				"那类失败下请求**从未到达服务端**（DNS/连接被拒/TLS/CORS 预检失败），" +
				"生成 logid 的 HTTP 中间件压根没执行过，服务端日志里不存在这次请求的" +
				"任何一行。**不要为了「让字段有值」而编一个**——一个查不到东西的 logid" +
				"比没有更浪费排查时间。"),
			"recent": arrayField("FeedbackCall", "最近几次接口调用的摘要。"+
				"**只有 `level: \"user\"` 会采用**，最多留最后 10 条。"),
			"consoleErrors": stringArrayField("最近几条控制台错误的首行。" +
				"**只有 `level: \"user\"` 会采用**，最多留最后 10 条。"),
		}),

	"POST /api/workspaces/{workspaceId}/skills": schemaRef("SkillDraft"),
	"PATCH /api/skills/{id}":                    schemaRef("SkillDraft"),
	"PUT /api/skills/{id}/file": objectSchema(
		"要改哪个附件、改成什么。", requiredFields("path", "content"),
		map[string]any{
			"path": stringField(
				"要改哪个附件，相对技能根目录的路径。**必须是已经存在的附件**" +
					"（取自 `GET /api/skills/{id}` 的 `files[].path`），不存在回 404。\n\n" +
					"⚠️ `SKILL.md` 会被拒：技能正文是 `instructions`，改它走 `PATCH /api/skills/{id}`。"),
			"content": stringField(
				"文件的**完整新内容**（整体替换，不是增量），**明文不是 base64**。\n\n" +
					"上限 " + core.HumanSkillFileLimit +
					"，且改完之后该技能所有附件的总量不能超过 " + core.HumanSkillTotalLimit + "。"),
		}),

	"POST /api/workspaces/{workspaceId}/issues": schemaRef("IssueDraft"),
	"PATCH /api/issues/{id}":                    schemaRef("IssueDraft"),
	"POST /api/issues/{id}/comments": objectSchema(
		"发一条评论。**只有 body 一个字段**——作者从登录身份取、时间由服务端打、"+
			"问题 id 在路径里。请求体里能出现的东西越少，能表达的越权就越少。",
		requiredFields("body"), map[string]any{
			"body": stringField("Markdown 正文，不能为空。"),
		}),
	"PATCH /api/issue-comments/{id}": objectSchema(
		"改一条评论的正文，服务端会刷新它的 `updatedAt`。",
		requiredFields("body"), map[string]any{
			"body": stringField("新的 Markdown 正文，不能为空。"),
		}),
	"POST /api/suggestions":       schemaRef("SuggestionDraft"),
	"PATCH /api/suggestions/{id}": schemaRef("SuggestionDraft"),
	"POST /api/suggestions/{id}/replies": objectSchema(
		"回一条。**只有 body 一个字段**——作者从登录身份取、`isStaff` 由服务端按名单判、"+
			"建议 id 在路径里。收下 isStaff 等于让任何人把自己标成官方答复。",
		requiredFields("body"), map[string]any{
			"body": stringField("Markdown 正文，不能为空。"),
		}),
	"PATCH /api/suggestion-replies/{id}": objectSchema(
		"改一条回复的正文，服务端会刷新它的 `updatedAt`。**只有作者本人能改**。",
		requiredFields("body"), map[string]any{
			"body": stringField("新的 Markdown 正文，不能为空。"),
		}),
	"POST /api/suggestions/{id}/attachments": objectSchema(
		"上传附件（`multipart/form-data`，字段名 `file`）。\n\n"+
			"判定规则与问题附件**逐字相同**（同一套实现）：类型由服务端嗅探，"+
			"允许 png/jpg/gif/webp、pdf、txt/md/csv/json、zip，内容与扩展名对不上一律拒，"+
			"**svg 不收**。单文件 5 MiB，单条建议最多 20 个。",
		requiredFields("file"), map[string]any{
			"file": func() any {
				m := newOrderedMap()
				m.set("type", "string")
				m.set("format", "binary")
				m.set("description", "要上传的文件。**文件名必须带扩展名**，否则判不出类型。")
				return m
			}(),
		}),

	"POST /api/issues/{id}/attachments": objectSchema(
		"上传附件（`multipart/form-data`，字段名 `file`）。\n\n"+
			"**类型由服务端嗅探判定**，不信客户端声明的 Content-Type："+
			"允许 png/jpg/gif/webp、pdf、txt/md/csv/json、zip；"+
			"内容与扩展名对不上一律拒。**svg 明确不收**（它能内嵌脚本）。\n\n"+
			"单文件上限 5 MiB，单个问题最多 20 个附件。",
		requiredFields("file"), map[string]any{
			"file": func() any {
				m := newOrderedMap()
				m.set("type", "string")
				m.set("format", "binary")
				m.set("description", "要上传的文件。**文件名必须带扩展名**，否则判不出类型。")
				return m
			}(),
		}),
	"POST /api/workspaces/{workspaceId}/skills/upload": objectSchema(
		"技能包上传。`SKILL.md` 单文件或 zip 包都收。", requiredFields("file"), map[string]any{
			"file": func() any {
				m := newOrderedMap()
				m.set("type", "string")
				m.set("format", "binary")
				m.set("description", "`SKILL.md` 或 zip 包。zip 里必须有 `SKILL.md`。")
				return m
			}(),
		}),

	// ⚠️ **Only region is required**: the key has two sources (`apiKey` filled by hand, or `fetchRef`
	// pointing to one of the keys in the Volcano account); they are mutually exclusive and one must be given. Marking apiKey required
	// would make the legal call "fetch from the Volcano account" look illegal in the spec, and an agent holding this
	// spec would keep stuffing in a plaintext it can never get.
	"PUT /api/workspaces/{workspaceId}/ark-credential": objectSchema(
		"存方舟凭证。key 二选一：手填 `apiKey`，或用 `fetchRef` 指向火山账号里已有的一把。\n\n"+
			"**保存时服务端会先拿这把 key 调一次方舟只读接口做校验**，按结果分三档：\n"+
			"- key 无效/被删（401/403）→ **422 `ark_key_invalid`，不落库**；\n"+
			"- key 有效但账号未开通 Managed Agents → 首次 **422 `ark_managed_agent_not_open`、不落库**，"+
			"用户确认后带 `confirmNotActivated: true` 重发才存；\n"+
			"- 方舟超时/5xx → **放行保存**，响应带一条 `warning`。",
		requiredFields("region"), map[string]any{
			"apiKey":   stringField("方舟 API Key 的**明文**。库里只存它的密文，响应里只回掩码。与 `fetchRef` 互斥。"),
			"region":   enumField("方舟区域", "cn-beijing", "ap-southeast-1"),
			"fetchRef": arkFetchRefSchema(),
			"confirmNotActivated": boolField("已看到「账号未开通 Managed Agents」警告仍要保存。" +
				"**只对未开通那一档生效**；坏 key 带它也会被拒。"),
		}),
	"PATCH /api/workspaces/{workspaceId}/ark-credential/region": objectSchema(
		"只改区域，不动 key。", requiredFields("region"), map[string]any{
			"region": enumField("方舟区域", "cn-beijing", "ap-southeast-1"),
		}),

	// The agent-level one, exactly the same shape as the workspace-level one (including the fetchRef source).
	"PUT /api/agents/{id}/ark-credential": objectSchema(
		"给这个 Agent 贴一把自己的 key。key 二选一：手填 `apiKey`，或用 `fetchRef` "+
			"指向火山账号里已有的一把。\n\n"+
			"配了之后这个 Agent 不再跟随空间；要改回跟随就 DELETE 这条资源。",
		requiredFields("region"), map[string]any{
			"apiKey":   stringField("方舟 API Key 的**明文**。库里只存它的密文，响应里只回掩码。与 `fetchRef` 互斥。"),
			"region":   enumField("方舟区域", "cn-beijing", "ap-southeast-1"),
			"fetchRef": arkFetchRefSchema(),
			"confirmNotActivated": boolField("已看到「账号未开通 Managed Agents」警告仍要保存。" +
				"三档校验与 422 细码同空间级那条。"),
		}),
	"PATCH /api/agents/{id}/ark-credential/region": objectSchema(
		"只改这个 Agent 那把 key 的区域。**跟随空间的 Agent 会被拒绝。**",
		requiredFields("region"), map[string]any{
			"region": enumField("方舟区域", "cn-beijing", "ap-southeast-1"),
		}),

	// Authorization injection. **Whole replacement**, with the same field name and shape as the PATCH agent one.
	"PUT /api/agents/{id}/integrations": objectSchema(
		"改这个 Agent 允许注入哪些授权源的凭据。**整份覆盖，不是增量。**\n\n"+
			"空数组是合法值，含义是「一个都不注入」——所以**这个键必须传**，"+
			"缺了会回 400 而不是当成空（否则一次拼错键名就把全部授权静默清掉）。\n\n"+
			"⚠️ 需要 `manage` 档位：注进去的凭据代表一个真人。",
		requiredFields("integrationIds"), map[string]any{
			"integrationIds": stringArrayField("授权源 id。取值见 `GET /api/integrations` " +
				"返回的 `id`（`ama` / `lark-user`）。\n\n" +
				"**不校验 id 是否存在**：悬空 id 的后果只是注入时跳过它。"),
		}),

	// The account-level one, exactly the same shape as the workspace-level one (including the fetchRef source).
	//
	// ⚠️ After saving, **every workspace referencing it uses the new key immediately**; the impact is larger than the workspace-level one.
	"PUT /api/global-ark-credential": objectSchema(
		"存**我自己**的账号级方舟凭证。key 二选一：手填 `apiKey`，或用 `fetchRef` "+
			"指向火山账号里已有的一把。\n\n"+
			"⚠️ 保存之后，所有 `source: inherit` 的空间会**立即**改用这把新 key。",
		requiredFields("region"), map[string]any{
			"apiKey":   stringField("方舟 API Key 的**明文**。库里只存它的密文，响应里只回掩码。与 `fetchRef` 互斥。"),
			"region":   enumField("方舟区域", "cn-beijing", "ap-southeast-1"),
			"fetchRef": arkFetchRefSchema(),
			"confirmNotActivated": boolField("已看到「账号未开通 Managed Agents」警告仍要保存。" +
				"三档校验与 422 细码同空间级那条。"),
		}),

	"POST /api/workspaces/{workspaceId}/conversations": objectSchema(
		"开一轮新对话。", requiredFields("agentId"), map[string]any{
			"agentId": stringField("要跟哪个 Agent 说话。"),
			"title":   stringField("会话标题，留空则由首条消息生成。"),
		}),
	"POST /api/runs/{runId}/events": objectSchema(
		"发一条消息。**这是往 Run 里追加事件**，见 /llms.txt 的数据模型一节。",
		requiredFields("events"), map[string]any{
			"events": func() any {
				m := newOrderedMap()
				m.set("type", "array")
				m.set("description",
					"要追加的事件。发一句话就是一个 `user.message` 事件：\n\n"+
						"```json\n"+
						"{\"events\": [{\"type\": \"user.message\", \"content\": [{\"type\": \"text\", \"text\": \"你好\"}]}]}\n"+
						"```\n\n"+
						"⚠️ 方舟**只接受 7 种 user 侧事件**，agent 侧的事件发不进去。")
				m.set("items", schemaRef("RunEvent"))
				return m
			}(),
		}),

	"POST /api/workspaces/{workspaceId}/schedules": schemaRef("ScheduleDraft"),
	"PATCH /api/schedules/{id}":                    schemaRef("ScheduleDraft"),
	"PATCH /api/schedules/{id}/status": objectSchema("启停一条定时任务。", requiredFields("status"), map[string]any{
		"status": enumField("新状态", "active", "paused"),
	}),

	"POST /api/workspaces/{workspaceId}/api-keys": objectSchema(
		"创建一把 API Key。", requiredFields("name"), map[string]any{
			"name": stringField("给这把 key 起个名字，便于日后分辨（如 `ci-deploy` / `my-agent`）。"),
		}),

	"POST /api/workspaces/{workspaceId}/feishu/registrations": objectSchema(
		"发起飞书应用自助注册。", nil, map[string]any{
			"appId": stringField("飞书应用的 app_id，形如 `cli_xxx`。"),
		}),
	"POST /api/integrations/{provider}/authorize": objectSchema(
		"发起一次授权（给我自己）。", nil, map[string]any{}),
	"PUT /api/integrations/{provider}/app": objectSchema(
		"换一个应用。", requiredFields("appId"), map[string]any{
			"appId": stringField("要换成哪个应用，形如 `cli_xxx`。\n\n" +
				"取值只能来自 `GET /api/integrations/{provider}/apps` 返回的 `appId`——" +
				"不在那份名单里的会回 404（那份名单就是「你能看到的应用」）。"),
		}),
}

func workspaceInputProps() map[string]any {
	return map[string]any{
		"name":        stringField("展示名。"),
		"key":         stringField("URL 标识，全局唯一，`[a-z0-9-]{2,32}`。**改它等于改所有链接**。"),
		"description": stringField("描述，可选。"),
	}
}

// onboardingStateSchema is the shape of the onboarding state, shared by two routes.
//
// A new one is built each time instead of sharing a variable: objectSchema returns a mutable orderedMap,
// and when the same instance is referenced in two places, a change in one changes the other.
func onboardingStateSchema() any {
	return objectSchema("我的新手引导状态", requiredFields("version"), map[string]any{
		"completedAt": stringField("看完引导的时刻（ISO 8601）。没看完就**没有这个键**。"),
		"skippedAt":   stringField("跳过引导的时刻（ISO 8601）。没跳过就**没有这个键**。"),
		"version": intField("我看过的引导内容版本。**没有记录时是 0**，" +
			"这个键一定存在（0 也给）。"),
	})
}

// ---------------------------------------------------------------------------
// Response bodies
// ---------------------------------------------------------------------------

// responsesFor generates a route's responses.
//
// Success responses are looked up by pattern; error responses are **all generated**: which error codes a route has is a function of its
// permission level and resolution rule, and both come from the matrix. Written by hand, some route would surely miss 403.
func responsesFor(r authz.RouteDecl) *orderedMap {
	res := newOrderedMap()

	// Success.
	key, body := successFor(r)
	ok := newOrderedMap()
	ok.set("description", successDescFor(r))
	if body != nil {
		content := newOrderedMap()
		media := newOrderedMap()
		media.set("schema", body)
		content.set(successContentType(r), media)
		ok.set("content", content)
	}
	res.set(key, ok)

	// Errors. The two routes without auth have no 401/403.
	if !isPublic(r) {
		res.set("401", errorResponse("未登录：没带凭据、凭据不认识、或会话过期。`code=unauthenticated`"))
		if r.Action != "none" {
			res.set("403", errorResponse(
				"没权限。`code=access_denied`。两种情况文案不同：**不是这个空间的成员**（去找管理员邀请你），"+
					"或**角色不够**（本接口要求 `"+r.MinRole+"`，去找管理员提权）。"))
		}
	}
	if r.ResourceKind != "" && !r.AllowMissing {
		res.set("404", errorResponse("资源不存在。`code=not_found`。\n\n"+
			"⚠️ **这里回 404 而不是 403 是刻意的**：把「已经被删了」说成「无权访问」会让你去要权限，而那个资源早就没了。"))
	}
	return res
}

// successFor returns the status code and schema of the success response.
func successFor(r authz.RouteDecl) (string, any) {
	pattern := r.Method + " " + r.Path
	// Ark passthrough endpoints: the response body is Ark's raw JSON, and the schema is not repeated. On DELETE Ark returns
	// a deletion receipt object (not 204); the content-type of the SSE one is handled in successContentType.
	if isArkProxyPath(r.Path) {
		if r.Method == http.MethodDelete {
			return "200", arbitrarySchema("方舟该接口的响应原文（删除回执对象）。")
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.Path, "/stream") {
			return "200", arbitrarySchema("SSE 事件流，每帧是一个方舟事件 JSON。")
		}
		return "200", arbitrarySchema("方舟该接口的响应原文，字段见方舟 Managed Agents API 文档。")
	}
	if schema, ok := responseSchemas[pattern]; ok {
		code := "200"
		if r.Method == "POST" && createsResource[pattern] {
			code = "201"
		}
		return code, schema
	}
	// Anything not registered counts as "success without content": DELETE and a few endpoints that only return ok.
	if r.Method == "DELETE" {
		return "204", nil
	}
	return "200", nil
}

func successDescFor(r authz.RouteDecl) string {
	if d, ok := successDescs[r.Method+" "+r.Path]; ok {
		return d
	}
	if r.Method == "DELETE" {
		return "删除成功。"
	}
	return "成功。"
}

func successContentType(r authz.RouteDecl) string {
	if strings.HasSuffix(r.Path, "/events/stream") {
		return "text/event-stream"
	}
	// The thread event stream passed through from Ark (the path ends with /stream, not /events/stream).
	if isArkProxyPath(r.Path) && strings.HasSuffix(r.Path, "/stream") {
		return "text/event-stream"
	}
	if r.Path == PathLLMsTxt {
		return "text/markdown"
	}
	// Attachment download returns **the raw bytes uploaded by the user**, with the type depending on the attachment (image/png,
	// application/pdf...). Writing application/json would make agents JSON.parse the response,
	// getting a parse error that cannot point to the real cause.
	//
	// `*/*` is how OpenAPI expresses "the type is not fixed"; the real type is in the response's
	// Content-Type header, decided by server-side sniffing.
	if r.Path == "/api/issue-attachments/{id}" && r.Method == "GET" {
		return "*/*"
	}
	if r.Path == "/api/suggestion-attachments/{id}" && r.Method == "GET" {
		return "*/*"
	}
	return "application/json"
}

// createsResource marks which POSTs return 201 instead of 200.
//
// Only "really created something with an id" is 201. Sending a message, verifying credentials, starting authorization
// are all 200: they create no new resource.
var createsResource = map[string]bool{
	"POST /api/workspaces":                        true,
	"POST /api/workspaces/{workspaceId}/members":  true,
	"POST /api/workspaces/{workspaceId}/agents":   true,
	"POST /api/workspaces/{workspaceId}/skills":   true,
	"POST /api/workspaces/{workspaceId}/issues":   true,
	"POST /api/issues/{id}/comments":              true,
	"POST /api/issues/{id}/attachments":           true,
	"POST /api/workspaces/{workspaceId}/api-keys": true,
	"POST /api/suggestions":                       true,
	"POST /api/suggestions/{id}/replies":          true,
	"POST /api/suggestions/{id}/attachments":      true,
	"POST /api/files":                             true,
}

var successDescs = map[string]string{
	"GET /api/workspaces":          "我所在的空间列表。**永远是数组，空的时候是 `[]` 不是 null**。",
	"POST /api/workspaces":         "建好的空间，调用者已是 owner。",
	"GET /api/workspace-by-key":    "那个空间。**非成员或不存在都回 404**。",
	"GET /api/workspace-key-taken": "`{\"taken\": true｜false}`。",

	"GET /api/workspaces/{workspaceId}/ark-credential": "凭证的**掩码视图**。\n\n" +
		"⚠️ 未配置时也是 200，`apiKeyMasked` 为空串——**不是 404**。",
	"POST /api/workspaces/{workspaceId}/ark-credential/verify": "回写了校验结果的凭证。\n\n" +
		"⚠️ **key 无效仍然是 200**，结论在 `check` 字段里，不是 HTTP 错误。",

	"GET /api/workspaces/{workspaceId}/api-keys": "我自己的 API Key 列表。**只有 `prefix`，没有明文**。",
	"POST /api/workspaces/{workspaceId}/api-keys": "建好的 key。\n\n" +
		"⚠️ **`key` 字段是明文，只在这一次响应里出现**——库里只存它的 sha256。" +
		"没存下来就只能吊销重建。",

	"POST /api/workspaces/{workspaceId}/conversations": "新建的那条 Run（`trigger: \"manual\"`）。拿它的 `id` 发消息。",
	"POST /api/runs/{runId}/events":                    "追加后的事件。agent 的回复是**异步**的，不在这个响应里。",
	"GET /api/runs/{runId}/events": "事件列表，按 `sequence` 升序。\n\n" +
		"⚠️ **run 不存在也回 200 + `[]`**，不是 404（列表接口永不 404）。",
	"GET /api/runs/{runId}/events/stream": "SSE 流。每帧是 `data: <一个事件的 JSON>`。\n\n" +
		"除了会落库的事件，还有 `event_deltas` 逐字增量帧——**那种不进事件历史**，只用来渲染打字效果。",

	"GET " + PathSpec:    "本 OpenAPI 文档。",
	"GET " + PathLLMsTxt: "Markdown 正文。",
}

// responseSchemas are the schemas of success responses, with keys exactly the same as the matrix patterns.
var responseSchemas = map[string]any{
	"GET /api/workspaces":                 arrayOf("Workspace"),
	"POST /api/workspaces":                schemaRef("Workspace"),
	"GET /api/workspace-by-key":           schemaRef("Workspace"),
	"PATCH /api/workspaces/{workspaceId}": schemaRef("Workspace"),
	"GET /api/workspace-key-taken": objectSchema("", requiredFields("taken"), map[string]any{
		"taken": boolField("这个 key 是否已被占用。"),
	}),

	"POST /api/workspaces/{workspaceId}/members":             schemaRef("WorkspaceMember"),
	"PATCH /api/workspaces/{workspaceId}/members/{memberId}": schemaRef("WorkspaceMember"),

	"GET /api/workspaces/{workspaceId}/agents":  arrayOf("Agent"),
	"POST /api/workspaces/{workspaceId}/agents": schemaRef("Agent"),
	"GET /api/agents/{id}":                      schemaRef("Agent"),
	"PATCH /api/agents/{id}":                    schemaRef("Agent"),

	"GET /api/workspaces/{workspaceId}/skills":         arrayOf("Skill"),
	"POST /api/workspaces/{workspaceId}/skills":        schemaRef("Skill"),
	"POST /api/workspaces/{workspaceId}/skills/upload": schemaRef("Skill"),
	"GET /api/skills/{id}":                             schemaRef("Skill"),
	"PATCH /api/skills/{id}":                           schemaRef("Skill"),
	// Returns the whole skill: the caller uses the new files[].size to refresh the list, and latestVersion to refresh the page header.
	"PUT /api/skills/{id}/file": schemaRef("Skill"),
	// Attachment content. **Inlined instead of creating a new components schema**: a new one would need syncing
	// schemaOrder in schemas.go, and missing it does not turn go test ./openapi/ red
	// (that test only reconciles the set of routes), so the spec would have a $ref pointing to a schema that does not exist.
	"GET /api/skills/{id}/file": objectSchema(
		"一个技能附件的内容。", requiredFields("path", "size", "content", "truncated", "binary"),
		map[string]any{
			"path": stringField("附件路径，规范化后的相对路径。"),
			"size": intField("这次返回的字节数（被截断时是截断后的长度，不是原文件大小）。"),
			"content": stringField(
				"文本内容，**明文不是 base64**。\n\n" +
					"⚠️ 与 `SkillFile.content` 刻意不同：那个字段是 Go 的 `[]byte`，" +
					"JSON 序列化成 base64。这里是 `string`，直接就是文本——" +
					"前端不要再 decode 一遍，那会把正常内容解成乱码。\n\n" +
					"`binary` 为真时是空串。"),
			"truncated": boolField(
				"是否只返回了前 256 KiB。\n\n" +
					"实践中**恒为 false**：上传侧单文件上限就是 256 KiB，所以入库的附件不会超。" +
					"这个字段是为「存量库里有超限的行」以及将来上限变化留的，" +
					"与 `GET /api/runs/{runId}/sandbox/file` 的响应形状保持一致。"),
			"binary": boolField(
				"是否是二进制文件（判据是含 NUL 字节）。为真时 `content` 是空串。\n\n" +
					"**单独一个字段而不是往 content 里塞一句「这是二进制」**：" +
					"那样调用方分不出「文件内容恰好是这句话」和「我们判定它是二进制」。"),
		}),

	"GET /api/suggestions":                   schemaRef("SuggestionPage"),
	"POST /api/suggestions":                  schemaRef("Suggestion"),
	"GET /api/suggestions/{id}":              schemaRef("Suggestion"),
	"PATCH /api/suggestions/{id}":            schemaRef("Suggestion"),
	"GET /api/suggestions/{id}/replies":      arrayOf("SuggestionReply"),
	"POST /api/suggestions/{id}/replies":     schemaRef("SuggestionReply"),
	"PATCH /api/suggestion-replies/{id}":     schemaRef("SuggestionReply"),
	"GET /api/suggestions/{id}/attachments":  arrayOf("SuggestionAttachment"),
	"POST /api/suggestions/{id}/attachments": schemaRef("SuggestionAttachment"),

	"GET /api/workspaces/{workspaceId}/issues":  arrayOf("Issue"),
	"POST /api/workspaces/{workspaceId}/issues": schemaRef("Issue"),
	"GET /api/issues/{id}":                      schemaRef("Issue"),
	"PATCH /api/issues/{id}":                    schemaRef("Issue"),
	"GET /api/issues/{id}/comments":             arrayOf("IssueComment"),
	"POST /api/issues/{id}/comments":            schemaRef("IssueComment"),
	"PATCH /api/issue-comments/{id}":            schemaRef("IssueComment"),
	"GET /api/issues/{id}/attachments":          arrayOf("IssueAttachment"),
	"POST /api/issues/{id}/attachments":         schemaRef("IssueAttachment"),

	"GET /api/workspaces/{workspaceId}/ark-credential":          schemaRef("ArkCredential"),
	"PUT /api/workspaces/{workspaceId}/ark-credential":          schemaRef("ArkCredential"),
	"PATCH /api/workspaces/{workspaceId}/ark-credential/region": schemaRef("ArkCredential"),
	"POST /api/workspaces/{workspaceId}/ark-credential/verify":  schemaRef("ArkCredential"),

	"GET /api/integrations":                 arrayOf("IntegrationState"),
	"GET /api/integrations/{provider}/apps": arrayOf("LarkUserApp"),
	"PUT /api/integrations/{provider}/app":  schemaRef("IntegrationState"),
	"GET /api/integrations/{provider}":      schemaRef("IntegrationState"),
	// ⚠️ This one **does not return IntegrationState**. What the handler actually writes is `{"authorizationUrl": ...}`
	// (authorizeIntegration in core/handler.go); declaring it as IntegrationState here before was
	// an existing inaccuracy, and an agent holding the wrong spec would read a field that does not exist.
	"POST /api/integrations/{provider}/authorize": objectSchema(
		"发起授权的结果：一条要在**新标签页**打开的授权页地址。\n\n"+
			"不做 302 是刻意的——重定向会把当前页顶掉，用户点完还得自己找回来。",
		requiredFields("authorizationUrl"), map[string]any{
			"authorizationUrl": stringField(
				"授权页地址。\n\n" +
					"⚠️ 飞书用户授权走**设备码**，所以这一次调用**有副作用**：" +
					"它已经向飞书申请了一个 device_code 并记在库里。" +
					"拿到地址之后必须去轮询 `GET /api/integrations/{provider}/authorization`，" +
					"**只有那次轮询才会把 token 换回来并落库**——不轮询的话用户在飞书里点了同意，" +
					"平台这边永远显示「未授权」。"),
		}),

	// Both return the same shape.
	//
	// ⚠️ `version` is **required**: people without a record also get `0`. Without this key
	// the caller's "onboard again if the version is behind" check would always be false, since undefined takes part in the comparison.
	"GET /api/user-onboarding":  onboardingStateSchema(),
	"POST /api/user-onboarding": onboardingStateSchema(),

	// Reporting returns 204 with no response body; no entry is written here (successFor produces no content for routes without an entry).
	//
	// Reading returns a page of feedback + the total. total is included because the caller needs to judge "is there a next page",
	// and relying on "does this page have limit items" is wrong when the last page happens to be full.
	"GET /api/feedback": objectSchema("一页问题反馈", requiredFields("reports", "total", "limit", "offset"),
		map[string]any{
			"reports": arrayField("FeedbackReport", "这一页的反馈，**按 `createdAt` 降序**。"+
				"永不为 null（空时是 `[]`）。"),
			"total":  intField("满足筛选条件的**总条数**（不只是这一页）。"),
			"limit":  intField("这次实际生效的每页条数。传 0 或超上限时服务端会收敛，所以**以这个值为准**。"),
			"offset": intField("这次跳过的条数。"),
		}),

	"GET /api/workspaces/{workspaceId}/runs":  arrayOf("Run"),
	"GET /api/workspaces/{workspaceId}/usage": schemaRef("UsageSummary"),
	"GET /api/runs/{id}":                      schemaRef("Run"),
	"GET /api/runs/{runId}/events":            arrayOf("RunEvent"),
	"GET /api/runs/{runId}/events/stream":     schemaRef("RunEvent"),

	"POST /api/workspaces/{workspaceId}/conversations": schemaRef("Run"),
	"POST /api/runs/{runId}/events":                    arrayOf("RunEvent"),

	"GET /api/workspaces/{workspaceId}/schedules":  arrayOf("Schedule"),
	"POST /api/workspaces/{workspaceId}/schedules": schemaRef("Schedule"),
	"GET /api/schedules/{id}":                      schemaRef("Schedule"),
	"PATCH /api/schedules/{id}":                    schemaRef("Schedule"),
	"PATCH /api/schedules/{id}/status":             schemaRef("Schedule"),
	"POST /api/schedules/{id}/run-now":             schemaRef("Run"),

	"GET /api/workspaces/{workspaceId}/api-keys":  arrayOf("APIKey"),
	"POST /api/workspaces/{workspaceId}/api-keys": schemaRef("APIKeyCreated"),

	"GET /api/workspaces/{workspaceId}/feishu/apps":                      arrayOf("FeishuApp"),
	"GET /api/feishu/apps/{id}":                                          schemaRef("FeishuApp"),
	"POST /api/feishu/apps/{id}/readiness/refresh":                       schemaRef("FeishuApp"),
	"POST /api/feishu/apps/{id}/admin-authorization":                     schemaRef("FeishuApp"),
	"GET /api/workspaces/{workspaceId}/feishu/registrations/{sessionId}": schemaRef("FeishuRegistration"),
	"POST /api/workspaces/{workspaceId}/feishu/registrations":            schemaRef("FeishuRegistration"),

	"GET " + PathSpec:    typeSchema("object"),
	"GET " + PathLLMsTxt: typeSchema("string"),
}

func errorResponse(desc string) *orderedMap {
	content := newOrderedMap()
	media := newOrderedMap()
	media.set("schema", schemaRef("Error"))
	content.set("application/json", media)

	res := newOrderedMap()
	res.set("description", desc)
	res.set("content", content)
	return res
}

// arkFetchRefSchema is the reference shape of "fetch a key from the Volcano account", shared by two save endpoints.
//
// ⚠️ **The plaintext is not here, and must never be added**: adding it would let the plaintext pass through the browser (into the devtools
// network panel, possibly read by extensions). The server fetches it on the spot with these two ids; see the top of core/arkfetch.go.
func arkFetchRefSchema() *orderedMap {
	m := objectSchema(
		"指向火山账号里已有的一把方舟 API Key。给了它就**不要**再给 `apiKey`。\n\n"+
			"两个 id 都从 `GET /api/ark-fetch/accounts/{accountId}/keys` 来。"+
			"明文由服务端现取，**不经过前端**。",
		requiredFields("accountId", "keyId"), map[string]any{
			"accountId": stringField("火山账号 id。"),
			"keyId":     stringField("那把 key 在火山侧的 id。"),
		})
	return m
}

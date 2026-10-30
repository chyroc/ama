// Entity schemas and a few builder helpers.
//
// Field descriptions are carried over from the comments in model/apimodel, focusing on the **counterintuitive** ones: which keys are [] when empty
// rather than missing, which 0s are valid values, what `requires_action` really means. Only by reading these
// will agents not misjudge, and those misjudgments are all of the "no error, just wrong behavior" kind.

package openapi

// ---------------------------------------------------------------------------
// helper
// ---------------------------------------------------------------------------

// typeSchema is the simplest one: only type.
func typeSchema(t string) *orderedMap {
	m := newOrderedMap()
	m.set("type", t)
	return m
}

// schemaRef references one entry under components.schemas.
func schemaRef(name string) *orderedMap {
	m := newOrderedMap()
	m.set("$ref", "#/components/schemas/"+name)
	return m
}

// arrayOf is "an array of some entity".
func arrayOf(name string) *orderedMap {
	m := newOrderedMap()
	m.set("type", "array")
	m.set("items", schemaRef(name))
	return m
}

func stringField(desc string) *orderedMap {
	m := newOrderedMap()
	m.set("type", "string")
	m.set("description", desc)
	return m
}

func intField(desc string) *orderedMap {
	m := newOrderedMap()
	m.set("type", "integer")
	m.set("description", desc)
	return m
}

func boolField(desc string) *orderedMap {
	m := newOrderedMap()
	m.set("type", "boolean")
	m.set("description", desc)
	return m
}

func stringArrayField(desc string) *orderedMap {
	m := newOrderedMap()
	m.set("type", "array")
	m.set("description", desc)
	m.set("items", typeSchema("string"))
	return m
}

func enumField(desc string, values ...string) *orderedMap {
	m := newOrderedMap()
	m.set("type", "string")
	m.set("description", desc)
	vals := make([]any, 0, len(values))
	for _, v := range values {
		vals = append(vals, v)
	}
	m.set("enum", vals)
	return m
}

func refField(name, desc string) *orderedMap {
	m := newOrderedMap()
	m.set("$ref", "#/components/schemas/"+name)
	m.set("description", desc)
	return m
}

func arrayField(itemRef, desc string) *orderedMap {
	m := newOrderedMap()
	m.set("type", "array")
	m.set("description", desc)
	m.set("items", schemaRef(itemRef))
	return m
}

// requiredFields is the required array; when nil objectSchema does not write this key.
func requiredFields(names ...string) []string { return names }

// objectSchema builds an object schema.
//
// **The keys of properties must be output sorted**: the input is a map, and without sorting the field order differs on every generation,
// so reconciliation (an empty diff after regenerating) would fail forever, with a failure message pointing to "the spec is outdated",
// not pointing to the real cause at all.
func objectSchema(desc string, required []string, props map[string]any) *orderedMap {
	m := newOrderedMap()
	m.set("type", "object")
	if desc != "" {
		m.set("description", desc)
	}
	if len(required) > 0 {
		vals := make([]any, 0, len(required))
		for _, r := range required {
			vals = append(vals, r)
		}
		m.set("required", vals)
	}
	p := newOrderedMap()
	for _, key := range sortedKeys(props) {
		p.set(key, props[key])
	}
	m.set("properties", p)
	return m
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Insertion sort: there are only a dozen or so fields, not worth the comparator closure of the sort package.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// components
// ---------------------------------------------------------------------------

func componentsObject() *orderedMap {
	comp := newOrderedMap()
	comp.set("securitySchemes", securitySchemes())
	// Query parameters are extracted as reusable definitions (the MA shape); see collectQueryParams in requests.go.
	if params := collectQueryParams(); params.len() > 0 {
		comp.set("parameters", params)
	}
	schemas := newOrderedMap()
	for _, name := range schemaOrder {
		schemas.set(name, entitySchemas[name])
	}
	comp.set("schemas", schemas)
	return comp
}

func securitySchemes() *orderedMap {
	bearer := newOrderedMap()
	bearer.set("type", "http")
	bearer.set("scheme", "bearer")
	bearer.set("description",
		"`Authorization: Bearer <token>`。两种 token 都走这里：\n\n"+
			"- **API Key**（`amak_` 开头）：在空间设置页创建，给 agent / 脚本 / CI 用。"+
			"它**继承创建者本人的权限**，不是独立作用域。\n"+
			"- **会话 token**：走本地账号或 OIDC 登录拿到的，浏览器放在 HttpOnly Cookie 里。\n\n"+
			"库里只存 token 的 sha256，所以吊销是真的能吊销。")

	cookie := newOrderedMap()
	cookie.set("type", "apiKey")
	cookie.set("in", "cookie")
	cookie.set("name", "ama_session")
	cookie.set("description", "浏览器的会话 Cookie（HttpOnly）。SSE 走它——`EventSource` 不能带自定义头。")

	m := newOrderedMap()
	m.set("bearerAuth", bearer)
	m.set("sessionCookie", cookie)
	return m
}

// schemaOrder is the output order of components.schemas.
//
// In an "outside in" reading order rather than alphabetical: workspace → member → Agent → skill → credential → run →
// event. Reading through once, an agent builds up the data model.
var schemaOrder = []string{
	"Error",
	"Workspace", "WorkspaceMember",
	"Agent", "AgentDraft", "ModelConfig",
	"Skill", "SkillDraft", "SkillFile",
	"Issue", "IssueDraft", "IssueComment", "IssueAttachment",
	"Suggestion", "SuggestionDraft", "SuggestionReply", "SuggestionAttachment",
	"SuggestionPage",
	"ArkCredential",
	"APIKey", "APIKeyCreated",
	"IntegrationState", "LarkUserApp",
	"FeedbackReport", "FeedbackCall",
	"Run", "RunEvent", "RunThread", "TokenUsage", "UsageSummary", "UsageBucket",
	"Schedule", "ScheduleDraft", "ScheduleSpec", "DeliveryTarget",
	"FeishuApp", "FeishuRegistration",
}

// agentProps are the fields of Agent and AgentDraft.
//
// One shared builder instead of writing it twice: the business fields of the two are exactly the same; Agent only adds
// the server-generated id / workspaceId / version / timestamps. Writing it twice would surely drift,
// and the symptom of drift is "the spec says there is this field, but sending it is ignored", with no error.
func agentProps(full bool) map[string]any {
	props := map[string]any{
		"name":        stringField("Agent 名字。"),
		"description": stringField("描述。"),
		"system":      stringField("系统提示词（方舟字段名就是 `system`）。每个 Agent 独立。"),
		"model":       refField("ModelConfig", "绑定的模型及参数。"),
		"builtin_tools": stringArrayField("启用的内置工具。六个：`bash` / `read` / `write` / " +
			"`edit` / `glob` / `grep`。\n\n" +
			"**它们全部在同一个容器里执行、共享一个文件系统**——所以 bash 写的文件 read 读得到。" +
			"空的时候是 `[]`。"),
		"skillIds": stringArrayField("引用的技能 id。\n\n" +
			"⚠️ **是引用不是拷贝**：技能内容改了，绑定它的 Agent 立即生效。空的时候是 `[]`。"),
		"feishuAppIds": stringArrayField("绑定的飞书应用 id。\n\n" +
			"⚠️ **一个飞书应用最多只能绑一个 Agent**，抢占别人已绑的会得到 409。空的时候是 `[]`。"),
		"status": enumField("启用状态（AMA 自有，方舟侧没这个概念）。\n\n"+
			"⚠️ **`disabled` 是真的会拦住**：停用的 Agent 不能发起新对话、不能在已有会话里"+
			"发消息（422）、飞书里会回一句提示而不是回答、定时任务到点会失败并记下原因。"+
			"历史照常可查，中断也照常放行。\n\n"+
			"**缺失或空串按 `active` 处理**（存量 Agent 没有这个字段）。",
			"active", "disabled"),
	}
	if full {
		props["id"] = stringField("Agent id。")
		props["workspaceId"] = stringField("所属空间。")
		props["version"] = intField("方舟的乐观锁。**更新必须回传当前 version**，每次 update 加一。" +
			"（**是数字**，与 `Skill.latestVersion` 是字符串刚好相反。）")
		props["createdAt"] = stringField("创建时刻。")
		props["updatedAt"] = stringField("更新时刻。")
		addActorProps(props)
	}
	return props
}

// addActorProps adds the two fields "who created it / who changed it" to an entity's properties.
//
// It is extracted because for four entities (Agent / Workspace / Skill / Schedule) these two fields **mean exactly
// the same thing**, and the most important sentence in the description is exactly "the key may be absent": writing it separately each time would diverge sooner or later,
// and divergence shows up as agents guessing the same field from different descriptions.
//
// ⚠️ **Neither goes into requiredFields**: existing rows do not have these two keys, and marking them required would make callers who
// generate validation from the spec judge normal data as invalid.
func addActorProps(props map[string]any) {
	props["createdBy"] = stringField("创建者的员工邮箱。\n\n" +
		"⚠️ **这个键可能整个不存在**，那是合法的长期状态，含义是「不知道是谁建的」" +
		"——本字段是后加的，在那之前建的数据没记过，且无从考据。" +
		"渲染成「未记录创建者」而不是空白或报错，**别拿空间 owner 之类去猜**。")
	props["updatedBy"] = stringField("最近一次改动者的员工邮箱。\n\n" +
		"与 `createdBy` 同理**可能不存在**。⚠️ 两者是两个字段：" +
		"`createdBy` 不会被后来的改动者顶掉。\n\n" +
		"**机器路径刻意不留痕**（定时任务到点重排、技能自动更新巡检、级联清理），" +
		"所以它只反映人做的那些改动。")
}

// withActorProps is the expression form of addActorProps, convenient to apply directly on a map literal.
func withActorProps(props map[string]any) map[string]any {
	addActorProps(props)
	return props
}

var entitySchemas = map[string]any{
	"Error": objectSchema(
		"错误信封。失败一律是这个形状。\n\n"+
			"⚠️ **判断要看 `code` 不看状态码**：路由写错时标准库返回的 404 是 text/plain，"+
			"按状态码判会把「路由挂错了」当成「资源不存在」。",
		requiredFields("message", "code"), map[string]any{
			"message": stringField("给人看的中文句子，可直接展示。"),
			"code": enumField("给程序判断的稳定标识。",
				"unauthenticated", "access_denied", "not_found", "invalid_argument",
				"conflict", "precondition_failed", "internal",
				// The two 422 detail codes when saving an Ark credential (the HTTP status is still 422):
				// a bad key (no way out) and an account not activated (can be saved after confirmNotActivated).
				"ark_key_invalid", "ark_managed_agent_not_open"),
		}),

	"Workspace": objectSchema("空间：一切资源的归属边界，没有「全局 Agent」。",
		requiredFields("id", "name", "key", "createdAt", "members"), withActorProps(map[string]any{
			"id":   stringField("形如 `ws_example`。**接口里用这个**。"),
			"name": stringField("展示名。"),
			"key": stringField("URL 标识，全局唯一，`[a-z0-9-]{2,32}`。\n\n" +
				"⚠️ **与 `id` 是两个不同的东西**：前端 URL 里是 key，接口参数要 id。" +
				"用 `GET /api/workspace-by-key` 换。"),
			"description": stringField("可选。清空时这个键会**整个消失**，不是空串。"),
			"createdAt":   stringField("ISO 8601，固定三位小数（`2026-08-25T10:00:00.000Z`）。"),
			"members":     arrayField("WorkspaceMember", "成员。**必填且必然是数组**，空的时候是 `[]`。"),
		})),

	"WorkspaceMember": objectSchema("空间成员。",
		requiredFields("id", "name", "email", "role", "joinedAt"), map[string]any{
			"id":        stringField("成员记录的 id。"),
			"name":      stringField("展示名。"),
			"email":     stringField("员工邮箱，**这就是 Actor ID**。比较时大小写不敏感。"),
			"role":      enumField("角色。owner 与 admin 权限面相同。", "owner", "admin", "member", "viewer"),
			"avatarUrl": stringField("头像地址，**只是快照会过时**。可能缺失。"),
			"joinedAt":  stringField("加入时刻。"),
		}),

	"Agent": objectSchema("一个智能体的定义。",
		requiredFields("id", "workspaceId", "name", "description", "system", "model",
			"version", "builtin_tools", "skillIds", "feishuAppIds", "status", "createdAt", "updatedAt"),
		agentProps(true)),
	"AgentDraft": objectSchema(
		"建 / 改 Agent 的入参。**改的时候传完整对象**（前端就是这么做的）。",
		requiredFields("name", "system", "model"), agentProps(false)),

	"ModelConfig": objectSchema(
		"绑定的模型及参数。\n\n"+
			"⚠️ 方舟的 Agent **没有** `temperature` / `max_tokens` / `top_p`，"+
			"推理强度由 `reasoning_effort` 与 `thinking` 表达。别按 OpenAI 的习惯造字段。",
		requiredFields("id"), map[string]any{
			"id":               stringField("模型 id。**字段名是 `id` 不是 `model`**（镜像方舟）。"),
			"thinking":         stringField("思考开关，可选。"),
			"reasoning_effort": stringField("推理强度，可选。"),
			"service_tier":     stringField("服务档位，可选。"),
		}),

	"Skill": objectSchema("空间级技能资产，可被多个 Agent 引用。",
		requiredFields("id", "workspaceId", "name", "displayTitle", "description",
			"instructions", "source", "latestVersion", "tags", "createdAt", "updatedAt"),
		withActorProps(map[string]any{
			"id":           stringField("技能 id。"),
			"workspaceId":  stringField("所属空间。"),
			"name":         stringField("技能名，`[a-z0-9-]` 且首字符非连字符。"),
			"displayTitle": stringField("展示标题。"),
			"description":  stringField("**决定模型何时调用它**，写得好不好直接影响命中率。"),
			"instructions": stringField("技能正文，即 `SKILL.md` 的内容。"),
			"source": enumField("来源。上传的技能也是 `custom`——上传是创建方式，不是来源。"+
				"`builtin` 是**平台内置技能**：所有空间都可见、可勾选，但正文由平台维护，改不了（PATCH/DELETE 回 403）；"+
				"要在它基础上改，用 `POST /api/workspaces/{workspaceId}/skills/fork` 复制一份。",
				"custom", "builtin"),
			"latestVersion": stringField("版本号，**是字符串不是数字**" +
				"（与 `Agent.version` 是数字刚好相反，容易写反）。"),
			"tags": stringArrayField("标签。空的时候是 `[]`。"),
			"files": arrayField("SkillFile", "附件（zip 上传才有）。\n\n"+
				"⚠️ **列表接口里没有 `content`**，只有文件名与大小——否则响应会随附件线性膨胀。"),
			"createdAt": stringField("创建时刻。"),
			"updatedAt": stringField("更新时刻。"),
		})),
	"SkillDraft": objectSchema("建 / 改技能的入参。", requiredFields("name", "displayTitle", "description", "instructions"),
		map[string]any{
			"name":         stringField("技能名，`[a-z0-9-]` 且首字符非连字符。"),
			"displayTitle": stringField("展示标题。"),
			"description":  stringField("决定模型何时调用它。"),
			"instructions": stringField("技能正文。"),
			"tags":         stringArrayField("标签。"),
		}),
	"SkillFile": objectSchema("技能包里除 `SKILL.md` 之外的附件。", requiredFields("path", "size"), map[string]any{
		"path":    stringField("相对技能根目录的路径，形如 `scripts/init.sh`。"),
		"size":    intField("字节数。**0 是合法值**。"),
		"content": stringField("文件内容（base64）。**只在解析预览与注入容器两条内部路径上有值**，列表接口里没有。"),
	}),

	"Issue": objectSchema(
		"空间级的问题记录，含评论与附件。\n\n"+
			"⚠️ **与 `/api/feedback` 不是同一件事。** 那个是错误发生时机器自动上报的流"+
			"（内容由白名单重新构造、跨空间、会去重、只有开发团队能读）；"+
			"这个是人手写的空间级实体（自由文本、有状态流转、能评论、会被改）。",
		requiredFields("id", "workspaceId", "title", "body", "status", "priority",
			"author", "createdAt", "updatedAt", "commentCount", "attachmentCount"),
		map[string]any{
			"id":          stringField("问题 id，`iss_` 开头。"),
			"workspaceId": stringField("所属空间。"),
			"title": stringField("标题。**服务端保证非空**——用户可以不填，" +
				"那时会从正文第一行提取（剥掉 Markdown 标记、图片取 alt、超长截断），" +
				"实在提不出来则用「未命名问题」兜底。"),
			"body":   stringField("Markdown 正文，可以是空串（只填标题是合法的）。"),
			"status": enumField("处理状态。", "open", "in_progress", "closed"),
			"priority": enumField("优先级。**不填落 `p2`**，不是 `p0`"+
				"——默认最高档会让每条新问题都在喊「最紧急」，那一档就失去意义了。",
				"p0", "p1", "p2"),
			"author": stringField("创建者邮箱。**只从登录身份取**，" +
				"请求体里没有这个字段（有的话就能以别人的名义提问题）。"),
			"createdAt": stringField("创建时刻。"),
			"updatedAt": stringField("最后一次改动时刻。改标题 / 正文 / 状态 / 优先级都会刷新它。"),
			"commentCount": intField("评论数。**派生字段，现算的**（不落库）。" +
				"不加 omitempty：0 是合法值，缺键会让界面算出 NaN。"),
			"attachmentCount": intField("附件数。同 `commentCount`，派生且 0 合法。"),
			"origin": enumField("谁产出的。**缺省（空）= 人手写的**，存量全部如此；"+
				"`schedule` 表示定时巡检任务产出。\n\n"+
				"⚠️ 它决定**去重的作用域**：人手写的问题永不去重"+
				"（两个人报同一件事就该是两条），而同一条巡检任务把同一批素材重扫 N 遍，"+
				"不合并只会把列表刷爆。两种来源之间永远不互相合并。",
				"", "schedule"),
			"fingerprint": stringField("同一个问题的判据，**只对机器产出的问题有意义**，" +
				"人手写的一律是空串。同空间 + 同 `origin` + 同指纹的第二次发现" +
				"会变成一条评论而不是一条新问题。"),
		}),

	"IssueDraft": objectSchema(
		"建 / 改问题的入参。\n\n"+
			"⚠️ **`title` 与 `body` 至少要有一个**，两个都空会被拒（400）。"+
			"两者都不是必填字段，但「都不填」不是一条有意义的记录。\n\n"+
			"改动时：`title`/`body` 传空串是**真的清空**（清空标题会重新从正文提取）；"+
			"`status`/`priority` 传空串或不传表示**这一项不改**。",
		requiredFields(),
		map[string]any{
			"title":    stringField("标题。不填则从 `body` 提取。"),
			"body":     stringField("Markdown 正文。"),
			"status":   enumField("处理状态。新建时不填落 `open`。", "open", "in_progress", "closed"),
			"priority": enumField("优先级。新建时不填落 `p2`。", "p0", "p1", "p2"),
		}),

	"IssueComment": objectSchema(
		"问题下的一条评论。**支持修改**，所以有 `updatedAt`。",
		requiredFields("id", "issueId", "workspaceId", "body", "author", "createdAt", "updatedAt"),
		map[string]any{
			"id":          stringField("评论 id，`cmt_` 开头。"),
			"issueId":     stringField("所属问题。"),
			"workspaceId": stringField("所属空间（从问题继承，**不从请求取**）。"),
			"body":        stringField("Markdown 正文。**不能为空**——空评论在时间线上就是个空气泡。"),
			"author":      stringField("作者邮箱，只从登录身份取。"),
			"createdAt":   stringField("发表时刻。"),
			"updatedAt":   stringField("最后一次编辑时刻。与 `createdAt` 不等即表示编辑过。"),
		}),

	"IssueAttachment": objectSchema(
		"问题的附件（图片或文档）的**元信息**。\n\n"+
			"⚠️ **不含内容字节**——一个问题最多挂 20 个附件、每个最大 5 MiB，"+
			"塞进列表响应会让一次加载搬运 100 MB。字节走 `url` 那条单独的 GET。",
		requiredFields("id", "issueId", "workspaceId", "filename", "contentType",
			"size", "author", "url", "createdAt"),
		map[string]any{
			"id":          stringField("附件 id，`att_` 开头。**URL 里就是它。**"),
			"issueId":     stringField("所属问题。"),
			"workspaceId": stringField("所属空间。**这是越权校验的依据**：访问附件要求你是这个空间的成员。"),
			"filename":    stringField("上传时的原始文件名，只用于显示与下载。**不参与路径拼接。**"),
			"contentType": stringField("**服务端嗅探出来的**类型，不是客户端声明的那个。" +
				"内容与扩展名对不上会在上传时被拒。"),
			"size":   intField("字节数。"),
			"author": stringField("上传者邮箱。"),
			"url": stringField("访问地址，形如 `/api/issue-attachments/att_x`。\n\n" +
				"**能直接塞进 `<img src>`**：同源请求会自动带上会话 Cookie。" +
				"⚠️ 但它**不是公开的**——要求你是该空间的成员，否则 403。"),
			"createdAt": stringField("上传时刻。附件不支持改内容（要换就删了重传），所以没有 `updatedAt`。"),
		}),

	"Suggestion": objectSchema(
		"对 **AMA 平台本身**的一条意见或建议。\n\n"+
			"⚠️ **这是第三种「有问题」的东西，三者都不该混。**\n\n"+
			"- `FeedbackReport`（`/api/feedback`）—— 机器采集的报错流，"+
			"内容由白名单重新构造、跨空间、会去重、用户看不到自己那条\n"+
			"- `Issue` —— 人手写的**空间级**待办，`workspaceId` 非空，空间成员按角色读\n"+
			"- 本类型 —— 人手写的产品意见，**平台级**\n\n"+
			"⚠️ **它没有 `workspaceId` 字段，那不是漏了**：用户是在给平台提意见，"+
			"不是在给某个空间提意见。留一个恒为空的字段会让人以为该按空间筛。\n\n"+
			"读权限：**提的人看自己的，平台方看全部**。",
		requiredFields("id", "title", "body", "status", "author",
			"createdAt", "updatedAt", "replyCount", "attachmentCount"),
		map[string]any{
			"id": stringField("建议 id，`sug_` 开头。"),
			"title": stringField("标题。**服务端保证非空**——用户可以不填，" +
				"那时会从正文第一行提取（与问题共用同一套提取规则）。"),
			"body":   stringField("Markdown 正文，可以是空串（只填标题是合法的）。"),
			"status": enumField("处理状态。**两态**，不是问题那样的三态。", "open", "resolved"),
			"author": stringField("提建议的人的邮箱。**只从登录身份取**，" +
				"请求体里没有这个字段。\n\n" +
				"**实名但不公开展示**：只有平台方看得到是谁——实名是回复与跟进的前提。"),
			"createdAt": stringField("提交时刻。"),
			"updatedAt": stringField("最后一次改动时刻。改标题 / 正文 / 状态都会刷新它。"),
			"replyCount": intField("回复数。**派生字段，现算的**（不落库）。" +
				"不加 omitempty：0 是合法值，缺键会让界面算出 NaN。"),
			"attachmentCount": intField("附件数。同 `replyCount`，派生且 0 合法。"),
		}),

	"SuggestionDraft": objectSchema(
		"提 / 改建议的入参。\n\n"+
			"⚠️ **`title` 与 `body` 至少要有一个**，两个都空会被拒（400）。\n\n"+
			"改动时：`title`/`body` 传空串是**真的清空**（清空标题会重新从正文提取），"+
			"但**两者同时为空**表示「这次不动它们」——那是为了让只改状态的 PATCH 走得通。"+
			"`status` 不传表示不改。",
		requiredFields(),
		map[string]any{
			"title": stringField("标题。不填则从 `body` 提取。"),
			"body":  stringField("Markdown 正文。"),
			"status": enumField("处理状态。新建时不填落 `open`。\n\n"+
				"⚠️ **只有平台方改得动这个字段**，作者本人改会 403："+
				"状态表达的是「平台方处理到哪一步了」。\n\n"+
				"认不出的值一律 400，**绝不默默改成 open**。", "open", "resolved"),
		}),

	"SuggestionReply": objectSchema(
		"一条建议下的回复。**平台方答复与用户追问走同一个类型**——"+
			"区分靠 `isStaff`，不是两种结构。",
		requiredFields("id", "suggestionId", "body", "author", "isStaff",
			"createdAt", "updatedAt"),
		map[string]any{
			"id":           stringField("回复 id，`sre_` 开头。"),
			"suggestionId": stringField("所属建议。"),
			"body":         stringField("Markdown 正文。"),
			"author":       stringField("作者邮箱。**只从登录身份取**。"),
			"isStaff": boolField("这条是不是平台方写的。\n\n" +
				"⚠️ **写入那一刻定下来并落库的**，不是读的时候拿 author 现比白名单——" +
				"否则有人调岗移出名单后，他当年那条官方答复会追溯性地变成用户留言。"),
			"origin": enumField("谁产出的。**缺省（空）= 人写的**，当前全部如此；"+
				"`agent` 表示由 agent 自动产出（为「以后让 agent 自动处理」预留）。\n\n"+
				"⚠️ 与 `Issue.origin` **用途不同**：那个决定去重作用域，"+
				"这个**不参与任何去重**（建议刻意不做去重）。", "", "agent"),
			"createdAt": stringField("发表时刻。"),
			"updatedAt": stringField("最后一次编辑时刻。与 `createdAt` 不等即表示编辑过。"),
		}),

	"SuggestionAttachment": objectSchema(
		"建议的附件（图片或文档）的**元信息**，不含内容字节。\n\n"+
			"形状与 `IssueAttachment` 几乎相同，**少一个 `workspaceId`**"+
			"——建议没有空间维度。",
		requiredFields("id", "suggestionId", "filename", "contentType",
			"size", "author", "url", "createdAt"),
		map[string]any{
			"id":           stringField("附件 id，`sat_` 开头。**URL 里就是它。**"),
			"suggestionId": stringField("所属建议。**这是越权校验的依据**：读权限跟着它走。"),
			"filename":     stringField("上传时的原始文件名，只用于显示与下载。**不参与路径拼接。**"),
			"contentType": stringField("**服务端嗅探出来的**类型，不是客户端声明的那个。" +
				"内容与扩展名对不上会在上传时被拒。"),
			"size":   intField("字节数。"),
			"author": stringField("上传者邮箱。"),
			"url": stringField("访问地址，形如 `/api/suggestion-attachments/sat_x`。\n\n" +
				"**能直接塞进 `<img src>`**：同源请求会自动带上会话 Cookie。" +
				"⚠️ 但它**不是公开的**——读权限与所属建议一致（提的人 + 平台方），否则 403。"),
			"createdAt": stringField("上传时刻。附件不支持改内容，所以没有 `updatedAt`。"),
		}),

	"SuggestionPage": objectSchema(
		"建议列表的一页。\n\n"+
			"⚠️ 它不是裸数组——除了 `suggestions`，还带着**你是不是平台方**这个判断，"+
			"调用方要靠它决定渲染哪一套界面。",
		requiredFields("suggestions", "limit", "offset", "viewerIsStaff"),
		map[string]any{
			"suggestions": arrayOf("Suggestion"),
			"limit":       intField("这一页最多几条（缺省 50，上限 200）。"),
			"offset":      intField("跳过了多少条。"),
			"viewerIsStaff": boolField("这次调用的人是不是平台方。\n\n" +
				"**由服务端算而不是调用方猜**：平台方名单是服务端配置，调用方看不到。" +
				"猜的结果是「界面上有个改状态的按钮，点下去 403」。"),
		}),

	"ArkCredential": objectSchema(
		"空间级的方舟凭证，**对外视图**——明文 key 永远不在这里。\n\n"+
			"⚠️ 未配置时接口回 **200 + 空壳**（`apiKeyMasked` 为空串），不是 404。",
		requiredFields("workspaceId", "apiKeyMasked", "region", "check"), map[string]any{
			"workspaceId":  stringField("所属空间。"),
			"apiKeyMasked": stringField("掩码后的 key，形如 `abcd****...****wxyz`。**未配置时是空串**。"),
			"region":       enumField("方舟区域。", "cn-beijing", "ap-southeast-1"),
			"check": enumField("校验状态。方舟没有「测 key」的专用接口，实现是拿这个 key 调一次只读接口看响应码。",
				"unchecked", "valid", "invalid"),
			"checkedAt":  stringField("上次校验时刻。改区域会把它**删掉**（不是置空）。"),
			"checkError": stringField("上次校验的失败原因。"),
			"updatedAt":  stringField("上次改动时刻。"),
			"updatedBy":  stringField("上次改动者的邮箱（凭证类配置要留痕）。"),
			"warning": objectSchema("**只在保存（PUT）响应里出现的易失提示，不入库、GET 不带**。\n\n"+
				"保存时那次在线校验没验成（方舟超时/5xx）时，key 已照常保存，但带这一条告诉用户"+
				"「没验成、可稍后点验证连通性」。",
				requiredFields("kind", "message"), map[string]any{
					"kind":    enumField("警告类型。", "verify_unavailable"),
					"message": stringField("直接展示给用户的中文句子。"),
				}),
		}),

	"APIKey": objectSchema(
		"一把 API Key 的**对外视图**。明文永远不在这里——库里只有 sha256。",
		requiredFields("id", "name", "prefix", "createdAt"), map[string]any{
			"id":        stringField("key 记录的 id，吊销时用它。**不是 key 本身**。"),
			"name":      stringField("创建时起的名字，便于分辨。"),
			"prefix":    stringField("明文的前几位（形如 `amak_a1b2c3`），**仅供在列表里认出是哪一把**。"),
			"createdAt": stringField("创建时刻。"),
			"lastUsedAt": stringField("最近一次用它调接口的时刻。\n\n" +
				"**缺失表示从来没用过**——据此能发现哪把 key 可以吊销了。"),
		}),
	"APIKeyCreated": objectSchema(
		"刚创建的 API Key。\n\n"+
			"⚠️ **`key` 是明文，只在这一次响应里出现，之后再也拿不到**。没存下来只能吊销重建。",
		requiredFields("id", "name", "prefix", "key", "createdAt"), map[string]any{
			"id":     stringField("key 记录的 id。"),
			"name":   stringField("名字。"),
			"prefix": stringField("明文的前几位。"),
			"key": stringField("**明文 key，形如 `amak_...`。就是这一次。** " +
				"用法：`Authorization: Bearer <key>`。"),
			"createdAt": stringField("创建时刻。"),
		}),

	"FeedbackCall": objectSchema(
		"一次接口调用的摘要，出现在**二级**反馈的 `payload.recent` 里。\n\n"+
			"⚠️ **这是白名单**：没有请求体、没有响应体、没有请求头，也**没有 query 部分**"+
			"（那里可能有 token）。只有「打了哪条接口、结果如何」。",
		requiredFields("method", "path"), map[string]any{
			"method": stringField("HTTP 方法。"),
			"path":   stringField("接口路径，**id 已抹成占位符、query 已砍掉**。"),
			"status": intField("HTTP 状态码。"),
			"code":   stringField("后端错误码，成功时为空。"),
			"logId": stringField("那次请求的服务端 logid。**没有响应的那几次（网络故障）不带这个键。**\n\n" +
				"它恰恰能救「连不上服务端」那个场景：当次失败自己没有 logid（请求没到达），" +
				"但如果用户是先正常用了一会儿、后端才挂的，那前面几次**到达过**服务端的失败" +
				"各自带着真 logid——顺着它们能定位到后端挂掉的时刻。"),
		}),

	"FeedbackReport": objectSchema(
		"一条问题反馈。\n\n"+
			"⚠️ **两级的 `payload` 字段集合不同**，这是采集边界而不是巧合：\n\n"+
			"- `auto`（自动记录，用户没点任何东西）：`payload` 只有 `message` 与 `appVersion`。"+
			"**不含任何能定位到具体业务对象的标识**——没有完整路径、没有 agentId、"+
			"没有接口调用轨迹、没有控制台错误。\n"+
			"- `user`（用户主动提交）：上面那些再加 `path`（含 id）、`agentId`、"+
			"`recent`、`consoleErrors`。（顶层的 `logId` **两级都有**，且不在 payload 里。）\n\n"+
			"两级都**绝不含** token / cookie / API Key / Authorization / 接口响应体 / 用户消息正文。",
		requiredFields("id", "level", "actor", "routePattern", "httpStatus", "errorCode",
			"errorKind", "userAgent", "payload", "createdAt", "lastSeenAt", "occurrences"),
		map[string]any{
			"id":    stringField("反馈记录的 id（`fb_` 开头）。"),
			"level": enumField("哪一级。决定了 `payload` 里有哪些字段。", "auto", "user"),
			"actor": stringField("遇到问题的人（员工邮箱）。"),
			"workspaceId": stringField("出问题时所在的空间 id。" +
				"**可能不存在**——错误可能发生在空间之外（空间列表页、偏好设置、还没加入任何空间的新人）。"),
			"routePattern": stringField("出错的路由，**id 已抹成占位符**。" +
				"低基数，所以 `group by` 它才有意义（「哪个页面出错最多」）。"),
			"httpStatus": intField("HTTP 状态码。**0 是合法值**（网络故障根本没有响应），所以这个键一定存在。"),
			"errorCode":  stringField("后端错误码。判因价值高于状态码——403 有多种成因。"),
			"errorKind":  stringField("前端错误分类（`denied` / `other`）。"),
			"userAgent":  stringField("浏览器概要，如 `Chrome 120 / macOS`。**不是原始 UA 串**（那有指纹价值）。"),
			"logId": stringField("服务端那次请求的 logid（响应头 `X-TT-Logid`）。" +
				"拿着它能在日志平台上搜到那一次请求的全部日志行。\n\n" +
				"**两级都可能有**（它不需要用户同意：服务端自己生成的串，不含用户输入）。\n\n" +
				"但**两级都可能没有**：`network_error` 那类失败下请求从未到达服务端，" +
				"根本没有 logid 被生成。缺键即表示「这次没有服务端日志」。"),
			"payload": objectSchema("其余明细。**服务端按白名单重建，不是请求体原文。**"+
				"字段集合随 `level` 变，见本 schema 的说明。", nil, map[string]any{}),
			"createdAt": stringField("**首次**记下这条的时刻。"),
			"lastSeenAt": stringField("**最近一次**撞到这条的时刻。\n\n" +
				"一级去重后 `[createdAt, lastSeenAt]` 是这个错误的持续区间；二级两者相等。"),
			"occurrences": intField("这条被撞了几次。\n\n" +
				"⚠️ **一级会去重**（同一人 + 同一路由 + 同一错误码 + 同一状态码 + 同一个十分钟窗口 " +
				"→ 一行），所以「发生了 312 次」体现在这个字段上，**不在行数上**。" +
				"按行数统计量级必然低报。二级恒为 1。"),
		}),

	"IntegrationState": objectSchema(
		"我在某个外部平台上的授权状态。**账号级，与空间无关**——同一个人在任何空间看到的都一样。",
		requiredFields("provider", "status"), map[string]any{
			"provider":    stringField("源标识，形如 `ama` / `lark-user`。"),
			"displayName": stringField("展示名。"),
			"status": enumField("状态。**`unknown` 是必须存在的一档**——把网络抖动当成「已失效」会让用户反复重授权。",
				"authorized", "unauthorized", "expired", "missing", "error", "unknown"),
			"authorizedAt": stringField("授权时刻。"),
			"expiresAt":    stringField("过期时刻。"),
			"message":      stringField("给人看的说明（未接入时是配置指引）。"),
		}),

	"LarkUserApp": objectSchema(
		"「飞书用户授权」可以选的一个飞书应用。\n\n"+
			"⚠️ **一个人可能有好几个**，而 **token 与应用是绑定的**：拿 A 应用的 token 配 "+
			"B 应用的 app_id，lark-cli 回 `99991668 token invalid`；即便侥幸有效，"+
			"两个应用的可见范围也不同（读不到那个群）。所以「当前用哪个」是有后果的。\n\n"+
			"⚠️ **这里没有任何凭据**：app_id 与应用名不是秘密（前者在飞书群里人人可见），"+
			"app_secret 则**没有对应字段**。",
		requiredFields("appId", "authorized", "current"), map[string]any{
			"appId": stringField("飞书应用 id，形如 `cli_xxx`。**这是选择时用的 key**。"),
			"name": stringField("应用展示名。\n\n" +
				"⚠️ **它不足以区分**：实测同一个人的三个应用全叫「AMA 助手」，" +
				"所以界面上要连 `appId` 一起显示。"),
			"workspaceName": stringField("这个应用属于哪个空间（展示名）。名字重复时" +
				"这一列往往是唯一分得清的依据。"),
			"authorized": boolField("这个应用**授权过没有**。\n\n" +
				"逐个应用算，不是全局一个布尔——库里那张表主键是 `(actor, app_id)`，" +
				"天然支持一人多应用多把 token。"),
			"current": boolField("是不是**当前默认**使用的那个。\n\n" +
				"⚠️ 「当前」指的是账号级默认值（卡片展示、发起/解除授权用）。" +
				"agent 跑起来时注入哪个应用**按运行所属空间定**，不看这个值。"),
		}),

	"Run": objectSchema(
		"一次运行，对应方舟的一个 Session。\n\n"+
			"**一次对话也是一条 Run**（`trigger: \"manual\"`）——对话与运行历史是同一份数据的"+
			"两种读法，消息是这条 Run 上的事件。见 /llms.txt 的数据模型一节。",
		requiredFields("id", "workspaceId", "agentId", "agentName", "title", "status",
			"trigger", "startedAt", "usage", "threads", "eventCount"), map[string]any{
			"id":          stringField("Run 的 id。"),
			"workspaceId": stringField("所属空间。"),
			"agentId":     stringField("哪个 Agent 在跑。"),
			"agentName":   stringField("Agent 名字的**快照**（Agent 被删后仍要说得出这是谁）。"),
			"title":       stringField("标题。"),
			"status": enumField(
				"状态。\n\n"+
					"⚠️ **`requires_action` 在本平台不是「等用户确认」**，而是"+
					"「**工具正在我们的容器里执行**」——环境类型是 self_hosted，方舟只做推理，"+
					"工具调用由我们的 worker 领走执行。**不需要任何人介入，等着就好**。"+
					"把它当成「等人确认」会让 agent 卡在那里等一个永远不会来的输入。",
				"running", "idle", "requires_action", "rescheduling", "terminated"),
			"trigger": enumField("触发来源。对话是 `manual`。",
				"manual", "feishu", "api", "schedule", "webhook"),
			"triggerDetail": stringField("如飞书群名、调用方标识。"),
			"startedAt":     stringField("开始时刻。"),
			"lastActivityAt": stringField("最后一条事件的时刻。\n\n" +
				"⚠️ **列表按它排序，不是 `startedAt`**：飞书会话会被复用，" +
				"只按 startedAt 排的话刚说完话的会话还躺在列表底部。老数据没有这个键。"),
			"endedAt":      stringField("结束时刻。**未结束时这个键不存在**。"),
			"durationMs":   intField("耗时毫秒。未结束时不存在（用缺失而不是 0 来区分）。"),
			"usage":        refField("TokenUsage", "token 用量汇总。"),
			"threads":      arrayField("RunThread", "线程。多 Agent 协作时子 agent 各占一个。"),
			"eventCount":   intField("事件数。"),
			"errorSummary": stringField("出错原因，只在失败的运行上有。"),
			"agentDeleted": boolField("关联的 Agent 已被删除、这个会话已冻结（不能再发消息）。" +
				"**派生字段，不落库**；缺失等价于 false。"),
			"agentDisabled": boolField("关联的 Agent 已被**停用**，这个会话同样只能查看历史。\n\n" +
				"与 `agentDeleted` 分成两个字段：两者要用户做的事不同（一个去改绑或新建，" +
				"一个把状态改回「已启用」）。**停用是可逆的**，所以这个值会随 Agent 状态来回变。\n\n" +
				"**派生字段，不落库**；缺失等价于 false。中断（`user.interrupt`）照常放行。"),
		}),

	"RunEvent": objectSchema(
		"一条会话事件。**这就是对话内容**——用户的话与 agent 的回复都是事件。\n\n"+
			"共 33 类，结构直接来自方舟、本平台原样透传，所以这里**不穷举字段**"+
			"（各变体的载荷差别很大，甚至同一个键在不同变体里的位置都不同）。\n\n"+
			"发消息只需要最简单的那一种：\n\n"+
			"```json\n"+
			"{\"type\": \"user.message\", \"content\": [{\"type\": \"text\", \"text\": \"你好\"}]}\n"+
			"```\n\n"+
			"⚠️ 方舟**只接受 7 种 `user.*` 事件**，`agent.*` 的发不进去。",
		requiredFields("type"), map[string]any{
			"id":       stringField("方舟发的事件 id（`sevt-...` / `call_...`）。"),
			"type":     stringField("事件类型，形如 `user.message` / `agent.message` / `agent.tool_use`。"),
			"sequence": intField("会话内单调递增的序号。列表按它升序。"),
			"session_thread_id": stringField("所属线程。**会话级事件（`session.status_*`）是 null**，" +
				"所以这个键是「可空」而不是「可选」。"),
			"processed_at": stringField("方舟处理时刻。"),
			"content": func() any {
				m := newOrderedMap()
				m.set("type", "array")
				m.set("description", "内容块。文本消息是 `[{\"type\":\"text\",\"text\":\"...\"}]`。")
				m.set("items", typeSchema("object"))
				return m
			}(),
		}),

	"RunThread": objectSchema("一次运行里的一个线程。",
		requiredFields("id", "agent_name", "status", "parent_thread_id", "startedAt"), map[string]any{
			"id":         stringField("对应方舟的 `session_thread_id`。"),
			"agent_name": stringField("这个线程属于哪个 Agent。"),
			"status":     stringField("线程状态。"),
			"parent_thread_id": stringField("父线程。**null 表示主线程，但这个键必须存在**" +
				"（可空而非可选）。"),
			"startedAt": stringField("开始时刻。"),
			"endedAt":   stringField("结束时刻。未结束时这个键不存在。"),
		}),

	"TokenUsage": objectSchema(
		"token 用量。**四个字段都必填、都不能缺**——0 是合法值，缺键会让调用方算出 NaN。",
		requiredFields("input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"),
		map[string]any{
			"input_tokens":                intField("输入 token。"),
			"output_tokens":               intField("输出 token。"),
			"cache_read_input_tokens":     intField("命中缓存的输入 token。"),
			"cache_creation_input_tokens": intField("写入缓存的输入 token。"),
		}),

	"UsageSummary": objectSchema(
		"用量聚合。窗口**左闭右开** `[since, until)`——闭区间会让边界上的 run 落进两个桶，"+
			"于是「每天加起来」比总数多，而两个数都长得合理。",
		requiredFields("total", "buckets"), map[string]any{
			"total":   refField("TokenUsage", "窗口内合计。"),
			"runs":    intField("运行次数。"),
			"buckets": arrayField("UsageBucket", "按天分桶。"),
		}),
	"UsageBucket": objectSchema("一天的用量。", requiredFields("date", "usage"), map[string]any{
		"date":  stringField("日期（`YYYY-MM-DD`）。"),
		"usage": refField("TokenUsage", "这一天的用量。"),
		"runs":  intField("这一天的运行次数。"),
	}),

	"Schedule": objectSchema("一条定时任务。",
		requiredFields("id", "workspaceId", "agentId", "agentName", "name", "spec",
			"prompt", "origin", "status", "delivery", "runCount", "failCount",
			"consecutiveFailures", "createdAt", "updatedAt"), withActorProps(map[string]any{
			"id":          stringField("任务 id。"),
			"workspaceId": stringField("所属空间。"),
			"agentId":     stringField("跑哪个 Agent。"),
			"agentName":   stringField("Agent 名字的快照。"),
			"name":        stringField("任务名。"),
			"spec":        refField("ScheduleSpec", "什么时候触发。"),
			"prompt":      stringField("到点喂给 Agent 的指令。"),
			"contextSummary": stringField("建任务时的上下文摘要，触发时**拼在 prompt 前面**。\n\n" +
				"存在的理由：每次触发都是全新会话、零历史。"),
			"origin":   enumField("谁建的。当前只有 `user`（人在界面上建的）。", "user"),
			"status":   enumField("状态。连续失败到上限会自动 `paused`。", "active", "paused"),
			"delivery": refField("DeliveryTarget", "结果往哪送，建任务时**冻结**下来。"),
			"nextRunAt": stringField("下次触发时刻。**暂停时为空；被认领的那一刻也会被置空**" +
				"（否则一轮跑几分钟期间会被反复重复触发）。"),
			"runNowRequested": boolField("有人点了 `run-now`，这一轮触发是人工要的，" +
				"**调度器认领的那一刻就会被消费掉**（一次点击只换一轮）。\n\n" +
				"存在的理由：认领只领 `active` 的任务，而「先配好、点一下看看对不对、再启用」" +
				"要求 `paused` 的也能试跑一次。少了这个标记，run-now 对 `paused` 会返回 200 " +
				"却**零效果**（`nextRunAt` 更新了，而调度器永远不认领它）。\n\n" +
				"**试跑不改 `status`**：跑完仍是 `paused` 就不排下一次，所以试跑不会把" +
				"暂停的任务变成周期任务。"),
			"lastRunAt":           stringField("上次触发时刻。"),
			"lastRunId":           stringField("上次触发产生的那条 Run。"),
			"lastError":           stringField("上次失败原因，成功时清空。"),
			"runCount":            intField("累计触发次数。**0 是合法值**。"),
			"failCount":           intField("累计失败次数。"),
			"consecutiveFailures": intField("连续失败次数，到上限自动暂停。"),
			"createdAt":           stringField("创建时刻。"),
			"updatedAt":           stringField("更新时刻。"),
			"agentDeleted":        boolField("关联 Agent 已删，这条任务再也跑不出结果。派生字段，不落库。"),
		})),
	"ScheduleDraft": objectSchema("建 / 改定时任务的入参。",
		requiredFields("agentId", "name", "spec", "prompt"), map[string]any{
			"agentId":        stringField("跑哪个 Agent。"),
			"name":           stringField("任务名。"),
			"spec":           refField("ScheduleSpec", "什么时候触发。"),
			"prompt":         stringField("到点喂给 Agent 的指令。"),
			"contextSummary": stringField("上下文摘要，可选。"),
			"delivery":       refField("DeliveryTarget", "结果往哪送。缺省 `run_only`。"),
		}),
	"ScheduleSpec": objectSchema("触发方式。三种，字段按 `kind` 取用。",
		requiredFields("kind"), map[string]any{
			"kind": enumField("哪一种。", "cron", "interval", "oneshot"),
			"cron": stringField("5 段表达式，`kind=cron` 时必填。"),
			"tz": stringField("cron 的解释时区（IANA 名）。留空是 `Asia/Shanghai`——" +
				"「工作日 9:30」写下来就该是本地时间。**只对 cron 有意义**。"),
			"everyMs": intField("间隔毫秒，`kind=interval` 时必填。\n\n" +
				"⚠️ **从上一轮跑完起算，不是固定网格**：按网格排会在慢的那轮之后立刻追一次，把间隔吃掉。"),
			"fireAt": stringField("触发时刻，`kind=oneshot` 时必填。**过期也照跑一次，跑完即删**。"),
		}),
	"DeliveryTarget": objectSchema(
		"结果的投递地址，建任务时**冻结**——到点时是全新会话，不冻结就只能让 agent 猜地址，"+
			"而它猜错不报错，只是把报表发到了别的群。\n\n"+
			"刻意**不存任何凭证**：只记 `feishuAppId`，触发时现场换 token。",
		requiredFields("kind"), map[string]any{
			"kind": enumField("往哪送。`run_only` 是所有任务的公共底座（Run 总会落下来）。\n\n"+
				"⚠️ 曾经有第三个值 `issue`（服务端解析 agent 输出的 JSON 数组代建问题），"+
				"**已删除**：那个契约太脆，一次 JSON 格式失误就让整轮巡检白跑。"+
				"巡检现在走 `run_only`，由 agent 自己调 `ama issue create` / "+
				"`ama issue comment`，判重靠标题里的固定标识前缀。",
				"run_only", "feishu"),
			"feishuAppId": stringField("用哪个飞书应用发，`kind=feishu` 时必填。"),
			"chatID":      stringField("飞书会话 id（`oc_` 开头）。"),
			"threadId":    stringField("非空表示这条任务来自一个话题。"),
			"anchorMessageId": stringField("投递时用来回复的那条消息（`om_` 开头）。\n\n" +
				"⚠️ **有它才能发进话题**：飞书的发消息接口只收 chat_id，" +
				"要落进话题必须回复话题里的某条消息。为空则退化成直发到会话。"),
			"chatName": stringField("群名快照，**纯展示**。会过时，不要用它做判断。"),
		}),

	"FeishuApp": objectSchema("一个已接入的飞书应用。",
		requiredFields("id", "workspaceId", "appId", "name", "boundAgentIds"), map[string]any{
			"id":          stringField("应用记录的 id。"),
			"workspaceId": stringField("所属空间。"),
			"appId":       stringField("飞书的 app_id，形如 `cli_xxx`。"),
			"name":        stringField("应用名。"),
			"boundAgentIds": stringArrayField("绑定它的 Agent id。\n\n" +
				"⚠️ **一个飞书应用最多只能绑一个 Agent**（一条入站消息只能有一个回答者）。" +
				"**派生字段，不落库**；必然是数组，空的时候是 `[]`。"),
			"readiness": stringField("权限与配置巡检结果。"),
		}),
	"FeishuRegistration": objectSchema("一次飞书应用自助注册流程（设备码）。",
		requiredFields("sessionId", "status"), map[string]any{
			"sessionId":       stringField("轮询用的 id。"),
			"status":          stringField("进度。"),
			"verificationUrl": stringField("授权页地址，给用户点。"),
			"reason":          stringField("失败原因，中文句子。"),
		}),
}

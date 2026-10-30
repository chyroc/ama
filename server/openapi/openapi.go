// Package openapi describes AMA's HTTP API as an OpenAPI 3.1 document, for **external agents** to read.
//
// # The source of truth is authz/matrix.go, not this file
//
// The route part of the spec is generated **entirely** from `authz.Routes()`; not a single route is hand-written. The reason is not convenience;
// a hand-written spec always drifts: a route gets renamed, changes method, or one more is added, and nothing reminds you to
// change the yaml. An agent holding an outdated spec keeps calling the wrong endpoints, **and the errors do not point to the real cause**:
// it sees 404 / 403, while the real cause is "this endpoint's path changed last month".
//
// Generating from the matrix never drifts, because that table is already pinned by the two-way check at startup (see
// authz/README.md): one missing → panic at registration; one extra → Verify() errors. That is,
// "table == real routes" is already taken care of, and the spec is automatically right as long as it follows the table.
//
// So the whole loop is:
//
//	new route → must be written in the matrix (or it will not start) → the spec changes automatically → reconciliation ensures the spec is committed
//
// The last step is covered by TestSpecFileIsUpToDate; see openapi_test.go.
//
// # Why the permission level goes into every endpoint's description
//
// This is something a hand-written spec cannot give: the matrix stores each route's level (none/read/write/manage)
// and the matching minimum role. With it the agent knows "what this key can do", instead of trying it out by hitting a pile of 403s:
// 403 only says "no", not "you need admin".
//
// # Entity schemas are hand-written, deliberately
//
// Routes must be generated (they drift and have a source of truth), while entity schemas are hand-written instead, because:
//
//  1. **domain json tags already have three iron rules** (see the model/apimodel package comment),
//     and reflecting tags alone cannot express the difference between "this key must be present, and must be []" and "this key may be absent",
//     and that difference is the core of the frontend contract.
//  2. Schemas need **Chinese descriptions** to be useful to agents (sentences like "requires_action is not waiting for user confirmation"
//     cannot be obtained by reflection), and those descriptions are in the domain comments; only a person can carry them over.
//
// The cost is syncing by hand when domain adds a field. This cost is bounded: missing one only means the spec describes one field fewer,
// and does not make agents call the wrong endpoint; route drift does. The two differ in severity by an order of magnitude, so only the latter gets a machine check.
package openapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/chyroc/ama/server/authz"
)

// Version is info.version in the spec.
//
// It has nothing to do with the repo version: it marks the version of **the API contract**, and should not change while the API does not.
const Version = "1.0.0"

// specTitle / specDescription are the two passages in info for people (and agents) to read.
const (
	specTitle = "AMA Platform API"

	specDescription = `AMA 平台的 HTTP API。

本文档**由路由权限表自动生成**，不是手写的——所以它不会与真实注册的路由漂移。

先读 /llms.txt：那里讲的是「调这些接口之前要知道什么」，光看本文档猜不出来
（比如「一次对话就是一条 trigger=manual 的 Run，消息是这条 Run 上的事件」）。

## 认证

两种凭据，都走 ` + "`Authorization: Bearer <token>`" + `：

- **API Key**（给 agent / 脚本 / CI）：在空间设置页创建，形如 ` + "`amak_...`" + `。
  明文只在创建那一刻返回一次，之后再也拿不到。
- **会话 Cookie**（给浏览器）：走本地账号或 OIDC 登录，凭据在 HttpOnly Cookie 里。

API Key 继承**创建者本人**的权限，不是一套独立的作用域——见下面「授权」。

## 授权

权限按「你在这个空间里的角色」判定，与凭据类型无关：

| 角色 | 能做什么 |
| --- | --- |
| owner / admin | 全部（含空间设置、成员、凭证） |
| member | 读 + 写（发消息、增删改 Agent 与技能） |
| viewer | 只读 |
| 非成员 | 一律 403 |

每条接口的 description 里都标了它要求的档位（none / read / write / manage）与最低角色。

## 错误信封

失败一律是 ` + "`{\"message\": \"中文句子\", \"code\": \"稳定标识\"}\"`" + `。
**判断要看 code 不看状态码**：路由写错时标准库返回的 404 是 text/plain，
按状态码判会把「路由挂错了」当成「资源不存在」。`
)

// Generate produces the JSON bytes of the OpenAPI 3.1 document (indented, with a trailing newline).
//
// The output is **deterministic**: the same input produces byte-for-byte identical output. This is the precondition for reconciliation (is the spec committed)
// to work: with random map iteration order, every generation would give a non-empty diff.
//
// Determinism relies on two things: authz.Routes() is already sorted, and every map below uses orderedMap to output in order.
func Generate() ([]byte, error) {
	doc := newOrderedMap()
	doc.set("openapi", "3.1.0")
	doc.set("info", infoObject())
	doc.set("servers", serversArray())
	doc.set("tags", tagsArray())
	doc.set("paths", pathsObject(allRoutes()))
	doc.set("components", componentsObject())

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	// HTML is not escaped: descriptions contain Chinese and angle brackets like `<token>`; escaped they would be unreadable to people,
	// and the spec is for people and agents to read.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("生成 OpenAPI 文档失败: %w", err)
	}
	return buf.Bytes(), nil
}

// internalPathPrefixes are path prefixes that **do not go into the spec**.
//
//	/api/_admin/    one-off endpoints for ops (none right now; the prefix is kept, and anything added goes out of the spec automatically)
//
// These endpoints are **of no use at all** to agents and external callers; writing them into the spec would only make people think they are
// everyday usable endpoints.
//
// ⚠️ **"Not in the spec" is not access control.** It stops nobody who can use curl; the real barrier
// must be a separate permission check in the Service layer. See "not showing an entry in the UI stops nobody" in core/README-feedback.md.
//
// ⚠️ **The two `_debug` routes deliberately still go into the spec**: e2e uses them, and stating clearly "only exists when DevRoutes
// is on" is more useful than hiding them. The difference between the two is "does anyone need to call it following the spec".
var internalPathPrefixes = []string{"/api/_admin/"}

// isInternalPath decides whether a path is "internal infrastructure, not in the spec".
//
// **Exported for tests** (openapi_test.go in the same package), so the reconciliation test and the generator share one prefix table:
// with a copy on each side, adding an internal route and changing one side but not the other would turn the test red
// on "route counts differ", and that message does not point to the real cause.
func isInternalPath(path string) bool {
	for _, prefix := range internalPathPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// filterOutInternal removes the routes that do not go into the spec.
func filterOutInternal(in []authz.RouteDecl) []authz.RouteDecl {
	out := in[:0:0] // do not reuse the underlying array: the caller gets the return value of authz.Routes()
	for _, r := range in {
		if !isInternalPath(r.Path) {
			out = append(out, r)
		}
	}
	return out
}

// allRoutes is all the routes the spec describes: those in the matrix + two self-describing endpoints without auth.
//
// # Why those two are not in the matrix
//
// They are registered on the **bare mux** (httpd/server.go), not through protectedMux, so they are outside Guard's
// view. Writing them into the matrix would trigger the reverse panic "declared but not registered", same as the five /api/auth/ ones.
//
// So their declaration can only live here. **This is the only place in this package where "routes do not come from the matrix"**,
// and the cost is covered by httpd's TestPublicRoutesRegistered: it hits these two paths with the real wiring,
// asserting 200 even without credentials. So "written here but not registered" is still caught;
// only what catches it is that test, not Guard.
func allRoutes() []authz.RouteDecl {
	routes := filterOutInternal(authz.Routes())
	// action is "none" rather than empty: permissionLine picks its text by it, and isPublic sends
	// those two down the "no auth" branch.
	public := []authz.RouteDecl{
		{Method: http.MethodGet, Path: PathSpec, Action: "none"},
		{Method: http.MethodGet, Path: PathLLMsTxt, Action: "none"},
	}
	out := append(routes, public...)
	// Sort again, otherwise those two would dangle at the end, inconsistent with the sort rule of authz.Routes(),
	// and the order of paths goes into the spec bytes; instability would make reconciliation fail.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})
	return out
}

func infoObject() *orderedMap {
	m := newOrderedMap()
	m.set("title", specTitle)
	m.set("version", Version)
	m.set("description", specDescription)
	return m
}

func serversArray() []any {
	m := newOrderedMap()
	// Relative path: the API is on whichever domain the spec was fetched from. Hard-coding an absolute address would make
	// one side wrong when local (localhost:5273) and production share one spec.
	m.set("url", "/")
	m.set("description", "与本文档同源")
	return []any{m}
}

// tagsArray is the grouping of endpoints, split by resource domain.
//
// The order is **a reading order for people** (going inwards from workspaces), not alphabetical: reading through once, an agent learns
// "create a workspace first, then an agent, then send messages".
func tagsArray() []any {
	groups := []struct{ name, desc string }{
		{tagWorkspace, "空间：一切资源的归属边界，没有「全局 agent」"},
		{tagMember, "空间成员与角色"},
		{tagAgent, "Agent 定义（提示词、模型、绑定的技能）"},
		{tagSkill, "空间级技能资产，可被多个 Agent 引用"},
		{tagIssue, "问题：空间级的问题记录，含评论与附件（与 /api/feedback 不是一回事）"},
		{tagSuggestion, "产品建议：对 AMA 平台本身的意见，**平台级**（没有空间维度）。" +
			"与「问题」（空间级待办）和 /api/feedback（机器采集的报错流）都不是一回事"},
		{tagArkCredential, "空间级的方舟凭证（付费 key），一个空间一份"},
		{tagAPIKey, "调用本 API 用的 key，账号级、继承创建者权限"},
		{tagIntegration, "外部平台授权（账号级，与空间无关）"},
		{tagConversation, "对话：一次对话就是一条 trigger=manual 的 Run"},
		{tagRun, "运行历史与事件流"},
		{tagSchedule, "定时任务与自唤醒"},
		{tagUsage, "token 用量聚合"},
		{tagFeishu, "飞书应用接入"},
		{tagUserFile, "个人文件：账号级的文件存储，按登录用户隔离（字节在对象存储）。" +
			"小文件一次上传，大文件分片上传；下载返回限时签名 URL"},
		{tagMeta, "面向 agent 的自描述接口（本文档与 /llms.txt），免鉴权"},
		{tagDebug, "开发用，只在 DevRoutes 开时注册，线上不存在"},
	}
	out := make([]any, 0, len(groups))
	for _, g := range groups {
		m := newOrderedMap()
		m.set("name", g.name)
		m.set("description", g.desc)
		out = append(out, m)
	}
	return out
}

// pathsObject groups route declarations by path into the OpenAPI paths object.
//
// The fallback route ("/api/", no method) **does not go into the spec**: it is not an endpoint, but the mechanism of "return a structured
// envelope on a miss". Writing it as a path would make agents think GET /api/ returns something.
func pathsObject(routes []authz.RouteDecl) *orderedMap {
	paths := newOrderedMap()
	for _, r := range routes {
		if r.Method == "" {
			continue
		}
		item, ok := paths.get(r.Path)
		if !ok {
			item = newOrderedMap()
			paths.set(r.Path, item)
		}
		pathItem := item.(*orderedMap)
		// Path parameters hang on the path item instead of each operation: the parameters of several methods under the same path
		// are always the same, and writing them in operations would repeat them three times and could get them wrong.
		if _, has := pathItem.get("parameters"); !has {
			if params := pathParameters(r.Path); len(params) > 0 {
				pathItem.set("parameters", params)
			}
		}
		pathItem.set(strings.ToLower(r.Method), operationObject(r))
	}
	return paths
}

// operationObject generates the description of one endpoint.
func operationObject(r authz.RouteDecl) *orderedMap {
	op := newOrderedMap()
	op.set("operationId", operationID(r))
	op.set("summary", summaryFor(r))
	op.set("tags", []any{tagFor(r)})
	op.set("description", descriptionFor(r))

	// query parameters use $ref pointing to components.parameters (the MA shape), and the definitions are
	// collected separately by collectQueryParams. A throwaway map is passed here: this function only wants the $ref array.
	if q := queryParamRefs(r, newOrderedMap()); len(q) > 0 {
		op.set("parameters", q)
	}
	if body := requestBodyFor(r); body != nil {
		op.set("requestBody", body)
	}
	op.set("responses", responsesFor(r))
	// The two endpoints without auth must explicitly declare security as an empty array, otherwise readers would think they also need a token.
	if isPublic(r) {
		op.set("security", []any{})
	}
	return op
}

// descriptionFor builds an endpoint's description: the paragraph written by a person + the machine-generated permission line.
//
// **The permission line must be generated**, so it always matches the matrix; the person-written paragraph comes first,
// provided by routeDocs (without an entry there is only the permission line, and the spec is still complete and usable).
func descriptionFor(r authz.RouteDecl) string {
	var b strings.Builder
	if doc, ok := routeDocs[r.Method+" "+r.Path]; ok {
		b.WriteString(doc)
		b.WriteString("\n\n")
	}
	b.WriteString(permissionLine(r))
	if r.ResourceKind != "" {
		fmt.Fprintf(&b, "\n\n空间归属按路径参数 `%s`（%s 的 id）反解。资源不存在返回 404 + `code=not_found`",
			r.IDParam, r.ResourceKind)
		if r.AllowMissing {
			b.WriteString("——**但本接口例外**：反解不到时不返回 404，而是放行给 handler，由它回 200 + `[]`（列表接口永不 404）")
		}
		b.WriteString("。")
	}
	if r.Optional {
		b.WriteString("\n\n⚠️ **本接口只在 DevRoutes 开启时注册**，线上不存在。")
	}
	return b.String()
}

// permissionLine is the "what permission is needed" line, generated from the matrix level.
func permissionLine(r authz.RouteDecl) string {
	switch r.Action {
	case "none":
		if isPublic(r) {
			return "**权限**：`none`——**免鉴权**，不需要任何凭据（agent 得先读懂这份文档才能认证）。"
		}
		return "**权限**：`none`——要求已登录，但不要求空间成员身份。" +
			"（这类接口的作用对象是「调用者本人」，当事人从凭据里取、不是请求参数，所以没有可判的越权。）"
	case "read":
		return "**权限**：`read`——要求是该空间成员，最低角色 `viewer`。"
	case "write":
		return "**权限**：`write`——要求是该空间成员，最低角色 `member`。"
	case "manage":
		return "**权限**：`manage`——要求是该空间成员，最低角色 `admin`（owner 同级）。"
	default:
		return "**权限**：`" + r.Action + "`。"
	}
}

// operationID generates an identifier of the `Service_verb` shape, consistent with Ark Managed Agents (MA).
//
// Generated rather than maintained by hand: a hand-maintained table would miss new routes, and operationId is what client generators
// use to name methods; a missing one becomes a method called unknown_3.
//
// # Why Service_verb instead of camelCase joined from "method + path"
//
// The previous shape was `getAgentsById` / `patchAgentsByIdArkCredentialRegion`: joining the HTTP method
// and the path segment by segment. It is unique, stable, and does work, but **readers have to translate it back to a path in their heads** to
// know which endpoint it is, while the MA style (`Agents_retrieve` / `Agents_update`) is "which resource +
// what it does", clear at a glance. AMA and MA are read by the same people, often side by side, and different shapes on the two sides bring no benefit.
//
// Renaming is **safe**: operationId only appears in the spec, and no code in the repo references it
// (the 14 calls in the frontend use path literals, and so does ama-cli). So this change does not touch the wire format,
// only how the spec expresses it.
//
// ⚠️ **Uniqueness is still a hard requirement** (duplicates make client generators silently overwrite one of the methods),
// and Service_verb collides more easily than "method+path": two different paths can map to the same
// (service, verb). So on a collision a suffix from the path completes it, and TestEveryRouteHasOperationID
// still checks every route for duplicates.
func operationID(r authz.RouteDecl) string {
	return serviceName(r) + "_" + verbName(r)
}

// serviceName is the `Service_` half of operationId: which resource this route operates on.
//
// The test is **the last "collection name" in the path** (a segment that is not a path parameter), not the first:
// `/api/workspaces/{workspaceId}/agents` operates on agents, not workspaces;
// taking the first would name every sub-resource under workspaces Workspaces_*, exactly the ambiguity to avoid.
func serviceName(r authz.RouteDecl) string {
	// A few paths cannot produce a good name, so they are hard-coded.
	//
	// ⚠️ **`/api/openapi.json` and `/llms.txt` must be here**: their last segments contain a dot,
	// and the general rule would generate `Openapi.json_list` / `Llms.txt_list`. **A dot is not a valid identifier character in almost any
	// language**, and client generators would either error or silently rename it.
	// And the spec itself is perfectly valid, so nothing would remind you of this.
	if name, ok := serviceNameOverrides[r.Path]; ok {
		return name
	}
	segs := collectionSegments(r.Path)
	if len(segs) == 0 {
		return "Meta"
	}
	// When the last segment is an action word (verb-like), the service name is the collection before it:
	// the service of `/api/skills/{id}/revisions/{revisionId}/rollback` is SkillRevisions.
	if len(segs) > 1 && actionSegments[segs[len(segs)-1]] {
		segs = segs[:len(segs)-1]
	}
	// The last segment is the main name, and earlier segments are kept as qualifiers only when needed to distinguish (see qualifierFor).
	return qualifierFor(segs) + camelSegment(segs[len(segs)-1])
}

// serviceNameOverrides are the few paths whose names cannot be built validly or readably by the general rule.
//
// The last segments of the first two contain a dot (see the comment on serviceName); the two `_debug` ones start with an underscore,
// generating names with a leading underscore like `_debug_clear`, which read like private members.
var serviceNameOverrides = map[string]string{
	PathSpec:            "OpenAPISpec",
	PathLLMsTxt:         "LLMsTxt",
	"/api/_debug/clear": "Debug",
	"/api/_debug/reset": "Debug",
	// The upgrade session passed through from Ark: the general rule would take "ark" before the action segment (Ark_upgrade),
	// which reads like a platform endpoint; it operates on sessions, so it is fixed as Sessions.
	"/api/runs/{runId}/ark/upgrade": "Sessions",
}

// collectionSegments picks out the "collection name" segments of a path, dropping the /api/ prefix and {xxx} parameters.
func collectionSegments(path string) []string {
	var out []string
	for _, seg := range strings.Split(strings.TrimPrefix(path, "/api/"), "/") {
		if seg == "" || strings.HasPrefix(seg, "{") {
			continue
		}
		out = append(out, seg)
	}
	return out
}

// qualifierFor adds a prefix qualifier to the service name, only when the last segment alone is not enough to distinguish.
//
// Example: the last segment of `/api/runs/{runId}/sandbox/terminal` is terminal; just Terminal does not say
// whose terminal it is, so it becomes SandboxTerminal. While agents, the last segment of `/api/workspaces/{id}/agents`,
// is clear enough, and does not become WorkspaceAgents.
//
// The test: when the last segment is a **generic word** (may appear under several resources), add one parent segment.
//
// ⚠️ **It must add enough to avoid collisions**, otherwise two different paths get the same operationId,
// and a duplicate operationId makes client generators silently overwrite one of the methods: that endpoint just
// disappears, **and the spec itself is perfectly valid**. So with `ark-credential` under both agents and
// workspaces, the service names are AgentArkCredential and WorkspaceArkCredential respectively.
func qualifierFor(segs []string) string {
	if len(segs) < 2 {
		return ""
	}
	last := segs[len(segs)-1]
	if !genericSegments[last] {
		return ""
	}
	// When the parent segment is itself a generic word (three levels like sandbox/terminal/input), go up one more level,
	// otherwise input/ticket under Terminal and SandboxTerminal still cannot be told apart.
	prefix := camelSegment(segs[len(segs)-2])
	if len(segs) >= 3 && genericSegments[segs[len(segs)-2]] {
		prefix = camelSegment(segs[len(segs)-3]) + prefix
	}
	return prefix
}

// genericSegments are the segment names where "on its own you cannot tell what it belongs to".
//
// They appear under several resources (revisions under agents / schedules / skills,
// ark-credential under agents and workspaces, attachments under issues and
// suggestions), so the service name must include the parent name to be unique and readable.
//
// ⚠️ **Missing a word leads to operationId collisions**, and collisions are silent (see qualifierFor).
// The test is mechanical: can this word hang under two different parent resources? If so, it goes into this table.
// TestEveryRouteHasOperationID checks every route for duplicates, and a miss turns it red on the spot.
var genericSegments = map[string]bool{
	"revisions": true,
	// hosts hangs at both the account level (/api/hosts) and the workspace level (/api/workspaces/{id}/hosts);
	// the latter needs the Workspace qualifier, otherwise both serviceNames are Hosts and the operationIds collide.
	"hosts":          true,
	"credentials":    true,
	"credential":     true,
	"ark-credential": true,
	"ark-console":    true,
	"attachments":    true,
	"comments":       true,
	"replies":        true,
	"file":           true,
	"files":          true,
	"terminal":       true,
	"apps":           true,
	"app":            true,
	"accounts":       true,
	"keys":           true,
	"api-keys":       true,
	"status":         true,
	"events":         true,
	"pending":        true,
	"chats":          true,
	"skills":         true,
	"agents":         true,
	"integrations":   true,
	"install":        true,
	"scan":           true,
	"import":         true,
	"fork":           true,
	"verify":         true,
	"authorize":      true,
	"stream":         true,
	"input":          true,
	"ticket":         true,
	"region":         true,
	"sandbox":        true,
}

// actionSegments are the words where the last segment is actually an action, not a resource.
//
// They decide the verb (see verbName) and should not go into the service name, otherwise odd names like Rollback_create
// appear, where "the service is called rollback".
//
// ⚠️ **When adding a route of the "resource + action" shape, add a word here.** Missing it gives no error;
// the name just becomes harder to read: a probe verified that `POST /api/widgets/{id}/publish` with publish not in this
// table generates `Publish_create` (the service is called "publish"), and only with it added is it `Widgets_publish`.
// The shape check (TestOperationIDsFollowMAShape) accepts both as valid, so it cannot stop this;
// only reading the generated name during review can.
var actionSegments = map[string]bool{
	"rollback":     true,
	"verify":       true,
	"refresh":      true,
	"authorize":    true,
	"resume":       true,
	"shell":        true,
	"redirect":     true,
	"install":      true,
	"scan":         true,
	"import":       true,
	"fork":         true,
	"clear":        true,
	"reset":        true,
	"run-now":      true,
	"use-global":   true,
	"upgrade":      true,
	"batch-create": true,
}

// camelSegment turns a path segment into PascalCase: ark-credential → ArkCredential.
func camelSegment(seg string) string {
	var b strings.Builder
	for _, part := range strings.Split(seg, "-") {
		b.WriteString(upperFirst(part))
	}
	return b.String()
}

// verbName is the `_verb` half of operationId, using MA's words.
//
// MA's word list: create / list / retrieve / update / remove. The mapping rules:
//
//	POST   → create (when the last segment is an action word, that action is used, e.g. _rollback / _verify)
//	GET    → list (the last path segment is a collection) or retrieve (the last segment is {id})
//	PATCH  → update ⚠️ see "why not change to POST" below
//	PUT    → replace (whole replacement, deliberately distinct from update's partial update)
//	DELETE → remove
//
// ⚠️ **MA has zero PATCH and zero PUT across its repo, always updating with POST; AMA deliberately keeps PATCH/PUT.**
// That is the wire format, and changing it would touch every caller of 15+7 endpoints (frontend and e2e),
// while PATCH itself carries the information "partial update" that POST cannot express.
// The full reasoning is in the "deliberately not adopted" section of ma_conventions.md.
func verbName(r authz.RouteDecl) string {
	segs := collectionSegments(r.Path)
	lastIsAction := len(segs) > 0 && actionSegments[segs[len(segs)-1]]

	switch r.Method {
	case http.MethodPost:
		if lastIsAction {
			return lowerCamel(segs[len(segs)-1])
		}
		// A POST on a single resource (the path ends with {id}) is an "update", not a "create":
		// Ark Managed Agents always updates with POST (not PATCH), and the passthrough group follows this shape.
		// AMA's own routes have no such POST (its own updates use PATCH, and POST either hits a collection or carries an action segment),
		// so this branch only affects the Ark passthrough endpoints.
		if strings.HasSuffix(r.Path, "}") {
			return "update"
		}
		return "create"
	case http.MethodGet:
		if lastIsAction {
			return lowerCamel(segs[len(segs)-1])
		}
		// The last segment is a path parameter → retrieve one; otherwise check whether it is a collection or a singleton.
		if strings.HasSuffix(r.Path, "}") {
			return "retrieve"
		}
		// ⚠️ **Singleton resources use retrieve, not list.** The last segment of `/api/agents/{id}/ark-credential`
		// is not a path parameter, but it gets **one** credential, not a list; judging it list would make
		// readers think the response is an array (while on the wire it is a single object), and would also collide with a real list of the same name.
		// The test is plural vs singular: those in singletonSegments are singletons.
		if singletonSegments[segs[len(segs)-1]] {
			return "retrieve"
		}
		return "list"
	case http.MethodPatch:
		return "update"
	case http.MethodPut:
		return "replace"
	case http.MethodDelete:
		return "remove"
	default:
		return strings.ToLower(r.Method)
	}
}

// lowerCamel turns run-now into runNow: an action word in the verb position starts with lower camelCase.
func lowerCamel(seg string) string {
	parts := strings.Split(seg, "-")
	var b strings.Builder
	b.WriteString(strings.ToLower(parts[0]))
	for _, p := range parts[1:] {
		b.WriteString(upperFirst(p))
	}
	return b.String()
}

// singletonSegments are the last segments that are "one, not a batch".
//
// GET on them returns a single object, so the verb is retrieve, not list (see verbName).
// The test is just singular vs plural: a workspace has only one Ark credential, a run only one sandbox,
// and a person only one set of preferences.
var singletonSegments = map[string]bool{
	"ark-credential":        true,
	"global-ark-credential": true,
	"ark-console":           true,
	"authorization":         true,
	"sandbox":               true,
	"terminal":              true,
	"trace":                 true,
	"file":                  true,
	"status":                true,
	"user-preferences":      true,
	"user-onboarding":       true,
	"sync-task":             true,
	"my-role":               true,
	"usage":                 true,
	"readiness":             true,
	"admin-authorization":   true,
	"app":                   true,
	"workspace-by-key":      true,
	"workspace-key-taken":   true,
	// The two self-describing endpoints each have only one document, not a list. Their service names go through serviceNameOverrides,
	// but the verb is still judged by this table: writing _list would make people think the response is an array.
	"openapi.json": true,
	"llms.txt":     true,
}

func upperFirst(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// isPublic means this route needs no auth.
//
// ⚠️ **Loosening one here opens an unauthorized entry**, so the list is hard-coded here and pinned by a test
// (TestPublicRoutesExposeNoTenantData). The test is not "looks harmless", but
// **the response has only structure, no instance data at all**: the spec describes field shapes, llms.txt describes model semantics,
// and neither queries the store or knows any workspace.
func isPublic(r authz.RouteDecl) bool {
	return publicRoutes[r.Method+" "+r.Path]
}

// publicRoutes is the list of routes without auth, matching their registration places in httpd one to one.
var publicRoutes = map[string]bool{
	"GET " + PathSpec:    true,
	"GET " + PathLLMsTxt: true,
}

// The paths of these two routes; both the httpd registration and the matrix declaration reference them, to avoid writing the string in three places.
const (
	// PathSpec is the address of this document itself.
	PathSpec = "/api/openapi.json"
	// PathLLMsTxt is the project description for agents to read.
	//
	// ⚠️ It is **not under /api/**; this is the llmstxt.org convention (agents look for it in the site root by convention).
	// The cost is that httpd's isBackendPath must let it through separately, otherwise the frontend SPA would swallow it as a route.
	PathLLMsTxt = "/llms.txt"
)

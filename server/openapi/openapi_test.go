// Reconciliation of the spec against the matrix, and security assertions for the two endpoints without auth.
//
// # Why this file exists: turn "OpenAPI must be in sync" into a machine-enforced rule
//
// Just writing "remember to regenerate the spec after changing routes" in CLAUDE.md is not enough: that relies on everyone
// remembering every time. And forgetting results in a spec that **looks normal but is already outdated**; agents calling by it get
// 404 / 403, with errors that do not point to the real cause at all ("this endpoint was renamed last month").
//
// So three things are pinned here:
//
//	TestSpecCoversEveryDeclaredRoute  the routes in the spec match the matrix one by one (checked in both directions)
//	TestSpecFileIsUpToDate            the openapi.json in git is up to date
//	TestLLMsTxtMentionsOnlyRealRoutes the endpoints mentioned in /llms.txt all really exist
//	TestLLMsTxtSpecLinkFollowsOrigin  the spec link in /llms.txt follows the request's origin
//	TestLLMsTxtLeavesNoPlaceholder    no placeholder left after rendering
//	TestLLMsTxtHasNoBoldMarkup        the body does not use Markdown bold for emphasis
//
// The loop they form is:
//
//	new route → must be written in the matrix (if missed the service will not start; the two-way check in authz/guard.go)
//	          → the spec changes automatically (this package generates it from authz.Routes())
//	          → reconciliation ensures the spec was regenerated and committed (TestSpecFileIsUpToDate)
//
// The whole chain **does not rely on anyone remembering**.

package openapi

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/authz"
)

// TestSpecCoversEveryDeclaredRoute checks that the spec's set of routes is exactly the same as the matrix.
//
// **Both directions must be checked**, for exactly the same reason as the two-way check of authz.Guard:
//
//	in the matrix, not in the spec → a new route is not documented, and agents do not know about the ability
//	in the spec, not in the matrix → the docs have an endpoint that does not exist, and agents calling it get 404
//
// Checking only one direction would catch only half of a change like "rename a route" (the old one disappears + a new one appears).
//
// # It compares the copy on disk, not one generated on the spot
//
// **This difference was found by mutation checks.** This used to compare the result of Generate(),
// so the mutation "hand-stuff a fake route into openapi.json" **could not be detected**:
// the copy generated on the spot naturally does not contain the hand-stuffed thing, so both sides matched.
//
// And what is served to agents in production is exactly the copy on disk (go:embed). So that is what must be asserted on.
func TestSpecCoversEveryDeclaredRoute(t *testing.T) {
	spec := parseSpec(t, specOnDisk(t))

	// The expected set on the matrix side.
	want := map[string]bool{}
	for _, r := range authz.Routes() {
		// The fallback route is not an endpoint (it is the mechanism of "return a structured envelope on a miss"), and does not go into the spec.
		if r.Method == "" {
			continue
		}
		// Internal infrastructure routes are deliberately left out of the spec; see the comment on
		// internalPathPrefixes next to allRoutes. **This filter must share the same prefix table with the generator**:
		// with a copy on each side, adding an internal route and changing the generator but forgetting this place
		// would turn this test red, with an error saying "counts differ", not pointing to the real cause.
		if isInternalPath(r.Path) {
			continue
		}
		want[r.Method+" "+r.Path] = true
	}
	// The two self-describing endpoints without auth are not in the matrix (registered on the bare mux), but must be in the spec.
	// See the comment on allRoutes for why.
	want["GET "+PathSpec] = true
	want["GET "+PathLLMsTxt] = true

	got := specOperations(spec)

	for op := range want {
		if !got[op] {
			t.Errorf("matrix 里声明了 %q，但 spec 里没有。\n"+
				"修复：cd server && go run ./cmd/gen-openapi", op)
		}
	}
	for op := range got {
		if !want[op] {
			t.Errorf("spec 里有 %q，但 matrix 里没有声明——这条接口不存在，"+
				"agent 照着调会 404。\n"+
				"要么在 authz/matrix.go 里补声明，要么它是笔误。", op)
		}
	}
	if len(want) != len(got) {
		t.Errorf("路由条数不一致：matrix %d 条，spec %d 条", len(want), len(got))
	}
	t.Logf("spec 覆盖 %d 条路由（matrix %d 条 + 2 条免鉴权自描述接口，"+
		"另有内部路由不进 spec）", len(got), len(want)-2)
}

// TestSpecFileIsUpToDate checks that the openapi.json in git is byte-for-byte identical to what the current code generates.
//
// **This is the last link of the whole loop.** The earlier steps ensure "the matrix matches the real routes" and "the spec is generated from
// the matrix"; this step ensures "what was generated was really committed". Otherwise the routes change and
// the spec changes automatically too, but only the in-memory copy; the one in the repo and in the binary (go:embed) is still old.
//
// The failure message gives the fix command directly, because when this test is red people are usually changing something else.
func TestSpecFileIsUpToDate(t *testing.T) {
	generated := mustGenerate(t)

	onDisk, err := os.ReadFile("openapi.json")
	if err != nil {
		t.Fatalf("读取 openapi.json 失败: %v\n\n"+
			"修复：cd server && go run ./cmd/gen-openapi", err)
	}

	if !bytes.Equal(onDisk, generated) {
		t.Errorf("openapi.json 已过期：authz/matrix.go 或 schema 改过了，但 spec 没重新生成。\n\n"+
			"修复：cd server && go run ./cmd/gen-openapi && git add openapi/openapi.json\n\n"+
			"（文件 %d 字节，重新生成 %d 字节）", len(onDisk), len(generated))
	}

	// The copy embedded into the binary must match too.
	//
	// **Asserting it separately is not redundant**: the comparison above is against the file on disk, this one against the go:embed result.
	// The two are normally the same, but they diverge when "the file changed but was not recompiled": then production serves
	// the old spec, while the file on disk looks right.
	if !bytes.Equal(Spec(), onDisk) {
		t.Error("go:embed 进来的 spec 与磁盘上的 openapi.json 不一致（可能是没重新编译）")
	}
}

// TestSpecIsDeterministic checks that generation is deterministic.
//
// **Determinism is the precondition for TestSpecFileIsUpToDate to work**: if the bytes differed on every generation
// (e.g. some map not sorted), that reconciliation would fail forever, with a message saying "the spec is outdated",
// not pointing to the real cause at all. This test makes the real cause, "unstable order", show itself.
func TestSpecIsDeterministic(t *testing.T) {
	// Run three times: two runs might happen to hit the same map iteration order, three is much harder.
	first := mustGenerate(t)
	for i := 2; i <= 3; i++ {
		again := mustGenerate(t)
		if !bytes.Equal(first, again) {
			t.Fatalf("第 %d 次生成的结果与第一次不同——某处 map 没有排序输出。\n"+
				"这会让 TestSpecFileIsUpToDate 永远失败，且报错指向「spec 过期」而不是真实原因。", i)
		}
	}
}

// TestEveryRouteHasPermissionInDescription checks that every endpoint's description has the permission note.
//
// This is the main added value of the spec over a hand-written yaml: with the level, agents know "what this key can do",
// instead of trying it out by hitting a pile of 403s. Missing it gives no error; that endpoint's description is just half missing.
func TestEveryRouteHasPermissionInDescription(t *testing.T) {
	spec := parseSpec(t, specOnDisk(t))
	paths := spec["paths"].(map[string]any)

	for path, itemAny := range paths {
		item := itemAny.(map[string]any)
		for method, opAny := range item {
			if method == "parameters" {
				continue
			}
			op := opAny.(map[string]any)
			desc, _ := op["description"].(string)
			if !strings.Contains(desc, "**权限**") {
				t.Errorf("%s %s 的 description 里没有权限说明", strings.ToUpper(method), path)
			}
		}
	}
}

// TestEveryRouteHasOperationID checks that operationId exists and is unique.
//
// Uniqueness is a hard requirement of OpenAPI: with duplicates client generators silently overwrite one of the methods,
// so some endpoint just disappears, **while the spec itself is valid**.
func TestEveryRouteHasOperationID(t *testing.T) {
	spec := parseSpec(t, specOnDisk(t))
	paths := spec["paths"].(map[string]any)

	seen := map[string]string{}
	for path, itemAny := range paths {
		item := itemAny.(map[string]any)
		for method, opAny := range item {
			if method == "parameters" {
				continue
			}
			op := opAny.(map[string]any)
			id, _ := op["operationId"].(string)
			if id == "" {
				t.Errorf("%s %s 没有 operationId", strings.ToUpper(method), path)
				continue
			}
			if prev, dup := seen[id]; dup {
				t.Errorf("operationId %q 重复：%s 与 %s %s。"+
					"重复会让 client 生成器静默覆盖其中一个方法。",
					id, prev, strings.ToUpper(method), path)
			}
			seen[id] = strings.ToUpper(method) + " " + path
		}
	}
}

// TestEveryPathParamIsDocumented checks that every {xxx} in the path is declared in parameters.
//
// If one is missing, agents following the spec would splice `{workspaceId}` into the URL as a **literal**,
// and get 404, while the spec looks completely normal.
func TestEveryPathParamIsDocumented(t *testing.T) {
	spec := parseSpec(t, specOnDisk(t))
	paths := spec["paths"].(map[string]any)

	for path, itemAny := range paths {
		item := itemAny.(map[string]any)

		declared := map[string]bool{}
		if raw, ok := item["parameters"]; ok {
			for _, p := range raw.([]any) {
				param := p.(map[string]any)
				if param["in"] == "path" {
					declared[param["name"].(string)] = true
				}
			}
		}

		for _, seg := range strings.Split(path, "/") {
			if !strings.HasPrefix(seg, "{") || !strings.HasSuffix(seg, "}") {
				continue
			}
			name := strings.Trim(seg, "{}")
			if !declared[name] {
				t.Errorf("%s 的路径参数 {%s} 没有在 parameters 里声明——"+
					"agent 会把它当字面量拼进 URL", path, name)
			}
		}
	}
}

// TestEveryRefResolves checks that every $ref points to a schema that really exists.
//
// A dangling $ref makes the spec invalid in any validator, and **generating a client fails outright**.
// Hand-writing schemaRef("Wrogn") gives no compile error; it only produces a broken document.
func TestEveryRefResolves(t *testing.T) {
	raw := specOnDisk(t)
	spec := parseSpec(t, raw)

	schemas := spec["components"].(map[string]any)["schemas"].(map[string]any)
	for name, def := range schemas {
		if def == nil {
			t.Errorf("schema %q 在 schemaOrder 里但 entitySchemas 里没有定义（会输出 null）", name)
		}
	}

	// Scan for $ref directly in the raw bytes instead of recursive walking: the latter easily misses some level of container type.
	const marker = `"$ref": "#/components/schemas/`
	rest := string(raw)
	for {
		i := strings.Index(rest, marker)
		if i < 0 {
			break
		}
		rest = rest[i+len(marker):]
		end := strings.Index(rest, `"`)
		if end < 0 {
			t.Fatal("spec 里有个 $ref 没有闭合引号，生成逻辑坏了")
		}
		name := rest[:end]
		if _, ok := schemas[name]; !ok {
			t.Errorf("$ref 指向不存在的 schema %q", name)
		}
	}
}

// TestLLMsTxtMentionsOnlyRealRoutes checks that the endpoints mentioned in /llms.txt all really exist.
//
// **Without this, that document slowly turns into lies**: a route gets renamed, llms.txt still has the old one,
// and agents calling by it get 404. It is hand-written prose with no compile-time constraint,
// so only this can pin it to the real routes.
func TestLLMsTxtMentionsOnlyRealRoutes(t *testing.T) {
	real := map[string]bool{}
	for _, r := range authz.Routes() {
		if r.Method == "" {
			continue
		}
		real[r.Method+" "+r.Path] = true
	}
	real["GET "+PathSpec] = true
	real["GET "+PathLLMsTxt] = true
	// /api/auth/me is also registered on the bare mux (the login endpoint cannot require logging in first), so it is not in the matrix either.
	// A constant is referenced instead of a literal: when that endpoint is renamed this must break along with it, otherwise the docs silently become lies.
	real["GET "+auth.PathMe] = true
	// Starting login likewise. They are **the first endpoints someone not logged in calls**, so of course they cannot require logging in first.
	real["POST "+auth.PathLogin] = true
	real["GET "+auth.PathOIDCLogin] = true

	mentioned := extractRouteMentions(LLMsTxt(""))
	if len(mentioned) == 0 {
		t.Fatal("从 /llms.txt 里没抽出任何接口——抽取逻辑坏了，这条测试就成了空转")
	}

	for _, m := range mentioned {
		if !real[m] {
			t.Errorf("/llms.txt 提到了 %q，但 matrix 里没有这条路由。\n"+
				"要么路径写错了，要么那条接口改过名——两种情况 agent 照着调都会 404。", m)
		}
	}
	t.Logf("/llms.txt 提到 %d 条接口，全部存在", len(mentioned))
}

// TestLLMsTxtSpecLinkFollowsOrigin checks that the OpenAPI link in the body changes with the caller's origin.
//
// # What this pins: "do not point agents to production during local development"
//
// With `https://ama.example.com` hard-coded, the docs served by a local service would make agents call **production**
// endpoints: the responses they get are completely normal, only they change production data. **That failure gives zero errors**,
// so it can only be pinned here.
//
// Both directions must be checked: the link must carry the given origin, and no other origin may appear.
func TestLLMsTxtSpecLinkFollowsOrigin(t *testing.T) {
	cases := []struct{ origin, want string }{
		{"http://localhost:5273", "http://localhost:5273" + PathSpec},
		{"https://ama.example.com", "https://ama.example.com" + PathSpec},
		// When Host cannot be obtained, fall back to a same-origin relative path: at least it can never point to the wrong environment.
		{"", PathSpec},
	}

	for _, tc := range cases {
		body := LLMsTxt(tc.origin)
		if !strings.Contains(body, tc.want) {
			t.Errorf("origin=%q 时正文里没有 %q", tc.origin, tc.want)
		}
		// Reverse direction: other origins must not appear. Hard-coding a domain would turn this red.
		for _, other := range cases {
			if other.origin == tc.origin || other.origin == "" {
				continue
			}
			if strings.Contains(body, other.origin) {
				t.Errorf("origin=%q 时正文里出现了 %q——这个链接是写死的，"+
					"本地开发会把 agent 指到线上", tc.origin, other.origin)
			}
		}
	}
}

// TestLLMsTxtLeavesNoPlaceholder checks that no placeholder is left in the body after rendering.
//
// A missed replacement gives no error; it only shows agents a fake link starting with `{{specURL}}`, which they would request,
// and get 404, with an error that does not point to "the link in the docs was not rendered" at all.
func TestLLMsTxtLeavesNoPlaceholder(t *testing.T) {
	body := LLMsTxt("https://example.invalid")
	if strings.Contains(body, "{{") {
		t.Errorf("渲染后的 /llms.txt 里还有 `{{`——某个占位符没被替换掉")
	}
	// Meta assertion: the template must have the placeholder in the first place, otherwise the test above spins on nothing.
	if !strings.Contains(llmsTxtBody, specURLPlaceholder) {
		t.Errorf("模板里没有 %s——那么上面那条断言什么都没验", specURLPlaceholder)
	}
}

// TestLLMsTxtHasNoBoldMarkup checks that the body has no Markdown bold.
//
// # Why "no bold" is pinned as a test
//
// This document once had 44 `**`, a fairly large share of the body. **Bold everywhere means no emphasis at all**:
// what readers (agents) see is an even spread of emphasis marks, carrying exactly the same information as no bold at all,
// plus noise. So emphasis is now carried by sentence form: a paragraph of its own, "Note:", "Common misreading:", "Test:".
//
// Bold easily creeps back one spot at a time in later edits (each time just one, looking harmless),
// so it is pinned here. If you really want to bring it back, first think through why it is better than another sentence form, then change this test.
func TestLLMsTxtHasNoBoldMarkup(t *testing.T) {
	body := LLMsTxt("https://example.invalid")
	if n := strings.Count(body, "**"); n != 0 {
		t.Errorf("/llms.txt 里有 %d 处 `**`。强调请用句式承载"+
			"（自成一段 / 「注意：」/「常见误读：」/「判据：」），不要通篇加粗——"+
			"通篇加粗与全不加粗的信息量相同，只多了噪音。", n/2)
	}
}

// extractRouteMentions extracts "method + path" pairs from Markdown.
//
// It only recognizes the shape "method followed by path" (`POST /api/x` or `POST` `/api/x`),
// because that is how the docs really say "call this endpoint". Paths appearing alone in prose
// (e.g. "all endpoints under /api/") take no part in the check: those are not specific callable endpoints.
func extractRouteMentions(doc string) []string {
	methods := []string{"GET", "POST", "PATCH", "PUT", "DELETE"}
	var out []string

	for i := 0; i < len(doc); i++ {
		for _, m := range methods {
			if !strings.HasPrefix(doc[i:], m) {
				continue
			}
			rest := doc[i+len(m):]
			// Skip whitespace and Markdown backticks between the method and the path.
			j := 0
			for j < len(rest) && (rest[j] == ' ' || rest[j] == '`' || rest[j] == '\t') {
				j++
			}
			if j == 0 || j >= len(rest) || rest[j] != '/' {
				continue
			}
			path := takePath(rest[j:])
			if path != "" {
				out = append(out, m+" "+path)
			}
		}
	}
	return out
}

// takePath takes a path from the start of a string, stopping at a separator.
//
// It removes the query part (`?key=growth`) and trailing punctuation: the docs write
// `GET /api/workspace-by-key?key=growth`, while the pattern in the matrix has no query.
//
// **Iterate by rune, not by byte**: Chinese punctuation (full-width period and comma) is multi-byte,
// and comparing by byte would never match, so the path would swallow everything up to the next space, making the whole test spin on nothing.
func takePath(s string) string {
	end := len(s)
	for i, r := range s {
		switch r {
		case ' ', '`', '\n', '\t', '?', ')', ',', '。', '，', '（', '）':
			end = i
		default:
			continue
		}
		break
	}
	path := strings.TrimRight(s[:end], ".、；:")
	// A concrete URL (/api/workspaces/ws_demo/agents) is not a pattern and cannot be turned back into one; skip it.
	// The test: variable segments in a pattern are always {xxx}, so concrete ids like ws_ / run_ / agent_
	// mean this is an example, not a pattern.
	for _, sample := range []string{"ws_", "run_", "agent_", "sch_", "skill_", "ak_", "cli_"} {
		if strings.Contains(path, sample) {
			return ""
		}
	}
	if !strings.HasPrefix(path, "/") {
		return ""
	}
	return path
}

// ---------------------------------------------------------------------------
// helper
// ---------------------------------------------------------------------------

func mustGenerate(t *testing.T) []byte {
	t.Helper()
	b, err := Generate()
	if err != nil {
		t.Fatalf("生成 spec 失败: %v", err)
	}
	return b
}

// specOnDisk reads the openapi.json on disk.
//
// **Every test asserting "is the spec's content right" should use it, not Generate()**:
// what is served to agents in production is this one (go:embed references it), and the one generated on the spot is only
// the baseline for comparison in TestSpecFileIsUpToDate.
func specOnDisk(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("openapi.json")
	if err != nil {
		t.Fatalf("读取 openapi.json 失败: %v\n\n修复：cd server && go run ./cmd/gen-openapi", err)
	}
	return b
}

func parseSpec(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("生成的 spec 不是合法 JSON: %v", err)
	}
	return out
}

// specOperations expands the spec's paths into a set of "method + path".
func specOperations(spec map[string]any) map[string]bool {
	out := map[string]bool{}
	paths, ok := spec["paths"].(map[string]any)
	if !ok {
		return out
	}
	for path, itemAny := range paths {
		item, ok := itemAny.(map[string]any)
		if !ok {
			continue
		}
		for method := range item {
			if method == "parameters" {
				continue
			}
			out[strings.ToUpper(method)+" "+path] = true
		}
	}
	return out
}

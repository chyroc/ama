// Reconciliation against the MA (Ark Managed Agents) conventions.
//
// # How this file and openapi_test.go divide the work
//
// openapi_test.go pins "the spec matches the real routes of this repo"; that loop is already closed
// (matrix → generate → reconcile), and does not need to be built again.
//
// This file pins **the other direction**: does the spec's expression follow the MA conventions. The two do not cover each other;
// a spec can match the matrix exactly and still be written in a shape nobody recognizes.
//
// The relationship between AMA and MA is "AMA is an MA with a fixed env", and the two are often read side by side by the same people.
// So for **concepts both have** (agent, session↔run, skill, credential) the same
// expression should be used; and that had no machine constraint before: the conventions relied on people remembering, so every new route could grow a bit
// crooked, and each spot looks harmless on its own.
//
// # Why the baseline is a file, not MA's spec fetched live
//
// MA's spec is not in this repo, and the CI container is not guaranteed to be able to fetch it; a check designed around "my machine can fetch it"
// would fail outright in CI, with a failure message that looks like a network problem.
//
// So the baseline is frozen into this repo's ma_conventions.md (via go:embed). The cost is written in that file:
// when MA changes its conventions this side will not know automatically; someone must reread MA's spec and update the baseline.
//
// ⚠️ **There is deliberately no "t.Skip if unavailable" degradation here.** This repo has already paid for that: the four packages touching the database
// skip as a group when they cannot connect, **and skipping shows green**, with nothing in the log reminding you it did not run
// (see the top of scripts/ci/check_backend.sh). So every check in this file only reads in-process things
// (the spec on disk + the embedded baseline), and none of them skips because of a different environment.

package openapi

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// maConventions is the baseline text of the MA conventions, committed together with the spec.
//
// Asserting it is non-empty is a **meta assertion**: when the file is accidentally deleted or the embed path is wrong, all the "is it mentioned in the baseline"
// checks below would pass falsely (of course no contradiction can be found in an empty string).
func maConventionsText(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("ma_conventions.md")
	if err != nil {
		t.Fatalf("读取 ma_conventions.md 失败: %v\n\n"+
			"这份文件是 MA 约定的基线，与 spec 一起入库。它不该被删除——"+
			"没有它，本文件的对账检查会全部变成空转。", err)
	}
	if len(b) < 500 {
		t.Fatalf("ma_conventions.md 只有 %d 字节，看起来被清空或截断了。"+
			"基线为空会让下面的检查全部假通过。", len(b))
	}
	return string(b)
}

// operationIDShape is MA's operationId shape: `Service_verb`.
//
// Both halves must be valid identifier characters: client generators use it as a method name, and on a dot or hyphen
// they either error or silently rename it, **while the spec itself is perfectly valid**, so it can only be pinned here.
var operationIDShape = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*_[a-z][A-Za-z0-9]*$`)

// TestOperationIDsFollowMAShape checks that every operationId is `Service_verb`.
//
// MA's shape is "which resource + what it does" (`Agents_retrieve` / `Agents_update`),
// and readers know at a glance which endpoint it is. AMA used to use camelCase joined from method + path
// (`getAgentsById` / `patchAgentsByIdArkCredentialRegion`): unique and stable,
// but you had to translate it back to a path in your head to know what it is.
//
// ⚠️ When this test is red **do not change the test** to accommodate a new shape: operationId is what client generators
// use to name methods, and inconsistent shapes would put two naming styles in the same SDK.
// To add a new route, let operationID() generate it naturally; it already covers every method and path shape.
func TestOperationIDsFollowMAShape(t *testing.T) {
	spec := parseSpec(t, specOnDisk(t))

	checked := 0
	for path, item := range spec["paths"].(map[string]any) {
		for method, opAny := range item.(map[string]any) {
			if method == "parameters" {
				continue
			}
			id, _ := opAny.(map[string]any)["operationId"].(string)
			checked++
			if !operationIDShape.MatchString(id) {
				t.Errorf("%s %s 的 operationId 是 %q，不符合 MA 的 `Service_verb` 形状。\n"+
					"修复：改 openapi.go 的 serviceName / verbName（必要时往 "+
					"serviceNameOverrides 里加一条），然后 "+
					"cd server && go run ./cmd/gen-openapi",
					strings.ToUpper(method), path, id)
			}
		}
	}
	if checked == 0 {
		t.Fatal("一条 operationId 都没检查到——spec 解析坏了，这条测试成了空转")
	}
	t.Logf("%d 条 operationId 全部符合 Service_verb", checked)
}

// TestOperationIDVerbsComeFromMAVocabulary checks that the verb half uses MA's words.
//
// MA's word list is create / list / retrieve / update / remove. AMA has two more:
//
//	replace         PUT, whole replacement. Deliberately separate from update (PATCH, partial update):
//	                merging the two into one word would leave no way to express "does passing half the fields clear the other half".
//	<action name>   endpoints that are not CRUD (verify / rollback / refresh / authorize...),
//	                where using the action itself as the verb is more accurate than forcing it into create.
//
// ⚠️ **New words go here, rather than loosening the check.** The value of this table is that "extra words must be
// acknowledged explicitly once": otherwise verbs would slowly grow into a pile of synonyms (get/fetch/read side by side),
// each reasonable on its own.
func TestOperationIDVerbsComeFromMAVocabulary(t *testing.T) {
	// MA's five words.
	maVerbs := map[string]bool{
		"create": true, "list": true, "retrieve": true, "update": true, "remove": true,
	}
	// What AMA additionally acknowledges: replace (PUT) + the action names of each non-CRUD endpoint.
	amaVerbs := map[string]bool{
		"replace": true,
		// Non-CRUD actions. Each matches the verb at the end of the path.
		"verify": true, "rollback": true, "refresh": true, "authorize": true,
		"resume": true, "shell": true, "redirect": true, "install": true,
		"scan": true, "import": true, "fork": true, "clear": true, "reset": true,
		"runNow": true, "useGlobal": true,
		// The two actions of chunked personal file upload: merge chunks / abandon upload.
		"complete": true, "abort": true,
		// The two non-CRUD actions of the Ark passthrough endpoints (named the same as the end of the Ark path):
		// upgrade = hot-upgrade the session config; batchCreate = write memories in batch.
		"upgrade": true, "batchCreate": true,
	}

	spec := parseSpec(t, specOnDisk(t))
	for path, item := range spec["paths"].(map[string]any) {
		for method, opAny := range item.(map[string]any) {
			if method == "parameters" {
				continue
			}
			id, _ := opAny.(map[string]any)["operationId"].(string)
			parts := strings.SplitN(id, "_", 2)
			if len(parts) != 2 {
				continue // shape problems are reported by TestOperationIDsFollowMAShape
			}
			verb := parts[1]
			if maVerbs[verb] || amaVerbs[verb] {
				continue
			}
			t.Errorf("%s %s 的 operationId 是 %q，verb 部分 %q 不在词表里。\n"+
				"MA 的词：create / list / retrieve / update / remove；"+
				"AMA 另外承认 replace（PUT）与各条非 CRUD 接口的动作名。\n"+
				"修复：要么让它落到既有的词上，要么在本测试的 amaVerbs 里"+
				"显式加一条（加之前想清楚它与既有词的区别）。",
				strings.ToUpper(method), path, id, verb)
		}
	}
}

// TestNoPlaceholderSummaries checks there are no placeholder summaries of the "method + path" kind.
//
// When summaryFor finds nothing in routeSummaries it falls back to `r.Method + " " + r.Path`,
// so that endpoint's summary in the spec becomes `GET /api/agents/{id}/ark-console`:
// it repeats the path, with **not a single word of new information**.
//
// The fallback itself is deliberate (see the top of routes.go: new routes are not forced to add text at the same time,
// which would make people write a filler sentence). But what the fallback produces **should not stay in the committed spec**:
// it is pure noise to readers, and looks like "a description has been written".
func TestNoPlaceholderSummaries(t *testing.T) {
	placeholder := regexp.MustCompile(`^(GET|POST|PUT|PATCH|DELETE)\s+/`)
	spec := parseSpec(t, specOnDisk(t))

	var bad []string
	for path, item := range spec["paths"].(map[string]any) {
		for method, opAny := range item.(map[string]any) {
			if method == "parameters" {
				continue
			}
			summary, _ := opAny.(map[string]any)["summary"].(string)
			if summary == "" {
				bad = append(bad, strings.ToUpper(method)+" "+path+"（summary 为空）")
				continue
			}
			if placeholder.MatchString(summary) {
				bad = append(bad, strings.ToUpper(method)+" "+path+"（summary 是 "+summary+"）")
			}
		}
	}
	if len(bad) > 0 {
		t.Errorf("有 %d 条接口的 summary 是「方法 + 路径」占位，等于没写：\n  %s\n\n"+
			"修复：在 openapi/routes.go 的 routeSummaries 里给它们各写一句"+
			"（动词开头的短中文，3–12 字，看看邻居怎么写的），然后 "+
			"cd server && go run ./cmd/gen-openapi",
			len(bad), strings.Join(bad, "\n  "))
	}
}

// TestEveryPathParamHasDescription checks that every path parameter has a description.
//
// The difference between having a description or not is not "are the docs complete", but whether agents fill it in wrong:
// without making `{revisionId}` clear, they would fill it with the `version` integer,
// getting 404, and the error does not point to "you filled in the wrong kind of id".
//
// ⚠️ TestEveryPathParamIsDocumented does not duplicate this: that one checks "is the parameter declared"
// (if missing, agents splice `{id}` into the URL as a literal), this one checks "does the declaration have a description".
func TestEveryPathParamHasDescription(t *testing.T) {
	spec := parseSpec(t, specOnDisk(t))

	checked := 0
	for path, itemAny := range spec["paths"].(map[string]any) {
		item := itemAny.(map[string]any)
		raw, ok := item["parameters"]
		if !ok {
			continue
		}
		for _, p := range raw.([]any) {
			param := p.(map[string]any)
			if param["in"] != "path" {
				continue
			}
			checked++
			if desc, _ := param["description"].(string); strings.TrimSpace(desc) == "" {
				t.Errorf("%s 的路径参数 {%v} 没有说明。\n"+
					"修复：在 openapi/requests.go 的 pathParamDocs（或 `{id}` 那种"+
					"按路径前缀分的 idParamDocs）里补一条，写清楚「这是什么的 id、"+
					"取值从哪来」，然后 cd server && go run ./cmd/gen-openapi",
					path, param["name"])
			}
		}
	}
	if checked == 0 {
		t.Fatal("一个路径参数都没检查到——spec 解析坏了，这条测试成了空转")
	}
	t.Logf("%d 个路径参数都有说明", checked)
}

// TestQueryParamsAreReusableRefs checks that query parameters use $ref to components.parameters.
//
// This is MA's shape (`$ref: '#/components/parameters/ListAgentsQuery.limit'`):
// the definition is written only once, and client generators can generate shared parameter types.
//
// ⚠️ **Parameters are named per operation, deliberately not merged by parameter name.** The two `limit`s mean opposite things
// (`/api/suggestions` defaults to 50, `/api/workspaces/{id}/runs` defaults to unlimited);
// merging would feed one's description to the other's callers, and that mistake gives no error;
// it only makes people think paging behaves the same and then statistics get silently truncated. MA itself also splits by operation.
func TestQueryParamsAreReusableRefs(t *testing.T) {
	spec := parseSpec(t, specOnDisk(t))
	components := spec["components"].(map[string]any)

	defsAny, ok := components["parameters"]
	if !ok {
		t.Fatal("components 下没有 parameters——query 参数没有抽成可复用的定义。\n" +
			"修复：见 openapi/requests.go 的 collectQueryParams")
	}
	defs := defsAny.(map[string]any)

	inlined := 0
	for path, item := range spec["paths"].(map[string]any) {
		for method, opAny := range item.(map[string]any) {
			if method == "parameters" {
				continue
			}
			op := opAny.(map[string]any)
			raw, has := op["parameters"]
			if !has {
				continue
			}
			for _, p := range raw.([]any) {
				param := p.(map[string]any)
				ref, isRef := param["$ref"].(string)
				if !isRef {
					// Parameters on an operation should only be query (path parameters hang on the path item).
					inlined++
					t.Errorf("%s %s 的参数 %v 是内联的，应该 $ref 到 components.parameters。\n"+
						"修复：见 openapi/requests.go 的 queryParamRefs",
						strings.ToUpper(method), path, param["name"])
					continue
				}
				key := strings.TrimPrefix(ref, "#/components/parameters/")
				if _, found := defs[key]; !found {
					t.Errorf("%s %s 的参数 $ref 指向不存在的 %q——悬空的 $ref 让 spec 不合法",
						strings.ToUpper(method), path, key)
				}
			}
		}
	}

	// Reverse direction: a parameter defined but referenced by nobody is dead code, and misleads readers into thinking some endpoint takes it.
	referenced := map[string]bool{}
	rest := string(specOnDisk(t))
	const marker = `"$ref": "#/components/parameters/`
	for {
		i := strings.Index(rest, marker)
		if i < 0 {
			break
		}
		rest = rest[i+len(marker):]
		end := strings.Index(rest, `"`)
		if end < 0 {
			t.Fatal("spec 里有个参数 $ref 没有闭合引号，生成逻辑坏了")
		}
		referenced[rest[:end]] = true
	}
	for key := range defs {
		if !referenced[key] {
			t.Errorf("components.parameters 里的 %q 没有任何接口引用它", key)
		}
	}

	if len(defs) == 0 {
		t.Fatal("components.parameters 是空的——这条测试成了空转")
	}
	t.Logf("%d 条可复用 query 参数，全部被引用；内联的 %d 条", len(defs), inlined)
}

// TestDeliberateDivergencesAreDocumented checks that the places "deliberately not consistent with MA"
// are all explained in the baseline file.
//
// # Why this one exists
//
// Differences come in two kinds: **left after thinking it through**, and **left without noticing**. The two
// look exactly the same in the spec, and the next person would take the latter for the former ("since it has always been like this, there must be a reason").
//
// So for every place inconsistent with MA, the baseline file must have a paragraph stating the reason. This test only checks
// "is it written"; whether it is written well can only be judged by people, but "is it written" is exactly the half that gets missed.
//
// ⚠️ What is checked is **the baseline mentions this thing**, not an exact match of some sentence: the latter would turn red on fixing a typo,
// and then people would delete the test.
func TestDeliberateDivergencesAreDocumented(t *testing.T) {
	baseline := maConventionsText(t)

	// Each entry is "a place where AMA differs from MA" + "a keyword that must appear in the baseline".
	divergences := []struct {
		what     string
		keywords []string
	}{
		{
			what:     "字段名保持 camelCase（MA 是 snake_case）",
			keywords: []string{"camelCase", "doc", "jsonb"},
		},
		{
			what:     "保留 PATCH / PUT（MA 更新一律用 POST）",
			keywords: []string{"PATCH", "PUT", "POST"},
		},
		{
			what:     "时间戳固定三位小数，不采纳 RFC 3339 那句宽松描述",
			keywords: []string{"RFC 3339", "ISO 8601"},
		},
		{
			what:     "不给 spec 加 type 字段与 ListXxxResponse 包装",
			keywords: []string{"ListAgentsResponse", "type"},
		},
		{
			what:     "方舟透传字段保持方舟的 snake_case 名",
			keywords: []string{"session_thread_id", "input_tokens"},
		},
		{
			// ⚠️ This one is listed separately, because it is **the only real deviation** in that rule, and the easiest for the next person
			// to copy as "an Ark field". Measured: the root word `built` has 0 hits in docs/ark-managed-agents/,
			// and none in shared/infra/ma/ either: it is never sent to Ark, it is an AMA concept of its own.
			// The baseline must say it has no Ark origin, otherwise "9 with an origin + 1 without" degrades into
			// "all 10 are Ark's", and the rule itself loses its test.
			what:     "builtin_tools 是那条 snake_case 规则里唯一没有方舟出处的字段",
			keywords: []string{"builtin_tools", "builtinTools"},
		},
		{
			// snake_case is not drift; it is a rule written in the apimodel package header and DESIGN.md.
			// The baseline must point to those two places, otherwise readers would think "inconsistent style" is a defect waiting to be fixed.
			what:     "snake/camel 的分域规则已在代码与设计文档里写明",
			keywords: []string{"DESIGN.md", "camelCase"},
		},
	}

	for _, d := range divergences {
		for _, kw := range d.keywords {
			if !strings.Contains(baseline, kw) {
				t.Errorf("刻意保留的差异「%s」在 ma_conventions.md 里没有交代"+
					"（找不到关键词 %q）。\n"+
					"修复：在 ma_conventions.md 的「刻意不采纳」一节里写清楚"+
					"这个差异是什么、为什么保留、以及照 MA 改会坏在哪。\n"+
					"没有理由的差异会被下一个人当成「一直这样想必有原因」而永久保留。",
					d.what, kw)
			}
		}
	}
}

// TestBaselineRecordsItsOwnStaleness checks that the baseline file itself explains that "it goes stale".
//
// # What this pins is a handover problem, not writing style
//
// The baseline is **synced by hand**: when MA changes its conventions, nothing here turns red. That cost is accepted
// (CI cannot fetch MA's spec; see this file's header), but it must be written in the file, otherwise the next person would think
// this baseline follows MA automatically, and make judgments based on conventions from three months ago.
//
// So the baseline must have: where it is on the MA side (the spec path), which day's snapshot was read,
// and the sentence "a person must update it".
func TestBaselineRecordsItsOwnStaleness(t *testing.T) {
	baseline := maConventionsText(t)

	required := []struct {
		what string
		kw   string
	}{
		{"MA 的 spec 路径", "openapi.yaml"},
		{"读的是哪天的快照（否则无从判断基线有多旧）", "快照日期"},
		{"为什么不实时拉取", "不随本仓库提供"},
	}
	for _, r := range required {
		if !strings.Contains(baseline, r.kw) {
			t.Errorf("ma_conventions.md 里缺「%s」（找不到 %q）。\n"+
				"基线是手工同步的，不写清楚来源与版本，下一个人会以为它自动跟着 MA 走。",
				r.what, r.kw)
		}
	}
}

// parseSpecComponents is a small helper shared by several tests: it gets a sub-object under components.
func parseSpecComponents(t *testing.T, key string) map[string]any {
	t.Helper()
	spec := parseSpec(t, specOnDisk(t))
	comp, ok := spec["components"].(map[string]any)
	if !ok {
		t.Fatal("spec 里没有 components")
	}
	sub, ok := comp[key].(map[string]any)
	if !ok {
		t.Fatalf("components 下没有 %s", key)
	}
	return sub
}

// TestSpecIsValidJSONWithMAVersion checks that the OpenAPI version declared by the spec matches MA.
//
// MA is 3.1.0. Different versions make the toolchain behave differently (3.0 and 3.1 handle nullable, examples, and
// JSON Schema dialects differently), and when the two sides differ by a minor version that difference **only shows up when generating a client**,
// with an error saying some schema is invalid.
func TestSpecIsValidJSONWithMAVersion(t *testing.T) {
	spec := parseSpec(t, specOnDisk(t))
	const want = "3.1.0"
	if got, _ := spec["openapi"].(string); got != want {
		t.Errorf("spec 声明的 OpenAPI 版本是 %q，MA 用的是 %q。\n"+
			"两边差一个小版本会让 client 生成器的行为不同，"+
			"而那个差异只在生成时暴露。", got, want)
	}
	// Also confirm securitySchemes is still there: it is the only thing under components that "does not belong to some route",
	// and the change that added parameters touched exactly componentsObject.
	if _, ok := parseSpecComponents(t, "securitySchemes")["bearerAuth"]; !ok {
		t.Error("components.securitySchemes 里没有 bearerAuth——" +
			"改 componentsObject 时把它弄丢了")
	}
}

// JSON tag gate: turns the three hard rules at the top of this package from comments into machine checks.
//
// # Why this test must exist
//
// The three hard rules (see the apimodel.go file header) govern **field presence**, and the frontend depends on presence
// no less than on types. What they share is that **breaking them is not a compile error**: tags are string literals; get one
// word wrong and the Go side is all green, unit tests all green; it only shows up on the page as odd, hard-to-debug problems: "unsaved" lit forever,
// a TypeError on `.length`, NaN from addition, broken sorting.
//
// Every load-bearing convention in this repo has a machine gate: an undeclared authz route → the service panics at startup; an OpenAPI spec
// out of sync → `go test ./openapi/` turns red; role permission notes drifting → `go test ./shared/gopkg/authzdoc/`
// turns red; a new doc jsonb column → `go test ./repository/schema/` turns red. These three hard rules used to
// have only comments, an exception. This file moves the check from "the author's self-discipline" to a red light in CI.
//
// # How the work is split with TestSeedRoundTrip
//
// The byte-fidelity test in apimodel_test.go only covers **the records that appear in the seed**, so it cannot pin
// "a new field was added and the seed does not have it", which is exactly the most common change. This file does the opposite: it does not look at
// data, only at types, walking **all** exported structs by reflection. The two complement each other; keep both.
//
// # The check
//
// Starting from 32 root types it walks recursively (nested structs, slice elements, pointer targets, embedded fields), and for every
// field that gets serialized asserts the three hard rules:
//
//	rule one    slices (including maps) must not have omitempty
//	rule two    numbers and booleans must not have omitempty
//	rule three  pointers must not have omitempty
//
// The other half of rule three, "optional fields use omitempty", cannot be checked mechanically (the author's intent cannot be judged), so this only
// pins the half that can be checked: pointer + omitempty is always rejected.
//
// Exceptions go through the allowedOmitEmpty allowlist, the same pattern as repository/schema/jsonbguard_test.go:
// it blocks no reasonable exception, but that line shows up in the diff, **so an exception becomes a decision that has to be explained,
// instead of a default nobody notices**. The allowlist has a reverse reconciliation and can only shrink, never grow.
//
// # Mutation check (does this guard really turn red)
//
// A guard without a mutation check is as good as no guard. Each of the three rules was broken on purpose once, and **in testing** each turned red:
//
//	rule one    Skill.Tags []string                add omitempty → red, reports "rule one"
//	rule two    TokenUsage.OutputTokens int        add omitempty → red, reports "rule two"
//	rule three  RunThread.ParentThreadID *string   add omitempty → red, reports "rule three"
//
// All three times it named only the changed field, with no false positives; after changing back, all green. The error paths of the last two were
// `Run.Usage.OutputTokens` and `Run.Threads[].ParentThreadID`, which also prove the recursion
// really goes into nested structs and slice elements, instead of only scanning top-level fields (scanning only the top level would catch neither).
//
// Two more were aimed at **this gate itself failing silently**, because "a guard that looks green but is not actually looking"
// is worse than no guard:
//
//	add an exported struct nobody references   → red (the coverage reconciliation demands adding it to roots)
//	put a nonexistent field in the allowlist   → red (the reverse reconciliation demands deleting that line)
package apimodel_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
)

// allowedOmitEmpty is the slice/number/bool/pointer fields currently allowed to have omitempty.
// The key is "declaring type.field name", not the access path: the same field may be reached by several paths
// (SkillFile.Content is under both Skill.Files[] and SkillPackage.Files[]),
// and recording by declaration needs only one line.
//
// ⚠️ **This table only shrinks, never grows.** Before adding a line, answer: when this field's key disappears, does the frontend
// get one check fewer, or one crash more? Only the former is an exception. Each entry below has a long comment at its type definition
// explaining why; new ones must do the same: the allowlist holds only one sentence, and the argument goes next to the field.
//
// These 10 entries are all **existing**, and all were thought through when the code was written (not oversights). Three kinds of reasons:
//
//	A. Derived fields not stored: omitempty keeps false out of the doc jsonb,
//	   otherwise every row would persist a snapshot that is surely stale, and its staleness cannot be seen.
//	B. Byte-for-byte seed comparison: seed.json is exported mechanically from the frontend's seed.ts, and JSON.stringify already
//	   drops keys that are undefined; without omitempty TestSeedRoundTrip would turn red immediately,
//	   and seed.json cannot be edited by hand.
//	C. Response size: list endpoints return the whole workspace at once, and putting attachment bytes/snapshots into every record would make the response
//	   grow linearly with data volume, while the list page does not render it at all.
var allowedOmitEmpty = map[string]string{
	// ---- A. Derived fields, not stored (a missing key and false are fully equivalent in the frontend; no third state) ----
	"Run.AgentDeleted":         "A：派生字段不落库，见 apimodel.go 该字段注释（铁律一的第一个书面例外）",
	"Run.AgentDisabled":        "A：同 Run.AgentDeleted，且停用可逆，落库必然留下陈旧快照",
	"Schedule.AgentDeleted":    "A：与 Run.AgentDeleted 完全同构",
	"Schedule.RunNowRequested": "A：试跑请求是一次性的，认领时清空；true 的残留不会让任务多跑一轮",

	// ---- B. Byte-for-byte seed comparison; keys must not appear out of nowhere ----
	"Skill.Files": "B：seed.json 里没有这个键，且前端是 `files?: SkillFile[]`（`skill.files ?? []`）",

	// ---- C. Response size; list endpoints deliberately leave it out ----
	"SkillFile.Content": "C：列表返回整个空间的技能，附件字节会让响应线性膨胀；前端是 `content?: string`",
	"Revision.Snapshot": "C：一份 agent 快照几 KB、20 条几十 KB，列表只渲染名字和时间，要正文时单条取",

	// ---- The other two: semantically really "optional", they just happen to be a number/pointer ----
	"Run.DurationMs":       "未结束的运行没有这个键（种子里 run_4/run_5），用指针以免把 0 毫秒和「未结束」混为一谈",
	"ScheduleSpec.EveryMs": "扁平联合：只有 Kind 为 interval 时才有，一条 cron 任务的 spec 里不该出现 everyMs",

	// ---- D. Transient response fields: only in the save (PUT) response; not in GET or the database ----
	"ArkCredential.Warning":       "D：保存校验没验成时的一次性提示，不入库、GET 不带；前端是 `warning?:`，键缺失即「没有提示」",
	"GlobalArkCredential.Warning": "D：同 ArkCredential.Warning（账号级）",
	"AgentArkCredential.Warning":  "D：同 ArkCredential.Warning（agent 级）",

	// ---- Existing exceptions brought along when moved from core into this package in step 1 ----
	"SandboxShellResult.ExitCode": "命令还在跑时这个键不该存在；前端是 `exitCode?: number`，" +
		"序列化成 null 会让 `?? 0` 把「没跑完」变成「退出码 0」。存量契约，搬家时逐字未改",
}

// roots are the starting points of the walk: all exported structs in this package that are **not referenced as a field by other structs**.
// The 12 nested ones (TokenUsage / RunThread / SkillFile / ScheduleSpec and so on) are reached by recursion,
// and need not and should not be listed here.
//
// ⚠️ Could this list miss new types? No: TestJSONTagGuardCoversEveryType uses go/ast to
// count the exported structs in the source and reconciles them with the set this walk actually reached; missing one turns red.
func roots() []any {
	return []any{
		apimodel.Agent{},
		apimodel.AgentArkCredential{},
		apimodel.AgentArkSecret{},
		apimodel.AgentDraft{},
		apimodel.AgentSyncTask{},
		apimodel.ArkKeyOption{},
		apimodel.ArkSecret{},
		apimodel.GlobalArkCredential{},
		apimodel.GlobalArkSecret{},
		apimodel.Host{},
		apimodel.WorkspaceHost{},
		apimodel.UserFile{},
		apimodel.WeixinAccount{},
		apimodel.WeixinLogin{},
		apimodel.StartUserFileUpload{},
		apimodel.UserFilePart{},
		apimodel.Issue{},
		apimodel.IssueAttachment{},
		apimodel.IssueComment{},
		apimodel.IssueDraft{},
		apimodel.LarkAppQuery{},
		apimodel.LarkUserApp{},
		apimodel.PendingMessage{},
		apimodel.Revision{},
		apimodel.Run{},
		apimodel.RunEvent{},
		apimodel.Schedule{},
		apimodel.ScheduleDraft{},
		apimodel.Skill{},
		apimodel.SkillDraft{},
		apimodel.SkillPackage{},
		apimodel.Suggestion{},
		apimodel.SuggestionAttachment{},
		apimodel.SuggestionDraft{},
		apimodel.SuggestionReply{},
		apimodel.UserProfile{},
		apimodel.VolcAccount{},
		apimodel.Workspace{},

		// ---- The batch moved into this package from core / infra (step 1) ----
		apimodel.ArkConsoleRef{},
		apimodel.ArkFetchStatus{},
		apimodel.FeedbackInput{},
		apimodel.FeedbackPage{},
		apimodel.MemberInput{},
		apimodel.OnboardingState{},
		apimodel.SandboxFileContent{},
		apimodel.SandboxListing{},
		apimodel.SandboxShellResult{},
		apimodel.SandboxState{},
		apimodel.UsageSummary{},
		apimodel.UserPreferences{},
		apimodel.WorkspaceInput{},
		apimodel.WorkspacePatch{},
		apimodel.ArkCredentialDraft{},
		apimodel.ArkKeyRef{},
		apimodel.SkillCreateOptions{},
		apimodel.SkillFileContent{},
		apimodel.SuggestionPage{},
	}
}

// TestJSONTagIronLaws is that gate.
func TestJSONTagIronLaws(t *testing.T) {
	w := &walker{
		t:       t,
		visited: map[reflect.Type]bool{},
		used:    map[string]bool{},
		seen:    map[string]bool{},
	}
	for _, root := range roots() {
		rt := reflect.TypeOf(root)
		w.walk(rt, rt.Name())
	}

	// Reverse reconciliation: entries listed in the allowlist that no longer exist (field renamed, deleted, or omitempty removed)
	// must be deleted. Otherwise the allowlist slowly becomes a list unrelated to reality, and the next person reading it would think those
	// exceptions still exist, and "stay consistent" with it.
	var stale []string
	for key := range allowedOmitEmpty {
		if !w.used[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	for _, key := range stale {
		t.Errorf(`allowedOmitEmpty 里的 %q 已经不需要了——那个字段要么改了名、要么删了、
要么 omitempty 已经拿掉。把这一行删掉（拿掉 omitempty 的话这是好事）。`, key)
	}
}

// walker walks the type graph recursively.
type walker struct {
	t       *testing.T
	visited map[reflect.Type]bool // guards against cycles, and against reporting the same type more than once
	used    map[string]bool       // allowlist keys that were hit, for the reverse reconciliation
	seen    map[string]bool       // names of exported structs reached, for the coverage reconciliation
}

// walk walks one struct type. path is the access path from the root type to here, used only to locate errors.
func (w *walker) walk(rt reflect.Type, path string) {
	if rt.Kind() != reflect.Struct || w.visited[rt] {
		return
	}
	w.visited[rt] = true
	if ast.IsExported(rt.Name()) {
		w.seen[rt.Name()] = true
	}

	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() {
			continue // unexported fields are not serialized
		}

		// json:"-" means "the whole field is left out of JSON"; whatever the tag says does not matter, so skip it.
		// ⚠️ It is not the same as json:"-,": the latter means "the field is named -" and is still serialized,
		// so here we compare the whole tag, not the parsed name.
		if f.Tag.Get("json") == "-" {
			continue
		}

		name, opts := parseTag(f)

		fieldPath := path + "." + f.Name
		if f.Anonymous && f.Tag.Get("json") == "" {
			// Embedded with no tag: the fields are promoted to the outer struct, equivalent to writing them here directly.
			// Marked with <embedded>, so the error does not point to a field name that cannot be found on the outer type.
			fieldPath = path + ".<embedded " + f.Type.Name() + ">"
		}

		if opts["omitempty"] {
			w.checkOmitEmpty(rt, f, name, fieldPath)
		}

		w.descend(f.Type, fieldPath)
	}
}

// checkOmitEmpty checks the three hard rules for a field with omitempty.
func (w *walker) checkOmitEmpty(owner reflect.Type, f reflect.StructField, jsonName, path string) {
	law, consequence := violation(f.Type)
	if law == "" {
		return // things like string: omitempty is a legitimate optional-field use (first half of rule three)
	}

	// The allowlist is keyed by "declaring type.field name", independent of the access path.
	key := owner.Name() + "." + f.Name
	if _, ok := allowedOmitEmpty[key]; ok {
		w.used[key] = true
		return
	}

	w.t.Errorf(`%s 的 json tag 带了 omitempty，违反%s。

  字段：%s %s
  tag ：json:"%s,omitempty"
  路径：%s

后果：%s

这条规则写在 model/apimodel/apimodel.go 文件头的「JSON tag 的三条铁律」。
它违反了不编译报错、Go 侧单测也不会红，只在页面上表现成难查的怪问题——
所以才有这道闸门。

确认这里真的是例外（键消失能让前端少一次判断，而不是多一个崩溃）之后，
把 %q 加进 jsontag_test.go 的 allowedOmitEmpty，并在字段定义处写清理由。`,
		key, law, f.Name, f.Type, jsonName, path, consequence, key)
}

// violation decides which hard rule a type with omitempty breaks, returning the rule name and consequence.
// An empty string means no violation.
func violation(ft reflect.Type) (law, consequence string) {
	switch ft.Kind() {
	case reflect.Slice, reflect.Array, reflect.Map:
		return "铁律一（切片一律不加 omitempty）",
			"nil 切片本来就会编码成 null，而契约要求 []（前端直接读 .length / .map()，" +
				"拿到 null 是 TypeError）；加上 omitempty 更糟——" +
				"会让 members / skillIds / threads 这些**必填**键整个消失。"

	case reflect.Bool:
		return "铁律二（布尔一律不加 omitempty）",
			"false 是有意义的状态而不是「未设置」（verificationTokenSet: false 就是如此）。" +
				"加了 omitempty，false 会让键整个消失，前端分不出「明确为假」与「没这个字段」。"

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return "铁律二（数字一律不加 omitempty）",
			"0 是合法值而不是「没有」（TokenUsage 的 cache_read_input_tokens 就有 0）。" +
				"加了 omitempty，0 会让键消失，而前端拿它做加法会算出 NaN。"

	case reflect.Ptr:
		return "铁律三（可空字段用指针且不加 omitempty）",
			"指针表达的是「必须有，但可以是 null」（TS 里的 `parent_thread_id: string | null`），" +
				"与「可以没有」（`endedAt?: string`）是两种类型。加 omitempty 会让 nil 时键消失，" +
				"于是前端声明的必填键不存在——要「可以没有」就别用指针，要 null 就别加 omitempty。"
	}
	return "", ""
}

// descend recurses into field types: pointer targets, slice/array elements, map values, and nested structs.
func (w *walker) descend(ft reflect.Type, path string) {
	switch ft.Kind() {
	case reflect.Ptr:
		w.descend(ft.Elem(), path)
	case reflect.Slice, reflect.Array:
		// []byte / json.RawMessage are leaves (encoded as a base64 string or raw bytes);
		// recursing into uint8 is pointless.
		if ft.Elem().Kind() == reflect.Uint8 {
			return
		}
		w.descend(ft.Elem(), path+"[]")
	case reflect.Map:
		w.descend(ft.Elem(), path+"[k]")
	case reflect.Struct:
		w.walk(ft, path)
	}
}

// parseTag splits a json tag into its name and options.
func parseTag(f reflect.StructField) (name string, opts map[string]bool) {
	opts = map[string]bool{}
	tag, ok := f.Tag.Lookup("json")
	if !ok {
		// Exported fields without a tag are serialized by field name and cannot have omitempty.
		return f.Name, opts
	}
	parts := strings.Split(tag, ",")
	name = parts[0]
	if name == "" {
		name = f.Name
	}
	for _, o := range parts[1:] {
		opts[o] = true
	}
	return name, opts
}

// TestJSONTagGuardCoversEveryType makes sure roots() above does not quietly go out of date.
//
// # Why this is needed
//
// Reflection can only start from **concrete types**, so roots() is a hand-written list, and the old problem of hand-written lists is
// that nobody remembers to add a line for a new type, so the gate looks green while that type is not checked at all.
// This kind of "guard silently failing" is worse than no guard: it makes people think it has been checked.
//
// So here go/ast counts the source directly: every exported struct type in this package must really be
// reached by the walk above (either in roots or reachable from roots by recursion). Missing one is named.
func TestJSONTagGuardCoversEveryType(t *testing.T) {
	w := &walker{
		t:       t,
		visited: map[reflect.Type]bool{},
		used:    map[string]bool{},
		seen:    map[string]bool{},
	}
	for _, root := range roots() {
		rt := reflect.TypeOf(root)
		w.walk(rt, rt.Name())
	}

	declared := declaredStructs(t)
	var missing []string
	for _, name := range declared {
		if !w.seen[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	for _, name := range missing {
		t.Errorf(`导出的结构体 %s 没有被 JSON tag 闸门检查到。

它要么是个新的顶层类型（把 domain.%s{} 加进 jsontag_test.go 的 roots()），
要么本该被某个已有类型引用到而没有。

⚠️ 不加的话闸门对它静默失效——测试仍然是绿的，但那个类型的 tag 没人看着。`, name, name)
	}

	// Reverse reconciliation: a type listed in roots() that no longer exists in the source fails to compile, so no need to check.
	// And "should be nested but was written into roots" does not affect correctness, so it is not checked either.
	if len(declared) == 0 {
		t.Fatal("一个导出结构体都没数到，说明源码解析出问题了（判据失效比违规更危险）")
	}
	t.Logf("已检查 %d 个导出结构体（源码声明 %d 个）", len(w.seen), len(declared))
}

// declaredStructs uses go/ast to count the names of all exported struct types in this package's source.
//
// _test.go files are not counted: helper structs in them (like seedShape) are not part of the contract.
func declaredStructs(t *testing.T) []string {
	t.Helper()

	fset := token.NewFileSet()
	// go test's working directory is the package directory, so "." points to model/apimodel.
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("解析不了本包源码：%v", err)
	}

	var names []string
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, d := range f.Decls {
				gd, ok := d.(*ast.GenDecl)
				if !ok || gd.Tok != token.TYPE {
					continue
				}
				for _, spec := range gd.Specs {
					ts := spec.(*ast.TypeSpec)
					if _, ok := ts.Type.(*ast.StructType); !ok {
						continue
					}
					if ast.IsExported(ts.Name.Name) {
						names = append(names, ts.Name.Name)
					}
				}
			}
		}
	}
	sort.Strings(names)
	return names
}

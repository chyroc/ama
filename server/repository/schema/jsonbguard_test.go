// The jsonb gate: turns the rule "expand fields into columns, do not add new doc jsonb" into a
// machine check.
//
// # Why this test is required instead of only writing it in the README
//
// **Every load-bearing convention in this repository has a machine gate**: a missing authz route
// declaration → the service panics at startup; an out-of-date OpenAPI spec → `go test ./openapi/`
// turns red; drift in the role permission notes → `go test ./shared/gopkg/authzdoc/` turns red.
//
// If the rule "stop adding doc columns" were only in the README, what it would ask for is **a piece
// of prose** ("explain in the file header why the fields cannot be listed here"), and prose is the
// cheapest thing: someone wanting to save effort complies with three lines of comments. Worse,
// **adding a doc key is zero DDL, zero errors, mergeable the same day**, so those doc tables keep
// growing, and no code review ever turns red because of it.
//
// This test moves the check from "the author's self-discipline" to "a red light in CI". It blocks no
// reasonable exception: just add it to the allow list below, but that line shows up in the diff, **so
// an exception becomes a decision that must be explained, instead of a default nobody notices**.
//
// # The check
//
// Scan the **real DDL** of all .sql files (skipping comment lines); any jsonb column must be on the
// allow list. The allow list pins the current state: the doc columns of 9 tables + 3 legitimate uses.
package schema

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// allowedJSONB is the jsonb columns currently allowed to exist; the key is "table.column".
//
// ⚠️ **This list only shrinks, never grows.** Before adding a line, answer the two decision questions
// in the README: one-to-many means a join table; fields that can be listed mean columns; only "a whole
// payload whose number of fields cannot even be stated" or "a raw replay value whose inside no query
// looks at" is jsonb.
//
// The first 9 entries are **existing doc tables** (not forced to split, but they must not grow
// either); the last 3 are **examples of legitimate use**.
var allowedJSONB = map[string]string{
	// ---- Existing: one aggregate per table + the whole entity in doc. Do not copy; see the schema.go package comment for why ----
	"agents.doc":          "既有",
	"skills.doc":          "既有",
	"runs.doc":            "既有",
	"run_events.doc":      "既有（且读路径不反序列化它，权威数据在 raw text 列）",
	"schedules.doc":       "既有",
	"ark_secrets.doc":     "既有",
	"ark_bindings.doc":    "既有",
	"feishu_apps.doc":     "既有",
	"feishu_sessions.doc": "既有",

	// ---- Legitimate use: jsonb only holds the part "nobody queries"; all other fields of the table are real columns ----
	"feedback_reports.payload": "二级反馈的明细，不定条数，没有任何查询按它过滤；" +
		"能查的 level/error_code/created_at/dedup_key 全是真列",
	"feishu_progress_deliveries.applied_snapshot": "一整帧进度快照，原样存原样取，" +
		"没有查询看它内部；同表另外 24 个字段全是真列",
	"selfhost_tool_results.result_event": "要原样重发给方舟的 user.tool_result 事件，" +
		"形状由 SDK 定、整块存取；没有任何查询看它内部（只按 session_id/call_id 主键点查），" +
		"其余字段 state/时间全是真列",
}

// jsonbColumn matches jsonb column declarations in DDL, like `    doc        jsonb not null,`, and
// `alter table x add column if not exists y jsonb` (so a real column addition later is caught too).
var jsonbColumn = regexp.MustCompile(`(?i)^\s*(?:alter\s+table\s+\S+\s+add\s+column(?:\s+if\s+not\s+exists)?\s+)?([a-z_][a-z0-9_]*)\s+jsonb\b`)

// TestNoNewJSONBColumns is that gate.
func TestNoNewJSONBColumns(t *testing.T) {
	entries, err := files.ReadDir(".")
	if err != nil {
		t.Fatalf("读不到 embed 目录：%v", err)
	}

	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		table := strings.TrimSuffix(name, ".sql")
		blob, err := files.ReadFile(name)
		if err != nil {
			t.Fatalf("读不到 %s：%v", name, err)
		}
		for i, line := range strings.Split(string(blob), "\n") {
			// Skip comments: the top of each .sql in this directory has a long "table outline" and
			// design notes that are bound to contain the text "doc jsonb"; matching by text would
			// flag them all falsely.
			if strings.HasPrefix(strings.TrimSpace(line), "--") {
				continue
			}
			m := jsonbColumn.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			key := table + "." + m[1]
			seen[key] = true
			if _, ok := allowedJSONB[key]; !ok {
				t.Errorf(`%s:%d 新增了 jsonb 列 %q。

本仓库的建表规则是**字段展开成列，一对多用关联表**（见本目录 README.md
「表结构设计的默认规则」）。按顺序过一遍判定问句：

  1. 是一对多吗？        → 建关联表（PG 无法对 jsonb 数组元素建唯一约束）
  2. 字段能列全吗？      → 建列，多少个都建，没有上界
  3. 形状真的由外部定？  → 才是 jsonb
  4. 只是还没想好？      → 想好再建表

⚠️ 「用户能自己输入 key」不算不可枚举（那是 key/value 两列 + 一对多）；
⚠️ 「要逐字节保真」要的是 text/bytea，jsonb 会重排键、压空白，恰恰不保真。

确认属于正当用途（整块载荷、没有任何查询看它内部、同表其余字段仍是真列）之后，
把 %q 加进 jsonbguard_test.go 的 allowedJSONB，并写清理由。`,
					name, i+1, m[1], key)
			}
		}
	}

	// Reverse reconciliation: entries on the allow list that no longer exist in the database must be
	// deleted, otherwise the allow list slowly becomes a list unrelated to reality, and the next
	// person reading it would think those columns still exist.
	var stale []string
	for key := range allowedJSONB {
		if !seen[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	for _, key := range stale {
		t.Errorf("allowedJSONB 里的 %q 在 .sql 里已经不存在了——"+
			"如果是拆 doc 拆完了，把这一行删掉（这是好事）。", key)
	}
}

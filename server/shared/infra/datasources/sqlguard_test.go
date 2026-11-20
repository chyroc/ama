// Tests for the read-only SQL guard, LIMIT injection and markdown rendering.
//
// All table-driven and offline: the guard is a pure function of the SQL string, so the whole
// "what may the model run" contract can be pinned here without a MySQL.

package datasources

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// TestGuardSQLAllows pins down the queries the model is expected to write.
func TestGuardSQLAllows(t *testing.T) {
	cases := []struct {
		name string
		sql  string
	}{
		{"simple select", "SELECT 1"},
		{"lowercase and whitespace", "  select * from t  "},
		{"semicolon inside string", "SELECT 'a;b' AS x"},
		{"escaped quote inside string", `SELECT 'it''s ok' AS x`},
		{"line comment with semicolon", "SELECT 1 -- ; DROP TABLE t"},
		{"block comment with semicolon", "SELECT 1 /* ; drop table t */"},
		{"hash comment", "SELECT 1 # ; drop table t"},
		{"forbidden phrase only in a string", "SELECT 'into outfile' AS x FROM t"},
		{"cte", "WITH a AS (SELECT 1) SELECT * FROM a"},
		{"recursive cte", "WITH RECURSIVE a AS (SELECT 1) SELECT * FROM a"},
		{"cte with column list", "WITH a (x, y) AS (SELECT 1, 2) SELECT * FROM a"},
		{"explain", "EXPLAIN SELECT * FROM t"},
		{"explain analyze", "EXPLAIN ANALYZE SELECT * FROM t"},
		{"explain format", "EXPLAIN FORMAT=JSON SELECT * FROM t"},
		{"explain format with space", "EXPLAIN FORMAT = JSON SELECT * FROM t"},
		{"show", "SHOW TABLES"},
		{"describe", "DESCRIBE t"},
		{"desc", "DESC t"},
		{"information_schema", "SELECT table_name FROM information_schema.tables"},
		{"subquery with limit", "SELECT * FROM t WHERE x IN (SELECT y FROM u LIMIT 5)"},
		{"json path with dots", "SELECT JSON_EXTRACT(doc, '$.a.b') FROM t"},
		{"backticked identifiers", "SELECT `select`, `from` FROM `t`"},
		{"cte named like a column", "WITH x AS (SELECT 1 AS update_col) SELECT update_col FROM x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := guardSQL(tc.sql, 200); err != nil {
				t.Fatalf("应当放行，却被拒绝：%v（SQL=%q）", err, tc.sql)
			}
		})
	}
}

// TestGuardSQLRejects pins down everything the guard must refuse.
//
// The first three are the security-relevant ones (a write could only succeed if the RDS account
// were mis-provisioned, but the whole point of the guard is that a mis-provisioned account does
// not turn into a silent write path).
func TestGuardSQLRejects(t *testing.T) {
	cases := []struct {
		name string
		sql  string
	}{
		{"empty", "   "},
		{"multiple statements", "SELECT 1; DROP TABLE t"},
		{"double semicolon", "SELECT 1;;"},
		{"insert", "INSERT INTO t VALUES (1)"},
		{"update", "UPDATE t SET a = 1"},
		{"delete", "DELETE FROM t"},
		{"drop", "DROP TABLE t"},
		{"truncate", "TRUNCATE TABLE t"},
		{"call", "CALL do_something()"},
		{"load data", "LOAD DATA INFILE '/tmp/x' INTO TABLE t"},
		{"set", "SET SESSION wait_timeout = 1"},
		{"with delete", "WITH a AS (SELECT 1) DELETE FROM t"},
		{"with update", "WITH a AS (SELECT 1) UPDATE t SET x = 1"},
		{"with insert", "WITH a AS (SELECT 1) INSERT INTO t SELECT * FROM a"},
		{"with no main statement", "WITH a AS (SELECT 1)"},
		{"explain delete", "EXPLAIN DELETE FROM t"},
		{"explain table", "EXPLAIN t"},
		{"explain for connection", "EXPLAIN FOR CONNECTION 3"},
		{"explain format without value", "EXPLAIN FORMAT= SELECT 1"},
		{"into outfile", "SELECT * INTO OUTFILE '/tmp/x' FROM t"},
		{"into outfile with comment", "SELECT * INTO/**/OUTFILE '/tmp/x' FROM t"},
		{"into dumpfile", "SELECT * INTO DUMPFILE '/tmp/x' FROM t"},
		{"for update", "SELECT * FROM t FOR UPDATE"},
		{"for share", "SELECT * FROM t FOR SHARE"},
		{"lock in share mode", "SELECT * FROM t LOCK IN SHARE MODE"},
		{"load_file", "SELECT LOAD_FILE('/etc/passwd')"},
		{"load_file with space", "SELECT LOAD_FILE ('/etc/passwd')"},
		{"mysql schema", "SELECT * FROM mysql.user"},
		{"mysql schema spaced", "SELECT * FROM mysql . user"},
		{"mysql schema backticked", "SELECT * FROM `mysql`.`user`"},
		{"performance_schema", "SELECT * FROM performance_schema.threads"},
		{"sys schema", "SELECT * FROM sys.schema_table_statistics"},
		{"executable comment", "SELECT 1 /*! DROP TABLE t */"},
		{"unterminated string", "SELECT 'abc"},
		{"unterminated backtick", "SELECT `abc"},
		{"unterminated block comment", "SELECT 1 /* abc"},
		{"unbalanced parens", "SELECT (1"},
		{"leading paren", "(SELECT 1)"},
		{"too long", "SELECT '" + strings.Repeat("a", maxSQLBytes) + "'"},
		// MySQL only treats `--` as a comment when it is followed by whitespace. If the guard
		// treated `1--1` as a comment it would mask the second statement below.
		{"dash dash operator then statement", "SELECT 1--1; DROP TABLE t"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := guardSQL(tc.sql, 200); err == nil {
				t.Fatalf("应当拒绝，却放行了：%q", tc.sql)
			}
		})
	}
}

// TestApplyLimit pins down when LIMIT is appended and when it must not be.
func TestApplyLimit(t *testing.T) {
	cases := []struct {
		name     string
		sql      string
		maxRows  int
		wantSQL  string
		injected bool
	}{
		{
			name:     "select without limit gets maxRows+1",
			sql:      "SELECT * FROM t",
			maxRows:  200,
			wantSQL:  "SELECT * FROM t\nLIMIT 201",
			injected: true,
		},
		{
			name:     "existing limit is left alone",
			sql:      "SELECT * FROM t LIMIT 10",
			maxRows:  200,
			wantSQL:  "SELECT * FROM t LIMIT 10",
			injected: false,
		},
		{
			name:     "existing limit with offset is left alone",
			sql:      "SELECT * FROM t LIMIT 5 OFFSET 2",
			maxRows:  200,
			wantSQL:  "SELECT * FROM t LIMIT 5 OFFSET 2",
			injected: false,
		},
		{
			name:     "trailing semicolon is removed before appending",
			sql:      "SELECT * FROM t;",
			maxRows:  5,
			wantSQL:  "SELECT * FROM t\nLIMIT 6",
			injected: true,
		},
		{
			name:     "trailing semicolon is removed even without injection",
			sql:      "SELECT * FROM t LIMIT 1;",
			maxRows:  5,
			wantSQL:  "SELECT * FROM t LIMIT 1",
			injected: false,
		},
		{
			name:     "cte gets a limit",
			sql:      "WITH a AS (SELECT 1) SELECT * FROM a",
			maxRows:  10,
			wantSQL:  "WITH a AS (SELECT 1) SELECT * FROM a\nLIMIT 11",
			injected: true,
		},
		{
			// A LIMIT inside a subquery does not constrain the outer result set.
			name:     "subquery limit does not count as outer limit",
			sql:      "SELECT * FROM t WHERE x IN (SELECT y FROM u LIMIT 5)",
			maxRows:  20,
			wantSQL:  "SELECT * FROM t WHERE x IN (SELECT y FROM u LIMIT 5)\nLIMIT 21",
			injected: true,
		},
		{
			name:     "trailing line comment is not swallowed",
			sql:      "SELECT 1 -- latest row",
			maxRows:  3,
			wantSQL:  "SELECT 1 -- latest row\nLIMIT 4",
			injected: true,
		},
		{
			// A semicolon followed by a trailing comment: the semicolon must be cut at its own
			// offset (masked), not by TrimSuffix. Otherwise the injected LIMIT lands after a
			// semicolon and becomes a second statement (1064, multiStatements is off).
			name:     "semicolon then trailing line comment",
			sql:      "SELECT 1; -- note",
			maxRows:  200,
			wantSQL:  "SELECT 1\nLIMIT 201",
			injected: true,
		},
		{
			name:     "semicolon then trailing block comment",
			sql:      "SELECT 1; /* note */",
			maxRows:  200,
			wantSQL:  "SELECT 1\nLIMIT 201",
			injected: true,
		},
		{
			name:     "semicolon then trailing block comment with space",
			sql:      "SELECT 1; /* note */ ",
			maxRows:  200,
			wantSQL:  "SELECT 1\nLIMIT 201",
			injected: true,
		},
		{
			name:     "semicolon then trailing comment with existing limit",
			sql:      "SELECT 1 LIMIT 5; -- note",
			maxRows:  200,
			wantSQL:  "SELECT 1 LIMIT 5",
			injected: false,
		},
		{
			// SHOW / DESCRIBE / EXPLAIN do not accept LIMIT, so the guard must not add one.
			name:     "show is not injected",
			sql:      "SHOW TABLES",
			maxRows:  200,
			wantSQL:  "SHOW TABLES",
			injected: false,
		},
		{
			name:     "describe is not injected",
			sql:      "DESCRIBE t",
			maxRows:  200,
			wantSQL:  "DESCRIBE t",
			injected: false,
		},
		{
			name:     "explain is not injected",
			sql:      "EXPLAIN SELECT 1",
			maxRows:  200,
			wantSQL:  "EXPLAIN SELECT 1",
			injected: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, injected, err := guardSQL(tc.sql, tc.maxRows)
			if err != nil {
				t.Fatalf("guardSQL 失败：%v", err)
			}
			if got != tc.wantSQL {
				t.Errorf("SQL 不符\n期望：%q\n实际：%q", tc.wantSQL, got)
			}
			if injected != tc.injected {
				t.Errorf("injected 期望 %v，实际 %v", tc.injected, injected)
			}
		})
	}
}

// TestGuardSQLOutputHasNoSemicolon: the text passed to the driver must not contain a statement
// terminator, otherwise an appended LIMIT becomes a second statement. This is the regression guard
// for the TrimSuffix bug (semicolon + trailing comment).
func TestGuardSQLOutputHasNoSemicolon(t *testing.T) {
	inputs := []string{
		"SELECT 1",
		"SELECT 1;",
		"SELECT 1; -- note",
		"SELECT 1; /* note */",
		"SELECT 1; /* note */ ",
		"SELECT 1 LIMIT 5; -- note",
		"WITH c AS (SELECT 1) SELECT * FROM c; -- note",
	}
	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			got, _, err := guardSQL(in, 10)
			if err != nil {
				t.Fatalf("guardSQL 失败：%v", err)
			}
			if strings.Contains(got, ";") {
				t.Errorf("输出里不能留分号（否则注入的 LIMIT 会变成第二条语句）：%q", got)
			}
			if strings.HasSuffix(got, "LIMIT 11") == false && !strings.Contains(got, "LIMIT 5") {
				t.Errorf("应当注入 LIMIT 11 或保留原有 LIMIT 5：%q", got)
			}
		})
	}
}

// TestSQLScanMasksStringsAndComments: the masking is what makes "top-level semicolon" and
// "depth-0 keyword" trustworthy; if it broke, `SELECT 'a;b'` would be rejected.
func TestSQLScanMasksStringsAndComments(t *testing.T) {
	scan, err := scanSQL("SELECT 'a;b', \"c;d\", `e;f` FROM t -- x; y\n/* z; */")
	if err != nil {
		t.Fatalf("扫描失败：%v", err)
	}
	// All four semicolons are inside strings/comments, so the masked text must have none left.
	if got := strings.Count(scan.masked, ";"); got != 0 {
		t.Fatalf("字符串/注释里的分号应当被抹掉，实际还剩 %d 个：%q", got, scan.masked)
	}
	// The backticked identifier is a token (so the schema check sees `mysql`.`user`), while the
	// single/double quoted literals are not.
	var quoted []string
	for _, tok := range scan.tokens {
		if tok.kind == tokQuoted {
			quoted = append(quoted, tok.text)
		}
	}
	if len(quoted) != 1 || quoted[0] != "e;f" {
		t.Fatalf("反引号标识符应当是一个 quoted token，实际 %v", quoted)
	}
}

// TestRenderSQLResult checks the markdown shape: metadata line, header, rows, NULL.
func TestRenderSQLResult(t *testing.T) {
	text := renderSQLResult(
		[]string{"id", "name"},
		[][]string{{"1", "张三"}, {"2", "NULL"}},
		false, 200, 12*time.Millisecond,
	)
	for _, want := range []string{
		"共 2 行",
		"｜耗时 12ms",
		"| id | name |",
		"| --- | --- |",
		"| 1 | 张三 |",
		"| 2 | NULL |",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("结果里应当有 %q，实际：\n%s", want, text)
		}
	}
	if strings.Contains(text, "已截断") {
		t.Errorf("没截断时不该出现「已截断」：\n%s", text)
	}
}

// TestRenderSQLResultTruncated checks the truncated marker and the zero-row wording.
func TestRenderSQLResultTruncated(t *testing.T) {
	text := renderSQLResult([]string{"id"}, [][]string{{"1"}}, true, 1, time.Millisecond)
	if !strings.Contains(text, "共 1 行（已截断，上限 1）") {
		t.Errorf("截断标记不对：\n%s", text)
	}
	empty := renderSQLResult([]string{"id"}, nil, false, 200, time.Millisecond)
	if !strings.Contains(empty, "共 0 行") || !strings.Contains(empty, "没有返回任何行") {
		t.Errorf("零行结果文案不对：\n%s", empty)
	}
}

// TestMarkdownCellEscaping: a pipe or newline inside a value must not break the table.
func TestMarkdownCellEscaping(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"plain", "plain"},
		{"a|b", `a\|b`},
		{"line1\nline2", "line1<br>line2"},
		{"line1\r\nline2", "line1<br>line2"},
		{"line1\rline2", "line1<br>line2"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := markdownCell(tc.in); got != tc.want {
			t.Errorf("markdownCell(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
	// Long cells are truncated by rune, not by byte (no broken UTF-8 in the output).
	long := strings.Repeat("值", maxCellRunes+50)
	got := markdownCell(long)
	if utf8.RuneCountInString(got) != maxCellRunes+1 { // maxCellRunes runes + "…"
		t.Errorf("长单元格应当截到 %d 个字符 + 省略号，实际 %d 个字符", maxCellRunes, utf8.RuneCountInString(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("截断的单元格应当以省略号结尾：%q", got[len(got)-10:])
	}
	if !utf8.ValidString(got) {
		t.Error("截断后出现了非法 UTF-8")
	}
}

// TestCapResultText: the overall size cap protects the model's context (and the bill).
func TestCapResultText(t *testing.T) {
	small := strings.Repeat("a", 1024)
	if capResultText(small) != small {
		t.Error("没超过上限时不该改动内容")
	}
	big := strings.Repeat("中", maxResultBytes) // 3 bytes each → way over the cap
	capped := capResultText(big)
	if len(capped) > maxResultBytes+200 {
		t.Errorf("截断后仍然太大：%d 字节", len(capped))
	}
	if !strings.Contains(capped, "结果文本超过 256KB") {
		t.Errorf("截断后应当注明原因，实际结尾：%q", capped[len(capped)-80:])
	}
	if !utf8.ValidString(capped) {
		t.Error("截断后出现了非法 UTF-8")
	}
}

// TestSingleLineSQL: audit lines must stay single-line and bounded.
func TestSingleLineSQL(t *testing.T) {
	if got := singleLineSQL("SELECT\n  1\nFROM t"); got != "SELECT 1 FROM t" {
		t.Errorf("单行化不对：%q", got)
	}
	long := "SELECT '" + strings.Repeat("a", maxSQLAuditBytes*2) + "'"
	got := singleLineSQL(long)
	if len(got) > maxSQLAuditBytes+32 {
		t.Errorf("审计 SQL 没截断：%d 字节", len(got))
	}
	if !strings.Contains(got, "已截断") {
		t.Errorf("截断后应当注明：%q", got[len(got)-20:])
	}
}

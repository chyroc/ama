// Read-only SQL validation, LIMIT injection and result rendering.
//
// # This is a guard rail, not a sandbox
//
// The real security boundary is the **read-only account** (the RDS user only has SELECT) plus the
// `SET SESSION transaction_read_only = 1` issued at the start of every call. This file stops a
// model that writes a write statement by mistake: it validates against MySQL's lexical rules
// (string literals, comments, backtick identifiers), so `SELECT 'a;b'` is not misjudged and
// `SELECT 1 /* ; */` is not either. But do **not** treat it as a sandbox against malicious SQL;
// malicious SQL cannot write through a read-only account anyway.
//
// # Why tokens instead of regular expressions
//
// Regexes fall apart on comments and strings: `INTO/**/OUTFILE` slips through, and
// `SELECT 'for update'` is rejected by mistake. So the input is scanned once, with string literals
// and comments blanked out (same length, so offsets stay usable for finding the semicolon) while a
// token stream with bracket depth is recorded; every later check works on tokens.
package datasources

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// maxSQLBytes caps the length of one SQL statement. 20KB is plenty for complex aggregations
	// while keeping absurd input ("union the whole table into one result") from holding a
	// connection hostage.
	maxSQLBytes = 20 << 10

	// maxSQLAuditBytes caps the SQL echoed in the audit log (single line, truncated).
	maxSQLAuditBytes = 2 << 10

	// maxResultBytes caps the total tool result text. Anything larger is truncated with a note,
	// so hundreds of KB do not end up in the model context (expensive and useless).
	maxResultBytes = 256 << 10

	// maxCellRunes caps a single cell: one column holding a whole JSON document would otherwise
	// blow up the entire row.
	maxCellRunes = 200
)

// Keywords a statement may start with (case-insensitive; tokens are already upper-cased).
var allowedFirstKeywords = map[string]bool{
	"SELECT":   true,
	"WITH":     true,
	"SHOW":     true,
	"DESCRIBE": true,
	"DESC":     true,
	"EXPLAIN":  true,
}

// statementKeywords are words that may start a new statement, used to find the main query after
// `WITH`.
//
// They are only used for "stop at the first match": only SELECT is allowed there, everything else
// is rejected. Listing every possibility means `WITH ... UPDATE` always has a token to match, so a
// miss cannot turn into "nothing found, therefore allow".
var statementKeywords = map[string]bool{
	"SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": true, "REPLACE": true,
	"CREATE": true, "ALTER": true, "DROP": true, "TRUNCATE": true, "RENAME": true,
	"GRANT": true, "REVOKE": true, "CALL": true, "LOAD": true, "HANDLER": true,
	"DO": true, "SET": true, "USE": true, "LOCK": true, "UNLOCK": true,
	"PREPARE": true, "EXECUTE": true, "DEALLOCATE": true, "FLUSH": true,
	"OPTIMIZE": true, "REPAIR": true, "ANALYZE": true, "CHECK": true,
	"CHECKSUM": true, "INSTALL": true, "UNINSTALL": true, "SHUTDOWN": true,
	"KILL": true, "RESET": true, "PURGE": true, "START": true, "STOP": true,
	"COMMIT": true, "ROLLBACK": true, "SAVEPOINT": true, "RELEASE": true,
	"EXPLAIN": true, "DESCRIBE": true, "DESC": true, "SHOW": true, "WITH": true,
	"TABLE": true, "VALUES": true,
}

// Word sequences that are rejected outright: writing files, reading local files, and locking
// reads. They match at any bracket depth, and comments or whitespace may sit between the words.
//
// LOAD_FILE is a single word; LOAD DATA / LOAD XML import files; INTO OUTFILE / DUMPFILE write
// files; FOR UPDATE / FOR SHARE / LOCK IN SHARE MODE take write or shared locks.
var forbiddenWordSequences = [][]string{
	{"INTO", "OUTFILE"},
	{"INTO", "DUMPFILE"},
	{"LOAD", "DATA"},
	{"LOAD", "XML"},
	{"LOAD_FILE"},
	{"FOR", "UPDATE"},
	{"FOR", "SHARE"},
	{"LOCK", "IN", "SHARE", "MODE"},
}

// System schemas that are rejected: they expose process lists / metadata and, with a
// mis-provisioned account, other databases. information_schema is allowed: read-only metadata is
// genuinely useful for "what tables does this database have".
var forbiddenSchemas = map[string]bool{
	"MYSQL":              true,
	"PERFORMANCE_SCHEMA": true,
	"SYS":                true,
}

// sqlTokenKind distinguishes token kinds.
type sqlTokenKind uint8

const (
	// tokWord is an unquoted word (keyword / identifier); text is upper-cased.
	tokWord sqlTokenKind = iota
	// tokQuoted is a backtick identifier; text keeps its original case.
	tokQuoted
	// tokPunct is a single punctuation byte ( ( ) , . ; = ... ).
	tokPunct
)

// sqlToken is one lexical unit produced by the scan. depth is its bracket depth (the "(" itself
// counts as the outer level).
type sqlToken struct {
	kind  sqlTokenKind
	text  string
	depth int
}

// sqlScan is the result of one scan.
type sqlScan struct {
	// sql is the original SQL with leading/trailing whitespace removed.
	sql string
	// masked has the same length as sql: string literals, comments and backtick identifier
	// contents are blanked to spaces, which makes "top-level semicolon" and "bracket depth"
	// trustworthy on it.
	masked string
	// tokens is the token stream (comments and string literals produce no tokens).
	tokens []sqlToken
}

// guardSQL validates a model-supplied SQL string and returns the text to execute.
//
// injected reports whether a LIMIT was appended (always maxRows+1: the extra row is what tells us
// whether the result was truncated).
func guardSQL(sql string, maxRows int) (guarded string, injected bool, err error) {
	trimmed := strings.TrimSpace(sql)
	if trimmed == "" {
		return "", false, fmt.Errorf("SQL 不能为空")
	}
	if len(trimmed) > maxSQLBytes {
		return "", false, fmt.Errorf("SQL 太长（%d 字节，上限 %d 字节），请拆小或改用聚合查询", len(trimmed), maxSQLBytes)
	}
	scan, err := scanSQL(trimmed)
	if err != nil {
		return "", false, err
	}
	if err := validateScan(scan); err != nil {
		return "", false, err
	}

	// Drop the trailing semicolon using the **masked** text: masked has the same length and byte
	// offsets as trimmed, with string literals and comments already blanked, and
	// checkSingleStatement has verified that only one top-level ";" exists and that nothing but
	// whitespace follows it.
	//
	// ⚠️ Do not use strings.TrimSuffix here: it only removes a semicolon that is the last byte,
	// so `SELECT 1; -- note` would keep its ";" in the middle and the injected LIMIT would become
	// a second statement (multiStatements is off, so MySQL answers 1064 "You have an error in
	// your SQL syntax"). Truncating at the semicolon also means the server never receives one.
	body := trimmed
	if i := strings.IndexByte(scan.masked, ';'); i >= 0 {
		body = strings.TrimSpace(trimmed[:i])
	}

	first := scan.tokens[0]
	// Only SELECT / WITH can take an appended LIMIT safely: SHOW / DESCRIBE / EXPLAIN do not
	// accept LIMIT and forcing one in would be a syntax error.
	if (first.text == "SELECT" || first.text == "WITH") && !hasTopLevelLimit(scan.tokens) {
		if maxRows < 1 {
			maxRows = 1
		}
		// Append on a new line instead of a space: a trailing line comment (`SELECT 1 -- note`)
		// would swallow everything after it on the same line, so only a new line keeps the LIMIT
		// effective.
		return body + "\nLIMIT " + strconv.Itoa(maxRows+1), true, nil
	}
	return body, false, nil
}

// scanSQL scans the SQL and produces the masked text and the token stream.
func scanSQL(sql string) (sqlScan, error) {
	n := len(sql)
	masked := make([]byte, n)
	copy(masked, sql)
	blank := func(from, to int) {
		for i := from; i < to && i < n; i++ {
			masked[i] = ' '
		}
	}
	tokens := make([]sqlToken, 0, 16)
	depth := 0
	for i := 0; i < n; {
		c := sql[i]
		switch {
		case c == '\'' || c == '"':
			end, err := skipQuoted(sql, i, c)
			if err != nil {
				return sqlScan{}, err
			}
			blank(i, end)
			i = end
		case c == '`':
			end, err := skipBacktick(sql, i)
			if err != nil {
				return sqlScan{}, err
			}
			tokens = append(tokens, sqlToken{kind: tokQuoted, text: sql[i+1 : end-1], depth: depth})
			blank(i, end)
			i = end
		case c == '#':
			end := lineEnd(sql, i)
			blank(i, end)
			i = end
		case c == '-' && i+1 < n && sql[i+1] == '-':
			// MySQL only treats `--` as a comment when it is followed by whitespace, a control
			// character, or end of line; `1--1` is the expression `1 - (-1)`, not a comment.
			// Follow MySQL's rule, otherwise `SELECT 1--1; DROP TABLE t` would be masked as
			// "DROP is commented out" and allowed.
			if i+2 < n && sql[i+2] > ' ' {
				tokens = append(tokens, sqlToken{kind: tokPunct, text: "-", depth: depth})
				i++
				continue
			}
			end := lineEnd(sql, i)
			blank(i, end)
			i = end
		case c == '/' && i+1 < n && sql[i+1] == '*':
			// MySQL's executable comment `/*! ... */` is executed by the server, so its content
			// cannot be blanked as a comment; there is also no legitimate reason for it in a
			// read-only query. Rejected outright.
			if i+2 < n && sql[i+2] == '!' {
				return sqlScan{}, fmt.Errorf("不支持 MySQL 可执行注释 /*! … */")
			}
			rel := strings.Index(sql[i+2:], "*/")
			if rel < 0 {
				return sqlScan{}, fmt.Errorf("SQL 里的块注释没有闭合")
			}
			end := i + 2 + rel + 2
			blank(i, end)
			i = end
		case c == '(':
			tokens = append(tokens, sqlToken{kind: tokPunct, text: "(", depth: depth})
			depth++
			i++
		case c == ')':
			if depth > 0 {
				depth--
			}
			tokens = append(tokens, sqlToken{kind: tokPunct, text: ")", depth: depth})
			i++
		case isWordByte(c):
			end := i
			for end < n && isWordByte(sql[end]) {
				end++
			}
			tokens = append(tokens, sqlToken{kind: tokWord, text: strings.ToUpper(sql[i:end]), depth: depth})
			i = end
		default:
			if c > ' ' {
				tokens = append(tokens, sqlToken{kind: tokPunct, text: string(c), depth: depth})
			}
			i++
		}
	}
	if depth != 0 {
		return sqlScan{}, fmt.Errorf("SQL 里的括号不配对")
	}
	return sqlScan{sql: sql, masked: string(masked), tokens: tokens}, nil
}

// validateScan is the body of the read-only check: single statement → leading keyword → the inner
// statement of WITH/EXPLAIN → forbidden fragments / system schemas.
func validateScan(scan sqlScan) error {
	if err := checkSingleStatement(scan.masked); err != nil {
		return err
	}
	if len(scan.tokens) == 0 {
		return fmt.Errorf("SQL 不能为空")
	}
	first := scan.tokens[0]
	if first.kind != tokWord || !allowedFirstKeywords[first.text] {
		return fmt.Errorf("只允许单条只读语句，且必须以 SELECT / WITH / SHOW / DESCRIBE / EXPLAIN 开头")
	}
	switch first.text {
	case "WITH":
		if err := checkWith(scan.tokens); err != nil {
			return err
		}
	case "EXPLAIN":
		if err := checkExplain(scan.tokens); err != nil {
			return err
		}
	}
	if err := checkForbidden(scan.tokens); err != nil {
		return err
	}
	return nil
}

// checkSingleStatement requires "a top-level semicolon may only appear at the end".
//
// Strings and comments are already spaces in masked, so `SELECT 'a;b'` is not misjudged, while
// `SELECT 1; DROP TABLE t` still has content after the first semicolon.
func checkSingleStatement(masked string) error {
	for i := 0; i < len(masked); i++ {
		if masked[i] != ';' {
			continue
		}
		if strings.TrimSpace(masked[i+1:]) != "" {
			return fmt.Errorf("只允许一条语句：分号之后不能再有内容")
		}
		return nil
	}
	return nil
}

// checkWith validates `WITH ... SELECT`: the first main statement after the CTE definitions must
// be SELECT.
//
// A CTE body sits inside parentheses (depth >= 1), so only depth-0 tokens are considered: `WITH`,
// `RECURSIVE`, CTE names, `AS` and commas are all at depth 0, while UPDATE/DELETE inside a body
// are at depth 1 and cannot be mistaken for the main statement.
func checkWith(tokens []sqlToken) error {
	for i := 1; i < len(tokens); i++ {
		t := tokens[i]
		if t.depth != 0 || t.kind != tokWord {
			continue
		}
		if !statementKeywords[t.text] {
			// A CTE name / AS / RECURSIVE etc.: keep looking.
			continue
		}
		if t.text == "SELECT" {
			return nil
		}
		return fmt.Errorf("WITH 之后的主语句只允许 SELECT，实际是 %s", t.text)
	}
	return fmt.Errorf("WITH 之后没找到主查询语句（只支持 WITH … SELECT）")
}

// checkExplain validates `EXPLAIN [ANALYZE] [FORMAT=...] <SELECT|WITH ... SELECT>`.
//
// It does not let `EXPLAIN <table>` (a DESCRIBE synonym) or `EXPLAIN FOR CONNECTION` through:
// the former is clearer as DESCRIBE, the latter reads another connection's state and has no
// business in a data query.
func checkExplain(tokens []sqlToken) error {
	i := 1
	for i < len(tokens) && tokens[i].depth == 0 {
		t := tokens[i]
		if t.kind != tokWord {
			break
		}
		if t.text == "ANALYZE" {
			i++
			continue
		}
		if t.text == "FORMAT" {
			i++
			if i < len(tokens) && tokens[i].kind == tokPunct && tokens[i].text == "=" {
				i++
			}
			if i < len(tokens) && tokens[i].kind == tokWord {
				i++
				continue
			}
			return fmt.Errorf("EXPLAIN FORMAT 后面缺少格式名（如 JSON / TREE）")
		}
		break
	}
	if i >= len(tokens) {
		return fmt.Errorf("EXPLAIN 后面只允许 SELECT 或 WITH … SELECT")
	}
	t := tokens[i]
	if t.depth != 0 || t.kind != tokWord {
		return fmt.Errorf("EXPLAIN 后面只允许 SELECT 或 WITH … SELECT")
	}
	switch t.text {
	case "SELECT":
		return nil
	case "WITH":
		return checkWith(tokens[i:])
	default:
		return fmt.Errorf("EXPLAIN 后面只允许 SELECT 或 WITH … SELECT，实际是 %s", t.text)
	}
}

// checkForbidden checks the forbidden fragments and system schemas.
//
// Comments and whitespace are already gone between tokens, so `INTO/**/OUTFILE` is recognised
// exactly like `INTO  OUTFILE`; and because string literals produce no tokens, `SELECT 'into
// outfile'` is not rejected by mistake.
func checkForbidden(tokens []sqlToken) error {
	// Only unquoted words take part in phrase matching: backtick contents are identifiers and
	// must not combine with a neighbouring identifier into something like FOR UPDATE.
	words := make([]sqlToken, 0, len(tokens))
	for _, t := range tokens {
		if t.kind == tokWord {
			words = append(words, t)
		}
	}
	for _, seq := range forbiddenWordSequences {
		for i := 0; i+len(seq) <= len(words); i++ {
			ok := true
			for j, want := range seq {
				if !strings.EqualFold(words[i+j].text, want) {
					ok = false
					break
				}
			}
			if ok {
				return fmt.Errorf("不允许 %s：只读工具不能写文件、导数据或加锁读", strings.Join(seq, " "))
			}
		}
	}
	// System schemas: `mysql` / `performance_schema` / `sys` followed by a dot. Token-based
	// rather than substring matching, so a table named `auxmysql.users` is not rejected by
	// mistake.
	for i := 0; i+1 < len(tokens); i++ {
		t := tokens[i]
		if (t.kind != tokWord && t.kind != tokQuoted) || !forbiddenSchemas[strings.ToUpper(t.text)] {
			continue
		}
		if tokens[i+1].kind == tokPunct && tokens[i+1].text == "." {
			return fmt.Errorf("不允许访问 %s 库：只支持业务库与 information_schema", strings.ToLower(t.text))
		}
	}
	return nil
}

// hasTopLevelLimit reports whether a LIMIT already exists at depth 0.
//
// A LIMIT inside a subquery does not count: `WHERE x IN (SELECT y FROM u LIMIT 5)` still needs an
// outer LIMIT.
func hasTopLevelLimit(tokens []sqlToken) bool {
	for _, t := range tokens {
		if t.depth == 0 && t.kind == tokWord && t.text == "LIMIT" {
			return true
		}
	}
	return false
}

// skipQuoted skips a '...' or "..." literal and returns the index just past it.
//
// Two escape forms are supported: a backslash (MySQL's default behavior) and a doubled quote.
func skipQuoted(s string, start int, quote byte) (int, error) {
	for i := start + 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++ // skip the escaped byte
		case quote:
			if i+1 < len(s) && s[i+1] == quote {
				i++ // two quotes in a row are the escaped quote itself
				continue
			}
			return i + 1, nil
		}
	}
	return 0, fmt.Errorf("SQL 里的引号没有闭合")
}

// skipBacktick skips a backtick identifier and returns the index just past it.
func skipBacktick(s string, start int) (int, error) {
	for i := start + 1; i < len(s); i++ {
		if s[i] != '`' {
			continue
		}
		if i+1 < len(s) && s[i+1] == '`' {
			i++
			continue
		}
		return i + 1, nil
	}
	return 0, fmt.Errorf("SQL 里的反引号没有闭合")
}

// lineEnd returns the index where the line starting at start ends (excluding the newline).
func lineEnd(s string, start int) int {
	if i := strings.IndexByte(s[start:], '\n'); i >= 0 {
		return start + i
	}
	return len(s)
}

// isWordByte reports whether the byte can be part of an unquoted word.
//
// Multi-byte UTF-8 bytes always count as word bytes: they only ever form non-keyword tokens, which
// changes no decision, but it keeps a non-ASCII table name from being split into odd pieces.
func isWordByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '_', c == '$', c >= 0x80:
		return true
	default:
		return false
	}
}

// singleLineSQL flattens the SQL into one line and truncates it, for the audit log.
//
// An audit line must show at a glance what the query did, but one very long SQL must not blow up
// the log.
func singleLineSQL(sql string) string {
	s := strings.Join(strings.Fields(sql), " ")
	if len(s) <= maxSQLAuditBytes {
		return s
	}
	cut := maxSQLAuditBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…（已截断）"
}

// renderSQLResult renders the query result as a markdown table, preceded by one metadata line.
//
// Metadata first: the model and the reader see how many rows, whether the result was truncated and
// how long it took before looking at the table.
func renderSQLResult(columns []string, rows [][]string, truncated bool, limit int, elapsed time.Duration) string {
	var b strings.Builder
	b.WriteString(sqlResultMeta(len(rows), truncated, limit, elapsed))
	b.WriteString("\n\n")
	if len(columns) == 0 {
		b.WriteString("（服务端没有返回列信息）")
		return capResultText(b.String())
	}
	b.WriteString("| ")
	for i, c := range columns {
		if i > 0 {
			b.WriteString(" | ")
		}
		b.WriteString(markdownCell(c))
	}
	b.WriteString(" |\n| ")
	for i := range columns {
		if i > 0 {
			b.WriteString(" | ")
		}
		b.WriteString("---")
	}
	b.WriteString(" |\n")
	for _, row := range rows {
		b.WriteString("| ")
		for i := range columns {
			if i > 0 {
				b.WriteString(" | ")
			}
			val := ""
			if i < len(row) {
				val = row[i]
			}
			b.WriteString(markdownCell(val))
		}
		b.WriteString(" |\n")
	}
	if len(rows) == 0 {
		b.WriteString("\n（查询没有返回任何行）")
	}
	return capResultText(b.String())
}

// sqlResultMeta builds the first line of the result: the row count, a truncation marker when
// needed, and the elapsed time. The text itself is Chinese (user-visible output); see the format
// strings below.
func sqlResultMeta(n int, truncated bool, limit int, elapsed time.Duration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "共 %d 行", n)
	if truncated {
		fmt.Fprintf(&b, "（已截断，上限 %d）", limit)
	}
	fmt.Fprintf(&b, "｜耗时 %dms", elapsed.Milliseconds())
	return b.String()
}

// markdownCell escapes a cell: a pipe breaks the table, a newline breaks the row, and an
// over-long value bloats the result.
func markdownCell(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "\n", "<br>")
	if utf8.RuneCountInString(s) > maxCellRunes {
		runes := []rune(s)
		s = string(runes[:maxCellRunes]) + "…"
	}
	return s
}

// capResultText puts a hard ceiling on the whole SQL result: anything over maxResultBytes is
// truncated with an explanation.
//
// A SQL result has no natural upper bound (the model can write a full-table scan), so a hard cap
// is required. read_object is different: how many bytes it reads is decided by the configured
// max_bytes, and it uses capTextAt directly (see tos.go).
func capResultText(s string) string {
	return capTextAt(s, maxResultBytes, "（结果文本超过 256KB，已截断；请缩小查询范围，或先用聚合 SQL 统计）")
}

// capTextAt cuts text to limit bytes (on a rune boundary, so the result stays valid UTF-8) and
// appends a notice.
func capTextAt(s string, limit int, notice string) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n\n" + notice
}

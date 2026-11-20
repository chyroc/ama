// Tests for the query_sql tool: row truncation, markdown escaping, NULL rendering, max_rows
// clamping, error mapping, timeouts, audit logging.
//
// Everything runs against fake sqlConnector / sqlExecutor / sqlRows, so no MySQL (and no network)
// is involved. The narrow interfaces in mysql.go exist exactly for this.

package datasources

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

// fakeRows is an in-memory cursor implementing sqlRows.
type fakeRows struct {
	columns []string
	rows    [][]any
	next    int
	err     error
	closed  bool
}

func (r *fakeRows) Columns() ([]string, error) { return r.columns, nil }

func (r *fakeRows) Next() bool {
	if r.next >= len(r.rows) {
		return false
	}
	r.next++
	return true
}

func (r *fakeRows) Scan(dest ...any) error {
	row := r.rows[r.next-1]
	for i := range dest {
		var v any
		if i < len(row) {
			v = row[i]
		}
		switch ptr := dest[i].(type) {
		case *any:
			*ptr = v
		case *string:
			if v == nil {
				*ptr = ""
				continue
			}
			*ptr = fmt.Sprint(v)
		case *int64:
			if v == nil {
				*ptr = 0
				continue
			}
			n, ok := v.(int64)
			if !ok {
				return fmt.Errorf("fakeRows.Scan: 想扫进 int64，实际 %T", v)
			}
			*ptr = n
		default:
			return fmt.Errorf("fakeRows.Scan: 不支持的扫描目标 %T", dest[i])
		}
	}
	return nil
}

func (r *fakeRows) Err() error   { return r.err }
func (r *fakeRows) Close() error { r.closed = true; return nil }

// fakeExecutor is one fake read-only session.
type fakeExecutor struct {
	columns   []string
	rows      [][]any
	queryErr  error
	delay     time.Duration
	queries   []string
	closed    bool
	connected bool
}

func (e *fakeExecutor) QueryContext(ctx context.Context, query string) (sqlRows, error) {
	e.queries = append(e.queries, query)
	if e.delay > 0 {
		select {
		case <-time.After(e.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if e.queryErr != nil {
		return nil, e.queryErr
	}
	return &fakeRows{columns: e.columns, rows: e.rows}, nil
}

func (e *fakeExecutor) Close() error { e.closed = true; return nil }

// fakeConnector hands out one fakeExecutor.
type fakeConnector struct {
	exec       *fakeExecutor
	connectErr error
	connects   int
	closed     bool
}

func (c *fakeConnector) Connect(ctx context.Context) (sqlExecutor, error) {
	c.connects++
	if c.connectErr != nil {
		return nil, c.connectErr
	}
	return c.exec, nil
}

func (c *fakeConnector) Close() error { c.closed = true; return nil }

// testMySQLConfig is a complete, enabled config with small limits.
func testMySQLConfig() MySQLConfig {
	return MySQLConfig{
		Enabled:  true,
		Host:     "mysql.example.com",
		Port:     3306,
		Database: "example_db",
		User:     "ro_user",
		Password: "s3cr3t-PASS",
		MaxRows:  200,
		Timeout:  2 * time.Second,
	}
}

// newTestQueryTool builds the tool under test.
func newTestQueryTool(cfg MySQLConfig, conn sqlConnector, logger *slog.Logger) *querySQLTool {
	return &querySQLTool{
		src:       &mysqlSource{cfg: cfg, conn: conn},
		sessionID: "sess-1",
		workID:    "work-1",
		logger:    logger,
	}
}

// runQuery is a tiny helper: marshal the arguments and execute.
func runQuery(t *testing.T, tool *querySQLTool, sql string, maxRows int) (text string, isError bool) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"sql": sql, "max_rows": maxRows})
	if err != nil {
		t.Fatalf("构造入参失败：%v", err)
	}
	result := tool.Execute(context.Background(), body)
	if len(result.Content) == 0 {
		t.Fatalf("结果里没有内容块")
	}
	return result.Content[0].Text, result.IsError
}

// TestQuerySQLHappyPath: rows are rendered as a markdown table with the metadata line, and the
// query gets its LIMIT appended.
func TestQuerySQLHappyPath(t *testing.T) {
	exec := &fakeExecutor{
		columns: []string{"id", "name"},
		rows: [][]any{
			{int64(1), []byte("张三")},
			{int64(2), nil},
		},
	}
	conn := &fakeConnector{exec: exec}
	tool := newTestQueryTool(testMySQLConfig(), conn, nil)

	text, isError := runQuery(t, tool, "SELECT id, name FROM t ORDER BY id", 0)
	if isError {
		t.Fatalf("不该是错误结果：%s", text)
	}
	for _, want := range []string{
		"共 2 行",
		"| id | name |",
		"| 1 | 张三 |",
		"| 2 | NULL |", // NULL must be explicit, not an empty cell
		"LIMIT 201",    // injected at maxRows+1
	} {
		if !strings.Contains(text, want) {
			t.Errorf("结果里应当有 %q，实际：\n%s", want, text)
		}
	}
	if len(exec.queries) != 1 {
		t.Fatalf("应当只查一次，实际 %d 次", len(exec.queries))
	}
	if got := exec.queries[0]; !strings.Contains(got, "\nLIMIT 201") {
		t.Errorf("发给 MySQL 的 SQL 应当被追加 LIMIT，实际 %q", got)
	}
	if !exec.closed {
		t.Error("会话用完应当关闭（归还连接）")
	}
}

// TestQuerySQLTruncatesRows: more rows than the limit → the extra row is read (to detect it),
// counted as truncated, and dropped from the output.
func TestQuerySQLTruncatesRows(t *testing.T) {
	rows := make([][]any, 0, 250)
	for i := 0; i < 250; i++ {
		rows = append(rows, []any{int64(i)})
	}
	exec := &fakeExecutor{columns: []string{"n"}, rows: rows}
	tool := newTestQueryTool(testMySQLConfig(), &fakeConnector{exec: exec}, nil)

	text, isError := runQuery(t, tool, "SELECT n FROM t", 0)
	if isError {
		t.Fatalf("不该是错误结果：%s", text)
	}
	if !strings.Contains(text, "共 200 行（已截断，上限 200）") {
		t.Errorf("截断标记不对：\n%s", text[:200])
	}
	if !strings.Contains(text, "| 199 |") {
		t.Error("第 200 行（下标 199）应当存在")
	}
	if strings.Contains(text, "| 200 |") {
		t.Error("第 201 行（下标 200）应当被截掉")
	}
	if got := exec.queries[0]; !strings.HasSuffix(got, "LIMIT 201") {
		t.Errorf("SQL 应当以 LIMIT 201 结尾，实际 %q", got)
	}
}

// TestQuerySQLMaxRowsParamClamp: max_rows can only lower the configured cap.
func TestQuerySQLMaxRowsParamClamp(t *testing.T) {
	cases := []struct {
		name      string
		requested int
		wantLimit string
		wantRows  int // how many rows the fake returns / what the meta says
	}{
		{name: "zero means default", requested: 0, wantLimit: "LIMIT 201"},
		{name: "negative means default", requested: -1, wantLimit: "LIMIT 201"},
		{name: "huge is clamped to config", requested: 1 << 30, wantLimit: "LIMIT 201"},
		{name: "smaller is honoured", requested: 2, wantLimit: "LIMIT 3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exec := &fakeExecutor{
				columns: []string{"n"},
				rows:    [][]any{{int64(1)}, {int64(2)}, {int64(3)}, {int64(4)}},
			}
			tool := newTestQueryTool(testMySQLConfig(), &fakeConnector{exec: exec}, nil)
			text, isError := runQuery(t, tool, "SELECT n FROM t", tc.requested)
			if isError {
				t.Fatalf("不该是错误结果：%s", text)
			}
			if got := exec.queries[0]; !strings.HasSuffix(got, tc.wantLimit) {
				t.Errorf("limit 期望以 %q 结尾，实际 %q", tc.wantLimit, got)
			}
		})
	}
}

// TestQuerySQLExistingLimitIsNotReinjected: a query with its own LIMIT is executed verbatim, and
// the truncation flag is decided by how many rows come back.
func TestQuerySQLExistingLimitIsNotReinjected(t *testing.T) {
	exec := &fakeExecutor{columns: []string{"n"}, rows: [][]any{{int64(1)}}}
	tool := newTestQueryTool(testMySQLConfig(), &fakeConnector{exec: exec}, nil)
	if _, isError := runQuery(t, tool, "SELECT n FROM t LIMIT 1", 0); isError {
		t.Fatal("不该是错误结果")
	}
	if got := exec.queries[0]; got != "SELECT n FROM t LIMIT 1" {
		t.Errorf("已有 LIMIT 的 SQL 应当原样执行，实际 %q", got)
	}
}

// TestQuerySQLRejectsInvalidSQL: the guard runs before any connection is opened.
func TestQuerySQLRejectsInvalidSQL(t *testing.T) {
	conn := &fakeConnector{exec: &fakeExecutor{}}
	tool := newTestQueryTool(testMySQLConfig(), conn, nil)
	text, isError := runQuery(t, tool, "SELECT 1; DROP TABLE t", 0)
	if !isError {
		t.Fatalf("写语句应当被拒绝，实际：%s", text)
	}
	if !strings.Contains(text, "SQL 被拒绝") {
		t.Errorf("错误文案应当说明被拒绝：%q", text)
	}
	if conn.connects != 0 {
		t.Error("被拒绝的 SQL 不该尝试连数据库")
	}
}

// TestQuerySQLErrorMapping: connection / query / timeout errors become Chinese tool errors and
// never leak the password.
func TestQuerySQLErrorMapping(t *testing.T) {
	t.Run("connect failed", func(t *testing.T) {
		conn := &fakeConnector{connectErr: fmt.Errorf("dial tcp 10.0.0.1:3306: connect: connection refused")}
		tool := newTestQueryTool(testMySQLConfig(), conn, nil)
		text, isError := runQuery(t, tool, "SELECT 1", 0)
		if !isError {
			t.Fatal("连不上应当是错误结果")
		}
		if !strings.Contains(text, "数据源不可用") || !strings.Contains(text, "connection refused") {
			t.Errorf("错误文案不对：%q", text)
		}
	})

	t.Run("query error from server", func(t *testing.T) {
		exec := &fakeExecutor{queryErr: errors.New("Error 1054 (42S22): Unknown column 'nope' in 'field list'")}
		tool := newTestQueryTool(testMySQLConfig(), &fakeConnector{exec: exec}, nil)
		text, isError := runQuery(t, tool, "SELECT nope FROM t", 0)
		if !isError {
			t.Fatal("SQL 语法错误应当是错误结果")
		}
		// MySQL's own message is useful to the model (it can fix the query), so it is echoed.
		if !strings.Contains(text, "查询失败") || !strings.Contains(text, "Unknown column") {
			t.Errorf("错误文案不对：%q", text)
		}
	})

	t.Run("password is redacted", func(t *testing.T) {
		cfg := testMySQLConfig()
		conn := &fakeConnector{connectErr: fmt.Errorf("invalid DSN for user ro_user password %s", cfg.Password)}
		tool := newTestQueryTool(cfg, conn, nil)
		text, _ := runQuery(t, tool, "SELECT 1", 0)
		if strings.Contains(text, cfg.Password) {
			t.Errorf("工具结果里不能出现密码：%q", text)
		}
		if !strings.Contains(text, "***") {
			t.Errorf("密码应当被替换成 ***：%q", text)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		cfg := testMySQLConfig()
		cfg.Timeout = 10 * time.Millisecond
		exec := &fakeExecutor{delay: 500 * time.Millisecond}
		tool := newTestQueryTool(cfg, &fakeConnector{exec: exec}, nil)
		start := time.Now()
		text, isError := runQuery(t, tool, "SELECT SLEEP(10)", 0)
		if !isError {
			t.Fatal("超时应当是错误结果")
		}
		if !strings.Contains(text, "查询超时") {
			t.Errorf("错误文案应当说明超时：%q", text)
		}
		if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
			t.Errorf("超时没生效，实际等了 %v", elapsed)
		}
	})
}

// TestQuerySQLMarkdownEscaping: table-breaking characters inside a cell are escaped.
func TestQuerySQLMarkdownEscaping(t *testing.T) {
	exec := &fakeExecutor{
		columns: []string{"txt"},
		rows:    [][]any{{[]byte("a|b\nc")}},
	}
	tool := newTestQueryTool(testMySQLConfig(), &fakeConnector{exec: exec}, nil)
	text, isError := runQuery(t, tool, "SELECT txt FROM t", 0)
	if isError {
		t.Fatalf("不该是错误结果：%s", text)
	}
	if !strings.Contains(text, `a\|b<br>c`) {
		t.Errorf("竖线/换行没转义：\n%s", text)
	}
}

// TestQuerySQLOutputCapped: the whole tool result is capped, so a wide result cannot blow up the
// model's context.
func TestQuerySQLOutputCapped(t *testing.T) {
	// Each cell is clamped to maxCellRunes characters first, so one wide column is not enough to
	// reach the 256KB total cap; a realistic "SELECT *" over a wide table is.
	const cols = 120
	columns := make([]string, cols)
	for i := range columns {
		columns[i] = fmt.Sprintf("c%d", i)
	}
	cell := []byte(strings.Repeat("值", maxCellRunes))
	rows := make([][]any, 200)
	for i := range rows {
		row := make([]any, cols)
		for j := range row {
			row[j] = cell
		}
		rows[i] = row
	}
	exec := &fakeExecutor{columns: columns, rows: rows}
	tool := newTestQueryTool(testMySQLConfig(), &fakeConnector{exec: exec}, nil)
	text, isError := runQuery(t, tool, "SELECT v FROM t", 0)
	if isError {
		t.Fatalf("不该是错误结果：%s", text)
	}
	if len(text) > maxResultBytes+200 {
		t.Errorf("结果没有被截断：%d 字节", len(text))
	}
	if !strings.Contains(text, "结果文本超过 256KB") {
		t.Errorf("截断后应当注明原因，结尾：%q", text[len(text)-100:])
	}
}

// TestQuerySQLAuditLog: every call writes an audit line carrying session/work id, the row count
// and the (single-line) SQL.
func TestQuerySQLAuditLog(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	exec := &fakeExecutor{columns: []string{"n"}, rows: [][]any{{int64(1)}}}
	tool := newTestQueryTool(testMySQLConfig(), &fakeConnector{exec: exec}, logger)

	if _, isError := runQuery(t, tool, "SELECT n\nFROM t", 0); isError {
		t.Fatal("不该是错误结果")
	}
	line := buf.String()
	for _, want := range []string{
		"datasource query_sql",
		"tool=query_sql",
		"session_id=sess-1",
		"work_id=work-1",
		"rows=1",
		"truncated=false",
		`sql="SELECT n FROM t"`,
		"status=ok",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("审计日志里应当有 %q，实际：\n%s", want, line)
		}
	}

	// Failures are Warn-level, still with the ids (that is what makes "who ran the failing query"
	// answerable).
	buf.Reset()
	exec2 := &fakeExecutor{queryErr: errors.New("Error 1064: bad SQL")}
	tool2 := newTestQueryTool(testMySQLConfig(), &fakeConnector{exec: exec2}, logger)
	_, _ = runQuery(t, tool2, "SELECT bad", 0)
	failLine := buf.String()
	for _, want := range []string{"level=WARN", "status=error", "session_id=sess-1", "work_id=work-1"} {
		if !strings.Contains(failLine, want) {
			t.Errorf("失败审计里应当有 %q，实际：\n%s", want, failLine)
		}
	}
}

// TestQuerySQLInputDecodeError: bad JSON is a tool error, not a panic.
func TestQuerySQLInputDecodeError(t *testing.T) {
	tool := newTestQueryTool(testMySQLConfig(), &fakeConnector{exec: &fakeExecutor{}}, nil)
	result := tool.Execute(context.Background(), json.RawMessage(`{"sql": 1}`))
	if !result.IsError {
		t.Fatal("坏入参应当是错误结果")
	}
	if !strings.Contains(result.Content[0].Text, "参数") {
		t.Errorf("错误文案不对：%q", result.Content[0].Text)
	}
}

// TestQuerySQLNameMatchesDeclaration: the implementation name must be the declared name; if they
// drift, Ark's agent.custom_tool_use can never be matched to an implementation.
func TestQuerySQLNameMatchesDeclaration(t *testing.T) {
	src := &mysqlSource{cfg: testMySQLConfig(), conn: &fakeConnector{exec: &fakeExecutor{}}}
	decl := src.declaration()
	if decl.Name != ToolNameQuerySQL {
		t.Errorf("声明名 %q 与常量 %q 不一致", decl.Name, ToolNameQuerySQL)
	}
	tool := src.tool("s", "w", nil)
	if tool.Name() != decl.Name {
		t.Errorf("实现名 %q 与声明名 %q 不一致", tool.Name(), decl.Name)
	}
	if len(decl.InputSchema) != 2 {
		t.Errorf("入参 schema 应当有 sql / max_rows 两个属性，实际 %d 个", len(decl.InputSchema))
	}
	if len(decl.Required) != 1 || decl.Required[0] != "sql" {
		t.Errorf("必填参数应当只有 sql，实际 %v", decl.Required)
	}
}

// TestMySQLConfigDefaults: the documented defaults and hard caps.
func TestMySQLConfigDefaults(t *testing.T) {
	if got := (MySQLConfig{}).maxRows(); got != defaultMySQLMaxRows {
		t.Errorf("默认 maxRows 期望 %d，实际 %d", defaultMySQLMaxRows, got)
	}
	if got := (MySQLConfig{MaxRows: hardMySQLMaxRows + 1}).maxRows(); got != hardMySQLMaxRows {
		t.Errorf("maxRows 超过硬上限应当夹到 %d，实际 %d", hardMySQLMaxRows, got)
	}
	if got := (MySQLConfig{}).timeout(); got != defaultMySQLTimeout {
		t.Errorf("默认 timeout 期望 %v，实际 %v", defaultMySQLTimeout, got)
	}
	if got := (MySQLConfig{}).port(); got != 3306 {
		t.Errorf("默认端口期望 3306，实际 %d", got)
	}
	if got := (MySQLConfig{Port: 13306}).port(); got != 13306 {
		t.Errorf("配置端口应当生效，实际 %d", got)
	}
}

// TestMySQLConfigValidate: enabled but incomplete must be an error (fail fast, not "silently one
// tool fewer").
func TestMySQLConfigValidate(t *testing.T) {
	valid := testMySQLConfig()
	if err := valid.validate(); err != nil {
		t.Fatalf("完整配置不该报错：%v", err)
	}
	if err := (MySQLConfig{}).validate(); err != nil {
		t.Errorf("未启用时不该校验字段：%v", err)
	}
	cases := []struct {
		name   string
		mutate func(*MySQLConfig)
		want   string
	}{
		{"missing host", func(c *MySQLConfig) { c.Host = "" }, "host"},
		{"missing database", func(c *MySQLConfig) { c.Database = "" }, "database"},
		{"missing user", func(c *MySQLConfig) { c.User = "" }, "user"},
		{"missing password", func(c *MySQLConfig) { c.Password = "" }, "password"},
		{"bad port", func(c *MySQLConfig) { c.Port = 70000 }, "port"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid
			tc.mutate(&cfg)
			err := cfg.validate()
			if err == nil {
				t.Fatalf("应当报错")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息里应当提到 %q，实际：%v", tc.want, err)
			}
		})
	}
}

// TestMySQLDSN: the DSN is built by the driver's own Config (hand-rolled strings break on
// passwords containing '@' / ':' / '/'), and it must round-trip.
//
// The DSN itself is a secret: it carries the password in cleartext, so it is only ever passed to
// sql.Open and never logged (redactSecret is what error paths use).
func TestMySQLDSN(t *testing.T) {
	cfg := testMySQLConfig()
	cfg.Password = "p@ss:word/x"
	dsn, err := mysqlDSN(cfg)
	if err != nil {
		t.Fatalf("拼 DSN 失败：%v", err)
	}
	back, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("DSN 解析不回来（说明密码里有字符没处理好）：%v", err)
	}
	if back.User != cfg.User || back.Passwd != cfg.Password || back.DBName != cfg.Database {
		t.Errorf("DSN 往返不一致：user=%q pass=%q db=%q", back.User, back.Passwd, back.DBName)
	}
	if back.Addr != "mysql.example.com:3306" {
		t.Errorf("地址不对：%q", back.Addr)
	}
	if back.ParseTime {
		t.Error("parseTime 必须为 false（时间按原始字节返回，避免时区打架）")
	}
	if back.ReadTimeout != 2*time.Second || back.Timeout != mysqlDialTimeout {
		t.Errorf("超时不对：read=%v dial=%v", back.ReadTimeout, back.Timeout)
	}
	// Anything that leaves this package (logs, tool results) must go through redactSecret.
	if strings.Contains(redactSecret(dsn, cfg.Password), cfg.Password) {
		t.Error("redactSecret 没把 DSN 里的密码抹掉")
	}
}

// TestProbeTables: the startup probe reads SHOW TABLES and caps the list.
func TestProbeTables(t *testing.T) {
	rows := make([][]any, 0, maxProbeTables+10)
	for i := 0; i < maxProbeTables+10; i++ {
		rows = append(rows, []any{fmt.Sprintf("t_%03d", i)})
	}
	exec := &fakeExecutor{columns: []string{"Tables_in_db"}, rows: rows}
	src := &mysqlSource{cfg: testMySQLConfig(), conn: &fakeConnector{exec: exec}}
	tables, err := src.probeTables(context.Background())
	if err != nil {
		t.Fatalf("探测失败：%v", err)
	}
	if len(tables) != maxProbeTables {
		t.Errorf("表清单应当截到 %d 个，实际 %d 个", maxProbeTables, len(tables))
	}
	if len(exec.queries) != 1 || exec.queries[0] != "SHOW TABLES" {
		t.Errorf("探测应当执行 SHOW TABLES，实际 %v", exec.queries)
	}
	if !exec.closed {
		t.Error("探测用完应当关闭会话")
	}
}

// TestProbeTablesErrorIsNotFatal: a broken probe is a warn, and the declaration degrades to the
// generic wording instead of failing startup.
func TestProbeTablesErrorIsNotFatal(t *testing.T) {
	conn := &fakeConnector{connectErr: errors.New("dial tcp: i/o timeout")}
	src := &mysqlSource{cfg: testMySQLConfig(), conn: conn}
	if _, err := src.probeTables(context.Background()); err == nil {
		t.Fatal("探测应当返回错误（由调用方 Warn）")
	}
	// A source without tables still produces a usable declaration.
	decl := src.declaration()
	if !strings.Contains(decl.Description, "SHOW TABLES") {
		t.Errorf("没探测到表时说明里应当提示先 SHOW TABLES：%s", decl.Description)
	}
	if !strings.Contains(decl.Description, "只读") {
		t.Errorf("说明里应当写明只读约束：%s", decl.Description)
	}
}

// TestProbeTableColumnsAndUnknownColumnHint verifies that probed column names appear in both the
// tool declaration and Error 1054 (Unknown column) recovery hints.
func TestProbeTableColumnsAndUnknownColumnHint(t *testing.T) {
	colExec := &fakeExecutor{
		columns: []string{"TABLE_NAME", "COLUMN_NAME"},
		rows: [][]any{
			{"products", "product_id"},
			{"products", "sku_name"},
			{"products", "list_price"},
			{"sku_costs", "product_id"},
			{"sku_costs", "unit_total_cost"},
		},
	}
	src := &mysqlSource{
		cfg:    testMySQLConfig(),
		conn:   &fakeConnector{exec: colExec},
		tables: []string{"products", "sku_costs"},
	}
	cols, err := src.probeTableColumns(context.Background())
	if err != nil {
		t.Fatalf("探测列名失败：%v", err)
	}
	src.columns = cols

	decl := src.declaration()
	for _, want := range []string{
		"- products(product_id, sku_name, list_price)",
		"- sku_costs(product_id, unit_total_cost)",
	} {
		if !strings.Contains(decl.Description, want) {
			t.Errorf("工具说明里应当包含 %q，实际：\n%s", want, decl.Description)
		}
	}

	errExec := &fakeExecutor{
		queryErr: errors.New("Error 1054 (42S22): Unknown column 'p.product_name' in 'field list'"),
	}
	src.conn = &fakeConnector{exec: errExec}
	tool := src.tool("sess-1", "work-1", nil).(*querySQLTool)
	text, isError := runQuery(t, tool, "SELECT p.product_name FROM products p", 0)
	if !isError {
		t.Fatal("1054 应当返回错误结果")
	}
	if !strings.Contains(text, "sku_name") || !strings.Contains(text, "当前库各表真实列名为") {
		t.Errorf("1054 错误结果应当附带真实列名提示，实际：\n%s", text)
	}
}

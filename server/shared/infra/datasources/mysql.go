// MySQL datasource: the query_sql tool.
//
// # One dedicated connection + one read-only session per call
//
// One tool call = `db.Conn(ctx)` to take a dedicated connection → `SET SESSION
// transaction_read_only = 1` → run the one SQL statement → return the connection. A dedicated
// connection instead of `db.Query` because the session-level read-only switch has to be on the
// same connection as the query; with a pooled query, "set read-only" and "run the query" can land
// on different connections, which means it was never set.
//
// # Why a read-only account is still required
//
// `transaction_read_only=1` is defense in depth, not access control: how much it constrains DDL
// depends on the server version, and it cannot change "which databases may be read". The real
// boundary is an RDS account that only has SELECT.
package datasources

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/volcengine/ark-runtime-go/arkruntime/toolset"

	"github.com/chyroc/ama/server/shared/infra/ma"
)

const (
	// Tool name. The declaration (Declarations) and the implementation (a ToolsFor map key)
	// share this constant, so they cannot drift apart.
	ToolNameQuerySQL = "query_sql"

	defaultMySQLMaxRows = 200
	hardMySQLMaxRows    = 1000

	defaultMySQLTimeout = 30 * time.Second
	// mysqlDialTimeout is the connect timeout; the total query timeout is MySQLConfig.Timeout.
	mysqlDialTimeout = 5 * time.Second

	// probeTimeout caps the startup probe (SHOW TABLES). The probe only exists so the tool
	// description can list table names; a failure only logs a Warn and must never hold up startup.
	probeTimeout = 5 * time.Second
	// maxProbeTables caps how many table names go into the tool description, so a few hundred
	// tables cannot bloat it.
	maxProbeTables = 100
	// maxProbeColumns caps the total number of (table, column) entries scanned during startup probe.
	maxProbeColumns = 2000
	// maxSchemaDescBytes caps the rendered table(col1, col2, ...) block inside the tool description
	// and 1054 error hints.
	maxSchemaDescBytes = 6144
)

// MySQLConfig is one MySQL datasource the worker-side tool can query.
//
// With Enabled=false the other fields may be empty; with Enabled=true host / database / user /
// password are required, and a missing one is a config error (startup fails, see config.DataSources).
type MySQLConfig struct {
	Enabled  bool
	Host     string
	Port     int
	Database string
	User     string
	Password string
	// MaxRows is the configured per-call row cap; 0 means the default 200, values above the hard
	// cap 1000 are clamped to 1000. The tool's max_rows parameter can only lower it, never raise it.
	MaxRows int
	// Timeout is the total per-call timeout; 0 means the default 30s.
	Timeout time.Duration
}

// validate checks the required fields when the source is enabled.
func (c MySQLConfig) validate() error {
	if !c.Enabled {
		return nil
	}
	var missing []string
	if strings.TrimSpace(c.Host) == "" {
		missing = append(missing, "host")
	}
	if strings.TrimSpace(c.Database) == "" {
		missing = append(missing, "database")
	}
	if strings.TrimSpace(c.User) == "" {
		missing = append(missing, "user")
	}
	if c.Password == "" {
		missing = append(missing, "password")
	}
	if len(missing) > 0 {
		return fmt.Errorf("mysql.enabled=true 但缺少 %s", strings.Join(missing, " / "))
	}
	if c.Port < 0 || c.Port > 65535 {
		return fmt.Errorf("mysql.port %d 不在 0~65535 范围内", c.Port)
	}
	return nil
}

// port returns the effective port.
func (c MySQLConfig) port() int {
	if c.Port > 0 {
		return c.Port
	}
	return 3306
}

// maxRows returns the effective row cap (including the hard-cap clamp).
func (c MySQLConfig) maxRows() int {
	n := c.MaxRows
	if n <= 0 {
		n = defaultMySQLMaxRows
	}
	if n > hardMySQLMaxRows {
		n = hardMySQLMaxRows
	}
	return n
}

// timeout returns the effective total query timeout.
func (c MySQLConfig) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return defaultMySQLTimeout
}

// maxRowsFor clamps the tool's max_rows parameter into [1, configured cap].
func (c MySQLConfig) maxRowsFor(requested int) int {
	limit := c.maxRows()
	if requested <= 0 {
		return limit
	}
	if requested > limit {
		return limit
	}
	return requested
}

// sqlExecutor is the minimum database/sql surface query_sql needs: one read-only session that can
// run one query.
//
// It is an interface instead of *sql.Conn so tests can inject a fake: everything query_sql does
// (row truncation, escaping, error mapping, timeouts) is testable offline, without a real MySQL.
type sqlExecutor interface {
	QueryContext(ctx context.Context, query string) (sqlRows, error)
	Close() error
}

// sqlConnector opens one sqlExecutor. The production implementation is a *sql.DB plus
// "SET SESSION transaction_read_only=1".
type sqlConnector interface {
	Connect(ctx context.Context) (sqlExecutor, error)
	Close() error
}

// sqlRows is the minimum row-cursor surface.
type sqlRows interface {
	Columns() ([]string, error)
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close() error
}

// mysqlSource is one assembled MySQL datasource: both the declaration and the execution come from
// here, so the two cannot drift apart.
type mysqlSource struct {
	cfg      MySQLConfig
	conn     sqlConnector
	tables   []string
	columns  map[string][]string
	redactor *strings.Replacer
}

// declaration builds the Ark-side tool declaration.
//
// The description includes the table names probed at startup: the model has to know what tables
// exist before it can write the right SQL. If the probe failed, the description falls back to
// generic wording (telling the model to run SHOW TABLES first) instead of reporting an error.
func (s *mysqlSource) declaration() ma.CustomToolSpec {
	var b strings.Builder
	b.WriteString("在只读 MySQL 数据源上执行一条 SQL，并返回结果表（markdown）。\n\n")
	b.WriteString("约束：\n")
	b.WriteString("- 只允许单条只读语句：SELECT / WITH … SELECT / SHOW / DESCRIBE / EXPLAIN\n")
	b.WriteString("- 禁止写入、DDL、SELECT … INTO OUTFILE / DUMPFILE、LOAD DATA、文件读取、加锁读\n")
	b.WriteString("- 禁止访问 mysql / performance_schema / sys 库；information_schema 可以读\n")
	fmt.Fprintf(&b, "- 单次最多返回 %d 行（可用 max_rows 调低，不能调高），超出会截断并标注\n", s.cfg.maxRows())
	b.WriteString("- 不要在 SQL 里写很大的 LIMIT：工具不会重复注入，但结果集仍会被整个读完（很慢）；限制行数用 max_rows\n")
	b.WriteString("- 结果表只用于查看，会话无法把结果保存成文件；需要明细时先用聚合 SQL 缩小范围\n")
	if len(s.tables) > 0 {
		fmt.Fprintf(&b, "\n可用数据源：%s（启动时探测到 %d 张表）\n", s.cfg.Database, len(s.tables))
		fmt.Fprintf(&b, "表名：%s\n", strings.Join(s.tables, ", "))
		if schemaText := s.renderTableColumns(maxSchemaDescBytes); schemaText != "" {
			b.WriteString("各表真实列名（写 SQL 时必须严格使用下列列名，严禁自行猜测列名）：\n")
			b.WriteString(schemaText)
		}
	} else {
		fmt.Fprintf(&b, "\n数据源：%s（启动时没探测到表清单，可以先执行 SHOW TABLES 看有什么表）\n", s.cfg.Database)
	}
	return ma.CustomToolSpec{
		Name:        ToolNameQuerySQL,
		Description: b.String(),
		InputSchema: map[string]any{
			"sql": map[string]any{
				"type":        "string",
				"description": "一条只读 SQL。只能单条语句；不需要自己写 LIMIT，工具会按上限截断。",
			},
			"max_rows": map[string]any{
				"type":        "integer",
				"description": fmt.Sprintf("本次最多返回多少行，默认 %d，只能调低不能调高。", s.cfg.maxRows()),
			},
		},
		Required: []string{"sql"},
	}
}

// tool creates the tool instance for one work item. sessionID / workID are captured so audit
// lines can name the session they belong to.
func (s *mysqlSource) tool(sessionID, workID string, logger *slog.Logger) toolset.Tool {
	return &querySQLTool{src: s, sessionID: sessionID, workID: workID, logger: logger}
}

// querySQLTool implements query_sql.
type querySQLTool struct {
	src       *mysqlSource
	sessionID string
	workID    string
	logger    *slog.Logger
}

// Name implements toolset.Tool; it shares ToolNameQuerySQL with the declaration.
func (t *querySQLTool) Name() string { return ToolNameQuerySQL }

// querySQLInput is the tool input.
type querySQLInput struct {
	SQL     string `json:"sql"`
	MaxRows int    `json:"max_rows"`
}

// Execute implements toolset.Tool.
func (t *querySQLTool) Execute(ctx context.Context, input json.RawMessage) toolset.Result {
	var args querySQLInput
	if len(input) > 0 {
		if err := json.Unmarshal(input, &args); err != nil {
			return toolset.ErrorResult("参数不是合法的 JSON：" + err.Error())
		}
	}
	maxRows := t.src.cfg.maxRowsFor(args.MaxRows)
	auditSQL := singleLineSQL(args.SQL)

	guarded, injected, err := guardSQL(args.SQL, maxRows)
	if err != nil {
		t.audit(auditSQL, 0, false, 0, err)
		return toolset.ErrorResult("SQL 被拒绝：" + err.Error() +
			"。只支持单条只读查询（SELECT / WITH … SELECT / SHOW / DESCRIBE / EXPLAIN）。")
	}

	timeout := t.src.cfg.timeout()
	queryCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	session, err := t.src.conn.Connect(queryCtx)
	if err != nil {
		t.audit(auditSQL, 0, false, time.Since(start), err)
		return toolset.ErrorResult("数据源不可用：" + t.redact(err))
	}
	defer func() { _ = session.Close() }()

	rows, err := session.QueryContext(queryCtx, guarded)
	if err != nil {
		elapsed := time.Since(start)
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(queryCtx.Err(), context.DeadlineExceeded) {
			t.audit(auditSQL, 0, false, elapsed, err)
			return toolset.ErrorResult(fmt.Sprintf("查询超时（上限 %s）：请缩小范围或改用聚合 SQL。", timeout))
		}
		if errors.Is(err, context.Canceled) {
			t.audit(auditSQL, 0, false, elapsed, err)
			return toolset.ErrorResult("查询已取消（work 已结束或会话被终止）。")
		}
		t.audit(auditSQL, 0, false, elapsed, err)
		msg := "查询失败：" + t.redact(err)
		if hint := t.src.unknownColumnHint(err); hint != "" {
			msg += "\n" + hint
		}
		return toolset.ErrorResult(msg)
	}
	defer func() { _ = rows.Close() }()

	columns, err := rows.Columns()
	if err != nil {
		colsErr := fmt.Errorf("读取结果列失败: %w", err)
		t.audit(auditSQL, 0, false, time.Since(start), colsErr)
		return toolset.ErrorResult("查询失败：" + t.redact(colsErr))
	}

	// Read one extra row: receiving maxRows+1 rows means "there is more", so truncated can be
	// reported honestly.
	collected := make([][]string, 0, maxRows+1)
	for rows.Next() {
		if len(collected) > maxRows {
			break
		}
		vals := make([]any, len(columns))
		for i := range vals {
			vals[i] = new(any)
		}
		if err := rows.Scan(vals...); err != nil {
			t.audit(auditSQL, len(collected), false, time.Since(start), err)
			return toolset.ErrorResult("读取结果失败：" + t.redact(err))
		}
		row := make([]string, len(columns))
		for i, v := range vals {
			row[i] = sqlValueString(*(v.(*any)), t.src.redactor)
		}
		collected = append(collected, row)
	}
	if err := rows.Err(); err != nil {
		elapsed := time.Since(start)
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(queryCtx.Err(), context.DeadlineExceeded) {
			t.audit(auditSQL, len(collected), false, elapsed, err)
			return toolset.ErrorResult(fmt.Sprintf("查询超时（上限 %s）：请缩小范围或改用聚合 SQL。", timeout))
		}
		t.audit(auditSQL, len(collected), false, elapsed, err)
		return toolset.ErrorResult("读取结果失败：" + t.redact(err))
	}

	truncated := len(collected) > maxRows
	if truncated {
		collected = collected[:maxRows]
	}
	elapsed := time.Since(start)
	t.audit(auditSQL, len(collected), truncated, elapsed, nil)

	text := renderSQLResult(columns, collected, truncated, maxRows, elapsed)
	if injected {
		// The metadata line already shows the row cap; this extra sentence says the tool added a
		// LIMIT itself, so the model knows it does not have to.
		text += fmt.Sprintf("\n（工具已自动追加 LIMIT %d）", maxRows+1)
	}
	return toolset.TextResult(text)
}

// audit writes the audit line. Info on success, Warn on failure, with fixed fields: tool name,
// session, work, duration, row count, truncated flag, SQL (single line, truncated). **This is the
// only "who queried what" record these tools have**, so session / work id must be there.
func (t *querySQLTool) audit(
	sql string, rows int, truncated bool, elapsed time.Duration, err error,
) {
	logger := t.logger
	if logger == nil {
		logger = slog.Default()
	}
	attrs := []any{
		slog.String("tool", ToolNameQuerySQL),
		slog.String("session_id", t.sessionID),
		slog.String("work_id", t.workID),
		slog.Int64("duration_ms", elapsed.Milliseconds()),
		slog.Int("rows", rows),
		slog.Bool("truncated", truncated),
		slog.String("sql", sql),
	}
	if err != nil {
		attrs = append(attrs, slog.String("status", "error"), slog.Any("err", err))
		logger.Warn("datasource query_sql", attrs...)
		return
	}
	attrs = append(attrs, slog.String("status", "ok"))
	logger.Info("datasource query_sql", attrs...)
}

// redact removes any password that may appear in an error. Driver errors usually do not contain
// the DSN, but sql.Open's parse errors do; echoing them to the model would write the password into
// the conversation.
func (t *querySQLTool) redact(err error) string {
	return redactSecret(err.Error(), t.src.cfg.Password)
}

// sqlValueString renders a scanned value as text. NULL is rendered as the literal NULL: empty
// string and NULL mean different things in business data, and a model that sees "" reads it as
// "the value is empty" while NULL tells it the value is missing.
func sqlValueString(v any, redactor *strings.Replacer) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return applyRedactor(redactor, string(x))
	case string:
		return applyRedactor(redactor, x)
	case time.Time:
		return x.UTC().Format("2006-01-02T15:04:05.000Z")
	default:
		return applyRedactor(redactor, fmt.Sprint(x))
	}
}

// redactSecret replaces any occurrence of a secret in the text with ***.
func redactSecret(text, secret string) string {
	if secret == "" || !strings.Contains(text, secret) {
		return text
	}
	return strings.ReplaceAll(text, secret, "***")
}

// openMySQLConnector builds the connection pool. **No network IO here**: sql.Open only parses the
// DSN and builds the pool; connectivity is the startup probe's (SHOW TABLES) and each tool call's
// job, and a failed probe only logs a Warn without blocking amad startup.
func openMySQLConnector(cfg MySQLConfig) (sqlConnector, error) {
	dsn, err := mysqlDSN(cfg)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 MySQL 连接池失败: %w", err)
	}
	// A tool call takes one connection, and the concurrency ceiling comes from the worker; a
	// conservative limit keeps a stuck work item from eating all of the RDS connections.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)
	return &mysqlConnector{db: db}, nil
}

// mysqlDSN builds the DSN with the driver's own Config: it knows how to handle special characters
// in the password, so do not hand-roll the string. **Callers must never print the return value**.
func mysqlDSN(cfg MySQLConfig) (string, error) {
	c := mysql.NewConfig()
	c.Net = "tcp"
	c.Addr = net.JoinHostPort(strings.TrimSpace(cfg.Host), strconv.Itoa(cfg.port()))
	c.DBName = strings.TrimSpace(cfg.Database)
	c.User = strings.TrimSpace(cfg.User)
	c.Passwd = cfg.Password
	c.Timeout = mysqlDialTimeout
	c.ReadTimeout = cfg.timeout()
	c.ParseTime = false
	// loc only affects time parsing; with ParseTime=false every time value comes back as raw
	// bytes, so the server time zone and the process time zone cannot fight each other.
	c.Loc = time.UTC
	dsn := c.FormatDSN()
	if dsn == "" {
		return "", errors.New("拼 MySQL DSN 失败")
	}
	return dsn, nil
}

// mysqlConnector is the real implementation: a connection pool plus one dedicated read-only
// session per call.
type mysqlConnector struct {
	db *sql.DB
}

// Connect takes a dedicated connection and turns it into a read-only session.
func (c *mysqlConnector) Connect(ctx context.Context) (sqlExecutor, error) {
	conn, err := c.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	// Defense in depth: even if the RDS account is mis-provisioned as writable, this session
	// cannot write.
	// ⚠️ It must be on **the same connection** as the query, hence conn.ExecContext instead of
	// db.ExecContext.
	// ⚠️ transaction_read_only is the MySQL 5.7.20+ / 8.0 spelling; older 5.6 uses tx_read_only.
	// On such a server this step fails with an explicit "failed to set a read-only session"
	// error instead of silently doing nothing.
	if _, err := conn.ExecContext(ctx, "SET SESSION transaction_read_only = 1"); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("设置只读会话失败: %w", err)
	}
	return &mysqlSession{conn: conn}, nil
}

// Close closes the connection pool.
func (c *mysqlConnector) Close() error { return c.db.Close() }

// mysqlSession is one dedicated read-only connection.
type mysqlSession struct {
	conn *sql.Conn
}

// QueryContext runs one query.
func (s *mysqlSession) QueryContext(ctx context.Context, query string) (sqlRows, error) {
	rows, err := s.conn.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	return &mysqlRows{rows: rows}, nil
}

// Close returns the connection to the pool.
func (s *mysqlSession) Close() error { return s.conn.Close() }

// mysqlRows adapts *sql.Rows to sqlRows.
type mysqlRows struct {
	rows *sql.Rows
}

func (r *mysqlRows) Columns() ([]string, error) { return r.rows.Columns() }
func (r *mysqlRows) Next() bool                 { return r.rows.Next() }
func (r *mysqlRows) Scan(dest ...any) error     { return r.rows.Scan(dest...) }
func (r *mysqlRows) Err() error                 { return r.rows.Err() }
func (r *mysqlRows) Close() error               { return r.rows.Close() }

// probeTables lists table names with SHOW TABLES for the tool description. A failure is returned
// as an error; the caller only logs a Warn.
func (s *mysqlSource) probeTables(ctx context.Context) ([]string, error) {
	exec, err := s.conn.Connect(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = exec.Close() }()
	rows, err := exec.QueryContext(ctx, "SHOW TABLES")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var tables []string
	for rows.Next() {
		if len(tables) >= maxProbeTables {
			break
		}
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tables, nil
}

// probeTableColumns reads (table_name, column_name) pairs from information_schema.columns for the
// current database so the tool description and Error 1054 hints can list exact column names.
func (s *mysqlSource) probeTableColumns(ctx context.Context) (map[string][]string, error) {
	exec, err := s.conn.Connect(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = exec.Close() }()
	const q = "SELECT TABLE_NAME, COLUMN_NAME FROM information_schema.columns WHERE TABLE_SCHEMA = DATABASE() ORDER BY TABLE_NAME, ORDINAL_POSITION"
	rows, err := exec.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil || len(cols) < 2 {
		return nil, err
	}
	out := make(map[string][]string)
	count := 0
	for rows.Next() {
		if count >= maxProbeColumns {
			break
		}
		var tbl, col string
		if err := rows.Scan(&tbl, &col); err != nil {
			return nil, err
		}
		if tbl == "" || col == "" {
			continue
		}
		out[tbl] = append(out[tbl], col)
		count++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// renderTableColumns formats table(col1, col2, ...) lines in the order of s.tables, capped at maxBytes.
func (s *mysqlSource) renderTableColumns(maxBytes int) string {
	if len(s.tables) == 0 || len(s.columns) == 0 {
		return ""
	}
	var b strings.Builder
	for _, tbl := range s.tables {
		cols := s.columns[tbl]
		if len(cols) == 0 {
			continue
		}
		line := fmt.Sprintf("- %s(%s)\n", tbl, strings.Join(cols, ", "))
		if maxBytes > 0 && b.Len()+len(line) > maxBytes {
			b.WriteString("- ...（其余表列已省略，可执行 DESCRIBE <表名> 查看）\n")
			break
		}
		b.WriteString(line)
	}
	return b.String()
}

// unknownColumnHint appends the probed table column list when a query fails with MySQL Error 1054
// (Unknown column), allowing the model to immediately rewrite the SQL with valid column names.
func (s *mysqlSource) unknownColumnHint(err error) string {
	if err == nil || len(s.columns) == 0 {
		return ""
	}
	lower := strings.ToLower(err.Error())
	if !strings.Contains(lower, "unknown column") && !strings.Contains(lower, "1054") {
		return ""
	}
	schemaText := s.renderTableColumns(maxSchemaDescBytes)
	if schemaText == "" {
		return ""
	}
	return "提示：请勿猜测列名，当前库各表真实列名为：\n" + strings.TrimSuffix(schemaText, "\n")
}

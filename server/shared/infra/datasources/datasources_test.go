// Tests for the Registry: config validation, "only enabled sources", startup probes being
// warn-only, Close, and — most importantly — that the declarations and the implementations can
// never drift apart.

package datasources

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// testLogger returns a logger writing into buf, so tests can assert on Warn lines.
func testLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// fakeDeps wires both sources to in-memory fakes. The factories record whether they were called.
type fakeDeps struct {
	mysqlCalled bool
	tosCalled   bool
	mysqlProbe  [][]any // rows returned by the SHOW TABLES probe
	tosObjects  map[string][]byte
	mysqlErr    error // construction error
	tosErr      error
	mysqlConn   *fakeConnector
	tosStore    *fakeObjectStore
}

func (d *fakeDeps) deps() sourceDeps {
	return sourceDeps{
		newMySQL: func(cfg MySQLConfig) (sqlConnector, error) {
			d.mysqlCalled = true
			if d.mysqlErr != nil {
				return nil, d.mysqlErr
			}
			rows := d.mysqlProbe
			if rows == nil {
				rows = [][]any{{"orders"}, {"users"}}
			}
			d.mysqlConn = &fakeConnector{exec: &fakeExecutor{
				columns: []string{"Tables_in_db"},
				rows:    rows,
			}}
			return d.mysqlConn, nil
		},
		newTOS: func(cfg TOSConfig) (objectStore, error) {
			d.tosCalled = true
			if d.tosErr != nil {
				return nil, d.tosErr
			}
			objects := d.tosObjects
			if objects == nil {
				objects = map[string][]byte{"example-prefix/reviews.csv": []byte("a,b\n")}
			}
			d.tosStore = &fakeObjectStore{objects: objects}
			return d.tosStore, nil
		},
	}
}

func fullConfig() Config {
	return Config{MySQL: testMySQLConfig(), TOS: testTOSConfig()}
}

// TestNewWithoutSourcesReturnsNil: "no datasource configured" must not produce a registry at all,
// so core.Config.CustomTools stays a bare nil interface.
func TestNewWithoutSourcesReturnsNil(t *testing.T) {
	reg, err := New(Config{}, nil)
	if err != nil {
		t.Fatalf("未配置时不该报错：%v", err)
	}
	if reg != nil {
		t.Fatalf("未配置时应当返回 nil Registry，实际 %#v", reg)
	}
}

// TestNewRejectsEnabledButIncomplete: fail fast instead of silently dropping a tool.
func TestNewRejectsEnabledButIncomplete(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"mysql missing user", Config{MySQL: func() MySQLConfig {
			c := testMySQLConfig()
			c.User = ""
			return c
		}()}, "user"},
		{"tos missing prefix", Config{TOS: func() TOSConfig {
			c := testTOSConfig()
			c.Prefix = ""
			return c
		}()}, "prefix"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg, err := New(tc.cfg, nil)
			if err == nil {
				t.Fatalf("应当返回配置错误，实际 reg=%#v", reg)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误里应当提到 %q，实际：%v", tc.want, err)
			}
		})
	}
}

// TestRegistryDeclarationsMatchTools is the anti-drift assertion: every declared name has an
// implementation with the same name, and nothing is implemented that was not declared.
func TestRegistryDeclarationsMatchTools(t *testing.T) {
	logger, _ := testLogger()
	fd := &fakeDeps{}
	reg, err := newRegistry(fullConfig(), logger, fd.deps())
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	defer func() { _ = reg.Close() }()

	if !fd.mysqlCalled || !fd.tosCalled {
		t.Fatalf("两个来源都启用了，工厂应当都被调用：mysql=%v tos=%v", fd.mysqlCalled, fd.tosCalled)
	}

	decls := reg.Declarations()
	if len(decls) != 2 {
		t.Fatalf("应当声明 2 个工具，实际 %d 个", len(decls))
	}
	declared := map[string]bool{}
	for _, d := range decls {
		declared[d.Name] = true
		if d.Description == "" {
			t.Errorf("工具 %q 的 Description 不能为空（模型靠它决定何时调用）", d.Name)
		}
		if len(d.InputSchema) == 0 {
			t.Errorf("工具 %q 的 InputSchema 不能为空", d.Name)
		}
	}

	tools := reg.ToolsFor("sess-1", "work-1", logger)
	if len(tools) != len(decls) {
		t.Fatalf("实现数量 %d 与声明数量 %d 不一致（声明了却没有实现 = work 卡在 requires_action）",
			len(tools), len(decls))
	}
	for name, tool := range tools {
		if !declared[name] {
			t.Errorf("实现了 %q 但没声明（模型永远不会调，且零报错）", name)
		}
		if tool.Name() != name {
			t.Errorf("map key %q 与工具自己的 Name() %q 不一致", name, tool.Name())
		}
	}
	// The probe results must make it into the descriptions: the model has to know what tables /
	// keys exist before it can write a useful call.
	if !strings.Contains(reg.Declarations()[0].Description, "orders") {
		t.Errorf("MySQL 说明里应当有探测到的表名：%s", reg.Declarations()[0].Description)
	}
	var tosDesc string
	for _, d := range reg.Declarations() {
		if d.Name == ToolNameReadObject {
			tosDesc = d.Description
		}
	}
	if !strings.Contains(tosDesc, "example-prefix/reviews.csv") {
		t.Errorf("TOS 说明里应当有探测到的 key：%s", tosDesc)
	}
}

// TestRegistryOnlyEnabledSources: a disabled source is not built, declared or exposed.
func TestRegistryOnlyEnabledSources(t *testing.T) {
	logger, _ := testLogger()
	fd := &fakeDeps{}
	reg, err := newRegistry(Config{MySQL: testMySQLConfig()}, logger, fd.deps())
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	defer func() { _ = reg.Close() }()

	if fd.tosCalled {
		t.Error("TOS 没启用，不该被构造（会白连一次网络）")
	}
	decls := reg.Declarations()
	if len(decls) != 1 || decls[0].Name != ToolNameQuerySQL {
		t.Fatalf("应当只声明 query_sql，实际 %+v", decls)
	}
	tools := reg.ToolsFor("s", "w", logger)
	if len(tools) != 1 {
		t.Fatalf("应当只实现 query_sql，实际 %d 个", len(tools))
	}
	if _, ok := tools[ToolNameReadObject]; ok {
		t.Error("没启用的来源不该出现在实现里")
	}
}

// TestRegistryProbeFailureIsWarnOnly: an unreachable datasource must not stop amad; the tool
// stays available and its description falls back to generic wording.
func TestRegistryProbeFailureIsWarnOnly(t *testing.T) {
	logger, buf := testLogger()
	fd := &fakeDeps{
		mysqlProbe: nil,
	}
	deps := fd.deps()
	// Make both probes fail at connect time.
	deps.newMySQL = func(cfg MySQLConfig) (sqlConnector, error) {
		return &fakeConnector{connectErr: errors.New("dial tcp 10.0.0.1:3306: i/o timeout")}, nil
	}
	deps.newTOS = func(cfg TOSConfig) (objectStore, error) {
		return &fakeObjectStore{listErr: errors.New("403 Forbidden")}, nil
	}
	reg, err := newRegistry(fullConfig(), logger, deps)
	if err != nil {
		t.Fatalf("探测失败不该阻塞启动：%v", err)
	}
	defer func() { _ = reg.Close() }()
	if len(reg.Declarations()) != 2 {
		t.Fatalf("探测失败后工具仍应可用，实际声明 %d 个", len(reg.Declarations()))
	}
	logs := buf.String()
	for _, want := range []string{"启动探测失败", "dial tcp", "403"} {
		if !strings.Contains(logs, want) {
			t.Errorf("启动日志里应当有 %q，实际：\n%s", want, logs)
		}
	}
}

// TestRegistryConstructionErrorFailsStartup: a client that cannot even be built (bad config for
// the SDK) is a startup error, not a warning.
func TestRegistryConstructionErrorFailsStartup(t *testing.T) {
	logger, _ := testLogger()
	fd := &fakeDeps{mysqlErr: errors.New("bad dsn")}
	if _, err := newRegistry(Config{MySQL: testMySQLConfig()}, logger, fd.deps()); err == nil {
		t.Fatal("构造失败应当返回错误")
	}
}

// TestRegistryClose: closing the registry closes the MySQL pool.
func TestRegistryClose(t *testing.T) {
	logger, _ := testLogger()
	fd := &fakeDeps{}
	reg, err := newRegistry(Config{MySQL: testMySQLConfig()}, logger, fd.deps())
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	if err := reg.Close(); err != nil {
		t.Fatalf("关闭失败：%v", err)
	}
	if fd.mysqlConn == nil || !fd.mysqlConn.closed {
		t.Error("Close 应当关闭 MySQL 连接池")
	}
}

// TestRegistryNilSafety: a nil *Registry must behave like "not configured" instead of panicking.
// All three methods are called on a nil receiver by the helpers in core when nothing is wired.
func TestRegistryNilSafety(t *testing.T) {
	var reg *Registry
	if got := reg.Declarations(); got != nil {
		t.Errorf("nil Registry 的 Declarations 应当返回 nil，实际 %v", got)
	}
	if got := reg.ToolsFor("s", "w", nil); got != nil {
		t.Errorf("nil Registry 的 ToolsFor 应当返回 nil，实际 %v", got)
	}
	if err := reg.Close(); err != nil {
		t.Errorf("nil Registry 的 Close 应当返回 nil，实际 %v", err)
	}
}

// TestToolsForCarriesSessionAndWorkID: the per-work map is what makes the audit lines know their
// session. Assert the tool really holds the ids it was built with (through a real execution).
func TestToolsForCarriesSessionAndWorkID(t *testing.T) {
	logger, buf := testLogger()
	fd := &fakeDeps{}
	reg, err := newRegistry(Config{MySQL: testMySQLConfig(), TOS: testTOSConfig()}, logger, fd.deps())
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	defer func() { _ = reg.Close() }()
	tools := reg.ToolsFor("sess-42", "work-99", logger)
	if len(tools) != 2 {
		t.Fatalf("应当有 2 个工具，实际 %d 个", len(tools))
	}
	// One execution is enough to see the ids on the audit line.
	result := tools[ToolNameReadObject].Execute(context.Background(), []byte(`{"key":"example-prefix/reviews.csv"}`))
	if result.IsError {
		t.Fatalf("读取应该成功：%s", result.Content[0].Text)
	}
	logs := buf.String()
	for _, want := range []string{"session_id=sess-42", "work_id=work-99"} {
		if !strings.Contains(logs, want) {
			t.Errorf("审计日志里应当有 %q，实际：\n%s", want, logs)
		}
	}
}

// TestConfigEnabled: the top-level "is anything configured" check.
func TestConfigEnabled(t *testing.T) {
	if (Config{}).Enabled() {
		t.Error("零值配置应当是未启用")
	}
	if !(Config{MySQL: MySQLConfig{Enabled: true}}).Enabled() {
		t.Error("启用 MySQL 应当算启用")
	}
	if !(Config{TOS: TOSConfig{Enabled: true}}).Enabled() {
		t.Error("启用 TOS 应当算启用")
	}
}

// TestConfigValidateDisablesIgnored: a disabled section with leftover fields is not validated
// (it is a leftover, not a mistake the user wants to be blocked on).
func TestConfigValidateDisablesIgnored(t *testing.T) {
	if err := (Config{MySQL: MySQLConfig{Host: "h"}}).Validate(); err != nil {
		t.Errorf("未启用的段落不该校验：%v", err)
	}
}

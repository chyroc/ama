// Unit tests for connection config and DSN building (standard.go). All offline: no network,
// no database.
//
// For real connectivity, see "Verify connectivity" in the README.
//
// The two most important groups: **the DSN may only contain standard libpq keys that pgx
// knows** (unknown ones are silently sent as PostgreSQL startup parameters, and the error does
// not point at the real cause at all), and **IPv6 must not have square brackets**.

package pg

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

// baseStdConfig is a minimal valid standard PG config.
func baseStdConfig() StdConfig {
	return StdConfig{
		Host:     "127.0.0.1",
		Port:     15432,
		Database: "ama",
		User:     "ama",
		Password: "s3cret",
	}
}

// ---------------------------------------------------------------------------
// Config validation
// ---------------------------------------------------------------------------

func TestStdConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     StdConfig
		wantErr error
	}{
		{"全空", StdConfig{}, ErrInvalidConfig},
		{"缺 Host", StdConfig{Database: "ama", User: "ama"}, ErrInvalidConfig},
		{"缺 Database", StdConfig{Host: "h", User: "ama"}, ErrInvalidConfig},
		{"缺 User", StdConfig{Host: "h", Database: "ama"}, ErrInvalidConfig},
		{"全空格也算空", StdConfig{Host: " ", Database: " ", User: " "}, ErrInvalidConfig},
		{"端口越界", StdConfig{Host: "h", Database: "ama", User: "u", Port: 70000}, ErrInvalidConfig},
		{"端口为负", StdConfig{Host: "h", Database: "ama", User: "u", Port: -1}, ErrInvalidConfig},
		// An empty password is valid: local trust auth, or ~/.pgpass provides the password.
		{"没密码也合法", StdConfig{Host: "h", Database: "ama", User: "u"}, nil},
		{"最小合法配置", StdConfig{Host: "h", Database: "ama", User: "u"}, nil},
		// Bracketed IPv6 is non-empty after stripping, so it is valid
		{"方括号 IPv6 合法", StdConfig{Host: "[::1]", Database: "ama", User: "u"}, nil},
		// Only brackets, empty after stripping: must be rejected
		{"空方括号算空", StdConfig{Host: "[]", Database: "ama", User: "u"}, ErrInvalidConfig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.validate()
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("期望通过校验，实际报错: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("期望 %v，实际 %v", tc.wantErr, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The DSN may only contain standard keys
// ---------------------------------------------------------------------------

// TestStdDSNOnlyHasLibpqKeys checks that the DSN only contains standard libpq keys that pgx
// knows.
//
// pgx passes keys it does not know to PostgreSQL as startup parameters as is, and the server
// then reports an error that does not point at the real cause at all; tens of minutes get lost
// on it. Here a batch of non-standard key names that "slipped in before, or are easy to add by
// mistake" are used for negative assertions.
func TestStdDSNOnlyHasLibpqKeys(t *testing.T) {
	cfg := baseStdConfig()
	cfg.ExtraParams = map[string]string{"application_name": "ama"}
	dsn, err := cfg.toStdDSN()
	if err != nil {
		t.Fatalf("拼 DSN 失败: %v", err)
	}
	// None of these are libpq keys; if one shows up, the build logic missed something
	for _, forbidden := range []string{
		"pool_max_conns", "enable_pgx_pool", "disable_metrics",
	} {
		if strings.Contains(dsn, forbidden+"=") {
			t.Errorf("DSN 里不该出现非 libpq 键 %q，实际 DSN: %s", forbidden, dsn)
		}
	}

	// Positive: every key that appears must be on the allowlist
	allowed := map[string]bool{
		keyHost: true, keyPort: true, keyDBName: true, keyUser: true,
		keyPassword: true, keySSLMode: true, keyConnectTimeout: true,
		keyStatementTimeout: true, keyLockTimeout: true,
		"application_name": true,
	}
	for _, field := range strings.Fields(dsn) {
		if idx := strings.Index(field, "="); idx > 0 && !allowed[field[:idx]] {
			t.Errorf("DSN 里出现了预期外的键 %q，实际 DSN: %s", field[:idx], dsn)
		}
	}
}

// TestStdPoolDefaultsAreSane keeps the pool defaults from being changed to unreasonable values.
//
// The upper bound is based on PostgreSQL's max_connections: 100 by default, and only a few
// hundred on small managed instances. So the defaults must be clearly below it, leaving room
// for other service instances and ops tools.
func TestStdPoolDefaultsAreSane(t *testing.T) {
	if stdDefaultMaxOpenConns <= 0 {
		t.Errorf("默认连接数 %d 必须是正数", stdDefaultMaxOpenConns)
	}
	if stdDefaultMaxOpenConns > 50 {
		t.Errorf("默认连接数 %d 太大了——最小规格实例只有 200 个连接，"+
			"多个服务实例共享一个库时容易一起把它顶满", stdDefaultMaxOpenConns)
	}
	if stdDefaultMaxIdleConns > stdDefaultMaxOpenConns {
		t.Errorf("空闲连接数 %d 不该超过最大连接数 %d",
			stdDefaultMaxIdleConns, stdDefaultMaxOpenConns)
	}
	if stdDefaultConnMaxIdleTime <= 0 || stdDefaultConnMaxLifetime <= 0 {
		t.Error("存活上限必须是正数，否则等于永不回收")
	}
}

// ---------------------------------------------------------------------------
// DSN building
// ---------------------------------------------------------------------------

func TestToStdDSNDefaults(t *testing.T) {
	dsn, err := StdConfig{Host: "127.0.0.1", Database: "ama", User: "ama"}.toStdDSN()
	if err != nil {
		t.Fatalf("拼 DSN 失败: %v", err)
	}
	want := map[string]string{
		keyHost:    "127.0.0.1",
		keyPort:    "5432", // empty means the default port
		keyDBName:  "ama",
		keyUser:    "ama",
		keySSLMode: defaultSSLMode,
	}
	for k, v := range want {
		if !strings.Contains(dsn, k+"="+v) {
			t.Errorf("DSN 里应当包含 %s=%s，实际: %s", k, v, dsn)
		}
	}
	// With no password there must be no password key: password='' makes trust/peer auth
	// behave inconsistently
	if strings.Contains(dsn, keyPassword+"=") {
		t.Errorf("没设密码时不该出现 %s 键，实际: %s", keyPassword, dsn)
	}
	// No timeout set means nothing is passed down
	if strings.Contains(dsn, keyConnectTimeout+"=") {
		t.Errorf("没设 ConnectTimeout 时不该出现该键，实际: %s", dsn)
	}
}

// TestToStdDSNIPv6NoBrackets guards the easiest trap in this package.
//
// Many docs say "IPv6 addresses in a connection string must be wrapped in square brackets",
// but that applies only to the URI form. In libpq's key=value form, brackets are treated as
// part of the host name:
//
//	host=[::1]  →  hostname resolving error: lookup [::1]: no such host
//	host=::1    →  connects normally
//
// So a bare address and a bracketed one must build **the same** DSN, with no brackets in it.
func TestToStdDSNIPv6NoBrackets(t *testing.T) {
	const v6 = "fd12:3456:789a:1:2dbe:9cef:1433:3c33"

	bare, err := StdConfig{Host: v6, Database: "ama", User: "u"}.toStdDSN()
	if err != nil {
		t.Fatalf("裸 IPv6 拼 DSN 失败: %v", err)
	}
	bracketed, err := StdConfig{Host: "[" + v6 + "]", Database: "ama", User: "u"}.toStdDSN()
	if err != nil {
		t.Fatalf("带方括号 IPv6 拼 DSN 失败: %v", err)
	}

	if bare != bracketed {
		t.Errorf("带方括号和不带应当拼出同一个 DSN\n裸:   %s\n方括号: %s", bare, bracketed)
	}
	if strings.ContainsAny(bare, "[]") {
		t.Errorf("DSN 里不该出现方括号（会被当成主机名的一部分），实际: %s", bare)
	}
	if !strings.Contains(bare, keyHost+"="+v6) {
		t.Errorf("DSN 里应当是裸的 IPv6 地址，实际: %s", bare)
	}
}

func TestToStdDSNOptions(t *testing.T) {
	cfg := baseStdConfig()
	cfg.SSLMode = "require"
	cfg.ConnectTimeout = 5 * time.Second
	dsn, err := cfg.toStdDSN()
	if err != nil {
		t.Fatalf("拼 DSN 失败: %v", err)
	}
	for _, want := range []string{
		keySSLMode + "=require",
		keyConnectTimeout + "=5",
		keyPassword + "=s3cret",
		keyPort + "=15432",
	} {
		if !strings.Contains(dsn, want) {
			t.Errorf("DSN 里应当包含 %q，实际: %s", want, dsn)
		}
	}
}

// TestToStdDSNConnectTimeoutRoundsUp: libpq's connect_timeout is in seconds, and less than 1
// second is truncated to 0 (= no timeout), which is "set but not in effect". So round up.
func TestToStdDSNConnectTimeoutRoundsUp(t *testing.T) {
	cases := []struct {
		name string
		in   time.Duration
		want string
	}{
		{"半秒向上取整到 1", 500 * time.Millisecond, "1"},
		{"刚好 1 秒", time.Second, "1"},
		{"1.2 秒向上取整到 2", 1200 * time.Millisecond, "2"},
		{"整 5 秒", 5 * time.Second, "5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseStdConfig()
			cfg.ConnectTimeout = tc.in
			dsn, err := cfg.toStdDSN()
			if err != nil {
				t.Fatalf("拼 DSN 失败: %v", err)
			}
			if !strings.Contains(dsn, keyConnectTimeout+"="+tc.want) {
				t.Errorf("期望 %s=%s，实际: %s", keyConnectTimeout, tc.want, dsn)
			}
		})
	}
}

// TestToStdDSNQueryTimeouts guards four things: "query timeouts are on by default,
// configurable, can be turned off, and ExtraParams can override them". If the defaults are
// lost, production gets "a stuck query holds a connection forever and ctx cancel does not
// return".
func TestToStdDSNQueryTimeouts(t *testing.T) {
	t.Run("零值用默认 10s/2s", func(t *testing.T) {
		dsn, err := StdConfig{Host: "h", Database: "d", User: "u"}.toStdDSN()
		if err != nil {
			t.Fatalf("拼 DSN 失败: %v", err)
		}
		for k, want := range map[string]string{
			keyStatementTimeout: "10000",
			keyLockTimeout:      "2000",
		} {
			if !strings.Contains(dsn, k+"="+want) {
				t.Errorf("期望默认 %s=%s，实际: %s", k, want, dsn)
			}
		}
	})
	t.Run("自定义值按毫秒下发", func(t *testing.T) {
		cfg := StdConfig{Host: "h", Database: "d", User: "u",
			StatementTimeout: 30 * time.Second, LockTimeout: 500 * time.Millisecond}
		dsn, err := cfg.toStdDSN()
		if err != nil {
			t.Fatalf("拼 DSN 失败: %v", err)
		}
		if !strings.Contains(dsn, keyStatementTimeout+"=30000") {
			t.Errorf("期望 statement_timeout=30000，实际: %s", dsn)
		}
		if !strings.Contains(dsn, keyLockTimeout+"=500") {
			t.Errorf("期望 lock_timeout=500，实际: %s", dsn)
		}
	})
	t.Run("负值显式关闭", func(t *testing.T) {
		cfg := StdConfig{Host: "h", Database: "d", User: "u",
			StatementTimeout: -1, LockTimeout: -1}
		dsn, err := cfg.toStdDSN()
		if err != nil {
			t.Fatalf("拼 DSN 失败: %v", err)
		}
		if !strings.Contains(dsn, keyStatementTimeout+"=0") ||
			!strings.Contains(dsn, keyLockTimeout+"=0") {
			t.Errorf("期望两个超时都下发 0（关闭），实际: %s", dsn)
		}
	})
	t.Run("ExtraParams 仍然能覆盖", func(t *testing.T) {
		cfg := StdConfig{Host: "h", Database: "d", User: "u",
			ExtraParams: map[string]string{keyStatementTimeout: "60000"}}
		dsn, err := cfg.toStdDSN()
		if err != nil {
			t.Fatalf("拼 DSN 失败: %v", err)
		}
		if strings.Contains(dsn, keyStatementTimeout+"=10000") {
			t.Errorf("ExtraParams 应覆盖默认值，实际: %s", dsn)
		}
		if !strings.Contains(dsn, keyStatementTimeout+"=60000") {
			t.Errorf("期望 ExtraParams 覆盖成 60000，实际: %s", dsn)
		}
	})
}

// TestToStdDSNRejectsMisspelledKeys: easy-to-misspell key names must be caught locally.
// These keys do not error but are silently ignored, which is much harder to track down than
// an error.
func TestToStdDSNRejectsMisspelledKeys(t *testing.T) {
	for _, bad := range []string{"ssl_mode", "SSL_MODE", "db_name", "database"} {
		t.Run(bad, func(t *testing.T) {
			cfg := baseStdConfig()
			cfg.ExtraParams = map[string]string{bad: "x"}
			_, err := cfg.toStdDSN()
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("期望 ErrInvalidConfig，实际 %v", err)
			}
			if !strings.Contains(err.Error(), "应该写") {
				t.Errorf("报错应当指出正确写法，实际: %v", err)
			}
		})
	}
}

func TestToStdDSNExtraParams(t *testing.T) {
	cfg := baseStdConfig()
	cfg.ExtraParams = map[string]string{"application_name": "ama probe"}
	dsn, err := cfg.toStdDSN()
	if err != nil {
		t.Fatalf("拼 DSN 失败: %v", err)
	}
	// The value has a space and must be quoted, otherwise the DSN syntax is wrong
	if !strings.Contains(dsn, "application_name='ama probe'") {
		t.Errorf("带空格的值应当被引号包起来，实际: %s", dsn)
	}

	// A same-name key in ExtraParams overrides the struct field
	cfg.ExtraParams = map[string]string{keySSLMode: "verify-full"}
	dsn, err = cfg.toStdDSN()
	if err != nil {
		t.Fatalf("拼 DSN 失败: %v", err)
	}
	if !strings.Contains(dsn, keySSLMode+"=verify-full") {
		t.Errorf("ExtraParams 应当覆盖 SSLMode，实际: %s", dsn)
	}

	// Empty keys must be rejected
	cfg.ExtraParams = map[string]string{"  ": "x"}
	if _, err := cfg.toStdDSN(); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("空键应当报 ErrInvalidConfig，实际 %v", err)
	}
}

// TestToStdDSNIsDeterministic: the same config must build the same DSN every time, otherwise
// logs and tests are hard to compare (map iteration order is random; sort.Strings handles it).
func TestToStdDSNIsDeterministic(t *testing.T) {
	cfg := baseStdConfig()
	cfg.ExtraParams = map[string]string{
		"application_name":  "ama",
		"statement_timeout": "5000",
		"search_path":       "public",
	}
	first, err := cfg.toStdDSN()
	if err != nil {
		t.Fatalf("拼 DSN 失败: %v", err)
	}
	for i := 0; i < 20; i++ {
		got, err := cfg.toStdDSN()
		if err != nil {
			t.Fatalf("第 %d 次拼 DSN 失败: %v", i, err)
		}
		if got != first {
			t.Fatalf("第 %d 次拼出的 DSN 不一致\n首次: %s\n本次: %s", i, first, got)
		}
	}
}

// ---------------------------------------------------------------------------
// The password must not leak
// ---------------------------------------------------------------------------

// TestRedactedHidesPassword: error messages carry Redacted(), so it must never leak the
// plain-text password.
func TestRedactedHidesPassword(t *testing.T) {
	cfg := baseStdConfig()
	got := cfg.Redacted()
	if strings.Contains(got, "s3cret") {
		t.Errorf("Redacted() 不该包含明文密码，实际: %s", got)
	}
	if !strings.Contains(got, "***") {
		t.Errorf("Redacted() 应当把密码渲染成 ***，实际: %s", got)
	}
	// Other parameters must be kept, otherwise there is no info when debugging
	for _, want := range []string{"127.0.0.1", "15432", "ama"} {
		if !strings.Contains(got, want) {
			t.Errorf("Redacted() 应当保留 %q，实际: %s", want, got)
		}
	}

	// With no password it must say it is empty, not show *** as well
	noPwd := baseStdConfig()
	noPwd.Password = ""
	if got := noPwd.Redacted(); !strings.Contains(got, "(空)") {
		t.Errorf("没密码时应当标明是空，实际: %s", got)
	}
}

// TestNewStandardErrorHasNoPassword: the error message for a failed connection must not
// contain the plain-text password either. It uses a port that surely fails, to reach the
// Ping failure branch.
func TestNewStandardErrorHasNoPassword(t *testing.T) {
	cfg := StdConfig{
		Host: "127.0.0.1", Port: 1, Database: "ama", User: "ama",
		Password: "sup3rs3cret", ConnectTimeout: time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := NewStandard(ctx, cfg)
	if err == nil {
		t.Fatal("期望连接失败（端口 1 不可能有 PostgreSQL）")
	}
	if strings.Contains(err.Error(), "sup3rs3cret") {
		t.Errorf("错误信息里不该有明文密码，实际: %v", err)
	}
}

func TestNewStandardValidatesBeforeConnecting(t *testing.T) {
	if _, err := NewStandard(context.Background(), StdConfig{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("空配置应当在发起连接前就报 ErrInvalidConfig，实际 %v", err)
	}
}

// ---------------------------------------------------------------------------
// Pool settings
// ---------------------------------------------------------------------------

func TestApplyStdPoolDefaults(t *testing.T) {
	db := openFakeDB(t)
	defer db.Close()

	StdConfig{}.applyStdPool(db)
	stats := db.Stats()
	if stats.MaxOpenConnections != stdDefaultMaxOpenConns {
		t.Errorf("期望最大连接数 %d，实际 %d", stdDefaultMaxOpenConns, stats.MaxOpenConnections)
	}
	// Borrow MaxIdle+1 and return them; only then can we see how many the idle pool kept
	if got := idleAfterUse(t, db, stdDefaultMaxIdleConns+1); got != stdDefaultMaxIdleConns {
		t.Errorf("期望空闲连接数 %d，实际 %d", stdDefaultMaxIdleConns, got)
	}
}

func TestApplyStdPoolExplicit(t *testing.T) {
	db := openFakeDB(t)
	defer db.Close()

	StdConfig{MaxOpenConns: 7, MaxIdleConns: 3}.applyStdPool(db)
	if got := db.Stats().MaxOpenConnections; got != 7 {
		t.Errorf("期望最大连接数 7，实际 %d", got)
	}
	if got := idleAfterUse(t, db, 4); got != 3 {
		t.Errorf("期望空闲连接数 3，实际 %d", got)
	}
}

// TestApplyStdPoolIdleClamped: the idle count must not exceed the max. If it does,
// database/sql silently uses the max, and the config no longer matches the real behavior.
func TestApplyStdPoolIdleClamped(t *testing.T) {
	db := openFakeDB(t)
	defer db.Close()

	StdConfig{MaxOpenConns: 2, MaxIdleConns: 50}.applyStdPool(db)
	if got := idleAfterUse(t, db, 2); got != 2 {
		t.Errorf("空闲连接数应当被夹到 2，实际 %d", got)
	}
}

// TestNewStandardClientSurface: on the Client returned by NewStandard, all shared methods must
// work (the database field must be filled in, otherwise error messages say db=).
func TestNewStandardClientSurface(t *testing.T) {
	// Do not really connect; build a Client equivalent to NewStandard's to check the fields.
	db := openFakeDB(t)
	defer db.Close()
	c := &Client{db: db, database: "ama"}

	if got := c.Database(); got != "ama" {
		t.Errorf("Database() 期望 ama，实际 %q", got)
	}
	if c.Raw() == nil {
		t.Error("Raw() 不该返回 nil")
	}
	var _ sql.DBStats = c.Stats()
}

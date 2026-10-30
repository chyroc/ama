// Unit tests for the config package. All offline; no real config file is read (except temp files
// written explicitly into t.TempDir()).
//
// The focus is two things that are easy to get wrong: **the priority of environment variable
// overrides**, and **misspelled keys must be reported instead of silently ignored**. The latter
// is the same kind of pitfall this repo keeps hitting: "set but not in effect" is much harder to
// track down than "fails outright".

package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// minimalYAML is the smallest config that passes validation.
const minimalYAML = `
pg:
  host: 127.0.0.1
  port: 15432
  database: ama
  user: ama
  password: ama
`

func TestParseMinimal(t *testing.T) {
	cfg, err := Parse([]byte(minimalYAML))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if cfg.PG.Host != "127.0.0.1" {
		t.Errorf("host 期望 127.0.0.1，实际 %q", cfg.PG.Host)
	}
	if cfg.PG.Port != 15432 {
		t.Errorf("port 期望 15432，实际 %d", cfg.PG.Port)
	}
	if cfg.PG.Database != "ama" {
		t.Errorf("database 期望 ama，实际 %q", cfg.PG.Database)
	}
}

// TestParseDurations: 5s / 30m in YAML must parse into time.Duration.
func TestParseDurations(t *testing.T) {
	cfg, err := Parse([]byte(`
pg:
  host: h
  database: ama
  user: u
  connectTimeout: 5s
  connMaxIdleTime: 4m
  connMaxLifetime: 30m
`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if cfg.PG.ConnectTimeout != 5*time.Second {
		t.Errorf("connectTimeout 期望 5s，实际 %v", cfg.PG.ConnectTimeout)
	}
	if cfg.PG.ConnMaxIdleTime != 4*time.Minute {
		t.Errorf("connMaxIdleTime 期望 4m，实际 %v", cfg.PG.ConnMaxIdleTime)
	}
	if cfg.PG.ConnMaxLifetime != 30*time.Minute {
		t.Errorf("connMaxLifetime 期望 30m，实际 %v", cfg.PG.ConnMaxLifetime)
	}
}

// TestParseReportsUnknownFieldWithoutFailing: a misspelled key must be reported, but **must not
// block startup**.
//
// Two needs pull against each other; this test pins the final tradeoff:
//   - by default yaml silently ignores unknown fields: write maxOpenConns without the s and you
//     will think it is set when it is not;
//   - but failing outright turns "config shipped before code" into an outage: the old binary
//     exits when it reads the new field. During a rolling release old and new binaries run side
//     by side, old instances will certainly read the new field, and an error means all instances
//     restart over and over.
//
// So unknown fields go into Result.UnknownFields (warned in the startup log), and the service
// starts as usual.
func TestParseReportsUnknownFieldWithoutFailing(t *testing.T) {
	res, err := ParseLayers("test", []byte(`
pg:
  host: h
  database: ama
  user: u
  maxOpenConn: 25
`))
	if err != nil {
		t.Fatalf("未知字段不该让解析失败，实际: %v", err)
	}
	if len(res.UnknownFields) != 1 {
		t.Fatalf("未知字段应当被记账，实际: %v", res.UnknownFields)
	}
	if !strings.Contains(res.UnknownFields[0], "maxOpenConn") {
		t.Errorf("记账应当点出是哪个键，实际: %v", res.UnknownFields)
	}
	// Key point: known fields still take effect. yaml.v3's TypeError is a soft error and the
	// struct has been filled, so "skip unknown fields" does not mean "the whole layer is void".
	if res.Config.PG.Host != "h" || res.Config.PG.Database != "ama" {
		t.Errorf("同一层里认识的字段应当照常生效，实际 host=%q database=%q",
			res.Config.PG.Host, res.Config.PG.Database)
	}
}

// TestParseRejectsWrongType: a wrong type must still be an error.
//
// This is what is easiest to let slip after downgrading unknown fields: if port written as a
// string also slipped through, it would silently become a zero value, and a "port 0" failure does
// not point at the config.
func TestParseRejectsWrongType(t *testing.T) {
	_, err := ParseLayers("test", []byte(`
pg:
  host: h
  database: ama
  user: u
  port: not-a-number
`))
	if err == nil {
		t.Fatal("类型不匹配必须报错，不能和未知字段一起被放过")
	}
	// yaml's type error message has a line number, not the key name ("line 6: cannot unmarshal
	// !!str into int"), so the assertion is "it points out a type problem", not "it contains the
	// word port".
	if !strings.Contains(err.Error(), "cannot unmarshal") {
		t.Errorf("报错应当指出类型不匹配，实际: %v", err)
	}
}

// TestParseUnknownFieldDoesNotMaskRealError: when one layer has both an unknown field and a type
// error, it must fail; "there is an unknown field" must not swallow every problem in the layer.
func TestParseUnknownFieldDoesNotMaskRealError(t *testing.T) {
	_, err := ParseLayers("test", []byte(`
pg:
  host: h
  database: ama
  user: u
  maxOpenConn: 25
  port: not-a-number
`))
	if err == nil {
		t.Fatal("未知字段与类型错误共存时，类型错误仍要致命")
	}
	if !strings.Contains(err.Error(), "cannot unmarshal") {
		t.Errorf("报错应当指出真正的问题（类型不匹配），实际: %v", err)
	}
	// The reverse must also hold: the error should not mention that unknown field, otherwise the
	// real cause gets buried in noise.
	if strings.Contains(err.Error(), "maxOpenConn") {
		t.Errorf("未知字段不该出现在致命错误里，实际: %v", err)
	}
}

// TestParseSurvivesConfigAheadOfCode: when config ships before code, the service must still start.
//
// If a new field is added to config while the Go code that knows it has not shipped yet, and
// parsing fails and the process exits, then since config is global, **all instances restart over
// and over and not a single one survives**. During rolling releases and rollbacks old and new
// binaries run side by side, and old instances will certainly read the new field, so this shape
// must always start.
//
// It uses a field name that does not exist right now, so the "unknown" path is really tested.
func TestParseSurvivesConfigAheadOfCode(t *testing.T) {
	res, err := ParseLayers("test", []byte(`
pg:
  host: h
  database: ama
  user: u
docker:
  image: ama-sandbox:v2
  someFieldFromTheFuture: 42
`))
	if err != nil {
		t.Fatalf("配置先于代码上线必须仍能启动，实际: %v", err)
	}
	if len(res.UnknownFields) != 1 {
		t.Fatalf("未来字段应当被记账，实际: %v", res.UnknownFields)
	}
	// Known fields in the same mapping must still take effect; otherwise old instances would
	// create containers with the default image, a failure harder to track down than a failed
	// startup.
	if res.Config.Docker.Image != "ama-sandbox:v2" {
		t.Errorf("未知字段的邻居应当照常生效，实际 image=%q", res.Config.Docker.Image)
	}
}

func TestParseValidates(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{"缺 host", "pg:\n  database: ama\n  user: u\n"},
		{"缺 database", "pg:\n  host: h\n  user: u\n"},
		{"缺 user", "pg:\n  host: h\n  database: ama\n"},
		{"host 全空格", "pg:\n  host: '  '\n  database: ama\n  user: u\n"},
		{"端口越界", "pg:\n  host: h\n  database: ama\n  user: u\n  port: 70000\n"},
		{"空配置", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("期望 ErrInvalidConfig，实际 %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Environment variable overrides
// ---------------------------------------------------------------------------

// TestEnvOverridesYAML: environment variables have higher priority than YAML.
//
// This is the basis of the password approach: production never writes the password into any
// file; it is passed in through AMA_PG_PASSWORD, overriding the local dev placeholder in YAML.
func TestEnvOverridesYAML(t *testing.T) {
	t.Setenv(EnvPGHost, "10.0.0.9")
	t.Setenv(EnvPGPort, "5432")
	t.Setenv(EnvPGDatabase, "prod")
	t.Setenv(EnvPGUser, "produser")
	t.Setenv(EnvPGPassword, "prodpwd")
	t.Setenv(EnvPGSSLMode, "require")

	cfg, err := Parse([]byte(minimalYAML))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	for _, tc := range []struct{ name, got, want string }{
		{"host", cfg.PG.Host, "10.0.0.9"},
		{"database", cfg.PG.Database, "prod"},
		{"user", cfg.PG.User, "produser"},
		{"password", cfg.PG.Password, "prodpwd"},
		{"sslmode", cfg.PG.SSLMode, "require"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s 期望被环境变量覆盖成 %q，实际 %q", tc.name, tc.want, tc.got)
		}
	}
	if cfg.PG.Port != 5432 {
		t.Errorf("port 期望被覆盖成 5432，实际 %d", cfg.PG.Port)
	}
}

// TestEmptyEnvDoesNotOverride: environment variables that are unset or set to empty must not wipe
// YAML values.
func TestEmptyEnvDoesNotOverride(t *testing.T) {
	t.Setenv(EnvPGHost, "")
	t.Setenv(EnvPGPassword, "")

	cfg, err := Parse([]byte(minimalYAML))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if cfg.PG.Host != "127.0.0.1" {
		t.Errorf("空环境变量不该覆盖 YAML，host 实际 %q", cfg.PG.Host)
	}
	if cfg.PG.Password != "ama" {
		t.Errorf("空环境变量不该覆盖 YAML，password 实际 %q", cfg.PG.Password)
	}
}

// TestBadEnvPortIgnored: when the port environment variable is not a number, ignore it instead of
// keeping the service from starting. A truly invalid port is caught by validate.
func TestBadEnvPortIgnored(t *testing.T) {
	t.Setenv(EnvPGPort, "不是数字")

	cfg, err := Parse([]byte(minimalYAML))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if cfg.PG.Port != 15432 {
		t.Errorf("非法端口环境变量应当被忽略，port 实际 %d", cfg.PG.Port)
	}
}

// TestEnvPasswordKeepsSpaces: passwords are not TrimSpace'd. In theory a password may start or
// end with spaces; trimming them breaks the connection, and that kind of failure is very hard to
// locate.
func TestEnvPasswordKeepsSpaces(t *testing.T) {
	t.Setenv(EnvPGPassword, "  pwd with spaces  ")

	cfg, err := Parse([]byte(minimalYAML))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if cfg.PG.Password != "  pwd with spaces  " {
		t.Errorf("密码不该被 TrimSpace，实际 %q", cfg.PG.Password)
	}
}

// ---------------------------------------------------------------------------
// Load
// ---------------------------------------------------------------------------

func TestLoadReadsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(minimalYAML), 0o600); err != nil {
		t.Fatalf("写临时配置失败: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if cfg.PG.Database != "ama" {
		t.Errorf("database 期望 ama，实际 %q", cfg.PG.Database)
	}
}

// TestLoadMissingFile: a missing file must be a clear error, not a silent fallback to defaults
// connecting to localhost. A wrong path is a very common slip, and a silent fallback only leaves
// people staring at a puzzling connection error.
func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "没有这个文件.yaml"))
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("期望 ErrInvalidConfig，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "config.example.yaml") {
		t.Errorf("报错应当提示可以从模板复制，实际: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Conversion
// ---------------------------------------------------------------------------

// TestToStdConfig: fields must be passed over one to one; missing one means "configured but not
// in effect".
func TestToStdConfig(t *testing.T) {
	p := PG{
		Host: "::1", Port: 5432, Database: "ama", User: "u", Password: "p",
		SSLMode: "require", ConnectTimeout: 5 * time.Second,
		MaxOpenConns: 7, MaxIdleConns: 3,
		ConnMaxIdleTime: time.Minute, ConnMaxLifetime: time.Hour,
	}
	got := p.ToStdConfig()

	if got.Host != "::1" || got.Port != 5432 || got.Database != "ama" ||
		got.User != "u" || got.Password != "p" || got.SSLMode != "require" {
		t.Errorf("基础字段没传全: %+v", got)
	}
	if got.ConnectTimeout != 5*time.Second {
		t.Errorf("ConnectTimeout 期望 5s，实际 %v", got.ConnectTimeout)
	}
	if got.MaxOpenConns != 7 || got.MaxIdleConns != 3 {
		t.Errorf("池大小没传全: open=%d idle=%d", got.MaxOpenConns, got.MaxIdleConns)
	}
	if got.ConnMaxIdleTime != time.Minute || got.ConnMaxLifetime != time.Hour {
		t.Errorf("池时长没传全: idle=%v life=%v", got.ConnMaxIdleTime, got.ConnMaxLifetime)
	}
}

// TestToRedisConfig: fields must be passed over one to one; missing one means "configured but not
// in effect".
func TestToRedisConfig(t *testing.T) {
	r := Redis{
		Addr:     "127.0.0.1:16379",
		Password: "p", DB: 3,
		DialTimeout: 5 * time.Second, OpTimeout: 2 * time.Second,
	}
	got := r.ToRedisConfig()

	if got.Addr != "127.0.0.1:16379" {
		t.Errorf("地址没传过去: addr=%q", got.Addr)
	}
	if got.Password != "p" || got.DB != 3 {
		t.Errorf("鉴权字段没传全: password=%q db=%d", got.Password, got.DB)
	}
	if got.DialTimeout != 5*time.Second || got.OpTimeout != 2*time.Second {
		t.Errorf("超时没传全: dial=%v op=%v", got.DialTimeout, got.OpTimeout)
	}
}

// The whole Redis section may be empty (then the long connection is not enabled); a non-empty
// Addr counts as configured.
func TestRedisEnabled(t *testing.T) {
	if (Redis{}).Enabled() {
		t.Error("Addr 空该算没配")
	}
	if (Redis{Addr: "  "}).Enabled() {
		t.Error("只有空白字符该算没配")
	}
	if !(Redis{Addr: "127.0.0.1:16379"}).Enabled() {
		t.Error("配了 Addr 该算启用")
	}
}

// TestFeedbackCanRead: the checks of the feedback read allowlist.
//
// These all pin cases of "misconfigured, yet no error": an extra space in the list, different
// letter case, or an empty item left in the list by accident. The first two show up as "I am
// clearly on the list but cannot read"; the third is worse: **an empty item would let an empty
// actor pass**, which means reading without login.
func TestFeedbackCanRead(t *testing.T) {
	cases := []struct {
		name    string
		readers []string
		actor   string
		want    bool
	}{
		{"精确命中", []string{"a@example.com"}, "a@example.com", true},
		{"不在名单里", []string{"a@example.com"}, "b@example.com", false},
		// The list is typed into the config by hand and may use mixed case; actor is lowercased
		// at the auth entry.
		{"大小写不敏感", []string{"A@Example.COM"}, "a@example.com", true},
		// An extra space after a YAML list item is completely invisible.
		{"名单项两边有空格", []string{"  a@example.com  "}, "a@example.com", true},
		{"actor 两边有空格", []string{"a@example.com"}, " a@example.com ", true},
		// ⚠️ Empty means nobody can read (fail-closed). With allow as the fallback, "forgot to
		// configure" would mean the gate does not exist at all, with zero errors.
		{"名单为空一律拒绝", nil, "a@example.com", false},
		{"名单为空切片也拒绝", []string{}, "a@example.com", false},
		// ⚠️ An empty actor is always denied, **even if the list has an empty item**. A `- ""` in
		// the list would make "readable without login" true, and that is a silent unauthorized
		// access.
		{"空 actor 一律拒绝", []string{"a@example.com"}, "", false},
		{"名单里有空项也不放空 actor", []string{"", "  ", "a@example.com"}, "", false},
		{"名单里有空项不影响正常判定", []string{"", "a@example.com"}, "a@example.com", true},
		{"多人名单", []string{"a@example.com", "b@example.com"}, "b@example.com", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Feedback{Readers: tc.readers}.CanRead(tc.actor)
			if got != tc.want {
				t.Errorf("CanRead(%q) with readers=%v = %v, 期望 %v",
					tc.actor, tc.readers, got, tc.want)
			}
		})
	}
}

// TestFeedbackReadersEnvOverride: the environment variable override is comma-separated and
// replaces the whole list.
func TestFeedbackReadersEnvOverride(t *testing.T) {
	t.Setenv(EnvFeedbackReaders, "x@example.com, y@example.com ")
	cfg := Config{Feedback: Feedback{Readers: []string{"old@example.com"}}}
	hit := cfg.applyEnv()

	// **It replaces the whole list, it does not merge with the list in the file**: with merging,
	// "remove one person via the environment variable" cannot be expressed, and that is exactly
	// the operation needed most when something goes wrong.
	if len(cfg.Feedback.Readers) != 2 {
		t.Fatalf("该整份替换成 2 个人，实际 %v", cfg.Feedback.Readers)
	}
	if cfg.Feedback.CanRead("old@example.com") {
		t.Error("文件里那个人该被覆盖掉，实际还能读")
	}
	// Items must be trimmed after splitting on commas: without trimming the spaces around
	// ` y@example.com `, that person could not read.
	if !cfg.Feedback.CanRead("y@example.com") {
		t.Error("逗号分隔的第二个人该能读（值两边的空格要 trim 掉）")
	}
	// The overridden field must be in the EnvOverridden list, otherwise "I changed it in the
	// config file but it did not take effect" cannot be tracked down.
	if !containsField(hit, "feedback.readers") {
		t.Errorf("覆盖清单里该有 feedback.readers，实际 %v", hit)
	}
}

// TestFeedbackReadersEnvIgnoresGarbage: a value with only empty items is treated as unset.
//
// Overriding with an empty list would **quietly remove everyone's read access**, showing up as
// "configured, yet nobody can read".
func TestFeedbackReadersEnvIgnoresGarbage(t *testing.T) {
	t.Setenv(EnvFeedbackReaders, " , , ")
	cfg := Config{Feedback: Feedback{Readers: []string{"keep@example.com"}}}
	cfg.applyEnv()
	if !cfg.Feedback.CanRead("keep@example.com") {
		t.Error("全是空项的环境变量该当没设，原名单要保留")
	}
}

// containsField reports whether a field name is in the override list.
func containsField(fields []string, name string) bool {
	for _, f := range fields {
		if f == name {
			return true
		}
	}
	return false
}

// TestWorkerConcurrencyDefault: when existing config has no workerConcurrency, the default is
// used.
//
// This is about release order: **code ships first, config is changed later**. So "the field is
// not in the config" must be a normal path, not a 0: 0 would be treated as serial, and then a
// second person sending a message in the same workspace would have to wait for the first to go
// idle.
func TestWorkerConcurrencyDefault(t *testing.T) {
	// minimalYAML has no worker section at all, which is exactly the shape of existing config.
	cfg, err := Parse([]byte(minimalYAML))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if cfg.Worker.Concurrency != nil {
		t.Fatalf("没配时指针要是 nil，实际 %v", *cfg.Worker.Concurrency)
	}
	if got := cfg.Worker.ConcurrencyOrDefault(); got != DefaultWorkerConcurrency {
		t.Errorf("缺字段时并发度 = %d，要 %d（默认值）", got, DefaultWorkerConcurrency)
	}
}

// TestWorkerConcurrencyFromYAML: if set, the set value is used, including 1 (falls back to
// serial).
//
// **1 must be settable**: it is the way to stop the bleeding without a release when concurrent
// scheduling goes wrong. The pointer is there to tell "not set" from "explicitly set to 1"; when
// the default is not 1, the two would look the same as an int.
func TestWorkerConcurrencyFromYAML(t *testing.T) {
	for _, tc := range []struct {
		yaml string
		want int
	}{
		{"\nworker:\n  concurrency: 1\n", 1},
		{"\nworker:\n  concurrency: 8\n", 8},
		{"\nworker:\n  concurrency: 50\n", 50},
	} {
		cfg, err := Parse([]byte(minimalYAML + tc.yaml))
		if err != nil {
			t.Fatalf("解析 %q 失败: %v", tc.yaml, err)
		}
		if got := cfg.Worker.ConcurrencyOrDefault(); got != tc.want {
			t.Errorf("配 %q 得到 %d，要 %d", strings.TrimSpace(tc.yaml), got, tc.want)
		}
	}
}

// TestWorkerConcurrencyFloorsAtOne: anything below 1 is treated as 1, never as "unlimited".
//
// Unlimited concurrency would let one user spamming messages use up all containers on the
// machine, and then the victims are other workspaces.
func TestWorkerConcurrencyFloorsAtOne(t *testing.T) {
	for _, n := range []int{0, -1, -100} {
		cfg := Config{Worker: Worker{Concurrency: &n}}
		if got := cfg.Worker.ConcurrencyOrDefault(); got != 1 {
			t.Errorf("配 %d 得到 %d，要 1（串行）而不是不限", n, got)
		}
	}
}

// TestWorkerConcurrencyEnvOverride: the environment variable overrides the value in the config
// file.
//
// AMA_WORKER_CONCURRENCY=1 is a production emergency measure: when concurrent scheduling goes
// wrong, change one environment variable and restart to fall back to serial, no release needed.
func TestWorkerConcurrencyEnvOverride(t *testing.T) {
	t.Setenv(EnvWorkerConcurrency, "1")
	cfg, err := Parse([]byte(minimalYAML + "\nworker:\n  concurrency: 20\n"))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got := cfg.Worker.ConcurrencyOrDefault(); got != 1 {
		t.Errorf("环境变量要盖过 YAML 的 20，实际 %d", got)
	}
}

// TestBadWorkerConcurrencyEnvIgnored: a typo in the environment variable is treated as unset;
// do not keep the service from starting.
//
// Same handling as pg.port. **0 is also treated as unset**: that value means nothing (concurrency
// cannot be 0), and treating it as "explicitly set to 0" would land in the pointer and then be
// floored to 1, looking as if it took effect.
func TestBadWorkerConcurrencyEnvIgnored(t *testing.T) {
	for _, bad := range []string{"abc", "0", "-3", "1.5"} {
		t.Run(bad, func(t *testing.T) {
			t.Setenv(EnvWorkerConcurrency, bad)
			cfg, err := Parse([]byte(minimalYAML))
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if cfg.Worker.Concurrency != nil {
				t.Errorf("坏值 %q 该当没设，实际落成 %d", bad, *cfg.Worker.Concurrency)
			}
			if got := cfg.Worker.ConcurrencyOrDefault(); got != DefaultWorkerConcurrency {
				t.Errorf("坏值 %q 之后要走默认值 %d，实际 %d", bad, DefaultWorkerConcurrency, got)
			}
		})
	}
}

// TestTerminalWSOriginsFromYAML pins that this key is really parsed.
//
// ⚠️ **This group guards against "configured but not in effect"**, which is exactly why these
// tests were added: core.Config had a TerminalWSOrigins field for a long time, but the config
// layer had no matching key and cmd/amad did not wire it, so writing this item in the config file
// had **zero effect**, and the symptom was "the terminal still uses the fallback", which does not
// point at "the config was not read" at all.
//
// This test only proves "YAML → Config works"; the wiring half is covered by amad's startup log
// (see that Warn / Info in main.go). Both halves are needed.
func TestTerminalWSOriginsFromYAML(t *testing.T) {
	cfg, err := Parse([]byte(minimalYAML + `
server:
  terminalWSOrigins:
    - https://ama.example.com
    - http://127.0.0.1:5273
`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(cfg.Server.TerminalWSOrigins) != 2 {
		t.Fatalf("期望解析出 2 个来源，实际 %d 个：%v",
			len(cfg.Server.TerminalWSOrigins), cfg.Server.TerminalWSOrigins)
	}
	if cfg.Server.TerminalWSOrigins[0] != "https://ama.example.com" {
		t.Errorf("第一个来源 = %q", cfg.Server.TerminalWSOrigins[0])
	}
}

// TestTerminalWSOriginsFromEnv pins the comma-separated environment variable override.
//
// Why this door is needed: to verify locally that "the terminal really uses WS", you need a
// same-origin address with a port, and that port follows dev.sh's PORT (each git worktree must
// use its own port). Putting it in config.yaml would mean editing the file every time the port
// changes.
func TestTerminalWSOriginsFromEnv(t *testing.T) {
	// Extra spaces on purpose: this value is typed into the command line by hand.
	t.Setenv(EnvTerminalWSOrigins, " http://127.0.0.1:5327 , https://ama.example.com ")

	cfg, err := Parse([]byte(minimalYAML))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	want := []string{"http://127.0.0.1:5327", "https://ama.example.com"}
	if len(cfg.Server.TerminalWSOrigins) != len(want) {
		t.Fatalf("期望 %d 个来源，实际 %v", len(want), cfg.Server.TerminalWSOrigins)
	}
	for i, w := range want {
		if cfg.Server.TerminalWSOrigins[i] != w {
			t.Errorf("第 %d 个 = %q，期望 %q（两侧空格要被 trim 掉）",
				i, cfg.Server.TerminalWSOrigins[i], w)
		}
	}
}

// TestBlankTerminalWSOriginsEnvDoesNotWipeYAML pins "an all-blank environment variable does not
// clear the YAML".
//
// ⚠️ This is not pedantry: clearing it makes the terminal **silently** fall back to SSE+POST (fast
// typing out of order, one round trip per character), when the user just wrote the environment
// variable as `AMA_TERMINAL_WS_ORIGINS=,,`. Zero errors, and the symptom is far from the cause.
func TestBlankTerminalWSOriginsEnvDoesNotWipeYAML(t *testing.T) {
	t.Setenv(EnvTerminalWSOrigins, " , , ")

	cfg, err := Parse([]byte(minimalYAML + `
server:
  terminalWSOrigins:
    - https://ama.example.com
`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(cfg.Server.TerminalWSOrigins) != 1 {
		t.Fatalf("YAML 里配的来源被空白环境变量清掉了：%v —— "+
			"那会让终端静默回退到 SSE，而用户只是把变量写成了逗号",
			cfg.Server.TerminalWSOrigins)
	}
}

// TestDockerDefaultsAndParsing: defaults of the docker section, and memory / duration parsing.
func TestDockerDefaultsAndParsing(t *testing.T) {
	cfg, err := Parse([]byte(minimalYAML))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !cfg.Docker.DockerEnabled() {
		t.Error("没配 docker 段时默认启用 Docker 执行端")
	}
	if got := cfg.Docker.ContainerTTLOrDefault(); got != 24*time.Hour {
		t.Errorf("containerTTL 默认值 = %s，要 24h", got)
	}
	if got := cfg.Docker.WorkspaceTTLOrDefault(); got != 7*24*time.Hour {
		t.Errorf("workspaceTTL 默认值 = %s，要 168h", got)
	}

	cfg, err = Parse([]byte(minimalYAML + "\ndocker:\n  enabled: false\n  memory: 2g\n  containerTTL: 0s\n  workspaceTTL: 1h30m\n"))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if cfg.Docker.DockerEnabled() {
		t.Error("enabled: false 应当关掉 Docker 执行端")
	}
	if n, err := cfg.Docker.MemoryBytes(); err != nil || n != 2<<30 {
		t.Errorf("memory 2g = %d, %v，要 %d", n, err, int64(2<<30))
	}
	if got := cfg.Docker.ContainerTTLOrDefault(); got != 0 {
		t.Errorf("显式配 0 要被尊重（= 不回收），实际 %s", got)
	}
	if got := cfg.Docker.WorkspaceTTLOrDefault(); got != 90*time.Minute {
		t.Errorf("workspaceTTL = %s，要 1h30m", got)
	}
	if _, err := (Docker{Memory: "lots"}).MemoryBytes(); err == nil {
		t.Error("看不懂的 memory 应当报错")
	}
}

// s3 section: the two nullable booleans default to true when empty, environment variables can
// override them, and secrets are not trimmed.
func TestS3DefaultsAndEnv(t *testing.T) {
	cfg, err := Parse([]byte("pg: {host: h, database: d, user: u}\ns3: {bucket: ama, endpoint: 'http://s3:9000'}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.S3.Enabled() {
		t.Fatal("配了 bucket 应算启用")
	}
	got := cfg.S3.ToS3Config()
	if !got.UsePathStyle || !got.CreateBucket {
		t.Errorf("usePathStyle / createBucket 留空应默认 true，实际 %v / %v", got.UsePathStyle, got.CreateBucket)
	}

	t.Setenv(EnvS3PublicEndpoint, "http://localhost:19000")
	t.Setenv(EnvS3SandboxEndpoint, "http://ama-s3:9000")
	t.Setenv(EnvS3SecretKey, " s p ")
	t.Setenv(EnvS3UsePathStyle, "false")
	t.Setenv(EnvS3CreateBucket, "nonsense") // unrecognized → treated as unset
	res, err := ParseLayers("t", []byte("pg: {host: h, database: d, user: u}\ns3: {bucket: ama}\n"))
	if err != nil {
		t.Fatal(err)
	}
	got = res.Config.S3.ToS3Config()
	if got.PublicEndpoint != "http://localhost:19000" || got.SandboxEndpoint != "http://ama-s3:9000" {
		t.Errorf("endpoint 环境变量没生效：%+v", got)
	}
	if got.SecretKey != " s p " {
		t.Errorf("secretKey 不该被 trim：%q", got.SecretKey)
	}
	if got.UsePathStyle || !got.CreateBucket {
		t.Errorf("布尔覆盖不对：pathStyle=%v create=%v", got.UsePathStyle, got.CreateBucket)
	}

	empty, _ := Parse([]byte("pg: {host: h, database: d, user: u}\n"))
	if empty.S3.Enabled() {
		t.Error("没配 bucket 不应算启用")
	}
}

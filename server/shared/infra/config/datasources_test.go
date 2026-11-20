// Tests for the dataSources config section: YAML parsing, defaults, env overrides, and the
// "enabled but incomplete is a startup error" contract.
//
// The point of these is not the mapping itself but the two silent-failure modes this repo keeps
// hitting: an env var that does not take effect, and a section that looks configured while the
// feature is actually off.

package config

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// dataSourcesYAML is a complete, enabled dataSources section (appended to minimalYAML).
const dataSourcesYAML = `
dataSources:
  mysql:
    enabled: true
    host: mysql.example.com
    port: 3307
    database: example_db
    user: ro_user
    password: yaml-password
    maxRows: 50
    timeout: 12s
  tos:
    enabled: true
    endpoint: tos-s3-cn-beijing.volces.com
    region: cn-beijing
    bucket: example-bucket
    prefix: example-prefix
    accessKey: yaml-ak
    secretKey: yaml-sk
    maxBytes: 2048
    timeout: 5s
    usePathStyle: true
`

// TestParseDataSources: every field lands where ToConfig expects it.
func TestParseDataSources(t *testing.T) {
	cfg, err := Parse([]byte(minimalYAML + dataSourcesYAML))
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if !cfg.DataSources.Enabled() {
		t.Fatal("两段都 enabled: true，应当算已启用")
	}
	got := cfg.DataSources.ToConfig()
	if got.MySQL.Host != "mysql.example.com" || got.MySQL.Port != 3307 {
		t.Errorf("mysql host/port 没解析对：%+v", got.MySQL)
	}
	if got.MySQL.Database != "example_db" || got.MySQL.User != "ro_user" {
		t.Errorf("mysql database/user 没解析对：%+v", got.MySQL)
	}
	if got.MySQL.Password != "yaml-password" {
		t.Errorf("mysql password 没解析对")
	}
	if got.MySQL.MaxRows != 50 || got.MySQL.Timeout != 12*time.Second {
		t.Errorf("mysql maxRows/timeout 没解析对：%+v", got.MySQL)
	}
	if got.TOS.Endpoint != "tos-s3-cn-beijing.volces.com" || got.TOS.Region != "cn-beijing" {
		t.Errorf("tos endpoint/region 没解析对：%+v", got.TOS)
	}
	if got.TOS.Bucket != "example-bucket" || got.TOS.Prefix != "example-prefix" {
		t.Errorf("tos bucket/prefix 没解析对：%+v", got.TOS)
	}
	if got.TOS.AccessKey != "yaml-ak" || got.TOS.SecretKey != "yaml-sk" {
		t.Errorf("tos 凭证没解析对")
	}
	if got.TOS.MaxBytes != 2048 || got.TOS.Timeout != 5*time.Second || !got.TOS.UsePathStyle {
		t.Errorf("tos maxBytes/timeout/usePathStyle 没解析对：%+v", got.TOS)
	}
}

// TestParseDataSourcesDisabled: leftover fields under `enabled: false` do not block startup.
func TestParseDataSourcesDisabled(t *testing.T) {
	cfg, err := Parse([]byte(minimalYAML + `
dataSources:
  mysql:
    enabled: false
    host: ""
  tos:
    enabled: false
`))
	if err != nil {
		t.Fatalf("禁用段落不该影响启动：%v", err)
	}
	if cfg.DataSources.Enabled() {
		t.Fatal("都禁用了，应当算未启用")
	}
}

// TestParseDataSourcesIncompleteFails: enabled but missing fields is ErrInvalidConfig.
//
// The whole point: "the config looks enabled but the model has no tool" must not be a
// zero-error state.
func TestParseDataSourcesIncompleteFails(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "mysql enabled without user",
			yaml: `
dataSources:
  mysql:
    enabled: true
    host: h
    database: db
    password: pw
`,
			want: "user",
		},
		{
			name: "mysql enabled without password",
			yaml: `
dataSources:
  mysql:
    enabled: true
    host: h
    database: db
    user: u
`,
			want: "password",
		},
		{
			name: "tos enabled without prefix",
			yaml: `
dataSources:
  tos:
    enabled: true
    endpoint: e
    region: r
    bucket: b
    accessKey: ak
    secretKey: sk
`,
			want: "prefix",
		},
		{
			name: "tos enabled without bucket",
			yaml: `
dataSources:
  tos:
    enabled: true
    endpoint: e
    region: r
    prefix: p/
    accessKey: ak
    secretKey: sk
`,
			want: "bucket",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(minimalYAML + tc.yaml))
			if err == nil {
				t.Fatal("应当返回配置错误")
			}
			if !errors.Is(err, ErrInvalidConfig) {
				t.Errorf("错误应当可以用 errors.Is(ErrInvalidConfig) 判断，实际：%v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息里应当提到 %q，实际：%v", tc.want, err)
			}
		})
	}
}

// TestDataSourcesEnvOverride: the credentials are meant to come from env vars, and the startup
// log must be able to show **which fields** were overridden (names only, never values).
func TestDataSourcesEnvOverride(t *testing.T) {
	t.Setenv(EnvDataMySQLEnabled, "true")
	t.Setenv(EnvDataMySQLHost, "env-mysql.example.com")
	t.Setenv(EnvDataMySQLPort, "3308")
	t.Setenv(EnvDataMySQLDatabase, "env_db")
	t.Setenv(EnvDataMySQLUser, "env_ro")
	t.Setenv(EnvDataMySQLPassword, "env-password")
	t.Setenv(EnvDataMySQLMaxRows, "77")
	t.Setenv(EnvDataMySQLTimeout, "9s")
	t.Setenv(EnvDataTOSEnabled, "true")
	t.Setenv(EnvDataTOSEndpoint, "env-tos.example.com")
	t.Setenv(EnvDataTOSRegion, "cn-shanghai")
	t.Setenv(EnvDataTOSBucket, "env-bucket")
	t.Setenv(EnvDataTOSPrefix, "env-prefix/")
	t.Setenv(EnvDataTOSAccessKey, "env-ak")
	t.Setenv(EnvDataTOSSecretKey, "env-sk")
	t.Setenv(EnvDataTOSMaxBytes, "4096")
	t.Setenv(EnvDataTOSTimeout, "7s")
	t.Setenv(EnvDataTOSUsePathStyle, "true")

	res, err := ParseLayers("inline", []byte(minimalYAML))
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	cfg := res.Config
	if !cfg.DataSources.MySQL.Enabled || !cfg.DataSources.TOS.Enabled {
		t.Fatal("enabled 应当被环境变量打开")
	}
	if cfg.DataSources.MySQL.Host != "env-mysql.example.com" || cfg.DataSources.MySQL.Port != 3308 {
		t.Errorf("mysql host/port 没被覆盖：%+v", cfg.DataSources.MySQL)
	}
	if cfg.DataSources.MySQL.Password != "env-password" || cfg.DataSources.MySQL.MaxRows != 77 {
		t.Errorf("mysql password/maxRows 没被覆盖：%+v", cfg.DataSources.MySQL)
	}
	if cfg.DataSources.MySQL.Timeout != 9*time.Second {
		t.Errorf("mysql timeout 没被覆盖：%v", cfg.DataSources.MySQL.Timeout)
	}
	if cfg.DataSources.TOS.Bucket != "env-bucket" || cfg.DataSources.TOS.Prefix != "env-prefix/" {
		t.Errorf("tos bucket/prefix 没被覆盖：%+v", cfg.DataSources.TOS)
	}
	if cfg.DataSources.TOS.SecretKey != "env-sk" || cfg.DataSources.TOS.MaxBytes != 4096 {
		t.Errorf("tos secretKey/maxBytes 没被覆盖：%+v", cfg.DataSources.TOS)
	}
	if cfg.DataSources.TOS.Timeout != 7*time.Second || !cfg.DataSources.TOS.UsePathStyle {
		t.Errorf("tos timeout/usePathStyle 没被覆盖：%+v", cfg.DataSources.TOS)
	}

	overridden := strings.Join(res.EnvOverridden, " ")
	for _, want := range []string{
		"dataSources.mysql.enabled", "dataSources.mysql.host", "dataSources.mysql.port",
		"dataSources.mysql.database", "dataSources.mysql.user", "dataSources.mysql.password",
		"dataSources.mysql.maxRows", "dataSources.mysql.timeout",
		"dataSources.tos.enabled", "dataSources.tos.endpoint", "dataSources.tos.region",
		"dataSources.tos.bucket", "dataSources.tos.prefix", "dataSources.tos.accessKey",
		"dataSources.tos.secretKey", "dataSources.tos.maxBytes", "dataSources.tos.timeout",
		"dataSources.tos.usePathStyle",
	} {
		if !strings.Contains(overridden, want) {
			t.Errorf("EnvOverridden 里应当有 %q，实际：%v", want, res.EnvOverridden)
		}
	}
	// Only field names, never values — secrets must not reach the startup log.
	for _, secret := range []string{"env-password", "env-sk", "env-ak"} {
		if strings.Contains(overridden, secret) {
			t.Errorf("EnvOverridden 里出现了密钥值 %q：%v", secret, res.EnvOverridden)
		}
	}
}

// TestDataSourcesEnvBadValuesIgnored: a typo in one env var is treated as "not set" (it must not
// keep the service from starting), and validate then reports the missing field.
func TestDataSourcesEnvBadValuesIgnored(t *testing.T) {
	t.Setenv(EnvDataMySQLEnabled, "true")
	t.Setenv(EnvDataMySQLHost, "h")
	t.Setenv(EnvDataMySQLDatabase, "db")
	t.Setenv(EnvDataMySQLUser, "u")
	t.Setenv(EnvDataMySQLPassword, "pw")
	t.Setenv(EnvDataMySQLPort, "not-a-number")
	t.Setenv(EnvDataMySQLTimeout, "12") // no unit → not a duration

	cfg, err := Parse([]byte(minimalYAML))
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if cfg.DataSources.MySQL.Port != 0 {
		t.Errorf("非法端口应当被当作未设置，实际 %d", cfg.DataSources.MySQL.Port)
	}
	if cfg.DataSources.MySQL.Timeout != 0 {
		t.Errorf("非法时长应当被当作未设置，实际 %v", cfg.DataSources.MySQL.Timeout)
	}
}

// TestExampleConfigHasDataSources: the template ships the section, with everything disabled (so
// copying it does not turn the tools on by accident).
func TestExampleConfigHasDataSources(t *testing.T) {
	cfg, err := loadExampleConfig(t)
	if err != nil {
		t.Fatalf("模板解析失败：%v", err)
	}
	if cfg.DataSources.Enabled() {
		t.Error("模板里的 dataSources 应当默认关闭")
	}
	if cfg.DataSources.MySQL.MaxRows != 200 {
		t.Errorf("模板里的 mysql.maxRows 应当写 200（默认值），实际 %d", cfg.DataSources.MySQL.MaxRows)
	}
	if cfg.DataSources.TOS.MaxBytes != 1048576 {
		t.Errorf("模板里的 tos.maxBytes 应当写 1048576（默认 1MiB），实际 %d", cfg.DataSources.TOS.MaxBytes)
	}
}

// loadExampleConfig parses server/config.example.yaml, so the template is checked by the same
// validation the service uses (and not just by the "unknown fields" test).
func loadExampleConfig(t *testing.T) (*Config, error) {
	t.Helper()
	raw, err := os.ReadFile("../../../config.example.yaml")
	if err != nil {
		return nil, err
	}
	res, err := ParseLayers("example", raw)
	if err != nil {
		return nil, err
	}
	return res.Config, nil
}

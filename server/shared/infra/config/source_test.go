// Unit tests for config sources and layer stacking. All offline: no real files are read (except
// temp files in t.TempDir()), and no external service is called.
//
// The three things pinned here are all of the "wrong without an error" kind:
//
//  1. **Stacking two YAML files merges instead of replacing whole sections.** Secrets can live in
//     a separate YAML stacked on top of the plaintext one. If yaml.Decode "replaced whole
//     sections", a second file with only pg.password would wipe pg.host to empty; the service
//     would connect to an empty address, and the error would not point at "config was wiped".
//  2. **Environment variables still have the highest priority**, and the overridden field names
//     must be reported truthfully. The latter is the only visible clue for the pitfall "I changed
//     it in the config file but it did not take effect".
//  3. **How the config file path is chosen**: only AMA_CONFIG counts; empty falls back to
//     config.yaml.

package config

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Layer stacking
// ---------------------------------------------------------------------------

// TestParseLayersMergesInsteadOfReplacing: a later layer only overrides the fields it mentions.
//
// **This is the foundation of the "secrets in a separate encrypted config" design.** It pins the
// behavior of yaml.Decode into an already filled struct: fields not mentioned keep their values.
// This is not behavior we implement but behavior of a dependency, which is all the more reason to
// pin it: if a yaml library upgrade ever changes it, this goes red, instead of us finding out when
// the production database cannot be reached.
func TestParseLayersMergesInsteadOfReplacing(t *testing.T) {
	main := []byte(`
pg:
  host: 10.0.0.1
  port: 5432
  database: ama
  user: ama
  sslmode: require
docker:
  image: ama-sandbox:test
  network: none
`)
	// The encrypted one: only secrets, no other field mentioned.
	secrets := []byte(`
pg:
  password: s3cret
ark:
  secretKey: ark_sk_x
`)

	res, err := ParseLayers("test", main, secrets)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	cfg := res.Config

	// Not a single field of the plaintext one may be wiped.
	for _, tc := range []struct{ name, got, want string }{
		{"pg.host", cfg.PG.Host, "10.0.0.1"},
		{"pg.database", cfg.PG.Database, "ama"},
		{"pg.user", cfg.PG.User, "ama"},
		{"pg.sslmode", cfg.PG.SSLMode, "require"},
		{"docker.image", cfg.Docker.Image, "ama-sandbox:test"},
		{"docker.network", cfg.Docker.Network, "none"},
		// Fields of the secrets one must take effect.
		{"pg.password", cfg.PG.Password, "s3cret"},
		{"ark.secretKey", cfg.Ark.SecretKey, "ark_sk_x"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s 期望 %q，实际 %q", tc.name, tc.want, tc.got)
		}
	}
	if cfg.PG.Port != 5432 {
		t.Errorf("pg.port 期望 5432，实际 %d", cfg.PG.Port)
	}
}

// TestParseLayersLaterWins: when two layers set the same field, the later layer wins.
func TestParseLayersLaterWins(t *testing.T) {
	res, err := ParseLayers("test",
		[]byte("pg:\n  host: h1\n  database: ama\n  user: u\n  password: placeholder\n"),
		[]byte("pg:\n  password: real\n"))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if res.Config.PG.Password != "real" {
		t.Errorf("后一层该赢，password 实际 %q", res.Config.PG.Password)
	}
}

// TestParseLayersSkipsEmptyLayers: empty layers are skipped and do not affect existing values.
//
// A config that exists but is empty is a valid intermediate state (for example secrets not filled
// in yet); it should not keep the service from starting, let alone wipe the plaintext config
// already read.
func TestParseLayersSkipsEmptyLayers(t *testing.T) {
	for _, empty := range []string{"", "   \n  ", "# 只有注释\n"} {
		res, err := ParseLayers("test", []byte(minimalYAML), []byte(empty))
		if err != nil {
			t.Fatalf("空层 %q 不该报错: %v", empty, err)
		}
		if res.Config.PG.Host != "127.0.0.1" {
			t.Errorf("空层 %q 把 host 抹掉了: %q", empty, res.Config.PG.Host)
		}
	}
}

// TestParseLayersAllEmpty: when everything is empty, report "config is empty" instead of
// connecting to localhost with a pile of zero values.
func TestParseLayersAllEmpty(t *testing.T) {
	_, err := ParseLayers("test", []byte(""), []byte("# 注释\n"))
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("期望 ErrInvalidConfig，实际 %v", err)
	}
}

// TestParseLayersReportsUnknownFieldInSecrets: a misspelled key in the encrypted one must also be
// recorded.
//
// The secrets file is the hardest to verify: once encrypted you cannot see the plaintext, so "a
// key is misspelled" is almost impossible to spot by eye. Detection must work for every layer;
// unknown fields are no longer fatal (see the notes on ParseLayers), but they must appear in
// UnknownFields, otherwise a typo in the encrypted layer would truly go unnoticed.
func TestParseLayersReportsUnknownFieldInSecrets(t *testing.T) {
	res, err := ParseLayers("test", []byte(minimalYAML), []byte("ark:\n  secretKeyy: x\n"))
	if err != nil {
		t.Fatalf("未知字段不该让解析失败，实际: %v", err)
	}
	if len(res.UnknownFields) != 1 || !strings.Contains(res.UnknownFields[0], "secretKeyy") {
		t.Errorf("加密层里拼错的键名应当被记账并点出键名，实际: %v", res.UnknownFields)
	}
}

// ---------------------------------------------------------------------------
// Environment variable overrides and source reporting
// ---------------------------------------------------------------------------

// TestResultReportsEnvOverridden: field names overridden by environment variables must be reported
// truthfully.
//
// **This is the only visible clue for "I changed it in the config file but it did not take
// effect".** cmd/amad prints it in the startup log, and ops use it to clean up extra environment
// variables. Every field missing from the report is a field that "does not react to changes" with
// no way to find out why.
func TestResultReportsEnvOverridden(t *testing.T) {
	t.Setenv(EnvPGHost, "10.9.9.9")
	t.Setenv(EnvPGPassword, "from-env")
	t.Setenv(EnvDockerImage, "image-from-env")

	res, err := ParseLayers("test", []byte(minimalYAML))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	for _, want := range []string{"pg.host", "pg.password", "docker.image"} {
		if !slices.Contains(res.EnvOverridden, want) {
			t.Errorf("应报出被覆盖的字段 %q，实际 %v", want, res.EnvOverridden)
		}
	}
	// Fields that were not set must not be falsely reported, otherwise that log line sends people
	// to the wrong place.
	if slices.Contains(res.EnvOverridden, "pg.user") {
		t.Errorf("没设的字段不该出现在覆盖列表里: %v", res.EnvOverridden)
	}
}

// TestEnvOverridesNewSections: the newly added sections must also be overridable by environment
// variables.
//
// Missing one field shows up as "I set the environment variable but it has no effect", the kind
// of silent failure this package keeps guarding against.
func TestEnvOverridesNewSections(t *testing.T) {
	t.Setenv(EnvAddr, ":9999")
	t.Setenv(EnvAllowedOrigin, "http://example.com")
	t.Setenv(EnvStore, StoreMem)
	t.Setenv(EnvAPIKey, "k")
	t.Setenv(EnvAPIKeyEmail, "e@example.com")
	t.Setenv(EnvArkAPIKey, "ark-key")
	t.Setenv(EnvDockerHost, "tcp://127.0.0.1:2375")
	t.Setenv(EnvDockerIdleAction, "stop")
	t.Setenv(EnvDockerWorkdir, "/w")
	t.Setenv(EnvAuthOIDCIssuer, "https://idp.example.com")
	t.Setenv(EnvAuthOIDCClientID, "ama")

	res, err := ParseLayers("test", []byte(minimalYAML))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	cfg := res.Config
	for _, tc := range []struct{ name, got, want string }{
		{"server.addr", cfg.Server.Addr, ":9999"},
		{"server.allowedOrigin", cfg.Server.AllowedOrigin, "http://example.com"},
		{"server.store", cfg.Server.Store, StoreMem},
		{"apiKey.key", cfg.APIKey.Key, "k"},
		{"apiKey.email", cfg.APIKey.Email, "e@example.com"},
		{"ark.apiKey", cfg.Ark.APIKey, "ark-key"},
		{"docker.host", cfg.Docker.Host, "tcp://127.0.0.1:2375"},
		{"docker.idleAction", cfg.Docker.IdleAction, "stop"},
		{"docker.workdir", cfg.Docker.Workdir, "/w"},
		{"auth.oidc.issuer", cfg.Auth.OIDC.Issuer, "https://idp.example.com"},
		{"auth.oidc.clientId", cfg.Auth.OIDC.ClientID, "ama"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s 期望被环境变量覆盖成 %q，实际 %q", tc.name, tc.want, tc.got)
		}
	}
}

// TestDevRoutesOnlyAcceptsOne: AMA_DEV_ROUTES only accepts "1".
//
// This is on purpose: it mounts /api/_debug/reset, where one request can wipe the whole database.
// A switch of that weight should not have several spellings; treating "true" as on would be
// dangerous, so any ambiguous value counts as off.
func TestDevRoutesOnlyAcceptsOne(t *testing.T) {
	for _, tc := range []struct {
		val  string
		want bool
	}{
		{"1", true},
		{"true", false},
		{"yes", false},
		{"0", false},
	} {
		t.Run(tc.val, func(t *testing.T) {
			t.Setenv(EnvDevRoutes, tc.val)
			res, err := ParseLayers("test", []byte(minimalYAML))
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if res.Config.Server.DevRoutes != tc.want {
				t.Errorf("AMA_DEV_ROUTES=%q 期望 %v，实际 %v", tc.val, tc.want, res.Config.Server.DevRoutes)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

// TestValidateRejectsBadStore: a misspelled store must be an error, not silently run as pg.
//
// Silent fallback shows up as "I clearly set mem, why is it still connecting to the database",
// and the error says nothing.
func TestValidateRejectsBadStore(t *testing.T) {
	for _, bad := range []string{"memory", "PG", "postgres"} {
		_, err := ParseLayers("test", []byte(minimalYAML), []byte("server:\n  store: "+bad+"\n"))
		if !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("store=%q 期望 ErrInvalidConfig，实际 %v", bad, err)
		}
	}
}

// TestValidateRejectsWildcardOrigin: allowedOrigin cannot be * (cookies are sent, and browsers
// reject it).
func TestValidateRejectsWildcardOrigin(t *testing.T) {
	_, err := ParseLayers("test", []byte(minimalYAML), []byte("server:\n  allowedOrigin: '*'\n"))
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("期望 ErrInvalidConfig，实际 %v", err)
	}
}

// ---------------------------------------------------------------------------
// Defaults and checks
// ---------------------------------------------------------------------------

func TestServerDefaults(t *testing.T) {
	var s Server
	// The default port must match the port agreed with the deployment side (docker-compose /
	// health check probes). If they differ, the probe knocks on the agreed port while the process
	// listens on another, and the instance is marked dead over and over.
	if got := s.AddrOrDefault(); got != "0.0.0.0:9999" {
		t.Errorf("addr 默认该是 0.0.0.0:9999，实际 %q", got)
	}
	// **The default must be pg**: the in-memory Store looks perfectly fine until a restart shows
	// all data is gone.
	if got := s.StoreOrDefault(); got != StorePG {
		t.Errorf("store 默认该是 pg，实际 %q", got)
	}
	if s.UseMemStore() {
		t.Error("没配 store 时不该走内存")
	}
	if (Server{Store: StoreMem}).UseMemStore() != true {
		t.Error("store=mem 该走内存")
	}
}

// TestSecureCookieFollowsPublicURL: the cookie's Secure follows the scheme of publicURL.
//
// Locally it is http://, and with Secure the browser does not store the cookie at all; it shows
// up as "login succeeds, but a refresh asks to log in again", with no error at all.
func TestSecureCookieFollowsPublicURL(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"https://ama.example.com", true},
		{"http://localhost:5273", false},
		{"", false},
	} {
		if got := (Server{PublicURL: tc.url}).SecureCookie(); got != tc.want {
			t.Errorf("publicURL=%q 期望 SecureCookie=%v，实际 %v", tc.url, tc.want, got)
		}
	}
}

// TestEnabledHelpers: the checks of the "enabled only when fully configured" kind.
//
// All of them need **both fields non-empty**: setting only half has different but equally hidden
// consequences: half an APIKey means the channel does not work, half an OIDC means the login page
// silently misses a button.
func TestEnabledHelpers(t *testing.T) {
	if (APIKey{Key: "k"}).Enabled() || (APIKey{Email: "e"}).Enabled() {
		t.Error("APIKey 只配一半不该算启用")
	}
	if !(APIKey{Key: "k", Email: "e"}).Enabled() {
		t.Error("APIKey 两个都配了该算启用")
	}
	off := false
	if !(Docker{}).DockerEnabled() {
		t.Error("Docker 执行端没配时默认启用")
	}
	if (Docker{Enabled: &off}).DockerEnabled() {
		t.Error("Docker 显式 enabled=false 时不该启用")
	}
	if (AuthOIDC{Issuer: "i"}).Enabled() || (AuthOIDC{ClientID: "c"}).Enabled() {
		t.Error("OIDC 只配一半不该算启用")
	}
	if !(AuthOIDC{Issuer: "i", ClientID: "c"}).Enabled() {
		t.Error("OIDC 两个都配了该算启用")
	}
}

// Without AMA_CONFIG it reads config.yaml; with it, it reads that path (surrounding whitespace
// trimmed).
func TestSourcesFromEnvPath(t *testing.T) {
	t.Setenv(EnvConfigPath, "")
	if got := SourcesFromEnv().Path; got != DefaultConfigPath {
		t.Errorf("默认路径该是 %s，实际 %q", DefaultConfigPath, got)
	}
	t.Setenv(EnvConfigPath, "  /etc/ama/amad.yaml ")
	if got := SourcesFromEnv().Path; got != "/etc/ama/amad.yaml" {
		t.Errorf("该读 AMA_CONFIG 的值，实际 %q", got)
	}
}

// All auth.github environment variables are wired; lists are comma/space separated and replace
// the whole list.
func TestEnvOverridesGitHub(t *testing.T) {
	t.Setenv(EnvAuthGitHubEnabled, "true")
	t.Setenv(EnvAuthGitHubClientID, "cid")
	t.Setenv(EnvAuthGitHubScopes, "read:user, user:email read:org")
	t.Setenv(EnvAuthGitHubDisplayName, "GH")
	t.Setenv(EnvAuthGitHubProxy, "http://127.0.0.1:8080")
	t.Setenv(EnvAuthGitHubAllowedUsers, "alice,bob")
	t.Setenv(EnvAuthGitHubAllowedEmailDomains, "example.com")
	t.Setenv(EnvAuthGitHubAllowedOrgs, "acme")
	res, err := ParseLayers("test", []byte(minimalYAML))
	if err != nil {
		t.Fatal(err)
	}
	g := res.Config.Auth.GitHub
	if !g.Active() || g.ClientID != "cid" || g.DisplayName != "GH" || g.Proxy != "http://127.0.0.1:8080" {
		t.Errorf("标量没接上: %+v", g)
	}
	if strings.Join(g.Scopes, "|") != "read:user|user:email|read:org" ||
		strings.Join(g.AllowedUsers, "|") != "alice|bob" ||
		strings.Join(g.AllowedEmailDomains, "|") != "example.com" ||
		strings.Join(g.AllowedOrgs, "|") != "acme" || !g.HasAllowlist() {
		t.Errorf("列表没接上: %+v", g)
	}
}

// A wrong proxy is an error at load time, not a silent failure after startup; the client id may
// be left unset.
func TestGitHubConfigValidation(t *testing.T) {
	t.Run("不配 clientId 也算启用（用默认值）", func(t *testing.T) {
		t.Setenv(EnvAuthGitHubEnabled, "true")
		res, err := ParseLayers("test", []byte(minimalYAML))
		if err != nil || !res.Config.Auth.GitHub.Active() {
			t.Errorf("enabled 但没 clientId 应正常启用: %v", err)
		}
	})
	t.Run("代理没写 scheme", func(t *testing.T) {
		t.Setenv(EnvAuthGitHubProxy, "127.0.0.1:8080")
		if _, err := ParseLayers("test", []byte(minimalYAML)); err == nil {
			t.Error("代理不合法应报错")
		}
	})
	t.Run("没开时不算启用", func(t *testing.T) {
		if (AuthGitHub{ClientID: "x"}).Active() {
			t.Error("enabled=false 不该算启用")
		}
	})
}

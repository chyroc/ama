// Entry point of the PG Store integration tests.
//
// There are **no assertions** here; they are all in store_test.go, and TestStoreSuite runs them once each against the in-memory
// and PG implementations. This file only handles "how to get a clean PGStore" and how to skip gracefully when the database
// is unreachable. It copies the approach of core/store_pg_test.go; the file header there has more detailed reasons.
//
// # Running offline is a hard requirement
//
// `go test ./...` must stay green for people who have not started Docker. So when the database is unreachable the whole group calls t.Skip, not
// t.Fatal. On the other hand, **skipping must not be silent**: the reason includes the connection parameters and the docker compose
// command, so nobody thinks the PG half has run.
//
// # Do not touch real data
//
// Tests are built in a separate schema (ama_test_auth_<random suffix>), pointed to through the DSN's search_path.
// Table names match production but live in another namespace; after the whole group runs, drop schema cascade cleans up.

package authdao

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/shared/infra/pg"
)

// testSchema is the schema name used only by integration tests, **randomly generated on every process start**.
// It contains "test", so if it ever runs against a production database it is obvious at a glance.
//
// ⚠️ The suffix must be random, for the same reason as the coredao version: with concurrent sessions a fixed name gets
// `drop schema ... cascade`-ed by another process, giving SQLSTATE 3F000, with a different subtest failing each time.
var testSchema = "ama_test_auth_" + randomSchemaSuffix()

// randomSchemaSuffix generates a 16-character lowercase hex suffix (crypto/rand, to avoid seed clashes when starting in the same second).
func randomSchemaSuffix() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		panic("生成测试 schema 后缀失败: " + err.Error())
	}
	return hex.EncodeToString(buf)
}

type pgTestEnv struct {
	client *pg.Client
	// A non-empty skip means this environment is unusable; the value is the reason passed to t.Skip.
	skip string
}

var (
	pgEnvOnce sync.Once
	pgEnv     pgTestEnv
)

// pgTestConfig is the parameters for connecting to the local dev database; environment variables can override them.
// Defaults match docker-compose.yml at the repo root (PG 17, port 15432, database/user/password all ama).
func pgTestConfig() pg.StdConfig {
	port := 15432
	if v := os.Getenv("AMA_TEST_PG_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			port = n
		}
	}
	env := func(key, def string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return def
	}
	return pg.StdConfig{
		Host:           env("AMA_TEST_PG_HOST", "127.0.0.1"),
		Port:           port,
		Database:       env("AMA_TEST_PG_DATABASE", "ama"),
		User:           env("AMA_TEST_PG_USER", "ama"),
		Password:       env("AMA_TEST_PG_PASSWORD", "ama"),
		SSLMode:        "disable",
		ConnectTimeout: 3 * time.Second,
		ExtraParams: map[string]string{
			"options": "-c search_path=" + testSchema,
		},
	}
}

func initPGEnv() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cfg := pgTestConfig()

	bootstrapCfg := cfg
	bootstrapCfg.ExtraParams = nil
	bootstrap, err := pg.NewStandard(ctx, bootstrapCfg)
	if err != nil {
		pgEnv.skip = "连不上本地 PostgreSQL（" + bootstrapCfg.Redacted() + "）：" +
			err.Error() + "\n起库：在仓库根跑 `docker compose up -d`，等 ps 显示 healthy"
		return
	}
	defer func() { _ = bootstrap.Close() }()

	if _, err := bootstrap.Exec(ctx, `create schema if not exists `+testSchema); err != nil {
		pgEnv.skip = "建测试 schema 失败：" + err.Error()
		return
	}

	client, err := pg.NewStandard(ctx, cfg)
	if err != nil {
		pgEnv.skip = "连测试 schema 失败：" + err.Error()
		return
	}
	pgEnv.client = client
}

// pgFactory is the factory for the PG implementation, same signature as memFactory, called by TestStoreSuite.
//
// Each call first clears both tables, so subtests do not affect each other, matching the in-memory version creating a new object each time.
func pgFactory(t *testing.T) auth.Store {
	t.Helper()
	pgEnvOnce.Do(initPGEnv)
	if pgEnv.skip != "" {
		t.Skip(pgEnv.skip)
	}

	ctx := context.Background()
	s, err := NewPGStore(ctx, pgEnv.client)
	if err != nil {
		t.Fatalf("建 PGStore 失败：%v", err)
	}
	// ⚠️ **Remember to add new tables here.** Missing one is very misleading: rows with the same primary key survive from one
	// subtest to the next, so the "round trip" test writes state-1 and consumes it, then the "consume only once" test
	// writes the same id, hits on conflict (which does not reset used_at), and the very first consume fails.
	// The error says "not found", which looks like wrong SQL, but it is really stale data.
	for _, table := range []string{"auth_sessions", "auth_oidc_states", "auth_github_flows", "users", "auth_local_accounts", "auth_login_failures"} {
		if _, err := pgEnv.client.Exec(ctx, `truncate table `+table); err != nil {
			t.Fatalf("清空 %s 失败：%v", table, err)
		}
	}
	return s
}

// NewPGStore must reject a nil client: that is a wiring error and should not wait until the first query to blow up.
func TestNewPGStoreRejectsNilClient(t *testing.T) {
	if _, err := NewPGStore(context.Background(), nil); err == nil {
		t.Error("nil client 应该被拒")
	}
}

// The DDL script must be safe to run repeatedly: the service runs it on every start, and during a rolling deploy two instances run it at once.
func TestPGMigrateIsIdempotent(t *testing.T) {
	pgEnvOnce.Do(initPGEnv)
	if pgEnv.skip != "" {
		t.Skip(pgEnv.skip)
	}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := NewPGStore(ctx, pgEnv.client); err != nil {
			t.Fatalf("第 %d 次建表失败（脚本必须可重复执行）：%v", i+1, err)
		}
	}
}

// TestMain drops the test schema after the whole package's tests finish.
func TestMain(m *testing.M) {
	code := m.Run()

	if pgEnv.client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if _, err := pgEnv.client.Exec(ctx, `drop schema if exists `+testSchema+` cascade`); err != nil {
			println("清理测试 schema 失败（请手工 drop schema " + testSchema + " cascade）：" + err.Error())
		}
		cancel()
		_ = pgEnv.client.Close()
	}
	os.Exit(code)
}

// Entry point of the PG Store integration tests.
//
// There are **no assertions** here; they are all in store_test.go, and TestStoreSuite runs them once each against the in-memory
// and PG implementations. This file only handles "how to get a clean PGStore" and how to skip gracefully when the database
// is unreachable.
//
// # Running offline is a hard requirement
//
// `go test ./...` must stay green for people who have not started Docker. So when the database is unreachable the whole group calls t.Skip, not
// t.Fatal: CI and other people's machines may not have that container.
//
// On the other hand, **skipping must not be silent**: the skip reason includes the connection parameters and the docker compose command,
// so nobody thinks the PG half has run.
//
// # Do not touch real data
//
// Tests are built in a **separate schema** (ama_test_core_<random suffix>), pointed to through the DSN's search_path. Table names
// are exactly the same as production but live in another namespace, so neither TRUNCATE nor table creation touches the real data
// under public. After the whole group runs, drop schema cascade cleans up.
//
// A schema rather than "a separate database": creating a database needs superuser rights, which managed PG often does not give.
// A schema can be created with a normal account, so local and production take the same path.

package coredao

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/chyroc/ama/server/core"
	"github.com/chyroc/ama/server/shared/infra/pg"
)

// testSchema is the schema name used only by integration tests, **randomly generated on every process start**.
//
// It contains "test" on purpose: if someone ever runs the tests against a production database, `\dn` shows at a glance what it is,
// and that it is safe to delete.
//
// ⚠️ The suffix must be random, not a fixed "ama_test_core". This workspace has concurrent sessions; when two
// processes run this package's PG tests at once, a fixed name lets whichever finishes first
// `drop schema ... cascade` and wipe out the other's tables while it is still running. The symptoms are
// `ERROR: no schema has been selected to create in (SQLSTATE 3F000)` and
// `relation "xxx" does not exist`, **with a different subtest failing each time**, which looks flaky
// or like your own change broke something. The feishu version was always random, so it never hit this.
var testSchema = "ama_test_core_" + randomSchemaSuffix()

// randomSchemaSuffix generates a 16-character lowercase hex suffix.
//
// crypto/rand instead of math/rand: without a seed, two processes starting in the same second could get
// the same sequence with the latter, which is no randomness at all.
func randomSchemaSuffix() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		panic("生成测试 schema 后缀失败: " + err.Error())
	}
	return hex.EncodeToString(buf)
}

// pgTestEnv is the single database connection shared by the whole package.
//
// The connection and schema are created only once (sync.Once), and every later subtest reuses the same pool. If each test
// called NewStandard itself, 17 tests would mean 17 handshakes, slow and pointless.
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
//
// Defaults match docker-compose.yml at the repo root (PG 17, port 15432, database/user/password all ama).
// To point at another instance set AMA_TEST_PG_*, but note this test creates a schema and loads seed data in the target database,
// so never point it at a database with real data.
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
		Host:     env("AMA_TEST_PG_HOST", "127.0.0.1"),
		Port:     port,
		Database: env("AMA_TEST_PG_DATABASE", "ama"),
		User:     env("AMA_TEST_PG_USER", "ama"),
		Password: env("AMA_TEST_PG_PASSWORD", "ama"),
		SSLMode:  "disable",
		// When unreachable, do not hang until the system TCP timeout: the test should skip quickly, not get stuck there.
		ConnectTimeout: 3 * time.Second,
		ExtraParams: map[string]string{
			// Point this connection's default schema at the test schema. Table creation, TRUNCATE and queries write no
			// schema prefix and rely entirely on this to land in the right namespace.
			//
			// Note the options value contains spaces; it gets quoted when the DSN is assembled (quoteDSNValue
			// does that), so the raw text can be written here directly.
			"options": "-c search_path=" + testSchema,
		},
	}
}

// initPGEnv creates the connection and the schema. Runs only once.
func initPGEnv() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cfg := pgTestConfig()

	// Creating the schema uses a connection **without search_path**: while the schema does not exist yet,
	// a connection with `-c search_path=<nonexistent schema>` can still be opened (PostgreSQL does not check whether
	// the schemas in search_path exist), but to have one less variable this is done in two steps.
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
// Each call **clears first and then loads seed data as needed**, so subtests do not affect each other, matching the in-memory version creating
// a new object each time.
func pgFactory(t *testing.T, withSeed bool) core.Store {
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
	// The previous test may have left data; clean it first.
	if withSeed {
		if err := s.Reset(ctx); err != nil {
			t.Fatalf("装载种子失败：%v", err)
		}
	} else if err := s.Clear(ctx); err != nil {
		t.Fatalf("清空失败：%v", err)
	}
	return s
}

// TestMain drops the test schema after the whole package's tests finish.
//
// Without cleanup, the next run hits the leftovers of the previous round (with odd errors if the table structure changed),
// and the dev database keeps an unknown schema lying around forever.
func TestMain(m *testing.M) {
	code := m.Run()

	if pgEnv.client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		// A failed drop does not change the test result: the tests are done, and a cleanup failure is an ops problem, not a code problem.
		// But say so, otherwise a schema quietly stays in the database and nobody knows.
		if _, err := pgEnv.client.Exec(ctx, `drop schema if exists `+testSchema+` cascade`); err != nil {
			println("清理测试 schema 失败（请手工 drop schema " + testSchema + " cascade）：" + err.Error())
		}
		cancel()
		_ = pgEnv.client.Close()
	}
	os.Exit(code)
}

// TestPGStoreSeededOnlyWhenEmpty: NewSeededPGStore loads seed data only into an empty database.
//
// **This is the whole point of persistence**: a service restart must not wipe user data. The in-memory implementation has no such problem
// (it has no previous data anyway), so this test only makes sense for PG and is not in the shared suite.
func TestPGStoreSeededOnlyWhenEmpty(t *testing.T) {
	pgEnvOnce.Do(initPGEnv)
	if pgEnv.skip != "" {
		t.Skip(pgEnv.skip)
	}
	ctx := context.Background()

	// Empty database: the seed should be loaded
	s, err := NewPGStore(ctx, pgEnv.client)
	if err != nil {
		t.Fatalf("建 PGStore 失败：%v", err)
	}
	if err := s.Clear(ctx); err != nil {
		t.Fatalf("清空失败：%v", err)
	}

	s, err = NewSeededPGStore(ctx, pgEnv.client)
	if err != nil {
		t.Fatalf("建 SeededPGStore 失败：%v", err)
	}
	ws, _ := s.ListWorkspaces(ctx)
	if len(ws) != 1 {
		t.Fatalf("空库起来应该装上种子，实际 %d 个空间", len(ws))
	}

	// Change some data, simulating "the user has used it for a while"
	a, err := s.GetAgent(ctx, "agent_triage")
	if err != nil {
		t.Fatalf("取 agent 失败：%v", err)
	}
	a.Name = "用户改过的名字"
	if _, err := s.SaveAgent(ctx, a); err != nil {
		t.Fatalf("存 agent 失败：%v", err)
	}

	// Start again (= service restart): the change **must not** be wiped
	s2, err := NewSeededPGStore(ctx, pgEnv.client)
	if err != nil {
		t.Fatalf("第二次建 SeededPGStore 失败：%v", err)
	}
	back, err := s2.GetAgent(ctx, "agent_triage")
	if err != nil {
		t.Fatalf("重启后取 agent 失败：%v", err)
	}
	if back.Name != "用户改过的名字" {
		t.Errorf("重启把用户数据冲掉了：期望 %q 实际 %q", "用户改过的名字", back.Name)
	}
}

// TestPGStoreMigrateIsIdempotent: the DDL script can be run repeatedly.
//
// The service runs migrate on every start; if it were not idempotent, the second start would fail.
func TestPGStoreMigrateIsIdempotent(t *testing.T) {
	pgEnvOnce.Do(initPGEnv)
	if pgEnv.skip != "" {
		t.Skip(pgEnv.skip)
	}
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := NewPGStore(ctx, pgEnv.client); err != nil {
			t.Fatalf("第 %d 次建表失败：%v", i+1, err)
		}
	}
}

// TestLegacyRowGetsUpdatedAt pins "old rows whose doc has no updatedAt can still read this field".
//
// # This one came from an end-to-end run; the unit tests were all green at the time
//
// updatedAt was a newly added field, and **the doc of existing rows had no such key at all**. If the read path only trusted
// doc, old data would read as an empty string, and the frontend would get `"updatedAt": ""`: no error, just a "last updated"
// column that is always empty. The unit test seed was newly written with the key in doc, so it could not catch it;
// it was only found by starting the real service locally and hitting /api/workspaces, where all four workspaces had empty updatedAt.
//
// The migration had already backfilled the columns, so the fix was to trust the columns when reading (fillTimes).
func TestLegacyRowGetsUpdatedAt(t *testing.T) {
	pgEnvOnce.Do(initPGEnv)
	if pgEnv.skip != "" {
		t.Skip(pgEnv.skip)
	}
	ctx := context.Background()

	s, err := NewPGStore(ctx, pgEnv.client)
	if err != nil {
		t.Fatalf("建 PGStore 失败：%v", err)
	}
	if err := s.Clear(ctx); err != nil {
		t.Fatalf("清库失败：%v", err)
	}

	// Old shape: the two timestamps only have values in the columns.
	//
	// ⚠️ This used to also insert a doc without createdAt / updatedAt, to express "not in doc,
	// can only be filled from the columns". The doc column has since been dropped (step five of splitting doc), and the read path now **only looks at columns**,
	// so the premise of this test is stronger than before: the values can only come from the columns. The assertions are unchanged.
	if _, err := pgEnv.client.Exec(ctx,
		`insert into workspaces (id, key, name, description, created_at, updated_at)
		 values ('ws_legacy', 'legacy', '老空间', '',
		         '2026-01-01T00:00:00Z'::timestamptz, '2026-02-02T03:04:05.678Z'::timestamptz)`,
	); err != nil {
		t.Fatalf("造老行失败：%v", err)
	}

	got, err := s.GetWorkspace(ctx, "ws_legacy")
	if err != nil {
		t.Fatalf("取老空间失败：%v", err)
	}
	if got.CreatedAt != "2026-01-01T00:00:00.000Z" {
		t.Errorf("createdAt 没从列里补上：%q", got.CreatedAt)
	}
	if got.UpdatedAt != "2026-02-02T03:04:05.678Z" {
		t.Errorf("updatedAt 没从列里补上：%q", got.UpdatedAt)
	}

	// ListWorkspaces is another query and must fill them too.
	all, err := s.ListWorkspaces(ctx)
	if err != nil {
		t.Fatalf("列空间失败：%v", err)
	}
	if len(all) != 1 {
		t.Fatalf("应有 1 个空间，实际 %d", len(all))
	}
	if all[0].UpdatedAt != "2026-02-02T03:04:05.678Z" {
		t.Errorf("ListWorkspaces 没补 updatedAt：%q", all[0].UpdatedAt)
	}

	if err := s.Clear(ctx); err != nil {
		t.Fatalf("收尾清库失败：%v", err)
	}
}

// TestPGStoreRejectsNilClient: the constructor must reject a nil client.
//
// Runs offline: it never touches the database.
func TestPGStoreRejectsNilClient(t *testing.T) {
	if _, err := NewPGStore(context.Background(), nil); err == nil {
		t.Error("nil client 应该报错")
	}
}

// TestPGStoreTablesLandInTestSchema confirms the tests really land in the separate schema.
//
// This is a **safety net**: if search_path did not take effect, tables would be created under public, and the TRUNCATE in the suite
// would wipe real data. Better red here than silently deleting data in someone else's dev database.
func TestPGStoreTablesLandInTestSchema(t *testing.T) {
	pgEnvOnce.Do(initPGEnv)
	if pgEnv.skip != "" {
		t.Skip(pgEnv.skip)
	}
	ctx := context.Background()

	if _, err := NewPGStore(ctx, pgEnv.client); err != nil {
		t.Fatalf("建表失败：%v", err)
	}
	for _, table := range pgTables {
		var schema string
		err := pgEnv.client.QueryRow(ctx,
			`select n.nspname from pg_class c
			 join pg_namespace n on n.oid = c.relnamespace
			 where c.relname = $1 and n.nspname = $2`,
			table, testSchema).Scan(&schema)
		if err != nil {
			if pg.IsNoRows(err) {
				t.Fatalf("表 %s 没建在 %s 里——search_path 没生效，"+
					"套件里的 TRUNCATE 有清掉真实数据的风险", table, testSchema)
			}
			t.Fatalf("查表所在 schema 失败：%v", err)
		}
	}
}

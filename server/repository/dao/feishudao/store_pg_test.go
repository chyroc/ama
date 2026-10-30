// Tests for PGStore.
//
// # Safe offline
//
// `go test ./...` must still run green on machines without a database, so when unreachable this calls t.Skip,
// not t.Fatal. The check is done only once (sync.Once); otherwise every test would wait for
// connect_timeout on one failed attempt.
//
// Start the database locally:
//
//	docker compose up -d           # repo root, PG 17, port 15432
//	go test -race ./feishu/
//
// Use AMA_TEST_PG_DSN to switch to another database, in the form
// `host=127.0.0.1 port=15432 dbname=ama user=ama password=ama sslmode=disable`.
//
// # One separate schema per test
//
// Tests must not interfere with each other, and the app_id of feishu_apps is a globally unique key: two tests both using
// cli_x would collide. So each test creates a randomly named schema, points search_path at it,
// and drop cascades it at the end. **This also never touches the tables in the developer's own database**: running tests does not wipe
// data you loaded by hand.
//
// search_path is connection-level session state, so each test opens its own *sql.DB (the pool is limited to 1 connection,
// see the comment on newPGStore) and shares it with no other test.
//
// # Where the assertions are
//
// The end-to-end ones (idempotent registration, session ownership, secrets not leaking, list isolation...) **are not in this file**:
// they are in feishu_test.go and run on both the in-memory and PG implementations through eachStore. There is only one copy of the assertions,
// otherwise the two implementations would sooner or later each be green on their own.
//
// This file only holds PG-specific things: connections and table creation, behavior at the SQL level (unique constraints, NULL secrets,
// ordering, expiry cleanup).

package feishudao

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/chyroc/ama/server/feishu"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chyroc/ama/server/shared/infra/pg"
)

// defaultTestDSN points at the local PG started by docker-compose (see docker-compose.yml at the repo root).
// The password is a throwaway password for the local dev database, not a credential.
const defaultTestDSN = "host=127.0.0.1 port=15432 dbname=ama user=ama password=ama sslmode=disable connect_timeout=3"

// EnvTestPGDSN overrides the DSN used by tests.
const EnvTestPGDSN = "AMA_TEST_PG_DSN"

var (
	pgProbeOnce sync.Once
	// pgProbeErr is the probe result: nil means reachable, non-nil is the reason to skip.
	pgProbeErr error
)

func testPGDSN() string {
	if v := strings.TrimSpace(os.Getenv(EnvTestPGDSN)); v != "" {
		return v
	}
	return defaultTestDSN
}

// requirePG confirms this machine has a usable PG, and skips the whole test if not.
//
// The probe runs only once and caches the result: when unreachable each attempt waits for connect_timeout, and over dozens of tests
// that would make one test run on "a machine without a database" very slow.
func requirePG(t *testing.T) {
	t.Helper()
	pgProbeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		db, err := sql.Open("pgx", testPGDSN())
		if err != nil {
			pgProbeErr = err
			return
		}
		defer func() { _ = db.Close() }()
		pgProbeErr = db.PingContext(ctx)
	})
	if pgProbeErr != nil {
		t.Skipf("跳过：连不上本地 PostgreSQL（%s=%q）。起库：docker compose up -d。原因: %v",
			EnvTestPGDSN, testPGDSN(), pgProbeErr)
	}
}

// newPGStore builds a PGStore running in a separate schema, and drops the whole schema when the test ends.
//
// The pool keeps only 1 connection: search_path is **connection-level** session state; if the pool had a second connection,
// it would not carry the search_path we set, so two queries of the same Store could land in different schemas,
// showing as "a row just written cannot be found", now and then. Writing search_path into the DSN with ExtraParams
// also works (pgx sends non-libpq keys as startup parameters, see pitfall two in shared/infra/pg), but that
// bypasses StdConfig's key-name allowlist; limiting to 1 connection is more direct.
func newPGStore(t *testing.T) *PGStore {
	t.Helper()
	requirePG(t)
	ctx := context.Background()

	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("生成 schema 名失败: %v", err)
	}
	schema := "feishu_test_" + hex.EncodeToString(buf)

	db, err := sql.Open("pgx", testPGDSN())
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.ExecContext(ctx, `create schema `+schema); err != nil {
		t.Fatalf("建 schema 失败: %v", err)
	}
	t.Cleanup(func() {
		// Drop with a new connection: at this point db's connection still has search_path pointing at this schema,
		// and it may already have been closed depending on the order of t.Cleanup.
		cleanupDB, err := sql.Open("pgx", testPGDSN())
		if err != nil {
			t.Logf("清理 schema %s 失败（打开连接）: %v", schema, err)
			return
		}
		defer func() { _ = cleanupDB.Close() }()
		if _, err := cleanupDB.ExecContext(context.Background(), `drop schema if exists `+schema+` cascade`); err != nil {
			t.Logf("清理 schema %s 失败: %v", schema, err)
		}
	})

	if _, err := db.ExecContext(ctx, `set search_path to `+schema); err != nil {
		t.Fatalf("切 search_path 失败: %v", err)
	}

	client, err := pg.NewWithDB(db)
	if err != nil {
		t.Fatalf("建 pg.Client 失败: %v", err)
	}
	store, err := NewPGStore(ctx, client)
	if err != nil {
		t.Fatalf("建 PGStore 失败: %v", err)
	}
	return store
}

// eachStore runs the same set of assertions once each on the two Store implementations.
//
// This is how this module enforces "the two implementations must not each be green on their own": every test in feishu_test.go that touches persistence
// is wrapped with it, and the PG branch is skipped automatically when there is no database.
func eachStore(t *testing.T, run func(t *testing.T, store feishu.Store)) {
	t.Helper()
	t.Run("mem", func(t *testing.T) {
		run(t, feishu.NewMemStore())
	})
	t.Run("pg", func(t *testing.T) {
		run(t, newPGStore(t))
	})
}

// Table creation must be idempotent: the process runs NewPGStore on every start.
func TestPGStoreSchemaIsIdempotent(t *testing.T) {
	store := newPGStore(t)
	ctx := context.Background()
	// Creating again on the same client must not error.
	if _, err := NewPGStore(ctx, store.client); err != nil {
		t.Fatalf("重复建表应当无害，实际 %v", err)
	}
	// And the existing data is still there.
	if _, err := store.CreateApp(ctx, &feishu.App{ID: "a1", AppID: "cli_1", WorkspaceID: testWorkspace}, "s"); err != nil {
		t.Fatalf("落库失败: %v", err)
	}
	if _, err := NewPGStore(ctx, store.client); err != nil {
		t.Fatalf("重复建表应当无害，实际 %v", err)
	}
	if _, err := store.GetApp(ctx, "a1"); err != nil {
		t.Errorf("重复建表不该清掉已有数据，实际 %v", err)
	}
}

func TestNewPGStoreRejectsNilClient(t *testing.T) {
	if _, err := NewPGStore(context.Background(), nil); !errors.Is(err, feishu.ErrInvalidArgument) {
		t.Fatalf("期望 %v，实际 %v", feishu.ErrInvalidArgument, err)
	}
}

// A clashing appID must return ErrAppExists: the idempotent branch in the business layer relies on this error.
func TestPGStoreCreateAppRejectsDuplicateAppID(t *testing.T) {
	store := newPGStore(t)
	ctx := context.Background()
	if _, err := store.CreateApp(ctx, &feishu.App{ID: "a1", AppID: "cli_dup", WorkspaceID: testWorkspace}, "s1"); err != nil {
		t.Fatalf("落库失败: %v", err)
	}
	// A different AMA-side ID, but the same Feishu appID.
	_, err := store.CreateApp(ctx, &feishu.App{ID: "a2", AppID: "cli_dup", WorkspaceID: testWorkspace}, "s2")
	if !errors.Is(err, feishu.ErrAppExists) {
		t.Fatalf("期望 %v，实际 %v", feishu.ErrAppExists, err)
	}
	// The failed attempt must not leave half a record.
	if _, err := store.GetApp(ctx, "a2"); !errors.Is(err, feishu.ErrAppNotFound) {
		t.Errorf("撞唯一键的插入不该落库，实际 %v", err)
	}
	// The secret should still be the first one too.
	secret, err := store.AppSecret(ctx, "cli_dup")
	if err != nil || secret != "s1" {
		t.Errorf("密钥期望 s1，实际 %q err=%v", secret, err)
	}
}

// When the check-then-insert pre-check is bypassed, the unique constraint must catch it; this bypasses the pre-check directly to verify the constraint really exists.
func TestPGStoreAppIDUniqueConstraintExists(t *testing.T) {
	store := newPGStore(t)
	ctx := context.Background()
	if _, err := store.CreateApp(ctx, &feishu.App{ID: "a1", AppID: "cli_u", WorkspaceID: testWorkspace}, "s"); err != nil {
		t.Fatalf("落库失败: %v", err)
	}
	// Do not go through CreateApp; insert directly. There is no pre-check on this path, so what it hits must be the database constraint.
	_, err := store.client.Exec(ctx, `
		insert into feishu_apps (id, workspace_id, app_id, created_at, doc)
		values ($1, $2, $3, $4, $5)`,
		"a2", testWorkspace, "cli_u", time.Now(), []byte(`{}`),
	)
	if !pg.IsUniqueViolation(err) {
		t.Fatalf("期望撞唯一约束（23505），实际 %v", err)
	}
	if name := pg.ConstraintName(err); name != appIDConstraint {
		t.Errorf("约束名期望 %q，实际 %q——store_pg.go 靠它区分主键冲突", appIDConstraint, name)
	}
}

// When the secret is empty, AppSecret must return ErrAppNotFound, not ("", nil).
//
// Matches the in-memory implementation: secrets is a separate map, and CreateApp never writes into it when given an empty secret.
// Storing it as an empty string would make this return an empty string plus nil, and the two implementations would drift. The PG side expresses the same thing
// by storing NULL (not an empty string) in the app_secret column.
func TestStoreEmptySecretIsAbsent(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ctx := context.Background()
		if _, err := store.CreateApp(ctx, &feishu.App{ID: "a1", AppID: "cli_nosecret", WorkspaceID: testWorkspace}, ""); err != nil {
			t.Fatalf("落库失败: %v", err)
		}
		// The app itself exists.
		if _, err := store.GetApp(ctx, "a1"); err != nil {
			t.Fatalf("应用应该存在，实际 %v", err)
		}
		// But the secret does not.
		secret, err := store.AppSecret(ctx, "cli_nosecret")
		if !errors.Is(err, feishu.ErrAppNotFound) {
			t.Errorf("期望 %v，实际 secret=%q err=%v", feishu.ErrAppNotFound, secret, err)
		}
	})
}

// SaveApp must not touch the secret: in the in-memory implementation the secret is in another map that SaveApp cannot reach at all.
// On the PG side the secret and doc are in the same row; overwriting the whole row casually would let an ordinary readiness refresh
// wipe the secret. So this assertion is a real risk only for PG; both run it to pin the same contract.
func TestStoreSaveAppKeepsSecret(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ctx := context.Background()
		app, err := store.CreateApp(ctx, &feishu.App{ID: "a1", AppID: "cli_keep", WorkspaceID: testWorkspace}, "s-1")
		if err != nil {
			t.Fatalf("落库失败: %v", err)
		}
		app.Name = "改个名"
		if _, err := store.SaveApp(ctx, app); err != nil {
			t.Fatalf("保存失败: %v", err)
		}
		secret, err := store.AppSecret(ctx, "cli_keep")
		if err != nil || secret != "s-1" {
			t.Errorf("SaveApp 不该动密钥，期望 s-1，实际 %q err=%v", secret, err)
		}
	})
}

// Error values of each read/write method when the target does not exist; the two implementations must match.
func TestStoreNotFoundErrors(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ctx := context.Background()
		if _, err := store.GetApp(ctx, "没有这个"); !errors.Is(err, feishu.ErrAppNotFound) {
			t.Errorf("GetApp 期望 %v，实际 %v", feishu.ErrAppNotFound, err)
		}
		if _, err := store.GetAppByAppID(ctx, "cli_没有"); !errors.Is(err, feishu.ErrAppNotFound) {
			t.Errorf("GetAppByAppID 期望 %v，实际 %v", feishu.ErrAppNotFound, err)
		}
		if _, err := store.SaveApp(ctx, &feishu.App{ID: "没有这个"}); !errors.Is(err, feishu.ErrAppNotFound) {
			t.Errorf("SaveApp 期望 %v，实际 %v", feishu.ErrAppNotFound, err)
		}
		if _, err := store.AppSecret(ctx, "cli_没有"); !errors.Is(err, feishu.ErrAppNotFound) {
			t.Errorf("AppSecret 期望 %v，实际 %v", feishu.ErrAppNotFound, err)
		}
		if err := store.SetAppSecret(ctx, "cli_没有", "s"); !errors.Is(err, feishu.ErrAppNotFound) {
			t.Errorf("SetAppSecret 期望 %v，实际 %v", feishu.ErrAppNotFound, err)
		}
		if err := store.DeleteApp(ctx, "没有这个"); !errors.Is(err, feishu.ErrAppNotFound) {
			t.Errorf("DeleteApp 期望 %v，实际 %v", feishu.ErrAppNotFound, err)
		}
		if _, err := store.GetSession(ctx, "没有这个"); !errors.Is(err, feishu.ErrSessionLost) {
			t.Errorf("GetSession 期望 %v，实际 %v", feishu.ErrSessionLost, err)
		}
		// Deleting a session is idempotent: missing is not an error.
		if err := store.DeleteSession(ctx, "没有这个"); err != nil {
			t.Errorf("删不存在的会话不该报错，实际 %v", err)
		}
		// Deleting again does not error either.
		if err := store.DeleteSession(ctx, "没有这个"); err != nil {
			t.Errorf("重复删会话不该报错，实际 %v", err)
		}
	})
}

// With invalid input both implementations must report the same error.
func TestStoreInvalidArguments(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ctx := context.Background()
		if err := store.PutSession(ctx, "  ", feishu.Session{}, time.Minute); !errors.Is(err, feishu.ErrInvalidArgument) {
			t.Errorf("空会话 ID 期望 %v，实际 %v", feishu.ErrInvalidArgument, err)
		}
		if _, err := store.CreateApp(ctx, nil, "s"); !errors.Is(err, feishu.ErrInvalidArgument) {
			t.Errorf("nil App 期望 %v，实际 %v", feishu.ErrInvalidArgument, err)
		}
		if _, err := store.CreateApp(ctx, &feishu.App{ID: "a1", AppID: "  "}, "s"); !errors.Is(err, feishu.ErrInvalidArgument) {
			t.Errorf("空 AppID 期望 %v，实际 %v", feishu.ErrInvalidArgument, err)
		}
		if _, err := store.SaveApp(ctx, nil); !errors.Is(err, feishu.ErrInvalidArgument) {
			t.Errorf("nil App 期望 %v，实际 %v", feishu.ErrInvalidArgument, err)
		}
		if err := store.SetAppSecret(ctx, "", "s"); !errors.Is(err, feishu.ErrInvalidArgument) {
			t.Errorf("空 appID 期望 %v，实际 %v", feishu.ErrInvalidArgument, err)
		}
		if err := store.SetAppSecret(ctx, "cli_x", ""); !errors.Is(err, feishu.ErrInvalidArgument) {
			t.Errorf("空密钥期望 %v，实际 %v", feishu.ErrInvalidArgument, err)
		}
	})
}

// An empty ListApps result must be a non-nil slice: the handler puts it into {"apps": ...},
// and nil serializes as null instead of [], forcing the frontend to add one more check.
func TestStoreListAppsNeverNil(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		apps, err := store.ListApps(context.Background(), "空空如也")
		if err != nil {
			t.Fatalf("列表失败: %v", err)
		}
		if apps == nil {
			t.Error("空结果也要返回非 nil 切片")
		}
		if len(apps) != 0 {
			t.Errorf("期望 0 个，实际 %d", len(apps))
		}
	})
}

// The list is ordered by connection time ascending, then by ID on ties. Map iteration is unordered, and SQL without order by is unordered too;
// both sides must order explicitly, otherwise the frontend list jumps on every refresh.
func TestStoreListAppsOrder(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ctx := context.Background()
		base := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
		// Written out of order on purpose; b/c have exactly the same time, to check the ID tie-breaker.
		seed := []struct {
			id string
			at time.Time
		}{
			{"a-3", base.Add(2 * time.Hour)},
			{"a-1", base},
			{"a-c", base.Add(time.Hour)},
			{"a-b", base.Add(time.Hour)},
		}
		for i, s := range seed {
			_, err := store.CreateApp(ctx, &feishu.App{
				ID: s.id, AppID: fmt.Sprintf("cli_%d", i),
				WorkspaceID: testWorkspace, CreatedAt: s.at,
			}, "s")
			if err != nil {
				t.Fatalf("落库失败: %v", err)
			}
		}
		apps, err := store.ListApps(ctx, testWorkspace)
		if err != nil {
			t.Fatalf("列表失败: %v", err)
		}
		got := make([]string, len(apps))
		for i, a := range apps {
			got[i] = a.ID
		}
		want := []string{"a-1", "a-b", "a-c", "a-3"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("顺序期望 %v，实际 %v", want, got)
		}
	})
}

// The whole App (including the nested Readiness and several pointer fields) must be exactly the same after storing and reading back.
//
// This is the core guarantee of the jsonb approach: fields are carried as a whole by encoding/json, and missing one blows up here.
func TestStoreRoundTripsFullApp(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ctx := context.Background()
		at := time.Date(2026, 8, 26, 10, 30, 0, 0, time.UTC)
		connected := at.Add(-time.Minute)
		requested := at.Add(-time.Hour)
		want := &feishu.App{
			ID:          "a1",
			WorkspaceID: testWorkspace,
			Name:        "AMA 助手",
			AppID:       "cli_full",
			AvatarURL:   "https://example.com/a.png",
			BotOpenID:   "ou_1",
			Region:      "feishu",
			TenantBrand: "feishu",
			Status:      feishu.StatusConnected,
			// **Deliberately set to a non-default mode.** With mention_only this assertion would be empty: old rows have no
			// triggerMode key, which decodes as an empty string, and Clone normalizes the empty string to mention_only,
			// so even if this field were not carried at all the assertion would still pass. Using topic_and_mention
			// really proves "the value is stored and read back as is".
			TriggerMode:   feishu.TriggerTopicAndMention,
			BoundAgentIDs: []string{"ag-1", "ag-2"},
			Readiness: &feishu.Readiness{
				Status:                      feishu.ReadinessWaitingForAdmin,
				CheckedAt:                   at,
				MissingScopes:               []string{"im:message"},
				UnapprovedScopes:            []string{"im:chat"},
				MissingEvents:               []string{"im.message.receive_v1"},
				MissingCallbacks:            []string{"card.action.trigger"},
				ConfigurationInspectable:    true,
				BotEnabled:                  true,
				Published:                   true,
				GroupMessageScopeGranted:    true,
				AdminAuthorizationRequested: true,
				RuntimeConnected:            true,
				LastConnectedAt:             &connected,
				LastRuntimeError:            "凭证可能已失效",
				LastCheckError:              "读不到配置",
			},
			AdminAuthorizationRequestedAt: &requested,
			CreatedByID:                   testActor,
			CreatedAt:                     at,
			UpdatedAt:                     at,
		}
		if _, err := store.CreateApp(ctx, want, "s"); err != nil {
			t.Fatalf("落库失败: %v", err)
		}
		got, err := store.GetApp(ctx, "a1")
		if err != nil {
			t.Fatalf("读取失败: %v", err)
		}
		// Compare JSON instead of structs: JSON is exactly this struct's external shape, and the contract itself.
		assertSameJSON(t, want, got)

		// The one fetched by Feishu appID should be the same too.
		byAppID, err := store.GetAppByAppID(ctx, "cli_full")
		if err != nil {
			t.Fatalf("按 appID 读取失败: %v", err)
		}
		assertSameJSON(t, want, byAppID)

		// And so should the one in the list.
		apps, err := store.ListApps(ctx, testWorkspace)
		if err != nil {
			t.Fatalf("列表失败: %v", err)
		}
		if len(apps) != 1 {
			t.Fatalf("期望 1 个，实际 %d", len(apps))
		}
		assertSameJSON(t, want, apps[0])
	})
}

// A session must also be exactly the same after storing and reading back.
func TestStoreRoundTripsSession(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ctx := context.Background()
		want := feishu.Session{
			WorkspaceID:         testWorkspace,
			ActorID:             testActor,
			DeviceCode:          "dc-1",
			Region:              "lark",
			PollIntervalSeconds: 10,
			ExpiresAt:           time.Date(2026, 8, 26, 10, 10, 0, 0, time.UTC),
		}
		if err := store.PutSession(ctx, "s1", want, time.Hour); err != nil {
			t.Fatalf("写会话失败: %v", err)
		}
		got, err := store.GetSession(ctx, "s1")
		if err != nil {
			t.Fatalf("读会话失败: %v", err)
		}
		assertSameJSON(t, want, got)

		// Overwrite: Poll writes again with the same id when it hits slow_down.
		want.PollIntervalSeconds = 15
		if err := store.PutSession(ctx, "s1", want, time.Hour); err != nil {
			t.Fatalf("覆盖会话失败: %v", err)
		}
		got, err = store.GetSession(ctx, "s1")
		if err != nil {
			t.Fatalf("读会话失败: %v", err)
		}
		if got.PollIntervalSeconds != 15 {
			t.Errorf("覆盖写没生效，间隔期望 15，实际 %d", got.PollIntervalSeconds)
		}

		// Not readable after deletion.
		if err := store.DeleteSession(ctx, "s1"); err != nil {
			t.Fatalf("删会话失败: %v", err)
		}
		if _, err := store.GetSession(ctx, "s1"); !errors.Is(err, feishu.ErrSessionLost) {
			t.Errorf("期望 %v，实际 %v", feishu.ErrSessionLost, err)
		}
	})
}

// Expired sessions cannot be read; the external behavior of the two implementations must be the same (the in-memory one sweeps in place,
// the PG one filters at query time + cleans up on the write path, see PGStore.GetSession).
func TestStoreExpiredSessionIsGone(t *testing.T) {
	now := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	ctx := context.Background()

	t.Run("mem", func(t *testing.T) {
		now = time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
		store := feishu.NewMemStoreWithClock(clock)
		assertSessionExpires(t, store, func(d time.Duration) { now = now.Add(d) })
	})
	t.Run("pg", func(t *testing.T) {
		now = time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
		store := newPGStore(t)
		store.now = clock
		assertSessionExpires(t, store, func(d time.Duration) { now = now.Add(d) })
		// On the PG side, also confirm the expired row was really cleaned up, not just filtered out in the query:
		// filtering without cleaning would let a database running for months fill up with dead sessions.
		if err := store.PutSession(ctx, "s2", feishu.Session{ActorID: testActor}, time.Minute); err != nil {
			t.Fatalf("写会话失败: %v", err)
		}
		var n int
		if err := store.client.QueryRow(ctx, `select count(*) from feishu_sessions`).Scan(&n); err != nil {
			t.Fatalf("统计会话失败: %v", err)
		}
		if n != 1 {
			t.Errorf("过期会话应在写路径被清掉，实际表里还有 %d 行", n)
		}
	})
}

// assertSessionExpires is the assertion shared by the two branches above: unreadable after expiry.
func assertSessionExpires(t *testing.T, store feishu.Store, advance func(time.Duration)) {
	t.Helper()
	ctx := context.Background()
	if err := store.PutSession(ctx, "s1", feishu.Session{ActorID: testActor}, time.Minute); err != nil {
		t.Fatalf("写会话失败: %v", err)
	}
	// Not expired yet, readable.
	if _, err := store.GetSession(ctx, "s1"); err != nil {
		t.Fatalf("未过期的会话应该读得到，实际 %v", err)
	}
	advance(2 * time.Minute)
	if _, err := store.GetSession(ctx, "s1"); !errors.Is(err, feishu.ErrSessionLost) {
		t.Fatalf("期望 %v，实际 %v", feishu.ErrSessionLost, err)
	}
}

// When ttl <= 0 it must fall back to sessionTTL, not be treated as "expire immediately".
func TestStoreDefaultsSessionTTL(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ctx := context.Background()
		if err := store.PutSession(ctx, "s1", feishu.Session{ActorID: testActor}, 0); err != nil {
			t.Fatalf("写会话失败: %v", err)
		}
		if _, err := store.GetSession(ctx, "s1"); err != nil {
			t.Errorf("ttl 为 0 应落到默认 TTL，实际 %v", err)
		}
	})
}

// Every method must TrimSpace its keys, the same in both implementations.
func TestStoreTrimsKeys(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ctx := context.Background()
		if err := store.PutSession(ctx, " s1 ", feishu.Session{ActorID: testActor}, time.Minute); err != nil {
			t.Fatalf("写会话失败: %v", err)
		}
		if _, err := store.GetSession(ctx, " s1 "); err != nil {
			t.Errorf("会话 ID 应被 trim，实际 %v", err)
		}
		if _, err := store.CreateApp(ctx, &feishu.App{ID: "a1", AppID: "cli_t", WorkspaceID: testWorkspace}, "s"); err != nil {
			t.Fatalf("落库失败: %v", err)
		}
		if _, err := store.GetApp(ctx, " a1 "); err != nil {
			t.Errorf("GetApp 的 ID 应被 trim，实际 %v", err)
		}
		if _, err := store.GetAppByAppID(ctx, " cli_t "); err != nil {
			t.Errorf("GetAppByAppID 的 appID 应被 trim，实际 %v", err)
		}
		if _, err := store.AppSecret(ctx, " cli_t "); err != nil {
			t.Errorf("AppSecret 的 appID 应被 trim，实际 %v", err)
		}
		if err := store.SetAppSecret(ctx, " cli_t ", "s2"); err != nil {
			t.Errorf("SetAppSecret 的 appID 应被 trim，实际 %v", err)
		}
		if _, err := store.ListApps(ctx, " "+testWorkspace+" "); err != nil {
			t.Errorf("ListApps 的 workspaceID 应被 trim，实际 %v", err)
		}
		if err := store.DeleteApp(ctx, " a1 "); err != nil {
			t.Errorf("DeleteApp 的 ID 应被 trim，实际 %v", err)
		}
	})
}

// After SetAppSecret changes the secret, AppSecret must read the new value.
func TestStoreSetAppSecret(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ctx := context.Background()
		if _, err := store.CreateApp(ctx, &feishu.App{ID: "a1", AppID: "cli_s", WorkspaceID: testWorkspace}, "old"); err != nil {
			t.Fatalf("落库失败: %v", err)
		}
		if err := store.SetAppSecret(ctx, "cli_s", "new"); err != nil {
			t.Fatalf("换密钥失败: %v", err)
		}
		got, err := store.AppSecret(ctx, "cli_s")
		if err != nil || got != "new" {
			t.Errorf("密钥期望 new，实际 %q err=%v", got, err)
		}
	})
}

// assertSameJSON compares the JSON shape of two values.
//
// Compare JSON instead of reflect.DeepEqual: JSON is exactly these structs' external shape, and this repo's real
// contract (the source of truth is the frontend's LocalApiClient). It also sidesteps differences in time zone representation: after a PG round trip the same instant
// may come back with +00:00 instead of Z; it is the same moment and should not be judged unequal.
func assertSameJSON(t *testing.T, want, got any) {
	t.Helper()
	wantRaw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	gotRaw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if string(wantRaw) != string(gotRaw) {
		t.Errorf("落库前后形状不一致\n期望: %s\n实际: %s", wantRaw, gotRaw)
	}
}

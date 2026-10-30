package channeldao

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/chyroc/ama/server/shared/infra/pg"
)

// The PG tests need a local database (docker compose up -d postgres). Without one they are skipped,
// and a skipped run is green: after changing SQL here, run them against a real database.
// AMA_TEST_PG_DSN overrides the connection; the default matches docker-compose.yml.
const defaultTestDSN = "host=127.0.0.1 port=15432 dbname=ama user=ama password=ama sslmode=disable connect_timeout=3"

var (
	pgProbeOnce sync.Once
	pgProbeErr  error
)

func testPGDSN() string {
	if v := strings.TrimSpace(os.Getenv("AMA_TEST_PG_DSN")); v != "" {
		return v
	}
	return defaultTestDSN
}

// newTestClient returns a client whose search_path is a fresh random schema, dropped after the test.
func newTestClient(t *testing.T) *pg.Client {
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
		t.Skipf("跳过：连不上本地 PostgreSQL（AMA_TEST_PG_DSN=%q）: %v", testPGDSN(), pgProbeErr)
	}

	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	schemaName := "channel_test_" + hex.EncodeToString(buf)
	db, err := sql.Open("pgx", testPGDSN())
	if err != nil {
		t.Fatal(err)
	}
	// One connection, so "set search_path" applies to every statement of the test.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `create schema `+schemaName); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, err := sql.Open("pgx", testPGDSN())
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_, _ = c.ExecContext(context.Background(), `drop schema if exists `+schemaName+` cascade`)
	})
	if _, err := db.ExecContext(ctx, `set search_path to `+schemaName); err != nil {
		t.Fatal(err)
	}
	client, err := pg.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

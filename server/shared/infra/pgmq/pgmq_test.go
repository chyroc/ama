// Contract tests for pgmq, **against a real PostgreSQL**.
//
// # Why not a fake DB
//
// Almost all of this package's value is in the SQL: atomic claiming (two instances must not
// get the same message), takeover after the lease expires, FIFO order. **A fake
// implementation cannot test any of these**: plugging in an in-memory map would only check
// "my Go logic matches my own idea", while what really goes wrong is semantics like the
// EvalPlanQual recheck that only a real database has (coredao/store_pg_schedule.go:188
// records the time this pattern bit us).
//
// When the database is unreachable the whole package does t.Skip, so `go test ./...` still
// runs offline for people who have not started the container. Same approach as
// coredao/store_pg_test.go.
package pgmq

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
)

// testDB adapts *sql.DB to this package's DB interface.
//
// Same shape as the shim in cmd/amad. This package intentionally does not import
// shared/infra/pg (see the layering section in the package comment).
type testDB struct{ db *sql.DB }

func (t testDB) Exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return t.db.ExecContext(ctx, q, args...)
}
func (t testDB) Query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return t.db.QueryContext(ctx, q, args...)
}
func (t testDB) QueryRow(ctx context.Context, q string, args ...any) *sql.Row {
	return t.db.QueryRowContext(ctx, q, args...)
}

var (
	envOnce sync.Once
	shared  *sql.DB
	skipWhy string
)

func dsn() string {
	get := func(k, def string) string {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
		return def
	}
	return fmt.Sprintf("host=%s port=%s database=%s user=%s password=%s sslmode=disable",
		get("AMA_TEST_PG_HOST", "127.0.0.1"), get("AMA_TEST_PG_PORT", "15432"),
		get("AMA_TEST_PG_DATABASE", "ama"), get("AMA_TEST_PG_USER", "ama"),
		get("AMA_TEST_PG_PASSWORD", "ama"))
}

// testQueue builds a clean queue. Each test uses its **own topic prefix**, so running in
// parallel (-race, or other sessions in the same workspace also running) never lets them see
// each other's messages.
func testQueue(t *testing.T, cfg Config) (Queue, string) {
	t.Helper()
	envOnce.Do(func() {
		db, err := sql.Open("pgx", dsn())
		if err != nil {
			skipWhy = "打不开数据库：" + err.Error()
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := db.PingContext(ctx); err != nil {
			skipWhy = "连不上本地 PostgreSQL（" + err.Error() + "）\n" +
				"起库：在仓库根跑 `docker compose up -d`，等 healthy"
			_ = db.Close()
			return
		}
		if _, err := db.ExecContext(ctx, schemaDDL); err != nil {
			skipWhy = "建表失败：" + err.Error()
			_ = db.Close()
			return
		}
		shared = db
	})
	if skipWhy != "" {
		t.Skip(skipWhy)
	}

	cfg.DB = testDB{shared}
	q, err := New(cfg)
	if err != nil {
		t.Fatalf("建队列失败：%v", err)
	}
	topic := fmt.Sprintf("test:%s:%d", t.Name(), time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = shared.Exec(`delete from mq_messages where topic = $1`, topic)
	})
	return q, topic
}

// schemaDDL is a minimal subset kept consistent with repository/schema/mq_messages.sql.
//
// ⚠️ It intentionally does **not** go:embed that file: go:embed cannot cross parent
// directories (this package is under infra/, schema is under repository/), and moving files
// around for embed would break the "one table per file, file name is the table name" rule.
const schemaDDL = `
create table if not exists mq_messages (
    id bigserial primary key,
    topic text not null,
    payload bytea not null,
    created_at timestamptz not null,
    expires_at timestamptz not null,
    claimed_by text,
    claimed_until timestamptz
);
create index if not exists mq_messages_due_idx on mq_messages (topic, id);`

// drain takes at most n messages and stops on timeout.
func drain(t *testing.T, q Queue, topic string, n int, within time.Duration) [][]byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()

	var got [][]byte
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = q.Consume(ctx, topic, func(_ context.Context, m Message) error {
			mu.Lock()
			got = append(got, m.Payload)
			full := len(got) >= n
			mu.Unlock()
			if full {
				cancel()
			}
			return nil
		})
	}()
	<-done
	mu.Lock()
	defer mu.Unlock()
	return got
}

// ⭐ What is sent must be received, with **exactly the same bytes**.
//
// Assert bytes, not strings: this queue carries PTY frames, which contain control characters
// and non-UTF-8 sequences. With a string comparison, switching the column to text/jsonb some
// day would still pass, while real data gets rewritten.
func TestPublishThenConsume(t *testing.T) {
	q, topic := testQueue(t, Config{})
	ctx := context.Background()

	// Contains NUL, ESC and an invalid UTF-8 byte, all things that really show up in a
	// terminal.
	want := []byte{0x1b, '[', 'A', 0x00, 0xff, 'l', 's', '\r'}
	if err := q.Publish(ctx, topic, want, time.Minute); err != nil {
		t.Fatalf("发布失败：%v", err)
	}

	got := drain(t, q, topic, 1, 5*time.Second)
	if len(got) != 1 {
		t.Fatalf("应当收到 1 条，实际 %d 条", len(got))
	}
	if string(got[0]) != string(want) {
		t.Errorf("字节被改写了：\n发出 %v\n收到 %v", want, got[0])
	}
}

// ⭐ One message **can only be taken by one consumer**.
//
// This is the foundation of the whole fix: when two pods consume the same topic at the same
// time, a keystroke delivered twice means the user types one character and the container
// gets two.
//
// ⚠️ The test must make the two consumers **really compete at the same time** (start both
// first, then send messages). Otherwise the first one takes all messages and the second one
// never competes, and the test would pass even if the claim SQL were wrong.
func TestClaimIsExclusive(t *testing.T) {
	q1, topic := testQueue(t, Config{Owner: "inst-1"})
	q2, err := New(Config{DB: testDB{shared}, Owner: "inst-2"})
	if err != nil {
		t.Fatalf("建第二条队列失败：%v", err)
	}

	const total = 30
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var mu sync.Mutex
	seen := map[int64]int{}
	count := func(m Message) {
		mu.Lock()
		seen[m.ID]++
		n := len(seen)
		mu.Unlock()
		if n >= total {
			cancel()
		}
	}

	var wg sync.WaitGroup
	for _, q := range []Queue{q1, q2} {
		wg.Add(1)
		go func(q Queue) {
			defer wg.Done()
			_ = q.Consume(ctx, topic, func(_ context.Context, m Message) error {
				count(m)
				return nil
			})
		}(q)
	}

	// Send only after both consumers are running, so they compete for the same batch.
	time.Sleep(200 * time.Millisecond)
	for i := 0; i < total; i++ {
		if err := q1.Publish(context.Background(), topic, []byte{byte(i)}, time.Minute); err != nil {
			t.Fatalf("发布失败：%v", err)
		}
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != total {
		t.Errorf("应当收到 %d 条不同的消息，实际 %d 条", total, len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("消息 %d 被投递了 %d 次——同一条消息只能有一个消费者拿到", id, n)
		}
	}
}

// ⭐ Delivery order is enqueue order (FIFO).
//
// ⚠️ What is asserted is **the order itself**, and the want below is a meaningful sequence:
// if the implementation switched to descending id, or did not sort at all (a sequential scan
// of a small table happens to equal insert order), this must fail. This repo has learned
// that "a small table does not expose a missing order by", so the test sends 20 messages to
// give a sequential scan a chance to differ from real sorting.
func TestFIFOOrder(t *testing.T) {
	q, topic := testQueue(t, Config{})
	ctx := context.Background()

	const n = 20
	for i := 0; i < n; i++ {
		if err := q.Publish(ctx, topic, []byte(strconv.Itoa(i)), time.Minute); err != nil {
			t.Fatalf("发布失败：%v", err)
		}
	}

	got := drain(t, q, topic, n, 15*time.Second)
	if len(got) != n {
		t.Fatalf("应当收到 %d 条，实际 %d 条", n, len(got))
	}
	for i, b := range got {
		if string(b) != strconv.Itoa(i) {
			t.Fatalf("第 %d 条应当是 %q，实际 %q —— 投递顺序与入队顺序不一致", i, strconv.Itoa(i), b)
		}
	}
}

// ⭐ Expired messages are not delivered.
//
// Time-sensitive cases (terminal keystrokes) need this: old keystrokes that piled up flooding
// into the container after a reconnect is harmful.
func TestExpiredIsNotDelivered(t *testing.T) {
	q, topic := testQueue(t, Config{})
	ctx := context.Background()

	// Expires after 1 millisecond.
	if err := q.Publish(ctx, topic, []byte("stale"), time.Millisecond); err != nil {
		t.Fatalf("发布失败：%v", err)
	}
	if err := q.Publish(ctx, topic, []byte("fresh"), time.Minute); err != nil {
		t.Fatalf("发布失败：%v", err)
	}
	time.Sleep(50 * time.Millisecond)

	got := drain(t, q, topic, 1, 5*time.Second)
	if len(got) != 1 {
		t.Fatalf("应当只收到 1 条（新的那条），实际 %d 条", len(got))
	}
	if string(got[0]) != "fresh" {
		t.Errorf("收到的应当是没过期的 %q，实际 %q——过期消息被投递了", "fresh", got[0])
	}
}

// ⭐ Messages that failed to process **are not lost**, and are redelivered after the lease
// expires.
//
// When fn returns an error the message is intentionally not deleted, and claimed_until
// expiring lets someone else (or itself) take over. This test uses a very short lease to keep
// "waiting for the lease to expire" within an acceptable test time.
func TestFailedMessageIsRedelivered(t *testing.T) {
	q, topic := testQueue(t, Config{ClaimFor: 300 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := q.Publish(context.Background(), topic, []byte("retry-me"), time.Minute); err != nil {
		t.Fatalf("发布失败：%v", err)
	}

	var mu sync.Mutex
	attempts := 0
	_ = q.Consume(ctx, topic, func(_ context.Context, m Message) error {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n < 2 {
			return errors.New("这一次故意失败")
		}
		cancel()
		return nil
	})

	mu.Lock()
	defer mu.Unlock()
	if attempts < 2 {
		t.Errorf("处理失败的消息应当被重投，实际只投了 %d 次", attempts)
	}
}

// ⭐ Without a Dialer it falls back to pure polling, and features are not affected.
//
// This guards the intentional fallback "local/test setups do not need two kinds of
// connections".
func TestWorksWithoutListener(t *testing.T) {
	q, topic := testQueue(t, Config{Dial: nil, PollEvery: 100 * time.Millisecond})
	if err := q.Publish(context.Background(), topic, []byte("polled"), time.Minute); err != nil {
		t.Fatalf("发布失败：%v", err)
	}
	got := drain(t, q, topic, 1, 5*time.Second)
	if len(got) != 1 || string(got[0]) != "polled" {
		t.Errorf("没有 Dialer 时应当靠轮询收到消息，实际收到 %d 条 %q", len(got), got)
	}
}

// ⭐ The channel name must stay under PG's 63-character limit, be stable for the same topic,
// and differ for different topics.
//
// ⚠️ This one could be tested without the database, but it guards a **measured platform
// limit**: using the topic directly as the channel name, a long id like `term:<terminalID>`
// would give `channel name too long`, which only blows up at runtime, and only when the id
// is long enough.
func TestChannelNameFitsPGLimit(t *testing.T) {
	long := "term:" + strings.Repeat("x", 500)
	ch := channelFor(long)
	if len(ch) > 63 {
		t.Errorf("通道名 %d 字符，超过 PG 的 63 上限：%s", len(ch), ch)
	}
	if ch != channelFor(long) {
		t.Error("同一个 topic 必须映射到同一个通道名，否则发布方与订阅方对不上")
	}
	if ch == channelFor(long+"y") {
		t.Error("不同 topic 映射到了同一个通道名——会互相唤醒")
	}
	// The channel name appears in places like `listen <name>` that cannot be parameterized,
	// so its character set must be clean.
	for _, r := range strings.TrimPrefix(ch, "ama_mq_") {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Errorf("通道名里出现了非 hex 字符 %q：%s", r, ch)
		}
	}
}

// ⭐ PurgeExpired deletes only expired messages and does not touch live ones.
func TestPurgeExpiredOnlyRemovesExpired(t *testing.T) {
	q, topic := testQueue(t, Config{})
	ctx := context.Background()

	if err := q.Publish(ctx, topic, []byte("stale"), time.Millisecond); err != nil {
		t.Fatalf("发布失败：%v", err)
	}
	if err := q.Publish(ctx, topic, []byte("fresh"), time.Hour); err != nil {
		t.Fatalf("发布失败：%v", err)
	}
	time.Sleep(50 * time.Millisecond)

	if _, err := PurgeExpired(ctx, testDB{shared}); err != nil {
		t.Fatalf("清理失败：%v", err)
	}

	var left int
	if err := shared.QueryRow(`select count(*) from mq_messages where topic = $1`, topic).Scan(&left); err != nil {
		t.Fatalf("查剩余失败：%v", err)
	}
	if left != 1 {
		t.Errorf("清理后应当剩 1 条（没过期的那条），实际剩 %d 条", left)
	}
}

// Make sure the stdlib driver is registered (database/sql's "pgx").
var _ = stdlib.GetDefaultDriver

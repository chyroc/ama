// Tests in this package **need a real Redis**. By default they connect to the one started by
// the local docker-compose (127.0.0.1:16379).
//
// # Why not a fake implementation
//
// This package does only three things: SET NX PX, two Lua scripts, and a random token. The
// semantics of the first two are fully defined by Redis, so testing with a fake would only
// test whether my own fake Redis is right. The traps we really need to guard against (Lua
// atomicity, renew must fail after someone else took the lock on expiry) are exactly where a
// fake most easily "passes".
//
// So: if Redis is unreachable, t.Skip instead of failing. `go test -race ./...` is still all
// green on machines without the container, matching the repo rule "fully offline, no
// credentials needed"; with the container running, it tests for real.
//
//	docker compose up -d        # at the repo root
//
// AMA_REDIS_ADDR can point it at another instance.

package redis

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// testClient connects to the test Redis, and skips the whole test if it cannot connect.
func testClient(t *testing.T) *Client {
	t.Helper()
	addr := strings.TrimSpace(os.Getenv("AMA_REDIS_ADDR"))
	if addr == "" {
		addr = DefaultAddr
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := New(ctx, Config{Addr: addr, DialTimeout: 2 * time.Second})
	if err != nil {
		t.Skipf("连不上 Redis %s，跳过（起容器：docker compose up -d）: %v", addr, err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// testKey builds a key with the test name and clears it at the end, so tests do not affect
// each other.
func testKey(t *testing.T, client *Client) string {
	t.Helper()
	key := "ama:test:" + t.Name()
	t.Cleanup(func() {
		_ = client.rdb.Del(context.Background(), key).Err()
	})
	return key
}

func TestAcquireExcludesSecondHolder(t *testing.T) {
	client := testClient(t)
	ctx := context.Background()
	key := testKey(t, client)

	lease, ok, err := client.Acquire(ctx, key, 5*time.Second)
	if err != nil {
		t.Fatalf("第一次抢锁出错: %v", err)
	}
	if !ok {
		t.Fatal("第一次就该抢到")
	}

	// A second holder takes the same lock: it must return false, and **must not be an error**.
	// The lock being in someone else's hands is a normal leader election result; reporting it
	// as a failure would make the caller think Redis is down.
	second, ok, err := client.Acquire(ctx, key, 5*time.Second)
	if err != nil {
		t.Fatalf("第二次抢锁不该出错: %v", err)
	}
	if ok {
		t.Fatal("锁已经在别人手上，第二次不该抢到")
	}
	if second != nil {
		t.Fatal("没抢到时不该返回 lease")
	}

	// After release it can be taken again.
	if err := lease.Release(ctx); err != nil {
		t.Fatalf("释放出错: %v", err)
	}
	third, ok, err := client.Acquire(ctx, key, 5*time.Second)
	if err != nil || !ok {
		t.Fatalf("释放后该能重新抢到: ok=%v err=%v", ok, err)
	}
	_ = third.Release(ctx)
}

func TestLeaseExpiresWhenNotRenewed(t *testing.T) {
	client := testClient(t)
	ctx := context.Background()
	key := testKey(t, client)

	if _, ok, err := client.Acquire(ctx, key, 300*time.Millisecond); err != nil || !ok {
		t.Fatalf("抢锁失败: ok=%v err=%v", ok, err)
	}
	// Do not renew; wait for it to expire on its own. This is exactly the case "the holder
	// process was killed": nobody releases it explicitly, so the lock must expire on its own,
	// otherwise no instance of that app could ever open the long connection again.
	time.Sleep(500 * time.Millisecond)

	lease, ok, err := client.Acquire(ctx, key, 2*time.Second)
	if err != nil {
		t.Fatalf("过期后抢锁出错: %v", err)
	}
	if !ok {
		t.Fatal("租约过期后该能被别人抢到")
	}
	_ = lease.Release(ctx)
}

func TestRenewKeepsLeaseAlive(t *testing.T) {
	client := testClient(t)
	ctx := context.Background()
	key := testKey(t, client)

	lease, ok, err := client.Acquire(ctx, key, 400*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("抢锁失败: ok=%v err=%v", ok, err)
	}
	defer func() { _ = lease.Release(ctx) }()

	// Renew twice within the original TTL, for a total longer than the original TTL. If
	// renewal works, the lock should stay ours the whole time.
	for i := 0; i < 2; i++ {
		time.Sleep(250 * time.Millisecond)
		renewed, err := lease.Renew(ctx, 400*time.Millisecond)
		if err != nil {
			t.Fatalf("第 %d 次续期出错: %v", i+1, err)
		}
		if !renewed {
			t.Fatalf("第 %d 次续期该成功，锁还在自己手上", i+1)
		}
	}

	if _, ok, _ := client.Acquire(ctx, key, time.Second); ok {
		t.Fatal("一直在续期，别人不该抢到")
	}
}

// This test pins the most dangerous trap of this package (see the package comment):
// after A's lease expires and B takes the lock, A's late renew/release **must never touch B's
// lock**. An implementation that does not compare the token fails here: Renew would extend
// B's lock as A's, and Release would delete B's lock outright, so two instances would open
// the long connection at the same time.
func TestStaleHolderCannotTouchNewLease(t *testing.T) {
	client := testClient(t)
	ctx := context.Background()
	key := testKey(t, client)

	stale, ok, err := client.Acquire(ctx, key, 200*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("抢锁失败: ok=%v err=%v", ok, err)
	}
	time.Sleep(350 * time.Millisecond) // let stale expire

	fresh, ok, err := client.Acquire(ctx, key, 5*time.Second)
	if err != nil || !ok {
		t.Fatalf("过期后新持有者该抢到: ok=%v err=%v", ok, err)
	}
	defer func() { _ = fresh.Release(ctx) }()

	// A late renew: must return false, and must not change the new holder's lock.
	renewed, err := stale.Renew(ctx, time.Minute)
	if err != nil {
		t.Fatalf("迟到的续期不该报错: %v", err)
	}
	if renewed {
		t.Fatal("锁已经不是自己的了，续期必须失败")
	}

	// A late release: must not delete the new holder's lock.
	if err := stale.Release(ctx); err != nil {
		t.Fatalf("迟到的释放不该报错: %v", err)
	}
	if _, ok, _ := client.Acquire(ctx, key, time.Second); ok {
		t.Fatal("新持有者的锁被迟到的 Release 删掉了")
	}

	// The new holder's own renewal still works.
	renewed, err = fresh.Renew(ctx, 5*time.Second)
	if err != nil || !renewed {
		t.Fatalf("新持有者续期该成功: renewed=%v err=%v", renewed, err)
	}
}

func TestAcquireRejectsBadArguments(t *testing.T) {
	client := testClient(t)
	ctx := context.Background()

	if _, _, err := client.Acquire(ctx, "  ", time.Second); err == nil {
		t.Error("空 key 该报错")
	}
	if _, _, err := client.Acquire(ctx, "ama:test:ttl", 0); err == nil {
		t.Error("ttl 为 0 该报错")
	}
}

// New must find "cannot connect" while connecting, instead of pushing the error to the first
// business operation.
func TestNewFailsFastOnUnreachableAddr(t *testing.T) {
	ctx := context.Background()
	// There is no Redis on 127.0.0.1:1.
	_, err := New(ctx, Config{Addr: "127.0.0.1:1", DialTimeout: time.Second})
	if err == nil {
		t.Fatal("连不上时 New 就该报错")
	}
	if !strings.Contains(err.Error(), "连接 Redis") {
		t.Errorf("错误信息该指向连接失败，实际: %v", err)
	}
}

// With no Addr it falls back to the local docker-compose address, and timeouts have defaults.
//
// This test does not need a real Redis: it only checks normalize and does not connect.
func TestConfigFallsBackToLocalAddr(t *testing.T) {
	cfg := Config{Addr: "   "}.normalize()

	if cfg.Addr != DefaultAddr {
		t.Errorf("该回落到 %s，实际 %q", DefaultAddr, cfg.Addr)
	}
	if cfg.DialTimeout <= 0 || cfg.OpTimeout <= 0 {
		t.Errorf("超时该有默认值，实际 dial=%v op=%v", cfg.DialTimeout, cfg.OpTimeout)
	}
}

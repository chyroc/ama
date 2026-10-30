// Package redis is the access layer for Redis.
//
// It has only one goal: **distributed leader election** for the Feishu long connection.
//
// # Why this repo needs Redis
//
// Feishu **allows only one long connection per app**. When several amad instances each
// connect to the same app directly, Feishu kicks out the earlier one, and the instances keep
// evicting each other. The symptom is not an error but **inbound messages randomly lost**:
// one message goes to instance A, the next to B, and whatever arrives in the gap while one is
// being kicked out is received by nobody. So a lock must be taken before connecting, and
// **if the lock is not acquired, never connect** (fail-closed). Better that this instance
// receives no messages than two instances fighting.
//
// The approach is a Redis lease + periodic renewal. Apart from this the repo uses Redis for
// nothing else: all data is in PostgreSQL, and there is no cache and no queue here.
//
// # Why not use PG's advisory lock and save a dependency
//
// We considered it. An advisory lock is bound to a session and is released automatically when
// the connection drops, which sounds like a perfect fit. But that "automatic" depends on when
// PG notices the connection is dead, and the connection pool also reuses the same session for
// other queries, so when the lease really expires becomes unpredictable. Leader election needs
// exactly a predictable expiry time. Redis's SET NX PX is an explicit millisecond TTL, so you
// can work out the worst-case gap just by reading the code.
//
// # Lease semantics
//
//	Acquire  SET key value NX PX ttl                take the lock; fails if key exists
//	Renew    extend only if value is still ours     Lua makes "compare + extend" atomic
//	Release  delete only if value is still ours     same as above
//
// value is a random string owned by each holder. **Comparing value is the key point**:
// without it, after A's lease expires and B takes the lock, A's late renew/release would
// touch B's lock. The renew would extend B's lock as A's, and the release would delete B's
// lock outright, so both instances would connect at the same time. This is the most classic
// trap of distributed locks.
//
// # Usage
//
//	client, err := redis.New(ctx, redis.Config{Addr: "127.0.0.1:16379"})
//	if err != nil {
//	    return err
//	}
//	defer client.Close()
//
//	lease, ok, err := client.Acquire(ctx, "lark:longconn:cli_xxx", 20*time.Second)
//	if err != nil || !ok {
//	    return err // lock not acquired: do not connect
//	}
//	defer lease.Release(context.WithoutCancel(ctx))
//	// start a goroutine that calls lease.Renew(ctx, 20*time.Second) every 5 seconds
//
// Under the hood it is the open-source github.com/redis/go-redis/v9, connecting directly to
// host:port (optionally with a password and database number). ctx is passed all the way to
// go-redis, so the caller's timeout and cancel apply to each command; the timeouts in Config
// are a fallback when no timeout is passed.
//
// ⚠️ Release must get a **ctx that will not be canceled** (context.WithoutCancel): when the
// process gets an exit signal, the outer ctx is already canceled, and releasing with it fails
// right away, so the lock can only wait for its TTL to expire.
package redis

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// ErrInvalidConfig means the config is invalid.
var ErrInvalidConfig = errors.New("redis: 配置不合法")

// DefaultAddr is the Redis started by the local docker-compose (container 6379 mapped to host
// 16379).
const DefaultAddr = "127.0.0.1:16379"

const defaultDialTimeout = 5 * time.Second
const defaultOpTimeout = 1 * time.Second

// Config is the Redis connection config.
type Config struct {
	// Addr is host:port. Defaults to DefaultAddr when empty.
	Addr string
	// Password: empty means no auth (that is how the local docker-compose is).
	Password string
	// DB is the database number. Defaults to 0 when empty.
	DB int
	// DialTimeout is the connect timeout. Defaults to 5s when empty.
	DialTimeout time.Duration
	// OpTimeout is the read/write timeout for one command. Defaults to 1s when empty.
	OpTimeout time.Duration
}

func (c Config) normalize() Config {
	c.Addr = strings.TrimSpace(c.Addr)
	if c.Addr == "" {
		c.Addr = DefaultAddr
	}
	if c.DialTimeout <= 0 {
		c.DialTimeout = defaultDialTimeout
	}
	if c.OpTimeout <= 0 {
		c.OpTimeout = defaultOpTimeout
	}
	return c
}

func (c Config) validate() error {
	if c.DB < 0 {
		return fmt.Errorf("%w: db 不能是负数", ErrInvalidConfig)
	}
	return nil
}

// Client is a Redis client that only exposes the few operations leader election needs.
type Client struct {
	rdb *goredis.Client
}

// New connects and pings once. **If it cannot connect, it errors right here**, instead of
// pushing the error to the first business operation.
func New(ctx context.Context, cfg Config) (*Client, error) {
	cfg = cfg.normalize()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	rdb := goredis.NewClient(&goredis.Options{
		Addr:         cfg.Addr,
		Password:     cfg.Password,
		DB:           cfg.DB,
		DialTimeout:  cfg.DialTimeout,
		ReadTimeout:  cfg.OpTimeout,
		WriteTimeout: cfg.OpTimeout,
		PoolTimeout:  cfg.OpTimeout,
	})
	pingCtx, cancel := context.WithTimeout(ctx, cfg.DialTimeout+cfg.OpTimeout)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("连接 Redis %s 失败: %w", cfg.Addr, err)
	}
	return &Client{rdb: rdb}, nil
}

// Close closes the connection pool. Safe on nil.
func (c *Client) Close() error {
	if c == nil || c.rdb == nil {
		return nil
	}
	return c.rdb.Close()
}

// Ping checks that Redis is alive.
func (c *Client) Ping(ctx context.Context) error {
	return c.rdb.Ping(ctx).Err()
}

// Lease is a lease that has been acquired.
type Lease struct {
	client *Client
	key    string
	token  string
}

// renewScript: extend only if value is still ours.
const renewScript = `
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("pexpire", KEYS[1], ARGV[2])
end
return 0
`

// releaseScript: delete only if value is still ours.
const releaseScript = `
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("del", KEYS[1])
end
return 0
`

func (c *Client) evalOne(ctx context.Context, script, key string, args ...any) (bool, error) {
	value, err := c.rdb.Eval(ctx, script, []string{key}, args...).Result()
	if err != nil {
		return false, err
	}
	switch res := value.(type) {
	case int64:
		return res == 1, nil
	case string:
		return res == "1", nil
	default:
		return false, nil
	}
}

// Acquire takes the lock: SET key token NX PX ttl. When key already exists it returns
// (nil, false, nil).
func (c *Client) Acquire(ctx context.Context, key string, ttl time.Duration) (*Lease, bool, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, false, fmt.Errorf("%w: key 不能为空", ErrInvalidConfig)
	}
	if ttl <= 0 {
		return nil, false, fmt.Errorf("%w: ttl 必须为正", ErrInvalidConfig)
	}
	token, err := newToken()
	if err != nil {
		return nil, false, err
	}
	ok, err := c.rdb.SetNX(ctx, key, token, ttl).Result()
	if err != nil {
		return nil, false, fmt.Errorf("抢锁 %s 失败: %w", key, err)
	}
	if !ok {
		return nil, false, nil
	}
	return &Lease{client: c, key: key, token: token}, true, nil
}

// Renew extends the lease. false means the lease is no longer ours (someone took it after it
// expired), and the caller must stop immediately.
func (l *Lease) Renew(ctx context.Context, ttl time.Duration) (bool, error) {
	if l == nil || l.client == nil {
		return false, nil
	}
	if ttl <= 0 {
		return false, fmt.Errorf("%w: ttl 必须为正", ErrInvalidConfig)
	}
	ok, err := l.client.evalOne(ctx, renewScript, l.key, l.token, ttl.Milliseconds())
	if err != nil {
		return false, fmt.Errorf("续期 %s 失败: %w", l.key, err)
	}
	return ok, nil
}

// Release releases the lease. It only deletes our own lock and leaves others' alone.
func (l *Lease) Release(ctx context.Context) error {
	if l == nil || l.client == nil {
		return nil
	}
	if _, err := l.client.evalOne(ctx, releaseScript, l.key, l.token); err != nil {
		return fmt.Errorf("释放 %s 失败: %w", l.key, err)
	}
	return nil
}

func newToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成锁标识失败: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

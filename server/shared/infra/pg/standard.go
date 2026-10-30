// This file handles "how to connect": connection config, DSN building, and pool creation.
//
// The target is standard PostgreSQL: production PG instances and the local docker-compose
// container take the same path. The DSN uses libpq's **key=value** form (not URI), and every
// key is a standard libpq key that pgx consumes directly.
//
// Choosing key=value over URI has a practical benefit: IPv6 addresses need no square-bracket
// escaping (see the StdConfig.Host comment below; that is the easiest trap to fall into in
// this package).
package pg

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	// The blank import registers the database/sql driver "pgx".
	//
	// pgx/v5 is already a direct dependency of this module (errors.go uses its pgconn to check
	// SQLSTATE), so this only pulls in one more subpackage.
	_ "github.com/jackc/pgx/v5/stdlib"
)

// stdDriverName is the database/sql driver name registered by pgx/v5/stdlib.
const stdDriverName = "pgx"

// DSN key names for the standard PG path. All are standard libpq keys consumed directly by pgx.
const (
	keyHost           = "host"
	keyPort           = "port"
	keyDBName         = "dbname"
	keyUser           = "user"
	keyPassword       = "password"
	keySSLMode        = "sslmode"
	keyConnectTimeout = "connect_timeout"

	// These two are PostgreSQL GUCs. pgx sends non-libpq startup parameters to the server as
	// is, and the server applies them for the whole life of that connection (same as running
	// SET once after each connect). Values are millisecond integer strings: GUC time unit
	// syntax works across versions, and plain numbers are the hardest to get wrong.
	keyStatementTimeout = "statement_timeout"
	keyLockTimeout      = "lock_timeout"

	// defaultStdPort is the port used when it is left empty.
	defaultStdPort = 5432

	// defaultSSLMode is the sslmode used when it is left empty.
	//
	// disable is fine on the same machine or a trusted private network. **Over the public
	// internet it must be require or stronger**, otherwise the password crosses the network
	// in plain text.
	defaultSSLMode = "disable"
)

// Common misspelled key names → the correct spelling.
//
// These are all "no error, silently ignored" traps: pgx passes keys it does not know to
// PostgreSQL as startup parameters, so sslmode never takes effect while you think it is set.
// Better to fail locally right away.
var misspelledKeys = map[string]string{
	"ssl_mode": keySSLMode,
	"db_name":  keyDBName,
	"database": keyDBName,
	"username": keyUser,
	"passwd":   keyPassword,
	"hostname": keyHost,
}

// Connection pool defaults.
//
// PostgreSQL's default max_connections is 100 (the local container uses 100); managed
// instances usually allow more depending on size. 25 is the usual value for database/sql,
// leaving plenty of room for other instances and ops tools.
const (
	// stdDefaultMaxOpenConns is the default maximum number of connections.
	stdDefaultMaxOpenConns = 25

	// stdDefaultMaxIdleConns is the default maximum number of idle connections.
	stdDefaultMaxIdleConns = 5

	// stdDefaultConnMaxIdleTime is the maximum lifetime of an idle connection. Do not hold a
	// server backend process for a long time.
	stdDefaultConnMaxIdleTime = 5 * time.Minute

	// stdDefaultConnMaxLifetime is the maximum total lifetime of a connection, so connections
	// get rebuilt periodically.
	//
	// For managed PG this has a practical benefit: after a primary/standby failover, old
	// connections point at the demoted node. Periodic rebuilds let the pool move over on its
	// own, without waiting for business queries to hit errors.
	stdDefaultConnMaxLifetime = 30 * time.Minute

	// stdDefaultStatementTimeout is the default server-side execution limit for each SQL
	// statement.
	//
	// ⚠️ **There must be a default; we cannot count on every caller's ctx having a timeout.**
	// database/sql's ctx only works while "sending the request / waiting for the result". When
	// the server holds the connection (waiting on a row lock, or a query already sent on a
	// half-dead connection), canceling on the client side may not rescue the hop that is
	// already stuck on the server. statement_timeout is enforced by the database itself, so
	// even a long row-lock wait gets interrupted.
	//
	// A past production outage had exactly this shape: a goroutine hung on a write with no
	// statement_timeout, canceling the ctx on failover did not make it return, and after that
	// no tool ran for any later session in the same environment.
	//
	// 10s is enough for this repo's point reads, point writes and single-table paging (P99 in
	// the slow log is in the hundreds of milliseconds). DDL / batch jobs must be exempted
	// explicitly with WithoutQueryTimeouts; do not raise the default for them. The default
	// protects the calls that "forgot to set a timeout".
	stdDefaultStatementTimeout = 10 * time.Second

	// stdDefaultLockTimeout is the limit for waiting on row/table locks, shorter than
	// statement_timeout.
	//
	// It is set separately because lock waits are almost always "ran into an uncommitted
	// transaction upstream", and waiting longer is a waste. Failing fast at 2s is better than
	// waiting the full 10s of statement_timeout. The error is SQLSTATE 55P03
	// (lock_not_available); the caller can just retry.
	stdDefaultLockTimeout = 2 * time.Second
)

// StdConfig is the config needed to connect to standard PostgreSQL.
//
// Local development values are in server/config.example.yaml; in production they come from
// the YAML loaded by shared/infra/config + environment variables.
type StdConfig struct {
	// Host is the host name or IP. Required.
	//
	// **Do not put square brackets around IPv6.** Many docs say "IPv6 addresses in a
	// connection string must be wrapped in square brackets", but that applies only to the URI
	// form of the DSN (postgres://user:pass@[::1]:5432/db). This file builds libpq's key=value
	// form, where brackets are treated as part of the host name:
	//
	//	host=[::1]  → hostname resolving error: lookup [::1]: no such host
	//	host=::1    → connects normally
	//
	// Copying the docs gets it wrong, and this is the easiest trap in this package. So that
	// nobody trips on it, toStdDSN strips leading/trailing brackets: it works even if you add
	// them, but do not expect other libraries to be this forgiving.
	Host string

	// Port is the port. Defaults to 5432 when empty.
	Port int

	// Database is the database name, required, e.g. "ama".
	Database string

	// User is the login user name. Required.
	//
	// It does not need to be a superuser. Make it the database Owner when creating the
	// database, so it can create tables and indexes.
	User string

	// Password is the login password. **It may be empty**: local trust auth, or setups where
	// ~/.pgpass provides the password, do not need it, so validate does not require it.
	//
	// The password is only used when building the DSN and never appears in any error message
	// or log. Use Redacted() for logging.
	Password string

	// SSLMode maps to libpq's sslmode. Defaults to "disable" when empty.
	//
	// disable is fine on the same machine or a trusted private network; over the public
	// internet it must be require or stronger.
	SSLMode string

	// ConnectTimeout is the timeout for one connection attempt. When empty it is not passed
	// down, and the driver/OS decides.
	//
	// Setting it is recommended: an unreachable address (for example, not on the allowlist)
	// otherwise hangs until the OS TCP timeout, tens of seconds before an error comes back,
	// which is painful when debugging locally.
	ConnectTimeout time.Duration

	// MaxOpenConns is the maximum number of connections. Defaults to 25 when empty.
	MaxOpenConns int

	// MaxIdleConns is the maximum number of idle connections. Defaults to 5 when empty.
	MaxIdleConns int

	// ConnMaxIdleTime is the maximum lifetime of an idle connection. Defaults to 5 minutes
	// when empty.
	ConnMaxIdleTime time.Duration

	// ConnMaxLifetime is the maximum total lifetime of a connection. Defaults to 30 minutes
	// when empty.
	ConnMaxLifetime time.Duration

	// StatementTimeout is the server-side execution limit for each SQL statement. Defaults to
	// 10s when empty.
	//
	// This is PostgreSQL's statement_timeout GUC, sent in the connection startup parameters,
	// and it applies to **every connection** in the pool. To exempt a single long task, use
	// Client.WithoutQueryTimeouts.
	//
	// A negative value means explicitly off (sends 0). Normal business code has no reason to
	// turn it off: a stuck query would hold a connection forever and ignore ctx cancel. See
	// the production outage in the constant's comment.
	StatementTimeout time.Duration

	// LockTimeout is the lock wait limit. Defaults to 2s when empty. A negative value also
	// means off.
	LockTimeout time.Duration

	// ExtraParams puts extra libpq key/values straight into the DSN (like application_name,
	// statement_timeout), as an escape hatch for fields this struct does not cover. A key with
	// the same name overrides the fields above.
	//
	// Key names go through misspelledKeys, so forms like ssl_mode that "do not error but are
	// silently ignored" get caught locally.
	ExtraParams map[string]string
}

// hostForDSN returns a host name that can be written directly into a key=value DSN.
//
// Its only job is to strip IPv6 square brackets. See the StdConfig.Host comment for why.
func (c StdConfig) hostForDSN() string {
	h := strings.TrimSpace(c.Host)
	if len(h) >= 2 && strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		return h[1 : len(h)-1]
	}
	return h
}

func (c StdConfig) validate() error {
	if c.hostForDSN() == "" {
		return fmt.Errorf("%w: Host 不能为空", ErrInvalidConfig)
	}
	if strings.TrimSpace(c.Database) == "" {
		return fmt.Errorf("%w: Database 不能为空", ErrInvalidConfig)
	}
	if strings.TrimSpace(c.User) == "" {
		return fmt.Errorf("%w: User 不能为空", ErrInvalidConfig)
	}
	if c.Port < 0 || c.Port > 65535 {
		return fmt.Errorf("%w: Port %d 不在 0~65535 范围内", ErrInvalidConfig, c.Port)
	}
	// Password is intentionally not checked: local trust auth and .pgpass do not need it.
	return nil
}

// Redacted returns a one-line description that is safe to log, with the password shown as ***.
//
// The reason it exists is simple: the raw DSN contains the plain-text password, and "cannot
// connect" is exactly when you most want to log the connection parameters. A safe version is
// provided so nobody takes the shortcut of logging the DSN.
func (c StdConfig) Redacted() string {
	pwd := "(空)"
	if c.Password != "" {
		pwd = "***"
	}
	return fmt.Sprintf(
		"host=%s port=%d dbname=%s user=%s password=%s sslmode=%s",
		c.hostForDSN(), c.portOrDefault(), strings.TrimSpace(c.Database),
		strings.TrimSpace(c.User), pwd, c.sslModeOrDefault(),
	)
}

func (c StdConfig) portOrDefault() int {
	if c.Port <= 0 {
		return defaultStdPort
	}
	return c.Port
}

func (c StdConfig) sslModeOrDefault() string {
	if s := strings.TrimSpace(c.SSLMode); s != "" {
		return s
	}
	return defaultSSLMode
}

// statementTimeoutOrDefault: zero means the default 10s, negative means explicitly off (0).
func (c StdConfig) statementTimeoutOrDefault() time.Duration {
	if c.StatementTimeout < 0 {
		return 0
	}
	if c.StatementTimeout > 0 {
		return c.StatementTimeout
	}
	return stdDefaultStatementTimeout
}

// lockTimeoutOrDefault is the same as above, with a 2s default.
func (c StdConfig) lockTimeoutOrDefault() time.Duration {
	if c.LockTimeout < 0 {
		return 0
	}
	if c.LockTimeout > 0 {
		return c.LockTimeout
	}
	return stdDefaultLockTimeout
}

// DSN builds the config into a libpq key=value DSN, for callers that need to **open their own
// connection**.
//
// ⚠️ It is exported because LISTEN must own a connection exclusively (the pool swaps
// connections, and after a swap notifications never arrive again, with no error), and that
// connection cannot be borrowed from the *Client pool. Callers doing pgx.Connect themselves
// must use **the same** DSN. Building it separately is sure to drift, and the drift is usually
// a missing sslmode or connect_timeout, whose errors do not point at the DSN at all.
//
// ⚠️ **The return value contains the plain-text password.** Do not log it; use Redacted().
func (c StdConfig) DSN() (string, error) { return c.toStdDSN() }

// toStdDSN builds the config into a libpq key=value DSN.
//
// It emits only standard libpq keys. pgx passes keys it does not know to PostgreSQL as
// startup parameters, and then the server reports an error that does not point at the real
// cause at all. That is why ExtraParams keys must pass the misspelledKeys check.
func (c StdConfig) toStdDSN() (string, error) {
	params := map[string]string{
		keyHost:    c.hostForDSN(),
		keyPort:    strconv.Itoa(c.portOrDefault()),
		keyDBName:  strings.TrimSpace(c.Database),
		keyUser:    strings.TrimSpace(c.User),
		keySSLMode: c.sslModeOrDefault(),
	}
	// Do not pass the password when it is empty: password='' makes some auth methods (trust /
	// peer) behave inconsistently.
	if c.Password != "" {
		params[keyPassword] = c.Password
	}
	if c.ConnectTimeout > 0 {
		// libpq's connect_timeout is in seconds, and the smallest effective value is 2; less
		// than 1 second would be treated as 0 (= no timeout), so round up to whole seconds.
		secs := int((c.ConnectTimeout + time.Second - 1) / time.Second)
		params[keyConnectTimeout] = strconv.Itoa(secs)
	}
	// Query/lock timeouts: zero uses the default, negative explicitly sends 0 (off). Set them
	// before ExtraParams so the escape hatch can still override them (the loop below
	// overwrites same-name keys).
	params[keyStatementTimeout] = strconv.FormatInt(c.statementTimeoutOrDefault().Milliseconds(), 10)
	params[keyLockTimeout] = strconv.FormatInt(c.lockTimeoutOrDefault().Milliseconds(), 10)

	for k, v := range c.ExtraParams {
		key := strings.TrimSpace(k)
		if key == "" {
			return "", fmt.Errorf("%w: ExtraParams 的键不能为空", ErrInvalidConfig)
		}
		if right, bad := misspelledKeys[strings.ToLower(key)]; bad {
			return "", fmt.Errorf(
				"%w: ExtraParams 里的 %q 不是有效的 DSN 键，应该写 %q"+
					"（写错不会报错，会被当成 PostgreSQL 启动参数静默失效）",
				ErrInvalidConfig, key, right,
			)
		}
		params[key] = v
	}

	// Sort the keys so the same config always builds the same DSN; otherwise logs and tests
	// are hard to compare.
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(quoteDSNValue(params[k]))
	}
	return b.String(), nil
}

// NewStandard connects to standard PostgreSQL and does one Ping to confirm it really connects.
//
// If Ping fails, the pool is closed too; no half-open Client is left behind.
//
// When it cannot connect, the error includes the Redacted() connection parameters (no
// password). The most common reasons for "cannot connect" are wrong parameters or the network
// not allowing it, and printing the parameters saves a round of debugging.
func NewStandard(ctx context.Context, cfg StdConfig) (*Client, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	dsn, err := cfg.toStdDSN()
	if err != nil {
		return nil, err
	}

	db, err := sql.Open(stdDriverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败（%s）: %w", cfg.Redacted(), err)
	}
	cfg.applyStdPool(db)

	// sql.Open is lazy and does not connect. Only a Ping here tells whether the config is
	// right and the network is reachable.
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("连接数据库失败（%s）: %w", cfg.Redacted(), err)
	}

	// Record the database name: Client.Database() reads it, and db= in error messages relies
	// on it. Also record the two timeout defaults: WithoutQueryTimeouts restores them when it
	// returns its dedicated connection.
	return &Client{
		db:                 db,
		database:           strings.TrimSpace(cfg.Database),
		statementTimeoutMS: cfg.statementTimeoutOrDefault().Milliseconds(),
		lockTimeoutMS:      cfg.lockTimeoutOrDefault().Milliseconds(),
	}, nil
}

// applyStdPool applies the pool settings to *sql.DB, using standard PG defaults for empty ones.
func (c StdConfig) applyStdPool(db *sql.DB) {
	maxOpen := c.MaxOpenConns
	if maxOpen <= 0 {
		maxOpen = stdDefaultMaxOpenConns
	}
	db.SetMaxOpenConns(maxOpen)

	idle := c.MaxIdleConns
	if idle <= 0 {
		idle = stdDefaultMaxIdleConns
	}
	if idle > maxOpen {
		idle = maxOpen
	}
	db.SetMaxIdleConns(idle)

	idleTime := c.ConnMaxIdleTime
	if idleTime <= 0 {
		idleTime = stdDefaultConnMaxIdleTime
	}
	db.SetConnMaxIdleTime(idleTime)

	lifetime := c.ConnMaxLifetime
	if lifetime <= 0 {
		lifetime = stdDefaultConnMaxLifetime
	}
	db.SetConnMaxLifetime(lifetime)
}

// quoteDSNValue quotes a value following libpq's rules.
//
// libpq separates key/value pairs with spaces, so a value containing a space must be wrapped
// in single quotes, with single quotes and backslashes each escaped. Passwords and
// application_name may well contain these characters; without this the DSN would have a
// syntax error. Empty values must be quoted too, otherwise `key=` followed directly by the
// next key would be parsed as one blob.
func quoteDSNValue(v string) string {
	if v == "" {
		return "''"
	}
	if !strings.ContainsAny(v, " '\\\t\n") {
		return v
	}
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range v {
		if r == '\'' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('\'')
	return b.String()
}

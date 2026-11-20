// Package datasources holds the worker-side datasource tools: query_sql (a read-only MySQL query)
// and read_object (reads one UTF-8 text object from TOS, which speaks the S3 protocol).
//
// # Why these live on the worker side
//
// Both need credentials. Putting them on the agent side means the credentials go into the session
// container and into the Agent config: the agent can `curl` them away, and plaintext copies end up
// in the container image, the Agent definition and the session environment. Keeping them in amad
// (the worker) means the model only ever sees two things it can call: **an account that may only
// SELECT** and **an object store prefix it may only read**. There is nothing to escalate with.
//
// Two things come for free with that placement:
//   - every query is auditable: who (session / work) asked what, how many rows, how long, in the
//     amad log;
//   - the limits are decided by the server: row count, byte count and timeout cannot be raised by
//     model-supplied parameters (max_rows / max_bytes can only lower them).
//
// # Declarations and execution come from one source
//
// Registry produces both Declarations (the Ark-side tool declarations) and ToolsFor (the worker-side
// implementations). Both use the same Registry, the same config and the same name constants, so
// "declared but not implemented" (or the other way round) cannot happen by construction; a test
// also asserts that the set of declared Names equals the set of ToolsFor map keys.
//
// Dependency direction: config → datasources (config only converts the YAML struct into this
// package's Config). This package imports neither config nor core; its only in-repo dependency is
// shared/infra/ma (the declaration struct).
package datasources

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/volcengine/ark-runtime-go/arkruntime/toolset"

	"github.com/chyroc/ama/server/shared/infra/ma"
)

// Config is the complete configuration of the worker-side datasource tools.
//
// The zero value means no source is enabled, hence no custom tool is declared, and behavior is
// exactly "this feature does not exist".
type Config struct {
	MySQL MySQLConfig
	TOS   TOSConfig
	// Redact is an optional substring replacement map applied to text returned by query_sql and
	// read_object (for example masking customer or tenant names in shared datasets). Empty leaves
	// query and object text untouched.
	Redact map[string]string
}

func buildRedactor(m map[string]string) *strings.Replacer {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		if k != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) > len(keys[j])
		}
		return keys[i] < keys[j]
	})
	pairs := make([]string, 0, len(keys)*2)
	for _, k := range keys {
		pairs = append(pairs, k, m[k])
	}
	return strings.NewReplacer(pairs...)
}

func applyRedactor(r *strings.Replacer, s string) string {
	if r == nil || s == "" {
		return s
	}
	return r.Replace(s)
}

// Enabled reports whether at least one source is enabled.
func (c Config) Enabled() bool { return c.MySQL.Enabled || c.TOS.Enabled }

// Validate checks the required fields of the enabled sections; fields under disabled sections are
// left alone.
//
// The config package calls it at startup and datasources.New calls it again: the former wraps the
// error in ErrInvalidConfig, the latter guarantees that a caller constructing a Registry directly
// cannot start from an invalid config either.
func (c Config) Validate() error {
	if err := c.MySQL.validate(); err != nil {
		return err
	}
	if err := c.TOS.validate(); err != nil {
		return err
	}
	return nil
}

// sourceDeps lets tests replace the two real clients (the offline unit tests never connect to RDS
// or TOS). The zero value means "use the real implementation".
type sourceDeps struct {
	newMySQL func(MySQLConfig) (sqlConnector, error)
	newTOS   func(TOSConfig) (objectStore, error)
}

// Registry provides both "declare" and "execute".
//
// Its lifetime is the amad process: New builds it and probes each source once, after which it is
// read-only; Close releases it. No locking is needed: the fields are filled once in New and only
// read afterwards.
type Registry struct {
	mysql  *mysqlSource
	tos    *tosSource
	logger *slog.Logger
}

// New builds the enabled sources.
//
// Returning (nil, nil) means "no source is enabled": the caller should pass a **bare nil** to
// core.Config.CustomTools. Do not return an empty Registry and pass that along instead: a nil
// pointer of a concrete type stored in an interface does not compare equal to nil, so
// `s.customTools != nil` would be true and the methods would run on a nil receiver.
//
// An invalid config (enabled but missing fields) returns an error and the caller should fail
// startup: silently having one tool fewer is far more dangerous than failing to start.
//
// A source that merely **cannot be reached** only logs a Warn and does not block startup: an RDS
// allowlist or a TOS policy may be temporarily wrong, and the process should still come up (every
// other feature is unaffected); tool calls then return a clear error.
func New(cfg Config, logger *slog.Logger) (*Registry, error) {
	return newRegistry(cfg, logger, sourceDeps{})
}

// newRegistry is New's implementation with replaceable client factories (for tests).
func newRegistry(cfg Config, logger *slog.Logger, deps sourceDeps) (*Registry, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	if !cfg.Enabled() {
		return nil, nil
	}

	redactor := buildRedactor(cfg.Redact)
	r := &Registry{logger: logger}
	if cfg.MySQL.Enabled {
		src, err := buildMySQLSource(cfg.MySQL, redactor, logger, deps)
		if err != nil {
			return nil, err
		}
		r.mysql = src
	}
	if cfg.TOS.Enabled {
		src, err := buildTOSSource(cfg.TOS, redactor, logger, deps)
		if err != nil {
			// A MySQL pool may already have been built; do not leak it.
			_ = r.Close()
			return nil, err
		}
		r.tos = src
	}
	return r, nil
}

// Declarations returns the Ark-side tool declarations, only for the enabled sources.
//
// nil means "no custom tool": core then gets spec.CustomTools == nil, exactly matching the Ark
// Agent from before this feature (an empty array would mean "clear existing tools", which is a
// different thing).
func (r *Registry) Declarations() []ma.CustomToolSpec {
	if r == nil {
		return nil
	}
	out := make([]ma.CustomToolSpec, 0, 2)
	if r.mysql != nil {
		out = append(out, r.mysql.declaration())
	}
	if r.tos != nil {
		out = append(out, r.tos.declaration())
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ToolsFor returns the tool implementations for this work item; the keys must exactly match the
// Names from Declarations.
//
// A fresh map is built per work item: the tool closures carry sessionID / workID so audit lines can
// name their session. The SDK's CustomTools is a process-level map, so building a new one each time
// sidesteps that limitation (see core's customToolsForWork).
func (r *Registry) ToolsFor(sessionID, workID string, logger *slog.Logger) map[string]toolset.Tool {
	if r == nil {
		return nil
	}
	if logger == nil {
		logger = r.logger
	}
	if logger == nil {
		logger = slog.Default()
	}
	m := make(map[string]toolset.Tool, 2)
	if r.mysql != nil {
		t := r.mysql.tool(sessionID, workID, logger)
		m[t.Name()] = t
	}
	if r.tos != nil {
		t := r.tos.tool(sessionID, workID, logger)
		m[t.Name()] = t
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// Close releases resources such as the connection pool. It is safe to call more than once.
func (r *Registry) Close() error {
	if r == nil {
		return nil
	}
	var errs []error
	if r.mysql != nil && r.mysql.conn != nil {
		if err := r.mysql.conn.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// buildMySQLSource builds the MySQL source and probes it once at startup (SHOW TABLES).
func buildMySQLSource(cfg MySQLConfig, redactor *strings.Replacer, logger *slog.Logger, deps sourceDeps) (*mysqlSource, error) {
	factory := deps.newMySQL
	if factory == nil {
		factory = openMySQLConnector
	}
	conn, err := factory(cfg)
	if err != nil {
		return nil, fmt.Errorf("MySQL 数据源初始化失败: %w", err)
	}
	src := &mysqlSource{cfg: cfg, conn: conn, redactor: redactor}
	// The table list only exists to make the tool description more useful; a failed probe does not
	// affect the tool itself (the model can still run SHOW TABLES).
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	tables, err := src.probeTables(ctx)
	if err != nil {
		logger.Warn("数据源 MySQL 启动探测失败（工具仍可用，只是说明里没有表清单）",
			slog.String("database", cfg.Database), slog.String("err", redactSecret(err.Error(), cfg.Password)))
		return src, nil
	}
	src.tables = tables
	if len(tables) > 0 {
		if cols, colErr := src.probeTableColumns(ctx); colErr != nil {
			logger.Warn("数据源 MySQL 启动列名探测失败（保留表名清单）",
				slog.String("database", cfg.Database), slog.String("err", redactSecret(colErr.Error(), cfg.Password)))
		} else if len(cols) > 0 {
			src.columns = cols
		}
	}
	logger.Info("数据源 MySQL 已装配",
		slog.String("database", cfg.Database),
		slog.Int("表数量", len(tables)),
		slog.Int("带列信息表数", len(src.columns)),
		slog.Int("max_rows", cfg.maxRows()),
		slog.Duration("timeout", cfg.timeout()))
	return src, nil
}

// buildTOSSource builds the TOS source and probes it once at startup (ListObjectsV2).
func buildTOSSource(cfg TOSConfig, redactor *strings.Replacer, logger *slog.Logger, deps sourceDeps) (*tosSource, error) {
	factory := deps.newTOS
	if factory == nil {
		factory = openS3ObjectStore
	}
	obj, err := factory(cfg)
	if err != nil {
		return nil, fmt.Errorf("TOS 数据源初始化失败: %w", err)
	}
	src := &tosSource{cfg: cfg, obj: obj, redactor: redactor}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	keys, err := obj.ListKeys(ctx, cfg.Bucket, cfg.prefix(), maxProbeKeys)
	if err != nil {
		logger.Warn("数据源 TOS 启动探测失败（工具仍可用，只是说明里没有 key 清单）",
			slog.String("bucket", cfg.Bucket),
			slog.String("prefix", cfg.prefix()),
			slog.String("err", redactSecret(err.Error(), cfg.SecretKey)))
		return src, nil
	}
	src.keys = keys
	logger.Info("数据源 TOS 已装配",
		slog.String("bucket", cfg.Bucket),
		slog.String("prefix", cfg.prefix()),
		slog.Int("对象数量", len(keys)),
		slog.Int64("max_bytes", cfg.maxBytes()),
		slog.Duration("timeout", cfg.timeout()))
	return src, nil
}

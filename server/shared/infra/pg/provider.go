package pg

import "context"

// Connector opens a database pool and verifies that it can be used. Implementations may
// use a different driver, credential source, or connection pool without changing callers
// that only need Client's query and transaction methods.
type Connector interface {
	Connect(ctx context.Context, cfg StdConfig) (*Client, error)
}

// ConnectorFunc adapts a function to Connector.
type ConnectorFunc func(ctx context.Context, cfg StdConfig) (*Client, error)

// Connect calls f(ctx, cfg).
func (f ConnectorFunc) Connect(ctx context.Context, cfg StdConfig) (*Client, error) {
	return f(ctx, cfg)
}

// StandardConnector connects with pgx through database/sql.
type StandardConnector struct{}

// Connect opens and pings the standard PostgreSQL pool.
func (StandardConnector) Connect(ctx context.Context, cfg StdConfig) (*Client, error) {
	return newStandard(ctx, cfg)
}

// NewStandardWith uses connector to open a PostgreSQL pool. A nil connector selects
// StandardConnector, keeping the standard database/sql path as the default.
func NewStandardWith(ctx context.Context, cfg StdConfig, connector Connector) (*Client, error) {
	if connector == nil {
		connector = StandardConnector{}
	}
	return connector.Connect(ctx, cfg)
}

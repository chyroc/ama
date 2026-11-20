package pg

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"
)

// This example shows how a downstream module replaces connection creation while retaining
// AMA's query, transaction, and SQLSTATE error handling. It implements Connector and uses
// NewWithDB so the caller does not depend on the downstream driver's pool type.
func ExampleNewStandardWith() {
	// type customConnector struct{ pool *sql.DB }
	// func (c customConnector) Connect(_ context.Context, _ StdConfig) (*Client, error) {
	//     return NewWithDB(c.pool)
	// }
	//
	// client, err := NewStandardWith(ctx, cfg, customConnector{pool: pool})
	// The caller still uses client.WithTx and IsUniqueViolation exactly as with NewStandard.
}

func TestNewStandardWithUsesInjectedConnector(t *testing.T) {
	db, err := sql.Open("pgconnectortest", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	want, err := NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	var gotCfg StdConfig
	got, err := NewStandardWith(context.Background(), StdConfig{Database: "example"},
		ConnectorFunc(func(_ context.Context, cfg StdConfig) (*Client, error) {
			gotCfg = cfg
			return want, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatal("NewStandardWith must return the pool created by the injected connector")
	}
	if gotCfg.Database != "example" {
		t.Fatalf("connector got database %q, want example", gotCfg.Database)
	}
}

func TestNewStandardWithPropagatesConnectorError(t *testing.T) {
	want := errors.New("credentials unavailable")
	_, err := NewStandardWith(context.Background(), StdConfig{},
		ConnectorFunc(func(context.Context, StdConfig) (*Client, error) { return nil, want }))
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want the injected connector's error", err)
	}
}

func TestNewStandardWithNilUsesStandardConnector(t *testing.T) {
	_, err := NewStandardWith(context.Background(), StdConfig{}, nil)
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("got %v, want standard configuration validation", err)
	}
}

type connectorTestDriver struct{}

func (connectorTestDriver) Open(string) (driver.Conn, error) {
	return connectorTestConn{}, nil
}

type connectorTestConn struct{}

func (connectorTestConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (connectorTestConn) Close() error                        { return nil }
func (connectorTestConn) Begin() (driver.Tx, error)           { return nil, errors.New("unused") }

func init() { sql.Register("pgconnectortest", connectorTestDriver{}) }

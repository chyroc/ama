package company

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"

	"github.com/chyroc/ama/server/shared/infra/pg"
)

type documents map[string]string

func (d documents) Documents(context.Context) ([]byte, []byte, error) {
	return []byte(d["public"]), []byte(d["secret"]), nil
}

func TestDownstreamConfigRetainsPrivateFieldsAndLayers(t *testing.T) {
	store := documents{
		"public": "database:\n  host: db.example.com\n  port: 5432\n  database: ama\n  user: app\ninternalFeatureFlag: true\n",
		"secret": "database:\n  password: example-secret\n",
	}
	cfg, err := LoadConfig(context.Background(), NewConfigSource(store))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.Host != "db.example.com" || cfg.Database.Password != "example-secret" {
		t.Fatalf("config layers were not merged: %+v", cfg.Database)
	}
	if !cfg.InternalFeatureFlag {
		t.Fatal("downstream-only field must survive public-source loading")
	}
}

func TestConnectorUsesDownstreamPool(t *testing.T) {
	db, err := sql.Open("companyexample", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	client, err := NewConnector(func(context.Context, pg.StdConfig) (*sql.DB, error) {
		return db, nil
	}).Connect(context.Background(), pg.StdConfig{Database: "example"})
	if err != nil {
		t.Fatal(err)
	}
	if client.Raw() != db {
		t.Fatal("public client must expose the downstream pool")
	}
}

func TestConnectorPingFailureClosesPool(t *testing.T) {
	pingErr = errors.New("example ping failure")
	t.Cleanup(func() { pingErr = nil })
	db, err := sql.Open("companyexample", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewConnector(func(context.Context, pg.StdConfig) (*sql.DB, error) {
		return db, nil
	}).Connect(context.Background(), pg.StdConfig{})
	if err == nil {
		t.Fatal("ping failure must be returned")
	}
	if !closedByClient {
		t.Fatal("failed Ping must close the downstream pool")
	}
}

var (
	pingErr        error
	closedByClient bool
)

type exampleDriver struct{}

func (exampleDriver) Open(string) (driver.Conn, error) { return exampleConn{}, nil }

type exampleConn struct{}

func (exampleConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (exampleConn) Close() error {
	closedByClient = true
	return nil
}
func (exampleConn) Begin() (driver.Tx, error)  { return nil, errors.New("unused") }
func (exampleConn) Ping(context.Context) error { return pingErr }

func init() { sql.Register("companyexample", exampleDriver{}) }

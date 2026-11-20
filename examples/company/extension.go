// Package company shows how a downstream distribution supplies its own configuration source
// and database connection while reusing AMA's public packages. It uses only example values and
// an in-memory configuration service; there is no company-specific protocol here.
package company

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/chyroc/ama/server/shared/infra/configsource"
	"github.com/chyroc/ama/server/shared/infra/pg"
	"gopkg.in/yaml.v3"
)

// Config belongs to the downstream module. It may contain fields AMA does not know about;
// those fields do not belong in the public config schema.
type Config struct {
	Database struct {
		Host     string `yaml:"host"`
		Port     int    `yaml:"port"`
		Database string `yaml:"database"`
		User     string `yaml:"user"`
		Password string `yaml:"password"`
	} `yaml:"database"`
	InternalFeatureFlag bool `yaml:"internalFeatureFlag"`
}

// DocumentStore is the narrow dependency needed by the configuration source. A real downstream
// module can implement it with its own configuration service; the example uses a map.
type DocumentStore interface {
	Documents(ctx context.Context) (public, secret []byte, err error)
}

// NewConfigSource returns the public Provider implemented with a downstream document store.
func NewConfigSource(store DocumentStore) configsource.Provider {
	return configsource.ProviderFunc("company-config", func(ctx context.Context) ([][]byte, error) {
		if store == nil {
			return nil, fmt.Errorf("company config store is required")
		}
		public, secret, err := store.Documents(ctx)
		if err != nil {
			return nil, fmt.Errorf("load company config: %w", err)
		}
		// Public settings come first, secrets second. The downstream decoder retains fields
		// omitted by the second layer instead of replacing the whole section.
		return [][]byte{public, secret}, nil
	})
}

// LoadConfig demonstrates decoding provider layers into the downstream Config, not AMA's Config.
// Environment overrides, validation, and unknown-field policy remain the downstream's choice.
func LoadConfig(ctx context.Context, provider configsource.Provider) (Config, error) {
	var cfg Config
	if provider == nil {
		return cfg, fmt.Errorf("config provider is required")
	}
	layers, err := provider.Load(ctx)
	if err != nil {
		return cfg, err
	}
	for _, layer := range layers {
		if err := yaml.Unmarshal(layer, &cfg); err != nil {
			return cfg, fmt.Errorf("decode %s: %w", provider.Name(), err)
		}
	}
	return cfg, nil
}

// DBOpener is the downstream's own pool factory. It can use any driver or credential mechanism.
type DBOpener func(ctx context.Context, cfg pg.StdConfig) (*sql.DB, error)

// NewConnector adapts a downstream pool factory to AMA's Connector. Queries, transactions, and
// SQLSTATE error handling stay in the public pg package.
func NewConnector(open DBOpener) pg.Connector {
	return pg.ConnectorFunc(func(ctx context.Context, cfg pg.StdConfig) (*pg.Client, error) {
		if open == nil {
			return nil, fmt.Errorf("database opener is required")
		}
		db, err := open(ctx, cfg)
		if err != nil {
			return nil, err
		}
		client, err := pg.NewWithDB(db)
		if err != nil {
			_ = db.Close()
			return nil, err
		}
		if err := client.Ping(ctx); err != nil {
			_ = client.Close()
			return nil, err
		}
		return client, nil
	})
}

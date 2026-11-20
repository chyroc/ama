// Package configsource reads YAML configuration without depending on AMA's complete
// configuration schema or its storage and login integrations. A downstream module can
// implement Provider and decode the layers into its own configuration type.
package configsource

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// Provider retrieves YAML configuration layers. Later layers override fields present in
// earlier layers; fields not mentioned keep their earlier values. Implementations can read a
// file, a configuration service, or several secret and non-secret documents.
type Provider interface {
	// Name identifies the source for startup logs. It must not contain credentials.
	Name() string
	Load(ctx context.Context) ([][]byte, error)
}

// ProviderFunc adapts a function to Provider. name is intended for logs and must not contain
// credentials.
func ProviderFunc(name string, load func(context.Context) ([][]byte, error)) Provider {
	return namedProvider{name: name, load: load}
}

type namedProvider struct {
	name string
	load func(context.Context) ([][]byte, error)
}

func (p namedProvider) Name() string { return p.name }
func (p namedProvider) Load(ctx context.Context) ([][]byte, error) {
	if p.load == nil {
		return nil, fmt.Errorf("config: provider %q has no load function", p.name)
	}
	return p.load(ctx)
}

// EnvConfigPath is the environment variable for the default config file path.
const EnvConfigPath = "AMA_CONFIG"

// DefaultConfigPath is the file read when AMA_CONFIG is not set.
const DefaultConfigPath = "config.yaml"

// Sources describes the default file source. The zero value reads DefaultConfigPath.
type Sources struct {
	Path string
}

// SourcesFromEnv reads the default file path from the environment.
func SourcesFromEnv() Sources {
	return Sources{Path: PathOr(os.Getenv(EnvConfigPath))}
}

// FileProvider returns a provider for one YAML file.
func FileProvider(path string) Provider {
	return ProviderFunc("file:"+PathOr(path), func(context.Context) ([][]byte, error) {
		raw, err := os.ReadFile(PathOr(path))
		if err != nil {
			return nil, err
		}
		return [][]byte{raw}, nil
	})
}

// PathOr returns path when non-blank, otherwise DefaultConfigPath.
func PathOr(path string) string {
	if v := strings.TrimSpace(path); v != "" {
		return v
	}
	return DefaultConfigPath
}

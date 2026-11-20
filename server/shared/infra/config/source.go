package config

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

// EnvConfigPath is the environment variable for the config file path.
//
// **The config of the config source itself is only read from environment variables**: you must
// know where to get the config before you can get it. Putting this item in the config file would
// be a chicken-and-egg problem.
const EnvConfigPath = "AMA_CONFIG"

// DefaultConfigPath is the file read when AMA_CONFIG is not set.
const DefaultConfigPath = "config.yaml"

// Sources describes the default file source. The zero value reads DefaultConfigPath.
type Sources struct {
	// Path is the config file path; empty means config.yaml.
	Path string
}

// SourcesFromEnv reads the config source settings from environment variables.
func SourcesFromEnv() Sources {
	return Sources{Path: pathOr(os.Getenv(EnvConfigPath))}
}

// FileProvider returns a provider for one YAML file.
func FileProvider(path string) Provider {
	return ProviderFunc("file:"+pathOr(path), func(context.Context) ([][]byte, error) {
		raw, err := os.ReadFile(pathOr(path))
		if err != nil {
			return nil, err
		}
		return [][]byte{raw}, nil
	})
}

// LoadFrom reads the default file source, applies environment variable overrides and validates.
func LoadFrom(s Sources) (*Result, error) {
	return LoadFile(pathOr(s.Path))
}

// LoadFromProvider loads configuration through provider, applies environment variable overrides
// and validates. A provider error is returned unchanged; there is deliberately no fallback to a
// local file, which could silently start the service with different settings.
func LoadFromProvider(ctx context.Context, provider Provider) (*Result, error) {
	if provider == nil {
		return nil, fmt.Errorf("config: provider must not be nil")
	}
	layers, err := provider.Load(ctx)
	if err != nil {
		return nil, err
	}
	return ParseLayers(provider.Name(), layers...)
}

func pathOr(path string) string {
	if v := strings.TrimSpace(path); v != "" {
		return v
	}
	return DefaultConfigPath
}

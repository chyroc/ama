package config

import (
	"context"
	"fmt"

	"github.com/chyroc/ama/server/shared/infra/configsource"
)

// Provider retrieves YAML configuration layers. Its definition lives in the lightweight
// configsource package so downstream distributions do not inherit the full config schema's
// storage, login, and datasource dependencies just to replace the source.
type Provider = configsource.Provider

// ProviderFunc adapts a function to Provider.
var ProviderFunc = configsource.ProviderFunc

// EnvConfigPath is the environment variable for the config file path.
const EnvConfigPath = configsource.EnvConfigPath

// DefaultConfigPath is the file read when AMA_CONFIG is not set.
const DefaultConfigPath = configsource.DefaultConfigPath

// Sources describes the default file source. The zero value reads DefaultConfigPath.
type Sources = configsource.Sources

// SourcesFromEnv reads the config source settings from environment variables.
var SourcesFromEnv = configsource.SourcesFromEnv

// FileProvider returns a provider for one YAML file.
var FileProvider = configsource.FileProvider

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

func pathOr(path string) string { return configsource.PathOr(path) }

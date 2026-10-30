package config

import (
	"os"
	"strings"
)

// EnvConfigPath is the environment variable for the config file path.
//
// **The config of the config source itself is only read from environment variables**: you must
// know where to get the config before you can get it. Putting this item in the config file would
// be a chicken-and-egg problem.
const EnvConfigPath = "AMA_CONFIG"

// DefaultConfigPath is the file read when AMA_CONFIG is not set.
const DefaultConfigPath = "config.yaml"

// Sources describes where to get the config. The zero value reads DefaultConfigPath.
type Sources struct {
	// Path is the config file path; empty means config.yaml.
	Path string
}

// SourcesFromEnv reads the config source settings from environment variables.
func SourcesFromEnv() Sources {
	return Sources{Path: pathOr(os.Getenv(EnvConfigPath))}
}

// LoadFrom reads the config file, applies environment variable overrides and validates.
//
// This is the entry point cmd/amad should call. The returned Result carries the source and
// "which fields were overridden by environment variables", and callers **must** print them in the
// startup log; otherwise "I changed the file but it did not take effect" becomes a pitfall with no
// error.
func LoadFrom(s Sources) (*Result, error) {
	return LoadFile(pathOr(s.Path))
}

func pathOr(path string) string {
	if v := strings.TrimSpace(path); v != "" {
		return v
	}
	return DefaultConfigPath
}

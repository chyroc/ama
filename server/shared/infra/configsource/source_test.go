package configsource

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFileProviderReadsOneLayer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("pg:\n  host: db.example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := FileProvider(path)
	layers, err := provider.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if provider.Name() != "file:"+path || len(layers) != 1 || string(layers[0]) != "pg:\n  host: db.example.com\n" {
		t.Fatalf("unexpected source %q or layers %q", provider.Name(), layers)
	}
}

func TestSourcesFromEnvUsesExplicitPath(t *testing.T) {
	t.Setenv(EnvConfigPath, " /path/to/ama.yaml ")
	if got := SourcesFromEnv().Path; got != "/path/to/ama.yaml" {
		t.Fatalf("path = %q, want trimmed explicit path", got)
	}
	t.Setenv(EnvConfigPath, "")
	if got := SourcesFromEnv().Path; got != DefaultConfigPath {
		t.Fatalf("path = %q, want default", got)
	}
}

func TestProviderFuncRejectsMissingLoad(t *testing.T) {
	if _, err := ProviderFunc("example", nil).Load(context.Background()); err == nil {
		t.Fatal("nil load function must be rejected")
	}
}

func TestFileProviderReturnsReadError(t *testing.T) {
	_, err := FileProvider(filepath.Join(t.TempDir(), "missing.yaml")).Load(context.Background())
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("got %v, want os.ErrNotExist", err)
	}
}

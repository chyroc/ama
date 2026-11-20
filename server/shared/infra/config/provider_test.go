package config

import (
	"context"
	"errors"
	"os"
	"slices"
	"testing"
)

func TestLoadFromProviderMergesLayers(t *testing.T) {
	provider := ProviderFunc("example", func(context.Context) ([][]byte, error) {
		return [][]byte{
			[]byte("pg:\n  host: db.example.com\n  database: ama\n  user: ama\n"),
			[]byte("pg:\n  password: secret\n"),
		}, nil
	})
	res, err := LoadFromProvider(context.Background(), provider)
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != "example" {
		t.Fatalf("source = %q, want example", res.Source)
	}
	if res.Config.PG.Host != "db.example.com" || res.Config.PG.Password != "secret" {
		t.Fatalf("layers were not merged: %+v", res.Config.PG)
	}
}

func TestLoadFromProviderPreservesEnvironmentPriority(t *testing.T) {
	t.Setenv(EnvPGHost, "env-db.example.com")
	provider := ProviderFunc("example", func(context.Context) ([][]byte, error) {
		return [][]byte{[]byte(minimalYAML)}, nil
	})
	res, err := LoadFromProvider(context.Background(), provider)
	if err != nil {
		t.Fatal(err)
	}
	if res.Config.PG.Host != "env-db.example.com" {
		t.Fatalf("host = %q, want environment override", res.Config.PG.Host)
	}
	if !slices.Contains(res.EnvOverridden, "pg.host") {
		t.Fatalf("overridden fields = %v, want pg.host", res.EnvOverridden)
	}
}

func TestLoadFromProviderDoesNotFallBack(t *testing.T) {
	want := errors.New("configuration service unavailable")
	provider := ProviderFunc("example", func(context.Context) ([][]byte, error) {
		return nil, want
	})
	if _, err := LoadFromProvider(context.Background(), provider); !errors.Is(err, want) {
		t.Fatalf("got %v, want provider error", err)
	}
}

func TestLoadFromProviderRejectsNil(t *testing.T) {
	if _, err := LoadFromProvider(context.Background(), nil); err == nil {
		t.Fatal("nil provider must be rejected")
	}
	if _, err := ProviderFunc("example", nil).Load(context.Background()); err == nil {
		t.Fatal("nil load function must be rejected")
	}
}

func TestFileProviderReadsFile(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(path, []byte(minimalYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := LoadFromProvider(context.Background(), FileProvider(path))
	if err != nil {
		t.Fatal(err)
	}
	if res.Config.PG.Host != "127.0.0.1" {
		t.Fatalf("host = %q, want 127.0.0.1", res.Config.PG.Host)
	}
}

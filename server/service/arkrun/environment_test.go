package arkrun

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
)

func TestEnsureEnvironmentPutsSelectedTypeOnTheWire(t *testing.T) {
	for _, tc := range []struct {
		envType        string
		wantNetworking bool
	}{
		{apimodel.ArkEnvironmentSelfHosted, false},
		{apimodel.ArkEnvironmentCloud, true},
	} {
		t.Run(tc.envType, func(t *testing.T) {
			var creates []map[string]any
			mux := http.NewServeMux()
			mux.HandleFunc("/environments", func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				creates = append(creates, body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"env-selected","name":"selected","type":"environment","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","config":{"type":"` + tc.envType + `"}}`))
			})
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)
			runner := NewLive()
			runner.BaseURLOverride = srv.URL

			got, err := runner.EnsureEnvironment(context.Background(), testArkKey(), "selected", tc.envType)
			if err != nil {
				t.Fatal(err)
			}
			if got != "env-selected" || len(creates) != 1 {
				t.Fatalf("environment=%q create calls=%d", got, len(creates))
			}
			config, _ := creates[0]["config"].(map[string]any)
			if config["type"] != tc.envType {
				t.Fatalf("wire config=%v, want type %q", config, tc.envType)
			}
			_, hasNetworking := config["networking"]
			if hasNetworking != tc.wantNetworking {
				t.Fatalf("networking present=%v, want %v", hasNetworking, tc.wantNetworking)
			}
		})
	}
}

func TestEnsureEnvironmentRejectsUnexpectedType(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/environments/env-wrong", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"env-wrong","name":"wrong","type":"environment","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","config":{"type":"cloud"}}`))
	})
	mux.HandleFunc("/environments", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"env-wrong","name":"wrong","type":"environment","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","config":{"type":"cloud"}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	runner := NewLive()
	runner.BaseURLOverride = srv.URL
	if _, err := runner.EnsureEnvironment(context.Background(), testArkKey(), "wrong", apimodel.ArkEnvironmentSelfHosted); err == nil {
		t.Fatal("expected a mismatch to be rejected instead of silently running tools in cloud")
	}
	if _, err := runner.EnsureEnvironment(context.Background(), testArkKey(), "invalid", "other"); err == nil {
		t.Fatal("expected an invalid type to be rejected")
	}
}

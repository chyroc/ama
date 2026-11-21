package core

import (
	"context"
	"errors"
	"testing"
)

func TestInjectSessionCredentialsHonorsGrantsAndPreservesPartialState(t *testing.T) {
	called := make([]string, 0)
	values := map[string]string{}
	want := errors.New("credential unavailable")
	sources := []SessionCredentialSource{
		{ID: "enabled", Inject: func(context.Context) error {
			called = append(called, "enabled")
			values["FIRST"] = "already computed"
			return nil
		}},
		{ID: "not-granted", Inject: func(context.Context) error {
			t.Fatal("ungranted source must not be called")
			return nil
		}},
		{ID: "failing", Inject: func(context.Context) error {
			called = append(called, "failing")
			values["SECOND"] = "also computed"
			return want
		}},
		{ID: "after-failure", Inject: func(context.Context) error {
			t.Fatal("later sources must not run after a failure")
			return nil
		}},
	}
	if err := InjectSessionCredentials(context.Background(), []string{"enabled", "failing"}, sources); !errors.Is(err, want) {
		t.Fatalf("error = %v, want source failure", err)
	}
	if len(called) != 2 || values["FIRST"] == "" || values["SECOND"] == "" {
		t.Fatalf("partial values were discarded: called=%v values=%v", called, values)
	}
	if IntegrationEnabled(nil, "enabled") {
		t.Fatal("empty grants must not enable credentials")
	}
}

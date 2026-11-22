package agent

import (
	"errors"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

func TestArkEnvironmentTypeValidation(t *testing.T) {
	valid := apimodel.AgentDraft{
		Name: "cloud", Model: apimodel.ModelConfig{ID: "model"},
		EnvironmentType: apimodel.ArkEnvironmentCloud,
	}
	if err := validateAgentDraft(valid); err != nil {
		t.Fatal(err)
	}
	valid.EnvironmentType = "other"
	if err := validateAgentDraft(valid); !errors.Is(err, persistmodel.ErrInvalidArgument) {
		t.Fatalf("unknown environment type must be rejected: %v", err)
	}
	valid.EnvironmentType = apimodel.ArkEnvironmentCloud
	valid.ExecutorType = "host"
	valid.HostID = "hst-not-applicable"
	if err := validateAgentDraft(valid); err != nil {
		t.Fatalf("cloud must discard stale host fields before validation: %v", err)
	}
	if normalizedDraftExecutor(valid).ExecutorType != "" {
		t.Fatal("cloud host choice was not discarded")
	}
	valid = apimodel.AgentDraft{
		Name: "bad", Model: apimodel.ModelConfig{ID: "model"}, ExecutorType: "unsupported",
	}
	if err := validateAgentDraft(valid); !errors.Is(err, persistmodel.ErrInvalidArgument) ||
		!strings.Contains(err.Error(), "执行端类型") {
		t.Fatalf("unsupported executor type must be rejected: %v", err)
	}
}

func TestArkEnvironmentTypeCannotChange(t *testing.T) {
	a := apimodel.Agent{EnvironmentType: apimodel.ArkEnvironmentSelfHosted}
	got := Snapshot(a)
	if got.EnvironmentType != apimodel.ArkEnvironmentSelfHosted {
		t.Fatalf("snapshot type=%q", got.EnvironmentType)
	}
	if !sameAgentDraft(got, apimodel.AgentDraft{EnvironmentType: ""}) {
		t.Fatal("old clients omitting the field must keep self_hosted unchanged")
	}
	a.EnvironmentType = apimodel.ArkEnvironmentCloud
	if Snapshot(a).EnvironmentType != apimodel.ArkEnvironmentCloud {
		t.Fatal("cloud must survive snapshots")
	}
	unchanged := Snapshot(a)
	unchanged.EnvironmentType = ""
	if !sameAgentDraft(Snapshot(a), unchanged) {
		t.Fatal("old clients omitting cloud must not turn it into self_hosted")
	}
	// The rejection of an explicit cloud -> self_hosted change is exercised
	// through Service.UpdateAgent in core/arkenvironment_test.go.
}

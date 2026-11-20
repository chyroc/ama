package shelltools

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type execEnvSourceFunc struct {
	calls int
	envs  []ExecEnv
	err   error
}

func (s *execEnvSourceFunc) ExecEnvs(context.Context, string) ([]ExecEnv, error) {
	s.calls++
	return s.envs, s.err
}

func TestExecEnvSourceWinsAndScrubsEveryResultField(t *testing.T) {
	const token = "example-session-token-1234567890"
	source := &execEnvSourceFunc{envs: []ExecEnv{{
		Key: "EXAMPLE_SESSION_TOKEN", Value: token, Secret: true, Reserved: true,
	}}}
	execer := &recordSpecEnvExecer{}
	// One backend records the actual ExecSpec.Envs and returns secrets in every result field.
	res, err := NewEnrichingExecerWithEnvs(execer, "session-example", fakeSessionEnvs{
		envs: map[string]string{"EXAMPLE_SESSION_TOKEN": "user-must-not-win"},
	}, source, quietLog()).Exec(context.Background(), ExecSpec{
		Path: "echo", Envs: map[string]string{"EXAMPLE_SESSION_TOKEN": "tool-must-not-win"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for field, value := range map[string]string{"stdout": res.Stdout, "stderr": res.Stderr, "err": res.Err} {
		if strings.Contains(value, token) {
			t.Errorf("%s leaked the injected credential: %q", field, value)
		}
	}
	if execer.gotEnvs["EXAMPLE_SESSION_TOKEN"] != token {
		t.Fatalf("platform credential did not win over user and tool env: %v", execer.gotEnvs)
	}
	if !IsPlatformReservedEnv("EXAMPLE_SESSION_TOKEN") {
		t.Fatal("downstream reserved name must be visible to alias validation")
	}
}

func TestExecEnvSourceRefreshesEachExecution(t *testing.T) {
	source := &execEnvSourceFunc{}
	injector := NewSessionInjectorWithEnvs("session-example", nil, source, quietLog())
	for _, value := range []string{"first-token-1234567890", "refreshed-token-1234567890"} {
		source.envs = []ExecEnv{{Key: "EXAMPLE_TOKEN", Value: value, Secret: true}}
		envs, _ := injector.Prepare(context.Background())
		if envs["EXAMPLE_TOKEN"] != value {
			t.Fatalf("got %q, want fresh value %q", envs["EXAMPLE_TOKEN"], value)
		}
	}
	if source.calls != 2 {
		t.Fatalf("source called %d times, want once per execution", source.calls)
	}
}

func TestExecEnvSourceFailureDoesNotDiscardOtherValues(t *testing.T) {
	source := &execEnvSourceFunc{err: errors.New("credential service unavailable")}
	injector := NewSessionInjectorWithEnvs("session-example", fakeSessionEnvs{
		envs: map[string]string{"PUBLIC_SETTING": "kept"},
	}, source, quietLog())
	envs, _ := injector.Prepare(context.Background())
	if envs["PUBLIC_SETTING"] != "kept" {
		t.Fatalf("source failure discarded independent values: %v", envs)
	}
}

type fixedResultExecer struct{ result *Result }

func (e *fixedResultExecer) Exec(context.Context, ExecSpec) (*Result, error) { return e.result, nil }

type recordSpecEnvExecer struct{ gotEnvs map[string]string }

func (e *recordSpecEnvExecer) Exec(_ context.Context, spec ExecSpec) (*Result, error) {
	e.gotEnvs = spec.Envs
	code := 0
	value := spec.Envs["EXAMPLE_SESSION_TOKEN"]
	return &Result{Stdout: value, Stderr: value, Err: value, ExitCode: &code}, nil
}

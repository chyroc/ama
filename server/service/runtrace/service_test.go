package runtrace

import (
	"context"
	"errors"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

type traceStore struct{ err error }

func (s traceStore) GetRun(context.Context, string) (apimodel.Run, error) {
	return apimodel.Run{}, s.err
}

type traceLocator struct{ id string }

func (l traceLocator) TraceRef(string) Ref {
	return Ref{TraceID: l.id, Enabled: true, WorkspaceID: "example"}
}

func TestGetRunTrace(t *testing.T) {
	if _, err := New(traceStore{}, nil).Get(context.Background(), " "); !errors.Is(err, persistmodel.ErrInvalidArgument) {
		t.Fatalf("empty id should be invalid: %v", err)
	}
	if _, err := New(traceStore{err: persistmodel.ErrNotFound}, nil).Get(context.Background(), "missing"); !errors.Is(err, persistmodel.ErrNotFound) {
		t.Fatalf("missing run should remain not found: %v", err)
	}
	ref, err := New(traceStore{}, nil).Get(context.Background(), "run_test")
	if err != nil || ref.Enabled || ref.TraceID != "" {
		t.Fatalf("unconfigured backend should be explicitly disabled: %+v %v", ref, err)
	}
	ref, err = New(traceStore{}, traceLocator{id: "trace_test"}).Get(context.Background(), "run_test")
	if err != nil || !ref.Enabled || ref.TraceID != "trace_test" || ref.WorkspaceID != "example" {
		t.Fatalf("locator result changed: %+v %v", ref, err)
	}
}

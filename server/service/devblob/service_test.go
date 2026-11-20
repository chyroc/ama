package devblob

import (
	"context"
	"errors"
	"testing"

	"github.com/chyroc/ama/server/model/persistmodel"
)

type memoryObjects struct {
	objects map[string][]byte
	getErr  error
}

func (m *memoryObjects) Put(_ context.Context, key string, content []byte, _ string) error {
	m.objects[key] = append([]byte(nil), content...)
	return nil
}
func (m *memoryObjects) Get(_ context.Context, key string) ([]byte, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
	content, ok := m.objects[key]
	if !ok {
		return nil, persistmodel.ErrNotFound
	}
	return append([]byte(nil), content...), nil
}
func (m *memoryObjects) Delete(_ context.Context, key string) error {
	delete(m.objects, key)
	return nil
}

type staffReaders struct{ allow bool }

func (r staffReaders) CanRead(string) bool { return r.allow }

func TestRestrictedObjectProxy(t *testing.T) {
	objects := &memoryObjects{objects: map[string][]byte{}}
	newService := func(readers Readers) *Service {
		service, err := New(Config{
			Objects: objects, Readers: readers, Prefix: "dev/", MaxBytes: 4,
			Actor:        func(context.Context) (string, error) { return "owner@example.com", nil },
			BlobNotFound: func(err error) bool { return errors.Is(err, persistmodel.ErrNotFound) },
		})
		if err != nil {
			t.Fatal(err)
		}
		return service
	}
	if err := newService(nil).Put(context.Background(), "dev/example", []byte("abc"), ""); !errors.Is(err, persistmodel.ErrAccessDenied) {
		t.Fatalf("nil readers must deny: %v", err)
	}
	if err := newService(staffReaders{allow: false}).Put(context.Background(), "dev/example", []byte("abc"), ""); !errors.Is(err, persistmodel.ErrAccessDenied) {
		t.Fatalf("non-staff caller must deny: %v", err)
	}
	service := newService(staffReaders{allow: true})
	for _, key := range []string{"other/example", "dev/../secret"} {
		if err := service.Put(context.Background(), key, []byte("abc"), ""); !errors.Is(err, persistmodel.ErrInvalidArgument) {
			t.Fatalf("out-of-scope key must reject: %q %v", key, err)
		}
	}
	if err := service.Put(context.Background(), "dev/large", []byte("large"), ""); !errors.Is(err, persistmodel.ErrInvalidArgument) {
		t.Fatalf("oversize object must reject: %v", err)
	}
	if err := service.Put(context.Background(), "dev/example", []byte("abc"), ""); err != nil {
		t.Fatal(err)
	}
	content, err := service.Get(context.Background(), "dev/example")
	if err != nil || string(content) != "abc" {
		t.Fatalf("unexpected content %q %v", content, err)
	}
	exists, err := service.Head(context.Background(), "dev/example")
	if err != nil || !exists {
		t.Fatalf("expected object to exist: %v %v", exists, err)
	}
	if err := service.Delete(context.Background(), "dev/example"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Get(context.Background(), "dev/example"); !errors.Is(err, persistmodel.ErrNotFound) {
		t.Fatalf("missing object must map to not found: %v", err)
	}
}

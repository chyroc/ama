package filebytes

import (
	"context"
	"errors"
	"testing"

	"github.com/chyroc/ama/server/model/persistmodel"
)

type fallbackBlobs struct{ getErr error }

func (b fallbackBlobs) Put(context.Context, string, []byte, string) error { return nil }
func (b fallbackBlobs) Get(context.Context, string) ([]byte, error)       { return nil, b.getErr }
func (b fallbackBlobs) Bucket() string                                    { return "example" }

type fallbackFiles struct{ err error }

func (f fallbackFiles) CreateFile(context.Context, persistmodel.FileRecord) error { return nil }
func (f fallbackFiles) GetFile(context.Context, string) (persistmodel.FileRecord, error) {
	return persistmodel.FileRecord{ObjectKey: "example/object"}, f.err
}

func TestReadWithFallbackOnlyUsesLegacyBytesForKnownGaps(t *testing.T) {
	fallback := func(context.Context) ([]byte, error) { return []byte("legacy"), nil }
	for _, tc := range []struct {
		name  string
		id    string
		files error
		blobs error
	}{
		{name: "empty id"},
		{name: "missing metadata", id: "file_test", files: persistmodel.ErrNotFound},
		{name: "missing object", id: "file_test", blobs: ErrBlobNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := New(fallbackBlobs{getErr: tc.blobs}, fallbackFiles{err: tc.files}, func() string { return "file_test" })
			content, err := store.ReadWithFallback(context.Background(), tc.id, fallback)
			if err != nil || string(content) != "legacy" {
				t.Fatalf("expected legacy bytes, got %q %v", content, err)
			}
		})
	}
	store := New(fallbackBlobs{getErr: errors.New("network down")}, fallbackFiles{}, func() string { return "file_test" })
	if _, err := store.ReadWithFallback(context.Background(), "file_test", fallback); err == nil {
		t.Fatal("storage failures must not be hidden by the legacy fallback")
	}
}

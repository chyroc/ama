package core

import (
	"context"
	"errors"
	"testing"

	"github.com/chyroc/ama/server/model/persistmodel"
)

type blobMigrationTestStore struct {
	payload  persistmodel.BlobPayload
	attached bool
	getErr   error
}

func (s *blobMigrationTestStore) ListUnmigratedBlobs(
	context.Context, persistmodel.BlobMigrationTable, int,
) ([]persistmodel.BlobRowKey, error) {
	return []persistmodel.BlobRowKey{{Primary: "row_example"}}, nil
}
func (s *blobMigrationTestStore) GetBlobForMigration(
	context.Context, persistmodel.BlobMigrationTable, persistmodel.BlobRowKey,
) (persistmodel.BlobPayload, error) {
	return s.payload, s.getErr
}
func (s *blobMigrationTestStore) AttachFileToRow(
	context.Context, persistmodel.BlobMigrationTable, persistmodel.BlobRowKey, persistmodel.FileRecord,
) error {
	s.attached = true
	return nil
}

type blobMigrationTestObjects struct {
	puts int
	read []byte
	err  error
}

func (o *blobMigrationTestObjects) Put(context.Context, string, []byte, string) error {
	o.puts++
	return o.err
}
func (o *blobMigrationTestObjects) Get(context.Context, string) ([]byte, error) {
	return o.read, o.err
}
func (*blobMigrationTestObjects) Delete(context.Context, string) error { return nil }
func (*blobMigrationTestObjects) Bucket() string                       { return "example-bucket" }

func newBlobMigrationTestService(
	store BlobMigrationStore, blobs BlobStore,
) *BlobMigrationService {
	return NewBlobMigrationService(BlobMigrationHooks{
		Store: store, Blobs: blobs, Tables: []persistmodel.BlobMigrationTable{persistmodel.BlobTableIssueAttachments},
		Prefix:        func(persistmodel.BlobMigrationTable) (string, error) { return "issue-attachments", nil },
		NewID:         func() string { return "file_example" },
		NowISO:        func() string { return "2026-01-01T00:00:00.000Z" },
		MissingAuthor: "migration@example.com",
	})
}

func TestBlobMigrationDryRunDoesNotWriteObjectsOrRows(t *testing.T) {
	store := &blobMigrationTestStore{payload: persistmodel.BlobPayload{Content: []byte("source")}}
	blobs := &blobMigrationTestObjects{}
	report, err := newBlobMigrationTestService(store, blobs).Migrate(context.Background(), "admin@example.com", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if report.TotalPending != 1 || len(report.Items) != 0 || blobs.puts != 0 || store.attached {
		t.Fatalf("dry-run changed data: report=%#v puts=%d attached=%v", report, blobs.puts, store.attached)
	}
}

func TestBlobMigrationReadbackMismatchLeavesRowUntouched(t *testing.T) {
	store := &blobMigrationTestStore{payload: persistmodel.BlobPayload{Content: []byte("expected bytes")}}
	blobs := &blobMigrationTestObjects{read: []byte("truncated")}
	report, err := newBlobMigrationTestService(store, blobs).Migrate(context.Background(), "admin@example.com", true, 1)
	if err != nil {
		t.Fatal(err)
	}
	if report.TotalMigrated != 0 || report.TotalFailed != 1 || store.attached {
		t.Fatalf("mismatch must not attach the row: report=%#v attached=%v", report, store.attached)
	}
}

func TestBlobMigrationApplyVerifiesBytesAndAttaches(t *testing.T) {
	content := []byte("complete source content")
	store := &blobMigrationTestStore{payload: persistmodel.BlobPayload{Content: content}}
	blobs := &blobMigrationTestObjects{read: content}
	report, err := newBlobMigrationTestService(store, blobs).Migrate(context.Background(), "admin@example.com", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if report.Limit != DefaultMigrateBlobLimit || report.TotalMigrated != 1 || !store.attached {
		t.Fatalf("valid bytes not migrated: report=%#v attached=%v", report, store.attached)
	}
}

func TestBlobMigrationReadFailureDoesNotAttach(t *testing.T) {
	store := &blobMigrationTestStore{payload: persistmodel.BlobPayload{Content: []byte("source")}}
	blobs := &blobMigrationTestObjects{err: errors.New("temporary object read failure")}
	report, err := newBlobMigrationTestService(store, blobs).Migrate(context.Background(), "admin@example.com", true, 1)
	if err != nil {
		t.Fatal(err)
	}
	if report.TotalFailed != 1 || store.attached {
		t.Fatalf("read failure must remain resumable: report=%#v attached=%v", report, store.attached)
	}
}

package core

import (
	"context"
	"errors"
	"testing"
	"time"
)

type blobObjectTestClient struct {
	getErr  error
	sizeErr error
}

func (blobObjectTestClient) Put(context.Context, string, []byte, string) error { return nil }
func (c blobObjectTestClient) Get(context.Context, string) ([]byte, error)     { return nil, c.getErr }
func (blobObjectTestClient) Delete(context.Context, string) error              { return nil }
func (c blobObjectTestClient) Size(context.Context, string) (int64, error)     { return 0, c.sizeErr }
func (blobObjectTestClient) Bucket() string                                    { return "example" }

func TestBlobObjectAdapterTranslatesEveryReadNotFound(t *testing.T) {
	missing := errors.New("provider missing")
	adapter := &BlobObjectAdapter{Client: blobObjectTestClient{getErr: missing, sizeErr: missing}, NotFound: missing}
	if _, err := adapter.Get(context.Background(), "missing"); !errors.Is(err, ErrBlobNotFound) {
		t.Fatalf("Get error = %v", err)
	}
	if _, err := adapter.Size(context.Background(), "missing"); !errors.Is(err, ErrBlobNotFound) {
		t.Fatalf("Size error = %v", err)
	}
}

func TestBlobObjectAdapterRequiresPresign(t *testing.T) {
	adapter := &BlobObjectAdapter{Client: blobObjectTestClient{}}
	if _, err := adapter.PresignedGetURL("key", time.Minute, LinkForPublic); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("missing presign error = %v", err)
	}
	adapter.Presign = func(key string, _ time.Duration, _ LinkReader) (string, error) {
		return "https://example.com/" + key, nil
	}
	if got, err := adapter.PresignedGetURL("skill.zip", time.Minute, LinkForPublic); err != nil || got != "https://example.com/skill.zip" {
		t.Fatalf("presign = %q %v", got, err)
	}
}

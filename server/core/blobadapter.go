package core

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// BlobObjectClient is the storage protocol shared by attachments, skills and personal files.
// The downstream owns endpoint selection, authentication and its own NotFound sentinel.
type BlobObjectClient interface {
	Put(context.Context, string, []byte, string) error
	Get(context.Context, string) ([]byte, error)
	Delete(context.Context, string) error
	Size(context.Context, string) (int64, error)
	Bucket() string
}

// BlobObjectAdapter translates storage NotFound into the core sentinel on every read path.
// Losing that translation makes a cache miss look like an outage and disables byte fallback.
type BlobObjectAdapter struct {
	Client   BlobObjectClient
	NotFound error
	Presign  func(string, time.Duration, LinkReader) (string, error)
}

var (
	_ BlobStore     = (*BlobObjectAdapter)(nil)
	_ BlobPresigner = (*BlobObjectAdapter)(nil)
	_ BlobSizer     = (*BlobObjectAdapter)(nil)
)

func (a *BlobObjectAdapter) Put(ctx context.Context, key string, content []byte, contentType string) error {
	return a.Client.Put(ctx, key, content, contentType)
}
func (a *BlobObjectAdapter) Get(ctx context.Context, key string) ([]byte, error) {
	content, err := a.Client.Get(ctx, key)
	if err != nil && a.NotFound != nil && errors.Is(err, a.NotFound) {
		return nil, fmt.Errorf("%w: %v", ErrBlobNotFound, err)
	}
	return content, err
}
func (a *BlobObjectAdapter) Delete(ctx context.Context, key string) error {
	return a.Client.Delete(ctx, key)
}
func (a *BlobObjectAdapter) Size(ctx context.Context, key string) (int64, error) {
	size, err := a.Client.Size(ctx, key)
	if err != nil && a.NotFound != nil && errors.Is(err, a.NotFound) {
		return 0, fmt.Errorf("%w: %v", ErrBlobNotFound, err)
	}
	return size, err
}
func (a *BlobObjectAdapter) Bucket() string { return a.Client.Bucket() }

// PresignedGetURL requires an implementation. Without it, the direct skill archive link
// must fail explicitly instead of silently pretending that the storage cannot sign links.
func (a *BlobObjectAdapter) PresignedGetURL(key string, ttl time.Duration, reader LinkReader) (string, error) {
	if a.Presign == nil {
		return "", fmt.Errorf("%w: 对象存储没有实现预签名下载", ErrPrecondition)
	}
	return a.Presign(key, ttl, reader)
}

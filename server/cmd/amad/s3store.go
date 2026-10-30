// Adapts the shared/infra/s3 client to two business ports:
//
//   - blobStore       → core.BlobStore (attachments, skill files, skill packages) + signed links + size lookup
//   - userFileObjects → service/userfile.ObjectStore (personal files: whole object + the four multipart operations)
//
// Both adapters share the same *s3.Client (newS3Client builds it only once).
//
// **This layer exists because sentinel errors cannot be shared**: business packages do not import infra, so "object does not exist" is
// a different error value on each side and must be translated here. Missing the translation makes errors.Is always false:
// the skill package cache cannot recognize a miss, and personal files cannot tell "object is gone" from other errors, all with zero errors reported.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/chyroc/ama/server/core"
	"github.com/chyroc/ama/server/service/userfile"
	"github.com/chyroc/ama/server/shared/infra/config"
	"github.com/chyroc/ama/server/shared/infra/s3"
)

// newS3Client builds the object storage client from config.
//
//   - Not configured (s3.bucket empty) → (nil, nil). The caller decides by tier: the PG tier refuses to start, the mem tier uses memory.
//   - Configured but unreachable / bucket unusable → returns an error and amad **refuses to start**. No degrading: when bytes have nowhere to go,
//     "the service is up but every attachment upload is a 500" is harder to debug than not starting.
func newS3Client(ctx context.Context, cfg *config.Config, log *slog.Logger) (*s3.Client, error) {
	if !cfg.S3.Enabled() {
		return nil, nil
	}
	sc := cfg.S3.ToS3Config()
	client, err := s3.New(ctx, sc)
	if err != nil {
		return nil, fmt.Errorf("对象存储不可用（s3 段 / AMA_S3_*）: %w", err)
	}
	// Log all three addresses: signing a presigned link with the wrong Host is the most common failure on this path, and its symptoms
	// (browser download 403, session container fails to pull the skill package) do not point to config at all.
	or := func(v, fallback string) string {
		if v == "" {
			return fallback + "（同 endpoint）"
		}
		return v
	}
	log.Info("对象存储已连上",
		slog.String("endpoint", sc.Endpoint),
		slog.String("publicEndpoint", or(sc.PublicEndpoint, sc.Endpoint)),
		slog.String("sandboxEndpoint", or(sc.SandboxEndpoint, sc.Endpoint)),
		slog.String("bucket", sc.Bucket),
		slog.Bool("pathStyle", sc.UsePathStyle))
	return client, nil
}

// blobStore adapts *s3.Client to core.BlobStore (plus two optional capabilities).
type blobStore struct{ client *s3.Client }

// Pins three things at compile time: the adapter satisfies **all** of core's ports.
//
// ⚠️ **The last two lines must stay.** BlobPresigner and BlobSizer are not in the core.BlobStore interface
// (core gets them through type assertions), so a missing implementation is not a compile error: the assertion fails → ensureSkillArchive
// returns nil directly → the whole "container downloads the skill package directly" path silently turns off, large skills do not fit, zero errors.
var (
	_ core.BlobStore     = (*blobStore)(nil)
	_ core.BlobPresigner = (*blobStore)(nil)
	_ core.BlobSizer     = (*blobStore)(nil)
)

func (b *blobStore) Put(ctx context.Context, key string, content []byte, contentType string) error {
	return b.client.Put(ctx, key, content, contentType)
}

func (b *blobStore) Get(ctx context.Context, key string) ([]byte, error) {
	data, err := b.client.Get(ctx, key)
	if errors.Is(err, s3.ErrNotFound) {
		return nil, fmt.Errorf("%w: %v", core.ErrBlobNotFound, err)
	}
	return data, err
}

func (b *blobStore) Delete(ctx context.Context, key string) error {
	return b.client.Delete(ctx, key)
}

func (b *blobStore) Bucket() string { return b.client.Bucket() }

// PresignedGetURL picks the signing address by reader; see core.LinkReader.
func (b *blobStore) PresignedGetURL(key string, ttl time.Duration, reader core.LinkReader) (string, error) {
	switch reader {
	case core.LinkForSandbox:
		return b.client.PresignGetForSandbox(key, ttl)
	case core.LinkForPublic:
		return b.client.PresignGetForPublic(key, ttl)
	default:
		// A new reader kind was added but not wired here: return an error instead of signing with any address,
		// because a link signed with the wrong Host cannot be reached, and that symptom does not point here.
		return "", fmt.Errorf("不认识的链接读者 %d", reader)
	}
}

func (b *blobStore) Size(ctx context.Context, key string) (int64, error) {
	n, err := b.client.Size(ctx, key)
	if errors.Is(err, s3.ErrNotFound) {
		return 0, fmt.Errorf("%w: %v", core.ErrBlobNotFound, err)
	}
	return n, err
}

// userFileObjects adapts *s3.Client to userfile.ObjectStore.
type userFileObjects struct{ client *s3.Client }

var _ userfile.ObjectStore = (*userFileObjects)(nil)

func (o *userFileObjects) Bucket() string { return o.client.Bucket() }

func (o *userFileObjects) Put(ctx context.Context, key string, content []byte, contentType string) error {
	return o.client.Put(ctx, key, content, contentType)
}

func (o *userFileObjects) Delete(ctx context.Context, key string) error {
	return o.client.Delete(ctx, key)
}

func (o *userFileObjects) Size(ctx context.Context, key string) (int64, error) {
	n, err := o.client.Size(ctx, key)
	if errors.Is(err, s3.ErrNotFound) {
		return 0, fmt.Errorf("%w: %v", userfile.ErrObjectNotFound, err)
	}
	return n, err
}

// PresignedGetURL: personal file download links are for the **browser**, so they are signed with publicEndpoint.
func (o *userFileObjects) PresignedGetURL(key string, ttl time.Duration) (string, error) {
	return o.client.PresignGetForPublic(key, ttl)
}

func (o *userFileObjects) Open(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	body, n, err := o.client.Open(ctx, key)
	if errors.Is(err, s3.ErrNotFound) {
		return nil, 0, fmt.Errorf("%w: %v", userfile.ErrObjectNotFound, err)
	}
	return body, n, err
}

func (o *userFileObjects) InitMultipartUpload(ctx context.Context, key, contentType string) (string, error) {
	return o.client.InitMultipartUpload(ctx, key, contentType)
}

func (o *userFileObjects) UploadPart(
	ctx context.Context, key, uploadID string, partNumber int, size int64, content io.Reader,
) error {
	return o.client.UploadPart(ctx, key, uploadID, partNumber, size, content)
}

func (o *userFileObjects) CompleteMultipartUpload(ctx context.Context, key, uploadID string) error {
	err := o.client.CompleteMultipartUpload(ctx, key, uploadID)
	if errors.Is(err, s3.ErrNotFound) {
		return fmt.Errorf("%w: %v", userfile.ErrObjectNotFound, err)
	}
	return err
}

func (o *userFileObjects) AbortMultipartUpload(ctx context.Context, key, uploadID string) error {
	return o.client.AbortMultipartUpload(ctx, key, uploadID)
}

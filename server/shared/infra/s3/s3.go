// Package s3 is the client for S3-compatible object storage: the **bytes** of attachments,
// skill files, skill packages and personal files all live here, while PostgreSQL only stores
// metadata (bucket + object_key in tables like files / user_files).
//
// It is written against **generic S3** (aws-sdk-go-v2) and not tied to any vendor: the local
// docker compose runs RustFS, and production can switch to AWS S3, MinIO, Ceph RGW, or a
// cloud provider's S3-compatible service by changing config only.
//
// # Three endpoints: one presigned URL cannot serve both sides
//
// A presigned URL (SigV4) **signs the Host too**. So "who will hit this URL" decides which
// address must be used when signing; hitting it with a different Host gives 403
// SignatureDoesNotMatch:
//
//	Endpoint         used by amad itself for reads/writes. In compose it is http://s3:9000
//	                 (the service name inside the container network)
//	PublicEndpoint   used for URLs signed for the **browser** and **the user's own machine**
//	                 (ama daemon). In compose it is http://localhost:19000 (host-mapped port)
//	SandboxEndpoint  used for URLs signed for the **Docker session container** (pulling skill
//	                 packages). Session containers are on ama-net; in compose it is the same
//	                 as Endpoint
//
// The last two fall back to Endpoint when empty. Signing is a purely local computation (no
// request is sent), so building a separate signing client per endpoint costs no network.
//
// ⚠️ **Do not try to "sign, then change the Host"**: the signature includes the Host, so
// changing it breaks verification. And do not expect one address to work for both sides:
// browsers cannot resolve the compose service name, and localhost inside a container points
// at the container itself.
//
// # Translating "object does not exist"
//
// This package only exports its own ErrNotFound. The caller (the adapter layer in cmd/amad)
// translates it into filebytes.ErrBlobNotFound / userfile.ErrObjectNotFound. This package is
// a leaf package and does not import business packages.
package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// ErrNotFound means the object (or bucket) does not exist. Check it with errors.Is.
var ErrNotFound = errors.New("s3: 对象不存在")

// Part number range. S3 protocol: part numbers start at 1, at most 10000 parts.
const (
	MinPartNumber = 1
	MaxPartNumber = 10000
)

// defaultTimeout is the limit for a single call. It must fit large requests like "read a whole
// 100 MiB skill package", so it cannot be a few seconds like metadata queries.
const defaultTimeout = 2 * time.Minute

// defaultRegion is used when region is empty. Most S3-compatible services do not care about
// region, but SigV4 signing needs one; us-east-1 is the S3 protocol default, and RustFS /
// MinIO both accept it.
const defaultRegion = "us-east-1"

// Config is the client config. Fields map one to one to config.S3.
type Config struct {
	// Endpoint is the address amad uses to read/write objects, like http://s3:9000. When empty
	// it uses the official AWS address (by Region).
	Endpoint string
	// PublicEndpoint is the address for URLs signed for the browser / the user's machine.
	// Falls back to Endpoint when empty.
	PublicEndpoint string
	// SandboxEndpoint is the address for URLs signed for the Docker session container. Falls
	// back to Endpoint when empty.
	SandboxEndpoint string

	Region string
	Bucket string

	AccessKey string
	SecretKey string

	// UsePathStyle uses http://host/bucket/key instead of http://bucket.host/key.
	// RustFS / MinIO / local development all need true (virtual-host style needs wildcard DNS).
	UsePathStyle bool

	// CreateBucket: when true, create the bucket at startup if it does not exist.
	CreateBucket bool

	// Timeout is the limit for a single call. Defaults to 2 minutes when empty.
	Timeout time.Duration
}

// Client is the client for S3-compatible object storage. Safe for concurrent use (the
// underlying SDK client is).
type Client struct {
	api     *awss3.Client
	public  *awss3.PresignClient
	sandbox *awss3.PresignClient
	bucket  string
	timeout time.Duration
}

// New builds the client and **checks at startup that the bucket is usable**: if the bucket
// does not exist and CreateBucket is true it creates it, otherwise it errors. Wrong
// credentials, wrong address or wrong bucket name all blow up here, instead of a 500 when the
// first user uploads an attachment.
func New(ctx context.Context, cfg Config) (*Client, error) {
	cfg.Bucket = strings.TrimSpace(cfg.Bucket)
	if cfg.Bucket == "" {
		return nil, errors.New("s3: bucket 不能为空")
	}
	if strings.TrimSpace(cfg.AccessKey) == "" || cfg.SecretKey == "" {
		return nil, errors.New("s3: accessKey 与 secretKey 都要配")
	}
	for name, ep := range map[string]string{
		"endpoint": cfg.Endpoint, "publicEndpoint": cfg.PublicEndpoint, "sandboxEndpoint": cfg.SandboxEndpoint,
	} {
		if err := checkEndpoint(ep); err != nil {
			return nil, fmt.Errorf("s3: %s 不合法: %w", name, err)
		}
	}
	region := strings.TrimSpace(cfg.Region)
	if region == "" {
		region = defaultRegion
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	creds := credentials.NewStaticCredentialsProvider(strings.TrimSpace(cfg.AccessKey), cfg.SecretKey, "")

	build := func(endpoint string) *awss3.Client {
		return awss3.New(awss3.Options{
			Region:       region,
			Credentials:  creds,
			UsePathStyle: cfg.UsePathStyle,
			BaseEndpoint: optional(endpoint),
			// ⚠️ Newer SDK versions compute CRC32 for every request by default and upload with
			// an aws-chunked trailing checksum. Many S3-compatible services (including some
			// versions of RustFS / MinIO / Ceph) do not accept it; the symptom is PutObject 400
			// or chunk headers mixed into the object content. Sending it only when the protocol
			// requires it gives the widest compatibility.
			RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
			ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
			HTTPClient:                 awshttp.NewBuildableClient(),
		})
	}

	endpoint := strings.TrimSpace(cfg.Endpoint)
	api := build(endpoint)
	public, sandbox := api, api
	if ep := strings.TrimSpace(cfg.PublicEndpoint); ep != "" && ep != endpoint {
		public = build(ep)
	}
	if ep := strings.TrimSpace(cfg.SandboxEndpoint); ep != "" && ep != endpoint {
		sandbox = build(ep)
	}

	c := &Client{
		api:     api,
		public:  awss3.NewPresignClient(public),
		sandbox: awss3.NewPresignClient(sandbox),
		bucket:  cfg.Bucket,
		timeout: timeout,
	}
	if err := c.ensureBucket(ctx, cfg.CreateBucket); err != nil {
		return nil, err
	}
	return c, nil
}

func optional(s string) *string {
	if s = strings.TrimSpace(s); s == "" {
		return nil
	}
	return aws.String(s)
}

// checkEndpoint only blocks clearly wrong addresses: one without a scheme ("s3:9000") would be
// treated by the SDK as a relative path, and the error would not point at "missing http://"
// at all.
func checkEndpoint(ep string) error {
	ep = strings.TrimSpace(ep)
	if ep == "" {
		return nil
	}
	u, err := url.Parse(ep)
	if err != nil {
		return err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%q 要写成 http(s)://host[:port]", ep)
	}
	return nil
}

// ensureBucket checks that the bucket exists; if not and creation is allowed, it creates it.
//
// When several instances start at once they may all create it: BucketAlreadyOwnedByYou /
// BucketAlreadyExists both count as success (some implementations also use the latter to mean
// "you just created it"). After creating, HeadBucket once more to confirm it really works.
func (c *Client) ensureBucket(ctx context.Context, create bool) error {
	err := c.headBucket(ctx)
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("s3: 检查桶 %s 失败（地址、凭据、网络？）: %w", c.bucket, err)
	}
	if !create {
		return fmt.Errorf("s3: 桶 %s 不存在，且 createBucket=false——先手动建桶", c.bucket)
	}
	cctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if _, err := c.api.CreateBucket(cctx, &awss3.CreateBucketInput{Bucket: aws.String(c.bucket)}); err != nil {
		var owned *types.BucketAlreadyOwnedByYou
		var exists *types.BucketAlreadyExists
		if !errors.As(err, &owned) && !errors.As(err, &exists) &&
			!hasCode(err, "BucketAlreadyOwnedByYou", "BucketAlreadyExists") {
			return fmt.Errorf("s3: 建桶 %s 失败: %w", c.bucket, err)
		}
	}
	if err := c.headBucket(ctx); err != nil {
		return fmt.Errorf("s3: 建桶 %s 后仍不可用: %w", c.bucket, err)
	}
	return nil
}

func (c *Client) headBucket(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	_, err := c.api.HeadBucket(cctx, &awss3.HeadBucketInput{Bucket: aws.String(c.bucket)})
	if err != nil && isNotFound(err) {
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	return err
}

// Bucket returns the bucket name (stored in the bucket column).
func (c *Client) Bucket() string { return c.bucket }

// Put writes a whole object in one call.
func (c *Client) Put(ctx context.Context, key string, content []byte, contentType string) error {
	if key == "" {
		return errors.New("s3: key 不能为空")
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	_, err := c.api.PutObject(ctx, &awss3.PutObjectInput{
		Bucket:        aws.String(c.bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(content),
		ContentLength: aws.Int64(int64(len(content))),
		ContentType:   optional(contentType),
	})
	if err != nil {
		return fmt.Errorf("s3: 写对象失败 key=%s: %w", key, err)
	}
	return nil
}

// Get reads a whole object. When it does not exist, it returns an error that
// errors.Is(err, ErrNotFound) recognizes.
func (c *Client) Get(ctx context.Context, key string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	out, err := c.api.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(key)})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: key=%s", ErrNotFound, key)
		}
		return nil, fmt.Errorf("s3: 读对象失败 key=%s: %w", key, err)
	}
	defer out.Body.Close()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("s3: 读对象内容失败 key=%s: %w", key, err)
	}
	return data, nil
}

// Open reads an object as a stream and returns the content and byte count. The caller must
// Close it. Returns ErrNotFound when it does not exist.
//
// ⚠️ **It does not apply this package's per-call timeout**: personal files can be up to
// 2 GiB, and how long reading takes depends on the other side's network speed; a 2 minute
// limit would cut slower downloads in half. Its lifetime follows the caller's ctx (it stops
// when the HTTP request disconnects).
func (c *Client) Open(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	out, err := c.api.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(key)})
	if err != nil {
		if isNotFound(err) {
			return nil, 0, fmt.Errorf("%w: key=%s", ErrNotFound, key)
		}
		return nil, 0, fmt.Errorf("s3: 读对象失败 key=%s: %w", key, err)
	}
	return out.Body, aws.ToInt64(out.ContentLength), nil
}

// Size returns the object's byte count (HEAD, without fetching content). Returns ErrNotFound
// when it does not exist.
func (c *Client) Size(ctx context.Context, key string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	out, err := c.api.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(key)})
	if err != nil {
		if isNotFound(err) {
			return 0, fmt.Errorf("%w: key=%s", ErrNotFound, key)
		}
		return 0, fmt.Errorf("s3: 查对象失败 key=%s: %w", key, err)
	}
	return aws.ToInt64(out.ContentLength), nil
}

// Delete deletes an object. Not existing is not an error (the S3 protocol already works this
// way; this adds one more safety layer for implementations that do not follow it).
func (c *Client) Delete(ctx context.Context, key string) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	_, err := c.api.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(key)})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("s3: 删对象失败 key=%s: %w", key, err)
	}
	return nil
}

// PresignGetForPublic signs a download URL for **the browser / the user's own machine**
// (Host = PublicEndpoint).
func (c *Client) PresignGetForPublic(key string, ttl time.Duration) (string, error) {
	return presignGet(c.public, c.bucket, key, ttl)
}

// PresignGetForSandbox signs a download URL for **the Docker session container**
// (Host = SandboxEndpoint).
func (c *Client) PresignGetForSandbox(key string, ttl time.Duration) (string, error) {
	return presignGet(c.sandbox, c.bucket, key, ttl)
}

func presignGet(p *awss3.PresignClient, bucket, key string, ttl time.Duration) (string, error) {
	if key == "" {
		return "", errors.New("s3: key 不能为空")
	}
	if ttl <= 0 {
		return "", errors.New("s3: 链接有效期必须为正")
	}
	// Signing is a purely local computation; context is just an SDK parameter.
	req, err := p.PresignGetObject(context.Background(),
		&awss3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)},
		awss3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("s3: 签下载链接失败 key=%s: %w", key, err)
	}
	if req.Method != http.MethodGet {
		return "", fmt.Errorf("s3: 签出来的方法是 %s 而不是 GET", req.Method)
	}
	return req.URL, nil
}

// InitMultipartUpload starts a multipart upload and returns the uploadId. contentType is set
// at this step (S3 protocol: object metadata is given at initialization).
func (c *Client) InitMultipartUpload(ctx context.Context, key, contentType string) (string, error) {
	if key == "" {
		return "", errors.New("s3: key 不能为空")
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	out, err := c.api.CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{
		Bucket:      aws.String(c.bucket),
		Key:         aws.String(key),
		ContentType: optional(contentType),
	})
	if err != nil {
		return "", fmt.Errorf("s3: 初始化分片上传失败 key=%s: %w", key, err)
	}
	id := aws.ToString(out.UploadId)
	if id == "" {
		// An empty uploadId would make every later part go out with empty credentials, and the
		// error would not point at "initialization failed" at all.
		return "", errors.New("s3: 初始化分片上传返回了空 uploadId")
	}
	return id, nil
}

// UploadPart uploads one part.
//
// ⚠️ **Read the whole part into memory first, then send it.** Two reasons:
//
//  1. When SigV4 runs over http (the usual case locally / on a private network), it computes
//     SHA-256 over the request body, and the SDK requires a seekable body.
//     http.Request.Body cannot Seek, so passing it in directly makes the SDK refuse.
//  2. size is the length the client declared. One byte short and the object is truncated,
//     one byte extra and the request hangs until timeout. After reading it in we can **check
//     on the spot** that "real byte count == size", and return a 400-class error if not.
//
// The part size is limited by the caller (service/userfile uses 8 MiB), so memory use is
// bounded.
func (c *Client) UploadPart(
	ctx context.Context, key, uploadID string, partNumber int, size int64, content io.Reader,
) error {
	switch {
	case key == "":
		return errors.New("s3: key 不能为空")
	case uploadID == "":
		return errors.New("s3: uploadId 不能为空")
	case partNumber < MinPartNumber || partNumber > MaxPartNumber:
		return fmt.Errorf("s3: 片号 %d 超出区间 [%d,%d]", partNumber, MinPartNumber, MaxPartNumber)
	case size < 0:
		return fmt.Errorf("s3: 片大小不能为负，收到 %d", size)
	case content == nil:
		return errors.New("s3: 片内容不能为 nil")
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(content, buf); err != nil {
		return fmt.Errorf("s3: 片内容不足声明的 %d 字节: %w", size, err)
	}
	var extra [1]byte
	if n, _ := content.Read(extra[:]); n > 0 {
		return fmt.Errorf("s3: 片内容超过声明的 %d 字节", size)
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	_, err := c.api.UploadPart(ctx, &awss3.UploadPartInput{
		Bucket:        aws.String(c.bucket),
		Key:           aws.String(key),
		UploadId:      aws.String(uploadID),
		PartNumber:    aws.Int32(int32(partNumber)),
		Body:          bytes.NewReader(buf),
		ContentLength: aws.Int64(size),
	})
	if err != nil {
		return fmt.Errorf("s3: 上传分片失败 key=%s part=%d: %w", key, partNumber, err)
	}
	return nil
}

// CompleteMultipartUpload merges the uploaded parts. The part list is fetched from object
// storage (ListParts), not passed in by the caller: parts may be uploaded through different
// instances, and only object storage knows the full set.
func (c *Client) CompleteMultipartUpload(ctx context.Context, key, uploadID string) error {
	if key == "" || uploadID == "" {
		return errors.New("s3: key 与 uploadId 都不能为空")
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var parts []types.CompletedPart
	pager := awss3.NewListPartsPaginator(c.api, &awss3.ListPartsInput{
		Bucket: aws.String(c.bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
	})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			if isNotFound(err) || hasCode(err, "NoSuchUpload") {
				return fmt.Errorf("%w: 分片上传不存在 key=%s", ErrNotFound, key)
			}
			return fmt.Errorf("s3: 列分片失败 key=%s: %w", key, err)
		}
		for _, p := range page.Parts {
			parts = append(parts, types.CompletedPart{ETag: p.ETag, PartNumber: p.PartNumber})
		}
	}
	if len(parts) == 0 {
		return fmt.Errorf("s3: 没有任何已上传的片，不能合并 key=%s", key)
	}
	// S3 requires parts in ascending part number order; ListParts is usually already sorted,
	// but we do not rely on it.
	sort.Slice(parts, func(i, j int) bool { return aws.ToInt32(parts[i].PartNumber) < aws.ToInt32(parts[j].PartNumber) })

	_, err := c.api.CompleteMultipartUpload(ctx, &awss3.CompleteMultipartUploadInput{
		Bucket:          aws.String(c.bucket),
		Key:             aws.String(key),
		UploadId:        aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		return fmt.Errorf("s3: 合并分片失败 key=%s: %w", key, err)
	}
	return nil
}

// AbortMultipartUpload abandons a multipart upload. A missing uploadId is not an error
// (idempotent).
func (c *Client) AbortMultipartUpload(ctx context.Context, key, uploadID string) error {
	if key == "" || uploadID == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	_, err := c.api.AbortMultipartUpload(ctx, &awss3.AbortMultipartUploadInput{
		Bucket: aws.String(c.bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
	})
	if err != nil && !isNotFound(err) && !hasCode(err, "NoSuchUpload") {
		return fmt.Errorf("s3: 废弃分片上传失败 key=%s: %w", key, err)
	}
	return nil
}

// isNotFound recognizes the forms of "does not exist": GetObject returns NoSuchKey;
// HeadObject / HeadBucket have no response body, only 404 (the SDK gives NotFound or a bare
// 404 ResponseError); a missing bucket returns NoSuchBucket.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	var nsk *types.NoSuchKey
	var nf *types.NotFound
	var nsb *types.NoSuchBucket
	if errors.As(err, &nsk) || errors.As(err, &nf) || errors.As(err, &nsb) {
		return true
	}
	if hasCode(err, "NoSuchKey", "NotFound", "NoSuchBucket") {
		return true
	}
	var re *awshttp.ResponseError
	return errors.As(err, &re) && re.HTTPStatusCode() == http.StatusNotFound
}

func hasCode(err error, codes ...string) bool {
	var ae smithy.APIError
	if !errors.As(err, &ae) {
		return false
	}
	for _, c := range codes {
		if ae.ErrorCode() == c {
			return true
		}
	}
	return false
}

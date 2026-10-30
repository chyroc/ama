package s3

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Offline part: a fake server that only answers HeadBucket 200 gets New running, then only
// signing is tested.
// ---------------------------------------------------------------------------

func newOffline(t *testing.T, cfg Config) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// At startup it only sends one HeadBucket.
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	cfg.Endpoint = srv.URL
	if cfg.Bucket == "" {
		cfg.Bucket = "ama"
	}
	cfg.AccessKey, cfg.SecretKey = "ak", "sk"
	cfg.UsePathStyle = true
	c, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New 失败：%v", err)
	}
	return c
}

// ⭐ Each of the three endpoints signs its own Host. This is what lets "the browser / session
// container reach the presigned URL".
func TestPresignUsesAudienceEndpoint(t *testing.T) {
	c := newOffline(t, Config{
		PublicEndpoint:  "http://localhost:19000",
		SandboxEndpoint: "http://s3:9000",
	})
	pub, err := c.PresignGetForPublic("user-files/a@x.com/uf_1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	box, err := c.PresignGetForSandbox("skill-archives/2026/10/h.zip", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	pu, _ := url.Parse(pub)
	bu, _ := url.Parse(box)
	if pu.Host != "localhost:19000" {
		t.Errorf("浏览器链接的 Host 应是 publicEndpoint，实际 %s", pu.Host)
	}
	if bu.Host != "s3:9000" {
		t.Errorf("容器链接的 Host 应是 sandboxEndpoint，实际 %s", bu.Host)
	}
	// path-style：/<bucket>/<key>
	if pu.Path != "/ama/user-files/a@x.com/uf_1" {
		t.Errorf("应是 path-style，实际 path=%s", pu.Path)
	}
	if pu.Query().Get("X-Amz-Expires") != "3600" || bu.Query().Get("X-Amz-Expires") != "60" {
		t.Errorf("有效期没签进去：%s / %s", pu.Query().Get("X-Amz-Expires"), bu.Query().Get("X-Amz-Expires"))
	}
}

// When the last two endpoints are empty they fall back to Endpoint.
func TestPresignFallsBackToEndpoint(t *testing.T) {
	c := newOffline(t, Config{})
	u1, _ := c.PresignGetForPublic("k", time.Minute)
	u2, _ := c.PresignGetForSandbox("k", time.Minute)
	p1, _ := url.Parse(u1)
	p2, _ := url.Parse(u2)
	if p1.Host != p2.Host || !strings.HasPrefix(p1.Host, "127.0.0.1:") {
		t.Errorf("留空应回落 endpoint，实际 %s / %s", p1.Host, p2.Host)
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	ctx := context.Background()
	if _, err := New(ctx, Config{AccessKey: "a", SecretKey: "b"}); err == nil {
		t.Error("没有 bucket 应报错")
	}
	if _, err := New(ctx, Config{Bucket: "b"}); err == nil {
		t.Error("没有凭据应报错")
	}
	if _, err := New(ctx, Config{Bucket: "b", AccessKey: "a", SecretKey: "b", Endpoint: "s3:9000"}); err == nil {
		t.Error("endpoint 没写协议应报错")
	}
}

// Bucket does not exist and creation is not allowed → startup fails (instead of a 500 on the
// first upload).
func TestNewFailsWhenBucketMissingAndCreateOff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	_, err := New(context.Background(), Config{
		Endpoint: srv.URL, Bucket: "nope", AccessKey: "a", SecretKey: "b", UsePathStyle: true,
	})
	if err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("应报桶不存在，实际 %v", err)
	}
}

// Several instances creating the bucket at once: someone else created it first
// (BucketAlreadyOwnedByYou), which must count as success.
func TestEnsureBucketToleratesAlreadyOwned(t *testing.T) {
	heads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			heads++
			if heads == 1 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
		case http.MethodPut:
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>BucketAlreadyOwnedByYou</Code><Message>x</Message></Error>`)
		}
	}))
	defer srv.Close()
	if _, err := New(context.Background(), Config{
		Endpoint: srv.URL, Bucket: "b", AccessKey: "a", SecretKey: "b", UsePathStyle: true, CreateBucket: true,
	}); err != nil {
		t.Fatalf("BucketAlreadyOwnedByYou 应当成功，实际 %v", err)
	}
}

// ---------------------------------------------------------------------------
// Integration part: runs against a real S3-compatible service (the local docker compose RustFS
// is enough).
//
//	AMA_TEST_S3_ENDPOINT=http://127.0.0.1:19000 AMA_TEST_S3_ACCESS_KEY=... \
//	AMA_TEST_S3_SECRET_KEY=... go test ./shared/infra/s3/
//
// Skip when not configured. Each run uses a new random bucket (which also tests "create the
// bucket if it does not exist").
// ---------------------------------------------------------------------------

func integrationClient(t *testing.T) *Client {
	t.Helper()
	ep := os.Getenv("AMA_TEST_S3_ENDPOINT")
	if ep == "" {
		t.Skip("没配 AMA_TEST_S3_ENDPOINT，跳过对象存储集成测试")
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	c, err := New(context.Background(), Config{
		Endpoint:     ep,
		Bucket:       "ama-test-" + hex.EncodeToString(b[:]),
		AccessKey:    os.Getenv("AMA_TEST_S3_ACCESS_KEY"),
		SecretKey:    os.Getenv("AMA_TEST_S3_SECRET_KEY"),
		UsePathStyle: true,
		CreateBucket: true,
	})
	if err != nil {
		t.Fatalf("连对象存储失败：%v", err)
	}
	return c
}

func TestIntegrationObjectRoundTrip(t *testing.T) {
	c := integrationClient(t)
	ctx := context.Background()

	if _, err := c.Get(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的对象 Get 应是 ErrNotFound，实际 %v", err)
	}
	if _, err := c.Size(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的对象 Size 应是 ErrNotFound，实际 %v", err)
	}
	if err := c.Delete(ctx, "missing"); err != nil {
		t.Fatalf("删不存在的对象不该报错：%v", err)
	}

	content := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("ama", 1000))
	if err := c.Put(ctx, "issue-attachments/2026/10/file_1", content, "image/png"); err != nil {
		t.Fatal(err)
	}
	got, err := c.Get(ctx, "issue-attachments/2026/10/file_1")
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("读回不一致：err=%v len=%d", err, len(got))
	}
	n, err := c.Size(ctx, "issue-attachments/2026/10/file_1")
	if err != nil || n != int64(len(content)) {
		t.Fatalf("Size 不对：%d %v", n, err)
	}

	// The presigned URL really downloads, and Content-Type is kept.
	u, err := c.PresignGetForPublic("issue-attachments/2026/10/file_1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := noProxyClient().Get(u)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Equal(body, content) {
		t.Fatalf("预签名下载失败：%d %q", resp.StatusCode, body[:min(len(body), 200)])
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type 应是 image/png，实际 %s", ct)
	}
	// A tampered signature must be rejected. This proves the URL really carries auth and the
	// bucket is not publicly readable.
	pu, _ := url.Parse(u)
	q := pu.Query()
	sig := q.Get("X-Amz-Signature")
	q.Set("X-Amz-Signature", strings.Repeat("0", len(sig))) // same length and format, only the value is wrong
	pu.RawQuery = q.Encode()
	resp2, err := noProxyClient().Get(pu.String())
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Errorf("坏签名应 403，实际 %d", resp2.StatusCode)
	}

	if err := c.Delete(ctx, "issue-attachments/2026/10/file_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(ctx, "issue-attachments/2026/10/file_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删完还读得到：%v", err)
	}
}

func TestIntegrationMultipart(t *testing.T) {
	c := integrationClient(t)
	ctx := context.Background()
	const key = "user-files/a@example.com/uf_test"

	id, err := c.InitMultipartUpload(ctx, key, "application/octet-stream")
	if err != nil {
		t.Fatal(err)
	}
	part1 := bytes.Repeat([]byte{'a'}, 5*1024*1024) // non-final parts must be ≥ 5 MiB
	part2 := []byte("tail")
	// Upload out of order on purpose.
	if err := c.UploadPart(ctx, key, id, 2, int64(len(part2)), bytes.NewReader(part2)); err != nil {
		t.Fatal(err)
	}
	// A declared length that does not match the real one must be rejected on the spot.
	if err := c.UploadPart(ctx, key, id, 1, int64(len(part1))+1, bytes.NewReader(part1)); err == nil {
		t.Fatal("声明长度多于实际应报错")
	}
	if err := c.UploadPart(ctx, key, id, 1, int64(len(part1))-1, bytes.NewReader(part1)); err == nil {
		t.Fatal("声明长度少于实际应报错")
	}
	// Use a reader that cannot Seek (mimics http.Request.Body).
	if err := c.UploadPart(ctx, key, id, 1, int64(len(part1)), io.NopCloser(bytes.NewReader(part1))); err != nil {
		t.Fatal(err)
	}
	if err := c.CompleteMultipartUpload(ctx, key, id); err != nil {
		t.Fatal(err)
	}
	n, err := c.Size(ctx, key)
	if err != nil || n != int64(len(part1)+len(part2)) {
		t.Fatalf("合并后大小不对：%d %v", n, err)
	}
	got, err := c.Get(ctx, key)
	if err != nil || !bytes.Equal(got, append(append([]byte{}, part1...), part2...)) {
		t.Fatal("合并后内容不对")
	}

	// Abort: an uploadId that is already completed / does not exist is not an error.
	if err := c.AbortMultipartUpload(ctx, key, id); err != nil {
		t.Errorf("Abort 已完成的上传不该报错：%v", err)
	}
	id2, err := c.InitMultipartUpload(ctx, key+"2", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.AbortMultipartUpload(ctx, key+"2", id2); err != nil {
		t.Fatal(err)
	}
	if err := c.CompleteMultipartUpload(ctx, key+"2", id2); err == nil {
		t.Error("废弃后的上传不该还能合并")
	}
	_ = c.Delete(ctx, key)
}

// The test process may carry HTTP(S)_PROXY; bypass it when hitting local ports.
func noProxyClient() *http.Client {
	return &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 30 * time.Second}
}

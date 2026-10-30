package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chyroc/ama/server/core"
	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/repository/dao/coremem"
	"github.com/chyroc/ama/server/service/userfile"
	"github.com/chyroc/ama/server/shared/infra/config"
)

// testBlobStoreContract is the contract shared by all core.BlobStore implementations: read matches write (binary, byte for byte),
// same key overwrites, missing returns core.ErrBlobNotFound, delete is idempotent, bucket name is non-empty.
//
// ⚠️ For the S3 adapter, "missing returns core.ErrBlobNotFound" is **translated**: if that step is missing,
// the skill package cache never recognizes a miss, with zero errors. So this one must run against real object storage.
func testBlobStoreContract(t *testing.T, b core.BlobStore) {
	t.Helper()
	ctx := context.Background()
	const key = "issue-attachments/2026/10/file_test_blob"

	if _, err := b.Get(ctx, key); !errors.Is(err, core.ErrBlobNotFound) {
		t.Fatalf("没写过的 key 该回 ErrBlobNotFound，实际 %v", err)
	}
	if err := b.Put(ctx, key, []byte("v1\x00bin"), "application/octet-stream"); err != nil {
		t.Fatalf("写失败：%v", err)
	}
	if got, err := b.Get(ctx, key); err != nil || !bytes.Equal(got, []byte("v1\x00bin")) {
		t.Fatalf("读回不一致：%q, %v", got, err)
	}
	if err := b.Put(ctx, key, []byte("v2"), "text/plain"); err != nil {
		t.Fatalf("覆盖写失败：%v", err)
	}
	if got, _ := b.Get(ctx, key); string(got) != "v2" {
		t.Fatalf("同 key 再写该覆盖，实际 %q", got)
	}
	if err := b.Delete(ctx, key); err != nil {
		t.Fatalf("删失败：%v", err)
	}
	if err := b.Delete(ctx, key); err != nil {
		t.Fatalf("删不存在的 key 不算错，实际 %v", err)
	}
	if _, err := b.Get(ctx, key); !errors.Is(err, core.ErrBlobNotFound) {
		t.Fatalf("删掉之后该回 ErrBlobNotFound，实际 %v", err)
	}
	if b.Bucket() == "" {
		t.Error("Bucket 不该是空串：它会落进 files.bucket")
	}
}

func TestMemBlobStoreContract(t *testing.T) {
	testBlobStoreContract(t, coremem.NewMemBlobStore())
}

// s3FromEnv connects to a real object storage with AMA_TEST_S3_* (a new random bucket); skips if not configured.
func s3FromEnv(t *testing.T, public, sandbox string) *config.Config {
	t.Helper()
	ep := os.Getenv("AMA_TEST_S3_ENDPOINT")
	if ep == "" {
		t.Skip("没配 AMA_TEST_S3_ENDPOINT，跳过对象存储集成测试")
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	cfg := &config.Config{S3: config.S3{
		Endpoint:        ep,
		PublicEndpoint:  public,
		SandboxEndpoint: sandbox,
		Bucket:          "ama-test-" + hex.EncodeToString(b[:]),
		AccessKey:       os.Getenv("AMA_TEST_S3_ACCESS_KEY"),
		SecretKey:       os.Getenv("AMA_TEST_S3_SECRET_KEY"),
	}}
	return cfg
}

func TestS3BlobStoreContract(t *testing.T) {
	cfg := s3FromEnv(t, "", "")
	client, err := newS3Client(context.Background(), cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	b := &blobStore{client: client}
	testBlobStoreContract(t, b)

	// Size's "missing" must also be translated to core's sentinel.
	if _, err := b.Size(context.Background(), "nope"); !errors.Is(err, core.ErrBlobNotFound) {
		t.Errorf("Size 不存在应回 ErrBlobNotFound，实际 %v", err)
	}
}

// ⭐ The two kinds of readers get links with different Hosts, and the endpoint one can really download.
func TestS3BlobStorePresignReaders(t *testing.T) {
	cfg := s3FromEnv(t, "http://public.example.test:19000", "")
	client, err := newS3Client(context.Background(), cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	b := &blobStore{client: client}
	ctx := context.Background()
	if err := b.Put(ctx, "skill-archives/2026/10/x.zip", []byte("PK zip"), "application/zip"); err != nil {
		t.Fatal(err)
	}

	pub, err := b.PresignedGetURL("skill-archives/2026/10/x.zip", time.Minute, core.LinkForPublic)
	if err != nil {
		t.Fatal(err)
	}
	if u, _ := url.Parse(pub); u.Host != "public.example.test:19000" {
		t.Errorf("给用户机器的链接应签 publicEndpoint，实际 %s", u.Host)
	}
	box, err := b.PresignedGetURL("skill-archives/2026/10/x.zip", time.Minute, core.LinkForSandbox)
	if err != nil {
		t.Fatal(err)
	}
	// sandboxEndpoint empty → falls back to endpoint, so this machine can reach it directly.
	resp, err := (&http.Client{Transport: &http.Transport{Proxy: nil}}).Get(box)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "PK zip" {
		t.Fatalf("容器链接下载失败：%d %q", resp.StatusCode, body)
	}
}

// ⭐ Personal files: real service/userfile + in-memory metadata + real object storage; run a multipart upload and download.
func TestUserFileMultipartAgainstS3(t *testing.T) {
	cfg := s3FromEnv(t, "", "")
	client, err := newS3Client(context.Background(), cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	store, err := coremem.NewSeededMemStore()
	if err != nil {
		t.Fatal(err)
	}
	svc, err := userfile.New(userfile.Config{Store: store, Objects: &userFileObjects{client: client}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const owner = "alice@example.com"

	part1 := bytes.Repeat([]byte("0123456789abcdef"), int(userfile.PartSize)/16)
	part2 := []byte("尾片")
	f, err := svc.StartUpload(ctx, owner, apimodel.StartUserFileUpload{
		Filename: "大文件.bin", ContentType: "application/octet-stream", Size: int64(len(part1) + len(part2)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.UploadMode != "multipart" {
		t.Fatalf("超过一片应走 multipart，实际 %s", f.UploadMode)
	}
	if err := svc.UploadPart(ctx, owner, f.ID, 2, int64(len(part2)), bytes.NewReader(part2)); err != nil {
		t.Fatal(err)
	}
	if err := svc.UploadPart(ctx, owner, f.ID, 1, int64(len(part1)), io.NopCloser(bytes.NewReader(part1))); err != nil {
		t.Fatal(err)
	}
	done, err := svc.CompleteUpload(ctx, owner, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Size != int64(len(part1)+len(part2)) {
		t.Fatalf("大小不对：%d", done.Size)
	}
	got, err := svc.GetFile(ctx, owner, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DownloadURL == "" {
		t.Fatal("没有下载链接")
	}
	resp, err := (&http.Client{Transport: &http.Transport{Proxy: nil}}).Get(got.DownloadURL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Equal(body, append(append([]byte{}, part1...), part2...)) {
		t.Fatalf("下载回来的字节不对：%d len=%d", resp.StatusCode, len(body))
	}
	if !strings.Contains(got.DownloadURL, "X-Amz-Signature=") {
		t.Errorf("下载链接应是预签名链接：%s", got.DownloadURL)
	}

	// Relayed download (`ama files download` uses this): stream back the same bytes.
	meta, rc, err := svc.OpenFile(ctx, owner, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	streamed, _ := io.ReadAll(rc)
	rc.Close()
	if meta.Size != int64(len(streamed)) || !bytes.Equal(streamed, body) {
		t.Fatalf("中转下载的字节不对：size=%d len=%d", meta.Size, len(streamed))
	}

	if err := svc.DeleteFile(ctx, owner, f.ID); err != nil {
		t.Fatal(err)
	}
}

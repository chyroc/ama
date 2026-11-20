// Tests for the read_object tool: prefix enforcement, key validation, max_bytes clamping, Range
// truncation, 404 mapping and non-UTF-8 handling.
//
// Everything runs against a fake objectStore, so no TOS (and no network) is involved.

package datasources

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// fakeObjectStore is an in-memory objectStore with the same truncation semantics as the real one.
type fakeObjectStore struct {
	objects  map[string][]byte
	getErr   error
	listErr  error
	delay    time.Duration
	getCalls []getObjectCall
}

type getObjectCall struct {
	bucket   string
	key      string
	maxBytes int64
}

func (f *fakeObjectStore) GetObject(
	ctx context.Context, bucket, key string, maxBytes int64,
) ([]byte, bool, error) {
	f.getCalls = append(f.getCalls, getObjectCall{bucket: bucket, key: key, maxBytes: maxBytes})
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
	}
	if f.getErr != nil {
		return nil, false, f.getErr
	}
	body, ok := f.objects[key]
	if !ok {
		return nil, false, fmt.Errorf("%w: NoSuchKey", errObjectNotFound)
	}
	if int64(len(body)) > maxBytes {
		return body[:maxBytes], true, nil
	}
	return body, false, nil
}

func (f *fakeObjectStore) ListKeys(
	ctx context.Context, bucket, prefix string, max int,
) ([]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	keys := make([]string, 0, max)
	for key := range f.objects {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		keys = append(keys, key)
		if len(keys) >= max {
			break
		}
	}
	return keys, nil
}

// testTOSConfig is a complete, enabled TOS config.
func testTOSConfig() TOSConfig {
	return TOSConfig{
		Enabled:   true,
		Endpoint:  "https://tos-s3-cn-beijing.volces.com",
		Region:    "cn-beijing",
		Bucket:    "example-bucket",
		Prefix:    "example-prefix/",
		AccessKey: "AK-test",
		SecretKey: "SK-test",
		MaxBytes:  1024,
		Timeout:   2 * time.Second,
	}
}

// newTestReadObjectTool builds the tool under test.
func newTestReadObjectTool(
	cfg TOSConfig, obj objectStore, logger *slog.Logger,
) *readObjectTool {
	return &readObjectTool{
		src:       &tosSource{cfg: cfg, obj: obj},
		sessionID: "sess-1",
		workID:    "work-1",
		logger:    logger,
	}
}

// runReadObject marshals the arguments and executes.
func runReadObject(t *testing.T, tool *readObjectTool, key string, maxBytes int64) (text string, isError bool) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"key": key, "max_bytes": maxBytes})
	if err != nil {
		t.Fatalf("构造入参失败：%v", err)
	}
	result := tool.Execute(context.Background(), body)
	if len(result.Content) == 0 {
		t.Fatalf("结果里没有内容块")
	}
	return result.Content[0].Text, result.IsError
}

// TestReadObjectHappyPath: text content comes back with bucket/key/bytes metadata.
func TestReadObjectHappyPath(t *testing.T) {
	store := &fakeObjectStore{objects: map[string][]byte{
		"example-prefix/reviews.csv": []byte("id,name\n1,张三\n"),
	}}
	tool := newTestReadObjectTool(testTOSConfig(), store, nil)

	text, isError := runReadObject(t, tool, "example-prefix/reviews.csv", 0)
	if isError {
		t.Fatalf("不该是错误结果：%s", text)
	}
	for _, want := range []string{
		"桶 example-bucket",
		"key example-prefix/reviews.csv",
		"17 字节",
		"（完整）",
		"id,name",
		"1,张三",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("结果里应当有 %q，实际：\n%s", want, text)
		}
	}
	if len(store.getCalls) != 1 {
		t.Fatalf("应当读一次，实际 %d 次", len(store.getCalls))
	}
	if got := store.getCalls[0]; got.bucket != "example-bucket" || got.maxBytes != 1024 {
		t.Errorf("读取参数不对：bucket=%q maxBytes=%d", got.bucket, got.maxBytes)
	}
}

// TestReadObjectKeyRejected: everything outside the configured prefix is refused **before** any
// network call, with a message that says what the allowed range is.
func TestReadObjectKeyRejected(t *testing.T) {
	cases := []struct {
		name string
		key  string
		want string
	}{
		{"empty", "", "不能为空"},
		{"leading slash", "/example-prefix/a.csv", "不能以 / 开头"},
		{"url", "https://tos.example.com/example-bucket/example-prefix/a.csv", "不能是 URL"},
		{"parent segment", "example-prefix/../other/a.csv", "路径段"},
		{"current segment", "example-prefix/./a.csv", "路径段"},
		{"outside prefix", "other-business/a.csv", "只允许读取前缀"},
		{"prefix lookalike", "example-prefix-secret/a.csv", "只允许读取前缀"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeObjectStore{objects: map[string][]byte{}}
			tool := newTestReadObjectTool(testTOSConfig(), store, nil)
			text, isError := runReadObject(t, tool, tc.key, 0)
			if !isError {
				t.Fatalf("应当拒绝：%s", text)
			}
			if !strings.Contains(text, tc.want) {
				t.Errorf("错误文案里应当有 %q，实际：%q", tc.want, text)
			}
			if len(store.getCalls) != 0 {
				t.Error("被拒绝的 key 不该发起读取")
			}
		})
	}
}

// TestReadObjectPrefixIsNormalized: a prefix without a trailing slash must not become a prefix
// match on "example-prefix-secret/...".
func TestReadObjectPrefixIsNormalized(t *testing.T) {
	cfg := testTOSConfig()
	cfg.Prefix = "example-prefix"
	if got := cfg.prefix(); got != "example-prefix/" {
		t.Fatalf("prefix 应当补斜杠，实际 %q", got)
	}
	store := &fakeObjectStore{objects: map[string][]byte{}}
	tool := newTestReadObjectTool(cfg, store, nil)
	if _, isError := runReadObject(t, tool, "example-prefix-secret/x.csv", 0); !isError {
		t.Fatal("相似前缀的对象必须被拒绝")
	}
}

// TestReadObjectMaxBytesClamp: max_bytes can only lower the configured cap.
func TestReadObjectMaxBytesClamp(t *testing.T) {
	cases := []struct {
		name     string
		request  int64
		wantRead int64
	}{
		{"zero means config", 0, 1024},
		{"negative means config", -1, 1024},
		{"huge is clamped", 1 << 30, 1024},
		{"smaller is honoured", 100, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeObjectStore{objects: map[string][]byte{
				"example-prefix/a.csv": []byte("hello"),
			}}
			tool := newTestReadObjectTool(testTOSConfig(), store, nil)
			if _, isError := runReadObject(t, tool, "example-prefix/a.csv", tc.request); isError {
				t.Fatal("不该是错误结果")
			}
			if got := store.getCalls[0].maxBytes; got != tc.wantRead {
				t.Errorf("max_bytes 期望 %d，实际 %d", tc.wantRead, got)
			}
		})
	}
}

// TestReadObjectTruncated: a longer object comes back truncated, with the cap in the metadata.
func TestReadObjectTruncated(t *testing.T) {
	body := strings.Repeat("a", 2000)
	store := &fakeObjectStore{objects: map[string][]byte{
		"example-prefix/big.csv": []byte(body),
	}}
	tool := newTestReadObjectTool(testTOSConfig(), store, nil)
	text, isError := runReadObject(t, tool, "example-prefix/big.csv", 0)
	if isError {
		t.Fatalf("不该是错误结果：%s", text)
	}
	if !strings.Contains(text, "已截断，上限 1024 字节") {
		t.Errorf("截断标记不对：%s", text[:200])
	}
	if !strings.Contains(text, strings.Repeat("a", 1024)) {
		t.Error("应当返回前 1024 字节")
	}
	if strings.Contains(text, strings.Repeat("a", 1100)) {
		t.Error("返回内容超过了 max_bytes")
	}
}

// TestReadObjectTruncatedAtRuneBoundary: cutting at max_bytes may split a multi-byte character;
// that last character is dropped instead of reporting "not UTF-8".
func TestReadObjectTruncatedAtRuneBoundary(t *testing.T) {
	// Four Chinese characters (12 bytes); a 4-byte cut splits the second character in half.
	store := &fakeObjectStore{objects: map[string][]byte{
		"example-prefix/cn.txt": []byte("你好世界"),
	}}
	cfg := testTOSConfig()
	cfg.MaxBytes = 4
	tool := newTestReadObjectTool(cfg, store, nil)
	text, isError := runReadObject(t, tool, "example-prefix/cn.txt", 0)
	if isError {
		t.Fatalf("末尾半个字符应当被丢掉，而不是报错：%s", text)
	}
	if !strings.Contains(text, "你") || strings.Contains(text, "好") {
		t.Errorf("应当只返回第一个完整字符，实际：%q", text)
	}
	if !strings.Contains(text, "已截断") {
		t.Errorf("应当标记截断：%q", text)
	}
}

// TestReadObjectNotFound: 404 becomes a Chinese "object does not exist" message.
func TestReadObjectNotFound(t *testing.T) {
	store := &fakeObjectStore{objects: map[string][]byte{}}
	tool := newTestReadObjectTool(testTOSConfig(), store, nil)
	text, isError := runReadObject(t, tool, "example-prefix/missing.csv", 0)
	if !isError {
		t.Fatal("不存在的对象应当是错误结果")
	}
	if !strings.Contains(text, "对象不存在") {
		t.Errorf("错误文案不对：%q", text)
	}
}

// TestReadObjectNonUTF8: binary objects are refused instead of being stuffed into the model's
// context.
func TestReadObjectNonUTF8(t *testing.T) {
	store := &fakeObjectStore{objects: map[string][]byte{
		"example-prefix/data.bin": {0xff, 0xfe, 0x00, 0x01},
	}}
	tool := newTestReadObjectTool(testTOSConfig(), store, nil)
	text, isError := runReadObject(t, tool, "example-prefix/data.bin", 0)
	if !isError {
		t.Fatal("二进制对象应当是错误结果")
	}
	if !strings.Contains(text, "不是 UTF-8 文本") || !strings.Contains(text, "只支持文本对象") {
		t.Errorf("错误文案不对：%q", text)
	}
}

// TestReadObjectTimeout: the per-call timeout becomes a clear message.
func TestReadObjectTimeout(t *testing.T) {
	store := &fakeObjectStore{
		objects: map[string][]byte{"example-prefix/a.csv": []byte("x")},
		delay:   500 * time.Millisecond,
	}
	cfg := testTOSConfig()
	cfg.Timeout = 10 * time.Millisecond
	tool := newTestReadObjectTool(cfg, store, nil)
	start := time.Now()
	text, isError := runReadObject(t, tool, "example-prefix/a.csv", 0)
	if !isError {
		t.Fatal("超时应当是错误结果")
	}
	if !strings.Contains(text, "读取超时") {
		t.Errorf("错误文案不对：%q", text)
	}
	if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
		t.Errorf("超时没生效，等了 %v", elapsed)
	}
}

// TestReadObjectSecretRedacted: credentials must never appear in a tool result.
func TestReadObjectSecretRedacted(t *testing.T) {
	cfg := testTOSConfig()
	store := &fakeObjectStore{getErr: fmt.Errorf("403 signature mismatch for key %s", cfg.SecretKey)}
	tool := newTestReadObjectTool(cfg, store, nil)
	text, _ := runReadObject(t, tool, "example-prefix/a.csv", 0)
	if strings.Contains(text, cfg.SecretKey) {
		t.Errorf("结果里不能出现 secretKey：%q", text)
	}
}

// TestReadObjectAuditLog: the audit line carries session/work id, key, bytes, truncation.
func TestReadObjectAuditLog(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	store := &fakeObjectStore{objects: map[string][]byte{
		"example-prefix/a.csv": []byte("hello"),
	}}
	tool := newTestReadObjectTool(testTOSConfig(), store, logger)
	if _, isError := runReadObject(t, tool, "example-prefix/a.csv", 0); isError {
		t.Fatal("不该是错误结果")
	}
	line := buf.String()
	for _, want := range []string{
		"datasource read_object",
		"tool=read_object",
		"session_id=sess-1",
		"work_id=work-1",
		"bytes=5",
		"truncated=false",
		"key=example-prefix/a.csv",
		"status=ok",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("审计日志里应当有 %q，实际：\n%s", want, line)
		}
	}
}

// TestReadObjectDeclaration: the description must tell the model the allowed prefix and the keys.
func TestReadObjectDeclaration(t *testing.T) {
	src := &tosSource{
		cfg:  testTOSConfig(),
		obj:  &fakeObjectStore{objects: map[string][]byte{}},
		keys: []string{"example-prefix/a.csv", "example-prefix/b.json"},
	}
	decl := src.declaration()
	if decl.Name != ToolNameReadObject {
		t.Errorf("声明名 %q 与常量 %q 不一致", decl.Name, ToolNameReadObject)
	}
	for _, want := range []string{
		"example-prefix/",
		"example-bucket",
		"UTF-8",
		"example-prefix/a.csv",
		"example-prefix/b.json",
		"只读",
	} {
		if !strings.Contains(decl.Description, want) {
			t.Errorf("工具说明里应当有 %q，实际：\n%s", want, decl.Description)
		}
	}
	tool := src.tool("s", "w", nil)
	if tool.Name() != decl.Name {
		t.Errorf("实现名 %q 与声明名 %q 不一致", tool.Name(), decl.Name)
	}
	if len(decl.InputSchema) != 2 {
		t.Errorf("入参 schema 应当有 key / max_bytes 两个属性，实际 %d 个", len(decl.InputSchema))
	}
	if len(decl.Required) != 1 || decl.Required[0] != "key" {
		t.Errorf("必填参数应当只有 key，实际 %v", decl.Required)
	}
}

// TestReadObjectDeclarationWithoutKeys: a failed probe degrades to generic wording, not an error.
func TestReadObjectDeclarationWithoutKeys(t *testing.T) {
	src := &tosSource{cfg: testTOSConfig(), obj: &fakeObjectStore{objects: map[string][]byte{}}}
	decl := src.declaration()
	if !strings.Contains(decl.Description, "没有探测到对象清单") {
		t.Errorf("没探测到 key 时应当说明，实际：%s", decl.Description)
	}
}

// TestTOSConfigDefaults: documented defaults and hard caps.
func TestTOSConfigDefaults(t *testing.T) {
	if got := (TOSConfig{}).maxBytes(); got != defaultTOSMaxBytes {
		t.Errorf("默认 maxBytes 期望 %d，实际 %d", defaultTOSMaxBytes, got)
	}
	if got := (TOSConfig{MaxBytes: hardTOSMaxBytes + 1}).maxBytes(); got != hardTOSMaxBytes {
		t.Errorf("maxBytes 超过硬上限应当夹到 %d，实际 %d", hardTOSMaxBytes, got)
	}
	if got := (TOSConfig{}).timeout(); got != defaultTOSTimeout {
		t.Errorf("默认 timeout 期望 %v，实际 %v", defaultTOSTimeout, got)
	}
}

// TestTOSEndpointNormalized: a scheme-less endpoint gets https:// (otherwise the SDK treats it as
// a relative path and the error does not point at the missing scheme).
func TestTOSEndpointNormalized(t *testing.T) {
	cases := map[string]string{
		"tos-s3-cn-beijing.volces.com":         "https://tos-s3-cn-beijing.volces.com",
		"https://tos-s3-cn-beijing.volces.com": "https://tos-s3-cn-beijing.volces.com",
		"http://127.0.0.1:9000":                "http://127.0.0.1:9000",
	}
	for in, want := range cases {
		if got := (TOSConfig{Endpoint: in}).endpoint(); got != want {
			t.Errorf("endpoint(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestTOSConfigValidate: enabled but incomplete must be an error, and prefix is required
// (fail-closed: no "read the whole bucket" fallback).
func TestTOSConfigValidate(t *testing.T) {
	valid := testTOSConfig()
	if err := valid.validate(); err != nil {
		t.Fatalf("完整配置不该报错：%v", err)
	}
	if err := (TOSConfig{}).validate(); err != nil {
		t.Errorf("未启用时不该校验字段：%v", err)
	}
	cases := []struct {
		name   string
		mutate func(*TOSConfig)
		want   string
	}{
		{"missing endpoint", func(c *TOSConfig) { c.Endpoint = "" }, "endpoint"},
		{"missing region", func(c *TOSConfig) { c.Region = "" }, "region"},
		{"missing bucket", func(c *TOSConfig) { c.Bucket = "" }, "bucket"},
		{"missing prefix", func(c *TOSConfig) { c.Prefix = "" }, "prefix"},
		{"blank prefix", func(c *TOSConfig) { c.Prefix = "   " }, "prefix"},
		{"missing access key", func(c *TOSConfig) { c.AccessKey = "" }, "accessKey"},
		{"missing secret key", func(c *TOSConfig) { c.SecretKey = "" }, "secretKey"},
		// A prefix that can never match a valid key would make every call fail with a message
		// pointing at the key, so it is rejected at startup instead.
		{"prefix with leading slash", func(c *TOSConfig) { c.Prefix = "/example-prefix/" }, "prefix"},
		{"prefix with parent segment", func(c *TOSConfig) { c.Prefix = "example-prefix/../other/" }, "prefix"},
		{"prefix with current segment", func(c *TOSConfig) { c.Prefix = "example-prefix/./x/" }, "prefix"},
		{"prefix is dots", func(c *TOSConfig) { c.Prefix = ".." }, "prefix"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid
			tc.mutate(&cfg)
			err := cfg.validate()
			if err == nil {
				t.Fatal("应当报错")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息里应当提到 %q，实际：%v", tc.want, err)
			}
		})
	}
}

// TestTrimIncompleteUTF8Suffix: only a partial trailing character is removed; invalid bytes in
// the middle are left for the caller's utf8.Valid check.
func TestTrimIncompleteUTF8Suffix(t *testing.T) {
	if got := trimIncompleteUTF8Suffix([]byte("你")); string(got) != "你" {
		t.Errorf("完整字符不该被改动：%q", got)
	}
	// One Chinese character is 3 bytes; keeping the first two bytes leaves it incomplete.
	if got := trimIncompleteUTF8Suffix([]byte("好")[:2]); len(got) != 0 {
		t.Errorf("不完整字符应当被去掉，实际 %q", got)
	}
	bad := []byte{'a', 0xff, 'b'}
	if got := trimIncompleteUTF8Suffix(bad); string(got) != string(bad) {
		t.Errorf("中间的非法字节不该被当作截断处理：%q", got)
	}
}

// TestListKeysProbe: the startup probe is bounded.
func TestProbeKeys(t *testing.T) {
	objects := make(map[string][]byte, maxProbeKeys+10)
	for i := 0; i < maxProbeKeys+10; i++ {
		objects[fmt.Sprintf("example-prefix/f_%03d.csv", i)] = []byte("x")
	}
	store := &fakeObjectStore{objects: objects}
	src := &tosSource{cfg: testTOSConfig(), obj: store}
	keys, err := store.ListKeys(context.Background(), src.cfg.Bucket, src.cfg.prefix(), maxProbeKeys)
	if err != nil {
		t.Fatalf("探测失败：%v", err)
	}
	if len(keys) != maxProbeKeys {
		t.Errorf("key 清单应当截到 %d 个，实际 %d 个", maxProbeKeys, len(keys))
	}
}

// TestProbeKeysErrorIsNotFatal: a 403 / unreachable endpoint only warns.
func TestProbeKeysErrorIsNotFatal(t *testing.T) {
	store := &fakeObjectStore{listErr: errors.New("403 Forbidden")}
	src := &tosSource{cfg: testTOSConfig(), obj: store}
	if _, err := store.ListKeys(context.Background(), src.cfg.Bucket, src.cfg.prefix(), maxProbeKeys); err == nil {
		t.Fatal("探测应当返回错误（由调用方 Warn）")
	}
	if decl := src.declaration(); decl.Name != ToolNameReadObject {
		t.Errorf("探测失败后仍应产出可用声明，实际 %q", decl.Name)
	}
}

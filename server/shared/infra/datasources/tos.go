// TOS (S3-compatible) datasource: the read_object tool.
//
// # Read-only, one prefix
//
// The TOS credentials are the only boundary on "which objects in the bucket may be read", and the
// tool can only tighten it, never widen it: the bucket is fixed in the config and a key must start
// with the configured prefix (a missing prefix fails startup, see TOSConfig.validate). So "one
// misconfigured field exposes another business directory" is a startup error here, not a runtime
// surprise.
//
// # Why aws-sdk-go-v2 instead of a vendor-specific TOS SDK
//
// TOS offers S3-compatible endpoints and the repo already depends on aws-sdk-go-v2/service/s3
// (the object-storage chain lives in shared/infra/s3), so reusing it avoids a second SDK and a
// second credential model. The differences are only the BaseEndpoint + UsePathStyle fields.
package datasources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/volcengine/ark-runtime-go/arkruntime/toolset"

	"github.com/chyroc/ama/server/shared/infra/ma"
)

const (
	// ToolNameReadObject is the tool name, shared by the declaration and the implementation.
	ToolNameReadObject = "read_object"

	defaultTOSMaxBytes = 1 << 20 // 1 MiB
	hardTOSMaxBytes    = 8 << 20 // 8 MiB

	defaultTOSTimeout = 20 * time.Second

	// maxProbeKeys caps how many keys go into the tool description.
	maxProbeKeys = 100
)

// errObjectNotFound means the object does not exist. The tool layer turns it into a clear message
// instead of handing the SDK's raw error to the model.
var errObjectNotFound = errors.New("datasources: 对象不存在")

// TOSConfig is one enabled TOS datasource.
//
// With Enabled=false the other fields may be empty; with Enabled=true endpoint / region / bucket /
// prefix / accessKey / secretKey are required.
type TOSConfig struct {
	Enabled  bool
	Endpoint string
	Region   string
	Bucket   string
	// Prefix is required: only objects under this prefix may be read (fail-closed, no reading
	// other prefixes in the bucket).
	Prefix    string
	AccessKey string
	SecretKey string
	// MaxBytes is the configured per-read byte cap; 0 means the default 1MiB, values above the
	// hard cap 8MiB are clamped to 8MiB.
	MaxBytes int64
	// Timeout is the per-read timeout; 0 means the default 20s.
	Timeout time.Duration
	// UsePathStyle uses http://host/bucket/key; TOS's S3-compatible domain is virtual-hosted.
	UsePathStyle bool
}

// validate checks the required fields when the source is enabled. Requiring the prefix is the
// fail-closed key: allowing the whole bucket would hand every other business directory in it to
// the model.
func (c TOSConfig) validate() error {
	if !c.Enabled {
		return nil
	}
	var missing []string
	for _, f := range []struct {
		name  string
		value string
	}{
		{"endpoint", c.Endpoint},
		{"region", c.Region},
		{"bucket", c.Bucket},
		{"prefix", c.Prefix},
		{"accessKey", c.AccessKey},
		{"secretKey", c.SecretKey},
	} {
		if strings.TrimSpace(f.value) == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("tos.enabled=true 但缺少 %s", strings.Join(missing, " / "))
	}
	if _, err := url.Parse(c.endpoint()); err != nil {
		return fmt.Errorf("tos.endpoint 不合法: %v", err)
	}
	// The prefix is the only thing standing between the tool and the rest of the bucket, so a
	// prefix that can never match a valid key is a startup error, not a runtime "every call is
	// rejected with a message that points at the key".
	//
	// checkKey rejects keys that start with "/" or contain "." / ".." segments; a prefix with
	// those properties would make every key fail. Same rule here, reported against the prefix.
	prefix := c.prefix()
	if strings.HasPrefix(prefix, "/") {
		return fmt.Errorf("tos.prefix 不能以 / 开头（写相对桶的前缀，例如 data/）")
	}
	for _, seg := range strings.Split(prefix, "/") {
		if seg == "." || seg == ".." {
			return fmt.Errorf("tos.prefix 不能包含 %q 路径段", seg)
		}
	}
	return nil
}

// endpoint returns the endpoint with a scheme filled in. A scheme-less value gets https:// added;
// without that, the SDK treats the address as a relative path and the error message never points
// at the missing scheme.
func (c TOSConfig) endpoint() string {
	ep := strings.TrimSpace(c.Endpoint)
	if ep == "" || strings.Contains(ep, "://") {
		return ep
	}
	return "https://" + ep
}

// prefix returns the normalized prefix, always ending with "/".
//
// Without that trailing slash, `example-prefix` would also let `example-prefix-secret/...`
// through, while the person configuring it believes they opened exactly one directory.
func (c TOSConfig) prefix() string {
	p := strings.TrimSpace(c.Prefix)
	if p == "" || strings.HasSuffix(p, "/") {
		return p
	}
	return p + "/"
}

// maxBytes returns the effective byte cap (including the hard-cap clamp).
func (c TOSConfig) maxBytes() int64 {
	n := c.MaxBytes
	if n <= 0 {
		n = defaultTOSMaxBytes
	}
	if n > hardTOSMaxBytes {
		n = hardTOSMaxBytes
	}
	return n
}

// timeout returns the effective read timeout.
func (c TOSConfig) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return defaultTOSTimeout
}

// maxBytesFor clamps the tool's max_bytes parameter into [1, configured cap].
func (c TOSConfig) maxBytesFor(requested int64) int64 {
	limit := c.maxBytes()
	if requested <= 0 {
		return limit
	}
	if requested > limit {
		return limit
	}
	return requested
}

// checkKey validates an object key and returns the key to read.
//
// Rejected: an empty key, a key starting with "/", one containing "://", one with "." or ".."
// segments, and one outside the configured prefix.
func (c TOSConfig) checkKey(key string) (string, error) {
	k := strings.TrimSpace(key)
	if k == "" {
		return "", fmt.Errorf("key 不能为空")
	}
	if strings.HasPrefix(k, "/") {
		return "", fmt.Errorf("key 不能以 / 开头（请写相对桶的 key，例如 %sfoo.csv）", c.prefix())
	}
	if strings.Contains(k, "://") {
		return "", fmt.Errorf("key 只能是对象 key，不能是 URL")
	}
	if strings.ContainsRune(k, 0) {
		return "", fmt.Errorf("key 里不能有 NUL 字节")
	}
	for _, seg := range strings.Split(k, "/") {
		if seg == "." || seg == ".." {
			return "", fmt.Errorf("key 里不能有 %q 路径段", seg)
		}
	}
	prefix := c.prefix()
	if !strings.HasPrefix(k, prefix) {
		return "", fmt.Errorf("只允许读取前缀 %q 下的对象", prefix)
	}
	return k, nil
}

// objectStore is the minimum S3 surface read_object needs. The interface exists for offline
// tests: key validation, the truncation flag, 404 mapping and the non-UTF-8 decision are all
// covered with a fake, without a real TOS.
type objectStore interface {
	// GetObject reads at most maxBytes+1 bytes; truncated means the object is longer.
	GetObject(ctx context.Context, bucket, key string, maxBytes int64) (body []byte, truncated bool, err error)
	// ListKeys lists keys under the prefix (used for "what objects exist" in the tool description).
	ListKeys(ctx context.Context, bucket, prefix string, max int) ([]string, error)
}

// tosSource is one assembled TOS datasource.
type tosSource struct {
	cfg      TOSConfig
	obj      objectStore
	keys     []string
	redactor *strings.Replacer
}

// declaration builds the Ark-side tool declaration.
func (s *tosSource) declaration() ma.CustomToolSpec {
	var b strings.Builder
	b.WriteString("读取 TOS 对象存储里的文本对象（只读）。\n\n")
	b.WriteString("约束：\n")
	fmt.Fprintf(&b, "- 只能读桶 %s 里前缀 %q 下的对象，其它一律拒绝\n", s.cfg.Bucket, s.cfg.prefix())
	b.WriteString("- 只支持 UTF-8 文本（CSV / JSON / SQL / 日志等）；不要把二进制文件交给这个工具\n")
	fmt.Fprintf(&b, "- 单次最多读 %d 字节（可用 max_bytes 调低，不能调高），超出会截断并标注\n", s.cfg.maxBytes())
	if len(s.keys) > 0 {
		fmt.Fprintf(&b, "\n可用对象 key（启动时探测到 %d 个，最多列 %d 个）：\n%s\n",
			len(s.keys), maxProbeKeys, strings.Join(s.keys, "\n"))
	} else {
		b.WriteString("\n启动时没有探测到对象清单；key 的命名可以先向用户确认，再按前缀规则填写。\n")
	}
	return ma.CustomToolSpec{
		Name:        ToolNameReadObject,
		Description: b.String(),
		InputSchema: map[string]any{
			"key": map[string]any{
				"type":        "string",
				"description": fmt.Sprintf("对象 key，必须以 %q 开头。", s.cfg.prefix()),
			},
			"max_bytes": map[string]any{
				"type":        "integer",
				"description": fmt.Sprintf("本次最多读多少字节，默认 %d，只能调低不能调高。", s.cfg.maxBytes()),
			},
		},
		Required: []string{"key"},
	}
}

// tool creates the tool instance for one work item.
func (s *tosSource) tool(sessionID, workID string, logger *slog.Logger) toolset.Tool {
	return &readObjectTool{src: s, sessionID: sessionID, workID: workID, logger: logger}
}

// readObjectTool implements read_object.
type readObjectTool struct {
	src       *tosSource
	sessionID string
	workID    string
	logger    *slog.Logger
}

// Name implements toolset.Tool.
func (t *readObjectTool) Name() string { return ToolNameReadObject }

// readObjectInput is the tool input.
type readObjectInput struct {
	Key      string `json:"key"`
	MaxBytes int64  `json:"max_bytes"`
}

// Execute implements toolset.Tool.
func (t *readObjectTool) Execute(ctx context.Context, input json.RawMessage) toolset.Result {
	var args readObjectInput
	if len(input) > 0 {
		if err := json.Unmarshal(input, &args); err != nil {
			return toolset.ErrorResult("参数不是合法的 JSON：" + err.Error())
		}
	}
	key, err := t.src.cfg.checkKey(args.Key)
	if err != nil {
		t.audit(args.Key, 0, false, 0, err)
		return toolset.ErrorResult("key 被拒绝：" + err.Error())
	}
	maxBytes := t.src.cfg.maxBytesFor(args.MaxBytes)

	timeout := t.src.cfg.timeout()
	readCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	body, truncated, err := t.src.obj.GetObject(readCtx, t.src.cfg.Bucket, key, maxBytes)
	elapsed := time.Since(start)
	if err != nil {
		if errors.Is(err, errObjectNotFound) {
			t.audit(key, 0, false, elapsed, err)
			return toolset.ErrorResult(fmt.Sprintf(
				"对象不存在：%s。可用的 key 见工具说明（启动时探测的清单）；也可以先向用户确认文件名。", key))
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(readCtx.Err(), context.DeadlineExceeded) {
			t.audit(key, 0, false, elapsed, err)
			return toolset.ErrorResult(fmt.Sprintf("读取超时（上限 %s）：对象可能太大，先确认 key 是否正确。", timeout))
		}
		t.audit(key, 0, false, elapsed, err)
		return toolset.ErrorResult("读取对象失败：" + redactSecret(err.Error(), t.src.cfg.SecretKey))
	}

	// Truncation may split a multi-byte character: only the last character can be incomplete, at
	// most 3 bytes. The patch only runs when truncated, otherwise a genuinely invalid byte in the
	// middle would be "fixed" and treated as valid text.
	if truncated {
		body = trimIncompleteUTF8Suffix(body)
	}
	if !utf8.Valid(body) {
		t.audit(key, len(body), truncated, elapsed, nil)
		return toolset.ErrorResult(fmt.Sprintf(
			"对象 %s 不是 UTF-8 文本（%d 字节），read_object 只支持文本对象。"+
				"如果是二进制文件，请向用户说明无法读取。", key, len(body)))
	}

	t.audit(key, len(body), truncated, elapsed, nil)

	sanitizedBody := applyRedactor(t.src.redactor, string(body))

	var b strings.Builder
	fmt.Fprintf(&b, "桶 %s｜key %s｜%d 字节", t.src.cfg.Bucket, key, len(sanitizedBody))
	if truncated {
		fmt.Fprintf(&b, "（已截断，上限 %d 字节）", maxBytes)
	} else {
		b.WriteString("（完整）")
	}
	b.WriteString("｜耗时 ")
	fmt.Fprintf(&b, "%dms", elapsed.Milliseconds())
	b.WriteString("\n\n")
	b.WriteString(sanitizedBody)
	// read_object's text cap is the configured max_bytes: body has already been cut to it, so
	// this only leaves room for the metadata line. The SQL result's 256KB hard cap does not apply
	// here; that one exists for queries whose row count is unbounded.
	return toolset.TextResult(capTextAt(b.String(), int(maxBytes)+1024,
		fmt.Sprintf("（结果文本超过 %d 字节，已截断；需要更多请分段读取）", maxBytes)))
}

// audit writes the audit line; fields line up with query_sql: tool / session_id / work_id /
// duration_ms / bytes / truncated / key / status.
func (t *readObjectTool) audit(
	key string, size int, truncated bool, elapsed time.Duration, err error,
) {
	logger := t.logger
	if logger == nil {
		logger = slog.Default()
	}
	attrs := []any{
		slog.String("tool", ToolNameReadObject),
		slog.String("session_id", t.sessionID),
		slog.String("work_id", t.workID),
		slog.Int64("duration_ms", elapsed.Milliseconds()),
		slog.Int("bytes", size),
		slog.Bool("truncated", truncated),
		slog.String("key", key),
	}
	if err != nil {
		attrs = append(attrs, slog.String("status", "error"), slog.Any("err", err))
		logger.Warn("datasource read_object", attrs...)
		return
	}
	attrs = append(attrs, slog.String("status", "ok"))
	logger.Info("datasource read_object", attrs...)
}

// trimIncompleteUTF8Suffix drops the one truncated UTF-8 character at the end (at most 3 bytes).
//
// Only "the last character was cut in half" is handled: walk back over continuation bytes, find
// the start byte of the last character, and drop it if it does not decode to a complete rune.
// **Bytes that are invalid in the middle are left alone**: those must be judged "not a text
// object" by the caller's utf8.Valid check, not quietly repaired here.
func trimIncompleteUTF8Suffix(b []byte) []byte {
	if utf8.Valid(b) {
		return b
	}
	for i := len(b) - 1; i >= 0 && len(b)-i <= utf8.UTFMax; i-- {
		if b[i] < utf8.RuneSelf {
			// An ASCII byte is a complete character, so the tail is not half a character.
			return b
		}
		if !utf8.RuneStart(b[i]) {
			continue
		}
		if _, size := utf8.DecodeRune(b[i:]); size <= 1 {
			return b[:i]
		}
		return b
	}
	return b
}

// s3ObjectStore is the real objectStore implementation (the aws-sdk-go-v2 S3 client).
type s3ObjectStore struct {
	client *awss3.Client
}

// openS3ObjectStore builds the client. **No network IO here**: connectivity is the startup
// probe's (ListObjectsV2) and each tool call's job, and a failed probe only logs a Warn.
func openS3ObjectStore(cfg TOSConfig) (objectStore, error) {
	region := strings.TrimSpace(cfg.Region)
	if region == "" {
		return nil, errors.New("tos.region 不能为空")
	}
	creds := credentials.NewStaticCredentialsProvider(strings.TrimSpace(cfg.AccessKey), cfg.SecretKey, "")
	client := awss3.New(awss3.Options{
		Region:       region,
		Credentials:  creds,
		UsePathStyle: cfg.UsePathStyle,
		BaseEndpoint: aws.String(cfg.endpoint()),
		// Same as shared/infra/s3: only calculate / validate checksums when the protocol
		// requires it. Newer SDK versions compute CRC32 for every request and send an
		// aws-chunked trailer checksum, which many S3-compatible services reject (400, or chunk
		// headers mixed into the object bytes).
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		HTTPClient:                 awshttp.NewBuildableClient(),
	})
	return &s3ObjectStore{client: client}, nil
}

// GetObject reads one extra byte (Range's end is inclusive) so truncated can be decided.
func (s *s3ObjectStore) GetObject(
	ctx context.Context, bucket, key string, maxBytes int64,
) ([]byte, bool, error) {
	out, err := s.client.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		// `bytes=0-n` is an inclusive range, so n = maxBytes yields maxBytes+1 bytes.
		Range: aws.String(fmt.Sprintf("bytes=0-%d", maxBytes)),
	})
	if err != nil {
		if isObjectNotFound(err) {
			return nil, false, fmt.Errorf("%w: %v", errObjectNotFound, err)
		}
		return nil, false, err
	}
	defer func() { _ = out.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(out.Body, maxBytes+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(body)) > maxBytes {
		return body[:maxBytes], true, nil
	}
	return body, false, nil
}

// ListKeys lists keys under the prefix (for the startup probe, at most max of them).
func (s *s3ObjectStore) ListKeys(
	ctx context.Context, bucket, prefix string, max int,
) ([]string, error) {
	out, err := s.client.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{
		Bucket:  aws.String(bucket),
		Prefix:  aws.String(prefix),
		MaxKeys: aws.Int32(int32(max)),
	})
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(out.Contents))
	for _, obj := range out.Contents {
		if obj.Key == nil {
			continue
		}
		keys = append(keys, *obj.Key)
		if len(keys) >= max {
			break
		}
	}
	return keys, nil
}

// isObjectNotFound recognises "the object / bucket does not exist".
//
// S3-compatible services do not agree on error codes (NoSuchKey / NotFound / NoSuchBucket / a
// bare 404), so both the APIError code and the HTTP status code are checked.
func isObjectNotFound(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "NoSuchBucket", "nosuchkey":
			return true
		}
	}
	var respErr *awshttp.ResponseError
	if errors.As(err, &respErr) && respErr.HTTPStatusCode() == 404 {
		return true
	}
	return false
}

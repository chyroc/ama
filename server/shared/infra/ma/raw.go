// Byte-for-byte pass-through channel for Ark HTTP APIs.
//
// # Why this layer exists on top of the SDK's typed methods
//
// This package offers a *Raw method for every resource (e.g. ListThreadsRaw), but their inputs
// and outputs are still SDK-generated structs. After one decode → encode round trip, **fields
// newly added by Ark are silently dropped**, and wrapper types like OptString may also change
// how a field is shaped.
// AMA has a group of APIs that must forward Ark's responses to the caller **byte for byte** (see
// core/arkproxy.go). Every extra re-encode on that path is a chance to drift: field names,
// nesting and how empty values look can all change, and "it compiles, one field is missing"
// gives no error at all.
//
// So RawDo does no JSON processing: it sends the request body bytes as given and returns the
// response body as read. The same holds for multipart upload (RawUpload) and long-lived SSE
// connections (RawStream).
//
// # How errors are reported
//
// Transport failures (DNS / connect / read timeout) return a plain error. When Ark replies with
// 4xx/5xx, **RawResponse and *APIError are returned together**: the caller can forward resp.Body
// to the client as is, and can also use AsAPIError to get the structured code / request_id. The
// two do not exclude each other.
//
// # Current limits
//
// Only an Instance built by ma.New can use pass-through (that is when we know baseURL and
// apiKey). NewWithClient takes an SDK client built by the caller, so baseURL is unknown, and the
// pass-through methods return ErrRawUnsupported.

package ma

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/volcengine/ark-runtime-go/arkruntime/model"
)

// ErrRawUnsupported: the Instance was not built with ma.New, so it lacks the baseURL / apiKey
// that pass-through needs.
var ErrRawUnsupported = errors.New("ma: 该 Instance 不支持字节级透传（NewWithClient 构造时缺少 baseURL）")

// rawMaxResponseBody is the read limit for pass-through response bodies.
//
// Normal Ark list responses are at the MB level (at most 1000 items per page), so 50 MiB is
// enough. Without a limit, Ark (or a device in between) could use an endless response to eat all
// of amad's memory.
const rawMaxResponseBody = 50 << 20

// rawCallTimeout is the timeout for ordinary (non-streaming) pass-through requests.
//
// It is looser than arkrun.callTimeout (30s): bodies for APIs like file upload or bulk memory
// writes can be large, and 30s is not enough on a slow network. SSE goes through RawStream,
// which uses a client with no Timeout and is not bound by this.
const rawCallTimeout = 2 * time.Minute

// rawClient holds everything needed to send HTTP requests itself, without reusing the SDK's
// client: the SDK does not export an entry point that "sends a raw body and returns a raw body".
type rawClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
	stream  *http.Client
}

// configureRaw sets up the pass-through channel after newInstance, using the config ma.New got.
// When neither region nor baseURL is given it uses the SDK's default address (Beijing), matching
// SDK behavior.
func (in *Instance) configureRaw(apiKey string, o options) {
	baseURL := strings.TrimRight(o.baseURL, "/")
	if baseURL == "" {
		// Keep in sync with the SDK's preset list (arkruntime/config.go).
		switch o.region {
		case "ap-southeast-1":
			baseURL = "https://ark.ap-southeast.bytepluses.com/api/v3"
		default:
			baseURL = "https://ark.cn-beijing.volces.com/api/v3"
		}
	}
	in.raw = &rawClient{
		baseURL: baseURL,
		apiKey:  apiKey,
		http:    &http.Client{Timeout: rawCallTimeout},
		stream:  &http.Client{Timeout: 0},
	}
}

// RawRequest is one byte-for-byte pass-through request.
type RawRequest struct {
	// Method is the HTTP method.
	Method string
	// Path is the path after /api/v3. It must start with / and may contain escaped content,
	// e.g. /sessions/sesn-xxx/resources/%7Bid%7D.
	Path string
	// Query is appended to the URL as is, including bracketed params like created_at[gt].
	Query url.Values
	// Body is the raw request body (JSON, or a whole multipart message). nil means no body.
	Body []byte
	// ContentType overrides the request header; when empty and Body is set it defaults to
	// application/json. For file uploads pass the multipart writer's FormDataContentType()
	// (which contains the boundary).
	ContentType string
}

// RawResponse is Ark's raw response. It is still valid when the status is not 2xx (Body is Ark's
// error JSON).
type RawResponse struct {
	StatusCode int
	// Header is mainly kept for checking Content-Type; callers rarely need it.
	Header http.Header
	Body   []byte
}

// RawDo sends one ordinary (non-streaming) HTTP request and reads the whole response body.
func (in *Instance) RawDo(ctx context.Context, req RawRequest) (*RawResponse, error) {
	rc, err := in.rawReady()
	if err != nil {
		return nil, err
	}
	resp, err := rc.do(ctx, rc.http, req, bytes.NewReader(req.Body))
	if err != nil {
		return nil, err
	}
	body, err := readLimited(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	out := &RawResponse{StatusCode: resp.StatusCode, Header: resp.Header, Body: body}
	if resp.StatusCode >= 300 {
		return out, parseAPIError(resp.StatusCode, body)
	}
	return out, nil
}

// RawStream opens a streaming response (SSE) and returns the response body **unclosed and
// unread**.
//
// The caller must Close the returned ReadCloser. It uses a client with no Timeout; the lifetime
// is controlled by ctx. When ctx is cancelled the server response body is closed and a blocked
// Read returns at once.
//
// Unlike RawDo, this does not read the body and does not parse 4xx into *APIError: the caller
// must check StatusCode first to decide whether it is reading error JSON or an event stream. On
// non-2xx the ReadCloser holds the error response body; read it and Close.
func (in *Instance) RawStream(ctx context.Context, req RawRequest) (*RawStream, error) {
	rc, err := in.rawReady()
	if err != nil {
		return nil, err
	}
	resp, err := rc.do(ctx, rc.stream, req, nil)
	if err != nil {
		return nil, err
	}
	return &RawStream{StatusCode: resp.StatusCode, Header: resp.Header, body: resp.Body}, nil
}

// RawStream is a streaming pass-through response.
type RawStream struct {
	StatusCode int
	Header     http.Header
	body       io.ReadCloser
}

// NewRawStream builds a RawStream.
//
// Production code only gets one from the RawStream method. The constructor is exported because
// arkrun.Runner is an interface, and its test double (fake runner) needs a way to build a
// *RawStream. The body field is deliberately unexported (so callers cannot swap out the real
// connection).
func NewRawStream(statusCode int, header http.Header, body io.ReadCloser) *RawStream {
	return &RawStream{StatusCode: statusCode, Header: header, body: body}
}

func (s *RawStream) Read(p []byte) (int, error) { return s.body.Read(p) }
func (s *RawStream) Close() error               { return s.body.Close() }

const jsonContentType = "application/json"

func (in *Instance) rawReady() (*rawClient, error) {
	if in.raw == nil {
		return nil, ErrRawUnsupported
	}
	return in.raw, nil
}

func (rc *rawClient) do(
	ctx context.Context,
	httpClient *http.Client,
	req RawRequest,
	body io.Reader,
) (*http.Response, error) {
	if req.Method == "" {
		return nil, errors.New("ma: 透传请求缺少 Method")
	}
	if !strings.HasPrefix(req.Path, "/") {
		return nil, fmt.Errorf("ma: 透传 Path 必须以 / 开头，得到 %q", req.Path)
	}
	u := rc.baseURL + req.Path
	if len(req.Query) > 0 {
		u += "?" + req.Query.Encode()
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, u, body)
	if err != nil {
		return nil, fmt.Errorf("ma: 构造透传请求失败: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+rc.apiKey)
	contentType := req.ContentType
	if contentType == "" && len(req.Body) > 0 {
		contentType = jsonContentType
	}
	if contentType != "" {
		httpReq.Header.Set("Content-Type", contentType)
	}
	httpReq.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("ma: 请求方舟失败: %w", err)
	}
	return resp, nil
}

func readLimited(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, rawMaxResponseBody+1))
	if err != nil {
		return nil, fmt.Errorf("ma: 读方舟响应失败: %w", err)
	}
	if len(data) > rawMaxResponseBody {
		return nil, fmt.Errorf("ma: 方舟响应体超过 %d 字节上限", rawMaxResponseBody)
	}
	return data, nil
}

// parseAPIError tries to turn Ark's error JSON back into an *APIError. If it cannot parse it, it
// builds a generic error with a summary of the raw text, so the caller always gets a status code
// instead of nil.
func parseAPIError(status int, body []byte) error {
	apiErr := &model.APIError{HTTPStatusCode: status}
	var envelope model.ErrorResponse
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error != nil && envelope.Error.Message != "" {
		envelope.Error.HTTPStatusCode = status
		return envelope.Error
	}
	apiErr.Code = "upstream_error"
	apiErr.Message = fmt.Sprintf("方舟返回了 HTTP %d", status)
	return apiErr
}

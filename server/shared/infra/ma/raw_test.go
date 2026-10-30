// Pass-through behavior tests for RawDo / RawStream: request bytes are sent as is, response
// bytes are returned as is, 4xx gives both *APIError and the raw response, multipart messages
// are forwarded as is, and brackets in the query are not encoded wrongly.

package ma

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func newRawTestInstance(t *testing.T, h http.HandlerFunc) *Instance {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	ins, err := New("test-key", WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("构造 Instance 失败：%v", err)
	}
	return ins
}

func TestRawDoForwardsBytesAndAuth(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotCT string
	var gotQuery url.Values
	var gotBody []byte
	ins := newRawTestInstance(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.Query()
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"x","new_field":1}],"next_page":null}`))
	})

	body := []byte(`{"type":"file","file_id":"file_1","weird_shape":{"a":[1,2]}}`)
	resp, err := ins.RawDo(context.Background(), RawRequest{
		Method: http.MethodPost,
		Path:   "/sessions/sesn-1/resources",
		Query:  url.Values{"created_at[gt]": []string{"2026-09-01T00:00:00Z"}, "types": []string{"a", "b"}},
		Body:   body,
	})
	if err != nil {
		t.Fatalf("RawDo 失败：%v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/sessions/sesn-1/resources" {
		t.Errorf("请求行动了：%s %s", gotMethod, gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	if string(gotBody) != string(body) {
		t.Errorf("请求体被改过：%s", gotBody)
	}
	if gotQuery.Get("created_at[gt]") != "2026-09-01T00:00:00Z" {
		t.Errorf("方括号 query 丢了：%v", gotQuery)
	}
	if gotQuery["types"][0] != "a" || gotQuery["types"][1] != "b" {
		t.Errorf("多值 query 丢了：%v", gotQuery["types"])
	}
	// Response bytes as is (including fields newly added by Ark).
	if !strings.Contains(string(resp.Body), `"new_field":1`) {
		t.Errorf("响应体不是原样：%s", resp.Body)
	}
	if resp.StatusCode != 200 {
		t.Errorf("StatusCode = %d", resp.StatusCode)
	}
}

func TestRawDoUpstreamErrorGivesBodyAndAPIError(t *testing.T) {
	ins := newRawTestInstance(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"ResourceNotFound","message":"没这个会话","request_id":"rid-1"}}`))
	})

	resp, err := ins.RawDo(context.Background(), RawRequest{Method: http.MethodGet, Path: "/sessions/x"})
	if resp == nil {
		t.Fatal("4xx 时 resp 不该为 nil")
	}
	if resp.StatusCode != 404 || !strings.Contains(string(resp.Body), "rid-1") {
		t.Errorf("原始错误响应没保留：%+v", resp)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("想要 *APIError，得到 %T %v", err, err)
	}
	if apiErr.Code != "ResourceNotFound" || apiErr.RequestId != "rid-1" || apiErr.HTTPStatusCode != 404 {
		t.Errorf("APIError 解析错：%+v", apiErr)
	}
}

func TestRawDoContentTypeOverrideForMultipart(t *testing.T) {
	var gotCT string
	var body []byte
	ins := newRawTestInstance(t, func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		body, _ = io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "file-1"})
	})
	mp := "--BOUNDARY\r\nContent-Disposition: form-data; name=\"purpose\"\r\n\r\nagent\r\n" +
		"--BOUNDARY--\r\n"
	resp, err := ins.RawDo(context.Background(), RawRequest{
		Method:      http.MethodPost,
		Path:        "/files",
		Body:        []byte(mp),
		ContentType: "multipart/form-data; boundary=BOUNDARY",
	})
	if err != nil {
		t.Fatalf("RawDo 失败：%v", err)
	}
	if !strings.HasPrefix(gotCT, "multipart/form-data; boundary=") {
		t.Errorf("multipart Content-Type 没透传：%q", gotCT)
	}
	if !strings.Contains(string(body), "name=\"purpose\"") {
		t.Errorf("multipart 报文没原样转发：%q", body)
	}
	if !strings.Contains(string(resp.Body), `"id":"file-1"`) {
		t.Errorf("响应没原样返回：%s", resp.Body)
	}
}

func TestRawStreamReturnsUpstreamBodyAndStatus(t *testing.T) {
	ins := newRawTestInstance(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)
		_, _ = w.Write([]byte(": ready\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("data: {\"type\":\"agent.message\"}\n\n"))
		flusher.Flush()
	})
	stream, err := ins.RawStream(context.Background(), RawRequest{Method: http.MethodGet, Path: "/sessions/x/threads/y/stream"})
	if err != nil {
		t.Fatalf("RawStream 失败：%v", err)
	}
	defer stream.Close()
	if stream.StatusCode != 200 {
		t.Fatalf("StatusCode = %d", stream.StatusCode)
	}
	data, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("读流失败：%v", err)
	}
	if !strings.Contains(string(data), ": ready") || !strings.Contains(string(data), "agent.message") {
		t.Errorf("流内容不是原样：%q", data)
	}
}

func TestRawStreamUpstream4xx(t *testing.T) {
	ins := newRawTestInstance(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"BadRequest","message":"线程不存在"}}`))
	})
	stream, err := ins.RawStream(context.Background(), RawRequest{Method: http.MethodGet, Path: "/sessions/x/threads/y/stream"})
	if err != nil {
		t.Fatalf("RawStream 的握手错误不该直接返回（状态在流上）：%v", err)
	}
	defer stream.Close()
	if stream.StatusCode != 400 {
		t.Fatalf("StatusCode = %d", stream.StatusCode)
	}
	data, _ := io.ReadAll(stream)
	if !strings.Contains(string(data), "BadRequest") {
		t.Errorf("4xx 错误体没保留：%s", data)
	}
}

func TestRawRejectsBadPath(t *testing.T) {
	ins, _ := New("k", WithBaseURL("http://127.0.0.1:0"))
	if _, err := ins.RawDo(context.Background(), RawRequest{Method: "GET", Path: "sessions"}); err == nil {
		t.Fatal("不以 / 开头的 Path 应当报错")
	}
}

// Pass-through is only available on instances built by ma.New; this is guaranteed by the
// implementation (the raw field is only set in New). On the NewWithClient path RawDo returns
// ErrRawUnsupported.

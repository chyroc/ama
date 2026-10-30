// Package docker is a minimal Docker Engine API client: it implements only the endpoints that
// dockertools really uses.
//
// # Why not github.com/docker/docker/client
//
// The official SDK drags dozens of indirect dependencies into go.mod (containerd, opencontainers,
// grpc, otel...), while we only use a dozen or so REST endpoints: image inspect/pull, container
// create/start/stop/pause/unpause/inspect/remove/list, volume create/remove/inspect, and exec
// create/start/inspect/resize. These are all stable JSON over HTTP, and writing them directly with
// net/http makes each step easier to see.
//
// # Which daemon to connect to
//
// Same convention as the docker CLI: `DOCKER_HOST` first; if empty, `unix:///var/run/docker.sock`.
// Supports `unix://`, `tcp://` and `http://`; **TLS is not supported** (`DOCKER_TLS_VERIFY`).
// To reach a remote daemon, use an SSH tunnel or socket forwarding.
//
// # API version
//
// Every request has the `/v1.41` prefix (supported since Docker 20.10). Newer daemons stay
// backward compatible with this version; older daemons answer "client version too new", and that error is passed through as is.
package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// DefaultHost is the address used when DOCKER_HOST is not set.
const DefaultHost = "unix:///var/run/docker.sock"

// DefaultAPIVersion is the API version in the request path.
const DefaultAPIVersion = "v1.41"

// Config is the client configuration.
type Config struct {
	// Host is the daemon address, like unix:///var/run/docker.sock, tcp://127.0.0.1:2375,
	// http://127.0.0.1:2375. If empty, the DOCKER_HOST env var is used, then DefaultHost.
	Host string
	// APIVersion defaults to DefaultAPIVersion when empty.
	APIVersion string
}

// Client is a Docker Engine API client. It is safe for concurrent use.
type Client struct {
	hc      *http.Client
	base    string // like http://docker or http://127.0.0.1:2375
	version string
	host    string // the original Host, only used in logs and errors
	dial    func(ctx context.Context) (net.Conn, error)
}

// New creates a client. It does not touch the network; call Ping to find out if it can connect.
func New(cfg Config) (*Client, error) {
	host := strings.TrimSpace(cfg.Host)
	if host == "" {
		host = strings.TrimSpace(os.Getenv("DOCKER_HOST"))
	}
	if host == "" {
		host = DefaultHost
	}
	version := strings.TrimSpace(cfg.APIVersion)
	if version == "" {
		version = DefaultAPIVersion
	}
	if !strings.HasPrefix(version, "v") {
		version = "v" + version
	}

	u, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("docker: 解析地址 %q 失败: %w", host, err)
	}

	c := &Client{version: version, host: host}
	var dialer net.Dialer
	switch u.Scheme {
	case "unix":
		path := u.Path
		if path == "" {
			path = u.Host
		}
		c.base = "http://docker"
		c.dial = func(ctx context.Context) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", path)
		}
	case "tcp", "http":
		addr := u.Host
		c.base = "http://" + addr
		c.dial = func(ctx context.Context) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp", addr)
		}
	default:
		return nil, fmt.Errorf("docker: 不支持的地址 %q（只支持 unix:// tcp:// http://，不支持 TLS）", host)
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return c.dial(ctx)
		},
		MaxIdleConns:        16,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
		MaxIdleConnsPerHost: 16,
	}
	// No overall Timeout: exec output streams can last for minutes, so timeouts are left to the caller's ctx.
	c.hc = &http.Client{Transport: transport}
	return c, nil
}

// Host returns the daemon address in use (for logs).
func (c *Client) Host() string { return c.host }

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// APIError is a non-2xx response from the daemon.
type APIError struct {
	Op         string
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("docker: %s 失败（HTTP %d）: %s", e.Op, e.StatusCode, e.Message)
}

// IsNotFound reports whether this is a 404 (container / image / volume / exec does not exist).
func IsNotFound(err error) bool { return statusIs(err, http.StatusNotFound) }

// IsConflict reports whether this is a 409 (name already in use, container state does not allow
// the operation, etc.).
func IsConflict(err error) bool { return statusIs(err, http.StatusConflict) }

func statusIs(err error, code int) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.StatusCode == code
}

// ---------------------------------------------------------------------------
// Basic requests
// ---------------------------------------------------------------------------

func (c *Client) url(path string, q url.Values) string {
	u := c.base + "/" + c.version + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

// do sends a request. On non-2xx (and not in okExtra) it parses the response body into an APIError.
// The caller must close the returned Body.
func (c *Client) do(
	ctx context.Context, op, method, path string, q url.Values, body any, okExtra ...int,
) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("docker: %s 编码请求失败: %w", op, err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url(path, q), rd)
	if err != nil {
		return nil, fmt.Errorf("docker: %s 构造请求失败: %w", op, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker: %s 请求失败（daemon=%s）: %w", op, c.host, err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	for _, code := range okExtra {
		if resp.StatusCode == code {
			return resp, nil
		}
	}
	defer func() { _ = resp.Body.Close() }()
	return nil, readAPIError(op, resp)
}

func readAPIError(op string, resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	msg := strings.TrimSpace(string(raw))
	var payload struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &payload) == nil && payload.Message != "" {
		msg = payload.Message
	}
	return &APIError{Op: op, StatusCode: resp.StatusCode, Message: msg}
}

// doJSON sends a request and decodes a 2xx response body into out (the body is discarded when out is nil).
func (c *Client) doJSON(
	ctx context.Context, op, method, path string, q url.Values, body, out any, okExtra ...int,
) error {
	resp, err := c.do(ctx, op, method, path, q, body, okExtra...)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if out == nil || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("docker: %s 解析响应失败: %w", op, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// System
// ---------------------------------------------------------------------------

// Ping checks that the daemon is reachable.
func (c *Client) Ping(ctx context.Context) error {
	resp, err := c.do(ctx, "ping", http.MethodGet, "/_ping", nil, nil)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}

// SystemInfo holds the fields of GET /info that we use.
type SystemInfo struct {
	// ID uniquely identifies the daemon. Multiple amad instances use it to tell "are we on the same daemon".
	ID            string `json:"ID"`
	Name          string `json:"Name"`
	ServerVersion string `json:"ServerVersion"`
}

// Info fetches daemon info.
func (c *Client) Info(ctx context.Context) (*SystemInfo, error) {
	var out SystemInfo
	if err := c.doJSON(ctx, "info", http.MethodGet, "/info", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---------------------------------------------------------------------------
// Images
// ---------------------------------------------------------------------------

// ImageInfo holds the fields of image inspect that we use.
type ImageInfo struct {
	ID string `json:"Id"`
}

// ImageInspect checks whether the image exists locally; if not, it returns an error for which
// IsNotFound is true.
func (c *Client) ImageInspect(ctx context.Context, ref string) (*ImageInfo, error) {
	var out ImageInfo
	if err := c.doJSON(ctx, "镜像 inspect", http.MethodGet,
		"/images/"+url.PathEscape(ref)+"/json", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ImagePull pulls an image and blocks until it is done.
//
// The daemon answers with 200 plus a stream of JSON progress lines, and **a failed pull is also
// 200**: the error is hidden in the `error` field of some line. Looking only at the status code
// treats failure as success, so we read every line to the end.
func (c *Client) ImagePull(ctx context.Context, ref string) error {
	name, tag := splitImageRef(ref)
	q := url.Values{"fromImage": {name}}
	if tag != "" {
		q.Set("tag", tag)
	}
	resp, err := c.do(ctx, "拉镜像 "+ref, http.MethodPost, "/images/create", q, nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	dec := json.NewDecoder(resp.Body)
	for {
		var line struct {
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := dec.Decode(&line); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("docker: 读拉镜像进度失败: %w", err)
		}
		if msg := firstNonEmpty(line.ErrorDetail.Message, line.Error); msg != "" {
			return &APIError{Op: "拉镜像 " + ref, StatusCode: http.StatusInternalServerError, Message: msg}
		}
	}
}

// splitImageRef splits `repo:tag`; with a digest (@sha256:) the whole string is the name.
func splitImageRef(ref string) (string, string) {
	if strings.Contains(ref, "@") {
		return ref, ""
	}
	slash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if colon > slash {
		return ref[:colon], ref[colon+1:]
	}
	return ref, "latest"
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// hijack: exec stdin and TTY terminals need a raw two-way connection
// ---------------------------------------------------------------------------

// HijackedConn is the raw connection after POST /exec/{id}/start is upgraded.
//
// Reads return the daemon's output (multiplexed frames without TTY, raw bytes with TTY);
// writes go to the process's stdin.
type HijackedConn struct {
	conn net.Conn
	r    *bufio.Reader
}

func (h *HijackedConn) Read(p []byte) (int, error)  { return h.r.Read(p) }
func (h *HijackedConn) Write(p []byte) (int, error) { return h.conn.Write(p) }

// Close closes the whole connection.
func (h *HijackedConn) Close() error { return h.conn.Close() }

// CloseWrite half-closes the write side so the process sees EOF on stdin. Does nothing when the
// connection type does not support it.
func (h *HijackedConn) CloseWrite() error {
	if cw, ok := h.conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// SetReadDeadline / SetWriteDeadline are passed through to the underlying connection.
func (h *HijackedConn) SetReadDeadline(t time.Time) error  { return h.conn.SetReadDeadline(t) }
func (h *HijackedConn) SetWriteDeadline(t time.Time) error { return h.conn.SetWriteDeadline(t) }

// hijack sends a POST with `Upgrade: tcp` and returns the upgraded raw connection.
//
// It cannot use http.Client: the standard library Client does not hand over the connection after
// a 101. So we dial ourselves, write the request line ourselves, read the response header with
// http.ReadResponse, and hand the rest of the byte stream to the caller.
func (c *Client) hijack(ctx context.Context, op, path string, body any) (*HijackedConn, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("docker: %s 编码请求失败: %w", op, err)
	}
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("docker: %s 连接 daemon 失败（%s）: %w", op, c.host, err)
	}
	// The handshake follows the ctx deadline; it is cleared once the handshake is done (the stream may last a long time).
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	req, err := http.NewRequest(http.MethodPost, c.url(path, nil), bytes.NewReader(raw))
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("docker: %s 写请求失败: %w", op, err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("docker: %s 读响应失败: %w", op, err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols && resp.StatusCode != http.StatusOK {
		defer func() { _ = conn.Close() }()
		return nil, readAPIError(op, resp)
	}
	_ = conn.SetDeadline(time.Time{})
	return &HijackedConn{conn: conn, r: br}, nil
}

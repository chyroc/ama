// Package weixin is the wire protocol of WeChat's iLink bot API: QR-code login, long-polling for messages,
// sending messages, and the typing indicator.
//
// It is a Go port of the client side of Tencent/openclaw-weixin (MIT), the official OpenClaw channel
// plugin; see README.md in this directory for the source and what was and was not ported. This package
// only speaks the protocol: no storage, no business rules. server/weixin builds the channel on top.
package weixin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Defaults of the public iLink deployment.
const (
	// DefaultBaseURL is where QR-code login always starts. After login the server returns the API base
	// URL to use for that bot (Account.BaseURL), which may differ.
	DefaultBaseURL = "https://ilinkai.weixin.qq.com"
	// DefaultBotType is the bot_type of QR-code login used by the reference client.
	DefaultBotType = "3"
	// DefaultAppID is the iLink-App-Id header value of the reference client.
	DefaultAppID = "bot"
	// DefaultChannelVersion is the protocol version we implement: the openclaw-weixin release this port
	// follows. It is sent as base_info.channel_version and encoded into iLink-App-ClientVersion.
	DefaultChannelVersion = "2.4.10"
	// DefaultBotAgent identifies us in base_info.bot_agent (observability only, never used for auth).
	DefaultBotAgent = "AMA/1.0"
)

// Timeouts used by the reference client.
const (
	// DefaultLongPollTimeout is how long getupdates may be held by the server. The server can suggest
	// another value per response (GetUpdatesResponse.LongPollingTimeoutMs).
	DefaultLongPollTimeout = 35 * time.Second
	apiTimeout             = 15 * time.Second
	lightTimeout           = 10 * time.Second
)

// StaleTokenCode is the ret/errcode getupdates returns when the bot token is no longer valid
// (the user unbound the bot or logged in elsewhere). The reference client pauses the account for an hour.
const StaleTokenCode = -14

// Config configures a Client. Zero values use the defaults above.
type Config struct {
	// LoginBaseURL is where QR-code login starts. Default DefaultBaseURL.
	LoginBaseURL   string
	AppID          string
	ChannelVersion string
	BotAgent       string
	// RouteTag is the optional SKRouteTag header some deployments need.
	RouteTag string
	// HTTPClient defaults to a client without a global timeout (every call sets its own deadline).
	HTTPClient *http.Client
}

// Client talks to the iLink bot API. It is stateless and safe for concurrent use.
type Client struct {
	cfg  Config
	http *http.Client
}

// New creates a client.
func New(cfg Config) *Client {
	if cfg.LoginBaseURL == "" {
		cfg.LoginBaseURL = DefaultBaseURL
	}
	if cfg.AppID == "" {
		cfg.AppID = DefaultAppID
	}
	if cfg.ChannelVersion == "" {
		cfg.ChannelVersion = DefaultChannelVersion
	}
	if cfg.BotAgent == "" {
		cfg.BotAgent = DefaultBotAgent
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	}
	return &Client{cfg: cfg, http: hc}
}

// LoginBaseURL returns the base URL QR-code login starts from.
func (c *Client) LoginBaseURL() string { return c.cfg.LoginBaseURL }

// HTTPError is a non-2xx HTTP response.
type HTTPError struct {
	Op     string
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("weixin %s: HTTP %d: %s", e.Op, e.Status, truncate(e.Body, 200))
}

// APIError is a 2xx response whose ret / errcode says the call failed.
type APIError struct {
	Op      string
	Ret     int
	ErrCode int
	ErrMsg  string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("weixin %s: ret=%d errcode=%d errmsg=%s", e.Op, e.Ret, e.ErrCode, e.ErrMsg)
}

// IsStaleToken reports whether err means the bot token is no longer valid.
func IsStaleToken(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && (apiErr.Ret == StaleTokenCode || apiErr.ErrCode == StaleTokenCode)
}

type baseInfo struct {
	ChannelVersion string `json:"channel_version"`
	BotAgent       string `json:"bot_agent"`
}

func (c *Client) baseInfo() baseInfo {
	return baseInfo{ChannelVersion: c.cfg.ChannelVersion, BotAgent: c.cfg.BotAgent}
}

// clientVersion encodes "major.minor.patch" as 0x00MMNNPP, sent as a decimal string.
func clientVersion(v string) string {
	parts := strings.SplitN(v, ".", 3)
	n := 0
	for i := 0; i < 3; i++ {
		x := 0
		if i < len(parts) {
			x, _ = strconv.Atoi(parts[i])
		}
		n = n<<8 | (x & 0xff)
	}
	return strconv.Itoa(n)
}

// randomUIN is the X-WECHAT-UIN header: a random uint32, as a decimal string, base64-encoded.
func randomUIN() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return base64.StdEncoding.EncodeToString([]byte(strconv.FormatUint(uint64(binary.BigEndian.Uint32(b[:])), 10)))
}

func (c *Client) commonHeaders(h http.Header) {
	h.Set("iLink-App-Id", c.cfg.AppID)
	h.Set("iLink-App-ClientVersion", clientVersion(c.cfg.ChannelVersion))
	if c.cfg.RouteTag != "" {
		h.Set("SKRouteTag", c.cfg.RouteTag)
	}
}

// post sends a JSON POST and decodes the response into out. token may be empty (QR-code request).
func (c *Client) post(
	ctx context.Context, baseURL, path, token string, timeout time.Duration, body, out any, op string,
) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("weixin %s: 编码请求失败: %w", op, err)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, joinURL(baseURL, path), bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("weixin %s: %w", op, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("AuthorizationType", "ilink_bot_token")
	req.Header.Set("X-WECHAT-UIN", randomUIN())
	c.commonHeaders(req.Header)
	if t := strings.TrimSpace(token); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	return c.do(req, out, op)
}

// get sends a GET with only the common headers (QR-code status polling).
func (c *Client) get(ctx context.Context, rawURL string, timeout time.Duration, out any, op string) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("weixin %s: %w", op, err)
	}
	c.commonHeaders(req.Header)
	return c.do(req, out, op)
}

func (c *Client) do(req *http.Request, out any, op string) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("weixin %s: %w", op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("weixin %s: 读响应失败: %w", op, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &HTTPError{Op: op, Status: resp.StatusCode, Body: string(raw)}
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("weixin %s: 解析响应失败: %w", op, err)
	}
	return nil
}

func joinURL(base, path string) string {
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(path, "/")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// isTimeout reports whether err is our own client-side deadline (not the caller's cancellation).
func isTimeout(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	var ne interface{ Timeout() bool }
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}

// escape is url.QueryEscape, named for readability at call sites.
func escape(s string) string { return url.QueryEscape(s) }

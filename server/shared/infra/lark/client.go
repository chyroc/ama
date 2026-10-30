// Package lark is the integration layer for the Feishu open platform. It only covers the
// "self-register a Feishu app" path.
//
// # Why hand-written HTTP
//
// The official Go SDK has none of the endpoints this path uses. Device code self-registration is on the accounts domain, app inspection is on the open
// domain, and the two domains even authenticate differently, so this package writes HTTP by hand
// instead of thinly wrapping an SDK like shared/infra/ma does.
//
// # Device code self-registration: start with zero credentials
//
// The usual Feishu integration has the user create an app on the open platform, copy the AppID and
// AppSecret, and paste them back.
// The device code flow (OAuth 2.0 device flow) turns that into "click a link and authorize":
//
//	Begin  →  get a device_code and an authorization page URL
//	         (give the URL to the user, who clicks approve in Feishu)
//	Poll   →  poll at interval; when authorized, it returns client_id / client_secret directly
//
// Key point: **the registration endpoints on the accounts domain need no token at all**. That is
// why the whole path can bootstrap itself "without having any Feishu app yet". Without it you would
// be stuck in the loop of "you need an app to call the API, and you need to call the API to create
// an app".
//
// # Two domains, two kinds of auth
//
//	accounts.feishu.cn      registration (Begin / Poll)    no auth needed
//	open.feishu.cn          inspection (InspectApp etc.)   needs tenant_access_token
//
// Mainland China uses feishu and international uses lark; each has its own set of addresses, see
// Region. Users often cannot say which domain their tenant belongs to; the tenant_brand returned by
// Poll reveals the real one, and the business layer corrects itself based on it.
//
// # Usage
//
//	c, err := lark.New(lark.Config{})
//	if err != nil {
//	    return err
//	}
//	ch, err := c.Begin(ctx, lark.BeginSpec{AppName: "AMA Assistant"})
//	if err != nil {
//	    return err
//	}
//	// Give ch.VerificationURL to the user to authorize, then poll at ch.Interval:
//	ticket, err := c.Poll(ctx, ch.DeviceCode)
//
// See example/main.go for a complete runnable example.
package lark

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The four default addresses. Mainland China and international each have an accounts (registration)
// and an open (inspection) domain.
const (
	// DefaultAccountsURLFeishu is the mainland China accounts domain, where the device code
	// registration endpoints live.
	DefaultAccountsURLFeishu = "https://accounts.feishu.cn"
	// DefaultAccountsURLLark is the international accounts domain.
	DefaultAccountsURLLark = "https://accounts.larksuite.com"
	// DefaultOpenURLFeishu is the mainland China open platform domain, where the app inspection
	// endpoints live.
	DefaultOpenURLFeishu = "https://open.feishu.cn/open-apis"
	// DefaultOpenURLLark is the international open platform domain.
	DefaultOpenURLLark = "https://open.larksuite.com/open-apis"
)

// RegistrationPath is the endpoint path for device code self-registration. begin and poll share it
// and are told apart by action.
const RegistrationPath = "/oauth/v1/app/registration"

// defaultTimeout is the timeout for a single request.
//
// The device code endpoints are all short requests (begin measured at about 80ms); there is no long
// polling, so an overall timeout is safe here. The polling **interval** is
// controlled by the caller per Challenge.Interval, which is a different thing from the per-request
// timeout.
const defaultTimeout = 30 * time.Second

// Region is the Feishu accounts domain. Mainland China and international are two fully separate
// tenant systems.
type Region string

const (
	// RegionFeishu is mainland China (Feishu), and also the default when empty.
	RegionFeishu Region = "feishu"
	// RegionLark is international (Lark).
	RegionLark Region = "lark"
)

// NormalizeRegion maps an external region name to a valid value; anything unrecognized is treated
// as mainland China.
//
// The business layer reads strings from storage, and user input case is not guaranteed, so this
// entry point is needed.
func NormalizeRegion(region string) Region {
	if strings.EqualFold(strings.TrimSpace(region), string(RegionLark)) {
		return RegionLark
	}
	return RegionFeishu
}

// Other returns the other accounts domain, used to correct a wrong domain guess.
func (r Region) Other() Region {
	if r == RegionLark {
		return RegionFeishu
	}
	return RegionLark
}

// Config is the configuration for building a Client. Every field may be left empty.
type Config struct {
	// Region is the accounts domain; empty means RegionFeishu (mainland China).
	Region Region

	// AccountsURL overrides the accounts domain address; empty derives it from Region.
	//
	// This field exists mainly for tests: unit tests start a fake accounts domain with httptest and
	// route requests to it through this field, so all tests run offline without touching real
	// Feishu.
	AccountsURL string

	// OpenURL overrides the open platform address; empty derives it from Region. Also a test
	// injection point.
	OpenURL string

	// HTTPClient is a custom HTTP client. Empty uses a default client with a 30-second timeout.
	HTTPClient *http.Client
}

func (c Config) validate() error {
	switch c.Region {
	case "", RegionFeishu, RegionLark:
	default:
		return fmt.Errorf("%w: 未知区域 %q", ErrInvalidConfig, string(c.Region))
	}
	return nil
}

// Client is the HTTP client for the Feishu open platform.
//
// Safe for concurrent use: it has no mutable state and can be shared by many goroutines.
type Client struct {
	cfg      Config
	region   Region
	accounts string
	open     string
	http     *http.Client
}

// New builds a Client from the config.
func New(cfg Config) (*Client, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	region := cfg.Region
	if region == "" {
		region = RegionFeishu
	}
	accounts := cfg.AccountsURL
	if accounts == "" {
		accounts = DefaultAccountsURLFeishu
		if region == RegionLark {
			accounts = DefaultAccountsURLLark
		}
	}
	open := cfg.OpenURL
	if open == "" {
		open = DefaultOpenURLFeishu
		if region == RegionLark {
			open = DefaultOpenURLLark
		}
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	return &Client{
		cfg:      cfg,
		region:   region,
		accounts: strings.TrimSuffix(accounts, "/"),
		open:     strings.TrimSuffix(open, "/"),
		http:     httpClient,
	}, nil
}

// Region returns the accounts domain this Client is bound to.
func (c *Client) Region() Region { return c.region }

// postForm sends a form-encoded request to the accounts domain. The registration endpoints only
// accept form, not JSON.
//
// **The registration endpoints' protocol states come with 4xx**: action=poll returns HTTP 400 +
// {"error":"authorization_pending"} while "still waiting for the user to authorize", and an invalid
// device code is also 400. So success cannot be judged by status code here; the response body must
// be parsed first. The protocol state is hidden in the error field, and the caller judges it with
// PollStatus.
//
// It is only treated as a transport error when the response body cannot be parsed at all (for
// example the gateway returned an HTML page).
func (c *Client) postForm(ctx context.Context, path string, form url.Values, out any) error {
	rawURL := c.accounts + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, raw, err := c.send(req, rawURL)
	if err != nil {
		return err
	}
	if jsonErr := json.Unmarshal(raw, out); jsonErr != nil {
		// Only look at the status code if parsing fails: 4xx/5xx and not JSON means a real error,
		// not a protocol state.
		if resp.StatusCode >= 300 {
			return newAPIError(resp.StatusCode, raw)
		}
		return fmt.Errorf("解析响应失败（body=%s）: %w", truncate(string(raw), 256), jsonErr)
	}
	return nil
}

// getJSON sends a GET with a tenant token to the open platform.
func (c *Client) getJSON(ctx context.Context, path, token string, out any) error {
	rawURL := c.open + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return c.do(req, rawURL, out)
}

// postJSON sends a JSON POST with a tenant token to the open platform. An empty token means the
// endpoint needs no auth.
func (c *Client) postJSON(ctx context.Context, path, token string, body, out any) error {
	return c.sendJSON(ctx, http.MethodPost, path, token, body, out)
}

// postJSONAllowProtocolError sends a JSON POST, but **also decodes the response body on 4xx**.
//
// # Why postJSON cannot be used
//
// The user OAuth token exchange / refresh endpoint (authen/v2/oauth/token) puts the protocol state
// in **the body of an HTTP 400 response**, while do() returns APIError on 4xx without parsing the
// body at all. So the caller's res.Error is always an empty string, and "the user has not clicked
// approve yet" (authorization_pending, also a 400) is treated as a hard error: **polling gives up
// on the first try and the user can never authorize**, while the error says "request failed 400",
// which does not point at the real cause.
//
// This is the same lesson as postForm (see the comment there), only the encoding differs: that one
// is form, this one needs JSON. So both deliberately bypass do().
//
// It is only treated as a transport error when the response body cannot be parsed at all (the
// gateway returned HTML and the like).
func (c *Client) postJSONAllowProtocolError(ctx context.Context, path string, body, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("序列化请求体失败: %w", err)
	}
	rawURL := c.open + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(string(raw)))
	if err != nil {
		return fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, respBody, err := c.send(req, rawURL)
	if err != nil {
		return err
	}
	if jsonErr := json.Unmarshal(respBody, out); jsonErr != nil {
		if resp.StatusCode >= 300 {
			return newAPIError(resp.StatusCode, respBody)
		}
		return fmt.Errorf("解析响应失败（body=%s）: %w", truncate(string(respBody), 256), jsonErr)
	}
	return nil
}

// patchJSON sends a JSON PATCH with a tenant token to the open platform.
//
// It is used to update a card already sent (PATCH /im/v1/messages/{id}): streaming replies work by
// editing the same card in place, see the header of message.go.
func (c *Client) patchJSON(ctx context.Context, path, token string, body, out any) error {
	return c.sendJSON(ctx, http.MethodPatch, path, token, body, out)
}

// deleteJSON sends a DELETE with a tenant token to the open platform.
//
// It is used to remove an emoji reaction (DELETE /im/v1/messages/{id}/reactions/{rid}).
func (c *Client) deleteJSON(ctx context.Context, path, token string, out any) error {
	rawURL := c.open + path
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, rawURL, nil)
	if err != nil {
		return fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return c.do(req, rawURL, out)
}

// putJSON sends a JSON PUT with a tenant token to the open platform.
//
// It is used to append CoT events (PUT /im/v1/message_cot), see cot.go.
func (c *Client) putJSON(ctx context.Context, path, token string, body, out any) error {
	return c.sendJSON(ctx, http.MethodPut, path, token, body, out)
}

// sendJSON is the shared implementation of postJSON / patchJSON.
func (c *Client) sendJSON(ctx context.Context, method, path, token string, body, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("序列化请求体失败: %w", err)
	}
	rawURL := c.open + path
	req, err := http.NewRequestWithContext(ctx, method, rawURL, strings.NewReader(string(raw)))
	if err != nil {
		return fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return c.do(req, rawURL, out)
}

// do sends a request and decodes the response body into out. The open platform endpoints use it:
// they follow the "HTTP 200 + business code" convention, so 4xx really is an error.
//
// The registration endpoints do not go through here, since their protocol states come with 4xx; see
// postForm.
// **The two user OAuth endpoints do not go through here either**, for the same reason; see
// postJSONAllowProtocolError.
func (c *Client) do(req *http.Request, rawURL string, out any) error {
	resp, raw, err := c.send(req, rawURL)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return newAPIError(resp.StatusCode, raw)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("解析响应失败（body=%s）: %w", truncate(string(raw), 256), err)
	}
	return nil
}

// send sends a request and reads the whole response body back.
func (c *Client) send(req *http.Request, rawURL string) (*http.Response, []byte, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("请求 %s 失败: %w", rawURL, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("读取响应失败: %w", err)
	}
	return resp, raw, nil
}

// truncate cuts overly long strings so the whole response body is not dumped into the error
// message.
func truncate(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "…"
}

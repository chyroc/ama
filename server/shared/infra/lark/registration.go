// Device code self-registration protocol.
// Related API: POST /oauth/v1/app/registration (shared by begin and poll, told apart by action)
//
// This endpoint has two counter-intuitive points:
//
//  1. **It needs no token at all.** The registration endpoints on the whole accounts domain are
//     unauthenticated; this is the only entry point that can bootstrap "without having any Feishu
//     app yet".
//  2. **Failures also return HTTP 200**, with the error hidden in the error field of the response
//     body. So success cannot be judged by status code; look at error. That is why PollStatus
//     exists.

package lark

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// Fixed parameters of the registration request. archetype decides the shape of the registered app,
// auth_method decides the credential type obtained (client_secret, i.e. AppSecret), and of the two
// fields requested by request_user_info, tenant_brand is the basis for correcting between the two
// domains.
const (
	registrationArchetype  = "PersonalAgent"
	registrationAuthMethod = "client_secret"
	registrationUserInfo   = "open_id tenant_brand"
)

// PollStatus is the protocol result of one poll.
type PollStatus string

const (
	// PollOK means the authorization flow had no error. Note that **the credentials may still be on
	// their way**, so after getting it you must also check that AppID / AppSecret are non-empty,
	// and keep polling if they are empty.
	PollOK PollStatus = "ok"
	// PollPending means still waiting for the user to click approve on the Feishu side.
	PollPending PollStatus = "pending"
	// PollSlowDown means polling is too fast and the interval must grow.
	PollSlowDown PollStatus = "slow_down"
	// PollExpired means the device_code has expired. Terminal; the only option is to start over.
	PollExpired PollStatus = "expired"
	// PollDenied means the user denied authorization. Terminal.
	PollDenied PollStatus = "denied"
	// PollInvalid means the device_code is invalid. Terminal.
	//
	// Tested: Feishu returns invalid_grant (code 20079) for a forged/corrupted device_code. It must
	// be its own case: lumping it into PollUnknown would trigger the "retry on the other accounts
	// domain" correction, while the domain is not the problem at all; it would still fail after a
	// wasted round, and the real cause would be hidden.
	PollInvalid PollStatus = "invalid"
	// PollUnknown is an error code the protocol does not define. Maybe the domain was guessed wrong
	// (then TenantBrand reveals the real one), or Feishu added a new error code.
	PollUnknown PollStatus = "unknown"
)

// Terminal reports whether the status is terminal, i.e. polling further will give no result.
func (s PollStatus) Terminal() bool {
	switch s {
	case PollExpired, PollDenied, PollInvalid:
		return true
	default:
		return false
	}
}

// pollStatusFor maps the protocol's error field to a PollStatus.
func pollStatusFor(protocolError string) PollStatus {
	switch strings.TrimSpace(protocolError) {
	case "":
		return PollOK
	case "authorization_pending":
		return PollPending
	case "slow_down":
		return PollSlowDown
	case "expired_token":
		return PollExpired
	case "access_denied":
		return PollDenied
	case "invalid_grant":
		return PollInvalid
	default:
		return PollUnknown
	}
}

// BeginSpec describes one registration start. All fields are optional.
type BeginSpec struct {
	// AppID, when non-empty, means **upgrading an existing app** (adding permissions to it) instead
	// of creating a new one.
	AppID string

	// AppName is the app name shown on the authorization page.
	AppName string

	// Source identifies the initiator, used for source stats on the authorization page.
	Source string

	// TenantScopes are tenant permissions requested in addition to the base template (BaseScopes).
	TenantScopes []string

	// Callbacks are callbacks subscribed in addition to the base template (BaseCallbacks).
	Callbacks []string
}

// Challenge is the device code awaiting user authorization, plus the authorization URL.
type Challenge struct {
	// DeviceCode is the polling credential. It is a JWT, but to us it is just an opaque string.
	DeviceCode string
	// VerificationURL is the authorization URL for the user to click, with display parameters and
	// addons already filled in.
	VerificationURL string
	// ExpiresIn is how many seconds the device_code is valid; measured at 3600.
	ExpiresIn int
	// Interval is the polling interval in seconds suggested by the protocol; measured at 5.
	Interval int
}

// Ticket is the result of one poll.
type Ticket struct {
	// Status is the protocol result; look at it before the other fields.
	Status PollStatus
	// AppID is the registered app ID, like cli_xxx. May only be set when Status is PollOK.
	AppID string
	// AppSecret is the app secret. **It should only stay in memory briefly; never log it or send it
	// back to the frontend.**
	AppSecret string
	// TenantBrand is the accounts domain the tenant really belongs to (feishu / lark).
	// It is used to correct a wrong domain guess; see Region.Other.
	TenantBrand string
	// ErrorDescription is the error description from the protocol; may be empty.
	ErrorDescription string
}

// beginResponse is the response of action=begin.
//
// Both field names expires_in and expire_in are accepted: testing shows expires_in is returned, but
// the protocol docs and some implementations write expire_in. Accepting only one would read the
// validity as 0.
type beginResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	ExpireIn                int    `json:"expire_in"`
	Interval                int    `json:"interval"`
	Error                   string `json:"error"`
	ErrorDescription        string `json:"error_description"`
}

// pollResponse is the response of action=poll.
type pollResponse struct {
	ClientID         string `json:"client_id"`
	ClientSecret     string `json:"client_secret"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
	UserInfo         struct {
		TenantBrand string `json:"tenant_brand"`
	} `json:"user_info"`
}

// Begin starts a device code registration and returns the device code awaiting user authorization
// and the authorization URL.
func (c *Client) Begin(ctx context.Context, spec BeginSpec) (*Challenge, error) {
	form := url.Values{
		"action":            []string{"begin"},
		"archetype":         []string{registrationArchetype},
		"auth_method":       []string{registrationAuthMethod},
		"request_user_info": []string{registrationUserInfo},
	}
	var res beginResponse
	if err := c.postForm(ctx, RegistrationPath, form, &res); err != nil {
		return nil, err
	}
	if res.Error != "" {
		return nil, fmt.Errorf("发起注册失败（error=%s）: %w",
			firstNonEmpty(res.ErrorDescription, res.Error), ErrInvalidResponse)
	}
	deviceCode := strings.TrimSpace(res.DeviceCode)
	if deviceCode == "" {
		return nil, fmt.Errorf("%w: 发起注册的响应缺少 device_code", ErrInvalidResponse)
	}
	verificationURL, err := buildVerificationURL(res, spec)
	if err != nil {
		return nil, err
	}
	return &Challenge{
		DeviceCode:      deviceCode,
		VerificationURL: verificationURL,
		ExpiresIn:       firstPositive(res.ExpiresIn, res.ExpireIn),
		Interval:        res.Interval,
	}, nil
}

// Poll checks the authorization result once with the device_code.
//
// A returned error only means this request did not get through; protocol results like "still
// waiting for authorization" or "expired" are not errors; look at Ticket.Status.
func (c *Client) Poll(ctx context.Context, deviceCode string) (*Ticket, error) {
	deviceCode = strings.TrimSpace(deviceCode)
	if deviceCode == "" {
		return nil, fmt.Errorf("%w: device_code 不能为空", ErrInvalidArgument)
	}
	form := url.Values{
		"action":      []string{"poll"},
		"device_code": []string{deviceCode},
	}
	var res pollResponse
	if err := c.postForm(ctx, RegistrationPath, form, &res); err != nil {
		return nil, err
	}
	return &Ticket{
		Status:           pollStatusFor(res.Error),
		AppID:            strings.TrimSpace(res.ClientID),
		AppSecret:        strings.TrimSpace(res.ClientSecret),
		TenantBrand:      strings.TrimSpace(res.UserInfo.TenantBrand),
		ErrorDescription: res.ErrorDescription,
	}, nil
}

// buildVerificationURL fills in the authorization page parameters: source, app name, and the addons
// for extra permissions.
func buildVerificationURL(res beginResponse, spec BeginSpec) (string, error) {
	raw := strings.TrimSpace(res.VerificationURIComplete)
	if raw == "" {
		base := strings.TrimSpace(res.VerificationURI)
		code := strings.TrimSpace(res.UserCode)
		if base != "" && code != "" {
			raw = strings.TrimRight(base, "/") + "?user_code=" + url.QueryEscape(code)
		}
	}
	if raw == "" {
		return "", fmt.Errorf("%w: 发起注册的响应缺少授权地址", ErrInvalidResponse)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("解析授权地址失败: %w", err)
	}
	query := parsed.Query()
	setIfAbsent(query, "from", "sdk")
	setIfAbsent(query, "tp", "sdk")
	if v := strings.TrimSpace(spec.Source); v != "" {
		setIfAbsent(query, "source", v)
	}
	if v := strings.TrimSpace(spec.AppName); v != "" {
		setIfAbsent(query, "name", v)
	}
	if v := strings.TrimSpace(spec.AppID); v != "" {
		setIfAbsent(query, "clientID", v)
	}
	addons, err := encodeAddons(spec.TenantScopes, spec.Callbacks)
	if err != nil {
		return "", err
	}
	if addons != "" {
		setIfAbsent(query, "addons", addons)
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// setIfAbsent only sets the parameter when Feishu did not provide it, without overwriting an
// existing value.
func setIfAbsent(query url.Values, key, value string) {
	if query.Get(key) == "" {
		query.Set(key, value)
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func firstPositive(values ...int) int {
	for _, v := range values {
		if v > 0 {
			return v
		}
	}
	return 0
}

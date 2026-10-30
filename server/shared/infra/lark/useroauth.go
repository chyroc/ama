// Feishu **user** OAuth: device-code authorization + token exchange + refresh.
// APIs used:
//
//	POST accounts domain /oauth/v1/device_authorization      start device code (no auth)
//	POST open domain     /authen/v2/oauth/token              exchange token / refresh
//	GET  open domain     /authen/v1/user_info                look up "whose token is this"
//
// ⚠️ The two open-domain paths **do not have the `/open-apis` prefix**: the base URL already
// includes it, see UserTokenPath.
//
// # Difference from registration.go: that one gets an "app", this one gets a "person"
//
// Both use device codes, but what they produce is completely different; do not mix the types:
//
//	registration.go  self-registers a **Feishu app**, produces app_id + app_secret
//	this file        gets a **user's** access/refresh token, produces user_access_token
//
// So requests in this file **must carry client_id/client_secret** (which app it is), while the
// registration API deliberately needs no credentials (there is no app yet at that point).
//
// # ⚠️ Why device code and not "authorization code + callback"
//
// The authorization code path needs a `redirect_uri`, and **the redirect_uri must first be added
// to the redirect URL allowlist on the open platform**. The "known limits" in
// `shared/infra/lark/README.md` say clearly that this package **cannot create/modify app
// config**, so that step can only be done by each app's owner by hand. And this platform's
// Feishu apps are **self-registered by users** (one or even several per person), which would mean
// every user must first configure the open platform by hand, or the authorization page fails.
//
// Device code has no such prerequisite: it needs no redirect_uri, so no allowlist. The cost is
// one extra step for the user, "open the link and confirm", which they had to do anyway (OAuth
// needs a real person to agree).
//
// A side benefit: **lark-cli itself also uses device code** (`lark-cli auth login`), so we and
// the CLI in the container use the same protocol and the same scopes, and there is no mismatch
// like "the token we got cannot be used by the CLI".
//
// # ⚠️ Failures can also be HTTP 400, with the error code hidden in the body
//
// **Unlike** registration.go's "failures also return 200", these two APIs fail with
// **HTTP 400 + error / code in the body** (tested):
//
//	device_authorization  client_id does not exist → 400 {"error":"invalid_client","code":20048}
//	oauth/token           refresh invalid          → 400 {"error":"invalid_grant","code":20026}
//
// So the check must look at the status code **and** the body: looking only at the status code
// treats "the user has not agreed yet" (authorization_pending, also 400) as a hard error, and
// polling gives up on the first try.
// This is exactly why the PollStatus model is needed here too.
package lark

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Paths of the two APIs.
const (
	// DeviceAuthorizationPath is on the **accounts domain**, the same domain as the registration API.
	//
	// ⚠️ In testing only this path exists: `/open-apis/oauth/v1/device_authorization` and
	// `/open-apis/authen/v1/device_authorization` both return 404. Do not add the prefix based on
	// the intuition that "other Feishu APIs all have the /open-apis prefix".
	DeviceAuthorizationPath = "/oauth/v1/device_authorization"

	// UserTokenPath is on the **open domain**, shared by token exchange and refresh, told apart by
	// grant_type.
	//
	// ⚠️⚠️ **Do not add the `/open-apis` prefix.** `DefaultOpenURLFeishu` is already
	// `https://open.feishu.cn/open-apis` (see client.go), so writing
	// `/open-apis/authen/v2/oauth/token` here builds `/open-apis/open-apis/...` → **404**.
	//
	// This pitfall is costly: a 404 looks like "Feishu has no such API" or "wrong path", so you go
	// read the docs, change domains, and try all sorts of path variants. The real cause is **the
	// prefix added twice**, and the accounts domain constant (`https://accounts.feishu.cn`, no
	// prefix) is the opposite. The base shapes of the two domains differ, so DeviceAuthorizationPath
	// starting with `/oauth/...` is right, while this one must not have `/open-apis`.
	// Rule: when adding an open-domain path, follow the shape of the constants next door in
	// message.go / directory.go.
	UserTokenPath = "/authen/v2/oauth/token"
)

// ScopeOfflineAccess is required to get a refresh_token.
//
// ⚠️ **Without requesting it there is no refresh_token**, so the access_token expires after two
// hours and the user has to scan again. All of the "automatic background refresh" relies on this
// one scope.
const ScopeOfflineAccess = "offline_access"

// ScopeUserReadGroupMessage is the permission to read group message history **as the user**.
//
// ⚠️⚠️ **It and ScopeGroupMessage (`im:message.group_msg`) are two different scopes; do not mix
// them up.** The names are almost the same, but who they belong to and what they do are
// completely different:
//
//	im:message.group_msg              tenant / bot identity. Controls "push messages to me even
//	                                  without @", and the bot reading group history.
//	                                  **Sensitive permission, needs admin approval, and cannot
//	                                  be added to existing apps** (see feishu/README.md).
//	                                  ⚠️ **Only messages sent by users, not by bots.**
//	im:message.group_msg.include_bot:read
//	                                  Same as above, but **also pushes messages from other
//	                                  bots** (still not its own). Alert groups where "only bots
//	                                  talk" need it, see scopes.go.
//	im:message.group_msg:get_as_user  Read group history as the **user**. Comes with user
//	                                  authorization, no admin approval needed.
//
// The three prefixes are almost the same, and **mixing them up costs different things**: using
// get_as_user to receive pushes receives nothing, while missing include_bot gives "replies when
// people talk, nothing when bots talk". The latter is especially hard to debug, because it
// behaves normally in most groups.
//
// Tested: calling the group history API with a token that has only the last one returns
// `230001 invalid container_id` (a parameter error, meaning **the permission gate was passed**)
// rather than `230027` (missing permission).
// So the "user authorization" path gets around the tenant permission that blocks the bot. That
// is its main value.
const ScopeUserReadGroupMessage = "im:message.group_msg:get_as_user"

// ScopeUserReadP2PMessage is the permission to read direct chat history as the user.
const ScopeUserReadP2PMessage = "im:message.p2p_msg:get_as_user"

// UserAuthScopes are the permissions requested when starting user authorization.
//
// **The order is fixed** (it goes into the authorization URL; a changing order makes logs hard to
// compare). When adding a scope, think about what the user will see on the authorization page:
// that page lists each one, and the more you ask for, the lower the agree rate.
var UserAuthScopes = []string{
	ScopeOfflineAccess,
	ScopeUserReadGroupMessage,
	ScopeUserReadP2PMessage,
}

// UserChallenge is a device-code authorization waiting for the user to confirm.
type UserChallenge struct {
	// DeviceCode is the polling credential, an opaque string to us. **Do not log it**.
	DeviceCode string
	// UserCode is the short code for the user to check; it can be shown.
	UserCode string
	// VerificationURL is the URL given to the user to open.
	//
	// Prefer verification_uri_complete (it already includes user_code, so the user does not type
	// it); fall back to verification_uri only when it is missing.
	VerificationURL string
	// ExpiresIn is how many seconds device_code is valid.
	ExpiresIn int
	// Interval is the polling interval in seconds suggested by the protocol.
	Interval int
}

// UserAccessToken is the user credential from a successful authorization (or refresh).
//
// The fields match the response of the Feishu OAuth token API.
// ⚠️ **Both tokens are secrets**; do not log them or put them in HTTP responses.
type UserAccessToken struct {
	AccessToken  string
	RefreshToken string
	// ExpiresIn / RefreshTokenExpiresIn are **seconds** (relative values, not points in time).
	//
	// ⚠️ Relative values must be converted to absolute times **the moment the response arrives**;
	// every step later adds drift. The caller does the conversion (core uses the domain clock);
	// this package only passes on what Feishu gave.
	ExpiresIn             int
	RefreshTokenExpiresIn int
	// Scope is the permissions actually granted, space separated. **It may be fewer than requested**
	// (the user or admin removed some), so it must be stored: the answer to "why can't I read group
	// history" is often here.
	Scope string
	// OpenID / Name is who this token belongs to, for display and debugging.
	OpenID string
	Name   string
}

// deviceAuthResponse is the response of device_authorization.
type deviceAuthResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	// Accept both expires_in and expire_in, same as beginResponse in registration.go:
	// accepting only one would read the expiry as 0.
	ExpiresIn        int    `json:"expires_in"`
	ExpireIn         int    `json:"expire_in"`
	Interval         int    `json:"interval"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
	Code             int    `json:"code"`
}

// userTokenResponse is the response of authen/v2/oauth/token.
//
// ⚠️ On success Feishu has **no** code field (or it is 0); error/code only have values on failure.
// So judge success by a non-empty access_token, not by code == 0.
type userTokenResponse struct {
	AccessToken           string `json:"access_token"`
	RefreshToken          string `json:"refresh_token"`
	ExpiresIn             int    `json:"expires_in"`
	RefreshTokenExpiresIn int    `json:"refresh_token_expires_in"`
	Scope                 string `json:"scope"`
	TokenType             string `json:"token_type"`
	Error                 string `json:"error"`
	ErrorDescription      string `json:"error_description"`
	Code                  int    `json:"code"`
}

// BeginUserAuth starts a user device-code authorization.
//
// When scopes is empty, UserAuthScopes is used.
//
// ⚠️⚠️ **client_secret must be included; this step is not "client_id only".**
//
// Tested: without the secret, Feishu returns:
//
//	{"error":"invalid_client","error_description":"The auth method is not supported.","code":20140}
//
// That message is very misleading: it sounds like "**this app** does not support the device code
// authorization method", so you naturally go check the app type, archetype, and open platform
// switches. In fact lark-cli's own app_id, **known to work with device code**, returns the same
// error, and adding client_secret for the same app makes it succeed at once.
// So the rule is: **on 20140, first suspect a missing parameter, not the app's capability.**
func (c *Client) BeginUserAuth(
	ctx context.Context, appID, appSecret string, scopes ...string,
) (*UserChallenge, error) {
	appID = strings.TrimSpace(appID)
	if appID == "" {
		return nil, fmt.Errorf("%w: appID 不能为空", ErrInvalidArgument)
	}
	appSecret = strings.TrimSpace(appSecret)
	if appSecret == "" {
		return nil, fmt.Errorf("%w: appSecret 不能为空（这一步与换 token 那步都要用）", ErrInvalidArgument)
	}
	if len(scopes) == 0 {
		scopes = UserAuthScopes
	}
	form := url.Values{
		"client_id": []string{appID},
		// ⚠️ See above: without it you get 20140, and that error points to "app not supported".
		"client_secret": []string{appSecret},
		// **Space separated**, and let url.Values do the encoding (it encodes as +, which Feishu accepts).
		"scope": []string{strings.Join(uniqueNonEmpty(scopes), " ")},
	}

	var res deviceAuthResponse
	// postForm also decodes the body for 4xx and gives it to us: the errors on this path are exactly in the body.
	err := c.postForm(ctx, DeviceAuthorizationPath, form, &res)
	if res.Error != "" {
		// Protocol errors take precedence over transport errors: the description in the body is more
		// specific (e.g. 20048 "app does not exist", which means that app_id in our database was already
		// deleted on the Feishu side).
		return nil, fmt.Errorf("%w: 发起用户授权失败（error=%s code=%d）: %s",
			ErrInvalidResponse, res.Error, res.Code,
			firstNonEmpty(res.ErrorDescription, res.Error))
	}
	if err != nil {
		return nil, err
	}
	deviceCode := strings.TrimSpace(res.DeviceCode)
	if deviceCode == "" {
		return nil, fmt.Errorf("%w: 响应缺少 device_code", ErrInvalidResponse)
	}
	// verification_uri_complete already includes user_code, so prefer it: it saves the user typing
	// the short code, and typing it by hand is where this flow fails most often.
	verifyURL := firstNonEmpty(
		strings.TrimSpace(res.VerificationURIComplete),
		strings.TrimSpace(res.VerificationURI))
	if verifyURL == "" {
		return nil, fmt.Errorf("%w: 响应缺少授权地址", ErrInvalidResponse)
	}
	return &UserChallenge{
		DeviceCode:      deviceCode,
		UserCode:        strings.TrimSpace(res.UserCode),
		VerificationURL: verifyURL,
		ExpiresIn:       firstPositive(res.ExpiresIn, res.ExpireIn),
		Interval:        res.Interval,
	}, nil
}

// PollUserAuth checks the authorization result once with device_code.
//
// **Returning (nil, PollPending, nil) is normal**: the user has not agreed yet. Same convention
// as Poll in registration.go: protocol states are not errors, and mixing them into errors would
// leave the caller unable to tell whether to keep polling or give up.
func (c *Client) PollUserAuth(
	ctx context.Context, appID, appSecret, deviceCode string,
) (*UserAccessToken, PollStatus, error) {
	appID = strings.TrimSpace(appID)
	deviceCode = strings.TrimSpace(deviceCode)
	if appID == "" || deviceCode == "" {
		return nil, PollUnknown, fmt.Errorf("%w: appID 与 device_code 都不能为空", ErrInvalidArgument)
	}
	body := map[string]string{
		"grant_type":    "urn:ietf:params:oauth:grant-type:device_code",
		"client_id":     appID,
		"client_secret": strings.TrimSpace(appSecret),
		"device_code":   deviceCode,
	}
	tok, status, err := c.userTokenRequest(ctx, body)
	if err != nil {
		return nil, status, err
	}
	return tok, status, nil
}

// RefreshUserToken exchanges a refresh_token for a new pair of tokens.
//
// ⚠️ **The returned refresh_token may be new**, and must be stored too (Feishu rotates it).
// Storing only the access_token means the next refresh uses the old refresh token, which may
// already be invalid. The symptom is "after refreshing for a while, everything suddenly needs
// authorizing again".
//
// The returned PollStatus lets the caller tell two kinds of failure apart; this split **decides
// whether the card shows a warning or an error** (credential validity needs an "unknown" state):
//
//	PollInvalid   invalid_grant (20026 etc.) → authorization is really gone, the user must scan again → error
//	PollUnknown   network down, Feishu 5xx   → just failed this time, may work next time → warning
func (c *Client) RefreshUserToken(
	ctx context.Context, appID, appSecret, refreshToken string,
) (*UserAccessToken, PollStatus, error) {
	appID = strings.TrimSpace(appID)
	refreshToken = strings.TrimSpace(refreshToken)
	if appID == "" || refreshToken == "" {
		return nil, PollUnknown, fmt.Errorf("%w: appID 与 refresh_token 都不能为空", ErrInvalidArgument)
	}
	body := map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     appID,
		"client_secret": strings.TrimSpace(appSecret),
		"refresh_token": refreshToken,
	}
	return c.userTokenRequest(ctx, body)
}

// userTokenRequest is the shared implementation of token exchange and refresh.
//
// ⚠️ **Errors are judged from both the body and transport errors, with the body first**: these
// two APIs fail with HTTP 400, and 400 covers both "still waiting for the user to agree" (keep
// polling) and "refresh invalid" (give up). Judging only by status code treats the former as a
// hard error, so polling gives up after one try and the user can never authorize.
func (c *Client) userTokenRequest(
	ctx context.Context, body map[string]string,
) (*UserAccessToken, PollStatus, error) {
	var res userTokenResponse
	// ⚠️ **Do not use postJSON**: it goes through do(), and do() returns APIError on 4xx directly
	// **without parsing the response body** (which is right for the open platform's other APIs:
	// those follow "200 + business code").
	// But this API's protocol states come with 400: authorization_pending is 400, and invalid_grant
	// is also 400. With postJSON, res.Error would always be empty, so "the user has not agreed yet"
	// would be treated as a hard error: **polling gives up on the first try, and the user can never
	// authorize**. So this uses postJSONAllowProtocolError, the same approach as postForm.
	err := c.postJSONAllowProtocolError(ctx, UserTokenPath, body, &res)

	if res.Error != "" {
		status := userPollStatusFor(res.Error)
		if status.Terminal() {
			// Only final states are real errors; include both error and code, since debugging relies on
			// them to tell "user denied", "code expired" and "refresh invalid" apart. **No tokens included**.
			return nil, status, fmt.Errorf("%w: 用户授权失败（error=%s code=%d）: %s",
				ErrInvalidResponse, res.Error, res.Code,
				firstNonEmpty(res.ErrorDescription, res.Error))
		}
		// pending / slow_down: not an error; let the caller keep polling.
		return nil, status, nil
	}
	if err != nil {
		// The transport layer failed (network, timeout, 5xx). **Mark it PollUnknown, not a final
		// state**: this is "failed this time", not "authorization is gone". The caller then shows a
		// warning and tries again later, instead of deleting a good credential as invalid.
		return nil, PollUnknown, err
	}
	if strings.TrimSpace(res.AccessToken) == "" {
		// Neither error nor token. Treat it as still being issued (same as the PollOK comment in
		// registration.go: the credential may still be on its way), and let the caller poll again
		// instead of failing.
		return nil, PollPending, nil
	}
	return &UserAccessToken{
		AccessToken:           strings.TrimSpace(res.AccessToken),
		RefreshToken:          strings.TrimSpace(res.RefreshToken),
		ExpiresIn:             res.ExpiresIn,
		RefreshTokenExpiresIn: res.RefreshTokenExpiresIn,
		Scope:                 strings.TrimSpace(res.Scope),
	}, PollOK, nil
}

// userPollStatusFor maps the OAuth error string to a PollStatus.
//
// Kept separate from pollStatusFor in registration.go instead of reused: **the same error string
// can mean different things in the two protocols**, and this path has one more,
// invalid_client (the app was deleted). Merging them means that when one protocol adds a new
// error code someday, the other one's behavior quietly changes.
func userPollStatusFor(errCode string) PollStatus {
	switch strings.TrimSpace(errCode) {
	case "":
		return PollOK
	case "authorization_pending":
		return PollPending
	case "slow_down":
		return PollSlowDown
	case "expired_token", "expired_device_code":
		return PollExpired
	case "access_denied", "authorization_declined":
		return PollDenied
	case "invalid_grant", "invalid_request", "invalid_client", "invalid_device_code":
		// ⚠️ invalid_client is a final state: the app was deleted on the Feishu side, and polling ten
		// thousand more times will not help. Treating it as unknown would make the upper layer keep
		// retrying something that will never get better.
		return PollInvalid
	default:
		return PollUnknown
	}
}

// UserInfoPath is "who this user token belongs to". **Open domain, no /open-apis prefix**
// (same reason as UserTokenPath).
const UserInfoPath = "/authen/v1/user_info"

// UserInfo is display info of the authorized user. **None of it is secret**; it can go into HTTP responses.
type UserInfo struct {
	Name   string
	OpenID string
}

// FetchUserInfo uses a user access token to look up "whose token this is".
//
// # Why a separate call is needed
//
// ⚠️ **The token exchange response does not have these two fields** (tested:
// `authen/v2/oauth/token` only returns access/refresh/expires/scope). So without this call,
// user_name / open_id in the database are empty: the "authorized user" line on the card stays
// blank, and ops cannot match this credential to the person on the Feishu side.
//
// **Failure is not fatal**: the authorization itself already succeeded; missing the name only
// loses some display info. So the caller should ignore the error here (just log it) and not let
// it mark the whole authorization as failed: that would make the user scan for nothing.
func (c *Client) FetchUserInfo(ctx context.Context, userAccessToken string) (*UserInfo, error) {
	token := strings.TrimSpace(userAccessToken)
	if token == "" {
		return nil, fmt.Errorf("%w: user access token 不能为空", ErrInvalidArgument)
	}
	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Name   string `json:"name"`
			OpenID string `json:"open_id"`
		} `json:"data"`
	}
	// ⚠️ This API uses getJSON (the normal "200 + business code" convention), **not** the special
	// case of the two OAuth APIs where "protocol states are hidden in a 400 body".
	if err := c.getJSON(ctx, UserInfoPath, token, &res); err != nil {
		return nil, err
	}
	if res.Code != 0 {
		return nil, fmt.Errorf("%w: 查用户信息失败（code=%d）: %s",
			ErrInvalidResponse, res.Code, res.Msg)
	}
	return &UserInfo{
		Name:   strings.TrimSpace(res.Data.Name),
		OpenID: strings.TrimSpace(res.Data.OpenID),
	}, nil
}

// ExpiryFrom converts the relative seconds from Feishu into an absolute time.
//
// ⚠️ **It must be called right after the response arrives**: expires_in is relative to "just
// now", and computing it later gives drift. It is a function so the two call sites (first
// authorization, refresh) use the same conversion, instead of each writing
// time.Now().Add(...).
//
// now is passed in explicitly instead of taking time.Now() inside: that way tests can pin the
// result. Otherwise the assertion could only be "roughly this number", and such an assertion
// would not fail even if the code were wrong.
func ExpiryFrom(now time.Time, seconds int) time.Time {
	return now.Add(time.Duration(seconds) * time.Second)
}

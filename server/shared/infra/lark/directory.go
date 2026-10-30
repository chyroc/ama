// App inspection: read the real config of a registered app on the Feishu side.
// Related APIs: /auth/v3/tenant_access_token/internal, /application/v6/*, /bot/v3/info/
//
// "Installed" does not mean "usable". Getting credentials from registration is only the first step;
// a list of conditions must also hold: are permissions approved, are events subscribed, is the bot
// capability on, is a version published. This file reads those facts back, and the business layer
// derives a diagnosable readiness from them.
//
// # Inspection itself needs permission
//
// Reading app details needs application:application:self_manage. Without it, only the permission
// list can be read (which shows which permissions are missing), not the versions or callback
// config. So AppConfiguration has an Inspectable field marking "how much we really saw this time",
// and "not seen" must not be taken as "not configured".

package lark

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// The inspection endpoints return these two business codes when permissions are not approved;
// neither counts as a failure.
const (
	// codeScopeApplyPending means there is already a pending request.
	codeScopeApplyPending = "212004"
	// codeScopeApplyNothing means there are no permissions to request.
	codeScopeApplyNothing = "212002"
)

// AppConfiguration is the real config of an app on the Feishu side.
type AppConfiguration struct {
	// Name is the app display name; only set when Inspectable is true.
	Name string
	// AvatarURL is the app avatar; only set when Inspectable is true.
	AvatarURL string
	// GrantedScopes are the granted permissions.
	GrantedScopes []string
	// UnapprovedScopes are permissions requested but not approved yet.
	//
	// This differs from "never requested": these were already submitted by the user and can only
	// wait for an admin to approve, and only the admin can be chased; only the unrequested ones
	// need the user to change the app config.
	UnapprovedScopes []string
	// SubscribedEvents are the events subscribed by the published version; empty when not
	// published.
	SubscribedEvents []string
	// SubscribedCallbacks are the subscribed callbacks.
	SubscribedCallbacks []string
	// CallbackType is how callbacks are delivered; it should be websocket in long connection mode.
	CallbackType string
	// Inspectable marks whether app details were read (i.e. whether the self_manage permission is
	// present).
	//
	// When false, SubscribedEvents / SubscribedCallbacks / Published cannot be trusted, and must
	// not be used to decide "the user did not configure it".
	Inspectable bool
	// BotEnabled marks whether the bot capability is enabled.
	BotEnabled bool
	// Published marks whether there is currently a published live version.
	Published bool
}

// BotInfo is the bot's basic info. Being able to read it means the credentials work and the bot is
// enabled.
type BotInfo struct {
	OpenID    string
	AppName   string
	AvatarURL string
}

// TenantAccessToken exchanges AppID + AppSecret for a tenant_access_token.
//
// Inspection endpoints all need it. The token expires (about 2 hours), and this package does not
// cache it: inspection is a low-frequency operation, and the business layer already caches the
// whole inspection result per app.
func (c *Client) TenantAccessToken(ctx context.Context, appID, appSecret string) (string, error) {
	appID = strings.TrimSpace(appID)
	appSecret = strings.TrimSpace(appSecret)
	if appID == "" {
		return "", fmt.Errorf("%w: appID 不能为空", ErrInvalidArgument)
	}
	if appSecret == "" {
		return "", fmt.Errorf("%w: appSecret 不能为空", ErrInvalidArgument)
	}
	var res struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
	}
	body := map[string]string{"app_id": appID, "app_secret": appSecret}
	if err := c.postJSON(ctx, "/auth/v3/tenant_access_token/internal", "", body, &res); err != nil {
		return "", err
	}
	token := strings.TrimSpace(res.TenantAccessToken)
	if token == "" {
		return "", fmt.Errorf("换取 tenant token 失败（code=%d msg=%s）: %w",
			res.Code, res.Msg, ErrInvalidResponse)
	}
	return token, nil
}

// BotInfo reads the bot info. It also serves as the "do the credentials still work" liveness probe.
func (c *Client) BotInfo(ctx context.Context, token string) (*BotInfo, error) {
	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Bot  struct {
			OpenID    string `json:"open_id"`
			AppName   string `json:"app_name"`
			AvatarURL string `json:"avatar_url"`
		} `json:"bot"`
	}
	if err := c.getJSON(ctx, "/bot/v3/info/", token, &res); err != nil {
		return nil, err
	}
	if res.Code != 0 {
		return nil, fmt.Errorf("读机器人信息失败（code=%d msg=%s）: %w",
			res.Code, res.Msg, ErrInvalidResponse)
	}
	return &BotInfo{
		OpenID:    strings.TrimSpace(res.Bot.OpenID),
		AppName:   strings.TrimSpace(res.Bot.AppName),
		AvatarURL: strings.TrimSpace(res.Bot.AvatarURL),
	}, nil
}

// InspectApp reads the app's config on the Feishu side.
//
// It has three parts; the last two may be unreadable because of permissions or no published
// version, in which case it returns what it has read instead of an error:
//
//  1. /application/v6/scopes            permissions (granted / pending)
//  2. /application/v6/applications/me   app details (needs self_manage)
//  3. .../app_versions/{id}             events subscribed by the published version
func (c *Client) InspectApp(ctx context.Context, token string) (*AppConfiguration, error) {
	cfg := &AppConfiguration{
		GrantedScopes:       []string{},
		UnapprovedScopes:    []string{},
		SubscribedEvents:    []string{},
		SubscribedCallbacks: []string{},
	}

	// Being able to read the bot info means the bot capability is enabled; failing to read it is
	// not an error, it is just not enabled.
	if bot, err := c.BotInfo(ctx, token); err == nil && bot != nil {
		cfg.BotEnabled = true
	}

	var scopes struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Scopes []struct {
				ScopeName   string `json:"scope_name"`
				GrantStatus int    `json:"grant_status"`
			} `json:"scopes"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, "/application/v6/scopes", token, &scopes); err != nil {
		return nil, err
	}
	for _, item := range scopes.Data.Scopes {
		name := strings.TrimSpace(item.ScopeName)
		if name == "" {
			continue
		}
		// Only grant_status 1 means granted; everything else counts as pending approval.
		if item.GrantStatus == 1 {
			cfg.GrantedScopes = append(cfg.GrantedScopes, name)
		} else {
			cfg.UnapprovedScopes = append(cfg.UnapprovedScopes, name)
		}
	}

	// Without self_manage the app details cannot be read, so stop here. The permission list already
	// read is still useful.
	if !ContainsValue(cfg.GrantedScopes, ScopeSelfManage) {
		return cfg, nil
	}

	query := url.Values{"lang": []string{"zh_cn"}}
	var app struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			App struct {
				AppName         string `json:"app_name"`
				AvatarURL       string `json:"avatar_url"`
				OnlineVersionID string `json:"online_version_id"`
				CallbackInfo    struct {
					CallbackType        string   `json:"callback_type"`
					SubscribedCallbacks []string `json:"subscribed_callbacks"`
				} `json:"callback_info"`
			} `json:"app"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, "/application/v6/applications/me?"+query.Encode(), token, &app); err != nil {
		return nil, err
	}
	cfg.Inspectable = true
	cfg.Name = strings.TrimSpace(app.Data.App.AppName)
	cfg.AvatarURL = strings.TrimSpace(app.Data.App.AvatarURL)
	cfg.CallbackType = strings.TrimSpace(app.Data.App.CallbackInfo.CallbackType)
	cfg.SubscribedCallbacks = append(cfg.SubscribedCallbacks, app.Data.App.CallbackInfo.SubscribedCallbacks...)

	versionID := strings.TrimSpace(app.Data.App.OnlineVersionID)
	cfg.Published = versionID != ""
	if versionID == "" {
		return cfg, nil
	}

	var version struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			AppVersion struct {
				Events []string `json:"events"`
			} `json:"app_version"`
		} `json:"data"`
	}
	path := "/application/v6/applications/me/app_versions/" + url.PathEscape(versionID) + "?" + query.Encode()
	if err := c.getJSON(ctx, path, token, &version); err != nil {
		return nil, err
	}
	cfg.SubscribedEvents = append(cfg.SubscribedEvents, version.Data.AppVersion.Events...)
	return cfg, nil
}

// RequestAdminAuthorization submits a permission approval request to the tenant admin.
//
// "There is already a pending request" and "there are no permissions to request" both count as
// success: the user clicked the button, and the outcome is "the request is with the admin".
// Returning an error would make people think it was not submitted.
func (c *Client) RequestAdminAuthorization(ctx context.Context, token string) error {
	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := c.postJSON(ctx, "/application/v6/scopes/apply", token, map[string]any{}, &res); err != nil {
		if apiErr, ok := AsAPIError(err); ok && isBenignScopeApplyCode(apiErr.Code) {
			return nil
		}
		return err
	}
	if res.Code != 0 && !isBenignScopeApplyCode(fmt.Sprint(res.Code)) {
		return fmt.Errorf("申请管理员授权失败（code=%d msg=%s）: %w",
			res.Code, res.Msg, ErrInvalidResponse)
	}
	return nil
}

func isBenignScopeApplyCode(code string) bool {
	switch strings.TrimSpace(code) {
	case codeScopeApplyPending, codeScopeApplyNothing:
		return true
	default:
		return false
	}
}

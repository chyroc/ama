package apimodel

// WeixinAccount is a WeChat bot connected to a workspace (server/weixin), as the page sees it.
// The bot token and the long-poll cursor never leave the server.
type WeixinAccount struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspaceId"`
	// BotID is the iLink bot id (ilink_bot_id).
	BotID string `json:"botId"`
	// OwnerUserID is the WeChat user who scanned the login code; the bot talks with this user.
	OwnerUserID string `json:"ownerUserId"`
	// AgentID is the bound agent; "" means none yet.
	AgentID string `json:"agentId"`
	// Status is WeixinAccountActive or WeixinAccountExpired.
	Status WeixinAccountStatus `json:"status"`
	// LastError is the last problem worth showing; "" when fine.
	LastError string `json:"lastError"`
	// LastInboundAt is when the last message came in; "" when none yet.
	LastInboundAt string `json:"lastInboundAt"`
	CreatedBy     string `json:"createdBy"`
	CreatedAt     string `json:"createdAt"`
	UpdatedAt     string `json:"updatedAt"`
}

// WeixinAccountStatus is the state of a connected WeChat bot.
type WeixinAccountStatus string

const (
	// WeixinAccountActive receives and answers messages.
	WeixinAccountActive WeixinAccountStatus = "active"
	// WeixinAccountExpired: WeChat rejected the token (the bot was unbound or logged in elsewhere).
	// Nothing is received until someone scans the login code again.
	WeixinAccountExpired WeixinAccountStatus = "expired"
)

// WeixinLogin is a QR-code login in progress.
type WeixinLogin struct {
	ID     string            `json:"id"`
	Status WeixinLoginStatus `json:"status"`
	// QRCodeDataURL is the QR code as a PNG data URL, ready for an <img>.
	QRCodeDataURL string `json:"qrcodeDataUrl"`
	// QRCodeContent is the text inside the QR code (a URL), for "open on this phone" fallbacks.
	QRCodeContent string `json:"qrcodeContent"`
	// Message is a human sentence about the current state; may be "".
	Message string `json:"message"`
	// AccountID is set when Status is confirmed or already_connected (when known).
	AccountID string `json:"accountId"`
	ExpiresAt string `json:"expiresAt"`
}

// WeixinLoginStatus is our own login state (the server's spellings are mapped in server/weixin).
type WeixinLoginStatus string

const (
	WeixinLoginWaiting        WeixinLoginStatus = "waiting"
	WeixinLoginScanned        WeixinLoginStatus = "scanned"
	WeixinLoginNeedVerifyCode WeixinLoginStatus = "need_verify_code"
	WeixinLoginConfirmed      WeixinLoginStatus = "confirmed"
	// WeixinLoginAlreadyConnected: this bot is already connected; no new token was issued.
	WeixinLoginAlreadyConnected WeixinLoginStatus = "already_connected"
	WeixinLoginExpired          WeixinLoginStatus = "expired"
	WeixinLoginFailed           WeixinLoginStatus = "failed"
)

// Done reports whether the login reached a final state (polling can stop).
func (s WeixinLoginStatus) Done() bool {
	switch s {
	case WeixinLoginConfirmed, WeixinLoginAlreadyConnected, WeixinLoginExpired, WeixinLoginFailed:
		return true
	}
	return false
}

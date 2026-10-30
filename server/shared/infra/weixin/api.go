package weixin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"
)

// GetBotQRCode starts a QR-code login. localTokens are bot tokens this deployment already holds (the
// reference client sends up to ten), so the server can recognize a bot that is already connected.
func (c *Client) GetBotQRCode(ctx context.Context, botType string, localTokens []string) (QRCode, error) {
	if botType == "" {
		botType = DefaultBotType
	}
	if localTokens == nil {
		localTokens = []string{}
	}
	var out QRCode
	err := c.post(ctx, c.cfg.LoginBaseURL, "ilink/bot/get_bot_qrcode?bot_type="+escape(botType), "",
		apiTimeout, map[string]any{"local_token_list": localTokens}, &out, "get_bot_qrcode")
	return out, err
}

// GetQRCodeStatus polls a login once. The server holds the request until the status changes or its
// long-poll times out; our own deadline (timeout) is reported as status wait, like the reference client.
// baseURL is DefaultBaseURL, or https://<redirect_host> after a scaned_but_redirect.
func (c *Client) GetQRCodeStatus(
	ctx context.Context, baseURL, qrcode, verifyCode string, timeout time.Duration,
) (QRStatus, error) {
	if baseURL == "" {
		baseURL = c.cfg.LoginBaseURL
	}
	u := joinURL(baseURL, "ilink/bot/get_qrcode_status") + "?qrcode=" + escape(qrcode)
	if v := strings.TrimSpace(verifyCode); v != "" {
		u += "&verify_code=" + escape(v)
	}
	var out QRStatus
	if err := c.get(ctx, u, timeout, &out, "get_qrcode_status"); err != nil {
		if isTimeout(ctx, err) {
			return QRStatus{Status: QRWait}, nil
		}
		return QRStatus{}, err
	}
	return out, nil
}

// GetUpdates long-polls for new messages after cursor ("" for the first call). Our own deadline is not
// an error: it returns an empty response that keeps the old cursor, and the caller simply polls again.
// A non-zero ret / errcode is returned as *APIError (see IsStaleToken).
func (c *Client) GetUpdates(
	ctx context.Context, baseURL, token, cursor string, timeout time.Duration,
) (GetUpdatesResponse, error) {
	if timeout <= 0 {
		timeout = DefaultLongPollTimeout
	}
	var out GetUpdatesResponse
	err := c.post(ctx, baseURL, "ilink/bot/getupdates", token, timeout,
		map[string]any{"get_updates_buf": cursor, "base_info": c.baseInfo()}, &out, "getupdates")
	if err != nil {
		if isTimeout(ctx, err) {
			return GetUpdatesResponse{Cursor: cursor}, nil
		}
		return GetUpdatesResponse{}, err
	}
	if out.Ret != 0 || out.ErrCode != 0 {
		return out, &APIError{Op: "getupdates", Ret: out.Ret, ErrCode: out.ErrCode, ErrMsg: out.ErrMsg}
	}
	return out, nil
}

// SendText sends a finished text message to a user. contextToken must be the context_token of the
// conversation's latest inbound message; the reference client still sends without it, with a warning.
// Returns the client id used (each call generates a new one).
func (c *Client) SendText(ctx context.Context, baseURL, token, toUserID, contextToken, text string) (string, error) {
	clientID := newClientID()
	msg := Message{
		ToUserID:     toUserID,
		ClientID:     clientID,
		MessageType:  MessageTypeBot,
		MessageState: MessageStateFinish,
		ContextToken: contextToken,
		Items:        []Item{{Type: ItemText, Text: &TextItem{Text: text}}},
	}
	var out struct {
		Ret    int    `json:"ret"`
		ErrMsg string `json:"errmsg"`
	}
	if err := c.post(ctx, baseURL, "ilink/bot/sendmessage", token, apiTimeout,
		map[string]any{"msg": msg, "base_info": c.baseInfo()}, &out, "sendmessage"); err != nil {
		return clientID, err
	}
	if out.Ret != 0 {
		return clientID, &APIError{Op: "sendmessage", Ret: out.Ret, ErrMsg: out.ErrMsg}
	}
	return clientID, nil
}

// GetTypingTicket fetches the ticket SendTyping needs for one user.
func (c *Client) GetTypingTicket(ctx context.Context, baseURL, token, userID, contextToken string) (string, error) {
	var out struct {
		Ret          int    `json:"ret"`
		ErrMsg       string `json:"errmsg"`
		TypingTicket string `json:"typing_ticket"`
	}
	body := map[string]any{"ilink_user_id": userID, "base_info": c.baseInfo()}
	if contextToken != "" {
		body["context_token"] = contextToken
	}
	if err := c.post(ctx, baseURL, "ilink/bot/getconfig", token, lightTimeout, body, &out, "getconfig"); err != nil {
		return "", err
	}
	if out.Ret != 0 {
		return "", &APIError{Op: "getconfig", Ret: out.Ret, ErrMsg: out.ErrMsg}
	}
	return out.TypingTicket, nil
}

// SendTyping shows (TypingStart) or clears (TypingCancel) the "typing" indicator for a user.
// The reference client ignores the response body; so do we.
func (c *Client) SendTyping(ctx context.Context, baseURL, token, userID, ticket string, status int) error {
	return c.post(ctx, baseURL, "ilink/bot/sendtyping", token, lightTimeout, map[string]any{
		"ilink_user_id": userID, "typing_ticket": ticket, "status": status, "base_info": c.baseInfo(),
	}, nil, "sendtyping")
}

// NotifyStart tells the server a client started receiving for this bot. Failures are only worth a log line.
func (c *Client) NotifyStart(ctx context.Context, baseURL, token string) error {
	return c.post(ctx, baseURL, "ilink/bot/msg/notifystart", token, lightTimeout,
		map[string]any{"base_info": c.baseInfo()}, nil, "notifystart")
}

// NotifyStop tells the server the client stopped. Failures are only worth a log line.
func (c *Client) NotifyStop(ctx context.Context, baseURL, token string) error {
	return c.post(ctx, baseURL, "ilink/bot/msg/notifystop", token, lightTimeout,
		map[string]any{"base_info": c.baseInfo()}, nil, "notifystop")
}

func newClientID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "ama-" + hex.EncodeToString(b[:])
}

package weixin

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// ID is a uint64 identifier (message_id, msg_id, svr_id). The server sends it as a JSON number that may
// exceed 2^53, so it is kept as its decimal text instead of a float or int.
type ID string

// UnmarshalJSON accepts a number or a string.
func (id *ID) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		*id = ""
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*id = ID(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("weixin: id 不是数字也不是字符串: %s", b)
	}
	*id = ID(n.String())
	return nil
}

// MarshalJSON writes the id back as a number, as the server sends it.
func (id ID) MarshalJSON() ([]byte, error) {
	if id == "" {
		return []byte("null"), nil
	}
	return []byte(id), nil
}

// Message types (WeixinMessage.message_type).
const (
	MessageTypeUser = 1
	MessageTypeBot  = 2
)

// Message states (WeixinMessage.message_state).
const (
	MessageStateNew        = 0
	MessageStateGenerating = 1
	MessageStateFinish     = 2
)

// Item types (MessageItem.type).
const (
	ItemText           = 1
	ItemImage          = 2
	ItemVoice          = 3
	ItemFile           = 4
	ItemVideo          = 5
	ItemToolCallStart  = 11
	ItemToolCallResult = 12
)

// Typing status values for SendTyping.
const (
	TypingStart  = 1
	TypingCancel = 2
)

// Message is one WeChat message (proto WeixinMessage). Fields we do not use are left out.
type Message struct {
	Seq          int64  `json:"seq,omitempty"`
	MessageID    ID     `json:"message_id,omitempty"`
	FromUserID   string `json:"from_user_id"`
	ToUserID     string `json:"to_user_id"`
	ClientID     string `json:"client_id,omitempty"`
	CreateTimeMs int64  `json:"create_time_ms,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
	GroupID      string `json:"group_id,omitempty"`
	MessageType  int    `json:"message_type,omitempty"`
	MessageState int    `json:"message_state,omitempty"`
	Items        []Item `json:"item_list,omitempty"`
	// ContextToken is issued per inbound message and must be echoed in every reply to that conversation.
	ContextToken string `json:"context_token,omitempty"`
	RunID        string `json:"run_id,omitempty"`
}

// Item is one content item of a message (proto MessageItem).
type Item struct {
	Type   int         `json:"type"`
	MsgID  ID          `json:"msg_id,omitempty"`
	RefMsg *RefMessage `json:"ref_msg,omitempty"`
	Text   *TextItem   `json:"text_item,omitempty"`
	Image  *MediaItem  `json:"image_item,omitempty"`
	Voice  *VoiceItem  `json:"voice_item,omitempty"`
	File   *FileItem   `json:"file_item,omitempty"`
	Video  *MediaItem  `json:"video_item,omitempty"`
}

// TextItem is a text item.
type TextItem struct {
	Text string `json:"text"`
}

// MediaItem covers image and video items; we only look at whether they exist for now.
type MediaItem struct {
	URL string `json:"url,omitempty"`
}

// VoiceItem is a voice item. Text is WeChat's own transcript, when it has one.
type VoiceItem struct {
	Text     string `json:"text,omitempty"`
	Playtime int64  `json:"playtime,omitempty"`
}

// FileItem is a file item.
type FileItem struct {
	FileName string `json:"file_name,omitempty"`
	Len      string `json:"len,omitempty"`
}

// RefMessage is a quoted message.
type RefMessage struct {
	Item  *Item  `json:"message_item,omitempty"`
	Title string `json:"title,omitempty"`
	SvrID ID     `json:"svr_id,omitempty"`
}

// GetUpdatesResponse is the response of getupdates.
type GetUpdatesResponse struct {
	Ret     int       `json:"ret"`
	ErrCode int       `json:"errcode"`
	ErrMsg  string    `json:"errmsg"`
	Msgs    []Message `json:"msgs"`
	// Cursor (get_updates_buf) must be sent with the next request. Empty means "keep the old one".
	Cursor               string `json:"get_updates_buf"`
	LongPollingTimeoutMs int64  `json:"longpolling_timeout_ms"`
}

// QRCode is a freshly issued login QR code.
type QRCode struct {
	// Code is the value used to poll the status.
	Code string `json:"qrcode"`
	// Content is what must be rendered as a QR code for the phone to scan (a URL).
	Content string `json:"qrcode_img_content"`
}

// Login statuses returned by GetQRCodeStatus.
const (
	QRWait              = "wait"
	QRScanned           = "scaned" // sic: the server's spelling
	QRConfirmed         = "confirmed"
	QRExpired           = "expired"
	QRNeedVerifyCode    = "need_verifycode"
	QRVerifyCodeBlocked = "verify_code_blocked"
	QRScannedRedirect   = "scaned_but_redirect"
	QRAlreadyBound      = "binded_redirect"
)

// QRStatus is the response of get_qrcode_status.
type QRStatus struct {
	Status string `json:"status"`
	// BotToken, BotID, BaseURL and UserID are set when Status is confirmed.
	BotToken string `json:"bot_token,omitempty"`
	BotID    string `json:"ilink_bot_id,omitempty"`
	BaseURL  string `json:"baseurl,omitempty"`
	// UserID is the WeChat user who scanned the code (the bot's owner).
	UserID string `json:"ilink_user_id,omitempty"`
	// RedirectHost is set with scaned_but_redirect: keep polling on https://<RedirectHost>.
	RedirectHost string `json:"redirect_host,omitempty"`
}

// Package weixin is the WeChat channel: connect a WeChat bot to a workspace by scanning a QR code, bind
// it to an agent, and let the agent answer the messages the bot receives.
//
// The wire protocol is shared/infra/weixin (iLink bot API); everything that is the same for every chat
// platform (dedup, session reuse, running a turn, one poller per account) is server/channel. This package
// only holds what is specific to WeChat: the login flow, the long-poll loop, turning WeChat messages into
// text for the agent, and sending the answer back.
//
// See README.md in this directory.
package weixin

import (
	"context"
	"errors"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
)

// Errors returned by the service; handler.go maps them to HTTP statuses.
var (
	ErrInvalidArgument    = errors.New("weixin: 参数不合法")
	ErrAccountNotFound    = errors.New("weixin: 微信账号不存在")
	ErrLoginNotFound      = errors.New("weixin: 登录会话不存在或已过期")
	ErrBotInOtherSpace    = errors.New("weixin: 这个微信机器人已接入别的空间")
	ErrAgentNotFound      = errors.New("weixin: agent 不存在")
	ErrAgentInOtherSpace  = errors.New("weixin: agent 不属于这个空间")
	ErrVerifyCodeNotAsked = errors.New("weixin: 当前不需要输入验证码")
	// ErrUpstream wraps failures of the WeChat API itself.
	ErrUpstream = errors.New("weixin: 微信接口调用失败")
)

// AccountRecord is a weixin_accounts row. BotToken is plain text here; the PG store encrypts it.
type AccountRecord struct {
	ID            string
	WorkspaceID   string
	BotID         string
	BotToken      string
	BaseURL       string
	OwnerUserID   string
	AgentID       string
	Status        apimodel.WeixinAccountStatus
	LastError     string
	SyncCursor    string
	CreatedBy     string
	CreatedAt     string
	UpdatedAt     string
	LastInboundAt string
}

// View is what the HTTP API returns: no token, no cursor.
func (r AccountRecord) View() apimodel.WeixinAccount {
	status := r.Status
	if status == "" {
		status = apimodel.WeixinAccountActive
	}
	return apimodel.WeixinAccount{
		ID: r.ID, WorkspaceID: r.WorkspaceID, BotID: r.BotID, OwnerUserID: r.OwnerUserID,
		AgentID: r.AgentID, Status: status, LastError: r.LastError, LastInboundAt: r.LastInboundAt,
		CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

// LoginRecord is a weixin_logins row.
type LoginRecord struct {
	ID                string
	WorkspaceID       string
	CreatedBy         string
	QRCode            string
	QRContent         string
	PollBaseURL       string
	Status            apimodel.WeixinLoginStatus
	PendingVerifyCode string
	RefreshCount      int
	AccountID         string
	Message           string
	CreatedAt         time.Time
	ExpiresAt         time.Time
}

// Store is the storage of this package. It must be shared by all instances (PostgreSQL in production).
type Store interface {
	// UpsertAccount inserts the account, or, when the bot is already connected in the same workspace,
	// updates its token / base URL / owner and marks it active again. When the bot is connected in another
	// workspace it returns ErrBotInOtherSpace. The returned record is what is stored now.
	UpsertAccount(ctx context.Context, rec AccountRecord) (AccountRecord, error)
	GetAccount(ctx context.Context, id string) (AccountRecord, error)
	// ListAccounts lists one workspace's accounts, oldest first.
	ListAccounts(ctx context.Context, workspaceID string) ([]AccountRecord, error)
	// ListActiveAccounts lists every active account of every workspace (for the pollers).
	ListActiveAccounts(ctx context.Context) ([]AccountRecord, error)
	SetAccountAgent(ctx context.Context, id, agentID, now string) error
	SetAccountCursor(ctx context.Context, id, cursor string) error
	SetAccountStatus(ctx context.Context, id string, status apimodel.WeixinAccountStatus, lastError, now string) error
	TouchAccountInbound(ctx context.Context, id, at string) error
	DeleteAccount(ctx context.Context, id string) error
	// RecentBotTokens returns up to n bot tokens, newest first (sent with a new QR-code request so the
	// server can recognize bots that are already connected).
	RecentBotTokens(ctx context.Context, n int) ([]string, error)

	CreateLogin(ctx context.Context, rec LoginRecord) error
	GetLogin(ctx context.Context, id string) (LoginRecord, error)
	UpdateLogin(ctx context.Context, rec LoginRecord) error
	// DeleteExpiredLogins removes logins whose expires_at is before now.
	DeleteExpiredLogins(ctx context.Context, now time.Time) error
}

// Agents is what this package needs to know about agents (cmd/amad adapts core to it).
type Agents interface {
	// AgentWorkspace returns the workspace of an agent, or ErrAgentNotFound.
	AgentWorkspace(ctx context.Context, agentID string) (string, error)
}

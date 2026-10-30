package channeldao

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/shared/infra/cryptobox"
	"github.com/chyroc/ama/server/shared/infra/pg"
	"github.com/chyroc/ama/server/weixin"
)

// WeixinPGStore is the PostgreSQL implementation of weixin.Store.
//
// Bot tokens are encrypted with the cryptobox when one is configured (ark.secretKey, the same key the Ark
// credentials use); without a key they are stored as plain text, like the Ark credentials. Logging about
// which mode is on happens once at startup in cmd/amad.
type WeixinPGStore struct {
	client *pg.Client
	crypto *cryptobox.Box
}

var _ weixin.Store = (*WeixinPGStore)(nil)

// NewWeixinPGStore creates the tables if needed and returns the store. crypto may be nil.
func NewWeixinPGStore(ctx context.Context, client *pg.Client, crypto *cryptobox.Box) (*WeixinPGStore, error) {
	if err := Migrate(ctx, client); err != nil {
		return nil, err
	}
	return &WeixinPGStore{client: client, crypto: crypto}, nil
}

func (s *WeixinPGStore) seal(token string) (string, error) {
	if s.crypto == nil || !s.crypto.Enabled() {
		return token, nil
	}
	return s.crypto.Seal(token)
}

func (s *WeixinPGStore) open(stored string) (string, error) {
	if s.crypto == nil {
		return stored, nil
	}
	return s.crypto.Open(stored)
}

const accountColumns = `id, workspace_id, bot_id, bot_token, base_url, owner_user_id, agent_id, status,
	last_error, sync_cursor, created_by, created_at, updated_at, last_inbound_at`

func (s *WeixinPGStore) scanAccount(row interface{ Scan(...any) error }) (weixin.AccountRecord, error) {
	var r weixin.AccountRecord
	var status string
	if err := row.Scan(&r.ID, &r.WorkspaceID, &r.BotID, &r.BotToken, &r.BaseURL, &r.OwnerUserID, &r.AgentID,
		&status, &r.LastError, &r.SyncCursor, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt, &r.LastInboundAt); err != nil {
		return weixin.AccountRecord{}, err
	}
	r.Status = apimodel.WeixinAccountStatus(status)
	token, err := s.open(r.BotToken)
	if err != nil {
		return weixin.AccountRecord{}, fmt.Errorf("解密微信 token 失败（ark.secretKey 换过？）: %w", err)
	}
	r.BotToken = token
	return r, nil
}

// UpsertAccount implements weixin.Store.
//
// One statement decides it: a new bot is inserted; a bot already in the same workspace gets the new token;
// a bot in another workspace is left alone (the where clause filters the update out) and reported.
func (s *WeixinPGStore) UpsertAccount(ctx context.Context, rec weixin.AccountRecord) (weixin.AccountRecord, error) {
	if rec.ID == "" || rec.WorkspaceID == "" || rec.BotID == "" {
		return weixin.AccountRecord{}, weixin.ErrInvalidArgument
	}
	token, err := s.seal(rec.BotToken)
	if err != nil {
		return weixin.AccountRecord{}, err
	}
	status := rec.Status
	if status == "" {
		status = apimodel.WeixinAccountActive
	}
	row := s.client.QueryRow(ctx, `
		insert into weixin_accounts (`+accountColumns+`)
		values ($1, $2, $3, $4, $5, $6, $7, $8, '', '', $9, $10, $11, '')
		on conflict (bot_id) do update
			set bot_token = excluded.bot_token, base_url = excluded.base_url,
				owner_user_id = excluded.owner_user_id, status = 'active', last_error = '',
				updated_at = excluded.updated_at
			where weixin_accounts.workspace_id = excluded.workspace_id
		returning `+accountColumns,
		rec.ID, rec.WorkspaceID, rec.BotID, token, rec.BaseURL, rec.OwnerUserID, rec.AgentID, string(status),
		rec.CreatedBy, rec.CreatedAt, rec.UpdatedAt)
	out, err := s.scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		// The conflict update was filtered out: the bot belongs to another workspace.
		return weixin.AccountRecord{}, weixin.ErrBotInOtherSpace
	}
	if err != nil {
		return weixin.AccountRecord{}, fmt.Errorf("保存微信账号失败: %w", err)
	}
	return out, nil
}

// GetAccount implements weixin.Store.
func (s *WeixinPGStore) GetAccount(ctx context.Context, id string) (weixin.AccountRecord, error) {
	out, err := s.scanAccount(s.client.QueryRow(ctx, `select `+accountColumns+` from weixin_accounts where id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return weixin.AccountRecord{}, weixin.ErrAccountNotFound
	}
	return out, err
}

func (s *WeixinPGStore) listAccounts(ctx context.Context, where string, args ...any) ([]weixin.AccountRecord, error) {
	rows, err := s.client.Query(ctx, `select `+accountColumns+` from weixin_accounts `+where+` order by created_at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("列微信账号失败: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []weixin.AccountRecord{}
	for rows.Next() {
		r, err := s.scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListAccounts implements weixin.Store.
func (s *WeixinPGStore) ListAccounts(ctx context.Context, workspaceID string) ([]weixin.AccountRecord, error) {
	return s.listAccounts(ctx, `where workspace_id = $1`, workspaceID)
}

// ListActiveAccounts implements weixin.Store.
func (s *WeixinPGStore) ListActiveAccounts(ctx context.Context) ([]weixin.AccountRecord, error) {
	return s.listAccounts(ctx, `where status <> $1`, string(apimodel.WeixinAccountExpired))
}

func (s *WeixinPGStore) updateAccount(ctx context.Context, set string, args ...any) error {
	res, err := s.client.Exec(ctx, `update weixin_accounts set `+set+` where id = $1`, args...)
	if err != nil {
		return fmt.Errorf("更新微信账号失败: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return weixin.ErrAccountNotFound
	}
	return nil
}

// SetAccountAgent implements weixin.Store.
func (s *WeixinPGStore) SetAccountAgent(ctx context.Context, id, agentID, now string) error {
	return s.updateAccount(ctx, `agent_id = $2, updated_at = $3`, id, agentID, now)
}

// SetAccountCursor implements weixin.Store.
func (s *WeixinPGStore) SetAccountCursor(ctx context.Context, id, cursor string) error {
	return s.updateAccount(ctx, `sync_cursor = $2`, id, cursor)
}

// SetAccountStatus implements weixin.Store.
func (s *WeixinPGStore) SetAccountStatus(
	ctx context.Context, id string, status apimodel.WeixinAccountStatus, lastError, now string,
) error {
	return s.updateAccount(ctx, `status = $2, last_error = $3, updated_at = $4`, id, string(status), lastError, now)
}

// TouchAccountInbound implements weixin.Store.
func (s *WeixinPGStore) TouchAccountInbound(ctx context.Context, id, at string) error {
	return s.updateAccount(ctx, `last_inbound_at = $2`, id, at)
}

// DeleteAccount implements weixin.Store.
func (s *WeixinPGStore) DeleteAccount(ctx context.Context, id string) error {
	res, err := s.client.Exec(ctx, `delete from weixin_accounts where id = $1`, id)
	if err != nil {
		return fmt.Errorf("删除微信账号失败: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return weixin.ErrAccountNotFound
	}
	return nil
}

// RecentBotTokens implements weixin.Store.
func (s *WeixinPGStore) RecentBotTokens(ctx context.Context, n int) ([]string, error) {
	rows, err := s.client.Query(ctx, `
		select bot_token from weixin_accounts where bot_token <> '' order by created_at desc, id desc limit $1`, n)
	if err != nil {
		return nil, fmt.Errorf("取微信 token 失败: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var stored string
		if err := rows.Scan(&stored); err != nil {
			return nil, err
		}
		token, err := s.open(stored)
		if err != nil {
			continue
		}
		out = append(out, token)
	}
	return out, rows.Err()
}

const loginColumns = `id, workspace_id, created_by, qrcode, qrcode_content, poll_base_url, status,
	pending_verify_code, refresh_count, account_id, message, created_at, expires_at`

// CreateLogin implements weixin.Store.
func (s *WeixinPGStore) CreateLogin(ctx context.Context, rec weixin.LoginRecord) error {
	if strings.TrimSpace(rec.ID) == "" {
		return weixin.ErrInvalidArgument
	}
	_, err := s.client.Exec(ctx, `insert into weixin_logins (`+loginColumns+`)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		rec.ID, rec.WorkspaceID, rec.CreatedBy, rec.QRCode, rec.QRContent, rec.PollBaseURL, string(rec.Status),
		rec.PendingVerifyCode, rec.RefreshCount, rec.AccountID, rec.Message, rec.CreatedAt, rec.ExpiresAt)
	if err != nil {
		return fmt.Errorf("保存微信登录失败: %w", err)
	}
	return nil
}

// GetLogin implements weixin.Store.
func (s *WeixinPGStore) GetLogin(ctx context.Context, id string) (weixin.LoginRecord, error) {
	var r weixin.LoginRecord
	var status string
	err := s.client.QueryRow(ctx, `select `+loginColumns+` from weixin_logins where id = $1`, id).Scan(
		&r.ID, &r.WorkspaceID, &r.CreatedBy, &r.QRCode, &r.QRContent, &r.PollBaseURL, &status,
		&r.PendingVerifyCode, &r.RefreshCount, &r.AccountID, &r.Message, &r.CreatedAt, &r.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return weixin.LoginRecord{}, weixin.ErrLoginNotFound
	}
	if err != nil {
		return weixin.LoginRecord{}, fmt.Errorf("查微信登录失败: %w", err)
	}
	r.Status = apimodel.WeixinLoginStatus(status)
	return r, nil
}

// UpdateLogin implements weixin.Store.
func (s *WeixinPGStore) UpdateLogin(ctx context.Context, rec weixin.LoginRecord) error {
	res, err := s.client.Exec(ctx, `
		update weixin_logins set qrcode = $2, qrcode_content = $3, poll_base_url = $4, status = $5,
			pending_verify_code = $6, refresh_count = $7, account_id = $8, message = $9
		where id = $1`,
		rec.ID, rec.QRCode, rec.QRContent, rec.PollBaseURL, string(rec.Status), rec.PendingVerifyCode,
		rec.RefreshCount, rec.AccountID, rec.Message)
	if err != nil {
		return fmt.Errorf("更新微信登录失败: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return weixin.ErrLoginNotFound
	}
	return nil
}

// DeleteExpiredLogins implements weixin.Store.
func (s *WeixinPGStore) DeleteExpiredLogins(ctx context.Context, now time.Time) error {
	if _, err := s.client.Exec(ctx, `delete from weixin_logins where expires_at < $1`, now); err != nil {
		return fmt.Errorf("清理过期微信登录失败: %w", err)
	}
	return nil
}

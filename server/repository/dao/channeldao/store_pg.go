package channeldao

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chyroc/ama/server/channel"
	"github.com/chyroc/ama/server/shared/infra/pg"
)

// PGStore is the PostgreSQL implementation of channel.Store.
type PGStore struct {
	client *pg.Client
}

var _ channel.Store = (*PGStore)(nil)

// NewPGStore creates the tables if needed and returns the store.
func NewPGStore(ctx context.Context, client *pg.Client) (*PGStore, error) {
	if err := Migrate(ctx, client); err != nil {
		return nil, err
	}
	return &PGStore{client: client}, nil
}

// ClaimInbound implements channel.Store.
//
// One statement decides it: a new row is inserted as pending; an existing row is taken over only when it
// is failed. RowsAffected tells which happened.
func (s *PGStore) ClaimInbound(ctx context.Context, msg channel.InboundMessage) (bool, error) {
	if strings.TrimSpace(msg.MessageID) == "" || strings.TrimSpace(msg.AccountID) == "" {
		return false, channel.ErrInvalidArgument
	}
	createdAt := msg.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	res, err := s.client.Exec(ctx, `
		insert into channel_inbound_messages (channel, account_id, message_id, conversation_id, status, created_at)
		values ($1, $2, $3, $4, $5, $6)
		on conflict (channel, account_id, message_id) do update
			set status = excluded.status, created_at = excluded.created_at
			where channel_inbound_messages.status = $7
	`, string(msg.Channel), msg.AccountID, msg.MessageID, msg.ConversationID,
		string(channel.InboundPending), createdAt, string(channel.InboundFailed))
	if err != nil {
		return false, fmt.Errorf("认领入站消息失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		// Cannot tell whether we got it: better not handle it than handle it twice.
		return false, nil
	}
	return n > 0, nil
}

// MarkInbound implements channel.Store.
func (s *PGStore) MarkInbound(
	ctx context.Context, kind channel.Kind, accountID, messageID string, status channel.InboundStatus,
) error {
	_, err := s.client.Exec(ctx, `
		update channel_inbound_messages set status = $4
		where channel = $1 and account_id = $2 and message_id = $3
	`, string(kind), accountID, messageID, string(status))
	if err != nil {
		return fmt.Errorf("更新入站消息状态失败: %w", err)
	}
	return nil
}

// GetScope implements channel.Store.
func (s *PGStore) GetScope(ctx context.Context, key channel.ScopeKey) (channel.Scope, error) {
	row := s.client.QueryRow(ctx, `
		select run_id, workspace_id, created_at, last_used_at, turn_count
		from channel_session_scopes
		where channel = $1 and account_id = $2 and scope_key = $3 and agent_id = $4
	`, string(key.Channel), key.AccountID, key.Key, key.AgentID)
	out := channel.Scope{ScopeKey: key}
	if err := row.Scan(&out.RunID, &out.WorkspaceID, &out.CreatedAt, &out.LastUsedAt, &out.TurnCount); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return channel.Scope{}, channel.ErrScopeNotBound
		}
		return channel.Scope{}, fmt.Errorf("查会话作用域失败: %w", err)
	}
	return out, nil
}

// BindScope implements channel.Store. The primary key decides the winner; the loser reads it back.
func (s *PGStore) BindScope(ctx context.Context, scope channel.Scope) (channel.Scope, bool, error) {
	if scope.Key == "" || scope.RunID == "" || scope.AccountID == "" {
		return channel.Scope{}, false, channel.ErrInvalidArgument
	}
	turns := scope.TurnCount
	if turns <= 0 {
		turns = 1
	}
	res, err := s.client.Exec(ctx, `
		insert into channel_session_scopes (
			channel, account_id, scope_key, agent_id, run_id, workspace_id, created_at, last_used_at, turn_count
		) values ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		on conflict (channel, account_id, scope_key, agent_id) do nothing
	`, string(scope.Channel), scope.AccountID, scope.Key, scope.AgentID, scope.RunID, scope.WorkspaceID,
		scope.CreatedAt, scope.LastUsedAt, turns)
	if err != nil {
		return channel.Scope{}, false, fmt.Errorf("绑定会话作用域失败: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		return scope, true, nil
	}
	winner, err := s.GetScope(ctx, scope.ScopeKey)
	if err != nil {
		return channel.Scope{}, false, err
	}
	return winner, false, nil
}

// TouchScope implements channel.Store.
func (s *PGStore) TouchScope(ctx context.Context, key channel.ScopeKey, now time.Time) error {
	res, err := s.client.Exec(ctx, `
		update channel_session_scopes set last_used_at = $5, turn_count = turn_count + 1
		where channel = $1 and account_id = $2 and scope_key = $3 and agent_id = $4
	`, string(key.Channel), key.AccountID, key.Key, key.AgentID, now)
	if err != nil {
		return fmt.Errorf("更新会话作用域失败: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return channel.ErrScopeNotBound
	}
	return nil
}

// ResetScope implements channel.Store.
func (s *PGStore) ResetScope(ctx context.Context, kind channel.Kind, accountID, key string) error {
	_, err := s.client.Exec(ctx, `
		delete from channel_session_scopes where channel = $1 and account_id = $2 and scope_key = $3
	`, string(kind), accountID, key)
	if err != nil {
		return fmt.Errorf("重置会话作用域失败: %w", err)
	}
	return nil
}

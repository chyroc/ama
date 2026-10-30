package channel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// SessionRequest asks for the session a message should run on.
type SessionRequest struct {
	Scope       ScopeKey
	WorkspaceID string
	// Detail is the source shown in the run history ("WeChat direct chat").
	Detail string
	Now    time.Time
}

// EnsureSession returns the session bound to the scope, creating and binding one when there is none.
//
// # Why not simply "create it if not found"
//
// Two instances can handle two messages of the same conversation at the same moment; both find nothing
// and both create an Ark session. So after creating, the database decides whose counts (BindScope's
// unique key) and the loser switches to the winner's session. Otherwise one conversation ends up with
// two contexts, with no error anywhere. The session the loser created is left unused: an empty Ark
// session costs nothing and has no container (containers are created on the first tool call).
//
// When binding fails for another reason, the new session is still used for this turn: the user gets an
// answer, and only the next message may start a new session.
func EnsureSession(
	ctx context.Context, store Store, conv Conversations, req SessionRequest, log *slog.Logger,
) (runID string, err error) {
	if log == nil {
		log = slog.Default()
	}
	bound, err := store.GetScope(ctx, req.Scope)
	switch {
	case err == nil:
		if err := store.TouchScope(ctx, req.Scope, req.Now); err != nil {
			log.Warn("更新作用域活跃时刻失败", "error", err.Error())
		}
		return bound.RunID, nil
	case errors.Is(err, ErrScopeNotBound):
	default:
		return "", fmt.Errorf("查会话作用域失败: %w", err)
	}

	run, err := conv.CreateChannelConversation(ctx, req.WorkspaceID, req.Scope.AgentID, req.Scope.Channel, req.Detail)
	if err != nil {
		return "", fmt.Errorf("建会话失败: %w", err)
	}
	winner, ok, err := store.BindScope(ctx, Scope{
		ScopeKey:    req.Scope,
		RunID:       run.ID,
		WorkspaceID: req.WorkspaceID,
		CreatedAt:   req.Now,
		LastUsedAt:  req.Now,
		TurnCount:   1,
	})
	if err != nil {
		log.Warn("绑定会话作用域失败，这一轮没有上下文复用", "error", err.Error())
		return run.ID, nil
	}
	if !ok {
		log.Info("会话作用域已被别的实例绑定，改用它那条", "winner", winner.RunID, "discarded", run.ID)
		return winner.RunID, nil
	}
	log.Info("新建会话", "run", run.ID, "scope", req.Scope.Key)
	return run.ID, nil
}

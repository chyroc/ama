// FeishuAuthorizer: connects the two methods of feishu.Authorizer to this package's role checks.
//
// # Why this adapter is needed
//
// The feishu package already abstracted authorization into the Authorizer interface (CanAccessWorkspace / CanManageWorkspace),
// and its Service calls it internally in several places (Begin/Poll in onboarding.go, DeleteApp in apps.go,
// the two in readiness.go). The implementation used to be AllowAllAuthorizer: **it only checked non-empty and allowed everything**.
//
// Guard already blocks the 8 Feishu routes at the HTTP boundary, but those calls inside Service still exist,
// and that is **a good thing**: they guarantee the order "authorize first, then look up the session" (the most important invariant of the feishu package,
// see TestBeginChecksAuthBeforeSession in feishu_test.go), and also cover anyone calling Service from somewhere else
// without going through HTTP. So both layers must check, and **the checks must agree**:
// if they disagree you get "Guard allows, Service rejects", and the error cannot point to the real cause.
//
// This adapter is that agreement: both layers use the same role table and the same level comparison.

package authz

import (
	"context"
	"strings"
)

// FeishuAuthorizer implements feishu.Authorizer.
//
// It deliberately does not import feishu: that would create an authz → feishu dependency, while feishu needs authz's implementation.
// Go's implicit interfaces let neither side know the other; feishu only declares the method shape it needs.
type FeishuAuthorizer struct {
	member Membership
}

// NewFeishuAuthorizer creates an authorizer for Feishu.
func NewFeishuAuthorizer(member Membership) *FeishuAuthorizer {
	return &FeishuAuthorizer{member: member}
}

// CanAccessWorkspace reports whether actor can read the workspace; maps to ActionRead (viewer and up).
func (a *FeishuAuthorizer) CanAccessWorkspace(ctx context.Context, actorID, workspaceID string) (bool, error) {
	return a.allow(ctx, actorID, workspaceID, ActionRead)
}

// CanManageWorkspace reports whether actor can change the workspace config; maps to ActionManage (admin and up).
//
// Connecting a Feishu app counts as Manage: it binds an **external identity** to the workspace, and binding it wrong makes the
// bot of the whole workspace reply to the wrong people.
func (a *FeishuAuthorizer) CanManageWorkspace(ctx context.Context, actorID, workspaceID string) (bool, error) {
	return a.allow(ctx, actorID, workspaceID, ActionManage)
}

// allow is the check shared by both methods.
//
// An empty identity or empty workspace is always rejected (fail-closed). The old AllowAllAuthorizer allowed as soon as
// both were non-empty; now non-empty is only a **precondition**, and the actor must really be a member of high enough level.
func (a *FeishuAuthorizer) allow(
	ctx context.Context, actorID, workspaceID string, action Action,
) (bool, error) {
	actor := strings.TrimSpace(actorID)
	ws := strings.TrimSpace(workspaceID)
	if actor == "" || ws == "" {
		return false, nil
	}
	role, isMember, err := a.member.RoleOf(ctx, ws, actor)
	if err != nil {
		// A database lookup failure must be **returned upward**, not turned into "no permission" by returning false:
		// feishu's requireManage turns false into ErrAccessDenied → 403,
		// so database jitter would show as "you have no permission", the user would go ask an admin for permission,
		// and the admin would see that they are clearly the owner. Returning the error makes it a 500.
		return false, err
	}
	if !isMember {
		return false, nil
	}
	return levelOf(role) >= requiredLevel(action), nil
}

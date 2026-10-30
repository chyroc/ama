// Package workspace is the orchestration layer for the main areas: workspaces / members / runs /
// chats / schedules / preferences.
//
// Agents and skills used to live here too (they had no domain package of their own, so their
// methods hung off this mixed domain); they are now
// the tenth and eleventh domain packages, orchestrator/agent and orchestrator/skill, and 9 methods each
// have been split out of this interface.

package workspace

import (
	"context"
	"encoding/json"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/shared/infra/ma"
)

// Service is the set of methods the handler really uses in this domain. **Only the used ones are listed.**
//
// This is a "narrow interface from the consumer's point of view": new methods on the domain package
// (still core.Service for now) are
// invisible to the handler by default; to expose one you must add a line in this file explicitly.
// What this saves is the 161
// empty forwarding methods: an interface needs only signatures, not function bodies.
//
// ⚠️ **The name is just Service; the package name carries the domain** (`issue.Service` / `ark.Service`).
// Earlier, nine interfaces were packed into one package orchestrator and could only be told apart
// by a prefix
// (IssueService / ArkService); after splitting into orchestrator/<domain>/ that prefix became
// repetition: the package name already says which domain it is.
//
// ⚠️ The package name only guards **between domain interface declarations**: earlier the nine
// interfaces shared a package, and one domain's file could
// reference another domain's interface name with no error; now doing so needs an import of the
// sibling package, which is visible.
// **Blocking cross-domain access on the handler side does not rely on this**: that is enforced by
// "the field type is a narrow interface"; see
// the comments on the ten fields of Handler in apihandler/handler.go.
//
// ⚠️ **Signatures always use apimodel.X, never core.X**. This is not style, it is a hard rule:
// if this package imported core, core would show up in `go list -deps ./orchestrator/...`,
// the orchestration layer would again depend on the package it is meant to hide, and the layer
// would be pointless (the full reasoning is in
// the file header of orchestrator/routes.go). Step 1 moved these types into domain
// precisely so that these signatures could be written.
type Service interface {
	AddMember(ctx context.Context, workspaceID string, in apimodel.MemberInput) (apimodel.WorkspaceMember, error)
	AppendRunEvents(ctx context.Context, runID string, raw []json.RawMessage) ([]json.RawMessage, error)
	// Passes raw Ark API calls straight through; see core/arkproxy.go.
	ArkProxyDoForRun(ctx context.Context, runID string, req ma.RawRequest) (*ma.RawResponse, error)
	ArkProxyDoForWorkspace(ctx context.Context, workspaceID string, req ma.RawRequest) (*ma.RawResponse, error)
	ArkProxyStreamForRun(ctx context.Context, runID string, req ma.RawRequest) (*ma.RawStream, error)
	ArkProxyStreamForWorkspace(ctx context.Context, workspaceID string, req ma.RawRequest) (*ma.RawStream, error)
	CancelPendingMessage(ctx context.Context, runID, id string) error
	Clear(ctx context.Context) error
	CreateConversation(ctx context.Context, workspaceID, agentID string) (apimodel.Run, error)
	CreateSchedule(
		ctx context.Context, workspaceID string, draft apimodel.ScheduleDraft,
	) (apimodel.Schedule, error)
	CreateWorkspace(ctx context.Context, in apimodel.WorkspaceInput) (apimodel.Workspace, error)
	DeleteSchedule(ctx context.Context, id string) error
	DeleteWorkspace(ctx context.Context, id string) error
	EnqueuePendingMessage(
		ctx context.Context, runID, text string,
	) ([]apimodel.PendingMessage, error)
	GetOnboarding(ctx context.Context) (apimodel.OnboardingState, error)
	GetRun(ctx context.Context, id string) (apimodel.Run, error)
	GetRunArkConsole(ctx context.Context, runID string) (apimodel.ArkConsoleRef, error)
	GetSchedule(ctx context.Context, id string) (apimodel.Schedule, error)
	GetUserPreferences(ctx context.Context) (apimodel.UserPreferences, error)
	GetWorkspaceByKey(ctx context.Context, key string) (apimodel.Workspace, error)
	IsWorkspaceKeyTaken(ctx context.Context, key, exceptID string) (bool, error)
	ListFeedback(ctx context.Context, q persistmodel.FeedbackQuery) (apimodel.FeedbackPage, error)
	ListPendingMessages(
		ctx context.Context, runID string,
	) ([]apimodel.PendingMessage, error)
	ListRunEvents(ctx context.Context, runID string, q persistmodel.EventQuery) ([]apimodel.RunEvent, error)
	ListRuns(ctx context.Context, workspaceID string, q persistmodel.RunQuery) ([]apimodel.Run, error)
	ListSchedules(ctx context.Context, workspaceID string) ([]apimodel.Schedule, error)
	ListWorkspacesFor(ctx context.Context, actor string) ([]apimodel.Workspace, error)
	MarkOnboarding(ctx context.Context, action apimodel.OnboardingAction, version int) (apimodel.OnboardingState, error)
	RedirectRun(
		ctx context.Context, runID, text string,
	) ([]json.RawMessage, error)
	RemoveMember(ctx context.Context, workspaceID, memberID string) error
	Reset(ctx context.Context) error
	ResolveUserProfiles(
		ctx context.Context, emails []string,
	) (map[string]apimodel.UserProfile, error)
	RoleOf(
		ctx context.Context, workspaceID, actor string,
	) (apimodel.WorkspaceRole, bool, error)
	RunExists(ctx context.Context, runID string) error
	RunScheduleNow(ctx context.Context, id string) (apimodel.Schedule, error)
	SaveUserPreferences(ctx context.Context, prefs apimodel.UserPreferences) (apimodel.UserPreferences, error)
	SetScheduleStatus(
		ctx context.Context, id string, status apimodel.ScheduleStatus,
	) (apimodel.Schedule, error)
	StreamRunEvents(
		ctx context.Context, runID string, onFrame func(raw []byte) error,
	) error
	SubmitFeedback(ctx context.Context, in apimodel.FeedbackInput) error
	UpdateMemberRole(
		ctx context.Context, workspaceID, memberID string, role apimodel.WorkspaceRole,
	) (apimodel.WorkspaceMember, error)
	UpdateSchedule(
		ctx context.Context, id string, draft apimodel.ScheduleDraft,
	) (apimodel.Schedule, error)
	UpdateWorkspace(ctx context.Context, id string, patch apimodel.WorkspacePatch) (apimodel.Workspace, error)
	WorkspaceUsage(ctx context.Context, workspaceID string, q persistmodel.UsageQuery) (apimodel.UsageSummary, error)
}

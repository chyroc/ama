// Package authz is AMA's authorization layer: it decides whether a request may pass based on "workspace member role".
//
// # How the work is split with the auth package
//
//	auth  answers "who are you": SSO session / API Key → Identity (the email is the Actor ID)
//	authz answers "are you allowed": Actor + workspace → role → is the level enough for this action
//
// Authentication first, authorization after: not logged in is 401 (auth's job); logged in but not allowed is 403 (this package's job).
//
// # Why authorization is in the handler layer, not the Service layer
//
// **Because core.Service methods are shared by two kinds of callers, and one kind has no identity.**
//
//	HTTP requests          ctx has auth.Identity: needs authorization
//	worker pool sweep      refresh in core/worker.go, process-level ctx: no identity
//	Feishu inbound msgs    dispatch in feishu/inbound.go, long-connection ctx: no identity
//	scheduled task firing  fire in core/scheduler.go, process-level ctx: no identity
//
// If "look up the current user's role" were put into Service, the last three would all be judged non-members, and their failures **report no errors**:
// the worker pool finds 0 workspaces → an agent gets stuck in requires_action as soon as it calls a tool; the Feishu bot silently stops replying;
// all scheduled tasks fail. Faults like this pass all tests on a single instance and show up in production as "sometimes works, sometimes not".
//
// So this package only provides an HTTP decorator (Guard), and system-driven paths never go through it.
// **Do not move the check down into Service to be "more thorough"**: that is not safer; it breaks three paths.
//
// # Missing one route declaration = crash at startup
//
// Permissions are **declared in one table by method + pattern** (routePermissions in matrix.go).
// When routes are registered, Guard compares against that table in both directions, and panics if either side does not match:
//
//	registered but not declared → a new route forgot to declare permissions, and would silently allow (the most dangerous kind of miss)
//	declared but not registered → a typo in the pattern, so that declaration never matches (checking only one direction cannot catch it)
//
// panic instead of logging: this repo once had an incident of "a purely new module that was never wired into the main path": zero errors,
// all tests passing, and the feature not in effect. Authorization is the kind of thing where "not in effect" cannot be seen, so it must fail loudly.
package authz

import (
	"context"
	"errors"

	"github.com/chyroc/ama/server/model/apimodel"
)

// Sentinel errors. Guard writes them as HTTP responses itself; they are exposed for FeishuAuthorizer
// and tests (the feishu side translates them into its own ErrAccessDenied).
var (
	// ErrNotMember means this person is not a member of the workspace. 403.
	ErrNotMember = errors.New("authz: 不是该空间的成员")

	// ErrForbidden means a member whose role level is not enough. 403.
	//
	// Kept apart from ErrNotMember because **what we tell the user differs**: the former says "you are not in this workspace"
	// (ask a workspace admin to invite you), the latter says "your role is not enough" (ask an admin to raise it).
	// Merging them into one sentence leaves users not knowing what to do.
	ErrForbidden = errors.New("authz: 操作权限不足")
)

// User-facing messages. The frontend shows these two sentences directly, so they are part of the contract.
const (
	MsgNotMember = "您不是该工作区的成员，无权访问"
	MsgForbidden = "操作权限不足"
	// MsgPlatformReadOnly is the message for "trying to change a built-in platform resource".
	//
	// Kept apart from MsgForbidden: that message sends users to an admin for permission, while for this **nobody can**,
	// and calling it a permission problem would send them on a wasted trip.
	MsgPlatformReadOnly = "平台内置资源由平台维护，不可修改"
)

// Level is a permission level and is comparable: a bigger number means more permission.
type Level int

const (
	// LevelNone is not a member and can do nothing.
	LevelNone Level = iota
	// LevelViewer is read-only.
	LevelViewer
	// LevelMember can operate (start chats, change agents and skills).
	LevelMember
	// LevelAdmin can manage (workspace settings, members, credentials, Feishu integration). owner and admin are the same level.
	LevelAdmin
)

// Action is the action level an endpoint belongs to.
type Action int

const (
	// ActionNone needs no workspace authorization (global endpoints, personal endpoints).
	//
	// **It still requires login**: that is guaranteed by auth.Protect and has nothing to do with this package.
	// It is defined explicitly instead of "allow if not in the table", so that a missing declaration can be caught by panic.
	ActionNone Action = iota
	// ActionRead is read: view workspaces, browse agents / skills / run history, view chats. viewer and up.
	ActionRead
	// ActionWrite is write: start chats, trigger Runs, create/edit/delete agents and skills, change scheduled tasks. member and up.
	ActionWrite
	// ActionManage is manage: workspace settings, add/edit/remove members, delete workspaces, Ark credentials, Feishu integration. admin and up.
	ActionManage
)

// levelOf turns a role into a level.
//
// **owner and admin are the same level**: in the product, owner only has one extra protection, "cannot be removed"
// (countOwners in core/workspace.go); the permissions are the same. Two levels would make every check
// think again "can admin do this", and the answer is always "yes".
//
// Unrecognized role strings are always LevelNone (fail-closed): if a misspelled role shows up in the database,
// better to reject it so someone notices than to let it in with minimal permission.
func levelOf(role apimodel.WorkspaceRole) Level {
	switch role {
	case apimodel.RoleOwner, apimodel.RoleAdmin:
		return LevelAdmin
	case apimodel.RoleMember:
		return LevelMember
	case apimodel.RoleViewer:
		return LevelViewer
	default:
		return LevelNone
	}
}

// requiredLevel is the lowest level an action requires.
func requiredLevel(a Action) Level {
	switch a {
	case ActionRead:
		return LevelViewer
	case ActionWrite:
		return LevelMember
	case ActionManage:
		return LevelAdmin
	default:
		return LevelNone
	}
}

// ResourceKind is the resource type whose owning workspace must be resolved for a flat path.
//
// The paths of single-resource endpoints (like /api/agents/{id}) **have no workspaceId**: that matches
// the existing shape of the frontend ApiClient (item-level methods pass only one opaque id). So we first look up by id
// which workspace the resource belongs to before we can check permissions.
type ResourceKind int

const (
	// KindNone needs no resolution (the workspace comes straight from the path parameter, or is not needed at all).
	KindNone ResourceKind = iota
	KindAgent
	KindSkill
	KindRun
	KindSchedule
	KindFeishuApp
	// KindIssue / KindIssueComment / KindIssueAttachment are the three of the issue domain.
	//
	// ⚠️ **KindIssueAttachment is the only resource in the whole table that is requested by `<img src>`.**
	// Its URL appears in Markdown bodies and is loaded by the browser as an image, so it looks like a static resource,
	// but **that is exactly why it needs resolution even more**: without checking the owning workspace, anyone with the URL could see images in
	// someone else's workspace, and that leak produces no error, and no UI would ever show that it happened.
	//
	// Resolution goes through core's GetIssueAttachment (reads metadata only, does not fetch bytes from object storage),
	// so the one extra query per image request is cheap.
	KindIssue
	KindIssueComment
	KindIssueAttachment
)

// Membership is the two pieces of data this package needs. Wired by cmd/amad with core.Service and feishu.Store.
//
// It is an interface instead of a direct dependency on core: core and feishu are deliberately coupled only by ID strings, and so is this package:
// it only declares "needs to look up roles and resolve ownership", and does not care who holds the data.
type Membership interface {
	// RoleOf returns actor's role in the workspace.
	//
	// The second return value false means **not a member** (kept apart from "database lookup failed": the latter goes through error,
	// which Guard maps to 500 instead of 403; database jitter should not be called "you have no permission").
	RoleOf(ctx context.Context, workspaceID, actor string) (apimodel.WorkspaceRole, bool, error)

	// WorkspaceOf resolves the owning workspace of a resource.
	//
	// When the resource does not exist it returns **an error wrapping ErrResourceNotFound**, and Guard replies 404 based on it
	// instead of 403; see the long comment in guard.go.
	WorkspaceOf(ctx context.Context, kind ResourceKind, id string) (string, error)
}

// ErrResourceNotFound means the resource to resolve does not exist. Guard replies 404 based on it.
//
// **It must be kept apart from "not a member"**: six nullable getters in the frontend rely on code == "not_found" in the envelope
// to turn the response into null (nullable in web/src/data/http.ts). Replying 403 "no access" for "this agent has been deleted"
// would make the user think it is a permission problem and go to an admin, while that agent is long gone:
// a reasonable but completely wrong sentence is much harder to debug than an error.
var ErrResourceNotFound = errors.New("authz: 资源不存在")

// ErrPlatformResource means this resource is **platform-level** and belongs to no workspace.
//
// Currently only built-in platform skills produce it (core/skillbuiltin.go). Guard turns it into
// **allow reads, reject writes**: built-in skills are visible and selectable in every workspace, but the body is maintained by the platform and nobody can change it.
//
// # Why "resolved to an empty string" cannot express this
//
// Guard allows directly when workspaceID is an empty string (that is the opening left for allowMissing list endpoints),
// and RoleOf never runs. Using an empty string for "no workspace" would **open up write permission too**,
// and logDenied would not record it: a privilege escalation with zero errors. So an explicit sentinel is needed.
var ErrPlatformResource = errors.New("authz: 平台级资源，不属于任何空间")

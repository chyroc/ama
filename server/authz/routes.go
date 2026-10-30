// Exposes routePermissions read-only, for the OpenAPI generator.
//
// # Why this opening exists
//
// `server/openapi` must generate the spec from **the same table**. A hand-written spec always ends up as "the code changed,
// the doc did not", and an agent with an outdated spec keeps calling the wrong endpoints; that error does not point to the real cause
// (it sees 404 or 403, while the real cause is "this endpoint was renamed three months ago").
//
// Generating from this table never drifts, because the table is already pinned by the two-way check at startup:
// one missing → panic when that route is registered; one extra → Verify() errors. In other words,
// "the table matches the real routes" is already enforced, and the spec is correct automatically as long as it follows the table.
//
// # Why a copy instead of exporting the map directly
//
// Exporting the map would make the security boundary writable: any package could do `authz.RoutePermissions["..."] = ...`
// to lower a route's level, and that would **report no error at all** (see the matrix.go file header).
// So this returns a value copy; callers cannot change the original table.

package authz

import (
	"sort"

	"github.com/chyroc/ama/server/model/apimodel"
)

// RouteDecl is the permission declaration of one route, read-only outside the package.
type RouteDecl struct {
	// Method is the HTTP method, like GET. The fallback route "/api/" has no method; it is an empty string here.
	Method string
	// Path is the route pattern, like /api/agents/{id}.
	Path string
	// Action is the string form of the permission level; see ActionName for values.
	Action string
	// MinRole is the lowest role that meets this level, like "viewer". Empty for ActionNone.
	MinRole string
	// ResourceKind, when non-empty, says this route's workspace is resolved from a resource id of that type.
	ResourceKind string
	// IDParam is the name of the resource id parameter in the path; only set when ResourceKind is non-empty.
	IDParam string
	// AllowMissing being true allows the request even if the resource cannot be resolved (the kind of list endpoint that never 404s).
	AllowMissing bool
	// Optional being true means this route may not be registered (the two _debug ones, only present when DevRoutes is on).
	Optional bool
}

// Routes returns all route declarations, sorted by Path then Method.
//
// **Sorting keeps the generated spec stable**: Go's map iteration order is random; without sorting, each
// generation gives different bytes, and the "is the spec up to date" check would always fail.
func Routes() []RouteDecl {
	out := make([]RouteDecl, 0, len(routePermissions))
	for pattern, perm := range routePermissions {
		decl := RouteDecl{
			Action:       ActionName(perm.action),
			MinRole:      minRoleName(perm.action),
			ResourceKind: kindName(perm.kind),
			IDParam:      perm.idParam,
			AllowMissing: perm.allowMissing,
			Optional:     optionalRoutes[pattern],
		}
		// The fallback route "/api/" has no method prefix; splitting it would treat the whole path as the method.
		if method, path, ok := splitPattern(pattern); ok {
			decl.Method, decl.Path = method, path
		} else {
			decl.Path = pattern
		}
		out = append(out, decl)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})
	return out
}

// splitPattern splits "GET /api/x" into method and path. The third return value false means there is no method prefix.
func splitPattern(pattern string) (method, path string, ok bool) {
	for i := 0; i < len(pattern); i++ {
		if pattern[i] == ' ' {
			return pattern[:i], pattern[i+1:], true
		}
	}
	return "", pattern, false
}

// ActionName turns a level into a stable string; the spec describes permissions with it.
//
// The values deliberately match the Action constant names (lowercase, without the Action prefix): the words the spec's readers (agents)
// get must match this repo's docs.
func ActionName(a Action) string {
	switch a {
	case ActionRead:
		return "read"
	case ActionWrite:
		return "write"
	case ActionManage:
		return "manage"
	default:
		return "none"
	}
}

// minRoleName is the name of the lowest role that meets this level. ActionNone has no role requirement and returns an empty string.
func minRoleName(a Action) string {
	switch a {
	case ActionRead:
		return string(apimodel.RoleViewer)
	case ActionWrite:
		return string(apimodel.RoleMember)
	case ActionManage:
		// owner and admin are the same level; admin is reported (see the comment on levelOf).
		return string(apimodel.RoleAdmin)
	default:
		return ""
	}
}

// kindName turns a resource kind into a string; KindNone returns an empty string.
func kindName(k ResourceKind) string {
	switch k {
	case KindAgent:
		return "agent"
	case KindSkill:
		return "skill"
	case KindRun:
		return "run"
	case KindSchedule:
		return "schedule"
	case KindFeishuApp:
		return "feishuApp"
	case KindIssue:
		return "issue"
	case KindIssueComment:
		return "issueComment"
	case KindIssueAttachment:
		return "issueAttachment"
	default:
		return ""
	}
}

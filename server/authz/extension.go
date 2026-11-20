package authz

import (
	"fmt"
	"net/http"
	"strings"
)

// Route declares permission for one downstream HTTP route. Method and Pattern are separate to
// avoid parsing a display string when the Guard is built. KindNone means the workspace id comes
// from Pattern's {workspaceId} parameter; a resource Kind resolves it through Membership instead.
type Route struct {
	Method  string
	Pattern string
	Action  Action

	Kind         ResourceKind
	IDParam      string
	AllowMissing bool
	Optional     bool
}

// RouteTable is an immutable snapshot of the built-in table plus explicit downstream routes.
// Construct it with NewRouteTable; its maps are never returned to callers.
type RouteTable struct {
	permissions map[string]permission
	optional    map[string]bool
}

// NewRouteTable adds downstream declarations to a copy of the built-in table. A collision with a
// built-in or another downstream route is an error, not an override: a plugin must not silently
// lower a built-in route's security level.
func NewRouteTable(declarations []Route) (*RouteTable, error) {
	t := &RouteTable{
		permissions: make(map[string]permission, len(routePermissions)+len(declarations)),
		optional:    make(map[string]bool, len(optionalRoutes)+len(declarations)),
	}
	for pattern, perm := range routePermissions {
		t.permissions[pattern] = perm
	}
	for pattern, optional := range optionalRoutes {
		t.optional[pattern] = optional
	}
	for _, decl := range declarations {
		method := strings.ToUpper(strings.TrimSpace(decl.Method))
		pattern := strings.TrimSpace(decl.Pattern)
		key := method + " " + pattern
		if method == "" || !strings.HasPrefix(pattern, "/") || strings.ContainsAny(pattern, " \t\r\n") {
			return nil, fmt.Errorf("authz: route requires an HTTP method and an absolute pattern without whitespace")
		}
		if _, exists := t.permissions[key]; exists {
			return nil, fmt.Errorf("authz: duplicate route declaration %q", key)
		}
		if decl.Action != ActionNone && decl.Action != ActionRead &&
			decl.Action != ActionWrite && decl.Action != ActionManage {
			return nil, fmt.Errorf("authz: route %q has an invalid action", key)
		}
		if decl.Action == ActionNone && (decl.Kind != KindNone || decl.AllowMissing || decl.IDParam != "") {
			return nil, fmt.Errorf("authz: ActionNone route %q cannot resolve a resource", key)
		}
		if decl.Kind == KindNone && decl.IDParam != "" {
			return nil, fmt.Errorf("authz: workspace route %q must not declare IDParam", key)
		}
		if decl.Kind != KindNone && strings.TrimSpace(decl.IDParam) == "" {
			return nil, fmt.Errorf("authz: resource route %q requires IDParam", key)
		}
		if decl.Action != ActionNone && decl.Kind == KindNone && !strings.Contains(pattern, "{workspaceId}") {
			return nil, fmt.Errorf("authz: workspace route %q requires {workspaceId}", key)
		}
		if decl.AllowMissing && method != http.MethodGet {
			return nil, fmt.Errorf("authz: only GET route %q may allow a missing resource", key)
		}
		t.permissions[key] = permission{
			action: decl.Action, kind: decl.Kind, idParam: decl.IDParam, allowMissing: decl.AllowMissing,
		}
		t.optional[key] = decl.Optional
	}
	return t, nil
}

func (t *RouteTable) permission(pattern string) (permission, bool) {
	if t == nil {
		return permission{}, false
	}
	perm, ok := t.permissions[pattern]
	return perm, ok
}

func (t *RouteTable) optionalRoute(pattern string) bool {
	return t != nil && t.optional[pattern]
}

// Routes returns independent declarations for OpenAPI generation. Callers cannot modify the
// table through the returned slice.
func (t *RouteTable) Routes() []RouteDecl {
	return routeDeclarations(t.permissions, t.optional)
}

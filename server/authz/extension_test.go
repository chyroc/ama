package authz

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/chyroc/ama/server/auth"
)

func TestRouteTableAddsDownstreamRouteWithoutOverridingBuiltins(t *testing.T) {
	declaration := Route{Method: "get", Pattern: "/api/workspaces/{workspaceId}/company", Action: ActionRead}
	table, err := NewRouteTable([]Route{declaration, declaration})
	if err == nil {
		t.Fatal("duplicate downstream declaration must fail")
	}
	table, err = NewRouteTable([]Route{declaration})
	if err != nil {
		t.Fatal(err)
	}
	perm, ok := table.permission("GET /api/workspaces/{workspaceId}/company")
	if !ok || perm.action != ActionRead {
		t.Fatalf("downstream declaration not preserved: %+v, %v", perm, ok)
	}
	if got := len(table.Routes()); got != len(Routes())+1 {
		t.Fatalf("route count = %d, want one more than built-ins", got)
	}
	if _, err := NewRouteTable([]Route{{
		Method: http.MethodGet, Pattern: "/api/agents/{id}", Action: ActionNone,
	}}); err == nil {
		t.Fatal("downstream must not downgrade a built-in route")
	}
}

func TestRouteTableRejectsUnsafeOrIncompleteDeclarations(t *testing.T) {
	cases := []Route{
		{Pattern: "/api/workspaces/{workspaceId}/x", Action: ActionRead},
		{Method: http.MethodGet, Pattern: "relative", Action: ActionRead},
		{Method: http.MethodGet, Pattern: "/api/no-workspace", Action: ActionRead},
		{Method: http.MethodGet, Pattern: "/api/workspaces/{workspaceId}/x", Action: Action(99)},
		{Method: http.MethodGet, Pattern: "/api/x/{id}", Action: ActionRead, Kind: KindAgent},
		{Method: http.MethodPost, Pattern: "/api/x/{id}", Action: ActionRead, Kind: KindAgent, IDParam: "id", AllowMissing: true},
		{Method: http.MethodGet, Pattern: "/api/x", Action: ActionNone, Kind: KindAgent, IDParam: "id"},
	}
	for _, declaration := range cases {
		if _, err := NewRouteTable([]Route{declaration}); err == nil {
			t.Errorf("declaration %+v must be rejected", declaration)
		}
	}
}

func TestGuardEnforcesDownstreamRouteAndVerifiesRegistration(t *testing.T) {
	declaration := Route{
		Method: http.MethodGet, Pattern: "/api/workspaces/{workspaceId}/company", Action: ActionRead,
	}
	table, err := NewRouteTable([]Route{declaration})
	if err != nil {
		t.Fatal(err)
	}
	g, err := New(Config{Membership: newFake(), Routes: table})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	g.Wrap(muxAdapter{mux}).HandleFunc("GET /api/workspaces/{workspaceId}/company", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	for _, tc := range []struct {
		actor string
		want  int
	}{
		{viewerMail, http.StatusNoContent},
		{strangerML, http.StatusForbidden},
		{"", http.StatusUnauthorized},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/workspaces/ws_demo/company", nil)
		if tc.actor != "" {
			req = req.WithContext(auth.WithIdentity(req.Context(), auth.Identity{Email: tc.actor}))
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("actor %q got %d, want %d", tc.actor, rec.Code, tc.want)
		}
	}
	if err := g.Verify(); err == nil {
		t.Fatal("Verify must still require every built-in route")
	}
	if got := g.Routes(); !slices.ContainsFunc(got, func(decl RouteDecl) bool {
		return decl.Method == http.MethodGet && decl.Path == "/api/workspaces/{workspaceId}/company" && decl.Action == "read"
	}) {
		t.Fatal("Guard's OpenAPI declarations must include downstream routes")
	}
}

func TestWithoutBuiltinsSkipsInstallationButNeverWeakensPermission(t *testing.T) {
	table, err := NewRouteTable(nil)
	if err != nil {
		t.Fatal(err)
	}
	const pattern = "GET /api/workspaces/{workspaceId}/weixin/accounts"
	if err := table.WithoutBuiltins(pattern); err != nil {
		t.Fatal(err)
	}
	perm, ok := table.permission(pattern)
	if !ok || perm.action != ActionRead || !table.optionalRoute(pattern) {
		t.Fatalf("optional module lost its permission: %+v, %v", perm, ok)
	}
	if err := table.WithoutBuiltins("GET /api/not-a-built-in"); err == nil {
		t.Fatal("unknown built-in route must be rejected")
	}
}

func TestGuardVerifyChecksOptionalDownstreamRoute(t *testing.T) {
	table, err := NewRouteTable([]Route{{
		Method: http.MethodGet, Pattern: "/api/workspaces/{workspaceId}/optional", Action: ActionRead, Optional: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	g, err := New(Config{Membership: newFake(), Routes: table})
	if err != nil {
		t.Fatal(err)
	}
	for pattern := range g.routes.permissions {
		if !g.routes.optionalRoute(pattern) {
			g.registered[pattern] = true
		}
	}
	if err := g.Verify(); err != nil {
		t.Fatalf("optional downstream route must not require registration: %v", err)
	}
	delete(g.registered, "GET /api/workspaces/{workspaceId}/optional")
}

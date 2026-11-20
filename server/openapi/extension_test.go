package openapi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/authz"
)

func TestGenerateWithRoutesIncludesDownstreamPermissions(t *testing.T) {
	table, err := authz.NewRouteTable([]authz.Route{{
		Method: "GET", Pattern: "/api/workspaces/{workspaceId}/company", Action: authz.ActionRead,
	}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := GenerateWithRoutes(table.Routes())
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	paths, _ := doc["paths"].(map[string]any)
	path, ok := paths["/api/workspaces/{workspaceId}/company"].(map[string]any)
	if !ok {
		t.Fatal("downstream path is missing from generated OpenAPI")
	}
	get, _ := path["get"].(map[string]any)
	description, _ := get["description"].(string)
	if !strings.Contains(description, "read") || !strings.Contains(description, "viewer") {
		t.Fatalf("downstream permission is missing: %q", description)
	}
}

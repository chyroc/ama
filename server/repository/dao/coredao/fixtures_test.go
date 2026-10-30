// Fixtures shared by the external test package (package core_test).
//
// # Why this file exists
//
// The contract tests moved to package core_test (they need to build the PGStore from repository/dao,
// and staying in the internal package would be an import cycle). After the move they cannot reach the
// fixtures defined in service_test.go, which is still in package core.
//
// Only constants that **both sides need and that are not worth copying** go here. Plain constructors (the
// three-to-five-line kind like prefsCtx and newConversationRun) deliberately keep one copy each and are not centralized here:
// centralizing them would create a coupling point where "one change affects two groups of tests",
// when they should be independent anyway.

package coredao

import (
	"github.com/chyroc/ama/server/repository/dao/coremem"
	"testing"

	"github.com/chyroc/ama/server/core"
)

// seedWS is the id of the workspace in the seed data.
//
// It must match the constant of the same name in service_test.go: both assert on the same seed.json.
// The source of truth is model/persistmodel/seed.json (exported from web/src/data/seed.ts).
const seedWS = "ws_demo"

// newService builds a Service with seed data, equivalent to the function of the same name in service_test.go.
//
// It only uses public API (NewSeededMemStore + New), so the external package can write an identical copy.
// The two copies must stay the same. If they differ, "the same assertion reaches different conclusions in the two groups",
// and that failure looks like a business logic bug when really the fixtures have drifted.
func newService(t *testing.T) (*core.Service, *coremem.MemStore) {
	t.Helper()
	store, err := coremem.NewSeededMemStore()
	if err != nil {
		t.Fatalf("装载种子失败：%v", err)
	}
	svc, err := core.New(core.Config{Store: store})
	if err != nil {
		t.Fatalf("装配 Service 失败：%v", err)
	}
	return svc, store
}

// Authorizers used in tests.
//
// These two used to live in feishu_test.go, which moved to
// repository/dao/feishudao together with the PG implementation. The tests left in this package (apps_binding_test.go) still need them,
// so they are split out into their own file. Having a copy on each side is deliberate: they are **test-local stand-ins**,
// not a production contract, and exporting them for sharing would let production code see something like "allow everything".

package feishu

import (
	"context"
	"sync/atomic"
)

// The actor and workspace shared by tests in this package.
//
// Originally defined in feishu_test.go; that file moved to repository/dao/feishudao together with the PG implementation.
// Tests left in this package still need them, so they sit next to the authorizer stand-ins.
const (
	testActor     = "u-alice"
	testWorkspace = "ws-1"
)

type denyAuthorizer struct{ calls int32 }

func (d *denyAuthorizer) CanAccessWorkspace(ctx context.Context, actorID, workspaceID string) (bool, error) {
	atomic.AddInt32(&d.calls, 1)
	return false, nil
}

func (d *denyAuthorizer) CanManageWorkspace(ctx context.Context, actorID, workspaceID string) (bool, error) {
	atomic.AddInt32(&d.calls, 1)
	return false, nil
}

// allowAuthorizer allows everything, **only used in tests**.
//
// The production fallback is denyAllAuthorizer (always deny; see the "why the fallback is deny" section in feishu.go),
// so most tests (what they check is **Feishu business logic** such as idempotent registration, dual-domain correction, readiness diagnosis,
// not authorization) must pass an allowing authorizer explicitly, or they all stop at 403 and never reach what they want to test.
//
// Real authorization decisions belong to server/authz, which has its own tests covering the role matrix.
type allowAuthorizer struct{}

func (allowAuthorizer) CanAccessWorkspace(context.Context, string, string) (bool, error) {
	return true, nil
}

func (allowAuthorizer) CanManageWorkspace(context.Context, string, string) (bool, error) {
	return true, nil
}

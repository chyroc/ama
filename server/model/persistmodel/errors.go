// Sentinel errors.
//
// # Why in this package
//
// Store implementations must return them (the PG implementation uses them 46 times), and the
// implementations may not live in the core package (repository/dao). Keeping them in core would
// stop the implementations from moving out; that is exactly why this package exists.
//
// **core keeps aliases with the same names** (`ErrNotFound = persistmodel.ErrNotFound` in
// core/core.go), so `core.ErrNotFound` still works, and the 400-plus call sites and the handler
// mapping did not change a line. An alias is the same variable, not a copy, so `errors.Is` still
// holds across packages; a test pins this.
//
// ⚠️ **auth has its own ErrNotFound** (its text starts with "auth:"), which is **not the same value** as this
// one. Do not mix them up, and do not move it here in passing: it is that module's boundary error,
// and errors.Is not matching between them is correct.

package persistmodel

import "errors"

// Handlers use errors.Is to map these to HTTP status codes; see the registration code in httpd.
//
// Same approach as the feishu package: error types only express "which kind of problem"; the
// user-readable Chinese message is wrapped on by fmt.Errorf at each site, because the frontend
// shows message to the user directly, so the text must copy LocalApiClient's original exactly
// (e2e asserts the exact strings of some).
//
// ⚠️ **The "core:" prefix in the error text is kept on purpose.** That is what they were always
// called; changing it to "storemodel:" would make the same kind of error inconsistent in the logs
// over time, and people searching logs would not know it is the same thing.
var (
	// ErrInvalidArgument: invalid input. Maps to 400.
	ErrInvalidArgument = errors.New("core: 参数不合法")

	// ErrNotFound: the resource does not exist. Maps to 404.
	//
	// Five frontend getters return `T | null` (getWorkspaceByKey / getAgent / getSkill /
	// getFeishuApp / getRun), and HttpApiClient turns 404 into null. Note this conversion must
	// check the code in the envelope, not the bare status code; otherwise a wrongly written route
	// would also be treated as "resource does not exist" and silently become an empty state.
	ErrNotFound = errors.New("core: 资源不存在")

	// ErrConflict: conflicts with existing data, for example the workspace Key is taken. Maps to 409.
	ErrConflict = errors.New("core: 与现有数据冲突")

	// ErrPrecondition: a precondition is not met, for example removing the last owner. Maps to
	// 422.
	//
	// Kept separate from ErrConflict because the meaning differs: Conflict is "a different value
	// would work"; Precondition is "this operation itself is not allowed right now".
	ErrPrecondition = errors.New("core: 前置条件不满足")

	// ErrAccessDenied: the identity is valid but has no permission. Maps to 403.
	//
	// Kept apart from 401 (not logged in, produced by the auth package): 401 makes the frontend go
	// to the login page; 403 means "you are logged in, but this is not something you can do";
	// going to the login page would just make the user log in again and hit the same wall.
	//
	// Most authorization checks are done by authz.Guard at the HTTP boundary and never reach here.
	// This sentinel is for **the few cases Guard cannot cover**: GET /api/workspace-by-key, whose
	// workspace comes from a query parameter, and the whole product suggestion domain
	// (platform-level, with no workspace dimension at all; everything in the matrix is
	// ActionNone), so the permission check lands in Service.
	//
	// ⚠️ **It lives in this package because domain packages must return it.** service/suggestion
	// does all its permission checks in its own package (suggestionForRead / only platform staff
	// can change status), and it cannot import core; that is exactly the direction the split cuts.
	// **More importantly, do not errors.New another one with the same name in a domain package**:
	// handlers map 403 via `errors.Is(err, ErrAccessDenied)` (core/handler.go), and two different
	// values would make that check always false, so "no permission" silently becomes a 500, and the
	// frontend shows "the server hit a snag" instead of "this is not your suggestion".
	ErrAccessDenied = errors.New("core: 没有权限")
)

// Package resource is the reverse lookup table from a resource to its owning workspace, used by
// the authorization layer.
//
// # What this package solves
//
// Paths of single-resource endpoints **have no workspaceId**: /api/agents/{id}, /api/issues/{id}...
// That matches the existing shape of the frontend ApiClient (item-level methods pass only one
// opaque id). But authorization must know "which workspace this thing belongs to", or it cannot
// check membership, so it has to look up the id once.
//
// Those lookups used to sit in one switch in core/resource.go, 7 cases covering 4 resource domains.
// After splitting packages by domain that switch became a problem: **every domain moved out meant
// coming back to edit this same function**, and it was a file every domain had to pass through:
// two sessions each moving their own domain had diffs colliding on the same hunk.
//
// With a registry instead, each domain fills its own slot at wiring time, and nobody has to come
// back and edit someone else's code.
//
// # ⚠️ If the lookup fails it must return an error, never an empty string
//
// This is the only rule in this package that can cause a security incident, so it comes first.
//
// authz/guard.go **lets the request through directly** when the looked-up workspaceID is an
// **empty string** (that branch was meant for allowMissing list endpoints), and the membership
// check never runs. So "I don't know whose this is" is read as "no check needed": any logged-in
// user can modify or delete the resource, and **not a single log line is left**.
//
// So: unregistered kinds return an error (see the default in WorkspaceOf),
// platform-level resources return each domain's own sentinel error (such as ErrPlatformResource
// for built-in skills), and **no path is allowed to return "no error + empty string"**.
package resource

import (
	"context"
	"errors"
	"fmt"

	"github.com/chyroc/ama/server/model/persistmodel"
)

// Kind is a resource type whose owning workspace can be looked up.
//
// The values map one-to-one to authz.ResourceKind, but **each side defines its own copy on
// purpose**: authz uses core's Service as its Membership implementation, so sharing one enum would
// create a cycle. The wiring layer (cmd/amad) maps between them once; both enums are small, and
// the cost of duplication is much lower than a dependency cycle.
//
// ⚠️ **New values may only be appended.** It is iota; inserting in the middle silently changes the
// meaning of every later value, while the switch in the wiring layer still compiles. The symptom
// is "authorization looks up resource A as if it were resource B": the lookup always fails, or
// worse, it resolves to someone else's workspace.
type Kind int

const (
	KindAgent Kind = iota
	KindSkill
	KindRun
	KindSchedule
	// KindIssue / KindIssueComment / KindIssueAttachment are the three in the issue domain.
	//
	// ⚠️ **The attachment one is the only resource in this table that "gets requested by <img src>".**
	// It looks like a static asset, but it still needs the lookup: without it, anyone who has the
	// URL can view images from someone else's workspace, and that produces no error.
	KindIssue
	KindIssueComment
	KindIssueAttachment
)

// Resolver looks up which workspace a resource belongs to by id.
//
// When the resource does not exist it must return an error wrapping persistmodel.ErrNotFound; the
// caller (authz.Guard) then returns 404 instead of 403. "Resource does not exist" and "you have no
// permission" are two different HTTP status codes, and **mixing them up tells a lie**: answering
// "this agent was deleted" with 403 "access denied" makes the user think they need to ask an admin
// for permission, when the agent is long gone.
type Resolver func(ctx context.Context, id string) (string, error)

// Registry is the table of "resource type -> how to find its owning workspace".
//
// Each domain fills its own slot at wiring time, and it is read-only at runtime, so there is no
// lock: all Register calls happen in core.New (single-threaded), and WorkspaceOf is called
// concurrently only afterwards.
// ⚠️ **Do not Register on the request path**; that would turn this map into concurrent read/write.
type Registry struct {
	resolvers map[Kind]Resolver
}

// NewRegistry creates an empty table.
func NewRegistry() *Registry {
	return &Registry{resolvers: make(map[Kind]Resolver)}
}

// Register fills one slot.
//
// # Why duplicate registration panics instead of letting the later one override
//
// Overriding is silent: both domains think they own this kind, but the one that takes effect is
// whichever is wired later, and the wiring order is not written down anywhere. The symptom is
// "authorization sometimes checks the wrong table": change another domain's init order and the
// permission checks here change, with errors that do not point at the real cause.
//
// The panic happens at process startup, the same approach as an undeclared route in
// authz/matrix.go: wiring-time errors must blow up at wiring time, not wait until production and
// become a security incident.
func (r *Registry) Register(kind Kind, fn Resolver) {
	if fn == nil {
		panic(fmt.Sprintf("resource: kind=%d 注册了一个 nil 解析器", kind))
	}
	if _, dup := r.resolvers[kind]; dup {
		panic(fmt.Sprintf("resource: kind=%d 被注册了两次", kind))
	}
	r.resolvers[kind] = fn
}

// WorkspaceOf looks up which workspace a resource belongs to.
//
// An empty id is also treated as "not found": it means the route's parameter name is wrong (for
// example {runId} written as {id}), and it shows up right away in tests, which is better than a
// silent pass.
//
// ⚠️ **Unregistered kinds return an error, not an empty string**, for the reason in the package
// header: an empty string means "let it through". The same goes for a nil receiver: if wiring
// missed the whole table it must blow up, not become "no auth for the whole site".
func (r *Registry) WorkspaceOf(ctx context.Context, kind Kind, id string) (string, error) {
	if r == nil || r.resolvers == nil {
		return "", fmt.Errorf("%w: 资源反解表未装配", persistmodel.ErrInvalidArgument)
	}
	fn, ok := r.resolvers[kind]
	if !ok {
		return "", fmt.Errorf("%w: 未知的资源类型", persistmodel.ErrInvalidArgument)
	}
	return fn(ctx, id)
}

// NotFound turns a Store error into a sentinel error with a Chinese message, for reuse by each
// domain's Resolver.
//
// Only ErrNotFound is treated as "does not exist"; everything else (database hiccups, timeouts) is
// passed up unchanged. Fail-open is the wrong direction here, but **calling a database failure
// "does not exist" is even worse**: the frontend would turn it into null, the page would become a
// silent empty state, and the user would think the resource was deleted.
func NotFound(err error, what string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, persistmodel.ErrNotFound) {
		return fmt.Errorf("%w: %s不存在", persistmodel.ErrNotFound, what)
	}
	return err
}

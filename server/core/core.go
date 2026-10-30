// Package core is AMA's core business module: workspaces, members, Agents, skills, Ark
// credentials, and run history.
//
// # Split of work with the feishu package
//
// Feishu apps belong to server/feishu (the device-code self-registration flow); this package
// does not touch them. The only overlap is one ID string: Agent.FeishuAppIDs refers to
// feishu.App.ID.
//
// # Layers
//
//	handler (httpd registers routes) → Service (business invariants) → Store (storage)
//
// Business invariants live in Service, not Store, because most of them **span aggregates**:
// writing an agent must sync the back reference on Feishu apps, and deleting a workspace
// must cascade to six places. If they lived in Store, a future PG implementation would have
// to rewrite them as-is, which would make the Store interface the wrong seam. Store only
// stores and loads; it knows nothing about business rules.
//
// # The source of truth for the contract is the frontend
//
// This package implements the server side of the ApiClient interface in
// web/src/data/api.ts. LocalApiClient in that file (the localStorage version) is the
// **behavior baseline**: every side effect, every error message, and the presence or absence
// of every field is something the frontend pages really depend on. So many comments in this
// package explain "why we copy a behavior that looks odd". Copying it means the frontend
// needs zero changes when switching from LocalApiClient to HttpApiClient. Every place that
// departs from the baseline states the reason.
package core

import (
	"errors"

	"github.com/chyroc/ama/server/model/persistmodel"
)

// Sentinel errors. The handler maps them to HTTP status codes with errors.Is; see the
// registration code in httpd.
//
// **The real definitions are in model/persistmodel**; these are aliases with the same name.
// They moved because Store implementations must return them, and an implementation may not
// be in this package (repository/dao). Keeping them here would stop implementations from
// moving out.
//
// An alias is **the same variable**, not a copy, so `errors.Is(err, core.ErrNotFound)` and
// `errors.Is(err, persistmodel.ErrNotFound)` are fully equivalent, and the 400+ call sites
// did not change a line. For messages and meaning see storemodel/errors.go, which lists the
// status code each error maps to.
var (
	// ErrInvalidArgument means the input is invalid. Maps to 400.
	ErrInvalidArgument = persistmodel.ErrInvalidArgument

	// ErrNotFound means the resource does not exist. Maps to 404.
	ErrNotFound = persistmodel.ErrNotFound

	// ErrConflict means a conflict with existing data, e.g. the workspace Key is taken. Maps to 409.
	ErrConflict = persistmodel.ErrConflict

	// ErrPrecondition means a precondition is not met, e.g. removing the last owner. Maps to 422.
	ErrPrecondition = persistmodel.ErrPrecondition

	// ErrPlatformResource means the resource is **platform-level** and belongs to no workspace.
	//
	// Currently only platform built-in skills (core/skillbuiltin.go) produce it; WorkspaceOf
	// returns it.
	//
	// # Why a dedicated sentinel instead of returning an empty string
	//
	// authz/guard.go **lets the request through directly** when the resolved workspaceID is
	// an empty string (that branch is for allowMissing list endpoints), and the membership
	// check never runs. So "belongs to no workspace" would be treated as "no check needed",
	// and any logged-in user could edit or delete built-in skills, without a single log line.
	//
	// Returning ErrNotFound does not work either: a skill visible in the list would say
	// "does not exist" when opened.
	//
	// The wiring layer (cmd/amad) translates it into authz.ErrPlatformResource, and Guard
	// turns that into "allow reads, reject writes".
	ErrPlatformResource = errors.New("core: 平台级资源，不属于任何空间")

	// ErrAccessDenied means the identity is valid but lacks permission. Maps to 403.
	//
	// It is kept separate from 401 (not logged in, produced by the auth package): 401 makes
	// the frontend jump to the login page, while 403 means "you are logged in, but this is
	// not something you can do". Jumping to the login page would only make the user log in
	// again and hit the same wall.
	//
	// Most authorization checks are done by authz.Guard at the HTTP boundary and never reach
	// this package. This sentinel is for **the few cases Guard cannot cover**: for
	// GET /api/workspace-by-key the workspace comes from a query parameter, which a pattern
	// cannot express, so the membership check lives in the Service (see GetWorkspaceByKey).
	// The product suggestion domain is the same (platform-level, no workspace to resolve; all
	// permission checks are in service/suggestion).
	//
	// **The real definition is in model/persistmodel**; this is a same-name alias, for the
	// same reason as the batch above: service/suggestion must return it, and that package
	// cannot import core. An alias is **the same variable**, so the
	// `errors.Is(err, ErrAccessDenied)` checks in core (the handler's 403 mapping, the
	// assertions in feedback_test) did not change a line.
	ErrAccessDenied = persistmodel.ErrAccessDenied
)

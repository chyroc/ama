// Resource → owning workspace lookup: registration and forwarding on the core side.
//
// # The mechanism has moved to shared/gopkg/resource
//
// This used to be a 7-branch switch covering 4 resource domains. After splitting packages by
// domain, that switch became a problem: **every domain moved out meant coming back to edit this
// same function**, and it was a file every domain had to go through.
//
// Now the table is in shared/gopkg/resource, and each domain fills in its own entries:
//
//	the three entries of service/issue are filled by issuesvc.Service.RegisterResources (issue/comment/attachment)
//	the four entries agent/skill/run/schedule are still in registerResources below for now;
//	                when these domains move out, **they take their entry with them**; do not leave it here
//
// Why a failed lookup must return an error and not an empty string (empty string means allow) is
// written in the shared/gopkg/resource package header.
//
// # Why a failed lookup must tell "does not exist" apart
//
// "Resource does not exist" and "you have no permission" are two different HTTP status codes, and
// **mixing them tells lies**: six nullable getters in the frontend rely on code == "not_found" in
// the envelope to turn the response into null. If "this agent has been deleted" comes back as 403
// "no access", the user will think they need to ask an admin for permission, while that agent is
// long gone. So ErrNotFound states it explicitly, and authz translates it into 404.

package core

import (
	"context"

	"github.com/chyroc/ama/server/shared/gopkg/resource"
)

// ResourceKind is a resource type whose owning workspace can be looked up.
//
// **It is an alias of shared/gopkg/resource.Kind** (with `=`), not a new type: tests in cmd/amad
// and httpd write `core.KindAgent`, while each domain registers with `resource.KindAgent`. The two
// must be the same type, otherwise entries registered could not be fetched by the core-side enum.
// If it were a new type, the compiler would stop it (Register's argument type would not match),
// so it is not a silent failure.
type ResourceKind = resource.Kind

// The seven values are kept as-is, so the mappings in cmd/amad/main.go and httpd/authz_test.go
// need no change at all.
const (
	KindAgent           = resource.KindAgent
	KindSkill           = resource.KindSkill
	KindRun             = resource.KindRun
	KindSchedule        = resource.KindSchedule
	KindIssue           = resource.KindIssue
	KindIssueComment    = resource.KindIssueComment
	KindIssueAttachment = resource.KindIssueAttachment
)

// registerResources fills the lookup table. New calls it once at the end of wiring.
//
// Each domain **fills only its own entries**. The three issue entries are filled by service/issue
// itself (it has already moved out); the four below belong to domains not moved yet, which take
// their registration with them when they move.
func (s *Service) registerResources() {
	reg := resource.NewRegistry()

	reg.Register(resource.KindAgent, func(ctx context.Context, id string) (string, error) {
		a, err := s.store.GetAgent(ctx, id)
		if err != nil {
			return "", resource.NotFound(err, "Agent")
		}
		return a.WorkspaceID, nil
	})

	reg.Register(resource.KindSkill, func(ctx context.Context, id string) (string, error) {
		// ⚠️ **Platform built-in skills must be handled before the DB lookup, and must not return
		// an empty string.**
		//
		// They are not in the skills table (the source of truth is skillbuiltin.go). A DB lookup
		// would give ErrNotFound → Guard returns 404, so a skill visible in the list would say
		// "does not exist" when opened.
		//
		// Returning an empty string is worse: authz/guard.go **allows directly** when
		// workspaceID == "" (that branch was meant for list endpoints with allowMissing), and
		// RoleOf is never called. That would let any logged-in user PATCH / DELETE built-in
		// skills, without a single log line.
		//
		// So return ErrPlatformResource, which the authz side translates into "allow reads, deny
		// writes".
		if IsBuiltinSkill(id) {
			return "", ErrPlatformResource
		}
		sk, err := s.store.GetSkill(ctx, id)
		if err != nil {
			return "", resource.NotFound(err, "技能")
		}
		return sk.WorkspaceID, nil
	})

	reg.Register(resource.KindRun, func(ctx context.Context, id string) (string, error) {
		r, err := s.store.GetRun(ctx, id)
		if err != nil {
			return "", resource.NotFound(err, "运行")
		}
		return r.WorkspaceID, nil
	})

	// The entries of the issue domain and the schedule domain are filled by those domains
	// themselves; neither domain is in core anymore.
	s.issues.RegisterResources(reg)
	s.schedules.RegisterResources(reg)

	s.resources = reg
}

// WorkspaceOf looks up which workspace a resource belongs to.
//
// It stays on *Service because authz's Membership interface expects this method (wired at
// authzMembership in cmd/amad/main.go). It forwards to the registry.
func (s *Service) WorkspaceOf(ctx context.Context, kind ResourceKind, id string) (string, error) {
	return s.resources.WorkspaceOf(ctx, kind, id)
}

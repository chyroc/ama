// The Service of the Workspace domain: what it needs to do its work.
//
// # This package holds two things: workspaces and members
//
// Step 7 of the package split plan. Workspaces and members are **deliberately one package**: member rows hang under workspaces,
// and the permission check (RoleOf) has to look at both at once; splitting them would require one package to import the other (zero imports between domain packages
// is a hard constraint).
//
// # Four dependencies
//
//	Store        the two tables workspaces / workspace_members; see store.go
//	NextID       issues ids. The prefixes ws / m are decided by this package, the sequence is given by the wiring side
//	DefaultOwner the fallback owner used to create a workspace when the current logged-in user cannot be obtained
//	Logger       optional
//
// # ⚠️ The implementation of authz.Membership **has not changed**; it is still core.Service
//
// This step only moves business logic and does not touch wiring. The core side keeps the two forwarding methods RoleOf / WorkspaceOf,
// authzMembership in cmd/amad/main.go does not change a line, and the authz package has zero changes. The test is that
// after `go run ./cmd/gen-authz-capabilities` roleCapabilities.ts has zero diff.
//
// # ⚠️ Never add a cache or call an external API on the permission check path
//
// RoleOf **runs on every request** in authz/guard.go. The full argument is on RoleOf; read it before changing this package.
package workspace

import (
	"errors"
	"log/slog"
	"strings"

	"github.com/chyroc/ama/server/model/apimodel"
)

// Owner is the identity of an operator.
type Owner struct {
	Name  string
	Email string
}

// Config is what is needed to build a Service.
type Config struct {
	// Store is this domain's persistence. Required.
	Store Store

	// NextID issues a new id with a prefix. Required.
	//
	// ⚠️ **The prefix is an observable contract**: ws_ appears in URLs, logs, and user screenshots.
	// Changing the prefix means a whole new set of ids, and every old link breaks with zero errors.
	NextID func(prefix string) string

	// DefaultOwner is the fallback owner used to create a workspace **when the current logged-in user cannot be obtained**.
	//
	// Empty means defaultOwner. Production never reaches this fallback (every business endpoint is behind the auth middleware);
	// it only serves unit tests and local play with AMA_STORE=mem; see ownerFrom.
	DefaultOwner Owner

	// Logger is optional; empty means slog.Default().
	Logger *slog.Logger
}

// Service is the business logic of the workspace domain.
type Service struct {
	store  Store
	nextID func(prefix string) string
	owner  Owner
	logger *slog.Logger
}

// New creates a Service. Store and NextID are both required, the rest optional; see Config for why.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, errors.New("workspace: Store 不能为 nil")
	}
	if cfg.NextID == nil {
		return nil, errors.New("workspace: NextID 不能为 nil")
	}
	owner := cfg.DefaultOwner
	if strings.TrimSpace(owner.Email) == "" {
		owner = defaultOwner
	}
	return &Service{
		store:  cfg.Store,
		nextID: cfg.NextID,
		owner:  owner,
		logger: cfg.Logger,
	}, nil
}

func (s *Service) log() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

// roleRank ranks roles, **larger means more permission**. The same order as authz.levelOf.
//
// Why not reuse authz's: this package does not import authz (authz depends on RoleOf in the other direction).
// Only a total order is needed here, and hard-coding four values is cleaner than importing a package.
func roleRank(role apimodel.WorkspaceRole) int {
	switch role {
	case apimodel.RoleOwner:
		return 4
	case apimodel.RoleAdmin:
		return 3
	case apimodel.RoleMember:
		return 2
	case apimodel.RoleViewer:
		return 1
	default:
		return 0
	}
}

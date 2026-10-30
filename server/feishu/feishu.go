// Package feishu is the business layer of "connecting Feishu apps".
//
// # What this layer is responsible for
//
// shared/infra/lark only does protocol translation; this package does the business: auth, session intermediate state, dual-domain correction, idempotent registration,
// readiness diagnosis. They are separate because the protocol is defined by Feishu and the business by AMA, and they change for different reasons.
//
//	handler.go      HTTP boundary: decoding, error mapping
//	onboarding.go   Begin / Poll orchestration
//	readiness.go    readiness diagnosis
//	store.go        persistence contract + in-memory implementation
//
// # Usage
//
//	svc := feishu.New(feishu.Config{
//	    Store:      feishu.NewMemStore(),
//	    Authorizer: myAuthorizer,
//	})
//	ch, err := svc.Begin(ctx, feishu.BeginOption{
//	    ActorID: "u1", WorkspaceID: "ws1",
//	})
//	// give ch.VerificationURL to the user; the frontend polls at ch.PollIntervalSeconds:
//	res, err := svc.Poll(ctx, feishu.PollOption{
//	    ActorID: "u1", WorkspaceID: "ws1", SessionID: ch.SessionID,
//	})
package feishu

import (
	"context"
	"github.com/chyroc/ama/server/feishu/feishumodel"
	"net/http"
	"strings"
	"time"

	"github.com/chyroc/ama/server/shared/infra/lark"
)

const (
	// sessionTTL is how long a registration session lives.
	//
	// The device code itself is valid for an hour (measured expires_in=3600), but here it is deliberately cut to ten minutes:
	// the chance of a user leaving the authorization page open and wandering off for an hour is far lower than "switch browsers and click again", and the intermediate state
	// holds the AppSecret, so the shorter it lives the better.
	sessionTTL = feishumodel.SessionTTL

	// defaultPollInterval is the fallback interval when Feishu gives no interval.
	defaultPollInterval = 5

	// slowDownIncrement is how much the polling interval grows on slow_down.
	SlowDownIncrement = 5

	// registrationSource identifies who started the registration, for source statistics on the Feishu side.
	registrationSource = "ama"

	// defaultAppName is the app name shown on the authorization page.
	defaultAppName = "AMA 助手"
)

// Authorizer decides whether someone has permission on a workspace.
//
// It is an interface instead of an inline check so that the call order "authenticate first, then look up the session" **is already right now**:
// the real implementation is in server/authz (authz.FeishuAuthorizer, judged by workspace member role),
// wired up by cmd/amad. When empty it falls back to denyAllAuthorizer, which **always denies**.
type Authorizer interface {
	// CanAccessWorkspace decides whether actor can read the workspace.
	CanAccessWorkspace(ctx context.Context, actorID, workspaceID string) (bool, error)
	// CanManageWorkspace decides whether actor can change the workspace's config (connecting Feishu apps belongs here).
	CanManageWorkspace(ctx context.Context, actorID, workspaceID string) (bool, error)
}

// denyAllAuthorizer always denies; it is **the fallback when no real authorizer is wired up**.
//
// # Why the fallback is deny and not allow
//
// This used to be AllowAllAuthorizer: it allowed anything as long as actorID and workspaceID were non-empty,
// i.e. **any logged-in user could operate the Feishu apps of any workspace**. That was a placeholder from the era of "real authorization not done yet",
// and now that the authz package has landed, the direction of the fallback must be reversed.
//
// The reason is that the two outcomes of "forgot to wire it up" are not symmetric:
//
//	fallback allow → forgetting to wire it = authorization effectively does not exist, with **zero errors and all tests green**,
//	                 found only by digging through logs afterwards or after someone acted beyond their rights;
//	fallback deny  → forgetting to wire it = every Feishu API returns 403, found on the first click.
//
// Authorization belongs to the "you cannot see it when it does not work" kind, so better to be wrong on the noisy side.
type denyAllAuthorizer struct{}

func (denyAllAuthorizer) CanAccessWorkspace(context.Context, string, string) (bool, error) {
	return false, nil
}

func (denyAllAuthorizer) CanManageWorkspace(context.Context, string, string) (bool, error) {
	return false, nil
}

// Unbinder clears the dangling bindings on the agent side after an app is disconnected, and looks up bindings in reverse.
//
// It is an interface instead of a direct dependency on core, for the same reason as Conversations: the two modules are coupled only by ID strings.
// The implementation is *core.Service, wired up in cmd/amad.
type Unbinder interface {
	// UnbindFeishuApp removes a Feishu app from the bindings of every agent.
	UnbindFeishuApp(ctx context.Context, feishuAppID string) error
	// AgentsBoundToFeishuApp lists the agent IDs bound to a Feishu app.
	//
	// Used to compute App.BoundAgentIDs on the fly on the read path; that field is derived and not stored.
	AgentsBoundToFeishuApp(ctx context.Context, feishuAppID string) ([]string, error)
}

// Config is the configuration needed to build a Service.
type Config struct {
	// Store is the persistence implementation, required.
	Store Store

	// Authorizer is the auth implementation; **empty means always deny** (see denyAllAuthorizer).
	// The real implementation is authz.FeishuAuthorizer, wired up in cmd/amad.
	Authorizer Authorizer

	// Unbinder cleans up bindings on the agent side after DeleteApp.
	//
	// **May be nil** (many tests only care about the Feishu side and should not be forced to wire up core). When nil,
	// disconnecting an app leaves dangling feishuAppIds on agents, with the symptom "the app is gone, but the agent
	// detail page still shows 1 bound", so cmd/amad must pass it.
	Unbinder Unbinder

	// HTTPClient is passed through to shared/infra/lark; empty uses its default. Tests use it to inject a fake Feishu.
	HTTPClient *http.Client

	// AccountsURL / OpenURL override the Feishu addresses; empty derives them from the region. Tests use it to point at httptest.
	AccountsURL string
	OpenURL     string

	// Now overrides how the current time is obtained, for tests only. Empty uses time.Now.
	Now func() time.Time
}

// Service is the business entry point of this module. Safe for concurrent use.
type Service struct {
	store      Store
	auth       Authorizer
	cfg        Config
	now        func() time.Time
	inspection *inspectionCache
}

// StoreForTest returns the Store this Service uses.
//
// Only for cross-package tests: after the contract tests moved to repository/dao/feishudao they cannot reach private fields,
// and they need to bypass Service to see what was really stored (errors like "the API returned success, but nothing was actually written"
// can only be caught by querying the storage directly).
//
// An accessor is given instead of exporting the field: once exported, callers could **replace** it,
// which would breed errors like "the Store Service uses is not the Store I am asserting on".
func (s *Service) StoreForTest() Store { return s.store }

// SetAuthorizerForTest swaps the authorizer, to check "behavior without permission".
//
// Only for cross-package tests. The production authorizer is decided by Config at New and should not change after wiring,
// so it deliberately has the ForTest suffix, making it look suspicious at a glance in production code.
func (s *Service) SetAuthorizerForTest(a Authorizer) { s.auth = a }

// New builds a Service from the config.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, ErrInvalidArgument
	}
	auth := cfg.Authorizer
	if auth == nil {
		auth = denyAllAuthorizer{}
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		store:      cfg.Store,
		auth:       auth,
		cfg:        cfg,
		now:        now,
		inspection: newInspectionCache(now),
	}, nil
}

// larkClient builds a protocol client for the account domain.
func (s *Service) larkClient(region lark.Region) (*lark.Client, error) {
	return lark.New(lark.Config{
		Region:      region,
		AccountsURL: s.cfg.AccountsURL,
		OpenURL:     s.cfg.OpenURL,
		HTTPClient:  s.cfg.HTTPClient,
	})
}

// requireManage checks whether the caller can change the workspace's config.
func (s *Service) requireManage(ctx context.Context, actorID, workspaceID string) error {
	actorID = strings.TrimSpace(actorID)
	workspaceID = strings.TrimSpace(workspaceID)
	if actorID == "" || workspaceID == "" {
		return ErrInvalidArgument
	}
	ok, err := s.auth.CanManageWorkspace(ctx, actorID, workspaceID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrAccessDenied
	}
	return nil
}

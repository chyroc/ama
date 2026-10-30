// Service of the run history (Run) domain: what it needs to do its job.
//
// # Its dependencies
//
//	Store           the tables this domain can read; see store.go
//	Backfill        "go to Ark and fill in the tail events", **may be nil** (nil when Ark is not wired)
//	HasActiveStream "is someone subscribed to this run over SSE right now", **may be nil** (treated as no)
//	ConsoleRegion   region resolution for console links, **may be nil** (empty region)
//	Logger          optional
//
// Only the first one is required; see the next two sections for why.
//
// # ⚠️ The others are cross-domain edges, and they are the only places this domain "reaches into
// someone else's house"
//
// The real implementation of backfill is SyncRunEvents in core/chat.go, and **it belongs to the chat
// domain** (it builds the Ark client, stores events one by one, computes the status), which has not
// been split out yet. This domain is only downstream of it: reading run history triggers a backfill
// on the way.
//
// So it follows what the revision step did: **the consumer declares a narrow interface for its own
// needs, and the wiring layer fills it in**. What is declared here is function fields instead of an
// interface, by the same rule as injecting firstOwnerEmail in the arkcred step: this domain does not
// want "something that can backfill" but **the very same function as on the core side**. Declaring
// an interface would reduce "same source" to a comment; injecting the function itself is that
// constraint.
//
// The reverse edge does not exist: **this domain calls nothing in core**; the check is
// `go list -deps ./service/run | grep -c 'ama/server/core$'` = 0.
//
// # ⚠️ Backfill being nil means "no backfill", not an error
//
// Unlike Store, these are **optional**: with the local memory store or without Ark configured they
// are nil; run history can still be listed and opened, it just does not fetch tail events from Ark.
// Requiring them in New would mean "it cannot start without Ark", turning a graceful degradation
// into a hard dependency.
//
// This check used to be written as `s.runner != nil` (core/ark.go), with exactly the same meaning:
// no runner, no backfill. After the move the runner field stays in core, and this domain only looks
// at "did someone give me Backfill".
package run

import (
	"context"
	"errors"
	"log/slog"

	"github.com/chyroc/ama/server/model/apimodel"
)

// Config is what is needed to build a Service.
type Config struct {
	// Store is this domain's persistence. Required.
	Store Store

	// Backfill goes to Ark, fills in this run's tail events and stores them. **May be nil = no
	// backfill.**
	//
	// ⚠️ **The implementation must bring its own time limit**: this function is called
	// synchronously **inside a request the user is waiting on** (the GetRun / ListRunEvents paths),
	// and the Ark SDK's default timeout is 10 minutes. The limit, the cooldown and "failures do not
	// enter the cooldown" are all on the implementation side (core/runbackfill.go), with the full
	// argument there; this domain only calls it once when a backfill is due.
	//
	// When it returns an error this domain only logs a warn and returns the stored data as usual:
	// **a failed backfill should not stop the page from opening**; the stored data is complete, it
	// may just be missing a tail.
	Backfill func(ctx context.Context, runID string) error

	// HasActiveStream tells whether an active SSE connection is currently subscribed to this run.
	// **May be nil**; then it is always treated as "no" (so backfill runs as usual).
	//
	// No backfill while someone is subscribed: that stream is already receiving events live, and
	// fetching them from Ark again would be wasted.
	HasActiveStream func(runID string) bool

	// ConsoleRegion resolves the region of the Ark key actually in effect for (workspace, agent),
	// used to build console link URLs. **May be nil = always an empty region**, and the frontend does
	// not render the entry.
	//
	// A function instead of a value (same shape as the two function fields above): the real
	// implementation is arkConsoleRegion in core/arkconsole.go, with its three-level fallback
	// resolution; the wiring passes the method value in.
	ConsoleRegion func(ctx context.Context, workspaceID, agentID string) apimodel.ArkRegion

	// Logger is optional; slog.Default() if empty.
	//
	// ⚠️ All three failures in this domain are **degradation paths** (backfill failed, looking up
	// an agent failed, listing agents failed); they are only logged and never returned as errors, so
	// the log is the only trace of them. Pass a real one in.
	Logger *slog.Logger
}

// Service is the business logic of the run history domain.
type Service struct {
	store           Store
	backfill        func(ctx context.Context, runID string) error
	hasStream       func(runID string) bool
	consoleRegionFn func(ctx context.Context, workspaceID, agentID string) apimodel.ArkRegion
	logger          *slog.Logger
}

// New builds a Service.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, errors.New("run: Store 不能为 nil")
	}
	return &Service{
		store:           cfg.Store,
		backfill:        cfg.Backfill,
		hasStream:       cfg.HasActiveStream,
		consoleRegionFn: cfg.ConsoleRegion,
		logger:          cfg.Logger,
	}, nil
}

func (s *Service) log() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

// ⚠️ This domain has no RegisterResources; that is not an oversight.
//
// The table in shared/gopkg/resource answers "which workspace does this id belong to", for authz to
// check membership, and the run entry is filled by core/resource.go (one line `s.store.GetRun` is
// enough). This step does not move it: the other three entries of that table (agent / skill /
// schedule) are still there too, and moving only the run entry would scatter the registration
// logic over two packages, while it can now be read in full in one place.

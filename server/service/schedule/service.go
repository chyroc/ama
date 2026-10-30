// Service of the scheduled task (Schedule) domain: what it needs to do its job.
//
// # Four dependencies
//
//	Store       the schedules table itself, plus reading two tables of others (workspaces /
//	            agents); see store.go
//	NextID      hands out ids. The prefix sch is decided by this package, the sequence by the wiring
//	Revisions   record a version before saving, clear history after deleting. **This package declares
//	            this narrow interface**; see below
//	Logger      optional
//
// The first two have exactly the same shape as the issue / suggestion / revision domains.
//
// # ⚠️ The Revisions interface is declared by this package; that shape was settled in the previous step
//
// This domain is **upstream** of the revision domain (record a version on save, clear history on
// delete). An interface should be declared by **its user**; this is exactly where the commit that
// moved revision said it would land:
//
//	"when agent / skill / schedule are each split out, they declare
//	 interface { Record(...); Drop(...) } on their side, and cmd/amad wires it"
//
// So the two forwarding methods in core/revision.go (recordRevision / dropRevisions) are no longer
// needed by this domain: the core side fills *revisionsvc.Service straight into Config, which
// satisfies this interface. Those two forwarding methods must stay, because agent.go / skill still
// use them.
//
// ⚠️ **The reverse direction (writing a snapshot back into a task on rollback) does not use this
// interface**; it uses the revisionsvc.Entity entry, filled by the side that owns the entity (still
// core for now), which calls this domain's UpdateScheduleWithReason. Both directions exist, with
// different mechanisms; the full argument is in the file header of service/revision/entity.go.
//
// # ⚠️ The scheduler (core/scheduler.go) was **not** moved here
//
// This step only moves "the CRUD and validation of the schedule entity". The long-running scheduler
// drives the whole chat path (create an Ark Session, store a Run, subscribe to the event stream,
// deliver back to Feishu), i.e. it depends on the run / chat / ark domains, which have not been
// split out yet (that is for later steps). Forcing the move would mean first extracting an interface
// for each of them, and those interfaces would almost certainly be rewritten during the real split:
// writing them twice, plus one more review.
//
// So the scheduler stays in core, keeps using ClaimDueSchedules / SaveSchedule through core.Store
// (which embeds this package's Store), and calls this package's ComputeNextRun directly. **Its
// behavior did not change at all**: claim semantics, trigger values, delivery paths, failure
// degradation are all as before. Once run/chat are split out it moves into this package too, and
// then declares narrow interfaces for its own needs.
package schedule

import (
	"context"
	"errors"
	"log/slog"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/shared/gopkg/resource"
)

// Revisions is the revision capability this domain needs, **listing only the two it needs**.
//
// ⚠️ **Neither method returns an error, on purpose**: failing to record history should not make the
// user's save fail, and failing to clear history should not report "the delete already succeeded" as
// a failure. The full argument is on revisionsvc.Service.Record / Drop; getting the direction wrong
// gives no error, and tests stay green.
type Revisions interface {
	// Record stores a snapshot from before the change. payload is the result of
	// Snapshot(current entity).
	Record(
		ctx context.Context,
		kind apimodel.RevisionKind,
		workspaceID, entityID, summary string,
		payload any,
		reason apimodel.RevisionReason,
	)

	// Drop deletes all history of an entity. Called when a task is deleted.
	Drop(ctx context.Context, kind apimodel.RevisionKind, entityID string)
}

// Config is what is needed to build a Service.
type Config struct {
	// Store is this domain's persistence. Required.
	Store Store

	// NextID hands out a new id with a prefix. Required.
	//
	// ⚠️ **The prefix is an observable contract**: sch_ appears in URLs, logs and user screenshots.
	// Changing the prefix means a different set of ids, and all old links break with no error.
	NextID func(prefix string) string

	// Revisions is the revision domain. Required.
	//
	// **It may not be nil, and there is no "skip when nil" fallback**: that fallback gives no error;
	// the symptom is "the task was changed but there is no history to roll back", found only on the
	// day the user really wants to roll back.
	Revisions Revisions

	// Logger is optional; slog.Default() if empty.
	//
	// ⚠️ This domain has two **fail-open** degradations (when marking "agent deleted" on the list and
	// on a single item, a failed query means no mark), and the log is the only trace of those
	// failures. Pass a real logger in.
	Logger *slog.Logger
}

// Service is the business logic of the scheduled task domain.
type Service struct {
	store     Store
	nextID    func(prefix string) string
	revisions Revisions
	logger    *slog.Logger
}

// New builds a Service. All three dependencies are required; see each one's comment on Config.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, errors.New("schedule: Store 不能为 nil")
	}
	if cfg.NextID == nil {
		return nil, errors.New("schedule: NextID 不能为 nil")
	}
	if cfg.Revisions == nil {
		return nil, errors.New("schedule: Revisions 不能为 nil")
	}
	return &Service{
		store:     cfg.Store,
		nextID:    cfg.NextID,
		revisions: cfg.Revisions,
		logger:    cfg.Logger,
	}, nil
}

func (s *Service) log() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

// RegisterResources fills the scheduled task entry into the resolution table, so the authorization
// layer can look up the owning workspace by id.
//
// Each domain fills its own entry; this used to be one branch of a big switch in core/resource.go,
// and adding a kind of resource meant going back to change that shared file. Same as
// issue.RegisterResources.
func (s *Service) RegisterResources(reg *resource.Registry) {
	reg.Register(resource.KindSchedule, func(ctx context.Context, id string) (string, error) {
		sch, err := s.store.GetSchedule(ctx, id)
		if err != nil {
			return "", resource.NotFound(err, "定时任务")
		}
		return sch.WorkspaceID, nil
	})
}

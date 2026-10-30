// Service of the entity revision (Revision) domain: what it needs to do its job.
//
// # Four dependencies
//
//	Store       this domain's own entity_revisions table, see store.go
//	NextID      issues ids. The rev prefix is decided by this package, the sequence by the wiring side
//	Entities    one slot per entity: "how to look it up, how to write it back", see entity.go
//	Logger      optional
//
// The first two have exactly the same shape as in the issue / suggestion domains. The third is
// unique to this domain, because this domain is not a leaf: it is downstream of agent / skill /
// schedule; the full reasoning is in the file header of entity.go.
//
// # ⚠️ All three slots must be present; a missing one is an error in New
//
// This is one check this domain does beyond shared/gopkg/resource, and it can afford it because
// **this domain's key set is closed** (apimodel.ValidRevisionKind has exactly three values, and
// the frontend union type matches it exactly).
//
// Without the check, a missing slot does not crash: **that kind of entity silently loses its
// history feature**. The list is always empty, rollback says "unsupported entity type", while the
// agent side keeps recording snapshots. Data piles up in the database, the UI shows none of it,
// and nothing reports an error. Wiring-time errors should fail at wiring time, like the rule for
// undeclared routes in authz/matrix.go.
//
// ⚠️ Conversely, **registering an extra slot with an unknown kind is also an error**: it means the
// wiring side thinks it supports a fourth entity while the domain side has no such value at all.
// ValidRevisionKind keeps it out of the read endpoints, so that slot would never be called.
// Silent dead code is worse than saying so clearly at startup.
package revision

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/chyroc/ama/server/model/apimodel"
)

// Config is what is needed to build a Service.
type Config struct {
	// Store is this domain's persistence. Required.
	Store Store

	// NextID issues a new id with a prefix. Required.
	//
	// ⚠️ **The prefix is an observable contract**: rev_ shows up in URLs, logs and user
	// screenshots. Changing the prefix means a whole new set of ids; all old links break, with
	// zero errors.
	NextID func(prefix string) string

	// Entities is the slot of each of the three entities; required, and all three must be
	// present, for the reason in the file header.
	Entities map[apimodel.RevisionKind]Entity

	// Logger is optional; empty means slog.Default().
	//
	// ⚠️ In this domain **most failures are only logged, not returned** (Record / Drop are both
	// degrade paths), so the log is the only trace of those failures. Pass in a real logger.
	Logger *slog.Logger
}

// Service is the business logic of the entity revision domain.
type Service struct {
	store    Store
	nextID   func(prefix string) string
	entities map[apimodel.RevisionKind]Entity
	logger   *slog.Logger
}

// New builds a Service.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, errors.New("revision: Store 不能为 nil")
	}
	if cfg.NextID == nil {
		return nil, errors.New("revision: NextID 不能为 nil")
	}

	// All three slots must be present, and no unknown kind is allowed. Reason in the file header.
	entities := make(map[apimodel.RevisionKind]Entity, len(cfg.Entities))
	for kind, ent := range cfg.Entities {
		if !apimodel.ValidRevisionKind(kind) {
			return nil, fmt.Errorf("revision: 注册了一个认不得的实体类型 %q", kind)
		}
		// Both functions are required; missing one gives the half-filled "can read, cannot roll
		// back" state.
		if ent.Locate == nil {
			return nil, fmt.Errorf("revision: 实体类型 %q 的 Locate 不能为 nil", kind)
		}
		if ent.Rollback == nil {
			return nil, fmt.Errorf("revision: 实体类型 %q 的 Rollback 不能为 nil", kind)
		}
		entities[kind] = ent
	}
	for _, kind := range apimodel.AllRevisionKinds() {
		if _, ok := entities[kind]; !ok {
			return nil, fmt.Errorf("revision: 实体类型 %q 没有注册，它的历史功能会静默失效", kind)
		}
	}

	return &Service{
		store:    cfg.Store,
		nextID:   cfg.NextID,
		entities: entities,
		logger:   cfg.Logger,
	}, nil
}

func (s *Service) log() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

// Purge removes each entity's excess history and returns how many rows were deleted.
//
// ⚠️ **For the scheduled task; do not call it on the write path**: deleting synchronously on the
// write path adds latency to every save, and "delete everything after rank N" is itself a race
// across multiple instances. The full reasoning is in the file header of core/revisionpurge.go;
// the cleaner itself still lives there (it needs core.Leases for leader election).
func (s *Service) Purge(ctx context.Context, keep int) (int64, error) {
	return s.store.PurgeExcessRevisions(ctx, keep)
}

// ⚠️ **This domain has no RegisterResources, and that is not an oversight.**
//
// The shared/gopkg/resource table answers "which workspace does this id belong to", for authz to
// check membership. The nine revision routes **declare the host entity as their resource**
// (agentId / skillId / scheduleId), not the rev_ id: authorization checks "can you change this
// agent", and those three slots are registered by the agent / skill / schedule domains
// themselves. See those nine routes in authz/matrix.go.
//
// ⚠️ **Do not register a slot for rev_ just to "match the shape of issue"**: not only is it useless
// (no route uses revisionId as its authorization resource), it would also bring in the one rule
// from the resource package header that can cause a security incident: if resolution returns an
// empty string, authz/guard.go **lets the request through**.
//
// One more thing: the "does this snapshot really belong to entityID" check in GetRevision **must
// not be removed**. It fills exactly the slot that is missing here: authorization only checked
// the host entity, and the snapshot id took no part in it.

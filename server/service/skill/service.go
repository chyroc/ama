// Service of the skill domain: what it needs to do its work.
//
// # Six dependencies
//
//	Store       its own two tables, skills / skill_files, plus reading two tables owned by others,
//	            workspaces / agents (see the store.go header for why)
//	NextID      issues ids. The prefixes skill / file are decided by this package; the sequence is given by the wiring side
//	Revisions   record a version before saving, clear history after deleting. **This package declares this narrow interface**, like agent / schedule
//	Blobs       object storage for skill attachment bytes (may be nil; see below)
//	Ark         locating capability for the Ark console jump (may be nil; see below)
//	Logger      optional
//
// The first three have exactly the same shape as the agent domain; Blobs / Ark are specific to this
// domain, and the reasons for each are written on the interface declarations below.
//
// # The two "may be nil" ones are feature switches, not missing wiring
//
//	Blobs    nil  → reading / writing skills with attachments returns "no file storage wired"
//	Ark      nil  → the console jump returns an all-empty ref (the frontend does not render the entry)
//
// This copies the existing semantics on the core side; this step is only a move and changes none
// of them.
package skill

import (
	"context"
	"errors"
	"log/slog"

	"github.com/chyroc/ama/server/model/apimodel"
)

// Revisions is the revision history capability this domain needs, **listing only the two used**.
//
// ⚠️ **Neither method returns an error, on purpose**: failing to record history should not make the
// user's save fail, and failing to clear history should not report "the delete already succeeded"
// as a failure. The full argument is on revisionsvc.Service.Record / Drop; getting the direction
// wrong gives no error, and tests stay green.
//
// Exactly the same shape as agentsvc.Revisions / schedulesvc.Revisions. **Deliberately not one
// shared declaration**: zero imports between domain packages is a hard rule, and sharing would
// need one to import another.
type Revisions interface {
	// Record stores a snapshot before the change. Pass the result of SnapshotSkill(current entity) as payload.
	Record(
		ctx context.Context,
		kind apimodel.RevisionKind,
		workspaceID, entityID, summary string,
		payload any,
		reason apimodel.RevisionReason,
	)

	// Drop deletes all history of an entity. Called when a skill is deleted.
	Drop(ctx context.Context, kind apimodel.RevisionKind, entityID string)
}

// BlobStore is the object storage capability for skill attachment bytes.
//
// Same shape as core.BlobStore but **lists only the three methods this domain uses**: dependency
// inversion puts the interface at the consumer, and what is passed in at wiring time is the same
// object (a value of the core interface is assigned here directly; its method set is a superset, so
// Go's interface-to-interface assignment just works).
//
// **May be nil** (when not wired in unit tests); then reading/writing skills with attachments
// returns "no file storage wired" instead of silently degrading: the bytes only exist in BlobStore,
// and handing out zero-byte files is worse than an error (see the comments on putSkillFileBlobs /
// hydrateSkillFiles in blobs.go).
type BlobStore interface {
	// Put writes one object.
	Put(ctx context.Context, key string, content []byte, contentType string) error
	// Get reads all bytes of one object.
	Get(ctx context.Context, key string) ([]byte, error)
	// Bucket returns the bucket name, recorded when storing; see the comment on the files.bucket column.
	Bucket() string
}

// Config is what it takes to build a Service.
type Config struct {
	// Store is this domain's persistence. Required.
	Store Store

	// NextID issues a new id with a prefix. Required.
	//
	// ⚠️ **The prefix is an observable contract**: skill_ / file_ appear in URLs, logs and user
	// screenshots, and are asserted by e2e. Changing the prefix is changing to a different set of ids;
	// all old links break, with no error.
	NextID func(prefix string) string

	// Revisions records / clears revision history. Required.
	Revisions Revisions

	// Blobs returns the storage for skill attachment bytes (cmd/amad wires an S3-compatible object
	// store); **returns nil when not wired**, and then reading/writing skills with attachments returns
	// "no file storage wired".
	//
	// ⚠️ **A function, not a value**: on the core side, the blobs field is assigned after New in
	// tests (New first, then put in a fake store, is the existing pattern of a dozen cases there, and
	// the migrateblob cases even remove it midway and put it back). Passing a value would freeze nil
	// forever at wiring time, and all those cases would become wrong. Ask fresh every time; same reason
	// as the two adapters agentIntegrations / agentSyncs on the core side.
	Blobs func() BlobStore

	// Ark is the locating capability the Ark console jump needs; see the declaration in console.go.
	// **May be nil**: then an all-empty ref is returned, and the frontend does not render the entry.
	Ark Ark

	// Logger defaults to slog.Default() when empty.
	Logger *slog.Logger
}

// Service is the business logic of the skill domain: CRUD, upload parsing and saving, attachment
// read/write, and Fork (copying a built-in or another skill into a custom skill in this
// workspace).
type Service struct {
	store     Store
	nextID    func(prefix string) string
	revisions Revisions
	blobs     func() BlobStore
	ark       Ark
	log       *slog.Logger
}

// New builds a Service.
//
// ⚠️ The three required ones (Store / NextID / Revisions) are checked explicitly here with an
// error. Missing one would otherwise **panic on the request path** instead of at startup; missing
// wiring must blow up at startup and say which one. The "may be nil" ones are feature switches and
// are not checked; see the file header for why.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, errors.New("skill: Store 不能为 nil")
	}
	if cfg.NextID == nil {
		return nil, errors.New("skill: NextID 不能为 nil")
	}
	if cfg.Revisions == nil {
		return nil, errors.New("skill: Revisions 不能为 nil")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		store:     cfg.Store,
		nextID:    cfg.NextID,
		revisions: cfg.Revisions,
		blobs:     cfg.Blobs,
		ark:       cfg.Ark,
		log:       log,
	}, nil
}

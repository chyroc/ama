// State machine model for delivering agent progress.
//
// One conversation produces one ProgressDelivery, which delivers the agent's thinking to Feishu in real time and guarantees four things:
// no duplicate creation, no lost final state, no fights between instances, and continuing after a crash. The table structure and per-field reasons are in schema_pg.sql.
//
// # State transitions
//
//	queued ──claim────────────────> processing ──remote created──> active ──final state pushed──> completed
//	   ^                            │                              │
//	   └──lease expired/failed──────┘                              └──final push failed up to the limit──> failed
//	                                │
//	                                └──CoT create result uncertain──> create_uncertain
//
// create_uncertain is **a state that cannot be skipped**. When creating the CoT times out or returns 5xx we do not know whether it was created:
// retrying directly may leave two chains of thought in the chat, and giving up directly may leave one spinning forever. So it is its own state,
// and the "uncertain window" decides when to give up (see CreateUncertainWindow).
//
// # Why two revision numbers, applied / desired
//
// Pushing every time the agent emits a few characters would flood Feishu. So progress is collected into snapshots and only pushed when the content really changes:
// desired is the latest wanted revision, applied is the revision confirmed as pushed. When they are equal there is nothing to do.
// After a crash it continues from applied, and what was already pushed is not pushed again.

package feishu

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// Delivery method.
const (
	// TransportCOT is the main path: Feishu's dedicated CoT message type.
	TransportCOT = "message_cot"
	// TransportCard is the fallback path: an ordinary interactive card PATCHed in place.
	//
	// It degrades to this when CoT is **definitely rejected** (not when uncertain: it may already have been created).
	TransportCard = "card_summary"
)

// Delivery states.
const (
	// ProgressQueued is queued, waiting to be claimed.
	ProgressQueued = "queued"
	// ProgressProcessing means an instance holds the lease and is processing.
	ProgressProcessing = "processing"
	// ProgressActive means it has been created remotely and is being pushed incrementally.
	ProgressActive = "active"
	// ProgressCompleted means the final state was pushed and closed.
	ProgressCompleted = "completed"
	// ProgressFailed means the failure count reached the limit and it was given up.
	ProgressFailed = "failed"
	// ProgressCreateUncertain means the result of creating it remotely is uncertain; see the file header.
	ProgressCreateUncertain = "create_uncertain"
)

// Time parameters for leases and retries.
const (
	// ProgressLeaseTTL is how long one claim is held.
	//
	// Shorter than the long-connection lock (20s): pushing a batch of events takes seconds, and if the holder crashes it is better for someone else to take over sooner.
	ProgressLeaseTTL = 30 * time.Second

	// ProgressMaxFailures is how many consecutive failures before giving up.
	//
	// 5 times with exponential backoff is roughly "if it still fails after a bit over a minute, stop trying". Trying forever costs
	// a flooded log + wasted quota, and the user's message has already been stuck at "thinking" for a long time.
	ProgressMaxFailures = 5

	// CreateUncertainWindow is the observation window after "the result of creating remotely is uncertain".
	//
	// Within the window it is not recreated: recreating may leave a second chain of thought in the chat. If after the window there is still no progress
	// (no cot_id obtained), the delivery is considered failed: leaving one spinning message is better than an extra duplicate one,
	// and the user's next message starts a new turn.
	CreateUncertainWindow = 2 * time.Minute
)

// ProgressSnapshot is the complete look of the agent's progress at a moment.
//
// "Complete" is deliberate: delivery can crash at any step, and on retry it must be able to rebuild what to push from the snapshot alone,
// without relying on increments collected in memory.
//
// # How order is expressed
//
// What the agent really produces is **text and tool calls interleaved**: think a bit, call a tool, think more, call two more.
// Here there is only one string Thinking and one array Tools, which look like two parallel tracks.
// The key to putting them back into one timeline is ProgressToolCall.AfterTextLen:
// each tool call records "how many bytes Thinking had accumulated when I happened".
//
//	Thinking = "check logs, then" (16 bytes)
//	Tools    = [ {bash,  AfterTextLen: 10},      ← happened after "check logs"
//	             {read,  AfterTextLen: 16} ]     ← happened after ", then"
//
//	rendered as: check logs → [bash] → , then → [read]
//
// **Without it there is no order**: the snapshot only knows "which tools exist", not after which piece of text the 3rd tool happened,
// so rendering could only lay out all the text first and pile all the tools below it, which is exactly the
// problem this field fixes (tools squeezed into a lump at the bottom of the message). The expansion logic is in cotEventBuilder.Diff.
//
// # Why not change it into one items array
//
// "An ordered mixed sequence" is more direct, but then the two json tags Thinking / Tools would be gone, and the snapshot is
// **stored in jsonb with json tags** (feishu_progress_deliveries.applied_snapshot):
// renaming fields would make existing rows read as empty, so turns running during a rolling deploy would push everything already pushed
// all over again, and users would see two copies in the chain of thought. Adding optional fields is safe in both directions:
// old instances reading new rows get Thinking / Tools exactly as before (extra fields are ignored).
type ProgressSnapshot struct {
	// Thinking is the agent's thinking text so far. **It only grows, never shrinks**; Diff relies on this to compute increments.
	Thinking string `json:"thinking,omitempty"`
	// Tools are the tool calls that happened, in order. Their interleaved positions are in each item's AfterTextLen.
	Tools []ProgressToolCall `json:"tools,omitempty"`
	// Ordered marks that the AfterTextLen values in Tools can be trusted.
	//
	// **This field cannot be dropped, because 0 is ambiguous**: a tool that happened before any thinking text
	// has an AfterTextLen of 0 anyway; and existing rows have no such field at all, which also decodes to 0.
	// The two need exactly opposite rendering: the former "tool first", the latter "tools after all text".
	//
	// Guessing from 0 is bound to be wrong half the time, and the wrong half is **existing data**: turns running during a rolling deploy
	// would throw all tools to the very top of the message, messier than before the fix. So the writer declares it explicitly once.
	//
	// New snapshots are always true (set by snapshotBuilder); existing rows do not have it and decode to false,
	// so they take the old "text first, tools after" path, with no garbling and no re-push.
	Ordered bool `json:"ordered,omitempty"`
	// Terminal marks whether this is the last frame.
	Terminal bool `json:"terminal,omitempty"`
	// Outcome is the final result: done / error, only meaningful when Terminal is true.
	Outcome string `json:"outcome,omitempty"`
	// ErrorMessage is what the user sees when there is an error.
	ErrorMessage string `json:"errorMessage,omitempty"`
}

// ProgressToolCall is one tool call.
type ProgressToolCall struct {
	// ID is the tool call identifier; CoT events use it to tie start / args / end / result together.
	ID string `json:"id"`
	// Name is the tool name.
	Name string `json:"name"`
	// Args is the text form of the input.
	Args string `json:"args,omitempty"`
	// Result is the text form of the result, empty if it has not come back.
	Result string `json:"result,omitempty"`
	// Done marks whether this call has ended. **Closing at the end depends on it**: unfinished ones need
	// TOOL_CALL_END + TOOL_CALL_RESULT added, or the client keeps showing this tool as spinning.
	Done bool `json:"done,omitempty"`
	// AfterTextLen is the **byte length** Thinking had accumulated when this call happened,
	// i.e. where it sits on the timeline. See "How order is expressed" on ProgressSnapshot.
	//
	// **Only trustworthy when ProgressSnapshot.Ordered is true**: 0 could mean "happened before all text"
	// or "the existing row lacks this field", and the two render in opposite ways; see the comment on that field.
	//
	// Bytes instead of runes: Diff compares it with len(prevThinking), and using the same unit avoids converting back and forth;
	// split points are handled by chunkText on rune boundaries, so multi-byte characters are not broken.
	AfterTextLen int `json:"afterTextLen,omitempty"`
}

// Equal decides whether two snapshots render the same.
//
// **Compare before pushing**: if they are the same, skip this remote call. The agent's event stream has many frames that do not affect display
// (heartbeats, repeated statuses); without comparing, every frame becomes a PUT, and Feishu would rate limit.
func (s ProgressSnapshot) Equal(other ProgressSnapshot) bool {
	if s.Thinking != other.Thinking || s.Terminal != other.Terminal ||
		s.Outcome != other.Outcome || s.ErrorMessage != other.ErrorMessage ||
		s.Ordered != other.Ordered || len(s.Tools) != len(other.Tools) {
		return false
	}
	for i := range s.Tools {
		if s.Tools[i] != other.Tools[i] {
			return false
		}
	}
	return true
}

// ProgressDelivery is one delivery record.
type ProgressDelivery struct {
	// RunID is the Ark session ID, one per conversation, and also the primary key.
	RunID string
	// LarkAppID / ChatID are the delivery target.
	LarkAppID string
	ChatID    string
	// OriginMessageID is the user message that triggered this turn.
	OriginMessageID string
	// ReplyInThread is whether to open a topic.
	ReplyInThread bool

	// Transport is the delivery method.
	Transport string
	// Status is the current state.
	Status string
	// StatusReason is the reason for entering the final state.
	StatusReason string

	// RemoteCOTID / RemoteMessageID are the Feishu-side identifiers.
	RemoteCOTID     string
	RemoteMessageID string

	// DesiredRevision / AppliedRevision: see the file header.
	DesiredRevision int64
	AppliedRevision int64
	// AppliedSnapshot is the one confirmed as pushed.
	AppliedSnapshot *ProgressSnapshot
	// TerminalRequested / TerminalApplied: whether the final state has been requested / pushed.
	TerminalRequested bool
	TerminalApplied   bool

	// LeaseToken / LeaseUntil are the lease.
	LeaseToken string
	LeaseUntil *time.Time

	// CreateAttemptedAt is when creating remotely was attempted, used for the "uncertain window".
	CreateAttemptedAt *time.Time

	// Retry counts.
	AttemptCount            int
	ConsecutiveFailureCount int
	NextRetryAt             *time.Time
	LastError               string

	CreatedAt  time.Time
	UpdatedAt  time.Time
	FinishedAt *time.Time
}

// Finished decides whether this delivery has entered a final state.
func (d *ProgressDelivery) Finished() bool {
	switch d.Status {
	case ProgressCompleted, ProgressFailed, ProgressCreateUncertain:
		return true
	default:
		return false
	}
}

// ReadyForFinalReply decides whether the final reply can be sent.
//
// Ordering gate: the final answer waits for the CoT to close, otherwise the user sees the answer first and then the thinking, which is odd.
// But **it must not wait forever**: if it cannot get it, better a slightly wrong order than no reply (see the cap in inbound.go).
func (d *ProgressDelivery) ReadyForFinalReply() bool {
	return d == nil || d.TerminalApplied || d.Finished()
}

// Clone makes a deep copy, so callers cannot change the object inside Store.
func (d *ProgressDelivery) Clone() *ProgressDelivery {
	if d == nil {
		return nil
	}
	out := *d
	if d.AppliedSnapshot != nil {
		snap := *d.AppliedSnapshot
		snap.Tools = append([]ProgressToolCall(nil), d.AppliedSnapshot.Tools...)
		out.AppliedSnapshot = &snap
	}
	out.LeaseUntil = cloneTime(d.LeaseUntil)
	out.CreateAttemptedAt = cloneTime(d.CreateAttemptedAt)
	out.NextRetryAt = cloneTime(d.NextRetryAt)
	out.FinishedAt = cloneTime(d.FinishedAt)
	return &out
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := *t
	return &v
}

// ProgressStore is the persistence contract of the delivery state machine.
//
// It is a separate interface instead of being put into Store: its only caller is the progress worker, which has nothing to do with the
// CRUD of "connecting Feishu apps". At wiring time the same PGStore implements both.
type ProgressStore interface {
	// EnsureProgressDelivery creates a delivery record, or returns the existing one if it exists.
	//
	// Idempotent: calling repeatedly for the same run gives only one. Matches SQL on conflict do nothing + read back.
	EnsureProgressDelivery(ctx context.Context, d *ProgressDelivery) (*ProgressDelivery, error)

	// GetProgressDelivery gets by runID; returns ErrProgressNotFound if it does not exist.
	GetProgressDelivery(ctx context.Context, runID string) (*ProgressDelivery, error)

	// ClaimProgressDelivery grabs the lease of a delivery.
	//
	// If grabbed it returns (delivery, true, nil); if held by someone else or the retry time has not come it returns (nil, false, nil).
	// **That is not an error**; it is a normal result under concurrency.
	ClaimProgressDelivery(ctx context.Context, runID, token string, ttl time.Duration) (*ProgressDelivery, bool, error)

	// RenewProgressLease renews the lease. false means the lease no longer belongs to us,
	// and the caller must stop immediately, or it would push the same CoT together with the new holder.
	RenewProgressLease(ctx context.Context, runID, token string, ttl time.Duration) (bool, error)

	// SaveProgressDelivery writes back a delivery. **It requires holding the lease**: a token mismatch returns
	// ErrProgressLeaseLost, so an expired holder cannot overwrite the new holder's progress.
	SaveProgressDelivery(ctx context.Context, d *ProgressDelivery, token string) error

	// ReleaseProgressLease gives up the lease (without changing state), so someone else can take over right away.
	ReleaseProgressLease(ctx context.Context, runID, token string) error

	// ListClaimableProgressDeliveries lists deliveries that can be claimed, final states first.
	ListClaimableProgressDeliveries(ctx context.Context, limit int) ([]*ProgressDelivery, error)
}

// ---------------------------------------------------------------------------
// In-memory implementation
// ---------------------------------------------------------------------------

// memProgressStore is the in-memory implementation of ProgressStore, for tests and development without a database.
//
// **Its semantics must match the PG version**, especially leases: if not, unit tests are all green in memory and it blows up with a database,
// which is exactly when this kind of race is hardest to debug.
type memProgressStore struct {
	mu   sync.Mutex
	rows map[string]*ProgressDelivery
	now  func() time.Time
}

// NewMemProgressStore creates an in-memory ProgressStore.
func NewMemProgressStore() ProgressStore {
	return &memProgressStore{rows: make(map[string]*ProgressDelivery), now: time.Now}
}

func (s *memProgressStore) EnsureProgressDelivery(_ context.Context, d *ProgressDelivery) (*ProgressDelivery, error) {
	if d == nil || strings.TrimSpace(d.RunID) == "" {
		return nil, ErrInvalidArgument
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.rows[d.RunID]; ok {
		return existing.Clone(), nil
	}
	now := s.now()
	stored := d.Clone()
	stored.Status = ProgressQueued
	stored.DesiredRevision = 1
	stored.AppliedRevision = 0
	stored.CreatedAt, stored.UpdatedAt = now, now
	s.rows[d.RunID] = stored
	return stored.Clone(), nil
}

func (s *memProgressStore) GetProgressDelivery(_ context.Context, runID string) (*ProgressDelivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.rows[strings.TrimSpace(runID)]
	if !ok {
		return nil, ErrProgressNotFound
	}
	return d.Clone(), nil
}

func (s *memProgressStore) ClaimProgressDelivery(
	_ context.Context, runID, token string, ttl time.Duration,
) (*ProgressDelivery, bool, error) {
	runID, token = strings.TrimSpace(runID), strings.TrimSpace(token)
	if runID == "" || token == "" {
		return nil, false, ErrInvalidArgument
	}
	if ttl <= 0 {
		ttl = ProgressLeaseTTL
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.rows[runID]
	if !ok || d.Finished() {
		return nil, false, nil
	}
	now := s.now()
	// The same three gates as the PG version: lease, retry time, state.
	if d.LeaseUntil != nil && d.LeaseUntil.After(now) && d.LeaseToken != token {
		return nil, false, nil
	}
	if d.NextRetryAt != nil && d.NextRetryAt.After(now) {
		return nil, false, nil
	}
	if d.Status != ProgressActive {
		d.Status = ProgressProcessing
	}
	until := now.Add(ttl)
	d.LeaseToken, d.LeaseUntil = token, &until
	d.AttemptCount++
	d.UpdatedAt = now
	return d.Clone(), true, nil
}

func (s *memProgressStore) RenewProgressLease(
	_ context.Context, runID, token string, ttl time.Duration,
) (bool, error) {
	runID, token = strings.TrimSpace(runID), strings.TrimSpace(token)
	if runID == "" || token == "" {
		return false, ErrInvalidArgument
	}
	if ttl <= 0 {
		ttl = ProgressLeaseTTL
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.rows[runID]
	if !ok || d.LeaseToken != token {
		return false, nil
	}
	until := s.now().Add(ttl)
	d.LeaseUntil = &until
	return true, nil
}

func (s *memProgressStore) SaveProgressDelivery(_ context.Context, d *ProgressDelivery, token string) error {
	if d == nil || strings.TrimSpace(d.RunID) == "" || strings.TrimSpace(token) == "" {
		return ErrInvalidArgument
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.rows[d.RunID]
	if !ok {
		return ErrProgressNotFound
	}
	// Same as the PG version: a token mismatch means the lease changed hands, so it must not overwrite.
	if existing.LeaseToken != token {
		return ErrProgressLeaseLost
	}
	now := s.now()
	stored := d.Clone()
	stored.UpdatedAt = now
	if stored.Finished() {
		if stored.FinishedAt == nil {
			stored.FinishedAt = &now
		}
		stored.LeaseToken, stored.LeaseUntil = "", nil
	} else {
		stored.LeaseToken = token
	}
	s.rows[d.RunID] = stored
	return nil
}

func (s *memProgressStore) ReleaseProgressLease(_ context.Context, runID, token string) error {
	runID, token = strings.TrimSpace(runID), strings.TrimSpace(token)
	if runID == "" || token == "" {
		return ErrInvalidArgument
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.rows[runID]; ok && d.LeaseToken == token {
		d.LeaseToken, d.LeaseUntil = "", nil
	}
	return nil
}

func (s *memProgressStore) ListClaimableProgressDeliveries(_ context.Context, limit int) ([]*ProgressDelivery, error) {
	if limit <= 0 {
		limit = 32
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	out := make([]*ProgressDelivery, 0, limit)
	for _, d := range s.rows {
		if d.Finished() {
			continue
		}
		if d.LeaseUntil != nil && d.LeaseUntil.After(now) {
			continue
		}
		if d.NextRetryAt != nil && d.NextRetryAt.After(now) {
			continue
		}
		out = append(out, d.Clone())
	}
	// Final states first, then by creation time. Map iteration is unordered, so it must be sorted.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].TerminalRequested != out[j].TerminalRequested {
			return out[i].TerminalRequested
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

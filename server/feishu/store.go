// Store interface and in-memory implementation.
//
// # Why the in-memory implementation came first
//
// A registration session is a ten-minute intermediate state anyway, so in-memory semantics are right. Connected apps should be stored, but the repo
// had no usable PG instance at the time, and writing an implementation that cannot connect would only pretend to be done. So the interface was defined clearly first,
// the in-memory implementation ran the whole path end to end, and a later pgStore implements the same interface without touching the business layer.
//
// The interface is deliberately designed as "SQL later": CreateApp returns ErrAppExists on a duplicate appID, matching
// a unique index conflict; read methods always return copies, to avoid behavior that differs between an in-memory implementation that can be mutated and a SQL one that cannot.
//
// **A process restart loses connected apps**: this is an explicit cost of the current implementation, not a bug.
//
// The PG implementation was later really added, but **not in this package**; it is in repository/dao/feishudao.
// Having the two implementations live apart is a choice made after weighing the cost; the reasons are in the in-file comment of the "in-memory implementation" section below.

package feishu

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store is the persistence contract of this module.
type Store interface {
	// PutSession writes or overwrites a registration session.
	PutSession(ctx context.Context, id string, session Session, ttl time.Duration) error
	// GetSession reads a registration session; returns ErrSessionLost if it does not exist or has expired.
	GetSession(ctx context.Context, id string) (Session, error)
	// DeleteSession deletes a registration session; not existing is not an error.
	DeleteSession(ctx context.Context, id string) error

	// CreateApp stores a newly connected app.
	// Returns ErrAppExists when the appID already exists, matching a SQL unique index conflict.
	CreateApp(ctx context.Context, app *App, secret string) (*App, error)
	// GetApp gets by AMA-side ID; returns ErrAppNotFound if it does not exist.
	GetApp(ctx context.Context, id string) (*App, error)
	// GetAppByAppID gets by Feishu appID; returns ErrAppNotFound if it does not exist.
	GetAppByAppID(ctx context.Context, appID string) (*App, error)
	// ListApps lists the apps in a workspace, in ascending order of connection time.
	ListApps(ctx context.Context, workspaceID string) ([]*App, error)
	// ListAllApps lists the apps of all workspaces, in ascending order of connection time.
	//
	// For the long-connection supervisor: when the process starts it must open a connection for **every** connected app,
	// which spans workspaces and has no notion of a "current user", so it does not go through the auth path of ListApps.
	// Should only be called inside the process; do not expose it over HTTP.
	ListAllApps(ctx context.Context) ([]*App, error)
	// SaveApp overwrites an existing app; returns ErrAppNotFound if it does not exist.
	SaveApp(ctx context.Context, app *App) (*App, error)
	// AppSecret gets the app secret, only for internal checks in this package.
	AppSecret(ctx context.Context, appID string) (string, error)
	// SetAppSecret refreshes the app secret, used to switch to a new secret on reconnect.
	SetAppSecret(ctx context.Context, appID, secret string) error
	// DeleteApp disconnects an app and clears its secret too.
	DeleteApp(ctx context.Context, id string) error

	// ClaimInboundMessage claims an inbound message, for **dedup**.
	//
	// true means this is a new message and should be handled; false means it was already handled or is being handled, and the caller
	// should just ack. Feishu delivers the same message more than once (network jitter, we returned an error, we were slow),
	// and without dedup the same sentence runs the agent twice and the user gets two cards.
	//
	// The semantics match SQL "insert ... on conflict do nothing" plus a status check:
	// pending / processed count as duplicates; **failed is let through for retry**: the one that died last time
	// should be tried again when Feishu redelivers it.
	//
	// The four anchors in rec (ChatType / ThreadID / RootID / ParentID) are only written **when the claim really succeeds**;
	// on duplicate delivery the existing values are not overwritten. The input of the first delivery is the one the key was computed from,
	// and if a redelivered frame has a different shape, overwriting would destroy the evidence for diagnosis.
	ClaimInboundMessage(ctx context.Context, rec InboundMessageRecord) (bool, error)

	// MarkInboundMessage updates the handling status of an inbound message; not existing is not an error.
	MarkInboundMessage(ctx context.Context, messageID, status string) error

	// GetInboundMessage gets the dedup record by Feishu message ID; returns ErrInboundMessageNotFound if it does not exist.
	//
	// **It exists only for debugging**; the business path has no caller (dedup relies on the return value of ClaimInboundMessage,
	// not on look-up-then-decide, which is TOCTOU). It was added because without a read path for those four anchor columns,
	// "were they stored correctly" could only be checked by querying SQL by hand, and the two Store implementations could not run the same assertions.
	GetInboundMessage(ctx context.Context, messageID string) (InboundMessageRecord, error)

	// PutInflightTurn updates the progress (card ID + content pushed so far) of a "turn in progress".
	//
	// This is the **only basis** for continuing after a service restart: turn state used to live only in process memory,
	// was lost on restart, and the card stayed at "thinking" forever. See the comment on this table in schema_pg.sql.
	//
	// **Use TryBeginTurn to start a turn, not this**: this method is a high-frequency write on the card update path,
	// it only updates the progress columns and does no mutual exclusion check. Using it to start a turn bypasses the "one turn per session at a time"
	// guard, and two concurrent turns would garble the same card.
	PutInflightTurn(ctx context.Context, rec InflightTurnRecord) error

	// TryBeginTurn starts a conversation turn; **only one turn at a time is allowed on the same session**.
	//
	// # Why it is needed: session reuse turned "one turn, one session" into "many turns, one session"
	//
	// With sessions reused by scope, two people @-mentioning the bot in a chat at the same time land on the same Ark session.
	// That session's event stream is **shared**: both cardUpdaters subscribe to it and both write the
	// agent.message they receive onto their own card, so both cards show the answers to both questions mixed together
	// (agentText carries no turn ID and cannot tell which sentence was meant for whom).
	//
	// It would also break this table itself: its primary key is run_id, so with two concurrent turns the later write overwrites the earlier one,
	// and DeleteInflightTurn from the turn that finishes first deletes the other turn's recovery record too.
	//
	// So turns on the same session must be serial. **The table itself is used as the mutex**: its lifetime is exactly
	// one turn (written before running, deleted after), so no extra lock is needed.
	//
	// false means a turn is already running on this session, and the caller should come back after it finishes.
	//
	// # Stale records must be preemptible
	//
	// The instance of the holder may have died before deleting the record (OOM, kill -9). The startup scan cleans it up,
	// but that waits for the next restart, and until then this chat stays blocked. So staleAfter is passed in:
	// a record older than that is taken over directly. It is equivalent to a lease with a TTL, where the TTL is the upper limit of a turn.
	//
	// Liveness uses max(StartedAt, UpdatedAt): a long task's StartedAt will be old,
	// but as long as it keeps updating the card it is not dead and should not be taken over.
	TryBeginTurn(ctx context.Context, rec InflightTurnRecord, staleAfter time.Duration) (bool, error)

	// ClaimInflightTurn claims an unfinished turn, for **mutual exclusion across instances**.
	//
	// true means the claim succeeded and this instance should take over; false means another instance already took over,
	// and the caller should skip it. Every instance runs the startup scan, and without this CAS two instances would write
	// to the same card at once, and the user would see the content jump back and forth.
	//
	// The semantics match SQL "update ... where status = 'inflight'" plus a RowsAffected check,
	// the same as ClaimInboundMessage. A successful claim does retry_count +1.
	ClaimInflightTurn(ctx context.Context, runID string) (bool, error)

	// ListInflightTurns lists all unfinished turns in ascending started_at order (the first interrupted is recovered first).
	//
	// For the startup scan: when the process starts it must find **all** unfinished turns **left by the previous process**, which span workspaces
	// and apps and have no notion of a "current user". Like ListAllApps, it should only be called inside the process;
	// do not expose it over HTTP.
	ListInflightTurns(ctx context.Context) ([]InflightTurnRecord, error)

	// DeleteInflightTurn deletes a turn record; not existing is not an error (idempotent).
	//
	// **It must be called when a turn ends, and there must be only one call site.** If cleanup only hung off the failure path,
	// successful turns would leave their records in the store, and the next restart would answer already-answered questions
	// again. So the call site is gathered into the defer of runAndReply.
	DeleteInflightTurn(ctx context.Context, runID string) error

	// -- Chat (group) registry ---------------------------------------------------

	// RememberChat records "the bot has seen messages in this chat".
	//
	// **It exists because scheduled tasks need to pick a delivery target.** Before this the system had no
	// "workspace/agent → chat" mapping: App has no chat field, and
	// the chat_id of InboundMessageRecord is a per-message_id dedup record, not a standing registry.
	//
	// Having users type oc_xxx by hand is the easiest implementation, but they would have to dig up the ID in Feishu, and a wrong value gives no error;
	// the report just never gets sent. So instead **we note it down as inbound messages pass through**: zero input from the user,
	// and naturally only chats "the bot is really in" are listed.
	//
	// Idempotent: calling repeatedly with the same (larkAppID, chatID) only updates lastSeenAt / name.
	RememberChat(ctx context.Context, rec ChatRecord) error

	// ListChats lists the chats recorded in a workspace, most recently seen first.
	//
	// When the workspace does not exist or no chat has been recorded yet, it returns an empty slice, **not an error**: that is the normal state of "the bot
	// has not been @-mentioned in a chat yet", and the UI should show a hint instead of an error.
	ListChats(ctx context.Context, workspaceID string) ([]ChatRecord, error)

	// SetChatName only changes the name of a chat; other fields (especially lastSeenAt) stay untouched.
	//
	// **A separate method instead of reusing RememberChat**: filling chat names happens on the "list chats" read path,
	// where the lastSeenAt at hand is an old value just read, and a whole-row upsert with it would overwrite the new time just written
	// by a concurrent inbound message. A single UPDATE that only changes the name does not have this problem.
	//
	// When the chat does not exist, it **neither errors nor creates one** (the chat list should only be produced by inbound messages). When name is empty it does nothing:
	// failing to get a name should not erase an existing name.
	SetChatName(ctx context.Context, larkAppID, chatID, name string) error

	// -- Session scope → Ark session mapping ---------------------------------------

	// GetSessionScope looks up the Ark session currently bound to a scope; returns ErrScopeNotBound if never bound.
	//
	// Located by (larkAppID, scopeKey, agentID): switching to another agent should start a new conversation,
	// and asking the new agent with the old agent's session history would give the wrong context.
	GetSessionScope(ctx context.Context, larkAppID, scopeKey, agentID string) (SessionScopeRecord, error)

	// BindSessionScope binds a scope to an Ark session, **safely under concurrency**.
	//
	// # The meaning of the return value is the whole point of this method
	//
	// What it returns is **the record that finally took effect**, not necessarily the one passed in. When two instances receive two messages from the same
	// chat at once, each creates its own Ark session and both come to bind: only one can win,
	// and the loser gets the winner's RunID. The caller must continue with the return value,
	// not with the one it just created; otherwise the two messages in the chat each hang on a session, and the context is still broken.
	//
	// The semantics match SQL `insert ... on conflict do nothing` followed by a read back: let the database decide,
	// instead of "look up first, then create" (that is TOCTOU: both instances find nothing and both create).
	//
	// bound is false when this attempt lost and what is returned is the one someone else already bound. The caller uses it to decide
	// whether to drop the session it just created.
	BindSessionScope(ctx context.Context, rec SessionScopeRecord) (out SessionScopeRecord, bound bool, err error)

	// RebindSessionScope rebinds a scope to a new Ark session, used when the old session is no longer valid.
	//
	// It does a CAS with expectRunID: it only changes the binding if it is still that invalid session. Without this comparison,
	// two instances finding the same session invalid at once would each create a new one, the later write would overwrite the earlier one,
	// and the session the earlier instance is sending messages into would no longer be this scope's session.
	//
	// false means someone else already changed it, and the caller should Get again for the latest one.
	RebindSessionScope(ctx context.Context, rec SessionScopeRecord, expectRunID string) (bool, error)

	// TouchSessionScope updates a scope's last active time and turn count; not existing is not an error.
	//
	// Pure ops information (debugging "how long has this chat been quiet", "how many turns were there"); failure should not affect the conversation.
	TouchSessionScope(ctx context.Context, larkAppID, scopeKey, agentID string, at time.Time) error
}

// SessionScopeRecord is the binding "one session scope ↔ one Ark session".
//
// The reason it exists is in the scope.go file header: without this table every message is a new session, and the bot has no memory.
//
// # Why it must be stored instead of kept in an in-process map
//
// Production amad runs multiple instances, and two adjacent messages are usually not in the same process (see the repo's CLAUDE.md).
// An in-process map behaves as "sometimes it remembers, sometimes not", depending on which instance the load balancer
// sent the message to, and it always degrades during rolling deploys.
//
// # Why a deterministic ID cannot replace this table
//
// Because RunID **is the session id issued by Ark** (see CreateConversation in core/chat.go),
// and we have no say in what it looks like. To "compute the session ID by a rule" we would have to invent an AMA-side ID and store a mapping,
// which needs one more table. So what is stored here is "scope → the ID Ark gave".
type SessionScopeRecord struct {
	// LarkAppID is the Feishu app that received the message (cli_xxx). Part of the primary key.
	LarkAppID string
	// ScopeKey is the scope key (see sessionScope.Key in scope.go). Part of the primary key.
	ScopeKey string
	// AgentID is the AMA agent this conversation is handed to. Part of the primary key.
	//
	// It is in the primary key on purpose: when an app switches to another agent, that should be a new conversation;
	// letting the new agent take over the old agent's session history would give mismatched context.
	AgentID string
	// RunID is the bound Ark session ID (i.e. apimodel.Run.ID).
	RunID string
	// WorkspaceID is the owning AMA workspace, stored redundantly for debugging.
	WorkspaceID string
	// ScopeType is thread / chat, for debugging (it can also be seen from ScopeKey).
	ScopeType string
	// ChatID is the chat the message is in, for debugging.
	ChatID string
	// ThreadID is the topic anchor, empty for non-topic scopes. For debugging.
	ThreadID string
	// CreatedAt is when it was first bound.
	CreatedAt time.Time
	// LastUsedAt is the last time a message landed in this scope.
	LastUsedAt time.Time
	// TurnCount is the number of turns already run in this scope.
	TurnCount int
}

// Clone returns a copy. The record only has value types, so returning it directly is enough; this method is kept so that
// "reads return copies" in the in-memory implementation is explicit at the call site.
func (r SessionScopeRecord) Clone() SessionScopeRecord { return r }

// ChatRecord is one chat (group) record.
type ChatRecord struct {
	// LarkAppID is the Feishu app that received the message (cli_xxx). Together with ChatID it forms the primary key.
	LarkAppID string
	// ChatID is the Feishu chat ID (oc_xxx); delivery only uses this.
	ChatID string
	// WorkspaceID is the owning AMA workspace, looked up from LarkAppID and stored along with it.
	//
	// Stored redundantly instead of joined every time: ListChats lists by workspace, and storing it makes that one index scan.
	// If the app were moved to another workspace this would go stale, but that does not happen today (App.WorkspaceID
	// never changes after creation); if it ever needs support, update this column along with it.
	WorkspaceID string
	// ChatType is p2p / group / topic_group.
	ChatType string
	// Name is the chat name, **may be empty**.
	//
	// Inbound messages **do not carry the chat name** (see lark.InboundMessage), so it is empty when first recorded.
	// It is filled in later by SetChatName: once when the inbound prefetch gets the chat name, and on the spot when listing chats
	// for chats without a name (see chatnames.go). If neither gets it, the UI falls back to showing ChatID, **which is not an error**.
	Name string
	// LastSeenAt is the last time a message was received in this chat, used for sorting (most recent first).
	LastSeenAt time.Time
}

// InflightTurnRecord is a "turn in progress" record.
//
// The rule for choosing fields is "the minimal set needed to rebuild one card update after a restart"; the reason for each column is in
// schema_pg.sql. **No token is stored**: it expires in two hours, so only the LarkAppID needed to exchange a new one is stored.
type InflightTurnRecord struct {
	// RunID is the Ark session id, the **primary key**.
	RunID string
	// LarkAppID is the owning Feishu app, used to rebuild the tenant_access_token.
	LarkAppID string
	// ChatID is the chat the message is in.
	ChatID string
	// AgentID is which agent this turn runs.
	//
	// **Only serves the recovery path**: after a restart resumeLive only has this record, and needs it to ask
	// "should this agent show the chain of thought" (see startTurnProgress in turnprogress.go).
	//
	// ⚠️ **May be an empty string**: agent_id is a column added later, and existing rows have no value. An empty string cannot be looked up to an agent,
	// and the upper layer fails open to "show", the same as the behavior before this switch was added.
	AgentID string
	// SrcMessageID is the user's message; it is both the reply anchor for creating the card and the key for clearing pending.
	SrcMessageID string
	// CardMessageID is the message ID of the reply card (PATCH target). **Empty means no card yet.**
	CardMessageID string
	// ReplyInThread records whether the card was created as a topic reply; a rebuild must match the original.
	ReplyInThread bool
	// LastText is the content already pushed to the card, used as the starting point on recovery.
	LastText string
	// Status is the claim state: inflight / resuming. Only serves mutual exclusion across instances.
	Status string
	// RetryCount is the number of times it has been automatically resumed; +1 on each claim.
	RetryCount int
	// StartedAt is the start of the turn, used to judge "stuck too long" (limit 1 hour, see inflightMaxAge in resume.go).
	StartedAt time.Time
	// UpdatedAt is the last write time, for debugging.
	UpdatedAt time.Time
}

// InboundMessageRecord is the dedup record of an inbound message.
type InboundMessageRecord struct {
	// LarkAppID is the Feishu app that received this message.
	LarkAppID string
	// MessageID is the Feishu message ID (om_xxx), the **dedup key**.
	MessageID string
	// ChatID is the chat the message is in.
	ChatID string
	// Status is the handling status: pending / processed / failed.
	Status string
	// CreatedAt is the claim time.
	CreatedAt time.Time

	// The four below are an **input snapshot** of scopeForMessage, purely for later diagnosis; they take no part in dedup
	// or any business decision. See the comments on those four columns in schema_pg.sql.
	//
	// Why store them: "continuing in a topic opened a new session" has two root causes: Feishu gave no anchor at all,
	// or it gave one but we computed the key wrong. The fixes for the two have nothing in common, and **the store alone cannot tell them apart**:
	// feishu_session_scopes only keeps the computed scope_key and none of the inputs before computing.
	// When a problem really happened, we could only ask the user to reproduce it again, and before the logs rolled over.
	//
	// An empty string means **Feishu did not give this field**, which is exactly the signal to judge, so the PG side stores NULL
	// rather than an empty string. Do not give them default ''; that would mix "not given" and "given as an empty string" into one thing.

	// ChatType is p2p / group / topic_group, which decides which branch scopeForMessage takes.
	ChatType string
	// ThreadID is the topic ID (omt_xxx). **One of the anchor candidates for group chats; direct chats deliberately do not use it**
	// (its namespace does not match message_id; see the scope.go file header). It is stored so we can see
	// shapes like "Feishu gave thread_id but not root_id".
	ThreadID string
	// RootID is the root message of the reply chain (om_xxx). **Reusing a session inside a direct-chat topic relies on it**:
	// it equals the root message's message_id, so the computed key is exactly the same as the root message's.
	RootID string
	// ParentID is the previous message (om_xxx), the third fallback anchor.
	ParentID string
}

// ---------------------------------------------------------------------------
// In-memory implementation
// ---------------------------------------------------------------------------

// # Why memStore stays in feishu while PGStore is in repository/dao/feishudao
//
// The MemStore of the neighboring core package has already moved into repository/dao/coremem, so people naturally ask
// "when will feishu follow". It was measured: **this one does not move**, and the reasons and numbers follow.
//
// First, the precedent that misleads people. The coremem move cost **0 exports / 0 package moves**, but that 0 is not
// a property of "moving MemStore"; it is a property of **where it lands**: core's MemStore does not depend on any
// core symbol (it only uses model/apimodel + model/persistmodel), so it could land in a sibling package that **does not
// import core**, and the cycle never exists in the first place.
//
// feishu has no such path. memStore's method signatures directly carry this package's App / Session /
// ChatRecord / InboundMessageRecord / InflightTurnRecord / SessionScopeRecord,
// and it must implement this package's Store and reuse SortApps / TrimScopeKeys, so feishumem **must
// import feishu**. And this package has 5 test files in `package feishu` that construct NewMemStore directly
// (apps_binding / resume / inbound / inbound_trio / inbound_turnlock), so
// `feishu(test) -> feishumem -> feishu` forms a cycle.
// ⚠️ This cycle **passes go build**; only go vet / go test report it
// (`import cycle not allowed in test`). "It compiles" is not evidence of feasibility; this was verified with a minimal reproduction.
//
// The only way out is turning those tests into the external test package feishu_test, and following the transitive closure of the **shared fixtures**,
// the cost is (measured, not estimated):
//
//   - **All 12 test files must move**; not one can stay. The fixtures connect them into one connected component:
//     testActor is in authorizer_fixture_test.go, fakeSender / agentFrame /
//     fakeConversations are in inbound_test.go, frame is in cotevents_test.go,
//     fakeCOT is in transport_test.go, newBindingService is in apps_binding_test.go.
//     Even trigger_test.go, which only tests pure functions, is pulled in because of testActor.
//   - **32 unexported production identifiers must be exported**, spread over 9 files. Most of them are internal
//     implementation details of Service, not zero-dependency pure functions like storemodel, so they **cannot be pushed down, only exported**:
//     progressDriver (a stateful driver), newCOTTransport / newCardTransport /
//     renderProgressCard (inside the delivery path), newCOTEventBuilder / newSnapshotBuilder,
//     sendFinalReply / waitForProgressTerminal / markDone (need this package's interfaces such as MessageSender,
//     ProgressStore, Reactor), and memStore's own state constants
//     (the 6 of inboundPending / inflightOpen / turnEnded).
//     The few that look like pure functions (countMarkdownTables / splitReplyByTables /
//     segmentIdempotencyKey / truncateForCOT / shouldTrigger) cannot be pushed down either: they
//     have call sites in this package's production code, so pushing them into a leaf package still needs exporting, only somewhere else,
//     plus one more package and a ring of imports.
//
// The other path, pushing those 6 types down into a leaf package so feishumem need not import feishu, is more expensive:
// the App type alone has 72 in-package + 35 out-of-package references, and App is also an HTTP contract entity
// (17 fields with json tags, whose source of truth is the frontend LocalApiClient); moving it means touching the contract.
//
// And the benefit side is empty: **the contract discipline is already satisfied**. eachStore in feishudao/store_pg_test.go
// gets the in-memory implementation through the **public API** feishu.NewMemStore() and runs once against each implementation
// (53 for mem + 53 for pg). The worry that "implementations living apart forces contract tests across packages" did not in fact happen.
//
// Conclusion: 32 new exports + 12 test files moved, in exchange for the look of "both implementations in the same directory",
// is not worth it: **those 32 exports would trade public API for directory shape**.
// When it is worth reopening: if one day the tests in `package feishu` no longer construct
// memStore directly, or that set of shared fixtures is split up (the connected component shrinks to single digits), the cost comes down.
//
// A reusable test (same as the coremem one): **the cost of "move or not" is a property of where it lands, not of the thing
// being moved**; and whether the landing place can import the original package depends on "**whose internal tests construct it**":
// feishudao can import feishu (no test in package feishu constructs PGStore),
// feishumem cannot (5 do). Two packages in the same directory can have opposite answers.

// memStore is the in-memory implementation of Store. Safe for concurrent use.
type memStore struct {
	mu       sync.Mutex
	sessions map[string]sessionEntry
	apps     map[string]*App                 // keyed by AMA-side ID
	byAppID  map[string]string               // Feishu appID → AMA-side ID, simulates the unique index
	secrets  map[string]string               // Feishu appID → AppSecret
	inbound  map[string]InboundMessageRecord // Feishu messageID → dedup record
	inflight map[string]InflightTurnRecord   // Ark session id → unfinished turn record
	chats    map[string]ChatRecord           // "feishuAppID\x00chatID" → chat record
	scopes   map[string]SessionScopeRecord   // scopeRowKey → scope binding
	now      func() time.Time
}

type sessionEntry struct {
	session   Session
	expiresAt time.Time
}

// NewMemStore creates an in-memory Store.
//
// Expired sessions are not cleaned by a background goroutine; expired entries are cleared along the way on each read and write (see sweepLocked).
// One less goroutine to stop means one less source of leaks; the total number of sessions is small anyway.
// NewMemStoreWithClock creates an in-memory Store, but with a clock given by the caller.
//
// It exists only for cross-package tests: the session expiry contract can only be checked by "moving time forward", and the PG implementation is in
// repository/dao/feishudao, which cannot reach memStore's private fields.
//
// A constructor is given instead of exporting all 10 fields: once fields are exported anyone can build a
// **half-initialized** memStore (miss any map and it panics on the first write),
// while this guarantees every map is created, and the caller can only swap the clock.
//
// When clock is nil it falls back to time.Now.
func NewMemStoreWithClock(clock func() time.Time) Store {
	s := NewMemStore().(*memStore)
	if clock != nil {
		s.now = clock
	}
	return s
}

func NewMemStore() Store {
	return &memStore{
		sessions: make(map[string]sessionEntry),
		apps:     make(map[string]*App),
		byAppID:  make(map[string]string),
		secrets:  make(map[string]string),
		inbound:  make(map[string]InboundMessageRecord),
		inflight: make(map[string]InflightTurnRecord),
		chats:    make(map[string]ChatRecord),
		scopes:   make(map[string]SessionScopeRecord),
		now:      time.Now,
	}
}

// sweepLocked clears expired sessions. The caller must hold the lock.
func (s *memStore) sweepLocked() {
	now := s.now()
	for id, entry := range s.sessions {
		if now.After(entry.expiresAt) {
			delete(s.sessions, id)
		}
	}
}

func (s *memStore) PutSession(ctx context.Context, id string, session Session, ttl time.Duration) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return ErrInvalidArgument
	}
	if ttl <= 0 {
		ttl = sessionTTL
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	s.sessions[id] = sessionEntry{session: session, expiresAt: s.now().Add(ttl)}
	return nil
}

func (s *memStore) GetSession(ctx context.Context, id string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	entry, ok := s.sessions[strings.TrimSpace(id)]
	if !ok {
		return Session{}, ErrSessionLost
	}
	return entry.session, nil
}

func (s *memStore) DeleteSession(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, strings.TrimSpace(id))
	return nil
}

func (s *memStore) CreateApp(ctx context.Context, app *App, secret string) (*App, error) {
	if app == nil || strings.TrimSpace(app.AppID) == "" {
		return nil, ErrInvalidArgument
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byAppID[app.AppID]; ok {
		return nil, ErrAppExists
	}
	stored := app.Clone()
	s.apps[stored.ID] = stored
	s.byAppID[stored.AppID] = stored.ID
	if secret != "" {
		s.secrets[stored.AppID] = secret
	}
	return stored.Clone(), nil
}

func (s *memStore) GetApp(ctx context.Context, id string) (*App, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	app, ok := s.apps[strings.TrimSpace(id)]
	if !ok {
		return nil, ErrAppNotFound
	}
	return app.Clone(), nil
}

func (s *memStore) GetAppByAppID(ctx context.Context, appID string) (*App, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byAppID[strings.TrimSpace(appID)]
	if !ok {
		return nil, ErrAppNotFound
	}
	return s.apps[id].Clone(), nil
}

func (s *memStore) ListApps(ctx context.Context, workspaceID string) ([]*App, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*App, 0, len(s.apps))
	for _, app := range s.apps {
		if app.WorkspaceID == workspaceID {
			out = append(out, app.Clone())
		}
	}
	// Ascending by connection time, with ID as a tie-breaker so the order is stable (map iteration itself is unordered).
	SortApps(out)
	return out, nil
}

// ListAllApps lists the apps of all workspaces. See the notes on the Store interface.
func (s *memStore) ListAllApps(_ context.Context) ([]*App, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*App, 0, len(s.apps))
	for _, app := range s.apps {
		out = append(out, app.Clone())
	}
	// Map iteration itself is unordered; sort to keep it stable.
	SortApps(out)
	return out, nil
}

func (s *memStore) SaveApp(ctx context.Context, app *App) (*App, error) {
	if app == nil {
		return nil, ErrInvalidArgument
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.apps[app.ID]; !ok {
		return nil, ErrAppNotFound
	}
	stored := app.Clone()
	s.apps[stored.ID] = stored
	return stored.Clone(), nil
}

func (s *memStore) AppSecret(ctx context.Context, appID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	secret, ok := s.secrets[strings.TrimSpace(appID)]
	if !ok {
		return "", ErrAppNotFound
	}
	return secret, nil
}

func (s *memStore) SetAppSecret(ctx context.Context, appID, secret string) error {
	appID = strings.TrimSpace(appID)
	if appID == "" || secret == "" {
		return ErrInvalidArgument
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byAppID[appID]; !ok {
		return ErrAppNotFound
	}
	s.secrets[appID] = secret
	return nil
}

func (s *memStore) DeleteApp(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	app, ok := s.apps[id]
	if !ok {
		return ErrAppNotFound
	}
	delete(s.apps, id)
	delete(s.byAppID, app.AppID)
	delete(s.secrets, app.AppID)
	return nil
}

// ClaimInboundMessage claims an inbound message. See the notes on the Store interface.
func (s *memStore) ClaimInboundMessage(_ context.Context, rec InboundMessageRecord) (bool, error) {
	id := strings.TrimSpace(rec.MessageID)
	if id == "" {
		return false, ErrInvalidArgument
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.inbound[id]; ok {
		// failed is let through for retry: the one that died last time should be tried again when Feishu redelivers it.
		// pending / processed are both duplicate deliveries; block them.
		if existing.Status != inboundFailed {
			return false, nil
		}
		// The let-through-for-retry branch overwrites the whole row, so **the anchors from the first delivery must be kept**: that is the one the key
		// was computed from, and only that one counts as evidence for diagnosis. A redelivered frame may not carry the same fields,
		// and letting it overwrite would replace the evidence with something of unknown origin. Matches coalesce on the PG side.
		rec.ChatType = firstNonEmpty(existing.ChatType, rec.ChatType)
		rec.ThreadID = firstNonEmpty(existing.ThreadID, rec.ThreadID)
		rec.RootID = firstNonEmpty(existing.RootID, rec.RootID)
		rec.ParentID = firstNonEmpty(existing.ParentID, rec.ParentID)
	}
	rec.MessageID = id
	s.inbound[id] = rec
	return true, nil
}

// firstNonEmpty returns the first non-empty string, or an empty string if all are empty.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// GetInboundMessage gets a dedup record. See the notes on the Store interface.
func (s *memStore) GetInboundMessage(
	_ context.Context, messageID string,
) (InboundMessageRecord, error) {
	id := strings.TrimSpace(messageID)
	if id == "" {
		return InboundMessageRecord{}, ErrInvalidArgument
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.inbound[id]
	if !ok {
		return InboundMessageRecord{}, ErrInboundMessageNotFound
	}
	return rec, nil
}

// MarkInboundMessage updates the handling status; not existing is not an error.
func (s *memStore) MarkInboundMessage(_ context.Context, messageID, status string) error {
	id := strings.TrimSpace(messageID)
	if id == "" {
		return ErrInvalidArgument
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.inbound[id]
	if !ok {
		return nil
	}
	rec.Status = status
	s.inbound[id] = rec
	return nil
}

// PutInflightTurn writes or overwrites an unfinished turn. See the notes on the Store interface.
//
// **The claim state and resume count are owned exclusively by ClaimInflightTurn**, and are not touched here (matching the PG implementation's
// on conflict, which only updates three columns). The write path calls this method repeatedly while updating the card; if it also reset status to
// inflight, one card update would let another instance claim it again: two writers on the same card, content jumping back and forth.
func (s *memStore) PutInflightTurn(_ context.Context, rec InflightTurnRecord) error {
	id := strings.TrimSpace(rec.RunID)
	if id == "" {
		return ErrInvalidArgument
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec.RunID = id
	if rec.Status == "" {
		rec.Status = inflightOpen
	}
	if existing, ok := s.inflight[id]; ok {
		rec.Status = existing.Status
		rec.RetryCount = existing.RetryCount
		// StartedAt also keeps the value from the first write: the timeout check needs "when did this turn start",
		// and if every card update refreshed it, it would never get old and the timeout branch would never fire.
		rec.StartedAt = existing.StartedAt
	}
	rec.UpdatedAt = s.now()
	s.inflight[id] = rec
	return nil
}

// ClaimInflightTurn claims an unfinished turn. See the notes on the Store interface.
func (s *memStore) ClaimInflightTurn(_ context.Context, runID string) (bool, error) {
	id := strings.TrimSpace(runID)
	if id == "" {
		return false, ErrInvalidArgument
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.inflight[id]
	if !ok {
		return false, nil
	}
	// Only inflight can be claimed. Already resuming means another instance (or this instance's previous scan)
	// has taken over, and claiming again would have two goroutines writing to the same card.
	if rec.Status != inflightOpen {
		return false, nil
	}
	rec.Status = inflightResuming
	rec.RetryCount++
	rec.UpdatedAt = s.now()
	s.inflight[id] = rec
	return true, nil
}

// ListInflightTurns lists all unfinished turns in ascending StartedAt order. See the notes on the Store interface.
func (s *memStore) ListInflightTurns(_ context.Context) ([]InflightTurnRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]InflightTurnRecord, 0, len(s.inflight))
	for _, rec := range s.inflight {
		out = append(out, rec)
	}
	sortInflightTurns(out)
	return out, nil
}

// DeleteInflightTurn deletes a turn record; not existing is not an error.
func (s *memStore) DeleteInflightTurn(_ context.Context, runID string) error {
	id := strings.TrimSpace(runID)
	if id == "" {
		return ErrInvalidArgument
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inflight, id)
	return nil
}

// chatKey joins "Feishu app + chat" into a map key.
//
// Separated by \x00: IDs are opaque strings and could in theory contain anything, but never NUL.
// With a separator like ":" two parts joined could collide with another pair (same as bindingKey in core).
func chatKey(larkAppID, chatID string) string {
	return larkAppID + "\x00" + chatID
}

func (s *memStore) RememberChat(_ context.Context, rec ChatRecord) error {
	app := strings.TrimSpace(rec.LarkAppID)
	chat := strings.TrimSpace(rec.ChatID)
	if app == "" || chat == "" {
		return ErrInvalidArgument
	}
	rec.LarkAppID = app
	rec.ChatID = chat
	if rec.LastSeenAt.IsZero() {
		rec.LastSeenAt = s.now()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	key := chatKey(app, chat)
	// **Do not let an empty name overwrite an existing name.** Inbound messages usually carry no chat name; if one ever does
	// (or an API lookup is added later), every later message without a name would erase it,
	// showing up as "the chat name appears and disappears".
	if existing, ok := s.chats[key]; ok && strings.TrimSpace(rec.Name) == "" {
		rec.Name = existing.Name
	}
	s.chats[key] = rec
	return nil
}

func (s *memStore) SetChatName(_ context.Context, larkAppID, chatID, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := chatKey(strings.TrimSpace(larkAppID), strings.TrimSpace(chatID))
	if rec, ok := s.chats[key]; ok {
		rec.Name = name
		s.chats[key] = rec
	}
	return nil
}

func (s *memStore) ListChats(_ context.Context, workspaceID string) ([]ChatRecord, error) {
	ws := strings.TrimSpace(workspaceID)
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]ChatRecord, 0)
	for _, rec := range s.chats {
		if rec.WorkspaceID == ws {
			out = append(out, rec)
		}
	}
	// Most recently seen first. Ties are broken by chatID so the order is stable:
	// map iteration is random, and without a tie-breaker the same data would come back in a different order each time (the PG side sorts the same way).
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].LastSeenAt.Equal(out[j].LastSeenAt) {
			return out[i].LastSeenAt.After(out[j].LastSeenAt)
		}
		return out[i].ChatID < out[j].ChatID
	})
	return out, nil
}

// TryBeginTurn starts a conversation turn; only one turn at a time on the same session. See the notes on the Store interface.
//
// This table is used as the mutex: if there is no record, write one (acquired); if there is one, take it over only when it is stale.
func (s *memStore) TryBeginTurn(
	_ context.Context, rec InflightTurnRecord, staleAfter time.Duration,
) (bool, error) {
	id := strings.TrimSpace(rec.RunID)
	if id == "" {
		return false, ErrInvalidArgument
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	rec.RunID = id
	now := s.now()
	if existing, ok := s.inflight[id]; ok {
		// A turn is already running. Only take it over when it is staler than the upper limit of a turn; otherwise normal concurrency
		// should be blocked and the caller should wait for the previous turn to finish.
		//
		// Liveness uses max(StartedAt, UpdatedAt): a long task's StartedAt will be old,
		// but as long as it keeps updating the card (UpdatedAt moves) it is not dead.
		fresh := existing.StartedAt
		if existing.UpdatedAt.After(fresh) {
			fresh = existing.UpdatedAt
		}
		if staleAfter <= 0 || now.Sub(fresh) < staleAfter {
			return false, nil
		}
	}
	if rec.Status == "" {
		rec.Status = inflightOpen
	}
	if rec.StartedAt.IsZero() {
		rec.StartedAt = now
	}
	// A takeover counts as a new turn: reset the resume count to zero, or a chat that talks for a long time would be misjudged as "resumed too many times"
	// and refused recovery.
	rec.RetryCount = 0
	rec.UpdatedAt = now
	s.inflight[id] = rec
	return true, nil
}

// scopeRowKey joins the three-part primary key of a scope binding into a map key.
//
// Separated by \x00, like chatKey: IDs are opaque strings but never contain NUL; with a separator like ":"
// two different triples could join into the same key.
func scopeRowKey(larkAppID, scopeKey, agentID string) string {
	return larkAppID + "\x00" + scopeKey + "\x00" + agentID
}

// trimScopeKeys trims the three-part primary key, returning false if any part is empty.
func TrimScopeKeys(larkAppID, scopeKey, agentID string) (string, string, string, bool) {
	app := strings.TrimSpace(larkAppID)
	scope := strings.TrimSpace(scopeKey)
	agent := strings.TrimSpace(agentID)
	return app, scope, agent, app != "" && scope != "" && agent != ""
}

// GetSessionScope looks up a scope's binding. See the notes on the Store interface.
func (s *memStore) GetSessionScope(
	_ context.Context, larkAppID, scopeKey, agentID string,
) (SessionScopeRecord, error) {
	app, scope, agent, ok := TrimScopeKeys(larkAppID, scopeKey, agentID)
	if !ok {
		return SessionScopeRecord{}, ErrInvalidArgument
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, found := s.scopes[scopeRowKey(app, scope, agent)]
	if !found {
		return SessionScopeRecord{}, ErrScopeNotBound
	}
	return rec.Clone(), nil
}

// BindSessionScope binds a scope; if already bound, it returns the existing one. See the notes on the Store interface.
//
// Matches PG's `on conflict do nothing` + read back: **first come first served; latecomers get the winner's record**.
// The whole check happens under one lock, so if two goroutines come in at once only one write succeeds.
func (s *memStore) BindSessionScope(
	_ context.Context, rec SessionScopeRecord,
) (SessionScopeRecord, bool, error) {
	app, scope, agent, ok := TrimScopeKeys(rec.LarkAppID, rec.ScopeKey, rec.AgentID)
	if !ok {
		return SessionScopeRecord{}, false, ErrInvalidArgument
	}
	if strings.TrimSpace(rec.RunID) == "" {
		return SessionScopeRecord{}, false, ErrInvalidArgument
	}
	rec.LarkAppID, rec.ScopeKey, rec.AgentID = app, scope, agent

	s.mu.Lock()
	defer s.mu.Unlock()
	key := scopeRowKey(app, scope, agent)
	if existing, found := s.scopes[key]; found {
		// Someone else bound first. Return the existing one; the caller must use it instead of the one it just created.
		return existing.Clone(), false, nil
	}
	now := s.now()
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = now
	}
	if rec.LastUsedAt.IsZero() {
		rec.LastUsedAt = now
	}
	if rec.TurnCount == 0 {
		rec.TurnCount = 1
	}
	s.scopes[key] = rec
	return rec.Clone(), true, nil
}

// RebindSessionScope rebinds with CAS. See the notes on the Store interface.
func (s *memStore) RebindSessionScope(
	_ context.Context, rec SessionScopeRecord, expectRunID string,
) (bool, error) {
	app, scope, agent, ok := TrimScopeKeys(rec.LarkAppID, rec.ScopeKey, rec.AgentID)
	if !ok {
		return false, ErrInvalidArgument
	}
	expect := strings.TrimSpace(expectRunID)
	if strings.TrimSpace(rec.RunID) == "" || expect == "" {
		return false, ErrInvalidArgument
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	key := scopeRowKey(app, scope, agent)
	existing, found := s.scopes[key]
	if !found {
		return false, nil
	}
	// CAS: the current binding must still be that invalid session, otherwise someone else already changed it.
	if existing.RunID != expect {
		return false, nil
	}
	existing.RunID = strings.TrimSpace(rec.RunID)
	existing.LastUsedAt = s.now()
	// A new session was swapped in, so the turn count restarts from this turn: it records "how many turns on the current session".
	existing.TurnCount = 1
	s.scopes[key] = existing
	return true, nil
}

// TouchSessionScope updates the last active time and turn count; not existing is not an error.
func (s *memStore) TouchSessionScope(
	_ context.Context, larkAppID, scopeKey, agentID string, at time.Time,
) error {
	app, scope, agent, ok := TrimScopeKeys(larkAppID, scopeKey, agentID)
	if !ok {
		return ErrInvalidArgument
	}
	if at.IsZero() {
		at = s.now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := scopeRowKey(app, scope, agent)
	rec, found := s.scopes[key]
	if !found {
		return nil
	}
	rec.LastUsedAt = at
	rec.TurnCount++
	s.scopes[key] = rec
	return nil
}

// sortInflightTurns sorts by StartedAt ascending, with RunID as a tie-breaker.
//
// Ascending is deliberate: the first interrupted is recovered first, so the causal order among turns matches the order users asked in.
// Insertion sort is enough: the number of unfinished turns at once is "the number of people chatting at the moment of restart", which is small.
func sortInflightTurns(recs []InflightTurnRecord) {
	for i := 1; i < len(recs); i++ {
		for j := i; j > 0 && inflightLess(recs[j], recs[j-1]); j-- {
			recs[j], recs[j-1] = recs[j-1], recs[j]
		}
	}
}

func inflightLess(a, b InflightTurnRecord) bool {
	if a.StartedAt.Equal(b.StartedAt) {
		return a.RunID < b.RunID
	}
	return a.StartedAt.Before(b.StartedAt)
}

// sortApps sorts by CreatedAt ascending, with ID as a tie-breaker. Insertion sort is enough: a single workspace has few apps.
func SortApps(apps []*App) {
	for i := 1; i < len(apps); i++ {
		for j := i; j > 0 && appLess(apps[j], apps[j-1]); j-- {
			apps[j], apps[j-1] = apps[j-1], apps[j]
		}
	}
}

func appLess(a, b *App) bool {
	if a.CreatedAt.Equal(b.CreatedAt) {
		return a.ID < b.ID
	}
	return a.CreatedAt.Before(b.CreatedAt)
}

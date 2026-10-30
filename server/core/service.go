// Wiring for Service, plus small helpers shared by all domains.
//
// Business invariants live in the various *_service files of this package. Their **reference
// behavior** is LocalApiClient in the frontend's web/src/data/api.ts: every side effect, every error
// message, and the presence or absence of every field in that class is something the pages really
// depend on. So many comments below explain "why we copy a behavior that looks odd": copying it lets
// the frontend switch from LocalApiClient to HttpApiClient with zero changes.

package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	agentsvc "github.com/chyroc/ama/server/service/agent"
	arkcredsvc "github.com/chyroc/ama/server/service/arkcred"
	issuesvc "github.com/chyroc/ama/server/service/issue"
	revisionsvc "github.com/chyroc/ama/server/service/revision"
	runsvc "github.com/chyroc/ama/server/service/run"
	schedulesvc "github.com/chyroc/ama/server/service/schedule"
	skillsvc "github.com/chyroc/ama/server/service/skill"
	suggestionsvc "github.com/chyroc/ama/server/service/suggestion"
	workspacesvc "github.com/chyroc/ama/server/service/workspace"
	"github.com/chyroc/ama/server/shared/gopkg/resource"
	"github.com/chyroc/ama/server/shared/infra/shelltools"
)

// ArkVerifier makes one real call to a read-only Ark API with a key to tell whether it works.
//
// **The real type is arkcredsvc.Verifier** (the interface is declared by the consumer, and the
// consumer has moved to service/arkcred). An alias is kept here: the real implementation
// LiveArkVerifier still lives in core/arkverify.go (it imports shared/infra/ma to call the real Ark
// service; it is an adapter, not business logic), and cmd/amad's wiring says
// core.NewLiveArkVerifier(), so that line doesn't need to change.
//
// It is an interface so tests don't need the network: tests plug in a fake.
type ArkVerifier = arkcredsvc.Verifier

// ErrArkKeyInvalid means Ark explicitly rejected this key (401/403), as opposed to the network not
// getting through.
//
// **The real value is arkcredsvc.ErrArkKeyInvalid** (the same variable, equal under errors.Is):
// core/arkverify.go wraps errors with it and service/arkcred uses it to branch, so both sides must
// be the same one.
//
// This distinction is needed: the frontend's verifyArkCredential has no try/catch, so throwing "key
// invalid" as an HTTP error would become an unhandled rejection, and the page would show nothing. So
// an invalid key must return 200 + check:"invalid", and only a real network failure counts as a
// failed request.
var ErrArkKeyInvalid = arkcredsvc.ErrArkKeyInvalid

// ArkKeyFetcher turns "list Ark API Keys from the Volcengine account" into an interface.
//
// **The real type is arkcredsvc.KeyFetcher** (the interface is declared by the consumer, which has
// moved to service/arkcred). An alias is kept here for core.Config to reference; currently cmd/amad
// passes nil (keys are pasted by hand).
type ArkKeyFetcher = arkcredsvc.KeyFetcher

// Config is the configuration needed to build a Service.
type Config struct {
	// Store is required.
	Store Store

	// When ArkVerifier is empty, verifyArkCredential returns a "no verifier configured" error.
	//
	// Leaving it empty is allowed: without it all other APIs run normally; only the verify action is
	// unavailable.
	ArkVerifier ArkVerifier

	// ArkRunner is the executor for the conversation path (creating Ark Agents / sessions, sending
	// and receiving events, subscribing to streams).
	//
	// When empty, the three conversation APIs return an ErrPrecondition saying "the server is not
	// connected to Ark", and all other APIs work as usual. As with ArkVerifier, it may be empty so
	// tests and local runs that don't touch Ark don't need the network.
	ArkRunner ArkRunner

	// Logger defaults to slog's default logger when empty.
	//
	// The conversation path needs it: problems such as failing to save an event should not interrupt
	// the reply the user is watching, but they must not be swallowed silently either.
	Logger *slog.Logger

	// DefaultOwner is the fallback owner used when creating a workspace **when the current logged-in
	// user can't be found**.
	//
	// A workspace must have an owner (logic on the members page such as "can't remove the only owner"
	// depends on it), and Ark credentials also record updatedBy. On the normal path the owner is **the
	// person currently logged in**: all business APIs sit behind the auth middleware, so ctx always
	// has an auth.Identity; see Service.ownerFrom.
	//
	// So this field only takes effect in two cases: unit tests calling Service directly (not via
	// HTTP), and playing around locally with AMA_STORE=mem. When empty, the person hard-coded in
	// LocalApiClient is used, so the e2e assertions keep holding.
	DefaultOwner Owner

	// ChatOrigins can find "where a session originally came from", so agent self-wakeup can decide
	// where to send the result.
	//
	// When empty, self-wakeup tasks are always run_only (the result only goes to run history). **That
	// is not a failed downgrade**: sessions opened from the conversation page have no Feishu origin
	// anyway, and seeing the result in run history is normal for the user.
	ChatOrigins ChatOrigins

	// FeedbackReaders decides who counts as "the dev team", used by the feedback read APIs to check
	// permissions.
	//
	// ⚠️ **When empty, no one can read** (fail-closed). This is the opposite of the other optional
	// dependencies in this package: an empty ArkVerifier / ArkRunner / ChatOrigins just means one
	// feature fewer, while leaving this one empty **closes a door**. If the fallback let everyone in,
	// "forgot to wire it" would mean the door doesn't exist at all, with zero errors; if the fallback
	// denies, it is noticed on the first click. Same reason as feishu.Config.Authorizer denying
	// everything when empty.
	FeedbackReaders FeedbackReaders

	// ArkKeyFetcher supports "automatically fetch the Ark API Key from the Volcengine account".
	//
	// **Leaving it empty just removes a shortcut** (same meaning as ArkVerifier, not closing a door):
	// the related APIs then return a clear precondition error, and pasting a key by hand still works.
	// See service/arkcred/fetch.go.
	ArkKeyFetcher ArkKeyFetcher

	// BlobStore decides where the bytes of attachments and skill files are stored. cmd/amad installs
	// an S3-compatible object store (shared/infra/s3, required for the PG tier); the mem tier uses an
	// in-memory implementation when it is not configured. **PG stores no file bytes.**
	//
	// When **empty** (only in unit tests), attachment upload and reading/writing skills with
	// attachments return ErrPrecondition (422); there is no fallback elsewhere.
	BlobStore BlobStore

	// TerminalRelay forwards terminal keystrokes from "the instance that got the POST" to "the
	// instance holding the PTY connection".
	//
	// ⚠️ **Leaving it empty reproduces the production failure exactly**: the terminal's downstream
	// SSE and each upstream keystroke are separate HTTP requests, randomly and independently spread by
	// the load balancer, while the PTY's WebSocket is only held by the instance that opened the SSE.
	// When empty, a process without this terminal can only return 404; in production 14 of 24
	// keystrokes were measured landing on another instance.
	//
	// Leaving it empty is safe for a single instance (local ./dev.sh, unit tests): then every POST is
	// sure to land in the same process.
	TerminalRelay TerminalRelay

	// WorkerWakeups lets "send a message into Ark" wake up that environment's worker poll loop at
	// once.
	//
	// **Optional**: without it, the worker only finds new work via the stepped-backoff timer, at worst
	// one step late (at most 60 seconds). Correct, just slow, so it is a degrade-open pure
	// optimization.
	//
	// ⚠️ It is backed by **the same pgmq queue** as TerminalRelay, just with a different topic and
	// payload. Both must work across pods: after leader election an environment has only one poll loop
	// globally, while the request that sends the message lands randomly via the load balancer, usually
	// not on that pod. See core/workerwake.go.
	WorkerWakeups WorkerWakeups

	// TerminalWSOrigins is the allowlist of **page origins** that may open a terminal WebSocket.
	//
	// The terminal's main path is a WS, and it connects to another domain (ama-ws.example.com, whose
	// gateway doesn't use h2). So the page is on the ama domain and the connection on the ama-ws
	// domain; **the Origin is always different**, same-origin can't be required, and it can only be
	// checked against an allowlist.
	//
	// ⚠️ **When empty, every connection with an Origin is rejected** (fail-closed), i.e. the browser
	// can't connect at all and can only use the SSE fallback. This is deliberate: this connection can
	// run any command in the container, and "allow everything when unconfigured" would open the
	// terminal to the whole network (CSWSH), a hole with zero errors. Better that the terminal can't
	// connect (logged, traceable) than to open it silently.
	//
	// In production set the page domain (e.g. `https://ama.example.com`); for local development set
	// `http://localhost:5273`. **"*" is not supported**, for the same reason as
	// httpd.Config.AllowedOrigin.
	TerminalWSOrigins []string

	// HostBindings is the binding check for the host executor, passed through to the agent domain
	// (when saving an agent draft it checks "this machine belongs to this person and is bound to the
	// workspace"). The real type is service/host.Service, built in cmd/amad before core.Service.
	// **Optional**: saving an agent with the host executor is then explicitly rejected.
	HostBindings agentsvc.HostBindings
}

// TerminalRelay is the cross-instance delivery ability the terminal's upstream needs, provided by
// shared/infra/pgmq.
//
// **Declared locally here instead of importing shared/infra/pgmq**: core should not depend on a
// specific queue implementation, and switching to something else (Redis, in-memory) needs no change
// here. Same approach as Leases / LeaseStore.
type TerminalRelay interface {
	// Publish delivers one frame of keystrokes to the instance holding this terminal.
	//
	// The short ttl is deliberate: a backlog of old keystrokes flooding into the container after a
	// reconnect is harmful.
	Publish(ctx context.Context, topic string, payload []byte, ttl time.Duration) error

	// Consume keeps consuming the topic until ctx ends. If fn returns an error, the message stays in the queue for redelivery.
	Consume(ctx context.Context, topic string, fn func(context.Context, RelayMessage) error) error
}

// RelayMessage is one forwarded frame of keystrokes.
//
// It deliberately keeps only Payload: core only cares "what this frame is"; the queue's internal
// id/timestamp are implementation details. Before adding a field, ask whether core really needs it.
type RelayMessage struct {
	Payload []byte
}

// ChatOrigins turns "Ark session → which Feishu message it originally came from" into an interface.
//
// # Why an interface instead of querying feishu's store directly
//
// core and feishu are deliberately coupled only by ID strings (see the comment in core/agent.go).
// The data for this lives on the feishu side (feishu_inflight_turns records the run → chat/message
// mapping), but the one that needs it is core's self-wakeup tool. So core only declares what it
// needs, and cmd/amad connects it during wiring, fully symmetric with feishu.Conversations.
type ChatOrigins interface {
	// OriginOf returns the delivery address for this run. A false second return value means it was
	// not found (sessions opened from the conversation page aren't found), and the caller falls back
	// to run_only.
	//
	// **Not found is not an error**, so the signature has no error: on this path "no Feishu origin"
	// is the normal case, and expressing it with error would make callers check every time "is this a
	// real error or just nothing there".
	OriginOf(ctx context.Context, runID string) (apimodel.DeliveryTarget, bool)
}

// Owner is the identity of an operator.
type Owner struct {
	Name  string
	Email string
}

// The creator hard-coded in LocalApiClient (createWorkspace in api.ts).
// e2e's 05-settings asserts that this member's remove button is disabled, so the default must match it.
var defaultOwner = Owner{Name: "张三", Email: "alice@example.com"}

// ownerFrom gets "the person doing this operation", used to write the creator as the workspace owner.
//
// **The normal path is the current logged-in user**: all business APIs sit behind the auth
// middleware, so ctx always has an auth.Identity, and the email is the Actor ID.
//
// Falling back to s.owner only happens in two cases: unit tests calling Service directly (not via
// HTTP), and playing around locally with AMA_STORE=mem. **Production never reaches this fallback**;
// reaching it means some API missed auth, which is a bug, but a usable owner is still given here
// instead of leaving it empty, because "a workspace without an owner" would break a pile of logic on
// the members page on the spot (can't remove the only owner, updatedBy on Ark credentials).
func (s *Service) ownerFrom(ctx context.Context) Owner {
	identity, ok := auth.IdentityFrom(ctx)
	if !ok {
		return s.owner
	}
	name := strings.TrimSpace(identity.Name)
	if name == "" {
		name = identity.Email
	}
	return Owner{Name: name, Email: identity.Email}
}

// Service is this package's business entry point. It is safe to share across goroutines.
type Service struct {
	store  Store
	runner ArkRunner
	logger *slog.Logger
	owner  Owner
	// chats looks up session origins so self-wakeup can decide the delivery address. May be nil; see Config.ChatOrigins.
	chats ChatOrigins

	// feedbackReaders decides who counts as "the dev team", used by ListFeedback to check permissions.
	//
	// **When nil, everyone is denied** (not everyone allowed); see the comment on ListFeedback.
	feedbackReaders FeedbackReaders

	// blobs is the byte store for attachments and skill files. **May be nil** (when not wired in unit
	// tests); see Config.BlobStore and core/blob.go.
	blobs BlobStore

	// termRelay forwards terminal keystrokes to the instance holding the PTY connection. **May be nil** (not needed for a single instance).
	// When nil, cross-instance keystrokes get a 404, i.e. the behavior before the fix; see Config.TerminalRelay.
	termRelay TerminalRelay

	// workerWakeups wakes up that environment's poll loop when "a new message goes into Ark". **May be nil**.
	//
	// When nil, the worker only finds new work via the stepped-backoff timer, at worst one step late
	// (at most 60 seconds): correct, just slow. See core/workerwake.go.
	workerWakeups WorkerWakeups

	// wsOrigins is the allowlist of page origins that may open a terminal WS.
	// **When empty, every connection with an Origin is rejected** (fail-closed); see Config.TerminalWSOrigins.
	wsOrigins []string

	// sandboxAttacher reattaches to the container by Run, used by the "view container" APIs.
	//
	// **May be nil** (it is nil when the Docker executor is not enabled); then status queries return
	// ErrPrecondition instead of pretending "already reclaimed": the container may be alive and well,
	// this process just can't attach to it. See attachSandbox in core/sandbox.go.
	sandboxAttacher SandboxAttacher

	// profiles looks up display names and avatars by email, so list pages can render "who created it".
	//
	// **May be nil** (in-memory store, authdao not wired locally); then ResolveUserProfiles returns an
	// empty map and the frontend falls back to the email's first letter: fully functional, just no
	// real avatar. See core/userprofile.go.
	profiles UserProfileLookup

	// integrations is the registry of authorization sources, used by error messages to look up a
	// provider's display name.
	//
	// **May be nil** (not wired in unit tests); then the display name falls back to the bare provider
	// id. Installed by cmd/amad with SetIntegrationRegistry (the registry is built after core).
	//
	// ⚠️ It is **the same registry instance** as Handler.integrations (cmd/amad passes the same pointer).
	integrations *IntegrationRegistry

	// shellEnvs lets webshell scrub credentials from its output; see scrubShellOutput. May be nil.
	//
	// ⚠️ **The shelltools scrub layer can't be reused**: that layer only covers the six tool calls;
	// webshell is another exit, and without scrubbing a single env from the user would show the
	// credentials in the browser.
	shellEnvs shelltools.SessionEnvSource

	// hostTerms is the frame dispatcher for the host web terminal (host.Service satisfies it),
	// injected during wiring by SetHostTerminalDispatcher. When empty, terminal connections for host
	// runs get 503.
	hostTerms HostTerminalDispatcher

	// termHub keeps the live terminal connections (SSE and POST use it to find the same PTY).
	//
	// **It is in-process on purpose**: losing it only means reopening the terminal once, and no data
	// is damaged. See the file header of core/sandboxterm.go for details.
	termHub  *terminalHub
	termOnce sync.Once

	// activeStreams tracks the number of SSE stream connections currently active for each run.
	streamMu      sync.Mutex
	activeStreams map[string]int

	// lastBackfill records "the last time backfilling Ark events for this run succeeded", used to cut
	// the second Ark round trip within the same page open (the detail page calls getRun and then
	// listRunEvents, and both use the same check).
	//
	// **In-process is deliberate and allowed**: by the rule in CLAUDE.md, only things whose loss "just
	// makes it slower" may live in-process, and this is one: the worst result of losing it is one
	// extra call to Ark, with no effect on functionality. See backfillCooldown in core/ark.go.
	backfillMu   sync.Mutex
	lastBackfill map[string]time.Time

	// idSeq is used by nextID. The frontend starts with a timestamp; an atomic counter is enough here,
	// since IDs only need to be unique within the process and carry a recognizable prefix.
	idSeq atomic.Uint64

	// windowRetry is the server-side retry budget for "hitting the tool execution window"; see
	// sendEventsThroughWindow.
	//
	// **Adjustable only for tests**: the zero value uses windowRetryBudget (1 second). Tests squeeze it
	// down to a few milliseconds, or every test of "Ark permanently refuses" would wait out a full
	// budget for nothing. Production code doesn't set it; Config deliberately has no matching field.
	windowRetry time.Duration

	// syncKick lets "save agent" kick this instance's AgentSyncer to scan right away, without waiting
	// for the next tick.
	//
	// **In-process is deliberate and allowed**: by the rule in CLAUDE.md, only things whose loss "just
	// makes it slower" may live in-process, and this is one: losing it (syncer not started, or the
	// instance that enqueued is not the one running it) is at worst one scan cycle late; correctness
	// rests on the atomic claim in PG. See core/agentsync.go.
	syncKickMu sync.RWMutex
	syncKick   chan<- struct{}

	// issues is the Service for the Issue domain; **the logic has moved to service/issue**.
	//
	// All that is left on the core side are the 13 forwarding methods in core/issue.go; the reason
	// (two static assertions require *core.Service to keep these methods) is written at the top of
	// that file. It is a **transitional state**, and goes away with the step "move handlers out of
	// core".
	//
	// **Non-nil**: New always builds it and returns an error if it can't. There is no "may be nil, so
	// degrade" here: the issue domain is a core feature, and failing to build it must blow up at
	// startup.
	issues *issuesvc.Service

	// suggestions is the Service for the product Suggestion domain; **the logic has moved to
	// service/suggestion**.
	//
	// All that is left on the core side are the 14 forwarding methods in core/suggestion.go; the
	// reason (two static assertions require *core.Service to keep these methods) is written at the top
	// of that file. It is a **transitional state**, and goes away with the step "move handlers out of
	// core".
	//
	// **Non-nil**: New always builds it and returns an error if it can't, same as issues.
	suggestions *suggestionsvc.Service

	// revisions is the Service for the entity revision history domain (rollback after mistakes);
	// **the logic has moved to service/revision**. core/revision.go only keeps forwarding methods and
	// one slot for each of the three entities.
	//
	// ⚠️ Unlike the two domains above, this one is **not a leaf**: saving and deleting an agent /
	// skill / schedule calls it to record a version (recordRevision / dropRevisions on the core side
	// forward here), and on rollback it in turn writes those three entities. The second direction is
	// untangled by the three slots in revisionsvc.Entity; see the file header of
	// service/revision/entity.go.
	revisions *revisionsvc.Service

	// schedules is the Service for the Schedule domain; **the logic has moved to
	// service/schedule**. core/schedule.go only keeps 7 forwarding methods and two function aliases.
	//
	// ⚠️ Unlike issue / suggestion, this domain is **not a leaf**: saving and deleting call revisions
	// to record a version / clear history. That direction is untangled by a narrow interface
	// **declared by this domain itself** (schedulesvc.Revisions, with only Record / Drop), and New
	// fills in s.revisions directly.
	//
	// ⚠️ **The scheduler (core/scheduler.go) did not move with it**: it drives the whole conversation
	// path and depends on the run / chat / ark domains, which are not split out yet. It still uses
	// the claim methods through s.store (core.Store embeds schedulesvc.Store), and its behavior is
	// unchanged line for line. The full argument is in the file header of service/schedule/service.go.
	schedules *schedulesvc.Service

	// workspaces is the workspace domain (workspaces + members).
	//
	// **Non-nil**: New always fills it in. The methods in core/workspace.go all forward to it, and
	// among them RoleOf **is the entry point for permission checks across the whole site**:
	// authz/guard.go calls it on every request via cmd/amad's authzMembership.
	workspaces *workspacesvc.Service

	// arkCreds is the Service for the Ark credential domain (three levels: account / workspace /
	// agent); **the logic has moved to service/arkcred**. core/ark.go only keeps 19 forwarding methods
	// plus two unexported resolver forwards; the reason (two static assertions require *core.Service
	// to keep them) is written at the top of that file. It is a **transitional state**.
	//
	// **Non-nil**: New always builds it and returns an error if it can't.
	//
	// ⚠️ Unlike issue / suggestion, this domain is **not a leaf**, but the coupling goes only one way:
	// chat.go on the core side (arkKey, getting the key for a conversation) and arkconsole.go
	// (computing the region) need the three-level resolution, so core/ark.go keeps two forwards,
	// resolveArkSecret / resolveArkSecretForAgent. There is no reverse direction: **this domain calls
	// nothing in core**; the check is that `go list -deps ./service/arkcred` doesn't include core.
	arkCreds *arkcredsvc.Service

	// runs is the Service for the run history (read-only) domain; **the logic has moved to
	// service/run**. core/run.go only keeps 3 forwarding methods (ListRuns / GetRun / ListRunEvents).
	//
	// **Non-nil**: New always builds it and returns an error if it can't.
	//
	// ⚠️ Unlike issue / suggestion, this domain is **not a leaf**; it has two coupling directions,
	// both pointing outward:
	//
	//	read agent     computes the two derived fields Run.AgentDeleted / AgentDisabled.
	//	               Uses this domain's own Store (reading other tables is fine, see
	//	               service/run/store.go).
	//	backfill Ark   injects s.backfillFromArk (the real one is in core/runbackfill.go; it calls
	//	               the conversation domain's SyncRunEvents, so it stays in core).
	//
	// There is no reverse direction: **this domain calls nothing in core**; the check is that
	// `go list -deps ./service/run` doesn't include core.
	runs *runsvc.Service

	// agents is the Service for the agent domain; **the logic has moved to service/agent**.
	// core/agent.go only keeps 10 forwarding methods.
	//
	// **Non-nil**: New always builds it and returns an error if it can't.
	//
	// ⚠️ This domain is **not a leaf**; it has four outward couplings, all of the form "the consumer
	// declares a narrow interface + it is filled in here"; the domain itself doesn't know core:
	//
	//	Revisions     record a version before save, clear history after delete → fill s.revisions directly
	//	Members       check admin/owner                         → fill s.workspaces directly (same source as RoleOf)
	//	Integrations  auth source display name / whether the    → the real one is still in core/wsintegration.go,
	//	              workspace is bound                          wrapped in an adapter here
	//	Syncs         schedule one Ark sync after save          → the real one is still in core/agentsync.go (10b),
	//	                                                          also wrapped in an adapter
	//
	// There is no reverse direction: the check is that `go list -deps ./service/agent` doesn't include core.
	agents *agentsvc.Service

	// skills is the Service for the Skill domain; **the logic has moved to service/skill** (step 11b).
	// core/skill.go only keeps forwarding methods and type aliases.
	//
	// ⚠️ The same two kinds of not-yet-moved pieces as for agent are still in core: skillark (uploads
	// skills to Ark, blocked on the runner, since the ark domain isn't split out) and skillinject /
	// skillarchive / skillupdater (injecting into containers, archive download, the auto-update sweep;
	// coupled to the long-running executors and the struct). They still use the skill domain through
	// the forwarding methods on this struct, with behavior unchanged line for line.
	skills *skillsvc.Service

	// resources is the table of "resource type → how to find the workspace it belongs to", used by
	// the authorization layer.
	//
	// **Non-nil**: New always fills it in (registerResources). When nil, WorkspaceOf returns an error
	// instead of an empty string; an empty string would make authz/guard.go let the request through
	// directly, see the header of the shared/gopkg/resource package.
	resources *resource.Registry
}

// New wires up a Service from the config.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, errors.New("core: Store 不能为 nil")
	}
	owner := cfg.DefaultOwner
	if strings.TrimSpace(owner.Email) == "" {
		owner = defaultOwner
	}
	s := &Service{
		store:           cfg.Store,
		runner:          cfg.ArkRunner,
		logger:          cfg.Logger,
		owner:           owner,
		chats:           cfg.ChatOrigins,
		feedbackReaders: cfg.FeedbackReaders,
		blobs:           cfg.BlobStore,
		termRelay:       cfg.TerminalRelay,
		workerWakeups:   cfg.WorkerWakeups,
		wsOrigins:       cfg.TerminalWSOrigins,
		activeStreams:   make(map[string]int),
		lastBackfill:    make(map[string]time.Time),
	}
	s.idSeq.Store(uint64(time.Now().UnixMilli()))

	// Wire the workspace domain (workspaces + members). **It must come after idSeq**, for the same
	// reason as the domains below: the NextID closure captures s, and nextID depends on idSeq's initial value.
	workspaces, err := workspacesvc.New(workspacesvc.Config{
		Store:  cfg.Store,
		NextID: s.nextID,
		// Fallback owner when the current logged-in user can't be found. The section above already filled an empty value with defaultOwner.
		DefaultOwner: workspacesvc.Owner{Name: owner.Name, Email: owner.Email},
		Logger:       cfg.Logger,
	})
	if err != nil {
		return nil, err
	}
	s.workspaces = workspaces

	// Wire the issue domain. **It must come after idSeq**: the NextID closure captures s, and nextID
	// depends on idSeq's initial value; in the wrong order the first few ids would start from 0 and
	// collide with existing ids.
	//
	// cfg.Store is used directly as issue.Store: core.Store embeds it (see core/store.go), so the
	// conversion holds at compile time and needs no assertion.
	issues, err := issuesvc.New(issuesvc.Config{
		Store:  cfg.Store,
		NextID: s.nextID,
		// Both hooks for attachment bytes go through that pair of forwards in core/blob.go (the real
		// one is in shared/infra/filebytes). **filebytes is not handed over directly**: then
		// service/issue would need to know how BlobStore and the files table are wired, while it only
		// cares about "store it, get it back".
		PutBlob:  s.putAttachmentBlob,
		ReadBlob: s.attachmentBytes,
	})
	if err != nil {
		return nil, err
	}
	s.issues = issues

	// Wire the suggestion domain. Same ordering requirement as above (after idSeq).
	//
	// ⚠️ **It has one more thing than issue, IsStaff**: suggestions are platform-level and authz
	// can't judge them (that batch in the matrix is all ActionNone); the permission check is entirely
	// inside the domain package, so it needs to know "who is the platform team". Pass the method
	// value s.isSuggestionStaff instead of computing a bool on the spot: that method reads the
	// s.feedbackReaders field, which **must be asked fresh every time**: in tests the field is
	// assigned after New, and computing it here would always get the "not wired" answer.
	suggestions, err := suggestionsvc.New(suggestionsvc.Config{
		Store:    cfg.Store,
		NextID:   s.nextID,
		PutBlob:  s.putAttachmentBlob,
		ReadBlob: s.attachmentBytes,
		IsStaff:  s.isSuggestionStaff,
	})
	if err != nil {
		return nil, err
	}
	s.suggestions = suggestions

	// Wire the revision history domain. Same ordering requirement as above (after idSeq).
	//
	// ⚠️ **It has one more thing than the two domains above, Entities**: this domain is not a leaf; it
	// touches the three entities agent / skill / schedule (the read APIs check for 404, rollback
	// writes the snapshot back), while domain packages import nothing from each other, so those three
	// slots are provided by **the side that owns the entities**, which is this package; see
	// registerRevisionEntities.
	//
	// ⚠️ **The closures in those three slots capture s, so they can only be installed here, not
	// earlier**: they call s.updateAgent / s.updateSkill / s.updateSchedule. In the other direction
	// agent.go calls s.revisions to record snapshots. Both directions are on the same s, and are kept
	// apart by "closures evaluate late": wiring only stores the functions, and the real mutual calls
	// happen on the request path, when both sides are wired.
	//
	// If one of the three slots is missing, New returns an error; the reason is in the file header of
	// service/revision/service.go.
	revisions, err := revisionsvc.New(revisionsvc.Config{
		Store:    cfg.Store,
		NextID:   s.nextID,
		Entities: s.registerRevisionEntities(),
		Logger:   cfg.Logger,
	})
	if err != nil {
		return nil, err
	}
	s.revisions = revisions

	// Wire the schedule domain. **It must come after s.revisions**: saving/deleting in this domain
	// calls it to record a version / clear history, and that dependency is **a concrete pointer read
	// inside New** (unlike the three slots above, which are closures that can delay evaluation to the
	// request path). In the wrong order nil would be passed in, and schedulesvc.New errors on the
	// spot; that is deliberate, not luck.
	//
	// ⚠️ **s.revisions is passed directly, with no extra wrapping.** *revisionsvc.Service has the two
	// methods Record / Drop and implicitly satisfies schedulesvc.Revisions. The two core-side forwards
	// recordRevision / dropRevisions **are not on this path**: they are still used by agent.go / the
	// skill side, and go away when those two domains are split into packages.
	schedules, err := schedulesvc.New(schedulesvc.Config{
		Store:     cfg.Store,
		NextID:    s.nextID,
		Revisions: revisions,
		Logger:    cfg.Logger,
	})
	if err != nil {
		return nil, err
	}
	s.schedules = schedules

	// Fill the resource lookup table. **It must come after s.issues / s.schedules are wired**: those
	// two domains fill their own slots.
	//
	// ⚠️ The suggestion domain has **no** such step, and that is not an omission: it is platform-level,
	// has no workspace to look up, so it fills no slot. The reason is in the last section of
	// service/suggestion/service.go.
	s.registerResources()

	// Wire the Ark credential domain. **It must come after s.workspaces**: OwnerEmailOf must share
	// its source with that domain's FirstOwnerEmail (different sources would make "who configured
	// this key" give different answers on two screens, with zero errors).
	//
	// ⚠️ Two func fields instead of an interface; the reason is on arkcredsvc.Config: they require
	// "**the same function** as on the core side", and injecting a function states that more clearly
	// than declaring an interface.
	arkCreds, err := arkcredsvc.New(arkcredsvc.Config{
		Store:      cfg.Store,
		Verifier:   cfg.ArkVerifier,
		KeyFetcher: cfg.ArkKeyFetcher,
		// Get "the person doing this operation" to write UpdatedBy on agent-level credentials. Uses
		// ownerFrom (with a fallback owner when there is no login identity), the same rule as writing
		// the owner when creating a workspace.
		ActorEmail: func(ctx context.Context) string { return s.ownerFrom(ctx).Email },
		// Get the workspace's "first owner" to write UpdatedBy on workspace-level credentials.
		// firstOwnerEmail is an alias of workspacesvc.FirstOwnerEmail; see core/workspace.go.
		OwnerEmailOf: firstOwnerEmail,
		Logger:       cfg.Logger,
	})
	if err != nil {
		return nil, err
	}
	s.arkCreds = arkCreds

	// Wire the run history domain (read-only).
	//
	// ⚠️ **Two func fields instead of an interface**, for the same reason as the two in arkcred
	// above: this domain doesn't want "something that can backfill", but **the same function as on
	// the core side**: s.backfillFromArk carries a 3-second cap and a 1-second cooldown
	// (core/runbackfill.go), and swapping in another implementation would connect Ark's 10-minute
	// default timeout to the user's page. The injected function is that constraint itself.
	//
	// ⚠️ **Pass method values instead of evaluating on the spot**: both read fields of s (s.runner /
	// s.activeStreams) and must be asked fresh each time. In tests s.runner is assigned after New
	// (runwatcher_test.go writes svc.runner directly), and computing it here would always get the
	// "not connected to Ark" answer.
	runs, err := runsvc.New(runsvc.Config{
		Store:           cfg.Store,
		Backfill:        s.backfillFromArk,
		HasActiveStream: s.hasActiveStream,
		ConsoleRegion:   s.arkConsoleRegion,
		Logger:          cfg.Logger,
	})
	if err != nil {
		return nil, err
	}
	s.runs = runs

	// Wire the agent domain. **It must come after s.revisions and s.workspaces**: both are concrete
	// dependencies it reads inside New (unlike the three revision slots, which are closures that can
	// delay evaluation to the request path). In the wrong order nil would be passed in, and
	// agentsvc.New errors on the spot; that is deliberate, not luck.
	//
	// ⚠️ **Members is filled with s.workspaces directly**, so "is this person an admin" and the
	// authz.Guard permission check go through **the same RoleOf**: there can't be a contradiction like
	// "Guard let it through but saving was blocked".
	//
	// ⚠️ **The other two dependencies are wrapped in adapters, because the real ones are still in
	// core**: the auth source registry is in core/wsintegration.go, and sync scheduling is in
	// core/agentsync.go (that family belongs to 10b). Both adapters hold s and ask fresh each time;
	// **they can't be evaluated here on the spot**: in tests the s.integrations field is assigned
	// after New.
	agents, err := agentsvc.New(agentsvc.Config{
		Store:        cfg.Store,
		NextID:       s.nextID,
		Revisions:    revisions,
		Members:      workspaces,
		Integrations: agentIntegrations{svc: s},
		Syncs:        agentSyncs{svc: s},
		Console:      agentConsole{svc: s},
		HostBindings: cfg.HostBindings,
		Logger:       cfg.Logger,
	})
	if err != nil {
		return nil, err
	}
	s.agents = agents

	// Wire the skill domain (11b). **It must come after s.revisions** (Revisions is a concrete
	// dependency it reads inside New); Blobs directly takes the field assigned at the start of
	// wiring, which **may be nil** (no object store connected); that is a feature switch, not a
	// wiring omission, and skillsvc.New doesn't check them; see service/skill/service.go.
	//
	// ⚠️ **Blobs is an interface-to-interface assignment**: core.BlobStore has four methods (including
	// Delete), skillsvc.BlobStore needs only three (Put/Get/Bucket); the method set is a superset, so
	// it is passed directly.
	skills, err := skillsvc.New(skillsvc.Config{
		Store:     cfg.Store,
		NextID:    s.nextID,
		Revisions: revisions,
		Blobs:     func() skillsvc.BlobStore { return s.blobs },
		Ark:       skillArk{svc: s},
		Logger:    cfg.Logger,
	})
	if err != nil {
		return nil, err
	}
	s.skills = skills

	return s, nil
}

// agentIntegrations connects the core-side authorization source abilities to service/agent.
//
// The real versions of both methods are in core/wsintegration.go (neither the auth source registry
// nor workspace bindings are split into packages yet). **It holds *Service instead of computing the
// values on the spot**: in tests the s.integrations field is assigned after New, and evaluating it
// during wiring would always get the "not wired" answer.
type agentIntegrations struct{ svc *Service }

func (a agentIntegrations) DisplayName(providerID string) string {
	return a.svc.integrationDisplayName(providerID)
}

// agentSyncs connects the core-side "schedule one Ark sync" to service/agent.
//
// The real one is in core/agentsync.go; it shares a pair of in-process state fields with the
// long-running executor AgentSyncer, and the executor depends on the ark / chat domains, which are
// not split out yet; the whole block is left for the next step (10b).
//
// ⚠️ **The return value is dropped on purpose**: EnqueueAgentSync returns a task row, but the agent
// domain has no use for it (the frontend checks sync status through another API). The interface only
// lists what is used; see agentsvc.Syncs.
type agentSyncs struct{ svc *Service }

func (a agentSyncs) EnqueueAgentSync(ctx context.Context, ag apimodel.Agent) {
	a.svc.EnqueueAgentSync(ctx, ag)
}

// markStreamActive marks a run's event stream as active, and returns the cleanup function for when the stream ends.
func (s *Service) markStreamActive(runID string) func() {
	s.streamMu.Lock()
	if s.activeStreams == nil {
		s.activeStreams = make(map[string]int)
	}
	s.activeStreams[runID]++
	s.streamMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			s.streamMu.Lock()
			defer s.streamMu.Unlock()
			if s.activeStreams[runID] <= 1 {
				delete(s.activeStreams, runID)
			} else {
				s.activeStreams[runID]--
			}
		})
	}
}

// hasActiveStream reports whether there is currently an active SSE connection subscribed to this run's events.
func (s *Service) hasActiveStream(runID string) bool {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	return s.activeStreams != nil && s.activeStreams[runID] > 0
}

// log returns this Service's logger, or slog's default if none is configured.
//
// It is not defaulted inside New, so that "not configured" stays visible in Config; defaulting at
// read time means call sites don't need a nil check every time.
func (s *Service) log() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

// nowISO is a local alias of apimodel.NowISO, so call sites don't have to write the package name.
func nowISO() string { return apimodel.NowISO() }

// nextID generates an ID with a prefix.
//
// The prefix is an **observable contract**: the frontend seed uses ws_ / m_ / agent_ / skill_ / fs_,
// and a Feishu app's ID is put into eventSubscriptionUrl. How the numeric part is made doesn't matter.
func (s *Service) nextID(prefix string) string {
	return fmt.Sprintf("%s_%s", prefix, strconv36(s.idSeq.Add(1)))
}

// strconv36 converts a number to base 36, matching toString(36) in the frontend's nextId.
func strconv36(n uint64) string {
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	if n == 0 {
		return "0"
	}
	var buf [16]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%36]
		n /= 36
	}
	return string(buf[i:])
}

// maskSecret masks a secret. **The real one is persistmodel.MaskSecret** (a zero-dependency pure
// string function that both core and repository/dao use to build/assert masks).
//
// The business call sites on the core side moved away with the credential domain; this alias is kept
// for tests in the same package (globalark_test.go / agentark_test.go use it to compute the expected
// mask).
var maskSecret = persistmodel.MaskSecret

// validateArkAPIKey validates an Ark API Key. **The real one is in service/arkcred.**
//
// Like maskSecret, this alias is kept for tests in the same package (service_test.go has four
// assertions pinning its Chinese messages; e2e's 08-ark-credential also asserts the original strings,
// so those messages can't change).
var validateArkAPIKey = arkcredsvc.ValidateAPIKey

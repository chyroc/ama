// MemStore: the in-memory implementation of Store.
//
// # Why it is here, and not in core next to the interface
//
// The two implementations of the same interface are kept together (PG is next door in coredao), so readers can compare
// "PG does it this way, memory does it that way" directly. It is also the implementation of the AMA_STORE=mem **production fallback**,
// not just a test double.
//
// # ⚠️ Why this package must not import core
//
// **It uses not a single core symbol; this is kept on purpose, do not add one.**
//
// Adding one `import core` line would immediately break a batch of tests: core's own internal tests (package core)
// construct MemStore, and then core → coremem → core is a cycle, reported as
// `import cycle not allowed in test`. ⚠️ This cycle **still passes go build**; only
// go vet / go test report it, so "it compiles" is not evidence of safety.
//
// coredao next door can import core because no package core test constructs PGStore.
// MemStore is different: core's internal tests construct it directly, so it may only depend on leaf packages
// (model/apimodel and model/persistmodel).
//
// By the way: ErrNotFound / ErrConflict in core are just aliases of storemodel anyway
// (core/core.go), so this uses the storemodel ones directly, with exactly the same meaning.

package coremem

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// ---------------------------------------------------------------------------
// In-memory implementation
// ---------------------------------------------------------------------------

// # Why it lands in its own coremem, instead of being stuffed into coredao
//
// "Move MemStore into coredao, in the same package as the PG implementation" was evaluated once, and the conclusion was not worth it. That evaluation
// was not wrong, but it computed the cost of **that landing spot**: coredao imports core, and 10
// `package core` tests construct MemStore directly, so those tests would have to become the external test package
// core_test; following the transitive closure of shared fixtures meant moving 14/37 test files and exporting 35 of core's
// unexported production identifiers (Service internals like parseSkillZip / resolveSymlink / sseData,
// not pure functions, so they could not be pushed down).
//
// With a separate coremem **that does not depend on core**, the cause of those 35 (tests moving to an external package)
// simply disappears: core's tests stay `package core` and just import one more leaf package.
// The measured cost is **0 new exports, 0 test files moved**.
//
// The lesson is recorded here: **the cost of "move or not" is a property of the landing spot, not of the thing being moved.**
// With a different landing spot, recompute; do not reuse the conclusion from the previous one.

// memData is all the in-memory state. It is pulled out on its own so Reset can swap it all at once.
type memData struct {
	workspaces []apimodel.Workspace
	agents     []apimodel.Agent
	skills     []apimodel.Skill
	// skillFiles is indexed by skillID and stores the files attached to skill packages (including content bytes).
	//
	// Indexed separately from skills, shaped like the separate PG table; both sides must behave the same,
	// otherwise unit tests (running the in-memory version) cannot see problems that only appear on PG.
	skillFiles map[string][]apimodel.SkillFile
	// issues are the workspace-level issue records; the slice keeps order (the list reads in reverse insertion order).
	//
	// A slice instead of a map: the list needs insertion order, and map iteration order is random,
	// which would cause "refresh and two rows swap", which looks like the data is jumping.
	issues []apimodel.Issue
	// issueComments and issueAttachments are indexed by issueID, shaped like the separate PG tables.
	//
	// Both sides must behave the same, otherwise unit tests (running the in-memory version) cannot see problems that only appear on PG;
	// SkillFilesRoundTrip once caught exactly this kind of difference (one side stored Size as given, the other recomputed it).
	issueComments map[string][]apimodel.IssueComment
	// issueAttachments only stores **metadata**: the bytes are in BlobStore (same as the PG version,
	// the attachment table has no content column).
	issueAttachments map[string][]apimodel.IssueAttachment
	// suggestions are **platform-level** product suggestions; the slice keeps order (same reason as issues).
	//
	// ⚠️ It is a different thing from issues: that one is indexed by workspace, this one **has no workspace dimension**.
	// Do not merge them into one generic container because the two look alike; the authorization rules are completely different
	// (see core/README-suggestion.md).
	suggestions []apimodel.Suggestion
	// Replies and attachment metadata are indexed by suggestionID; the bytes are in BlobStore, same as the issue group.
	suggestionReplies     map[string][]apimodel.SuggestionReply
	suggestionAttachments map[string][]apimodel.SuggestionAttachment
	// files is the metadata (bucket, key, size, hash) of the bytes in object storage, indexed by file id.
	// The bytes themselves are in BlobStore, same as on the PG side.
	files map[string]persistmodel.FileRecord
	runs  []apimodel.Run
	// rescueCounts matches the PG runs.rescue_count column. It does not go into apimodel.Run (that is an HTTP
	// contract type), so the in-memory version keeps it in a separate map.
	rescueCounts map[string]int
	events       map[string][]apimodel.RunEvent
	// arkSecrets is indexed by workspaceID. The plaintext key lives only here and never goes into any response.
	//
	// ⚠️ Rows with Source inherit **have no plaintext** (APIKey is an empty string): that workspace references the
	// one in globalArkSecrets below. Do not "casually add a copy" here:
	// that would break "changing the global one links through automatically", which is the whole point of the reference mechanism.
	arkSecrets map[string]apimodel.ArkSecret
	// globalArkSecrets is indexed by **employee email**; it is the account-level Ark credential.
	globalArkSecrets map[string]apimodel.GlobalArkSecret
	// larkUserTokens are Feishu user OAuth credentials, keyed by actor+"\x00"+larkAppID.
	//
	// ⚠️ **This layer is plaintext** (the PG side encrypts when storing). So under AMA_STORE=mem
	// tokens only live in process memory, and a process restart requires re-authorizing. That is the normal cost of the fallback,
	// not a defect.
	larkUserTokens map[string]persistmodel.LarkUserToken
	// larkAuthSessions is the device-code intermediate state of Feishu user authorization, keyed the same way.
	larkAuthSessions map[string]persistmodel.LarkAuthSession
	// larkUserAppPrefs is "which Feishu app I use by default", keyed by actor, with values like cli_xxx.
	//
	// ⚠️ **Account level, not per workspace**: injection picks separately by the run's workspace and does not read this.
	// See the comments on that group of methods on core.Store.
	larkUserAppPrefs map[string]string
	// agentArkSecrets are the agent-level Ark credentials, keyed by agentID.
	//
	// "No entry" means "follow the workspace setting", not "not configured"; see
	// the comment on apimodel.AgentArkCredential.
	agentArkSecrets map[string]apimodel.AgentArkSecret
	// onboarding is indexed by **employee email** and records "has this person seen the onboarding".
	//
	// The only map in the repo not indexed by workspaceID: onboarding state is account level,
	// unrelated to workspaces (see the onboarding.go file header).
	onboarding map[string]persistmodel.OnboardingRecord
	// userPrefs is indexed by **employee email** and records this person's language and appearance.
	//
	// Same kind as onboarding: account level, not indexed by workspaceID (see preferences.go).
	userPrefs map[string]persistmodel.UserPreferencesRecord
	// feedback is issue feedback; **the slice is insertion order**.
	//
	// A slice instead of a map: the read path of this domain is "page in reverse time order", which needs a stable order,
	// and map iteration is unordered (that would make pages repeat or miss records).
	//
	// dedupIndex separately stores "dedup key → index in the slice", so PutFeedback does not scan linearly.
	// It matches the PG unique index; the two sides must dedup the same way, otherwise unit tests (running the in-memory version)
	// cannot see problems that only appear on PG, and vice versa.
	feedback      []persistmodel.FeedbackRecord
	feedbackDedup map[string]int
	// arkBindings is indexed by workspaceID+agentID and records "AMA agent → Ark Agent/environment".
	arkBindings map[string]persistmodel.ArkBinding
	// arkBindingOrder records insertion order, so ListArkBindings returns a stable order (map iteration is unordered).
	arkBindingOrder []string
	// runSandboxes indexes "which container this Run uses" by runID.
	//
	// No order needed: it is only looked up by primary key and has no list endpoint (the frontend always "looks at the container of this one conversation";
	// there is no "list all containers" case).
	runSandboxes map[string]persistmodel.RunSandbox
	// arkAuthFailures records the start of consecutive Ark credential failures per environment id (same as PG's ark_auth_failures).
	arkAuthFailures map[string]memArkAuthFailure
	// terminalActive records "until when the webshell terminal is in use" per runID, used for "do not pause while in use".
	//
	// The in-memory implementation uses time.Now() (same as the auth session expiry in this file), while the PG side
	// deliberately lets **the database** compute the time, since wall clocks differ across instances. The in-memory implementation is only used for single-process
	// local development and tests, where that problem does not exist.
	terminalActive map[string]time.Time
	// sandboxActivity records the container's last activity time per runID (used by the reclaim sweep).
	sandboxActivity map[string]time.Time
	// arkSkillUploads is indexed by workspaceID+skillID+contentHash+keyFingerprint,
	// recording "this version of an AMA skill's content → Ark skill id". Shaped like the separate PG table.
	arkSkillUploads map[string]persistmodel.ArkSkillUpload
	// schedules are the scheduled tasks; the slice is insertion order (ListSchedules returns them in it).
	schedules []apimodel.Schedule
	// scheduleLeases records "who claimed this task and until when the lease lasts", indexed by scheduleID.
	//
	// Kept apart from Schedule instead of adding two fields: the lease is **the scheduler's runtime state** and should not appear in
	// HTTP responses (the frontend does not need to know which instance is running it). Same on the PG side: the lease is two separate
	// columns, not in doc.
	scheduleLeases map[string]scheduleLease

	// revisions are the entity history versions; the slice is write order.
	//
	// A flat slice instead of map[kind+id][]Revision: PurgeExcessRevisions does
	// "keep N per entity group", which needs a full pass with a map too, and the flat shape makes
	// "scan everything once" directly writable. Data volume is small in the in-memory implementation anyway.
	revisions []apimodel.Revision

	// runWatchLeases records "who is subscribed long-term to this conversation and until when the lease lasts", indexed by runID.
	//
	// Same structure as scheduleLeases, and likewise **not in doc**: it is runtime state and should not appear in
	// HTTP responses (on the PG side it matches the two real columns watch_by / watch_until of the runs table).
	runWatchLeases map[string]scheduleLease

	// pending is the queued messages waiting to be sent; see store_pending.go.
	pending []pendingRow
	// pendingOrd orders messages queued at the same instant, matching PG's bigserial.
	pendingOrd int64

	// agentSyncTasks is the ledger of "sync to Ark after saving an agent" tasks; see store_agentsync.go.
	agentSyncTasks []persistmodel.AgentSyncTaskRow
	// agentSyncOrd orders tasks queued at the same instant, matching PG's bigserial.
	agentSyncOrd int64

	// terminalTickets are the one-time entry tickets for the terminal WebSocket, keyed by ticket value.
	//
	// A map instead of a slice: redeeming locates exactly by ticket value (on the PG side it is the primary key),
	// and there is no "scan by range" need here except that one cleanup. See store_terminalticket.go.
	terminalTickets map[string]persistmodel.TerminalTicketRow

	// hosts / workspaceHosts / hostDispatches are the three host executor tables; see
	// service/host/store.go and store_host.go. All three use slices: listing and claiming depend on
	// a stable order (created_at,id / seq), and random map iteration would make the two sides sort differently.
	hosts          []persistmodel.Host
	workspaceHosts []persistmodel.WorkspaceHost
	hostDispatches []persistmodel.HostDispatch
	// hostDispatchSeq is each host's downstream command counter (matching PG's
	// hosts.last_dispatch_seq). Deliberately kept apart from the hostDispatches slice: purging all of a host's rows from the slice
	// must not wrap the sequence back, otherwise the daemon's last_seq cursor can never fetch new commands.
	hostDispatchSeq map[string]int64

	// userFiles is the account-level personal file table. A slice: the list depends on a stable (created_at,id) descending order.
	userFiles []persistmodel.UserFile
}

// scheduleLease is the claim lease of one task.
type scheduleLease struct {
	owner string
	until time.Time
}

// MemStore is the in-memory implementation of Store. Safe to share across goroutines.
type MemStore struct {
	mu   sync.RWMutex
	data memData

	// toolResults is the self_hosted tool-result ledger (see store_toolresult.go).
	// Not in memData/seed: a runtime ledger should not go into the seed. Lazily initialized.
	toolResults map[string]*memToolResult
}

// NewMemStore builds an empty in-memory Store.
func NewMemStore() *MemStore {
	return &MemStore{data: emptyData()}
}

// NewSeededMemStore builds an in-memory Store loaded with seed data.
//
// The seed is a mechanical export of the frontend's seed.ts (see scripts/export-seed/export.sh), so it is exactly
// the shape the frontend really sees. A parse failure returns an error instead of silently degrading to an empty store: a broken seed must be known right away.
func NewSeededMemStore() (*MemStore, error) {
	data, err := loadSeed()
	if err != nil {
		return nil, err
	}
	return &MemStore{data: data}, nil
}

func emptyData() memData {
	return memData{
		workspaces:       []apimodel.Workspace{},
		agents:           []apimodel.Agent{},
		skills:           []apimodel.Skill{},
		skillFiles:       map[string][]apimodel.SkillFile{},
		issues:           []apimodel.Issue{},
		issueComments:    map[string][]apimodel.IssueComment{},
		issueAttachments: map[string][]apimodel.IssueAttachment{},

		suggestions:           []apimodel.Suggestion{},
		suggestionReplies:     map[string][]apimodel.SuggestionReply{},
		suggestionAttachments: map[string][]apimodel.SuggestionAttachment{},

		files: map[string]persistmodel.FileRecord{},

		runs:             []apimodel.Run{},
		rescueCounts:     map[string]int{},
		events:           map[string][]apimodel.RunEvent{},
		arkSecrets:       map[string]apimodel.ArkSecret{},
		globalArkSecrets: map[string]apimodel.GlobalArkSecret{},
		larkUserTokens:   map[string]persistmodel.LarkUserToken{},
		larkUserAppPrefs: map[string]string{},
		larkAuthSessions: map[string]persistmodel.LarkAuthSession{},
		agentArkSecrets:  map[string]apimodel.AgentArkSecret{},
		onboarding:       map[string]persistmodel.OnboardingRecord{},
		userPrefs:        map[string]persistmodel.UserPreferencesRecord{},
		feedback:         []persistmodel.FeedbackRecord{},
		feedbackDedup:    map[string]int{},
		arkBindings:      map[string]persistmodel.ArkBinding{},
		arkBindingOrder:  []string{},
		runSandboxes:     map[string]persistmodel.RunSandbox{},
		arkAuthFailures:  map[string]memArkAuthFailure{},
		terminalActive:   map[string]time.Time{},
		sandboxActivity:  map[string]time.Time{},
		arkSkillUploads:  map[string]persistmodel.ArkSkillUpload{},
		schedules:        []apimodel.Schedule{},
		scheduleLeases:   map[string]scheduleLease{},
		runWatchLeases:   map[string]scheduleLease{},
		agentSyncTasks:   []persistmodel.AgentSyncTaskRow{},
		revisions:        []apimodel.Revision{},
		terminalTickets:  map[string]persistmodel.TerminalTicketRow{},

		hosts:           []persistmodel.Host{},
		workspaceHosts:  []persistmodel.WorkspaceHost{},
		hostDispatches:  []persistmodel.HostDispatch{},
		hostDispatchSeq: map[string]int64{},

		userFiles: []persistmodel.UserFile{},
	}
}

// -- Workspaces -------------------------------------------------------------------

func (s *MemStore) ListWorkspaces(_ context.Context) ([]apimodel.Workspace, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]apimodel.Workspace, 0, len(s.data.workspaces))
	for _, ws := range s.data.workspaces {
		out = append(out, cloneWorkspace(ws))
	}
	return out, nil
}

func (s *MemStore) GetWorkspace(_ context.Context, id string) (apimodel.Workspace, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, ws := range s.data.workspaces {
		if ws.ID == id {
			return cloneWorkspace(ws), nil
		}
	}
	return apimodel.Workspace{}, persistmodel.ErrNotFound
}

func (s *MemStore) GetWorkspaceByKey(_ context.Context, key string) (apimodel.Workspace, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, ws := range s.data.workspaces {
		if ws.Key == key {
			return cloneWorkspace(ws), nil
		}
	}
	return apimodel.Workspace{}, persistmodel.ErrNotFound
}

func (s *MemStore) KeyTaken(_ context.Context, key, exceptID string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.keyTakenLocked(key, exceptID), nil
}

func (s *MemStore) keyTakenLocked(key, exceptID string) bool {
	for _, ws := range s.data.workspaces {
		if ws.Key == key && ws.ID != exceptID {
			return true
		}
	}
	return false
}

func (s *MemStore) CreateWorkspace(_ context.Context, ws apimodel.Workspace) (apimodel.Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keyTakenLocked(ws.Key, "") {
		return apimodel.Workspace{}, fmt.Errorf("%w: 空间 Key「%s」已被占用", persistmodel.ErrConflict, ws.Key)
	}
	s.data.workspaces = append(s.data.workspaces, cloneWorkspace(ws))
	return cloneWorkspace(ws), nil
}

func (s *MemStore) SaveWorkspace(_ context.Context, ws apimodel.Workspace) (apimodel.Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, cur := range s.data.workspaces {
		if cur.ID != ws.ID {
			continue
		}
		// Check uniqueness only when key changed, excluding itself; otherwise every settings save would clash with itself.
		if ws.Key != cur.Key && s.keyTakenLocked(ws.Key, ws.ID) {
			return apimodel.Workspace{}, fmt.Errorf("%w: 空间 Key「%s」已被占用", persistmodel.ErrConflict, ws.Key)
		}
		s.data.workspaces[i] = cloneWorkspace(ws)
		return cloneWorkspace(ws), nil
	}
	return apimodel.Workspace{}, persistmodel.ErrNotFound
}

// DeleteWorkspace cascades cleanup: agents, skills, runs, the events of those runs, Ark credentials.
//
// It deliberately does **not** clean dangling references from agents in other workspaces, copying LocalApiClient's behavior
// (api.ts:270-278 does not clean them either). Not "improving" it here keeps the two clients behaving the same.
func (s *MemStore) DeleteWorkspace(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.data.workspaces = filterSlice(s.data.workspaces, func(ws apimodel.Workspace) bool {
		return ws.ID != id
	})
	s.data.agents = filterSlice(s.data.agents, func(a apimodel.Agent) bool {
		return a.WorkspaceID != id
	})
	s.data.skills = filterSlice(s.data.skills, func(sk apimodel.Skill) bool {
		if sk.WorkspaceID == id {
			// Attachments are cleaned along with skills. Missing them leaves a batch of orphan data that is never read and never deleted,
			// and the PG side deletes by workspace_id; if the two differ, unit tests
			// (running the in-memory version) cannot see this leak.
			delete(s.data.skillFiles, sk.ID)
		}
		return sk.WorkspaceID != id
	})
	// Collect the runs to delete first, then delete their events; otherwise once the runs are gone the keys cannot be found.
	for _, r := range s.data.runs {
		if r.WorkspaceID == id {
			delete(s.data.events, r.ID)
		}
	}
	s.data.runs = filterSlice(s.data.runs, func(r apimodel.Run) bool {
		return r.WorkspaceID != id
	})
	// The workspace is gone, so its credentials go too: keeping them is dangling data, and a secret that should not be kept.
	delete(s.data.arkSecrets, id)
	// Same for Ark resource bindings: with the workspace gone, those Ark Agent / environment mappings mean nothing.
	// (The resources on the Ark side are not deleted: there is no key here, and deleting a workspace should not call external services.)
	for _, key := range s.data.arkBindingOrder {
		if b, ok := s.data.arkBindings[key]; ok && b.WorkspaceID == id {
			delete(s.data.arkBindings, key)
		}
	}
	s.data.arkBindingOrder = filterSlice(s.data.arkBindingOrder, func(key string) bool {
		_, ok := s.data.arkBindings[key]
		return ok
	})
	// Scheduled tasks are deleted with the workspace: without the workspace they can never produce a result, and keeping them only fails once per cycle.
	for _, sch := range s.data.schedules {
		if sch.WorkspaceID == id {
			delete(s.data.scheduleLeases, sch.ID)
		}
	}
	s.data.schedules = filterSlice(s.data.schedules, func(sch apimodel.Schedule) bool {
		return sch.WorkspaceID != id
	})
	// Ark sync tasks are deleted with the workspace, for word-for-word the same reason as scheduled tasks (keeping them would make the sync executor hit Ark again and again
	// for agents that no longer exist). The PG side deletes them in DeleteWorkspace; the two must match.
	s.data.agentSyncTasks = filterSlice(s.data.agentSyncTasks,
		func(t persistmodel.AgentSyncTaskRow) bool { return t.WorkspaceID != id })
	// Issues are deleted with the workspace, and comments and attachments are cleaned along with them.
	//
	// **The attachments especially must not be missed**: they hold bytes, so missing them costs far more than elsewhere,
	// and it is completely invisible: no page ever shows orphan attachments, the database just keeps growing.
	// The PG side does three DELETEs by workspace_id; the two must match.
	for _, iss := range s.data.issues {
		if iss.WorkspaceID != id {
			continue
		}
		delete(s.data.issueComments, iss.ID)
		delete(s.data.issueAttachments, iss.ID)
	}
	s.data.issues = filterSlice(s.data.issues, func(iss apimodel.Issue) bool {
		return iss.WorkspaceID != id
	})
	return nil
}

// -- Agent ------------------------------------------------------------------

func (s *MemStore) ListAgents(_ context.Context, workspaceID string) ([]apimodel.Agent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]apimodel.Agent, 0)
	for _, a := range s.data.agents {
		if a.WorkspaceID == workspaceID {
			out = append(out, cloneAgent(a))
		}
	}
	return out, nil
}

func (s *MemStore) GetAgent(_ context.Context, id string) (apimodel.Agent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, a := range s.data.agents {
		if a.ID == id {
			return cloneAgent(a), nil
		}
	}
	return apimodel.Agent{}, persistmodel.ErrNotFound
}

func (s *MemStore) CreateAgent(_ context.Context, a apimodel.Agent) (apimodel.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.agents = append(s.data.agents, cloneAgent(a))
	return cloneAgent(a), nil
}

func (s *MemStore) SaveAgent(_ context.Context, a apimodel.Agent) (apimodel.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, cur := range s.data.agents {
		if cur.ID == a.ID {
			s.data.agents[i] = cloneAgent(a)
			return cloneAgent(a), nil
		}
	}
	return apimodel.Agent{}, persistmodel.ErrNotFound
}

func (s *MemStore) DeleteAgent(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.agents = filterSlice(s.data.agents, func(a apimodel.Agent) bool {
		return a.ID != id
	})
	// Its own Ark key must be deleted too; see PGStore.DeleteAgent for why (the point is "deleting and recreating
	// with the same id would silently inherit someone else's credential"). The two implementations must match.
	delete(s.data.agentArkSecrets, id)
	// Same for Ark sync tasks; see PGStore.DeleteAgent for why (the point is "an unfinished old task would block
	// a new agent with the same id from queueing"). The two implementations must match.
	s.data.agentSyncTasks = filterSlice(s.data.agentSyncTasks,
		func(t persistmodel.AgentSyncTaskRow) bool { return t.AgentID != id })
	return nil
}

// -- Skills -------------------------------------------------------------------

func (s *MemStore) ListSkills(_ context.Context, workspaceID string) ([]apimodel.Skill, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]apimodel.Skill, 0)
	for _, sk := range s.data.skills {
		if sk.WorkspaceID == workspaceID {
			out = append(out, cloneSkill(sk))
		}
	}
	return out, nil
}

func (s *MemStore) GetSkill(_ context.Context, id string) (apimodel.Skill, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sk := range s.data.skills {
		if sk.ID == id {
			return cloneSkill(sk), nil
		}
	}
	return apimodel.Skill{}, persistmodel.ErrNotFound
}

func (s *MemStore) SkillNameTaken(_ context.Context, workspaceID, name string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sk := range s.data.skills {
		if sk.WorkspaceID == workspaceID && sk.Name == name {
			return true, nil
		}
	}
	return false, nil
}

func (s *MemStore) CreateSkill(_ context.Context, sk apimodel.Skill) (apimodel.Skill, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.skills = append(s.data.skills, cloneSkill(sk))
	return cloneSkill(sk), nil
}

func (s *MemStore) SaveSkill(_ context.Context, sk apimodel.Skill) (apimodel.Skill, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, cur := range s.data.skills {
		if cur.ID == sk.ID {
			s.data.skills[i] = cloneSkill(sk)
			return cloneSkill(sk), nil
		}
	}
	return apimodel.Skill{}, persistmodel.ErrNotFound
}

func (s *MemStore) DeleteSkill(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.skills = filterSlice(s.data.skills, func(sk apimodel.Skill) bool {
		return sk.ID != id
	})
	// Attachments are deleted together, matching the two DELETEs on the PG side.
	delete(s.data.skillFiles, id)
	return nil
}

// PutSkillFiles replaces attachments entirely. workspaceID is not part of the index in the in-memory version (skillID is already unique);
// it is taken only to match the PG table structure, which uses it for cascading on workspace deletion.
func (s *MemStore) PutSkillFiles(
	_ context.Context, skillID, _ string, files []apimodel.SkillFile,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(files) == 0 {
		// Empty means clear. Keeping an empty slice instead of deleting the key would also work, but deleting is closer to PG
		// (there, no rows means no rows).
		delete(s.data.skillFiles, skillID)
		return nil
	}
	stored := make([]apimodel.SkillFile, 0, len(files))
	for _, f := range files {
		sf := cloneSkillFile(f)

		// ⚠️ **content is not kept**, same as the PG side: skill_files has no byte column; all the bytes are in
		// BlobStore, pointed to by file_id. If the in-memory version kept content, the guard in `hydrateSkillFiles`
		// "error when the bytes cannot be fetched" would never fire, and its tests run exactly on the in-memory version.
		sf.Content = nil

		// Size is always 0; **the caller's value is not trusted**.
		//
		// Same as the PG side: PGStore.SkillFiles only does `select path, file_id`,
		// and **the Store layer has no idea how big those bytes are** (it does not query the files table). The real Size is filled in by
		// `hydrateSkillFiles` from `files.size`, the only place holding both "the bytes" and
		// "the metadata". Keeping the caller's length here would make the in-memory version report the real value and the PG version 0:
		// the split of "tests all green, production wrong".
		sf.Size = 0

		stored = append(stored, sf)
	}
	sortSkillFiles(stored)
	s.data.skillFiles[skillID] = stored
	return nil
}

func (s *MemStore) SkillFiles(_ context.Context, skillID string) ([]apimodel.SkillFile, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Non-nil empty slice: the Store convention is "never return a nil slice" (see the start of this file).
	out := make([]apimodel.SkillFile, 0, len(s.data.skillFiles[skillID]))
	for _, f := range s.data.skillFiles[skillID] {
		out = append(out, cloneSkillFile(f))
	}
	return out, nil
}

// -- Ark credentials ---------------------------------------------------------------

func (s *MemStore) GetArkSecret(_ context.Context, workspaceID string) (apimodel.ArkSecret, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sec, ok := s.data.arkSecrets[workspaceID]
	if !ok {
		return apimodel.ArkSecret{}, persistmodel.ErrNotFound
	}
	return sec, nil
}

func (s *MemStore) PutArkSecret(_ context.Context, sec apimodel.ArkSecret) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.arkSecrets[sec.WorkspaceID] = sec
	return nil
}

func (s *MemStore) DeleteArkSecret(_ context.Context, workspaceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.arkSecrets, workspaceID)
	return nil
}

// -- Feishu user OAuth credentials -----------------------------------------------------
//
// No clone function: LarkUserToken has only scalars (string / time.Time), so a value copy is enough,
// for the same reason as ArkSecret.

// larkUserTokenKey builds the primary key. It is separated by \x00 instead of ":": Feishu app ids and emails never contain
// NUL, while a colon is a valid character in an email, and using it could make two different (actor, app) pairs collide into the same key.
func larkUserTokenKey(actor, larkAppID string) string {
	return actor + "\x00" + larkAppID
}

func (s *MemStore) GetLarkUserToken(
	_ context.Context, actor, larkAppID string,
) (persistmodel.LarkUserToken, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tok, ok := s.data.larkUserTokens[larkUserTokenKey(actor, larkAppID)]
	if !ok {
		return persistmodel.LarkUserToken{}, persistmodel.ErrNotFound
	}
	return tok, nil
}

func (s *MemStore) PutLarkUserToken(_ context.Context, tok persistmodel.LarkUserToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.larkUserTokens[larkUserTokenKey(tok.Actor, tok.LarkAppID)] = tok
	return nil
}

func (s *MemStore) DeleteLarkUserToken(_ context.Context, actor, larkAppID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.larkUserTokens, larkUserTokenKey(actor, larkAppID))
	return nil
}

// ListLarkUserTokens lists all of a person's credentials.
//
// ⚠️ **It must be sorted by lark_app_id**: map iteration order is random, and without sorting the order
// changes every time. Small tables do not show this problem (the order happens to be stable), but the contract requires stability;
// see the lesson that small tables hide a missing order by. The PG side has the matching order by too.
func (s *MemStore) ListLarkUserTokens(
	_ context.Context, actor string,
) ([]persistmodel.LarkUserToken, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []persistmodel.LarkUserToken{}
	for _, tok := range s.data.larkUserTokens {
		if tok.Actor == actor {
			out = append(out, tok)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LarkAppID < out[j].LarkAppID })
	return out, nil
}

// -- "Which Feishu app I use by default" -------------------------------------------------

// GetLarkUserAppPref gets the Feishu app id this person picked; returns ErrNotFound if never picked.
//
// **It returns ErrNotFound instead of an empty string**: the caller must tell "never picked" (take the first in default order)
// from "picked, but that app is gone" (also fall back to default order, but this row can be cleaned up along the way).
// An empty string would merge the two into one.
func (s *MemStore) GetLarkUserAppPref(_ context.Context, actor string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	appID, ok := s.data.larkUserAppPrefs[actor]
	if !ok || appID == "" {
		return "", persistmodel.ErrNotFound
	}
	return appID, nil
}

// PutLarkUserAppPref records the Feishu app this person picked (upsert).
func (s *MemStore) PutLarkUserAppPref(_ context.Context, actor, larkAppID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.larkUserAppPrefs[actor] = larkAppID
	return nil
}

// -- Account-level Ark credentials -----------------------------------------------------------
//
// No clone function: GlobalArkSecret has only scalars, so a value copy is enough (same as ArkSecret).

func (s *MemStore) GetGlobalArkSecret(_ context.Context, email string) (apimodel.GlobalArkSecret, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sec, ok := s.data.globalArkSecrets[email]
	if !ok {
		return apimodel.GlobalArkSecret{}, persistmodel.ErrNotFound
	}
	return sec, nil
}

func (s *MemStore) PutGlobalArkSecret(_ context.Context, sec apimodel.GlobalArkSecret) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.globalArkSecrets[sec.Email] = sec
	return nil
}

func (s *MemStore) DeleteGlobalArkSecret(_ context.Context, email string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.globalArkSecrets, email)
	return nil
}

// -- Agent-level Ark credentials -------------------------------------------------------
//
// No clone function: AgentArkSecret has only scalars, so a value copy is enough (same as GlobalArkSecret).

func (s *MemStore) GetAgentArkSecret(
	_ context.Context, agentID string,
) (apimodel.AgentArkSecret, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sec, ok := s.data.agentArkSecrets[agentID]
	if !ok {
		return apimodel.AgentArkSecret{}, persistmodel.ErrNotFound
	}
	return sec, nil
}

func (s *MemStore) PutAgentArkSecret(_ context.Context, sec apimodel.AgentArkSecret) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.agentArkSecrets[sec.AgentID] = sec
	return nil
}

func (s *MemStore) DeleteAgentArkSecret(_ context.Context, agentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.agentArkSecrets, agentID)
	return nil
}

// -- Onboarding ---------------------------------------------------------------
//
// No clone function: OnboardingRecord has only scalars (string, int, time.Time),
// so a value copy is enough. Same reason as GetArkSecret: a deep copy is needed only with slice or pointer fields,
// and there are none here.

func (s *MemStore) GetOnboarding(_ context.Context, email string) (persistmodel.OnboardingRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.data.onboarding[email]
	if !ok {
		return persistmodel.OnboardingRecord{}, persistmodel.ErrNotFound
	}
	return rec, nil
}

// PutOnboarding is the in-memory upsert; **the merge behavior must match PG's coalesce exactly**.
//
// If the two implementations differ here, "skip first, finish later" loses one time in memory but not on PG,
// and the shared store suite (hitting both implementations) would be half green, half red, while the first reaction would be
// "the PG environment is broken", when the real cause is that the two sides behave differently.
func (s *MemStore) PutOnboarding(_ context.Context, rec persistmodel.OnboardingRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if prev, ok := s.data.onboarding[rec.Email]; ok {
		// A zero-value timestamp in the input does not overwrite the stored value, equivalent to SQL's
		// `coalesce(excluded.x, user_onboarding.x)`.
		if rec.CompletedAt.IsZero() {
			rec.CompletedAt = prev.CompletedAt
		}
		if rec.SkippedAt.IsZero() {
			rec.SkippedAt = prev.SkippedAt
		}
	}
	s.data.onboarding[rec.Email] = rec
	return nil
}

// -- User preferences ---------------------------------------------------------------
//
// No clone function: UserPreferencesRecord has only scalars, so a value copy is enough (same as the previous group).

func (s *MemStore) GetUserPreferences(_ context.Context, email string) (persistmodel.UserPreferencesRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.data.userPrefs[email]
	if !ok {
		return persistmodel.UserPreferencesRecord{}, persistmodel.ErrNotFound
	}
	return rec, nil
}

// PutUserPreferences is the in-memory upsert.
//
// **It overwrites the whole row, without the merge of PutOnboarding**, matching the PG side's
// `do update set language = excluded.language, ...`.
// Casually adding a coalesce "to be safe" would lose "changing from dark back to follow system",
// because under merge semantics that change looks like "no new value provided".
func (s *MemStore) PutUserPreferences(_ context.Context, rec persistmodel.UserPreferencesRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.userPrefs[rec.Email] = rec
	return nil
}

// -- Issue feedback ---------------------------------------------------------------

// PutFeedback stores one piece of feedback; **the dedup behavior must match that PG unique index exactly**.
//
// The consequence of the two implementations differing here is especially hidden: unit tests run the in-memory version, so the "rate limiting works" assertion
// is green, while production runs the PG version. And vice versa. So the branches here must match one to one with
// `on conflict (dedup_key) do update` in store_pg.go.
func (s *MemStore) PutFeedback(_ context.Context, rec persistmodel.FeedbackRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// An empty DedupKey matches PG's NULL: not deduped, a new row every time.
	// Level two (submitted by the user) takes this path: clicking twice should give two rows.
	if rec.DedupKey != "" {
		if i, ok := s.data.feedbackDedup[rec.DedupKey]; ok {
			// Collided: **only add to the count and advance last_seen_at**; other fields keep the first one.
			//
			// Keeping the first created_at and payload is deliberate: that is the scene of this error's first appearance,
			// more valuable than the 300th (the first often still carries the context that triggered it).
			prev := s.data.feedback[i]
			prev.Occurrences++
			prev.LastSeenAt = rec.LastSeenAt
			s.data.feedback[i] = prev
			return nil
		}
		s.data.feedbackDedup[rec.DedupKey] = len(s.data.feedback)
	}
	s.data.feedback = append(s.data.feedback, cloneFeedback(rec))
	return nil
}

// ListFeedback lists feedback by conditions, returning this page and the total.
func (s *MemStore) ListFeedback(_ context.Context, q persistmodel.FeedbackQuery) ([]persistmodel.FeedbackRecord, int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	matched := make([]persistmodel.FeedbackRecord, 0, len(s.data.feedback))
	for _, rec := range s.data.feedback {
		if !feedbackMatches(rec, q) {
			continue
		}
		matched = append(matched, cloneFeedback(rec))
	}

	// created_at descending, id as tie-breaker. **It must match PG's order by**, otherwise the same data
	// pages differently under the two implementations, and that error never shows with small data.
	sort.SliceStable(matched, func(i, j int) bool {
		if !matched[i].CreatedAt.Equal(matched[j].CreatedAt) {
			return matched[i].CreatedAt.After(matched[j].CreatedAt)
		}
		return matched[i].ID > matched[j].ID
	})

	total := len(matched)
	// Paging comes **after** sorting: cutting first and then sorting gives a page in random order.
	if q.Offset >= total {
		return []persistmodel.FeedbackRecord{}, total, nil
	}
	end := q.Offset + q.Limit
	if q.Limit <= 0 || end > total {
		end = total
	}
	return matched[q.Offset:end], total, nil
}

// feedbackMatches reports whether a piece of feedback meets the filter conditions.
//
// It is pulled out so the two implementations have one **readable** authoritative definition (the PG side is the SQL where;
// the shapes cannot share code, but the rules must match).
func feedbackMatches(rec persistmodel.FeedbackRecord, q persistmodel.FeedbackQuery) bool {
	if q.Level != "" && rec.Level != q.Level {
		return false
	}
	if q.ErrorCode != "" && rec.ErrorCode != q.ErrorCode {
		return false
	}
	// The time window is half-open [Since, Until). Compare fixed-length ISO strings, same as ListRunsForUsage:
	// for that shape lexical order equals time order, so string comparison is time comparison.
	if q.Since != "" && apimodel.FormatISO(rec.CreatedAt) < q.Since {
		return false
	}
	if q.Until != "" && apimodel.FormatISO(rec.CreatedAt) >= q.Until {
		return false
	}
	return true
}

// -- Run history ---------------------------------------------------------------

// ListRuns lists runs by workspace. Filtering and paging **match PG case by case**; see
// coredao.PGStore.ListRuns and persistmodel.RunQuery.
func (s *MemStore) ListRuns(_ context.Context, workspaceID string, q persistmodel.RunQuery) ([]apimodel.Run, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	statusSet := make(map[apimodel.RunStatus]struct{}, len(q.Status))
	for _, st := range q.Status {
		statusSet[st] = struct{}{}
	}
	triggerSet := make(map[apimodel.RunTrigger]struct{}, len(q.Trigger))
	for _, tg := range q.Trigger {
		triggerSet[tg] = struct{}{}
	}
	needle := strings.ToLower(q.Search)

	out := make([]apimodel.Run, 0)
	for _, r := range s.data.runs {
		if r.WorkspaceID != workspaceID {
			continue
		}
		if q.AgentID != "" && r.AgentID != q.AgentID {
			continue
		}
		if q.ScheduleID != "" && r.ScheduleID != q.ScheduleID {
			continue
		}
		if len(statusSet) > 0 {
			if _, ok := statusSet[r.Status]; !ok {
				continue
			}
		}
		// Trigger type allowlist, matching PG's `doc->>'trigger' = any($n)`.
		if len(triggerSet) > 0 {
			if _, ok := triggerSet[r.Trigger]; !ok {
				continue
			}
		}
		// All three fields must match; searching only the title would be a silent behavior change (api.ts:594-598).
		if needle != "" && !strings.Contains(strings.ToLower(r.Title), needle) &&
			!strings.Contains(strings.ToLower(r.AgentName), needle) &&
			!strings.Contains(strings.ToLower(r.ID), needle) {
			continue
		}
		out = append(out, cloneRun(r))
	}
	// Most recently active first. Descending by **string**, same as the frontend's localeCompare: timestamps have
	// the fixed-length .000Z shape, so lexical order equals time order.
	//
	// The sort key is lastActivityAt, not startedAt: Feishu reuses the same conversation, and a new turn only appends events
	// without changing startedAt; sorting by startedAt would leave a conversation that just spoke at the bottom of the list.
	sort.SliceStable(out, func(i, j int) bool {
		return persistmodel.LastActivityOf(out[i]) > persistmodel.LastActivityOf(out[j])
	})

	// Paging comes **after** sorting: cutting first and then sorting gives a page in random order (same as in ListFeedback).
	// Limit == 0 means no limit, matching "no limit clause generated" on the PG side.
	if q.Offset >= len(out) {
		return []apimodel.Run{}, nil
	}
	if q.Offset > 0 {
		out = out[q.Offset:]
	}
	if q.Limit > 0 && q.Limit < len(out) {
		out = out[:q.Limit]
	}
	return out, nil
}

func (s *MemStore) ListRunsForUsage(_ context.Context, workspaceID string, q persistmodel.UsageQuery) ([]apimodel.Run, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]apimodel.Run, 0)
	for _, r := range s.data.runs {
		if r.WorkspaceID != workspaceID {
			continue
		}
		// The time window uses **string comparison**, matching started_at >= $n on the PG side exactly.
		// Timestamps are fixed-length .000Z, and lexical order equals time order, so there is no need to parse into time.Time;
		// parsing and then comparing would introduce time zone/precision differences between the two implementations.
		if q.Since != "" && r.StartedAt < q.Since {
			continue
		}
		if q.Until != "" && r.StartedAt >= q.Until {
			continue
		}
		out = append(out, cloneRun(r))
	}
	// Same order as ListRuns (most recent first). The aggregation itself does not depend on order, but the two implementations must match,
	// otherwise the store contract tests would fail on PG because of a different row order.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].StartedAt > out[j].StartedAt
	})
	return out, nil
}

// ListActiveRuns lists the conversations still running in all workspaces. See the notes on the core.Store interface.
//
// **The status allowlist matches the PG one exactly** (running / rescheduling / requires_action).
// If the two differ, "unit tests on MemStore are all green while production on PG behaves differently",
// and such drift always gives zero errors, the same situation as the AppendRunEvent dedup case.
func (s *MemStore) ListActiveRuns(_ context.Context) ([]apimodel.Run, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]apimodel.Run, 0)
	for _, r := range s.data.runs {
		switch r.Status {
		case apimodel.RunRunning, apimodel.RunRescheduling, apimodel.RunRequiresAction:
			out = append(out, cloneRun(r))
		}
	}
	// Matches PG's `order by started_at, id` exactly: **those that started first are subscribed first**.
	// The order must be stable, otherwise the contract tests would fail on PG because of a different row order
	// (and on a small table a sequential scan happens to equal insertion order, so a missing sort would not show).
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].StartedAt != out[j].StartedAt {
			return out[i].StartedAt < out[j].StartedAt
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// ClaimRunWatch atomically claims "this conversation is subscribed long-term by me". See the notes on the core.Store interface.
//
// **The rules match the PG one exactly**: same status allowlist, an expired lease can be taken. If the two drift, it shows as
// "unit tests on MemStore all green while production on PG differs", always with zero errors.
func (s *MemStore) ClaimRunWatch(
	_ context.Context, runID, owner string, now time.Time, leaseFor time.Duration,
) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, r := range s.data.runs {
		if r.ID != runID {
			continue
		}
		switch r.Status {
		case apimodel.RunRunning, apimodel.RunRescheduling, apimodel.RunRequiresAction:
		default:
			// Final states are not subscribed: an idle conversation has no events to receive, and staying attached only wastes a connection.
			return false, nil
		}
		// Someone is subscribed and the lease has not expired → cannot take it. If expired it can be taken over (the original owner may be dead).
		if lease, ok := s.data.runWatchLeases[runID]; ok && lease.until.After(now) {
			return false, nil
		}
		s.data.runWatchLeases[runID] = scheduleLease{owner: owner, until: now.Add(leaseFor)}
		return true, nil
	}
	// run missing: cannot be claimed, but not an error (it may have just been deleted).
	return false, nil
}

// RunWatchLeaseHeld asks "is this conversation's subscription lease still held by someone right now".
//
// **For tests only** (the PG side has no matching method and does not need one): when checking "the lease was released after unsubscribing",
// the indirect path "try to claim again" is disturbed by the rule "final states cannot be claimed"; whether released or not
// it returns false, the assertion always holds, a false green. So read the lease itself directly.
//
// Expiry is not checked: the caller cares whether "this record still exists", and releasing means deleting.
func (s *MemStore) RunWatchLeaseHeld(runID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.data.runWatchLeases[runID]
	return ok
}

// ReleaseRunWatch clears the claim lease of the long-term subscription. See the notes on the core.Store interface.
func (s *MemStore) ReleaseRunWatch(_ context.Context, runID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.runWatchLeases, runID)
	return nil
}

// IncrRunRescueCount: see the notes on the core.Store interface. The in-memory implementation is only for local/tests.
func (s *MemStore) IncrRunRescueCount(_ context.Context, runID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	for _, r := range s.data.runs {
		if r.ID == runID {
			found = true
			break
		}
	}
	if !found {
		return 0, persistmodel.ErrNotFound
	}
	s.data.rescueCounts[runID]++
	return s.data.rescueCounts[runID], nil
}

// isStaleActiveRun is the rule for "a stuck active conversation", matching PG's staleActiveRunPredicate
// exactly.
//
// **If the two drift, it shows as "all green on mem, missed in production"** (or the other way around), with zero errors,
// so the three details here must follow that SQL: the same status allowlist, LastActivityOf falling back to
// StartedAt, and a strict less-than comparison.
func isStaleActiveRun(r apimodel.Run, staleBefore string) bool {
	switch r.Status {
	case apimodel.RunRunning, apimodel.RunRescheduling, apimodel.RunRequiresAction:
	default:
		return false
	}
	return persistmodel.LastActivityOf(r) < staleBefore
}

// ListStaleActiveRuns lists conversations with no activity for a long time but a status still active. See the core.Store interface.
//
// Ordering matches PG: by activity time ascending, id as tie-breaker, **the oldest handled first**.
// The order must be stable; otherwise on a small table a sequential scan happens to equal insertion order, and a missing sort would not show.
func (s *MemStore) ListStaleActiveRuns(_ context.Context, staleBefore string) ([]apimodel.Run, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]apimodel.Run, 0)
	for _, r := range s.data.runs {
		if isStaleActiveRun(r, staleBefore) {
			out = append(out, cloneRun(r))
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		li, lj := persistmodel.LastActivityOf(out[i]), persistmodel.LastActivityOf(out[j])
		if li != lj {
			return li < lj
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// RetireStaleRun pushes a stuck conversation to a final state. See the notes on the core.Store interface.
//
// **The check must be redone while holding the lock**; the snapshot from when the caller listed it cannot be trusted: a late
// tool_result may have pushed the run back to running in between, and overwriting would kill a conversation that just came back to life.
// This is the in-memory version of "repeat the rule in the UPDATE's WHERE" on the PG side.
func (s *MemStore) RetireStaleRun(
	_ context.Context, run apimodel.Run, staleBefore string,
) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, cur := range s.data.runs {
		if cur.ID != run.ID {
			continue
		}
		if !isStaleActiveRun(cur, staleBefore) {
			return false, nil
		}
		s.data.runs[i] = cloneRun(run)
		return true, nil
	}
	return false, nil
}

func (s *MemStore) GetRun(_ context.Context, id string) (apimodel.Run, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.data.runs {
		if r.ID == id {
			return cloneRun(r), nil
		}
	}
	return apimodel.Run{}, persistmodel.ErrNotFound
}

func (s *MemStore) ListRunEvents(_ context.Context, runID string, q persistmodel.EventQuery) ([]apimodel.RunEvent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	typeSet := make(map[string]struct{}, len(q.Types))
	for _, t := range q.Types {
		typeSet[t] = struct{}{}
	}

	// A missing run gives an empty slice, not ErrNotFound; see the interface comment.
	out := make([]apimodel.RunEvent, 0)
	for _, e := range s.data.events[runID] {
		if len(typeSet) > 0 {
			if _, ok := typeSet[e.Type]; !ok {
				continue
			}
		}
		if q.ThreadID != "" && (e.ThreadID == nil || *e.ThreadID != q.ThreadID) {
			continue
		}
		out = append(out, cloneEvent(e))
	}
	// Ark's event list defaults to asc (old to new); keep it the same.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Sequence < out[j].Sequence
	})
	return out, nil
}

// CreateRun stores a new run.
//
// Like CreateAgent it is a plain append: a run has no unique constraint other than the primary key, and the ID is generated by the Service.
func (s *MemStore) CreateRun(_ context.Context, r apimodel.Run) (apimodel.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.runs = append(s.data.runs, cloneRun(r))
	// Reserve a slot in the event table first: ListRunEvents reads this map, and without it there is no way to tell it from "run does not exist".
	if _, ok := s.data.events[r.ID]; !ok {
		s.data.events[r.ID] = []apimodel.RunEvent{}
	}
	return cloneRun(r), nil
}

func (s *MemStore) SaveRun(_ context.Context, r apimodel.Run) (apimodel.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, cur := range s.data.runs {
		if cur.ID == r.ID {
			s.data.runs[i] = cloneRun(r)
			return cloneRun(r), nil
		}
	}
	return apimodel.Run{}, persistmodel.ErrNotFound
}

// AppendRunEvent appends one event, with sequence number "current max +1".
//
// Assigning the number and writing happen under one lock, so concurrent appends never clash; that is exactly why the number is handed out by
// the Store instead of passed by the caller (see the interface comment).
//
// **When the same event (same id) is sent again, the existing row is returned and nothing is appended.** This must match the behavior PGStore
// gets from the unique index run_events_event_id_idx: one Ark session can have several subscribers at once,
// and each writes the frames it receives into the database (see persistArkEvent in core/chat.go).
// If the two behave differently, unit tests on MemStore are all green while production on PG gets duplicates, or the other way around,
// worse: PG fixed while the in-memory version keeps piling up duplicates, and the problem comes back as soon as `AMA_STORE=mem` is used.
//
// Frames with an empty id (the undecodable kind) are not deduped and are appended one by one as usual: they cannot be deduped anyway.
func (s *MemStore) AppendRunEvent(
	_ context.Context, runID string, raw []byte,
) (apimodel.RunEvent, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := 1
	for _, e := range s.data.events[runID] {
		if e.Sequence >= next {
			next = e.Sequence + 1
		}
	}
	ev := apimodel.NewRunEventFromArk(raw, next)
	// Frames with an empty id (the undecodable kind) are not deduped: they cannot be, and storing them one by one is better than swallowing them.
	if ev.EventID != "" {
		for _, e := range s.data.events[runID] {
			if e.EventID == ev.EventID {
				// Already there: created=false, so the caller knows not to trigger external side effects again.
				return cloneEvent(e), false, nil
			}
		}
	}
	s.data.events[runID] = append(s.data.events[runID], cloneEvent(ev))
	return cloneEvent(ev), true, nil
}

// -- Ark resource bindings -----------------------------------------------------------

func (s *MemStore) GetArkBinding(_ context.Context, workspaceID, agentID string) (persistmodel.ArkBinding, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.data.arkBindings[bindingKey(workspaceID, agentID)]
	if !ok {
		return persistmodel.ArkBinding{}, persistmodel.ErrNotFound
	}
	return b, nil
}

func (s *MemStore) PutArkBinding(_ context.Context, b persistmodel.ArkBinding) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := bindingKey(b.WorkspaceID, b.AgentID)
	// Record the insertion order, so ListArkBindings returns a stable order (the PG side relies on the ord column).
	if _, exists := s.data.arkBindings[key]; !exists {
		s.data.arkBindingOrder = append(s.data.arkBindingOrder, key)
	}
	s.data.arkBindings[key] = b
	return nil
}

func (s *MemStore) ListArkBindings(_ context.Context, workspaceID string) ([]persistmodel.ArkBinding, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]persistmodel.ArkBinding, 0)
	for _, key := range s.data.arkBindingOrder {
		if b, ok := s.data.arkBindings[key]; ok && b.WorkspaceID == workspaceID {
			out = append(out, b)
		}
	}
	return out, nil
}

// bindingKey joins "workspace + agent" into a map key.
//
// It is separated by \x00 instead of other characters: IDs are opaque strings and could in theory contain anything,
// but never NUL. With a separator like ":", two joined parts could collide with another pair.
func bindingKey(workspaceID, agentID string) string {
	return workspaceID + "\x00" + agentID
}

func (s *MemStore) DeleteArkBinding(_ context.Context, workspaceID, agentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := bindingKey(workspaceID, agentID)
	delete(s.data.arkBindings, key)
	s.data.arkBindingOrder = filterSlice(s.data.arkBindingOrder, func(k string) bool {
		return k != key
	})
	return nil
}

// RepointArkEnvironment means the same as that PG method: a targeted repoint of bindings still pointing at the old environment,
// returning how many rows changed. Under the lock only the matching rows change, and the order is unchanged.
func (s *MemStore) RepointArkEnvironment(
	_ context.Context, workspaceID, oldEnvID, newEnvID string,
) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, key := range s.data.arkBindingOrder {
		b, ok := s.data.arkBindings[key]
		if !ok || b.WorkspaceID != workspaceID || b.ArkEnvironmentID != oldEnvID {
			continue
		}
		b.ArkEnvironmentID = newEnvID
		s.data.arkBindings[key] = b
		n++
	}
	return n, nil
}

// -- Start of consecutive Ark credential failures ---------------------------------------------------

// memArkAuthFailure is the in-memory shape of one ark_auth_failures row.
type memArkAuthFailure struct {
	fingerprint string
	first       time.Time
}

// RecordArkAuthFailure follows the same rule as the PG side: when the fingerprint changes, the clock starts over.
func (s *MemStore) RecordArkAuthFailure(
	_ context.Context, environmentID, _ /* workspaceID */, fingerprint string,
) (time.Duration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	rec, ok := s.data.arkAuthFailures[environmentID]
	if !ok || rec.fingerprint != fingerprint {
		rec = memArkAuthFailure{fingerprint: fingerprint, first: now}
		s.data.arkAuthFailures[environmentID] = rec
	}
	return now.Sub(rec.first), nil
}

func (s *MemStore) ClearArkAuthFailure(_ context.Context, environmentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.arkAuthFailures, environmentID)
	return nil
}

// -- Session containers (Docker) ------------------------------------------------------

func (s *MemStore) GetRunSandbox(_ context.Context, runID string) (persistmodel.RunSandbox, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.data.runSandboxes[runID]
	if !ok {
		return persistmodel.RunSandbox{}, persistmodel.ErrNotFound
	}
	return rec, nil
}

// PutRunSandbox overwrites. **It overwrites, it does not insert**: when a container is rebuilt after being reclaimed, the same Run gets a
// new container id, and insert semantics would leave the old id that no longer exists in the table (see the interface comment).
func (s *MemStore) PutRunSandbox(_ context.Context, rec persistmodel.RunSandbox) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.runSandboxes[rec.RunID] = rec
	return nil
}

func (s *MemStore) DeleteRunSandbox(_ context.Context, runID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.runSandboxes, runID)
	delete(s.data.terminalActive, runID)
	delete(s.data.sandboxActivity, runID)
	return nil
}

// TouchRunSandboxTerminal records "until when this container's terminal is in use".
//
// When the record does not exist it **does nothing**, same as the PG side (there an UPDATE affects 0 rows).
// This must not become "create one along the way": with no container there is nothing to protect, and creating a record out of nothing would make
// a later GetRunSandbox return an incomplete record with an empty session_id.
func (s *MemStore) TouchRunSandboxTerminal(
	_ context.Context, runID string, ttl time.Duration,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data.runSandboxes[runID]; !ok {
		return nil
	}
	s.data.terminalActive[runID] = time.Now().Add(ttl)
	return nil
}

// TerminalActiveSessions returns the names of containers whose terminal is still in use.
func (s *MemStore) TerminalActiveSessions(_ context.Context) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	var out []string
	for runID, until := range s.data.terminalActive {
		if !until.After(now) {
			continue
		}
		// If the container record was deleted (DeleteRunSandbox cleans it too, but just in case), skip it:
		// there is no session_id to return, and putting an empty string in would make the caller compare against it,
		// while an empty string may happen to equal some uninitialized field.
		if rec, ok := s.data.runSandboxes[runID]; ok && rec.SessionID != "" {
			out = append(out, rec.SessionID)
		}
	}
	return out, nil
}

// TouchRunSandboxActivity records the container's last activity time. Does nothing when the record does not exist (same as PG's UPDATE of 0 rows).
func (s *MemStore) TouchRunSandboxActivity(_ context.Context, runID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data.runSandboxes[runID]; !ok {
		return nil
	}
	s.data.sandboxActivity[runID] = time.Now()
	return nil
}

// RunSandboxActivity fetches last activity times in bulk (those that never recorded activity fall back to the creation time).
func (s *MemStore) RunSandboxActivity(_ context.Context, runIDs []string) (map[string]time.Time, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]time.Time, len(runIDs))
	for _, id := range runIDs {
		rec, ok := s.data.runSandboxes[id]
		if !ok {
			continue
		}
		if t, ok := s.data.sandboxActivity[id]; ok {
			out[id] = t
		} else {
			out[id] = rec.CreatedAt
		}
	}
	return out, nil
}

// -- Ark skill upload records -------------------------------------------------------

func (s *MemStore) GetArkSkillUpload(
	_ context.Context, workspaceID, skillID, contentHash, keyFingerprint string,
) (persistmodel.ArkSkillUpload, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.data.arkSkillUploads[uploadKey(workspaceID, skillID, contentHash, keyFingerprint)]
	if !ok {
		return persistmodel.ArkSkillUpload{}, persistmodel.ErrNotFound
	}
	return u, nil
}

// PutArkSkillUpload records an upload result; **if one exists, the existing one is kept**.
//
// "First come, first served" instead of overwriting, matching `on conflict do nothing` + read back on the PG side.
// Overwrite semantics would make two instances keep changing the same row under multiple instances, with the skill_id it points to flapping,
// while the caller (ensureArkSkills) depends exactly on "the same content always resolves to the same Ark skill".
func (s *MemStore) PutArkSkillUpload(_ context.Context, u persistmodel.ArkSkillUpload) (persistmodel.ArkSkillUpload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := uploadKey(u.WorkspaceID, u.SkillID, u.ContentHash, u.KeyFingerprint)
	if existing, ok := s.data.arkSkillUploads[key]; ok {
		return existing, nil
	}
	s.data.arkSkillUploads[key] = u
	return u, nil
}

// uploadKey builds the composite key of an upload record.
//
// It is separated by \x00 instead of underscores: skill ids and fingerprints are opaque strings,
// and printable separators could in theory collide (same reason as bindingKey).
func uploadKey(workspaceID, skillID, contentHash, keyFingerprint string) string {
	return workspaceID + "\x00" + skillID + "\x00" + contentHash + "\x00" + keyFingerprint
}

func (s *MemStore) AgentsBoundTo(_ context.Context, feishuAppID string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0)
	for _, a := range s.data.agents {
		for _, id := range a.FeishuAppIDs {
			if id == feishuAppID {
				out = append(out, a.ID)
				break
			}
		}
	}
	return out, nil
}

// -- Scheduled tasks ---------------------------------------------------------------

func (s *MemStore) ListSchedules(_ context.Context, workspaceID string) ([]apimodel.Schedule, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]apimodel.Schedule, 0)
	for _, sch := range s.data.schedules {
		if sch.WorkspaceID == workspaceID {
			out = append(out, cloneSchedule(sch))
		}
	}
	return out, nil
}

func (s *MemStore) GetSchedule(_ context.Context, id string) (apimodel.Schedule, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sch := range s.data.schedules {
		if sch.ID == id {
			return cloneSchedule(sch), nil
		}
	}
	return apimodel.Schedule{}, persistmodel.ErrNotFound
}

func (s *MemStore) CreateSchedule(_ context.Context, sch apimodel.Schedule) (apimodel.Schedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.schedules = append(s.data.schedules, cloneSchedule(sch))
	return cloneSchedule(sch), nil
}

func (s *MemStore) SaveSchedule(_ context.Context, sch apimodel.Schedule) (apimodel.Schedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, cur := range s.data.schedules {
		if cur.ID == sch.ID {
			s.data.schedules[i] = cloneSchedule(sch)
			return cloneSchedule(sch), nil
		}
	}
	return apimodel.Schedule{}, persistmodel.ErrNotFound
}

func (s *MemStore) DeleteSchedule(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.schedules = filterSlice(s.data.schedules, func(sch apimodel.Schedule) bool {
		return sch.ID != id
	})
	// The lease is deleted along with it, otherwise if this ID were used again (it will not be, but) it would carry a stale lease.
	delete(s.data.scheduleLeases, id)
	return nil
}

// ClaimDueSchedules claims the tasks that are due.
//
// **The whole thing runs under one write lock**, which is exactly the difference between "claim" and "check then change": in two steps, two
// goroutines would each find the same due task and both think they claimed it. The PG side gets the same effect with a single
// `update ... where`.
//
// The behavior matches the PG implementation case by case (the same assertions run on both in TestStoreSuite):
// only "active or try-run clicked" counts, meeting one of two branches: **normally due** (next_run_at non-empty,
// due, no unexpired lease), or **zombie reclaim** (next_run_at empty but the lease expired,
// meaning the process that claimed it died). The background of the second branch is in the long comment on the store_pg_schedule.go side.
func (s *MemStore) ClaimDueSchedules(
	_ context.Context, owner string, now time.Time, leaseFor time.Duration, limit int,
) ([]apimodel.Schedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]apimodel.Schedule, 0)
	for i := range s.data.schedules {
		if limit > 0 && len(out) >= limit {
			break
		}
		sch := &s.data.schedules[i]
		// **"Or try-run was clicked" cannot be dropped, nor loosened to only checking NextRunAt.** Matches the PG side's
		// `(status = 'active' or run_now_requested)` exactly:
		//   - only accepting active → run-now on paused returns 200 with zero effect (clicked and nothing happened);
		//   - deleting the status condition entirely → every task runs once **the moment it is paused**,
		//     because the NextRunAt left over from before pausing is still in the past.
		if sch.Status != apimodel.ScheduleActive && !sch.RunNowRequested {
			continue
		}
		// Lease: having an entry is equivalent to claimed_by / claimed_until being non-empty on the PG side.
		lease, leased := s.data.scheduleLeases[sch.ID]
		if sch.NextRunAt == "" {
			// **Zombie reclaim**, matching the second PG branch case by case: NextRunAt is empty **and** there is an
			// expired lease → the process that claimed it died, the write-back will never come, anyone can take over.
			//
			// The "has a lease" condition cannot be dropped (it matches PG's `claimed_by is not null`):
			// without it, tasks that "were never claimed and never scheduled" would be claimed by mistake.
			// The full background is in the comment on ClaimDueSchedules in store_pg_schedule.go.
			if !leased || lease.until.After(now) {
				continue
			}
		} else {
			// The time window uses **string comparison**, matching `next_run_at <= $n` on the PG side exactly:
			// timestamps are fixed-length .000Z, and lexical order equals time order. Parsing into time.Time and comparing would introduce time zone/precision differences
			// between the two implementations (the same trade-off as ListRunsForUsage).
			if sch.NextRunAt > apimodel.FormatISO(now) {
				continue
			}
			// Someone is running it and the lease has not expired → skip. If the lease expired it can be taken over (the original owner may be dead).
			if leased && lease.until.After(now) {
				continue
			}
		}
		s.data.scheduleLeases[sch.ID] = scheduleLease{owner: owner, until: now.Add(leaseFor)}
		// Clear next_run_at: see the interface comment; otherwise it would be claimed repeatedly during one fire.
		sch.NextRunAt = ""
		// The try-run mark is one-time and consumed on claim (same as `run_now_requested = false` on the PG side).
		sch.RunNowRequested = false
		out = append(out, cloneSchedule(*sch))
	}
	return out, nil
}

// ReleaseScheduleClaim clears the claim lease; see the interface comment.
func (s *MemStore) ReleaseScheduleClaim(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.scheduleLeases, id)
	return nil
}

func (s *MemStore) SchedulesForAgent(_ context.Context, agentID string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0)
	for _, sch := range s.data.schedules {
		if sch.AgentID == agentID {
			out = append(out, sch.ID)
		}
	}
	return out, nil
}

// -- For e2e -----------------------------------------------------------------

func (s *MemStore) Reset(_ context.Context) error {
	data, err := loadSeed()
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = data
	return nil
}

func (s *MemStore) Clear(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = emptyData()
	return nil
}

// ---------------------------------------------------------------------------
// Deep copy
// ---------------------------------------------------------------------------
//
// The deep copy helpers moved to model/persistmodel (both Store implementations need them, and they only depend on domain).
//
// A set of package-level variables with the same names is kept here, so the ~200 call sites in this file stay unchanged.
// The invariants they keep are described there: **slices are copied and stay non-nil** (nil encodes as null and the frontend's .map() blows up),
// plus the two deliberate exceptions Skill.Files / SkillFile.Content where "nil stays nil".
var (
	cloneStrings   = persistmodel.CloneStrings
	cloneWorkspace = persistmodel.CloneWorkspace
	cloneAgent     = persistmodel.CloneAgent
	cloneAgentEnvs = persistmodel.CloneAgentEnvs
	cloneSkill     = persistmodel.CloneSkill
	cloneSkillFile = persistmodel.CloneSkillFile
	sortSkillFiles = persistmodel.SortSkillFiles
	cloneSchedule  = persistmodel.CloneSchedule
	cloneRun       = persistmodel.CloneRun
	cloneEvent     = persistmodel.CloneEvent
	cloneFeedback  = persistmodel.CloneFeedback
)

// filterSlice filters in place, keeping elements where keep is true, and the result is non-nil.
func filterSlice[T any](in []T, keep func(T) bool) []T {
	out := make([]T, 0, len(in))
	for _, v := range in {
		if keep(v) {
			out = append(out, v)
		}
	}
	return out
}

// ListAuthzRoles is the in-memory version of the authorization query; the contract is on workspacesvc.Store.
//
// ⚠️ The in-memory implementation is **the reference behavior**; the rules must be equivalent to the PG one case by case:
//
//   - **workspace missing → empty slice**, not ErrNotFound.
//   - **email is case-insensitive**, same as core.memberOf (member emails are typed by hand by admins).
//   - an empty actor matches nobody.
func (s *MemStore) ListAuthzRoles(
	_ context.Context, workspaceID, actor string,
) ([]persistmodel.AuthzRole, error) {
	who := strings.ToLower(strings.TrimSpace(actor))

	s.mu.RLock()
	defer s.mu.RUnlock()

	// Non-nil empty slice, same as the other list methods in this file.
	out := make([]persistmodel.AuthzRole, 0, 1)
	if who == "" {
		return out, nil
	}
	for i := range s.data.workspaces {
		ws := &s.data.workspaces[i]
		if ws.ID != workspaceID {
			continue
		}
		for _, m := range ws.Members {
			if strings.EqualFold(strings.TrimSpace(m.Email), who) {
				out = append(out, persistmodel.AuthzRole{Role: m.Role})
				break
			}
		}
		break
	}
	return out, nil
}

// -- Device-code intermediate state of Feishu user authorization -----------------------------------------------
//
// ⚠️ In the in-memory implementation these rows **do not survive a process restart**, and with multiple instances each keeps its own. That is why
// the PG one is the production path (AMA_STORE=mem is only a fallback). See persistmodel.LarkAuthSession.

func (s *MemStore) PutLarkAuthSession(_ context.Context, sess persistmodel.LarkAuthSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.larkAuthSessions[larkUserTokenKey(sess.Actor, sess.LarkAppID)] = sess
	return nil
}

// GetLarkAuthSession gets a pending authorization record. **Expired counts as missing**, same as the PG side.
func (s *MemStore) GetLarkAuthSession(
	_ context.Context, actor, larkAppID string,
) (persistmodel.LarkAuthSession, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.data.larkAuthSessions[larkUserTokenKey(actor, larkAppID)]
	if !ok {
		return persistmodel.LarkAuthSession{}, persistmodel.ErrNotFound
	}
	// ⚠️ Expired counts as missing. Missing this step would make the two implementations diverge (PG relies on the SQL's
	// expires_at filter), while the contract suite runs the same assertions on both.
	if !sess.ExpiresAt.After(time.Now()) {
		return persistmodel.LarkAuthSession{}, persistmodel.ErrNotFound
	}
	return sess, nil
}

func (s *MemStore) DeleteLarkAuthSession(_ context.Context, actor, larkAppID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.larkAuthSessions, larkUserTokenKey(actor, larkAppID))
	return nil
}

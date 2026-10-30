// Package arkrun is the Ark (Ark Managed Agents) call domain: it really runs a chat on Ark.
//
// # This domain only "creates resources / sends and receives events on the Ark side"; it remembers nothing
//
// Everything here is a **stateless** call wrapper: the Agent id, Environment id and Session id it
// creates are stored in the Store by the caller (core's ensureBinding). They are separate because
// "remembering" must survive process restarts, while this domain is just reuse of an HTTP client.
//
// LiveRunner once held an in-memory cache; after a restart, creating an environment again with the
// same name hit 409 right away, and that workspace could never start a new chat again; see
// ensureBinding in core/chat.go.
//
// # ⚠️ Why only this package could be moved in this step
//
// Step 9b originally meant to move chat + worker + arkrun + pendingmessage. In practice only this
// domain could stand alone as a package; the test was **the number of couplings to *core.Service**:
//
//	arkrun.go                    0   <- this domain, moved out
//	workerbackoff/workerratelimit 0   <- also 0, but they are private parts of WorkerPool;
//	                                   moving them out alone would split one type across two packages
//	chat.go                      8   <- s.arkSkillsFor / s.resolveArkSecretForAgent /
//	                                   s.markStreamActive / s.hasActiveStream /
//	                                   s.reportEvent / s.nextID / s.ownerFrom / s.log
//	worker*.go                   6   <- not "zero cross-domain calls". worker is not a method set
//	                                   on *Service; instead **WorkerPool holds svc *Service**
//	                                   (the WorkerPool struct in core/worker.go). Through
//	                                   p.svc. it uses ListWorkspaces / arkKey / store /
//	                                   SetSandboxAttacher / SetShellCredentialScrubber /
//	                                   SetShellEnvScrubber, of which **arkKey and store are unexported**:
//	                                   once moved out of core they are out of reach, so the credential
//	                                   lookup and Store edges must each become narrow interfaces
//	                                   first, which is a separate step.
//
// So the boundary of this step is the mechanical test "zero coupling to Service", not a cut by file
// name. chat / worker / pendingmessage stay in core for a later step.
//
// # ⚠️ The types moved here, but not one line on the core side changed: thanks to type aliases
//
// BindingKey / AgentSpec / CreatedAgent / Runner used to live in core/chat.go, and 10 files use
// them (chat / worker / arkconsole / skillark / agentsync and their tests). The core side now has
// **aliases of the same names** (core/arkrunner.go), so none of those references need to change,
// and `core.ArkBindingKey` and `arkrun.BindingKey` are **the same type**.
//
// This is exactly the same approach as the sentinel errors (ErrInvalidArgument in core/core.go):
// an alias is not "a stopgap for the transition"; it guarantees the two sides cannot diverge, since
// diverging would be a compile error.
//
// The direction is one-way: **core may import this package; this package may not import core**.
// Test: `go list -deps ./service/arkrun | grep -c 'ama/server/core$'` = 0.
package arkrun

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/shared/infra/ma"
)

// callTimeout is the timeout for a single synchronous Ark call (create Agent / create Session /
// send events).
//
// It is required: these are all actions a user waits on right after clicking a button. Streaming
// subscriptions are **not bound by it**; that path's lifetime is managed by the request context.
//
// ⚠️ **Without it you do not get "no timeout"**: the SDK's NewClientConfig has its own
// defaultTimeout = 10 minutes, and that number once made production "chat stream interrupted"
// failures stop at exactly 10m0.00x s (see LiveRunner.instance).
const callTimeout = 30 * time.Second

// API base URLs per region, matching the baseUrl of the frontend ARK_REGIONS one to one.
const (
	baseURLCNBeijing    = "https://ark.cn-beijing.volces.com/api/v3"
	baseURLAPSoutheast1 = "https://ark.ap-southeast.bytepluses.com/api/v3"
)

// BaseURL gives the API base URL for a region.
//
// ⚠️ **It is exported because core/arkverify.go still uses it** (that one only checks whether a
// key works, and pairs with this domain). The verifier did not move along: it sits on core's
// credential check path, for the same reason the arkcred step stayed in core.
//
// The two places must share the same mapping. With one copy each, forgetting one side when adding
// a region shows up as "the key check says this region works, but starting a chat says unknown
// region", with zero errors.
func BaseURL(region apimodel.ArkRegion) (string, error) {
	switch region {
	case apimodel.RegionCNBeijing:
		return baseURLCNBeijing, nil
	case apimodel.RegionAPSoutheast1:
		return baseURLAPSoutheast1, nil
	default:
		return "", fmt.Errorf("%w: 未知的方舟区域 %q", persistmodel.ErrInvalidArgument, region)
	}
}

// BindingKey is the credentials and coordinates needed to access Ark.
type BindingKey struct {
	// WorkspaceID decides which key to use and which Environment to reuse.
	WorkspaceID string
	// AgentID is the AMA-side agent ID; the Ark Agent is reused by it.
	AgentID string
	// APIKey is the plaintext Ark key configured for this workspace.
	APIKey string
	Region apimodel.ArkRegion
}

// AgentSpec is the config used to create an Ark Agent, translated from apimodel.Agent.
type AgentSpec struct {
	Name        string
	Description string
	System      string
	Model       string
	// Speed maps to Ark's model.speed, taken from AMA's service_tier.
	Speed string
	// Tools is the list of toolset types, usually []string{ma.ToolsetDefault}.
	Tools []string
	// CustomTools are custom tool declarations; currently only the self-wakeup one (see core/wakeup.go).
	//
	// **Declaring and executing are two things**: declaring here lets the model know the tool exists,
	// and the worker side must also register an implementation with the same name (CustomTools in
	// core/worker.go).
	CustomTools []ma.CustomToolSpec

	// Skills are the skills attached to this Agent, at most 20 (the Ark-side limit).
	//
	// Same reasoning as CustomTools: **if they are not attached, the model does not know the skill
	// exists**, so it never uses it, with zero errors. A skill package must first be uploaded with
	// UploadSkill to get an id.
	Skills []persistmodel.ArkSkillRef
}

// CreatedAgent is the result of creating an Ark Agent.
//
// # Why not return just an id
//
// SkillCount is **the number of skills Ark itself confirmed as attached**, and the caller uses it
// to decide whether it may write back ArkBinding.SkillsFingerprint. This distinction is not
// fussiness; it fixes a real failure:
//
// CreateAgent used to return only the id, and ensureBinding wrote back the fingerprint
// **unconditionally** after it succeeded. So when the literal in the ma layer forgot to pass
// Skills (see CreateAgent in live.go), the binding record claimed "this set of skills is attached"
// while the Ark side was actually empty: **false records**. The next chat hit the existing binding
// -> syncBindingSkills compared fingerprints -> equal -> returned right away, and the
// UpdateAgentSkills call that would have fixed it was never reached.
//
// The result: just adding the missing field **does not fix agents already affected**, because
// their fingerprints were already poisoned and the repair path was blocked by itself. So "how many
// Ark confirmed" is put into the type here, forcing the caller to deal with it. It is the same
// principle as the line in syncBindingSkills "do not write back the fingerprint if the update did
// not succeed"; back then it was only applied on the update path, and the create path missed it.
type CreatedAgent struct {
	// ID is the Ark-side Agent id.
	ID string
	// SkillCount is the number of skills Ark confirmed as attached.
	//
	// In practice Ark's create-Agent response body includes skills (probe-agent-skills -echo-check),
	// so getting it needs no extra round trip.
	SkillCount int
}

// Runner is the abstraction for "running a chat on Ark".
//
// It is an interface so that Service tests do not need the network, for the same reason as
// ArkVerifier.
//
// **Persisting bindings is not this layer's job.** This layer only "creates resources on the Ark
// side"; remembering what was created is Service's job (through the Store). They are separate
// because "remembering" must survive process restarts, while the runner is a stateless call wrapper.
type Runner interface {
	// CreateAgent creates an Agent on the Ark side.
	//
	// Besides the id, the returned CreatedAgent carries **the number of skills Ark confirmed as
	// attached**; the caller uses it to decide whether it may write back the skill fingerprint. See
	// the comment on CreatedAgent.
	CreateAgent(ctx context.Context, key BindingKey, spec AgentSpec) (CreatedAgent, error)
	// EnsureEnvironment creates a self_hosted runtime environment and returns its ID.
	//
	// **It can only be self_hosted**: tool execution must happen in our own containers (see
	// core/worker.go). name is unique within a project, so the caller must make sure it has a unique
	// suffix. **Do not use a fixed name**: after a fixed name hits 409, "claiming" the environment with
	// the same name might pick up an old cloud environment, with the wrong type and no error anywhere
	// along the chain.
	EnsureEnvironment(ctx context.Context, key BindingKey, name string) (string, error)
	// CreateSession opens a session for the Agent in the given environment and returns the session id.
	CreateSession(ctx context.Context, key BindingKey, b persistmodel.ArkBinding, title string) (string, error)
	// SendEvents forwards events sent up by the client to Ark as-is, and returns Ark's receipt (the
	// list of accepted events).
	//
	// raw is the original events array from the frontend, passed through one by one; the payload is
	// not interpreted here, matching the model/apimodel rule of "store as-is, return as-is" for events.
	SendEvents(ctx context.Context, key BindingKey, sessionID string, raw []json.RawMessage) ([]json.RawMessage, error)
	// Stream subscribes to session events and hands the raw bytes of each frame to onFrame.
	//
	// It blocks until the stream ends or ctx is canceled. If onFrame returns an error, the
	// subscription stops (the caller disconnected).
	Stream(ctx context.Context, key BindingKey, sessionID string, onFrame func(raw []byte) error) error
	// ListEvents gets the raw bytes of all historical events of a session.
	//
	// Used for stream-drop recovery or looking back at history.
	ListEvents(ctx context.Context, key BindingKey, sessionID string) ([][]byte, error)

	// UploadSkill uploads a skill package (zip bytes) to Ark and returns (skillID, version).
	//
	// **Every call creates a new skill on the Ark side**: Ark has no upsert, no version update, and
	// no delete API (api/06-Skills/ has only the create and get-detail pages). So the caller must
	// first check ark_skill_uploads to confirm this version was not uploaded before; otherwise every
	// chat piles up another copy of garbage that cannot be cleaned up.
	UploadSkill(
		ctx context.Context, key BindingKey, fileName, displayTitle string, zip []byte,
	) (skillID string, version string, err error)

	// UpdateAgentSkills **replaces the whole set** of skills attached to the Ark Agent with refs.
	//
	// Whole replacement is Ark's semantics (the "update agent" page under api/01-Agents/: "replaced as a whole
	// when passed"), so the full set must be sent each time, not a delta. Empty refs means clear.
	//
	// Ark's UpdateAgent uses optimistic locking (version must equal the server's current value), so
	// the implementation must read first and then write; a version conflict means another instance
	// just updated it, and **is not an error** (see the LiveRunner implementation).
	UpdateAgentSkills(
		ctx context.Context, key BindingKey, arkAgentID string, refs []persistmodel.ArkSkillRef,
	) error

	// UpdateAgentSpec brings the Ark Agent's system / model / description / tools up to date with spec.
	//
	// # Why it is required
	//
	// An Ark Agent is created once and cached forever (ensureBinding returns right away when it hits
	// an existing binding), so if the user changes the system prompt or the model after the first
	// chat, the Ark side never knows. The symptom is "I changed the prompt but the agent still talks
	// the old way", **with zero errors, forever**.
	// This is the same problem as with skills (UpdateAgentSkills); back then only skills were kept
	// up to date.
	//
	// # ⚠️ spec must be the full value computed by arkSpecFor
	//
	// The system sent to Ark is assembled (user text + self-wakeup instructions + the
	// `<harness:env>` block). Sending only agent.System would **wipe out the two parts the platform
	// injects**, a zero-error regression: from then on the model gets no runtime facts and does not
	// know it can wake itself up.
	//
	// **Skills are not on this path** (UpdateAgentSkills handles them): the two have independent
	// fingerprints, and mixing them would make "change skills" and "change prompt" trigger pointless
	// round trips for each other.
	UpdateAgentSpec(
		ctx context.Context, key BindingKey, arkAgentID string, spec AgentSpec,
	) error

	// RawDo / RawStream pass raw Ark HTTP API calls through at the byte level.
	//
	// Why they exist: AMA forwards the rest of the Ark Managed Agents HTTP APIs (session resources,
	// threads, Vaults, memory stores, Files and so on) as-is, with fields kept identical to Ark; they
	// must not go through the SDK's strongly typed structs in between (new Ark fields would be
	// silently dropped). See shared/infra/ma/raw.go.
	//
	// multipart uploads (Files) also go through RawDo: the request body is forwarded whole and
	// unchanged, and the ContentType is carried by RawRequest.ContentType (the multipart boundary is in it).
	RawDo(ctx context.Context, key BindingKey, req ma.RawRequest) (*ma.RawResponse, error)
	RawStream(ctx context.Context, key BindingKey, req ma.RawRequest) (*ma.RawStream, error)
}

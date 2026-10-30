// The real implementation of Runner: runs chats on Ark Managed Agents.
//
// It pairs with arkverify.go: that one only checks "does this key work", this one does the real
// sessions. Both share the same region-to-base-URL mapping (BaseURL).
//
// # One Ark client per workspace
//
// The key is configured **per workspace** (see core/ark.go), so the client cannot be a global
// singleton. Instances are cached by "region + key": consecutive requests from the same workspace
// reuse the same http connection pool, and a changed key naturally gets a new instance, while the
// old one waits for GC.
//
// # Creating resources is idempotent, but not free
//
// Creating Agents / Environments on Ark are quota-limited resource operations; creating them once
// per message would fill up the console quickly, so "create only once per AMA agent" is a
// necessity, not an optimization.
//
// But **remembering what was created is not this layer's job**: this type is stateless, and
// bindings are stored in the Store by Service. An in-memory cache used to live here; after a
// process restart the cache was empty, creating an environment again with the same name hit 409
// right away, and that workspace could never start a new chat again. See ensureBinding in chat.go.

package arkrun

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/shared/infra/ma"
	"github.com/volcengine/ark-runtime-go/arkruntime/model/session"
)

// LiveRunner is the Runner that calls the real Ark.
//
// **Stateless**: resource IDs it creates are stored in the Store by Service and not cached here.
// An in-memory cache used to live here, and after a restart it would create an environment again
// with the same name and hit 409; see ensureBinding in chat.go. Only http client reuse is kept here.
type LiveRunner struct {
	// BaseURLOverride, when not empty, ignores the region and is used directly. A hook kept only for
	// integration testing, same as LiveArkVerifier.
	BaseURLOverride string

	mu        sync.Mutex
	instances map[string]*ma.Instance
}

// NewLive creates a runner that calls the real Ark.
func NewLive() *LiveRunner {
	return &LiveRunner{instances: map[string]*ma.Instance{}}
}

var _ Runner = (*LiveRunner)(nil)

// instance gets (or creates) the Ark client for a workspace.
//
// **No WithTimeout**: that option is set on the underlying http.Client, while timeouts for normal
// requests here are always controlled by context (see callTimeout in each method), so it is not needed.
//
// But **do not take "no WithTimeout" as "no timeout"**: the SDK's NewClientConfig has its own
// defaultTimeout = 10 minutes. SSE long connections are not bound by it because ma.New builds a
// separate client without Timeout just for streaming APIs (see ma.newStreamHTTPClient), not because
// http.Client.Timeout cannot reach SSE. It can, and this trap really did blow up: production
// "chat stream interrupted" failures all stopped at exactly 10m0.00x s.
func (r *LiveRunner) instance(key BindingKey) (*ma.Instance, error) {
	baseURL := r.BaseURLOverride
	if baseURL == "" {
		var err error
		if baseURL, err = BaseURL(key.Region); err != nil {
			return nil, err
		}
	}
	cacheKey := baseURL + "\x00" + key.APIKey

	r.mu.Lock()
	defer r.mu.Unlock()
	if ins, ok := r.instances[cacheKey]; ok {
		return ins, nil
	}
	ins, err := ma.New(key.APIKey, ma.WithBaseURL(baseURL))
	if err != nil {
		return nil, fmt.Errorf("构造方舟客户端失败: %w", err)
	}
	r.instances[cacheKey] = ins
	return ins, nil
}

// CreateAgent creates an Agent on the Ark side.
//
// Ark Agents **allow duplicate names** (verified), so there is no dedup here: "create only once
// per AMA agent" is guaranteed by the binding in the Store, not by the name.
//
// # ⚠️ New fields must be passed down; this literal once missed Skills for a whole release
//
// The `ma.AgentSpec{...}` below is the **only** translation point between core and ma, and it is
// written field by field by hand: missing a field causes no compile error, no test turns red, and
// Ark still returns 200.
// It really happened: `Skills` was not written in, so the skill refs computed upstream
// (`spec.Skills = refs` in chat.go) were silently dropped on this line, and the Agent created on
// the Ark side had empty skills.
// The symptom was "the user configured 3 skills, asked the agent what skills it has, and it said
// none": the skill packages uploaded fine, the binding record was complete, the files were sitting
// in the container, and the whole chain had zero errors.
//
// Now TestArkAgentSpecMapsEveryFieldToMA (arkrun_test.go) uses reflection to watch every field of
// AgentSpec, and a missing one turns red. **Do not get around it when adding fields**.
//
// # Return the number of attached skills, not just the id
//
// The caller uses it to decide "may the fingerprint be written back": the fingerprint means "this
// is exactly the set attached on the Ark side right now", and writing it back before Ark confirms
// would make the next chat hit the fingerprint and skip the update, permanently blocking the path
// that should have self-healed (see ensureBinding in chat.go).
// In practice Ark's create-Agent **response body includes skills** (verified with
// probe-agent-skills -echo-check), so this confirmation costs zero extra round trips.
func (r *LiveRunner) CreateAgent(
	ctx context.Context, key BindingKey, spec AgentSpec,
) (CreatedAgent, error) {
	ins, err := r.instance(key)
	if err != nil {
		return CreatedAgent{}, err
	}

	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	ag, err := ins.Agents().Create(callCtx, ma.AgentSpec{
		Name:        spec.Name,
		Model:       spec.Model,
		Speed:       spec.Speed,
		System:      spec.System,
		Description: spec.Description,
		Tools:       spec.Tools,
		CustomTools: spec.CustomTools,
		// ⚠️ Do not miss it again. See the paragraph above.
		Skills: toMASkillRefs(spec.Skills),
	})
	if err != nil {
		return CreatedAgent{}, fmt.Errorf("建方舟 Agent 失败: %w", err)
	}
	return CreatedAgent{ID: ag.ID, SkillCount: len(ag.Skills)}, nil
}

// EnsureEnvironment creates a self_hosted environment.
//
// # Only self_hosted, never cloud
//
// With cloud, Ark runs the agent loop itself and tools execute in its managed sandbox; with
// self_hosted, Ark only does model inference, and tool calls are put into the work queue, picked
// up by our worker, and run in our Docker containers (see core/worker.go and
// shared/infra/dockertools). This project needs the latter: the execution environment must be in
// our hands.
//
// **self_hosted does not accept networking**: outbound network access is managed by the
// self-hosted runtime itself, and passing it is rejected
// (400 InvalidParameter: config.networking: not allowed when config.type is
// "self_hosted"). Counter-intuitively, **reading it back after creation does include a networking
// field**, so do not infer what the request should send from the response body. ma.EnvironmentSpec
// already handles this difference (only EnvCloud fills Networking), so leave it empty here.
//
// # The name must be unique; no fixed names
//
// Environment names are unique within a project, and creating a duplicate returns 409. **This used
// to use the fixed name `ama-{workspaceID}` and "claim" the same-named environment on 409, which was
// a source of silent failure**: after changing the type from cloud to self_hosted, the 409 made the
// code reuse the **old cloud environment**. The type never changed, while logs, UI and API
// responses all looked normal; only tool execution quietly stayed on the Ark side.
//
// So now each creation gets a unique suffix, with no claiming by name. The cost is one extra
// environment when "the binding table was cleared" (an orphan resource left on the Ark side); in
// return, **the type always matches the code**.
// Reuse still works, but goes through the binding table (see ensureBinding in chat.go: if another
// agent in the same workspace already created one, its environment_id is reused), not by name.
func (r *LiveRunner) EnsureEnvironment(
	ctx context.Context, key BindingKey, name string,
) (string, error) {
	ins, err := r.instance(key)
	if err != nil {
		return "", err
	}

	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	env, err := ins.Environments().Create(callCtx, ma.EnvironmentSpec{
		Name: name,
		Type: ma.EnvSelfHosted,
		// No Networking: self_hosted returns 400 if it is passed; see the comment above.
		Description: "AMA 空间 " + key.WorkspaceID + " 的对话运行环境（self_hosted）",
	})
	if err != nil {
		return "", fmt.Errorf("建方舟 self_hosted 运行环境失败: %w", err)
	}

	// Read it back once after creation to confirm the type really is self_hosted.
	//
	// **This step is not redundant.** An empty `ma.EnvironmentSpec.Type` defaults to cloud (see
	// toRequest in shared/infra/ma/environments.go), so any change that leaves Type empty (a field
	// missed in a refactor, a copy-paste somewhere that forgot it) silently falls back to cloud:
	// chats still work (Ark runs the tools itself), only the execution environment is not in our
	// hands, and **no error will tell you**. That is exactly how it slipped through last time: the
	// type was cloud, the UI was all green, until someone looked at the environment details.
	//
	// A failed read-back is not fatal (the environment was already created), so only log and do not
	// block; a wrong type must return an error.
	if got, err := ins.Environments().Get(callCtx, env.ID); err == nil {
		if cfg, ok := got.Config.Get(); ok {
			if t := string(cfg.Type); t != ma.EnvSelfHosted {
				return "", fmt.Errorf(
					"方舟运行环境 %s 的类型是 %q，不是 self_hosted——"+
						"工具会跑在方舟托管沙箱而不是我们的容器里，拒绝使用", env.ID, t)
			}
		}
	}
	return env.ID, nil
}

// CreateSession opens a session.
func (r *LiveRunner) CreateSession(
	ctx context.Context, key BindingKey, b persistmodel.ArkBinding, title string,
) (string, error) {
	ins, err := r.instance(key)
	if err != nil {
		return "", err
	}

	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	sess, err := ins.Sessions().Create(callCtx, ma.SessionSpec{
		AgentID:       b.ArkAgentID,
		EnvironmentID: b.ArkEnvironmentID,
		Title:         title,
	})
	if err != nil {
		return "", err
	}
	return sess.ID, nil
}

// SendEvents forwards upstream events to Ark as-is.
//
// # Why decode into the SDK union first instead of passing the bytes through
//
// Passing bytes through would mean bypassing the SDK's own body-building layer, which amounts to
// rewriting SendSessionEvents. The SDK's ManagedAgentsEventParams is exactly the discriminated
// union of these 7 kinds of client upstream events: if it decodes, the shape is valid; if it does
// not, it should be stopped here anyway (otherwise Ark returns an English InvalidParameter that
// users cannot understand). In practice user.message / user.interrupt both round-trip losslessly.
//
// The returned receipt is **not** the one the SDK gives: the SDK's SendSessionEventsResponse has
// only a success field and drops the data array Ark actually returns (accepted events with ids).
// But the frontend sendMessage needs the ids in the receipt to draw the message. So we build one
// here ourselves: the original upstream events plus Ark-style ids. See the comment on AcceptedFrom
// below.
func (r *LiveRunner) SendEvents(
	ctx context.Context, key BindingKey, sessionID string, raw []json.RawMessage,
) ([]json.RawMessage, error) {
	ins, err := r.instance(key)
	if err != nil {
		return nil, err
	}

	events := make([]session.ManagedAgentsEventParams, 0, len(raw))
	for i, item := range raw {
		var ev session.ManagedAgentsEventParams
		if err := json.Unmarshal(item, &ev); err != nil {
			return nil, fmt.Errorf("%w: 第 %d 个事件的格式不被方舟接受（%v）",
				persistmodel.ErrInvalidArgument, i+1, err)
		}
		events = append(events, ev)
	}

	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	if _, err := ins.Sessions().SendEvents(callCtx, sessionID, events...); err != nil {
		return nil, err
	}
	return AcceptedFrom(raw), nil
}

// AcceptedFrom builds the "accepted events list" receipt.
//
// What Ark really returns is `{"data":[{"id":"sevt-...","type":"user.message","content":[...]}]}`:
// the original upstream events plus an id. But the SDK's response struct drops data entirely (it
// only declares the success field), so those ids are out of reach.
//
// The frontend uses the receipt for one thing only: drawing the just-sent message on screen right
// away (appendLocal, deduped by id). And **Ark pushes this message back into the event stream
// as-is**, with the real id it assigned; that one is authoritative and replaces the temporary
// version given here when it arrives. So the id here only needs to be locally unique and not
// collide with Ark's.
//
// That is why the `sevt-local-` prefix is used: you can tell at a glance it is a local temporary
// id, and it can never equal Ark's `sevt-<timestamp>-<random>`.
//
// ⚠️ **It is exported for core's fakeRunner** (core/chat_test.go): that stand-in has to mimic
// Ark's receipt shape, and it must give **the same shape** as the real implementation. With one
// copy each, changing the receipt format would only change the production path, while tests keep
// asserting the old shape and **stay all green**, which is exactly what this chain must not have
// (same as the "tests on both sides fail to cover the wiring" noted at the top of arkrun_test.go).
func AcceptedFrom(raw []json.RawMessage) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(raw))
	for i, item := range raw {
		id := fmt.Sprintf("sevt-local-%d-%d", time.Now().UnixNano(), i)
		out = append(out, withLocalEnvelope(item, id))
	}
	return out
}

// withLocalEnvelope adds the envelope fields the frontend needs to one upstream event.
//
// It adds four: id / processed_at / session_thread_id / sequence, which RunEventBase requires.
// **It splices bytes and does not marshal**: the upstream event carries the user's original input,
// and going through encoding/json would escape < > &, so it would no longer equal, byte for byte,
// the copy Ark pushes back later.
//
// sequence is 0: this one is temporary; the frontend only uses it for dedup and display, and the
// real event replaces it by id when it arrives.
func withLocalEnvelope(raw json.RawMessage, id string) json.RawMessage {
	trimmed := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(trimmed, "{") {
		return raw
	}
	var head strings.Builder
	head.WriteByte('{')
	head.WriteString(`"id":`)
	idBytes, _ := json.Marshal(id)
	head.Write(idBytes)
	head.WriteString(`,"processed_at":`)
	tsBytes, _ := json.Marshal(apimodel.NowISO())
	head.Write(tsBytes)
	head.WriteString(`,"session_thread_id":null,"sequence":0`)
	if len(strings.TrimSpace(trimmed[1:len(trimmed)-1])) > 0 {
		head.WriteByte(',')
	}
	head.WriteString(trimmed[1:])
	return json.RawMessage(head.String())
}

// Stream subscribes to session events and hands the raw bytes of each frame to onFrame.
//
// # event_deltas must be set explicitly
//
// Without it Ark **sends no delta frames at all**, and the reply pops out in one piece instead of
// word by word, so the frontend's event_start / event_delta typewriter effect is wasted. Ark
// currently supports only two kinds, agent.message / agent.thinking (see the "stream session
// events" doc page).
//
// The SDK's StreamSessionEvents does not expose this parameter, but it accepts requestOption, and
// arkruntime.WithQuery is exported, so there is no need to bypass the SDK and build the URL
// ourselves. In practice Ark accepts both the comma-separated and the repeated-parameter forms;
// the comma form is used here (WithQuery has Set semantics, so repeated calls overwrite each other
// and cannot produce repeated parameters).
func (r *LiveRunner) Stream(
	ctx context.Context, key BindingKey, sessionID string, onFrame func(raw []byte) error,
) error {
	ins, err := r.instance(key)
	if err != nil {
		return err
	}

	dec, err := ins.Sessions().StreamEvents(ctx, sessionID, "agent.message", "agent.thinking")
	if err != nil {
		return err
	}
	// Close is required, or the underlying HTTP connection leaks.
	defer func() { _ = dec.Close() }()

	// Close the stream when ctx is canceled: dec.Next() blocks on read, and only closing the response
	// body makes it return. Passing ctx to the SDK alone is not enough: the read loop of that SSE
	// connection does not honor context.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = dec.Close()
		case <-done:
		}
	}()

	for dec.Next() {
		frame := dec.Event()
		// RawPayload is Ark's original text, kept byte for byte; both forwarding to the browser and
		// writing to the database use it.
		if err := onFrame(frame.RawPayload); err != nil {
			return err
		}
	}
	if err := dec.Err(); err != nil {
		// A close caused by ctx cancellation is not an error; it is a normal wind-down (the browser
		// closed the page).
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	return nil
}

// ListEvents fetches all historical event frames of a Session page by page.
func (r *LiveRunner) ListEvents(
	ctx context.Context, key BindingKey, sessionID string,
) ([][]byte, error) {
	ins, err := r.instance(key)
	if err != nil {
		return nil, err
	}

	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	return ins.Sessions().ListAllRawEvents(callCtx, sessionID)
}

// ---------------------------------------------------------------------------
// Skills
// ---------------------------------------------------------------------------

// UploadSkill uploads a skill package to Ark.
//
// # Every call creates a new skill
//
// Ark's Skills API only has create and get-detail (docs/ark-managed-agents/api/06-Skills/), with
// no upsert, no version update, and no delete. So **the caller must first check ark_skill_uploads**
// to confirm this version was not uploaded, or every new chat piles up another copy of garbage that
// cannot be cleaned up. The dedup logic is in Service (ensureArkSkills), not this layer; this layer
// is a stateless call wrapper.
func (r *LiveRunner) UploadSkill(
	ctx context.Context, key BindingKey, fileName, displayTitle string, zip []byte,
) (string, string, error) {
	ins, err := r.instance(key)
	if err != nil {
		return "", "", err
	}

	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	// bytes.NewReader instead of CreateFromPath: the package is in memory, and writing it to disk to
	// read it again is wasted work. Also, production runs many instances, and each instance's disk is
	// separate and lost on rebuild (see CLAUDE.md).
	sk, err := ins.Skills().Create(callCtx, bytes.NewReader(zip), fileName, displayTitle)
	if err != nil {
		return "", "", err
	}
	if sk == nil || sk.ID == "" {
		// Ark returned 200 but no id: going on would attach an empty string to the Agent as a skill
		// id, the model would then see a skill whose content cannot be found, and the error would
		// happen on a different path.
		return "", "", fmt.Errorf("方舟未返回技能 ID")
	}
	return sk.ID, sk.LatestVersion, nil
}

// UpdateAgentSkills replaces the whole set of skills attached to the Ark Agent with refs.
//
// # Why Get first and then Update
//
// Ark's UpdateAgent uses **optimistic locking**: the version in the request body must equal the
// server's current value, or the write is rejected (ma.AgentUpdate.Version is required). We do not
// cache that version number: caching it would mean handling "stale cache", while fetching it fresh
// costs only one more round trip, and this path is not hot anyway (it only runs when the skill
// fingerprint changes).
func (r *LiveRunner) UpdateAgentSkills(
	ctx context.Context, key BindingKey, arkAgentID string, refs []persistmodel.ArkSkillRef,
) error {
	return r.updateAgent(ctx, key, arkAgentID, "技能", func(upd *ma.AgentUpdate) {
		if len(refs) == 0 {
			// **It must be cleared explicitly**; nil cannot be passed: nil on the wire means "not
			// sent", and Ark keeps the existing skills, so "unbind the last skill" silently does
			// nothing: the user thinks it was unbound, and the model keeps using it.
			upd.ClearSkills = true
		} else {
			upd.Skills = toMASkillRefs(refs)
		}
	})
}

// UpdateAgentSpec brings the Ark Agent's system / model / description / tools up to date with spec.
//
// # ⚠️ Always send Tools and CustomTools together
//
// Ark's tools are **replaced as a whole**, and built-in toolsets and custom tools share the same
// array. Sending only the former deletes `ama_wakeup`: the agent suddenly cannot wake itself up,
// and Ark still returns 200. ma.toolItems puts the two together, so just pass them down as-is here
// (see that function's comment).
//
// # Skills are not on this path
//
// upd.Skills stays nil (= no change); skills are handled by UpdateAgentSkills. **Do not also slip
// spec.Skills in here**: the spec passed in by syncBindingSpec is arkSpecFor's return value as-is,
// and its Skills is empty, so putting it in would be treated as nil (no change) by the ma layer.
// Fine today, but if someone fills it in someday, the two paths would overwrite each other.
func (r *LiveRunner) UpdateAgentSpec(
	ctx context.Context, key BindingKey, arkAgentID string, spec AgentSpec,
) error {
	return r.updateAgent(ctx, key, arkAgentID, "提示词/模型", func(upd *ma.AgentUpdate) {
		upd.Name = spec.Name
		upd.Description = spec.Description
		upd.System = spec.System
		upd.Model = spec.Model
		upd.Speed = spec.Speed
		upd.Tools = spec.Tools
		upd.CustomTools = spec.CustomTools
	})
}

// updateAgent is the shared implementation of "Get the version first, then Update".
//
// what is only used in logs and error messages ("skills" / "prompt/model").
//
// # A version conflict must be retried, not treated as success
//
// ⚠️ **This used to return nil for a 409, treating it as success**, on the grounds that "a conflict
// means another instance just updated the same Agent, and what it wrote is the same as what we want
// to write (computed from the same AMA data)". That held **when there was only one kind of update**,
// and stopped holding once a second kind (spec) was added:
//
//	instance A wants to push spec (system from V1 to V2), Gets version=3
//	instance B pushes skills at the same time, writes first, version becomes 4
//	A's Update hits 409 -> treated as success -> **writes back the spec fingerprint**
//	-> on the Ark side system is still V1, while the binding claims V2
//
// So once again "false records block the repair path": the next chat finds the fingerprint equal
// and returns right away, and the Update that should have corrected it is never called. This is the
// same problem CreatedAgent.SkillCount fixed, only this time the trigger is concurrency instead of a
// missing field: **production runs many instances, so two people starting chats at the same time
// will hit it**.
//
// So it now retries once with the new version: only a conflict after the retry returns an error
// (then the caller does not write back the fingerprint, the next chat tries again, and the
// self-healing path stays open). One retry is enough: two conflicts in a row are very unlikely, and
// unlimited retries would add latency on a path the user is waiting on.
func (r *LiveRunner) updateAgent(
	ctx context.Context, key BindingKey, arkAgentID, what string, fill func(*ma.AgentUpdate),
) error {
	if arkAgentID == "" {
		return fmt.Errorf("方舟 Agent ID 为空，无法更新%s", what)
	}
	ins, err := r.instance(key)
	if err != nil {
		return err
	}

	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	// Two chances: the first uses the version from Get; on 409, Get again and retry once.
	for attempt := 0; attempt < 2; attempt++ {
		cur, err := ins.Agents().Get(callCtx, arkAgentID)
		if err != nil {
			return fmt.Errorf("读方舟 Agent 失败: %w", err)
		}
		if cur == nil {
			return fmt.Errorf("方舟 Agent %s 不存在", arkAgentID)
		}

		upd := ma.AgentUpdate{Version: cur.Version}
		fill(&upd)

		if _, err := ins.Agents().Update(callCtx, arkAgentID, upd); err != nil {
			if ma.IsConflict(err) && attempt == 0 {
				// Another instance just wrote; our version number is stale. Read again and retry.
				continue
			}
			return fmt.Errorf("更新方舟 Agent 的%s失败: %w", what, err)
		}
		return nil
	}
	// Both attempts hit a conflict. **Return an error, not nil**: the caller uses it to decide not to
	// write back the fingerprint, so the next chat tries again. Calling "not written" "written" is the
	// real trap.
	return fmt.Errorf("更新方舟 Agent 的%s连续两次版本冲突，下次会重试", what)
}

// toMASkillRefs translates core's skill refs into the ma layer's shape.
//
// Type is left empty and ma.toSkillRefs fills it in as custom: everything we upload is a custom skill.
func toMASkillRefs(refs []persistmodel.ArkSkillRef) []ma.SkillRef {
	out := make([]ma.SkillRef, 0, len(refs))
	for _, r := range refs {
		out = append(out, ma.SkillRef{SkillID: r.ArkSkillID, Version: r.Version})
	}
	return out
}

// ---------------------------------------------------------------------------
// Byte-level pass-through: forwards the rest of the Ark Managed Agents HTTP APIs as-is
// (session resources, threads, Vaults, memory stores, Files and so on); see ma/raw.go.
// ---------------------------------------------------------------------------

func (r *LiveRunner) RawDo(ctx context.Context, key BindingKey, req ma.RawRequest) (*ma.RawResponse, error) {
	ins, err := r.instance(key)
	if err != nil {
		return nil, err
	}
	return ins.RawDo(ctx, req)
}

func (r *LiveRunner) RawStream(ctx context.Context, key BindingKey, req ma.RawRequest) (*ma.RawStream, error) {
	ins, err := r.instance(key)
	if err != nil {
		return nil, err
	}
	return ins.RawStream(ctx, req)
}

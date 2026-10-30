// Agent: a versioned, reusable agent configuration asset.
// API: /api/v3/agents

package ma

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/go-faster/jx"
	"github.com/volcengine/ark-runtime-go/arkruntime"
	"github.com/volcengine/ark-runtime-go/arkruntime/model/agent"
)

// AgentService creates, reads, updates and deletes Agent definitions. Get it via
// Instance.Agents().
type AgentService struct {
	client *arkruntime.Client
}

// AgentSpec is the convenience input for creating an Agent and covers the most common fields.
//
// For more complex configuration such as MCP servers, Skills or multi-Agent orchestration, use
// CreateRaw and pass the SDK's own request body.
type AgentSpec struct {
	// Name is the agent name, 1–100 characters. Required.
	Name string
	// Model is the model ID, e.g. "doubao-seed-2-1-pro-260628". Required.
	Model string
	// Speed is the model speed tier: "standard" / "fast"; empty uses the model default.
	// Not every model supports fast.
	Speed string
	// System is the system prompt, at most 100,000 characters.
	System string
	// Description is the agent description, at most 2048 characters.
	Description string
	// Tools lists toolset types; usually pass []string{ma.ToolsetDefault} to enable the
	// built-in tools.
	Tools []string
	// CustomTools are custom tool declarations (`type: "custom"`).
	//
	// **The built-in toolset is a fixed set of 8** (bash/read/write/edit/glob/grep/web_fetch/
	// web_search). To let the agent call a tool of our own, registering the implementation on
	// the worker side is **not enough**: Ark does not tell the model about it, so the model
	// never calls it, and nothing reports an error. It must be declared when the Agent is
	// created, and this field does exactly that.
	//
	// The execution side is handled by the SDK's EnvironmentWorkerOptions.CustomTools, which
	// sends the user.custom_tool_result reply itself; we do not hand-write the protocol.
	CustomTools []CustomToolSpec

	// Skills are the skill references attached to this Agent, at most 20 (the Ark-side limit).
	//
	// **Same kind of thing as CustomTools**: if a skill is not attached, the model does not
	// even know it exists, so it never uses it, and nothing reports an error. The skill package
	// itself must first be uploaded with Skills().Create to get a skill_id.
	Skills []SkillRef
}

// SkillRef is one skill reference.
//
// # type is custom, even though the docs say "phase one only supports skill_hub"
//
// That sentence (skills.type in the Ark "Create Agent" API doc) is **outdated**: the official
// 02-Skills.md guide and two best-practice articles all use `type: "custom"`, the SDK's
// SkillRefType enum and validator both accept it, and the `source` field in the CreateSkill
// response itself has the value custom. It is the same kind of issue as "the Ark thinking doc
// says thinking content is not returned, yet 69/72 measured events carried it": **trust the
// SDK and measurements, not the docs**.
type SkillRef struct {
	// SkillID is the Ark-side skill id, the one CreateSkill returns. Required.
	SkillID string
	// Version is the skill version; empty uses the latest.
	Version string
	// Type defaults to SkillTypeCustom when empty (everything we upload is a custom skill).
	Type string
}

// Skill reference types. Ark supports three; we only use custom (packages we upload).
const (
	// SkillTypeCustom is a custom skill uploaded by the user; all AMA skills are of this kind.
	SkillTypeCustom = "custom"
	// SkillTypeHub is a platform preset skill (picked from SkillHub); AMA does not use it now.
	SkillTypeHub = "skill_hub"
)

// toSkillRefs translates skill references into the SDK's shape.
//
// An empty Type is filled in as custom: most callers only care about skill_id, and a missing
// type makes Ark return 400 right away (type is required), with an English InvalidParameter
// error.
func toSkillRefs(refs []SkillRef) []agent.AgentSkillRef {
	if len(refs) == 0 {
		// Return nil, not an empty slice: the SDK's encodeFields only writes the skills key
		// when it is non-nil, and on the Ark side "not sent" and "empty array sent" are two
		// different things (the latter **clears** them). When creating an Agent both give the
		// same result, but on update the difference is big; do not plant a trap here.
		return nil
	}
	out := make([]agent.AgentSkillRef, 0, len(refs))
	for _, r := range refs {
		typ := r.Type
		if typ == "" {
			typ = SkillTypeCustom
		}
		item := agent.AgentSkillRef{Type: agent.SkillRefType(typ)}
		if r.SkillID != "" {
			item.SkillID = agent.NewOptString(r.SkillID)
		}
		if r.Version != "" {
			item.Version = agent.NewOptString(r.Version)
		}
		out = append(out, item)
	}
	return out
}

// CustomToolSpec declares a custom tool.
//
// All three fields are required by Ark: the model uses Description to decide when to call it,
// and InputSchema to constrain the arguments it generates. A vague Description makes the model
// skip the call when it should call, or call when it should not.
type CustomToolSpec struct {
	// Name is the tool name, 1–128 characters, [A-Za-z0-9_-].
	//
	// **The key registered on the worker side must match it exactly**, otherwise when Ark
	// sends agent.custom_tool_use the worker cannot find it in the map, and the call hangs on
	// requires_action.
	Name string
	// Description explains the purpose; the model uses it to decide when to call the tool.
	Description string
	// InputSchema is the JSON Schema of the arguments (top-level object).
	//
	// A map instead of a struct: JSON Schema's shape is too free-form, and a hand-made type
	// would only limit what can be expressed. Keys are property names, values are that
	// property's schema (`{"type":"string","description":"..."}`).
	InputSchema map[string]any
	// Required lists the required property names.
	Required []string
}

// toRequest turns the convenience input into the SDK request body.
func (s AgentSpec) toRequest() *agent.CreateAgentRequest {
	req := &agent.CreateAgentRequest{
		Name:        s.Name,
		Model:       modelConfig(s.Model, s.Speed),
		System:      optAgentString(s.System),
		Description: optAgentString(s.Description),
	}
	req.Tools = toolItems(s.Tools, s.CustomTools)
	req.Skills = toSkillRefs(s.Skills)
	return req
}

// modelConfig builds the model field.
//
// **Create and update must both use it**: the update side once only filled in the ID and
// dropped Speed, so after "switch the agent to the fast tier" was synced, the tier silently fell
// back to the default. Zero errors, just slower.
func modelConfig(id, speed string) agent.ModelConfig {
	cfg := agent.ModelConfig{ID: id}
	if speed != "" {
		cfg.Speed = agent.NewOptModelSpeed(agent.ModelSpeed(speed))
	}
	return cfg
}

// toolItems joins "built-in toolsets + custom tools" into Ark's tools array.
//
// # ⚠️ Create and update must both use it, or update silently wipes the custom tools
//
// Ark's tools field is **replaced as a whole** (same for create and update), and built-in
// toolsets and custom tools **share the same array**. So sending only the built-in toolset entry
// on update deletes the custom tools. The symptom is that the agent suddenly can no longer wake
// itself up (`ama_wakeup` disappears from the model's tool list), and this **reports no error at
// all**: Ark still returns 200, the model just never sees that tool again.
//
// This is the next form of the trap noted in this package's README, "a hand-written
// field-by-field translation layer drops fields"; last time CreateAgent dropped Skills. Pulling
// it into one function makes it **impossible for the two paths to drift**.
//
// nil means both are empty: callers use that to omit the tools key (on update "not sent" keeps
// the current value, while an empty array clears it, which is a big difference).
func toolItems(tools []string, customTools []CustomToolSpec) []agent.ToolItem {
	if len(tools) == 0 && len(customTools) == 0 {
		return nil
	}
	items := make([]agent.ToolItem, 0, len(tools)+len(customTools))
	for _, t := range tools {
		items = append(items, agent.ToolItem{Type: t})
	}
	for _, ct := range customTools {
		item := agent.ToolItem{
			Type:        "custom",
			Name:        agent.NewOptString(ct.Name),
			Description: agent.NewOptString(ct.Description),
		}
		schema := agent.CustomToolInputSchema{
			Type:     agent.NewOptString("object"),
			Required: ct.Required,
		}
		if len(ct.InputSchema) > 0 {
			props := agent.CustomToolInputSchemaProperties{}
			for name, def := range ct.InputSchema {
				// Encode each property's schema to bytes first and put that in: the value type
				// of that SDK map is jx.Raw (raw JSON), not any. If encoding fails, skip this
				// property. That means the caller passed something that cannot be serialized,
				// which is a programming error, but it should not fail the whole Agent creation
				// (the model getting a schema with one item missing is better than not being
				// able to create it at all).
				blob, err := json.Marshal(def)
				if err != nil {
					continue
				}
				props[name] = jx.Raw(blob)
			}
			schema.Properties = agent.NewOptCustomToolInputSchemaProperties(props)
		}
		item.InputSchema = agent.NewOptCustomToolInputSchema(schema)
		items = append(items, item)
	}
	return items
}

// Create creates an Agent.
//
//	ag, err := ins.Agents().Create(ctx, ma.AgentSpec{
//	    Name:   "triage",
//	    Model:  "doubao-seed-2-1-pro-260628",
//	    System: "You are a helpful assistant.",
//	    Tools:  []string{ma.ToolsetDefault},
//	})
func (s *AgentService) Create(ctx context.Context, spec AgentSpec) (*agent.Agent, error) {
	if spec.Name == "" {
		return nil, errors.New("ma: AgentSpec.Name 必填")
	}
	if spec.Model == "" {
		return nil, errors.New("ma: AgentSpec.Model 必填")
	}
	return s.CreateRaw(ctx, spec.toRequest())
}

// CreateRaw creates an Agent using the SDK's own request body, for fields that AgentSpec does
// not cover (MCP servers, Skills, multi-Agent topology, metadata, tags, etc.).
func (s *AgentService) CreateRaw(
	ctx context.Context,
	req *agent.CreateAgentRequest,
) (*agent.Agent, error) {
	return s.client.CreateAgent(ctx, req)
}

// Get returns Agent details by ID.
func (s *AgentService) Get(ctx context.Context, agentID string) (*agent.Agent, error) {
	return s.client.GetAgent(ctx, agentID)
}

// List lists Agents, page by page.
//
// Paging: put the returned NextPage into the next call's Page.Page; an empty NextPage means
// the end.
func (s *AgentService) List(ctx context.Context, page Page) (*agent.ListAgentsResponse, error) {
	return s.ListRaw(ctx, &agent.AgentsListParams{
		Limit:        optAgentInt32(page.Limit),
		Page:         optAgentString(page.Page),
		CreatedAtGte: optAgentString(page.CreatedAtGte),
		CreatedAtLte: optAgentString(page.CreatedAtLte),
	})
}

// ListRaw paginates using the SDK's own query parameters.
func (s *AgentService) ListRaw(
	ctx context.Context,
	params *agent.AgentsListParams,
) (*agent.ListAgentsResponse, error) {
	return s.client.ListAgents(ctx, params)
}

// AgentUpdate describes one Agent update.
//
// Only fields set explicitly are sent; unset fields keep their current value. But note that
// Tools is **replaced as a whole** (that is how the server defines it): any non-nil value
// overwrites the existing tool configuration.
type AgentUpdate struct {
	// Version is the current version expected to be updated, for optimistic concurrency
	// control; the write is rejected if it differs from the server. Required.
	Version int32
	// Name is the new name; empty means unchanged.
	Name string
	// Model is the new model ID; empty means unchanged.
	Model string
	// Speed is the new speed tier ("fast" / empty for the model default).
	//
	// ⚠️ **It is sent together with Model**: Ark's model is one object (`{id, speed}`),
	// replaced as a whole. So when Model is empty this field cannot be sent either; to change
	// the tier you must also give Model. Missing it shows up as "after switching the agent to
	// fast and syncing once, the tier silently falls back to the default", with zero errors.
	Speed string
	// System is the new system prompt; empty means unchanged.
	System string
	// Description is the new description; empty means unchanged.
	Description string
	// Tools is the new toolset list; when non-nil it replaces the old one as a whole.
	Tools []string
	// CustomTools are custom tool declarations, sent **in the same array** as Tools.
	//
	// ⚠️ **When you change Tools you must also give this**: tools is replaced as a whole, so
	// sending only the built-in toolset deletes the custom tools. The symptom is that the agent
	// suddenly can no longer wake itself up (`ama_wakeup` disappears from the model's tool
	// list), while Ark still returns 200. See the comment on toolItems.
	CustomTools []CustomToolSpec

	// Skills is the new skill reference list; **when non-nil it replaces the old one as a
	// whole** (that is the Ark-side behavior; the Ark "Update Agent" API doc says "replaced as
	// a whole when passed").
	//
	// ⚠️ "Not sent" and "empty array sent" are different: the former keeps the existing skills,
	// the latter **clears** them. So to clear skills you must send a non-nil empty slice, and in
	// Go `[]SkillRef{}` and nil look the same by len(). Use ClearSkills to state that intent
	// explicitly.
	Skills []SkillRef
	// ClearSkills, when true, explicitly empties skills (sends `"skills": []`).
	//
	// A separate bool instead of relying on "Skills is a non-nil empty slice": the latter is too
	// easily turned into nil by append/copy along the way, and then the behavior is "keep the
	// existing ones", a silent opposite action.
	ClearSkills bool
}

// Update updates an Agent; on success the version number goes up.
//
// Version must be the current version (take it from Get's result), otherwise the server rejects
// the write. This is an optimistic lock that prevents concurrent overwrites.
func (s *AgentService) Update(
	ctx context.Context,
	agentID string,
	upd AgentUpdate,
) (*agent.Agent, error) {
	if upd.Version == 0 {
		return nil, errors.New("ma: AgentUpdate.Version 必填（乐观并发控制）")
	}
	return s.UpdateRaw(ctx, agentID, upd.toRequest())
}

// toRequest turns the update input into the SDK request body.
func (u AgentUpdate) toRequest() *agent.UpdateAgentRequest {
	req := &agent.UpdateAgentRequest{
		Version:     u.Version,
		Name:        optAgentString(u.Name),
		System:      optAgentString(u.System),
		Description: optAgentString(u.Description),
	}
	if u.Model != "" {
		req.Model = agent.NewOptModelConfig(modelConfig(u.Model, u.Speed))
	}
	req.Tools = toolItems(u.Tools, u.CustomTools)
	if u.ClearSkills {
		// An explicit empty array = clear. **Do not shorten it to nil**: nil on the wire means
		// "not sent", Ark keeps the existing skills, and "unbind the last skill" silently does
		// nothing.
		req.Skills = []agent.AgentSkillRef{}
	} else {
		req.Skills = toSkillRefs(u.Skills)
	}
	return req
}

// UpdateRaw updates an Agent using the SDK's own request body.
func (s *AgentService) UpdateRaw(
	ctx context.Context,
	agentID string,
	req *agent.UpdateAgentRequest,
) (*agent.Agent, error) {
	return s.client.UpdateAgent(ctx, agentID, req)
}

// Delete deletes an Agent.
func (s *AgentService) Delete(ctx context.Context, agentID string) (*agent.DeleteAgentResponse, error) {
	return s.client.DeleteAgent(ctx, agentID)
}

// ListVersions lists the past versions of an Agent.
//
// The return type is the same as List (the server reuses the agent list structure); each
// element in Data is one past version of the same Agent.
func (s *AgentService) ListVersions(
	ctx context.Context,
	agentID string,
	page Page,
) (*agent.ListAgentsResponse, error) {
	return s.ListVersionsRaw(ctx, agentID, &agent.AgentsListVersionsParams{
		AgentId: agentID, // the params struct also carries the path ID; the SDK uses it to build the URL
		Limit:   optAgentInt32(page.Limit),
		Page:    optAgentString(page.Page),
	})
}

// ListVersionsRaw lists past versions using the SDK's own query parameters.
func (s *AgentService) ListVersionsRaw(
	ctx context.Context,
	agentID string,
	params *agent.AgentsListVersionsParams,
) (*agent.ListAgentsResponse, error) {
	return s.client.ListAgentVersions(ctx, agentID, params)
}

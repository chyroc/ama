// Package apimodel is AMA's domain model, matching the TypeScript types under web/src/types/
// one to one.
//
// # Naming conventions
//
// Fields mirroring Ark use snake_case (the same as the Ark API, easy to pass through and compare with the docs), and AMA's own concepts use
// camelCase. When you see snake_case, you know the authoritative source of that data is Ark. This convention comes from
// section 3 of docs/DESIGN.md; read it before renaming fields.
//
// # The three hard rules of JSON tags
//
// The frontend depends not only on field **types** but also on field **presence**, so a wrong tag is not a compile error;
// it only shows up on the page as odd, hard-to-debug problems. The three hard rules:
//
// One: **slice fields never have omitempty**, and must be non-nil empty slices before serialization.
// A nil slice encodes as null, while the contract requires []: FeishuPage.tsx:391 reads
// `deleting.boundAgentIds.length`, and gets a TypeError on null. Adding omitempty is worse:
// it makes **required** fields like members / skillIds / threads lose the whole key.
//
// Two: **numbers and booleans never have omitempty**. A 0 in TokenUsage is a valid value (in the seed, run_3's
// cache_read_input_tokens is 0), RunsPage.tsx:250 adds them up, and a missing key gives NaN;
// likewise verificationTokenSet: false is a meaningful state, not "not set".
//
// Three: **optional fields use omitempty; nullable fields use pointers without omitempty**. In TS these two are
// different things: `endedAt?: string` is "may be absent", `parent_thread_id: string | null`
// is "must be present, but may be null". Writing the latter with omitempty drops the key, breaking the TS type.
//
// # Timestamps are opaque strings
//
// The timestamps in the seed all come from JS's toISOString(), like 2026-08-25T10:00:00.000Z:
// exactly three decimals. Go's time.RFC3339 and RFC3339Nano both drop the .000, so the same moment
// would have two spellings. This is not just about looks: listRuns sorts descending by startedAt as a **string**
// (localeCompare at api.ts:601), and '.'(0x2E) < 'Z'(0x5A), so mixing the two shapes would
// directly change the sort result. So timestamps in this package are always string, never going through time.Time when stored or read;
// use NowISO() to generate a new timestamp.
package apimodel

import (
	"fmt"
	"strings"
	"time"
)

// isoLayout is the format of JS toISOString(): UTC, exactly three decimals, ending in a literal Z.
// time.RFC3339 (drops decimals) or time.RFC3339Nano (drops trailing zeros) cannot be used.
const isoLayout = "2006-01-02T15:04:05.000Z"

// NowISO returns the current moment as an ISO 8601 string, the same shape as the frontend's toISOString().
func NowISO() string {
	return time.Now().UTC().Format(isoLayout)
}

// FormatISO formats a moment into the same shape as the frontend.
func FormatISO(t time.Time) string {
	return t.UTC().Format(isoLayout)
}

// ParseISO parses an ISO 8601 string back into a moment.
//
// # Why this package needs a reverse function
//
// This package's approach is "timestamps are always opaque strings" (see the file header), and most places **should not** use it.
// It exists only for one case: **handing a timestamp from the frontend to the database for comparison**, where the target column is
// timestamptz rather than text. Currently only feedback_reports.created_at is like this
// (runs.started_at is text and keeps using string comparison; do not change it).
//
// Comparing a string directly against a timestamptz column makes PG do an implicit conversion: it can neither use the index,
// and may give unexpected results around time zones; neither reports an error.
//
// # All three shapes are accepted
//
// Strictly accepting only the exactly-three-decimals shape is wrong: this value comes from an HTTP query parameter, and a hand-typed
// `2026-08-28T00:00:00Z` (no decimals) and one with nanoseconds are both valid ISO 8601.
// Rejecting them would give the error "not a valid timestamp", while users see that what they wrote is clearly the standard format.
//
// **On parse failure return an error, not the zero value**: the zero value is year 1 AD, and as one end of a time window it would silently
// "filter nothing", turning a mistyped since parameter into "pull the whole table".
func ParseISO(s string) (time.Time, error) {
	for _, layout := range []string{isoLayout, time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("domain: 不是合法的 ISO 8601 时间戳: %q", s)
}

// ---------------------------------------------------------------------------
// Workspace (AMA's own)
// ---------------------------------------------------------------------------

// WorkspaceRole is a member's role in a workspace.
type WorkspaceRole string

const (
	RoleOwner  WorkspaceRole = "owner"
	RoleAdmin  WorkspaceRole = "admin"
	RoleMember WorkspaceRole = "member"
	RoleViewer WorkspaceRole = "viewer"
)

// WorkspaceMember is a workspace member, i.e. one row of the workspace_members table.
type WorkspaceMember struct {
	ID    string        `json:"id"`
	Name  string        `json:"name"`
	Email string        `json:"email"`
	Role  WorkspaceRole `json:"role"`
	// AvatarURL is the avatar address, **only a cache, not the source of truth**.
	//
	// It is brought along at invite time and never refreshed afterwards. So it goes stale over time; this is a **deliberately accepted** cost:
	// "looking it up on demand when rendering the member list" would be implemented as one serial request per member (N+1).
	//
	// A broken one does not matter: the frontend uses `<Avatar.Image src>` + `<Avatar.Fallback>` initials,
	// and Radix only renders `<img>` after the image has loaded, so "empty value", "loading" and "URL broken"
	// all fall back to the initial, never leaving an empty circle (same as auth.Identity.AvatarURL). **omitempty is part of the contract**: existing members do not have this field,
	// and an empty string would give the frontend one more shape the backend never produces.
	AvatarURL string `json:"avatarUrl,omitempty"`
	JoinedAt  string `json:"joinedAt"`
}

// Workspace is the ownership boundary of all resources; there is no "global agent".
type Workspace struct {
	ID string `json:"id"`
	// Name is the display name, changeable.
	Name string `json:"name"`
	// Key is the URL identifier, globally unique, [a-z0-9-]{2,32}. Changing the key changes all links.
	Key string `json:"key"`
	// Description is optional: after clearing it the body sent by the frontend does not have this key at all, so omitempty is needed,
	// otherwise it would emit "description": "" while the frontend expects the key to be absent.
	Description string `json:"description,omitempty"`
	CreatedAt   string `json:"createdAt"`
	// UpdatedAt is the moment of the last change (rename / change key / change description / add, edit or remove members).
	//
	// **No omitempty**: it is a required field, and a missing key would give the frontend undefined. Existing data was backfilled by a migration
	// to equal createdAt ("never changed"), not now() (which would make workspaces untouched for months
	// show as "just updated").
	UpdatedAt string `json:"updatedAt"`
	// Members is required and must be []: SettingsPage reads .length directly in several places.
	//
	// **In storage it is no longer in the doc** (the source of truth is the workspace_members association table),
	// but the HTTP contract keeps it as is: the read path has the Store fill it back. See fillMembers in core/store_pg.go
	// and that part in schema_pg.sql.
	Members []WorkspaceMember `json:"members"`

	// CreatedBy / UpdatedBy are the emails of the person who created this workspace and of the most recent editor.
	// Exactly the same meaning as the two on Agent (including both reasons for omitempty); see the long comment there.
	//
	// # ⚠️ It is not the same thing as "the first owner member"; do not use the latter instead
	//
	// When a workspace is created the creator is indeed written as an owner member (see core.CreateWorkspace), so it looks like
	// "looking up the owner tells who created it". But that inference can be wrong, and **being wrong reports no error**:
	//
	//   - **owner can be transferred, and there can be several**, and someone can also demote or remove themselves. After a transfer
	//     "the first owner" becomes the person who took over, while the workspace was not created by them;
	//
	// So "who created it" must have its own record. **Likewise do not give existing workspaces a guessed value**
	// (like the current first owner): that is exactly the value the points above would get wrong.
	CreatedBy string `json:"createdBy,omitempty"`
	UpdatedBy string `json:"updatedBy,omitempty"`
}

// ---------------------------------------------------------------------------
// Ark credentials (AMA's own, per workspace)
// ---------------------------------------------------------------------------

// ArkRegion is the Ark region. Values match the frontend's ARK_REGIONS.
type ArkRegion string

const (
	RegionCNBeijing    ArkRegion = "cn-beijing"
	RegionAPSoutheast1 ArkRegion = "ap-southeast-1"
)

// ArkCredentialCheck is the verification status of a credential. Ark has no dedicated "test key" endpoint; the actual implementation
// calls a read-only endpoint once with this key and judges by the response code.
type ArkCredentialCheck string

const (
	CheckUnchecked ArkCredentialCheck = "unchecked"
	CheckValid     ArkCredentialCheck = "valid"
	CheckInvalid   ArkCredentialCheck = "invalid"
)

// ArkCredentialSource says **where** an Ark credential comes from.
//
// # This is the core of the "three-level config" mechanism
//
//	SourceOverride  this level stores its own key
//	SourceInherit   this level **stores no** key, and reading falls back to the level above
//
// The fallback order of the three levels is **agent level → workspace level → account level (global)**; the more specific wins.
// The key point is that with inherit the lower level **has no copy at all**, so "change the upper level, and the lower levels referencing it change too"
// holds naturally, without any sync logic. Making it "copy one to the lower level" would be the complete opposite:
// changing the upper level would not propagate, and that is exactly the problem this feature solves.
//
// # An empty string must be equivalent to override
//
// Existing ark_secrets rows do not have this key (deserialized from the doc jsonb it is an empty string).
// If the empty string landed on the inherit side, **after the upgrade every configured workspace would suddenly switch to the global key**,
// without anyone clicking any switch, and most likely the global one does not even exist yet, so all chats in the workspace
// would report "please configure an Ark API Key first" together. So NormalizeArkSource narrows the empty string and any unrecognized
// value to override (= the current state). Same as feishu's TriggerMode, for exactly the same reason.
type ArkCredentialSource string

const (
	// SourceOverride is customized at this level, **the default** (and the only behavior before this mechanism was added).
	SourceOverride ArkCredentialSource = "override"
	// SourceInherit references the upper level's credential, falling back when read.
	//
	// At the workspace level it means "use the account-level one"; at the agent level it means "follow the workspace setting"
	// (and the workspace itself may be inherit too, falling back further to the account level: it chains).
	SourceInherit ArkCredentialSource = "inherit"
)

// NormalizeArkSource narrows the empty string and unrecognized values to override.
//
// **The read path and the decision path each normalize once, independently**: the former makes sure no empty string appears in responses,
// the latter makes sure that even if someone bypasses the read path and slips in a dirty value, the behavior falls back to the most conservative side
// (use your own key, instead of quietly switching to someone else's).
func NormalizeArkSource(s ArkCredentialSource) ArkCredentialSource {
	if s == SourceInherit {
		return SourceInherit
	}
	return SourceOverride
}

// ArkSaveWarning is "the save succeeded, but there is a sentence the user must see".
//
// Currently there is only one kind: the online verification during save did not complete (Ark timeout/5xx), and we let it through by the red line rule:
// a dependency failure must not stop users from saving config, but we must tell them "verification did not complete this time; you can click verify connectivity later".
//
// # Transient: only in the save endpoint's response, never stored, never in GET
//
// It describes something attached to "this one save request", not a property of the credential. Storing it would pop
// an outdated sentence again on the next refresh, and with multiple instances there is no good moment to clean it up.
type ArkSaveWarning struct {
	// Kind is a stable identifier for programs to tell apart; currently only VerifyUnavailable.
	Kind string `json:"kind"`
	// Message is a sentence shown directly to the user.
	Message string `json:"message"`
}

// ArkWarnVerifyUnavailable is the warning kind for "online verification did not complete but it was let through".
const ArkWarnVerifyUnavailable = "verify_unavailable"

// ArkCredential is the **public view** of the workspace-level Ark credential; the plaintext key is not here; see ArkSecret.
//
// When not configured the frontend gets an empty shell (ApiKeyMasked empty string, Region cn-beijing),
// not 404: SettingsPage.tsx:243 judges "not configured" by an empty mask, and "still loading" by null.
// Returning 404 would leave it on the skeleton forever.
type ArkCredential struct {
	WorkspaceID string `json:"workspaceId"`
	// ApiKeyMasked is the masked key, like abcd****...****wxyz. Empty string when not configured.
	//
	// ⚠️ When Source is inherit this is **the mask of the global key**, not an empty string:
	// empty would make the frontend judge it "not configured", while this workspace actually works.
	ApiKeyMasked string             `json:"apiKeyMasked"`
	Region       ArkRegion          `json:"region"`
	Check        ArkCredentialCheck `json:"check"`
	// Source says where this credential comes from: inherit (referencing the global one) or override (customized for this workspace).
	//
	// **No omitempty**: override is a meaningful value, and omitempty would erase the whole key when it equals the zero value.
	// The frontend relies on it to render "from global settings" or "customized for this workspace":
	// with the key absent that check gets undefined, neither branch matches, and nothing shows in the UI.
	Source ArkCredentialSource `json:"source"`
	// InheritFrom is the email of the one being referenced, only set when Source is inherit.
	//
	// The UI must say clearly "whose key this is": a workspace may have several admins, each with their own
	// account-level credential; without saying whose, nobody knows that replacing whose key would affect this workspace.
	//
	// **Has omitempty**: with override it should be absent, not an empty string.
	InheritFrom string `json:"inheritFrom,omitempty"`
	// The following four are all optional: updateArkRegion deletes CheckedAt / CheckError
	// (not set to empty), so they must have omitempty here for the keys to really disappear.
	CheckedAt  string `json:"checkedAt,omitempty"`
	CheckError string `json:"checkError,omitempty"`
	UpdatedAt  string `json:"updatedAt,omitempty"`
	// UpdatedBy is the email of the most recent editor; credential-type config must leave a trace.
	UpdatedBy string `json:"updatedBy,omitempty"`

	// Warning is the transient notice of this save; see ArkSaveWarning. GET does not carry it.
	Warning *ArkSaveWarning `json:"warning,omitempty"`
}

// ArkSecret is the internal form of the credential, with the plaintext key. **It never appears in any HTTP response**.
type ArkSecret struct {
	ArkCredential
	APIKey string
}

// GlobalArkSecret is the **account-level** Ark credential, with the plaintext key. Likewise it never goes into any HTTP response.
//
// # Its relation to ArkSecret
//
// When the workspace side's Source is inherit, reading falls back to here. **The workspace side keeps no copy**, so changing this one
// takes effect immediately for every workspace referencing it: exactly the linkage "configure once globally" wants.
//
// The primary key is Email (account level), not WorkspaceID.
type GlobalArkSecret struct {
	// Email is whom this credential belongs to. **It is the Actor ID**, already lowercased.
	Email string
	// ApiKeyMasked is the mask, which may go into responses (GlobalArkCredential carries it).
	ApiKeyMasked string
	Region       ArkRegion
	Check        ArkCredentialCheck
	CheckedAt    string
	CheckError   string
	UpdatedAt    string
	// APIKey is the plaintext, **flowing only on the server**.
	APIKey string
}

// GlobalArkCredential is the **public view** of the account-level credential; the plaintext is not here.
//
// Two types instead of reusing one: that one has WorkspaceID and Source,
// and in the account-level context neither field means anything (the global one belongs to no workspace and references nobody).
// Forcing them in would only give the frontend two always-empty fields, and then someone would start guessing what to fill in.
type GlobalArkCredential struct {
	// ApiKeyMasked is an empty string when not configured; the frontend judges "not configured yet" by it.
	ApiKeyMasked string             `json:"apiKeyMasked"`
	Region       ArkRegion          `json:"region"`
	Check        ArkCredentialCheck `json:"check"`
	CheckedAt    string             `json:"checkedAt,omitempty"`
	CheckError   string             `json:"checkError,omitempty"`
	UpdatedAt    string             `json:"updatedAt,omitempty"`
	// Warning is the transient notice of this save; see ArkSaveWarning. GET does not carry it.
	Warning *ArkSaveWarning `json:"warning,omitempty"`
}

// Credential gets the public view of the account-level credential.
func (g GlobalArkSecret) Credential() GlobalArkCredential {
	return GlobalArkCredential{
		ApiKeyMasked: g.ApiKeyMasked,
		Region:       g.Region,
		Check:        g.Check,
		CheckedAt:    g.CheckedAt,
		CheckError:   g.CheckError,
		UpdatedAt:    g.UpdatedAt,
	}
}

// ---------------------------------------------------------------------------
// Agent-level Ark credential (the most specific of the three levels)
// ---------------------------------------------------------------------------

// AgentArkCredential is the **public view** of the agent-level credential; the plaintext is not here.
//
// # "No such row" means "follow the workspace setting", without the user clicking anything
//
// This deliberately differs from the workspace level. The workspace referencing the account-level one requires **one explicit click**
// (UseGlobalArkCredential), because that is "switch to someone else's key", which should not be guessed.
// Whereas an agent following its workspace by default has always been its behavior: newly created agents must work out of the box.
// Requiring a click to "follow the workspace" would leave all existing agents suddenly without credentials after the upgrade.
//
// So the agent level has no stored inherit state: **a row = customized, no row = follow the workspace**.
type AgentArkCredential struct {
	AgentID string `json:"agentId"`
	// ApiKeyMasked is the mask. **Empty string when not configured**; the frontend judges "this agent follows the workspace" by it.
	ApiKeyMasked string             `json:"apiKeyMasked"`
	Region       ArkRegion          `json:"region"`
	Check        ArkCredentialCheck `json:"check"`
	// Custom says whether this agent has its own key.
	//
	// **No omitempty**: false is a meaningful value ("follow the workspace"), and omitempty would erase the whole key
	// when it equals the zero value; the frontend would get undefined, neither branch matches, and nothing shows in the UI.
	// Same reason as ArkCredential.Source.
	Custom bool `json:"custom"`
	// The following four are all optional, same as those four in ArkCredential.
	CheckedAt  string `json:"checkedAt,omitempty"`
	CheckError string `json:"checkError,omitempty"`
	UpdatedAt  string `json:"updatedAt,omitempty"`
	UpdatedBy  string `json:"updatedBy,omitempty"`

	// Warning is the transient notice of this save; see ArkSaveWarning. GET does not carry it.
	Warning *ArkSaveWarning `json:"warning,omitempty"`

	// Effective is the summary of **the credential actually in effect for this agent**, for the UI to show the source.
	//
	// # Why this extra layer
	//
	// Without it, an agent that "follows the workspace" could only show "follows the workspace setting" in the UI,
	// while what users really want to know is "which key is that, and does it work now". Without this information they would have to jump to
	// the workspace settings page, which may in turn show "from global settings", requiring another jump.
	Effective ArkEffective `json:"effective"`
}

// ArkEffective is a summary of "the Ark credential currently in effect", for display only.
//
// The most confusing thing about three-level config is "I configured three places; which one is actually used",
// so this struct exists to state that answer directly.
type ArkEffective struct {
	// Level is the level in effect: agent / workspace / global.
	//
	// **An empty string means none of the three levels is configured** (then ApiKeyMasked is also empty). No sentinel value like "none":
	// the frontend always judges "not configured" by whether the mask is empty, and one more value means one more check to keep in sync.
	Level ArkLevel `json:"level"`
	// ApiKeyMasked is the mask of the key in effect; empty string when none of the three levels is configured.
	ApiKeyMasked string             `json:"apiKeyMasked"`
	Region       ArkRegion          `json:"region"`
	Check        ArkCredentialCheck `json:"check"`
	// OwnerEmail is only set when Level is global: whose account-level credential that key is.
	//
	// The UI must say clearly "who configured this key": a workspace may have several admins, each with their own
	// account-level credential; without saying whose, nobody knows that replacing whose key would affect this.
	OwnerEmail string `json:"ownerEmail,omitempty"`
}

// ArkLevel is one level of the three-level config.
type ArkLevel string

const (
	// LevelAgent is configured by this agent itself.
	LevelAgent ArkLevel = "agent"
	// LevelWorkspace is configured by its workspace.
	LevelWorkspace ArkLevel = "workspace"
	// LevelGlobal is someone's account-level credential (the fallback when the workspace is inherit).
	LevelGlobal ArkLevel = "global"
)

// AgentArkSecret is the internal form of the agent-level credential, with the plaintext key.
// **It never appears in any HTTP response**, same as ArkSecret / GlobalArkSecret.
type AgentArkSecret struct {
	AgentID     string
	WorkspaceID string
	// APIKey is the plaintext, **flowing only on the server**.
	APIKey       string
	ApiKeyMasked string
	Region       ArkRegion
	Check        ArkCredentialCheck
	CheckedAt    string
	CheckError   string
	UpdatedAt    string
	UpdatedBy    string
}

// Credential gets the public view of the agent-level credential (without Effective, which Service builds on the fly).
func (a AgentArkSecret) Credential() AgentArkCredential {
	return AgentArkCredential{
		AgentID:      a.AgentID,
		ApiKeyMasked: a.ApiKeyMasked,
		Region:       a.Region,
		Check:        a.Check,
		Custom:       true,
		CheckedAt:    a.CheckedAt,
		CheckError:   a.CheckError,
		UpdatedAt:    a.UpdatedAt,
		UpdatedBy:    a.UpdatedBy,
	}
}

// ---------------------------------------------------------------------------
// Auto-fetch Ark API Keys from the Volcengine account (lists to choose from; none contain plaintext)
// ---------------------------------------------------------------------------

// VolcAccount is a Volcengine account, letting users choose one of the several accounts under their name.
//
// One person often has several (tested: 10), so they must choose. Guessing one would, when wrong, show as
// "the keys listed are not the ones I want", and they would have no way to tell why.
type VolcAccount struct {
	ID string `json:"id"`
	// Name is the account name, which users rely on to recognize "which one is the one I want".
	Name string `json:"name"`
	// Type looks like single_project / multiple, carried out as is for display.
	Type string `json:"type,omitempty"`
}

// ArkKeyOption is the **metadata** of one selectable Ark API Key.
//
// ⚠️ **There is no plaintext here, and never add one**. After the user chooses, the frontend only sends back the id,
// and the server fetches the plaintext itself and stores it encrypted: the plaintext only flows "server memory → database".
// Add a plaintext field here, and it would pass through the browser and into the devtools network panel.
type ArkKeyOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Masked is the mask given by Volcengine. It carries little information (often just a string of asterisks), but with it users dare to confirm
	// "this is the one I chose".
	Masked string `json:"masked,omitempty"`
	// Status looks like Active. **Non-Active ones must be visible to users**: choosing one will not work,
	// and the failure then would show as "invalid key", pointing to the credential itself rather than its status.
	Status string `json:"status,omitempty"`
	// ProjectName is the project it belongs to; one account may have several.
	ProjectName string `json:"projectName,omitempty"`
	// CreateTime / LastAccessTime help users recognize "which one I have been using all along".
	CreateTime     string `json:"createTime,omitempty"`
	LastAccessTime string `json:"lastAccessTime,omitempty"`
	// CreatedBy is the creator's user name, taken from `sys:ark:createdBy` in the Volcengine Tags.
	//
	// **This is the only answer to "which one is mine"**: several keys under the same Volcengine account may belong to
	// different people, and neither the name nor the mask shows ownership.
	//
	// ⚠️ **Both shapes have been seen**: a bare user name `alice`,
	// and a full email `alice@example.com`. Deciding "is it me" must compare both;
	// see isMyArkKey in web/src/types/domain.ts. Empty string when not available.
	CreatedBy string `json:"createdBy,omitempty"`
	// CreatedByUserID is the creator's numeric IAM id.
	//
	// More reliable than CreatedBy (no shape problem), but **we do not have "my IAM id"**:
	// AMA's identity is the email, so it cannot be used to decide "is it me", only as a display fallback.
	CreatedByUserID string `json:"createdByUserId,omitempty"`
}

// ---------------------------------------------------------------------------
// Model config (mirrors Ark)
// ---------------------------------------------------------------------------

// ModelConfig is the model bound to an agent and its parameters.
//
// Note Ark's Agent **has no** temperature / max_tokens / top_p; reasoning strength is expressed by
// reasoning_effort and thinking; do not invent fields by OpenAI habits.
//
// The three optional fields deliberately use string + omitempty, **not** *string:
// isDirty at AgentDetailPage.tsx:89 is a bare JSON.stringify string comparison,
// and if it emitted "thinking": null (the result of *string without omitempty), after a successful save
// "unsaved changes" would stay lit forever: that key does not exist in the body the frontend sends.
type ModelConfig struct {
	// ID is Ark's field name, not model.
	ID              string `json:"id"`
	Thinking        string `json:"thinking,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	ServiceTier     string `json:"service_tier,omitempty"`
}

// ---------------------------------------------------------------------------
// Agent (mirrors Ark + AMA extensions)
// ---------------------------------------------------------------------------

// AgentStatus is the agent's enabled state, AMA's own (Ark has no such concept).
//
// # Only two values, and "disabled" really blocks
//
// There used to be a third, `draft`. It was removed because **nothing read it to make any decision**:
// it was written when creating an agent, and nobody ever looked at it again. And what it meant to express, "not configured yet, do not take production traffic",
// already has a better carrier in this repo: a diagnosable report rather than a switch users flip by hand
// (see the FeishuReadiness part in docs/DESIGN.md, "installed does not mean usable").
//
// Keeping it was worse: users would mark a configured agent as "draft" thinking it would not take production traffic,
// while it ran just the same; a state that lies is worse than not having the state.
type AgentStatus string

// The prefix is Status instead of Agent, to avoid clashing with the AgentDraft struct.
const (
	StatusActive   AgentStatus = "active"
	StatusDisabled AgentStatus = "disabled"
)

// IsDisabled reports whether this agent has been disabled (= no new turn may be triggered).
//
// # Why `== disabled` instead of `!= active`
//
// **An empty string must be equivalent to "enabled", never "disabled".** This is not a style question:
//
//   - Existing agents in production may not have a status key in their doc at all (this field was added later),
//     so it reads back as an empty string. Writing `!= active` would make **all existing agents stop replying overnight**,
//     with the error saying "disabled", while users never disabled anything; the cause cannot be found from the symptom.
//   - Many unit tests do `store.CreateAgent(ctx, apimodel.Agent{ID: "a1"})` without setting Status
//     (core/store_test.go, usage_test.go and others), so those agents are empty strings too.
//   - Existing `draft` values land here likewise: allowed, behaving **exactly** as before the enum was removed
//     (draft could run back then too). So removing draft **is already correct without a data migration**.
//
// In other words: unknown values are always allowed, and only an explicit "disabled" blocks. Same as the discipline that unknown fields in config must not
// be fatal errors: the cost of blocking wrongly (the whole site stops replying) is far bigger than missing a block (keeping the status quo).
func (s AgentStatus) IsDisabled() bool {
	return s == StatusDisabled
}

// Stable ids of authorization sources. **Exactly the same as the provider ids on the core side.**
//
// Declared in domain instead of core: Agent.IntegrationIDs stores exactly these values, and domain is
// where that field is defined. Each provider in core also has an ID() method, and the two must be equal:
// a mismatch shows as "checked but not injected", with zero errors. A test on the core side pins this.
const (
	// IntegrationAMA is the AMA platform itself, injecting the three AMA_TOKEN variables.
	IntegrationAMA = "ama"

	// IntegrationLarkUser is Feishu **user** authorization, injecting the two variables lark-cli recognizes
	// (LARKSUITE_CLI_USER_ACCESS_TOKEN + LARKSUITE_CLI_APP_ID), so the agent in the container
	// can read group chat and thread history **with the Feishu identity of the workspace owner**.
	//
	// ⚠️ It differs from the other two: **the credential is stored in our database** (OAuth's refresh_token
	// must be persisted to renew), instead of exchanged fresh each time. For the reason see the file header of
	// core/integration_larkuser.go: that is not a license to "cache platform state"; the rule is "who holds the source of truth".
	IntegrationLarkUser = "lark-user"
)

// IntegrationKind is the **identity kind** of an authorization source: does it represent a real person, or an app.
//
// The decision is moved down to the backend, and **defaults to the safe side** (see IntegrationKindOf:
// unknown ids always count as personal authorization).
//
// # The difference between the two is not "who configured it" but "who takes the blame if it leaks"
//
//	KindPersonal        the credential represents **a real person**. With it, the agent can do what that employee
//	                    can do, and the audit log shows that person operating. So it **can only follow
//	                    the person who injected it**, and must never be bound to a workspace for others to share.
//	KindServiceAccount  the credential represents **an app**. It was designed for "the server calling on someone's behalf",
//	                    its permission scope is narrowed on the open platform by whoever configured it, and sharing among many people is its
//	                    legitimate use, so it can be bound to a workspace.
type IntegrationKind string

const (
	// IntegrationKindPersonal is personal authorization: the credential represents a real person.
	//
	// ⚠️ **This is the safe side, and the default for unknown ids.** Misclassifying a source into this kind
	// only means "it cannot be bound to a workspace" (users will ask); misclassifying into service account is a silent privilege escalation.
	IntegrationKindPersonal IntegrationKind = "personal"

	// IntegrationKindServiceAccount is service account authorization: the credential represents an app.
	IntegrationKindServiceAccount IntegrationKind = "service_account"
)

// serviceAccountIntegrations is **all** service-account authorization sources.
//
// ⚠️ **This is an allowlist, not a denylist**, and the direction is deliberate: when a new authorization source is added and someone forgets to classify it,
// it lands on the "personal" side (conservative, noticed by users), not on the "can be shared with the whole workspace"
// side (privilege escalation, zero errors).
//
// The rule is **whom the credential represents**, not "was the credential typed in by the user": the latter would put Feishu user authorization
// (obtained via OAuth, representing a real person) and cloud platform service accounts (hand-typed AK/SK, representing an app) on the same side,
// while the sharing safety of the two is completely opposite.
//
// Currently there are none: the open-source version only has personal authorization sources (AMA / Feishu user).
// When a source "representing an app" is added in the future, register it here.
var serviceAccountIntegrations = map[string]bool{}

// IntegrationKindOf reports which kind an authorization source id belongs to.
//
// ⚠️ **Unknown ids return personal** (the most conservative side); for the reason see the comment on IntegrationKind.
// Dangling ids are a real state: normalizeIntegrationIDs **deliberately does not check that ids exist**
// (see core/agent.go), so the database may contain sources that were already removed.
func IntegrationKindOf(id string) IntegrationKind {
	if serviceAccountIntegrations[strings.TrimSpace(id)] {
		return IntegrationKindServiceAccount
	}
	return IntegrationKindPersonal
}

// IsServiceAccountIntegration is the boolean version of IntegrationKindOf, reading more smoothly.
func IsServiceAccountIntegration(id string) bool {
	return IntegrationKindOf(id) == IntegrationKindServiceAccount
}

// IsPersonalIntegration reports whether this source is personal authorization.
func IsPersonalIntegration(id string) bool {
	return IntegrationKindOf(id) == IntegrationKindPersonal
}

// PersonalIntegrationIDs picks the personal authorizations out of a batch of authorization ids, keeping the original order.
//
// Used for difference computations like "which personal authorizations changed" (see core.ensureIntegrationsUnchanged).
// **Returns a non-nil empty slice**, the same convention as other list functions in this package.
func PersonalIntegrationIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if IsPersonalIntegration(id) {
			out = append(out, id)
		}
	}
	return out
}

// HostSource answers one question: is the host chosen by the agent **shared by the workspace** or **the creator's personal
// direct connection**? It is the same opposition as IntegrationKind (shareable by the workspace vs belonging to one real person),
// and the two sets of checks are exactly symmetric; see ensureExecutorAllowed in service/agent.
type HostSource string

const (
	// HostSourceWorkspace is workspace assignment: the machine is bound into the workspace in workspace_hosts,
	// and workspace admins can assign it to any agent in the workspace (the machine owner can also choose machines they bound themselves).
	// After the workspace unbinds it, existing agents are blocked the next time they create a session.
	HostSourceWorkspace HostSource = "workspace"

	// HostSourcePersonal is personal direct connection: the machine was registered by the agent's creator, without going through
	// workspace_hosts; only agent.CreatedBy can change this choice (not even workspace admins),
	// only the machine owner can open the web terminal, and workspace unbinding has nothing to do with it.
	HostSourcePersonal HostSource = "personal"
)

// EffectiveHostSource is **the only way to read** the host_source column.
//
// agents.host_source / runs.host_source are nullable columns: existing rows (agents bound to hosts before this feature launched)
// and rows written by old binaries during rolling deploys are all NULL. The meaning of NULL must be fixed: it equals
// HostSourceWorkspace: in v1 all hosts could only be used through workspace binding.
//
// ⚠️ **coalescing may only go through this function**; do not write a separate empty-value check in scan or at each call site:
// the mem store has no scan, and the empty string scanned from PG and the zero value in mem are two entry points; two normalizations would surely drift.
// Non-host executors always return an empty string (host_source only means something when ExecutorType=="host").
func EffectiveHostSource(executorType, hostSource string) string {
	if strings.TrimSpace(executorType) != "host" {
		return ""
	}
	switch HostSource(strings.TrimSpace(hostSource)) {
	case HostSourcePersonal:
		return string(HostSourcePersonal)
	default:
		// The empty string and any unrecognized value land on workspace assignment: it is the side that requires admin permission,
		// so defaulting here does not amplify permissions.
		return string(HostSourceWorkspace)
	}
}

// LarkUserApp is one Feishu app that "Feishu user authorization" can choose.
//
// # Why it cannot be stuffed into IntegrationState.Details
//
// Details is `{label, value}` name/value pairs, which can express "currently using cli_xxx",
// but cannot express "which three are available, whether each is authorized, which is current": that is a structured table.
// Forced into name/value pairs it could only be joined into a string, then the frontend would parse it, and **that kind of parsing always drifts**:
// change a separator on the backend and the frontend silently misaligns, with nothing reporting an error. So this provider gets
// a dedicated endpoint (GET /api/integrations/lark-user/apps) returning this structure.
//
// # ⚠️ Not a single field here may be a credential
//
// For the rule see that hard rule at the top of core/integration.go. app_id (cli_xxx) and the app name **are not**
// secrets: the former is visible to everyone in Feishu groups, and the latter is printed right next to the bot avatar; app_secret is,
// so this struct has no field for it at all: not "remember not to fill it" but **it cannot be filled in**.
type LarkUserApp struct {
	// AppID is the Feishu app id, like cli_xxx. **This is the picker's key**, globally unique.
	AppID string `json:"appId"`

	// Name is the app's display name.
	//
	// ⚠️ **It is not enough to tell them apart**: tested, all three apps of the same person were named "AMA Assistant" (all built from
	// the same doc). So in the UI the name is the main title, and AppID must be shown alongside as secondary info:
	// with only names, users face three identical options and cannot choose.
	Name string `json:"name"`

	// WorkspaceName is the display name of the workspace this app belongs to.
	//
	// It is given because **the answer to "which one should I use" is usually "which workspace am I working in now"**:
	// Feishu apps are workspace-level resources, and injection chooses by workspace (see feishu.UserAppResolver).
	// With duplicate names, this column is often the only thing users can tell apart by.
	WorkspaceName string `json:"workspaceName"`

	// Authorized reports whether this app **has been authorized**.
	//
	// With one person having several apps, the database can naturally have several tokens (the primary key of lark_user_tokens is
	// (actor, lark_app_id)), so this is a per-app property, not a global one.
	// The UI shows "ready / not authorized" by it; **not authorized uses warning, not danger**:
	// something fixable with one click should not look like a fault.
	Authorized bool `json:"authorized"`

	// Current reports whether this app is **the one currently in effect**.
	//
	// "Current" means "ResolveApp would pick it when no workspace is given", i.e. the one the user
	// sees on the authorization management page. Note injection picks separately by **the workspace the run is in** (see the comment there),
	// so current here is the default from the account's view, not "all agents use it".
	Current bool `json:"current"`
}

// LarkAppQuery is the condition of one "which Feishu app to use" query.
//
// # Why a struct instead of two or three methods
//
// Choosing an app has two constraints of **different dimensions**, and their priority is exactly the easiest thing to get wrong on this path:
//
//	WorkspaceID  hard constraint. When non-empty, only search in this workspace; **if not found, fail, and never fall back to another workspace**
//	PreferAppID  soft constraint. The one the user explicitly chose wins, but it may not override WorkspaceID
//
// Split into three methods ResolveApp / ResolveAppForWorkspace / ResolveAppByID,
// the rule "workspace is hard, preference is soft" would be scattered and written three times, and missing one shows as
// **a silent mismatch** (taking an app from another workspace), with no error. Collected into one struct, there is only one copy of the rule.
//
// ⚠️ Both fields being empty is valid: that is "give me any usable one", for callers without a workspace context
// and without a preference set.
type LarkAppQuery struct {
	// Actor is the subject (employee email), required.
	Actor string

	// WorkspaceID **pins** the choice to one workspace; empty means no limit.
	//
	// ⚠️ **The injection path must fill it in**. Tokens are bound to apps: using the token of workspace A's app
	// with the app_id of workspace B's app, lark-cli returns 99991668 "invalid token"; even if it happens to
	// work, the two apps have different visibility (cannot read that group). Neither failure points to
	// "the wrong app was chosen", so it must be chosen right at selection time.
	WorkspaceID string

	// PreferAppID is the app the user explicitly chose (cli_xxx); empty means take the first usable one in default order.
	//
	// ⚠️ It is a **soft** constraint: when this app is not in the WorkspaceID workspace it is ignored,
	// instead of failing the whole query; otherwise after a user chose workspace A's app in settings,
	// all agents in workspace B would stop working.
	PreferAppID string
}

// AgentEnv is one custom environment variable the user configured for this agent.
//
// # Why a slice of structs instead of a map
//
// The JSON serialization order of a map is **unstable** (Go sorts by key, JS keeps insertion order), and the frontend's isDirty
// is a bare `JSON.stringify` comparison: a map would make "nothing changed" sometimes count as changed,
// lighting up the save button by itself. A slice's order is under our control and stable.
//
// Also a map cannot express the "sensitive" flag (the value would have to become an object, so we might as well use a struct).
type AgentEnv struct {
	// Key is the env var name, its shape constrained by IsValidEnvKey.
	Key string `json:"key"`

	// Value is the value. **For sensitive items this field is still plaintext** (we need to inject it into the container, so it must be recoverable);
	// it just does not go into the system prompt, and the frontend masks it when echoing back. See the comment on Secret.
	Value string `json:"value"`

	// Secret marks whether this item is sensitive.
	//
	// ⚠️⚠️ **It is not a security guarantee; do not treat it as access control.** Sensitive items are injected into the container just like normal ones,
	// and the agent has bash: a single `env` reads the plaintext. This is exactly the same situation as platform credentials
	// (the "security boundary" in shared/infra/shelltools/credential.go admits it is an inherent cost, not a defect a reliable implementation
	// could remove).
	//
	// It really does only two things, both **reducing accidental leaks**:
	//
	//  1. **It does not go into the system prompt**, so it does not enter the model context, is not repeated by the model in replies,
	//     and is not stored permanently in RunEvent with the chat;
	//  2. **It does not sit in plaintext on the page**, so screenshots, screen sharing and passers-by cannot see it.
	//
	// So the UI text must honestly say "the agent can still read it". Calling it "secure" would make users put in
	// credentials that really should not be given to the agent, which is worse than not having this flag.
	Secret bool `json:"secret"`
}

// AgentEnvAlias injects a **platform-injected credential** once more under a variable name the user chooses.
//
// It is for compatibility with third-party CLIs that do not read our default variable names: for example a CLI only recognizes `MY_TOKEN`,
// while we inject `AMA_TOKEN`; declaring one `MY_TOKEN ← AMA_TOKEN`
// makes it run, without changing the CLI.
//
// # ⚠️⚠️ Why it is a "mapping" instead of letting users fill in a value: the whole reason this type exists
//
// **The next person will very likely "casually" change it to let users fill in a value themselves. That would open a privilege escalation hole.**
//
// The merge order in shared/infra/shelltools/pause.go has an explicitly written security boundary: **platform credentials must win last**.
// Because if a user could write `AMA_TOKEN=<someone else's token>` in custom env and have it take effect,
// that would be "swapping the identity in the container by filling a form": with the permission "can edit the agent" they would get
// "making the container call the platform with any identity". platformReservedEnvs is the first line of defense, and the merge order is the second.
//
// This type **goes around that boundary instead of tearing it down**: users only declare From (which platform credential to reference),
// and **the value is filled in by the server at the moment of injection**. So three things hold at once:
//
//  1. Users **never touch the raw credential**: it is not in the request body, the database, the HTTP response, or the frontend;
//  2. Neither line of defense needs to move (reserved keys still block direct overrides, and the platform still wins last);
//  3. **It inherits redaction naturally**: the alias value is exactly the same as the source, and shared/infra/secret replaces **by value**,
//     so wherever the source is scrubbed the alias is scrubbed too, with no second mechanism needed.
//
// The other way, "users fill in values", would have to answer "where is that value stored, how is it encrypted, who can see it",
// and this repo **has no at-rest encryption facility** (shared/infra/secret only has Mask and scrub-by-value).
//
// # Why a separate field instead of adding a From to AgentEnv
//
// Because renderEnvPrompt in core/envprompt.go **renders non-sensitive Envs one by one into the system
// prompt** (the `<harness:env>` block). Reusing AgentEnv would mean the credential value has a path into the model
// context: once in, it lands in RunEvent, gets repeated by the model, is pushed to the browser, and permanently.
// A separate field makes this **impossible by construction**, instead of guarding against it with one more if.
type AgentEnvAlias struct {
	// Key is the target variable name (the name injected into the container), its shape constrained by IsValidEnvKey.
	//
	// ⚠️ **It must not be a platform-reserved name**: that would bypass platformReservedEnvs to override platform credentials,
	// exactly the privilege escalation hole above. The write path and the injection point each block it; see core/agent.go and pause.go.
	Key string `json:"key"`

	// From is the source: which platform variable to reference. **It must be in shelltools.AliasableEnvKeys()**.
	//
	// It is a **fixed list**, not an arbitrary string: the list only takes those whose "value itself is a usable credential",
	// so "what can be aliased" is defined in only one place and cannot drift between two sides.
	From string `json:"from"`
}

// Limits of custom env.
//
// # These numbers are worked back from the downstream channel, not made up
//
// The downstream of env is `sandbox.ExecSpec.Envs`, sent through the SDK in a JSON request body (**not in argv**,
// see shared/infra/dockertools/session.go). So the 512 KiB ARG_MAX limit of skill packages
// **does not apply here**: that one exists because base64 travels with the command line.
//
// The real ceiling is when `execve` runs in the container:
//
//	a single KEY=VALUE     limited by MAX_ARG_STRLEN, 128 KiB on Linux (32 × PAGE_SIZE);
//	                       beyond it execve returns E2BIG right away, which shows as "every command in the container fails",
//	                       not pointing to this form at all.
//	total environment      shares ARG_MAX with argv (measured 2 MiB on this machine). And the agent's own commands
//	                       also spend this budget, so env must not eat it all.
//
// So a single value is capped at **32 KiB** (4x margin from the 128 KiB hard limit, enough for a certificate or a piece of JSON
// config), the total at **256 KiB** (only 1/8 of ARG_MAX, leaving the rest for the agent's command lines),
// and the count at **64** (the same order of magnitude as maxSkillFiles; any more and the form itself becomes unreadable).
//
// **What a loose limit buys is not capability, but failures whose root cause nobody can find**: same as that line in skillpkg.go.
const (
	// MaxAgentEnvs is the count limit.
	MaxAgentEnvs = 64

	// MaxAgentEnvValueBytes is the byte limit of a single value.
	MaxAgentEnvValueBytes = 32 << 10

	// MaxAgentEnvTotalBytes is the total byte limit of all keys and values.
	MaxAgentEnvTotalBytes = 256 << 10

	// MaxAgentEnvKeyBytes is the length limit of a variable name.
	//
	// There is no hard system limit (the name is only constrained by the total length of a single KEY=VALUE); 128 is "enough and
	// readable at a glance": overlong variable names would break the form layout, and that is not a capability.
	MaxAgentEnvKeyBytes = 128
)

// IsValidEnvKey reports whether a string can be an environment variable name.
//
// The rule is the POSIX one: **the first character is a letter or underscore, the rest are letters, digits or underscores**.
//
// # Why it must be validated instead of "letting the container report errors itself"
//
// Invalid names (containing `=`, spaces, Chinese, hyphens) behave completely differently at different stages, and **not one
// of those behaviors points to this form**:
//
//   - one with `=` makes `KEY=VALUE` parse into a truncated name, so the variable silently becomes something else;
//   - one with a space or hyphen is not a valid assignment in a shell at all, so `export` fails or it is treated as a command;
//   - and the SDK layer just puts the map into JSON **without any validation**, so if we do not block it nobody does.
//
// It is blocked on the write side (not just in the frontend) because the API is public: blocking in the frontend is only UX;
// blocking in core is the contract.
func IsValidEnvKey(k string) bool {
	if k == "" || len(k) > MaxAgentEnvKeyBytes {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
			// Valid in any position.
		case c >= '0' && c <= '9':
			// Digits **cannot come first** (`2FOO` is not a valid variable name in a shell).
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// FollowUpMode decides what happens when the user says something more while the agent is still running.
//
// # Two meanings
//
//	FollowUpRedirect  steer: interrupt the current turn and do the new message instead. **The default**
//	FollowUpQueue     queue: wait for this turn to finish, then send the new message
//
// # ⚠️ An empty string equals redirect, the **opposite** direction from other enums in this repo
//
// Other enums (AgentStatus / feishu.TriggerMode / ArkCredentialSource) all follow
// "the empty string lands on the most conservative side", because their aggressive side has real side effects (the whole site stops replying,
// the bot becomes chatty, quietly switching to someone else's key). This one is different:
//
//   - neither side causes a failure; the only difference is "which thing is done first";
//   - the product explicitly requires **steering by default** (when users change their mind their first instinct is "stop doing that"),
//     and existing agents' doc has no such key, so it reads back as an empty string.
//
// So letting the empty string land on redirect is **deliberate**: it makes "steer by default" apply equally to existing and new agents,
// without a one-off data migration. The cost is that "queue" must be chosen explicitly by users, and only then does the key exist in the database.
//
// ⚠️ So **do not** add omitempty to it: with it "queue" can be stored, but explicitly choosing back "redirect"
// would make the key disappear entirely, indistinguishable in storage from "never expressed a choice". The current behavior of the two is the same,
// but if the default changes again someday, those rows would change along with it, while the user clearly made a choice.
// Same reason as IntegrationIDs, "an empty value has meaning, so no omitempty".
type FollowUpMode string

const (
	// FollowUpRedirect is steer: interrupt the current turn and do the new message instead. **An empty string also equals it**.
	FollowUpRedirect FollowUpMode = "redirect"
	// FollowUpQueue is queue: send it after this turn finishes.
	FollowUpQueue FollowUpMode = "queue"
)

// NormalizeFollowUpMode narrows the empty string and unrecognized values to redirect (the default).
//
// Unknown values also land on redirect instead of erroring: the enum is not constrained by the database, and a dirty value should fall back to the default behavior,
// not make that agent stop replying from then on. Same rule as NormalizeTriggerMode, only the default side differs
// (there the default is the conservative side; here the default is the aggressive side; for the reason see the comment on FollowUpMode).
func NormalizeFollowUpMode(m FollowUpMode) FollowUpMode {
	if FollowUpMode(strings.TrimSpace(string(m))) == FollowUpQueue {
		return FollowUpQueue
	}
	return FollowUpRedirect
}

// ProgressDetail decides whether this agent shows detailed work content (the chain of thought) in Feishu.
//
// # Two meanings
//
//	ProgressDetailFull   show: one more chain-of-thought message in the group, laying out thinking and each tool call in real time. **The default**
//	ProgressDetailBrief  do not show: this turn has only "reaction + final answer"
//
// # ⚠️ An empty string equals full, the same direction as FollowUpMode (neither lands on the conservative side)
//
// The reason is also the same: the product requires **showing by default**, and existing agents' doc has no such key, so it reads back as an empty string.
// Letting the empty string land on full is **deliberate**; it makes "show by default" apply equally to existing and new agents,
// without a one-off data migration. The cost is that "do not show" must be chosen explicitly by users, and only then does the key exist in the database.
//
// **So do not make it a bool.** Go's bool zero value is false, while the default is "show":
// exactly the opposite direction, causing two zero-error failures: the chain of thought of all existing agents disappears overnight;
// and old frontends do not send this key on PATCH (see the warning on AgentDraft),
// so a setting the user never touched gets silently turned off. The same kind of trap as flipping-a-default.
//
// ⚠️ So **do not** add omitempty to it: with it "brief" can be stored, but explicitly choosing back "full"
// would make the key disappear entirely, indistinguishable in storage from "never expressed a choice". Exactly the same reason as FollowUpMode.
//
// # Why "do not show" means not creating the CoT at all, instead of passing cot_hidden
//
// Feishu's create-CoT API has a `cot_hidden` parameter, which the docs say "hides the COT message after the task completes",
// which sounds just right. **Tested, it does nothing of the sort**: it makes the message stay at "thinking" forever after it is sealed,
// neither hidden nor completed, while the server returns code: 0 all along. See the test notes in shared/infra/lark/cot.go.
//
// So brief is implemented by making turnProgress nil directly (that degradation path already exists),
// so this turn has only the reaction and the answer, and no chain-of-thought message ever appears in the group.
type ProgressDetail string

const (
	// ProgressDetailFull shows detailed work content. **An empty string also equals it**.
	ProgressDetailFull ProgressDetail = "full"
	// ProgressDetailBrief does not show it; this turn has only the reaction and the final answer.
	ProgressDetailBrief ProgressDetail = "brief"
)

// NormalizeProgressDetail narrows the empty string and unrecognized values to full (the default).
//
// Unknown values also land on full instead of erroring: the enum is not constrained by the database, and a dirty value should fall back to the default behavior
// (same rule as NormalizeFollowUpMode). Here the default side is "show",
// so a dirty value at most shows the user one more chain of thought, never making them lose information.
func NormalizeProgressDetail(d ProgressDetail) ProgressDetail {
	if ProgressDetail(strings.TrimSpace(string(d))) == ProgressDetailBrief {
		return ProgressDetailBrief
	}
	return ProgressDetailFull
}

// Agent is the definition of an agent.
type Agent struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspaceId"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// System is the system prompt; Ark's field name is system. Each agent has its own.
	System string      `json:"system"`
	Model  ModelConfig `json:"model"`
	// Version is Ark's optimistic lock: an update must send back the current version. It goes up by one on each update.
	Version int `json:"version"`
	// None of the following three slices has omitempty, and they must be non-nil:
	// toDraft copies them into the draft as is and then compares with JSON.stringify; null would make the page "dirty" forever.
	BuiltinTools []string `json:"builtin_tools"`
	// SkillIDs are **references** to workspace skills, not copies: when skill content changes, agents bound to it get it immediately.
	SkillIDs     []string    `json:"skillIds"`
	FeishuAppIDs []string    `json:"feishuAppIds"`
	Status       AgentStatus `json:"status"`
	// IntegrationIDs are which authorization sources this agent is allowed to inject credentials from (ama / lark-user / …).
	//
	// ⚠️⚠️ **No omitempty, and it must be non-nil**; both reasons cause zero-error failures:
	//
	//  1. An empty list **has meaning** (= inject nothing), and must mean the same as "the field does not exist",
	//     so there is no need for omitempty to tell "expressed a choice or not" apart.
	//  2. nil encodes as `null`, and the frontend's isDirty is a bare JSON.stringify comparison:
	//     `null` and `[]` are not equal, so the save button stays lit forever as soon as you open the detail page.
	//     The same hard rule as the three BuiltinTools / SkillIDs / FeishuAppIDs.
	//
	// **Order matters**: the frontend also compares with JSON.stringify, and ["a","b"] and ["b","a"] are not equal,
	// so the order read back must be stable. The write side sorts it in normalize (see core/agent.go).
	IntegrationIDs []string `json:"integrationIds"`

	// ExecutorType is the tool executor type: empty string (default) = Docker container, "host" = a host
	// registered by the user (see the three tables hosts / workspace_hosts / host_dispatches).
	// Written together with HostID into the executor_type / host_id columns of the agents table; an empty string is written as NULL.
	//
	// Has omitempty: an empty string means the same as "the field does not exist" (both the default Docker container); existing agents and
	// requests from old frontends do not have this key.
	ExecutorType string `json:"executorType,omitempty"`
	// HostID is the host id (hst_ prefix) used when ExecutorType=="host". A soft reference:
	// after the machine is revoked, the read path expresses invalidity as "binding does not exist"; no foreign key. An empty string is written as NULL.
	HostID string `json:"hostId,omitempty"`
	// HostSource is the source of this host: workspace (workspace assignment) / personal (the creator's personal
	// direct connection); constants are HostSource* in the same file. Only meaningful when ExecutorType=="host";
	// an empty string is the existing/old frontend form, normalized to workspace by the server (see EffectiveHostSource).
	// Written together with ExecutorType/HostID into the real column agents.host_source; the doc does not store this key;
	// an empty string is written as NULL.
	HostSource string `json:"hostSource,omitempty"`

	// Envs are the custom environment variables the user configured; see AgentEnv.
	//
	// **Also no omitempty, and it must be non-nil**, for exactly the same reason as the slices above:
	// nil encodes as null, and the frontend's isDirty is a bare JSON.stringify comparison,
	// where null and [] being unequal would keep the save button lit forever.
	//
	// **The order is the order the user entered, and must be kept as is**: this is an editable list, and reordering would make the row
	// the user is editing jump away. So it is **not sorted** here (the opposite of IntegrationIDs:
	// that is a set of checkboxes where order means nothing, so it is sorted for a stable byte shape).
	Envs []AgentEnv `json:"envs"`
	// EnvAliases are declarations of "inject some platform credential once more under another variable name"; see AgentEnvAlias.
	//
	// ⚠️ **Also no omitempty, and it must be non-nil**, for exactly the same reason as the slices above
	// (nil → null → the frontend's isDirty is "dirty" forever).
	//
	// **The order is kept as is** (like Envs, not sorted): this is also a user-editable list.
	EnvAliases []AgentEnvAlias `json:"envAliases"`
	// FollowUp decides whether the user saying the next thing in a row steers or queues; values in FollowUpMode.
	//
	// **The zero value (empty string) is the default, redirect (steer)**; always go through NormalizeFollowUpMode when reading,
	// do not compare directly: existing agents' doc does not have this key.
	// ⚠️ No omitempty; see that part on FollowUpMode for why.
	FollowUp FollowUpMode `json:"followUp"`
	// ProgressDetail decides whether detailed work content (the chain of thought) is shown in Feishu; values in ProgressDetail.
	//
	// **The zero value (empty string) is the default, full (show)**; always go through NormalizeProgressDetail when reading,
	// do not compare directly: existing agents' doc does not have this key.
	// ⚠️ No omitempty; see that part on ProgressDetail for why.
	ProgressDetail ProgressDetail `json:"progressDetail"`
	CreatedAt      string         `json:"createdAt"`
	UpdatedAt      string         `json:"updatedAt"`

	// CreatedBy / UpdatedBy are the emails of the person who created this agent and of the most recent editor.
	// Naming and meaning follow ArkCredential.UpdatedBy (whose reason is "credential-type config must leave a trace"),
	// without a separate scheme.
	//
	// **Only taken from auth.ActorFrom(ctx); AgentDraft deliberately has neither field**: the request body
	// can be changed by the caller, and letting it specify "who created it" would allow impersonation. Same rule as Issue in this file.
	//
	// # ⚠️ Both have omitempty, and that is part of the contract, not saving bytes
	//
	// **Existing agents can never get a creator filled in**: these two fields were only added by this change, and agents created before it
	// never recorded who created them, with no way to trace it (not in the jsonb doc, not on the Ark side either). So
	// "the key does not exist" is a **legitimate state that will persist for a long time**, meaning "we do not know who created it",
	// and it must be kept apart from "an empty string": the latter is a shape the backend never produces.
	//
	// The frontend renders it as a question-mark avatar and says clearly "creator not recorded", instead of leaving it blank or erroring;
	// see the file header of web/src/components/UserAvatar.tsx. **Do not fill existing rows with a guessed value
	// just for tidiness** (like the workspace owner): a wrong attribution would be read as fact, and it reports no error.
	//
	// ⚠️ Another independent reason: without omitempty TestSeedRoundTrip would turn red immediately: that case
	// decodes and re-encodes seed.json for a **byte-for-byte** comparison, and these two keys do not exist in the seed.
	CreatedBy string `json:"createdBy,omitempty"`
	UpdatedBy string `json:"updatedBy,omitempty"`
}

// AgentDraft is the input for creating/updating an agent.
//
// The frontend's updateAgent signature is Partial<AgentDraft>, but the actual call (AgentDetailPage.tsx:527)
// always passes the full object, so value types are used here instead of a bunch of pointers. Pointers would instead fall into the ModelConfig
// null trap.
type AgentDraft struct {
	Name         string      `json:"name"`
	Description  string      `json:"description"`
	System       string      `json:"system"`
	Model        ModelConfig `json:"model"`
	BuiltinTools []string    `json:"builtin_tools"`
	SkillIDs     []string    `json:"skillIds"`
	FeishuAppIDs []string    `json:"feishuAppIds"`
	Status       AgentStatus `json:"status"`
	// IntegrationIDs: see the field of the same name on Agent.
	//
	// ⚠️ **Old frontends do not send this key**, and then it parses as nil. The core side must normalize nil to
	// `[]` (inject nothing) instead of falling back to "inject platform credentials": **that is exactly where the old and new defaults point
	// in opposite directions**, and falling back would silently turn "the user explicitly removed all authorizations" back into injection.
	// Existing data is filled by a one-off migration, not by a read-path fallback: a fallback would mean you could never see whether the migration missed anything.
	IntegrationIDs []string `json:"integrationIds"`

	// Envs: see the field of the same name on Agent.
	//
	// ⚠️ The values of sensitive items are **plaintext** here (when saving, the frontend sends up the values the user newly entered/changed as is).
	// The frontend echoes them back masked, but that is only the display layer: to really change a sensitive value the whole value must be re-entered,
	// because a mask string sent back up cannot be told apart from "the user really wants to set the value to this string of dots".
	// This trade-off is written in the comments on the frontend side.
	Envs []AgentEnv `json:"envs"`

	// EnvAliases: see the field of the same name on Agent.
	//
	// ⚠️ Here there are **only variable names, no values**: values are filled in by the server at the moment of injection.
	// This is exactly where the "mapping, not filling values" design lands: the raw credential never passes through the request body.
	EnvAliases []AgentEnvAlias `json:"envAliases"`

	// FollowUp: see the field of the same name on Agent.
	//
	// ⚠️ **Old frontends do not send this key**, parsing as an empty string, and the empty string is redirect (the default),
	// so there is no need here to worry, as with IntegrationIDs, that "the fallback direction is reversed":
	// not expressing a choice and explicitly choosing steer are the same behavior anyway.
	FollowUp FollowUpMode `json:"followUp"`

	// ProgressDetail: see the field of the same name on Agent.
	//
	// ⚠️ **Old frontends do not send this key**, parsing as an empty string, and the empty string is full (the default),
	// so, like FollowUp, there is no need to worry that "the fallback direction is reversed":
	// not expressing a choice and explicitly choosing "show" are the same behavior anyway.
	// **This is exactly why it is a string enum and not a bool**: a bool's zero value false would land on
	// the "do not show" side, so every save from an old frontend would silently turn off the user's chain of thought.
	ProgressDetail ProgressDetail `json:"progressDetail"`

	// ExecutorType / HostID / HostSource: the tool executor choice, with the same meaning as the fields of the same name
	// on Agent.
	//
	// The frontend's toAgentDraft must always carry these three keys (when old frontends do not, they land as empty strings = Docker container,
	// exactly the default behavior); with an empty executorType, hostId/hostSource must also be empty strings.
	// An empty hostSource is normalized to workspace by the server (the existing v1 meaning).
	ExecutorType string `json:"executorType,omitempty"`
	HostID       string `json:"hostId,omitempty"`
	HostSource   string `json:"hostSource,omitempty"`
}

// ---------------------------------------------------------------------------
// User profiles (account-level, across workspaces)
// ---------------------------------------------------------------------------

// UserProfile is "what this email's person is called and which avatar they have", for the UI to render avatars.
//
// # Why it is a separate endpoint instead of stuffed into each entity
//
// The counterexample is right next door: `WorkspaceMember` carries its own two fields Name/AvatarURL, which is **deliberately accepted
// staleness**: the upstream directory only has one entry point, `?search=`, with no batch lookup by email, so it can only
// be copied at the moment of inviting (see WorkspaceMember.AvatarURL).
//
// **Agent creators are not bound by that limit**: the users table is right in the local database, and `where email = any($1)`
// looks up a batch in one SQL. Writing name/avatar into agents.doc would be worse: that is a full jsonb write,
// and the copied name would be **frozen forever at the moment the agent was created**, never updated after renames or avatar changes,
// with no error at all. So only the email is stored here (apimodel.Agent.CreatedBy),
// and display info is **looked up fresh every time**.
//
// # All three fields may be empty
//
// The users table allows rows with "only an email, and empty name and avatar_url": people who were invited but have not logged in yet
// are in this state (see auth_pg.sql). The frontend has a fallback for it (initials);
// see web/src/components/UserAvatar.tsx.
type UserProfile struct {
	// Email is the employee email, i.e. the Actor ID, already lowercased. **It is the key of this record**.
	Email string `json:"email"`
	// Name is the display name, **may be an empty string** (that person has not logged in yet).
	//
	// No omitempty: the frontend uses it for the `name || email` fallback; a present key with an empty value is
	// a shape it can handle, while a missing key would add another shape to check for.
	Name string `json:"name"`
	// AvatarURL is the avatar address, **only a cache, not the source of truth**, and the field may be absent entirely.
	//
	// Has omitempty, same reason as WorkspaceMember.AvatarURL: for people who never logged in it is an empty string,
	// and emitting "avatarUrl": "" would give the frontend one more shape the backend could avoid producing.
	// The frontend always uses `<Avatar.Image src>` + `<Avatar.Fallback>` initials,
	// covering all three "no image" cases uniformly; **do not write conditional rendering**.
	AvatarURL string `json:"avatarUrl,omitempty"`
}

// ---------------------------------------------------------------------------
// Skill (workspace-level asset)
// ---------------------------------------------------------------------------

// SkillSource tells self-built skills apart from built-in platform skills.
//
// **Uploaded skills are still custom.** Adding a value for "upload" was once considered, and that was wrong: upload describes
// **how it was created**, not **where it came from**; the source is still user-built. Whether it has attachments is decided by whether Skill.Files is empty,
// with no extra enum value needed.
//
// The body of builtin is maintained by the platform, written in core/skillbuiltin.go, and not stored. It drives
// three things no other field can express: visible in every workspace, not changeable by users, and a badge in the list.
//
// ⚠️ Ark's own protocol values include skill_hub (SkillTypeHub in shared/infra/ma/agents.go);
// **that is another axis**; do not mix them.
//
// ⚠️ **Adding a value requires updating that discriminated union in the frontend too** (SkillSource in web/src/types/domain.ts).
// That one is a union type, so missing it is a compile error; **while Go has no exhaustive check, and a missed place only
// silently takes default**. The known places to take care of are in the table at the top of core/skillbuiltin.go.
type SkillSource string

const (
	SkillCustom SkillSource = "custom"
	// SkillBuiltin is a built-in platform skill; the source of truth is core/skillbuiltin.go, not stored.
	SkillBuiltin SkillSource = "builtin"
)

// ValidSkillName reports whether a skill name meets Ark-side constraints: lowercase letters, digits and hyphens, not starting with a hyphen.
//
// Matches SKILL_NAME_PATTERN in the frontend's SkillsPage.tsx exactly. It is in domain because
// both upload parsing (core/skillpkg.go) and create validation need it, and writing one copy each would surely drift.
func ValidSkillName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0:
		default:
			return false
		}
	}
	return true
}

// SkillFile is a file in the skill package other than SKILL.md (scripts/ references/ and so on).
//
// # Why Content has omitempty
//
// This breaks this package's hard rule "slices have no omitempty"; it is **the only deliberate exception**, because it is not
// a field the frontend depends on: the list endpoint returns all skills of the workspace at once, and putting attachment bytes into every record would make
// the GET /skills response grow linearly with attachments (tens of KB × number of skills), while the list page only needs file names.
// So Content only has a value on the two internal paths "parse preview" and "inject into the container", and does not appear otherwise.
//
// The frontend type for it is `content?: string` (optional), not `string | null`:
// the key disappearing is expected behavior, not a bug. Size, on the other hand, always has a value, and the UI shows the size from it.
type SkillFile struct {
	// Path is the path relative to the skill root, like scripts/init.sh.
	// It has passed the Zip Slip check (see core/skillpkg.go), guaranteed to be a normalized relative path.
	Path string `json:"path"`
	// Size is the byte count. No omitempty: a 0-byte file is valid, and a missing key would make the UI compute NaN.
	Size int `json:"size"`
	// Content is the file content. See the part above for why it is the only omitempty exception.
	Content []byte `json:"content,omitempty"`

	// FileID points to the files table: non-empty means these bytes are in the BlobStore, and **that copy is what gets read**.
	//
	// Same meaning as IssueAttachment.FileID (non-empty = the read path uses the BlobStore).
	// ⚠️ This used to say "it only means one copy was written; the read path still only reads the content column",
	// which was before the second step of dual-writing-without-switching-reads; reads have been switched now, so do not reason from that sentence.
	//
	// ⚠️⚠️ **Empty is now an error, no longer "the bytes are in the content column".** After 4a (the first half of step four)
	// `skill_files.content` is neither written nor read, so an empty FileID means these bytes
	// are stored nowhere: the read path refuses to hand over the whole skill (see core.hydrateSkillFiles),
	// and the write path never produces it at all (see core.putSkillFileBlobs, which errors).
	// The only place empty values can still be seen is **existing dirty data**, and then an error is exactly what we want.
	// For the full argument see the end of repository/schema/files.sql.
	//
	// `json:"-"`, deliberately not in HTTP responses: the frontend need not know where the bytes are stored.
	FileID string `json:"-"`
}

// SkillPackage is the result of parsing an uploaded package, **not yet stored**.
//
// A separate type instead of reusing SkillDraft: the preview shows the attachment list, while SkillDraft is
// the input of the "hand-filled form" with no concept of files; adding Files to it would give the hand-filled path a field that is always empty.
type SkillPackage struct {
	Name         string `json:"name"`
	DisplayTitle string `json:"displayTitle"`
	Description  string `json:"description"`
	Instructions string `json:"instructions"`
	// Files does not include SKILL.md itself (its content is in Instructions).
	// No omitempty, and it must be non-nil: the frontend calls .map() on it directly.
	Files []SkillFile `json:"files"`
}

// Skill is a workspace-level skill asset.
type Skill struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspaceId"`
	// Name is the Ark-side skill name, [a-z0-9-].
	Name string `json:"name"`
	// DisplayTitle maps to Ark's field display_title.
	DisplayTitle string `json:"displayTitle"`
	// Description decides when the model calls the skill; how well it is written directly affects the hit rate.
	Description string `json:"description"`
	// Instructions is the skill body, i.e. the content of SKILL.md.
	Instructions string      `json:"instructions"`
	Source       SkillSource `json:"source"`
	// LatestVersion is the version number in **string** form (Ark's latest_version), not a number.
	// Exactly the opposite of Agent.Version being a number, easy to get backwards.
	LatestVersion string   `json:"latestVersion"`
	Tags          []string `json:"tags"`
	// Files are the files that come with the skill package (only from zip upload; hand-filled skills are always empty).
	//
	// # The omitempty here is required, not an oversight
	//
	// It breaks this package's hard rule "slices have no omitempty", for two reasons; without either one it would not hold:
	//
	//  1. **The seed does not have this key.** TestSeedRoundTrip in model/apimodel/apimodel_test.go
	//     decodes and re-encodes each seed skill and compares with the raw bytes of seed.json **byte for byte**.
	//     Without omitempty, serialization would add "files":[], and that test would turn red immediately, while seed.json
	//     is generated from the frontend's seed.ts and cannot be edited by hand (see core/seed.go).
	//  2. **The frontend reads it as optional.** In types/domain.ts it is `files?: SkillFile[]`, and pages use
	//     `skill.files ?? []`: a missing key and an empty array are the same thing to it.
	//
	// So the cost of this exception is contained: the key only disappears when "there are no attachments", and when there are no attachments the frontend
	// does not render the attachment area anyway. When there are attachments the key is always present, and always a non-empty array.
	Files     []SkillFile `json:"files,omitempty"`
	CreatedAt string      `json:"createdAt"`
	UpdatedAt string      `json:"updatedAt"`

	// CreatedBy / UpdatedBy are the emails of the person who created this skill and of the most recent editor.
	// **Exactly the same** meaning as the two on Agent (including both reasons for omitempty); see the long comment there;
	// only three differences related to skills are noted here.
	//
	// One: **built-in skills never have these two values**: they are code constants (core/skillbuiltin.go),
	// never in the skills table, and "who created it" does not apply. The frontend renders them as "creator not recorded",
	// the same presentation as existing skills, which is right: built-in platform skills really were not created by a person.
	//
	// Two: **for skills installed from the marketplace / imported from a code repository, CreatedBy is the person who clicked install**,
	// not the skill author. Author info is in Origin (that is its provenance); do not mix the two:
	// these two fields answer "who brought this row into this workspace".
	//
	// Three: **for a Fork copy, CreatedBy is the person who clicked Fork**, not inherited from the source skill. The copy is
	// a new, self-owned skill detached from its source (see ForkSkill), so ownership follows the action.
	CreatedBy string `json:"createdBy,omitempty"`
	UpdatedBy string `json:"updatedBy,omitempty"`
}

// SkillDraft is the input for creating/updating a skill.
//
// ⚠️ **Deliberately no CreatedBy / UpdatedBy**, same as AgentDraft: the request body can be changed by the caller,
// and accepting it would allow "creating skills in someone else's name". The value only comes from auth.ActorFrom(ctx).
type SkillDraft struct {
	Name         string   `json:"name"`
	DisplayTitle string   `json:"displayTitle"`
	Description  string   `json:"description"`
	Instructions string   `json:"instructions"`
	Tags         []string `json:"tags"`
}

// ---------------------------------------------------------------------------
// Feishu apps
// ---------------------------------------------------------------------------
//
// The Feishu app model **is not in this package**; it is in server/feishu (feishu.App).
//
// The reason: Feishu integration was changed to device-code self-registration, and the authoritative data of apps is produced by the feishu package during registration and inspection
// (including Readiness inspection results, tenant domain corrections and so on); another copy in this package would only become a second source of truth.
// When a Feishu app must be referenced, use its ID string (see Agent.FeishuAppIDs); do not rebuild the struct here.

// ---------------------------------------------------------------------------
// Run history
// ---------------------------------------------------------------------------

// RunStatus is the status of one run.
//
// Ark's SessionStatus has only four values: idle / running / terminated / rescheduling;
// requires_action is a frontend-derived state (on the Ark side it is still idle, but when stop_reason is requires_action,
// "stuck waiting for confirmation" and "normally idle" are two different things to users).
type RunStatus string

const (
	RunRunning        RunStatus = "running"
	RunIdle           RunStatus = "idle"
	RunRequiresAction RunStatus = "requires_action"
	RunRescheduling   RunStatus = "rescheduling"
	RunTerminated     RunStatus = "terminated"
)

// RunTrigger is the trigger source of a run.
type RunTrigger string

// The five trigger sources. **The values must match the RunTrigger union in the frontend web/src/types/domain.ts exactly**:
// that is a discriminated union; one extra value and TypeScript errors, one missing and RUN_TRIGGER_LABELS cannot find
// the label, which shows as blank in the UI.
//
// The first four existed long ago (the seed data already has feishu / api), but before this the Go side only had one
// unexported triggerManual in core/chat.go, and the rest were scattered string literals; the scheduler needs them outside the package, so they are
// gathered into constants here.
const (
	// TriggerManual is started by a person on the chat page. ChatPage uses it to filter the session list in the left column.
	TriggerManual RunTrigger = "manual"
	// TriggerFeishu is triggered by @-mentioning the bot in a Feishu group.
	TriggerFeishu RunTrigger = "feishu"
	// TriggerAPI is triggered by an external caller through the API.
	TriggerAPI RunTrigger = "api"
	// TriggerSchedule is triggered by a scheduled task when it is due (cron / interval / a user-scheduled oneshot).
	TriggerSchedule RunTrigger = "schedule"
	// TriggerWebhook is triggered by a webhook. **This round only reserves the value, without delivering**: keeping the value aligns the
	// frontend and backend enums once, saving another change to both sides' contract next time.
	TriggerWebhook RunTrigger = "webhook"
)

// TokenUsage is the token usage summary, summed from model_usage of all span.model_request_end.
// All four fields are required, and **none may have omitempty**: 0 is a valid value, and a missing key would make the frontend compute NaN.
type TokenUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// RunThread is one thread in a run; with multi-Agent collaboration each sub-agent has one.
type RunThread struct {
	// ID maps to Ark's session_thread_id.
	ID        string `json:"id"`
	AgentName string `json:"agent_name"`
	Status    string `json:"status"`
	// ParentThreadID is **nullable, not optional**: null means the main thread, and the key must exist.
	// So it is *string **without** omitempty: with it the key would disappear, breaking the TS type
	// `parent_thread_id: string | null`.
	ParentThreadID *string `json:"parent_thread_id"`
	StartedAt      string  `json:"startedAt"`
	// EndedAt is truly optional: in the seed the threads of run_4 / run_5 have not ended, and do not have this key.
	EndedAt string `json:"endedAt,omitempty"`
}

// PendingMessageSource says where a queued message was queued from.
type PendingMessageSource string

const (
	// PendingFromWeb is queued from the web chat page. **An empty string equals it** (existing rows do not have this field).
	PendingFromWeb PendingMessageSource = "web"
	// PendingFromFeishu is queued from a Feishu group.
	PendingFromFeishu PendingMessageSource = "feishu"
)

// PendingMessageStatus is the status of one queued message.
//
// ⚠️ **It is not "chat status"** (that is RunStatus, driven by Ark events). This only serves
// the multi-instance mutual exclusion of "who sends this one", and "tell the user when it cannot be sent".
type PendingMessageStatus string

const (
	// PendingWaiting is waiting for the previous turn to finish. **An empty string equals it**.
	PendingWaiting PendingMessageStatus = "pending"
	// PendingSending has been claimed by some instance and is being sent.
	PendingSending PendingMessageStatus = "sending"
	// PendingFailed failed to send repeatedly and was given up. The original text is kept for the user to resend or rewrite.
	PendingFailed PendingMessageStatus = "failed"
)

// PendingMessage is one queued message waiting to be sent.
//
// # It is not the same thing as "automatic resend after hitting the tool execution window"
//
// That one is a race of tens of milliseconds, retried in place by the server (sendEventsThroughWindow in core/chat.go).
// This one is **queued by the user on purpose**: they know the agent is still running and would have to wait a whole turn (tens of seconds to minutes),
// they just do not want to hold it in. The wait times differ by three orders of magnitude, and the handling is completely different.
//
// Stored in the database instead of only in browser state: closing the page / refreshing / multiple tabs / the Feishu path,
// in all four cases the frontend's queue would be wrong. See the file header of repository/schema/core_pending_pg.sql.
type PendingMessage struct {
	ID     string               `json:"id"`
	RunID  string               `json:"runId"`
	Text   string               `json:"text"`
	Status PendingMessageStatus `json:"status"`
	// Source is where it was queued from. **An empty string equals web**; always go through NormalizePendingSource.
	Source PendingMessageSource `json:"source"`
	// Actor is the person who queued it (email). Empty string when not available (then only admins can touch it).
	Actor string `json:"actor,omitempty"`
	// Attempts is how many times sending has been tried; at the limit it is marked failed.
	//
	// **No omitempty**: 0 is a valid value (not tried yet), and with it the key would disappear and the frontend would get undefined.
	Attempts int `json:"attempts"`
	// LastError is the reason of the last failure, showing the user "why it was not sent".
	LastError string `json:"lastError,omitempty"`
	// QueuedAt is the queueing moment; **it goes into the HTTP contract, so it is a fixed-length ISO string**.
	//
	// It and the timestamptz created_at in the database are two copies stored by the same write: that one is for database sorting,
	// this one is for frontend display. For the rule see the repo's convention on timestamp units and formats.
	QueuedAt string `json:"queuedAt"`
}

// NormalizePendingSource narrows the empty string and unrecognized values to web.
func NormalizePendingSource(s PendingMessageSource) PendingMessageSource {
	if s == PendingFromFeishu {
		return PendingFromFeishu
	}
	return PendingFromWeb
}

// NormalizePendingStatus narrows the empty string and unrecognized values to pending.
//
// Unknown values land on pending instead of failed: better to try once more than to silently declare dead a message that should have been sent:
// that would show as "queued but never sent", and users could not see why.
func NormalizePendingStatus(s PendingMessageStatus) PendingMessageStatus {
	switch s {
	case PendingSending:
		return PendingSending
	case PendingFailed:
		return PendingFailed
	default:
		return PendingWaiting
	}
}

// Run is one run, mapping to one Ark Session.
type Run struct {
	ID          string     `json:"id"`
	WorkspaceID string     `json:"workspaceId"`
	AgentID     string     `json:"agentId"`
	AgentName   string     `json:"agentName"`
	Title       string     `json:"title"`
	Status      RunStatus  `json:"status"`
	Trigger     RunTrigger `json:"trigger"`
	// TriggerDetail is like the Feishu group name or the caller identifier.
	TriggerDetail string `json:"triggerDetail,omitempty"`
	// ScheduleID points back to the scheduled task that triggered this turn; only set when Trigger is schedule.
	//
	// # Why this one field is really needed
	//
	// Schedule only stores LastRunID (the latest one, overwritten on every fire), and the Run side used to only have
	// `trigger: "schedule"`: **when a workspace has several scheduled tasks, their runs are mixed together and cannot be told apart**.
	// So the question "how has this task run historically" could not be answered at the data layer at all, while the value of scheduled tasks
	// lies exactly in long-term stability (consecutive failures, occasional failures, durations growing).
	//
	// ⚠️ **TriggerDetail cannot replace it**, although it already stores the task name (that place in scheduler.go):
	// names can be changed and duplicated; after a rename history breaks, and that breaking reports no error: the list just has fewer rows.
	// The link must be pinned to an immutable ID.
	//
	// # Existing data does not have this key
	//
	// Runs produced before this field are all missing it, and **it cannot be filled in** (it was not recorded then). So the detail page must be able to say
	// "earlier history cannot be found" instead of showing an empty list: the latter would be read as "this task never ran",
	// exactly the opposite of the truth. The check is schedule.runCount being greater than the number of rows found.
	//
	// Has omitempty: absent and empty string are equivalent (both mean "not triggered by a scheduled task"), and it keeps
	// the doc of manual / feishu / api runs free of one more always-empty key.
	ScheduleID string `json:"scheduleId,omitempty"`
	// HostID is the host id (hst_ prefix) snapshotted from the agent when this session was created; an empty string
	// means tools run in a Docker container. Like agents.host_id it is a soft-reference snapshot: later executor changes on the agent
	// do not go back to historical runs, and deleting the machine does not touch this column either; it is only for debugging by host, and the read path's
	// authoritative data is still the doc (GetRun gets hostId just by reading the doc). Has omitempty.
	HostID string `json:"hostId,omitempty"`
	// HostSource is the source (workspace/personal) snapshotted from the agent along with HostID when the session was created.
	// The permission tier of terminal tickets is judged by **this snapshot** (later source changes on the agent do not affect historical runs); an empty string
	// is interpreted as workspace, read through EffectiveHostSource. Has omitempty.
	HostSource string `json:"hostSource,omitempty"`
	StartedAt  string `json:"startedAt"`
	// LastActivityAt is the moment of the last event; **the session list sorts by it** (not StartedAt).
	//
	// Why a separate field is needed: Feishu reuses sessions by scope (feishu/scope.go), so the Nth turn of a group
	// is still appended to the Run of the first turn. Sorting only by StartedAt, a session that was just talked in
	// would still sit at the bottom of the list: users would think the message was lost, while it was only sorted at a position ten hours ago.
	//
	// Old data does not have this key, so sorting falls back to StartedAt (see the coalesce in store_pg.go
	// and lastActivityOf in store.go). omitempty is there exactly to keep "absent" absent,
	// instead of turning it into an empty string taking part in comparisons.
	LastActivityAt string `json:"lastActivityAt,omitempty"`
	// EndedAt / DurationMs are **missing** on runs that have not ended (run_4, run_5 in the seed),
	// not zero values. DurationMs uses a pointer so that 0 milliseconds is not confused with "not ended".
	EndedAt    string      `json:"endedAt,omitempty"`
	DurationMs *int64      `json:"durationMs,omitempty"`
	Usage      TokenUsage  `json:"usage"`
	Threads    []RunThread `json:"threads"`
	EventCount int         `json:"eventCount"`
	// ErrorSummary only exists on runs that errored (only run_3 in the seed).
	ErrorSummary string `json:"errorSummary,omitempty"`
	// AgentDeleted means the linked agent has been deleted, and this session is frozen (no more messages can be sent).
	//
	// **A derived field, not stored**: Agents are hard-deleted with no deleted_at, so it can only be computed when Service reads the Run
	// (see markAgentDeleted in core/ark.go). Same handling as feishu.App.BoundAgentIDs.
	//
	// **omitempty was added deliberately; this is the first written exception to the three hard rules above**, for two reasons:
	//
	// One: rule two is there to prevent NaN and keep "a meaningful false", and this field touches neither: a missing key and
	// false behave exactly the same in the frontend (both not frozen), with no third state to tell apart, so it actually
	// falls under rule three (optional fields) rather than rule two.
	//
	// Two: with it, false can never get into runs.doc. The runs table is "scalar columns + doc jsonb storing the whole
	// entity", and SaveRun marshals the whole Run into doc; without it every row would persist a derived snapshot that is
	// surely stale, and whose staleness cannot be seen.
	AgentDeleted bool `json:"agentDeleted,omitempty"`
	// AgentDisabled means the linked agent has been **disabled**, and this session likewise can only be viewed, not sent to.
	//
	// Side by side with AgentDeleted instead of merged into one "frozen" bool: the two need different user actions
	// (one goes to rebind/create new, the other switches the state back to enabled); merged, the frontend could no longer say what to do.
	// Same as that FeishuReadiness rule, "there are seven or eight reasons it cannot be used; returning only false leaves users trying them one by one".
	//
	// **A derived field, not stored, with omitempty**, from the same source and the same database lookup as AgentDeleted
	// (markAgentDeleted in core/ark.go); both reasons there apply to this field exactly.
	//
	// ⚠️ Unlike AgentDeleted, disabling is **reversible**. So this value goes back and forth with the agent's state,
	// and any approach that persists it would leave a snapshot whose staleness cannot be seen.
	AgentDisabled bool `json:"agentDisabled,omitempty"`
}

// IssueStatus is the handling status of an issue.
//
// **An enum, not a boolean** (`closed bool`): adding a level in the future needs no table structure change, and the frontend does not have to
// guess again "does true mean closed or open". Three levels already cover the real flow, and in_progress has its value:
// "someone is looking" and "nobody has picked it up" are completely different things to the reporter, and with only open/closed
// those two states look exactly the same.
type IssueStatus string

const (
	IssueOpen       IssueStatus = "open"
	IssueInProgress IssueStatus = "in_progress"
	IssueClosed     IssueStatus = "closed"
)

// ValidIssueStatus reports whether a status value is known. **Unknown is always rejected**; do not quietly change it to open:
// that would let a misspelled value from the frontend silently become "reopen", while the user thinks they closed it.
func ValidIssueStatus(s IssueStatus) bool {
	switch s {
	case IssueOpen, IssueInProgress, IssueClosed:
		return true
	default:
		return false
	}
}

// IssuePriority is the priority of an issue.
//
// **Defaults to P2** (see DefaultIssuePriority). The reason for not defaulting to P0 is practical: defaulting to the highest level means
// every new issue shouts "most urgent", so the P0 level immediately loses its meaning: the real P0s drown in
// a sea of P0s, which is exactly the problem priority is meant to solve.
type IssuePriority string

const (
	IssueP0 IssuePriority = "p0"
	IssueP1 IssuePriority = "p1"
	IssueP2 IssuePriority = "p2"
)

// DefaultIssuePriority is the level used when no priority is given.
const DefaultIssuePriority = IssueP2

// ValidIssuePriority reports whether a priority value is known. Same as ValidIssueStatus: unknown is always rejected.
func ValidIssuePriority(p IssuePriority) bool {
	switch p {
	case IssueP0, IssueP1, IssueP2:
		return true
	default:
		return false
	}
}

// Issue is a workspace-level issue record.
//
// It is **deliberately a separate entity** from feedback_reports; do not merge them (the reason is in core/README-issue.md):
// that table is machine-collected, its content rebuilt from an allowlist, cross-workspace, and deduplicated by a unique index; this one is human-written
// free text, workspace-level, with status flow and comments, and two people reporting the same thing should give two entries.
type Issue struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspaceId"`
	// Title is **guaranteed non-empty** when stored: users may leave it blank, but Service extracts one from Body
	// (see shared/gopkg/mdtitle/mdtitle.go). An empty title is an invisible row in the list.
	Title string `json:"title"`
	// Body is the Markdown body, may be empty (filling only the title is valid).
	//
	// No omitempty: the frontend detail page hands it straight to the Markdown renderer, and a missing key would give undefined.
	Body     string        `json:"body"`
	Status   IssueStatus   `json:"status"`
	Priority IssuePriority `json:"priority"`
	// Author is the creator's employee email (Actor).
	//
	// **Only taken from auth.ActorFrom(ctx); the request body has no such field**: the request body can be changed by the caller,
	// and accepting it would allow "filing issues in someone else's name".
	Author    string `json:"author"`
	CreatedAt string `json:"createdAt"`
	// UpdatedAt must be refreshed on title / body / status / priority changes, stamped in one place by Service.
	UpdatedAt string `json:"updatedAt"`
	// CommentCount and AttachmentCount are **derived fields**; the list page needs them to show
	// "3 comments" without querying each one (N+1).
	//
	// No omitempty (hard rule two: a missing numeric key would make the frontend compute NaN).
	CommentCount    int `json:"commentCount"`
	AttachmentCount int `json:"attachmentCount"`

	// Origin marks who produced this issue. **Empty string = written by a person** (all existing ones are).
	//
	// The only reason to add it is to give dedup an **explicit scope**; see IssueOriginSchedule.
	Origin IssueOrigin `json:"origin,omitempty"`

	// Fingerprint is the check for "the same issue", **only meaningful for machine-produced issues**.
	//
	// The human-written path did not change a word: Origin and Fingerprint both stay empty,
	// and the unique index is partial (only covering rows with origin='schedule' and a non-empty fingerprint),
	// so the invariant "two people reporting the same thing should give two entries" holds as is.
	Fingerprint string `json:"fingerprint,omitempty"`
}

// IssueOrigin marks who produced an issue.
//
// # ⚠️ Why it was added: dedup must have a scope
//
// README-issue.md and schema_issue_pg.sql both say "issues exactly **must not be deduplicated**: two people reporting
// the same thing should give two entries". **That still holds, and this field does not overturn it**, because it is about **people**:
// two people each hitting the same phenomenon are two independent testimonies, and merging would lose "how many people are affected",
// the key information for judging priority.
//
// Bot inspection is another matter: the same scheduled task rescans **the same batch of material** every X minutes,
// and as long as the problem persists, it "finds" the same thing every round. Without dedup the result is not "a few more testimonies",
// but hundreds of identical records drowning this workspace's issue list, and then the real issues reported by people cannot be found.
//
// So the check is **who produced it**, not "does the content look alike":
//
//	Origin empty string (person)   never deduplicated, nothing merged
//	IssueOriginSchedule            same source + same fingerprint → merged into one + a comment appended
//
// The two sources **are never merged with each other** (the partial unique index includes origin),
// so the bot never swallows issues reported by people, and vice versa.
type IssueOrigin string

const (
	// IssueOriginSchedule means this issue was produced by a scheduled inspection task.
	//
	// ⚠️ **Do not delete this constant**: the database already has existing rows with origin='schedule',
	// and the partial unique index issues_bot_fingerprint_key scopes by exactly it.
	// Deleting it would only make those rows read back a value nobody recognizes.
	//
	// The path that produced it (delivery=issue, created by the server on the agent's behalf) has been removed; see
	// the comment on apimodel.DeliveryKind: now the inspection agent calls `ama issue create` itself,
	// and the issues it creates have an empty origin (going through the endpoint people use). So new rows will not have this value.
	IssueOriginSchedule IssueOrigin = "schedule"
)

// ValidIssueOrigin reports whether an origin value is known. **An empty string is valid**: it means "written by a person",
// and it is the value of all existing rows. Same as ValidIssueStatus: unknown is always rejected.
func ValidIssueOrigin(o IssueOrigin) bool {
	switch o {
	case "", IssueOriginSchedule:
		return true
	default:
		return false
	}
}

// IssueDraft is the input for creating/editing an issue.
//
// The four fields have **pointer or nullable meaning** for different reasons; do not unify them: Title and Body may be empty strings
// (that is the real operation "clear it"), while an empty Status and Priority mean "do not change this time".
type IssueDraft struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	// When Status is empty: creation uses open; update means do not change this item.
	Status IssueStatus `json:"status,omitempty"`
	// When Priority is empty: creation uses DefaultIssuePriority (P2); update means do not change this item.
	Priority IssuePriority `json:"priority,omitempty"`
}

// IssueComment is one comment under an issue. **Editing is supported**, so it has UpdatedAt.
type IssueComment struct {
	ID      string `json:"id"`
	IssueID string `json:"issueId"`
	// WorkspaceID is stored redundantly: deleting a workspace can cascade by it directly without first joining back to issues.
	WorkspaceID string `json:"workspaceId"`
	Body        string `json:"body"`
	Author      string `json:"author"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
}

// IssueAttachment is the **metadata** of an issue's attachment (image or document).
//
// ⚠️ **It contains no content bytes**, the opposite of SkillFile. The reason is that this type appears in list responses
// (one issue may have 20 attachments), and putting bytes into JSON would make the response grow linearly with attachments,
// while the frontend only needs "what it is called, how big, what the URL is". The bytes go through a separate GET route.
type IssueAttachment struct {
	ID      string `json:"id"`
	IssueID string `json:"issueId"`
	// WorkspaceID is **the basis of the privilege check**: authz resolves it from the attachment id, then checks whether the subject is
	// a member of this workspace. It is stored as a redundant column instead of joining issues, so that resolution queries only one table.
	WorkspaceID string `json:"workspaceId"`
	// Filename is the original file name at upload, used only for display and Content-Disposition.
	//
	// **It takes part in no path building**: the URL only has the ID. Putting the file name into a path would require handling path traversal and encoding,
	// and it has no value for <img src>.
	Filename string `json:"filename"`
	// ContentType is the type **sniffed by the server**, not the one the client declared.
	//
	// A client saying image/png while the content is HTML is the most classic attack, so what is stored here is always the result of
	// http.DetectContentType (also cross-checked with the extension; see shared/gopkg/attachment/attachment.go).
	ContentType string `json:"contentType"`
	// Size is the byte count. No omitempty: 0-byte files are rejected, but a missing key would make the UI compute NaN.
	Size   int    `json:"size"`
	Author string `json:"author"`
	// URL is a **derived field**, like /api/issue-attachments/att_x.
	//
	// Built by the server instead of letting the frontend build it: this path is the backend's business; if the frontend built it, changing the route would mean changing two places,
	// and missing the frontend one shows as "all images broken". It can go straight into <img src>: same-origin requests carry
	// the HttpOnly Cookie, so authentication works automatically (see core/README-issue.md).
	URL string `json:"url"`
	// CreatedAt is the upload moment. Attachments do not support changing content (to replace, delete and upload again), so there is no UpdatedAt.
	CreatedAt string `json:"createdAt"`

	// FileID points to the files table; the bytes are in object storage (files.bucket + files.object_key).
	// **Always non-empty**: PG does not store attachment bytes, and an attachment without file_id cannot be read back (the column is not null).
	//
	// ⚠️ **`json:"-"`, deliberately not in HTTP responses.** The frontend need not know where the bytes are stored: it only uses
	// the URL to get the content, and that URL is exactly the same in both cases. Exposing it would make it a de facto contract,
	// and changing the storage location later would require touching the frontend.
	FileID string `json:"-"`
}

// ---------------------------------------------------------------------------
// Product suggestions (Suggestion, platform-level)
// ---------------------------------------------------------------------------
//
// ⚠️ **This is the third kind of "something is wrong" thing, not the same as Issue or FeedbackReport.**
// For the full comparison of the three see core/README-suggestion.md; the one-line version:
//
//	FeedbackReport  machine-collected error stream  cross-workspace, deduplicated, users cannot see their own entries
//	Issue           human-written to-dos            **workspace-level**, read by workspace members by role
//	Suggestion      human-written product feedback  **platform-level, with no concept of workspaces at all**
//
// **Why it is not called feedback**: that word is already taken in this repo by "automatic error collection"
// (the feedback_reports table / /api/feedback / feedback.readers / the feedback.* i18n keys).
// Using it again would keep the two things confused in code, logs and conversations for a long time, and this repo already
// wrote a whole section of docs to tell apart "issue and feedback have similar names".

// SuggestionStatus is the handling status of a suggestion.
//
// **Two states, not three like issues.** in_progress is valuable for issues because the reporter
// is right in this workspace waiting for the result, and "someone is looking" and "nobody has picked it up" are two things to them. Suggestions are different:
// the person who made it only wants to know "has this been handled", and how far along the platform staff are internally should be stated in the reply text
// (a reply can say "in the next version"; an enum level cannot). An extra intermediate state nobody maintains
// would only leave half the entries in the list stuck there forever.
//
// Adding levels is cheap (same as issues: there is no check constraint, so adding a level needs no DDL migration),
// while **removing levels requires touching existing data**, so it starts with two states.
type SuggestionStatus string

const (
	// SuggestionOpen is unresolved. The default at creation.
	SuggestionOpen SuggestionStatus = "open"
	// SuggestionResolved is resolved.
	SuggestionResolved SuggestionStatus = "resolved"
)

// ValidSuggestionStatus reports whether this is a known status.
//
// ⚠️ **Unknown is always rejected, never quietly changed to open** (this discipline is copied from ValidIssueStatus).
// The consequence of silent correction: a caller misspells the status name (`resolve` missing a d, `Resolved` capitalized),
// the endpoint returns 200 while the suggestion's status did not become what they wanted: they think it is changed,
// while the platform staff still see "unresolved". A noisy error is better than a silently wrong status.
func ValidSuggestionStatus(s SuggestionStatus) bool {
	switch s {
	case SuggestionOpen, SuggestionResolved:
		return true
	default:
		return false
	}
}

// SuggestionOrigin says who wrote a reply.
//
// **This layer is reserved for "letting an agent handle it automatically later"** (not done this time, but the data model must leave the way open).
// An empty string means written by a person, as all existing and current ones are; non-empty means machine-produced.
//
// ⚠️ It **has a different purpose from IssueOrigin; do not copy the reasoning from there**: there origin is in the unique index,
// deciding the dedup scope; here it **takes part in no unique constraint**, and is only used to distinguish "this was replied by a bot" in the UI.
// Suggestions **are deliberately never deduplicated**; for the reason see core/README-suggestion.md.
type SuggestionOrigin string

const (
	// SuggestionOriginAgent means this reply was produced automatically by an agent.
	SuggestionOriginAgent SuggestionOrigin = "agent"
)

// ValidSuggestionOrigin reports whether this is a known origin. An empty string is valid (= written by a person).
func ValidSuggestionOrigin(o SuggestionOrigin) bool {
	switch o {
	case "", SuggestionOriginAgent:
		return true
	default:
		return false
	}
}

// Suggestion is one opinion or suggestion about the AMA platform itself.
//
// **Platform-level: no WorkspaceID field, deliberately.** It is not "a nullable workspace" (that is the shape of
// FeedbackReport); **this concept has no workspace dimension at all**: users are giving feedback to the platform,
// not to some workspace. Keeping an always-empty column would make the next person think it should be filtered by workspace.
//
// It follows that authorization cannot go through authz either: that model is "actor + workspace → role", and there is no workspace
// to judge here. The gate is in the Service layer (same as ListFeedback); see core/suggestion.go.
type Suggestion struct {
	ID string `json:"id"`
	// Title is the title. **Guaranteed non-empty when stored**: users may leave it blank, and Service extracts it from the body
	// (reusing DeriveIssueTitle), with a fallback when nothing can be extracted.
	Title string `json:"title"`
	// Body is the Markdown body, may be empty (filling only the title is valid).
	//
	// No omitempty: an empty body and "no such field" are two different things in the frontend, and a missing key would give
	// the edit box undefined (same reason as Issue.Body).
	Body   string           `json:"body"`
	Status SuggestionStatus `json:"status"`
	// Author is the employee email of the person who made the suggestion.
	//
	// **Real-name, but not publicly shown**: only platform staff can see who it is (shown in the list), and the person who made it
	// can only see their own, so "colleagues seeing what I suggested" does not happen.
	// Real names are the precondition for replies and follow-ups: if anonymous, when platform staff reply "can you describe the specific scenario",
	// nobody would see it. The value **only comes from context**; the request body has no such field.
	Author    string `json:"author"`
	CreatedAt string `json:"createdAt"`
	// UpdatedAt is refreshed on title / body / status changes.
	UpdatedAt string `json:"updatedAt"`
	// ReplyCount is the number of replies, **a derived field computed by the server on the fly** (not stored).
	//
	// Required, no omitempty: the list shows the count directly from it, and a missing key would compute NaN
	// (same as Issue.CommentCount). Storing a copy would require updating it on every add/delete,
	// and missing one place shows as "the list says 3 replies, but clicking in shows only 2", with no error.
	ReplyCount int `json:"replyCount"`
	// AttachmentCount: same as above.
	AttachmentCount int `json:"attachmentCount"`
}

// SuggestionDraft is the input for creating / editing a suggestion.
//
// ⚠️ **The two kinds of "empty" mean different things**, same as IssueDraft:
//
//	title / body sent as empty string  →  really clear it (clearing the title extracts it from the body again)
//	status not sent                    →  do not change this item (creation uses the default)
//
// Value types instead of pointers; the cost is that "both empty" must be told apart by a heuristic rule
// (see Service.UpdateSuggestion), and the gain is that the frontend need not tell null from undefined.
type SuggestionDraft struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	// An empty Status means do not change it. **Unrecognized values are always rejected**, not quietly changed to open.
	Status SuggestionStatus `json:"status,omitempty"`
}

// SuggestionReply is one reply under a suggestion.
//
// Platform staff replies and user follow-up questions use **the same table and the same path**, told apart by Author and IsStaff,
// not by two types. Split into two kinds, the check "who can reply" would be scattered in two places, while it has only one rule.
type SuggestionReply struct {
	ID           string `json:"id"`
	SuggestionID string `json:"suggestionId"`
	Body         string `json:"body"`
	Author       string `json:"author"`
	// IsStaff means this reply came from platform staff; **it is a stored real column, not computed on the fly**.
	//
	// ⚠️ **It must not be computed at read time by comparing Author with the allowlist**: the allowlist changes,
	// and after someone changes roles and is removed from the list, the reply they once wrote as platform staff would **retroactively**
	// become a user reply: historical records should not drift with config. At the moment of writing they really were platform staff,
	// and that is the fact.
	IsStaff bool `json:"isStaff"`
	// Origin is who produced it; empty / absent means written by a person, and "agent" means machine-produced.
	// Reserved for "letting an agent handle it automatically later"; this time there are only human-written ones.
	Origin    SuggestionOrigin `json:"origin,omitempty"`
	CreatedAt string           `json:"createdAt"`
	// UpdatedAt differing from CreatedAt means it was edited.
	UpdatedAt string `json:"updatedAt"`
}

// SuggestionAttachment is the attachment **metadata** of a suggestion. ⚠️ It contains no content bytes.
//
// The shape matches IssueAttachment exactly (minus WorkspaceID, since suggestions have no workspace dimension),
// and both the upload and streaming paths fully reuse the type sniffing and response header building of shared/gopkg/attachment/attachment.go:
// that three-layer defense (allowlist+sniffing / nosniff / Content-Disposition) is independent of the entity,
// and a copy would only make the two drift apart slowly, and what drifts apart are security holes.
type SuggestionAttachment struct {
	ID           string `json:"id"`
	SuggestionID string `json:"suggestionId"`
	Filename     string `json:"filename"`
	// ContentType is the type **sniffed by the server**, not the one the client declared.
	ContentType string `json:"contentType"`
	Size        int    `json:"size"`
	Author      string `json:"author"`
	// URL looks like /api/suggestion-attachments/sat_x, built by the server.
	//
	// ⚠️ **It is not public**: read permission is the same as the owning suggestion (the person who made it + platform staff),
	// and anyone else getting the URL still gets 403. See SuggestionAttachmentContent in core/suggestion.go.
	URL       string `json:"url"`
	CreatedAt string `json:"createdAt"`

	// FileID points to the files table, with exactly the same meaning as IssueAttachment.FileID
	// (bytes in object storage, always non-empty), and likewise `json:"-"`, not in responses.
	FileID string `json:"-"`
}

// AgentSyncStatus is the status of one "sync the agent to Ark" task.
//
// ⚠️ **It is not the agent's status** (that is AgentStatus, active / disabled), nor the chat
// status (RunStatus). This one only answers "the save I just clicked, has the Ark side caught up".
type AgentSyncStatus string

const (
	// AgentSyncPending is queued, not yet claimed by any instance.
	AgentSyncPending AgentSyncStatus = "pending"
	// AgentSyncRunning has been claimed by some instance and is syncing.
	AgentSyncRunning AgentSyncStatus = "running"
	// AgentSyncSucceeded means the Ark side has caught up.
	AgentSyncSucceeded AgentSyncStatus = "succeeded"
	// AgentSyncFailed failed to sync repeatedly and was given up. The reason stays in FailureReason for the user.
	AgentSyncFailed AgentSyncStatus = "failed"
)

// Terminal reports whether this status is final.
//
// Frontend polling relies on it to decide "schedule another round or not": **there may only be one copy of this check**:
// if each place wrote `s == "succeeded" || s == "failed"` itself, adding a final state in the future
// (like canceled) would miss some place, showing as the frontend polling forever in that state.
func (s AgentSyncStatus) Terminal() bool {
	return s == AgentSyncSucceeded || s == AgentSyncFailed
}

// AgentSyncStage marks which step the sync failed at.
//
// A separate field instead of folding it into FailureReason: the frontend gives **different guidance** by it (credential problems send the user
// to workspace settings to change config; Ark service failures ask them to retry), and working out "which step" from a sentence
// could only be done by string matching, which always drifts, and after drifting the guidance would point to the wrong place.
type AgentSyncStage string

const (
	// AgentSyncStageCredential: not even the Ark key could be resolved (not configured, or the referenced global config is gone).
	// Users can fix this level themselves; the guidance points to workspace settings.
	AgentSyncStageCredential AgentSyncStage = "credential"
	// AgentSyncStageEnvironment: creating/reusing the Ark Environment failed.
	AgentSyncStageEnvironment AgentSyncStage = "environment"
	// AgentSyncStageAgent: creating the Ark Agent failed.
	AgentSyncStageAgent AgentSyncStage = "agent"
	// AgentSyncStageSkills: skills were not attached.
	AgentSyncStageSkills AgentSyncStage = "skills"
	// AgentSyncStageSpec: the prompt / model did not catch up.
	AgentSyncStageSpec AgentSyncStage = "spec"
)

// AgentSyncTask is the record of one "sync the agent to Ark" task.
//
// # Why it must be stored in the database, instead of a background goroutine plus an in-process map
//
// Production amad runs as multiple instances. Keeping it in process would break three things at once:
//
//  1. **Frontend polling cannot find it**. Requests land randomly on any instance through the load balancer, and the status check is likely to land on
//     another process, where the map is empty: it shows as "the sync task vanished".
//  2. **When an instance is rebuilt it stays at running forever**. Deploys / scale-downs / failed health checks all rebuild instances,
//     and that in-process goroutine is just gone, **without telling anyone**.
//  3. **Clicking twice runs it twice**. Without the database deciding, two instances each start a goroutine syncing the same
//     agent, and both change the same Ark Agent.
//
// Stored in the database, all three are solved by the same table: anyone can query the status, an expired lease gets taken over by someone else,
// and a partial unique index lets the database decide "the same agent can only have one unfinished task".
type AgentSyncTask struct {
	ID          string          `json:"id"`
	WorkspaceID string          `json:"workspaceId"`
	AgentID     string          `json:"agentId"`
	Status      AgentSyncStatus `json:"status"`
	// FailureReason is the failure reason, **a sentence shown directly to the user** (same as core's error text convention).
	FailureReason string `json:"failureReason,omitempty"`
	// FailureStage is which step failed; the frontend gives different guidance by it.
	FailureStage AgentSyncStage `json:"failureStage,omitempty"`
	// Attempts is how many times it has run.
	//
	// **No omitempty**: 0 is a valid value (just queued, not run yet), and with it the key would disappear and the frontend would get
	// undefined, and `undefined > 1` and `0 > 1` are both false in JS but mean different things.
	Attempts int `json:"attempts"`
	// CreatedAt / UpdatedAt are **fixed-length ISO strings that go into the HTTP contract**.
	//
	// They and the two timestamptz columns in the database are two copies stored by the same write: those are for database sorting and timeout decisions,
	// these are for frontend display. For the rule see the repo's convention on timestamp units and formats.
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

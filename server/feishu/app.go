// Domain model of Feishu apps and registration sessions.
//
// # Why AppSecret is not in App
//
// App is a struct that gets serialized into API responses, so it **has no AppSecret field at all**: not
// by a json:"-" tag, but by not having the field. The secret is stored separately in secrets, readable only inside this package.
// That way "the secret leaks into a response" is not a mistake avoided by discipline, but something that does not compile.

package feishu

import (
	"strings"
	"time"
)

// AppStatus is the coarse status of an app, matching the three values on the frontend.
//
// Fine-grained diagnosis goes in Readiness. This split follows ArkCredential: the coarse status is a badge
// for the list page, the details are diagnosis for the detail page; the two change at different rates, and mixing them would tangle them.
type AppStatus string

const (
	// StatusUnverified means registered but not checked yet, or the check shows something is still missing.
	StatusUnverified AppStatus = "unverified"
	// StatusConnected means the check passed and the path is usable.
	StatusConnected AppStatus = "connected"
	// StatusError means the check failed, e.g. the credentials are no longer valid.
	StatusError AppStatus = "error"
)

// Four readiness states. Installed does not mean usable, and what the user must do differs in each, so they are separate.
const (
	// ReadinessSetupRequired means configuration is still missing; the user must add it on the open platform.
	ReadinessSetupRequired = "setup_required"
	// ReadinessWaitingForAdmin means it was requested and is waiting for the tenant admin to approve. The user just needs to nudge the admin, not submit again.
	ReadinessWaitingForAdmin = "waiting_for_admin"
	// ReadinessReady means everything is ready.
	ReadinessReady = "ready"
	// ReadinessRuntimeUnavailable means the configuration is complete but it cannot connect, usually because the credentials are invalid.
	ReadinessRuntimeUnavailable = "runtime_unavailable"
)

// Readiness is a diagnosable readiness report.
//
// Deliberately not a boolean: "not usable" has seven or eight causes, and each needs the user to do something different.
// Returning only false would leave the user guessing.
type Readiness struct {
	// Status is one of the four states, used as the UI title.
	Status string `json:"status"`
	// CheckedAt is the time of this check.
	CheckedAt time.Time `json:"checkedAt"`

	// MissingScopes are permissions never requested; the user must add them on the open platform.
	MissingScopes []string `json:"missingScopes"`
	// UnapprovedScopes are permissions requested and waiting for admin approval.
	//
	// Keeping them apart from MissingScopes is the core of this report: one needs the user to change config, the other can only wait for the admin.
	// Merging them into one field would make users submit already-submitted permissions again and again.
	UnapprovedScopes []string `json:"unapprovedScopes"`
	// MissingEvents are events not subscribed.
	MissingEvents []string `json:"missingEvents"`
	// MissingCallbacks are callbacks not subscribed.
	MissingCallbacks []string `json:"missingCallbacks"`

	// ConfigurationInspectable marks how much this check could really see. When false, the booleans below are not trustworthy.
	ConfigurationInspectable bool `json:"configurationInspectable"`
	// BotEnabled is whether the bot capability is enabled.
	BotEnabled bool `json:"botEnabled"`
	// Published is whether there is a published production version.
	Published bool `json:"published"`
	// GroupMessageScopeGranted is whether the permission to receive chat messages without an @ has been granted.
	GroupMessageScopeGranted bool `json:"groupMessageScopeGranted"`
	// GroupMessageScope is the name of that permission (im:message.group_msg).
	//
	// **Sent by the server instead of hard-coded on the frontend**: the UI shows this string as-is for the user to find on the open
	// platform; with a copy on each side, the frontend would miss a change to the constant, and the miss would show up as "following the hint I cannot find
	// this permission"; users would not suspect we wrote it wrong.
	GroupMessageScope string `json:"groupMessageScope"`
	// GroupMessageAuthURL is the open platform URL for requesting the permission above, **empty when already granted**.
	//
	// Empty/non-empty is itself the "show a hint or not" switch; the frontend need not decide by itself.
	GroupMessageAuthURL string `json:"groupMessageAuthURL,omitempty"`
	// BotMessageScopeGranted is whether the permission "also push chat messages sent by **bots** to me" has been granted.
	//
	// # Why it must be separate from GroupMessageScopeGranted
	//
	// Feishu splits "receive chat messages" into two levels, and **with only the level above, not a single bot message is pushed**.
	// If the two were merged into one boolean, the failure "the alert chat receives no alerts" would be completely invisible in the UI:
	// that is how 2026-09-02 took two rounds of debugging: the long connection was fine, readiness all green, and the inbound table empty.
	//
	// ⚠️ It **does not go into DeriveStatus**, for the same reason as GroupMessageScopeGranted:
	// it is a **conditionally required** permission (only needed when no-@ mode is chosen), and DeriveStatus does not get
	// TriggerMode, so counting it would misreport a large number of working apps as missing permissions.
	BotMessageScopeGranted bool `json:"botMessageScopeGranted"`
	// BotMessageScope is the name of that permission (im:message.group_msg.include_bot:read).
	// Also sent by the server; see GroupMessageScope for why.
	BotMessageScope string `json:"botMessageScope"`
	// BotMessageAuthURL is the open platform URL for requesting it, **empty when already granted**.
	BotMessageAuthURL string `json:"botMessageAuthURL,omitempty"`
	// AdminAuthorizationRequested is whether an admin approval request has already been made.
	AdminAuthorizationRequested bool `json:"adminAuthorizationRequested"`
	// RuntimeConnected is whether it can really connect (one liveness probe was made with the credentials).
	//
	// ⚠️⚠️ **It has nothing to do with "the long connection is alive"; do not use it to debug missing messages.**
	// What sets it is `client.BotInfo()`, a **REST liveness probe** that only proves "AppID/AppSecret
	// can still exchange a token and the bot info can be read". If the websocket broke, the bot was kicked out of the chat,
	// or Feishu does not push messages at all, this field **is still true**.
	//
	// Likewise MissingEvents **is always an empty array** (the long connection does not need events ticked in the console).
	// So "readiness all green" rules out no inbound problem at all; the 2026-09-01 and 09-02
	// investigations both took a detour on this. There are only two trustworthy inbound tests:
	// the `收到飞书消息但没有触发 agent` line in the log (dropped.go, Info level),
	// and whether that message_id exists in feishu_inbound_messages.
	RuntimeConnected bool `json:"runtimeConnected"`

	// LastConnectedAt is the last time it connected; empty if never connected.
	LastConnectedAt *time.Time `json:"lastConnectedAt,omitempty"`
	// LastRuntimeError is the reason of the last connection failure, a Chinese sentence shown directly to the user.
	LastRuntimeError string `json:"lastRuntimeError,omitempty"`
	// LastCheckError is the reason of the last check failure.
	LastCheckError string `json:"lastCheckError,omitempty"`

	// The three below are display info read along the way during the check, used to fill back the matching fields of App.
	// Unexported, so they do not go into JSON: they belong to App, not to this diagnosis report.
	inspectedName      string
	inspectedAvatar    string
	inspectedBotOpenID string
}

// TriggerMode decides when this app's bot speaks in chats.
//
// # Why an enum instead of a boolean
//
// "No @ in topic groups" looks like a switch, but it is not: there will be a third mode later (most likely
// "every message in the chat triggers"). A boolean would need another field, and two booleans have four combinations, one of which,
// "not no-@ but every chat message triggers", is meaningless and would have to be blocked by convention. An enum naturally has only the listed values,
// adding a level is adding a constant, and readers can see at a glance how many modes there are.
type TriggerMode string

const (
	// TriggerMentionOnly triggers only when the bot is @-mentioned. **This is the default**; see App.TriggerMode for why.
	//
	// Direct chats are not affected: the inbound layer treats direct chats as always @-mentioned (see shared/infra/lark/longconn.go),
	// since a user talking to the bot directly should not be required to type an @.
	TriggerMentionOnly TriggerMode = "mention_only"
	// TriggerTopicAndMention is "automatic in topic groups + @ triggers in other chats".
	//
	// Only chats with chat_type == topic_group are no-@; ordinary chats still need an @. Without the @,
	// the natural self-trigger protection of mentionsBot is gone, so this path must explicitly block
	// messages sent by bots; see trigger.go.
	TriggerTopicAndMention TriggerMode = "topic_and_mention"
)

// NormalizeTriggerMode maps existing data and external input to a known value.
//
// **The empty string must map to TriggerMentionOnly**; that is the main reason this function exists: old rows in the store
// (the feishu_apps.doc jsonb has no triggerMode key at all) deserialize to an empty string.
// Without this, after an upgrade every chat would suddenly stop triggering, or worse, fall to the no-@ side and start auto-replying.
//
// Unknown values also fall back to the default mode: when the config has a dirty value, better to fall back to the most conservative behavior
// than to let a misspelled string turn the bot into a chatterbox.
func NormalizeTriggerMode(mode TriggerMode) TriggerMode {
	switch TriggerMode(strings.TrimSpace(string(mode))) {
	case TriggerTopicAndMention:
		return TriggerTopicAndMention
	default:
		return TriggerMentionOnly
	}
}

// App is a connected Feishu app. **It has no AppSecret**; see the file header comment.
type App struct {
	// ID is the AMA-side identifier.
	ID string `json:"id"`
	// WorkspaceID is the owning workspace.
	WorkspaceID string `json:"workspaceId"`
	// Name is the display name, refreshed from the Feishu side during checks.
	Name string `json:"name"`
	// AppID is the Feishu app ID, like cli_xxx, globally unique.
	AppID string `json:"appId"`
	// AvatarURL is the app avatar, read from the Feishu side during checks.
	AvatarURL string `json:"avatarUrl,omitempty"`
	// BotOpenID is the bot's open_id.
	BotOpenID string `json:"botOpenId,omitempty"`
	// Region is the account domain the app belongs to (feishu / lark).
	Region string `json:"region"`
	// TenantBrand is the tenant brand Feishu reported at registration, the basis for dual-domain correction.
	TenantBrand string `json:"tenantBrand,omitempty"`
	// Status is the coarse status.
	Status AppStatus `json:"status"`
	// TriggerMode decides when the bot speaks in chats; values are listed in TriggerMode.
	//
	// **The zero value (empty string) is the default mode mention_only**, on purpose: old rows in the store
	// (feishu_apps stores the whole struct in a jsonb doc, and old rows lack this key) decode to an empty string.
	// Always pass it through NormalizeTriggerMode before use, do not compare directly: **the default must fall on the safe side**,
	// and every chat suddenly auto-replying after an upgrade is unacceptable.
	TriggerMode TriggerMode `json:"triggerMode"`
	// BoundAgentIDs are the agents bound to this app, a reverse reference.
	BoundAgentIDs []string `json:"boundAgentIds"`
	// Readiness is the latest check result; empty if never checked.
	Readiness *Readiness `json:"readiness,omitempty"`
	// AdminAuthorizationRequestedAt is when the admin approval request was submitted; empty if never submitted.
	//
	// A time is stored instead of a boolean: when we later need to judge "how long since the request, should we prompt the user to nudge again", the struct need not change.
	AdminAuthorizationRequestedAt *time.Time `json:"adminAuthorizationRequestedAt,omitempty"`
	// CreatedByID is the person who connected it, used when deciding ownership idempotently.
	CreatedByID string `json:"createdById"`
	// CreatedAt is the connection time.
	CreatedAt time.Time `json:"createdAt"`
	// UpdatedAt is the time of the latest change.
	UpdatedAt time.Time `json:"updatedAt"`
}

// cloneStrings copies a string slice and guarantees it is non-nil.
//
// "Non-nil" is the whole reason this function exists; do not switch to append([]string(nil), in...) for convenience:
// that form **returns nil** when in is empty, nil encodes as JSON null, and the frontend contract requires []
// —— the frontend uses .length / .map() directly, and null blanks the whole page (ReadinessCard crashed once reading
// r.missingScopes.length). The function of the same name in core is the same; see core/store.go.
func cloneStrings(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	return out
}

// Clone returns a deep copy.
//
// The in-memory Store must return copies: handing out internal pointers means a change by the caller is a change to the store,
// and once switched to the PG implementation such a change would not take effect. Bugs where two implementations behave differently are the hardest to debug.
//
// Its other job is **guarding the "slices are non-nil" contract**: in the doc the PG implementation decodes from jsonb,
// an empty slice may be null or missing entirely, and is nil after decoding. Both implementations pass return values through Clone,
// so this invariant only needs to be guaranteed once, here.
//
// **Normalizing TriggerMode is also here**, for the same reason: old rows' doc lacks the triggerMode key,
// which decodes to an empty string, and an empty string should not appear in HTTP responses (the frontend would need another branch to guess its mode).
// The read paths of both Stores go through Clone, so normalizing once here means only known values are ever exposed.
// The real trigger decision does not depend on this step; it normalizes again itself, see trigger.go.
func (a *App) Clone() *App {
	if a == nil {
		return nil
	}
	out := *a
	out.TriggerMode = NormalizeTriggerMode(a.TriggerMode)
	out.BoundAgentIDs = cloneStrings(a.BoundAgentIDs)
	if a.AdminAuthorizationRequestedAt != nil {
		ts := *a.AdminAuthorizationRequestedAt
		out.AdminAuthorizationRequestedAt = &ts
	}
	if a.Readiness != nil {
		r := *a.Readiness
		r.MissingScopes = cloneStrings(a.Readiness.MissingScopes)
		r.UnapprovedScopes = cloneStrings(a.Readiness.UnapprovedScopes)
		r.MissingEvents = cloneStrings(a.Readiness.MissingEvents)
		r.MissingCallbacks = cloneStrings(a.Readiness.MissingCallbacks)
		if a.Readiness.LastConnectedAt != nil {
			ts := *a.Readiness.LastConnectedAt
			r.LastConnectedAt = &ts
		}
		out.Readiness = &r
	}
	return &out
}

// Session is the intermediate state of a registration attempt.
//
// It only exists during the ten minutes of registration and is deleted afterwards. AppSecret stayed here briefly, so this
// struct **does not appear in any API response**; handler only derives the status and polling interval from it.
type Session struct {
	// WorkspaceID is the workspace at the time it was started. It is compared on polling, to prevent using a session of workspace A in workspace B.
	WorkspaceID string
	// ActorID is the initiator. Polling by anyone other than the initiator is refused; see the auth order in onboarding.go.
	ActorID string
	// DeviceCode is Feishu's polling credential.
	DeviceCode string
	// Region is the account domain currently used. It is rewritten in place when the domain was guessed wrong; see Service.Poll.
	Region string
	// PollIntervalSeconds is the current polling interval; it grows on slow_down.
	PollIntervalSeconds int
	// ExpiresAt is when the device code expires.
	ExpiresAt time.Time
}

// RegistrationStatus is the external result of one poll.
type RegistrationStatus string

const (
	// RegistrationPending is still waiting for the user to authorize.
	RegistrationPending RegistrationStatus = "pending"
	// RegistrationCompleted means registration is done, and the App field is set.
	RegistrationCompleted RegistrationStatus = "completed"
	// RegistrationExpired means the session or device code has expired.
	RegistrationExpired RegistrationStatus = "expired"
	// RegistrationDenied means the user refused authorization.
	RegistrationDenied RegistrationStatus = "denied"
	// RegistrationInvalid means the device code is invalid.
	RegistrationInvalid RegistrationStatus = "invalid"
)

// Challenge is the result of starting a registration, handed to the frontend to guide the user.
type Challenge struct {
	// SessionID is a high-entropy session ID the frontend uses for polling.
	SessionID string `json:"sessionId"`
	// VerificationURL is the Feishu authorization URL for the user to open.
	VerificationURL string `json:"verificationUrl"`
	// ExpiresAt is the expiry time.
	ExpiresAt time.Time `json:"expiresAt"`
	// PollIntervalSeconds is the suggested polling interval.
	PollIntervalSeconds int `json:"pollIntervalSeconds"`
}

// PollResult is the result of one poll.
type PollResult struct {
	// Status is the result of this poll; look at it first.
	Status RegistrationStatus `json:"status"`
	// PollIntervalSeconds is how long to wait before the next poll; it grows on slow_down.
	PollIntervalSeconds int `json:"pollIntervalSeconds"`
	// App only has a value when Status is completed.
	App *App `json:"app,omitempty"`
	// Reason is the failure reason, a Chinese sentence that can be shown directly.
	Reason string `json:"reason,omitempty"`
}

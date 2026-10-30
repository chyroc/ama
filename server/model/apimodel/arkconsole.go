// Ark console deep links, and the authorization status for "fetch key".

package apimodel

// ArkConsoleRef is where an AMA entity is located in the Ark console.
//
// Three kinds of entities share this one struct: the field that has a value is its id, the rest are empty. They are merged because
// their **absence means exactly the same thing** ("not created on the Ark side yet"), and the frontend handles them the same (no entry rendered).
type ArkConsoleRef struct {
	// ArkAgentID is the Agent id on the Ark side (like agent-20260831113008-frhdh).
	//
	// **Empty is normal, not an error**: Ark resources are **created lazily**; `ensureBinding` only creates them on the first
	// chat. An agent that was just created and never chatted is always empty; the UI must say so clearly,
	// so users do not think it is broken.
	ArkAgentID string `json:"arkAgentId,omitempty"`

	// ArkSkillID is the skill id on the Ark side (like skill-20260901120830-px59g).
	//
	// Also created lazily: a skill is only uploaded after **some agent bound to it has had a chat**
	// (arkSkillsFor → ensureArkSkill). A skill that is created but never used never gets this id.
	ArkSkillID string `json:"arkSkillId,omitempty"`

	// ArkSessionID is the session id on the Ark side (like sesn-20260101000000-xxxxx).
	//
	// **It is Run.ID itself**, not another id (see where the Run is created in chat.go:
	// "the ID is the Ark session id"). So this one always has a value: if the session exists, the
	// Ark-side session has already been created. It is still put in this struct instead of letting the frontend use
	// run.id directly, so all three entries use the same contract: **the frontend does not need to know which ids happen to be equal**;
	// if that "happens to be the same" knowledge spreads into the frontend, nothing will report an error on the day they stop being equal.
	ArkSessionID string `json:"arkSessionId,omitempty"`

	// Region is the region needed to build the console URL, taken from **the Ark credential actually in effect**
	// (three-level lookup: agent → workspace → account), not a constant.
	//
	// Empty means no credential could be resolved (no key configured). Then the frontend renders no entry: without a region
	// the correct URL cannot be built, and a link with a default region would take the user to a
	// "resource not found" page, which is worse than no entry.
	Region ArkRegion `json:"region,omitempty"`
}

// ArkFetchStatus is the availability of the auto-fetch feature.
type ArkFetchStatus struct {
	// Available says whether this feature is **wired up** on the server. When false, the frontend should not show the entry.
	Available bool `json:"available"`
	// Authorized says whether the current user has authorized.
	Authorized bool `json:"authorized"`
	// AuthorizeURL only has a value when Available and not authorized; the frontend uses it for an "authorize" link.
	//
	// **Has omitempty**: once authorized it should be absent, not an empty string.
	// ⚠️ This link is valid for only 600 seconds, so **do not cache it**; fetch it fresh every time.
	AuthorizeURL string `json:"authorizeUrl,omitempty"`
	// Reason explains why when unavailable, shown directly to the user.
	Reason string `json:"reason,omitempty"`
}

// Input shapes for Ark credentials, and key references.
//
// Moved from core so the orchestration layer's signatures do not need to import core. Fields and json tags are unchanged.

package apimodel

// ArkCredentialDraft is the input for saving a credential.
type ArkCredentialDraft struct {
	APIKey string
	Region ArkRegion

	// Fetch being non-empty means "fetch one from the Volcengine account"; APIKey should then be empty.
	//
	// # Why the plaintext does not come from the frontend
	//
	// Letting the frontend get the plaintext and PUT it back would be simpler (just reuse this field), but then a long-lived
	// Ark key passes through the browser: it shows up in the devtools network panel, may be read by browser extensions, and stays in
	// frontend memory and possibly logs. The plaintext should only flow "server memory → database".
	//
	// So the frontend only sends back {accountId, keyId}, and the server **fetches the plaintext itself** (see resolveDraftKey).
	Fetch *ArkKeyRef

	// ConfirmNotActivated being true means: the user has seen the warning "the Ark account of this key has not
	// activated Managed Agents yet" and **still wants to save**.
	//
	// Saving takes two steps: the first request is sent without it; if the backend probe finds it not activated, it returns
	// 422 ark_managed_agent_not_open (nothing stored); the frontend offers two choices, "Activate / Save anyway",
	// and after the user clicks "Save anyway" it resends with this flag, and only then does the backend store it.
	//
	// ⚠️ It **only** applies to the "not activated" case: a bad key (401/403) has no force-save path with a flag;
	// such a key, once saved, fails on the first chat.
	ConfirmNotActivated bool
}

// ArkKeyRef points to one Ark API Key in a Volcengine account.
type ArkKeyRef struct {
	// AccountID is the Volcengine account id (one person may have several).
	AccountID string
	// KeyID is the numeric id of that key on the Volcengine side.
	KeyID string
}

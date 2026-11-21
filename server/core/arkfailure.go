package core

// ArkFailure translates an Ark error into the user-facing form used by chat and
// agent sync. The precondition/conflict sentinels remain distinguishable, while
// the original Ark error stays in the chain for request ids and structured checks.
func ArkFailure(what string, err error) error { return arkFailure(what, err) }

// ArkUserError carries both a business sentinel and the original Ark cause.
type ArkUserError = arkUserError

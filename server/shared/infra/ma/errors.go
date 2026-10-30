// This file does one thing: make it easy for callers to tell apart the errors Ark returns.
//
// All non-streaming SDK methods return *model.APIError when HTTP >= 300. It carries the HTTP
// status code, Ark's error code and request_id. Logging err.Error() directly loses this
// structured information, and request_id matters most when debugging production problems.

package ma

import (
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/volcengine/ark-runtime-go/arkruntime/model"
)

// APIError is the structured error returned by Ark, the same as the SDK's *model.APIError.
// It is kept as an alias instead of a new type so that errors.As also works with code that uses
// the SDK directly.
type APIError = model.APIError

// AsAPIError tries to turn err back into Ark's structured error.
//
//	if apiErr, ok := ma.AsAPIError(err); ok {
//	    log.Printf("Ark error code=%s request_id=%s", apiErr.Code, apiErr.RequestId)
//	}
func AsAPIError(err error) (*APIError, bool) {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr, true
	}
	return nil, false
}

// StatusCode returns the HTTP status code of the error; 0 if it is not an Ark structured error.
func StatusCode(err error) int {
	if apiErr, ok := AsAPIError(err); ok {
		return apiErr.HTTPStatusCode
	}
	return 0
}

// RequestID returns the request_id of this request. It is the most useful thing to give the Ark
// side when reporting a problem.
func RequestID(err error) string {
	if apiErr, ok := AsAPIError(err); ok {
		return apiErr.RequestId
	}
	return ""
}

// IsNotFound reports whether the error is a 404, often used for idempotent handling of "the
// resource is already deleted".
func IsNotFound(err error) bool {
	return StatusCode(err) == http.StatusNotFound
}

// IsConflict reports whether the error is a 409.
//
// Two cases hit it: **creating** a resource with a taken name (Environment names are unique in a
// project), and an optimistic-lock version mismatch when **updating** a resource (UpdateAgent's
// version must equal the server's current value).
//
// With many instances the latter is normal, not a failure: two instances update the same Agent
// at once, the slower one gets 409, and what it wanted to write is the same as the faster one
// (computed from the same source data). So callers should usually **let it pass**; see
// LiveArkRunner.UpdateAgentSkills.
func IsConflict(err error) bool {
	return StatusCode(err) == http.StatusConflict
}

// IsRateLimited reports whether the error is a 429; use it with backoff and retry.
func IsRateLimited(err error) bool {
	return StatusCode(err) == http.StatusTooManyRequests
}

// IsAuthError reports whether authentication failed (401 / 403), which usually means the API Key
// is invalid or the service is not activated.
func IsAuthError(err error) bool {
	switch StatusCode(err) {
	case http.StatusUnauthorized, http.StatusForbidden:
		return true
	}
	return false
}

// notOpenAccountRe matches the account id in the ManagedAgentNotOpen message,
// such as "Your account 2100000001 has not activated the managed agent.".
var notOpenAccountRe = regexp.MustCompile(`account (\d+)`)

// IsManagedAgentNotOpen reports whether "this Ark account has not activated the Managed Agents
// service".
//
// Ark replies **HTTP 404 + code=ManagedAgentNotOpen**, with the account id in the message:
//
//	Your account 2100000001 has not activated the managed agent.
//	Please activate the service in the Ark Console.
//
// # This is an account configuration problem, not a service failure
//
// It is the same kind of thing as 401/403 "invalid key": someone has to activate it in the Ark
// console, and no number of retries will fix it. Once in production the global credential was
// replaced with a key from an account that had not activated the service. Every workspace that
// inherited the global credential hit this 404 when creating an environment, and since nothing
// recognized it, it fell into the handler's default → 500 "internal server error". The page
// only said the service was down; the only fix, "activate it in the console", was only in the
// server logs.
//
// ⚠️ **Do not judge by 404 alone**: the same 404 also means "the resource was deleted"
// (IsNotFound, which chat.go relies on to self-heal stale bindings), and the right reactions to
// the two are opposite. The code is the contract.
func IsManagedAgentNotOpen(err error) bool {
	return ErrorCode(err) == "ManagedAgentNotOpen"
}

// NotOpenAccountID extracts the Ark account id from the ManagedAgentNotOpen message; it returns
// an empty string if this is not that error or no id is found. When reporting a problem the user
// needs to know **which account** is not activated.
func NotOpenAccountID(err error) string {
	apiErr, ok := AsAPIError(err)
	if !ok {
		return ""
	}
	if m := notOpenAccountRe.FindStringSubmatch(apiErr.Message); len(m) == 2 {
		return m[1]
	}
	return ""
}

// ErrorCode returns Ark's own error code (the `code` field, e.g. "RequiresActionRejected");
// an empty string if it is not an Ark structured error.
//
// **It is not the same thing as the HTTP status code**: Ark files failures with completely
// different meanings under 400 and tells them apart by this field. Looking only at the status
// code mixes up "waiting for a tool result" with "a parameter is wrong".
func ErrorCode(err error) string {
	if apiErr, ok := AsAPIError(err); ok {
		return apiErr.Code
	}
	return ""
}

// IsEnvironmentNotFound reports whether "the Ark Environment referenced by the session no longer
// exists (or this key has no access to it)".
//
// Ark replies **HTTP 404 + code=InvalidEnvironment.NotFound**. The original text from a real
// call (caught by a scheduled-task check in production):
//
//	The environment env-xxxxxxxxxxxxxx-xxxxx does not exist or you do not
//	have access to it.
//
// # Why it must be separated from the "Agent is gone" 404
//
// CreateSession only takes two resources, Agent and Environment, and either one missing returns
// 404. But the self-heal scope differs (see createConversation in core/chat.go):
//
//   - Agent gone: only delete this agent's own binding; the environment is reused per
//     workspace and is still alive;
//   - Environment gone: the bindings of **all sibling agents** in the same workspace store
//     this dead environment ID. Deleting only your own row means the rebuild picks the same
//     dead environment back up from a sibling row, the second CreateSession gets the same 404,
//     and this repeats every turn, leaking one Ark Agent each time.
//
// Same as IsManagedAgentNotOpen: one 404 has at least three meanings, so judging by status code
// alone is bound to be wrong; the code is the contract. This code is not listed in the docs; it
// was caught from real calls.
func IsEnvironmentNotFound(err error) bool {
	return ErrorCode(err) == "InvalidEnvironment.NotFound"
}

// IsRequiresActionRejected reports whether "the session is waiting for a tool's result and right
// now only accepts tool_result / tool_confirmation / interrupt".
//
// # This is a timing race, not something the user did wrong
//
// With self_hosted, tools run in our own containers, so there is a window between Ark sending
// tool_use and the worker returning `user.tool_result`. During the window Ark rejects
// `user.message` with HTTP 400 + code=RequiresActionRejected. Measured, this window is usually
// only tens of milliseconds (48ms was seen in production), but it comes back again and again
// when the agent calls tools in a row, and a fast user will hit it.
//
// **Do not judge by status code**: it shares 400 with "invalid event format", and the latter
// will never succeed no matter how many retries. Mixing them makes real parameter errors also
// show as "wait and resend", and the user waits for nothing.
func IsRequiresActionRejected(err error) bool {
	return ErrorCode(err) == "RequiresActionRejected"
}

// IsRuntimeBusy reports whether "this session is busy with another exclusive command; retry
// later".
//
// # A different code from RequiresActionRejected, but the same kind of thing
//
// Ark replies **HTTP 409 + code=RuntimeBusy**, and the text literally says `retry later`:
//
//	Session is currently busy with another exclusive command; retry later.
//
// Caught from real calls (during a rapid-send stress test): when the agent calls tools in a row,
// besides the tens-of-milliseconds `RequiresActionRejected` window, it also hits this one: on
// the Ark side an exclusive command is running on the same session. The right reaction to both
// is exactly the same: **wait a moment and send again**.
//
// The cost of not recognizing it is concrete: it falls into the handler's default → **500
// "internal server error"**, which is neither a server fault nor something that should make the
// user think the service is down. This is exactly the problem RequiresActionRejected was fixed
// for, except that back then only one of the two codes had been recognized.
//
// **Also do not judge by status code**: 409 is used for other meanings on the Ark side too, and
// our own ErrConflict is also 409. Only the code is the contract.
func IsRuntimeBusy(err error) bool {
	return ErrorCode(err) == "RuntimeBusy"
}

// IsTransientSendRejection reports whether this send failure is the "try again at another
// moment" kind.
//
// There are currently two codes (see the two functions above), **both caught from real calls,
// not taken from the docs**. Callers (core's retry and 409 translation) should always ask this
// one function, not each code separately. Missing one shows up as the user occasionally seeing a
// 500 that points to no real fault.
func IsTransientSendRejection(err error) bool {
	return IsRequiresActionRejected(err) || IsRuntimeBusy(err)
}

// modelEndpointRe captures the model id in the "this account has not activated the model"
// message.
var modelEndpointRe = regexp.MustCompile(`endpoint "([^"]+)" is invalid`)

// ModelEndpointNotOpen reports whether "the Ark account that owns this API Key has not activated
// inference for this model", and if so returns the model id.
//
// Ark replies **HTTP 400 + code=InvalidParameter**. The message caught in production:
//
//	model.id: endpoint "doubao-seed-evolving" is invalid:
//	get endpoint meta doubao-seed-evolving: service not open Request id: 0217...
//
// # This is an account configuration problem, not a service failure
//
// The model itself is valid (it is in our MODEL_SPECS and in the Ark docs list), but Ark models
// are **activated one by one per account**: with the same model id, account A can create an
// Agent, while account B, which has not activated it in the console, is rejected with 400. This
// is most likely right after the global Ark credential is replaced (with another account's
// key): models the old account had activated are not necessarily activated on the new one.
//
// It happened in production: the global key was switched to a new account's key, an agent used
// doubao-seed-evolving which the new account had not activated, the key change triggered a
// binding rebuild → CreateAgent got this 400, nothing recognized it, and it fell into the
// handler default → 500 "internal server error". The fix (activate the model in the Ark console,
// or switch models) was only in the server logs; the user had no way to act.
//
// ⚠️ **Do not match on InvalidParameter alone**: the same code also means "a request body field
// is wrong", which is our own bug and should be a 500. Only when `service not open` and the
// endpoint name both appear in the message, all three together, does it count as "account not
// activated".
func ModelEndpointNotOpen(err error) (string, bool) {
	apiErr, ok := AsAPIError(err)
	if !ok {
		return "", false
	}
	if apiErr.Code != "InvalidParameter" || apiErr.HTTPStatusCode != http.StatusBadRequest {
		return "", false
	}
	if !strings.Contains(apiErr.Message, "service not open") {
		return "", false
	}
	m := modelEndpointRe.FindStringSubmatch(apiErr.Message)
	if len(m) != 2 {
		return "", false
	}
	return m[1], true
}

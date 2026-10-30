// Package auth is the login and authentication module of AMA.
//
// In one sentence: **turn "the person who logged in" into "the Actor AMA knows", and keep that
// identity valid across instances and after a browser refresh.**
//
// # The unique user ID is the lower-cased email
//
// Workspace members (apimodel.WorkspaceMember) are already identified by email. Using the email
// as the Actor means logged-in users and workspace members match naturally, with no mapping
// table to maintain.
//
// Emails are **lower-cased before they are stored and compared** (normalizeEmail). The case the
// IdP returns is not guaranteed to be stable; without normalizing at the entry point, the same
// person becomes two Actors downstream.
//
// # Four authentication channels
//
//  1. Local account (human)  email + password login → get an opaque session token (local.go)
//  2. OIDC (human)           Authorization Code + PKCE redirect login → same as above (oidc.go)
//  3. Fixed API Key (machine) one key + one email set by environment variables → act as that person
//  4. User API Key (machine)  `amak_` prefix, created by the user; identity = creator (apikey.go)
//
// 1 and 2 are turned on by config (auth.local / auth.oidc) and can both be on. Channel 3 is
// **off by default** (it does not exist unless the environment variables are set); it is for
// e2e / CI / probes.
//
// # The session credential is a random token, not a JWT
//
// We issue 32 random bytes and **store only their sha256 in the database**. To check, the
// incoming token is hashed again and looked up. Reasons: it can really be revoked (deleting the
// row is a real logout), there is no signing key to distribute and rotate, and a database leak
// does not yield tokens. The cost is one extra primary-key lookup per request.
//
// # Sessions and in-progress login state must live outside the process
//
// amad runs as multiple instances in production: a browser login request may land on A and the
// next request on B. Keeping sessions, the OIDC state/nonce/PKCE, or failed-password counters
// in an in-process map breaks with multiple instances (random logouts, occasional "login has
// expired", rate limits multiplied by the number of instances), and a single local process
// never reproduces it. That is why Store has a PG implementation (repository/dao/authdao).
//
// MemStore is only for tests and AMA_STORE=mem; its meaning is "restart the process and you
// must log in again".
//
// # Usage
//
//	svc, err := auth.New(auth.Config{Store: store, Local: auth.LocalConfig{Enabled: true}})
//	res, err := svc.LoginWithPassword(ctx, email, password) // res.SessionToken goes into the cookie
//	url, err := svc.StartOIDCLogin(ctx, redirectURI, next)  // browser is sent there with a 303
//	res, err := svc.CompleteOIDCLogin(ctx, redirectURI, state, code)
//	identity, err := svc.Resolve(ctx, token)                // the middleware uses it to get the identity
package auth

import (
	"errors"
	"strings"

	"github.com/chyroc/ama/server/model/persistmodel"
)

// Errors of this module. The upper layer (handler) maps them to HTTP status codes and error codes.
var (
	// ErrInvalidArgument means the call arguments are invalid. → 400
	ErrInvalidArgument = errors.New("auth: 参数不合法")

	// ErrUnauthenticated means there is no valid identity: no credential, an unknown credential,
	// or an expired session. → 401
	//
	// **Keep it apart from ErrNotFound**: 401 means "I do not know who you are", and the
	// frontend sends the user to the login page; a 404 is turned by the frontend's "not found →
	// null" rule into a silent empty state, which would show the user a blank page instead of
	// the login page.
	ErrUnauthenticated = errors.New("auth: 未登录")

	// ErrNotFound is the storage layer's "no such row" (session, login flow, local account). → 404
	ErrNotFound = errors.New("auth: 未找到")
)

// Identity is an authenticated caller.
//
// **Email is the unique ID**; the other fields are only for display.
type Identity struct {
	// Email is the user's email and the AMA Actor ID. Already lower-cased.
	Email string `json:"email"`

	// Name is the display name (the IdP's name / the name entered at sign-up, else the email prefix).
	Name string `json:"name"`

	// AvatarURL may be empty.
	AvatarURL string `json:"avatarUrl,omitempty"`

	// EmployeeID is the employee number; may be empty.
	EmployeeID string `json:"employeeId,omitempty"`

	// ViaAPIKey means this identity came from the API Key channel, not from a login session.
	//
	// The frontend uses it to know "this is a machine identity"; it also helps tell them apart
	// in logs.
	ViaAPIKey bool `json:"viaApiKey,omitempty"`
}

// Valid reports whether this is a usable identity.
func (i Identity) Valid() bool { return strings.TrimSpace(i.Email) != "" }

// normalizeEmail normalizes the case and whitespace of an email.
//
// **Every entry point must go through it**: the case from the IdP and from user input is not
// guaranteed to be stable; without normalizing at the entry point, the same person becomes two
// Actors downstream. This kind of bug only shows up for one person on one login and is very
// hard to track down.
// **The real implementation is persistmodel.NormalizeEmail** (auth and repository/dao/authdao
// must both use the same algorithm); this is a forwarding function with the same name, so the
// 9 call sites inside auth do not have to be rewritten one by one.
func normalizeEmail(email string) string {
	return persistmodel.NormalizeEmail(email)
}

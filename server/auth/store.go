// Storage of sessions, login flows and local accounts.
//
// Five kinds of things with completely different lifetimes:
//
//	Session         a logged-in session. Long (7 days by default); it is the "logged-in state" itself.
//	OIDCState       an OIDC login in progress. Short (10 minutes); useless once it becomes a session.
//	GitHubFlow      a GitHub device flow login in progress. Short (the lifetime GitHub gives, about 15 minutes).
//	LocalAccount    a local account (email + bcrypt hash). Long-lived.
//	Login failures  anti-brute-force counter of password logins, fixed window (15 minutes by default).
//
// All of them **must live outside the process**: production runs multiple instances. An OIDC login
// starts on A and its callback lands on B; consecutive password attempts are spread by the load
// balancer over N instances, so an in-process counter gives N times the budget. A single local
// process never reproduces any of this. See "This is a distributed service" in CLAUDE.md.

package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Session is one logged-in session.
//
// **It does not hold the token itself**, only its hash: what the database stores is not enough to
// forge a session.
type Session struct {
	// TokenHash is the sha256 (hex) of the session token. Primary key.
	TokenHash string

	// Email is who this session belongs to. Already lower-cased.
	Email string

	// Name / AvatarURL / EmployeeID are display details obtained from SSO at login, stored so we do
	// not ask SSO on every request.
	Name       string
	AvatarURL  string
	EmployeeID string

	// CreatedAt / ExpiresAt are the lifetime of the session.
	CreatedAt time.Time
	ExpiresAt time.Time
}

// Expired reports whether this session has expired at time now.
func (s Session) Expired(now time.Time) bool { return !now.Before(s.ExpiresAt) }

// User is a person's profile, **kept across sessions for a long time**.
//
// # Difference from the Name/AvatarURL in Session
//
// Session also has these two fields, but its primary key is TokenHash and it has ExpiresAt: it
// **disappears at logout or expiry**, and there is one row per device. It stores "a snapshot at the
// moment of this login". User stores "who this person is" and stays after logout: the workspace
// member list needs to show names and avatars, and that person very likely has no session right
// now.
//
// # Email alone is allowed
//
// Inviting someone who has never logged in into a workspace is a normal operation, and at that
// point only the email is known. Name / AvatarURL can be empty strings, filled in at the first
// login. **Do not design it as "a row only exists after login"**.
type User struct {
	// Email is the user's email, which is the Actor ID. Already lower-cased.
	Email string

	// Name / AvatarURL may be empty: that is the state of someone invited who has not logged in yet.
	Name      string
	AvatarURL string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Identity turns the session into a usable identity.
func (s Session) Identity() Identity {
	return Identity{
		Email:      s.Email,
		Name:       s.Name,
		AvatarURL:  s.AvatarURL,
		EmployeeID: s.EmployeeID,
	}
}

// OIDCState is one OIDC login in progress (Authorization Code + PKCE).
//
// It only lives between the two requests "start login" and "user comes back from the IdP", for
// 10 minutes. **It must be stored in the database** (not an in-process map): the two requests very
// likely land on different instances; see the argument in repository/schema/auth_oidc_states.sql.
type OIDCState struct {
	// State is the OAuth state parameter: we generate it and the IdP sends it back unchanged.
	// **It is the CSRF protection**: if the state in the callback is not found, that callback was
	// not started by us.
	State string

	// Nonce is written into the authorization request; the nonce claim in the ID token must equal
	// it (prevents replay).
	Nonce string

	// CodeVerifier is the PKCE plain value. The authorization request only carries its S256 digest.
	CodeVerifier string

	// ReturnTo is where to send the user after a successful login; **only same-origin relative
	// paths are stored** (checked before writing).
	ReturnTo string

	// UsedAt is when it was consumed; zero means not used yet.
	//
	// "One-time" relies entirely on the single UPDATE in Store.ConsumeOIDCState, not on reading
	// this field: reading it and then deciding leaves a window for concurrent replay.
	UsedAt time.Time

	CreatedAt time.Time
	ExpiresAt time.Time
}

// Expired reports whether this login flow has expired at time now.
func (s OIDCState) Expired(now time.Time) bool { return !now.Before(s.ExpiresAt) }

// GitHubFlow is one GitHub device flow login in progress. See auth/github.go and
// repository/schema/auth_github_flows.sql.
//
// **It must be stored in the database**: the start and every later poll may land on different
// instances, and the poll interval must also be shared across instances.
type GitHubFlow struct {
	// FlowID is the opaque id given to the browser. Primary key.
	FlowID string

	// BindingHash is the sha256 of the plain binding cookie value. A poll must match both the flow
	// id and it.
	BindingHash string

	// DeviceCode is the device_code from GitHub. **Secret; never sent to the browser.**
	DeviceCode string

	UserCode        string
	VerificationURI string

	// IntervalSeconds is the current poll interval. +5 and persisted on slow_down.
	IntervalSeconds int

	// NextPollAt is the next time we are allowed to ask GitHub.
	NextPollAt time.Time

	CreatedAt time.Time
	ExpiresAt time.Time
}

// Expired reports whether this login has expired at time now.
func (f GitHubFlow) Expired(now time.Time) bool { return !now.Before(f.ExpiresAt) }

// LocalAccount is a local account (email + password).
//
// The display name and avatar are not here; they are in User (the profile shared by all login
// methods).
type LocalAccount struct {
	// Email is the lower-cased email, which is the Actor ID.
	Email string

	// PasswordHash is the bcrypt hash. **Neither the plain text nor a reversible ciphertext is stored.**
	PasswordHash string

	// Admin marks a platform admin: when self sign-up is closed, they create other accounts.
	Admin bool

	// Bootstrap marks "the first account registered on a fresh deployment". At most one row in the
	// whole database is true, enforced by a unique index (see auth_local_accounts.sql).
	Bootstrap bool

	// CreatedBy is who created it: empty for self sign-up, the admin's email when an admin created it.
	CreatedBy string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Store is the persistence contract of auth.
//
// There are two implementations: MemStore (tests and AMA_STORE=mem) and PGStore (default).
// Both run the same contract tests (store_test.go), so any difference in behavior fails right away.
type Store interface {
	// PutSession writes a session. Overwrites on the same TokenHash.
	PutSession(ctx context.Context, session Session) error

	// GetSession gets a session by token hash. Returns ErrNotFound if missing.
	//
	// **An expired session also counts as missing** (returns ErrNotFound): "expired" disappears in
	// the storage layer, so the upper layer does not need to remember to check it everywhere.
	GetSession(ctx context.Context, tokenHash string) (Session, error)

	// DeleteSession deletes a session (logout). Deleting a missing one is not an error (safe to repeat).
	DeleteSession(ctx context.Context, tokenHash string) error

	// DeleteSessionsByEmail deletes all sessions of a person (kick out). Returns how many were deleted.
	DeleteSessionsByEmail(ctx context.Context, email string) (int, error)

	// PutOIDCState writes an OIDC login in progress. Overwrites on the same state (and resets used_at).
	PutOIDCState(ctx context.Context, state OIDCState) error

	// ConsumeOIDCState consumes an OIDC login **and guarantees it can succeed only once**.
	//
	// On success it returns the row (for Nonce / CodeVerifier / ReturnTo). The following three
	// cases all return ErrNotFound: state does not exist, already consumed, expired.
	//
	// ⚠️ **Merging them into one error is deliberate**: the caller handles all three the same way
	// (make the user log in again), and telling them apart would tell an attacker "this state
	// exists, it just expired".
	//
	// ⚠️ **The implementation must put the "check" and the "mark as used" in the same statement**
	// (on PG, `update ... where used_at is null ... returning`). Writing it as "query, then update"
	// leaves a concurrency window, which means the same code could be redeemed twice.
	ConsumeOIDCState(ctx context.Context, state string) (OIDCState, error)

	// PutGitHubFlow writes a GitHub device flow login in progress (and clears expired rows on the way).
	PutGitHubFlow(ctx context.Context, flow GitHubFlow) error

	// ClaimGitHubPoll grabs this chance to "ask GitHub".
	//
	// It succeeds when flowID and bindingHash both match, it has not expired, and NextPollAt <= now:
	// it returns the row (for DeviceCode) and, **in the same statement**, pushes NextPollAt to
	// now + interval. So among concurrent polls of the same flow only one gets the device code; the
	// others get:
	//
	//   - row exists, just not time yet → ErrPollTooSoon (only IntervalSeconds of the returned flow
	//     is meaningful);
	//   - row missing / expired / binding does not match → ErrNotFound (the three are merged, for
	//     the same reason as ConsumeOIDCState).
	//
	// ⚠️ The check and the push must be the same UPDATE; query-then-update would let two concurrent
	// polls both ask GitHub.
	ClaimGitHubPoll(ctx context.Context, flowID, bindingHash string, now time.Time) (GitHubFlow, error)

	// SlowDownGitHubFlow handles GitHub's slow_down: the interval becomes
	// max(old interval + 5, minInterval), NextPollAt is pushed to now + new interval, and the new
	// interval is returned. Returns ErrNotFound if the row is missing. Done in a single statement.
	SlowDownGitHubFlow(ctx context.Context, flowID string, minInterval int, now time.Time) (int, error)

	// ConsumeGitHubFlow deletes this row and reports "was it me who deleted it". **It is the point
	// that decides "only one session is created"**: the request that got the token must get true
	// before creating a session; a concurrent other one gets false.
	// Expired rows do not count (returns false).
	ConsumeGitHubFlow(ctx context.Context, flowID string, now time.Time) (bool, error)

	// DeleteGitHubFlow deletes a row (on expiry / denial). A missing row is not an error.
	DeleteGitHubFlow(ctx context.Context, flowID string) error

	// CreateLocalAccount creates a local account. If the email already exists it **does not
	// overwrite** and returns (false, nil).
	//
	// "Already exists" is decided by the primary key (insert ... on conflict do nothing), not by
	// query-then-create: with two concurrent sign-ups of the same email, query-then-create would let
	// both pass the check, and the later write would overwrite the earlier password.
	CreateLocalAccount(ctx context.Context, acct LocalAccount) (bool, error)

	// CreateFirstLocalAccount creates an account only when "there are no users at all in the
	// database", and returns whether it was created.
	//
	// "No users at all" = both the users table and the local accounts table are empty. The created
	// row has Bootstrap=true, and a unique index ensures at most one such row in the database:
	// **when two people race to register the first account, only one succeeds**; the other gets
	// (false, nil). "Count first whether anyone exists" alone cannot stop this race.
	CreateFirstLocalAccount(ctx context.Context, acct LocalAccount) (bool, error)

	// GetLocalAccount gets a local account by email. Returns ErrNotFound if missing.
	GetLocalAccount(ctx context.Context, email string) (LocalAccount, error)

	// HasAnyUser reports whether the database already has any user (users or local accounts not
	// empty).
	//
	// Only used for hints like "should the login page show the sign-up entry"; the real decision is
	// in the CreateFirstLocalAccount statement, not here (there is a window between this read and
	// the write).
	HasAnyUser(ctx context.Context) (bool, error)

	// RecordLoginFailure records one unsuccessful attempt for key and returns the total **within
	// the current window** (including this one).
	//
	// The caller does "record first, then verify the password, clear on success" (see
	// LoginWithPassword in local.go), so the count always equals "attempts in the window that have
	// not led to success". Recording first means a batch of concurrent attempts is also counted
	// strictly: with verify-then-record, N requests arriving at once would all read the old value
	// and all pass.
	//
	// The window is a fixed window: when the start of the previous window is earlier than
	// now-window, the count starts again from 1.
	// ⚠️ "Read the old value + write the new value" must be one statement: a read-modify-write would
	// count two concurrent failures as one.
	RecordLoginFailure(ctx context.Context, key string, now time.Time, window time.Duration) (int, error)

	// ClearLoginFailures clears the failure count of key (called on successful login). A missing
	// one is not an error.
	ClearLoginFailures(ctx context.Context, key string) error

	// PutAPIKey writes a user-created API Key. Overwrites on the same ID.
	PutAPIKey(ctx context.Context, key APIKeyRecord) error

	// GetAPIKeyByHash gets a key by the sha256 of its plain text. Returns ErrNotFound if missing.
	//
	// **Every request with an API Key goes through it once**, so the hash must be an index /
	// primary key.
	GetAPIKeyByHash(ctx context.Context, keyHash string) (APIKeyRecord, error)

	// ListAPIKeysByEmail lists all keys of a person, **newest first by creation time**.
	//
	// The order is part of the contract instead of being left to the caller: if one implementation
	// were ordered and the other not, the list order would change with the implementation, and in
	// tests that only looks like "the order is different".
	ListAPIKeysByEmail(ctx context.Context, email string) ([]APIKeyRecord, error)

	// DeleteAPIKey deletes a key. The first return value says whether it was really deleted.
	//
	// **email is part of the condition**, not a parameter to check: expressing "you can only delete
	// your own" as the SQL where condition removes the TOCTOU window of "check ownership, then
	// delete".
	DeleteAPIKey(ctx context.Context, email, id string) (bool, error)

	// TouchAPIKey records one use.
	//
	// **A failure should not make the request fail**; callers only log it. It is a pure statistics
	// field, and rejecting a request that would otherwise pass because of it is not worth it.
	TouchAPIKey(ctx context.Context, keyHash string, at time.Time) error

	// PutUser writes or updates a person's profile (called at login).
	//
	// **Empty values do not overwrite existing non-empty values**: if SSO does not provide the name
	// or avatar on some login (it does not guarantee to), writing an empty string back would erase
	// the real name we already had; the symptom is that person suddenly showing only an email in
	// the member list, with no error at all.
	//
	// When it already exists, the original CreatedAt is kept ("the first time we saw this person"
	// should not be refreshed by later logins).
	PutUser(ctx context.Context, user User) error

	// GetUser gets a profile by email. Returns ErrNotFound if missing.
	GetUser(ctx context.Context, email string) (User, error)

	// ListUsers gets profiles for a batch of emails, **in one query**.
	//
	// # Why a batch version is required
	//
	// The callers are "render avatars for a list of people" (creators in the agent list, the member
	// list, issue authors...). Calling GetUser one by one is N+1: a page of 20 agents means 20
	// database calls. This path runs every time a list page opens, and N grows with the number of
	// agents in the workspace.
	//
	// # Rules
	//
	//   - The returned map is keyed by **lower-cased email** (same as GetUser; see normalizeEmail).
	//   - **Emails that are not found are left out of the map, and that is not an error**: "this
	//     person has never logged in" is normal (invited but never logged in, the machine identity
	//     of the fixed API Key, former colleagues in older data).
	//     Callers render an initial-letter fallback for them; do not treat it as a failure.
	//   - Empty input returns an empty map, not nil, which saves the caller a nil check.
	ListUsers(ctx context.Context, emails []string) (map[string]User, error)
}

// ---------------------------------------------------------------------------
// Generating and comparing tokens
// ---------------------------------------------------------------------------

// tokenBytes is the number of random bytes in a session token.
//
// 32 bytes = 256 bits, far beyond what brute force can enumerate. Do not lower it to make URLs
// look nicer: this string is as good as a password.
const tokenBytes = 32

// newToken generates a new opaque token.
//
// It uses base64 URL encoding (no padding): the token must go safely into cookies and query
// parameters (the SSE path), and the three characters + / = of standard base64 would need escaping
// in both places.
func newToken() (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成随机 token 失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// hashToken turns a token into the hash stored in the database.
//
// **One-way**: the database stores the hash, and an incoming token is hashed and compared. So even
// if the database is read, no usable token can be recovered.
//
// No salt, no bcrypt: the token is a 256-bit high-entropy random string, not a password chosen by
// a person. Rainbow tables and brute force do not apply, sha256 is enough, and it is fast enough
// not to matter for a once-per-request cost.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// constantTimeEqual is a constant-time string comparison, used to check the API Key.
//
// **Do not use ==**: that is a short-circuit comparison whose time varies with "how many of the
// first characters match", so in theory the key could be guessed byte by byte. The difference is
// almost unmeasurable locally, but this is the correct approach at zero cost.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// ---------------------------------------------------------------------------
// Memory implementation
// ---------------------------------------------------------------------------

// # ⚠️ Why it stays in auth instead of moving to repository/dao with PGStore
//
// On the core side both implementations live under `repository/dao` (`coredao` = PG, `coremem` =
// memory); **auth deliberately does not follow that.** This is not an oversight; it was decided
// after measuring. Do not move it again.
//
// The root of the difference is that `coremem` **uses not a single symbol of core**, so it can
// live in a sibling package that does not import core; the cycle never exists, at the cost of 0
// exports and 0 package moves. auth's MemStore is different: it uses Session / Login / User /
// APIKeyRecord / Store, plus auth's own `ErrNotFound` (9 places) and `ErrInvalidArgument` (6
// places). Those two sentinels **cannot move down into storemodel**: storemodel/errors.go says
// plainly that auth's and its own are not the same value, and errors.Is not matching between them
// is correct. So authmem would have to import auth, and the cycle cannot be avoided.
//
// ⚠️ That cycle **passes `go build` (measured: exit 0)**; only go vet / go test report
// `import cycle not allowed in test`. "It compiles" is not proof that it is feasible.
//
// What really blocks this is not exporting a few functions, but **two whitebox tests**:
//
//	apikey_test.go   TestAPIKeyPlaintextNeverStored reads store.mu / store.apiKeys directly and
//	                 checks every row and field that no plain text was stored (even the map keys)
//	service_test.go  likewise reads store.mu / store.sessions, plus store.now to inject the clock
//
// What they assert is exactly the security invariant "plain text is never stored", which **cannot
// be expressed through the public API** (the public API has no plain text at all). Moving the
// package would require exporting these fields, i.e. turning the private state of the memory
// implementation into public API just to move a directory. That costs much more than "the two
// implementations are not in the same layer".
//
// By the way, the test files cannot be moved one at a time: `fakeSSO` is defined in
// service_test.go and used 19 times in handler_test.go, passed down through fixture closures, so
// **all 82 test functions would have to move to an external package auth_test**, plus exporting 8
// unexported identifiers (measured with the compiler: credentialFrom / hashAPIKey / hashToken /
// looksLikeAPIKey / newAPIKey / newToken / constantTimeEqual / normalizeEmail / prefixDisplayLen;
// `deriveManagedKey` only appears in comments and does not count).
//
// **Contract discipline is not affected**: the suite in authdao/store_test.go already runs once
// against mem and once against pg, and it gets the instance through the public constructor
// `auth.NewMemStore()`, so across packages it is not affected by this decision at all.
// A reusable lesson: **the cost of "move or not" is a property of the destination, but "can it be
// moved" first depends on whether whitebox tests pin private state**; if they do, every
// destination is equally expensive.

// MemStore is the memory implementation of Store.
//
// **Restart the process and you must log in again**, and with multiple instances each instance
// only knows its own. So it should only be used for tests and AMA_STORE=mem. Production must use
// PGStore (in repository/dao/authdao). See the package comment.
type MemStore struct {
	mu         sync.RWMutex
	sessions   map[string]Session
	oidcStates map[string]OIDCState
	// githubFlows is indexed by flow id.
	githubFlows map[string]GitHubFlow
	// accounts is indexed by lower-cased email.
	accounts map[string]LocalAccount
	// failures is indexed by key; see RecordLoginFailure.
	failures map[string]loginFailure
	// apiKeys is indexed by hash: the check path looks up by hash, and it is the hottest path.
	// List and delete iterate over it; the size is "a few keys per person", so it does not matter.
	apiKeys map[string]APIKeyRecord
	// users is indexed by lower-cased email, matching lower(email) on the PG side.
	users map[string]User
	// now is only overridden by tests.
	now func() time.Time
}

var _ Store = (*MemStore)(nil)

// NewMemStore creates a memory Store.
func NewMemStore() *MemStore {
	return &MemStore{
		sessions:    make(map[string]Session),
		oidcStates:  make(map[string]OIDCState),
		githubFlows: make(map[string]GitHubFlow),
		accounts:    make(map[string]LocalAccount),
		failures:    make(map[string]loginFailure),
		apiKeys:     make(map[string]APIKeyRecord),
		users:       make(map[string]User),
		now:         time.Now,
	}
}

func (s *MemStore) PutSession(_ context.Context, session Session) error {
	if session.TokenHash == "" {
		return fmt.Errorf("%w: TokenHash 不能为空", ErrInvalidArgument)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[session.TokenHash] = session
	return nil
}

func (s *MemStore) GetSession(_ context.Context, tokenHash string) (Session, error) {
	if tokenHash == "" {
		return Session{}, ErrNotFound
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	session, ok := s.sessions[tokenHash]
	if !ok {
		return Session{}, ErrNotFound
	}
	// Expired means missing; see the comment on Store.GetSession.
	if session.Expired(s.now()) {
		return Session{}, ErrNotFound
	}
	return session, nil
}

func (s *MemStore) DeleteSession(_ context.Context, tokenHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, tokenHash)
	return nil
}

func (s *MemStore) DeleteSessionsByEmail(_ context.Context, email string) (int, error) {
	target := normalizeEmail(email)
	if target == "" {
		return 0, fmt.Errorf("%w: email 不能为空", ErrInvalidArgument)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for hash, session := range s.sessions {
		if session.Email == target {
			delete(s.sessions, hash)
			n++
		}
	}
	return n, nil
}

func (s *MemStore) PutOIDCState(_ context.Context, st OIDCState) error {
	if st.State == "" {
		return fmt.Errorf("%w: state 不能为空", ErrInvalidArgument)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.oidcStates[st.State] = st
	return nil
}

// ConsumeOIDCState is the memory implementation.
//
// **The whole function body runs under the write lock** (not the read lock), because it has to
// "check + update": checking under a read lock and then taking the write lock to update would let
// two goroutines both pass the check, so the same state is consumed twice. That is exactly the
// window the PG side avoids with a single UPDATE. Both implementations must behave the same here;
// otherwise the contract tests pass on memory and only fail on PG.
func (s *MemStore) ConsumeOIDCState(_ context.Context, state string) (OIDCState, error) {
	if state == "" {
		return OIDCState{}, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.oidcStates[state]
	// Missing / already used / expired: always ErrNotFound; see the comment on the Store interface.
	if !ok || !st.UsedAt.IsZero() || st.Expired(s.now()) {
		return OIDCState{}, ErrNotFound
	}
	st.UsedAt = s.now()
	s.oidcStates[state] = st
	return st, nil
}

// ---- GitHub device flow ----
//
// Each method runs entirely under the write lock: they are all "check + update", matching the
// single-statement behavior on the PG side.

func (s *MemStore) PutGitHubFlow(_ context.Context, f GitHubFlow) error {
	if f.FlowID == "" || f.DeviceCode == "" {
		return fmt.Errorf("%w: flow id 与 device code 都不能为空", ErrInvalidArgument)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for id, cur := range s.githubFlows {
		if cur.Expired(now) {
			delete(s.githubFlows, id)
		}
	}
	s.githubFlows[f.FlowID] = f
	return nil
}

func (s *MemStore) ClaimGitHubPoll(_ context.Context, flowID, bindingHash string, now time.Time) (GitHubFlow, error) {
	if flowID == "" || bindingHash == "" {
		return GitHubFlow{}, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.githubFlows[flowID]
	if !ok || f.BindingHash != bindingHash || f.Expired(now) {
		return GitHubFlow{}, ErrNotFound
	}
	if f.NextPollAt.After(now) {
		return GitHubFlow{IntervalSeconds: f.IntervalSeconds}, ErrPollTooSoon
	}
	f.NextPollAt = now.Add(time.Duration(f.IntervalSeconds) * time.Second)
	s.githubFlows[flowID] = f
	return f, nil
}

func (s *MemStore) SlowDownGitHubFlow(_ context.Context, flowID string, minInterval int, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.githubFlows[flowID]
	if !ok {
		return 0, ErrNotFound
	}
	f.IntervalSeconds = max(f.IntervalSeconds+githubSlowDownStep, minInterval)
	f.NextPollAt = now.Add(time.Duration(f.IntervalSeconds) * time.Second)
	s.githubFlows[flowID] = f
	return f.IntervalSeconds, nil
}

func (s *MemStore) ConsumeGitHubFlow(_ context.Context, flowID string, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.githubFlows[flowID]
	if !ok {
		return false, nil
	}
	delete(s.githubFlows, flowID)
	return !f.Expired(now), nil
}

func (s *MemStore) DeleteGitHubFlow(_ context.Context, flowID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.githubFlows, flowID)
	return nil
}

// ---- API Key ----

func (s *MemStore) PutAPIKey(_ context.Context, key APIKeyRecord) error {
	if strings.TrimSpace(key.KeyHash) == "" {
		return fmt.Errorf("%w: KeyHash 不能为空", ErrInvalidArgument)
	}
	if strings.TrimSpace(key.ID) == "" {
		return fmt.Errorf("%w: ID 不能为空", ErrInvalidArgument)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.apiKeys[key.KeyHash] = key
	return nil
}

func (s *MemStore) GetAPIKeyByHash(_ context.Context, keyHash string) (APIKeyRecord, error) {
	if keyHash == "" {
		return APIKeyRecord{}, ErrNotFound
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.apiKeys[keyHash]
	if !ok {
		return APIKeyRecord{}, ErrNotFound
	}
	return rec, nil
}

func (s *MemStore) ListAPIKeysByEmail(_ context.Context, email string) ([]APIKeyRecord, error) {
	target := normalizeEmail(email)
	if target == "" {
		return nil, fmt.Errorf("%w: email 不能为空", ErrInvalidArgument)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []APIKeyRecord
	for _, rec := range s.apiKeys {
		if rec.Email == target {
			out = append(out, rec)
		}
	}
	// Newest first by creation time; ID breaks ties at the same moment.
	//
	// **Ties need a fixed order**: two keys created back to back in a test may have the same
	// CreatedAt (time.Now's precision does not guarantee two adjacent calls differ). Sorting by time
	// only would make the order random, so asserting "newest first" would fail now and then, and
	// that kind of flaky failure is the hardest to track down.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

func (s *MemStore) DeleteAPIKey(_ context.Context, email, id string) (bool, error) {
	target := normalizeEmail(email)
	if target == "" || strings.TrimSpace(id) == "" {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for hash, rec := range s.apiKeys {
		// **Ownership is part of the condition**: someone else's key does not match here, so the
		// "check ownership, then delete" step is not needed (and neither is its TOCTOU window).
		if rec.ID == id && rec.Email == target {
			delete(s.apiKeys, hash)
			return true, nil
		}
	}
	return false, nil
}

func (s *MemStore) TouchAPIKey(_ context.Context, keyHash string, at time.Time) error {
	if keyHash == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.apiKeys[keyHash]
	if !ok {
		return nil
	}
	rec.LastUsedAt = at
	s.apiKeys[keyHash] = rec
	return nil
}

func (s *MemStore) PutUser(_ context.Context, user User) error {
	key := strings.ToLower(strings.TrimSpace(user.Email))
	if key == "" {
		return fmt.Errorf("auth: 用户邮箱不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.users[key]; ok {
		// Empty values do not overwrite: SSO does not guarantee a name and avatar every time, and
		// writing an empty string back would erase what we already had.
		if user.Name == "" {
			user.Name = cur.Name
		}
		if user.AvatarURL == "" {
			user.AvatarURL = cur.AvatarURL
		}
		// CreatedAt is "the first time we saw this person"; later logins should not refresh it.
		user.CreatedAt = cur.CreatedAt
	}
	user.Email = key
	s.users[key] = user
	return nil
}

func (s *MemStore) GetUser(_ context.Context, email string) (User, error) {
	key := strings.ToLower(strings.TrimSpace(email))
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[key]
	if !ok {
		return User{}, ErrNotFound
	}
	return u, nil
}

// ListUsers: see the rules on the Store interface: emails not found are left out of the map, and
// that is not an error.
func (s *MemStore) ListUsers(_ context.Context, emails []string) (map[string]User, error) {
	out := make(map[string]User, len(emails))
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range emails {
		key := strings.ToLower(strings.TrimSpace(e))
		if key == "" {
			continue
		}
		if u, ok := s.users[key]; ok {
			out[key] = u
		}
	}
	return out, nil
}

// ---- Local accounts ----

func (s *MemStore) CreateLocalAccount(_ context.Context, acct LocalAccount) (bool, error) {
	email := normalizeEmail(acct.Email)
	if email == "" || acct.PasswordHash == "" {
		return false, fmt.Errorf("%w: 邮箱与密码哈希都不能为空", ErrInvalidArgument)
	}
	acct.Email = email
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.accounts[email]; ok {
		return false, nil
	}
	// Same as PG: a normal create never sets the bootstrap flag (that row can only be written by
	// CreateFirstLocalAccount).
	acct.Bootstrap = false
	s.accounts[email] = acct
	return true, nil
}

// CreateFirstLocalAccount is the memory implementation. **The check and the write are under the
// same write lock**, matching the single insert on PG: checking under a read lock and then writing
// under the write lock would let two concurrent calls both succeed.
func (s *MemStore) CreateFirstLocalAccount(_ context.Context, acct LocalAccount) (bool, error) {
	email := normalizeEmail(acct.Email)
	if email == "" || acct.PasswordHash == "" {
		return false, fmt.Errorf("%w: 邮箱与密码哈希都不能为空", ErrInvalidArgument)
	}
	acct.Email = email
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.users) > 0 || len(s.accounts) > 0 {
		return false, nil
	}
	acct.Bootstrap = true
	s.accounts[email] = acct
	return true, nil
}

func (s *MemStore) GetLocalAccount(_ context.Context, email string) (LocalAccount, error) {
	key := normalizeEmail(email)
	if key == "" {
		return LocalAccount{}, ErrNotFound
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	acct, ok := s.accounts[key]
	if !ok {
		return LocalAccount{}, ErrNotFound
	}
	return acct, nil
}

func (s *MemStore) HasAnyUser(_ context.Context) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.users) > 0 || len(s.accounts) > 0, nil
}

// loginFailure is the failure count of one key (fixed window).
type loginFailure struct {
	failures    int
	windowStart time.Time
}

func (s *MemStore) RecordLoginFailure(_ context.Context, key string, now time.Time, window time.Duration) (int, error) {
	if strings.TrimSpace(key) == "" {
		return 0, fmt.Errorf("%w: key 不能为空", ErrInvalidArgument)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.failures[key]
	if !ok || f.windowStart.Before(now.Add(-window)) {
		f = loginFailure{failures: 0, windowStart: now}
	}
	f.failures++
	s.failures[key] = f
	return f.failures, nil
}

func (s *MemStore) ClearLoginFailures(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.failures, key)
	return nil
}

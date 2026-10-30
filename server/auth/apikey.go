// API Key: a long-lived credential for **external agents / scripts / CI**.
//
// # Not the same thing as the APIKey in config; do not mix them up
//
//	auth.APIKey (service.go)      a process-level **singleton**: one key set by environment
//	                              variables, not set in production by default. For e2e / CI /
//	                              probes, not for users. **Kept as is.**
//	auth.UserAPIKey (this file)   created by users themselves, **many of them**, stored in the
//	                              database, revocable, with the last-used time recorded.
//
// Having both channels is deliberate: the first is a side door set by operators (changing it
// requires a config change), the second is a product feature (users create and revoke their own).
// Merging them would mean "rotate the CI key" goes through the product UI, and CI is exactly what
// runs when nobody is around.
//
// # A key is bound to a person, not to a workspace
//
// **This is the most important design decision in this file.** Permissions are already decided
// by authz from "this person's role in this workspace" (56 route declarations); a key is only
// **another way to prove "who I am"** and should not set up a separate permission system.
//
// A "workspace-level key" would mean two parallel permission models (the key's scope vs the
// member role), and two models will sooner or later disagree. Then "what can this key actually
// do" has no single answer, and the inconsistency shows up as "an endpoint is sometimes 403,
// sometimes 200", which does not point to the real cause.
//
// Letting the key inherit its creator's role reuses those 56 declarations automatically:
// **calling an endpoint with a key is exactly the same as the person calling it while logged
// in**. The cost is that "give the agent a read-only key" cannot be expressed; that has to wait
// for a scope field when there is a real need, instead of building a permission system nobody
// uses now.
//
// The UI lives in workspace settings (that is where users manage things), but **the data model
// is account-level**: opening it from another workspace shows the same keys. These two facts do
// not conflict.
//
// # Only the sha256 is stored; the plain text is returned once, at creation
//
// Exactly the same approach as session tokens (see "The session credential is a random token,
// not a JWT" in auth.go): the database stores the hash, and an incoming key is hashed and looked
// up. So **even if the database is read, no usable key can be recovered**.
//
// The cost is that a user who loses a key can only revoke it and create a new one. That is
// exactly the right behavior for "the plain text appears only once": a credential that can be
// shown again means the plain text is stored.

package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// KeyPrefix is the fixed prefix of an API Key's plain text.
//
// There are two reasons for it; the second one is the main one:
//
//  1. You can tell at a glance what it is (compared with a base64 string with no context).
//  2. **Secret scanners can recognize it.** A credential leaked into a code repository / logs /
//     chat history can only be found automatically if it has a fixed prefix. Scanners cannot
//     recognize a high-entropy string without a prefix, so a leak goes unnoticed. That is the
//     worst kind, because it produces no signal at all.
const KeyPrefix = "amak_"

// prefixDisplayLen is how many characters are shown in the list.
//
// Prefix + 6 characters: enough to tell a few keys apart (users know which is which), and far
// from enough to brute-force the rest (more than 250 bits of entropy remain).
const prefixDisplayLen = len(KeyPrefix) + 6

// UserAPIKey is the **public view** of an API Key created by a user.
//
// **It has no plain text**, only Prefix (the first few characters, just to recognize which key it
// is in a list). The plain text appears only once, in the return value of CreateAPIKey; see
// CreatedAPIKey.
type UserAPIKey struct {
	// ID is the id of this record, used to revoke it. **It is not the key itself.**
	ID string `json:"id"`

	// Name is the name given at creation, to tell keys apart later (e.g. ci-deploy / my-agent).
	Name string `json:"name"`

	// Prefix is the first few characters of the plain text, like amak_a1b2c3.
	Prefix string `json:"prefix"`

	// CreatedAt is the creation time, a fixed-length ISO string (same shape as apimodel.NowISO).
	CreatedAt string `json:"createdAt"`

	// LastUsedAt is the last time it was used to call an endpoint.
	//
	// **It has omitempty; missing means never used**, so users can find keys they can revoke.
	// An empty string would merge "never used" and "used but the time was not recorded" into one
	// value.
	LastUsedAt string `json:"lastUsedAt,omitempty"`
}

// CreatedAPIKey is a newly created key, **the only form that carries the plain text**.
type CreatedAPIKey struct {
	UserAPIKey

	// Key is the plain text, like amak_xxx.
	//
	// ⚠️ **It appears only in the response of that one creation and can never be fetched again**
	// (the database only has the sha256).
	// No omitempty: it always has a value, and a missing key would make the caller think creation
	// failed.
	Key string `json:"key"`
}

// APIKeyRecord is the stored form. **It never appears in any HTTP response.**
type APIKeyRecord struct {
	// ID is the primary key.
	ID string
	// KeyHash is the sha256 (hex) of the plain text. Lookups match on it.
	KeyHash string
	// Email is who the key belongs to, **which is also its identity**. Already lower-cased.
	Email string
	// Name is the display name.
	Name string
	// Prefix is the first few characters of the plain text, for display only.
	Prefix string
	// CreatedAt / LastUsedAt use time.Time: this table is not part of the byte-sensitive frontend
	// contract (same as auth_sessions; see the header of schema_pg.sql), so the database can do the
	// time comparisons.
	CreatedAt time.Time
	// A zero LastUsedAt means never used.
	LastUsedAt time.Time
}

// View turns the stored form into the public view (dropping the hash and the owner).
func (r APIKeyRecord) View() UserAPIKey {
	out := UserAPIKey{
		ID:        r.ID,
		Name:      r.Name,
		Prefix:    r.Prefix,
		CreatedAt: formatISO(r.CreatedAt),
	}
	// Zero means never used; in that case the key must **disappear entirely** rather than become
	// a fake timestamp.
	if !r.LastUsedAt.IsZero() {
		out.LastUsedAt = formatISO(r.LastUsedAt)
	}
	return out
}

// isoLayout has the same shape as apimodel.NowISO (UTC, exactly three decimals, literal Z).
//
// It does not import model/apimodel: auth is a lower-level package, and one format string is not
// worth a dependency. For why the shape must match, see "timestamps are opaque strings" in the
// model/apimodel package comment.
const isoLayout = "2006-01-02T15:04:05.000Z"

func formatISO(t time.Time) string { return t.UTC().Format(isoLayout) }

// ---------------------------------------------------------------------------
// Generation and checking
// ---------------------------------------------------------------------------

// newAPIKey generates a new key and returns the plain text and its display prefix.
//
// It reuses newToken's 32 random bytes (256 bits): the same strength as session tokens; see the
// comment on tokenBytes for why.
func newAPIKey() (plaintext, prefix string, err error) {
	raw, err := newToken()
	if err != nil {
		return "", "", err
	}
	plaintext = KeyPrefix + raw
	// The plain text is always longer than the display length (32 bytes in base64 give 43
	// characters), but check anyway: if someone ever lowers tokenBytes, an out-of-range panic here
	// would be hard to track down.
	if len(plaintext) < prefixDisplayLen {
		return plaintext, plaintext, nil
	}
	return plaintext, plaintext[:prefixDisplayLen], nil
}

// hashAPIKey turns a plain key into the hash stored in the database.
//
// Same algorithm as hashToken, but **deliberately two functions**: they hash two different kinds
// of credentials, stored in two different tables. With one function, adding a salt / changing the
// algorithm for one later would silently affect the other.
func hashAPIKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// looksLikeAPIKey tells whether a credential **looks like** an API Key.
//
// It saves a useless database query: the credential Resolve gets may be a session token, which
// has no amak_ prefix, so there is no need to look it up in the api_keys table.
//
// ⚠️ **This is not a check**, only a quick rule-out. The real decision is the database lookup.
func looksLikeAPIKey(cred string) bool {
	return strings.HasPrefix(cred, KeyPrefix)
}

// ---------------------------------------------------------------------------
// The three operations on Service
// ---------------------------------------------------------------------------

// CreateAPIKey creates a key for email and returns the result **with the plain text**.
//
// The plain text exists only in this return value; Service keeps no copy.
func (s *Service) CreateAPIKey(ctx context.Context, email, name string) (CreatedAPIKey, error) {
	owner := normalizeEmail(email)
	if owner == "" {
		return CreatedAPIKey{}, fmt.Errorf("%w: email 不能为空", ErrInvalidArgument)
	}
	label := strings.TrimSpace(name)
	if label == "" {
		return CreatedAPIKey{}, fmt.Errorf("%w: 请给这把 key 起个名字", ErrInvalidArgument)
	}
	if len(label) > apiKeyNameMaxLen {
		return CreatedAPIKey{}, fmt.Errorf("%w: 名字不能超过 %d 个字符", ErrInvalidArgument, apiKeyNameMaxLen)
	}

	plaintext, prefix, err := newAPIKey()
	if err != nil {
		return CreatedAPIKey{}, err
	}
	id, err := newToken()
	if err != nil {
		return CreatedAPIKey{}, err
	}

	rec := APIKeyRecord{
		// The id is an opaque random string instead of an auto-increment number: with multiple
		// instances there is no central place to hand out numbers; see the "auto-increment IDs,
		// sequence numbers" row in CLAUDE.md.
		ID:        "ak_" + id[:16],
		KeyHash:   hashAPIKey(plaintext),
		Email:     owner,
		Name:      label,
		Prefix:    prefix,
		CreatedAt: s.now(),
	}
	if err := s.store.PutAPIKey(ctx, rec); err != nil {
		return CreatedAPIKey{}, err
	}
	s.log.InfoContext(ctx, "创建 API Key",
		"email", owner, "keyId", rec.ID, "name", label)
	return CreatedAPIKey{UserAPIKey: rec.View(), Key: plaintext}, nil
}

// apiKeyNameMaxLen is the maximum name length.
//
// The limit stops people from stuffing a whole JSON document in as the name (which would make the
// list page unreadable); 128 is more than enough for uses like "ci-deploy" or "alice's local
// script".
const apiKeyNameMaxLen = 128

// ListAPIKeys lists all keys of one person, newest first by creation time.
//
// **It never returns the plain text**, and never returns other people's keys.
func (s *Service) ListAPIKeys(ctx context.Context, email string) ([]UserAPIKey, error) {
	owner := normalizeEmail(email)
	if owner == "" {
		return nil, fmt.Errorf("%w: email 不能为空", ErrInvalidArgument)
	}
	recs, err := s.store.ListAPIKeysByEmail(ctx, owner)
	if err != nil {
		return nil, err
	}
	// **It must be a non-nil empty slice**: nil encodes as null, but the contract requires []
	// (see rule one in model/apimodel; the frontend calls .map() on it directly).
	out := make([]UserAPIKey, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.View())
	}
	return out, nil
}

// RevokeAPIKey revokes a key, effective immediately.
//
// **You can only revoke your own**: when the id exists but belongs to someone else, ErrNotFound
// is returned instead of a permission error. Answering "access denied" would confirm "this id
// exists", which is an unnecessary leak of information.
// For the caller both cases are handled the same way too (this key is not yours).
func (s *Service) RevokeAPIKey(ctx context.Context, email, id string) error {
	owner := normalizeEmail(email)
	if owner == "" {
		return fmt.Errorf("%w: email 不能为空", ErrInvalidArgument)
	}
	keyID := strings.TrimSpace(id)
	if keyID == "" {
		return ErrNotFound
	}
	deleted, err := s.store.DeleteAPIKey(ctx, owner, keyID)
	if err != nil {
		return err
	}
	if !deleted {
		return ErrNotFound
	}
	s.log.InfoContext(ctx, "吊销 API Key", "email", owner, "keyId", keyID)
	return nil
}

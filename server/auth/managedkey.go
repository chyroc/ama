// Platform-managed API Keys: **the plain text is neither stored nor cached; it is computed from a
// server secret every time.**
//
// This is the credential (`AMA_TOKEN`) for the `ama` command inside containers. It **shares one
// table and one check path** with the keys users create themselves in apikey.go; the only
// difference is where the plain text comes from:
//
//	User-created (apikey.go)    crypto/rand random → plain text appears once in the create
//	                            response → never available afterwards
//	Platform-managed (this file) HMAC(secret, stable id) → **can be computed again at any time,
//	                            on any instance**
//
// # Why it must be "derived" instead of "random + stored"
//
// Injection happens **every time a tool command runs** (see
// shared/infra/shelltools/credential.go), not in a one-off HTTP response. So this path needs "get
// the same plain text again and again", while randomly generated plain text is gone forever once
// its hash is stored: the second exec would have nothing to read.
//
// Three constraints that would otherwise conflict all hold at once:
//
//  1. **The database still only has the sha256.** The plain text is neither stored nor kept in
//     any in-process cache, so the invariant "even if the database is read, no usable key can be
//     recovered" is not broken by a single byte.
//  2. **Every exec can get the plain text.** Because it is computed, not fetched.
//  3. **It is a real API Key.** It is a row in auth_api_keys and goes through the existing check
//     path looksLikeAPIKey → GetAPIKeyByHash with no line changed; users can see it on the
//     settings page and revoke it.
//
// # Why this also solves the multi-instance problem
//
// "Each instance generates its own random value" is the root of that class of failures: A creates
// one key, B another, and any "clear the old platform key" logic makes the two instances step on
// each other: A deletes B's key, B's token gets a 401 on the spot, so B creates another one, and
// round it goes. **Derivation is deterministic**: N instances compute byte-identical strings, so
// there is no "old key" at all and nothing to clean up.
// Creating the row uses `ON CONFLICT DO NOTHING` (key_hash already has a unique constraint, which
// exists exactly for this), so whoever goes first, the result is right. This is the CLAUDE.md rule
// "creation with remote side effects → unique constraint + ON CONFLICT, let the database decide".
//
// # ⚠️ Revoking means "reset", not "rotate"
//
// After a revoke, the next injection derives **the same plain text** and creates the row again.
// Its limit must be stated clearly: **if that token has leaked, revoking it once does not make it
// invalid** (the next injection brings it back to life). To really get a different key there are
// only two ways: replace the secret below (affects all workspaces), or add a version field to the
// stable id. Do not read this behavior as "revoke = invalidate".
//
// ⚠️ **Revoking now only has one entry point: the "API Key" page.** The AMA card on the
// authorization management page used to have a "remove authorization" button (the requirement at
// the time was "revocable; after revoking it is created again automatically next time"). The
// requirement changed to **removing authorization is not supported**, and that button was removed
// together with the provider's revoke capability (see the file header of
// service/integration/ama.go).
//
// That page uses the generic `RevokeAPIKey` (`DELETE /api/api-keys/{id}`), **which does not go
// through this file**: a managed key is just an ordinary row in auth_api_keys, and that path only
// deletes by id + email, without checking whether it is managed. So `RevokeManagedAPIKey` below
// **has no production caller** now; only tests use it. (It is still the only expression of
// "compute the id for a person, then delete", so keeping it is better than deleting it: if an
// operator tool for "reset by person" is ever needed, this is it.)
//
// # Rotating the secret
//
// Replacing managedKeySecret means: AMA_TOKEN in every container becomes invalid at once, the next
// exec derives a new string and creates the row automatically, and the old rows become garbage
// that never matches again (you can keep them or clean them up by hand).
// That cost is acceptable, so **no version prefix is added for it**, deliberately the opposite of
// cryptobox: there the ciphertext **cannot be regenerated** (lose the secret and the user's Ark
// key is lost forever), so it must have a version field; here everything can be recomputed at any
// time, so rotating only makes everyone fetch it once more.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// managedKeyDomain is the domain separation prefix of the HMAC.
//
// **It must exist, and it must carry a version.** The same secret may later be used to derive
// other things (for other CLIs or other credential types); without domain separation two uses
// would compute the same string on the same input, so one leak equals two leaks. The version `v1`
// leaves room for "change the derivation later without changing the secret".
const managedKeyDomain = "amacli:v1:"

// ManagedKeyName is the name this row shows in the list.
//
// A fixed value instead of one passed in by the caller: in "the key list the user sees" it must
// be recognizable at a glance as created by the platform, not by the user. Users can revoke it,
// but should not think they created it by accident.
const ManagedKeyName = "AMA CLI（平台自动创建）"

// ErrManagedKeyUnavailable means no derivation secret is configured, so this feature is entirely
// unavailable.
//
// **Strictly separate from "revoked"**: not configured is a missing deployment config (an admin
// has to set it), revoked is the user's own choice. They are handled completely differently;
// merging them into one error would make users hunt for a way to undo "you revoked it", while the
// problem is actually in the server config.
var ErrManagedKeyUnavailable = errors.New("auth: 未配置平台托管 API Key 的推导密钥")

// ManagedKeySubject is the stable id used for derivation: **only the person, no workspace.**
//
// # Why not one per workspace
//
// "One key per workspace" feels safer, but that is an illusion: **this key only proves "who I
// am"**; permissions are decided on the spot by authz from "this person's role in this
// workspace" ("A key is bound to a person, not to a workspace" in apikey.go).
// So a token leaked from a container of workspace A can call the endpoints of workspace B just
// the same. **Splitting by workspace gives no isolation at all**, only N keys and a card that
// cannot show them (the authorization management page is account-level; its path has no
// workspaceId at all).
//
// Telling the container "which workspace this is" is a different matter; it uses the separate
// AMA_WORKSPACE_ID environment variable and should not be encoded in the credential.
type ManagedKeySubject struct {
	// Email is the person this key belongs to, which is also its identity. It is lower-cased.
	Email string
}

// normalize normalizes and checks it is not empty.
func (s ManagedKeySubject) normalize() (ManagedKeySubject, error) {
	out := ManagedKeySubject{Email: normalizeEmail(s.Email)}
	if out.Email == "" {
		return out, fmt.Errorf("%w: Email 不能为空", ErrInvalidArgument)
	}
	return out, nil
}

// hmacInput builds the HMAC input.
//
// No separator is needed between the domain prefix and the email (the prefix is a fixed-length
// constant), but a `\x00` is kept anyway: if a second field is ever added to this id, without a
// separator ("a","b") and ("ab","") would collide into the same key, and that kind of
// concatenation ambiguity gives no error, it just silently breaks isolation. Keep it now so
// nobody has to think back when adding a field.
func (s ManagedKeySubject) hmacInput() string {
	return managedKeyDomain + s.Email + "\x00"
}

// deriveManagedKey computes the plain key from the secret and the id.
//
// Deterministic: the same inputs always give the same output, independent of process, instance
// and time. That is why no coordination is needed with multiple instances.
func deriveManagedKey(secret string, subj ManagedKeySubject) string {
	mac := hmac.New(sha256.New, []byte(secret))
	// Write on hash.Hash never returns an error (the docs say so), so it is not checked here.
	mac.Write([]byte(subj.hmacInput()))
	// RawURLEncoding: the same encoding as newToken, so both kinds of keys have the same shape
	// (amak_ + 43 URL-safe characters), and looksLikeAPIKey and the prefix display logic work for
	// both.
	return KeyPrefix + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// ManagedKeyEnabled reports whether the derivation secret is configured.
//
// The wiring code uses it to decide whether to wire this feature, and to state the consequence
// in the startup log. Same purpose as cryptobox.Enabled(): the two modes look the same in
// behavior, and only the log tells operators which mode is active.
func (s *Service) ManagedKeyEnabled() bool {
	return strings.TrimSpace(s.managedKeySecret) != ""
}

// EnsureManagedAPIKey gets (creating it if needed) this person's platform-managed key and returns
// the **plain text**.
//
// # The key difference from CreateAPIKey
//
// CreateAPIKey creates a **new** (random) key on every call, so it may only be called from the
// HTTP path "the user clicked the create button". This method is **safe to repeat**: repeated
// calls return the same plain text and the database always has just one row, so it can be called
// again and again by "automatic injection in the background with nobody around". That is the
// whole reason this method exists.
//
// # It comes back to life after a revoke
//
// After the user revokes it on the settings page (deleting the row), the next injection gets here,
// finds no row, and creates one again; the plain text is still the same string. This is the
// required behavior (revoke → created again automatically at the next injection), but see "Revoking
// means reset, not rotate" in the file header for its limit.
func (s *Service) EnsureManagedAPIKey(
	ctx context.Context, subj ManagedKeySubject,
) (string, error) {
	secret := strings.TrimSpace(s.managedKeySecret)
	if secret == "" {
		return "", ErrManagedKeyUnavailable
	}
	norm, err := subj.normalize()
	if err != nil {
		return "", err
	}

	plaintext := deriveManagedKey(secret, norm)
	hash := hashAPIKey(plaintext)

	// If it already exists, return it right away without any write.
	//
	// **This fast path is not only about saving a write**: injection is a hot path taken on every
	// exec, and almost always the row is already in the database.
	if _, err := s.store.GetAPIKeyByHash(ctx, hash); err == nil {
		return plaintext, nil
	} else if !errors.Is(err, ErrNotFound) {
		// A database hiccup must not be treated as "no such row": that would make us create a row,
		// the unique constraint would make that write fail, and the error would drift from "read
		// failed" to "write failed", which does not point to the real cause.
		return "", fmt.Errorf("查平台托管 API Key 失败: %w", err)
	}

	// Create it if missing.
	//
	// ⚠️ **The id must be derived too, not random.** When two instances get here at the same time,
	// random ids make each of them insert with a different primary key, and PutAPIKey's
	// `on conflict (id)` does not hit. Then the unique constraint on key_hash blocks the second
	// one, with error 23505, and an injection that should succeed fails. With a derived id the two
	// instances' inserts are identical, and `on conflict (id) do update` makes the later one a
	// harmless rewrite.
	rec := APIKeyRecord{
		ID:        managedKeyID(hash),
		KeyHash:   hash,
		Email:     norm.Email,
		Name:      ManagedKeyName,
		Prefix:    managedKeyPrefix(plaintext),
		CreatedAt: s.now(),
	}
	if err := s.store.PutAPIKey(ctx, rec); err != nil {
		return "", fmt.Errorf("创建平台托管 API Key 失败: %w", err)
	}
	// **Log only the id and the owner, never the plain text** (same as the warning on the
	// CreateAPIKey handler).
	s.log.InfoContext(ctx, "已自动创建平台托管 API Key（供容器内 ama 命令使用）",
		"email", norm.Email, "keyId", rec.ID)
	return plaintext, nil
}

// managedKeyID derives this row's primary key from the key's hash.
//
// It takes the first 16 hex characters of the hash and adds the `ak_` prefix, the same shape as
// user-created keys (the revoke endpoint finds by id, so both kinds must look alike to share one
// path). **It contains no secret**: the hash itself can be public (it is what the database
// stores); truncating only makes the id shorter.
func managedKeyID(keyHash string) string {
	return "ak_" + keyHash[:16]
}

// managedKeyPrefix returns the display prefix of the plain text.
//
// Exactly the same rule as newAPIKey (KeyPrefix + 6 characters); otherwise the two kinds of keys
// in the same list would have prefixes of two lengths, which looks like one of them was cut off.
func managedKeyPrefix(plaintext string) string {
	if len(plaintext) < prefixDisplayLen {
		return plaintext
	}
	return plaintext[:prefixDisplayLen]
}

// ManagedAPIKeyRecord checks whether this person's managed key is in the database now.
//
// For the "authorization management" card: it needs to show the prefix and creation time, and
// **must not show the plain text**.
// ErrNotFound means the user revoked it (or it has never been injected).
//
// ⚠️ **It deliberately does not return the plain text**, although it could be computed here. The
// card is an HTTP response; plain text in the response ends up in the browser, in logs, in
// screenshots, and it is a long-lived credential. Being able to compute it does not mean it
// should be handed out.
func (s *Service) ManagedAPIKeyRecord(
	ctx context.Context, subj ManagedKeySubject,
) (UserAPIKey, error) {
	secret := strings.TrimSpace(s.managedKeySecret)
	if secret == "" {
		return UserAPIKey{}, ErrManagedKeyUnavailable
	}
	norm, err := subj.normalize()
	if err != nil {
		return UserAPIKey{}, err
	}
	rec, err := s.store.GetAPIKeyByHash(ctx, hashAPIKey(deriveManagedKey(secret, norm)))
	if err != nil {
		return UserAPIKey{}, err
	}
	return rec.View(), nil
}

// RevokeManagedAPIKey revokes this person's managed key.
//
// It reuses RevokeAPIKey (delete by id + email), so "revoke a managed key" and "revoke a
// self-created key" go through the same path and the same ownership check. **No new delete path is
// added**: with two paths, each one's "you can only delete your own" would drift on its own.
//
// After a revoke, the next injection creates the same key again; see the file header.
func (s *Service) RevokeManagedAPIKey(ctx context.Context, subj ManagedKeySubject) error {
	secret := strings.TrimSpace(s.managedKeySecret)
	if secret == "" {
		return ErrManagedKeyUnavailable
	}
	norm, err := subj.normalize()
	if err != nil {
		return err
	}
	hash := hashAPIKey(deriveManagedKey(secret, norm))
	return s.RevokeAPIKey(ctx, norm.Email, managedKeyID(hash))
}

// ManagedKeysFor narrows Service down to a small adapter with "only the two managed-key
// operations".
//
// core's AMA provider only needs these two things; giving it the whole *Service would make "what
// else does it touch here" something you can only learn by reading the code (Service also has
// login, sessions, SSO, dozens of methods).
//
// **The adapter lives in auth, not in core**: the shape of the method signatures is decided by
// types here (UserAPIKey / ErrNotFound both belong to this package); putting it on the other side
// would make core guess the meaning of things here.
func (s *Service) ManagedKeysFor() ManagedKeyOps { return managedKeyOps{svc: s} }

// ManagedKeyOps is the set of operations ManagedKeysFor returns.
//
// core declares an interface of the same shape (core.ManagedAPIKeys), and the two meet through
// Go's implicit interface satisfaction. **core is deliberately not made to import this interface
// type**: that would make core depend on a type that only exists for wiring, while in the
// dependency direction core should only know auth's data types (UserAPIKey) and sentinel errors.
//
// ⚠️ **There used to be a Revoke here; it was removed**: the AMA card no longer supports removing
// authorization (see "Revoking means reset" above), and keeping this method would leave that
// provider a way to delete credentials that has no UI and that nobody would think to look at. The
// revoke path is on the API Key page, using the generic `RevokeAPIKey`, not this adapter.
type ManagedKeyOps interface {
	Record(ctx context.Context, email string) (UserAPIKey, error)
	Enabled() bool
}

type managedKeyOps struct{ svc *Service }

func (o managedKeyOps) Record(ctx context.Context, email string) (UserAPIKey, error) {
	return o.svc.ManagedAPIKeyRecord(ctx, ManagedKeySubject{Email: email})
}

func (o managedKeyOps) Enabled() bool { return o.svc.ManagedKeyEnabled() }

// EnsureManagedKey is the narrow-signature version for the injection path on the core side.
//
// core.ManagedKeyIssuer only needs "give an email, get the plain text". Making it build a
// ManagedKeySubject would leak the internal shape from here to there: when a field is added to the
// id later, core would have to change too, although it does not care about that at all.
func (s *Service) EnsureManagedKey(ctx context.Context, email string) (string, error) {
	return s.EnsureManagedAPIKey(ctx, ManagedKeySubject{Email: email})
}

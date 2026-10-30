// Package cryptobox is **decryptable** credential storage: it encrypts keys that must be sent
// as is to external services before storing them, and decrypts them back to plaintext when used.
//
// # It is the opposite direction from the sha256 used for auth_api_keys
//
//	Ark API key    we use it to call Ark for inference → **the plaintext must be recoverable** → this package (symmetric encryption)
//	auth_api_keys  others use it to call us, we only compare → store only sha256, never recover
//
// Getting the direction wrong costs asymmetrically: storing only a hash of an Ark key means never
// getting a usable key back (the data is irreversibly ruined); the other way round, decryptable
// storage for our own API keys would add an "a database leak is a credential leak" exposure for
// nothing. So the two sides deliberately use two mechanisms; do not merge them.
//
// # Ciphertext format: the version prefix is the whole reason existing plaintext keeps working
//
//	v1:<base64(nonce || ciphertext+tag)>   encrypted
//	<any other shape>                       existing plaintext, Open returns it as is
//
// Before this shipped, the database already had a batch of plaintext rows
// (global_ark_secrets.api_key is bare text). Open recognizes the prefix, so **existing rows keep
// working with zero migration**, while all writes are encrypted. Gradual migration happens
// naturally: no downtime, no backfill script, and either release order works.
//
// ⚠️ **Do not change this to "add an encrypted bool column".** That needs a DDL change, and the
// default for existing rows must be false; as soon as some read path forgets to read that column,
// it sends the ciphertext to Ark as is. Ark answers 401, and the error says "API key invalid",
// which does not point at "you sent the ciphertext as plaintext" at all. The prefix describes
// itself, the check travels with the data, and it cannot be missed.
//
// # Without a key it degrades to plaintext, on purpose
//
// A hard error would keep the service from starting, and "the service does not start" is far
// worse than "plaintext in the database"; the latter is the state we already ran in. So without a
// key, Enabled() is false, Seal returns its input as is, and the caller **states the consequence**
// in the startup log (saying only "encryption key not configured" is not enough for people to
// decide whether to act).
//
// # Key rotation is not supported yet
//
// The `v1:` version marker is the opening left for it. Real rotation needs a `v2:`, keeping v1
// decryption, and re-Sealing existing rows. **Do not think changing the key in config rotates
// it**: the result would be that all v1 rows cannot be decrypted, and the symptom is "everyone's
// Ark credentials fail at once".
package cryptobox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors of this package.
var (
	// ErrBadKey means a key is configured but invalid (not base64, or it does not decode to 32
	// bytes).
	//
	// **Strictly separate from "no key configured"**: no key is a degrade (plaintext, service keeps
	// running), a wrong key is a config incident (fix it now); the two are handled in opposite
	// ways. Merging them into one error would treat a wrong key as no key, so it would quietly keep
	// running with plaintext, while ops think encryption is on.
	ErrBadKey = errors.New("cryptobox: 加密密钥不合法")
	// ErrCorrupted means this ciphertext cannot be decrypted (wrong key, or data truncated /
	// tampered with).
	ErrCorrupted = errors.New("cryptobox: 密文解不开")
)

// Prefix is the version prefix of ciphertext. **Plaintext never starts with it**, see the
// comment on Open.
const Prefix = "v1:"

// KeySize is the key size in bytes required by AES-256.
const KeySize = 32

// Box is an encrypter/decrypter, safe for concurrent use (aead itself is stateless, and Seal uses
// a fresh random nonce).
//
// The zero value is usable and means "no key": Enabled() is false, and Seal returns the plaintext
// as is.
type Box struct {
	aead cipher.AEAD
}

// New builds a Box.
//
// An empty masterKeyBase64 means degrade mode (returns a usable zero Box and nil error, see the
// package comment); non-empty but not decoding to KeySize bytes returns ErrBadKey: a wrong key
// must be called out right away, not left for runtime.
func New(masterKeyBase64 string) (*Box, error) {
	raw := strings.TrimSpace(masterKeyBase64)
	if raw == "" {
		// Degrade: not an error, an explicit operating mode.
		return &Box{}, nil
	}
	key, err := decodeKey(raw)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadKey, err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadKey, err)
	}
	return &Box{aead: aead}, nil
}

// decodeKey decodes the base64 string from config into KeySize bytes.
//
// It accepts both the standard and URL variants of base64: keys are usually copied by people from
// some console, both variants are common, and the error for "pasted the wrong variant"
// (illegal base64 data) does not point at the real cause at all.
func decodeKey(s string) ([]byte, error) {
	var key []byte
	var err error
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding,
		base64.RawStdEncoding, base64.RawURLEncoding} {
		key, err = enc.DecodeString(s)
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("%w: 不是合法的 base64", ErrBadKey)
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("%w: 解出 %d 字节，AES-256 要 %d 字节",
			ErrBadKey, len(key), KeySize)
	}
	return key, nil
}

// Enabled tells whether this Box really encrypts.
//
// Callers use it to state the current mode in the startup log. **This is the only way ops can
// know "is encryption actually on"**, because the two modes work exactly the same (keys work
// normally either way) and only the shape of the stored data differs.
func (b *Box) Enabled() bool { return b != nil && b.aead != nil }

// Seal encrypts plaintext into a string that can be stored.
//
// In degrade mode (no key) it returns the plaintext as is, so callers do not need two write paths;
// "encrypt or not" is decided only here, once.
//
// An empty string is returned as is: it means "no key at this level" (inherited workspace rows are
// empty strings), and encrypting an empty string would only make "not configured" and "configured
// as empty" look different in the database.
func (b *Box) Seal(plaintext string) (string, error) {
	if plaintext == "" || !b.Enabled() {
		return plaintext, nil
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("cryptobox: 取随机 nonce 失败: %w", err)
	}
	// The nonce is prepended to the ciphertext: decryption needs it, and it does not need to be
	// secret.
	sealed := b.aead.Seal(nil, nonce, []byte(plaintext), nil)
	return Prefix + base64.StdEncoding.EncodeToString(append(nonce, sealed...)), nil
}

// Open turns the stored column back into plaintext.
//
// # No prefix means existing plaintext, returned as is
//
// This is the whole mechanism for existing data. **The check is the prefix, not "try to decrypt,
// treat failure as plaintext"**: the latter would also treat "the key is misconfigured" as
// plaintext, so a ciphertext would be sent to Ark as is, with an error saying "API key invalid",
// and only at the very end would you find the key was wrong. Checking the prefix keeps the two
// apart.
//
// ⚠️ A real Ark key can never start with "v1:" (they look like `ark-...`), so the prefix cannot
// collide with plaintext. Even if it did, it would only make one malformed key report
// ErrCorrupted, not fail silently.
func (b *Box) Open(stored string) (string, error) {
	if !strings.HasPrefix(stored, Prefix) {
		// Existing plaintext (including what degrade mode wrote).
		return stored, nil
	}
	if !b.Enabled() {
		// The database has ciphertext but this process has no key. **It must return an error, not
		// the ciphertext**: returned ciphertext would be sent to Ark as a key, the 401 error would
		// point at "invalid credential", while the real cause is "this instance is missing the
		// key". With multiple instances it is even more hidden: instances that have the key work
		// fine.
		return "", fmt.Errorf("%w: 库里是密文，但本进程没有配加密密钥", ErrCorrupted)
	}
	blob, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, Prefix))
	if err != nil {
		return "", fmt.Errorf("%w: base64 解不开: %v", ErrCorrupted, err)
	}
	ns := b.aead.NonceSize()
	if len(blob) < ns {
		return "", fmt.Errorf("%w: 长度 %d 不足一个 nonce", ErrCorrupted, len(blob))
	}
	plain, err := b.aead.Open(nil, blob[:ns], blob[ns:], nil)
	if err != nil {
		// A GCM check failure covers both "wrong key" and "data was changed", and the platform
		// cannot tell them apart, so it does not guess here either; but it must name the most
		// likely cause, otherwise nobody will think of checking the key.
		return "", fmt.Errorf("%w: 校验失败（最可能是加密密钥与写入时不一致）", ErrCorrupted)
	}
	return string(plain), nil
}

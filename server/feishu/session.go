// Generation of registration session IDs.
//
// # Why it must be a high-entropy random string
//
// The sessionID appears in frontend URLs and is submitted back again and again. The polling endpoint already
// authenticates before looking up the session (see onboarding.go), but defense in depth requires the ID itself not to
// be enumerable: if some future entry point forgets to authenticate, a guessable ID directly means someone else's registration flow can be hijacked.
//
// So we use crypto/rand, not math/rand, and not a timestamp plus counter.

package feishu

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// sessionIDBytes is the number of random bytes in a session ID. 32 bytes is 256 bits, far beyond enumerable range.
const SessionIDBytes = 32

// base58Alphabet drops the four easily confused characters 0OIl.
//
// We use base58 instead of hex to keep it shorter (32 bytes in hex is 64 characters), and since it appears in a URL,
// shorter is better; confusable characters are dropped because users may copy the link by hand or read it aloud.
const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// newSessionID generates a high-entropy session ID.
func newSessionID() (string, error) {
	buf := make([]byte, SessionIDBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成会话标识失败: %w", err)
	}
	// Map byte by byte onto the alphabet. This is not strict base58 encoding (no big-number base conversion), but the only goal is
	// "an unguessable ID"; each byte still contributes log2(58)≈5.86 bits, so 32 bytes give about 187 bits of entropy, which is enough.
	out := make([]byte, len(buf))
	for i, b := range buf {
		out[i] = base58Alphabet[int(b)%len(base58Alphabet)]
	}
	return string(out), nil
}

// newAppID generates the app ID on the AMA side.
func newAppID() (string, error) {
	suffix, err := newSessionID()
	if err != nil {
		return "", err
	}
	return "fsa_" + suffix[:16], nil
}

// secretDigest is a digest of the secret, used to tell whether a cached check result still matches the current secret.
//
// Store only the digest, not the plaintext: cache keys can be seen in logs and pprof, and a plaintext secret must not show up there.
func secretDigest(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

package core

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// TerminalMatchesRun checks that a one-time terminal ticket belongs to the run
// being opened. A valid ticket for another run must never widen its scope.
func TerminalMatchesRun(ticketRunID, requestedRunID string) bool {
	return ticketRunID != "" && ticketRunID == requestedRunID
}

// NewTerminalID returns a random identifier suitable for cross-instance routing.
// Process-local sequences must not be used: two instances could route input to
// the same terminal. The time fallback avoids failing terminal creation if the
// system random source is unavailable.
func NewTerminalID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("term_%d", time.Now().UnixNano())
	}
	return "term_" + hex.EncodeToString(b[:])
}

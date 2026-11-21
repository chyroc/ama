package core

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// NewAgentSyncTaskID returns a random ID for a persistent sync task. Process-local
// counters are unsafe because an insert collision looks like an existing task and
// silently drops the new save. A timestamp fallback keeps enqueue available if the
// system random source fails.
func NewAgentSyncTaskID() string {
	var b [9]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("astask_%d", time.Now().UnixNano())
	}
	return "astask_" + hex.EncodeToString(b[:])
}

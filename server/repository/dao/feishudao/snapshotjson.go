// Encoding and decoding for the jsonb column.
//
// Only the PG implementation needs these two (MemStore holds *feishu.ProgressSnapshot directly and does
// not serialize), so they moved here with the implementation instead of staying in feishu as unused unexported functions.
package feishudao

import (
	"encoding/json"

	"github.com/chyroc/ama/server/feishu"
)

// snapshotJSON encodes a snapshot into the jsonb column.
func snapshotJSON(s *feishu.ProgressSnapshot) ([]byte, error) {
	if s == nil {
		return nil, nil
	}
	return json.Marshal(s)
}

// snapshotFromJSON decodes a snapshot from the jsonb column.
func snapshotFromJSON(raw []byte) (*feishu.ProgressSnapshot, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var s feishu.ProgressSnapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

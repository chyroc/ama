package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// FingerprintArkSpecV1WithoutBaseAgent preserves the v1 field order used by
// configurations that predate the public BaseAgent field. It still includes tools
// and custom tool declarations, so toggling either one triggers synchronization.
func FingerprintArkSpecV1WithoutBaseAgent(spec ArkAgentSpec, version string) string {
	if version == "" {
		version = defaultArkSpecFingerprintVersion
	}
	h := sha256.New()
	write := func(parts ...string) {
		for _, part := range parts {
			_, _ = fmt.Fprintf(h, "%d:%s", len(part), part)
		}
	}
	write(spec.Name, spec.Description, spec.System, spec.Model, spec.Speed)
	write(spec.Tools...)
	for _, customTool := range spec.CustomTools {
		write(customTool.Name, customTool.Description)
		keys := make([]string, 0, len(customTool.InputSchema))
		for key := range customTool.InputSchema {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			blob, err := json.Marshal(customTool.InputSchema[key])
			if err != nil {
				blob = nil
			}
			write(key, string(blob))
		}
		write(customTool.Required...)
	}
	return version + hex.EncodeToString(h.Sum(nil))
}

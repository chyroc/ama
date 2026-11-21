package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/chyroc/ama/server/shared/infra/ma"
)

const defaultArkSpecFingerprintVersion = "v1:"

// FingerprintArkSpec computes the versioned fingerprint of an assembled Ark Agent
// configuration. Tools and custom tools are included; skill references are tracked
// separately. Custom schema keys are sorted so equivalent configurations are stable.
func FingerprintArkSpec(
	spec ArkAgentSpec, version string, extraFields []string, customTools []ma.CustomToolSpec,
) string {
	if version == "" {
		version = defaultArkSpecFingerprintVersion
	}
	h := sha256.New()
	write := func(parts ...string) {
		for _, part := range parts {
			_, _ = fmt.Fprintf(h, "%d:%s", len(part), part)
		}
	}
	write(spec.Name, spec.Description, spec.System, spec.Model, spec.Speed, spec.BaseAgent)
	write(extraFields...)
	write(spec.Tools...)
	for _, customTool := range customTools {
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

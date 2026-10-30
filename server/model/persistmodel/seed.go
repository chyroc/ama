// Loading the seed data.
//
// seed.json is exported mechanically from the frontend's web/src/data/seed.ts by
// scripts/export-seed/export.sh. **Do not edit it by hand**: the next export overwrites it. To
// change the seed content, edit seed.ts and run the export script again.
//
// Why export mechanically instead of copying into Go literals by hand: seed.ts is 39 KB and only
// imports types, so it is pure data literals; copying by hand is tedious and bound to have
// mistakes. The exported JSON also serves as a **byte-faithful golden file** (see the tests in
// model/apimodel): JSON.stringify already drops keys whose value is undefined, which is exactly the
// shape Go should have after omitempty.
//
// The most complex run in the seed (run_1, 38 events) covers multi-thread collaboration, built-in
// tools, MCP tools, tools that need user confirmation, error retries, context compaction, and
// outcome evaluation; the classification/pairing/filtering logic of the run history view can only
// be verified with it.
//
// # Why in this package and not core
//
// Both Store implementations load the seed: MemStore uses it directly, and PG's Reset uses it too
// (`insertSeed`). Keeping it in core would stop the PG implementation from moving out.
//
// **seed.json must be in the same directory as this file**: go:embed cannot cross parent
// directories (a language limit, the same reason as the DDL in repository/schema). So it moved
// here together with this file, and the output path of scripts/export-seed/export.sh changed too.

package persistmodel

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/chyroc/ama/server/model/apimodel"
)

//go:embed seed.json
var seedJSON []byte

// Seed is the parsed form of the seed data.
//
// It deliberately **has no** feishuApps: Feishu apps belong to server/feishu (the device-code
// registration path), whose data is managed by that package; core does not touch it. So the
// feishuApps section of the seed is ignored.
//
// It is only "what is in the seed", not "MemStore's internal state"; the latter has a dozen more
// fields (skill attachments, issues, leases...), none of which are in the seed. The two are
// separate, and MemStore fills in its own.
type Seed struct {
	Workspaces []apimodel.Workspace
	Agents     []apimodel.Agent
	Skills     []apimodel.Skill
	Runs       []apimodel.Run
	Events     map[string][]apimodel.RunEvent

	// ArkSecrets is always empty: seed.json has no such section.
	//
	// **The field is still kept**: on the PG side, insertSeed iterates it and stores each entry
	// with sealKey. Without that step, if a credential were ever really added to the seed it would
	// silently not be stored (or be stored as plaintext, and the read path, judging by prefix,
	// would treat it as existing plaintext; it would work, but "encrypted at rest" would have a
	// gap).
	ArkSecrets map[string]apimodel.ArkSecret
}

// seedShape is the top-level structure of seed.json.
type seedShape struct {
	Workspaces []apimodel.Workspace           `json:"workspaces"`
	Agents     []apimodel.Agent               `json:"agents"`
	Skills     []apimodel.Skill               `json:"skills"`
	Runs       []apimodel.Run                 `json:"runs"`
	Events     map[string][]apimodel.RunEvent `json:"events"`
}

// LoadSeed parses the embedded seed into fresh data.
//
// Each call parses again and returns data that shares nothing; Reset relies on this so repeated
// resets never leak into each other.
func LoadSeed() (Seed, error) {
	var seed seedShape
	if err := json.Unmarshal(seedJSON, &seed); err != nil {
		return Seed{}, fmt.Errorf("storemodel: 解析种子数据失败: %w", err)
	}

	out := Seed{
		Workspaces: []apimodel.Workspace{},
		Agents:     []apimodel.Agent{},
		Skills:     []apimodel.Skill{},
		Runs:       []apimodel.Run{},
		Events:     map[string][]apimodel.RunEvent{},
		ArkSecrets: map[string]apimodel.ArkSecret{},
	}
	// Deep-copy each entry: using the decoded slices directly would also work, but going through
	// clone guarantees all slice fields are non-nil, consistent with the other read paths' invariant
	// (a nil slice is encoded as null, and the frontend blows up).
	for _, ws := range seed.Workspaces {
		out.Workspaces = append(out.Workspaces, CloneWorkspace(ws))
	}
	for _, a := range seed.Agents {
		out.Agents = append(out.Agents, CloneAgent(a))
	}
	for _, sk := range seed.Skills {
		out.Skills = append(out.Skills, CloneSkill(sk))
	}
	for _, r := range seed.Runs {
		out.Runs = append(out.Runs, CloneRun(r))
	}
	for runID, events := range seed.Events {
		cloned := make([]apimodel.RunEvent, 0, len(events))
		for _, e := range events {
			cloned = append(cloned, CloneEvent(e))
		}
		out.Events[runID] = cloned
	}
	return out, nil
}

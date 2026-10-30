// Seed data adapter on the core side.
//
// **The real loading lives in model/persistmodel** (moved there together with seed.json; see the file
// header there: both Store implementations load the seed, and keeping it in core would block moving the PG implementation out).
//
// This file does one thing: spread persistmodel.Seed into MemStore's internal state memData.
// They are not the same thing. Seed is "what the seed contains" (5 domains); memData is all of MemStore's
// state (twenty-plus fields; the rest have nothing in the seed and keep the empty values of emptyData).

package coremem

import "github.com/chyroc/ama/server/model/persistmodel"

// loadSeed builds a fresh in-memory state.
//
// Each call parses again (guaranteed by persistmodel.LoadSeed) and returns data shared with nothing else.
// Reset relies on this so repeated resets do not leak into each other.
func loadSeed() (memData, error) {
	seed, err := persistmodel.LoadSeed()
	if err != nil {
		return memData{}, err
	}

	data := emptyData()
	data.workspaces = seed.Workspaces
	data.agents = seed.Agents
	data.skills = seed.Skills
	data.runs = seed.Runs
	for runID, events := range seed.Events {
		data.events[runID] = events
	}
	for ws, sec := range seed.ArkSecrets {
		data.arkSecrets[ws] = sec
	}
	return data, nil
}

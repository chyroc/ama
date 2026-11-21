package core

import "github.com/chyroc/ama/server/model/apimodel"

// ArkConsoleRegion returns the configured region, defaulting legacy credentials
// to the standard region. Missing credentials return an empty region so the caller
// can omit the console entry instead of linking to an unrelated project.
func ArkConsoleRegion(region apimodel.ArkRegion) apimodel.ArkRegion {
	if region == "" {
		return apimodel.RegionCNBeijing
	}
	return region
}

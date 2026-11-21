package apimodel

// MarketPackage is provider-neutral metadata for a discoverable skill package.
// Identifier is the stable reference shared with SkillOrigin; clients and credentials stay outside it.
type MarketPackage struct {
	Identifier    string `json:"identifier"`
	Namespace     string `json:"namespace"`
	Name          string `json:"name"`
	DisplayTitle  string `json:"displayTitle"`
	Description   string `json:"description"`
	Version       string `json:"version"`
	CreatedBy     string `json:"createdBy"`
	DownloadTotal int    `json:"downloadTotal"`
	Private       bool   `json:"private"`
	Installed     bool   `json:"installed"`
}

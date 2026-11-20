package apimodel

// SkillOrigin describes where an externally installed skill came from.
// Provider-specific identifiers stay opaque strings so downstreams can use their own
// marketplaces or repositories without changing the common Skill type.
type SkillOrigin struct {
	Kind          string `json:"kind"`
	Identifier    string `json:"identifier"`
	Namespace     string `json:"namespace"`
	Name          string `json:"name"`
	Repo          string `json:"repo,omitempty"`
	Ref           string `json:"ref,omitempty"`
	Path          string `json:"path,omitempty"`
	Version       string `json:"version"`
	Cksum         string `json:"cksum"`
	AutoUpdate    bool   `json:"autoUpdate"`
	InstalledBy   string `json:"installedBy"`
	LastCheckedAt string `json:"lastCheckedAt,omitempty"`
	LastError     string `json:"lastError,omitempty"`
}

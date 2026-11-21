package apimodel

// SourceScanResult identifies the upstream revision that was scanned.
type SourceScanResult struct {
	Namespace string         `json:"namespace"`
	Repo      string         `json:"repo"`
	Ref       string         `json:"ref"`
	Commit    string         `json:"commit,omitempty"`
	Skills    []ScannedSkill `json:"skills"`
}

// ScannedSkill reports each item independently so one invalid directory does not hide the others.
type ScannedSkill struct {
	Dir          string             `json:"dir"`
	Name         string             `json:"name"`
	DisplayTitle string             `json:"displayTitle"`
	Description  string             `json:"description"`
	Files        int                `json:"files"`
	Bytes        int                `json:"bytes"`
	Status       ScannedSkillStatus `json:"status"`
	Reason       string             `json:"reason,omitempty"`
}

type ScannedSkillStatus string

const (
	ScanOK        ScannedSkillStatus = "ok"
	ScanInstalled ScannedSkillStatus = "installed"
	ScanInvalid   ScannedSkillStatus = "invalid"
	ScanTooBig    ScannedSkillStatus = "too_big"
	ScanFailed    ScannedSkillStatus = "failed"
)

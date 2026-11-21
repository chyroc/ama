package apimodel

// MigrateBlobReport is self-describing: zero migrated rows cannot be confused with dry-run.
type MigrateBlobReport struct {
	Apply         bool                     `json:"apply"`
	Limit         int                      `json:"limit"`
	Bucket        string                   `json:"bucket"`
	Tables        []MigrateBlobTableReport `json:"tables"`
	Items         []MigrateBlobItem        `json:"items"`
	TotalPending  int                      `json:"totalPending"`
	TotalMigrated int                      `json:"totalMigrated"`
	TotalFailed   int                      `json:"totalFailed"`
}

type MigrateBlobTableReport struct {
	Table    string `json:"table"`
	Pending  int    `json:"pending"`
	Migrated int    `json:"migrated"`
	Failed   int    `json:"failed"`
}

// MigrateBlobItem exposes every failure instead of hiding it inside a successful total.
type MigrateBlobItem struct {
	Table  string `json:"table"`
	Key    string `json:"key"`
	OK     bool   `json:"ok"`
	Bytes  int    `json:"bytes"`
	FileID string `json:"fileId,omitempty"`
	Error  string `json:"error,omitempty"`
}

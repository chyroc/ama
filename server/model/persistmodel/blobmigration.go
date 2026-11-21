package persistmodel

// BlobMigrationTable is an enumerated storage target, never a caller-provided table name.
// Implementations switch on it instead of interpolating a string into SQL.
type BlobMigrationTable string

const (
	BlobTableIssueAttachments      BlobMigrationTable = "issue_attachments"
	BlobTableSuggestionAttachments BlobMigrationTable = "suggestion_attachments"
	// BlobTableSkillFiles supports downstream stores that still have this legacy shape.
	// It is not selected automatically; the caller supplies only tables that still exist.
	BlobTableSkillFiles BlobMigrationTable = "skill_files"
)

// BlobRowKey supports both single-column and composite primary keys.
type BlobRowKey struct {
	Primary   string
	Secondary string
}

func (k BlobRowKey) String() string {
	if k.Secondary == "" {
		return k.Primary
	}
	return k.Primary + "/" + k.Secondary
}

// BlobPayload carries one legacy row. Missing author/time values are filled by the caller,
// not fabricated in SQL, so those defaults can be tested without a database.
type BlobPayload struct {
	Content     []byte
	Filename    string
	ContentType string
	Author      string
	CreatedAt   string
}

// Row type of the user_files table: account-level personal files (/api/files).
//
// Same rule as the rest of this package: it is stored and appears in Store signatures, but never
// goes into HTTP responses as is (the external shape is apimodel.UserFile). For the full design
// of owner isolation and the state machine, see the header of repository/schema/user_files.sql.
package persistmodel

// The two states and two upload modes of a user file. No enum (consistent across the repo: literals live in code).
const (
	UserFileStatusUploading = "uploading"
	UserFileStatusCompleted = "completed"

	UserFileModeSimple    = "simple"
	UserFileModeMultipart = "multipart"
)

// UserFile is one row of the user_files table.
type UserFile struct {
	// ID is a random id with the uf_ prefix, used in URLs.
	ID string
	// OwnerEmail is the owner's lowercase email; every read/write SQL includes it; see user_files.sql.
	OwnerEmail string
	// Bucket + ObjectKey locate the bytes in object storage. The bucket name is stored separately (old records stay readable after a bucket change).
	Bucket    string
	ObjectKey string

	Filename    string
	ContentType string
	// Size is the byte count after completion; 0 for uploading rows, and on complete it is the value read back via object storage HeadObject.
	Size int64

	// Status is one of UserFileStatus*; UploadMode is one of UserFileMode*.
	Status     string
	UploadMode string
	// UploadID is the multipart upload handle; empty for simple mode.
	UploadID string

	// CreatedAt is a fixed three-digit-millisecond ISO string (part of the contract, sorted as a string).
	CreatedAt string
}

// HTTP contract entities for account-level personal files (/api/files).
//
// Kept apart from persistmodel.UserFile: the database has internal fields like bucket/object_key/upload_id
// ("where the bytes are, what the resume credential is"), and the response returns none of them: they could be used to bypass owner
// isolation and access object storage directly, and they do not matter to callers.
package apimodel

// UserFile is the public shape of one personal file.
//
// OwnerEmail is not included (same reason as apimodel.Host): the list is always the caller's own files,
// so returning the owner email is pointless and adds one more leak surface.
type UserFile struct {
	ID string `json:"id"`
	// Filename is the original file name given at upload, for display only.
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	// Size is the byte count; 0 while uploading.
	Size int64 `json:"size"`
	// Status is uploading / completed.
	Status string `json:"status"`
	// UploadMode is simple / multipart; after starting an upload the client uses it to decide which set of endpoints to call.
	UploadMode string `json:"uploadMode"`
	// PartSize is the part size (bytes) required by multipart mode; 0 in simple mode.
	// Numeric fields have no omitempty (0 is a valid value; see jsontag hard rule two).
	PartSize  int64  `json:"partSize"`
	CreatedAt string `json:"createdAt"`

	// DownloadURL is a time-limited signed object storage link, given only by the "file detail" endpoint;
	// the list does not include it (signing dozens of links per list call is pointless, and would spread links across the list response).
	DownloadURL string `json:"downloadUrl,omitempty"`
	// ExpiresInSeconds is the validity (seconds) of the link above; 0 when there is no link.
	ExpiresInSeconds int `json:"expiresInSeconds"`
}

// StartUserFileUpload is the request body of POST /api/files.
type StartUserFileUpload struct {
	// Filename is required, stored for display only, not part of the object storage key.
	Filename string `json:"filename"`
	// ContentType may be empty.
	ContentType string `json:"contentType"`
	// Size is the total byte count the caller expects; may be empty:
	// if given and ≤ PartSize, use simple single upload; otherwise multipart.
	// The server does not pre-allocate anything based on it; on complete, the size read back from object storage wins.
	Size int64 `json:"size"`
}

// UserFilePart is the response after uploading one part.
//
// No etag returned: when completing the merge, the server queries the part list from object storage itself, so the client need not keep it.
type UserFilePart struct {
	PartNumber int `json:"partNumber"`
}

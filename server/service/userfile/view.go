// Owner normalization, filename validation, and the row -> API type mapping.
package userfile

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// normalizeOwner turns the login identity taken from the HTTP layer into the owner string used
// in object keys and SQL.
//
// It must be lowercased and trimmed: that is how the auth side normalizes it when writing to the
// database (auth/store.go). If we did not do it again here, the same person would get two
// directories in object storage, `user-files/A@x.com/` and `user-files/a@x.com/`, and SQL would
// not find their own rows. An empty string (no identity) is rejected outright. On the normal
// path the Protect middleware guarantees it is non-empty; this is defense in depth.
func normalizeOwner(owner string) (string, error) {
	o := strings.ToLower(strings.TrimSpace(owner))
	if o == "" {
		return "", fmt.Errorf("%w: 缺少登录身份", ErrAccessDenied)
	}
	return o, nil
}

// validateFilename validates the filename given at upload time.
//
// It is **only used for display** (it does not go into the object key), so this check guards
// against dirty data, not path traversal. Path traversal is ruled out at the root because the
// filename never goes into the key. What we still block: empty names, names that are too long,
// and control characters (newlines/tabs would break the list and Content-Disposition).
func validateFilename(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("%w: 文件名不能为空", ErrInvalidArgument)
	}
	if len(name) > maxFilenameBytes {
		return "", fmt.Errorf("%w: 文件名不能超过 %d 字节", ErrInvalidArgument, maxFilenameBytes)
	}
	if !utf8.ValidString(name) {
		return "", fmt.Errorf("%w: 文件名必须是合法 UTF-8", ErrInvalidArgument)
	}
	if strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("%w: 文件名不能包含控制字符", ErrInvalidArgument)
	}
	return name, nil
}

// objectKey builds the object key: user-files/<lowercase email>/<ufid>.
//
// owner is normalized before the call; id is a random string from this package's newID. Email
// characters (letters, digits, @._-) are all valid object key characters.
func objectKey(owner, id string) string {
	return keyPrefix + "/" + owner + "/" + id
}

// view maps a stored row to the external shape.
func view(row persistmodel.UserFile) apimodel.UserFile {
	out := apimodel.UserFile{
		ID:          row.ID,
		Filename:    row.Filename,
		ContentType: row.ContentType,
		Size:        row.Size,
		Status:      row.Status,
		UploadMode:  row.UploadMode,
		CreatedAt:   row.CreatedAt,
	}
	if row.Status == persistmodel.UserFileStatusUploading &&
		row.UploadMode == persistmodel.UserFileModeMultipart {
		// Tell the multipart uploader how large each part should be.
		out.PartSize = PartSize
	}
	return out
}

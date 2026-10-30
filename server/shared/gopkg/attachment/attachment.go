// Package attachment handles attachment type detection, size limits and response headers.
// Uploaded files are **untrusted input**.
//
// # What this file defends against
//
// Attachments are served straight to the browser through GET /api/issue-attachments/{id}, and that
// URL is put into <img src>. In other words: **a user can make someone else's browser load bytes he
// uploaded**. This is a real attack surface, and three layers of defense each block one kind of
// failure:
//
//  1. type allowlist + server-side sniffing  ->  blocks "HTML uploaded disguised as png"
//  2. X-Content-Type-Options                 ->  blocks the browser guessing the type on its own (sniffing bypass)
//  3. Content-Disposition                    ->  non-images are always downloaded, never rendered in the browser
//
// The first layer alone is not enough: type detection can always be bypassed (building a file that
// looks like both an image and HTML has been done before), while the other two layers apply
// unconditionally and do not depend on the detection being right.
//
// # Why SVG is not accepted
//
// SVG is **executable**: it can embed <script>, and it can stuff HTML in with <foreignObject>.
// An image/svg+xml response is rendered by the browser as a vector image even with nosniff, scripts
// still run, and they run in our origin: they can read cookies (HttpOnly protects the session token,
// but not other things) and can call all of our APIs.
//
// The only safe way to accept it is to re-parse it and strip script nodes, which means pulling in an
// XML library and maintaining our own list of dangerous nodes, with no alarm when it goes wrong. The
// benefit (a user occasionally wants to upload a vector image) is far out of proportion to the cost,
// so it is not accepted.
//
// Mostly pure functions; all edge cases have tests (attachment_test.go).
//
// # Why a separate package (this is the whole reason it exists)
//
// Its name used to include issue, but it is **not owned by issue alone**: attachments in both the
// issue and suggestion domains use the same detection and response headers. In practice 15
// references come from non-issue files:
// `core/suggestion.go`, `core/handler_suggestion.go`, `core/devblob.go`.
// core is being split into packages by resource domain. If this were not pulled out first, moving
// issue out would take these constants with it, and suggestion would stop compiling. This is the
// same kind of move as shared/infra/filebytes and shared/gopkg/humanize.
//
// ⚠️ The package name already says "attachment", so names inside do not repeat it:
// `attachment.MaxBytes`, not `attachment.MaxAttachmentBytes`.
package attachment

import (
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/shared/gopkg/humanize"
)

const (
	// MaxBytes is the size limit of a single attachment.
	//
	// 5 MiB is set for "one screenshot": a 4K PNG screenshot is usually 1-3 MB, a phone photo 2-5 MB.
	// Anything bigger (videos, datasets) should not go through issue attachments: both upload and
	// download pass entirely through amad's memory (attachments are not chunked). For large files use
	// personal files (/api/files, chunked upload).
	MaxBytes = 5 << 20

	// MaxUploadBytes is the limit on the upload request body, **a bit larger** than the per-file limit.
	//
	// The extra 1 MiB is room for the multipart wrapping (boundary, each part's headers, other
	// fields in the form). If both were the same value, a file of exactly 5 MiB would be stopped by
	// the request body gate because of the wrapping overhead: the error says "over 5 MiB" while the
	// file is exactly 5 MiB, the user cannot make sense of it, and can only trim content bit by bit.
	MaxUploadBytes = 6 << 20

	// MaxCount is the attachment count limit for a single issue.
	//
	// **It is required**: without a limit, one issue could carry any number of attachments, and the
	// detail page has to list them all. 20 is enough to describe any problem; more than that means it
	// should be something else (a document, a repository).
	MaxCount = 20

	// FieldName is the name of the file field in the upload form.
	//
	// It has the same value "file" as the skill upload one (uploadFieldName), but **each defines its
	// own on purpose**: the two routes are two separate contracts, and if one side changes someday
	// (say, to support multiple files), a shared constant would change the other side too without
	// anyone noticing.
	FieldName = "file"
)

// allowedTypes maps allowed MIME types -> the extensions accepted for that type.
//
// **An allowlist, not a blocklist.** A blocklist ("no .html .svg .js") always misses something:
// .htm / .xhtml / .shtml / .svgz / .mhtml... you can never list them all, and missing one causes no
// error. An allowlist is the opposite: anything not on the list is rejected, and new formats must be
// added explicitly. This is the same approach as the collection boundary in core/feedback.go.
//
// **The values are extension sets, used for cross-checking**: if sniffing says image/png but the
// extension is .pdf, the two signals conflict, so reject. Sniffing alone is not enough: it only
// looks at the first 512 bytes, and a file whose first 512 bytes look like PNG followed by
// something else can be built.
var allowedTypes = map[string][]string{
	"image/png":        {".png"},
	"image/jpeg":       {".jpg", ".jpeg"},
	"image/gif":        {".gif"},
	"image/webp":       {".webp"},
	"application/pdf":  {".pdf"},
	"text/plain":       {".txt", ".md", ".log", ".csv"},
	"text/csv":         {".csv"},
	"application/json": {".json"},
	"application/zip":  {".zip"},
}

// inlineTypes are the types that may be shown inline: **bitmaps only**.
//
// This list directly decides Content-Disposition: on the list -> inline (so <img> can display it),
// not on the list -> always attachment (the browser downloads instead of rendering).
//
// ⚠️ Think before adding a type here: inline means **the browser renders it**.
// Adding any format that can run scripts (html / svg / xml) creates a self-service XSS entry point.
var inlineTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

// ErrTooLarge is the error for an attachment over the per-file limit.
var ErrTooLarge = fmt.Errorf(
	"%w: 附件超过 %s 上限", persistmodel.ErrInvalidArgument, humanize.HumanBytes(MaxBytes))

// ErrTooMany is the error when the attachment count is over the limit.
var ErrTooMany = fmt.Errorf(
	"%w: 一个问题最多上传 %d 个附件", persistmodel.ErrInvalidArgument, MaxCount)

// ErrEmpty is the error for uploading an empty file.
//
// It is a separate error instead of being merged into "unsupported type": an empty file sniffs as
// text/plain, and "unsupported type" would send the user to check the file format, when the real
// cause is that the file is empty.
var ErrEmpty = fmt.Errorf("%w: 上传的文件是空的", persistmodel.ErrInvalidArgument)

// DetectType determines the real type of an attachment and cross-checks it with the extension.
//
// It returns the Content-Type **decided by the server**; both storage and responses use it, and
// **the one the client declared in the multipart is never used**. A client saying image/png while
// the content is HTML is the most classic attack; trusting it means having no type check at all.
//
// # Why both signals are needed
//
//	sniffing only    ->  sniffing reads only the first 512 bytes. "First 512 bytes look like PNG,
//	                     followed by HTML" can be built, and the browser may not judge by the same rules.
//	extension only   ->  the extension is part of the file name, a string fully controlled by the user.
//	                     Renaming evil.html to evil.png gets around it.
//
// Both must match the same allowlist entry to pass.
func DetectType(filename string, content []byte) (string, error) {
	if len(content) == 0 {
		return "", ErrEmpty
	}

	// http.DetectContentType implements the WHATWG sniffing algorithm, the same rules browsers use.
	// That matters: what we need to decide is exactly "what the browser will treat it as".
	// It only looks at the first 512 bytes and always returns a value (falling back to
	// application/octet-stream).
	sniffed := normalizeMediaType(http.DetectContentType(content))

	ext := strings.ToLower(path.Ext(filename))
	if ext == "" {
		return "", fmt.Errorf("%w: 文件名缺少扩展名，无法判断类型", persistmodel.ErrInvalidArgument)
	}

	// When sniffing **has a clear opinion** (it recognized a specific format), the extension must match it.
	//
	// This branch blocks "the content is A, the name says B", the typical shape of an attack.
	// When the two signals conflict, reject outright instead of "picking one": picking either is a
	// guess, and one wrong guess is an XSS.
	if exts, ok := allowedTypes[sniffed]; ok && !sniffHasNoOpinion(sniffed) {
		for _, e := range exts {
			if e == ext {
				return sniffed, nil
			}
		}
		return "", fmt.Errorf(
			"%w: 文件内容看起来是 %s，与扩展名 %s 不符", persistmodel.ErrInvalidArgument, sniffed, ext)
	}

	// Sniffing **has no opinion** (text/plain or octet-stream), or does not recognize it at all:
	// the extension decides, but the result must still come from the allowlist.
	//
	// This is not "bypassing the check"; it is a known limit of the sniffing algorithm: .md / .csv /
	// .json are all plain text, and sniffing always returns text/plain for them (it does not parse
	// structure); and when a .zip does not start with the standard PK signature (an empty zip, a
	// self-extracting archive) it returns octet-stream.
	// If these were rejected as "does not match .txt", uploading a .json would be told "the content
	// looks like text/plain, which does not match the extension .json", which users cannot make sense of.
	if resolved, ok := resolveByExtension(sniffed, ext); ok {
		return resolved, nil
	}
	return "", unsupportedTypeErr(ext)
}

// sniffHasNoOpinion reports whether the sniffing result means "I don't know what this is".
//
// http.DetectContentType always returns a value, but two of them are fallbacks rather than verdicts:
//
//	text/plain                 the content is printable text, but **the format is unknown**
//	application/octet-stream   not recognized at all
//
// These two cannot be used as "detected as .txt" or "detected as binary" for conflict checks, or
// every .md and .json would be judged "content does not match extension".
func sniffHasNoOpinion(sniffed string) bool {
	return sniffed == "text/plain" || sniffed == "application/octet-stream"
}

// resolveByExtension handles the few plain-text/archive formats that "sniffing cannot recognize but
// whose extension can be trusted".
//
// It only lets a file through when both conditions hold:
//   - the sniffing result is text/plain or application/octet-stream (that is, "sniffing has no
//     opinion", not "sniffing says it is something else"; the latter is a conflict and must be rejected);
//   - the extension belongs to some allowlisted type.
//
// The returned type comes from the allowlist; it is not made up.
func resolveByExtension(sniffed, ext string) (string, bool) {
	if !sniffHasNoOpinion(sniffed) {
		return "", false
	}
	for mediaType, exts := range allowedTypes {
		for _, e := range exts {
			if e != ext {
				continue
			}
			// When sniffing has no opinion, only non-image types are accepted.
			//
			// ⚠️ This blocks "rename arbitrary bytes to .png": such a file sniffs as octet-stream, and
			// if it were let through as image/png here, it would be sent inline to <img> to render,
			// while we know nothing about its content.
			// Images must be recognized by sniffing (real images have fixed magic numbers, sniffing does
			// not get them wrong).
			if strings.HasPrefix(mediaType, "image/") {
				return "", false
			}
			return mediaType, true
		}
	}
	return "", false
}

// unsupportedTypeErr builds a clear "unsupported type" error.
//
// It lists the allowed extensions instead of just saying "unsupported"; the latter makes users try
// them one by one.
func unsupportedTypeErr(ext string) error {
	return fmt.Errorf("%w: 不支持的文件类型 %s，可上传图片（png/jpg/gif/webp）、"+
		"pdf、txt/md/csv/json 与 zip", persistmodel.ErrInvalidArgument, ext)
}

// normalizeMediaType strips the parameter part (`text/plain; charset=utf-8` -> `text/plain`).
//
// This is required: http.DetectContentType always adds charset for text types, while the allowlist
// keys are bare types. Without normalizing, no text attachment would match the allowlist, and the
// symptom is "uploading txt says txt is not supported".
func normalizeMediaType(ct string) string {
	parsed, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(ct))
	}
	return parsed
}

// Disposition builds the value of the Content-Disposition header.
//
// # Two things, neither can be skipped
//
// **One: non-images are always attachment.** This is the last layer of defense in depth: even if
// type detection is bypassed, attachment makes the browser download instead of render, so scripts
// cannot run.
// Images must be inline, or they will not show in <img src> (the browser treats them as downloads).
//
// **Two: the file name must be encoded per RFC 5987.** Writing a non-ASCII (for example Chinese)
// file name straight into filename= garbles it (that field only accepts ASCII), so the
// filename*=UTF-8”<pct-encoded> form is used.
// An ASCII filename= is also kept as a fallback for old clients.
//
// # Response header injection
//
// The file name is user input, and it goes into an HTTP header. A file name with newlines can
// inject extra response headers (classic CRLF injection), and one with quotes can close
// filename="..." early. sanitizeFilename removes these characters before they are put in.
//
// Go's net/http actually rejects header values with newlines (error at write time), but that is
// **the last line of defense at runtime**: relying on it means the attack shows up as a 500 rather
// than "handled correctly", and it is gone if you switch HTTP libraries.
func Disposition(contentType, filename string) string {
	kind := "attachment"
	if inlineTypes[contentType] {
		kind = "inline"
	}
	safe := sanitizeFilename(filename)
	// The filename= copy keeps only printable ASCII characters, so Chinese names degrade to _,
	// but the filename*= copy is exact, and modern browsers prefer it.
	return fmt.Sprintf("%s; filename=%q; filename*=UTF-8''%s",
		kind, asciiFallback(safe), url.PathEscape(safe))
}

// sanitizeFilename removes characters in a file name that would break the response header or path.
//
// It removes four kinds:
//   - newlines and carriage returns: response header injection;
//   - double quotes and backslashes: they close filename="..." early;
//   - path separators: a file name should not contain directories (`../../etc/passwd`);
//   - control characters: escape sequences in terminals and logs.
//
// If nothing is left, use a placeholder name: an empty filename= makes some browsers treat the
// whole header as malformed and ignore it, and then the attachment half is lost too.
func sanitizeFilename(name string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '"' || r == '\\' || r == '/':
			return '_'
		case r < 0x20 || r == 0x7f:
			return -1
		default:
			return r
		}
	}, name)
	cleaned = strings.TrimSpace(cleaned)
	// Take the base to guard against directory traversal once more (Windows-style backslashes were
	// already replaced with _ above).
	cleaned = path.Base(cleaned)
	if cleaned == "" || cleaned == "." || cleaned == ".." {
		return "attachment"
	}
	return cleaned
}

// asciiFallback replaces non-ASCII characters with underscores, for the filename= copy.
func asciiFallback(s string) string {
	out := strings.Map(func(r rune) rune {
		if r > 0x7f {
			return '_'
		}
		return r
	}, s)
	if strings.TrimSpace(out) == "" {
		return "attachment"
	}
	return out
}

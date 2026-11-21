package core

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	gopath "path"
	"strings"
	"time"
)

// RawSandboxFile is a bounded file read ready for safe HTTP delivery.
type RawSandboxFile struct {
	Name        string
	ContentType string
	Inline      bool
	Data        []byte
}

// ServeRawSandboxFile applies the same download and Range rules to any executor. The caller
// resolves the run and its sandbox, then opens the remote file; this package does not import Docker
// or a proprietary sandbox SDK.
func ServeRawSandboxFile(
	w http.ResponseWriter, r *http.Request, file RawSandboxFile, download bool,
) {
	w.Header().Set("Content-Type", file.ContentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; media-src 'self'; img-src 'self'; sandbox")
	disposition := "attachment"
	if file.Inline && !download {
		disposition = "inline"
	}
	name := strings.NewReplacer(`"`, "", "\r", "", "\n", "").Replace(gopath.Base(file.Name))
	if name == "" || name == "." || name == "/" {
		name = "download"
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`%s; filename="%s"`, disposition, name))
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(file.Data))
}

// ReadRawSandboxFile reads at most limit bytes and decides whether the response may be displayed
// inline. Active documents and unknown binary types remain forced downloads.
func ReadRawSandboxFile(
	ctx context.Context, name string, limit int64, download bool,
	open func(context.Context) (io.ReadCloser, error),
) (RawSandboxFile, error) {
	return ReadRawSandboxFileWithHeaders(ctx, name, limit, download, open)
}

// ReadRawSandboxFileWithHeaders also returns Content-Length when the executor reports it.
func ReadRawSandboxFileWithHeaders(
	ctx context.Context, name string, limit int64, download bool,
	open func(context.Context) (io.ReadCloser, error),
) (RawSandboxFile, error) {
	if limit <= 0 {
		limit = sandboxRawFileMaxBytes
	}
	rc, err := open(ctx)
	if err != nil {
		return RawSandboxFile{}, err
	}
	data, readErr := io.ReadAll(io.LimitReader(rc, limit))
	closeErr := rc.Close()
	if readErr != nil {
		return RawSandboxFile{}, readErr
	}
	if closeErr != nil {
		return RawSandboxFile{}, closeErr
	}
	contentType := detectMediaContentType(gopath.Base(name), data)
	return RawSandboxFile{
		Name: gopath.Base(name), ContentType: contentType,
		Inline: !download && isSafeInlineMediaMIME(contentType), Data: data,
	}, nil
}

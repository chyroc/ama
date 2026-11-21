package core

import (
	"fmt"
	gopath "path"
	"strings"

	"github.com/chyroc/ama/server/model/apimodel"
)

const DefaultSandboxListMaxEntries = 500

// CleanSandboxPath normalizes an absolute container path and rejects relative
// input. The cleaned path cannot escape the root and is stable in the response.
func CleanSandboxPath(path string, invalidArgument error) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "/", nil
	}
	if !strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("%w: 路径必须以 / 开头", invalidArgument)
	}
	clean := gopath.Clean(path)
	if !strings.HasPrefix(clean, "/") {
		return "", fmt.Errorf("%w: 路径不合法", invalidArgument)
	}
	return clean, nil
}

// BuildSandboxListing converts entries and marks truncation explicitly. Files is
// always an initialized slice so the HTTP response contains [] rather than null.
func BuildSandboxListing[T any](
	status, path string, entries []T, maxEntries int,
	convert func(T) apimodel.SandboxFile,
) apimodel.SandboxListing {
	if maxEntries <= 0 {
		maxEntries = DefaultSandboxListMaxEntries
	}
	out := apimodel.SandboxListing{Status: status, Path: path, Files: []apimodel.SandboxFile{}}
	if len(entries) > maxEntries {
		entries = entries[:maxEntries]
		out.Truncated = true
	}
	for _, entry := range entries {
		out.Files = append(out.Files, convert(entry))
	}
	return out
}

// IsBinaryContent uses the presence of a NUL byte, matching common VCS tooling.
// Logs with other control characters are not misclassified by a printable-character ratio.
func IsBinaryContent(data []byte) bool {
	for _, b := range data {
		if b == 0 {
			return true
		}
	}
	return false
}

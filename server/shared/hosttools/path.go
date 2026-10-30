package hosttools

import (
	"fmt"
	"path"
	"strconv"
	"strings"
)

// ResolvePath normalizes the path the agent gave into a path relative to the working directory root.
//
// It behaves exactly like shelltools.resolvePath (the long reasoning there is not repeated): a
// relative path is resolved against workdir, and an empty workdir means "/"; **it is not confined to
// workdir**. bash can read any path anyway, so jailing only read/write/edit/glob/grep would make the
// tools inconsistent in what they can do. path is used here (always `/`-separated slash semantics);
// when the real os calls happen, the filepath package accepts `/` on both darwin and linux.
func ResolvePath(workdir, p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("路径不能为空")
	}
	if !path.IsAbs(p) {
		base := workdir
		if base == "" {
			base = "/"
		}
		p = path.Join(base, p)
	}
	return path.Clean(p), nil
}

// plural builds "N xxx", adding s when N>1. Same shape as shelltools.plural.
func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

package skill

import (
	"fmt"
	"strings"

	"github.com/chyroc/ama/server/model/persistmodel"
)

// SplitSourceIdentifier splits skills:<namespace>/<name>. The skills: prefix is optional;
// a caller may also supply namespace/name. Unknown provider identifiers remain opaque to
// the common service and are not rewritten into internal repository identifiers.
func SplitSourceIdentifier(identifier string) (namespace, name string, err error) {
	id := strings.TrimSpace(identifier)
	id = strings.TrimPrefix(id, "skills:")
	id = strings.Trim(id, "/")
	if id == "" {
		return "", "", fmt.Errorf("%w: 请给出技能标识", persistmodel.ErrInvalidArgument)
	}
	index := strings.LastIndex(id, "/")
	if index <= 0 || index == len(id)-1 {
		return "", "", fmt.Errorf("%w: 技能标识应形如 skills:<namespace>/<name>，实际 %q", persistmodel.ErrInvalidArgument, identifier)
	}
	return id[:index], id[index+1:], nil
}

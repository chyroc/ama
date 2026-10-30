// Three small helpers used by core itself.
//
// They used to live in the in-memory section of store.go (that section moved to
// repository/dao/coremem). **They were not moved along because core's business code
// uses them itself**: agent.go / workspace.go and others.
//
// sortSkillFiles is only an alias of the one in storemodel: both Store implementations and
// core need it, and the real one is in a leaf package (see the file header of
// model/persistmodel/clone.go).
//
// ⚠️ **filterSlice is deliberately kept here and not pushed down into a leaf package.** This
// was considered when core was split up, and the decision was not to move it: there are few
// call sites (after the agent domain moved out, only the skill and workspace ones remain),
// and repository/dao/coremem has long kept its own copy for the same job. For ten lines of
// code, each side keeping its own copy is better than adding a package dependency. The rule
// for pushing something down is "shared by several resource domains and big enough", not
// "it is a generic helper".

package core

import "github.com/chyroc/ama/server/model/persistmodel"

// sortSkillFiles sorts by path ascending; the invariant it keeps is described on the storemodel side.
var sortSkillFiles = persistmodel.SortSkillFiles

// contains reports whether a string slice contains a value.
//
// It used to live in core/skill.go and moved to service/skill when the skill domain was
// split out; a copy stays here because core's own tests still use it.
// The two copies cannot drift: it is a linear scan, and the signature fixes its behavior.
func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

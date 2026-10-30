// Renders authz.Capabilities() into a TypeScript module the frontend can import directly.
//
// # Why the output is .ts and not .json
//
// The frontend does not enable `resolveJsonModule` (web/tsconfig.app.json), and this repo's
// existing habit is "data lives in .ts modules": `data/seed.ts`, `prefs/messages.ts`,
// `components/modelIconPaths.ts` all work this way. Following that habit gives one thing for free:
// **the output is typed**, so if the frontend misspells a role name `tsc` fails, instead of
// rendering a blank column.
//
// # Why render on the Go side instead of having the frontend read an API
//
// An API would need a new route, a matrix entry, and a gen-openapi run, and this table **never
// changes between releases** (it is decided by code, not by data). Making it an API turns a
// compile-time constant into a network round trip, and also creates a new question: "what does
// this table show when the user is not logged in / has no permission?"
//
// The cost of committing generated output is that it can go stale (matrix changed but nobody
// regenerated), and a machine guards that cost: `TestGeneratedFileIsUpToDate` compares byte by
// byte. The shape is exactly the same as the openapi setup; see the header of
// `cmd/gen-openapi/main.go` for the reasons.

package authzdoc

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/chyroc/ama/server/authz"
)

// header is the comment block at the top of the output. **It must say "do not edit by hand" and
// "how to regenerate"**: without that, the next person sees an ordinary .ts file and edits it,
// then gets stopped by the consistency test, and by then their edits are wasted.
const header = `/**
 * Role permission table. **This file is generated. Do not edit it by hand.**
 *
 * To regenerate:
 *
 *     cd server && go run ./cmd/gen-authz-capabilities
 *
 * The source of truth is ` + "`server/authz/matrix.go`" + ` (each route declares the level it
 * needs) plus ` + "`server/authz/capability.go`" + ` (groups routes by feature area into
 * one human-readable row).
 *
 * # Why generated instead of hand-written
 *
 * A hand-written permission table **will drift**: the next time someone adds a route, this
 * table silently goes stale. And a stale permission table is worse than none: users trust it,
 * think they can do something, get a 403, then ask an admin for permission, and the admin looks
 * at the same table and also thinks they should be able to. Two people fooled by the same page.
 *
 * Drift is blocked by a machine: ` + "`go test ./shared/gopkg/authzdoc/`" + ` derives it
 * again and compares it with this file byte by byte, and fails on any difference; and "added a
 * route but did not categorize it" already errors on the Go side.
 *
 * ⚠️ **owner and admin have exactly the same permissions.** This is not a missing check mark;
 * see ROLE_NOTE below.
 */
`

// Generate renders the full bytes of the output file.
func Generate() ([]byte, error) {
	caps, err := authz.Capabilities()
	if err != nil {
		return nil, err
	}

	var b bytes.Buffer
	b.WriteString(header)
	b.WriteString("\nimport type { WorkspaceRole } from \"@/types/domain\";\n")

	// Type definitions. Written into the output instead of declared by the frontend: otherwise
	// each side has its own copy, and when a field is renamed the frontend copy does not follow
	// (and TS only says "property does not exist", which does not point at the generator).
	b.WriteString(`
/** One row of the permission table. */
export interface RoleCapability {
  /** Stable id, used by e2e and as the React key. */
  id: string;
  /** Feature area name, shown on screen as is. */
  label: string;
  /** Extra explanation; may be an empty string. */
  detail: string;
  /** The lowest role that can do this. */
  minRole: WorkspaceRole;
  /**
   * All roles that can do this.
   *
   * ⚠️ **owner and admin are always both in or both out**: on the server, levelOf puts them
   * at the same level, and this array is computed from it, not copied.
   */
  roles: readonly WorkspaceRole[];
  /** Action level: read / write / manage. */
  action: "read" | "write" | "manage";
  /**
   * The routes this row covers, like ` + "`PATCH /api/agents/{id}`" + `.
   *
   * **The UI does not show it.** It is here so every row can be traced back to matrix.go,
   * and so that "someone added a route" is visible in this file's diff.
   */
  routes: readonly string[];
}

`)

	// Role column order.
	b.WriteString(`/** Role order in the table header, from most to least permission. Matches authz.RoleOrder() on the server. */
export const ROLE_COLUMNS: readonly WorkspaceRole[] = [`)
	for i, r := range authz.RoleOrder() {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%q", r)
	}
	b.WriteString("];\n")

	// The "owner == admin" sentence.
	//
	// **Kept in the generated output instead of hand-written in the frontend**: it is a statement
	// about levelOf and comes from the same source as the table. If the frontend wrote it by hand,
	// the day owner really gets its own level this sentence would not change; then it would be
	// wrong, and no test would fail.
	b.WriteString(`
/**
 * The sentence that must be shown under the table.
 *
 * ⚠️ **It is not an optional note.** "owner has more permission than admin" is a strong
 * intuition; when those two columns look identical, the user's first thought is "this table is
 * wrong / unfinished". So the text must state that "identical is correct", plus the one extra
 * thing owner has.
 */
export const ROLE_NOTE =
  "拥有者与管理员的权限完全相同；拥有者只多一条保护：不能被移除，且空间至少要有一位。";

/**
 * Second sentence under the table: non-members.
 *
 * Deliberately **not drawn as a column of all crosses**: a non-member is not "the role with
 * the least permission", they **cannot see this workspace at all** (the workspace list does
 * not return workspaces they are not in). A column would suggest they can get in but cannot do
 * anything.
 */
export const NON_MEMBER_NOTE = "不在成员名单里的人无法访问这个空间，也不会在空间列表里看到它。";

/**
 * Third sentence under the table: account-level features.
 *
 * ⚠️ **It must be shown**, otherwise users wonder "why is authorization management not in the
 * table". Those features (authorization management, API Key, UI preferences, issue reports)
 * are account-level: the server sets ActionNone, they have nothing to do with workspace roles,
 * and any logged-in user can use them. They have moved to global settings.
 */
export const ACCOUNT_SCOPE_NOTE =
  "授权管理、API Key、界面偏好等属于账号级设置，与空间角色无关，在「全局设置」里。";

/** All rows, sorted by lowest role from low to high (viewer → member → admin). */
export const ROLE_CAPABILITIES: readonly RoleCapability[] = [
`)

	for _, c := range caps {
		b.WriteString("  {\n")
		fmt.Fprintf(&b, "    id: %s,\n", tsString(c.ID))
		fmt.Fprintf(&b, "    label: %s,\n", tsString(c.Label))
		fmt.Fprintf(&b, "    detail: %s,\n", tsString(c.Detail))
		fmt.Fprintf(&b, "    minRole: %s,\n", tsString(c.MinRole))
		fmt.Fprintf(&b, "    roles: [%s],\n", joinTS(c.Roles))
		fmt.Fprintf(&b, "    action: %s,\n", tsString(c.Action))
		fmt.Fprintf(&b, "    routes: [%s],\n", joinTS(c.Routes))
		b.WriteString("  },\n")
	}
	b.WriteString("];\n")

	return b.Bytes(), nil
}

// joinTS renders a string slice as the inner part of a TS array literal.
//
// Long arrays are not wrapped: the output is for machines, and **the wrapping rule must not
// depend on prettier**. This repo has no prettier config (running it would reformat the whole
// file), so this function alone decides the format.
func joinTS(items []string) string {
	parts := make([]string, len(items))
	for i, s := range items {
		parts[i] = tsString(s)
	}
	return strings.Join(parts, ", ")
}

// tsString renders a Go string as a TS string literal.
//
// ⚠️ **Do not use %q**: Go's %q escapes Chinese as \uXXXX (strconv.Quote only guarantees ASCII
// safety), so the output would be full of things like `\u7a7a\u95f4`. **It compiles and runs,
// but nobody can read it**, and the text in this file is meant for people.
//
// Only the four characters that must be escaped are handled: backslash, double quote, newline,
// carriage return. The table text is written by us and has no control characters.
func tsString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

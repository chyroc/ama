// **Input** shapes for workspaces and members (create workspace, update workspace, add member).
//
// Kept separate from the two output types Workspace / WorkspaceMember: input is "what the user filled in",
// output is "what is in the database now"; the two have different rules for which fields are optional.

package apimodel

// WorkspaceInput is the input for creating a workspace.
type WorkspaceInput struct {
	Name        string
	Key         string
	Description string
}

// WorkspacePatch is the input for updating a workspace.
//
// All three fields are pointers: we need to tell "key not sent" from "empty string sent". Pay special
// attention to what Description means; see the comment on UpdateWorkspace.
type WorkspacePatch struct {
	Name        *string
	Key         *string
	Description *string
}

// MemberInput is the input for adding a member.
type MemberInput struct {
	Name  string
	Email string
	Role  WorkspaceRole
	// AvatarURL is the avatar address; may be empty. The frontend brings it over from the people picker's candidate record.
	//
	// **Not validated, not built, not looked up upstream**: it is only a cache; if it is broken the frontend falls back to initials.
	// See the comment on WorkspaceMember.AvatarURL.
	AvatarURL string
}

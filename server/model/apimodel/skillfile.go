// Content shape of skill files, and options for creating a skill.
//
// Moved from core so the orchestration layer's signatures do not need to import core. Fields and json tags are unchanged.

package apimodel

// SkillCreateOptions holds the optional arguments of CreateSkillFromPackage.
//
// The zero value is the default behavior (Source=custom, version "1"); both the upload and Fork paths pass the zero value.
type SkillCreateOptions struct {
	// An empty Source means SkillCustom.
	Source SkillSource
	// An empty LatestVersion means "1".
	LatestVersion string
}

// SkillFileContent is the content of one skill attachment, put directly into the HTTP response.
//
// The shape follows SandboxFileContent (see core/sandbox.go): both solve the same problem,
// "show the frontend a file that may be large and may be binary, safely", so the frontend can reuse
// the same rendering branches.
type SkillFileContent struct {
	Path string `json:"path"`
	Size int    `json:"size"`
	// Content is the text content. Binary files get no content; see Binary.
	Content string `json:"content"`
	// Truncated being true means only the first skillFileMaxBytes bytes were read.
	Truncated bool `json:"truncated"`
	// Binary being true means this is not a text file, and Content is empty.
	//
	// **A separate field instead of putting "this is binary" into Content**: otherwise the frontend cannot tell
	// "the file content happens to be that sentence" from "we decided it is binary". Same as SandboxFileContent.
	Binary bool `json:"binary"`
}

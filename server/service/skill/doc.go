// Package skill is the skill domain.
//
// # What this package is made of now (after step 11b)
//
//	The three pure-function files moved in 11a:
//	skillpkg.go      skill package parsing + all security checks (the line of defense for untrusted input)
//	skillzip.go      packaging for Ark / for the container, plus two fingerprints
//	skillbuiltin.go  the three built-in skills (code constants, not in the database)
//
//	The four business files moved in 11b (this domain's Service; see service.go):
//	service.go       Config / Service / New: the shape of dependency injection (following service/agent)
//	store.go         this domain's narrow Store interface (skills / skill_files + two tables owned by others)
//	skills.go        entity CRUD and attachment read/write (formerly core/skill.go)
//	blobs.go         reading and writing attachment bytes in object storage (the skill half of the former core/blob.go)
//
// On the core side, core/skill.go only has one forwarding line and type aliases left; the reasons
// are in each file's header (the fallback assertion requires the methods to exist).
//
// # ⚠️ Three skill domain files are still in core, each stuck on something
//
//	skillark.go      uploads skills to Ark. Needs runner (**runner belongs to the ark domain, not split yet**)
//	skillarchive.go  object storage presigned downloads. Needs BlobPresigner (the attachment code is still in core/blob.go)
//	skillinject.go   injects into the container. Holds *core.Service and uses unexported members
//	                 (ensureSkillArchive / store); it would have to become injected functions or a narrow interface,
//	                 which is a structural change, not a move
//
// These three belong to later steps, by the same rule as the 11a argument: mixed into a move diff,
// nobody could review them.
package skill

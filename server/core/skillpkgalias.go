// Aliases left on the core side after skill package parsing / packing / built-in skills moved
// into service/skill.
//
// # Why keep aliases instead of making callers switch to skillsvc.
//
// Exactly the same rule as the earlier domain moves (precedents: ComputeNextRun in
// core/schedule.go, IssueAttachmentURL in core/issue.go, ArkVerifier in core/service.go):
// **packages outside core should not change because of a pure move**. `core.HumanSkillFileLimit`
// is used by `openapi`, `core.MaxUploadBytes` is used by `handler.go`; changing those too means
// the diff is no longer only moves, and the whole value of a move MR is "the diff is only moves".
//
// # ⚠️ These are aliases (`=`), not new variables
//
// `var ErrUploadTooLarge = skillsvc.ErrUploadTooLarge` is **the same variable**,
// so `errors.Is(err, core.ErrUploadTooLarge)` and
// `errors.Is(err, skillsvc.ErrUploadTooLarge)` are equivalent, and the three 413 checks in the
// handler do not need to change. Types also use real aliases `type X = Y`, so the two
// compile-time assertions `var _ core.BlobPresigner = ...` in `cmd/amad` still hold.
//
// If we copied instead of aliasing, the sentinel would become two different variables and
// `errors.Is` would silently return false; that is exactly where zero-error failures like
// "an oversized upload returns 400 instead of 413" come from.
package core

import (
	skillsvc "github.com/chyroc/ama/server/service/skill"
)

// -- Upload and parsing (skillpkg.go) ------------------------------------------

// MaxUploadBytes is the limit on the upload request body; see skillsvc.MaxUploadBytes.
//
// ⚠️ **It must be set with http.MaxBytesReader before reading**; do not read into memory first
// and then check the length.
const MaxUploadBytes = skillsvc.MaxUploadBytes

// HumanSkillFileLimit / HumanSkillTotalLimit are the human-readable limits for docs and user text.
//
// ⚠️ **The `openapi` package uses these two** (twice each in routes.go and requests.go): the
// "per-file limit X, total limit Y" in API descriptions is computed from the constants, not a
// hard-coded literal. When the limits change the text follows automatically; see the comment
// on the skillsvc side for why.
var (
	HumanSkillFileLimit  = skillsvc.HumanSkillFileLimit
	HumanSkillTotalLimit = skillsvc.HumanSkillTotalLimit
)

// ErrUploadTooLarge is the sentinel error for a request body larger than MaxUploadBytes.
//
// ⚠️ **An alias, not a new value**: those handler checks use errors.Is to tell 413 apart from a
// plain 400, and a copy would make the check silently stop working.
var ErrUploadTooLarge = skillsvc.ErrUploadTooLarge

// IsUploadTooLarge reports whether an error means the request body was too large.
var IsUploadTooLarge = skillsvc.IsUploadTooLarge

// ParseSkillPackage parses an uploaded skill package (SKILL.md or zip).
//
// ⚠️ **It is the line of defense for untrusted input** (Zip Slip, decompression bombs, symlinks,
// absolute paths); the rules for all four attack types and their mutation checks are in the
// file header of service/skill/skillpkg.go.
var ParseSkillPackage = skillsvc.ParseSkillPackage

// CleanSkillPath validates and normalizes a relative path inside a skill.
//
// ⚠️ Its rules match `sanitizeZipEntry` one by one (`TestCleanSkillPathMatchesZipEntryJudgement`
// pins this relation); both guard the same invariant: a path stored in `skill_files` is always
// a normalized relative path. If they drift, "a shape that upload rejects can be pushed in
// through another entry point".
var CleanSkillPath = skillsvc.CleanSkillPath

// -- Packing and fingerprint (skillzip.go) -------------------------------------

// SkillZipResult is the packing result, including whether it was downgraded.
type SkillZipResult = skillsvc.SkillZipResult

// BuildSkillZip packs a skill into a zip that **Ark** accepts; when it exceeds Ark's three
// limits it downgrades to sending only SKILL.md.
//
// ⚠️ **The container side must use BuildFullSkillZip**; the two were mixed up once, with zero errors.
var BuildSkillZip = skillsvc.BuildSkillZip

// BuildFullSkillZip packs a **complete** skill package, ignoring Ark's three limits.
var BuildFullSkillZip = skillsvc.BuildFullSkillZip

// SkillFingerprint computes a skill's content fingerprint, used to decide "does it need to be
// uploaded to Ark again".
var SkillFingerprint = skillsvc.SkillFingerprint

// SkillsFingerprint computes the overall fingerprint of a set of skill references.
//
// ⚠️ Its result goes into `ArkBinding.SkillsFingerprint` (the stored format of a jsonb column);
// do not change a single byte of the algorithm or the version prefix `v2:`. Changing it makes
// that workspace unable to start new conversations ever again, with zero errors.
var SkillsFingerprint = skillsvc.SkillsFingerprint

// -- Built-in skills (skillbuiltin.go) -----------------------------------------

// BuiltinSkillPrefix is the fixed prefix of built-in skill ids.
const BuiltinSkillPrefix = skillsvc.BuiltinSkillPrefix

// The ids of the built-in skills. **These strings are stored in agent.SkillIDs**, so they can
// never change.
const (
	BuiltinSkillIDLarkCLI = skillsvc.BuiltinSkillIDLarkCLI
	BuiltinSkillIDAMA     = skillsvc.BuiltinSkillIDAMA
)

// IsBuiltinSkill reports whether a skill id is a platform built-in.
//
// ⚠️ **This is a security boundary**: `resource.go` and `revision.go` rely on it to resolve
// built-in skills to platform-level resources (`ErrPlatformResource`). Returning an empty
// string would make authz let the request through, so any logged-in user could delete built-in
// skills.
var IsBuiltinSkill = skillsvc.IsBuiltinSkill

// BuiltinSkills returns all built-in skills in a fixed order. Each call returns a new slice.
var BuiltinSkills = skillsvc.BuiltinSkills

// BuiltinSkill gets one built-in skill by id; the second return value says whether it was found.
var BuiltinSkill = skillsvc.BuiltinSkill

// -- Unexported aliases: keep those files in core unchanged --------------------
//
// The five below are **exported** on the service/skill side (`SkillFileName`,
// `MaxSkillFileBytes`…); here each gets its original lowercase name back. **This is not
// pointless extra work**: `core/skill.go` references them, and with the aliases that file did
// not need a single character changed this time; the whole value of a move MR is "the diff is
// only moves". When those files move over themselves, delete these five lines with them.
//
// ⚠️ **They are exported on the skillsvc side because this move forced it, not as "widening
// visibility while we were at it"**: core and service/skill are different packages, and
// unexported symbols cannot be reached across packages. The rule is "is anyone on the core side
// still using it": once nobody does, change it back to lowercase on the skillsvc side.
const (
	maxSkillFileBytes  = skillsvc.MaxSkillFileBytes
	maxSkillTotalBytes = skillsvc.MaxSkillTotalBytes

	// maxSkillPathBytes is only used by core/skillfile_test.go (it builds an overlong path to
	// check that SkillFileContent rejects it). **It must be computed from the constant, not
	// written as the literal 200**: if hard-coded, the test stays green after the limit changes,
	// and the whole point of that test is "an overlong path is rejected".
	maxSkillPathBytes = skillsvc.MaxSkillPathBytes

	// maxSkillFiles likewise is only used by core/skillmarket_test.go (it builds a market
	// package over the entry-count limit to check the error clearly says "the package is fine,
	// we just cannot fit it").
	maxSkillFiles = skillsvc.MaxSkillFiles
)

// The five aliases below are **only used by core tests** (when they moved, the real definitions
// went to service/skill while the tests stayed in core and keep running). Same rule as the
// section above: once no core production code uses them, delete the aliases and move the tests
// to service/skill as well.
const (
	skillFileMaxBytes   = skillsvc.SkillFileMaxBytes
	blobPrefixSkillFile = skillsvc.BlobPrefixSkillFile
)

var (
	bumpVersion    = skillsvc.BumpVersion
	snapshotSkill  = skillsvc.SnapshotSkill
	sameSkillDraft = skillsvc.SameSkillDraft
)

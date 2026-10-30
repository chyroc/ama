// Business logic for skills: entity CRUD and attachment read/write.
//
// This file **was moved as a whole** from core/skill.go (step 11b), with not a line of logic
// changed; core/skill.go only has one forwarding line left. The argument for the move and the
// story of "domain ≠ file" are in that file's history and in service/skill/doc.go.
//
// # Cross-aggregate invariant of the skill domain
//
// Deleting a skill must clear dangling references on every agent (see the loop in DeleteSkill);
// the same shape as the Feishu app one, and the reason this logic needs a Service.

package skill

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// ---------------------------------------------------------------------------
// Skills
// ---------------------------------------------------------------------------

// ListSkills lists the skills in a workspace, **with platform built-in skills first**.
//
// Built-in skills are not in the database (the source of truth is skillbuiltin.go), so they are
// joined in here.
//
// # The merge lives in Service, not Store
//
// Store has two implementations (MemStore and PGStore); writing it in each would surely drift, and
// since unit tests run MemStore and production runs PGStore, drift shows up as "tests all green,
// production wrong". store.go records that SkillFilesRoundTrip already caught one split of this
// kind. In Service there is only one copy.
func (s *Service) ListSkills(ctx context.Context, workspaceID string) ([]apimodel.Skill, error) {
	own, err := s.store.ListSkills(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	builtin := BuiltinSkills()
	// Preallocate the final length, so append does not copy again when growing.
	out := make([]apimodel.Skill, 0, len(builtin)+len(own))
	out = append(out, builtin...)
	return append(out, own...), nil
}

// GetSkill gets one skill; returns ErrNotFound if it does not exist.
//
// **Built-in skills short-circuit before the database query**: they are not in the database, so
// querying would always give ErrNotFound.
func (s *Service) GetSkill(ctx context.Context, id string) (apimodel.Skill, error) {
	if sk, ok := BuiltinSkill(id); ok {
		return sk, nil
	}
	sk, err := s.store.GetSkill(ctx, id)
	if err != nil {
		if errors.Is(err, persistmodel.ErrNotFound) {
			return apimodel.Skill{}, fmt.Errorf("%w: 技能不存在", persistmodel.ErrNotFound)
		}
		return apimodel.Skill{}, err
	}
	return sk, nil
}

// CreateSkill creates a skill.
//
// Skill name uniqueness is **per workspace** (copied from api.ts:411). Note this differs from the
// uniqueness of a Feishu app's appId, which is global. This asymmetry is existing frontend
// behavior; do not "unify" it.
func (s *Service) CreateSkill(ctx context.Context, workspaceID string, draft apimodel.SkillDraft) (apimodel.Skill, error) {
	if err := validateSkillDraft(draft); err != nil {
		return apimodel.Skill{}, err
	}
	if _, err := s.store.GetWorkspace(ctx, workspaceID); err != nil {
		return apimodel.Skill{}, fmt.Errorf("%w: 空间不存在", persistmodel.ErrNotFound)
	}

	name := strings.TrimSpace(draft.Name)
	// Name clashes with built-in skills must be blocked too: built-in skills are visible in every
	// workspace, a clash would show two skills with the same name in the list, and the binding UI
	// could not tell them apart by name at all.
	if BuiltinSkillNameTaken(name) {
		return apimodel.Skill{}, fmt.Errorf("%w: 技能名「%s」是平台内置技能，请换一个", persistmodel.ErrConflict, name)
	}
	taken, err := s.store.SkillNameTaken(ctx, workspaceID, name)
	if err != nil {
		return apimodel.Skill{}, err
	}
	if taken {
		return apimodel.Skill{}, fmt.Errorf("%w: 技能名「%s」在本空间已存在", persistmodel.ErrConflict, name)
	}

	now := apimodel.NowISO()
	actor := auth.ActorFrom(ctx)
	sk := apimodel.Skill{
		ID:           s.nextID("skill"),
		WorkspaceID:  workspaceID,
		Name:         name,
		DisplayTitle: draft.DisplayTitle,
		Description:  draft.Description,
		Instructions: draft.Instructions,
		Source:       apimodel.SkillCustom,
		// The version is a number in string form, starting at "1"
		LatestVersion: "1",
		Tags:          nonNil(draft.Tags),
		CreatedAt:     now,
		UpdatedAt:     now,
		// Who created it. **Only taken from context**; SkillDraft has no such fields; see the section
		// on apimodel.Skill.CreatedBy. Without an identity it is an empty string, and with omitempty the
		// key disappears, the same presentation as existing skills ("unknown who created it"); no fake
		// value on purpose.
		CreatedBy: actor,
		UpdatedBy: actor,
	}
	return s.store.CreateSkill(ctx, sk)
}

// UpdateSkill updates a skill and increments latestVersion.
//
// **Deliberately no uniqueness check**: LocalApiClient's updateSkill has none either
// (api.ts:428-436), so a rename clash is allowed. The asymmetry with CreateSkill is existing
// frontend behavior.
//
// latestVersion is a number in **string** form (Ark's latest_version is a string), so incrementing
// means parse → +1 → format. A non-numeric value does not panic: a parse failure counts from 0,
// unlike JS's String(Number("x") + 1) → "NaN", but that result would be worse.
func (s *Service) UpdateSkill(ctx context.Context, id string, draft apimodel.SkillDraft) (apimodel.Skill, error) {
	return s.UpdateSkillWithReason(ctx, id, draft, apimodel.RevisionReasonEdit)
}

// UpdateSkillWithReason is the implementation of UpdateSkill, with one more thing: "what reason to
// record for this change in history".
//
// It is exported separately because the rollback path goes through it (reason rollback), and the
// rollback logic is in the revision domain, not this package. Same as
// agentsvc.UpdateAgentWithReason / schedulesvc.UpdateScheduleWithReason.
func (s *Service) UpdateSkillWithReason(
	ctx context.Context, id string, draft apimodel.SkillDraft, reason apimodel.RevisionReason,
) (apimodel.Skill, error) {
	// The body of a built-in skill is maintained by the platform (the source of truth is the
	// constants in skillbuiltin.go) and cannot be changed.
	//
	// **Return ErrPrecondition instead of letting it continue**: continuing would turn into "skill
	// does not exist" at GetSkill, and that sentence is wrong; it is right there in the list. The
	// authz layer also blocks this route; this is the second line: Service is an exported API, and
	// the Feishu path and future callers do not go through HTTP.
	if IsBuiltinSkill(id) {
		return apimodel.Skill{}, fmt.Errorf("%w: 平台内置技能不可修改", persistmodel.ErrPrecondition)
	}
	if err := validateSkillDraft(draft); err != nil {
		return apimodel.Skill{}, err
	}
	sk, err := s.store.GetSkill(ctx, id)
	if err != nil {
		return apimodel.Skill{}, fmt.Errorf("%w: 技能不存在", persistmodel.ErrNotFound)
	}
	// Record a snapshot before the change, for rolling back mistakes. Nothing is recorded if the content did not change, same as UpdateAgent.
	if before := SnapshotSkill(sk); !SameSkillDraft(before, draft) {
		s.revisions.Record(ctx, apimodel.RevisionKindSkill, sk.WorkspaceID, sk.ID,
			sk.DisplayTitle, before, reason)
	}

	sk.Name = strings.TrimSpace(draft.Name)
	sk.DisplayTitle = draft.DisplayTitle
	sk.Description = draft.Description
	sk.Instructions = draft.Instructions
	sk.Tags = nonNil(draft.Tags)
	sk.LatestVersion = bumpVersion(sk.LatestVersion)
	sk.UpdatedAt = apimodel.NowISO()
	// Who changed it. **Only overwrite UpdatedBy, never touch CreatedBy**: the latter is the one read
	// back above, kept as is. Assigning CreatedBy too is a natural slip, and after that "who created
	// it" would become "who changed it last", collapsing two fields into one with no error at all.
	// Same as in UpdateAgent.
	sk.UpdatedBy = auth.ActorFrom(ctx)

	return s.store.SaveSkill(ctx, sk)
}

// bumpVersion increments a version number in string form.
func bumpVersion(cur string) string {
	n, err := strconv.Atoi(strings.TrimSpace(cur))
	if err != nil {
		// Dirty data should not make the request fail; start counting again from 1
		return "1"
	}
	return strconv.Itoa(n + 1)
}

// CreateSkillFromPackage creates a skill from a parsed skill package, saving its attachments too.
//
// # Why not reuse CreateSkill
//
// Reusing it would mean creating the skill first and then saving attachments in a separate call,
// leaving a window where "the skill exists but the attachments are not written yet". If a session
// happens to use this skill in that window, files are missing when injecting into the container,
// and missing files give no error; they just make the agent retry again and again. So it takes this
// separate path, doing both writes in order before returning.
//
// Name uniqueness follows the same rule as CreateSkill (per workspace, only on the create side).
// **This check cannot be skipped**: the upload path is also a create, and a clash must give the
// same sentence as the hand-filled one, otherwise one rule behaves two ways.
//
// For opts see apimodel.SkillCreateOptions; the zero value equals the old behavior.
func (s *Service) CreateSkillFromPackage(
	ctx context.Context, workspaceID string, pkg apimodel.SkillPackage, opts apimodel.SkillCreateOptions,
) (apimodel.Skill, error) {
	if _, err := s.store.GetWorkspace(ctx, workspaceID); err != nil {
		return apimodel.Skill{}, fmt.Errorf("%w: 空间不存在", persistmodel.ErrNotFound)
	}

	name := strings.TrimSpace(pkg.Name)
	// The parser already validated the name's character set, but block empty values again here:
	// CreateSkillFromPackage is an exported method, and other callers may not go through the parser.
	if name == "" {
		return apimodel.Skill{}, fmt.Errorf("%w: 请填写标识", persistmodel.ErrInvalidArgument)
	}
	// Same reason as CreateSkill: built-in skills are visible in every workspace, so names must not clash.
	if BuiltinSkillNameTaken(name) {
		return apimodel.Skill{}, fmt.Errorf("%w: 技能名「%s」是平台内置技能，请换一个", persistmodel.ErrConflict, name)
	}
	taken, err := s.store.SkillNameTaken(ctx, workspaceID, name)
	if err != nil {
		return apimodel.Skill{}, err
	}
	if taken {
		return apimodel.Skill{}, fmt.Errorf("%w: 技能名「%s」在本空间已存在", persistmodel.ErrConflict, name)
	}

	displayTitle := strings.TrimSpace(pkg.DisplayTitle)
	if displayTitle == "" {
		displayTitle = name
	}

	// The Files stored in doc only keep path + size, **without content bytes**: doc is the return
	// value of ListSkills, and including bytes would make list responses grow with attachments. Bytes
	// go to the skill_files table (see that comment in schema_pg.sql).
	//
	// ⚠️ **Size is computed from the byte count, not trusted from the caller's `f.Size`.**
	// Both Store implementations use this rule ("Size is computed from the content length, the
	// caller's value is not trusted"), and this follows it; otherwise one concept would have two
	// algorithms in three places.
	//
	// The difference really shows: the `ParseSkillPackage` path (zip upload, import from a
	// repository) fills Size, while **the Fork path does not**: its pkg.Files come from
	// `s.SkillFiles()`, whose Size is the value after hydration, and if any step in between dropped
	// it, it would land as 0. Landing as 0 only shows in the UI: the file shows "0 bytes" in the list
	// while **the bytes are actually fine**, with no error at all.
	// TestSkillFilesSizeSurvivesContentStopWrite pins this.
	meta := make([]apimodel.SkillFile, 0, len(pkg.Files))
	for _, f := range pkg.Files {
		meta = append(meta, apimodel.SkillFile{Path: f.Path, Size: len(f.Content)})
	}
	persistmodel.SortSkillFiles(meta)

	now := apimodel.NowISO()
	actor := auth.ActorFrom(ctx)
	// Each of the three opts fields has a default; see apimodel.SkillCreateOptions. **Written in one go**, no backfill window.
	source := opts.Source
	if source == "" {
		source = apimodel.SkillCustom
	}
	version := strings.TrimSpace(opts.LatestVersion)
	if version == "" {
		version = "1"
	}
	sk := apimodel.Skill{
		ID:            s.nextID("skill"),
		WorkspaceID:   workspaceID,
		Name:          name,
		DisplayTitle:  displayTitle,
		Description:   pkg.Description,
		Instructions:  pkg.Instructions,
		Source:        source,
		LatestVersion: version,
		Tags:          nonNil(nil),
		CreatedAt:     now,
		UpdatedAt:     now,
		// Who created it. This path is the **shared** exit of upload / Fork, all triggered by a user
		// clicking, so it is always stamped.
		//
		// ⚠️ It records **the person who clicked install**, not the skill author; the author is in
		// Origin. See the three differences on apimodel.Skill.CreatedBy.
		CreatedBy: actor,
		UpdatedBy: actor,
	}
	if len(meta) > 0 {
		// Only assigned when there really are attachments: Skill.Files has omitempty, and both an
		// empty slice and nil serialize as "no such key", but leaving nil is clearer (see the
		// apimodel.Skill.Files comment).
		sk.Files = meta
	}

	created, err := s.store.CreateSkill(ctx, sk)
	if err != nil {
		return apimodel.Skill{}, err
	}
	if len(pkg.Files) > 0 {
		// Write the bytes into object storage first, fill in FileID, then save.
		//
		// ⚠️ **A failure blocks**: bytes only live in object storage, so failing to write them means
		// they have nowhere to go, and the skill must be deleted again here instead of leaving an empty
		// shell. See the putSkillFileBlobs comment.
		withBlobs, err := s.PutSkillFileBlobs(ctx, created.ID, auth.ActorFrom(ctx), pkg.Files)
		if err != nil {
			// Same as the one below: the skill has been created, so if attachments cannot be written,
			// delete it again for the user to retry; otherwise we leave a skill that "has 3 attachments"
			// while none can be read in the container.
			s.rollbackCreatedSkill(ctx, created.ID, err)
			return apimodel.Skill{}, err
		}
		if err := s.store.PutSkillFiles(ctx, created.ID, workspaceID, withBlobs); err != nil {
			// If the attachments were not written, the skill should not stay; otherwise the user sees a
			// skill that "has 3 attachments" while none can be read in the container. Deleting it again
			// for a retry is better than leaving something half-done.
			s.rollbackCreatedSkill(ctx, created.ID, err)
			return apimodel.Skill{}, err
		}
	}
	return created, nil
}

// rollbackCreateTimeout is the time budget for "the attachments failed, delete the just-created skill again".
const rollbackCreateTimeout = 10 * time.Second

// rollbackCreatedSkill deletes a new skill whose attachments failed to write.
//
// ⚠️ **Do not delete with the request's ctx.** The most common reason attachment writes fail is
// exactly that the request ctx was cancelled (client disconnect, gateway timeout): a skill with a
// few hundred files is written to object storage one by one, the request is cut off halfway, and
// you get `context canceled`. Deleting with the same cancelled ctx fails immediately too, and if
// that failure were swallowed, the database would be left with a skill that declares a few hundred
// attachments and has not a single file (later injection reports "the skill declares attachments
// but the database has none"), exactly the half-done state this guards against. So the rollback is
// detached from the request's cancellation and gets its own time budget.
//
// A failed delete is only logged, not reported: at that point cause itself is the reason the user
// should see.
func (s *Service) rollbackCreatedSkill(ctx context.Context, skillID string, cause error) {
	delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackCreateTimeout)
	defer cancel()
	if err := s.store.DeleteSkill(delCtx, skillID); err != nil {
		s.log.Error("附件没写成，删回新建的技能也失败了：库里会留下一个没有附件字节的技能，需要手工删",
			"skill", skillID, "cause", cause.Error(), "err", err.Error())
	}
}

// SkillFiles gets **all** attachments of a skill (with content bytes).
//
// Used by the container injection path (see SkillLoader in shared/infra/shelltools and
// core/skillinject).
//
// ⚠️ **This method itself still does not go into HTTP**: it returns all bytes at once (up to 2
// MiB), while the page only looks at one file at a time. What is exposed externally is
// SkillFileContent below: one by path, with truncation and binary detection. This used to say "the
// HTTP layer does not expose attachment content", which no longer holds (the skill detail page has
// to let people see files), so it now says clearly "which one is not exposed, and why".
//
// # Byte source: object storage only
//
// ⚠️ **This is the only entry point this service has for reading skill file bytes; do not bypass it
// by calling store.SkillFiles directly**: that layer returns no bytes at all (only path and
// file_id), and bypassing it gives a batch of zero-byte files, while the container injection path
// degrades silently and nobody reports an error. Fetching bytes, filling in Size, and concurrent
// fetching are all in hydrateSkillFiles.
func (s *Service) SkillFiles(ctx context.Context, skillID string) ([]apimodel.SkillFile, error) {
	// Built-in skills have no attachments (the CLIs are already in the container; no files need to
	// be written there) and are not in the database; querying would only give empty, at the cost of a
	// wasted database call.
	if IsBuiltinSkill(skillID) {
		return nil, nil
	}
	files, err := s.store.SkillFiles(ctx, skillID)
	if err != nil {
		return nil, err
	}
	return s.HydrateSkillFiles(ctx, files)
}

// skillFileMaxBytes is the size limit for viewing a single attachment on the page.
//
// Same value as sandboxFileMaxBytes (256 KiB) and **deliberately defined separately**: that one is
// for arbitrary files in the container, this one for skill attachments, and the two may change
// independently later.
//
// ⚠️ **It is no longer equal to MaxSkillFileBytes, and truncation is no longer a rare branch.**
// Both used to be 256 KiB, so "normal attachments are never truncated" and truncation only guarded
// against existing dirty data. The attachment limit has been raised to 25 MiB (after the injection
// channel changed, it is no longer limited by the shell command line), while **the page preview
// limit was deliberately not raised with it**: stuffing 25 MiB into a JSON response and rendering it
// in the browser makes no sense.
//
// So `Truncated: true` went from "almost never seen" to **the normal case for large attachments**.
// The frontend must show the "only the first 256 KiB are shown" hint as usual
// (skill.fileTruncated in messages.ts), and that hint will now really be seen.
const skillFileMaxBytes = 256 * 1024

// SkillFileMaxBytes exports the page preview limit, for test assertions on the core side (it is a
// different limit from MaxSkillFileBytes; see the section above). Same rule as skillpkgalias.
const SkillFileMaxBytes = skillFileMaxBytes

// SkillFileContent gets the content of one skill attachment by path, for the detail page.
//
// # Why not reuse SkillFiles and let the frontend pick
//
// That method returns all bytes of all attachments (up to 2 MiB), while the page only shows one
// file at a time. Making the frontend fetch everything costs sending all attachments on every visit
// to the detail page, most of which are never looked at.
//
// # Three ways of handling that match ReadRunSandboxFile
//
//   - Binaries get no content (the rule is containing a NUL byte; see isBinary): stuffing random
//     bytes into JSON and rendering them on the page makes no sense.
//   - Beyond skillFileMaxBytes, truncate and mark it instead of refusing; the first 256 KiB are
//     usually enough.
//   - Normalize the path before comparing, so `./scripts/x.sh` and `scripts/x.sh` are not treated
//     as two files.
func (s *Service) SkillFileContent(
	ctx context.Context, skillID, filePath string,
) (apimodel.SkillFileContent, error) {
	// An empty path must give an explicit error. Otherwise it would degrade into "looped through and
	// found nothing" → 404, and "file does not exist" is wrong for a request that gave no path at all.
	clean, ok := CleanSkillPath(filePath)
	if !ok {
		return apimodel.SkillFileContent{}, fmt.Errorf("%w: 要给一个合法的文件路径", persistmodel.ErrInvalidArgument)
	}

	// **Built-in skills short-circuit before the database query.** They have zero attachments (see
	// the top of skillbuiltin.go), so querying would only be wasted; more importantly they are not in
	// the skills table, and continuing would give "skill does not exist", which is wrong; it is right
	// there in the list.
	if IsBuiltinSkill(skillID) {
		return apimodel.SkillFileContent{}, fmt.Errorf("%w: 平台内置技能没有附件", persistmodel.ErrNotFound)
	}

	// First confirm the skill itself exists, so "the skill is gone" and "the skill exists, this file
	// does not" are reported separately. Merged into one "does not exist", the user could not tell
	// whether to recreate the skill or re-upload the attachment.
	if _, err := s.store.GetSkill(ctx, skillID); err != nil {
		if errors.Is(err, persistmodel.ErrNotFound) {
			return apimodel.SkillFileContent{}, fmt.Errorf("%w: 技能不存在", persistmodel.ErrNotFound)
		}
		return apimodel.SkillFileContent{}, err
	}

	files, err := s.store.SkillFiles(ctx, skillID)
	if err != nil {
		return apimodel.SkillFileContent{}, err
	}
	for _, f := range files {
		if f.Path != clean {
			continue
		}
		// ⚠️ **Only fill in this one file's bytes, not the whole batch.**
		//
		// Going through s.SkillFiles would fetch the bytes of every file in the skill from object
		// storage (15 files = 15 GETs), while this API needs only one of them; the page would pay for
		// everything every time a file is opened. So pick it by path first, then fill in only that one.
		//
		// Pass a one-element slice instead of adding a "fill in one" function: the fallback rule
		// should have only one implementation, and a second copy would drift (and drift would show as
		// "some paths fall back, some do not", with no error).
		hydrated, err := s.HydrateSkillFiles(ctx, []apimodel.SkillFile{f})
		if err != nil {
			return apimodel.SkillFileContent{}, err
		}
		f = hydrated[0]

		out := apimodel.SkillFileContent{Path: f.Path, Size: len(f.Content)}
		data := f.Content
		if len(data) > skillFileMaxBytes {
			data = data[:skillFileMaxBytes]
			out.Size = skillFileMaxBytes
			out.Truncated = true
		}
		if isBinary(data) {
			out.Binary = true
			return out, nil
		}
		out.Content = string(data)
		return out, nil
	}
	return apimodel.SkillFileContent{}, fmt.Errorf("%w: 技能里没有这个文件", persistmodel.ErrNotFound)
}

// UpdateSkillFile changes the content of an **existing** attachment.
//
// # ⚠️ This function must block builtin itself; it cannot rely on UpdateSkill
//
// It goes through `PutSkillFiles` and **never passes through UpdateSkill**, so the read-only gate
// there (`IsBuiltinSkill`) never runs on this path. Missing it means "the body of a built-in skill
// cannot be changed, but its attachments can": an asymmetric hole with **no error**. Pinned by a
// test, and mutation-tested.
//
// # Only change existing files, never create
//
// The path must already be among this skill's attachments, otherwise ErrNotFound. That way the
// entry count limit (MaxSkillFiles) naturally cannot be exceeded, and there is no need to check
// "would adding one go over" here. Create / delete / rename are another matter (they need to deal
// with full-replacement semantics head on), and this function does not do them.
//
// # Saving is three steps, and the order cannot change
//
//  1. write skill_files (the source of truth read by container injection)
//  2. recompute the size in doc.Files
//  3. SaveSkill (the source of truth read by the list display)
//
// **Attachments first, doc after**: the other way round, in the window between them the agent
// would read by the new list content that has not landed yet, and **failing to read gives no
// error**; it just makes it retry again and again.
//
// And step 2 cannot be skipped: `doc.Files[i].Size` was computed and stored at upload time, and if
// the content changes without syncing it, the UI **shows the old size forever** for that file (the
// count does not change, so "N attachments" looks right; only the size is silently wrong).
//
// # The fingerprint changes, and that is expected
//
// SkillFingerprint includes each attachment's path and bytes, so the next conversation re-uploads
// to Ark and swaps skills via UpdateAgent; **do not bypass it**, that is exactly how "takes effect
// right after the change" is implemented. The version number is bumped too, symmetric with changing
// the body (`SkillFingerprint` deliberately excludes LatestVersion, so the bump itself does not
// cause a pointless re-upload).
func (s *Service) UpdateSkillFile(
	ctx context.Context, skillID, filePath string, content []byte,
) (apimodel.Skill, error) {
	// Built-in skills: zero attachments and not in the database. **Blocked before the database
	// query**, for the same reason as UpdateSkill (continuing would turn into "skill does not
	// exist", which is wrong). It also stops a real piece of dirty data: writing with a
	// skill-builtin-* id would insert an orphan row with an empty workspace_id into skill_files.
	if IsBuiltinSkill(skillID) {
		return apimodel.Skill{}, fmt.Errorf("%w: 平台内置技能不可修改", persistmodel.ErrPrecondition)
	}

	clean, ok := CleanSkillPath(filePath)
	if !ok {
		return apimodel.Skill{}, fmt.Errorf("%w: 要给一个合法的文件路径", persistmodel.ErrInvalidArgument)
	}
	// SKILL.md is not an attachment: its content is in Instructions, and changing it goes through
	// UpdateSkill.
	//
	// **It must be rejected explicitly, not silently let through**: `skill_files` has no SKILL.md row
	// at all (see "not stored again in Files" in skillpkg.go); letting it through would insert an
	// attachment called SKILL.md into the database, creating a second source of truth competing with
	// Instructions; and when injecting into the container it would be written as the container's
	// SKILL.md, fighting with the Ark-side copy. No error.
	if clean == SkillFileName {
		return apimodel.Skill{}, fmt.Errorf(
			"%w: %s 是技能正文，请在正文编辑区修改", persistmodel.ErrInvalidArgument, SkillFileName)
	}
	if len(content) > MaxSkillFileBytes {
		return apimodel.Skill{}, fmt.Errorf(
			"%w: 单个文件不能超过 %d KiB", persistmodel.ErrInvalidArgument, MaxSkillFileBytes>>10)
	}

	sk, err := s.store.GetSkill(ctx, skillID)
	if err != nil {
		return apimodel.Skill{}, fmt.Errorf("%w: 技能不存在", persistmodel.ErrNotFound)
	}
	files, err := s.store.SkillFiles(ctx, skillID)
	if err != nil {
		return apimodel.Skill{}, err
	}
	// ⚠️ **Fill in the bytes before changing**: this is a read-modify-write, and the other files'
	// Content is written back as is. Without filling in, files already moved to object storage would
	// be overwritten by old bytes in the content column: changing file A reverts file B to an old
	// version, with no error.
	//
	// Now that the content column is no longer written, not filling in is **silent data corruption**.
	files, err = s.HydrateSkillFiles(ctx, files)
	if err != nil {
		return apimodel.Skill{}, err
	}
	// Find that entry and replace its content. **A whole read-modify-write instead of adding a
	// single-file Store method**: PutSkillFiles is "delete then insert in one transaction", already
	// atomic; adding PutSkillFile would mean writing it once in MemStore and once in PGStore, and those
	// two copies have drifted before (the Size semantics split caught by SkillFilesRoundTrip).
	//
	// ⚠️ This used to say "at most 64 attachments and 2 MiB total, so the cost of writing everything
	// back is negligible"; the limits have been raised to 10000 entries / 100 MiB (skillpkg.go), and
	// "negligible" no longer holds: changing one attachment rewrites all attachments of the skill. We
	// still accept that cost today (changing attachments is a rare manual action, and splitting into
	// single-file writes needs a copy in each Store, which have drifted before), but **it is no
	// longer free**, and it is noticeably slower on large skills.
	found := false
	total := 0
	for i := range files {
		if files[i].Path == clean {
			files[i].Content = content
			files[i].Size = len(content)
			found = true
		}
		total += len(files[i].Content)
	}
	if !found {
		// Only existing files can be changed, so "no such path" is an error, not "then create one".
		return apimodel.Skill{}, fmt.Errorf("%w: 技能里没有这个文件", persistmodel.ErrNotFound)
	}
	if total > MaxSkillTotalBytes {
		return apimodel.Skill{}, fmt.Errorf(
			"%w: 附件总量不能超过 %d MiB", persistmodel.ErrInvalidArgument, MaxSkillTotalBytes>>20)
	}

	// Step 1: attachments first. Bytes are written into object storage (see putSkillFileBlobs).
	// ⚠️ A write failure is reported up: bytes only live in object storage, and if they cannot be written they have nowhere to land.
	files, err = s.PutSkillFileBlobs(ctx, skillID, auth.ActorFrom(ctx), files)
	if err != nil {
		return apimodel.Skill{}, err
	}
	if err := s.store.PutSkillFiles(ctx, skillID, sk.WorkspaceID, files); err != nil {
		return apimodel.Skill{}, err
	}
	// Step 2: recompute the size of the list in doc (**only path + size, no bytes**, the same as
	// CreateSkillFromPackage; doc is the return value of ListSkills).
	meta := make([]apimodel.SkillFile, 0, len(files))
	for _, f := range files {
		meta = append(meta, apimodel.SkillFile{Path: f.Path, Size: f.Size})
	}
	persistmodel.SortSkillFiles(meta)
	sk.Files = meta
	sk.LatestVersion = bumpVersion(sk.LatestVersion)
	sk.UpdatedAt = apimodel.NowISO()
	// Step 3: doc after.
	return s.store.SaveSkill(ctx, sk)
}

// DeleteSkill deletes a skill and removes every agent's binding to it.
//
// The cleanup is **across all workspaces** (not filtered by workspace), and **does not bump the
// agent's version or touch updatedAt**, copied from api.ts:441-443. Bumping would make the frontend
// think someone changed the agent.
//
// Deleting an id that does not exist is not an error (idempotent).
func (s *Service) DeleteSkill(ctx context.Context, id string) error {
	// Built-in skills cannot be deleted, for the same reason as UpdateSkill.
	//
	// ⚠️ **This one must not take the idempotent "deleting a missing one is not an error" path**:
	// that would succeed silently, and then everything below would remove every agent's binding to
	// it; the user clicks delete once, the skill is still in the list, but every agent's checkbox is
	// gone, with no error at all.
	if IsBuiltinSkill(id) {
		return fmt.Errorf("%w: 平台内置技能不可删除", persistmodel.ErrPrecondition)
	}
	if err := s.store.DeleteSkill(ctx, id); err != nil {
		return err
	}
	// The skill is gone, so its history is cleared too (same as DeleteAgent).
	s.revisions.Drop(ctx, apimodel.RevisionKindSkill, id)
	// When a skill is deleted, unbind it from all agents so no dangling reference is left.
	// This needs to go through agents in every workspace, so list workspaces first, then agents.
	workspaces, err := s.store.ListWorkspaces(ctx)
	if err != nil {
		return err
	}
	for _, ws := range workspaces {
		agents, err := s.store.ListAgents(ctx, ws.ID)
		if err != nil {
			return err
		}
		for _, a := range agents {
			if !contains(a.SkillIDs, id) {
				continue
			}
			a.SkillIDs = filterSlice(a.SkillIDs, func(x string) bool { return x != id })
			if _, err := s.store.SaveAgent(ctx, a); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateSkillDraft(d apimodel.SkillDraft) error {
	if strings.TrimSpace(d.Name) == "" {
		return fmt.Errorf("%w: 技能名不能为空", persistmodel.ErrInvalidArgument)
	}
	return nil
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func filterSlice[T any](in []T, keep func(T) bool) []T {
	out := make([]T, 0, len(in))
	for _, v := range in {
		if keep(v) {
			out = append(out, v)
		}
	}
	return out
}

func nonNil(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// isBinary tells whether some bytes "should not be rendered as text": containing a NUL byte counts
// as binary.
//
// Same name and body as the one in core/sandbox.go (that one is for container files, this one for
// skill attachments). The reason for a copy each is the same as the frameType precedent: the two
// check different things, and sharing would pull in a package dependency for a two-line function.
func isBinary(data []byte) bool {
	for _, b := range data {
		if b == 0 {
			return true
		}
	}
	return false
}

// SnapshotSkill compresses a skill into a pre-change snapshot, for the revision domain to archive
// and roll back.
//
// Moved as is from core/revision.go ("which fields should be recorded in history" is this domain's
// own knowledge, following the precedent of snapshotSchedule, which already moved with
// service/schedule).
//
// ⚠️ **SkillDraft has no Files**, so attachments are outside the rollback scope. This is not an
// oversight but a trade-off: attachments live in the separate skill_files table (bytes in object
// storage), and snapshotting them would make every history entry carry the bytes of the whole skill
// package; a skill with 20 history entries could be tens of MB, while what users want to restore is
// almost always the body. **The UI must say this clearly**, or users will think rollback also
// restored the attachments.
func SnapshotSkill(sk apimodel.Skill) apimodel.SkillDraft {
	return apimodel.SkillDraft{
		Name:         sk.Name,
		DisplayTitle: sk.DisplayTitle,
		Description:  sk.Description,
		Instructions: sk.Instructions,
		Tags:         sk.Tags,
	}
}

// SameSkillDraft tells "did this save change anything"; history is only recorded if it did.
//
// Moved as is from core/revisiondiff.go (the normalize + DeepEqual semantics unchanged), for the
// same reason as SnapshotSkill.
func SameSkillDraft(before, draft apimodel.SkillDraft) bool {
	after := apimodel.SkillDraft{
		Name:         strings.TrimSpace(draft.Name),
		DisplayTitle: draft.DisplayTitle,
		Description:  draft.Description,
		Instructions: draft.Instructions,
		Tags:         nonNil(draft.Tags),
	}
	normalized := apimodel.SkillDraft{
		Name:         strings.TrimSpace(before.Name),
		DisplayTitle: before.DisplayTitle,
		Description:  before.Description,
		Instructions: before.Instructions,
		Tags:         nonNil(before.Tags),
	}
	return reflect.DeepEqual(normalized, after)
}

// BumpVersion exports the increment of a string-form version number, for test assertions on the core side.
func BumpVersion(cur string) string { return bumpVersion(cur) }

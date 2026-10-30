// Syncs AMA skills to the Ark side: uploads the skill package and attaches it to the Ark Agent.
//
// # This layer covers "letting the model know the skill exists"
//
// It and skillinject.go are **two complementary paths**; a skill is incomplete if either is
// missing:
//
//	this file        → the Ark Agent's skills field  → the model **knows** the skill exists and when to use it
//	skillinject.go   → the session container's file system → tools **can read** the attachment bytes
//
// With only the latter, the model never knows the skill exists, so it never reads those files:
// they sit untouched in the container, with zero errors. With only the former, the model
// follows SKILL.md and reads a script that is not in the container; read returns "file does
// not exist", and the model just tries a few other paths and then gives up.
//
// # ⚠️ The two sides are **allowed on purpose to differ** in completeness
//
// Our own skill package limits (100 MiB / 10000 files, see skillpkg.go) are **looser than
// Ark's** (50 MiB / 500 files / 25 MiB per file; external hard limits that will not change
// with us). When a package exceeds any of Ark's three limits, **the whole package no longer
// fails; only SKILL.md is uploaded** (decided by BuildSkillZip):
//
//	Ark side        only SKILL.md         → the model still **knows** the skill exists and what it does
//	container side  all attachments       → tools can still **read** those bytes
//
// So "this skill has no attachments in the Ark console" is **expected, not a bug**. The model
// can still use it, because the paths written in SKILL.md all really exist in the container.
// An Info log explains the reason when this fallback happens.
//
// What this trade-off buys is "skill packages with thousands of files can be installed"; such
// packages used to be rejected outright.
//
// # The SDK's automatic skill install does nothing for us
//
// The SDK's EnvironmentWorker calls envinit.Setup for every work item
// (arkruntime/lib/environments/worker.go:200), which reads session.SkillRefs() and downloads
// and unpacks the Ark-side skill packages. So once skills are attached to the Agent, **the SDK
// fetches them automatically**; we do not have to write that step.
//
// But it lands on **the worker's own machine**: with SkillsDir empty it defaults to
// `<Workdir>/skills`, and the Workdir we pass is workerRootDir (/tmp/ama-worker-sessions).
// The six tools are all container versions (dockertools), and the agent sees the session
// container's file system; the two places have nothing to do with each other.
// envinit failures are also warn-only, so **there are zero errors end to end**.
//
// It also means some waste: every work item downloads and unpacks the skill packages onto the
// worker machine, and nobody reads them. Skill packages are only tens of KB, so we leave it
// for now, but do not assume those files are useful.
//
// # Failures do not block the conversation
//
// Upload or update failures only log a WARN and the conversation starts as usual; the model
// just does not know about the skill this turn. Conversations work without skills today, and
// dragging the main feature down for an add-on is a bad trade.
// Same handling as the container injection side (injectSkills failures are also only Warn).
package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// maxArkSkillsPerAgent is Ark's limit on the number of skills on a single Agent.
//
// See the Skills guide in the Ark Managed Agents docs: "a single Agent can have at most 20
// Skills", and the API reference also says "at most 20 items" for the skills field.
//
// **Stop it here instead of waiting for Ark to reject it**: when exceeded, Ark returns an
// English InvalidParameter, and that error shows up as "this agent cannot start a
// conversation", which does not point at "you bound 21 skills" at all.
const maxArkSkillsPerAgent = 20

// arkSkillsFor syncs the skills bound to an agent to Ark, and returns the references that can
// be attached to the Agent plus the fingerprint of this set of skills.
//
// Flow (once per skill):
//
//	compute content fingerprint → look up ark_skill_uploads
//	  hit  → use the ark_skill_id in the record (**zero network calls**, the normal case)
//	  miss → build zip → upload to Ark → store (first one wins) → use the winner's id
//
// The returned refs are in the same order as agent.SkillIDs, so the order in the Ark console
// matches the AMA UI.
//
// **A single failed skill is only skipped; the whole batch does not fail**: one broken skill
// package (e.g. attachments over the limit) should not make the other skills unusable. It does
// log a WARN, and the fingerprint is computed only over **the ones that really succeeded**.
// This matters: computing the fingerprint over "what should be there" would cache the failure;
// the next conversation would hit the fingerprint and not even retry, and that skill would be
// missing forever.
func (s *Service) arkSkillsFor(
	ctx context.Context, key ArkBindingKey, agent apimodel.Agent,
) ([]persistmodel.ArkSkillRef, string, error) {
	if len(agent.SkillIDs) == 0 {
		return nil, "", nil
	}
	if len(agent.SkillIDs) > maxArkSkillsPerAgent {
		// This one **returns an error** instead of truncating: truncating would make "the 21st
		// skill does not work" a silent behavior; the user sees 21 bindings in the UI but only
		// 20 take effect.
		return nil, "", fmt.Errorf(
			"%w: 这个 Agent 绑定了 %d 个技能，方舟侧最多 %d 个，请先解绑一些",
			ErrPrecondition, len(agent.SkillIDs), maxArkSkillsPerAgent)
	}

	log := s.log().With(
		slog.String("workspace", key.WorkspaceID), slog.String("agent", key.AgentID))

	refs := make([]persistmodel.ArkSkillRef, 0, len(agent.SkillIDs))
	perSkill := make(map[string]string, len(agent.SkillIDs))

	for _, id := range agent.SkillIDs {
		up, err := s.ensureArkSkill(ctx, key, id)
		if err != nil {
			// If a single skill cannot be synced, skip it. **Do not abort the batch**: the other
			// skills should still work.
			log.Warn("技能同步到方舟失败，这一轮模型不会知道它",
				slog.String("skill", id), slog.Any("err", err))
			continue
		}
		refs = append(refs, up.Ref())
		// The fingerprint only includes the successful ones; see the part of the function
		// comment about "computing over what should be there caches the failure forever".
		perSkill[id] = up.ContentHash
	}
	return refs, SkillsFingerprint(perSkill), nil
}

// ensureArkSkill makes sure the **current content** of an AMA skill has a matching skill
// package on the Ark side.
//
// If it was already uploaded (same content), return the record directly with no network calls.
// That is the vast majority of cases, because skill content rarely changes.
func (s *Service) ensureArkSkill(
	ctx context.Context, key ArkBindingKey, skillID string,
) (persistmodel.ArkSkillUpload, error) {
	sk, err := s.GetSkill(ctx, skillID)
	if err != nil {
		// Dangling reference: the skill was deleted but the binding on the agent was not cleaned
		// up. DeleteSkill cleans it, so getting here means some other path missed it (same
		// reasoning as the check in skillinject.go).
		return persistmodel.ArkSkillUpload{}, fmt.Errorf("技能不存在: %w", err)
	}
	// **Cross-workspace references must be blocked. This is not just dirty data, it is data
	// crossing a boundary.**
	//
	// `DeleteWorkspace` on purpose does not clean dangling references from agents in other
	// workspaces (the comment in store_pg.go explains it: to behave the same as
	// LocalApiClient), so agent.SkillIDs **really can** contain skill ids from another
	// workspace.
	//
	// Without the block: we would use workspace A's Ark key to upload the body and attachments
	// of a skill from workspace B, so B's content would appear in A's project, visible to A's
	// members in the Ark console. That is a silent unauthorized leak, not a functional problem
	// like "one skill missing".
	//
	// **Built-in platform skills are exempt from this check**: they belong to no workspace
	// (WorkspaceID is an empty string), and every workspace can see and select them, so
	// "owning workspace differs from calling workspace" does not apply to them.
	// Without the exemption, selecting one would never attach it, and arkSkillsFor only logs a
	// Warn for a single failed skill, so it shows up as "skill selected, the model knows
	// nothing", with zero errors.
	// A built-in skill's body is a code constant with no user data, so uploading it to any
	// project leaks nothing.
	if !IsBuiltinSkill(skillID) && sk.WorkspaceID != key.WorkspaceID {
		return persistmodel.ArkSkillUpload{}, fmt.Errorf(
			"技能属于空间 %s，不能在空间 %s 里使用", sk.WorkspaceID, key.WorkspaceID)
	}
	// The skill has our own real body and attachments, which must really be uploaded to Ark.
	// Otherwise it shows up as "skill installed and selected, but the model knows nothing",
	// with only one Warn log.
	// Attachments must carry their **content bytes**: the copy in doc only has path+size, and a
	// package built from it has 0 bytes for every attachment. Ark does not validate the format,
	// so **there is no error**; the agent just reads a bunch of empty files.
	//
	// Go through Service instead of store: built-in skills are not in the database, so the
	// store path would waste a query.
	files, err := s.SkillFiles(ctx, skillID)
	if err != nil {
		return persistmodel.ArkSkillUpload{}, fmt.Errorf("取技能附件失败: %w", err)
	}

	// # Also prepare the copy the container uses while we are here (prewarm)
	//
	// This step has nothing to do with Ark. It is here only because **right now we happen to
	// hold all the attachment bytes**, the most expensive thing in the whole chain (one PG
	// query + N concurrent object storage fetches).
	//
	// The timing is good: `CreateConversation` gets here at the moment the **session is
	// created**, while the container is only created when the user first really uses a tool
	// (lazy creation, measured ~20 seconds). There are often several text-only turns in
	// between. Moving "pack + upload to object storage" here leaves only "sign a string" for
	// the `ensureRunning` moment.
	//
	// ⚠️ **Only prewarm, do not sign the URL**: the signature TTL is 30 minutes, and the time
	// from session creation to the first tool call can be long (half an hour of plain text chat
	// is common). A link signed early would have expired by then, which is worse.
	// Only "pack and upload" can be done early, and that is the expensive part.
	//
	// ⚠️ **Failures only log at Debug and never block**: this is a pure optimization. If the
	// prewarm did not succeed, `ensureSkillArchive` does it again at injection time as usual
	// (idempotent; the key is the content fingerprint).
	// Logging at Warn would add a kind of noise that "actually has no consequences".
	s.warmSkillArchive(ctx, sk, files)

	hash := SkillFingerprint(sk, files)
	// An Ark skill belongs to the project of the key used to upload it, so "was it uploaded"
	// must be tracked per key.
	// See ArkSkillUpload.KeyFingerprint and the migration comment in schema_pg.sql.
	keyFP := ArkKeyFingerprint(key.APIKey)

	// First check: was this version of the content uploaded with this key.
	if up, err := s.store.GetArkSkillUpload(ctx, key.WorkspaceID, skillID, hash, keyFP); err == nil {
		return up, nil
	} else if !errors.Is(err, ErrNotFound) {
		// If the database cannot be read, do not go on: going on would upload again, and a
		// re-upload leaves a skill on the Ark side that cannot be cleaned up.
		// A database hiccup is fixed by retrying, so return an error here instead of forcing
		// the upload.
		return persistmodel.ArkSkillUpload{}, fmt.Errorf("查方舟技能上传记录失败: %w", err)
	}

	zipped, err := BuildSkillZip(sk, files)
	if err != nil {
		return persistmodel.ArkSkillUpload{}, err
	}
	if zipped.Degraded {
		// **Log at Info, not Warn**: this is an intended normal path, not a fault.
		// Logging at Warn would bury the question "is anything wrong in production" under a
		// pile of normal fallbacks.
		//
		// But it must be logged, with the consequence stated: in the Ark console this skill is
		// incomplete (only SKILL.md), while the container has all attachments. Without this log,
		// "why are there no attachments in the console" cannot be explained.
		s.log().Info("技能超出方舟上限，只把 SKILL.md 传给方舟（附件仍会进容器）",
			slog.String("skill", skillID), slog.String("name", sk.Name),
			slog.String("reason", zipped.Reason))
	}

	displayTitle := sk.DisplayTitle
	if displayTitle == "" {
		displayTitle = sk.Name
	}
	// The file name ends with .zip: the Ark server **uses it to detect the format** (the SDK's
	// fileName parameter goes straight into the multipart filename). Without the suffix it may
	// be treated as another format.
	arkID, version, err := s.runner.UploadSkill(
		ctx, key, sk.Name+".zip", displayTitle, zipped.Zip)
	if err != nil {
		return persistmodel.ArkSkillUpload{}, fmt.Errorf("上传技能包到方舟失败: %w", err)
	}

	// Store and read back.
	//
	// **Under concurrency one Ark skill leaks**: two instances both find "not uploaded" and
	// each uploads a copy, but only one can be stored. The losing copy becomes an orphan on the
	// Ark side, and Ark has no delete API, so it cannot be cleaned up.
	//
	// This is **accepted on purpose**: Ark skills use no runtime quota (they are not
	// containers), and each is tens of KB; the alternative (take a Redis lease before
	// uploading) would add a new failure path for a rare and harmless leak.
	// Reading back makes sure all instances of the same workspace end up pointing at the same
	// Ark skill (first one wins).
	up, err := s.store.PutArkSkillUpload(ctx, persistmodel.ArkSkillUpload{
		WorkspaceID:    key.WorkspaceID,
		SkillID:        skillID,
		ContentHash:    hash,
		KeyFingerprint: keyFP,
		ArkSkillID:     arkID,
		ArkVersion:     version,
		CreatedAt:      apimodel.NowISO(),
	})
	if err != nil {
		return persistmodel.ArkSkillUpload{}, fmt.Errorf("存方舟技能上传记录失败: %w", err)
	}
	if up.ArkSkillID != arkID {
		// We lost the race: the database has the copy someone else uploaded. The one we just
		// uploaded is now an orphan.
		s.log().Info("方舟技能并发上传，采用先落库的那份（我们这份成为孤儿）",
			slog.String("skill", skillID),
			slog.String("kept", up.ArkSkillID), slog.String("orphan", arkID))
	}
	return up, nil
}

// arkSkillIDsOf extracts the Ark skill ids from the references, only to store them in the
// binding record for debugging.
func arkSkillIDsOf(refs []persistmodel.ArkSkillRef) []string {
	if len(refs) == 0 {
		return nil
	}
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.ArkSkillID)
	}
	return out
}

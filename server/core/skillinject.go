// The implementation of "which skills does this session install", called back by
// shared/infra/dockertools when injecting into the container.
//
// # Why this part is in core and not in shelltools
//
// Because only core knows the chain it has to walk: the Ark sessionID **is** Run.ID (see the
// CreateConversation implementation in chat.go), Run records AgentID, Agent records SkillIDs,
// and a skill's attachments are in the skill_files table. infra/ does not import core (that
// would form a cycle and also tie the access layer to business models), so shelltools only
// declares a SkillLoader interface, the implementation lives here, and amad wires them together.
//
// # This step fills a gap the SDK leaves
//
// The SDK installs skills too (arkruntime/selfhosted/envinit), but into Workdir/skills/ on the
// **worker host**. We replaced all six tools with container versions, so the agent sees the
// container's file system. The two places have nothing to do with each other: the copy the SDK
// installs is invisible to the agent, and nothing reports an error.
package core

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/shared/hosttools"
	"github.com/chyroc/ama/server/shared/infra/shelltools"
)

// SkillFileLoader gets, for an Ark session, the skill files that session should install into
// the container.
//
// It implements shelltools.SkillLoader. **Service does not implement that interface directly**:
// that would make core depend on shared/infra/dockertools at the type level (Service is this
// package's core type, and one more dependency there has too wide an impact), while a small
// separate type only shows up at the wiring site.
type SkillFileLoader struct {
	svc *Service
	log *slog.Logger
}

// NewSkillFileLoader builds a loader. svc is required.
func NewSkillFileLoader(svc *Service, logger *slog.Logger) *SkillFileLoader {
	if logger == nil {
		logger = slog.Default()
	}
	return &SkillFileLoader{svc: svc, log: logger}
}

// Compile-time check that it satisfies the interface on the shelltools side.
//
// **This line must not be dropped.** The interface is declared in another package; without
// this assertion a signature mismatch only shows up at the amad wiring line. Worse, if the
// wiring site uses a loose form like interface{}, nothing reports an error at all, and it shows
// up as "injection is configured but never took effect once".
var _ shelltools.SkillLoader = (*SkillFileLoader)(nil)

// SkillsFor looks up which skills an Ark session should install.
//
// Chain: arkSessionID == Run.ID → Run.AgentID → Agent.SkillIDs → each skill's attachments.
//
// **If any link is not found it returns empty, not an error**, because the caller of this path
// is the container creation flow: returning an error only adds a Warn to the log, while "this
// session has no skills" is the most common normal case (hand-written skills have no
// attachments, and most agents have no skill bound at all). Reporting the normal case as an
// error fills the log with noise and buries the real failures.
func (l *SkillFileLoader) SkillsFor(
	ctx context.Context, arkSessionID string,
) ([]shelltools.SkillBundle, error) {
	// Docker session container: skill package links are signed for an address the container
	// can reach (s3.sandboxEndpoint).
	return l.skillsFor(ctx, arkSessionID, LinkForSandbox)
}

// skillsFor is the shared implementation of SkillsFor / HostSkillsFor. reader only affects who
// the skill package link is signed for.
func (l *SkillFileLoader) skillsFor(
	ctx context.Context, arkSessionID string, reader LinkReader,
) ([]shelltools.SkillBundle, error) {
	// The Ark sessionID is Run.ID: CreateConversation uses the id returned by CreateSession
	// directly as Run.ID (chat.go). This is not a coincidence but on purpose; otherwise we
	// would need another mapping table here.
	run, err := l.svc.store.GetRun(ctx, arkSessionID)
	if err != nil {
		// Not finding the run is normal: the Feishu path and the probe scripts both create
		// Ark sessions without storing a Run.
		return nil, nil
	}
	if run.AgentID == "" {
		return nil, nil
	}

	agent, err := l.svc.store.GetAgent(ctx, run.AgentID)
	if err != nil {
		// The agent was deleted but an old conversation is still being continued; there are no
		// skills to install then, which is not a failure.
		return nil, nil
	}
	if len(agent.SkillIDs) == 0 {
		return nil, nil
	}

	bundles := make([]shelltools.SkillBundle, 0, len(agent.SkillIDs))
	for _, id := range agent.SkillIDs {
		// Go through Service rather than store: built-in skills are not in the DB, so store would
		// treat them as dangling references and log a Warn. **They must be written to disk
		// too**; see "skills without attachments still need a directory" below.
		sk, err := l.svc.GetSkill(ctx, id)
		if err != nil {
			// Dangling reference: the skill was deleted but the binding on the agent was not
			// fully cleaned up. DeleteSkill does clean it, so reaching here means some other
			// path missed it. Log it, but do not affect the other skills.
			l.log.Warn("会话绑定的技能不存在，跳过",
				slog.String("ark_session", arkSessionID), slog.String("skill", id))
			continue
		}

		// # Skills without attachments still need a directory under the skills directory
		//
		// This used to be `if len(sk.Files) == 0 { continue }`, so **hand-written skills and
		// the three built-in skills had no directory at all in the container**. Whether the
		// skill directory (`<workdir>/skills/<skill name>/`, see shelltools.SkillsRootDir)
		// exists is exactly how the model (and the agent frameworks in the container that scan
		// directories by convention) decides "does this skill exist": no directory = no skill.
		// The user report where `ls -la /mnt/skills/ama/` returned
		// `No such file or directory` was exactly this (back then the location was still
		// /mnt/skills), because ama is a built-in skill with zero attachments.
		//
		// So now **every bound skill writes out SKILL.md** (the body comes from Instructions,
		// which is exactly the SKILL.md content; see the field comment on apimodel.Skill), and
		// skills with attachments also write the attachments. The cost is one extra file of a
		// few KB per skill; in exchange we get the invariant "if it is bound, it is visible".
		//
		// ⚠️ Write it even when the body is empty: write a SKILL.md with only a title, so that
		// **the directory exists** holds. An empty file beats no directory: with the former the
		// model reads "this skill has no body", with the latter it concludes "no such skill".
		files := make([]shelltools.SkillFile, 0, len(sk.Files)+1)
		files = append(files, shelltools.SkillFile{
			Path:    "SKILL.md",
			Content: []byte(renderSkillMD(sk)),
		})

		if len(sk.Files) > 0 {
			// Files in doc only has path + size (Content has omitempty, see
			// apimodel.SkillFile); the real bytes need another lookup in skill_files.
			//
			// ⚠️ **Go through Service.SkillFiles, not store.SkillFiles**: the bytes may be in
			// object storage (the ones with a non-empty file_id), and the store layer only gives
			// the content column. Bypassing it shows up as "object storage has it, but the
			// container reads an old/empty copy", with zero errors.
			// Concurrent fetching and per-file fallback are both in the Service layer
			// (hydrateSkillFiles).
			stored, err := l.svc.SkillFiles(ctx, id)
			if err != nil {
				// Report it upward; the upper layer logs a Warn and keeps the container usable.
				// Two cases can reach here:
				//
				//  1. the DB itself has a problem;
				//  2. **some file cannot be fetched from object storage**; then hydrateSkillFiles
				//     prefers failing the whole batch over handing out an incomplete skill
				//     (incomplete is worse than missing; see that function's comment).
				//
				// ⚠️ Object storage is the only source of skill file bytes; if it cannot be
				// read, the whole skill is unusable.
				return nil, fmt.Errorf("取技能 %s 的附件失败: %w", id, err)
			}
			if len(stored) == 0 {
				// doc says there are attachments but the table has none: the two disagree.
				// CreateSkillFromPackage creates the skill first, then writes attachments, and
				// rolls back if writing fails, so in theory this should not happen; if it does,
				// say so. **But still write SKILL.md**: the skill itself exists, only its
				// attachments are lost.
				l.log.Warn("技能声明了附件但库里没有，只写 SKILL.md",
					slog.String("skill", id), slog.Int("declared", len(sk.Files)))
			}

			// # Skills with attachments use "the container downloads an archive directly"
			//
			// Attachment bytes are no longer stuffed into the shell command line one by one as
			// base64: that path is limited by MAX_ARG_STRLEN (128 KiB), and a skill package of
			// tens of MB simply does not fit. Instead the whole package goes into object
			// storage, and the container gets a time-limited URL to curl down and unpack itself.
			//
			// **Failure only downgrades, it does not block**: when no link can be signed (object
			// storage not configured, object storage hiccup) archive is nil, and below the
			// attachments are put into Files one by one along the old path as before.
			// The old path is fully enough for small skills and fails on the container side for
			// large ones, but that is still mild compared with "the whole session cannot
			// start", and the injection side logs it.
			if archive, err := l.svc.ensureSkillArchive(ctx, sk, stored, reader); err != nil {
				// ⚠️ Only log a Warn, do not report upward: reporting upward makes SkillsFor fail
				// entirely, and then **not a single skill directory gets created** (the provider
				// does not inject when it gets an error). Falling back to base64 at least keeps
				// small skills working.
				l.log.Warn("技能包上传/签名失败，回落到逐文件注入（大技能可能塞不进去）",
					slog.String("skill", id), slog.Any("err", err))
			} else if archive != nil {
				// The fallback SKILL.md **must also use the user's copy** (if the package has one).
				//
				// ⚠️ Without this there is a very subtle inconsistency: on a successful download
				// the container has **the user's copy** (from the package), but on fallback it
				// becomes **the one we synthesized from Instructions**. The two may differ (the
				// user's copy may have front-matter and other sections), so "the same skill has
				// SKILL.md A sometimes and B other times in the container", and the switch
				// condition is "did the download fail"; nobody would think to look there.
				fallback := files
				for _, f := range stored {
					if f.Path == "SKILL.md" {
						fallback = []shelltools.SkillFile{{Path: "SKILL.md", Content: f.Content}}
						break
					}
				}
				bundles = append(bundles, shelltools.SkillBundle{
					Name: sk.Name,
					// Only carry SKILL.md as a **fallback**: if the container download fails it
					// gets written, so the invariant "a bound skill always has a visible
					// directory" holds. On a successful download the injection side skips it and
					// uses the copy in the package.
					Files:        fallback,
					ArchiveURL:   archive.URL,
					ArchiveBytes: archive.Bytes,
				})
				continue
			}

			for _, f := range stored {
				// The uploaded package may include its own SKILL.md; then **the user's copy
				// wins** and replaces the one we generated from Instructions. Otherwise the same
				// path is written twice in one batch of commands, and the final content depends on
				// the order (effectively random).
				if f.Path == "SKILL.md" {
					files[0] = shelltools.SkillFile{Path: "SKILL.md", Content: f.Content}
					continue
				}
				files = append(files, shelltools.SkillFile{Path: f.Path, Content: f.Content})
			}
		}

		bundles = append(bundles, shelltools.SkillBundle{Name: sk.Name, Files: files})
	}
	return bundles, nil
}

// renderSkillMD renders a skill into SKILL.md content.
//
// Instructions is the SKILL.md body (see the field comment on apimodel.Skill), so when there is
// a body it is **returned as is, with no wrapping**. Wrapping would break things like the
// front-matter the user uploaded, and that breakage gives zero errors (the model just reads a
// document of the wrong shape).
//
// When the body is empty, fall back to a minimal Markdown: the only goal is for the directory
// and file to exist, so "a bound skill" does not become "no such skill in the container". The
// description is included because it decides when the model uses this skill.
func renderSkillMD(sk apimodel.Skill) string {
	if strings.TrimSpace(sk.Instructions) != "" {
		return sk.Instructions
	}
	title := sk.DisplayTitle
	if strings.TrimSpace(title) == "" {
		title = sk.Name
	}
	md := "# " + title + "\n"
	if strings.TrimSpace(sk.Description) != "" {
		md += "\n" + sk.Description + "\n"
	}
	return md
}

// HostSkillsFor is the native host-machine version of SkillsFor: it converts the shelltools
// shape (file content []byte) into the hosttools shape (content string, sent via JSON rather
// than shell argv). The loading logic, fallback SKILL.md, archive and cache key are exactly the
// same in both versions; only the wire format differs.
func (l *SkillFileLoader) HostSkillsFor(
	ctx context.Context, arkSessionID string,
) ([]HostSkillBundle, error) {
	// ⚠️ The user's own machine is outside, so skill package links must be signed for the
	// public address (s3.publicEndpoint); the ones SkillsFor signs for the session container
	// cannot be reused: the Host differs and the signature would not verify.
	bundles, err := l.skillsFor(ctx, arkSessionID, LinkForPublic)
	if err != nil {
		return nil, err
	}
	out := make([]HostSkillBundle, 0, len(bundles))
	for _, b := range bundles {
		hb := HostSkillBundle{
			Name:         b.Name,
			ArchiveURL:   b.ArchiveURL,
			ArchiveBytes: b.ArchiveBytes,
		}
		for _, f := range b.Files {
			hb.Files = append(hb.Files, hosttools.SkillFile{Path: f.Path, Content: string(f.Content)})
		}
		out = append(out, hb)
	}
	return out, nil
}

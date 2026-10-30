// The "direct download into the container" path for skill packages: pack a skill into a zip,
// put it in object storage, and sign a time-limited URL for the container.
//
// # Why this path exists
//
// Injecting into the container used to have only one path: base64 + shell redirection. It is
// limited by `MAX_ARG_STRLEN` (128 KiB), and a skill package of tens of MB simply does not fit
// (see maxInjectBatchBytes in shared/infra/shelltools/skills.go).
//
// With object storage, the bytes go in a straight line **object storage → container**,
// unrelated to command-line length, and amad only signs a string.
// This assumes the container side can reach object storage directly.
//
// # Why not every skill uses this path
//
// Skills with zero attachments (the three built-in skills, hand-written skills) only have one
// SKILL.md of a few KB. Packing it, uploading to object storage, then curl + unzip in the
// container is much slower than writing it in directly with base64, and adds two failure
// points for nothing (object storage write fails, container download fails). So the rule is
// **does it have attachments**; see SkillsFor.
//
// # Cache: the key is the content hash, so it is naturally idempotent
//
// The object key is `skill-archives/<yyyy>/<mm>/<content fingerprint>.zip`, and the fingerprint
// is computed by SkillFingerprint (body + path and bytes of each attachment). So:
//
//   - skill unchanged → key unchanged → already in object storage, skip upload (one Exists HEAD)
//   - skill changed → key changed → upload a new one, keep the old one (the section below
//     explains why it is not deleted)
//
// **A skill used by N sessions is uploaded only once**, while this path runs every time a
// container is created.
//
// ⚠️ Old packages are **deliberately not deleted**: if they were, a running container that
// needs to download again (snapshot restore, container rebuild) could not get them, while the
// URL it holds has not expired yet; it shows up as "the skill suddenly cannot be read".
// Expiring them is left to object storage lifecycle rules, not something we delete on the side
// in the write path.
package core

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
)

// blobPrefixSkillArchive is the object storage prefix for skill archives.
//
// **Separate from blobPrefixSkillFile (single attachment bytes)**: the two have different
// lifecycles. Attachments are the source of truth and must never be cleaned automatically,
// while archives are rebuildable derived artifacts, so the object storage side can give them a
// rule like "delete after N days without access". Under one shared prefix that rule would
// take the source of truth away too.
const blobPrefixSkillArchive = "skill-archives"

// skillArchiveURLTTL is how long the download link given to the container stays valid.
//
// # Why 30 minutes and not shorter
//
// The URL is signed **at the moment the container is created**, and the container downloads
// it afterwards. In between are container creation (measured ~12 seconds) and possible
// queueing. 30 minutes is there to cover the long tail of "container creation is slow".
//
// # Why not longer
//
// It is a time-limited pass: whoever gets it can read. The shorter the window the better, and
// 30 minutes is already far longer than one injection needs (the whole path measured in
// seconds).
//
// ⚠️ **Do not raise it so that "a rebuilt container can reuse it"**: a rebuild goes through
// injection again and signs a new one; reusing an old URL is not part of the design.
const skillArchiveURLTTL = 30 * time.Minute

// BlobPresigner is the ability to "sign time-limited download links".
//
// # Why a separate interface instead of adding it to BlobStore
//
// BlobStore is the minimal surface that **all attachments** need (issue images, suggestion
// attachments); signing links is only useful for the skill package path. Adding it there would
// force every implementation (including test doubles) to write it once more.
//
// ⚠️ **But "optional" does not mean "may be silently absent".** We hit this once: the core side
// added the assertion, the adapter in `cmd/amad` forgot to implement it, so the assertion gave
// `ok == false` and `ensureSkillArchive` returned nil right away. **Even with object storage
// configured in production it always took the base64 fallback**, which was exactly the problem
// that round was meant to solve, with zero errors the whole time and no compile error either.
//
// So these two interfaces are exported, so the wiring layer can write a compile-time assertion
// (`var _ core.BlobPresigner = (*blobStore)(nil)`). **Comments alone cannot guard this.**
type BlobPresigner interface {
	// PresignedGetURL signs a download link valid within ttl, **for the party reader names**.
	PresignedGetURL(key string, ttl time.Duration, reader LinkReader) (string, error)
}

// LinkReader says who a presigned link is made for.
//
// # Why split them
//
// S3 presigning (SigV4) signs the Host in, so a link can only be used against the address it
// was signed for. The two kinds of readers can reach different object storage addresses:
//
//	LinkForSandbox  Docker session container (on ama-net together with object storage, uses the internal service name)
//	LinkForPublic   the user's own machine (ama daemon, outside, uses the public address)
//
// Signing the wrong one shows up as the container / daemon getting 403 or failing to connect
// when downloading the skill package, and then **silently falling back to writing only
// SKILL.md** (all of the skill's attachments missing), with just one Warn. See the package
// comment of shared/infra/s3.
type LinkReader int

const (
	// LinkForSandbox is for the Docker session container (config s3.sandboxEndpoint).
	LinkForSandbox LinkReader = iota
	// LinkForPublic is for browsers and the user's own machine (config s3.publicEndpoint).
	LinkForPublic
)

// BlobSizer is the ability to "get only the object size, not the bytes".
//
// Same reasoning as BlobPresigner (see above).
//
// ⚠️ Implementations must translate "object does not exist" into ErrBlobNotFound, otherwise
// skillArchiveSize cannot recognize a cache miss.
type BlobSizer interface {
	// Size gets the object's byte count; when the object does not exist it returns an error
	// that errors.Is(err, ErrBlobNotFound) recognizes.
	Size(ctx context.Context, key string) (int64, error)
}

// skillArchive is where a skill's archive lives in object storage, and its size.
type skillArchive struct {
	// URL is the presigned download link. ⚠️ It carries a signature; do not write it into logs
	// or environment variables.
	URL string
	// Bytes is the package size in bytes; the container side uses it to check "was it fully
	// downloaded".
	Bytes int
}

// ensureSkillArchive makes sure the current content of a skill has a matching zip in object
// storage, and signs a download link.
//
// files must carry content bytes (the kind Service.SkillFiles returns).
//
// reader decides who the link is signed for (session container or the user's own machine);
// see LinkReader.
//
// When no object storage is wired (unit tests) or the wired implementation cannot sign links
// (MemBlobStore) it returns (nil, nil). **That is not an error**: the caller then falls back to
// per-file base64 injection, which is still fully functional, just unable to fit large packages.
func (s *Service) ensureSkillArchive(
	ctx context.Context, sk apimodel.Skill, files []apimodel.SkillFile, reader LinkReader,
) (*skillArchive, error) {
	if !s.blobEnabled() {
		return nil, nil
	}
	signer, ok := s.blobs.(BlobPresigner)
	if !ok {
		// The BlobStore interface has no signing ability (that lives on the concrete
		// implementation). If the assertion fails, treat this path as absent: fall back to
		// base64, no error.
		return nil, nil
	}

	// The fingerprint is the main part of the key: the same content always gives the same key,
	// so it is naturally idempotent. **Reuse SkillFingerprint instead of computing another
	// one**: it already covers "all fields that affect the package content" (body + path and
	// bytes of each attachment), and a second one would surely drift.
	hash := SkillFingerprint(sk, files)
	key := attachmentObjectKey(blobPrefixSkillArchive, hash+".zip", apimodel.NowISO())

	// If it was already uploaded, do not pack again. This is by far the most common case
	// (skill content rarely changes, while this path runs every time a container is created).
	// Packing a 100 MiB skill is not cheap.
	size, err := s.skillArchiveSize(ctx, key)
	if err != nil {
		return nil, err
	}
	if size < 0 {
		// ⚠️ **BuildFullSkillZip, not BuildSkillZip.**
		//
		// The latter decides downgrades by **Ark's** three limits (50 MiB / 500 files / 25 MiB
		// per file), while the container side has none of those limits; it is exactly the end
		// that needs the complete skill.
		//
		// We measured the cost of using the wrong one: any skill over Ark's limits gets no
		// package here → skillinject falls back to base64 → and base64 cannot fit large packages
		// (60 MiB takes 853 commands, about 12.8 minutes). The net effect is that "raising the
		// limit to 100 MiB" does nothing for the range above 25 MiB, with only one Warn.
		zip, err := BuildFullSkillZip(sk, files)
		if err != nil {
			return nil, fmt.Errorf("打技能包失败: %w", err)
		}
		if err := s.blobs.Put(ctx, key, zip, "application/zip"); err != nil {
			return nil, fmt.Errorf("技能包写对象存储失败: %w", err)
		}
		size = len(zip)
	}

	url, err := signer.PresignedGetURL(key, skillArchiveURLTTL, reader)
	if err != nil {
		return nil, fmt.Errorf("签技能包下载链接失败: %w", err)
	}
	return &skillArchive{URL: url, Bytes: size}, nil
}

// skillArchiveSize checks whether a package is in object storage, and returns its byte count
// if so; returns -1 if not.
//
// ⚠️ **It needs the byte count, not just "is it there"**: the container side uses it to check
// download integrity (when the connection drops, curl leaves an incomplete but existing file
// and exits 0). If we only checked existence, the cache-hit path would have no Bytes and the
// check would have to be skipped, and that is exactly the case that needs it most (only large
// packages go through the cache).
func (s *Service) skillArchiveSize(ctx context.Context, key string) (int, error) {
	sizer, ok := s.blobs.(BlobSizer)
	if !ok {
		// The BlobStore interface has no Size. Fall back to "treat as not present"; the cost is
		// re-packing and uploading every time (idempotent, same content), and it still works.
		return -1, nil
	}
	n, err := sizer.Size(ctx, key)
	if err != nil {
		if isBlobNotFound(err) {
			return -1, nil
		}
		return 0, fmt.Errorf("查技能包失败: %w", err)
	}
	return int(n), nil
}

// warmSkillArchive packs the skill package the container will need and uploads it to object
// storage ahead of time.
//
// # Which part it saves
//
// When injecting into the container, `ensureSkillArchive` does four things: compute the
// fingerprint → check whether object storage has it → if not, pack and upload → sign the URL.
// **Only the third is expensive** (packing 25 MiB measured 62ms, uploading takes longer), and
// it does not need the container at all.
//
// It is called from `ensureArkSkill` (skillark.go), which runs when **creating the session**,
// while the container is only created when the user first really uses a tool (lazy creation,
// measured ~20 seconds). Between the two there are often several rounds of text-only
// conversation, so the time this step takes is completely invisible to the user.
//
// After warming, at injection time `ensureSkillArchive` takes the "cache hit" path:
// one HEAD + signing one string.
//
// # Why it signs no URL and returns nothing
//
// The signature TTL is only 30 minutes, while the gap between creating the session and the
// first tool call can be long (chatting text-only for half an hour is common). A link signed
// ahead of time would have expired by then, which is worse than not signing. So this only
// does "put the package in object storage"; the URL is still signed fresh at injection time.
//
// No return value is needed either: `ensureSkillArchive` uses the **content fingerprint** as
// the key, so warming and injection naturally point at the same object, and no state needs to
// be passed.
//
// # Why failure only logs at Debug
//
// This is purely an optimization. If warming did not succeed, injection just does it fresh as
// usual (idempotent), and the user notices nothing. Logging a Warn would add a kind of noise
// that "actually has no consequence" and drown out the real anomalies.
func (s *Service) warmSkillArchive(ctx context.Context, sk apimodel.Skill, files []apimodel.SkillFile) {
	// Skills without attachments never take the archive path (they use base64, see
	// skillinject.go), so no warming is needed.
	if len(files) == 0 {
		return
	}
	if _, err := s.ensureSkillArchive(ctx, sk, files, LinkForSandbox); err != nil { // only warm the object; discard the signed link
		s.log().Debug("技能包预热失败，注入时会重试（不影响功能）",
			slog.String("skill", sk.ID), slog.Any("err", err))
	}
}

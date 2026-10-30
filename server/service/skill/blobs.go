// Reading and writing skill attachment bytes in object storage (BlobStore).
//
// This file **was moved as a whole** from the skill half of core/blob.go (step 11b), with not a
// line of logic changed. The attachment half (issue/suggestion attachments) stays in core: those go
// through the filebytes path and have nothing to do with this domain.
//
// # All three aliases point to the real thing in shared/infra/filebytes
//
// objectKey / contentSHA256 / isBlobNotFound are **the same functions** as on the core side
// (filebytes.ObjectKey and so on), not copies. The shape of the object key is "the only clue for
// finding those bytes", and once two implementations drift, old objects can never be found again;
// so there is only one real implementation, and both sides alias it (the same argument as on
// core/blob.go).
package skill

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/shared/infra/filebytes"
)

// objectKey builds the object storage key of an attachment.
//
// The real thing is in shared/infra/filebytes (`filebytes.ObjectKey`). The shape
// `<purpose>/<yyyy>/<mm>/<fileID>` and the reason for each of the three segments are written there
// and in the object_key comment of files.sql. ⚠️ Do not write a second implementation here.
var objectKey = filebytes.ObjectKey

// contentSHA256 computes the content hash, lowercase hex. See the content_sha256 comment in files.sql for its use.
var contentSHA256 = filebytes.ContentSHA256

// isBlobNotFound tells "the object does not exist", used to classify log wording. The real thing is in filebytes.
var isBlobNotFound = filebytes.IsBlobNotFound

// blobPrefixSkillFile is the object prefix for skill attachment bytes.
//
// It sits alongside the two attachment prefixes (issue-attachments / suggestion-attachments, in the
// core half) without sharing: the storage side needs to measure usage and configure lifecycle rules
// separately, and once merged, splitting again would mean moving objects.
const blobPrefixSkillFile = "skill-files"

// putSkillFileBlobs writes the bytes of a batch of skill files into object storage and fills in each
// file's FileID.
//
// # Two differences from putAttachmentBlob
//
//  1. **Batch.** Skill files are replaced in full each time (PutSkillFiles deletes then inserts), so
//     the whole batch is handled here at once, instead of each of the 4 call sites looping on its
//     own; missing one would mean silently not writing.
//  2. **The key is newly generated every time**, so updating a skill package leaks old objects. See
//     the section below.
//
// # ⚠️⚠️ Write failures are **not swallowed**; fail fast
//
// Skill file bytes **only exist in object storage** (skill_files only records path + file_id). So an
// empty FileID really means **the bytes have nowhere to go**, and swallowing it is silent data loss.
// The invariant is:
//
//	returns nil   ⟹ every file's FileID is non-empty and all bytes are in object storage
//	returns error ⟹ the caller aborts the upload, and **not a single byte is stored**
//
// ⚠️ **The cost is real**: one storage blip and uploading a skill fails. When bytes have only one
// place to go, failing to put them there must be an error, not a pretend success. Better to make the
// user retry once than to accept a skill with empty content: the container injection path
// **degrades silently** (core/skillinject.go); an empty skill gives no error, it just makes the agent
// read a skill with every directory present and every file empty.
//
// # ⚠️ Error immediately when BlobStore is not wired
//
// Without object storage there is nowhere to put the bytes, so this returns ErrPrecondition (→ 422)
// and the upload fails on the spot. cmd/amad refuses to start in the PG tier when object storage is
// not configured, so production never hits this.
//
// # ⚠️ It leaks faster than attachments, because skill files are "replaced in full"
//
// PutSkillFiles deletes then inserts: **every skill package update generates new file_ids for all
// files**, and the old files rows and old objects are left with nobody cleaning them (the same
// trade-off as attachments: better to leak than to corrupt; see the files.sql header).
//
// But the scale differs: attachments are "uploaded once, rarely touched", skill files are "a new
// batch every time the package changes". A 15-file skill updated 10 times is 150 objects, 135 of
// them garbage.
//
// No cleanup now because skill packages are tens of KB, so the absolute leak is small. If it is
// ever collected, the right way is the reconciliation job not written yet (list the object prefix →
// compare with the files table → delete unreferenced objects), not deleting here: deleting remote
// objects on the write path means data is gone once something is deleted by mistake; the object
// deleted by mistake is **one someone reads**, and it is the **only** copy.
func (s *Service) PutSkillFileBlobs(
	ctx context.Context, skillID, author string, files []apimodel.SkillFile,
) ([]apimodel.SkillFile, error) {
	if len(files) == 0 {
		return files, nil
	}
	blobs := s.blobs()
	if blobs == nil {
		// ⚠️ The check comes after len(files) == 0: when there are no files to write, it should not
		// fail because "storage is not wired"; that would also make "skill attachments go from some to
		// none" (clearing with an empty slice) an error, and that path does not need object storage
		// at all.
		return nil, fmt.Errorf(
			"%w: 没有装配文件存储（BlobStore），无法保存技能文件字节", persistmodel.ErrPrecondition)
	}

	out := make([]apimodel.SkillFile, len(files))
	copy(out, files)

	createdAt := apimodel.NowISO()
	for i := range out {
		fileID := s.nextID("file")
		key := objectKey(blobPrefixSkillFile, fileID, createdAt)

		// ⚠️ contentType is an empty string: skill files are of every type (md / png / sh), and this
		// path **does not go through the attachment type sniffing** (DetectAttachmentType). Rather than
		// guess a type that may be wrong, write none: a wrong Content-Type is worse than none, because
		// it makes people think the field can be trusted.
		//
		// The read path does not need sniffing either: reading only takes the bytes (blobs.Get
		// returns []byte), and injecting into the container writes files by path, **never looking at
		// Content-Type**.
		if err := blobs.Put(ctx, key, out[i].Content, ""); err != nil {
			// ⚠️ **Report it up, do not swallow it**: the bytes have no second place to go.
			// Include the path in the error: which one of a batch of 64 files failed must be visible
			// in the logs.
			return nil, fmt.Errorf("skill: 技能文件写对象存储失败（%s）: %w", out[i].Path, err)
		}

		rec := persistmodel.FileRecord{
			ID:        fileID,
			Bucket:    blobs.Bucket(),
			ObjectKey: key,
			// Filename stores the **path**, not the basename: a skill file's identity is its relative
			// path (the primary key is (skill_id, path)); storing only the basename would make
			// references/a.png and scripts/a.png look the same in the files table.
			Filename:      out[i].Path,
			ContentType:   "",
			Size:          len(out[i].Content),
			ContentSHA256: contentSHA256(out[i].Content),
			Author:        author,
			CreatedAt:     createdAt,
		}
		if err := s.store.CreateFile(ctx, rec); err != nil {
			// Same as above: report it up. The object has already been written, so an orphan object
			// is left here; **no rollback delete**, same as putAttachmentBlob: the delete itself can
			// also fail, and adding another remote call on a path that has already failed does no good.
			// Orphan objects are part of the "better to leak than to corrupt" trade-off.
			return nil, fmt.Errorf("skill: 技能文件元信息落库失败（%s）: %w", out[i].Path, err)
		}
		out[i].FileID = fileID
	}
	return out, nil
}

// skillBlobFetchConcurrency is the limit on fetching skill file bytes concurrently.
//
// # Why concurrency instead of one Get at a time
//
// Container injection (core/skillinject.go) **reads all files of a skill on every session**.
// Fetching serially is N network round trips end to end, while this path used to need only one SQL
// query.
//
// For remote object storage, fetching 15 files serially versus with concurrency 8 can differ by
// more than 4x.
//
// # Why 8 and not "all at once"
//
// One skill can have up to 10000 files (MaxSkillFiles), so "all at once" means **ten thousand
// goroutines**; that does not just amplify load, it can kill the connection pool and the downstream
// together. The value 8: gains flatten out after 8 (fetching is IO-bound, and the connection pool
// has a limit too). Deliberately bounded concurrency, like the sem in core/worker.go.
const skillBlobFetchConcurrency = 8

// hydrateSkillFiles fills in the bytes for a batch of skill files: **the bytes only exist in object
// storage, and failing to get them is an error**.
//
// # Relation to attachmentBytes: the same rule, batch version
//
// The per-item fallback rule is exactly the same as attachmentBytes (in the core half); there are
// only two differences, both forced by how **hot** this path is:
//
//  1. **Metadata is fetched in one query** (store.GetFiles), not once per file. Looping GetFile would
//     turn one PG query into N, which is exactly what this round removes.
//  2. **Bytes are fetched concurrently**, up to skillBlobFetchConcurrency.
//
// # ⚠️ An empty file_id is an **error**
//
// skill_files has no content column, so an empty file_id means **nowhere has this file's bytes**,
// and it must be an error, not a zero-byte file handed out, which would be silently injected into
// the container (see the next section).
//
// # The input is exactly the return value of store.SkillFiles: only path and FileID, no bytes
//
// That layer returns no content, so Content is always nil. This is the **only** place that can fill
// in the bytes, and if it cannot, it must report up.
//
// # ⚠️ Never return a skill that is "incomplete but looks complete"
//
// Failure on the container injection path **degrades silently** (the function comment in
// core/skillinject.go: any missing piece returns empty instead of an error), so "got 20 of 21
// files" means the model reads a skill with all directories present, SKILL.md present, but one
// references/*.md missing; **that is worse than the whole skill not existing**, because the latter
// is a state that can be noticed.
//
// So the approach is "one failure fails the whole skill": if object storage cannot give it, it
// cannot, and there is no second place to read from. starved is the only thing that stops
// incomplete skills; **never delete it because it looks like dead code**;
// TestSkillFilesRefusesToReturnPartialSkill pins it, and it was mutation-tested.
//
// # ⚠️ The source of truth for Size is `files.size`, and it can only be that
//
// `store.SkillFiles` returns no content, so the Content it returns is always nil and Size is always
// 0. And Size flows to two places users can see: `doc.Files[i].Size` (the return value of
// `ListSkills`; in the UI, "how many files and how big is this skill") and the read-modify-write
// in `UpdateSkillFile`.
//
// So here it is taken from the `files.size` column (`recs[id].Size`, written by the write path); it
// is the **only** source.
//
// ⚠️ This **must be done outside the store layer**: `PGStore.SkillFiles` does not query the `files`
// table (it only does `select path, file_id`), so it cannot get size. This is the only place that
// holds both "bytes" and "metadata".
func (s *Service) HydrateSkillFiles(
	ctx context.Context, files []apimodel.SkillFile,
) ([]apimodel.SkillFile, error) {
	if len(files) == 0 {
		return files, nil
	}
	blobs := s.blobs()
	if blobs == nil {
		// ⚠️ When storage is not wired, **do not return as is**: that would hand out a batch of
		// zero-byte files, and the container injection path degrades silently; the agent would read a
		// skill with all directories present and all contents empty, with no error. So error here.
		return nil, fmt.Errorf(
			"%w: 没有装配文件存储（BlobStore），无法读取技能文件字节", persistmodel.ErrPrecondition)
	}

	recs, err := s.store.GetFiles(ctx, skillFileIDs(files))
	if err != nil {
		return nil, fmt.Errorf("skill: 批量查技能文件元信息失败: %w", err)
	}

	out := make([]apimodel.SkillFile, len(files))
	copy(out, files)

	// Writes by index, no lock: each goroutine only touches its own out[i] slot.
	// Same shape as IntegrationRegistry.List.
	//
	// starved collects the paths whose "bytes cannot be fetched"; **all** of them fail the whole
	// batch, because there is no second place to read from. A lock rather than index writes:
	// normally it is empty, and saving one lock for an empty slice is not worth making concurrent
	// writes more complex.
	var (
		mu      sync.Mutex
		starved []string
	)
	sem := make(chan struct{}, skillBlobFetchConcurrency)
	var wg sync.WaitGroup
	for i := range out {
		id := strings.TrimSpace(out[i].FileID)
		if id == "" {
			// ⚠️ **An empty file_id is an error**: this file's bytes are not stored anywhere.
			// The error makes it visible; never hand out a zero-byte file.
			// No lock in this branch: we are not in a goroutine yet.
			starved = append(starved, out[i].Path)
			continue
		}
		rec, ok := recs[id]
		if !ok {
			// The files row is gone but skill_files still points to it. The data is inconsistent, and
			// **there is nowhere to fall back to**: error, as above.
			starved = append(starved, out[i].Path)
			continue
		}

		wg.Add(1)
		go func(i int, rec persistmodel.FileRecord) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			content, err := blobs.Get(ctx, rec.ObjectKey)
			if err != nil {
				// ⚠️⚠️ **Fail the whole batch; no fallback.**
				//
				// If it cannot be fetched, it cannot; this is the only place that stops incomplete
				// skills (see the function comment).
				//
				// Object not found (ErrBlobNotFound) and network failure are **handled the same**: both
				// mean "these bytes cannot be had", and the right reaction does not depend on telling
				// them apart. The difference only shows in the log wording.
				//
				// ⚠️ The log includes rec.Size (the size recorded in the files table), **not**
				// out[i].Size: the latter is always 0 at this point (the store layer does not select
				// content), and printing it would just be a useless 0.
				lvl := "技能文件读对象存储失败"
				if isBlobNotFound(err) {
					lvl = "技能文件的对象不存在"
				}
				s.log.ErrorContext(ctx, lvl,
					"path", out[i].Path, "key", rec.ObjectKey,
					"size", rec.Size, "err", err)
				mu.Lock()
				starved = append(starved, out[i].Path)
				mu.Unlock()
				return
			}
			out[i].Content = content
			// Size follows **the object actually fetched**: we have the object itself in hand, and its
			// length is more authoritative than any metadata (`files.size` was recorded at write
			// time; it drifts if the object was changed by something else).
			//
			// ⚠️ This **deliberately does not preset `out[i].Size = rec.Size`**: if fetching fails the
			// whole batch errors, so that row **never goes out**, and the preset would be an assignment
			// nobody can observe. Keeping a line of code "whose correctness cannot be tested" is worse
			// than deleting it: the next person would think it guards something.
			out[i].Size = len(content)
		}(i, rec)
	}
	wg.Wait()

	if len(starved) > 0 {
		// ⚠️ Error rather than "use however many were fetched". Not injecting the whole skill is an
		// honest state; an incomplete skill is not. The core/skillinject.go side records this error as
		// a Warn and keeps the container usable as usual.
		sort.Strings(starved) // stable order, otherwise the same failure would read differently every time
		return nil, fmt.Errorf(
			"skill: %d 个技能文件在对象存储里取不到，拒绝交出残缺的技能（%s）",
			len(starved), strings.Join(starved, ", "))
	}
	return out, nil
}

// skillFileIDs gets all non-empty FileIDs in a batch of skill files.
//
// ⚠️ The empty ones **are not in the return value, but not ignored either**: the main loop of
// hydrateSkillFiles puts them in starved (an empty FileID is an error). This only collects the query
// parameters for GetFiles; do not do correctness checks here.
func skillFileIDs(files []apimodel.SkillFile) []string {
	ids := make([]string, 0, len(files))
	for i := range files {
		if id := strings.TrimSpace(files[i].FileID); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// BlobPrefixSkillFile exports the object prefix for skill attachment bytes, for the core side (the
// alias in core/skillpkgalias.go). Writing the literal in two places would drift.
const BlobPrefixSkillFile = blobPrefixSkillFile

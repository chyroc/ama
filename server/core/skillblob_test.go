// Tests for the path where skill files go through object storage.
//
// ⚠️ **This round is the first half of step four (4a: the Go side stops reading and writing
// content)**, so several more assertions **were deliberately reversed**. The reason for each
// reversal is written in its own function comment; do not mistake them for wrong tests:
//
//	step three: still write content when file_id is empty   →  4a: not a single byte of content is written
//	step three: content to fall back on if object storage is down  →  4a: **no fallback**, always refuse to hand out
//	step three: no object storage is a normal setup (uses bytea)  →  4a: no object storage means 422 right away
//
// # The invariant changed; this is where every assertion in the file comes from
//
// It used to be "the bytes must be stored in exactly one place", with the two places taking
// turns as the fallback. After 4a only one place is left:
//
//	**The bytes live only in object storage. If they cannot be fetched, report an error; never
//	hand out an empty file.**
//
// This is stricter, but it is the precondition for 4b (drop column): as long as any path still
// depends on content, dropping the column produces a batch of 42703 errors (see that incident
// in schema/README.md).
//
// # Why "stricter" is right and not a step back in availability
//
// Failure on the inject-into-container path is a **silent downgrade** (skillinject.go), so
// handing out a zero-byte skill makes nobody report an error; it only makes the model read a
// skill with all directories present and all content empty. That is much worse than "this
// skill cannot be used", because the latter can be noticed.
//
// It locks down seven "wrong but no error" things:
//
//  1. with object storage configured, the bytes really go into object storage and FileID is
//     filled back in;
//  2. ⭐⭐ **not a single byte is written to the content column**; if this slips, 4b's drop
//     column produces 42703;
//  3. ⭐ **the read path only trusts the object storage copy**;
//  4. ⭐⭐ **if object storage cannot be read, the whole batch fails** (no more falling back to
//     reading the DB); this is the most important reversal in 4a;
//  5. ⭐⭐ **never hand out an incomplete skill**: injection is a silent downgrade, and an
//     incomplete skill is worse than a missing one (see the comment on hydrateSkillFiles);
//  6. ⭐⭐ **the source of truth for Size is files.size**: the store layer does not select
//     content at all, so it cannot compute the size;
//  7. ⭐⭐ **no object storage configured / failing to write object storage is always reported
//     upward**, no more silent fallback.
//     ⚠️ This is the trade-off "better to fail the upload than accept an empty skill"; see
//     putSkillFileBlobs.
package core

import (
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
)

// sortSkillFilesByPath sorts by path ascending, so "pick the first one" is reproducible.
//
// It does not reuse sortSkillFiles from coremem: that one is in another package and not exported.
func sortSkillFilesByPath(files []apimodel.SkillFile) {
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
}

// skillPkgWithFiles builds a skill package with attachments.
//
// The three files deliberately **have different content, none a substring of another**: when
// asserting "which file maps to which bytes", identical content would make the assertion always
// true (the same lesson as the one in blob_test.go).
func skillPkgWithFiles(name string) apimodel.SkillPackage {
	return apimodel.SkillPackage{
		Name:         name,
		DisplayTitle: name,
		Instructions: "这是技能正文",
		Files: []apimodel.SkillFile{
			{Path: "scripts/init.sh", Content: []byte("#!/bin/sh\necho alpha-init")},
			{Path: "references/note.md", Content: []byte("# bravo 参考资料")},
			{Path: "data/blob.bin", Content: []byte{0x00, 0x01, 0xff, 0xfe, 0x43}},
		},
	}
}

// ⭐⭐ **Bytes go into object storage, FileID is filled back, and the content column no longer
// keeps a copy.**
//
// ⚠️ **This assertion was reversed in step three (stop writing content); the test is not wrong.**
// It used to be called TestSkillFilesDualWriteToBlobStore and asserted "both sides have the
// bytes", which was the right shape during dual writing. Now `PutSkillFiles` only writes content
// when `file_id` is empty, so **rows with a non-empty file_id must have empty content**.
//
// Why the assertion direction must flip with it: keeping the old assertion ("content must have
// bytes too") is the same as requiring dual writing to continue, while the goal of this round is
// exactly to stop it; that test would reject the whole of step three.
// And **asserting the reverse, that content is empty, has value of its own**: it pins down "the
// write stop really took effect"; otherwise we would only discover it is still writing at step
// four's drop column, and then it is a batch of 42703.
func TestSkillFilesStopWritingContentWhenInBlobStore(t *testing.T) {
	ctx := actorCtx(t)
	svc, blobs := newServiceWithBlobs(t)

	sk, err := svc.CreateSkillFromPackage(ctx, seedWS, skillPkgWithFiles("stop-dual-write"), SkillCreateOptions{})
	if err != nil {
		t.Fatalf("建技能失败：%v", err)
	}

	// 1. All three files were written into object storage.
	if len(blobs.putKeys) != 3 {
		t.Fatalf("应写入 3 个对象，实际 %d：%v", len(blobs.putKeys), blobs.putKeys)
	}
	for _, k := range blobs.putKeys {
		if !strings.HasPrefix(k, blobPrefixSkillFile+"/") {
			t.Errorf("对象 key 前缀应是 %s/，实际 %s", blobPrefixSkillFile, k)
		}
	}

	// 2. The DB copy: FileID is non-empty, **the content column is empty** (the write stop works).
	//
	// ⚠️ **It must go through svc.store, not svc.SkillFiles**: the latter fills the bytes back
	// from object storage, so asserting "the content column is empty" with it can never hold; it
	// cannot tell which side the bytes came from. What this test must prove is exactly that **the
	// DB copy** is gone, so it must skip the fill-in and read the DB directly.
	stored, err := svc.store.SkillFiles(ctx, sk.ID)
	if err != nil {
		t.Fatalf("读技能文件失败：%v", err)
	}
	if len(stored) != 3 {
		t.Fatalf("应有 3 个文件，实际 %d", len(stored))
	}
	for _, f := range stored {
		if f.FileID == "" {
			t.Errorf("%s 的 FileID 是空的——字节没进对象存储", f.Path)
		}
		if len(f.Content) != 0 {
			t.Errorf("%s 的 content 列还有 %d 字节——**停写没生效**，"+
				"第四步 drop column 时会发现还在写它", f.Path, len(f.Content))
		}
	}

	// 3. The bytes really are in object storage and the content matches (looked up by path, not
	// by order). **This one must not be dropped**: asserting only "content is empty" would let an
	// implementation that "never wrote to object storage" pass too, and that is exactly the shape
	// of data loss.
	got, err := svc.SkillFiles(ctx, sk.ID)
	if err != nil {
		t.Fatalf("读技能文件失败：%v", err)
	}
	want := map[string]string{
		"scripts/init.sh":    "#!/bin/sh\necho alpha-init",
		"references/note.md": "# bravo 参考资料",
	}
	for _, f := range got {
		if w, ok := want[f.Path]; ok && string(f.Content) != w {
			t.Errorf("%s 内容不对：%q", f.Path, f.Content)
		}
	}
}

// ⭐⭐ **The read path must prefer the object storage copy.**
//
// This is the core of this file, done the same way as the "prefer object storage" test in
// blob_test.go: put **different** bytes on the two sides, then see which copy is read back.
//
// ⚠️ This test used to be **the other way round** (called TestSkillFilesReadStillComesFromDatabase,
// asserting "it should not read the object storage copy"). Back then the read path was
// deliberately not switched, because injecting into the container is a hot path and fetching N
// files one after another was not worth it. Now it is switched, on the condition that
// hydrateSkillFiles does concurrent fetching and per-file fallback, so the assertion was reversed
// with it; the test is not wrong.
func TestSkillFilesReadPrefersBlobStore(t *testing.T) {
	ctx := actorCtx(t)
	svc, blobs := newServiceWithBlobs(t)

	sk, err := svc.CreateSkillFromPackage(ctx, seedWS, skillPkgWithFiles("read-from-blob"), SkillCreateOptions{})
	if err != nil {
		t.Fatalf("建技能失败：%v", err)
	}

	// Replace the three copies in object storage with distinguishable content.
	// (In reality the two sides match; the difference is made only so that "which side was read"
	// is observable.)
	blobs.mu.Lock()
	for k := range blobs.objects {
		blobs.objects[k] = []byte("THIS-IS-THE-BLOB-COPY")
	}
	blobs.mu.Unlock()

	stored, err := svc.SkillFiles(ctx, sk.ID)
	if err != nil {
		t.Fatalf("读技能文件失败：%v", err)
	}
	if len(stored) != 3 {
		t.Fatalf("应有 3 个文件，实际 %d", len(stored))
	}
	for _, f := range stored {
		if string(f.Content) != "THIS-IS-THE-BLOB-COPY" {
			t.Errorf("%s 读到的不是对象存储那份（拿到 %q）——读路径没切过去", f.Path, f.Content)
		}
		// Size must follow the bytes; otherwise the size in the UI stays at the length of the DB
		// copy, silently.
		if f.Size != len("THIS-IS-THE-BLOB-COPY") {
			t.Errorf("%s 的 Size 没跟着对象存储那份走：%d", f.Path, f.Size)
		}
	}
}

// ⭐⭐ **When object storage cannot be read but the DB still has bytes, fall back to reading the
// content column without an error.**
//
// ⭐⭐ **If object storage cannot be read, the whole batch fails; after 4a there is no fallback.**
//
// # The meaning of this test was fully reversed in 4a; keeping the history here is useful
//
// It was originally called "fall back to the content column when object storage cannot be read",
// and locked down "existing rows hitting an object storage outage should degrade to the
// pre-switch behavior, not leave the whole session without skills". That judgment was right at
// the time, and for a solid reason: the caller is the container creation flow, and an error
// means the whole session cannot install skills.
//
// 4a removed its precondition: **the content column is neither written nor read anymore**, so
// there is nothing to fall back to. So for the same scenario (object storage completely down) the
// right behavior changed from "silently degrade" to "report an error clearly".
//
// ⚠️ **This is a real loss of availability, not a casual change.** In exchange, 4b can safely
// drop the column, plus a more important guarantee: never hand a zero-byte skill to container
// injection (that path is a silent downgrade; an empty skill gives no error, it only makes the
// model read a skill with all directories present and all content empty).
//
// ⚠️ **Do not add the fallback back "to make it pass like before"**: that would break 4b's
// precondition, and if any instance still reads content when the column is dropped, it is a batch
// of 42703 (that incident in schema/README.md).
func TestSkillFilesFailsWhenBlobMissing(t *testing.T) {
	ctx := actorCtx(t)
	svc, blobs := newServiceWithBlobs(t)

	sk, err := svc.CreateSkillFromPackage(ctx, seedWS, skillPkgWithFiles("blob-gone"), SkillCreateOptions{})
	if err != nil {
		t.Fatalf("建技能失败：%v", err)
	}

	// The objects are all gone (cleared by a lifecycle rule, or a migration went wrong).
	blobs.mu.Lock()
	blobs.missing = true
	blobs.mu.Unlock()

	// ⚠️ The check is **an error**, not "the fallback read the bytes".
	_, err = svc.SkillFiles(ctx, sk.ID)
	if err == nil {
		t.Fatal("⚠️对象存储全灭时必须报错——4a 之后 content 列没有第二份字节可回落，" +
			"不报错就意味着交出了一批空文件，而注入容器那条路径是静默降级")
	}
	// The error must name which files, otherwise production debugging does not know which object
	// to look at.
	if !strings.Contains(err.Error(), "scripts/init.sh") {
		t.Errorf("报错要带上取不到的路径，实际：%v", err)
	}
	if !strings.Contains(err.Error(), "拒绝交出残缺的技能") {
		t.Errorf("报错要说清「拒绝交出残缺的技能」，实际：%v", err)
	}
}

// ⭐⭐ **Rows with an empty file_id (existing dirty data) are always rejected; the content column
// is no longer read.**
//
// # The precondition of this test disappeared in 4a, so its meaning was reversed with it
//
// It was originally called "switched and unswitched files coexist and each is read its own way",
// locking down "do not fall back for the whole batch, and do not use object storage for the whole
// batch either". That mixed state **really existed** in step three: putSkillFileBlobs swallowed
// write failures one by one, so one skill could have half its files in object storage and half
// only in the DB.
//
// After 4a the mixed state **can no longer be produced**: if putSkillFileBlobs fails to write any
// one file, the whole batch fails (see core/blob.go), so either every row has a file_id or the
// upload never reached the DB at all.
//
// So "empty file_id" has only one source left: **existing dirty data** (rows the migration missed,
// or rows left by failed writes before 4a). And since the content column is not read after 4a,
// those rows' bytes cannot be fetched from anywhere, so the right behavior is to report an error
// and make the dirty data visible.
//
// Normally no row should take this path; if one does, it is a data problem and should not be
// silently covered up as an empty file.
func TestSkillFilesRefusesRowWithoutFileID(t *testing.T) {
	ctx := actorCtx(t)
	svc, blobs := newServiceWithBlobs(t)

	sk, err := svc.CreateSkillFromPackage(ctx, seedWS, skillPkgWithFiles("mixed"), SkillCreateOptions{})
	if err != nil {
		t.Fatalf("建技能失败：%v", err)
	}

	files, err := svc.store.SkillFiles(ctx, sk.ID)
	if err != nil {
		t.Fatalf("读库里那份失败：%v", err)
	}
	if len(files) != 3 {
		t.Fatalf("前置条件：应有 3 个文件，实际 %d", len(files))
	}

	// Build a piece of existing dirty data: clear the FileID of one of them and leave a copy of
	// the bytes in the content column.
	// ⚠️ **Bytes in content must not be read out either**: the 4a read path does not look at that
	// column at all, and this is exactly what this test pins down: having bytes ≠ being readable.
	sortSkillFilesByPath(files)
	orphan := files[0].Path
	for i := range files {
		if files[i].Path == orphan {
			files[i].FileID = ""
			files[i].Content = []byte("ONLY-IN-DATABASE")
		}
	}
	if err := svc.store.PutSkillFiles(ctx, sk.ID, seedWS, files); err != nil {
		t.Fatalf("写回失败：%v", err)
	}

	// Replace the others in object storage with distinguishable bytes. They are fine, and they
	// prove "the error is not because the whole batch is broken, but because that one file_id is
	// empty".
	blobs.mu.Lock()
	for k := range blobs.objects {
		blobs.objects[k] = []byte("FROM-BLOB")
	}
	blobs.mu.Unlock()

	_, err = svc.SkillFiles(ctx, sk.ID)
	if err == nil {
		t.Fatalf("⚠️ %s 的 file_id 是空的，4a 之后它的字节无处可取，必须报错——"+
			"不报错就意味着交出了一个空文件（content 列已经不读了）", orphan)
	}
	// The error must name that one row, not vaguely say "read failed".
	if !strings.Contains(err.Error(), orphan) {
		t.Errorf("报错要带上 %s，实际：%v", orphan, err)
	}
}

// ⭐⭐ **Size must not become 0 because content is no longer written.**
//
// This locks down the knock-on effect most easily missed in step three, the kind of trap where
// it "silently becomes 0, the UI shows 0 bytes, and nobody reports an error".
//
// # Mechanism
//
// `store.SkillFiles` computes Size with `len(content)` (the PG side does not store Size at all).
// After writes stop, rows with a non-empty file_id have NULL content, so it computes **0**. And
// Size flows to two places users can see:
//
//   - `doc.Files[i].Size`: the return value of `ListSkills`, "how many files and how large this
//     skill is" in the UI
//   - the read-modify-write of `UpdateSkillFile`
//
// So the source of truth was switched to `files.size` (`hydrateSkillFiles` takes it from
// `recs[id].Size`).
//
// ⚠️ **Both paths must be asserted, because their Size comes from different places**:
// with the bytes fetched it is `len(content)`; when object storage cannot be read it is
// `files.size`. Testing only the first would let the "set Size after blobs.Get succeeds" approach
// pass too, and that is exactly the shape that shows 0 bytes when object storage is down.
func TestSkillFilesSizeSurvivesContentStopWrite(t *testing.T) {
	ctx := actorCtx(t)
	// blobs is not used: this test no longer simulates "object storage is down" (after 4a that is
	// a whole-batch error, guarded by TestSkillFilesFailsWhenBlobMissing). Here we only care
	// where Size comes from.
	svc, _ := newServiceWithBlobs(t)

	pkg := skillPkgWithFiles("size-not-zero")
	// Record how large each file originally was and check one by one; using a total would let
	// "A's size given to B" pass.
	wantSize := map[string]int{}
	for _, f := range pkg.Files {
		wantSize[f.Path] = len(f.Content)
	}

	sk, err := svc.CreateSkillFromPackage(ctx, seedWS, pkg, SkillCreateOptions{})
	if err != nil {
		t.Fatalf("建技能失败：%v", err)
	}

	// 1. The list in doc (this is what the UI shows).
	// **This is the number users actually see**, so it is asserted first.
	if len(sk.Files) != len(wantSize) {
		t.Fatalf("doc 里应有 %d 个文件，实际 %d", len(wantSize), len(sk.Files))
	}
	for _, f := range sk.Files {
		if f.Size == 0 {
			t.Errorf("doc.Files 里 %s 的 Size 是 0——界面上会显示「0 字节」且不报错", f.Path)
		}
		if f.Size != wantSize[f.Path] {
			t.Errorf("doc.Files 里 %s 的 Size 是 %d，应为 %d", f.Path, f.Size, wantSize[f.Path])
		}
	}

	// 2. When the read path works (object storage returns the bytes), Size follows the bytes.
	got, err := svc.SkillFiles(ctx, sk.ID)
	if err != nil {
		t.Fatalf("读技能文件失败：%v", err)
	}
	for _, f := range got {
		if f.Size != wantSize[f.Path] {
			t.Errorf("%s 的 Size 是 %d，应为 %d", f.Path, f.Size, wantSize[f.Path])
		}
	}

	// 3. ⭐⭐ **Size must come from files.size, not len(Content).**
	//
	// ⚠️ This part is the **only** assertion that kills the "compute Size with len(Content)"
	// approach; the first two parts cannot, and the reason is worth writing down (we hit it):
	//
	//   * part one looks at doc.Files, which is computed from pkg.Files **at upload time** and
	//     never goes through hydrate;
	//   * in part two object storage returns the bytes, so Content is full and `len(Content)`
	//     **happens to equal** `rec.Size`; both approaches give the same result, so **the
	//     assertion is always true**.
	//
	// So here the two sources are deliberately **made unequal**: the input carries a content
	// shorter than files.size. Such rows do not exist in reality (after 4a the store layer does
	// not return content at all); the difference is made only so that "where Size came from" is
	// observable, the same technique as "different content is needed to disprove" at the top of
	// this file.
	//
	// ⚠️ **After 4a object storage must be readable**, otherwise the whole batch fails (that is
	// guarded separately by TestSkillFilesFailsWhenBlobMissing) and there is no return value to
	// check Size on. So this part no longer sets blobs.missing = true: it tests "where Size comes
	// from", not "what happens when object storage is down"; the two are tested separately.
	raw, err := svc.store.SkillFiles(ctx, sk.ID)
	if err != nil {
		t.Fatalf("读库里那份失败：%v", err)
	}
	if len(raw) == 0 {
		t.Fatal("前置条件：库里应当有行")
	}
	// The DB copy's Size must be 0 and Content must be empty (4a stopped reading the content
	// column). Confirm this precondition first; otherwise the assertion below might be "right
	// anyway" rather than "hydrate filled it in correctly".
	for _, f := range raw {
		if f.Size != 0 {
			t.Fatalf("前置条件不成立：库里 %s 的 Size 是 %d，说明 store 层还在读 content", f.Path, f.Size)
		}
		if len(f.Content) != 0 {
			t.Fatalf("前置条件不成立：库里 %s 还带着 %d 字节，说明 store 层还在 select content",
				f.Path, len(f.Content))
		}
		if f.FileID == "" {
			t.Fatalf("前置条件不成立：%s 没有 FileID，走不到 files.size 那条路", f.Path)
		}
	}

	// The input carries a content **shorter than the real bytes**, with FileID kept as is.
	// So: len(content) = 3, while files.size is the original length (≥5), so they must differ.
	// If hydrate computes Size as len(input content), the first assertion below goes red.
	const shortContent = "abc"
	patched := make([]apimodel.SkillFile, 0, len(raw))
	for _, f := range raw {
		patched = append(patched, apimodel.SkillFile{
			Path: f.Path, Content: []byte(shortContent), FileID: f.FileID,
		})
	}
	// ⚠️ Call hydrateSkillFiles directly: it is visible within the package and is exactly what is
	// under test. Going through store would clear the input content (FileID is non-empty), and the
	// difference could not be built.
	hydrated, err := svc.skills.HydrateSkillFiles(ctx, patched)
	if err != nil {
		t.Fatalf("对象存储取得到时不该报错，实际：%v", err)
	}
	for _, f := range hydrated {
		// ⭐ **Size must be the real byte count, not the 3 in the input.**
		// If it were len(input content) this would be 3: the size in the UI would be wrong, with no
		// error.
		if f.Size == len(shortContent) {
			t.Errorf("%s 的 Size 是 %d（= len(入参 content)）——**Size 的真源错了**，"+
				"应当来自 files.size / 真正取到的对象", f.Path, f.Size)
		}
		if f.Size != wantSize[f.Path] {
			t.Errorf("%s 的 Size 是 %d，应为 files.size 记的 %d",
				f.Path, f.Size, wantSize[f.Path])
		}
	}
}

// ⭐⭐ **Without object storage configured, a skill with attachments must fail to upload.**
//
// skill_files has no bytes column; without a BlobStore **there is nowhere to put the bytes**. If
// the upload were accepted as usual, the skill files would all become zero bytes, while container
// injection is a silent downgrade with zero errors. So it is blocked by **rejecting the upload**:
// the invariant is "bytes are never silently lost".
//
// ⭐⭐ **Without object storage configured, "read" must also fail, not hand out zero-byte files
// as is.**
//
// # This one was added because of mutation testing, not because someone thought of it
//
// The previous test (…RejectsUpload) only guards the **write** path. We measured: after changing
// the "no object storage → error" line in `hydrateSkillFiles` back to step three's
// `return files, nil` (return as is), **all 38 packages stayed green**. No test took the read path
// with "no object storage configured", so not a single test touched that silent downgrade.
//
// And its consequence is the worst kind: after 4a `store.SkillFiles` only returns path and
// file_id, so "return as is" means handing out a batch of files whose `Content` is nil. The
// container injection path is a **silent downgrade** (skillinject.go), so the model reads a skill
// with all directories present and all content empty, and nothing reports an error.
//
// So write and read **each need a test**: in the code they are two different `blobEnabled()`
// checks (putSkillFileBlobs and hydrateSkillFiles), and each can be broken on its own.
func TestSkillFilesWithoutBlobStoreRefusesRead(t *testing.T) {
	ctx := actorCtx(t)
	// First create the skill on a service **with** object storage configured (so the DB has the
	// normal new shape: with file_id).
	svc, _ := newServiceWithBlobs(t)
	sk, err := svc.CreateSkillFromPackage(ctx, seedWS, skillPkgWithFiles("blob-gone-later"), SkillCreateOptions{})
	if err != nil {
		t.Fatalf("建技能失败：%v", err)
	}

	// Then remove object storage, simulating "config was withdrawn / wiring failed"; the DB rows
	// stay the same.
	svc.blobs = nil

	_, err = svc.SkillFiles(ctx, sk.ID)
	if err == nil {
		t.Fatal("⚠️ 没配对象存储时读技能文件必须报错——store 层只返回 path 与 file_id，" +
			"原样交出去就是一批零字节文件，而注入容器是静默降级、线上不会报错")
	}
	if !errors.Is(err, ErrPrecondition) {
		t.Errorf("应回 ErrPrecondition（→422），实际：%v", err)
	}
	if !strings.Contains(err.Error(), "文件存储") {
		t.Errorf("报错要指向「没装配文件存储」这个真实原因，实际：%v", err)
	}
}

// ⚠️ The check must be on **svc.store** (the DB copy): going through svc.SkillFiles would fail
// first after 4a, and we could not see what actually landed in the DB.
func TestSkillFilesWithoutBlobStoreRejectsUpload(t *testing.T) {
	ctx := actorCtx(t)
	svc, _ := newServiceWithoutBlobs(t) // deliberately no object storage

	_, err := svc.CreateSkillFromPackage(ctx, seedWS, skillPkgWithFiles("no-blob-no-loss"), SkillCreateOptions{})
	if err == nil {
		t.Fatal("⚠️ 没配对象存储时带附件的技能必须传不上去——4a 之后 content 列不写了，" +
			"放行等于收下一个零字节的技能，而注入容器是静默降级、线上不会报错")
	}
	if !errors.Is(err, ErrPrecondition) {
		t.Errorf("应回 ErrPrecondition（→422），实际：%v", err)
	}
	if !strings.Contains(err.Error(), "文件存储") {
		t.Errorf("报错要指向「没装配文件存储」这个真实原因，实际：%v", err)
	}

	// ⚠️ **And no half-finished result may be left**: the skill itself should not stay in the DB
	// either. If it did, the user would see a skill "with 3 attachments" while the container could
	// read none of them.
	skills, err := svc.store.ListSkills(ctx, seedWS)
	if err != nil {
		t.Fatalf("列技能失败：%v", err)
	}
	for _, s := range skills {
		if strings.Contains(s.Name, "no-blob-no-loss") {
			t.Errorf("⚠️ 上传失败却留下了技能 %q——应当删回去让用户重试", s.Name)
		}
	}
}

// ⭐⭐ **When writing to object storage fails, the upload must fail; no more swallowing it.**
//
// # This one and the previous one are two halves of the same reversal; the history is worth keeping
//
// It was originally called "bytes must not be lost when writing to object storage fails: those
// files' content column must still be written", locking down that putSkillFileBlobs swallowed
// failures one by one while PutSkillFiles wrote content as the fallback; the two together gave
// "the bytes always have somewhere to land".
//
// After 4a the content column is not written, so that pairing fell apart: swallowing the failure
// = the bytes are on neither side. So **the same invariant (bytes are not lost) is now kept by
// "making the upload fail"**, not by a fallback.
//
// ⚠️ So this test and TestSkillFilesWithoutBlobStoreRejectsUpload are still **not duplicates**:
// that one is "object storage not configured at all" (a config problem), this one is "configured
// but cannot be written" (a runtime failure). The two paths are different branches in the code
// (the blobEnabled() check vs the err from blobs.Put), and each can be broken on its own.
func TestSkillFilesBlobWriteFailureRejectsUpload(t *testing.T) {
	ctx := actorCtx(t)
	svc, blobs := newServiceWithBlobs(t)
	blobs.putErr = errors.New("对象存储挂了")

	_, err := svc.CreateSkillFromPackage(ctx, seedWS, skillPkgWithFiles("blob-write-down"), SkillCreateOptions{})
	if err == nil {
		t.Fatal("⚠️ 写对象存储失败时上传必须失败——4a 之后 content 列没有兜底，" +
			"吞掉失败就是静默丢字节")
	}
	// The error must say which file failed: with a batch of 64 files this decides where debugging
	// starts.
	if !strings.Contains(err.Error(), "对象存储挂了") {
		t.Errorf("报错要把底层原因带上来，实际：%v", err)
	}

	// No half-finished skill may be left.
	skills, err := svc.store.ListSkills(ctx, seedWS)
	if err != nil {
		t.Fatalf("列技能失败：%v", err)
	}
	for _, s := range skills {
		if strings.Contains(s.Name, "blob-write-down") {
			t.Errorf("⚠️ 上传失败却留下了技能 %q——应当删回去让用户重试", s.Name)
		}
	}
}

// ⚠️ There used to be two more tests here; 4a **replaced them entirely**, they were not casually
// deleted:
//
//	TestSkillFilesWithoutBlobStoreUnchanged     no object storage + create skill → assert **success**
//	TestSkillFilesBlobFailureDoesNotBlockUpload writing object storage fails + create skill → assert **success**
//
// They have **exactly the same scenarios as the two above (…RejectsUpload) with exactly the
// opposite assertions**, so they cannot coexist; after 4a "success" is the wrong side (the content
// column is gone, so letting it through means accepting an empty skill).
//
// Keeping the old two would make the tests contradict each other; keeping only the new two does
// not reduce scenario coverage: the "no object storage" and "writing object storage fails"
// branches each still have a test guarding them, only the check flipped from "succeeds as usual
// with the bytes in the DB" to "must fail and leave nothing half-finished".

// ⭐ Full-replacement semantics are not broken: files present in the previous version but not in
// this one must disappear.
//
// PutSkillFiles is "delete then insert", and this invariant must still hold after adding FileID.
// If it broke, old files would stay in the DB and keep being injected into the container, so the
// agent would see a file that the UI does not show.
func TestSkillFilesReplaceStillDropsRemovedFiles(t *testing.T) {
	ctx := actorCtx(t)
	svc, _ := newServiceWithBlobs(t)

	sk, err := svc.CreateSkillFromPackage(ctx, seedWS, skillPkgWithFiles("replace"), SkillCreateOptions{})
	if err != nil {
		t.Fatalf("建技能失败：%v", err)
	}
	if got, _ := svc.SkillFiles(ctx, sk.ID); len(got) != 3 {
		t.Fatalf("前置条件：应有 3 个文件，实际 %d", len(got))
	}

	// Replace it with a new version that has only one file.
	putSkillFilesViaBlobs(t, ctx, svc, sk.ID, []apimodel.SkillFile{
		{Path: "only.md", Content: []byte("只剩这一个")},
	})

	got, err := svc.SkillFiles(ctx, sk.ID)
	if err != nil {
		t.Fatalf("读技能文件失败：%v", err)
	}
	if len(got) != 1 || got[0].Path != "only.md" {
		t.Errorf("全量替换语义破了：应只剩 only.md，实际 %d 个 %v", len(got), pathsOf(got))
	}
}

func pathsOf(files []apimodel.SkillFile) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

// ⭐⭐ **When the fallback cannot get the bytes either, the whole batch must fail; never hand out
// an incomplete skill.**
//
// This locks down the most dangerous failure mode: failure on the container injection path is a
// **silent downgrade**, so the result of "fetched 20 of 21 files" is that the model reads a skill
// with all directories and SKILL.md present but one file missing. **That is worse than the whole
// skill not existing**, because the latter is a state that can be noticed.
//
// skill_files has no bytes column and object storage is the only source, so **this branch is the
// only thing that stops incomplete skills**.
func TestSkillFilesRefusesToReturnPartialSkill(t *testing.T) {
	ctx := actorCtx(t)
	svc, blobs := newServiceWithBlobs(t)

	sk, err := svc.CreateSkillFromPackage(ctx, seedWS, skillPkgWithFiles("no-partial"), SkillCreateOptions{})
	if err != nil {
		t.Fatalf("建技能失败：%v", err)
	}

	files, err := svc.store.SkillFiles(ctx, sk.ID)
	if err != nil {
		t.Fatalf("读库里那份失败：%v", err)
	}
	// It has a file_id; the bytes are only in object storage.
	sortSkillFilesByPath(files)
	for i := range files {
		files[i].Content = nil
	}
	if err := svc.store.PutSkillFiles(ctx, sk.ID, seedWS, files); err != nil {
		t.Fatalf("写回失败：%v", err)
	}
	// Then make object storage unreadable, so these files have bytes on neither side.
	blobs.mu.Lock()
	blobs.missing = true
	blobs.mu.Unlock()

	_, err = svc.SkillFiles(ctx, sk.ID)
	if err == nil {
		t.Fatal("两边都取不到字节时必须报错——交出残缺的技能会让模型读到一个" +
			"「看起来完整」的技能，而注入失败是静默降级")
	}
	if !strings.Contains(err.Error(), "残缺") {
		t.Errorf("错误信息该说清楚是拒绝交出残缺技能，实际：%v", err)
	}
}

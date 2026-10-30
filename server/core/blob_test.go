// Tests for the path that stores attachment bytes in object storage.
//
// They lock in three things that "report no error when wrong":
//
//  1. **The bytes really go into object storage**, and what is read back is the copy in object
//     storage (PG does not store attachment bytes);
//  2. **With no object storage wired, it fails clearly**, and never quietly writes the bytes
//     somewhere else;
//  3. **When the object is gone, it reports the error honestly**, and never reads empty bytes as
//     success.
package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/model/apimodel"
)

// actorCtx creates a context with an identity; all attachment write paths require a logged-in user.
//
// It uses the owner from the seed, so permission checks for creating an issue / a suggestion pass
// naturally.
func actorCtx(t *testing.T) context.Context {
	t.Helper()
	return auth.WithIdentity(context.Background(), auth.Identity{
		Email: "alice@example.com",
		Name:  "测试用户",
	})
}

// fakeBlobStore is a test double for BlobStore that keeps objects in a map.
//
// ⚠️ **It does not do the code under test's job for it**: Put/Get only do the simplest
// store/load, with no fallback and no key normalization. All fallback logic is in core/blob.go,
// which is what is under test.
type fakeBlobStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	// When putErr is non-nil, Put fails right away; used to verify "if the object write fails, nothing is stored in the database".
	putErr error
	// When onPut is non-nil, every Put calls it first; used to simulate "the request is cancelled" while the object is being written.
	onPut func()
	// When missing is true, Get always returns ErrBlobNotFound, simulating "the object is gone".
	missing bool
	putKeys []string
}

func newFakeBlobStore() *fakeBlobStore {
	return &fakeBlobStore{objects: map[string][]byte{}}
}

func (f *fakeBlobStore) Put(_ context.Context, key string, content []byte, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.onPut != nil {
		f.onPut()
	}
	if f.putErr != nil {
		return f.putErr
	}
	f.objects[key] = append([]byte(nil), content...)
	f.putKeys = append(f.putKeys, key)
	return nil
}

func (f *fakeBlobStore) Get(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.missing {
		return nil, ErrBlobNotFound
	}
	data, ok := f.objects[key]
	if !ok {
		return nil, ErrBlobNotFound
	}
	return append([]byte(nil), data...), nil
}

func (f *fakeBlobStore) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key)
	return nil
}

func (f *fakeBlobStore) Bucket() string { return "ama-test" }

// newServiceWithBlobs builds a Service wired to object storage.
func newServiceWithBlobs(t *testing.T) (*Service, *fakeBlobStore) {
	t.Helper()
	svc, _ := newService(t)
	blobs := newFakeBlobStore()
	svc.blobs = blobs
	return svc, blobs
}

// putSkillFilesViaBlobs seeds skill files in **the normal shape after 4a**: bytes go into object
// storage, and the database only keeps file_id.
//
// # Why not call store.PutSkillFiles directly
//
// That would seed **legacy rows** (bytes in the content column, file_id empty), and after 4a the
// read path treats "empty file_id" as an error (see hydrateSkillFiles). So any test that "seeds
// then reads back" would die on "refusing to hand over an incomplete skill", and **that error has
// nothing to do with what they test**: TestSkillFileContentReadsText wants to test that text
// attachments can be read, and TestArkSkillsUploadedOnceAndAttachedToAgent wants to test that a
// skill is uploaded only once.
//
// So those tests should seed "normal rows", and normal rows must go through putSkillFileBlobs.
//
// ⚠️ **It deliberately goes through svc.putSkillFileBlobs instead of building FileID by hand**:
// building it by hand means creating files rows, computing sha256 and stuffing objects into
// fakeBlobStore yourself, which rewrites the code under test inside the test. Getting that wrong
// would let tests pass on a wrong premise.
//
// ⚠️ Tests that want to seed **legacy rows** (to check fallback or migration) still call
// store.PutSkillFiles directly. That is on purpose, and must be stated clearly in the test.
func putSkillFilesViaBlobs(
	t *testing.T, ctx context.Context, svc *Service, skillID string, files []apimodel.SkillFile,
) []apimodel.SkillFile {
	t.Helper()
	withBlobs, err := svc.skills.PutSkillFileBlobs(ctx, skillID, "test@example.com", files)
	if err != nil {
		t.Fatalf("字节写对象存储失败：%v", err)
	}
	sk, err := svc.store.GetSkill(ctx, skillID)
	if err != nil {
		t.Fatalf("取技能失败：%v", err)
	}
	if err := svc.store.PutSkillFiles(ctx, skillID, sk.WorkspaceID, withBlobs); err != nil {
		t.Fatalf("写技能附件失败：%v", err)
	}
	return withBlobs
}

// seedIssueForAttachment creates an issue to attach files to.
func seedIssueForAttachment(t *testing.T, ctx context.Context, svc *Service) string {
	t.Helper()
	iss, err := svc.CreateIssue(ctx, seedWS, apimodel.IssueDraft{Title: "带附件的问题"})
	if err != nil {
		t.Fatalf("建问题失败：%v", err)
	}
	return iss.ID
}

// ⭐ With object storage configured, the bytes must really go into object storage and be read back unchanged.
func TestIssueAttachmentGoesToBlobStore(t *testing.T) {
	ctx := actorCtx(t)
	svc, blobs := newServiceWithBlobs(t)
	issueID := seedIssueForAttachment(t, ctx, svc)

	content := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("payload", 100))
	att, err := svc.CreateIssueAttachment(ctx, issueID, "shot.png", content)
	if err != nil {
		t.Fatalf("传附件失败：%v", err)
	}

	// 1. The business row must point at files.
	if att.FileID == "" {
		t.Fatal("配了对象存储，附件却没有 fileID——字节没进对象存储")
	}
	// 2. Object storage really has these bytes.
	if len(blobs.putKeys) != 1 {
		t.Fatalf("应写入 1 个对象，实际 %d", len(blobs.putKeys))
	}
	stored, err := blobs.Get(ctx, blobs.putKeys[0])
	if err != nil {
		t.Fatalf("对象存储里读不到：%v", err)
	}
	if string(stored) != string(content) {
		t.Error("对象存储里的字节与上传的不一致")
	}
	// 3. The normal read path returns exactly the same bytes.
	_, got, err := svc.IssueAttachmentContent(ctx, att.ID)
	if err != nil {
		t.Fatalf("读附件失败：%v", err)
	}
	if string(got) != string(content) {
		t.Errorf("读回的字节与上传的不一致：%d vs %d 字节", len(got), len(content))
	}
}

// ⭐ The key must follow the convention: <purpose>/<yyyy>/<mm>/<fileID>.
//
// This is not pedantry: the prefix is what object storage lifecycle rules and usage statistics
// are configured on, and **once an object key is written it cannot be changed** (it is the only
// way to find those bytes).
func TestBlobObjectKeyShape(t *testing.T) {
	ctx := actorCtx(t)
	svc, blobs := newServiceWithBlobs(t)
	issueID := seedIssueForAttachment(t, ctx, svc)

	att, err := svc.CreateIssueAttachment(ctx, issueID, "a.png", []byte("\x89PNG\r\n\x1a\nxx"))
	if err != nil {
		t.Fatalf("传附件失败：%v", err)
	}

	key := blobs.putKeys[0]
	parts := strings.Split(key, "/")
	if len(parts) != 4 {
		t.Fatalf("key 应是四段 <用途>/<yyyy>/<mm>/<fileID>，实际 %q", key)
	}
	if parts[0] != blobPrefixIssueAttachment {
		t.Errorf("用途前缀应是 %s，实际 %s", blobPrefixIssueAttachment, parts[0])
	}
	if len(parts[1]) != 4 || len(parts[2]) != 2 {
		t.Errorf("年月应是 yyyy/mm，实际 %s/%s", parts[1], parts[2])
	}
	if parts[3] != att.FileID {
		t.Errorf("key 结尾应是 fileID %s，实际 %s", att.FileID, parts[3])
	}
	// ⚠️ **The file name must never go into the key**: it is user input, and putting it in the path means handling traversal and name clashes.
	if strings.Contains(key, "a.png") {
		t.Errorf("用户提供的文件名不该出现在对象 key 里：%s", key)
	}
}

// ⭐ When the object write fails, **nothing may be stored in the database**: better to make the user re-upload than to leave a record whose bytes cannot be read.
func TestBlobPutFailureAbortsUpload(t *testing.T) {
	ctx := actorCtx(t)
	svc, blobs := newServiceWithBlobs(t)
	issueID := seedIssueForAttachment(t, ctx, svc)

	blobs.putErr = errors.New("对象存储挂了")

	if _, err := svc.CreateIssueAttachment(
		ctx, issueID, "c.png", []byte("\x89PNG\r\n\x1a\nxx")); err == nil {
		t.Fatal("写对象失败时上传应当失败")
	}

	// Not a single row should be left in the database.
	list, err := svc.ListIssueAttachments(ctx, issueID)
	if err != nil {
		t.Fatalf("列附件失败：%v", err)
	}
	if len(list) != 0 {
		t.Errorf("写对象失败却落了库，留下 %d 条读不出字节的记录", len(list))
	}
}

// ⭐ Suggestion attachments go the same way (both domains share the core/blob.go code).
func TestSuggestionAttachmentGoesToBlobStore(t *testing.T) {
	ctx := actorCtx(t)
	svc, blobs := newServiceWithBlobs(t)

	sug, err := svc.CreateSuggestion(ctx, apimodel.SuggestionDraft{Title: "一条建议"})
	if err != nil {
		t.Fatalf("建建议失败：%v", err)
	}

	content := []byte("\x89PNG\r\n\x1a\n建议的截图")
	att, err := svc.CreateSuggestionAttachment(ctx, sug.ID, "s.png", content)
	if err != nil {
		t.Fatalf("传附件失败：%v", err)
	}
	if att.FileID == "" {
		t.Fatal("建议附件也该走对象存储")
	}
	if got := blobs.putKeys[0]; !strings.HasPrefix(got, blobPrefixSuggestionAttachment+"/") {
		t.Errorf("建议附件的 key 前缀不对：%s", got)
	}

	_, gotBytes, err := svc.SuggestionAttachmentContent(ctx, att.ID)
	if err != nil {
		t.Fatalf("读附件失败：%v", err)
	}
	if string(gotBytes) != string(content) {
		t.Error("读回的字节与上传的不一致")
	}
}

// ⭐ The read path reads the copy in object storage: swap the object for distinguishable
// content, and what is read back must change with it.
//
// "Is the content read back correct" only means something when the double and the real value
// **differ in the asserted dimension**; otherwise it tests "did not crash", not "where it read from".
func TestReadComesFromBlobStore(t *testing.T) {
	ctx := actorCtx(t)
	svc, blobs := newServiceWithBlobs(t)
	issueID := seedIssueForAttachment(t, ctx, svc)

	att, err := svc.CreateIssueAttachment(ctx, issueID, "p.png", []byte("\x89PNG\r\n\x1a\n原来那份"))
	if err != nil {
		t.Fatalf("传附件失败：%v", err)
	}
	swapped := []byte("\x89PNG\r\n\x1a\n对象存储里换过的那份")
	blobs.mu.Lock()
	blobs.objects[blobs.putKeys[0]] = swapped
	blobs.mu.Unlock()

	_, got, err := svc.IssueAttachmentContent(ctx, att.ID)
	if err != nil {
		t.Fatalf("读附件失败：%v", err)
	}
	if string(got) != string(swapped) {
		t.Errorf("读回的不是对象存储里那份：%q", got)
	}
}

// ⭐ When the object is gone (cleared by lifecycle, deleted by hand), report the error honestly; never return an empty attachment as success.
func TestBlobMissingIsAnError(t *testing.T) {
	ctx := actorCtx(t)
	svc, blobs := newServiceWithBlobs(t)
	issueID := seedIssueForAttachment(t, ctx, svc)

	att, err := svc.CreateIssueAttachment(ctx, issueID, "b.png", []byte("\x89PNG\r\n\x1a\nxx"))
	if err != nil {
		t.Fatalf("传附件失败：%v", err)
	}
	blobs.missing = true
	if _, _, err := svc.IssueAttachmentContent(ctx, att.ID); !errors.Is(err, ErrBlobNotFound) {
		t.Fatalf("对象没了应报 ErrBlobNotFound，实际 %v", err)
	}
}

// ⭐⭐ With no object storage wired, upload must **fail clearly**, and not a single row is left
// in the database; the bytes are never quietly written back to PG (the attachment table no longer
// has a content column).
func TestAttachmentWithoutBlobStoreFails(t *testing.T) {
	ctx := actorCtx(t)
	svc, _ := newServiceWithoutBlobs(t)
	issueID := seedIssueForAttachment(t, ctx, svc)

	_, err := svc.CreateIssueAttachment(ctx, issueID, "c.png", []byte("\x89PNG\r\n\x1a\nxx"))
	if !errors.Is(err, ErrPrecondition) {
		t.Fatalf("没装配对象存储应报 ErrPrecondition，实际 %v", err)
	}
	list, err := svc.ListIssueAttachments(ctx, issueID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("上传失败却落了 %d 条记录", len(list))
	}
}

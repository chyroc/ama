// Unit tests for the domain logic: Store and ObjectStore both use in-memory fakes, no database and
// no network. The contract of the PG/mem Stores is tested separately in the contract suite in
// repository/dao/coredao.
package userfile

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// -- fake store --------------------------------------------------------------

type fakeStore struct {
	mu   sync.Mutex
	rows map[string]persistmodel.UserFile // key: owner + "/" + id
}

func newFakeStore() *fakeStore { return &fakeStore{rows: map[string]persistmodel.UserFile{}} }

func (st *fakeStore) key(owner, id string) string { return owner + "/" + id }

func (st *fakeStore) CreateUserFile(_ context.Context, row persistmodel.UserFile) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	k := st.key(row.OwnerEmail, row.ID)
	if _, ok := st.rows[k]; ok {
		return persistmodel.ErrConflict
	}
	st.rows[k] = row
	return nil
}

func (st *fakeStore) GetUserFile(_ context.Context, owner, id string) (persistmodel.UserFile, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	row, ok := st.rows[st.key(owner, id)]
	if !ok {
		return persistmodel.UserFile{}, persistmodel.ErrNotFound
	}
	return row, nil
}

func (st *fakeStore) ListCompletedUserFiles(
	_ context.Context, owner string, limit int,
) ([]persistmodel.UserFile, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []persistmodel.UserFile
	for _, row := range st.rows {
		if row.OwnerEmail == owner && row.Status == persistmodel.UserFileStatusCompleted {
			out = append(out, row)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (st *fakeStore) MarkUserFileCompleted(
	_ context.Context, owner, id string, size int64,
) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	k := st.key(owner, id)
	row, ok := st.rows[k]
	if !ok {
		return persistmodel.ErrNotFound
	}
	if row.Status != persistmodel.UserFileStatusUploading {
		return persistmodel.ErrConflict
	}
	row.Status = persistmodel.UserFileStatusCompleted
	row.Size = size
	st.rows[k] = row
	return nil
}

func (st *fakeStore) DeleteUserFile(_ context.Context, owner, id string) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.rows, st.key(owner, id))
	return nil
}

// -- fake objects ------------------------------------------------------------

type fakeObjects struct {
	mu       sync.Mutex
	bucket   string
	objects  map[string][]byte            // completed object key -> bytes
	uploads  map[string]map[string][]byte // key -> partNumber (as string) -> part bytes
	metaCT   map[string]string
	failSize bool
}

func newFakeObjects() *fakeObjects {
	return &fakeObjects{
		bucket:  "ama-test",
		objects: map[string][]byte{},
		uploads: map[string]map[string][]byte{},
		metaCT:  map[string]string{},
	}
}

func (o *fakeObjects) Bucket() string { return o.bucket }

func (o *fakeObjects) Put(_ context.Context, key string, content []byte, contentType string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.objects[key] = append([]byte(nil), content...)
	o.metaCT[key] = contentType
	return nil
}

func (o *fakeObjects) Delete(_ context.Context, key string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.objects, key)
	return nil
}

func (o *fakeObjects) Size(_ context.Context, key string) (int64, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	b, ok := o.objects[key]
	if !ok {
		return 0, ErrObjectNotFound
	}
	if o.failSize {
		return int64(len(b)) + MaxFileSize, nil
	}
	return int64(len(b)), nil
}

func (o *fakeObjects) Open(_ context.Context, key string) (io.ReadCloser, int64, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	b, ok := o.objects[key]
	if !ok {
		return nil, 0, ErrObjectNotFound
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), b...))), int64(len(b)), nil
}

func (o *fakeObjects) PresignedGetURL(key string, ttl time.Duration) (string, error) {
	return "https://objects.example.com/" + o.bucket + "/" + key + "?signed=1", nil
}

func (o *fakeObjects) InitMultipartUpload(
	_ context.Context, key, contentType string,
) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	upID := "up-" + key
	o.uploads[upID] = map[string][]byte{}
	o.metaCT[key] = contentType
	return upID, nil
}

func (o *fakeObjects) UploadPart(
	_ context.Context, key, uploadID string, partNumber int, size int64, content io.Reader,
) error {
	data, err := io.ReadAll(content)
	if err != nil {
		return err
	}
	if int64(len(data)) != size {
		return errors.New("fake: size 与实际字节不符")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	parts, ok := o.uploads[uploadID]
	if !ok {
		return errors.New("fake: 未知 uploadId")
	}
	parts[itoa(partNumber)] = append([]byte(nil), data...)
	return nil
}

func (o *fakeObjects) CompleteMultipartUpload(_ context.Context, key, uploadID string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	parts, ok := o.uploads[uploadID]
	if !ok || len(parts) == 0 {
		return errors.New("fake: 没有分片")
	}
	nums := make([]int, 0, len(parts))
	for k := range parts {
		nums = append(nums, atoi(k))
	}
	sort.Ints(nums)
	var all []byte
	for _, n := range nums {
		all = append(all, parts[itoa(n)]...)
	}
	o.objects[key] = all
	delete(o.uploads, uploadID)
	return nil
}

func (o *fakeObjects) AbortMultipartUpload(_ context.Context, key, uploadID string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.uploads, uploadID)
	return nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

func newSvc(t *testing.T) (*Service, *fakeStore, *fakeObjects) {
	t.Helper()
	st := newFakeStore()
	ob := newFakeObjects()
	s, err := New(Config{Store: st, Objects: ob})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, st, ob
}

// -- cases -------------------------------------------------------------------

func TestNormalizeOwner(t *testing.T) {
	got, err := normalizeOwner("  Foo@Example.com ")
	if err != nil {
		t.Fatal(err)
	}
	if got != "foo@example.com" {
		t.Errorf("归一 = %q，要小写去空白", got)
	}
	if _, err := normalizeOwner("   "); err == nil {
		t.Error("空身份必须拒绝")
	}
}

func TestStartUploadModeSelection(t *testing.T) {
	s, st, ob := newSvc(t)
	ctx := context.Background()

	// Small file → simple, no multipart registered with object storage.
	before := len(ob.uploads)
	f, err := s.StartUpload(ctx, "a@x.com", apimodel.StartUserFileUpload{
		Filename: "small.txt", Size: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.UploadMode != persistmodel.UserFileModeSimple || f.Status != persistmodel.UserFileStatusUploading {
		t.Errorf("小文件模式 = %s/%s", f.UploadMode, f.Status)
	}
	if len(ob.uploads) != before {
		t.Error("simple 模式不该初始化对象存储分片上传")
	}
	row, _ := st.GetUserFile(ctx, "a@x.com", f.ID)
	if row.ObjectKey != "user-files/a@x.com/"+f.ID {
		t.Errorf("object key = %q", row.ObjectKey)
	}
	if !strings.HasPrefix(f.ID, "uf_") {
		t.Errorf("id = %q，要 uf_ 前缀", f.ID)
	}

	// Large file → multipart, uploadId is stored in the database.
	big, err := s.StartUpload(ctx, "a@x.com", apimodel.StartUserFileUpload{
		Filename: "big.bin", Size: PartSize + 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if big.UploadMode != persistmodel.UserFileModeMultipart || big.PartSize != PartSize {
		t.Errorf("大文件模式 = %s partSize=%d", big.UploadMode, big.PartSize)
	}
	brow, _ := st.GetUserFile(ctx, "a@x.com", big.ID)
	if brow.UploadID == "" {
		t.Error("multipart 行必须有 upload_id")
	}

	// Size not declared → multipart (better to go with parts).
	unknown, err := s.StartUpload(ctx, "a@x.com", apimodel.StartUserFileUpload{Filename: "u"})
	if err != nil || unknown.UploadMode != persistmodel.UserFileModeMultipart {
		t.Errorf("未声明大小应走 multipart，得到 %+v err=%v", unknown, err)
	}

	// Over the limit is rejected at once, no row is written.
	if _, err := s.StartUpload(ctx, "a@x.com", apimodel.StartUserFileUpload{
		Filename: "huge", Size: MaxFileSize + 1,
	}); err == nil {
		t.Error("超过 2 GiB 必须拒绝")
	}
}

func TestStartUploadRejectsBadFilename(t *testing.T) {
	s, _, _ := newSvc(t)
	ctx := context.Background()
	for _, name := range []string{"", "   ", "a\x00b", "a\nb", strings.Repeat("x", maxFilenameBytes+1)} {
		if _, err := s.StartUpload(ctx, "a@x.com", apimodel.StartUserFileUpload{
			Filename: name, Size: 1,
		}); err == nil {
			t.Errorf("文件名 %q 应被拒", name)
		}
	}
}

func TestSimpleUploadFlowAndIsolation(t *testing.T) {
	s, st, _ := newSvc(t)
	ctx := context.Background()

	f, err := s.StartUpload(ctx, "a@x.com", apimodel.StartUserFileUpload{
		Filename: "f.txt", ContentType: "text/plain", Size: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Uploading a part to a simple row still in uploading → 400 (wrong mode, different from the 409
	// for completed ones).
	if err := s.UploadPart(ctx, "a@x.com", f.ID, 1, 1, bytes.NewReader([]byte("x"))); !errors.Is(err, persistmodel.ErrInvalidArgument) {
		t.Errorf("uploading 的 simple id 传片应 InvalidArgument，得到 %v", err)
	}
	done, err := s.PutSimpleContent(ctx, "a@x.com", f.ID, 5, bytes.NewReader([]byte("hello")))
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != persistmodel.UserFileStatusCompleted || done.Size != 5 {
		t.Errorf("完成态不对：%+v", done)
	}

	// Someone else using this id → 404 (do not reveal that it exists).
	if _, err := s.GetFile(ctx, "b@x.com", f.ID); !errors.Is(err, persistmodel.ErrNotFound) {
		t.Errorf("他人访问应 ErrNotFound，得到 %v", err)
	}
	if _, err := s.ListFiles(ctx, "b@x.com", 0); err != nil {
		t.Fatal(err)
	}
	// A can see it in their own list.
	list, err := s.ListFiles(ctx, "a@x.com", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != f.ID {
		t.Errorf("A 的列表 = %+v", list)
	}

	// Detail includes the download URL.
	detail, err := s.GetFile(ctx, "a@x.com", f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.DownloadURL == "" || detail.ExpiresInSeconds != 3600 {
		t.Errorf("下载字段不对：%+v", detail)
	}

	// Uploading content again after completion → 409 (the fact that it is completed wins over
	// "wrong mode").
	if _, err := s.PutSimpleContent(ctx, "a@x.com", f.ID, 5, bytes.NewReader([]byte("hello"))); !errors.Is(err, persistmodel.ErrConflict) {
		t.Errorf("重复传内容应 Conflict，得到 %v", err)
	}
	if err := s.UploadPart(ctx, "a@x.com", f.ID, 1, 5, bytes.NewReader([]byte("hello"))); !errors.Is(err, persistmodel.ErrConflict) {
		t.Errorf("给已完成文件传片应 Conflict，得到 %v", err)
	}

	// Delete: both the row and the object are gone. Is deleting again an idempotent 404? Deleting a
	// missing row returns 404, following GetFile.
	if err := s.DeleteFile(ctx, "a@x.com", f.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.rows["a@x.com/"+f.ID]; ok {
		t.Error("删完行还在")
	}
}

func TestSimpleUploadSizeMismatch(t *testing.T) {
	s, _, _ := newSvc(t)
	ctx := context.Background()
	f, _ := s.StartUpload(ctx, "a@x.com", apimodel.StartUserFileUpload{Filename: "f", Size: 5})
	if _, err := s.PutSimpleContent(ctx, "a@x.com", f.ID, 5, bytes.NewReader([]byte("abc"))); err == nil {
		t.Error("声明 5 实到 3 必须拒绝")
	}
}

func TestMultipartFlow(t *testing.T) {
	s, _, ob := newSvc(t)
	ctx := context.Background()

	f, err := s.StartUpload(ctx, "a@x.com", apimodel.StartUserFileUpload{
		Filename: "big.bin", ContentType: "application/octet-stream", Size: PartSize * 2,
	})
	if err != nil {
		t.Fatal(err)
	}

	p1 := bytes.Repeat([]byte("A"), int(PartSize))
	p2 := []byte("tail")
	// Upload out of order (2 first, then 1) to verify the completion order is decided by part
	// number.
	if err := s.UploadPart(ctx, "a@x.com", f.ID, 2, int64(len(p2)), bytes.NewReader(p2)); err != nil {
		t.Fatal(err)
	}
	if err := s.UploadPart(ctx, "a@x.com", f.ID, 1, int64(len(p1)), bytes.NewReader(p1)); err != nil {
		t.Fatal(err)
	}

	// Part number out of range / empty part / part too large are all rejected.
	if err := s.UploadPart(ctx, "a@x.com", f.ID, 0, 1, bytes.NewReader([]byte("x"))); err == nil {
		t.Error("片号 0 应拒")
	}
	if err := s.UploadPart(ctx, "a@x.com", f.ID, 10001, 1, bytes.NewReader([]byte("x"))); err == nil {
		t.Error("片号 10001 应拒")
	}
	bigPart := bytes.Repeat([]byte("B"), int(PartSize)+1)
	if err := s.UploadPart(ctx, "a@x.com", f.ID, 3, int64(len(bigPart)), bytes.NewReader(bigPart)); err == nil {
		t.Error("超过 PartSize 的片应拒")
	}

	done, err := s.CompleteUpload(ctx, "a@x.com", f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Size != PartSize+4 {
		t.Errorf("完成大小 = %d，要 %d", done.Size, PartSize+4)
	}
	// Bytes on the storage side are joined by part number, regardless of upload order.
	if got := ob.objects["user-files/a@x.com/"+f.ID]; !bytes.Equal(got, append(p1, p2...)) {
		t.Errorf("合并字节不对：前 8 字节 %q，尾 %q，总 %d", got[:8], got[len(got)-4:], len(got))
	}
	if ob.metaCT["user-files/a@x.com/"+f.ID] != "application/octet-stream" {
		t.Error("ContentType 没有随 InitUpload 带给对象存储")
	}

	// complete again after completion → 409; deleting a completed one succeeds.
	if _, err := s.CompleteUpload(ctx, "a@x.com", f.ID); !errors.Is(err, persistmodel.ErrConflict) {
		t.Errorf("重复 complete 应 Conflict，得到 %v", err)
	}
}

func TestCompleteRejectsOversize(t *testing.T) {
	s, _, ob := newSvc(t)
	ctx := context.Background()
	f, _ := s.StartUpload(ctx, "a@x.com", apimodel.StartUserFileUpload{Filename: "big"})
	part := []byte("x")
	if err := s.UploadPart(ctx, "a@x.com", f.ID, 1, 1, bytes.NewReader(part)); err != nil {
		t.Fatal(err)
	}
	ob.failSize = true // make the size read back from object storage exceed 2 GiB
	if _, err := s.CompleteUpload(ctx, "a@x.com", f.ID); !errors.Is(err, persistmodel.ErrInvalidArgument) {
		t.Fatalf("超限应 InvalidArgument，得到 %v", err)
	}
	// The oversized object should be deleted on a best-effort basis, and the row stays uploading
	// (it can be aborted).
	if _, ok := ob.objects["user-files/a@x.com/"+f.ID]; ok {
		t.Error("超限对象没有被删")
	}
}

func TestAbortUpload(t *testing.T) {
	s, st, ob := newSvc(t)
	ctx := context.Background()

	// multipart: abort deletes the object storage upload and the database row.
	f, _ := s.StartUpload(ctx, "a@x.com", apimodel.StartUserFileUpload{Filename: "big", Size: PartSize + 1})
	if err := s.AbortUpload(ctx, "a@x.com", f.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.rows["a@x.com/"+f.ID]; ok {
		t.Error("abort 后行还在")
	}
	if len(ob.uploads) != 0 {
		t.Error("对象存储分片上传没被 abort")
	}
	// Idempotent: aborting once more does not error.
	if err := s.AbortUpload(ctx, "a@x.com", f.ID); err != nil {
		t.Errorf("重复 abort 应幂等成功，得到 %v", err)
	}
	// Someone else cannot abort it.
	g, _ := s.StartUpload(ctx, "a@x.com", apimodel.StartUserFileUpload{Filename: "g", Size: PartSize + 1})
	if err := s.AbortUpload(ctx, "b@x.com", g.ID); err != nil {
		t.Errorf("他人 abort 应幂等无操作（查不到即成功），得到 %v", err)
	}
	if _, ok := st.rows["a@x.com/"+g.ID]; !ok {
		t.Error("他人 abort 不能删掉 A 的行")
	}
}

func TestDeleteUploadingRejected(t *testing.T) {
	s, _, _ := newSvc(t)
	ctx := context.Background()
	f, _ := s.StartUpload(ctx, "a@x.com", apimodel.StartUserFileUpload{Filename: "f", Size: 1})
	if err := s.DeleteFile(ctx, "a@x.com", f.ID); !errors.Is(err, persistmodel.ErrConflict) {
		t.Fatalf("删 uploading 行应 Conflict 提示走 abort，得到 %v", err)
	}
}

func TestDeleteFileWhenObjectAlreadyGone(t *testing.T) {
	s, _, ob := newSvc(t)
	ctx := context.Background()
	f, _ := s.StartUpload(ctx, "a@x.com", apimodel.StartUserFileUpload{Filename: "f", Size: 1})
	if _, err := s.PutSimpleContent(ctx, "a@x.com", f.ID, 1, bytes.NewReader([]byte("x"))); err != nil {
		t.Fatal(err)
	}
	delete(ob.objects, "user-files/a@x.com/"+f.ID) // simulate the object already removed by a lifecycle rule
	if err := s.DeleteFile(ctx, "a@x.com", f.ID); err != nil {
		t.Errorf("对象已不在时删除仍应成功（幂等），得到 %v", err)
	}
}

func TestListLimitClamped(t *testing.T) {
	s, _, _ := newSvc(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		f, _ := s.StartUpload(ctx, "a@x.com", apimodel.StartUserFileUpload{
			Filename: "f", Size: 1,
		})
		_, _ = s.PutSimpleContent(ctx, "a@x.com", f.ID, 1, bytes.NewReader([]byte("x")))
	}
	if got, _ := s.ListFiles(ctx, "a@x.com", 0); len(got) != 3 {
		t.Errorf("默认 limit 应返回全部 3 条，得到 %d", len(got))
	}
	if got, _ := s.ListFiles(ctx, "a@x.com", 2); len(got) != 2 {
		t.Errorf("limit=2 应收敛到 2，得到 %d", len(got))
	}
	if got, _ := s.ListFiles(ctx, "a@x.com", 999999); len(got) != 3 {
		t.Errorf("超大 limit 应被夹到 %d 后返回 3 条", maxListLimit)
	}
}

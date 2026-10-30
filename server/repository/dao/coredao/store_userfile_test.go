// Store contract tests for user_files: run once each against MemStore and PGStore (registered in
// runStoreSuite in store_test.go). Only storage behavior is tested, not object storage;
// the chunk/state machine business logic is in the unit tests of service/userfile.
package coredao

import (
	"errors"
	"testing"

	"github.com/chyroc/ama/server/model/persistmodel"
)

// sampleUserFile builds an uploading row owned by a@x.com.
func sampleUserFile(id, owner, status string) persistmodel.UserFile {
	return persistmodel.UserFile{
		ID:          id,
		OwnerEmail:  owner,
		Bucket:      "ama-test",
		ObjectKey:   "user-files/" + owner + "/" + id,
		Filename:    "f.bin",
		ContentType: "application/octet-stream",
		Status:      status,
		UploadMode:  persistmodel.UserFileModeMultipart,
		UploadID:    "up-" + id,
		CreatedAt:   "2026-09-13T10:00:00.000Z",
	}
}

func testUserFileLifecycle(t *testing.T, newStore storeFactory) {
	s := newStore(t, false)
	ctx := t.Context()

	a := sampleUserFile("uf_a", "a@x.com", persistmodel.UserFileStatusUploading)
	if err := s.CreateUserFile(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Read back and compare field by field.
	got, err := s.GetUserFile(ctx, "a@x.com", "uf_a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ObjectKey != a.ObjectKey || got.UploadID != "up-uf_a" || got.Size != 0 {
		t.Errorf("读回的行不对：%+v", got)
	}

	// CAS to completed.
	if err := s.MarkUserFileCompleted(ctx, "a@x.com", "uf_a", 123); err != nil {
		t.Fatalf("MarkCompleted: %v", err)
	}
	got, _ = s.GetUserFile(ctx, "a@x.com", "uf_a")
	if got.Status != persistmodel.UserFileStatusCompleted || got.Size != 123 {
		t.Errorf("完成态不对：%+v", got)
	}

	// Setting completed again must give 409 (row exists but wrong status), not succeed silently.
	if err := s.MarkUserFileCompleted(ctx, "a@x.com", "uf_a", 456); !isErrConflict(err) {
		t.Errorf("重复完成应 ErrConflict，得到 %v", err)
	}
	if got2, _ := s.GetUserFile(ctx, "a@x.com", "uf_a"); got2.Size != 123 {
		t.Errorf("重复完成不该改大小，得到 %d", got2.Size)
	}

	// Not readable after delete; deleting again is idempotent.
	if err := s.DeleteUserFile(ctx, "a@x.com", "uf_a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.GetUserFile(ctx, "a@x.com", "uf_a"); !isErrNotFound(err) {
		t.Errorf("删后 Get 应 ErrNotFound，得到 %v", err)
	}
	if err := s.DeleteUserFile(ctx, "a@x.com", "uf_a"); err != nil {
		t.Errorf("重复删除应幂等成功，得到 %v", err)
	}
}

func testUserFileOwnerIsolation(t *testing.T, newStore storeFactory) {
	s := newStore(t, false)
	ctx := t.Context()

	row := sampleUserFile("uf_x", "a@x.com", persistmodel.UserFileStatusCompleted)
	if err := s.CreateUserFile(ctx, row); err != nil {
		t.Fatal(err)
	}

	// Someone else querying the same id → NotFound (not 403: do not reveal existence).
	if _, err := s.GetUserFile(ctx, "b@x.com", "uf_x"); !isErrNotFound(err) {
		t.Errorf("他人 Get 应 NotFound，得到 %v", err)
	}
	// Someone else updating / deleting does not affect A's row.
	if err := s.MarkUserFileCompleted(ctx, "b@x.com", "uf_x", 9); !isErrNotFound(err) {
		t.Errorf("他人 MarkCompleted 应 NotFound，得到 %v", err)
	}
	if err := s.DeleteUserFile(ctx, "b@x.com", "uf_x"); err != nil {
		t.Errorf("他人 Delete 应幂等无操作但不能报错，得到 %v", err)
	}
	if _, err := s.GetUserFile(ctx, "a@x.com", "uf_x"); err != nil {
		t.Errorf("A 的行必须还在，得到 %v", err)
	}
	if list, _ := s.ListCompletedUserFiles(ctx, "b@x.com", 10); len(list) != 0 {
		t.Errorf("B 不该列出 A 的文件，得到 %d 条", len(list))
	}
}

func testUserFileListOrderAndLimit(t *testing.T, newStore storeFactory) {
	s := newStore(t, false)
	ctx := t.Context()

	// Three rows: two completed (different times), one uploading (must not be in the list).
	mk := func(id, createdAt, status string) {
		r := sampleUserFile(id, "a@x.com", status)
		r.CreatedAt = createdAt
		if err := s.CreateUserFile(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	mk("uf_1", "2026-09-11T10:00:00.000Z", persistmodel.UserFileStatusCompleted)
	mk("uf_2", "2026-09-13T10:00:00.000Z", persistmodel.UserFileStatusCompleted)
	mk("uf_3", "2026-09-14T10:00:00.000Z", persistmodel.UserFileStatusUploading)

	list, err := s.ListCompletedUserFiles(ctx, "a@x.com", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("只应列 2 条 completed，得到 %d：%+v", len(list), list)
	}
	if list[0].ID != "uf_2" || list[1].ID != "uf_1" {
		t.Errorf("顺序应为 uf_2, uf_1（created_at 倒序），得到 %s, %s",
			list[0].ID, list[1].ID)
	}

	// An empty result must be a non-nil empty slice (contract: list endpoints do not return null).
	empty, err := s.ListCompletedUserFiles(ctx, "nobody@x.com", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 || empty == nil {
		t.Errorf("空列表应是非 nil 空切片，得到 %v", empty)
	}

	// limit takes effect.
	two, _ := s.ListCompletedUserFiles(ctx, "a@x.com", 1)
	if len(two) != 1 || two[0].ID != "uf_2" {
		t.Errorf("limit=1 应只回最新一条，得到 %+v", two)
	}
}

func testUserFileUniqueObjectKey(t *testing.T, newStore storeFactory) {
	s := newStore(t, false)
	ctx := t.Context()

	r1 := sampleUserFile("uf_a", "a@x.com", persistmodel.UserFileStatusUploading)
	if err := s.CreateUserFile(ctx, r1); err != nil {
		t.Fatal(err)
	}
	// Same owner, same object_key, different id: clashes on the unique key.
	r2 := r1
	r2.ID = "uf_b"
	if err := s.CreateUserFile(ctx, r2); !isErrConflict(err) {
		t.Errorf("撞 (owner, object_key) 唯一键应 ErrConflict，得到 %v", err)
	}
}

// isErrNotFound / isErrConflict are the sentinel checks for these tests (PG and mem both return
// the same sentinel wrapped by fmt.Errorf, and errors.Is recognizes it on both sides).
func isErrNotFound(err error) bool { return errors.Is(err, persistmodel.ErrNotFound) }

func isErrConflict(err error) bool { return errors.Is(err, persistmodel.ErrConflict) }

package core

import (
	"context"
	"strings"
	"testing"

	coremem "github.com/chyroc/ama/server/repository/dao/coremem"
)

// ctxHonoringStore makes DeleteSkill honor ctx like a real database: it fails once ctx is
// cancelled.
//
// The in-memory store ignores ctx, so testing "the rollback used a cancelled ctx" with it is
// always green, while PG in production reports context canceled right away. This stand-in
// fills only that one gap; every other method passes straight through.
type ctxHonoringStore struct {
	Store
}

func (s ctxHonoringStore) DeleteSkill(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.Store.DeleteSkill(ctx, id)
}

// TestCreateSkillRollsBackEvenWhenRequestCancelled pins down "when attachment writes fail
// because the request was cancelled, the newly created skill must still be deleted".
//
// Seen in production: a skill with 635 files wrote them to object storage one by one; at 60
// seconds the request was cut off with context canceled. The rollback deleted with the same
// cancelled ctx, so the delete failed too and the error was swallowed. The database was left
// with a skill that declared 635 attachments and had not a single file.
func TestCreateSkillRollsBackEvenWhenRequestCancelled(t *testing.T) {
	mem, err := coremem.NewSeededMemStore()
	if err != nil {
		t.Fatalf("装载种子失败：%v", err)
	}
	svc, err := New(Config{Store: ctxHonoringStore{Store: mem}})
	if err != nil {
		t.Fatalf("装配 Service 失败：%v", err)
	}
	blobs := newFakeBlobStore()
	svc.blobs = blobs

	ctx, cancel := context.WithCancel(actorCtx(t))
	defer cancel()
	// The request is cancelled while writing the first object. As in production, the failure
	// cause is the ctx itself.
	blobs.onPut = cancel
	blobs.putErr = context.Canceled

	if _, err := svc.CreateSkillFromPackage(ctx, seedWS, skillPkgWithFiles("cancelled-upload"),
		SkillCreateOptions{}); err == nil {
		t.Fatal("写对象存储失败时上传必须失败")
	}

	skills, err := mem.ListSkills(context.Background(), seedWS)
	if err != nil {
		t.Fatalf("列技能失败：%v", err)
	}
	for _, s := range skills {
		if strings.Contains(s.Name, "cancelled-upload") {
			t.Errorf("⚠️ 请求被取消导致上传失败，却留下了技能 %q——回滚用了已取消的 ctx，"+
				"删除失败被吞掉，库里是一个声明了附件、一个文件都没有的半成品", s.Name)
		}
	}
}

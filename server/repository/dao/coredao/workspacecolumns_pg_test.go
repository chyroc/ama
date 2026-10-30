// Integration tests for the name / created_by / updated_by columns of workspaces: the write paths really write the columns correctly.
//
// All tests in this file are skipped as a group when the database is unreachable, using the same check as store_pg_test.go (pgEnv.skip).

package coredao

import (
	"context"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// showPtr renders a *string in a human-readable form; NULL and empty string must be easy to tell apart.
func showPtr(p *string) string {
	if p == nil {
		return "NULL"
	}
	return `"` + *p + `"`
}

// TestWorkspaceWritePathsFillColumns pins "all three write paths write the new columns".
//
// It reads the **columns** back directly, not through the read path, so a self-consistent error like "both read and write miss the same field"
// can only be caught by it (a Put-then-Get round-trip test is all green for that kind of error).
//
// It also pins that the column must be NULL when CreatedBy is empty (nullIfEmpty): "never recorded" and "empty string" must be distinguishable.
func TestWorkspaceWritePathsFillColumns(t *testing.T) {
	pgEnvOnce.Do(initPGEnv)
	if pgEnv.skip != "" {
		t.Skip(pgEnv.skip)
	}
	ctx := context.Background()

	s, err := NewPGStore(ctx, pgEnv.client)
	if err != nil {
		t.Fatalf("建 PGStore 失败：%v", err)
	}
	if err := s.Clear(ctx); err != nil {
		t.Fatalf("清库失败：%v", err)
	}
	defer func() {
		if err := s.Clear(ctx); err != nil {
			t.Logf("收尾清库失败：%v", err)
		}
	}()

	readCols := func(id string) (name, createdBy, updatedBy *string) {
		t.Helper()
		if err := pgEnv.client.QueryRow(ctx,
			`select name, created_by, updated_by from workspaces where id = $1`, id,
		).Scan(&name, &createdBy, &updatedBy); err != nil {
			t.Fatalf("回读 %s 的列失败：%v", id, err)
		}
		return
	}

	// --- CreateWorkspace ---------------------------------------------------
	// The three fields have different values, for the same reason as above: with the same value, an implementation that writes name into created_by
	// would also pass.
	ws := apimodel.Workspace{
		ID: "ws_w1", Key: "w1", Name: "写路径空间",
		CreatedAt: apimodel.NowISO(), UpdatedAt: apimodel.NowISO(),
		CreatedBy: "maker@example.com", UpdatedBy: "maker@example.com",
	}
	if _, err := s.CreateWorkspace(ctx, ws); err != nil {
		t.Fatalf("建空间失败：%v", err)
	}
	name, createdBy, updatedBy := readCols("ws_w1")
	if name == nil || *name != "写路径空间" {
		t.Errorf("CreateWorkspace 没写对 name 列：%s", showPtr(name))
	}
	if createdBy == nil || *createdBy != "maker@example.com" {
		t.Errorf("CreateWorkspace 没写对 created_by 列：%s", showPtr(createdBy))
	}
	if updatedBy == nil || *updatedBy != "maker@example.com" {
		t.Errorf("CreateWorkspace 没写对 updated_by 列：%s", showPtr(updatedBy))
	}

	// --- SaveWorkspace -----------------------------------------------------
	// Rename + change updatedBy; both columns must follow. **CreatedBy stays unchanged**: editing a workspace must not replace
	// "who created it" (see apimodel.Workspace.CreatedBy and the core test of the same name).
	ws.Name = "改过名的空间"
	ws.UpdatedBy = "editor@example.com"
	ws.UpdatedAt = apimodel.NowISO()
	if _, err := s.SaveWorkspace(ctx, ws); err != nil {
		t.Fatalf("存空间失败：%v", err)
	}
	name, createdBy, updatedBy = readCols("ws_w1")
	if name == nil || *name != "改过名的空间" {
		t.Errorf("SaveWorkspace 没更新 name 列：%s——"+
			"读路径已经切到列上了，所以这就是「改了名字不生效」", showPtr(name))
	}
	if createdBy == nil || *createdBy != "maker@example.com" {
		t.Errorf("SaveWorkspace 顶掉了 created_by：%s", showPtr(createdBy))
	}
	if updatedBy == nil || *updatedBy != "editor@example.com" {
		t.Errorf("SaveWorkspace 没更新 updated_by 列：%s", showPtr(updatedBy))
	}

	// --- An empty CreatedBy must be stored as NULL, not as an empty string ---------------------------
	noActor := apimodel.Workspace{
		ID: "ws_w2", Key: "w2", Name: "没有创建者的空间",
		CreatedAt: apimodel.NowISO(), UpdatedAt: apimodel.NowISO(),
	}
	if _, err := s.CreateWorkspace(ctx, noActor); err != nil {
		t.Fatalf("建无创建者空间失败：%v", err)
	}
	_, createdBy, updatedBy = readCols("ws_w2")
	if createdBy != nil {
		t.Errorf("CreatedBy 为空时 created_by 列应是 NULL，实际 %s——"+
			"「没记过」与「空串」混在一起，`is null` 会查不全", showPtr(createdBy))
	}
	if updatedBy != nil {
		t.Errorf("UpdatedBy 为空时 updated_by 列应是 NULL，实际 %s", showPtr(updatedBy))
	}
}

// TestSeedFillsWorkspaceColumns pins that the seed-loading write path also writes the new columns.
//
// insertSeed is the **third** write path, with its own SQL separate from CreateWorkspace. If it is missed,
// the workspace columns loaded on the first start with an empty database are all NULL.
func TestSeedFillsWorkspaceColumns(t *testing.T) {
	pgEnvOnce.Do(initPGEnv)
	if pgEnv.skip != "" {
		t.Skip(pgEnv.skip)
	}
	ctx := context.Background()

	s, err := NewPGStore(ctx, pgEnv.client)
	if err != nil {
		t.Fatalf("建 PGStore 失败：%v", err)
	}
	if err := s.Clear(ctx); err != nil {
		t.Fatalf("清库失败：%v", err)
	}
	defer func() {
		if err := s.Clear(ctx); err != nil {
			t.Logf("收尾清库失败：%v", err)
		}
	}()

	seed, err := persistmodel.LoadSeed()
	if err != nil {
		t.Fatalf("读种子失败：%v", err)
	}
	if len(seed.Workspaces) == 0 {
		t.Fatal("种子里一个空间都没有，这条用例失去意义")
	}
	// Reset = truncate + insertSeed, which is exactly the path being verified.
	if err := s.Reset(ctx); err != nil {
		t.Fatalf("装种子失败：%v", err)
	}

	for _, ws := range seed.Workspaces {
		var name *string
		if err := pgEnv.client.QueryRow(ctx,
			`select name from workspaces where id = $1`, ws.ID).Scan(&name); err != nil {
			t.Fatalf("回读种子空间 %s 失败：%v", ws.ID, err)
		}
		if name == nil || *name != ws.Name {
			t.Errorf("种子空间 %s 的 name 列没写对：想要 %q，实际 %s——"+
				"insertSeed 是独立的第三条写路径，漏改它零报错", ws.ID, ws.Name, showPtr(name))
		}
	}
}

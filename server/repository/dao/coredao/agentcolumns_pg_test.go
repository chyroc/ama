// Integration tests for the scalar columns of agents (name / description / status / version / times / creator / executor):
// write paths write columns, read paths read columns, and doc only holds the fields not pulled out.
//
// All tests in this file are skipped as a group when the database is unreachable, using the same check as store_pg_test.go (pgEnv.skip).

package coredao

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// showIntPtr renders an *int in a human-readable form; NULL and 0 must be easy to tell apart.
//
// ⚠️ For version this difference is not pedantry: 0 is **a valid initial value for optimistic locking**, while NULL means
// "this column was not written". Showing both as the same thing makes debugging take a detour.
func showIntPtr(p *int) string {
	if p == nil {
		return "NULL"
	}
	return strconv.Itoa(*p)
}

// TestAgentDoubleWriteFillsColumns pins "all three write paths write the columns correctly".
//
// It reads the **columns** back directly, not through the read path, so a self-consistent error like "both read and write miss the same field"
// can only be caught by it.
//
// The three write paths come from `grep -n "insert into agents\|update agents"`:
// CreateAgent / SaveAgent / insertSeed (seed loading is the easiest to miss; it is two thousand lines away).
func TestAgentDoubleWriteFillsColumns(t *testing.T) {
	pgEnvOnce.Do(initPGEnv)
	if pgEnv.skip != "" {
		t.Skip(pgEnv.skip)
	}
	ctx := context.Background()

	store, err := NewPGStore(ctx, pgEnv.client)
	if err != nil {
		t.Fatalf("建 store 失败：%v", err)
	}
	if err := store.Clear(ctx); err != nil {
		t.Fatalf("清库失败：%v", err)
	}

	// Every field has a different value, for the same reason as above: if all were "x", an implementation that swaps columns would pass too.
	a := apimodel.Agent{
		ID: "ag_dw", WorkspaceID: "ws_dw",
		Name: "双写助手", Description: "双写简介",
		Status: apimodel.StatusDisabled, Version: 3,
		CreatedAt: "2026-05-06T07:08:09.123Z", UpdatedAt: "2026-06-07T08:09:10.234Z",
		CreatedBy: "c@example.com", UpdatedBy: "u@example.com",
		// executor_type / host_id / host_source also take values different from each other and from the other fields,
		// so swapped columns can be detected.
		ExecutorType: "host", HostID: "hst_dw", HostSource: "personal",
	}
	if _, err := store.CreateAgent(ctx, persistmodel.CloneAgent(a)); err != nil {
		t.Fatalf("建 agent 失败：%v", err)
	}
	assertAgentColumns(t, ctx, a, "CreateAgent")

	// SaveAgent: switch to a set of **brand-new values**, to check the update includes all 11 columns too.
	// Changing only one field, "update forgot some column" would be hidden by the old values.
	a.Name = "改名后"
	a.Description = "改过的简介"
	a.Status = apimodel.StatusActive
	a.Version = 4
	a.UpdatedAt = "2026-07-08T09:10:11.345Z"
	a.UpdatedBy = "u2@example.com"
	// Switch the executor from host back to the default Docker container: the three columns should become NULL (empty values go through nullIfEmpty),
	// which also checks the "unbind executor" path; otherwise an implementation missing columns would be hidden by the old values.
	a.ExecutorType = ""
	a.HostID = ""
	a.HostSource = ""
	if _, err := store.SaveAgent(ctx, persistmodel.CloneAgent(a)); err != nil {
		t.Fatalf("存 agent 失败：%v", err)
	}
	assertAgentColumns(t, ctx, a, "SaveAgent")

	// insertSeed: go through Reset (truncate + insertSeed) with **the real seed** instead of made-up
	// data. insertSeed has only this one entry point, and a made-up Seed would bypass its real calling shape.
	seed, err := persistmodel.LoadSeed()
	if err != nil {
		t.Fatalf("读种子失败：%v", err)
	}
	if len(seed.Agents) == 0 {
		t.Fatal("种子里一个 agent 都没有，这条用例失去意义")
	}
	if err := store.Reset(ctx); err != nil {
		t.Fatalf("装种子失败：%v", err)
	}
	for _, sa := range seed.Agents {
		assertAgentColumns(t, ctx, sa, "insertSeed")
	}
}

// assertAgentColumns reads the **columns** back (not doc) and compares them field by field with the expected Agent.
//
// It deliberately selects specific column names instead of `select *`: with a missing column the former errors, the latter silently compares one less.
func assertAgentColumns(t *testing.T, ctx context.Context, want apimodel.Agent, path string) {
	t.Helper()
	var name, description, status, createdAt, updatedAt, createdBy, updatedBy,
		executorType, hostID, hostSource *string
	var version *int
	err := pgEnv.client.QueryRow(ctx,
		`select name, description, status, version,
		        created_at, updated_at, created_by, updated_by,
		        executor_type, host_id, host_source
		   from agents where id = $1`, want.ID).
		Scan(&name, &description, &status, &version,
			&createdAt, &updatedAt, &createdBy, &updatedBy,
			&executorType, &hostID, &hostSource)
	if err != nil {
		t.Fatalf("%s：回读列失败：%v", path, err)
	}

	eq := func(col string, got *string, exp string) {
		t.Helper()
		if got == nil {
			t.Errorf("%s 之后 %s 列是 NULL，想要 %q——这条写路径漏了双写", path, col, exp)
			return
		}
		if *got != exp {
			t.Errorf("%s 之后 %s 列是 %q，想要 %q", path, col, *got, exp)
		}
	}
	eq("name", name, want.Name)
	eq("description", description, want.Description)
	eq("status", status, string(want.Status))
	eq("created_at", createdAt, want.CreatedAt)
	eq("updated_at", updatedAt, want.UpdatedAt)

	// ⚠️ created_by / updated_by use **a different check**; the eq above cannot be used.
	//
	// These two columns go through nullIfEmpty: empty values are written as SQL NULL, not an empty string (see the comment on agentColumnValues;
	// otherwise the same "unknown who" would have two shapes in the database).
	// So the correct result for "Agent.CreatedBy is an empty string" is **NULL**; that is not a missing write.
	//
	// This difference really happens: **the agents in the seed do not have these two fields**.
	nullable := func(col string, got *string, exp string) {
		t.Helper()
		if exp == "" {
			if got != nil {
				t.Errorf("%s 之后 %s 列是 %q，想要 NULL——空值必须过 nullIfEmpty 写成 NULL，"+
					"「没记过」与「空串」要分开；两种形态混在库里，按 is null 找「缺创建者」的行会漏掉一半",
					path, col, *got)
			}
			return
		}
		eq(col, got, exp)
	}
	nullable("created_by", createdBy, want.CreatedBy)
	nullable("updated_by", updatedBy, want.UpdatedBy)
	// executor_type / host_id / host_source also go through nullIfEmpty: an empty string (default Docker container /
	// no machine picked / no source recorded) must be stored as NULL; non-empty values must be exactly equal.
	nullable("executor_type", executorType, want.ExecutorType)
	nullable("host_id", hostID, want.HostID)
	nullable("host_source", hostSource, want.HostSource)

	if version == nil {
		t.Errorf("%s 之后 version 列是 NULL，想要 %d——这条写路径漏了双写", path, want.Version)
	} else if *version != want.Version {
		t.Errorf("%s 之后 version 列是 %d，想要 %d", path, *version, want.Version)
	}
}

// TestAgentReadPathUsesColumnsNotDoc pins "the 8 fields pulled out are read from columns, not doc".
//
// # Why the whole suite could not prove this before
//
// In normally built data the columns and doc mean the same thing, so "read columns" and "read doc" may give the same
// result, and **changing the select back to `select doc` still compiles**.
//
// The only way to tell them apart is to build a row where **columns and doc deliberately differ**, then assert what is read is the columns.
// Such rows should not exist in production, but **the columns must be authoritative**: write paths no longer write these keys into doc,
// so still reading doc means reading empty values.
func TestAgentReadPathUsesColumnsNotDoc(t *testing.T) {
	pgEnvOnce.Do(initPGEnv)
	if pgEnv.skip != "" {
		t.Skip(pgEnv.skip)
	}
	ctx := context.Background()

	store, err := NewPGStore(ctx, pgEnv.client)
	if err != nil {
		t.Fatalf("建 store 失败：%v", err)
	}
	if err := store.Clear(ctx); err != nil {
		t.Fatalf("清库失败：%v", err)
	}

	if _, err := store.CreateAgent(ctx, apimodel.Agent{
		ID: "ag_skew", WorkspaceID: "ws_skew", Name: "doc 里的名字",
		Description: "doc 里的简介", Status: apimodel.StatusActive, Version: 1,
		CreatedAt: "2026-01-01T00:00:00.000Z", UpdatedAt: "2026-01-01T00:00:00.000Z",
		CreatedBy: "doc@example.com", UpdatedBy: "doc@example.com",
		// In doc this agent is bound to another machine; below only the columns are changed to check "columns override doc".
		ExecutorType: "host", HostID: "hst_doc", HostSource: "personal",
	}); err != nil {
		t.Fatalf("建 agent 失败：%v", err)
	}
	// Change only the columns, not doc, simulating "columns updated, doc still old".
	// **All 11 fields are skewed**: changing only one would let an implementation where "the other 10 still read doc" slip through.
	if _, err := pgEnv.client.Exec(ctx, `
		update agents
		   set name = '列里的名字', description = '列里的简介', status = 'disabled',
		       version = 42, created_at = '2026-09-09T09:09:09.999Z',
		       updated_at = '2026-10-10T10:10:10.888Z',
		       created_by = 'col@example.com', updated_by = 'col2@example.com',
		       executor_type = 'host', host_id = 'hst_col', host_source = 'workspace'
		 where id = 'ag_skew'`); err != nil {
		t.Fatalf("造偏差失败：%v", err)
	}

	got, err := store.GetAgent(ctx, "ag_skew")
	if err != nil {
		t.Fatalf("查 agent 失败：%v", err)
	}

	hint := "——读路径还在读 doc？列才是权威值"
	for _, c := range []struct{ col, got, want string }{
		{"name", got.Name, "列里的名字"},
		{"description", got.Description, "列里的简介"},
		{"status", string(got.Status), "disabled"},
		{"createdAt", got.CreatedAt, "2026-09-09T09:09:09.999Z"},
		{"updatedAt", got.UpdatedAt, "2026-10-10T10:10:10.888Z"},
		{"createdBy", got.CreatedBy, "col@example.com"},
		{"updatedBy", got.UpdatedBy, "col2@example.com"},
		{"executorType", got.ExecutorType, "host"},
		{"hostId", got.HostID, "hst_col"},
		{"hostSource", got.HostSource, "workspace"},
	} {
		if c.got != c.want {
			t.Errorf("%s 读到 %q，想要 %q%s", c.col, c.got, c.want, hint)
		}
	}
	if got.Version != 42 {
		t.Errorf("version 读到 %d，想要 42%s", got.Version, hint)
	}

	// ListAgents uses another SQL and must be checked separately: an implementation that only changed GetAgent would pass all the assertions above.
	list, err := store.ListAgents(ctx, "ws_skew")
	if err != nil {
		t.Fatalf("列 agent 失败：%v", err)
	}
	if len(list) != 1 {
		t.Fatalf("想要 1 个 agent，实际 %d 个", len(list))
	}
	if list[0].Name != "列里的名字" || list[0].Version != 42 {
		t.Errorf("ListAgents 读到 name=%q version=%d，想要「列里的名字」/42%s",
			list[0].Name, list[0].Version, hint)
	}
}

// TestAgentReadPathStillLoadsDocOnlyFields pins "fields **not pulled out** are still read from doc".
//
// # Why this one is even easier to miss than the previous
//
// The key difference between agents and workspaces: that table has no doc column; **this table only pulled out the scalars**, and system / model / skillIds /
// feishuAppIds / envs / integrationIds and so on **still exist only in doc**.
//
// So "casually dropping doc from the select" following the workspaces shape is an extremely natural move,
// and the result is that all those fields **become zero values**: the agent's system prompt is gone, the model is gone,
// the bound skills and Feishu apps are all empty. **Zero errors**: struct fields have zero values, JSON serializes as usual,
// and the page shows "the config was suddenly wiped", with nothing pointing at this change.
//
// The previous test cannot stop it (it only looks at the 8 pulled-out fields, unaffected by whether doc is dropped).
func TestAgentReadPathStillLoadsDocOnlyFields(t *testing.T) {
	pgEnvOnce.Do(initPGEnv)
	if pgEnv.skip != "" {
		t.Skip(pgEnv.skip)
	}
	ctx := context.Background()

	store, err := NewPGStore(ctx, pgEnv.client)
	if err != nil {
		t.Fatalf("建 store 失败：%v", err)
	}
	if err := store.Clear(ctx); err != nil {
		t.Fatalf("清库失败：%v", err)
	}

	want := apimodel.Agent{
		ID: "ag_doc", WorkspaceID: "ws_doc", Name: "有配置的助手",
		System:         "你是一个严谨的助手",
		Model:          apimodel.ModelConfig{ID: "doubao-seed-2-1-pro-260628"},
		BuiltinTools:   []string{"bash", "read"},
		SkillIDs:       []string{"skill-a", "skill-b"},
		FeishuAppIDs:   []string{"fs_1"},
		IntegrationIDs: []string{"example_integration"},
		Envs:           []apimodel.AgentEnv{{Key: "FOO", Value: "bar"}},
	}
	if _, err := store.CreateAgent(ctx, persistmodel.CloneAgent(want)); err != nil {
		t.Fatalf("建 agent 失败：%v", err)
	}

	got, err := store.GetAgent(ctx, "ag_doc")
	if err != nil {
		t.Fatalf("查 agent 失败：%v", err)
	}

	hint := "——select 里是不是把 doc 摘掉了？这张表只抽了 8 个标量，其余字段只在 doc 里"
	if got.System != want.System {
		t.Errorf("system 读到 %q，想要 %q%s", got.System, want.System, hint)
	}
	// ⚠️ The field in ModelConfig is called ID, not Model (Ark's field name); the type comment says so explicitly.
	if got.Model.ID != want.Model.ID {
		t.Errorf("model 读到 %q，想要 %q%s", got.Model.ID, want.Model.ID, hint)
	}
	if strings.Join(got.SkillIDs, ",") != "skill-a,skill-b" {
		t.Errorf("skillIds 读到 %v，想要 [skill-a skill-b]%s", got.SkillIDs, hint)
	}
	if strings.Join(got.FeishuAppIDs, ",") != "fs_1" {
		t.Errorf("feishuAppIds 读到 %v，想要 [fs_1]%s", got.FeishuAppIDs, hint)
	}
	if strings.Join(got.BuiltinTools, ",") != "bash,read" {
		t.Errorf("builtin_tools 读到 %v，想要 [bash read]%s", got.BuiltinTools, hint)
	}
	if strings.Join(got.IntegrationIDs, ",") != "example_integration" {
		t.Errorf("integrationIds 读到 %v，想要 [example_integration]%s", got.IntegrationIDs, hint)
	}
	if len(got.Envs) != 1 || got.Envs[0].Key != "FOO" || got.Envs[0].Value != "bar" {
		t.Errorf("envs 读到 %v，想要 [{FOO bar}]%s", got.Envs, hint)
	}
}

// TestAgentWritePathStopsWritingDocKeys pins "the write path does not write those 8 fields into doc".
//
// # Why this must be its own test: no existing test catches it
//
// The write-columns test asserts on **columns** and passes whether or not doc is written; the read-columns test asserts on **the Agent
// read back**, and the read path already overrides doc with columns, so it cannot see whether doc has those 8 keys either.
// That is, **changing marshalAgentDoc back to marshalDoc leaves the whole suite all green**;
// that is exactly why this test exists.
//
// # The check is on "the values of those 8 keys in doc", not "whether the keys exist"
//
// Only createdBy / updatedBy, which have omitempty, disappear entirely; the other 6 have no omitempty,
// so after zeroing doc still keeps `"name":""`. So the assertions have two levels:
//
//   - createdBy / updatedBy: **the key must not exist** (`doc ? 'x'` is false);
//   - the other 6: **the key may exist, but the value must be the zero value**; it must not carry real data anymore.
//
// ⚠️ It also checks "**the 10 fields not pulled out are still written into doc as usual**". The most dangerous mistake in this round is
// copying workspaces and stopping doc entirely (that table pulled out all 9 keys, this one did not),
// which would make system / model / skillIds and so on **never written again**, with zero errors.
func TestAgentWritePathStopsWritingDocKeys(t *testing.T) {
	pgEnvOnce.Do(initPGEnv)
	if pgEnv.skip != "" {
		t.Skip(pgEnv.skip)
	}
	ctx := context.Background()

	store, err := NewPGStore(ctx, pgEnv.client)
	if err != nil {
		t.Fatalf("建 store 失败：%v", err)
	}
	if err := store.Clear(ctx); err != nil {
		t.Fatalf("清库失败：%v", err)
	}

	a := apimodel.Agent{
		ID: "ag_stop", WorkspaceID: "ws_stop",
		Name: "停写助手", Description: "停写简介",
		Status: apimodel.StatusDisabled, Version: 11,
		CreatedAt: "2026-11-12T13:14:15.161Z", UpdatedAt: "2026-11-13T14:15:16.171Z",
		CreatedBy: "c@example.com", UpdatedBy: "u@example.com",
		// Give the three executor columns non-empty values: then "marshalAgentDoc forgot to zero it" gets killed by the
		// doc ? 'executorType' assertion above; with empty values the key would not be there anyway, and it could not be detected.
		ExecutorType: "host", HostID: "hst_stop", HostSource: "personal",
		// Fields not pulled out: must go into doc as usual.
		System: "系统提示词还在", Model: apimodel.ModelConfig{ID: "doubao-seed-2-1-pro-260628"},
		SkillIDs: []string{"sk-1"}, FeishuAppIDs: []string{"fs-1"},
	}
	if _, err := store.CreateAgent(ctx, persistmodel.CloneAgent(a)); err != nil {
		t.Fatalf("建 agent 失败：%v", err)
	}
	assertAgentDocStripped(t, ctx, "ag_stop", "CreateAgent")

	// SaveAgent is another write path, checked separately: an implementation that only changed CreateAgent would pass the test above.
	a.Name = "改名后"
	a.Version = 12
	if _, err := store.SaveAgent(ctx, persistmodel.CloneAgent(a)); err != nil {
		t.Fatalf("存 agent 失败：%v", err)
	}
	assertAgentDocStripped(t, ctx, "ag_stop", "SaveAgent")

	// insertSeed is the third, going through Reset with the real seed.
	seed, err := persistmodel.LoadSeed()
	if err != nil {
		t.Fatalf("读种子失败：%v", err)
	}
	if len(seed.Agents) == 0 {
		t.Fatal("种子里一个 agent 都没有，这条用例失去意义")
	}
	if err := store.Reset(ctx); err != nil {
		t.Fatalf("装种子失败：%v", err)
	}
	for _, sa := range seed.Agents {
		assertAgentDocStripped(t, ctx, sa.ID, "insertSeed")
	}
}

// assertAgentDocStripped asserts that the keys pulled out into columns no longer carry information in doc, while the other fields are still there.
func assertAgentDocStripped(t *testing.T, ctx context.Context, id, path string) {
	t.Helper()
	var hasCreatedBy, hasUpdatedBy, hasExecutorType, hasHostID, hasHostSource bool
	var name, description, status, createdAt, updatedAt *string
	var version *int
	var system *string
	err := pgEnv.client.QueryRow(ctx, `
		select doc ? 'createdBy', doc ? 'updatedBy',
		       doc ? 'executorType', doc ? 'hostId', doc ? 'hostSource',
		       doc ->> 'name', doc ->> 'description', doc ->> 'status',
		       doc ->> 'createdAt', doc ->> 'updatedAt',
		       (doc ->> 'version')::int, doc ->> 'system'
		  from agents where id = $1`, id).
		Scan(&hasCreatedBy, &hasUpdatedBy, &hasExecutorType, &hasHostID, &hasHostSource,
			&name, &description, &status,
			&createdAt, &updatedAt, &version, &system)
	if err != nil {
		t.Fatalf("%s：回读 doc 失败：%v", path, err)
	}

	// ---- The four with omitempty: the key must disappear entirely ----
	if hasCreatedBy {
		t.Errorf("%s 之后 doc 里还有 createdBy 键——写路径没停写？"+
			"（它带 omitempty，置零之后整个键应该消失）", path)
	}
	if hasUpdatedBy {
		t.Errorf("%s 之后 doc 里还有 updatedBy 键——写路径没停写？", path)
	}
	// The fixture gave these two keys non-empty values, so if they are still here, marshalAgentDoc forgot to zero them.
	if hasExecutorType {
		t.Errorf("%s 之后 doc 里还有 executorType 键——marshalAgentDoc 漏置零？", path)
	}
	if hasHostID {
		t.Errorf("%s 之后 doc 里还有 hostId 键——marshalAgentDoc 漏置零？", path)
	}
	// hostSource never goes into doc (marshalAgentDoc zeroes it unconditionally, and its lack of omitempty does not
	// matter either: after zeroing, the zero value still does not write the key). If it is still there, zeroing was missed.
	if hasHostSource {
		t.Errorf("%s 之后 doc 里还有 hostSource 键——marshalAgentDoc 漏置零？", path)
	}

	// ---- The other 6: the key may exist, but must not carry a real value anymore ----
	blank := func(col string, got *string) {
		t.Helper()
		if got != nil && *got != "" {
			t.Errorf("%s 之后 doc.%s 还是 %q——写路径仍在往 doc 里写这个字段。"+
				"列已经是唯一真源，doc 里再留一份就会漂移", path, col, *got)
		}
	}
	blank("name", name)
	blank("description", description)
	blank("status", status)
	blank("createdAt", createdAt)
	blank("updatedAt", updatedAt)
	if version != nil && *version != 0 {
		t.Errorf("%s 之后 doc.version 还是 %d，想要 0（或键不存在）", path, *version)
	}

	// ---- Fields not pulled out must still be there (seed agents do not always have system, so only check when non-empty) ----
	if path != "insertSeed" && (system == nil || *system == "") {
		t.Errorf("%s 之后 doc.system 空了——是不是照抄 workspaces 把 doc 整个停写了？"+
			"这张表还有 10 个字段只存在于 doc，停掉它们就永久写不进去了", path)
	}
}

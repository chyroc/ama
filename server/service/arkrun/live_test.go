// Wiring tests for LiveRunner: **they check that fields really reach the wire**, not just that
// functions were called.
//
// # Why this file is required
//
// When the "configured 3 skills, asked the agent what skills it has, it said none" failure
// happened, **every unit test was green**:
//
//   - on the core side: skillark_test.go / skillbuiltin_test.go assert that `runner.spec.Skills`
//     is filled correctly, and fakeRunner receives core's `AgentSpec`, which really was right;
//   - on the ma side: TestAgentSpecToRequest checks the `ma.AgentSpec` -> request body translation,
//     and that layer was fine too.
//
// The field was lost in the **gap between the two**: the hand-written `ma.AgentSpec{...}` literal
// in `LiveRunner.CreateAgent` missed `Skills`. And `grep LiveRunner *_test.go` returned **zero
// results** back then: the production implementation had not a single test, and neither side's
// tests could cover it.
//
// This is the same pattern as the repo's earlier note "a purely new module can be all green yet
// have no effect": nobody tests the wiring.
//
// Here httptest starts an Ark stand-in, the real `LiveRunner` calls it, and then **the HTTP request
// body actually sent is read**. That is the only thing that proves "the field reached Ark";
// asserting on a struct at any layer in between would miss a drop at the next layer.

package arkrun

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/shared/infra/ma"
)

// newFakeArkAgents starts an Ark stand-in that only implements create Agent, and hands over the
// request bodies it receives.
//
// echoSkills decides how many skills the response body echoes back: the real Ark echoes the ones in
// the request (verified with probe-agent-skills -echo-check), and "echo 0" is exactly the shape when
// they are dropped.
func newFakeArkAgents(t *testing.T, echoSkills func(reqSkills []any) []any) (*LiveRunner, *map[string]any) {
	t.Helper()

	var lastBody map[string]any

	mux := http.NewServeMux()
	mux.HandleFunc("/agents", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("解请求体失败: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		lastBody = body

		reqSkills, _ := body["skills"].([]any)
		// The response body is a **fixed minimal valid shape** and does not echo the tools / model in
		// the request.
		//
		// The assertions read the **request body** (lastBody); the response only needs to be decodable
		// by the SDK, so it does not need to echo like the real Ark. Echoing was tried and hit a trap:
		// those two fields have different shapes on the request side and the response side, giving an
		// error that looks like "our request is wrong".
		//
		// ⚠️ This minimal shape was found by trial; none of the four fields can be left out, and **the
		// error for any missing one happens while decoding the response, yet looks like a problem with
		// the request** (the `RequestError code: 200` prefix is especially misleading: 200 is the
		// success status code, and the failure is in client-side decoding):
		//
		//	type          fixed "agent"; missing gives `invalid: type (field required)`
		//	created_at    a **string**, not a number; a number gives `unexpected byte 49 '1'`
		//	updated_at    same as above
		//	skills        **must not be JSON null** (declared as an array); null gives `"[" expected`
		echoed := echoSkills(reqSkills)
		if echoed == nil {
			echoed = []any{}
		}
		resp := map[string]any{
			"id":         "agent-fake-wire",
			"type":       "agent",
			"name":       "fake",
			"version":    1,
			"created_at": "2026-08-30T00:00:00.000Z",
			"updated_at": "2026-08-30T00:00:00.000Z",
			"skills":     echoed,
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("写响应失败: %v", err)
		}
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	r := NewLive()
	r.BaseURLOverride = srv.URL
	return r, &lastBody
}

// echoAll echoes the skills in the request as they are, mimicking the real Ark.
func echoAll(reqSkills []any) []any { return reqSkills }

// testArkKey is the coordinates for the stand-in. **APIKey must not be empty**: ma.New rejects an
// empty key (returning "missing API Key"), and that error looks like a credentials problem, not a
// mistake in the test.
func testArkKey() BindingKey {
	return BindingKey{WorkspaceID: "ws-1", AgentID: "agent-1", APIKey: "dummy-key"}
}

// TestLiveCreateAgentPutsSkillsOnTheWire is the one that **really blocks that failure**.
//
// It reads the `skills` array in the HTTP request body actually sent, so a drop at any layer in
// between turns it red: delete the line `Skills: toMASkillRefs(spec.Skills)` in arkrun.go and this
// fails right away.
//
// It asserts down to skill_id, not just the count: counting alone would still pass the shape
// "passed, but every id empty", and an empty skill_id on the Ark side means attaching a skill whose
// content cannot be found, which is equally broken with zero errors.
func TestLiveCreateAgentPutsSkillsOnTheWire(t *testing.T) {
	runner, body := newFakeArkAgents(t, echoAll)

	created, err := runner.CreateAgent(context.Background(), testArkKey(), AgentSpec{
		Name:  "n",
		Model: "m",
		Skills: []persistmodel.ArkSkillRef{
			{ArkSkillID: "skill-ark-1", Version: "1"},
			{ArkSkillID: "skill-ark-2", Version: "3"},
		},
	})
	if err != nil {
		t.Fatalf("建 Agent 失败: %v", err)
	}

	skills, ok := (*body)["skills"].([]any)
	if !ok {
		t.Fatalf("请求体里压根没有 skills 字段——技能在 core→ma 边界被丢了。实际请求体: %v", *body)
	}
	if len(skills) != 2 {
		t.Fatalf("请求体里应有 2 条技能，实际 %d 条: %v", len(skills), skills)
	}

	// Check skill_id and type one by one: if type is missing, Ark returns 400 (it is required),
	// and that error is an English InvalidParameter that does not point here.
	want := []struct{ id, version string }{{"skill-ark-1", "1"}, {"skill-ark-2", "3"}}
	for i, w := range want {
		got, _ := skills[i].(map[string]any)
		if got["skill_id"] != w.id {
			t.Errorf("第 %d 条 skill_id = %v，期望 %s", i+1, got["skill_id"], w.id)
		}
		if got["version"] != w.version {
			t.Errorf("第 %d 条 version = %v，期望 %s", i+1, got["version"], w.version)
		}
		if got["type"] != "custom" {
			t.Errorf("第 %d 条 type = %v，期望 custom（漏了方舟会 400）", i+1, got["type"])
		}
	}

	// The return value must carry the count Ark confirmed: the caller uses it to decide whether it
	// may write back the fingerprint.
	if created.SkillCount != 2 {
		t.Errorf("SkillCount = %d，期望 2（方舟回显了 2 条）", created.SkillCount)
	}
	if created.ID != "agent-fake-wire" {
		t.Errorf("ID = %q", created.ID)
	}
}

// TestLiveCreateAgentReportsSkillCountFromArk locks in "the count comes from Ark's answer, not our input".
//
// This is the flip side of the previous one: even if we pass 2, when Ark accepts only 0 it must be
// reported truthfully; otherwise ensureBinding would again write a false fingerprint into the
// binding and block the repair path (see CreatedAgent).
func TestLiveCreateAgentReportsSkillCountFromArk(t *testing.T) {
	// The stand-in accepts the request but echoes empty skills: exactly the shape of "Ark did not attach them".
	runner, _ := newFakeArkAgents(t, func([]any) []any { return nil })

	created, err := runner.CreateAgent(context.Background(), testArkKey(), AgentSpec{
		Name: "n", Model: "m",
		Skills: []persistmodel.ArkSkillRef{{ArkSkillID: "skill-ark-1"}},
	})
	if err != nil {
		t.Fatalf("建 Agent 失败: %v", err)
	}
	if created.SkillCount != 0 {
		t.Errorf("SkillCount = %d，期望 0——条数必须来自方舟的回答，"+
			"照抄入参会让「方舟其实没挂上」这件事被永久写成假账", created.SkillCount)
	}
}

// TestLiveCreateAgentOmitsSkillsWhenNone locks in "no skills means no skills key".
//
// "Not sent" and "sent as an empty array" are two different things on the Ark side (the latter
// **clears**, see the comment on ma.toSkillRefs). When creating an Agent both give the same result,
// so this does not guard current correctness; it stops people from thinking they can casually
// change it to `[]`, a change that is destructive on the update path.
func TestLiveCreateAgentOmitsSkillsWhenNone(t *testing.T) {
	runner, body := newFakeArkAgents(t, echoAll)

	if _, err := runner.CreateAgent(context.Background(), testArkKey(), AgentSpec{
		Name: "n", Model: "m",
	}); err != nil {
		t.Fatalf("建 Agent 失败: %v", err)
	}
	if v, present := (*body)["skills"]; present && v != nil {
		t.Errorf("没有技能时不该下发 skills 键，实际 %v", v)
	}
}

// TestArkAgentSpecMapsEveryFieldToMA is the one that **prevents the next time**.
//
// The cases above guard the single Skills field. But the root cause was not "Skills is especially
// easy to miss"; it was that the `ma.AgentSpec{...}` in `LiveRunner.CreateAgent` is **written field
// by field by hand**: add a field to AgentSpec, forget to carry it in that literal, and the compiler
// says nothing, Ark still returns 200, and existing tests are all green.
//
// So this uses reflection to watch **every field** of AgentSpec: put a recognizable non-zero value
// in it, and require it to leave a trace in the request body actually sent. A new field that is not
// wired up turns this red and names the field directly.
//
// # Why not write per-field assertions by hand
//
// Per-field assertions only guard "fields that exist today", and this failure was exactly "a field
// added later was not wired up": a hand-written list needs someone to remember to keep it in sync,
// which is the very step that already failed once.
// The reflection version automatically covers "another field added later", without relying on
// anyone remembering.
func TestArkAgentSpecMapsEveryFieldToMA(t *testing.T) {
	// For each field: "what to fill in" and "how to recognize it in the request body".
	//
	// When adding a field, this table needs a new entry too, or the subtest fails and tells you what
	// to add. That is deliberate: it forces whoever adds a field to think "is it really passed down?".
	cases := map[string]struct {
		fill   func(*AgentSpec)
		marker string // string that must appear in the request body JSON
	}{
		"Name":        {func(s *AgentSpec) { s.Name = "probe-name" }, "probe-name"},
		"Model":       {func(s *AgentSpec) { s.Model = "probe-model" }, "probe-model"},
		"Description": {func(s *AgentSpec) { s.Description = "probe-desc" }, "probe-desc"},
		"System":      {func(s *AgentSpec) { s.System = "probe-system" }, "probe-system"},
		"Speed":       {func(s *AgentSpec) { s.Speed = "fast" }, "fast"},
		"Tools": {func(s *AgentSpec) {
			s.Tools = []string{"probe_toolset"}
		}, "probe_toolset"},
		"CustomTools": {func(s *AgentSpec) {
			s.CustomTools = []ma.CustomToolSpec{{
				Name: "probe_tool", Description: "d",
			}}
		}, "probe_tool"},
		"Skills": {func(s *AgentSpec) {
			s.Skills = []persistmodel.ArkSkillRef{{ArkSkillID: "probe-skill-id"}}
		}, "probe-skill-id"},
	}

	// Reconcile first: are all AgentSpec fields in the table?
	//
	// This step catches the problem earlier than the subtests: add a field without adding it to the
	// table and this goes red, instead of waiting until production someday shows "configured but has
	// no effect".
	specType := reflect.TypeOf(AgentSpec{})
	for i := 0; i < specType.NumField(); i++ {
		name := specType.Field(i).Name
		if _, ok := cases[name]; !ok {
			t.Errorf("AgentSpec 新增了字段 %s，但本用例的表里没有它——"+
				"请补一条（填一个可判别的值 + 它在请求体里的痕迹），"+
				"确认这个字段真的被传给方舟了。漏传是零报错故障，"+
				"见 arkrun.go 的 CreateAgent。", name)
		}
	}

	for field, tc := range cases {
		t.Run(field, func(t *testing.T) {
			runner, body := newFakeArkAgents(t, echoAll)

			// Fill in only this one field (Name/Model are required and the SDK rejects them otherwise,
			// so they are always included).
			spec := AgentSpec{Name: "n", Model: "m"}
			tc.fill(&spec)

			if _, err := runner.CreateAgent(context.Background(), testArkKey(), spec); err != nil {
				t.Fatalf("建 Agent 失败: %v", err)
			}
			raw, err := json.Marshal(*body)
			if err != nil {
				t.Fatalf("重新编码请求体失败: %v", err)
			}
			if !strings.Contains(string(raw), tc.marker) {
				t.Errorf("字段 %s 没有出现在发给方舟的请求体里——它在 core→ma 的翻译里被丢了。\n"+
					"期望请求体含 %q\n实际请求体: %s", field, tc.marker, raw)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// UpdateAgentSpec wiring: check that system / model / speed / custom tools really reach the wire
// ---------------------------------------------------------------------------

// newFakeArkAgentUpdate starts an Ark stand-in that implements "Get + Update".
//
// On Ark these two APIs are **the same path** (/agents/{id}), told apart by HTTP method: GET reads,
// POST updates (not PATCH/PUT, following the SDK's UpdateAgent implementation).
//
// conflicts controls how many Updates return 409 at first: used to check retries on version conflicts.
// It returns every update request body received, in order: **we need to see what the retry sent**.
func newFakeArkAgentUpdate(t *testing.T, conflicts int) (*LiveRunner, *[]map[string]any, *int) {
	t.Helper()

	var bodies []map[string]any
	version := int32(7) // a value other than 0/1, to check "the version comes from Get, not hard-coded"
	gets := 0

	mux := http.NewServeMux()
	mux.HandleFunc("/agents/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gets++
			writeFakeAgent(t, w, version)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("解更新请求体失败: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		bodies = append(bodies, body)
		if len(bodies) <= conflicts {
			// Mimic another instance having just written: the version moves forward one step, and this
			// attempt of ours is rejected.
			version++
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"ResourceConflict","message":"version mismatch"}}`))
			return
		}
		version++
		writeFakeAgent(t, w, version)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	r := NewLive()
	r.BaseURLOverride = srv.URL
	return r, &bodies, &gets
}

// writeFakeAgent writes a minimal valid Agent response (none of the four fields can be left out;
// see the paragraph in newFakeArkAgents: the error for any missing one looks like a request problem).
func writeFakeAgent(t *testing.T, w http.ResponseWriter, version int32) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"id":         "agent-fake-wire",
		"type":       "agent",
		"name":       "fake",
		"version":    version,
		"created_at": "2026-08-30T00:00:00.000Z",
		"updated_at": "2026-08-30T00:00:00.000Z",
		"skills":     []any{},
	}); err != nil {
		t.Errorf("写响应失败: %v", err)
	}
}

// TestLiveUpdateAgentSpecPutsEveryFieldOnTheWire is the **only case on this path that proves the
// fields reached Ark**.
//
// The core-side fakeRunner receives `AgentSpec`; it being filled correctly **does not mean** the ma
// layer passed it down. The skills failure happened exactly in this gap (core computed it, the ma
// literal dropped it, Ark returned 200). So this reads the HTTP request body actually sent.
//
// Mutation check: delete `upd.System = spec.System` in LiveRunner.UpdateAgentSpec
// -> this case turns red.
func TestLiveUpdateAgentSpecPutsEveryFieldOnTheWire(t *testing.T) {
	runner, bodies, gets := newFakeArkAgentUpdate(t, 0)

	err := runner.UpdateAgentSpec(context.Background(), testArkKey(), "agent-fake-wire", AgentSpec{
		Name:        "probe-name",
		Description: "probe-desc",
		System:      "probe-system-V2",
		Model:       "probe-model",
		Speed:       "fast",
		Tools:       []string{"probe_toolset"},
		// ⚠️ The custom tool name **must deliberately not be a substring of a toolset name**. This used
		// to be `probe_tool` + `probe_toolset`, so `strings.Contains(raw,"probe_tool")` was satisfied by
		// `probe_toolset`: deleting CustomTools entirely still left this case green.
		// Found by mutation checking: an assertion meant to guard against "ama_wakeup silently wiped
		// out" was completely useless because the values were prefixes of each other.
		CustomTools: []ma.CustomToolSpec{{Name: "probe_wakeup_ct", Description: "d"}},
	})
	if err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	if len(*bodies) != 1 {
		t.Fatalf("应当发一次更新，实际 %d 次", len(*bodies))
	}
	body := (*bodies)[0]

	// The version must come from Get (optimistic locking), not a hard-coded 0/1.
	if *gets != 1 {
		t.Errorf("更新前应当 Get 一次拿版本号，实际 %d 次", *gets)
	}
	if got := body["version"]; got != float64(7) {
		t.Errorf("version = %v，期望 7（来自 Get 的返回值）——写死的版本号会被方舟拒", got)
	}

	if got := body["system"]; got != "probe-system-V2" {
		t.Errorf("system 没到 wire 上：%v。这正是「改了提示词方舟侧不变」的根因层", got)
	}
	if got := body["name"]; got != "probe-name" {
		t.Errorf("name = %v", got)
	}
	if got := body["description"]; got != "probe-desc" {
		t.Errorf("description = %v", got)
	}

	// model is an **object** ({id,speed}) replaced as a whole: filling in only id would quietly put
	// the fast tier back to the default.
	model, ok := body["model"].(map[string]any)
	if !ok {
		t.Fatalf("model 应当是个对象，实际 %T %v", body["model"], body["model"])
	}
	if model["id"] != "probe-model" {
		t.Errorf("model.id = %v", model["id"])
	}
	if model["speed"] != "fast" {
		t.Errorf("model.speed = %v，期望 fast——漏了它的表现是「调成 fast 之后档位"+
			"悄悄退回默认」，零报错", model["speed"])
	}

	// tools is replaced as a whole, and built-in toolsets and custom tools **share the same array**.
	// Sending only the former deletes ama_wakeup, and Ark still returns 200.
	tools, ok := body["tools"].([]any)
	if !ok {
		t.Fatalf("tools 应当是数组，实际 %T %v", body["tools"], body["tools"])
	}
	raw, err := json.Marshal(tools)
	if err != nil {
		t.Fatalf("重新编码 tools 失败: %v", err)
	}
	if !strings.Contains(string(raw), "probe_toolset") {
		t.Errorf("内置工具集没到 wire 上: %s", raw)
	}
	if !strings.Contains(string(raw), "probe_wakeup_ct") {
		t.Errorf("自定义工具没到 wire 上——tools 是整体替换，这等于把 ama_wakeup "+
			"从模型的工具列表里删掉，而方舟照样 200: %s", raw)
	}

	// Skills are handled by UpdateAgentSkills; this path must not touch them.
	// "Not sent" keeps the existing value; an empty array **clears** it. Sending the wrong one clears
	// all skills with zero errors.
	if v, present := body["skills"]; present && v != nil {
		if arr, ok := v.([]any); !ok || len(arr) > 0 {
			t.Errorf("UpdateAgentSpec 不该下发 skills（那是 UpdateAgentSkills 的职责，"+
				"而空数组在方舟那边是**清空**），实际 %v", v)
		}
	}
}

// TestLiveUpdateAgentRetriesOnVersionConflict locks in "409 must be retried, not treated as success".
//
// ⚠️ This used to return nil for a 409, treating it as success, on the grounds that "a conflict
// means another instance just updated it, and what it wrote is the same as what we want to write".
// That held **when there was only one kind of update** (skills), and stopped holding once spec was
// added:
//
//	A pushes spec (V1->V2), Gets version=3
//	B pushes skills at the same time, writes first, version becomes 4
//	A hits 409 -> treated as success -> writes back fingerprint -> Ark side still V1 while binding claims V2 = false record
//
// So the next chat hits the fingerprint and returns right away, and the Update that should have
// corrected it is never called. Production runs many instances, so two people starting chats at
// the same time will hit it.
//
// Mutation check: change `if ma.IsConflict(err) && attempt == 0 { continue }` in updateAgent
// back to `if ma.IsConflict(err) { return nil }` -> this case turns red.
func TestLiveUpdateAgentRetriesOnVersionConflict(t *testing.T) {
	runner, bodies, gets := newFakeArkAgentUpdate(t, 1) // first 409, second succeeds

	err := runner.UpdateAgentSpec(context.Background(), testArkKey(), "agent-fake-wire", AgentSpec{
		Name: "n", Model: "m", System: "probe-system-V2",
	})
	if err != nil {
		t.Fatalf("撞一次 409 之后应当重试成功，实际返回错误: %v", err)
	}
	if len(*bodies) != 2 {
		t.Fatalf("应当重试一次（共 2 次 Update），实际 %d 次——"+
			"1 次意味着把 409 当成了成功，那会写下一个假指纹并把修复通道堵死", len(*bodies))
	}
	// The retry must **Get again** for the new version, or it hits 409 again with the stale version.
	if *gets != 2 {
		t.Errorf("重试前应当重新 Get 拿新版本号，实际 Get %d 次", *gets)
	}
	first, second := (*bodies)[0]["version"], (*bodies)[1]["version"]
	if first == second {
		t.Errorf("重试时用的还是老版本号（%v）——那会再撞一次 409", first)
	}
	// And the retry must send the same content.
	if got := (*bodies)[1]["system"]; got != "probe-system-V2" {
		t.Errorf("重试时发的 system = %v，期望 probe-system-V2", got)
	}
}

// TestLiveUpdateAgentFailsAfterRepeatedConflicts locks in "repeated conflicts must return an error".
//
// Returning nil is the real trap: the caller would then write back the fingerprint, so "not
// written" is recorded as "written", and the next chat hits the fingerprint without even retrying.
// Returning an error means the fingerprint is not written back, and the self-healing path stays open.
func TestLiveUpdateAgentFailsAfterRepeatedConflicts(t *testing.T) {
	runner, bodies, _ := newFakeArkAgentUpdate(t, 99) // 409 every time

	err := runner.UpdateAgentSpec(context.Background(), testArkKey(), "agent-fake-wire", AgentSpec{
		Name: "n", Model: "m", System: "s",
	})
	if err == nil {
		t.Fatal("连续撞 409 必须返回错误——返回 nil 会让调用方回写指纹，" +
			"把「没写成」记成「写成了」，那个改动就永久不生效且零报错")
	}
	// Retries have a limit and cannot go on forever (a user is waiting on this path).
	if len(*bodies) != 2 {
		t.Errorf("重试上限是一次（共 2 次 Update），实际 %d 次", len(*bodies))
	}
}

// TestLiveUpdateAgentSkillsStillOmitsSpecFields locks in that the two updates **do not interfere**.
//
// Both now share updateAgent, so a refactor could easily make the skills path also send system.
// `ma.AgentUpdate` means "empty = no change", so sending an empty system would actually **not**
// clear it (optAgentString returns unset for an empty string). The real risk is the reverse:
// if someone fills it with a system computed elsewhere, it would overwrite the value the spec path
// just wrote correctly.
//
// This case locks in "a skills update carries only skills".
func TestLiveUpdateAgentSkillsStillOmitsSpecFields(t *testing.T) {
	runner, bodies, _ := newFakeArkAgentUpdate(t, 0)

	if err := runner.UpdateAgentSkills(context.Background(), testArkKey(), "agent-fake-wire",
		[]persistmodel.ArkSkillRef{{ArkSkillID: "skill-ark-1", Version: "1"}}); err != nil {
		t.Fatalf("更新技能失败: %v", err)
	}
	if len(*bodies) != 1 {
		t.Fatalf("应当发一次更新，实际 %d 次", len(*bodies))
	}
	body := (*bodies)[0]

	skills, ok := body["skills"].([]any)
	if !ok || len(skills) != 1 {
		t.Fatalf("skills 应当有 1 条，实际 %v", body["skills"])
	}
	// Must not carry system / model / tools: those are UpdateAgentSpec's job.
	for _, k := range []string{"system", "model", "tools", "name", "description"} {
		if v, present := body[k]; present && v != nil {
			t.Errorf("更新技能时不该下发 %s（那是 UpdateAgentSpec 的职责），实际 %v", k, v)
		}
	}
}

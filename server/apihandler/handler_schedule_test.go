// HTTP boundary tests for schedules: status codes, error envelopes, route shapes.
//
// Split of work with the Service layer tests: those cover business invariants; this file only
// covers "is the protocol conversion right": which status code a sentinel error maps to, whether
// create returns 201 or 200, whether delete has a body. Mistakes in this layer do not fail
// compilation; they just give the frontend a response it does not understand.
//
// # ⚠️ This file imports core, but **the apihandler package under test does not import core**
//
// `go list -deps ./apihandler | grep -c 'ama/server/core$'` is 0. That rule is guaranteed by the
// production code and is not affected by this file: Go's external test dependencies do not enter
// the dependency graph of the package under test.
//
// Why a real core.Service instead of a stand-in: these cases assert "which status code a sentinel
// error from Service is turned into". A stand-in can only return **errors I wrote into it**, so the
// test would become "my stand-in returns what I wrote". Only real seed data + a real Service ensure
// the sentinels are the ones the business logic really produces.
//
// Wiring goes through orchestrator: the handler takes the orchestration layer's narrow interfaces,
// so this file wires it along the real path too.

package apihandler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/core"
	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/orchestrator"
	"github.com/chyroc/ama/server/repository/dao/coremem"
)

// newScheduleMux builds a mux with routes installed, plus the matching Service.
func newScheduleMux(t *testing.T) (*http.ServeMux, *core.Service) {
	t.Helper()
	svc := newService(t)
	h, err := NewHandler(HandlerConfig{Orchestrator: newTestOrchestrator(t, svc)})
	if err != nil {
		t.Fatalf("装配 handler 失败：%v", err)
	}
	mux := http.NewServeMux()
	h.Register(mux)
	return mux, svc
}

// seedWS is the id of the workspace in the seed data. It matches the constant of the same name in
// core/service_test.go (both follow the coremem seed, which is core/seed.json).
const seedWS = "ws_demo"

// newService builds a real core.Service with seed data, for this package's HTTP boundary cases.
func newService(t *testing.T) *core.Service {
	t.Helper()
	store, err := coremem.NewSeededMemStore()
	if err != nil {
		t.Fatalf("装载种子失败：%v", err)
	}
	svc, err := core.New(core.Config{Store: store})
	if err != nil {
		t.Fatalf("装配 Service 失败：%v", err)
	}
	return svc
}

// newTestOrchestrator wires a core.Service into the orchestration layer's narrow interfaces along
// the real path.
//
// It has the same shape as the wiring in cmd/amad/orchestrator.go: the eight domain packages are
// taken from coreSvc, and anything out of reach goes to Fallback. **It deliberately takes no
// shortcut** (such as passing coreSvc directly as the interface): then the tested path would not
// be the one used in production, and the 10 forwards in the orchestration composite are exactly
// where mistakes happen.
func newTestOrchestrator(t *testing.T, svc *core.Service) *orchestrator.Orchestrator {
	t.Helper()
	orch, err := orchestrator.New(orchestrator.Config{
		Issue:      svc.Issues(),
		Suggestion: svc.Suggestions(),
		Revision:   svc.Revisions(),
		ArkCred:    svc.ArkCreds(),
		Workspace:  svc.Workspaces(),
		Agent:      svc.Agents(),
		Schedule:   svc.Schedules(),
		Run:        svc.Runs(),
		Skill:      svc.Skills(),
		Fallback:   svc,
	})
	if err != nil {
		t.Fatalf("装配编排层失败：%v", err)
	}
	return orch
}

// doJSON sends a JSON request and returns the response recorder.
func doJSON(t *testing.T, mux *http.ServeMux, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body != nil {
		blob, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("编码请求失败：%v", err)
		}
		reader = strings.NewReader(string(blob))
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// scheduleBody is the minimal valid body for creating a schedule.
func scheduleBody() apimodel.ScheduleDraft {
	return apimodel.ScheduleDraft{
		AgentID: "agent_triage",
		Name:    "每日巡检",
		Spec: apimodel.ScheduleSpec{
			Kind: apimodel.ScheduleCron,
			Cron: "30 9 * * 1-5",
			TZ:   apimodel.DefaultScheduleTZ,
		},
		Prompt:   "汇总昨天的告警",
		Delivery: apimodel.DeliveryTarget{Kind: apimodel.DeliveryRunOnly},
	}
}

// TestScheduleRoutesCRUD goes through create/read/update/delete once, asserting each status code.
func TestScheduleRoutesCRUD(t *testing.T) {
	mux, _ := newScheduleMux(t)

	// Create: 201 + the whole entity.
	rec := doJSON(t, mux, http.MethodPost, "/api/workspaces/"+seedWS+"/schedules", scheduleBody())
	if rec.Code != http.StatusCreated {
		t.Fatalf("建任务应返回 201，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var created apimodel.Schedule
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("解码失败：%v", err)
	}
	if created.ID == "" || created.NextRunAt == "" {
		t.Fatalf("回执要带 ID 与 nextRunAt：%+v", created)
	}

	// List: 200 + an array.
	rec = doJSON(t, mux, http.MethodGet, "/api/workspaces/"+seedWS+"/schedules", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("列任务应返回 200，实际 %d", rec.Code)
	}
	var list []apimodel.Schedule
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("列表应是数组：%v（body=%s）", err, rec.Body.String())
	}
	if len(list) != 1 {
		t.Errorf("应有 1 条，实际 %d", len(list))
	}

	// Get one: 200.
	rec = doJSON(t, mux, http.MethodGet, "/api/schedules/"+created.ID, nil)
	if rec.Code != http.StatusOK {
		t.Errorf("取单个应返回 200，实际 %d", rec.Code)
	}

	// Update: 200.
	draft := scheduleBody()
	draft.Name = "改过的名字"
	rec = doJSON(t, mux, http.MethodPatch, "/api/schedules/"+created.ID, draft)
	if rec.Code != http.StatusOK {
		t.Fatalf("改任务应返回 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var updated apimodel.Schedule
	_ = json.Unmarshal(rec.Body.Bytes(), &updated)
	if updated.Name != "改过的名字" {
		t.Errorf("改名没生效：%+v", updated)
	}

	// Delete: 204 and **no body**.
	rec = doJSON(t, mux, http.MethodDelete, "/api/schedules/"+created.ID, nil)
	if rec.Code != http.StatusNoContent {
		t.Errorf("删除应返回 204，实际 %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("204 不该带 body（前端无条件 res.json() 会抛异常），实际 %q", rec.Body.String())
	}
}

// TestScheduleStatusRoute: enabling/pausing goes through a separate PATCH .../status.
func TestScheduleStatusRoute(t *testing.T) {
	mux, _ := newScheduleMux(t)

	rec := doJSON(t, mux, http.MethodPost, "/api/workspaces/"+seedWS+"/schedules", scheduleBody())
	var created apimodel.Schedule
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	// Pause.
	rec = doJSON(t, mux, http.MethodPatch, "/api/schedules/"+created.ID+"/status",
		map[string]string{"status": "paused"})
	if rec.Code != http.StatusOK {
		t.Fatalf("暂停应返回 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var paused apimodel.Schedule
	_ = json.Unmarshal(rec.Body.Bytes(), &paused)
	if paused.Status != apimodel.SchedulePaused {
		t.Errorf("状态应是 paused，实际 %s", paused.Status)
	}
	if paused.NextRunAt != "" {
		t.Errorf("暂停后不该有 nextRunAt，实际 %q", paused.NextRunAt)
	}

	// Enable again; nextRunAt must come back.
	rec = doJSON(t, mux, http.MethodPatch, "/api/schedules/"+created.ID+"/status",
		map[string]string{"status": "active"})
	if rec.Code != http.StatusOK {
		t.Fatalf("启用应返回 200，实际 %d", rec.Code)
	}
	var active apimodel.Schedule
	_ = json.Unmarshal(rec.Body.Bytes(), &active)
	if active.NextRunAt == "" {
		t.Error("启用后要重算 nextRunAt")
	}

	// Invalid status -> 400 + invalid_argument.
	rec = doJSON(t, mux, http.MethodPatch, "/api/schedules/"+created.ID+"/status",
		map[string]string{"status": "deleted"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("非法状态应返回 400，实际 %d", rec.Code)
	}
	assertErrorCode(t, rec, "invalid_argument")
}

// TestScheduleRunNowRoute: run-now pushes nextRunAt to now and returns 200 + the whole entity.
func TestScheduleRunNowRoute(t *testing.T) {
	mux, _ := newScheduleMux(t)

	rec := doJSON(t, mux, http.MethodPost, "/api/workspaces/"+seedWS+"/schedules", scheduleBody())
	var created apimodel.Schedule
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	rec = doJSON(t, mux, http.MethodPost, "/api/schedules/"+created.ID+"/run-now", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("试跑应返回 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var fired apimodel.Schedule
	if err := json.Unmarshal(rec.Body.Bytes(), &fired); err != nil {
		t.Fatalf("试跑要返回整个实体（前端据此更新那一行）：%v", err)
	}
	// The next fire is pushed to "now", so it must be earlier than the original one (weekdays 9:30
	// is in the future).
	if fired.NextRunAt >= created.NextRunAt {
		t.Errorf("试跑应把 nextRunAt 推到现在，原来 %s 现在 %s",
			created.NextRunAt, fired.NextRunAt)
	}
}

// TestScheduleRunNowOnDeletedAgent: run-now must return 422, not 404, when the agent was deleted.
//
// The schedule itself exists; it is "this action is not allowed right now". With 404, the
// NOT_FOUND_CODES in the frontend http.ts would turn it into null, and the page would show a
// silent empty state.
func TestScheduleRunNowOnDeletedAgent(t *testing.T) {
	mux, svc := newScheduleMux(t)

	rec := doJSON(t, mux, http.MethodPost, "/api/workspaces/"+seedWS+"/schedules", scheduleBody())
	var created apimodel.Schedule
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	if err := svc.DeleteAgent(t.Context(), "agent_triage"); err != nil {
		t.Fatalf("删 agent 失败：%v", err)
	}

	rec = doJSON(t, mux, http.MethodPost, "/api/schedules/"+created.ID+"/run-now", nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("agent 已删时试跑应返回 422，实际 %d：%s", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, "precondition_failed")
}

// TestScheduleRoutesErrorMapping: each kind of error maps to its status code and code.
func TestScheduleRoutesErrorMapping(t *testing.T) {
	mux, _ := newScheduleMux(t)

	t.Run("取不存在的返 404 且 code 是 not_found", func(t *testing.T) {
		rec := doJSON(t, mux, http.MethodGet, "/api/schedules/sch_nope", nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("应返回 404，实际 %d", rec.Code)
		}
		// **The frontend decides "does not exist" by code, not status code** (the standard library
		// also returns 404 when a route is mounted wrong).
		assertErrorCode(t, rec, "not_found")
	})

	t.Run("cron 写错返 400", func(t *testing.T) {
		draft := scheduleBody()
		draft.Spec.Cron = "99 9 * * *"
		rec := doJSON(t, mux, http.MethodPost, "/api/workspaces/"+seedWS+"/schedules", draft)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("cron 非法应返回 400，实际 %d", rec.Code)
		}
		assertErrorCode(t, rec, "invalid_argument")
		// The error message must be shown to users as-is, and include an example of the correct syntax.
		var env struct{ Message string }
		_ = json.Unmarshal(rec.Body.Bytes(), &env)
		if !strings.Contains(env.Message, "cron") {
			t.Errorf("错误文案要说清是 cron 的问题，实际 %q", env.Message)
		}
	})

	t.Run("空间不存在返 404", func(t *testing.T) {
		rec := doJSON(t, mux, http.MethodPost, "/api/workspaces/ws_nope/schedules", scheduleBody())
		if rec.Code != http.StatusNotFound {
			t.Errorf("应返回 404，实际 %d", rec.Code)
		}
	})

	t.Run("body 不是合法 JSON 返 400", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost,
			"/api/workspaces/"+seedWS+"/schedules", strings.NewReader("{不是 json"))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("非法 JSON 应返回 400，实际 %d", rec.Code)
		}
	})
}

// TestScheduleListNeverNotFound: the list endpoint returns 200 + [] even for a workspace that does
// not exist.
//
// The frontend listSchedules returns an array, not a nullable value; a 404 would make the page
// throw instead of showing an empty list.
func TestScheduleListNeverNotFound(t *testing.T) {
	mux, _ := newScheduleMux(t)

	rec := doJSON(t, mux, http.MethodGet, "/api/workspaces/ws_nope/schedules", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("列表接口永不 404，实际 %d", rec.Code)
	}
	// And it must be [], not null: the frontend calls .map() on it directly.
	if body := strings.TrimSpace(rec.Body.String()); body != "[]" {
		t.Errorf("空列表要序列化成 []，实际 %q", body)
	}
}

// TestScheduleDeleteIsIdempotent: deleting something that does not exist also returns 204.
func TestScheduleDeleteIsIdempotent(t *testing.T) {
	mux, _ := newScheduleMux(t)
	rec := doJSON(t, mux, http.MethodDelete, "/api/schedules/sch_nope", nil)
	if rec.Code != http.StatusNoContent {
		t.Errorf("删不存在的应当幂等（204），实际 %d", rec.Code)
	}
}

// assertErrorCode asserts the code in the error envelope.
func assertErrorCode(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	var env struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("错误响应应是 {message, code} 信封：%v（body=%s）", err, rec.Body.String())
	}
	if env.Code != want {
		t.Errorf("code 应是 %q，实际 %q", want, env.Code)
	}
	if strings.TrimSpace(env.Message) == "" {
		t.Error("message 不能为空——前端会把它直接显示给用户")
	}
}

package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chyroc/ama/server/repository/dao/coremem"
)

func configureTestOpenViking(svc *Service, url string) {
	svc.openViking = OpenVikingServerConfig{
		Enabled:        true,
		URL:            url,
		APIKey:         "ov-test-secret-key",
		ResourceID:     "ov-test-resource",
		RecallTimeout:  3500 * time.Millisecond,
		CaptureTimeout: 15 * time.Second,
	}
}

func TestOpenVikingAutoRecallAndTitlePreservation(t *testing.T) {
	var (
		mu          sync.Mutex
		gotAPIKey   string
		gotResID    string
		gotUser     string
		gotAgent    string
		gotQuery    string
		gotMode     string
		searchCalls int
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/search/search" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		searchCalls++
		gotAPIKey = r.Header.Get("X-API-Key")
		gotResID = r.Header.Get("X-OpenViking-Resource-Id")
		gotUser = r.Header.Get("X-OpenViking-User")
		gotAgent = r.Header.Get("X-OpenViking-Agent")
		var body struct {
			Query string `json:"query"`
			Mode  string `json:"mode"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotQuery = body.Query
		gotMode = body.Mode
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"status": "ok",
			"result": {
				"rendered": "<memory uri=\"viking://user/default/memories/preferences/roi.md\" type=\"preferences\" score=\"0.68\" detail=\"full\">\n周报默认采用 30 天归因 ROI 口径。\n</memory>"
			}
		}`))
	}))
	defer srv.Close()

	ctx := context.Background()
	svc, store, runner := newChatService(t)
	configureTestOpenViking(svc, srv.URL)
	bindSkills(t, store, seedChatAgent, BuiltinSkillIDOpenVikingMemory)
	// Long-term memory is opt-in per agent (default off): enable it here to verify recall injection.
	enableOpenViking(t, store)

	run, err := svc.CreateConversation(ctx, seedWS, seedChatAgent)
	if err != nil {
		t.Fatalf("CreateConversation failed: %v", err)
	}

	userFrame := json.RawMessage(`{"type":"user.message","content":[{"type":"text","text":"本周 ROI 是多少？"}]}`)
	accepted, err := svc.AppendRunEvents(ctx, run.ID, []json.RawMessage{userFrame})
	if err != nil {
		t.Fatalf("AppendRunEvents failed: %v", err)
	}
	if len(accepted) != 1 {
		t.Fatalf("expected 1 accepted event, got %d", len(accepted))
	}

	mu.Lock()
	if searchCalls != 1 {
		t.Errorf("expected 1 search call, got %d", searchCalls)
	}
	if gotAPIKey != "ov-test-secret-key" {
		t.Errorf("expected X-API-Key ov-test-secret-key, got %q", gotAPIKey)
	}
	if gotResID != "ov-test-resource" {
		t.Errorf("expected X-OpenViking-Resource-Id ov-test-resource, got %q", gotResID)
	}
	if gotUser != seedWS {
		t.Errorf("expected X-OpenViking-User scoped to workspace %q, got %q", seedWS, gotUser)
	}
	if gotAgent != seedChatAgent {
		t.Errorf("expected X-OpenViking-Agent scoped to agent %q, got %q", seedChatAgent, gotAgent)
	}
	if gotQuery != "本周 ROI 是多少？" {
		t.Errorf("expected query %q, got %q", "本周 ROI 是多少？", gotQuery)
	}
	if gotMode != "context" {
		t.Errorf("expected mode=context, got %q", gotMode)
	}
	mu.Unlock()

	if len(runner.sentEvents) != 1 {
		t.Fatalf("expected 1 sent event on runner, got %d", len(runner.sentEvents))
	}
	var sentEv struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(runner.sentEvents[0], &sentEv); err != nil {
		t.Fatalf("unmarshal sent event: %v", err)
	}
	if len(sentEv.Content) != 2 {
		t.Fatalf("expected 2 content blocks (user text + <harness:openviking>), got %d: %+v", len(sentEv.Content), sentEv.Content)
	}
	if !strings.HasPrefix(sentEv.Content[1].Text, openVikingPromptOpenTag) ||
		!strings.HasSuffix(sentEv.Content[1].Text, openVikingPromptCloseTag) ||
		!strings.Contains(sentEv.Content[1].Text, "30 天归因 ROI 口径") {
		t.Errorf("unexpected <harness:openviking> block: %q", sentEv.Content[1].Text)
	}

	updatedRun, err := store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun failed: %v", err)
	}
	if updatedRun.Title != "本周 ROI 是多少？" {
		t.Errorf("run.Title must not be polluted by <harness:openviking>; got %q", updatedRun.Title)
	}
}

func TestOpenVikingAutoCaptureAndCommitOnTurnIdle(t *testing.T) {
	var (
		mu             sync.Mutex
		createdSID     string
		capturedMsgs   []openVikingChatMessage
		committedSID   string
		committedReady = make(chan struct{}, 1)
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/sessions":
			var req struct {
				SessionID string `json:"session_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			createdSID = req.SessionID
			_, _ = w.Write([]byte(`{"status":"ok","result":{"session_id":"` + req.SessionID + `"}}`))

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/messages/batch"):
			var req struct {
				Messages []openVikingChatMessage `json:"messages"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			capturedMsgs = append(capturedMsgs, req.Messages...)
			_, _ = w.Write([]byte(`{"status":"ok","result":{"message_count":2}}`))

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/commit"):
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/sessions/"), "/")
			if len(parts) > 0 {
				committedSID = parts[0]
			}
			_, _ = w.Write([]byte(`{"status":"ok","result":{"status":"accepted"}}`))
			select {
			case committedReady <- struct{}{}:
			default:
			}

		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	svc, store, _ := newChatService(t)
	configureTestOpenViking(svc, srv.URL)
	bindSkills(t, store, seedChatAgent, BuiltinSkillIDOpenVikingMemory)
	enableOpenViking(t, store)

	run, err := svc.CreateConversation(ctx, seedWS, seedChatAgent)
	if err != nil {
		t.Fatalf("CreateConversation failed: %v", err)
	}

	svc.persistArkEvent(ctx, run.ID, []byte(`{"id":"sevt-u1","type":"user.message","content":[{"type":"text","text":"请记住：周报固定用 30 天归因口径"},{"type":"text","text":"<harness:openviking>\n旧召回内容\n</harness:openviking>"}]}`))
	svc.persistArkEvent(ctx, run.ID, []byte(`{"id":"sevt-a1","type":"agent.message","content":[{"type":"text","text":"好的，已记录周报使用 30 天归因口径。"}]}`))
	svc.persistArkEvent(ctx, run.ID, []byte(`{"id":"sevt-idle1","type":"session.status_idle","stop_reason":{"type":"end_turn"}}`))

	select {
	case <-committedReady:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for OpenViking turn commit")
	}

	mu.Lock()
	defer mu.Unlock()
	if createdSID != run.ID {
		t.Errorf("expected session_id %q, got %q", run.ID, createdSID)
	}
	if committedSID != run.ID {
		t.Errorf("expected committed session_id %q, got %q", run.ID, committedSID)
	}
	if len(capturedMsgs) != 2 {
		t.Fatalf("expected 2 captured messages (user + assistant), got %d: %+v", len(capturedMsgs), capturedMsgs)
	}
	if capturedMsgs[0].Role != "user" || capturedMsgs[0].Content != "请记住：周报固定用 30 天归因口径" {
		t.Errorf("expected clean user message without <harness:openviking>, got %+v", capturedMsgs[0])
	}
	if capturedMsgs[1].Role != "assistant" || capturedMsgs[1].Content != "好的，已记录周报使用 30 天归因口径。" {
		t.Errorf("unexpected assistant message: %+v", capturedMsgs[1])
	}
}

func TestOpenVikingSessionEnvsInjection(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newChatService(t)
	svc.openViking = OpenVikingServerConfig{
		Enabled:    true,
		URL:        "https://api.vikingdb.cn-beijing.volces.com/openviking",
		APIKey:     "ov-secret-123",
		ResourceID: "ov-test-resource",
	}
	bindSkills(t, store, seedChatAgent, BuiltinSkillIDOpenVikingMemory)
	enableOpenViking(t, store)

	run, err := svc.CreateConversation(ctx, seedWS, seedChatAgent)
	if err != nil {
		t.Fatalf("CreateConversation failed: %v", err)
	}

	envSrc := NewSessionEnvSource(svc, nil, nil, "http://localhost:9999", nil)
	envs, secrets, _, _ := envSrc.SessionEnvs(ctx, run.ID)
	if envs["OPENVIKING_URL"] != "https://api.vikingdb.cn-beijing.volces.com/openviking" {
		t.Errorf("unexpected OPENVIKING_URL: %q", envs["OPENVIKING_URL"])
	}
	if envs["OPENVIKING_API_KEY"] != "ov-secret-123" {
		t.Errorf("unexpected OPENVIKING_API_KEY: %q", envs["OPENVIKING_API_KEY"])
	}
	if envs["OPENVIKING_RESOURCE_ID"] != "ov-test-resource" {
		t.Errorf("unexpected OPENVIKING_RESOURCE_ID: %q", envs["OPENVIKING_RESOURCE_ID"])
	}
	if envs["OPENVIKING_USER"] != seedWS {
		t.Errorf("unexpected OPENVIKING_USER: %q", envs["OPENVIKING_USER"])
	}
	if envs["OPENVIKING_AGENT_ID"] != seedChatAgent {
		t.Errorf("unexpected OPENVIKING_AGENT_ID: %q", envs["OPENVIKING_AGENT_ID"])
	}
	if envs["OPENVIKING_SESSION_ID"] != run.ID {
		t.Errorf("unexpected OPENVIKING_SESSION_ID: %q", envs["OPENVIKING_SESSION_ID"])
	}
	if !contains(secrets, "OPENVIKING_API_KEY") {
		t.Errorf("OPENVIKING_API_KEY must be included in secretKeys for output scrubbing, got %v", secrets)
	}
}

// enableOpenViking flips the per-agent opt-in on the seeded chat agent.
func enableOpenViking(t *testing.T, store *coremem.MemStore) {
	t.Helper()
	ctx := context.Background()
	a, err := store.GetAgent(ctx, seedChatAgent)
	if err != nil {
		t.Fatalf("读取种子 agent 失败: %v", err)
	}
	a.OpenVikingEnabled = true
	if _, err := store.SaveAgent(ctx, a); err != nil {
		t.Fatalf("保存种子 agent 失败: %v", err)
	}
}

// TestOpenVikingSkippedWhenAgentNotOptedIn ensures an agent that did not select long-term memory
// does not receive recall injection or capture turns.
func TestOpenVikingSkippedWhenAgentNotOptedIn(t *testing.T) {
	var (
		mu          sync.Mutex
		searchCalls int
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		searchCalls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","result":{"rendered":"<memory>should not be injected</memory>"}}`))
	}))
	defer srv.Close()

	ctx := context.Background()
	svc, store, runner := newChatService(t)
	configureTestOpenViking(svc, srv.URL)
	bindSkills(t, store, seedChatAgent, BuiltinSkillIDOpenVikingMemory)
	// No enableOpenViking here on purpose.

	run, err := svc.CreateConversation(ctx, seedWS, seedChatAgent)
	if err != nil {
		t.Fatalf("CreateConversation failed: %v", err)
	}
	userFrame := json.RawMessage(`{"type":"user.message","content":[{"type":"text","text":"本周 ROI 是多少？"}]}`)
	if _, err := svc.AppendRunEvents(ctx, run.ID, []json.RawMessage{userFrame}); err != nil {
		t.Fatalf("AppendRunEvents failed: %v", err)
	}

	mu.Lock()
	got := searchCalls
	mu.Unlock()
	if got != 0 {
		t.Errorf("未开启记忆的 agent 不应触发 OpenViking 召回，实际调用 %d 次", got)
	}
	if len(runner.sentEvents) != 1 {
		t.Fatalf("expected 1 sent event, got %d", len(runner.sentEvents))
	}
	var sentEv struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(runner.sentEvents[0], &sentEv); err != nil {
		t.Fatalf("unmarshal sent event: %v", err)
	}
	if len(sentEv.Content) != 1 || sentEv.Content[0].Text != "本周 ROI 是多少？" {
		t.Errorf("expected untouched user message, got %+v", sentEv.Content)
	}
}

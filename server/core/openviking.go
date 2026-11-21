// OpenViking long-term memory lifecycle hooks:
//
//  1. Auto-Recall (before sending user.message to Ark in AppendRunEvents):
//     Queries OpenViking's context search (`POST /api/v1/search/search` with mode="context")
//     and appends a `<harness:openviking>…</harness:openviking>` text content block to the
//     outgoing `user.message`.
//     Because it uses the `<harness:*>` prefix, `firstUserText` skips it when computing the
//     session title, and the frontend `ConversationView` collapses it into the InjectedContext
//     panel instead of polluting the user's message bubble.
//
//  2. Auto-Capture & Commit (after a turn finishes with terminal `session.status_idle` in applyEventToRun):
//     Extracts the newly completed turn's `user.message` (stripping `<harness:*>` blocks) and
//     `agent.message` text, appends them to the OpenViking session (`POST /api/v1/sessions/{sid}/messages/batch`),
//     and triggers background memory extraction (`POST /api/v1/sessions/{sid}/commit?wait=false`).
//
// All network calls are bounded in time and fail-open so chat availability is never impacted if
// OpenViking is unreachable or slow.
package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/shared/gopkg/arkframe"
)

const (
	openVikingPromptOpenTag  = "<harness:openviking>"
	openVikingPromptCloseTag = "</harness:openviking>"

	defaultOpenVikingURL            = "https://api.vikingdb.cn-beijing.volces.com/openviking"
	defaultOpenVikingRecallTimeout  = 3500 * time.Millisecond
	defaultOpenVikingCaptureTimeout = 15 * time.Second
)

type openVikingConfig struct {
	Enabled        bool
	URL            string
	APIKey         string
	ResourceID     string
	User           string
	AgentID        string
	RecallTimeout  time.Duration
	CaptureTimeout time.Duration
}

func (s *Service) resolveOpenVikingConfig(workspaceID string, agent apimodel.Agent) openVikingConfig {
	return resolveOpenVikingConfig(s.openViking, workspaceID, agent)
}

func resolveOpenVikingConfig(serverCfg OpenVikingServerConfig, workspaceID string, agent apimodel.Agent) openVikingConfig {

	agentEnv := make(map[string]string, len(agent.Envs))
	for _, e := range agent.Envs {
		k := strings.TrimSpace(e.Key)
		if k != "" {
			agentEnv[k] = strings.TrimSpace(e.Value)
		}
	}

	// Memory is opt-in per agent (`agent.OpenVikingEnabled` or explicit `OPENVIKING_ENABLED` env on
	// the agent), gated globally by server config (`openViking.enabled` unless the agent supplies
	// its own standalone `OPENVIKING_API_KEY` + `OPENVIKING_RESOURCE_ID`).
	enabled := agent.OpenVikingEnabled
	if raw := strings.ToLower(strings.TrimSpace(agentEnv["OPENVIKING_ENABLED"])); raw != "" {
		enabled = !(raw == "false" || raw == "0" || raw == "off" || raw == "no")
	}
	if !enabled {
		return openVikingConfig{Enabled: false}
	}

	agentKey := strings.TrimSpace(agentEnv["OPENVIKING_API_KEY"])
	agentRes := strings.TrimSpace(agentEnv["OPENVIKING_RESOURCE_ID"])
	if !serverCfg.Enabled && (agentKey == "" || agentRes == "") {
		return openVikingConfig{Enabled: false}
	}

	apiKey := agentKey
	if apiKey == "" && serverCfg.Enabled {
		apiKey = serverCfg.APIKey
	}
	if strings.TrimSpace(apiKey) == "" {
		return openVikingConfig{Enabled: false}
	}

	resourceID := agentRes
	if resourceID == "" && serverCfg.Enabled {
		resourceID = strings.TrimSpace(serverCfg.ResourceID)
	}
	if resourceID == "" {
		return openVikingConfig{Enabled: false}
	}

	baseURL := strings.TrimRight(strings.TrimSpace(agentEnv["OPENVIKING_URL"]), "/")
	if baseURL == "" {
		baseURL = strings.TrimRight(strings.TrimSpace(serverCfg.URL), "/")
	}
	if baseURL == "" {
		baseURL = defaultOpenVikingURL
	}

	// Scope memories by workspace and agent by default so different workspaces and agents do not
	// read or overwrite each other's long-term memories.
	user := strings.TrimSpace(agentEnv["OPENVIKING_USER"])
	if user == "" {
		user = strings.TrimSpace(agent.WorkspaceID)
	}
	if user == "" {
		user = strings.TrimSpace(workspaceID)
	}
	if user == "" {
		user = "default"
	}

	ovAgentID := strings.TrimSpace(agentEnv["OPENVIKING_AGENT_ID"])
	if ovAgentID == "" {
		ovAgentID = strings.TrimSpace(agent.ID)
	}
	if ovAgentID == "" {
		ovAgentID = "default"
	}

	recallTimeout := serverCfg.RecallTimeout
	if recallTimeout <= 0 {
		recallTimeout = defaultOpenVikingRecallTimeout
	}
	captureTimeout := serverCfg.CaptureTimeout
	if captureTimeout <= 0 {
		captureTimeout = defaultOpenVikingCaptureTimeout
	}

	return openVikingConfig{
		Enabled:        true,
		URL:            baseURL,
		APIKey:         apiKey,
		ResourceID:     resourceID,
		User:           user,
		AgentID:        ovAgentID,
		RecallTimeout:  recallTimeout,
		CaptureTimeout: captureTimeout,
	}
}

func (cfg openVikingConfig) doJSON(ctx context.Context, method, path string, reqBody any, out any) (int, error) {
	var bodyReader io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return 0, err
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, cfg.URL+path, bodyReader)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-API-Key", cfg.APIKey)
	if cfg.ResourceID != "" {
		req.Header.Set("X-OpenViking-Resource-Id", cfg.ResourceID)
	}
	if cfg.User != "" {
		req.Header.Set("X-OpenViking-User", cfg.User)
	}
	if cfg.AgentID != "" {
		req.Header.Set("X-OpenViking-Agent", cfg.AgentID)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode >= 400 {
		return resp.StatusCode, fmt.Errorf("openviking http %d: %s", resp.StatusCode, strings.TrimSpace(string(respBytes)))
	}
	if out != nil && len(respBytes) > 0 {
		if err := json.Unmarshal(respBytes, out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

// injectOpenVikingRecall queries OpenViking for relevant long-term memories before sending the
// user's message to Ark, and appends a `<harness:openviking>` content block when memories match.
func (s *Service) injectOpenVikingRecall(
	ctx context.Context, run apimodel.Run, agent apimodel.Agent, raw []json.RawMessage,
) []json.RawMessage {
	cfg := s.resolveOpenVikingConfig(run.WorkspaceID, agent)
	if !cfg.Enabled || len(raw) == 0 {
		return raw
	}

	targetIdx := -1
	var query string
	for i := len(raw) - 1; i >= 0; i-- {
		if arkframe.Type(raw[i]) != "user.message" {
			continue
		}
		q, alreadyHasBlock := extractPlainUserMessageForOpenViking(raw[i])
		if alreadyHasBlock || q == "" {
			return raw
		}
		targetIdx = i
		query = q
		break
	}
	if targetIdx < 0 || query == "" {
		return raw
	}

	recallCtx, cancel := context.WithTimeout(ctx, cfg.RecallTimeout)
	defer cancel()

	reqPayload := map[string]any{
		"query":      query,
		"mode":       "context",
		"max_tokens": 1500,
		"purpose":    "chat",
	}
	if strings.TrimSpace(run.ID) != "" {
		reqPayload["session_id"] = strings.TrimSpace(run.ID)
	}

	var searchResp struct {
		Status string `json:"status"`
		Result struct {
			Rendered string `json:"rendered"`
		} `json:"result"`
	}
	if _, err := cfg.doJSON(recallCtx, http.MethodPost, "/api/v1/search/search", reqPayload, &searchResp); err != nil {
		s.log().Debug("OpenViking 自动召回跳过", "run", run.ID, "error", err.Error())
		return raw
	}

	rendered := strings.TrimSpace(searchResp.Result.Rendered)
	if rendered == "" || strings.Contains(rendered, openVikingPromptCloseTag) {
		return raw
	}

	updatedItem, ok := appendOpenVikingHarnessBlock(raw[targetIdx], rendered)
	if !ok {
		return raw
	}

	out := make([]json.RawMessage, len(raw))
	copy(out, raw)
	out[targetIdx] = updatedItem
	return out
}

func extractPlainUserMessageForOpenViking(rawItem json.RawMessage) (string, bool) {
	var ev struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(rawItem, &ev); err != nil || ev.Type != "user.message" {
		return "", false
	}
	var parts []string
	for _, block := range ev.Content {
		if block.Type != "text" {
			continue
		}
		trimmed := strings.TrimSpace(block.Text)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, openVikingPromptOpenTag) {
			return "", true
		}
		if strings.HasPrefix(trimmed, HarnessTagPrefix) || strings.HasPrefix(trimmed, "<openviking-context>") {
			continue
		}
		parts = append(parts, trimmed)
	}
	return strings.TrimSpace(strings.Join(parts, "\n")), false
}

func appendOpenVikingHarnessBlock(rawItem json.RawMessage, rendered string) (json.RawMessage, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(rawItem, &obj); err != nil {
		return nil, false
	}
	var content []json.RawMessage
	if rawContent, ok := obj["content"]; ok && len(rawContent) > 0 {
		if err := json.Unmarshal(rawContent, &content); err != nil {
			return nil, false
		}
	}
	blockText := openVikingPromptOpenTag + "\n" +
		"以下是从 OpenViking 长期记忆库中自动召回的相关历史记忆与上下文（仅供参考，请结合当前问题自然使用，不要在回复中提及底层标签名）：\n" +
		rendered + "\n" +
		openVikingPromptCloseTag

	newBlock, err := json.Marshal(map[string]string{
		"type": "text",
		"text": blockText,
	})
	if err != nil {
		return nil, false
	}
	content = append(content, newBlock)
	encodedContent, err := json.Marshal(content)
	if err != nil {
		return nil, false
	}
	obj["content"] = encodedContent
	encodedObj, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	return encodedObj, true
}

func buildOpenVikingSessionMetadata(envID string, vaultIDs []string) map[string]string {
	meta := make(map[string]string, 2)
	if trimmed := strings.TrimSpace(envID); trimmed != "" {
		meta["env_id"] = trimmed
	}
	if len(vaultIDs) > 0 {
		if vaultBytes, err := json.Marshal(vaultIDs); err == nil {
			meta["vault_ids"] = string(vaultBytes)
		}
	}
	if len(meta) == 0 {
		return nil
	}
	return meta
}

// triggerOpenVikingSessionCreate asynchronously registers the newly created session in OpenViking.
func (s *Service) triggerOpenVikingSessionCreate(run apimodel.Run, agent apimodel.Agent, envID string) {
	if s == nil || strings.TrimSpace(run.ID) == "" {
		return
	}
	cfg := s.resolveOpenVikingConfig(run.WorkspaceID, agent)
	if !cfg.Enabled {
		return
	}
	meta := buildOpenVikingSessionMetadata(envID, s.arkVaultIDs)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), cfg.RecallTimeout)
		defer cancel()
		body := map[string]any{"session_id": run.ID}
		if len(meta) > 0 {
			body["metadata"] = meta
		}
		status, err := cfg.doJSON(ctx, http.MethodPost, "/api/v1/sessions", body, nil)
		if err != nil && status != http.StatusConflict {
			s.log().Debug("OpenViking 会话创建跳过", "run", run.ID, "error", err.Error())
		}
	}()
}

// triggerOpenVikingCapture runs asynchronously when a turn finishes (`session.status_idle` with
// `stop_reason != "requires_action"` and `created == true`) to persist the turn's messages to
// OpenViking and trigger background long-term memory extraction.
func (s *Service) triggerOpenVikingCapture(run apimodel.Run, endSeq int) {
	if s == nil || s.store == nil || strings.TrimSpace(run.ID) == "" || strings.TrimSpace(run.AgentID) == "" {
		return
	}
	agent, err := s.store.GetAgent(context.Background(), run.AgentID)
	if err != nil {
		return
	}
	cfg := s.resolveOpenVikingConfig(run.WorkspaceID, agent)
	if !cfg.Enabled {
		return
	}
	var envID string
	if b, bErr := s.store.GetArkBinding(context.Background(), run.WorkspaceID, run.AgentID); bErr == nil {
		envID = b.ArkEnvironmentID
	}
	vaultIDs := append([]string(nil), s.arkVaultIDs...)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), cfg.CaptureTimeout)
		defer cancel()
		if err := s.captureOpenVikingTurn(ctx, cfg, run.ID, envID, vaultIDs, endSeq); err != nil {
			s.log().Debug("OpenViking 回合记忆归档跳过", "run", run.ID, "error", err.Error())
		}
	}()
}

type openVikingChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (s *Service) captureOpenVikingTurn(
	ctx context.Context, cfg openVikingConfig, runID, envID string, vaultIDs []string, endSeq int,
) error {
	events, err := s.store.ListRunEvents(ctx, runID, persistmodel.EventQuery{})
	if err != nil || len(events) == 0 {
		return err
	}
	return captureOpenVikingEvents(ctx, cfg, runID, envID, vaultIDs, events, endSeq)
}

func captureOpenVikingEvents(
	ctx context.Context, cfg openVikingConfig, runID, envID string, vaultIDs []string,
	events []apimodel.RunEvent, endSeq int,
) error {
	// Find the start of the turn that ended at endSeq (after the previous terminal session.status_idle).
	startSeq := 0
	for _, ev := range events {
		if endSeq > 0 && ev.Sequence >= endSeq {
			break
		}
		if ev.Type == "session.status_idle" && arkframe.StopReason(ev.Raw) != "requires_action" {
			startSeq = ev.Sequence
		}
	}

	var userParts []string
	var assistantParts []string
	for _, ev := range events {
		if ev.Sequence <= startSeq {
			continue
		}
		if endSeq > 0 && ev.Sequence > endSeq {
			break
		}
		switch ev.Type {
		case "user.message":
			if text := extractMessageTextWithoutHarness(ev.Raw); text != "" {
				userParts = append(userParts, text)
			}
		case "agent.message":
			if text := extractMessageTextWithoutHarness(ev.Raw); text != "" {
				assistantParts = append(assistantParts, text)
			}
		}
	}

	userText := strings.TrimSpace(strings.Join(userParts, "\n\n"))
	assistantText := strings.TrimSpace(strings.Join(assistantParts, "\n\n"))
	if userText == "" && assistantText == "" {
		return nil
	}

	// 1. Ensure session exists (409 ALREADY_EXISTS is normal and expected on follow-up turns).
	return captureOpenVikingTurn(ctx, cfg, runID, envID, vaultIDs, userParts, assistantParts)
}

func captureOpenVikingTurn(
	ctx context.Context, cfg openVikingConfig, runID, envID string, vaultIDs []string,
	userParts, assistantParts []string,
) error {
	batch := make([]openVikingChatMessage, 0, 2)
	if len(userParts) > 0 {
		batch = append(batch, openVikingChatMessage{Role: "user", Content: strings.Join(userParts, "\n\n")})
	}
	if len(assistantParts) > 0 {
		batch = append(batch, openVikingChatMessage{Role: "assistant", Content: strings.Join(assistantParts, "\n\n")})
	}
	if len(batch) == 0 {
		return nil
	}
	createBody := map[string]any{"session_id": runID}
	if meta := buildOpenVikingSessionMetadata(envID, vaultIDs); len(meta) > 0 {
		createBody["metadata"] = meta
	}
	status, err := cfg.doJSON(ctx, http.MethodPost, "/api/v1/sessions", createBody, nil)
	if err != nil && status != http.StatusConflict {
		return fmt.Errorf("create session: %w", err)
	}

	// 2. Append turn messages in batch.
	escapedSID := url.PathEscape(runID)
	if _, err := cfg.doJSON(
		ctx,
		http.MethodPost,
		"/api/v1/sessions/"+escapedSID+"/messages/batch",
		map[string]any{"messages": batch},
		nil,
	); err != nil {
		return fmt.Errorf("append messages: %w", err)
	}

	// 3. Trigger asynchronous session commit so OpenViking archives the turn and extracts memories.
	if _, err := cfg.doJSON(
		ctx,
		http.MethodPost,
		"/api/v1/sessions/"+escapedSID+"/commit?wait=false",
		map[string]any{},
		nil,
	); err != nil {
		return fmt.Errorf("commit session: %w", err)
	}
	return nil
}

func extractMessageTextWithoutHarness(rawItem json.RawMessage) string {
	var ev struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(rawItem, &ev); err != nil {
		return ""
	}
	var parts []string
	for _, block := range ev.Content {
		if block.Type != "text" {
			continue
		}
		trimmed := strings.TrimSpace(block.Text)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, HarnessTagPrefix) || strings.HasPrefix(trimmed, "<openviking-context>") {
			continue
		}
		parts = append(parts, trimmed)
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

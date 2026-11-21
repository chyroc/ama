package core

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/shared/gopkg/arkframe"
)

// MemoryHooks applies OpenViking recall before sending a user message, and archives a turn
// after its terminal idle event. Downstream core services can install it without adopting the
// public core.Store or duplicating the wire protocol.
type MemoryHooks struct {
	Config   OpenVikingServerConfig
	VaultIDs []string
	Logger   *slog.Logger
	GetAgent func(context.Context, string) (apimodel.Agent, error)
	GetEnvID func(context.Context, string, string) string
	// Capture, when set, runs the completed-turn upload. Tests and callers that need explicit
	// lifecycle control may set it to CaptureOpenVikingTurn themselves.
	Capture func(context.Context, OpenVikingServerConfig, []string, string, string, apimodel.Agent, []apimodel.RunEvent) error
}

func (h *MemoryHooks) log() *slog.Logger {
	if h != nil && h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}

// Recall returns a copy with a memory block appended to the latest eligible user.message.
// Network and protocol failures leave the original events unchanged.
func (h *MemoryHooks) Recall(
	ctx context.Context, run apimodel.Run, agent apimodel.Agent, raw []json.RawMessage,
) []json.RawMessage {
	if h == nil {
		return raw
	}
	cfg := resolveOpenVikingConfig(h.Config, run.WorkspaceID, agent)
	if !cfg.Enabled {
		return raw
	}
	return injectOpenVikingRecall(ctx, cfg, run, raw, h.log())
}

func injectOpenVikingRecall(
	ctx context.Context, cfg openVikingConfig, run apimodel.Run, raw []json.RawMessage, log *slog.Logger,
) []json.RawMessage {
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
		targetIdx, query = i, q
		break
	}
	if targetIdx < 0 || query == "" {
		return raw
	}
	recallCtx, cancel := context.WithTimeout(ctx, cfg.RecallTimeout)
	defer cancel()
	reqPayload := map[string]any{"query": query, "mode": "context", "max_tokens": 1500, "purpose": "chat"}
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
		log.Debug("OpenViking 自动召回跳过", "run", run.ID, "error", err.Error())
		return raw
	}
	rendered := strings.TrimSpace(searchResp.Result.Rendered)
	if rendered == "" || strings.Contains(rendered, openVikingPromptCloseTag) {
		return raw
	}
	updated, ok := appendOpenVikingHarnessBlock(raw[targetIdx], rendered)
	if !ok {
		return raw
	}
	out := append([]json.RawMessage(nil), raw...)
	out[targetIdx] = updated
	return out
}

// CaptureTurn archives the event range ending at endSeq. The caller supplies a snapshot after
// the terminal event has been persisted, so it works with both PG and in-memory stores.
func (h *MemoryHooks) CaptureTurn(ctx context.Context, run apimodel.Run, endSeq int, events []apimodel.RunEvent) {
	if h == nil || h.GetAgent == nil || run.ID == "" || run.AgentID == "" {
		return
	}
	agent, err := h.GetAgent(ctx, run.AgentID)
	if err != nil {
		return
	}
	cfg := resolveOpenVikingConfig(h.Config, run.WorkspaceID, agent)
	if !cfg.Enabled {
		return
	}
	envID := ""
	if h.GetEnvID != nil {
		envID = h.GetEnvID(ctx, run.WorkspaceID, run.AgentID)
	}
	capture := h.Capture
	if capture == nil {
		capture = CaptureOpenVikingTurn
	}
	if err := capture(ctx, h.Config, h.VaultIDs, run.ID, envID, agent, openVikingTurnEvents(events, endSeq)); err != nil {
		h.log().Debug("OpenViking 回合记忆归档跳过", "run", run.ID, "error", err.Error())
	}
}

// CaptureOpenVikingTurn creates the remote session if needed, appends the turn, then commits it.
// It is synchronous: callers decide whether to run it in their managed background-task module.
func CaptureOpenVikingTurn(
	ctx context.Context, serverCfg OpenVikingServerConfig, vaultIDs []string,
	runID, envID string, agent apimodel.Agent, events []apimodel.RunEvent,
) error {
	cfg := resolveOpenVikingConfig(serverCfg, agent.WorkspaceID, agent)
	if !cfg.Enabled {
		return nil
	}
	var userParts, assistantParts []string
	for _, ev := range events {
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
	return captureOpenVikingTurn(ctx, cfg, runID, envID, vaultIDs, userParts, assistantParts)
}

func openVikingTurnEvents(events []apimodel.RunEvent, endSeq int) []apimodel.RunEvent {
	startSeq := 0
	for _, ev := range events {
		if endSeq > 0 && ev.Sequence >= endSeq {
			break
		}
		if ev.Type == "session.status_idle" && arkframe.StopReason(ev.Raw) != "requires_action" {
			startSeq = ev.Sequence
		}
	}
	out := make([]apimodel.RunEvent, 0, len(events))
	for _, ev := range events {
		if ev.Sequence <= startSeq || (endSeq > 0 && ev.Sequence > endSeq) {
			continue
		}
		out = append(out, ev)
	}
	return out
}

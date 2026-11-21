package core

import (
	"encoding/json"
	"strings"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/shared/gopkg/arkframe"
)

// IsDoneFrame reports whether a raw SSE payload is Ark's [DONE] marker.
func IsDoneFrame(raw []byte) bool { return strings.TrimSpace(string(raw)) == "[DONE]" }

// IsDeltaFrame reports whether a frame is an event_start or event_delta preview.
func IsDeltaFrame(raw []byte) bool {
	switch arkframe.Type(raw) {
	case "event_start", "event_delta":
		return true
	}
	return false
}

// SessionErrorMessage extracts the message from a session.error event.
func SessionErrorMessage(raw []byte) string {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return ""
	}
	return envelope.Error.Message
}

// AgentNameOf returns a thread event's agent_name, or the run's fallback name.
func AgentNameOf(raw []byte, fallback string) string {
	var envelope struct {
		AgentName string `json:"agent_name"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.AgentName != "" {
		return envelope.AgentName
	}
	return fallback
}

// AddTokenUsage adds all four token counters carried by a model_usage event.
func AddTokenUsage(sum *apimodel.TokenUsage, raw []byte) {
	var envelope struct {
		ModelUsage *apimodel.TokenUsage `json:"model_usage"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.ModelUsage == nil {
		return
	}
	sum.InputTokens += envelope.ModelUsage.InputTokens
	sum.OutputTokens += envelope.ModelUsage.OutputTokens
	sum.CacheReadInputTokens += envelope.ModelUsage.CacheReadInputTokens
	sum.CacheCreationInputTokens += envelope.ModelUsage.CacheCreationInputTokens
}

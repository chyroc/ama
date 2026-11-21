package app

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestStartupLogsUseActualCapability(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, nil))
	LogLoginAvailability(logger, LoginAvailability{LoginConfigured: false, MissingLoginHint: "missing test login"})
	LogTerminalWebSocket(logger, nil)
	text := output.String()
	if !strings.Contains(text, "登录不可用") || !strings.Contains(text, "missing test login") || !strings.Contains(text, "回退到 SSE+POST") {
		t.Fatalf("missing unavailable-capability diagnostics: %s", text)
	}
	output.Reset()
	LogTerminalWebSocket(logger, []string{"https://app.example.com"})
	if !strings.Contains(output.String(), "app.example.com") {
		t.Fatalf("configured origin not logged: %s", output.String())
	}
}

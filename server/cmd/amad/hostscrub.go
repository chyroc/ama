package main

import (
	"context"

	"github.com/chyroc/ama/server/core"
	hostsvc "github.com/chyroc/ama/server/service/host"
	"github.com/chyroc/ama/server/shared/hostproto"
	"github.com/chyroc/ama/server/shared/hosttools"
)

// newHostResultScrubber sends host exec results through the same secret-scrubbing path as
// webshell (core.Service.ScrubHostOutput) before they are stored: it removes plaintext secrets
// from stdout/stderr using the credential/session env injected for that Ark session. The host
// package does not depend on core directly, so the adapter lives in the wiring layer.
func newHostResultScrubber(svc *core.Service) hostsvc.ExecResultScrubber {
	return func(ctx context.Context, sessionID string, res hostproto.ExecResult) hostproto.ExecResult {
		res.Stdout = svc.ScrubHostOutput(ctx, sessionID, res.Stdout)
		res.Stderr = svc.ScrubHostOutput(ctx, sessionID, res.Stderr)
		return res
	}
}

// newHostToolResultScrubber is the same kind of scrubber for native KindTool results: a native
// result is just one final text for the agent, scrubbed before it goes into
// host_dispatches.result (kept for 7 days).
func newHostToolResultScrubber(svc *core.Service) hostsvc.ToolResultScrubber {
	return func(ctx context.Context, sessionID string, res hosttools.ToolResult) hosttools.ToolResult {
		res.Text = svc.ScrubHostOutput(ctx, sessionID, res.Text)
		return res
	}
}

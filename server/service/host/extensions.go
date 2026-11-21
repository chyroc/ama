package host

import (
	"context"

	"github.com/chyroc/ama/server/shared/hostproto"
	"github.com/chyroc/ama/server/shared/hosttools"
)

// TerminalFrameDelivery is the downstream terminal hub. Host does not import core or
// depend on a specific browser/SSE/WS transport; raw base64 passes through unchanged.
type TerminalFrameDelivery interface {
	DeliverHostTermFrame(context.Context, string, string, string, string) error
}

// NewTerminalSink forwards host daemon frames to the downstream hub without decoding them.
func NewTerminalSink(delivery TerminalFrameDelivery) TermUpSink {
	return terminalSink{delivery: delivery}
}

type terminalSink struct{ delivery TerminalFrameDelivery }

func (s terminalSink) DeliverTermFrame(
	ctx context.Context, terminalID, frameType, data, dataB64 string,
) error {
	return s.delivery.DeliverHostTermFrame(ctx, terminalID, frameType, data, dataB64)
}

// OutputScrubber removes credentials using the downstream session's actual injected env.
// It must not be a global static token list: each session can have different credentials.
type OutputScrubber interface {
	ScrubHostOutput(context.Context, string, string) string
}

// NewExecResultScrubber scrubs both stdout and stderr before persistent result storage.
func NewExecResultScrubber(scrubber OutputScrubber) ExecResultScrubber {
	return func(ctx context.Context, sessionID string, result hostproto.ExecResult) hostproto.ExecResult {
		result.Stdout = scrubber.ScrubHostOutput(ctx, sessionID, result.Stdout)
		result.Stderr = scrubber.ScrubHostOutput(ctx, sessionID, result.Stderr)
		return result
	}
}

// NewToolResultScrubber scrubs the one final text of a native tool result before storage.
func NewToolResultScrubber(scrubber OutputScrubber) ToolResultScrubber {
	return func(ctx context.Context, sessionID string, result hosttools.ToolResult) hosttools.ToolResult {
		result.Text = scrubber.ScrubHostOutput(ctx, sessionID, result.Text)
		return result
	}
}

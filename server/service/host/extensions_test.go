package host

import (
	"context"
	"testing"

	"github.com/chyroc/ama/server/shared/hostproto"
	"github.com/chyroc/ama/server/shared/hosttools"
)

type extensionTestDelivery struct {
	terminalID string
	dataB64    string
}

func (d *extensionTestDelivery) DeliverHostTermFrame(_ context.Context, terminalID, _, _, dataB64 string) error {
	d.terminalID, d.dataB64 = terminalID, dataB64
	return nil
}

type extensionTestScrubber struct{}

func (extensionTestScrubber) ScrubHostOutput(_ context.Context, _ string, text string) string {
	return "redacted"
}

func TestTerminalSinkPreservesRawBase64(t *testing.T) {
	delivery := &extensionTestDelivery{}
	sink := NewTerminalSink(delivery)
	if err := sink.DeliverTermFrame(context.Background(), "terminal_example", "output", "", "AAEC/w=="); err != nil {
		t.Fatal(err)
	}
	if delivery.dataB64 != "AAEC/w==" {
		t.Fatalf("base64 changed: %q", delivery.dataB64)
	}
}

func TestResultScrubbersCoverAllStoredText(t *testing.T) {
	result := NewExecResultScrubber(extensionTestScrubber{})(context.Background(), "session_example", hostproto.ExecResult{
		Stdout: "secret stdout", Stderr: "secret stderr",
	})
	if result.Stdout != "redacted" || result.Stderr != "redacted" {
		t.Fatalf("exec result not scrubbed: %#v", result)
	}
	tool := NewToolResultScrubber(extensionTestScrubber{})(context.Background(), "session_example", hosttools.ToolResult{Text: "secret"})
	if tool.Text != "redacted" {
		t.Fatalf("tool result not scrubbed: %#v", tool)
	}
}

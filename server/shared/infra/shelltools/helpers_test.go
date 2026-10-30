// Small helpers shared by the shelltools package tests.
package shelltools

import (
	"io"
	"log/slog"
)

// quietLog is a logger that discards output: unit tests should not flood test output with
// business logs.
func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

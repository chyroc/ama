package core

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

// ArkWorkerID returns a unique worker id with a host hint and crypto-random suffix.
// Randomness matters because colliding workers may reclaim each other's work during a rolling deploy.
func ArkWorkerID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("ama-worker-%d", time.Now().UnixNano())
	}
	suffix := hex.EncodeToString(b[:])
	if host, err := os.Hostname(); err == nil && host != "" {
		return "ama-worker-" + host + "-" + suffix
	}
	return "ama-worker-" + suffix
}

// SDKLogWriter forwards the SDK standard logger to slog without carrying raw request bodies.
type SDKLogWriter struct{ log *slog.Logger }

// NewSDKLogWriter returns a standard-library log writer backed by slog.
func NewSDKLogWriter(logger *slog.Logger) SDKLogWriter { return SDKLogWriter{log: logger} }

// Write implements io.Writer.
func (w SDKLogWriter) Write(b []byte) (int, error) {
	logger := w.log
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info(strings.TrimRight(string(b), "\n"), slog.String("from", "ark-sdk"))
	return len(b), nil
}

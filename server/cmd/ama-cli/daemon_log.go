// The log file in daemon service mode: rotated automatically by size, so /tmp/ama-daemon.log does
// not grow forever in a long-running launchd/systemd service.
//
// Why not use system facilities:
//   - macOS launchd does not rotate logs (StandardOutPath only appends);
//   - systemd --user does not necessarily come with a user-level logrotate timer;
//   - writing our own 60-line writer behaves the same on both platforms and does not depend on
//     machine configuration.
//
// Shape: <path> (current), <path>.1 (the previous one); rotate once a single file exceeds the size,
// keeping only one history copy: daemon logs are recent records for debugging and not worth keeping
// more.
package main

import (
	"os"
	"strconv"
	"sync"
)

// rotatingLogger is an io.Writer rotated by size (thread-safe; the log package writes concurrently).
type rotatingLogger struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	keep     int // number of history files (excluding the current file)
	f        *os.File
	written  int64
}

// newRotatingLogger opens (creating if missing) the log file and gets ready to rotate by maxBytes.
// The writer the caller gets can be handed straight to log.SetOutput.
func newRotatingLogger(path string, maxBytes int64, keep int) (*rotatingLogger, error) {
	r := &rotatingLogger{path: path, maxBytes: maxBytes, keep: keep}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *rotatingLogger) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	r.f = f
	r.written = info.Size()
	return nil
}

// Write satisfies io.Writer. A single Write that is itself too long is not split (a log line never
// reaches MB scale); it only rotates first when "writing this line would cross the limit".
func (r *rotatingLogger) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.written+int64(len(p)) > r.maxBytes && r.written > 0 {
		if err := r.rotate(); err != nil {
			// A failed rotate does not lose logs: keep writing to the current file (even over the
			// limit); when debugging, having the logs matters more than file size. Try again next time.
			// Keep writing as usual.
			_ = err
		}
	}
	n, err := r.f.Write(p)
	r.written += int64(n)
	return n, err
}

// rotate closes the current file → pushes out the old backup → renames the current file to .1 → opens a new file.
func (r *rotatingLogger) rotate() error {
	if r.f != nil {
		_ = r.f.Close()
	}
	// Shift from the oldest toward the newest: delete .keep-1, rename .i to .i+1. With keep=1 only .1 is deleted.
	for i := r.keep; i >= 1; i-- {
		cur := backupPath(r.path, i)
		if i == r.keep {
			_ = os.Remove(cur)
			continue
		}
		_ = os.Rename(cur, backupPath(r.path, i+1))
	}
	if err := os.Rename(r.path, backupPath(r.path, 1)); err != nil && !os.IsNotExist(err) {
		// Even if the rename fails, try to reopen the original file and keep writing.
		if rerr := r.open(); rerr != nil {
			return rerr
		}
		return err
	}
	return r.open()
}

// backupPath adds a .1/.2 suffix to path (simple string concatenation; also right when path has no extension).
func backupPath(path string, i int) string {
	return path + "." + strconv.Itoa(i)
}

// envDaemonLogFile is the environment variable name the service installer (install) writes into the
// launchd/systemd unit: when the daemon sees it at startup it connects the standard logger to the
// rotating file; when not set it writes to the terminal.
const envDaemonLogFile = "AMA_DAEMON_LOG_FILE"

// Per-file limit and number of history copies for daemon logs. 10MiB×2 (current + one backup) ≈
// 20MiB at most, enough to keep recent debugging info, and long runs will not fill up the disk.
const (
	daemonLogMaxBytes int64 = 10 << 20
	daemonLogKeep           = 1
)

// daemonLogPath is the fixed log path in service mode (the same on launchd / systemd), matching the
// path tailed by serviceLogs()/the logs command.
const daemonLogPath = "/tmp/ama-daemon.log"

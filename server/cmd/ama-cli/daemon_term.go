//go:build darwin || linux

// The daemon's web terminal: for each term_open it starts a real PTY (login shell) on this machine;
// the browser's keystrokes go via amad → dispatch to here and are written into the PTY, and PTY
// output is reported back via results.
//
// This and the six-tool exec are two separate channels: a terminal is a long-lived session (may be
// open for hours) living outside the heartbeat loop; exec is a one-shot synchronous call.
package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/chyroc/ama/server/shared/hostproto"
)

// termOpenPayload is the payload of a term_open dispatch.
type termOpenPayload struct {
	Cols  int               `json:"cols"`
	Rows  int               `json:"rows"`
	Cwd   string            `json:"cwd"`
	Env   map[string]string `json:"env,omitempty"`
	Shell string            `json:"shell,omitempty"`
}

// termInputPayload / resize payload (the shape of the browser's xterm).
type termInputPayload struct {
	Data string `json:"data"`
}
type termResizePayload struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// frameSink receives one terminal frame to report to amad (termManager does not care how it is POSTed).
type frameSink func(hostproto.TermFrame)

// termManager manages all active terminals on this machine.
// termOrphanIdleTimeout: how long a PTY can have no input/output at all before it is judged an orphan
// and reclaimed.
//
// The scenario: the browser closes the page directly / the network drops, and the term_close frame
// was never sent (or the daemon happened to be offline when it was). A terminal in normal use has
// continuous output or user keystrokes and will not be killed by mistake; quiet commands like
// `tail -f` are also kept alive periodically by input/resize from the server's 20s heartbeat / the
// browser's keepalive. 30 minutes: clearly longer than any normal pause, and also covers "walked away
// from an open terminal for a while".
const termOrphanIdleTimeout = 30 * time.Minute

// termReapInterval: how often to check for orphans.
const termReapInterval = time.Minute

type termManager struct {
	mu       sync.Mutex
	sessions map[string]*termSession
	sink     frameSink
	reapStop chan struct{}
}

type termSession struct {
	manager *termManager
	id      string
	pty     *os.File
	cmd     *exec.Cmd
	done    chan struct{}

	// closeReason is the reason for an active close (browser closed/reopened the page), used by
	// readLoop when it sends the closed frame **exactly once** after the process is reaped. closeMu
	// protects it and the closed flag.
	closeMu     sync.Mutex
	closeReason string
	closed      bool

	// lastActivity is the time of the latest "browser input or PTY output"; idle reclaiming looks at
	// it. When the browser closes the page and term_close is lost, there is no activity at all → the
	// reaper reclaims it, so the daemon does not keep an orphan PTY around for a long time. It has its
	// own lock: activity updates are very frequent and should not share termManager.mu with session
	// add/remove.
	activityMu   sync.Mutex
	lastActivity time.Time
}

func newTermManager(sink frameSink) *termManager {
	m := &termManager{
		sessions: make(map[string]*termSession),
		sink:     sink,
		reapStop: make(chan struct{}),
	}
	go m.reapLoop()
	return m
}

// reapLoop periodically reclaims orphan PTYs that have had no input/output for a long time.
func (m *termManager) reapLoop() {
	t := time.NewTicker(termReapInterval)
	defer t.Stop()
	for {
		select {
		case <-m.reapStop:
			return
		case now := <-t.C:
			for _, id := range m.idleSessionIDs(now) {
				log.Printf("终端 %s 超过 %s 无活动，按孤儿回收", id, termOrphanIdleTimeout)
				m.closeID(id, "idle_reaped")
			}
		}
	}
}

// idleSessionIDs returns the ids of terminals currently past the idle timeout (a snapshot, so close is not called while holding the lock).
func (m *termManager) idleSessionIDs(now time.Time) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for id, s := range m.sessions {
		if s.idleFor(now) > termOrphanIdleTimeout {
			out = append(out, id)
		}
	}
	return out
}

// stopReaper stops the background check (when the daemon exits; closeAll calls it too).
func (m *termManager) stopReaper() {
	select {
	case <-m.reapStop:
	default:
		close(m.reapStop)
	}
}

// has reports whether this machine still holds the terminal session. A term_open lease is only 30s,
// and on redelivery the old PTY is very likely still alive; then only send the ack again, and do not
// kill and restart like a "user reopened" (that would kill the shell the user is using).
func (m *termManager) has(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id] != nil
}

// open starts a login shell and begins reporting PTY output. Opening the same id again closes the old one first.
func (m *termManager) open(id string, raw json.RawMessage) {
	var p termOpenPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		m.sendError(id, "终端参数无法解析: "+err.Error())
		return
	}
	m.closeID(id, "reopened")

	shell := p.Shell
	if shell == "" {
		shell = loginShell()
	}
	cmd := exec.Command(shell, "-l")
	if p.Cwd != "" {
		// Same as exec: in service mode the process cwd is a system directory, so relative paths are anchored to $HOME.
		cmd.Dir = resolveCwd(p.Cwd)
	}
	// The env for the terminal shell: inherit the daemon environment + the injection sent by amad (a
	// credential snapshot, same meaning as webshell: not refreshed after the shell starts).
	cmd.Env = mergeEnviron(os.Environ(), p.Env)
	cmd.Env = append(cmd.Env, "TERM=xterm-256color")

	size := &pty.Winsize{Cols: uint16(clampDim(p.Cols, 120)), Rows: uint16(clampDim(p.Rows, 30))}
	tty, err := pty.StartWithSize(cmd, size)
	if err != nil {
		m.sendError(id, "启动终端 shell 失败: "+err.Error())
		return
	}

	s := &termSession{
		manager:      m,
		id:           id,
		pty:          tty,
		cmd:          cmd,
		done:         make(chan struct{}),
		lastActivity: time.Now(),
	}
	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()

	// Read output → report. PTY output is a byte stream in arbitrary chunks, reported chunk by chunk as
	// read (xterm stitches it together); HTTP batching at the upper layer merges frequent small chunks.
	go m.readLoop(s)
}

func (m *termManager) readLoop(s *termSession) {
	buf := make([]byte, 8192)
	for {
		n, err := s.pty.Read(buf)
		if n > 0 {
			s.touchActivity()
			// PTY output is reported as base64 of the raw bytes: it is arbitrary binary (possibly
			// invalid UTF-8), and as a JSON string it would be rewritten. closed/error still use Data
			// text.
			m.sink(hostproto.TermFrame{
				TerminalID: s.id,
				Type:       hostproto.TermFrameOutput,
				DataB64:    base64.StdEncoding.EncodeToString(buf[:n]),
			})
		}
		if err != nil {
			// Shell exit / PTY closed both come here. After the command is really reaped, this single
			// exit sends closed **exactly once**: with the recorded reason for an active close, otherwise
			// with EOF/the error.
			_ = s.cmd.Wait()
			s.closeMu.Lock()
			reason := s.closeReason
			s.closed = true
			s.closeMu.Unlock()
			if reason == "" {
				reason = "closed"
				if err != io.EOF {
					reason = err.Error()
				}
			}
			m.sink(hostproto.TermFrame{TerminalID: s.id, Type: hostproto.TermFrameClosed, Data: reason})
			m.remove(s)
			return
		}
	}
}

func (m *termManager) input(id string, raw json.RawMessage) {
	var p termInputPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return
	}
	m.mu.Lock()
	s := m.sessions[id]
	m.mu.Unlock()
	if s != nil {
		s.touchActivity()
		_, _ = s.pty.WriteString(p.Data)
	}
}

// touchActivity updates the last activity time (browser input / PTY output both count).
func (s *termSession) touchActivity() {
	s.activityMu.Lock()
	s.lastActivity = time.Now()
	s.activityMu.Unlock()
}

// idleFor returns how long it has been idle.
func (s *termSession) idleFor(now time.Time) time.Duration {
	s.activityMu.Lock()
	defer s.activityMu.Unlock()
	return now.Sub(s.lastActivity)
}

func (m *termManager) resize(id string, raw json.RawMessage) {
	var p termResizePayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return
	}
	if p.Cols <= 0 || p.Rows <= 0 {
		return
	}
	m.mu.Lock()
	s := m.sessions[id]
	m.mu.Unlock()
	if s != nil {
		_ = pty.Setsize(s.pty, &pty.Winsize{
			Cols: uint16(clampDim(p.Cols, 120)),
			Rows: uint16(clampDim(p.Rows, 30)),
		})
	}
}

// closeID closes a terminal; a non-empty reason only **records** the close reason, and the closed
// frame is sent once by readLoop after the process is reaped; this avoids an active close and
// readLoop each sending a frame, producing a duplicate closed (the second one would also carry the
// underlying error string, muddying the business close reason).
func (m *termManager) closeID(id, reason string) {
	m.mu.Lock()
	s := m.sessions[id]
	delete(m.sessions, id)
	m.mu.Unlock()
	if s == nil {
		return
	}
	if reason != "" {
		s.closeMu.Lock()
		if !s.closed {
			s.closeReason = reason
		}
		s.closeMu.Unlock()
	}
	_ = s.pty.Close()
	if s.cmd.Process != nil {
		// SIGHUP first (the hang-up semantics of a terminal); closing the PTY itself usually makes the
		// shell exit already; readLoop will reap it and report the single closed.
		_ = s.cmd.Process.Signal(syscall.SIGHUP)
	}
}

// remove only removes the session from the map and closes the PTY when the map still holds **this
// very** session.
//
// ⚠️ Identity must be compared: when the same terminalID is reopened (term_open redelivered after
// its 30s lease expired), the old readLoop may only get here after reaping; without comparing, it
// would close and remove the new session (the map has already switched to the new PTY), making the
// just-reopened terminal die immediately.
func (m *termManager) remove(s *termSession) {
	m.mu.Lock()
	cur, ok := m.sessions[s.id]
	if ok && cur == s {
		_ = s.pty.Close()
		delete(m.sessions, s.id)
	}
	m.mu.Unlock()
}

func (m *termManager) sendError(id, message string) {
	m.sink(hostproto.TermFrame{TerminalID: id, Type: hostproto.TermFrameError, Data: message})
	m.sink(hostproto.TermFrame{TerminalID: id, Type: hostproto.TermFrameClosed, Data: "open_failed"})
}

// closeAll closes all terminals when the daemon exits, and stops the orphan check.
func (m *termManager) closeAll() {
	m.stopReaper()
	m.mu.Lock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.closeID(id, "")
	}
}

// loginShell returns the user's login shell: $SHELL first, falling back to /bin/bash on macOS and
// /bin/sh on Linux (minimal images may not have bash).
func loginShell() string {
	if s := os.Getenv("SHELL"); s != "" {
		if _, err := os.Stat(s); err == nil {
			return s
		}
	}
	if _, err := os.Stat("/bin/bash"); err == nil {
		return "/bin/bash"
	}
	return "/bin/sh"
}

// clampDim brings the terminal dimensions reported by the browser into a valid range: non-positive
// values use the default, values over the uint16 limit are capped at 65535. Without clamping,
// negative/huge values converted to uint16 would wrap into a random size, and the pty would fail to
// be created or produce an odd window.
func clampDim(v, def int) int {
	if v <= 0 {
		return def
	}
	if v > 65535 {
		return 65535
	}
	return v
}

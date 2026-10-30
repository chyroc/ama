package dockertools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chyroc/ama/server/shared/infra/docker"
)

// The "view the container" features on the page: check state, resume, list directories, read files,
// run a command, open a terminal.
//
// They use the same container as the six tools (the name is derived from the session id), but they
// do not go through shelltools: these requests come from the browser, land on any instance, and
// only have the session id and the database record.

// ErrNotFound means the container (and its workspace volume) are both gone, or the file to read
// does not exist.
var ErrNotFound = errors.New("dockertools: 不存在")

// ErrOtherDaemon means this session's container was created on another Docker daemon that this
// instance cannot see.
//
// **Do not treat it as "reaped"**: the container may be alive and well on another daemon, and
// saying "gone" would make people think the workspace was lost.
var ErrOtherDaemon = errors.New("dockertools: 容器在另一个 Docker daemon 上，本实例看不到")

// IsNotFound reports whether err means "the thing is gone".
func IsNotFound(err error) bool {
	return errors.Is(err, ErrNotFound) || docker.IsNotFound(err)
}

// Container states.
const (
	StateRunning = "running"
	StatePaused  = "paused"
	// StateStopped means the container stopped, or the container was removed but the workspace
	// volume is still there. Both can be "resumed".
	StateStopped = "stopped"
)

// State is the current state of the container.
type State struct {
	Status    string
	StartedAt string
	// ContainerMissing means the container itself is gone and only the workspace volume is left.
	ContainerMissing bool
}

// Attached is a handle to a session's container (for the page).
type Attached struct {
	p            *Provider
	arkSessionID string
	name         string
}

// Attach reconnects to a session's container.
//
// daemonID is the "which daemon the container was created on" value stored in the database; when it
// differs from the one this instance is connected to, it returns ErrOtherDaemon.
// When neither the container nor the workspace volume exists, it returns ErrNotFound.
func (p *Provider) Attach(ctx context.Context, arkSessionID, daemonID string) (*Attached, error) {
	if mine := p.DaemonID(); daemonID != "" && mine != "" && daemonID != mine {
		return nil, fmt.Errorf("%w（容器所在 daemon=%s，本实例 daemon=%s）", ErrOtherDaemon, daemonID, mine)
	}
	a := &Attached{p: p, arkSessionID: arkSessionID, name: p.ContainerName(arkSessionID)}
	if _, err := a.State(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

// Name returns the container name.
func (a *Attached) Name() string { return a.name }

// State looks up the state right now.
func (a *Attached) State(ctx context.Context) (*State, error) {
	info, err := a.p.engine.ContainerInspect(ctx, a.name)
	if docker.IsNotFound(err) {
		if _, verr := a.p.engine.VolumeInspect(ctx, a.p.VolumeName(a.arkSessionID)); verr != nil {
			if docker.IsNotFound(verr) {
				return nil, ErrNotFound
			}
			return nil, verr
		}
		return &State{Status: StateStopped, ContainerMissing: true}, nil
	}
	if err != nil {
		return nil, err
	}
	st := &State{StartedAt: info.State.StartedAt}
	switch info.State.Status {
	case "running":
		st.Status = StateRunning
	case "paused":
		st.Status = StatePaused
	default:
		st.Status = StateStopped
	}
	return st, nil
}

// Resume brings the container back to running: unfreeze if paused, start if stopped, rebuild if
// only the volume is left (a rebuild injects skills and records activity as usual; it is the same
// code as a rebuild triggered by a tool call).
func (a *Attached) Resume(ctx context.Context) error {
	_, err := a.p.session(a.arkSessionID).ensureRunning(ctx)
	return err
}

// run executes in the container and requires it to be running (actions on the page do not
// auto-resume the container).
func (a *Attached) run(ctx context.Context, opt docker.RunOptions) (*docker.RunResult, error) {
	return a.p.engine.Run(ctx, a.name, opt)
}

// FileEntry is one entry of a directory listing.
type FileEntry struct {
	Name          string
	Path          string
	Type          string // file / directory / symlink / other
	Size          int64
	Permissions   string
	ModTime       string
	SymlinkTarget string
}

// listScript uses GNU find to list one level at a time, fields separated by \t and entries by \0
// (file names can contain newlines).
// When the directory does not exist it exits 44, so the caller can tell "does not exist" from
// "failed to run".
const listScript = `[ -d "$1" ] || exit 44
find "$1" -mindepth 1 -maxdepth "$2" -printf '%y\t%s\t%M\t%T@\t%l\t%p\0'`

// List lists a directory. depth is the recursion depth; 1 lists only the current level.
func (a *Attached) List(ctx context.Context, dir string, depth int) ([]FileEntry, error) {
	if depth < 1 {
		depth = 1
	}
	out, err := a.run(ctx, docker.RunOptions{
		Cmd:         []string{"sh", "-c", listScript, "sh", dir, strconv.Itoa(depth)},
		OutputLimit: 8 << 20,
	})
	if err != nil {
		return nil, err
	}
	if out.ExitCode == 44 {
		return nil, fmt.Errorf("%w: 目录 %s", ErrNotFound, dir)
	}
	if out.ExitCode != 0 && len(out.Stdout) == 0 {
		return nil, fmt.Errorf("列目录失败（exit=%d）: %s", out.ExitCode, strings.TrimSpace(string(out.Stderr)))
	}
	var entries []FileEntry
	for _, rec := range bytes.Split(out.Stdout, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		f := strings.SplitN(string(rec), "\t", 6)
		if len(f) != 6 {
			continue
		}
		size, _ := strconv.ParseInt(f[1], 10, 64)
		e := FileEntry{
			Name:          path.Base(f[5]),
			Path:          f[5],
			Type:          fileType(f[0]),
			Size:          size,
			Permissions:   f[2],
			ModTime:       epochISO(f[3]),
			SymlinkTarget: f[4],
		}
		entries = append(entries, e)
	}
	// Directories first, then by name, matching the usual file manager habit.
	sort.SliceStable(entries, func(i, j int) bool {
		di, dj := entries[i].Type == "directory", entries[j].Type == "directory"
		if di != dj {
			return di
		}
		return entries[i].Name < entries[j].Name
	})
	return entries, nil
}

func fileType(y string) string {
	switch y {
	case "f":
		return "file"
	case "d":
		return "directory"
	case "l":
		return "symlink"
	default:
		return "other"
	}
}

// epochISO turns find's %T@ (unix seconds with a fraction) into a millisecond-precision ISO string.
func epochISO(s string) string {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return ""
	}
	sec, frac := math.Modf(f)
	return time.Unix(int64(sec), int64(frac*1e9)).UTC().Format("2006-01-02T15:04:05.000Z")
}

// Download reads the first limit bytes of a regular file. Returns ErrNotFound when the file does
// not exist / is not a regular file.
func (a *Attached) Download(ctx context.Context, file string, limit int64) (io.ReadCloser, error) {
	if limit <= 0 {
		limit = 1 << 20
	}
	out, err := a.run(ctx, docker.RunOptions{
		Cmd:         []string{"sh", "-c", `[ -f "$1" ] || exit 44; head -c "$2" -- "$1"`, "sh", file, strconv.FormatInt(limit, 10)},
		OutputLimit: limit,
	})
	if err != nil {
		return nil, err
	}
	if out.ExitCode == 44 {
		return nil, fmt.Errorf("%w: 文件 %s", ErrNotFound, file)
	}
	if out.ExitCode != 0 {
		return nil, fmt.Errorf("读文件失败（exit=%d）: %s", out.ExitCode, strings.TrimSpace(string(out.Stderr)))
	}
	return io.NopCloser(bytes.NewReader(out.Stdout)), nil
}

// ShellResult is the result of running a command from the web page.
type ShellResult struct {
	Output   string
	ExitCode *int
	// Status is completed / timeout.
	Status string
}

// shellScript runs the user's command in a shell that "remembers".
//
// Docker has no long-lived shell session; every command is a new exec. So that the next command is
// still in the directory after a `cd`, the cwd is stored in a small file inside the container (one
// per shellID), read at the start of each command and written at the end. Environment variables are
// not kept (that needs a real long-lived process; use the terminal for that).
//
// The command text is passed in through an environment variable and then eval'ed, instead of being
// spliced into the script: splicing has to handle quote escaping, while eval of a variable's value
// needs no changes at all.
const shellScript = `exec 2>&1
mkdir -p /tmp/ama-webshell
d=$(cat "$AMA_WEBSHELL_STATE" 2>/dev/null)
cd "${d:-$AMA_WEBSHELL_HOME}" 2>/dev/null || cd "$AMA_WEBSHELL_HOME" 2>/dev/null || cd /
eval "$AMA_WEBSHELL_CMD"
rc=$?
pwd > "$AMA_WEBSHELL_STATE" 2>/dev/null
exit $rc`

// Shell runs a command for at most timeout (killed inside the container on time).
func (a *Attached) Shell(ctx context.Context, shellID, command string, timeout time.Duration) (*ShellResult, error) {
	if timeout <= 0 {
		timeout = time.Minute
	}
	secs := int(math.Ceil(timeout.Seconds()))
	safeID := nameKey(shellID)
	rctx, cancel := context.WithTimeout(ctx, timeout+15*time.Second)
	defer cancel()
	out, err := a.run(rctx, docker.RunOptions{
		Cmd: []string{"timeout", "-k", "3", strconv.Itoa(secs), "bash", "-lc", shellScript},
		Env: []string{
			"AMA_WEBSHELL_CMD=" + command,
			"AMA_WEBSHELL_STATE=/tmp/ama-webshell/" + safeID + ".cwd",
			"AMA_WEBSHELL_HOME=" + a.p.cfg.Workdir,
			"TERM=dumb",
		},
		WorkingDir:  a.p.cfg.Workdir,
		OutputLimit: 1 << 20,
	})
	if err != nil {
		return nil, err
	}
	code := out.ExitCode
	res := &ShellResult{Output: string(out.Stdout), ExitCode: &code, Status: "completed"}
	if code == 124 || code == 137 {
		res.Status = "timeout"
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// Terminal
// ---------------------------------------------------------------------------

// Terminal is an interactive shell in the container (docker exec -it bash -l).
//
// From the outside it looks like a WebSocket connection that "sends and receives JSON text frames",
// and the frame shape is the protocol the browser terminal already speaks:
//
//	browser → container  {"type":"input","data":"ls\r"}  /  {"type":"resize","data":{"cols":120,"rows":30}}
//	container → browser  {"type":"output","dataB64":"<base64 of raw PTY bytes>"}
//
// This way the terminal forwarding code in core (the SSE fallback and WS paths) does not need to
// know there is a docker exec behind it.
// Output uses dataB64: the PTY byte stream may break in the middle of a multi-byte UTF-8 character,
// and turning that into a Go string and then JSON-encoding it would replace half a character with
// �.
type Terminal struct {
	conn   *docker.HijackedConn
	engine Engine
	execID string

	// readErr is the error that came "together with data" and is kept for the next read: hand over
	// the data first, otherwise the last screen of output before the shell exits would be lost.
	readErr error
	closeMu sync.Once
}

// textMessage is the same as gorilla/websocket.TextMessage (RFC 6455 opcode 1).
const textMessage = 1

// OpenTerminal opens a login shell in the container. Requires the container to be running.
func (a *Attached) OpenTerminal(ctx context.Context) (*Terminal, error) {
	id, err := a.p.engine.ExecCreate(ctx, a.name, docker.ExecConfig{
		Cmd:          []string{"/bin/sh", "-c", "if command -v bash >/dev/null 2>&1; then exec bash -l; else exec sh -l; fi"},
		Env:          []string{"TERM=xterm-256color"},
		WorkingDir:   a.p.cfg.Workdir,
		Tty:          true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return nil, err
	}
	conn, err := a.p.engine.ExecStartAttached(ctx, id, true)
	if err != nil {
		return nil, err
	}
	return &Terminal{conn: conn, engine: a.p.engine, execID: id}, nil
}

// ReadMessage reads one output frame.
func (t *Terminal) ReadMessage() (int, []byte, error) {
	if t.readErr != nil {
		return 0, nil, t.readErr
	}
	buf := make([]byte, 32<<10)
	n, err := t.conn.Read(buf)
	if err != nil {
		t.readErr = err
		if n == 0 {
			return 0, nil, err
		}
	}
	frame, _ := json.Marshal(map[string]string{
		"type":    "output",
		"dataB64": base64.StdEncoding.EncodeToString(buf[:n]),
	})
	return textMessage, frame, nil
}

// WriteMessage takes one frame of browser input. Unknown frame types are ignored (forward
// compatible).
func (t *Terminal) WriteMessage(_ int, data []byte) error {
	var f struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return nil // drop frames that cannot be decoded, do not drop the connection
	}
	switch f.Type {
	case "input":
		var s string
		if err := json.Unmarshal(f.Data, &s); err != nil {
			return nil
		}
		_, err := t.conn.Write([]byte(s))
		return err
	case "resize":
		var sz struct {
			Cols int `json:"cols"`
			Rows int `json:"rows"`
		}
		if err := json.Unmarshal(f.Data, &sz); err != nil || sz.Cols <= 0 || sz.Rows <= 0 {
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Failing to resize only affects layout; do not drop the connection.
		_ = t.engine.ExecResize(ctx, t.execID, sz.Rows, sz.Cols)
		return nil
	default:
		return nil
	}
}

// SetReadDeadline / SetWriteDeadline are passed through to the underlying connection.
func (t *Terminal) SetReadDeadline(d time.Time) error  { return t.conn.SetReadDeadline(d) }
func (t *Terminal) SetWriteDeadline(d time.Time) error { return t.conn.SetWriteDeadline(d) }

// Close closes the terminal. Once the connection drops, the shell on the PTY side gets SIGHUP and
// exits.
func (t *Terminal) Close() error {
	var err error
	t.closeMu.Do(func() { err = t.conn.Close() })
	return err
}

// Session containers: store "which container this Run uses" in the database, and query its
// state live.
//
// # Why this layer exists
//
// The tool executor (shared/infra/dockertools) derives the container name from the session
// id, so the worker itself needs no mapping table. But when the frontend wants to look at a
// container, the request lands on **any instance** through the load balancer, and that
// instance has to answer two things:
//
//   - did this Run ever have a container at all (this is what separates none from gone among
//     the four states);
//   - which Docker daemon the container was created on (instances may connect to different
//     daemons).
//
// So when a container is created we record a row in PG (run_sandbox_sessions), which every
// instance can query.
//
// # Four states, not three
//
// ⚠️ **"Never had a container" and "the container is gone" must be kept apart.**
//
// Plain-text chats never create a container (lazy creation). Showing that case as
// "reclaimed" is a **fake failure**: users think their workspace was wiped and come asking
// "where are my files", when there were never any files.
//
//	none    no record in the database                → this chat has not used a container yet
//	running container is running                     → can list files, can open a shell
//	paused  paused / stopped / only the workspace left → can resume, workspace still there
//	gone    container and workspace volume both gone → reclaimed, workspace has been reset
//
// # State is always queried live, never cached
//
// The database **only stores things that never change after creation**: container name, its
// daemon, creation time. State is inspected live every time: a container's state **changes on
// its own** (paused when idle, deleted by the reclaim scan), and storing a snapshot leads to
// "the page shows running while it is long gone".
package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	gopath "path"
	"strings"
	"time"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/shared/infra/dockertools"
	"github.com/chyroc/ama/server/shared/infra/shelltools"
)

// RunSandbox has moved to model/persistmodel (a Store row struct that does not go into HTTP
// responses).

// The four container states. These four strings go straight into HTTP responses and the
// frontend branches on them, so **changing them breaks the API**.
const (
	// SandboxStatusNone means this Run never created a container.
	//
	// **Not a failure**: plain-text chats do not touch containers. In this state the frontend
	// should show no buttons and should not say "reclaimed", which would make users think
	// something was lost.
	SandboxStatusNone = "none"
	// SandboxStatusRunning means the container is alive and not paused; files can be listed
	// and a shell can be opened.
	SandboxStatusRunning = "running"
	// SandboxStatusPaused means the container is paused: compute resources are returned, and
	// **the workspace is still there**.
	//
	// The difference from gone is "can it come back". This is the pair among the four states
	// most likely to be merged, and the one that should least be merged.
	SandboxStatusPaused = "paused"
	// SandboxStatusGone means the container was reclaimed by the platform, and the workspace
	// and its files are gone with it.
	SandboxStatusGone = "gone"
)

// ErrSandboxGone means the container and its workspace are both gone (gone among the four
// states).
//
// SandboxAttacher implementations must translate "does not exist" into it (checkable with
// errors.Is). **The gone state depends entirely on this check**; if it is not recognized,
// "reclaimed" is reported as a 500.
var ErrSandboxGone = errors.New("容器已不存在")

func isSandboxGone(err error) bool { return errors.Is(err, ErrSandboxGone) }

// SandboxAttacher reattaches a Run to its container.
//
// It is an interface rather than using dockertools directly: **it can be tested offline**,
// and core does not have to tie itself to the executor at the type level.
type SandboxAttacher interface {
	// AttachSandbox reattaches the container. sandboxID is the stored "which daemon the
	// container is on".
	AttachSandbox(ctx context.Context, runID, sandboxID string) (SandboxSession, error)
}

// Live container states (SandboxInfo.Status).
const (
	sandboxInfoRunning = "running"
	sandboxInfoPaused  = "paused"
	sandboxInfoStopped = "stopped"
)

// SandboxInfo is the live state of a container.
type SandboxInfo struct {
	// Status is running / paused / stopped. stopped includes "container deleted but the
	// workspace is still there".
	Status string
	// StartedAt is when the container last started (RFC3339 from the daemon, passed through
	// as is).
	StartedAt string
}

// SandboxFSEntry is one item of a directory listing.
type SandboxFSEntry struct {
	Name          string
	Path          string
	Type          string
	Size          int64
	Permissions   string
	ModTime       string
	SymlinkTarget string
}

// SandboxShellOutput is the result of running one command in the webshell.
type SandboxShellOutput struct {
	Output   string
	ExitCode *int
	Status   string
}

// TerminalConn is one container terminal, with the same read/write methods as
// gorilla/websocket.Conn: it sends and receives the JSON text frames of the browser terminal
// protocol (see dockertools.Terminal).
type TerminalConn interface {
	ReadMessage() (int, []byte, error)
	WriteMessage(messageType int, data []byte) error
	SetReadDeadline(t time.Time) error
	SetWriteDeadline(t time.Time) error
	Close() error
}

// SandboxSession is the few abilities we actually use on a reattached container.
type SandboxSession interface {
	// Info queries the current state. **Queried live every time** (see "State is always
	// queried live" in the file header).
	Info(ctx context.Context) (*SandboxInfo, error)
	// Resume brings the container back to running.
	Resume(ctx context.Context) error
	// List lists a directory. depth controls recursion depth; 1 means only the current level.
	List(ctx context.Context, path string, depth int) ([]SandboxFSEntry, error)
	// Shell runs one webshell command. A stable shellID means cwd is kept across commands.
	Shell(ctx context.Context, shellID, command string, timeout time.Duration) (*SandboxShellOutput, error)
	// Download reads the first limit bytes of a file.
	Download(ctx context.Context, path string, limit int64) (io.ReadCloser, error)
	// OpenTerminal opens an interactive shell in the container.
	OpenTerminal(ctx context.Context) (TerminalConn, error)
}

// SetSandboxAttacher sets up the container attach ability.
//
// It is a setter because the Service is created before the executor. **It works without it
// too**: queries then return a precondition error saying "not set up".
func (s *Service) SetSandboxAttacher(a SandboxAttacher) {
	s.sandboxAttacher = a
}

// RecordSession records "which container this Run uses now".
//
// It implements dockertools.SessionRecorder and is called back **every time a new container
// is created** (first lazy creation + recreation after deletion). sessionID is the container
// name, sandboxID is the id of the Docker daemon the container is on.
//
// **Failures are only logged, not returned**: a bookkeeping failure should not fail a tool
// call. The cost is that this Run's container is not visible on the page (falls back to
// none), while the chat itself keeps running.
func (s *Service) RecordSession(ctx context.Context, runID, sessionID, sandboxID string) {
	runID, sessionID = strings.TrimSpace(runID), strings.TrimSpace(sessionID)
	if runID == "" || sessionID == "" {
		return
	}
	rec := persistmodel.RunSandbox{
		RunID:     runID,
		SessionID: sessionID,
		SandboxID: strings.TrimSpace(sandboxID),
		CreatedAt: time.Now(),
	}
	if err := s.store.PutRunSandbox(ctx, rec); err != nil {
		s.log().Warn("记录会话容器失败，该会话的容器在页面上会显示为「未创建」",
			slog.String("run", runID),
			slog.String("session", sessionID),
			slog.String("err", err.Error()))
	}
}

// LastSession returns the container name recorded for this Run, implementing
// dockertools.SessionLookup.
//
// It is only used for log wording: when both the container and workspace are gone, it tells
// "first creation" apart from "workspace has been reset".
//
// **When nothing is found it returns an empty string with err == nil**: "none" is a normal
// result, not a failure. Plain-text chats never create a container, so many Runs simply have
// no record. Treating it as an error would log a "ledger lookup failed" line for every
// plain-text session, and real failures would be buried.
func (s *Service) LastSession(ctx context.Context, runID string) (string, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return "", nil
	}
	rec, err := s.store.GetRunSandbox(ctx, runID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", nil
		}
		return "", err
	}
	return rec.SessionID, nil
}

// ActiveTerminalSessions returns the container names whose webshell terminals are still in
// use, implementing dockertools.TerminalActivity.
//
// Idle shutdown uses it to skip containers where "someone is working in the terminal". This
// check must work across instances: terminal requests and idle shutdown are almost never in
// the same process.
func (s *Service) ActiveTerminalSessions(ctx context.Context) ([]string, error) {
	return s.store.TerminalActiveSessions(ctx)
}

// TouchSession sets the "last activity time" of this Run's container to now, implementing
// dockertools.ActivityLedger.
//
// The reclaim scan uses it to judge how long a container has been idle. Failures are only
// logged at Debug: every tool call may reach here, a single database glitch should not flood
// the logs, and the worst result is that reclaim time is counted a bit earlier.
func (s *Service) TouchSession(ctx context.Context, runID string) {
	if strings.TrimSpace(runID) == "" {
		return
	}
	if err := s.store.TouchRunSandboxActivity(ctx, runID); err != nil {
		s.log().Debug("刷新容器活动时刻失败", slog.String("run", runID), slog.String("err", err.Error()))
	}
}

// SessionActivity fetches the last activity times of containers in a batch, implementing
// dockertools.ActivityLedger.
func (s *Service) SessionActivity(ctx context.Context, runIDs []string) (map[string]time.Time, error) {
	return s.store.RunSandboxActivity(ctx, runIDs)
}

// Compile-time check that Service satisfies the interfaces on the dockertools side.
//
// **This line cannot be dropped**: interfaces match structurally, and a signature mismatch
// would otherwise only show up at the wiring line.
var (
	_ dockertools.SessionRecorder  = (*Service)(nil)
	_ dockertools.SessionLookup    = (*Service)(nil)
	_ dockertools.ActivityLedger   = (*Service)(nil)
	_ dockertools.TerminalActivity = (*Service)(nil)
)

// RunSandboxState queries the current state of a Run's container.
//
// **Authorization is done by the Guard in the HTTP layer** (the authz table works out the
// workspace from KindRun), so it is not checked again here. That is exactly the "do not push
// authorization down into the Service layer" rule that authz/README.md keeps stressing.
func (s *Service) RunSandboxState(ctx context.Context, runID string) (SandboxState, error) {
	rec, err := s.store.GetRunSandbox(ctx, runID)
	if err != nil {
		// Not found = this Run never created a container. **This is a normal result**, not
		// an error.
		if errors.Is(err, ErrNotFound) {
			return SandboxState{Status: SandboxStatusNone}, nil
		}
		return SandboxState{}, err
	}

	base := SandboxState{
		SessionID: rec.SessionID,
		SandboxID: rec.SandboxID,
		CreatedAt: apimodel.FormatISO(rec.CreatedAt),
	}

	sess, err := s.attachSandbox(ctx, runID, rec.SandboxID)
	if err != nil {
		// Attach failures come in two kinds that **must be kept apart**:
		//   does not exist → container and workspace are both gone (gone), an expected end
		//                    (reclaim scan);
		//   anything else  → daemon unreachable, container on another daemon... saying
		//                    "reclaimed" then would be a lie, and users would think the
		//                    workspace was lost. Let it surface as an error; the page shows
		//                    "query failed".
		if isSandboxGone(err) {
			base.Status = SandboxStatusGone
			return base, nil
		}
		return SandboxState{}, fmt.Errorf("接入容器失败（container=%s）: %w", rec.SessionID, err)
	}

	info, err := sess.Info(ctx)
	if err != nil {
		if isSandboxGone(err) {
			base.Status = SandboxStatusGone
			return base, nil
		}
		return SandboxState{}, fmt.Errorf("查询容器状态失败（container=%s）: %w", rec.SessionID, err)
	}

	return fillSandboxState(base, info), nil
}

// fillSandboxState translates the state from the executor into our four states.
//
// It is a separate pure function so it can be table-tested directly.
func fillSandboxState(base SandboxState, info *SandboxInfo) SandboxState {
	if info == nil {
		// With no usable information, gone is the only answer that does not mislead users.
		base.Status = SandboxStatusGone
		return base
	}
	switch info.Status {
	case sandboxInfoRunning:
		base.Status = SandboxStatusRunning
	case sandboxInfoPaused, sandboxInfoStopped:
		// Stopped (and "container deleted, workspace volume still there") is the same as
		// paused for users: the workspace is still there, and one click brings it back.
		base.Status = SandboxStatusPaused
		base.CanResume = true
	default:
		base.Status = SandboxStatusGone
	}
	return base
}

// ResumeRunSandbox resumes a Run's container.
//
// **Called only when the user explicitly clicks "resume"**; viewing state and listing files
// never resume automatically. Resuming takes compute resources again (a stopped container is
// restarted, a deleted one is recreated), while the user only wanted to take a look. Letting
// a read-only action cost that, without the user knowing what they triggered, is wrong.
func (s *Service) ResumeRunSandbox(ctx context.Context, runID string) (SandboxState, error) {
	rec, err := s.store.GetRunSandbox(ctx, runID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// No container to resume. Return ErrNotFound so the HTTP layer turns it into a 404,
			// instead of pretending it succeeded.
			return SandboxState{}, ErrNotFound
		}
		return SandboxState{}, err
	}

	sess, err := s.attachSandbox(ctx, runID, rec.SandboxID)
	if err != nil {
		if isSandboxGone(err) {
			return SandboxState{
				Status:    SandboxStatusGone,
				SessionID: rec.SessionID,
				SandboxID: rec.SandboxID,
				CreatedAt: apimodel.FormatISO(rec.CreatedAt),
			}, nil
		}
		return SandboxState{}, fmt.Errorf("接入容器失败（container=%s）: %w", rec.SessionID, err)
	}

	if err := sess.Resume(ctx); err != nil {
		if isSandboxGone(err) {
			return s.RunSandboxState(ctx, runID)
		}
		return SandboxState{}, fmt.Errorf("恢复容器失败（container=%s）: %w", rec.SessionID, err)
	}

	s.log().Info("容器已按用户请求恢复",
		slog.String("run", runID), slog.String("container", rec.SessionID))

	return s.RunSandboxState(ctx, runID)
}

// attachSandbox reattaches the container, and gives a clear error when no attacher is set up.
func (s *Service) attachSandbox(ctx context.Context, runID, sandboxID string) (SandboxSession, error) {
	if s.sandboxAttacher == nil {
		// Not set up (no Docker executor configured). **Not gone**: calling it "reclaimed"
		// would send people chasing a failure that does not exist.
		return nil, fmt.Errorf("%w: 未装配容器接入能力（Docker 执行端未启用）", ErrPrecondition)
	}
	return s.sandboxAttacher.AttachSandbox(ctx, runID, sandboxID)
}

// DockerSandboxAttacher adapts *dockertools.Provider to SandboxAttacher.
//
// The only translation needed is for errors: dockertools "does not exist" → ErrSandboxGone,
// "container is on another daemon" → ErrPrecondition (**not gone**; the container may be alive
// and well).
type DockerSandboxAttacher struct {
	P *dockertools.Provider
}

// AttachSandbox implements SandboxAttacher.
func (a DockerSandboxAttacher) AttachSandbox(ctx context.Context, runID, sandboxID string) (SandboxSession, error) {
	att, err := a.P.Attach(ctx, runID, sandboxID)
	if err != nil {
		return nil, translateDockerErr(err)
	}
	return dockerSandbox{a: att}, nil
}

func translateDockerErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, dockertools.ErrOtherDaemon):
		return fmt.Errorf("%w: %v", ErrPrecondition, err)
	case dockertools.IsNotFound(err):
		return fmt.Errorf("%w: %v", ErrSandboxGone, err)
	default:
		return err
	}
}

// dockerSandbox adapts *dockertools.Attached to SandboxSession.
type dockerSandbox struct{ a *dockertools.Attached }

func (d dockerSandbox) Info(ctx context.Context) (*SandboxInfo, error) {
	st, err := d.a.State(ctx)
	if err != nil {
		return nil, translateDockerErr(err)
	}
	return &SandboxInfo{Status: st.Status, StartedAt: st.StartedAt}, nil
}

func (d dockerSandbox) Resume(ctx context.Context) error {
	return translateDockerErr(d.a.Resume(ctx))
}

func (d dockerSandbox) List(ctx context.Context, path string, depth int) ([]SandboxFSEntry, error) {
	entries, err := d.a.List(ctx, path, depth)
	if err != nil {
		return nil, translateDockerErr(err)
	}
	out := make([]SandboxFSEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, SandboxFSEntry{
			Name: e.Name, Path: e.Path, Type: e.Type, Size: e.Size,
			Permissions: e.Permissions, ModTime: e.ModTime, SymlinkTarget: e.SymlinkTarget,
		})
	}
	return out, nil
}

func (d dockerSandbox) Shell(ctx context.Context, shellID, command string, timeout time.Duration) (*SandboxShellOutput, error) {
	res, err := d.a.Shell(ctx, shellID, command, timeout)
	if err != nil {
		return nil, translateDockerErr(err)
	}
	return &SandboxShellOutput{Output: res.Output, ExitCode: res.ExitCode, Status: res.Status}, nil
}

func (d dockerSandbox) Download(ctx context.Context, path string, limit int64) (io.ReadCloser, error) {
	rc, err := d.a.Download(ctx, path, limit)
	return rc, translateDockerErr(err)
}

func (d dockerSandbox) OpenTerminal(ctx context.Context) (TerminalConn, error) {
	t, err := d.a.OpenTerminal(ctx)
	if err != nil {
		return nil, translateDockerErr(err)
	}
	return t, nil
}

// ---------------------------------------------------------------------------
// File listing (read-only)
// ---------------------------------------------------------------------------

// Two hard limits for directory listing.
const (
	// sandboxListMaxEntries is the max number of entries returned at once.
	//
	// Beyond it the list is truncated and the response is marked Truncated. **Truncation must
	// be stated**: quietly returning fewer entries makes users think "that is all there is in
	// the directory", and a directory like node_modules easily has tens of thousands of
	// entries. Without the mark, the question "where are my files" would point at the
	// container, when the truth is that we cut it ourselves.
	sandboxListMaxEntries = 500

	// sandboxListMaxDepth is the recursion depth limit.
	//
	// Setting it to 1 (only the current level) is deliberate: deep recursion in the container
	// is a full disk walk, slow and large, and in the UI users click in one level at a time
	// anyway. To see a subdirectory, send another request.
	sandboxListMaxDepth = 1
)

// ListRunSandboxFiles lists a directory in the container.
//
// **Read-only**: this feature offers no delete, overwrite or upload. Those are a different
// thing with completely different risk (one wrong click could destroy the workspace the agent
// is using), and if ever built they need their own design and their own permissions.
//
// When the container is not running it is **not resumed automatically**; the state is
// returned to the frontend so it can show a "resume" button. See ResumeRunSandbox for why.
func (s *Service) ListRunSandboxFiles(ctx context.Context, runID, path string) (SandboxListing, error) {
	clean, err := cleanSandboxPath(path)
	if err != nil {
		return SandboxListing{}, err
	}

	state, err := s.RunSandboxState(ctx, runID)
	if err != nil {
		return SandboxListing{}, err
	}
	// Anything not running returns the state + an empty list right away.
	//
	// **Files must be []{} rather than nil**: the frontend calls .map() on it right away, and
	// nil serializes to null, which throws.
	if state.Status != SandboxStatusRunning {
		return SandboxListing{Status: state.Status, Path: clean, Files: []SandboxFile{}}, nil
	}

	rec, err := s.store.GetRunSandbox(ctx, runID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return SandboxListing{Status: SandboxStatusNone, Path: clean, Files: []SandboxFile{}}, nil
		}
		return SandboxListing{}, err
	}

	sess, err := s.attachSandbox(ctx, runID, rec.SandboxID)
	if err != nil {
		if isSandboxGone(err) {
			return SandboxListing{Status: SandboxStatusGone, Path: clean, Files: []SandboxFile{}}, nil
		}
		return SandboxListing{}, fmt.Errorf("接入容器失败（container=%s）: %w", rec.SessionID, err)
	}

	entries, err := sess.List(ctx, clean, sandboxListMaxDepth)
	if err != nil {
		// Directory does not exist (or the container vanished at this moment): return an
		// empty list, not a 500.
		if isSandboxGone(err) {
			return SandboxListing{Status: state.Status, Path: clean, Files: []SandboxFile{}}, nil
		}
		return SandboxListing{}, fmt.Errorf("列目录失败（session=%s path=%s）: %w", rec.SessionID, clean, err)
	}

	return buildSandboxListing(state.Status, clean, entries), nil
}

// buildSandboxListing translates the platform's entries into our shape and truncates.
//
// It is a separate pure function so the truncation branch can be tested directly; in a real
// environment it takes 500 files to trigger.
func buildSandboxListing(status, path string, entries []SandboxFSEntry) SandboxListing {
	out := SandboxListing{Status: status, Path: path, Files: []SandboxFile{}}
	if len(entries) > sandboxListMaxEntries {
		entries = entries[:sandboxListMaxEntries]
		out.Truncated = true
	}
	for _, e := range entries {
		out.Files = append(out.Files, SandboxFile{
			Name: e.Name,
			Path: e.Path,
			// Use the Type given by the executor as is, do not compute it from Mode bits;
			// see the comment on SandboxFile.Type.
			Type:          e.Type,
			Size:          e.Size,
			Permissions:   e.Permissions,
			ModTime:       e.ModTime,
			SymlinkTarget: e.SymlinkTarget,
		})
	}
	return out
}

// cleanSandboxPath normalizes the path given by the user and blocks escaping the root.
//
// # Why this step exists
//
// path is **given directly by the user** (a query parameter). Without normalizing, something
// like `/a/../../etc` would be sent into the container as is. Commands in the container parse
// it themselves too, but we should not hand that decision over. Also, only after
// normalizing is the Path returned to the frontend stable (clicking `..` twice does not build
// up a long string).
//
// This blocks "path traversal out of the root", not "permissions inside the container"; the
// latter is handled by the container's own file permissions, and we can only see what the
// agent can see anyway.
func cleanSandboxPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		// If not given, start from the root. Use a default instead of an error: when the
		// frontend first opens the panel it has no path to give.
		return "/", nil
	}
	if !strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("%w: 路径必须以 / 开头", ErrInvalidArgument)
	}
	// path.Clean folds away ".." and extra "/". A path starting with / **cannot** go above the
	// root after Clean ("/.." → "/"), so this step also guards against escaping.
	clean := gopath.Clean(path)
	if !strings.HasPrefix(clean, "/") {
		// In theory we never get here (the code above ensures it starts with /). It stays
		// because this is a security boundary: if Clean's behavior ever changes, we would
		// rather error than let an out-of-bounds path through.
		return "", fmt.Errorf("%w: 路径不合法", ErrInvalidArgument)
	}
	return clean, nil
}

// ---------------------------------------------------------------------------
// webshell
// ---------------------------------------------------------------------------

// shellCommandTimeout is the hard timeout for one command.
//
// When time is up the command is killed, not just "HTTP returns first while the command keeps
// running in the container". The latter would mean a user who types an infinite loop can
// never get it back, and that process would keep using the container's CPU until the TTL.
//
// 60 seconds: shorter than the bash tool's 120 seconds, because a person is waiting at the
// terminal, and no output for over a minute almost always means a typo. Truly long tasks
// should use `nohup` or the agent.
const shellCommandTimeout = 60 * time.Second

// RunSandboxShell runs one webshell command in the container.
//
// # Permissions
//
// This route is **ActionManage** (owner/admin) in authz. The reason is not a vague "running
// commands is dangerous", but three concrete points:
//
//  1. **It shares the same file system as the agent.** The container holds everything the
//     agent produced this time: cloned code, intermediate files, generated data. A shell can
//     read and change all of it; that is full read/write access to workspace data, not
//     "taking a look".
//  2. **It can read what the agent left on disk.** Credentials are not always only in
//     environment variables: the agent may have written a token into a file, or some CLI may
//     have left a login cache, and `cat` in the shell can read it.
//  3. **Changing the container changes the agent's execution environment**: install a
//     package, change a file, and the agent uses it in the next round.
//
// The injected session env (AMA_TOKEN etc.) goes through the exec env **of each tool call**,
// so it is not in the webshell's environment. The scrubbing layer is still kept (see
// scrubShellOutput): in case the agent wrote a token into a file, it must not flow out
// through the output as is.
//
// # Why "one command per request" instead of WebSocket
//
// Each command is a separate docker exec, and cwd is stored in the container (per shellID),
// so "cd and stay there, like a terminal" needs no long-lived connection. **The shell state
// is in the container, not in any amad process**, so two adjacent commands landing on
// different instances is fine. For truly interactive use (vim, top) use the terminal.
//
// # Audit
//
// Each command logs one Info line: who, which run, which container, what was run.
// **The command text must be logged**: if something goes wrong we must be able to answer "who
// did what in this container that day".
func (s *Service) RunSandboxShell(ctx context.Context, runID, command string) (SandboxShellResult, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return SandboxShellResult{}, fmt.Errorf("%w: 命令不能为空", ErrInvalidArgument)
	}

	state, err := s.RunSandboxState(ctx, runID)
	if err != nil {
		return SandboxShellResult{}, err
	}
	// When the container is not running it is **not resumed automatically**, same check as
	// file listing: resuming takes over ten seconds and takes global quota again, so let the
	// user click it. Return the state so the frontend shows guidance.
	if state.Status != SandboxStatusRunning {
		return SandboxShellResult{Status: state.Status}, nil
	}

	rec, err := s.store.GetRunSandbox(ctx, runID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return SandboxShellResult{Status: SandboxStatusNone}, nil
		}
		return SandboxShellResult{}, err
	}

	sess, err := s.attachSandbox(ctx, runID, rec.SandboxID)
	if err != nil {
		if isSandboxGone(err) {
			return SandboxShellResult{Status: SandboxStatusGone}, nil
		}
		return SandboxShellResult{}, fmt.Errorf("接入容器失败（container=%s）: %w", rec.SessionID, err)
	}

	// Audit. **Logged before running**: if the command breaks the container, the log line is
	// still there afterwards.
	s.log().Info("webshell 执行命令",
		slog.String("actor", auth.ActorFrom(ctx)),
		slog.String("run", runID),
		slog.String("session", rec.SessionID),
		slog.String("command", command))

	res, err := sess.Shell(ctx, shellIDFor(runID), command, shellCommandTimeout)
	if err != nil {
		if isSandboxGone(err) {
			return SandboxShellResult{Status: SandboxStatusGone}, nil
		}
		return SandboxShellResult{}, fmt.Errorf("执行命令失败（session=%s）: %w", rec.SessionID, err)
	}

	// ⚠️ **The output must have injected credentials scrubbed.**
	//
	// The shelltools/scrub.go layer guards the **tool call** exit; the webshell is **another**
	// exit that does not go through container.Exec, so that layer has no effect at all.
	// Without scrubbing, one `env` from the user would show the owner's JWT in the browser
	// (and put it in browser memory and possibly logs).
	out := s.scrubShellOutput(ctx, runID, res.Output)

	return SandboxShellResult{
		Status:      SandboxStatusRunning,
		Output:      out,
		ExitCode:    res.ExitCode,
		ShellStatus: res.Status,
	}, nil
}

// shellIDFor gives a Run a stable shell session id.
//
// **It must be stable** (not random each time): the container tracks cwd by this id, and a
// new id is like opening a new terminal, so after `cd` the next command is back in the
// working directory.
func shellIDFor(runID string) string { return "ama-webshell-" + runID }

// scrubShellOutput removes credentials injected into the container from the output.
//
// Credentials are fetched the same way as at injection time (the same sources), so **what is
// scrubbed is exactly what was really injected**. Guessing by a fixed pattern (for example
// "strings that look like a JWT") would miss some and also damage the user's own output.
//
// # ⚠️ This must be maintained **separately** from the shelltools layer
//
// shelltools' scrub sits on enrichingExecer, the choke point shared by **the six tools**, and
// the webshell does not go through there. So every new kind of injected credential must be
// added in both places:
//
//	shared/infra/shelltools/injector.go  prepareWithBase()  → output of the six tools
//	core/sandbox.go                      scrubShellOutput() → output of the web terminal (this function)
func (s *Service) scrubShellOutput(ctx context.Context, runID, out string) string {
	return s.ScrubHostOutput(ctx, runID, out)
}

// ScrubHostOutput is the exported version of scrubShellOutput, used to scrub host exec results
// before the host domain stores them (cmd/amad wires it as hostsvc.ExecResultScrubber). It
// uses the same secret list as the webshell and the six tools; missing one place means a
// plaintext secret ends up in the database or on screen.
func (s *Service) ScrubHostOutput(ctx context.Context, runID, out string) string {
	if out == "" || s.shellEnvs == nil {
		return out
	}
	envs, secretKeys, aliases, err := s.shellEnvs.SessionEnvs(ctx, runID)
	if err != nil && len(envs) == 0 {
		return out
	}
	// The ones marked secret in the session env (AMA_TOKEN + custom env the user marked
	// sensitive).
	//
	// **Only scrub the ones secretKeys reports**, not the normal env the user filled in: that
	// is config they wrote themselves, and scrubbing it would make the command output hard to
	// understand (same rule as on the shelltools side).
	var secrets []string
	for _, k := range secretKeys {
		if v := strings.TrimSpace(envs[k]); v != "" {
			secrets = append(secrets, v)
		}
	}
	// Credential aliases: the value is exactly the same as the source, so scrubbing by value
	// covers them automatically as long as the source is in secrets. Add them explicitly
	// here too, so aliases are not missed when the source is not in secretKeys.
	for _, a := range aliases {
		if v := strings.TrimSpace(envs[strings.TrimSpace(a.From)]); v != "" {
			secrets = append(secrets, v)
		}
	}
	for _, v := range secrets {
		out = strings.ReplaceAll(out, v, "[已隐藏的凭据]")
	}
	return out
}

// SetShellEnvScrubber sets up "which session env values the webshell output must also scrub".
//
// Missing this line means `env` in the web terminal shows AMA_TOKEN in plain text, with zero
// errors.
func (s *Service) SetShellEnvScrubber(src shelltools.SessionEnvSource) {
	s.shellEnvs = src
}

// ---------------------------------------------------------------------------
// File details (read-only)
// ---------------------------------------------------------------------------

// sandboxFileMaxBytes is the size limit for one file viewed on the page.
//
// Beyond it the file is truncated and marked. 256 KiB: enough for the tail of a log and most
// source files, while anything larger only freezes the page in the browser. Users who really
// want the whole file should use `less` in the terminal.
const sandboxFileMaxBytes = 256 * 1024

// ReadRunSandboxFile reads a file in the container for preview.
//
// **Read-only**, and text only. Binaries get no content (returns Binary=true): putting random
// bytes into JSON and rendering them on the page is pointless and may mess up the
// terminal/page.
func (s *Service) ReadRunSandboxFile(ctx context.Context, runID, path string) (SandboxFileContent, error) {
	clean, err := cleanSandboxPath(path)
	if err != nil {
		return SandboxFileContent{}, err
	}
	if clean == "/" {
		return SandboxFileContent{}, fmt.Errorf("%w: 要给一个文件路径", ErrInvalidArgument)
	}

	rec, err := s.store.GetRunSandbox(ctx, runID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return SandboxFileContent{}, ErrNotFound
		}
		return SandboxFileContent{}, err
	}
	sess, err := s.attachSandbox(ctx, runID, rec.SandboxID)
	if err != nil {
		if isSandboxGone(err) {
			return SandboxFileContent{}, ErrNotFound
		}
		return SandboxFileContent{}, fmt.Errorf("接入容器失败（container=%s）: %w", rec.SessionID, err)
	}

	rc, err := sess.Download(ctx, clean, sandboxFileMaxBytes+1)
	if err != nil {
		if isSandboxGone(err) {
			return SandboxFileContent{}, ErrNotFound
		}
		return SandboxFileContent{}, fmt.Errorf("读文件失败（path=%s）: %w", clean, err)
	}
	defer func() { _ = rc.Close() }()

	// Read one extra byte to tell "was it truncated": reading exactly up to the limit cannot
	// tell "exactly this big" from "there is more", and one extra byte settles it.
	buf := make([]byte, sandboxFileMaxBytes+1)
	n, err := io.ReadFull(rc, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return SandboxFileContent{}, fmt.Errorf("读文件失败（path=%s）: %w", clean, err)
	}
	data := buf[:n]

	out := SandboxFileContent{Path: clean, Size: int64(n)}
	if n > sandboxFileMaxBytes {
		data = data[:sandboxFileMaxBytes]
		out.Size = sandboxFileMaxBytes
		out.Truncated = true
	}
	if isBinary(data) {
		out.Binary = true
		return out, nil
	}
	out.Content = string(data)
	return out, nil
}

// isBinary roughly judges whether some bytes are binary.
//
// The check is **contains a NUL byte**, the same heuristic git and grep use. It is simple and
// almost never wrong: valid UTF-8 text never contains 0x00, while the header of almost every
// binary format does.
//
// More complex detection (for example the share of non-printable characters) is avoided on
// purpose: such rules would misjudge logs with lots of control characters as binary, and
// those are among the things users most want to see.
func isBinary(data []byte) bool {
	for _, b := range data {
		if b == 0 {
			return true
		}
	}
	return false
}

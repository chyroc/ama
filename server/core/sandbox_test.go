// Tests for the session container endpoints.
//
// The four things this file pins down (the first three are all the "wrong but no error" kind):
//
//  1. **Four-state classification**: in particular "paused" must not be shown as "reclaimed"
//     (the workspace is still there vs already gone), and "never created a container" must not
//     be shown as "reclaimed" (that is the normal state of a text-only conversation);
//  2. **The container can still be found with multiple instances**: this is the only reason the
//     whole move-to-DB change exists; not testing it is the same as not doing it;
//  3. **Path normalization blocks escaping the root**;
//  4. Truncation must be labeled honestly.
//
// Access across workspaces (people in workspace A cannot get workspace B's container) is not
// tested here but in authz_test.go: that is the table-driven job of the authorization layer,
// and writing it at the Service layer would go against the rule "authorization does not move
// down".
package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/chyroc/ama/server/repository/dao/coremem"

	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/shared/infra/shelltools"
)

// notFound builds a "container and workspace are both gone" error, to simulate "the container
// has been reclaimed".
//
// **It must wrap ErrSandboxGone, not just any error**: the gone check uses
// errors.Is(err, ErrSandboxGone). With a plain error the test would slide past the "gone" branch
// and then pass on the 500 branch, which is exactly the mistake to guard against. The extra
// wrapping is deliberate too: implementations usually fmt.Errorf("...: %w") and throw it upward,
// so the check must see through that.
func notFound() error {
	return fmt.Errorf("inspect 容器: %w", ErrSandboxGone)
}

// fakeSandboxSession is a programmable container.
type fakeSandboxSession struct {
	info       *SandboxInfo
	infoErr    error
	resumeErr  error
	entries    []SandboxFSEntry
	listErr    error
	listedPath string
	resumed    int
	shellOut   string
	shellErr   error
	shellID    string
	ranCommand string
}

func (f *fakeSandboxSession) Info(context.Context) (*SandboxInfo, error) {
	return f.info, f.infoErr
}
func (f *fakeSandboxSession) Resume(context.Context) error {
	f.resumed++
	return f.resumeErr
}
func (f *fakeSandboxSession) List(_ context.Context, path string, _ int) ([]SandboxFSEntry, error) {
	f.listedPath = path
	return f.entries, f.listErr
}
func (f *fakeSandboxSession) Shell(
	_ context.Context, shellID, command string, _ time.Duration,
) (*SandboxShellOutput, error) {
	f.shellID, f.ranCommand = shellID, command
	if f.shellErr != nil {
		return nil, f.shellErr
	}
	code := 0
	return &SandboxShellOutput{Output: f.shellOut, ExitCode: &code, Status: "completed"}, nil
}
func (f *fakeSandboxSession) Download(context.Context, string, int64) (io.ReadCloser, error) {
	return nil, errors.New("fake: 不支持下载")
}
func (f *fakeSandboxSession) OpenTerminal(context.Context) (TerminalConn, error) {
	return nil, errors.New("fake: 不支持终端")
}

// fakeAttacher hands out containers by run id; anything it does not recognize is treated as
// reclaimed.
//
// attached records the sandboxID given on each attach (what the DB records as "which daemon the
// container is on"), to pin down "it really attached using that DB record".
type fakeAttacher struct {
	sessions  map[string]*fakeSandboxSession
	attachErr error
	attached  []string
}

func (a *fakeAttacher) AttachSandbox(_ context.Context, runID, sandboxID string) (SandboxSession, error) {
	a.attached = append(a.attached, sandboxID)
	if a.attachErr != nil {
		return nil, a.attachErr
	}
	if s, ok := a.sessions[runID]; ok {
		return s, nil
	}
	return nil, notFound()
}

// newSandboxTestService builds a Service with the fake attacher wired in.
func newSandboxTestService(t *testing.T, a SandboxAttacher) (*Service, Store) {
	t.Helper()
	store := coremem.NewMemStore()
	svc, err := New(Config{Store: store})
	if err != nil {
		t.Fatalf("建 Service 失败: %v", err)
	}
	svc.SetSandboxAttacher(a)
	return svc, store
}

// runningInfo / pausedInfo build "running" and "paused" daemon responses.
func runningInfo() *SandboxInfo {
	return &SandboxInfo{Status: sandboxInfoRunning, StartedAt: "2026-08-29T09:00:00Z"}
}
func pausedInfo() *SandboxInfo {
	return &SandboxInfo{Status: sandboxInfoPaused, StartedAt: "2026-08-29T09:00:00Z"}
}

// TestSandboxFourStates pins down the four-state classification.
//
// **Read these four together**: the difference between any two is "saying the wrong one makes
// the user do the wrong thing". Calling paused gone makes the user think the workspace is lost
// and start over; calling none gone makes the user ask "where are my files" when there never
// were any files.
func TestSandboxFourStates(t *testing.T) {
	const runID = "run-1"

	t.Run("none：从来没建过容器", func(t *testing.T) {
		// No record at all in the DB = text-only conversation; containers are created lazily,
		// and this one was never created.
		svc, _ := newSandboxTestService(t, &fakeAttacher{})
		got, err := svc.RunSandboxState(context.Background(), runID)
		if err != nil {
			t.Fatalf("查状态失败: %v", err)
		}
		if got.Status != SandboxStatusNone {
			t.Errorf("status = %q，期望 %q（没记录 = 没建过，不是已回收）",
				got.Status, SandboxStatusNone)
		}
		// **Do not just return gone and let the frontend guess**: the two have completely
		// different text in the UI.
		if got.Status == SandboxStatusGone {
			t.Error("把「没建过」显示成「已回收」，用户会以为工作区被清了")
		}
	})

	t.Run("running：活着且没暂停", func(t *testing.T) {
		sess := &fakeSandboxSession{info: runningInfo()}
		svc, store := newSandboxTestService(t,
			&fakeAttacher{sessions: map[string]*fakeSandboxSession{runID: sess}})
		seedSandbox(t, store, runID, "ama-1")

		got, err := svc.RunSandboxState(context.Background(), runID)
		if err != nil {
			t.Fatalf("查状态失败: %v", err)
		}
		if got.Status != SandboxStatusRunning {
			t.Errorf("status = %q，期望 %q", got.Status, SandboxStatusRunning)
		}
		if got.SessionID != "ama-1" {
			t.Errorf("sessionId = %q，期望 ama-1", got.SessionID)
		}
		if got.CanResume {
			t.Error("running 时不该给恢复按钮")
		}
	})

	for _, st := range []string{sandboxInfoPaused, sandboxInfoStopped} {
		t.Run(st+"：工作区还在，显示成 paused 且能恢复", func(t *testing.T) {
			// stopped includes "the container was deleted but the workspace volume is still
			// there", which to the user is the same as paused.
			sess := &fakeSandboxSession{info: &SandboxInfo{Status: st}}
			svc, store := newSandboxTestService(t,
				&fakeAttacher{sessions: map[string]*fakeSandboxSession{runID: sess}})
			seedSandbox(t, store, runID, "ama-2")

			got, err := svc.RunSandboxState(context.Background(), runID)
			if err != nil {
				t.Fatalf("查状态失败: %v", err)
			}
			if got.Status != SandboxStatusPaused {
				t.Errorf("status = %q，期望 %q", got.Status, SandboxStatusPaused)
			}
			// ⚠️ This one is the point of the group: paused ≠ gone.
			if got.Status == SandboxStatusGone {
				t.Error("把「已暂停」显示成「已回收」——工作区其实还在，用户会白白重来")
			}
			if !got.CanResume {
				t.Error("canResume 应为 true，否则前端不给恢复按钮")
			}
		})
	}

	t.Run("gone：接入时报不存在就是被回收了", func(t *testing.T) {
		// The attacher has no such run → returns an error wrapping ErrSandboxGone.
		svc, store := newSandboxTestService(t, &fakeAttacher{})
		seedSandbox(t, store, runID, "ama-missing")

		got, err := svc.RunSandboxState(context.Background(), runID)
		if err != nil {
			t.Fatalf("查状态失败: %v", err)
		}
		if got.Status != SandboxStatusGone {
			t.Errorf("status = %q，期望 %q", got.Status, SandboxStatusGone)
		}
	})

	t.Run("gone：接上了但 Info 报不存在", func(t *testing.T) {
		// The container was deleted by the reclaim sweep between attaching and checking: still
		// gone, not 500.
		sess := &fakeSandboxSession{infoErr: notFound()}
		svc, store := newSandboxTestService(t,
			&fakeAttacher{sessions: map[string]*fakeSandboxSession{runID: sess}})
		seedSandbox(t, store, runID, "ama-3")

		got, err := svc.RunSandboxState(context.Background(), runID)
		if err != nil {
			t.Fatalf("查状态失败: %v", err)
		}
		if got.Status != SandboxStatusGone {
			t.Errorf("status = %q，期望 %q", got.Status, SandboxStatusGone)
		}
	})
}

// TestFillSandboxState is table-driven over the pure function: daemon status → four states.
func TestFillSandboxState(t *testing.T) {
	cases := []struct {
		info      *SandboxInfo
		want      string
		canResume bool
	}{
		{nil, SandboxStatusGone, false}, // with no information at all we can only say gone
		{&SandboxInfo{Status: sandboxInfoRunning}, SandboxStatusRunning, false},
		{&SandboxInfo{Status: sandboxInfoPaused}, SandboxStatusPaused, true},
		{&SandboxInfo{Status: sandboxInfoStopped}, SandboxStatusPaused, true},
		{&SandboxInfo{Status: "weird"}, SandboxStatusGone, false},
	}
	for _, c := range cases {
		got := fillSandboxState(SandboxState{SessionID: "ama-1"}, c.info)
		if got.Status != c.want || got.CanResume != c.canResume {
			t.Errorf("fillSandboxState(%+v) = %q/canResume=%v，期望 %q/%v",
				c.info, got.Status, got.CanResume, c.want, c.canResume)
		}
		if got.SessionID != "ama-1" {
			t.Errorf("base 字段被丢了：%+v", got)
		}
	}
}

// TestSandboxStateNonNotFoundErrorIsNotGone pins down "cannot attach ≠ necessarily gone".
//
// Network hiccups, daemon failures, or the container being on another daemon all make Attach
// fail. Saying "reclaimed" then is a lie: the user would think the workspace is lost while the
// container is actually alive and well. The error must bubble up.
func TestSandboxStateNonNotFoundErrorIsNotGone(t *testing.T) {
	svc, store := newSandboxTestService(t,
		&fakeAttacher{attachErr: errors.New("网络不通")})
	seedSandbox(t, store, "run-1", "ama-1")

	got, err := svc.RunSandboxState(context.Background(), "run-1")
	if err == nil {
		t.Fatalf("网络错误应该往上抛，实际回了 status=%q", got.Status)
	}
	if got.Status == SandboxStatusGone {
		t.Error("把网络故障说成「容器已回收」，用户会以为工作区丢了")
	}
}

// TestSandboxStateSurvivesEmptyProcessMap is **the only reason storing this in the DB exists**.
//
// Production amad runs multiple instances: the pod a user's request lands on is usually not the
// pod that created the container, and that pod has nothing in process. This simulates exactly
// that kind of pod: a brand-new Service with nothing in process, relying only on the container
// record in the DB + AttachSandbox.
//
// Without this test the DB step might as well not exist, and the failure only shows up
// sporadically in production (a single local process is always right).
func TestSandboxStateSurvivesEmptyProcessMap(t *testing.T) {
	store := coremem.NewMemStore()
	ctx := context.Background()

	// —— pod A: created the container and recorded it ——
	podA, err := New(Config{Store: store})
	if err != nil {
		t.Fatalf("建 pod A 失败: %v", err)
	}
	podA.RecordSession(ctx, "run-x", "ama-shared", "daemon-1")

	// —— pod B: another process that shares nothing except this DB ——
	sess := &fakeSandboxSession{info: runningInfo()}
	podB, err := New(Config{Store: store})
	if err != nil {
		t.Fatalf("建 pod B 失败: %v", err)
	}
	attacher := &fakeAttacher{sessions: map[string]*fakeSandboxSession{"run-x": sess}}
	podB.SetSandboxAttacher(attacher)

	got, err := podB.RunSandboxState(ctx, "run-x")
	if err != nil {
		t.Fatalf("另一个实例查状态失败: %v", err)
	}
	if got.Status != SandboxStatusRunning {
		t.Errorf("status = %q，期望 %q——另一个实例必须也能查到容器",
			got.Status, SandboxStatusRunning)
	}
	if got.SessionID != "ama-shared" {
		t.Errorf("sessionId = %q，期望 ama-shared", got.SessionID)
	}
	// Pin down "it really attached back via the daemon recorded in the DB", not by luck from
	// somewhere else.
	if len(attacher.attached) != 1 || attacher.attached[0] != "daemon-1" {
		t.Errorf("attach 记录 = %v，期望恰好按 daemon-1 接了一次", attacher.attached)
	}
}

// TestRecordSessionOverwritesOnRebuild pins down "a rebuild overwrites, not inserts".
//
// After the reclaim sweep deletes a container it gets rebuilt, and then the same Run has a new
// container. With insert semantics the table keeps the **old container id**, and that container
// no longer exists, so the page always shows "reclaimed", nobody can see the rebuilt container,
// and there is no error at all.
func TestRecordSessionOverwritesOnRebuild(t *testing.T) {
	store := coremem.NewMemStore()
	svc, err := New(Config{Store: store})
	if err != nil {
		t.Fatalf("建 Service 失败: %v", err)
	}
	ctx := context.Background()

	svc.RecordSession(ctx, "run-1", "ama-old", "daemon-1")
	svc.RecordSession(ctx, "run-1", "ama-new", "daemon-1") // rebuilt after the container was reclaimed

	rec, err := store.GetRunSandbox(ctx, "run-1")
	if err != nil {
		t.Fatalf("取记录失败: %v", err)
	}
	if rec.SessionID != "ama-new" {
		t.Errorf("sessionId = %q，期望 ama-new——重建必须覆盖，否则永远指向已消失的旧容器",
			rec.SessionID)
	}
}

// TestTerminalKeepAliveRoundTrip pins down the read/write loop of the "terminal in use" ledger.
//
// # Why this one is pinned separately
//
// The two ends of this feature are in **different processes** (written on the instance that
// receives key presses, read on the instance running the worker), so "can what was written be
// read back" is its only seam. If the seam breaks, the symptom is: the container still gets
// paused while the user is typing commands, **exactly the same** as if the feature was never
// built, with zero errors.
func TestTerminalKeepAliveRoundTrip(t *testing.T) {
	store := coremem.NewMemStore()
	svc, err := New(Config{Store: store})
	if err != nil {
		t.Fatalf("建 Service 失败: %v", err)
	}
	ctx := context.Background()

	// There must be a container record first: no container, nothing to protect.
	svc.RecordSession(ctx, "run-1", "ama-1", "daemon-1")

	// Nobody has opened a terminal yet: the set must be empty.
	//
	// ⚠️ **This step must not be skipped.** Without asserting empty first, an implementation that
	// "unconditionally returns all containers" would also pass the assertion below, and that would
	// keep every container from ever being paused.
	before, err := svc.ActiveTerminalSessions(ctx)
	if err != nil {
		t.Fatalf("查在用终端失败: %v", err)
	}
	if len(before) != 0 {
		t.Fatalf("没人开终端时集合应为空，实际 %v", before)
	}

	svc.touchTerminal(ctx, "run-1")

	after, err := svc.ActiveTerminalSessions(ctx)
	if err != nil {
		t.Fatalf("查在用终端失败: %v", err)
	}
	// It returns **container names**, not run ids: the reading side (the idle-cleanup check on the
	// dockertools side) only has containers and does not know which Run they belong to. Give it
	// the wrong thing and nothing matches, and that bug also shows up as "the feature silently
	// does nothing".
	if len(after) != 1 || after[0] != "ama-1" {
		t.Fatalf("续期后应能查到 [ama-1]，实际 %v —— 返回的必须是容器名而不是 run id", after)
	}
}

// TestTerminalKeepAliveWithoutContainerIsNoop pins down "with no container, do not make up a
// record".
//
// Text-only conversations do not create containers, while the key-press path **always renews**
// (on purpose: it landing on an instance that does not have this terminal is the normal case).
// If it inserted a row along the way, the table would get an incomplete row with an empty
// session_id, and GetRunSandbox would return it as "has a container", so the page would show a
// container that does not exist.
func TestTerminalKeepAliveWithoutContainerIsNoop(t *testing.T) {
	store := coremem.NewMemStore()
	svc, err := New(Config{Store: store})
	if err != nil {
		t.Fatalf("建 Service 失败: %v", err)
	}
	ctx := context.Background()

	svc.touchTerminal(ctx, "run-never-had-a-container")

	if _, err := store.GetRunSandbox(ctx, "run-never-had-a-container"); !errors.Is(err, ErrNotFound) {
		t.Errorf("续期不该凭空造出容器记录，GetRunSandbox err = %v，期望 ErrNotFound", err)
	}
	active, err := svc.ActiveTerminalSessions(ctx)
	if err != nil {
		t.Fatalf("查在用终端失败: %v", err)
	}
	if len(active) != 0 {
		t.Errorf("没有容器时不该有在用终端，实际 %v", active)
	}

	// ⚠️ **Only this part really tests "do not record out of thin air".**
	//
	// The two assertions above can be satisfied by the filtering on the read side alone (reads
	// skip rows without a container record), so with only them, the wrong implementation
	// "unconditionally record on renew" still passes; we measured this.
	//
	// The real difference shows up at the moment **this Run later creates a container**: that
	// stale activity record suddenly "comes back to life" and marks a container nobody is
	// watching as in use, so it escapes pausing and wastes quota until TTL. The PG side does not
	// have this problem (the UPDATE affects 0 rows, nothing is written); the memory
	// implementation must behave the same.
	svc.RecordSession(ctx, "run-never-had-a-container", "ama-late", "daemon-1")
	revived, err := svc.ActiveTerminalSessions(ctx)
	if err != nil {
		t.Fatalf("查在用终端失败: %v", err)
	}
	if len(revived) != 0 {
		t.Errorf("建容器之后那笔陈旧的活动记录不该复活，实际 %v —— "+
			"续期时必须先确认容器存在，否则会把没人用的容器判成在用", revived)
	}
}

// TestListSandboxFiles pins down several behaviors of the file listing.
func TestListSandboxFiles(t *testing.T) {
	ctx := context.Background()

	t.Run("正常列出，且用执行端给的 Type 判类型", func(t *testing.T) {
		sess := &fakeSandboxSession{
			info: runningInfo(),
			entries: []SandboxFSEntry{
				{Name: "app.go", Path: "/w/app.go", Type: "file", Size: 12, Permissions: "rw-r--r--"},
				{Name: "sub", Path: "/w/sub", Type: "directory"},
			},
		}
		svc, store := newSandboxTestService(t,
			&fakeAttacher{sessions: map[string]*fakeSandboxSession{"run-1": sess}})
		seedSandbox(t, store, "run-1", "ama-1")

		got, err := svc.ListRunSandboxFiles(ctx, "run-1", "/w")
		if err != nil {
			t.Fatalf("列目录失败: %v", err)
		}
		if got.Status != SandboxStatusRunning || len(got.Files) != 2 {
			t.Fatalf("status=%q files=%d，期望 running / 2 条", got.Status, len(got.Files))
		}
		if got.Files[0].Type != "file" || got.Files[1].Type != "directory" {
			t.Errorf("类型取值错了：%q / %q", got.Files[0].Type, got.Files[1].Type)
		}
		if got.Truncated {
			t.Error("只有两条不该被标成截断")
		}
	})

	t.Run("暂停时回状态 + 空列表，不报错也不自动恢复", func(t *testing.T) {
		sess := &fakeSandboxSession{info: pausedInfo()}
		svc, store := newSandboxTestService(t,
			&fakeAttacher{sessions: map[string]*fakeSandboxSession{"run-1": sess}})
		seedSandbox(t, store, "run-1", "ama-2")

		got, err := svc.ListRunSandboxFiles(ctx, "run-1", "/")
		if err != nil {
			t.Fatalf("暂停时列目录不该报错: %v", err)
		}
		if got.Status != SandboxStatusPaused {
			t.Errorf("status = %q，期望 paused", got.Status)
		}
		// ⚠️ A read-only operation must never resume the container on the side: that takes over
		// ten seconds and takes up global concurrency quota again, while the user only wants a
		// quick look.
		if sess.resumed != 0 {
			t.Errorf("列目录恢复了 %d 次容器——只读操作不该有这种副作用", sess.resumed)
		}
		// An empty list must be [], not nil: the frontend calls .map() directly.
		if got.Files == nil {
			t.Error("Files 为 nil，序列化成 null 会让前端 .map() 抛异常")
		}
	})

	t.Run("超过上限要截断并如实标注", func(t *testing.T) {
		many := make([]SandboxFSEntry, sandboxListMaxEntries+10)
		for i := range many {
			many[i] = SandboxFSEntry{Name: "f", Type: "file"}
		}
		got := buildSandboxListing(SandboxStatusRunning, "/", many)
		if len(got.Files) != sandboxListMaxEntries {
			t.Errorf("回了 %d 条，期望截到 %d", len(got.Files), sandboxListMaxEntries)
		}
		if !got.Truncated {
			t.Error("截断了却没标 Truncated——用户会以为目录里就这些")
		}
	})
}

// TestCleanSandboxPath pins down path normalization.
//
// path is a query parameter given directly by the user; without normalization `..` would be
// sent into the container as is.
func TestCleanSandboxPath(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "/", false},                            // none given, start from the root
		{"/w/app", "/w/app", false},                 //
		{"/w//sub/", "/w/sub", false},               // extra slashes collapsed
		{"/w/sub/..", "/w", false},                  // .. resolved in place
		{"/../../etc/passwd", "/etc/passwd", false}, // cannot escape the root
		{"/..", "/", false},                         // same as above
		{"relative/path", "", true},                 // relative paths rejected
	}
	for _, c := range cases {
		got, err := cleanSandboxPath(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("cleanSandboxPath(%q) 应该报错，实际得到 %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("cleanSandboxPath(%q) 报错: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("cleanSandboxPath(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// TestResumeOnRunningContainerIsNotGone pins down "Resume says not found ≠ reclaimed".
//
// ⚠️ Resume's "not found" only says this attempt did not resume anything; it cannot be returned
// to the frontend as gone directly: if the user double-clicks the resume button, or another
// instance has just resumed it, the second attempt may get this error. The right thing is to
// check the status again: when the container is actually alive it must show running.
func TestResumeOnRunningContainerIsNotGone(t *testing.T) {
	sess := &fakeSandboxSession{
		info:      runningInfo(),
		resumeErr: notFound(), // Resume says not found, but Info says it is running
	}
	svc, store := newSandboxTestService(t,
		&fakeAttacher{sessions: map[string]*fakeSandboxSession{"run-1": sess}})
	seedSandbox(t, store, "run-1", "ama-1")

	got, err := svc.ResumeRunSandbox(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	if got.Status != SandboxStatusRunning {
		t.Errorf("status = %q，期望 running——容器其实好好活着，"+
			"Resume 的「不存在」不能直接当成已回收", got.Status)
	}
}

// seedSandbox puts a container record into the DB.
func seedSandbox(t *testing.T, store Store, runID, sessionID string) {
	t.Helper()
	err := store.PutRunSandbox(context.Background(), persistmodel.RunSandbox{
		RunID:     runID,
		SessionID: sessionID,
		SandboxID: "daemon-test",
		CreatedAt: time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("塞容器记录失败: %v", err)
	}
}

// -- webshell ---------------------------------------------------------------

// fakeSessionEnvs is a session env source that always returns the same AMA_TOKEN.
type fakeSessionEnvs struct{ token string }

func (f fakeSessionEnvs) SessionEnvs(context.Context, string) (
	map[string]string, []string, []shelltools.EnvAlias, error,
) {
	envs := map[string]string{
		shelltools.EnvAMAToken:   f.token,
		shelltools.EnvAMABaseURL: "http://127.0.0.1:5273",
	}
	return envs, []string{shelltools.EnvAMAToken}, nil, nil
}

// TestShellScrubsInjectedCredential is **the most important test** for webshell.
//
// AMA_TOKEN (identity = workspace owner) is injected into the session env. If it ever appears
// in webshell output (the agent wrote it into a file, the user cats it), the plaintext would
// travel back to the browser with the output, and the shelltools/scrub.go layer **cannot block
// this**: it only covers the exit path of the six tools, while webshell takes a different path.
//
// When this spot is mutated (the scrubbing removed), the test must go red.
func TestShellScrubsInjectedCredential(t *testing.T) {
	const token = "amak_super-secret-owner-token"
	sess := &fakeSandboxSession{
		info:     runningInfo(),
		shellOut: "AMA_TOKEN=" + token + "\nAMA_BASE_URL=http://127.0.0.1:5273\nPATH=/usr/bin\n",
	}
	svc, store := newSandboxTestService(t,
		&fakeAttacher{sessions: map[string]*fakeSandboxSession{"run-1": sess}})
	seedSandbox(t, store, "run-1", "ama-1")
	svc.SetShellEnvScrubber(fakeSessionEnvs{token: token})

	got, err := svc.RunSandboxShell(context.Background(), "run-1", "env")
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if strings.Contains(got.Output, token) {
		t.Errorf("输出里带着明文凭据！\n输出=%q\n"+
			"注意 shelltools/scrub.go 挡不住这条——它只覆盖工具调用那条出口", got.Output)
	}
	// Only the credential itself should be scrubbed; the rest of the output must stay as is
	// (do not swallow the whole thing).
	if !strings.Contains(got.Output, "PATH=/usr/bin") {
		t.Errorf("抹得太狠，正常输出也没了：%q", got.Output)
	}
	// Session env not in secretKeys is not scrubbed: that is normal config, and scrubbing it
	// would make the output unreadable for the user.
	if !strings.Contains(got.Output, "AMA_BASE_URL=http://127.0.0.1:5273") {
		t.Errorf("非秘密的会话 env 也被抹了：%q", got.Output)
	}
}

// TestShellUsesStableSessionID pins down "the same Run uses the same shell session".
//
// If shellID changed every time, cwd and environment variables would not be kept: after the
// user runs `cd /workspace`, the next command is back at the root, and then it is not a
// terminal anymore.
func TestShellUsesStableSessionID(t *testing.T) {
	sess := &fakeSandboxSession{info: runningInfo()}
	svc, store := newSandboxTestService(t,
		&fakeAttacher{sessions: map[string]*fakeSandboxSession{"run-1": sess}})
	seedSandbox(t, store, "run-1", "ama-1")
	ctx := context.Background()

	if _, err := svc.RunSandboxShell(ctx, "run-1", "cd /workspace"); err != nil {
		t.Fatalf("第一条命令失败: %v", err)
	}
	first := sess.shellID
	if _, err := svc.RunSandboxShell(ctx, "run-1", "pwd"); err != nil {
		t.Fatalf("第二条命令失败: %v", err)
	}
	if sess.shellID != first || first == "" {
		t.Errorf("两条命令的 shellID 不一致（%q vs %q）——cwd 与环境变量不会保留，"+
			"用户 cd 完下一条又回到根目录", first, sess.shellID)
	}
}

// TestShellOnNonRunningContainer: when the container is not running, return the status, no
// error, no automatic resume.
func TestShellOnNonRunningContainer(t *testing.T) {
	sess := &fakeSandboxSession{info: pausedInfo()}
	svc, store := newSandboxTestService(t,
		&fakeAttacher{sessions: map[string]*fakeSandboxSession{"run-1": sess}})
	seedSandbox(t, store, "run-1", "ama-1")

	got, err := svc.RunSandboxShell(context.Background(), "run-1", "ls")
	if err != nil {
		t.Fatalf("暂停时执行不该报错: %v", err)
	}
	if got.Status != SandboxStatusPaused {
		t.Errorf("status = %q，期望 paused", got.Status)
	}
	if sess.ranCommand != "" {
		t.Errorf("容器暂停着却把命令发下去了：%q", sess.ranCommand)
	}
	if sess.resumed != 0 {
		t.Error("不该自动恢复容器——那要十几秒且占全局配额，得让用户自己点")
	}
}

// TestShellRejectsEmptyCommand rejects an empty command right away, without a network call.
func TestShellRejectsEmptyCommand(t *testing.T) {
	svc, store := newSandboxTestService(t, &fakeAttacher{})
	seedSandbox(t, store, "run-1", "ama-1")
	if _, err := svc.RunSandboxShell(context.Background(), "run-1", "   "); err == nil {
		t.Error("空命令应该被拒")
	}
}

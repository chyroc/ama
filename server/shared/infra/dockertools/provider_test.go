package dockertools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chyroc/ama/server/shared/infra/docker"
	"github.com/chyroc/ama/server/shared/infra/shelltools"
)

type fakeSkills struct{ calls int }

func (f *fakeSkills) SkillsFor(context.Context, string) ([]shelltools.SkillBundle, error) {
	f.calls++
	return []shelltools.SkillBundle{{Name: "demo", Files: []shelltools.SkillFile{{Path: "SKILL.md", Content: []byte("# demo")}}}}, nil
}

func newTestProvider(t *testing.T, cfg Config) (*Provider, *fakeEngine, *fakeLedger, *fakeSkills) {
	t.Helper()
	eng := newFakeEngine()
	p := New(eng, cfg, quietLog())
	if err := p.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	led := newFakeLedger()
	sk := &fakeSkills{}
	p.SetSessionLedger(led, led, led)
	p.SetTerminalActivity(led)
	p.SetSkillLoader(sk)
	return p, eng, led, sk
}

func run(t *testing.T, s *session, cmd string) *shelltools.Result {
	t.Helper()
	res, err := s.Exec(context.Background(), shelltools.ExecSpec{Path: "bash", Args: []string{"-lc", cmd}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	return res
}

// First tool call: create the volume, create the container, start it, record it, inject skills, and
// the names are derived from the session id.
func TestFirstExecCreatesContainerLazily(t *testing.T) {
	p, eng, led, sk := newTestProvider(t, Config{})
	s := p.session("sesn-1")

	if len(eng.containers) != 0 {
		t.Fatal("造句柄时不该建容器（懒建）")
	}
	run(t, s, "echo hi")

	c := eng.containers["ama-sess-sesn-1"]
	if c == nil || c.status != "running" {
		t.Fatalf("容器应当建好并在跑，实际 %+v", c)
	}
	if _, ok := eng.volumes["ama-ws-sesn-1"]; !ok {
		t.Error("工作区 volume 没建")
	}
	m := c.cfg.HostConfig.Mounts
	if len(m) != 1 || m[0].Source != "ama-ws-sesn-1" || m[0].Target != DefaultWorkdir {
		t.Errorf("volume 要挂在工作目录上，实际 %+v", m)
	}
	if c.labels["ama.ark-session"] != "sesn-1" || c.labels["ama.managed"] != "true" {
		t.Errorf("label 不对: %v", c.labels)
	}
	if rec := led.records["sesn-1"]; rec != [2]string{"ama-sess-sesn-1", "daemon-A"} {
		t.Errorf("账本记录 = %v", rec)
	}
	if sk.calls != 1 {
		t.Errorf("新容器要注一次技能，实际 %d 次", sk.calls)
	}

	// The second command does not create again or inject skills again.
	run(t, s, "echo again")
	if eng.creates != 1 || sk.calls != 1 {
		t.Errorf("容器只该建一次（实际 %d）、技能只注一次（实际 %d）", eng.creates, sk.calls)
	}
}

// Two instances create a container for the same session at once: the loser gets 409 and switches to
// reusing the one the other built.
func TestCreateConflictReusesOtherInstancesContainer(t *testing.T) {
	p, eng, led, sk := newTestProvider(t, Config{})
	eng.createConflictOnce = true
	s := p.session("sesn-race")
	run(t, s, "true")

	if eng.creates != 0 {
		t.Errorf("409 之后不该再建一个，实际建了 %d 个", eng.creates)
	}
	if eng.runOn[len(eng.runOn)-1] != "other-1" {
		t.Errorf("命令应该落在对方建好的容器上，实际 %v", eng.runOn)
	}
	// Recording and injecting skills is the job of the instance that created the container.
	if _, ok := led.records["sesn-race"]; ok || sk.calls != 0 {
		t.Errorf("复用别人的容器时不该记账/注技能（records=%v skills=%d）", led.records, sk.calls)
	}
}

// A paused one is unfrozen and a stopped one is started; neither is rebuilt (workspace and process
// state are still there).
func TestEnsureRunningResumesPausedAndStopped(t *testing.T) {
	p, eng, _, sk := newTestProvider(t, Config{})
	s := p.session("sesn-2")
	run(t, s, "true")

	eng.containers["ama-sess-sesn-2"].status = "paused"
	run(t, p.session("sesn-2"), "true") // new handle = the next work item, possibly on another instance
	eng.containers["ama-sess-sesn-2"].status = "exited"
	run(t, p.session("sesn-2"), "true")

	if eng.creates != 1 || sk.calls != 1 {
		t.Errorf("暂停/停止后恢复不该重建（creates=%d skills=%d）", eng.creates, sk.calls)
	}
	joined := strings.Join(eng.ops, ",")
	if !strings.Contains(joined, "unpause ama-sess-sesn-2") || strings.Count(joined, "start ama-sess-sesn-2") < 2 {
		t.Errorf("要先 unpause 再 start，实际 %s", joined)
	}
}

// The container was removed: rebuild it, the volume is still there (same name), skills are injected
// again.
func TestContainerRemovedIsRecreatedWithSameVolume(t *testing.T) {
	p, eng, _, sk := newTestProvider(t, Config{})
	run(t, p.session("sesn-3"), "true")
	delete(eng.containers, "ama-sess-sesn-3")

	run(t, p.session("sesn-3"), "true")
	if eng.creates != 2 || sk.calls != 2 {
		t.Errorf("要重建并重注技能（creates=%d skills=%d）", eng.creates, sk.calls)
	}
	if len(eng.volumes) != 1 {
		t.Errorf("volume 应当沿用同一个，实际 %d 个", len(eng.volumes))
	}
}

// Image missing: create container gets 404 → pull once → create again.
func TestMissingImageIsPulledOnCreate(t *testing.T) {
	p, eng, _, _ := newTestProvider(t, Config{Image: "custom:1"})
	run(t, p.session("sesn-img"), "true")
	if eng.pulls != 1 || eng.creates != 1 {
		t.Errorf("pulls=%d creates=%d", eng.pulls, eng.creates)
	}
}

// Exec must wrap the command with timeout, pass env and cwd, and turn 124 into a clear timeout
// message.
func TestExecWrapsTimeoutAndEnv(t *testing.T) {
	p, eng, _, _ := newTestProvider(t, Config{})
	eng.runFn = func(_ string, opt docker.RunOptions) (*docker.RunResult, error) {
		if opt.Cmd[0] == "timeout" && opt.Cmd[len(opt.Cmd)-1] == "sleep 999" {
			return &docker.RunResult{ExitCode: 124}, nil
		}
		return &docker.RunResult{Stdout: []byte("ok")}, nil
	}
	s := p.session("sesn-4")
	res, err := s.Exec(context.Background(), shelltools.ExecSpec{
		Path: "bash", Args: []string{"-lc", "sleep 999"},
		Timeout: 3 * time.Second, Envs: map[string]string{"B": "2", "A": "1"}, Cwd: "/tmp",
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	last := eng.runs[len(eng.runs)-1]
	if strings.Join(last.Cmd[:4], " ") != "timeout -k 5 3" {
		t.Errorf("命令要用 timeout 包一层，实际 %v", last.Cmd)
	}
	if strings.Join(last.Env, ",") != "A=1,B=2" || last.WorkingDir != "/tmp" {
		t.Errorf("env=%v cwd=%q", last.Env, last.WorkingDir)
	}
	if res.Err == "" || !strings.Contains(res.Err, "已在容器里被终止") {
		t.Errorf("124 要翻成超时说明，实际 Err=%q", res.Err)
	}
}

// During exec the container was just paused by another instance (409): run ensureRunning again and
// retry once, instead of returning an error to the agent.
func TestExecRetriesWhenPausedUnderneath(t *testing.T) {
	p, eng, _, _ := newTestProvider(t, Config{})
	s := p.session("sesn-5")
	run(t, s, "true")
	eng.containers["ama-sess-sesn-5"].status = "paused" // the handle's 2-second short cache still thinks it is running
	res := run(t, s, "true")
	if res.ExitCode == nil || *res.ExitCode != 0 {
		t.Errorf("重试后应当成功，实际 %+v", res)
	}
	if eng.containers["ama-sess-sesn-5"].status != "running" {
		t.Error("应当被 unpause")
	}
}

func TestIdlePausesByDefault(t *testing.T) {
	p, eng, _, _ := newTestProvider(t, Config{})
	s := p.session("sesn-6")
	run(t, s, "true")
	s.idle(context.Background())
	if eng.containers["ama-sess-sesn-6"].status != "paused" {
		t.Errorf("空闲后应当暂停，实际 %s", eng.containers["ama-sess-sesn-6"].status)
	}
}

func TestIdleStopPolicy(t *testing.T) {
	p, eng, _, _ := newTestProvider(t, Config{IdleAction: IdleStop})
	s := p.session("sesn-7")
	run(t, s, "true")
	s.idle(context.Background())
	if eng.containers["ama-sess-sesn-7"].status != "exited" {
		t.Errorf("stop 策略下空闲后应当停止，实际 %s", eng.containers["ama-sess-sesn-7"].status)
	}
}

// This work item never touched the container (text-only reply): idle cleanup does nothing, not even
// inspect.
func TestIdleWithoutUseIsNoop(t *testing.T) {
	p, eng, _, _ := newTestProvider(t, Config{})
	run(t, p.session("sesn-8"), "true")
	before := len(eng.ops)
	p.session("sesn-8").idle(context.Background())
	if len(eng.ops) != before || eng.containers["ama-sess-sesn-8"].status != "running" {
		t.Errorf("没用过容器的 work 不该动它，ops=%v", eng.ops[before:])
	}
}

func TestIdleSkipsWhenTerminalOpen(t *testing.T) {
	p, eng, led, _ := newTestProvider(t, Config{})
	s := p.session("sesn-9")
	run(t, s, "true")
	led.terminals = []string{"ama-sess-sesn-9"}
	s.idle(context.Background())
	if eng.containers["ama-sess-sesn-9"].status != "running" {
		t.Error("网页终端开着时不该暂停")
	}
}

func TestIdleSkipsWhenBackgroundJobRunning(t *testing.T) {
	p, eng, _, _ := newTestProvider(t, Config{})
	eng.runFn = func(_ string, opt docker.RunOptions) (*docker.RunResult, error) {
		if len(opt.Cmd) == 3 && strings.Contains(opt.Cmd[2], shelltools.BGLogDir) {
			return &docker.RunResult{Stdout: []byte("RUNNING\n")}, nil
		}
		return &docker.RunResult{}, nil
	}
	s := p.session("sesn-10")
	run(t, s, "true")
	s.idle(context.Background())
	if eng.containers["ama-sess-sesn-10"].status != "running" {
		t.Error("有后台任务在跑时不该暂停")
	}
}

// The activity ledger is written with throttling: several commands in a row write only once.
func TestActivityTouchIsThrottled(t *testing.T) {
	p, _, led, _ := newTestProvider(t, Config{})
	s := p.session("sesn-11")
	for i := 0; i < 5; i++ {
		run(t, s, "true")
	}
	if led.touches != 1 {
		t.Errorf("30 秒内只该写一次活动时刻，实际 %d 次", led.touches)
	}
}

func TestNameKeyIsDeterministicAndSafe(t *testing.T) {
	if nameKey("sesn-abc_1.2") != "sesn-abc_1.2" {
		t.Error("方舟 id 原样可用")
	}
	weird := "会话/../x"
	if a, b := nameKey(weird), nameKey(weird); a != b || !safeName.MatchString(a) {
		t.Errorf("不安全的 id 要退成确定性的安全名字，实际 %q %q", a, b)
	}
}

// ---------------------------------------------------------------------------
// Attach, used by the page
// ---------------------------------------------------------------------------

func TestAttachStates(t *testing.T) {
	p, eng, _, _ := newTestProvider(t, Config{})
	ctx := context.Background()

	if _, err := p.Attach(ctx, "sesn-none", ""); !IsNotFound(err) {
		t.Errorf("容器与 volume 都不在要报不存在，实际 %v", err)
	}
	if _, err := p.Attach(ctx, "sesn-x", "daemon-B"); !errors.Is(err, ErrOtherDaemon) {
		t.Errorf("建在别的 daemon 上要报 ErrOtherDaemon，实际 %v", err)
	}

	run(t, p.session("sesn-a"), "true")
	a, err := p.Attach(ctx, "sesn-a", "daemon-A")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if st, _ := a.State(ctx); st.Status != StateRunning {
		t.Errorf("state=%+v", st)
	}
	eng.containers["ama-sess-sesn-a"].status = "paused"
	if st, _ := a.State(ctx); st.Status != StatePaused {
		t.Errorf("state=%+v", st)
	}

	// The container was reaped but the volume is still there: shown as "stopped", rebuilt on
	// resume.
	delete(eng.containers, "ama-sess-sesn-a")
	st, err := a.State(ctx)
	if err != nil || st.Status != StateStopped || !st.ContainerMissing {
		t.Fatalf("只剩 volume 时 state=%+v err=%v", st, err)
	}
	if err := a.Resume(ctx); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if c := eng.containers["ama-sess-sesn-a"]; c == nil || c.status != "running" {
		t.Errorf("Resume 应当重建出一个在跑的容器，实际 %+v", c)
	}
}

func TestListParsesFindOutput(t *testing.T) {
	p, eng, _, _ := newTestProvider(t, Config{})
	run(t, p.session("sesn-l"), "true")
	eng.runFn = func(_ string, opt docker.RunOptions) (*docker.RunResult, error) {
		out := "f\t12\t-rw-r--r--\t1700000000.5\t\t/workspace/b.txt\x00" +
			"d\t4096\tdrwxr-xr-x\t1700000000\t\t/workspace/src\x00" +
			"l\t7\tlrwxrwxrwx\t1700000000\t/etc/x\t/workspace/link\x00"
		return &docker.RunResult{Stdout: []byte(out)}, nil
	}
	a, err := p.Attach(context.Background(), "sesn-l", "")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := a.List(context.Background(), "/workspace", 1)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 3 || entries[0].Type != "directory" || entries[0].Name != "src" {
		t.Fatalf("目录要排在前面，实际 %+v", entries)
	}
	if entries[1].Name != "b.txt" || entries[1].Size != 12 || entries[1].ModTime != "2023-11-14T22:13:20.500Z" {
		t.Errorf("文件条目 = %+v", entries[1])
	}
	if entries[2].Type != "symlink" || entries[2].SymlinkTarget != "/etc/x" {
		t.Errorf("软链条目 = %+v", entries[2])
	}

	eng.runFn = func(string, docker.RunOptions) (*docker.RunResult, error) {
		return &docker.RunResult{ExitCode: 44}, nil
	}
	if _, err := a.List(context.Background(), "/nope", 1); !IsNotFound(err) {
		t.Errorf("目录不存在要报不存在，实际 %v", err)
	}
}

// ---------------------------------------------------------------------------
// Reaper
// ---------------------------------------------------------------------------

type fakeLeases struct {
	grant bool
	keys  []string
}

type nopLease struct{}

func (nopLease) Release(context.Context) error { return nil }

func (f *fakeLeases) Acquire(_ context.Context, key string, _ time.Duration) (Lease, bool, error) {
	f.keys = append(f.keys, key)
	if !f.grant {
		return nil, false, nil
	}
	return nopLease{}, true, nil
}

func TestReaperRemovesIdleContainersAndWorkspaces(t *testing.T) {
	p, eng, led, _ := newTestProvider(t, Config{ContainerTTL: time.Hour, WorkspaceTTL: 48 * time.Hour})
	for _, id := range []string{"sesn-fresh", "sesn-stale", "sesn-ancient"} {
		run(t, p.session(id), "true")
	}
	now := time.Now()
	led.activity["sesn-fresh"] = now.Add(-10 * time.Minute)
	led.activity["sesn-stale"] = now.Add(-3 * time.Hour)
	led.activity["sesn-ancient"] = now.Add(-72 * time.Hour)
	// An orphan container not in the ledger: judged by its own creation time.
	eng.containers["ama-sess-orphan"] = &fakeContainer{id: "o1", name: "ama-sess-orphan", status: "exited",
		labels: p.labels("orphan", "session"), created: now.Add(-5 * time.Hour).Unix()}
	// Containers of other services (without our label) must never be touched.
	eng.containers["someone-else"] = &fakeContainer{id: "x1", name: "someone-else", status: "running",
		labels: map[string]string{"app": "db"}, created: now.Add(-1000 * time.Hour).Unix()}

	leases := &fakeLeases{grant: true}
	c, v := p.reapOnce(context.Background(), leases)

	if leases.keys[0] != "ama:docker-reaper:daemon-A" {
		t.Errorf("锁要按 daemon 分，实际 %v", leases.keys)
	}
	if _, ok := eng.containers["ama-sess-sesn-fresh"]; !ok {
		t.Error("刚活动过的容器不该删")
	}
	for _, gone := range []string{"ama-sess-sesn-stale", "ama-sess-sesn-ancient", "ama-sess-orphan"} {
		if _, ok := eng.containers[gone]; ok {
			t.Errorf("%s 闲置超过 containerTTL，应当删掉", gone)
		}
	}
	if _, ok := eng.containers["someone-else"]; !ok {
		t.Error("不是我们建的容器绝不能删")
	}
	if _, ok := eng.volumes["ama-ws-sesn-stale"]; !ok {
		t.Error("闲置 3 小时的工作区还没到 workspaceTTL，volume 要保留")
	}
	if _, ok := eng.volumes["ama-ws-sesn-ancient"]; ok {
		t.Error("闲置 72 小时的工作区超过 workspaceTTL，volume 应当删掉")
	}
	if c != 3 || v != 1 {
		t.Errorf("删了 %d 个容器、%d 个 volume，要 3 与 1", c, v)
	}
}

func TestReaperSkipsWithoutLease(t *testing.T) {
	p, eng, led, _ := newTestProvider(t, Config{ContainerTTL: time.Minute})
	run(t, p.session("sesn-old"), "true")
	led.activity["sesn-old"] = time.Now().Add(-time.Hour)
	if c, _ := p.reapOnce(context.Background(), &fakeLeases{grant: false}); c != 0 {
		t.Error("抢不到锁就不该扫")
	}
	if _, ok := eng.containers["ama-sess-sesn-old"]; !ok {
		t.Error("抢不到锁时什么都不该删")
	}
}

// The ledger cannot be read: better not delete at all than fall back to creation time and wrongly
// delete old sessions still in use.
func TestReaperSkipsWhenLedgerFails(t *testing.T) {
	p, eng, led, _ := newTestProvider(t, Config{ContainerTTL: time.Nanosecond})
	run(t, p.session("sesn-busy"), "true")
	led.actErr = errors.New("db down")
	if c, _ := p.reapOnce(context.Background(), nil); c != 0 {
		t.Error("账本出错时不该删任何东西")
	}
	if _, ok := eng.containers["ama-sess-sesn-busy"]; !ok {
		t.Error("容器被误删了")
	}
}

func TestToolsForRunsSixToolsInContainer(t *testing.T) {
	p, eng, _, _ := newTestProvider(t, Config{})
	set, err := p.ToolsFor(context.Background(), "sesn-tools", t.TempDir())
	if err != nil {
		t.Fatalf("ToolsFor: %v", err)
	}
	defer func() { _ = set.Close() }()
	for _, name := range []string{"bash", "read", "write", "edit", "glob", "grep"} {
		if !set.Has(name) {
			t.Errorf("工具集里缺 %s", name)
		}
	}
	if len(eng.containers) != 0 {
		t.Error("建工具集时不该建容器")
	}
}

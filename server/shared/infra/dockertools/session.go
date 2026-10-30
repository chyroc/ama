package dockertools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chyroc/ama/server/shared/infra/docker"
	"github.com/chyroc/ama/server/shared/infra/shelltools"
)

// session is the container handle for one Ark session. It satisfies shelltools.Execer.
//
// It is the executor the six tools get: whichever tool is called, it first runs ensureRunning
// (create if missing, resume if paused, start if stopped, rebuild if removed), so each tool does
// not have to do it, and none can be missed.
//
// **It does not remember container state** (apart from a 2-second "just confirmed running" short
// cache): pause/resume/remove may happen on other instances, so state is always looked up fresh
// with inspect.
type session struct {
	p            *Provider
	arkSessionID string
	name         string
	volume       string
	log          *slog.Logger

	mu        sync.Mutex
	id        string    // the container id confirmed most recently
	checkedAt time.Time // the last time the container was confirmed running
	used      bool      // whether this work item really touched the container (if not, idle cleanup does nothing)
	touchedAt time.Time // the last time the activity ledger was written (for throttling)
}

// runningCacheFor is how long "just confirmed the container is running" stays valid.
//
// In one turn the agent often calls several tools in a row, and inspecting every time is a wasted
// round trip. 2 seconds is short enough that the chance of another instance pausing it in between
// is negligible (pausing only happens after a turn has been idle for 60 seconds), and if it really
// happens, exec returns 409 and Exec below retries once.
const runningCacheFor = 2 * time.Second

// touchEvery is the minimum interval between activity ledger writes. Reaping works at the scale of
// hours, so there is no need to write to the database for every command.
const touchEvery = 30 * time.Second

// maxOutputBytes is the max bytes collected for each of stdout / stderr of one command.
//
// The tool layer truncates again on its own (bash 64 KiB, read 2 MiB of base64); this is only to
// stop a single `cat /dev/urandom` from blowing up amad's memory.
const maxOutputBytes = 16 << 20

// Exec satisfies shelltools.Execer: make sure the container is running, then docker exec.
func (s *session) Exec(ctx context.Context, spec shelltools.ExecSpec) (*shelltools.Result, error) {
	res, err := s.exec(ctx, spec)
	if err != nil && (docker.IsConflict(err) || docker.IsNotFound(err)) {
		// The container was paused / removed by another instance between "confirmed running" and
		// "exec" (409 container is paused / 404 no such container). Clear the short cache and try
		// once more; ensureRunning will bring it back.
		s.mu.Lock()
		s.checkedAt = time.Time{}
		s.mu.Unlock()
		res, err = s.exec(ctx, spec)
	}
	return res, err
}

func (s *session) exec(ctx context.Context, spec shelltools.ExecSpec) (*shelltools.Result, error) {
	id, err := s.ensureRunning(ctx)
	if err != nil {
		return nil, err
	}
	s.touch(ctx)

	cmd := append([]string{spec.Path}, spec.Args...)
	if spec.Timeout > 0 {
		// The Engine API has no "kill this exec on time" endpoint; canceling ctx only makes us stop
		// waiting while the process keeps running in the container. So wrap it with coreutils
		// timeout: TERM on time, then KILL 5 seconds later.
		secs := int(math.Ceil(spec.Timeout.Seconds()))
		cmd = append([]string{"timeout", "-k", "5", fmt.Sprintf("%d", secs)}, cmd...)
		// Wait a little longer on our side so timeout gets the chance to act first and bring back
		// the exit code.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.Timeout+15*time.Second)
		defer cancel()
	}
	workdir := spec.Cwd
	if workdir == "" {
		workdir = s.p.cfg.Workdir
	}
	var stdin []byte
	if spec.Stdin != "" {
		stdin = []byte(spec.Stdin)
	}

	start := s.p.now()
	out, err := s.p.engine.Run(ctx, id, docker.RunOptions{
		Cmd:         cmd,
		Env:         envList(spec.Envs),
		WorkingDir:  workdir,
		User:        spec.User,
		Stdin:       stdin,
		OutputLimit: maxOutputBytes,
	})
	if err != nil {
		return nil, err
	}
	code := out.ExitCode
	res := &shelltools.Result{ExitCode: &code, Stdout: string(out.Stdout), Stderr: string(out.Stderr)}
	if spec.Timeout > 0 && (code == 124 || code == 137) {
		res.Err = fmt.Sprintf("命令超过 %s 没结束，已在容器里被终止（exit=%d）。"+
			"长命令请改用后台执行（run_in_background）", spec.Timeout, code)
	}
	if out.Truncated {
		res.Stderr += fmt.Sprintf("\n[输出超过 %d 字节，超出部分已丢弃]", maxOutputBytes)
	}
	s.log.Debug("容器内命令执行完成",
		slog.String("path", spec.Path), slog.Int("exit", code),
		slog.Duration("took", s.p.now().Sub(start).Round(time.Millisecond)))
	return res, nil
}

// envList turns the map into the K=V list docker wants, sorted by key (stable logs and tests).
func envList(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out
}

// touch writes the activity ledger with throttling. Failures do not affect execution (the
// implementation logs them itself).
func (s *session) touch(ctx context.Context) {
	s.p.mu.Lock()
	ledger := s.p.activity
	s.p.mu.Unlock()
	if ledger == nil {
		return
	}
	s.mu.Lock()
	now := s.p.now()
	if !s.touchedAt.IsZero() && now.Sub(s.touchedAt) < touchEvery {
		s.mu.Unlock()
		return
	}
	s.touchedAt = now
	s.mu.Unlock()
	ledger.TouchSession(ctx, s.arkSessionID)
}

// ensureRunning makes sure the container is running and returns the container id.
//
// Each step follows the daemon's real current state, so whatever another instance did in between
// (pause, remove, create first) is handled:
//
//	missing          → create (409 = someone just created it → inspect again and reuse)
//	paused           → unpause
//	created/exited   → start
//	removing/dead    → force remove, then rebuild
//	running          → use as is
func (s *session) ensureRunning(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.used = true
	if s.id != "" && s.p.now().Sub(s.checkedAt) < runningCacheFor {
		return s.id, nil
	}

	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		info, err := s.p.engine.ContainerInspect(ctx, s.name)
		if docker.IsNotFound(err) {
			id, cerr := s.createLocked(ctx)
			if docker.IsConflict(cerr) {
				// Another instance (or another work item in the same process) just created it;
				// inspect again and use theirs.
				lastErr = cerr
				continue
			}
			if cerr != nil {
				return "", cerr
			}
			return s.markRunningLocked(id), nil
		}
		if err != nil {
			return "", fmt.Errorf("查容器 %s 失败: %w", s.name, err)
		}

		switch info.State.Status {
		case "running":
			return s.markRunningLocked(info.ID), nil
		case "paused":
			start := s.p.now()
			if err := s.p.engine.ContainerUnpause(ctx, info.ID); err != nil && !docker.IsConflict(err) {
				return "", fmt.Errorf("恢复容器 %s 失败: %w", s.name, err)
			}
			s.log.Info("容器已恢复（unpause）", slog.String("container", s.name),
				slog.Duration("took", s.p.now().Sub(start).Round(time.Millisecond)))
			lastErr = nil
			continue // inspect once more to confirm it is really running
		case "created", "exited":
			start := s.p.now()
			if err := s.p.engine.ContainerStart(ctx, info.ID); err != nil {
				return "", fmt.Errorf("启动容器 %s 失败: %w", s.name, err)
			}
			s.log.Info("容器已重新启动（之前是停止状态，工作区 volume 保留、后台进程不会回来）",
				slog.String("container", s.name),
				slog.Duration("took", s.p.now().Sub(start).Round(time.Millisecond)))
			continue
		case "restarting":
			lastErr = fmt.Errorf("容器 %s 正在重启", s.name)
			if !sleepCtx(ctx, 500*time.Millisecond) {
				return "", ctx.Err()
			}
			continue
		default: // removing / dead
			s.log.Warn("容器处于不可用状态，删掉重建（工作区 volume 保留）",
				slog.String("container", s.name), slog.String("status", info.State.Status))
			if err := s.p.engine.ContainerRemove(ctx, info.ID, true); err != nil {
				return "", fmt.Errorf("删除不可用的容器 %s 失败: %w", s.name, err)
			}
			continue
		}
	}
	if lastErr == nil {
		lastErr = errors.New("状态反复变化")
	}
	return "", fmt.Errorf("容器 %s 一直没能进入运行状态: %w", s.name, lastErr)
}

func (s *session) markRunningLocked(id string) string {
	s.id = id
	s.checkedAt = s.p.now()
	return id
}

// createLocked creates the workspace volume and the container and starts it, then does session
// setup (inject skills, record).
// The caller holds s.mu. When the name is taken it returns an error for which IsConflict is true.
func (s *session) createLocked(ctx context.Context) (string, error) {
	start := s.p.now()
	p := s.p

	// First check whether the volume exists: only used to say clearly in the logs whether the
	// "workspace was kept" or "reset".
	_, verr := p.engine.VolumeInspect(ctx, s.volume)
	volumeExisted := verr == nil
	if verr != nil && !docker.IsNotFound(verr) {
		return "", fmt.Errorf("查工作区 volume %s 失败: %w", s.volume, verr)
	}
	// If one with the same name exists the daemon just returns it, so concurrent creation is safe.
	if _, err := p.engine.VolumeCreate(ctx, s.volume, p.labels(s.arkSessionID, "workspace")); err != nil {
		return "", fmt.Errorf("建工作区 volume %s 失败: %w", s.volume, err)
	}

	cfg := p.containerConfig(s.arkSessionID, s.volume)
	id, err := p.engine.ContainerCreate(ctx, s.name, cfg)
	if docker.IsNotFound(err) {
		// The image is not local. Pull once and create again.
		if perr := p.ensureImage(ctx); perr != nil {
			return "", perr
		}
		id, err = p.engine.ContainerCreate(ctx, s.name, cfg)
	}
	if err != nil {
		if docker.IsConflict(err) {
			return "", err
		}
		return "", fmt.Errorf("建容器 %s 失败: %w", s.name, err)
	}
	if err := p.engine.ContainerStart(ctx, id); err != nil {
		return "", fmt.Errorf("启动容器 %s 失败: %w", s.name, err)
	}

	took := p.now().Sub(start).Round(time.Millisecond)
	switch {
	case volumeExisted:
		s.log.Info("容器不存在，已重建——工作区 volume 还在，之前的文件都保留着",
			slog.String("container", s.name), slog.String("volume", s.volume), slog.Duration("took", took))
	case s.hadContainerBefore(ctx):
		s.log.Warn("容器与工作区 volume 都已不在（被回收或被手动删除），已新建——工作区已重置",
			slog.String("container", s.name), slog.String("volume", s.volume), slog.Duration("took", took))
	default:
		s.log.Info("容器已创建（首次调用工具时懒建）",
			slog.String("container", s.name), slog.String("volume", s.volume), slog.Duration("took", took))
	}

	s.initFresh(ctx, id)
	return id, nil
}

// hadContainerBefore asks the ledger "did this session have a container before". Not found / not
// wired both count as no.
func (s *session) hadContainerBefore(ctx context.Context) bool {
	s.p.mu.Lock()
	lookup := s.p.lookup
	s.p.mu.Unlock()
	if lookup == nil {
		return false
	}
	prev, err := lookup.LastSession(ctx, s.arkSessionID)
	return err == nil && prev != ""
}

// initFresh does session setup for a freshly created container: record first, then inject skills.
//
// Done while holding s.mu: concurrent tool calls cannot start running before the skill files are
// written. The executor used to inject skills targets this container id directly and **must not go
// through s.Exec** (that would take s.mu again and deadlock).
//
// Failures are only logged: missing skill files is far less bad than a whole session being unable
// to run commands.
func (s *session) initFresh(ctx context.Context, id string) {
	p := s.p
	p.mu.Lock()
	recorder, loader := p.recorder, p.skills
	daemonID := p.daemonID
	p.mu.Unlock()

	if recorder != nil {
		recorder.RecordSession(ctx, s.arkSessionID, s.name, daemonID)
	}
	if loader == nil {
		return
	}
	bundles, err := loader.SkillsFor(ctx, s.arkSessionID)
	if err != nil {
		s.log.Warn("查会话的技能失败，容器仍可用但没有技能文件", slog.Any("err", err))
		return
	}
	raw := rawExecer{s: s, id: id}
	if err := shelltools.InjectSkills(ctx, raw, p.cfg.Workdir, bundles, s.log); err != nil {
		s.log.Warn("技能文件注入失败，容器仍可用但 agent 读不到附件", slog.Any("err", err))
	}
}

// rawExecer runs directly on a container id without ensureRunning (the caller makes sure the
// container is awake).
type rawExecer struct {
	s  *session
	id string
}

func (r rawExecer) Exec(ctx context.Context, spec shelltools.ExecSpec) (*shelltools.Result, error) {
	cmd := append([]string{spec.Path}, spec.Args...)
	if spec.Timeout > 0 {
		cmd = append([]string{"timeout", "-k", "5",
			fmt.Sprintf("%d", int(math.Ceil(spec.Timeout.Seconds())))}, cmd...)
	}
	wd := spec.Cwd
	if wd == "" {
		wd = r.s.p.cfg.Workdir
	}
	out, err := r.s.p.engine.Run(ctx, r.id, docker.RunOptions{
		Cmd: cmd, Env: envList(spec.Envs), WorkingDir: wd, User: spec.User, OutputLimit: maxOutputBytes,
	})
	if err != nil {
		return nil, err
	}
	code := out.ExitCode
	return &shelltools.Result{ExitCode: &code, Stdout: string(out.Stdout), Stderr: string(out.Stderr)}, nil
}

// containerConfig builds the create parameters for the session container.
func (p *Provider) containerConfig(arkSessionID, volume string) docker.ContainerConfig {
	init := true
	hc := docker.HostConfig{
		Mounts:      []docker.Mount{{Type: "volume", Source: volume, Target: p.cfg.Workdir}},
		NetworkMode: strings.TrimSpace(p.cfg.Network),
		// tini as PID 1: someone reaps the zombie processes left after background jobs (setsid
		// nohup) exit.
		Init: &init,
		// The container runs commands the agent generated; at least do not let them escalate
		// privileges inside it.
		SecurityOpt: []string{"no-new-privileges"},
	}
	if p.cfg.CPUs > 0 {
		hc.NanoCPUs = int64(p.cfg.CPUs * 1e9)
	}
	if p.cfg.MemoryBytes > 0 {
		hc.Memory = p.cfg.MemoryBytes
	}
	if p.cfg.PidsLimit > 0 {
		n := p.cfg.PidsLimit
		hc.PidsLimit = &n
	}
	return docker.ContainerConfig{
		Image: p.cfg.Image,
		// Keep the container "alive doing nothing"; every command goes through exec.
		//
		// Not `sleep infinity`: busybox sleep does not accept infinity. Not a bare sleep loop
		// either: it ignores SIGTERM, so docker stop would wait out the whole timeout before KILL.
		// This line exits right away on TERM.
		Entrypoint: []string{"/bin/sh", "-c", "trap 'exit 0' TERM INT; while :; do sleep 3600 & wait $!; done"},
		WorkingDir: p.cfg.Workdir,
		Hostname:   "ama",
		Labels:     p.labels(arkSessionID, "session"),
		HostConfig: hc,
	}
}

// ---------------------------------------------------------------------------
// Idle
// ---------------------------------------------------------------------------

// idle is the "this turn is over" callback: pause or stop the container per config, without
// touching the workspace.
//
// Three cases are skipped: this work item never touched the container, a web terminal is open, or a
// background job is running in the container.
// Failures are only logged: the turn already ended normally, and failing to save resources should
// not make the caller think something went wrong.
func (s *session) idle(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.used || s.p.cfg.IdleAction == IdleNone {
		return
	}
	info, err := s.p.engine.ContainerInspect(ctx, s.name)
	if err != nil {
		if !docker.IsNotFound(err) {
			s.log.Debug("空闲收尾查容器失败，跳过", slog.Any("err", err))
		}
		return
	}
	if info.State.Status != "running" {
		return
	}
	if s.terminalInUse(ctx) {
		s.log.Info("网页终端正在使用，跳过空闲处理（等它空闲后的下一轮）")
		return
	}
	if s.hasRunningBackgroundJob(ctx, info.ID) {
		s.log.Info("容器里有后台任务在跑，跳过空闲处理（等它结束后的下一轮）")
		return
	}

	start := s.p.now()
	switch s.p.cfg.IdleAction {
	case IdleStop:
		if err := s.p.engine.ContainerStop(ctx, info.ID, 10*time.Second); err != nil {
			s.log.Warn("停止空闲容器失败，它会继续占用资源", slog.Any("err", err))
			return
		}
		s.log.Info("会话空闲，容器已停止（工作区 volume 保留）",
			slog.Duration("took", s.p.now().Sub(start).Round(time.Millisecond)))
	default:
		if err := s.p.engine.ContainerPause(ctx, info.ID); err != nil && !docker.IsConflict(err) {
			s.log.Warn("暂停空闲容器失败，它会继续占用 CPU", slog.Any("err", err))
			return
		}
		s.log.Info("会话空闲，容器已暂停",
			slog.Duration("took", s.p.now().Sub(start).Round(time.Millisecond)))
	}
	s.checkedAt = time.Time{}
}

// terminalInUse asks the cross-instance ledger: is this container's web terminal still open. On
// error it counts as "not in use".
func (s *session) terminalInUse(ctx context.Context) bool {
	s.p.mu.Lock()
	terms := s.p.terms
	s.p.mu.Unlock()
	if terms == nil {
		return false
	}
	active, err := terms.ActiveTerminalSessions(ctx)
	if err != nil {
		s.log.Debug("查在用终端失败，按「没在用」处理", slog.Any("err", err))
		return false
	}
	for _, name := range active {
		if name == s.name {
			return true
		}
	}
	return false
}

// hasRunningBackgroundJob asks whether a background job started by the bash tool is still running
// in the container.
//
// Pausing would freeze it (it continues after resume, but makes no progress meanwhile), and
// stopping would kill it. Both would leave a build/release started with `run_in_background` stuck
// halfway with no notice, so if a job is running this round is skipped.
//
// Ask every time instead of relying on an in-process "this session started a background job" flag:
// the background job may have been started by the previous work item (or even another instance).
// The container is on the local daemon, and one exec takes tens of milliseconds.
func (s *session) hasRunningBackgroundJob(ctx context.Context, id string) bool {
	script := `for f in ` + shelltools.BGLogDir + `/*.pid; do
  [ -e "$f" ] || continue
  pid=$(cat "$f" 2>/dev/null) || continue
  if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then echo RUNNING; break; fi
done
true`
	out, err := s.p.engine.Run(ctx, id, docker.RunOptions{
		Cmd: []string{"sh", "-c", script}, OutputLimit: 4096,
	})
	if err != nil {
		s.log.Debug("查后台任务失败，按「没有」处理", slog.Any("err", err))
		return false
	}
	return strings.Contains(string(out.Stdout), "RUNNING")
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

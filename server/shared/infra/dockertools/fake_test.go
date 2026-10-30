package dockertools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/chyroc/ama/server/shared/infra/docker"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func apiErr(code int, msg string) error {
	return &docker.APIError{Op: "fake", StatusCode: code, Message: msg}
}

type fakeContainer struct {
	id      string
	name    string
	status  string
	labels  map[string]string
	created int64
	cfg     docker.ContainerConfig
}

// fakeEngine is an in-memory Docker daemon, good enough to test the state machine and reaping.
type fakeEngine struct {
	mu         sync.Mutex
	daemonID   string
	images     map[string]bool
	containers map[string]*fakeContainer // by name
	volumes    map[string]docker.VolumeInfo
	runs       []docker.RunOptions
	runOn      []string
	// runFn decides the result of Run; when nil it returns exit 0 and empty output.
	runFn func(container string, opt docker.RunOptions) (*docker.RunResult, error)
	// createConflictOnce makes the next ContainerCreate return 409, with "someone else" having just
	// created it.
	createConflictOnce bool
	creates            int
	pulls              int
	nextID             int
	ops                []string
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{
		daemonID:   "daemon-A",
		images:     map[string]bool{DefaultImage: true},
		containers: map[string]*fakeContainer{},
		volumes:    map[string]docker.VolumeInfo{},
	}
}

func (f *fakeEngine) op(s string) { f.ops = append(f.ops, s) }

func (f *fakeEngine) find(idOrName string) *fakeContainer {
	if c, ok := f.containers[idOrName]; ok {
		return c
	}
	for _, c := range f.containers {
		if c.id == idOrName {
			return c
		}
	}
	return nil
}

func (f *fakeEngine) Ping(context.Context) error { return nil }
func (f *fakeEngine) Info(context.Context) (*docker.SystemInfo, error) {
	return &docker.SystemInfo{ID: f.daemonID, Name: "fake"}, nil
}
func (f *fakeEngine) ImageInspect(_ context.Context, ref string) (*docker.ImageInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.images[ref] {
		return &docker.ImageInfo{ID: "sha256:" + ref}, nil
	}
	return nil, apiErr(http.StatusNotFound, "No such image")
}
func (f *fakeEngine) ImagePull(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulls++
	f.images[ref] = true
	return nil
}
func (f *fakeEngine) ContainerCreate(_ context.Context, name string, cfg docker.ContainerConfig) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createConflictOnce {
		f.createConflictOnce = false
		f.nextID++
		f.containers[name] = &fakeContainer{id: fmt.Sprintf("other-%d", f.nextID), name: name, status: "running", labels: cfg.Labels}
		return "", apiErr(http.StatusConflict, "name in use")
	}
	if _, ok := f.containers[name]; ok {
		return "", apiErr(http.StatusConflict, "name in use")
	}
	if !f.images[cfg.Image] {
		return "", apiErr(http.StatusNotFound, "No such image: "+cfg.Image)
	}
	f.creates++
	f.nextID++
	c := &fakeContainer{id: fmt.Sprintf("cid-%d", f.nextID), name: name, status: "created", labels: cfg.Labels, cfg: cfg, created: time.Now().Unix()}
	f.containers[name] = c
	f.op("create " + name)
	return c.id, nil
}
func (f *fakeEngine) setStatus(id, from, to, op string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.find(id)
	if c == nil {
		return apiErr(http.StatusNotFound, "No such container")
	}
	if from != "" && !strings.Contains(from, c.status) {
		return apiErr(http.StatusConflict, "container is "+c.status)
	}
	c.status = to
	f.op(op + " " + c.name)
	return nil
}
func (f *fakeEngine) ContainerStart(_ context.Context, id string) error {
	return f.setStatus(id, "created exited running", "running", "start")
}
func (f *fakeEngine) ContainerStop(_ context.Context, id string, _ time.Duration) error {
	return f.setStatus(id, "running paused exited", "exited", "stop")
}
func (f *fakeEngine) ContainerPause(_ context.Context, id string) error {
	return f.setStatus(id, "running", "paused", "pause")
}
func (f *fakeEngine) ContainerUnpause(_ context.Context, id string) error {
	return f.setStatus(id, "paused", "running", "unpause")
}
func (f *fakeEngine) ContainerInspect(_ context.Context, id string) (*docker.ContainerInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.find(id)
	if c == nil {
		return nil, apiErr(http.StatusNotFound, "No such container")
	}
	info := &docker.ContainerInfo{ID: c.id, Name: "/" + c.name}
	info.State.Status = c.status
	info.Config.Labels = c.labels
	return info, nil
}
func (f *fakeEngine) ContainerRemove(_ context.Context, id string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.find(id)
	if c == nil {
		return nil
	}
	delete(f.containers, c.name)
	f.op("rm " + c.name)
	return nil
}
func matchLabels(have map[string]string, want []string) bool {
	for _, w := range want {
		k, v, hasV := strings.Cut(w, "=")
		got, ok := have[k]
		if !ok || (hasV && got != v) {
			return false
		}
	}
	return true
}
func (f *fakeEngine) ContainerList(_ context.Context, labels []string) ([]docker.ContainerSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []docker.ContainerSummary
	for _, c := range f.containers {
		if matchLabels(c.labels, labels) {
			out = append(out, docker.ContainerSummary{ID: c.id, Names: []string{"/" + c.name}, State: c.status, Created: c.created, Labels: c.labels})
		}
	}
	return out, nil
}
func (f *fakeEngine) VolumeCreate(_ context.Context, name string, labels map[string]string) (*docker.VolumeInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.volumes[name]; ok {
		return &v, nil
	}
	v := docker.VolumeInfo{Name: name, Labels: labels, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	f.volumes[name] = v
	f.op("volume " + name)
	return &v, nil
}
func (f *fakeEngine) VolumeInspect(_ context.Context, name string) (*docker.VolumeInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.volumes[name]; ok {
		return &v, nil
	}
	return nil, apiErr(http.StatusNotFound, "no such volume")
}
func (f *fakeEngine) VolumeRemove(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.volumes, name)
	f.op("rmvol " + name)
	return nil
}
func (f *fakeEngine) VolumeList(_ context.Context, labels []string) ([]docker.VolumeInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []docker.VolumeInfo
	for _, v := range f.volumes {
		if matchLabels(v.Labels, labels) {
			out = append(out, v)
		}
	}
	return out, nil
}
func (f *fakeEngine) Run(_ context.Context, container string, opt docker.RunOptions) (*docker.RunResult, error) {
	f.mu.Lock()
	c := f.find(container)
	if c == nil {
		f.mu.Unlock()
		return nil, apiErr(http.StatusNotFound, "No such container")
	}
	if c.status != "running" {
		f.mu.Unlock()
		return nil, apiErr(http.StatusConflict, "container is "+c.status)
	}
	f.runs = append(f.runs, opt)
	f.runOn = append(f.runOn, container)
	fn := f.runFn
	f.mu.Unlock()
	if fn != nil {
		return fn(container, opt)
	}
	return &docker.RunResult{}, nil
}
func (f *fakeEngine) ExecCreate(context.Context, string, docker.ExecConfig) (string, error) {
	return "", errors.New("fake: 终端不在单测里覆盖（见 integration_test.go）")
}
func (f *fakeEngine) ExecStartAttached(context.Context, string, bool) (*docker.HijackedConn, error) {
	return nil, errors.New("fake: 不支持")
}
func (f *fakeEngine) ExecResize(context.Context, string, int, int) error { return nil }

var _ Engine = (*fakeEngine)(nil)

// fakeLedger is an in-memory session ledger.
type fakeLedger struct {
	mu        sync.Mutex
	records   map[string][2]string // arkSessionID → {container name, daemon}
	activity  map[string]time.Time
	touches   int
	actErr    error
	terminals []string
}

func newFakeLedger() *fakeLedger {
	return &fakeLedger{records: map[string][2]string{}, activity: map[string]time.Time{}}
}

func (l *fakeLedger) RecordSession(_ context.Context, id, name, daemon string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records[id] = [2]string{name, daemon}
}
func (l *fakeLedger) LastSession(_ context.Context, id string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.records[id][0], nil
}
func (l *fakeLedger) TouchSession(_ context.Context, id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.touches++
	l.activity[id] = time.Now()
}
func (l *fakeLedger) SessionActivity(_ context.Context, ids []string) (map[string]time.Time, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.actErr != nil {
		return nil, l.actErr
	}
	out := map[string]time.Time{}
	for _, id := range ids {
		if t, ok := l.activity[id]; ok {
			out[id] = t
		}
	}
	return out, nil
}
func (l *fakeLedger) ActiveTerminalSessions(context.Context) ([]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.terminals, nil
}

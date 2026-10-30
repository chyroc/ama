package docker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// ContainerConfig is the input for creating a container. It only has the fields we use.
//
// Field names map one-to-one to the Engine API ContainerCreate request body (serialized by json tag).
type ContainerConfig struct {
	Image      string            `json:"Image"`
	Cmd        []string          `json:"Cmd,omitempty"`
	Entrypoint []string          `json:"Entrypoint,omitempty"`
	Env        []string          `json:"Env,omitempty"`
	WorkingDir string            `json:"WorkingDir,omitempty"`
	User       string            `json:"User,omitempty"`
	Hostname   string            `json:"Hostname,omitempty"`
	Labels     map[string]string `json:"Labels,omitempty"`
	Tty        bool              `json:"Tty,omitempty"`
	OpenStdin  bool              `json:"OpenStdin,omitempty"`
	HostConfig HostConfig        `json:"HostConfig"`
}

// HostConfig is the HostConfig section of ContainerCreate.
type HostConfig struct {
	// Mounts are the mounts (we only use type=volume).
	Mounts []Mount `json:"Mounts,omitempty"`
	// NetworkMode is bridge / none / host / a custom network name. Empty uses the daemon default (bridge).
	NetworkMode string `json:"NetworkMode,omitempty"`
	// NanoCPUs is the CPU quota, 1 core = 1e9. 0 means no limit.
	NanoCPUs int64 `json:"NanoCpus,omitempty"`
	// Memory is the memory limit (bytes). 0 means no limit.
	Memory int64 `json:"Memory,omitempty"`
	// PidsLimit is the process count limit. nil means no limit.
	PidsLimit *int64 `json:"PidsLimit,omitempty"`
	// Init makes the daemon use tini as PID 1, to reap zombie processes left by background jobs.
	Init *bool `json:"Init,omitempty"`
	// RestartPolicy is deliberately not set (default no): we decide when a stopped container
	// starts again, instead of the daemon restarting it and wasting resources.
	SecurityOpt []string `json:"SecurityOpt,omitempty"`
	CapDrop     []string `json:"CapDrop,omitempty"`
}

// Mount is one mount.
type Mount struct {
	Type   string `json:"Type"` // volume / bind / tmpfs
	Source string `json:"Source"`
	Target string `json:"Target"`
}

// ContainerState is the State section of inspect.
type ContainerState struct {
	// Status is one of created / running / paused / restarting / removing / exited / dead.
	Status     string `json:"Status"`
	Running    bool   `json:"Running"`
	Paused     bool   `json:"Paused"`
	ExitCode   int    `json:"ExitCode"`
	StartedAt  string `json:"StartedAt"`
	FinishedAt string `json:"FinishedAt"`
}

// ContainerInfo holds the fields of container inspect that we use.
type ContainerInfo struct {
	ID      string         `json:"Id"`
	Name    string         `json:"Name"`
	Created string         `json:"Created"`
	State   ContainerState `json:"State"`
	Config  struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

// ContainerCreate creates a container and returns its id.
//
// When the name is already taken it returns an error for which IsConflict is true. **Callers rely
// on this to "race to create"**: when several instances create a container for the same session
// at once, only one succeeds; the others get 409 and inspect and reuse that one.
// When the image does not exist it returns an error for which IsNotFound is true.
func (c *Client) ContainerCreate(ctx context.Context, name string, cfg ContainerConfig) (string, error) {
	q := url.Values{}
	if name != "" {
		q.Set("name", name)
	}
	var out struct {
		ID string `json:"Id"`
	}
	if err := c.doJSON(ctx, "建容器 "+name, http.MethodPost, "/containers/create", q, cfg, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// ContainerStart starts a container. Already running (304) is not an error.
func (c *Client) ContainerStart(ctx context.Context, id string) error {
	return c.doJSON(ctx, "启动容器 "+id, http.MethodPost,
		"/containers/"+url.PathEscape(id)+"/start", nil, nil, nil, http.StatusNotModified)
}

// ContainerStop stops a container; timeout is the time the process gets to exit cleanly. Already
// stopped (304) is not an error.
func (c *Client) ContainerStop(ctx context.Context, id string, timeout time.Duration) error {
	q := url.Values{"t": {strconv.Itoa(int(timeout.Seconds()))}}
	return c.doJSON(ctx, "停容器 "+id, http.MethodPost,
		"/containers/"+url.PathEscape(id)+"/stop", q, nil, nil, http.StatusNotModified)
}

// ContainerPause freezes every process in the container (cgroup freezer); memory is kept.
//
// The daemon answers 409 when the container is not running or already paused.
func (c *Client) ContainerPause(ctx context.Context, id string) error {
	return c.doJSON(ctx, "暂停容器 "+id, http.MethodPost,
		"/containers/"+url.PathEscape(id)+"/pause", nil, nil, nil)
}

// ContainerUnpause unfreezes. The daemon answers 409 when it is not paused.
func (c *Client) ContainerUnpause(ctx context.Context, id string) error {
	return c.doJSON(ctx, "恢复容器 "+id, http.MethodPost,
		"/containers/"+url.PathEscape(id)+"/unpause", nil, nil, nil)
}

// ContainerInspect looks up a container (id or name). If it does not exist, it returns an error
// for which IsNotFound is true.
func (c *Client) ContainerInspect(ctx context.Context, id string) (*ContainerInfo, error) {
	var out ContainerInfo
	if err := c.doJSON(ctx, "查容器 "+id, http.MethodGet,
		"/containers/"+url.PathEscape(id)+"/json", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ContainerRemove removes a container. force kills running processes first; not existing (404)
// is not an error.
//
// It deliberately does **not pass v=1**: that would also remove anonymous volumes. Our workspace
// is a named volume, which it would not remove anyway, but leaving the parameter out makes
// "removing a container does not touch data" more obvious.
func (c *Client) ContainerRemove(ctx context.Context, id string, force bool) error {
	q := url.Values{}
	if force {
		q.Set("force", "1")
	}
	err := c.doJSON(ctx, "删容器 "+id, http.MethodDelete,
		"/containers/"+url.PathEscape(id), q, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// ContainerSummary holds the fields of each item when listing containers.
type ContainerSummary struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	State   string            `json:"State"`
	Created int64             `json:"Created"` // unix seconds
	Labels  map[string]string `json:"Labels"`
}

// ContainerList lists containers by label (including stopped ones). labels look like "key=value" or "key".
func (c *Client) ContainerList(ctx context.Context, labels []string) ([]ContainerSummary, error) {
	q := url.Values{"all": {"1"}}
	if f := labelFilter(labels); f != "" {
		q.Set("filters", f)
	}
	var out []ContainerSummary
	if err := c.doJSON(ctx, "列容器", http.MethodGet, "/containers/json", q, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func labelFilter(labels []string) string {
	if len(labels) == 0 {
		return ""
	}
	raw, _ := json.Marshal(map[string][]string{"label": labels})
	return string(raw)
}

// ---------------------------------------------------------------------------
// volume
// ---------------------------------------------------------------------------

// VolumeInfo holds the fields of a volume.
type VolumeInfo struct {
	Name      string            `json:"Name"`
	CreatedAt string            `json:"CreatedAt"`
	Labels    map[string]string `json:"Labels"`
}

// VolumeCreate creates a named volume. If one with the same name exists, the daemon just returns
// it (no error),
// so several instances creating the same volume at once is safe.
func (c *Client) VolumeCreate(ctx context.Context, name string, labels map[string]string) (*VolumeInfo, error) {
	var out VolumeInfo
	body := map[string]any{"Name": name, "Labels": labels}
	if err := c.doJSON(ctx, "建 volume "+name, http.MethodPost, "/volumes/create", nil, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// VolumeInspect looks up a volume. If it does not exist, it returns an error for which IsNotFound is true.
func (c *Client) VolumeInspect(ctx context.Context, name string) (*VolumeInfo, error) {
	var out VolumeInfo
	if err := c.doJSON(ctx, "查 volume "+name, http.MethodGet,
		"/volumes/"+url.PathEscape(name), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// VolumeRemove removes a volume. Not existing is not an error; the daemon answers 409 while a
// container still uses it.
func (c *Client) VolumeRemove(ctx context.Context, name string) error {
	err := c.doJSON(ctx, "删 volume "+name, http.MethodDelete,
		"/volumes/"+url.PathEscape(name), nil, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// VolumeList lists volumes by label.
func (c *Client) VolumeList(ctx context.Context, labels []string) ([]VolumeInfo, error) {
	q := url.Values{}
	if f := labelFilter(labels); f != "" {
		q.Set("filters", f)
	}
	var out struct {
		Volumes []VolumeInfo `json:"Volumes"`
	}
	if err := c.doJSON(ctx, "列 volume", http.MethodGet, "/volumes", q, nil, &out); err != nil {
		return nil, err
	}
	return out.Volumes, nil
}

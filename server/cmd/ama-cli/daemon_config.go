// The local state file ~/.ama/host.json shared by the daemon subcommands.
//
// # Why "one per user" and not a system path per machine
//
// host_token is a credential registered by **this user**, binding workspaces and running their
// agents under their identity; when several users share one Mac (or CI machine) each must have
// separate credentials. Putting it in $HOME isolates by user naturally; launchd using
// ~/Library/LaunchAgents (user-level) and systemd using --user follow the same boundary.
//
// # File security
//
// It contains the plaintext host_token (the server only stores sha256; if lost, the only option is
// to register again). Permissions must be 0600; when the file content is logged, the token field is
// always printed as "***".
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// daemonConfig is the content of ~/.ama/host.json.
type daemonConfig struct {
	// HostUUID is a random id generated on this machine's first start, reported on registration;
	// the same user registering again idempotently hits the same hosts row. Reinstalling/deleting
	// this file registers a **new** machine (see the reset command).
	HostUUID string `json:"hostUuid"`
	// HostID is the host id returned by the server after successful registration.
	HostID string `json:"hostId"`
	// Token is the plaintext host_token, used for heartbeats/results. It only exists in this file.
	Token string `json:"token"`
	// BaseURL is the AMA address used at registration (AMA_BASE_URL or the default local address),
	// so in service mode it can keep connecting to the same address even without a shell env.
	BaseURL string `json:"baseUrl"`
	// LastSeq is the cursor of confirmed dispatch deliveries (the daemon side's persistent source of
	// truth; see host_dispatches.sql). Persisted every time it advances on receive/replay.
	LastSeq int64 `json:"lastSeq"`
	// WSBaseURL is an explicit override of the WebSocket entry (an origin, like
	// wss://ama-ws.example.com). When empty it is derived from BaseURL at runtime (see
	// deriveWSBaseURL); the derivation rule evolves with the code and is deliberately not persisted,
	// so a frozen old mapping does not get in the way of a future domain change.
	WSBaseURL string `json:"wsBaseUrl,omitempty"`
	// Transport is the transport preference: ""/"auto" = prefer WS, fall back to HTTP on failure;
	// "http" = HTTP only (escape hatch); "ws" = same as auto. Invalid values count as auto.
	Transport string `json:"transport,omitempty"`
}

func amaDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("找不到用户主目录: %w", err)
	}
	return filepath.Join(home, ".ama"), nil
}

func configPath() (string, error) {
	dir, err := amaDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "host.json"), nil
}

// loadDaemonConfig reads the state file; if missing it returns (zero value, fs.ErrNotExist), and the
// caller uses that to decide to register first.
func loadDaemonConfig() (daemonConfig, error) {
	p, err := configPath()
	if err != nil {
		return daemonConfig{}, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return daemonConfig{}, err
	}
	var cfg daemonConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return daemonConfig{}, fmt.Errorf("解析 %s 失败: %w（文件损坏，可删除后重新 AMA_TOKEN=... ama daemon install）", p, err)
	}
	return cfg, nil
}

// save writes back with 0600 (write a temp file first, then rename, so a half-written file is never read).
func (cfg *daemonConfig) save() error {
	dir, err := amaDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建 %s 失败: %w", dir, err)
	}
	p := filepath.Join(dir, "host.json")
	tmp := p + ".tmp"
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("写 %s 失败: %w", tmp, err)
	}
	if err := os.Rename(tmp, p); err != nil {
		return fmt.Errorf("保存 %s 失败: %w", p, err)
	}
	return nil
}

// wipeToken wipes the credential on a 401 (host revoked/deleted); the daemon then exits and asks to
// register again. HostUUID is deliberately kept: after the host is **deleted**, registering again with
// the same uuid makes the server treat it as "a new machine" (the old row no longer exists, the unique
// key does not match) and issue a new token, and the uuid keeps being the stable identity of "this
// machine"; it is not about hitting the old row: with the old row gone, it should be a new host.
func (cfg *daemonConfig) wipeToken() error {
	cfg.Token = ""
	cfg.HostID = ""
	return cfg.save()
}

func isNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }

// newHostUUID generates 16 random bytes as hex.
func newHostUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

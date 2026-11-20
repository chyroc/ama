package shelltools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
)

// ExecEnv is one execution-scoped environment variable. Secret values are scrubbed from stdout,
// stderr, and backend errors before a tool result leaves the process. Reserved names may not be
// replaced by an agent's user-defined environment.
type ExecEnv struct {
	Key      string
	Value    string
	Secret   bool
	Reserved bool
}

// ExecEnvSource supplies environment values for one execution. It is called on every tool call,
// so implementations can refresh expiring credentials instead of storing them for the lifetime of
// a paused or resumed container. It belongs in the executor's process, not in the session image.
type ExecEnvSource interface {
	ExecEnvs(ctx context.Context, sessionID string) ([]ExecEnv, error)
}

// ExecEnvFunc adapts a function to ExecEnvSource.
type ExecEnvFunc func(ctx context.Context, sessionID string) ([]ExecEnv, error)

// ExecEnvs calls f(ctx, sessionID). A nil function returns no environment values.
func (f ExecEnvFunc) ExecEnvs(ctx context.Context, sessionID string) ([]ExecEnv, error) {
	if f == nil {
		return nil, nil
	}
	return f(ctx, sessionID)
}

// CredentialSource is the common shape of a single expiring credential and its display subject.
// Implementations should return an empty token without an error when the session has no credential.
type CredentialSource interface {
	Credential(ctx context.Context, sessionID string) (token, subject string, err error)
}

// CredentialEnvSet describes how a CredentialSource becomes environment variables. Keys and the
// data root are supplied by the downstream integration, not hard-coded in the public package.
type CredentialEnvSet struct {
	TokenKey       string
	SubjectKey     string
	DataRootKey    string
	DataRoot       string
	AdditionalEnvs []ExecEnv
}

// NewCredentialEnvSource adapts one refreshing credential to the generic ExecEnvSource interface.
// The token is both secret and reserved; the subject and data root are not secret, but are reserved
// when present so user-defined environment cannot redirect a credential helper's state.
func NewCredentialEnvSource(source CredentialSource, config CredentialEnvSet) ExecEnvSource {
	return ExecEnvFunc(func(ctx context.Context, sessionID string) ([]ExecEnv, error) {
		if source == nil {
			return nil, nil
		}
		token, subject, err := source.Credential(ctx, sessionID)
		if err != nil {
			return nil, err
		}
		token = strings.TrimSpace(token)
		if token == "" {
			return nil, nil
		}
		envs := []ExecEnv{{Key: config.TokenKey, Value: token, Secret: true, Reserved: true}}
		if subject = strings.TrimSpace(subject); subject != "" && config.SubjectKey != "" {
			envs = append(envs, ExecEnv{Key: config.SubjectKey, Value: subject, Reserved: true})
			if config.DataRootKey != "" && config.DataRoot != "" {
				envs = append(envs, ExecEnv{Key: config.DataRootKey, Value: config.DataRoot, Reserved: true})
			}
		}
		envs = append(envs, config.AdditionalEnvs...)
		return envs, nil
	})
}

var (
	envNamesMu       sync.RWMutex
	reservedEnvNames = map[string]bool{}
)

func markReservedEnv(key string) {
	envNamesMu.Lock()
	defer envNamesMu.Unlock()
	reservedEnvNames[key] = true
}

func isReservedEnv(key string) bool {
	envNamesMu.RLock()
	defer envNamesMu.RUnlock()
	return platformReservedEnvs[key] || reservedEnvNames[key]
}

func credentialFingerprint(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:8]
}

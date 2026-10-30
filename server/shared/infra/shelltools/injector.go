// SessionInjector prepares credential/env injection and output scrubbing for one tool execution.
//
// It pulls the "merge session env + aliases + build the scrubber" logic out of the old
// enrichingExecer into a piece that does not depend on the executor, shared by two paths:
//
//   - container path (Docker): enrichingExecer calls Prepare on each Exec and puts the merged
//     result into ExecSpec.Envs;
//   - host native path: nativeTool calls Prepare on each execution; envs are only attached to
//     bash calls, and scrub is used on the result text of all six tools.
//
// The warn-once state lives with the injector (one per session), with the same lifetime as the
// old enrichingExecer.
package shelltools

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"

	"github.com/chyroc/ama/server/model/persistmodel"
)

// SessionInjector: see the file header.
type SessionInjector struct {
	sessionID string
	envs      SessionEnvSource
	log       *slog.Logger

	mu sync.Mutex
	// envWarnLogged makes a fetch failure log only once: logging on every command would flood the log.
	envWarnLogged bool
}

// NewSessionInjector builds one. envs may be nil (no session env is injected).
func NewSessionInjector(sessionID string, envs SessionEnvSource, log *slog.Logger) *SessionInjector {
	if log == nil {
		log = slog.Default()
	}
	return &SessionInjector{sessionID: sessionID, envs: envs, log: log}
}

// Prepare computes the env to inject for this execution and the output scrub function.
//
// Called once per tool call: the injected token expires, and only a fresh fetch gets the renewed
// value. Any failure degrades to "inject a few fewer variables"; it never makes the tool fail.
func (i *SessionInjector) Prepare(ctx context.Context) (map[string]string, func(string) string) {
	envs, scrub := i.prepareWithBase(ctx, nil)
	if !scrub.active() {
		return envs, nil
	}
	return envs, scrub.text
}

// prepareWithBase is the same as Prepare, but allows the tool's own base env
// (ExecSpec.Envs, used by the container path; the native path has no such layer and passes nil).
func (i *SessionInjector) prepareWithBase(
	ctx context.Context, base map[string]string,
) (map[string]string, scrubber) {
	sessionEnvs, secretKeys, aliases := i.sessionEnvs(ctx)

	if len(sessionEnvs) == 0 && len(base) == 0 {
		return base, scrubber{}
	}

	// Merge priority from low to high: the tool's own env → session env.
	// Within session env, core makes sure the platform-injected keys win last (user-defined env
	// cannot replace the platform identity).
	merged := make(map[string]string, len(base)+len(sessionEnvs))
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range sessionEnvs {
		merged[k] = v
	}

	// Credential aliases: the value is taken fresh from merged, so it naturally inherits the
	// authorization decision; see the long comment in the old enrich.go.
	var aliasSecrets []string
	for _, a := range aliases {
		key := strings.TrimSpace(a.Key)
		if key == "" || IsPlatformReservedEnv(key) {
			continue
		}
		v, ok := merged[strings.TrimSpace(a.From)]
		if !ok || v == "" {
			continue
		}
		merged[key] = v
		if secretEnvNames[strings.TrimSpace(a.From)] {
			aliasSecrets = append(aliasSecrets, v)
		}
	}

	var toScrub []string
	for _, k := range secretKeys {
		if v, ok := sessionEnvs[k]; ok && v != "" {
			toScrub = append(toScrub, v)
		}
	}
	toScrub = append(toScrub, aliasSecrets...)
	return merged, newScrubber(toScrub...)
}

// sessionEnvs gets session-level env and blocks overrides of platform reserved keys; on failure
// it degrades to "no extra variables".
func (i *SessionInjector) sessionEnvs(ctx context.Context) (map[string]string, []string, []EnvAlias) {
	if i.envs == nil {
		return nil, nil, nil
	}
	got, secretKeys, aliases, err := i.envs.SessionEnvs(ctx, i.sessionID)
	if err != nil {
		i.mu.Lock()
		first := !i.envWarnLogged
		i.envWarnLogged = true
		i.mu.Unlock()
		if first {
			// ⚠️ **Pick the level by error type; do not log everything as Error.**
			//
			// This branch mixes two completely different things, whose owners and urgency are opposite:
			//
			//	precondition not met  the user/admin setup is one step short (he has not done Feishu user
			//	(ErrPrecondition)     authorization yet, the agent enabled it but picked no credential, the
			//	                      server has not connected some source). The platform works fine, **the
			//	                      error text already says what to do**, and that text goes into the
			//	                      conversation with err, so the person who sees it is the one who can fix it.
			//	real failure          network down while fetching credentials, platform 5xx, the database
			//	                      cannot be read. This needs the on-call person, not the user.
			//
			// Both used to log Error, so "some user has not authorized yet" got mixed into the
			// `_level=Error` list on every inspection round, posing as a platform failure that had to be
			// ruled out by hand (this came up again and again in an issue's comments). The value of Error
			// comes from "if it shows up, something is wrong". Mix in one thing the user can fix himself,
			// and the whole list starts being skipped as noise. That is the real cost: the next real failure
			// gets skipped along with it.
			//
			// The rule uses ErrPrecondition, an **existing** sentinel, instead of inventing a new category:
			// the addXxxEnvs functions in core/sessionenv.go already use it to mark "the setup is not done"
			// (each one there is `%w: ...ErrPrecondition`), while a real network failure while fetching
			// credentials goes through a plain fmt.Errorf without the sentinel. The same split already has a
			// precedent in core/credentialinject.go (the "setup not done" kind was lowered to Info);
			// here we just apply it to another source.
			//
			// ⚠️ **Both kinds are still logged, both with the original err; neither is swallowed**:
			// only the level is lowered, and searching by session id when debugging still finds every line.
			//
			if errors.Is(err, persistmodel.ErrPrecondition) {
				i.log.Warn("这个 agent 勾了某个授权，但它的凭据还没配好，"+
					"本会话少注入那一类变量（其余工具正常）；"+
					"按下面这句错误里说的去补一次即可，不用找平台",
					slog.String("ark_session_id", i.sessionID),
					slog.Any("err", err))
			} else {
				i.log.Error("取会话凭据失败：agent 勾了某个授权但拿不到它的凭据",
					slog.String("ark_session_id", i.sessionID),
					slog.Any("err", err))
			}
		}
		// Do not return: the source works as "partial success", and the parts already computed are
		// injected as usual.
	}
	if len(got) == 0 {
		return nil, nil, aliases
	}
	// Note: filtering of platform reserved keys is deliberately done at the source
	// (core/sessionenv.go, the second gate: the write path in agent.go is the first, and the platform
	// winning last in merge order is the third), so **there is no filtering here**. Reserved keys
	// returned by SessionEnvs are valid injections computed by the platform itself (AMA_TOKEN etc.);
	// deleting them by name here would wrongly remove the platform identity. That is exactly how the
	// incident described in the old enrich.go comments happened.
	return got, secretKeys, aliases
}

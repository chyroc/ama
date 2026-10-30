// PostgreSQL storage for the session-to-container mapping.
//
// Table structure and "why it must be in the database" are at the top of schema_sandbox_pg.sql.
// In short: the Ark session → container mapping used to live only in an in-process map, while HTTP requests land on any instance.
package coredao

import (
	"context"
	"fmt"
	"time"

	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/shared/infra/pg"
)

// GetRunSandbox gets the container used by a Run.
//
// Missing returns ErrNotFound, and the caller (RunSandboxState) turns it into none among the four states.
// **"Never had a container" is a normal result**; plain-text chats are like that.
func (s *PGStore) GetRunSandbox(ctx context.Context, runID string) (persistmodel.RunSandbox, error) {
	var rec persistmodel.RunSandbox
	err := s.client.QueryRow(ctx,
		`select run_id, session_id, sandbox_id, created_at
		   from run_sandbox_sessions where run_id = $1`, runID).
		Scan(&rec.RunID, &rec.SessionID, &rec.SandboxID, &rec.CreatedAt)
	if err != nil {
		if pg.IsNoRows(err) {
			return persistmodel.RunSandbox{}, persistmodel.ErrNotFound
		}
		return persistmodel.RunSandbox{}, fmt.Errorf("core: 查会话容器失败: %w", err)
	}
	return rec, nil
}

// PutRunSandbox overwrites "which container this Run uses".
//
// ⚠️ **`on conflict do update`, not `do nothing`.** The difference between these two words was a real incident:
// after the platform reclaims a container it gets rebuilt (TTL one hour, the IsNotFound branch of ensureRunning in pause.go),
// and then the same Run has a new container. With do nothing the table keeps the **old container id**,
// and that container no longer exists, so the page always shows "reclaimed" and nobody can see the rebuilt container,
// with **no error at all** (the write returns success).
func (s *PGStore) PutRunSandbox(ctx context.Context, rec persistmodel.RunSandbox) error {
	_, err := s.client.Exec(ctx,
		`insert into run_sandbox_sessions (run_id, session_id, sandbox_id, created_at)
		 values ($1, $2, $3, $4)
		 on conflict (run_id) do update
		    set session_id = excluded.session_id,
		        sandbox_id = excluded.sandbox_id,
		        created_at = excluded.created_at`,
		rec.RunID, rec.SessionID, rec.SandboxID, rec.CreatedAt)
	if err != nil {
		return fmt.Errorf("core: 写会话容器失败: %w", err)
	}
	return nil
}

// DeleteRunSandbox deletes one record; missing is not an error (idempotent).
func (s *PGStore) DeleteRunSandbox(ctx context.Context, runID string) error {
	if _, err := s.client.Exec(ctx,
		`delete from run_sandbox_sessions where run_id = $1`, runID); err != nil {
		return fmt.Errorf("core: 删会话容器失败: %w", err)
	}
	return nil
}

// TouchRunSandboxTerminal pushes the "terminal still in use" deadline back to now+ttl.
//
// ⚠️ **The time is computed by the database (`now() + $2`), not in Go and passed in.**
// Each instance's wall clock is off a bit, and the writer and reader are often different instances. If each side uses its own
// clock, the error turns directly into "should have been extended but was not" or "still extending long after nobody uses it". Letting the database be the only
// clock is the only choice that is clear-cut in a distributed setup.
//
// ⚠️ **One UPDATE, not read-modify-write.** This runs on every keystroke; with select then update,
// two concurrent keystrokes would overwrite each other (same as the comment on authdao.TouchAPIKey).
//
// When the record does not exist `Exec` affects 0 rows and returns nil, **which is what we want**: plain-text chats have no container record,
// so this path should not be reached then, and reaching it is not an error.
func (s *PGStore) TouchRunSandboxTerminal(ctx context.Context, runID string, ttl time.Duration) error {
	// Build the interval with make_interval(secs => $2), not by string-joining `$2 || ' seconds'`:
	// the latter relies on implicit type conversion, and pgx sends the parameter as text, so the joined value
	// may fail to parse as interval under some settings, a runtime error that only shows up in specific environments.
	if _, err := s.client.Exec(ctx,
		`update run_sandbox_sessions
		    set terminal_active_until = now() + make_interval(secs => $2)
		  where run_id = $1`,
		runID, ttl.Seconds()); err != nil {
		return fmt.Errorf("core: 续期终端占用失败: %w", err)
	}
	return nil
}

// TerminalActiveSessions returns the container session ids whose "terminal is still in use".
//
// `> now()` is compared by the database, for the same reason as TouchRunSandboxTerminal: do not fetch the time back into Go to compare.
// `terminal_active_until is null` (terminal never opened) naturally fails `> now()`, so no special case is needed.
func (s *PGStore) TerminalActiveSessions(ctx context.Context) ([]string, error) {
	rows, err := s.client.Query(ctx,
		`select session_id from run_sandbox_sessions
		  where terminal_active_until > now()`)
	if err != nil {
		return nil, fmt.Errorf("core: 查在用终端失败: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("core: 读在用终端失败: %w", err)
		}
		out = append(out, id)
	}
	// ⚠️ rows.Err() must not be skipped: errors in the middle of iteration only show up here. Without checking, a
	// "read half and got cut off" looks like "there are just this many", and one missing row here means pausing one more container that is in use.
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("core: 读在用终端失败: %w", err)
	}
	return out, nil
}

// TouchRunSandboxActivity sets the container's last activity time to the database's now().
//
// One UPDATE, time computed by the database, for the same reason as TouchRunSandboxTerminal. When the record does not exist it affects 0 rows
// and returns nil: plain-text chats have no container record, and that is not an error.
func (s *PGStore) TouchRunSandboxActivity(ctx context.Context, runID string) error {
	if _, err := s.client.Exec(ctx,
		`update run_sandbox_sessions set last_active_at = now() where run_id = $1`,
		runID); err != nil {
		return fmt.Errorf("core: 刷新容器活动时刻失败: %w", err)
	}
	return nil
}

// RunSandboxActivity fetches last activity times in bulk; those that never recorded activity fall back to created_at.
func (s *PGStore) RunSandboxActivity(ctx context.Context, runIDs []string) (map[string]time.Time, error) {
	out := make(map[string]time.Time, len(runIDs))
	if len(runIDs) == 0 {
		return out, nil
	}
	rows, err := s.client.Query(ctx,
		`select run_id, coalesce(last_active_at, created_at)
		   from run_sandbox_sessions where run_id = any($1)`, runIDs)
	if err != nil {
		return nil, fmt.Errorf("core: 查容器活动时刻失败: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var at time.Time
		if err := rows.Scan(&id, &at); err != nil {
			return nil, fmt.Errorf("core: 读容器活动时刻失败: %w", err)
		}
		out[id] = at
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("core: 读容器活动时刻失败: %w", err)
	}
	return out, nil
}

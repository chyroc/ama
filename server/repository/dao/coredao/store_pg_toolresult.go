// PostgreSQL storage for the self_hosted worker tool-result ledger.
//
// This layer only provides point lookups/reads/writes "with ctx + sessionID". The thin wrapper that adapts to the SDK's
// selfhosted.ToolResultStore interface (methods without ctx, unaware of session) is in
// the core package (core/pgtoolresult.go), built per work item and bound to the current session.
// Same approach as dockertools.ActivityLedger: "infra declares a narrow interface, coredao implements it, cmd wires it".
//
// Why not have a coredao type implement the SDK interface directly: the SDK's Recover/Begin/SaveResult/
// MarkSent signatures have no sessionID, but the ledger must be isolated per session (otherwise you get the poison-pill
// crosstalk of the local file ledger). The sessionID is known in the newWorker factory; the core adapter captures it and
// passes it here, so the DAO holds no mutable "current session".
package coredao

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Three ledger states; the string values match the SDK's FileToolResultStore, for easy comparison and debugging.
const (
	toolResultStateStarted = "started"
	toolResultStateResult  = "result"
	toolResultStateSent    = "sent"
)

// toolResultRow is the storage form of one ledger row. result_event is the raw event JSON.
type toolResultRow struct {
	CallID      string
	State       string
	ResultEvent []byte
}

// GetToolResultState reads the ledger state of one tool call.
//
// Missing returns ("", false, nil), so the caller can tell "never seen this call" from "database read error".
func (s *PGStore) GetToolResultState(
	ctx context.Context, sessionID, callID string,
) (state string, resultEvent []byte, found bool, err error) {
	row := s.client.QueryRow(ctx,
		`select state, result_event from selfhost_tool_results
		  where session_id = $1 and call_id = $2`, sessionID, callID)
	var ev []byte
	if err := row.Scan(&state, &ev); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil, false, nil
		}
		return "", nil, false, fmt.Errorf("读工具结果账本失败: %w", err)
	}
	return state, ev, true, nil
}

// BeginToolResult marks a call as "started".
//
// It inserts started only if the row does not exist yet; if it exists it does nothing (returns the current row). This is idempotent:
// after a reclaim reassigns the work, another instance calling Begin on the same call must not overwrite a stored result back to started.
func (s *PGStore) BeginToolResult(ctx context.Context, sessionID, callID string) error {
	_, err := s.client.Exec(ctx,
		`insert into selfhost_tool_results (session_id, call_id, state, result_event)
		 values ($1, $2, $3, '{}'::jsonb)
		 on conflict (session_id, call_id) do nothing`,
		sessionID, callID, toolResultStateStarted)
	if err != nil {
		return fmt.Errorf("写工具开始状态失败: %w", err)
	}
	return nil
}

// MarkToolResultResult sets a call to the result state and stores the event to resend.
//
// Uses upsert: a started row is upgraded to result; if the row already exists (for example a race between the started insert and
// this one), the result is still written. The result state carries more information than started, so overwriting is safe.
func (s *PGStore) MarkToolResultResult(
	ctx context.Context, sessionID, callID string, resultEvent []byte,
) error {
	if !json.Valid(resultEvent) {
		// Should not happen (the event is built by the SDK), but block it before writing to the database so one bad JSON row does not break all of Recover.
		return fmt.Errorf("工具结果事件不是合法 JSON，拒绝落库")
	}
	_, err := s.client.Exec(ctx,
		`insert into selfhost_tool_results (session_id, call_id, state, result_event)
		 values ($1, $2, $3, $4)
		 on conflict (session_id, call_id)
		 do update set state = excluded.state,
		               result_event = excluded.result_event,
		               updated_at = now()`,
		sessionID, callID, toolResultStateResult, string(resultEvent))
	if err != nil {
		return fmt.Errorf("写工具结果失败: %w", err)
	}
	return nil
}

// MarkToolResultSent sets a call to the sent state (the result was delivered back to Ark).
func (s *PGStore) MarkToolResultSent(ctx context.Context, sessionID, callID string) error {
	_, err := s.client.Exec(ctx,
		`update selfhost_tool_results set state = $3, updated_at = now()
		  where session_id = $1 and call_id = $2`,
		sessionID, callID, toolResultStateSent)
	if err != nil {
		return fmt.Errorf("写工具已回传状态失败: %w", err)
	}
	return nil
}

// ListToolResultsForRecover lists all ledger rows of a session that are "not confirmed delivered"
// (started / result states), used to recover when a worker takes over that session.
//
// ⚠️ **The session_id in the query is the whole reason this table exists**: only calls of the current session are recovered,
// never fishing out and flushing leftovers of other sessions like the local file ledger did (poison-pill crosstalk).
func (s *PGStore) ListToolResultsForRecover(
	ctx context.Context, sessionID string,
) (pending map[string][]byte, sent map[string]bool, err error) {
	rows, qerr := s.client.Query(ctx,
		`select call_id, state, result_event from selfhost_tool_results
		  where session_id = $1 and state <> $2`, sessionID, toolResultStateSent)
	if qerr != nil {
		return nil, nil, fmt.Errorf("恢复工具结果账本失败: %w", qerr)
	}
	defer rows.Close()

	pending = map[string][]byte{}
	sent = map[string]bool{}
	for rows.Next() {
		var r toolResultRow
		if scanErr := rows.Scan(&r.CallID, &r.State, &r.ResultEvent); scanErr != nil {
			return nil, nil, fmt.Errorf("读工具结果账本行失败: %w", scanErr)
		}
		switch r.State {
		case toolResultStateResult:
			// Finished running, delivery of the result not confirmed: the new owner resends this result directly without rerunning the tool.
			pending[r.CallID] = append([]byte(nil), r.ResultEvent...)
		case toolResultStateStarted:
			// Killed while "started, unknown whether it finished": mark as sent so the upper layer takes the branch "do not run again,
			// return an unknown result" (same behavior as the SDK's FileToolResultStore).
			sent[r.CallID] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("遍历工具结果账本失败: %w", err)
	}
	return pending, sent, nil
}

// DeleteOldToolResults deletes ledger rows with updated_at older than olderThan (cleans up delivered/zombie leftovers).
func (s *PGStore) DeleteOldToolResults(ctx context.Context, olderThan time.Time) (int64, error) {
	res, err := s.client.Exec(ctx,
		`delete from selfhost_tool_results where updated_at < $1`, olderThan)
	if err != nil {
		return 0, fmt.Errorf("清理工具结果账本失败: %w", err)
	}
	return res.RowsAffected()
}

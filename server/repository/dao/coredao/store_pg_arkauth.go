// PostgreSQL storage for the start time of consecutive Ark credential failures. Table structure and reasons in schema/ark_auth_failures.sql.
package coredao

import (
	"context"
	"fmt"
	"time"
)

// RecordArkAuthFailure records one auth failure and returns how long this run of consecutive failures has lasted.
//
// ⚠️ **One upsert, not read-then-write.** Failures for the same environment can happen on several instances at once
// (instances that are not the leader also start workers and hit the same 401), so read-modify-write would overwrite each other's first_failed_at.
//
// If the fingerprint changed (the user switched keys), the start is reset to now(): a new key starts the clock over.
// "How long" is computed by the database in the same statement, not compared against Go's wall clock (see the table file header).
func (s *PGStore) RecordArkAuthFailure(
	ctx context.Context, environmentID, workspaceID, fingerprint string,
) (time.Duration, error) {
	var secs float64
	err := s.client.QueryRow(ctx,
		`insert into ark_auth_failures
		     (environment_id, workspace_id, key_fingerprint, first_failed_at, last_failed_at, failures)
		 values ($1, $2, $3, now(), now(), 1)
		 on conflict (environment_id) do update
		    set workspace_id    = excluded.workspace_id,
		        first_failed_at = case when ark_auth_failures.key_fingerprint = excluded.key_fingerprint
		                               then ark_auth_failures.first_failed_at else now() end,
		        failures        = case when ark_auth_failures.key_fingerprint = excluded.key_fingerprint
		                               then ark_auth_failures.failures + 1 else 1 end,
		        key_fingerprint = excluded.key_fingerprint,
		        last_failed_at  = now()
		 returning extract(epoch from now() - first_failed_at)::float8`,
		environmentID, workspaceID, fingerprint).Scan(&secs)
	if err != nil {
		return 0, fmt.Errorf("core: 记方舟凭据失效失败: %w", err)
	}
	return time.Duration(secs * float64(time.Second)), nil
}

// ClearArkAuthFailure deletes an environment's failure record. Missing is not an error (idempotent).
func (s *PGStore) ClearArkAuthFailure(ctx context.Context, environmentID string) error {
	if _, err := s.client.Exec(ctx,
		`delete from ark_auth_failures where environment_id = $1`, environmentID); err != nil {
		return fmt.Errorf("core: 清方舟凭据失效记录失败: %w", err)
	}
	return nil
}

// Two maintenance methods for e2e: restore the seed, and clear all data.
//
// # Why they did not move with the run domain
//
// They used to live at the very end of core/ark.go (under the "for e2e" banner), right next to
// the run history section. **They were neighbors by position, not the same domain.**
//
// The test is "which tables do they touch": `Reset` restores the **whole database** to the seed,
// and `Clear` wipes **all** data, covering workspaces / members / agents / skills / credentials /
// runs ... every domain. Moving them into service/run would give that read-only run history
// domain a method that can delete everyone's data, while it does not even have write methods for
// its own table (see "deliberately has no CreateRun / SaveRun" in service/run/store.go).
//
// We also did not create a separate "maintenance domain": it would be an empty package with two
// pass-throughs, and these two methods only hand the call straight to Store. **They have no
// business logic to move.**
//
// ⚠️ So the "for e2e" declarations in core/store.go also stay where they are and are not moved out.
package core

import "context"

// Reset restores the data to the seed. Matches window.__amaApi.reset() in the frontend.
func (s *Service) Reset(ctx context.Context) error { return s.store.Reset(ctx) }

// Clear deletes all data. Matches window.__amaApi.clear() in the frontend.
func (s *Service) Clear(ctx context.Context) error { return s.store.Clear(ctx) }

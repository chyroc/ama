package core

import (
	"context"
	"log/slog"
	"strings"

	"github.com/chyroc/ama/server/model/apimodel"
)

// UserProfileLookup is the "look up display name and avatar for a batch of emails" ability this
// module needs.
//
// **The interface is declared on the consumer side** (same as the DepartmentLookup /
// DirectorySearcher precedents). The implementation is provided in cmd/amad by
// authdao.PGStore. core does not import authdao, since that would tie the "six resource
// domains" to the auth storage.
//
// The rules are exactly the same as auth.Store.ListUsers:
//   - the returned map is keyed by **lowercase email**;
//   - **emails that are not found are left out of the map, and that is not an error** (it is
//     normal that the person has never logged in).
type UserProfileLookup interface {
	ListUsers(ctx context.Context, emails []string) (map[string]apimodel.UserProfile, error)
}

// SetUserProfileLookup wires in the user profile lookup.
//
// It is a setter instead of a Config field, same as the SetSandboxAttacher precedent: the
// Service is built before authStore (see cmd/amad). **It also runs without it**: then avatars
// always fall back to initials.
func (s *Service) SetUserProfileLookup(l UserProfileLookup) {
	s.profiles = l
}

// ResolveUserProfiles looks up display names and avatars for a batch of emails.
//
// # Why this is a separate API instead of having listAgents return it
//
// Three reasons. The first one decides it:
//
//  1. **agents are stored in full as a doc jsonb**. If name/avatar were put on apimodel.Agent,
//     they would be written into the database along with the doc, so "the name at the moment
//     the agent was created" would be frozen forever. After that person changes their name or
//     avatar, the list still shows the old one, with no error at all.
//     This is exactly why WorkspaceMember has a stale snapshot. There it was forced by the
//     upstream API (no entry point to batch-look-up by email), **but here there is no such
//     limit**.
//  2. The same ability has four callers (creator in the agent list, member list, issue author,
//     scheduled task creator). If it hung off the agent response, the other three would each
//     need it added again.
//  3. A broken avatar or an unreachable users table should not stop the whole agent list from
//     opening. With them separate, the frontend can draw the list first and let avatars load
//     on their own.
//
// # Degrade instead of failing
//
// When lookup is nil (memory store, or authdao not wired locally) it returns an empty map. When
// a caller gets no profile it falls back to the email's first letter, and the feature still
// works fully. **Do not return an error here**: that would let a purely decorative piece of
// information take down the whole page.
func (s *Service) ResolveUserProfiles(
	ctx context.Context, emails []string,
) (map[string]apimodel.UserProfile, error) {
	out := make(map[string]apimodel.UserProfile)
	if s.profiles == nil || len(emails) == 0 {
		return out, nil
	}
	// Normalize once here: callers may pass emails exactly as stored in the doc,
	// while the users table keys are lowercase (see auth.normalizeEmail). A case mismatch
	// **silently finds nothing**: the avatar always falls back to initials, which looks like
	// "this person has never logged in".
	keys := make([]string, 0, len(emails))
	for _, e := range emails {
		if key := strings.ToLower(strings.TrimSpace(e)); key != "" {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return out, nil
	}

	found, err := s.profiles.ListUsers(ctx, keys)
	if err != nil {
		// Degrade: no profile just means no avatar; it should not fail the caller.
		//
		// ⚠️ Use s.log() instead of s.logger: **the latter can be nil** (when Config.Logger
		// is empty), and using it directly would panic on this fallback path. That would turn
		// "the upstream lookup failed" from "a few avatars missing" into "the whole request
		// returns 500", which is exactly what this code is meant to prevent.
		s.log().WarnContext(ctx, "查用户档案失败，头像走首字母兜底",
			slog.String("error", err.Error()), slog.Int("count", len(keys)))
		return out, nil
	}
	return found, nil
}

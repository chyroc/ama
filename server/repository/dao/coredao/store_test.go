// Tests for Store, **run once on each implementation**.
//
// The point is not "what is stored can be read back", but the behaviors that are **easy to get wrong and give no error when wrong**:
// whether deep copies really isolate internal state, whether slices are non-nil, whether cascading deletes clean everything,
// whether filtering and sorting match the frontend's LocalApiClient.
//
// # Why parameterized
//
// These assertions are the contract of the Store interface, not details of one implementation. The in-memory implementation is **the reference behavior**, and the PG implementation
// must match it case by case. So the tests are written once and runStoreSuite runs them once on each implementation, instead of copying
// and tweaking. Copies would drift apart slowly, and the drift goes exactly in the direction of "the PG copy gets changed to its current
// behavior", at which point the tests lose their meaning.
//
// The PG run is skipped entirely with t.Skip when the database is unreachable (see store_pg_test.go), so `go test ./...`
// still runs offline for people without containers.

package coredao

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/chyroc/ama/server/repository/dao/coremem"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chyroc/ama/server/core"
	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// storeFactory builds a Store instance. When withSeed is true, seed data is loaded.
//
// Each call must give an instance **that does not interfere with others**: a new object for the in-memory version, cleared and rebuilt for PG,
// otherwise tests leak into each other.
type storeFactory func(t *testing.T, withSeed bool) core.Store

// memFactory is the factory of the in-memory implementation, and also the reference behavior.
func memFactory(t *testing.T, withSeed bool) core.Store {
	t.Helper()
	if !withSeed {
		return coremem.NewMemStore()
	}
	s, err := coremem.NewSeededMemStore()
	if err != nil {
		t.Fatalf("装载种子失败：%v", err)
	}
	return s
}

// TestStoreSuite runs the whole set of contract tests once on each implementation.
//
// The PG run is provided by pgFactory (store_pg_test.go), which calls t.Skip when the database is unreachable.
func TestStoreSuite(t *testing.T) {
	for _, impl := range []struct {
		name string
		make storeFactory
	}{
		{"MemStore", memFactory},
		{"PGStore", pgFactory},
	} {
		t.Run(impl.name, func(t *testing.T) {
			runStoreSuite(t, impl.make)
		})
	}
}

// runStoreSuite is all the contract tests. Each subtest gets a clean instance.
func runStoreSuite(t *testing.T, newStore storeFactory) {
	t.Run("SeedLoads", func(t *testing.T) { testSeedLoads(t, newStore) })
	t.Run("ReadReturnsCopy", func(t *testing.T) { testReadReturnsCopy(t, newStore) })
	t.Run("SlicesNeverNil", func(t *testing.T) { testSlicesNeverNil(t, newStore) })
	t.Run("ListRunEventsUnknownRun", func(t *testing.T) { testListRunEventsUnknownRun(t, newStore) })
	t.Run("EventFilters", func(t *testing.T) { testEventFilters(t, newStore) })
	t.Run("EventRawIsByteExact", func(t *testing.T) { testEventRawIsByteExact(t, newStore) })
	t.Run("ListRunsFilters", func(t *testing.T) { testListRunsFilters(t, newStore) })
	t.Run("ListRunsByScheduleID", func(t *testing.T) { testListRunsByScheduleID(t, newStore) })
	t.Run("ListRunsByTrigger", func(t *testing.T) { testListRunsByTrigger(t, newStore) })
	t.Run("ListRunsPagination", func(t *testing.T) { testListRunsPagination(t, newStore) })
	t.Run("ListRunsOrdersByLastActivity", func(t *testing.T) {
		testListRunsOrdersByLastActivity(t, newStore)
	})
	t.Run("ListRunsForUsageRange", func(t *testing.T) { testListRunsForUsageRange(t, newStore) })
	t.Run("DeleteWorkspaceCascades", func(t *testing.T) { testDeleteWorkspaceCascades(t, newStore) })
	t.Run("DeletesAreIdempotent", func(t *testing.T) { testDeletesAreIdempotent(t, newStore) })
	t.Run("WorkspaceKeyConflict", func(t *testing.T) { testWorkspaceKeyConflict(t, newStore) })
	t.Run("MemberOrderIsStable", func(t *testing.T) { testMemberOrderIsStable(t, newStore) })
	t.Run("MemberRoleUpdateKeepsOrder", func(t *testing.T) { testMemberRoleUpdateKeepsOrder(t, newStore) })
	t.Run("MemberFieldsSurviveRoundTrip", func(t *testing.T) {
		testMemberFieldsSurviveRoundTrip(t, newStore)
	})
	// Round trip of the workspace's own name / createdBy / updatedBy. **It must run once on each implementation**:
	// on the PG side these three values are now **written into both doc and separate columns** (step one of splitting doc), while the in-memory version
	// stores the whole struct: two completely different mechanisms. Checking only the in-memory version, a column missed on the PG side
	// goes unwatched, and since the read path still uses doc in this round, a miss would **still be all green**.
	t.Run("WorkspaceOwnFieldsSurviveRoundTrip", func(t *testing.T) {
		testWorkspaceOwnFieldsSurviveRoundTrip(t, newStore)
	})
	t.Run("KeyTakenExceptID", func(t *testing.T) { testKeyTakenExceptID(t, newStore) })
	t.Run("SaveMissingReturnsNotFound", func(t *testing.T) { testSaveMissingReturnsNotFound(t, newStore) })
	t.Run("AgentsBoundTo", func(t *testing.T) { testAgentsBoundTo(t, newStore) })

	// Agent Ark sync tasks. ⚠️ The ClaimIsAtomic one only means something on real PG
	// (MemStore cannot hit the two pitfalls of EPQ recheck and skip locked).
	t.Run("AgentSyncEnqueueDedups", func(t *testing.T) { testAgentSyncEnqueueDedups(t, newStore) })
	t.Run("AgentSyncResyncMarkedWhileRunning", func(t *testing.T) {
		testAgentSyncResyncMarkedWhileRunning(t, newStore)
	})
	t.Run("AgentSyncZombieReclaim", func(t *testing.T) { testAgentSyncZombieReclaim(t, newStore) })
	t.Run("AgentSyncClaimIsAtomic", func(t *testing.T) { testAgentSyncClaimIsAtomic(t, newStore) })
	t.Run("AgentSyncClaimLimit", func(t *testing.T) { testAgentSyncClaimLimit(t, newStore) })
	t.Run("AgentSyncFieldsSurviveRoundTrip", func(t *testing.T) {
		testAgentSyncFieldsSurviveRoundTrip(t, newStore)
	})
	t.Run("AgentSyncLatestPicksNewest", func(t *testing.T) { testAgentSyncLatestPicksNewest(t, newStore) })
	t.Run("AgentSyncLatestMissingIsNotFound", func(t *testing.T) {
		testAgentSyncLatestMissingIsNotFound(t, newStore)
	})
	t.Run("AgentSyncRequeueClearsLease", func(t *testing.T) { testAgentSyncRequeueClearsLease(t, newStore) })
	t.Run("AgentSyncPurgeRemovesOnlyFinished", func(t *testing.T) {
		testAgentSyncPurgeRemovesOnlyFinished(t, newStore)
	})
	t.Run("AgentsBoundToOrder", func(t *testing.T) { testAgentsBoundToOrder(t, newStore) })
	t.Run("SkillNameTaken", func(t *testing.T) { testSkillNameTaken(t, newStore) })
	t.Run("SkillFilesRoundTrip", func(t *testing.T) { testSkillFilesRoundTrip(t, newStore) })
	t.Run("GetFilesBatch", func(t *testing.T) { testGetFilesBatch(t, newStore) })
	// Issues, implemented in issue_store_test.go. **It must run once on each implementation**:
	// ordering, counts and byte isolation in this domain are implemented by different mechanisms on the two sides
	// (PG relies on SQL order by / subqueries / not selecting bytea, the in-memory version on Go code);
	// checking only one side leaves drift on the other unwatched, and unit tests run exactly the in-memory version.
	t.Run("IssueRoundTrip", func(t *testing.T) { testIssueRoundTrip(t, newStore) })
	t.Run("IssueListOrderAndFilter", func(t *testing.T) { testIssueListOrderAndFilter(t, newStore) })
	t.Run("IssueCountsAreDerived", func(t *testing.T) { testIssueCountsAreDerived(t, newStore) })
	t.Run("IssueDeleteCascades", func(t *testing.T) { testIssueDeleteCascades(t, newStore) })
	t.Run("IssueAttachmentMetadata", func(t *testing.T) {
		testIssueAttachmentMetadata(t, newStore)
	})
	t.Run("IssueCommentsAreOrdered", func(t *testing.T) { testIssueCommentsAreOrdered(t, newStore) })
	// Issues produced by bots are deduped by origin + fingerprint, while human-written ones are never deduped.
	// **Both sides must run it**: the PG side relies on a partial unique index, the in-memory version on a piece of Go logic,
	// two completely different mechanisms. Checking only the in-memory version, "the where of that PG index is wrong"
	// (for example missing `origin <> ''`, so human-written issues get pulled into dedup too) goes unwatched.
	t.Run("IssueBotFingerprintDedup", func(t *testing.T) {
		testIssueBotFingerprintDedup(t, newStore)
	})
	t.Run("DeleteWorkspaceCascadesIssues", func(t *testing.T) {
		testDeleteWorkspaceCascadesIssues(t, newStore)
	})
	// Product suggestions, implemented in suggestion_store_test.go.
	// **It must run once on each implementation**: ordering, counts, byte isolation and paging are implemented on the two sides by
	// completely different mechanisms (PG relies on SQL, the in-memory version on Go code).
	//
	// AuthorFilterAppliesBeforePaging is especially important: it guards against
	// "cut the page first, then filter by author", with which users see an empty list while the endpoint returns 200.
	t.Run("SuggestionRoundTrip", func(t *testing.T) { testSuggestionRoundTrip(t, newStore) })
	t.Run("SuggestionListOrderAndFilter", func(t *testing.T) {
		testSuggestionListOrderAndFilter(t, newStore)
	})
	t.Run("SuggestionAuthorFilterAppliesBeforePaging", func(t *testing.T) {
		testSuggestionAuthorFilterAppliesBeforePaging(t, newStore)
	})
	t.Run("SuggestionPaging", func(t *testing.T) { testSuggestionPaging(t, newStore) })
	t.Run("SuggestionCountsAreDerived", func(t *testing.T) {
		testSuggestionCountsAreDerived(t, newStore)
	})
	t.Run("SuggestionRepliesAreOrdered", func(t *testing.T) {
		testSuggestionRepliesAreOrdered(t, newStore)
	})
	t.Run("SuggestionReplyStaffFlagPersists", func(t *testing.T) {
		testSuggestionReplyStaffFlagPersists(t, newStore)
	})
	t.Run("SuggestionAttachmentMetadata", func(t *testing.T) {
		testSuggestionAttachmentMetadata(t, newStore)
	})
	t.Run("SuggestionDeleteCascades", func(t *testing.T) {
		testSuggestionDeleteCascades(t, newStore)
	})
	t.Run("SuggestionSaveDoesNotChangeAuthor", func(t *testing.T) {
		testSuggestionSaveDoesNotChangeAuthor(t, newStore)
	})
	t.Run("SuggestionsAreNeverDeduped", func(t *testing.T) {
		testSuggestionsAreNeverDeduped(t, newStore)
	})
	t.Run("ArkSkillUploadRoundTrip", func(t *testing.T) { testArkSkillUploadRoundTrip(t, newStore) })
	t.Run("ArkSecretRoundTrip", func(t *testing.T) { testArkSecretRoundTrip(t, newStore) })
	// Entity history versions, implemented in revision_test.go. **All four must run once each on Mem and PG**:
	// the two implementations work completely differently (PG is unique constraint + window function, memory is a slice walk),
	// and checking only the in-memory version would never execute those PG SQLs once, while they are exactly the parts easiest to get wrong.
	t.Run("RevisionRoundTrip", func(t *testing.T) { testRevisionRoundTrip(t, newStore) })
	t.Run("RevisionNumbersAreSequential", func(t *testing.T) {
		testRevisionNumbersAreSequential(t, newStore)
	})
	t.Run("RevisionListOmitsSnapshot", func(t *testing.T) {
		testRevisionListOmitsSnapshot(t, newStore)
	})
	t.Run("RevisionPurgeKeepsNewestPerEntity", func(t *testing.T) {
		testRevisionPurgeKeepsNewestPerEntity(t, newStore)
	})
	t.Run("ActorFieldsRoundTrip", func(t *testing.T) { testActorFieldsRoundTrip(t, newStore) })
	// Onboarding, implemented in onboarding_test.go. Both must run once each on Mem and PG:
	// the invariant "never onboard twice" is finally guaranteed by PG's unique constraint, and the in-memory implementation cannot show the behavior difference
	// after on conflict is removed.
	t.Run("OnboardingPersistsCompletion", func(t *testing.T) {
		testOnboardingPersistsCompletion(t, newStore)
	})
	t.Run("OnboardingPersistsSkip", func(t *testing.T) { testOnboardingPersistsSkip(t, newStore) })
	t.Run("OnboardingIsPerAccount", func(t *testing.T) { testOnboardingIsPerAccount(t, newStore) })
	t.Run("OnboardingMergesBothTimestamps", func(t *testing.T) {
		testOnboardingMergesBothTimestamps(t, newStore)
	})
	// User preferences, implemented in preferences_test.go. **It must run once on each implementation**:
	// both sides must upsert the same way, and this group is **deliberately the opposite** of the onboarding one (overwrite the whole row
	// vs merge). Checking only one side, if someone "unifies" things one day by copying the coalesce over,
	// the in-memory version still overwrites while the PG version merges, so "change back to follow system" is lost in production
	// but not under AMA_STORE=mem, and neither side errors.
	t.Run("UserPreferencesRoundTrip", func(t *testing.T) {
		testUserPreferencesRoundTrip(t, newStore)
	})
	t.Run("UserPreferencesOverwrites", func(t *testing.T) {
		testUserPreferencesOverwrites(t, newStore)
	})
	t.Run("UserPreferencesIsPerAccount", func(t *testing.T) {
		testUserPreferencesIsPerAccount(t, newStore)
	})
	// Issue feedback, implemented in feedback_store_test.go. **It must run once on each implementation**:
	// rate limiting is finally guaranteed by PG's `on conflict (dedup_key)`, while unit tests run the in-memory version;
	// checking only one side, "PG fixed but the in-memory version keeps piling up duplicates" or the other way around
	// would make AMA_STORE=mem and production behave differently, and that inconsistency has no symptom at all.
	t.Run("FeedbackDedupesAutoLevel", func(t *testing.T) { testFeedbackDedupesAutoLevel(t, newStore) })
	t.Run("FeedbackKeepsUserLevel", func(t *testing.T) { testFeedbackKeepsUserLevel(t, newStore) })
	t.Run("FeedbackLogIDRoundTrips", func(t *testing.T) { testFeedbackLogIDRoundTrips(t, newStore) })
	t.Run("FeedbackListOrderAndPaging", func(t *testing.T) {
		testFeedbackListOrderAndPaging(t, newStore)
	})
	t.Run("FeedbackFilters", func(t *testing.T) { testFeedbackFilters(t, newStore) })
	t.Run("ResetAndClear", func(t *testing.T) { testResetAndClear(t, newStore) })
	t.Run("ConcurrentAccess", func(t *testing.T) { testConcurrentAccess(t, newStore) })
	t.Run("CreateAndSaveRun", func(t *testing.T) { testCreateAndSaveRun(t, newStore) })
	t.Run("AppendRunEventSequences", func(t *testing.T) { testAppendRunEventSequences(t, newStore) })
	t.Run("AppendRunEventKeepsBytes", func(t *testing.T) { testAppendRunEventKeepsBytes(t, newStore) })
	t.Run("AppendRunEventDedupesByID", func(t *testing.T) { testAppendRunEventDedupesByID(t, newStore) })
	t.Run("ConcurrentAppendRunEvent", func(t *testing.T) { testConcurrentAppendRunEvent(t, newStore) })
	t.Run("ConcurrentAppendSameEventID", func(t *testing.T) { testConcurrentAppendSameEventID(t, newStore) })
	// Reclaiming stuck conversations. **Both implementations must run it**: PG relies on a SQL predicate (with jsonb extraction and
	// coalesce fallback), the in-memory version on Go logic, two completely different mechanisms, and drift results in
	// "unit tests all green, zombies keep piling up in production".
	//
	// The three **must be paired**; missing any one lets a wrong implementation be all green:
	//   - only Lists  → "list everything blindly" also passes, and that would reclaim running conversations too;
	//   - only Skips  → "list nothing" also passes, as if the feature were never built;
	//   - without the CAS one → a "read then write" implementation also passes, and it would kill a conversation just revived by a late
	//     tool_result again (zero errors; the database only shows one terminated row).
	t.Run("ListsStaleActiveRuns", func(t *testing.T) { testListsStaleActiveRuns(t, newStore) })
	t.Run("RetireStaleRunIsConditional", func(t *testing.T) { testRetireStaleRunIsConditional(t, newStore) })
	t.Run("RetireStaleRunIsAtomic", func(t *testing.T) { testRetireStaleRunIsAtomic(t, newStore) })
	// Ark bindings, implemented in store_arkbinding_test.go.
	t.Run("RepointArkEnvironment", func(t *testing.T) { testRepointArkEnvironment(t, newStore) })
	// Start of consecutive Ark credential failures, implemented in store_arkauth_test.go.
	t.Run("ArkAuthFailureSince", func(t *testing.T) { testArkAuthFailureSince(t, newStore) })
	t.Run("RunSandboxActivity", func(t *testing.T) { testRunSandboxActivity(t, newStore) })
	// Scheduled tasks, implemented in store_schedule_test.go.
	t.Run("ScheduleCRUD", func(t *testing.T) { testScheduleCRUD(t, newStore) })
	t.Run("ClaimOnlyDueAndActive", func(t *testing.T) { testClaimOnlyDueAndActive(t, newStore) })
	t.Run("ClaimRespectsLeaseExpiry", func(t *testing.T) { testClaimRespectsLeaseExpiry(t, newStore) })
	// These two **must be paired**: the first checks "zombies can be rescued", the second "unscheduled ones are not mistaken for zombies".
	// With only the first, loosening the reclaim condition to `next_run_at is null` would also be all green,
	// and that would make every paused/unscheduled task get claimed and run once.
	t.Run("ClaimReclaimsZombieAfterLeaseExpiry", func(t *testing.T) {
		testClaimReclaimsZombieAfterLeaseExpiry(t, newStore)
	})
	t.Run("ClaimDoesNotStealNeverClaimedSchedules", func(t *testing.T) {
		testClaimDoesNotStealNeverClaimedSchedules(t, newStore)
	})
	t.Run("ClaimIsAtomicUnderConcurrency", func(t *testing.T) { testClaimIsAtomicUnderConcurrency(t, newStore) })
	// Queued messages waiting to be sent. **These three must run on both implementations**: MemStore all green while PG behaves differently
	// is a pattern this repo has hit again and again, and drift in this group means "the same message is sent twice".
	t.Run("PendingMessagesAreFIFO", func(t *testing.T) { testPendingFIFO(t, newStore) })
	t.Run("PendingClaimIsAtomic", func(t *testing.T) { testPendingClaimIsAtomic(t, newStore) })
	t.Run("PendingFailedIsNotClaimable", func(t *testing.T) { testPendingFailedNotClaimable(t, newStore) })
	t.Run("ClaimLimit", func(t *testing.T) { testClaimLimit(t, newStore) })
	// The manual try-run branch: paused can be claimed too, but **only after try-run was clicked**. The two assertions are paired;
	// see the comment on testClaimPausedOnlyWhenRunNowRequested.
	t.Run("ClaimPausedOnlyWhenRunNowRequested", func(t *testing.T) {
		testClaimPausedOnlyWhenRunNowRequested(t, newStore)
	})
	t.Run("ClaimRunNowStillNeedsDueTime", func(t *testing.T) {
		testClaimRunNowStillNeedsDueTime(t, newStore)
	})
	t.Run("ScheduleRunNowFlagRoundTrips", func(t *testing.T) {
		testScheduleRunNowFlagRoundTrips(t, newStore)
	})
	t.Run("ReleaseClaimAllowsReclaim", func(t *testing.T) { testReleaseClaimAllowsReclaim(t, newStore) })
	t.Run("SchedulesForAgent", func(t *testing.T) { testSchedulesForAgent(t, newStore) })
	t.Run("DeleteWorkspaceCascadesSchedules", func(t *testing.T) { testDeleteWorkspaceCascadesSchedules(t, newStore) })
	// The authorization query (ListAuthzRoles). **This group especially must run once on each implementation**:
	// in production authorization runs the PG SQL; if it drifts from mem, "unit tests green, wrong authorization in production".
	t.Run("AuthzRolesIsCaseInsensitive", func(t *testing.T) {
		testAuthzRolesIsCaseInsensitive(t, newStore)
	})
	t.Run("AuthzRolesUnknownWorkspaceIsEmpty", func(t *testing.T) {
		testAuthzRolesUnknownWorkspaceIsEmpty(t, newStore)
	})
	t.Run("AuthzRolesScopedToWorkspace", func(t *testing.T) {
		testAuthzRolesScopedToWorkspace(t, newStore)
	})

	// The three host executor tables (service/host), implemented in store_host_test.go. **It must run once on each
	// implementation**: claiming relies on CTE + for update skip locked in PG and on a write lock in mem;
	// nullable time columns (NULL vs zero value) and seq assignment are two mechanisms too; checking only one side is the same as not checking.
	t.Run("HostRegistration", func(t *testing.T) { testHostRegistration(t, newStore) })
	t.Run("HostHeartbeatAndOffline", func(t *testing.T) {
		testHostHeartbeatAndOffline(t, newStore)
	})
	t.Run("WorkspaceHostBinding", func(t *testing.T) { testWorkspaceHostBinding(t, newStore) })
	t.Run("HostDispatchQueue", func(t *testing.T) { testHostDispatchQueue(t, newStore) })
	t.Run("HostFailOnlyExecsAndPurge", func(t *testing.T) {
		testHostFailOnlyExecsAndPurge(t, newStore)
	})
	t.Run("HostDispatchSeqSurvivesPurge", func(t *testing.T) {
		testHostDispatchSeqSurvivesPurge(t, newStore)
	})
	t.Run("AgentExecutorColumnsRoundTrip", func(t *testing.T) {
		testAgentExecutorColumnsRoundTrip(t, newStore)
	})

	// Account-level personal files (user_files). Owner isolation, CAS to completed, and descending list order must be checked on both
	// MemStore and PGStore (unique key conflicts only really happen on PG).
	t.Run("UserFileLifecycle", func(t *testing.T) { testUserFileLifecycle(t, newStore) })
	t.Run("UserFileOwnerIsolation", func(t *testing.T) {
		testUserFileOwnerIsolation(t, newStore)
	})
	t.Run("UserFileListOrderAndLimit", func(t *testing.T) {
		testUserFileListOrderAndLimit(t, newStore)
	})
	t.Run("UserFileUniqueObjectKey", func(t *testing.T) {
		testUserFileUniqueObjectKey(t, newStore)
	})
}

// newConversationRun builds a run in the shape of a conversation, for the tests below.
func newConversationRun(id string) apimodel.Run {
	return apimodel.Run{
		ID:          id,
		WorkspaceID: "ws_demo",
		AgentID:     "agent_triage",
		AgentName:   "工单分诊助手",
		Title:       "",
		Status:      apimodel.RunIdle,
		Trigger:     apimodel.TriggerManual,
		StartedAt:   apimodel.NowISO(),
		Usage:       apimodel.TokenUsage{},
		Threads:     []apimodel.RunThread{},
		EventCount:  0,
	}
}

// testListRunsOrdersByLastActivity: a reused conversation that gets a new message must float to the top of the list.
//
// # Which real failure this test guards against
//
// Feishu reuses conversations by scope (feishu/scope.go), so the Nth DM turn is still appended to the first turn's Run,
// and **startedAt does not change**. ListRuns used to sort only by startedAt descending, so right after the user said something in Feishu,
// that conversation still sat at its ten-hours-ago position in the web conversation list, showing as "messages sent in Feishu
// cannot be found on the web", while the messages were in the database all along. Sorting by startedAt would fail the first assertion below.
//
// It also pins the fallback: old data without lastActivityAt takes part in sorting by startedAt, and both stores
// must give the same order (the PG side uses coalesce(nullif(...)), the Mem side lastActivityOf).
func testListRunsOrdersByLastActivity(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, false)
	const ws = "ws_demo"

	// old is created first and active later (simulating an old conversation reused by Feishu); fresh is created later with no active time (the old data shape).
	old := newConversationRun("sesn-old")
	old.StartedAt = "2026-08-27T00:00:00.000Z"
	old.LastActivityAt = "2026-08-27T10:00:00.000Z"
	if _, err := s.CreateRun(ctx, old); err != nil {
		t.Fatalf("建 old 失败：%v", err)
	}

	fresh := newConversationRun("sesn-fresh")
	fresh.StartedAt = "2026-08-27T05:00:00.000Z"
	fresh.LastActivityAt = "" // old data: no such key, sorting must fall back to startedAt
	if _, err := s.CreateRun(ctx, fresh); err != nil {
		t.Fatalf("建 fresh 失败：%v", err)
	}

	list, err := s.ListRuns(ctx, ws, persistmodel.RunQuery{})
	if err != nil {
		t.Fatalf("列 run 失败：%v", err)
	}
	if len(list) != 2 {
		t.Fatalf("期望 2 条 run，得到 %d 条", len(list))
	}
	// old's startedAt is earlier, but it was just active, so it must come first.
	if list[0].ID != "sesn-old" {
		t.Errorf("刚活跃过的会话没排到最前面，得到 %q（按 startedAt 排就会是这个结果）", list[0].ID)
	}
	if list[1].ID != "sesn-fresh" {
		t.Errorf("回退到 startedAt 的那条位置不对，得到 %q", list[1].ID)
	}
}

// testCreateAndSaveRun: a new run can be read back and overwritten, and writing one that does not exist reports ErrNotFound.
func testCreateAndSaveRun(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	run := newConversationRun("sesn-test-0001")
	created, err := s.CreateRun(ctx, run)
	if err != nil {
		t.Fatalf("建 run 失败：%v", err)
	}
	if created.ID != run.ID || created.Trigger != apimodel.TriggerManual {
		t.Errorf("建出来的 run 不对：%+v", created)
	}
	// Slices must be non-nil: nil encodes as null, and the frontend calls .map() on run.threads directly.
	if created.Threads == nil {
		t.Error("threads 不能是 nil")
	}

	got, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("查新建的 run 失败：%v", err)
	}
	if got.AgentName != "工单分诊助手" {
		t.Errorf("agentName 不对：%q", got.AgentName)
	}

	// It must be filterable as manual by ListRuns: that is how ChatPage's left column gets the conversation list.
	runs, err := s.ListRuns(ctx, "ws_demo", persistmodel.RunQuery{})
	if err != nil {
		t.Fatalf("列 run 失败：%v", err)
	}
	var found bool
	for _, r := range runs {
		if r.ID == run.ID {
			found = true
		}
	}
	if !found {
		t.Error("新建的 run 没出现在列表里")
	}

	// Overwrite: status, title, usage and end time must all be stored.
	got.Status = apimodel.RunIdle
	got.Title = "你好"
	got.Usage.InputTokens = 5831
	got.EventCount = 7
	ms := int64(1234)
	got.DurationMs = &ms
	got.EndedAt = apimodel.NowISO()
	if _, err := s.SaveRun(ctx, got); err != nil {
		t.Fatalf("存 run 失败：%v", err)
	}
	after, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("再查 run 失败：%v", err)
	}
	if after.Title != "你好" || after.Usage.InputTokens != 5831 || after.EventCount != 7 {
		t.Errorf("覆盖写没生效：%+v", after)
	}
	if after.DurationMs == nil || *after.DurationMs != 1234 {
		t.Errorf("durationMs 没存住：%v", after.DurationMs)
	}
	// The pointer must be a fresh copy; the caller must not get an address it can modify.
	if after.DurationMs == got.DurationMs {
		t.Error("durationMs 应该是深拷贝")
	}

	missing := newConversationRun("sesn-不存在")
	if _, err := s.SaveRun(ctx, missing); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("存不存在的 run 应返回 ErrNotFound，实际 %v", err)
	}
}

// testAppendRunEventSequences: sequence numbers start from 1, increase one by one, and are filled into the raw event.
//
// This is the fallback for "Ark does not send sequence" (see apimodel.NewRunEventFromArk):
// the frontend sorts by sequence, and if the server did not fill it in, the order of the whole timeline would be undefined.
func testAppendRunEventSequences(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	run := newConversationRun("sesn-test-0002")
	if _, err := s.CreateRun(ctx, run); err != nil {
		t.Fatalf("建 run 失败：%v", err)
	}

	// The shape Ark really pushes: no sequence, and session-level events do not even have session_thread_id.
	raws := [][]byte{
		[]byte(`{"type":"session.status_running","id":"sevt-1","processed_at":"2026-08-26T11:14:42.189Z"}`),
		[]byte(`{"id":"sevt-2","type":"agent.message","content":[{"type":"text","text":"你好"}],"session_thread_id":"sthr-1"}`),
		[]byte(`{"type":"session.status_idle","id":"sevt-3","stop_reason":{"type":"end_turn"}}`),
	}
	for i, raw := range raws {
		ev, _, err := s.AppendRunEvent(ctx, run.ID, raw)
		if err != nil {
			t.Fatalf("追加第 %d 条事件失败：%v", i+1, err)
		}
		if ev.Sequence != i+1 {
			t.Errorf("第 %d 条的 sequence 应为 %d，实际 %d", i+1, i+1, ev.Sequence)
		}
	}

	events, err := s.ListRunEvents(ctx, run.ID, persistmodel.EventQuery{})
	if err != nil {
		t.Fatalf("列事件失败：%v", err)
	}
	if len(events) != 3 {
		t.Fatalf("事件数应为 3，实际 %d", len(events))
	}
	for i, ev := range events {
		var decoded struct {
			Sequence *int    `json:"sequence"`
			ThreadID *string `json:"session_thread_id"`
			Type     string  `json:"type"`
		}
		if err := json.Unmarshal(ev.Raw, &decoded); err != nil {
			t.Fatalf("第 %d 条事件不是合法 JSON：%v（原文 %s）", i+1, err, ev.Raw)
		}
		// sequence must be **really written into the bytes**: the frontend reads this key in the JSON,
		// not the field on the Go struct.
		if decoded.Sequence == nil || *decoded.Sequence != i+1 {
			t.Errorf("第 %d 条事件原文里的 sequence 不对：%s", i+1, ev.Raw)
		}
		if decoded.Type != events[i].Type {
			t.Errorf("索引出来的 type 与原文不一致：%q vs %q", events[i].Type, decoded.Type)
		}
	}

	// Session-level events (Ark never sends this key) must be filled with null: the frontend's TS type is
	// `session_thread_id: string | null`, so the key must exist.
	var first map[string]json.RawMessage
	if err := json.Unmarshal(events[0].Raw, &first); err != nil {
		t.Fatalf("解第一条事件失败：%v", err)
	}
	if v, ok := first["session_thread_id"]; !ok || string(v) != "null" {
		t.Errorf("会话级事件的 session_thread_id 应补成 null，实际 %s", events[0].Raw)
	}

	// Filtering by thread: session-level events (thread is null) must be excluded.
	byThread, err := s.ListRunEvents(ctx, run.ID, persistmodel.EventQuery{ThreadID: "sthr-1"})
	if err != nil {
		t.Fatalf("按线程列事件失败：%v", err)
	}
	if len(byThread) != 1 || byThread[0].Type != "agent.message" {
		t.Errorf("按线程筛的结果不对：%+v", byThread)
	}
}

// testAppendRunEventKeepsBytes: < > & in the raw event must not be escaped.
//
// Same reason as testEventRawIsByteExact, but watching the **write** side:
// these three characters will sooner or later show up in agent replies (pasted code, pasted HTML), and escaping them would differ from Ark's raw text,
// while the frontend aligns the SSE copy and the listRunEvents copy by id; if the two copies differ in bytes, it draws two messages.
func testAppendRunEventKeepsBytes(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	run := newConversationRun("sesn-test-0003")
	if _, err := s.CreateRun(ctx, run); err != nil {
		t.Fatalf("建 run 失败：%v", err)
	}

	const text = `if a < b && c > d`
	raw := []byte(`{"id":"sevt-x","type":"agent.message","session_thread_id":"sthr-1","content":[{"type":"text","text":"` + text + `"}]}`)
	if _, _, err := s.AppendRunEvent(ctx, run.ID, raw); err != nil {
		t.Fatalf("追加事件失败：%v", err)
	}

	events, err := s.ListRunEvents(ctx, run.ID, persistmodel.EventQuery{})
	if err != nil {
		t.Fatalf("列事件失败：%v", err)
	}
	if len(events) != 1 {
		t.Fatalf("事件数应为 1，实际 %d", len(events))
	}
	got := string(events[0].Raw)
	// The escaped form is that **literal** string "backslash u 003c"; the assertion must look for it, not the raw character,
	// since the raw character should be there anyway, and watching it tests nothing.
	//
	// These patterns are built by concatenation instead of hard-coded: writing "<" like that in Go source is decoded by the compiler into
	// '<' itself, so the assertion would go back to "watching the raw character", testing nothing.
	for _, hex := range []string{"003c", "003e", "0026"} {
		escaped := `\u` + hex
		if strings.Contains(got, escaped) {
			t.Errorf("原文被 HTML 转义成了 %s：%s", escaped, got)
		}
	}
	if !strings.Contains(got, text) {
		t.Errorf("原文没保住：%s", got)
	}
}

// testAppendRunEventDedupesByID: the same event sent again is stored as only one row, and the first sequence is returned.
//
// # This guards against "the same line shown several times on the chat page"
//
// One Ark session can have several subscribers **at the same time**: Feishu inbound, Feishu resume,
// the browser's SSE, scheduled tasks. Each subscriber calls AppendRunEvent once for every frame it receives, and the sequence number is
// assigned by AppendRunEvent itself (current max +1), so without dedup the same event would get two
// different numbers and two rows would be stored peacefully; the primary key (run_id, sequence) does not conflict at all.
//
// What really happened: in a conversation started from Feishu, from the moment the browser opened the chat page,
// every later event was stored as 2 rows (same id, raw byte-for-byte identical except for the injected sequence),
// and a line in the UI ("let me check whether these tools are installed in the current environment") showed up several times.
//
// **The return value must be right too**: the bytes forwarded to the browser carry sequence, and two subscribers must forward the same number,
// otherwise the frontend sorting by sequence would put the same message in two places.
func testAppendRunEventDedupesByID(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	run := newConversationRun("sesn-test-0005")
	if _, err := s.CreateRun(ctx, run); err != nil {
		t.Fatalf("建 run 失败：%v", err)
	}

	// Two subscribers receive the same event: Ark's raw text has no sequence, so the bytes coming in both times are exactly the same.
	raw := []byte(`{"id":"sevt-dup","type":"agent.message","content":[{"type":"text","text":"让我检查一下当前环境中是否安装了这些工具。"}],"session_thread_id":"sthr-1"}`)

	first, firstCreated, err := s.AppendRunEvent(ctx, run.ID, raw)
	if err != nil {
		t.Fatalf("第一次追加失败：%v", err)
	}
	second, secondCreated, err := s.AppendRunEvent(ctx, run.ID, raw)
	if err != nil {
		t.Fatalf("第二次追加失败：%v", err)
	}

	// created is the only signal for "newly stored" vs "hit dedup".
	//
	// **It has external side effects**: observability reporting hangs on this boolean (persistArkEvent in core/chat.go).
	// Several instances each see the same event once; if the second also reported created=true,
	// call counts and token usage on the observability platform would all be inflated, with no error, the data just quietly wrong.
	if !firstCreated {
		t.Error("第一次追加应报 created=true（它真的写了一行）")
	}
	if secondCreated {
		t.Error("撞去重时必须报 created=false，否则观测会把同一条事件报多次")
	}

	if second.Sequence != first.Sequence {
		t.Errorf("重复事件应返回首次的 sequence %d，实际 %d（两个订阅者转发的号不一致，"+
			"前端会把同一条消息排到两个位置）", first.Sequence, second.Sequence)
	}
	if !bytes.Equal(first.Raw, second.Raw) {
		t.Errorf("重复事件返回的字节应与首次一致：\n首次 %s\n二次 %s", first.Raw, second.Raw)
	}

	events, err := s.ListRunEvents(ctx, run.ID, persistmodel.EventQuery{})
	if err != nil {
		t.Fatalf("列事件失败：%v", err)
	}
	if len(events) != 1 {
		t.Fatalf("同一条事件只该存 1 行，实际 %d 行（这正是界面上一句话显示多遍的原因）",
			len(events))
	}

	// Different ids are appended as usual: dedup must not swallow real new events.
	other := []byte(`{"id":"sevt-other","type":"agent.message","content":[{"type":"text","text":"另一句"}],"session_thread_id":"sthr-1"}`)
	if _, _, err := s.AppendRunEvent(ctx, run.ID, other); err != nil {
		t.Fatalf("追加不同 id 的事件失败：%v", err)
	}
	events, err = s.ListRunEvents(ctx, run.ID, persistmodel.EventQuery{})
	if err != nil {
		t.Fatalf("列事件失败：%v", err)
	}
	if len(events) != 2 {
		t.Fatalf("不同 id 应各存一行，事件数应为 2，实际 %d", len(events))
	}

	// Frames without an id are not deduped: they cannot be, and storing them one by one is better than swallowing them.
	// Use two **exactly identical** id-less byte strings, to confirm they are not treated as the same one.
	noID := []byte(`{"type":"agent.message","content":[{"type":"text","text":"无 id"}]}`)
	for i := 0; i < 2; i++ {
		if _, _, err := s.AppendRunEvent(ctx, run.ID, noID); err != nil {
			t.Fatalf("追加第 %d 条无 id 事件失败：%v", i+1, err)
		}
	}
	events, err = s.ListRunEvents(ctx, run.ID, persistmodel.EventQuery{})
	if err != nil {
		t.Fatalf("列事件失败：%v", err)
	}
	if len(events) != 4 {
		t.Fatalf("无 id 的帧应逐条存下，事件数应为 4，实际 %d", len(events))
	}
}

// testConcurrentAppendSameEventID: several subscribers push the same event **at the same time**, and only one row may survive.
//
// The difference from testAppendRunEventDedupesByID is that this one is concurrent: in reality several subscribers
// receive the same frame in their own goroutines at the same time. "Check once whether it exists, insert if not" is bound to miss here:
// both goroutines' queries say "not yet". The PG side decides by the unique index, MemStore by
// the same lock; run with -race.
func testConcurrentAppendSameEventID(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	run := newConversationRun("sesn-test-0006")
	if _, err := s.CreateRun(ctx, run); err != nil {
		t.Fatalf("建 run 失败：%v", err)
	}

	// 8 subscribers push the same event at once.
	const n = 8
	raw := []byte(`{"id":"sevt-race","type":"agent.message","content":[{"type":"text","text":"并发同一条"}],"session_thread_id":"sthr-1"}`)

	var wg sync.WaitGroup
	seqs := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			ev, _, err := s.AppendRunEvent(ctx, run.ID, raw)
			if err != nil {
				t.Errorf("并发追加失败：%v", err)
				return
			}
			seqs[k] = ev.Sequence
		}(i)
	}
	wg.Wait()

	events, err := s.ListRunEvents(ctx, run.ID, persistmodel.EventQuery{})
	if err != nil {
		t.Fatalf("列事件失败：%v", err)
	}
	if len(events) != 1 {
		t.Fatalf("并发推同一条事件只该存 1 行，实际 %d 行", len(events))
	}
	// Every caller should get the sequence number of that single row.
	for i, got := range seqs {
		if got != events[0].Sequence {
			t.Errorf("第 %d 个调用方拿到 sequence %d，应为 %d", i, got, events[0].Sequence)
		}
	}
}

// testListsStaleActiveRuns: stuck active conversations must be listed, and not a single normal one may be pulled in.
//
// # What this group guards against
//
// A run can stay in requires_action forever, stuck for days. The real cause is that
// `user.tool_result` never came back because the execution environment was replaced, and **nothing anywhere** was responsible for pushing the run
// to a final state. Each zombie is also resubscribed every 30 seconds by the long-term subscription, hanging for the full 10-minute fallback timeout.
//
// # The negative assertions matter more than the positive half
//
// Checking only "zombies can be listed", an implementation that "lists all active conversations" would still be all green,
// and it would push conversations **working normally** to a final state too: the user's answer gets cut off, and the symptom is exactly the same
// as a real zombie (both are one terminated row in the database), making people think the fallback did not work.
//
// So one row is built for each of four "must not be reclaimed" kinds; each blocks one way of writing the predicate wrong:
//
//	running, just active           missing the time condition → all active conversations reclaimed
//	requires_action, just active   judging by status only → conversations waiting for tool results cut off
//	idle, finished long ago        missing the status allowlist → finished history changed to terminated
//	old run (no lastActivityAt)    → see below; it guards the fallback
func testListsStaleActiveRuns(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	now := time.Now().UTC()
	staleBefore := apimodel.FormatISO(now.Add(-30 * time.Minute))
	old := apimodel.FormatISO(now.Add(-2 * time.Hour))   // before the threshold
	fresh := apimodel.FormatISO(now.Add(-1 * time.Hour)) // also before the threshold, but later
	recent := apimodel.FormatISO(now.Add(-time.Minute))  // after the threshold

	cases := []struct {
		id         string
		status     apimodel.RunStatus
		lastActive string
		startedAt  string
		wantStale  bool
		why        string
	}{
		{"sesn-stale-ra", apimodel.RunRequiresAction, old, old, true,
			"卡在等工具结果、两小时没动静——这正是那类僵尸的形状"},
		{"sesn-stale-run", apimodel.RunRunning, fresh, fresh, true,
			"running 同样会卡死（方舟不再推帧），不能只收 requires_action"},
		{"sesn-stale-resched", apimodel.RunRescheduling, old, old, true,
			"rescheduling 也在活跃白名单里，漏了它那批会话永远没人收"},
		{"sesn-fresh-ra", apimodel.RunRequiresAction, recent, recent, false,
			"刚调了工具、正等结果——把它收掉就是把用户的回答腰斩"},
		{"sesn-fresh-run", apimodel.RunRunning, recent, recent, false,
			"正在跑，收掉它等于中断一次正常对话"},
		{"sesn-done-idle", apimodel.RunIdle, old, old, false,
			"已经正常结束的历史，不该被改成 terminated"},
		{"sesn-done-term", apimodel.RunTerminated, old, old, false,
			"已经是终态，重复处理只会刷无意义的日志"},
		// ⚠️ Old data has no lastActivityAt key (omitempty), so it must fall back to startedAt.
		// Without the fallback `doc->>'lastActivityAt'` is NULL, and `NULL < $1` is NULL too,
		// so **the whole row is silently filtered out by WHERE**, and exactly the oldest batch of zombies would never be reclaimed,
		// while they are the ones most in need. This is what it guards against.
		{"sesn-legacy-noactivity", apimodel.RunRequiresAction, "", old, true,
			"老 run 没有 lastActivityAt，要回退到 startedAt 判定"},
	}

	for _, tc := range cases {
		run := newConversationRun(tc.id)
		run.Status = tc.status
		run.StartedAt = tc.startedAt
		run.LastActivityAt = tc.lastActive
		if _, err := s.CreateRun(ctx, run); err != nil {
			t.Fatalf("造 run %s 失败：%v", tc.id, err)
		}
	}

	got, err := s.ListStaleActiveRuns(ctx, staleBefore)
	if err != nil {
		t.Fatalf("列卡死的会话失败：%v", err)
	}
	listed := map[string]bool{}
	for _, r := range got {
		listed[r.ID] = true
	}
	for _, tc := range cases {
		if listed[tc.id] != tc.wantStale {
			t.Errorf("run %s（%s / lastActivity=%q）：应当被判为卡死=%v，实际=%v —— %s",
				tc.id, tc.status, tc.lastActive, tc.wantStale, listed[tc.id], tc.why)
		}
	}

	// It also pins the ordering contract: **the oldest is reclaimed first**, so even when cut off by the limit, the longest backlog is always handled.
	// Checking only "two results are the same" is not enough: that is always true when the input is already ordered, and still green with order by deleted
	// (see the CLAUDE.md lesson "small tables do not reveal a missing order by").
	for i := 1; i < len(got); i++ {
		prev, cur := persistmodel.LastActivityOf(got[i-1]), persistmodel.LastActivityOf(got[i])
		if prev > cur {
			t.Errorf("应按活跃时刻升序（最久的先收），实际 %q 排在 %q 前面", prev, cur)
		}
	}
}

// testRetireStaleRunIsConditional: reclaiming is a CAS: if the conversation came back to life in between, it must not be changed.
//
// # This guards against the silent "read then write" failure
//
// There is a real window between listing candidates and writing back, and that late `user.tool_result` may well
// arrive right then: it pushes the run back to running and refreshes lastActivityAt. If the implementation is
// "read it, change it, SaveRun", it would **kill the just-revived conversation again**: the user sees the conversation
// suddenly become "terminated", while the database only has a terminated row, with no log saying it ever came back.
//
// Only putting the rule into the UPDATE's WHERE blocks it. This checks it by "revive it first, then try to reclaim":
// a read-then-write implementation would succeed (return true), while the correct one matches no rows (returns false).
func testRetireStaleRunIsConditional(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	now := time.Now().UTC()
	staleBefore := apimodel.FormatISO(now.Add(-30 * time.Minute))

	run := newConversationRun("sesn-revived")
	run.Status = apimodel.RunRequiresAction
	run.StartedAt = apimodel.FormatISO(now.Add(-2 * time.Hour))
	run.LastActivityAt = run.StartedAt
	if _, err := s.CreateRun(ctx, run); err != nil {
		t.Fatalf("造 run 失败：%v", err)
	}

	// Simulate the late tool_result arriving: status back to running, active time refreshed to now.
	revived := run
	revived.Status = apimodel.RunRunning
	revived.LastActivityAt = apimodel.FormatISO(now)
	if _, err := s.SaveRun(ctx, revived); err != nil {
		t.Fatalf("模拟会话复活失败：%v", err)
	}

	// Reclaim using **the snapshot from before** (exactly the one the sweep would have in hand).
	doomed := run
	doomed.Status = apimodel.RunTerminated
	doomed.EndedAt = apimodel.FormatISO(now)
	ok, err := s.RetireStaleRun(ctx, doomed, staleBefore)
	if err != nil {
		t.Fatalf("回收出错：%v", err)
	}
	if ok {
		t.Error("会话已经活过来了，不该被回收——先读后写会把它重新按死，且零报错")
	}

	after, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("查 run 失败：%v", err)
	}
	if after.Status != apimodel.RunRunning {
		t.Errorf("复活的会话状态应保持 running，实际 %q", after.Status)
	}
	if after.EndedAt != "" {
		t.Errorf("复活的会话不该有 endedAt，实际 %q", after.EndedAt)
	}

	// The reverse: a real zombie must be reclaimable. Without this half, an implementation that "always returns false" would pass the first half.
	zombie := newConversationRun("sesn-zombie")
	zombie.Status = apimodel.RunRequiresAction
	zombie.StartedAt = apimodel.FormatISO(now.Add(-2 * time.Hour))
	zombie.LastActivityAt = zombie.StartedAt
	if _, err := s.CreateRun(ctx, zombie); err != nil {
		t.Fatalf("造僵尸 run 失败：%v", err)
	}
	retired := zombie
	retired.Status = apimodel.RunTerminated
	retired.EndedAt = apimodel.FormatISO(now)
	retired.ErrorSummary = "工具结果没有返回"
	ok, err = s.RetireStaleRun(ctx, retired, staleBefore)
	if err != nil {
		t.Fatalf("回收僵尸出错：%v", err)
	}
	if !ok {
		t.Fatal("真正的僵尸应当被回收，否则这个功能等于没做")
	}
	got, err := s.GetRun(ctx, zombie.ID)
	if err != nil {
		t.Fatalf("查僵尸 run 失败：%v", err)
	}
	if got.Status != apimodel.RunTerminated {
		t.Errorf("僵尸回收后应为 terminated，实际 %q", got.Status)
	}
	if got.ErrorSummary == "" {
		t.Error("回收后要留一句能看懂的说明，否则用户对着一条空白的『已终止』猜原因")
	}
}

// testRetireStaleRunIsAtomic: several instances reclaiming the same row at once; only one may win.
//
// Each instance runs its own sweep, so the same zombie is found by several at once. Without atomicity every instance
// "successfully" reclaims it once, so that Warn log is printed N times; when debugging it would be read as "this conversation keeps getting stuck",
// while in fact it was stuck only once. The log is the only output of this feature, and dirtying it is the same as having no output.
//
// Same shape as the ClaimRunWatch concurrency test; run with -race.
func testRetireStaleRunIsAtomic(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	now := time.Now().UTC()
	staleBefore := apimodel.FormatISO(now.Add(-30 * time.Minute))

	run := newConversationRun("sesn-race-reap")
	run.Status = apimodel.RunRequiresAction
	run.StartedAt = apimodel.FormatISO(now.Add(-2 * time.Hour))
	run.LastActivityAt = run.StartedAt
	if _, err := s.CreateRun(ctx, run); err != nil {
		t.Fatalf("造 run 失败：%v", err)
	}

	const n = 8
	var wg sync.WaitGroup
	wins := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			doomed := run
			doomed.Status = apimodel.RunTerminated
			doomed.EndedAt = apimodel.FormatISO(now)
			ok, err := s.RetireStaleRun(ctx, doomed, staleBefore)
			if err != nil {
				t.Errorf("并发回收出错：%v", err)
				return
			}
			wins[k] = ok
		}(i)
	}
	wg.Wait()

	won := 0
	for _, ok := range wins {
		if ok {
			won++
		}
	}
	if won != 1 {
		t.Errorf("并发回收同一条会话应当恰好有 1 个成功，实际 %d 个——"+
			"多个都成功会让那条 Warn 日志打 N 遍，看着像反复卡死", won)
	}
}

// This is **the very reason** sequence numbers are handed out by the Store (instead of letting the caller read the max once and add one):
// Ark events are pushed down concurrently, and two goroutines computing on their own would always get the same number,
// which on the PG side directly hits the primary key (run_id, sequence). Run with -race.
func testConcurrentAppendRunEvent(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	run := newConversationRun("sesn-test-0004")
	if _, err := s.CreateRun(ctx, run); err != nil {
		t.Fatalf("建 run 失败：%v", err)
	}

	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			raw := []byte(fmt.Sprintf(
				`{"id":"sevt-%d","type":"agent.message","content":[{"type":"text","text":"第 %d 条"}]}`, k, k))
			if _, _, err := s.AppendRunEvent(ctx, run.ID, raw); err != nil {
				t.Errorf("并发追加失败：%v", err)
			}
		}(i)
	}
	wg.Wait()

	events, err := s.ListRunEvents(ctx, run.ID, persistmodel.EventQuery{})
	if err != nil {
		t.Fatalf("列事件失败：%v", err)
	}
	if len(events) != n {
		t.Fatalf("事件数应为 %d，实际 %d（撞号会让事件被吞掉）", n, len(events))
	}
	// Sequence numbers must all be different and strictly increasing: ListRunEvents sorts by them.
	seen := map[int]bool{}
	for i, ev := range events {
		if seen[ev.Sequence] {
			t.Errorf("sequence %d 重复了", ev.Sequence)
		}
		seen[ev.Sequence] = true
		if i > 0 && ev.Sequence <= events[i-1].Sequence {
			t.Errorf("sequence 没有递增：%d 之后是 %d", events[i-1].Sequence, ev.Sequence)
		}
	}
}

// testSeedLoads: the seed loads, and the counts match the frontend.
func testSeedLoads(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	ws, err := s.ListWorkspaces(ctx)
	if err != nil {
		t.Fatalf("列空间失败：%v", err)
	}
	if len(ws) != 1 {
		t.Fatalf("空间数应为 1，实际 %d", len(ws))
	}
	agents, _ := s.ListAgents(ctx, ws[0].ID)
	if len(agents) != 4 {
		t.Errorf("agent 数应为 4，实际 %d", len(agents))
	}
	skills, _ := s.ListSkills(ctx, ws[0].ID)
	if len(skills) != 4 {
		t.Errorf("技能数应为 4，实际 %d", len(skills))
	}
	runs, _ := s.ListRuns(ctx, ws[0].ID, persistmodel.RunQuery{})
	if len(runs) != 8 {
		t.Errorf("run 数应为 8，实际 %d", len(runs))
	}
}

// testReadReturnsCopy: changing what was read must not affect the stored state.
//
// Without this, one handler changing a return value would pollute the data another request sees, the kind of
// "occasionally wrong" bug that is extremely hard to track down.
func testReadReturnsCopy(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	a1, err := s.GetAgent(ctx, "agent_triage")
	if err != nil {
		t.Fatalf("取 agent 失败：%v", err)
	}
	// Change the slices and scalars of the return value in place
	if len(a1.SkillIDs) > 0 {
		a1.SkillIDs[0] = "污染"
	}
	a1.Name = "改过的名字"

	a2, _ := s.GetAgent(ctx, "agent_triage")
	if a2.Name == "改过的名字" {
		t.Error("标量被外部改动影响了，说明没拷贝")
	}
	if len(a2.SkillIDs) > 0 && a2.SkillIDs[0] == "污染" {
		t.Error("切片被外部改动影响了，说明是浅拷贝")
	}
}

// testSlicesNeverNil: every slice field must be non-nil: nil encodes as null, and the frontend blows up.
func testSlicesNeverNil(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, false)

	// Lists from an empty store must be [] too
	if ws, _ := s.ListWorkspaces(ctx); ws == nil {
		t.Error("ListWorkspaces 返回了 nil")
	}
	if a, _ := s.ListAgents(ctx, "不存在"); a == nil {
		t.Error("ListAgents 返回了 nil")
	}
	if sk, _ := s.ListSkills(ctx, "不存在"); sk == nil {
		t.Error("ListSkills 返回了 nil")
	}
	if r, _ := s.ListRuns(ctx, "不存在", persistmodel.RunQuery{}); r == nil {
		t.Error("ListRuns 返回了 nil")
	}
	if e, _ := s.ListRunEvents(ctx, "不存在", persistmodel.EventQuery{}); e == nil {
		t.Error("ListRunEvents 返回了 nil")
	}
	if b, _ := s.AgentsBoundTo(ctx, "不存在"); b == nil {
		t.Error("AgentsBoundTo 返回了 nil")
	}

	// Store an agent whose slices are all nil; they must become [] when read back
	_, err := s.CreateAgent(ctx, apimodel.Agent{ID: "a1", WorkspaceID: "w1"})
	if err != nil {
		t.Fatalf("建 agent 失败：%v", err)
	}
	got, _ := s.GetAgent(ctx, "a1")
	if got.SkillIDs == nil || got.FeishuAppIDs == nil || got.BuiltinTools == nil {
		t.Error("agent 的切片字段有 nil，会被编码成 null")
	}
	// Really serialize once to confirm
	blob, _ := json.Marshal(got)
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(blob, &raw)
	for _, key := range []string{"skillIds", "feishuAppIds", "builtin_tools"} {
		if string(raw[key]) != "[]" {
			t.Errorf("%s 应编码成 []，实际 %s", key, raw[key])
		}
	}

	// Same for a workspace with no members: Members must be []
	if _, err := s.CreateWorkspace(ctx, apimodel.Workspace{ID: "w1", Key: "k1"}); err != nil {
		t.Fatalf("建空间失败：%v", err)
	}
	gotWS, _ := s.GetWorkspace(ctx, "w1")
	if gotWS.Members == nil {
		t.Error("空间的 members 是 nil，会被编码成 null")
	}
}

// testListRunEventsUnknownRun: a run that does not exist returns an empty slice, not ErrNotFound.
// The return type of the frontend's listRunEvents is an array, not nullable; a 404 would make the page throw.
func testListRunEventsUnknownRun(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)
	got, err := s.ListRunEvents(ctx, "run_不存在", persistmodel.EventQuery{})
	if err != nil {
		t.Fatalf("不该报错，实际：%v", err)
	}
	if len(got) != 0 {
		t.Errorf("应是空切片，实际 %d 条", len(got))
	}
}

// testEventFilters: type and thread filtering, and ascending sequence.
func testEventFilters(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	all, _ := s.ListRunEvents(ctx, "run_1", persistmodel.EventQuery{})
	// 44: matches the real content of seed.ts.
	//
	// Historically this said 38 → 42, and both times **the golden file had drifted**: someone added events for run_1
	// to seed.ts without rerunning `scripts/export-seed/export.sh`, so seed.json stayed at the old version,
	// and the assertion matched that old version. The check is "export a clean tree with zero changes via git archive, rerun the export script,
	// then run this test"; if it is still red then, it has nothing to do with the current change.
	if len(all) != 44 {
		t.Fatalf("run_1 应有 44 个事件，实际 %d", len(all))
	}
	// Ascending
	for i := 1; i < len(all); i++ {
		if all[i-1].Sequence > all[i].Sequence {
			t.Fatalf("事件没有按 sequence 升序：第 %d 条乱序", i)
		}
	}

	// Filter by type
	filtered, _ := s.ListRunEvents(ctx, "run_1", persistmodel.EventQuery{Types: []string{"agent.message"}})
	if len(filtered) == 0 || len(filtered) >= len(all) {
		t.Errorf("类型筛选没生效，筛出 %d 条（总 %d）", len(filtered), len(all))
	}
	for _, e := range filtered {
		if e.Type != "agent.message" {
			t.Errorf("筛出了不该有的类型：%s", e.Type)
		}
	}

	// An allowlist of several types should filter out more than a single one
	multi, _ := s.ListRunEvents(ctx, "run_1", persistmodel.EventQuery{
		Types: []string{"agent.message", "agent.tool_use"},
	})
	if len(multi) <= len(filtered) {
		t.Errorf("两个类型应比一个类型筛出更多：%d vs %d", len(multi), len(filtered))
	}

	// Filter by thread: run_1 has a main thread and a sub-thread
	sub, _ := s.ListRunEvents(ctx, "run_1", persistmodel.EventQuery{ThreadID: "sth_sub_sql_01"})
	if len(sub) == 0 {
		t.Error("按线程筛选没筛出任何事件")
	}
	for _, e := range sub {
		if e.ThreadID == nil || *e.ThreadID != "sth_sub_sql_01" {
			t.Error("筛出了不属于该线程的事件")
		}
	}
}

// testEventRawIsByteExact: an event's Raw must match the seed **byte for byte**.
//
// This is a key guard for the PG implementation: the raw bytes of Raw are written straight into the HTTP response, to get around
// encoding/json's HTML escaping (see core/README.md). If it were stored in jsonb, PostgreSQL
// would reorder keys and squeeze whitespace: **all field values are still there, and the parsed structs compare equal**, but the bytes changed,
// so the response no longer matches Ark's raw data. So this compares string(Raw), not the parsed value.
func testEventRawIsByteExact(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	// The baseline comes from the seed itself, independent of the implementation.
	want, err := persistmodel.LoadSeed()
	if err != nil {
		t.Fatalf("装载种子失败：%v", err)
	}

	for runID, wantEvents := range want.Events {
		got, err := s.ListRunEvents(ctx, runID, persistmodel.EventQuery{})
		if err != nil {
			t.Fatalf("列 %s 的事件失败：%v", runID, err)
		}
		if len(got) != len(wantEvents) {
			t.Fatalf("%s 的事件数不对：期望 %d 实际 %d", runID, len(wantEvents), len(got))
		}
		// **Pair by id, not by index.**
		//
		// `ListRunEvents` returns in ascending sequence, while the seed array **is not guaranteed to be laid out by sequence**:
		// in run_6, sevt-0605(seq 90) is deliberately placed before sevt-0603(seq 88), precisely so that
		// "the implementation really sorted" is testable (see the shuffledSameMillis comment in seed.ts).
		// Comparing by index would read this **deliberate** disorder as "the raw bytes changed", an error pointing at Raw being
		// rewritten (jsonb reordering keys and so on), miles away from the real cause.
		byID := make(map[string][]byte, len(wantEvents))
		for _, e := range wantEvents {
			byID[e.EventID] = e.Raw
		}
		for i := range got {
			wantRaw, ok := byID[got[i].EventID]
			if !ok {
				t.Fatalf("%s 第 %d 个事件 %s 不在种子里", runID, i, got[i].EventID)
			}
			if string(got[i].Raw) != string(wantRaw) {
				t.Fatalf("%s 的事件 %s 原始字节变了：\n期望 %s\n实际 %s",
					runID, got[i].EventID, wantRaw, got[i].Raw)
			}
		}
	}
}

// testListRunsFilters: the three filters agentId / status / search, and descending by activity time.
func testListRunsFilters(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)
	const ws = "ws_demo"

	all, _ := s.ListRuns(ctx, ws, persistmodel.RunQuery{})
	// Most recently active first. The seed data has no lastActivityAt, so this compares the fallback startedAt;
	// the fallback itself is one of the things to assert (both stores must agree).
	for i := 1; i < len(all); i++ {
		if persistmodel.LastActivityOf(all[i-1]) < persistmodel.LastActivityOf(all[i]) {
			t.Fatalf("run 没有按最后活跃时刻降序")
		}
	}

	byAgent, _ := s.ListRuns(ctx, ws, persistmodel.RunQuery{AgentID: "agent_triage"})
	if len(byAgent) == 0 {
		t.Error("按 agentId 筛没筛出东西")
	}
	for _, r := range byAgent {
		if r.AgentID != "agent_triage" {
			t.Error("筛出了别的 agent 的 run")
		}
	}

	byStatus, _ := s.ListRuns(ctx, ws, persistmodel.RunQuery{Status: []apimodel.RunStatus{apimodel.RunTerminated}})
	if len(byStatus) == 0 {
		t.Error("按状态筛没筛出东西")
	}
	for _, r := range byStatus {
		if r.Status != apimodel.RunTerminated {
			t.Error("状态筛选筛出了别的状态")
		}
	}

	// Allowlist of several statuses
	multi, _ := s.ListRuns(ctx, ws, persistmodel.RunQuery{
		Status: []apimodel.RunStatus{apimodel.RunTerminated, apimodel.RunRunning},
	})
	if len(multi) <= len(byStatus) {
		t.Errorf("两个状态应比一个状态筛出更多：%d vs %d", len(multi), len(byStatus))
	}

	// search must match on all three fields title / agentName / id, case-insensitively
	byID, _ := s.ListRuns(ctx, ws, persistmodel.RunQuery{Search: "RUN_3"})
	if len(byID) != 1 {
		t.Errorf("按 id 大小写不敏感搜索应命中 1 条，实际 %d", len(byID))
	}
	byAgentName, _ := s.ListRuns(ctx, ws, persistmodel.RunQuery{Search: all[0].AgentName})
	if len(byAgentName) == 0 {
		t.Error("按 agentName 搜索没命中")
	}
	if len(all) > 0 && all[0].Title != "" {
		byTitle, _ := s.ListRuns(ctx, ws, persistmodel.RunQuery{Search: all[0].Title})
		if len(byTitle) == 0 {
			t.Error("按 title 搜索没命中")
		}
	}
	// No match returns empty, instead of degrading to "no filter"
	none, _ := s.ListRuns(ctx, ws, persistmodel.RunQuery{Search: "绝不可能命中的字符串-zzz"})
	if len(none) != 0 {
		t.Errorf("搜不到时应返回 0 条，实际 %d", len(none))
	}

	// Filters combine: agentId + status take effect together
	combo, _ := s.ListRuns(ctx, ws, persistmodel.RunQuery{
		AgentID: "agent_triage",
		Status:  []apimodel.RunStatus{apimodel.RunIdle},
	})
	for _, r := range combo {
		if r.AgentID != "agent_triage" || r.Status != apimodel.RunIdle {
			t.Error("组合筛选没同时生效")
		}
	}
}

// testListRunsByScheduleID: filter a scheduled task's own run history.
//
// # This tests "the history list on the detail page gets nobody else's data"
//
// The seed has no run with a scheduleId (all eight are manual / feishu / api), so build them here.
// **Runs for two different tasks must be built**: with only one, a wrong implementation that "does not filter at all and returns every run in the workspace"
// would still pass, and that is exactly the behavior from before this field existed.
//
// Also both runs deliberately belong to **the same agent**: schedule_id and agent_id are two independent filter axes,
// and building data with different agents would let an implementation where "it is really agentId doing the work" guess right.
func testListRunsByScheduleID(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)
	const ws = "ws_demo"

	mk := func(id, scheduleID string) {
		t.Helper()
		if _, err := s.CreateRun(ctx, apimodel.Run{
			ID: id, WorkspaceID: ws, AgentID: "agent_triage", AgentName: "分诊助手",
			Title: "定时巡检", Status: apimodel.RunTerminated, Trigger: apimodel.TriggerSchedule,
			TriggerDetail: "巡检", ScheduleID: scheduleID,
			StartedAt: "2026-08-30T10:00:00.000Z",
			Usage:     apimodel.TokenUsage{}, Threads: []apimodel.RunThread{},
		}); err != nil {
			t.Fatalf("造 run %s 失败: %v", id, err)
		}
	}
	mk("run_sch_a1", "sch_a")
	mk("run_sch_a2", "sch_a")
	mk("run_sch_b1", "sch_b")

	got, err := s.ListRuns(ctx, ws, persistmodel.RunQuery{ScheduleID: "sch_a"})
	if err != nil {
		t.Fatalf("按 scheduleId 筛失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("sch_a 应有 2 条运行，实际 %d", len(got))
	}
	for _, r := range got {
		if r.ScheduleID != "sch_a" {
			t.Errorf("筛出了别的任务的 run：%s 属于 %q", r.ID, r.ScheduleID)
		}
	}

	// ScheduleID must round-trip. **On the PG side it is a separate column**, and an empty string must be written as NULL;
	// without this, an implementation that "writes only doc, not the column" could pass the filter assertions above (MemStore uses the in-memory
	// field and is always right), while filtering on PG always gives 0 rows.
	one, err := s.GetRun(ctx, "run_sch_b1")
	if err != nil {
		t.Fatalf("取 run 失败: %v", err)
	}
	if one.ScheduleID != "sch_b" {
		t.Errorf("ScheduleID 没 round-trip：想要 sch_b，得到 %q", one.ScheduleID)
	}

	// Runs that belong to no task must not be pulled in by this filter, nor be treated as a "match" because of an empty string.
	// The eight in the seed are this kind.
	if bogus, _ := s.ListRuns(ctx, ws, persistmodel.RunQuery{ScheduleID: "sch_不存在"}); len(bogus) != 0 {
		t.Errorf("不存在的任务应筛出 0 条，实际 %d", len(bogus))
	}

	// SaveRun must write this column too, not only CreateRun. **This is the path that really breaks**:
	// the scheduler creates sessions through CreateConversation (which does not know the schedule yet),
	// and the link is written afterwards by GetRun → change → SaveRun (runOnce in core/scheduler.go).
	// Without it production shows "run history never found", while unit tests covering only CreateRun would be all green.
	plain, err := s.GetRun(ctx, "run_1")
	if err != nil {
		t.Fatalf("取种子 run 失败: %v", err)
	}
	plain.ScheduleID = "sch_a"
	if _, err := s.SaveRun(ctx, plain); err != nil {
		t.Fatalf("SaveRun 失败: %v", err)
	}
	after, _ := s.ListRuns(ctx, ws, persistmodel.RunQuery{ScheduleID: "sch_a"})
	if len(after) != 3 {
		t.Errorf("SaveRun 写完 sch_a 应有 3 条，实际 %d（SaveRun 没写 schedule_id 列？）", len(after))
	}
}

// testListRunsByTrigger: filter by trigger type; the chat page's left column (manual + feishu) relies on it.
//
// # Why this filter must be on the backend
//
// It used to be the frontend's `runs.filter(r => CONVERSATION_TRIGGERS.includes(r.trigger))`.
// That was right in the days of "fetch all runs at once", but it broke as soon as ListRuns paged: the client can only filter
// the page it has, so the left column misses conversations, **while the page renders normally with zero errors**.
//
// ⚠️ The PG side has no trigger scalar column, only `doc->>'trigger'`. So on PG this test
// also pins "that key in doc is called trigger": changing the json tag of apimodel.Run turns it red,
// which is exactly what this repo's lesson "jsonb doc key names are not locked by tests" wants.
//
// The 8 in seed ws_demo: 3 manual, 4 feishu, 1 api.
func testListRunsByTrigger(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)
	const ws = "ws_demo"

	all, _ := s.ListRuns(ctx, ws, persistmodel.RunQuery{})

	// A single trigger.
	manual, err := s.ListRuns(ctx, ws, persistmodel.RunQuery{
		Trigger: []apimodel.RunTrigger{apimodel.TriggerManual},
	})
	if err != nil {
		t.Fatalf("按 trigger 筛失败: %v", err)
	}
	if len(manual) != 3 {
		t.Errorf("manual 应有 3 条，实际 %d", len(manual))
	}
	for _, r := range manual {
		if r.Trigger != apimodel.TriggerManual {
			t.Errorf("筛出了别的 trigger：%s", r.Trigger)
		}
	}

	// The allowlist must accept several: the chat page's left column is exactly manual + feishu.
	//
	// **It must assert strictly fewer than all**: an implementation that "does not filter at all" returns all 8,
	// and comparing only `> 0` or `>= len(manual)` would let that implementation pass.
	conv, err := s.ListRuns(ctx, ws, persistmodel.RunQuery{
		Trigger: []apimodel.RunTrigger{apimodel.TriggerManual, apimodel.TriggerFeishu},
	})
	if err != nil {
		t.Fatalf("按多个 trigger 筛失败: %v", err)
	}
	if len(conv) != 7 {
		t.Errorf("manual+feishu 应有 7 条，实际 %d", len(conv))
	}
	if len(conv) >= len(all) {
		t.Errorf("多 trigger 白名单不该等于不筛：%d vs 全部 %d（压根没筛？）", len(conv), len(all))
	}
	for _, r := range conv {
		if r.Trigger != apimodel.TriggerManual && r.Trigger != apimodel.TriggerFeishu {
			t.Errorf("对话筛选带出了 %s", r.Trigger)
		}
	}

	// An empty allowlist = no filter; do not degrade to "give nothing".
	if none, _ := s.ListRuns(ctx, ws, persistmodel.RunQuery{Trigger: nil}); len(none) != len(all) {
		t.Errorf("空 trigger 白名单应等于不筛：%d vs %d", len(none), len(all))
	}

	// An unrecognized trigger filters out 0 rows, instead of degrading to no filter.
	if bogus, _ := s.ListRuns(ctx, ws, persistmodel.RunQuery{
		Trigger: []apimodel.RunTrigger{"绝不存在的trigger"},
	}); len(bogus) != 0 {
		t.Errorf("不存在的 trigger 应筛出 0 条，实际 %d", len(bogus))
	}

	// Combined with other filters, both must take effect.
	combo, _ := s.ListRuns(ctx, ws, persistmodel.RunQuery{
		Trigger: []apimodel.RunTrigger{apimodel.TriggerFeishu},
		AgentID: all[0].AgentID,
	})
	for _, r := range combo {
		if r.Trigger != apimodel.TriggerFeishu || r.AgentID != all[0].AgentID {
			t.Error("trigger 与 agentId 的组合筛选没同时生效")
		}
	}
}

// testListRunsPagination watches the meaning of limit/offset, especially the three boundaries that go wrong silently.
//
// # Why this test exists
//
// Paging was added to cure ListRuns of "fetch everything + deserialize everything" (55ms / 19MB at 5000 rows,
// see bench_listruns_test.go). It has three ways to go wrong that **give no error, the result is just quietly wrong**:
//
//  1. **Limit==0 treated as "give nothing".** ListRuns has 6 call sites, and the full-result ones
//     (usage, scheduled task history) pass exactly `RunQuery{}`. If the zero value gave an empty list, they would silently become
//     empty pages, and an empty list looks exactly like "no runs yet" in the UI. So 0 must mean "no limit".
//  2. **Paging before sorting.** Then each page is sorted internally, but pages are out of order with each other,
//     and page two shows conversations newer than page one. Testing only "page one == the first N of all" cannot catch it,
//     so the ids are compared one by one below.
//  3. **Offset written inside `if Limit > 0`.** Then "skip the first N, give all the rest"
//     (Limit==0 and Offset>0) silently stops working and returns everything.
func testListRunsPagination(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)
	const ws = "ws_demo"

	all, err := s.ListRuns(ctx, ws, persistmodel.RunQuery{})
	if err != nil {
		t.Fatalf("列全部失败: %v", err)
	}
	if len(all) < 4 {
		t.Fatalf("种子 run 太少（%d 条），这条用例需要至少 4 条才有分页可言", len(all))
	}

	// One. Limit == 0 means **no limit**, not 0 rows. That is all of backward compatibility.
	if len(all) == 0 {
		t.Fatal("Limit==0 返回了空列表——零值被当成「一条都不要」了")
	}

	// Two. Page one is the first N of the full result, **compared id by id**.
	//
	// Compare ids, not just counts: same count but different order (paging before sorting) is the easiest mistake here,
	// and it is all green with assertions that only compare length.
	const page = 2
	first, err := s.ListRuns(ctx, ws, persistmodel.RunQuery{Limit: page})
	if err != nil {
		t.Fatalf("取第一页失败: %v", err)
	}
	if len(first) != page {
		t.Fatalf("第一页应有 %d 条，实际 %d", page, len(first))
	}
	for i := range first {
		if first[i].ID != all[i].ID {
			t.Errorf("第一页第 %d 条不是全量的同位：%s vs %s（分页发生在排序之前？）",
				i, first[i].ID, all[i].ID)
		}
	}

	// Three. Paging with no duplicates and no gaps: page two follows page one.
	second, err := s.ListRuns(ctx, ws, persistmodel.RunQuery{Limit: page, Offset: page})
	if err != nil {
		t.Fatalf("取第二页失败: %v", err)
	}
	if len(second) != page {
		t.Fatalf("第二页应有 %d 条，实际 %d", page, len(second))
	}
	for i := range second {
		if second[i].ID != all[page+i].ID {
			t.Errorf("第二页第 %d 条对不上：%s vs %s", i, second[i].ID, all[page+i].ID)
		}
	}
	// The two pages must not overlap.
	seen := make(map[string]struct{}, len(first))
	for _, r := range first {
		seen[r.ID] = struct{}{}
	}
	for _, r := range second {
		if _, dup := seen[r.ID]; dup {
			t.Errorf("第二页重复了第一页的 %s", r.ID)
		}
	}

	// Four. **Offset alone** (Limit==0) must skip the first N and return all the rest.
	// This blocks the mistake of "writing Offset inside if Limit > 0".
	rest, err := s.ListRuns(ctx, ws, persistmodel.RunQuery{Offset: page})
	if err != nil {
		t.Fatalf("只给 Offset 失败: %v", err)
	}
	if len(rest) != len(all)-page {
		t.Errorf("只给 Offset 应返回剩下 %d 条，实际 %d（Offset 被 Limit==0 吞掉了？）",
			len(all)-page, len(rest))
	}
	for i := range rest {
		if rest[i].ID != all[page+i].ID {
			t.Errorf("Offset 单独用时第 %d 条对不上：%s vs %s", i, rest[i].ID, all[page+i].ID)
		}
	}

	// Five. An out-of-range offset gives an empty list, not an error, and not wrapping back to page one.
	over, err := s.ListRuns(ctx, ws, persistmodel.RunQuery{Offset: len(all) + 10})
	if err != nil {
		t.Fatalf("越界 offset 不该报错: %v", err)
	}
	if len(over) != 0 {
		t.Errorf("越界 offset 应返回 0 条，实际 %d", len(over))
	}
	// **The slice is never nil** (this package's SlicesNeverNil contract): nil is json-encoded as
	// null, and the frontend's `runs.map` blows up.
	if over == nil {
		t.Error("越界 offset 返回了 nil 切片，应该是空切片")
	}

	// Six. A Limit larger than the total gives everything, without padding or erroring.
	big, _ := s.ListRuns(ctx, ws, persistmodel.RunQuery{Limit: len(all) + 10})
	if len(big) != len(all) {
		t.Errorf("Limit 超过总数应返回全部 %d 条，实际 %d", len(all), len(big))
	}

	// Seven. Paging must apply to the set **after filtering**, not page first and then filter.
	//
	// First take a set that still has several rows after filtering, then page it; if the implementation "cuts the first N then filters",
	// there would be fewer than expected here.
	conv, _ := s.ListRuns(ctx, ws, persistmodel.RunQuery{
		Trigger: []apimodel.RunTrigger{apimodel.TriggerFeishu},
	})
	if len(conv) < 2 {
		t.Fatalf("feishu 的 run 太少（%d），测不了「筛选 + 分页」的叠加", len(conv))
	}
	convPage, _ := s.ListRuns(ctx, ws, persistmodel.RunQuery{
		Trigger: []apimodel.RunTrigger{apimodel.TriggerFeishu},
		Limit:   1,
	})
	if len(convPage) != 1 {
		t.Fatalf("筛选后分页应有 1 条，实际 %d", len(convPage))
	}
	if convPage[0].ID != conv[0].ID {
		t.Errorf("筛选 + 分页取到的不是筛选结果的第一条：%s vs %s（先分页后筛？）",
			convPage[0].ID, conv[0].ID)
	}
}

// testDeleteWorkspaceCascades: deleting a workspace must cascade to agents / skills / runs / events / credentials.
// testListRunsForUsageRange watches that the time window is **half-open**.
//
// All 8 runs in the seed are on 2026-08-25, at 10:00 / 11:00 / 12:00 / 13:00 /
// 13:30 / 14:10 (run_7's failed conversation) / 14:26:40 (run_8 rescheduling) / 15:33:20 (run_6).
// A half-open interval is the precondition for correct daily buckets: a closed interval would put the boundary run into two adjacent buckets,
// so "the sum of runs per day" would exceed the total, while both numbers look reasonable and nothing reveals it.
//
// ⚠️ The last three are all after 13:30, so the boundaries below (12:00 / 11:00~13:00) still hit
// the same rows as before; only "total" and "since=12:00" change.
func testListRunsForUsageRange(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)
	const ws = "ws_demo"

	all, err := s.ListRunsForUsage(ctx, ws, persistmodel.UsageQuery{})
	if err != nil {
		t.Fatalf("不带时间窗取失败：%v", err)
	}
	if len(all) != 8 {
		t.Fatalf("种子里应有 8 条 run，实际 %d", len(all))
	}
	// Same order as ListRuns (most recent first); the two implementations must agree.
	for i := 1; i < len(all); i++ {
		if all[i-1].StartedAt < all[i].StartedAt {
			t.Fatalf("run 没有按 startedAt 降序")
		}
	}

	// Since is closed: when it exactly equals a run's startedAt, that run must be included.
	since, err := s.ListRunsForUsage(ctx, ws, persistmodel.UsageQuery{Since: "2026-08-25T12:00:00.000Z"})
	if err != nil {
		t.Fatalf("带 since 取失败：%v", err)
	}
	if len(since) != 6 {
		t.Errorf("since=12:00 应取到 12:00/13:00/13:30/14:10/14:26/15:33 共 6 条，实际 %d", len(since))
	}

	// Until is open: when it exactly equals a run's startedAt, that run must be excluded.
	until, err := s.ListRunsForUsage(ctx, ws, persistmodel.UsageQuery{Until: "2026-08-25T12:00:00.000Z"})
	if err != nil {
		t.Fatalf("带 until 取失败：%v", err)
	}
	if len(until) != 2 {
		t.Errorf("until=12:00 应取到 10:00/11:00 共 2 条（12:00 那条要被排除），实际 %d", len(until))
	}

	// Both ends together: [11:00, 13:00) hits 11:00 and 12:00.
	both, err := s.ListRunsForUsage(ctx, ws, persistmodel.UsageQuery{
		Since: "2026-08-25T11:00:00.000Z",
		Until: "2026-08-25T13:00:00.000Z",
	})
	if err != nil {
		t.Fatalf("带双端时间窗取失败：%v", err)
	}
	if len(both) != 2 {
		t.Errorf("[11:00, 13:00) 应命中 2 条，实际 %d", len(both))
	}

	// An empty window and a missing workspace both return an empty slice, not nil / an error.
	empty, err := s.ListRunsForUsage(ctx, ws, persistmodel.UsageQuery{
		Since: "2027-01-01T00:00:00.000Z",
	})
	if err != nil {
		t.Fatalf("空窗取失败：%v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Errorf("空窗应回长度 0 的非 nil 切片，实际 %#v", empty)
	}
	none, err := s.ListRunsForUsage(ctx, "ws_missing", persistmodel.UsageQuery{})
	if err != nil {
		t.Fatalf("不存在的空间应不报错，实际 %v", err)
	}
	if none == nil || len(none) != 0 {
		t.Errorf("不存在的空间应回长度 0 的非 nil 切片，实际 %#v", none)
	}
}

func testDeleteWorkspaceCascades(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)
	const ws = "ws_demo"

	if err := s.PutArkSecret(ctx, apimodel.ArkSecret{
		ArkCredential: apimodel.ArkCredential{WorkspaceID: ws},
		APIKey:        "k",
	}); err != nil {
		t.Fatalf("写凭证失败：%v", err)
	}

	if err := s.DeleteWorkspace(ctx, ws); err != nil {
		t.Fatalf("删空间失败：%v", err)
	}

	if _, err := s.GetWorkspace(ctx, ws); err == nil {
		t.Error("空间还在")
	}
	if a, _ := s.ListAgents(ctx, ws); len(a) != 0 {
		t.Errorf("agent 没清干净，剩 %d 个", len(a))
	}
	if sk, _ := s.ListSkills(ctx, ws); len(sk) != 0 {
		t.Errorf("技能没清干净，剩 %d 个", len(sk))
	}
	if r, _ := s.ListRuns(ctx, ws, persistmodel.RunQuery{}); len(r) != 0 {
		t.Errorf("run 没清干净，剩 %d 个", len(r))
	}
	// The events of those runs must be gone too
	if e, _ := s.ListRunEvents(ctx, "run_1", persistmodel.EventQuery{}); len(e) != 0 {
		t.Errorf("run 的事件没清干净，剩 %d 条", len(e))
	}
	if _, err := s.GetArkSecret(ctx, ws); err == nil {
		t.Error("凭证没跟着删——那是不该留的密钥")
	}
}

// testDeletesAreIdempotent: deleting something that does not exist is not an error.
//
// All five deletes in the frontend are idempotent no-ops; double-clicking the delete button must not pop an error.
func testDeletesAreIdempotent(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, false)
	if err := s.DeleteWorkspace(ctx, "没有"); err != nil {
		t.Errorf("删不存在的空间报错了：%v", err)
	}
	if err := s.DeleteAgent(ctx, "没有"); err != nil {
		t.Errorf("删不存在的 agent 报错了：%v", err)
	}
	if err := s.DeleteSkill(ctx, "没有"); err != nil {
		t.Errorf("删不存在的技能报错了：%v", err)
	}
	if err := s.DeleteArkSecret(ctx, "没有"); err != nil {
		t.Errorf("删不存在的凭证报错了：%v", err)
	}
}

// testWorkspaceKeyConflict: creating a workspace with a clashing key reports a conflict; changing your own key does not count as a clash.
func testWorkspaceKeyConflict(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	_, err := s.CreateWorkspace(ctx, apimodel.Workspace{ID: "ws_x", Key: "growth"})
	if err == nil {
		t.Fatal("撞 key 应该报冲突")
	}
	// The error must be recognizable by errors.Is (the handler maps it to 409), and the text must match exactly:
	// the frontend shows message directly to the user.
	if !errors.Is(err, core.ErrConflict) {
		t.Errorf("撞 key 的错误应该是 ErrConflict，实际：%v", err)
	}
	const wantMsg = "空间 Key「growth」已被占用"
	if !strings.Contains(err.Error(), wantMsg) {
		t.Errorf("冲突文案不对，应含 %q，实际 %q", wantMsg, err.Error())
	}

	// Saving itself back (key unchanged) must not error; otherwise every settings save would clash with itself
	cur, _ := s.GetWorkspaceByKey(ctx, "growth")
	cur.Name = "改个名"
	if _, err := s.SaveWorkspace(ctx, cur); err != nil {
		t.Errorf("保存自己报错了：%v", err)
	}
	back, _ := s.GetWorkspace(ctx, cur.ID)
	if back.Name != "改个名" {
		t.Errorf("保存没生效：期望 %q 实际 %q", "改个名", back.Name)
	}

	// Changing to a key someone else holds must conflict
	if _, err := s.CreateWorkspace(ctx, apimodel.Workspace{ID: "ws_y", Key: "other"}); err != nil {
		t.Fatalf("建第二个空间失败：%v", err)
	}
	back.Key = "other"
	if _, err := s.SaveWorkspace(ctx, back); !errors.Is(err, core.ErrConflict) {
		t.Errorf("改成别人的 key 应该冲突，实际：%v", err)
	}
}

// testKeyTakenExceptID: the meaning of exceptId: editing yourself is not a conflict.
func testKeyTakenExceptID(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	taken, _ := s.KeyTaken(ctx, "growth", "")
	if !taken {
		t.Error("growth 应该已被占用")
	}
	ws, _ := s.GetWorkspaceByKey(ctx, "growth")
	taken, _ = s.KeyTaken(ctx, "growth", ws.ID)
	if taken {
		t.Error("排除自己后不该算占用")
	}
	if taken, _ := s.KeyTaken(ctx, "从来没人用过的key", ""); taken {
		t.Error("没人占的 key 不该算占用")
	}
}

// testSaveMissingReturnsNotFound: saving/getting an entity that does not exist must return ErrNotFound.
//
// All three Saves are "overwrite" rather than upsert: they error when missing, instead of quietly creating.
func testSaveMissingReturnsNotFound(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, false)

	if _, err := s.SaveWorkspace(ctx, apimodel.Workspace{ID: "没有", Key: "k"}); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("存不存在的空间应返回 ErrNotFound，实际：%v", err)
	}
	if _, err := s.SaveAgent(ctx, apimodel.Agent{ID: "没有"}); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("存不存在的 agent 应返回 ErrNotFound，实际：%v", err)
	}
	if _, err := s.SaveSkill(ctx, apimodel.Skill{ID: "没有"}); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("存不存在的技能应返回 ErrNotFound，实际：%v", err)
	}
	// Same for the six Gets
	if _, err := s.GetWorkspace(ctx, "没有"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("取不存在的空间应返回 ErrNotFound，实际：%v", err)
	}
	if _, err := s.GetWorkspaceByKey(ctx, "没有"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("按 key 取不存在的空间应返回 ErrNotFound，实际：%v", err)
	}
	if _, err := s.GetAgent(ctx, "没有"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("取不存在的 agent 应返回 ErrNotFound，实际：%v", err)
	}
	if _, err := s.GetSkill(ctx, "没有"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("取不存在的技能应返回 ErrNotFound，实际：%v", err)
	}
	if _, err := s.GetRun(ctx, "没有"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("取不存在的 run 应返回 ErrNotFound，实际：%v", err)
	}
	if _, err := s.GetArkSecret(ctx, "没有"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("取不存在的凭证应返回 ErrNotFound，实际：%v", err)
	}
}

// testAgentsBoundTo looks up bindings in reverse.
//
// **In the seed one Feishu app is bound to only one agent** (1:1 exclusive, see
// ensureFeishuAppsFree in core/agent.go), so only a single one can be asserted here. The Store layer does not enforce 1:1:
// it returns everything it finds; tolerance and warnings for multiple bindings are on the feishu/inbound.go side.
// The contract "returned in storage order" is pinned by testAgentsBoundToOrder with hand-built multi-binding data.
func testAgentsBoundTo(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)
	got, err := s.AgentsBoundTo(ctx, "fs_1")
	if err != nil {
		t.Fatalf("查绑定失败：%v", err)
	}
	if len(got) != 1 || got[0] != "agent_triage" {
		t.Errorf("绑定关系不对：%v", got)
	}
	// fs_2 belongs to agent_analyst; each of the two apps has its own owner.
	other, err := s.AgentsBoundTo(ctx, "fs_2")
	if err != nil {
		t.Fatalf("查绑定失败：%v", err)
	}
	if len(other) != 1 || other[0] != "agent_analyst" {
		t.Errorf("fs_2 的绑定不对：%v", other)
	}
	none, _ := s.AgentsBoundTo(ctx, "fs_没有")
	if len(none) != 0 {
		t.Errorf("没人绑的应用应返回 0 个，实际 %d", len(none))
	}
}

// testAgentsBoundToOrder: when several agents are bound to the same app, they are returned in the agents' storage order.
//
// This shape **is invalid in business terms** (1:1 exclusive), but the Store layer should not filter it itself: it returns what it finds,
// and the upper layer decides what to do. The order matters: the inbound side takes the first one when it meets existing multi-binding data,
// and without order by PostgreSQL may order arbitrarily, so "who answers" would change between two requests,
// and that instability is impossible to diagnose from the symptoms.
//
// The bindings are written straight into the Store (bypassing the Service), because the Service would reject this shape.
func testAgentsBoundToOrder(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	// agent_reviewer comes after agent_triage in the seed, so the expected order is
	// triage first, reviewer after: by ord, not by who was bound first.
	reviewer, err := s.GetAgent(ctx, "agent_reviewer")
	if err != nil {
		t.Fatalf("取 agent_reviewer 失败：%v", err)
	}
	reviewer.FeishuAppIDs = []string{"fs_1"}
	if _, err := s.SaveAgent(ctx, reviewer); err != nil {
		t.Fatalf("存 agent 失败：%v", err)
	}

	got, err := s.AgentsBoundTo(ctx, "fs_1")
	if err != nil {
		t.Fatalf("查绑定失败：%v", err)
	}
	if len(got) != 2 || got[0] != "agent_triage" || got[1] != "agent_reviewer" {
		t.Errorf("多绑时的顺序不对，期望 [agent_triage agent_reviewer]，实际 %v", got)
	}
}

// testSkillNameTaken: the skill name uniqueness check is **per workspace**, and renaming into a clash must be allowed.
func testSkillNameTaken(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	skills, _ := s.ListSkills(ctx, "ws_demo")
	if len(skills) < 2 {
		t.Fatal("种子里应该有至少两个技能")
	}
	name := skills[0].Name

	taken, err := s.SkillNameTaken(ctx, "ws_demo", name)
	if err != nil {
		t.Fatalf("查技能名失败：%v", err)
	}
	if !taken {
		t.Errorf("%q 在 ws_demo 里应该已被占用", name)
	}
	// The same name in another workspace does not count as taken
	if taken, _ := s.SkillNameTaken(ctx, "ws_别的", name); taken {
		t.Error("别的空间的同名技能不该算占用")
	}
	if taken, _ := s.SkillNameTaken(ctx, "ws_demo", "从来没人用过的技能名"); taken {
		t.Error("没人用过的名字不该算占用")
	}

	// **Renaming into a clash is allowed**: uniqueness is only enforced on the Service.CreateSkill side,
	// and Service.UpdateSkill deliberately does not check (copied from the frontend api.ts:428-436);
	// TestSkillNameUniquePerWorkspaceOnly in core/service_test.go pins this down.
	// So the Store's SaveSkill must allow renaming a skill to a name already taken;
	// if the PG implementation added UNIQUE on (workspace_id, name), this would blow up.
	other := skills[1]
	other.Name = name
	if _, err := s.SaveSkill(ctx, other); err != nil {
		t.Errorf("改名撞车应被允许（Service.UpdateSkill 没查重）：%v", err)
	}
}

// testSkillFilesRoundTrip: the storage contract of skill attachments.
//
// ⚠️ **After 4a this layer no longer handles bytes** (the content column is neither written nor read), so "bytes stored
// are read back as is" is no longer this layer's contract; it moved to the core layer (`putSkillFileBlobs` writes into
// object storage, `hydrateSkillFiles` reads from there).
//
// The original item 1 was "bytes read back as is (including binary; a text column would blow up on write)". That argument still
// holds, only **the place that proves it changed**: TestSkillFilesReadPrefersBlobStore in the core layer
// guards the same thing with content containing 0x00/0xFF.
//
// Four behaviors, each a silent failure when wrong:
//
//  1. **No bytes returned**: Content always nil, Size always 0 (missing this would make the 4b drop column produce 42703);
//  2. FileID round-trips as is, read back in ascending path order (injection into containers must be reproducible);
//  3. PutSkillFiles is a **full replacement**, not an append (otherwise deleted files would keep being injected into containers);
//  4. deleting a skill also deletes its attachments (otherwise they become orphan data that can never be read or deleted).
func testSkillFilesRoundTrip(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	skills, _ := s.ListSkills(ctx, "ws_demo")
	if len(skills) < 2 {
		t.Fatal("种子里应该有至少两个技能")
	}
	target := skills[0].ID

	// Passed in out of order on purpose, with every FileID different: with values from the same source,
	// an implementation bug like "assigning a FileID to the wrong path" would also pass the assertions.
	// Content is still passed (that is how callers pass it), but it **must not be stored**; see the assertion below.
	files := []apimodel.SkillFile{
		{Path: "scripts/init.sh", Content: []byte("#!/bin/sh\necho one\n"), FileID: "file_init"},
		{Path: "references/rules.md", Content: []byte("# 规则\n第二份内容\n"), FileID: "file_rules"},
		{Path: "assets/logo.bin", Content: []byte{0x00, 0xFF, 0x01, 0xFE, 0x00}, FileID: "file_logo"},
	}
	if err := s.PutSkillFiles(ctx, target, "ws_demo", files); err != nil {
		t.Fatalf("存附件失败：%v", err)
	}

	got, err := s.SkillFiles(ctx, target)
	if err != nil {
		t.Fatalf("读附件失败：%v", err)
	}
	if len(got) != 3 {
		t.Fatalf("应当读回 3 个附件，实际 %d 个", len(got))
	}
	// Order: assets < references < scripts (by path ascending).
	wantOrder := []string{"assets/logo.bin", "references/rules.md", "scripts/init.sh"}
	for i, want := range wantOrder {
		if got[i].Path != want {
			t.Errorf("第 %d 个附件应当是 %q，实际 %q（顺序必须按 path 升序）", i, want, got[i].Path)
		}
	}
	// Match FileID one by one: it is now the only clue to "where the bytes are", and a mismatch means reading someone else's file.
	wantFileID := map[string]string{
		"scripts/init.sh":     "file_init",
		"references/rules.md": "file_rules",
		"assets/logo.bin":     "file_logo",
	}
	for _, f := range got {
		if f.FileID != wantFileID[f.Path] {
			t.Errorf("%s 的 FileID 应为 %q，实际 %q", f.Path, wantFileID[f.Path], f.FileID)
		}
	}
	// ⭐⭐ **No bytes returned, and no size either.**
	//
	// After 4a this layer does not select content, so both are always zero values. **This must be pinned**:
	// if someone ever adds content back into the SQL, 4b's drop column would produce a batch of
	// 42703 during the rolling window (see that incident in schema/README.md), and that error does not point at all to
	// "someone added a column back into the SQL".
	for _, f := range got {
		if len(f.Content) != 0 {
			t.Errorf("⚠️ %s 带回了 %d 字节——store 层不该再读 content 列，"+
				"这会让 4b drop column 打出 42703", f.Path, len(f.Content))
		}
		if f.Size != 0 {
			t.Errorf("⚠️ %s 的 Size 是 %d——这一层算不出大小（拿不到字节），"+
				"真源是 files.size，由 core 的 hydrateSkillFiles 补", f.Path, f.Size)
		}
	}

	// **Full replacement**: store again with only one file, and the other two must disappear.
	// Written as upsert they would stay and keep being injected into containers, while the UI does not show them.
	if err := s.PutSkillFiles(ctx, target, "ws_demo", []apimodel.SkillFile{
		{Path: "scripts/init.sh", Content: []byte("#!/bin/sh\necho replaced\n"), FileID: "file_replaced"},
	}); err != nil {
		t.Fatalf("替换附件失败：%v", err)
	}
	got, _ = s.SkillFiles(ctx, target)
	if len(got) != 1 {
		t.Fatalf("PutSkillFiles 是全量替换，应当只剩 1 个，实际 %d 个：%+v", len(got), got)
	}
	// The check switches to FileID: the bytes are not in this layer, so "was it overwritten" can only be seen from this pointer.
	if got[0].FileID != "file_replaced" {
		t.Errorf("FileID 应当被覆盖成 file_replaced，实际 %q", got[0].FileID)
	}

	// Other skills are unaffected: isolated by skill_id.
	if other, _ := s.SkillFiles(ctx, skills[1].ID); len(other) != 0 {
		t.Errorf("另一个技能不该有附件，实际 %d 个", len(other))
	}

	// Deleting a skill also deletes its attachments.
	if err := s.DeleteSkill(ctx, target); err != nil {
		t.Fatalf("删技能失败：%v", err)
	}
	if left, _ := s.SkillFiles(ctx, target); len(left) != 0 {
		t.Errorf("删技能后附件必须一起没了，实际还剩 %d 个（孤儿数据）", len(left))
	}

	// -----------------------------------------------------------------------
	// Item 5: **an empty FileID must round-trip as is too** (it must not become a third state other than the empty string)
	// -----------------------------------------------------------------------
	//
	// content is not in the SQL at all, but **an empty FileID must still be storable and read back as an empty string**:
	//
	//   * the PG side must write an empty string as NULL (the column means "is there a matching files row"),
	//     and read it back as an empty string with coalesce;
	//   * the in-memory version just keeps the empty string.
	//
	// If the two implementations diverge here, the core layer's check "has this row been moved" (TrimSpace(FileID) != "")
	// would reach different conclusions on the two Stores, while unit tests run the in-memory version.
	//
	// ⚠️ A row with an empty FileID **is an error in business terms** (putSkillFileBlobs errors when it cannot get a file_id,
	// see core/blob.go), but **the storage layer must still represent it faithfully**, so the core layer can judge it as an error.
	target2 := skills[1].ID
	if err := s.PutSkillFiles(ctx, target2, "ws_demo", []apimodel.SkillFile{
		{Path: "in-blob.md", Content: []byte("字节在 BlobStore 里"), FileID: "file_in_blob"},
		// The shape of dirty data: no file_id.
		{Path: "orphan.md", Content: []byte("这行的字节没搬")},
	}); err != nil {
		t.Fatalf("存混合形态的附件失败：%v", err)
	}
	mixed, err := s.SkillFiles(ctx, target2)
	if err != nil {
		t.Fatalf("读混合形态失败：%v", err)
	}
	if len(mixed) != 2 {
		t.Fatalf("应有 2 个附件，实际 %d", len(mixed))
	}
	for _, f := range mixed {
		switch f.Path {
		case "in-blob.md":
			if f.FileID != "file_in_blob" {
				t.Errorf("in-blob.md 的 FileID 该原样存回，实际 %q", f.FileID)
			}
			// Rows with a file_id carry no bytes: the direct result of 4a stopping reads.
			if len(f.Content) != 0 {
				t.Errorf("in-blob.md 带回了 %d 字节——store 层不该读 content 列", len(f.Content))
			}
		case "orphan.md":
			// ⭐ The check is **an empty string**, not NULL and not some other sentinel value. Both implementations must agree.
			if f.FileID != "" {
				t.Errorf("orphan.md 的 FileID 应当是空串，实际 %q", f.FileID)
			}
			// Neither implementation keeps bytes: skill_files has no byte column.
			if len(f.Content) != 0 {
				t.Errorf("orphan.md 带回了 %d 字节——store 层不存字节", len(f.Content))
			}
		default:
			t.Errorf("多出一个附件：%s", f.Path)
		}
	}
}

// testArkSkillUploadRoundTrip: the contract of Ark skill upload records.
//
// Four behaviors; any one wrong piles up orphan skills on the Ark side or lets the model see old content:
//
//  1. never uploaded returns ErrNotFound (the caller uses it to decide whether to upload);
//  2. what is stored can be fetched back by the (workspace, skill, hash) triple;
//  3. **a repeated Put keeps the first one** (first come, first served), not an overwrite: overwriting would let two instances
//     keep changing it, so the same content resolves to different Ark ids at different times;
//  4. the dimensions are independent: a different workspace, skill or content fingerprint is another record.
func testArkSkillUploadRoundTrip(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	// Never uploaded → ErrNotFound.
	if _, err := s.GetArkSkillUpload(ctx, "ws_demo", "skill_1", "hash_a", "fp_key1"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("没传过应当返回 ErrNotFound，实际 %v", err)
	}

	first := persistmodel.ArkSkillUpload{
		WorkspaceID:    "ws_demo",
		SkillID:        "skill_1",
		ContentHash:    "hash_a",
		KeyFingerprint: "fp_key1",
		ArkSkillID:     "skill-ark-first",
		ArkVersion:     "1",
		CreatedAt:      "2026-08-28T10:00:00.000Z",
	}
	got, err := s.PutArkSkillUpload(ctx, first)
	if err != nil {
		t.Fatalf("存上传记录失败：%v", err)
	}
	if got.ArkSkillID != first.ArkSkillID || got.ArkVersion != first.ArkVersion {
		t.Errorf("Put 应当回显落库的那条，实际 %+v", got)
	}

	read, err := s.GetArkSkillUpload(ctx, "ws_demo", "skill_1", "hash_a", "fp_key1")
	if err != nil {
		t.Fatalf("读上传记录失败：%v", err)
	}
	if read.ArkSkillID != "skill-ark-first" || read.ArkVersion != "1" ||
		read.CreatedAt != first.CreatedAt {
		t.Errorf("读回来的记录对不上：%+v", read)
	}

	// **First come, first served**: Put another one with a different ark_skill_id, and the first must still be returned.
	// This simulates "two instances uploading at once, each getting an id": then whoever is stored first counts,
	// otherwise the same content would resolve to different Ark skills at different times.
	second := first
	second.ArkSkillID = "skill-ark-second"
	second.ArkVersion = "9"
	again, err := s.PutArkSkillUpload(ctx, second)
	if err != nil {
		t.Fatalf("重复 Put 不该报错：%v", err)
	}
	if again.ArkSkillID != "skill-ark-first" {
		t.Errorf("重复 Put 应当保留先到的那条（先到先得），实际拿到 %q", again.ArkSkillID)
	}
	if again.ArkVersion != "1" {
		t.Errorf("版本也该是先到那条的，实际 %q", again.ArkVersion)
	}

	// **The four dimensions are independent**: changing any one should be "never uploaded".
	//
	// ⚠️ The "different key" one came only after agent-level credentials: an Ark skill belongs to the project of the key used to upload it,
	// so switching to another key in the same workspace requires uploading again. Missing this dimension shows up as
	// "getting a skill_id that does not exist in your own project", with an error not pointing at the real cause.
	for _, tc := range []struct {
		name                string
		ws, skill, hash, fp string
	}{
		{"换空间", "ws_other", "skill_1", "hash_a", "fp_key1"},
		{"换技能", "ws_demo", "skill_2", "hash_a", "fp_key1"},
		{"换内容指纹", "ws_demo", "skill_1", "hash_b", "fp_key1"},
		{"换 key", "ws_demo", "skill_1", "hash_a", "fp_key2"},
	} {
		if _, err := s.GetArkSkillUpload(ctx, tc.ws, tc.skill, tc.hash, tc.fp); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("%s 应当是另一条记录（ErrNotFound），实际 %v", tc.name, err)
		}
	}

	// The same content uploaded once with each of two keys: the two records must coexist without affecting each other.
	otherKey := first
	otherKey.KeyFingerprint = "fp_key2"
	otherKey.ArkSkillID = "skill-ark-key2"
	if _, err := s.PutArkSkillUpload(ctx, otherKey); err != nil {
		t.Fatalf("换一把 key 传同一份内容失败：%v", err)
	}
	if got, err := s.GetArkSkillUpload(ctx, "ws_demo", "skill_1", "hash_a", "fp_key2"); err != nil {
		t.Errorf("另一把 key 那条读不到：%v", err)
	} else if got.ArkSkillID != "skill-ark-key2" {
		t.Errorf("两把 key 的记录串了：%+v", got)
	}
	if got, err := s.GetArkSkillUpload(ctx, "ws_demo", "skill_1", "hash_a", "fp_key1"); err != nil {
		t.Errorf("第一把 key 那条被弄丢了：%v", err)
	} else if got.ArkSkillID != "skill-ark-first" {
		t.Errorf("第一把 key 的记录被覆盖了：%+v", got)
	}

	// Another version of the same skill's content can coexist: the old version must be kept, since old conversations may still point to it.
	newVersion := first
	newVersion.ContentHash = "hash_b"
	newVersion.ArkSkillID = "skill-ark-v2"
	if _, err := s.PutArkSkillUpload(ctx, newVersion); err != nil {
		t.Fatalf("存另一版内容失败：%v", err)
	}
	if old, err := s.GetArkSkillUpload(ctx, "ws_demo", "skill_1", "hash_a", "fp_key1"); err != nil {
		t.Errorf("老版本必须留着（可能还有历史会话指着它）：%v", err)
	} else if old.ArkSkillID != "skill-ark-first" {
		t.Errorf("老版本被改了：%+v", old)
	}
}

// testArkSecretRoundTrip: storing and reading credentials, including the plaintext key; a repeated Put overwrites instead of erroring.
func testArkSecretRoundTrip(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, false)

	sec := apimodel.ArkSecret{
		ArkCredential: apimodel.ArkCredential{
			WorkspaceID:  "ws_1",
			ApiKeyMasked: "abcd****wxyz",
			Region:       apimodel.RegionCNBeijing,
			Check:        apimodel.CheckValid,
			CheckedAt:    "2026-08-25T10:00:00.000Z",
			UpdatedBy:    "someone@example.com",
		},
		APIKey: "真的明文key",
	}
	if err := s.PutArkSecret(ctx, sec); err != nil {
		t.Fatalf("写凭证失败：%v", err)
	}
	got, err := s.GetArkSecret(ctx, "ws_1")
	if err != nil {
		t.Fatalf("取凭证失败：%v", err)
	}
	if got.APIKey != sec.APIKey {
		t.Errorf("明文 key 没存住：期望 %q 实际 %q", sec.APIKey, got.APIKey)
	}
	if got.ApiKeyMasked != sec.ApiKeyMasked || got.Region != sec.Region ||
		got.Check != sec.Check || got.CheckedAt != sec.CheckedAt ||
		got.UpdatedBy != sec.UpdatedBy {
		t.Errorf("凭证字段没存全：%+v", got)
	}

	// Putting again overwrites, not a conflict
	sec.APIKey = "换了个key"
	if err := s.PutArkSecret(ctx, sec); err != nil {
		t.Fatalf("覆盖凭证失败：%v", err)
	}
	got, _ = s.GetArkSecret(ctx, "ws_1")
	if got.APIKey != "换了个key" {
		t.Errorf("覆盖没生效：%q", got.APIKey)
	}
}

// testResetAndClear: e2e relies on these two to prepare data.
func testResetAndClear(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	if err := s.Clear(ctx); err != nil {
		t.Fatalf("清空失败：%v", err)
	}
	if ws, _ := s.ListWorkspaces(ctx); len(ws) != 0 {
		t.Error("清空后还有空间")
	}
	// Clearing must clear everything: no events left either
	if e, _ := s.ListRunEvents(ctx, "run_1", persistmodel.EventQuery{}); len(e) != 0 {
		t.Errorf("清空后还有事件，剩 %d 条", len(e))
	}

	if err := s.Reset(ctx); err != nil {
		t.Fatalf("重置失败：%v", err)
	}
	if ws, _ := s.ListWorkspaces(ctx); len(ws) != 1 {
		t.Error("重置后没恢复种子")
	}
	// 44: matches the real content of seed.ts (see the note in testEventFilters).
	if e, _ := s.ListRunEvents(ctx, "run_1", persistmodel.EventQuery{}); len(e) != 44 {
		t.Errorf("重置后事件没恢复，实际 %d 条", len(e))
	}

	// Repeated resets must not leak into each other: change something and reset, and it should go back to the original
	a, _ := s.GetAgent(ctx, "agent_triage")
	original := a.Name
	a.Name = "改过"
	_, _ = s.SaveAgent(ctx, a)
	_ = s.Reset(ctx)
	back, _ := s.GetAgent(ctx, "agent_triage")
	if back.Name != original {
		t.Errorf("reset 没恢复干净：期望 %q 实际 %q", original, back.Name)
	}
	// After reset the bindings must return to the seed shape too (fs_1 belongs to agent_triage, and only to it)
	bound, _ := s.AgentsBoundTo(ctx, "fs_1")
	if len(bound) != 1 || bound[0] != "agent_triage" {
		t.Errorf("reset 后 fs_1 的绑定不对：%v", bound)
	}
}

// testConcurrentAccess: concurrent reads and writes must have no data race. Only meaningful with -race.
func testConcurrentAccess(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			// Half reads, half writes, all on the same data
			if n%2 == 0 {
				a, err := s.GetAgent(ctx, "agent_triage")
				if err != nil {
					return
				}
				// Serialize the return value: if the store handed out internal pointers, this would race with the writers
				_, _ = json.Marshal(a)
				_, _ = s.ListRunEvents(ctx, "run_1", persistmodel.EventQuery{})
				return
			}
			a, err := s.GetAgent(ctx, "agent_triage")
			if err != nil {
				return
			}
			a.SkillIDs = append(a.SkillIDs, "新技能")
			_, _ = s.SaveAgent(ctx, a)
		}(i)
	}
	wg.Wait()
}

// -- Order of workspace members ----------------------------------------------------------

// testMemberOrderIsStable pins "the order of listed members is deterministic".
//
// # What this guards against is not "the list looks messy" but injecting the wrong person's credential
//
// firstOwnerEmail in core/ark.go takes **the first owner**, and core/credentialinject.go
// uses it to decide whose credential to inject into the agent's container. Members used to live in the doc's jsonb array,
// naturally ordered; after moving into a join table **rows have no inherent order**: without an explicit order by,
// PostgreSQL may return them in a different order after any VACUUM, page split or parallel scan.
//
// So in a workspace with several owners **the person whose credential is injected changes randomly**, with zero errors: the logs look normal,
// just sometimes using A's permissions and sometimes B's. So this reads 20 times, and all must be the same.
func testMemberOrderIsStable(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	ws, err := s.GetWorkspace(ctx, seedWS)
	if err != nil {
		t.Fatalf("取空间失败：%v", err)
	}
	if len(ws.Members) < 2 {
		t.Fatalf("种子空间应有多个成员，实际 %d", len(ws.Members))
	}
	want := make([]string, 0, len(ws.Members))
	for _, m := range ws.Members {
		want = append(want, m.ID)
	}

	for i := 0; i < 20; i++ {
		got, err := s.GetWorkspace(ctx, seedWS)
		if err != nil {
			t.Fatalf("第 %d 次取空间失败：%v", i, err)
		}
		if len(got.Members) != len(want) {
			t.Fatalf("第 %d 次成员数变了：%d → %d", i, len(want), len(got.Members))
		}
		for j, m := range got.Members {
			if m.ID != want[j] {
				t.Fatalf("第 %d 次成员顺序变了：位置 %d 期望 %s，实际 %s",
					i, j, want[j], m.ID)
			}
		}
	}

	// **The order must be the contract "ascending by join time" itself, not just "two reads are the same".**
	//
	// Comparing only "two reads are the same" is not enough: with few rows PostgreSQL scans the heap sequentially and happens to return
	// insertion order, so **removing order by is still green** (confirmed by measurement, which is why this assertion was added).
	// Only asserting the real ordering contract is independent of physical storage.
	for i := 1; i < len(ws.Members); i++ {
		if ws.Members[i-1].JoinedAt > ws.Members[i].JoinedAt {
			t.Errorf("成员没有按入组时间升序：位置 %d 的 %s 晚于位置 %d 的 %s",
				i-1, ws.Members[i-1].JoinedAt, i, ws.Members[i].JoinedAt)
		}
	}

	// The ListWorkspaces path must agree too: it is a different query from GetWorkspace.
	all, err := s.ListWorkspaces(ctx)
	if err != nil {
		t.Fatalf("列空间失败：%v", err)
	}
	for _, w := range all {
		if w.ID != seedWS {
			continue
		}
		for j, m := range w.Members {
			if m.ID != want[j] {
				t.Errorf("ListWorkspaces 的成员顺序与 GetWorkspace 不一致："+
					"位置 %d 期望 %s，实际 %s", j, want[j], m.ID)
			}
		}
	}
}

// testMemberRoleUpdateKeepsOrder pins "changing one member's role does not shuffle the order".
//
// The PG side does a diff sync (syncMembersTx). Written as "delete all then insert all", every save would rewrite
// all member rows with created_at refreshed, so the list order would change with every edit,
// and for the reason in the previous test, that would change who firstOwnerEmail picks.
func testMemberRoleUpdateKeepsOrder(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	ws, err := s.GetWorkspace(ctx, seedWS)
	if err != nil {
		t.Fatalf("取空间失败：%v", err)
	}
	if len(ws.Members) < 3 {
		t.Fatalf("这条用例需要至少 3 个成员，实际 %d", len(ws.Members))
	}
	before := make([]string, 0, len(ws.Members))
	for _, m := range ws.Members {
		before = append(before, m.ID)
	}

	// **Change the role of the first member, not the last.**
	//
	// PostgreSQL's UPDATE is "mark the old version + insert a new version at the end of the heap", so an updated row
	// **moves to the end** in queries without order by. Changing the last one, it was at the end anyway,
	// and the order would look unchanged; this test would then still be green when "order by was forgotten",
	// passing by luck (confirmed by measurement: the first version picked the last one, and removing order by did not turn it red).
	idx := 0
	target := ws.Members[idx].ID
	newRole := apimodel.RoleMember
	if ws.Members[idx].Role == apimodel.RoleMember {
		newRole = apimodel.RoleViewer
	}
	ws.Members[idx].Role = newRole
	if _, err := s.SaveWorkspace(ctx, ws); err != nil {
		t.Fatalf("存空间失败：%v", err)
	}

	got, err := s.GetWorkspace(ctx, seedWS)
	if err != nil {
		t.Fatalf("回读空间失败：%v", err)
	}
	if len(got.Members) != len(before) {
		t.Fatalf("改角色之后成员数变了：%d → %d", len(before), len(got.Members))
	}
	// Same as above: assert the ordering contract itself, not just "same as before the change".
	for i := 1; i < len(got.Members); i++ {
		if got.Members[i-1].JoinedAt > got.Members[i].JoinedAt {
			t.Errorf("改角色之后不再按入组时间升序：%s 晚于 %s",
				got.Members[i-1].JoinedAt, got.Members[i].JoinedAt)
		}
	}
	for j, m := range got.Members {
		if m.ID != before[j] {
			t.Errorf("改角色打乱了顺序：位置 %d 期望 %s，实际 %s", j, before[j], m.ID)
		}
	}
	for _, m := range got.Members {
		if m.ID == target && m.Role != newRole {
			t.Errorf("角色没改成功：期望 %s，实际 %s", newRole, m.Role)
		}
	}
}

// testMemberFieldsSurviveRoundTrip pins "every field of a member is read back as stored".
//
// # This was found by starting the real service locally, while all unit tests were green at the time
//
// When members were moved from doc into a join table, the table was built as "id / workspace_id / member_email / role /
// created_at / updated_at", **missing name and avatar_url**, while apimodel.WorkspaceMember
// has these two fields and the UI really renders them.
//
// Symptom: after changing a member's role once, the response had `"name": ""`, and that person showed only an email in the member list.
// **No error at all**, and at the time the ordering, counting and role tests were all green: none of them compared field contents.
//
// So this compares field by field, instead of only looking at "how many members and in what order".
func testMemberFieldsSurviveRoundTrip(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	ws, err := s.GetWorkspace(ctx, seedWS)
	if err != nil {
		t.Fatalf("取空间失败：%v", err)
	}
	if len(ws.Members) == 0 {
		t.Fatal("种子空间应该有成员")
	}

	// The names in the seed must be readable (with a missing column this would be an empty string).
	for _, m := range ws.Members {
		if m.Name == "" {
			t.Errorf("成员 %s 的 name 是空的——存储漏了这个字段？", m.Email)
		}
		if m.Email == "" {
			t.Errorf("成员 %s 的 email 是空的", m.ID)
		}
		if m.Role == "" {
			t.Errorf("成员 %s 的 role 是空的", m.Email)
		}
		if m.JoinedAt == "" {
			t.Errorf("成员 %s 的 joinedAt 是空的", m.Email)
		}
	}

	// Write a member with **every field different**, then read back and compare field by field.
	// The values are deliberately all different: with the same string everywhere, fields landing in the wrong place (name written into the email column)
	// would still pass.
	added := apimodel.WorkspaceMember{
		ID:        "m_roundtrip",
		Name:      "往返测试员",
		Email:     "roundtrip@example.com",
		Role:      apimodel.RoleViewer,
		AvatarURL: "https://example.com/roundtrip.png",
		JoinedAt:  "2026-08-30T01:02:03.456Z",
	}
	ws.Members = append(ws.Members, added)
	if _, err := s.SaveWorkspace(ctx, ws); err != nil {
		t.Fatalf("存空间失败：%v", err)
	}

	back, err := s.GetWorkspace(ctx, seedWS)
	if err != nil {
		t.Fatalf("回读空间失败：%v", err)
	}
	var got *apimodel.WorkspaceMember
	for i := range back.Members {
		if back.Members[i].Email == added.Email {
			got = &back.Members[i]
			break
		}
	}
	if got == nil {
		t.Fatal("新加的成员读不回来")
	}
	if got.Name != added.Name {
		t.Errorf("name 没存住：期望 %q，实际 %q", added.Name, got.Name)
	}
	if got.AvatarURL != added.AvatarURL {
		t.Errorf("avatarUrl 没存住：期望 %q，实际 %q", added.AvatarURL, got.AvatarURL)
	}
	if got.Role != added.Role {
		t.Errorf("role 没存住：期望 %q，实际 %q", added.Role, got.Role)
	}
	if got.JoinedAt != added.JoinedAt {
		t.Errorf("joinedAt 没存住：期望 %q，实际 %q", added.JoinedAt, got.JoinedAt)
	}
	if got.ID != added.ID {
		t.Errorf("id 没存住：期望 %q，实际 %q", added.ID, got.ID)
	}
}

// testWorkspaceOwnFieldsSurviveRoundTrip pins that the workspace's **own three fields** round-trip as is:
// name / createdBy / updatedBy.
//
// # Why this exists separately instead of relying on SeedRoundTrip
//
// These three fields were just pulled out into separate columns in step one of "splitting doc" (see the end of schema/workspaces.sql).
// The PG side now **writes both**: doc and the columns each get a copy, while the read path **still only reads doc**,
// so "a column missed on write" gives **zero errors** in this round, and only blows up as "all workspace names empty" when the reads switch over next round.
//
// This test guards the other half: the doc copy must not be broken by the dual-write change. The column half is guarded by
// workspacecolumns_pg_test.go querying the columns directly; only together are they complete.
//
// # The values must all differ
//
// With the same string in all three fields, a swap like "createdBy written into updatedBy" would still pass,
// for the same reason as at the top of testMemberFieldsSurviveRoundTrip.
func testWorkspaceOwnFieldsSurviveRoundTrip(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	ws, err := s.GetWorkspace(ctx, seedWS)
	if err != nil {
		t.Fatalf("取空间失败：%v", err)
	}
	if ws.Name == "" {
		t.Error("种子空间的 name 是空的——存储漏了这个字段？" +
			"下一轮读路径切到列之后，这就是「界面上空间名全空」")
	}

	ws.Name = "往返测试空间"
	ws.CreatedBy = "creator@example.com"
	ws.UpdatedBy = "editor@example.com"
	if _, err := s.SaveWorkspace(ctx, ws); err != nil {
		t.Fatalf("存空间失败：%v", err)
	}

	back, err := s.GetWorkspace(ctx, seedWS)
	if err != nil {
		t.Fatalf("回读空间失败：%v", err)
	}
	if back.Name != ws.Name {
		t.Errorf("name 没存住：期望 %q，实际 %q", ws.Name, back.Name)
	}
	if back.CreatedBy != ws.CreatedBy {
		t.Errorf("createdBy 没存住：期望 %q，实际 %q", ws.CreatedBy, back.CreatedBy)
	}
	if back.UpdatedBy != ws.UpdatedBy {
		t.Errorf("updatedBy 没存住：期望 %q，实际 %q——"+
			"与 createdBy 串位的话两个值会一样", ws.UpdatedBy, back.UpdatedBy)
	}

	// Create a new workspace and read it back: CreateWorkspace and SaveWorkspace are **two** write paths,
	// and only the latter was checked above.
	fresh := apimodel.Workspace{
		ID: "ws_roundtrip", Key: "roundtrip", Name: "新建往返空间",
		CreatedAt: apimodel.NowISO(), UpdatedAt: apimodel.NowISO(),
		CreatedBy: "maker@example.com", UpdatedBy: "maker@example.com",
	}
	if _, err := s.CreateWorkspace(ctx, fresh); err != nil {
		t.Fatalf("建空间失败：%v", err)
	}
	got, err := s.GetWorkspace(ctx, fresh.ID)
	if err != nil {
		t.Fatalf("回读新空间失败：%v", err)
	}
	if got.Name != fresh.Name {
		t.Errorf("新建空间的 name 没存住：期望 %q，实际 %q", fresh.Name, got.Name)
	}
	if got.CreatedBy != fresh.CreatedBy {
		t.Errorf("新建空间的 createdBy 没存住：期望 %q，实际 %q", fresh.CreatedBy, got.CreatedBy)
	}
}

// testGetFilesBatch is the contract of fileStore.GetFiles.
//
// **It must run once on each implementation**: this behavior is implemented by different mechanisms on the two sides (PG relies on expanded
// `in ($1,$2,…)`, the in-memory version on map lookups), while unit tests usually run the in-memory version, so the PG one's
// correctness goes unwatched. The skill file read path (hydrateSkillFiles in core/blob.go)
// calls it every time it injects into a container.
//
// It locks four things; each gives no error when wrong:
//
//  1. **missing ids are skipped silently without error**, deliberately different from GetFile's ErrNotFound.
//     Erroring would let a few missing files waste the whole batch that could be read;
//  2. **the returned map is indexed by id with correct content**: a mismatch would let file A read B's bytes;
//  3. **empty input does not hit the database** (`in ()` is a syntax error in PG);
//  4. **duplicate ids and empty-string ids do not blow up**.
//
// ⚠️ **No mutation can kill item 4** (removing the dedup on the PG side leaves the test green), because placeholder
// numbers come from len(args) and cannot mismatch the parameter count. It stays to pin the promise to callers that
// "passing them in will not blow up", not to protect the dedup logic itself. Do not treat it as a guard for dedup.
func testGetFilesBatch(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, false)

	recs := []persistmodel.FileRecord{
		{ID: "file_a", Bucket: "b", ObjectKey: "k/a", Filename: "a.md",
			Size: 3, ContentSHA256: "sha-a", CreatedAt: apimodel.NowISO()},
		{ID: "file_b", Bucket: "b", ObjectKey: "k/b", Filename: "b.md",
			Size: 5, ContentSHA256: "sha-b", CreatedAt: apimodel.NowISO()},
		{ID: "file_c", Bucket: "b", ObjectKey: "k/c", Filename: "c.md",
			Size: 7, ContentSHA256: "sha-c", CreatedAt: apimodel.NowISO()},
	}
	for _, r := range recs {
		if err := s.CreateFile(ctx, r); err != nil {
			t.Fatalf("建文件 %s 失败：%v", r.ID, err)
		}
	}

	// One. Empty input: returns an empty map without error (on the PG side `in ()` is a syntax error, so it must short-circuit).
	got, err := s.GetFiles(ctx, nil)
	if err != nil {
		t.Fatalf("空入参不该报错：%v", err)
	}
	if len(got) != 0 {
		t.Errorf("空入参应返回空 map，实际 %d 条", len(got))
	}

	// Two. Query with a mix of "existing", "missing", "empty string" and "duplicate".
	got, err = s.GetFiles(ctx, []string{"file_a", "file_nope", "", "file_c", "file_a"})
	if err != nil {
		t.Fatalf("批量查失败：%v", err)
	}
	// Missing ones do not error and do not appear in the result.
	if len(got) != 2 {
		t.Fatalf("应只返回查到的 2 条（file_a / file_c），实际 %d 条：%v", len(got), got)
	}
	if _, ok := got["file_nope"]; ok {
		t.Error("不存在的 id 不该出现在结果里")
	}
	// Three. The content matches: indexed by id, not by order.
	if a := got["file_a"]; a.ObjectKey != "k/a" || a.Filename != "a.md" || a.Size != 3 {
		t.Errorf("file_a 的内容不对：%+v", a)
	}
	if c := got["file_c"]; c.ObjectKey != "k/c" || c.Filename != "c.md" || c.Size != 7 {
		t.Errorf("file_c 的内容不对：%+v", c)
	}

	// Four. When all are found, none is missing.
	got, err = s.GetFiles(ctx, []string{"file_a", "file_b", "file_c"})
	if err != nil {
		t.Fatalf("批量查失败：%v", err)
	}
	if len(got) != 3 {
		t.Errorf("应返回 3 条，实际 %d 条", len(got))
	}
}

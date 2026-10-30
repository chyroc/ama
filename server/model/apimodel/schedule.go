// Domain model of scheduled tasks (Schedule): the data shapes of the proactive capability.
//
// # What this domain solves
//
// Before this, agents only answered passively: they ran a turn only when someone sent a message on the chat page or @-mentioned them in a Feishu group.
// This domain gives them initiative, with these triggers:
//
//	cron      automatic inspection at 9:30 on workdays, sending the report back to a Feishu group
//	interval  polling at a fixed interval
//	oneshot   "remind me to check the release result in 1 hour"
//
// # One firing is one Run
//
// The same approach as the chat page (see the file header of core/chat.go): **deliberately no new "execution record" entity**.
// When the time comes, a Run with Trigger schedule is started, which gives three things for free: it goes into run
// history automatically, you can click in to see the full timeline of the 33 event kinds, and you can continue chatting from history. Schedule only keeps
// LastRunID pointing to it.
//
// **Claiming must not use an in-process sync.Map**: this repo runs as multiple instances, and claiming relies on the database; see core/scheduler.go.
package apimodel

// ---------------------------------------------------------------------------
// Trigger kinds
// ---------------------------------------------------------------------------

// ScheduleKind decides how "when is the next run" is computed.
type ScheduleKind string

const (
	// ScheduleCron is a 5-field cron expression (minute hour day month weekday), with a minimum granularity of 1 minute.
	// Paired with ScheduleSpec.TZ: "9:30 on workdays" as written means local time.
	ScheduleCron ScheduleKind = "cron"
	// ScheduleInterval is a fixed interval, decided by EveryMs.
	//
	// **The interval counts from "the end of the previous run", not a fixed grid**: one agent chat turn may take tens of seconds or
	// several minutes, and a fixed grid would fire again right after a slow turn, eating up the interval.
	ScheduleInterval ScheduleKind = "interval"
	// ScheduleOneshot is a one-time trigger; FireAt is the desired moment. **Deleted after it runs.**
	ScheduleOneshot ScheduleKind = "oneshot"
)

// ScheduleOrigin marks who scheduled this task, for audit and for telling them apart in the UI.
//
// ⚠️ **Currently OriginUser is the only value**, because agent self-wakeup (`agent_self`) was removed.
// The field is kept instead of being removed too: it is in the HTTP contract, and "who scheduled it" will have a second answer some day.
// Existing `agent_self` data was rewritten to `user` by the migration in schedules.sql.
type ScheduleOrigin string

const (
	// OriginUser means a person created it in the UI.
	OriginUser ScheduleOrigin = "user"
)

// ScheduleStatus is the on/off state of a task.
type ScheduleStatus string

const (
	ScheduleActive ScheduleStatus = "active"
	// SchedulePaused is paused. Reaching the threshold of consecutive failures switches to it automatically (to prevent avalanches);
	// when resuming, ConsecutiveFailures must be reset to zero, otherwise it would be judged as needing a pause again right after resuming.
	SchedulePaused ScheduleStatus = "paused"
)

// DefaultScheduleTZ is the default time zone for cron.
//
// **UTC cannot be the default**: when users write "9:30 on workdays" they mean local time; storing UTC would make people convert it themselves
// (or tell the agent in the system prompt to convert by hand, which is easy to get wrong),
// and daylight saving time would make a hard-coded UTC offset drift twice a year.
const DefaultScheduleTZ = "Asia/Shanghai"

// MinScheduleIntervalMs is the lower limit for interval and oneshot delays, 1 minute.
//
// Anything shorter is pointless: the scheduler's scan period itself is on the order of seconds, and one agent chat turn (create session → inference →
// call tools → reply) takes well over a minute, so scheduling more densely would only pile up on "the previous turn is still running" and be skipped.
const MinScheduleIntervalMs int64 = 60_000

// ScheduleSpec is the trigger expression; one of three, by Kind.
//
// A flat union instead of three sub-structs: clean JSON, and all three payload fields have omitempty,
// so the spec of a cron task does not contain unrelated keys like everyMs / fireAt.
type ScheduleSpec struct {
	Kind ScheduleKind `json:"kind"`
	// Cron is the 5-field expression; required when Kind is cron.
	Cron string `json:"cron,omitempty"`
	// TZ is the time zone used to interpret Cron (an IANA name, like Asia/Shanghai). Empty means DefaultScheduleTZ.
	//
	// Only meaningful for cron: interval is a relative duration, and oneshot's FireAt is an absolute moment; neither depends on
	// the time zone.
	TZ string `json:"tz,omitempty"`
	// EveryMs is the interval in milliseconds; required when Kind is interval, and must not be less than MinScheduleIntervalMs.
	EveryMs int64 `json:"everyMs,omitempty"`
	// FireAt is the moment of a one-time trigger (a fixed-length ISO string); required when Kind is oneshot.
	FireAt string `json:"fireAt,omitempty"`
}

// ---------------------------------------------------------------------------
// Result delivery
// ---------------------------------------------------------------------------

// DeliveryKind is where the result goes.
type DeliveryKind string

const (
	// DeliveryRunOnly does not push anywhere; after running it is just one Run in the run history.
	//
	// This is **the common base of all tasks**: whether or not it is delivered to Feishu, the Run is always stored.
	// Tasks created in the UI use it by default.
	DeliveryRunOnly DeliveryKind = "run_only"
	// DeliveryFeishu sends the result back to Feishu.
	//
	// The goal is not to make users look up a chat_id in Feishu and fill it in, but to **record where this task came from**:
	// if it came from a group, send back to that group; if it came from a thread, send back to that thread.
	DeliveryFeishu DeliveryKind = "feishu"
)

// ⚠️ There used to be a third value `issue`: the server parsed a JSON array from the agent's output, computed a sha256
// fingerprint by key, and created issues on its behalf (the same problem a second time became a comment). **It was removed**; do not add it back.
//
// It was removed because that JSON contract was too fragile: a parse failure wasted the whole inspection round, and the cause (the model missed
// a bracket, wrapped the array in an object) had nothing to do with "does the platform have a problem": one formatting slip turned a whole
// health check into one "failed to parse inspection result", and nobody knew whether the platform was healthy or not.
//
// Now the inspection agent creates them itself with `ama issue create` / `ama issue comment`, using run_only.
// The dedup responsibility moved to the agent along with it, relying on **a fixed identifier prefix in the title**
// (`[inspection] <identifier>: ...`, with the identifier chosen from a fixed list); the rules are in the agent's system prompt.
// The old lesson still holds under the new approach: if two rounds give different identifiers for the same thing, two duplicate
// issues are created instead of one follow-up comment; only the executor changed from the server to the agent.

// DeliveryTarget is the delivery address of the result, **frozen** when the task is created.
//
// # Why frozen instead of computed at fire time
//
// When the time comes, this round is a brand new
// agent session, without any context of "where that message came from back then". Without freezing, we could only let the agent
// guess the delivery address itself, and a wrong guess reports no error: the report just goes to another group.
//
// It deliberately **stores no credentials**: Feishu's tenant_access_token expires in two hours, and the AppSecret should even less have
// a copy here. Only FeishuAppID is recorded, and at fire time the feishu package exchanges for a token on the spot.
type DeliveryTarget struct {
	Kind DeliveryKind `json:"kind"`
	// FeishuAppID references feishu.App.ID; required when Kind is feishu.
	FeishuAppID string `json:"feishuAppId,omitempty"`
	// ChatID is the Feishu chat ID (starting with oc_); required when Kind is feishu.
	ChatID string `json:"chatId,omitempty"`
	// ThreadID being non-empty means this task came from a thread.
	//
	// The requirement "send back to the thread if it came from a Feishu thread" rests on this field. But **it alone cannot post into the thread**:
	// Feishu's send message API only takes chat_id; to land in a thread you must "reply to some message in the thread"
	// (reply + reply_in_thread), so AnchorMessageID is also needed.
	// It is still stored here because it decides "should we reply in the thread".
	ThreadID string `json:"threadId,omitempty"`
	// AnchorMessageID is the message replied to at delivery time (om_xxx), usually the user message that originally triggered this
	// task.
	//
	// **Only with it can we post into the thread** (see ThreadID). If empty, it degrades to posting straight to the chat:
	// the content is still in the group, just not in the thread, which is an acceptable degradation.
	AnchorMessageID string `json:"anchorMessageId,omitempty"`
	// ChatName is a snapshot of the group name, for display only (the list page shows "where it goes").
	//
	// Being a snapshot, it goes stale: if the group is renamed this is still the old one. **Do not make any decision with it**;
	// delivery only uses ChatID. Not getting the group name is not an error either; the UI falls back to showing the ID.
	ChatName string `json:"chatName,omitempty"`
}

// ---------------------------------------------------------------------------
// Schedule
// ---------------------------------------------------------------------------

// Schedule is one scheduled task.
//
// Timestamps are all **fixed-length ISO strings** (the NowISO shape), not time.Time, for the same reason as Run.StartedAt;
// see the comment at the top of this package. The only exception is that the scheduler uses time.Time internally to compute the next firing,
// an intermediate state converted back to a string right after.
type Schedule struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspaceId"`
	AgentID     string `json:"agentId"`
	// AgentName is a snapshot of the name when the task was created.
	//
	// Same as Run.AgentName: after the agent is deleted, the list page must still be able to say whom this task originally belonged to,
	// otherwise users facing a failed task from "nobody knows who" have nowhere to start.
	AgentName string `json:"agentName"`
	// Name is the task name, for display (the Feishu card title also uses it).
	Name string       `json:"name"`
	Spec ScheduleSpec `json:"spec"`
	// Prompt is the instruction fed to the agent when the time comes.
	Prompt string `json:"prompt"`
	// ContextSummary is a summary of the chat context at the moment the task was created; at fire time it is **put in front of Prompt**.
	//
	// Why it exists: every firing is a brand new session with zero history. When a user says in a chat "check the release result
	// for me in 1 hour", they have a whole turn of context in mind (which service, what to do if it failed),
	// while the new session at that time knows nothing.
	//
	// The sessionID is not reused to continue context: once the session is reclaimed for being idle, that ID can no longer be used,
	// so the context is carried along as text.
	ContextSummary string         `json:"contextSummary,omitempty"`
	Origin         ScheduleOrigin `json:"origin"`
	Status         ScheduleStatus `json:"status"`
	Delivery       DeliveryTarget `json:"delivery"`

	// -- Scheduling state -----------------------------------------------------------

	// NextRunAt is the next firing moment. Empty when paused.
	//
	// **It is cleared at the moment of claiming** (see the claim SQL in core/scheduler.go): if kept,
	// a firing runs for several minutes, and every scan period in between would see "it is due" and fire again.
	// Blocking this with an in-process running map does not work with multiple instances.
	NextRunAt string `json:"nextRunAt,omitempty"`
	// RunNowRequested means "someone clicked try-run; this firing was requested by hand", and **it is consumed at the moment of claiming**.
	//
	// # Why this extra field is really needed
	//
	// Claiming only takes active tasks (paused ones do not run), while "configure first, click once to see if it is right, then enable" requires
	// a paused task to be test-runnable once too. These two cannot be told apart by NextRunAt alone: when paused it is cleared,
	// but **any** action that fills it back looks the same, so either test-run does nothing for paused (the state before this field:
	// run-now returned 200 and NextRunAt was updated, but the scheduler never claimed it,
	// showing as "clicked try-run and nothing happened", with zero errors), or it is loosened to "also claim if paused and due",
	// and then every task would be run immediately **at the moment it is paused** (the NextRunAt left from before pausing is still in the past).
	// So "requested by hand" must be marked explicitly.
	//
	// # It is one-time, and only takes effect paired with a pending NextRunAt
	//
	// The claim condition is `(active or this field) and NextRunAt non-empty and due`, and claiming clears both
	// NextRunAt **and this field**. So: pausing a task clears NextRunAt, and a test-run request not yet consumed
	// becomes void (the user clicked pause, which is what they want); and a leftover true will not make the task
	// run an extra round out of nowhere: without NextRunAt it cannot be claimed.
	//
	// **Test-run does not change Status**: when the finish side sees it is still paused, it does not schedule the next one
	// (see finish in core/scheduler.go). So a test-run does not turn a paused task into a periodic task.
	RunNowRequested bool   `json:"runNowRequested,omitempty"`
	LastRunAt       string `json:"lastRunAt,omitempty"`
	// LastRunID points to the Run produced by the previous firing; the UI uses it to jump to the run detail.
	LastRunID string `json:"lastRunId,omitempty"`
	// LastError is the reason for the last failure, cleared on success.
	LastError string `json:"lastError,omitempty"`
	// RunCount / FailCount are cumulative counts. **No omitempty**: 0 is a valid value,
	// and a missing key would make the frontend compute NaN (same as the four TokenUsage fields; see hard rule two at the top of this package).
	RunCount  int `json:"runCount"`
	FailCount int `json:"failCount"`
	// ConsecutiveFailures is the number of consecutive failures; at MaxConsecutiveFailures the task pauses automatically.
	ConsecutiveFailures int    `json:"consecutiveFailures"`
	CreatedAt           string `json:"createdAt"`
	UpdatedAt           string `json:"updatedAt"`

	// CreatedBy / UpdatedBy are the emails of the person who created this task and of the most recent editor.
	// Exactly the same meaning as the two on Agent (including both reasons for omitempty); see that long comment in apimodel.go.
	//
	// ⚠️ **For existing tasks these two fields may be empty; that is not a missed fill.** First, early tasks never recorded them;
	// second, the removed agent self-wakeup tasks were stored directly by a tool without going through Service, and on that path
	// `auth.ActorFrom` is always an empty string. **Do not use "has a value or not" as the check** to infer where a task came from.
	CreatedBy string `json:"createdBy,omitempty"`
	UpdatedBy string `json:"updatedBy,omitempty"`

	// AgentDeleted means the linked agent has been deleted, and this task can never produce a result again.
	//
	// **A derived field, not stored**, exactly the same as Run.AgentDeleted (including the reason for that omitempty
	// exception): Agents are hard-deleted with no deleted_at, so it can only be computed when Service reads it.
	// Stored, it would become a stale snapshot that never updates, with no symptom at all.
	AgentDeleted bool `json:"agentDeleted,omitempty"`
}

// MaxConsecutiveFailures is the threshold for automatic pausing.
//
// Avalanche prevention: a task with a wrong prompt, or whose agent was deleted, fails once every period, and each time it really
// creates an Ark session, burns tokens, and may post a failure card to a group. After this many in a row it stops and waits for a person.
const MaxConsecutiveFailures = 5

// ScheduleDraft is the input for creating/updating a task.
//
// Value types instead of a bunch of pointers: the frontend sends the full object on update (same trade-off as AgentDraft).
//
// ⚠️ **Deliberately no CreatedBy / UpdatedBy**, same as AgentDraft / SkillDraft: the request body can be changed by the caller,
// and accepting it would allow "scheduling tasks in someone else's name". The value only comes from auth.ActorFrom(ctx).
type ScheduleDraft struct {
	AgentID        string         `json:"agentId"`
	Name           string         `json:"name"`
	Spec           ScheduleSpec   `json:"spec"`
	Prompt         string         `json:"prompt"`
	ContextSummary string         `json:"contextSummary,omitempty"`
	Delivery       DeliveryTarget `json:"delivery"`
	// Status may be paused already at creation (configure first, enable later). Empty means active.
	Status ScheduleStatus `json:"status,omitempty"`
}

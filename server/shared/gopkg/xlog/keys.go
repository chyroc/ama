// Field keys for structured logs across the whole call chain, managed in this one file.
//
// # Why keep them in one place
//
// CtxAddKVs in logs/v2 is a free-form "string key -> value" KV. Once each call site builds its own
// key names, the same thing shows up as run_id / runID / runId / ama_run, which is the same as not
// being structured at all: when you try to link a whole call chain with one session id, half the
// logs do not match.
//
// So every entity id key that goes into ctx is defined as a constant here. Call sites must use
// these constants and may not write bare string literals. Grep one constant to find "which layers
// attach this id".
//
// # Naming rules
//
// Keys carry a platform prefix to avoid clashing with same-named fields from third-party SDKs or
// other systems (searching ama_run_id only hits ours):
//
//   - ama_*: AMA's own entities (database / business-side ids);
//   - ma_*: entities on the Ark Managed Agents side (the ark runtime ids);
//   - no prefix: execution identities shared across systems (worker_id and so on).
package xlog

// AMA-side entity ids (business/storage layer).
const (
	// KAgentID is AMA's own agent id (like agent_xxx). One of the two most important keys.
	KAgentID = "ama_agent_id"
	// KRunID is the AMA / Ark session run id (like sesn-xxx). One of the two most important keys.
	KRunID = "ama_run_id"
	// KWorkspaceID is the workspace id (like ws_xxx).
	KWorkspaceID = "ama_workspace_id"
	// KSkillID is the AMA skill id.
	KSkillID = "ama_skill_id"
	// KLarkAppID is the Feishu app id.
	KLarkAppID = "ama_lark_app_id"
	// KThreadID is the AMA session thread id (sth-xxx).
	KThreadID = "ama_thread_id"
	// KEventID is the Ark event id (sevt-xxx / call_xxx).
	KEventID = "ama_event_id"
	// KToolName is the name of the called tool (bash/read/...).
	KToolName = "ama_tool_name"
	// KToolUseID is the id of one tool call issued by Ark (call_xxx).
	KToolUseID = "ama_tool_use_id"
	// KWorkID is the id of one work item in the Ark work queue.
	KWorkID = "ama_work_id"
	// KScheduleID is the schedule id.
	KScheduleID = "ama_schedule_id"
	// KIssueID is the feedback/issue id.
	KIssueID = "ama_issue_id"
	// KFeishuChatID is the Feishu group/chat id.
	KFeishuChatID = "ama_feishu_chat_id"
	// KUserID is the AMA-side operator (employee email).
	KUserID = "ama_actor_email"
	// KSandboxSessionID is the session identifier of the execution environment container.
	KSandboxSessionID = "ama_sandbox_session_id"
	// KSandboxID is the execution environment config id.
	KSandboxID = "ama_sandbox_id"
	// KHostID is the host executor id.
	KHostID = "ama_host_id"
)

// Entity ids on the Ark Managed Agents side.
const (
	// KMAgentID is the Ark-side agent id (like agent-2026...). One of the two most important ma keys.
	KMAgentID = "ma_agent_id"
	// KMSessionID is the Ark session id (usually the same value as ama_run_id, but it belongs to
	// the Ark side). One of the two most important ma keys.
	KMSessionID = "ma_session_id"
	// KMEnvironmentID is the Ark self_hosted environment id (like env-xxx).
	KMEnvironmentID = "ma_environment_id"
	// KMSkillID is the Ark skill id (like skill-xxx).
	KMSkillID = "ma_skill_id"
	// KMThreadID is the Ark thread id (sth-xxx, the session_thread_id in events).
	KMThreadID = "ma_thread_id"
)

// Execution identity / runtime.
const (
	// KWorkerID is the worker instance id (the process-level identity that polls the Ark work queue).
	KWorkerID = "worker_id"
	// KPod is the instance (pod / container) name.
	KPod = "pod"
	// KComponent is an internal module name, used to tell callers apart in shared library logs.
	KComponent = "component"
)

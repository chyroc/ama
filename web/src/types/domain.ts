/**
 * Domain model of the AMA platform.
 *
 * Naming convention: fields mirroring Ark use snake_case (same as the Ark API, easy to pass through
 * and compare with docs),
 * and AMA's own concepts use camelCase. snake_case tells you the authoritative source of the data is
 * Ark.
 */

import type { TokenUsage } from "./events";
import { zhCN, type MessageKey } from "@/prefs/messages";
import type { TFunc } from "@/prefs/PrefsContext";

/**
 * Fallback for the validators below when the caller passes no `t`: reads the Chinese table.
 *
 * UI callers pass `t` so the message follows the user's language. The mock backend in data/api.ts
 * calls them without `t` on purpose: its errors stand in for the real backend's, which are Chinese.
 */
const zhT: TFunc = (key, vars) =>
  zhCN[key].replace(/\{(\w+)\}/g, (whole, name: string) =>
    vars && name in vars ? String(vars[name]) : whole,
  );

export type { TokenUsage } from "./events";

// ---------------------------------------------------------------------------
// Workspace (AMA's own)
// ---------------------------------------------------------------------------

export type WorkspaceRole = "owner" | "admin" | "member" | "viewer";

export const WORKSPACE_ROLE_LABELS: Record<WorkspaceRole, string> = {
  owner: "拥有者",
  admin: "管理员",
  member: "成员",
  viewer: "只读",
};

/**
 * "What is my role in this workspace", returned by `GET /api/workspaces/{id}/my-role`.
 *
 * # Why a dedicated endpoint, rather than computing it from workspace.members
 *
 * This endpoint uses **the same function** the server uses for permission checks (core.RoleOf), so
 * there is no second truth:
 * if the frontend compares strings itself, it drifts from the server's rules sooner or later, and
 * that inconsistency gives no error.
 */
export interface WorkspaceRoleInfo {
  /** My role. In theory, if you got in it always has a value, but an empty string is possible (see the server comment). */
  role: WorkspaceRole | "";
  /**
   * Whether admin-type actions are allowed (true for both owner and admin).
   *
   * ⚠️ **Computed by the server; the frontend must not compare strings itself**: "owner and admin
   * have exactly the same permissions"
   * is the easiest thing to miss by checking only owner, and that inconsistency gives no error.
   */
  canManage: boolean;
}

export interface WorkspaceMember {
  id: string;
  name: string;
  email: string;
  role: WorkspaceRole;
  /**
   * Avatar URL; **only a cache, not the source of truth**, and **the field may be missing
   * entirely**.
   *
   * It is carried over from the people picker's candidate record at invite time, and never
   * refreshed after, so it goes stale over time;
   * that cost is accepted on purpose: upstream has only one entry, `?search=<keyword>`, with no
   * batch lookup by email,
   * so "look it up on demand when rendering the list" would mean one serial request per member
   * (N+1).
   *
   * Broken is fine, and **do not write conditional rendering like `avatarUrl && ...`**:
   * `<Avatar.Image src>` + `<Avatar.Fallback>` initials are enough; Radix only renders `<img>`
   * after the image has loaded,
   * so the three cases "no such field", "still loading" and "URL broken" all fall back to
   * initials; hand-written conditions would instead leave an empty circle when loading fails. Same as
   * AuthUser.avatarUrl.
   *
   * Existing members (the few in the seed) have no such field, and take the initials path.
   */
  avatarUrl?: string;
  joinedAt: string;
}

/**
 * "What this email's person is called and which avatar they have", for rendering avatars in the UI.
 *
 * # How it differs from those two fields in WorkspaceMember
 *
 * `WorkspaceMember` carries its own name/avatarUrl, which is **staleness accepted on purpose**: the
 * upstream directory has no
 * batch lookup by email, so a copy can only be taken at invite time (see the comment there).
 *
 * This one is different: it comes from the backend's own users table, **looked up live every
 * time**, so when someone changes their name or avatar
 * it is right at once. Do not copy it into any entity to store it; that would duplicate the
 * staleness problem.
 *
 * # name may be an empty string
 *
 * The users table allows rows with "only an email, empty name and avatar"; people invited but never
 * logged in are
 * in this state. So always render `name || email`; do not assume name has a value.
 */
export interface UserProfile {
  /** Employee email, which is the Actor ID, already lowercased. **It is the key of this record**. */
  email: string;
  /** Display name; **may be an empty string** (that person has not logged in yet). */
  name: string;
  /**
   * Avatar URL; **only a cache, not the source of truth**, and the field may be missing entirely.
   *
   * Broken is fine, and **do not write conditional rendering like `avatarUrl && ...`**: the reason
   * is
   * word for word the same as WorkspaceMember.avatarUrl (Radix only renders `<img>` after the
   * image has loaded;
   * hand-written conditions would leave an empty circle when loading fails). Always use the
   * UserAvatar component.
   */
  avatarUrl?: string;
}

export interface Workspace {
  id: string;
  name: string;
  /** URL identifier, globally unique, [a-z0-9-]{2,32}. */
  key: string;
  description?: string;
  createdAt: string;
  /**
   * The time of the last change (rename / change key / change description / add, remove or change
   * members).
   *
   * Differing from createdAt means it was edited. Same format as other time fields: ISO 8601, UTC,
   * **fixed three decimals** (`2026-07-26T10:00:00.000Z`).
   */
  updatedAt: string;
  members: WorkspaceMember[];
}

/** Key validation rules live here, shared by creation and settings. */
export const WORKSPACE_KEY_PATTERN = /^[a-z0-9][a-z0-9-]{1,31}$/;

export function validateWorkspaceKey(key: string, t: TFunc = zhT): string | null {
  if (!key) return t("domain.wsKeyRequired");
  if (key.length < 2) return t("domain.wsKeyTooShort");
  if (key.length > 32) return t("domain.wsKeyTooLong");
  if (!WORKSPACE_KEY_PATTERN.test(key)) {
    return t("domain.wsKeyInvalid");
  }
  return null;
}

// ---------------------------------------------------------------------------
// Ark credentials (AMA's own, per workspace)
// ---------------------------------------------------------------------------

/**
 * Ark regions. Taken from the SDK's default config (ark-runtime-go/arkruntime/config.go):
 * Volcengine (cn-beijing) and BytePlus (ap-southeast-1), each with its own base URL and region.
 */
export type ArkRegion = "cn-beijing" | "ap-southeast-1";

export const ARK_REGIONS: { id: ArkRegion; label: string; baseUrl: string }[] = [
  {
    id: "cn-beijing",
    label: "火山引擎 · 华北（北京）",
    baseUrl: "https://ark.cn-beijing.volces.com/api/v3",
  },
  {
    id: "ap-southeast-1",
    label: "BytePlus · 亚太（新加坡）",
    baseUrl: "https://ark.ap-southeast.bytepluses.com/api/v3",
  },
];

/** Translation keys for ARK_REGIONS labels: render with `t(ARK_REGION_KEYS[id])`. */
export const ARK_REGION_KEYS: Record<ArkRegion, MessageKey> = {
  "cn-beijing": "enum.arkRegion.cn-beijing",
  "ap-southeast-1": "enum.arkRegion.ap-southeast-1",
};

export function arkRegionLabel(region: ArkRegion, t?: TFunc): string {
  if (t && region in ARK_REGION_KEYS) return t(ARK_REGION_KEYS[region]);
  return ARK_REGIONS.find((r) => r.id === region)?.label ?? region;
}

/** The API Key management page of the Ark console, where users copy keys from. */
export const ARK_API_KEY_CONSOLE_URL =
  "https://console.volcengine.com/ark/region:ark+cn-beijing/apiKey";

/** The quick start page of Ark Managed Agents; the intro sentence on the landing page links here. */
export const ARK_MANAGED_AGENTS_URL =
  "https://console.volcengine.com/ark/region:cn-beijing/managed-agents/quickstart";

/**
 * Verification state. Ark has no dedicated "test a key" endpoint; in practice we call a
 * read-only endpoint (list Agents) with this key and judge by the status code: 401/403 → invalid,
 * other success → valid.
 */
export type ArkCredentialCheck = "unchecked" | "valid" | "invalid";

export const ARK_CHECK_LABELS: Record<ArkCredentialCheck, string> = {
  unchecked: "未验证",
  valid: "可用",
  invalid: "不可用",
};

/** Translation keys for ARK_CHECK_LABELS: render with `t(ARK_CHECK_KEYS[check])`. */
export const ARK_CHECK_KEYS: Record<ArkCredentialCheck, MessageKey> = {
  unchecked: "enum.arkCheck.unchecked",
  valid: "enum.arkCheck.valid",
  invalid: "enum.arkCheck.invalid",
};

/**
 * Workspace-level Ark credentials.
 *
 * One per workspace: the workspace is the boundary for billing and isolation, and agents / sessions
 * in the same workspace share one key.
 * The plain text only appears on submit; once stored the frontend only gets the mask, the same
 * handling as Feishu app credentials.
 */
/**
 * **Where** this workspace's Ark credentials come from.
 *
 *   override  this workspace's own custom key (it pasted one)
 *   inherit   references the account-level global credential, falling back on read
 *
 * ⚠️ With inherit the workspace side **stores no copy**, so changing the global one takes effect at
 * once for every workspace referencing it;
 * that is exactly the linkage "configure once globally" wants. Do not do any "copy the value down
 * and cache it" in the frontend.
 */
export type ArkCredentialSource = "override" | "inherit";

/**
 * A **transient** note that comes with a successful credential save.
 *
 * There is only one kind: the online verification during save did not complete (Ark timeout/5xx);
 * the key was saved as usual,
 * but the user should be told "not verified; you can click verify connectivity later". It only
 * appears in the PUT response,
 * is not stored, and disappears after refresh (GET); it describes "this one save", not a property of
 * the credential.
 */
export interface ArkSaveWarning {
  kind: "verify_unavailable";
  message: string;
}

/** Stable fine-grained code in the error envelope when a save is blocked (HTTP is still 422); the frontend branches on it, not on Chinese sentences. */
export const ARK_REJECT_INVALID_KEY = "ark_key_invalid";
export const ARK_REJECT_NOT_ACTIVATED = "ark_managed_agent_not_open";

export interface ArkCredential {
  workspaceId: string;
  /**
   * The masked key, like `abcd****...****wxyz`. Empty string when not configured.
   *
   * ⚠️ When `source` is inherit this is **the mask of the global key**, not an empty string.
   * So the check for "not configured" is `!apiKeyMasked`, which holds for both sources.
   */
  apiKeyMasked: string;
  region: ArkRegion;
  check: ArkCredentialCheck;
  /**
   * Credential source. **The backend always sends this key** (no omitempty).
   *
   * Old data does not have it, and the backend normalizes it to "override", i.e. the status quo.
   */
  source: ArkCredentialSource;
  /** Email of the referenced owner, only present when source is inherit. The UI must say clearly "whose key this is". */
  inheritFrom?: string;
  /** Time of the last verification; absent if never verified. */
  checkedAt?: string;
  /** Why verification failed, shown to the user. */
  checkError?: string;
  updatedAt?: string;
  /** Email of the operator of the last change; credential config must leave a trail. */
  updatedBy?: string;
  /** Transient note that only appears in save responses; see ArkSaveWarning. GET does not carry it. */
  warning?: ArkSaveWarning;
}

/**
 * The external view of account-level Ark credentials.
 *
 * Split into two types rather than reusing ArkCredential: that one has workspaceId and source,
 * and in the account-level context neither field makes sense (the global one belongs to no
 * workspace, and references nobody).
 */
export interface GlobalArkCredential {
  /** Empty string when not configured; the frontend uses it to judge "not configured yet". */
  apiKeyMasked: string;
  region: ArkRegion;
  check: ArkCredentialCheck;
  checkedAt?: string;
  checkError?: string;
  updatedAt?: string;
  /** Transient note that only appears in save responses; see ArkSaveWarning. GET does not carry it. */
  warning?: ArkSaveWarning;
}

export interface ArkCredentialDraft {
  apiKey: string;
  region: ArkRegion;
  /**
   * Non-empty means "take that key from the Volcengine account", and then apiKey should be an
   * empty string.
   *
   * ⚠️ **The plain text never passes through the frontend**: only two ids are sent here; the server
   * fetches the plain text itself, computes the mask and stores it encrypted.
   * Letting the frontend get the plain text and PUT it back would be simpler, but that would put a
   * long-lived Ark key into devtools'
   * network panel, where browser extensions might read it.
   */
  fetchRef?: ArkKeyRef;
  /**
   * Save anyway after seeing the "account has not activated Managed Agents" warning.
   *
   * After the first save is blocked by 422 ark_managed_agent_not_open, it is carried when the user
   * clicks "save anyway"
   * to resend. **It has no effect on a bad key (ark_key_invalid)**; there is no force-save path for
   * that.
   */
  confirmNotActivated?: boolean;
}

/** Points to one Ark API Key in the Volcengine account; both ids come from listVolcArkKeys. */
export interface ArkKeyRef {
  accountId: string;
  keyId: string;
}

// ---------------------------------------------------------------------------
// Three levels of Ark credentials: agent → workspace → account
// ---------------------------------------------------------------------------
//
// **The more specific, the higher the priority.** Storage and meaning of each level:
//
//   agent level      has a record = custom, no record = follow the workspace (there is no stored
//                    "inherit" state)
//   workspace level  source=override stores its own; source=inherit references the account-level
//                    one
//   account level    one per person, referenced by workspaces
//
// ⚠️ **Lower levels store no copy of upper levels**: that is the whole implementation of "changing
// the upper level takes effect at once".

/** One level of the three-level config. */
export type ArkLevel = "agent" | "workspace" | "global";

/**
 * Summary of "the Ark credential actually in effect now", for display only.
 *
 * The most confusing thing about three config levels is "I configured three places, which one is
 * used";
 * this structure exists to state that answer directly, rather than making users reason by jumping
 * between three pages.
 */
export interface ArkEffective {
  /** The level in effect. **Empty string means none of the three is configured** (then apiKeyMasked is also empty). */
  level: ArkLevel | "";
  apiKeyMasked: string;
  region: ArkRegion;
  check: ArkCredentialCheck;
  /** Only present when level is global: whose account-level credential that key is. */
  ownerEmail?: string;
}

/** The external view of agent-level Ark credentials; the plain text is not here. */
export interface AgentArkCredential {
  agentId: string;
  /** Empty string when not configured. */
  apiKeyMasked: string;
  region: ArkRegion;
  check: ArkCredentialCheck;
  /**
   * Whether this agent has its own key. false = follow workspace settings.
   *
   * **Not an optional field**: false is a meaningful value; if the key were absent the check would
   * get undefined,
   * neither branch would match, and the UI would show nothing. Same reason as ArkCredential.source.
   */
  custom: boolean;
  checkedAt?: string;
  checkError?: string;
  updatedAt?: string;
  updatedBy?: string;
  /** Summary of the one actually in effect; agents that "follow the workspace" need it more. */
  effective: ArkEffective;
  /** Transient note that only appears in save responses; see ArkSaveWarning. GET does not carry it. */
  warning?: ArkSaveWarning;
}

// ---------------------------------------------------------------------------
// Auto-fetching Ark API Keys from the Volcengine account (lists to choose from, none containing plain text)
// ---------------------------------------------------------------------------

/** Availability of the auto-fetch feature. The three states must be kept apart, because what the user should do differs completely. */
export interface ArkFetchStatus {
  /** Whether the server has it wired up. When false the entry should not be shown. */
  available: boolean;
  /** Whether the current user has authorized it. */
  authorized: boolean;
  /** Only present when available and not authorized. ⚠️ Valid for 600 seconds; **do not cache it**. */
  authorizeUrl?: string;
  /** Reason when unavailable, shown directly to the user. */
  reason?: string;
}

/** One Volcengine account. A person often has several (measured: 10), so they must be able to choose. */
export interface VolcAccount {
  id: string;
  name: string;
  type?: string;
}

/**
 * Metadata of one selectable Ark API Key.
 *
 * ⚠️ **There is no plain text here, and never add it**: adding it would let plain text pass through
 * the browser.
 */
export interface ArkKeyOption {
  id: string;
  name: string;
  /** The mask from Volcengine (often just a row of asterisks); with it users dare to confirm "this is the one I picked". */
  masked?: string;
  /** Like Active. **Non-Active ones must be visible to the user**: picking one would not work. */
  status?: string;
  projectName?: string;
  createTime?: string;
  lastAccessTime?: string;
  /**
   * Creator's user name, taken from Volcengine's `sys:ark:createdBy` tag.
   *
   * ⚠️ **Both shapes have been seen in real data**: mostly bare user names (`bob`),
   * and also full emails (`heidi@example.com`). So judging "is it me"
   * must compare both; see isMyArkKey. Absent when unavailable (old keys may not have this tag).
   */
  createdBy?: string;
  /**
   * Creator's IAM numeric id.
   *
   * More reliable than createdBy (no shape problem), but **we do not have "my IAM id"**:
   * AMA's identity is an email, so it cannot be used to judge "is it me"; it is only a display
   * fallback.
   */
  createdByUserId?: string;
}

/**
 * Whether this key was created by the current user.
 *
 * # Why comparing only one shape is not enough
 *
 * Volcengine's `createdBy` has two shapes in practice (dumped 10 real ones):
 *
 *   bob                  bare user name, most are like this
 *   heidi@example.com    full email
 *
 * And AMA's identity is an email. So both sides are normalized before comparing: a **full email**
 * compares the whole string,
 * a **bare user name** compares the part of the email before @. Comparing only one would leave people
 * with the other shape never recognizing their own keys.
 *
 * # When it cannot be decided, treat it as "not me"
 *
 * When createdBy is unavailable (old keys without the tag) it returns false, and the UI just does
 * not mark "me".
 * **The reverse (defaulting to mine) is unacceptable**: it would mark others' keys as mine,
 * and users would pick a key based on that, thinking they have permission. Better to mark too few
 * than to mark wrongly.
 *
 * ⚠️ This deliberately **does not fall back to createdByUserId**: we only have the email, not the IAM
 * numeric id,
 * and comparing it with any local value would be guessing.
 */
export function isMyArkKey(key: ArkKeyOption, myEmail: string): boolean {
  const who = (key.createdBy ?? "").trim().toLowerCase();
  const me = myEmail.trim().toLowerCase();
  if (!who || !me) return false;
  if (who === me) return true;
  // Bare user name shape: compare the part of the email before @.
  const myUsername = me.split("@")[0];
  return Boolean(myUsername) && who === myUsername;
}

/**
 * Ark API Key prefixes are not fixed (console-generated ones look like `ARKxxxx`, and plain UUIDs
 * have been seen too),
 * so only a minimum check of length and charset is done, with no guessing of prefixes; guessing wrong
 * would block valid keys.
 */
export function validateArkApiKey(key: string, t: TFunc = zhT): string | null {
  const k = key.trim();
  if (!k) return t("domain.arkKeyRequired");
  if (k.length < 16) return t("domain.arkKeyTooShort");
  if (/\s/.test(k)) return t("domain.arkKeyHasSpace");
  return null;
}

// ---------------------------------------------------------------------------
// Platform API Key (credentials for calling this platform's API)
// ---------------------------------------------------------------------------
//
// ⚠️ **This is a completely different thing from ArkCredential above; do not mix them up:**
//
//   ArkCredential    the credential **we** use to call **Ark** (workspace-level, one per
//                    workspace, paid)
//   ApiKey (below)   the credential **others** use to call **us** (account-level, several per
//                    person)
//
// Opposite directions. In the UI both are on the workspace settings page, so the text must make it
// clear.

/** Fixed prefix of the ApiKey plain text, matching the backend's auth.KeyPrefix. */
export const API_KEY_PREFIX = "amak_";

/**
 * The **external view** of one platform API Key. No plain text: the backend only stores sha256.
 *
 * `lastUsedAt` **missing means never used** (not an empty string): users use it to judge which ones
 * can be revoked.
 */
export interface ApiKey {
  /** Record id, used for revoking. **Not the key itself.** */
  id: string;
  /** The name given at creation, to tell them apart. */
  name: string;
  /** The first few characters of the plain text (like `amak_a1b2c3`), only to recognize which one it is in the list. */
  prefix: string;
  createdAt: string;
  /** Time of last use. **Missing = never used.** */
  lastUsedAt?: string;
}

/**
 * A just-created key, **the only shape that carries plain text**.
 *
 * ⚠️ `key` only appears in the response of that one creation, and can never be fetched again. The UI
 * must let the user copy it right away,
 * and must say so; otherwise they find out they did not save it only after closing the dialog, and
 * can only revoke and recreate.
 */
export interface CreatedApiKey extends ApiKey {
  /** Plain text key, like `amak_...`. **Just this once.** */
  key: string;
}

/**
 * Validate a key's name. Returns error text or null, the same shape as the other validate* in this
 * file.
 *
 * The text must match the error of the backend's auth.CreateAPIKey: `LocalApiClient` is the contract
 * baseline,
 * and a mismatch would make e2e fail on assertions after switching to HTTP.
 */
export function validateApiKeyName(name: string, t: TFunc = zhT): string | null {
  const n = name.trim();
  if (!n) return t("domain.apiKeyNameRequired");
  if (n.length > 128) return t("domain.apiKeyNameTooLong");
  return null;
}

// ---------------------------------------------------------------------------
// Model config (mirrors Ark)
// ---------------------------------------------------------------------------

export type ModelId =
  | "doubao-seed-2-1-pro-260628"
  | "doubao-seed-2-1-pro-260915"
  | "doubao-seed-2-1-turbo-260628"
  | "doubao-seed-2-0-lite-260428"
  | "doubao-seed-evolving"
  | "deepseek-v4-pro-260425"
  | "deepseek-v4-pro-ga-260813"
  | "deepseek-v4-flash-ga-260731"
  | "deepseek-v4-1-flash-261017"
  | "glm-5-2-260617";

export type ReasoningEffort =
  | "none"
  | "minimal"
  | "low"
  | "medium"
  | "high"
  | "xhigh"
  | "max";

export type ServiceTier = "auto" | "default" | "fast";

export interface ModelConfig {
  /** The Ark field name is id, not model. */
  id: ModelId;
  thinking?: "enabled" | "disabled" | "auto";
  reasoning_effort?: ReasoningEffort;
  service_tier?: ServiceTier;
}

/**
 * The parameter levels each model supports.
 *
 * **The source of truth for this table is the Ark platform itself, not the docs.** It is generated
 * by running `server/cmd/probe-models` against the real
 * platform: for each model and each level, send `POST /api/v3/agents`; those rejected with 400 are
 * unsupported.
 * When an Agent is created successfully, Ark echoes the resolved default in `model`, and
 * `defaultReasoningEffort`
 * is that echoed value.
 *
 * Why not copy the docs: the docs themselves say "the supported model range may change with model
 * releases",
 * and Ark's docs have been proven wrong by testing (the thinking page says no thinking content is
 * returned; measured 69/72 events had content).
 * The current 8 entries **match the docs mirror exactly in testing**, but matching is a conclusion,
 * not a premise.
 *
 * The model picker follows this to avoid submitting invalid combinations (Ark returns 400
 * InvalidParameter).
 * After changing this table, run `npm run check-models` to reconcile; it compares the values here
 * against the docs mirror.
 * For full background (including why the backend has no allowlist) see "model list" in
 * server/shared/infra/ma/README.md.
 */
export interface ModelSpec {
  id: ModelId;
  label: string;
  vendor: string;
  reasoningEfforts: ReasoningEffort[];
  defaultReasoningEffort: ReasoningEffort;
  serviceTiers: ServiceTier[];
  supportsThinking: boolean;
}

export const MODEL_SPECS: ModelSpec[] = [
  {
    id: "doubao-seed-2-1-pro-260628",
    label: "Doubao Seed 2.1 Pro",
    vendor: "豆包",
    reasoningEfforts: ["minimal", "low", "medium", "high"],
    defaultReasoningEffort: "high",
    serviceTiers: ["auto", "default"],
    supportsThinking: true,
  },
  {
    // Two coexisting dated versions of the same model as 260628 above (listed side by side in the
    // docs table, both usable in probe tests).
    // The levels are exactly the same; the second line in the dropdown shows the id, telling them
    // apart by 260628/260915.
    id: "doubao-seed-2-1-pro-260915",
    label: "Doubao Seed 2.1 Pro",
    vendor: "豆包",
    reasoningEfforts: ["minimal", "low", "medium", "high"],
    defaultReasoningEffort: "high",
    serviceTiers: ["auto", "default"],
    supportsThinking: true,
  },
  {
    id: "doubao-seed-2-1-turbo-260628",
    label: "Doubao Seed 2.1 Turbo",
    vendor: "豆包",
    reasoningEfforts: ["minimal", "low", "medium", "high"],
    defaultReasoningEffort: "high",
    serviceTiers: ["auto", "default", "fast"],
    supportsThinking: true,
  },
  {
    id: "doubao-seed-2-0-lite-260428",
    label: "Doubao Seed 2.0 Lite",
    vendor: "豆包",
    reasoningEfforts: ["minimal", "low", "medium", "high"],
    defaultReasoningEffort: "medium",
    serviceTiers: ["auto", "default", "fast"],
    supportsThinking: true,
  },
  {
    id: "doubao-seed-evolving",
    label: "Doubao Seed Evolving",
    vendor: "豆包",
    reasoningEfforts: ["minimal", "low", "medium", "high"],
    defaultReasoningEffort: "high",
    serviceTiers: ["auto", "default"],
    supportsThinking: true,
  },
  {
    // A **different model** from deepseek-v4-pro-ga-260813 below (that one has -ga-).
    // Measured, the difference is exactly reasoning_effort: this one does not support none, the GA
    // one does.
    id: "deepseek-v4-pro-260425",
    label: "DeepSeek V4 Pro (260425)",
    vendor: "DeepSeek",
    reasoningEfforts: ["minimal", "low", "high", "max"],
    defaultReasoningEffort: "high",
    serviceTiers: ["auto", "default"],
    supportsThinking: true,
  },
  {
    id: "deepseek-v4-pro-ga-260813",
    label: "DeepSeek V4 Pro",
    vendor: "DeepSeek",
    reasoningEfforts: ["none", "minimal", "low", "high", "max"],
    defaultReasoningEffort: "high",
    serviceTiers: ["auto", "default"],
    supportsThinking: true,
  },
  {
    id: "deepseek-v4-flash-ga-260731",
    label: "DeepSeek V4 Flash",
    vendor: "DeepSeek",
    reasoningEfforts: ["none", "minimal", "low", "high", "max"],
    defaultReasoningEffort: "high",
    serviceTiers: ["auto", "default"],
    supportsThinking: true,
  },
  {
    // Measured by the probe: the platform can already create Agents with it and real inference
    // works, but the online docs
    // have not listed this generation yet. The platform catalog also has two older versions of the
    // same family, 260901 / 260910, with exactly the same levels;
    // only the latest 261017 is kept (consistent with listing only one GA version for V4 Flash).
    // The rationale is recorded in KNOWN_DOC_LAG in web/scripts/check-models.mjs.
    id: "deepseek-v4-1-flash-261017",
    label: "DeepSeek V4.1 Flash",
    vendor: "DeepSeek",
    reasoningEfforts: ["none", "low", "high", "max"],
    defaultReasoningEffort: "high",
    serviceTiers: ["auto", "default"],
    supportsThinking: true,
  },
  {
    id: "glm-5-2-260617",
    label: "GLM 5.2",
    vendor: "智谱",
    reasoningEfforts: ["none", "minimal", "high", "max"],
    defaultReasoningEffort: "max",
    serviceTiers: ["auto", "default"],
    supportsThinking: true,
  },
];

export function modelSpec(id: ModelId): ModelSpec {
  return MODEL_SPECS.find((m) => m.id === id) ?? MODEL_SPECS[0];
}

export const REASONING_EFFORT_LABELS: Record<ReasoningEffort, string> = {
  none: "关闭",
  minimal: "极简",
  low: "低",
  medium: "中",
  high: "高",
  xhigh: "很高",
  max: "最高",
};

// ---------------------------------------------------------------------------
// Agent
// ---------------------------------------------------------------------------

export type BuiltinTool =
  | "bash"
  | "read"
  | "write"
  | "edit"
  | "glob"
  | "grep"
  | "web_fetch"
  | "web_search";

export const BUILTIN_TOOLS: { id: BuiltinTool; label: string; description: string }[] = [
  { id: "bash", label: "bash", description: "在沙箱里执行 shell 命令" },
  { id: "read", label: "read", description: "读取文件内容" },
  { id: "write", label: "write", description: "写入文件" },
  { id: "edit", label: "edit", description: "编辑已有文件" },
  { id: "glob", label: "glob", description: "按通配符查找文件" },
  { id: "grep", label: "grep", description: "在文件里搜索内容" },
  { id: "web_fetch", label: "web_fetch", description: "抓取网页" },
  { id: "web_search", label: "web_search", description: "联网搜索" },
];

/**
 * The agent's enabled state. **Only two values, and "disabled" really blocks.**
 *
 * There used to be a third, `draft`, which was removed: nothing read it to decide anything, and
 * what it meant,
 * "not set up yet, do not take live traffic", is in this project a diagnosable report rather than a
 * switch the user flips
 * (see the FeishuReadiness part in DESIGN.md, "installed does not mean usable").
 *
 * ⚠️ **The meaning of disabled must match the backend word for word**: when disabled, no new
 * sessions can start and no messages can be sent in existing ones,
 * but **history can still be viewed, and running turns can still be interrupted**. The frontend's
 * disabled state must follow this; do not casually
 * disable the interrupt button too, or after disabling an agent its running turn could never be
 * stopped.
 */
export type AgentStatus = "active" | "disabled";

export const AGENT_STATUS_LABELS: Record<AgentStatus, string> = {
  active: "已启用",
  disabled: "已停用",
};

export interface Agent {
  id: string;
  workspaceId: string;
  name: string;
  description: string;
  /** System prompt; the Ark field name is system. Independent per agent. */
  system: string;
  model: ModelConfig;
  /** Ark uses it for optimistic locking: updates must send back the current version. */
  version: number;
  builtin_tools: BuiltinTool[];
  /** References workspace skills, not copies: when skill content changes, agents binding it take effect at once. */
  skillIds: string[];
  feishuAppIds: string[];
  status: AgentStatus;
  /**
   * Which authorization sources' credentials this agent may inject (`ama` / `lark-user` / ...).
   *
   * **An empty array means inject none** (new agents default to none selected). The frontend does no
   * fallback:
   * a fallback would silently turn "the user explicitly cancelled all authorizations" back into
   * injection.
   *
   * **Must be non-nil** (the backend guarantees `[]` rather than null): isDirty is a bare
   * JSON.stringify
   * comparison, and null vs [] differing would keep the save button lit forever. The order matters too
   * (the backend sorted it).
   */
  integrationIds: string[];
  /**
   * Tool executor type. Empty string = Docker container (default), "host" = the user's own
   * registered host machine.
   *
   * Paired with hostId: when executorType === "host", hostId must be a machine already bound to this
   * workspace,
   * owned by the creator. A soft reference: when the machine is deleted the agent is not cascade
   * deleted,
   * but tools fail at runtime, so the machine dropdown must clearly flag deleted/unbound machines.
   *
   * ⚠️ **May be missing** (older backends / existing agents do not return it); treated as an empty
   * string (cloud container).
   */
  executorType?: ExecutorType;
  /** Host id (hst_ prefix). Must be an empty string when executorType is not "host". */
  hostId?: string;
  /**
   * Host source (workspace/personal). Existing agents may lack it; interpreted as workspace.
   * Paired with hostId, a soft reference: deleting/unbinding a machine does not delete the agent;
   * session creation and the terminal entry guard it.
   */
  hostSource?: HostSource;
  /**
   * Custom environment variables set by the user; see AgentEnv.
   *
   * Also must be non-nil, and **the order is the order the user entered** (the backend
   * deliberately does not sort; reordering would make the row they are
   * editing jump away).
   */
  envs: AgentEnv[];
  /**
   * Credential aliases; see AgentEnvAlias. Also must be non-nil, with order kept as is.
   */
  envAliases: AgentEnvAlias[];
  /**
   * How to handle the user saying something else while the agent is still running.
   *
   * ⚠️ **An empty string equals redirect, the opposite direction from other enums in this repo.**
   * Other enums put "empty string on the most conservative side", while this one's default is the
   * aggressive side; neither side causes a fault,
   * and the product requires redirect by default. Existing agents have no such key, so it reads as
   * an empty string.
   * Always check through followUpOrDefault(), never compare bare. See FollowUpMode in
   * server/internal/domain.
   */
  followUp: FollowUpMode | "";
  /**
   * Whether to show detailed work content (chain of thought) in Feishu.
   *
   * ⚠️ **An empty string equals full (show)**, the same direction as followUp. Existing agents have
   * no such key,
   * so it reads as an empty string. Always check through progressDetailOrDefault(), never compare
   * bare.
   * See ProgressDetail in server/internal/domain.
   */
  progressDetail: ProgressDetail | "";
  createdAt: string;
  updatedAt: string;
  /**
   * Emails of the person who created this agent and of the latest editor.
   *
   * ⚠️⚠️ **Either may be missing entirely, and that is a long-lasting valid state**,
   * not broken data: these two fields were added later, and **agents created before that can never
   * get a creator filled in**
   * (it was not recorded then; neither the jsonb doc nor the Ark side has it, so there is no way to
   * find out).
   *
   * So the UI must be able to say "**we do not know who created it**": `<UserAvatar
   * email={undefined}>`
   * renders a question-mark avatar and says plainly it was not recorded; **do not show it as blank,
   * and do not treat it as an error**.
   * The backend side also uses omitempty, precisely to keep "key absent" and "empty string" apart.
   *
   * ⚠️ Agents created by identity-less system paths also lack these two fields (the backend does not
   * fill in fake values),
   * and the frontend treats them the same, with no need to tell which kind.
   *
   * Display names and avatars **are not here**; look them up in batch with `useUserProfiles`:
   * storing them in the agent would freeze "the name at creation" forever (agents are stored whole as
   * doc jsonb),
   * so after that person changes name or avatar the list would still show the old ones, with no
   * error. See domain.UserProfile.
   */
  createdBy?: string;
  updatedBy?: string;
}

/**
 * Tool executor type.
 *
 *   ""      Docker container (default): one container per session, works out of the box, pauses
 *           automatically when idle.
 *   "host"  a host machine registered by the user: runs on the user's machine, and the workspace
 *           persists.
 */
export type ExecutorType = "" | "host";

/**
 * Source of the host machine (two-level binding, matching the backend's apimodel.HostSource):
 *
 *   ""          draft/existing default; on save the backend interprets it as "workspace"
 *   "workspace" workspace assignment: the machine is bound in workspace settings, and admins can
 *               assign it to any agent
 *   "personal"  personal direct connect: the agent creator's own machine, not going through
 *               workspace binding;
 *               only the creator can change it, and only the machine owner can open the web
 *               terminal
 */
export type HostSource = "" | "workspace" | "personal";

/**
 * How to handle sending another message.
 *
 *   redirect  interrupt the current turn and do this new one instead. **The default** (an empty
 *             string also equals it)
 *   queue     wait for this turn to finish, then send
 */
export type FollowUpMode = "redirect" | "queue";

/**
 * Whether to show detailed work content (chain of thought) in Feishu.
 *
 *   full   show: the group gets an extra chain-of-thought message, with thinking and every tool
 *          call laid out live. **The default** (an empty string also equals it)
 *   brief  do not show: this turn only has "emoji + final answer"
 */
export type ProgressDetail = "full" | "brief";

/**
 * Normalize progressDetail to a definite level.
 *
 * **It must go through this before use**, for exactly the same reason as followUpOrDefault: in
 * existing agents this field is an empty string,
 * and a bare comparison would make the dropdown select nothing, looking "never configured", while
 * it is really running on the default (show).
 *
 * Same basis as the backend's domain.NormalizeProgressDetail: unknown values also fall to full.
 */
export function progressDetailOrDefault(
  detail: ProgressDetail | "",
): ProgressDetail {
  return detail === "brief" ? "brief" : "full";
}

/**
 * Normalize a possibly-empty followUp into the level actually in effect.
 *
 * **It must go through this before use**: in existing agents this field is an empty string (the
 * backend's zero value),
 * and an empty string is redirect. A bare comparison `a.followUp === "redirect"` would judge
 * existing agents
 * as "neither", so the dropdown shows empty and saving writes the empty string back; zero errors,
 * but the UI is wrong.
 *
 * Same basis as the backend's domain.NormalizeFollowUpMode: unknown values also fall to redirect.
 */
export function followUpOrDefault(mode: FollowUpMode | ""): FollowUpMode {
  return mode === "queue" ? "queue" : "redirect";
}

/**
 * One custom environment variable.
 *
 * An array of objects rather than `Record<string,string>`: the JSON serialization order of a map is
 * not stable,
 * and isDirty is a bare JSON.stringify comparison, which would judge "nothing changed" as changed.
 */
export interface AgentEnv {
  key: string;
  /**
   * The value.
   *
   * ⚠️ **Sensitive items are plain text here too**: to inject them into the container they must be
   * recoverable. The frontend **masks them when echoing**
   * (see maskEnvValue), but sends plain text on save; to change a sensitive value the user must
   * re-enter the whole item,
   * because "the mask string sent back" and "the user really wants the value to be these dots"
   * cannot be told apart.
   */
  value: string;
  /**
   * Sensitive flag.
   *
   * ⚠️⚠️ **It is not a security guarantee, and the UI text must say so honestly.** Sensitive items
   * are injected into the container
   * just like normal ones, and the agent has bash: a single `env` reads the plain text.
   *
   * Its real effect is only two things, both **reducing unintended spread**:
   *
   *  1. Not in the system prompt → not in the model context → the model will not repeat it in
   *     replies, and it does not land in RunEvent;
   *  2. Not shown in plain text on the page → not visible in screenshots, screen sharing, or to
   *     people walking by.
   *
   * Calling it "secure" would make users put in credentials that really should not be given to the
   * agent, which is worse than not having this flag.
   */
  secret: boolean;
}

/** The valid shape of an environment variable name, matching the backend's domain.IsValidEnvKey. */
const ENV_KEY_RE = /^[A-Za-z_][A-Za-z0-9_]*$/;

/**
 * Whether a string can be used as an environment variable name.
 *
 * **The same rule** as the backend (POSIX: first character a letter or underscore, the rest
 * letters, digits and underscores).
 * The frontend check is for experience (an immediate hint); the backend check is the contract; both
 * must exist.
 */
export function isValidEnvKey(key: string): boolean {
  return ENV_KEY_RE.test(key);
}

/** Limits for custom env, matching those constants in the backend's domain. */
export const ENV_LIMITS = {
  maxCount: 64,
  maxValueBytes: 32 * 1024,
} as const;

/**
 * Mask a sensitive value for echoing.
 *
 * Like `••••••4f2a`: keep the last four characters so the user can check "is this the one I
 * entered",
 * with a fixed-length mask in front (**not leaking the original length**). Values too short are
 * masked entirely.
 */
export function maskEnvValue(value: string): string {
  const dots = "••••••";
  if (value.length <= 8) return dots;
  return dots + value.slice(-4);
}

/**
 * Environment variable names reserved by the platform, which users cannot define.
 *
 * ⚠️ **This is not purism; it is a privilege escalation hole**: entering `AMA_TOKEN=<a stolen key>`
 * would let the container
 * call the platform as any identity. The backend blocks it on the write path and at injection; this
 * frontend copy is for **an immediate hint**
 * rather than an error after saving. **When adding a new platform-injected variable, sync it here**
 * (missing it gives no error,
 * just that saving is rejected after the user enters it, without the frontend saying so first).
 */
export const RESERVED_ENV_KEYS = [
  // ⚠️ **Matches the backend's shelltools.platformReservedEnvs word for word**.
  "AMA_TOKEN",
  "AMA_BASE_URL",
  "AMA_WORKSPACE_ID",
  "AMA_SESSION_ID",
  // The Feishu user authorization group (the two names lark-cli reads).
  //
  // ⚠️ These two **were once missing here** (in the backend's platformReservedEnvs, not in the
  // frontend):
  // the symptom was a user entering LARKSUITE_CLI_USER_ACCESS_TOKEN, no hint from the frontend, and
  // rejection by the backend only on save,
  // after they had filled in the whole form. This frontend copy only handles "immediate hints";
  // missing one causes no vulnerability,
  // but makes people retry again and again on an input that looks valid.
  "LARKSUITE_CLI_USER_ACCESS_TOKEN",
  "LARKSUITE_CLI_APP_ID",
] as const;

/** Whether a variable name is reserved by the platform. */
export function isReservedEnvKey(key: string): boolean {
  return (RESERVED_ENV_KEYS as readonly string[]).includes(key);
}

/**
 * One credential alias: inject a **platform credential** once more under a variable name the user
 * chooses.
 *
 * It is for third-party CLIs that do not read our default variable names (e.g. a tool that only
 * reads `MY_TOKEN`,
 * while the platform injects `AMA_TOKEN`).
 *
 * # ⚠️ Why there is **no value field** here
 *
 * This is the core of the whole design, not an omission: the user only declares "which platform
 * credential to reference", and **the value is filled in by the server
 * at injection time**. So the raw credential never goes into a request body, the database, or the
 * frontend.
 *
 * Letting users fill in the value would open a privilege escalation hole: it would trade "can edit
 * the agent" for "let the container
 * call the platform as any identity". See the comment on AgentEnvAlias in server/model/apimodel.
 *
 * When users need to fill in **their own** value (e.g. a third-party password the platform does not
 * have), there is another path:
 * ordinary custom env (AgentEnv), checking secret if needed.
 */
export interface AgentEnvAlias {
  /** Target variable name; same shape as custom env, and it cannot be a platform-reserved name. */
  key: string;
  /** Source: which platform credential to reference. Values come from ALIASABLE_ENV_SOURCES. */
  from: string;
}

/**
 * Platform credentials that can be referenced by an alias.
 *
 * ⚠️ **Must match the backend's shelltools.AliasableEnvKeys()**. The rule is "the value itself is a
 * usable credential"; so `AMA_WORKSPACE_ID` is not in it (a public identifier),
 * and neither is `LARKSUITE_CLI_APP_ID` (an app id alone can do nothing).
 *
 * labelKey is the translation key of the plain-words dropdown label, but **the variable name must be
 * shown too**: that is what can be matched up when debugging.
 */
export const ALIASABLE_ENV_SOURCES = [
  { key: "AMA_TOKEN", labelKey: "domain.envSourceAmaToken" },
  { key: "LARKSUITE_CLI_USER_ACCESS_TOKEN", labelKey: "domain.envSourceLarkUserToken" },
] as const satisfies readonly { key: string; labelKey: MessageKey }[];

/** Whether a platform variable can be referenced by an alias. */
export function isAliasableEnvSource(key: string): boolean {
  return ALIASABLE_ENV_SOURCES.some((s) => s.key === key);
}

/**
 * The state of one "sync the Agent to Ark".
 *
 * ⚠️ **`none` only exists in the frontend**: when the server has no sync record it returns
 * `{status:"none"}`
 * (not 404; see getAgentSyncTask in server/core/handler.go). An existing Agent that was never
 * saved, or whose record was cleaned up after the retention period, is a normal state and should not
 * render as an error.
 */
export type AgentSyncStatus = "none" | "pending" | "running" | "succeeded" | "failed";

/**
 * Which step the sync got stuck on. The frontend gives **different guidance** by it: credential
 * problems send the user to workspace settings to change config,
 * Ark service failures ask them to retry. Inferring "which step" from a Chinese sentence could only
 * be done by string matching,
 * which is sure to drift, so the server gives this field separately.
 */
export type AgentSyncStage = "" | "credential" | "environment" | "agent" | "skills" | "spec";

/**
 * Result of the async sync after saving an Agent.
 *
 * Saving (POST / PATCH) immediately schedules a task to catch up Ark's system / model / tools /
 * skills,
 * and the frontend polls for the result: **success is silent, only failure is shown**.
 */
export interface AgentSyncTask {
  id?: string;
  workspaceId?: string;
  agentId?: string;
  status: AgentSyncStatus;
  /** Failure reason, a Chinese sentence that can be shown directly. */
  failureReason?: string;
  failureStage?: AgentSyncStage;
  attempts?: number;
  createdAt?: string;
  updatedAt?: string;
}

/**
 * Whether the sync has ended (succeeded or failed).
 *
 * ⚠️ **There must be only this one check**: if each place wrote `s === "succeeded" || s === "failed"`
 * itself,
 * adding a terminal state later would miss some place, showing up as the frontend polling that state
 * forever.
 * The server side is domain.AgentSyncStatus.Terminal(); the two mean the same.
 */
export function isAgentSyncTerminal(status: AgentSyncStatus): boolean {
  return status === "succeeded" || status === "failed" || status === "none";
}

export interface AgentDraft {
  name: string;
  description: string;
  system: string;
  model: ModelConfig;
  builtin_tools: BuiltinTool[];
  skillIds: string[];
  feishuAppIds: string[];
  status: AgentStatus;
  /**
   * See the field of the same name on Agent.
   *
   * ⚠️ **toDraft must carry these two**, otherwise isDirty (a bare JSON.stringify comparison) is
   * never
   * equal, and the save button stays lit forever.
   */
  integrationIds: string[];
  /**
   * Tool executor choice; see the field of the same name on Agent.
   *
   * ⚠️ In the draft these are **non-optional** fields: executorType empty string means cloud
   * container (the default),
   * hostId empty string means no machine selected. If optional, undefined and "" differ under
   * JSON.stringify,
   * and isDirty would stay lit forever (the same lesson as the fields above).
   */
  executorType: ExecutorType;
  hostId: string;
  /**
   * Host source. Non-optional in the draft (empty string and undefined differ under isDirty's
   * JSON.stringify);
   * an empty string is normalized to workspace by the backend on save.
   */
  hostSource: HostSource;
  envs: AgentEnv[];
  /** See the field of the same name on Agent. ⚠️ toAgentDraft must carry it too, same reason as above. */
  envAliases: AgentEnvAlias[];
  /**
   * Redirect or queue when sending another message; see the field of the same name on Agent.
   *
   * An empty string is the default (redirect), so it is fine if older frontends do not send this
   * key:
   * "never decided" and "explicitly chose redirect" are the same behavior anyway.
   */
  followUp: FollowUpMode;
  /**
   * Whether to show detailed work content in Feishu; see the field of the same name on Agent.
   *
   * An empty string is the default (show), so it is fine if older frontends do not send this key;
   * **this is exactly why it is a string enum rather than a boolean**: a boolean defaults to false,
   * which falls on the "do not show" side, so every save by an old frontend would silently turn off
   * the user's chain of thought.
   */
  progressDetail: ProgressDetail;
}

/**
 * Take the editable part out of a saved Agent.
 *
 * **Both callers must go through it; do not build the draft by hand separately.** The server's
 * PATCH takes the complete
 * AgentDraft (updateAgent in core/handler.go), and one missing field clears that field:
 * missing name gives a 400 (blocked by validateAgentDraft); missing integrationIds **silently**
 * changes
 * credential injection (the backend assigns it unconditionally, and an empty array means "cancel
 * all").
 * Neither symptom points at "I only wanted to change another field".
 *
 * ⚠️ **When AgentDraft gets a field, it must be added here too.** AgentDetailPage's isDirty is a
 * bare
 * JSON.stringify comparison; a missing field makes "the agent read back" and "the draft" never equal,
 * so the save button stays lit and does not go away after clicking.
 */
export function toAgentDraft(a: Agent): AgentDraft {
  return {
    name: a.name,
    description: a.description,
    system: a.system,
    model: a.model,
    builtin_tools: a.builtin_tools,
    skillIds: a.skillIds,
    feishuAppIds: a.feishuAppIds,
    status: a.status,
    // ⚠️ Both must be copied, and **copied into new arrays/objects**: referencing the same copy
    // would make in-place edits in the form
    // also change "the saved one", so isDirty is always false; the user changed something
    // but finds the save button not lit.
    //
    // The backend guarantees they are non-nil, but existing data in the mock may lack these two
    // keys (see the load migration
    // in api.ts), so `?? []` covers it; without it reading old data throws TypeError.
    integrationIds: [...(a.integrationIds ?? [])],
    // ⚠️ Must be carried (`?? ""`): existing agents lack these two keys, which read as undefined,
    // while in the draft type they are non-optional strings; missing them would make isDirty under
    // JSON.stringify
    // judge them unequal, keeping the save button lit forever (the same lesson as followUp).
    executorType: a.executorType ?? "",
    hostId: a.hostId ?? "",
    hostSource: a.hostSource ?? "",
    envs: (a.envs ?? []).map((e) => ({ ...e })),
    envAliases: (a.envAliases ?? []).map((x) => ({ ...x })),
    // **Must go through followUpOrDefault**: in existing agents this field is an empty string, and
    // leaving the empty string in the draft
    // makes the dropdown select nothing (looking never configured), and saving writes the empty
    // string back. Normalizing to the level actually in effect
    // makes the UI show the real behavior.
    followUp: followUpOrDefault(a.followUp),
    // Likewise must be normalized. **Missing this line does not just "show wrong", it keeps the save
    // button lit forever**:
    // isDirty is a bare JSON.stringify comparison, and one missing key in the draft never equals the
    // agent read back.
    progressDetail: progressDetailOrDefault(a.progressDetail),
  };
}

// ---------------------------------------------------------------------------
// Skill (workspace-level asset)
// ---------------------------------------------------------------------------

/**
 * Skill source.
 *
 * - `custom`: created by the user (**uploaded ones are this too**: uploading is a way of creating,
 *   not a source)
 * - `builtin`: **platform built-in**. Visible to all workspaces, can be checked when creating an
 *   Agent,
 *   but the body is maintained by the platform and cannot be changed (the backend returns 403 for
 *   PATCH / DELETE); to change it, fork it,
 *   and the copy is `custom`. The source of truth is server/core/skillbuiltin.go, not the database.
 *
 * ⚠️ **Matches the backend's `apimodel.SkillSource` word for word**; adding a value needs both sides
 * changed.
 */
export type SkillSource = "custom" | "builtin";

/**
 * Whether a skill is platform built-in.
 *
 * **Decided by source, not by id prefix**: source is a contract field, while the id's shape is a
 * backend
 * implementation detail (today it is the skill-builtin- prefix, tomorrow it may not be).
 */
export function isBuiltinSkill(skill: { source: SkillSource }): boolean {
  return skill.source === "builtin";
}

/**
 * The maximum number of skills one Agent can bind, **a hard limit on the Ark side**.
 *
 * The source of truth is `maxArkSkillsPerAgent` in `server/core/skillark.go` (its comment quotes the
 * Ark docs:
 * "a single Agent can configure at most 20 Skills"). Both places are constants with no API passing
 * it down,
 * so this is a **copy that can drift**, but the cost of drift is bounded: this frontend check is
 * only for experience,
 * and the real gate is on the backend, which returns a 422 Chinese error when exceeded.
 *
 * # Why the frontend checks too
 *
 * The backend check **does not fire when the Agent is created**, but **when a conversation starts**
 * (`arkSkillsFor`).
 * So relying only on the backend, users can smoothly create an agent bound to 21 skills, and then in
 * a completely unrelated
 * place (the first time talking to it) hit "this Agent cannot start a conversation", which does not
 * point at "you picked too many".
 * Blocking at the checkbox right away and showing "N/20 selected" puts the limit where the user
 * makes the decision.
 *
 * ⚠️ **Built-in skills count too** (pinned by the backend's `TestBuiltinSkillCountsTowardArkLimit`),
 * and they are checked by default, so the create page already uses a few on entry, and the count
 * must include them.
 */
export const MAX_SKILLS_PER_AGENT = 20;

/**
 * Whether a skill's body can be edited.
 *
 * `builtin` cannot: the body is a platform code constant, guarded by a 403 from the backend.
 * This function only makes sure **the UI does not show the edit entry**; showing it and then
 * erroring would make users think it is a bug.
 *
 * To change it, fork it: the copy is `custom`.
 *
 * ⚠️ **When adding a source, this must be updated too.** Missing it gives no compile error: this is
 * an `===` comparison rather than
 * an exhaustive switch, so a new source would silently become "editable", the skill library would
 * render an Edit button,
 * the detail page's body and attachments would become editable, and users would only hit the
 * backend's 403 **at the moment of clicking save**.
 */
export function isReadOnlySkill(skill: { source: SkillSource }): boolean {
  return skill.source === "builtin";
}

/**
 * Which text the source badge of a skill should show, and its testid suffix.
 *
 * # Why such a function
 *
 * The badge renders once in **four places**: skill library cards (SkillsPage), the skill detail page
 * (SkillDetailPage), the binding list on the Agent detail page (AgentDetailPage), and the
 * "pick from skill library" dialog
 * when creating an Agent (components/SkillLibraryPicker). The four are independent
 * copies, and changing one while missing three **gives no error**; the skill just looks self-built on
 * some pages.
 *
 * ⚠️ The **inline** checkbox area on the create Agent page (`NewAgentCapabilities` in `AgentsPage`)
 * is still
 * not on this list: its data source is `skills.filter(isBuiltinSkill)`, listing only built-in skills,
 * so hard-coding a "platform built-in" chip there is right. Checking workspace skills has moved into
 * the dialog above,
 * which uses this function. **Do not mix these two up**: in the same file, one should use it and one
 * should not.
 *
 * Returns null when no badge should be shown (`custom`).
 *
 * # labelKey rather than label
 *
 * Returns an **i18n key** rather than a Chinese literal: call sites must `t()` it to display,
 * so the English UI is right too.
 *
 * ⚠️ **The testid keeps the existing literal** (`builtin`): e2e relies on selectors like
 * `skill-builtin-*` and
 * `bind-skill-builtin-*`, and changing it would turn a batch of tests red.
 */
export function skillSourceBadge(
  source: SkillSource,
): { testid: string; labelKey: "skill.builtin" } | null {
  switch (source) {
    case "builtin":
      return { testid: "builtin", labelKey: "skill.builtin" };
    default:
      return null;
  }
}

/**
 * Skill name validation rule: lowercase letters, digits and hyphens, and the first character is not
 * a hyphen.
 *
 * **Placed here rather than in a page**: both the manual form and upload parsing (mock) need it,
 * and the matching backend implementation is domain.ValidSkillName. If any two of the three
 * implementations drift,
 * the symptom is "some path can create a skill name that Ark rejects", and that rejection happens on
 * another path.
 */
export const SKILL_NAME_PATTERN = /^[a-z0-9][a-z0-9-]*$/;

/**
 * A file bundled with a skill package (only for zip uploads).
 *
 * `content` only has a value in the **parse preview**: the list endpoint returns all skills of a
 * workspace at once, and including bytes would make
 * the response grow linearly with attachments, so the backend omits it on that path (omitempty on
 * the Go side).
 * So it is optional rather than `string | null`: the key being absent is expected.
 *
 * `size`, on the other hand, always has a value; the UI uses it to show the size.
 */
export interface SkillFile {
  /** Path relative to the skill root, like scripts/init.sh. The backend has already done the Zip Slip check. */
  path: string;
  size: number;
  /** base64-encoded content (that is how Go's []byte serializes), only present in previews. */
  content?: string;
}

/**
 * The content of one skill attachment (fetched when a file is opened on the detail page).
 *
 * The shape matches `SandboxFileContent`, so the render branches of the two can be copied from each
 * other.
 *
 * ⚠️ **`content` is plain text, not base64**: deliberately different from `SkillFile.content` above:
 * that field is Go's `[]byte` (serialized as base64), while here the Go side is a `string`.
 * Decoding it again would turn normal content into garbage.
 */
export interface SkillFileContent {
  /** The normalized path, which may differ from the one passed in the request (`./a.md` → `a.md`). */
  path: string;
  /** Number of bytes returned this time (the truncated length when truncated). */
  size: number;
  /** Text content. Empty string when `binary` is true. */
  content: string;
  /**
   * Only the first 256 KiB were returned.
   *
   * ⚠️ **Since 2026-09 this field really can be true.** The preview threshold (256 KiB) used to
   * equal exactly
   * the per-file upload limit, so normal attachments were never truncated; the attachment limit has
   * since been raised to 25 MiB,
   * while the preview threshold deliberately was not (stuffing 25 MiB into JSON and rendering it in
   * the browser makes no sense).
   * So on large attachments it is normal, and the "only showing the first 256 KiB" hint is now really
   * seen.
   *
   * The field is also kept to match the shape of
   * `SandboxFileContent`, and to cover over-limit rows that may exist in old data.
   */
  truncated: boolean;
  /**
   * This is a binary file (decided by containing NUL bytes); there is no content to view.
   *
   * **A separate field rather than stuffing a sentence into content**: otherwise the frontend could
   * not tell
   * "the file content happens to be this sentence" from "we judged it binary".
   */
  binary: boolean;
}

export interface Skill {
  id: string;
  workspaceId: string;
  /** Skill name on the Ark side, [a-z0-9-]. */
  name: string;
  /** Ark field display_title. */
  displayTitle: string;
  /** Decides when the model calls this skill; how well it is written directly affects the hit rate. */
  description: string;
  /** Skill body, i.e. the content of SKILL.md. */
  instructions: string;
  source: SkillSource;
  latestVersion: string;
  tags: string[];
  /**
   * Files bundled with the skill package; **manually written skills do not have this key**.
   *
   * Optional rather than a required array: the backend uses omitempty (the key disappears when there
   * are no attachments), and the seed also lacks
   * this key; making it required would break both seed.ts and Go's TestSeedRoundTrip.
   * Always read it as `skill.files ?? []`.
   */
  files?: SkillFile[];
  createdAt: string;
  updatedAt: string;
}

/**
 * The parse result of an uploaded skill package, **not stored yet**.
 *
 * The user sees it first, confirms nothing was parsed wrong, then decides whether to really create it
 * (see the upload flow in SkillsPage).
 * Kept apart from SkillDraft because this has files, while the manual form has no concept of files.
 */
export interface SkillPackage {
  name: string;
  displayTitle: string;
  description: string;
  instructions: string;
  /** Does not include SKILL.md itself (its content is in instructions). */
  files: SkillFile[];
}

export interface SkillDraft {
  name: string;
  displayTitle: string;
  description: string;
  instructions: string;
  tags: string[];
}

// ---------------------------------------------------------------------------
// Feishu apps (AMA's own)
// ---------------------------------------------------------------------------

export type FeishuAppStatus = "unverified" | "connected" | "error";

export const FEISHU_STATUS_LABELS: Record<FeishuAppStatus, string> = {
  unverified: "待验证",
  connected: "已连接",
  error: "连接异常",
};

/** Mapping from status to semantic color. Shared by the list page and the Agent detail page, so the two do not drift. */
export const FEISHU_STATUS_COLOR: Record<FeishuAppStatus, "success" | "warning" | "danger"> = {
  connected: "success",
  unverified: "warning",
  error: "danger",
};

/**
 * Readiness status. Installed does not mean usable; the user actions for the four states are
 * completely different:
 * setup_required means fixing config on the open platform yourself, waiting_for_admin means you can
 * only nudge the admin,
 * runtime_unavailable is usually invalid credentials.
 */
export type FeishuReadinessStatus =
  | "setup_required"
  | "waiting_for_admin"
  | "ready"
  | "runtime_unavailable";

/**
 * A diagnosable readiness report. Deliberately not a boolean: "not usable" has seven or eight
 * causes,
 * and returning only false would leave users trying them one by one.
 */
export interface FeishuReadiness {
  status: FeishuReadinessStatus;
  checkedAt: string;
  /** Scopes never requested at all; the user must add them on the open platform. */
  missingScopes: string[];
  /**
   * Scopes requested and waiting for admin approval.
   * Keeping it apart from missingScopes is the core of this report: one needs the user to change
   * config, the other can only wait for approval.
   * Merging them into one field would make users request already-submitted scopes again and again.
   */
  unapprovedScopes: string[];
  missingEvents: string[];
  missingCallbacks: string[];
  /** Whether the app details were read. When false the booleans below cannot be trusted. */
  configurationInspectable: boolean;
  botEnabled: boolean;
  published: boolean;
  groupMessageScopeGranted: boolean;
  /** Name of the group message scope (im:message.group_msg), sent by the backend so both sides do not hard-code different values. */
  groupMessageScope: string;
  /** URL to request that scope on the open platform. **Empty when already granted**; empty/non-empty is "whether to show the hint". */
  groupMessageAuthURL?: string;
  /**
   * Whether the scope "push group messages sent by **bots** to me too" is granted.
   *
   * ⚠️ Kept apart from groupMessageScopeGranted: that level **only covers messages sent by users**,
   * and not a single bot message is pushed. Alert groups are all bots talking, so without this level
   * nothing is received at all.
   */
  botMessageScopeGranted: boolean;
  /** Name of that scope (im:message.group_msg.include_bot:read), sent by the backend. */
  botMessageScope: string;
  /** URL to request it. **Empty when already granted**. */
  botMessageAuthURL?: string;
  adminAuthorizationRequested: boolean;
  /**
   * Whether it can really connect.
   *
   * ⚠️ **Do not use it to judge "can messages come in"**: what the backend sets is one REST
   * liveness probe
   * (reading bot info), unrelated to the long-lived connection. If the websocket is down and Feishu
   * pushes no messages at all,
   * it is still true. See the comment in server/feishu/app.go.
   */
  runtimeConnected: boolean;
  lastConnectedAt?: string;
  lastRuntimeError?: string;
  lastCheckError?: string;
}

/**
 * When the bot speaks in groups.
 *
 * **An enum, not a boolean**: there will be a third level later (e.g. "trigger on every group
 * message"), and a boolean would need
 * another field, while two booleans' four combinations include meaningless states. Matches the
 * backend's feishu.TriggerMode.
 */
export type FeishuTriggerMode = "mention_only" | "topic_and_mention";

/**
 * A connected Feishu app.
 *
 * It uses device-code self-registration, so there is **no** appSecretMasked / encryptKeySet /
 * verificationTokenSet: users no longer type credentials, and the platform does not return secrets.
 */
export interface FeishuApp {
  id: string;
  workspaceId: string;
  name: string;
  /** Like cli_xxx. */
  appId: string;
  avatarUrl?: string;
  botOpenId?: string;
  /** Account domain: feishu (China) or lark (international). */
  region: string;
  /** The tenant reported by Feishu at registration. */
  tenantBrand?: string;
  /** Coarse status; for details see readiness. */
  status: FeishuAppStatus;
  /**
   * When the bot speaks in groups. **The server guarantees it is non-empty**: old data does not have
   * this field,
   * but the read path normalizes it to mention_only (see App.Clone on the Go side), so the frontend
   * need not check for empty.
   */
  triggerMode: FeishuTriggerMode;
  /** The event callback URL generated by the platform. */
  /**
   * Back reference, to see bindings from the app side.
   *
   * **One app can only be bound to one agent** (1:1 exclusive), so in practice there is at most one
   * element.
   * It is still an array rather than `string | null`: the field shape is part of the HTTP contract,
   * and changing it to a scalar would touch frontend, backend and all existing data, while existing
   * data may still have invalid multi-binding records;
   * such data must be readable and visible to be fixed.
   */
  boundAgentIds: string[];
  readiness?: FeishuReadiness;
  adminAuthorizationRequestedAt?: string;
  createdById: string;
  createdAt: string;
  updatedAt: string;
}

/** Polling status. Everything except pending is terminal. */
export type FeishuRegistrationStatus =
  | "pending"
  | "completed"
  | "expired"
  | "denied"
  | "invalid";

/** The result of starting onboarding; poll with sessionId. */
export interface FeishuRegistration {
  sessionId: string;
  /** The Feishu authorization URL for the user to click. */
  verificationUrl: string;
  expiresAt: string;
  pollIntervalSeconds: number;
}

/** The result of one poll. */
export interface FeishuRegistrationResult {
  status: FeishuRegistrationStatus;
  /** How long to wait before the next poll. The backend raises it when Feishu thinks we are too fast. */
  pollIntervalSeconds: number;
  /** Only present when status is completed. */
  app?: FeishuApp;
  /** Failure reason, a Chinese sentence that can be shown directly. */
  reason?: string;
}

// ---------------------------------------------------------------------------
// Run history
// ---------------------------------------------------------------------------

/**
 * Ark's SessionStatus has only four values: idle / running / terminated / rescheduling.
 * requires_action is a frontend-derived state: on the Ark side it is still idle, but when
 * `stop_reason` is requires_action
 * it **is not idle**: under self_hosted it means Ark has handed control back to AMA, and the tool is
 * running in our own
 * container, with Ark waiting for `user.tool_result` to come back.
 *
 * ⚠ **It does not mean "waiting for user confirmation".** Every tool call goes through this state,
 * and nobody is waiting for the user to click
 * anything. Really waiting for a user click is a different thing (`evaluated_permission === "ask"`
 * with no
 * `user.tool_confirmation` yet), and that state lives at the event level, not the RunStatus level.
 */
export type RunStatus =
  | "running"
  | "idle"
  | "requires_action"
  | "rescheduling"
  | "terminated";

export const RUN_STATUS_LABELS: Record<RunStatus, string> = {
  running: "运行中",
  idle: "空闲",
  requires_action: "工具执行中",
  rescheduling: "调度中",
  terminated: "已结束",
};

export type RunTrigger = "manual" | "feishu" | "api" | "schedule" | "webhook";

export const RUN_TRIGGER_LABELS: Record<RunTrigger, string> = {
  manual: "手动",
  feishu: "飞书",
  api: "API",
  schedule: "定时",
  webhook: "Webhook",
};

export type ThreadStatus = "idle" | "running" | "terminated" | "rescheduling";

export interface RunThread {
  /** session_thread_id */
  id: string;
  agent_name: string;
  status: ThreadStatus;
  /** null means the main thread. */
  parent_thread_id: string | null;
  startedAt: string;
  endedAt?: string;
}

export interface Run {
  id: string;
  workspaceId: string;
  agentId: string;
  agentName: string;
  title: string;
  status: RunStatus;
  trigger: RunTrigger;
  /** E.g. the Feishu group name, the caller identifier. */
  triggerDetail?: string;
  /**
   * The schedule that triggered this run; only present when trigger is schedule.
   *
   * **A different thing from triggerDetail**: that stores the task name (for people), this stores the
   * id (for machine lookup).
   * Names can change and repeat; looking up history only by name would break the moment it is
   * renamed, and break with no error.
   *
   * ⚠️ **Historical runs before 2026-08-30 do not have this field**, and it cannot be backfilled. So
   * the count filtered by it may be
   * less than `schedule.runCount`; the difference is the earlier ones. The detail page must state
   * this gap,
   * so people do not read it as "this task has only run this many times".
   */
  scheduleId?: string;
  startedAt: string;
  /**
   * The time of the last event; **the conversation list sorts by it** (not startedAt).
   *
   * Feishu reuses conversations by scope; the Nth turn is still appended to the first turn's Run,
   * and startedAt does not change;
   * sorting only by startedAt would leave just-active conversations at the bottom of the list. Old
   * data lacks this key, so sorting must fall back.
   */
  lastActivityAt?: string;
  endedAt?: string;
  durationMs?: number;
  usage: TokenUsage;
  threads: RunThread[];
  eventCount: number;
  errorSummary?: string;
  /**
   * The linked agent has been deleted → this conversation is frozen (no more messages, history only).
   *
   * **Derived by the server, not stored**: agents are hard deleted, with no deleted_at, so the backend
   * computes it when reading the Run
   * (markAgentDeleted in server/core/ark.go). So it only appears when true,
   * and a missing key is exactly the same as false; do not write checks like `=== false`.
   *
   * agentName is a snapshot from when the conversation was created; it stays after the agent is
   * deleted, so we know whom the conversation belonged to.
   */
  agentDeleted?: boolean;
  /**
   * The linked agent has been **disabled** → this conversation is also view-only, no sending.
   *
   * Side by side with agentDeleted rather than merged into one frozen: the two need different user
   * actions (one rebinds
   * or creates a new one, the other switches the status back to "enabled"); merged, the UI could not
   * say what to do.
   *
   * **Derived by the server, not stored**, from the same source and the same database query as
   * agentDeleted
   * (markAgentDeleted in server/core/ark.go). A missing key equals false.
   *
   * ⚠️ Unlike agentDeleted: **disabling is reversible**, and when the user re-enables it this value
   * should disappear.
   * So do not cache it in component state.
   */
  agentDisabled?: boolean;
  /**
   * Which host machine this conversation's tools run on (hst_ prefix).
   *
   * **Snapshotted from the agent when the conversation is created**: later changes to the agent's
   * executor do not affect existing runs.
   * Empty/missing = running in a Docker container, the default. The chat page's container panel
   * branches on it:
   * when non-empty it does not query the Docker container's four states, and shows a "running on host"
   * card instead.
   */
  hostId?: string;
  /** Host source snapshot (taken from the agent when the conversation is created); empty string is interpreted as workspace. Terminal permissions are tiered by it. */
  hostSource?: HostSource;
}

/**
 * One queued pending message (the server queue, field by field matching the backend's
 * domain.PendingMessage).
 *
 * # ⚠️ This is not the same as "automatic resend after hitting the tool-execution window"
 *
 * That is a **race** of tens of milliseconds (Ark rejects user.message between sending tool_use and
 * the worker returning tool_result),
 * unrelated to user intent, retried in place by the server without the user noticing; the frontend
 * fallback for it
 * is still in `lib/useSendQueue.ts`.
 *
 * This is **the user queueing on purpose**: they know the agent is still running and must wait a
 * whole turn (tens of seconds to minutes),
 * they just do not want to hold it in. The wait times differ by three orders of magnitude. Do not
 * merge them.
 *
 * Stored in the database rather than only in browser state, because the latter is wrong in four
 * cases: closing the page / refreshing /
 * opening several tabs / the Feishu path.
 */
export interface PendingMessage {
  id: string;
  /** Which conversation it goes to. **Resend back to this one**; do not substitute the runId in the current URL. */
  runId: string;
  text: string;
  status: PendingMessageStatus;
  source: PendingMessageSource;
  /** Who queued it (email). Empty string if unavailable, and then only admins can touch it. */
  actor?: string;
  /**
   * How many send attempts have been made; at the limit it is marked failed.
   *
   * **Required**: 0 is a valid value (not tried yet); the backend deliberately has no omitempty.
   */
  attempts: number;
  /** Reason for the last failure, to show the user "why it was not sent". */
  lastError?: string;
  /**
   * Queue time, **an ISO string with fixed three decimals** (not a number of milliseconds).
   *
   * Written in the same write as that timestamptz in the database: that one is for database sorting,
   * this one for frontend display.
   */
  queuedAt: string;
}

/**
 * State of a queued message.
 *
 * ⚠️ **Not the "conversation state"** (that is RunStatus, driven by Ark events). This only serves
 * multi-instance mutual exclusion for "who sends this", and "tell the user when it cannot be sent".
 *
 * There is no sent level: once sent it is deleted, and the message then lives in RunEvent.
 */
export type PendingMessageStatus =
  /** Waiting for the previous turn to finish. */
  | "pending"
  /** Claimed by some instance, being sent. */
  | "sending"
  /** Failed to send again and again, given up. The text is kept for the user to resend or rewrite. */
  | "failed";

/** Where a queued message came from. Web and Feishu share the same queue. */
export type PendingMessageSource = "web" | "feishu";

/**
 * Aggregated results for the usage and insights dashboard (AMA's own, camelCase).
 *
 * Computed by the server (server/core/usage.go), not reduced in the frontend, which would need all
 * runs pulled into
 * the browser. Three bases match the backend word for word; changing one side requires changing the
 * other:
 *
 * 1. **Duration stats only include finished runs**. `Run.durationMs` is **missing** rather than 0
 *    while unfinished,
 *    and taking it as 0 in the sample would drag P50 all the way to 0, showing on the page as "the
 *    agent got faster".
 *    So completedRuns is given separately to report the sample size, and the UI uses it to label n
 *    or show "—".
 * 2. **Percentiles are nearest-rank, not interpolated**. Sample sizes are often single digits, and
 *    interpolated numbers never happened.
 * 3. **The failure check is errorSummary non-empty**, not status: RunStatus has no "failed" level
 *    (terminated covers both normal endings and errors), matching OverviewPage's logic.
 *
 * All numeric fields are required: 0 is a valid value, and a missing key would make the frontend
 * compute NaN.
 */
export interface UsageSummary {
  totalRuns: number;
  /** Number of runs with non-empty errorSummary. */
  failedRuns: number;
  /** Number of finished runs, i.e. the sample size for duration stats; when 0 all three percentiles are 0 and should not be shown. */
  completedRuns: number;
  usage: TokenUsage;
  p50Ms: number;
  p90Ms: number;
  avgMs: number;
  /** Descending by call count, ties ascending by name code point (stable order, so the chart does not jump). */
  agents: AgentUsage[];
  /** Ascending by date, used left to right in the trend chart directly. */
  daily: DailyUsage[];
}

/** Usage of one agent. agentName is a snapshot on the run, kept after the agent is deleted. */
export interface AgentUsage {
  agentId: string;
  agentName: string;
  /** Unlike Run.agentDeleted: this always has a value (the backend always writes it); do not make it optional. */
  agentDeleted: boolean;
  runs: number;
  failedRuns: number;
  completedRuns: number;
  usage: TokenUsage;
  p50Ms: number;
  p90Ms: number;
  avgMs: number;
}

/** Usage of one day; date looks like 2026-08-25 (a UTC day). */
export interface DailyUsage {
  date: string;
  runs: number;
  usage: TokenUsage;
}

// ---------------------------------------------------------------------------
// Schedules (proactive capability)
// ---------------------------------------------------------------------------

/**
 * Trigger kind.
 *
 * `oneshot` serves two scenarios: the user saying "remind me in 1 hour", and the agent scheduling
 * itself
 * "wake up in 10 minutes to check CI". Both have the same shape, told apart by `origin` (who
 * scheduled it).
 */
export type ScheduleKind = "cron" | "interval" | "oneshot";

export const SCHEDULE_KIND_LABELS: Record<ScheduleKind, string> = {
  cron: "按 Cron",
  interval: "固定间隔",
  oneshot: "一次性",
};

/**
 * Who scheduled this task.
 *
 * ⚠️ **Currently there is only one value, `user`**: agent self-wakeup (`agent_self`) was removed,
 * and existing data was rewritten to `user` by a backend
 * migration. The type is kept because it is in the HTTP contract, and "who scheduled it" may get a
 * second answer later.
 */
export type ScheduleOrigin = "user";

export type ScheduleStatus = "active" | "paused";

export const SCHEDULE_STATUS_LABELS: Record<ScheduleStatus, string> = {
  active: "已启用",
  paused: "已暂停",
};

/** Default time zone for cron. "Weekdays 9:30" written down is local time, not UTC. */
export const DEFAULT_SCHEDULE_TZ = "Asia/Shanghai";

/** Lower bound for interval and oneshot delays (ms). Shorter makes no sense, and the server rejects it too. */
export const MIN_SCHEDULE_INTERVAL_MS = 60_000;

/** After this many consecutive failures the server pauses it automatically (to prevent avalanches). */
export const MAX_CONSECUTIVE_FAILURES = 5;

export interface ScheduleSpec {
  kind: ScheduleKind;
  /** A 5-field expression (minute hour day month weekday); required when kind is cron. */
  cron?: string;
  /** IANA time zone name, only meaningful for cron. Empty means DEFAULT_SCHEDULE_TZ. */
  tz?: string;
  /** Interval in ms; required when kind is interval. */
  everyMs?: number;
  /** Fire time; required when kind is oneshot. */
  fireAt?: string;
}

/**
 * Where the result is delivered.
 *
 * ⚠️ There used to be a third value `issue` (the server parsed a JSON array from the agent output to
 * create issues),
 * **now removed**: that contract was too fragile; one JSON formatting slip wasted a whole
 * inspection round. Inspection now uses
 * `run_only`, with the agent calling `ama issue create` / `ama issue comment` itself.
 * The server no longer accepts this value (it returns 400), so do not add it back.
 */
export type DeliveryKind = "run_only" | "feishu";

export const DELIVERY_KIND_LABELS: Record<DeliveryKind, string> = {
  run_only: "只记录到运行历史",
  feishu: "发到飞书",
};

/**
 * The delivery address, **frozen** when the task is created.
 *
 * Frozen rather than computed at fire time: by then it is a brand new agent session, with no context
 * of "where that message came from at the time",
 * so the agent could only guess, and a wrong guess gives no error, just sends the report to another
 * group.
 */
export interface DeliveryTarget {
  kind: DeliveryKind;
  /** AMA-side Feishu app ID (not cli_xxx); required when kind is feishu. */
  feishuAppId?: string;
  /** Feishu chat ID (oc_xxx); required when kind is feishu. */
  chatId?: string;
  /** Non-empty means this task came from a topic. */
  threadId?: string;
  /**
   * The message to reply to when delivering.
   *
   * **Only with it can delivery go into a topic**: Feishu's send-message API only takes chat_id, and
   * to land in a topic you must reply to
   * some message in the topic. When empty it falls back to sending straight to the chat.
   */
  anchorMessageId?: string;
  /** Group name snapshot, for display only. May be empty (inbound messages carry no group name), and then the UI shows chatId. */
  chatName?: string;
}

export interface Schedule {
  id: string;
  workspaceId: string;
  agentId: string;
  /** Agent name snapshot at task creation, so ownership is known even after the agent is deleted. */
  agentName: string;
  name: string;
  spec: ScheduleSpec;
  /** The instruction fed to the agent when it fires. */
  prompt: string;
  /**
   * Context summary from task creation, prepended to the prompt when it fires.
   *
   * Every fire is a brand new session with zero history, relying on this to carry context on.
   * Self-wakeup depends on this field especially.
   */
  contextSummary?: string;
  origin: ScheduleOrigin;
  status: ScheduleStatus;
  delivery: DeliveryTarget;
  /** Next fire time. Absent when paused; **also absent at the moment it is running** (the server clears it when claiming). */
  nextRunAt?: string;
  lastRunAt?: string;
  /** The Run produced by the last fire; the UI uses it to jump to run detail. */
  lastRunId?: string;
  lastError?: string;
  /** All three counters are required: 0 is a valid value, and a missing key would make the page compute NaN. */
  runCount: number;
  failCount: number;
  consecutiveFailures: number;
  createdAt: string;
  updatedAt: string;
  /** Derived by the server: the linked agent has been deleted, so this task cannot produce results. Same as Run.agentDeleted. */
  agentDeleted?: boolean;
}

export interface ScheduleDraft {
  agentId: string;
  name: string;
  spec: ScheduleSpec;
  prompt: string;
  contextSummary?: string;
  delivery: DeliveryTarget;
  /** It may be paused already at creation (configure first, enable later). */
  status?: ScheduleStatus;
}

/** Feishu groups the bot has seen; the dropdown data source when configuring the delivery target. */
export interface FeishuChat {
  chatId: string;
  /** AMA-side app ID. */
  feishuAppId: string;
  chatType: string;
  /** May be empty: inbound messages carry no group name, and then the UI shows chatId. */
  name: string;
  lastSeenAt: string;
}

/**
 * Validate the **shape** of a cron expression (5 fields, field ranges).
 *
 * Only the part the frontend can do: the real semantic validation is on the server (robfig/cron).
 * This blocks the most common typos, saving a round trip. Returns null when there is no problem.
 */
export function validateCron(expr: string, t: TFunc = zhT): string | null {
  const parts = expr.trim().split(/\s+/).filter(Boolean);
  if (parts.length !== 5) {
    return t("domain.cronFieldCount");
  }
  const ranges: [number, number][] = [
    [0, 59],
    [0, 23],
    [1, 31],
    [1, 12],
    [0, 7],
  ];
  for (let i = 0; i < parts.length; i++) {
    const [min, max] = ranges[i];
    // Only check plain numbers and number ranges; names (mon-fri) and steps (*/15) are left to the
    // server.
    for (const token of parts[i].split(",")) {
      const body = token.split("/")[0];
      if (body === "*") continue;
      for (const n of body.split("-")) {
        if (!/^\d+$/.test(n)) continue;
        const v = Number(n);
        if (v < min || v > max) {
          return t("domain.cronOutOfRange", { field: i + 1, value: v, min, max });
        }
      }
    }
  }
  return null;
}

// ---------------------------------------------------------------------------
// Authorization management (multiple authorization providers)
// ---------------------------------------------------------------------------

/**
 * The state of one authorization source, **four states**.
 *
 * # Why four and not two, "authorized / not authorized"
 *
 * Two states cannot express these real things that need completely different handling, especially
 * the first two:
 *
 * - `missing` not configured on the server yet → an **admin** must configure it, and the user
 *   clicking authorize **does nothing**;
 * - `error`   needs the user or an admin to act to recover (not authorized yet, credentials invalid,
 *   platform unreachable);
 * - `warning` usable but worth knowing (expiring soon, the platform renews automatically) → usually
 *   nothing to do;
 * - `ok`      fine.
 *
 * The worst part of two states is showing `missing` as "not authorized": users click the authorize
 * button again and again on "not authorized",
 * while the problem is in server config, and a hundred clicks do nothing. So whether the card offers
 * a "start authorization" button
 * **does not depend on the state but on `canAuthorize`**, which the backend computes; see below.
 */
export type IntegrationStatus = "ok" | "warning" | "error" | "missing";

/** One name-value pair on the card. The backend guarantees it contains no plain-text credentials. */
export interface IntegrationDetail {
  label: string;
  value: string;
}

/**
 * The full state of one authorization source. **The card list is driven by this array returned by
 * the backend**; the frontend hard-codes no list,
 * otherwise adding a provider on the backend would require changing the frontend, and missing it
 * gives no error, just one card fewer on the page.
 *
 * The backend asks the platform live every time and stores nothing locally, so this is the truth.
 * For why see server/core/integration.go:
 * keeping a copy would drift from the platform, and the drift shows up as "the page says authorized,
 * tool calls still fail",
 * the hardest kind to self-check.
 */
/**
 * A Feishu app that "Feishu user authorization" can choose.
 *
 * # ⚠️ Why it is not a detail of IntegrationState
 *
 * `details` are `{label, value}` name-value pairs; they can say "currently using cli_xxx",
 * but not "which three can be chosen and whether each is authorized", which is a structured table.
 * Stuffing it into name-value pairs
 * would mean joining a string and parsing it in the frontend, and **that parsing is sure to drift**:
 * change a separator on the backend and this side
 * silently misaligns, with nothing reporting an error. So it uses its own endpoint
 * (`GET /api/integrations/{provider}/apps`).
 *
 * # ⚠️ There are no credentials here at all
 *
 * The app_id (`cli_xxx`) and app name are **not** secrets: the former is visible to everyone in a
 * Feishu group.
 * app_secret is, so this type **has no field for it**, rather than "remember not to put it".
 * See the iron rule in the file header of server/core/integration.go.
 */
export interface LarkUserApp {
  /** Feishu app id, like `cli_xxx`. **This is the key used when choosing**. */
  appId: string;
  /**
   * App display name.
   *
   * ⚠️ **It is not enough to tell them apart**: measured, one person's three apps all had the same
   * name (all created from the same
   * doc). So in the UI the name is the main title, and `appId` must be shown with it as secondary
   * information;
   * with names only, users facing three identical options could not choose.
   */
  name: string;
  /**
   * Which workspace this app belongs to (display name).
   *
   * When names repeat, this column is often the only thing users can tell apart, so it cannot be
   * omitted.
   */
  workspaceName: string;
  /**
   * Whether this app **has been authorized**.
   *
   * Computed per app, not one global boolean: that table's primary key is `(actor, app_id)`,
   * naturally supporting several apps and several tokens per person. With a global boolean, after
   * the user switches to an app never authorized,
   * the card would still show "authorized", and then the agent fails as soon as it calls lark-cli.
   */
  authorized: boolean;
  /**
   * Whether it is the one used by **current default**.
   *
   * ⚠️ "Current" means the account-level default (for card display, starting/revoking
   * authorization). When an agent runs,
   * which app is injected **is decided by the run's workspace**, ignoring this value; that is a
   * correctness issue,
   * see UserAppResolver in server/feishu/apps.go.
   */
  current: boolean;
}

export interface IntegrationState {
  /** Stable identifier of the provider (ama / lark-user), used as key and testid. */
  id: string;
  /** Card title. */
  name: string;
  /** Card subtitle. */
  description: string;
  status: IntegrationStatus;
  /** One sentence saying what is going on now; present in all four states. */
  summary: string;
  /** Extra information (authorized user, platform-side identifier, valid until...); may be empty. */
  details?: IntegrationDetail[];
  /** "What to do"; should be present when not ok. */
  fixHint?: string;
  /** "Where to do it"; may be empty. Points to external addresses like the platform console, opened in a new tab. */
  fixUrl?: string;
  /**
   * Whether the "start authorization" action makes sense right now.
   *
   * ⚠️ **This is where "not connected ≠ not authorized" lands in the UI.** When not connected it is
   * false,
   * and the card **does not render** that button; if rendered, clicking it would only get a 422,
   * and the user would have no way to know the server must be configured. **Do not guess this from
   * status.**
   */
  canAuthorize: boolean;
  /** Whether "revoke authorization" makes sense. False when the platform does not support revoking; a button sure to fail is worse than none. */
  canRevoke: boolean;
  /** When this state was checked, ISO. */
  checkedAt: string;
  /**
   * Whether this authorization source **injects credentials into the agent container**.
   *
   * ⚠️ **The "authorization" checkbox area on the agent form must filter by this, not guess by
   * `status === "ok"`.**
   * "Ready" and "can inject something" are two different things, and guessing by status is wrong in
   * both directions at once:
   *
   *  - a purely diagnostic source is ok, but its credentials do not go into the container
   *    → checking it does nothing, while the switch looks like it took effect;
   *  - `ama` is missing when the server has no key configured, but it **is** an injecting one
   * 
   *    → it should be shown saying "an admin must configure it", rather than vanishing (vanished,
   *    users would never know it exists).
   *
   * **May be missing**: older backends do not return this field, and then it is treated as false
   * (one option fewer is the safe
   * side, while wrongly showing a switch that never takes effect is not).
   */
  injectable?: boolean;
  /**
   * Whether to **poll for the result** after clicking "start authorization".
   *
   * ⚠️ **This is not an experience preference; it decides whether the feature works at all.** The two
   * kinds of sources complete authorization completely differently:
   *
   *  - Sources using a browser redirect: after the user clicks on the platform page, **the platform
   *    records it itself**, and next time we check status
   *    we can see it. So not polling is also correct (the user clicks "check again").
   *  - Feishu user authorization: uses a **device code**, with no callback. **Only when we actively
   *    poll is the
   *    token exchanged and stored**; without polling, after the user clicks approve in Feishu, this
   *    side forever shows
   *    "not authorized", even if they click "check again" a hundred times.
   *
   * **May be missing**: older backends do not return it, and then it is treated as false (the
   * behavior before this field).
   */
  pollsAuthorization?: boolean;
  /**
   * This source needs the user to **fill in a set of credentials by hand** (currently no source uses
   * it).
   *
   * ⚠️ **It is a different thing from canAuthorize; do not merge them**:
   *
   *  - `canAuthorize && !needsCredential` → one click jumps to the platform authorization page
   *    (like Feishu)
   *  - `canAuthorize && needsCredential`  → fill in a set of credentials in the card
   *
   * The frontend uses it to decide "render a button or a form". Check this rather than the provider
   * id,
   * for the same reason as injectable: with a hard-coded provider list in the frontend, if some source
   * later needs manual credentials too,
   * missing it here **gives no error**; that card just has nowhere to fill anything in.
   *
   * **May be missing**: older backends do not return it, and then it is treated as false (the
   * behavior of the existing five sources).
   */
  needsCredential?: boolean;
  /**
   * This source manages **a set of named credentials**, not a single one.
   *
   * ⚠️ **It is a separate bit from needsCredential; do not let one bit do both**: both are true, but
   * they ask different things:
   *
   *  - `needsCredential && !credentialCollection` → fill in **one**, re-filling overwrites
   *  - `needsCredential && credentialCollection`  → fill in **several**, each named, can add/remove
   *
   * Currently no source uses this bit, and the frontend has no matching form.
   *
   * **May be missing**: older backends do not return it, and then it is treated as false (the
   * behavior of the existing source).
   */
  credentialCollection?: boolean;
  /**
   * The **identity type** of this source: does it stand for a real person, or an app.
   *
   * ⚠️ **You must use this bit from the backend; do not check `id === "service-account"` in the
   * frontend**:
   * same reason as the three bits injectable / needsCredential / credentialCollection: that would
   * mean keeping
   * a second provider list in the frontend, and **missing the classification of a new personal
   * source gives no error**; it would just be
   * shown as a service account, "can be added to a workspace for sharing", while it actually stands
   * for some real person.
   * That is an error in the security direction, found only when something goes wrong.
   *
   * The two kinds are handled completely differently:
   *
   *   personal          the credential stands for a real person. **Only the Agent's creator can add
   *                     or remove it**
   *                     (not even admins), and it **cannot be added at workspace level**
   *   service_account   the credential stands for an app. Admins can bind it to a workspace for
   *                     everyone to share,
   *                     and agents can only pick those already bound in the workspace
   *
   * **May be missing**: older backends do not return it, and then it is treated as `personal`, the
   * **conservative** side
   * (at most the UI offers one fewer "add to workspace" entry, and users will ask),
   * while the reverse would show a personal credential as shareable, a silent overreach hint.
   */
  kind?: IntegrationKind;
}

/**
 * Identity type of an authorization source. **Matches the backend's domain.IntegrationKind word for
 * word.**
 *
 * ⚠️ The value is the underscored `service_account` (not kebab): it is the value of a Go constant on
 * the backend,
 * and **looks like but is not the same thing as** the provider id `service-account`: the former is a
 * category,
 * the latter is that specific source. Mixing them makes checks never match, with zero errors.
 */
export type IntegrationKind = "personal" | "service_account";

// ---------------------------------------------------------------------------
// Host executor
// ---------------------------------------------------------------------------

/**
 * A host machine registered by the user (an account-level resource; the backend fetches by the
 * registrant's email).
 *
 * Matches the backend's `apimodel.Host` field by field. The daemon is installed on the user's
 * machine and registers and heartbeats with a
 * UserAPI Key (AMA_TOKEN); when an agent picks the "host" executor, tool calls
 * go through the work queue to the daemon on that machine.
 */
export interface Host {
  /** hst_ prefix. */
  id: string;
  /** Machine name taken at daemon registration (defaults to hostname; the user can change it). */
  name: string;
  /** Operating system, like darwin / linux. */
  os: string;
  /** Architecture, like arm64 / amd64. */
  arch: string;
  /** Coarse status recorded by the daemon, "online" / "offline". Online is decided by online. */
  status: string;
  /**
   * Online flag **computed live** by the backend from the last heartbeat (within 90 seconds), not
   * cached; it changes on its own,
   * for the same reason Docker container state is not cached. The list polls every 10-15 seconds.
   */
  online: boolean;
  /**
   * GNU tool self-check flag of old daemons, only reported by old versions, no longer used by the
   * UI.
   * @deprecated Use nativeToolsOk.
   */
  gnuToolsOk: boolean;
  /** @deprecated Old GNU self-check output. */
  toolcheck?: string;
  /**
   * Whether the daemon supports the structured native tool protocol (six tools run natively in Go,
   * zero brew dependencies).
   * false means the daemon version is too old, and the UI should prompt an upgrade.
   */
  nativeToolsOk: boolean;
  /** The daemon's version on the machine. May be absent (old versions do not report it). */
  daemonVersion?: string;
  /** ISO string with fixed three decimals (same as this project's other timestamps). */
  createdAt: string;
  /** Time of the last heartbeat. May be absent (no heartbeat yet after registering). */
  lastSeenAt?: string;
}

/**
 * The binding between a workspace and a host. Returned by GET /api/workspaces/{id}/hosts.
 *
 * The Host field is looked up live by the backend by joining the host row (a reference, not a
 * copy).
 */
export interface WorkspaceHost extends Host {
  /**
   * Email of the machine owner (registrant). In the two-level model a workspace list may contain
   * machines bound in by **someone else**,
   * and the UI uses it to label "whose machine". The account-level GET /api/hosts does not return
   * this field.
   */
  ownerEmail?: string;
  /**
   * Who bound this machine into the workspace (lowercase email). May not be the same person as
   * ownerEmail:
   * a machine registered by A may be bound in by admin A/B. Absent for old bindings that did not
   * record it.
   */
  addedByEmail?: string;
  /** When it was bound into the workspace; may be absent for old bindings. */
  boundAt?: string;
}

/**
 * The result of one "is authorization done yet" query (for the device-code flow).
 *
 * ⚠️ **Does not contain device_code**: that is on the same level as a secret (it can be exchanged
 * for a token),
 * and the backend deliberately leaves it out of responses.
 */
export interface AuthorizationStatus {
  /** Credentials obtained and stored; the card can be refreshed. */
  done: boolean;
  /** Still waiting for the user to click approve in Feishu. **This is not an error**; keep polling. */
  pending: boolean;
  /** Suggested seconds until the next poll (given by Feishu). */
  pollInterval?: number;
  /** One sentence for the user. When not pending, it explains the result. */
  message?: string;
}

/**
 * Mapping from state to semantic color.
 *
 * `missing` uses a neutral color rather than warning: it is not "something went wrong", just that
 * this feature is not connected yet;
 * the same reasoning as using a neutral color for "not checked yet" in FeishuReadiness (see the
 * comment in that file).
 */
export const INTEGRATION_STATUS_COLOR: Record<
  IntegrationStatus,
  "success" | "warning" | "danger" | "default"
> = {
  ok: "success",
  warning: "warning",
  error: "danger",
  missing: "default",
};

/** Mapping from state to icon, used by the Notice on cards. */
export const INTEGRATION_STATUS_ICON: Record<IntegrationStatus, string> = {
  ok: "gravity-ui:circle-check",
  warning: "gravity-ui:triangle-exclamation",
  error: "gravity-ui:circle-xmark",
  missing: "gravity-ui:plug-connection",
};

// ---------------------------------------------------------------------------
// Onboarding (account level)
// ---------------------------------------------------------------------------

/**
 * A person's onboarding state. **Account-level, without workspaceId**: "have I seen the onboarding"
 * is a property of this person,
 * unrelated to workspaces (they may not even belong to any workspace yet, which is exactly when
 * onboarding is needed most).
 *
 * The backend takes the actor from the session; the frontend **can neither send nor read anyone
 * else's**; see server/core/onboarding.go.
 */
export interface OnboardingState {
  /** When the onboarding was completed; the key is absent if not completed. */
  completedAt?: string;
  /** When the onboarding was skipped; the key is absent if not skipped. */
  skippedAt?: string;
  /**
   * The onboarding **content version** this person has seen; 0 when there is no record.
   *
   * ⚠️ **The backend always sends this key** (even 0, no omitempty). Without it `version < X`
   * would always be false because undefined takes part in the comparison, so new users would not be
   * onboarded; the one thing this feature
   * must get right would silently fail.
   */
  version: number;
}

/**
 * The version of the current onboarding content. **Increment it when the onboarding content
 * changes.**
 *
 * This is the answer to "what about later revisions": after incrementing, people whose `version` is
 * lower (**including those who completed
 * the previous version**) are onboarded again, and those who have seen the new version are not.
 * Without this mechanism there are only two choices:
 * everyone starts over, or nobody sees it.
 *
 * **The version number belongs to the frontend**: the onboarding content is here, so that number
 * should be here; the backend only stores the value it is told,
 * and does no comparison (see that part in server/core/schema_onboarding_pg.sql).
 */
export const ONBOARDING_VERSION = 1;

/**
 * Whether to show this person the onboarding. No state (still loading) means do not show.
 *
 * **The only check is version; the two timestamps are not looked at.** Because version already
 * implies them:
 * only "completed" or "skipped" writes a version number, so `version >= current version` equals
 * "they have already handled the current onboarding".
 *
 * Conversely, a lagging `version` means seeing it again, which holds for and is wanted by both kinds
 * of people: brand new users (0),
 * and people who saw an old version. This is also why it cannot be written as `if (state.completedAt)
 * return false`:
 * then after a revision old users would never see the new onboarding, and the version field would
 * be useless.
 */
export function shouldShowOnboarding(state: OnboardingState | null): boolean {
  if (!state) return false;
  return state.version < ONBOARDING_VERSION;
}

// ---------------------------------------------------------------------------
// User preferences (account level)
// ---------------------------------------------------------------------------

/** UI language. */
export type Language = "zh-CN" | "en-US";

/** Appearance. system means following the system's prefers-color-scheme. */
export type Appearance = "light" | "dark" | "system";

/**
 * A person's preferences: language and appearance.
 *
 * # It has two storage locations, and neither is redundant
 *
 *   database (user_preferences table)  authoritative, **across devices**
 *   localStorage (ama.prefs.v1)        local cache, **prevents first-frame flicker**
 *
 * The cache cannot be dropped: appearance must be applied to <html> **before** React mounts
 * (main.tsx),
 * and at that moment there is no login, let alone a network round trip. Relying only on the
 * database, every page open would first flash
 * light then switch to dark; that is exactly why localStorage existed in the first place.
 *
 * The database copy cannot be dropped either: switch computers, switch browsers, clear the cache
 * once, and the settings are gone;
 * while the user's mental model is "I set dark mode on this platform".
 *
 * Sync rules are in prefs/PrefsContext.tsx: render with the cache first, then correct with the
 * database copy after login.
 *
 * # The backend always sends both fields (no omitempty)
 *
 * An empty string would make `MESSAGES[""]` undefined, and t() falls back to returning the key as is,
 * **so all page text degrades into strings like "nav.overview"**: the page gives no error, it just
 * all becomes gibberish.
 */
export interface UserPreferences {
  language: Language;
  appearance: Appearance;
}

/**
 * Preferences when never set.
 *
 * **Must match the backend's core.DefaultUserPreferences() word for word.** Otherwise,
 * someone who never set anything would see the UI jump between "first frame uses defaults" and "the
 * API response arrives",
 * a jump nobody triggered.
 */
export const DEFAULT_USER_PREFERENCES: UserPreferences = {
  language: "zh-CN",
  appearance: "system",
};

/**
 * Narrow any input into valid values; unrecognized ones fall back to defaults **field by field**.
 *
 * Matches the backend's normalizeUserPreferences one to one. Both sides need it: the backend keeps
 * dirty data out of the database,
 * the frontend guards against dirty data **already** in the database and cache (written by old
 * versions, or broken by hand edits).
 *
 * **Fall back per field rather than as a whole**: if only the language field of a user is broken,
 * the dark mode they set
 * should not be wiped along with it.
 */
export function normalizeUserPreferences(input: Partial<UserPreferences> | null): UserPreferences {
  return {
    language: input?.language === "en-US" ? "en-US" : "zh-CN",
    appearance:
      input?.appearance === "light" || input?.appearance === "dark"
        ? input.appearance
        : "system",
  };
}

// ---------------------------------------------------------------------------
// Issues (workspace level)
// ---------------------------------------------------------------------------

/**
 * Issue handling status.
 *
 * **An enum, not a boolean** (`closed: boolean`): adding a level later needs no data structure
 * change,
 * and the frontend does not have to guess "does true mean closed or open".
 */
export type IssueStatus = "open" | "in_progress" | "closed";

/**
 * Priority. **Defaults to P2 when not given, not P0**.
 *
 * Defaulting to the highest level would mean every new issue shouts "most urgent", and P0 would lose
 * its meaning at once.
 */
export type IssuePriority = "p0" | "p1" | "p2";

export const ISSUE_STATUSES: IssueStatus[] = ["open", "in_progress", "closed"];
export const ISSUE_PRIORITIES: IssuePriority[] = ["p0", "p1", "p2"];

/** The level used when no priority is given. Matches the backend's domain.DefaultIssuePriority. */
export const DEFAULT_ISSUE_PRIORITY: IssuePriority = "p2";

/**
 * A workspace-level issue record.
 *
 * ⚠️ **Not the same thing as "problem feedback"** (lib/feedback.ts): that is a machine-collected
 * stream reported automatically when errors happen
 * (cross-workspace, deduped, users cannot see what they reported); this is a human-written entity.
 */
export interface Issue {
  id: string;
  workspaceId: string;
  /**
   * Title. **The backend guarantees it is non-empty**: users may leave it empty, and then it is
   * extracted from the first line of the body
   * (Markdown markup stripped, images use alt, overlong truncated); if nothing can be extracted it
   * falls back to "Untitled issue".
   */
  title: string;
  /** Markdown body; may be an empty string (only a title is valid). */
  body: string;
  status: IssueStatus;
  priority: IssuePriority;
  /** Creator email. **Only taken by the server from the login identity**; the request body has no such field. */
  author: string;
  createdAt: string;
  /** Refreshed by changes to title / body / status / priority. */
  updatedAt: string;
  /**
   * Comment count and attachment count, **derived fields computed live by the server** (not
   * stored).
   *
   * Required rather than optional: the list page shows counts from them directly, and a missing key
   * would compute NaN
   * (same reason as those numeric fields of TokenUsage).
   */
  commentCount: number;
  attachmentCount: number;
  /**
   * Who produced it. **Default (absent / empty string) = human-written**, as all existing data is;
   * `"schedule"` means produced by a scheduled inspection task.
   *
   * ⚠️ It decides **the dedup scope**: human-written issues are never deduped (two people reporting
   * the same thing should be two issues),
   * while an inspection task rescans the same material every X minutes, and without merging it would
   * flood the list.
   * The two origins are never merged with each other.
   */
  origin?: IssueOrigin;
  /**
   * The "same issue" check, **only meaningful for machine-produced issues**; human-written ones never
   * have it.
   * A second finding with the same workspace + same origin + same fingerprint becomes a comment, not a
   * new issue.
   */
  fingerprint?: string;
}

/** Who produced the issue. Empty / absent means human-written. */
export type IssueOrigin = "schedule";

/**
 * Input for creating / editing an issue.
 *
 * ⚠️ **Two kinds of "empty" mean different things**; this is the easiest thing to get wrong in this
 * type:
 *
 *   title / body sent as empty string  → **really cleared** (clearing the title re-extracts it from
 *                                         the body)
 *   status / priority not sent         → this item is not changed (defaults on creation)
 */
export interface IssueDraft {
  title: string;
  body: string;
  status?: IssueStatus;
  priority?: IssuePriority;
}

/** One comment under an issue. **Can be edited**, so it has updatedAt. */
export interface IssueComment {
  id: string;
  issueId: string;
  workspaceId: string;
  body: string;
  author: string;
  createdAt: string;
  /** Differing from createdAt means it was edited; the UI uses it to show "edited". */
  updatedAt: string;
}

/**
 * Attachment **metadata**. ⚠️ No content bytes: to get content, request `url`.
 */
export interface IssueAttachment {
  id: string;
  issueId: string;
  workspaceId: string;
  filename: string;
  /** The type **sniffed by the server**, not the one declared at upload. */
  contentType: string;
  size: number;
  author: string;
  /**
   * Access URL, like `/api/issue-attachments/att_x`.
   *
   * **Can go straight into `<img src>`**: same-origin requests carry the HttpOnly Cookie
   * automatically, so auth works.
   * ⚠️ But it **is not public**: non-members of this workspace get 403 on this URL.
   *
   * Built by the server rather than the frontend: if the frontend forgot to update after a route
   * change, it would show as "all images broken",
   * a purely frontend symptom that cannot point at the real cause.
   */
  url: string;
  createdAt: string;
}

/** Whether an attachment can be shown as an image directly on the page. */
export function isImageAttachment(att: IssueAttachment): boolean {
  return att.contentType.startsWith("image/");
}

// ---------------------------------------------------------------------------
// Product suggestions (platform level)
// ---------------------------------------------------------------------------

/**
 * Suggestion handling status. **Two states**, not three like Issue.
 *
 * Issue's `in_progress` has value because the reporter is in that workspace waiting for the result;
 * suggestions are different: the filer only wants to know "has this been handled"; how far platform
 * staff have gotten internally
 * should be explained by the reply text (a reply can say "next version", an enum level cannot).
 */
export type SuggestionStatus = "open" | "resolved";

export const SUGGESTION_STATUSES: SuggestionStatus[] = ["open", "resolved"];

/**
 * A piece of feedback or suggestion about **the AMA platform itself**.
 *
 * ⚠️ **This is the third kind of "something is wrong" thing, and the three must not be mixed**:
 *
 * - "problem feedback" (`lib/feedback.ts`): a stream **reported automatically by machine** when
 *   errors happen,
 *   cross-workspace, deduped, users cannot see what they reported;
 * - {@link Issue}: human-written **workspace-level** to-dos, with a non-empty `workspaceId`;
 * - this type: human-written product feedback, **platform-level**.
 *
 * ⚠️ **It has no `workspaceId` field, and that is not an omission**: the user is giving feedback on
 * the platform,
 * not on some workspace. Adding an always-empty field would make people think it should be filtered
 * by workspace.
 *
 * Read permissions: **filers see their own, platform staff see all**.
 */
export interface Suggestion {
  id: string;
  /** Title. **The backend guarantees it is non-empty**: when not given it is extracted from the body (the same rules as Issue). */
  title: string;
  /** Markdown body; may be an empty string (only a title is valid). */
  body: string;
  status: SuggestionStatus;
  /**
   * Email of the person who filed it. **Only taken by the server from the login identity**; the
   * request body has no such field.
   *
   * **Real names, but not shown publicly**: ordinary users only see their own, so there is no
   * "colleagues see what I filed"; only platform staff can see who.
   */
  author: string;
  createdAt: string;
  updatedAt: string;
  /**
   * Reply count and attachment count, **derived fields computed live by the server** (not stored).
   *
   * Required rather than optional: the list shows counts from them directly, and a missing key would
   * compute NaN
   * (same reason as Issue.commentCount).
   */
  replyCount: number;
  attachmentCount: number;
}

/**
 * Input for filing / editing a suggestion.
 *
 * ⚠️ **Two kinds of "empty" mean different things**, same as IssueDraft:
 *
 *   title / body sent as empty string  → **really cleared** (clearing the title re-extracts it from
 *                                         the body)
 *   status not sent                    → this item is not changed (open on creation)
 *
 * ⚠️ But **title and body both empty** means "do not touch them this time", so that
 * a "status only" PATCH works (the heuristic in the backend's UpdateSuggestion).
 */
export interface SuggestionDraft {
  title: string;
  body: string;
  status?: SuggestionStatus;
}

/**
 * One reply under a suggestion. **Staff replies and user follow-ups are the same type**;
 * they are told apart by `isStaff`, not by two structures.
 */
export interface SuggestionReply {
  id: string;
  suggestionId: string;
  body: string;
  /**
   * Author email.
   *
   * ⚠️ **Staff replies have an empty string for ordinary users**, deliberately removed by the
   * server (not missing data):
   * such replies speak **in the platform's name**, and showing the email would reveal who develops
   * the platform,
   * and let users message that person directly to push for progress, going around this channel.
   *
   * Staff see real names themselves (they need to know who followed up). So when rendering check "is
   * there a value",
   * **do not check `!isStaff`**: that would hide it from the staff view too.
   */
  author: string;
  /**
   * Whether this was written by platform staff.
   *
   * ⚠️ **Decided and stored at write time**, not computed at read time by comparing author with the
   * list;
   * otherwise when someone is moved off the list, their old official replies would retroactively
   * become user messages.
   */
  isStaff: boolean;
  /**
   * Who produced it. Default (absent / empty string) = written by a person, **currently all of
   * them**;
   * `"agent"` means produced automatically by an agent (reserved for "let agents handle it
   * automatically later").
   */
  origin?: SuggestionOrigin;
  createdAt: string;
  /** Differing from createdAt means it was edited. */
  updatedAt: string;
}

/** Who produced the reply. Empty / absent means written by a person. */
export type SuggestionOrigin = "agent";

/**
 * Suggestion attachment **metadata**. ⚠️ No content bytes: to get content, request `url`.
 *
 * Almost the same shape as {@link IssueAttachment}, **minus workspaceId**
 * (suggestions have no workspace dimension).
 */
export interface SuggestionAttachment {
  id: string;
  suggestionId: string;
  filename: string;
  /** The type **sniffed by the server**, not the one declared at upload. */
  contentType: string;
  size: number;
  author: string;
  /**
   * Access URL, like `/api/suggestion-attachments/sat_x`.
   *
   * **Can go straight into `<img src>`**: same-origin requests carry the HttpOnly Cookie
   * automatically.
   * ⚠️ But it **is not public**: read permission matches the suggestion it belongs to (the filer +
   * platform staff).
   */
  url: string;
  createdAt: string;
}

/**
 * One page of the suggestion list.
 *
 * ⚠️ It is not a bare array: besides `suggestions`, it carries the decision **whether you are
 * platform staff**.
 * The UI uses it to decide which version to render (whether to show the author column, whether to
 * give status controls).
 * **The frontend cannot guess it**: the staff list is server config, unavailable to the frontend.
 */
export interface SuggestionPage {
  suggestions: Suggestion[];
  limit: number;
  offset: number;
  viewerIsStaff: boolean;
}

/** Whether a suggestion attachment can be shown as an image directly on the page. */
export function isImageSuggestionAttachment(att: SuggestionAttachment): boolean {
  return att.contentType.startsWith("image/");
}

// ---------------------------------------------------------------------------
// Entity version history (rollback after mistakes)
// ---------------------------------------------------------------------------

/**
 * Entity types that can keep version history.
 *
 * ⚠️ **Values must match the backend's domain.RevisionKind word for word**
 * (server/internal/domain/revision.go).
 * This side is a discriminated union: one extra value and TypeScript reports an error; one missing
 * and the label lookup fails, showing blank in the UI.
 */
export type RevisionKind = "agent" | "skill" | "schedule";

/**
 * Which action produced this snapshot.
 *
 * `rollback` is the one left by **the rollback itself** (how it looked before rolling back).
 * Without telling them apart, history would show
 * a record of unknown origin, and users could not tell "what I changed" from "what my rollback
 * caused".
 */
export type RevisionReason = "edit" | "rollback";

/**
 * One history snapshot.
 *
 * # ⚠️ `no` is not `Agent.version`
 *
 * "version" already has two meanings in this repo; do not mix in a third:
 *
 * | Name                  | What it is                                                         |
 * | --------------------- | ----------------------------------------------- |
 * | `Agent.version`       | number, a counter incremented on each change, shown as `v7` on the detail page |
 * | `Skill.latestVersion` | string, also a counter but **a string** (exactly the opposite)     |
 * | **`Revision.no`**     | **snapshot sequence number**, starting from 1 for each entity      |
 *
 * `no` counts "how many snapshots are stored", `version` counts "how many changes"; the two are not
 * equal,
 * and `no` **is not guaranteed to be contiguous** (old snapshots beyond the cap are cleaned up by
 * the backend).
 * So do not show it in the UI as "how many changes in total".
 */
export interface Revision {
  id: string;
  workspaceId: string;
  kind: RevisionKind;
  entityId: string;
  no: number;
  /**
   * The entity's content at that moment.
   *
   * ⚠️ **This field is missing in the list endpoint** (the backend deliberately leaves it out; a
   * snapshot is a few KB × 20).
   * To get the body use `getRevision` for a single one, so it is optional and must be checked
   * before reading.
   */
  snapshot?: unknown;
  /** The name of that version at the time. **Deliberately frozen at write time**; staleness is exactly its meaning. */
  summary: string;
  createdAt: string;
  /** Whose change produced this snapshot. **May be absent** (system paths have no human identity). */
  createdBy?: string;
  reason: RevisionReason;
}

/**
 * One page of the history list.
 *
 * ⚠️ Not a bare array: it also carries `maxKept` (the backend's retention cap), which the UI uses to
 * say "at most N versions are kept".
 * **The frontend must not hard-code that number**: then it would rot as soon as the backend changes
 * it, with no error.
 */
export interface RevisionList {
  revisions: Revision[];
  maxKept: number;
}

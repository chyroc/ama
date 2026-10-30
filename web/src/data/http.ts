/**
 * The HTTP implementation of ApiClient, talking to the Go service in server/ (the core + feishu
 * modules).
 *
 * It and LocalApiClient are two implementations of the same interface; page code does not know
 * which one it is talking to. Which one is used is decided at the end of `data/api.ts`.
 *
 * # Fields pass through as is, no mapping
 *
 * The JSON from the backend **is** the frontend's domain model shape: fields mirrored from Ark are
 * snake_case (`builtin_tools` / `parent_thread_id` / `agent_name`), AMA's own are camelCase. The
 * mix is a design decision (see the top of types/domain.ts), not a mistake. So there is not, and
 * should not be, a "camelCase to snake_case" layer here; with one, every new field needs a table
 * entry, and the first field missed would silently vanish as undefined.
 *
 * # Three things you must not change on assumptions
 *
 * **1. Nullable getters check the code, not the status code.** getWorkspaceByKey / getAgent /
 * getSkill / getRun / getFeishuApp return `T | null`. Writing "404 → null" as a status-code check
 * would make "route mounted wrong" (backend returns code=route_not_found) look like "resource does
 * not exist": the page shows a silent empty state, and the cause cannot be found from the symptom.
 * So only not_found / app_not_found count.
 *
 * **2. Error messages come straight from the backend.** The error body is `{message, code}`;
 * message is a Chinese sentence, and pages display it as is after catching
 * (`e instanceof Error ? e.message : fallback`), and e2e asserts the exact text of some of them.
 * So what is thrown here must be an Error with that message, never "HTTP 409".
 *
 * **3. DELETE returns 204 with no body.** Calling res.json() unconditionally would throw on the
 * empty body and turn every successful delete into an error popup. So readBody checks the status
 * code first, then the length.
 */

import type {
  ApiClient,
  ArkConsoleRef,
  AuthProviders,
  GitHubDeviceStart,
  GitHubPollResult,
  AuthUser,
  EventQuery,
  RunQuery,
  UsageQuery,
  SandboxState,
  SandboxListing,
  SandboxShellResult,
  SandboxFileContent,
  TerminalTicket,
} from "@/data/api";
import type {
  Agent,
  AgentDraft,
  AgentSyncTask,
  ApiKey,
  CreatedApiKey,
  ArkCredential,
  ArkCredentialDraft,
  ArkRegion,
  FeishuApp,
  FeishuTriggerMode,
  GlobalArkCredential,
  AgentArkCredential,
  ArkFetchStatus,
  ArkKeyOption,
  VolcAccount,
  IntegrationState,
  AuthorizationStatus,
  LarkUserApp,
  UserProfile,
  Issue,
  IssueAttachment,
  IssueComment,
  IssueDraft,
  IssueStatus,
  Suggestion,
  SuggestionAttachment,
  SuggestionDraft,
  SuggestionPage,
  SuggestionReply,
  SuggestionStatus,
  FeishuChat,
  FeishuRegistration,
  FeishuRegistrationResult,
  OnboardingState,
  PendingMessage,
  Run,
  Revision,
  RevisionKind,
  RevisionList,
  Schedule,
  ScheduleDraft,
  ScheduleStatus,
  Skill,
  SkillDraft,
  SkillFileContent,
  SkillPackage,
  UsageSummary,
  UserPreferences,
  Workspace,
  WorkspaceMember,
  WorkspaceRole,
  WorkspaceRoleInfo,
  Host,
  WorkspaceHost,
} from "@/types/domain";
import type { RunEvent, StreamFrame } from "@/types/events";
import { recordFailedCall, routePatternForReport, type FeedbackPayload } from "@/lib/feedback";

/**
 * API prefix. Defaults to same-origin `/api`, forwarded to the backend by vite's server.proxy;
 * same-origin means no cross-origin, and the backend needs no CORS config. To connect elsewhere,
 * override with VITE_API_BASE (no trailing slash).
 */
const API_BASE = (import.meta.env.VITE_API_BASE ?? "/api").replace(/\/+$/, "");

/**
 * How identity is sent.
 *
 * **Via cookie, with no identity header.** After login the backend sets an HttpOnly
 * `ama_session`, and the browser sends it on same-origin requests automatically; so nothing needs
 * to be done here, `credentials: "same-origin"` (fetch's default) is enough.
 *
 * Three reasons; the first is a hard constraint:
 *
 * 1. **SSE can only work this way.** EventSource cannot send custom request headers. The old
 *    placeholder put the identity in `?actor=`, which writes credentials into the URL (browser
 *    history, server access logs). Cookies are sent by the browser automatically, including for
 *    same-origin EventSource.
 * 2. With HttpOnly, JS cannot read it, so XSS cannot steal it; a token in localStorage is gone
 *    after one XSS.
 * 3. Frontend and backend are same-origin (the frontend build is go:embed-ed into the binary), so
 *    none of the cross-site cookie problems apply.
 *
 * So the old `X-AMA-Actor` header and `VITE_AMA_ACTOR` are gone: "who is this" is decided by the
 * backend session and the frontend cannot change it; that is the point of enforced login.
 */
const CREDENTIALS: RequestCredentials = "same-origin";

/**
 * Error codes for "does not exist". The backend uses these two to mean the resource does not
 * exist; any other 404 (such as route_not_found) is a real error.
 */
const NOT_FOUND_CODES = new Set(["not_found", "app_not_found"]);

/**
 * Error code for not logged in. Every protected backend API returns it when credentials are
 * missing.
 *
 * **Never merge it into NOT_FOUND_CODES**: that would turn "you are not logged in" into null, the
 * page would become a silent empty state, and the user would see neither the login page nor any
 * hint.
 */
export const UNAUTHENTICATED_CODE = "unauthenticated";

/**
 * Error code for no permission. The backend returns it (403) when "not a workspace member" or
 * "role level too low".
 *
 * **Never merge it into NOT_FOUND_CODES**: that would turn "you have no permission" into null and
 * the page into a silent empty state; the user cannot tell it is a permission problem and just
 * thinks things disappeared.
 *
 * It also **should not trigger notifyUnauthenticated**: 403 is not an expired login, and jumping
 * to the login page would just make the user log in again and hit the same wall. Each page should
 * show it as a readable sentence.
 */
export const ACCESS_DENIED_CODE = "access_denied";

/**
 * Response header name for the server logid. **Must match the Go side
 * `shared/gopkg/requestid.Header` exactly** (`server/shared/gopkg/requestid/requestid.go`).
 *
 * fetch's `Headers.get()` is case-insensitive, so it reads whatever form Go's `net/http`
 * canonicalizes it to.
 *
 * ⚠️ **It cannot be read cross-origin unless the server sends `Access-Control-Expose-Headers`.**
 * The default is same-origin (`API_BASE = "/api"`, and production is go:embed same-origin), so it
 * can be read now; once `VITE_API_BASE` points to another host it becomes cross-origin, the
 * browser only exposes the 7 CORS-safelisted response headers, and this header **silently becomes
 * null**; the symptom is "the logid is sometimes there and sometimes not". The server's cors
 * middleware must add it to expose; both sides must be in place together.
 */
const LOGID_HEADER = "X-Request-Id";

/**
 * Callback for when the session expires. Registered by AuthProvider; switches the whole app back
 * to the login page.
 *
 * Why it exists: a session can expire while in use (7 days by default, or logged out
 * elsewhere). After that **every** request gets 401, and if each showed its own error the user
 * would see a screen full of "please log in first" with nowhere to go. Notifying once at
 * request()'s single entry/exit point is the simplest way and the hardest to miss.
 */
let onUnauthenticated: (() => void) | null = null;

/** Register the session-expired callback. Returns a function that unregisters it. */
export function setUnauthenticatedHandler(handler: () => void): () => void {
  onUnauthenticated = handler;
  return () => {
    if (onUnauthenticated === handler) onUnauthenticated = null;
  };
}

function notifyUnauthenticated(): void {
  onUnauthenticated?.();
}

/**
 * API error with a code.
 *
 * message is the Chinese sentence from the backend, shown by pages directly; code is for
 * program checks (nullable getters use it to recognize "does not exist", and future i18n can map
 * it to translation keys without parsing Chinese).
 */
export class ApiError extends Error {
  readonly code: string;
  readonly status: number;
  /**
   * The server logid for this request (response header `X-Request-Id`); empty string if not
   * available.
   *
   * # It turns "a user reported an error" into something you can look up
   *
   * The server generates a logid for every request and writes it into the response header
   * (`withLogID` in `httpd/server.go`); it also goes into **every** log line of that request (the
   * request id field) and downstream. So with it, one report lands directly on that request in
   * the logs, with no guessing by "around what time + which API"; under concurrency timestamps
   * collide, so that kind of guessing was never reliable anyway.
   *
   * # ⚠️ For `network_error` it is always an empty string, and this **is not a missed read**
   *
   * fetch only rejects on network-level failures (DNS failure, connection refused, TLS failure,
   * CORS preflight failure); then **the request never reached the server**: the `withLogID` HTTP
   * middleware never ran, no logid was generated, and the server logs have no line for this
   * request at all. Even the `res` variable has not been assigned; it is not "forgot to read the
   * response header", there is **structurally no response object to read**.
   *
   * So if it is empty here, leave it empty. **Never fall back to a made-up id**: that would make
   * the person debugging think it matched while searching finds nothing (the same mistake as
   * `LogIDDefault` returning `"-"`, forbidden in the comments of `core/observe.go` and
   * `shared/infra/sandbox/client.go`). Generating one in the frontend and putting it in the
   * request header does not work either: the request never arrived, so that id cannot be found in
   * the server logs either; it only turns "empty" into "has a value but finds nothing", which is
   * worse.
   */
  readonly logId: string;

  /**
   * The failed API itself: `apiMethod` is the HTTP method, `apiRoute` is the **route pattern
   * with ids removed** (`/api/workspaces/{id}/agents`). Empty string if not available.
   *
   * # Why record it separately (`routePattern` is not enough)
   *
   * `routePattern` in an issue report is the **page** route (which screen the user is on), not
   * the **API** route. One page, `/w/{id}/chat`, calls a dozen APIs, and level-1 reports **do not
   * collect `recent`**; so before this, a level-1 record could not show "which API failed", which
   * is the first question to answer when debugging.
   *
   * # ⚠️ It stores the pattern, not the real path
   *
   * Where it is assigned (the failure branch of `request()`), it goes through `stripQuery` then
   * `stripIDs`, the same processing as `recordFailedCall`. So it cannot point to a specific
   * session/agent, and the level-1 boundary "do not collect full paths" is not bypassed; the query
   * is also structurally absent (historically the identity really did go into `?actor=`).
   */
  readonly apiRoute: string;
  readonly apiMethod: string;

  constructor(
    message: string,
    code: string,
    status: number,
    logId = "",
    apiMethod = "",
    apiRoute = "",
  ) {
    super(message);
    this.name = "ApiError";
    this.code = code;
    this.status = status;
    this.logId = logId;
    this.apiMethod = apiMethod;
    this.apiRoute = apiRoute;
  }
}

type QueryParams = Record<string, string | string[] | undefined>;

interface RequestOptions {
  query?: QueryParams;
  /**
   * If set, serialized as a JSON body with Content-Type.
   *
   * **FormData is the exception**: it is passed to fetch as is, and **no Content-Type is set**.
   * That header must be generated by the browser because it includes a boundary the browser
   * computes (`multipart/form-data; boundary=----xxx`). A hand-written header without the
   * boundary makes the backend's ParseMultipartForm fail outright, with the error "not a valid
   * multipart form", which does not point here.
   *
   * ⚠️ This branch is not an optional optimization: body is typed unknown, and passing FormData to
   * JSON.stringify silently gives `"{}"` (FormData has no enumerable own properties), so the
   * backend gets an empty JSON object, **with no type error at all**.
   */
  body?: unknown;
}

/**
 * Build the URL.
 *
 * Array values are sent as **repeated parameters** (?status=a&status=b), the natural form of
 * URLSearchParams.append; the backend accepts both forms (see core.multiValue). Empty strings and
 * undefined are never sent: the backend treats "not sent" and "sent empty" as the same, and
 * sending one fewer parameter is cleaner.
 */
function buildUrl(path: string, query?: QueryParams): string {
  const url = `${API_BASE}${path}`;
  if (!query) return url;
  const search = new URLSearchParams();
  for (const [key, value] of Object.entries(query)) {
    if (value === undefined) continue;
    if (Array.isArray(value)) {
      for (const item of value) if (item) search.append(key, item);
    } else if (value !== "") {
      search.set(key, value);
    }
  }
  const qs = search.toString();
  return qs ? `${url}?${qs}` : url;
}

/** Ids in paths are always escaped: they are opaque strings and not guaranteed URL-safe. */
function seg(value: string): string {
  return encodeURIComponent(value);
}

/**
 * Common prefix of the nine revision-history routes.
 *
 * ⚠️ **kind and the path segment are not the same word**: kind is singular (`agent`), the path
 * is plural (`/agents`). Writing `` `/${kind}s/` `` also happens to give the right result, but
 * that is luck: a future kind whose name is not just "add s" (for example `feishu_app` →
 * `/feishu/apps`) would be silently built wrong, and the symptom is a 404 pointing at "that API
 * does not exist" rather than "the path was built wrong". So list them explicitly.
 */
function revisionBase(kind: RevisionKind, entityId: string): string {
  const prefix: Record<RevisionKind, string> = {
    agent: "/agents",
    skill: "/skills",
    schedule: "/schedules",
  };
  return `${prefix[kind]}/${seg(entityId)}`;
}

/**
 * Build the form for skill upload.
 *
 * The field name `file` must match the backend's core.uploadFieldName exactly; a mismatch shows
 * up as "please choose a file to upload", which points at "the user did not choose a file" when
 * the field name is actually wrong.
 *
 * The third argument (the file name) cannot be omitted: the backend uses the **extension** to
 * decide between zip parsing and a bare SKILL.md. Without it the browser fills in "blob", so a
 * .zip package is parsed as markdown, and the error is "missing frontmatter", which does not
 * point at the real cause at all.
 */
function skillUploadForm(file: File): FormData {
  const form = new FormData();
  form.append("file", file, file.name);
  return form;
}

/**
 * Form for attachment upload. The field name matches the backend's core.attachmentFieldName.
 *
 * **The file name cannot be omitted either**: the backend cross-checks the extension against the
 * sniffed type, and the browser's default "blob" has no extension, so it is rejected as "file name
 * has no extension"; that error points at the file name while the user clearly chose shot.png.
 *
 * Deliberately written separately from skillUploadForm rather than sharing a helper: the two
 * routes are two separate contracts, and if one side changes some day (for example to support
 * multiple files), a shared helper would change the other side too without anyone noticing.
 */
function attachmentForm(file: File): FormData {
  const form = new FormData();
  form.append("file", file, file.name);
  return form;
}

/**
 * Read the response body.
 *
 * 204 and empty bodies return undefined; if it is not valid JSON, return the raw text. On gateway
 * or proxy failures the body is HTML, and keeping the original text is better than throwing
 * another SyntaxError here that hides the real situation.
 */
async function readBody(res: Response): Promise<unknown> {
  if (res.status === 204) return undefined;
  const text = await res.text();
  if (!text) return undefined;
  try {
    return JSON.parse(text);
  } catch {
    return text;
  }
}

/**
 * Turn an error response into an ApiError.
 *
 * **The logid can only be read here**: it comes from a response header, and this function is the
 * only place that has both `res` and the error to throw. Every later layer (classifyError /
 * buildFeedbackPayload) only has an Error object; the response is long gone.
 *
 * ⚠️ **Do not give it a fallback value.** When the header is missing (cross-origin without
 * expose, or the response was not sent by amad at all, like gateway HTML) it is an empty string,
 * and downstream uses that to decide "this time there is no server logid". A fake id would make
 * the person debugging search with it and find nothing, which wastes more time than having none.
 */
function errorFrom(res: Response, payload: unknown, method = "", route = ""): ApiError {
  // Cross-origin without expose, get() returns null, so fall back to an empty string, not a fake value.
  const logId = readLogID(res);
  if (payload && typeof payload === "object") {
    const body = payload as { message?: unknown; code?: unknown };
    if (typeof body.message === "string" && body.message !== "") {
      return new ApiError(
        body.message,
        typeof body.code === "string" ? body.code : "unknown",
        res.status,
        logId,
        method,
        route,
      );
    }
  }
  // Getting here usually means a gateway/proxy returned HTML (see readBody). Such a response
  // **may or may not have a logid**: the request may have reached the gateway but not amad, and
  // withLogID reuses a header passed in from upstream. If present, include it (it can still be
  // found on the gateway side); if not, leave it empty. This is an in-between state and should
  // not pretend to be either end.
  return new ApiError(
    `请求失败（HTTP ${res.status}）`,
    "unknown",
    res.status,
    logId,
    method,
    route,
  );
}

/** Get the server logid from the response header; empty string if not available. See LOGID_HEADER and ApiError.logId. */
function readLogID(res: Response): string {
  try {
    return res.headers.get(LOGID_HEADER)?.trim() ?? "";
  } catch {
    // headers should always be there in theory, but this function sits on the path of every
    // failed request, so a fallback is worth it: failing to read the logid must never hide the
    // real error.
    return "";
  }
}

/**
 * All shared logic for one request: build the URL, send credentials, parse the body, and turn
 * the error envelope into an ApiError.
 *
 * All 34 methods go through here, so "how errors are reported" has one implementation.
 *
 * Identity is sent automatically via cookie (see CREDENTIALS); no identity header is set here.
 */
async function request<T>(
  method: string,
  path: string,
  options: RequestOptions = {},
): Promise<T> {
  const headers: Record<string, string> = {};
  const init: RequestInit = { method, headers, credentials: CREDENTIALS };
  if (options.body !== undefined) {
    if (options.body instanceof FormData) {
      // No Content-Type: only the browser can compute the boundary (see RequestOptions.body).
      init.body = options.body;
    } else {
      headers["Content-Type"] = "application/json";
      init.body = JSON.stringify(options.body);
    }
  }

  let res: Response;
  try {
    res = await fetch(buildUrl(path, options.query), init);
  } catch {
    // fetch only rejects on network-level failure, with the native message "Failed to fetch";
    // pages show it to the user directly, so replace it with a Chinese sentence that says what
    // to do.
    //
    // ⚠️ **There is no logid on this path, structurally** (so the fourth argument is empty): the
    // request never reached the server, the `withLogID` middleware never ran, no id was generated,
    // and the server logs have no line for this request. `httpStatus: 0` is how the data model
    // expresses that. **Do not make up an id here**; see the ApiError.logId comment.
    // ⚠️ The fourth argument (logid) stays empty: the request never reached the server, and no id
    // was generated. **But method/route are known to us when we send the request**, regardless of
    // whether the server responded, so they still have values on this path. In the network_error
    // case they are exactly the only thing that tells "which API we were calling".
    throw new ApiError(
      "连不上服务端，请确认后端已启动",
      "network_error",
      0,
      "",
      method,
      routePatternForReport(path),
    );
  }

  const payload = await readBody(res);
  if (!res.ok) {
    // Route pattern: same processing as recordFailedCall (the order must not be reversed; see that function).
    const apiRoute = routePatternForReport(path);
    const err = errorFrom(res, payload, method, apiRoute);
    // Record into the "recent failed API calls" ring buffer, for level-2 issue reports (see
    // lib/feedback.ts).
    //
    // **This is the only right place**: all 34 methods go through request(), so recording once
    // covers everything, and no caller can bypass it. Recording separately in each method is
    // wrong: missing one gives no error, and the missed one may be exactly the API with the most
    // problems.
    //
    // The buffer **fills at both levels, but only level 2 reports it** (level 1 never reads it).
    //
    // With the logid included, this trail can save the `network_error` case: before the backend
    // went down, the failures that **did reach** the server each carry a real logid, which can
    // pin down when the backend went down; the network_error itself has no logid.
    recordFailedCall(method, path, err.status, err.code, err.logId);
    // A session can expire while in use (7 days by default, or logged out elsewhere). Then
    // **every** request gets 401, and if each just showed its own error, the user would see a
    // screen full of "please log in first" with nowhere to go. So notify the outside once at this
    // single entry/exit point, and AuthProvider switches the whole app back to the login page.
    if (err.code === UNAUTHENTICATED_CODE) notifyUnauthenticated();
    throw err;
  }
  return payload as T;
}

/** Requests that do not care about the response body (DELETE returns 204, Feishu disconnect returns 200 + {id}; neither should be parsed). */
async function requestVoid(
  method: string,
  path: string,
  options: RequestOptions = {},
): Promise<void> {
  await request<unknown>(method, path, options);
}

/** Turn "resource does not exist" into null and rethrow other errors. The check is the code, not the status code; see the file header. */
async function nullable<T>(p: Promise<T>): Promise<T | null> {
  try {
    return await p;
  } catch (e) {
    if (e instanceof ApiError && NOT_FOUND_CODES.has(e.code)) return null;
    throw e;
  }
}

/**
 * Recognize one SSE data frame as a StreamFrame.
 *
 * Ark tells the three kinds of frame apart by fields on the envelope: incremental preview frames
 * carry `kind` (event_start / event_delta), the rest are complete session events. **Anything not
 * recognized is treated as a complete event**: recognizing one too many at worst shows an "unknown
 * type" entry on the timeline, while missing one would make a whole message not show.
 */
function parseFrame(raw: Record<string, unknown>): StreamFrame {
  const kind = typeof raw.kind === "string" ? raw.kind : undefined;

  if (kind === "event_start") {
    const type = raw.type === "agent.thinking" ? "agent.thinking" : "agent.message";
    return {
      kind: "event_start",
      id: String(raw.id ?? ""),
      type,
      session_thread_id:
        typeof raw.session_thread_id === "string" ? raw.session_thread_id : null,
    };
  }

  if (kind === "event_delta") {
    return { kind: "event_delta", id: String(raw.id ?? ""), text: String(raw.text ?? "") };
  }

  return { kind: "event", event: raw as unknown as RunEvent };
}

export class HttpApiClient implements ApiClient {
  // -- Login ------------------------------------------------------------------

  /**
   * The current logged-in user. **Returns null instead of throwing when not logged in**: the app
   * uses it at startup to detect login state, and "not logged in yet" is the most common normal
   * case, which should not go down the error path.
   *
   * The check is the code, not the status code, the same as the nullable getters (see the file
   * header).
   */
  async getCurrentUser(): Promise<AuthUser | null> {
    try {
      const res = await request<{ user: AuthUser }>("GET", "/auth/me");
      return res.user;
    } catch (e) {
      if (e instanceof ApiError && e.code === UNAUTHENTICATED_CODE) return null;
      throw e;
    }
  }

  getAuthProviders(): Promise<AuthProviders> {
    return request<AuthProviders>("GET", "/auth/providers");
  }

  /**
   * Email + password login. The backend sets the cookie in the response; this only gets the
   * identity back.
   *
   * A wrong password returns 401 `invalid_credentials`, **not** `unauthenticated`, so it does not
   * trigger notifyUnauthenticated (which would "switch back" a user who is still on the login
   * page to the login page).
   */
  async loginWithPassword(email: string, password: string): Promise<AuthUser> {
    const res = await request<{ user: AuthUser }>("POST", "/auth/login", {
      body: { email, password },
    });
    return res.user;
  }

  startGitHubLogin(): Promise<GitHubDeviceStart> {
    return request<GitHubDeviceStart>("POST", "/auth/github/device");
  }

  pollGitHubLogin(flowId: string): Promise<GitHubPollResult> {
    return request<GitHubPollResult>("POST", "/auth/github/poll", { body: { flowId } });
  }

  /** Sign up and log in directly. See ApiClient.signup. */
  async signup(input: { email: string; password: string; name?: string }): Promise<AuthUser> {
    const res = await request<{ user: AuthUser }>("POST", "/auth/signup", { body: input });
    return res.user;
  }

  /** Log out. The credential is in an HttpOnly cookie; the backend sends the instruction to clear it, and the frontend cannot and need not touch it. */
  logout(): Promise<void> {
    return requestVoid("POST", "/auth/logout");
  }

  // -- workspace ------------------------------------------------------------

  listWorkspaces(): Promise<Workspace[]> {
    return request<Workspace[]>("GET", "/workspaces");
  }

  /** The key goes in a query parameter, not a path segment: a path segment would conflict with /workspaces/{id}/agents (see the comment on the backend's Register). */
  getWorkspaceByKey(key: string): Promise<Workspace | null> {
    return nullable(request<Workspace>("GET", "/workspace-by-key", { query: { key } }));
  }

  createWorkspace(input: { name: string; key: string; description?: string }): Promise<Workspace> {
    return request<Workspace>("POST", "/workspaces", { body: input });
  }

  /**
   * Update a workspace.
   *
   * Send the patch as the body directly, **without filling in undefined fields**: the backend
   * takes these three fields as pointers, and a missing description key means "clear it".
   * JSON.stringify drops keys whose value is undefined, which matches Object.assign's behavior in
   * LocalApiClient (description: undefined also clears it).
   */
  updateWorkspace(
    id: string,
    patch: Partial<Pick<Workspace, "name" | "key" | "description">>,
  ): Promise<Workspace> {
    return request<Workspace>("PATCH", `/workspaces/${seg(id)}`, { body: patch });
  }

  deleteWorkspace(id: string): Promise<void> {
    return requestVoid("DELETE", `/workspaces/${seg(id)}`);
  }

  isWorkspaceKeyTaken(key: string, exceptId?: string): Promise<boolean> {
    return request<{ taken: boolean }>("GET", "/workspace-key-taken", {
      query: { key, exceptId },
    }).then((r) => r.taken);
  }

  // -- members --------------------------------------------------------------

  addMember(
    workspaceId: string,
    input: { name: string; email: string; role: WorkspaceRole; avatarUrl?: string },
  ): Promise<WorkspaceMember> {
    return request<WorkspaceMember>("POST", `/workspaces/${seg(workspaceId)}/members`, {
      body: input,
    });
  }

  updateMemberRole(
    workspaceId: string,
    memberId: string,
    role: WorkspaceRole,
  ): Promise<WorkspaceMember> {
    return request<WorkspaceMember>(
      "PATCH",
      `/workspaces/${seg(workspaceId)}/members/${seg(memberId)}`,
      { body: { role } },
    );
  }

  removeMember(workspaceId: string, memberId: string): Promise<void> {
    return requestVoid("DELETE", `/workspaces/${seg(workspaceId)}/members/${seg(memberId)}`);
  }

  // -- agents ---------------------------------------------------------------

  listAgents(workspaceId: string): Promise<Agent[]> {
    return request<Agent[]>("GET", `/workspaces/${seg(workspaceId)}/agents`);
  }

  getAgent(id: string): Promise<Agent | null> {
    return nullable(request<Agent>("GET", `/agents/${seg(id)}`));
  }

  createAgent(workspaceId: string, draft: AgentDraft): Promise<Agent> {
    return request<Agent>("POST", `/workspaces/${seg(workspaceId)}/agents`, { body: draft });
  }

  updateAgent(id: string, patch: Partial<AgentDraft>): Promise<Agent> {
    return request<Agent>("PATCH", `/agents/${seg(id)}`, { body: patch });
  }

  deleteAgent(id: string): Promise<void> {
    return requestVoid("DELETE", `/agents/${seg(id)}`);
  }

  /**
   * Get the result of the latest sync.
   *
   * ⚠️ When "there is no record", the server returns **200 + `{status:"none"}`**, not 404; so do
   * not wrap it in nullable(), read it as a normal response.
   */
  getAgentSyncTask(id: string): Promise<AgentSyncTask> {
    return request<AgentSyncTask>("GET", `/agents/${seg(id)}/sync-task`);
  }

  retryAgentSync(id: string): Promise<AgentSyncTask> {
    return request<AgentSyncTask>("POST", `/agents/${seg(id)}/sync-task`);
  }

  /**
   * Update integration injection. **It has its own route**, because it has its own permission
   * level (manage).
   *
   * The server rejects changing this field via `PATCH /agents/{id}` (rejected only if the value
   * changed; sending it back unchanged is allowed), so it cannot go through updateAgent.
   */
  setAgentIntegrations(id: string, integrationIds: string[]): Promise<Agent> {
    return request<Agent>("PUT", `/agents/${seg(id)}/integrations`, {
      body: { integrationIds },
    });
  }

  getMyRole(workspaceId: string): Promise<WorkspaceRoleInfo> {
    return request<WorkspaceRoleInfo>("GET", `/workspaces/${seg(workspaceId)}/my-role`);
  }

  // -- Workspace-level integration bindings (**workspace-level**, not the same as the account-level /integrations group above) --
  //
  // Mounting the path under workspace is deliberate: that is what lets authz.Guard's pattern
  // cover it (the account-level paths have no workspaceId, so cross-workspace access cannot even
  // be expressed there).

  // -- Host executors ----------------------------------------------------------

  listHosts(): Promise<Host[]> {
    return request<Host[]>("GET", `/hosts`);
  }

  deleteHost(hostId: string): Promise<void> {
    return requestVoid("DELETE", `/hosts/${seg(hostId)}`);
  }

  listWorkspaceHosts(workspaceId: string): Promise<WorkspaceHost[]> {
    return request<WorkspaceHost[]>("GET", `/workspaces/${seg(workspaceId)}/hosts`);
  }

  bindWorkspaceHost(workspaceId: string, hostId: string): Promise<void> {
    // 201 with no body: requestVoid is safe for 2xx empty bodies (see point 3 in the file header).
    return requestVoid("POST", `/workspaces/${seg(workspaceId)}/hosts`, { body: { hostId } });
  }

  unbindWorkspaceHost(workspaceId: string, hostId: string): Promise<void> {
    return requestVoid(
      "DELETE",
      `/workspaces/${seg(workspaceId)}/hosts/${seg(hostId)}`,
    );
  }

  // -- skills ---------------------------------------------------------------

  listSkills(workspaceId: string): Promise<Skill[]> {
    return request<Skill[]>("GET", `/workspaces/${seg(workspaceId)}/skills`);
  }

  getSkill(id: string): Promise<Skill | null> {
    return nullable(request<Skill>("GET", `/skills/${seg(id)}`));
  }

  createSkill(workspaceId: string, draft: SkillDraft): Promise<Skill> {
    return request<Skill>("POST", `/workspaces/${seg(workspaceId)}/skills`, { body: draft });
  }

  updateSkill(id: string, patch: Partial<SkillDraft>): Promise<Skill> {
    return request<Skill>("PATCH", `/skills/${seg(id)}`, { body: patch });
  }

  deleteSkill(id: string): Promise<void> {
    return requestVoid("DELETE", `/skills/${seg(id)}`);
  }

  /**
   * Read the content of one attachment.
   *
   * The path goes in a **query parameter**, not a path segment: attachment paths contain `/`, and
   * a path segment would need a wildcard route (no precedent in this repo), while the backend
   * authz Guard resolves resource ownership from a single path parameter. The precedent is
   * `readRunSandboxFile`, which has exactly the same shape.
   *
   * `buildUrl` encodes query values, so `/` in the path needs no special handling.
   */
  readSkillFile(skillId: string, path: string): Promise<SkillFileContent> {
    return request<SkillFileContent>("GET", `/skills/${seg(skillId)}/file`, {
      query: { path },
    });
  }

  /**
   * Update the content of one attachment.
   *
   * **Both path and content are in the body** (deliberately different from the `?path=` of the
   * read): a write has a body anyway, and putting the path in it is clearer than "half query,
   * half body".
   */
  updateSkillFile(skillId: string, path: string, content: string): Promise<Skill> {
    return request<Skill>("PUT", `/skills/${seg(skillId)}/file`, {
      body: { path, content },
    });
  }

  /** Parse a skill package without saving. Uses ?dryRun=1, the same route as create. */
  parseSkillPackage(workspaceId: string, file: File): Promise<SkillPackage> {
    return request<SkillPackage>("POST", `/workspaces/${seg(workspaceId)}/skills/upload`, {
      query: { dryRun: "1" },
      body: skillUploadForm(file),
    });
  }

  createSkillFromUpload(workspaceId: string, file: File): Promise<Skill> {
    return request<Skill>("POST", `/workspaces/${seg(workspaceId)}/skills/upload`, {
      body: skillUploadForm(file),
    });
  }

  /**
   * Fork a skill into a custom copy in this workspace.
   *
   * ⚠️ **The source id goes in the body, not the path**; this route shape is deliberate on the
   * server: a form like `/skills/{id}/fork` would make the authz layer resolve the workspace from
   * the id, and built-in skills cannot be resolved (they belong to no workspace), so forking a
   * built-in skill would always be 403. See server/core/README.md.
   */
  forkSkill(workspaceId: string, sourceId: string): Promise<Skill> {
    return request<Skill>("POST", `/workspaces/${seg(workspaceId)}/skills/fork`, {
      body: { sourceId },
    });
  }

  // -- issues ----------------------------------------------------------------

  listIssues(workspaceId: string, status?: IssueStatus): Promise<Issue[]> {
    return request<Issue[]>("GET", `/workspaces/${seg(workspaceId)}/issues`, {
      // Without a status, do not send the parameter: an empty string would be rejected by the backend as "unrecognized status".
      query: status ? { status } : undefined,
    });
  }
  getIssue(id: string): Promise<Issue | null> {
    return nullable(request<Issue>("GET", `/issues/${seg(id)}`));
  }
  createIssue(workspaceId: string, draft: IssueDraft): Promise<Issue> {
    return request<Issue>("POST", `/workspaces/${seg(workspaceId)}/issues`, { body: draft });
  }
  updateIssue(id: string, draft: IssueDraft): Promise<Issue> {
    return request<Issue>("PATCH", `/issues/${seg(id)}`, { body: draft });
  }
  deleteIssue(id: string): Promise<void> {
    return requestVoid("DELETE", `/issues/${seg(id)}`);
  }

  listIssueComments(issueId: string): Promise<IssueComment[]> {
    return request<IssueComment[]>("GET", `/issues/${seg(issueId)}/comments`);
  }
  createIssueComment(issueId: string, body: string): Promise<IssueComment> {
    return request<IssueComment>("POST", `/issues/${seg(issueId)}/comments`, {
      body: { body },
    });
  }
  updateIssueComment(id: string, body: string): Promise<IssueComment> {
    return request<IssueComment>("PATCH", `/issue-comments/${seg(id)}`, { body: { body } });
  }
  deleteIssueComment(id: string): Promise<void> {
    return requestVoid("DELETE", `/issue-comments/${seg(id)}`);
  }

  listIssueAttachments(issueId: string): Promise<IssueAttachment[]> {
    return request<IssueAttachment[]>("GET", `/issues/${seg(issueId)}/attachments`);
  }
  uploadIssueAttachment(issueId: string, file: File): Promise<IssueAttachment> {
    return request<IssueAttachment>("POST", `/issues/${seg(issueId)}/attachments`, {
      body: attachmentForm(file),
    });
  }
  deleteIssueAttachment(id: string): Promise<void> {
    return requestVoid("DELETE", `/issue-attachments/${seg(id)}`);
  }

  // -- Product suggestions (platform-level) ------------------------------------------------------
  //
  // ⚠️ The path is **not under a workspace**: suggestions have no workspace dimension (the only shape difference from issues above).

  listSuggestions(params?: {
    status?: SuggestionStatus;
    author?: string;
    limit?: number;
    offset?: number;
  }): Promise<SuggestionPage> {
    const query: Record<string, string> = {};
    // Never send empty values: an empty status string would be rejected by the backend as "unrecognized status".
    if (params?.status) query.status = params.status;
    if (params?.author) query.author = params.author;
    if (params?.limit) query.limit = String(params.limit);
    if (params?.offset) query.offset = String(params.offset);
    return request<SuggestionPage>("GET", "/suggestions", {
      query: Object.keys(query).length > 0 ? query : undefined,
    });
  }
  getSuggestion(id: string): Promise<Suggestion | null> {
    return nullable(request<Suggestion>("GET", `/suggestions/${seg(id)}`));
  }
  createSuggestion(draft: SuggestionDraft): Promise<Suggestion> {
    return request<Suggestion>("POST", "/suggestions", { body: draft });
  }
  updateSuggestion(id: string, draft: SuggestionDraft): Promise<Suggestion> {
    return request<Suggestion>("PATCH", `/suggestions/${seg(id)}`, { body: draft });
  }
  deleteSuggestion(id: string): Promise<void> {
    return requestVoid("DELETE", `/suggestions/${seg(id)}`);
  }

  listSuggestionReplies(suggestionId: string): Promise<SuggestionReply[]> {
    return request<SuggestionReply[]>("GET", `/suggestions/${seg(suggestionId)}/replies`);
  }
  createSuggestionReply(suggestionId: string, body: string): Promise<SuggestionReply> {
    return request<SuggestionReply>("POST", `/suggestions/${seg(suggestionId)}/replies`, {
      body: { body },
    });
  }
  updateSuggestionReply(id: string, body: string): Promise<SuggestionReply> {
    return request<SuggestionReply>("PATCH", `/suggestion-replies/${seg(id)}`, {
      body: { body },
    });
  }
  deleteSuggestionReply(id: string): Promise<void> {
    return requestVoid("DELETE", `/suggestion-replies/${seg(id)}`);
  }

  listSuggestionAttachments(suggestionId: string): Promise<SuggestionAttachment[]> {
    return request<SuggestionAttachment[]>(
      "GET",
      `/suggestions/${seg(suggestionId)}/attachments`,
    );
  }
  uploadSuggestionAttachment(suggestionId: string, file: File): Promise<SuggestionAttachment> {
    return request<SuggestionAttachment>(
      "POST",
      `/suggestions/${seg(suggestionId)}/attachments`,
      { body: attachmentForm(file) },
    );
  }
  deleteSuggestionAttachment(id: string): Promise<void> {
    return requestVoid("DELETE", `/suggestion-attachments/${seg(id)}`);
  }

  // -- feishu ---------------------------------------------------------------

  /** The list API returns {apps: [...]}, one level deeper than a bare array; unwrap it here so pages still get an array. */
  async listFeishuApps(workspaceId: string): Promise<FeishuApp[]> {
    const body = await request<{ apps: FeishuApp[] | null }>(
      "GET",
      `/workspaces/${seg(workspaceId)}/feishu/apps`,
    );
    return body.apps ?? [];
  }

  getFeishuApp(id: string): Promise<FeishuApp | null> {
    return nullable(request<FeishuApp>("GET", `/feishu/apps/${seg(id)}`));
  }

  /** region / appId are not sent: the backend corrects the region automatically from the tenant_brand returned by poll.
   *
   * requestGroupMessage must be sent: it decides whether the authorization page also requests
   * im:message.group_msg. **This switch only matters at this moment**: it is encoded into the
   * addons parameter of the authorization page URL and fixed once the page opens. This used to
   * hard-code `{}`, so that permission was never requested, and without it Feishu does not push
   * group messages that do not @ the bot at all.
   */
  beginFeishuRegistration(
    workspaceId: string,
    requestGroupMessage?: boolean,
  ): Promise<FeishuRegistration> {
    return request<FeishuRegistration>(
      "POST",
      `/workspaces/${seg(workspaceId)}/feishu/registrations`,
      { body: requestGroupMessage ? { requestGroupMessage: true } : {} },
    );
  }

  pollFeishuRegistration(
    workspaceId: string,
    sessionId: string,
  ): Promise<FeishuRegistrationResult> {
    return request<FeishuRegistrationResult>(
      "GET",
      `/workspaces/${seg(workspaceId)}/feishu/registrations/${seg(sessionId)}`,
    );
  }

  refreshFeishuReadiness(appId: string): Promise<FeishuApp> {
    return request<FeishuApp>("POST", `/feishu/apps/${seg(appId)}/readiness/refresh`);
  }

  requestFeishuAdminAuthorization(appId: string): Promise<FeishuApp> {
    return request<FeishuApp>("POST", `/feishu/apps/${seg(appId)}/admin-authorization`);
  }

  setFeishuTriggerMode(appId: string, mode: FeishuTriggerMode): Promise<FeishuApp> {
    return request<FeishuApp>("PATCH", `/feishu/apps/${seg(appId)}/trigger-mode`, {
      body: { triggerMode: mode },
    });
  }

  deleteFeishuApp(id: string): Promise<void> {
    return requestVoid("DELETE", `/feishu/apps/${seg(id)}`);
  }

  // -- Ark credentials --------------------------------------------------------------

  /**
   * Get the credential. **Never null**: when not configured, the backend returns an empty shell
   * with an empty mask.
   *
   * Deliberately not wrapped in nullable: the page uses `cred === null` for "loading" and an empty
   * mask for "not configured"; turning "read failed" into null too would leave it on the skeleton
   * forever.
   */
  getArkCredential(workspaceId: string): Promise<ArkCredential> {
    return request<ArkCredential>("GET", `/workspaces/${seg(workspaceId)}/ark-credential`);
  }

  saveArkCredential(workspaceId: string, draft: ArkCredentialDraft): Promise<ArkCredential> {
    return request<ArkCredential>("PUT", `/workspaces/${seg(workspaceId)}/ark-credential`, {
      body: draft,
    });
  }

  updateArkRegion(workspaceId: string, region: ArkRegion): Promise<ArkCredential> {
    return request<ArkCredential>(
      "PATCH",
      `/workspaces/${seg(workspaceId)}/ark-credential/region`,
      { body: { region } },
    );
  }

  verifyArkCredential(workspaceId: string): Promise<ArkCredential> {
    return request<ArkCredential>("POST", `/workspaces/${seg(workspaceId)}/ark-credential/verify`);
  }

  deleteArkCredential(workspaceId: string): Promise<void> {
    return requestVoid("DELETE", `/workspaces/${seg(workspaceId)}/ark-credential`);
  }

  useGlobalArkCredential(workspaceId: string): Promise<ArkCredential> {
    return request<ArkCredential>(
      "POST",
      `/workspaces/${seg(workspaceId)}/ark-credential/use-global`,
    );
  }

  // -- Account-level Ark credentials --------------------------------------------------------
  //
  // None of the four **send any identity parameter**: the backend takes the user from the session.

  getGlobalArkCredential(): Promise<GlobalArkCredential> {
    // Like getArkCredential, deliberately **not wrapped in nullable**: when not configured the
    // backend returns an empty shell, not a 404; the page uses an empty mask for "not configured"
    // and null for "loading".
    return request<GlobalArkCredential>("GET", "/global-ark-credential");
  }

  saveGlobalArkCredential(draft: ArkCredentialDraft): Promise<GlobalArkCredential> {
    return request<GlobalArkCredential>("PUT", "/global-ark-credential", { body: draft });
  }

  verifyGlobalArkCredential(): Promise<GlobalArkCredential> {
    return request<GlobalArkCredential>("POST", "/global-ark-credential/verify");
  }

  deleteGlobalArkCredential(): Promise<void> {
    return requestVoid("DELETE", "/global-ark-credential");
  }

  // -- Agent-level Ark credentials (the most specific of the three levels) -------------------------------
  //
  // The three-level fallback order is **agent → workspace → account**; more specific wins.
  // ⚠️ "No record" = follow the workspace, so delete means "go back to following".

  /**
   * Read the agent-level credential. **Deliberately not wrapped in nullable**: when not
   * configured the backend returns 200 + an empty shell (custom:false), not 404. A null fallback
   * would leave a failed read on the skeleton forever, for the same reason as
   * getGlobalArkCredential.
   */
  getAgentArkCredential(agentId: string): Promise<AgentArkCredential> {
    return request<AgentArkCredential>("GET", `/agents/${seg(agentId)}/ark-credential`);
  }

  saveAgentArkCredential(
    agentId: string,
    draft: ArkCredentialDraft,
  ): Promise<AgentArkCredential> {
    return request<AgentArkCredential>("PUT", `/agents/${seg(agentId)}/ark-credential`, {
      body: draft,
    });
  }

  updateAgentArkRegion(agentId: string, region: ArkRegion): Promise<AgentArkCredential> {
    return request<AgentArkCredential>(
      "PATCH",
      `/agents/${seg(agentId)}/ark-credential/region`,
      { body: { region } },
    );
  }

  verifyAgentArkCredential(agentId: string): Promise<AgentArkCredential> {
    return request<AgentArkCredential>("POST", `/agents/${seg(agentId)}/ark-credential/verify`);
  }

  deleteAgentArkCredential(agentId: string): Promise<void> {
    return requestVoid("DELETE", `/agents/${seg(agentId)}/ark-credential`);
  }

  // -- Fetch Ark API Keys automatically from a Volcengine account (three read-only calls) -----------------------------
  //
  // ⚠️ **Only masks and ids come back, no plaintext.** After choosing, go through
  // save*ArkCredential with draft.fetchRef, and the server fetches the plaintext itself; the
  // plaintext never passes through the frontend.

  getArkFetchStatus(): Promise<ArkFetchStatus> {
    return request<ArkFetchStatus>("GET", "/ark-fetch/status");
  }

  listVolcAccounts(): Promise<VolcAccount[]> {
    return request<VolcAccount[]>("GET", "/ark-fetch/accounts");
  }

  listVolcArkKeys(accountId: string): Promise<ArkKeyOption[]> {
    return request<ArkKeyOption[]>(
      "GET",
      `/ark-fetch/accounts/${seg(accountId)}/keys`,
    );
  }

  // -- Platform API Keys ----------------------------------------------------------
  //
  // ⚠️ **The opposite direction from the Ark credentials above**: those are us calling Ark; this
  // is others calling us.
  //
  // None of the three **send workspaceId**: keys are account-level, and the backend uses the
  // current user's email (the user comes from context; email appears in neither path nor body, so
  // cross-account access cannot be expressed).
  //
  // The backend still keeps the two equivalent `/workspaces/{id}/api-keys` routes (they were
  // already published in openapi.json; removing them would give 404 to agents written against
  // the old spec), but the frontend always uses the account-level one: the global settings page is
  // outside any workspace, with no workspace to fill in.

  listApiKeys(): Promise<ApiKey[]> {
    return request<ApiKey[]>("GET", "/api-keys");
  }

  /**
   * Create a key. **The plaintext in the return value only appears this once** (the backend only
   * stores sha256).
   *
   * Not wrapped in nullable: a failed create is a real failure, and turning it into null would
   * make the page silently do nothing.
   */
  createApiKey(name: string): Promise<CreatedApiKey> {
    return request<CreatedApiKey>("POST", "/api-keys", { body: { name } });
  }

  revokeApiKey(id: string): Promise<void> {
    return requestVoid("DELETE", `/api-keys/${seg(id)}`);
  }

  /**
   * Look up display names and avatars for a batch of emails. See ApiClient.listUserProfiles for
   * the contract.
   *
   * **POST + body instead of GET + query**: the input is a batch of emails; 20 creators on a page
   * make a URL of six or seven hundred characters, and proxies at each layer have different URL
   * length limits. Over the limit you get a 414 or **silent truncation**, and the latter makes the
   * last few people's avatars mysteriously missing. It really is read-only.
   */
  listUserProfiles(emails: string[]): Promise<Record<string, UserProfile>> {
    return request<Record<string, UserProfile>>("POST", "/user-profiles", {
      body: { emails },
    });
  }

  // -- Integration management (multiple integration providers) -----------------------------------------
  //
  // No workspaceId: integrations are **account-level**, unrelated to workspaces.
  //
  // The provider list comes from listIntegrations and is not hard-coded in the frontend: adding
  // an integration source only changes the backend.

  listIntegrations(workspaceId?: string): Promise<IntegrationState[]> {
    // Deliberately not wrapped in nullable: not connected / not authorized are **normal states**,
    // expressed by the status field, and the backend always returns 200 + an array. Treating it as
    // "may not exist" would add a page branch that is never reached.
    //
    // ⚠️ workspaceId decides which app the Feishu card checks status against; the agent page must
    // pass it. buildUrl drops undefined, so when not passed the URL is exactly the same as before.
    return request<IntegrationState[]>("GET", "/integrations", { query: { workspaceId } });
  }

  getIntegration(provider: string, workspaceId?: string): Promise<IntegrationState> {
    return request<IntegrationState>("GET", `/integrations/${seg(provider)}`, {
      query: { workspaceId },
    });
  }

  async startIntegrationAuthorize(provider: string, workspaceId?: string): Promise<string> {
    const body = await request<{ authorizationUrl: string }>(
      "POST",
      `/integrations/${seg(provider)}/authorize`,
      // ⚠️ Uses the **query** instead of the request body, for two reasons (see
      // server/core/handler.go):
      // ① the polling call is a GET with no body, and the two **must resolve the same app or the
      //    whole flow dies**; the same shape makes it obvious at a glance whether both are wired;
      // ② the backend's h.decode returns 400 when it gets no body, and this used to send none.
      // buildUrl drops undefined, so when not passed the URL is exactly the same as before.
      { query: { workspaceId } },
    );
    return body.authorizationUrl;
  }

  pollIntegrationAuthorization(
    provider: string,
    workspaceId?: string,
  ): Promise<AuthorizationStatus> {
    // **"Still waiting" is 200 + pending:true, not an error code**, so this is not wrapped in
    // nullable and 4xx is not special-cased: it only throws on a real error (for example calling
    // it for an unsupported source).
    //
    // ⚠️ workspaceId must be the same as in the startIntegrationAuthorize call; otherwise the
    // backend looks for the device_code under another app, finds nothing, and returns "expired";
    // see that comment in api.ts.
    return request<AuthorizationStatus>(
      "GET",
      `/integrations/${seg(provider)}/authorization`,
      { query: { workspaceId } },
    );
  }

  revokeIntegration(provider: string): Promise<void> {
    return requestVoid("DELETE", `/integrations/${seg(provider)}`);
  }

  // -- Switch Feishu app -----------------------------------------------------------
  //
  // ⚠️ Neither route **sends identity parameters**, like the ones above: the backend takes the user from the session.

  listIntegrationApps(provider: string, workspaceId?: string): Promise<LarkUserApp[]> {
    return request<LarkUserApp[]>("GET", `/integrations/${seg(provider)}/apps`, {
      query: { workspaceId },
    });
  }

  selectIntegrationApp(provider: string, appId: string): Promise<IntegrationState> {
    // The backend returns **the full state after switching**, not 204, precisely so the card can
    // redraw with the new state in one go (switching to an app never authorized changes the state
    // from ok to unauthorized). So return it as is; do not change it to requestVoid, which would
    // force the caller to make another GET.
    return request<IntegrationState>("PUT", `/integrations/${seg(provider)}/app`, {
      body: { appId },
    });
  }

  // -- Onboarding -------------------------------------------------------------
  //
  // Neither **sends any identity parameter**: the backend takes the user from the session. This
  // is not laziness, it is a security boundary: if an email could be passed here, "read someone
  // else's onboarding state" would become an expressible request.

  getOnboarding(): Promise<OnboardingState> {
    // Deliberately **not wrapped in nullable**: "this person has not seen the tour" is a normal
    // state, expressed by the version field, and the backend always returns 200. Treating it as
    // "may not exist" would turn it into null, and callers use null to mean "still loading"; so new
    // users would stay in loading forever and the tour would never appear, with no error.
    return request<OnboardingState>("GET", "/user-onboarding");
  }

  markOnboarding(action: "complete" | "skip", version: number): Promise<OnboardingState> {
    return request<OnboardingState>("POST", "/user-onboarding", {
      body: { action, version },
    });
  }

  // -- User preferences -------------------------------------------------------------
  //
  // Same as above: neither **sends any identity parameter**; the backend takes the user from the session.

  getUserPreferences(): Promise<UserPreferences> {
    // Like getOnboarding, deliberately **not wrapped in nullable**: "never set" is a normal state,
    // and the backend always returns 200 + the defaults. null would make callers think "still
    // loading".
    return request<UserPreferences>("GET", "/user-preferences");
  }

  saveUserPreferences(prefs: UserPreferences): Promise<UserPreferences> {
    // PUT rather than PATCH: the request body is a complete set of preferences, meaning full replacement.
    return request<UserPreferences>("PUT", "/user-preferences", { body: prefs });
  }

  // -- Issue reports -------------------------------------------------------------

  /**
   * Report one issue. **Always resolves, never rejects.**
   *
   * # Why errors must be swallowed
   *
   * Level 1 is sent automatically in `useAsyncData`'s catch; at that point the page **is already
   * handling an error**. If the report itself threw another one, there are two bad outcomes:
   *
   *   - an unhandled rejection hides the real error scene (the console line for that is the
   *     useful one);
   *   - worse: when the backend route does not exist (an old amad), every error becomes two
   *     errors, and the second is pure noise.
   *
   * Level 2 is the same: the user clicks "report this problem" and gets "report failed", piling
   * more frustration onto a page that already failed. The caller shows the button's receipt on
   * resolve; a failed report is not something the user can deal with.
   *
   * ⚠️ **So this method's return value does not mean "reported successfully"**, only "we tried".
   * To know whether it was received, check `GET /api/feedback` (only the dev team can read it).
   */
  async submitFeedback(payload: FeedbackPayload): Promise<void> {
    try {
      await requestVoid("POST", "/feedback", { body: payload });
    } catch {
      // Deliberately do nothing, not even console.error: reporting hooks into the console.error
      // capture chain (installConsoleCapture in lib/feedback.ts), and logging here would turn
      // "report failed" itself into the next console error to report.
    }
  }

  // -- Schedules -------------------------------------------------------------

  listSchedules(workspaceId: string): Promise<Schedule[]> {
    return request<Schedule[]>("GET", `/workspaces/${seg(workspaceId)}/schedules`);
  }

  getSchedule(id: string): Promise<Schedule | null> {
    return nullable(request<Schedule>("GET", `/schedules/${seg(id)}`));
  }

  createSchedule(workspaceId: string, draft: ScheduleDraft): Promise<Schedule> {
    return request<Schedule>("POST", `/workspaces/${seg(workspaceId)}/schedules`, { body: draft });
  }

  updateSchedule(id: string, draft: ScheduleDraft): Promise<Schedule> {
    return request<Schedule>("PATCH", `/schedules/${seg(id)}`, { body: draft });
  }

  setScheduleStatus(id: string, status: ScheduleStatus): Promise<Schedule> {
    return request<Schedule>("PATCH", `/schedules/${seg(id)}/status`, { body: { status } });
  }

  deleteSchedule(id: string): Promise<void> {
    return requestVoid("DELETE", `/schedules/${seg(id)}`);
  }

  runScheduleNow(id: string): Promise<Schedule> {
    return request<Schedule>("POST", `/schedules/${seg(id)}/run-now`);
  }

  // -- Entity revision history ---------------------------------------------------------
  //
  // The backend has nine routes (three entities × three actions); the prefix is built from kind.
  // ⚠️ **Do not build it as `/revisions?kind=...`**: the backend deliberately has no such single
  // route, because authz.Guard cannot read query parameters (see server/core/handler_revision.go).

  listRevisions(kind: RevisionKind, entityId: string): Promise<RevisionList> {
    return request<RevisionList>("GET", `${revisionBase(kind, entityId)}/revisions`);
  }

  getRevision(kind: RevisionKind, entityId: string, revisionId: string): Promise<Revision> {
    return request<Revision>(
      "GET",
      `${revisionBase(kind, entityId)}/revisions/${seg(revisionId)}`,
    );
  }

  rollbackRevision(
    kind: RevisionKind,
    entityId: string,
    revisionId: string,
  ): Promise<unknown> {
    return request<unknown>(
      "POST",
      `${revisionBase(kind, entityId)}/revisions/${seg(revisionId)}/rollback`,
    );
  }

  /**
   * Feishu groups the bot has seen.
   *
   * The backend wraps it in `{chats: [...]}` (all the feishu APIs return this way), so unwrap one
   * level.
   */
  async listFeishuChats(workspaceId: string): Promise<FeishuChat[]> {
    const res = await request<{ chats: FeishuChat[] }>(
      "GET",
      `/workspaces/${seg(workspaceId)}/feishu/chats`,
    );
    return res.chats ?? [];
  }

  // -- runs -----------------------------------------------------------------

  /**
   * Run history. **All filtering and paging is pushed down to the backend** (done in SQL); do not
   * `.filter()` in the caller.
   *
   * ⚠️ **`limit` / `offset` go through `String()` instead of passing numbers directly**:
   * `buildUrl`'s `QueryParams` only accepts strings and string arrays, and its check for
   * non-array values is `value !== ""`; the number 0 would pass, but the type is wrong as a whole.
   * Same as in listFeedback.
   *
   * Without `limit`, the backend applies **no limit** (all rows by default), exactly the same as
   * before paging was added; this is deliberate, see `RunQuery.limit` in api.ts.
   */
  listRuns(workspaceId: string, query: RunQuery = {}): Promise<Run[]> {
    return request<Run[]>("GET", `/workspaces/${seg(workspaceId)}/runs`, {
      query: {
        agentId: query.agentId,
        status: query.status,
        search: query.search,
        scheduleId: query.scheduleId,
        trigger: query.trigger,
        limit: query.limit ? String(query.limit) : undefined,
        offset: query.offset ? String(query.offset) : undefined,
      },
    });
  }

  getRun(id: string): Promise<Run | null> {
    return nullable(request<Run>("GET", `/runs/${seg(id)}`));
  }

  /** When the run does not exist the backend returns 200 + [], not 404; this method returns an array, not a nullable. */
  listRunEvents(runId: string, query: EventQuery = {}): Promise<RunEvent[]> {
    return request<RunEvent[]>("GET", `/runs/${seg(runId)}/events`, {
      query: { types: query.types, threadId: query.threadId },
    });
  }

  /**
   * Ark console references (three calls). **None wrapped in nullable()**: when the Ark side has
   * not been created yet, the backend returns 200 + empty fields, not 404 (Ark resources are
   * created lazily).
   */
  getAgentArkConsole(agentId: string): Promise<ArkConsoleRef> {
    return request<ArkConsoleRef>("GET", `/agents/${seg(agentId)}/ark-console`);
  }

  getSkillArkConsole(skillId: string): Promise<ArkConsoleRef> {
    return request<ArkConsoleRef>("GET", `/skills/${seg(skillId)}/ark-console`);
  }

  getRunArkConsole(runId: string): Promise<ArkConsoleRef> {
    return request<ArkConsoleRef>("GET", `/runs/${seg(runId)}/ark-console`);
  }

  /**
   * Session container state. **Not wrapped in nullable()**: when no container was ever created
   * the backend returns 200 + `status:"none"`, not 404. Wrapping it would turn "has not used a
   * container yet" into null, and the page could not tell it apart from "read failed".
   */
  getRunSandbox(runId: string): Promise<SandboxState> {
    return request<SandboxState>("GET", `/runs/${seg(runId)}/sandbox`);
  }

  resumeRunSandbox(runId: string): Promise<SandboxState> {
    return request<SandboxState>("POST", `/runs/${seg(runId)}/sandbox/resume`);
  }

  /**
   * List files in the container. Also not wrapped in nullable(): when the container is gone it
   * returns 200 + empty list + state.
   */
  listRunSandboxFiles(runId: string, path?: string): Promise<SandboxListing> {
    return request<SandboxListing>("GET", `/runs/${seg(runId)}/sandbox/files`, {
      query: { path },
    });
  }

  writeRunSandboxTerminal(runId: string, terminalId: string, frame: unknown): Promise<void> {
    return request<void>("POST", `/runs/${seg(runId)}/sandbox/terminal/input`, {
      body: { terminalId, frame },
    });
  }

  issueTerminalTicket(runId: string): Promise<TerminalTicket> {
    // **This one must go through same-origin /api** (with cookie, through authz); it is the only
    // step on the whole WS path that can still authenticate by cookie. See the comment on
    // issueTerminalTicket in api.ts.
    return request<TerminalTicket>("POST", `/runs/${seg(runId)}/sandbox/terminal/ticket`);
  }

  readRunSandboxFile(runId: string, path: string): Promise<SandboxFileContent> {
    return request<SandboxFileContent>("GET", `/runs/${seg(runId)}/sandbox/file`, {
      query: { path },
    });
  }

  runSandboxShell(runId: string, command: string): Promise<SandboxShellResult> {
    return request<SandboxShellResult>("POST", `/runs/${seg(runId)}/sandbox/shell`, {
      body: { command },
    });
  }

  /**
   * Usage aggregates. **Not wrapped in nullable()**: when the workspace does not exist the backend
   * returns 200 + an all-zero summary, and the return type is not nullable. Wrapping it would
   * leave a failed read on the skeleton forever (same as getArkCredential).
   */
  getWorkspaceUsage(workspaceId: string, query: UsageQuery = {}): Promise<UsageSummary> {
    return request<UsageSummary>("GET", `/workspaces/${seg(workspaceId)}/usage`, {
      query: { since: query.since, until: query.until },
    });
  }

  // -- Chat ------------------------------------------------------------------

  createConversation(workspaceId: string, agentId: string): Promise<Run> {
    return request<Run>("POST", `/workspaces/${seg(workspaceId)}/conversations`, {
      body: { agentId },
    });
  }

  /**
   * Maps to Ark POST /sessions/{id}/events.
   *
   * After accepting, the backend returns the "list of accepted events" (that is Ark's design),
   * one per event in the request. Only one is sent here, so return the first.
   */
  async sendMessage(runId: string, text: string): Promise<RunEvent> {
    const accepted = await request<RunEvent[]>("POST", `/runs/${seg(runId)}/events`, {
      body: { events: [{ type: "user.message", content: [{ type: "text", text }] }] },
    });
    const first = accepted[0];
    if (!first) throw new Error("发送后没有收到回执事件");
    return first;
  }

  interruptRun(runId: string): Promise<void> {
    return requestVoid("POST", `/runs/${seg(runId)}/events`, {
      body: { events: [{ type: "user.interrupt" }] },
    });
  }

  /**
   * Redirect: one call does "interrupt + send a new message instead".
   *
   * **Deliberately its own route, not two /events calls here.** The network round trip between
   * the two steps is exactly another rejection window; see the notes on ApiClient.redirectRun and
   * RedirectRun in server/core/chat.go. The response shape is the same as sendMessage (a bare
   * array), so taking the first works the same way.
   */
  async redirectRun(runId: string, text: string): Promise<RunEvent> {
    const accepted = await request<RunEvent[]>("POST", `/runs/${seg(runId)}/redirect`, {
      body: { text },
    });
    const first = accepted[0];
    if (!first) throw new Error("转向后没有收到回执事件");
    return first;
  }

  /** List this session's queued messages. The server already returns them in queue order; the frontend does not sort again. */
  async listPendingMessages(runId: string): Promise<PendingMessage[]> {
    return request<PendingMessage[]>("GET", `/runs/${seg(runId)}/pending`);
  }

  /**
   * Add to the end of the queue and return the whole queue afterwards.
   *
   * When full, the server returns 422 + code=precondition_failed, and this **rethrows it as is**:
   * the caller relies on that code to give the original text back to the user; swallowing it would
   * silently lose the message.
   */
  async enqueuePendingMessage(runId: string, text: string): Promise<PendingMessage[]> {
    return request<PendingMessage[]>("POST", `/runs/${seg(runId)}/pending`, {
      body: { text },
    });
  }

  /** Cancel one queued message. The server returns 204 and is idempotent; cancelling one that is already gone is not an error. */
  async cancelPendingMessage(runId: string, messageId: string): Promise<void> {
    await requestVoid("DELETE", `/runs/${seg(runId)}/pending/${seg(messageId)}`);
  }

  /**
   * SSE subscription.
   *
   * Uses native EventSource rather than fetch + ReadableStream: the browser already handles
   * automatic reconnect and ignoring `:` comment lines (Ark uses them for heartbeats); doing it
   * ourselves would only be worse.
   *
   * **Identity is via cookie, not the query.** EventSource cannot send custom headers, so the
   * identity used to be stuffed into `?actor=`; with cookies this path gets authentication for
   * free: the browser sends it on same-origin requests, and the credential never appears in the
   * URL (which would land in browser history and server access logs).
   *
   * `event_deltas` is an opt-in whitelist: without it, Ark sends no delta frames, and replies pop
   * out in one piece instead of character by character. Ark currently only supports
   * agent.message / agent.thinking.
   */
  subscribeRunEvents(
    runId: string,
    onFrame: (frame: StreamFrame) => void,
    onOpen?: () => void,
    onPermanentError?: () => void,
  ): () => void {
    const url = buildUrl(`/runs/${seg(runId)}/events/stream`, {
      event_deltas: ["agent.message", "agent.thinking"],
    });
    const source = new EventSource(url, { withCredentials: true });

    // Only count as subscribed once the connection is really established (response headers
    // arrived). Callers use this to decide when they can send messages: constructing an
    // EventSource is synchronous, and at that moment the request may not have gone out yet.
    source.onopen = () => onOpen?.();

    source.onmessage = (msg: MessageEvent<string>) => {
      // When the Ark stream ends it sends `data: [DONE]`, which is not JSON; filter it before parsing.
      if (msg.data === "[DONE]") {
        onFrame({ kind: "done" });
        source.close();
        return;
      }
      try {
        onFrame(parseFrame(JSON.parse(msg.data) as Record<string, unknown>));
      } catch {
        // If one frame is bad, drop that frame; do not tear down the whole stream. The final event will correct the state.
      }
    };

    // **Do not close** in onerror: EventSource reconnects by itself, and closing gives that up.
    // Ark does not support history replay; events during reconnect must be filled in by the caller
    // fetching listRunEvents again.
    //
    // But "permanent failure" and "temporary disconnect" must be reported to the caller
    // separately, and the check is readyState:
    //
    //   readyState === CLOSED(2)     the browser **has given up** and will not reconnect
    //   readyState === CONNECTING(0) the browser is reconnecting by itself; nothing to do
    //
    // This was not inferred from docs; it was measured on real Chromium (see that section in
    // core/README.md): when the response is 404 / not text/event-stream, the spec requires the UA
    // to "fail the connection"; measured, the server was hit only once and readyState=2 in
    // onerror. When a connection drops mid-way after connecting, measured readyState=0 and the
    // browser reconnects by itself with a 3-second backoff.
    //
    // **So "endless reconnects" cannot be the browser's doing**; it can only be the app layer
    // creating a new EventSource. After onPermanentError the caller must stop and not reattach.
    source.onerror = () => {
      if (source.readyState === EventSource.CLOSED) onPermanentError?.();
    };

    return () => source.close();
  }

  // -- e2e helpers --------------------------------------------------------------

  /** Restore seed data. The backend only mounts this route when AMA_DEV_ROUTES=1; it does not exist in production. */
  reset(): Promise<void> {
    return requestVoid("POST", "/_debug/reset");
  }

  clear(): Promise<void> {
    return requestVoid("POST", "/_debug/clear");
  }

  /**
   * No server equivalent; throws directly.
   *
   * In mock, this hook resets the device-code session's poll counter; in the real flow the
   * "authorized" state can only come from the user clicking approve in Feishu, and the backend
   * does not have, and should not have, an API that conjures an authorization result. e2e cases
   * that need it must run in local mode (see playwright.config.ts).
   */
  completeFeishuAuth(): never {
    throw new Error(
      "completeFeishuAuth() 只有 LocalApiClient 有：真实后端的设备码授权必须由用户在飞书点同意，" +
        "没有等价接口。需要它的测试请用 VITE_API_MODE=local 跑。",
    );
  }
}

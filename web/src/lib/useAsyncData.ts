/**
 * Shared hook for loading async data.
 *
 * # Why it exists
 *
 * Before it, 11 pages each wrote this shape on their own:
 *
 * ```ts
 * const load = useCallback(async () => {
 *   setData(await api.listXxx(workspace.id));   // bare await, no try/catch
 * }, [workspace.id]);
 * useEffect(() => { void load(); }, [load]);
 * if (data === null) return <ListSkeleton />;   // loading = data is still null
 * ```
 *
 * This shape has a failure with **zero errors**: once `await` rejects, `setData` never runs,
 * `data` stays `null` forever, and the page **stays on the skeleton forever**: the user sees a
 * blank page,
 * with no hint in the UI, only an unhandled rejection in the console. And `void load()`
 * even swallows how visible that unhandled rejection would be.
 *
 * It stayed hidden for a long time because these endpoints never fail in local development and
 * mocks. After RBAC shipped,
 * non-members / insufficient roles all get 403, and the 403 goes right down this path with no catch.
 *
 * # Why a hook rather than letting each page write its own try/catch
 *
 * "Forgetting catch" is a mistake **that gets repeated**: when adding a new page, code missing the
 * catch is
 * green in typecheck, build and e2e; it only shows when the failure branch really runs.
 * As a hook, the page gets `{data, error, ...}`; **the error state is part of the return value**,
 * and to ignore it you must first see it.
 *
 * # Things deliberately not done
 *
 * **No caching, no dedupe, no polling.** This only solves one thing: "do not stay on the skeleton
 * on failure".
 * Adding those would make it half of react-query, which this repo does not need: page data
 * dependencies are shallow,
 * and most pages call `reload()` themselves after writes.
 */

import { useCallback, useEffect, useState } from "react";
import { ACCESS_DENIED_CODE, ApiError } from "@/data/http";

/** Failure category. **403 must be kept apart from other errors**, because what the user should do is completely different. */
export type AsyncErrorKind = "denied" | "other";

export interface AsyncError {
  kind: AsyncErrorKind;
  /** One sentence for the user. The backend's error message is a Chinese sentence; use it directly. */
  message: string;
  /**
   * HTTP status code. **For diagnosis, not shown to users.**
   *
   * `kind` has only two levels (denied / other), enough to decide "show a retry button or not",
   * but not enough to
   * diagnose: 500 and a network drop are both `other`, yet they are two completely different
   * failures. So
   * `ApiError.status` is carried out as is for problem feedback (see lib/feedback.ts).
   *
   * **0 when it is not an ApiError** (`ApiError` itself also uses 0 when fetch fails, with the same
   * meaning:
   * "no HTTP response was received"). 0 rather than undefined keeps the set of reported fields
   * fixed;
   * an optional field would make "not captured" and "captured 0" look the same in the database.
   */
  status: number;
  /**
   * Backend error code (`access_denied` / `not_found` / `network_error` …).
   *
   * Better for diagnosis than status: a 403 may be `access_denied` or something else, while code is
   * a stable identifier the backend
   * gives explicitly. **Empty string when it is not an ApiError.**
   */
  code: string;
  /**
   * The server's logid for this request (response header `X-Request-Id`). **Empty string if not
   * available.**
   *
   * Carried out for second-level feedback (see lib/feedback.ts): with it, a feedback entry can go
   * straight to
   * all log lines of that one request, instead of guessing by "roughly what time + which endpoint".
   *
   * ⚠️ **Under `network_error` it is always empty, and that is not a defect**: the request never
   * reached the server,
   * the middleware that generates the logid never ran, and the request does not exist in server
   * logs. Empty is the only honest value;
   * it must never fall back to a placeholder; see `ApiError.logId`.
   *
   * Empty string rather than undefined, for the same reason status uses 0: keep the field set fixed,
   * so "not captured" and "captured empty" do not become two shapes downstream.
   */
  logId: string;
  /**
   * The endpoint that failed: method + **route pattern with ids removed**. Empty string when it is
   * not an ApiError.
   *
   * Like the other fields in `AsyncError`, it is "for diagnosis, not shown to users". It fills a
   * blind spot of first-level feedback:
   * the reported `routePattern` is the **page** route, and first level does not collect `recent`,
   * so before this, first-level records could not show which endpoint failed. See `ApiError.apiRoute`.
   */
  apiMethod: string;
  apiRoute: string;
}

export interface AsyncData<T> {
  /** `null` until loaded. Also `null` on load failure; use `isLoading` to check "loading". */
  data: T | null;
  error: AsyncError | null;
  /**
   * Really loading (no data and no error).
   *
   * **Stop using `data === null` to check loading**: on failure `data` is also `null`, which is
   * exactly
   * what causes the "permanent skeleton".
   */
  isLoading: boolean;
  /** Reload. For the "retry" button, and for refreshing after writes. */
  reload: () => Promise<void>;
  /** Change the data in place without a new request (for when a write returned the full object). */
  setData: React.Dispatch<React.SetStateAction<T | null>>;
}

/**
 * Classify the error.
 *
 * 403 (`access_denied`) is its own category: it is still 403 after ten thousand retries, so a
 * "retry" button would be a lie.
 * Everything else (network, 5xx, parse failures) can be retried.
 *
 * **Not a word of the three branches' conditions changed** (the change only carries out status /
 * code): `kind` is still decided only by
 * `e.code === ACCESS_DENIED_CODE`, and the second branch still only checks that `message` is
 * non-empty.
 * "Optimizing" it into `status === 403` on the side would break this; that is exactly the trap the
 * http.ts file header keeps
 * stressing (the backend may return 403 with other meanings too, and `not_found` would be hit by
 * mistake by status-code checks).
 */
export function classifyError(e: unknown, fallback: string): AsyncError {
  if (e instanceof ApiError && e.code === ACCESS_DENIED_CODE) {
    return {
      kind: "denied",
      message: e.message,
      status: e.status,
      code: e.code,
      logId: e.logId,
      apiMethod: e.apiMethod,
      apiRoute: e.apiRoute,
    };
  }
  // ApiError also goes here: its message is the Chinese sentence from the backend, more accurate
  // than any fallback text.
  // An ApiError that gets here (404 / 409 / 500 / network_error…) must still carry status and code,
  // otherwise the two most valuable diagnostic fields "one-click feedback" gets would be empty on
  // exactly the most common failures.
  if (e instanceof Error && e.message !== "") {
    const api = e instanceof ApiError ? e : null;
    return {
      kind: "other",
      message: e.message,
      status: api?.status ?? 0,
      code: api?.code ?? "",
      // network_error takes exactly this branch, and its logId is empty to begin with;
      // "the request never reached the server" is expressed honestly here as an empty value, with
      // no fallback.
      logId: api?.logId ?? "",
      apiMethod: api?.apiMethod ?? "",
      apiRoute: api?.apiRoute ?? "",
    };
  }
  // Not even a message: not an Error, or the message is empty. No diagnostic information to carry.
  return {
    kind: "other",
    message: fallback,
    status: 0,
    code: "",
    logId: "",
    apiMethod: "",
    apiRoute: "",
  };
}

/**
 * First-level automatic record: report silently on read failure.
 *
 * The implementation moved to `lib/feedback.ts` (`reportAutoFeedback`), because **write failures
 * must go the same way too**:
 * it used to be a private function of this file, so first level only covered read failures, while
 * write failures (sending messages, creating agents,
 * saving settings) were never recorded at all. See the docs there.
 *
 * It is still imported dynamically here: this file is imported back by `lib/feedback.ts` (for the
 * AsyncError type),
 * so a static import would form a cycle, and ESM circular imports give no error, they just leave
 * some module with undefined.
 */
async function reportAutoFeedback(error: AsyncError): Promise<void> {
  try {
    const { reportAutoFeedback: report } = await import("@/lib/feedback");
    await report(error);
  } catch {
    // Silent: anything thrown on this path would cover up the real error scene.
  }
}

/**
 * Load one piece of async data, giving a **renderable error state** on failure rather than a
 * permanent skeleton.
 *
 * `fetcher` must be wrapped in `useCallback` (reload only when dependencies change), matching the
 * old `load`
 * pattern on each page.
 *
 * `fallbackMessage` is the fallback text for when not even a message can be read; the caller takes
 * it from `t()`,
 * so this file does not have to touch i18n.
 */
export function useAsyncData<T>(
  fetcher: () => Promise<T>,
  fallbackMessage: string,
): AsyncData<T> {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<AsyncError | null>(null);

  const run = useCallback(async () => {
    setError(null);
    try {
      setData(await fetcher());
    } catch (e) {
      // The real cause stays in the console: the sentence for the user is a summary; debugging
      // needs this one.
      console.error("加载数据失败", e);
      const classified = classifyError(e, fallbackMessage);
      setError(classified);
      // First-level automatic record: **report silently, swallow failures**.
      //
      // Placed here rather than letting 13 error-state call sites each send one: this is the single
      // meeting point of every load failure on the site,
      // so recording once covers everything, and no page can miss it.
      //
      // ⚠️ Three things that must not change:
      //
      //   1. **No await**. Reporting is a side path; awaiting it would make the error-state render
      //      wait one extra network round trip,
      //      so the "failed to load" text would show up later than now; users would pay latency for
      //      a feature they do not know
      //      exists.
      //   2. **Must have a catch**. `void p` does not guard against rejection (exactly the trap at
      //      the top of this file),
      //      and an unhandled rejection thrown here would cover up the really useful
      //      console.error record above.
      //   3. **Only collect non-sensitive metadata** (level: "auto"). The user did not click
      //      anything,
      //      so no full path, agent id, request trail or console errors are included;
      //      see the table at the top of lib/feedback.ts.
      void reportAutoFeedback(classified);
    }
  }, [fetcher, fallbackMessage]);

  useEffect(() => {
    // When dependencies change (e.g. switching workspace) go back to loading first, otherwise old
    // data would be paired with the new workspace's UI.
    setData(null);
    setError(null);
    void run();
  }, [run]);

  return { data, error, isLoading: data === null && error === null, reload: run, setData };
}

/**
 * Error state for write operations: classify, auto-report, and give the `AsyncError` for the
 * second-level entry.
 *
 * # Why it must exist (this came out of a real incident)
 *
 * One day after one-click feedback shipped the table was empty, not a single record. The root
 * cause was not rate limiting, not the backend:
 * **first level only hung in the catch of `useAsyncData`, and that hook only handles "loading
 * data"**.
 * Write operations (sending messages, creating agents, saving settings) each did `try/catch` then
 * `setXxxError(e.message)`,
 * so:
 *
 *   - first level recorded nothing (`useAsyncData` was not involved in this path at all);
 *   - second level had no entry either (`FeedbackButton` hangs on `AsyncErrorBlock`, while write
 *     failures
 *     rendered an inline `<p class="text-danger">`).
 *
 * When users hit "failed to send message", both levels failed **at the same time**. And this gave
 * zero errors:
 * all the feature code was there, tests green, build passing; it was just never triggered.
 *
 * # Usage: replace `setXxxError(e.message)` with `fail(e)`
 *
 * ```tsx
 * const { error, fail, clear } = useWriteError(t("chat.sendFailedUnknown"));
 * try { … } catch (e) { fail(e); }          // records one first-level entry + gets an AsyncError
 * {error ? <InlineError error={error} /> : null}   // the second-level entry comes along
 * ```
 *
 * `error.message` is the same sentence as the old `e.message` (`classifyError` prefers the
 * backend text),
 * so switching over changes neither the text users see nor the text assertions in existing e2e.
 *
 * # Why reporting lives in `fail()` rather than each caller sending one
 *
 * 20 write-operation catches; making each call report once = 20 chances to miss,
 * and missing gives zero errors. Inside `fail()`, "classify" and "report" become one action:
 * to get a renderable error state, you must already have reported.
 */
export function useWriteError(fallbackMessage: string): {
  error: AsyncError | null;
  /** Record one write failure: classify → silently report first level → store in state for rendering. */
  fail: (e: unknown) => AsyncError;
  /** Clear the error state (call before retry; matches the old `setXxxError(null)` one to one). */
  clear: () => void;
} {
  const [error, setError] = useState<AsyncError | null>(null);

  const fail = useCallback(
    (e: unknown): AsyncError => {
      // The real cause stays in the console: the sentence for the user is a summary; debugging
      // needs this one.
      // Consistent with useAsyncData's read-failure path.
      console.error("写操作失败", e);
      const classified = classifyError(e, fallbackMessage);
      setError(classified);
      // First-level automatic record: **no await, and it never rejects itself** (see
      // lib/feedback.ts).
      void reportAutoFeedback(classified);
      return classified;
    },
    [fallbackMessage],
  );

  const clear = useCallback(() => setError(null), []);

  return { error, fail, clear };
}

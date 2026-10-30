/**
 * Formatting for times, durations and token counts. Shared by list and detail pages so each place
 * does not write its own.
 *
 * Locale-dependent parts (relative time, dates, thousands separators) use Intl rather than
 * hand-written Chinese strings, so they follow
 * the current language. Callers take language from usePrefs() and pass it in; without it the
 * browser default is used.
 */

export function formatDateTime(iso: string, locale?: string): string {
  const d = new Date(iso);
  return d.toLocaleString(locale ?? "zh-CN", {
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    hour12: false,
  });
}

export function formatTime(iso: string, locale?: string): string {
  return new Date(iso).toLocaleTimeString(locale ?? "zh-CN", {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    hour12: false,
  });
}

/**
 * A date down to the "day" only, for things like **join time** that do not care about hours,
 * minutes and seconds.
 *
 * Split with formatDateTime: that one has h:m:s and is for debugging ("what minute did this event
 * happen");
 * this one has only year/month/day and is for statements ("which day did they join").
 *
 * ⚠️ It exists because of **layout**: that column in the member list has limited width,
 * `2026/07/26 18:00:00 joined` would push into the remove button, and those three `:00` carry no
 * information at all.
 */
export function formatDate(iso: string, locale?: string): string {
  const d = new Date(iso);
  return d.toLocaleDateString(locale ?? "zh-CN", {
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
  });
}

/**
 * Relative time. For something recent "x minutes ago" reads better than an absolute time; past 30
 * days it falls back to an absolute date **down to the day**.
 *
 * Uses Intl.RelativeTimeFormat rather than hand-writing Chinese and English strings: plurals and
 * word order differ by language,
 * and the platform does it more reliably than our own string building.
 *
 * ⚠️ The fallback uses formatDate, not formatDateTime: all callers are a column in a list,
 * where h:m:s is useless and would blow up the column (in the member list it was seen pushing
 * into the remove button).
 * Places that really need h:m:s call formatDateTime directly.
 */
export function formatRelative(iso: string, locale?: string): string {
  const loc = locale ?? "zh-CN";
  const diffMs = Date.now() - new Date(iso).getTime();
  const sec = Math.floor(diffMs / 1000);
  if (sec < 0) return formatDateTime(iso, loc);

  const rtf = new Intl.RelativeTimeFormat(loc, { numeric: "auto" });
  if (sec < 60) return rtf.format(-sec, "second");
  const min = Math.floor(sec / 60);
  if (min < 60) return rtf.format(-min, "minute");
  const hour = Math.floor(min / 60);
  if (hour < 24) return rtf.format(-hour, "hour");
  const day = Math.floor(hour / 24);
  if (day < 30) return rtf.format(-day, "day");
  return formatDate(iso, loc);
}

export function formatDuration(ms: number | undefined): string {
  if (ms == null) return "—";
  if (ms < 1000) return `${ms}ms`;
  const sec = ms / 1000;
  if (sec < 60) return `${sec.toFixed(1)}s`;
  const min = Math.floor(sec / 60);
  const rest = Math.round(sec % 60);
  return `${min}m ${rest}s`;
}

/** Token counts are shortened with k so long numbers do not stretch the list. */
export function formatTokens(n: number): string {
  if (n < 1000) return String(n);
  if (n < 1_000_000) return `${(n / 1000).toFixed(1)}k`;
  return `${(n / 1_000_000).toFixed(2)}M`;
}

export function formatNumber(n: number, locale?: string): string {
  return n.toLocaleString(locale ?? "zh-CN");
}

/** Truncate long text, for summary columns in lists. */
export function truncate(s: unknown, max = 80): string {
  // Takes unknown rather than string: callers often pass wire fields that "the docs say are
  // string but are really objects"
  // (e.g. agent.tool_use.input). This once blanked the whole page because s.replace was not a
  // function:
  // one tool-call event could crash the chat page, far more costly than one extra type check.
  if (s == null) return "";
  const text = typeof s === "string" ? s : safeStringify(s);
  const flat = text.replace(/\s+/g, " ").trim();
  return flat.length <= max ? flat : `${flat.slice(0, max)}…`;
}

/** Fallback serialization: falls back to String() on odd cases like circular references. */
function safeStringify(v: unknown): string {
  try {
    return JSON.stringify(v) ?? String(v);
  } catch {
    return String(v);
  }
}

/**
 * Try to pretty-print JSON with indentation; if it is not valid JSON, return it as is.
 *
 * Takes `unknown` rather than `string`: the Ark docs say `agent.tool_use.input` is a "JSON
 * string", but what actually comes down is often already an **object**. Passing an object in
 * makes `JSON.parse` throw;
 * the old code returned the object as is in catch, and it ended up rendered as `[object Object]`.
 * So we branch on type first and do not trust the docs' `string`.
 */
export function prettyJson(raw: unknown): string {
  if (raw == null) return "";
  if (typeof raw !== "string") {
    try {
      return JSON.stringify(raw, null, 2);
    } catch {
      return String(raw);
    }
  }
  try {
    return JSON.stringify(JSON.parse(raw), null, 2);
  } catch {
    return raw;
  }
}

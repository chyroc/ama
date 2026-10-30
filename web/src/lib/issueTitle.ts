/**
 * Automatic extraction of issue titles: **two implementations of the same rules as the backend's
 * core/issuetitle.go**.
 *
 * # Why the frontend has a copy too
 *
 * `LocalApiClient` is the source of truth for the backend contract, and also the base e2e runs on
 * (in local mode not a single network request
 * is sent). Without this copy, the behavior "leave title empty → extract it from the body" could
 * never be reached in e2e,
 * and it is exactly the part of this feature most likely to go wrong.
 *
 * ⚠️ **The two copies must match rule for rule.** Drift shows up as "in demo mode the title is
 * this, with the real backend it becomes
 * that", and both results look reasonable, so it cannot be found from the symptom. When changing
 * any rule, change both sides,
 * and update the tests on both sides too.
 *
 * # Rules (one to one with the Go version)
 *
 * 1. Title non-empty → use it as is (trim + collapse whitespace + truncate)
 * 2. Title empty, body non-empty → take the body's first "line with content"
 * 3. Both empty → return an empty string, **the caller rejects it** (no fallback to "untitled
 *    issue" here)
 *
 * Stripping markup is not a nice-to-have: the most typical failure is a user pasting an image and
 * sending (the body is
 * `![](https://x/a.png)`), and the title becomes that Markdown string: no error, it saves fine,
 * and the list shows a line of garbage.
 */

/** Title length limit, **in code points, not bytes**. Same as the backend's maxIssueTitleRunes. */
export const MAX_ISSUE_TITLE_LENGTH = 80;

/** Fallback title when nothing at all can be extracted. Matches the backend's untitledIssueTitle word for word. */
export const UNTITLED_ISSUE_TITLE = "未命名问题";

// Block-level prefixes. A space is required after `#`: `#urgent` is not a heading, and stripping
// the hash would lose what the user meant.
const MD_HEADING = /^#{1,6}\s+/;
const MD_QUOTE = /^(?:\s*>)+\s*/;
// A space is also required after: a line starting with `*emphasis*` is not a list item, and
// stripping it as a list would leave a lone asterisk.
const MD_BULLET = /^\s*[-*+]\s+/;
const MD_ORDERED = /^\s*\d+[.)]\s+/;
// Fence lines are **skipped entirely** rather than prefix-stripped: stripping ``` leaves the
// language name (go / bash), which is not a title.
const MD_FENCE = /^\s*(?:```|~~~)/;
const MD_THEMATIC_BREAK = /^\s*(?:-{3,}|\*{3,}|_{3,})\s*$/;

// Inline structures. ⚠️ Images must come before links: image syntax is a link with a `!` in front,
// and the other order would turn `![alt](url)` into `!alt`, with an extra exclamation mark.
const MD_IMAGE = /!\[([^\]]*)\]\([^)]*\)/g;
const MD_LINK = /\[([^\]]*)\]\([^)]*\)/g;
const MD_AUTOLINK = /<([^>]*)>/g;
const MD_CODE = /`([^`]*)`/g;
const MD_STRIKE = /~~(.+?)~~/g;
// Bold must come before italic: matching `**x**` with single asterisks would leave `*x*`.
const MD_BOLD = /(\*\*|__)(.+?)(\*\*|__)/g;
const MD_EMPHASIS = /(\*|_)([^*_]+?)(\*|_)/g;

const SPACE_RUN = /\s+/g;

/**
 * Decide an issue's final title.
 *
 * **When both are empty return an empty string, not a fallback**: that is a record with no
 * content at all, and the caller should
 * reject it. A fallback here would let empty records save silently, adding a row to the list that
 * has nothing inside.
 */
export function deriveIssueTitle(title: string, body: string): string {
  const t = title.trim();
  if (t !== "") {
    return truncateByCodePoints(collapseSpaces(t), MAX_ISSUE_TITLE_LENGTH);
  }
  if (body.trim() === "") return "";
  return extractTitleFromBody(body);
}

/**
 * Extract the first sentence with content from a Markdown body.
 *
 * Scan line by line rather than only the first line: the first line may well be blank, a fence,
 * or an image without alt;
 * looking only at it would give an empty title while the body clearly has words.
 */
function extractTitleFromBody(body: string): string {
  for (const line of body.split("\n")) {
    if (MD_FENCE.test(line)) continue;
    if (MD_THEMATIC_BREAK.test(line)) continue;
    const cleaned = stripMarkdown(line);
    if (cleaned !== "") {
      return truncateByCodePoints(cleaned, MAX_ISSUE_TITLE_LENGTH);
    }
  }
  return UNTITLED_ISSUE_TITLE;
}

/** Strip one line of Markdown into plain text; returns an empty string if nothing is left. */
function stripMarkdown(line: string): string {
  let s = line;

  // Block-level prefixes. A quote may wrap a list (`> - item`), so try the list after stripping
  // the quote.
  s = s.replace(MD_QUOTE, "");
  s = s.replace(MD_HEADING, "");
  s = s.replace(MD_ORDERED, "");
  s = s.replace(MD_BULLET, "");

  // Inline structures; the order must not change (see the comments on the constants above).
  s = s.replace(MD_IMAGE, "$1");
  s = s.replace(MD_LINK, "$1");
  s = s.replace(MD_AUTOLINK, "$1");
  s = s.replace(MD_CODE, "$1");
  s = s.replace(MD_STRIKE, "$1");
  s = s.replace(MD_BOLD, "$2");
  s = s.replace(MD_EMPHASIS, "$2");

  s = collapseSpaces(s);

  // A line with only punctuation left after stripping does not count as content (table separator
  // rows `|---|---|`, a line of a few symbols).
  // Making it the title would look as ugly as not stripping the Markdown at all.
  if (!hasReadableChar(s)) return "";
  return s;
}

/**
 * Whether a piece of text has "readable characters".
 *
 * Uses Unicode property classes rather than listing ASCII: CJK text and Latin letters must get the
 * same answer.
 */
function hasReadableChar(s: string): boolean {
  return /[\p{L}\p{N}]/u.test(s);
}

/** Collapse runs of whitespace into a single space and trim; a title is always one line. */
function collapseSpaces(s: string): string {
  return s.replace(SPACE_RUN, " ").trim();
}

/**
 * Truncate by **code point**, adding an ellipsis when too long.
 *
 * `[...s]` rather than `s.slice()`: the latter cuts by UTF-16 code unit, and would cut emoji and
 * supplementary-plane characters
 * in half (leaving a lone surrogate). For Chinese in the BMP the two agree, but a single emoji
 * gives it away.
 *
 * Cut to limit-1 and then add the ellipsis, so the total length is still limit; otherwise "limit
 * 80" would produce 81 characters.
 */
function truncateByCodePoints(s: string, limit: number): string {
  const chars = [...s];
  if (chars.length <= limit) return s;
  return chars.slice(0, limit - 1).join("").trimEnd() + "…";
}

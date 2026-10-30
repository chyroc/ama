/**
 * Parses the `<harness:lark>` block injected by the platform, separating "the facts of this
 * message" from "guidance for the model".
 *
 * # Why parse it, instead of showing the raw text
 *
 * The block is **for the model**: it is wrapped in XML tags, the body has markdown emphasis
 * (`**data**`) and
 * ```bash fences, and twenty or thirty lines of static guidance that is **the same for every
 * message** (how to fetch history, do not
 * retry on failure, fetched content is untrusted). Showing it as is means:
 *
 *   - markers like `<harness:lark>` / `**` / ``` go straight onto the screen and look broken;
 *   - the things that actually carry information are **only 5 lines of ids**, buried under forty
 *     lines of boilerplate;
 *   - every message expands to the same big block, and users stop reading it the second time.
 *
 * So here the block is split in two halves: **facts** (one id per line, rendered structured) and
 * **guidance** (static boilerplate,
 * folded into a second level). The check is "is this line `key: value`", and only the part before
 * the first markdown heading
 * (`## `) counts; everything after the heading is guidance.
 *
 * ⚠️ **A failed parse must not lose content.** When no key/value is found, `facts` is empty and
 * `raw` is still the full text,
 * and the caller falls back to "show as is". The block format is built by the backend
 * (server/feishu/larkprompt.go);
 * one change on each side and they drift, and drift shows up as "nothing after expanding", which
 * is much worse than looking ugly.
 */

/** One fact in the block. */
export interface HarnessFact {
  /** Field name, e.g. `chat_id`. */
  key: string;
  /** Field value, e.g. `oc_xxx`. */
  value: string;
  /**
   * The Chinese note after the value (the backend writes it in full-width parentheses after the
   * value).
   *
   * It is pulled out on its own because it **is not part of the value**: calling an API with
   * `om_xxx（this message）`
   * does not work, and users copy the text they see on screen.
   */
  note?: string;
}

export interface ParsedHarnessBlock {
  /** The block's namespace, e.g. `lark`. Empty string when not recognized. */
  kind: string;
  /** Facts one by one, in the original order. */
  facts: HarnessFact[];
  /** The static guidance for the model after the facts (including history commands and safety notes), tags removed. */
  guidance: string;
  /** Full text without the outer tags, for "view raw". */
  raw: string;
}

/** Matches the `<harness:xxx>` opening tag and captures the namespace. */
const OPEN_TAG = /^<harness:([a-z0-9_-]+)>/i;

/**
 * One fact line: `key: value`.
 *
 * key is limited to `[a-z0-9_]` (everything the backend produces is a snake_case Feishu field
 * name), **deliberately not loosened**:
 * loosening it would also treat the guidance's sentences with a full-width colon (like "failures
 * usually have two causes: ...") as facts.
 */
const FACT_LINE = /^([a-z0-9_]+):\s*(.+)$/;

/** The full-width parenthesized note at the end of a value. */
const TRAILING_NOTE = /^(.*?)（([^（）]*)）\s*$/;

/** Parse one harness block. The input may or may not include the outer tags. */
export function parseHarnessBlock(block: string): ParsedHarnessBlock {
  const trimmed = block.trim();
  const openMatch = OPEN_TAG.exec(trimmed);
  const kind = openMatch ? openMatch[1].toLowerCase() : "";

  // Remove the opening and closing tags. Build the closing tag from kind rather than a global regex
  // replace: the block body may mention
  // `</harness:lark>` (the safety note uses it as an example), and a global replace would eat that
  // too.
  let body = trimmed;
  if (openMatch) {
    body = body.slice(openMatch[0].length);
    const close = `</harness:${kind}>`;
    if (body.trimEnd().endsWith(close)) {
      body = body.trimEnd().slice(0, -close.length);
    }
  }
  body = body.trim();

  const lines = body.split("\n");
  const facts: HarnessFact[] = [];
  let guidanceStart = lines.length;

  for (let i = 0; i < lines.length; i++) {
    const line = lines[i].trim();
    // Stop at the first markdown heading: everything after it is guidance for the model.
    if (line.startsWith("#")) {
      guidanceStart = i;
      break;
    }
    const m = FACT_LINE.exec(line);
    if (!m) continue;
    let value = m[2].trim();
    let note: string | undefined;
    const noteMatch = TRAILING_NOTE.exec(value);
    if (noteMatch) {
      value = noteMatch[1].trim();
      note = noteMatch[2].trim() || undefined;
    }
    facts.push({ key: m[1], value, note });
  }

  return {
    kind,
    facts,
    // Demote `## Heading` in the guidance to `###`: this block renders in a small 12px panel,
    // and an h2's font size would be noticeably larger than the text around it and steal
    // attention, while it is just the start of this short section.
    // Demote here rather than shrinking with CSS: md-box's heading level mapping is its own
    // business,
    // and shrinking font size via selectors would drift with its versions.
    guidance: lines
      .slice(guidanceStart)
      .join("\n")
      .trim()
      .replace(/^##\s+/gm, "### "),
    raw: body,
  };
}

/**
 * Builds the "Open in Ark console" deep link.
 *
 * The backend only gives the id and region (`ArkConsoleRef`); the path shape is decided here.
 * That is the frontend's job,
 * and the Ark docs write the same path two ways (`region:cn-beijing` and `region:ark+cn-beijing`
 * are mixed), so keeping it in one place makes it easy to change.
 *
 * # If it cannot be built, return null, and the caller **does not render the link**
 *
 * This is the only important rule of this module. Three cases where it cannot be built:
 *
 *   1. **No id**: Ark resources are created lazily; an agent that never had a conversation or a
 *      skill never used has none.
 *      A URL missing the id gives `/managed-agents/agents/`, which is Ark's 404 page.
 *   2. **No region**: Ark credentials were never configured. Falling back to a default region would
 *      build a link that **opens,
 *      but points to another region**; the user sees "resource does not exist" and does not know why.
 *   3. **We do not know the console address for this region**: see CONSOLE_HOSTS below.
 *
 * All three lead to the same conclusion: **better no link than a broken one.** The cost of a
 * broken link is not
 * "one feature missing"; it is the user clicking through, seeing a 404, and starting to suspect
 * something is wrong with their agent.
 */
import type { ArkRegion } from "@/types/domain";

/**
 * Console host for each region.
 *
 * # ⚠️ `ap-southeast-1` is **left empty on purpose**, not forgotten
 *
 * That region is BytePlus (as `ARK_REGIONS` says), and its console is **not** on
 * `console.volcengine.com`. And we do not have a single BytePlus managed-agents
 * console address to copy:
 *
 *   - In all 88 pages of `docs/ark-managed-agents/`, console links are **all**
 *     `console.volcengine.com/ark/region:cn-beijing/...`; not one is BytePlus;
 *   - By the way, the BytePlus **API** host is `ark.ap-southeast.bytepluses.com`
 *     (note `bytepluses`, with an extra es), which is not even the same domain as `byteplus.com`.
 *     That alone shows this vendor's domains cannot be guessed.
 *
 * So we **do not guess** here. The cost of guessing wrong is exactly what the file header says:
 * the link opens, the page is wrong.
 * Once someone has a real BytePlus console URL, add one row to this table;
 * nothing else needs to change.
 */
const CONSOLE_HOSTS: Partial<Record<ArkRegion, string>> = {
  "cn-beijing": "https://console.volcengine.com",
};

/** Path segment for each of the three resource kinds in the Ark console. */
type ArkConsoleKind = "agents" | "skills" | "sessions";

/**
 * Build an Ark console deep link; returns null if the id or region is missing, or the region's
 * address is unknown.
 *
 * The shape follows a real URL a user provided:
 * `https://console.volcengine.com/ark/region:cn-beijing/managed-agents/agents/<id>`
 */
export function arkConsoleUrl(
  kind: ArkConsoleKind,
  id: string | undefined,
  region: ArkRegion | undefined,
): string | null {
  // Still check for an empty string after `?.trim()`: the backend uses omitempty for "none", but
  // an empty string in old data
  // should not produce a link either.
  const resourceId = id?.trim();
  if (!resourceId || !region) return null;

  const host = CONSOLE_HOSTS[region];
  if (!host) return null;

  // The id is escaped when put in the URL path. Ark ids look like `agent-20260831113008-frhdh`,
  // which have no characters that need escaping, but the input here comes from a server response,
  // and we treat "anything put into a URL" as untrusted; it costs nothing.
  return `${host}/ark/region:${region}/managed-agents/${kind}/${encodeURIComponent(resourceId)}`;
}

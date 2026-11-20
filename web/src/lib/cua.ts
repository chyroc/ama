/**
 * Volcengine Agent Plan CUA (Computer Use Agent / Cloud Desktop) URL & session extractor.
 *
 * Scans AMA run events (tool results from `cua.py` and assistant messages) for CUA cloud desktop
 * access URLs (`full_interface_url`, `desktop_view_url`, `access_url`) and task metadata so AMA can
 * render the live cloud desktop directly inside the conversation and the CUA panel.
 */

import type { RunEvent } from "@/types/events";
import type { TimelineItem } from "@/lib/timeline";

export interface CuaDesktopSession {
  /** Cloud desktop instance identifier (extracted from JSON or the `/desktops/<id>` URL path). */
  desktopId: string;
  /** Full CUA web application URL (`/desktops/<id>/cua-app/?ticket=...`). */
  fullInterfaceUrl: string;
  /** Desktop-only stream view URL (`/desktops/<id>?ticket=...`). */
  desktopViewUrl: string;
  /** Base access URL returned by CUA. */
  accessUrl: string;
  /** Latest CUA CLI action observed for this desktop (e.g. "desktop access", "delegate"). */
  action?: string;
  /** Latest task ID associated with this desktop, if any. */
  taskId?: string;
  /** Latest task outcome ("completed" | "needs_human" | "failed" | "cancelled" | "timeout"). */
  outcome?: string;
  /** Latest result summary text, if any. */
  resultText?: string;
  /** ISO timestamp of the event that produced or updated this session. */
  updatedAt: string;
  /** Event sequence number where the URL was most recently updated. */
  seq: number;
}

/**
 * Derive both `desktopViewUrl` and `fullInterfaceUrl` from any CUA desktop URL containing `/desktops/<id>`.
 * Mirrors `build_desktop_urls` in `skills/ark-cua/scripts/cua.py`.
 */
export function deriveCuaUrls(rawUrl: string): {
  desktopId: string;
  accessUrl: string;
  desktopViewUrl: string;
  fullInterfaceUrl: string;
} | null {
  const trimmed = rawUrl.trim();
  if (!trimmed) return null;
  try {
    const parsed = new URL(trimmed);
    if (parsed.protocol !== "https:" && parsed.protocol !== "http:") {
      return null;
    }
    // Match `/.../desktops/<desktopId>` optionally followed by `/cua-app` or `/relay/rustdesk/<desktopId>`
    const match = parsed.pathname.match(
      /^(.*\/desktops\/([^/]+?))(\/cua-app\/?|\/relay\/rustdesk\/[^/]+\/?)?\/?$/,
    );
    if (!match) {
      return null;
    }
    const basePath = match[1];
    const desktopId = decodeURIComponent(match[2]);
    if (!desktopId) {
      return null;
    }

    const fullInterface = new URL(parsed.toString());
    fullInterface.pathname = `${basePath}/cua-app/`;
    fullInterface.searchParams.delete("ama_mode");

    const desktopView = new URL(parsed.toString());
    desktopView.pathname = `${basePath}/relay/rustdesk/${encodeURIComponent(desktopId)}/`;
    desktopView.searchParams.delete("ama_mode");

    return {
      desktopId,
      accessUrl: fullInterface.toString(),
      desktopViewUrl: desktopView.toString(),
      fullInterfaceUrl: fullInterface.toString(),
    };
  } catch {
    return null;
  }
}

/**
 * Convert an absolute CUA URL (`https://cua.ark.cn-beijing.volces.com/desktops/...`)
 * into a same-origin proxy path (`/desktops/...`) so `<iframe>` embedding avoids
 * cross-site `SameSite=Lax` cookie blocking (`401 MissingTicket`) and plain-HTTP `crypto.randomUUID` errors.
 */
export function toEmbeddableCuaUrl(rawUrl: string): string {
  const trimmed = rawUrl.trim();
  if (!trimmed) return "";
  try {
    const parsed = new URL(
      trimmed,
      typeof window !== "undefined" ? window.location.origin : "http://localhost",
    );
    const rustdeskMatch = parsed.pathname.match(
      /^.*(\/desktops\/[^/]+\/relay\/rustdesk\/[^/]+)\/?$/,
    );
    if (rustdeskMatch) {
      return `${rustdeskMatch[1]}/${parsed.search}${parsed.hash}`;
    }
    const match = parsed.pathname.match(/^.*(\/desktops\/([^/]+?))(?:\/cua-app\/?)?\/?$/);
    if (match) {
      if (parsed.searchParams.get("ama_mode") === "desktop") {
        parsed.searchParams.delete("ama_mode");
        return `${match[1]}/relay/rustdesk/${match[2]}/${parsed.search}${parsed.hash}`;
      }
      return `${match[1]}/cua-app/${parsed.search}${parsed.hash}`;
    }
    return trimmed;
  } catch {
    return trimmed;
  }
}

/**
 * Regex matching CUA desktop URLs in plain text, markdown, or JSON strings.
 * Matches `http(s)://.../desktops/<id>[/cua-app/]?ticket=...`
 */
const CUA_URL_REGEX =
  /https?:\/\/[^\s"'<>)\]]+\/desktops\/[A-Za-z0-9._-]+(?:\/cua-app\/?)?(?:\?[^\s"'<>)\]]*)?/g;

interface RawCuaPayload {
  action?: string;
  desktopId?: string;
  taskId?: string;
  outcome?: string;
  resultText?: string;
  accessUrl?: string;
  desktopViewUrl?: string;
  fullInterfaceUrl?: string;
}

function collectBalancedJsonCandidates(text: string): string[] {
  const trimmed = text.trim();
  if (!trimmed) return [];
  try {
    const direct = JSON.parse(trimmed);
    if (direct && typeof direct === "object" && !Array.isArray(direct)) {
      return [trimmed];
    }
  } catch {
    // Fall back to scanning balanced `{...}` fragments inside mixed text.
  }

  const candidates: string[] = [];
  for (let i = 0; i < text.length; i++) {
    if (text[i] !== "{") continue;
    let depth = 0;
    let inStr = false;
    let escaped = false;
    for (let j = i; j < text.length; j++) {
      const ch = text[j];
      if (inStr) {
        if (escaped) {
          escaped = false;
        } else if (ch === "\\") {
          escaped = true;
        } else if (ch === '"') {
          inStr = false;
        }
        continue;
      }
      if (ch === '"') {
        inStr = true;
      } else if (ch === "{") {
        depth++;
      } else if (ch === "}") {
        depth--;
        if (depth === 0) {
          const slice = text.slice(i, j + 1);
          if (
            slice.includes("full_interface_url") ||
            slice.includes("desktop_view_url") ||
            slice.includes("access_url") ||
            slice.includes("desktop_id")
          ) {
            candidates.push(slice);
          }
          i = j;
          break;
        }
      }
    }
  }
  return candidates;
}

/**
 * Extract structured CUA JSON payloads and/or raw CUA desktop URLs from a text block.
 */
function parseCuaFromText(text: string): RawCuaPayload[] {
  if (
    !text ||
    (!text.includes("/desktops/") &&
      !text.includes("cua.py") &&
      !text.includes("desktop_id") &&
      !text.includes("full_interface_url") &&
      !text.includes("desktop_view_url"))
  ) {
    return [];
  }

  const results: RawCuaPayload[] = [];
  const candidates = collectBalancedJsonCandidates(text);
  const seenUrls = new Set<string>();

  for (const candidate of candidates) {
    try {
      const parsed = JSON.parse(candidate) as Record<string, unknown>;
      if (!parsed || typeof parsed !== "object") continue;
      const data =
        parsed.data && typeof parsed.data === "object"
          ? (parsed.data as Record<string, unknown>)
          : parsed;

      const action = typeof parsed.action === "string" ? parsed.action : undefined;
      const desktopId = typeof data.desktop_id === "string" ? data.desktop_id : undefined;
      const taskId = typeof data.task_id === "string" ? data.task_id : undefined;
      const outcome = typeof data.outcome === "string" ? data.outcome : undefined;
      const resultText = typeof data.result_text === "string" ? data.result_text : undefined;
      const accessUrl = typeof data.access_url === "string" ? data.access_url : undefined;
      const desktopViewUrl =
        typeof data.desktop_view_url === "string" ? data.desktop_view_url : undefined;
      const fullInterfaceUrl =
        typeof data.full_interface_url === "string" ? data.full_interface_url : undefined;

      if (accessUrl || desktopViewUrl || fullInterfaceUrl || desktopId || (action && taskId)) {
        if (accessUrl) seenUrls.add(accessUrl);
        if (desktopViewUrl) seenUrls.add(desktopViewUrl);
        if (fullInterfaceUrl) seenUrls.add(fullInterfaceUrl);
        results.push({
          action,
          desktopId,
          taskId,
          outcome,
          resultText,
          accessUrl,
          desktopViewUrl,
          fullInterfaceUrl,
        });
      }
    } catch {
      // Ignore non-JSON fragments.
    }
  }

  // Also scan for raw `/desktops/<id>` URLs in markdown or plain text that were not already captured.
  const matches = text.match(CUA_URL_REGEX) ?? [];
  for (const rawMatch of matches) {
    const cleanUrl = rawMatch.replace(/[.,;!?]+$/, "");
    if (seenUrls.has(cleanUrl)) continue;
    const derived = deriveCuaUrls(cleanUrl);
    if (!derived) continue;
    seenUrls.add(derived.accessUrl);
    seenUrls.add(derived.desktopViewUrl);
    seenUrls.add(derived.fullInterfaceUrl);
    results.push({
      desktopId: derived.desktopId,
      accessUrl: derived.accessUrl,
      desktopViewUrl: derived.desktopViewUrl,
      fullInterfaceUrl: derived.fullInterfaceUrl,
    });
  }

  return results;
}

function mergePayloadIntoSessions(
  byDesktop: Map<string, CuaDesktopSession>,
  p: RawCuaPayload,
  lastKnownDesktopId: string | undefined,
  updatedAt: string,
  seq: number,
): string | undefined {
  const seedUrl = p.fullInterfaceUrl || p.desktopViewUrl || p.accessUrl;
  const derived = seedUrl ? deriveCuaUrls(seedUrl) : null;
  const canFallbackDesktopId = Boolean(p.action && p.taskId);
  const desktopId =
    p.desktopId || derived?.desktopId || (canFallbackDesktopId ? lastKnownDesktopId : undefined);
  if (!desktopId) return lastKnownDesktopId;

  const existing = byDesktop.get(desktopId);
  const fallbackFull = desktopId.startsWith("desk_") ? `/desktops/${desktopId}/cua-app/` : "";
  const fallbackDesktop = desktopId.startsWith("desk_")
    ? `/desktops/${desktopId}/relay/rustdesk/${desktopId}/`
    : "";
  const fullInterfaceUrl =
    derived?.fullInterfaceUrl || p.fullInterfaceUrl || existing?.fullInterfaceUrl || fallbackFull;
  const desktopViewUrl =
    derived?.desktopViewUrl || p.desktopViewUrl || existing?.desktopViewUrl || fallbackDesktop;
  const accessUrl = derived?.accessUrl || p.accessUrl || existing?.accessUrl || fallbackFull;

  if (!fullInterfaceUrl && !desktopViewUrl && !existing) {
    return lastKnownDesktopId;
  }

  byDesktop.set(desktopId, {
    desktopId,
    fullInterfaceUrl: fullInterfaceUrl || desktopViewUrl,
    desktopViewUrl: desktopViewUrl || fullInterfaceUrl,
    accessUrl: accessUrl || desktopViewUrl || fullInterfaceUrl,
    action: p.action ?? existing?.action,
    taskId: p.taskId ?? existing?.taskId,
    outcome: p.outcome ?? existing?.outcome,
    resultText: p.resultText ?? existing?.resultText,
    updatedAt,
    seq,
  });

  return desktopId;
}

/**
 * Extract all CUA cloud desktop sessions from a list of `RunEvent`s, ordered from newest to oldest.
 */
export function extractCuaSessions(events: RunEvent[]): CuaDesktopSession[] {
  const byDesktop = new Map<string, CuaDesktopSession>();
  let lastKnownDesktopId: string | undefined;

  for (let idx = 0; idx < events.length; idx++) {
    const ev = events[idx];
    const texts: string[] = [];
    if (
      ev.type === "agent.tool_result" ||
      ev.type === "user.tool_result" ||
      ev.type === "user.custom_tool_result" ||
      ev.type === "agent.mcp_tool_result" ||
      ev.type === "agent.message"
    ) {
      for (const blk of ev.content ?? []) {
        if (blk.type === "text" && blk.text) {
          texts.push(blk.text);
        }
      }
    }

    for (const txt of texts) {
      const payloads = parseCuaFromText(txt);
      for (const p of payloads) {
        lastKnownDesktopId = mergePayloadIntoSessions(
          byDesktop,
          p,
          lastKnownDesktopId,
          ev.processed_at ?? "",
          ev.sequence ?? idx,
        );
      }
    }
  }

  return Array.from(byDesktop.values()).sort((a, b) => b.seq - a.seq);
}

/**
 * Extract CUA cloud desktop sessions from paired `TimelineItem`s (such as a single conversation turn).
 */
export function extractCuaSessionsFromTimelineItems(items: TimelineItem[]): CuaDesktopSession[] {
  const byDesktop = new Map<string, CuaDesktopSession>();
  let lastKnownDesktopId: string | undefined;

  for (let idx = 0; idx < items.length; idx++) {
    const item = items[idx];
    const texts: string[] = [];
    if (item.result?.text) {
      texts.push(item.result.text);
    }
    if (item.body) {
      texts.push(item.body);
    }

    for (const txt of texts) {
      const payloads = parseCuaFromText(txt);
      for (const p of payloads) {
        lastKnownDesktopId = mergePayloadIntoSessions(
          byDesktop,
          p,
          lastKnownDesktopId,
          item.processedAt ?? "",
          item.sequence ?? idx,
        );
      }
    }
  }

  return Array.from(byDesktop.values()).sort((a, b) => b.seq - a.seq);
}

/**
 * Strip redundant CUA access link sections and bullet lines from assistant markdown text.
 *
 * AMA extracts CUA desktop sessions directly from tool results (`user.tool_result`), and raw
 * `https://cua.ark.../desktops/...?ticket=...` URLs contain single-use tickets that expire once
 * consumed by the embedded viewer. Stripping these link blocks keeps the conversation text focused
 * on business results.
 */
export function stripCuaUrlNoise(markdown: string): string {
  if (!markdown || !markdown.includes("/desktops/")) {
    return markdown;
  }
  const lines = markdown.split("\n");
  const filtered: string[] = [];
  let inCodeFence = false;

  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    const trimmed = line.trim();

    if (trimmed.startsWith("```")) {
      inCodeFence = !inCodeFence;
      filtered.push(line);
      continue;
    }
    if (inCodeFence) {
      filtered.push(line);
      continue;
    }

    // Skip markdown headings that only introduce CUA links, e.g. "## Cloud Desktop (Live View)"
    if (/^#{1,6}\s*(?:CUA\s*)?云桌面[（(]?实时画面[)）]?\s*$/i.test(trimmed)) {
      continue;
    }

    // Skip bullet or standalone lines whose main purpose is pasting a raw `/desktops/desk_...` ticket URL
    if (
      /^(?:[-*•]|\d+\.)?\s*(?:完整\s*CUA\s*界面|云桌面完整界面|桌面实时画面|云桌面实时画面|云桌面访问链接|CUA\s*实时画面|full_interface_url|desktop_view_url|access_url)\s*[:：]/i.test(
        trimmed,
      ) &&
      trimmed.includes("/desktops/")
    ) {
      continue;
    }

    filtered.push(line);
  }

  return filtered
    .join("\n")
    .replace(/\n{3,}/g, "\n\n")
    .trim();
}

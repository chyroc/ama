/**
 * Volcengine Seedance MA video task & URL extractor.
 *
 * Scans AMA timeline items (tool calls and JSON outputs from `seedance_ma.py`) so the chat view
 * can render a live Seedance progress & storyboard card while the video is rendering and an
 * inline HTML5 `<video>` player once completed.
 */

import { API_BASE } from "@/data/http";
import type { TimelineItem } from "@/lib/timeline";

export interface SeedanceImage {
  index: number;
  localPath?: string;
  imageUrl?: string;
  arkImageUrl?: string;
}

export interface SeedanceVideoTask {
  /** Video tasks use a player; image tasks use a gallery and never invent a video failure. */
  kind: "video" | "image";
  /** Seedance MA session ID (`sesn-...`) or temporary key while initial call is executing. */
  sessionId: string;
  /** Short video title. */
  title?: string;
  /** User's creative prompt passed to `--prompt`. */
  userPrompt?: string;
  /** Detailed storyboard & camera prompt crafted by Seedance MA. */
  storyboardPrompt?: string;
  /** Aspect ratio (e.g. "16:9", "9:16"). */
  ratio?: string;
  /** Video duration in seconds (e.g. 5). */
  duration?: number;
  /** Underlying video generation model identifier, if reported. */
  videoModel?: string;
  /** Task outcome ("in_progress" | "completed" | "failed"). */
  outcome: "in_progress" | "completed" | "failed";
  /** Current pipeline stage identifier. */
  stage?: string;
  /** Human-readable stage description. */
  stageLabel?: string;
  /** Upstream async task ID (`task-...`). */
  taskId?: string;
  /** Seedance content generation task ID (`cgt-...`). */
  cgtTaskId?: string;
  /** Playable HTTPS MP4 URL. */
  videoUrl?: string;
  /** Raw Ark ACG video URL. */
  arkVideoUrl?: string;
  /** Container-local file path. */
  localPath?: string;
  /** Seedance MA's summary of the generated video. */
  images?: SeedanceImage[];
  imageModel?: string;
  summary?: string;
  /** Error message if `outcome === "failed"`. */
  errorMessage?: string;
  /** Sequence number of the latest event updating this task. */
  seq: number;
}

function extractCliFlag(cmd: string, flagPattern: RegExp): string | undefined {
  const m = cmd.match(flagPattern);
  if (!m) return undefined;
  return (m[1] || m[2] || m[3] || "").trim() || undefined;
}

/**
 * Extract the last valid balanced JSON object from a CLI stdout string so earlier log lines
 * containing `{...}` do not break parsing.
 */
function parseJsonObjectFromText(raw: string): Record<string, unknown> | null {
  const trimmed = raw.trim();
  if (!trimmed) return null;
  try {
    const direct = JSON.parse(trimmed);
    if (direct && typeof direct === "object" && !Array.isArray(direct)) {
      return direct as Record<string, unknown>;
    }
  } catch {
    // Fall through to balanced block scan.
  }

  const blocks: string[] = [];
  for (let i = 0; i < trimmed.length; i++) {
    if (trimmed[i] !== "{") continue;
    let depth = 0;
    let inStr = false;
    let escaped = false;
    for (let j = i; j < trimmed.length; j++) {
      const ch = trimmed[j];
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
          blocks.push(trimmed.slice(i, j + 1));
          i = j;
          break;
        }
      }
    }
  }

  for (let k = blocks.length - 1; k >= 0; k--) {
    try {
      const parsed = JSON.parse(blocks[k]);
      if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
        return parsed as Record<string, unknown>;
      }
    } catch {
      // Try earlier block.
    }
  }
  return null;
}

export function isBrowserPlayableVideoUrl(url: string | undefined): url is string {
  if (!url) return false;
  const trimmed = url.trim();
  return trimmed.startsWith("https://") || trimmed.startsWith("http://");
}

function pickPlayableVideoUrl(
  videoUrl: string | undefined,
  arkVideoUrl: string | undefined,
): string | undefined {
  if (isBrowserPlayableVideoUrl(videoUrl)) return videoUrl;
  if (isBrowserPlayableVideoUrl(arkVideoUrl)) return arkVideoUrl;
  return undefined;
}

/**
 * Build ordered candidate video URLs for a Seedance task.
 *
 * Prioritizes the AMA backend raw file endpoint (`/api/runs/{runId}/sandbox/file?raw=1&path=...`)
 * when a container-local file exists, falling back to direct remote video URLs.
 */
export function rawMediaUrl(runId: string, localPath: string | undefined, download = false): string | undefined {
  // Older skill versions emitted relative paths. Without the executor workdir we must not guess a root.
  if (!runId.trim() || !localPath?.startsWith("/")) return undefined;
  const params = new URLSearchParams({ raw: "1", path: localPath });
  if (download) params.set("download", "1");
  return `${API_BASE}/runs/${encodeURIComponent(runId.trim())}/sandbox/file?${params.toString()}`;
}

export function buildSeedanceImageUrls(image: SeedanceImage, runId?: string): string[] {
  const urls: string[] = [];
  const add = (url: string | undefined) => {
    if (url && !urls.includes(url)) urls.push(url);
  };
  add(rawMediaUrl(runId || "", image.localPath));
  if (image.imageUrl && isBrowserPlayableVideoUrl(image.imageUrl)) add(image.imageUrl);
  if (image.arkImageUrl && isBrowserPlayableVideoUrl(image.arkImageUrl)) add(image.arkImageUrl);
  return urls;
}

export function buildSeedancePlayableUrls(
  task: SeedanceVideoTask,
  runId?: string,
): {
  sources: string[];
  downloadUrl?: string;
  externalUrl?: string;
} {
  const sources: string[] = [];
  const addUnique = (u: string | undefined) => {
    if (u && !sources.includes(u)) {
      sources.push(u);
    }
  };

  let sameOriginInlineUrl: string | undefined;
  let sameOriginDownloadUrl: string | undefined;
  const trimmedRunId = runId?.trim();
  if (trimmedRunId && task.outcome === "completed") {
    sameOriginInlineUrl = rawMediaUrl(trimmedRunId, task.localPath);
    sameOriginDownloadUrl = rawMediaUrl(trimmedRunId, task.localPath, true);
    if (sameOriginInlineUrl) addUnique(sameOriginInlineUrl);
  }

  if (isBrowserPlayableVideoUrl(task.videoUrl)) {
    addUnique(task.videoUrl);
  }
  if (isBrowserPlayableVideoUrl(task.arkVideoUrl)) {
    addUnique(task.arkVideoUrl);
  }

  return {
    sources,
    downloadUrl: sameOriginDownloadUrl || sources[0],
    externalUrl: sameOriginInlineUrl || sources[0],
  };
}

/**
 * Extract Seedance MA video creation tasks from a turn's timeline items.
 */
export function extractSeedanceTasksFromTimelineItems(
  items: TimelineItem[],
): SeedanceVideoTask[] {
  const bySession = new Map<string, SeedanceVideoTask>();
  let pendingGenerateTask: SeedanceVideoTask | null = null;

  for (let idx = 0; idx < items.length; idx++) {
    const item = items[idx];
    const itemSeq = item.sequence ?? idx;
    if (item.category !== "tool") continue;
    const cmdText = `${item.summary || ""}\n${item.body || ""}`;
    const isSeedanceTool =
      cmdText.includes("seedance_ma.py") || cmdText.includes("skills/seedance-ma");

    const promptArg = isSeedanceTool
      ? extractCliFlag(
          cmdText,
          /(?:--prompt|-p)\s+(?:"([^"]+)"|'([^']+)'|([^\s\\]+))/,
        )
      : undefined;
    const titleArg = isSeedanceTool
      ? extractCliFlag(
          cmdText,
          /(?:--title|-t)\s+(?:"([^"]+)"|'([^']+)'|([^\s\\]+))/,
        )
      : undefined;
    const ratioArg = isSeedanceTool
      ? extractCliFlag(
          cmdText,
          /(?:--ratio|-r)\s+(?:"([^"]+)"|'([^']+)'|([^\s\\]+))/,
        )
      : undefined;
    const durationArg = isSeedanceTool
      ? extractCliFlag(
          cmdText,
          /(?:--duration|-d)\s+(?:"([^"]+)"|'([^']+)'|(\d+))/,
        )
      : undefined;

    if (isSeedanceTool && /\bgenerate\b/.test(cmdText) && !item.result) {
      pendingGenerateTask = {
        kind: /\bgenerate-images\b/.test(cmdText) ? "image" : "video",
        sessionId: `pending-${itemSeq}`,
        title: titleArg || (promptArg ? promptArg.slice(0, 28) : undefined),
        userPrompt: promptArg,
        ratio: ratioArg || "16:9",
        duration: durationArg ? Number(durationArg) : 5,
        outcome: "in_progress",
        stage: "planning_storyboard",
        seq: itemSeq,
      };
    }

    if (!item.result?.text) continue;
    const parsed = parseJsonObjectFromText(item.result.text);
    if (!parsed) continue;

    const data =
      parsed.data && typeof parsed.data === "object" && !Array.isArray(parsed.data)
        ? (parsed.data as Record<string, unknown>)
        : null;
    if (!data) continue;

    const videoModel = typeof data.video_model === "string" ? data.video_model : "";
    const sessionId = typeof data.session_id === "string" ? data.session_id : "";
    if (!isSeedanceTool && !videoModel.toLowerCase().includes("seedance")) {
      continue;
    }
    if (!sessionId) continue;

    // Once a real session_id arrives from `generate`, clear any temporary pending placeholder.
    pendingGenerateTask = null;

    const prev = bySession.get(sessionId);
    const rawOutcome =
      typeof data.video_outcome === "string"
        ? data.video_outcome
        : typeof data.outcome === "string"
          ? data.outcome
          : "in_progress";
    const outcome: SeedanceVideoTask["outcome"] =
      rawOutcome === "completed"
        ? "completed"
        : rawOutcome === "failed"
          ? "failed"
          : "in_progress";

    const errObj =
      parsed.error && typeof parsed.error === "object" && !Array.isArray(parsed.error)
        ? (parsed.error as Record<string, unknown>)
        : null;
    const errorMessage =
      typeof errObj?.message === "string" ? errObj.message : prev?.errorMessage;

    const rawImages = Array.isArray(data.images) ? data.images : [];
    const images: SeedanceImage[] = rawImages.flatMap((value, index) => {
      if (!value || typeof value !== "object" || Array.isArray(value)) return [];
      const image = value as Record<string, unknown>;
      return [{
        index: typeof image.index === "number" ? image.index : index + 1,
        localPath: typeof image.local_path === "string" ? image.local_path : undefined,
        imageUrl: typeof image.image_url === "string" ? image.image_url : undefined,
        arkImageUrl: typeof image.ark_image_url === "string" ? image.ark_image_url : undefined,
      }];
    });

    const isImageTask = images.length > 0 || Array.isArray(data.images) || prev?.kind === "image" || /\bgenerate-images\b/.test(cmdText);

    const merged: SeedanceVideoTask = {
      kind: isImageTask ? "image" : "video",
      sessionId,
      title:
        (typeof data.title === "string" && data.title) ||
        titleArg ||
        prev?.title,
      userPrompt:
        (typeof data.user_prompt === "string" && data.user_prompt) ||
        promptArg ||
        prev?.userPrompt,
      storyboardPrompt:
        (typeof data.storyboard_prompt === "string" && data.storyboard_prompt) ||
        prev?.storyboardPrompt,
      ratio:
        (typeof data.ratio === "string" && data.ratio) ||
        ratioArg ||
        prev?.ratio ||
        "16:9",
      duration:
        isImageTask
          ? undefined
          : typeof data.duration === "number"
            ? data.duration
            : durationArg
              ? Number(durationArg)
              : prev?.duration || 5,
      videoModel: videoModel || prev?.videoModel,
      outcome,
      stage: (typeof data.stage === "string" && data.stage) || prev?.stage,
      stageLabel:
        (typeof data.stage_label === "string" && data.stage_label) || prev?.stageLabel,
      taskId: (typeof data.task_id === "string" && data.task_id) || prev?.taskId,
      cgtTaskId:
        (typeof data.cgt_task_id === "string" && data.cgt_task_id) || prev?.cgtTaskId,
      videoUrl: pickPlayableVideoUrl(
        (typeof data.video_url === "string" && data.video_url) || prev?.videoUrl,
        (typeof data.ark_video_url === "string" && data.ark_video_url) ||
          prev?.arkVideoUrl,
      ),
      arkVideoUrl:
        (typeof data.ark_video_url === "string" && data.ark_video_url) ||
        prev?.arkVideoUrl,
      localPath:
        (typeof data.local_path === "string" && data.local_path) || prev?.localPath,
      images: images.length > 0 ? images : prev?.images,
      imageModel: (typeof data.image_model === "string" && data.image_model) || prev?.imageModel,
      summary: (typeof data.summary === "string" && data.summary) || prev?.summary,
      errorMessage,
      seq: itemSeq,
    };

    bySession.set(sessionId, merged);
  }

  const list = Array.from(bySession.values());
  if (pendingGenerateTask && list.length === 0) {
    list.push(pendingGenerateTask);
  }
  return list.sort((a, b) => b.seq - a.seq);
}

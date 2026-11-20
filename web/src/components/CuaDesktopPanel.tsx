/**
 * Live CUA (Computer Use Agent / Cloud Desktop) renderer for AMA.
 *
 * Embeds the Volcengine Agent Plan CUA web interface (`full_interface_url` or `desktop_view_url`)
 * inside an interactive iframe, with mode switching, desktop selector, reload, and external tab link.
 */

import { useEffect, useState } from "react";
import { Icon } from "@iconify/react";
import { toEmbeddableCuaUrl, type CuaDesktopSession } from "@/lib/cua";
import { useT } from "@/prefs/PrefsContext";

export type CuaRenderMode = "full" | "desktop";

interface Props {
  sessions: CuaDesktopSession[];
  selectedDesktopId?: string;
  onSelectDesktop?: (desktopId: string) => void;
  defaultMode?: CuaRenderMode;
  onClose?: () => void;
  compact?: boolean;
}

export function CuaDesktopPanel({
  sessions,
  selectedDesktopId,
  onSelectDesktop,
  defaultMode = "desktop",
  onClose,
  compact = false,
}: Props) {
  const t = useT();
  const [internalDesktopId, setInternalDesktopId] = useState<string | undefined>(selectedDesktopId);
  const [mode, setMode] = useState<CuaRenderMode>(defaultMode);
  const [reloadNonce, setReloadNonce] = useState(0);

  useEffect(() => {
    if (selectedDesktopId) {
      setInternalDesktopId(selectedDesktopId);
    }
  }, [selectedDesktopId]);

  const activeSession =
    sessions.find((s) => s.desktopId === (selectedDesktopId ?? internalDesktopId)) ?? sessions[0];

  if (!activeSession) {
    return (
      <div className="flex h-full min-h-[360px] flex-col items-center justify-center r-container border border-border bg-surface px-6 py-12 text-center">
        <div className="mb-3 flex h-11 w-11 items-center justify-center r-inset border border-border bg-surface-secondary text-muted">
          <Icon icon="gravity-ui:display" className="size-5" />
        </div>
        <h3 className="text-sm font-medium text-foreground">{t("cua.empty.title")}</h3>
        <p className="mt-1.5 max-w-md text-xs leading-relaxed text-muted">
          {t("cua.empty.desc")}
        </p>
        <div className="mt-4 inline-flex items-center gap-2 r-row border border-border bg-surface-secondary px-3 py-1.5 font-mono text-xs text-muted">
          <Icon icon="gravity-ui:terminal" className="size-3.5 shrink-0" />
          <span>{t("cua.empty.cmdHint")}</span>
        </div>
      </div>
    );
  }

  const activeUrl =
    mode === "full"
      ? activeSession.fullInterfaceUrl || activeSession.desktopViewUrl
      : activeSession.desktopViewUrl || activeSession.fullInterfaceUrl;
  const baseEmbedUrl = toEmbeddableCuaUrl(activeUrl);
  const embedUrl =
    reloadNonce > 0 && baseEmbedUrl
      ? `${baseEmbedUrl}${baseEmbedUrl.includes("?") ? "&" : "?"}ama_reload=${reloadNonce}`
      : baseEmbedUrl;
  const externalUrl = toEmbeddableCuaUrl(activeSession.fullInterfaceUrl || activeUrl);

  const needsHuman = activeSession.outcome === "needs_human";

  const handleSelectDesktop = (id: string) => {
    setInternalDesktopId(id);
    onSelectDesktop?.(id);
  };

  return (
    <div
      className={`flex h-full flex-col overflow-hidden r-container border border-border bg-surface ${
        compact ? "min-h-[360px]" : "min-h-[560px]"
      }`}
      data-testid="cua-desktop-panel"
    >
      {/* Top toolbar */}
      <div className="flex flex-wrap items-center justify-between gap-2 border-b border-border bg-surface-secondary/50 px-3.5 py-2">
        <div className="flex min-w-0 items-center gap-2">
          <span className="inline-flex items-center gap-1.5 text-xs font-medium text-foreground">
            <Icon icon="gravity-ui:display" className="size-3.5 text-accent" />
            <span>{t("cua.title")}</span>
          </span>

          {/* Status badge */}
          <span
            className={`inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-[11px] font-medium ${
              needsHuman
                ? "bg-warning/15 text-warning"
                : "bg-success/15 text-success"
            }`}
          >
            <span
              className={`size-1.5 rounded-full ${
                needsHuman ? "bg-warning animate-pulse" : "bg-success"
              }`}
            />
            {needsHuman ? t("cua.badgeNeedsHuman") : t("cua.badgeLive")}
          </span>

          {/* Desktop selector or badge */}
          {sessions.length > 1 ? (
            <select
              value={activeSession.desktopId}
              onChange={(e) => handleSelectDesktop(e.target.value)}
              className="rounded border border-border bg-surface px-2 py-0.5 font-mono text-xs text-foreground focus:border-accent focus:outline-none"
            >
              {sessions.map((s) => (
                <option key={s.desktopId} value={s.desktopId}>
                  {t("cua.desktopLabel", { id: s.desktopId })}
                </option>
              ))}
            </select>
          ) : (
            <span className="truncate rounded bg-surface px-1.5 py-0.5 font-mono text-xs text-muted">
              {t("cua.desktopLabel", { id: activeSession.desktopId })}
            </span>
          )}

          {activeSession.taskId && !compact && (
            <span className="hidden truncate rounded bg-surface px-1.5 py-0.5 font-mono text-xs text-muted sm:inline">
              {t("cua.taskLabel", { id: activeSession.taskId })}
            </span>
          )}
        </div>

        <div className="flex items-center gap-1.5">
          {/* Mode toggle: Desktop View Only vs Full CUA App */}
          <div className="inline-flex rounded-md border border-border bg-surface p-0.5 text-xs">
            <button
              type="button"
              onClick={() => setMode("desktop")}
              className={`rounded px-2 py-0.5 transition-colors ${
                mode === "desktop"
                  ? "bg-surface-secondary font-medium text-foreground"
                  : "text-muted hover:text-foreground"
              }`}
            >
              {t("cua.modeDesktopOnly")}
            </button>
            <button
              type="button"
              onClick={() => setMode("full")}
              className={`rounded px-2 py-0.5 transition-colors ${
                mode === "full"
                  ? "bg-surface-secondary font-medium text-foreground"
                  : "text-muted hover:text-foreground"
              }`}
            >
              {t("cua.modeFull")}
            </button>
          </div>

          {/* Reload iframe */}
          <button
            type="button"
            onClick={() => setReloadNonce((n) => n + 1)}
            title={t("cua.reload")}
            aria-label={t("cua.reload")}
            className="inline-flex size-7 items-center justify-center rounded-md border border-border bg-surface text-muted transition-colors hover:bg-surface-secondary hover:text-foreground"
          >
            <Icon icon="gravity-ui:arrows-rotate-right" className="size-3.5" />
          </button>

          {/* Open in external browser tab */}
          <a
            href={externalUrl}
            target="_blank"
            rel="noopener noreferrer"
            className="inline-flex h-7 items-center gap-1 rounded-md border border-border bg-surface px-2 text-xs font-medium text-muted no-underline transition-colors hover:bg-surface-secondary hover:text-foreground"
          >
            <span>{t("cua.openExternal")}</span>
            <Icon icon="gravity-ui:arrow-up-right-from-square" className="size-3" />
          </a>

          {onClose && (
            <button
              type="button"
              onClick={onClose}
              title={t("cua.closeSplit")}
              aria-label={t("cua.closeSplit")}
              className="inline-flex size-7 items-center justify-center rounded-md border border-border bg-surface text-muted transition-colors hover:bg-surface-secondary hover:text-foreground"
            >
              <Icon icon="gravity-ui:xmark" className="size-3.5" />
            </button>
          )}
        </div>
      </div>

      {/* Needs-human banner */}
      {needsHuman && (
        <div className="flex items-center gap-2 border-b border-warning/30 bg-warning/10 px-3.5 py-2 text-xs text-warning">
          <Icon icon="gravity-ui:circle-info" className="size-3.5 shrink-0" />
          <span>{t("cua.needsHumanHint")}</span>
        </div>
      )}

      {/* Embedded CUA desktop iframe */}
      <div className="relative flex-1 bg-black/95">
        <iframe
          key={`${activeSession.desktopId}:${mode}:${embedUrl}:${reloadNonce}`}
          src={embedUrl}
          title={`${t("cua.title")} - ${activeSession.desktopId}`}
          className="h-full w-full border-0"
          allow="clipboard-read; clipboard-write; fullscreen; autoplay"
        />
      </div>

      {/* Footer note */}
      <div className="flex items-center justify-between gap-2 border-t border-border bg-surface-secondary/30 px-3.5 py-1.5 text-xs text-muted">
        <span className="truncate">{t("cua.ticketNotice")}</span>
        <span className="shrink-0 font-mono">{activeSession.desktopId}</span>
      </div>
    </div>
  );
}

import { useCallback, useState } from "react";
import { Icon } from "@iconify/react";
import { Link } from "react-router-dom";
import { api } from "@/data/api";
import { useWorkspace, useWorkspacePath } from "@/app/WorkspaceGate";
import { AsyncErrorBlock, Empty, ListSkeleton } from "@/components/Page";
import { ModelIcon } from "@/components/ModelIcon";
import { AzureButton } from "@/components/azure/atoms/Button";
import { AzureStatusPill } from "@/components/azure/atoms/StatusPill";
import { AzureChip } from "@/components/azure/atoms/Chip";
import { UserAvatar } from "@/components/UserAvatar";
import { useUserProfiles } from "@/lib/useUserProfiles";
import { useAuth } from "@/app/AuthGate";
import { formatRelative } from "@/lib/format";
import { useAsyncData } from "@/lib/useAsyncData";
import { usePrefs } from "@/prefs/PrefsContext";
import { isCodexAgent, modelSpec, type Agent } from "@/types/domain";

function AzureAgentCard({ agent }: { agent: Agent }) {
  const { t } = usePrefs();
  const wp = useWorkspacePath();
  const spec = modelSpec(agent.model.id);
  return (
    <Link
      to={wp(`/agents/${agent.id}`)}
      data-testid={`agent-card-${agent.id}`}
      className="group flex min-w-0 flex-col rounded-[10px] border border-line bg-surface p-4 text-ink no-underline shadow-hairline transition-[border-color,box-shadow] hover:border-line-strong hover:shadow-card"
    >
      <div className="flex items-start gap-3">
        <span className="flex size-9 shrink-0 items-center justify-center rounded-[8px] bg-accent-tint text-accent-ink">
          <Icon icon="gravity-ui:face-robot" className="size-4" />
        </span>
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-2">
            <h3 className="truncate text-[15px] font-semibold text-ink">{agent.name}</h3>
            {isCodexAgent(agent.baseAgent) ? <AzureChip tone="accent">{t("agent.baseAgentCodexBadge")}</AzureChip> : null}
          </div>
          <p className="mt-0.5 truncate text-[13px] text-ink-2">{agent.description}</p>
        </div>
        <AzureStatusPill tone={agent.status === "active" ? "green" : "neutral"}>
          {t(`enum.agentStatus.${agent.status}`)}
        </AzureStatusPill>
      </div>

      <p className="mt-4 line-clamp-2 min-h-10 text-[13px] leading-5 text-ink-2">{agent.system}</p>

      <div className="mt-4 flex flex-wrap items-center gap-1.5">
        <span className="inline-flex items-center gap-1.5 rounded-[6px] bg-inset px-2 py-1 text-[12px] text-ink-2">
          <ModelIcon vendor={spec.vendor} />
          {spec.label}
        </span>
        {agent.skillIds.length > 0 ? <AzureChip>{t("agent.skillCount", { count: agent.skillIds.length })}</AzureChip> : null}
        {agent.environmentType === "cloud" ? <AzureChip tone="accent">cloud</AzureChip> : null}
      </div>
      <div className="mt-4 flex items-center gap-3 border-t border-line pt-3 text-[12px] text-ink-3">
        <span className="num">v{agent.version}</span>
        <span className="num">{t("agent.toolCount", { count: agent.builtin_tools.length })}</span>
        <span className="ml-auto text-accent-ink opacity-0 transition-opacity group-hover:opacity-100">
          {t("common.open")} →
        </span>
      </div>
    </Link>
  );
}

/** Full Agents page for the Azure component implementation. */
export function AzureAgentsPage() {
  const { t, language } = usePrefs();
  const { workspace } = useWorkspace();
  const { user } = useAuth();
  const wp = useWorkspacePath();
  const [mineOnly, setMineOnly] = useState(false);
  const [query, setQuery] = useState("");
  const fetch = useCallback(() => api.listAgents(workspace.id), [workspace.id]);
  const { data: agents, error, isLoading, reload } = useAsyncData(fetch, t("common.loadFailedUnknown"));
  const profiles = useUserProfiles((agents ?? []).map((agent) => agent.createdBy));
  const filtered = (agents ?? []).filter((agent) => {
    const text = `${agent.name} ${agent.description} ${agent.system}`.toLowerCase();
    return text.includes(query.trim().toLowerCase()) && (!mineOnly || (agent.createdBy ?? "").toLowerCase() === (user?.email ?? "").toLowerCase());
  });
  const active = (agents ?? []).filter((agent) => agent.status === "active").length;

  if (isLoading) return <ListSkeleton rows={3} />;
  if (error) return <AsyncErrorBlock error={error} onRetry={reload} />;

  return (
    <div className="flex min-h-full flex-col gap-5 p-4 sm:p-6" data-testid="azure-agents-page">
      <header className="flex flex-wrap items-end justify-between gap-4">
        <div>
          <p className="text-[12px] font-medium text-ink-3">{workspace.name}</p>
          <h1 className="mt-1 text-[24px] font-semibold tracking-tight text-ink">Agents</h1>
          <p className="mt-1 text-[13px] text-ink-2">{t("agent.listDesc")}</p>
        </div>
        <Link to={wp("/agents/new")} className="no-underline">
          <AzureButton variant="accent" data-testid="new-agent">{t("agent.new")}</AzureButton>
        </Link>
      </header>

      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex items-center gap-4 text-[13px] text-ink-2">
          <span><strong className="num font-semibold text-ink">{agents?.length ?? 0}</strong> Agents</span>
          <span><strong className="num font-semibold text-ink">{active}</strong> {t("agent.activeSummary")}</span>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <label className="flex h-8 items-center gap-1.5 rounded-full bg-field px-3 text-ink-3 shadow-hairline focus-within:text-ink-2">
            <Icon icon="gravity-ui:magnifier" className="size-3.5" />
            <input
              value={query}
              onChange={(event) => setQuery(event.target.value)}
              placeholder={t("agent.searchPlaceholder")}
              aria-label={t("agent.searchPlaceholder")}
              className="w-44 bg-transparent text-[13px] text-ink outline-none placeholder:text-ink-3"
            />
          </label>
          <AzureButton
            variant={mineOnly ? "accent" : "secondary"}
            onClick={() => setMineOnly((current) => !current)}
            data-testid="filter-mine-only"
          >
            {t("agent.mineOnly")}
          </AzureButton>
        </div>
      </div>

      {filtered.length === 0 ? (
        <Empty
          icon="gravity-ui:face-robot"
          title={query || mineOnly ? t("agent.mineOnlyEmpty") : t("agent.emptyTitle")}
          description={t("agent.emptyDesc")}
          action={
            <Link to={wp("/agents/new")} className="no-underline">
              <AzureButton variant="accent">{t("agent.new")}</AzureButton>
            </Link>
          }
        />
      ) : (
        <div className="grid gap-3 md:grid-cols-2 xl:grid-cols-3">
          {filtered.map((agent) => <AzureAgentCard key={agent.id} agent={agent} />)}
        </div>
      )}

      <footer className="mt-auto flex items-center justify-between border-t border-line pt-4 text-[12px] text-ink-3">
        <span>{t("agent.updatedAt", {
          time: agents?.[0] ? formatRelative(agents[0].updatedAt, language) : "—",
        })}</span>
        <div className="flex -space-x-1">
          {(agents ?? []).slice(0, 3).map((agent) => (
            <UserAvatar
              key={agent.id}
              email={agent.createdBy}
              profile={profiles[(agent.createdBy ?? "").toLowerCase()]}
              label={t("agent.createdBy")}
            />
          ))}
        </div>
      </footer>
    </div>
  );
}

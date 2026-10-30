/**
 * Feishu app management.
 *
 * Bindings are **1:1 and exclusive** (see core.Service.ensureFeishuAppsFree): this page looks
 * from the "app" side at who is handling it; the agent detail page picks an app from the other side.
 *
 * Onboarding uses device-code self-registration (see FeishuConnectPage), so there is no
 * "enter credentials by hand" form here;
 * the primary button goes straight to the onboarding flow. The platform neither holds nor echoes the AppSecret.
 *
 * **Unbound must have a clickable entry**, not just a line of gray text: it is a state that needs
 * the user to act,
 * while bound actually has a link to follow (exactly backwards). The entry is an in-place Agent
 * picker; for the reason see
 * components/FeishuBindPrompt.tsx.
 */

import { useCallback, useState } from "react";
import { Button, Card, Chip, Tooltip } from "@heroui/react";
import { Icon } from "@iconify/react";
import { Link, useNavigate } from "react-router-dom";
import { api } from "@/data/api";
import { useWorkspace, useWorkspacePath } from "@/app/WorkspaceGate";
import { AsyncErrorBlock, Empty, ListSkeleton, PageBody, PageHeader } from "@/components/Page";
import { ConfirmModal } from "@/components/FormModal";
import { ReadinessCard } from "@/components/FeishuReadiness";
import { FeishuBindPrompt } from "@/components/FeishuBindPrompt";
import { formatRelative } from "@/lib/format";
import { useAsyncData } from "@/lib/useAsyncData";
import {
  FEISHU_STATUS_COLOR,
  type Agent,
  type FeishuApp,
  type FeishuAppStatus,
} from "@/types/domain";
import { usePrefs } from "@/prefs/PrefsContext";

function StatusChip({ status }: { status: FeishuAppStatus }) {
  const { t } = usePrefs();
  return (
    <Chip size="sm" color={FEISHU_STATUS_COLOR[status]} variant="soft">
      <Chip.Label>{t(`enum.feishuStatus.${status}`)}</Chip.Label>
    </Chip>
  );
}

export function FeishuPage() {
  const { t, language } = usePrefs();
  const { workspace } = useWorkspace();
  const wp = useWorkspacePath();
  const navigate = useNavigate();
  const [deleting, setDeleting] = useState<FeishuApp | null>(null);

  // Failures must be visible, not stuck on the skeleton (see the file header of lib/useAsyncData.ts).
  const fetch = useCallback(async () => {
    const [apps, agents] = await Promise.all([
      api.listFeishuApps(workspace.id),
      api.listAgents(workspace.id),
    ]);
    return { apps, agents };
  }, [workspace.id]);
  const { data, error, isLoading, reload: load, setData } = useAsyncData(
    fetch,
    t("common.loadFailedUnknown"),
  );
  const apps = data?.apps ?? null;
  const agents = data?.agents ?? [];

  /** Re-inspect and request-authorization both return the whole app; replace it in place, no need to reload the list. */
  function replaceApp(next: FeishuApp) {
    setData((prev) =>
      prev ? { ...prev, apps: prev.apps.map((a) => (a.id === next.id ? next : a)) } : prev,
    );
  }

  /**
   * Sync both sides after an in-place bind succeeds.
   *
   * Replacing only the agent is not enough: "who is handling it" on the card is looked up in
   * reverse via `agents.feishuAppIds`,
   * but 1:1 is **exclusive**, so binding pushes out the previous holder. Updating only this one
   * agent would leave the pushed-out one
   * still shown on another card until the user refreshes. So in the same batch of data we remove
   * other agents' references to this app.
   */
  function applyBinding(next: Agent) {
    setData((prev) =>
      prev
        ? {
            ...prev,
            agents: prev.agents.map((a) =>
              a.id === next.id
                ? next
                : { ...a, feishuAppIds: a.feishuAppIds.filter((id) => !next.feishuAppIds.includes(id)) },
            ),
          }
        : prev,
    );
  }

  const goConnect = () => navigate(wp("/feishu/connect"));

  return (
    <PageBody>
      <PageHeader
        title={t("nav.feishu")}
        description={t("feishu.listDesc")}
        actions={
          <Button onPress={goConnect} data-testid="new-feishu">
            {t("feishu.connect")}
          </Button>
        }
      />

      {isLoading ? (
        <ListSkeleton />
      ) : error ? (
        <AsyncErrorBlock error={error} onRetry={load} />
      ) : !apps ? null : apps.length === 0 ? (
        <Empty
          icon="gravity-ui:comment"
          title={t("feishu.emptyTitle")}
          description={t("feishu.emptyDesc")}
          action={<Button onPress={goConnect}>{t("feishu.connect")}</Button>}
        />
      ) : (
        <div className="flex flex-col gap-4">
          {apps.map((app) => {
            const bound = agents.filter((a) => a.feishuAppIds.includes(app.id));
            return (
              <Card key={app.id} data-testid={`feishu-card-${app.appId}`}>
                <Card.Content className="flex flex-col gap-5 pt-6">
                  <div className="flex items-start justify-between gap-4">
                    <div className="flex min-w-0 items-start gap-3">
                      <span className="flex size-9 shrink-0 items-center justify-center r-chip bg-surface-secondary">
                        <Icon icon="gravity-ui:comment" className="size-4 text-muted" />
                      </span>
                      <div className="flex min-w-0 flex-col gap-1">
                        <div className="flex items-center gap-2">
                          <p className="truncate font-medium">{app.name}</p>
                          <StatusChip status={app.status} />
                        </div>
                        <p className="truncate font-mono text-xs text-muted">{app.appId}</p>
                      </div>
                    </div>
                    <div className="flex shrink-0 items-center gap-1">
                      <Tooltip>
                        <Tooltip.Trigger>
                          <Button
                            variant="ghost"
                            isIconOnly
                            aria-label={t("feishu.deleteAction")}
                            onPress={() => setDeleting(app)}
                          >
                            <Icon icon="gravity-ui:trash-bin" className="size-4" />
                          </Button>
                        </Tooltip.Trigger>
                        <Tooltip.Content>{t("common.delete")}</Tooltip.Content>
                      </Tooltip>
                    </div>
                  </div>

                  {/* Readiness check: installed ≠ usable; say clearly what is missing. */}
                  <ReadinessCard
                    app={app}
                    onRefresh={async () => replaceApp(await api.refreshFeishuReadiness(app.id))}
                    onRequestAdmin={async () =>
                      replaceApp(await api.requestFeishuAdminAuthorization(app.id))
                    }
                  />

                  <div className="flex flex-col gap-2">
                    <p className="text-xs text-muted">{t("feishu.boundAgents")}</p>
                    {bound.length === 0 ? (
                      // Unbound: give a clickable entry, not a line of gray text. See the component file header.
                      <FeishuBindPrompt app={app} agents={agents} onBound={applyBinding} />
                    ) : (
                      <div className="flex flex-wrap gap-2" data-testid={`feishu-bound-${app.appId}`}>
                        {bound.map((a) => (
                          <Link
                            key={a.id}
                            to={wp(`/agents/${a.id}?tab=feishu`)}
                            className="no-underline"
                          >
                            <Chip variant="soft" size="sm">
                              <Chip.Label>{a.name}</Chip.Label>
                            </Chip>
                          </Link>
                        ))}
                      </div>
                    )}
                  </div>

                  <p className="text-xs text-muted">
                    {t("feishu.connectedAt", { time: formatRelative(app.createdAt, language) })}
                  </p>
                </Card.Content>
              </Card>
            );
          })}
        </div>
      )}

      <ConfirmModal
        isOpen={deleting !== null}
        onOpenChange={(open) => !open && setDeleting(null)}
        title={t("feishu.deleteTitle", { name: deleting?.name ?? "" })}
        description={
          deleting && deleting.boundAgentIds.length > 0
            ? t("feishu.deleteBound", { count: deleting.boundAgentIds.length })
            : t("feishu.deleteDesc")
        }
        onConfirm={async () => {
          if (!deleting) return;
          await api.deleteFeishuApp(deleting.id);
          setDeleting(null);
          await load();
        }}
      />
    </PageBody>
  );
}

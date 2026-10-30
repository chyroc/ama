/**
 * Readiness diagnostic card for the Feishu app.
 *
 * "Installed" does not mean "usable", so instead of one boolean we list each missing item separately;
 * what the user needs to do differs for each. The look follows the existing Ark credential block in workspace settings:
 * status uses a Chip + relative time, problems use r-inset soft-color warning bars, and "not checked" uses a neutral color
 * (it is not an error, just not tested yet).
 *
 * The key point: missingScopes (never requested; the user must add them on the open platform) and
 * unapprovedScopes (requested, waiting for admin approval) are rendered as two blocks, each with a different call to action.
 * Merging them into one makes users keep re-requesting permissions they already submitted.
 */

import { Button, Card, Chip } from "@heroui/react";
import { Icon } from "@iconify/react";
import { formatRelative } from "@/lib/format";
import { usePrefs } from "@/prefs/PrefsContext";
import type { FeishuApp, FeishuReadinessStatus } from "@/types/domain";

/** ready is the only "good" state; not ready uses warning, only unreachable is danger. */
const READINESS_COLOR: Record<FeishuReadinessStatus, "success" | "warning" | "danger"> = {
  ready: "success",
  setup_required: "warning",
  waiting_for_admin: "warning",
  runtime_unavailable: "danger",
};

/** One to-do: icon + explanation + the specific items.
 *
 * Exported so the trigger-mode picker can reuse the same warning style (see GroupScopeNotice in
 * AgentDetailPage): that notice and this diagnostic are the same kind of thing; writing another set would make the two
 * drift apart visually over time.
 */
export function Issue({
  tone,
  icon,
  title,
  items,
  hint,
  action,
  testId,
}: {
  tone: "warning" | "danger";
  icon: string;
  title: string;
  items?: string[];
  hint?: string;
  action?: React.ReactNode;
  /** For e2e to locate the whole warning block. Asserting "warning is absent" must anchor on the container;
   *  anchoring on a button inside becomes an always-green empty assertion, since the button itself may be conditionally rendered. */
  testId?: string;
}) {
  const color = tone === "danger" ? "text-danger" : "text-warning";
  const bg = tone === "danger" ? "bg-danger/5" : "bg-warning/5";
  return (
    <div className={`flex items-start gap-2 r-inset ${bg} px-3 py-2.5`} data-testid={testId}>
      <Icon icon={icon} className={`mt-0.5 size-4 shrink-0 ${color}`} />
      <div className="flex min-w-0 flex-1 flex-col gap-1.5">
        <p className={`text-sm ${color}`}>{title}</p>
        {hint ? <p className="text-xs text-muted">{hint}</p> : null}
        {items && items.length > 0 ? (
          <div className="flex flex-wrap gap-1.5">
            {items.map((item) => (
              <code key={item} className="r-chip bg-surface-secondary px-1.5 py-0.5 text-xs">
                {item}
              </code>
            ))}
          </div>
        ) : null}
        {action}
      </div>
    </div>
  );
}

export function ReadinessCard({
  app,
  onRefresh,
  onRequestAdmin,
}: {
  app: FeishuApp;
  onRefresh?: () => Promise<void>;
  onRequestAdmin?: () => Promise<void>;
}) {
  const { t, language } = usePrefs();
  const r = app.readiness;

  // Never inspected: use a neutral color, not warning. It is not a problem, just not tested yet.
  if (!r) {
    return (
      <div className="flex items-center gap-2">
        <Chip size="sm" variant="soft">
          <Chip.Label>{t("feishu.readinessUnchecked")}</Chip.Label>
        </Chip>
        {onRefresh ? (
          <Button variant="outline" size="sm" onPress={() => void onRefresh()}>
            {t("feishu.recheck")}
          </Button>
        ) : null}
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-3" data-testid={`readiness-${app.appId}`}>
      <div className="flex flex-wrap items-center gap-2">
        <Chip size="sm" variant="soft" color={READINESS_COLOR[r.status]}>
          <Chip.Label data-testid={`readiness-status-${app.appId}`}>
            {t(`enum.feishuReadiness.${r.status}`)}
          </Chip.Label>
        </Chip>
        <span className="text-xs text-muted">
          {t("feishu.checkedAt", { time: formatRelative(r.checkedAt, language) })}
        </span>
        {onRefresh ? (
          <Button
            variant="outline"
            size="sm"
            onPress={() => void onRefresh()}
            data-testid={`recheck-${app.appId}`}
          >
            {t("feishu.recheck")}
          </Button>
        ) : null}
      </div>

      {r.status === "ready" ? (
        <p className="text-sm text-muted">{t("feishu.readyHint")}</p>
      ) : null}

      {/* Permissions never requested: the user must add them on the open platform. */}
      {r.missingScopes.length > 0 ? (
        <Issue
          tone="warning"
          icon="gravity-ui:triangle-exclamation"
          title={t("feishu.missingScopes", { count: r.missingScopes.length })}
          hint={t("feishu.missingScopesHint")}
          items={r.missingScopes}
        />
      ) : null}

      {/* Requested, waiting for admin approval: all the user can do is nudge; submitting again does nothing. */}
      {r.unapprovedScopes.length > 0 ? (
        <Issue
          tone="warning"
          icon="gravity-ui:clock"
          title={t("feishu.unapprovedScopes", { count: r.unapprovedScopes.length })}
          hint={
            r.adminAuthorizationRequested
              ? t("feishu.unapprovedRequestedHint")
              : t("feishu.unapprovedScopesHint")
          }
          items={r.unapprovedScopes}
          action={
            onRequestAdmin && !r.adminAuthorizationRequested ? (
              <div className="pt-0.5">
                <Button
                  variant="outline"
                  size="sm"
                  onPress={() => void onRequestAdmin()}
                  data-testid={`request-admin-${app.appId}`}
                >
                  {t("feishu.requestAdmin")}
                </Button>
              </div>
            ) : null
          }
        />
      ) : null}

      {r.missingEvents.length > 0 ? (
        <Issue
          tone="warning"
          icon="gravity-ui:triangle-exclamation"
          title={t("feishu.missingEvents", { count: r.missingEvents.length })}
          hint={t("feishu.missingEventsHint")}
          items={r.missingEvents}
        />
      ) : null}

      {r.missingCallbacks.length > 0 ? (
        <Issue
          tone="warning"
          icon="gravity-ui:triangle-exclamation"
          title={t("feishu.missingCallbacks", { count: r.missingCallbacks.length })}
          hint={t("feishu.missingCallbacksHint")}
          items={r.missingCallbacks}
        />
      ) : null}

      {!r.botEnabled ? (
        <Issue tone="warning" icon="gravity-ui:face-robot" title={t("feishu.botDisabled")} hint={t("feishu.botDisabledHint")} />
      ) : null}

      {!r.published ? (
        <Issue
          tone="warning"
          icon="gravity-ui:rocket"
          title={t("feishu.notPublished")}
          hint={t("feishu.notPublishedHint")}
        />
      ) : null}

      {/* Unreachable usually means the credential is invalid; this is the one that should stand out most. */}
      {!r.runtimeConnected ? (
        <Issue
          tone="danger"
          icon="gravity-ui:plug-connection"
          title={r.lastRuntimeError || t("feishu.runtimeUnavailable")}
          hint={t("feishu.runtimeUnavailableHint")}
        />
      ) : null}

      {r.lastCheckError ? (
        <Issue tone="danger" icon="gravity-ui:circle-xmark" title={r.lastCheckError} />
      ) : null}

      {/* When the app config cannot be read, say clearly "the checks below are not reliable"; do not pretend we saw clearly. */}
      {!r.configurationInspectable ? (
        <Issue
          tone="warning"
          icon="gravity-ui:eye-slash"
          title={t("feishu.notInspectable")}
          hint={t("feishu.notInspectableHint")}
        />
      ) : null}
    </div>
  );
}

/** In the detail view, wrap the diagnostic in a card; in lists use ReadinessCard directly. */
export function ReadinessPanel({
  app,
  onRefresh,
  onRequestAdmin,
}: {
  app: FeishuApp;
  onRefresh?: () => Promise<void>;
  onRequestAdmin?: () => Promise<void>;
}) {
  const { t } = usePrefs();
  return (
    <Card>
      <Card.Header>
        <Card.Title>{t("feishu.readinessTitle")}</Card.Title>
        <Card.Description>{t("feishu.readinessDesc")}</Card.Description>
      </Card.Header>
      <Card.Content className="pt-2">
        <ReadinessCard app={app} onRefresh={onRefresh} onRequestAdmin={onRequestAdmin} />
      </Card.Content>
    </Card>
  );
}

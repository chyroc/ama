/**
 * The "Ark credentials" card on the Agent detail page: the most specific of the three config levels.
 *
 * # The three levels and their fallback order
 *
 *   agent level      has a record = custom, no record = follow the workspace (**there is no stored
 *                    "inherit" state**)
 *   workspace level  stores its own key, or references the account-level one
 *   account level    one per person
 *
 * **The more specific, the higher the priority.**
 *
 * # The only reason this card exists is to "say clearly which key is in use now"
 *
 * The most confusing thing about three config levels is "I configured three places, which one is
 * used". So whether or not this agent
 * has its own key, a **source bar** is always shown; agents that follow the workspace need it even
 * more:
 * without it the UI can only say "follows workspace settings", while the user wants to know which
 * key that really is and whether it works.
 *
 * The source bar's look **copies the one on the workspace settings page** (ark-source-inherit in
 * `SettingsPage.tsx`):
 * a neutral `bg-surface-secondary` background + `gravity-ui:link`.
 * ⚠️ **No badge, and no warning color**: it states a fact, it does not report good or bad;
 * warning would make people think "follows workspace" is a problem to fix.
 */

import { useCallback, useState } from "react";
import { Link } from "react-router-dom";
import { Button, Card, Chip, Skeleton } from "@heroui/react";
import { Icon } from "@iconify/react";
import { api } from "@/data/api";
import { AsyncErrorBlock, SectionTitle } from "@/components/Page";
import { useAsyncData } from "@/lib/useAsyncData";
import { formatRelative } from "@/lib/format";
import { usePrefs, useT } from "@/prefs/PrefsContext";
import {
  ARK_CHECK_KEYS,
  arkRegionLabel,
  type AgentArkCredential,
  type ArkCredentialCheck,
} from "@/types/domain";
import { ArkKeyPicker } from "@/components/ArkKeyPicker";

/** Colors for the verification state. Unverified uses a **neutral color**, not warning: it is not a problem, just not verified yet. */
const CHECK_COLOR: Record<ArkCredentialCheck, "default" | "success" | "danger"> = {
  unchecked: "default",
  valid: "success",
  invalid: "danger",
};

export function AgentArkCard({
  agentId,
  workspacePath,
}: {
  agentId: string;
  /** Used to build the "go to workspace settings" link. Workspace settings is a route **inside the workspace**; do not hard-code the site root path. */
  workspacePath: (to: string) => string;
}) {
  const { language } = usePrefs();
  const t = useT();
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const [picking, setPicking] = useState(false);
  // Transient note for when Ark could not verify on save (timeout/5xx) but it was let through.
  const [saveWarning, setSaveWarning] = useState<string | null>(null);

  const fetchCred = useCallback(() => api.getAgentArkCredential(agentId), [agentId]);
  // ⚠️ Use useAsyncData rather than a bare await: a reject on a bare await means setData never
  // runs,
  // and the card stays on the skeleton forever. See the file header of lib/useAsyncData.ts.
  const { data: cred, isLoading, error, reload, setData } = useAsyncData(
    fetchCred,
    t("agentArk.loadFailed"),
  );

  const run = async (fn: () => Promise<AgentArkCredential | void>) => {
    setBusy(true);
    setActionError(null);
    try {
      const next = await fn();
      if (next) setData(next);
      else reload();
    } catch (e) {
      setActionError(e instanceof Error ? e.message : t("agentArk.actionFailed"));
    } finally {
      setBusy(false);
    }
  };

  if (isLoading) {
    return (
      <Card>
        <Card.Content className="flex flex-col gap-4 pt-6">
          <Skeleton className="h-5 w-32" />
          <Skeleton className="h-16 w-full" />
        </Card.Content>
      </Card>
    );
  }
  if (error) {
    return (
      <Card>
        <Card.Content className="pt-6">
          <AsyncErrorBlock error={error} onRetry={reload} />
        </Card.Content>
      </Card>
    );
  }
  // isLoading is already handled above; cred may still be null here (first frame);
  // do not treat it as an error state, that would flash an error box.
  if (!cred) return null;

  const eff = cred.effective;
  // Three states, the same shape as the three states on the workspace settings page:
  //   custom      this agent has its own key
  //   following   follows the level above, and that level really has a key
  //   unconfigured none of the three levels is configured (then eff.level is an empty string)
  const custom = cred.custom;
  const configured = Boolean(eff.apiKeyMasked);
  const following = !custom && configured;

  return (
    <Card>
      <Card.Content className="flex flex-col gap-4 pt-6">
        <SectionTitle
          title={t("agentArk.title")}
          description={t("agentArk.description")}
          actions={
            custom ? (
              <div className="flex gap-2">
                <Button size="sm" variant="ghost" isDisabled={busy} onPress={() => setPicking(true)}>
                  {t("arkCred.replace")}
                </Button>
                {/* "Switch to workspace settings" = delete this agent's row, and resolution naturally falls back.
                    ⚠️ The text must not say "delete credentials": that would make people think the workspace key is deleted. */}
                <Button
                  size="sm"
                  variant="ghost"
                  isDisabled={busy}
                  data-testid="agent-ark-use-workspace"
                  onPress={() => run(() => api.deleteAgentArkCredential(agentId))}
                >
                  {t("agentArk.useWorkspace")}
                </Button>
              </div>
            ) : (
              <Button
                size="sm"
                variant="ghost"
                isDisabled={busy}
                data-testid="agent-ark-setup"
                onPress={() => setPicking(true)}
              >
                {t("agentArk.setup")}
              </Button>
            )
          }
        />

        {/*
          Source bar: **see at a glance where the current key comes from**.
          The only anchor of the three config levels in the UI; one sentence per source.
        */}
        {custom ? (
          <div
            className="flex flex-wrap items-center gap-x-2 gap-y-1 r-inset bg-surface-secondary px-3 py-2.5"
            data-testid="agent-ark-source-agent"
          >
            <Icon icon="gravity-ui:link" className="size-4 shrink-0 text-muted" />
            <p className="text-sm text-muted">
              {t("agentArk.sourceAgentBefore")}
              <strong className="font-medium text-foreground">
                {t("agentArk.sourceAgentStrong")}
              </strong>
              {t("agentArk.sourceAgentAfter")}
            </p>
          </div>
        ) : following ? (
          <div
            className="flex flex-wrap items-center gap-x-2 gap-y-1 r-inset bg-surface-secondary px-3 py-2.5"
            data-testid="agent-ark-source-inherited"
          >
            <Icon icon="gravity-ui:link" className="size-4 shrink-0 text-muted" />
            <p className="text-sm text-muted">
              {eff.level === "global" ? (
                <>
                  {t("agentArk.inheritGlobalBefore")}
                  <strong className="font-medium text-foreground">
                    {t("agentArk.inheritGlobalStrong")}
                  </strong>
                  {eff.ownerEmail ? t("agentArk.ownerEmail", { email: eff.ownerEmail }) : null}
                  {t("agentArk.inheritGlobalAfter")}
                </>
              ) : (
                <>
                  {t("agentArk.inheritWorkspaceBefore")}
                  <strong className="font-medium text-foreground">
                    {t("agentArk.inheritWorkspaceStrong")}
                  </strong>
                  {t("agentArk.inheritWorkspaceAfter")}
                </>
              )}
              {t("agentArk.inheritTail")}
            </p>
            <Link
              to={workspacePath("/settings?tab=ark")}
              className="text-sm text-accent no-underline hover:underline"
            >
              {t("chat.noArkAction")}
            </Link>
          </div>
        ) : (
          /* None of the three levels is configured: **the guide must point to workspace settings** (the most common place to configure),
             rather than making users think every agent needs its own key. */
          <div
            className="flex items-start gap-2 r-inset bg-warning/5 px-3 py-2.5"
            data-testid="agent-ark-source-missing"
          >
            <Icon
              icon="gravity-ui:triangle-exclamation"
              className="mt-0.5 size-4 shrink-0 text-warning"
            />
            <p className="text-sm text-warning">
              {t("agentArk.missingBefore")}
              <Link to={workspacePath("/settings?tab=ark")} className="text-warning underline">
                {t("agentArk.missingLink")}
              </Link>
              {t("agentArk.missingAfter")}
            </p>
          </div>
        )}

        {actionError ? (
          <p className="text-sm text-danger" role="alert" data-testid="agent-ark-error">
            {actionError}
          </p>
        ) : null}

        {saveWarning ? (
          <div
            className="flex items-start gap-2 r-inset bg-warning/5 px-3 py-2.5"
            role="status"
            data-testid="agent-ark-save-warning"
          >
            <Icon
              icon="gravity-ui:circle-info"
              className="mt-0.5 size-4 shrink-0 text-warning"
            />
            <p className="text-sm text-warning">{saveWarning}</p>
          </div>
        ) : null}

        {configured ? (
          <div className="grid gap-x-8 gap-y-4 sm:grid-cols-2">
            <div className="flex flex-col gap-1">
              <p className="text-xs text-muted">{t("agentArk.effectiveKey")}</p>
              <p className="font-mono text-sm tabular-nums" data-testid="agent-ark-masked">
                {eff.apiKeyMasked}
              </p>
            </div>
            <div className="flex flex-col gap-1">
              <p className="text-xs text-muted">{t("arkCred.status")}</p>
              <div className="flex items-center gap-2">
                <Chip size="sm" color={CHECK_COLOR[eff.check]} variant="soft">
                  {t(ARK_CHECK_KEYS[eff.check])}
                </Chip>
                <Button
                  size="sm"
                  variant="ghost"
                  isDisabled={busy}
                  data-testid="agent-ark-verify"
                  onPress={() => run(() => api.verifyAgentArkCredential(agentId))}
                >
                  {t("agentArk.verify")}
                </Button>
              </div>
            </div>
            <div className="flex flex-col gap-1">
              <p className="text-xs text-muted">{t("arkCred.region")}</p>
              <p className="text-sm">{arkRegionLabel(eff.region, t)}</p>
            </div>
            {custom && cred.updatedAt ? (
              <div className="flex flex-col gap-1">
                <p className="text-xs text-muted">{t("agentArk.updated")}</p>
                <p className="text-sm">
                  {formatRelative(cred.updatedAt, language)}
                  {cred.updatedBy ? ` · ${cred.updatedBy}` : null}
                </p>
              </div>
            ) : null}
          </div>
        ) : null}

        {cred.checkError ? (
          <p className="text-sm text-danger" data-testid="agent-ark-check-error">
            {cred.checkError}
          </p>
        ) : null}
      </Card.Content>

      {/* Dialog to set / change the key: type it by hand or pick one from the Volcengine account. */}
      <ArkKeyPicker
        isOpen={picking}
        onOpenChange={setPicking}
        title={custom ? t("agentArk.pickerReplaceTitle") : t("agentArk.pickerSetupTitle")}
        onSubmit={async (draft) => {
          // ArkKeyPicker handles the "not activated" second confirm itself; here we only write a
          // plain save.
          const next = await api.saveAgentArkCredential(agentId, draft);
          setData(next);
          setSaveWarning(next.warning?.message ?? null);
          return next;
        }}
      />
    </Card>
  );
}

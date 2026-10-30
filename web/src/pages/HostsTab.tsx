/**
 * The "Hosts" tab in global settings: which host executors I have registered.
 *
 * # It is an account-level resource, so it lives here
 *
 * Hosts are registered by users installing the daemon with their own API Key (AMA_TOKEN), **owned
 * by the registrant's
 * email**, and belong to no workspace (`GET /api/hosts` is ActionNone, with no
 * workspaceId in the path). Workspace settings only do "binding"; same reasoning as API Keys /
 * authorization management being account-level
 * resources. So it is in /preferences, a different scope from "Workspace settings → Hosts";
 * do not mix them (the Notice at the top of WorkspaceHostsTab explains the boundary).
 *
 * # The online flag changes on its own, so we poll
 *
 * `online` is **computed live** by the backend from the last 90 seconds of heartbeats: closing the
 * laptop, losing network, or the daemon exiting
 * flips it on its own, without going through amad. For the same reason session container state is
 * "not cached", this refetches every 12 seconds.
 * The polling follows useRegistrationPoll: recursive setTimeout (not setInterval,
 * which stacks requests on slow responses), and cancel on unmount.
 *
 * # The empty state is not "no data" but the entry for "how to connect"
 *
 * This feature has no other create form: machines register by typing two commands on their own
 * computer.
 * So when the list is empty the two-step guide is laid out right here (with copyable command
 * blocks), instead of just saying
 * "no hosts yet" and making people dig through docs.
 */

import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { Button, Card, Separator } from "@heroui/react";
import { Icon } from "@iconify/react";
import { api } from "@/data/api";
import { AsyncErrorBlock, Empty, ListSkeleton, SectionTitle } from "@/components/Page";
import { ConfirmModal } from "@/components/FormModal";
import { CopyButton } from "@/components/CopyButton";
import { formatRelative } from "@/lib/format";
import { useAsyncData } from "@/lib/useAsyncData";
import { preferencesPath } from "@/app/preferencesLink";
import { useOptionalWorkspace } from "@/app/WorkspaceGate";
import { usePrefs, useT } from "@/prefs/PrefsContext";
import type { Host } from "@/types/domain";

/** Polling interval. The online heartbeat expires at 90 seconds; refreshing every 12 seconds follows closely enough at almost no cost. */
const POLL_INTERVAL_MS = 12_000;

/**
 * Step 1: install the AMA CLI. How the open-source CLI is published is not decided yet, so build
 * it from source for now (needs Go 1.24+),
 * naming the output `ama` so it matches the command in step 2.
 */
const HOST_CLI_INSTALL_COMMAND =
  'git clone https://github.com/chyroc/ama && cd ama/server && go build -o "$(go env GOPATH)/bin/ama" ./cmd/ama-cli';

export function HostsTab() {
  const t = useT();
  const { language } = usePrefs();
  const ws = useOptionalWorkspace();

  // Empty dependency array: this list is unrelated to the workspace (account level), same as
  // ApiKeysTab.
  const fetchHosts = useCallback(() => api.listHosts(), []);
  const {
    data: hosts,
    error: loadError,
    isLoading,
    reload,
  } = useAsyncData(fetchHosts, t("hosts.loadFailed"));

  /**
   * Poll the online flag. **Only call reload, do not re-run useAsyncData's effect**: reload does
   * not clear
   * data (clearing only happens when the fetcher changes), so polling refreshes do not flash the
   * skeleton.
   * Recursive setTimeout: request time is unpredictable, and setInterval stacks requests on slow
   * responses.
   */
  useEffect(() => {
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const schedule = () => {
      timer = setTimeout(() => {
        void reload().finally(() => {
          if (!cancelled) schedule();
        });
      }, POLL_INTERVAL_MS);
    };
    schedule();
    return () => {
      cancelled = true;
      if (timer) clearTimeout(timer);
    };
  }, [reload]);

  const [removing, setRemoving] = useState<Host | null>(null);

  if (isLoading) {
    return (
      <Card>
        <Card.Content className="pt-6">
          <ListSkeleton rows={2} />
        </Card.Content>
      </Card>
    );
  }

  if (loadError) {
    return (
      <Card>
        <Card.Content className="pt-6">
          <AsyncErrorBlock error={loadError} onRetry={reload} />
        </Card.Content>
      </Card>
    );
  }

  if (hosts === null) return null;

  return (
    <div className="flex flex-col gap-4">
      <Card>
        <Card.Content className="flex flex-col gap-5 pt-6">
          <SectionTitle title={t("hosts.title")} description={t("hosts.desc")} />

          {hosts.length === 0 ? (
            <Empty
              icon="gravity-ui:cpu"
              title={t("hosts.emptyTitle")}
              description={t("hosts.emptyDesc")}
            />
          ) : (
            <div className="flex flex-col" data-testid="host-list">
              {hosts.map((h, i) => (
                <div key={h.id} data-testid="hosts-nonempty">
                  {i > 0 ? <Separator /> : null}
                  <HostRow
                    host={h}
                    language={language}
                    onRemove={() => setRemoving(h)}
                  />
                </div>
              ))}
            </div>
          )}
        </Card.Content>
      </Card>

      {/* Only the empty state shows the connect guide: with machines present this block is pure noise (the commands are typed only once at registration). */}
      {hosts.length === 0 ? (
        <HostOnboardingCard apikeysLink={preferencesPath({ workspaceKey: ws?.workspace.key, tab: "apikeys" })} />
      ) : null}

      <ConfirmModal
        isOpen={removing !== null}
        onOpenChange={(open) => {
          if (!open) setRemoving(null);
        }}
        title={t("hosts.deleteTitle", { name: removing?.name ?? "" })}
        description={t("hosts.deleteBody")}
        confirmLabel={t("hosts.delete")}
        onConfirm={async () => {
          if (!removing) return;
          await api.deleteHost(removing.id);
          setRemoving(null);
          await reload();
        }}
      />
    </div>
  );
}

/** One machine in the list: name / OS / online dot / daemon version / tool self-check / last heartbeat. */
function HostRow({
  host,
  language,
  onRemove,
}: {
  host: Host;
  language: string;
  onRemove: () => void;
}) {
  const t = useT();
  return (
    <div className="flex flex-col gap-2 py-3" data-testid={`host-row-${host.id}`}>
      <div className="flex items-center gap-3">
        <Icon icon="gravity-ui:cpu" className="size-4 shrink-0 text-muted" />
        <div className="flex min-w-0 flex-col gap-0.5">
          <p className="truncate text-sm font-medium">{host.name}</p>
          <p className="font-mono text-xs text-muted">
            {host.os}/{host.arch}
          </p>
        </div>
        <div className="ml-auto flex items-center gap-3">
          <HostOnline host={host} language={language} />
          <Button
            size="sm"
            variant="danger-soft"
            onPress={onRemove}
            data-testid={`host-delete-${host.id}`}
          >
            {t("hosts.delete")}
          </Button>
        </div>
      </div>

      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 pl-7 text-xs text-muted">
        {host.daemonVersion ? (
          <span className="font-mono" data-testid={`host-version-${host.id}`}>
            daemon {host.daemonVersion}
          </span>
        ) : null}
        {host.nativeToolsOk ? (
          <span data-testid={`host-native-ok-${host.id}`}>{t("hosts.nativeOk")}</span>
        ) : (
          <span className="text-warning" data-testid={`host-native-bad-${host.id}`}>
            {t("hosts.nativeBad")}
          </span>
        )}
      </div>

      {/* The daemon version is too old and does not support the native tool protocol: the server rejects tool calls outright;
          just prompt an upgrade (the new version no longer needs any brew install). */}
      {!host.nativeToolsOk ? (
        <div className="pl-7" data-testid={`host-native-bad-detail-${host.id}`}>
          <p className="text-xs text-warning">{t("hosts.nativeBadHint")}</p>
        </div>
      ) : null}
    </div>
  );
}

/** Online status: a small dot + text; when offline, add the last heartbeat as relative time. */
function HostOnline({ host, language }: { host: Host; language: string }) {
  const t = useT();
  return (
    <span
      className="flex items-center gap-1.5 text-xs text-muted"
      data-testid={`host-online-${host.id}`}
      data-online={host.online ? "1" : "0"}
    >
      {/* Semantic color: online is success, offline is neutral gray. Offline is normal (computer off), not a fault,
          so no danger. A small dot rather than a Chip; inline status should be light. */}
      <span
        className={`size-2 rounded-full ${host.online ? "bg-success" : "bg-muted"}`}
        aria-hidden
      />
      {host.online ? t("hosts.online") : t("hosts.offline")}
      {!host.online && host.lastSeenAt ? (
        <span className="text-muted">· {t("hosts.lastSeen", { time: formatRelative(host.lastSeenAt, language) })}</span>
      ) : null}
    </span>
  );
}

/**
 * Empty-state connect guide: two steps of commands, each with a copyable code block.
 *
 * apikeysLink points to the "API Key" tab on the same page (carrying from): the AMA_TOKEN needed in
 * step 2
 * can only be created there, and **its plain text is shown only once, at creation**; the text must
 * say this.
 * The single install command both registers and installs the service; AMA_TOKEN is used only once,
 * on first registration.
 */
function HostOnboardingCard({ apikeysLink }: { apikeysLink: string }) {
  const t = useT();
  return (
    <Card data-testid="hosts-empty">
      <Card.Content className="flex flex-col gap-5 pt-6">
        <SectionTitle title={t("hosts.guideTitle")} description={t("hosts.guideDesc")} />
        <ol className="flex flex-col gap-4" data-testid="hosts-onboarding">
          <GuideStep index={1} title={t("hosts.step1Title")} desc={t("hosts.step1Desc")}>
            <CommandBlock command={HOST_CLI_INSTALL_COMMAND} />
          </GuideStep>
          <GuideStep
            index={2}
            title={t("hosts.step2Title")}
            desc={t("hosts.step2Desc")}
            action={
              <Link
                to={apikeysLink}
                className="text-xs text-accent underline-offset-2 hover:underline"
                data-testid="hosts-apikeys-link"
              >
                {t("hosts.step2ApiKeyLink")}
              </Link>
            }
          >
            <CommandBlock command="AMA_TOKEN=amak_xxx ama daemon install" />
          </GuideStep>
        </ol>
      </Card.Content>
    </Card>
  );
}

/** One step in the guide: number + title/description + optional external link, with a command block below. */
function GuideStep({
  index,
  title,
  desc,
  action,
  children,
}: {
  index: number;
  title: string;
  desc: string;
  action?: React.ReactNode;
  children: React.ReactNode;
}) {
  return (
    <li className="flex flex-col gap-2">
      <div className="flex items-start gap-2.5">
        <span className="mt-0.5 flex size-5 shrink-0 items-center justify-center rounded-full bg-surface-secondary text-xs tabular-nums text-muted">
          {index}
        </span>
        <div className="flex min-w-0 flex-col gap-0.5">
          <p className="text-sm font-medium">{title}</p>
          <p className="text-xs text-muted">{desc}</p>
          {action ? <div className="mt-0.5">{action}</div> : null}
        </div>
      </div>
      <div className="pl-[30px]">{children}</div>
    </li>
  );
}

/**
 * One copyable command line: a monospace code block + copy button.
 *
 * Uses `bg-panel2` / `r-inset`, the same shape as the plain-text key in ApiKeysTab,
 * so users see the same "copyable credential/command" language in both tabs.
 */
function CommandBlock({ command }: { command: string }) {
  return (
    <div className="flex items-start gap-2">
      <code className="min-w-0 flex-1 break-all rounded-lg bg-panel2 px-3 py-2 font-mono text-xs">
        {command}
      </code>
      <CopyButton text={command} />
    </div>
  );
}

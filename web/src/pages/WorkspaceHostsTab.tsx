/**
 * The "Hosts" tab in workspace settings: which members' own machines this workspace's agents can
 * run on.
 *
 * # Two scopes
 *
 *   /preferences?tab=hosts    **account level**: machines I registered (the daemon registers with
 *                             my own
 *                             AMA_TOKEN, owned by the registrant's email)
 *   this tab                  **workspace level**: which of them are bound to this workspace
 *
 * v1 can only bind machines **you own** into a workspace (the backend POST checks the owner;
 * binding someone else's returns 422).
 * This is not a permission gap but a deliberate v1 boundary: the machines are personal computers,
 * and "binding someone else's computer into a
 * workspace for them" needs a separate confirmation mechanism. The Notice at the top must state
 * this limit, otherwise admins
 * who cannot find a colleague's machine will think the feature is broken.
 *
 * # A reference, not a copy; fetched fresh every time
 *
 * A machine's name / online flag / tool self-check are live values on the host row (GET returns
 * the join result);
 * if the machine is deleted or renamed in global settings, a refresh here follows. Do not cache
 * them in local state.
 */

import { useCallback, useMemo, useState } from "react";
import { Button, Card, Chip, Description, Label, ListBox, Select, Separator } from "@heroui/react";
import { Icon } from "@iconify/react";
import { api } from "@/data/api";
import { preferencesPath } from "@/app/preferencesLink";
import { AsyncErrorBlock, Empty, ListSkeleton, Notice, SectionTitle } from "@/components/Page";
import { ConfirmModal } from "@/components/FormModal";
import { UserAvatar, displayNameOf, profileKey } from "@/components/UserAvatar";
import { useUserProfiles } from "@/lib/useUserProfiles";
import { formatRelative } from "@/lib/format";
import { useAsyncData } from "@/lib/useAsyncData";
import { usePrefs, useT } from "@/prefs/PrefsContext";
import type { Host, UserProfile, WorkspaceHost } from "@/types/domain";

export function WorkspaceHostsTab({
  workspaceId,
  workspaceKey,
}: {
  workspaceId: string;
  workspaceKey: string;
}) {
  const t = useT();
  const { language } = usePrefs();
  const [removing, setRemoving] = useState<WorkspaceHost | null>(null);

  const fetchBound = useCallback(() => api.listWorkspaceHosts(workspaceId), [workspaceId]);
  const {
    data: bound,
    error: loadError,
    isLoading,
    reload: reloadBound,
  } = useAsyncData(fetchBound, t("hosts.loadFailed"));

  // Look up profiles for registrants and binders once (deduped in the hook); the two avatars in a
  // row share this table.
  const profiles = useUserProfiles([
    ...(bound ?? []).map((h) => h.ownerEmail),
    ...(bound ?? []).map((h) => h.addedByEmail),
  ]);

  // Candidates come from the **account-level** list (all machines I registered); bound ones are
  // filtered out below.
  const fetchMine = useCallback(() => api.listHosts(), []);
  const { data: mine, reload: reloadMine } = useAsyncData(fetchMine, t("hosts.loadFailed"));

  const reloadBoth = useCallback(async () => {
    await Promise.all([reloadBound(), reloadMine()]);
  }, [reloadBound, reloadMine]);

  /**
   * Unbind. ConfirmModal manages error/pending itself;
   * here we only throw, and do not keep our own copy of the error.
   */
  const onRemove = async () => {
    if (!removing) return;
    await api.unbindWorkspaceHost(workspaceId, removing.id);
    setRemoving(null);
    await reloadBoth();
  };

  const boundIds = useMemo(() => new Set((bound ?? []).map((h) => h.id)), [bound]);
  const candidates = (mine ?? []).filter((h) => !boundIds.has(h.id));
  // Not a single machine (not "all already bound"): guide the user to global settings.
  const noHostsAtAll = (mine?.length ?? 0) === 0;

  return (
    <div className="flex flex-col gap-4">
      {/*
        This Notice explains the v1 boundary: you can only bind your own machines, where machines
        come from,
        and where to register. Without it, admins facing a dropdown with no colleagues' machines
        will think it is a bug.
      */}
      <Notice
        tone="info"
        icon="gravity-ui:cpu"
        title={t("wsHosts.scopeTitle")}
        hint={t("wsHosts.scopeBody")}
        action={
          <Button
            variant="outline"
            size="sm"
            // Carry workspaceKey so global settings can lead back to this workspace (same as the
            // integrations tab).
            onPress={() => {
              window.location.assign(
                preferencesPath({ workspaceKey, tab: "hosts" }),
              );
            }}
          >
            <Icon icon="gravity-ui:arrow-up-right-from-square" className="size-4" />
            {t("wsHosts.gotoPrefs")}
          </Button>
        }
        data-testid="ws-hosts-scope"
      />

      <SectionTitle title={t("wsHosts.title")} description={t("wsHosts.desc")} />

      <Card>
        <Card.Content className="pt-6">
          {isLoading ? (
            <ListSkeleton rows={2} />
          ) : loadError ? (
            <AsyncErrorBlock error={loadError} onRetry={reloadBound} />
          ) : (bound ?? []).length === 0 ? (
            <Empty
              icon="gravity-ui:cpu"
              title={t("wsHosts.empty")}
              description={t("wsHosts.emptyHint")}
            />
          ) : (
            <div className="flex flex-col" data-testid="ws-host-list">
              {(bound ?? []).map((h, i) => (
                <div key={h.id}>
                  {i > 0 ? <Separator /> : null}
                  <BoundHostRow
                    host={h}
                    profiles={profiles}
                    language={language}
                    onRemove={() => setRemoving(h)}
                  />
                </div>
              ))}
            </div>
          )}
        </Card.Content>
      </Card>

      <AddBindingCard
        workspaceId={workspaceId}
        workspaceKey={workspaceKey}
        candidates={candidates}
        noHostsAtAll={noHostsAtAll}
        onBound={reloadBoth}
      />

      <ConfirmModal
        isOpen={removing !== null}
        onOpenChange={(open) => {
          if (!open) setRemoving(null);
        }}
        title={t("wsHosts.removeTitle", { name: removing?.name ?? "" })}
        // ⚠️ Must say clearly "the machine itself is not deleted": unbinding only cuts its link to
        // this workspace;
        // the machine is still in global settings and can still be bound by other workspaces (same
        // wording rule as unbinding integrations).
        description={t("wsHosts.removeBody")}
        confirmLabel={t("wsHosts.remove")}
        onConfirm={onRemove}
      />
    </div>
  );
}

/**
 * One row of the bound list: online dot + name + OS, with a line below marking "who registered it,
 * who bound it in",
 * and unbind at the end of the row.
 *
 * Registrant (ownerEmail) and binder (addedByEmail) are labeled separately on purpose: v1 only
 * allows binding your own,
 * so they are usually the same, but they diverge once an admin rebinds after an unbind; old
 * bindings did not record added_by,
 * so the binder shows "not recorded" as plain text, not guessed to be the machine owner.
 */
function BoundHostRow({
  host,
  profiles,
  language,
  onRemove,
}: {
  host: WorkspaceHost;
  profiles: Record<string, UserProfile>;
  language: string;
  onRemove: () => void;
}) {
  const t = useT();
  return (
    <div
      className="flex flex-col gap-2 py-3"
      data-testid={`ws-host-${host.id}`}
    >
      <div className="flex items-center gap-3">
        <Icon icon="gravity-ui:cpu" className="size-4 shrink-0 text-muted" />
        <span
          className={`size-2 shrink-0 rounded-full ${host.online ? "bg-success" : "bg-muted"}`}
          aria-hidden
        />
        <div className="flex min-w-0 flex-col gap-0.5">
          <p className="truncate text-sm font-medium">{host.name}</p>
          <p className="font-mono text-xs text-muted">
            {host.os}/{host.arch}
          </p>
        </div>
        <span className="ml-auto shrink-0 text-xs text-muted">
          {host.online ? t("hosts.online") : t("hosts.offline")}
        </span>
        <Button variant="danger-soft" size="sm" onPress={onRemove}>
          {t("wsHosts.remove")}
        </Button>
      </div>

      <div className="flex flex-wrap items-center gap-x-4 gap-y-1 pl-7 text-xs text-muted">
        <PersonTag
          label={t("wsHosts.registeredBy")}
          email={host.ownerEmail}
          profile={profiles[profileKey(host.ownerEmail)]}
          testId={`ws-host-owner-${host.id}`}
        />
        <span className="flex items-center gap-1.5" data-testid={`ws-host-binder-${host.id}`}>
          <PersonTag
            label={t("wsHosts.boundBy")}
            email={host.addedByEmail}
            profile={profiles[profileKey(host.addedByEmail)]}
          />
          {host.boundAt ? (
            <span className="text-muted">· {formatRelative(host.boundAt, language)}</span>
          ) : null}
        </span>
      </div>
    </div>
  );
}

/** An inline block of "role label + avatar + name".
 *
 * When the email is empty (old bindings did not record added_by), UserAvatar is not rendered: its
 * question-mark avatar tooltip
 * hard-codes "creator not recorded", which is the wrong wording for a binder; here we just give
 * "not recorded".
 * Not guessed to be the machine owner: the registrant and binder may differ.
 */
function PersonTag({
  label,
  email,
  profile,
  testId,
}: {
  label: string;
  email?: string;
  profile?: UserProfile;
  testId?: string;
}) {
  const t = useT();
  const trimmed = (email ?? "").trim();
  return (
    <span className="flex items-center gap-1.5" data-testid={testId}>
      <span className="text-muted">{label}</span>
      {trimmed ? (
        <>
          <UserAvatar email={trimmed} profile={profile} sizeClass="size-5" label={label} />
          <span className="max-w-40 truncate text-foreground">
            {displayNameOf(trimmed, profile)}
          </span>
        </>
      ) : (
        <span className="text-muted">{t("wsHosts.notRecorded")}</span>
      )}
    </span>
  );
}

/**
 * "Add binding" at the bottom: the account-level host list with bound ones filtered out; pick one
 * + bind.
 *
 * When there are no hosts at all the picker is not rendered; it is replaced by a guide to global
 * settings. A dropdown
 * with no options looks like a load failure.
 */
function AddBindingCard({
  workspaceId,
  workspaceKey,
  candidates,
  noHostsAtAll,
  onBound,
}: {
  workspaceId: string;
  workspaceKey: string;
  candidates: Host[];
  noHostsAtAll: boolean;
  onBound: () => Promise<void>;
}) {
  const t = useT();
  const [hostId, setHostId] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const bind = async () => {
    if (!hostId || busy) return;
    setBusy(true);
    setError(null);
    try {
      await api.bindWorkspaceHost(workspaceId, hostId);
      setHostId("");
      await onBound();
    } catch (e) {
      // The backend's 422 already says "you can only bind machines you registered"; show it as is.
      setError(e instanceof Error ? e.message : t("common.actionFailed"));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card>
      <Card.Content className="flex flex-col gap-4 pt-6" data-testid="ws-host-add">
        <SectionTitle title={t("wsHosts.addTitle")} description={t("wsHosts.addDesc")} />

        {noHostsAtAll ? (
          <Notice
            tone="info"
            icon="gravity-ui:cpu"
            title={t("wsHosts.noneTitle")}
            hint={t("wsHosts.noneHint")}
            action={
              <Button
                variant="outline"
                size="sm"
                onPress={() => {
                  window.location.assign(preferencesPath({ workspaceKey, tab: "hosts" }));
                }}
              >
                <Icon icon="gravity-ui:arrow-up-right-from-square" className="size-4" />
                {t("wsHosts.gotoPrefs")}
              </Button>
            }
            data-testid="ws-hosts-none"
          />
        ) : candidates.length === 0 ? (
          // There are machines but all are already bound to this workspace: not an error, and no
          // empty dropdown either.
          <p className="text-sm text-muted" data-testid="ws-hosts-all-bound">
            {t("wsHosts.allBound")}
          </p>
        ) : (
          <div className="flex flex-col gap-2">
            <div className="flex flex-wrap items-end gap-2">
              <Select
                className="min-w-56 flex-1"
                selectedKey={hostId || null}
                onSelectionChange={(k) => {
                  setHostId(String(k ?? ""));
                  setError(null);
                }}
                // ⚠️ **Must be given explicitly**: when nothing is selected HeroUI's built-in
                // placeholder is English
                // 'Select an item'; without it an English phrase pops up on a Chinese page
                // (typecheck cannot catch it).
                placeholder={t("wsHosts.pickHost")}
                data-testid="ws-host-select"
              >
                <Label>{t("wsHosts.pickHost")}</Label>
                <Select.Trigger data-testid="ws-host-trigger">
                  <Select.Value />
                  <Select.Indicator />
                </Select.Trigger>
                <Select.Popover>
                  <ListBox>
                    {candidates.map((h) => (
                      <ListBox.Item
                        key={h.id}
                        id={h.id}
                        textValue={`${h.name} ${h.os}/${h.arch} ${
                          h.online ? t("hosts.online") : t("hosts.offline")
                        }`}
                      >
                        <span
                          className={`size-2 shrink-0 rounded-full ${h.online ? "bg-success" : "bg-muted"}`}
                          aria-hidden
                        />
                        <Label>{h.name}</Label>
                        <Description>
                          <span className="font-mono">
                            {h.os}/{h.arch}
                          </span>
                        </Description>
                        <Chip size="sm" variant="soft" color={h.online ? "success" : "default"}>
                          <Chip.Label>
                            {h.online ? t("hosts.online") : t("hosts.offline")}
                          </Chip.Label>
                        </Chip>
                        <ListBox.ItemIndicator />
                      </ListBox.Item>
                    ))}
                  </ListBox>
                </Select.Popover>
              </Select>
              <Button
                onPress={() => void bind()}
                isPending={busy}
                isDisabled={hostId === ""}
                data-testid="ws-host-bind"
              >
                <Icon icon="gravity-ui:plus" className="size-4" />
                {t("wsHosts.bind")}
              </Button>
            </div>
            {error ? (
              <p className="text-xs text-danger" role="alert" data-testid="ws-host-bind-error">
                {error}
              </p>
            ) : null}
          </div>
        )}
      </Card.Content>
    </Card>
  );
}

/**
 * Workspace settings: basic info (name / key), members, Ark credentials, danger zone.
 *
 * The key is part of the URL; changing it breaks old links, so a separate warning is shown and
 * after saving it jumps to the new address.
 *
 * **Members is a tab here** (`?tab=members`), not a top-level sidebar item. It was once its own
 * page,
 * but then both the sidebar and the tab bar had an entry pointing to the same place, with their
 * highlights fighting each other. If only one stays,
 * it belongs in settings: members answers "what is this workspace", the same kind as basic info,
 * so it sits right next to it.
 *
 * # ✅ This page now has only one scope
 *
 * The four tabs (basic info / members / Ark credentials / danger zone) **are all workspace-level**:
 * changes affect
 * everyone in this workspace. The page title is "Workspace settings", so the default expectation is
 * workspace-level, and now the content
 * matches that expectation; no marker is needed to correct it anymore.
 *
 * Account-level tabs used to be mixed in here, and that was **the only trap in this page's
 * history**: a user creates a new workspace,
 * opens "Authorization management", immediately sees their own authorization records, and
 * reasonably suspects "is this a permission leak, is data
 * crossing between workspaces". The backend was always clean (fetched by actor, no workspaceId in
 * the path, pinned by tests);
 * **the location was wrong**. The fix back then was a "personal" Chip on account-level tabs,
 * which was **text making up for
 * the wrong location**; now both account-level tabs have moved, and the location itself says the
 * right thing.
 *
 * # Both account-level tabs have moved to "Global settings"
 *
 *   API Key                   → `/preferences?tab=apikeys` (`auth_api_keys` has no workspace_id
 *                               column)
 *   Authorization management  → `/preferences?tab=integrations` (the four `/api/integrations`
 *                               routes fetch
 *                               by actor, with no workspaceId in the path at all; see the
 *                               reasoning in authz/matrix.go)
 *
 * ⚠️ **The old conclusion "authorization management and Ark credentials are two forms of the same
 * thing, splitting them is harder to understand" is withdrawn.**
 * It was the reason for keeping authorization management **on** this page, at the cost of one
 * account-level tab mixed among four workspace-level
 * ones. The real tradeoff: the understanding gained from that "adjacency" did not outweigh the
 * misunderstanding caused by "mixed scopes";
 * the misunderstanding really happened, while the gain was only a guess. Besides, the pair is
 * **still adjacent** in the new home
 * (in global settings Ark credentials sit right next to authorization management), and there both
 * have the same scope, so adjacency is no longer a trap.
 *
 * The Ark credentials on this page are not left isolated by this: its own card says "workspace-level:
 * all members of this workspace
 * share one key, and replacing it affects everyone" (see ArkTab), a sentence that never depended on
 * its neighbors.
 *
 * The old addresses `?tab=apikeys` and `?tab=integrations` are caught by the two redirects below;
 * both addresses
 * really were used out in the wild.
 */

import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { Link, useNavigate, useSearchParams, Navigate } from "react-router-dom";
import {
  Avatar,
  Button,
  Card,
  Chip,
  Description,
  FieldError,
  Input,
  Label,
  ListBox,
  Select,
  Separator,
  TextArea,
  TextField,
  Tooltip,
} from "@heroui/react";
import { Icon } from "@iconify/react";
import { api } from "@/data/api";
import { useWorkspace } from "@/app/WorkspaceGate";
import { preferencesPath } from "@/app/preferencesLink";
import {
  AsyncErrorBlock,
  Empty,
  ListSkeleton,
  PageBody,
  PageHeader,
  SectionTitle,
} from "@/components/Page";
import { ConfirmModal, FormModal } from "@/components/FormModal";
import { ArkNotActivatedDialog } from "@/components/ArkNotActivatedDialog";
import {
  ArkFetchPicker,
  resolveArkSubmission,
  type ArkKeySource,
} from "@/components/ArkFetchPicker";
import { RolePermissionTable } from "@/components/RolePermissionTable";
import { WorkspaceHostsTab } from "@/pages/WorkspaceHostsTab";
import { formatRelative } from "@/lib/format";
import { useAsyncData } from "@/lib/useAsyncData";
import {
  isArkSaveCancelled,
  saveArkCredentialWithConfirm,
} from "@/lib/arkSave";
import {
  ARK_CHECK_KEYS,
  ARK_REGION_KEYS,
  ARK_REGIONS,
  type ArkKeyRef,
  validateWorkspaceKey,
  type ArkCredentialCheck,
  type ArkCredentialDraft,
  type ArkRegion,
  type WorkspaceMember,
  type WorkspaceRole,
} from "@/types/domain";
import { usePrefs, useT } from "@/prefs/PrefsContext";
import type { MessageKey } from "@/prefs/messages";

/**
 * Tab definitions. **All four are workspace-level**; the two account-level ones (API Key,
 * authorization management) moved to
 * global settings; see the file header. So there is no `personal` flag here anymore, and no
 * "personal" Chip is rendered:
 * with nothing to tell apart, a marker only makes people look for what it is being compared to.
 */
const TABS: readonly { id: string; labelKey: MessageKey }[] = [
  { id: "general", labelKey: "settings.tabGeneral" },
  // Members sit right next to basic info: both answer "what is this workspace".
  { id: "members", labelKey: "settings.tabMembers" },
  // Hosts are a workspace-level binding ("which machines this workspace's agents can run on").
  { id: "hosts", labelKey: "settings.tabHosts" },
  // Ark credentials are **workspace-level** (ark_secrets has one row per workspace; replacing it
  // affects all agents in this workspace),
  // the same scope as the other tabs on this page. Its account-level form (configured once
  // globally, referenced by workspaces) and "authorization management"
  // are both in /preferences.
  { id: "ark", labelKey: "settings.tabArk" },
  { id: "danger", labelKey: "settings.tabDanger" },
];

const ROLE_ORDER: WorkspaceRole[] = ["owner", "admin", "member", "viewer"];

/**
 * Renders a translated sentence and wraps each `/w/{name}` path in a <code>.
 * The sentence stays one translation key, so word order can differ per language.
 */
function withCodePaths(text: string, paths: Record<string, string>, codeClassName?: string) {
  return text.split(/(\/w\/\{\w+\})/).map((part, i) => {
    const m = /^\/w\/\{(\w+)\}$/.exec(part);
    return m ? (
      <code key={i} className={codeClassName}>
        /w/{paths[m[1]]}
      </code>
    ) : (
      part
    );
  });
}

/** Unverified uses a neutral color: it is not an error, just "not tested yet"; warning would falsely report a problem. */
const CHECK_COLOR: Record<ArkCredentialCheck, "success" | "danger" | "default"> = {
  valid: "success",
  invalid: "danger",
  unchecked: "default",
};

function GeneralTab() {
  const { workspace, refresh } = useWorkspace();
  const navigate = useNavigate();
  const t = useT();
  const [name, setName] = useState(workspace.name);
  const [key, setKey] = useState(workspace.key);
  const [description, setDescription] = useState(workspace.description ?? "");
  const [keyError, setKeyError] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);

  // After switching workspace, sync the form over
  useEffect(() => {
    setName(workspace.name);
    setKey(workspace.key);
    setDescription(workspace.description ?? "");
  }, [workspace]);

  const keyChanged = key !== workspace.key;
  const dirty =
    name !== workspace.name || keyChanged || description !== (workspace.description ?? "");

  async function save() {
    if (!name.trim()) {
      setError(t("settings.wsNameRequired"));
      return;
    }
    const ke = validateWorkspaceKey(key, t);
    if (ke) {
      setKeyError(ke);
      return;
    }
    setSaving(true);
    setError(null);
    setKeyError(null);
    try {
      if (keyChanged && (await api.isWorkspaceKeyTaken(key, workspace.id))) {
        setKeyError(t("settings.keyTaken", { key }));
        return;
      }
      await api.updateWorkspace(workspace.id, {
        name: name.trim(),
        key,
        description: description.trim() || undefined,
      });
      if (keyChanged) {
        // The key changed, so the current URL points at a workspace that no longer exists; we must
        // jump to the new address
        navigate(`/w/${key}/settings?tab=general`, { replace: true });
      } else {
        await refresh();
        setSaved(true);
        setTimeout(() => setSaved(false), 2400);
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : t("settings.saveFailed"));
    } finally {
      setSaving(false);
    }
  }

  return (
    <Card>
      <Card.Content className="flex flex-col gap-5 pt-6">
        <SectionTitle title={t("settings.tabGeneral")} />

        <TextField isRequired>
          <Label>{t("settings.wsName")}</Label>
          <Input
            value={name}
            onChange={(e) => setName(e.target.value)}
            data-testid="settings-name"
          />
        </TextField>

        <TextField isRequired isInvalid={Boolean(keyError)}>
          <Label>{t("settings.wsKey")}</Label>
          <Input
            value={key}
            onChange={(e) => {
              setKey(e.target.value);
              setKeyError(validateWorkspaceKey(e.target.value, t));
            }}
            data-testid="settings-key"
          />
          <Description>
            {withCodePaths(t("settings.currentUrl"), { key: workspace.key }, "text-xs")}
          </Description>
          {keyError ? <FieldError>{keyError}</FieldError> : null}
        </TextField>

        {keyChanged && !keyError ? (
          <div className="flex items-start gap-2 r-inset bg-warning/5 px-3 py-2.5">
            <Icon
              icon="gravity-ui:triangle-exclamation"
              className="mt-0.5 size-4 shrink-0 text-warning"
            />
            <p className="text-sm text-warning">
              {withCodePaths(t("settings.keyChangeWarn"), { old: workspace.key, next: key })}
            </p>
          </div>
        ) : null}

        <TextField>
          <Label>{t("settings.wsDesc")}</Label>
          <TextArea
            rows={2}
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            data-testid="settings-desc"
          />
        </TextField>

        {error ? (
          <p className="text-sm text-danger" role="alert">
            {error}
          </p>
        ) : null}

        <Separator />

        <div className="flex items-center justify-end gap-3">
          {saved ? (
            <span className="flex items-center gap-1 text-sm text-success">
              <Icon icon="gravity-ui:circle-check" className="size-4" />
              {t("common.saved")}
            </span>
          ) : null}
          <Button
            onPress={save}
            isDisabled={!dirty}
            isPending={saving}
            data-testid="settings-save"
          >
            {t("common.save")}
          </Button>
        </div>
      </Card.Content>
    </Card>
  );
}

/**
 * Ark credentials.
 *
 * Where the key comes from: **by default "pick one from the Volcengine account"** (`ArkFetchPicker`;
 * `keySource` starts as
 * `fetch`); typing by hand is the permanent fallback. ⚠️ Do not write "it can only be entered by
 * hand" again; that claim is outdated.
 *
 * What still holds: Volcengine has no OpenAPI to **create** keys (see the file header of
 * `server/core/arkfetch.go`),
 * so that path is "list the existing ones for the user to pick"; and it depends on an undocumented
 * frontend signature,
 * which may break, so typing by hand is always kept. The external link to the console is in
 * ArkFetchPicker.
 *
 * Once saved only the mask is echoed back, and one real verification is offered, so users do not
 * find out about a misconfiguration only when running an agent.
 */
function ArkTab() {
  // Relative time must follow the current language
  const { language } = usePrefs();
  const { workspace } = useWorkspace();
  const t = useT();
  const [editing, setEditing] = useState(false);
  const [clearing, setClearing] = useState(false);
  const [apiKey, setApiKey] = useState("");
  const [region, setRegion] = useState<ArkRegion>("cn-beijing");
  const [keyError, setKeyError] = useState<string | null>(null);
  // "The one picked from the Volcengine account". null means use the one typed by hand below.
  const [pickedRef, setPickedRef] = useState<ArkKeyRef | null>(null);
  /**
   * The key source the user picked. **Defaults to fetch**: picking from Volcengine is the normal
   * path, typing by hand is the fallback.
   *
   * ⚠️ This is only "what the user picked". When auto-fetch is unavailable ArkFetchPicker forces a
   * fallback to typing by hand,
   * and that check is inside it (`effectiveSource`); so **on submit this value alone is not
   * enough**;
   * see the comment in onSubmit.
   */
  const [keySource, setKeySource] = useState<ArkKeySource>("fetch");
  // Incremented each time the dialog opens, so ArkFetchPicker fetches again.
  const [openSeq, setOpenSeq] = useState(0);
  const [verifying, setVerifying] = useState(false);
  const [usingGlobal, setUsingGlobal] = useState(false);
  const [useGlobalError, setUseGlobalError] = useState<string | null>(null);
  // Content of the second-confirm dialog for "account not activated" on save; null means not shown.
  const [notActivated, setNotActivated] = useState<{
    message: string;
    /** The draft resent with confirmNotActivated after the user confirms (the two sources already merged). */
    draft: ArkCredentialDraft;
  } | null>(null);
  // Transient note for "not verified but let through", coming with the save response; cleared on
  // switching away / saving again.
  const [saveWarning, setSaveWarning] = useState<string | null>(null);
  // The Promise resolver for the not-activated dialog's "go activate / save anyway"; the dialog
  // buttons call it.
  const notActivatedResolver = useRef<((confirmed: boolean) => void) | null>(null);

  // A first-load failure must be visible. getArkCredential deliberately is not wrapped as nullable
  // (see http.ts),
  // but this used to be a bare await: once it rejected (non-member 403, or 5xx) cred stayed null
  // forever,
  // and the card stayed on the skeleton forever. See the file header of lib/useAsyncData.ts.
  const fetchCred = useCallback(() => api.getArkCredential(workspace.id), [workspace.id]);
  const {
    data: cred,
    error: loadError,
    isLoading,
    reload: load,
    setData: setCred,
  } = useAsyncData(fetchCred, t("settings.arkLoadFailed"));

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
          <AsyncErrorBlock error={loadError} onRetry={load} />
        </Card.Content>
      </Card>
    );
  }

  if (cred === null) return null;

  const configured = Boolean(cred.apiKeyMasked);
  // "Referencing global" has three states; do not merge them into two:
  //
  //   inherited  referencing, and the global one exists  → show the source + mask, offer "switch to
  //              custom"
  //   dangling   referencing, but the global one is gone → show as not configured, but guide to
  //              **global settings**
  //   override   this workspace's own custom key
  //
  // dangling is pulled out on its own because its next step differs from "never configured":
  // judging it as the latter would guide users to paste a workspace key, when what they really
  // should do is reconfigure the global one.
  const inheriting = cred.source === "inherit";
  const dangling = inheriting && !configured;

  async function verify() {
    setVerifying(true);
    try {
      setCred(await api.verifyArkCredential(workspace.id));
    } finally {
      setVerifying(false);
    }
  }

  /** Reference the current user's global credentials. On failure (e.g. global not configured yet) show the message as is. */
  async function useGlobal() {
    setUsingGlobal(true);
    setUseGlobalError(null);
    try {
      setCred(await api.useGlobalArkCredential(workspace.id));
    } catch (e) {
      setUseGlobalError(e instanceof Error ? e.message : t("settings.arkUseGlobalFailed"));
    } finally {
      setUsingGlobal(false);
    }
  }

  return (
    <div className="flex flex-col gap-4">
      <Card>
        <Card.Content className="flex flex-col gap-5 pt-6">
          <SectionTitle
            title={t("settings.arkApiKey")}
            // ⚠️ The sentence "**workspace-level**, shared by all members of this workspace" must
            // not be dropped.
            //
            // The neighboring "authorization management" is account-level (only yours, invisible to
            // others); the two tabs were adjacent,
            // both about credentials, and the cards look alike, yet they mean opposite things. When
            // users look back and forth between the two,
            // the only thing that corrects their expectation is this line. It used to say only
            // "configured independently per workspace", which answers
            // "do I need to reconfigure when switching workspace", not "will my change affect
            // others".
            description={t("settings.arkApiKeyDesc")}
            actions={
              <div className="flex items-center gap-2">
                {/*
                  "Reference global config": **this is that "one explicit configuration step"**.
                  A workspace never silently inherits anything; someone must click this.

                  Not shown when inherited (already referencing; clicking again means nothing).
                  outline rather than solid: it and "Replace" are two parallel secondary actions,
                  and this card has no primary CTA.
                */}
                {!inheriting ? (
                  <Button
                    size="sm"
                    variant="outline"
                    isPending={usingGlobal}
                    onPress={useGlobal}
                    data-testid="ark-use-global"
                  >
                    {t("settings.arkUseGlobal")}
                  </Button>
                ) : null}
                {configured || inheriting ? (
                  <Button
                    size="sm"
                    variant="outline"
                    onPress={() => {
                      setApiKey("");
                      setRegion(inheriting ? "cn-beijing" : cred.region);
                      setKeyError(null);
                      setPickedRef(null);
                      // Reset the source too: a half-picked state from last time should not stay,
                      // same reason as openSeq.
                      setKeySource("fetch");
                      setOpenSeq((n) => n + 1);
                      setEditing(true);
                    }}
                    data-testid="ark-replace"
                  >
                    {/* When inherited, this action means "switch back to this workspace's custom key";
                        writing "Replace" would make people think they are changing the global one. */}
                    {inheriting ? t("settings.arkSwitchToCustom") : t("settings.arkReplace")}
                  </Button>
                ) : null}
              </div>
            }
          />

          {/*
            Source bar: **see at a glance where the current key comes from**.
            
            This is the only anchor of the reference mechanism in the UI. Without it, an inherited
            workspace and a
            workspace that pasted its own key look **exactly the same** (both show a mask), so users
            do not know
            whether changing it here affects other workspaces, or where to change it.

            A neutral surface-secondary background rather than a semantic color: it states a fact,
            not good or bad.
            (warning would make people think "referencing global" is a problem to fix.)
          */}
          {inheriting && !dangling ? (
            <div
              className="flex flex-wrap items-center gap-x-2 gap-y-1 r-inset bg-surface-secondary px-3 py-2.5"
              data-testid="ark-source-inherit"
            >
              <Icon icon="gravity-ui:link" className="size-4 shrink-0 text-muted" />
              <p className="text-sm text-muted">
                {t("settings.arkInheritPrefix")}
                <strong className="font-medium text-foreground">
                  {t("settings.arkInheritSource")}
                </strong>
                {t("settings.arkInheritSuffix")}
                {cred.inheritFrom ? t("settings.arkInheritFrom", { name: cred.inheritFrom }) : null}
                {t("settings.arkInheritHint")}
              </p>
              {/* ⚠️ **Must carry the current workspace** (`?from=`): without it one click loses the shell,
                  the whole sidebar disappears and back goes to the workspace list; the user only went to look at that key,
                  and should not be kicked out of the workspace they are working in. The address is always built by preferencesPath(),
                  and none of the four callers builds it themselves (see app/preferencesLink.ts). */}
              <Link
                to={preferencesPath({ workspaceKey: workspace.key, tab: "ark" })}
                className="text-sm text-accent no-underline hover:underline"
              >
                {t("settings.arkGoToGlobal")}
              </Link>
            </div>
          ) : null}

          {/*
            References a global credential that no longer exists. **The guide must point to global
            settings**:
            judging it "not configured" would make users paste a workspace key, when what they really
            should do is reconfigure the global one.
          */}
          {dangling ? (
            <div
              className="flex items-start gap-2 r-inset bg-warning/5 px-3 py-2.5"
              data-testid="ark-source-dangling"
            >
              <Icon
                icon="gravity-ui:triangle-exclamation"
                className="mt-0.5 size-4 shrink-0 text-warning"
              />
              <p className="text-sm text-warning">
                {t("settings.arkDanglingPrefix")}
                <Link
                  to={preferencesPath({ workspaceKey: workspace.key, tab: "ark" })}
                  className="text-warning underline"
                >
                  {t("settings.arkDanglingLink")}
                </Link>
                {t("settings.arkDanglingSuffix")}
              </p>
            </div>
          ) : null}

          {useGlobalError ? (
            <p className="text-sm text-danger" role="alert" data-testid="ark-use-global-error">
              {useGlobalError}
            </p>
          ) : null}

          {/* Transient note for when Ark timed out/5xx on save but let it through: neutral color, the credentials are not broken. */}
          {saveWarning ? (
            <div className="flex items-start gap-2 r-inset bg-warning/5 px-3 py-2.5">
              <Icon
                icon="gravity-ui:circle-info"
                className="mt-0.5 size-4 shrink-0 text-warning"
              />
              <p
                className="text-sm text-warning"
                role="status"
                data-testid="ark-save-warning"
              >
                {saveWarning}
              </p>
            </div>
          ) : null}

          {configured ? (
            <>
              <div className="grid gap-x-8 gap-y-4 sm:grid-cols-2">
                <div className="flex flex-col gap-1">
                  <p className="text-xs text-muted">API Key</p>
                  <p className="font-mono text-sm tabular-nums" data-testid="ark-masked">
                    {cred.apiKeyMasked}
                  </p>
                </div>
                <div className="flex flex-col gap-1">
                  <p className="text-xs text-muted">{t("settings.arkStatus")}</p>
                  <div className="flex items-center gap-2">
                    <Chip size="sm" variant="soft" color={CHECK_COLOR[cred.check]}>
                      <Chip.Label>{t(ARK_CHECK_KEYS[cred.check])}</Chip.Label>
                    </Chip>
                    {cred.checkedAt ? (
                      <span className="text-xs text-muted">
                        {t("settings.arkCheckedAt", {
                          time: formatRelative(cred.checkedAt, language),
                        })}
                      </span>
                    ) : null}
                  </div>
                </div>
              </div>

              {/* When inherited, the region belongs to the global one and changing it here has no effect (the backend also rejects it),
                  so disable it and say where to change it; leaving it clickable would let users change it and see nothing happen. */}
              <Select
                className="max-w-sm"
                isDisabled={inheriting}
                selectedKey={cred.region}
                onSelectionChange={async (k) => {
                  setCred(await api.updateArkRegion(workspace.id, String(k) as ArkRegion));
                }}
              >
                <Label>{t("settings.arkRegion")}</Label>
                <Select.Trigger data-testid="ark-region">
                  <Select.Value />
                  <Select.Indicator />
                </Select.Trigger>
                <Select.Popover>
                  <ListBox>
                    {ARK_REGIONS.map((r) => (
                      <ListBox.Item key={r.id} id={r.id} textValue={t(ARK_REGION_KEYS[r.id])}>
                        {t(ARK_REGION_KEYS[r.id])}
                      </ListBox.Item>
                    ))}
                  </ListBox>
                </Select.Popover>
                <Description>
                  {inheriting ? (
                    t("settings.arkRegionInherited")
                  ) : (
                    <code className="text-xs">
                      {ARK_REGIONS.find((r) => r.id === cred.region)?.baseUrl}
                    </code>
                  )}
                </Description>
              </Select>

              {cred.check === "invalid" && cred.checkError ? (
                <div className="flex items-start gap-2 r-inset bg-danger/5 px-3 py-2.5">
                  <Icon
                    icon="gravity-ui:triangle-exclamation"
                    className="mt-0.5 size-4 shrink-0 text-danger"
                  />
                  <p className="text-sm text-danger" data-testid="ark-error">
                    {cred.checkError}
                  </p>
                </div>
              ) : null}

              <Separator />

              <div className="flex items-center justify-between gap-3">
                <span className="text-xs text-muted">
                  {cred.updatedAt
                    ? t("settings.arkUpdatedAt", { time: formatRelative(cred.updatedAt, language) })
                    : null}
                  {cred.updatedBy ? ` · ${cred.updatedBy}` : null}
                </span>
                <div className="flex items-center gap-2">
                  <Button
                    variant="danger-soft"
                    size="sm"
                    onPress={() => setClearing(true)}
                    data-testid="ark-clear"
                  >
                    {t("settings.arkClear")}
                  </Button>
                  <Button
                    variant="outline"
                    size="sm"
                    onPress={verify}
                    isPending={verifying}
                    data-testid="ark-verify"
                  >
                    {t("settings.arkVerify")}
                  </Button>
                </div>
              </div>
            </>
          ) : (
            <Empty
              icon="gravity-ui:key"
              title={t("settings.arkEmptyTitle")}
              description={t("settings.arkEmptyDesc")}
              action={
                // Both paths are offered side by side. **"Reference global" goes first**: most
                // people have only one key,
                // and configuring once to use everywhere is the wanted default path; filling in a
                // separate one is the exception.
                <div className="flex flex-wrap items-center justify-center gap-2">
                  <Button
                    variant="outline"
                    isPending={usingGlobal}
                    onPress={useGlobal}
                    data-testid="ark-use-global-empty"
                  >
                    {t("settings.arkUseGlobal")}
                  </Button>
                  <Button
                    onPress={() => {
                      setApiKey("");
                      setRegion("cn-beijing");
                      setKeyError(null);
                      setPickedRef(null);
                      // Reset the source too: a half-picked state from last time should not stay,
                      // same reason as openSeq.
                      setKeySource("fetch");
                      setOpenSeq((n) => n + 1);
                      setEditing(true);
                    }}
                    data-testid="ark-setup"
                  >
                    {t("settings.arkSetup")}
                  </Button>
                </div>
              }
            />
          )}
        </Card.Content>
      </Card>

      <FormModal
        isOpen={editing}
        onOpenChange={setEditing}
        title={configured ? t("settings.arkReplaceTitle") : t("settings.arkSetupTitle")}
        description={t("settings.arkModalDesc")}
        submitLabel={t("common.save")}
        testId="ark-modal"
        onSubmit={async () => {
          // Merge the two sources. **The decision order matters** ("use what is there" rather than
          // "what they said to use"),
          // and "picked auto but did not choose a key" must report the right message; see the
          // comment on resolveArkSubmission.
          const resolved = resolveArkSubmission(keySource, pickedRef, apiKey, t);
          if (resolved instanceof Error) {
            // ⚠️ **With "pick from Volcengine", the error must not go into FieldError**: that
            // FieldError hangs in
            // manualSlot, and the manual input is not rendered at this moment, so the error **is
            // not visible at all**,
            // and the user only sees save doing nothing. Throw it to FormModal's error bar.
            if (keySource === "fetch" && !pickedRef) throw resolved;
            setKeyError(resolved.message);
            return false;
          }
          // The fetchRef path **only hands over two ids**; the plain text is fetched by the server
          // itself (it never passes through the frontend).
          const draft: ArkCredentialDraft = { apiKey: "", region, ...resolved };
          try {
            const saved = await saveArkCredentialWithConfirm(
              draft,
              (d) => api.saveArkCredential(workspace.id, d),
              // Not activated: close the form dialog and show the second confirm (go activate /
              // save anyway).
              (message) =>
                new Promise<boolean>((resolvePromise) => {
                  setEditing(false);
                  notActivatedResolver.current = resolvePromise;
                  setNotActivated({ message, draft });
                }),
            );
            setSaveWarning(saved.warning?.message ?? null);
            setEditing(false);
            await load();
          } catch (e) {
            if (isArkSaveCancelled(e)) return false;
            // Bad key / other errors: handed to FormModal's modal-error bar.
            throw e;
          }
        }}
      >
        {/*
          The part for picking an account and key. It also handles **the source dropdown** and
          "when the manual input appears":
          the manual input is passed in as manualSlot and not rendered by default (see the
          ArkFetchPicker file header).
        */}
        <ArkFetchPicker
          resetKey={openSeq}
          picked={pickedRef}
          source={keySource}
          onSourceChange={setKeySource}
          onPick={(ref) => {
            setPickedRef(ref);
            if (ref) {
              setApiKey("");
              setKeyError(null);
            }
          }}
          manualSlot={
            <TextField isRequired isInvalid={Boolean(keyError)}>
              <Label>API Key</Label>
              <Input
                type="password"
                placeholder={t("settings.arkKeyPlaceholder")}
                value={apiKey}
                onChange={(e) => {
                  setApiKey(e.target.value);
                  setKeyError(null);
                }}
                data-testid="ark-key-input"
              />
              {keyError ? <FieldError>{keyError}</FieldError> : null}
            </TextField>
          }
        />

        <Select selectedKey={region} onSelectionChange={(k) => setRegion(String(k) as ArkRegion)}>
          <Label>{t("settings.arkRegion")}</Label>
          <Select.Trigger data-testid="ark-region-input">
            <Select.Value />
            <Select.Indicator />
          </Select.Trigger>
          <Select.Popover>
            <ListBox>
              {ARK_REGIONS.map((r) => (
                <ListBox.Item key={r.id} id={r.id} textValue={t(ARK_REGION_KEYS[r.id])}>
                  {t(ARK_REGION_KEYS[r.id])}
                </ListBox.Item>
              ))}
            </ListBox>
          </Select.Popover>
        </Select>
      </FormModal>

      <ConfirmModal
        isOpen={clearing}
        onOpenChange={setClearing}
        title={t("settings.arkClearTitle")}
        description={t("settings.arkClearDesc")}
        confirmLabel={t("settings.arkClear")}
        onConfirm={async () => {
          await api.deleteArkCredential(workspace.id);
          setClearing(false);
          await load();
        }}
      />

      <ArkNotActivatedDialog
        isOpen={notActivated !== null}
        message={notActivated?.message ?? ""}
        onOpenChange={(open) => {
          if (!open) {
            // Cancel / close = do not save.
            notActivatedResolver.current?.(false);
            notActivatedResolver.current = null;
            setNotActivated(null);
          }
        }}
        onConfirmSave={() => {
          notActivatedResolver.current?.(true);
          notActivatedResolver.current = null;
          setNotActivated(null);
        }}
      />
    </div>
  );
}

/**
 * Member management, a tab of the settings page (`?tab=members`).
 *
 * One row is one person. When inviting, type the other person's login email directly (the
 * lowercase email is the user's unique ID);
 * the name is optional, and defaults to the part of the email before @.
 *
 * **Do not add a page header to it**: the outer SettingsPage already has the "Workspace settings" h1.
 */
function MembersTab() {
  // Relative time must follow the current language
  const { language } = usePrefs();
  const { workspace, refresh } = useWorkspace();
  const t = useT();

  const [inviting, setInviting] = useState(false);
  /** The member pending removal; null means no removal in progress. */
  const [removing, setRemoving] = useState<WorkspaceMember | null>(null);

  const [email, setEmail] = useState("");
  const [name, setName] = useState("");
  const [role, setRole] = useState<WorkspaceRole>("member");
  const [emailError, setEmailError] = useState<string | null>(null);

  /**
   * People sorted by role: owner → admin → member → viewer.
   *
   * ⚠️ **The owner must come first**: "whose workspace is this" is the most important piece of
   * information in this list.
   */
  const people = [...workspace.members].sort(
    (a, b) => ROLE_ORDER.indexOf(a.role) - ROLE_ORDER.indexOf(b.role),
  );

  const ownerCount = workspace.members.filter((m) => m.role === "owner").length;

  const openInvite = () => {
    setEmail("");
    setName("");
    setRole("member");
    setEmailError(null);
    setInviting(true);
  };

  return (
    <Card>
      <Card.Content className="flex flex-col gap-5 pt-6">
        <SectionTitle
          title={t("settings.tabMembers")}
          description={t("settings.membersSectionDesc")}
          actions={
            <Button size="sm" onPress={openInvite} data-testid="invite-member">
              {t("settings.invite")}
            </Button>
          }
        />

        <div className="flex flex-col gap-6" data-testid="member-list">
          <MemberSection
            label={t("settings.tabMembers")}
            count={people.length}
            testId="member-section-people"
            empty={t("settings.membersEmpty")}
          >
            {people.map((m, i) => (
              <div key={m.id}>
                {i > 0 ? <Separator /> : null}
                <PersonRow
                  member={m}
                  ownerCount={ownerCount}
                  language={language}
                  workspaceId={workspace.id}
                  onChanged={refresh}
                  onRemove={() => setRemoving(m)}
                />
              </div>
            ))}
          </MemberSection>
        </div>

        {/*
          The footer's "what each role can do", collapsed by default. It is **reference material**,
          answering "which role to give this
          person", right below the role dropdown, so when hesitating a glance down finds it.

          ⚠️ **The table content is generated; do not hand-write permissions here or in that
          component**: the source of truth is
          `server/authz/matrix.go`; see the file header of RolePermissionTable.
        */}
        <div className="border-t border-separator pt-1">
          <RolePermissionTable />
        </div>

        <FormModal
          isOpen={inviting}
          onOpenChange={(open) => !open && setInviting(false)}
          title={t("settings.inviteTitle")}
          submitLabel={t("settings.inviteAction")}
          onSubmit={async () => {
            const trimmed = email.trim().toLowerCase();
            if (!EMAIL_PATTERN.test(trimmed)) {
              setEmailError(t("settings.memberEmailNotValid"));
              return false;
            }
            setEmailError(null);
            await api.addMember(workspace.id, {
              name: name.trim() || trimmed.split("@")[0],
              email: trimmed,
              role,
            });
            await refresh();
          }}
        >
          <TextField isRequired isInvalid={Boolean(emailError)}>
            <Label>{t("settings.memberEmail")}</Label>
            <Input
              type="email"
              value={email}
              placeholder="alice@example.com"
              onChange={(e) => {
                setEmail(e.target.value);
                setEmailError(null);
              }}
              data-testid="member-email"
            />
            {emailError ? (
              <FieldError data-testid="member-error">{emailError}</FieldError>
            ) : (
              <Description>{t("settings.memberEmailHint")}</Description>
            )}
          </TextField>
          <TextField>
            <Label>{t("settings.memberDisplayName")}</Label>
            <Input
              value={name}
              placeholder={t("settings.memberNamePlaceholder")}
              onChange={(e) => setName(e.target.value)}
              data-testid="member-name"
            />
          </TextField>
          <RoleSelect role={role} onChange={setRole} testId="member-role" />
        </FormModal>

        <ConfirmModal
          isOpen={removing !== null}
          onOpenChange={(open) => !open && setRemoving(null)}
          title={t("settings.removeMemberTitle", { name: removing?.name ?? "" })}
          description={t("settings.removeMemberDesc")}
          confirmLabel={t("common.remove")}
          onConfirm={async () => {
            if (!removing) return;
            await api.removeMember(workspace.id, removing.id);
            setRemoving(null);
            await refresh();
          }}
        />
      </Card.Content>
    </Card>
  );
}

/** Only the roughest shape check (has @, both sides non-empty, a dot in the domain); the real check is on the backend. */
const EMAIL_PATTERN = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

/**
 * One section of the list: a small heading + count + some rows.
 *
 * The card already has a "Members" section heading (SectionTitle is an h2), so the section heading
 * is deliberately very light:
 * **it is a divider label, not a section heading**.
 */
function MemberSection({
  label,
  count,
  testId,
  empty,
  children,
}: {
  label: string;
  count: number;
  testId: string;
  /** Text shown when the count is 0. If not given, an empty section is not rendered at all. */
  empty?: string;
  children: ReactNode;
}) {
  return (
    <div className="flex flex-col gap-1.5" data-testid={testId}>
      <div className="flex items-baseline gap-1.5">
        <span className="text-xs font-medium text-muted">{label}</span>
        {/* tabular-nums: digit width does not jump when the count changes */}
        <span className="text-xs tabular-nums text-muted/70">{count}</span>
      </div>
      {count === 0 && empty ? (
        <p className="py-3 text-sm text-muted">{empty}</p>
      ) : (
        <div className="flex flex-col">{children}</div>
      )}
    </div>
  );
}

/** Role dropdown (for the invite dialog). */
function RoleSelect({
  role,
  onChange,
  testId,
}: {
  role: WorkspaceRole;
  onChange: (r: WorkspaceRole) => void;
  testId: string;
}) {
  const t = useT();
  return (
    <Select selectedKey={role} onSelectionChange={(k) => onChange(String(k) as WorkspaceRole)}>
      <Label>{t("settings.memberRole")}</Label>
      <Select.Trigger data-testid={testId}>
        <Select.Value />
        <Select.Indicator />
      </Select.Trigger>
      <Select.Popover>
        <ListBox>
          {ROLE_ORDER.map((r) => (
            <ListBox.Item key={r} id={r} textValue={t(`enum.role.${r}`)}>
              {t(`enum.role.${r}`)}
            </ListBox.Item>
          ))}
        </ListBox>
      </Select.Popover>
    </Select>
  );
}

/** One "person" row. */
function PersonRow({
  member: m,
  ownerCount,
  language,
  workspaceId,
  onChanged,
  onRemove,
}: {
  member: WorkspaceMember;
  ownerCount: number;
  language: string;
  workspaceId: string;
  onChanged: () => Promise<void> | void;
  onRemove: () => void;
}) {
  const t = useT();
  const isLastOwner = m.role === "owner" && ownerCount === 1;
  return (
    // ⚠️ **This testid is for e2e to count rows**; do not delete it. Old tests counted rows with
    // `[data-testid="member-list"] > div`, which depended on DOM depth; after sections were added
    // member-list contained section containers, and those tests all went red on the spot. With a
    // stable testid
    // layout changes no longer break the count assertions.
    <div className="flex items-center gap-3 py-3" data-testid="member-row">
      {/*
        None of the avatar's three "no image" cases need our own checks: Radix only renders <img>
        after loaded,
        and before that and after failure the Fallback stays in place. **Do not write
        conditional rendering like `m.avatarUrl && ...`.**
        Same as CurrentUser in AppShell. alt is empty because the name is right next to it.
      */}
      <Avatar className="size-8 shrink-0">
        <Avatar.Image src={m.avatarUrl} alt="" data-testid={`member-avatar-${m.email}`} />
        <Avatar.Fallback className="text-xs font-medium">{m.name.slice(0, 1)}</Avatar.Fallback>
      </Avatar>

      <div className="flex min-w-0 flex-1 flex-col">
        <span className="truncate text-sm font-medium">{m.name}</span>
        <span className="truncate text-xs text-muted">{m.email}</span>
      </div>

      {/*
        The only owner cannot change roles, otherwise nobody could manage the workspace.

        ⚠️ **The chip must be wrapped in a container as wide as the Select (w-28)**: a bare chip is
        only 48px,
        so this row's role column was 64px narrower than other rows, pushing the date and buttons on
        the right;
        measured role column x for the three row kinds was 1088 / 1126 / 1152, visibly "squeezed".
      */}
      {isLastOwner ? (
        <div className="flex w-28 justify-center">
          <Chip size="sm" variant="soft">
            <Chip.Label>{t("enum.role.owner")}</Chip.Label>
          </Chip>
        </div>
      ) : (
        <Select
          className="w-28"
          aria-label={t("settings.roleOf", { name: m.name })}
          selectedKey={m.role}
          onSelectionChange={async (k) => {
            await api.updateMemberRole(workspaceId, m.id, String(k) as WorkspaceRole);
            await onChanged();
          }}
        >
          <Select.Trigger data-testid={`role-${m.email}`}>
            <Select.Value />
            <Select.Indicator />
          </Select.Trigger>
          <Select.Popover>
            <ListBox>
              {ROLE_ORDER.map((r) => (
                <ListBox.Item key={r} id={r} textValue={t(`enum.role.${r}`)}>
                  {t(`enum.role.${r}`)}
                </ListBox.Item>
              ))}
            </ListBox>
          </Select.Popover>
        </Select>
      )}

      {/*
        Join time. **w-28 + whitespace-nowrap**: past 30 days it falls back to an absolute date
        (formatRelative's behavior), and that string is much longer than "26 days ago"; without
        enough width
        and no-wrap, it would break into two lines, making the row taller and misaligned with its
        neighbors.
      */}
      <span className="hidden w-28 shrink-0 text-right text-xs whitespace-nowrap text-muted sm:block">
        {t("settings.joinedAt", { time: formatRelative(m.joinedAt, language) })}
      </span>

      <Tooltip>
        <Tooltip.Trigger>
          <Button
            variant="ghost"
            isIconOnly
            size="sm"
            aria-label={t("settings.removeMember", { name: m.name })}
            isDisabled={isLastOwner}
            onPress={onRemove}
            data-testid={`remove-${m.email}`}
          >
            <Icon icon="gravity-ui:xmark" className="size-4" />
          </Button>
        </Tooltip.Trigger>
        <Tooltip.Content>
          {isLastOwner ? t("settings.cannotRemoveOwner") : t("settings.removeMemberTooltip")}
        </Tooltip.Content>
      </Tooltip>
    </div>
  );
}

function DangerTab() {
  const { workspace } = useWorkspace();
  const navigate = useNavigate();
  const [confirming, setConfirming] = useState(false);
  const t = useT();

  return (
    <Card>
      <Card.Content className="flex flex-col gap-5 pt-6">
        <SectionTitle
          title={t("settings.deleteWorkspace")}
          description={t("settings.deleteWorkspaceDesc")}
        />
        <div className="flex items-start gap-2 r-inset bg-danger/5 px-3 py-2.5">
          <Icon
            icon="gravity-ui:triangle-exclamation"
            className="mt-0.5 size-4 shrink-0 text-danger"
          />
          <p className="text-sm text-danger">{t("settings.deleteWorkspaceWarn")}</p>
        </div>
        <div className="flex justify-start">
          <Button variant="danger-soft" onPress={() => setConfirming(true)} data-testid="delete-workspace">
            {t("settings.deleteWorkspace")}
          </Button>
        </div>

        <ConfirmModal
          isOpen={confirming}
          onOpenChange={setConfirming}
          title={t("settings.deleteWorkspaceTitle", { name: workspace.name })}
          description={t("settings.deleteWorkspaceConfirm")}
          onConfirm={async () => {
            await api.deleteWorkspace(workspace.id);
            navigate("/workspaces", { replace: true });
          }}
        />
      </Card.Content>
    </Card>
  );
}

export function SettingsPage() {
  const [params, setParams] = useSearchParams();
  // The redirect must build a global settings address **within the workspace** (see the one
  // below), so we need the workspace here.
  const { workspace } = useWorkspace();
  const t = useT();
  // **Do not assert it as TabId**: the URL can be edited by the user and may contain any string
  // (including the moved
  // apikeys / integrations; see the redirect below). Asserting would make TS think it is always
  // one of the four valid values,
  // and comparing with "apikeys" would be judged "never true" and fail to compile.
  const tab = params.get("tab") ?? "general";

  /**
   * The two account-level tabs that moved; each old address gets a redirect:
   *
   *   ?tab=apikeys      → /preferences?tab=apikeys&from=:key
   *   ?tab=integrations → /preferences?tab=integrations&from=:key
   *
   * **Must carry `?from=`**: the user came from this workspace's settings page and should not be
   * sent out of the workspace
   * (without that parameter it is the shell-less form, and the whole sidebar disappears).
   *
   * Both addresses **really were used out in the wild** (docs, bookmarks and existing e2e all pointed
   * at them; `?tab=integrations`
   * is also indirectly pointed at by 9 backend error hints, which say "Global settings →
   * Authorization management"). Without catching them,
   * `tab` would be an unknown value, none of the `tab === "xxx"` checks below would match, and the
   * page would render a shell with only
   * a tab bar and nothing below: **no error and no redirect**; users would only feel "that page is
   * broken",
   * and would not think the address changed.
   *
   * Navigate rather than useEffect + navigate: it swaps during render, without first flashing an
   * empty shell.
   * `replace` is required: keeping this history entry would make the browser back button from
   * global settings return to
   * the old address and get redirected away again, breaking the back key (same reason as
   * MembersRedirect).
   */
  if (tab === "apikeys" || tab === "integrations") {
    return <Navigate to={preferencesPath({ workspaceKey: workspace.key, tab })} replace />;
  }

  return (
    <PageBody>
      <PageHeader title={t("settings.title")} description={t("settings.desc")} />

      <div className="flex items-center gap-1 border-b border-border">
        {TABS.map((tb) => (
          <button
            key={tb.id}
            type="button"
            onClick={() => {
              params.set("tab", tb.id);
              setParams(params, { replace: true });
            }}
            data-testid={`settings-tab-${tb.id}`}
            className={[
              "relative flex items-center gap-1.5 px-3 py-2 text-sm cursor-[var(--cursor-interactive)]",
              tab === tb.id
                ? "font-medium text-foreground after:absolute after:inset-x-0 after:-bottom-px after:h-0.5 after:bg-accent"
                : "text-muted hover:text-foreground",
            ].join(" ")}
          >
            {t(tb.labelKey)}
          </button>
        ))}
      </div>

      {tab === "general" ? <GeneralTab /> : null}
      {tab === "members" ? <MembersTab /> : null}
      {tab === "ark" ? <ArkTab /> : null}
      {tab === "hosts" ? (
        <WorkspaceHostsTab workspaceId={workspace.id} workspaceKey={workspace.key} />
      ) : null}
      {tab === "danger" ? <DangerTab /> : null}
    </PageBody>
  );
}

/**
 * Account-level Ark credentials: the one that is "configured once globally and referenced by
 * workspaces".
 *
 * # How it relates to the Ark credentials card in workspace settings
 *
 *   this component (global settings)  configured once, belongs to **you as a person**
 *   ArkTab (workspace settings)        each workspace picks "reference this one" or "paste its own"
 *
 * ⚠️ **Changing this immediately affects every workspace that references it**: referencing
 * workspaces store no copy, and on read they
 * fall back to this one. That is the point of the feature (changing the key only takes one
 * place), but it also means
 * its impact is much larger than the workspace-level key, so the card must say so.
 *
 * # Where the key comes from: by default "pick one from the Volcengine account"; typing by hand is
 * the permanent fallback
 *
 * ⚠️ **Do not write "it can only be entered by hand" again**: that claim is outdated. The default
 * source now is to list
 * existing keys from the Volcengine account and pick one (`ArkFetchPicker`; `keySource` starts as
 * `fetch`).
 *
 * What still holds is something else: Volcengine **has no OpenAPI to create** keys (grepping the
 * whole repo for `CreateApiKey`
 * finds nothing; see the file header of `server/core/arkfetch.go`), so this path is "list the
 * existing ones for the user to
 * pick", not "create one for the user". It also depends on a frontend signature and undocumented
 * internal console
 * endpoints that may break when the other side changes its frontend; **so typing by hand is always
 * kept**, and any failure of auto-fetch
 * only shows as "auto-fetch unavailable, please enter it by hand", never blocking the user on
 * this path.
 *
 * The external link to the console is in `ConsoleLink` of `ArkFetchPicker` (in the not-connected
 * and empty-list branches),
 * which is when users really need it; the explainer card "why it must be entered by hand" at the
 * bottom of the page has been removed,
 * because the UI below directly contradicts it.
 */

import { useCallback, useRef, useState } from "react";
import {
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
  TextField,
} from "@heroui/react";
import { Icon } from "@iconify/react";
import { api } from "@/data/api";
import { AsyncErrorBlock, Empty, ListSkeleton, SectionTitle } from "@/components/Page";
import { ConfirmModal, FormModal } from "@/components/FormModal";
import { ArkNotActivatedDialog } from "@/components/ArkNotActivatedDialog";
import { isArkSaveCancelled, saveArkCredentialWithConfirm } from "@/lib/arkSave";
import {
  ArkFetchPicker,
  resolveArkSubmission,
  type ArkKeySource,
} from "@/components/ArkFetchPicker";
import { formatRelative } from "@/lib/format";
import { useAsyncData } from "@/lib/useAsyncData";
import {
  ARK_CHECK_KEYS,
  ARK_REGION_KEYS,
  ARK_REGIONS,
  type ArkCredentialCheck,
  type ArkCredentialDraft,
  type ArkKeyRef,
  type ArkRegion,
} from "@/types/domain";
import { usePrefs, useT } from "@/prefs/PrefsContext";

/** Unverified uses a neutral color: it is not an error, just "not tested yet"; warning would falsely report a problem. */
const CHECK_COLOR: Record<ArkCredentialCheck, "success" | "danger" | "default"> = {
  valid: "success",
  invalid: "danger",
  unchecked: "default",
};

export function GlobalArkTab() {
  const { language } = usePrefs();
  const t = useT();
  const [editing, setEditing] = useState(false);
  const [clearing, setClearing] = useState(false);
  const [apiKey, setApiKey] = useState("");
  const [region, setRegion] = useState<ArkRegion>("cn-beijing");
  const [keyError, setKeyError] = useState<string | null>(null);
  // "The one picked from the Volcengine account". null means use the one typed by hand below.
  const [pickedRef, setPickedRef] = useState<ArkKeyRef | null>(null);
  /** The key source the user picked. **Defaults to fetch**; when unavailable the picker forces a fallback to typing by hand. */
  const [keySource, setKeySource] = useState<ArkKeySource>("fetch");
  // Incremented each time the dialog opens, so ArkFetchPicker fetches again (a half-picked state
  // from last time should not stay).
  const [openSeq, setOpenSeq] = useState(0);
  const [verifying, setVerifying] = useState(false);
  // Content of the second-confirm dialog for "account not activated" on save; null means not shown.
  const [notActivated, setNotActivated] = useState<{ message: string } | null>(null);
  // Transient note for "not verified but let through".
  const [saveWarning, setSaveWarning] = useState<string | null>(null);
  const notActivatedResolver = useRef<((confirmed: boolean) => void) | null>(null);

  // Same as the workspace side: once a bare await rejects, cred stays null forever and the card
  // stays on the skeleton forever.
  const fetchCred = useCallback(() => api.getGlobalArkCredential(), []);
  const {
    data: cred,
    error: loadError,
    isLoading,
    reload: load,
    setData: setCred,
  } = useAsyncData(fetchCred, t("globalArk.loadFailed"));

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

  async function verify() {
    setVerifying(true);
    try {
      setCred(await api.verifyGlobalArkCredential());
    } finally {
      setVerifying(false);
    }
  }

  return (
    <div className="flex flex-col gap-4">
      <Card>
        <Card.Content className="flex flex-col gap-5 pt-6">
          <SectionTitle
            title={t("globalArk.title")}
            // ⚠️ The sentence "referencing workspaces change right away" must not be dropped.
            //
            // It is the only difference in meaning between this card and the workspace one: there,
            // changing the key affects one workspace;
            // here, changing the key affects **every workspace that references it**. Without saying
            // so users would think
            // the two are operations of the same size, and change a batch of workspaces' credentials
            // without knowing.
            description={t("globalArk.description")}
            actions={
              configured ? (
                <Button
                  size="sm"
                  variant="outline"
                  onPress={() => {
                    setApiKey("");
                    setRegion(cred.region);
                    setKeyError(null);
                    setPickedRef(null);
                    setKeySource("fetch");
                    setOpenSeq((n) => n + 1);
                    setEditing(true);
                  }}
                  data-testid="global-ark-replace"
                >
                  {t("arkCred.replace")}
                </Button>
              ) : null
            }
          />

          {configured ? (
            <>
              <div className="grid gap-x-8 gap-y-4 sm:grid-cols-2">
                <div className="flex flex-col gap-1">
                  <p className="text-xs text-muted">API Key</p>
                  <p className="font-mono text-sm tabular-nums" data-testid="global-ark-masked">
                    {cred.apiKeyMasked}
                  </p>
                </div>
                <div className="flex flex-col gap-1">
                  <p className="text-xs text-muted">{t("arkCred.status")}</p>
                  <div className="flex items-center gap-2">
                    <Chip size="sm" variant="soft" color={CHECK_COLOR[cred.check]}>
                      <Chip.Label>{t(ARK_CHECK_KEYS[cred.check])}</Chip.Label>
                    </Chip>
                    {cred.checkedAt ? (
                      <span className="text-xs text-muted">
                        {t("globalArk.checkedAt", { time: formatRelative(cred.checkedAt, language) })}
                      </span>
                    ) : null}
                  </div>
                </div>
              </div>

              <Select
                className="max-w-sm"
                selectedKey={cred.region}
                onSelectionChange={async (k) => {
                  // The region is a property of this key; changing it changes what was verified,
                  // so the whole thing is saved again (the backend resets check).
                  setCred(
                    await api.saveGlobalArkCredential({
                      apiKey: "",
                      region: String(k) as ArkRegion,
                    }),
                  );
                }}
                // ⚠️ The global one **has no separate "change region only" endpoint** (the workspace
                // side does).
                // Changing region means re-entering the key too, because the server only stores
                // things other than the hash and
                // cannot get the plain text to rebuild the whole record; so it is disabled here, and
                // changing region goes through "Replace".
                isDisabled
              >
                <Label>{t("arkCred.region")}</Label>
                <Select.Trigger data-testid="global-ark-region">
                  <Select.Value />
                  <Select.Indicator />
                </Select.Trigger>
                <Select.Popover>
                  <ListBox>
                    {ARK_REGIONS.map((r) => (
                      <ListBox.Item
                        key={r.id}
                        id={r.id}
                        textValue={t(ARK_REGION_KEYS[r.id])}
                      >
                        {t(ARK_REGION_KEYS[r.id])}
                      </ListBox.Item>
                    ))}
                  </ListBox>
                </Select.Popover>
                <Description>{t("globalArk.regionHint")}</Description>
              </Select>

              {cred.check === "invalid" && cred.checkError ? (
                <div className="flex items-start gap-2 r-inset bg-danger/5 px-3 py-2.5">
                  <Icon
                    icon="gravity-ui:triangle-exclamation"
                    className="mt-0.5 size-4 shrink-0 text-danger"
                  />
                  <p className="text-sm text-danger" data-testid="global-ark-error">
                    {cred.checkError}
                  </p>
                </div>
              ) : null}

              {saveWarning ? (
                <div
                  className="flex items-start gap-2 r-inset bg-warning/5 px-3 py-2.5"
                  role="status"
                  data-testid="global-ark-save-warning"
                >
                  <Icon
                    icon="gravity-ui:circle-info"
                    className="mt-0.5 size-4 shrink-0 text-warning"
                  />
                  <p className="text-sm text-warning">{saveWarning}</p>
                </div>
              ) : null}

              <Separator />

              <div className="flex items-center justify-between gap-3">
                <span className="text-xs text-muted">
                  {cred.updatedAt
                    ? t("globalArk.updatedAt", { time: formatRelative(cred.updatedAt, language) })
                    : null}
                </span>
                <div className="flex items-center gap-2">
                  <Button
                    variant="danger-soft"
                    size="sm"
                    onPress={() => setClearing(true)}
                    data-testid="global-ark-clear"
                  >
                    {t("globalArk.clear")}
                  </Button>
                  <Button
                    variant="outline"
                    size="sm"
                    onPress={verify}
                    isPending={verifying}
                    data-testid="global-ark-verify"
                  >
                    {t("globalArk.verify")}
                  </Button>
                </div>
              </div>
            </>
          ) : (
            <Empty
              icon="gravity-ui:key"
              title={t("globalArk.emptyTitle")}
              description={t("globalArk.emptyDesc")}
              action={
                <Button
                  onPress={() => {
                    setApiKey("");
                    setRegion("cn-beijing");
                    setKeyError(null);
                    setPickedRef(null);
                    setKeySource("fetch");
                    setOpenSeq((n) => n + 1);
                    setEditing(true);
                  }}
                  data-testid="global-ark-setup"
                >
                  {t("globalArk.setup")}
                </Button>
              }
            />
          )}
        </Card.Content>
      </Card>

      <FormModal
        isOpen={editing}
        onOpenChange={setEditing}
        title={configured ? t("globalArk.modalReplaceTitle") : t("globalArk.modalSetupTitle")}
        description={t("globalArk.modalDesc")}
        submitLabel={t("common.save")}
        testId="global-ark-modal"
        onSubmit={async () => {
          // Merges the two sources; see the comment on resolveArkSubmission (decision order + where
          // errors go).
          const resolved = resolveArkSubmission(keySource, pickedRef, apiKey, t);
          if (resolved instanceof Error) {
            // ⚠️ With "pick from Volcengine" the manual input is not rendered, so putting the error
            // in its FieldError would hide it.
            if (keySource === "fetch" && !pickedRef) throw resolved;
            setKeyError(resolved.message);
            return false;
          }
          const draft: ArkCredentialDraft = { apiKey: "", region, ...resolved };
          try {
            const saved = await saveArkCredentialWithConfirm(
              draft,
              (d) => api.saveGlobalArkCredential(d),
              (message) =>
                new Promise<boolean>((resolvePromise) => {
                  setEditing(false);
                  notActivatedResolver.current = resolvePromise;
                  setNotActivated({ message });
                }),
            );
            setSaveWarning(saved.warning?.message ?? null);
            setEditing(false);
            await load();
          } catch (e) {
            if (isArkSaveCancelled(e)) return false;
            throw e;
          }
        }}
      >
        {/* Pick an existing one from the Volcengine account. When unavailable / not authorized this part hides itself; typing by hand works as usual. */}
        <ArkFetchPicker
          resetKey={openSeq}
          // ⚠️ global makes the picker show, right after a selection, "every workspace that
          // references it switches to the new
          // key right away". The same sentence in the dialog title is out of view after scrolling,
          // and pressing save affects
          // a batch of workspaces, a much larger impact than the workspace-level key, so it is said
          // once more on the last screen before pressing.
          scope="global"
          picked={pickedRef}
          source={keySource}
          onSourceChange={setKeySource}
          onPick={(ref) => {
            setPickedRef(ref);
            // Picking one clears the typed one; the server rejects "both given" outright.
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
                placeholder={t("globalArk.keyPlaceholder")}
                value={apiKey}
                onChange={(e) => {
                  setApiKey(e.target.value);
                  setKeyError(null);
                }}
                data-testid="global-ark-key-input"
              />
              {keyError ? <FieldError>{keyError}</FieldError> : null}
            </TextField>
          }
        />

        <Select selectedKey={region} onSelectionChange={(k) => setRegion(String(k) as ArkRegion)}>
          <Label>{t("arkCred.region")}</Label>
          <Select.Trigger data-testid="global-ark-region-input">
            <Select.Value />
            <Select.Indicator />
          </Select.Trigger>
          <Select.Popover>
            <ListBox>
              {ARK_REGIONS.map((r) => (
                <ListBox.Item
                  key={r.id}
                  id={r.id}
                  textValue={t(ARK_REGION_KEYS[r.id])}
                >
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
        title={t("globalArk.clearTitle")}
        // ⚠️ This sentence must state "referencing workspaces stop working too". Clearing cannot be
        // undone,
        // and its impact spans many workspaces; the user should see the full cost in the confirm box.
        description={t("globalArk.clearDesc")}
        confirmLabel={t("globalArk.clear")}
        onConfirm={async () => {
          await api.deleteGlobalArkCredential();
          setClearing(false);
          await load();
        }}
      />

      <ArkNotActivatedDialog
        isOpen={notActivated !== null}
        message={notActivated?.message ?? ""}
        onOpenChange={(open) => {
          if (!open) {
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

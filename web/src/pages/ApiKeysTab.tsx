/**
 * Platform API Key: credentials for external agents / scripts / CI to call this platform's API.
 *
 * # It lives in "global settings", because it is account-level by nature
 *
 * Its table (auth_api_keys) **has no workspace_id column**; the backend fetches by the current
 * user's email.
 * It used to hang in a tab under "workspace settings", relying on a "personal" Chip and a note to
 * explain
 * "why the same keys show in another workspace"; that was text making up for the wrong location.
 * Now the location is right,
 * so that compensating note is not needed.
 *
 * The only thing kept is the line "these keys belong to your account": it does not answer "why is
 * it here",
 * but "how much power does this key have", which must be said on any page.
 *
 * # The opposite direction from Ark credentials; the UI must make it clear
 *
 *   Ark credentials (ArkTab in SettingsPage)  we use them to call **Ark**
 *   this component                            others use it to call **us**
 *
 * Both are called "API Key", so the description must name the direction, otherwise users paste
 * Ark keys
 * here (and then wonder why there is no input box).
 *
 * # The plain text is shown only once, and the UI must say so
 *
 * The backend only stores sha256; after the create response **the server itself cannot get the
 * plain text back**. So the success dialog
 * cannot close on click like a normal form: it must stay there, offer a copy button, and clearly
 * tell the user
 * "once closed you can never see it again". As a normal toast, users would find out they did not
 * save it only after closing, and could only revoke and recreate.
 */

import { useCallback, useState } from "react";
import { Button, Card, Input, Label, Separator, TextField, FieldError } from "@heroui/react";
import { Icon } from "@iconify/react";
import { api } from "@/data/api";
import { AsyncErrorBlock, Empty, ListSkeleton, SectionTitle } from "@/components/Page";
import { ConfirmModal, FormModal } from "@/components/FormModal";
import { formatRelative } from "@/lib/format";
import { useAsyncData } from "@/lib/useAsyncData";
import { validateApiKeyName, type ApiKey, type CreatedApiKey } from "@/types/domain";
import { usePrefs, useT } from "@/prefs/PrefsContext";
import { copyText } from "@/lib/clipboard";

export function ApiKeysTab() {
  const { language } = usePrefs();
  const t = useT();

  // useAsyncData rather than a bare useEffect: it has the isLoading / error / reload states.
  // A bare await that rejects stays on the skeleton forever; see the file header of
  // lib/useAsyncData.ts.
  //
  // **No longer depends on workspace**: this list is unrelated to the workspace, so the dependency
  // array is empty.
  const fetchKeys = useCallback(() => api.listApiKeys(), []);
  const {
    data: keys,
    error: loadError,
    isLoading,
    reload: load,
  } = useAsyncData(fetchKeys, t("apiKeys.loadFailed"));

  const [creating, setCreating] = useState(false);
  const [name, setName] = useState("");
  const [errors, setErrors] = useState<{ name?: string }>({});
  // The one just created (**with plain text**). When non-null, the "only once" dialog is shown.
  const [justCreated, setJustCreated] = useState<CreatedApiKey | null>(null);
  const [copied, setCopied] = useState(false);
  const [revoking, setRevoking] = useState<ApiKey | null>(null);

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

  if (keys === null) return null;

  async function copyKey(value: string) {
    // When both paths fail, **do not report an error**: the plain text is shown right there, and
    // the user can select and copy it by hand.
    // Showing an error would make people think the key was not created.
    setCopied(await copyText(value));
  }

  return (
    <div className="flex flex-col gap-4">
      <Card>
        <Card.Content className="flex flex-col gap-5 pt-6">
          <SectionTitle
            title="API Key"
            description={t("apiKeys.description")}
            actions={
              <Button
                size="sm"
                data-testid="create-api-key"
                onPress={() => {
                  setName("");
                  setErrors({});
                  setCreating(true);
                }}
              >
                {t("apiKeys.create")}
              </Button>
            }
          />

          {keys.length === 0 ? (
            <Empty
              icon="gravity-ui:key"
              title={t("apiKeys.emptyTitle")}
              description={t("apiKeys.emptyDesc")}
            />
          ) : (
            <div className="flex flex-col" data-testid="api-key-list">
              {keys.map((k, i) => (
                <div key={k.id}>
                  {i > 0 ? <Separator /> : null}
                  <div className="flex items-center gap-3 py-3">
                    <Icon icon="gravity-ui:key" className="size-4 shrink-0 text-muted" />
                    <div className="flex min-w-0 flex-col gap-0.5">
                      <p className="truncate text-sm font-medium">{k.name}</p>
                      <p className="font-mono text-xs text-muted">{k.prefix}…</p>
                    </div>
                    <div className="ml-auto flex items-center gap-3">
                      <span className="text-xs text-muted" data-testid="api-key-last-used">
                        {/* Missing means never used; this is exactly what users rely on to judge "can I revoke it",
                            so it must be stated, not shown as blank. */}
                        {k.lastUsedAt
                          ? t("apiKeys.lastUsed", { time: formatRelative(k.lastUsedAt, language) })
                          : t("apiKeys.neverUsed")}
                      </span>
                      <Button
                        size="sm"
                        variant="danger-soft"
                        data-testid="revoke-api-key"
                        onPress={() => setRevoking(k)}
                      >
                        {t("apiKeys.revoke")}
                      </Button>
                    </div>
                  </div>
                </div>
              ))}
            </div>
          )}

          <Separator />
          <p className="text-xs text-muted">
            {t("apiKeys.usageLabel")}
            <code className="font-mono">Authorization: Bearer &lt;key&gt;</code>
            {t("apiKeys.usageDocs")}{" "}
            <a
              className="text-accent underline-offset-2 hover:underline"
              href="/api/openapi.json"
              target="_blank"
              rel="noreferrer noopener"
            >
              /api/openapi.json
            </a>
            {t("apiKeys.usageLlms")}{" "}
            <a
              className="text-accent underline-offset-2 hover:underline"
              href="/llms.txt"
              target="_blank"
              rel="noreferrer noopener"
            >
              /llms.txt
            </a>
            {t("apiKeys.usageEnd")}
          </p>
        </Card.Content>
      </Card>

      <FormModal
        isOpen={creating}
        onOpenChange={setCreating}
        title={t("apiKeys.create")}
        description={t("apiKeys.createDesc")}
        submitLabel={t("common.create")}
        testId="create-api-key-modal"
        onSubmit={async () => {
          const err = validateApiKeyName(name, t);
          if (err) {
            setErrors({ name: err });
            return false;
          }
          const created = await api.createApiKey(name);
          // **The create dialog must be closed first, then the "only once" one shown.**
          //
          // If both dialogs were mounted at once, each would have a button with
          // data-testid="modal-submit"
          // (FormModal's submit button testid is fixed), so the action "click submit"
          // becomes ambiguous: e2e fails right away with a strict mode violation, and a person
          // clicking by hand
          // would see two overlays stacked on each other.
          setCreating(false);
          setJustCreated(created);
          setCopied(false);
          await load();
        }}
      >
        <TextField isInvalid={Boolean(errors.name)}>
          <Label>{t("apiKeys.name")}</Label>
          <Input
            autoFocus
            value={name}
            placeholder={t("apiKeys.namePlaceholder")}
            data-testid="api-key-name"
            onChange={(e) => {
              setName(e.target.value);
              setErrors({});
            }}
          />
          {errors.name ? <FieldError>{errors.name}</FieldError> : null}
        </TextField>
      </FormModal>

      {/* The "only once" dialog. FormModal rather than a plain notice: it needs an explicit
          "I have saved it" action for the user to confirm, rather than closing by clicking anywhere. */}
      <FormModal
        isOpen={justCreated !== null}
        onOpenChange={(open) => {
          if (!open) setJustCreated(null);
        }}
        title={t("apiKeys.createdTitle")}
        submitLabel={t("apiKeys.savedIt")}
        testId="api-key-created-modal"
        onSubmit={async () => {
          setJustCreated(null);
        }}
      >
        <div className="flex items-start gap-2 r-inset bg-warning/5 px-3 py-2.5">
          <Icon
            icon="gravity-ui:triangle-exclamation"
            className="mt-0.5 size-4 shrink-0 text-warning"
          />
          <p className="text-sm text-warning">
            {t("apiKeys.onceBefore")}
            <strong>{t("apiKeys.onceStrong")}</strong>
            {t("apiKeys.onceAfter")}
          </p>
        </div>

        <div className="flex flex-col gap-2">
          <p className="text-xs text-muted">{justCreated?.name}</p>
          <div className="flex items-start gap-2">
            <code
              className="min-w-0 flex-1 break-all r-inset bg-panel2 px-3 py-2 font-mono text-xs"
              data-testid="api-key-plaintext"
            >
              {justCreated?.key}
            </code>
            <Button
              size="sm"
              variant="outline"
              data-testid="copy-api-key"
              onPress={() => justCreated && copyKey(justCreated.key)}
            >
              {copied ? t("timeline.copied") : t("timeline.copy")}
            </Button>
          </div>
        </div>
      </FormModal>

      <ConfirmModal
        isOpen={revoking !== null}
        onOpenChange={(open) => {
          if (!open) setRevoking(null);
        }}
        title={t("apiKeys.revokeTitle", { name: revoking?.name ?? "" })}
        description={t("apiKeys.revokeDesc")}
        confirmLabel={t("apiKeys.revoke")}
        onConfirm={async () => {
          if (!revoking) return;
          await api.revokeApiKey(revoking.id);
          setRevoking(null);
          await load();
        }}
      />
    </div>
  );
}

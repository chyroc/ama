/**
 * Dialog for picking an Ark API Key: **type it in**, or **choose an existing one from the Volcengine account**.
 *
 * # How the work is split with ArkFetchPicker
 *
 * `ArkFetchPicker` is **the part that can be embedded in any dialog** (pick account, pick key); this component is
 * "the dialog shell + manual input + region picker", with that part embedded at the top.
 *
 * The personal and workspace pages **already have their own dialogs** (with testids e2e depends on), so those two only embed
 * `ArkFetchPicker`; only the Agent card has no ready-made dialog, so it uses this full one. All three share the same
 * "pick key" implementation instead of each writing its own.
 *
 * # ⚠️ The plaintext passes through the frontend only on the "type it in" path
 *
 * On the "choose from the Volcengine account" path **the frontend never gets the plaintext**: we only hand `{accountId, keyId}` to
 * the server, which fetches the plaintext itself, computes the mask, and stores it encrypted.
 *
 * Letting the frontend get the plaintext and PUT it back would be simpler (just reuse the apiKey field), but then a long-lived
 * Ark key would show up in the devtools network panel, could be read by browser extensions, and would stay in frontend memory.
 * The plaintext should only flow "server memory → database".
 *
 * # Auto-fetch is a nice extra; manual input always stays
 *
 * That path depends on a reverse-engineered frontend signature and undocumented internal console APIs of Volcengine, and may break when their frontend
 * changes. So when it is unavailable **only that half of the card is degraded**, and manual input works as usual: users must never be stuck on this path.
 */

import { useEffect, useRef, useState } from "react";
import {
  Description,
  FieldError,
  Input,
  Label,
  ListBox,
  Select,
  TextField,
} from "@heroui/react";
import { FormModal } from "@/components/FormModal";
import { ArkNotActivatedDialog } from "@/components/ArkNotActivatedDialog";
import { isArkSaveCancelled, saveArkCredentialWithConfirm } from "@/lib/arkSave";
import { useT } from "@/prefs/PrefsContext";
import {
  ArkFetchPicker,
  resolveArkSubmission,
  type ArkKeySource,
} from "@/components/ArkFetchPicker";
import {
  ARK_REGION_KEYS,
  ARK_REGIONS,
  type AgentArkCredential,
  type ArkCredentialDraft,
  type ArkKeyRef,
  type ArkRegion,
} from "@/types/domain";

export function ArkKeyPicker({
  isOpen,
  onOpenChange,
  title,
  onSubmit,
}: {
  isOpen: boolean;
  onOpenChange: (open: boolean) => void;
  title: string;
  /**
   * Saves a key. Returns the agent credential view after saving (may carry a transient warning).
   *
   * The two-step confirmation for "Managed Agents not activated" (422 ark_managed_agent_not_open →
   * resend with confirmNotActivated) is handled here, so callers only write a plain save call.
   */
  onSubmit: (draft: ArkCredentialDraft) => Promise<AgentArkCredential>;
}) {
  const t = useT();
  const [region, setRegion] = useState<ArkRegion>(ARK_REGIONS[0].id);
  const [apiKey, setApiKey] = useState("");

  // The one chosen from the Volcengine account. null means use the manually typed one below.
  const [pickedRef, setPickedRef] = useState<ArkKeyRef | null>(null);
  /** The key source the user chose. **Defaults to fetch**; when unavailable the picker forces a fallback to manual input. */
  const [keySource, setKeySource] = useState<ArkKeySource>("fetch");
  /** Error for the manual path. When using fetch the manual input is not rendered, so the error must go to the dialog then. */
  const [keyError, setKeyError] = useState<string | null>(null);
  // Incremented on every open, so ArkFetchPicker reloads (a half-finished selection from last time should not stay).
  const [openSeq, setOpenSeq] = useState(0);
  // The second confirmation dialog for "not activated".
  const [notActivated, setNotActivated] = useState<{ message: string } | null>(null);
  const notActivatedResolver = useRef<((confirmed: boolean) => void) | null>(null);

  useEffect(() => {
    if (!isOpen) return;
    setApiKey("");
    setPickedRef(null);
    setKeySource("fetch");
    setKeyError(null);
    setOpenSeq((n) => n + 1);
  }, [isOpen]);

  return (
    <>
    <FormModal
      isOpen={isOpen}
      onOpenChange={onOpenChange}
      title={title}
      submitLabel={t("common.save")}
      testId="ark-key-picker"
      onSubmit={async () => {
        // Merge the two sources; see the comment on resolveArkSubmission (check order + where errors go).
        const resolved = resolveArkSubmission(keySource, pickedRef, apiKey, t);
        if (resolved instanceof Error) {
          // ⚠️ When using "choose from Volcengine" the manual input is not rendered, so putting the error into its FieldError would hide it.
          if (keySource === "fetch" && !pickedRef) throw resolved;
          setKeyError(resolved.message);
          return false;
        }
        const draft: ArkCredentialDraft = { apiKey: "", region, ...resolved };
        try {
          // When not activated, close the form dialog first and let the separate confirm dialog take over, so the confirm dialog is a sibling of
          // FormModal (see the fragment in return), not nested inside it.
          await saveArkCredentialWithConfirm(
            draft,
            onSubmit,
            (message) =>
              new Promise<boolean>((resolvePromise) => {
                onOpenChange(false);
                notActivatedResolver.current = resolvePromise;
                setNotActivated({ message });
              }),
          );
        } catch (e) {
          if (isArkSaveCancelled(e)) return false;
          // Errors like a bad key: shown in FormModal's error bar.
          throw e;
        }
      }}
    >
      <div className="flex flex-col gap-5">
        {/* The pick account and key part, sharing one implementation with the personal / workspace pages. */}
        <ArkFetchPicker
          resetKey={openSeq}
          picked={pickedRef}
          source={keySource}
          onSourceChange={setKeySource}
          onPick={(ref) => {
            setPickedRef(ref);
            // Choosing one clears the manual input; the server returns an error when given both.
            if (ref) {
              setApiKey("");
              setKeyError(null);
            }
          }}
          /*
            The manual path **always stays** (auto-fetch is a nice extra, see the file header), but it is **not rendered by default**:
            it only appears when actually used. "When it appears" is decided by ArkFetchPicker in one place,
            so the three places do not each write that check (see its file header).
          */
          manualSlot={
            <TextField isInvalid={Boolean(keyError)}>
              <Label>API Key</Label>
              <Input
                value={apiKey}
                onChange={(e) => {
                  setApiKey(e.target.value);
                  setKeyError(null);
                }}
                placeholder={t("arkKeyPicker.keyPlaceholder")}
                data-testid="ark-key-input"
              />
              {keyError ? <FieldError>{keyError}</FieldError> : null}
              <Description>
                {t("arkKeyPicker.consoleBefore")}
                <a
                  href="https://console.volcengine.com/ark/region:ark+cn-beijing/apiKey"
                  target="_blank"
                  rel="noopener noreferrer"
                  className="text-accent"
                >
                  {t("arkKeyPicker.consoleLink")}
                </a>
                {t("arkKeyPicker.consoleAfter")}
              </Description>
            </TextField>
          }
        />

        {/* Region picker: the shape copies the one in the SettingsPage dialog (the same HeroUI usage). */}
        <Select
          selectedKey={region}
          onSelectionChange={(k) => setRegion(String(k) as ArkRegion)}
        >
          <Label>{t("arkCred.region")}</Label>
          <Select.Trigger data-testid="ark-region-input">
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
      </div>
    </FormModal>

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
    </>
  );
}

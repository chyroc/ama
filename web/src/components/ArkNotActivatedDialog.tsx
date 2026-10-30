/**
 * Confirm dialog for "this Ark account has not activated Managed Agents yet".
 *
 * Shown when saving a credential is blocked by 422 ark_managed_agent_not_open. Exits:
 *
 *   Activate (primary button; opens the console in a new tab, the dialog stays: the user comes back to save after activating)
 *   Save anyway (secondary exit; resends with confirmNotActivated)
 *   Cancel (does nothing)
 *
 * Uses the project's existing HeroUI v3 Modal compound structure, same style as FormModal (Backdrop wraps
 * Container, onPress instead of onClick). The main CTA is solid and "Save anyway" is outline,
 * matching the site-wide rule "primary action solid, secondary actions muted".
 */

import { Button, Modal } from "@heroui/react";
import { Icon } from "@iconify/react";
import { useT } from "@/prefs/PrefsContext";
import { ARK_MANAGED_AGENTS_URL } from "@/types/domain";

export function ArkNotActivatedDialog({
  isOpen,
  onOpenChange,
  message,
  onConfirmSave,
}: {
  isOpen: boolean;
  onOpenChange: (open: boolean) => void;
  /** Explanation from the backend (with account id and request_id), shown as is. */
  message: string;
  /** The user clicked "Save anyway". */
  onConfirmSave: () => void;
}) {
  const t = useT();
  return (
    <Modal.Backdrop isOpen={isOpen} onOpenChange={onOpenChange}>
      <Modal.Container size="sm">
        <Modal.Dialog data-testid="ark-not-activated-modal">
          <Modal.Header>
            <Modal.Heading>{t("arkNotActivated.title")}</Modal.Heading>
          </Modal.Header>
          <Modal.Body className="flex flex-col gap-4">
            <div className="flex items-start gap-2 r-inset bg-warning/5 px-3 py-2.5">
              <Icon
                icon="gravity-ui:triangle-exclamation"
                className="mt-0.5 size-4 shrink-0 text-warning"
              />
              <p className="text-sm text-warning" data-testid="ark-not-activated-message">
                {message}
              </p>
            </div>
            <p className="text-sm text-muted">{t("arkNotActivated.hint")}</p>
          </Modal.Body>
          <Modal.Footer>
            <Button variant="ghost" onPress={() => onOpenChange(false)}>
              {t("common.cancel")}
            </Button>
            <Button
              variant="outline"
              onPress={onConfirmSave}
              data-testid="ark-not-activated-save-anyway"
            >
              {t("arkNotActivated.saveAnyway")}
            </Button>
            {/* Open the console in a new tab and keep the dialog: the user comes back to click save after activating. */}
            <Button
              onPress={() => window.open(ARK_MANAGED_AGENTS_URL, "_blank", "noopener,noreferrer")}
              data-testid="ark-not-activated-open-console"
            >
              {t("arkNotActivated.activate")}
            </Button>
          </Modal.Footer>
        </Modal.Dialog>
      </Modal.Container>
    </Modal.Backdrop>
  );
}

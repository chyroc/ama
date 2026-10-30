/**
 * Shared skeleton for form modals.
 *
 * Create/edit in all six modules is "a form in a modal, a primary button + cancel at the bottom". Pulled out so each place
 * does not rewrite the four-layer Modal structure and the submit/error state. Design rule: the modal is as narrow as its content (max-w-md/lg),
 * and there is always an escape exit (cancel) at the bottom.
 */

import { Button, Modal } from "@heroui/react";
import { useState, type ReactNode } from "react";
import { useT } from "@/prefs/PrefsContext";

interface FormModalProps {
  isOpen: boolean;
  onOpenChange: (open: boolean) => void;
  title: string;
  description?: string;
  submitLabel?: string;
  /** Return false to mean validation failed; the modal stays open. */
  onSubmit: () => Promise<boolean | void>;
  children: ReactNode;
  /** Maps to Modal.Container's size. Form modals are as narrow as their content; do not always fill the screen. */
  size?: "sm" | "md" | "lg";
  testId?: string;
}

export function FormModal({
  isOpen,
  onOpenChange,
  title,
  description,
  submitLabel,
  onSubmit,
  children,
  size = "md",
  testId,
}: FormModalProps) {
  const t = useT();
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function handleSubmit() {
    setSubmitting(true);
    setError(null);
    try {
      const ok = await onSubmit();
      if (ok !== false) onOpenChange(false);
    } catch (e) {
      setError(e instanceof Error ? e.message : t("common.actionFailed"));
    } finally {
      setSubmitting(false);
    }
  }

  return (
    /*
      The correct structure for a controlled modal: Modal.Backdrop itself takes isOpen/onOpenChange and is the root,
      and Modal.Container must be **nested inside** the Backdrop (Backdrop is RAC's ModalOverlay,
      Container is ModalDialog). If written as siblings, RAC never receives the exit animation's animationend,
      and the backdrop stays in data-exiting forever, blocking clicks on the whole page.
    */
    <Modal.Backdrop
      isOpen={isOpen}
      onOpenChange={(open) => {
        if (!open) setError(null);
        onOpenChange(open);
      }}
    >
      <Modal.Container size={size}>
        <Modal.Dialog data-testid={testId}>
          <Modal.Header>
            <Modal.Heading>{title}</Modal.Heading>
            {description ? <p className="text-sm text-muted">{description}</p> : null}
          </Modal.Header>
          <Modal.Body className="flex flex-col gap-5">
            {children}
            {error ? (
              // The testid lets us assert "the error is shown here": some validation errors are deliberately thrown to this line
              // instead of the field's FieldError, because that field may not be rendered at all
              // (e.g. the manual input does not exist when ArkFetchPicker uses auto-fetch).
              <p className="text-sm text-danger" role="alert" data-testid="modal-error">
                {error}
              </p>
            ) : null}
          </Modal.Body>
          <Modal.Footer>
            <Button variant="ghost" onPress={() => onOpenChange(false)}>
              {t("common.cancel")}
            </Button>
            <Button onPress={handleSubmit} isPending={submitting} data-testid="modal-submit">
              {submitLabel ?? t("common.save")}
            </Button>
          </Modal.Footer>
        </Modal.Dialog>
      </Modal.Container>
    </Modal.Backdrop>
  );
}

/** Confirm dialog for dangerous actions. All delete actions use it, so each place does not build its own. */
export function ConfirmModal({
  isOpen,
  onOpenChange,
  title,
  description,
  confirmLabel,
  onConfirm,
}: {
  isOpen: boolean;
  onOpenChange: (open: boolean) => void;
  title: string;
  description: string;
  confirmLabel?: string;
  onConfirm: () => Promise<void>;
}) {
  const t = useT();
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function run() {
    setSubmitting(true);
    setError(null);
    try {
      await onConfirm();
      onOpenChange(false);
    } catch (e) {
      setError(e instanceof Error ? e.message : t("common.actionFailed"));
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <Modal.Backdrop isOpen={isOpen} onOpenChange={onOpenChange}>
      <Modal.Container size="sm">
        <Modal.Dialog>
          <Modal.Header>
            <Modal.Heading>{title}</Modal.Heading>
          </Modal.Header>
          <Modal.Body>
            <p className="text-sm text-muted">{description}</p>
            {error ? (
              <p className="mt-3 text-sm text-danger" role="alert">
                {error}
              </p>
            ) : null}
          </Modal.Body>
          <Modal.Footer>
            <Button variant="ghost" onPress={() => onOpenChange(false)}>
              {t("common.cancel")}
            </Button>
            <Button
              variant="danger-soft"
              onPress={run}
              isPending={submitting}
              data-testid="confirm-danger"
            >
              {confirmLabel ?? t("common.delete")}
            </Button>
          </Modal.Footer>
        </Modal.Dialog>
      </Modal.Container>
    </Modal.Backdrop>
  );
}

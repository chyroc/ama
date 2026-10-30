/**
 * The two-step flow for saving Ark credentials.
 *
 * On save the backend first calls a read-only Ark API with the key. Three outcomes:
 *
 *   bad key (422 ark_key_invalid)                → hard error; fix the key and resend, no confirm;
 *   not activated (422 ark_managed_agent_not_open) → let the user pick "go activate / save anyway";
 *                                                  the latter resends with confirmNotActivated;
 *   timeout/5xx                                   → the backend lets it through, response has a warning,
 *                                                  save succeeds, no conflict.
 *
 * This helper only handles the "not activated → ask → retry with confirm" branch; a bad key is
 * thrown straight to
 * the form's existing modal-error bar; the warning comes back with the normal save response and
 * the caller shows it.
 *
 * Decisions only look at the structured code (ApiError.code), never match Chinese sentences.
 */

import { ApiError } from "@/data/http";
import {
  ARK_REJECT_NOT_ACTIVATED,
  type ArkCredential,
  type ArkCredentialDraft,
  type AgentArkCredential,
  type GlobalArkCredential,
} from "@/types/domain";

/** One save call: returns a credential view of any level (all three shapes carry a warning field). */
type SaveCall<D extends ArkCredentialDraft, R> = (draft: D) => Promise<R>;

/**
 * Runs one save; on hitting "not activated" it calls askConfirm; after the user confirms it
 * resends with the flag.
 *
 * askConfirm returns true = save anyway, false = go activate (then it rejects with a
 * CancellationError; the caller should close normally / not report an error. This is not a failure).
 */
export async function saveArkCredentialWithConfirm<
  D extends ArkCredentialDraft,
  R extends ArkCredential | GlobalArkCredential | AgentArkCredential,
>(
  baseDraft: D,
  save: SaveCall<D, R>,
  askConfirm: (message: string) => Promise<boolean>,
): Promise<R> {
  try {
    return await save(baseDraft);
  } catch (e) {
    if (!(e instanceof ApiError) || e.code !== ARK_REJECT_NOT_ACTIVATED) {
      throw e;
    }
    const confirmed = await askConfirm(e.message);
    if (!confirmed) {
      throw new ArkSaveCancelled();
    }
    return save({ ...baseDraft, confirmNotActivated: true });
  }
}

/** The user picked "go activate" on the not-activated confirm bar: not an error, the caller just closes the dialog quietly. */
export class ArkSaveCancelled extends Error {
  constructor() {
    super("用户取消保存（去开通）");
    this.name = "ArkSaveCancelled";
  }
}

/** Tells whether a failed save was "the user cancelled on purpose"; if so, do not show it on the error bar. */
export function isArkSaveCancelled(e: unknown): boolean {
  return e instanceof ArkSaveCancelled;
}

/**
 * Writes a piece of text to the clipboard. Returns true on success.
 *
 * **`navigator.clipboard` only exists in a secure context (https or localhost)**. When the
 * page is opened at `http://<hostname or LAN IP>:9999` it is entirely undefined, and that is the
 * most common way self-hosted deployments are accessed, so we cannot rely on it alone. The
 * fallback is the old `document.execCommand("copy")`: it is marked deprecated,
 * but all major browsers still support it in non-secure contexts, and it works as long as it is
 * called in the same event as the user's click.
 *
 * Order is new first, then old: even in a secure context `clipboard.writeText` can be rejected
 * by a permissions policy (iframe,
 * no focus); in that case we also fall back to execCommand and try again.
 */
export async function copyText(text: string): Promise<boolean> {
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // fall through to the execCommand path below.
    }
  }
  return copyWithExecCommand(text);
}

function copyWithExecCommand(text: string): boolean {
  const ta = document.createElement("textarea");
  ta.value = text;
  // Read-only + moved off screen: no mobile keyboard pops up, and the page does not jump.
  ta.setAttribute("readonly", "");
  ta.style.position = "fixed";
  ta.style.top = "0";
  ta.style.left = "-9999px";
  ta.style.opacity = "0";
  document.body.appendChild(ta);
  // Copying changes the current selection (e.g. text the user just selected); restore it after.
  const selection = document.getSelection();
  const previous = selection && selection.rangeCount > 0 ? selection.getRangeAt(0) : null;
  const active = document.activeElement as HTMLElement | null;
  try {
    ta.select();
    ta.setSelectionRange(0, text.length);
    return document.execCommand("copy");
  } catch {
    return false;
  } finally {
    document.body.removeChild(ta);
    if (previous && selection) {
      selection.removeAllRanges();
      selection.addRange(previous);
    }
    active?.focus?.();
  }
}

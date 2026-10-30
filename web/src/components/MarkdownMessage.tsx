/**
 * Markdown rendering for agent replies: a wrapper around `react-markdown` + `remark-gfm`.
 *
 * What the agent outputs is already standard Markdown (headings, code blocks, tables, lists). Before, it was put straight into
 * `whitespace-pre-wrap` as plain text, so `**bold**` showed up with its asterisks and tables collapsed into a pile of
 * pipes. Now it goes through GFM, and code blocks get one-click copy.
 *
 * Three things you must not change on assumption:
 *
 * 1. **The heavy part is isolated by React.lazy.** The file that really imports the renderer is `MarkdownBody.tsx`;
 *    here `React.lazy` pushes it into its own chunk, so the main bundle does not carry unified / micromark.
 *
 * 2. **The Suspense fallback must be the same text, not a spinner or blank.**
 *    The chat streams text, and the draft re-renders on every frame; the fallback only shows when the chunk has really not arrived
 *    (the short moment when you first open the chat page). A spinner makes the whole reply "flash" when loading finishes,
 *    and blank makes content appear out of nowhere. Use the plain text version: the words are always there, just not formatted yet.
 *
 * 3. **Styles are scoped inside `.md-message`**; the rules are in `index.css`, all colors use theme tokens,
 *    and light/dark follow `.dark` automatically.
 */

import { lazy, Suspense } from "react";

const MarkdownBody = lazy(() =>
  import("@/components/MarkdownBody").then((m) => ({ default: m.MarkdownBody })),
);

interface MarkdownMessageProps {
  /** Raw Markdown text. */
  children: string;
  /**
   * Still streaming.
   *
   * An unclosed code block (only half a ``` written) is treated as a code block running to the end, so the layout does not jump.
   */
  streaming?: boolean;
  className?: string;
}

export function MarkdownMessage({ children, streaming, className }: MarkdownMessageProps) {
  // md-message is the hook for the Markdown styles in index.css.
  const cls = className ? `md-message ${className}` : "md-message";

  return (
    <Suspense fallback={<div className={`${cls} whitespace-pre-wrap`}>{children}</div>}>
      <MarkdownBody markdown={children} streaming={streaming} className={cls} />
    </Suspense>
  );
}

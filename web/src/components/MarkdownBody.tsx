/**
 * Where Markdown is actually rendered. **This module is built into its own chunk.**
 *
 * Uses `react-markdown` + `remark-gfm` (tables, strikethrough, task lists, autolinks).
 * It is split out to isolate heavy dependencies: the unified / micromark / mdast chain is only needed by the chat page and the run detail page.
 * The repo did the same for recharts (UsagePage uses React.lazy in `router.tsx`).
 *
 * So: **apart from MarkdownMessage.tsx, nothing else may import this file**;
 * one static import pulls the chunk back into the main bundle.
 *
 * Security: react-markdown **does not render raw HTML** by default (things like `<script>` show as plain text),
 * and it filters dangerous links like `javascript:`, so no extra sanitizer is needed.
 */

import { useEffect, useRef, useState, type ComponentPropsWithoutRef, type MouseEvent } from "react";
import ReactMarkdown, { type Components } from "react-markdown";
import remarkGfm from "remark-gfm";
import { Icon } from "@iconify/react";
import { useT } from "@/prefs/PrefsContext";
import { copyText } from "@/lib/clipboard";

export interface MarkdownBodyProps {
  markdown: string;
  streaming?: boolean;
  className: string;
}

/**
 * External links always open in a new window with noopener, so the opened page cannot get `window.opener`.
 * Anchor links (#xxx) are not intercepted; they use the browser's default in-page jump.
 */
function MdLink({ href, children, ...rest }: ComponentPropsWithoutRef<"a">) {
  if (!href || href.startsWith("#")) {
    return (
      <a href={href} {...rest}>
        {children}
      </a>
    );
  }
  return (
    <a href={href} target="_blank" rel="noopener noreferrer" {...rest}>
      {children}
    </a>
  );
}

/**
 * Code block: wrapped in an outer box with a "copy" button at the top right.
 *
 * `navigator.clipboard` only exists in a secure context (https or localhost), so we use copyText from `lib/clipboard`
 * (it falls back to execCommand); when both fail we show visible feedback (the icon turns into a cross) instead of failing silently.
 */
function MdPre({ children, ...rest }: ComponentPropsWithoutRef<"pre">) {
  const t = useT();
  const preRef = useRef<HTMLPreElement | null>(null);
  const [state, setState] = useState<"idle" | "ok" | "fail">("idle");

  useEffect(() => {
    if (state === "idle") return;
    const id = window.setTimeout(() => setState("idle"), 1500);
    return () => window.clearTimeout(id);
  }, [state]);

  const onCopy = async (e: MouseEvent) => {
    e.preventDefault();
    setState((await copyText(preRef.current?.innerText ?? "")) ? "ok" : "fail");
  };

  const icon =
    state === "ok" ? "gravity-ui:check" : state === "fail" ? "gravity-ui:xmark" : "gravity-ui:copy";

  return (
    <div className="md-code group relative">
      <pre ref={preRef} {...rest}>
        {children}
      </pre>
      <button
        type="button"
        onClick={onCopy}
        title={t("timeline.copy")}
        aria-label={t("timeline.copy")}
        className="md-code__copy absolute top-1.5 right-1.5 flex size-7 items-center justify-center rounded-md text-muted opacity-0 transition-opacity hover:bg-default hover:text-foreground focus-visible:opacity-100 group-hover:opacity-100"
      >
        <Icon icon={icon} className="size-3.5" />
      </button>
    </div>
  );
}

/** Tables must scroll horizontally in a narrow bubble and must not stretch the bubble. */
function MdTable(props: ComponentPropsWithoutRef<"table">) {
  return (
    <div className="md-table-scroll">
      <table {...props} />
    </div>
  );
}

const COMPONENTS: Components = {
  a: ({ node: _node, ...props }) => <MdLink {...props} />,
  pre: ({ node: _node, ...props }) => <MdPre {...props} />,
  table: ({ node: _node, ...props }) => <MdTable {...props} />,
};

const REMARK_PLUGINS = [remarkGfm];

export function MarkdownBody({ markdown, streaming, className }: MarkdownBodyProps) {
  return (
    // data-streaming is just a hook: styles can be adjusted separately during streaming if needed.
    <div className={className} data-streaming={streaming ? "true" : undefined}>
      <ReactMarkdown remarkPlugins={REMARK_PLUGINS} components={COMPONENTS}>
        {markdown}
      </ReactMarkdown>
    </div>
  );
}

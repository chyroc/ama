/**
 * Empty state: "icon + title + description + action", centered.
 *
 * Built only with Tailwind, no state of its own. The shape is a compound component
 * (EmptyState.Header / Media / Title / Description / Content), same as other HeroUI components here.
 */

import type { HTMLAttributes, ReactNode } from "react";

type DivProps = HTMLAttributes<HTMLDivElement> & { children?: ReactNode };

function cx(...parts: (string | undefined | false)[]) {
  return parts.filter(Boolean).join(" ");
}

function EmptyStateRoot({ className, ...props }: DivProps) {
  return (
    <div
      {...props}
      className={cx("flex flex-col items-center justify-center gap-4 px-6 py-10 text-center", className)}
    />
  );
}

function EmptyStateHeader({ className, ...props }: DivProps) {
  return <div {...props} className={cx("flex max-w-sm flex-col items-center gap-2", className)} />;
}

/** variant has only one value for now, icon: the icon sits in a light rounded square. */
function EmptyStateMedia({ className, variant: _variant = "icon", ...props }: DivProps & { variant?: "icon" }) {
  return (
    <div
      {...props}
      aria-hidden
      className={cx(
        "mb-1 flex size-10 items-center justify-center rounded-xl bg-default text-muted [&_svg]:size-5",
        className,
      )}
    />
  );
}

function EmptyStateTitle({ className, ...props }: HTMLAttributes<HTMLHeadingElement>) {
  return <h3 {...props} className={cx("text-sm font-medium text-foreground", className)} />;
}

function EmptyStateDescription({ className, ...props }: HTMLAttributes<HTMLParagraphElement>) {
  return <p {...props} className={cx("text-sm text-muted", className)} />;
}

function EmptyStateContent({ className, ...props }: DivProps) {
  return <div {...props} className={cx("flex items-center justify-center gap-2", className)} />;
}

export const EmptyState = Object.assign(EmptyStateRoot, {
  Header: EmptyStateHeader,
  Media: EmptyStateMedia,
  Title: EmptyStateTitle,
  Description: EmptyStateDescription,
  Content: EmptyStateContent,
});

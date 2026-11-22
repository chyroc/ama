import type { ButtonHTMLAttributes } from "react";
import { cn } from "../cn";

export type AzureButtonVariant = "primary" | "secondary" | "ghost" | "accent" | "quiet";

/** Adapted from the upstream Button primitive; see ../README.md. */
export function AzureButton({
  variant = "secondary",
  className,
  type = "button",
  ...props
}: ButtonHTMLAttributes<HTMLButtonElement> & { variant?: AzureButtonVariant }) {
  const variants: Record<AzureButtonVariant, string> = {
    primary: "bg-ink text-canvas hover:opacity-90 shadow-[inset_0_1px_0_rgba(255,255,255,0.14)]",
    secondary: "bg-surface text-ink shadow-btn hover:bg-inset aria-expanded:bg-hover",
    ghost: "bg-hover-2 text-ink hover:bg-line-strong",
    accent: "bg-accent text-white hover:bg-accent-ink shadow-[inset_0_1px_0_rgba(255,255,255,0.14)]",
    quiet: "text-ink hover:bg-hover",
  };
  return (
    <button
      type={type}
      className={cn(
        "inline-flex h-[27px] select-none items-center justify-center gap-1.5 rounded-full px-3 text-[13px] leading-none font-medium transition-[transform,background-color,opacity] duration-150 ease-out active:scale-[0.96] disabled:pointer-events-none disabled:opacity-50",
        variants[variant],
        className,
      )}
      {...props}
    />
  );
}

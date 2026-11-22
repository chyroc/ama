import type { ReactNode } from "react";
import { usePrefs } from "@/prefs/PrefsContext";

/**
 * Semantic AMA components choose a complete implementation per theme.
 * Callers supply data and actions, never a visual variant.
 */
export function ThemedComponent({
  classic,
  azure,
}: {
  classic: () => ReactNode;
  azure: () => ReactNode;
}) {
  const { theme } = usePrefs();
  return theme === "azure" ? azure() : classic();
}

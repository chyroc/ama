/**
 * "Open in the Ark console": a small entry that jumps to the matching resource in the Ark console.
 *
 * Shared by agent detail, skill detail and session detail. It is one component instead of a copy per page
 * because **the rules for "when not to show it" are three and none is obvious** (see below); copied three times,
 * one place would miss one, and a miss shows up as a link to an Ark 404 page.
 *
 * # This is a secondary feature and must step back visually
 *
 * A ghost icon-only button + Tooltip, after the main actions. Not a big button with text:
 * most users do not need it (it is for comparing with the Ark-side resource while debugging), and making it prominent would crowd out
 * the real main actions like "Save" and "Try it".
 *
 * # The three "do not render" rules
 *
 * 1. Data not loaded yet (loading): no placeholder, to avoid layout jumps;
 * 2. Not created on the Ark side yet (no id): **this is a normal state**, not an error; see ArkConsoleRef;
 * 3. No Ark credential configured (no region): the correct URL cannot be built.
 *
 * All three end up as `arkConsoleUrl()` returning null, so this only checks once.
 *
 * ⚠️ **Do not change it to "render a disabled button"**: that would put a button that cannot be clicked on every agent detail page
 * that never had a chat, and users would keep clicking it and then ask why. If there is nothing, show nothing.
 */
import { Icon } from "@iconify/react";
import { Tooltip } from "@heroui/react";

import { arkConsoleUrl } from "@/lib/arkConsole";
import { useT } from "@/prefs/PrefsContext";
import type { ArkConsoleRef } from "@/data/api";

interface ArkConsoleLinkProps {
  /** Which of the three resource kinds; decides the URL path segment. */
  kind: "agents" | "skills" | "sessions";
  /** Reference from the backend; pass null before it is loaded (nothing rendered). */
  refData: ArkConsoleRef | null;
  /** Message key for the Tooltip and aria-label. */
  labelKey: "arkConsole.openAgent" | "arkConsole.openSkill" | "arkConsole.openSession";
  /** Anchor for e2e. */
  testId: string;
}

export function ArkConsoleLink({ kind, refData, labelKey, testId }: ArkConsoleLinkProps) {
  const t = useT();

  const id =
    kind === "agents"
      ? refData?.arkAgentId
      : kind === "skills"
        ? refData?.arkSkillId
        : refData?.arkSessionId;
  const href = arkConsoleUrl(kind, id, refData?.region);
  if (!href) return null;

  const label = t(labelKey);
  return (
    <Tooltip>
      <Tooltip.Trigger>
        {/*
         * **Use <a> instead of onPress + window.open**: middle click, Cmd/Ctrl+click and
         * right-click "open in new tab" are free browser behaviors for anchors; onPress would
         * lose them all (same as the note in SandboxTerminal.tsx).
         *
         * `rel` is required: without it the opened page could control this page back through window.opener.
         *
         * The style follows the icon-only external link in SandboxTerminal.tsx, to keep the two the same.
         */}
        <a
          href={href}
          target="_blank"
          rel="noopener noreferrer"
          aria-label={label}
          className="text-muted hover:bg-surface-secondary hover:text-foreground r-inset flex size-8 items-center justify-center no-underline cursor-[var(--cursor-interactive)]"
          data-testid={testId}
        >
          <Icon icon="gravity-ui:arrow-up-right-from-square" className="size-4" />
        </a>
      </Tooltip.Trigger>
      <Tooltip.Content>{label}</Tooltip.Content>
    </Tooltip>
  );
}

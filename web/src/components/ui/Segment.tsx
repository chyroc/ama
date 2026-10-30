/**
 * Segmented switch (single choice, empty selection not allowed).
 *
 * Built on the OSS ToggleButtonGroup (react-aria handles keyboard and screen reader semantics). This only does two things:
 *
 * 1. Narrow the public shape to "one selectedKey + a callback with a single key", so callers do not handle a Set;
 * 2. Give it a "light track + raised selected item" look; colors use the `--segment` token in index.css.
 *
 * `disallowEmptySelection` is hard-coded: clicking the selected option again does not deselect it,
 * so the "nothing selected" state cannot happen and callers need no fallback.
 */

import type { ReactNode } from "react";
import { ToggleButton, ToggleButtonGroup } from "@heroui/react";
import type { Key } from "@heroui/react";

interface SegmentProps {
  selectedKey: Key;
  onSelectionChange: (key: Key) => void;
  size?: "sm" | "md";
  className?: string;
  children: ReactNode;
  "aria-label"?: string;
  "data-testid"?: string;
}

function SegmentRoot({
  selectedKey,
  onSelectionChange,
  size = "md",
  className,
  children,
  ...rest
}: SegmentProps) {
  return (
    <ToggleButtonGroup
      {...rest}
      size={size}
      isDetached
      selectionMode="single"
      disallowEmptySelection
      selectedKeys={[selectedKey]}
      onSelectionChange={(keys) => {
        const [next] = keys;
        if (next != null) onSelectionChange(next);
      }}
      className={`ama-segment gap-0.5 rounded-lg bg-default p-0.5 ${className ?? ""}`}
    >
      {children}
    </ToggleButtonGroup>
  );
}

interface SegmentItemProps {
  id: Key;
  children: ReactNode;
  className?: string;
  "data-testid"?: string;
}

function SegmentItem({ id, children, className, ...rest }: SegmentItemProps) {
  return (
    <ToggleButton {...rest} id={id} className={`ama-segment__item ${className ?? ""}`}>
      {children}
    </ToggleButton>
  );
}

export const Segment = Object.assign(SegmentRoot, { Item: SegmentItem });

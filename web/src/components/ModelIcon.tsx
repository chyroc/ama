/**
 * Model vendor marks: draws a vendor icon before each model in the model picker.
 *
 * Paths come from `modelIconPaths.ts` (generated, upstream lobe-icons, MIT);
 * for why they are inlined instead of installed as a dependency, see the file header of `scripts/model-icons/generate.py`.
 *
 * # What to draw for an unknown vendor
 *
 * **No fallback icon; return null.** This is deliberate: when a new vendor is added to `MODEL_SPECS` without an icon
 * here, "one icon missing" is easier to notice than "all vendors show the same robot head"; the latter looks
 * normal and nobody stops at it in review. For the same reason no initial-letter square: at 16px such a placeholder is hard to
 * tell from a real icon.
 *
 * The picker layout already allows a missing icon (`gap` is on the flex, so one missing does not misalign).
 *
 * # Colors do not follow the theme, and this was calculated
 *
 * We take the brand-color versions; `fill` is a hard-coded hex: a vendor mark should keep its own colors.
 * The recognizability threshold for non-text graphics is 3:1 (WCAG 1.4.11). Measured contrast:
 *
 *              vs light #fff    vs dark #161719
 *   deepseek       4.33            4.14
 *   zhipu          5.20            3.45
 *   doubao blue    6.98            2.57
 *   doubao cyan    1.66           10.83
 *
 * Both sides pass, so no `dark:` variants are needed (the repo does not use them anywhere; light/dark switch only through semantic tokens,
 * see docs/guides/styling.md). **The two doubao rows are the key**: it is a multi-color mark; the blue is
 * only 2.57 on dark and the cyan only 1.66 on light, so neither color alone is enough, but the two complement each other in one shape:
 * under either theme at least one color has high contrast, so the whole mark is always visible.
 * Think about this before switching it to a single-color version: pure blue would look dull on dark.
 */

import { MODEL_ICON_PATHS, MODEL_ICON_VIEWBOX } from "@/components/modelIconPaths";

export function ModelIcon({
  vendor,
  className = "size-4",
}: {
  /** The `vendor` field in MODEL_SPECS, used for the lookup. */
  vendor: string;
  /** Size and extra styles. Default size-4, the same height as a body text line in the picker. */
  className?: string;
}) {
  const paths = MODEL_ICON_PATHS[vendor];
  if (!paths) return null;

  return (
    <svg
      viewBox={`0 0 ${MODEL_ICON_VIEWBOX} ${MODEL_ICON_VIEWBOX}`}
      className={`${className} shrink-0`}
      // Purely decorative: the model name and model ID come right after the icon, so having the screen reader read the vendor name again is repetition.
      aria-hidden
      data-testid={`model-icon-${vendor}`}
    >
      {paths.map((p, i) => (
        // Index as key: this list is in a fixed order in the generated file and is never reordered or changed.
        <path key={i} d={p.d} fill={p.fill} />
      ))}
    </svg>
  );
}

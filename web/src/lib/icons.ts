/**
 * Offline icons: register the local @iconify-json/gravity-ui into @iconify/react.
 *
 * **Without registering, `<Icon icon="gravity-ui:x">` fetches the SVG from api.iconify.design at
 * runtime.**
 * `@iconify-json/gravity-ui` has long been in package.json, but installed does not mean used:
 * @iconify/react
 * only knows offline data that was "explicitly added via addCollection"; anything else goes to the
 * network. So on a machine that cannot reach
 * that CDN (private network), the whole page shows no icons at all, and **with no error and no
 * placeholder**, just quietly empty.
 * Icon-only buttons (the "new conversation" + in the chat page's left column) become invisible
 * clickable areas.
 *
 * This is not about occasional CDN hiccups; "works offline" should simply be the default:
 *   - the UI must be complete on private or disconnected networks;
 *   - the first screen should not wait an extra cross-origin round trip for a few 14px icons (seen
 *     locally as ERR_CONNECTION_TIMED_OUT);
 *   - e2e should not depend on the public internet being reachable either.
 *
 * Only the icons **actually referenced in code** are registered (74 now), not all 799:
 * `icons.json` is 444KB; putting the whole package in the bundle to use 9% of it is not worth it.
 * The list is checked against the source by scripts/check-icons.mjs; a missing one fails at that
 * step,
 * instead of being noticed only when an icon is missing on screen.
 *
 * ⚠️ **"Only register what is used" once only guarded runtime, not bundling.** This file used to
 * directly
 * `import { icons } from "@iconify-json/gravity-ui"`, which pulled the whole JSON in as **one
 * module**.
 * JSON cannot be tree-shaken by key, and the subset loop below only ran at runtime,
 * by which time all 799 icons were already sitting in the main chunk. Measured (rollup output
 * metadata): **451.5KB raw /
 * 103.7KB gzip**, 94% of which was never referenced.
 * Now it imports the subset `iconData.ts` generated at build time (74 icons, 11.6KB gzip);
 * runtime behavior is unchanged. When the list changes, re-run `node scripts/gen-icon-data.mjs`;
 * forgetting it is caught by check-icons.mjs.
 *
 * To add a new icon: just write `<Icon icon="gravity-ui:foo" />`, then
 * `node scripts/check-icons.mjs` tells you to add `foo` to the list below (or that the name is
 * misspelled).
 */

import { addCollection } from "@iconify/react";
import { GRAVITY_ICONS, GRAVITY_PREFIX } from "@/lib/iconData";

/**
 * Icon names referenced in the source. Checked by scripts/check-icons.mjs; do not reorder by hand.
 *
 * **It is still the single source of truth for this list**, only its consumer changed: it used to
 * be the runtime loop below,
 * now it is the build-time `scripts/gen-icon-data.mjs` (which picks iconData.ts by this list).
 * So no runtime code reads it; the `export` is only there so `noUnusedLocals` does not complain,
 * not for other modules to use. Delete it and both scripts break.
 */
export const USED = [
  "arrow-down",
  "arrow-left",
  "arrow-right-from-line",
  "arrow-rotate-left",
  "arrow-up",
  "arrow-up-right-from-square",
  "arrows-rotate-right",
  "bars",
  "box",
  "branches-right",
  "bug",
  "chart-column",
  "chart-line",
  "check",
  "chevron-down",
  "chevron-left",
  "chevron-right",
  "chevrons-collapse-up-right",
  "chevrons-expand-up-right",
  "circle-check",
  "circle-exclamation",
  "circle-info",
  "circle-pause",
  "circle-xmark",
  "clock",
  "cloud",
  "cloud-arrow-up-in",
  "code",
  "comment",
  "comments",
  "copy",
  "copy-check",
  "cpu",
  "display",
  "eye-slash",
  "face-robot",
  "file",
  "file-text",
  "flask",
  "folder",
  "funnel",
  "gear",
  "globe",
  "hourglass",
  "key",
  "layers",
  "link",
  "list-ul",
  "lock",
  "logo-github",
  "magnifier",
  "minus",
  "moon",
  "paperclip",
  "pencil",
  "pencil-to-line",
  "pencil-to-square",
  "person",
  "person-gear",
  "persons",
  "picture",
  "play",
  "plug-connection",
  "plus",
  "puzzle",
  "rocket",
  "shield-check",
  "smartphone",
  "sparkles",
  "stop",
  "sun",
  "terminal",
  "trash-bin",
  "triangle-exclamation",
  "wrench",
  "xmark",
] as const;

/**
 * Register.
 *
 * Two traps, neither of which errors, and both can only be checked by "are the icons still there
 * when offline":
 *
 * 1. **Icon data has one more level than you would expect.** The upstream `icons` export is "the
 *    whole icon set"
 *    (`{prefix, icons, lastModified}`); a single icon is in `.icons[name]`.
 *    What we import now is the build-time picked `GRAVITY_ICONS`, which **already is** that
 *    dictionary itself
 *    (`gen-icon-data.mjs` only exports the `.icons` level), so here it is passed down whole.
 *
 * 2. **Do not pass `width` / `height` through into addCollection.** The gravity-ui set has no
 *    set-wide
 *    size, so it reads as `undefined`; and `addCollection({..., width: undefined})` makes
 *    iconify **register no icons at all**, while still returning `true` (it treats it as "size was
 *    provided explicitly",
 *    then drops the whole batch in validation). Measured: with these two keys `listIcons()` is
 *    always 0; without them it works.
 *    So only prefix + icons are passed here, and iconify takes the default size from the icon data
 *    itself.
 *
 * The subset is picked **at build time** (see the file header), so there is no runtime filter
 * loop here anymore.
 * The USED list is still the source of truth; only its consumer changed from this function to
 * gen-icon-data.mjs.
 */
export function registerIcons(): void {
  addCollection({ prefix: GRAVITY_PREFIX, icons: GRAVITY_ICONS });
}

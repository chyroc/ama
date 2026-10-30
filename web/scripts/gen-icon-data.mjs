#!/usr/bin/env node
/**
 * Generates `src/lib/iconData.ts`: keeps **only the gravity-ui icons the source actually uses**.
 *
 * # Why this step exists (the "only register what is used" line in `icons.ts` is about runtime,
 * # not about bundling)
 *
 * `icons.ts` has always said "only register the icons the code really references, not all 799".
 * That sentence describes the **runtime** behavior of `addCollection`, and it is correct. But it
 * does not stop **bundling**:
 *
 *     import { icons as gravityCollection } from "@iconify-json/gravity-ui";
 *
 * This line pulls the whole `icons.json` in as **one module**, and JSON cannot be tree-shaken by
 * key. The subset loop only runs at runtime, and by then all 799 icons are already in the main
 * chunk. Measured (from rollup-plugin-visualizer output metadata, not by grepping minified text):
 * the full set takes **451.5KB raw / 103.7KB gzip** in the main chunk, while the 74 icons really
 * used are only **36.9KB raw / 10.8KB gzip**. **94% is dead weight.**
 *
 * So the "pick a subset" work moves to build time: this script freezes the subset into a .ts
 * module based on the USED list in `icons.ts`, and `icons.ts` imports that module instead.
 * Runtime behavior is exactly the same (addCollection still gets the same icons); the only
 * thing removed is the 725 icons that were never referenced.
 *
 * # The output is checked in
 *
 * Same as the precedent of `openapi.json` and `roleCapabilities.ts`: the generated file is
 * committed, and `check-icons.mjs` checks it before every `npm run build`. If the list changes
 * and you forget to re-run this, that step fails right away and prints the fix command, so you
 * do not find out only when an icon goes missing in the UI.
 *
 * Usage:
 *   node scripts/gen-icon-data.mjs      # run from web/
 */

import { readFileSync, writeFileSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const web = join(dirname(fileURLToPath(import.meta.url)), "..");

/** Reads the USED list from icons.ts; it is still the hand-maintained source of truth. */
export function readUsed(webDir = web) {
  const src = readFileSync(join(webDir, "src/lib/icons.ts"), "utf8");
  const block = src.match(/const USED = \[([\s\S]*?)\] as const;/);
  if (!block) throw new Error("icons.ts 里找不到 USED 名单，是不是改了它的写法？");
  return [...block[1].matchAll(/"([a-z0-9-]+)"/g)].map((m) => m[1]);
}

/**
 * Picks the subset from the full set by the list. Unknown names are collected as missing (that
 * means a typo); check-icons reports them with a friendlier message.
 */
export function buildSubset(used, webDir = web) {
  const collection = JSON.parse(
    readFileSync(join(webDir, "node_modules/@iconify-json/gravity-ui/icons.json"), "utf8"),
  );
  const icons = {};
  const missing = [];
  for (const name of used) {
    if (collection.icons[name]) icons[name] = collection.icons[name];
    else missing.push(name);
  }
  return { prefix: collection.prefix, icons, missing };
}

export function render(prefix, icons) {
  // Sort by name so the output is stable; otherwise every regeneration produces a shuffled diff.
  const sorted = Object.fromEntries(Object.keys(icons).sort().map((k) => [k, icons[k]]));
  return `/**
 * **Generated file, do not edit by hand.** Produced by \`node scripts/gen-icon-data.mjs\`: the
 * subset of \`@iconify-json/gravity-ui\` picked by the USED list in \`src/lib/icons.ts\`.
 *
 * Why not import the whole package: that would put all 799 icons (451KB raw / 104KB gzip) into
 * the main chunk, while we only use ${Object.keys(sorted).length} of them. JSON cannot be
 * tree-shaken by key, and the runtime subset loop does not stop bundling. See the header
 * comment of scripts/gen-icon-data.mjs for details.
 *
 * To change the icon list: edit USED in \`src/lib/icons.ts\`, then re-run the command above.
 * If you forget, \`check-icons.mjs\` blocks it before \`npm run build\`.
 */

import type { IconifyJSON } from "@iconify/types";

export const GRAVITY_PREFIX = ${JSON.stringify(prefix)};

export const GRAVITY_ICONS: IconifyJSON["icons"] = ${JSON.stringify(sorted, null, 2)};
`;
}

// Only write the file when run directly; when check-icons.mjs imports this, it only uses the
// functions above.
if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  const used = readUsed();
  const { prefix, icons, missing } = buildSubset(used);
  if (missing.length) {
    console.error(`✗ 这些名字在图标集里不存在（拼错了？）：${missing.join(", ")}`);
    process.exit(1);
  }
  const out = join(web, "src/lib/iconData.ts");
  writeFileSync(out, render(prefix, icons));
  console.log(`✓ 已生成 src/lib/iconData.ts：${Object.keys(icons).length} 个图标`);
}

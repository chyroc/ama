#!/usr/bin/env node
/**
 * Icon check: every gravity-ui icon referenced in the source must both exist in the local icon
 * set and be registered in the list in src/lib/icons.ts.
 *
 * Why this step exists: a missing registration and a misspelled name **both fail silently**.
 * When @iconify/react has no local data it quietly goes to api.iconify.design, and if that fails
 * it draws an empty <svg>. In the UI this shows up as "one icon is missing", and an icon-only
 * button becomes an invisible clickable area, which is hard to spot in review.
 * This has bitten us twice: the whole set was not registered (everything gone offline), and
 * `gravity-ui:robot` is a name that does not exist upstream at all (the local set only has
 * face-robot).
 *
 * Usage:
 *   node scripts/check-icons.mjs        # run from web/; a non-zero exit means a problem
 */

import { readFileSync, readdirSync, statSync, existsSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const web = join(dirname(fileURLToPath(import.meta.url)), "..");
const PREFIX = "gravity-ui";

/** Recursively collects the ts/tsx files under src. */
function walk(dir) {
  const out = [];
  for (const entry of readdirSync(dir)) {
    const p = join(dir, entry);
    if (statSync(p).isDirectory()) out.push(...walk(p));
    else if (/\.tsx?$/.test(entry)) out.push(p);
  }
  return out;
}

const files = walk(join(web, "src"));

// Scan the icon names in use. Note the list inside icons.ts itself does not count as "use",
// otherwise the check would always agree with itself.
const iconsModule = join(web, "src/lib/icons.ts");
const used = new Map(); // name -> where it appears
for (const f of files) {
  if (f === iconsModule) continue;
  const text = readFileSync(f, "utf8");
  for (const m of text.matchAll(new RegExp(`${PREFIX}:([a-z0-9-]+)`, "g"))) {
    if (!used.has(m[1])) used.set(m[1], []);
    const line = text.slice(0, m.index).split("\n").length;
    used.get(m[1]).push(`${f.replace(web + "/", "")}:${line}`);
  }
}

// What the local icon set actually contains.
const collection = JSON.parse(
  readFileSync(join(web, "node_modules/@iconify-json/gravity-ui/icons.json"), "utf8"),
);
const available = new Set([...Object.keys(collection.icons), ...Object.keys(collection.aliases ?? {})]);

// Which icons icons.ts registers.
//
// ⚠️ **Share the same parser with gen-icon-data.mjs**; do not write a separate regex here.
// This used to be `/^\s*"([a-z0-9-]+)",$/gm` (requires "alone on a line + trailing comma"),
// while the generator grabs every string literal in the USED block. Usually both give the same
// result, but as soon as someone writes two names on one line (`"wrench", "xmark",`), that regex
// **only sees one of them**, and reports "xmark is used but not registered", even though it is
// right there in the list. We hit this: the error does not point at the real cause at all (the
// real cause is how the parser is written, not the list). With one shared parser this split
// cannot happen.
const { readUsed, buildSubset, render } = await import("./gen-icon-data.mjs");
const registered = new Set(readUsed(web));

const missingUpstream = [];
const notRegistered = [];
for (const [name, where] of used) {
  if (!available.has(name)) missingUpstream.push([name, where]);
  else if (!registered.has(name)) notRegistered.push([name, where]);
}
// Names still in the list but no longer used by the code: only a warning, not an error (easy to
// forget when deleting code).
const stale = [...registered].filter((n) => !used.has(n));

let bad = false;
if (missingUpstream.length) {
  bad = true;
  console.error(`\n✗ 本地图标集里没有这些名字（拼错了，或该图标集确实没有）：`);
  for (const [n, where] of missingUpstream) {
    const hint = [...available].filter((a) => a.includes(n) || n.includes(a)).slice(0, 3);
    console.error(`  ${PREFIX}:${n}  ← ${where.join(", ")}${hint.length ? `\n      也许你想要：${hint.join(" / ")}` : ""}`);
  }
}
if (notRegistered.length) {
  bad = true;
  console.error(`\n✗ 用到了但没注册进 src/lib/icons.ts（会退化成走 CDN，离线即消失）：`);
  for (const [n, where] of notRegistered) console.error(`  ${PREFIX}:${n}  ← ${where.join(", ")}`);
  console.error(`\n  把它们按字母序补进 icons.ts 的 USED 数组即可。`);
}
if (stale.length) {
  console.warn(`\n! icons.ts 里注册了但源码已不再使用（可以删）：${stale.join(", ")}`);
}

/*
 * Generated-file check: `src/lib/iconData.ts` must match the current USED list exactly.
 *
 * Without this step, "changed the list but forgot to re-run the generator" fails with **zero
 * errors**: tsc passes, build passes, and the new icon is quietly blank in the UI (the same
 * failure mode described at the top of this file). So we fail here, the same way the two other
 * generated files, openapi.json and roleCapabilities.ts, are handled.
 *
 * ⚠️ **The list must be parsed with `readUsed()`, not with `registered` above.** Their regexes
 * differ (`registered` requires "one per line with a trailing comma", `readUsed` grabs every
 * string literal in the USED block). Usually they agree, but as soon as someone writes two names
 * on one line the two parses split, and the split shows up as **re-running the generator does
 * not fix it**: generation uses list A, the check uses list B, so it always reports a mismatch.
 * With one shared parser this endless loop cannot happen.
 */
let dataDrift = null;
try {
  const { icons, missing } = buildSubset(readUsed(web), web);
  if (!missing.length) {
    const expected = render(PREFIX, icons);
    const actual = existsSync(join(web, "src/lib/iconData.ts"))
      ? readFileSync(join(web, "src/lib/iconData.ts"), "utf8")
      : "";
    if (expected !== actual) dataDrift = Object.keys(icons).length;
  }
} catch (e) {
  dataDrift = -1;
  console.error(`\n✗ 校验 iconData.ts 时出错：${e.message}`);
}
if (dataDrift !== null) {
  bad = true;
  console.error(`\n✗ src/lib/iconData.ts 与 icons.ts 的 USED 名单不一致（或还没生成）。`);
  console.error(`  它是构建期挑出来的图标子集，不重新生成的话新图标在界面上会静静地空着。`);
  console.error(`\n  修复：node scripts/gen-icon-data.mjs\n`);
}

if (bad) {
  console.error(`\n共用到 ${used.size} 个图标，${missingUpstream.length + notRegistered.length} 个有问题。\n`);
  process.exit(1);
}
console.log(`✓ 图标对账通过：${used.size} 个引用全部存在且已注册（离线可用），iconData.ts 是最新的`);

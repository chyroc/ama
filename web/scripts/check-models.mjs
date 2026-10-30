#!/usr/bin/env node
/**
 * Model list check: `MODEL_SPECS` (web/src/types/domain.ts) must match, cell by cell, the
 * "parameters supported by each model" table in the Ark docs mirror.
 *
 * # Why a check script, and not a backend `GET /api/models` or "generate from the docs"
 *
 * **The backend does not know about models at all.** We traced the whole path: model.id is a
 * free-form string from the HTTP request body through domain to the SDK, `validateAgentDraft` in
 * `core/agent.go` only checks it is non-empty, and it is stored in PG as jsonb with no CHECK.
 * So a `GET /api/models` would just move this hand-written list from TS to Go: still one list to
 * maintain by hand, plus one more route to declare in authz/matrix.go, and the frontend would
 * lose the `ModelId` union type (the effort-level linking in AgentsPage relies on it to block
 * invalid combinations at compile time). Moving the list does not fix drift.
 *
 * **The docs are not the source of truth, so we cannot "generate from the docs".** The source of
 * truth is the Ark platform itself: `server/cmd/probe-models` calls `POST /api/v3/agents` for
 * each model and each level, and whatever gets rejected with 400 is unsupported. The docs
 * themselves say "the supported range may change as model versions are released", and the Ark
 * docs have been proven wrong by testing (the thinking page). Using a snapshot that goes stale as
 * the generator's input would lock the errors in.
 *
 * So this is what is left: **keep the list hand-written (to keep compile-time types), and use a
 * check script as a guard.** Here the docs mirror is **a tripwire, not an authority**: as soon as
 * it changes (the next fetch-ark-docs pulls a new table) the script goes red and forces someone
 * to run the probe to confirm, instead of drifting silently. check-icons.mjs works the same way.
 *
 * Usage:
 *   node scripts/check-models.mjs       # run from web/; a non-zero exit means a problem
 *
 * When the list really needs to change: first run `cd server && go run ./cmd/probe-models` to get
 * the platform facts, update MODEL_SPECS from that, then run this script again to confirm it
 * matches the docs. If the docs really lag behind the platform, add an entry to KNOWN_DOC_LAG
 * below and state the evidence.
 */

import { existsSync, readFileSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const web = join(dirname(fileURLToPath(import.meta.url)), "..");
const repo = join(web, "..");
const DOMAIN_TS = join(web, "src/types/domain.ts");
const DOC_MD = join(
  repo,
  "docs/ark-managed-agents/guide/02-定义-Agent/06-模型支持的参数列表.md",
);

// When the docs mirror is not in the repo (the open-source version does not ship the Ark docs
// mirror) there is nothing to check against, so skip.
if (!existsSync(DOC_MD)) {
  console.log("跳过模型清单对账：没有方舟文档镜像（docs/ark-managed-agents）");
  process.exit(0);
}

/**
 * Known differences where the docs lag behind the platform. Best kept empty.
 *
 * Entries may only be added when "the probe proved the docs wrong", and each one must state its
 * evidence; otherwise this becomes a rug to sweep drift under.
 */
const KNOWN_DOC_LAG = [
  {
    id: "deepseek-v4-1-flash-261017",
    why:
      "2026-10-21 探针实测：方舟可建 Agent，且真发一轮消息有正常回复" +
      "（`go run ./cmd/probe-models -infer deepseek-v4-1-flash-261017`）；" +
      "建 Agent 回显与逐档位试探一致：thinking 默认 enabled、" +
      "reasoning_effort=[none,low,high,max]（默认 high）、service_tier=[auto,default]，" +
      "塞伪造值会被拒（说明有元数据校验）。" +
      "而线上文档 2026-10-20 版仍未收录该模型，属文档落后于平台。",
  },
];

/** For ordered set comparison: the canonical level order, following the order of reasoning_effort
 * values in the Ark docs. */
const EFFORT_ORDER = ["none", "minimal", "low", "medium", "high", "xhigh", "max"];
const TIER_ORDER = ["auto", "default", "fast"];

const sortEfforts = (xs) =>
  [...xs].sort((a, b) => EFFORT_ORDER.indexOf(a) - EFFORT_ORDER.indexOf(b));
const sortTiers = (xs) =>
  [...xs].sort((a, b) => TIER_ORDER.indexOf(a) - TIER_ORDER.indexOf(b));

// ---------------------------------------------------------------------------
// Parse MODEL_SPECS
// ---------------------------------------------------------------------------

/**
 * Extracts MODEL_SPECS from domain.ts.
 *
 * Uses regexes instead of the TS compiler: this script runs before `npm run build` (like
 * check-icons), when build output cannot be relied on, and one table is not worth pulling in a
 * parser. The cost is that it is format-sensitive, so **it exits with an error when parsing
 * fails**, and never quietly returns an empty array, which would make a "pass" meaningless.
 */
function parseSpecs() {
  const src = readFileSync(DOMAIN_TS, "utf8");
  const start = src.indexOf("export const MODEL_SPECS");
  if (start < 0) fail(`在 ${rel(DOMAIN_TS)} 里找不到 export const MODEL_SPECS`);
  const body = src.slice(start, src.indexOf("\n];", start));
  if (!body) fail("MODEL_SPECS 的数组结尾没找到（是不是不再以 \\n]; 收尾了？）");

  const specs = [];
  // Split into chunks at `id: "..."`; each chunk runs until just before the next id.
  const idRe = /\bid:\s*"([^"]+)"/g;
  const marks = [...body.matchAll(idRe)];
  if (marks.length === 0) fail("MODEL_SPECS 里一个 id 都没解析出来");

  for (let i = 0; i < marks.length; i++) {
    const chunk = body.slice(
      marks[i].index,
      i + 1 < marks.length ? marks[i + 1].index : body.length,
    );
    const id = marks[i][1];
    const efforts = arrayField(chunk, "reasoningEfforts", id);
    const tiers = arrayField(chunk, "serviceTiers", id);
    const def = scalarField(chunk, "defaultReasoningEffort", id);
    const thinking = /supportsThinking:\s*true/.test(chunk);
    specs.push({ id, efforts, tiers, default: def, thinking });
  }
  return specs;
}

function arrayField(chunk, name, id) {
  const m = chunk.match(new RegExp(`${name}:\\s*\\[([^\\]]*)\\]`));
  if (!m) fail(`模型 ${id} 解析不出 ${name}`);
  return [...m[1].matchAll(/"([^"]+)"/g)].map((x) => x[1]);
}

function scalarField(chunk, name, id) {
  const m = chunk.match(new RegExp(`${name}:\\s*"([^"]+)"`));
  if (!m) fail(`模型 ${id} 解析不出 ${name}`);
  return m[1];
}

// ---------------------------------------------------------------------------
// Parse the table in the docs mirror
// ---------------------------------------------------------------------------

/**
 * The table looks like:
 *   |`doubao-seed-2-1-pro-260628` |enabled（默认）、disabled |low、medium、high（默认）、minimal |auto、default（默认） |
 * Takes the model ID, the reasoning_effort set and the one marked "默认" (default) in it, and the
 * service_tier set.
 */
function parseDoc() {
  const src = readFileSync(DOC_MD, "utf8");

  // Only look for the table inside the "supported models and parameters" section.
  //
  // We cannot scan the whole page: the earlier "model parameter fields" table has a `model.id`
  // row shaped exactly like a model row (wrapped in backticks, followed by three cells). Scanning
  // everything would treat it as a model named model.id, and the check would report a "missing
  // model": a failure that is pure noise.
  const anchor = src.indexOf('<span id="support-list">');
  if (anchor < 0) {
    fail(
      `在 ${rel(DOC_MD)} 里找不到锚点 <span id="support-list">——文档结构可能变了。\n` +
        `      别放宽匹配去迁就，先确认那一节还在不在。`,
    );
  }
  // Up to the next section (the one about fast support).
  const end = src.indexOf('<span id="fast-service-tier">', anchor);
  const section = src.slice(anchor, end > 0 ? end : src.length);

  const rows = [
    ...section.matchAll(/^\|`([a-z0-9.\-]+)`\s*\|([^|]*)\|([^|]*)\|([^|]*)\|/gim),
  ];
  if (rows.length === 0) {
    fail(
      `在 ${rel(DOC_MD)} 的「支持模型与参数列表」一节里没解析出任何模型行——表格形状可能变了。\n` +
        `      别改脚本去迁就，先看文档是不是真的换了结构。`,
    );
  }
  return rows.map(([, id, thinkingCell, effortCell, tierCell]) => {
    const cell = (s) =>
      s
        .split(/[、,]/)
        .map((x) => x.replace(/（默认）/g, "").trim())
        .filter(Boolean);
    const marked = (s) => {
      const hit = s.split(/[、,]/).find((x) => x.includes("（默认）"));
      return hit ? hit.replace(/（默认）/g, "").trim() : null;
    };
    return {
      id,
      efforts: cell(effortCell),
      defaultEffort: marked(effortCell),
      tiers: cell(tierCell),
      thinking: cell(thinkingCell).includes("enabled"),
    };
  });
}

// ---------------------------------------------------------------------------
// Check
// ---------------------------------------------------------------------------

const problems = [];
const note = (m) => problems.push(m);

const specs = parseSpecs();
const doc = parseDoc();

console.log(
  `对账：MODEL_SPECS ${specs.length} 个模型 ↔ 文档镜像 ${doc.length} 个模型`,
);

const specById = new Map(specs.map((s) => [s.id, s]));
const docById = new Map(doc.map((d) => [d.id, d]));
const lagged = new Set(KNOWN_DOC_LAG.map((x) => x.id));

// 1. In the docs but not in the list: a "missed sync", the main thing this script is for.
for (const d of doc) {
  if (!specById.has(d.id) && !lagged.has(d.id)) {
    note(
      `缺模型：文档里有 \`${d.id}\`，MODEL_SPECS 里没有。\n` +
        `        跑 \`cd server && go run ./cmd/probe-models\` 确认平台档位后补进去。`,
    );
  }
}

// 2. In the list but not in the docs: maybe the docs lag (fine; once the probe proves it, record it
//    in KNOWN_DOC_LAG), or maybe the model ID is mistyped (that fails silently: the frontend can
//    select it, and Ark only returns 400 when creating the Agent).
for (const s of specs) {
  if (!docById.has(s.id) && !lagged.has(s.id)) {
    note(
      `多模型：MODEL_SPECS 里有 \`${s.id}\`，文档里没有。\n` +
        `        要么是模型 ID 拼错了（前端能选但方舟会 400），要么文档落后于平台——\n` +
        `        用探针确认；若确属文档落后，记进 check-models.mjs 的 KNOWN_DOC_LAG。`,
    );
  }
}

// 3. In both: compare cell by cell.
for (const s of specs) {
  const d = docById.get(s.id);
  if (!d || lagged.has(s.id)) continue;

  const se = sortEfforts(s.efforts).join(",");
  const de = sortEfforts(d.efforts).join(",");
  if (se !== de) {
    note(`\`${s.id}\` 的 reasoningEfforts 不一致：\n        清单 [${se}]\n        文档 [${de}]`);
  }

  const st = sortTiers(s.tiers).join(",");
  const dt = sortTiers(d.tiers).join(",");
  if (st !== dt) {
    note(`\`${s.id}\` 的 serviceTiers 不一致：\n        清单 [${st}]\n        文档 [${dt}]`);
  }

  if (d.defaultEffort && s.default !== d.defaultEffort) {
    note(
      `\`${s.id}\` 的 defaultReasoningEffort 不一致：清单 ${s.default}，文档 ${d.defaultEffort}`,
    );
  }

  if (s.thinking !== d.thinking) {
    note(`\`${s.id}\` 的 supportsThinking 不一致：清单 ${s.thinking}，文档 ${d.thinking}`);
  }
}

// 4. Self-consistency: the default level must be in the supported set, otherwise the selector
//    starts out with an invalid combination.
for (const s of specs) {
  if (!s.efforts.includes(s.default)) {
    note(
      `\`${s.id}\` 的 defaultReasoningEffort=${s.default} 不在 reasoningEfforts [${s.efforts.join(",")}] 里——\n` +
        `        选择器默认值即非法组合，方舟会 400。`,
    );
  }
  if (!s.tiers.includes("default")) {
    note(`\`${s.id}\` 的 serviceTiers 里没有 "default"，方舟所有模型都支持它，八成是漏了。`);
  }
}

// 5. Model IDs in the seed data must be in the list.
//
// Why this is worth checking: the seed **really goes into the database** (an empty database gets
// the seed, see core/store_pg.go), and the backend only checks model.id is non-empty. A mistyped
// ID can be stored all the way into PG, and only when a user starts a chat with that agent does
// Ark return 400, with an error that does not point at the seed. The frontend seed.ts is
// protected at compile time by the TS union type, but core/seed.json is plain JSON and **nothing
// stops it**, so this is the only guard.
for (const [file, ids] of seedModelIds()) {
  for (const id of ids) {
    if (!specById.has(id)) {
      note(
        `种子数据 ${file} 用了不在 MODEL_SPECS 里的模型 \`${id}\`。\n` +
          `        种子会真的入库，而后端只校验非空——拼错要等用户开对话才 400。`,
      );
    }
  }
}

/** Collects the model IDs used in the two seed files. */
function seedModelIds() {
  const out = [];
  for (const f of ["../server/core/seed.json", "src/data/seed.ts"]) {
    const p = join(web, f);
    let src;
    try {
      src = readFileSync(p, "utf8");
    } catch {
      // Skip if the seed file is missing: that is not this script's job, and something else
      // will report it.
      continue;
    }
    const ids = new Set(
      [...src.matchAll(/"((?:doubao|deepseek|glm)[a-z0-9.\-]*)"/gi)].map((m) => m[1]),
    );
    out.push([rel(p), [...ids]]);
  }
  return out;
}

// ---------------------------------------------------------------------------

if (problems.length > 0) {
  console.error(`\n✗ 模型清单对账失败，${problems.length} 处：\n`);
  for (const p of problems) console.error(`  · ${p}`);
  console.error(
    `\n真源是方舟平台，不是文档：\`cd server && go run ./cmd/probe-models\`。\n` +
      `背景见 server/shared/infra/ma/README.md 的「模型清单」。\n`,
  );
  process.exit(1);
}

console.log(`✓ ${specs.length} 个模型逐格一致（档位 / 默认值 / service_tier / thinking）`);
if (KNOWN_DOC_LAG.length > 0) {
  console.log(`  （${KNOWN_DOC_LAG.length} 个已知文档落后项被跳过）`);
}

function fail(msg) {
  console.error(`✗ 模型清单对账无法进行：${msg}`);
  process.exit(1);
}

function rel(p) {
  return p.startsWith(repo) ? p.slice(repo.length + 1) : p;
}

/**
 * Frontend copy of the platform's built-in skills (**only used by LocalApiClient**).
 *
 * # Why there is a copy here
 *
 * The source of truth is on the backend, `server/service/skill/skillbuiltin.go`. Over HTTP,
 * built-in skills come back in the `listSkills` response, so the frontend does not need to know
 * what they look like at all.
 *
 * But `LocalApiClient` **sends no requests**: it is the data source for local mode and e2e (see
 * the top of data/api.ts). There is no backend to ask there, so it has to keep its own copy;
 * otherwise built-in skills would not show in the skill library in local mode, and that is
 * exactly the path e2e covers.
 *
 * # ⚠️ This copy drifts from the backend, and drift gives no error
 *
 * Each side has its own copy; changing one and forgetting the other gives **no compile error or
 * test failure**. The symptom is "local mode behaves differently from production", and that kind
 * of mismatch is usually only found when someone uses e2e to reproduce a production issue, and
 * at that point people first suspect a code bug.
 *
 * So this only keeps **the fields the frontend really renders** (name, title, description,
 * source, tags, time). **The instructions body is deliberately empty**: copying it in would only
 * add one more duplicate that is sure to go stale. The real body is in the backend's
 * core/skillbuiltin.go.
 *
 * ⚠️ **The earlier reason given here, "the frontend never shows the body", no longer holds**: the
 * skill detail page (SkillDetailPage) exists to show SKILL.md. So the cost of leaving it empty
 * went from "no cost" to **the body area of built-in skills is empty in local mode** (in HTTP
 * mode it has real content).
 *
 * That blank is **handled by the detail page itself**: for empty instructions it shows a line
 * saying "the body is maintained by the platform", instead of a blank that looks broken. We chose
 * this over copying the body because those two bodies are dozens of lines and change with the
 * backend; a copy would surely drift, and the symptom of drift is exactly "the two modes show
 * different skill bodies", much harder to track down than a blank.
 *
 * The ids match the backend's BuiltinSkillIDLarkCLI / BuiltinSkillIDAMA exactly: they are stored
 * in `agent.skillIds`, and a mismatch would make selections not carry over between the two modes.
 */

import type { TFunc } from "@/prefs/PrefsContext";
import type { MessageKey } from "@/prefs/messages";
import type { Skill } from "@/types/domain";

/** Matches the backend's core.builtinSkillTime; a fixed literal (not now, or it would change on every refresh). */
const BUILTIN_TIME = "2026-08-29T00:00:00.000Z";

export const BUILTIN_SKILLS: Skill[] = [
  {
    id: "skill-builtin-lark-cli",
    workspaceId: "",
    name: "lark-cli",
    displayTitle: "lark-cli（飞书 CLI）",
    description:
      "操作飞书：消息、云文档、多维表格、日历、审批、任务、邮箱、会议等。需要读写飞书里的数据时用它，优先于直接调 OpenAPI。",
    instructions: "",
    source: "builtin",
    latestVersion: "1",
    tags: ["内置", "飞书"],
    createdAt: BUILTIN_TIME,
    updatedAt: BUILTIN_TIME,
  },
  {
    id: "skill-builtin-ama",
    workspaceId: "",
    name: "ama",
    displayTitle: "ama（AMA 平台自身的 CLI）",
    description:
      "操作 AMA 平台自己的数据：空间、Agent、技能、运行历史、问题（issue）。用户让你「提个问题」「看看有哪些 agent」「查运行历史」这类请求时用它，它以当前用户的身份操作，权限与那个人登录时完全一样。",
    // The body is maintained by the backend; see the file header.
    instructions: "",
    source: "builtin",
    latestVersion: "1",
    tags: ["内置", "AMA"],
    createdAt: BUILTIN_TIME,
    updatedAt: BUILTIN_TIME,
  },
];

/**
 * Title / description / tags of a skill as they should be shown in the current language.
 *
 * Built-in skills come from the backend with Chinese text (it is also what the agent reads), so the
 * frontend swaps in translated text by skill name at render time. Other skills are shown as is: their
 * text was written by users.
 */
const BUILTIN_TEXT_KEYS: Record<string, { title: MessageKey; description: MessageKey }> = {
  "lark-cli": { title: "skill.builtin.larkCli.title", description: "skill.builtin.larkCli.description" },
  ama: { title: "skill.builtin.ama.title", description: "skill.builtin.ama.description" },
};

const BUILTIN_TAG_KEYS: Record<string, MessageKey> = {
  内置: "skill.builtin.tagBuiltin",
  飞书: "skill.builtin.tagFeishu",
};

export function skillText(
  s: Pick<Skill, "name" | "source" | "displayTitle" | "description" | "tags">,
  t: TFunc,
): { title: string; description: string; tags: string[] } {
  const keys = s.source === "builtin" ? BUILTIN_TEXT_KEYS[s.name] : undefined;
  if (!keys) return { title: s.displayTitle, description: s.description, tags: s.tags };
  return {
    title: t(keys.title),
    description: t(keys.description),
    tags: s.tags.map((tag) => (BUILTIN_TAG_KEYS[tag] ? t(BUILTIN_TAG_KEYS[tag]) : tag)),
  };
}

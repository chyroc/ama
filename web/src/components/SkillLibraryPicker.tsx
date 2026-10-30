/**
 * "Choose from the skill library": the dialog for picking workspace skills when creating an Agent.
 *
 * # Why a dialog instead of inlining the skill library into the create form
 *
 * The checkbox area of `AgentsPage` had a clear decision: **list only built-in skills, not the workspace's own skills**,
 * because "the create form already has four blocks, name/description/prompt/model; adding a whole skill library would bury the main flow".
 * That decision **still holds**, and this component does not overturn it; it only fixes one side effect of it:
 * at the create step users **could not see at all that workspace skills can be bound**, so they thought the platform had only three built-in skills
 * (the detail page could already bind all of them; the feature was there, the entry was missing).
 *
 * So this offers a third way: the form gets only one more row (a button + a line "N/20 selected"),
 * and the whole skill library is kept in a dialog. **The form length barely changes**, which is exactly what the original comment worried about.
 *
 * # The shape must match the detail page's SkillsTab exactly
 *
 * The same `bind-skill-<name>` / `bind-skill-<badge.testid>-<name>` testids, the same
 * `skillSourceBadge` helper, the same `skill.bindLabel` text. Same reason as
 * `NewAgentCapabilities` reusing `tool-<id>`: **"what you check when creating" and "what you change later"
 * must be the same mental model**; once the two places split, users think they are two different things, and e2e has to write two sets of selectors.
 *
 * # Read-only skills can be bound; do not filter them out
 *
 * `isReadOnlySkill()` means **the body cannot be edited**; it has nothing to do with "can it be attached to an agent".
 *
 * # Built-in skills are not here
 *
 * They have their own block in the main form (because they are **checked by default**, and a default the user cannot see is the same as secretly
 * deciding for them). Listing them in both places would give the same skill two checkboxes and duplicate testids,
 * and Playwright's strict mode errors right away. **The check uses `isBuiltinSkill`, not an id prefix**,
 * the same check used there to filter built-in skills, so nothing is missed or duplicated.
 */

import { skillText } from "@/data/builtinSkills";
import { useEffect, useMemo, useState } from "react";
import { Button, Checkbox, Chip, Input, SearchField } from "@heroui/react";
import { Link } from "react-router-dom";
import { Empty } from "@/components/Page";
import { FormModal } from "@/components/FormModal";
import { useWorkspacePath } from "@/app/WorkspaceGate";
import { usePrefs } from "@/prefs/PrefsContext";
import { truncate } from "@/lib/format";
import {
  MAX_SKILLS_PER_AGENT,
  isBuiltinSkill,
  skillSourceBadge,
  type Skill,
} from "@/types/domain";

/**
 * How many items before the search box appears.
 *
 * With a dozen skills in a workspace, a search box is pure noise (one screen shows them all); with dozens and no search
 * you can only scroll. The threshold 8 is a rule of thumb for "roughly fits on one screen", **no need to be exact**:
 * both behaviors are correct; it is only about which is less effort.
 */
const SEARCH_THRESHOLD = 8;

export function SkillLibraryPicker({
  isOpen,
  onOpenChange,
  skills,
  selectedIds,
  onConfirm,
}: {
  isOpen: boolean;
  onOpenChange: (open: boolean) => void;
  /** All skills of the workspace (including built-in ones, which this component filters out itself). */
  skills: Skill[];
  /** Ids already bound in the current draft, **including built-in skills**: the limit must count them. */
  selectedIds: string[];
  /** On confirm, hand over the **full** new id list (including built-in skills not shown here). */
  onConfirm: (ids: string[]) => void;
}) {
  const { t } = usePrefs();
  const wp = useWorkspacePath();
  const [keyword, setKeyword] = useState("");

  /**
   * Temporary selection inside the dialog.
   *
   * **It changes a copy, not draft**: clicking "Cancel" must really cancel this round of checks;
   * changing draft directly would apply the changes as soon as the dialog closes, and that "Cancel" would be a lie.
   */
  const [staged, setStaged] = useState<string[]>(selectedIds);

  // Re-sync a copy from the current draft on every open. Without it, the second open would show the temporary values from
  // the last close, while the user may have changed things outside (unchecked a built-in skill).
  useEffect(() => {
    if (!isOpen) return;
    setStaged(selectedIds);
    setKeyword("");
    // selectedIds is a new array on every render; putting it in deps would rerun this effect every frame
    // and wipe out the user's checks in the dialog right away. Only the moment of "open" counts.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [isOpen]);

  /** Options: workspace skills; built-in ones are not here (see the file header). */
  const pickable = useMemo(() => skills.filter((s) => !isBuiltinSkill(s)), [skills]);

  const visible = useMemo(() => {
    const kw = keyword.trim().toLowerCase();
    if (!kw) return pickable;
    return pickable.filter(
      (s) =>
        s.name.toLowerCase().includes(kw) ||
        s.displayTitle.toLowerCase().includes(kw) ||
        s.description.toLowerCase().includes(kw),
    );
  }, [pickable, keyword]);

  /*
   * The limit check uses **the total after the temporary changes**, i.e. counting built-in skills not shown in this dialog too.
   * Counting only how many are checked in the dialog would miss those 3 built-in ones, so the user picks 20 here,
   * and on save the backend rejects 23; and that rejection happens at the moment a chat starts, not pointing to this dialog.
   */
  const total = staged.length;
  const atLimit = total >= MAX_SKILLS_PER_AGENT;

  function toggle(id: string) {
    setStaged((cur) => {
      if (cur.includes(id)) return cur.filter((x) => x !== id);
      // At the limit, no more checks. **Silently ignoring does not work** (a click with no response looks broken),
      // so the count below uses the danger color and the checkboxes are disabled: disabled is more explainable than unresponsive.
      if (cur.length >= MAX_SKILLS_PER_AGENT) return cur;
      return [...cur, id];
    });
  }

  return (
    <FormModal
      isOpen={isOpen}
      onOpenChange={onOpenChange}
      title={t("skill.pickerTitle")}
      description={t("skill.pickerDesc")}
      submitLabel={t("skill.pickerConfirm")}
      size="lg"
      testId="skill-picker"
      onSubmit={() => {
        onConfirm(staged);
        return Promise.resolve(true);
      }}
    >
      {/*
        Empty state: this workspace has no skills of its own at all.
        **Do not give a drawer that opens empty**: say directly "where to get one": on the skill library page
        you can create or upload one.
      */}
      {pickable.length === 0 ? (
        <div data-testid="skill-picker-empty">
          <Empty
            icon="gravity-ui:puzzle"
            title={t("skill.pickerEmptyTitle")}
            description={t("skill.pickerEmptyDesc")}
            action={
              <Link to={wp("/skills")} className="no-underline">
                <Button variant="outline">{t("skill.goToLibrary")}</Button>
              </Link>
            }
          />
        </div>
      ) : (
        <>
          {pickable.length > SEARCH_THRESHOLD ? (
            <SearchField value={keyword} onChange={setKeyword} aria-label={t("skill.pickerSearch")}>
              <Input placeholder={t("skill.pickerSearch")} data-testid="skill-picker-search" />
            </SearchField>
          ) : null}

          {/*
            The count is the main feedback of this dialog, so it is **above** the list (if it only showed at the bottom,
            the user would only learn about the limit when the 21st is rejected). It turns danger at the limit.
          */}
          <p
            className={`num text-xs ${atLimit ? "text-danger" : "text-muted"}`}
            data-testid="skill-picker-count"
          >
            {t("skill.pickerCount", { count: total, max: MAX_SKILLS_PER_AGENT })}
            {atLimit ? ` ${t("skill.pickerAtLimit")}` : ""}
          </p>

          {visible.length === 0 ? (
            // "No search match" and "no skills at all" are two different things and cannot share the empty state above:
            // that one guides the user to install skills, while here they just mistyped the keyword.
            <p className="text-sm text-muted" data-testid="skill-picker-no-match">
              {t("skill.pickerNoMatch")}
            </p>
          ) : (
            <div className="flex max-h-96 flex-col gap-2 overflow-y-auto">
              {visible.map((s) => {
                const bound = staged.includes(s.id);
                const badge = skillSourceBadge(s.source);
                return (
                  <div
                    key={s.id}
                    className={[
                      "flex items-start gap-3 r-inset px-3 py-3",
                      bound ? "bg-surface-secondary" : "",
                    ].join(" ")}
                  >
                    <Checkbox
                      isSelected={bound}
                      // At the limit you can only uncheck, not add. Keep **checked items** clickable,
                      // otherwise at the limit the user cannot even swap one and must start over.
                      isDisabled={atLimit && !bound}
                      onChange={() => toggle(s.id)}
                      aria-label={t("skill.bindLabel", { name: skillText(s, t).title })}
                      data-testid={`bind-skill-${s.name}`}
                      className="mt-0.5"
                    >
                      <Checkbox.Content>
                        <Checkbox.Control>
                          <Checkbox.Indicator />
                        </Checkbox.Control>
                      </Checkbox.Content>
                    </Checkbox>
                    <div className="flex min-w-0 flex-1 flex-col gap-1">
                      <div className="flex items-center gap-2">
                        <span className="text-sm font-medium">{skillText(s, t).title}</span>
                        <span className="font-mono text-xs text-muted">{s.name}</span>
                        {/*
                          The source badge uses the skillSourceBadge helper, matching the detail page's binding
                          list exactly (including the testid shape). To add a source, change that one function.
                        */}
                        {badge ? (
                          <Chip
                            size="sm"
                            variant="soft"
                            data-testid={`bind-skill-${badge.testid}-${s.name}`}
                          >
                            <Chip.Label>{t(badge.labelKey)}</Chip.Label>
                          </Chip>
                        ) : null}
                      </div>
                      <p className="text-xs text-muted">{truncate(skillText(s, t).description, 120)}</p>
                    </div>
                  </div>
                );
              })}
            </div>
          )}
        </>
      )}
    </FormModal>
  );
}

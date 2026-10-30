/**
 * Agent list and creation.
 *
 * "Each agent binds a different prompt and model" is the core requirement, so prompt and model are
 * the stars of the create form;
 * skill/Feishu bindings live on the detail page (binding needs an agent first, and is often
 * adjusted after creation).
 */

import { skillText } from "@/data/builtinSkills";
import { useCallback, useEffect, useRef, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import {
  Button,
  Card,
  Checkbox,
  Chip,
  Description,
  FieldError,
  Input,
  Label,
  ListBox,
  Select,
  ToggleButton,
  TextArea,
  TextField,
} from "@heroui/react";
import { Icon } from "@iconify/react";
import { api } from "@/data/api";
import { useWorkspace, useWorkspacePath } from "@/app/WorkspaceGate";
import { AsyncErrorBlock, Empty, ListSkeleton, PageBody, PageHeader, SectionTitle } from "@/components/Page";
import { ModelIcon } from "@/components/ModelIcon";
import { formatRelative, truncate } from "@/lib/format";
import { useAsyncData } from "@/lib/useAsyncData";
import {
  BUILTIN_TOOLS,
  MODEL_SPECS,
  isBuiltinSkill,
  modelSpec,
  type AgentDraft,
  type AgentStatus,
  type BuiltinTool,
  type ModelId,
  type ReasoningEffort,
  type ServiceTier,
  type IntegrationState,
  type Skill,
} from "@/types/domain";
import { usePrefs } from "@/prefs/PrefsContext";
import {
  AgentEnvAliasSection,
  AgentEnvSection,
  AgentExecutorSection,
  AgentIntegrationsSection,
} from "@/components/AgentRuntimeSections";
import { useCanManage } from "@/lib/useCanManage";
import { SkillLibraryPicker } from "@/components/SkillLibraryPicker";
import { UserAvatar } from "@/components/UserAvatar";
import { useUserProfiles } from "@/lib/useUserProfiles";
import { useAuth } from "@/app/AuthGate";

const STATUS_COLOR: Record<AgentStatus, "default" | "success" | "warning"> = {
  active: "success",
  disabled: "default",
};

/**
 * Initial draft for a new agent.
 *
 * # All built-in tools checked by default, **only affecting new agents**
 *
 * This function has only one caller, the `useState` initial value of `NewAgentPage`; existing
 * agents go through
 * `toAgentDraft(agent)` (types/domain.ts), which takes the config from the database. So "defaults"
 * are safe here: they cannot produce the overwrite "open an old agent and get default tools stuffed
 * in".
 *
 * ⚠️ **This boundary must be pinned by tests**: the day someone uses it as a general "fill in the
 * draft" function, existing
 * agents' unchecked tools would be filled with defaults, and stored on the next save; tools the user
 * never checked
 * would just grow out of nowhere, with no error. The e2e "editing an existing agent does not get
 * default checks stuffed in" guards against this.
 *
 * We tested one way of writing it that does **not** break: `{...emptyAgentDraft(), ...toAgentDraft(a)}`.
 * The later spread always overrides the earlier one, so behavior did not change by a single byte
 * (using it as a mutation, the tests stayed all green;
 * that is an ineffective mutation, not a test gap). What really breaks is the **union** kind of
 * code,
 * e.g. `[...new Set([...emptyAgentDraft().builtin_tools, ...d.builtin_tools])]`;
 * only that quietly adds the unchecked tools back, and that mutation really did turn the tests red.
 *
 * # Why defaults are in the frontend and not in the backend's CreateAgent
 *
 * Because "initial checks of the create form" is a UI concept, and defaults on the backend would
 * fall into the same kind of trap as `CredentialMode`
 * (see core/agent.go:82): `builtin_tools: []` is a **valid value**,
 * meaning "this agent wants no tools at all". The backend cannot tell "the user cleared it on
 * purpose" from "the caller did not pass it":
 * in JSON both are empty arrays (on the Go side nil and [] are both normalized by nonNil), so
 * building a
 * chat-only agent would become impossible. Doing it on the backend would first need the field to
 * become a pointer to tell null from [],
 * which changes the contract, at a cost far above the benefit.
 */
export function emptyAgentDraft(): AgentDraft {
  return {
    name: "",
    description: "",
    system: "",
    model: {
      id: "doubao-seed-2-1-pro-260628",
      thinking: "enabled",
      reasoning_effort: "high",
      service_tier: "default",
    },
    // All built-in tools are available by default: nearly every agent needs to read/write files and
    // run commands,
    // and making users check them one by one from zero ends with **forgetting to check**; a missed
    // check shows up as "the model says it has no tools",
    // which does not point at this form. Those not needed can be unchecked on the spot.
    builtin_tools: BUILTIN_TOOLS.map((tool) => tool.id),
    // Built-in skills (lark-cli / ama) are also checked by default, but **their ids are not written
    // here**:
    // we only know which exist after the skill list comes back; see NewAgentPage's useEffect.
    skillIds: [],
    feishuAppIds: [],
    // New means enabled. **It used to default to draft**, a state that blocks nothing and only makes
    // users click once more;
    // someone who fills the form and clicks save clearly does not intend "create one that cannot be
    // used".
    status: "active",
    // Sending another message defaults to **redirect** (interrupt the current turn and do the new
    // one): the default the product asked for.
    // Written explicitly rather than left as an empty string: a new agent should have a definite
    // value in the database;
    // an empty string behaves the same, but it says "never decided".
    followUp: "redirect",
    // In Feishu, detailed work content (chain of thought) is **shown** by default. Same as above:
    // write the default explicitly rather than leave an empty string.
    progressDetail: "full",
    // ⚠️ **No authorization is checked by default** (the requirement says so), so this is an empty
    // array.
    //
    // And because the default is empty, **this part does not need the defaultsApplied ref gate**:
    // that mechanism is for "apply default checks once" (because "the user cancelled all" and
    // "defaults not applied yet" both look like empty arrays, and a ref is needed to tell them
    // apart). There is no default to apply here.
    integrationIds: [],
    envs: [],
    envAliases: [],
    // Default Docker container (executorType empty string), no host attached. **Empty string, not
    // undefined**:
    // isDirty is a bare JSON.stringify comparison, and the two differing would keep the save button
    // lit forever.
    executorType: "",
    hostId: "",
    hostSource: "",
  };
}

/**
 * Model and inference parameters.
 *
 * Each model supports different reasoning_effort levels and service_tier (see "parameters supported
 * by models"
 * in the Ark docs); when switching models, invalid options must be narrowed away, otherwise Ark
 * rejects the submit with a 400.
 */
export function ModelPicker({
  draft,
  setDraft,
}: {
  draft: AgentDraft;
  setDraft: (d: AgentDraft) => void;
}) {
  const { t } = usePrefs();
  const spec = modelSpec(draft.model.id);

  function changeModel(id: ModelId) {
    const next = modelSpec(id);
    setDraft({
      ...draft,
      model: {
        id,
        thinking: next.supportsThinking ? (draft.model.thinking ?? "enabled") : undefined,
        // After switching models, if the old level is not supported, fall back to the new model's
        // default
        reasoning_effort: next.reasoningEfforts.includes(
          draft.model.reasoning_effort as ReasoningEffort,
        )
          ? draft.model.reasoning_effort
          : next.defaultReasoningEffort,
        service_tier: next.serviceTiers.includes(draft.model.service_tier as ServiceTier)
          ? draft.model.service_tier
          : "default",
      },
    });
  }

  return (
    <div className="grid gap-5 sm:grid-cols-2">
      <Select
        className="sm:col-span-2"
        selectedKey={draft.model.id}
        onSelectionChange={(k) => changeModel(String(k) as ModelId)}
      >
        <Label>{t("agent.model")}</Label>
        <Select.Trigger data-testid="agent-model">
          {/*
            **Do not draw another ModelIcon in the collapsed state.**
            `Select.Value` renders the whole content of the selected ListBox.Item; the icon is already
            in there,
            and adding one gives two stacked icons (found by testing; see the crop screenshot).
            HeroUI's Select is not the kind where "Value only gives text"; do not guess from the
            literal meaning of Trigger.
          */}
          <Select.Value />
          <Select.Indicator />
        </Select.Trigger>
        <Select.Popover>
          <ListBox>
            {MODEL_SPECS.map((m) => (
              <ListBox.Item key={m.id} id={m.id} textValue={m.label}>
                {/* items-center rather than the default stretch: the icon must center against the two lines of text as a whole */}
                <span className="flex items-center gap-2">
                  <ModelIcon vendor={m.vendor} />
                  <span className="flex flex-col">
                    <span className="text-sm">{m.label}</span>
                    <span className="font-mono text-xs text-muted">{m.id}</span>
                  </span>
                </span>
              </ListBox.Item>
            ))}
          </ListBox>
        </Select.Popover>
      </Select>

      <Select
        selectedKey={draft.model.reasoning_effort ?? spec.defaultReasoningEffort}
        onSelectionChange={(k) =>
          setDraft({
            ...draft,
            model: { ...draft.model, reasoning_effort: String(k) as ReasoningEffort },
          })
        }
      >
        <Label>{t("agent.reasoning")}</Label>
        <Select.Trigger data-testid="agent-effort">
          <Select.Value />
          <Select.Indicator />
        </Select.Trigger>
        <Select.Popover>
          <ListBox>
            {spec.reasoningEfforts.map((r) => (
              <ListBox.Item key={r} id={r} textValue={t(`enum.effort.${r}`)}>
                {t(`enum.effort.${r}`)}
              </ListBox.Item>
            ))}
          </ListBox>
        </Select.Popover>
      </Select>

      <Select
        selectedKey={draft.model.service_tier ?? "default"}
        onSelectionChange={(k) =>
          setDraft({
            ...draft,
            model: { ...draft.model, service_tier: String(k) as ServiceTier },
          })
        }
      >
        <Label>{t("agent.serviceTier")}</Label>
        <Select.Trigger data-testid="agent-tier">
          <Select.Value />
          <Select.Indicator />
        </Select.Trigger>
        <Select.Popover>
          <ListBox>
            {spec.serviceTiers.map((tier) => (
              <ListBox.Item key={tier} id={tier} textValue={tier}>
                {tier}
              </ListBox.Item>
            ))}
          </ListBox>
        </Select.Popover>
      </Select>

      {!spec.serviceTiers.includes("fast") ? (
        <p className="sm:col-span-2 text-xs text-muted">
          {t("agent.noFastTier")}
        </p>
      ) : null}
    </div>
  );
}

export function AgentsPage() {
  const { t, language } = usePrefs();
  const { workspace } = useWorkspace();
  const wp = useWorkspacePath();
  const { user } = useAuth();

  // Failures must be visible, not stuck on the skeleton (see the file header of lib/useAsyncData.ts).
  const fetch = useCallback(() => api.listAgents(workspace.id), [workspace.id]);
  const {
    data: agents,
    error,
    isLoading,
    reload,
  } = useAsyncData(fetch, t("common.loadFailedUnknown"));

  /*
   * "Created by me" **is filtered in the frontend**.
   *
   * The reasoning is that `listAgents` returns **all** agents of this workspace at once, with no
   * paging; so filtering in the frontend
   * saves an endpoint and a round trip, and switching the filter is instant (no skeleton flash).
   *
   * ⚠️ **This conclusion is tied to "no paging".** The day listAgents gets paging, this must
   * move to backend filtering: frontend filtering can only see the current page, so "created by me"
   * would miss mine on later pages,
   * and that **gives no error at all**: the user sees a correct-looking list missing a few rows.
   * When that day comes, the rule is "filter conditions must be evaluated on the same side as
   * paging".
   */
  const [mineOnly, setMineOnly] = useState(false);
  const mine = (user?.email ?? "").trim().toLowerCase();
  const shown = (agents ?? []).filter(
    // Normalize both sides before comparing: createdBy is stored by the backend as is, while the
    // users table keys are lowercase.
    // Without normalizing, a case mismatch would filter out your own agents with zero errors.
    (a) => !mineOnly || (a.createdBy ?? "").trim().toLowerCase() === mine,
  );

  /*
   * Creators needed by the whole page, **looked up at once**.
   *
   * Pass agents rather than shown: filtering is pure frontend, and switching the filter should not
   * send a new request
   * (and when the filter is cleared those profiles are needed right away).
   */
  const profiles = useUserProfiles((agents ?? []).map((a) => a.createdBy));

  return (
    <PageBody>
      <PageHeader
        title="Agents"
        description={t("agent.listDesc")}
        actions={
          <Link to={wp("/agents/new")} className="no-underline">
            <Button data-testid="new-agent">{t("agent.new")}</Button>
          </Link>
        }
      />

      {isLoading ? (
        <ListSkeleton />
      ) : error ? (
        <AsyncErrorBlock error={error} onRetry={reload} />
      ) : !agents ? null : agents.length === 0 ? (
        <Empty
          icon="gravity-ui:face-robot"
          title={t("agent.emptyTitle")}
          description={t("agent.emptyDesc")}
          action={
            <Link to={wp("/agents/new")} className="no-underline">
              <Button>{t("agent.new")}</Button>
            </Link>
          }
        />
      ) : (
        <>
          {/*
            The filter bar is only drawn **when there is something to filter**: when the whole
            workspace has two or three agents,
            a filter switch takes more space than what it filters out. The threshold 2 is the lower
            bound of "can show a difference at all".

            ⚠️ It comes **after** the `agents.length === 0` empty state: an empty workspace should
            not show a filter
            (that would make people think things were filtered out, and click it).
          */}
          {agents.length > 1 ? (
            <div className="flex flex-wrap items-center gap-2">
              <ToggleButton
                size="sm"
                isSelected={mineOnly}
                onChange={setMineOnly}
                data-testid="filter-mine-only"
              >
                {t("agent.mineOnly")}
              </ToggleButton>
            </div>
          ) : null}

          {/*
            When filtering leaves nothing, say clearly it was **filtered out**, rather than reusing
            the "no Agents yet" empty state;
            the latter makes users think the workspace is empty and click "New", when they only need
            to clear the filter.
          */}
          {shown.length === 0 ? (
            <Empty
              icon="gravity-ui:face-robot"
              title={t("agent.mineOnlyEmpty")}
              description={t("agent.emptyDesc")}
              action={
                <Button variant="outline" onPress={() => setMineOnly(false)}>
                  {t("common.all")}
                </Button>
              }
            />
          ) : (
            <div className="grid gap-4 lg:grid-cols-2">
              {shown.map((a) => {
                const spec = modelSpec(a.model.id);
                return (
                  <Link
                    key={a.id}
                    to={wp(`/agents/${a.id}`)}
                    /*
                      ⚠️ `min-w-0` cannot be dropped; it fixes an overflow **only visible on narrow
                      screens**.

                      A grid item's `min-width` defaults to `auto` (= min-content),
                      and the system prompt text in the card is a long string that does not break
                      easily, so min-content
                      was pushed to 839px; on a 390px phone the whole right part of the card (status
                      badge,
                      creator avatar) **was pushed off screen**, while documentElement.scrollWidth
                      still equaled 390, so assertions like "check whether body scrolls
                      horizontally" were **falsely green**.
                      Overflow can only be judged by measuring each element's rect.

                      This is an existing problem (with this change's avatar and filter removed, the
                      card is still 839px),
                      fixed along the way: it happens exactly on the row where the creator avatar is.
                    */
                    className="no-underline min-w-0"
                data-testid={`agent-card-${a.id}`}
              >
                <Card className="h-full">
                  <Card.Content className="flex h-full flex-col gap-4 pt-6">
                    <div className="flex items-start justify-between gap-3">
                      <div className="flex min-w-0 items-start gap-3">
                        <span className="flex size-9 shrink-0 items-center justify-center r-chip bg-surface-secondary">
                          <Icon icon="gravity-ui:face-robot" className="size-4 text-muted" />
                        </span>
                        <div className="flex min-w-0 flex-col gap-1">
                          <p className="truncate font-medium">{a.name}</p>
                          <p className="truncate text-xs text-muted">{a.description}</p>
                        </div>
                      </div>
                      <Chip size="sm" color={STATUS_COLOR[a.status]} variant="soft">
                        <Chip.Label>{t(`enum.agentStatus.${a.status}`)}</Chip.Label>
                      </Chip>
                    </div>

                    {/*
                      The prompt preview is fixed at two lines.
                      Padding must be on the outer element: after -webkit-line-clamp cuts lines,
                      overflow:hidden
                      clips at the padding edge, so an element with padding-bottom leaks the third
                      line into
                      its bottom padding (looks like broken rendering). An inner <p> without padding
                      has no such problem.
                    */}
                    <div className="r-inset bg-surface-secondary px-3 py-2">
                      <p className="clamp-2 text-xs leading-4 text-muted">
                        {truncate(a.system, 160)}
                      </p>
                    </div>

                    <div className="mt-auto flex flex-wrap items-center gap-2">
                      <Chip size="sm" variant="soft">
                        <Chip.Label>{spec.label}</Chip.Label>
                      </Chip>
                      {a.skillIds.length > 0 ? (
                        <Chip size="sm" variant="soft">
                          <Chip.Label>{t("agent.skillCount", { count: a.skillIds.length })}</Chip.Label>
                        </Chip>
                      ) : null}
                      {a.feishuAppIds.length > 0 ? (
                        <Chip size="sm" variant="soft">
                          <Chip.Label>{t("agent.feishuBound")}</Chip.Label>
                        </Chip>
                      ) : null}
                    </div>

                    <div className="flex items-center gap-4 text-xs text-muted">
                      <span className="num">v{a.version}</span>
                      <span className="num">{t("agent.toolCount", { count: a.builtin_tools.length })}</span>
                      <span>{t("agent.updatedAt", { time: formatRelative(a.updatedAt, language) })}</span>
                      {/*
                        The creator avatar is **on the right**: it is information that "can be
                        there, but needs no big emphasis"
                        (the user's own words), placed after the time at the end of the row, so
                        people scanning the row read
                        version and time first, and glance at the avatar only when needed.

                        ⚠️ Rendered unconditionally, **do not write `a.createdBy && ...`**: old agents
                        have no
                        creator, and then it must show a question-mark avatar and say "not recorded"
                        plainly, rather than a blank spot;
                        one missing avatar in a column looks like broken rendering. The check is in
                        UserAvatar,
                        and callers do not repeat it.
                      */}
                      <span className="ml-auto shrink-0">
                        <UserAvatar
                          email={a.createdBy}
                          profile={profiles[(a.createdBy ?? "").toLowerCase()]}
                          label={t("agent.createdBy")}
                          testId={`agent-creator-${a.id}`}
                        />
                      </span>
                    </div>
                  </Card.Content>
                </Card>
              </Link>
                );
              })}
            </div>
          )}
        </>
      )}
    </PageBody>
  );
}

/**
 * Checkbox area for built-in tools and skills (**only on the create page**).
 *
 * The matching two parts on the detail page are in ConfigTab / SkillsTab of `AgentDetailPage`, and
 * their shape is deliberately the same
 * (the same `tool-<id>` / `bind-skill-<name>` testids), so "what you check at creation" and
 * "what you change later" are one mental model, and e2e can use one set of selectors.
 *
 * # Built-in skills inline, workspace skills in a dialog: two different things, not laziness
 *
 * This used to say "**only list built-in skills, not workspace-created ones**", because "the create
 * form already has four blocks, name/
 * description/prompt/model, and stuffing in a whole skill library would bury the main line". **That
 * half still holds,
 * and the whole skill library is still not inlined into the form**; but it had an unattended side
 * effect: at the create step users
 * could not see that "workspace skills can be bound too", so they thought the platform only had
 * three built-in skills.
 * (The feature was always there; the detail page's SkillsTab can bind all of them; only the entry
 * was missing.)
 *
 * So now: **built-in skills inline, workspace skills behind a button that opens a dialog**. The
 * difference is not importance,
 * but **visibility of defaults**:
 *
 *  built-in skills   **checked by default**, so they must be laid out on the form: defaults the
 *                    user cannot see
 *                    amount to deciding for them in secret. Three items, little space.
 *  workspace skills  none checked by default, and there may be dozens. Laid out, that is the
 *                    "bury the main line" above;
 *                    in a dialog the form only gains one row (button + "N/20 selected" summary).
 *
 * The dialog's shape matches the detail page word for word (the same testids, the same
 * `skillSourceBadge`);
 * see `components/SkillLibraryPicker.tsx`.
 */
function NewAgentCapabilities({
  draft,
  setDraft,
  builtinSkills,
  skills,
}: {
  draft: AgentDraft;
  setDraft: (d: AgentDraft) => void;
  builtinSkills: Skill[];
  /** All skills in the workspace (built-in included). null means not fetched yet or fetch failed. */
  skills: Skill[] | null;
}) {
  const { t } = usePrefs();
  const [pickerOpen, setPickerOpen] = useState(false);

  function toggleTool(id: BuiltinTool) {
    const has = draft.builtin_tools.includes(id);
    setDraft({
      ...draft,
      builtin_tools: has
        ? draft.builtin_tools.filter((x) => x !== id)
        : [...draft.builtin_tools, id],
    });
  }

  function toggleSkill(id: string) {
    const has = draft.skillIds.includes(id);
    setDraft({
      ...draft,
      skillIds: has ? draft.skillIds.filter((x) => x !== id) : [...draft.skillIds, id],
    });
  }

  /*
   * The summary only lists **non-built-in** ones.
   *
   * Built-in skills are visible on their own in the block above (three checkboxes right there);
   * counting them in this summary would
   * give "3 workspace skills bound" while the user picked none, which is false.
   *
   * ⚠️ But **the limit count must not be computed this way**: Ark's 20 is a total, and built-in
   * skills take slots too
   * (pinned by the backend's TestBuiltinSkillCountsTowardArkLimit). So the dialog counts
   * the full length of `draft.skillIds`, on a basis **deliberately different** from here; each
   * matches its own meaning.
   *
   * Map to skill objects in the order of `draft.skillIds` (i.e. the order the user checked them),
   * rather than the order of `skills`: the one just checked goes last, right where the user's eyes
   * just left.
   * Ids that cannot be resolved are dropped: they should not appear on the create page (ids can only
   * come from the same skills),
   * and if they did, they would render as a nameless empty chip; better not to draw it.
   */
  const builtinIds = new Set(builtinSkills.map((s) => s.id));
  const byId = new Map((skills ?? []).map((s) => [s.id, s]));
  const picked = draft.skillIds
    .filter((id) => !builtinIds.has(id))
    .map((id) => byId.get(id))
    .filter((s): s is Skill => s !== undefined);

  return (
    <>
      <div className="flex flex-col gap-4">
        <SectionTitle
          title={t("agent.toolsSection")}
          description={t("agent.toolsSectionDesc")}
        />
        <div className="grid gap-3 sm:grid-cols-2">
          {BUILTIN_TOOLS.map((tool) => (
            <Checkbox
              key={tool.id}
              isSelected={draft.builtin_tools.includes(tool.id)}
              onChange={() => toggleTool(tool.id)}
              data-testid={`tool-${tool.id}`}
            >
              <Checkbox.Content>
                <Checkbox.Control>
                  <Checkbox.Indicator />
                </Checkbox.Control>
                <span className="flex flex-col">
                  <span className="font-mono text-sm">{tool.label}</span>
                  <span className="text-xs text-muted">{t(`enum.tool.${tool.id}`)}</span>
                </span>
              </Checkbox.Content>
            </Checkbox>
          ))}
        </div>
      </div>

      {/*
        When the skill list has not come back yet (or this deployment has no built-in skills at
        all) the whole block is not rendered.
        **No placeholder skeleton**: this block confirms "defaults are already set up", not data
        the user is waiting for;
        leaving a flashing gray bar for it would only make the form look unfinished loading.
      */}
      {builtinSkills.length > 0 ? (
        <div className="flex flex-col gap-4">
          <SectionTitle
            title={t("agent.builtinSkillsSection")}
            description={t("agent.builtinSkillsSectionDesc")}
          />
          <div className="flex flex-col gap-2">
            {builtinSkills.map((s) => (
              <div
                key={s.id}
                className={[
                  "flex items-start gap-3 r-inset px-3 py-3",
                  draft.skillIds.includes(s.id) ? "bg-surface-secondary" : "",
                ].join(" ")}
              >
                <Checkbox
                  isSelected={draft.skillIds.includes(s.id)}
                  onChange={() => toggleSkill(s.id)}
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
                    <Chip size="sm" variant="soft">
                      <Chip.Label>{t("skill.builtin")}</Chip.Label>
                    </Chip>
                  </div>
                  <p className="text-xs text-muted">{truncate(skillText(s, t).description, 120)}</p>
                </div>
              </div>
            ))}
          </div>
        </div>
      ) : null}

      {/*
        Workspace skills: a one-line summary + a button that opens the dialog. **The form only gets
        this one row longer**, exactly what
        the file header worried about with "stuffing in a whole skill library would bury the main
        line".

        ⚠️ **When the skill list cannot be fetched the whole block is not rendered** (skills ===
        null), for the same reason as the authorization block:
        skills are a bonus, and "cannot create an agent because skills cannot be listed" turns an
        optional feature into a single point on the main path.
        ⚠️ But **when the list is empty this block stays**: then the dialog guides "go install one
        from the skill library",
        and hiding it would leave users unaware of that path (see SkillLibraryPicker's empty state).
      */}
      {skills !== null ? (
        <div className="flex flex-col gap-4">
          <SectionTitle
            title={t("skill.pickerSection")}
            description={t("skill.pickerSectionDesc")}
            actions={
              <Button variant="outline" size="sm" onPress={() => setPickerOpen(true)} data-testid="open-skill-picker">
                {t("skill.pickerOpen")}
              </Button>
            }
          />
          <PickedSkillSummary skills={picked} onUnbind={toggleSkill} />

          <SkillLibraryPicker
            isOpen={pickerOpen}
            onOpenChange={setPickerOpen}
            skills={skills}
            selectedIds={draft.skillIds}
            /*
              The dialog hands back the **complete** id list (including built-in skills that did not
              appear in it):
              it was edited starting from draft.skillIds, so the built-in ones come along unchanged.
              Assign the whole thing directly, no merge: a merge would eat the user's "uncheck a
              workspace
              skill" action in the dialog (that id is not in the new list, and merging would add it
              back).
            */
            onConfirm={(ids) => setDraft({ ...draft, skillIds: ids })}
          />
        </div>
      ) : null}
    </>
  );
}

/**
 * The most chips laid out in the summary; the rest fold into "and N more".
 *
 * The limit itself is 20 (`MAX_SKILLS_PER_AGENT`); laid out fully that is four or five lines on a
 * narrow screen, pushing the
 * submit button below off screen. 6 is a rule of thumb for "fits within two lines"; **it need not
 * be exact**:
 * both folded and unfolded are correct, it is only about which is easier to scan.
 */
const SUMMARY_CHIP_LIMIT = 6;

/**
 * Summary on the create page of "which workspace skills are bound": **a row of skill-name chips,
 * not a number**.
 *
 * # Why a count is not enough
 *
 * This used to be one sentence, "N workspace skills bound". It cannot say **which ones**, and that
 * is exactly what the user
 * needs to confirm at this step: skills are checked in a dialog, and once the dialog closes nothing
 * is visible,
 * so "did I pick the right ones" could only be answered by reopening the dialog and counting again.
 *
 * The same lesson was already learned on the FeishuTab of `AgentDetailPage` (see
 * "why add a bound/unbound note" there): the status badge there shows **the app's own health**,
 * yet users read it as "the current agent is connected". **A signal not about "this one"
 * gets read as being about "this one".** A count is the same kind of thing: it answers "how many",
 * while the user is asking "which ones".
 *
 * # Why the list is still not laid out
 *
 * The judgment "do not inline the whole skill library into the create form" has not been
 * overturned (see the file headers of `NewAgentCapabilities`
 * and `SkillLibraryPicker`). What is drawn here is **only the ones the user already picked**,
 * not the full set of options: none picked is one line of text, N picked is N chips, and beyond
 * `SUMMARY_CHIP_LIMIT` they fold into "+N". The form length **is decided by the user's choices and
 * has an upper bound**,
 * which is a different thing from "laying out dozens of skills on the form".
 *
 * # Chips can be removed in place
 *
 * Not a duplicate of "open the picker"; the two go opposite directions: **the dialog handles adding
 * (browse the whole library, search, read descriptions),
 * chips handle removing**. Finding a wrong pick and only being able to "open dialog → find that row
 * → uncheck → confirm", four steps,
 * while the thing is right in front of you. The shape copies `ActiveChip` in `TimelineToolbar` (a
 * native button rather than
 * `<Button isIconOnly>`, whose built-in padding and minimum touch size would make the chip twice as
 * tall).
 *
 * # ⚠️ The testid cannot reuse `bind-skill-<name>`
 *
 * That testid belongs to checkboxes (one in the detail page's SkillsTab and one in the dialog,
 * deliberately the same shape).
 * If the chips here used that name too, **with "a skill bound + dialog open" the same testid would
 * match
 * two elements**, hitting Playwright's strict mode; exactly what the `SkillLibraryPicker` file
 * header
 * "listing in both places would duplicate the testid" guards against, just happening somewhere
 * else.
 * So new ones are used, `picked-skill-<name>` / `unbind-skill-<name>`, named in the style of
 * `bind-skill-*`
 * (also by `s.name` rather than id), but not sharing the same string.
 */
function PickedSkillSummary({
  skills,
  onUnbind,
}: {
  /** Bound **workspace** skills, in the order the user checked them. Built-in ones are not here. */
  skills: Skill[];
  onUnbind: (id: string) => void;
}) {
  const { t } = usePrefs();

  if (skills.length === 0) {
    return (
      <p className="text-sm text-muted" data-testid="picked-skill-summary">
        {t("skill.pickerSummaryNone")}
      </p>
    );
  }

  const shown = skills.slice(0, SUMMARY_CHIP_LIMIT);
  const overflow = skills.length - shown.length;

  return (
    /*
      `flex-wrap` rather than horizontal scrolling: a row of chips overflowing on narrow screens is
      a trap this repo has hit,
      and **wrapping naturally has no overflow** (not enough width just takes another line, no
      breakpoint logic needed).
      The scrollbar path would also have to handle "users do not know there is more on the right";
      not worth it here.

      role/aria-label give screen reader users an overview: visually the count is replaced by the
      chips themselves,
      but "how many in total" is still useful information for listeners.
    */
    <div
      role="group"
      aria-label={t("skill.pickerSummary", { count: skills.length })}
      className="flex flex-wrap items-center gap-2"
      data-testid="picked-skill-summary"
    >
      {shown.map((s) => (
        <Chip key={s.id} size="sm" variant="soft" data-testid={`picked-skill-${s.name}`}>
          <Chip.Label>{skillText(s, t).title}</Chip.Label>
          <button
            type="button"
            aria-label={t("skill.unbindLabel", { name: skillText(s, t).title })}
            onClick={() => onUnbind(s.id)}
            data-testid={`unbind-skill-${s.name}`}
            className="ml-1 flex items-center opacity-60 cursor-[var(--cursor-interactive)] hover:opacity-100"
          >
            <Icon icon="gravity-ui:xmark" className="size-3" />
          </button>
        </Chip>
      ))}
      {/*
        The folded ones are not drawn as chips: they cannot be removed (removing a "+3" means
        nothing),
        and drawing them the same way would make users click that x. One plain-text line saying how
        many more is enough;
        to see all, open the dialog, which lists everything with the selected ones checked.
      */}
      {overflow > 0 ? (
        <span className="num text-xs text-muted" data-testid="picked-skill-overflow">
          {t("skill.pickerSummaryMore", { count: overflow })}
        </span>
      ) : null}
    </div>
  );
}

export function NewAgentPage() {
  const { t } = usePrefs();
  const { workspace } = useWorkspace();
  const wp = useWorkspacePath();
  const navigate = useNavigate();
  const [draft, setDraft] = useState<AgentDraft>(emptyAgentDraft());
  const [errors, setErrors] = useState<Partial<Record<string, string>>>({});
  const [submitting, setSubmitting] = useState(false);

  // Skill list: built-in skills (the three checked by default) and the "pick from skill library"
  // dialog share this one copy.
  // Failure does not block creation: skills are a bonus, and "cannot create an agent because skills
  // cannot be listed" turns an optional feature
  // into a single point on the main path. On failure data is null, and both blocks are not
  // rendered at all.
  const fetchSkills = useCallback(() => api.listSkills(workspace.id), [workspace.id]);
  const { data: skills } = useAsyncData(fetchSkills, t("common.loadFailedUnknown"));

  // Authorization list: **its own hook**, for the same reason as the skill list: failing to fetch
  // should not block creating an agent.
  // On failure data is null, and the component does not render that block at all.
  //
  // ⚠️⚠️ **Must carry workspaceId**, same as AgentDetailPage: the "start authorization" on this page
  // authorizes per workspace,
  // and if the status call misses it, it is judged by the account-level default app: authorization
  // succeeds, the card still says "not authorized".
  const fetchIntegrations = useCallback(
    () => api.listIntegrations(workspace.id),
    [workspace.id],
  );
  const {
    data: providers,
    setData: setProviders,
  } = useAsyncData(fetchIntegrations, t("integrations.loadFailed"));

  /*
   * After an in-place authorization succeeds, **replace only that one item**; do not refetch the
   * whole list.
   *
   * ⚠️ On the **create page** this rule matters even more: refetching the whole list makes providers
   * go null and then the new value,
   * and this page has a `defaultsApplied` effect on the same tree; any casual "reset when the list
   * changes" code
   * would wipe the name/description/prompt the user already filled in, with zero errors.
   * Replacing one item only touches one element of providers, and leaves draft untouched to the byte.
   */
  const replaceProvider = useCallback(
    (next: IntegrationState) => {
      setProviders((prev) => (prev ?? []).map((p) => (p.id === next.id ? next : p)));
    },
    [setProviders],
  );

  // "Can I configure authorization injection". Three states; while not known yet that block is not
  // rendered at all.
  // ⚠️ The create-agent path must check it too: otherwise a member checks it, clicks save and gets
  // a 403 from the server,
  // after already filling in the whole form.
  const canManage = useCanManage(workspace.id);

  /*
   * Built-in skills are checked by default.
   *
   * # ⚠️ Ids must be filtered from the list, not written as literals
   *
   * The source of truth is the backend's service/skill/skillbuiltin.go; hard-coding
   * "skill-builtin-lark-cli" in the frontend would
   * **silently break** when the backend changes ids or adds/removes built-in skills: the checkbox is
   * still there, but it checks a nonexistent id,
   * and a dangling skill id only logs a Warn in core/skillark.go, showing up as "the skill is
   * checked,
   * but the model knows nothing about it". Filtering by `source === "builtin"` always follows the
   * backend.
   *
   * # Why a ref applies it only once, rather than checking `skillIds.length === 0`
   *
   * Because "the user unchecked both" and "defaults not applied yet" look **exactly the same** in the
   * draft
   * (both empty arrays). With the empty check, the render right after the user unchecks the last one
   * would refill it,
   * showing as "cannot uncheck"; the classic symptom of defaults put in the wrong place.
   * The ref is a one-time gate: once applied it never touches draft again, and every later change
   * by the user is final.
   */
  const defaultsApplied = useRef(false);
  const builtinSkills = (skills ?? []).filter(isBuiltinSkill);
  useEffect(() => {
    if (defaultsApplied.current || skills === null) return;
    defaultsApplied.current = true;
    const ids = skills.filter(isBuiltinSkill).map((s) => s.id);
    if (ids.length === 0) return;
    // Functional update: this effect should not depend on draft (depending on it would re-run after
    // every edit),
    // and the draft in the closure would be the stale first-frame copy: a name the user typed
    // before skills loaded would be wiped.
    setDraft((cur) => ({ ...cur, skillIds: [...cur.skillIds, ...ids] }));
  }, [skills]);

  async function submit() {
    const e: Partial<Record<string, string>> = {};
    if (!draft.name.trim()) e.name = t("agent.nameRequired");
    if (!draft.system.trim()) e.system = t("agent.systemRequired");
    setErrors(e);
    if (Object.keys(e).length > 0) return;

    setSubmitting(true);
    try {
      const agent = await api.createAgent(workspace.id, draft);
      navigate(wp(`/agents/${agent.id}`));
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <PageBody>
      <PageHeader
        title={t("agent.new")}
        description={t("agent.newDesc")}
        above={
          <Link
            to={wp("/agents")}
            className="flex w-fit items-center gap-1 text-sm text-muted no-underline hover:text-foreground"
          >
            <Icon icon="gravity-ui:arrow-left" className="size-4" />
            Agents
          </Link>
        }
      />

      <Card>
        <Card.Content className="flex flex-col gap-6 pt-6">
          <div className="grid gap-5 sm:grid-cols-2">
            <TextField isRequired isInvalid={Boolean(errors.name)}>
              <Label>{t("agent.name")}</Label>
              <Input
                placeholder={t("agent.namePlaceholder")}
                value={draft.name}
                onChange={(e) => setDraft({ ...draft, name: e.target.value })}
                data-testid="agent-name"
                autoFocus
              />
              {errors.name ? <FieldError>{errors.name}</FieldError> : null}
            </TextField>
            <TextField>
              <Label>{t("ws.desc")}</Label>
              <Input
                placeholder={t("agent.descPlaceholder")}
                value={draft.description}
                onChange={(e) => setDraft({ ...draft, description: e.target.value })}
                data-testid="agent-desc"
              />
            </TextField>
          </div>

          <TextField isRequired isInvalid={Boolean(errors.system)}>
            <Label>{t("agent.system")}</Label>
            <TextArea
              rows={10}
              placeholder={t("agent.systemPlaceholder")}
              value={draft.system}
              onChange={(e) => setDraft({ ...draft, system: e.target.value })}
              data-testid="agent-system"
            />
            <Description>{t("agent.systemDesc")}</Description>
            {errors.system ? <FieldError>{errors.system}</FieldError> : null}
          </TextField>

          <ModelPicker draft={draft} setDraft={setDraft} />

          <NewAgentCapabilities
            draft={draft}
            setDraft={setDraft}
            builtinSkills={builtinSkills}
            skills={skills}
          />

          {/* Executor choice: placed between "what this agent can do" and "authorization injection";
              it answers "where tools run", runtime config like capabilities; both blocks share one component. */}
          <AgentExecutorSection
            draft={draft}
            setDraft={setDraft}
            workspaceId={workspace.id}
            workspaceKey={workspace.key}
          />

          {/*
            Authorization + custom env: the same kind as "what this agent can use", so it follows
            right after
            tools/skills. Both blocks **share one component** with the detail page, the same shape
            word for word.
          */}
          <AgentIntegrationsSection
            draft={draft}
            setDraft={setDraft}
            providers={providers}
            canManage={canManage}
            onProviderRefreshed={replaceProvider}
            // ⚠️ Pass the workspace: it decides "which Feishu app to authorize when starting
            // authorization". Without it the account-level default
            // is authorized, while the agent at runtime picks by its workspace; when the two
            // differ it fails with zero errors.
            workspaceId={workspace.id}
            workspaceKey={workspace.key}
          />
          <AgentEnvSection draft={draft} setDraft={setDraft} />
          <AgentEnvAliasSection draft={draft} setDraft={setDraft} canManage={canManage} />

          <div className="flex justify-end gap-2">
            <Link to={wp("/agents")} className="no-underline">
              <Button variant="ghost">{t("common.cancel")}</Button>
            </Link>
            <Button onPress={submit} isPending={submitting} data-testid="agent-submit">
              {t("common.create")}
            </Button>
          </div>
        </Card.Content>
      </Card>
    </PageBody>
  );
}

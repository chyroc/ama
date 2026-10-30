/**
 * Agent detail. Four tabs:
 *
 * - Config: prompt, model, built-in tools
 * - Skills: picked from the workspace skill library; stores references (skillIds), not copies
 * - Feishu: bind Feishu apps, visible from both the app side and here
 * - Runs: entry to this agent's run history
 *
 * Tab state lives in the URL's ?tab=, so other places can link straight to a tab (the binding chip
 * on the Feishu page
 * links to ?tab=feishu).
 */

import { skillText } from "@/data/builtinSkills";
import { useCallback, useEffect, useState } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import {
  Button,
  Card,
  Checkbox,
  Chip,
  Description,
  Input,
  Label,
  ListBox,
  Select,
  Separator,
  Switch,
  TextArea,
  TextField,
  Tooltip,
} from "@heroui/react";
import { Icon } from "@iconify/react";
import { api } from "@/data/api";
import type { ArkConsoleRef } from "@/data/api";
import { ArkConsoleLink } from "@/components/ArkConsoleLink";
import {
  AgentEnvAliasSection,
  AgentEnvSection,
  AgentExecutorSection,
  AgentIntegrationsSection,
} from "@/components/AgentRuntimeSections";
import { useCanManage, useCanWrite } from "@/lib/useCanManage";
import { RevisionPanel } from "@/components/RevisionPanel";
import { useWorkspace, useWorkspacePath } from "@/app/WorkspaceGate";
import {
  AsyncErrorBlock,
  Empty,
  LoadingBlock,
  PageBody,
  PageHeader,
  SectionTitle,
} from "@/components/Page";
import { ConfirmModal } from "@/components/FormModal";
import { Issue } from "@/components/FeishuReadiness";
import { ModelPicker } from "@/pages/AgentsPage";
import { AgentArkCard } from "@/pages/AgentArkCard";
import { formatDuration, formatRelative, truncate } from "@/lib/format";
import { useAsyncData } from "@/lib/useAsyncData";
import { useAgentSyncStatus } from "@/lib/useAgentSyncStatus";
import {
  AGENT_STATUS_LABELS,
  BUILTIN_TOOLS,
  FEISHU_STATUS_COLOR,
  modelSpec,
  skillSourceBadge,
  followUpOrDefault,
  progressDetailOrDefault,
  toAgentDraft,
  type Agent,
  type AgentDraft,
  type AgentStatus,
  type FeishuApp,
  type FeishuTriggerMode,
  type FollowUpMode,
  type ProgressDetail,
  type Run,
  type IntegrationState,
  type Skill,
} from "@/types/domain";
import { RunStatusChip } from "@/components/RunStatusChip";
import { usePrefs } from "@/prefs/PrefsContext";
import type { MessageKey } from "@/prefs/messages";

/**
 * Tab definitions.
 *
 * Store translation keys, not text: these are module-level constants, evaluated before the React
 * context exists, so t() cannot be called.
 * Translate at render time.
 */
const TABS = [
  { id: "config", labelKey: "agent.tabConfig" },
  { id: "skills", labelKey: "agent.tabSkills" },
  { id: "feishu", labelKey: "agent.tabFeishu" },
  { id: "runs", labelKey: "agent.tabRuns" },
  // Version history goes last: it is an entry for "when something goes wrong", and should not
  // crowd the daily path
  // (the user's words: "do not let it steal the show").
  { id: "revisions", labelKey: "revision.tab" },
] as const satisfies readonly { id: string; labelKey: MessageKey }[];

type TabId = (typeof TABS)[number]["id"];

function toDraft(a: Agent): AgentDraft {
  // Implemented in types/domain.ts: FeishuBindPrompt also needs "send everything back, change one
  // field";
  // building it separately in both places would surely drift, and the symptoms of a missing field
  // (400 / silently changed credential injection) do not point at the root cause.
  return toAgentDraft(a);
}

/** Whether the draft differs from the saved agent, used to decide whether to show "Save". */
function isDirty(a: Agent, d: AgentDraft): boolean {
  return JSON.stringify(toDraft(a)) !== JSON.stringify(d);
}

// ---------------------------------------------------------------------------
// Config tab
// ---------------------------------------------------------------------------

function ConfigTab({
  draft,
  setDraft,
  agentId,
  createdByEmail,
}: {
  agentId: string;
  draft: AgentDraft;
  setDraft: (d: AgentDraft) => void;
  /** Creator email of the saved agent: the executor's personal direct-connect read-only state depends on it. */
  createdByEmail?: string;
}) {
  const { t } = usePrefs();
  const wp = useWorkspacePath();

  /*
   * The authorization list is **its own hook, deliberately not merged into the page-level
   * Promise.all**.
   *
   * That one is all-or-nothing: putting listIntegrations in it means that once it is slow or fails,
   * the whole agent detail page will not open, while authorization is just a small optional piece
   * of config on this page.
   * Same reason NewAgentPage gives the skill list its own hook ("failure does not block creation").
   *
   * On failure null is passed, and the component does not render that block at all.
   */
  // ⚠️ Fetched before fetchIntegrations: that hook's dependency needs workspace.id
  // (authorization state must be judged per workspace; see the comment below).
  const { workspace } = useWorkspace();

  // ⚠️⚠️ **Must carry workspaceId**: the "start authorization" next to the Feishu card on this
  // page authorizes **per workspace**,
  // and the state must be judged by the same workspace, otherwise it is the dead end "authorization
  // succeeded, the card says not authorized forever"
  // (only three of the four paths were wired; see the file header of
  // server/core/integration_larkuser.go).
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
   * ⚠️ This is where "do not wipe the draft the user is editing" lands. Refetching the list makes
   * providers go
   * null (useAsyncData clears on dependency change) and then the new value, so the whole
   * authorization area flashes and rebuilds;
   * and the draft is on the same tree, so any casual "reset after loading" would erase the
   * half-edited
   * name/prompt. Replacing one item only touches one element of providers, and leaves draft
   * untouched to the byte.
   *
   * The same shape as replaceOne in IntegrationsTab (which also refreshes just one card).
   */
  const replaceProvider = useCallback(
    (next: IntegrationState) => {
      setProviders((prev) => (prev ?? []).map((p) => (p.id === next.id ? next : p)));
    },
    [setProviders],
  );

  /*
   * "Can I change authorization injection". Three states; while not known yet that block is not
   * rendered at all.
   *
   * Like providers, its own hook, not merged into the page-level Promise.all: the same reason
   * (all-or-nothing would let a small optional piece of config bring down the whole page). The two
   * requests go out concurrently.
   *
   * ⚠️ workspace is fetched above (before fetchIntegrations), where its id is needed.
   */
  const canManage = useCanManage(workspace.id);

  /*
   * "Has it been created on the Ark side yet": only used to decide whether the note below appears.
   *
   * Fetched on its own for the same reason as the two hooks above: it is optional, and putting it
   * in the page-level Promise.all
   * would let a secondary note bring down the whole page. On failure treat it as absent (no note).
   */
  const [arkConsole, setArkConsole] = useState<ArkConsoleRef | null>(null);
  useEffect(() => {
    let alive = true;
    void api
      .getAgentArkConsole(agentId)
      .then((ref) => {
        if (alive) setArkConsole(ref);
      })
      .catch(() => {
        if (alive) setArkConsole(null);
      });
    return () => {
      alive = false;
    };
  }, [agentId]);

  function toggleTool(id: (typeof BUILTIN_TOOLS)[number]["id"]) {
    const has = draft.builtin_tools.includes(id);
    setDraft({
      ...draft,
      builtin_tools: has
        ? draft.builtin_tools.filter((x) => x !== id)
        : [...draft.builtin_tools, id],
    });
  }

  return (
    <div className="flex flex-col gap-6">
      <Card>
        <Card.Content className="flex flex-col gap-5 pt-6">
          <SectionTitle title={t("agent.basicInfo")} />
          <div className="grid gap-5 sm:grid-cols-2">
            <TextField isRequired>
              <Label>{t("agent.name")}</Label>
              <Input
                value={draft.name}
                onChange={(e) => setDraft({ ...draft, name: e.target.value })}
                data-testid="cfg-name"
              />
            </TextField>
            <TextField>
              <Label>{t("ws.desc")}</Label>
              <Input
                value={draft.description}
                onChange={(e) => setDraft({ ...draft, description: e.target.value })}
                data-testid="cfg-desc"
              />
            </TextField>
          </div>

          <Select
            className="max-w-xs"
            selectedKey={draft.status}
            onSelectionChange={(k) => setDraft({ ...draft, status: String(k) as AgentStatus })}
          >
            <Label>{t("agent.status")}</Label>
            <Select.Trigger data-testid="cfg-status">
              <Select.Value />
              <Select.Indicator />
            </Select.Trigger>
            <Select.Popover>
              <ListBox>
                {(Object.keys(AGENT_STATUS_LABELS) as AgentStatus[]).map((s) => (
                  <ListBox.Item key={s} id={s} textValue={t(`enum.agentStatus.${s}`)}>
                    {t(`enum.agentStatus.${s}`)}
                  </ListBox.Item>
                ))}
              </ListBox>
            </Select.Popover>
          </Select>
        </Card.Content>
      </Card>

      <Card>
        <Card.Content className="flex flex-col gap-5 pt-6">
          <SectionTitle
            title={t("agent.system")}
            description={t("agent.systemSectionDesc")}
          />
          <TextField isRequired>
            <Label className="sr-only">{t("agent.system")}</Label>
            <TextArea
              rows={14}
              value={draft.system}
              onChange={(e) => setDraft({ ...draft, system: e.target.value })}
              data-testid="cfg-system"
            />
            <Description>
              {t("agent.systemCount", { count: draft.system.length })}
            </Description>
          </TextField>
        </Card.Content>
      </Card>

      <Card>
        <Card.Content className="flex flex-col gap-5 pt-6">
          <SectionTitle title={t("agent.model")} description={t("agent.modelSectionDesc")} />
          <ModelPicker draft={draft} setDraft={setDraft} />
        </Card.Content>
      </Card>

      <Card>
        <Card.Content className="flex flex-col gap-5 pt-6">
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
        </Card.Content>
      </Card>

      {/*
        "What this agent can get at runtime": executor choice + authorization checks + custom env.
        The three blocks **share one component** with the create page, the same shape word for word
        (see AgentRuntimeSections).

        There are several authorization sources, so it is a group of checkboxes rather than a single
        boolean switch.
      */}
      <Card>
        <Card.Content className="flex flex-col gap-6 pt-6">
          <AgentExecutorSection
            draft={draft}
            setDraft={setDraft}
            workspaceId={workspace.id}
            workspaceKey={workspace.key}
            createdByEmail={createdByEmail}
          />
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
        </Card.Content>
      </Card>

      {/*
        Ark credentials: the most specific of the three config levels (agent → workspace → account).
        **It does not go through draft**: credentials are an independent resource with their own
        endpoint, and changing them should not light up "unsaved changes"
        (that would make people think it does not take effect without clicking save). See
        AgentArkCard.
      */}
      <AgentArkCard agentId={agentId} workspacePath={wp} />

      {/*
        "Why is there no 'Open in Ark console' link": **only shown when it would really confuse
        people**.
        The check is two conditions together: Ark credentials are configured (there is a region, so
        this person knows what Ark is),
        and the Ark side has not been created yet (no arkAgentId).

        # Why not a permanent note

        That would explain "something that does not exist" on every agent page; most users do not
        even know
        this link exists, and explaining it first would only create confusion, also breaking "do not
        let secondary features steal the show".
        On the other hand, saying nothing is not OK either: people who know the link exists (seen on
        another agent)
        would think it is broken when they cannot find it here.

        It sits below the Ark credentials card: that is the Ark-related area of this page,
        and people looking for this link will naturally see it there.
      */}
      {arkConsole && arkConsole.region && !arkConsole.arkAgentId ? (
        <p className="text-xs text-muted" data-testid="agent-ark-console-absent">
          {t("arkConsole.agentNotCreated")}
        </p>
      ) : null}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Skills tab
// ---------------------------------------------------------------------------

function SkillsTab({
  draft,
  setDraft,
  skills,
}: {
  draft: AgentDraft;
  setDraft: (d: AgentDraft) => void;
  skills: Skill[];
}) {
  const { t } = usePrefs();
  const wp = useWorkspacePath();

  function toggle(id: string) {
    const has = draft.skillIds.includes(id);
    setDraft({
      ...draft,
      skillIds: has ? draft.skillIds.filter((s) => s !== id) : [...draft.skillIds, id],
    });
  }

  if (skills.length === 0) {
    return (
      <Empty
        icon="gravity-ui:puzzle"
        title={t("skill.noneInWorkspace")}
        description={t("skill.noneInWorkspaceDesc")}
        action={
          <Link to={wp("/skills")} className="no-underline">
            <Button variant="outline">{t("skill.goToLibrary")}</Button>
          </Link>
        }
      />
    );
  }

  return (
    <Card>
      <Card.Content className="flex flex-col gap-5 pt-6">
        <SectionTitle
          title={t("skill.bindTitle")}
          description={t("skill.bindDesc")}
          actions={
            <Link to={wp("/skills")} className="no-underline">
              <Button variant="ghost" size="sm">
                {t("skill.manageLibrary")}
              </Button>
            </Link>
          }
        />

        <div className="flex flex-col gap-2" data-testid="skill-binding-list">
          {skills.map((s) => {
            const bound = draft.skillIds.includes(s.id);
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
                    <span className="num text-xs text-muted">v{s.latestVersion}</span>
                    {/*
                      Source badge (currently only "platform built-in"). Goes through the single
                      helper in domain, rather than writing === comparisons one by one here: the badge
                      renders once on each of four pages,
                      and missing one spot when adding a source **gives no error**; the skill just
                      looks
                      self-built on this page. The testid keeps the existing literal (e2e relies on
                      bind-skill-builtin-*).
                    */}
                    {(() => {
                      const badge = skillSourceBadge(s.source);
                      return badge ? (
                        <Chip
                          size="sm"
                          variant="soft"
                          data-testid={`bind-skill-${badge.testid}-${s.name}`}
                        >
                          <Chip.Label>{t(badge.labelKey)}</Chip.Label>
                        </Chip>
                      ) : null;
                    })()}
                  </div>
                  <p className="text-xs text-muted">{truncate(skillText(s, t).description, 120)}</p>
                </div>
              </div>
            );
          })}
        </div>

        <Separator />
        <p className="num text-xs text-muted">
          {t("skill.bindCount", { bound: draft.skillIds.length, total: skills.length })}
        </p>
      </Card.Content>
    </Card>
  );
}

// ---------------------------------------------------------------------------
// Feishu tab
// ---------------------------------------------------------------------------

/** The two values of trigger mode; order is UI order: the default first. */
const TRIGGER_MODES: FeishuTriggerMode[] = ["mention_only", "topic_and_mention"];

/** The two values for sending another message; order is UI order: **the default (redirect) first**, same as above. */
const FOLLOW_UP_MODES: FollowUpMode[] = ["redirect", "queue"];
const PROGRESS_DETAILS: ProgressDetail[] = ["full", "brief"];

/**
 * Trigger mode picker: when this Feishu app's bot speaks in groups.
 *
 * # Why it saves immediately, rather than following the "Save" at the bottom of the page
 *
 * Because it changes a field on the **Feishu app**, not on the agent. The page's save button calls
 * `api.updateAgent(agent.id, draft)`; putting trigger mode into draft would submit it with the
 * agent,
 * and the server's agent endpoint does not know this field at all, so the change would be silently
 * dropped (zero errors).
 *
 * The cost is managing saving / rollback on failure here; the gain is **correct meaning**: one app
 * has one trigger mode,
 * which is a different thing from "which apps this agent binds".
 *
 * # Failure must roll back, and say why
 *
 * Optimistic update (change local state first, then send the request) makes the picker move as
 * soon as it is clicked, but when the request fails it must be turned back;
 * without rollback the UI shows "no @ needed" while the server is still on the default, users think
 * it is set up,
 * and then wonder why the bot does not reply automatically.
 */
function TriggerModeSelect({
  app,
  onSaved,
}: {
  app: FeishuApp;
  onSaved: () => void;
}) {
  const { t } = usePrefs();
  // Local state makes the picker respond at once; after the server returns, the reload triggered by
  // onSaved aligns it as a fallback.
  const [mode, setMode] = useState<FeishuTriggerMode>(app.triggerMode);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // When outside data changes (after reload, or another app), follow it, otherwise this keeps
  // showing the old value.
  useEffect(() => {
    setMode(app.triggerMode);
  }, [app.id, app.triggerMode]);

  async function change(next: FeishuTriggerMode) {
    if (next === mode) return;
    const previous = mode;
    setMode(next);
    setSaving(true);
    setError(null);
    try {
      await api.setFeishuTriggerMode(app.id, next);
      onSaved();
    } catch (err) {
      // Turn it back so the UI does not falsely claim it took effect. The server's text is a
      // Chinese sentence that can be shown directly.
      setMode(previous);
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="flex flex-col gap-1.5">
      <Select
        className="max-w-sm"
        selectedKey={mode}
        isDisabled={saving}
        onSelectionChange={(k) => void change(String(k) as FeishuTriggerMode)}
      >
        <Label>{t("feishu.triggerTitle")}</Label>
        <Select.Trigger data-testid={`feishu-trigger-${app.appId}`}>
          <Select.Value />
          <Select.Indicator />
        </Select.Trigger>
        <Select.Popover>
          <ListBox>
            {TRIGGER_MODES.map((m) => (
              <ListBox.Item key={m} id={m} textValue={t(`enum.feishuTrigger.${m}`)}>
                {t(`enum.feishuTrigger.${m}`)}
              </ListBox.Item>
            ))}
          </ListBox>
        </Select.Popover>
      </Select>
      {/* Behavior note for the selected option. The option name alone may still leave "do other groups need @" unclear. */}
      <span className="text-xs text-muted" data-testid={`feishu-trigger-hint-${app.appId}`}>
        {t(`feishu.triggerHint.${mode}`)}
      </span>
      {error ? <span className="text-xs text-danger">{error}</span> : null}
      <GroupScopeNotice app={app} mode={mode} onRefreshed={onSaved} />
      <BotScopeNotice app={app} mode={mode} onRefreshed={onSaved} />
    </div>
  );
}

/**
 * Warning for when "auto-reply to new topics" is selected but Feishu has not granted the
 * permission.
 *
 * # Why this block is required
 *
 * Triggering without @ **depends entirely on Feishu's im:message.group_msg permission**: without
 * it, Feishu never
 * pushes un-@'d group messages to us. The events never reach us, so the inbound table has not a
 * single row, and the logs
 * have nothing to find either: **a silent failure with zero errors**.
 *
 * It really happened in production: a user set "auto-reply to new topics", posted in a topic group,
 * the bot said nothing,
 * and in the UI this app was a green ready (readiness does not check this permission, because the
 * default really does not need it).
 * The backend had computed groupMessageScopeGranted long before, but no UI read it anywhere.
 *
 * # Why it hangs right below the picker, rather than inside the readiness card
 *
 * This is a **conditionally required** permission: needed only when no-@ is selected. The hint must
 * sit right next to "the choice
 * just made", so users connect the two; inside the readiness card it would become a generic
 * diagnostic unrelated to the current action,
 * and people using only the default would also see a warning they should not care about.
 *
 * # Nothing is shown after authorization
 *
 * Once the permission is in place the whole block disappears, with no leftover like "authorized
 * ✓": the normal state should not take visual space,
 * and no warning itself means "it is fine". The check uses whether the URL from the backend is
 * empty, rather than building the domain in the frontend
 * (Feishu in China and international Lark are two separate open platforms; picking the wrong domain
 * lands users on a site where the app does not exist).
 */
function GroupScopeNotice({
  app,
  mode,
  onRefreshed,
}: {
  app: FeishuApp;
  mode: FeishuTriggerMode;
  onRefreshed: () => void;
}) {
  const { t } = usePrefs();
  const [checking, setChecking] = useState(false);
  const r = app.readiness;

  // All three conditions are required:
  //   no-@ selected  : the default does not need this permission; reporting it is noise
  //   inspected      : asserting "missing permission" without inspection is a baseless accusation
  //   really not authorized
  if (mode !== "topic_and_mention" || !r || r.groupMessageScopeGranted) return null;

  return (
    <Issue
      tone="warning"
      icon="gravity-ui:triangle-exclamation"
      title={t("feishu.groupScopeMissing")}
      hint={t("feishu.groupScopeMissingHint")}
      items={r.groupMessageScope ? [r.groupMessageScope] : undefined}
      testId={`feishu-group-scope-notice-${app.appId}`}
      action={
        <div className="flex flex-wrap items-center gap-2 pt-0.5">
          {r.groupMessageAuthURL ? (
            <Button
              variant="outline"
              size="sm"
              // Use an a for the external link rather than onPress, so users can middle-click / open
              // in a new tab.
              // HeroUI's Button supports polymorphism via a render prop.
              onPress={() => window.open(r.groupMessageAuthURL, "_blank", "noopener,noreferrer")}
              data-testid={`feishu-group-scope-grant-${app.appId}`}
            >
              {t("feishu.grantOnOpenPlatform")}
              <Icon icon="gravity-ui:arrow-up-right-from-square" className="size-3.5" />
            </Button>
          ) : null}
          {/* Coming back from Feishu needs a clear "I am done": inspection results have a 45-second cache,
              and this button makes the server skip the cache and ask again. */}
          <Button
            variant="outline"
            size="sm"
            isPending={checking}
            onPress={async () => {
              setChecking(true);
              try {
                await api.refreshFeishuReadiness(app.id);
                onRefreshed();
              } finally {
                setChecking(false);
              }
            }}
            data-testid={`feishu-group-scope-recheck-${app.appId}`}
          >
            {t("feishu.recheck")}
          </Button>
        </div>
      }
    />
  );
}

/**
 * Warning for when "auto-reply to new topics" is selected and Feishu granted the group message
 * permission, but **not the "receive bot messages" level**.
 *
 * # Why it is a separate block from GroupScopeNotice, rather than merged into one
 *
 * Feishu splits "receive group messages" into two levels, and `im:message.group_msg` **only covers
 * messages sent by real people**:
 *
 *   im:message.group_msg                   user messages only, no bots
 *   im:message.group_msg.include_bot:read  users + other bots' messages
 *
 * The two **have the same fix (apply on the open platform) but completely different symptoms**:
 * missing the former means "nobody in the group can trigger it",
 * missing the latter means "real people get replies, bots do not". The latter is especially hard to
 * debug: in most groups
 * it behaves completely normally, and only groups like alert groups where "no real person talks"
 * expose it.
 *
 * Merging them has a real cost: in one production incident, this app's
 * `groupMessageScopeGranted` **was true**, everything looked normal in the UI, while the alert
 * group got
 * not a single event in 20 hours. With only one boolean, the UI could never say the sentence that
 * actually helps.
 *
 * # Why not merge it into the readiness card
 *
 * Same as GroupScopeNotice: this is a **conditionally required** permission, needed only when no-@
 * is selected. The reason is
 * word for word the same as "why it hangs right below the picker"; see the comment on that
 * component above.
 */
function BotScopeNotice({
  app,
  mode,
  onRefreshed,
}: {
  app: FeishuApp;
  mode: FeishuTriggerMode;
  onRefreshed: () => void;
}) {
  const { t } = usePrefs();
  const [checking, setChecking] = useState(false);
  const r = app.readiness;

  // All four conditions are required; the first three are the same as GroupScopeNotice.
  //
  // ⚠️ The fourth is `r.groupMessageScopeGranted`: **do not report this one while the level above
  // is not granted yet**.
  // Showing both at once would make users think they must apply twice, and not know which to do
  // first; in fact checking
  // "group messages" once applies for both levels together (see server/feishu/onboarding.go).
  // This one only appears after the one above disappears, so users only ever face one to-do.
  if (mode !== "topic_and_mention" || !r || !r.groupMessageScopeGranted) return null;
  if (r.botMessageScopeGranted) return null;

  return (
    <Issue
      tone="warning"
      icon="gravity-ui:triangle-exclamation"
      title={t("feishu.botScopeMissing")}
      hint={t("feishu.botScopeMissingHint")}
      items={r.botMessageScope ? [r.botMessageScope] : undefined}
      testId={`feishu-bot-scope-notice-${app.appId}`}
      action={
        <div className="flex flex-wrap items-center gap-2 pt-0.5">
          {r.botMessageAuthURL ? (
            <Button
              variant="outline"
              size="sm"
              onPress={() => window.open(r.botMessageAuthURL, "_blank", "noopener,noreferrer")}
              data-testid={`feishu-bot-scope-grant-${app.appId}`}
            >
              {t("feishu.grantOnOpenPlatform")}
              <Icon icon="gravity-ui:arrow-up-right-from-square" className="size-3.5" />
            </Button>
          ) : null}
          <Button
            variant="outline"
            size="sm"
            isPending={checking}
            onPress={async () => {
              setChecking(true);
              try {
                await api.refreshFeishuReadiness(app.id);
                onRefreshed();
              } finally {
                setChecking(false);
              }
            }}
            data-testid={`feishu-bot-scope-recheck-${app.appId}`}
          >
            {t("feishu.recheck")}
          </Button>
        </div>
      }
    />
  );
}

/**
 * Feishu tab: bind Feishu apps to this agent.
 *
 * # ⚠️ One agent binding **multiple** apps is allowed; do not get the direction backwards
 *
 * Someone once asked "why can't I bind multiple Feishu bots". **The feature was always there**:
 * `draft.feishuAppIds` is an array,
 * `toggle` below **appends** (`[...draft.feishuAppIds, id]`), each app has its own Switch,
 * and the switch is only disabled when **another** agent holds that app (see `locked`). Tested:
 * one agent binding two apps at once, PATCH returns 200, two rows are stored, and both apps'
 * `boundAgentIds` point to it.
 *
 * So why does it look like "only one can be bound"? Because **this page only lists apps already
 * connected in the current workspace**,
 * and that workspace often has only one connected (Feishu apps are workspace-level resources). To
 * bind a second one you must first go to
 * `/w/:key/feishu` and connect another; so the "Manage Feishu apps" entry at the top of this page is
 * not decoration.
 *
 * # One app can only be bound to one agent
 *
 * The reverse direction is what is forbidden: the server rejects taking over (409,
 * `core.ensureFeishuAppsFree`),
 * so the UI must make this clear **in advance**, rather than erroring only after the user clicks
 * save.
 * Apps held by someone else: switch disabled + say directly who holds it.
 *
 * # Why add a "bound / unbound" note
 *
 * The status badge on the right of each row is **the app's own health** (connected / pending /
 * connection error),
 * a different thing from "is this agent bound to it". Earlier there was only that one badge, so a
 * healthy app showed
 * a green "connected" on this page for every agent; users took it to mean the current agent was
 * connected,
 * while the switch was actually off. Two agents' pages thus looked like they were connected to the
 * same app.
 */
/**
 * Picker for "sending another message": redirect / queue.
 *
 * # Why the "cost" must be written next to the options
 *
 * Both option names are just one word, yet their consequences are far apart: redirect **kills the
 * running turn**
 * (even a tool halfway through stops), while queue waits until it finishes. From the words
 * "redirect / queue" alone,
 * users cannot know the former is destructive. So the selected option is always followed by a note,
 * the same handling as
 * TriggerModeSelect (whose option names also cannot explain the behavior).
 *
 * # The key difference from TriggerModeSelect: this one goes through the draft, not saved
 * immediately
 *
 * That one is an **app-level** setting, calling the API as soon as it changes (so it manages
 * optimistic update and rollback itself);
 * this one is a field of the agent, saved with the whole form. So here we only setDraft,
 * and saving and dirty checks are handled by the outer layer; **do not add its own save logic
 * here**,
 * which would fight the outer save button (users would see "saved" while the save button is still
 * lit).
 */
function FollowUpModeField({
  draft,
  setDraft,
}: {
  draft: AgentDraft;
  setDraft: (d: AgentDraft) => void;
}) {
  const { t } = usePrefs();
  // For old agents this field is an empty string, and **must be normalized**; otherwise the dropdown
  // selects nothing,
  // looking "never configured", while it is actually running on the default (redirect).
  const mode = followUpOrDefault(draft.followUp);

  return (
    <div className="flex flex-col gap-1.5">
      <Select
        className="max-w-sm"
        selectedKey={mode}
        onSelectionChange={(k) =>
          setDraft({ ...draft, followUp: String(k) as FollowUpMode })
        }
      >
        <Label>{t("agent.followUpTitle")}</Label>
        <Select.Trigger data-testid="agent-followup">
          <Select.Value />
          <Select.Indicator />
        </Select.Trigger>
        <Select.Popover>
          <ListBox>
            {FOLLOW_UP_MODES.map((m) => (
              <ListBox.Item key={m} id={m} textValue={t(`enum.followUp.${m}`)}>
                {t(`enum.followUp.${m}`)}
              </ListBox.Item>
            ))}
          </ListBox>
        </Select.Popover>
      </Select>
      {/* Consequence of the selected option. Redirect is destructive, which the option name does not show. */}
      <span className="text-xs text-muted" data-testid="agent-followup-hint">
        {t(`agent.followUpHint.${mode}`)}
      </span>
    </div>
  );
}

/**
 * Picker for "show detailed work content": show / do not show.
 *
 * The shape is exactly the same as FollowUpModeField (goes through the draft, a note under the
 * selected option),
 * and both reasons hold here word for word; **do not add its own save logic here**.
 *
 * # Why a two-option dropdown rather than a switch
 *
 * In data it is a string enum rather than a boolean: the default is "show", while a boolean
 * defaults to
 * false, exactly the opposite; old agents and old frontends would silently turn off the chain of
 * thought.
 * See ProgressDetail in types/domain.ts and the backend's domain.ProgressDetail.
 * Since the underlying type is an enum, the UI uses a dropdown shaped like followUp's, so the two
 * settings read the same way.
 */
function ProgressDetailField({
  draft,
  setDraft,
}: {
  draft: AgentDraft;
  setDraft: (d: AgentDraft) => void;
}) {
  const { t } = usePrefs();
  // Same as followUp: old agents have an empty string, which must be normalized, otherwise the
  // dropdown selects nothing.
  const detail = progressDetailOrDefault(draft.progressDetail);

  return (
    <div className="flex flex-col gap-1.5">
      <Select
        className="max-w-sm"
        selectedKey={detail}
        onSelectionChange={(k) =>
          setDraft({ ...draft, progressDetail: String(k) as ProgressDetail })
        }
      >
        <Label>{t("agent.progressDetailTitle")}</Label>
        <Select.Trigger data-testid="agent-progress-detail">
          <Select.Value />
          <Select.Indicator />
        </Select.Trigger>
        <Select.Popover>
          <ListBox>
            {PROGRESS_DETAILS.map((d) => (
              <ListBox.Item
                key={d}
                id={d}
                textValue={t(`enum.progressDetail.${d}`)}
              >
                {t(`enum.progressDetail.${d}`)}
              </ListBox.Item>
            ))}
          </ListBox>
        </Select.Popover>
      </Select>
      {/* Once off, the group **gets no chain-of-thought messages**, which the option name does not show. */}
      <span
        className="text-xs text-muted"
        data-testid="agent-progress-detail-hint"
      >
        {t(`agent.progressDetailHint.${detail}`)}
      </span>
    </div>
  );
}

function FeishuTab({
  draft,
  setDraft,
  apps,
  agents,
  selfId,
  onAppChanged,
}: {
  draft: AgentDraft;
  setDraft: (d: AgentDraft) => void;
  apps: FeishuApp[];
  agents: Agent[];
  selfId: string;
  /** After the trigger mode is saved, refetch data so the apps on the page align with the server. */
  onAppChanged: () => void;
}) {
  const { t } = usePrefs();
  const wp = useWorkspacePath();

  /** Find the **other** agent holding this app; undefined if nobody holds it. */
  function holderOf(appId: string): Agent | undefined {
    return agents.find((a) => a.id !== selfId && a.feishuAppIds.includes(appId));
  }

  /**
   * Apps bound by **another** agent are **not shown at all** on this page.
   *
   * Earlier they were listed as usual with the switch disabled and a sentence like "bound by agent
   * "X", unbind it there
   * first". That sentence gave the reader of this page nothing to act on: they can do nothing on
   * this page, yet are forced to know
   * another agent exists and its name. In the user's words, "very strange": being told about B on
   * A's page.
   *
   * ⚠️ **Hiding is only a UI matter; the race still exists**: amad runs as multiple instances in
   * production, and two people can at the same time
   * bind the same app on their own agents, neither seeing the other's draft at that moment. The real
   * guard is the 409 the server returns on save
   * (`core.Service.ensureFeishuAppsFree`, with the mock side's counterpart
   * `assertFeishuAppsFree`); that **must not be removed**; one less hint here does not mean it can
   * retire.
   *
   * ⚠️ Apps bound to this agent itself are not filtered out (`holderOf` excludes selfId), so "the
   * list is empty"
   * equals "this agent has no apps bound, and cannot bind any now either".
   */
  const bindable = apps.filter((app) => holderOf(app.id) === undefined);

  function toggle(id: string) {
    const has = draft.feishuAppIds.includes(id);
    setDraft({
      ...draft,
      feishuAppIds: has
        ? draft.feishuAppIds.filter((x) => x !== id)
        : [...draft.feishuAppIds, id],
    });
  }

  if (apps.length === 0) {
    return (
      <Empty
        icon="gravity-ui:comment"
        title={t("feishu.noneInWorkspace")}
        description={t("feishu.noneInWorkspaceDesc")}
        action={
          <Link to={wp("/feishu")} className="no-underline">
            <Button variant="outline">{t("feishu.goToApps")}</Button>
          </Link>
        }
      />
    );
  }

  return (
    <Card>
      <Card.Content className="flex flex-col gap-5 pt-6">
        <SectionTitle
          title={t("feishu.bindTitle")}
          description={t("feishu.bindDesc")}
          actions={
            <Link to={wp("/feishu")} className="no-underline">
              <Button variant="ghost" size="sm">
                {t("feishu.manageApps")}
              </Button>
            </Link>
          }
        />

        {/*
          "Sending another message" is an **agent-level** setting, not configured per app; so it is
          **above** the binding list,
          separated from it by a Separator. Putting it in each row would make people think different
          Feishu apps
          can have different settings, while the backend only knows the single value on the agent.
        */}
        <FollowUpModeField draft={draft} setDraft={setDraft} />
        {/* Also an agent-level setting, so likewise placed above the binding list. */}
        <ProgressDetailField draft={draft} setDraft={setDraft} />
        <Separator />

        <div className="flex flex-col gap-3" data-testid="feishu-binding-list">
          {bindable.length === 0 ? (
            /*
              The workspace has apps, but **all are bound by other agents**, so this page lists none.

              This sentence must be written: after filtering an empty list remains, and the user's
              first reaction is "broken" or
              "the workspace has not connected Feishu", and the latter is exactly **another** empty
              state (feishu.noneInWorkspace,
              returned earlier in this component when apps.length === 0). Two kinds of "nothing" look
              the same
              with completely different causes; not telling them apart disguises a normal state as a
              fault.

              **Deliberately does not name the holder**: that is exactly what this change removes. What
              we give is a sentence that lets them move on
              themselves: go to the Feishu page to see bindings, or connect another app.
            */
            <Empty
              icon="gravity-ui:lock"
              title={t("feishu.allTakenTitle")}
              description={t("feishu.allTakenDesc", { count: apps.length })}
              action={
                <Link to={wp("/feishu")} className="no-underline">
                  <Button variant="outline">{t("feishu.manageApps")}</Button>
                </Link>
              }
            />
          ) : null}
          {bindable.map((app) => {
            const bound = draft.feishuAppIds.includes(app.id);
            return (
              <div
                key={app.id}
                className={[
                  "flex flex-col gap-3 r-inset px-3 py-3",
                  bound ? "bg-surface-secondary" : "",
                ].join(" ")}
                data-testid={`feishu-binding-row-${app.appId}`}
              >
                <div className="flex items-center gap-3">
                  <div className="flex min-w-0 flex-1 flex-col gap-1">
                    <div className="flex items-center gap-2">
                      <span className="text-sm font-medium">{app.name}</span>
                      {/* Binding state. **A different thing from the health badge below**; see this component's comment. */}
                      <Chip
                        size="sm"
                        color={bound ? "success" : "default"}
                        variant="soft"
                        data-testid={`feishu-bindstate-${app.appId}`}
                      >
                        <Chip.Label>
                          {bound ? t("feishu.stateBound") : t("feishu.stateUnbound")}
                        </Chip.Label>
                      </Chip>
                      {/* All three states are shown as they are: earlier error was folded into "pending" here,
                          so an app that could not connect only looked "not verified yet" on this page, hiding the fault. */}
                      <Chip size="sm" color={FEISHU_STATUS_COLOR[app.status]} variant="soft">
                        <Chip.Label>{t(`enum.feishuStatus.${app.status}`)}</Chip.Label>
                      </Chip>
                    </div>
                    <span className="font-mono text-xs text-muted">{app.appId}</span>
                  </div>
                  {/* Switch at the end of the row, per the design convention */}
                  <Switch
                    isSelected={bound}
                    onChange={() => toggle(app.id)}
                    aria-label={t("feishu.bindLabel", { name: app.name })}
                    data-testid={`bind-feishu-${app.appId}`}
                  >
                    <Switch.Content>
                      <Switch.Control>
                        <Switch.Thumb />
                      </Switch.Control>
                    </Switch.Content>
                  </Switch>
                </div>
                {/*
                  The trigger mode only appears when **bound**: an app with no agent bound gets no
                  answer to messages anyway,
                  so asking the user to configure "when to answer" first is putting the cart before
                  the horse.

                  ⚠️ bound reads the draft (local), so it appears right after turning the switch on,
                  before clicking save,
                  while the binding is not stored yet. This is deliberate: the two controls are in the
                  same row,
                  and making the picker wait for a save before showing up would look stuck. The picker
                  itself stores immediately,
                  and it and the binding use separate endpoints, independent of each other.
                */}
                {bound ? <TriggerModeSelect app={app} onSaved={onAppChanged} /> : null}
              </div>
            );
          })}
        </div>
      </Card.Content>
    </Card>
  );
}

// ---------------------------------------------------------------------------
// Run history tab
// ---------------------------------------------------------------------------

function RunsTab({ agentId, runs }: { agentId: string; runs: Run[] }) {
  const { t, language } = usePrefs();
  const wp = useWorkspacePath();

  if (runs.length === 0) {
    return (
      <Empty
        icon="gravity-ui:list-ul"
        title={t("run.agentNoRuns")}
        description={t("run.agentNoRunsDesc")}
      />
    );
  }

  return (
    <div className="flex flex-col gap-3">
      <div className="flex items-center justify-between">
        <p className="num text-sm text-muted">{t("run.recentRuns", { count: runs.length })}</p>
        <Link to={wp(`/runs?agent=${agentId}`)} className="no-underline">
          <Button variant="ghost" size="sm">
            {t("run.viewInHistory")}
          </Button>
        </Link>
      </div>
      {runs.map((r) => (
        <Link
          key={r.id}
          to={wp(`/runs/${r.id}`)}
          className="no-underline"
          data-testid={`agent-run-${r.id}`}
        >
          <div className="flex items-center gap-3 r-row border border-border px-4 py-3 hover:bg-surface-secondary">
            <RunStatusChip status={r.status} hasError={Boolean(r.errorSummary)} />
            <span className="min-w-0 flex-1 truncate text-sm">{r.title}</span>
            <span className="num shrink-0 text-xs text-muted">
              {formatDuration(r.durationMs)}
            </span>
            <span className="shrink-0 text-xs text-muted">{formatRelative(r.startedAt, language)}</span>
            <Icon icon="gravity-ui:chevron-right" className="size-4 shrink-0 text-muted" />
          </div>
        </Link>
      ))}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Page
// ---------------------------------------------------------------------------

export function AgentDetailPage() {
  const { t } = usePrefs();
  const { agentId } = useParams<{ agentId: string }>();
  const { workspace } = useWorkspace();
  const wp = useWorkspacePath();
  const navigate = useNavigate();
  const [params, setParams] = useSearchParams();
  const tab = (params.get("tab") ?? "config") as TabId;

  const [draft, setDraft] = useState<AgentDraft | null>(null);
  const [saving, setSaving] = useState(false);
  const [savedAt, setSavedAt] = useState<number | null>(null);
  const [deleting, setDeleting] = useState(false);
  // saveError is the reason a save failed, shown directly to the user.
  //
  // Taking over a Feishu app makes the server return 409, and earlier there was no catch here at
  // all: the error was swallowed by finally,
  // and the user saw "the save button spun for a moment and nothing happened", while the binding
  // was not actually saved.
  const [saveError, setSaveError] = useState<string | null>(null);

  /*
   * Fetch the agent and its four surrounding lists at once. It used to be a bare await: once
   * getAgent rejected
   * (only not_found becomes null; 403 is a reject) agent stayed undefined forever,
   * and the page stayed on "loading" forever. See the file header of lib/useAsyncData.ts.
   *
   * agent being null still means "this agent does not exist", and "failed to load" has its own
   * display.
   */
  const fetch = useCallback(async () => {
    if (!agentId) {
      return { agent: null, skills: [], apps: [], runs: [], agents: [] };
    }
    const a = await api.getAgent(agentId);
    if (!a) {
      return { agent: null, skills: [], apps: [], runs: [], agents: [] };
    }
    const [s, f, r, ag] = await Promise.all([
      api.listSkills(workspace.id),
      api.listFeishuApps(workspace.id),
      api.listRuns(workspace.id, { agentId }),
      api.listAgents(workspace.id),
    ]);
    return { agent: a, skills: s, apps: f, runs: r, agents: ag };
  }, [agentId, workspace.id]);
  const {
    data,
    error: loadError,
    isLoading,
    reload: load,
  } = useAsyncData(fetch, t("common.loadFailedUnknown"));

  // ⚠️ Rollback needs write (member and above), **not** manage; guarding it with canManage would
  // hide the rollback button from members, while the server allows it (that route declares
  // ActionWrite).
  // See the comment on useCanWrite in lib/useCanManage.ts.
  const canWrite = useCanWrite(workspace.id) ?? false;

  const agent = data?.agent ?? null;
  const skills = data?.skills ?? [];
  const apps = data?.apps ?? [];
  const runs = data?.runs ?? [];
  const agents = data?.agents ?? [];

  /*
   * The reference used by "Open in Ark console". **Fetched on its own, not merged into the
   * Promise.all above**:
   * it is an optional secondary entry; in the main fetch, its failure would turn **the whole detail
   * page** into
   * a load failure; trading whole-page availability for a link is a bad deal.
   *
   * On failure treat it as absent (stay null → do not render the link), deliberately with no error
   * and no retry.
   */
  const [arkConsole, setArkConsole] = useState<ArkConsoleRef | null>(null);
  useEffect(() => {
    if (!agentId) return;
    let alive = true;
    void api
      .getAgentArkConsole(agentId)
      .then((ref) => {
        if (alive) setArkConsole(ref);
      })
      .catch(() => {
        // A secondary feature; degrade quietly to "no link".
        if (alive) setArkConsole(null);
      });
    return () => {
      alive = false;
    };
  }, [agentId]);

  /*
   * The draft is an editable local copy, so data.agent cannot be used directly: reloading while
   * the user is halfway through
   * would wipe their input. When a new agent is fetched, reset the draft by id: a different agent
   * needs a different draft,
   * and after saving the same agent it must also follow the server's new values.
   */
  useEffect(() => {
    setDraft(agent ? toDraft(agent) : null);
  }, [agent]);

  /**
   * Trigger counter for sync polling: +1 on every successful save.
   *
   * 0 means "not saved yet during this page visit", and then there is no polling; opening the
   * detail page of an existing agent
   * should not send a query right away.
   */
  const [syncToken, setSyncToken] = useState(0);
  const sync = useAgentSyncStatus(agent?.id ?? null, syncToken);

  async function save() {
    if (!agent || !draft) return;
    setSaving(true);
    setSaveError(null);
    try {
      await api.updateAgent(agent.id, draft);
      await load();
      setSavedAt(Date.now());
      setTimeout(() => setSavedAt(null), 2400);
      // Restart sync polling. **Use an incrementing counter, not agentId**: saving the same agent
      // twice in a row
      // keeps the id unchanged, the effect would not re-run, and the result of the second save would
      // never arrive.
      setSyncToken((n) => n + 1);
    } catch (err) {
      // The server's error text is a Chinese sentence that can be shown directly (see the convention
      // in server/core).
      setSaveError(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  }

  if (isLoading || (agent && !draft)) {
    return (
      <PageBody>
        <LoadingBlock label={t("agent.loading")} />
      </PageBody>
    );
  }

  if (loadError) {
    return (
      <PageBody>
        <AsyncErrorBlock error={loadError} onRetry={load} />
      </PageBody>
    );
  }

  if (agent === null) {
    return (
      <PageBody>
        <Empty
          icon="gravity-ui:circle-info"
          title={t("agent.notFound")}
          description={t("agent.notFoundDesc")}
          action={
            <Link to={wp("/agents")} className="no-underline">
              <Button variant="outline">{t("agent.backToList")}</Button>
            </Link>
          }
        />
      </PageBody>
    );
  }

  const d = draft!;
  const dirty = isDirty(agent, d);
  const spec = modelSpec(d.model.id);

  return (
    <PageBody>
      <PageHeader
        title={agent.name}
        description={agent.description || t("common.notFilled")}
        above={
          <Link
            to={wp("/agents")}
            className="flex w-fit items-center gap-1 text-sm text-muted no-underline hover:text-foreground"
          >
            <Icon icon="gravity-ui:arrow-left" className="size-4" />
            Agents
          </Link>
        }
        actions={
          <>
            {/* The reason a save failed. Placed here (next to the save button) rather than at the top of the page:
                the user's attention is right here, and a banner at the top scrolls out of view on a long form. */}
            {saveError ? (
              <span
                className="flex items-center gap-1 text-sm text-danger"
                data-testid="agent-save-error"
              >
                <Icon icon="gravity-ui:circle-xmark" className="size-4" />
                {saveError}
              </span>
            ) : null}
            {savedAt ? (
              <span className="flex items-center gap-1 text-sm text-success">
                <Icon icon="gravity-ui:circle-check" className="size-4" />
                {t("common.saved")}
              </span>
            ) : null}
            {/*
             * The result of syncing to Ark. **Nothing is shown for success or in-progress** (the
             * user explicitly asked for
             * "silent success"); it only appears on failure, so this whole block only renders when
             * failed.
             *
             * Same position as saveError (next to the save button): the user's attention is right
             * here,
             * and a banner at the top scrolls out of view on a long form.
             */}
            {sync.status === "failed" ? (
              <span
                className="flex items-center gap-1 text-sm text-danger"
                data-testid="agent-sync-error"
                data-sync-stage={sync.failureStage || undefined}
              >
                <Icon icon="gravity-ui:triangle-exclamation" className="size-4 shrink-0" />
                {/* The reason from the server is a Chinese sentence that can be shown directly; when none is given fall back to the generic one. */}
                <span>{sync.failureReason || t("agent.syncFailed")}</span>
                <Button
                  variant="ghost"
                  size="sm"
                  isPending={sync.isRetrying}
                  onPress={() => void sync.retry()}
                  data-testid="agent-sync-retry"
                >
                  {sync.isRetrying ? t("agent.syncRetrying") : t("agent.syncRetry")}
                </Button>
              </span>
            ) : null}
            <ArkConsoleLink
              kind="agents"
              refData={arkConsole}
              labelKey="arkConsole.openAgent"
              testId="agent-ark-console"
            />
            <Tooltip>
              <Tooltip.Trigger>
                <Button
                  variant="ghost"
                  isIconOnly
                  aria-label={t("agent.deleteAction")}
                  onPress={() => setDeleting(true)}
                >
                  <Icon icon="gravity-ui:trash-bin" className="size-4" />
                </Button>
              </Tooltip.Trigger>
              <Tooltip.Content>{t("agent.deleteAction")}</Tooltip.Content>
            </Tooltip>
            {/*
              "Try it" uses outline: this row already has "Save" as the solid primary CTA,
              and a screen keeps only one solid button; secondary actions must be visually demoted.
            */}
            <Button
              variant="outline"
              onPress={() => navigate(wp(`/chat?agent=${agent.id}`))}
              data-testid="agent-try"
            >
              {t("agent.tryIt")}
            </Button>
            <Button onPress={save} isDisabled={!dirty} isPending={saving} data-testid="agent-save">
              {t("common.save")}
            </Button>
          </>
        }
      />

      {/* Summary bar: lay out key config at the top, visible without switching tabs */}
      <div className="flex flex-wrap items-center gap-x-6 gap-y-2 r-inset bg-surface-secondary px-4 py-3 text-xs">
        <span className="flex items-center gap-1.5">
          <span className="text-muted">{t("agent.model")}</span>
          <span className="font-medium">{spec.label}</span>
        </span>
        <span className="flex items-center gap-1.5">
          <span className="text-muted">{t("agent.summaryReasoning")}</span>
          <span>{t(`enum.effort.${d.model.reasoning_effort ?? "high"}`)}</span>
        </span>
        <span className="flex items-center gap-1.5">
          <span className="text-muted">{t("overview.metricSkills")}</span>
          <span className="num" data-testid="summary-skills">
            {d.skillIds.length}
          </span>
        </span>
        <span className="flex items-center gap-1.5">
          <span className="text-muted">{t("agent.summaryFeishu")}</span>
          <span className="num" data-testid="summary-feishu">
            {d.feishuAppIds.length}
          </span>
        </span>
        <span className="flex items-center gap-1.5">
          <span className="text-muted">{t("agent.summaryVersion")}</span>
          <span className="num">v{agent.version}</span>
        </span>
      </div>

      {/* Tab navigation. State lives in the URL so other places can link straight in */}
      <div className="flex items-center gap-1 border-b border-border">
        {TABS.map((item) => (
          <button
            key={item.id}
            type="button"
            onClick={() => {
              params.set("tab", item.id);
              setParams(params, { replace: true });
            }}
            data-testid={`tab-${item.id}`}
            className={[
              "relative px-3 py-2 text-sm cursor-[var(--cursor-interactive)]",
              tab === item.id
                ? "font-medium text-foreground after:absolute after:inset-x-0 after:-bottom-px after:h-0.5 after:bg-accent"
                : "text-muted hover:text-foreground",
            ].join(" ")}
          >
            {t(item.labelKey)}
            {item.id === "runs" && runs.length > 0 ? (
              <span className="ml-1.5 num text-xs opacity-70">{runs.length}</span>
            ) : null}
          </button>
        ))}
      </div>

      {tab === "revisions" ? (
        <RevisionPanel
          kind="agent"
          entityId={agent.id}
          canWrite={canWrite}
          // After a rollback the entity changed, so reload the whole page; **updating only the draft
          // is not enough**:
          // the version badge, updatedBy and so on are in data, and changing only the draft would
          // leave them at old values.
          onRolledBack={load}
        />
      ) : null}
      {tab === "config" ? (
        <ConfigTab
          agentId={agent.id}
          draft={d}
          setDraft={setDraft}
          createdByEmail={agent.createdBy}
        />
      ) : null}
      {tab === "skills" ? <SkillsTab draft={d} setDraft={setDraft} skills={skills} /> : null}
      {tab === "feishu" ? (
        <FeishuTab
          draft={d}
          setDraft={setDraft}
          apps={apps}
          agents={agents}
          selfId={agent.id}
          onAppChanged={() => void load()}
        />
      ) : null}
      {tab === "runs" ? <RunsTab agentId={agent.id} runs={runs} /> : null}

      <ConfirmModal
        isOpen={deleting}
        onOpenChange={setDeleting}
        title={t("agent.deleteTitle", { name: agent.name })}
        description={t("agent.deleteDesc")}
        onConfirm={async () => {
          await api.deleteAgent(agent.id);
          navigate(wp("/agents"));
        }}
      />
    </PageBody>
  );
}

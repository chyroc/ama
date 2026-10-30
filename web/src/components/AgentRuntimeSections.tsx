/**
 * "What this agent can get at runtime": authorization checkboxes + custom environment variables.
 *
 * The two blocks are in one file, **shared** by the create page and the detail page, because they are the same kind of problem, and their shape must
 * match exactly: if what you check at creation looks different from what you change later, users think they are two different things
 * (the same reason NewAgentCapabilities and ConfigTab share the `tool-<id>` testid).
 *
 * # No authorization is selected by default
 *
 * ⚠️ **Do not use that `defaultsApplied` ref gate here** (the one AgentsPage uses for built-in skills).
 * That is for "one-time default checks": because "the user unchecked both" and "the defaults have not been applied yet" look
 * exactly the same in the draft (both empty arrays), a ref is needed to remember "already applied".
 *
 * Here **the default is nothing selected**, so there is no default to apply at all; adding that mechanism would instead introduce the
 * bug it is meant to prevent.
 *
 * # Wording discipline for sensitive env
 *
 * ⚠️⚠️ **It must honestly say "the agent can still read it"**. The sensitive flag only does two things: keeps it out of the system prompt,
 * and keeps it from sitting in plaintext on the page. It is **not** access control: a single `env` in the container gets the plaintext.
 * Calling it "secure" would make users put in credentials that really should not be given to the agent, which is worse than not having the flag.
 * See AgentEnv.secret in types/domain.ts.
 */
import { useEffect, useState } from "react";
import { Icon } from "@iconify/react";
import {
  Button,
  Checkbox,
  Chip,
  Description,
  Disclosure,
  Input,
  Label,
  ListBox,
  Radio,
  RadioGroup,
  Select,
  Switch,
  TextField,
} from "@heroui/react";

import { Link } from "react-router-dom";

import { Notice, SectionTitle } from "@/components/Page";
import { useAuth } from "@/app/AuthGate";
import { preferencesPath } from "@/app/preferencesLink";
import { useCanManage } from "@/lib/useCanManage";
import { api } from "@/data/api";
import { useIntegrationAuthorize } from "@/lib/useIntegrationAuthorize";
import { usePrefs, useT } from "@/prefs/PrefsContext";
import {
  ALIASABLE_ENV_SOURCES,
  ENV_LIMITS,
  isReservedEnvKey,
  isValidEnvKey,
  maskEnvValue,
  type AgentDraft,
  type AgentEnv,
  type AgentEnvAlias,
  type Host,
  type HostSource,
  type IntegrationKind,
  type IntegrationState,
  type LarkUserApp,
  type WorkspaceHost,
} from "@/types/domain";

/**
 * Authorization checkbox area.
 *
 * providers being null means still loading: then **the whole block is not rendered** (no skeleton):
 * this block is optional config, and a flickering gray bar for it would make the form look unfinished loading.
 * Load failure also passes null, for the same reason: failing to fetch the authorization list should not block the main path "create agent".
 *
 * # When canManage is false it renders as a **read-only list**, not hidden, and not disabled checkboxes
 *
 * This is where two existing rules meet, and neither may be broken:
 *
 *   "Controls hidden by default need derived    An unusable main path traps people: hiding the whole block would make member
 *    visibility"                                 think the capability does not exist, so they neither know whether the agent
 *                                                got credentials nor whom to ask to enable it.
 *   "A button that can never be clicked is      A row of grayed-out checkboxes is pure noise: it expresses "you have no
 *    more confusing than no button"              permission" as a broken control.
 *
 * The third way is **a different presentation**: no controls, a conclusion instead. One line "injected: Feishu user authorization, AMA"
 * plus one line "changing requires admin permission, please contact a workspace admin": they know the current state, why they cannot change it,
 * and whom to ask, and there is not a single misleading control on the page.
 *
 * ⚠️ **The read-only state only lists "what is really injected", not "which sources are available"**: the latter is for people who will change it
 * (they need to choose), while member cannot choose, and listing them would only make them ask "then how do I check it".
 *
 * # onProviderRefreshed: after in-place authorization succeeds, the ability to refresh one item must be in the parent's hands
 *
 * After one authorization succeeds, it must go from "disabled" to "checkable". There are only two ways, and **only one is right**:
 *
 *   ✅ call back to the parent, which replaces that one entry in providers (that state is the only source of truth)
 *   ❌ the child refetches the whole list itself
 *
 * The latter is wrong because this component lives inside **a form with an unsaved draft**. The child fetching the list itself adds a
 * loading state out of sync with the parent; and on the create page path the parent would also rerun an effect when the list changes:
 * the draft is in the same tree, and **the name/prompt the user is editing would be wiped**, with zero errors.
 *
 * It also works without it (the in-place authorize button is still offered), only the item will not become checkable automatically after authorizing.
 */
export function AgentIntegrationsSection({
  draft,
  setDraft,
  providers,
  canManage = true,
  onProviderRefreshed,
  workspaceId,
  workspaceKey,
}: {
  draft: AgentDraft;
  setDraft: (d: AgentDraft) => void;
  providers: IntegrationState[] | null;
  /**
   * Which workspace this agent belongs to. **Decides "which Feishu app to authorize when starting authorization".**
   *
   * ⚠️ Without it, authorization lands on the **account-level default** app, while at runtime the agent picks by its workspace:
   * when the two differ the agent still cannot use lark-cli, but the error says "user authorization for Feishu app cli_xxx
   * has not been done yet", which does not point to "you authorized the wrong one". So both call sites must pass it.
   *
   * It is optional so other reusers are not forced to pass it, but **both of these pages have it** (useWorkspace()).
   */
  workspaceId?: string;
  /** The key of the same workspace, only used to build the "go connect a Feishu app" link. */
  workspaceKey?: string;
  /**
   * Whether the current user can change this field (owner / admin). **Three states**:
   * `null` = not known yet → like `providers === null`, **the whole block is not rendered**
   * (otherwise admins would see a flash of "you have no permission" every time they open the page; see useCanManage).
   *
   * ⚠️ **Default true**: this component is reused on two pages, and when not passed it should fall back to the existing behavior
   * (editable) rather than quietly becoming read-only: the real block is on the server; the UI just must not mislead.
   * With a default of false / null, any call site that forgets to pass it would leave even admins unable to change it,
   * and that failure would look like "permissions were judged wrong".
   */
  canManage?: boolean | null;
  /** The latest state after one authorization succeeds. The parent uses it to **replace only this entry**, not refetch the whole list. */
  onProviderRefreshed?: (next: IntegrationState) => void;
}) {
  const { t } = usePrefs();

  /*
   * Only lists authorization sources that are **really usable now**: they inject credentials into the container (`injectable`) **and**
   * their status is ready (`status === "ok"`).
   *
   * # Each of the two conditions guards against something different; neither can be missing
   *
   *   `injectable`  diagnostic-only items inject no credentials into the container. Their status may be ok, but checking them
   *                 **does nothing at all**: that switch never takes effect, and nothing in the UI could explain it.
   *   `status ok`   when the server is not configured (missing) or the user has not authorized (error), checking it cannot inject either.
   *
   * # ⚠️ Why it is hidden here but not on the "authorization management" page
   *
   * The two pages have different readers, and the asymmetry is deliberate:
   *
   *   Authorization management page (global settings)  the reader is **managing the platform**: they need to know which sources exist, which are not configured,
   *                                                    and whom to ask. So missing ones **must be shown** with a FixHint
   *                                                    (see "register even with missing config" in newIntegrationRegistry).
   *   Agent form (here)                                the reader is **configuring one agent**: they can do nothing about "please have an admin set
   *                                                    auth.managedKeySecret". A disabled checkbox + a sentence they neither understand
   *                                                    nor can act on is pure noise.
   *
   * So it is not shown here; telling people "this capability exists" is the job of the authorization management page.
   *
   * What really must not be silent is **runtime**: when an agent explicitly checked a source but cannot get credentials, it must error,
   * not quietly skip injection (see core/sessionenv.go). One fewer row in the UI ≠ a failure swallowed.
   */
  /*
   * ⚠️ **The check is "can the user make it usable themselves", not "is it usable right now".**
   *
   * This used to be `injectable && status === "ok"`, hiding everything not ok. That was right for the first four sources
   * (their non-ok states all need **an admin** to configure the server, and a disabled box is pure noise),
   * but **Feishu user authorization broke that assumption**: its `error` state means "you have not authorized yet",
   * which is **something the user can fix with one click**.
   *
   * Hiding it creates a dead end (`hiding-a-control-by-default-needs-derived-visibility`):
   *
   *   not visible in the form → you do not know this capability exists
   *   → to make it appear you must first authorize on the authorization management page
   *   → but you do not know you need to, because it is invisible
   *
   * So there are two tiers, **both derived from backend fields** (not hard-coded by id):
   *
   *   status === "ok"   → checkable
   *   canAuthorize      → shown but not checkable, with **an in-place "start authorization" button**
   *   the rest (missing) → still hidden (needs an admin to configure; the user can do nothing; the original decision stays)
   *
   * ⚠️ The second tier once had only a pointer line "go to 'Global settings → Authorization management' and click once".
   * That was **a half fix** of this dead end: it turned the dead end into a round trip, and that round trip made the user leave a
   * **form with an unsaved draft**; the real feedback was "I should not need to go to another page to authorize".
   * Now authorization is done right in this row, and that pointer line was removed with it: keeping it would give two contradicting
   * instructions at once ("click here" and "go elsewhere").
   */
  const listed = (providers ?? []).filter(
    (p) => p.injectable === true && (p.status === "ok" || p.canAuthorize === true),
  );

  /*
   * Read-only view (non-admins).
   *
   * ⚠️ **Checked before the `listed.length === 0` early return**, because the two ask different questions:
   * `listed` is "which sources can be checked", while the read-only state answers "what has this agent been injected with".
   * After the early return, an agent that already has credentials injected but currently has no checkable source
   * would **disappear as a whole**: member could no longer see "it is holding a credential",
   * which is exactly what they most need to know.
   *
   * Names are looked up from providers; if not found, fall back to the id (the provider went offline, but this agent
   * still keeps that check). **It must not be hidden just because the name cannot be found**: that would erase an injection still
   * in effect from the UI.
   */
  // Do not render the whole block before "can I manage" is known, same as the providers === null handling.
  // Otherwise admins would see a flash of "you have no permission" every time they open the page (see useCanManage).
  if (canManage === null) return null;

  if (!canManage) {
    const injected = draft.integrationIds.map((id) => {
      const p = (providers ?? []).find((x) => x.id === id);
      return { id, name: p?.name ?? id };
    });
    return (
      <div className="flex flex-col gap-4">
        <SectionTitle
          title={t("agent.integrationsSection")}
          description={t("agent.integrationsSectionDesc")}
        />
        <div className="flex flex-col gap-2" data-testid="agent-integrations-readonly">
          <span className="text-xs text-muted">{t("agent.integrationsReadOnly")}</span>
          {injected.length > 0 ? (
            <div className="flex flex-wrap items-center gap-2">
              {injected.map((p) => (
                <Chip key={p.id} size="sm" variant="soft" data-testid={`injected-integration-${p.id}`}>
                  <Chip.Label>{p.name}</Chip.Label>
                </Chip>
              ))}
            </div>
          ) : (
            <span className="text-sm text-muted" data-testid="injected-integration-none">
              {t("agent.integrationsReadOnlyNone")}
            </span>
          )}
          {/*
            ⚠️ This line must stay; it is the whole difference between "read-only" and "gone":
            without it, member only sees a list they cannot change, without knowing why or whom to ask.
          */}
          <p className="text-xs text-muted" data-testid="agent-integrations-need-admin">
            {t("agent.integrationsReadOnlyHint")}
          </p>
        </div>
      </div>
    );
  }

  if (listed.length === 0) return null;

  function toggle(id: string) {
    const has = draft.integrationIds.includes(id);
    setDraft({
      ...draft,
      integrationIds: has
        ? draft.integrationIds.filter((x) => x !== id)
        : [...draft.integrationIds, id],
    });
  }

  return (
    <div className="flex flex-col gap-4">
      <SectionTitle
        title={t("agent.integrationsSection")}
        description={t("agent.integrationsSectionDesc")}
      />
      <div className="flex flex-col gap-4" data-testid="agent-integrations">
        {/* ⚠️ **Split into two groups by identity kind, the same shape as the global authorization management page** (which uses
            `groupByKind`). The two must match: after users learn the split "personal authorization /
            service account" on the authorization management page, seeing another way of organizing it here would make them think it is something else.

            ⚠️ The order within a group **is kept as is** (filter keeps order); grouping only cuts once by `kind`:
            the discipline "the frontend does not hard-code provider order" does not give way; see
            the comment on groupByKind in IntegrationsTab.tsx.

            ⚠️ The two group descriptions are **not rendered again here** (the authorization management page has them): in this form the reader is
            configuring one agent and wants "which ones to check"; two paragraphs of explanation would double the length of the list.
            The headings are enough to separate the two kinds, and "why split" was already said on the authorization management page. */}
        {bindGroups(listed).map((g) => (
          <div key={g.kind} className="flex flex-col gap-2">
            <p className="text-xs font-medium text-muted">{t(BIND_GROUP_TITLE_KEY[g.kind])}</p>
            {g.providers.map((p) => (
              <IntegrationBindRow
                key={p.id}
                provider={p}
                isSelected={draft.integrationIds.includes(p.id)}
                onToggle={() => toggle(p.id)}
                onRefreshed={onProviderRefreshed}
                workspaceId={workspaceId}
                workspaceKey={workspaceKey}
              />
            ))}
          </div>
        ))}
      </div>
      {/*
        After checking, the cost must be stated. This is not decoration: the injected credentials belong to **the workspace owner**, and the agent can
        read them; the person checking may not be the owner, and may not realize this.
      */}
      {draft.integrationIds.length > 0 && (
        <p className="text-xs text-warning" data-testid="agent-integration-risk">
          {t("agent.integrationRisk")}
        </p>
      )}
    </div>
  );
}

/**
 * Executor (cloud container / host) cards + the two-level host source picker.
 *
 * Each executor comes with one line of explanation (cloud "pauses automatically when idle" vs host "workspace kept persistently"),
 * the information users really need to decide, which a switch or dropdown cannot hold. The shape follows HeroUI's official card-style
 * Radio (Radio.Content clickable as a whole row + data-[selected] border).
 *
 * # Empty means cloud container, and there must be a clear selected state
 *
 * An empty string in draft.executorType means "Docker container" (all existing agents have this value).
 * But RAC RadioGroup treats an empty string as "nothing selected", so inside the component two non-empty values "docker"/"host" are used,
 * converted to and from the draft at the boundary; do not let the draft contain values like "docker" that the backend does not accept.
 *
 * # The two host sources
 *
 *   workspace (workspace assignment): candidates come from `GET /api/workspaces/{id}/hosts`: machines already bound
 *     to the workspace, including ones bound by others. Workspace admins can assign any of them; non-admins can only choose ones they
 *     own (the others are still listed but disabled, so users do not choose one and only hit 403 on save).
 *   personal (personal direct connection): candidates come from `GET /api/hosts`: all machines registered by the caller,
 *     no workspace binding needed. Only agent.CreatedBy can choose this tier; when others (including workspace admins)
 *     open the edit page, the whole block is read-only with a hint naming the creator.
 *
 * ⚠️ **Offline machines may be selected**: the daemon being offline is temporary (laptop lid closed, network down),
 * and disabling the picker for it would force users to keep reselecting; but a yellow warning is required: running tools right now would fail.
 */
export function AgentExecutorSection({
  draft,
  setDraft,
  workspaceId,
  workspaceKey,
  /**
   * Creator email of a saved agent. Not passed on the create page (the creator is the current user).
   * Personal direct-connection machines can only be changed by the creator, so the read-only state depends on it; when missing (existing agents) it is treated as workspace assignment,
   * not locked.
   */
  createdByEmail,
}: {
  draft: AgentDraft;
  setDraft: (d: AgentDraft) => void;
  workspaceId: string;
  /** Workspace key, only used to build the "go bind a host in workspace settings" link. */
  workspaceKey?: string;
  createdByEmail?: string;
}) {
  const t = useT();
  const { user } = useAuth();
  // null = role not known yet; same rule as AgentIntegrationsSection.
  const canManage = useCanManage(workspaceId);
  const isHost = draft.executorType === "host";

  const me = (user?.email ?? "").trim().toLowerCase();
  const creator = (createdByEmail ?? "").trim().toLowerCase();
  // On the create page (createdByEmail missing) you are the creator; on the edit page compare emails (the backend historically has
  // uppercase emails, so always compare lowercase; see that lesson in authscope).
  const isCreator = !createdByEmail || creator === me;

  // Personal direct-connection lock: host + personal + clearly not the creator. Existing agents with createdBy missing are not locked
  // (treated as workspace assignment; edit permission follows the workspace role).
  const personalLocked =
    isHost && draft.hostSource === "personal" && !!createdByEmail && !isCreator;
  // Non-creators may never "switch into" personal direct connection, even while on workspace assignment. Otherwise they could switch from workspace
  // to personal, choose a machine, and only hit the backend 403 on save (the backend's ensureExecutorAllowed only lets
  // the creator change personal direct connection). The workspace assignment group stays editable by workspace role.
  const personalForbidden = isHost && !!createdByEmail && !isCreator;

  const selectExecutor = (next: "docker" | "host") => {
    if (personalLocked) return;
    if (next === "host") {
      // Entering host: when the source is missing, default to "workspace assignment" (matching the backend's empty-value normalization), but do not preselect
      // a specific machine: force users to choose explicitly in the two groups, so it never quietly attaches to a machine they did not notice.
      setDraft({
        ...draft,
        executorType: "host",
        hostSource: draft.hostSource || "workspace",
      });
    } else {
      // ⚠️ **Switching back to the cloud container must clear hostId/source**: leaving a dangling id would carry a possibly stale choice
      // the next time it switches back to host, and in the cloud container state they are meaningless data.
      setDraft({ ...draft, executorType: "", hostId: "", hostSource: "" });
    }
  };

  return (
    <div className="flex flex-col gap-4" data-testid="agent-executor">
      <SectionTitle
        title={t("agent.executorSection")}
        description={t("agent.executorSectionDesc")}
      />
      {personalLocked ? (
        <Notice
          tone="warning"
          icon="gravity-ui:lock"
          title={t("agent.executorPersonalLockedTitle")}
          hint={t("agent.executorPersonalLockedHint", { email: createdByEmail ?? "" })}
          data-testid="agent-executor-personal-locked"
        />
      ) : null}
      {/* ⚠️ `**:data-[slot=radio]:mt-0` must stay: the vertical radio-group layout gives each
          .radio an mt-4, and with the grid's own gap-3 stacked on top the first item also sinks and row spacing becomes uneven. */}
      <RadioGroup
        isDisabled={personalLocked}
        className="grid w-full gap-3 sm:grid-cols-2 **:data-[slot=radio]:mt-0"
        aria-label={t("agent.executorSection")}
        value={isHost ? "host" : "docker"}
        onChange={(v) => selectExecutor(v === "host" ? "host" : "docker")}
      >
        <ExecutorCard
          value="docker"
          icon="gravity-ui:cloud"
          title={t("agent.executorCloud")}
          desc={t("agent.executorCloudDesc")}
          testId="executor-docker"
        />
        <ExecutorCard
          value="host"
          icon="gravity-ui:cpu"
          title={t("agent.executorHost")}
          desc={t("agent.executorHostDesc")}
          testId="executor-host"
        />
      </RadioGroup>

      {isHost ? (
        <HostPicker
          draft={draft}
          setDraft={setDraft}
          workspaceId={workspaceId}
          workspaceKey={workspaceKey}
          canManage={canManage}
          me={me}
          locked={personalLocked}
          personalForbidden={personalForbidden}
          creatorEmail={createdByEmail ?? ""}
        />
      ) : null}
    </div>
  );
}

/** A clickable executor card: Radio control + icon + title/subtitle, the whole row is the click area. */
function ExecutorCard({
  value,
  icon,
  title,
  desc,
  testId,
  isDisabled,
}: {
  value: string;
  icon: string;
  title: string;
  desc: string;
  testId: string;
  /** Only disables this card (the others in the group stay selectable). Placed on the outer Radio (RAC's RadioField, which takes
   *  AriaRadioProps): it feeds the disabled state to the inner Radio.Content through context;
   *  Radio.Content (RadioButton) itself has no isDisabled prop. */
  isDisabled?: boolean;
}) {
  return (
    <Radio value={value} data-testid={testId} isDisabled={isDisabled}>
      <Radio.Content
        className={[
          "group flex w-full items-start gap-3 rounded-xl border border-border bg-surface px-4 py-3",
          "cursor-[var(--cursor-interactive)] transition-colors",
          "data-[hovered=true]:bg-surface-secondary",
          "data-[selected=true]:border-accent data-[selected=true]:bg-surface-secondary",
          "data-[disabled=true]:cursor-not-allowed data-[disabled=true]:opacity-60",
        ].join(" ")}
      >
        <Radio.Control className="mt-0.5 shrink-0">
          <Radio.Indicator />
        </Radio.Control>
        <Icon icon={icon} className="mt-0.5 size-4 shrink-0 text-muted" />
        <span className="flex min-w-0 flex-col gap-0.5">
          <span className="text-sm font-medium">{title}</span>
          <Description className="text-xs">{desc}</Description>
        </span>
      </Radio.Content>
    </Radio>
  );
}

/**
 * Two-level host picker: choose the source first (workspace assignment / personal direct connection), then pick a machine among its candidates.
 *
 * Candidates, empty states, and dangling ids (unbound/deleted) are all separated by source, because the "way out" of the two groups differs:
 * workspace group empty → bind one in workspace settings or ask an admin to bind; personal group empty → register your own machine in global preferences.
 */
function HostPicker({
  draft,
  setDraft,
  workspaceId,
  workspaceKey,
  canManage,
  me,
  locked,
  personalForbidden,
  creatorEmail,
}: {
  draft: AgentDraft;
  setDraft: (d: AgentDraft) => void;
  workspaceId: string;
  workspaceKey?: string;
  /** Workspace admin flag (null = loading). Decides whether other people's machines in the workspace group can be chosen. */
  canManage: boolean | null;
  /** Lowercase email of the current user, used to mark "my machine" and to check the machine owner. */
  me: string;
  /** Personal direct-connection read-only state (non-creator on personal). */
  locked: boolean;
  /** Non-creator: forbidden to switch into personal direct connection (must be blocked even while on workspace, or they only hit 403 after choosing and saving). */
  personalForbidden: boolean;
  /** Agent creator email, only used for the disabled hint text. */
  creatorEmail: string;
}) {
  const t = useT();
  // Fetch both groups of candidates in parallel: the workspace group is readable by viewer+ (member needs to see candidates on the edit page),
  // and the personal group is account-level ActionNone, returning only the user's own machines.
  const [wsHosts, setWsHosts] = useState<WorkspaceHost[] | null>(null);
  const [myHosts, setMyHosts] = useState<Host[] | null>(null);
  const [wsFailed, setWsFailed] = useState(false);

  useEffect(() => {
    let alive = true;
    setWsHosts(null);
    setMyHosts(null);
    setWsFailed(false);
    void (async () => {
      try {
        const [bound, mine] = await Promise.all([
          api.listWorkspaceHosts(workspaceId),
          api.listHosts(),
        ]);
        if (!alive) return;
        setWsHosts(bound);
        setMyHosts(mine);
      } catch {
        // When either read fails, fall back to its own empty list + an explicit failure hint for the workspace group (same as the old behavior),
        // instead of silently becoming an empty dropdown that makes people think the feature is broken.
        if (alive) {
          setWsHosts([]);
          setMyHosts([]);
          setWsFailed(true);
        }
      }
    })();
    return () => {
      alive = false;
    };
  }, [workspaceId]);

  const source: HostSource = draft.hostSource || "workspace";

  const selectSource = (next: HostSource) => {
    if (locked || next === source) return;
    // Double safety: non-creators cannot enter personal direct connection, even if the disabled state did not stop it (e.g. scripts/old flags).
    if (next === "personal" && personalForbidden) return;
    // Switching groups must clear the chosen machine: the two machines belong to different authorization relations, and switching with the old id would
    // mix up "chose a machine from group A, saved under group B's rules".
    setDraft({ ...draft, hostSource: next, hostId: "" });
  };

  const loading = wsHosts === null || myHosts === null;

  return (
    <div className="flex flex-col gap-3" data-testid="agent-executor-host-picker">
      {/* Choose one of two sources: two small cards (the lightweight form of a detached radio group).
          When locked the whole group is read-only; otherwise only the personal card is disabled: when a non-creator is on workspace,
          disabling the whole group would also lock workspace assignment, which they can still edit by their workspace role. */}
      <RadioGroup
        isDisabled={locked}
        className="grid w-full gap-3 sm:grid-cols-2 **:data-[slot=radio]:mt-0"
        aria-label={t("agent.executorSourceLabel")}
        value={source}
        onChange={(v) => selectSource(v === "personal" ? "personal" : "workspace")}
      >
        <ExecutorCard
          value="workspace"
          icon="gravity-ui:persons"
          title={t("agent.executorSourceWorkspace")}
          desc={t("agent.executorSourceWorkspaceDesc")}
          testId="executor-source-workspace"
        />
        <ExecutorCard
          value="personal"
          icon="gravity-ui:person"
          title={t("agent.executorSourcePersonal")}
          desc={t("agent.executorSourcePersonalDesc")}
          testId="executor-source-personal"
          isDisabled={!locked && personalForbidden}
        />
      </RadioGroup>
      {!locked && personalForbidden ? (
        <p className="text-xs text-muted" data-testid="agent-executor-personal-forbidden">
          {t("agent.executorPersonalForbiddenHint", { email: creatorEmail })}
        </p>
      ) : null}

      {loading ? (
        <p className="text-xs text-muted">{t("common.loading")}</p>
      ) : source === "workspace" ? (
        <HostSelect
          // The testId has the same name as the old workspace picker (agent-executor-host-*): workspace assignment is
          // the default source, so the old testid is kept and existing e2e and recording scripts need no change.
          testId="agent-executor-host"
          hosts={wsHosts.map((h) => ({ ...h, _owner: (h.ownerEmail ?? "").trim().toLowerCase() }))}
          draft={draft}
          locked={locked}
          failed={wsFailed}
          emptyTitle={t("agent.executorNoneTitle")}
          emptyHint={t("agent.executorNoneHint")}
          pickLabel={t("agent.executorHostPick")}
          placeholder={t("agent.executorHostPlaceholder")}
          me={me}
          // Workspace group: admins can choose all; non-admins can only choose machines they own.
          canPick={(h) => canManage === true || h._owner === me}
          notAssignableHint={t("agent.executorNotAssignable")}
          onSelect={(id) => setDraft({ ...draft, hostSource: "workspace", hostId: id })}
          emptyTestId="agent-executor-no-hosts"
          action={
            workspaceKey ? (
              <Link
                to={`/w/${workspaceKey}/settings?tab=hosts`}
                className="text-sm text-accent underline-offset-2 hover:underline"
              >
                {t("agent.executorNoneLink")}
              </Link>
            ) : undefined
          }
        />
      ) : (
        <HostSelect
          testId="agent-executor-personal-select"
          hosts={myHosts.map((h) => ({ ...h, _owner: me }))}
          draft={draft}
          locked={locked}
          failed={false}
          emptyTitle={t("agent.executorPersonalNoneTitle")}
          emptyHint={t("agent.executorPersonalNoneHint")}
          pickLabel={t("agent.executorPersonalPick")}
          placeholder={t("agent.executorHostPlaceholder")}
          me={me}
          // The personal group lists only the user's own machines, all selectable.
          canPick={() => true}
          notAssignableHint=""
          onSelect={(id) => setDraft({ ...draft, hostSource: "personal", hostId: id })}
          emptyTestId="agent-executor-personal-no-hosts"
          action={
            <Link
              to={preferencesPath({ workspaceKey, tab: "hosts" })}
              className="text-sm text-accent underline-offset-2 hover:underline"
            >
              {t("agent.executorPersonalGotoRegister")}
            </Link>
          }
          extraNote={t("agent.executorPersonalTerminalNote")}
        />
      )}
    </div>
  );
}

/**
 * A Select of host candidates + empty/dangling/offline/not-assignable hints.
 * Shared by the workspace group and the personal group; the only differences are the candidates, the empty-state text, and canPick.
 */
function HostSelect({
  hosts,
  draft,
  locked,
  failed,
  emptyTitle,
  emptyHint,
  pickLabel,
  placeholder,
  me,
  canPick,
  notAssignableHint,
  onSelect,
  action,
  extraNote,
  testId,
  emptyTestId,
}: {
  hosts: Array<Host & { _owner: string }>;
  draft: AgentDraft;
  locked: boolean;
  failed: boolean;
  emptyTitle: string;
  emptyHint?: string;
  pickLabel: string;
  placeholder: string;
  me: string;
  canPick: (h: Host & { _owner: string }) => boolean;
  notAssignableHint: string;
  onSelect: (id: string) => void;
  action?: React.ReactNode;
  extraNote?: string;
  testId: string;
  /** testid of the empty-state Notice (must differ between the workspace and personal groups, otherwise an empty database gives two identical ids on one page). */
  emptyTestId: string;
}) {
  const t = useT();
  const selected = hosts.find((h) => h.id === draft.hostId) ?? null;
  // In the read-only state this personal machine may not be in any candidate list (someone else's machine, not returned by GET /api/hosts):
  // that is not a "dangling" error but expected invisibility, shown with its own placeholder.
  const invisibleLocked = locked && !selected && draft.hostId !== "";
  // Host chosen but the id is empty / not found among candidates: show an inline error; this state must not be saved quietly.
  const missing = !locked && hosts !== null && (draft.hostId === "" || selected === null);

  if (invisibleLocked) {
    return (
      <Notice
        tone="warning"
        icon="gravity-ui:cpu"
        title={t("agent.executorPersonalInvisibleTitle")}
        hint={t("agent.executorPersonalInvisibleHint")}
        data-testid="agent-executor-host-invisible"
      />
    );
  }

  return (
    <div className="flex flex-col gap-2">
      {hosts.length === 0 ? (
        <Notice
          tone="warning"
          icon="gravity-ui:cpu"
          title={failed ? t("hosts.loadFailed") : emptyTitle}
          hint={failed ? undefined : emptyHint}
          action={action}
          data-testid={emptyTestId}
        />
      ) : (
        <Select
          isDisabled={locked}
          variant="secondary"
          isInvalid={missing}
          selectedKey={selected ? draft.hostId : null}
          onSelectionChange={(k) => {
            const id = String(k ?? "");
            const h = hosts.find((x) => x.id === id);
            if (h && !canPick(h)) return;
            onSelect(id);
          }}
          className="w-full sm:max-w-md"
          placeholder={placeholder}
          data-testid={testId}
        >
          <Label>{pickLabel}</Label>
          <Select.Trigger data-testid={`${testId}-trigger`}>
            <Select.Value>
              {({ defaultChildren, isPlaceholder, state }) => {
                const h = hosts.find((x) => x.id === state.selectedItems[0]?.key);
                if (isPlaceholder || !h) return defaultChildren;
                return (
                  <span className="flex min-w-0 items-center gap-1.5">
                    <span
                      className={`size-2 shrink-0 rounded-full ${h.online ? "bg-success" : "bg-muted"}`}
                    />
                    <span className="truncate">{h.name}</span>
                    <span className="truncate font-mono text-xs text-muted">
                      {h.os}/{h.arch}
                    </span>
                    {h._owner && h._owner !== me ? (
                      <span className="truncate text-xs text-muted">{h._owner.split("@")[0]}</span>
                    ) : (
                      <span className="truncate text-xs text-muted">{t("agent.executorMyHost")}</span>
                    )}
                  </span>
                );
              }}
            </Select.Value>
            <Select.Indicator />
          </Select.Trigger>
          <Select.Popover>
            <ListBox>
              {hosts.map((h) => {
                const allowed = canPick(h);
                return (
                  <ListBox.Item
                    key={h.id}
                    id={h.id}
                    isDisabled={!allowed}
                    textValue={`${h.name} ${h.os}/${h.arch} ${
                      h.online ? t("agent.executorOnline") : t("agent.executorOffline")
                    } ${h._owner === me ? t("agent.executorMyHost") : h._owner.split("@")[0]}`}
                  >
                    <span
                      className={`size-2 shrink-0 rounded-full ${h.online ? "bg-success" : "bg-muted"}`}
                      aria-hidden
                    />
                    <Label>{h.name}</Label>
                    <Description>
                      <span className="font-mono">
                        {h.os}/{h.arch}
                      </span>
                      {" · "}
                      {h._owner === me ? t("agent.executorMyHost") : h._owner.split("@")[0]}
                      {!allowed ? ` · ${notAssignableHint}` : ""}
                    </Description>
                    <Chip size="sm" variant="soft" color={h.online ? "success" : "default"}>
                      <Chip.Label>
                        {h.online ? t("agent.executorOnline") : t("agent.executorOffline")}
                      </Chip.Label>
                    </Chip>
                    <ListBox.ItemIndicator />
                  </ListBox.Item>
                );
              })}
            </ListBox>
          </Select.Popover>
        </Select>
      )}

      {/* Offline machines may be selected, but say clearly that tools would fail right now: a warning, not a block. */}
      {selected && !selected.online ? (
        <p className="text-xs text-warning" data-testid="agent-executor-host-offline">
          {t("agent.executorOfflineWarn")}
        </p>
      ) : null}

      {/* Dangling id (the machine was unbound/deleted, or an empty id was left): give an error instead of a silent blank. */}
      {missing && hosts.length > 0 ? (
        <p className="text-xs text-danger" role="alert" data-testid="agent-executor-host-missing">
          {t("agent.executorHostMissing")}
        </p>
      ) : null}

      {extraNote ? <p className="text-xs text-muted">{extraNote}</p> : null}
    </div>
  );
}
/**
 * Splits checkable authorization sources into two groups, "personal authorization" and "service account".
 *
 * ⚠️ **It is the same split as `groupByKind` on the authorization management page, and the two must stay consistent**
 * (the classification users learn on that page should not become another one here). The full argument is on
 * `groupByKind` in `pages/IntegrationsTab.tsx`; only the key points are noted here:
 *
 *   - `filter` keeps order within a group → still the backend registration order; "the frontend does not hard-code order" does not give way;
 *   - a missing `kind` is treated as `personal` → the conservative side (putting a credential representing a real person into
 *     the "shareable" group is the mistake in the unsafe direction);
 *   - empty groups are not rendered → a group with only a heading and nothing below is harder to read than no grouping.
 *
 * ⚠️ This function is deliberately **not shared** with the one over there: the return shapes are the same, but the group headings,
 * whether there are group descriptions, and whether it will later be cut again by checkability all differ; merging too early would force changes in one place
 * to drag the other along. The rule is "the **grouping basis** of the two must match", and that basis is
 * the `kind` from the backend, which is already shared.
 */
function bindGroups(
  providers: IntegrationState[],
): { kind: IntegrationKind; providers: IntegrationState[] }[] {
  return [
    { kind: "personal" as const, providers: providers.filter((p) => p.kind !== "service_account") },
    {
      kind: "service_account" as const,
      providers: providers.filter((p) => p.kind === "service_account"),
    },
  ].filter((g) => g.providers.length > 0);
}

/**
 * i18n keys of the group headings. **Explicit mapping**, for the same reason as elsewhere: building strings would break the key name union check of `t()`,
 * so a misspelled key would no longer be a compile error, only showing up on the page as the key name itself.
 */
const BIND_GROUP_TITLE_KEY: Record<
  IntegrationKind,
  "integrations.groupPersonalTitle" | "integrations.groupServiceTitle"
> = {
  personal: "integrations.groupPersonalTitle",
  service_account: "integrations.groupServiceTitle",
};

/**
 * One row in the authorization checkbox area: checkbox + name/status + **in-place authorization**.
 *
 * # Why an inline button instead of a Modal / Popover
 *
 * Stacking three design rules leaves only this option:
 *
 *   "In place, lightweight, no interruption"    This is a small block in a form, not the main flow. Opening a full-screen
 *                                              Modal for it would push away the whole form: exactly the "having to go to another
 *                                              page" users complained about; a popover is just a fancier way of doing the same thing.
 *   "The CTA goes on its own line below        So the button is not squeezed onto the title row, but follows this row's explanation.
 *    the description"
 *   "In progress needs feedback and no         Authorization waits for a real person to click agree in Feishu, possibly tens of seconds. The button keeps its place throughout,
 *    layout shift"                             only isPending and the text change; the size stays the same.
 *
 * A Popover has one more downside: while open it blocks all clicks outside (the dismiss layer), and right next to this row are
 * the form fields the user wants to keep editing.
 *
 * # One component instance per row, because the authorization state is **per row**
 *
 * The polling state (who we are waiting on to click agree, what the error is) belongs to one specific item. Putting it at the section level would need an extra
 * `Record<id, state>`, and that shape sooner or later produces "A's error shown on B's row".
 */
function IntegrationBindRow({
  provider,
  isSelected,
  onToggle,
  onRefreshed,
  workspaceId,
  workspaceKey,
}: {
  provider: IntegrationState;
  isSelected: boolean;
  onToggle: () => void;
  onRefreshed?: (next: IntegrationState) => void;
  workspaceId?: string;
  workspaceKey?: string;
}) {
  const { t } = usePrefs();
  const isReady = provider.status === "ok";

  /*
   * Which app this source authorizes **in this workspace**.
   *
   * # Why it must be shown
   *
   * The user's own words: "when authorizing I need to choose which Feishu app to authorize". They have several Feishu apps belonging to different
   * workspaces, and this row used to have only one "start authorization" button: clicking it authorized the **account-level default** one,
   * so they might authorize the wrong one, and the error after that (at runtime "user authorization for Feishu app cli_xxx
   * has not been done yet") **does not point to "you authorized the wrong one"**.
   *
   * Now authorization is pinned by workspace (workspaceId is passed to useIntegrationAuthorize), but **pinning alone is not enough**:
   * users cannot confirm what they cannot see. So "which one is about to be authorized" is written next to the button.
   *
   * # ⚠️ Pick by workspace; `current` must not be used
   *
   * `current` is the **account-level default** (the one shown on the authorization management page), exactly the wrong value.
   * And LarkUserApp only has workspaceName, no workspace id, so matching is not done in the frontend here;
   * instead we **ask the backend**: the app it resolves for the workspace is the one to authorize.
   * The check does not hard-code `lark-user` either: if no app list can be obtained (the other four sources return apps_unsupported)
   * the whole block is not shown.
   */
  const [apps, setApps] = useState<LarkUserApp[] | null>(null);
  useEffect(() => {
    let alive = true;
    void (async () => {
      try {
        // ⚠️⚠️ **workspaceId is required**, otherwise the backend computes `current` from the account-level default:
        // exactly the wrong value this change fixes, which would move the defect into "showing the target app" itself.
        const list = await api.listIntegrationApps(provider.id, workspaceId);
        if (alive) setApps(list);
      } catch {
        // ⚠️ Silently treated as "this source has no apps to choose". Three of the four sources always end up here
        // (the backend returns 400 + apps_unsupported); that is a **normal state**, not a fault:
        // showing it as an error would hang red text on three rows. Same as that part in IntegrationsTab.
        if (alive) setApps(null);
      }
    })();
    return () => {
      alive = false;
    };
    // ⚠️ workspaceId is in the dependencies: when it changes we must ask again "which app should this workspace use",
    // otherwise the same component instance would still show the old workspace's app after switching workspaces (a mismatch with zero errors).
  }, [provider.id, provider.checkedAt, workspaceId]);

  const auth = useIntegrationAuthorize(
    provider.id,
    provider.pollsAuthorization,
    // Authorization succeeded → hand the latest state to the parent. **Only this entry is replaced**, the whole list is not refetched:
    // this row lives in a form with an unsaved draft, and refetching the whole list would wipe the draft too.
    (next) => onRefreshed?.(next),
    {
      authorizeFailed: t("integrations.authorizeFailed"),
      timeout: t("integrations.authorizeTimeout"),
    },
    // ⚠️ This is where this fix lands: authorization picks the app by **the workspace this agent is in**.
    workspaceId,
  );

  // Both starting and polling count as busy, to avoid clicking into several parallel authorization flows.
  const isWorking = auth.isBusy || auth.isPolling;

  /*
   * Does this source have the concept of "apps"? If so, which one should this workspace use?
   *
   * `hasApps` is derived from **the list returned by the backend being non-empty**, not from `provider.id === "lark-user"`
   * (useIntegrationAuthorize.ts explicitly forbids the frontend from hard-coding a provider list;
   * if some other source supports choosing apps later, missing a change here would report no error, it would just not show).
   *
   * `target` is the one the backend resolves for the workspace (what current means when workspaceId is passed).
   * ⚠️ **It must not fall back to the account-level meaning of `apps.find(a => a.current)`**: that is exactly the wrong value.
   * No target while hasApps is true = **this workspace has not connected a Feishu app yet**,
   * and then clicking the authorize button is sure to give 422, so it must be folded away with a "go connect one" path offered.
   */
  const hasApps = (apps?.length ?? 0) > 0;
  const target = apps?.find((a) => a.current);
  const missingAppInWorkspace = hasApps && workspaceId !== undefined && !target;

  return (
    <div
      className={[
        "flex items-start gap-3 r-inset px-3 py-3",
        isSelected ? "bg-surface-secondary" : "",
      ].join(" ")}
    >
      <Checkbox
        isSelected={isSelected}
        // Disabled when not ready: checking it cannot inject anything, and making it checkable would be a fake switch.
        // ⚠️ The way to unlock it is now **right in this row** (the button below); no need to jump to another page any more.
        isDisabled={!isReady}
        onChange={onToggle}
        aria-label={t("agent.integrationBindLabel", { name: provider.name })}
        // ⚠️ The testid uses the `bind-integration-` prefix and **must not use `integration-`**:
        // e2e/11-integrations.spec.ts uses `integration-*` to select the cards on the authorization management page,
        // and a clash would turn those cases into strict mode conflicts.
        data-testid={`bind-integration-${provider.id}`}
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
          <span className="text-sm font-medium">{provider.name}</span>
          {/*
            Two tiers: ready → success; not authorized → **warning, not danger**.
            The user can fix it with one click; it is not an error, and danger would make them think something is broken.
          */}
          {isReady ? (
            <Chip size="sm" variant="soft" color="success">
              <Chip.Label>{t("agent.integrationReady")}</Chip.Label>
            </Chip>
          ) : (
            <Chip size="sm" variant="soft" color="warning">
              <Chip.Label>{t("agent.integrationNeedsAuth")}</Chip.Label>
            </Chip>
          )}
        </div>
        <p className="text-xs text-muted">{provider.description}</p>

        {/*
          ⚠️⚠️ **In-place authorization**: this part is where the requirement "no need to go to another page" lands.

          The button is given by `canAuthorize`, **not guessed from status**: when not integrated (missing) it is
          false; clicking such a source would only give a 422, and the user would have no way to know the server needs configuring.
          (Though missing is already hidden by the filter above; this is the second line of defense.)
        */}
        {/*
          ⚠️ **This workspace has not connected a Feishu app yet**: clicking the authorize button is sure to give 422 (the backend cannot resolve an app),
          so the button is folded away and instead it says "go connect one".
          This is the proper use of the rule "controls hidden by default need derived visibility": the check is not a preference,
          but **the main path really is unusable**, and the next step offered is one the user can carry out.

          ⚠️ Never fall back to the account-level default app: that is the cross-workspace mismatch an earlier fix removed
          (using workspace A's token with workspace B's app_id, to which lark-cli returns 99991668).
        */}
        {!isReady && provider.canAuthorize === true && missingAppInWorkspace ? (
          <p
            className="mt-1 text-xs text-warning"
            data-testid={`bind-integration-noapp-${provider.id}`}
          >
            {workspaceKey ? (
              <>
                {t("agent.integrationNoAppInWorkspace")}{" "}
                <Link to={`/w/${workspaceKey}/feishu`} className="underline">
                  {t("nav.feishu")}
                </Link>
              </>
            ) : (
              t("agent.integrationNoAppInWorkspace")
            )}
          </p>
        ) : null}

        {/*
          ⚠️ **Sources with needsCredential cannot take the "in-place authorization" path.**

          That button's shape is "click → the platform opens in a new tab → come back and it is ready automatically", while sources with hand-entered credentials
          need **a set of credentials pasted in**, and there is nowhere in this row to paste them. Forcing a button there
          would have users open the console, copy the credentials, come back, and find nowhere to fill them in: a dead end.

          So it falls into the pointer text below (the condition of that branch was loosened accordingly).
          ⚠️ It does not contradict the Feishu "in-place authorization": the check is always "can the user make it usable
          **within this row**"; Feishu can (one click), sources with hand-entered credentials cannot (they need a form).
        */}
        {!isReady &&
        provider.canAuthorize === true &&
        provider.needsCredential !== true &&
        !missingAppInWorkspace ? (
          <div className="mt-1 flex flex-col gap-1.5">
            {/*
              ⚠️⚠️ **"Which app is about to be authorized" must be written out.**
              The user's own words were "when authorizing I need to choose which Feishu app to authorize": they have several Feishu apps belonging to
              different workspaces, and this row used to have only one button, which authorized the **account-level default** one.
              The error after authorizing the wrong one (at runtime "user authorization for Feishu app cli_xxx has not been done yet")
              **does not point to "you authorized the wrong one"**, so the target must be visible before clicking.

              What is shown is the one the backend resolves for **this workspace**, not the account-level meaning of current.
            */}
            {target ? (
              <p
                className="text-xs text-muted"
                data-testid={`bind-integration-target-${provider.id}`}
              >
                {t("agent.integrationAuthorizeTarget", {
                  name: target.name,
                  appId: target.appId,
                })}
              </p>
            ) : null}
            <div>
              {/*
                The CTA is on its own line, `w-fit`: same as the Chip rule "inline elements should not stretch full width";
                a button filling the whole row would read as the main CTA, while this is just an action of a small block in a form.
              */}
              <Button
                size="sm"
                variant="outline"
                className="w-fit"
                isPending={isWorking}
                isDisabled={isWorking}
                onPress={() => void auth.start()}
                data-testid={`bind-integration-authorize-${provider.id}`}
              >
                {/*
                  ⚠️ Both the icon and the text **only change content, never add or remove nodes**: switching variants must not introduce layout
                  shift. Both texts are four Chinese characters, and the icon is size-4 in both states, so the width stays the same.

                  ⚠️⚠️ **`<Spinner>` is deliberately not used here**: its `.spinner--sm` is **unlayered
                  CSS**, which beats the `size-4` utility class (tested: it rendered at 22.4px, stretching the button from
                  102px to 106px, exactly what the skill rule "switching button variants must not introduce layout
                  shift" guards against). Using an icon from the same iconify set with `animate-spin` has no such
                  problem, and the animation can also be removed under motion-reduce.
                */}
                <Icon
                  icon={isWorking ? "gravity-ui:arrows-rotate-right" : "gravity-ui:key"}
                  className={[
                    "size-4",
                    isWorking ? "animate-spin motion-reduce:animate-none" : "",
                  ].join(" ")}
                />
                {auth.isPolling
                  ? t("agent.integrationAuthorizeWaiting")
                  : t("agent.integrationAuthorize")}
              </Button>
            </div>

            {/*
              The line shown while polling matters: the device code is in a new tab, and when the user comes back to this page they only see a
              spinning button. Without saying "it will update by itself once you click there", they would think it is stuck and refresh,
              and **refreshing loses the whole draft together with this polling**.
            */}
            {auth.hasOpened ? (
              <p
                className="text-xs text-muted"
                data-testid={`bind-integration-polling-${provider.id}`}
              >
                {auth.isPolling
                  ? t("agent.integrationAuthorizePolling")
                  : t("agent.integrationAuthorizeOpened")}
              </p>
            ) : null}

            {/*
              Failed/timed out/revoked. The backend's sentence is shown as is: it carries "what to do"
              (an expired code and a user refusal each have a different next step).
            */}
            {auth.error ? (
              <p
                className="text-xs text-danger"
                role="alert"
                data-testid={`bind-integration-error-${provider.id}`}
              >
                {auth.error}
              </p>
            ) : null}
          </div>
        ) : null}

        {/*
          Only the kind the user cannot authorize themselves (canAuthorize false) gets the pointer text.
          ⚠️ **Mutually exclusive** with the button above: both appearing at once would be two contradicting instructions
          ("click here" and "go elsewhere").
        */}
        {!isReady && (provider.canAuthorize !== true || provider.needsCredential === true) ? (
          <p className="text-xs text-warning" data-testid={`bind-integration-hint-${provider.id}`}>
            {t("agent.integrationNeedsAuthHint")}
          </p>
        ) : null}
      </div>
    </div>
  );
}

/**
 * Custom environment variable area.
 *
 * One per row, key/value editable, with a "sensitive" switch and a delete button at the end.
 */
export function AgentEnvSection({
  draft,
  setDraft,
}: {
  draft: AgentDraft;
  setDraft: (d: AgentDraft) => void;
}) {
  const { t } = usePrefs();
  /**
   * Which rows have their sensitive value in the "being changed" state.
   *
   * # Why this state is needed
   *
   * Sensitive values show as masks on the page. If the mask string went straight into the input box, a user saving once without touching it would store
   * **the mask itself** as the new value (that credential would be ruined, and you would only find out when the agent errors). So:
   *
   *   not being changed  → shows the mask, input box read-only
   *   being changed      → cleared, editable; the user must re-enter the whole value
   *
   * "Can only re-enter the whole value" is an inherent cost of this design (a mask sent back up cannot be told apart from "the user really wants to set the value to this string
   * of dots"); it is written here so later readers do not think "partial editing" was forgotten.
   */
  const [editingSecret, setEditingSecret] = useState<Set<number>>(new Set());

  const envs = draft.envs;

  function update(next: AgentEnv[]) {
    setDraft({ ...draft, envs: next });
  }

  function add() {
    update([...envs, { key: "", value: "", secret: false }]);
  }

  function remove(i: number) {
    update(envs.filter((_, idx) => idx !== i));
    // Indexes shift left as a whole, so the editing markers must shift with them, otherwise deleting one row would make **another row**
    // suddenly enter edit mode and clear its value: zero errors, just very odd.
    setEditingSecret((prev) => {
      const next = new Set<number>();
      prev.forEach((idx) => {
        if (idx < i) next.add(idx);
        else if (idx > i) next.add(idx - 1);
      });
      return next;
    });
  }

  function patch(i: number, fields: Partial<AgentEnv>) {
    update(envs.map((e, idx) => (idx === i ? { ...e, ...fields } : e)));
  }

  /** Computes the error hint per row. An empty string means the row is fine. */
  function errorFor(e: AgentEnv, i: number): string {
    const key = e.key.trim();
    if (key === "") return "";
    if (!isValidEnvKey(key)) return t("agent.envKeyInvalid");
    if (isReservedEnvKey(key)) return t("agent.envKeyReserved");
    // Duplicates: only reported on **the later one**, otherwise both rows turn red and the user does not know which to change.
    if (envs.some((other, idx) => idx < i && other.key.trim() === key)) {
      return t("agent.envKeyDuplicate");
    }
    if (e.value.length > ENV_LIMITS.maxValueBytes) return t("agent.envValueTooLong");
    return "";
  }

  return (
    <div className="flex flex-col gap-4">
      <SectionTitle title={t("agent.envSection")} description={t("agent.envSectionDesc")} />

      {envs.length > 0 && (
        <div className="flex flex-col gap-3" data-testid="agent-envs">
          {envs.map((e, i) => {
            const err = errorFor(e, i);
            const isEditing = editingSecret.has(i);
            const showMask = e.secret && !isEditing && e.value !== "";
            return (
              <div key={i} className="flex flex-col gap-1" data-testid={`env-row-${i}`}>
                <div className="flex flex-wrap items-end gap-2">
                  <TextField className="min-w-40 flex-1" isInvalid={err !== ""}>
                    <Label className="text-xs text-muted">{t("agent.envKey")}</Label>
                    <Input
                      value={e.key}
                      onChange={(ev) => patch(i, { key: ev.target.value })}
                      placeholder="MY_VAR"
                      className="font-mono"
                      data-testid={`env-key-${i}`}
                    />
                  </TextField>
                  <TextField className="min-w-40 flex-[2]">
                    <Label className="text-xs text-muted">{t("agent.envValue")}</Label>
                    <Input
                      // In the mask state it shows the masked string, read-only: letting the mask into an editable input box directly
                      // would make a user saving once without touching it store the mask itself as the new value.
                      value={showMask ? maskEnvValue(e.value) : e.value}
                      readOnly={showMask}
                      onChange={(ev) => patch(i, { value: ev.target.value })}
                      className="font-mono"
                      data-testid={`env-value-${i}`}
                    />
                  </TextField>
                  {showMask && (
                    <Button
                      size="sm"
                      variant="outline"
                      onPress={() => {
                        // Enter edit mode and **clear it**: the whole value must be re-entered; see editingSecret.
                        setEditingSecret((prev) => new Set(prev).add(i));
                        patch(i, { value: "" });
                      }}
                      data-testid={`env-replace-${i}`}
                    >
                      {t("agent.envReplace")}
                    </Button>
                  )}
                  <Switch
                    isSelected={e.secret}
                    onChange={(on) => {
                      patch(i, { secret: on });
                      // When switching from sensitive back to normal, leave edit mode, otherwise that row would stay "being changed" forever.
                      if (!on) {
                        setEditingSecret((prev) => {
                          const next = new Set(prev);
                          next.delete(i);
                          return next;
                        });
                      }
                    }}
                    data-testid={`env-secret-${i}`}
                  >
                    <Switch.Content className="text-xs">
                      <Switch.Control>
                        <Switch.Thumb />
                      </Switch.Control>
                      {t("agent.envSecret")}
                    </Switch.Content>
                  </Switch>
                  <Button
                    size="sm"
                    variant="ghost"
                    isIconOnly
                    aria-label={t("agent.envRemove")}
                    onPress={() => remove(i)}
                    data-testid={`env-remove-${i}`}
                  >
                    <Icon icon="gravity-ui:trash-bin" className="size-4" />
                  </Button>
                </div>
                {err !== "" && (
                  <p className="text-xs text-danger" role="alert" data-testid={`env-error-${i}`}>
                    {err}
                  </p>
                )}
              </div>
            );
          })}
        </div>
      )}

      <div className="flex items-center gap-3">
        <Button
          size="sm"
          variant="outline"
          onPress={add}
          isDisabled={envs.length >= ENV_LIMITS.maxCount}
          data-testid="env-add"
        >
          <Icon icon="gravity-ui:plus" className="size-4" />
          {t("agent.envAdd")}
        </Button>
        {envs.length >= ENV_LIMITS.maxCount && (
          <span className="text-xs text-muted">
            {t("agent.envAtLimit", { max: ENV_LIMITS.maxCount })}
          </span>
        )}
      </div>

      {/*
        ⚠️ This explanation is **part of the feature, not decoration**. It says three things at once:
        normal items go into the system prompt, sensitive items do not, and **the agent can still read sensitive items**.
        Without the third sentence, users would mark credentials that really should not be given to the agent as "sensitive" and think they are safe.
      */}
      {envs.length > 0 && (
        <p className="text-xs text-muted" data-testid="agent-env-note">
          {t("agent.envNote")}
        </p>
      )}
    </div>
  );
}

/**
 * Credential aliases: inject an existing platform credential once more under a variable name the user chooses.
 *
 * # Why this block **has no value input box**
 *
 * That is the core of the design, not an omission. Users can only choose "which platform credential to reference"; **the value is filled in by the server at the moment
 * of injection**, so the raw credential never goes into the request body, the database, or the browser.
 *
 * Letting users fill in values would open a privilege escalation hole (trading "can edit the agent" for "making the container call the platform with any
 * identity"); see the AgentEnvAlias comment in server/internal/domain for details.
 * For users to fill in **their own** values, use the "environment variables" block above, and check sensitive if needed.
 *
 * # Why collapsed by default
 *
 * This is an advanced feature: the vast majority of agents are fine with the platform's default variable names; only connecting third-party CLIs
 * (like one that only accepts VOLC_JWT) needs it. It is collapsed so it does not take attention from the main path.
 *
 * ⚠️ **But collapsed ≠ hidden**: the heading row is always visible, so users know at a glance it exists.
 * Making it "impossible to find the entry" would leave the people who need it stuck at this step, with no clue in the UI.
 */
export function AgentEnvAliasSection({
  draft,
  setDraft,
  canManage = true,
}: {
  draft: AgentDraft;
  setDraft: (d: AgentDraft) => void;
  /**
   * Whether it can be changed (owner/admin). Three states, meaning exactly the same as AgentIntegrationsSection:
   * `null` is "not known yet", when the whole block is not rendered, to avoid drawing editable first and then jumping to read-only.
   */
  canManage?: boolean | null;
}) {
  const { t } = usePrefs();
  const aliases = draft.envAliases ?? [];
  const envs = draft.envs ?? [];

  function set(next: AgentEnvAlias[]) {
    setDraft({ ...draft, envAliases: next });
  }
  function patch(i: number, part: Partial<AgentEnvAlias>) {
    set(aliases.map((a, idx) => (idx === i ? { ...a, ...part } : a)));
  }

  function errorFor(a: AgentEnvAlias, i: number): string {
    const key = a.key.trim();
    if (key === "") return "";
    if (!isValidEnvKey(key)) return t("agent.envAliasNameInvalid");
    if (isReservedEnvKey(key)) return t("agent.envAliasNameReserved");
    // Duplicates: only reported on **the later one**, same rule as the env side (with both rows red the user does not know which to change).
    if (aliases.some((other, idx) => idx < i && other.key.trim() === key)) {
      return t("agent.envAliasNameDuplicate");
    }
    // Same name as a custom env: the backend rejects it (which one wins is undefined), so warn right here instead of waiting for save.
    if (envs.some((e) => e.key.trim() === key)) return t("agent.envAliasClashesEnv");
    return "";
  }

  // Role not known yet: the whole block is not rendered. Same as AgentIntegrationsSection.
  if (canManage === null) return null;

  // Read-only state: **no controls, a conclusion instead**. The same treatment as the authorization block: disabled controls make people keep clicking,
  // while one line "who can change it" is the information they need.
  //
  // ⚠️ This branch must come before "empty list renders nothing": when member opens an agent that already has aliases configured
  // they must see those aliases, otherwise they would think none are configured.
  if (!canManage) {
    return (
      <div className="flex flex-col gap-4">
        <SectionTitle
          title={t("agent.envAliasSection")}
          description={t("agent.envAliasSectionDesc")}
        />
        <div className="flex flex-col gap-2" data-testid="agent-env-aliases-readonly">
          <span className="text-xs text-muted">{t("agent.envAliasReadOnly")}</span>
          {aliases.length > 0 ? (
            <div className="flex flex-wrap items-center gap-2">
              {aliases.map((a, i) => (
                <Chip key={i} size="sm" variant="soft" data-testid={`env-alias-chip-${i}`}>
                  <Chip.Label className="font-mono">
                    {a.key} ← {a.from}
                  </Chip.Label>
                </Chip>
              ))}
            </div>
          ) : (
            <span className="text-sm text-muted" data-testid="env-alias-readonly-none">
              {t("agent.envAliasReadOnlyNone")}
            </span>
          )}
          <p className="text-xs text-muted" data-testid="env-alias-need-admin">
            {t("agent.envAliasReadOnlyHint")}
          </p>
        </div>
      </div>
    );
  }

  return (
    <Disclosure>
      <Disclosure.Heading>
        <Disclosure.Trigger
          className="flex items-center gap-2 text-left text-xs text-muted cursor-[var(--cursor-interactive)] hover:text-foreground"
          data-testid="env-alias-toggle"
        >
          <Disclosure.Indicator>
            <Icon icon="gravity-ui:chevron-right" className="size-3" />
          </Disclosure.Indicator>
          <span className="font-medium">{t("agent.envAliasSection")}</span>
          {aliases.length > 0 && <span className="num">({aliases.length})</span>}
        </Disclosure.Trigger>
      </Disclosure.Heading>
      <Disclosure.Content>
        <Disclosure.Body>
          <div className="mt-2 flex flex-col gap-3" data-testid="agent-env-aliases">
            <p className="text-xs text-muted">{t("agent.envAliasSectionDesc")}</p>

            {aliases.map((a, i) => {
              const err = errorFor(a, i);
              return (
                <div
                  key={i}
                  className="flex flex-col gap-1"
                  data-testid={`env-alias-row-${i}`}
                >
                  <div className="flex flex-wrap items-end gap-2">
                    <TextField className="min-w-40 flex-1" isInvalid={err !== ""}>
                      <Label className="text-xs text-muted">{t("agent.envAliasName")}</Label>
                      <Input
                        value={a.key}
                        onChange={(ev) => patch(i, { key: ev.target.value })}
                        placeholder="VOLC_JWT"
                        className="font-mono"
                        data-testid={`env-alias-key-${i}`}
                      />
                    </TextField>
                    {/*
                      The source can only be chosen from a fixed list, not typed in: typing would make "references a credential that does not
                      exist" a common state, and then the alias silently does not work, with no clue in the UI.
                    */}
                    <Select
                      className="min-w-56 flex-[2]"
                      selectedKey={a.from}
                      onSelectionChange={(k) => patch(i, { from: String(k) })}
                    >
                      <Label className="text-xs text-muted">
                        {t("agent.envAliasSource")}
                      </Label>
                      <Select.Trigger data-testid={`env-alias-from-${i}`}>
                        <Select.Value />
                        <Select.Indicator />
                      </Select.Trigger>
                      <Select.Popover>
                        <ListBox>
                          {ALIASABLE_ENV_SOURCES.map((src) => (
                            <ListBox.Item key={src.key} id={src.key} textValue={t(src.labelKey)}>
                              {t(src.labelKey)}
                            </ListBox.Item>
                          ))}
                        </ListBox>
                      </Select.Popover>
                    </Select>
                    <Button
                      variant="ghost"
                      isIconOnly
                      aria-label={t("agent.envAliasRemove")}
                      onPress={() => set(aliases.filter((_, idx) => idx !== i))}
                      data-testid={`env-alias-remove-${i}`}
                    >
                      <Icon icon="gravity-ui:trash-bin" className="size-4" />
                    </Button>
                  </div>
                  {err !== "" && (
                    <span className="text-xs text-danger" data-testid={`env-alias-error-${i}`}>
                      {err}
                    </span>
                  )}
                </div>
              );
            })}

            <div className="flex items-center gap-3">
              <Button
                variant="outline"
                size="sm"
                isDisabled={aliases.length >= ENV_LIMITS.maxCount}
                onPress={() =>
                  set([...aliases, { key: "", from: ALIASABLE_ENV_SOURCES[0].key }])
                }
                data-testid="env-alias-add"
              >
                <Icon icon="gravity-ui:plus" className="size-4" />
                {t("agent.envAliasAdd")}
              </Button>
              {aliases.length >= ENV_LIMITS.maxCount && (
                <span className="text-xs text-muted">
                  {t("agent.envAliasAtLimit", { max: ENV_LIMITS.maxCount })}
                </span>
              )}
            </div>

            {/*
              ⚠️ This explanation is **part of the feature**. None of the three sentences can be missing, especially the second:
              "it does not work when the source's authorization is not checked" is the case that most easily makes people think the feature is broken: they configured the alias,
              but the container has no value, and nothing in the UI points to "you have not checked that authorization".
            */}
            {aliases.length > 0 && (
              <p className="text-xs text-muted" data-testid="agent-env-alias-note">
                {t("agent.envAliasNote")}
              </p>
            )}
          </div>
        </Disclosure.Body>
      </Disclosure.Content>
    </Disclosure>
  );
}

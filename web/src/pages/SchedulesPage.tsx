/**
 * Schedules page: the config screen for proactive capabilities.
 *
 * Three trigger kinds (cron / fixed interval / one-shot) + two deliveries (record only / send to
 * Feishu).
 * One trigger is one Run with `trigger: schedule`, so "last result" links straight to run detail;
 * we deliberately do not redraw the event timeline here.
 */

import { useCallback, useMemo, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import {
  Button,
  Chip,
  ComboBox,
  Description,
  FieldError,
  Input,
  Label,
  ListBox,
  Select,
  Switch,
  TextArea,
  TextField,
  Tooltip,
} from "@heroui/react";
import { Icon } from "@iconify/react";
import { api } from "@/data/api";
import { useWorkspace, useWorkspacePath } from "@/app/WorkspaceGate";
import {
  AgentDeletedChip,
  AsyncErrorBlock,
  Empty,
  ListSkeleton,
  Notice,
  PageBody,
  PageHeader,
} from "@/components/Page";
import { ConfirmModal, FormModal } from "@/components/FormModal";
import { formatRelative } from "@/lib/format";
import { useAsyncData } from "@/lib/useAsyncData";
import { usePrefs } from "@/prefs/PrefsContext";
import type { MessageKey } from "@/prefs/messages";
import {
  DEFAULT_SCHEDULE_TZ,
  MIN_SCHEDULE_INTERVAL_MS,
  validateCron,
} from "@/types/domain";
import type {
  Agent,
  DeliveryKind,
  FeishuChat,
  Schedule,
  ScheduleDraft,
  ScheduleKind,
} from "@/types/domain";

/** cron quick templates. Hand-written cron is easy to get wrong, so give a few of the most common. */
const CRON_PRESETS: { labelKey: "sched.presetWorkdayMorning" | "sched.presetDaily" | "sched.presetHourly" | "sched.presetMonday"; expr: string }[] = [
  { labelKey: "sched.presetWorkdayMorning", expr: "30 9 * * 1-5" },
  { labelKey: "sched.presetDaily", expr: "0 9 * * *" },
  { labelKey: "sched.presetHourly", expr: "0 * * * *" },
  { labelKey: "sched.presetMonday", expr: "0 10 * * 1" },
];

/** Interval options (milliseconds). No free input: below the minute level is meaningless, and the server rejects it too. */
const INTERVAL_OPTIONS = [5, 15, 30, 60, 180, 720].map((min) => ({
  ms: min * 60_000,
  min,
}));

/** Delay options for one-shot tasks (minutes). */
const DELAY_OPTIONS = [10, 30, 60, 180, 1440];

function emptyDraft(agentId: string): ScheduleDraft {
  return {
    agentId,
    name: "",
    spec: { kind: "cron", cron: "30 9 * * 1-5", tz: DEFAULT_SCHEDULE_TZ },
    prompt: "",
    delivery: { kind: "run_only" },
  };
}

export type DraftErrors = Partial<Record<"name" | "prompt" | "spec" | "delivery", string>>;

/**
 * Validate the draft and return per-field errors.
 *
 * **Exported and shared by two callers** (the create dialog and the detail page): with a copy the
 * two would drift,
 * and the drift always goes "a new check only got into one side": a gap with zero errors, where the
 * same invalid input
 * is blocked in the dialog but let through on the detail page, then rejected by the server with a
 * 400.
 *
 * `t` is passed in by the caller rather than called here: this is a module-level pure function,
 * and hooks can only be called at the top level of a component body
 * (see docs/guides/i18n.md).
 */
export function validateScheduleDraft(
  d: ScheduleDraft,
  t: (key: MessageKey, vars?: Record<string, string | number>) => string,
): DraftErrors {
  const e: DraftErrors = {};
  if (!d.name.trim()) e.name = t("sched.nameRequired");
  if (!d.prompt.trim()) e.prompt = t("sched.promptRequired");
  if (d.spec.kind === "cron") {
    const expr = (d.spec.cron ?? "").trim();
    if (!expr) e.spec = t("sched.cronRequired");
    else {
      const err = validateCron(expr, t);
      if (err) e.spec = err;
    }
  }
  if (d.delivery.kind === "feishu" && !d.delivery.chatId) {
    e.delivery = t("sched.chatRequired");
  }
  return e;
}

/**
 * Human-readable description of the trigger kind, used by list rows and the detail page header.
 *
 * The text is passed in by the caller rather than calling `t()` here: this is a module-level pure
 * function,
 * and hooks can only be called at the top level of a component body (see docs/guides/i18n.md).
 */
export function describeSpec(
  sc: Schedule,
  labels: { every: (min: number) => string; oneshot: string },
): string {
  switch (sc.spec.kind) {
    case "cron":
      return sc.spec.cron ?? "";
    case "interval":
      return labels.every(Math.round((sc.spec.everyMs ?? 0) / 60_000));
    case "oneshot":
      // A one-shot task's fire time is already shown in the nextRunAt column; here we only mark
      // the kind.
      return labels.oneshot;
    default:
      return "";
  }
}

/**
 * Task form, **shared by the create dialog and the detail page**.
 *
 * Shared rather than written twice: there are nine fields, and each of the three specs has its
 * own shape, so a copy would surely drift
 * (a new field only gets into one side, with zero errors). The real differences between the two
 * are just two, both made into props:
 *
 *   - `promptRows`: 4 lines in the dialog, 18 on the detail page; **that is the first reason the
 *     detail page exists**
 *     (a real inspection task's prompt has 6180 characters, which simply cannot be edited in a
 *     dialog).
 *   - `testIdPrefix`: when both are in the DOM at once their testids would collide (the detail
 *     page form and the list page dialog
 *     never appear together, but the prefix makes e2e intent clearer, and guards against them
 *     really coexisting later).
 */
export function ScheduleForm({
  draft,
  setDraft,
  errors,
  agents,
  chats,
  promptRows = 4,
  testIdPrefix = "sched",
}: {
  draft: ScheduleDraft;
  setDraft: (d: ScheduleDraft) => void;
  errors: DraftErrors;
  agents: Agent[];
  chats: FeishuChat[];
  promptRows?: number;
  testIdPrefix?: string;
}) {
  const { t } = usePrefs();
  const tid = (name: string) => `${testIdPrefix}-${name}`;

  return (
    <>
      <div className="grid gap-5 sm:grid-cols-2">
        <TextField isRequired isInvalid={Boolean(errors.name)}>
          <Label>{t("sched.name")}</Label>
          <Input
            placeholder={t("sched.namePlaceholder")}
            value={draft.name}
            onChange={(e) => setDraft({ ...draft, name: e.target.value })}
            data-testid={tid("name")}
          />
          {errors.name ? <FieldError>{errors.name}</FieldError> : null}
        </TextField>

        <Select
          selectedKey={draft.agentId}
          /*
            Disabled agents cannot be bound: once bound this task is sure to fail once when it fires
            (the backend's CreateConversation
            returns 422), and the failure is only visible at the next trigger. Keeping them in the
            list grayed out with the reason
            is better than letting the user finish configuring and then wait for a failure.
          */
          disabledKeys={agents
            .filter((a) => a.status === "disabled")
            .map((a) => a.id)}
          onSelectionChange={(k) => setDraft({ ...draft, agentId: String(k) })}
        >
          <Label>{t("sched.agent")}</Label>
          <Select.Trigger data-testid={tid("agent")}>
            <Select.Value />
            <Select.Indicator />
          </Select.Trigger>
          <Select.Popover>
            <ListBox>
              {agents.map((a) => (
                <ListBox.Item key={a.id} id={a.id} textValue={a.name}>
                  {a.status === "disabled"
                    ? `${a.name}（${t("enum.agentStatus.disabled")}）`
                    : a.name}
                </ListBox.Item>
              ))}
            </ListBox>
          </Select.Popover>
        </Select>
      </div>

      {/* Trigger kind */}
      <Select
        className="max-w-xs"
        selectedKey={draft.spec.kind}
        onSelectionChange={(k) => {
          const kind = String(k) as ScheduleKind;
          // When switching kind, replace the payload with that kind's default; keeping the previous
          // kind's fields would make the server
          // receive a spec whose kind does not match its payload.
          if (kind === "cron") {
            setDraft({ ...draft, spec: { kind, cron: "30 9 * * 1-5", tz: DEFAULT_SCHEDULE_TZ } });
          } else if (kind === "interval") {
            setDraft({ ...draft, spec: { kind, everyMs: 15 * 60_000 } });
          } else {
            setDraft({
              ...draft,
              spec: { kind, fireAt: new Date(Date.now() + 60 * 60_000).toISOString() },
            });
          }
        }}
      >
        <Label>{t("sched.kind")}</Label>
        <Select.Trigger data-testid={tid("kind")}>
          <Select.Value />
          <Select.Indicator />
        </Select.Trigger>
        <Select.Popover>
          <ListBox>
            <ListBox.Item id="cron" textValue={t("sched.kindCron")}>
              {t("sched.kindCron")}
            </ListBox.Item>
            <ListBox.Item id="interval" textValue={t("sched.kindInterval")}>
              {t("sched.kindInterval")}
            </ListBox.Item>
            <ListBox.Item id="oneshot" textValue={t("sched.kindOneshot")}>
              {t("sched.kindOneshot")}
            </ListBox.Item>
          </ListBox>
        </Select.Popover>
      </Select>

      {draft.spec.kind === "cron" ? (
        <div className="flex flex-col gap-3">
          <TextField isRequired isInvalid={Boolean(errors.spec)}>
            <Label>{t("sched.cron")}</Label>
            <Input
              className="font-mono"
              placeholder="30 9 * * 1-5"
              value={draft.spec.cron ?? ""}
              onChange={(e) =>
                setDraft({ ...draft, spec: { ...draft.spec, cron: e.target.value } })
              }
              data-testid={tid("cron")}
            />
            <Description>{t("sched.cronDesc", { tz: draft.spec.tz ?? DEFAULT_SCHEDULE_TZ })}</Description>
            {errors.spec ? <FieldError>{errors.spec}</FieldError> : null}
          </TextField>
          <div className="flex flex-wrap items-center gap-2">
            {CRON_PRESETS.map((p, i) => (
              <Button
                key={p.expr}
                size="sm"
                variant="outline"
                onPress={() => setDraft({ ...draft, spec: { ...draft.spec, cron: p.expr } })}
                data-testid={tid(`preset-${i}`)}
              >
                {t(p.labelKey)}
              </Button>
            ))}
          </div>
        </div>
      ) : null}

      {draft.spec.kind === "interval" ? (
        <Select
          className="max-w-xs"
          selectedKey={String(draft.spec.everyMs ?? MIN_SCHEDULE_INTERVAL_MS)}
          onSelectionChange={(k) =>
            setDraft({ ...draft, spec: { ...draft.spec, everyMs: Number(k) } })
          }
        >
          <Label>{t("sched.interval")}</Label>
          <Select.Trigger data-testid={tid("interval")}>
            <Select.Value />
            <Select.Indicator />
          </Select.Trigger>
          <Select.Popover>
            <ListBox>
              {INTERVAL_OPTIONS.map((o) => (
                <ListBox.Item
                  key={o.ms}
                  id={String(o.ms)}
                  textValue={t("sched.everyMinutes", { count: o.min })}
                >
                  {t("sched.everyMinutes", { count: o.min })}
                </ListBox.Item>
              ))}
            </ListBox>
          </Select.Popover>
        </Select>
      ) : null}

      {draft.spec.kind === "oneshot" ? (
        <Select
          className="max-w-xs"
          selectedKey="60"
          onSelectionChange={(k) =>
            setDraft({
              ...draft,
              spec: {
                ...draft.spec,
                fireAt: new Date(Date.now() + Number(k) * 60_000).toISOString(),
              },
            })
          }
        >
          <Label>{t("sched.delay")}</Label>
          <Select.Trigger data-testid={tid("delay")}>
            <Select.Value />
            <Select.Indicator />
          </Select.Trigger>
          <Select.Popover>
            <ListBox>
              {DELAY_OPTIONS.map((min) => (
                <ListBox.Item
                  key={min}
                  id={String(min)}
                  textValue={t("sched.afterMinutes", { count: min })}
                >
                  {t("sched.afterMinutes", { count: min })}
                </ListBox.Item>
              ))}
            </ListBox>
          </Select.Popover>
        </Select>
      ) : null}

      <TextField isRequired isInvalid={Boolean(errors.prompt)}>
        <Label>{t("sched.prompt")}</Label>
        <TextArea
          rows={promptRows}
          placeholder={t("sched.promptPlaceholder")}
          value={draft.prompt}
          onChange={(e) => setDraft({ ...draft, prompt: e.target.value })}
          data-testid={tid("prompt")}
        />
        <Description>{t("sched.promptDesc")}</Description>
        {errors.prompt ? <FieldError>{errors.prompt}</FieldError> : null}
      </TextField>

      {/* Delivery */}
      <Select
        className="max-w-sm"
        selectedKey={draft.delivery.kind}
        onSelectionChange={(k) => {
          const kind = String(k) as DeliveryKind;
          if (kind === "run_only") {
            setDraft({ ...draft, delivery: { kind } });
          } else {
            const first = chats[0];
            setDraft({
              ...draft,
              delivery: {
                kind,
                feishuAppId: first?.feishuAppId,
                chatId: first?.chatId,
                chatName: first?.name,
              },
            });
          }
        }}
      >
        <Label>{t("sched.delivery")}</Label>
        <Select.Trigger data-testid={tid("delivery")}>
          <Select.Value />
          <Select.Indicator />
        </Select.Trigger>
        <Select.Popover>
          <ListBox>
            <ListBox.Item id="run_only" textValue={t("sched.deliveryRunOnly")}>
              {t("sched.deliveryRunOnly")}
            </ListBox.Item>
            <ListBox.Item id="feishu" textValue={t("sched.deliveryFeishu")}>
              {t("sched.deliveryFeishu")}
            </ListBox.Item>
          </ListBox>
        </Select.Popover>
      </Select>

      {draft.delivery.kind === "feishu" ? (
        chats.length === 0 ? (
          // When no group has been recorded, give a guide instead of an empty dropdown: groups are
          // "recorded as inbound messages pass by",
          // so the user must first @ the bot once in the group.
          <Notice
            tone="warning"
            icon="gravity-ui:triangle-exclamation"
            title={t("sched.noChatsTitle")}
            hint={t("sched.noChatsDesc")}
          />
        ) : (
          // ComboBox rather than Select: a bot may have seen dozens of groups in one workspace, so it
          // must be searchable by group name.
          // Filtering uses RAC's built-in one (contains match on textValue), so textValue only holds
          // the group name;
          // it is also the text filled back into the input after selection, and adding chat_id would
          // stretch the input unreadably.
          //
          // menuTrigger="focus": clicking into the input opens **all** groups (RAC does not filter by
          // the selected group name in the input when opening on focus), so the user need not delete
          // the filled-in name to see other groups.
          <ComboBox
            className="max-w-sm"
            menuTrigger="focus"
            selectedKey={draft.delivery.chatId ?? null}
            onSelectionChange={(k) => {
              const chat = chats.find((c) => c.chatId === String(k));
              if (!chat) return;
              setDraft({
                ...draft,
                delivery: {
                  ...draft.delivery,
                  feishuAppId: chat.feishuAppId,
                  chatId: chat.chatId,
                  chatName: chat.name,
                },
              });
            }}
          >
            <Label>{t("sched.chat")}</Label>
            <ComboBox.InputGroup>
              <Input placeholder={t("sched.chatSearch")} data-testid={tid("chat")} />
              <ComboBox.Trigger />
            </ComboBox.InputGroup>
            <ComboBox.Popover>
              <ListBox
                renderEmptyState={() => (
                  <div className="px-3 py-6 text-center text-sm text-muted">
                    {t("sched.chatNoMatch")}
                  </div>
                )}
              >
                {chats.map((c) => (
                  <ListBox.Item
                    key={c.chatId}
                    id={c.chatId}
                    // When the group name cannot be fetched (the bot was removed from the group,
                    // etc.) fall back to showing chatId.
                    textValue={c.name || c.chatId}
                  >
                    <div className="flex min-w-0 flex-col py-0.5">
                      <span className="truncate text-sm">{c.name || c.chatId}</span>
                      {/* With a group name, also show chat_id: groups with the same name are not rare, and it tells them apart. */}
                      {c.name ? (
                        <span className="truncate text-xs text-muted">{c.chatId}</span>
                      ) : null}
                    </div>
                    <ListBox.ItemIndicator />
                  </ListBox.Item>
                ))}
              </ListBox>
            </ComboBox.Popover>
          </ComboBox>
        )
      ) : null}
      {errors.delivery ? <p className="text-xs text-danger">{errors.delivery}</p> : null}
    </>
  );
}

export function SchedulesPage() {
  const { t, language } = usePrefs();
  const { workspace } = useWorkspace();
  const wp = useWorkspacePath();
  const navigate = useNavigate();

  const [creating, setCreating] = useState(false);
  const [deleting, setDeleting] = useState<Schedule | null>(null);
  const [draft, setDraft] = useState<ScheduleDraft>(emptyDraft(""));
  const [errors, setErrors] = useState<DraftErrors>({});
  const [notice, setNotice] = useState<string | null>(null);

  // Failures must be visible, not stuck on the skeleton (see the file header of lib/useAsyncData.ts).
  const fetch = useCallback(async () => {
    const [schedules, agents, chats] = await Promise.all([
      api.listSchedules(workspace.id),
      api.listAgents(workspace.id),
      api.listFeishuChats(workspace.id),
    ]);
    return { schedules, agents, chats };
  }, [workspace.id]);
  const { data, error, isLoading, reload: load } = useAsyncData(
    fetch,
    t("common.loadFailedUnknown"),
  );
  const schedules = data?.schedules ?? null;
  const agents = data?.agents ?? [];
  const chats = data?.chats ?? [];
  const chatNameById = useMemo(
    () => new Map((data?.chats ?? []).filter((c) => c.name).map((c) => [c.chatId, c.name])),
    [data?.chats],
  );

  function validate(d: ScheduleDraft): boolean {
    // Use the exported one, shared with the detail page (see the comment on validateScheduleDraft).
    const e = validateScheduleDraft(d, t);
    setErrors(e);
    return Object.keys(e).length === 0;
  }

  function openCreate() {
    setDraft(emptyDraft(agents[0]?.id ?? ""));
    setErrors({});
    setCreating(true);
  }

  async function toggle(sc: Schedule) {
    await api.setScheduleStatus(sc.id, sc.status === "active" ? "paused" : "active");
    await load();
  }

  async function runNow(sc: Schedule) {
    await api.runScheduleNow(sc.id);
    // Test-run is async (the server sets nextRunAt to now, the scheduler does the real run),
    // so we can only say "queued" here, not "done"; that would be false.
    setNotice(t("sched.runNowQueued", { name: sc.name }));
    await load();
  }

  const hasAgents = agents.length > 0;
  const sorted = useMemo(
    () =>
      schedules
        ? [...schedules].sort((a, b) => {
            // Enabled first, then by next fire time ascending (the soonest at the top).
            if (a.status !== b.status) return a.status === "active" ? -1 : 1;
            return (a.nextRunAt ?? "￿").localeCompare(b.nextRunAt ?? "￿");
          })
        : null,
    [schedules],
  );

  return (
    <PageBody>
      <PageHeader
        title={t("nav.schedules")}
        description={t("sched.listDesc")}
        actions={
          <Button onPress={openCreate} isDisabled={!hasAgents} data-testid="new-schedule">
            {t("sched.new")}
          </Button>
        }
      />

      {/*
        The test-run receipt uses one plain line, not a Notice: Notice only has warning / danger,
        which mean "something went wrong", while this is a successful action.
      */}
      {notice ? (
        <p className="text-sm text-muted" data-testid="schedule-notice">
          {notice}
        </p>
      ) : null}

      {isLoading ? (
        <ListSkeleton />
      ) : error ? (
        <AsyncErrorBlock error={error} onRetry={load} />
      ) : sorted === null ? null : !hasAgents ? (
        <Empty
          icon="gravity-ui:clock"
          title={t("sched.needAgentTitle")}
          description={t("sched.needAgentDesc")}
          action={
            <Button onPress={() => {}} isDisabled>
              {t("sched.new")}
            </Button>
          }
        />
      ) : sorted.length === 0 ? (
        <Empty
          icon="gravity-ui:clock"
          title={t("sched.emptyTitle")}
          description={t("sched.emptyDesc")}
          action={<Button onPress={openCreate}>{t("sched.new")}</Button>}
        />
      ) : (
        <div className="flex flex-col gap-2" data-testid="schedule-list">
          {sorted.map((sc) => (
            <div
              key={sc.id}
              className="flex flex-col gap-3 r-row border border-border px-4 py-3.5 sm:flex-row sm:items-center"
              data-testid={`schedule-row-${sc.id}`}
            >
              <div className="flex min-w-0 flex-1 flex-col gap-1">
                <div className="flex items-center gap-2">
                  {/*
                    Only the title is a link, the whole card is not wrapped in one: this row has 4
                    interactive controls on the right
                    (test-run/detail/delete/toggle), and wrapping a <Link> around them would nest
                    interactive elements.
                    Same tradeoff as in SkillsPage (AgentsPage can wrap the whole card because its
                    cards have no buttons).
                  */}
                  <Link
                    to={wp(`/schedules/${sc.id}`)}
                    className="truncate text-sm font-medium no-underline hover:underline"
                    data-testid={`schedule-link-${sc.id}`}
                  >
                    {sc.name}
                  </Link>
                  {sc.status === "paused" ? (
                    <Chip size="sm" variant="soft" color="warning">
                      <Chip.Label>{t("sched.paused")}</Chip.Label>
                    </Chip>
                  ) : null}
                </div>
                <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted">
                  <span className="flex items-center gap-1.5">
                    <Icon icon="gravity-ui:face-robot" className="size-3.5" />
                    {sc.agentName}
                    {sc.agentDeleted ? <AgentDeletedChip /> : null}
                  </span>
                  <span className="flex items-center gap-1.5 font-mono">
                    <Icon icon="gravity-ui:clock" className="size-3.5" />
                    {describeSpec(sc, {
                      every: (min) => t("sched.everyMinutes", { count: min }),
                      oneshot: t("sched.kindOneshot"),
                    })}
                  </span>
                  {sc.delivery.kind === "feishu" ? (
                    <span className="flex items-center gap-1.5">
                      <Icon icon="gravity-ui:comment" className="size-3.5" />
                      {/* Tasks created early stored an empty chatName (group names could not be fetched then), so look it up once. */}
                      {sc.delivery.chatName ||
                        chatNameById.get(sc.delivery.chatId ?? "") ||
                        sc.delivery.chatId}
                    </span>
                  ) : null}
                </div>
                {sc.lastError ? (
                  <p className="truncate text-xs text-danger" data-testid={`schedule-error-${sc.id}`}>
                    {t("sched.lastError", { error: sc.lastError })}
                  </p>
                ) : null}
              </div>

              <div className="flex shrink-0 items-center gap-6 text-xs">
                <div className="flex w-28 flex-col gap-0.5">
                  <span className="text-muted">{t("sched.nextRun")}</span>
                  <span className="num">
                    {sc.nextRunAt
                      ? formatRelative(sc.nextRunAt, language)
                      : // An empty nextRunAt has two causes; **tell them apart by status**: active means it
                        // was just claimed by the scheduler and is running (claiming clears
                        // next_run_at); only paused is
                        // the "not scheduled" the user stopped on purpose. Merging them into one
                        // sentence would describe "running" as "you did not configure it".
                        t(sc.status === "active" ? "sched.running" : "sched.notScheduled")}
                  </span>
                </div>
                <div className="flex w-20 flex-col gap-0.5">
                  <span className="text-muted">{t("sched.runCount")}</span>
                  <span className="num">
                    {sc.runCount}
                    {sc.failCount > 0 ? (
                      <span className="text-danger"> / {sc.failCount}</span>
                    ) : null}
                  </span>
                </div>
                <div className="flex w-24 flex-col gap-0.5">
                  <span className="text-muted">{t("sched.lastRun")}</span>
                  {sc.lastRunId ? (
                    <Link to={wp(`/runs/${sc.lastRunId}`)} className="num underline">
                      {sc.lastRunAt ? formatRelative(sc.lastRunAt, language) : t("sched.view")}
                    </Link>
                  ) : (
                    <span className="num text-muted">—</span>
                  )}
                </div>

                <div className="flex items-center gap-1">
                  <Tooltip>
                    <Tooltip.Trigger>
                      <Button
                        variant="ghost"
                        isIconOnly
                        aria-label={t("sched.runNow")}
                        isDisabled={sc.agentDeleted}
                        onPress={() => void runNow(sc)}
                        data-testid={`run-now-${sc.id}`}
                      >
                        <Icon icon="gravity-ui:play" className="size-4" />
                      </Button>
                    </Tooltip.Trigger>
                    <Tooltip.Content>{t("sched.runNow")}</Tooltip.Content>
                  </Tooltip>
                  <Tooltip>
                    <Tooltip.Trigger>
                      <Button
                        variant="ghost"
                        isIconOnly
                        aria-label={t("sched.openDetail")}
                        onPress={() => navigate(wp(`/schedules/${sc.id}`))}
                        data-testid={`edit-schedule-${sc.id}`}
                      >
                        <Icon icon="gravity-ui:pencil-to-square" className="size-4" />
                      </Button>
                    </Tooltip.Trigger>
                    <Tooltip.Content>{t("sched.openDetail")}</Tooltip.Content>
                  </Tooltip>
                  <Tooltip>
                    <Tooltip.Trigger>
                      <Button
                        variant="ghost"
                        isIconOnly
                        aria-label={t("common.delete")}
                        onPress={() => setDeleting(sc)}
                        data-testid={`delete-schedule-${sc.id}`}
                      >
                        <Icon icon="gravity-ui:trash-bin" className="size-4" />
                      </Button>
                    </Tooltip.Trigger>
                    <Tooltip.Content>{t("common.delete")}</Tooltip.Content>
                  </Tooltip>
                  {/* Toggle at the end of the row, matching the Feishu binding on the agent detail page */}
                  <Switch
                    isSelected={sc.status === "active"}
                    onChange={() => void toggle(sc)}
                    aria-label={t("sched.toggleLabel", { name: sc.name })}
                    data-testid={`toggle-schedule-${sc.id}`}
                  >
                    <Switch.Content>
                      <Switch.Control>
                        <Switch.Thumb />
                      </Switch.Control>
                    </Switch.Content>
                  </Switch>
                </div>
              </div>
            </div>
          ))}
        </div>
      )}

      <FormModal
        isOpen={creating}
        onOpenChange={setCreating}
        title={t("sched.new")}
        description={t("sched.newDesc")}
        submitLabel={t("common.create")}
        size="lg"
        testId="schedule-modal"
        onSubmit={async () => {
          if (!validate(draft)) return false;
          await api.createSchedule(workspace.id, draft);
          await load();
        }}
      >
        <ScheduleForm draft={draft} setDraft={setDraft} errors={errors} agents={agents} chats={chats} />
      </FormModal>

      <ConfirmModal
        isOpen={deleting !== null}
        onOpenChange={(open) => !open && setDeleting(null)}
        title={t("sched.deleteTitle", { name: deleting?.name ?? "" })}
        description={t("sched.deleteDesc")}
        confirmLabel={t("common.delete")}
        onConfirm={async () => {
          if (!deleting) return;
          await api.deleteSchedule(deleting.id);
          await load();
        }}
      />
    </PageBody>
  );
}

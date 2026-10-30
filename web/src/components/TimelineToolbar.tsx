/**
 * Filter toolbar of the event view: **one row**.
 *
 * 34 event kinds laid out flat cannot be read without filtering, so there are five ways to narrow, each matching a category that really exists in Ark
 * (not made-up buckets, see the top of `lib/eventFacets.ts`): quick filters, event domain, semantic category,
 * specific type, plus "only slow / only failed" and thread.
 *
 * # Why it was squeezed from 6 rows into 1
 *
 * Before, these axes were **laid out as six rows** (search row / quick filters / event domain / semantic category / type fold / derived conditions),
 * stacked over most of the screen **above** the event list. The problem was not "ugly"; it was that **the main content was pushed down**:
 * the main content of the event view is that list, and before scrolling half the first screen went to the filters, while filters are
 * things you "adjust once in a while", and the list is what you read all the time.
 *
 * The current shape:
 *
 *   [search box]  [Filter ③ ▾]  [chips of selected conditions…]  [Reset]
 *
 * Three design decisions:
 *
 * 1. **In a Popover, not a Disclosure.** A Disclosure pushes everything below down when it expands,
 *    so "adjusting filters" would make the list you are reading jump; a Popover floats, and the list stays still.
 * 2. **Selected conditions must stay outside.** What is folded away is the **controls**, not the **state**:
 *    if "what is filtered out right now" were hidden too, users would look at an incomplete list without knowing why
 *    (exactly the source of confusion like "nothing left after filtering"). So the selected options stay on the row as chips,
 *    each removable in place.
 * 3. **The trigger has a count badge.** Without expanding you know "3 conditions are active".
 *
 * # The "sort" dropdown was removed
 *
 * It was a **dead control**: `applyRawFilter` never read `filter.sort`, and switching to "by duration descending" did not
 * change any order.
 * And with "one event per row", a single event cannot have a duration anyway (the only
 * exception is `span.model_request_end`, which carries `model_request_start_id` from Ark),
 * so this option was not "not wired up yet"; it **should not exist**. It was removed together with `?sort=` in the URL.
 *
 * Two pages share this component (run detail and chat), but **each gives counts by its own rule** (`ToolbarCounts`).
 */

import { useMemo } from "react";
import {
  Badge,
  Button,
  Chip,
  Popover,
  Separator,
  ToggleButton,
  Tooltip,
} from "@heroui/react";
import { Icon } from "@iconify/react";
import { CategoryFilter, ThreadSwitcher } from "@/components/RunTimeline";
import {
  EVENT_DOMAINS,
  FACET_PRESETS,
  SLOW_TOOL_MS,
  domainOf,
  type EventDomain,
  type FacetPresetId,
} from "@/lib/eventFacets";
import type { TimelineFilterState } from "@/lib/useTimelineFilter";
import { DEFAULT_VISIBLE_CATEGORIES, type EventCategory, type RunEventType } from "@/types/events";
import type { RunThread } from "@/types/domain";
import { usePrefs } from "@/prefs/PrefsContext";

/**
 * The counts the toolbar actually uses.
 *
 * It deliberately declares only these five instead of taking `RunStats` directly: the two views **count differently**:
 * the chat view counts "paired entries" (`computeStats()`), the event view counts "raw events"
 * (`rawEventStats()`), and the toolbar treats both the same. Narrowed to a structural type, both can be passed in,
 * and the compiler ensures the toolbar does not secretly use a field only one side has.
 */
export interface ToolbarCounts {
  byCategory: Record<string, number>;
  byDomain: Record<EventDomain, number>;
  byType: Partial<Record<RunEventType, number>>;
  /** Count for the "only slow calls" option. */
  slowCount: number;
  /** Count for the "only failed" option. */
  failedCount: number;
}

export interface TimelineToolbarProps {
  filter: TimelineFilterState;
  stats: ToolbarCounts;
  threads: RunThread[];
  isDirty: boolean;
  onToggleCategory: (key: string) => void;
  onToggleDomain: (domain: EventDomain) => void;
  onSetTypes: (types: Set<RunEventType>) => void;
  onApplyPreset: (id: FacetPresetId) => void;
  onReset: () => void;
  onQueryChange: (q: string) => void;
  onSlowOnlyChange: (on: boolean) => void;
  onErrorsOnlyChange: (on: boolean) => void;
  onThreadChange: (id: string | null) => void;
}

/** Whether a quick filter is currently selected: both the category and type axes must match the preset. */
function presetActive(
  filter: TimelineFilterState,
  categories: readonly string[],
  types: readonly RunEventType[],
) {
  const sameCat =
    filter.categories.size === categories.length &&
    categories.every((c) => filter.categories.has(c as never));
  const sameType =
    filter.types.size === types.length && types.every((t) => filter.types.has(t));
  return sameCat && sameType;
}

/** A "selected condition" chip that can be removed in place. */
function ActiveChip({
  label,
  onRemove,
  testId,
  tone,
}: {
  label: string;
  onRemove: () => void;
  testId: string;
  tone?: "warning";
}) {
  const { t } = usePrefs();
  return (
    <Chip
      size="sm"
      variant="soft"
      color={tone === "warning" ? "warning" : "default"}
      data-testid={testId}
    >
      <Chip.Label>{label}</Chip.Label>
      {/*
        A native button instead of <Button isIconOnly>: a button with its own padding and minimum
        touch size inside a Chip would make the chip twice as tall. Here we only need a 12px cross.
      */}
      <button
        type="button"
        aria-label={t("filter.removeCondition", { name: label })}
        onClick={onRemove}
        className="ml-1 flex items-center opacity-60 cursor-[var(--cursor-interactive)] hover:opacity-100"
      >
        <Icon icon="gravity-ui:xmark" className="size-3" />
      </button>
    </Chip>
  );
}

/** A section in the popover. Title + content; sections are grouped by spacing, not dividers. */
function Group({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="flex flex-col gap-2">
      <span className="text-xs font-medium text-muted">{title}</span>
      {children}
    </div>
  );
}

export function TimelineToolbar({
  filter,
  stats,
  threads,
  isDirty,
  onToggleCategory,
  onToggleDomain,
  onSetTypes,
  onApplyPreset,
  onReset,
  onQueryChange,
  onSlowOnlyChange,
  onErrorsOnlyChange,
  onThreadChange,
}: TimelineToolbarProps) {
  const { t } = usePrefs();

  // Category labels follow the current language, still iterated in a fixed order, so the filter does not jump.
  const categoryLabels = useMemo(
    () =>
      Object.fromEntries(
        (Object.keys(stats.byCategory) as EventCategory[]).map((c) => [
          c,
          t(`enum.category.${c}`),
        ]),
      ),
    [stats.byCategory, t],
  );

  /**
   * Options of the type dropdown: **only types that really appeared in this run**, grouped by domain.
   *
   * Filling in all 34 would stuff the dropdown with options that "always give nothing". Labels are Ark's type strings themselves:
   * this is a debugging view, and `agent.mcp_tool_use` is more precise than any translation, and matches the raw payload.
   */
  const typeGroups = useMemo(() => {
    const present = Object.keys(stats.byType) as RunEventType[];
    return EVENT_DOMAINS.map((domain) => ({
      domain,
      types: present.filter((ty) => domainOf(ty) === domain).sort(),
    })).filter((g) => g.types.length > 0);
  }, [stats.byType]);

  /**
   * "How many conditions are active", for the badge on the trigger.
   *
   * ⚠️ The keyword is not counted: it has its own input box right next to it, and users can see what they typed.
   * The badge only counts "the ones hidden in the popover", otherwise the number would not match "what you see when you open it".
   *
   * Categories and domains count **the number of options that differ from the default**, not the number selected: the default is "7 of 8 categories shown
   * (trace hidden by default)" and "all 4 domains on", and then the badge should be 0, not 11.
   */
  const activeCount = useMemo(() => {
    let n = 0;
    for (const c of DEFAULT_VISIBLE_CATEGORIES) if (!filter.categories.has(c)) n += 1;
    for (const c of filter.categories) {
      if (!DEFAULT_VISIBLE_CATEGORIES.includes(c)) n += 1;
    }
    n += EVENT_DOMAINS.filter((d) => !filter.domains.has(d)).length;
    n += filter.types.size;
    if (filter.slowOnly) n += 1;
    if (filter.errorsOnly) n += 1;
    if (filter.threadId) n += 1;
    return n;
  }, [filter]);

  /** The "selected conditions" kept outside on the row as chips. The controls fold; the state does not. */
  const activeChips = useMemo(() => {
    const chips: { key: string; label: string; onRemove: () => void; tone?: "warning" }[] = [];

    // Default categories turned off (like "chat hidden") and non-default categories turned on (like "trace shown")
    for (const c of DEFAULT_VISIBLE_CATEGORIES) {
      if (!filter.categories.has(c)) {
        chips.push({
          key: `cat-off-${c}`,
          label: t("filter.chipHidden", { name: t(`enum.category.${c}`) }),
          onRemove: () => onToggleCategory(c),
        });
      }
    }
    for (const c of filter.categories) {
      if (!DEFAULT_VISIBLE_CATEGORIES.includes(c)) {
        chips.push({
          key: `cat-on-${c}`,
          label: t(`enum.category.${c}`),
          onRemove: () => onToggleCategory(c),
        });
      }
    }
    for (const d of EVENT_DOMAINS) {
      if (!filter.domains.has(d)) {
        chips.push({
          key: `dom-off-${d}`,
          label: t("filter.chipHidden", { name: t(`enum.domain.${d}`) }),
          onRemove: () => onToggleDomain(d),
        });
      }
    }
    for (const ty of filter.types) {
      chips.push({
        key: `type-${ty}`,
        label: ty,
        onRemove: () => {
          const next = new Set(filter.types);
          next.delete(ty);
          onSetTypes(next);
        },
      });
    }
    if (filter.slowOnly) {
      chips.push({
        key: "slow",
        label: t("filter.slowOnly", { seconds: SLOW_TOOL_MS / 1000 }),
        onRemove: () => onSlowOnlyChange(false),
        tone: "warning",
      });
    }
    if (filter.errorsOnly) {
      chips.push({
        key: "failed",
        label: t("filter.errorsOnly"),
        onRemove: () => onErrorsOnlyChange(false),
        tone: "warning",
      });
    }
    if (filter.threadId) {
      const th = threads.find((x) => x.id === filter.threadId);
      chips.push({
        key: "thread",
        label: th?.agent_name ?? filter.threadId,
        onRemove: () => onThreadChange(null),
      });
    }
    return chips;
  }, [
    filter,
    threads,
    t,
    onToggleCategory,
    onToggleDomain,
    onSetTypes,
    onSlowOnlyChange,
    onErrorsOnlyChange,
    onThreadChange,
  ]);

  return (
    <div
      className="flex flex-wrap items-center gap-2"
      data-testid="timeline-toolbar"
      // The number of active conditions also goes into a data attribute: the badge only renders when >0,
      // so when e2e wants to assert "now in the default state" there is no element to select.
      data-active-filters={activeCount}
    >
      {/* The keyword stays outside: it is the most used one; hiding it in the popover would add two clicks every time */}
      <div className="relative w-full max-w-xs">
        <Icon
          icon="gravity-ui:magnifier"
          className="pointer-events-none absolute top-1/2 left-2.5 size-3.5 -translate-y-1/2 text-muted"
        />
        <input
          type="search"
          value={filter.query}
          onChange={(e) => onQueryChange(e.target.value)}
          aria-label={t("filter.searchLabel")}
          placeholder={t("filter.searchPlaceholder")}
          data-testid="timeline-search"
          /*
            A native input instead of HeroUI's SearchField: that component's Group stretches the height
            to 40px (md size), one step off from the size="sm" button next to it, and two heights in one row are obvious.
            Here we only need a 32px box, styled with the field token.
          */
          className="h-8 w-full r-row border border-field-border bg-surface pr-2 pl-8 text-sm placeholder:text-muted focus:border-accent focus:outline-none"
        />
      </div>

      {/* Filter popover: four axes + two derived conditions + thread, all in here */}
      <Popover>
        <Popover.Trigger
          className="flex h-8 items-center gap-1.5 r-row border border-field-border bg-surface px-2.5 text-sm cursor-[var(--cursor-interactive)] hover:bg-surface-secondary"
          data-testid="timeline-filter-open"
        >
          <Icon icon="gravity-ui:funnel" className="size-3.5 text-muted" />
          {t("filter.title")}
          {activeCount > 0 ? (
            <Badge size="sm" color="accent" variant="soft" className="num ml-0.5">
              {activeCount}
            </Badge>
          ) : null}
          <Icon icon="gravity-ui:chevron-down" className="size-3 text-muted" />
        </Popover.Trigger>
        {/*
          The popover scrolls itself: one run often has twenty-some types, and laying them all out would exceed the screen height.
          max-h is capped at 70vh; the overflow scrolls inside the popover without moving the page.
        */}
        <Popover.Content className="w-[min(30rem,92vw)]" placement="bottom start">
          <Popover.Dialog
            className="max-h-[70vh] overflow-y-auto p-3"
            data-testid="timeline-filter-panel"
          >
            <div className="flex flex-col gap-4">
              <Group title={t("filter.preset")}>
                <div className="flex flex-wrap items-center gap-2">
                  {FACET_PRESETS.map((preset) => {
                    const control = (
                      <ToggleButton
                        size="sm"
                        isSelected={presetActive(filter, preset.categories, preset.types)}
                        onChange={() => onApplyPreset(preset.id)}
                        data-testid={`filter-preset-${preset.id}`}
                      >
                        {t(preset.labelKey)}
                      </ToggleButton>
                    );
                    // What it does not cover must be said on the control itself (e.g. "system" does not include environment setup).
                    return preset.hintKey ? (
                      <Tooltip key={preset.id}>
                        <Tooltip.Trigger>{control}</Tooltip.Trigger>
                        <Tooltip.Content>{t(preset.hintKey)}</Tooltip.Content>
                      </Tooltip>
                    ) : (
                      <span key={preset.id}>{control}</span>
                    );
                  })}
                </div>
              </Group>

              <Group title={t("filter.category")}>
                <CategoryFilter
                  counts={stats.byCategory}
                  selected={filter.categories}
                  labels={categoryLabels}
                  onToggle={onToggleCategory}
                />
              </Group>

              <Group title={t("filter.domain")}>
                <div className="flex flex-wrap items-center gap-2">
                  {EVENT_DOMAINS.map((domain) => {
                    const count = stats.byDomain[domain];
                    if (count === 0) return null;
                    return (
                      <Tooltip key={domain}>
                        <Tooltip.Trigger>
                          <ToggleButton
                            size="sm"
                            isSelected={filter.domains.has(domain)}
                            onChange={() => onToggleDomain(domain)}
                            data-testid={`filter-domain-${domain}`}
                          >
                            {t(`enum.domain.${domain}`)}
                            <span className="ml-1.5 num text-xs opacity-70">{count}</span>
                          </ToggleButton>
                        </Tooltip.Trigger>
                        <Tooltip.Content>{t(`enum.domain.${domain}Hint`)}</Tooltip.Content>
                      </Tooltip>
                    );
                  })}
                </div>
              </Group>

              {/* Derived conditions: offered only when computable, and when offered, say clearly what is computed */}
              {stats.slowCount > 0 || stats.failedCount > 0 ? (
                <Group title={t("filter.derived")}>
                  <div className="flex flex-wrap items-center gap-2">
                    {stats.slowCount > 0 ? (
                      <Tooltip>
                        <Tooltip.Trigger>
                          <ToggleButton
                            size="sm"
                            isSelected={filter.slowOnly}
                            onChange={() => onSlowOnlyChange(!filter.slowOnly)}
                            data-testid="filter-slow"
                          >
                            <Icon icon="gravity-ui:hourglass" className="size-3.5" />
                            {t("filter.slowOnly", { seconds: SLOW_TOOL_MS / 1000 })}
                            <span className="ml-1.5 num text-xs opacity-70">
                              {stats.slowCount}
                            </span>
                          </ToggleButton>
                        </Tooltip.Trigger>
                        {/* This line is required: duration includes queueing and container resume; not saying so is misleading */}
                        <Tooltip.Content>{t("filter.slowOnlyHint")}</Tooltip.Content>
                      </Tooltip>
                    ) : null}

                    {stats.failedCount > 0 ? (
                      <ToggleButton
                        size="sm"
                        isSelected={filter.errorsOnly}
                        onChange={() => onErrorsOnlyChange(!filter.errorsOnly)}
                        data-testid="filter-failed"
                      >
                        <Icon icon="gravity-ui:triangle-exclamation" className="size-3.5" />
                        {t("filter.errorsOnly")}
                        <span className="ml-1.5 num text-xs opacity-70">{stats.failedCount}</span>
                      </ToggleButton>
                    ) : null}
                  </div>
                </Group>
              ) : null}

              {threads.length > 1 ? (
                <Group title={t("run.threads")}>
                  <ThreadSwitcher
                    threads={threads}
                    active={filter.threadId}
                    onChange={onThreadChange}
                  />
                </Group>
              ) : null}

              {typeGroups.length > 0 ? (
                <>
                  <Separator />
                  <Group title={t("filter.type")}>
                    <div className="flex flex-col gap-2">
                      {typeGroups.map((group) => (
                        <div key={group.domain} className="flex flex-wrap items-center gap-1.5">
                          <span className="w-16 shrink-0 text-xs text-muted">
                            {t(`enum.domain.${group.domain}`)}
                          </span>
                          {group.types.map((ty) => (
                            <ToggleButton
                              key={ty}
                              size="sm"
                              isSelected={filter.types.has(ty)}
                              onChange={() => {
                                const next = new Set(filter.types);
                                if (next.has(ty)) next.delete(ty);
                                else next.add(ty);
                                onSetTypes(next);
                              }}
                              data-testid={`filter-type-${ty}`}
                            >
                              {/* Labels are Ark's type strings directly: in a debugging view they are more precise than any translation,
                                  and match the type field in the raw payload at a glance. */}
                              <span className="font-mono text-xs">{ty}</span>
                              <span className="ml-1 num text-xs opacity-70">
                                {stats.byType[ty]}
                              </span>
                            </ToggleButton>
                          ))}
                        </div>
                      ))}
                    </div>
                  </Group>
                </>
              ) : null}
            </div>
          </Popover.Dialog>
        </Popover.Content>
      </Popover>

      {/*
        Selected conditions stay on the row. **This is not duplication**: the popover folds the controls; this shows the state.
        Hiding "what is filtered out right now" would make users look at an incomplete list without finding the reason.
      */}
      {activeChips.map((c) => (
        <ActiveChip
          key={c.key}
          label={c.label}
          tone={c.tone}
          onRemove={c.onRemove}
          testId={`filter-chip-${c.key}`}
        />
      ))}

      {/* No reset when the filters were never touched: a button that is always there but does nothing only makes people hesitate */}
      {isDirty ? (
        <Button size="sm" variant="ghost" onPress={onReset} data-testid="filter-reset">
          <Icon icon="gravity-ui:arrow-rotate-left" className="size-3.5" />
          {t("filter.reset")}
        </Button>
      ) : null}
    </div>
  );
}

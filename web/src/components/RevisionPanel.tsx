/**
 * Entity history panel: see the change records and switch back to an earlier version.
 *
 * # This is "undo", not "audit"
 *
 * The user's own words: "this feature is only for **recovering after a mistake**" and "it must not steal the spotlight".
 * Those two sentences decide every trade-off here:
 *
 *  - **It lives in a secondary tab on the detail page**, off the main path. The rule is "out of the way day to day, findable when something goes wrong":
 *    not a popover (you would have to know the button exists), and not a permanent sidebar (taking space every day).
 *  - **Only the latest N are kept** (20 on the backend, passed up via `maxKept`); the bottom of the list says so.
 *  - **Nothing is expanded by default**: you see a column of times + who changed it, and click to see the content.
 *
 * # ⚠️ Rollback is destructive, so you must see clearly before confirming
 *
 * "Pick a timestamp and roll back blindly" is the way this feature most easily causes a second mistake. So:
 *
 *  1. Each entry can be **expanded to see that version's content** (snapshot fetched on demand; the list endpoint does not include it);
 *  2. The confirm dialog **shows the key fields again**, instead of just asking "are you sure you want to roll back";
 *  3. A rollback **also creates a history entry itself** (done by the backend), so a wrong click can be undone;
 *     this must be said in the UI, otherwise users do not dare to click.
 *
 * # ⚠️ `no` is not `version`
 *
 * The `#5` shown in the list is the **snapshot number**, and it is **not the same thing** as the `v7` badge in the agent detail header
 * (`Agent.version`, a counter that goes up by one on every change), and they are not equal:
 * not every change creates a snapshot (no record when content did not change), and old snapshots over the limit are deleted.
 * So this deliberately does not say things like "5th change": that would be wrong. See types/domain.ts.
 */
import { useCallback, useState } from "react";
import { Button, Chip, Spinner } from "@heroui/react";
import { Icon } from "@iconify/react";

import { api } from "@/data/api";
import { formatDateTime, formatRelative } from "@/lib/format";
import { useAsyncData } from "@/lib/useAsyncData";
import { useUserProfiles } from "@/lib/useUserProfiles";
import { usePrefs } from "@/prefs/PrefsContext";
import type { Revision, RevisionKind, UserProfile } from "@/types/domain";
import { AsyncErrorBlock, Empty, LoadingBlock } from "@/components/Page";
import { ConfirmModal } from "@/components/FormModal";
import { UserAvatar } from "@/components/UserAvatar";

interface RevisionPanelProps {
  kind: RevisionKind;
  entityId: string;
  /**
   * Whether rollback is allowed. When `false`, only history is shown, with no button.
   *
   * ⚠️ **This only narrows the UI; it is not a security boundary**: the real gate is on the server
   * (`POST .../rollback` needs ActionWrite; viewers get 403). The frontend hides the button so
   * read-only users are not rejected after clicking, not because "the frontend checked, so the server need not".
   */
  canWrite: boolean;
  /** Called after a successful rollback, so the detail page can refresh itself with the latest entity. */
  onRolledBack: () => void;
}

export function RevisionPanel({ kind, entityId, canWrite, onRolledBack }: RevisionPanelProps) {
  const { t, language } = usePrefs();
  const fetchList = useCallback(
    () => api.listRevisions(kind, entityId),
    [kind, entityId],
  );
  const { data, error, isLoading, reload } = useAsyncData(fetchList, t("common.loadFailedUnknown"));
  const [confirming, setConfirming] = useState<Revision | null>(null);

  const revisions = data?.revisions ?? [];

  /*
    Display names and avatars of the people who made changes.

    # Why it is looked up here instead of passed in by the three detail pages

    The history list is **fetched by this component itself** (`api.listRevisions`), so the list of emails for "who changed these snapshots"
    is only known here. If callers passed profiles in, the agent / skill / schedule
    detail pages would each have to fetch the same list again to build the names: the same data fetched twice,
    and **all three places could miss it** (no error when missed; that place just falls back to initials again).
    The query lives where the data is, so all three kinds are wired up naturally.

    ⚠️ It must be called before the early returns: isLoading / error below have returns, and a hook written after them
    would break the Rules of Hooks (the hook count changes on the frame loading completes, and React throws).
    While loading, revisions is an empty array, and the hook sends no request for an empty list.
  */
  const profiles = useUserProfiles(revisions.map((r) => r.createdBy));

  if (isLoading) return <LoadingBlock />;
  if (error) return <AsyncErrorBlock error={error} onRetry={reload} />;

  // An existing entity with no history at all is normal, not an anomaly: say clearly "none yet"; do not leave it blank.
  if (revisions.length === 0) {
    return (
      <Empty
        icon="gravity-ui:clock"
        title={t("revision.emptyTitle")}
        description={t("revision.emptyDesc")}
      />
    );
  }

  return (
    <section className="flex flex-col gap-4" data-testid="revision-panel">
      <ul className="flex flex-col gap-2">
        {revisions.map((rev) => (
          <RevisionRow
            key={rev.id}
            kind={kind}
            entityId={entityId}
            rev={rev}
            language={language}
            canWrite={canWrite}
            profile={profiles[(rev.createdBy ?? "").toLowerCase()]}
            onRollback={() => setConfirming(rev)}
          />
        ))}
      </ul>

      {/* The limit is stated at the bottom of the list: when users see "only 5 entries", their most likely question is "were some lost".
          The number comes from the server (maxKept), not hard-coded in the frontend: hard-coded, it would rot as soon as the backend changes. */}
      <p className="text-xs text-muted" data-testid="revision-max-kept">
        {t("revision.maxKept", { n: String(data?.maxKept ?? 0) })}
      </p>

      <ConfirmModal
        isOpen={confirming !== null}
        onOpenChange={(open) => {
          if (!open) setConfirming(null);
        }}
        title={t("revision.rollbackTitle", { no: String(confirming?.no ?? "") })}
        description={t("revision.rollbackDesc")}
        confirmLabel={t("revision.rollback")}
        onConfirm={async () => {
          if (!confirming) return;
          await api.rollbackRevision(kind, entityId, confirming.id);
          setConfirming(null);
          // Both must be done: the list must refresh (the rollback **created a new history entry itself**),
          // and the detail page must refresh too (the entity content changed). Missing the former, the user does not see the new record,
          // thinks "the rollback left no trace", and does not dare to click a second time.
          reload();
          onRolledBack();
        }}
      />
    </section>
  );
}

interface RevisionRowProps {
  kind: RevisionKind;
  entityId: string;
  rev: Revision;
  language: string;
  canWrite: boolean;
  /**
   * Display name and avatar of the person who made the change. **Not found is normal** (that person never logged in → not in the users table);
   * just pass undefined to UserAvatar as the fallback; do not check for empty here.
   */
  profile?: UserProfile;
  onRollback: () => void;
}

function RevisionRow({
  kind,
  entityId,
  rev,
  language,
  canWrite,
  profile,
  onRollback,
}: RevisionRowProps) {
  const { t } = usePrefs();
  const [expanded, setExpanded] = useState(false);

  return (
    <li className="r-container border border-border" data-testid={`revision-row-${rev.no}`}>
      <div className="flex items-center gap-3 px-4 py-3">
        {/* Number. num (tabular digits) keeps a column of numbers aligned. */}
        <span className="num text-sm text-muted tabular-nums">#{rev.no}</span>

        <div className="flex min-w-0 flex-1 flex-col gap-0.5">
          <div className="flex items-center gap-2">
            {/* The name that version had at the time. **Deliberately the historical value**; it may differ from the entity's current name. */}
            <span className="truncate text-sm font-medium">{rev.summary}</span>
            {rev.reason === "rollback" ? (
              // Mark the one created by a rollback, otherwise users cannot tell "what I changed" from "what my rollback caused".
              <Chip size="sm" variant="soft" color="warning" data-testid="revision-reason-rollback">
                {t("revision.reasonRollback")}
              </Chip>
            ) : null}
          </div>
          <div className="flex items-center gap-2 text-xs text-muted">
            <time dateTime={rev.createdAt} title={formatDateTime(rev.createdAt, language)}>
              {formatRelative(rev.createdAt, language)}
            </time>
            {/* The createdBy field may be missing entirely (system paths have no human identity).
                UserAvatar falls back to "creator not recorded" itself; **do not write conditional rendering** here.

                ⚠️ Same for profile: if not found it falls back to the email initial; **do not check for empty either**.
                Leaving this prop out shows as "always only an initial": the feature seems to be there,
                it just never shows the real name and avatar, and reports no error. */}
            <UserAvatar
              email={rev.createdBy}
              profile={profile}
              label={t("revision.changedBy")}
              testId={`revision-author-${rev.no}`}
            />
          </div>
        </div>

        <Button
          variant="ghost"
          size="sm"
          onPress={() => setExpanded((v) => !v)}
          data-testid={`revision-toggle-${rev.no}`}
        >
          {expanded ? t("revision.hideDetail") : t("revision.showDetail")}
        </Button>
        {canWrite ? (
          <Button
            variant="outline"
            size="sm"
            onPress={onRollback}
            data-testid={`revision-rollback-${rev.no}`}
          >
            {t("revision.rollback")}
          </Button>
        ) : null}
      </div>

      {expanded ? <RevisionDetail kind={kind} entityId={entityId} revisionId={rev.id} /> : null}
    </li>
  );
}

/**
 * Fetches the content of this version on expand.
 *
 * **The list endpoint deliberately does not include snapshot** (a few KB each × 20), so it is fetched one at a time.
 * So "request on expand" is not an optimization; it is the only workable way.
 */
function RevisionDetail({
  kind,
  entityId,
  revisionId,
}: {
  kind: RevisionKind;
  entityId: string;
  revisionId: string;
}) {
  const { t } = usePrefs();
  const fetchOne = useCallback(
    () => api.getRevision(kind, entityId, revisionId),
    [kind, entityId, revisionId],
  );
  const { data, error, isLoading, reload } = useAsyncData(fetchOne, t("common.loadFailedUnknown"));

  if (isLoading) {
    return (
      <div className="flex items-center gap-2 border-t border-border px-4 py-3 text-sm text-muted">
        <Spinner size="sm" />
        {t("common.loading")}
      </div>
    );
  }
  if (error) {
    return (
      <div className="border-t border-border px-4 py-3">
        <AsyncErrorBlock error={error} onRetry={reload} />
      </div>
    );
  }

  return (
    <div className="border-t border-border px-4 py-3" data-testid="revision-detail">
      <SnapshotFields snapshot={data?.snapshot} />
      {kind === "skill" ? (
        // Skill attachments are not covered by rollback (SkillDraft has no files). **This must be said**,
        // otherwise users would think rollback restored the attachments too, and that reports no error.
        <p className="mt-2 flex items-center gap-1.5 text-xs text-muted">
          <Icon icon="gravity-ui:circle-info" className="size-3.5 shrink-0" />
          {t("revision.skillFilesNote")}
        </p>
      ) : null}
    </div>
  );
}

/**
 * Renders the snapshot as "field name → value".
 *
 * # Why generic rendering instead of three custom views
 *
 * The three entities have different field sets; three custom views would need three places changed in sync every time an entity gets a field,
 * and missing one shows as "that field is not visible in history", with no error. Generic rendering has no such upkeep:
 * whatever is in the snapshot is shown.
 *
 * The cost is that field names are the raw English (`system` instead of a translated label like "system prompt"). This is acceptable:
 * this panel is for "confirming this version is right when something goes wrong", and the person looking knows what they are looking for.
 */
function SnapshotFields({ snapshot }: { snapshot: unknown }) {
  const { t } = usePrefs();
  if (snapshot === null || snapshot === undefined || typeof snapshot !== "object") {
    return <p className="text-sm text-muted">{t("revision.snapshotUnavailable")}</p>;
  }
  const entries = Object.entries(snapshot as Record<string, unknown>);
  if (entries.length === 0) {
    return <p className="text-sm text-muted">{t("revision.snapshotUnavailable")}</p>;
  }
  return (
    <dl className="flex flex-col gap-2">
      {entries.map(([key, value]) => (
        <div key={key} className="flex flex-col gap-0.5 sm:flex-row sm:gap-3">
          <dt className="shrink-0 text-xs text-muted sm:w-36">{key}</dt>
          <dd className="min-w-0 flex-1 whitespace-pre-wrap break-words text-sm">
            {renderValue(value)}
          </dd>
        </div>
      ))}
    </dl>
  );
}

/** Renders the value of one snapshot field as a human-readable line (or a few lines). */
function renderValue(value: unknown): string {
  if (value === null || value === undefined) return "—";
  if (typeof value === "string") return value === "" ? "—" : value;
  if (typeof value === "number" || typeof value === "boolean") return String(value);
  if (Array.isArray(value)) {
    if (value.length === 0) return "—";
    // Array elements may be scalars (skillIds) or objects (envs); both must be readable.
    return value.map((v) => (typeof v === "object" ? JSON.stringify(v) : String(v))).join("、");
  }
  return JSON.stringify(value);
}

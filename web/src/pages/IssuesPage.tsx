/**
 * Issue list page `/w/:key/issues`.
 *
 * # It is a different thing from "problem feedback"
 *
 * Feedback (`components/FeedbackButton.tsx`) is a machine capture reported with one click on
 * error; users cannot see
 * what they reported. This page is human-written entities that can be searched, edited and
 * discussed. The two are deliberately not linked.
 *
 * # The five-branch loading shape is the same across the site
 *
 * `isLoading → error → !data → empty → content`. **Do not use `data === null` to check loading**:
 * on failure data is also null, which is exactly what causes "the permanent skeleton" (see
 * lib/useAsyncData.ts).
 */

import { useCallback, useState } from "react";
import { Link } from "react-router-dom";
import { Button, Chip, Label, ListBox, Select } from "@heroui/react";
import { api } from "@/data/api";
import { AsyncErrorBlock, Empty, ListSkeleton, PageBody, PageHeader } from "@/components/Page";
import { FormModal } from "@/components/FormModal";
import { IssuePriorityChip, IssuePrioritySelect, IssueStatusChip, IssueStatusSelect } from "@/components/IssueBits";
import { UserAvatar, profileKey } from "@/components/UserAvatar";
import { useAsyncData } from "@/lib/useAsyncData";
import { useUserProfiles } from "@/lib/useUserProfiles";
import { formatRelative } from "@/lib/format";
import { usePrefs } from "@/prefs/PrefsContext";
import type { MessageKey } from "@/prefs/messages";
import { useWorkspace, useWorkspacePath } from "@/app/WorkspaceGate";
import {
  DEFAULT_ISSUE_PRIORITY,
  ISSUE_STATUSES,
  type Issue,
  type IssueDraft,
  type IssueStatus,
  type UserProfile,
} from "@/types/domain";

/** Initial values for the new form. Status and priority get default levels, not left empty. */
function emptyDraft(): IssueDraft {
  return { title: "", body: "", status: "open", priority: DEFAULT_ISSUE_PRIORITY };
}

export function IssuesPage() {
  const { t, language } = usePrefs();
  const { workspace } = useWorkspace();
  const wp = useWorkspacePath();

  const [creating, setCreating] = useState(false);
  const [draft, setDraft] = useState<IssueDraft>(emptyDraft());
  const [formError, setFormError] = useState<string | null>(null);
  // Status filter. Empty string means all, matching the backend's default of no filter when
  // `?status=` is missing.
  const [filter, setFilter] = useState<IssueStatus | "">("");

  const fetch = useCallback(
    () => api.listIssues(workspace.id, filter || undefined),
    [workspace.id, filter],
  );
  const { data: issues, error, isLoading, reload } = useAsyncData(
    fetch,
    t("common.loadFailedUnknown"),
  );

  /*
    Look up display names and avatars for all authors on the page at once. **Do not query per
    row**: that is N+1
    (see lib/useUserProfiles.ts). Passing an array literal is safe: the hook normalizes it into
    a stable string key before comparing, so it does not query again if the content is unchanged.
  */
  const profiles = useUserProfiles((issues ?? []).map((i) => i.author));

  function openCreate() {
    setDraft(emptyDraft());
    setFormError(null);
    setCreating(true);
  }

  return (
    <PageBody>
      <PageHeader
        title={t("issue.title")}
        description={t("issue.listDesc")}
        actions={
          <Button onPress={openCreate} data-testid="new-issue">
            {t("issue.new")}
          </Button>
        }
      />

      {/*
        Filter. **Not full width**: it is an inline control; stretching it across the row makes
        people think it is a form.
        It sits above the list rather than in PageHeader's actions: that slot is for the main CTA.
      */}
      <div className="flex items-center gap-3">
        <Select
          selectedKey={filter || "all"}
          onSelectionChange={(k) => setFilter(k === "all" ? "" : (String(k) as IssueStatus))}
          className="max-w-48"
        >
          <Label className="sr-only">{t("issue.fieldStatus")}</Label>
          <Select.Trigger data-testid="issue-filter-status">
            <Select.Value />
            <Select.Indicator />
          </Select.Trigger>
          <Select.Popover>
            <ListBox>
              <ListBox.Item id="all" textValue={t("issue.filterAll")}>
                {t("issue.filterAll")}
              </ListBox.Item>
              {ISSUE_STATUSES.map((s) => (
                <ListBox.Item
                  key={s}
                  id={s}
                  textValue={t(`enum.issueStatus.${s}` as MessageKey)}
                >
                  {t(`enum.issueStatus.${s}` as MessageKey)}
                </ListBox.Item>
              ))}
            </ListBox>
          </Select.Popover>
        </Select>
      </div>

      {isLoading ? (
        <ListSkeleton />
      ) : error ? (
        <AsyncErrorBlock error={error} onRetry={reload} />
      ) : !issues ? null : issues.length === 0 ? (
        <Empty
          icon="gravity-ui:bug"
          title={t("issue.emptyTitle")}
          description={t("issue.emptyDesc")}
          action={<Button onPress={openCreate}>{t("issue.new")}</Button>}
        />
      ) : (
        <ul className="flex flex-col gap-2" data-testid="issue-list">
          {issues.map((issue) => (
            <IssueRow
              key={issue.id}
              issue={issue}
              to={wp(`/issues/${issue.id}`)}
              locale={language}
              // ⚠️ Keys are **lowercase** emails (normalized by useUserProfiles), so use profileKey().
              // Using profiles[issue.author] directly silently misses when the email has uppercase,
              // looking the same as
              // "no profile passed" (only initials left), but harder to debug.
              profile={profiles[profileKey(issue.author)]}
            />
          ))}
        </ul>
      )}

      <FormModal
        isOpen={creating}
        onOpenChange={setCreating}
        title={t("issue.createTitle")}
        submitLabel={t("common.create")}
        testId="issue-create-modal"
        onSubmit={async () => {
          // Same check as the backend: at least one of title and body must be filled.
          // The text uses the i18n string, which matches the backend's errIssueEmpty() word for word.
          if (!draft.title.trim() && !draft.body.trim()) {
            setFormError(t("issue.requireTitleOrBody"));
            return false;
          }
          setFormError(null);
          await api.createIssue(workspace.id, draft);
          await reload();
        }}
      >
        <IssueForm draft={draft} setDraft={setDraft} error={formError} />
      </FormModal>
    </PageBody>
  );
}

/** One row in the list. The whole row is clickable and goes to the detail page. */
function IssueRow({
  issue,
  to,
  locale,
  profile,
}: {
  issue: Issue;
  to: string;
  locale: string;
  profile?: UserProfile;
}) {
  const { t } = usePrefs();
  return (
    <li>
      <Link
        to={to}
        className="flex items-center gap-3 r-inset bg-surface p-4 no-underline shadow-surface"
        data-testid={`issue-row-${issue.id}`}
      >
        {/*
          Author avatar. **The list page shows only the avatar, with the name in a tooltip**,
          deliberately different from the detail page:
          a screen here has a dozen rows; spelling out "created by xxx" on every row makes the
          author name compete with the title,
          and people scanning the list look for the title and status. The avatar keeps the "who
          filed it" information
          while taking only one cell of width; same as the creator avatar on AgentsPage (also the
          default size-6 there).

          It sits where the old text was (start of the row, left of the title), so after switching
          from text to avatar
          the "author" information is still in the same place and does not need to be found again.
        */}
        <UserAvatar
          email={issue.author}
          profile={profile}
          label={t("issue.authorLabel")}
          testId={`issue-author-${issue.id}`}
        />

        <div className="flex min-w-0 flex-1 flex-col gap-1">
          <span className="truncate text-sm font-medium text-foreground">{issue.title}</span>
          <span className="text-xs text-muted">
            {t("issue.updatedAt", { time: formatRelative(issue.updatedAt, locale) })}
          </span>
        </div>

        {/* Counts only show when non-zero: a row of "0 comments 0 attachments" is pure noise. */}
        {issue.commentCount > 0 ? (
          <Chip size="sm" variant="soft">
            <Chip.Label className="tabular-nums">
              {t("issue.countComments", { count: issue.commentCount })}
            </Chip.Label>
          </Chip>
        ) : null}
        {issue.attachmentCount > 0 ? (
          <Chip size="sm" variant="soft">
            <Chip.Label className="tabular-nums">
              {t("issue.countAttachments", { count: issue.attachmentCount })}
            </Chip.Label>
          </Chip>
        ) : null}

        <IssuePriorityChip priority={issue.priority} />
        <IssueStatusChip status={issue.status} />
      </Link>
    </li>
  );
}

/**
 * Create/edit form. Shared by the list page and the detail page.
 *
 * ⚠️ **Neither title nor body is marked isRequired**: their rule is "at least one is filled",
 * and isRequired would show a required star on both fields, which is a lie;
 * users would think both must be filled. The rule is expressed by the single error text on submit.
 */
export function IssueForm({
  draft,
  setDraft,
  error,
}: {
  draft: IssueDraft;
  setDraft: (d: IssueDraft) => void;
  error: string | null;
}) {
  const { t } = usePrefs();

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-col gap-1.5">
        <Label htmlFor="issue-title-input">{t("issue.fieldTitle")}</Label>
        <input
          id="issue-title-input"
          value={draft.title}
          onChange={(e) => setDraft({ ...draft, title: e.target.value })}
          placeholder={t("issue.fieldTitlePlaceholder")}
          data-testid="issue-title"
          className="w-full r-inset border border-field-border bg-surface-secondary px-3 py-2 text-sm outline-none focus:border-accent"
        />
        <p className="text-xs text-muted">{t("issue.fieldTitleDesc")}</p>
      </div>

      <div className="flex flex-col gap-1.5">
        <Label htmlFor="issue-body-input">{t("issue.fieldBody")}</Label>
        {/*
          Use a native textarea rather than HeroUI's TextArea: we need onPaste and a ref here
          for paste upload (the detail page version), and TextArea passes native props through with
          no extra benefit.
          Both places keep the same shape, so the same input does not look different on two pages.
        */}
        <textarea
          id="issue-body-input"
          value={draft.body}
          onChange={(e) => setDraft({ ...draft, body: e.target.value })}
          placeholder={t("issue.fieldBodyPlaceholder")}
          rows={8}
          data-testid="issue-body"
          className="w-full r-inset border border-field-border bg-surface-secondary px-3 py-2 text-sm outline-none focus:border-accent"
        />
        <p className="text-xs text-muted">{t("issue.fieldBodyDesc")}</p>
      </div>

      {/* On narrow screens it must be one column: on a 402px-wide phone the two dropdowns get less than 170px each,
          and values like "In progress" or "Priority P2" cannot even show their labels. Above the sm breakpoint (640px) it is still two columns,
          and the desktop look does not change at all. */}
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
        <IssueStatusSelect
          value={draft.status ?? "open"}
          onChange={(status) => setDraft({ ...draft, status })}
          testId="issue-status-select"
        />
        <IssuePrioritySelect
          value={draft.priority ?? DEFAULT_ISSUE_PRIORITY}
          onChange={(priority) => setDraft({ ...draft, priority })}
          testId="issue-priority-select"
        />
      </div>

      {error ? (
        <p className="text-sm text-danger" role="alert" data-testid="issue-form-error">
          {error}
        </p>
      ) : null}
    </div>
  );
}

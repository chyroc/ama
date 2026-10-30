/**
 * Issue detail page `/w/:key/issues/:issueId`: body + attachments + discussion.
 *
 * # The body uses the existing MarkdownMessage; md-box is not imported anew
 *
 * The top of `components/MarkdownBody.tsx` says "apart from MarkdownMessage.tsx, no second
 * place should import this file": one static import pulls the 160 KB gzip core back into the main
 * chunk.
 * Here we use `MarkdownMessage` (a React.lazy shell); theme overrides are already handled by the
 * **unlayered** `.flow-markdown-body.md-message` rule in `index.css`,
 * so not a single line of new CSS is needed.
 *
 * # Each of the four end states renders separately
 *
 * `isLoading` / load failed / **issue does not exist** (data is null) / normal.
 * The third needs its own empty state: "does not exist" and "failed to load" are two different
 * things; mixing them would leave the user
 * clicking a "retry" button again and again for an issue that has been deleted.
 */

import { useCallback, useRef, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { Button, Label, Tooltip } from "@heroui/react";
import { Icon } from "@iconify/react";
import { api } from "@/data/api";
import {
  AsyncErrorBlock,
  Empty,
  ListSkeleton,
  PageBody,
  PageHeader,
  SectionTitle,
} from "@/components/Page";
import { ConfirmModal, FormModal } from "@/components/FormModal";
import { MarkdownMessage } from "@/components/MarkdownMessage";
import {
  AttachmentDropZone,
  AttachmentList,
  IssuePriorityChip,
  IssuePrioritySelect,
  IssueStatusChip,
  IssueStatusSelect,
} from "@/components/IssueBits";
import { IssueForm } from "@/pages/IssuesPage";
import { UserAvatar, displayNameOf, profileKey } from "@/components/UserAvatar";
import { useAsyncData } from "@/lib/useAsyncData";
import { useUserProfiles } from "@/lib/useUserProfiles";
import { formatDateTime, formatRelative } from "@/lib/format";
import { usePrefs } from "@/prefs/PrefsContext";
import { useWorkspacePath } from "@/app/WorkspaceGate";
import type { Issue, IssueComment, IssueDraft } from "@/types/domain";

export function IssueDetailPage() {
  const { t, language } = usePrefs();
  const { issueId = "" } = useParams<{ issueId: string }>();
  const navigate = useNavigate();
  const wp = useWorkspacePath();

  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState<IssueDraft>({ title: "", body: "" });
  const [formError, setFormError] = useState<string | null>(null);
  const [deleting, setDeleting] = useState(false);

  const fetch = useCallback(async () => {
    const issue = await api.getIssue(issueId);
    // If the issue does not exist, do not query its sub-resources: each would return an empty
    // array, looking like "the issue exists but has no content".
    if (!issue) return { issue: null, comments: [], attachments: [] };
    const [comments, attachments] = await Promise.all([
      api.listIssueComments(issueId),
      api.listIssueAttachments(issueId),
    ]);
    return { issue, comments, attachments };
  }, [issueId]);

  const { data, error, isLoading, reload } = useAsyncData(fetch, t("common.loadFailedUnknown"));

  /*
    The author's display name and avatar.

    ⚠️ **Must be written before the early returns below**: hooks can only be at the top level of
    the component body.
    Moving it after `if (isLoading)` would call one extra hook on the "loaded" frame, and React
    reports
    "Rendered more hooks than during the previous render" and blanks the page, while typecheck
    cannot catch it (same as the useT() note in docs/guides/i18n.md).

    Only one person to look up, but it still uses the batch hook rather than sending its own
    request: that way "fall back to initials if not found"
    and "race guard" behave exactly like the other two places, with nothing to rethink here.
  */
  const profiles = useUserProfiles([data?.issue?.author]);

  if (isLoading) {
    return (
      <PageBody>
        <ListSkeleton />
      </PageBody>
    );
  }
  if (error) {
    return (
      <PageBody>
        <AsyncErrorBlock error={error} onRetry={reload} />
      </PageBody>
    );
  }
  // Separate "does not exist" from "failed to load": the former gets an empty state + back to
  // list, with no retry button
  // (it still does not exist after ten thousand retries).
  if (!data?.issue) {
    return (
      <PageBody>
        <Empty
          icon="gravity-ui:bug"
          title={t("issue.emptyTitle")}
          description={t("issue.emptyDesc")}
          action={<Button onPress={() => navigate(wp("/issues"))}>{t("issue.backToList")}</Button>}
        />
      </PageBody>
    );
  }

  const issue: Issue = data.issue;
  // The visible name next to the avatar. When no profile is found it is the email, never an empty
  // string;
  // an empty string only happens when "no author was recorded at all", and then the name is not
  // rendered (see the comment below).
  const authorName = displayNameOf(issue.author, profiles[profileKey(issue.author)]);

  function openEdit() {
    setDraft({
      title: issue.title,
      body: issue.body,
      status: issue.status,
      priority: issue.priority,
    });
    setFormError(null);
    setEditing(true);
  }

  /** Change only status or priority: no dialog, edit in place and save at once. */
  async function patchMeta(patch: Partial<IssueDraft>) {
    await api.updateIssue(issue.id, {
      title: issue.title,
      body: issue.body,
      status: issue.status,
      priority: issue.priority,
      ...patch,
    });
    await reload();
  }

  return (
    <PageBody>
      <PageHeader
        above={
          <Button
            variant="ghost"
            size="sm"
            onPress={() => navigate(wp("/issues"))}
            data-testid="issue-back"
          >
            <Icon icon="gravity-ui:arrow-left" className="size-4" />
            {t("issue.backToList")}
          </Button>
        }
        title={issue.title}
        description={
          <span className="flex flex-wrap items-center gap-2">
            <IssueStatusChip status={issue.status} />
            <IssuePriorityChip priority={issue.priority} />
            {/*
              Author: avatar + name. **The detail page deliberately shows one more thing than the
              list page, the name**:
              it is the most important ownership info on the page ("who filed this issue"), while
              the list page has
              a dozen rows per screen where the name would compete with the title, so there only the
              avatar stays, with the name in a tooltip.

              The name uses displayNameOf rather than issue.author: when that person has never
              logged in it
              falls back to the email itself, the same rule as what the avatar tooltip shows.
              An empty string is not rendered: "we do not know who" is honestly shown by the
              question mark on the avatar side,
              and drawing an empty span on the text side would only look like broken rendering.
            */}
            <span className="flex items-center gap-1.5 text-xs text-muted">
              <UserAvatar
                email={issue.author}
                profile={profiles[profileKey(issue.author)]}
                label={t("issue.authorLabel")}
                testId="issue-author"
              />
              {authorName ? <span data-testid="issue-author-name">{authorName}</span> : null}
            </span>
            <span className="text-xs text-muted">
              {t("issue.createdAt", { time: formatDateTime(issue.createdAt, language) })}
            </span>
          </span>
        }
        actions={
          <>
            <Button variant="ghost" onPress={openEdit} data-testid="issue-edit">
              {t("issue.editAction")}
            </Button>
            <Button
              variant="danger-soft"
              onPress={() => setDeleting(true)}
              data-testid="issue-delete"
            >
              {t("common.delete")}
            </Button>
          </>
        }
      />

      {/*
        In-place pickers for status and priority. Changing these two is the most frequent action
        (triage, closing);
        opening the edit dialog every time is too heavy.
      */}
      {/* One column on narrow screens, same as the IssuesPage form. max-w-md keeps it from filling the row on desktop,
          but that cap does nothing on a 402px phone: the real squeeze comes from grid-cols-2 itself. */}
      <div className="grid max-w-md grid-cols-1 gap-4 sm:grid-cols-2">
        <IssueStatusSelect
          value={issue.status}
          onChange={(status) => void patchMeta({ status })}
          testId="issue-status-inline"
        />
        <IssuePrioritySelect
          value={issue.priority}
          onChange={(priority) => void patchMeta({ priority })}
          testId="issue-priority-inline"
        />
      </div>

      {/* Body */}
      <section className="flex flex-col gap-3 r-container bg-surface p-6 shadow-surface">
        {issue.body.trim() ? (
          <MarkdownMessage>{issue.body}</MarkdownMessage>
        ) : (
          <p className="text-sm text-muted" data-testid="issue-no-body">
            {t("issue.noBody")}
          </p>
        )}
      </section>

      {/* Attachments */}
      <section className="flex flex-col gap-3">
        <SectionTitle title={t("issue.attachments")} />
        <AttachmentList
          attachments={data.attachments}
          onDelete={async (att) => {
            await api.deleteIssueAttachment(att.id);
            await reload();
          }}
        />
        <AttachmentDropZone issueId={issue.id} onUploaded={() => void reload()} />
      </section>

      {/* Discussion */}
      <CommentSection issueId={issue.id} comments={data.comments} onChanged={reload} />

      <FormModal
        isOpen={editing}
        onOpenChange={setEditing}
        title={t("issue.editTitle")}
        submitLabel={t("common.save")}
        testId="issue-edit-modal"
        onSubmit={async () => {
          if (!draft.title.trim() && !draft.body.trim()) {
            setFormError(t("issue.requireTitleOrBody"));
            return false;
          }
          setFormError(null);
          await api.updateIssue(issue.id, draft);
          await reload();
        }}
      >
        <IssueForm draft={draft} setDraft={setDraft} error={formError} />
      </FormModal>

      <ConfirmModal
        isOpen={deleting}
        onOpenChange={setDeleting}
        title={t("issue.deleteTitle", { name: issue.title })}
        description={t("issue.deleteDesc")}
        confirmLabel={t("issue.deleteAction")}
        onConfirm={async () => {
          await api.deleteIssue(issue.id);
          navigate(wp("/issues"));
        }}
      />
    </PageBody>
  );
}

/** Discussion area: list + compose box. */
function CommentSection({
  issueId,
  comments,
  onChanged,
}: {
  issueId: string;
  comments: IssueComment[];
  onChanged: () => Promise<void> | void;
}) {
  const { t, language } = usePrefs();
  const [body, setBody] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [editingId, setEditingId] = useState<string | null>(null);
  const [editBody, setEditBody] = useState("");
  const [deleting, setDeleting] = useState<IssueComment | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const textareaRef = useRef<HTMLTextAreaElement>(null);

  /*
    Commenters' display names and avatars. **The whole discussion is looked up at once** (deduped
    inside the hook);
    one query per comment would be N+1; one person replying ten times still counts as one email.
  */
  const profiles = useUserProfiles(comments.map((c) => c.author));

  /**
   * Pasted images are uploaded directly, and Markdown image syntax is inserted at the cursor.
   *
   * ⚠️ **Must preventDefault**: otherwise the browser pastes the image's file name as text,
   * so an extra line "screenshot.png" appears in the input while the user thinks the image was
   * pasted.
   *
   * The insert position uses selectionStart/End rather than appending at the end: users often
   * paste an image halfway through writing,
   * and appending at the end would put the image after the whole paragraph.
   */
  const onPaste = useCallback(
    async (e: React.ClipboardEvent<HTMLTextAreaElement>) => {
      const file = Array.from(e.clipboardData.files).find((f) => f.type.startsWith("image/"));
      if (!file) return;
      e.preventDefault();

      setError(null);
      try {
        const att = await api.uploadIssueAttachment(issueId, file);
        const el = textareaRef.current;
        const snippet = `![${att.filename}](${att.url})`;
        setBody((cur) => {
          const start = el?.selectionStart ?? cur.length;
          const end = el?.selectionEnd ?? cur.length;
          return cur.slice(0, start) + snippet + cur.slice(end);
        });
        setNotice(t("issue.attachmentPasted"));
        await onChanged();
      } catch (err) {
        setError(err instanceof Error ? err.message : t("common.actionFailed"));
      }
    },
    [issueId, onChanged, t],
  );

  async function submit() {
    if (!body.trim()) {
      setError(t("issue.commentRequired"));
      return;
    }
    setSubmitting(true);
    setError(null);
    try {
      await api.createIssueComment(issueId, body);
      setBody("");
      setNotice(null);
      await onChanged();
    } catch (e) {
      setError(e instanceof Error ? e.message : t("common.actionFailed"));
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <section className="flex flex-col gap-4">
      <SectionTitle title={t("issue.comments")} />

      {comments.length === 0 ? (
        <p className="text-sm text-muted" data-testid="issue-comment-empty">
          {t("issue.commentEmpty")}
        </p>
      ) : (
        <ul className="flex flex-col gap-3" data-testid="issue-comment-list">
          {comments.map((c) => (
            <li
              key={c.id}
              className="flex flex-col gap-2 r-container bg-surface p-4 shadow-surface"
              data-testid={`issue-comment-${c.id}`}
            >
              <div className="flex items-center gap-2">
                {/*
                  Commenter: avatar + name. **The name keeps font-medium**: it was already the
                  visual anchor of this row
                  (time and "edited" are both text-xs text-muted); the avatar was added in front of
                  it, not in place of it.
                  A screen of discussion may have a dozen comments from different people,
                  and "who said it" must be readable without hover, so this matches the detail
                  page author and is
                  deliberately different from the list page (avatar only).

                  The avatar uses the default size-6: in proportion with the text in that row
                  (text-sm / text-xs),
                  and the same level as the other two places; an inconsistency would only show in
                  screenshots.
                */}
                <UserAvatar
                  email={c.author}
                  profile={profiles[profileKey(c.author)]}
                  label={t("issue.commentAuthorLabel")}
                  testId={`issue-comment-author-${c.id}`}
                />
                <span className="text-sm font-medium">
                  {displayNameOf(c.author, profiles[profileKey(c.author)])}
                </span>
                <span className="text-xs text-muted">
                  {formatRelative(c.createdAt, language)}
                </span>
                {/* createdAt not equal to updatedAt means it was edited. */}
                {c.updatedAt !== c.createdAt ? (
                  <span className="text-xs text-muted" data-testid="issue-comment-edited">
                    （{t("issue.commentEdited")}）
                  </span>
                ) : null}

                <span className="ml-auto flex items-center gap-1">
                  <Tooltip>
                    <Tooltip.Trigger>
                      <Button
                        size="sm"
                        variant="ghost"
                        isIconOnly
                        aria-label={t("issue.commentEditAction")}
                        onPress={() => {
                          setEditingId(c.id);
                          setEditBody(c.body);
                        }}
                        data-testid={`issue-comment-edit-${c.id}`}
                      >
                        <Icon icon="gravity-ui:pencil" className="size-4" />
                      </Button>
                    </Tooltip.Trigger>
                    <Tooltip.Content>{t("issue.commentEditAction")}</Tooltip.Content>
                  </Tooltip>
                  <Tooltip>
                    <Tooltip.Trigger>
                      <Button
                        size="sm"
                        variant="ghost"
                        isIconOnly
                        aria-label={t("issue.commentDeleteAction")}
                        onPress={() => setDeleting(c)}
                        data-testid={`issue-comment-delete-${c.id}`}
                      >
                        <Icon icon="gravity-ui:trash-bin" className="size-4" />
                      </Button>
                    </Tooltip.Trigger>
                    <Tooltip.Content>{t("issue.commentDeleteAction")}</Tooltip.Content>
                  </Tooltip>
                </span>
              </div>

              {editingId === c.id ? (
                <div className="flex flex-col gap-2">
                  <textarea
                    value={editBody}
                    onChange={(e) => setEditBody(e.target.value)}
                    rows={4}
                    data-testid={`issue-comment-editor-${c.id}`}
                    className="w-full r-inset border border-field-border bg-surface-secondary px-3 py-2 text-sm outline-none focus:border-accent"
                  />
                  <div className="flex gap-2">
                    <Button
                      size="sm"
                      onPress={async () => {
                        await api.updateIssueComment(c.id, editBody);
                        setEditingId(null);
                        await onChanged();
                      }}
                      data-testid={`issue-comment-save-${c.id}`}
                    >
                      {t("issue.commentSave")}
                    </Button>
                    <Button size="sm" variant="ghost" onPress={() => setEditingId(null)}>
                      {t("issue.commentCancel")}
                    </Button>
                  </div>
                </div>
              ) : (
                <MarkdownMessage>{c.body}</MarkdownMessage>
              )}
            </li>
          ))}
        </ul>
      )}

      {/* Compose box */}
      <div className="flex flex-col gap-2">
        <Label htmlFor="issue-comment-input" className="sr-only">
          {t("issue.comments")}
        </Label>
        <textarea
          id="issue-comment-input"
          ref={textareaRef}
          value={body}
          onChange={(e) => setBody(e.target.value)}
          onPaste={(e) => void onPaste(e)}
          placeholder={t("issue.commentPlaceholder")}
          rows={4}
          data-testid="issue-comment-input"
          className="w-full r-inset border border-field-border bg-surface-secondary px-3 py-2 text-sm outline-none focus:border-accent"
        />
        {notice ? (
          <p className="text-xs text-muted" data-testid="issue-comment-notice">
            {notice}
          </p>
        ) : null}
        {error ? (
          <p className="text-sm text-danger" role="alert" data-testid="issue-comment-error">
            {error}
          </p>
        ) : null}
        <div className="flex justify-end">
          <Button
            onPress={() => void submit()}
            isPending={submitting}
            data-testid="issue-comment-submit"
          >
            {t("issue.commentSubmit")}
          </Button>
        </div>
      </div>

      <ConfirmModal
        isOpen={deleting !== null}
        onOpenChange={(open) => !open && setDeleting(null)}
        title={t("issue.commentDeleteTitle")}
        description={t("issue.commentDeleteDesc")}
        confirmLabel={t("common.delete")}
        onConfirm={async () => {
          if (deleting) await api.deleteIssueComment(deleting.id);
          setDeleting(null);
          await onChanged();
        }}
      />
    </section>
  );
}

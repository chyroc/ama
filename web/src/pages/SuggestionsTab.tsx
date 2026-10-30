/**
 * Product suggestions: where users give feedback on **the AMA platform itself**.
 *
 * # It is not the same as the other two "something is wrong" things
 *
 *   problem feedback (lib/feedback.ts)  a stream **reported automatically by machine** when an
 *                                       error happens; the user types nothing,
 *                                       and cannot see what they reported
 *   issues (IssuesPage)                 human-written **workspace-level** to-dos, belonging to one
 *                                       workspace
 *   this component                      human-written product feedback, **platform-level**, with no
 *                                       workspace concept
 *
 * ⚠️ All three have similar names, and the repo already has a whole doc section telling the
 * first two apart
 * (server/core/README-issue.md). Read README-suggestion.md before changing this.
 *
 * # Why in "global settings" and not inside a workspace
 *
 * It is account-level, like API Key and authorization management: **you see the same ones in
 * any workspace** (what you filed is
 * what you filed, regardless of which workspace you were in). Hanging it under a workspace would
 * also mean "newcomers who have not joined any workspace"
 * cannot give feedback, and they are exactly the ones with the most to say.
 *
 * # There are two UIs, decided by viewerIsStaff
 *
 *   ordinary users  see only their own; can file, edit title and body, and follow up
 *   platform staff  see all (the list has an extra author column); can reply and change status
 *
 * ⚠️ **This decision comes from the server; the frontend must not compute it**: the staff list is
 * server config
 * (feedback.readers), which the frontend cannot get at all. Guessing would give "the UI has a
 * change-status
 * button, and clicking it returns 403".
 */

import { useCallback, useState } from "react";
import {
  Button,
  Card,
  Chip,
  Input,
  Label,
  ListBox,
  Select,
  Separator,
  TextArea,
  TextField,
} from "@heroui/react";
import { Icon } from "@iconify/react";
import { api } from "@/data/api";
import { AsyncErrorBlock, Empty, InlineError, ListSkeleton, SectionTitle } from "@/components/Page";
import { ConfirmModal, FormModal } from "@/components/FormModal";
import { MarkdownMessage } from "@/components/MarkdownMessage";
import { AttachmentList } from "@/components/IssueBits";
import { formatRelative } from "@/lib/format";
import { useAsyncData, useWriteError } from "@/lib/useAsyncData";
import { usePrefs } from "@/prefs/PrefsContext";
import {
  SUGGESTION_STATUSES,
  type Suggestion,
  type SuggestionAttachment,
  type SuggestionReply,
  type SuggestionStatus,
} from "@/types/domain";

/**
 * Status → Chip color.
 *
 * **Resolved uses success** (a solved problem is good); unresolved uses default (neutral:
 * it is just "not handled yet", not a fault).
 *
 * ⚠️ Do not make unresolved danger: that would make every new suggestion look like an incident.
 * Using the wrong semantic color is worse than none (same judgment as STATUS_COLOR in IssueBits).
 */
const STATUS_COLOR: Record<SuggestionStatus, "default" | "success"> = {
  open: "default",
  resolved: "success",
};

function statusLabel(s: SuggestionStatus, t: (k: never) => string): string {
  // Text keys are enum.suggestionStatus.* in messages.ts
  return t(`enum.suggestionStatus.${s}` as never);
}

function SuggestionStatusChip({ status }: { status: SuggestionStatus }) {
  const { t } = usePrefs();
  return (
    <Chip size="sm" color={STATUS_COLOR[status]} variant="soft" data-testid={`suggestion-status-${status}`}>
      <Chip.Label>{statusLabel(status, t as never)}</Chip.Label>
    </Chip>
  );
}

export function SuggestionsTab() {
  const { t, language } = usePrefs();

  // The list and "am I platform staff" come back together; the latter is computed by the server,
  // see the file header.
  //
  // ⚠️ The dependency array is empty: this list is unrelated to the workspace (same as ApiKeysTab).
  const fetchPage = useCallback(() => api.listSuggestions(), []);
  const {
    data: page,
    error: loadError,
    isLoading,
    reload: load,
  } = useAsyncData(fetchPage, t("suggestion.loadFailed"));

  const [composing, setComposing] = useState(false);
  const [title, setTitle] = useState("");
  const [body, setBody] = useState("");
  // Attachments of the one just submitted (it has an id only after submit, so images are "submit
  // first, then upload").
  const [openDetail, setOpenDetail] = useState<Suggestion | null>(null);
  const [deleting, setDeleting] = useState<Suggestion | null>(null);
  const { error: writeError, fail, clear } = useWriteError(t("common.actionFailed"));

  if (isLoading) {
    return (
      <Card>
        <Card.Content className="pt-6">
          <ListSkeleton rows={3} />
        </Card.Content>
      </Card>
    );
  }
  if (loadError) {
    return (
      <Card>
        <Card.Content className="pt-6">
          <AsyncErrorBlock error={loadError} onRetry={load} />
        </Card.Content>
      </Card>
    );
  }
  if (page === null) return null;

  const isStaff = page.viewerIsStaff;

  return (
    <div className="flex flex-col gap-4">
      <Card>
        <Card.Content className="flex flex-col gap-5 pt-6">
          <SectionTitle
            title={t("suggestion.title")}
            // The description changes with identity: staff see "these are everyone's",
            // ordinary users see "these are yours". The same sentence would make staff
            // think they can only see their own.
            description={isStaff ? t("suggestion.descStaff") : t("suggestion.desc")}
            actions={
              <Button
                size="sm"
                data-testid="create-suggestion"
                onPress={() => {
                  setTitle("");
                  setBody("");
                  clear();
                  setComposing(true);
                }}
              >
                {t("suggestion.create")}
              </Button>
            }
          />

          {writeError ? <InlineError error={writeError} testId="suggestion-write-error" /> : null}

          {page.suggestions.length === 0 ? (
            <Empty
              icon="gravity-ui:comment"
              title={t("suggestion.emptyTitle")}
              description={t("suggestion.emptyDesc")}
            />
          ) : (
            <div className="flex flex-col" data-testid="suggestion-list">
              {page.suggestions.map((sg, i) => (
                <div key={sg.id}>
                  {i > 0 ? <Separator /> : null}
                  <div className="flex items-center gap-3 py-3">
                    <div className="flex min-w-0 flex-col gap-1">
                      <button
                        type="button"
                        className="cursor-[var(--cursor-interactive)] truncate text-left text-sm font-medium hover:text-accent"
                        data-testid={`suggestion-open-${sg.id}`}
                        onClick={() => {
                          clear();
                          setOpenDetail(sg);
                        }}
                      >
                        {sg.title}
                      </button>
                      <div className="flex items-center gap-2 text-xs text-muted">
                        <SuggestionStatusChip status={sg.status} />
                        {/* ⚠️ The author is only shown to staff. Ordinary users' lists contain only their own,
                            so labeling their own email again is pure noise; and **not showing real names publicly**
                            is exactly what lets this feature ask for real names. */}
                        {isStaff ? (
                          <span className="truncate" data-testid="suggestion-author">
                            {sg.author}
                          </span>
                        ) : null}
                        <span className="tabular-nums">
                          {formatRelative(sg.createdAt, language)}
                        </span>
                        {/*
                          ⚠️ Each of the two badges has its own testid. **Do not let e2e count the
                          whole row's text**:
                          from the staff view this row also renders the author email
                          (`alice@example.com`), which contains digits,
                          so an assertion like "the list contains 1" stays green even when the count
                          is not rendered at all.
                        */}
                        {sg.replyCount > 0 ? (
                          <span
                            className="flex items-center gap-1 tabular-nums"
                            data-testid="suggestion-reply-count"
                          >
                            <Icon icon="gravity-ui:comment" className="size-3" />
                            {sg.replyCount}
                          </span>
                        ) : null}
                        {sg.attachmentCount > 0 ? (
                          <span
                            className="flex items-center gap-1 tabular-nums"
                            data-testid="suggestion-attachment-count"
                          >
                            <Icon icon="gravity-ui:paperclip" className="size-3" />
                            {sg.attachmentCount}
                          </span>
                        ) : null}
                      </div>
                    </div>
                    <div className="ml-auto flex shrink-0 items-center gap-2">
                      <Button
                        size="sm"
                        variant="outline"
                        data-testid={`suggestion-detail-${sg.id}`}
                        onPress={() => {
                          clear();
                          setOpenDetail(sg);
                        }}
                      >
                        {t("suggestion.view")}
                      </Button>
                      <Button
                        size="sm"
                        variant="danger-soft"
                        data-testid={`suggestion-delete-${sg.id}`}
                        onPress={() => setDeleting(sg)}
                      >
                        {t("common.delete")}
                      </Button>
                    </div>
                  </div>
                </div>
              ))}
            </div>
          )}

          <Separator />
          <p className="text-xs text-muted">{t("suggestion.footnote")}</p>
        </Card.Content>
      </Card>

      <FormModal
        isOpen={composing}
        onOpenChange={setComposing}
        title={t("suggestion.create")}
        description={t("suggestion.createDesc")}
        submitLabel={t("suggestion.submit")}
        testId="create-suggestion-modal"
        onSubmit={async () => {
          try {
            const created = await api.createSuggestion({ title, body });
            // On success clear the red box from the previous failure. Otherwise after "fail, tweak,
            // succeed"
            // the card still carries that no-longer-true error, and users see it when closing the
            // detail.
            clear();
            await load();
            // Open the detail right after submit: images must hang on a suggestion that already
            // exists
            // (the attachment table has a suggestion_id foreign key), so it is "submit first, then
            // upload".
            // Without jumping there, users would have no entry for adding images after submitting.
            setOpenDetail(created);
            return true;
          } catch (e) {
            /*
              ⚠️ **On failure, `fail(e)` records the first-level feedback, then throw it back for
              FormModal to render.**

              This used to be `fail(e); return false;`, a path where **the user sees not a single
              word**:
              `fail` only writes the error into `suggestion-write-error` on the card, and on failure
              the dialog deliberately
              stays open (correctly, otherwise what the user just wrote is lost), so that sentence
              lands right
              under the overlay. It looks like "clicking submit does nothing", exactly the shape
              that makes people click submit again and again.

              Zero errors; neither tsc nor code review can see it; only screenshots or e2e catch it
              (the "title and body both empty" case in 33-suggestions is the one that catches it).

              Both must be done; neither can be skipped:
                fail(e)  records first-level feedback + keeps console.error (needed for debugging),
                         and makes that box on the card show the second-level "report this
                         problem" entry
                throw    hands it to FormModal to render into `modal-error`, so the user sees it
                         right there

              What is thrown is the message of the AsyncError returned by `fail()`, not the raw `e`:
              FormModal takes the text with `e instanceof Error ? e.message : …`, and AsyncError
              is **a plain object, not an Error**; throwing it directly would fall back to the
              generic "operation failed",
              losing the backend's specific "please fill in at least a title or body".
            */
            throw new Error(fail(e).message);
          }
        }}
      >
        <TextField className="w-full" value={title} onChange={setTitle}>
          <Label>{t("suggestion.fieldTitle")}</Label>
          <Input data-testid="suggestion-title" placeholder={t("suggestion.titlePlaceholder")} />
        </TextField>
        <TextField className="w-full" value={body} onChange={setBody}>
          <Label>{t("suggestion.fieldBody")}</Label>
          <TextArea
            rows={6}
            data-testid="suggestion-body"
            placeholder={t("suggestion.bodyPlaceholder")}
          />
        </TextField>
        <p className="text-xs text-muted">{t("suggestion.titleHint")}</p>
      </FormModal>

      {openDetail ? (
        <SuggestionDetail
          suggestion={openDetail}
          isStaff={isStaff}
          onClose={() => setOpenDetail(null)}
          onChanged={async () => {
            await load();
          }}
        />
      ) : null}

      <ConfirmModal
        isOpen={deleting !== null}
        onOpenChange={(open) => {
          if (!open) setDeleting(null);
        }}
        title={t("suggestion.deleteTitle")}
        description={t("suggestion.deleteDesc", { title: deleting?.title ?? "" })}
        confirmLabel={t("common.delete")}
        onConfirm={async () => {
          if (!deleting) return;
          // ⚠️ Failures must be **thrown**, not swallowed: ConfirmModal relies on reject to decide
          // "keep the dialog open + show the error". If swallowed, the dialog closes and the user
          // thinks the delete succeeded.
          await api.deleteSuggestion(deleting.id);
          setDeleting(null);
          await load();
        }}
      />
    </div>
  );
}

/**
 * Detail of one suggestion: body + attachments + conversation.
 *
 * Uses the FormModal shell rather than its own route page: this page lives in global settings,
 * and global settings has only one route (`/preferences?tab=`). Adding
 * `/preferences/suggestions/:id` would add one more case to the already hard rule "this page has
 * one route but two shells"
 * (see the PreferencesPage file header).
 */
function SuggestionDetail({
  suggestion,
  isStaff,
  onClose,
  onChanged,
}: {
  suggestion: Suggestion;
  isStaff: boolean;
  onClose: () => void;
  onChanged: () => Promise<void>;
}) {
  const { t, language } = usePrefs();
  const [current, setCurrent] = useState<Suggestion>(suggestion);
  const [replyBody, setReplyBody] = useState("");
  const { error: writeError, fail, clear } = useWriteError(t("common.actionFailed"));

  const fetchReplies = useCallback(
    () => api.listSuggestionReplies(suggestion.id),
    [suggestion.id],
  );
  const { data: replies, reload: reloadReplies } = useAsyncData<SuggestionReply[]>(
    fetchReplies,
    t("suggestion.loadFailed"),
  );

  const fetchAttachments = useCallback(
    () => api.listSuggestionAttachments(suggestion.id),
    [suggestion.id],
  );
  const { data: attachments, reload: reloadAttachments } = useAsyncData<SuggestionAttachment[]>(
    fetchAttachments,
    t("suggestion.loadFailed"),
  );

  return (
    <FormModal
      isOpen
      onOpenChange={(open) => {
        if (!open) onClose();
      }}
      title={current.title}
      submitLabel={t("common.back")}
      testId="suggestion-detail-modal"
      onSubmit={async () => {
        onClose();
        return true;
      }}
    >
      <div className="flex flex-col gap-4">
        <div className="flex flex-wrap items-center gap-2">
          <SuggestionStatusChip status={current.status} />
          {isStaff ? (
            <span className="text-xs text-muted" data-testid="suggestion-detail-author">
              {current.author}
            </span>
          ) : null}
          <span className="text-xs text-muted tabular-nums">
            {formatRelative(current.createdAt, language)}
          </span>
        </div>

        {writeError ? (
          <InlineError error={writeError} testId="suggestion-detail-error" />
        ) : null}

        {/*
          ⚠️ **The status control is only for staff**. Status says "how far staff have gotten with
          it",
          and letting the filer click "resolved" themselves would make this field meaningless.
          The backend also blocks it (authors get 403); hiding it here is so people do not find out
          only after clicking.
        */}
        {isStaff ? (
          <Select
            className="max-w-3xs"
            selectedKey={current.status}
            onSelectionChange={async (k) => {
              clear();
              try {
                const next = await api.updateSuggestion(current.id, {
                  // ⚠️ Title and body are sent as empty strings: the backend's heuristic "both empty
                  // = do not touch them this time"
                  // keeps them. Sending current.title/body also works, but then one
                  // status change would rewrite the whole body (and change what updatedAt means).
                  title: "",
                  body: "",
                  status: String(k) as SuggestionStatus,
                });
                setCurrent(next);
                await onChanged();
              } catch (e) {
                fail(e);
              }
            }}
            aria-label={t("suggestion.fieldStatus")}
          >
            <Label>{t("suggestion.fieldStatus")}</Label>
            <Select.Trigger data-testid="suggestion-status-select">
              {/* Select.Value renders the whole content of the selected item; do not draw it again here. */}
              <Select.Value />
              <Select.Indicator />
            </Select.Trigger>
            <Select.Popover>
              <ListBox>
                {SUGGESTION_STATUSES.map((s) => (
                  <ListBox.Item key={s} id={s} textValue={statusLabel(s, t as never)}>
                    {statusLabel(s, t as never)}
                  </ListBox.Item>
                ))}
              </ListBox>
            </Select.Popover>
          </Select>
        ) : null}

        {current.body ? (
          <div className="r-inset bg-surface-secondary p-3">
            <MarkdownMessage className="text-sm">{current.body}</MarkdownMessage>
          </div>
        ) : null}

        {/* Attachments: images are added here. Upload needs an existing suggestion, so the detail opens automatically after submit. */}
        <div className="flex flex-col gap-2">
          <SectionTitle title={t("suggestion.attachments")} />
          <SuggestionAttachmentPicker
            suggestionId={current.id}
            onUploaded={async () => {
              await reloadAttachments();
              await onChanged();
            }}
            onError={fail}
          />
          <AttachmentList
            attachments={(attachments ?? []).map((a) => ({
              // AttachmentList takes the IssueAttachment shape. The two differ only by
              // workspaceId (suggestions have no workspace); fill in an empty string to reuse it;
              // that field is not used in rendering, only for issue-side resource-to-workspace
              // lookup against unauthorized access.
              ...a,
              issueId: a.suggestionId,
              workspaceId: "",
            }))}
            onDelete={async (att) => {
              clear();
              try {
                await api.deleteSuggestionAttachment(att.id);
                await reloadAttachments();
                await onChanged();
              } catch (e) {
                fail(e);
              }
            }}
          />
        </div>

        {/* Conversation: staff replies and user follow-ups use the same path */}
        <div className="flex flex-col gap-3">
          <SectionTitle title={t("suggestion.replies")} />
          {(replies ?? []).length === 0 ? (
            <p className="text-sm text-muted">{t("suggestion.noReplies")}</p>
          ) : (
            <ul className="flex flex-col gap-3" data-testid="suggestion-reply-list">
              {(replies ?? []).map((r) => (
                <li key={r.id} className="r-inset bg-surface-secondary p-3">
                  <div className="mb-1 flex items-center gap-2">
                    {/* The official reply must stand out at a glance: it is what users most want to find */}
                    {r.isStaff ? (
                      <Chip size="sm" color="accent" variant="soft" data-testid="reply-staff">
                        <Chip.Label>{t("suggestion.staffBadge")}</Chip.Label>
                      </Chip>
                    ) : null}
                    {/*
                      ⚠️ **The author email of staff replies is empty; that is not missing data.**

                      The server **deliberately removes** it for non-staff (hideStaffIdentity in
                      core/suggestion.go): staff replies speak
                      **in the platform's name**,
                      and showing the email would reveal who develops the platform, and also let
                      users message that person directly
                      to push for progress, going around this channel.

                      So the check here is "is there a value", not "is it staff":
                      staff see real names themselves (they need to know who followed up), and then
                      this
                      span renders as usual. Writing `!r.isStaff && ...` would hide it from the staff
                      view,
                      which is the error in the other direction.
                    */}
                    {r.author ? (
                      <span className="text-xs text-muted">{r.author}</span>
                    ) : null}
                    <span className="text-xs text-muted tabular-nums">
                      {formatRelative(r.createdAt, language)}
                    </span>
                    {r.updatedAt !== r.createdAt ? (
                      <span className="text-xs text-muted">{t("suggestion.edited")}</span>
                    ) : null}
                  </div>
                  <MarkdownMessage className="text-sm">{r.body}</MarkdownMessage>
                </li>
              ))}
            </ul>
          )}

          <TextField className="w-full" value={replyBody} onChange={setReplyBody}>
            <Label>{isStaff ? t("suggestion.replyAsStaff") : t("suggestion.replyAsUser")}</Label>
            <TextArea
              rows={3}
              data-testid="suggestion-reply-body"
              placeholder={t("suggestion.replyPlaceholder")}
            />
          </TextField>
          <Button
            size="sm"
            className="w-fit"
            data-testid="suggestion-reply-submit"
            isDisabled={replyBody.trim() === ""}
            onPress={async () => {
              clear();
              try {
                await api.createSuggestionReply(current.id, replyBody);
                setReplyBody("");
                await reloadReplies();
                await onChanged();
              } catch (e) {
                fail(e);
              }
            }}
          >
            {t("suggestion.replySubmit")}
          </Button>
        </div>
      </div>
    </FormModal>
  );
}

/**
 * Upload area for suggestion attachments.
 *
 * **Does not reuse IssueBits' AttachmentDropZone**: that component hard-codes
 * `api.uploadIssueAttachment`, while this one hits a different route. Changing it to take an
 * upload function
 * would touch the two issue pages, and the gain is not worth the risk; here we only need a button,
 * no drop zone.
 */
function SuggestionAttachmentPicker({
  suggestionId,
  onUploaded,
  onError,
}: {
  suggestionId: string;
  onUploaded: () => Promise<void>;
  onError: (e: unknown) => void;
}) {
  const { t } = usePrefs();
  const [uploading, setUploading] = useState(false);

  return (
    <div>
      {/*
        ⚠️ Use sr-only rather than display:none: the latter breaks click() in some browsers,
        and e2e's setInputFiles needs an input really in the DOM (same as IssueBits).
      */}
      <input
        type="file"
        accept="image/png,image/jpeg,image/gif,image/webp,.pdf,.txt,.md,.csv,.json,.zip"
        className="sr-only"
        id={`suggestion-file-${suggestionId}`}
        data-testid="suggestion-attachment-input"
        onChange={async (e) => {
          const file = e.target.files?.[0];
          // Clear value: otherwise picking the same file in a row does not trigger change,
          // looking like "the second click does nothing".
          e.target.value = "";
          if (!file) return;
          setUploading(true);
          try {
            await api.uploadSuggestionAttachment(suggestionId, file);
            await onUploaded();
          } catch (err) {
            onError(err);
          } finally {
            setUploading(false);
          }
        }}
      />
      <Button
        size="sm"
        variant="outline"
        isPending={uploading}
        data-testid="suggestion-attachment-pick"
        onPress={() => document.getElementById(`suggestion-file-${suggestionId}`)?.click()}
      >
        <Icon icon="gravity-ui:paperclip" className="size-4" />
        {uploading ? t("suggestion.uploading") : t("suggestion.addImage")}
      </Button>
    </div>
  );
}

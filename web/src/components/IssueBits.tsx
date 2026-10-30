/**
 * Small components shared by the Issue pages: status and priority Chips, pickers, attachment upload area.
 *
 * Pulled out because both the list page and the detail page use them, and their semantic colors and text mapping must match:
 * if each wrote its own, "open" would be gray in the list and yellow on the detail page,
 * and users would think those are two different states.
 */

import { useCallback, useRef, useState } from "react";
import { Button, Chip, Label, ListBox, Select, Tooltip } from "@heroui/react";
import { Icon } from "@iconify/react";
import {
  api,
  ATTACHMENT_ACCEPT,
  attachmentTypeOf,
  formatBytesIEC,
  MAX_ATTACHMENT_BYTES,
} from "@/data/api";
import { usePrefs } from "@/prefs/PrefsContext";
import type { MessageKey } from "@/prefs/messages";
import {
  ISSUE_PRIORITIES,
  ISSUE_STATUSES,
  isImageAttachment,
  type IssueAttachment,
  type IssuePriority,
  type IssueStatus,
} from "@/types/domain";

/**
 * Status → Chip color.
 *
 * Meaning: **closed uses success** (a solved problem is good), in progress uses warning
 * (someone is looking but not done), open uses default (neutral: it is just "not started yet", not bad).
 *
 * ⚠️ Do not make open danger: that would make a normal new issue look like an outage,
 * and truly urgent things should be expressed by priority. Using semantic colors wrong is worse than not using them.
 */
const STATUS_COLOR: Record<IssueStatus, "default" | "warning" | "success"> = {
  open: "default",
  in_progress: "warning",
  closed: "success",
};

/**
 * Priority → Chip color. Only P0 is danger; P2 is neutral.
 *
 * P2 is the default (used when not filled in), so it must look the least noticeable; otherwise every new issue
 * visually shouts "look at me", and the priority dimension is wasted.
 */
const PRIORITY_COLOR: Record<IssuePriority, "default" | "warning" | "danger"> = {
  p0: "danger",
  p1: "warning",
  p2: "default",
};

/** Message keys for statuses. Module-level constants cannot call t(), so we store keys and translate at render time. */
function statusKey(s: IssueStatus): MessageKey {
  return `enum.issueStatus.${s}` as MessageKey;
}

function priorityKey(p: IssuePriority): MessageKey {
  return `enum.issuePriority.${p}` as MessageKey;
}

export function IssueStatusChip({ status }: { status: IssueStatus }) {
  const { t } = usePrefs();
  return (
    <Chip size="sm" color={STATUS_COLOR[status]} variant="soft" data-testid={`issue-status-${status}`}>
      <Chip.Label>{t(statusKey(status))}</Chip.Label>
    </Chip>
  );
}

export function IssuePriorityChip({ priority }: { priority: IssuePriority }) {
  const { t } = usePrefs();
  return (
    <Chip
      size="sm"
      color={PRIORITY_COLOR[priority]}
      variant="soft"
      data-testid={`issue-priority-${priority}`}
    >
      {/* Priority uses tabular-nums: P0/P1/P2 need equal width to line up as a column in the list. */}
      <Chip.Label className="tabular-nums">{t(priorityKey(priority))}</Chip.Label>
    </Chip>
  );
}

/** Status picker. */
export function IssueStatusSelect({
  value,
  onChange,
  testId,
}: {
  value: IssueStatus;
  onChange: (s: IssueStatus) => void;
  testId?: string;
}) {
  const { t } = usePrefs();
  return (
    <Select selectedKey={value} onSelectionChange={(k) => onChange(String(k) as IssueStatus)}>
      <Label>{t("issue.fieldStatus")}</Label>
      <Select.Trigger data-testid={testId}>
        {/* Select.Value renders the whole content of the selected item; do not draw the icon again here. */}
        <Select.Value />
        <Select.Indicator />
      </Select.Trigger>
      <Select.Popover>
        <ListBox>
          {ISSUE_STATUSES.map((s) => (
            <ListBox.Item key={s} id={s} textValue={t(statusKey(s))}>
              {t(statusKey(s))}
            </ListBox.Item>
          ))}
        </ListBox>
      </Select.Popover>
    </Select>
  );
}

/** Priority picker. */
export function IssuePrioritySelect({
  value,
  onChange,
  testId,
}: {
  value: IssuePriority;
  onChange: (p: IssuePriority) => void;
  testId?: string;
}) {
  const { t } = usePrefs();
  return (
    <Select selectedKey={value} onSelectionChange={(k) => onChange(String(k) as IssuePriority)}>
      <Label>{t("issue.fieldPriority")}</Label>
      <Select.Trigger data-testid={testId}>
        <Select.Value />
        <Select.Indicator />
      </Select.Trigger>
      <Select.Popover>
        <ListBox>
          {ISSUE_PRIORITIES.map((p) => (
            <ListBox.Item key={p} id={p} textValue={t(priorityKey(p))}>
              {t(priorityKey(p))}
            </ListBox.Item>
          ))}
        </ListBox>
      </Select.Popover>
    </Select>
  );
}

/**
 * Attachment upload area: drag-and-drop + click to choose; both go through the same handler.
 *
 * # Why not the ready-made DropZone
 *
 * Tested: it does `import "motion/react"`, and this repo **does not have motion installed**; using it breaks the build right away
 * (same trap as PromptInput, see the dependency table in docs/guides/heroui.md).
 * So it copies the SkillsPage approach: a real sr-only input + a div with role=button.
 *
 * # The hidden input must really be in the DOM
 *
 * Use `sr-only`, not `display:none`: the latter makes `click()` stop working in some browsers,
 * and e2e's `setInputFiles` needs a real input (native drag-and-drop cannot be simulated with it).
 */
export function AttachmentDropZone({
  issueId,
  onUploaded,
  testIdPrefix = "issue-attachment",
}: {
  issueId: string;
  onUploaded: (att: IssueAttachment) => void;
  testIdPrefix?: string;
}) {
  const { t } = usePrefs();
  const inputRef = useRef<HTMLInputElement>(null);
  const [uploading, setUploading] = useState(false);
  const [dragging, setDragging] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const accept = useCallback(
    async (picked: File | undefined) => {
      setError(null);
      if (!picked) return;

      // The frontend checks type and size first, so the user sees feedback right away instead of being rejected after uploading.
      // **This is not a security boundary**: the real limit is in the backend (sniffing + MaxBytesReader).
      if (!attachmentTypeOf(picked)) {
        setError(t("issue.attachmentBadType"));
        return;
      }
      if (picked.size > MAX_ATTACHMENT_BYTES) {
        setError(t("issue.attachmentTooLarge", { size: formatBytesIEC(MAX_ATTACHMENT_BYTES) }));
        return;
      }

      setUploading(true);
      try {
        onUploaded(await api.uploadIssueAttachment(issueId, picked));
      } catch (e) {
        // Show the backend error text directly (it is already a readable sentence).
        setError(e instanceof Error ? e.message : t("common.actionFailed"));
      } finally {
        setUploading(false);
      }
    },
    [issueId, onUploaded, t],
  );

  return (
    <div className="flex flex-col gap-2">
      <input
        ref={inputRef}
        type="file"
        accept={ATTACHMENT_ACCEPT}
        className="sr-only"
        data-testid={`${testIdPrefix}-input`}
        onChange={(e) => {
          void accept(e.target.files?.[0]);
          // Clear value: otherwise choosing the same file again does not fire change,
          // which shows as "the second click does nothing".
          e.target.value = "";
        }}
      />

      <div
        role="button"
        tabIndex={0}
        aria-label={t("issue.attachmentDrop")}
        data-testid={`${testIdPrefix}-drop`}
        className={`r-inset flex cursor-[var(--cursor-interactive)] flex-col items-center gap-2 border border-dashed p-6 text-center ${
          dragging ? "border-accent bg-accent/5" : "border-default"
        }`}
        onClick={() => inputRef.current?.click()}
        onKeyDown={(e) => {
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            inputRef.current?.click();
          }
        }}
        onDragOver={(e) => {
          // preventDefault is required, otherwise the browser treats the file as navigation (opens it directly).
          e.preventDefault();
          setDragging(true);
        }}
        onDragLeave={() => setDragging(false)}
        onDrop={(e) => {
          e.preventDefault();
          setDragging(false);
          void accept(e.dataTransfer.files?.[0]);
        }}
      >
        <Icon icon="gravity-ui:paperclip" className="size-6 text-muted" />
        <p className="text-sm">
          {uploading ? t("issue.attachmentUploading") : t("issue.attachmentDrop")}
        </p>
        <p className="text-xs text-muted">
          {t("issue.attachmentHint", { size: formatBytesIEC(MAX_ATTACHMENT_BYTES) })}
        </p>
      </div>

      {error ? (
        <p className="text-sm text-danger" role="alert" data-testid={`${testIdPrefix}-error`}>
          {error}
        </p>
      ) : null}
    </div>
  );
}

/**
 * Attachment list. Images show thumbnails; others show the file name.
 *
 * Each item is a link to `att.url`; that URL requires auth
 * (see IssueAttachment.url in types/domain.ts), and same-origin requests carry the Cookie automatically.
 */
export function AttachmentList({
  attachments,
  onDelete,
  onInsert,
}: {
  attachments: IssueAttachment[];
  onDelete?: (att: IssueAttachment) => void;
  onInsert?: (att: IssueAttachment) => void;
}) {
  const { t } = usePrefs();

  if (attachments.length === 0) {
    return <p className="text-sm text-muted">{t("issue.attachmentNone")}</p>;
  }

  return (
    <ul className="flex flex-col gap-2" data-testid="issue-attachment-list">
      {attachments.map((att) => (
        <li
          key={att.id}
          className="flex items-center gap-3 r-inset bg-surface-secondary p-2"
          data-testid={`issue-attachment-${att.filename}`}
        >
          {isImageAttachment(att) ? (
            // The thumbnail uses that authenticated URL directly; the browser sends the same-origin Cookie.
            <img
              src={att.url}
              alt={att.filename}
              className="size-10 shrink-0 rounded-md object-cover ring-1 ring-black/5 dark:ring-white/10"
            />
          ) : (
            <span className="flex size-10 shrink-0 items-center justify-center rounded-md bg-surface">
              <Icon icon="gravity-ui:file" className="size-5 text-muted" />
            </span>
          )}

          <a
            href={att.url}
            target="_blank"
            rel="noopener noreferrer"
            className="min-w-0 flex-1 truncate text-sm no-underline hover:underline"
          >
            {att.filename}
          </a>
          <span className="shrink-0 text-xs text-muted tabular-nums">
            {formatBytesIEC(att.size)}
          </span>

          {onInsert ? (
            <Tooltip>
              <Tooltip.Trigger>
                <Button
                  size="sm"
                  variant="ghost"
                  isIconOnly
                  aria-label={t("issue.attachmentInsert")}
                  onPress={() => onInsert(att)}
                  data-testid={`issue-attachment-insert-${att.filename}`}
                >
                  <Icon icon="gravity-ui:arrow-right-from-line" className="size-4" />
                </Button>
              </Tooltip.Trigger>
              <Tooltip.Content>{t("issue.attachmentInsert")}</Tooltip.Content>
            </Tooltip>
          ) : null}

          {onDelete ? (
            <Tooltip>
              <Tooltip.Trigger>
                <Button
                  size="sm"
                  variant="ghost"
                  isIconOnly
                  aria-label={t("issue.attachmentDeleteAction")}
                  onPress={() => onDelete(att)}
                  data-testid={`issue-attachment-delete-${att.filename}`}
                >
                  <Icon icon="gravity-ui:trash-bin" className="size-4" />
                </Button>
              </Tooltip.Trigger>
              <Tooltip.Content>{t("issue.attachmentDeleteAction")}</Tooltip.Content>
            </Tooltip>
          ) : null}
        </li>
      ))}
    </ul>
  );
}

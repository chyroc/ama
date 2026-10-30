/**
 * Workspace skill library. Skills are **workspace-level assets**, and agents bind them by
 * reference; so this page manages the assets themselves,
 * while bindings are configured on the agent detail page.
 */

import { skillText } from "@/data/builtinSkills";
import { useCallback, useRef, useState } from "react";
import {
  Button,
  Card,
  Chip,
  Description,
  FieldError,
  Input,
  Label,
  TextArea,
  TextField,
  Tooltip,
} from "@heroui/react";
import { Icon } from "@iconify/react";
import { Link } from "react-router-dom";
import { MAX_SKILL_UPLOAD_BYTES, SKILL_UPLOAD_ACCEPT, api, formatBytesIEC } from "@/data/api";
import { useWorkspace, useWorkspacePath } from "@/app/WorkspaceGate";
import { AsyncErrorBlock, Empty, ListSkeleton, PageBody, PageHeader } from "@/components/Page";
import { ConfirmModal, FormModal } from "@/components/FormModal";
import { formatRelative, truncate } from "@/lib/format";
import { useAsyncData } from "@/lib/useAsyncData";
import {
  isBuiltinSkill,
  isReadOnlySkill,
  skillSourceBadge,
  SKILL_NAME_PATTERN,
  type Skill,
  type SkillDraft,
  type SkillPackage,
} from "@/types/domain";
import { usePrefs } from "@/prefs/PrefsContext";

function emptyDraft(): SkillDraft {
  return { name: "", displayTitle: "", description: "", instructions: "", tags: [] };
}

/**
 * Form for uploading a skill package: pick a file → parse → preview → the outer FormModal confirms
 * creation.
 *
 * # Why there is a "preview" step
 *
 * name and description are read from the frontmatter of SKILL.md, and users cannot see the parse
 * result
 * before uploading. Saving directly would silently turn a package with broken frontmatter into a
 * skill with an odd name, and users would first notice it in the list, when they must delete it
 * and start over.
 *
 * # Drag-and-drop and click-to-pick share one handler
 *
 * Both paths land in accept(), so validation and error messages have a single implementation.
 * The hidden <input type="file"> is the only entry e2e can drive (setInputFiles needs a real
 * input, and native drag-and-drop cannot be simulated with it), so it must be in the DOM, just
 * visually hidden.
 */
function SkillUploadForm({
  pkg,
  setPkg,
  file,
  setFile,
}: {
  pkg: SkillPackage | null;
  setPkg: (p: SkillPackage | null) => void;
  /**
   * The currently selected file, **kept in state rather than re-read from the input**.
   *
   * Reason: the input's value is cleared after change (otherwise picking the same file again would
   * not trigger
   * change, looking like "the second click does nothing"), so input.files cannot be read on submit.
   * And submit needs the original file: the server does not store the parse intermediate state
   * (with multiple instances that state is likely not in
   * the same process), so the same file must be sent again on create.
   */
  file: File | null;
  setFile: (f: File | null) => void;
}) {
  const { t } = usePrefs();
  const { workspace } = useWorkspace();
  const inputRef = useRef<HTMLInputElement>(null);
  const [parsing, setParsing] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [dragging, setDragging] = useState(false);

  const accept = useCallback(
    async (picked: File | undefined) => {
      setError(null);
      setPkg(null);
      setFile(null);
      if (!picked) return;

      // The frontend checks type and size first so users get feedback at once instead of being
      // rejected after uploading.
      // **This is not a security boundary**: the real limit is on the backend (MaxBytesReader is set
      // before reading).
      const lower = picked.name.toLowerCase();
      if (!lower.endsWith(".md") && !lower.endsWith(".zip")) {
        setError(t("skill.uploadBadType"));
        return;
      }
      if (picked.size > MAX_SKILL_UPLOAD_BYTES) {
        setError(t("skill.uploadTooLarge", { size: formatBytesIEC(MAX_SKILL_UPLOAD_BYTES) }));
        return;
      }

      setParsing(true);
      try {
        // Parsing is done on the server (dryRun), nothing stored. Error text comes straight from
        // the backend and is shown as is.
        const parsed = await api.parseSkillPackage(workspace.id, picked);
        setPkg(parsed);
        setFile(picked);
      } catch (e) {
        setError(e instanceof Error ? e.message : t("common.actionFailed"));
      } finally {
        setParsing(false);
      }
    },
    [setPkg, setFile, t, workspace.id],
  );

  return (
    <>
      {/*
        The hidden real input. Use sr-only rather than display:none: the latter in some browsers
        breaks click(), and e2e's setInputFiles needs it really in the DOM.
      */}
      <input
        ref={inputRef}
        type="file"
        accept={SKILL_UPLOAD_ACCEPT}
        className="sr-only"
        data-testid="skill-upload-input"
        onChange={(e) => {
          void accept(e.target.files?.[0]);
          // Clear value: otherwise picking the same file in a row would not trigger change,
          // looking like "the second click does nothing".
          e.target.value = "";
        }}
      />

      {/*
        Drop zone. It is a div, not a Button: it holds multi-line notes and the file name,
        and putting those inside a Button would give a nested focusable element. Accessibility is
        filled in with role=button + onKeyDown.
      */}
      <div
        role="button"
        tabIndex={0}
        aria-label={t("skill.uploadPick")}
        data-testid="skill-upload-drop"
        className={`r-inset flex cursor-pointer flex-col items-center gap-2 border border-dashed p-6 text-center transition-colors ${
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
          // Must preventDefault, otherwise the browser treats the file as navigation (opens it
          // directly).
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
        <Icon icon="gravity-ui:cloud-arrow-up-in" className="size-6 text-muted" />
        <p className="text-sm">
          {parsing ? t("skill.uploadParsing") : (file?.name ?? t("skill.uploadDrop"))}
        </p>
        <p className="text-xs text-muted">
          {t("skill.uploadHint", { size: formatBytesIEC(MAX_SKILL_UPLOAD_BYTES) })}
        </p>
      </div>

      {error ? (
        <p className="text-sm text-danger" role="alert" data-testid="skill-upload-error">
          {error}
        </p>
      ) : null}

      {pkg ? <SkillPackagePreview pkg={pkg} /> : null}
    </>
  );
}

/** Read-only preview of the parse result. Users confirm the content here is right before creating. */
function SkillPackagePreview({ pkg }: { pkg: SkillPackage }) {
  const { t } = usePrefs();
  return (
    <div className="flex flex-col gap-4" data-testid="skill-preview">
      <div>
        <p className="text-sm font-medium">{t("skill.previewTitle")}</p>
        <p className="text-xs text-muted">{t("skill.previewDesc")}</p>
      </div>

      <div className="grid gap-3 sm:grid-cols-2">
        <div>
          <p className="text-xs text-muted">{t("skill.title")}</p>
          <p className="truncate text-sm" data-testid="skill-preview-title">
            {pkg.displayTitle}
          </p>
        </div>
        <div>
          <p className="text-xs text-muted">{t("skill.name")}</p>
          <p className="truncate font-mono text-sm" data-testid="skill-preview-name">
            {pkg.name}
          </p>
        </div>
      </div>

      <div>
        <p className="text-xs text-muted">{t("skill.when")}</p>
        <p className="text-sm" data-testid="skill-preview-desc">
          {pkg.description}
        </p>
      </div>

      <div>
        <p className="text-xs text-muted">
          {pkg.files.length > 0
            ? t("skill.previewFiles", { count: pkg.files.length })
            : t("skill.previewNoFiles")}
        </p>
        {pkg.files.length > 0 ? (
          <ul className="mt-1 flex flex-col gap-1" data-testid="skill-preview-files">
            {pkg.files.map((f) => (
              <li key={f.path} className="flex items-center justify-between gap-3 text-xs">
                <span className="truncate font-mono">{f.path}</span>
                <span className="num shrink-0 text-muted">{formatBytesIEC(f.size)}</span>
              </li>
            ))}
          </ul>
        ) : null}
      </div>

      <div>
        <p className="text-xs text-muted">{t("skill.previewBody")}</p>
        <pre className="r-inset mt-1 max-h-48 overflow-auto bg-default/40 p-3 font-mono text-xs whitespace-pre-wrap">
          {truncate(pkg.instructions, 1200)}
        </pre>
      </div>
    </div>
  );
}

function SkillForm({
  draft,
  setDraft,
  errors,
}: {
  draft: SkillDraft;
  setDraft: (d: SkillDraft) => void;
  errors: Partial<Record<keyof SkillDraft, string>>;
}) {
  const { t } = usePrefs();
  return (
    <>
      <div className="grid gap-5 sm:grid-cols-2">
        <TextField isRequired isInvalid={Boolean(errors.displayTitle)}>
          <Label>{t("skill.title")}</Label>
          <Input
            placeholder={t("skill.titlePlaceholder")}
            value={draft.displayTitle}
            onChange={(e) => setDraft({ ...draft, displayTitle: e.target.value })}
            data-testid="skill-title"
          />
          {errors.displayTitle ? <FieldError>{errors.displayTitle}</FieldError> : null}
        </TextField>
        <TextField isRequired isInvalid={Boolean(errors.name)}>
          <Label>{t("skill.name")}</Label>
          <Input
            placeholder="ticket-triage"
            value={draft.name}
            onChange={(e) => setDraft({ ...draft, name: e.target.value })}
            data-testid="skill-name"
          />
          <Description>{t("skill.nameInvalid")}</Description>
          {errors.name ? <FieldError>{errors.name}</FieldError> : null}
        </TextField>
      </div>

      <TextField isRequired isInvalid={Boolean(errors.description)}>
        <Label>{t("skill.when")}</Label>
        <TextArea
          rows={2}
          placeholder={t("skill.whenPlaceholder")}
          value={draft.description}
          onChange={(e) => setDraft({ ...draft, description: e.target.value })}
          data-testid="skill-desc"
        />
        <Description>{t("skill.whenDesc")}</Description>
        {errors.description ? <FieldError>{errors.description}</FieldError> : null}
      </TextField>

      <TextField isRequired isInvalid={Boolean(errors.instructions)}>
        <Label>{t("skill.body")}</Label>
        <TextArea
          rows={10}
          className="font-mono text-xs"
          placeholder={t("skill.bodyPlaceholder")}
          value={draft.instructions}
          onChange={(e) => setDraft({ ...draft, instructions: e.target.value })}
          data-testid="skill-instructions"
        />
        <Description>{t("skill.bodyDesc")}</Description>
        {errors.instructions ? <FieldError>{errors.instructions}</FieldError> : null}
      </TextField>

      <TextField>
        <Label>{t("skill.tags")}</Label>
        <Input
          placeholder={t("skill.tagsPlaceholder")}
          value={draft.tags.join(", ")}
          onChange={(e) =>
            setDraft({
              ...draft,
              tags: e.target.value
                .split(/[,，]/)
                .map((s) => s.trim())
                .filter(Boolean),
            })
          }
        />
        <Description>{t("skill.tagsDesc")}</Description>
      </TextField>
    </>
  );
}

export function SkillsPage() {
  const { t, language } = usePrefs();
  const { workspace } = useWorkspace();
  const wp = useWorkspacePath();
  const [editing, setEditing] = useState<Skill | null>(null);
  const [creating, setCreating] = useState(false);
  const [draft, setDraft] = useState<SkillDraft>(emptyDraft());
  const [errors, setErrors] = useState<Partial<Record<keyof SkillDraft, string>>>({});
  const [deleting, setDeleting] = useState<Skill | null>(null);
  // State of uploading a skill package. A non-empty pkg means it parsed successfully and can be
  // created.
  const [uploading, setUploading] = useState(false);
  const [pkg, setPkg] = useState<SkillPackage | null>(null);
  const [uploadFile, setUploadFile] = useState<File | null>(null);
  // Which skill is being forked (by id). **Use id, not boolean**: each card in the list has its own
  // Fork button, and a boolean would make all buttons spin together.
  const [forking, setForking] = useState<string | null>(null);
  // The sentence for when a whole operation (Fork) fails. Kept apart from form field errors:
  // this one shows above the list, not under some input.
  const [actionError, setActionError] = useState<string | null>(null);

  // Failures must be visible, not stuck on the skeleton (see the file header of lib/useAsyncData.ts).
  const fetch = useCallback(async () => {
    const [skills, agents] = await Promise.all([
      api.listSkills(workspace.id),
      api.listAgents(workspace.id),
    ]);
    return { skills, agents };
  }, [workspace.id]);
  const { data, error, isLoading, reload: load } = useAsyncData(
    fetch,
    t("common.loadFailedUnknown"),
  );
  const skills = data?.skills ?? null;
  const agents = data?.agents ?? [];

  function validate(d: SkillDraft): boolean {
    const e: Partial<Record<keyof SkillDraft, string>> = {};
    if (!d.displayTitle.trim()) e.displayTitle = t("skill.titleRequired");
    if (!d.name.trim()) e.name = t("skill.nameRequired");
    else if (!SKILL_NAME_PATTERN.test(d.name)) e.name = t("skill.nameInvalid");
    if (!d.description.trim()) e.description = t("skill.whenRequired");
    if (!d.instructions.trim()) e.instructions = t("skill.bodyRequired");
    setErrors(e);
    return Object.keys(e).length === 0;
  }

  function openCreate() {
    setDraft(emptyDraft());
    setErrors({});
    setCreating(true);
  }

  /** Open the upload dialog. Clear the previous parse result each time, otherwise the previous package's preview shows. */
  function openUpload() {
    setPkg(null);
    setUploadFile(null);
    setUploading(true);
  }

  function openEdit(s: Skill) {
    setDraft({
      name: s.name,
      displayTitle: s.displayTitle,
      description: s.description,
      instructions: s.instructions,
      tags: s.tags,
    });
    setErrors({});
    setEditing(s);
  }

  /** How many agents bind this skill: before deleting, users need to know the impact. */
  function boundCount(skillId: string): number {
    return agents.filter((a) => a.skillIds.includes(skillId)).length;
  }

  /**
   * Fork a read-only skill, and on success open the copy's edit dialog right away.
   *
   * **Why go straight to editing after fork**: the reason users click Fork is "I want to change
   * it";
   * staying on the list would make them find the copy and click edit themselves.
   */
  async function doFork(s: Skill) {
    setForking(s.id);
    setActionError(null);
    try {
      const fork = await api.forkSkill(workspace.id, s.id);
      await load();
      openEdit(fork);
    } catch (e) {
      // Failure must be **visible** and say why (the page shows message directly).
      // Not setErrors: that is for form field errors, while this is a whole operation failing.
      setActionError(e instanceof Error ? e.message : t("common.loadFailedUnknown"));
    } finally {
      setForking(null);
    }
  }

  return (
    <PageBody>
      <PageHeader
        title={t("nav.skills")}
        description={t("skill.listDesc")}
        actions={
          <>
            {/* Upload is a secondary entry, so ghost: typing by hand is still the default path and should not take the primary button. */}
            <Button variant="ghost" onPress={openUpload} data-testid="upload-skill">
              {t("skill.upload")}
            </Button>
            <Button onPress={openCreate} data-testid="new-skill">
              {t("skill.new")}
            </Button>
          </>
        }
      />

      {isLoading ? (
        <ListSkeleton />
      ) : error ? (
        <AsyncErrorBlock error={error} onRetry={load} />
      ) : !skills ? null : skills.length === 0 ? (
        <Empty
          icon="gravity-ui:puzzle"
          title={t("skill.emptyTitle")}
          description={t("skill.emptyDesc")}
          action={<Button onPress={openCreate}>{t("skill.new")}</Button>}
        />
      ) : (
        <div className="flex flex-col gap-4">
          {/*
            The sentence for when a whole operation (Fork) fails. Placed **above** the list, not
            inside a card:
            what failed is one operation, not a property of some skill.
          */}
          {actionError ? (
            <p className="text-sm text-danger" role="alert" data-testid="skill-action-error">
              {actionError}
            </p>
          ) : null}
          <div className="grid gap-4 lg:grid-cols-2">
          {skills.map((s) => {
            const bound = boundCount(s.id);
            const isBuiltin = isBuiltinSkill(s);
            // Read-only = built-in. No edit entry; offer Fork.
            const readOnly = isReadOnlySkill(s);
            const sourceBadge = skillSourceBadge(s.source);
            return (
              <Card key={s.id} data-testid={`skill-card-${s.name}`}>
                <Card.Content className="flex flex-col gap-4 pt-6">
                  <div className="flex items-start justify-between gap-3">
                    <div className="flex min-w-0 flex-col gap-1">
                      <div className="flex items-center gap-2">
                        {/*
                          The title is the entry to the detail page.

                          ⚠️ **Only make the title a link; do not wrap the whole card in `<Link>`**
                          (AgentsPage
                          wraps the whole card): skill cards have 2~3 buttons at the top right, and
                          wrapping another
                          link around them nests interactive elements. Clicking a button would also
                          trigger navigation, and React would
                          report DOM validation issues for nested clickable elements.
                        */}
                        <Link
                          to={wp(`/skills/${s.id}`)}
                          className="truncate font-medium no-underline"
                          aria-label={t("skill.detailOpen", { name: skillText(s, t).title })}
                          data-testid={`skill-open-${s.name}`}
                        >
                          {skillText(s, t).title}
                        </Link>
                        {/*
                          Source badge (currently only "built-in"). Goes through the function in
                          domain: the badge
                          renders once on several pages, and missing one spot when adding a source
                          gives no error; the skill just looks
                          self-built on some pages. The testid keeps the existing literal (e2e
                          relies on skill-builtin-*).
                        */}
                        {sourceBadge ? (
                          <Chip
                            size="sm"
                            variant="soft"
                            data-testid={`skill-${sourceBadge.testid}-${s.name}`}
                          >
                            <Chip.Label>{t(sourceBadge.labelKey)}</Chip.Label>
                          </Chip>
                        ) : null}
                        {/*
                          Skills with attachments must be visible as such in the list: it is the only
                          hint that "this skill writes files
                          into the container". `files` is optional (backend omitempty),
                          so always use ?? [] rather than reading .length directly.
                        */}
                        {(s.files ?? []).length > 0 ? (
                          <Chip size="sm" variant="soft" data-testid={`skill-files-${s.name}`}>
                            <Chip.Label>
                              {t("skill.filesCount", { count: (s.files ?? []).length })}
                            </Chip.Label>
                          </Chip>
                        ) : null}
                      </div>
                      <p className="truncate font-mono text-xs text-muted">{s.name}</p>
                    </div>
                    <div className="flex shrink-0 items-center gap-1">
                      {/*
                        Read-only skills (built-in) **get no edit entry**: the backend returns 403
                        for PATCH.
                        Rendering it would only let users find out after clicking, by which time the
                        error happens at the save step.
                        Not rendered at all rather than isDisabled: a button that can never be
                        clicked is more confusing than no button.

                        But **Fork** must be offered, otherwise users are stuck: they want to change
                        one sentence of an official skill,
                        and without Fork they could only copy and paste the full text by hand (and
                        attachments would not come along).
                      */}
                      {readOnly ? (
                        <Tooltip>
                          <Tooltip.Trigger>
                            <Button
                              variant="ghost"
                              isIconOnly
                              aria-label={t("skill.forkAction")}
                              isPending={forking === s.id}
                              onPress={() => void doFork(s)}
                              data-testid={`fork-skill-${s.name}`}
                            >
                              <Icon icon="gravity-ui:copy" className="size-4" />
                            </Button>
                          </Tooltip.Trigger>
                          <Tooltip.Content>{t("skill.forkAction")}</Tooltip.Content>
                        </Tooltip>
                      ) : (
                        <Tooltip>
                          <Tooltip.Trigger>
                            <Button
                              variant="ghost"
                              isIconOnly
                              aria-label={t("skill.editTitle")}
                              onPress={() => openEdit(s)}
                              data-testid={`edit-skill-${s.name}`}
                            >
                              <Icon icon="gravity-ui:pencil-to-square" className="size-4" />
                            </Button>
                          </Tooltip.Trigger>
                          <Tooltip.Content>{t("common.edit")}</Tooltip.Content>
                        </Tooltip>
                      )}
                      {/*
                        Delete: built-in skills have none (the backend returns 403; they belong to
                        no workspace).
                      */}
                      {isBuiltin ? null : (
                        <Tooltip>
                          <Tooltip.Trigger>
                            <Button
                              variant="ghost"
                              isIconOnly
                              aria-label={t("skill.deleteAction")}
                              onPress={() => setDeleting(s)}
                            >
                              <Icon icon="gravity-ui:trash-bin" className="size-4" />
                            </Button>
                          </Tooltip.Trigger>
                          <Tooltip.Content>{t("common.delete")}</Tooltip.Content>
                        </Tooltip>
                      )}
                    </div>
                  </div>

                  <p className="clamp-2 text-sm text-muted">{truncate(skillText(s, t).description, 140)}</p>

                  <div className="flex flex-wrap items-center gap-2">
                    {skillText(s, t).tags.map((tag) => (
                      <Chip key={tag} size="sm" variant="soft">
                        <Chip.Label>{tag}</Chip.Label>
                      </Chip>
                    ))}
                  </div>

                  <div className="flex items-center gap-4 text-xs text-muted">
                    <span className="num">v{s.latestVersion}</span>
                    <span className="num" data-testid={`skill-bound-${s.name}`}>
                      {t("skill.boundCount", { count: bound })}
                    </span>
                    <span>{t("agent.updatedAt", { time: formatRelative(s.updatedAt, language) })}</span>
                  </div>
                </Card.Content>
              </Card>
            );
          })}
          </div>
        </div>
      )}

      <FormModal
        isOpen={uploading}
        onOpenChange={setUploading}
        title={t("skill.uploadTitle")}
        description={t("skill.uploadDesc")}
        submitLabel={t("common.create")}
        size="lg"
        testId="skill-upload-modal"
        onSubmit={async () => {
          // No file picked / not parsed yet means not allowed. Throw so FormModal shows this
          // sentence
          // and keeps the dialog open, so the user can keep picking.
          if (!pkg || !uploadFile) {
            throw new Error(t("skill.uploadEmpty"));
          }
          // **Send the same file again**, rather than sending back the pkg from the previous step:
          // the server does not store
          // the parse intermediate state (multiple instances in production; the second request
          // likely lands on another process).
          await api.createSkillFromUpload(workspace.id, uploadFile);
          setPkg(null);
          setUploadFile(null);
          await load();
        }}
      >
        <SkillUploadForm pkg={pkg} setPkg={setPkg} file={uploadFile} setFile={setUploadFile} />
      </FormModal>

      <FormModal
        isOpen={creating}
        onOpenChange={setCreating}
        title={t("skill.new")}
        submitLabel={t("common.create")}
        size="lg"
        testId="skill-modal"
        onSubmit={async () => {
          if (!validate(draft)) return false;
          await api.createSkill(workspace.id, draft);
          await load();
        }}
      >
        <SkillForm draft={draft} setDraft={setDraft} errors={errors} />
      </FormModal>

      <FormModal
        isOpen={editing !== null}
        onOpenChange={(open) => !open && setEditing(null)}
        title={t("skill.editTitle")}
        description={
          editing && boundCount(editing.id) > 0
            ? t("skill.editHint", { count: boundCount(editing.id) })
            : undefined
        }
        size="lg"
        onSubmit={async () => {
          if (!editing) return false;
          if (!validate(draft)) return false;
          await api.updateSkill(editing.id, draft);
          setEditing(null);
          await load();
        }}
      >
        <SkillForm draft={draft} setDraft={setDraft} errors={errors} />
      </FormModal>

      <ConfirmModal
        isOpen={deleting !== null}
        onOpenChange={(open) => !open && setDeleting(null)}
        title={t("skill.deleteTitle", { name: deleting?.displayTitle ?? "" })}
        description={
          deleting && boundCount(deleting.id) > 0
            ? t("skill.deleteBound", { count: boundCount(deleting.id) })
            : t("common.irreversible")
        }
        onConfirm={async () => {
          if (!deleting) return;
          await api.deleteSkill(deleting.id);
          setDeleting(null);
          await load();
        }}
      />
    </PageBody>
  );
}

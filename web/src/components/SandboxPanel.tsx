import { useCallback, useEffect, useState } from "react";

import { Icon } from "@iconify/react";
import { Button, Chip } from "@heroui/react";
import { EmptyState } from "@/components/ui/EmptyState";

import {
  api,
  type SandboxFile,
  type SandboxFileContent,
  type SandboxState,
  type SandboxStatus,
} from "@/data/api";
import { usePrefs } from "@/prefs/PrefsContext";
import { SandboxTerminal } from "@/components/SandboxTerminal";
import { useRunView } from "@/components/ViewSwitch";

/**
 * Container panel: see which container the agent works in, browse its files, enter its terminal.
 *
 * # Four states, not three
 *
 * ⚠️ The easiest mistake in this panel is **merging the four states into two** ("exists" and "does not").
 * Two pairs among the four must be kept apart, each matching a completely different user action:
 *
 * | State | What it means | What happens if described wrong |
 * | --- | --- | --- |
 * | `none` | This chat has not called any tool yet; no container was ever created | Called "reclaimed" → the user thinks files were lost, while there never were any |
 * | `running` | The container is alive | — |
 * | `paused` | Paused automatically after 60 s idle; **the workspace is still there** | Called "reclaimed" → the user redoes everything for nothing |
 * | `gone` | Reclaimed by the platform past its TTL; the files are really gone | Called "paused" → the user clicks resume, which is sure to fail |
 *
 * So each of the four states has its own title, description and actions, and `none` **gets no button** (there is nothing to resume).
 *
 * # The state is not cached; it is queried fresh every time
 *
 * The backend inspects the container each time. The container pauses **by itself** and gets reclaimed by itself, and neither goes through amad,
 * so a cache would surely show "the page says running, but it is long gone". Same origin as the "no caching" rule of the authorization management page.
 *
 * # No automatic resume
 *
 * Opening this panel or listing files does **not** resume the container on the side: resuming takes over ten seconds and takes a slot of the global concurrency quota again
 * (capped at 20). Letting a "just take a look" action carry that cost, without the user knowing what they triggered,
 * is wrong. So the paused state gets an explicit button.
 *
 * # Why files and terminal are two tabs
 *
 * They are two completely different uses: the file list is "glance at what is there", the terminal is "go in and work",
 * and the latter needs the full height to be usable. Squeezed into one tab, the terminal would only ever get the bottom half of the screen.
 * Split apart, `?view=files` and `?view=terminal` can each be shared directly too.
 *
 * The container state (the four-state part) **is shown on both sides**: it is the shared precondition for using both features,
 * and showing it on only one side would leave "why is it empty" on the other side unexplained.
 *
 * # Component choices
 *
 * components/ui/EmptyState renders the three states none/paused/gone: it is exactly the shape "icon + title + description +
 * actions", and building our own would only look different from the other empty states on the site.
 *
 * **No ready-made tree component is used.** The reason is that such components usually require **the whole tree at once**
 * (children inline or via useTreeData), while we **fetch level by level on demand**: the backend's depth hard limit
 * is 1, and only entering a directory sends a request. Forcing in a FileTree would mean fetching the whole tree first, which is exactly what the backend deliberately
 * does not do (fetching a big directory at once is slow and large). So this is a plain single-level list + a breadcrumb-like
 * "up one level", matching "fetch level by level" one to one.
 */
export function SandboxPanel({
  runId,
  mode,
  className,
  hostId,
  hostName,
}: {
  runId: string;
  /** What this tab shows: the file list or the terminal. Both share the container state part. */
  mode: "files" | "terminal";
  className?: string;
  /**
   * The host id when this session runs on a host (Run.hostId, snapshotted from the agent when the session was created).
   *
   * When non-empty, **the whole Docker container branch is skipped**: no four-state query, no resume/file browsing; instead it shows
   * a "running on host" card; the terminal works as usual: the backend WS already routes by run.hostId to the
   * host daemon, and terminalTransport's URL and ticket endpoint are reused unchanged.
   */
  hostId?: string;
  /** Host name. If not available (the caller never looked up the host list), shows "my host". */
  hostName?: string;
}) {
  const { t } = usePrefs();
  const [, setView] = useRunView();

  const [state, setState] = useState<SandboxState | null>(null);
  const [loadError, setLoadError] = useState(false);
  const [resuming, setResuming] = useState(false);

  /**
   * Fetches the state.
   *
   * Failure is **shown explicitly** instead of silently degrading to none: the two mean different things to the user
   * ("no container" vs "could not check"), and silently degrading would make a real fault look like a normal state.
   * This differs from the trace panel's "degrade to null on failure": there, not being connected is normal; here it is not.
   */
  const load = useCallback(async () => {
    // Host sessions have no Docker container four states to query (that endpoint would report "not a container session" for them),
    // so the whole query branch is skipped.
    if (hostId) return;
    try {
      const s = await api.getRunSandbox(runId);
      setState(s);
      setLoadError(false);
    } catch {
      setState(null);
      setLoadError(true);
    }
  }, [runId, hostId]);

  useEffect(() => {
    // Clear first when switching runs, otherwise the previous session's container info would show briefly.
    setState(null);
    setLoadError(false);
    void load();
  }, [load]);

  /**
   * If the container is paused, **bring it up automatically**, without making the user click a button.
   *
   * # Why it is automatic here, while it was deliberately manual at first
   *
   * The original concern was "resuming takes over ten seconds and takes a global concurrency slot; a read-only action should not trigger it".
   * That concern holds for **listing the state**, but not for these two tabs: the user specifically clicked into "Files" or
   * "Terminal", so the intent is already clear: they want to use this container. Asking them to click "resume container" once more
   * would ask again about something they already expressed, and they would also have to understand "paused ≠ gone" first.
   *
   * The quota point still holds, so **it only resumes when really entering these two tabs**:
   * just viewing the run detail or staying on the chat tab does not touch it.
   *
   * `resuming` doubles as dedup: resuming takes over ten seconds, during which polling/re-renders must not send it again.
   */
  useEffect(() => {
    if (state?.status !== "paused" || !state.canResume || resuming) return;
    void resume();
    // resume is a closure recreated on every render; putting it in deps would trigger itself; here we only depend on the few values that really decide
    // "should we resume".
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [state?.status, state?.canResume, resuming]);

  const resume = async () => {
    setResuming(true);
    try {
      setState(await api.resumeRunSandbox(runId));
      setLoadError(false);
    } catch {
      setLoadError(true);
    } finally {
      setResuming(false);
    }
  };

  // Host branch: all hooks have been called unconditionally above, so branching from here does not break
  // rules-of-hooks. The terminal is still the same SandboxTerminal: the backend routes the
  // WS / ticket endpoint to the host daemon by run.hostId, and the frontend transport layer did not change a word.
  if (hostId) {
    const meta = <HostMeta hostName={hostName} />;
    return (
      <div className={className} data-testid="sandbox-panel" data-executor="host">
        {mode === "terminal" ? (
          // status is always "running": hosts have no pause/reclaim four states; when the daemon is offline
          // the connection fails by itself and goes into "disconnected + reconnect", and that status line still shows it faithfully.
          <SandboxTerminal runId={runId} status="running" meta={meta} />
        ) : (
          <div className="flex flex-col gap-4">
            {meta}
            <HostRunningCard hostName={hostName} onOpenTerminal={() => setView("terminal")} />
          </div>
        )}
      </div>
    );
  }

  if (loadError) {
    return (
      <div className={className} data-testid="sandbox-panel">
        <EmptyState>
          <EmptyState.Header>
            <EmptyState.Media variant="icon">
              <Icon icon="gravity-ui:circle-exclamation" />
            </EmptyState.Media>
            <EmptyState.Title>{t("sandbox.loadFailed")}</EmptyState.Title>
          </EmptyState.Header>
          <EmptyState.Content>
            <Button size="sm" variant="outline" onPress={() => void load()}>
              {t("common.retry")}
            </Button>
          </EmptyState.Content>
        </EmptyState>
      </div>
    );
  }

  // Not loaded yet. No skeleton: this panel is small anyway, and a skeleton flashing by is more jarring than blank.
  if (!state) return <div className={className} data-testid="sandbox-panel" />;

  return (
    <div className={className} data-testid="sandbox-panel" data-status={state.status}>
      {state.status === "none" ? (
        <EmptyState>
          <EmptyState.Header>
            <EmptyState.Media variant="icon">
              <Icon icon="gravity-ui:box" />
            </EmptyState.Media>
            <EmptyState.Title>{t("sandbox.none.title")}</EmptyState.Title>
            <EmptyState.Description>{t("sandbox.none.desc")}</EmptyState.Description>
          </EmptyState.Header>
        </EmptyState>
      ) : (
        <div className="flex flex-col gap-4">
          {/* ⚠️ **The terminal tab does not render the status line here**; it is passed into the terminal as `meta`,
              on **the same row** as the two buttons at the top right.

              There was a version that rendered them separately, and the status line took one row while the buttons took another, with empty space in between:
              the two rows together pushed the terminal down about 60px, while they should sit facing each other on one row
              (on the left "what container this is", on the right "what to do with it", the two ends of the same toolbar).

              The files tab keeps its own row: there are no buttons on the right there, and squeezing it in would require making an empty
              right half. */}
          {mode === "terminal" ? (
            <SandboxTerminal
              runId={runId}
              status={state.status}
              meta={<SandboxMeta state={state} />}
            />
          ) : (
            <>
              <SandboxMeta state={state} />

              {state.status === "paused" && (
                <EmptyState>
                  <EmptyState.Header>
                    <EmptyState.Media variant="icon">
                      <Icon icon="gravity-ui:circle-pause" />
                    </EmptyState.Media>
                    <EmptyState.Title>{t("sandbox.resuming")}</EmptyState.Title>
                    <EmptyState.Description>{t("sandbox.autoResuming")}</EmptyState.Description>
                  </EmptyState.Header>
                </EmptyState>
              )}

              {state.status === "gone" && (
                <EmptyState>
                  <EmptyState.Header>
                    <EmptyState.Media variant="icon">
                      <Icon icon="gravity-ui:circle-xmark" />
                    </EmptyState.Media>
                    <EmptyState.Title>{t("sandbox.gone.title")}</EmptyState.Title>
                    <EmptyState.Description>{t("sandbox.gone.desc")}</EmptyState.Description>
                  </EmptyState.Header>
                </EmptyState>
              )}

              {/* The file list has no local state to keep, and there is nothing to list when the container is gone,
                  so rendering by state is right (the terminal is the opposite; see the part below). */}
              {state.status === "running" && <SandboxFiles runId={runId} />}
            </>
          )}
        </div>
      )}
    </div>
  );
}

/** Semantic color of the status badge. gone uses danger instead of warning: it is a final state, not "pay attention". */
const STATUS_TONE: Record<Exclude<SandboxStatus, "none">, "success" | "warning" | "danger"> = {
  running: "success",
  paused: "warning",
  gone: "danger",
};

/**
 * Basic info of the container: status badge + container id, **one row**.
 *
 * # Why "created at / expires at" are no longer shown
 *
 * Three reasons together; none alone is enough to remove them, but together they are:
 *
 * 1. **They take up a second row, and that row has very low information density**: two absolute times that users would not act on
 *    at all. The terminal tab loses the most: each extra row above means one less row of terminal.
 * 2. **The expiry time is now extended** (webshell in use keeps it alive; see KeepAlive on the server side),
 *    so "expires at 07:00:46" is a value that **changes but the page does not reload**. Showing an outdated
 *    absolute time is worse than not showing it: users would plan "I have 20 minutes left" by it, while it was long since extended.
 * 3. What really matters is the **state** (running / paused / gone), which is on the badge, visible at a glance.
 *
 * `createdAt` is removed too: it pairs with the expiry time, and a lone creation time is useless:
 * nobody makes decisions by "this container was created at 06:05".
 *
 * ⚠️ Those two fields on `SandboxState` **are kept** (the backend still returns them; see the comment in data/api.ts).
 * This only stops rendering them; it does not narrow the contract.
 */
function SandboxMeta({ state }: { state: SandboxState }) {
  const { t } = usePrefs();
  const status = state.status as Exclude<SandboxStatus, "none">;

  return (
    <div className="flex items-center gap-2">
      <Chip size="sm" color={STATUS_TONE[status]} variant="soft" data-testid="sandbox-status">
        {t(`sandbox.${status}`)}
      </Chip>
      {/* `min-w-0` makes truncate work: the container id has 38 characters, and without truncation on narrow screens it would
          stretch this row and push the things after it out of the viewport. */}
      {state.sessionId && (
        <span className="text-muted min-w-0 truncate font-mono text-xs">{state.sessionId}</span>
      )}
    </div>
  );
}

/**
 * Status line of the host executor: a neutral icon + the host name.
 *
 * It does not check online status (v1 stays simple; list polling is only done on the settings page): this line only answers "which machine
 * this turn lives on", and whether the daemon is online is shown faithfully by the terminal's own connection status line.
 */
function HostMeta({ hostName }: { hostName?: string }) {
  const { t } = usePrefs();
  return (
    <div className="flex items-center gap-2" data-testid="sandbox-host-meta">
      {/* A small neutral pill (no semantic color): a host is another execution form, not a success/warning state.
          The shape follows the "compatibility mode" badge in the terminal. */}
      <span className="r-inset flex items-center gap-1 bg-surface-secondary px-1.5 py-0.5 text-xs">
        <Icon icon="gravity-ui:cpu" className="size-3 text-muted" />
        {t("sandbox.host.badge")}
      </span>
      <span className="min-w-0 truncate text-xs text-muted">
        {hostName || t("sandbox.host.fallbackName")}
      </span>
    </div>
  );
}

/**
 * Host card in the files tab: explains this turn runs on a host + an entry into the terminal.
 *
 * ⚠️ **No file browsing**: in v1 hosts do not have the container file list endpoints; an entry that is sure to
 * 404 when clicked is worse than saying "use the terminal", so here we say it clearly and make the only entry (terminal)
 * the primary button. The terminal entry still uses ?view=terminal, and the WS/ticket endpoints are reused.
 */
function HostRunningCard({
  hostName,
  onOpenTerminal,
}: {
  hostName?: string;
  onOpenTerminal: () => void;
}) {
  const { t } = usePrefs();
  return (
    <div
      className="r-inset flex flex-col items-start gap-3 bg-surface-secondary px-4 py-5"
      data-testid="sandbox-host-card"
    >
      <div className="flex items-center gap-2">
        <Icon icon="gravity-ui:cpu" className="size-5 text-muted" />
        <div className="flex flex-col">
          <p className="text-sm font-medium">{t("sandbox.host.title")}</p>
          <p className="text-xs text-muted">
            {t("sandbox.host.desc", { name: hostName || t("sandbox.host.fallbackName") })}
          </p>
        </div>
      </div>
      <p className="text-xs text-muted">{t("sandbox.host.filesUnsupported")}</p>
      <Button size="sm" variant="outline" onPress={onOpenTerminal} data-testid="sandbox-host-terminal">
        <Icon icon="gravity-ui:terminal" className="size-4" />
        {t("sandbox.host.openTerminal")}
      </Button>
    </div>
  );
}

/**
 * File list: **read-only, single level, fetched level by level**.
 *
 * No delete/overwrite/upload: that is another matter with a completely different risk (one wrong click could destroy the workspace the agent is using);
 * if ever done it should be designed and permissioned separately.
 */
/**
 * The user's home directory in the container.
 *
 * **Start from here by default, not `/`**: this is where the agent works (the tools' cwd, the code it clones,
 * and the outputs it writes are all under the home directory), while the 22 entries in `/` are all system directories like bin/boot/dev/proc,
 * and none of what the user wants is on the first screen.
 *
 * The working directory of the Docker session container is `/workspace` (the default of `docker.workdir`, also the tools' cwd).
 * If the image ever switches users and it cannot be listed, it falls back to `/` (see the catch in SandboxFiles).
 */
const SANDBOX_HOME = "/workspace";

function SandboxFiles({ runId }: { runId: string }) {
  const { t } = usePrefs();
  const [path, setPath] = useState(SANDBOX_HOME);
  // The selected file: when non-empty, its content is shown on the right.
  const [picked, setPicked] = useState<SandboxFile | null>(null);
  const [files, setFiles] = useState<SandboxFile[] | null>(null);
  const [truncated, setTruncated] = useState(false);

  useEffect(() => {
    let alive = true;
    setFiles(null);
    void api
      .listRunSandboxFiles(runId, path)
      .then((listing) => {
        if (!alive) return;
        setFiles(listing.files);
        setTruncated(listing.truncated);
      })
      // A bare .then must come with a .catch; see that note in the file header of lib/useAsyncData.ts.
      .catch(() => {
        if (!alive) return;
        // If the home directory cannot be listed (the image switched users or the like), fall back to the root, so the panel is not empty.
        if (path === SANDBOX_HOME) {
          setPath("/");
          return;
        }
        setFiles([]);
      });
    return () => {
      alive = false;
    };
  }, [runId, path]);

  const parent = path === "/" ? null : path.slice(0, path.lastIndexOf("/")) || "/";
  const openDir = (next: string) => {
    setPicked(null);
    setPath(next);
  };

  return (
    <div className="flex flex-col gap-2" data-testid="sandbox-files">
      <div className="flex items-center gap-2">
        <span className="text-sm font-medium">{t("sandbox.files")}</span>
        <Chip size="sm" variant="soft">
          {t("sandbox.readOnly")}
        </Chip>
        <span className="text-muted ml-auto truncate font-mono text-xs">{path}</span>
      </div>

      <div className="flex flex-col">
        {parent !== null && (
          <button
            type="button"
            className="hover:bg-surface-secondary flex items-center gap-2 rounded-lg px-2 py-1.5 text-left text-sm cursor-[var(--cursor-interactive)]"
            data-testid="sandbox-file-parent"
            onClick={() => openDir(parent)}
          >
            <Icon icon="gravity-ui:arrow-up" className="text-muted size-4 shrink-0" />
            <span className="text-muted">{t("sandbox.parentDir")}</span>
          </button>
        )}

        {files?.length === 0 && (
          <p className="text-muted px-2 py-3 text-sm">{t("sandbox.filesEmpty")}</p>
        )}

        {files?.map((f) => (
          <SandboxFileRow
            key={f.path}
            file={f}
            active={picked?.path === f.path}
            onOpen={() => (f.type === "directory" ? openDir(f.path) : setPicked(f))}
          />
        ))}
      </div>

      {picked && <SandboxFileDetail runId={runId} file={picked} onClose={() => setPicked(null)} />}

      {/* When truncated, **say so**: otherwise "this is all the directory has" is an illusion,
          and users would go look in the container for a file that actually exists. */}
      {truncated && (
        <p className="text-muted px-2 text-xs">
          {t("sandbox.filesTruncated").replace("{count}", String(files?.length ?? 0))}
        </p>
      )}
    </div>
  );
}

/** One file row. Directories can be clicked into; files and symlinks are only shown. */
function SandboxFileRow({
  file,
  active,
  onOpen,
}: {
  file: SandboxFile;
  active: boolean;
  onOpen: () => void;
}) {
  // ⚠️ The type always follows the type field from the platform; **do not compute it from the mode bits yourself**:
  // the Unix file type is an enum, not flag bits, and `mode & S_IFLNK` is true for every regular file.
  const isDir = file.type === "directory";
  const icon = isDir
    ? "gravity-ui:folder"
    : file.type === "symlink"
      ? "gravity-ui:link"
      : "gravity-ui:file-text";

  const body = (
    <>
      <Icon icon={icon} className="text-muted size-4 shrink-0" />
      <span className="truncate">{file.name}</span>
      {file.symlinkTarget && (
        <span className="text-muted truncate font-mono text-xs">→ {file.symlinkTarget}</span>
      )}
      {!isDir && (
        <span className="text-muted ml-auto shrink-0 font-mono text-xs tabular-nums">
          {formatBytes(file.size)}
        </span>
      )}
    </>
  );

  // Directories open on click, files open to show their content: both are interactive, so both are buttons.
  // Symlinks are not clickable: following them would need resolving the target first, which is another request, not worth the complexity.
  if (file.type === "symlink") {
    return (
      <div className="flex items-center gap-2 px-2 py-1.5 text-sm" data-testid="sandbox-file">
        {body}
      </div>
    );
  }
  return (
    <button
      type="button"
      className={`flex items-center gap-2 rounded-lg px-2 py-1.5 text-left text-sm cursor-[var(--cursor-interactive)] ${
        active ? "bg-surface-secondary" : "hover:bg-surface-secondary"
      }`}
      data-testid="sandbox-file"
      data-active={active}
      onClick={onOpen}
    >
      {body}
    </button>
  );
}

/** Human-readable byte count. From KB up it uses base 1024, same as `ls -lh`. */
function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / 1024 / 1024).toFixed(1)} MB`;
}

/**
 * File detail: click a file to see its content.
 *
 * **Read-only, text only.** Binary files get no content (the backend returns `binary:true`): rendering random bytes
 * on the page is pointless and could break the layout. Files that are too big are truncated to 256 KiB, and it **says so**;
 * otherwise users would think that is the file's full size.
 *
 * No syntax highlighting: that would pull in a highlighter (the md-box prism path),
 * while this is for "glancing at logs/config"; monospace + wrapping is enough. To look closely, use the terminal.
 */
function SandboxFileDetail({
  runId,
  file,
  onClose,
}: {
  runId: string;
  file: SandboxFile;
  onClose: () => void;
}) {
  const { t } = usePrefs();
  const [body, setBody] = useState<SandboxFileContent | null>(null);
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    let alive = true;
    setBody(null);
    setFailed(false);
    void api
      .readRunSandboxFile(runId, file.path)
      .then((c) => {
        if (alive) setBody(c);
      })
      // A bare .then must come with a .catch; see that note in the file header of lib/useAsyncData.ts.
      .catch(() => {
        if (alive) setFailed(true);
      });
    return () => {
      alive = false;
    };
  }, [runId, file.path]);

  return (
    <div className="flex flex-col gap-2" data-testid="sandbox-file-detail">
      <div className="flex items-center gap-2">
        <Icon icon="gravity-ui:file-text" className="text-muted size-4 shrink-0" />
        <span className="truncate font-mono text-xs">{file.path}</span>
        <Button
          size="sm"
          variant="ghost"
          isIconOnly
          className="ml-auto"
          aria-label={t("sandbox.fileClose")}
          data-testid="sandbox-file-close"
          onPress={onClose}
        >
          <Icon icon="gravity-ui:xmark" />
        </Button>
      </div>

      {failed && <p className="text-danger text-xs">{t("sandbox.fileLoadFailed")}</p>}

      {body?.binary && <p className="text-muted text-xs">{t("sandbox.fileBinary")}</p>}

      {body && !body.binary && (
        <pre
          className="bg-surface-secondary max-h-80 overflow-auto rounded-xl p-3 font-mono text-xs whitespace-pre-wrap break-all"
          data-testid="sandbox-file-content"
        >
          {body.content}
        </pre>
      )}

      {/* When truncated, say so, same as the file list. */}
      {body?.truncated && <p className="text-muted text-xs">{t("sandbox.fileTruncated")}</p>}
    </div>
  );
}

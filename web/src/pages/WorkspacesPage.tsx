/**
 * Workspace list. This is the only page outside a workspace.
 *
 * **When the list is empty it is still this list page**, only the card grid is replaced by an
 * empty-state hint; the page header, preferences entry
 * and "New workspace" button are all still there. This used to be "replace the whole page with a
 * create form", which had two problems:
 *
 * 1. The preferences entry went away too, and /workspaces is the only reachable page when there
 *    are no workspaces;
 * 2. **An empty list has two causes, and the frontend cannot tell them apart** (the backend
 *    returns `[]` for both): it may be a new user who really
 *    has no workspace yet, or this person is not in any workspace's member list
 *    (`ListWorkspacesFor` only returns
 *    workspaces the actor is a member of). The latter was sent the wrong way by "create a
 *    workspace first": what they should do
 *    is ask an admin to add them, not create another one. So the empty-state text offers both paths.
 */

import { useCallback, useEffect, useRef, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import {
  Button,
  Card,
  Description,
  FieldError,
  Input,
  Label,
  Modal,
  TextArea,
  TextField,
} from "@heroui/react";
import { api } from "@/data/api";
import { Empty, ListSkeleton, PageBody } from "@/components/Page";
import { formatRelative } from "@/lib/format";
import { slugifyName } from "@/lib/slug";
import { validateWorkspaceKey, type Workspace } from "@/types/domain";
import { usePrefs } from "@/prefs/PrefsContext";

/** Create form. The dialog is the only container: both empty and non-empty lists use setCreating(true), no second copy. */
function CreateWorkspaceForm({
  onCreated,
  onCancel,
}: {
  onCreated: (ws: Workspace) => void;
  onCancel?: () => void;
}) {
  const { t } = usePrefs();
  const [name, setName] = useState("");
  const [key, setKey] = useState("");
  const [description, setDescription] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [keyError, setKeyError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  // While the key has not been edited by hand it follows the name; once edited it is no longer
  // auto-overwritten (keyEditedRef tracks this;
  // it is only read by callbacks and no UI re-renders on it, so it is a ref, not state).
  //
  // ⚠️ **Derivation is async** (the pinyin dictionary loads on demand, see lib/slug.ts), so there
  // are two races to guard against:
  //
  //  1. With fast typing several promises run at once; an earlier one may arrive later and write
  //     the key derived from a stale name.
  //     latestName remembers the latest input; when an await returns and it is not the latest, drop it.
  //  2. The user may edit the Key by hand **while the dictionary is loading**. At that moment it
  //     should become true, but the closure captured
  //     the old value (false), and writing as usual would wipe what they just typed. So read the
  //     **current** state from the ref,
  //     not the snapshot in the closure.
  const latestName = useRef("");
  const keyEditedRef = useRef(false);
  function handleName(v: string) {
    setName(v);
    latestName.current = v;
    if (keyEditedRef.current) return;
    void slugifyName(v).then((derived) => {
      if (latestName.current !== v || keyEditedRef.current) return;
      setKey(derived);
    });
  }

  function handleKey(v: string) {
    keyEditedRef.current = true;
    setKey(v);
    setKeyError(validateWorkspaceKey(v, t));
  }

  async function submit() {
    const trimmedName = name.trim();
    if (!trimmedName) {
      setError(t("ws.nameRequired"));
      return;
    }
    const ke = validateWorkspaceKey(key, t);
    if (ke) {
      setKeyError(ke);
      return;
    }
    setSubmitting(true);
    setError(null);
    try {
      if (await api.isWorkspaceKeyTaken(key)) {
        setKeyError(t("ws.keyTaken", { key }));
        return;
      }
      const ws = await api.createWorkspace({
        name: trimmedName,
        key,
        description: description.trim() || undefined,
      });
      onCreated(ws);
    } catch (e) {
      setError(e instanceof Error ? e.message : t("ws.createFailed"));
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <div className="flex flex-col gap-5">
      <TextField isRequired>
        <Label>{t("ws.name")}</Label>
        <Input
          placeholder={t("ws.namePlaceholder")}
          value={name}
          onChange={(e) => handleName(e.target.value)}
          data-testid="ws-name"
          autoFocus
        />
      </TextField>

      <TextField isRequired isInvalid={Boolean(keyError)}>
        <Label>{t("ws.key")}</Label>
        <Input
          placeholder="growth"
          value={key}
          onChange={(e) => handleKey(e.target.value)}
          data-testid="ws-key"
        />
        <Description>{t("ws.keyDesc")}</Description>
        {keyError ? <FieldError>{keyError}</FieldError> : null}
      </TextField>

      <TextField>
        <Label>{t("ws.desc")}</Label>
        <TextArea
          placeholder={t("ws.descPlaceholder")}
          rows={2}
          value={description}
          onChange={(e) => setDescription(e.target.value)}
          data-testid="ws-desc"
        />
      </TextField>

      {error ? (
        <p className="text-sm text-danger" role="alert">
          {error}
        </p>
      ) : null}

      <div className="flex flex-col gap-2">
        <Button onPress={submit} isPending={submitting} data-testid="ws-submit">
          {t("ws.create")}
        </Button>
        {onCancel ? (
          <Button variant="ghost" onPress={onCancel}>
            {t("common.cancel")}
          </Button>
        ) : null}
      </div>
    </div>
  );
}

export function WorkspacesPage() {
  const { t, language } = usePrefs();
  const navigate = useNavigate();
  const [list, setList] = useState<Workspace[] | null>(null);
  const [creating, setCreating] = useState(false);

  const load = useCallback(async () => {
    // **Must catch**: it used to be a bare await, and any failure left list stuck at null forever,
    // so the page stayed on the skeleton, with no error and no visible cause.
    //
    // On failure fall back to an empty list (the empty state below): for this page "cannot see a
    // single workspace" and
    // "cannot list them" are the same thing to the user, and the empty state at least is a usable
    // screen. Its two paths
    // (create / ask an admin) are not misleading for the "read failed" cause either. The real cause
    // is still in the console.
    try {
      setList(await api.listWorkspaces());
    } catch (e) {
      console.error("列空间失败", e);
      setList([]);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  function enter(ws: Workspace) {
    navigate(`/w/${ws.key}`);
  }

  if (list === null) {
    return (
      <PageBody>
        <ListSkeleton rows={3} />
      </PageBody>
    );
  }

  return (
    <PageBody>
      <header className="flex items-start justify-between gap-4">
        <div className="flex flex-col gap-1">
          <h1 className="text-2xl font-semibold tracking-tight">{t("ws.pickTitle")}</h1>
          <p className="text-sm text-muted">{t("ws.pickDesc")}</p>
        </div>
        {/* Preferences are global, so the entry lives here: /workspaces is the only reachable page when there are no workspaces.
            That is why it must be in the **shell**, not only on the "has workspaces" branch. */}
        <div className="flex items-center gap-2">
          <Link to="/preferences" className="no-underline">
            <Button variant="outline" data-testid="open-preferences">
              {t("nav.preferences")}
            </Button>
          </Link>
          <Button onPress={() => setCreating(true)} data-testid="new-workspace">
            {t("ws.new")}
          </Button>
        </div>
      </header>

      {list.length === 0 ? (
        // The empty state only replaces the card grid. The text must cover both causes; see the file header.
        <Empty
          icon="gravity-ui:folder"
          title={t("ws.emptyTitle")}
          description={t("ws.emptyDesc")}
          action={
            <Button onPress={() => setCreating(true)} data-testid="empty-new-workspace">
              {t("ws.new")}
            </Button>
          }
        />
      ) : (
        <div className="grid gap-4 sm:grid-cols-2">
          {list.map((ws) => (
            <Card key={ws.id}>
              <Card.Content className="flex flex-col gap-4 pt-6">
                <div className="flex items-start gap-3">
                  <span className="flex size-9 shrink-0 items-center justify-center r-chip bg-accent text-sm font-semibold text-accent-foreground">
                    {ws.name.slice(0, 1)}
                  </span>
                  <div className="flex min-w-0 flex-col gap-0.5">
                    <p className="truncate font-medium">{ws.name}</p>
                    <p className="truncate text-xs text-muted num">{ws.key}</p>
                  </div>
                </div>
                {ws.description ? (
                  <p className="clamp-2 text-sm text-muted">{ws.description}</p>
                ) : null}
                <div className="flex items-center gap-4 text-xs text-muted">
                  <span className="num">{t("ws.memberCount", { count: ws.members.length })}</span>
                  <span>{t("ws.createdAt", { time: formatRelative(ws.createdAt, language) })}</span>
                </div>
                <Button variant="outline" onPress={() => enter(ws)} data-testid={`enter-${ws.key}`}>
                  {t("ws.enter")}
                </Button>
              </Card.Content>
            </Card>
          ))}
        </div>
      )}

      <Modal.Backdrop isOpen={creating} onOpenChange={setCreating}>
        <Modal.Container size="sm">
          <Modal.Dialog>
            <Modal.Header>
              <Modal.Heading>{t("ws.new")}</Modal.Heading>
            </Modal.Header>
            <Modal.Body>
              <CreateWorkspaceForm
                onCreated={(ws) => {
                  setCreating(false);
                  enter(ws);
                }}
                onCancel={() => setCreating(false)}
              />
            </Modal.Body>
          </Modal.Dialog>
        </Modal.Container>
      </Modal.Backdrop>
    </PageBody>
  );
}

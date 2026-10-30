/**
 * WeChat bots of the workspace (server/weixin).
 *
 * Connecting is a QR-code login done in place: the panel at the top shows the code, follows the
 * login with a poll loop, and asks for the number shown on the phone when WeChat wants one. Each
 * connected bot is a card with an agent picker: the bound agent answers the messages the bot gets.
 *
 * The poll loop calls the next poll right after the previous one returns: the server already waits
 * up to ~20 seconds on WeChat's long poll, so a client-side interval would only add delay.
 */

import { useCallback, useEffect, useState } from "react";
import {
  Button,
  Card,
  Chip,
  Input,
  ListBox,
  Select,
  TextField,
  Tooltip,
} from "@heroui/react";
import { Icon } from "@iconify/react";
import { Link } from "react-router-dom";
import { api } from "@/data/api";
import { useWorkspace, useWorkspacePath } from "@/app/WorkspaceGate";
import { AsyncErrorBlock, Empty, ListSkeleton, PageBody, PageHeader } from "@/components/Page";
import { ConfirmModal } from "@/components/FormModal";
import { formatRelative } from "@/lib/format";
import { useAsyncData } from "@/lib/useAsyncData";
import {
  WEIXIN_LOGIN_DONE,
  type Agent,
  type WeixinAccount,
  type WeixinLogin,
} from "@/types/domain";
import { usePrefs } from "@/prefs/PrefsContext";

/** Select key for "no agent": ListBox keys must not be empty strings. */
const NO_AGENT = "__none__";

export function WeixinPage() {
  const { t } = usePrefs();
  const { workspace } = useWorkspace();
  const [login, setLogin] = useState<WeixinLogin | null>(null);
  const [starting, setStarting] = useState(false);
  const [startError, setStartError] = useState("");
  const [deleting, setDeleting] = useState<WeixinAccount | null>(null);

  const fetch = useCallback(async () => {
    const [accounts, agents] = await Promise.all([
      api.listWeixinAccounts(workspace.id),
      api.listAgents(workspace.id),
    ]);
    return { accounts, agents };
  }, [workspace.id]);
  const { data, error, isLoading, reload, setData } = useAsyncData(
    fetch,
    t("common.loadFailedUnknown"),
  );
  const accounts = data?.accounts ?? null;
  const agents = data?.agents ?? [];

  async function startLogin() {
    setStarting(true);
    setStartError("");
    try {
      setLogin(await api.startWeixinLogin(workspace.id));
    } catch (e) {
      setStartError(e instanceof Error ? e.message : String(e));
    } finally {
      setStarting(false);
    }
  }

  function replaceAccount(next: WeixinAccount) {
    setData((prev) =>
      prev
        ? { ...prev, accounts: prev.accounts.map((a) => (a.id === next.id ? next : a)) }
        : prev,
    );
  }

  const onLoginDone = useCallback(
    (done: WeixinLogin) => {
      if (done.status === "confirmed" || done.status === "already_connected") void reload();
    },
    [reload],
  );

  const connectButton = (
    <Button onPress={() => void startLogin()} isPending={starting} data-testid="weixin-connect">
      {t("weixin.connect")}
    </Button>
  );

  return (
    <PageBody>
      <PageHeader
        title={t("nav.weixin")}
        description={t("weixin.listDesc")}
        actions={login ? null : connectButton}
      />

      {startError ? (
        <p className="mb-4 text-sm text-danger" data-testid="weixin-start-error">
          {startError}
        </p>
      ) : null}

      {login ? (
        <LoginPanel
          workspaceId={workspace.id}
          login={login}
          onChange={setLogin}
          onDone={onLoginDone}
          onClose={() => setLogin(null)}
          onRestart={() => void startLogin()}
        />
      ) : null}

      {isLoading ? (
        <ListSkeleton />
      ) : error ? (
        <AsyncErrorBlock error={error} onRetry={reload} />
      ) : !accounts ? null : accounts.length === 0 ? (
        login ? null : (
          <Empty
            icon="gravity-ui:smartphone"
            title={t("weixin.emptyTitle")}
            description={t("weixin.emptyDesc")}
            action={connectButton}
          />
        )
      ) : (
        <div className="flex flex-col gap-4">
          {accounts.map((acc) => (
            <AccountCard
              key={acc.id}
              account={acc}
              agents={agents}
              onChanged={replaceAccount}
              onDelete={() => setDeleting(acc)}
            />
          ))}
        </div>
      )}

      <ConfirmModal
        isOpen={deleting !== null}
        onOpenChange={(open) => !open && setDeleting(null)}
        title={t("weixin.deleteTitle")}
        description={t("weixin.deleteDesc")}
        confirmLabel={t("weixin.deleteAction")}
        onConfirm={async () => {
          if (!deleting) return;
          await api.deleteWeixinAccount(workspace.id, deleting.id);
          setDeleting(null);
          await reload();
        }}
      />
    </PageBody>
  );
}

// -- Login panel ---------------------------------------------------------------

function LoginPanel({
  workspaceId,
  login,
  onChange,
  onDone,
  onClose,
  onRestart,
}: {
  workspaceId: string;
  login: WeixinLogin;
  onChange: (next: WeixinLogin) => void;
  onDone: (done: WeixinLogin) => void;
  onClose: () => void;
  onRestart: () => void;
}) {
  const { t } = usePrefs();
  const [pollError, setPollError] = useState("");
  const [code, setCode] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const done = WEIXIN_LOGIN_DONE.includes(login.status);
  const loginId = login.id;

  // Poll until a final status. Recursive (not setInterval), cancelled on unmount or a new login.
  useEffect(() => {
    if (done) return;
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    async function tick() {
      try {
        const next = await api.pollWeixinLogin(workspaceId, loginId);
        if (cancelled) return;
        onChange(next);
        if (WEIXIN_LOGIN_DONE.includes(next.status)) {
          onDone(next);
          return;
        }
        timer = setTimeout(() => void tick(), 500);
      } catch (e) {
        if (!cancelled) setPollError(e instanceof Error ? e.message : String(e));
      }
    }
    setPollError("");
    void tick();
    return () => {
      cancelled = true;
      if (timer) clearTimeout(timer);
    };
    // Re-run only for a new login or a change of "done"; each poll result updates `login` itself.
  }, [workspaceId, loginId, done]);

  async function submitCode() {
    setSubmitting(true);
    try {
      onChange(await api.submitWeixinVerifyCode(workspaceId, loginId, code.trim()));
      setCode("");
    } catch (e) {
      setPollError(e instanceof Error ? e.message : String(e));
    } finally {
      setSubmitting(false);
    }
  }

  const success = login.status === "confirmed" || login.status === "already_connected";

  return (
    <Card className="mb-6" data-testid="weixin-login-panel">
      <Card.Content className="flex flex-col gap-5 pt-6 sm:flex-row sm:items-start">
        {login.qrcodeDataUrl ? (
          <img
            src={login.qrcodeDataUrl}
            alt={t("weixin.qrAlt")}
            className="size-48 shrink-0 self-center r-inset bg-white p-2 sm:self-start"
            data-testid="weixin-qr"
          />
        ) : null}
        <div className="flex min-w-0 flex-1 flex-col gap-3">
          <div className="flex items-center gap-2">
            <p className="font-medium">{t("weixin.loginTitle")}</p>
            <Chip
              size="sm"
              variant="soft"
              color={success ? "success" : done ? "danger" : "default"}
              data-testid="weixin-login-status"
            >
              <Chip.Label>{t(`enum.weixinLogin.${login.status}`)}</Chip.Label>
            </Chip>
          </div>
          {login.message ? <p className="text-sm text-muted">{login.message}</p> : null}

          {login.status === "need_verify_code" ? (
            <div className="flex flex-wrap items-end gap-2">
              <TextField className="w-48">
                <Input
                  variant="secondary"
                  aria-label={t("weixin.verifyCode")}
                  placeholder={t("weixin.verifyCode")}
                  value={code}
                  inputMode="numeric"
                  onChange={(e) => setCode(e.target.value)}
                  data-testid="weixin-verify-code"
                />
              </TextField>
              <Button
                isDisabled={!code.trim()}
                isPending={submitting}
                onPress={() => void submitCode()}
              >
                {t("weixin.verifySubmit")}
              </Button>
            </div>
          ) : null}

          {pollError ? <p className="text-sm text-danger">{pollError}</p> : null}

          <div className="flex flex-wrap gap-2">
            {done && !success ? (
              <Button onPress={onRestart}>{t("weixin.retry")}</Button>
            ) : null}
            <Button variant="ghost" onPress={onClose} data-testid="weixin-login-close">
              {done ? t("weixin.close") : t("common.cancel")}
            </Button>
          </div>
        </div>
      </Card.Content>
    </Card>
  );
}

// -- Account card --------------------------------------------------------------

function AccountCard({
  account,
  agents,
  onChanged,
  onDelete,
}: {
  account: WeixinAccount;
  agents: Agent[];
  onChanged: (next: WeixinAccount) => void;
  onDelete: () => void;
}) {
  const { t, language } = usePrefs();
  const wp = useWorkspacePath();
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const bound = agents.find((a) => a.id === account.agentId) ?? null;

  async function bind(agentId: string) {
    if (agentId === account.agentId) return;
    setSaving(true);
    setError("");
    try {
      onChanged(await api.bindWeixinAgent(account.workspaceId, account.id, agentId));
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setSaving(false);
    }
  }

  return (
    <Card data-testid={`weixin-card-${account.id}`}>
      <Card.Content className="flex flex-col gap-5 pt-6">
        <div className="flex items-start justify-between gap-4">
          <div className="flex min-w-0 items-start gap-3">
            <span className="flex size-9 shrink-0 items-center justify-center r-chip bg-surface-secondary">
              <Icon icon="gravity-ui:smartphone" className="size-4 text-muted" />
            </span>
            <div className="flex min-w-0 flex-col gap-1">
              <div className="flex items-center gap-2">
                <p className="truncate font-mono text-sm">{account.botId}</p>
                <Chip size="sm" variant="soft" color={account.status === "active" ? "success" : "danger"}>
                  <Chip.Label>{t(`enum.weixinAccount.${account.status}`)}</Chip.Label>
                </Chip>
              </div>
              <p className="truncate text-xs text-muted">
                {t("weixin.owner", { user: account.ownerUserId || "-" })}
              </p>
            </div>
          </div>
          <Tooltip>
            <Tooltip.Trigger>
              <Button
                variant="ghost"
                isIconOnly
                aria-label={t("weixin.deleteAction")}
                onPress={onDelete}
                data-testid={`weixin-delete-${account.id}`}
              >
                <Icon icon="gravity-ui:trash-bin" className="size-4" />
              </Button>
            </Tooltip.Trigger>
            <Tooltip.Content>{t("weixin.deleteAction")}</Tooltip.Content>
          </Tooltip>
        </div>

        {account.status === "expired" ? (
          <p className="text-sm text-danger">{account.lastError || t("weixin.expiredHint")}</p>
        ) : null}

        <div className="flex flex-col gap-2">
          <p className="text-xs text-muted">{t("weixin.boundAgent")}</p>
          {agents.length === 0 ? (
            <div className="flex flex-col gap-2">
              <p className="text-sm text-muted">{t("feishu.bindNoAgents")}</p>
              <div>
                <Link to={wp("/agents/new")} className="no-underline">
                  <Button>{t("agent.new")}</Button>
                </Link>
              </div>
            </div>
          ) : (
            <div className="flex flex-wrap items-center gap-2">
              <Select
                className="w-56"
                variant="secondary"
                aria-label={t("weixin.pickAgent")}
                placeholder={t("weixin.pickAgent")}
                selectedKey={account.agentId || NO_AGENT}
                isDisabled={saving}
                onSelectionChange={(k) => {
                  if (k === null) return;
                  void bind(String(k) === NO_AGENT ? "" : String(k));
                }}
              >
                <Select.Trigger data-testid={`weixin-agent-select-${account.id}`}>
                  {/* A deleted agent is not in the list; show its id instead of a blank picker. */}
                  <Select.Value>
                    {({ defaultChildren, state }) =>
                      state.selectedItems.length === 0 && account.agentId
                        ? account.agentId
                        : defaultChildren
                    }
                  </Select.Value>
                  <Select.Indicator />
                </Select.Trigger>
                <Select.Popover>
                  <ListBox>
                    <ListBox.Item id={NO_AGENT} textValue={t("weixin.noAgent")}>
                      {t("weixin.noAgent")}
                    </ListBox.Item>
                    {agents.map((a) => (
                      <ListBox.Item key={a.id} id={a.id} textValue={a.name}>
                        {a.name}
                      </ListBox.Item>
                    ))}
                  </ListBox>
                </Select.Popover>
              </Select>
              {bound ? (
                <Link to={wp(`/agents/${bound.id}`)} className="text-sm text-muted">
                  {t("weixin.openAgent")}
                </Link>
              ) : null}
            </div>
          )}
          {!account.agentId ? <p className="text-sm text-muted">{t("weixin.unboundHint")}</p> : null}
          {error ? <p className="text-sm text-danger">{error}</p> : null}
        </div>

        <p className="text-xs text-muted">
          {t("weixin.connectedAt", { time: formatRelative(account.createdAt, language) })}
          {account.lastInboundAt
            ? ` · ${t("weixin.lastInbound", { time: formatRelative(account.lastInboundAt, language) })}`
            : ""}
        </p>
      </Card.Content>
    </Card>
  );
}

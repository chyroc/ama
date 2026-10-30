/**
 * Connect a Feishu app: device-code self-registration.
 *
 * Users no longer type AppID/AppSecret by hand: they click a link to authorize in Feishu, and this
 * page polls until it gets the credentials.
 *
 * Why a separate page and not a dialog: the flow has three steps, and the middle one has no
 * "submit" action
 * (you can only wait), which does not fit FormModal's two-button "cancel + submit" footer. Also
 * sessionId
 * lives in the URL, so a refresh continues polling instead of starting over.
 *
 * # Connecting is not the end
 *
 * The app is connected, but without an Agent handling it, it is a bot that never replies in
 * Feishu. So the main
 * spot of step three is "pick an Agent to bind" (1:1 exclusive, only one), and "Done" is demoted
 * to a secondary exit.
 * Earlier there was only "Done" there, and users stopped right after connecting.
 */

import { useCallback, useEffect, useState } from "react";
import { Button, Card, Checkbox, Chip } from "@heroui/react";
import { Icon } from "@iconify/react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { api } from "@/data/api";
import { useWorkspace, useWorkspacePath } from "@/app/WorkspaceGate";
import { PageBody, PageHeader } from "@/components/Page";
import { ReadinessCard } from "@/components/FeishuReadiness";
import { FeishuBindPrompt } from "@/components/FeishuBindPrompt";
import { useRegistrationPoll } from "@/lib/useRegistrationPoll";
import { usePrefs } from "@/prefs/PrefsContext";
import type { Agent, FeishuApp } from "@/types/domain";

/** Which of the three steps we are on. */
type Step = "idle" | "waiting" | "done" | "failed";

/** The step number dot on the left. */
function StepDot({ n, active, done }: { n: number; active: boolean; done: boolean }) {
  // Note it is text-accent-foreground, not text-on-accent: the latter is not a HeroUI token,
  // and Tailwind **silently produces no CSS** for unknown classes; in dark mode accent is near
  // white, giving white text on white.
  const cls =
    done || active ? "bg-accent text-accent-foreground" : "bg-surface-secondary text-muted";
  return (
    <span
      className={`flex size-6 shrink-0 items-center justify-center rounded-full text-xs font-medium ${cls}`}
    >
      {done ? <Icon icon="gravity-ui:check" className="size-3.5" /> : n}
    </span>
  );
}

function StepRow({
  n,
  active,
  done,
  title,
  children,
}: {
  n: number;
  active: boolean;
  done: boolean;
  title: string;
  children?: React.ReactNode;
}) {
  return (
    <div className="flex gap-3">
      <StepDot n={n} active={active} done={done} />
      <div className="flex min-w-0 flex-1 flex-col gap-2 pb-2">
        <p className={active || done ? "text-sm font-medium" : "text-sm text-muted"}>{title}</p>
        {active || done ? children : null}
      </div>
    </div>
  );
}

export function FeishuConnectPage() {
  const { t } = usePrefs();
  const { workspace } = useWorkspace();
  const wp = useWorkspacePath();
  const navigate = useNavigate();
  const [params, setParams] = useSearchParams();

  // sessionId is in the URL, so a refresh can resume.
  const sessionId = params.get("session");
  const [verificationUrl, setVerificationUrl] = useState("");
  const [interval, setIntervalSeconds] = useState(5);
  const [starting, setStarting] = useState(false);
  // Whether to also request the permission "receive group messages without being @-mentioned".
  //
  // **Unchecked by default**: it is a sensitive permission that needs admin approval, and asking
  // for more makes approvers hesitate more; also
  // only people who plan to use "auto-reply to new topics" need it.
  //
  // ⚠️ This choice **only counts at the moment "Go authorize in Feishu" is clicked**: it is baked
  // into the authorization page URL,
  // fixed as soon as the page opens, and cannot be changed later. So the text must say "to add it
  // later you must go through this again".
  const [wantGroupMessage, setWantGroupMessage] = useState(false);
  const [startError, setStartError] = useState("");
  /** Name of the Agent bound in step three. Once bound, stop urging the user to bind and show the result instead. */
  const [boundName, setBoundName] = useState("");

  const poll = useRegistrationPoll(workspace.id, sessionId, interval);

  /**
   * Agents in the workspace, for the picker in step three.
   *
   * **`null` means both "not fetched yet" and "fetch failed"**; both make FeishuBindPrompt fall
   * back to
   * the "go to the Agent list" exit, which really works in both cases. useAsyncData is
   * deliberately not used
   * here: failing to fetch agents should not turn the whole page into an error page; the
   * connection itself already succeeded.
   *
   * `.catch()` cannot be skipped: a reject on a bare `.then()` is an unhandled rejection,
   * and failure here is expected (403 / network), not a bug.
   */
  const [agents, setAgents] = useState<Agent[] | null>(null);
  useEffect(() => {
    let alive = true;
    api
      .listAgents(workspace.id)
      .then((list) => {
        if (alive) setAgents(list);
      })
      .catch((e: unknown) => console.error("列 Agent 失败（飞书接入页）", e));
    return () => {
      alive = false;
    };
  }, [workspace.id]);

  const begin = useCallback(async () => {
    setStarting(true);
    setStartError("");
    try {
      const reg = await api.beginFeishuRegistration(workspace.id, wantGroupMessage);
      setVerificationUrl(reg.verificationUrl);
      setIntervalSeconds(reg.pollIntervalSeconds);
      // Write it into the URL; the hook starts polling right away.
      setParams({ session: reg.sessionId }, { replace: true });
      // Open the authorization page directly, saving a click. If the popup is blocked there is an
      // explicit button below.
      window.open(reg.verificationUrl, "_blank", "noopener,noreferrer");
    } catch (e) {
      setStartError(e instanceof Error ? e.message : String(e));
    } finally {
      setStarting(false);
    }
  }, [workspace.id, setParams, wantGroupMessage]);

  function restart() {
    setVerificationUrl("");
    setStartError("");
    // "Connect another" walks the flow again; the previous app's binding result must not stay on
    // the page,
    // otherwise step three of the new app would show someone else's "handed to XXX", looking as
    // if it were already bound.
    setBoundName("");
    setParams({}, { replace: true });
  }

  const step: Step = !sessionId
    ? "idle"
    : poll.status === "completed"
      ? "done"
      : poll.status === "pending"
        ? "waiting"
        : "failed";
  const app: FeishuApp | null = poll.app;

  return (
    <PageBody>
      <PageHeader
        title={t("feishu.connectTitle")}
        description={t("feishu.connectDesc")}
        actions={
          <Button variant="ghost" onPress={() => navigate(wp("/feishu"))} data-testid="connect-back">
            {t("feishu.backToList")}
          </Button>
        }
      />

      <Card>
        <Card.Content className="flex flex-col gap-4 pt-6">
          {/* Step one: go authorize in Feishu */}
          <StepRow
            n={1}
            active={step === "idle"}
            done={step !== "idle"}
            title={t("feishu.step1Title")}
          >
            <p className="text-sm text-muted">{t("feishu.step1Desc")}</p>
            {step === "idle" ? (
              <div className="flex flex-col gap-3 pt-1">
                {/* Placed **above** the button: clicking it opens the authorization page, and this choice is fixed then;
                    putting it below would let users see it only after they can no longer go back. */}
                <Checkbox
                  isSelected={wantGroupMessage}
                  onChange={setWantGroupMessage}
                  data-testid="connect-want-group-message"
                >
                  <Checkbox.Content>
                    <Checkbox.Control>
                      <Checkbox.Indicator />
                    </Checkbox.Control>
                    <span className="flex flex-col gap-0.5">
                      <span className="text-sm">{t("feishu.requestGroupMessage")}</span>
                      <span className="text-xs text-muted">
                        {t("feishu.requestGroupMessageHint")}
                      </span>
                    </span>
                  </Checkbox.Content>
                </Checkbox>
                <div>
                  <Button onPress={() => void begin()} isPending={starting} data-testid="begin-connect">
                    <Icon icon="gravity-ui:arrow-up-right-from-square" className="size-4" />
                    {t("feishu.openAuthPage")}
                  </Button>
                </div>
                {startError ? (
                  <div className="flex items-start gap-2 r-inset bg-danger/5 px-3 py-2.5">
                    <Icon
                      icon="gravity-ui:triangle-exclamation"
                      className="mt-0.5 size-4 shrink-0 text-danger"
                    />
                    <p className="text-sm text-danger" data-testid="connect-error">
                      {startError}
                    </p>
                  </div>
                ) : null}
              </div>
            ) : null}
          </StepRow>

          {/* Step two: wait for the user to click approve in Feishu */}
          <StepRow
            n={2}
            active={step === "waiting"}
            done={step === "done"}
            title={t("feishu.step2Title")}
          >
            {step === "waiting" ? (
              <div className="flex flex-col gap-2">
                <div className="flex items-center gap-2">
                  <Icon
                    icon="gravity-ui:arrows-rotate-right"
                    className="size-4 animate-spin text-muted"
                  />
                  <p className="text-sm text-muted" data-testid="connect-waiting">
                    {t("feishu.step2Waiting")}
                  </p>
                </div>
                {/* Fallback entry when the browser blocks the popup. */}
                {verificationUrl ? (
                  <a
                    href={verificationUrl}
                    target="_blank"
                    rel="noreferrer"
                    className="text-sm text-accent"
                    data-testid="auth-link"
                  >
                    {t("feishu.reopenAuthPage")}
                  </a>
                ) : null}
                <div className="pt-1">
                  <Button variant="outline" size="sm" onPress={restart} data-testid="connect-cancel">
                    {t("common.cancel")}
                  </Button>
                </div>
              </div>
            ) : null}
          </StepRow>

          {/* Step three: result */}
          <StepRow
            n={3}
            active={step === "done" || step === "failed"}
            done={step === "done"}
            title={t("feishu.step3Title")}
          >
            {step === "done" && app ? (
              <div className="flex flex-col gap-4">
                <div className="flex flex-wrap items-center gap-2">
                  <p className="text-sm font-medium" data-testid="connected-name">
                    {app.name}
                  </p>
                  <Chip size="sm" variant="soft">
                    <Chip.Label>
                      <span className="font-mono text-xs">{app.appId}</span>
                    </Chip.Label>
                  </Chip>
                </div>
                <ReadinessCard app={app} />

                {/* Next step: bind an Agent. If already bound, stop urging and show the result instead. */}
                {boundName ? (
                  <div
                    className="flex items-center gap-2 r-inset bg-surface-secondary px-3 py-2.5"
                    data-testid="connect-bound"
                  >
                    <Icon icon="gravity-ui:circle-check" className="size-4 shrink-0 text-success" />
                    <p className="text-sm">{t("feishu.boundToAgent", { name: boundName })}</p>
                  </div>
                ) : (
                  <div className="flex flex-col gap-2">
                    <p className="text-sm font-medium">{t("feishu.nextStepTitle")}</p>
                    <FeishuBindPrompt
                      app={app}
                      agents={agents}
                      onBound={(a) => setBoundName(a.name)}
                    />
                  </div>
                )}

                {/* Binding is the main line, so "Done" is demoted to a secondary exit, but it must always be there:
                    the user may just want to bind later. */}
                <div className="flex gap-2">
                  <Button
                    variant="outline"
                    onPress={() => navigate(wp("/feishu"))}
                    data-testid="connect-finish"
                  >
                    {t("feishu.finish")}
                  </Button>
                  <Button variant="ghost" onPress={restart}>
                    {t("feishu.connectAnother")}
                  </Button>
                </div>
              </div>
            ) : null}

            {step === "failed" ? (
              <div className="flex flex-col gap-2">
                <div className="flex items-start gap-2 r-inset bg-danger/5 px-3 py-2.5">
                  <Icon
                    icon="gravity-ui:triangle-exclamation"
                    className="mt-0.5 size-4 shrink-0 text-danger"
                  />
                  <p className="text-sm text-danger" data-testid="connect-failed">
                    {poll.reason || t(`feishu.failed.${poll.status}`)}
                  </p>
                </div>
                <div className="pt-1">
                  <Button variant="outline" onPress={restart} data-testid="connect-retry">
                    {t("feishu.retry")}
                  </Button>
                </div>
              </div>
            ) : null}
          </StepRow>
        </Card.Content>
      </Card>

      {/* Explains why credentials are not typed by hand; mirrors the explainer card for Ark in workspace settings:
          Ark has no OAuth so it must be typed by hand; Feishu has device codes so it need not be. */}
      <Card>
        <Card.Content className="flex flex-col gap-2 pt-6">
          <p className="text-sm font-medium">{t("feishu.whyTitle")}</p>
          <p className="text-sm text-muted">{t("feishu.whyDesc")}</p>
        </Card.Content>
      </Card>
    </PageBody>
  );
}

/**
 * Landing page when not logged in.
 *
 * Rendered by AuthGate on the anonymous branch; **it is not a route**.
 *
 * # One screen of product intro + one login card
 *
 * What the login card shows is decided by `GET /api/auth/providers` (deployment config, not
 * hard-coded):
 *
 * - **Email + password** (`auth.local`): a plain JSON request; on success the backend sets the
 *   HttpOnly
 *   Cookie and returns `{user}`, which is handed to AuthGate's `onLoggedIn`, **with no page reload**.
 *   When `signupOpen` is true a "sign up" toggle is shown; it is also true when the deployment has
 *   no accounts yet,
 *   and then the hint "the first account to sign up becomes the platform admin" is shown.
 * - **OIDC** (`auth.oidc`): a **full-page navigation** to `/api/auth/oidc/login?next=...`;
 *   the backend 303s to the IdP, receives the callback, sets the Cookie and sends you back to the
 *   original page. That is a fresh load,
 *   so AuthGate remounts and checks the login state again itself.
 * - **GitHub** (`auth.github`, device flow): clicking the button **does not leave this page**; the
 *   card is replaced by a
 *   "go to GitHub and enter this code" panel (big short code + copy + open
 *   github.com/login/device),
 *   polling `POST /api/auth/github/poll` at the interval the backend gives; on success `onLoggedIn`.
 *   The binding Cookie that polling needs is set by the backend (HttpOnly); here we only handle
 *   the flow id.
 * - None enabled: say plainly "no login method is configured" and name the two settings to
 *   configure. A blank login page with no way out
 *   is the worst state.
 *
 * # How failures are shown
 *
 * When the OIDC callback fails the backend 303s back to `/?login_error=<code>` (not JSON: the user
 * arrives with the whole
 * tab, and a screen of `{"error":...}` is neither readable nor gives a way out). Here that code is
 * turned into
 * a hint above the login card. Password login failures are translated by ApiError.code and shown
 * in the form.
 */

import { useCallback, useEffect, useRef, useState } from "react";
import type { FormEvent } from "react";
import { Button, Card, Form, Input, Label, Separator, Spinner, TextField } from "@heroui/react";
import { Icon } from "@iconify/react";
import { BrandMark } from "@/components/BrandMark";
import { CopyButton } from "@/components/CopyButton";
import { api } from "@/data/api";
import type { AuthProviders, AuthUser, GitHubDeviceStart } from "@/data/api";
import { ApiError } from "@/data/http";
import { useT } from "@/prefs/PrefsContext";
import type { MessageKey } from "@/prefs/messages";
import { ARK_MANAGED_AGENTS_URL } from "@/types/domain";

/** Reason code the backend 303s back with when the OIDC callback fails → text key. Unknown codes fall back to "service unavailable". */
const LOGIN_ERROR_KEYS: Record<string, MessageKey> = {
  denied: "login.errDenied",
  expired: "login.errExpired",
  no_identity: "login.errNoIdentity",
  email_unverified: "login.errEmailUnverified",
  unavailable: "login.errUnavailable",
};

/** ApiError.code on password login / sign-up failure → text key. Unknown codes fall back to errGeneric. */
const SUBMIT_ERROR_KEYS: Record<string, MessageKey> = {
  invalid_credentials: "login.errInvalidCredentials",
  too_many_attempts: "login.errTooManyAttempts",
  email_taken: "login.errEmailTaken",
  signup_disabled: "login.errSignupDisabled",
  weak_password: "login.errWeakPassword",
  local_disabled: "login.errLocalDisabled",
  invalid_argument: "login.errInvalidArgument",
};

/** ApiError.code on GitHub polling failure → text key. These are terminal: stop polling and offer "get a new code". */
const GITHUB_ERROR_KEYS: Record<string, MessageKey> = {
  github_expired: "login.errGithubExpired",
  github_denied: "login.errGithubDenied",
  github_no_verified_email: "login.errGithubNoEmail",
  github_not_allowed: "login.errGithubNotAllowed",
  github_disabled: "login.errGithubDisabled",
};

/**
 * Build the URL that starts login, carrying "where to go after login".
 *
 * next is the current pathname + search, **without hash** (the hash is never sent to the server,
 * so it would be useless).
 * The backend checks again that it is a same-site relative path (against open redirects), so we
 * need not check it here;
 * but **do not** build an absolute URL here either, since the backend would reject it and fall
 * back to the home page.
 */
function oidcLoginHref(): string {
  const params = new URLSearchParams(window.location.search);
  // Coming back with login_error should not be treated as "come back here after login": otherwise
  // after a successful login
  // the address bar still carries the previous failure reason, and the hint bar stays forever.
  params.delete("login_error");
  const search = params.toString();
  const next = window.location.pathname + (search ? "?" + search : "");
  // Already on the home page, so no need for next; saves a useless parameter.
  if (next === "/" || next === "") return "/api/auth/oidc/login";
  return "/api/auth/oidc/login?next=" + encodeURIComponent(next);
}

/**
 * The four feature cards.
 *
 * **Store translation keys, not translations**: when module-level constants are evaluated
 * PrefsContext does not exist yet, so calling t() here gets nothing
 * (see docs/guides/i18n.md). Translate at render time.
 */
const FEATURES: { icon: string; titleKey: MessageKey; descKey: MessageKey }[] = [
  {
    icon: "gravity-ui:rocket",
    titleKey: "landing.hostTitle",
    descKey: "landing.hostDesc",
  },
  {
    icon: "gravity-ui:persons",
    titleKey: "landing.collabTitle",
    descKey: "landing.collabDesc",
  },
  {
    icon: "gravity-ui:shield-check",
    titleKey: "landing.sandboxTitle",
    descKey: "landing.sandboxDesc",
  },
  {
    icon: "gravity-ui:plug-connection",
    titleKey: "landing.feishuTitle",
    descKey: "landing.feishuDesc",
  },
];

export function LandingPage({ onLoggedIn }: { onLoggedIn: (user: AuthUser) => void }) {
  const t = useT();
  // Read the callback failure reason from the URL. **Use window.location, not useSearchParams**:
  // this component is **outside** RouterProvider (AuthGate wraps the whole route table), so it has
  // no router context.
  const reason = new URLSearchParams(window.location.search).get("login_error");
  const errorKey = reason ? (LOGIN_ERROR_KEYS[reason] ?? "login.errUnavailable") : null;

  // The outer full-page shell and background belong here; LoginPage only draws one card. Writing
  // min-h-dvh in both places would stack two layers.
  // Both phases share the same background color, so switching does not change color.
  //
  // ⚠️ This page is **not inside AppShell** (AuthGate renders it directly when logged out), so its
  // four edges are the screen's
  // physical edges, and it must handle the safe area itself. `py-16` (64px) looks like enough to
  // clear the 59px Dynamic Island, but only
  // when content is short enough to center: on narrow screens the feature cards are a single
  // column of four, and once taller than the screen it scrolls from the top,
  // and the first card runs right under the island. Landscape is worse: `px-6`'s 24px cannot clear
  // the 59px side notch.
  return (
    <div
      className="flex min-h-dvh flex-col items-center justify-center bg-background px-6 py-16"
      style={{
        paddingTop: "calc(4rem + var(--safe-top))",
        paddingBottom: "calc(4rem + var(--safe-bottom))",
        paddingLeft: "calc(1.5rem + var(--safe-left))",
        paddingRight: "calc(1.5rem + var(--safe-right))",
      }}
    >
      <div className="flex w-full max-w-5xl flex-col items-center gap-12" data-testid="landing">
          <header className="flex flex-col items-center gap-6 text-center">
            {/* Brand mark: logo + wordmark. On the whole page only this and the login card have weight. */}
            <div className="flex items-center gap-2.5">
              <BrandMark />
              <span className="text-2xl font-semibold tracking-tight">AMA</span>
            </div>

            <div className="flex flex-col items-center gap-3">
              <h1 className="text-3xl font-semibold tracking-tight text-balance sm:text-4xl">
                {t("landing.slogan")}
              </h1>
              {/*
                The intro sentence has an external link in the middle, so the text is joined from
                three parts (see those keys in messages.ts).
                In Chinese the link falls mid-sentence, in English at the end; three parts let each
                language decide the position.
              */}
              <p className="max-w-xl text-base text-muted text-balance">
                {t("landing.introBefore")}
                <a
                  href={ARK_MANAGED_AGENTS_URL}
                  target="_blank"
                  rel="noreferrer"
                  className="text-accent underline underline-offset-2"
                  data-testid="landing-ark-link"
                >
                  {t("landing.introLink")}
                </a>
                {t("landing.introAfter")}
              </p>
            </div>

            {/*
              Hint for an OIDC login failure. **Placed above the login card**: when users come back
              to this page the first thing they must see
              is "why the login did not work", rather than the same button as last time, thinking
              they missed the click.
              A danger-soft Card rather than a full-screen error page: the product intro should
              still be visible.
            */}
            {errorKey && (
              <div
                className="flex max-w-xl items-start gap-2 rounded-2xl bg-surface-secondary px-4 py-3 text-left"
                role="alert"
                data-testid="login-error"
              >
                <Icon
                  icon="gravity-ui:circle-exclamation"
                  className="mt-0.5 size-4 shrink-0 text-danger"
                />
                <p className="text-sm text-foreground">{t(errorKey)}</p>
              </div>
            )}

            <LoginCard onLoggedIn={onLoggedIn} />
          </header>

          {/* Narrow screens one column, sm two columns, lg four in a row. */}
          <div className="grid w-full gap-4 sm:grid-cols-2 lg:grid-cols-4">
            {FEATURES.map((f) => (
              <Card key={f.titleKey} className="h-full">
                <Card.Content className="flex flex-col gap-2 pt-6">
                  {/* Feature list icons use muted; only body text uses the standard foreground. Icons assist and should not grab attention. */}
                  <Icon icon={f.icon} className="size-5 text-muted" />
                  <p className="font-medium">{t(f.titleKey)}</p>
                  <p className="text-sm text-muted">{t(f.descKey)}</p>
                </Card.Content>
              </Card>
            ))}
          </div>
      </div>
    </div>
  );
}

/** Three states of fetching login methods. */
type ProvidersState =
  | { phase: "loading" }
  | { phase: "ready"; providers: AuthProviders }
  | { phase: "failed" };

/**
 * Login card: email/password form (can switch to sign-up) + OIDC button; which parts show depends
 * on providers.
 */
function LoginCard({ onLoggedIn }: { onLoggedIn: (user: AuthUser) => void }) {
  const t = useT();
  const [state, setState] = useState<ProvidersState>({ phase: "loading" });
  const [githubFlow, setGitHubFlow] = useState<GitHubDeviceStart | null>(null);
  const [githubStarting, setGitHubStarting] = useState(false);
  const [githubStartError, setGitHubStartError] = useState<MessageKey | null>(null);

  const startGitHub = useCallback(async () => {
    setGitHubStarting(true);
    setGitHubStartError(null);
    try {
      setGitHubFlow(await api.startGitHubLogin());
    } catch (err) {
      const code = err instanceof ApiError ? err.code : "";
      setGitHubFlow(null);
      setGitHubStartError(GITHUB_ERROR_KEYS[code] ?? "login.errUnavailable");
    } finally {
      setGitHubStarting(false);
    }
  }, []);

  const load = useCallback(async () => {
    setState({ phase: "loading" });
    try {
      setState({ phase: "ready", providers: await api.getAuthProviders() });
    } catch {
      setState({ phase: "failed" });
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  if (state.phase === "loading") {
    return (
      <div className="flex h-32 items-center justify-center">
        <Spinner size="sm" aria-label={t("common.loading")} />
      </div>
    );
  }

  if (state.phase === "failed") {
    return (
      <Card className="w-full max-w-sm text-left" data-testid="login-card">
        <Card.Content className="flex flex-col items-start gap-3">
          <p className="text-sm text-foreground" role="alert">
            {t("login.providersFailed")}
          </p>
          <Button variant="secondary" onPress={() => void load()} data-testid="login-retry">
            {t("login.retry")}
          </Button>
        </Card.Content>
      </Card>
    );
  }

  const { local, oidc } = state.providers;
  // Older backends without a github field are treated as disabled, so the whole card does not
  // crash on undefined.
  const github = state.providers.github ?? { enabled: false, displayName: "" };

  if (!local.enabled && !oidc.enabled && !github.enabled) {
    return (
      <Card className="w-full max-w-sm text-left" data-testid="login-card">
        <Card.Content>
          <p className="text-sm text-foreground" role="alert" data-testid="login-no-method">
            {t("login.noMethod")}
          </p>
        </Card.Content>
      </Card>
    );
  }

  // GitHub authorization in progress: the whole card becomes the code panel; cancel returns to
  // choosing a login method.
  if (githubFlow) {
    return (
      <Card className="w-full max-w-sm text-left" data-testid="login-card">
        <Card.Content>
          <GitHubDevicePanel
            key={githubFlow.flowId}
            flow={githubFlow}
            onLoggedIn={onLoggedIn}
            onRestart={() => void startGitHub()}
            onCancel={() => setGitHubFlow(null)}
          />
        </Card.Content>
      </Card>
    );
  }

  const others = local.enabled || oidc.enabled;

  return (
    <Card className="w-full max-w-sm text-left" data-testid="login-card">
      <Card.Content className="flex flex-col gap-4">
        {local.enabled && (
          <PasswordForm
            signupOpen={local.signupOpen}
            firstUser={local.signupOpen && !local.allowSignup}
            onLoggedIn={onLoggedIn}
          />
        )}
        {local.enabled && oidc.enabled && (
          <div className="flex items-center gap-3" aria-hidden="true">
            <Separator className="flex-1" />
            <span className="text-xs text-muted">{t("login.or")}</span>
            <Separator className="flex-1" />
          </div>
        )}
        {oidc.enabled && (
          // Starting OIDC login is a **full-page navigation** (to the backend's 303, then on to the
          // IdP),
          // so use location.assign rather than router navigation: the target is not in the SPA.
          // ⚠️ Button **has no href prop** (checked the component docs).
          <Button
            fullWidth
            variant={local.enabled ? "secondary" : "primary"}
            onPress={() => window.location.assign(oidcLoginHref())}
            data-testid="login-oidc"
          >
            {t("login.oidc", { name: oidc.displayName || "SSO" })}
          </Button>
        )}
        {local.enabled && !oidc.enabled && github.enabled && (
          <div className="flex items-center gap-3" aria-hidden="true">
            <Separator className="flex-1" />
            <span className="text-xs text-muted">{t("login.or")}</span>
            <Separator className="flex-1" />
          </div>
        )}
        {github.enabled && (
          <div className="flex flex-col gap-2">
            <Button
              fullWidth
              variant={others ? "secondary" : "primary"}
              isPending={githubStarting}
              onPress={() => void startGitHub()}
              data-testid="login-github"
            >
              <Icon icon="gravity-ui:logo-github" className="size-4" />
              {t("login.github", { name: github.displayName || "GitHub" })}
            </Button>
            {githubStartError ? (
              <p className="text-sm text-danger" role="alert" data-testid="login-github-error">
                {t(githubStartError)}
              </p>
            ) : null}
          </div>
        )}
      </Card.Content>
    </Card>
  );
}

/**
 * Email + password form. `mode` switches between login / sign-up; sign-up is only reachable when
 * signupOpen.
 *
 * Uses <Form onSubmit> rather than a button onPress: Enter-to-submit and browser password managers
 * recognizing the form both rely on it.
 */
function PasswordForm({
  signupOpen,
  firstUser,
  onLoggedIn,
}: {
  signupOpen: boolean;
  /** The deployment has no accounts yet (sign-up is not enabled in config, but the first account can sign up). */
  firstUser: boolean;
  onLoggedIn: (user: AuthUser) => void;
}) {
  const t = useT();
  // With no accounts at all, login is sure to fail, so start on sign-up.
  const [mode, setMode] = useState<"login" | "signup">(firstUser ? "signup" : "login");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [name, setName] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [errorKey, setErrorKey] = useState<MessageKey | null>(null);

  const isSignup = mode === "signup" && signupOpen;

  const onSubmit = async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    if (submitting) return;
    setSubmitting(true);
    setErrorKey(null);
    try {
      const trimmedEmail = email.trim();
      const user = isSignup
        ? await api.signup({
            email: trimmedEmail,
            password,
            ...(name.trim() ? { name: name.trim() } : {}),
          })
        : await api.loginWithPassword(trimmedEmail, password);
      onLoggedIn(user);
    } catch (err) {
      const code = err instanceof ApiError ? err.code : "";
      setErrorKey(SUBMIT_ERROR_KEYS[code] ?? "login.errGeneric");
      setSubmitting(false);
    }
  };

  return (
    <Form className="flex flex-col gap-4" onSubmit={(e) => void onSubmit(e)}>
      <h2 className="text-lg font-semibold">{isSignup ? t("login.signupTitle") : t("login.title")}</h2>

      {isSignup && firstUser && (
        <p className="text-sm text-muted" data-testid="login-first-user-hint">
          {t("login.firstUserHint")}
        </p>
      )}

      <TextField isRequired name="email">
        <Label>{t("login.email")}</Label>
        <Input
          type="email"
          autoComplete="email"
          value={email}
          placeholder="you@example.com"
          data-testid="login-email"
          onChange={(e) => {
            setEmail(e.target.value);
            setErrorKey(null);
          }}
        />
      </TextField>

      {isSignup && (
        <TextField name="name">
          <Label>{t("login.name")}</Label>
          <Input
            autoComplete="name"
            value={name}
            data-testid="login-name"
            onChange={(e) => setName(e.target.value)}
          />
        </TextField>
      )}

      <TextField isRequired name="password">
        <Label>{t("login.password")}</Label>
        <Input
          type="password"
          autoComplete={isSignup ? "new-password" : "current-password"}
          value={password}
          placeholder={isSignup ? t("login.passwordHint") : undefined}
          data-testid="login-password"
          onChange={(e) => {
            setPassword(e.target.value);
            setErrorKey(null);
          }}
        />
      </TextField>

      {/*
        Server failures are not attached to a field's isInvalid: Form uses native validation by
        default,
        and isInvalid would be written as customValidity, **which also blocks the next submit**
        (after "too many attempts",
        waiting and clicking again as is would not work). So it gets its own alert line.
      */}
      {errorKey ? (
        <p className="text-sm text-danger" role="alert" data-testid="login-submit-error">
          {t(errorKey)}
        </p>
      ) : null}

      <Button type="submit" fullWidth isPending={submitting} data-testid="login-submit">
        {isSignup ? t("login.signupSubmit") : t("login.submit")}
      </Button>

      {signupOpen && (
        <Button
          variant="ghost"
          size="sm"
          className="self-center"
          onPress={() => {
            setMode(isSignup ? "login" : "signup");
            setErrorKey(null);
          }}
          data-testid="login-toggle-signup"
        >
          {isSignup ? t("login.toLogin") : t("login.toSignup")}
        </Button>
      )}
    </Form>
  );
}

/** Format remaining seconds as m:ss. */
function formatRemaining(sec: number): string {
  const s = Math.max(0, Math.floor(sec));
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
}

/**
 * The code panel for GitHub device flow: big short code (copyable) + open GitHub's code page +
 * automatic polling.
 *
 * Polling pace follows the backend completely: each response carries interval (it grows after
 * slow_down), and the next wait is that long.
 * Wait an extra half second: the backend lets through at "last poll + interval", and arriving
 * exactly on time tends to land just before the boundary, wasting a trip.
 *
 * Terminal states (expired / denied / no email / not allowed) stop and offer "get a new code";
 * GitHub being temporarily unreachable
 * (login_unavailable) **is not terminal**: show a hint and keep retrying at the interval; the user
 * may be clicking authorize on GitHub right now.
 */
function GitHubDevicePanel({
  flow,
  onLoggedIn,
  onRestart,
  onCancel,
}: {
  flow: GitHubDeviceStart;
  onLoggedIn: (user: AuthUser) => void;
  onRestart: () => void;
  onCancel: () => void;
}) {
  const t = useT();
  const expiresAtMs = Date.parse(flow.expiresAt);
  const [remaining, setRemaining] = useState(() => (expiresAtMs - Date.now()) / 1000);
  const [errorKey, setErrorKey] = useState<MessageKey | null>(null);
  const [retrying, setRetrying] = useState(false);
  const stopped = errorKey !== null || remaining <= 0;
  // onLoggedIn may be a new function on every render; put it in a ref so the polling effect does
  // not restart because of it.
  const loggedIn = useRef(onLoggedIn);
  loggedIn.current = onLoggedIn;

  // Countdown. At 0 it counts as expired, and the polling effect also stops because of stopped.
  useEffect(() => {
    const id = setInterval(() => setRemaining((expiresAtMs - Date.now()) / 1000), 1000);
    return () => clearInterval(id);
  }, [expiresAtMs]);

  useEffect(() => {
    if (stopped) return;
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout>;
    const schedule = (sec: number) => {
      timer = setTimeout(() => void tick(), Math.max(1, sec) * 1000 + 500);
    };
    const tick = async () => {
      try {
        const res = await api.pollGitHubLogin(flow.flowId);
        if (cancelled) return;
        setRetrying(false);
        if (res.status === "ok") {
          loggedIn.current(res.user);
          return;
        }
        schedule(res.interval);
      } catch (err) {
        if (cancelled) return;
        const code = err instanceof ApiError ? err.code : "";
        const key = GITHUB_ERROR_KEYS[code];
        if (key) {
          setErrorKey(key);
          return;
        }
        setRetrying(true);
        schedule(flow.interval);
      }
    };
    schedule(flow.interval);
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [flow.flowId, flow.interval, stopped]);

  const shownError = errorKey ?? (remaining <= 0 ? "login.errGithubExpired" : null);

  return (
    <div className="flex flex-col gap-4" data-testid="github-device">
      <div className="flex items-center gap-2">
        <Icon icon="gravity-ui:logo-github" className="size-5" />
        <h2 className="text-lg font-semibold">{t("login.githubTitle")}</h2>
      </div>
      <p className="text-sm text-muted">{t("login.githubHint")}</p>

      <div className="flex items-center justify-center gap-2 rounded-2xl bg-surface-secondary px-4 py-5">
        <span
          className="select-all font-mono text-3xl font-semibold tracking-[0.15em]"
          aria-label={t("login.githubCode")}
          data-testid="github-user-code"
        >
          {flow.userCode}
        </span>
        <CopyButton text={flow.userCode} label={t("login.githubCopy")} />
      </div>

      {shownError ? (
        <>
          <p className="text-sm text-danger" role="alert" data-testid="github-error">
            {t(shownError)}
          </p>
          <Button fullWidth onPress={onRestart} data-testid="github-restart">
            {t("login.githubRestart")}
          </Button>
        </>
      ) : (
        <>
          {/*
            Opens in a new tab (not a full-page jump): this page must stay to keep polling.
            ⚠️ Button has no href; use window.open + noopener.
          */}
          <Button
            fullWidth
            onPress={() => window.open(flow.verificationUri, "_blank", "noopener,noreferrer")}
            data-testid="github-open"
          >
            <Icon icon="gravity-ui:arrow-up-right-from-square" className="size-4" />
            {t("login.githubOpen")}
          </Button>
          <div className="flex items-center gap-2 text-sm text-muted" role="status" aria-live="polite">
            <Spinner size="sm" aria-hidden="true" />
            <span>
              {retrying ? t("login.errGithubRetrying") : t("login.githubWaiting")}
              {" · "}
              {t("login.githubExpiresIn", { time: formatRemaining(remaining) })}
            </span>
          </div>
        </>
      )}

      <Button variant="ghost" size="sm" className="self-center" onPress={onCancel} data-testid="github-cancel">
        {t("login.githubCancel")}
      </Button>
    </div>
  );
}

/**
 * "Choose an existing Ark API Key from the Volcengine account": **only the picker part**, without the dialog shell.
 *
 * # Why this granularity
 *
 * The dialogs for the three credential levels (personal / workspace / Agent) **already exist separately**, carrying testids e2e depends on
 * (`ark-modal` / `global-ark-modal` / `ark-key-input` …). Replacing the whole dialog with a new component
 * would change those testids too and turn a batch of existing cases red, while what they test is the "type it in" path, which has not changed.
 *
 * So this only provides **the part that can be embedded in any dialog**: pick account and key on top, and below it is still each page's original
 * manual input box. The three places share one implementation instead of each writing its own.
 *
 * # ⚠️ The plaintext does not pass through the frontend
 *
 * After choosing, all that is handed over is `{accountId, keyId}`; the server fetches the plaintext itself, computes the mask, and stores it encrypted.
 * Letting the frontend get the plaintext and PUT it back would be simpler (just reuse the apiKey field), but then a long-lived
 * Ark key would show up in the devtools network panel, could be read by browser extensions, and would stay in frontend memory.
 *
 * # Auto-fetch is a nice extra; manual input always stays
 *
 * That path (managed credential → an authorization service → Volcengine console → list keys) depends on a reverse-engineered
 * frontend signature and undocumented internal console APIs of Volcengine, and may break when their frontend changes. So when it is unavailable
 * **only this part is degraded**, and manual input works as usual: users must never be stuck on this path.
 *
 * ---
 *
 * # Information architecture: account and key are two levels, and the UI must express that order
 *
 * The first version laid the two out **flat** as button lists of equal weight: 10 account tiles on top, 10 key tiles below.
 * Users saw 20 identical-looking entries, so three things broke at once:
 *
 * 1. **You cannot tell which are accounts and which are keys** (same visual weight; you can only guess by content);
 * 2. **You cannot see "keys only come after choosing an account"** (the key list is empty before an account is chosen, which looks like "no keys");
 * 3. **The 20 keys have very similar names** (`api-key-20260827175443` / `api-key-20260827173154`;
 *    tested: under one account 6 of 10 were named with timestamps from the same day), **and the masks are all identical**
 *    (what Volcengine gives is just a string of asterisks), so **only the creation time can tell them apart**, and it was not shown at all back then.
 *
 * The current structure:
 *
 *   Step 1  Volcengine account  ComboBox (searchable dropdown)      ← it is the **precondition**
 *   Step 2  API Key             search box + scrollable list         ← **the whole part does not appear** before an account is chosen
 *
 * Each of the three choices has a reason:
 *
 * - **Accounts use a dropdown instead of a flat layout**: it is a precondition, and once chosen it should fold up to make room for step two;
 *   and 10 accounts laid out flat would take three or four rows, pushing the keys you actually want to pick off screen.
 * - **The key part is not rendered before an account is chosen** (not rendered as an empty list): that is the most direct expression of "order",
 *   and it avoids showing "you have not chosen an account yet" as "this account has no keys".
 * - **Three pieces of info per key row**: name + mask + **creation time**. The time uses `tabular-nums`,
 *   right-aligned on the same row as the name, because it is the only way to tell them apart; hidden on a second line it would be taken as decoration.
 *
 * # Five states, each shown on its own; these two pairs especially must not be mixed
 *
 * | State | When | What it gives |
 * | --- | --- | --- |
 * | Loading | status/accounts still in flight | **skeleton** (not an empty list, see below) |
 * | Not integrated | the server has no app_key configured | an entry to the console, **no "authorize" button** |
 * | Not authorized | configured, but this person has not authorized | an authorization entry |
 * | Empty list | this account has no keys | says where to create one |
 * | Failed | listing accounts/keys errored | shows the error as is + retry |
 *
 * ⚠️ **"Not integrated" gets no authorize button**: then clicking would only get the user a 422, and they would have no way to know they need to ask
 * an admin to configure the server. Same as the `canAuthorize` comment in `IntegrationsTab`: this trap was hit once on the authorization page;
 * the check is "is the server integrated", not "has the user authorized".
 *
 * ⚠️ **"Loading" does not share a display with "empty list"**: that is the same mistake as the batch of blank screens in `useAsyncData`
 * (treating `data === null` as empty, so both failure and loading show "no data"). The mock delay
 * is only 90ms, so in e2e this branch can only be tested with the `hangReads` hook.
 *
 * ---
 *
 * # The source is a dropdown, and the manual input box **does not appear by default**
 *
 * The two paths used to sit side by side: the picker on top, and the manual input box always hanging below. That made "which one should I use"
 * something users had to think about, while 99% of the time the answer is "choose from Volcengine": manual input is just the fallback after that path
 * breaks. So it became an explicit source dropdown, defaulting to "choose from the Volcengine account",
 * and **the input box is only rendered when "type it in" is selected**.
 *
 * ⚠️⚠️ **But the bottom line "manual input always stays" must not break**, and hiding the input box by default is exactly what most easily breaks it:
 * when auto-fetch is unavailable (the server has no app_key configured / upstream is down), if the UI still dutifully renders by
 * "the user chose auto", the user sees a picker that cannot be used **with nothing below it**,
 * and is completely stuck: they do not even know manual input is possible.
 *
 * So whether the manual input box shows is **not decided by that dropdown alone**, but by a derived value:
 *
 *   effectiveSource = is auto-fetch usable ? whatever the user chose : "manual"
 *
 * "Force fallback to manual when unavailable" is only one line of code, but it is the only **really dangerous** spot in this change:
 * written as `source === "manual"` it shuts users out with zero errors. 22-ark-fetch-picker
 * has a case dedicated to pinning it (the input box must be there when not integrated), mutation-checked.
 *
 * ⚠️ And the **opposite of this line was also wrong once**: for a while "listing accounts failed" also counted as unavailable, so then it was forced to
 * fall back to manual, the whole fetch branch folded up, and **the error and the "retry" button disappeared with it**. The check is
 * **can it be retried**: with no app_key configured, retrying ten thousand times is the same; a 503 from listing accounts may well be fine on retry.
 * The latter must stay in place showing the error and retry, while keeping the source dropdown usable (to type it in, one click switches).
 *
 * # The manual input box is given by the caller (children), not drawn by this component
 *
 * The input boxes of the three call sites all differ: different testids (`ark-key-input` /
 * `global-ark-key-input`), with or without `type="password"`, different ways of attaching validation hints,
 * and existing e2e depends on all of them. So this component only decides **whether to show it**; what it looks like still belongs to the caller:
 * it passes the input box in as `manualSlot`, and this component renders it in the right place at the right time.
 *
 * This way "no input box by default" is implemented only once, and the three places agree automatically, instead of each call site
 * writing `source === "manual" && ...` on its own (that kind of duplication drifts sooner or later, and drift reports no error).
 */

import { useCallback, useEffect, useState, type ReactNode } from "react";
import {
  Button,
  Chip,
  ComboBox,
  Description,
  Input,
  Label,
  ListBox,
  Select,
  Skeleton,
} from "@heroui/react";
import { Icon } from "@iconify/react";
import { api } from "@/data/api";
import { useAuth } from "@/app/AuthGate";
import { Notice } from "@/components/Page";
import { formatDateTime, formatRelative } from "@/lib/format";
import { usePrefs, useT, type TFunc } from "@/prefs/PrefsContext";
import {
  ARK_API_KEY_CONSOLE_URL,
  isMyArkKey,
  validateArkApiKey,
  type ArkFetchStatus,
  type ArkKeyOption,
  type ArkKeyRef,
  type VolcAccount,
} from "@/types/domain";

/**
 * Merges "source + the chosen key + the typed string" into one submission.
 *
 * Shared by the three call sites, because **they all hit the same easy-to-get-wrong edge**: when the source is "choose from Volcengine"
 * but the user has not picked a key yet, validating `apiKey` directly reports "API Key must not be empty",
 * while that input box **is not on screen at all** right now (not rendered by default). The user looks at an empty
 * form, is told some field is empty, and has no idea what to do.
 *
 * The check order is deliberate too: **pickedRef first, then apiKey, and only then source**:
 * when auto-fetch is unavailable the UI has already forced a fallback to manual, while the `source` the caller holds is still what the user originally chose,
 * "fetch"; if the user typed into the manual box then, judging by source would treat their input as nothing.
 * "Use whatever is there" is more reliable than "what they said they would use".
 *
 * Returning an Error means this submission is invalid, with the error text ready.
 */
export function resolveArkSubmission(
  source: ArkKeySource,
  pickedRef: ArkKeyRef | null,
  apiKey: string,
  t: TFunc,
): { fetchRef: ArkKeyRef } | { apiKey: string } | Error {
  if (pickedRef) return { fetchRef: pickedRef };
  const manual = apiKey.trim();
  if (manual) {
    const err = validateArkApiKey(manual, t);
    return err ? new Error(err) : { apiKey: manual };
  }
  // Both are empty. What to report depends on which control the user sees.
  if (source === "fetch") return new Error(t("arkFetch.pickRequired"));
  return new Error(validateArkApiKey("", t) ?? t("arkFetch.pickRequired"));
}

/** The two sources. fetch is the default: manual is the fallback after that path breaks, not a regular option. */
export type ArkKeySource = "fetch" | "manual";

export function ArkFetchPicker({
  /** Incremented every time the dialog opens, to trigger a refetch (a half-finished selection from last time should not stay). */
  resetKey,
  picked,
  onPick,
  /**
   * The manual input box. **Given by the caller**, because the testids and forms differ in the three places,
   * and existing e2e depends on all of them (see the file header). This component only decides when it appears.
   */
  manualSlot,
  /**
   * What the user chose in the source dropdown, and the callback to the caller when it changes.
   *
   * ⚠️ The caller needs to know the current source to decide what to submit (the manual path validates apiKey,
   * the auto path only hands over two ids), so this state lives in the caller, not inside this component.
   */
  source,
  onSourceChange,
  /**
   * Whether this choice is for the **account-level (global)** credential.
   *
   * ⚠️ When true, say right here "all workspaces that reference it will switch to the new key immediately". The user is choosing inside the dialog,
   * and may have scrolled past the line in the dialog title, while pressing save really affects a batch of workspaces:
   * a much bigger impact than the workspace-level key, so it must be said once more on the last screen before pressing.
   * Kept the same as the line on the GlobalArkTab card (one concept, one wording).
   */
  scope = "workspace",
}: {
  resetKey: unknown;
  /** The currently chosen key; null means "use the typed one". */
  picked: ArkKeyRef | null;
  onPick: (ref: ArkKeyRef | null) => void;
  manualSlot: ReactNode;
  source: ArkKeySource;
  onSourceChange: (source: ArkKeySource) => void;
  scope?: "global" | "workspace";
}) {
  const t = useT();
  const { language } = usePrefs();
  // Deciding "which ones did I create" needs the current user's email; see isMyArkKey.
  const { user } = useAuth();

  const [status, setStatus] = useState<ArkFetchStatus | null>(null);
  const [accounts, setAccounts] = useState<VolcAccount[] | null>(null);
  const [accountId, setAccountId] = useState("");
  const [keys, setKeys] = useState<ArkKeyOption[] | null>(null);
  const [fetchError, setFetchError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  /**
   * The "list accounts" step failed.
   *
   * **Kept apart from fetchError (listing keys failed)**: the two need different retry actions: the former retries the whole part,
   * the latter only retries the current account. Merged into one, the "retry" button would not know what to retry.
   */
  const [accountError, setAccountError] = useState<string | null>(null);

  /** Fetch "status + account list". The authorization page link is valid for 600 seconds, **fetched fresh every time, not cached**. */
  const loadStatusAndAccounts = useCallback(() => {
    setAccountId("");
    setKeys(null);
    setFetchError(null);
    setAccountError(null);
    setStatus(null);
    setAccounts(null);

    api
      .getArkFetchStatus()
      .then((st) => {
        setStatus(st);
        if (st.available && st.authorized) {
          // ⚠️ This catch must stay, otherwise when listing accounts fails accounts stays null forever,
          // and the UI **stays on the skeleton forever**: exactly the cause of the batch of blank screens in useAsyncData.
          return api
            .listVolcAccounts()
            .then(setAccounts)
            .catch((e: unknown) => {
              setAccountError(e instanceof Error ? e.message : t("arkFetch.loadFailed"));
              // An empty array instead of leaving null: null means "still loading", and here there is already a conclusion (failed).
              setAccounts([]);
            });
        }
      })
      .catch(() => {
        // Auto-fetch being unavailable **is not an error**: take the "not integrated" branch, and manual input works as usual.
        setStatus({ available: false, authorized: false });
      });
  }, [t]);

  useEffect(() => {
    loadStatusAndAccounts();
    // ⚠️ **Only resetKey goes into the dependencies.**
    //
    // loadStatusAndAccounts depends on `t` (for the fallback error text), and `t` is memoized on
    // `prefs.language`: putting it in the dependencies would refetch the account list whenever the language changes, while the list content
    // has nothing to do with the language. The meaning of this effect is "refetch every time the dialog opens",
    // which is expressed by the resetKey the caller increments; nothing else should trigger it.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [resetKey]);

  const loadKeys = useCallback(
    async (acct: string) => {
      setAccountId(acct);
      onPick(null);
      setKeys(null);
      setFetchError(null);
      if (!acct) return;
      setLoading(true);
      try {
        setKeys(await api.listVolcArkKeys(acct));
      } catch (e) {
        // The error here must be **shown as is**: the server says clearly "which account lacks which permission",
        // which is exactly the information the user needs to request access. Showing "this account has no keys" would make them
        // create a new key, which would be rejected just the same.
        setFetchError(e instanceof Error ? e.message : t("arkFetch.loadFailed"));
      } finally {
        setLoading(false);
      }
    },
    [onPick, t],
  );

  /**
   * ⚠️⚠️ **The source actually in effect. This line is the most dangerous spot in the whole component.**
   *
   * When auto-fetch **cannot be used at all** (status not back yet / not configured on the server / this person has not authorized), force a fallback to manual,
   * no matter what the user chose in the dropdown. Written as `source` it shuts users out with zero errors:
   * they see a picker that cannot be used, with nothing below it, while "manual input always stays" is the bottom line of this path.
   *
   * ⚠️ **But `accountError` is deliberately not part of the check**, even though "listing accounts failed" also looks unavailable.
   *
   * The difference between the two is **whether it can be retried**: no app_key configured is a config problem, the same after ten thousand retries;
   * listing accounts failing is an upstream hiccup (tested: 503), and a retry may well fix it. Counting it would force a fallback to manual,
   * so the whole fetch branch folds up, and **the error and the "retry" button disappear with it**: the user can neither see
   * what happened nor retry, left with just an empty manual box. That is not "keeping the fallback"; that is hiding information.
   *
   * Then the user is not stuck: the error and the retry button are there, and the source dropdown **stays usable**;
   * to type it in, one click switches. Both paths being open is what is right.
   */
  const fetchUsable = status !== null && status.available && status.authorized;
  const effectiveSource: ArkKeySource = fetchUsable ? source : "manual";

  /** Source dropdown + manual input box. **All five states need it**, so it is pulled out instead of written in each branch. */
  const sourceControls = (
    <>
      <SourceSelect
        // When unavailable, lock this dropdown and show it as "type it in": leaving it on an unclickable
        // "choose from Volcengine" would tell the user "you chose auto" when that is not true, which is a lie.
        effectiveSource={effectiveSource}
        isDisabled={!fetchUsable}
        onChange={(next) => {
          onSourceChange(next);
          // Switching to manual clears the chosen key: the server returns an error when given both.
          if (next === "manual") onPick(null);
        }}
        t={t}
      />
      {/* **No input box by default**: only rendered when actually taking the manual path (see the file header). */}
      {effectiveSource === "manual" ? manualSlot : null}
    </>
  );

  // ---- State 1: loading. **Skeleton, not an empty list** (see the file header). ----
  if (status === null) {
    return (
      <div className="flex flex-col gap-4" data-testid="ark-fetch-loading">
        <PickerHeading t={t} />
        <Skeleton className="h-10 w-full r-inset" />
        <Skeleton className="h-4 w-2/3 r-inset" />
        {/*
          ⚠️ The manual input box is given while loading too. Before the status comes back fetchUsable is false,
          so this falls on manual: **users need not wait for a response that may never come**.
        */}
        {sourceControls}
      </div>
    );
  }

  // ---- State 2: not integrated (server not configured). **No "authorize" button** ----
  //
  // The check is available, not authorized: keeping those two apart is exactly why this branch exists.
  // With an authorize button, clicking it gives the user only a 422, and they would have no way to know they should go to an admin.
  if (!status.available) {
    return (
      <div className="flex flex-col gap-4" data-testid="ark-fetch-unavailable">
        <PickerHeading t={t} />
        <Notice
          tone="info"
          icon="gravity-ui:circle-info"
          // If the server gave a reason, use that sentence (more specific, like "no app_key configured").
          title={status.reason || t("arkFetch.unavailableBody")}
          hint={t("arkFetch.unavailableHint")}
        />
        <ConsoleLink t={t} />
        {/* ⚠️ The manual input box here is the user's **only** way out right now; without it they are completely stuck. */}
        {sourceControls}
      </div>
    );
  }

  // ---- State 3: usable but not authorized → give the authorization entry, **not an error** ----
  if (!status.authorized) {
    return (
      <div className="flex flex-col gap-4" data-testid="ark-fetch-authorize">
        <PickerHeading t={t} />
        <div className="flex flex-col gap-2.5 r-inset bg-surface-secondary px-3 py-3">
          <div className="flex flex-col gap-1">
            <p className="text-sm">{t("arkFetch.authorizeTitle")}</p>
            <p className="text-xs text-muted">{t("arkFetch.authorizeBody")}</p>
          </div>
          {status.authorizeUrl ? (
            <Button
              size="sm"
              variant="outline"
              className="self-start"
              onPress={() => window.open(status.authorizeUrl, "_blank", "noopener,noreferrer")}
              data-testid="ark-fetch-authorize-action"
            >
              {t("arkFetch.authorizeAction")}
            </Button>
          ) : null}
        </div>
        {/* When not authorized, manual input is the only path usable right away (authorizing means a trip out). */}
        {sourceControls}
      </div>
    );
  }

  const accountName = accounts?.find((a) => a.id === accountId)?.name ?? "";

  return (
    <div className="flex flex-col gap-4" data-testid="ark-fetch-picker">
      {/* When taking the manual path this heading should not appear; see the comment on PickerHeading. */}
      {effectiveSource === "fetch" ? <PickerHeading t={t} /> : null}

      {/*
        The source dropdown comes first: it decides what shows below, so it must appear first.
        When "type it in" is chosen, the two-level picker folds up entirely: the user has already said they will not take this path,
        and keeping it is just noise (HeroUI's design taste: remove what can be removed).
      */}
      <SourceSelect
        effectiveSource={effectiveSource}
        isDisabled={!fetchUsable}
        onChange={(next) => {
          onSourceChange(next);
          if (next === "manual") onPick(null);
        }}
        t={t}
      />

      {effectiveSource === "manual" ? (
        manualSlot
      ) : (
        <>
          {/* ---- Step 1: choose the account. It is the precondition, so it has a step number. ---- */}
          <div className="flex flex-col gap-2" data-testid="ark-fetch-step-account">
            {accounts === null ? (
              // Accounts still in flight: skeleton, not "no accounts".
              <div className="flex flex-col gap-2" data-testid="ark-account-loading">
                <StepLabel step={t("arkFetch.step1")} label={t("arkFetch.accountLabel")} />
                <Skeleton className="h-10 w-full r-inset" />
              </div>
            ) : accountError ? (
              // ---- State 5a: listing accounts failed → retryable, error shown as is. ----
              <div className="flex flex-col gap-2">
                <StepLabel step={t("arkFetch.step1")} label={t("arkFetch.accountLabel")} />
                <Notice
                  tone="danger"
                  icon="gravity-ui:triangle-exclamation"
                  title={accountError}
                  data-testid="ark-account-error"
                />
                <Button
                  size="sm"
                  variant="outline"
                  className="self-start"
                  onPress={loadStatusAndAccounts}
                  data-testid="ark-account-retry"
                >
                  {t("arkFetch.keyRetry")}
                </Button>
              </div>
            ) : accounts.length === 0 ? (
              <div className="flex flex-col gap-2">
                <StepLabel step={t("arkFetch.step1")} label={t("arkFetch.accountLabel")} />
                <p className="text-xs text-muted" data-testid="ark-account-empty">
                  {t("arkFetch.accountEmpty")}
                </p>
              </div>
            ) : (
              <AccountCombo accounts={accounts} accountId={accountId} onSelect={loadKeys} t={t} />
            )}
          </div>

          {/*
        ---- Step 2: choose the key ----

        ⚠️ **Not rendered at all before an account is chosen**; this is the most direct expression of "keys only come after choosing an account".
        Rendered as an empty list it would show "you have not chosen an account yet" as "this account has no keys",
        and the two ask the user to do completely different things.
      */}
          {accountId ? (
            <div className="flex flex-col gap-2" data-testid="ark-fetch-step-key">
              <StepLabel
                step={t("arkFetch.step2")}
                label={t("arkFetch.keyLabel")}
                // The account name goes with step two: when picking a key the user must see which account they are picking under.
                context={accountName}
              />

              {loading ? (
                // ---- State 1 (second level): loading → skeleton ----
                <div className="flex flex-col gap-2" data-testid="ark-key-loading">
                  <Skeleton className="h-14 w-full r-inset" />
                  <Skeleton className="h-14 w-full r-inset" />
                  <Skeleton className="h-14 w-full r-inset" />
                </div>
              ) : fetchError ? (
                // ---- State 5b: listing keys failed → shown as is + retry the current account ----
                <div className="flex flex-col gap-2">
                  <Notice
                    tone="danger"
                    icon="gravity-ui:triangle-exclamation"
                    title={fetchError}
                    data-testid="ark-fetch-error"
                  />
                  <Button
                    size="sm"
                    variant="outline"
                    className="self-start"
                    onPress={() => void loadKeys(accountId)}
                    data-testid="ark-key-retry"
                  >
                    {t("arkFetch.keyRetry")}
                  </Button>
                </div>
              ) : keys && keys.length === 0 ? (
                // ---- State 4: empty list → say where to create one (a different thing from "no permission") ----
                <div
                  className="flex flex-col gap-2 r-inset bg-surface-secondary px-3 py-3"
                  data-testid="ark-key-empty"
                >
                  <p className="text-sm">{t("arkFetch.keyEmptyTitle")}</p>
                  <p className="text-xs text-muted">{t("arkFetch.keyEmptyDesc")}</p>
                  <ConsoleLink t={t} />
                </div>
              ) : keys ? (
                <KeyCombo
                  keys={keys}
                  accountId={accountId}
                  // Only selected when "the chosen one is exactly this key under this account": after switching accounts picked is already cleared,
                  // but one more check blocks the coincidental highlight of "two accounts happen to have the same key id".
                  pickedKeyId={picked?.accountId === accountId ? picked.keyId : ""}
                  onPick={onPick}
                  language={language}
                  myEmail={user.email}
                  t={t}
                />
              ) : null}
            </div>
          ) : accounts && accounts.length > 0 && !accountError ? (
            // When no account is chosen, give a sentence instead of leaving blank: blank looks like "loading broke".
            <p className="text-xs text-muted" data-testid="ark-key-waiting">
              {t("arkFetch.keyWaiting")}
            </p>
          ) : null}

          {picked ? (
            <div className="flex flex-col gap-2" data-testid="ark-fetch-picked">
              <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
                <Icon icon="gravity-ui:circle-check" className="size-4 text-success" />
                <span className="text-sm">{t("arkFetch.pickedTitle")}</span>
                <span className="text-xs text-muted">{t("arkFetch.pickedHint")}</span>
                <Button
                  size="sm"
                  variant="ghost"
                  onPress={() => onPick(null)}
                  data-testid="ark-fetch-use-manual"
                >
                  {t("arkFetch.useManual")}
                </Button>
              </div>

              {/*
            ⚠️ The account-level (global) one must explain its impact right here.
            By the time the user scrolls here in the dialog, the hint in the title is long out of view, and pressing save affects
            **all workspaces that reference it**: consistent with the line on the GlobalArkTab card.
          */}
              {scope === "global" ? (
                <Notice
                  tone="warning"
                  icon="gravity-ui:triangle-exclamation"
                  title={t("arkFetch.cascadeGlobal")}
                  data-testid="ark-fetch-cascade"
                />
              ) : null}
            </div>
          ) : null}
        </>
      )}
    </div>
  );
}

/**
 * Source dropdown: choose from the Volcengine account (default) / type it in.
 *
 * # Why it deserves the space of a control
 *
 * The two paths used to sit side by side (picker on top, input box always below), so "which one should I use" became
 * something users had to judge. And 99% of the time the answer is "choose from Volcengine": manual input is just the fallback.
 * An explicit dropdown makes this clear; the cost is one row of control, and the gain is **one fewer input box by default**.
 *
 * # ⚠️ When unavailable it shows "type it in", not what the user chose
 *
 * When `effectiveSource` and `source` differ (auto-fetch cannot be used), the dropdown must show and lock on
 * the one **actually in effect**. Leaving it on an unclickable "choose from Volcengine" would tell the user
 * "you are using auto-fetch" when that is not true: a lie, and they would keep waiting for that list to appear.
 */
function SourceSelect({
  effectiveSource,
  isDisabled,
  onChange,
  t,
}: {
  /**
   * The source **actually in effect** (already forced to manual when unavailable).
   *
   * It deliberately takes only this one and not `source` as well: the dropdown's display value and its check should both be "which one is actually used",
   * and taking a "what the user chose" too would only make people think there are two places to keep in sync.
   */
  effectiveSource: ArkKeySource;
  isDisabled: boolean;
  onChange: (next: ArkKeySource) => void;
  t: TFunc;
}) {
  return (
    <Select
      // ⚠️ Show effectiveSource instead of source; see the function header comment.
      selectedKey={effectiveSource}
      onSelectionChange={(k) => onChange(String(k) as ArkKeySource)}
      isDisabled={isDisabled}
    >
      <Label>{t("arkFetch.sourceLabel")}</Label>
      <Select.Trigger data-testid="ark-source-select">
        <Select.Value />
        <Select.Indicator />
      </Select.Trigger>
      <Select.Popover>
        <ListBox>
          <ListBox.Item id="fetch" textValue={t("arkFetch.sourceFetch")}>
            {t("arkFetch.sourceFetch")}
          </ListBox.Item>
          <ListBox.Item id="manual" textValue={t("arkFetch.sourceManual")}>
            {t("arkFetch.sourceManual")}
          </ListBox.Item>
        </ListBox>
      </Select.Popover>
      {/* When locked, explain why, otherwise users only see a dropdown that does not click. */}
      {isDisabled ? <Description>{t("arkFetch.sourceLockedHint")}</Description> : null}
    </Select>
  );
}

/**
 * The heading of the whole part + the line "the plaintext does not pass through the browser".
 *
 * ⚠️ **Not rendered on the manual path** (the caller decides by effectiveSource): everything it says is about
 * "fetching from the Volcengine account", with a "recommended" tag, while the user has explicitly chosen to type it in:
 * keeping it would contradict itself, and the line "the plaintext does not pass through the browser" is **false** on the manual path
 * (typed plaintext really does pass through the frontend, it just is not written to disk). Only a screenshot shows this.
 */
function PickerHeading({ t }: { t: TFunc }) {
  return (
    <div className="flex flex-col gap-1.5">
      <div className="flex items-center gap-2">
        <Icon icon="gravity-ui:cloud" className="size-4 text-muted" />
        <p className="text-sm font-medium">{t("arkFetch.title")}</p>
        <Chip size="sm" variant="soft" color="success">
          <Chip.Label>{t("arkFetch.recommended")}</Chip.Label>
        </Chip>
      </div>
      <p className="text-xs text-muted">
        {t("arkFetch.intro")}
        {/* "The plaintext does not pass through the browser" is bold: it is the security premise of this path, not a side note. */}
        <strong className="font-medium text-foreground">{t("arkFetch.plaintextNote")}</strong>
        ——{t("arkFetch.plaintextBody")}
      </p>
    </div>
  );
}

/**
 * The "Step N · label" line.
 *
 * The step number is **the explicit expression of the two-level relation**: users need not guess the order from the layout; the number says it.
 */
function StepLabel({ step, label, context }: { step: string; label: string; context?: string }) {
  return (
    <div className="flex flex-wrap items-center gap-2">
      <span className="text-xs text-muted tabular-nums">{step}</span>
      <span className="text-sm font-medium">{label}</span>
      {context ? (
        <Chip size="sm" variant="soft">
          <Chip.Label>{context}</Chip.Label>
        </Chip>
      ) : null}
    </div>
  );
}

/** External link to the Ark console. Both the not-integrated and empty-list branches need it: then it is the only thing the user can do. */
function ConsoleLink({ t }: { t: TFunc }) {
  return (
    <a
      href={ARK_API_KEY_CONSOLE_URL}
      target="_blank"
      rel="noopener noreferrer"
      className="flex items-center gap-1.5 self-start text-sm text-accent no-underline"
      data-testid="ark-fetch-console-link"
    >
      {t("arkFetch.openConsole")}
      <Icon icon="gravity-ui:arrow-up-right-from-square" className="size-3.5" />
    </a>
  );
}

/**
 * Level one: a searchable account dropdown.
 *
 * # Why ComboBox is safe to use here
 *
 * The two common ComboBox traps (Esc does not close the popover, it pops open again after selecting) are caused by **controlling both
 * `inputValue` and `selectedKey`**: RAC's `useComboBoxState` has an effect that runs on every render,
 * and when `inputValue !== lastValue` it calls `open()` on the menu again.
 *
 * Here **only `selectedKey` is controlled**, and the input value is left to RAC (the account list is local static data,
 * and filtering is done by its built-in `contains`, no network calls, no debounce needed). So that effect takes the
 * uncontrolled path, `lastValue` is maintained by RAC itself, and Esc and "fold up after selecting" are native behavior.
 *
 * ⚠️ `ComboBox.InputGroup` **may only have a fixed two children** (`Input` + `ComboBox.Trigger`);
 * their number and order must not change with state: it treats the last child as the Trigger slot, and conditionally inserting a node makes
 * `Input` disappear from the DOM entirely, losing the text already typed.
 */
function AccountCombo({
  accounts,
  accountId,
  onSelect,
  t,
}: {
  accounts: VolcAccount[];
  accountId: string;
  onSelect: (id: string) => void;
  t: TFunc;
}) {
  return (
    <ComboBox
      selectedKey={accountId || null}
      onSelectionChange={(k) => {
        // null means "the input was cleared". Then fold up the key list too (onSelect("") clears it);
        // that is more honest than keeping the previous account's keys: the user is no longer on that account.
        onSelect(k === null ? "" : String(k));
      }}
      data-testid="ark-account-combo"
    >
      <Label>
        <span className="flex flex-wrap items-center gap-2">
          <span className="text-xs text-muted tabular-nums">{t("arkFetch.step1")}</span>
          <span className="text-sm font-medium">{t("arkFetch.accountLabel")}</span>
          <span className="text-xs text-muted tabular-nums">
            {t("arkFetch.accountCount", { count: accounts.length })}
          </span>
        </span>
      </Label>
      {/* ⚠️ A fixed two children; see the function header comment. */}
      <ComboBox.InputGroup>
        <Input
          placeholder={t("arkFetch.accountPlaceholder")}
          className="w-full pe-9"
          data-testid="ark-account-search"
        />
        <ComboBox.Trigger aria-label={t("arkFetch.accountToggle")} />
      </ComboBox.InputGroup>
      <ComboBox.Popover>
        <ListBox
          renderEmptyState={() => (
            <div
              className="px-3 py-6 text-center text-sm text-muted"
              data-testid="ark-account-no-match"
            >
              {t("arkFetch.accountNoMatch")}
            </div>
          )}
        >
          {accounts.map((a) => (
            <ListBox.Item
              key={a.id}
              id={a.id}
              // textValue must include the ID: users often have only that string of digits at hand (copied from elsewhere).
              textValue={`${a.name} ${a.id}`}
              data-testid={`ark-account-${a.id}`}
            >
              <div className="flex min-w-0 items-center justify-between gap-3">
                <span className="truncate text-sm">{a.name}</span>
                {/* The ID uses monospace + tabular-nums: the IDs of the 10 accounts are all 10 digits,
                    and only aligned can they be scanned. */}
                <span className="shrink-0 font-mono text-xs text-muted tabular-nums">{a.id}</span>
              </div>
            </ListBox.Item>
          ))}
        </ListBox>
      </ComboBox.Popover>
    </ComboBox>
  );
}

/**
 * Level two: choose the key. **A searchable dropdown**, the same kind of control as level one.
 *
 * # Why a dropdown here too
 *
 * It used to be a flat scrollable list; 10 keys laid out took half the dialog height, pushing the "region" and "save"
 * buttons out of view; and its **shape differed** from level one (a dropdown), looking like two things of different nature,
 * while they are really the same "pick one from a pile". With the same control on both levels,
 * "step 1 choose account → step 2 choose key" reads as one continuous path.
 *
 * # How to tell apart 20 keys with similar names
 *
 * The mask **does not help** (Volcengine gives a string of 32 asterisks for all of them, identical), and the names are almost the same too
 * (`api-key-20260827175443` / `api-key-20260827173154`). So:
 *
 * - **Creation time is on the same row as the name**, right-aligned, `tabular-nums`. It is the only way to tell them apart;
 *   demoted to small text on a second line it would be skimmed past as decoration.
 * - ⚠️ **`textValue` must include the formatted time**: RAC's built-in filtering does `contains` on `textValue`;
 *   with only `name`, the promise "search by creation time" becomes false
 *   (what users remember is often "the one from Aug 27", and `2026/08/27` is only in the time, not in the name).
 *   This is pinned by e2e, mutation-checked.
 * - After folding up, the Trigger shows name + time, so "which one did I choose" is always visible.
 */
function KeyCombo({
  keys,
  accountId,
  pickedKeyId,
  onPick,
  language,
  myEmail,
  t,
}: {
  keys: ArkKeyOption[];
  accountId: string;
  /** Id of the currently chosen key, **limited to this account** (the caller already cleared it when the account changed). */
  pickedKeyId: string;
  onPick: (ref: ArkKeyRef) => void;
  language: string;
  /** Email of the current user, used to decide which keys they created. */
  myEmail: string;
  t: TFunc;
}) {
  return (
    <div className="flex flex-col gap-1.5">
      <ComboBox
        selectedKey={pickedKeyId || null}
        onSelectionChange={(k) => {
          // null means "the input was cleared". It is not treated as deselecting: the caller's picked stays,
          // and to change, the user just picks another one; clearing it to null would suddenly unlock the manual input box.
          if (k === null) return;
          onPick({ accountId, keyId: String(k) });
        }}
        data-testid="ark-key-combo"
      >
        <Label>
          <span className="flex flex-wrap items-center gap-2">
            <span className="text-xs text-muted tabular-nums">{t("arkFetch.step2")}</span>
            <span className="text-sm font-medium">{t("arkFetch.keyLabel")}</span>
            <span className="text-xs text-muted tabular-nums" data-testid="ark-key-count">
              {t("arkFetch.keyCount", { count: keys.length })}
            </span>
          </span>
        </Label>
        {/* ⚠️ A fixed two children; see the comment on AccountCombo (conditionally inserted nodes push out Input). */}
        <ComboBox.InputGroup>
          <Input
            placeholder={t("arkFetch.keySearchPlaceholder")}
            className="w-full pe-9"
            data-testid="ark-key-search"
          />
          <ComboBox.Trigger aria-label={t("arkFetch.keyToggle")} />
        </ComboBox.InputGroup>
        <ComboBox.Popover>
          <ListBox
            renderEmptyState={() => (
              <div
                className="px-3 py-6 text-center text-sm text-muted"
                data-testid="ark-key-no-match"
              >
                {t("arkFetch.keyNoMatch")}
              </div>
            )}
          >
            {keys.map((k) => {
              const created = k.createTime ? formatDateTime(k.createTime, language) : "";
              /*
                The relative time ("3 days ago") is only shown when it **really differs from the absolute time**.

                ⚠️ `formatRelative` falls back to formatDateTime beyond 30 days, so that row would
                **print the same timestamp twice** (one at the top right, another below as "created at …"):
                pure noise, and it makes people think those are two different times. 3 of 10 keys are older than 30 days,
                so this is not an edge case. Only a screenshot shows it.
              */
              const relative = k.createTime ? formatRelative(k.createTime, language) : "";
              const showRelative = Boolean(relative) && relative !== created;
              const inactive = Boolean(k.status && k.status !== "Active");
              /*
                ⚠️ **The creator is the only answer to "which one is mine".**

                Tested: under one Volcengine account, 10 keys belonged to 10 different people, and neither the names
                (`api-key-2026082…`) nor the masks (all the same) show ownership. Without this column,
                picking a key is guessing among other people's credentials.

                Only "created by me" is marked, with no default filter: tested, "my own" is often 0 keys
                (none at all among my 10 accounts and dozens of keys), so showing only your own by default
                would give an empty list, which is harder to use than no filter. The search box can already filter by creator.
              */
              const mine = isMyArkKey(k, myEmail);
              return (
                <ListBox.Item
                  key={k.id}
                  id={k.id}
                  /*
                    ⚠️ **The time must go into textValue**, otherwise "search by creation time" is false:
                    RAC uses this string for contains filtering, and it is also the text shown on the Trigger
                    after folding up, so "name + time" is exactly the two things the user wants to see.
                  */
                  /*
                    ⚠️ The creator must go into textValue too: RAC's built-in filtering only looks at this string (not at the rendered
                    JSX); without it, "search a colleague's name to find the one they created" finds nothing, and that is exactly
                    the most natural use once this column was added.
                  */
                  textValue={`${k.name}  ${created}  ${k.createdBy ?? ""}`}
                  data-testid={`ark-key-${k.id}`}
                >
                  <div className="flex min-w-0 flex-1 flex-col gap-0.5">
                    <div className="flex min-w-0 items-baseline justify-between gap-3">
                      <span className="truncate text-sm">{k.name}</span>
                      {/* The only way to tell them apart, so on the same row as the name, right-aligned, tabular digits. */}
                      <span
                        className="shrink-0 font-mono text-xs text-muted tabular-nums"
                        data-testid={`ark-key-created-${k.id}`}
                      >
                        {created || t("arkFetch.keyCreatedUnknown")}
                      </span>
                    </div>
                    <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1">
                      {/* 32 asterisks are longer than the row width; without min-w-0 + truncate they would push the relative time and
                          project name on the same row onto a new line, making the row height jump around. */}
                      <span className="min-w-0 flex-1 truncate font-mono text-xs text-muted">
                        {k.masked}
                      </span>
                      {showRelative ? (
                        <span className="shrink-0 text-xs text-muted">
                          {t("arkFetch.keyCreatedAt", { time: relative })}
                        </span>
                      ) : null}
                      {k.projectName ? (
                        <span className="shrink-0 text-xs text-muted">
                          {t("arkFetch.keyProject", { name: k.projectName })}
                        </span>
                      ) : null}
                      {/*
                        Your own key is marked with an accent Chip, others' with plain gray text:
                        this column must be **scannable**, not something you have to read. When the creator cannot be determined, nothing is shown
                        (better to mark too little than wrongly; see isMyArkKey).
                      */}
                      {mine ? (
                        <Chip size="sm" color="accent" variant="soft">
                          <Chip.Label>{t("arkFetch.keyCreatedByMe")}</Chip.Label>
                        </Chip>
                      ) : k.createdBy ? (
                        <span className="shrink-0 text-xs text-muted">
                          {t("arkFetch.keyCreatedBy", { name: k.createdBy })}
                        </span>
                      ) : null}
                      {/* **Non-Active ones must be visible to users**: choosing one will not work, and the failure then would
                          show as "invalid key", pointing to the credential itself rather than its status. */}
                      {inactive ? (
                        <Chip size="sm" color="warning" variant="soft">
                          <Chip.Label>{k.status}</Chip.Label>
                        </Chip>
                      ) : null}
                    </div>
                  </div>
                </ListBox.Item>
              );
            })}
          </ListBox>
        </ComboBox.Popover>
      </ComboBox>
      {/* With similar names, look at the time: this hint hangs below the box and is visible in the folded state too. */}
      <p className="text-xs text-muted">{t("arkFetch.keyHint")}</p>
    </div>
  );
}

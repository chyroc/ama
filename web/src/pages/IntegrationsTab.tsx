/**
 * Authorization management / Integrations: **the card list is driven by the provider array the
 * backend returns**.
 *
 * # The frontend does not hard-code the provider list
 *
 * The backend's `GET /api/integrations` returns an array, rendered here one card per item. Adding
 * an authorization source only changes the backend
 * registry, and the page gets one more card automatically; with a hard-coded list, forgetting the
 * frontend gives no error, just one card fewer on the page,
 * and nobody is ever notified about "one card missing".
 *
 * # It is a tab of "Global settings" (`/preferences?tab=integrations`)
 *
 * Authorization is **account-level**: the same person sees the same state in any workspace. The
 * backend's four endpoints take the actor via
 * `auth.ActorFrom(ctx)`, with no workspaceId in the path at all, and request parameters cannot
 * change
 * the actor (pinned by `core/integration_actor_test.go` with two identities + six forgery
 * attempts, each one
 * mutation-tested); in authz all four are `ActionNone`, deliberately not `ActionRead`, so that
 * **people who have not joined
 * any workspace yet** can also open this page to authorize.
 *
 * It sits right next to "Ark credentials" (`GlobalArkTab`) on the same page: **the two are two forms
 * of the same thing**:
 * paste a key there, click authorize here.
 *
 * # ⚠️⚠️ This page is **not** where Feishu bots are managed; do not mix the two up
 *
 * Someone reported "global settings cannot switch to another Feishu bot / cannot bind several".
 * After checking, **it is not a missing feature**;
 * two things both labeled "Feishu" were not told apart on this page:
 *
 *   "Feishu user authorization" on this page  **account-level**: I authorize AMA to use **my own
 *                                             Feishu identity**
 *                                             (OAuth gets a user token, for lark-cli in the container
 *                                             to read group chat history)
 *   "Feishu" in the sidebar                   **workspace-level**: register / delete **bot apps**
 *                                             (`cli_xxx`),
 *                                             and bind them to an agent to send/receive messages
 *                                             (`/w/:key/feishu`)
 *
 * When users want to "switch bots" they look here and cannot find it, because it is not here at
 * all. And this page used to
 * **say nothing** about it, so they could only conclude "it cannot be done". The guidance under
 * AppPicker now
 * fills that gap (see the part above `showPicker` in that component: **even with only one app,
 * when the picker does not appear, that guidance must still be there**).
 *
 * # ⚠️ It used to live in "Workspace settings", a trap we really hit
 *
 * Real feedback: create a new workspace, open "Authorization management", immediately see your own
 * authorization records, and suspect
 * "is this a permission leak / data crossing between workspaces". That suspicion was entirely
 * reasonable: **the tab was in "Workspace settings",
 * and every other tab there really was workspace-level**; only this one was not. The backend was
 * always clean;
 * **the location was wrong**.
 *
 * The fix back then was "text making up for the wrong location": a "personal" Chip on the tab + a
 * note inside the tab.
 * Now it has moved to global settings, **the location itself states the scope correctly**, and that
 * Chip was removed with it
 * (with nothing to tell apart, a marker only makes people look for what it is being compared to).
 *
 * There was an old conclusion that "it and Ark credentials are two forms of the same thing,
 * splitting them is harder to understand, so it must stay in
 * workspace settings"; **that is withdrawn**. It was the reason for mixing one account-level tab
 * among four workspace-level ones,
 * and the understanding gained from that "adjacency" did not outweigh the misunderstanding caused by
 * mixed scopes (the misunderstanding really happened, the gain was only
 * a guess). Besides, the pair is still adjacent in the new home, and there both have the same
 * scope, so adjacency is no longer a trap.
 *
 * # Why that note is still kept: location cannot answer "can others see it"
 *
 * The move removed the "scope" misdirection, so the first two sentences of the Notice in TabShell
 * are now just confirmation.
 * **But the third sentence cannot be dropped**: "it is in global settings" does not imply
 * "colleagues cannot see my authorization"; that is a permission question,
 * not a scope question, and it is exactly what users worry about most. Deleting the whole Notice
 * would leave that question
 * unanswered again.
 *
 * ⚠️ The state in cards **is not cached**: every visit and every "check again" asks the backend
 * live, and the backend asks
 * the platform live. For why see the file header of server/core/integration.go: keeping a copy
 * would drift from the platform, and
 * the drift shows up as "the page says authorized, tool calls still fail", the hardest kind to
 * self-check.
 *
 * # Four states, not "authorized / not authorized"
 *
 * ok / warning / error / missing each have their own handling; the card uses `summary` + `fixHint`
 * + `fixUrl` to say "what is going on / what to do / where to do it". Saying only "broken" pushes all
 * the debugging
 * onto the user. See the comment on IntegrationStatus in types/domain.ts.
 *
 * # ⚠️ Collapsible cards + a top summary (the information layout after sources multiplied)
 *
 * With all 7 sources expanded the page is about 3000px, and "which ones need me" is only known
 * after scrolling the whole page, which is exactly this page's
 * main use. Now the collapsed state keeps only "name + status + one sentence"; the expanded state
 * is exactly as before.
 *
 * ## Default expansion policy: **expand only the first one that "needs you"**
 *
 *   error / warning  the first one expanded, the rest collapsed (see `initialExpandedIds`)
 *   ok               collapsed: already fine, nothing to read
 *   missing          collapsed (see below; this one is the easiest to get backwards)
 *
 * ⚠️⚠️ **The first version said "expand all error/warning", and measured data rejected it.**
 * After the change it measured 4333px vs 4183px before, **even taller**, with all 10 expanded. The
 * root cause was not a logic bug, but that
 * rule cancelling itself in the worst case: **a new user has no source configured**, so nearly every
 * card is error,
 * and "expand all todos" = expand all = the status quo, for exactly the people who need this change
 * most.
 * Now exactly one is always expanded, regardless of how many sources. The tradeoffs of three
 * candidate approaches are written
 * above `initialExpandedIds`; read that before changing this rule.
 *
 * ⚠️ **Collapsing `missing` is a judgment, not laziness.** It means "not configured on the server",
 * and expanded the user can do nothing anyway: that card deliberately has no "start authorization"
 * button (not connected ≠ not authorized),
 * only a sentence "ask an admin to configure app_key" and an external link to the console. Laying
 * it out together with error
 * would fill the most visible spots on screen with "things you cannot fix", pushing down the ones
 * really waiting for a click;
 * that would cancel out the benefit of this change. So it is collapsed, but **the status and
 * summary are still on the collapsed row**
 * (the user can see "this exists, an admin must configure it"), and the top summary gives it its
 * own count.
 *
 * ⚠️ **The card "just back from clicking authorize" must stay expanded.** Authorization is async (go
 * click approve on the platform,
 * device codes also need polling); if the expansion state were recomputed from the new status the
 * moment it flips from error to ok, the card would
 * **collapse itself before the user's eyes**, and the two hints "authorization page opened in a new
 * tab" and "come back and click check again when done",
 * plus the polling note, would all disappear together; users would think they broke something.
 * So the source of truth for expansion is **one `expandedIds` for the page**: its initial value is
 * computed from the first frame's states,
 * and once the user touches it, it is no longer recomputed from states; it does not move when
 * `replaceOne` swaps provider data.
 * This and the docs/guides/heroui.md entry "defaultExpanded is read only once on mount" are two
 * sides of
 * the same thing: the trap there is wanting to change it but not being able to; here it is
 * **deliberately** not following the data.
 *
 * ⚠️ Collapsing **does not reorder** anything: the order within a group stays as is (card order =
 * backend registration order,
 * the frontend deliberately does not sort). The top summary row is only **counts**, not grouping;
 * grouping by status would scatter the registration order,
 * and that approach was already rejected.
 *
 * # ⚠️ Split into two groups by identity type
 *
 * Two groups, "personal authorization" and "service accounts", decided by `state.kind` from the
 * backend. The reason is that the two groups' **handling
 * really differs** (the former cannot be bound to a workspace for others to share, the latter can),
 * and the page used to say not a word
 * about this; users only found out when "this source cannot be found in workspace settings", and the
 * error then said "cannot be shared",
 * not pointing at "it is a personal authorization".
 *
 * ⚠️ **This does not break the "no reordering" rule above**: grouping only cuts once by `kind`,
 * `filter` keeps order,
 * and within a group it is still strictly the backend registration order. The only order the
 * frontend decides is "personal group first", an order between two
 * constants, not involving the provider list. See `groupByKind`.
 *
 * When a screen fits only a few cards, users must scroll to compare "which ones are not configured
 * yet", and that is exactly this page's main use.
 * Compression did four things: the status Chip moved into the title row (saving a whole block, and
 * removing the redundant "status" label along the way),
 * name-value pairs went from a two-row two-column grid to a single line, the bottom `<Separator />`
 * was removed, and gap went from 5 to 3.
 *
 * Two rules **do not give way just because "it could be shorter"**:
 *
 *   1. **`summary` is never collapsed; `fixHint` and action buttons are only collapsed when "there
 *      is nothing to do".**
 *
 *      ⚠️ **This rule was revised by this collapsing change, not overturned.** The original was
 *      "summary / fixHint /
 *      the bottom buttons are never collapsed", because "what is going on / what to do next" is why
 *      this page exists,
 *      and hiding actionable controls by default would block the main path. Both reasons still
 *      hold,
 *      so:
 *
 *        · `summary` **moved into the collapsed row** (name + status + this sentence); it is visible
 *          in any state;
 *          "what is going on" lost not a word;
 *        · `fixHint` and buttons only go into the collapsed area for **ok / missing**, and those two
 *          are exactly
 *          the two where "the user has nothing to do" (ok needs nothing, missing cannot be done);
 *        · **error / warning are expanded by default**, so for the ones that "need you", guidance
 *          and buttons
 *          are laid out as before. The main path was not hidden.
 *
 *      In other words, what is collapsed is never "what the user needs right now". If you really
 *      want to go back to "never collapse",
 *      first answer the problem it solves: with all 7 cards laid out, the ones needing action are on
 *      the second screen or later.
 */

import { useCallback, useEffect, useState } from "react";
import type { ReactNode } from "react";
import {
  Button,
  Card,
  Chip,
  Description,
  Disclosure,
  Label,
  ListBox,
  Select,
} from "@heroui/react";
import { Icon } from "@iconify/react";

import { Link } from "react-router-dom";

import { useOptionalWorkspace } from "@/app/WorkspaceGate";
import { AsyncErrorBlock, Empty, ListSkeleton, Notice, SectionTitle } from "@/components/Page";
import { api } from "@/data/api";
import { formatRelative } from "@/lib/format";
import { useAsyncData } from "@/lib/useAsyncData";
import { useIntegrationAuthorize } from "@/lib/useIntegrationAuthorize";
import { usePrefs, useT } from "@/prefs/PrefsContext";
import {
  INTEGRATION_STATUS_COLOR,
  INTEGRATION_STATUS_ICON,
  type IntegrationKind,
  type IntegrationState,
  type LarkUserApp,
} from "@/types/domain";

/**
 * Placeholder for the expansion state in the frame before the first data arrives.
 *
 * ⚠️ Hoisted to module level rather than `new Set()` inside the component: the latter is a new
 * object on every render,
 * which would make memos/effects depending on it think "it changed" every frame.
 */
const EMPTY_EXPANDED: ReadonlySet<string> = new Set<string>();

export function IntegrationsTab() {
  const t = useT();

  // A first-load failure must be visible. It used to be a bare await: once it rejected (this
  // endpoint is ActionNone,
  // so no 403, but network and 5xx can still happen) state stayed null forever and the cards stayed
  // on the skeleton forever,
  // while the buttons below each have their own catch and can show errors; they were just never
  // reached.
  // See the file header of lib/useAsyncData.ts.
  const fetch = useCallback(() => api.listIntegrations(), []);
  const {
    data: providers,
    error: loadError,
    isLoading,
    reload: load,
    setData: setProviders,
  } = useAsyncData(fetch, t("integrations.loadFailed"));

  // Refresh only one card: "check again" on one source should not spin the whole page, nor wipe
  // others' state.
  const replaceOne = useCallback(
    (next: IntegrationState) => {
      setProviders((prev) => (prev ?? []).map((p) => (p.id === next.id ? next : p)));
    },
    [setProviders],
  );

  /**
   * Which cards are expanded. **The source of truth is here, not in each card itself.**
   *
   * ⚠️⚠️ **`null` only means "the first batch of data has not arrived yet", not "the user has not
   * touched it".**
   *
   * We hit a P1 here (caught by code review, reproduced with e2e): it used to be
   * `expanded = expandedIds ?? initialExpandedIds(providers)`, i.e. "while the user has not touched
   * it,
   * recompute from current states on every render". So **when the user did not click collapse and
   * directly clicked "start authorization" → "check again"
   * on the card expanded by default**, `expandedIds` was still null, while `replaceOne`
   * swapped providers and the state flipped from error to ok; the result recomputed by
   * `initialExpandedIds`
   * no longer had it, so **that card collapsed itself before the user's eyes**, taking "authorization
   * page opened in a new
   * tab" and the polling hint with it. That is exactly what the file header promises to avoid, and it
   * gave zero errors.
   *
   * Now: **when the first batch of data arrives, snapshot the first-frame policy once** (the effect
   * below), and after that
   * `expandedIds` is always non-null and never recomputed from `providers` again. User clicks on
   * collapse only add/remove on this
   * snapshot.
   *
   * ⚠️ The effect depends only on `providers`, and inside it **does nothing when `prev !== null`**;
   * so every later re-run triggered by `replaceOne` is a no-op, and does not wipe the user's
   * expansion state.
   */
  const [expandedIds, setExpandedIds] = useState<ReadonlySet<string> | null>(null);
  useEffect(() => {
    if (!providers) return;
    // Only the first time counts: once non-null this never changes it again (the user's expansion
    // state is the only source of truth).
    setExpandedIds((prev) => prev ?? initialExpandedIds(providers));
  }, [providers]);
  // Before the first data arrives (or before the effect runs), give an empty set rather than
  // undefined:
  // in that frame the page is a skeleton/empty state, and nobody reads the expansion state.
  const expanded = expandedIds ?? EMPTY_EXPANDED;

  const setOneExpanded = useCallback((id: string, next: boolean) => {
    setExpandedIds((prev) => {
      const out = new Set(prev ?? []);
      if (next) out.add(id);
      else out.delete(id);
      return out;
    });
  }, []);

  const setAllExpanded = useCallback(
    (next: boolean) => {
      setExpandedIds(next ? new Set((providers ?? []).map((p) => p.id)) : new Set<string>());
    },
    [providers],
  );

  // isLoading means still loading: a different thing from "not connected"; they cannot share the
  // empty state
  if (isLoading) {
    return (
      <TabShell>
        <Card>
          {/* The testid hangs on this layer rather than changing the shared ListSkeleton: that component is reused in a dozen places,
              changing it for one test is not worth it, and it might collide with a file another session is editing. */}
          <Card.Content className="pt-6" data-testid="integrations-loading">
            <ListSkeleton rows={3} />
          </Card.Content>
        </Card>
      </TabShell>
    );
  }

  if (loadError) {
    return (
      <TabShell>
        <Card>
          <Card.Content className="pt-6">
            <AsyncErrorBlock error={loadError} onRetry={load} />
          </Card.Content>
        </Card>
      </TabShell>
    );
  }

  if (providers === null) return null;

  // The backend registered no provider at all. **Not the same as "not connected"**: that is "this
  // source is not configured",
  // this is "there are no sources at all"; the former is shown by the card itself as missing, only
  // the latter takes this empty state.
  if (providers.length === 0) {
    return (
      <TabShell>
        <Card>
          <Card.Content className="pt-6">
            <Empty
              icon="gravity-ui:plug-connection"
              title={t("integrations.noneTitle")}
              description={t("integrations.noneDesc")}
            />
          </Card.Content>
        </Card>
      </TabShell>
    );
  }

  return (
    <TabShell>
      <div className="flex flex-col gap-4" data-testid="integrations-list">
        <SummaryBar
          providers={providers}
          // ⚠️ Pass "how many are expanded **within the current batch of providers**", not
          // `expanded.size`:
          // that set is stored by id and may in theory keep ids no longer in the list (e.g. the
          // backend retired
          // a source), so size could exceed providers.length, and the "expand all" button
          // would flip to "collapse all" too early. Computing the intersection does not depend on
          // how clean the set is.
          expandedCount={providers.filter((p) => expanded.has(p.id)).length}
          onSetAll={setAllExpanded}
        />

        {/* ⚠️ **Two groups, order within each group unchanged**; see the comment on groupByKind. */}
        {groupByKind(providers).map((g) => (
          /* ⚠️ Use `<section aria-labelledby>` rather than a bare div: this repo's heading levels are **flat**
             (card names and SectionTitle are both `<h2>`, see components/Page.tsx), so adding just
             a sibling heading **cannot express "these cards belong to this group"**; screen reader users hear a flat list
             of headings. section + an accessible name adds the containment, without touching the card's `<h2>`
             (e2e finds it by role=heading, and the file has a whole comment on why it must be the only heading). */
          <section
            key={g.kind}
            className="flex flex-col gap-4"
            aria-labelledby={`integrations-group-title-${g.kind}`}
            data-testid={`integrations-group-${g.kind}`}
          >
            {/* Group heading: one sentence on "what this group is". **Not a Card**: it should read as a
                divider in the list; wrapping it in a Card would make a third kind of card, clashing with the source cards below at the same level
                (the skill rule "do not nest a surface inside a surface"). */}
            <div className="flex flex-col gap-0.5 px-1">
              <h2 id={`integrations-group-title-${g.kind}`} className="text-sm font-medium">
                {t(GROUP_TITLE_KEY[g.kind])}
              </h2>
              {/* One sentence explaining how the two groups differ. **That is the whole value of grouping**: with just two headings,
                  users still do not know "why it is in this group", and the two groups' handling really differs
                  (personal authorizations can only be used by yourself; service accounts can be bound to a workspace for everyone to share). */}
              <p className="text-xs text-muted">{t(GROUP_DESC_KEY[g.kind])}</p>
            </div>

            {g.providers.map((p) => (
              <ProviderCard
                key={p.id}
                state={p}
                onRefreshed={replaceOne}
                isExpanded={expanded.has(p.id)}
                onExpandedChange={(next) => setOneExpanded(p.id, next)}
              />
            ))}
          </section>
        ))}

        {/* Like the "Ark credentials" card, write "why this step is needed" on the page: this path involves
            three parties (AMA / external platform / sandbox), and without explanation users just feel it is one more formality. */}
        <Card>
          <Card.Content className="flex flex-col gap-2 pt-6">
            <SectionTitle title={t("integrations.whyTitle")} />
            <p className="text-sm text-muted">{t("integrations.whyBody")}</p>
          </Card.Content>
        </Card>
      </div>
    </TabShell>
  );
}

/**
 * Split into two groups by identity type: **personal authorization** and **service accounts**.
 *
 * # Why split: the two groups' handling really differs
 *
 *   personal          the credential stands for a real person. Only you can use it; it **cannot be
 *                     bound to a workspace** for others to share
 *   service_account   the credential stands for an app. Admins can bind it to a workspace, and
 *                     sharing among many people is a legitimate use
 *
 * Mixed in one stream, this difference had **not a single word** on the page; users only found out
 * when "trying to add
 * a source in workspace settings and not finding it", and the error then said "this source cannot
 * be shared",
 * not pointing at "it is a personal authorization". Grouping is the lightest way to say it up front.
 *
 * # ⚠️⚠️ The order within each group **is unchanged**; this cannot give way
 *
 * "The frontend does not hard-code provider order" is an existing rule of this page (see the file
 * header and SummaryBar):
 * card order = backend registration order; if the frontend added an ordering array, sources the
 * backend adds later would fall outside
 * the array, with zero errors, only showing up as a puzzling order.
 *
 * This **does not break it**: `filter` keeps order, so within a group it is still strictly the
 * order the backend gives;
 * grouping only cuts once on the single `kind` field, without introducing any frontend priority.
 *
 * ⚠️ **The group order (personal first) is the only order the frontend decides, and it only
 * involves two constants, not
 * the provider list**. Personal first because most people do that one first, while service
 * accounts are
 * for when "configuring for the whole workspace".
 *
 * ⚠️ **When `kind` is missing it is treated as `personal`** (older backends do not return it): that
 * is the **conservative**
 * side; at worst a service-account source temporarily shows in the personal group, while the
 * reverse would put a credential standing for a real person
 * into the "can be shared" group, an error in the security direction. See the kind part in
 * types/domain.ts.
 *
 * ⚠️ **Empty groups are not rendered**: a group with only a heading and nothing below is harder to
 * read than no grouping
 * (users go looking for the things that "should be here").
 */
function groupByKind(
  providers: IntegrationState[],
): { kind: IntegrationKind; providers: IntegrationState[] }[] {
  const groups: { kind: IntegrationKind; providers: IntegrationState[] }[] = [
    // ⚠️ Missing is grouped as personal; the rule matches that part in types/domain.ts.
    { kind: "personal", providers: providers.filter((p) => p.kind !== "service_account") },
    { kind: "service_account", providers: providers.filter((p) => p.kind === "service_account") },
  ];
  return groups.filter((g) => g.providers.length > 0);
}

/**
 * i18n keys for group headings and descriptions.
 *
 * ⚠️ **Written as an explicit mapping rather than string building** (`integrations.group${...}`):
 * building it gives
 * a broad template literal type, and the key-union check of `t()` stops working; a misspelled key
 * is no longer a compile
 * error, only showing up on the page as the key itself. Same as the CB_CRED_STATUS_KEY table in this
 * file.
 */
const GROUP_TITLE_KEY: Record<
  IntegrationKind,
  "integrations.groupPersonalTitle" | "integrations.groupServiceTitle"
> = {
  personal: "integrations.groupPersonalTitle",
  service_account: "integrations.groupServiceTitle",
};

const GROUP_DESC_KEY: Record<
  IntegrationKind,
  "integrations.groupPersonalDesc" | "integrations.groupServiceDesc"
> = {
  personal: "integrations.groupPersonalDesc",
  service_account: "integrations.groupServiceDesc",
};

/**
 * Which bucket this card is in: nothing to do / needs you / not yours to handle.
 *
 * ⚠️ **`missing` must have its own bucket, not merged into "to handle".** It means "not configured
 * on the server":
 * an admin must configure it, and the user clicking an authorize button a hundred times would not
 * help (that card deliberately has no such button).
 * Merging it would make the summary tell the user "4 things need you", while one of them they cannot
 * do at all,
 * so they look for something to click on the card, find nothing, and think the UI is broken.
 * **This is where "four states must not degrade into
 * two" lands on the summary.**
 *
 * Merging warning and error into one bucket is a judgment in the other direction: the user **can act
 * on both**
 * (the former "re-authorize", the latter "start authorization"); the difference is only urgency, and
 * urgency is already expressed by
 * the Chip color on the card. Giving each its own bucket would make this row four numbers,
 * and the answer to "what do I need to do now" would be harder to read.
 */
function bucketOf(status: IntegrationState["status"]): "ready" | "todo" | "missing" {
  switch (status) {
    case "ok":
      return "ready";
    case "warning":
    case "error":
      return "todo";
    case "missing":
      return "missing";
  }
}

/**
 * Which cards to expand by default: **only the first one that "needs you"; collapse all the rest.**
 *
 * # ⚠️ This rule was changed by measured data; do not go back to "expand all todos"
 *
 * The first version said "expand all error/warning", which looked reasonable: "those are the ones
 * that need you".
 * Measurement rejected it: **after the change it was 4333px, even taller than 4183px before**, all 10
 * cards expanded,
 * so collapsing had no effect.
 *
 * The root cause was not a logic bug, but that rule **cancelling itself in the real worst case**:
 * a **new user has no source configured**, so nearly every card is error/warning,
 * and "expand all todos" = expand all = the status quo. And that is exactly the person who needs this
 * change most;
 * old users with most things configured are where collapsing helps most visibly.
 * In other words, the more users needed that rule, the less it worked.
 *
 * # Why "expand only the first one", rather than the other two directions
 *
 * The three directions considered, and their tradeoffs:
 *
 *   ① **expand only the first todo** (chosen)  exactly one expanded at all times, regardless of
 *                                              how many sources,
 *                                              so the "new user" worst case is covered
 *                                              automatically;
 *                                              and "there is something to do" is still expressed
 *                                              by **expanding**,
 *                                              the strongest visual signal on the page.
 *   ② collapse todos too + summary as anchor links  shortest collapsed state, but "needs you"
 *                                              degrades into a number,
 *                                              and the user must click before seeing anything
 *                                              actionable.
 *                                              One more click buys only shortness, and this page's
 *                                              problem
 *                                              was never "too long", but **what needs action is not
 *                                              visible**.
 *   ③ a more compact collapsed state for todos  "half expanded" is neither expanded nor collapsed,
 *                                              one fixHint line on each of 7 cards still fills most
 *                                              of the screen,
 *                                              and adds a third visual state for users to learn.
 *
 * The cost of ① is "todos after the 2nd must be opened by the user". That cost is acceptable,
 * because
 * **the top summary already states the total** ("to handle 3"), and the collapsed row carries the
 * summary;
 * the user knows how many things there are and what each is, and expands that one only when acting.
 * "One thing at a time" is also closer to real use: authorizations are completed by jumping out to
 * each platform one by one.
 *
 * ⚠️ **In order, take "the first one" rather than "the most severe one"**: card order = backend
 * registration order,
 * picking by severity would introduce an implicit priority, and whether error or warning is more
 * urgent really depends on what the user
 * wants to do (see that part in bucketOf). Taking the first matches page order, which users can
 * expect.
 */
function initialExpandedIds(providers: IntegrationState[]): ReadonlySet<string> {
  const first = providers.find((p) => bucketOf(p.status) === "todo");
  return new Set(first ? [first.id] : []);
}

/**
 * The summary row at the top: counts per state + an "expand/collapse all".
 *
 * # Why it is not grouping
 *
 * It only **counts**, it does not reorder. Grouping by state would scatter and reorder the providers
 * array, while card order = backend registration
 * order is an existing contract of this page.
 * If the frontend added an ordering array, sources the backend adds later would fall outside it, with
 * zero errors,
 * only showing up as a puzzling order. See the file header.
 *
 * # "Expand/collapse all" decides which action it is by how many are currently expanded
 *
 * The check is **the actual number expanded** (computed and passed in by the parent), not "was the
 * last click expand or collapse":
 * the latter drifts from what the user actually did card by card, so a click would change nothing
 * (they just expanded all by hand, yet the button still wants to expand once more).
 */
function SummaryBar({
  providers,
  expandedCount,
  onSetAll,
}: {
  providers: IntegrationState[];
  expandedCount: number;
  onSetAll: (expanded: boolean) => void;
}) {
  const t = useT();
  const ready = providers.filter((p) => bucketOf(p.status) === "ready").length;
  const todo = providers.filter((p) => bucketOf(p.status) === "todo").length;
  const missing = providers.filter((p) => bucketOf(p.status) === "missing").length;

  // "Expand all" or "collapse all": decided by **how many in this batch of providers are expanded**
  // (the parent computes the intersection
  // and passes it in). `providers.length === 0` never gets here (then the page is the empty state).
  const allExpanded = expandedCount === providers.length;

  return (
    <Card data-testid="integrations-summary">
      {/* ⚠️ The horizontal row must be **its own div**; do not put flex-wrap/items-center directly on
          Card.Content. `.card__content` is an **unlayered** BEM rule with
          `flex-direction: column`, which beats Tailwind utility classes (this repo's rule
          "unlayered CSS beats @layer overrides"); it shows as the three parts stacked into three lines,
          and centered by `items-center`; the screenshot showed exactly that, with typecheck and build all green. */}
      <Card.Content className="pt-5">
        <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
          {/* Three counts side by side. **The order is ready → to handle → not connected**, not sorted by number:
              this row answers "what do I need to do now"; putting the answer (to handle) in the middle,
              with "nothing to do" and "not yours to handle" on the sides, makes it read as one sentence.
              ⚠️ Digits use tabular-nums: this row changes with "check again", and non-monospace digits would make
              the whole row jump left and right. */}
          <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5 tabular-nums">
            <Chip size="sm" variant="soft" color="success">
              <Chip.Label data-testid="integrations-count-ready">
                {t("integrations.summaryReady", { count: ready })}
              </Chip.Label>
            </Chip>
            {/* ⚠️ When to-handle is 0, do not show a "to handle 0" Chip; use a sentence instead:
                "0" needs the user to interpret it, while the sentence gives the conclusion directly. */}
            {todo > 0 ? (
              <Chip size="sm" variant="soft" color="warning">
                <Chip.Label data-testid="integrations-count-todo">
                  {t("integrations.summaryTodo", { count: todo })}
                </Chip.Label>
              </Chip>
            ) : null}
            {/* Not connected uses default (neutral gray): the same color as that Chip on the card (see
                INTEGRATION_STATUS_COLOR); inconsistent colors in the two places would make people think they mean different things.
                ⚠️ It also **must not** use warning, which would paint "needs an admin to configure" as "needs you". */}
            {missing > 0 ? (
              <Chip size="sm" variant="soft" color="default">
                <Chip.Label data-testid="integrations-count-missing">
                  {t("integrations.summaryMissing", { count: missing })}
                </Chip.Label>
              </Chip>
            ) : null}
          </div>

          {/* One sentence saying "what this row means". Three cases, three sentences, none decorative:
              · there are things to handle → say "the first one is already expanded for you" (otherwise they would wonder why
                only one is open)
              · nothing to handle but some not connected → say plainly those are not theirs to handle (that is the
                whole point of the missing bucket)
              · neither → "no authorizations need your attention" */}
          <p className="text-xs text-muted" data-testid="integrations-summary-hint">
            {todo > 0
              ? t("integrations.summaryTodoHint")
              : missing > 0
                ? t("integrations.summaryMissingHint")
                : t("integrations.summaryAllDone")}
          </p>

          <Button
            className="ms-auto"
            size="sm"
            variant="outline"
            onPress={() => onSetAll(!allExpanded)}
            data-testid="integrations-toggle-all"
          >
            {allExpanded ? t("integrations.collapseAll") : t("integrations.expandAll")}
          </Button>
        </div>
      </Card.Content>
    </Card>
  );
}

/**
 * The tab shell: puts the "this is account-level" note at the top, followed by the content passed in.
 *
 * # Why it is still here after moving to global settings
 *
 * The location already states the "scope" correctly, so the first two sentences (belongs to the
 * account / shared by all workspaces) are now **confirmation**,
 * no longer correction. **The reason to keep this Notice is the third sentence**: "can others see my
 * authorization" is a permission
 * question, not a scope question; where the page lives cannot answer it at all, and it is what users
 * worry about most.
 *
 * # Why a shell rather than adding a sentence only in the list branch
 *
 * All four branches (loading / load failed / no authorization sources / has cards) **must show it**.
 * The ones that need this sentence most
 * are exactly the abnormal branches: when loading fails the user stares at an error, and is even more
 * likely to wonder "can I not see
 * others' things because of permissions"; and if the "not a single source" empty state does not state
 * the scope, users think they have not configured it yet.
 *
 * A component rather than copying it four times: with copies you change one and miss three, and
 * missing gives no error.
 */
function TabShell({ children }: { children: ReactNode }) {
  const t = useT();
  return (
    <div className="flex flex-col gap-4">
      {/* tone=info: this is a statement of fact, not a warning. warning would make users think something is broken,
          while they need to do nothing; that is exactly the worry this note removes, not one to create. */}
      <Notice
        tone="info"
        icon="gravity-ui:person"
        title={t("integrations.scopeTitle")}
        hint={`${t("integrations.scopeBody")}${t("integrations.scopePeers")}`}
        data-testid="integrations-scope-note"
      />
      {children}
    </div>
  );
}

/**
 * One card per authorization source. The four states + Summary + fix guidance all land here.
 *
 * ⚠️ The "start authorization → open the authorization page → poll" state machine is in
 * `lib/useIntegrationAuthorize.ts`,
 * **one copy shared with the in-place authorization on the agent config page**. Its three rules (has
 * a cap / interval follows the backend /
 * stop on unmount) and "why device codes must be polled" are written at the top of that file.
 */
function ProviderCard({
  state,
  onRefreshed,
  isExpanded,
  onExpandedChange,
}: {
  state: IntegrationState;
  onRefreshed: (next: IntegrationState) => void;
  /**
   * Expanded or not. **Controlled; the source of truth is the page's single `expandedIds`.**
   *
   * ⚠️ Deliberately not "the card's own useState + defaultExpanded for the initial value": then
   * "expand/collapse all" would need to push commands down to drive child effects, and clicking the
   * same direction twice in a row
   * keeps the value unchanged so the effect does not re-run; **the cards already expanded by hand
   * could not be collapsed** (the button does nothing,
   * with zero errors). A single source of truth has no such seam.
   */
  isExpanded: boolean;
  onExpandedChange: (next: boolean) => void;
}) {
  const t = useT();
  // Relative time must follow the current language
  const { language } = usePrefs();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const auth = useIntegrationAuthorize(state.id, state.pollsAuthorization, onRefreshed, {
    authorizeFailed: t("integrations.authorizeFailed"),
    timeout: t("integrations.authorizeTimeout"),
  });

  async function refresh() {
    setBusy(true);
    setError(null);
    auth.clearError();
    try {
      onRefreshed(await api.getIntegration(state.id));
    } catch (e) {
      setError(e instanceof Error ? e.message : t("integrations.loadFailed"));
    } finally {
      setBusy(false);
    }
  }

  async function revoke() {
    setBusy(true);
    setError(null);
    auth.clearError();
    try {
      await api.revokeIntegration(state.id);
      onRefreshed(await api.getIntegration(state.id));
    } catch (e) {
      // Some platforms have no revoke endpoint, so this is **sure** to be reached. Show the
      // backend's sentence as is: it carries
      // "where to revoke", less likely to go stale than hard-coding "not supported" in the
      // frontend.
      setError(e instanceof Error ? e.message : t("integrations.revokeFailed"));
    } finally {
      setBusy(false);
    }
  }

  const isOK = state.status === "ok";
  // Polling also counts as busy: avoids the user clicking out several parallel authorization flows.
  const disabled = busy || auth.isBusy || auth.isPolling;
  // The card has only one place to show errors; the two sources (local check-again/revoke and the
  // authorization flow) merge there.
  const shownError = error ?? auth.error;

  // ⚠️ **The derived `hasActions` was deleted; do not add it back.**
  //
  // It existed to avoid one specific bad shape: the bottom action area was wrapped by a
  // `<Separator />`, and the three
  // buttons each rendered conditionally, so when "none of the three is given" it left **a dangling
  // separator + an empty
  // zero-height flex row**; the separator said "there is more below", while there was nothing.
  // That was not hypothetical:
  // the AMA card measured exactly that.
  //
  // Now the separator itself is gone, and the bottom row **always has content** (at least the "when
  // checked" sentence),
  // so the "empty row" failure mode cannot happen structurally, and no derived switch is needed to
  // avoid it.

  return (
    <Card data-testid={`integration-card-${state.id}`}>
      {/* gap-3 rather than gap-5: 20px between blocks is too loose, much of the card's height was empty,
          and the blocks already have their own inner spacing; 12px between them is enough to separate them.
          ⚠️ **`py-3` rather than `pt-5`**: a collapsed card has only the title row, and 20px top and bottom would make
          half its height empty; 10 cards waste over 200px. Measured, a collapsed card
          shrank from 90px to ~74px. The expanded-state spacing is provided by Disclosure.Body's pt-3,
          so the expanded look did not get tighter. */}
      <Card.Content className="flex flex-col gap-3 py-3">
        <Disclosure isExpanded={isExpanded} onExpandedChange={onExpandedChange}>
          {/* ⚠️ **The whole title row is the collapse trigger**, rather than adding a small arrow button at the end of the row.
              With a 20px arrow on each of 7 cards, the hit area is small, and "this whole row is clickable"
              is only known after the user discovers the arrow. A clickable row needs no discovery.

              ⚠️⚠️ **The "check again" button must not go inside the trigger.** That would give nested buttons
              (React reports a DOM validation error right away, same as the `Dropdown.Trigger` entry in
              docs/guides/heroui.md), and clicking it would also toggle the collapse state.
              So the structure is "Trigger + button" as two siblings in the same flex row,
              with the Trigger side using `flex-1` to take the remaining width. */}
          <div className="flex flex-wrap items-start gap-x-3 gap-y-2">
            {/* ⚠️⚠️ **Do not wrap this layer in `Disclosure.Heading`.**
                It renders RAC's `Heading` (a real `<h3>`), while the card name below
                is also a heading element; so the same accessible name gets **two nested headings**,
                `getByRole("heading", {name: <card name>})` resolves to 2 elements,
                hitting Playwright strict mode (that is exactly the error we measured).
                Semantically there should be only one heading too: this row's heading is the card name.
                So this is a plain div, and the heading element is left to that one `<h2>`. */}
            <div className="min-w-0 flex-1">
              <Disclosure.Trigger
                className="flex w-full min-w-0 items-start gap-2 r-row px-1 py-0.5 text-left cursor-[var(--cursor-interactive)] hover:bg-surface-secondary"
                data-testid={`integration-toggle-${state.id}`}
              >
                {/* The arrow goes first: it is the only visual cue that "this can expand",
                    and at the end of the row it would drift onto the next line when wrapping on narrow screens.

                    ⚠️⚠️ **Use chevron-down, not chevron-right.**
                    `.disclosure__indicator` is `-rotate-180` when expanded (not 90°),
                    so a right-pointing arrow would point **left** after expanding, which reads like
                    "fold back", exactly opposite to the actual action. Caught in a screenshot,
                    with typecheck and build all green.
                    A down arrow rotated 180° points up; "expanded = up, collapsed = down" is the usual meaning
                    of collapse controls. */}
                <Disclosure.Indicator className="mt-1 shrink-0 text-muted">
                  <Icon icon="gravity-ui:chevron-down" className="size-3.5" />
                </Disclosure.Indicator>

                <div className="flex min-w-0 flex-1 flex-col gap-1">
                  {/* The card name and status Chip always stay on the same line (together they form "what this is +
                      how it is now"; split apart they lose meaning).
                      ⚠️ **`<h2>` is the only heading element in this row**; e2e uses
                      `getByRole("heading", {name: <card name>})` to find it.
                      So do not wrap another `Disclosure.Heading` outside (that would add an `<h3>`,
                      two nested headings hitting strict mode; see the part above);
                      and do not demote it to a `<span>` either; that assertion would find nothing.
                      Measured: a heading nested in a button can still be found by role. */}
                  <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5">
                    <h2 className="min-w-0 text-base font-medium">{state.name}</h2>
                    <Chip size="sm" variant="soft" color={INTEGRATION_STATUS_COLOR[state.status]}>
                      <Chip.Label data-testid={`integration-status-${state.id}`}>
                        {t(`enum.integrationStatus.${state.status}`)}
                      </Chip.Label>
                    </Chip>
                  </div>

                  {/* ⚠️⚠️ **`summary` must also be there in the collapsed state**; this is the precondition for the collapsing design.
                      It is the "what is going on" sentence, and the only thing in the file header's rule that is **never
                      collapsed**. If only name + status stayed when collapsed, users would still have to expand each card to
                      know what happened, which would be no change at all.
                      This is plain text rather than a Notice: a Notice has a background and icon, and one on each of 7 cards
                      would make the collapsed list look busier than the expanded one. The tone of the four states is carried by
                      the color of the Chip above.

                      ⚠️ **When empty the whole line is not rendered**: sources in the ok state deliberately get no
                      summary anymore (that sentence matched the green Chip word for word), and an empty `<p>` would leave
                      a gap with no explainable origin. Check `.trim()`, not truthiness: that backend field
                      has no omitempty, so the empty value is `""`, not undefined. */}
                  {state.summary.trim() ? (
                    <p className="text-xs text-muted" data-testid={`integration-brief-${state.id}`}>
                      {state.summary}
                    </p>
                  ) : null}
                </div>
              </Disclosure.Trigger>
            </div>

            {/* "Check again": a sibling of the Trigger; for why see the part above (nested buttons).
                ⚠️ It is **outside the collapsed area**, clickable even when collapsed: state on this page is not cached,
                and "is this green light I see still valid" is a question that can come up in any state. */}
            <Button
              size="sm"
              variant="outline"
              isDisabled={disabled}
              onPress={refresh}
              data-testid={`integration-refresh-${state.id}`}
            >
              <Icon icon="gravity-ui:arrows-rotate-right" className="size-4" />
              {t("integrations.refresh")}
            </Button>
          </div>

          <Disclosure.Content>
            {/* ⚠️ `px-0`: Disclosure.Body defaults to `p-2` (see heroui's disclosure.css),
                and the outer Card.Content already has padding; stacked, the expanded area would be indented
                further than the title row. Only the bottom margin is kept, to separate it from the card's bottom edge. */}
            <Disclosure.Body className="flex flex-col gap-3 px-0 pt-3 pb-0">
              {/* ⚠️ The description is the **only unconditionally rendered** thing in the collapsed area, so e2e's
                  `expandIntegration` uses it to judge "is it expanded" (before, it used
                  `integration-summary-*`, but that Notice is no longer on every card).
                  Do not remove this testid. */}
              <p className="text-sm text-muted" data-testid={`integration-desc-${state.id}`}>
                {state.description}
              </p>

        {/* "Which app to use": only the Feishu user authorization card renders anything here;
            the decision comes from the backend (see the AppPicker file header), with no hard-coded provider list here.
            Placed above the Summary: first say **who** the "what is going on" is about, then what is going on with it.
            In the reverse order, users reading "ready" would not yet know which app it refers to. */}
        <AppPicker state={state} onSwitched={onRefreshed} isDisabled={disabled} />

        {/* "What is going on / what to do". **The collapsed row already has the summary**, so this
            carries `fixHint` ("what to do next") and the four-state tone colors.
            The testid stays `integration-summary-*`; e2e asserts its text.

            ⚠️ **When both summary and fixHint are empty the whole thing is not rendered**: sources in the ok state give neither
            (that sentence repeats the green Chip), and a Notice with only an icon + empty title is pure noise. */}
        {state.summary.trim() || state.fixHint ? (
          <Notice
            tone={noticeTone(state.status)}
            icon={INTEGRATION_STATUS_ICON[state.status]}
            title={state.summary}
            // FixHint is "what to do". Only present when not ok, so it may be empty here.
            hint={state.fixHint}
            data-testid={`integration-summary-${state.id}`}
          />
        ) : null}

        {/* ⚠️ **Name-value pairs are squeezed into a single "label value" line, no longer a two-row two-column grid.**
            In that grid each detail took two lines (label on one, value on the other) + gap-y-4, so four details
            ate eight lines. With "gray label + monospace value" on one line, the count is the same, no information lost,
            and the height halved.
            `text-xs` sets the hierarchy: these are **secondary** information (the current state and next step are in the
            Notice above), and they should not weigh as much as body text.
            ⚠️ It **is not collapsed**: it holds things like "authorization effective in this workspace"
            and "granted scopes" that explain "why the agent cannot read the group"; hiding them would leave people
            unable to find the cause. */}
        {state.details && state.details.length > 0 ? (
          <dl className="flex flex-col gap-1">
            {state.details.map((d) => (
              <div key={d.label} className="flex flex-wrap items-baseline gap-x-2 text-xs">
                <dt className="shrink-0 text-muted">{d.label}</dt>
                {/* Monospace: these values (email, username, Access Key) often need checking character by character,
                    especially since "authorized user" and "platform-side identifier" are different things, and a mismatch gives no error. */}
                <dd className="min-w-0 break-all font-mono">{d.value}</dd>
              </div>
            ))}
          </dl>
        ) : null}

        {auth.hasOpened ? (
          <Notice
            tone="warning"
            icon="gravity-ui:circle-info"
            title={t("integrations.openedTitle")}
            {...(auth.isPolling
              ? {
                  // While polling: **do not tell the user to click "check again"**; on the device-code
                  // path that sentence is
                  // wrong guidance (we are already checking automatically). Saying "it takes effect
                  // automatically once you click" is right.
                  hint: t("integrations.pollingHint"),
                }
              : { hint: t("integrations.openedHint") })}
            data-testid={`integration-opened-${state.id}`}
          />
        ) : null}
        {shownError ? (
          <p
            className="text-sm text-danger"
            role="alert"
            data-testid={`integration-error-${state.id}`}
          >
            {shownError}
          </p>
        ) : null}

        {/* ⚠️ The bottom row: "when it was checked" on the left, action buttons on the right.
            **This row is rendered unconditionally**, even with no buttons at all.

            Two things therefore hold together:
            - `checkedAt` is kept. It used to be its own block (after the status Chip), and now moves to take
              the empty left part of the button row: **zero net height added**. It is the only thing that says
              "when this state is from": card state is not cached (checked live on page entry),
              so in a tab left open for a long time, that green light may no longer be valid.
            - The old `<Separator />` is removed. Its only trouble was exactly "when none of the three buttons is given,
              a dangling separator + a zero-height empty row were left" (the AMA card measured
              exactly that), so it used to rely on a derived `hasActions` to hide it. Now this row **always has
              content** (at least the checkedAt sentence), a zero-height empty row can no longer happen,
              and the right-aligned buttons themselves say "this is the action area"; the spacing is enough, no line needed. */}
        <div className="flex flex-wrap items-center justify-between gap-2 pt-1">
          <span className="text-xs text-muted">
            {t("integrations.checkedAt", { time: formatRelative(state.checkedAt, language) })}
          </span>

          <div className="flex flex-wrap items-center gap-2">
            {/* FixURL is "where to do it". If present give an external link; **the not-connected branch relies on it especially**:
              then there is no "start authorization" button, and going to the console is the only thing the user can do. */}
            {state.fixUrl ? (
              <Button
                size="sm"
                variant="outline"
                onPress={() => window.open(state.fixUrl, "_blank", "noopener,noreferrer")}
                data-testid={`integration-fixurl-${state.id}`}
              >
                <Icon icon="gravity-ui:arrow-up-right-from-square" className="size-4" />
                {t("integrations.openConsole")}
              </Button>
            ) : null}

            {/* ⚠️ Decided by canRevoke, not by state: when the platform has no revoke endpoint it is false,
              and a button sure to fail is worse than none. **The AMA card is exactly false**
              (the credential is managed by the platform, and the requirement explicitly does not support revoking; see integration_ama.go). */}
            {state.canRevoke ? (
              <Button
                size="sm"
                variant="danger-soft"
                isDisabled={disabled}
                onPress={revoke}
                data-testid={`integration-revoke-${state.id}`}
              >
                {t("integrations.revoke")}
              </Button>
            ) : null}

            {/* ⚠️⚠️ **Decided by canAuthorize, not by state.**
              "Not connected ≠ not authorized": not connected needs an admin to configure it, and the user clicking this button **does nothing**
              (it only gets a 422, and they have no way to know the server must be configured). The backend already computes this as
              canAuthorize, so use it directly; **do not guess from status**. */}
            {state.canAuthorize ? (
              <Button
                size="sm"
                variant={isOK ? "outline" : "primary"}
                isDisabled={disabled}
                onPress={() => void auth.start()}
                data-testid={`integration-authorize-${state.id}`}
              >
                <Icon icon="gravity-ui:key" className="size-4" />
                {isOK ? t("integrations.reauthorize") : t("integrations.authorize")}
              </Button>
            ) : null}
          </div>
        </div>
            </Disclosure.Body>
          </Disclosure.Content>
        </Disclosure>
      </Card.Content>
    </Card>
  );
}

/**
 * Picker for "which app to use". **Currently only Feishu user authorization has this concept.**
 *
 * # ⚠️ "Switch the current app" and "authorize" are two different things
 *
 * Switching only changes an **account-level preference** (the backend's `lark_user_app_prefs`,
 * whose primary key is just the actor);
 * it controls **whom this card shows, and whom "start authorization" authorizes**. Switching to an
 * app never authorized,
 * the card should honestly become "not authorized" and offer the authorize entry; **it must never
 * silently fall back to the previous app's
 * token**, which is exactly the shape of the bug just fixed (using workspace A's token with workspace
 * B's app_id,
 * lark-cli returns 99991668 "invalid token", and that message does not point at "the wrong app was
 * picked").
 *
 * So after switching, the whole card is redrawn from **the new state the backend returns**, with no
 * guessing in the frontend.
 *
 * # ⚠️ It does not decide which one the agent uses at runtime
 *
 * Injection is pinned by **the run's workspace**, ignoring this preference (see the comment above
 * GetLarkUserAppPref in
 * server/core/store.go). So the text only says "which one this page shows and authorizes",
 * not "the agent will use this"; saying it wrong would make users think switching changes the
 * agent's behavior.
 *
 * # Why decide by `apps_unsupported` rather than `state.id === "lark-user"`
 *
 * Of the four sources only Feishu has apps to choose from; the other three get 400 +
 * `apps_unsupported` from the backend.
 * Checking the provider id would hard-code a list in the frontend (explicitly forbidden by
 * useIntegrationAuthorize.ts),
 * and if another source later supports picking an app, missing this would give no error; the picker
 * would just not appear.
 *
 * # Not rendered when there is only one app
 *
 * Nothing to choose; a dropdown with one item is noise, and the details row under the card already
 * says
 * "which one is in use".
 */
function AppPicker({
  state,
  onSwitched,
  isDisabled,
}: {
  state: IntegrationState;
  onSwitched: (next: IntegrationState) => void;
  isDisabled: boolean;
}) {
  const t = useT();
  /*
   * Which workspace the "where to manage bots" link points to.
   *
   * ⚠️ **Use useOptionalWorkspace rather than reading `?from=` directly**: that parameter **can be
   * edited by the user**
   * (it may be a nonexistent key, or someone else's workspace). PreferencesRoute has already
   * resolved it into a
   * workspace once and checked membership; when not a member it does not wrap the shell, so this
   * reads null and the link degrades into
   * plain text. Copying the string from the URL would build a link that 404s or 403s when clicked.
   * See the part above useOptionalWorkspace in WorkspaceGate.
   */
  const fromKey = useOptionalWorkspace()?.workspace.key ?? null;
  const [apps, setApps] = useState<LarkUserApp[] | null>(null);
  const [switching, setSwitching] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Every change of card state (after authorizing, revoking, checking again) must refetch:
  // authorized is a per-app
  // property, and after revoking the current one it must become "not authorized", otherwise the
  // option still says authorized.
  // Depend on checkedAt rather than the whole state: the latter is a new object every render, and
  // would refetch forever.
  useEffect(() => {
    let alive = true;
    void (async () => {
      try {
        const list = await api.listIntegrationApps(state.id);
        if (alive) setApps(list);
      } catch {
        // ⚠️ **Silently treat it as "this source has no apps to choose"**, with no error. Three
        // sources always get here
        // (the backend returns 400 + apps_unsupported); that is a normal state, not a fault;
        // showing it as an error would hang red text on three good cards.
        //
        // A real read failure (network down) also lands here, at the cost of the picker not
        // appearing: then the card
        // itself is also showing an error, so users can see "something is wrong" and will not think
        // everything is fine.
        if (alive) setApps(null);
      }
    })();
    return () => {
      alive = false;
    };
  }, [state.id, state.checkedAt]);

  // The one in effect. **Computed before switchTo**: it is used to check "is the clicked one already
  // selected".
  // The backend guarantees exactly one current (the first in the list when no preference is set);
  // when unavailable give an empty string;
  // then Select shows the placeholder rather than blank.
  const currentAppId = apps?.find((a) => a.current)?.appId ?? "";

  async function switchTo(appId: string) {
    // Clicking the one already selected: do nothing. **Do not send the PUT anyway**: it would go
    // through the whole redraw,
    // making the card flash while the user changed nothing.
    if (appId === currentAppId) return;
    setSwitching(true);
    setError(null);
    try {
      // The backend returns **the complete state after switching**; redraw the whole card with it
      // directly.
      // Changing current in the frontend while keeping the old state would, after switching to an
      // unauthorized app,
      // keep saying "ready" on the card; that would move the bug into the UI.
      onSwitched(await api.selectIntegrationApp(state.id, appId));
      // apps is refetched by the effect above following checkedAt; no need to change it by hand
      // here.
    } catch (e) {
      setError(e instanceof Error ? e.message : t("integrations.appSwitchFailed"));
    } finally {
      setSwitching(false);
    }
  }

  // Other sources have no concept of "apps" at all (the backend returns apps_unsupported → null
  // here); the whole block is not rendered.
  if (!apps || apps.length === 0) return null;

  /*
   * ⚠️ **With only one app the picker is not rendered, but the guidance below still is.**
   *
   * Picker: nothing to choose; a dropdown with one item is noise (the details row under the card
   * already says
   * which one is in use).
   *
   * Guidance: **it is needed most exactly when "there is only one"**. The user's report was "global
   * settings cannot switch to another
   * Feishu bot", and their workspace really had only one app connected, so this page had neither a
   * picker
   * nor any hint that "bots are managed elsewhere", which looked like a missing feature.
   * In fact Feishu apps (bots) are **workspace-level** resources, managed under "Feishu" in the
   * sidebar.
   */
  const showPicker = apps.length >= 2;

  return (
    <div className="flex flex-col gap-1">
      {showPicker ? (
      <Select
        // secondary: this picker is inside a Card, and the shadowed primary one would look like it
        // floats on the card surface.
        variant="secondary"
        // Controlled. selectedKey points to current; after switching it is driven by the new state the
        // backend returns.
        selectedKey={currentAppId}
        onSelectionChange={(k) => void switchTo(String(k))}
        // Disable the whole picker while switching, to avoid clicking out two parallel PUTs.
        // ⚠️ **Do not switch to something like isPending that changes size**: right below this row
        // are the Notice and
        // details, and if the picker grows taller the whole card jumps.
        isDisabled={isDisabled || switching}
        // Keep the width small: this is a small control inside a card, not a main form field.
        // Also give a lower bound, so a slightly long workspace name does not squeeze out the appId.
        className="w-full sm:max-w-md"
        data-testid={`integration-app-select-${state.id}`}
      >
        <Label>{t("integrations.appLabel")}</Label>
        {/* ⚠️ The testid hangs on the Trigger; **assertions on the collapsed state must use it, not the outer one**.
            The div rendered by the outer `<Select>` contains a hidden native `<select>`, which holds
            the textValue of **all options**; so an assertion on the outer one like "the text contains <a workspace name>" is **always true**,
            staying green even if the collapsed state is broken entirely. Tested: swapping this back to a bare `<Select.Value />`
            (the version that loses the workspace name), the test still passed; that assertion was testing nothing back then. */}
        <Select.Trigger data-testid={`integration-app-trigger-${state.id}`}>
          {/* ⚠️ **The collapsed state must render itself; a bare `<Select.Value />` is not enough.**
              Select.Value renders the whole option's Label and Chip, **but drops the Description**
              (measured), and the workspace name and appId are exactly what is written in the Description. So two apps both named
              the same in the collapsed state are **the same line of text**, and users cannot tell which one is in use,
              which is exactly the question this card exists to answer.

              So it uses the official "Custom Value" render function (render props are
              `{defaultChildren, isPlaceholder, state}`; see docs/guides/heroui.md):
              the name as the main part, followed by the workspace name to tell them apart. **The Chip is not rendered again**: the authorization status
              is already stated in the "status" row below, and showing it again in the collapsed state is repeated information. */}
          <Select.Value>
            {({ defaultChildren, isPlaceholder, state }) => {
              const picked = apps.find((a) => a.appId === state.selectedItems[0]?.key);
              // Fall back to defaultChildren: when selectedKey points to an item not in the
              // collection,
              // a bare Select.Value renders blank (no error, no placeholder), showing up in the UI as
              // "no app selected". See that trap in docs/guides/heroui.md.
              if (isPlaceholder || !picked) return defaultChildren;
              return (
                <span className="flex min-w-0 items-center gap-1.5">
                  <span className="truncate">{picked.name}</span>
                  {/* The workspace name is the only thing users can tell apart among "three apps with the same name", so it must
                      be there in the collapsed state too. text-muted lowers its level: it is secondary information, not a heading. */}
                  <span className="truncate text-muted">{picked.workspaceName}</span>
                </span>
              );
            }}
          </Select.Value>
          <Select.Indicator />
        </Select.Trigger>
        {/* ⚠️ There used to be an `integrations.appHint` sentence here ("decides which app this page shows and authorizes;
            the agent at runtime picks automatically by its workspace"), removed during a cleanup:
            the picker already has the Label "app in use" above it, and the second half was about the agent's runtime
            behavior, which belongs to another page (agent config); here it would only make people think
            switching this dropdown changes the agent's behavior.
            ⚠️ That guidance **must not go into the Select's Description**: that spot is inside the Trigger
            and is rendered along with Select.Value, so every option would be followed by this sentence. */}
        <Select.Popover>
          <ListBox>
            {apps.map((a) => (
              // textValue is **name + workspace name**: it is the string used for keyboard search and
              // read out by accessibility tools.
              // With only name, three apps with the same name would be identical to a screen reader,
              // impossible to pick.
              <ListBox.Item
                key={a.appId}
                id={a.appId}
                textValue={`${a.name} · ${a.workspaceName}`}
              >
                {/* The name as the main title, workspace name and appId as secondary information.
                    ⚠️ **All three apps may have the same name** (measured: all were created from the same doc),
                    so those two items are not decoration; they are the only thing users can tell apart. */}
                <Label>{a.name}</Label>
                <Description>
                  {/* Workspace name first: the answer to "which one to use" is usually "which workspace I am working in".
                      appId uses monospace; it is something to check character by character. */}
                  {a.workspaceName}
                  <span className="mx-1.5 text-muted">·</span>
                  <span className="font-mono">{a.appId}</span>
                </Description>
                {/* Authorization status. **Not authorized uses warning, not danger**: something fixed with one click
                    should not look like a fault. */}
                <Chip size="sm" variant="soft" color={a.authorized ? "success" : "warning"}>
                  <Chip.Label>
                    {a.authorized
                      ? t("integrations.appAuthorized")
                      : t("integrations.appUnauthorized")}
                  </Chip.Label>
                </Chip>
                <ListBox.ItemIndicator />
              </ListBox.Item>
            ))}
          </ListBox>
        </Select.Popover>
      </Select>
      ) : null}
      {/*
        ⚠️⚠️ "Where are bots managed": this guidance is where the feedback "the authorization page
        cannot switch Feishu bots" lands.
        It is not a missing feature; **two concepts were not told apart on this page**:

          this page (authorization management)  account-level: **I** authorize AMA to use **my**
                                                Feishu identity (get a user token)
          "Feishu" in the sidebar               workspace-level: register a **bot app** for agents to
                                                send/receive messages

        Users look for "switch bot" on this page and cannot find it, because it is not here at all.
        And this page used to say nothing about it,
        so they could only conclude "it cannot be done".

        ⚠️ /preferences is **outside** WorkspaceGate, with no "current workspace" available; and
        LarkUserApp
        only has the workspace **name**, no key. So the link uses the page's own `?from=<key>` (the
        source workspace recorded when entering this page);
        without it it degrades into plain text, rather than making up a link that may be wrong.
      */}
      <p className="text-xs text-muted" data-testid={`integration-app-manage-hint-${state.id}`}>
        {fromKey ? (
          <>
            {t("integrations.appManageHint")}{" "}
            <Link to={`/w/${fromKey}/feishu`} className="underline">
              {t("nav.feishu")}
            </Link>
          </>
        ) : (
          t("integrations.appManageHintNoLink")
        )}
      </p>
      {error ? (
        <p
          className="text-sm text-danger"
          role="alert"
          data-testid={`integration-app-error-${state.id}`}
        >
          {error}
        </p>
      ) : null}
    </div>
  );
}

/**
 * Mapping from the four states to the Notice tone.
 *
 * `missing` uses neutral info rather than warning: it is not "something went wrong", just that this
 * feature is not connected yet.
 * Showing it as a warning would make users think something is broken and go to an admin to put out
 * a fire, while it simply is not configured.
 */
function noticeTone(status: IntegrationState["status"]): "info" | "warning" | "danger" {
  switch (status) {
    case "ok":
      return "info";
    case "warning":
      return "warning";
    case "error":
      return "danger";
    case "missing":
      return "info";
  }
}

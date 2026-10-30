/**
 * Global settings (account level).
 *
 * Route `/preferences`, deliberately outside the workspace (a sibling of `/workspaces`):
 * these settings belong to **you as a person**; they should not change when switching workspace,
 * and must open even when you belong to no workspace.
 *
 * **This page has one route but two shells**: with `?from=<key>` PreferencesRoute wraps it in
 * AppShell (sidebar present, back returns to that workspace); without it, it renders bare. This
 * component **differs in only one place** between the two:
 * the "back" link below, decided by whether there is a workspace context. The decision to wrap,
 * and how an invalid key
 * degrades, all live in `app/PreferencesRoute.tsx`; do not write them again here.
 *
 * # Split with "workspace settings", and why no second page was created
 *
 *   this page (global settings)   appearance & language / Ark credentials / API Key /
 *                                 authorization management
 *                                 (only yours, the same in every workspace)
 *   workspace settings (/w/:key/settings)  basic info / members / Ark credentials / danger zone
 *                                 (affects the whole workspace)
 *
 * API Key and authorization management were both **moved over** from workspace settings. Both
 * were always account-level
 * (`auth_api_keys` has no workspace_id column; the four `/api/integrations` routes fetch by actor,
 * with no workspaceId in the path at all). Living in workspace settings was only historical, at
 * the cost of relying on a "personal"
 * Chip plus a note to explain "why the same ones show in another workspace": **text making up for
 * the wrong location**.
 *
 * When moving them we **did not create a separate "global settings" page**; instead the existing
 * `/preferences` was upgraded into
 * a tabbed form. The reason is the insight noted in the workspace settings file header: account-level
 * and workspace-level things being
 * "adjacent + similar-looking + opposite in meaning" is where this product most easily fools
 * people. Creating another sibling account-level
 * entry would copy that trap; users would ask "what is the difference between preferences and
 * global settings",
 * and that question has no good answer. The path also stays `/preferences`: the account menu, the
 * workspace list page,
 * and a batch of e2e all point at it; changing the path would only bring a pile of dead links.
 *
 * # The "personal" Chip retired together with authorization management
 *
 * That Chip (`integrations.scopeBadge`) used to mark, **inside workspace settings**, the few
 * account-level tabs. Authorization management was the last one; once it moved, all five tabs in
 * workspace settings are workspace-level,
 * and the marker has nothing left to distinguish; keeping it would be a sign pointing at nothing.
 * So the Chip's rendering,
 * the `personal` flag and that translation key were all deleted.
 *
 * **This page does not need to take over the Chip**: the page itself carries the scope (the title
 * is "Global settings",
 * and all four tabs are account-level); marking each one again would mean marking all five, which
 * is the same reason we **did not**
 * mark workspace-level tabs back then.
 */

import { Link, useSearchParams } from "react-router-dom";
import {
  Button,
  Card,
  Chip,
  ListBox,
  Select,
  ToggleButton,
  ToggleButtonGroup,
} from "@heroui/react";
import { Icon } from "@iconify/react";
import { useOnboarding } from "@/app/OnboardingTour";
import { useOptionalWorkspace } from "@/app/WorkspaceGate";
import { PageBody, PageHeader, SectionTitle } from "@/components/Page";
import { ApiKeysTab } from "@/pages/ApiKeysTab";
import { GlobalArkTab } from "@/pages/GlobalArkTab";
import { HostsTab } from "@/pages/HostsTab";
import { IntegrationsTab } from "@/pages/IntegrationsTab";
import { SuggestionsTab } from "@/pages/SuggestionsTab";
import { usePrefs } from "@/prefs/PrefsContext";
import type { Appearance, Language } from "@/prefs/storage";

const LANGUAGE_LABELS: Record<Language, string> = {
  "zh-CN": "简体中文",
  "en-US": "English",
};

/**
 * Tab definitions. Store translation keys, not text: when module-level constants are evaluated
 * PrefsContext does not exist yet,
 * and calling `t()` here would get no context (see docs/guides/i18n.md).
 */
const TABS = [
  { id: "general", labelKey: "prefs.tabGeneral" },
  // Ark credentials → authorization management → API Key: the two "outgoing" forms first, the
  // "incoming" one after.
  //
  // Ark credentials and authorization management **are two forms of the same thing** (paste a key
  // there, click authorize here),
  // so they sit next to each other; this mental model moved over from workspace settings, where it
  // was built by adjacency.
  // ⚠️ The difference: on **this page** their scope is finally the same (both account-level), while
  // in workspace settings
  // the two meant opposite things (Ark credentials workspace-level / authorization account-level),
  // so adjacency itself was a trap, patched only by a
  // "personal" Chip. After the move that trap no longer exists, so the Chip does not need to come
  // along.
  //
  // API Key goes last: the first two are "we call others on your behalf", it is "others call us".
  { id: "ark", labelKey: "prefs.tabArk" },
  { id: "integrations", labelKey: "prefs.tabIntegrations" },
  { id: "apikeys", labelKey: "prefs.tabApiKeys" },
  // Hosts follow API Key: the AMA_TOKEN the daemon registers with is the
  // UserAPI Key created in the previous tab; adjacency links "the key" with "the machines that
  // registered with it".
  { id: "hosts", labelKey: "prefs.tabHosts" },
  // Product suggestions go **last**, not in the same group as the three above.
  //
  // The first four are all "settings": changing them affects how this platform works for you
  // (language, colors,
  // which key to use, who can call you). This one is a "conversation": you say something to the
  // platform and wait for a reply.
  // A different kind of thing, so it goes after the settings rather than in the middle.
  //
  // ⚠️ It also must not go before general to take the top spot: that would make everyone who comes
  // to change the language first see a
  // "give feedback" entry, as if nagging for feedback. It should be something you look for when
  // "you want to say something while using it".
  { id: "suggestions", labelKey: "prefs.tabSuggestions" },
] as const;

type TabId = (typeof TABS)[number]["id"];

/** Appearance and language. */
function GeneralTab() {
  const { language, appearance, resolvedAppearance, setLanguage, setAppearance, t } = usePrefs();
  const onboarding = useOnboarding();

  const appearanceOptions: { id: Appearance; label: string; icon: string }[] = [
    { id: "light", label: t("prefs.appearanceLight"), icon: "gravity-ui:sun" },
    { id: "dark", label: t("prefs.appearanceDark"), icon: "gravity-ui:moon" },
    { id: "system", label: t("prefs.appearanceSystem"), icon: "gravity-ui:display" },
  ];

  return (
    <Card>
      <Card.Content className="flex flex-col gap-6 pt-6">
        <div className="flex flex-col gap-4">
          <SectionTitle title={t("prefs.language")} description={t("prefs.languageDesc")} />
          {/* Inline controls do not stretch; constrained to content width */}
          <Select
            className="max-w-3xs"
            selectedKey={language}
            onSelectionChange={(k) => setLanguage(String(k) as Language)}
            aria-label={t("prefs.language")}
          >
            <Select.Trigger data-testid="prefs-language">
              <Select.Value />
              <Select.Indicator />
            </Select.Trigger>
            <Select.Popover>
              <ListBox>
                {(Object.keys(LANGUAGE_LABELS) as Language[]).map((lang) => (
                  <ListBox.Item key={lang} id={lang} textValue={LANGUAGE_LABELS[lang]}>
                    {LANGUAGE_LABELS[lang]}
                  </ListBox.Item>
                ))}
              </ListBox>
            </Select.Popover>
          </Select>
        </div>

        <div className="flex flex-col gap-4">
          <SectionTitle title={t("prefs.appearance")} description={t("prefs.appearanceDesc")} />
          <div className="flex flex-wrap items-center gap-3">
            {/* A three-way mode uses a separated toggle group, lighter than a dropdown and shows all options at once */}
            <ToggleButtonGroup
              isDetached
              selectionMode="single"
              disallowEmptySelection
              selectedKeys={new Set([appearance])}
              onSelectionChange={(keys) => {
                const next = [...keys][0];
                if (next) setAppearance(String(next) as Appearance);
              }}
              aria-label={t("prefs.appearance")}
            >
              {appearanceOptions.map((opt) => (
                <ToggleButton key={opt.id} id={opt.id} data-testid={`prefs-appearance-${opt.id}`}>
                  <Icon icon={opt.icon} className="size-4" />
                  {opt.label}
                </ToggleButton>
              ))}
            </ToggleButtonGroup>

            {/* When following the system, say what it currently resolves to, otherwise users do not know which set is in effect */}
            {appearance === "system" ? (
              <Chip size="sm" variant="soft">
                <Chip.Label>
                  {t("prefs.appearanceSystemHint", {
                    mode:
                      resolvedAppearance === "dark"
                        ? t("prefs.appearanceDark")
                        : t("prefs.appearanceLight"),
                  })}
                </Chip.Label>
              </Chip>
            ) : null}
          </div>
        </div>

        {/*
          Review the onboarding again.
          **Placed here rather than in the account menu**: it is the same kind as language and
          appearance, settings for "how this person
          uses the platform", which should not change with workspace. The account menu only keeps
          personal identity items (see AppShell).

          Why this entry is required: onboarding can be skipped, and skipping is **stored in the
          database** (otherwise you would be
          bothered at every login). Without a way to review it, once users click skip they can never
          get it back;
          one misclick would permanently turn off a feature.
        */}
        <div className="flex flex-col gap-4">
          <SectionTitle title={t("onboarding.replay")} description={t("onboarding.replayDesc")} />
          <div className="flex flex-wrap items-center gap-3">
            <Button
              variant="outline"
              onPress={() => onboarding?.restart()}
              // Disable rather than hide when there is no context: if hidden, "where did this
              // button go"
              // cannot be traced; disabled is at least visible. On the normal path it is always
              // available (the Provider
              // wraps the whole route table inside AuthGate).
              isDisabled={!onboarding}
              data-testid="replay-onboarding"
            >
              {t("onboarding.replayAction")}
            </Button>
          </div>
        </div>
      </Card.Content>
    </Card>
  );
}

export function PreferencesPage() {
  const [params, setParams] = useSearchParams();
  const { t } = usePrefs();
  const tab = (params.get("tab") ?? "general") as TabId;

  /**
   * Where "back" goes depends on whether you came in with a source workspace:
   *
   *   /preferences?from=growth  → back to that workspace (the shell is still there; back just
   *                               wraps up)
   *   /preferences              → back to the workspace list (there was no workspace context)
   *
   * **The check is whether there is a workspace context, not parsing the query ourselves**:
   * PreferencesRoute has already
   * made this decision (and also handled the "key does not exist / not a member" degrade); matching
   * `?from=` again here would be a second rule, which would sooner or later disagree with that one,
   * and the disagreement would show as "the back button
   * points to a workspace you cannot enter".
   *
   * The degrade need not be written a second time here: when the workspace cannot be fetched,
   * PreferencesRoute does not wrap the shell at all,
   * so the context read here is null, taking the same path as "no parameter".
   *
   * Hard-coding back to /workspaces really caused a problem once: a user in a workspace clicked the
   * sidebar to change colors,
   * clicked back and was sent to the workspace list, though they never meant to leave that
   * workspace.
   */
  const ws = useOptionalWorkspace();
  const backTo = ws ? `/w/${ws.workspace.key}` : "/workspaces";
  const backLabel = ws
    ? t("prefs.backToWorkspace", { name: ws.workspace.name })
    : t("prefs.backToWorkspaces");

  return (
    <PageBody>
      <PageHeader
        title={t("prefs.title")}
        description={t("prefs.desc")}
        above={
          <Link
            to={backTo}
            data-testid="prefs-back"
            className="flex w-fit items-center gap-1 text-sm text-muted no-underline hover:text-foreground"
          >
            <Icon icon="gravity-ui:arrow-left" className="size-4" />
            {backLabel}
          </Link>
        }
      />

      {/* The tab bar shape matches the workspace settings page: the two pages exist side by side, and looking different would only make people think
          they are two different kinds of things. The selected underline uses an after pseudo-element and takes no layout height. */}
      <div className="flex items-center gap-1 border-b border-border">
        {TABS.map((tb) => (
          <button
            key={tb.id}
            type="button"
            onClick={() => {
              params.set("tab", tb.id);
              setParams(params, { replace: true });
            }}
            data-testid={`prefs-tab-${tb.id}`}
            className={[
              "relative flex items-center gap-1.5 px-3 py-2 text-sm cursor-[var(--cursor-interactive)]",
              tab === tb.id
                ? "font-medium text-foreground after:absolute after:inset-x-0 after:-bottom-px after:h-0.5 after:bg-accent"
                : "text-muted hover:text-foreground",
            ].join(" ")}
          >
            {t(tb.labelKey)}
          </button>
        ))}
      </div>

      {tab === "ark" ? <GlobalArkTab /> : null}
      {tab === "integrations" ? <IntegrationsTab /> : null}
      {tab === "apikeys" ? <ApiKeysTab /> : null}
      {tab === "hosts" ? <HostsTab /> : null}
      {tab === "suggestions" ? <SuggestionsTab /> : null}
      {/* Unknown tabs fall back to the default page, rather than rendering a shell with "only a tab bar, empty below";
          that gives no error but looks broken.
          ⚠️ When adding a tab this line needs another condition; missing it shows as "clicking that tab shows
          appearance and language": both pages look fine, only the content is wrong. */}
      {tab !== "ark" &&
      tab !== "integrations" &&
      tab !== "apikeys" &&
      tab !== "hosts" &&
      tab !== "suggestions" ? (
        <GeneralTab />
      ) : null}
    </PageBody>
  );
}

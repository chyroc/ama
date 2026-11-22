import { useState } from "react";
import { Icon } from "@iconify/react";
import { Link } from "react-router-dom";
import { useOnboarding } from "@/app/OnboardingTour";
import { AzureButton } from "@/components/azure/atoms/Button";
import { AzureChip } from "@/components/azure/atoms/Chip";
import { AzureSegmentedControl } from "@/components/azure/atoms/SegmentedControl";
import { usePrefs } from "@/prefs/PrefsContext";
import type { Appearance, Language, Theme } from "@/prefs/storage";

function ThemePreview({ theme, label, description }: {
  theme: Theme;
  label: string;
  description: string;
}) {
  return (
    <span className={`theme-preview theme-preview--${theme}`} aria-hidden="true">
      <span className="theme-preview__canvas">
        <span className="theme-preview__side" />
        <span className="theme-preview__content">
          <span className="theme-preview__title" />
          <span className="theme-preview__line" />
          <span className="theme-preview__cards"><span /><span /></span>
          <span className="theme-preview__button" />
        </span>
      </span>
      <span className="flex flex-col gap-1 text-left">
        <span className="text-sm font-medium text-ink">{label}</span>
        <span className="max-w-3xs text-xs leading-5 text-ink-2">{description}</span>
      </span>
    </span>
  );
}

function Section({ title, description, children }: {
  title: string;
  description?: string;
  children: React.ReactNode;
}) {
  return (
    <section className="flex flex-col gap-3 rounded-[10px] border border-line bg-surface p-4 shadow-hairline">
      <div>
        <h2 className="text-[14px] font-semibold text-ink">{title}</h2>
        {description ? <p className="mt-1 text-[12.5px] leading-5 text-ink-2">{description}</p> : null}
      </div>
      {children}
    </section>
  );
}

/** Full preference presentation for the Azure component contract. */
export function AzurePreferencesPage() {
  const { language, appearance, resolvedAppearance, setLanguage, setAppearance, theme, setTheme, t } = usePrefs();
  const onboarding = useOnboarding();
  const [backHover, setBackHover] = useState(false);
  const appearanceOptions = (["light", "dark", "system"] as const).map((id) => ({
    value: id as Appearance,
    label: (
      <span className="inline-flex items-center gap-1.5">
        <Icon icon={id === "light" ? "gravity-ui:sun" : id === "dark" ? "gravity-ui:moon" : "gravity-ui:display"} className="size-3.5" />
        {t(`prefs.appearance${id === "light" ? "Light" : id === "dark" ? "Dark" : "System"}`)}
      </span>
    ),
    testId: `prefs-appearance-${id}`,
  }));

  return (
    <div className="flex min-h-full flex-col gap-5 p-4 sm:p-6" data-testid="azure-preferences-page">
      <header>
        <Link
          to="/workspaces"
          data-testid="prefs-back"
          onMouseEnter={() => setBackHover(true)}
          onMouseLeave={() => setBackHover(false)}
          className="inline-flex items-center gap-1.5 text-[13px] text-ink-2 no-underline hover:text-ink"
        >
          <Icon icon="gravity-ui:arrow-left" className={backHover ? "size-3.5 -translate-x-0.5" : "size-3.5"} />
          {t("prefs.backToWorkspaces")}
        </Link>
        <h1 className="mt-3 text-[24px] font-semibold tracking-tight text-ink">{t("prefs.title")}</h1>
        <p className="mt-1 text-[13px] text-ink-2">{t("prefs.desc")}</p>
      </header>

      <div className="grid gap-3 xl:grid-cols-2">
        <Section title={t("prefs.language")} description={t("prefs.languageDesc")}>
          <div className="flex flex-wrap gap-2" data-testid="prefs-language">
            {(["zh-CN", "en-US"] as const).map((id: Language) => (
              <AzureButton
                key={id}
                variant={language === id ? "accent" : "secondary"}
                onClick={() => setLanguage(id)}
              >
                {id === "zh-CN" ? "简体中文" : "English"}
              </AzureButton>
            ))}
          </div>
        </Section>

        <Section title={t("prefs.appearance")} description={t("prefs.appearanceDesc")}>
          <div className="flex flex-wrap items-center gap-3">
            <AzureSegmentedControl
              options={appearanceOptions}
              value={appearance}
              onChange={setAppearance}
              label={t("prefs.appearance")}
            />
            {appearance === "system" ? (
              <AzureChip tone="accent">
                {t("prefs.appearanceSystemHint", {
                  mode: resolvedAppearance === "dark" ? t("prefs.appearanceDark") : t("prefs.appearanceLight"),
                })}
              </AzureChip>
            ) : null}
          </div>
        </Section>
      </div>

      <Section title={t("prefs.theme")} description={t("prefs.themeDesc")}>
        <div className="grid gap-3 sm:grid-cols-2" data-testid="prefs-theme-section">
          {(["classic", "azure"] as const).map((id) => (
            <button
              key={id}
              type="button"
              data-testid={`prefs-theme-${id}`}
              aria-pressed={theme === id}
              onClick={() => setTheme(id)}
              className={`rounded-[10px] border p-3 text-left transition-[border-color,background-color] ${
                theme === id ? "border-accent bg-accent-tint" : "border-line hover:border-line-strong hover:bg-hover"
              }`}
            >
              <ThemePreview
                theme={id}
                label={t(id === "classic" ? "prefs.themeClassic" : "prefs.themeAzure")}
                description={t(id === "classic" ? "prefs.themeClassicDesc" : "prefs.themeAzureDesc")}
              />
            </button>
          ))}
        </div>
      </Section>

      <Section title={t("onboarding.replay")} description={t("onboarding.replayDesc")}>
        <div>
          <AzureButton
            variant="accent"
            onClick={() => onboarding?.restart()}
            disabled={!onboarding}
            data-testid="replay-onboarding"
          >
            {t("onboarding.replayAction")}
          </AzureButton>
        </div>
      </Section>
    </div>
  );
}

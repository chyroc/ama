/**
 * Onboarding tour: teaches new users the core concepts of this platform.
 *
 * # Why a floating panel in the bottom-right, not a modal
 *
 * The user may just be taking a look. A blocking modal covers the whole UI and forces them to
 * deal with the tour before they can see anything, and "just looking first" is a perfectly valid
 * need. So this is a **non-blocking floating panel**: no backdrop, the page stays clickable, and
 * it can be skipped at any time.
 *
 * z-40 is deliberate: AppShell's mobile drawer is z-50, and when the drawer is open it should
 * cover the tour, not have the tour float above the navigation and hide nav items.
 *
 * # Only shown to logged-in users, by mount position rather than a condition
 *
 * This component is mounted **inside** AuthGate (see main.tsx). When logged out, AuthGate renders
 * LandingPage and does not render children at all (the anonymous branch in AuthGate.tsx), so
 * "the tour does not appear when logged out" is a **structural guarantee**, not an `if (user)`.
 * The latter would break when someone refactors, with no error when it does.
 *
 * # State is stored in the database, not localStorage
 *
 * "The same person is never onboarded twice" is the core requirement of this feature, and
 * localStorage is gone when you switch browsers, auth_sessions is gone when you log out. So the
 * state is in the PostgreSQL user_onboarding table, keyed by the user's email (account-level,
 * cross-device); see server/core/schema_onboarding_pg.sql.
 *
 * # What about a new version
 *
 * When changing the tour content, bump ONBOARDING_VERSION in types/domain.ts by one. People whose
 * version is behind (**including those who finished the previous version**) get the tour again;
 * those who have seen the new version do not.
 *
 * # A failed read does not bother the user
 *
 * If fetching the state fails, **show nothing**, no error. The tour is an extra; when it breaks it
 * should not throw an error in the user's face that they can neither understand nor act on,
 * especially right after login, where it would make people think the whole platform is broken.
 * One line in the console is enough.
 */

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
} from "react";
import { Button, Card } from "@heroui/react";
import { Stepper } from "@/components/ui/Stepper";
import { Icon } from "@iconify/react";
import { api } from "@/data/api";
import type { MessageKey } from "@/prefs/messages";
import { useT } from "@/prefs/PrefsContext";
import { ONBOARDING_VERSION, shouldShowOnboarding } from "@/types/domain";

/**
 * The six steps of the tour.
 *
 * **Store translation keys, not text**: module-level constants are evaluated before PrefsContext
 * exists, so calling t() directly gets nothing (see guides/i18n.md). Translate at render time.
 *
 * The content is this project's **real** key concepts, not filler: step one, "a workspace is a
 * container", matches the core rule in DESIGN.md 1.1; step four, "one conversation is one run
 * record", matches 3.6.3.
 */
const STEPS: { icon: string; titleKey: MessageKey; bodyKey: MessageKey }[] = [
  {
    icon: "gravity-ui:layers",
    titleKey: "onboarding.step1Title",
    bodyKey: "onboarding.step1Body",
  },
  {
    icon: "gravity-ui:face-robot",
    titleKey: "onboarding.step2Title",
    bodyKey: "onboarding.step2Body",
  },
  {
    icon: "gravity-ui:puzzle",
    titleKey: "onboarding.step3Title",
    bodyKey: "onboarding.step3Body",
  },
  {
    icon: "gravity-ui:comments",
    titleKey: "onboarding.step4Title",
    bodyKey: "onboarding.step4Body",
  },
  {
    icon: "gravity-ui:plug-connection",
    titleKey: "onboarding.step5Title",
    bodyKey: "onboarding.step5Body",
  },
  {
    icon: "gravity-ui:chart-line",
    titleKey: "onboarding.step6Title",
    bodyKey: "onboarding.step6Body",
  },
];

interface OnboardingContextValue {
  /** Start over from step one. Used by "view the tour again" on the preferences page. */
  restart: () => void;
}

const OnboardingContext = createContext<OnboardingContextValue | null>(null);

/**
 * Get the tour's control handle.
 *
 * **Returns null instead of throwing** (the opposite of useAuth): the caller is the preferences
 * page, which cannot be reached when logged out and may be rendered alone in tests. Throwing would
 * let "view again", a minor feature, crash the whole page, and it is not worth that.
 */
export function useOnboarding(): OnboardingContextValue | null {
  return useContext(OnboardingContext);
}

export function OnboardingProvider({ children }: { children: React.ReactNode }) {
  /*
   * Open/closed is **an explicit state**, not derived from "state in the database + current step".
   *
   * We tried the derived version and it has an unavoidable bug: for someone who already finished
   * the tour, state is non-null, step is 0, and they have not clicked close. Those three are
   * exactly the same as "just clicked view again", so the derivation cannot tell them apart and
   * **it pops up on every login**, getting the one thing this feature must get right exactly
   * backwards.
   *
   * An explicit state has no such ambiguity: only two places open it: fetching the state and
   * finding the tour is due, and the user clicking "view again".
   */
  const [open, setOpen] = useState(false);
  const [step, setStep] = useState(0);

  useEffect(() => {
    let alive = true;
    api
      .getOnboarding()
      .then((s) => {
        if (!alive) return;
        // The only place that opens it automatically. For someone who already saw the current
        // version, shouldShowOnboarding is false, so nothing happens; this is where "the same
        // person is never onboarded twice" lands on the frontend.
        if (shouldShowOnboarding(s)) setOpen(true);
      })
      .catch((e: unknown) => {
        // The catch is required: a bare .then() rejection becomes an unhandled rejection, which
        // is a single line in the console and easy to mistake for another problem. See
        // guides/frontend.md.
        console.warn("拉取新手引导状态失败，本次不展示引导", e);
      });
    return () => {
      alive = false;
    };
  }, []);

  // Write "finished" or "skipped" back to the backend.
  //
  // **Close the panel first, then send the request**, without waiting for it: if the user clicks
  // skip it should disappear at once. Waiting on the request would leave the panel hanging around
  // on a slow network, making people think the button did nothing and click it several times.
  const mark = useCallback((action: "complete" | "skip") => {
    setOpen(false);
    api.markOnboarding(action, ONBOARDING_VERSION).catch((e: unknown) => {
      // A failed write is only logged: this panel is already closed (the user's intent was
      // respected), and the only cost is seeing the tour again on next login. Not worth an error
      // popup.
      console.warn("保存新手引导状态失败，下次登录可能再次引导", e);
    });
  }, []);

  // View again: go back to step one and open.
  //
  // **It must work for people who already finished**: they are exactly the ones most likely to
  // use this entry. So the database state is not checked here; if the user clicked, they want it.
  const restart = useCallback(() => {
    setStep(0);
    setOpen(true);
  }, []);

  const value = useMemo(() => ({ restart }), [restart]);

  return (
    <OnboardingContext.Provider value={value}>
      {children}
      {open ? (
        <OnboardingCard
          step={step}
          onStep={setStep}
          onSkip={() => mark("skip")}
          onDone={() => mark("complete")}
        />
      ) : null}
    </OnboardingContext.Provider>
  );
}

function OnboardingCard({
  step,
  onStep,
  onSkip,
  onDone,
}: {
  step: number;
  onStep: (n: number) => void;
  onSkip: () => void;
  onDone: () => void;
}) {
  const t = useT();
  const current = STEPS[step];
  const isLast = step === STEPS.length - 1;

  return (
    // Fixed to the bottom-right. The width uses min() to handle narrow screens: a hard-coded
    // 24rem overflows the right edge on phones.
    //
    // ⚠️ This is the "has an offset, but not enough" kind; writing `bottom-6` does not make it
    // safe: 24px is less than the Home Indicator's 34px, so **the card's bottom row, which is
    // exactly the button row** (skip / previous / next), sits under the system gesture bar, and
    // iOS swallows the first tap. The symptom is "the button is visible but cannot be tapped",
    // harder to diagnose than being fully covered. Same on the right: in landscape the notch is on
    // the side, and `right-6`'s 24px cannot cover 59px.
    //
    // The `calc(100vw-3rem)` width must also subtract the left and right safe areas; otherwise in
    // landscape the card is wider than the usable width and its right side is cut off by the
    // notch.
    <div
      className="fixed z-40"
      style={{
        bottom: "calc(1.5rem + var(--safe-bottom))",
        right: "calc(1.5rem + var(--safe-right))",
        width: "min(24rem, calc(100vw - 3rem - var(--safe-left) - var(--safe-right)))",
      }}
      role="region"
      aria-label={t("onboarding.title")}
      data-testid="onboarding-card"
    >
      {/* Card already has shadow-surface; do not add shadow-overlay on top (HeroUI rule: do not stack shadows) */}
      <Card>
        <Card.Content className="flex flex-col gap-4 pt-5">
          {/*
            The progress rail only holds dots and connecting lines; titles do not go into the
            Stepper. Squeezing titles into a six-step horizontal stepper would push each
            character into its own line. onStepChange makes the dots clickable (to look back at a
            step).
          */}
          <Stepper currentStep={step} onStepChange={onStep} size="sm">
            {STEPS.map((s, i) => (
              <Stepper.Step key={s.titleKey}>
                <Stepper.Indicator />
                <Stepper.Separator />
                {/* The dot needs an accessible name; otherwise screen readers only read the number */}
                <span className="sr-only">{t(s.titleKey)}</span>
                <span className="sr-only">{i + 1}</span>
              </Stepper.Step>
            ))}
          </Stepper>

          <div className="flex flex-col gap-2">
            <div className="flex items-center gap-2">
              {/* Icon uses muted: as heavy as the title, it would look like emphasizing a decoration */}
              <Icon icon={current.icon} className="size-4 shrink-0 text-muted" />
              <h2 className="text-base font-medium" data-testid="onboarding-step-title">
                {t(current.titleKey)}
              </h2>
            </div>
            <p className="text-sm text-muted">{t(current.bodyKey)}</p>
          </div>

          <div className="flex items-center justify-between gap-3">
            {/* Skip is an escape action; ghost makes it clearly secondary to the main CTA */}
            <Button variant="ghost" onPress={onSkip} data-testid="onboarding-skip">
              {t("onboarding.skip")}
            </Button>
            <div className="flex items-center gap-2">
              {/* Step count uses tabular-nums: fixed-width digits, so this line does not jump left and right when paging */}
              <span className="text-xs text-muted tabular-nums">
                {t("onboarding.progress", { current: step + 1, total: STEPS.length })}
              </span>
              {step > 0 ? (
                <Button variant="outline" onPress={() => onStep(step - 1)} data-testid="onboarding-prev">
                  {t("onboarding.prev")}
                </Button>
              ) : null}
              <Button
                onPress={() => (isLast ? onDone() : onStep(step + 1))}
                data-testid={isLast ? "onboarding-done" : "onboarding-next"}
              >
                {isLast ? t("onboarding.done") : t("onboarding.next")}
              </Button>
            </div>
          </div>
        </Card.Content>
      </Card>
    </div>
  );
}

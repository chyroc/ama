import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { RouterProvider } from "react-router-dom";
import { router } from "@/router";
import { registerIcons } from "@/lib/icons";
import { installConsoleCapture } from "@/lib/feedback";
import { AuthGate } from "@/app/AuthGate";
import { OnboardingProvider } from "@/app/OnboardingTour";
import { PrefsProvider } from "@/prefs/PrefsContext";
import { applyAppearance, applyLanguage, loadPrefs } from "@/prefs/storage";
import "@/index.css";

// Icons must be registered before rendering: after the first frame, @iconify/react has already sent
// network requests because it found them "not available locally".
registerIcons();

// Start capturing the first line of console.error, to attach when the **user chooses** to send
// feedback (not attached at level one).
//
// It must be installed before rendering: installing it late misses errors during startup, and those
// are exactly the hardest kind to debug (the user sees a white screen, and that console line is the
// only clue).
//
// ⚠️ It only wraps and forwards unchanged; **it does not change any behavior of console.error**.
// The buffer is memory only and keeps only the first line of each entry (the full stack contains
// source paths and variable values), and it is only reported when the user clicks "report this
// problem". See lib/feedback.ts.
installConsoleCapture();

// Before mounting, apply appearance and language to <html>, so the first frame does not flash light
// before switching to dark
const initial = loadPrefs();
applyAppearance(initial.appearance);
applyLanguage(initial.language);

// AuthGate wraps **outside** RouterProvider: so pages outside WorkspaceGate, such as /workspaces
// and /preferences, also require login, with no "look first, log in later" gap.
//
// It is inside PrefsProvider because the login page text goes through t().
//
// OnboardingProvider sits between AuthGate and RouterProvider, and both sides are deliberate:
//
//   * **inside** AuthGate: when not logged in, AuthGate renders LandingPage and never renders
//     children, so "no onboarding when logged out" is guaranteed by structure, not by any condition.
//   * **outside** RouterProvider: the onboarding overlay does not belong to any page and should not
//     remount on route changes (remounting would send it back to step one). The cost is that it
//     cannot use <Link> inside; see that file's notes.
createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <PrefsProvider>
      <AuthGate>
        <OnboardingProvider>
          <RouterProvider router={router} />
        </OnboardingProvider>
      </AuthGate>
    </PrefsProvider>
  </StrictMode>,
);

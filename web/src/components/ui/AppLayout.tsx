import { Suspense, useEffect, useState, type ReactNode } from "react";
import { Outlet, useLocation } from "react-router-dom";
import { Icon } from "@iconify/react";
import { useWorkspace } from "@/app/WorkspaceGate";
import { AppShellScope, ListSkeleton } from "@/components/Page";
import { ThemedComponent } from "@/components/ui/ThemedComponent";
import { AzureNavigation, type AzureNavItem } from "@/components/azure/layout/Navigation";
import { useT } from "@/prefs/PrefsContext";

interface AppLayoutProps {
  classic: ReactNode;
  children?: ReactNode;
}

function AzureAppLayout({ children }: { children?: ReactNode }) {
  const { workspace } = useWorkspace();
  const t = useT();
  const [mobileOpen, setMobileOpen] = useState(false);
  const location = useLocation();
  useEffect(() => setMobileOpen(false), [location.key]);
  const base = `/w/${workspace.key}`;
  const navItems: AzureNavItem[] = [
    { to: base, label: t("nav.overview"), icon: "gravity-ui:chart-column", end: true },
    { to: `${base}/agents`, label: "Agents", icon: "gravity-ui:face-robot" },
    { to: `${base}/chat`, label: t("nav.chat"), icon: "gravity-ui:comments" },
    { to: `${base}/skills`, label: t("nav.skills"), icon: "gravity-ui:puzzle" },
    { to: `${base}/schedules`, label: t("nav.schedules"), icon: "gravity-ui:clock" },
    { to: `${base}/runs`, label: t("nav.runs"), icon: "gravity-ui:list-ul" },
    { to: `${base}/usage`, label: t("nav.usage"), icon: "gravity-ui:chart-line" },
    { to: `${base}/issues`, label: t("nav.issues"), icon: "gravity-ui:bug" },
    { to: `${base}/feishu`, label: t("nav.feishu"), icon: "gravity-ui:comment" },
    { to: `${base}/weixin`, label: t("nav.weixin"), icon: "gravity-ui:smartphone" },
    { to: `${base}/settings`, label: t("nav.workspaceSettings"), icon: "gravity-ui:gear" },
  ];

  return (
    <div className="flex h-dvh bg-canvas p-2.5 text-ink lg:pl-0" onClick={() => setMobileOpen(false)}>
      <div className="hidden lg:flex"><AzureNavigation items={navItems} /></div>
      {mobileOpen ? (
        <div className="fixed inset-0 z-50 bg-surface p-2.5 lg:hidden" data-testid="mobile-nav-drawer">
          <button
            type="button"
            aria-label={t("nav.closeNav")}
            onClick={() => setMobileOpen(false)}
            className="absolute right-4 top-4 z-50 flex size-8 items-center justify-center rounded-[8px] bg-surface text-ink-2 shadow-btn"
          >
            <Icon icon="gravity-ui:xmark" className="size-4" />
          </button>
          <AzureNavigation items={navItems} />
        </div>
      ) : null}
      <div className="flex min-w-0 flex-1 flex-col gap-2.5">
        <header
          className="flex shrink-0 items-center gap-2 border-b border-line px-2 pb-2 lg:hidden"
          data-testid="mobile-topbar"
        >
          <button
            type="button"
            aria-label={t("nav.openNav")}
            onClick={(event) => {
              event.stopPropagation();
              setMobileOpen(true);
            }}
            data-testid="mobile-nav-open"
            className="flex size-8 items-center justify-center rounded-[8px] text-ink-2 hover:bg-hover"
          >
            <Icon icon="gravity-ui:bars" className="size-4" />
          </button>
          <span className="truncate text-[13px] font-medium text-ink">{workspace.name}</span>
        </header>
        <main className="min-h-0 flex-1 overflow-y-auto overscroll-contain" data-testid="app-scroll">
          <Suspense fallback={<ListSkeleton rows={3} />}>{children ?? <Outlet />}</Suspense>
        </main>
      </div>
    </div>
  );
}

/** Full application layout contract. Classic keeps the existing AppShell intact. */
export function AppLayout({ classic, children }: AppLayoutProps) {
  return (
    <AppShellScope>
      <ThemedComponent
        classic={() => classic}
        azure={() => <AzureAppLayout>{children}</AzureAppLayout>}
      />
    </AppShellScope>
  );
}

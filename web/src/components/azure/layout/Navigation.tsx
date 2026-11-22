import { useEffect, useRef, useState, type CSSProperties } from "react";
import { Icon } from "@iconify/react";
import { NavLink, useNavigate, Link } from "react-router-dom";
import { useWorkspace } from "@/app/WorkspaceGate";
import { preferencesPath } from "@/app/preferencesLink";
import { useT } from "@/prefs/PrefsContext";
import { api } from "@/data/api";
import type { Workspace } from "@/types/domain";

export interface AzureNavItem {
  to: string;
  label: string;
  icon: string;
  end?: boolean;
}

function WorkspaceMenu({ onClose }: { onClose: () => void }) {
  const { workspace } = useWorkspace();
  const navigate = useNavigate();
  const t = useT();
  const [workspaces, setWorkspaces] = useState<Workspace[]>([]);
  const menuRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    void api.listWorkspaces().then(setWorkspaces).catch(() => setWorkspaces([]));
    const close = (event: PointerEvent) => {
      if (!menuRef.current?.contains(event.target as Node)) onClose();
    };
    document.addEventListener("pointerdown", close);
    return () => document.removeEventListener("pointerdown", close);
  }, [onClose]);

  return (
    <div
      ref={menuRef}
      data-workspace-menu
      className="absolute left-2 top-11 z-50 w-64 rounded-[14px] bg-surface p-1.5 shadow-overlay"
      style={{ animation: "pop-in 180ms cubic-bezier(0.23,1,0.32,1) both" }}
    >
      {workspaces.map((workspaceItem) => (
        <button
          key={workspaceItem.key}
          type="button"
          onClick={() => {
            navigate(`/w/${workspaceItem.key}/agents`);
            onClose();
          }}
          className="relative z-10 flex h-9 w-full items-center gap-1.5 rounded-[8px] px-2 text-left hover:bg-hover-2"
        >
          <span className="flex size-6 shrink-0 items-center justify-center rounded-[7px] bg-ink text-[11px] font-semibold text-surface">
            {workspaceItem.name.slice(0, 1)}
          </span>
          <span className="min-w-0 flex-1 truncate text-[13.5px] font-medium text-ink">{workspaceItem.name}</span>
          {workspaceItem.key === workspace.key ? <Icon icon="gravity-ui:check" className="size-4 text-ink" /> : null}
        </button>
      ))}
      <div className="my-1 h-px bg-line" />
      <button
        type="button"
        onClick={() => {
          navigate("/workspaces");
          onClose();
        }}
        className="flex h-9 w-full items-center gap-1.5 rounded-[8px] px-2 text-left hover:bg-hover-2"
      >
        <Icon icon="gravity-ui:plus" className="size-4 text-ink-2" />
        <span className="text-[13.5px] text-ink">{t("nav.manageWorkspaces")}</span>
      </button>
    </div>
  );
}

/** AMA navigation contract's full Azure implementation, adapted from SidebarNav. */
export function AzureNavigation({ items }: { items: readonly AzureNavItem[] }) {
  const { workspace } = useWorkspace();
  const t = useT();
  const [collapsed, setCollapsed] = useState(false);
  const [workspaceOpen, setWorkspaceOpen] = useState(false);
  const [query, setQuery] = useState("");
  const visibleItems = items.filter((item) => item.label.toLowerCase().includes(query.trim().toLowerCase()));

  return (
    <aside
      data-sidebar-collapsed={collapsed}
      aria-label="Workspace navigation"
      className="relative flex h-full shrink-0 overflow-hidden transition-[width]"
      style={{
        width: collapsed ? 52 : 224,
        transitionDuration: "280ms",
        transitionTimingFunction: "cubic-bezier(0.16, 1, 0.3, 1)",
        "--sidebar-copy-duration": "180ms",
        "--sidebar-copy-offset": "8px",
        "--sidebar-easing": "cubic-bezier(0.16, 1, 0.3, 1)",
      } as CSSProperties}
    >
      <div className="flex min-h-0 w-[224px] shrink-0 flex-col px-2">
        <div className="relative mb-2.5 h-10 shrink-0">
          <button
            type="button"
            data-workspace-trigger
            data-testid="workspace-switcher"
            aria-expanded={workspaceOpen}
            aria-hidden={collapsed}
            tabIndex={collapsed ? -1 : 0}
            onClick={() => setWorkspaceOpen((current) => !current)}
            className="absolute left-0 top-1 flex h-8 w-[174px] items-center rounded-[8px] px-2 text-left hover:bg-hover-2"
          >
            <span className="flex size-5 shrink-0 items-center justify-center rounded-[5px] bg-ink text-[11px] font-semibold text-surface">
              {workspace.name.slice(0, 1)}
            </span>
            <span className="sidebar-copy ml-1.5 min-w-0 flex-1 truncate text-[14px] font-medium text-ink-2">
              {workspace.name}
            </span>
            <Icon icon="gravity-ui:chevron-down" className="sidebar-copy size-4 shrink-0 text-ink-3" />
          </button>
          {workspaceOpen ? <WorkspaceMenu onClose={() => setWorkspaceOpen(false)} /> : null}
          <button
            type="button"
            aria-label={collapsed ? t("nav.expandSidebar") : t("nav.collapseSidebar")}
            onClick={() => setCollapsed((current) => !current)}
            data-testid="app-nav-collapse"
            className="absolute right-1 top-1 flex size-8 items-center justify-center rounded-[8px] text-ink-3 hover:bg-hover-2 hover:text-ink"
          >
            <Icon icon={collapsed ? "gravity-ui:arrow-right-from-line" : "gravity-ui:arrow-left"} className="size-4" />
          </button>
        </div>

        <Link
          to={`/w/${workspace.key}/agents/new`}
          className="sidebar-copy mx-1 mb-2 flex h-8 items-center gap-1.5 rounded-[8px] px-2 text-left hover:bg-hover-2"
        >
          <Icon icon="gravity-ui:plus" className="size-4 text-ink-2" />
          <span className="text-[13px] font-medium text-ink">{t("agent.new")}</span>
        </Link>

        <div className="sidebar-copy relative mx-1 mb-1 h-8">
          <label className="flex h-8 items-center gap-1.5 rounded-[8px] bg-field px-2 text-ink-3 shadow-hairline">
            <Icon icon="gravity-ui:magnifier" className="size-3.5 shrink-0" />
            <input
              value={query}
              onChange={(event) => setQuery(event.target.value)}
              placeholder={t("nav.filterNav")}
              aria-label={t("nav.filterNav")}
              className="min-w-0 flex-1 bg-transparent text-[13px] font-medium text-ink outline-none placeholder:text-ink-3"
            />
          </label>
        </div>

        <nav className="mt-1 min-h-0 flex-1 overflow-y-auto">
          {visibleItems.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              end={item.end}
              data-row
              className={({ isActive }) =>
                `sidebar-row relative z-10 mx-1 flex h-8 items-center rounded-[8px] px-2 text-left ${
                  isActive ? "bg-hover-2" : "hover:bg-hover"
                }`
              }
            >
              {({ isActive }) => (
                <>
                  <Icon icon={item.icon} className={`size-4 shrink-0 ${isActive ? "text-ink" : "text-ink-2"}`} />
                  <span className={`sidebar-copy ml-1.5 min-w-0 flex-1 truncate text-[13px] font-medium ${isActive ? "text-ink" : "text-ink-2"}`}>
                    {item.label}
                  </span>
                </>
              )}
            </NavLink>
          ))}
        </nav>

        <div className="sidebar-copy mt-3 border-t border-line pt-3">
          <NavLink
            to={preferencesPath({ workspaceKey: workspace.key })}
            className="flex h-8 w-full items-center justify-center gap-1.5 rounded-[8px] bg-hover-2 text-[12.5px] font-medium text-ink hover:bg-line-strong"
          >
            <Icon icon="gravity-ui:gear" className="size-3.5" />
            {t("nav.globalSettings")}
          </NavLink>
        </div>
      </div>
    </aside>
  );
}

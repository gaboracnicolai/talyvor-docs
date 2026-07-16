import { useEffect, useState } from "react";
import { Outlet, useNavigate, useParams } from "react-router-dom";
import { Sidebar } from "~/components/layout/Sidebar";
import { Header } from "~/components/layout/Header";
import { AskAI } from "~/components/AskAI";
import { SearchModal } from "~/components/SearchModal";
import { OfflineIndicator } from "~/components/OfflineIndicator";
import { useSpace, workspaceID as currentWorkspaceID } from "~/hooks/useSpaces";
import { paths } from "./paths";
import type { Page, Space } from "~/api/types";

// Layout is the app chrome (sidebar + header + offline indicator + Cmd+K search) wrapped
// around an <Outlet/> for the active route. It replaces App.tsx's hand-rolled state machine:
// navigation is now URL-driven, so the sidebar's callbacks navigate() instead of setState,
// and the active-item highlight + breadcrumbs read from useParams().
//
// The leaf components (Sidebar, Header, SearchModal) keep their existing callback props —
// this wrapper just supplies navigate-backed implementations. Minimal churn, and a link is
// a real <a href> a user can middle-click / copy, not an onClick.
export function Layout() {
  const navigate = useNavigate();
  const { spaceID = null, pageID = null } = useParams();
  const workspaceID = currentWorkspaceID();
  const [searchOpen, setSearchOpen] = useState(false);

  // Cmd/Ctrl+K toggles global search. Bound at the chrome level so it works regardless of
  // which route is mounted in the Outlet.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const mod = e.metaKey || e.ctrlKey;
      if (mod && (e.key === "k" || e.key === "K")) {
        e.preventDefault();
        setSearchOpen((v) => !v);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  // Breadcrumbs need the space NAME, which the URL doesn't carry — resolve it. Deduped by
  // react-query with the route's own useSpace call, so it is not an extra network request.
  const activeSpace = useSpace(spaceID);
  const breadcrumbs = useBreadcrumbs(spaceID, activeSpace.data, navigate);

  return (
    <div className="flex h-screen w-full flex-col bg-bg text-text">
      <OfflineIndicator />
      <div className="flex flex-1 overflow-hidden">
        <Sidebar
          onHome={() => navigate(paths.home())}
          onOpenSpace={(sp: Space) => navigate(paths.space(sp.id))}
          onOpenPage={(sp: Space, pg: Page) => navigate(paths.page(sp.id, pg.id))}
          onOpenAnalytics={() => navigate(paths.analytics())}
          onOpenStale={() => navigate(paths.stale())}
          onOpenTemplates={() => navigate(paths.templates())}
          onOpenApprovals={() => navigate(paths.approvals())}
          onOpenDomains={() => navigate(paths.domains())}
          workspaceID={workspaceID}
          activeSpaceID={spaceID}
          activePageID={pageID}
        />
        <div className="flex flex-1 flex-col overflow-hidden">
          <Header breadcrumbs={breadcrumbs} onOpenSearch={() => setSearchOpen(true)} />
          <AskAI workspaceId={workspaceID} />
          <SearchModal
            workspaceId={workspaceID}
            open={searchOpen}
            onClose={() => setSearchOpen(false)}
            onOpenPage={(r) => {
              setSearchOpen(false);
              // Prefer a built path from ids; fall back to the result's own url (already a
              // /spaces/../pages/.. string) when the space id wasn't resolved.
              navigate(r.spaceID ? paths.page(r.spaceID, r.pageID) : r.url);
            }}
          />
          <Outlet />
        </div>
      </div>
    </div>
  );
}

function useBreadcrumbs(
  spaceID: string | null,
  space: Space | undefined,
  navigate: ReturnType<typeof useNavigate>,
): { label: string; onClick?: () => void }[] {
  const { pathname } = window.location;
  if (pathname === paths.analytics()) return [{ label: "Analytics" }];
  if (pathname === paths.stale()) return [{ label: "Needs review" }];
  if (pathname === paths.templates()) return [{ label: "Templates" }];
  if (pathname === paths.approvals()) return [{ label: "Approvals" }];
  if (pathname === paths.domains()) return [{ label: "Custom domains" }];
  if (!spaceID) return [];
  const spaceLabel = space?.name || "Space";
  const crumbs = [{ label: spaceLabel, onClick: () => navigate(paths.space(spaceID)) }];
  if (pathname.includes("/pages/")) crumbs.push({ label: "Page", onClick: () => undefined });
  return crumbs;
}

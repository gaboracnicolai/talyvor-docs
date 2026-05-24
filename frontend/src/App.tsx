import { useEffect, useState } from "react";
import { Sidebar } from "./components/layout/Sidebar";
import { Header } from "./components/layout/Header";
import { AskAI } from "./components/AskAI";
import { SearchModal } from "./components/SearchModal";
import { HomePage } from "./pages/Home";
import { SpaceViewPage } from "./pages/SpaceView";
import { PageViewPage } from "./pages/PageView";
import { AnalyticsPage } from "./pages/Analytics";
import { StalePagesPage } from "./pages/StalePages";
import { SharedPage } from "./pages/SharedPage";
import type { Page, Space } from "./api/types";

// Routes are modelled as a discriminated union held in App state.
// Phase 2 doesn't ship a URL-bound router — the App is single-page
// admin chrome — but the structure here mirrors what a TanStack
// Router migration will look like in Phase 3.
type Route =
  | { kind: "home" }
  | { kind: "space"; space: Space }
  | { kind: "page"; space: Space; pageID: string }
  | { kind: "analytics" }
  | { kind: "stale" };

export function App() {
  // /s/:token is handled before any other routing — the public
  // viewer renders without the sidebar / header chrome.
  const sharedMatch = typeof window !== "undefined"
    ? window.location.pathname.match(/^\/s\/([A-Za-z0-9]+)$/)
    : null;
  if (sharedMatch) {
    return <SharedPage token={sharedMatch[1]} />;
  }

  const [route, setRoute] = useState<Route>({ kind: "home" });
  const [searchOpen, setSearchOpen] = useState(false);

  const goHome = () => setRoute({ kind: "home" });
  const goSpace = (space: Space) => setRoute({ kind: "space", space });
  const goPage = (space: Space, page: Page) =>
    setRoute({ kind: "page", space, pageID: page.id });
  const goAnalytics = () => setRoute({ kind: "analytics" });
  const goStale = () => setRoute({ kind: "stale" });

  // Cmd/Ctrl+K toggles the global search modal. Bound at the App
  // level so the shortcut works regardless of focus.
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

  const breadcrumbs = (() => {
    if (route.kind === "home") return [];
    if (route.kind === "analytics") return [{ label: "Analytics" }];
    if (route.kind === "stale") return [{ label: "Needs review" }];
    if (route.kind === "space") {
      return [{ label: route.space.name, onClick: () => goSpace(route.space) }];
    }
    return [
      { label: route.space.name, onClick: () => goSpace(route.space) },
      { label: "Page" }, // resolved title lives in PageViewPage
    ];
  })();

  const workspaceID =
    route.kind === "page" || route.kind === "space"
      ? route.space.workspace_id || "default"
      : "default";

  return (
    <div className="flex h-screen w-full bg-bg text-text">
      <Sidebar
        onHome={goHome}
        onOpenSpace={goSpace}
        onOpenPage={goPage}
        onOpenAnalytics={goAnalytics}
        onOpenStale={goStale}
        workspaceID={workspaceID}
        activeSpaceID={
          route.kind === "space" || route.kind === "page" ? route.space.id : null
        }
        activePageID={route.kind === "page" ? route.pageID : null}
      />
      <div className="flex flex-1 flex-col overflow-hidden">
        <Header breadcrumbs={breadcrumbs} onOpenSearch={() => setSearchOpen(true)} />
        <AskAI workspaceId={workspaceID} />
        <SearchModal
          workspaceId={workspaceID}
          open={searchOpen}
          onClose={() => setSearchOpen(false)}
          onOpenPage={(r) => {
            // SearchModal hands back the chosen page; if we know the
            // space ID we can route fully. Otherwise we use a minimal
            // Space stub — PageView reads the real record from its
            // own cache.
            const stubSpace: Space = {
              id: r.spaceID ?? "",
              workspace_id: workspaceID,
              name: r.spaceName,
            } as Space;
            setRoute({ kind: "page", space: stubSpace, pageID: r.pageID });
          }}
        />
        {route.kind === "home" ? (
          <main className="flex-1 overflow-y-auto">
            <HomePage onOpenSpace={goSpace} />
          </main>
        ) : route.kind === "space" ? (
          <main className="flex-1 overflow-y-auto">
            <SpaceViewPage space={route.space} onOpenPage={(p) => goPage(route.space, p)} />
          </main>
        ) : route.kind === "analytics" ? (
          <main className="flex-1 overflow-y-auto">
            <AnalyticsPage workspaceID={workspaceID} />
          </main>
        ) : route.kind === "stale" ? (
          <main className="flex-1 overflow-y-auto">
            <StalePagesPage
              workspaceID={workspaceID}
              onOpenPage={(spaceID, pageID) =>
                setRoute({
                  kind: "page",
                  space: { id: spaceID, workspace_id: workspaceID, name: "" } as Space,
                  pageID,
                })
              }
            />
          </main>
        ) : (
          <PageViewPage space={route.space} pageID={route.pageID} />
        )}
      </div>
    </div>
  );
}

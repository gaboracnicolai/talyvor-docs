import { useState } from "react";
import { Sidebar } from "./components/layout/Sidebar";
import { Header } from "./components/layout/Header";
import { AskAI } from "./components/AskAI";
import { HomePage } from "./pages/Home";
import { SpaceViewPage } from "./pages/SpaceView";
import { PageViewPage } from "./pages/PageView";
import type { Page, Space } from "./api/types";

// Routes are modelled as a discriminated union held in App state.
// Phase 2 doesn't ship a URL-bound router — the App is single-page
// admin chrome — but the structure here mirrors what a TanStack
// Router migration will look like in Phase 3.
type Route =
  | { kind: "home" }
  | { kind: "space"; space: Space }
  | { kind: "page"; space: Space; pageID: string };

export function App() {
  const [route, setRoute] = useState<Route>({ kind: "home" });
  const [searchQuery, setSearchQuery] = useState("");

  const goHome = () => setRoute({ kind: "home" });
  const goSpace = (space: Space) => setRoute({ kind: "space", space });
  const goPage = (space: Space, page: Page) =>
    setRoute({ kind: "page", space, pageID: page.id });

  const breadcrumbs = (() => {
    if (route.kind === "home") return [];
    if (route.kind === "space") {
      return [{ label: route.space.name, onClick: () => goSpace(route.space) }];
    }
    return [
      { label: route.space.name, onClick: () => goSpace(route.space) },
      { label: "Page" }, // resolved title lives in PageViewPage
    ];
  })();

  return (
    <div className="flex h-screen w-full bg-bg text-text">
      <Sidebar
        onHome={goHome}
        onOpenSpace={goSpace}
        onOpenPage={goPage}
        activeSpaceID={
          route.kind === "space" || route.kind === "page" ? route.space.id : null
        }
        activePageID={route.kind === "page" ? route.pageID : null}
      />
      <div className="flex flex-1 flex-col overflow-hidden">
        <Header breadcrumbs={breadcrumbs} onSearch={setSearchQuery} />
        <AskAI
          workspaceId={
            route.kind === "home"
              ? "default"
              : route.space.workspace_id || "default"
          }
        />
        {route.kind === "home" ? (
          <main className="flex-1 overflow-y-auto">
            <HomePage
              searchQuery={searchQuery}
              onOpenSpace={goSpace}
              onOpenPageById={(spaceID, pageID) => {
                // The Home page searches by workspace and only knows
                // the space ID; we synthesise a minimal Space stub so
                // the route can render until SpaceView reads the real
                // record from the cache.
                setRoute({
                  kind: "page",
                  space: { id: spaceID } as Space,
                  pageID,
                });
              }}
            />
          </main>
        ) : route.kind === "space" ? (
          <main className="flex-1 overflow-y-auto">
            <SpaceViewPage space={route.space} onOpenPage={(p) => goPage(route.space, p)} />
          </main>
        ) : (
          <PageViewPage space={route.space} pageID={route.pageID} />
        )}
      </div>
    </div>
  );
}

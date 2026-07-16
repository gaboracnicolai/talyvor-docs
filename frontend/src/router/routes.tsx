import type { RouteObject } from "react-router-dom";
import { useNavigate, useParams } from "react-router-dom";
import { HomePage } from "~/pages/Home";
import { SpaceViewPage } from "~/pages/SpaceView";
import { PageViewPage } from "~/pages/PageView";
import { AnalyticsPage } from "~/pages/Analytics";
import { StalePagesPage } from "~/pages/StalePages";
import { TemplateGalleryPage } from "~/pages/TemplateGallery";
import { ApprovalInboxPage } from "~/pages/ApprovalInbox";
import { DomainSettingsPage } from "~/pages/DomainSettings";
import { SharedPage } from "~/pages/SharedPage";
import { usePage } from "~/hooks/usePage";
import { useSpace, workspaceID as currentWorkspaceID } from "~/hooks/useSpaces";
import type { Space } from "~/api/types";
import { Layout } from "./Layout";
import { paths } from "./paths";
import { resourceState } from "./guard";
import { Guard, NotFoundView } from "./views";

// The route table. Exported as plain RouteObject[] so the browser router (App.tsx) and the
// memory router (tests) drive the SAME tree — a route that works in a test works in the app.
//
// The wrapper components below are the adapter layer: they read ids from useParams(), resolve
// objects via the existing query hooks, run the resource guard, and hand the existing page
// components their callback props (navigate-backed). The leaf pages are unchanged.

export const routes: RouteObject[] = [
  {
    path: "/",
    element: <Layout />,
    children: [
      { index: true, element: <HomeRoute /> },
      { path: paths.patterns.space.slice(1), element: <SpaceRoute /> },
      { path: paths.patterns.page.slice(1), element: <PageRoute /> },
      { path: "analytics", element: <AnalyticsRoute /> },
      { path: "needs-review", element: <StaleRoute /> },
      { path: "templates", element: <TemplatesRoute /> },
      { path: "approvals", element: <ApprovalsRoute /> },
      { path: "domains", element: <DomainsRoute /> },
      // In-chrome catch-all: an unknown in-app URL shows Not found WITH the sidebar, so the
      // user isn't stranded. Same no-oracle copy as a guarded miss.
      { path: "*", element: <NotFoundView /> },
    ],
  },
  // Public share viewer — no app chrome, its own top-level path (matches the pre-router
  // /s/:token behaviour and nginx's SPA fallback).
  { path: paths.patterns.shared, element: <SharedRoute /> },
];

function HomeRoute() {
  const navigate = useNavigate();
  return <HomePage onOpenSpace={(sp) => navigate(paths.space(sp.id))} />;
}

function SpaceRoute() {
  const navigate = useNavigate();
  const { spaceID = "" } = useParams();
  const q = useSpace(spaceID);
  return (
    <Guard state={resourceState(q)}>
      {q.data ? (
        <main className="flex-1 overflow-y-auto">
          <SpaceViewPage
            space={q.data}
            onOpenPage={(p) => navigate(paths.page(spaceID, p.id))}
          />
        </main>
      ) : null}
    </Guard>
  );
}

function PageRoute() {
  const { spaceID = "", pageID = "" } = useParams();
  // The addressed resource is the PAGE — guard on it. The backend gates the page fetch by
  // its space's privacy, so a page you may not see 404s here exactly as a nonexistent one.
  const pageQ = usePage(spaceID, pageID);
  // The space is resolved only for display (name/private). Both hooks run unconditionally
  // (rules of hooks); react-query dedupes the page query with PageView's own call.
  const spaceQ = useSpace(spaceID);
  const state = resourceState(pageQ);
  if (state !== "ready") return <Guard state={state}>{null}</Guard>;

  const space: Space =
    spaceQ.data ??
    ({ id: spaceID, name: "", workspace_id: pageQ.data?.workspace_id ?? "" } as Space);
  return <PageViewPage space={space} pageID={pageID} />;
}

function AnalyticsRoute() {
  return (
    <main className="flex-1 overflow-y-auto">
      <AnalyticsPage workspaceID={currentWorkspaceID()} />
    </main>
  );
}

function StaleRoute() {
  const navigate = useNavigate();
  return (
    <main className="flex-1 overflow-y-auto">
      <StalePagesPage
        workspaceID={currentWorkspaceID()}
        onOpenPage={(spaceID, pageID) => navigate(paths.page(spaceID, pageID))}
      />
    </main>
  );
}

function TemplatesRoute() {
  const navigate = useNavigate();
  return (
    <main className="flex-1 overflow-y-auto">
      <TemplateGalleryPage
        workspaceID={currentWorkspaceID()}
        onCreated={(spaceID, pageID) => navigate(paths.page(spaceID, pageID))}
      />
    </main>
  );
}

function ApprovalsRoute() {
  const navigate = useNavigate();
  return (
    <main className="flex-1 overflow-y-auto">
      <ApprovalInboxPage
        workspaceID={currentWorkspaceID()}
        onOpenPage={(spaceID, pageID) => navigate(paths.page(spaceID, pageID))}
      />
    </main>
  );
}

function DomainsRoute() {
  return (
    <main className="flex-1 overflow-y-auto">
      <DomainSettingsPage workspaceID={currentWorkspaceID()} />
    </main>
  );
}

function SharedRoute() {
  const { token = "" } = useParams();
  return <SharedPage token={token} />;
}

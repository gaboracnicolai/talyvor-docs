import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import type { EditSession } from "~/api/editsession";

// This is the load-bearing PageView-level proof for the edit-session wiring: it renders the REAL
// PageViewPage against a mocked edit-session API + lock hook and inspects the Editor's readOnly
// prop and the banner. No browser needed.

// --- stub the heavy editor + siblings so the render is about the wiring, not their internals ---
vi.mock("~/components/editor/Editor", () => ({
  Editor: (props: { readOnly?: boolean }) => (
    <div data-testid="editor" data-readonly={String(!!props.readOnly)} />
  ),
}));
vi.mock("~/components/ApprovalPanel", () => ({ ApprovalPanel: () => null }));
vi.mock("~/components/CommentsPanel", () => ({ CommentsPanel: () => null }));
vi.mock("~/components/CommentStatsBar", () => ({ CommentStatsBar: () => null }));
vi.mock("~/components/SharePanel", () => ({ SharePanel: () => null }));
vi.mock("~/components/ExportMenu", () => ({ ExportMenu: () => null }));
vi.mock("~/components/editor/PresenceBar", () => ({ PresenceBar: () => null }));
vi.mock("~/components/FreshnessBadge", () => ({ FreshnessBadge: () => null }));
vi.mock("~/components/FreshnessPanel", () => ({ FreshnessPanel: () => null }));
vi.mock("~/components/DocStatusBadge", () => ({ DocStatusBadge: () => null }));
vi.mock("~/components/editor/IssueSearchDialog", () => ({ IssueSearchDialog: () => null }));
vi.mock("~/components/editor/blocks/IssueEmbed", () => ({ IssueEmbed: () => null }));

// --- page + its mutation ---
const fakePage = {
  id: "p1",
  workspace_id: "w1",
  space_id: "s1",
  title: "Doc",
  content: "{}",
  content_text: "hi there",
  doc_status: "draft",
  icon: "📄",
  view_count: 0,
  ai_cost_usd: 0,
  created_by: "alice",
  updated_by: "alice",
  updated_at: "2026-01-01T00:00:00Z",
  parent_id: null,
  page_type: "document",
  last_verified_at: null,
};
vi.mock("~/hooks/usePage", () => ({
  usePage: () => ({ data: fakePage, isLoading: false }),
  useUpdatePage: () => ({ mutate: vi.fn(), isPending: false }),
}));

// --- controllable lock hook (for the "manual lock unaffected" proof) ---
type LockReturn = {
  state: { locked: boolean; locked_by?: string | null } | undefined;
  lockedByMe: boolean;
  lock: { mutate: () => void; isPending: boolean };
  unlock: { mutate: (x?: unknown) => void; isPending: boolean };
};
const lockRef: { current: LockReturn } = {
  current: {
    state: undefined,
    lockedByMe: false,
    lock: { mutate: vi.fn(), isPending: false },
    unlock: { mutate: vi.fn(), isPending: false },
  },
};
vi.mock("~/hooks/usePageLock", () => ({ usePageLock: () => lockRef.current }));

// --- the star: the edit-session API ---
vi.mock("~/api/editsession", () => ({
  editSessionApi: {
    get: vi.fn(),
    acquire: vi.fn(),
    heartbeat: vi.fn(),
    release: vi.fn(),
    takeover: vi.fn(),
  },
}));
import { editSessionApi } from "~/api/editsession";
const api = editSessionApi as unknown as Record<keyof typeof editSessionApi, ReturnType<typeof vi.fn>>;

// --- APIs PageView / VersionHistory / effects hit on mount ---
vi.mock("~/api/pages", () => ({
  pagesApi: {
    recordView: vi.fn().mockResolvedValue({ ok: true }),
    versions: vi.fn().mockResolvedValue([]),
    version: vi.fn(),
    diffVersions: vi.fn(),
    restore: vi.fn(),
  },
}));
vi.mock("~/api/freshness", () => ({ freshnessApi: { forPage: vi.fn().mockResolvedValue(null) } }));
vi.mock("~/api/analytics", () => ({ analyticsApi: { recordView: vi.fn().mockResolvedValue({}) } }));
vi.mock("~/api/links", () => ({ linksApi: { list: vi.fn().mockResolvedValue([]), create: vi.fn(), remove: vi.fn() } }));

import { PageViewPage } from "./PageView";

const live = (holder: string): EditSession => ({
  page_id: "p1",
  workspace_id: "w1",
  holder,
  acquired_at: "2026-01-01T00:00:00Z",
  last_heartbeat: "2026-01-01T00:00:00Z",
  live: true,
});

const space = { id: "s1", name: "Space", workspace_id: "w1" } as never;

function renderPage() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <PageViewPage space={space} pageID="p1" />
    </QueryClientProvider> as ReactNode,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  localStorage.setItem("docs_member_id", "me");
  lockRef.current = {
    state: undefined,
    lockedByMe: false,
    lock: { mutate: vi.fn(), isPending: false },
    unlock: { mutate: vi.fn(), isPending: false },
  };
  api.get.mockResolvedValue(null);
  api.acquire.mockResolvedValue(live("me"));
  api.release.mockResolvedValue({ ok: true });
  api.takeover.mockResolvedValue(live("me"));
  api.heartbeat.mockResolvedValue(live("me"));
});
afterEach(() => localStorage.clear());

const editorReadOnly = () => screen.getByTestId("editor").getAttribute("data-readonly");

describe("PageView edit-session wiring", () => {
  it("acquires the writer slot on mount for an editable page", async () => {
    renderPage();
    await waitFor(() => expect(api.acquire).toHaveBeenCalledTimes(1));
    // editable + I hold it → editor is NOT read-only.
    await waitFor(() => expect(editorReadOnly()).toBe("false"));
  });

  it("makes the editor READ-ONLY when the session is held by someone else, and shows the banner", async () => {
    api.get.mockResolvedValue(live("alice")); // alice holds it, I am "me"
    renderPage();
    await waitFor(() => expect(editorReadOnly()).toBe("true"));
    expect(screen.getByText(/alice is editing/i)).toBeInTheDocument();
  });

  it("manual pagelock path is UNAFFECTED: a foreign manual lock still forces read-only (no session)", async () => {
    api.get.mockResolvedValue(null); // no edit session at all
    lockRef.current.state = { locked: true, locked_by: "alice" };
    lockRef.current.lockedByMe = false;
    renderPage();
    await waitFor(() => expect(editorReadOnly()).toBe("true"));
    // The edit-session banner does NOT claim someone is editing — the read-only came from the lock.
    expect(screen.queryByText(/is editing this page/i)).not.toBeInTheDocument();
  });

  it("mounts the version-history panel", async () => {
    renderPage();
    // "Version history" appears twice — the panel-section title and VersionHistory's own header.
    await waitFor(() => expect(screen.getAllByText(/version history/i).length).toBeGreaterThan(0));
  });
});

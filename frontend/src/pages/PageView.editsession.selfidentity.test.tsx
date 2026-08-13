import { beforeEach, afterEach, describe, expect, it, vi } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";

// THE EDITOR LOCKED YOU OUT OF THE PAGE YOU HAD JUST CLAIMED, NAMED YOU AS THE STRANGER DOING
// IT, AND THEN LET THE SLOT YOU HELD EXPIRE.
//
// `useEditSession` decides "is this session mine" by comparing the server's `holder` against
// `localStorage.getItem("docs_member_id")`. The server's `holder` is a workspace_members row id
// resolved from the GATEWAY-VERIFIED identity (editsession/handler.go `actorFor` ->
// permission.ActorFromContext; "the request body is never read for authority"). The client's
// side of that comparison is a localStorage key **NOTHING IN THE PRODUCT EVER WRITES** — the
// only `localStorage.setItem` in the whole SPA is useSearch's recent-pages list, and no README,
// Makefile or compose file mentions `docs_member_id` at all. Its default value is therefore
// `""` in every browser that has not been hand-seeded, and `sessionFlags` reads that as:
//
//     heldByMe    = live && !!"" && holder === ""   -> ALWAYS FALSE
//     heldByOther = live && !!holder && holder !== "" -> ALWAYS TRUE
//
// ⚠ MEASURED ON THE SHIPPED SCREEN BEFORE ANY CHANGE, by rendering it against a stubbed fetch
// and reading the panel's textContent rather than reading the JSX — autoAcquire takes the slot,
// the server answers with `holder` = this caller, and the screen comes back:
//
//     banner:   "3f9c1d20-0000-4000-8000-000000000001 is editing this page"   (that id is YOU)
//     editor:   readOnly = true
//     heartbeats sent in 11s: 0
//
// Three separate consequences, and the third is the one that outlives the session: the
// heartbeat interval is gated on `heldByMe`, so the slot this screen JUST ACQUIRED is never
// refreshed and dies at the backend's 30s TTL while the author is still typing. The
// single-writer guarantee is not enforced by a client flag — page.Store.Update composes the
// server-side check — so nothing here is a security hole; it is the whole editing surface
// being switched off for its own holder.
//
// ⚠ THE FIX TAKES THE ANSWER FROM THE SERVER RATHER THAN INVENTING A POLICY. Acquire,
// Heartbeat and Takeover each RETURN the Session, and a 2xx from any of them means the slot is
// the caller's — so `holder` on that response IS this browser's verified member id, already on
// the wire. The hook now learns it there and falls back to `docs_member_id` when it has not
// been learned yet. A 423 (live foreign session) is a non-2xx, so nothing is learned from
// someone else's session; control C3 is that direction.
//
// ⚠ WHAT THIS DOES NOT FIX, STATED SO THE SCOPE IS NOT READ AS WIDER THAN IT IS: `docs_member_id`
// is read in SEVEN places (usePageLock, useComments, useEditSession, Sidebar, ApprovalInbox,
// PageView's viewer id, Editor's presence id). This merge fixes the edit-session surface, which
// is the one that has a server answer available on its own wire. Where the SPA's identity should
// come from in general is a product decision, not a patch.

vi.mock("~/components/editor/Editor", () => ({
  Editor: ({ readOnly }: { readOnly?: boolean }) => (
    <div data-testid="editor" data-readonly={String(!!readOnly)} />
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
vi.mock("~/components/VersionHistory", () => ({ VersionHistory: () => null }));
vi.mock("~/components/LinkedIssuesSection", () => ({ LinkedIssuesSection: () => null }));
vi.mock("~/components/changelog/ChangelogView", () => ({ ChangelogView: () => null }));
vi.mock("~/hooks/usePage", () => ({
  usePage: () => ({
    data: {
      id: "p1", workspace_id: "w1", space_id: "s1", title: "Doc", content: "{}",
      content_text: "hi", doc_status: "draft", icon: "D", view_count: 0,
      ai_cost_usd: 0, own_ai_cost_usd: 0, total_ai_cost_usd: 0,
      created_by: "alice", updated_by: "alice", updated_at: "2026-01-01T00:00:00Z",
      parent_id: null, page_type: "document", last_verified_at: null,
    },
    isLoading: false,
  }),
  useUpdatePage: () => ({ mutate: vi.fn(), isPending: false }),
}));
vi.mock("~/hooks/usePageLock", () => ({
  usePageLock: () => ({
    state: undefined, lockedByMe: false,
    lock: { mutate: vi.fn(), isPending: false },
    unlock: { mutate: vi.fn(), isPending: false },
  }),
}));
vi.mock("~/api/freshness", () => ({ freshnessApi: { forPage: vi.fn().mockResolvedValue(null) } }));
vi.mock("~/api/links", () => ({
  linksApi: { list: vi.fn().mockResolvedValue([]), create: vi.fn(), remove: vi.fn() },
}));
vi.mock("~/api/templates", () => ({
  templatesApi: { list: vi.fn().mockResolvedValue([]), fromPage: vi.fn(), use: vi.fn(), delete: vi.fn() },
}));
vi.mock("~/api/analytics", () => ({ analyticsApi: { recordView: vi.fn().mockResolvedValue({}) } }));

import { PageViewPage } from "./PageView";
import { HEARTBEAT_MS } from "~/hooks/useEditSession";

// The server's identity for this caller: a workspace_members row id, which is exactly what
// permission.ActorFromContext returns and what page_edit_sessions.holder stores. `~/api/editsession`
// is deliberately NOT mocked — the wire is the subject.
const SERVER_ACTOR = "3f9c1d20-0000-4000-8000-000000000001";
// STRANGER is a DIFFERENT member holding the slot. `claimStatus` lets a case answer 423 to a
// claim the way the server does for a live foreign session, so "learn who I am from a claim"
// is exercised in the direction where there is nothing to learn.
const STRANGER = "9a11beef-0000-4000-8000-00000000dead";
let holderOnWire = SERVER_ACTOR;
let claimStatus = 200;

const space = { id: "s1", name: "Space", workspace_id: "w1" } as never;
let heartbeats = 0;
let acquires = 0;

function stubFetch() {
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string, init?: RequestInit) => {
      const path = String(url);
      const method = (init?.method ?? "GET").toUpperCase();
      if (path.includes("/edit-session")) {
        const isClaim = method === "POST";
        if (path.endsWith("/heartbeat")) heartbeats += 1;
        else if (isClaim && path.endsWith("/edit-session")) acquires += 1;
        const session = {
          page_id: "p1", workspace_id: "w1", holder: holderOnWire,
          acquired_at: "2026-01-01T00:00:00Z", last_heartbeat: "2026-01-01T00:00:00Z",
          live: true,
        };
        if (isClaim && claimStatus !== 200) {
          return Promise.resolve({
            ok: false, status: claimStatus, statusText: "Locked",
            json: () => Promise.resolve({ error: "held by other", session }),
          } as unknown as Response);
        }
        return Promise.resolve({
          ok: true, status: 200, statusText: "OK",
          json: () => Promise.resolve(session),
        } as unknown as Response);
      }
      return Promise.resolve({
        ok: true, status: 200, statusText: "OK",
        json: () => Promise.resolve({ ok: true }),
      } as unknown as Response);
    }),
  );
}

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
  heartbeats = 0;
  acquires = 0;
  holderOnWire = SERVER_ACTOR;
  claimStatus = 200;
  localStorage.removeItem("docs_member_id");
  localStorage.removeItem("docs_member_name");
  stubFetch();
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe("the edit session the screen just acquired is recognised as its own", () => {
  it("with docs_member_id unset, the holder is not locked out of their own page", async () => {
    renderPage();

    // PRECONDITION, NOT DECORATION. Everything below is about what the screen concludes from
    // the session it acquired — so if autoAcquire never fired, every assertion after this one
    // would be about a screen with no session at all and would pass for free. Control C4
    // (autoAcquire forced off) reddens exactly here.
    await waitFor(() => {
      expect(
        acquires,
        "[SESSION-ACQUIRED] the screen never POSTed to /edit-session, so it holds no slot and " +
          "the assertions below would be vacuous",
      ).toBeGreaterThan(0);
    });

    await waitFor(() => {
      expect(screen.queryByTestId("editor")).not.toBeNull();
    });

    // Give the acquire response + the invalidated session query time to land.
    await act(async () => {
      await new Promise((r) => setTimeout(r, 50));
    });

    expect(
      document.body.textContent,
      "[NO-SELF-AS-STRANGER] the screen tells the holder that someone else is editing, naming " +
        `the holder's own member id (${SERVER_ACTOR}). ` +
        "sessionFlags compares the server's verified holder against localStorage " +
        "docs_member_id, which nothing in this SPA ever writes.",
    ).not.toContain("is editing this page");

    expect(
      screen.getByTestId("editor").getAttribute("data-readonly"),
      "[HOLDER-CAN-EDIT] the editor is read-only for the member who holds the edit session. " +
        "PageView folds heldByOther into Editor's readOnly, and with docs_member_id unset " +
        "heldByOther is true for every live session including your own.",
    ).toBe("false");
  });

  it("the session it holds is heartbeated, so it does not die at the server's TTL", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    renderPage();

    await waitFor(() => {
      expect(acquires, "[SESSION-ACQUIRED] no acquire, so there is no slot to heartbeat").toBeGreaterThan(0);
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(HEARTBEAT_MS * 2 + 500);
    });

    expect(
      heartbeats,
      `[SESSION-HEARTBEATED] ${HEARTBEAT_MS * 2 + 500}ms passed while this screen held the edit ` +
        "session and it sent ZERO heartbeats. The interval is gated on heldByMe, which is false " +
        "whenever docs_member_id is unset — so the slot the screen acquired expires at the " +
        "backend's 30s TTL while the author is still typing.",
    ).toBeGreaterThan(0);
  });
  // THE MUST-STAY-GREEN DIRECTION, AND IT IS WHAT STOPS THE FIX BEING "never show the banner".
  // A live session held by someone ELSE answers 423 to this screen's claim, so there is nothing
  // to learn from it — the banner must still appear, the editor must still be read-only, and
  // this screen must NOT heartbeat a slot it does not hold. C3 (learn the id from the GET, which
  // reports whoever holds the slot) reddens exactly here and nowhere else.
  it("a session held by someone else still locks this screen out and is not heartbeated", async () => {
    holderOnWire = STRANGER;
    claimStatus = 423;
    vi.useFakeTimers({ shouldAdvanceTime: true });
    renderPage();

    await waitFor(() => {
      expect(screen.queryByTestId("editor")).not.toBeNull();
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(HEARTBEAT_MS * 2 + 500);
    });

    expect(
      document.body.textContent,
      "[STRANGER-STILL-LOCKS] a live session held by another member no longer raises the " +
        "editing banner — the screen has started treating someone else's holder as its own",
    ).toContain("is editing this page");
    expect(
      screen.getByTestId("editor").getAttribute("data-readonly"),
      "[STRANGER-STILL-LOCKS] the editor is writable while another member holds a live edit session",
    ).toBe("true");
    expect(
      heartbeats,
      "[NO-FOREIGN-HEARTBEAT] this screen heartbeated a session it does not hold — heartbeating " +
        "someone else's slot is how a client would keep a stranger's lock alive (the server " +
        "rejects it, but the client should never ask)",
    ).toBe(0);
  });
});

import { beforeEach, afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";

// LOCK YOUR OWN PAGE AND THE UI GAVE YOU NO WAY BACK — AND THIS LOCK, UNLIKE THE EDIT SESSION,
// NEVER EXPIRES.
//
// This is the second half of the identity defect fixed for `useEditSession` in `ef7f935`, on a
// surface where the consequence is permanent rather than 30 seconds long. `usePageLock` derives
//
//     lockedByMe = !!s?.locked && !!memberID && s.locked_by === memberID
//
// with `memberID = localStorage.getItem("docs_member_id") || ""` — a key NOTHING in this SPA
// ever writes (the only `localStorage.setItem` in the tree is useSearch's recent-pages list, and
// no README, Makefile or compose file mentions it). `""` makes `lockedByMe` FALSE for every
// lock, including the one this browser just took, and EVERY unlock affordance in the product
// sits behind that flag:
//
//   LockBadge   lockedByMe ? <button "Locked by you" onClick=unlock> : <span "Locked">   (a SPAN)
//   LockBanner  lockedByMe ? "You locked this page." + Unlock : "Locked by X" + a no-op
//               "Request unlock" anchor whose own title says "message the locker directly"
//   PageView    lockedByOther = locked && !lockedByMe   ->  folded into Editor readOnly
//
// So the locker is shown their own member id as the stranger holding the lock, their editor
// goes read-only, and the only remaining route out is an admin. `pages.locked_at` has no TTL
// anywhere — `pagelock/store.go` sets `locked = true` and only an explicit Unlock clears it
// (`SET locked = false, locked_by = NULL`), so nothing times this out the way
// editsession's 30s window did.
//
// ⚠ THE SERVER WOULD HAVE ALLOWED THE UNLOCK. `pagelock/handler.go Unlock` resolves the actor
// from the gateway-verified identity and `UnlockInWorkspaces` permits the locker (or an admin);
// the caller IS the locker. Nothing here is an authorization defect — it is the client refusing
// to offer an action the server would have granted.
//
// ⚠ THE FIX IS THE SAME SHAPE AS `ef7f935` AND FOR THE SAME REASON: `POST .../lock` RETURNS the
// LockState, and a 200 means the caller now holds it, so `locked_by` on that response IS this
// browser's verified member id — already on the wire, no new endpoint, no policy invented. A
// lock held by someone else is a 423, which apiRequest throws on, so nothing is learned from a
// stranger's row; the `get` query is deliberately not a source. Control C3 is that direction.
//
// ⚠ WHAT THIS GUARD IS COUPLED TO, NAMED RATHER THAN DISCOVERED BY A LATER REWORD: exactly two
// product strings, LockBanner's "You locked this page." and LockBadge's "You locked this — click
// to unlock" title. Those two ARE the screen's answer to "is this lock mine", so the coupling is
// the assertion rather than an accident. Everything else is found structurally — the stranger
// case settles on the editor's readOnly, not on its banner copy, and control C5 rewords that
// branch's prose to prove it.

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
// The edit session is mocked to "nobody holds it" so the ONLY thing gating the editor here is
// the manual lock. Without this the two read-only sources are indistinguishable.
vi.mock("~/hooks/useEditSession", () => ({
  HEARTBEAT_MS: 10_000,
  useEditSession: () => ({
    session: undefined, holder: null, live: false, heldByMe: false, heldByOther: false,
    acquire: { mutate: vi.fn(), isPending: false },
    takeover: { mutate: vi.fn(), isPending: false },
    release: { mutate: vi.fn(), isPending: false },
    memberID: "",
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

// What the server stores in pages.locked_by: the workspace_members row id it resolved from the
// gateway-verified identity (`pagelock/handler.go actorFor`). `~/api/pagelock` is deliberately
// NOT mocked — the wire is the subject.
const SERVER_ACTOR = "3f9c1d20-0000-4000-8000-000000000001";
const STRANGER = "9a11beef-0000-4000-8000-00000000dead";

const space = { id: "s1", name: "Space", workspace_id: "w1" } as never;

let lockedBy: string | null = null; // server-side lock state, mutated by POST/DELETE
let lockPosts = 0;

function stubFetch() {
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string, init?: RequestInit) => {
      const path = String(url);
      const method = (init?.method ?? "GET").toUpperCase();
      const state = () => ({
        locked: lockedBy !== null,
        locked_by: lockedBy,
        locked_by_name: null,
        locked_at: lockedBy ? "2026-01-01T00:00:00Z" : null,
      });
      if (path.endsWith("/lock")) {
        if (method === "POST") {
          lockPosts += 1;
          if (lockedBy !== null && lockedBy !== SERVER_ACTOR) {
            // A lock held by someone else is a 423, exactly as pagelock/handler.go answers.
            return Promise.resolve({
              ok: false, status: 423, statusText: "Locked",
              json: () => Promise.resolve({ error: "locked by another member" }),
            } as unknown as Response);
          }
          lockedBy = SERVER_ACTOR;
        } else if (method === "DELETE") {
          lockedBy = null;
          return Promise.resolve({
            ok: true, status: 200, statusText: "OK",
            json: () => Promise.resolve({ ok: true }),
          } as unknown as Response);
        }
        return Promise.resolve({
          ok: true, status: 200, statusText: "OK",
          json: () => Promise.resolve(state()),
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
  lockedBy = null;
  lockPosts = 0;
  localStorage.removeItem("docs_member_id");
  localStorage.removeItem("docs_member_name");
  stubFetch();
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("the lock this screen takes is recognised as its own", () => {
  it("after locking, the locker is offered a way to unlock and is not read-only", async () => {
    renderPage();

    const lockBtn = await screen.findByTitle("Lock this page");
    fireEvent.click(lockBtn);

    // PRECONDITION, NOT DECORATION: if the click never reached the API there is no lock at all
    // and every assertion below would be about an unlocked page — all of them trivially true.
    // Control C4 (the Lock affordance disabled) reddens exactly here.
    await waitFor(() => {
      expect(
        lockPosts,
        "[LOCK-TAKEN] the Lock control never POSTed to .../lock, so the page is not locked and " +
          "the assertions below would pass vacuously",
      ).toBeGreaterThan(0);
    });
    await waitFor(() => {
      expect(document.body.textContent).toContain("Lock");
    });

    await waitFor(() => {
      expect(
        document.body.textContent,
        "[LOCK-SELF-RECOGNISED] after locking the page itself, the screen reports it as locked " +
          `by a stranger and prints the locker's own member id (${SERVER_ACTOR}). ` +
          "usePageLock compares the server's verified locked_by against localStorage " +
          "docs_member_id, which nothing in this SPA ever writes.",
      ).toContain("You locked this page.");
    });

    expect(
      screen.queryByTitle("You locked this — click to unlock"),
      "[LOCKER-CAN-UNLOCK] there is no unlock control for the member who holds the lock. Every " +
        "unlock affordance (LockBadge's button, LockBanner's Unlock) sits behind lockedByMe, " +
        "and pages.locked_by has NO TTL — so this page stays locked until an admin intervenes, " +
        "even though the server would have granted this caller's unlock.",
    ).not.toBeNull();

    expect(
      screen.getByTestId("editor").getAttribute("data-readonly"),
      "[LOCKER-CAN-EDIT] the editor is read-only for the member who just locked the page. " +
        "A lock exists to keep OTHERS out; PageView folds lockedByOther (= locked && " +
        "!lockedByMe) into Editor's readOnly, so with lockedByMe false it locks out the locker.",
    ).toBe("false");
  });

  // THE MUST-STAY-GREEN DIRECTION. A lock held by someone ELSE must still lock this screen out,
  // must NOT offer an unlock, and must not be mistaken for our own. C3 (learn the id from the
  // GET, which reports whoever holds the lock) reddens exactly here and nowhere else.
  it("a lock held by someone else still locks this screen out and offers no unlock", async () => {
    lockedBy = STRANGER;
    renderPage();

    // Settle on a STRUCTURAL fact rather than the stranger-branch copy: the editor going
    // read-only is what "someone else holds this" means to the product. Control C5 rewords that
    // branch's prose and this test must not notice.
    await waitFor(() => {
      expect(
        screen.getByTestId("editor").getAttribute("data-readonly"),
        "[STRANGER-STILL-LOCKS] the editor never went read-only for a lock held by another " +
          "member. This is the settle step AND an assertion: an untagged wait here would " +
          "report a breakage of this case as a bare timeout, which is how controls C2/C3 " +
          "first came back MISSED despite reddening the suite.",
      ).toBe("true");
    });

    expect(
      document.body.textContent,
      "[STRANGER-STILL-LOCKS] another member's lock is being reported as this screen's own",
    ).not.toContain("You locked this page.");
    expect(
      screen.queryByTitle("You locked this — click to unlock"),
      "[STRANGER-STILL-LOCKS] an unlock control is offered for a lock this member does not hold",
    ).toBeNull();
    expect(
      screen.getByTestId("editor").getAttribute("data-readonly"),
      "[STRANGER-STILL-LOCKS] the editor is writable while another member holds the page lock",
    ).toBe("true");
  });
});

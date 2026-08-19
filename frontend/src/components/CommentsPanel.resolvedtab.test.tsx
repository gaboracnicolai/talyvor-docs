import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { Comment, CommentStats } from "~/api/comments";

// THE "RESOLVED" TAB LISTED THE OPEN THREADS TOO, BESIDE A COUNT THAT DID NOT INCLUDE THEM.
//
// `include_resolved` is an INCLUDE flag on the server, not an ONLY flag. `comment.Store.ListByPage`
// is one statement and the parameter only ever ADDS a predicate:
//
//     q := `SELECT ... FROM page_comments WHERE page_id = $1`
//     if !includeResolved { q += ` AND resolved = false` }
//
// so `include_resolved=true` drops the filter and returns EVERY thread on the page. The SPA drove
// that flag from a two-value TAB — `useComments(spaceID, pageID, tab === "resolved")` — and then
// rendered `threads.map(...)` whole. Clicking "Resolved" therefore asked for "open AND resolved"
// and painted the answer under a heading that promises one of them.
//
// ⚠ THE COUNT BESIDE THE TAB COMES FROM A DIFFERENT QUERY THAT *IS* EXCLUSIVE, WHICH IS WHY THE
// SCREEN CONTRADICTS ITSELF RATHER THAN MERELY OVER-ANSWERING. `comment.Store.GetStats` counts
// `parent_id IS NULL AND resolved = true` for the resolved half, so on the fixture below the tab
// reads "Resolved (1)" above a list of THREE. One number and one list, on one screen, disagreeing
// by construction.
//
// ⚠ AND THE STRAY THREADS ARRIVE WITH THE WRONG CONTROLS ATTACHED: `CommentThread` renders the
// reply box + "Resolve" button for an unresolved thread, so the Resolved tab offered "Resolve" on
// threads it was claiming were already resolved.
//
// ⚠ THE SERVER IS NOT THE THING THAT IS WRONG, AND THE FIX IS DELIBERATELY NOT THERE. An
// `include_*` parameter that means "also include" is the ordinary reading of the name, the Go
// handler is the only definition of it, and the SPA is its only caller — so narrowing the flag
// server-side would fix this screen by making the parameter lie about its own name. The panel
// asks for the superset and selects from it; the data it needs is already on the wire
// (`Comment.resolved`, non-optional in `~/api/comments`). The server's inclusive contract is
// PINNED in Go by TestListByPage_IncludeResolvedIsInclusiveNotExclusive_RealPG so this fix cannot
// later be "corrected" into a server change that silently re-breaks the same screen.
//
// ⚠ WHY NOTHING COULD SEE IT: `CommentsPanel` and `CommentThread` had NO test file of any kind
// before this one — the whole comment surface of the SPA was rendered by nothing. The Go side is
// covered (`internal/comment`), and it is correct; the defect lives entirely in the composition
// between a flag and a tab, which is exactly the seam neither side's tests reach.
//
// ⚠ THE FAKE BELOW IMPLEMENTS THE SERVER'S REAL SEMANTICS RATHER THAN A CONVENIENT ONE. If it
// filtered to `resolved === true` for the resolved arm it would be asserting my own fixture and
// would have passed against the defect. It reproduces `ListByPage` line for line: no filter when
// including, `resolved === false` otherwise.

const thread = (id: string, content: string, resolved: boolean): Comment => ({
  id,
  page_id: "pg-1",
  thread_id: id,
  parent_id: null,
  author_id: "mem-1",
  author_name: "Alice",
  content,
  resolved,
  resolved_by: resolved ? "mem-2" : null,
  resolved_at: resolved ? "2026-08-19T09:00:00Z" : null,
  replies: [],
  created_at: "2026-08-19T08:00:00Z",
  updated_at: "2026-08-19T08:00:00Z",
});

// TWO open threads and ONE resolved thread — and the asymmetry is load-bearing, not decoration.
//
// ⚠ THE FIRST VERSION OF THIS FIXTURE WAS 1 AND 1, AND A CONTROL PROVED IT VACUOUS. With one of
// each, [RESOLVED-TAB-MATCHES-ITS-COUNT] survives an INVERTED selection: the resolved tab renders
// the one open thread, the label still claims 1, and 1 === 1. Control F2 predicted that case would
// catch the inversion and it did NOT — the count assertion was pinning cardinality against a set
// whose cardinality could not move. Unequal counts are the only shape in which "as many as it
// claims" and "the ones it claims" are different assertions.
const ALL = [
  thread("c-open-1", "the first open one", false),
  thread("c-open-2", "the second open one", false),
  thread("c-done", "the resolved one", true),
];

vi.mock("~/api/comments", () => ({
  commentsApi: {
    // comment.Store.ListByPage, verbatim: the flag only ever REMOVES the `resolved = false`
    // predicate. It never selects FOR resolved.
    list: vi.fn(
      async (_s: string, _p: string, includeResolved = false): Promise<Comment[]> =>
        includeResolved ? ALL : ALL.filter((c) => !c.resolved),
    ),
    // comment.Store.GetStats: counted over thread HEADS (parent_id IS NULL), and the resolved
    // half is exclusive — `AND resolved = true`.
    stats: vi.fn(
      async (): Promise<CommentStats> => ({
        total: ALL.length,
        open: ALL.filter((c) => !c.resolved).length,
        resolved: ALL.filter((c) => c.resolved).length,
      }),
    ),
    create: vi.fn(async () => ALL[0]),
    reply: vi.fn(async () => ALL[0]),
    resolve: vi.fn(async () => ({})),
    unresolve: vi.fn(async () => ({})),
    remove: vi.fn(async () => ({})),
  },
}));

import { CommentsPanel } from "./CommentsPanel";

// renderPanel mounts the real panel and returns a helper that reads the rendered thread bodies.
//
// ⚠ THREADS ARE COUNTED BY THEIR RENDERED CONTENT, not by a container query: the panel wraps each
// thread in a CommentThread <article>, and asserting on the article count alone would stay green
// if the right NUMBER of wrong threads were drawn.
async function renderPanel() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <CommentsPanel spaceID="sp-1" pageID="pg-1" />
    </QueryClientProvider>,
  );
  const u = userEvent.setup();
  return {
    u,
    async bodies(): Promise<string[]> {
      // Wait for the list to settle rather than for a specific body — waiting for the thread we
      // EXPECT would let the defect through, because the extra thread arrives in the same render.
      await waitFor(() => {
        if (document.querySelectorAll("article").length === 0) {
          throw new Error("no threads rendered yet");
        }
      });
      // Identify each rendered thread by WHICH fixture body it carries, so a wrong thread is a
      // wrong NAME in the assertion rather than a right count of anonymous articles.
      return Array.from(document.querySelectorAll("article")).map((a) => {
        const text = a.textContent ?? "";
        const hit = ALL.find((c) => text.includes(c.content));
        return hit ? hit.content : `<unrecognised: ${text.slice(0, 40)}>`;
      });
    },
  };
}

describe("CommentsPanel tabs", () => {
  it("[RESOLVED-TAB-EXCLUDES-OPEN] the Resolved tab lists only resolved threads", async () => {
    const { u, bodies } = await renderPanel();
    await u.click(screen.getByRole("button", { name: /^Resolved/ }));
    expect(await bodies()).toEqual(["the resolved one"]);
  });

  it("[RESOLVED-TAB-MATCHES-ITS-COUNT] the Resolved tab lists exactly as many threads as its own count claims", async () => {
    const { u, bodies } = await renderPanel();
    await u.click(screen.getByRole("button", { name: /^Resolved/ }));
    // The label is the product's own claim about the list beneath it. Read the number out of it
    // rather than restating 1 here, so the two sides cannot be edited apart.
    const label = screen.getByRole("button", { name: /^Resolved/ }).textContent ?? "";
    const claimed = Number(/\((\d+)\)/.exec(label)?.[1]);
    expect(claimed).toBe(1);
    expect((await bodies()).length).toBe(claimed);
  });

  // MUST-STAY-GREEN COMPANION. The Open tab was already correct — the server's default arm is the
  // exclusive one — and it must not be collateral damage of the fix. Without this, "the Resolved
  // tab shows one thread" is also satisfied by a panel that shows one thread everywhere.
  it("[OPEN-TAB-UNCHANGED] the Open tab still lists only open threads", async () => {
    const { bodies } = await renderPanel();
    expect(await bodies()).toEqual(["the first open one", "the second open one"]);
  });
});

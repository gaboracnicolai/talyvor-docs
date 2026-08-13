import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";

// THE "NEEDS REVIEW" SCREEN PROMISED A CRITERION ITS ROUTE CANNOT APPLY, AND ITS EMPTY STATE
// TURNED THAT INTO A POSITIVE CLAIM.
//
// Header, verbatim at 9e0ac54: "Pages flagged by freshness rules — past their stale_after_days
// threshold, or with linked Track issues completed since last edit." Empty state: "All clear —
// nothing needs review."
//
// The whole population of GET /workspaces/{ws}/freshness is page.Store.GetStalePages, ONE SQL
// predicate over stale_after_days. The linked-issue signal is computed only for pages that
// predicate already returned — it annotates a row and can never add one. MEASURED on real
// Postgres with links and a Track reader genuinely wired, by
// internal/freshness/stalereport_population_realpg_test.go: a page with stale_after_days = 0 and
// BOTH linked issues `done` is absent from this screen while the per-page route reports
// suggest_review = true for that same page.
//
// So a workspace whose every page is inside its TTL, all of their Track issues completed,
// rendered "All clear — nothing needs review." — the exact shape internal/freshness's own
// comment rejects for this route ("An empty stale report is not a neutral response").
//
// ⚠ THIS FILE RENDERS THE SCREEN RATHER THAN GREPPING IT. The claim is about what a human reads,
// and the copy sits inside JSX that a text search over the file cannot tell apart from a
// comment — this same block, three lines up, contains the false sentence verbatim.
//
// ⚠ THE SCREEN HAD NO TEST AT ALL BEFORE THIS ONE, which is the other half of why the sentence
// survived: nothing rendered it.
//
// THE CASES:
//
//   [NO-FALSE-OR]     header does not promise the linked-issue criterion
//   [NAMES-REAL]      header still names the criterion the route DOES apply
//   [EMPTY-HONEST]    the empty state claims a threshold, not universal all-clear
//   [ROWS-RENDER]     a returned row still names its page          ← positive control

const forWorkspace = vi.fn();
vi.mock("~/api/freshness", async (orig) => {
  const actual = await (orig() as Promise<Record<string, unknown>>);
  return { ...actual, freshnessApi: { forWorkspace: (...a: unknown[]) => forWorkspace(...a) } };
});
vi.mock("~/api/pages", () => ({ pagesApi: { verify: vi.fn() } }));

import { StalePagesPage } from "./StalePages";

// The exact shape GET /v1/workspaces/{ws}/freshness returns for a TTL-blown page that also has
// two completed linked issues — the ONLY kind of row this route can produce.
const ONE_STALE_ROW = [
  {
    page_id: "62777159-ccea-4081-9770-ed1bde148610",
    space_id: "sp-1",
    title: "Deploy runbook",
    status: "stale",
    days_since_edit: 400,
    stale_after_days: 1,
    linked_issues_closed: 2,
    suggest_review: true,
    reason: "Not updated in 400 days (TTL: 1 days) · 2 linked issues completed",
    updated_at: "2025-07-10T00:00:00Z",
  },
];

function wrap(ui: ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return <QueryClientProvider client={qc}>{ui}</QueryClientProvider>;
}

describe("Needs review — the screen must not promise what its route cannot deliver", () => {
  beforeEach(() => {
    forWorkspace.mockReset();
  });

  it("[NO-FALSE-OR] + [NAMES-REAL] + [EMPTY-HONEST]", async () => {
    forWorkspace.mockResolvedValue([]);
    render(wrap(<StalePagesPage workspaceID="ws-1" onOpenPage={() => {}} />));

    const header = await screen.findByRole("heading", { name: /needs review/i });
    const blurb = header.parentElement!.textContent ?? "";

    // [NO-FALSE-OR] — the criterion the population cannot apply must not be offered as one.
    expect(blurb).not.toMatch(/or with linked Track issues completed/i);
    // [NAMES-REAL] — and dropping the false half must not drop the true one.
    expect(blurb).toMatch(/stale_after_days/);

    // [EMPTY-HONEST] — an empty list is a statement about one threshold, not about the
    // workspace. "All clear" is the claim this route is not entitled to make.
    await waitFor(() => {
      expect(screen.getByText(/no page is past its stale_after_days threshold/i)).toBeTruthy();
    });
    expect(screen.queryByText(/all clear/i)).toBeNull();
  });

  // POSITIVE CONTROL. Without it, every assertion above is satisfied by a screen that renders
  // nothing at all — including one whose row rendering is broken.
  it("[ROWS-RENDER] a stale row still names its page", async () => {
    forWorkspace.mockResolvedValue(ONE_STALE_ROW);
    render(wrap(<StalePagesPage workspaceID="ws-1" onOpenPage={() => {}} />));
    expect(await screen.findByText("Deploy runbook")).toBeTruthy();
    expect(screen.queryByText(/no page is past its stale_after_days threshold/i)).toBeNull();
  });
});

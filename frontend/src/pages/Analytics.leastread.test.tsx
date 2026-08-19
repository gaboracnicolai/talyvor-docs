import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";

// "All pages have plenty of traffic." COULD ONLY EVER APPEAR WHEN IT WAS FALSE.
//
// The workspace tab renders the least-read cohort through `PageList`, whose empty caption was the
// constant "All pages have plenty of traffic.". That is a positive claim about readership, and the
// state it was displayed in is the one state in which the claim is untrue.
//
// ⚠ NOT "SOMETIMES WRONG" — UNREACHABLE-WHEN-TRUE, AND THAT IS MEASURED IN GO ON REAL POSTGRES,
// not argued here: internal/analytics/leastreadcaption_realpg_test.go drives the shipped route and
// pins
//
//	    least_read_pages == []   ⟺   total_views == 0
//
// Both come from ONE slice inside `GetWorkspaceStats`: `LeastReadPages` is filled from `visible`
// and `TotalViews` is summed over that same `visible`, and every ranked row carries `COUNT(*) >= 1`.
// So the cohort is empty in exactly one situation — NOTHING THE CALLER CAN SEE HAS BEEN READ AT
// ALL — and that is precisely when "all pages have plenty of traffic" is a false statement. Its
// [HIDDEN-TRAFFIC] case covers the one way the equivalence could have failed (real readership the
// caller may not see) and shows both figures falling to zero together.
//
// The result on screen was a self-contradiction in a single response: a workspace with fifty
// documents nobody has ever opened reported
//
//	    Never read   50            ← the stat tile, from never_read_count
//	    Needs attention (lowest read):  All pages have plenty of traffic.
//
// on the screen whose entire purpose is surfacing neglected documents, in the state where that
// purpose matters most.
//
// ⚠ THE BRANCH IS DELETED RATHER THAN MADE CONDITIONAL, AND THAT IS DELIBERATE. Keeping "all pages
// have plenty of traffic" for the case where traffic EXISTS and the cohort is somehow still empty
// would be a branch no production path can reach — which is a defect this repository has found and
// removed repeatedly (the `debitTotal` sign fallback whose comment named a caller it did not have;
// the dead `track/formatUSD`; `useTrackProbe`, whose docstring called it "the whole mechanism" for
// a state nothing could produce). A branch kept alive for a caller that does not exist is its own
// finding, so this does not ship one.
//
// ⚠ WHAT REPLACES IT INVENTS NO PRODUCT COPY. The caption becomes "No views yet." — the string this
// same screen ALREADY shows, for this same data condition, on the sibling cohort directly above it.
// A more specific sentence ("no page has been read in the last 30 days") would read better and is
// NOT taken: what a screen should say beyond being true is a product call, and swapping a false
// claim for an existing true one needs no such call.
//
// ⚠⚠ AN EXISTING TEST ASSERTED THE DEFECT AS CORRECT AND IS CHANGED IN THIS COMMIT — said loudly
// rather than left for a reviewer to notice in the diff. `Analytics.emptystate.test.tsx`'s
// [WS-EMPTY] case fed a roll-up with `never_read_count: 1` and asserted the screen showed "All
// pages have plenty of traffic.". It was written to prove the empty path RENDERS AT ALL (the null
// crash), and it pinned the wording it happened to find on the way. Its assertion is updated to the
// corrected caption; nothing else in that file moves, and its purpose is untouched.
//
// THE CASES:
//
//	[NO-TRAFFIC-NO-CLAIM]   unread workspace   the false claim is ABSENT       ← RED before
//	[NO-TRAFFIC-SAYS-TRUE]  unread workspace   a true empty caption is shown   ← RED before
//	[POPULATED-UNCHANGED]   cohorts with rows  still names the pages, no caption  ← green before
//	[NEVER-READ-STILL-SHOWN] unread workspace  the never_read figure survives  ← green before

const pageStats = vi.fn();
const workspaceStats = vi.fn();
vi.mock("~/api/analytics", () => ({
  analyticsApi: {
    pageStats: (...a: unknown[]) => pageStats(...a),
    workspaceStats: (...a: unknown[]) => workspaceStats(...a),
    recordView: vi.fn(),
  },
}));

import { AnalyticsPage } from "./Analytics";

function wrap(ui: ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return <QueryClientProvider client={qc}>{ui}</QueryClientProvider>;
}

// The EXACT shape GET /v1/workspaces/{ws}/analytics/pages returns for a workspace whose documents
// exist and have never been read — the state leastreadcaption_realpg_test.go's [NO-TRAFFIC] case
// measures on real Postgres. `total_views: 0` beside an empty cohort is that file's equivalence.
const UNREAD_WORKSPACE = {
  total_views: 0,
  unique_viewers: 0,
  most_read_pages: [],
  least_read_pages: [],
  never_read_count: 50,
};

const POPULATED = {
  total_views: 2,
  unique_viewers: 1,
  most_read_pages: [
    { page_id: "2f67cae8", title: "Runbook", total_views: 2, unique_viewers: 1, avg_duration_sec: 10, views_by_day: [], top_viewers: [] },
  ],
  least_read_pages: [
    { page_id: "2f67cae8", title: "Runbook", total_views: 2, unique_viewers: 1, avg_duration_sec: 10, views_by_day: [], top_viewers: [] },
  ],
  never_read_count: 0,
};

const FALSE_CLAIM = "All pages have plenty of traffic.";

describe("Analytics workspace tab — the least-read cohort's empty caption", () => {
  beforeEach(() => {
    pageStats.mockReset();
    workspaceStats.mockReset();
  });

  it("[NO-TRAFFIC-NO-CLAIM] does not claim traffic for a workspace nothing has been read in", async () => {
    workspaceStats.mockResolvedValue(UNREAD_WORKSPACE);
    render(wrap(<AnalyticsPage workspaceID="w1" />));

    // ⚠ WAIT ON THE SECTION HEADING, AND DO NOT "TIDY" THIS INTO A STATIC ANCHOR. The heading is
    // rendered by `PageList` — the component under test — so this line makes the absence assertion
    // below SELF-GUARDING: it cannot pass because the whole section vanished, which is the classic
    // way an absence assertion goes vacuous. Measured, not intended: control C3 (least-read section
    // deleted) was predicted to fail only the two length assertions and failed this case too,
    // precisely because the anchor went with the section.
    expect(await screen.findByText("Needs attention (lowest read)")).toBeInTheDocument();
    expect(screen.queryByText(FALSE_CLAIM)).not.toBeInTheDocument();
  });

  it("[NO-TRAFFIC-SAYS-TRUE] shows a true empty caption instead", async () => {
    workspaceStats.mockResolvedValue(UNREAD_WORKSPACE);
    render(wrap(<AnalyticsPage workspaceID="w1" />));

    // Both cohorts are empty in this state and both now carry the same true caption, so there are
    // two. Asserting the COUNT is what stops the least-read list from silently rendering nothing
    // at all and passing [NO-TRAFFIC-NO-CLAIM] by disappearing.
    await screen.findByText("Needs attention (lowest read)");
    expect(screen.getAllByText("No views yet.")).toHaveLength(2);
  });

  it("[NEVER-READ-STILL-SHOWN] still reports the never-read figure that makes the claim false", async () => {
    workspaceStats.mockResolvedValue(UNREAD_WORKSPACE);
    render(wrap(<AnalyticsPage workspaceID="w1" />));

    expect(await screen.findByText("Never read")).toBeInTheDocument();
    expect(screen.getByText("50")).toBeInTheDocument();
  });

  it("[POPULATED-UNCHANGED] still names the ranked pages when the cohorts carry rows", async () => {
    workspaceStats.mockResolvedValue(POPULATED);
    render(wrap(<AnalyticsPage workspaceID="w1" />));

    // The positive control: a caption fix that emptied the lists, or that showed an empty caption
    // unconditionally, passes every case above and fails here.
    await screen.findByText("Needs attention (lowest read)");
    expect(screen.getAllByText("Runbook")).toHaveLength(2);
    expect(screen.queryByText("No views yet.")).not.toBeInTheDocument();
    expect(screen.queryByText(FALSE_CLAIM)).not.toBeInTheDocument();
  });
});

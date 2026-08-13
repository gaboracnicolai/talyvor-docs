import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";

// THE ANALYTICS SCREEN HAD NO TEST AT ALL, AND ITS EMPTY STATE BLANKED THE WHOLE APPLICATION.
//
// `api/analytics.ts` declares four REQUIRED arrays — `ReadStats.views_by_day`,
// `ReadStats.top_viewers`, `WorkspaceReadStats.most_read_pages`, `.least_read_pages` — and this
// screen dereferences all four bare. The Go store only ever `append`s to the matching slices, so
// an unappended one is nil and `encoding/json` writes `null`.
//
// MEASURED off the shipped routes on real Postgres at 366808c, raw response bytes:
//
//   rollup, brand-new workspace   …"most_read_pages":null,"least_read_pages":null…
//   per-page, never-viewed page   …"views_by_day":null,"top_viewers":null…
//
// Rendered with those bytes, BEFORE the store fix:
//
//   workspace tab   TypeError: Cannot read properties of null (reading 'length')   Analytics.tsx:156
//   this-page tab   TypeError: Cannot read properties of null (reading 'map')      Analytics.tsx:70
//
// ⚠⚠ AND THERE IS NO ErrorBoundary ANYWHERE IN THIS SPA — measured: zero files under src/ match
// `ErrorBoundary|componentDidCatch|getDerivedStateFromError`. React 18 unmounts the ROOT on an
// uncaught render throw, so the blast radius of either line above is the entire application, not
// the Analytics panel.
//
// ⚠ WHAT THIS FILE IS AND IS NOT, SAID RATHER THAN IMPLIED. THE FIX IS SERVER-SIDE — the store now
// normalises its nil slices (internal/analytics/store.go, withEmptyLists/withEmptyCohorts) and the
// guard that FAILS on the defect is Go's emptylists_realpg_test.go, which asserts on the RAW
// response bytes. This file is the OTHER end of the same contract: it feeds the screen the exact
// bytes that route now emits and asserts the screen renders them. The empty-array cases below are
// GREEN both before and after the server fix — an empty array never crashed. Their job is that the
// empty path of this screen is exercised at all (it never was), so a future `points[0].date` or
// `items[0].title` is loud here instead of on a customer's first day.
//
// THE CASES:
//
//   [WS-EMPTY]        rollup with `[]` cohorts     renders both empty captions + the figures
//   [PAGE-EMPTY]      page stats with `[]` lists   renders "No data yet." + "No views yet."
//   [WS-POPULATED]    rollup with rows             names the pages          ← positive control
//   [PAGE-POPULATED]  page stats with rows         names the viewer + peak  ← positive control
//
// The two positive controls are why rendering the empty caption unconditionally is not a pass.

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

// The EXACT bytes GET /v1/workspaces/{ws}/analytics/pages returns for a workspace whose ranked
// cohorts are empty — measured on real Postgres through the shipped handler after the store fix.
const EMPTY_ROLLUP =
  '{"total_views":0,"unique_viewers":0,"most_read_pages":[],"least_read_pages":[],"never_read_count":1}';

// The EXACT bytes GET /v1/spaces/{sp}/pages/{pg}/analytics returns for a page with no views in
// the window, same measurement.
const EMPTY_PAGE_STATS =
  '{"page_id":"2f67cae8-1674-4c46-b06c-e80a5842c519","title":"","total_views":0,' +
  '"unique_viewers":0,"avg_duration_sec":0,"views_by_day":[],"top_viewers":[]}';

// A populated roll-up, same shape the route emits — note each ranked ROW also carries `[]` for
// its two lists, which is the half the empty-workspace case cannot show.
const FULL_ROLLUP =
  '{"total_views":2,"unique_viewers":1,"most_read_pages":[{"page_id":"2f67cae8","title":"Runbook",' +
  '"total_views":2,"unique_viewers":0,"avg_duration_sec":0,"views_by_day":[],"top_viewers":[]}],' +
  '"least_read_pages":[{"page_id":"2f67cae8","title":"Runbook","total_views":2,"unique_viewers":0,' +
  '"avg_duration_sec":0,"views_by_day":[],"top_viewers":[]}],"never_read_count":0}';

const FULL_PAGE_STATS =
  '{"page_id":"2f67cae8","title":"","total_views":2,"unique_viewers":1,"avg_duration_sec":10,' +
  '"last_viewed_at":"2026-08-13T14:46:45.599717+03:00",' +
  '"views_by_day":[{"date":"2026-08-13T03:00:00+03:00","count":2}],' +
  '"top_viewers":[{"viewer_id":"mbr_32ae","viewer_name":"Alice","view_count":2,' +
  '"last_viewed":"2026-08-13T14:46:45.599717+03:00"}]}';

function wrap(ui: ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return <QueryClientProvider client={qc}>{ui}</QueryClientProvider>;
}

describe("Analytics — the empty state the roll-up actually returns", () => {
  beforeEach(() => {
    pageStats.mockReset();
    workspaceStats.mockReset();
  });

  it("[WS-EMPTY] renders the workspace tab when both cohorts are empty", async () => {
    workspaceStats.mockResolvedValue(JSON.parse(EMPTY_ROLLUP));
    render(wrap(<AnalyticsPage workspaceID="w1" />));

    expect(await screen.findByText("No views yet.")).toBeInTheDocument();
    expect(screen.getByText("All pages have plenty of traffic.")).toBeInTheDocument();
    // The one figure this state DOES report — a workspace with a never-read page is precisely
    // the state whose screen used to blank.
    expect(screen.getByText("Never read")).toBeInTheDocument();
    expect(screen.getByText("1")).toBeInTheDocument();
  });

  it("[PAGE-EMPTY] renders the page tab when both lists are empty", async () => {
    pageStats.mockResolvedValue(JSON.parse(EMPTY_PAGE_STATS));
    render(
      wrap(<AnalyticsPage workspaceID="w1" page={{ spaceID: "s1", pageID: "p1", title: "Runbook" }} />),
    );

    // The chart's own empty state, and the top-viewers list's.
    expect(await screen.findByText("No data yet.")).toBeInTheDocument();
    expect(screen.getByText("No views yet.")).toBeInTheDocument();
    // `Math.max(0, ...[].map(…))` is 0, not -Infinity — the line the null crashed on.
    expect(screen.getByText("Day peak")).toBeInTheDocument();
    expect(screen.getByText("0s")).toBeInTheDocument();
  });

  it("[WS-POPULATED] still names the ranked pages when the cohorts carry rows", async () => {
    workspaceStats.mockResolvedValue(JSON.parse(FULL_ROLLUP));
    render(wrap(<AnalyticsPage workspaceID="w1" />));

    // Both cohorts hold the same single page, so the title appears twice.
    await waitFor(() => expect(screen.getAllByText("Runbook")).toHaveLength(2));
    expect(screen.getAllByText("2 views")).toHaveLength(2);
    expect(screen.queryByText("No views yet.")).not.toBeInTheDocument();
    expect(screen.queryByText("All pages have plenty of traffic.")).not.toBeInTheDocument();
  });

  it("[PAGE-POPULATED] still draws the series and the viewer list", async () => {
    pageStats.mockResolvedValue(JSON.parse(FULL_PAGE_STATS));
    render(
      wrap(<AnalyticsPage workspaceID="w1" page={{ spaceID: "s1", pageID: "p1", title: "Runbook" }} />),
    );

    expect(await screen.findByText("Alice")).toBeInTheDocument();
    expect(screen.getByText("2 views")).toBeInTheDocument();
    expect(screen.getByText("10s")).toBeInTheDocument();
    expect(screen.queryByText("No data yet.")).not.toBeInTheDocument();
    expect(screen.queryByText("No views yet.")).not.toBeInTheDocument();
  });
});

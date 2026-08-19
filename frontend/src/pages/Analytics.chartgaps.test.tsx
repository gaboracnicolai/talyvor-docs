import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";

// THE VIEWS-BY-DAY CHART SPACED ITS POINTS BY INDEX, AND THE SERIES IT IS FED OMITS ITS ZEROS.
//
// `analytics.Store.GetReadStats` builds `views_by_day` with
//
//     SELECT DATE_TRUNC('day', created_at), COUNT(*)::int … GROUP BY 1 ORDER BY 1
//
// so a day with no views produces NO ROW. The series is SPARSE, and nothing on the wire says
// which days are missing — only each surviving row's own `date` does.
//
// `ViewsLineChart` placed point i at `padX + i*(width-2padX)/(n-1)`: EVENLY SPACED BY POSITION IN
// THE ARRAY, with no reference to `date` at all. Every gap therefore rendered the same width
// whatever its real length.
//
// ⚠ MEASURED THROUGH THE SHIPPED SCREEN BEFORE THE FIX (jsdom, the real component, the exact
// series shape the route emits) — Aug 1 (5 views), Aug 2 (3), Aug 29 (1):
//
//     path  M12.0,12.0 L300.0,50.4 L588.0,88.8
//     cx    ["12", "300", "588"]            ← 288px, 288px
//     tips  ["8/1/2026: 5 views", "8/2/2026: 3 views", "8/29/2026: 1 views"]
//
// A ONE-DAY gap and a TWENTY-SEVEN-DAY gap drawn at IDENTICAL width, on the screen whose stated
// job is finding documents nobody reads. 27 dead days render as a gentle downward slope,
// pixel-for-pixel what a page read every single day would draw. The tooltips are what settle that
// the axis is meant to be time and not ordinal: each names a calendar date.
//
// ⚠ THE COMMENT ON THE COMPONENT ASSERTED THE THING THAT IS FALSE — "the data has at most 30
// points" is true and reads as "one point per day", which is what index spacing would need. The
// count is bounded; the SPACING is not uniform. That comment is corrected in the same commit.
//
// ⚠ WHAT THIS FIX DOES NOT DO, SAID RATHER THAN IMPLIED. The x domain is FIRST POINT → LAST
// POINT, not the requested `?days=` window: the chart still says nothing about dead days BEFORE
// the first view or AFTER the last, because the series does not carry the window and inventing
// one here would be a second unmeasured claim. And a segment between two distant points still
// INTERPOLATES — the line from Aug 2 to Aug 29 passes through heights no day actually had. That
// is the ordinary semantics of a line chart and a real remaining weakness; killing it means
// zero-filling the series in Go, which changes what the empty page returns (`[]` → 30 zeros) and
// with it the shipped "No data yet." state that [EMPTY-UNCHANGED] below pins. Separate finding,
// separate merge.
//
// THE CASES, and which of them can fail on the defect:
//
//   [GAP-PROPORTIONAL]   sparse series          gap widths track REAL day counts   ← RED before
//   [EVEN-STAYS-EVEN]    3 consecutive days     still evenly spaced                ← green before
//   [SINGLE-POINT]       one day                finite coords, at the left pad     ← green before
//   [EMPTY-UNCHANGED]    no days                still "No data yet."               ← green before
//
// The last three are must-stay-green controls, and they are not decoration: a "fix" that simply
// scattered the x values, or that collapsed every point onto one x, or that divided by a zero
// span, passes [GAP-PROPORTIONAL] and fails one of them. They are what stops this file from being
// satisfied by anything other than a real time axis.
//
// ⚠ TWO OF THESE CASES ARE STRONGER THAN THEIR NAMES, AND I LEARNED THAT FROM A CONTROL RATHER
// THAN FROM WRITING THEM — recorded here because the next reader will otherwise trust the names.
// Harness ~/talyvor-queue/w31-chartgaps-controls-6e4b.py, 7 controls, each predicting its catcher
// first; two predictions were WRONG, both by catching more than predicted:
//   • [EVEN-STAYS-EVEN] also pins the axis DIRECTION. Reversing the scale leaves equal gaps equal,
//     so its `toBeCloseTo` clause is blind to that mutation; what fires is `x2 - x0 > 500`, which
//     is -576 when the series runs right-to-left. The span clause is doing two jobs.
//   • [SINGLE-POINT] also rejects a wrong axis outright, because it asserts the exact coordinate
//     (12) and not merely a finite one — an x driven by view count puts its lone 3-view day at 588.
// Neither weakens the guard. Both mean the file catches more than its case names promise, which is
// worth knowing before someone "simplifies" one of those assertions.

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

function stats(days: { date: string; count: number }[]) {
  return {
    page_id: "2f67cae8",
    title: "",
    total_views: days.reduce((n, d) => n + d.count, 0),
    unique_viewers: 1,
    avg_duration_sec: 10,
    views_by_day: days,
    top_viewers: [],
  };
}

// The chart is the ONLY svg on this screen with this viewBox; the stat tiles each carry a lucide
// icon svg. Selecting `svg` bare picks up an icon — measured, it returned the Eye glyph's path.
const CHART = 'svg[viewBox="0 0 600 120"]';

async function renderChart(days: { date: string; count: number }[]) {
  pageStats.mockResolvedValue(stats(days));
  render(
    wrap(<AnalyticsPage workspaceID="w1" page={{ spaceID: "s1", pageID: "p1", title: "Runbook" }} />),
  );
  await screen.findByText("Top viewers");
  const svg = document.querySelector(CHART);
  if (!svg) return null;
  return [...svg.querySelectorAll("circle")].map((c) => Number(c.getAttribute("cx")));
}

describe("ViewsLineChart — a sparse daily series on a time axis", () => {
  it("[GAP-PROPORTIONAL] draws a 27-day gap 27x wider than a 1-day gap", async () => {
    const cx = await renderChart([
      { date: "2026-08-01T00:00:00Z", count: 5 },
      { date: "2026-08-02T00:00:00Z", count: 3 },
      { date: "2026-08-29T00:00:00Z", count: 1 },
    ]);
    expect(cx).not.toBeNull();
    expect(cx).toHaveLength(3);
    const [x0, x1, x2] = cx!;
    cx!.forEach((x) => expect(Number.isFinite(x)).toBe(true));

    // The span is 28 days; the first gap is 1 of them. Asserted as a RATIO of the drawn width so
    // the test does not re-encode padX/width — restating those here is the third-copy trap.
    const total = x2 - x0;
    expect(total).toBeGreaterThan(0);
    expect((x1 - x0) / total).toBeCloseTo(1 / 28, 3);
    expect((x2 - x1) / total).toBeCloseTo(27 / 28, 3);

    // And the plain statement of the defect, independent of the arithmetic above: the long gap is
    // drawn wider than the short one. On index spacing the two were EQUAL (288px, 288px).
    expect(x2 - x1).toBeGreaterThan(10 * (x1 - x0));
  });

  it("[EVEN-STAYS-EVEN] leaves an unbroken daily run evenly spaced", async () => {
    const cx = await renderChart([
      { date: "2026-08-01T00:00:00Z", count: 4 },
      { date: "2026-08-02T00:00:00Z", count: 2 },
      { date: "2026-08-03T00:00:00Z", count: 7 },
    ]);
    expect(cx).toHaveLength(3);
    const [x0, x1, x2] = cx!;
    // Equal real gaps must stay equal drawn gaps, and the series must still SPAN the chart —
    // together these refuse both "scatter the points" and "pile them on one x".
    expect(x1 - x0).toBeCloseTo(x2 - x1, 6);
    expect(x2 - x0).toBeGreaterThan(500);
  });

  it("[SINGLE-POINT] gives one day a finite coordinate instead of a zero-span divide", async () => {
    const cx = await renderChart([{ date: "2026-08-01T00:00:00Z", count: 3 }]);
    expect(cx).toHaveLength(1);
    expect(Number.isFinite(cx![0])).toBe(true);
    expect(cx![0]).toBe(12); // padX — the same place index spacing put it
  });

  it("[EMPTY-UNCHANGED] still shows the chart's own empty state for no days at all", async () => {
    const cx = await renderChart([]);
    expect(cx).toBeNull(); // no chart svg is rendered at all
    expect(screen.getByText("No data yet.")).toBeInTheDocument();
  });
});

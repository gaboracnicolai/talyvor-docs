import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { PageVersion, VersionCostSplit } from "~/api/types";

// VERSION HISTORY SHOWED PART OF A PAGE'S AI SPEND AND CALLED IT THE WHOLE.
//
// #190 gave each revision a cost and, in the same commit, wrote the function that says how much
// of the page's spend those rows actually account for — with this argument in its own docstring:
//
//	⚠ IT EXISTS BECAUSE A PER-REVISION FIGURE THAT DOES NOT ADD UP IS A LIE ABOUT MONEY THAT
//	LOOKS LIKE A FEATURE. Summing the ai_cost_usd column down a page's version history gives
//	Attributed and nothing else, and a reader has no way to tell that from the page's
//	own_ai_cost_usd.
//
// MEASURED at merged main `f1ad4db`: `Store.VersionCostSplit` had ZERO production callers — no
// handler, no route, no fetch. The reconciliation was guaranteed in Go and unreachable from the
// product, which is the same distance from a reader as not existing.
//
// ⚠ AND BOTH NUMBERS WERE ALREADY ON ONE SCREEN, which is what makes this a render and not a
// report. `pages/PageView.tsx` mounts `<VersionHistory>` at :459 and, directly beneath it, the
// AI-cost panel printing `own_ai_cost_usd`. So a reader saw the revision rows AND the page total,
// and whenever a page carried pending or pre-0021 spend they did not add up, with nothing on
// screen saying why. #190's own fixture gives the size of the silence: "version history shows
// $0.07 of a page that has spent $0.14".
//
// ⚠ WHAT IS ASSERTED IS THE RENDERED TEXT, not that a fetch happened. A component that calls the
// new route and drops the answer before painting is exactly as silent as one that never called
// it — the same reason `versioncost_realpg_test.go` asserts the route's BYTES rather than a store
// return value.

const v = (version: number, ai_cost_usd: number): PageVersion => ({
  id: `ver-${version}`,
  page_id: "pg-1",
  workspace_id: "ws-1",
  version,
  title: `Protocol v${version}`,
  content: `{"body":"${version}"}`,
  created_by: "mem-1",
  created_at: "2026-08-13T10:00:00Z",
  ai_cost_usd,
});

// Mutable so each case installs its own figures; the mock is hoisted and must not close over a
// per-test binding that does not exist yet.
let versions: PageVersion[] = [];
let split: VersionCostSplit = {
  attributed_usd: 0,
  pending_usd: 0,
  unattributable_usd: 0,
  page_total_usd: 0,
};
// ⚠ THE SPLIT MUST BE ABLE TO FAIL IN THIS FILE, AND FOR ONE DRAFT IT COULD NOT. The last case
// below is named "when the split could not be read" and the mock RESOLVED on every path, so it
// exercised the success branch under a failure's name — caught by control C8, which broke the
// rows on a failed split and watched this file stay green.
let splitFails = false;

vi.mock("~/api/pages", () => ({
  pagesApi: {
    versions: vi.fn(async () => versions),
    versionCostSplit: vi.fn(async () => {
      if (splitFails) throw new Error("version-cost read failed");
      return split;
    }),
    diffVersions: vi.fn(async () => ({ from: versions[0], to: versions[0] })),
    restore: vi.fn(async () => ({})),
  },
}));

import { VersionHistory } from "./VersionHistory";

async function renderPanel(): Promise<HTMLElement> {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const { container } = render(
    <QueryClientProvider client={qc}>
      <VersionHistory spaceID="sp-1" pageID="pg-1" />
    </QueryClientProvider>,
  );
  await screen.findByText("v2");
  return container as HTMLElement;
}

beforeEach(() => {
  splitFails = false;
  versions = [v(2, 0.03), v(1, 0.04)];
  // ⚠ EVERY FIGURE IS DISTINCT AS RENDERED, and the first draft's were not — attributed and
  // pending were both 0.07, so "the pending bucket is reported" was satisfied by the string the
  // ATTRIBUTED bucket had already put on screen. Two buckets that print the same characters
  // cannot be told apart by an assertion over text.
  //
  // They also straddle the formatter's two branches on purpose: 0.0042 is below a cent (four
  // decimals) and 0.0311 is above it (two), so a regression that widened or narrowed the whole
  // strip to one branch is visible here rather than in a rounding test alone.
  split = {
    attributed_usd: 0.07, // → "$0.07"   the rows' own sum
    pending_usd: 0.0042, // → "$0.0042" below a cent
    unattributable_usd: 0.0311, // → "$0.03"   above a cent
    page_total_usd: 0.1053, // → "$0.11"   0.07 + 0.0042 + 0.0311
  };
});

describe("version history reconciles what it shows against what the page spent", () => {
  // ⚠ THE CASE THIS FILE EXISTS FOR. The rows sum to $0.07 of a page that has spent $0.11. Before
  // this change the screen said $0.07 and stopped.
  it("says how much of the page's spend its rows do NOT account for", async () => {
    const el = await renderPanel();
    await waitFor(() => {
      expect(el.querySelector("[data-testid='version-cost-reconcile']")).toBeTruthy();
    });
    const text = el.querySelector("[data-testid='version-cost-reconcile']")!.textContent ?? "";
    // The two unshown buckets are named separately, because they are unshown for DIFFERENT
    // reasons and only one of them will ever resolve.
    expect(text, "the pending bucket is not reported").toContain("$0.0042");
    expect(text, "the unattributable bucket is not reported").toContain("$0.03");
    expect(text, "the page's own total is not reported").toContain("$0.11");
    expect(text, "what the rows DO account for is not reported").toContain("$0.07");
  });

  // ⚠ PENDING AND UNATTRIBUTABLE MUST NOT BE MERGED INTO ONE "MISSING" FIGURE. Pending money
  // appears on its revision the moment the next save creates it; unattributable money never will,
  // because the fact was not captured. One number for both would tell a reader that $0.0353 is
  // coming, when $0.0311 of it never will be.
  it("keeps the two unshown buckets apart, because only one of them will ever land", async () => {
    const el = await renderPanel();
    await waitFor(() => {
      expect(el.querySelector("[data-testid='version-cost-reconcile']")).toBeTruthy();
    });
    const text = el.querySelector("[data-testid='version-cost-reconcile']")!.textContent ?? "";
    expect(text, "the two buckets were summed into one figure").not.toContain("$0.0353");
    expect(text.toLowerCase()).toMatch(/next save|not yet exist|does not exist/);
    expect(text.toLowerCase()).toMatch(/never recorded|was not recorded|cannot be/);
  });

  // ⚠ SILENCE WHEN THERE IS NOTHING TO SAY. A page whose rows account for all of its spend must
  // NOT render a reconciliation line: a permanent "nothing is missing" strip is noise, and worse,
  // it trains a reader to skip the region that carries the warning in the case that matters.
  it("says nothing at all when the rows already account for the whole spend", async () => {
    split = { attributed_usd: 0.07, pending_usd: 0, unattributable_usd: 0, page_total_usd: 0.07 };
    const el = await renderPanel();
    // Give the query the same settle time the positive cases get, so this is a measured absence
    // rather than an assertion that ran before the fetch resolved.
    await waitFor(() => expect(el.textContent).toContain("v1"));
    expect(
      el.querySelector("[data-testid='version-cost-reconcile']"),
      "a reconciliation strip rendered for a page with nothing unaccounted for",
    ).toBeNull();
  });

  // ⚠ THE SPLIT FAILING IS NOT THE SAME AS THERE BEING NOTHING UNACCOUNTED FOR, and rendering
  // nothing in both cases would be the quieter of two wrong answers. The rows are still correct
  // and still worth showing, so the list stays; what must not happen is the screen implying the
  // rows are complete when it does not know.
  it("does not claim completeness when the split could not be read", async () => {
    splitFails = true;
    const el = await renderPanel();
    await waitFor(() => expect(el.textContent).toContain("v1"));
    // ⚠ THE ROWS SURVIVE. A side-read that fails must not withdraw a feature that is still
    // correct: the per-revision costs came from a DIFFERENT request, which succeeded.
    expect(
      el.textContent,
      "a failed split read took the version rows down with it",
    ).toContain("v2");
    expect(el.textContent).toContain("$0.03");
    // ⚠ AND NOTHING CLAIMS COMPLETENESS. Rendering the strip from a failed read would state a
    // reconciliation the screen does not have; the honest options were to say so or to stay
    // quiet, and what must never happen is the third one.
    expect(
      el.querySelector("[data-testid='version-cost-reconcile']"),
      "a reconciliation strip rendered from a split that failed to load",
    ).toBeNull();
  });
});

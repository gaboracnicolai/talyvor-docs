import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { Page, Space } from "~/api/types";
import type { ReadStats, WorkspaceReadStats } from "~/api/analytics";

// THE SPACE ROLL-UP HAS TO REACH A READER, WHICH IS THE HALF THE SERVER CANNOT DO.
//
// talyvor.higgsfield.app/products/docs sells "PAGE, SPACE AND ORG ROLLUPS". The org scope has had
// a screen since Analytics.tsx existed and the page scope has one on the page itself; the SPACE
// scope had neither a route nor a surface until this change.
//
// ⚠ AND A ROUTE WITHOUT A SURFACE IS THE DEFECT THIS SESSION HAS ALREADY FIXED ONCE TODAY, one
// merge earlier: #190 shipped `Store.VersionCostSplit` with a correct query, a real-Postgres
// guard and eleven of its own controls, and no caller — a reconciliation guaranteed in Go and
// unreachable from the product. Serving the space roll-up and stopping would repeat it in the
// same repo on the same afternoon. So the section is rendered, and this file asserts the rendered
// text rather than that a fetch happened.

const space: Space = {
  id: "sp-1",
  workspace_id: "ws-1",
  name: "Engineering",
  slug: "engineering",
  description: "How we build",
  icon: "📘",
  color: "#0B7A85",
  private: false,
  created_by: "m-1",
  created_at: "2026-06-01T00:00:00Z",
  updated_at: "2026-07-01T00:00:00Z",
} as Space;

const pages: Page[] = [];

/** A ranked row in the shape the server actually sends — every required field, no casts. */
function row(page_id: string, title: string, total_views: number): ReadStats {
  return {
    page_id,
    title,
    total_views,
    unique_viewers: 1,
    avg_duration_sec: 10,
    views_by_day: [],
    top_viewers: [],
  };
}

let stats: WorkspaceReadStats = {
  total_views: 0,
  unique_viewers: 0,
  most_read_pages: [],
  least_read_pages: [],
  never_read_count: 0,
};
// ⚠ THE READ MUST BE ABLE TO FAIL HERE. A section whose only fixture succeeds cannot distinguish
// "this space has no readership" from "the roll-up could not be read", and those are the two
// states a zero is ambiguous between.
let statsFails = false;

vi.mock("~/api/analytics", async (orig) => ({
  ...(await orig<Record<string, unknown>>()),
  analyticsApi: {
    spaceStats: vi.fn(async () => {
      if (statsFails) throw new Error("space roll-up read failed");
      return stats;
    }),
  },
}));

vi.mock("~/hooks/usePage", () => ({
  usePages: vi.fn(() => ({ data: pages, isLoading: false })),
  useCreatePage: vi.fn(() => ({ mutate: vi.fn() })),
}));

import { SpaceViewPage } from "./SpaceView";

async function renderSpace(): Promise<HTMLElement> {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const { container } = render(
    <QueryClientProvider client={qc}>
      <SpaceViewPage space={space} onOpenPage={() => {}} />
    </QueryClientProvider>,
  );
  await screen.findByText("Engineering");
  return container as HTMLElement;
}

beforeEach(() => {
  statsFails = false;
  stats = {
    total_views: 128,
    unique_viewers: 17,
    // ⚠ THE FIELD IS `total_views`, AND THE FIRST DRAFT OF THIS FIXTURE SAID `view_count`.
    // It was written with an `as` cast, so vitest ran it happily and all three cases went green
    // against a shape the server never sends — `tsc --noEmit` is what said so. `view_count` IS a
    // real key in this API, on `ViewerStat`, which is why it reads as plausible. The casts are
    // gone: a fixture that has to be cast into its own type is asserting against a shape nobody
    // serves.
    most_read_pages: [
      row("pg-1", "Rollback runbook", 90),
      row("pg-2", "On-call rota", 38),
    ],
    least_read_pages: [row("pg-2", "On-call rota", 38)],
    never_read_count: 4,
  };
});

describe("the space view reports its own readership", () => {
  it("shows the space's roll-up figures", async () => {
    const el = await renderSpace();
    await waitFor(() => {
      expect(el.querySelector("[data-testid='space-rollup']")).toBeTruthy();
    });
    const text = el.querySelector("[data-testid='space-rollup']")!.textContent ?? "";
    expect(text, "total views are not reported").toContain("128");
    expect(text, "unique visitors are not reported").toContain("17");
    expect(text, "the never-read count is not reported").toContain("4");
    expect(text, "the most-read page is not named").toContain("Rollback runbook");
  });

  // ⚠ THE FIGURES ARE ABOUT THIS SPACE AND THE LABEL MUST SAY SO. The identical component shape
  // renders the ORG roll-up on Analytics.tsx, and the same four numbers under an unqualified
  // heading would read as the workspace's — which is exactly the confusion the server-side scope
  // exists to prevent, reintroduced in words. `spacerollup_realpg_test.go` holds the numbers; this
  // holds what the screen says they are about.
  it("says the figures are the SPACE's, not the workspace's", async () => {
    const el = await renderSpace();
    await waitFor(() => {
      expect(el.querySelector("[data-testid='space-rollup']")).toBeTruthy();
    });
    const text = (el.querySelector("[data-testid='space-rollup']")!.textContent ?? "").toLowerCase();
    expect(text, "the section does not say which scope these figures describe").toMatch(
      /in this space|this space/,
    );
  });

  // ⚠ ZERO READERSHIP AND A FAILED READ MUST NOT LOOK THE SAME. A space nobody has opened is a
  // real, ordinary answer; a roll-up that could not be read is not an answer at all. Rendering
  // `0 views` for both states states a fact the screen does not have — the same distinction
  // `search.Result` draws for its three cost fields, and the reason they carry `omitempty`.
  it("a failed read is not reported as zero readership", async () => {
    statsFails = true;
    const el = await renderSpace();
    await waitFor(() => expect(el.textContent).toContain("Engineering"));
    const strip = el.querySelector("[data-testid='space-rollup']");
    if (strip) {
      const text = (strip.textContent ?? "").toLowerCase();
      expect(
        text,
        "a roll-up that could not be read rendered as a space with no readership",
      ).not.toMatch(/\b0\b/);
    }
    // The page list is unaffected: a failed side-read does not withdraw the rest of the screen.
    expect(el.textContent).toContain("Pages");
  });
});

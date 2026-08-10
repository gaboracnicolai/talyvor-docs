import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";

// THE PAGE SCREEN PRINTED ONE ADDEND AND A SENTENCE THAT NAMED THE SUM.
//
// A page carries two independent costs and a derived sum (migration 0018):
//
//   ai_cost_usd        the cost of the Track ISSUES linked to this page
//   own_ai_cost_usd    the cost of AI operations performed ON this document
//   total_ai_cost_usd  their sum, derived on read
//
// `b4f0b77` (#55) and `8b3e1be` (#54) made the two BACKEND emitters honest. This is the third
// copy of that seam and the only one where a false sentence is printed next to the number.
// PageView.tsx rendered, verbatim:
//
//   {page.ai_cost_usd > 0 ? ( <PanelSection title="AI cost">
//     ✨ AI writing cost: ${page.ai_cost_usd.toFixed(2)}
//     Includes Lens writing + Track implementation spend.
//
// THE PROSE STATES THE SUM AND THE VALUE IS ONE ADDEND. "AI WRITING cost" is the name of the
// OTHER addend (own_ai_cost_usd), and "Includes Lens writing + Track implementation spend" is the
// definition of total_ai_cost_usd — which exists precisely so a caller does not add the two
// itself and get it subtly wrong (model.go).
//
// ⚠ AND THE GATE INVERTS IT THE SAME WAY #54's `omitempty` DID. The panel was conditioned on
// `ai_cost_usd > 0` — THE TRACK HALF — so a document funded entirely by its own AI writing showed
// NO AI-COST PANEL AT ALL. The panel titled "AI writing cost" was hidden from exactly the document
// that had one. That is case A below and it is why this is a guard and not a copy-edit.
//
// ⚠ THE DATA WAS ALREADY ON THE WIRE — THIS IS A RENDERER FIX, NOT AN API ONE, AND IT IS MEASURED:
// GET /v1/.../pages/{pageID} (page/handler.go Get) writes *model.Page DIRECTLY through writeJSON,
// read via GetByIDInWorkspaces -> page.scan -> withDerivedTotal, so all three fields were
// serialised to this SPA already. api/types.ts declared `ai_cost_usd` and NEITHER of the other
// two: they arrived and were dropped at the TYPE boundary, which is why nothing here was red.

vi.mock("~/components/editor/Editor", () => ({ Editor: () => <div data-testid="editor" /> }));
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

const costs = { own: 0, track: 0 };
// `stale` models a record cached before own/total shipped: the two keys are ABSENT, not zero.
let stale = false;
const pageRef = () => ({
  id: "p1",
  workspace_id: "w1",
  space_id: "s1",
  title: "Doc",
  content: "{}",
  content_text: "hi there",
  doc_status: "draft",
  icon: "📄",
  view_count: 0,
  ai_cost_usd: costs.track,
  ...(stale ? {} : { own_ai_cost_usd: costs.own, total_ai_cost_usd: costs.track + costs.own }),
  created_by: "alice",
  updated_by: "alice",
  updated_at: "2026-01-01T00:00:00Z",
  parent_id: null,
  page_type: "document",
  last_verified_at: null,
});
vi.mock("~/hooks/usePage", () => ({
  usePage: () => ({ data: pageRef(), isLoading: false }),
  useUpdatePage: () => ({ mutate: vi.fn(), isPending: false }),
}));
vi.mock("~/hooks/usePageLock", () => ({
  usePageLock: () => ({
    state: undefined,
    lockedByMe: false,
    lock: { mutate: vi.fn(), isPending: false },
    unlock: { mutate: vi.fn(), isPending: false },
  }),
}));
vi.mock("~/api/editsession", () => ({
  editSessionApi: {
    get: vi.fn().mockResolvedValue(null),
    acquire: vi.fn(),
    heartbeat: vi.fn(),
    release: vi.fn(),
    takeover: vi.fn(),
  },
}));
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
vi.mock("~/api/links", () => ({
  linksApi: { list: vi.fn().mockResolvedValue([]), create: vi.fn(), remove: vi.fn() },
}));

import { PageViewPage } from "./PageView";

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
  stale = false;
});

describe("PageView reports what a document actually cost", () => {
  // D carries BOTH halves non-zero and is what holds the headline to being a SUM rather than a
  // copy of either addend: with A/B/C alone, "show own" and "show track" are each right on two of
  // the three.
  const cases = [
    { name: "A own-funded", own: 12.34, track: 0, total: "12.34" },
    { name: "B track-funded", own: 0, track: 1.5, total: "1.50" },
    { name: "D both halves", own: 12.34, track: 1.5, total: "13.84" },
  ];

  for (const c of cases) {
    it(`${c.name}: the panel is shown and the headline is the TOTAL ($${c.total})`, () => {
      costs.own = c.own;
      costs.track = c.track;
      renderPage();

      // The panel must EXIST. For case A it did not: the gate read the Track half, so the
      // document whose whole spend was its own AI writing showed no AI-cost panel at all.
      const panel = screen.queryByTestId("ai-cost-panel");
      expect(
        panel,
        `no AI-cost panel rendered for a document that cost $${c.total} ` +
          `(own ${c.own} + track ${c.track}). The panel gate is reading one addend.`,
      ).not.toBeNull();

      // The headline number must be the TOTAL, not either half.
      expect(
        screen.getByTestId("ai-cost-total").textContent,
        `the headline AI cost is not the total for own ${c.own} + track ${c.track}`,
      ).toContain(`$${c.total}`);

      // The sentence claims a composition; the breakdown is what makes that claim checkable
      // rather than asserted, so each addend must be readable on its own.
      expect(screen.getByTestId("ai-cost-own").textContent).toContain(`$${c.own.toFixed(2)}`);
      expect(screen.getByTestId("ai-cost-track").textContent).toContain(`$${c.track.toFixed(2)}`);
    });
  }

  it("C genuinely free: no panel — a document that cost nothing must not grow one", () => {
    costs.own = 0;
    costs.track = 0;
    renderPage();
    // ONE-DIRECTIONAL AND DELIBERATE. Without this, "always show the panel" passes every case
    // above, and the fix for an inverted gate would be no gate at all.
    expect(screen.queryByTestId("ai-cost-panel")).toBeNull();
  });

  // ⚠ EARNED BY A REGRESSION THIS FIX ALMOST SHIPPED, NOT BY IMAGINATION. api/client.ts persists
  // page GETs to IndexedDB and serves them back when fetch REJECTS, so a record cached before
  // these fields existed is a real Page this component gets handed on the offline path. The first
  // version of this fix read `page.total_ai_cost_usd.toFixed(2)` directly and crashed the entire
  // page view on exactly that record — measured, not feared: it is what reddened four tests in
  // PageView.editsession.test.tsx, whose fixture has that shape.
  it("a pre-0018 cached page (no own/total) renders instead of crashing, degraded to the old view", () => {
    costs.own = 0;
    costs.track = 1.5;
    stale = true;
    renderPage();
    expect(screen.getByTestId("ai-cost-total").textContent).toContain("$1.50");
    expect(screen.getByTestId("ai-cost-track").textContent).toContain("$1.50");
    expect(screen.getByTestId("ai-cost-own").textContent).toContain("$0.00");
  });

  it("Page info shows the total, not the Track half", () => {
    costs.own = 12.34;
    costs.track = 1.5;
    renderPage();
    expect(screen.getByTestId("page-info-ai-cost").textContent).toContain("$13.84");
  });
});

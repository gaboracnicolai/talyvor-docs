import { beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";

// B18.40 — "My approvals" and the sidebar's approvals badge work for the signed-in user. Both were
// gated on `docs_member_id`, a localStorage key nothing writes, so neither ever showed anything;
// the server already derives the reviewer from the verified membership. This test sets no such key.

const pending = vi.fn();
vi.mock("~/api/approval", () => ({
  approvalApi: {
    pending: (...a: unknown[]) => pending(...a),
    decide: vi.fn().mockResolvedValue({ ok: true }),
  },
}));
vi.mock("~/api/freshness", () => ({ freshnessApi: { forWorkspace: vi.fn().mockResolvedValue([]) } }));
vi.mock("~/hooks/useSpaces", () => ({
  useSpaces: () => ({ data: [], isLoading: false }),
  useCreateSpace: () => ({ mutate: vi.fn() }),
}));
vi.mock("~/hooks/usePage", () => ({ usePages: () => ({ data: [], isLoading: false }) }));

const ROW = {
  id: "req-1",
  page_id: "pg-77",
  space_id: "sp-42",
  workspace_id: "ws-test",
  requested_by: "mbr-author",
  reviewers: ["mbr-me"],
  message: "",
  status: "pending",
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
};

function withQuery(ui: ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

beforeEach(() => {
  cleanup();
  localStorage.clear();
  pending.mockReset();
  pending.mockResolvedValue([ROW]);
});

describe("a pending approval reaches the signed-in reviewer", () => {
  it("shows in My approvals and in the sidebar badge, with nothing set in the browser", async () => {
    const { ApprovalInboxPage } = await import("./ApprovalInbox");
    withQuery(<ApprovalInboxPage workspaceID="ws-test" onOpenPage={() => {}} />);
    expect(await screen.findByRole("button", { name: /pg-77/ })).toBeInTheDocument();
    expect(pending).toHaveBeenCalledWith("ws-test");
    cleanup();

    const { Sidebar } = await import("~/components/layout/Sidebar");
    const noop = () => {};
    withQuery(
      <Sidebar
        onHome={noop}
        onOpenSpace={noop}
        onOpenPage={noop}
        onOpenAnalytics={noop}
        onOpenStale={noop}
        onOpenTemplates={noop}
        onOpenApprovals={noop}
        onOpenDomains={noop}
        workspaceID="ws-test"
        activeSpaceID={null}
        activePageID={null}
      />,
    );
    const approvals = await screen.findByRole("button", { name: /Approvals/ });
    expect(await within(approvals).findByText("1")).toBeInTheDocument();
  });
});

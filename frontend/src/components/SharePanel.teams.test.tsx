import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { Permission } from "~/api/permissions";
import type { Team } from "~/api/teams";

// B28.446 — the share panel shares a page with a team, and shows a team grant by the team's name.

const design: Team = {
  id: "team-design",
  name: "Design",
  created_by: "mbr_alice",
  created_at: "2026-10-07T00:00:00Z",
  members: ["mbr_bob"],
  can_manage: true,
};

const grants: Permission[] = [];
const grantPage = vi.fn(async (_s: string, _p: string, body: Omit<Permission, "id">) => {
  const row = { ...body, id: "perm-1" } as Permission;
  grants.push(row);
  return row;
});

vi.mock("~/api/permissions", () => ({
  permissionsApi: {
    listPage: async () => [...grants],
    grantPage: (s: string, p: string, b: Omit<Permission, "id">) => grantPage(s, p, b),
    revokePage: async () => ({ ok: true }),
  },
}));
vi.mock("~/api/sharing", () => ({ sharingApi: { list: async () => [] } }));
vi.mock("~/api/teams", () => ({ teamsApi: { list: async () => [design] } }));

describe("SharePanel teams", () => {
  it("shares the page with a team and lists the grant under the team's name", async () => {
    const { SharePanel } = await import("./SharePanel");
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <SharePanel spaceID="sp-1" pageID="pg-1" workspaceID="ws-1" spacePrivate open onClose={() => {}} />
      </QueryClientProvider>,
    );

    const team = await screen.findByRole("option", { name: "Design" });
    await userEvent.selectOptions(screen.getByLabelText("Team"), team);
    await userEvent.selectOptions(screen.getByLabelText("Team access"), "edit");
    const section = screen.getByText("Share with a team").closest("section")!;
    await userEvent.click(section.querySelector("button")!);

    await waitFor(() => expect(grantPage).toHaveBeenCalledOnce());
    expect(grantPage.mock.calls[0][2]).toMatchObject({
      subject_type: "team",
      subject_id: "team-design",
      access: "edit",
    });
    // The grant row names the team rather than its id.
    await waitFor(() =>
      expect(screen.getAllByText("Design").some((el) => el.closest("li")?.textContent?.includes("Can edit"))).toBe(
        true,
      ),
    );
  });
});

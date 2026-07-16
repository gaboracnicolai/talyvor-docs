import { describe, expect, it } from "vitest";
import { sessionFlags } from "./useEditSession";
import type { EditSession } from "~/api/editsession";

// sessionFlags is the pure rule the whole single-writer UI gates on — headless-correct, so it
// gets a real test. Only a LIVE session held by someone constrains anyone.
const live = (holder: string): EditSession => ({
  page_id: "p1",
  workspace_id: "w1",
  holder,
  acquired_at: "2026-01-01T00:00:00Z",
  last_heartbeat: "2026-01-01T00:00:00Z",
  live: true,
});

describe("sessionFlags", () => {
  it("no session → nothing held (page open to all)", () => {
    expect(sessionFlags(null, "me")).toEqual({
      holder: null,
      live: false,
      heldByMe: false,
      heldByOther: false,
    });
    expect(sessionFlags(undefined, "me")).toMatchObject({ heldByMe: false, heldByOther: false });
  });

  it("live session held by me → heldByMe, not heldByOther", () => {
    const f = sessionFlags(live("me"), "me");
    expect(f).toMatchObject({ holder: "me", live: true, heldByMe: true, heldByOther: false });
  });

  it("live session held by someone else → heldByOther, not heldByMe", () => {
    const f = sessionFlags(live("alice"), "me");
    expect(f).toMatchObject({ holder: "alice", live: true, heldByMe: false, heldByOther: true });
  });

  it("EXPIRED session (live=false) constrains nobody — page is claimable", () => {
    const expired = { ...live("alice"), live: false };
    const f = sessionFlags(expired, "me");
    expect(f).toMatchObject({ heldByMe: false, heldByOther: false });
  });

  it("empty memberID never counts as the holder", () => {
    expect(sessionFlags(live(""), "")).toMatchObject({ heldByMe: false });
  });
});

import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { EditingBanner } from "./EditingBanner";
import { sessionFlags } from "~/hooks/useEditSession";

// THE "TAKE OVER" BUTTON IS ON SCREEN IN EXACTLY THE STATE THE TAKEOVER ROUTE REFUSES, AND OFF
// SCREEN IN THE STATE IT ACCEPTS.
//
// The backend half is measured on real Postgres in
// internal/editsession/takeoverparity_realpg_test.go: `Store.Takeover` is `return s.claim(...)`,
// the same call `Acquire` makes, so a LIVE foreign session gets 423 from BOTH doors and an
// EXPIRED one gets 200 from both. This file pins the client half of the same predicate, in the
// only place it is decidable without a browser:
//
//	EditingBanner renders    ⟺ flags.heldByOther
//	flags.heldByOther        ⟺ live && holder && holder !== me      (sessionFlags)
//	the route answers 423    ⟺ a LIVE session held by someone else  (claim's UPSERT WHERE)
//
// Same field, same condition. So the button appears when it cannot work and is removed — the whole
// banner unmounts — within one poll (`refetchInterval: 10_000`) of becoming usable, at which point
// the page is editable through ordinary auto-acquire and no button was needed.
//
// ⚠ THESE ARE NOT ASSERTIONS THAT THE BEHAVIOUR IS RIGHT. Whether "Take over" should steal a live
// session is a product decision (see the Go file's header). They exist so that the day someone
// changes either side, the OTHER side reds and the two halves cannot drift apart silently — which
// is exactly how the affordance came to promise something the route never granted.
//
// ⚠ AND THEY DELIBERATELY DO NOT PIN THE TOOLTIP OR BUTTON COPY. Wording is a product's to change;
// what is pinned is the RELATIONSHIP between the render predicate and the refusal predicate.

const holderName = "mbr_someone_else";
const me = "mbr_me";

describe("the Take over affordance and the state it can succeed in", () => {
  it("[SHOWN-WHEN-REFUSED] renders the button for a LIVE session held by someone else — the one state the route answers 423 in", () => {
    const flags = sessionFlags({ holder: holderName, live: true } as never, me);
    expect(flags.heldByOther).toBe(true);
    render(<EditingBanner flags={flags} onTakeover={() => {}} />);
    expect(screen.getByRole("button", { name: /take over/i })).toBeTruthy();
  });

  it("[GONE-WHEN-GRANTED] renders NOTHING once the session has expired — the one state the route answers 200 in", () => {
    const flags = sessionFlags({ holder: holderName, live: false } as never, me);
    expect(flags.heldByOther).toBe(false);
    const { container } = render(<EditingBanner flags={flags} onTakeover={() => {}} />);
    expect(container.firstChild).toBeNull();
    expect(screen.queryByRole("button", { name: /take over/i })).toBeNull();
  });

  it("[ONE-PREDICATE] heldByOther is false for every non-live session, whoever holds it — so no expired state can put the button on screen", () => {
    for (const holder of [holderName, me, null]) {
      const flags = sessionFlags({ holder, live: false } as never, me);
      expect(flags.heldByOther, `holder=${String(holder)}`).toBe(false);
    }
    // …and the live/foreign case is the ONLY one that is true, which is what makes the two
    // predicates the same predicate rather than merely overlapping.
    expect(sessionFlags({ holder: me, live: true } as never, me).heldByOther).toBe(false);
    expect(sessionFlags({ holder: holderName, live: true } as never, me).heldByOther).toBe(true);
  });
});

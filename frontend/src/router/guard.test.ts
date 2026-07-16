import { describe, expect, it } from "vitest";
import { APIError } from "~/api/client";
import { resourceState } from "./guard";

// resourceState is the SECURITY-adjacent core of the router. When a deep URL addresses a
// resource, the route fetches it and this maps the fetch to a view state. The one property
// that matters: the router must not become an existence oracle. The server already answers
// 404 for a cross-tenant resource and 403 for an in-tenant-but-unauthorized one; if the UI
// rendered those differently, an attacker rotating page ids could tell "exists but you
// can't see it" from "doesn't exist". So 403 and 404 MUST collapse to the same state with
// the same copy.

type Q = { isLoading: boolean; isError: boolean; error: unknown };
const q = (over: Partial<Q>): Q => ({ isLoading: false, isError: false, error: null, ...over });

describe("resourceState", () => {
  it("is loading while the query is in flight", () => {
    expect(resourceState(q({ isLoading: true }))).toBe("loading");
  });

  it("is ready on success", () => {
    expect(resourceState(q({}))).toBe("ready");
  });

  // THE SECURITY PROPERTY.
  it("collapses 403 and 404 to the SAME notfound state — no existence oracle", () => {
    const forbidden = resourceState(q({ isError: true, error: new APIError("forbidden", 403) }));
    const missing = resourceState(q({ isError: true, error: new APIError("not found", 404) }));
    expect(forbidden).toBe("notfound");
    expect(missing).toBe("notfound");
    expect(forbidden).toBe(missing); // stated explicitly: they are indistinguishable
  });

  it("surfaces offline distinctly (it is not an existence signal)", () => {
    expect(resourceState(q({ isError: true, error: new APIError("offline", 0, "OFFLINE") }))).toBe(
      "offline",
    );
  });

  // A 500 is a genuine server fault, not an existence signal — the server returns 404, not
  // 500, for a resource you may not see — so a distinct "error" state is not an oracle and
  // is more honest than mislabelling it "not found".
  it("maps a server error (500) to a distinct error state", () => {
    expect(resourceState(q({ isError: true, error: new APIError("boom", 500) }))).toBe("error");
  });

  // Fail safe: an error we cannot classify must never imply the resource EXISTS. Anything
  // unrecognised collapses into notfound rather than leaking through as ready/error-with-detail.
  it("treats an unclassifiable error as notfound, never as ready", () => {
    expect(resourceState(q({ isError: true, error: new Error("weird") }))).toBe("notfound");
    expect(resourceState(q({ isError: true, error: "a string" }))).toBe("notfound");
    expect(resourceState(q({ isError: true, error: null }))).toBe("notfound");
  });
});

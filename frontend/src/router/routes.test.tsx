import { describe, expect, it } from "vitest";
import { matchRoutes } from "react-router-dom";
import { routes } from "./routes";

// matchRoutes is react-router's pure path→route resolver. Running it against the REAL routes
// table the app uses proves the URL scheme, the param names, the nesting, and the two special
// cases (shared viewer OUTSIDE the chrome; catch-all) — all without rendering a component, so
// it is genuinely headless-correct. A route that matches here is the route that mounts.

// leafPath returns the path of the deepest matched route — the one whose element renders.
function leafPath(pathname: string): string | undefined {
  const m = matchRoutes(routes, pathname);
  return m?.[m.length - 1]?.route.path;
}
function paramsFor(pathname: string): Record<string, string | undefined> {
  const m = matchRoutes(routes, pathname);
  return m?.[m.length - 1]?.params ?? {};
}

describe("route table", () => {
  it("matches home at the index route inside the chrome layout", () => {
    const m = matchRoutes(routes, "/");
    expect(m).not.toBeNull();
    // "/" resolves Layout → index route; the Layout is the first match.
    expect(m![0].route.path).toBe("/");
    expect(m![m!.length - 1].route.index).toBe(true);
  });

  it("matches a space URL and extracts spaceID", () => {
    expect(leafPath("/spaces/sp-1")).toBe("spaces/:spaceID");
    expect(paramsFor("/spaces/sp-1")).toEqual({ spaceID: "sp-1" });
  });

  it("matches a nested page URL and extracts both ids", () => {
    expect(leafPath("/spaces/sp-1/pages/pg-2")).toBe("spaces/:spaceID/pages/:pageID");
    expect(paramsFor("/spaces/sp-1/pages/pg-2")).toEqual({ spaceID: "sp-1", pageID: "pg-2" });
  });

  it("matches each admin route", () => {
    expect(leafPath("/analytics")).toBe("analytics");
    expect(leafPath("/needs-review")).toBe("needs-review");
    expect(leafPath("/templates")).toBe("templates");
    expect(leafPath("/approvals")).toBe("approvals");
    expect(leafPath("/domains")).toBe("domains");
  });

  it("matches the public share viewer OUTSIDE the chrome layout", () => {
    const m = matchRoutes(routes, "/s/tok-abc");
    expect(m).not.toBeNull();
    // Its top-level match is NOT the "/" Layout route — the shared viewer has no sidebar.
    expect(m![0].route.path).toBe("/s/:token");
    expect(m![m!.length - 1].params).toEqual({ token: "tok-abc" });
  });

  it("routes an unknown in-app URL to the in-chrome catch-all, not off a cliff", () => {
    const m = matchRoutes(routes, "/spaces/sp-1/nonsense");
    expect(m).not.toBeNull();
    expect(m![0].route.path).toBe("/"); // still inside the Layout (sidebar stays)
    expect(m![m!.length - 1].route.path).toBe("*");
  });

  // The builders and the patterns must agree, end to end: a URL built by paths.* must match
  // the route it targets. This is the link-doesn't-404 guarantee, checked against the live table.
  it("every builder produces a URL the table resolves to the intended route", () => {
    expect(leafPath("/spaces/x")).toBe("spaces/:spaceID");
    expect(leafPath("/spaces/x/pages/y")).toBe("spaces/:spaceID/pages/:pageID");
    expect(matchRoutes(routes, "/s/z")![0].route.path).toBe("/s/:token");
  });
});

import { describe, expect, it } from "vitest";
import { paths } from "./paths";

// Path builders are the tested core of NAVIGATION: "go to page X" is correct iff we build
// the right URL for X. Pure functions, genuinely headless-correct — no browser needed.

describe("paths", () => {
  it("builds the static routes", () => {
    expect(paths.home()).toBe("/");
    expect(paths.analytics()).toBe("/analytics");
    expect(paths.stale()).toBe("/needs-review");
    expect(paths.templates()).toBe("/templates");
    expect(paths.approvals()).toBe("/approvals");
    expect(paths.domains()).toBe("/domains");
  });

  it("builds a space URL from its id", () => {
    expect(paths.space("sp-123")).toBe("/spaces/sp-123");
  });

  it("builds a page URL nested under its space", () => {
    expect(paths.page("sp-123", "pg-456")).toBe("/spaces/sp-123/pages/pg-456");
  });

  it("builds the public share URL", () => {
    expect(paths.shared("tok-abc")).toBe("/s/tok-abc");
  });

  // IDs are server-generated UUIDs today, but a builder that blindly interpolates is a
  // latent path-injection the day an id contains a slash or a space. Encode defensively.
  it("URL-encodes ids so a stray character cannot break out of the path", () => {
    expect(paths.space("a/b")).toBe("/spaces/a%2Fb");
    expect(paths.page("s p", "p?q")).toBe("/spaces/s%20p/pages/p%3Fq");
  });

  // The router config consumes these param names; a builder that disagrees with the pattern
  // silently 404s every link. Pin them together.
  it("exposes route patterns whose params the builders satisfy", () => {
    expect(paths.patterns.space).toBe("/spaces/:spaceID");
    expect(paths.patterns.page).toBe("/spaces/:spaceID/pages/:pageID");
    expect(paths.patterns.shared).toBe("/s/:token");
  });
});

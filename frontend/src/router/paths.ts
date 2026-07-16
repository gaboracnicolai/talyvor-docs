// URL builders + route patterns, in one place so a link and the route it targets can never
// drift apart. Pure functions — the tested core of navigation.
//
// The scheme is the one the codebase already implied before any router existed:
// pushRecentPage stamped `/spaces/:s/pages/:p`, and nginx.conf's SPA fallback names
// `/spaces/:id, /s/:token`. We formalise exactly that.

const enc = encodeURIComponent;

export const paths = {
  home: () => "/",
  space: (spaceID: string) => `/spaces/${enc(spaceID)}`,
  page: (spaceID: string, pageID: string) => `/spaces/${enc(spaceID)}/pages/${enc(pageID)}`,
  analytics: () => "/analytics",
  stale: () => "/needs-review",
  templates: () => "/templates",
  approvals: () => "/approvals",
  domains: () => "/domains",
  shared: (token: string) => `/s/${enc(token)}`,

  // The patterns the router config registers. Kept beside the builders so the test can pin
  // that `space(id)` fills `/spaces/:spaceID` and not, say, `/spaces/:id` — a mismatch would
  // silently 404 every space link.
  patterns: {
    space: "/spaces/:spaceID",
    page: "/spaces/:spaceID/pages/:pageID",
    shared: "/s/:token",
  },
} as const;

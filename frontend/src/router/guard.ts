// The router's security-adjacent core: map a resource fetch to a view state without
// creating an existence oracle.
//
// The frontend has NO independent authorization knowledge and must not invent any — it
// surfaces exactly what the API returns (that is the tenancy guarantee). The one rule it
// enforces on top: a resource the caller may not see and a resource that does not exist must
// look identical. The server already makes them identical at the API layer (404 for
// cross-tenant, and it never leaks existence); the router must preserve that at the view
// layer, or an attacker rotating ids in the URL learns which ones are real.

import { APIError } from "~/api/client";

export type ResourceState = "loading" | "ready" | "notfound" | "offline" | "error";

// A minimal shape of a TanStack Query result — kept tiny so the guard is a pure function
// testable without React.
export interface ResourceQuery {
  isLoading: boolean;
  isError: boolean;
  error: unknown;
}

export function resourceState(q: ResourceQuery): ResourceState {
  if (q.isLoading) return "loading";
  if (!q.isError) return "ready";

  if (q.error instanceof APIError) {
    // 403 (in-tenant, unauthorized) and 404 (cross-tenant / absent) are DELIBERATELY the
    // same. Do not split them — that split is the oracle.
    if (q.error.status === 403 || q.error.status === 404) return "notfound";
    // status 0 / code OFFLINE is a connectivity condition, not an existence signal.
    if (q.error.status === 0 || q.error.code === "OFFLINE") return "offline";
    // A real server fault (5xx). Not an existence signal, so a distinct state is honest and
    // not an oracle.
    if (q.error.status >= 500) return "error";
  }

  // Anything we cannot classify: fail safe toward notfound. Never fall through to "ready"
  // (would render a resource we failed to load) and never invent an existence-revealing
  // distinction.
  return "notfound";
}

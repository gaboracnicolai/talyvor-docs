import { describe, expect, it } from "vitest";

import { goRoutes, handlerFacts, webSites } from "./requestSurface";

/**
 * EVERY FIELD AND QUERY KEY THIS SPA SENDS IS ONE THE HANDLER ON THE OTHER END ACTUALLY READS.
 *
 * `route-response-type.census.test.ts` pins what comes BACK. Nothing pinned what goes OUT, and the
 * estate has now shipped this defect twice in its other two client/server pairs:
 *
 *   talyvor-track (W3.68, `8359a30`) — `DisallowUnknownFields` turned a retired `member_id` the
 *     SPA still sent into a 400. Starting a timer and logging time were DEAD in the shipped UI.
 *   talyvor-suite (W6.49, `df2d105`) — the BFF decodes an `expires_at` nothing sends, so every API
 *     key minted through the console is non-expiring while every layer beneath enforces expiry.
 *
 * ⚠ HERE IT IS SILENT, WHICH IS THE WORSE HALF. Measured before this file was written:
 * `grep -rn DisallowUnknownFields internal/ cmd/` returns ZERO in this repository. So an extra body
 * field is decoded away and answered 200, and an extra query parameter returns the UNFILTERED
 * result. There is no 400 for anyone to report.
 *
 * ── WHAT IT FOUND AT d1bfd11, AND ALL THREE ARE THE SAME SHAPE ────────────────
 *
 * Three surfaces still send a parameter the server RETIRED FOR A SECURITY REASON, and each
 * handler's own comment says so:
 *
 *   1. GET /v1/workspaces/{}/approvals/pending?reviewer_id=…
 *      `internal/approval/handler.go Pending`: *"this route was ungated, took the reviewer from the
 *      QUERY STRING, and PREFERRED that value over the verified actor — so any member read another
 *      member's pending-approval queue with ?reviewer_id=victim."* The reviewer now comes from the
 *      authorized membership. The SPA still sends it, so `approvalApi.pending(ws, reviewerID)`
 *      advertised a capability the server refuses to have.
 *
 *   2. POST + DELETE /v1/spaces/{}/pages/{}/lock  { member_id, is_admin }
 *      `internal/pagelock/handler.go`: *"Both member_id and is_admin used to live here and both
 *      were forgeable… The request body is not read at all."* The SPA still sent both — including
 *      an `is_admin` flag, which reads like a privilege claim and has never been one.
 *
 *   3. POST /v1/spaces/{}/pages/{}/view  { viewer_id, workspace_id }
 *      `internal/analytics/handler.go RecordView`: *"viewer_id was not overridden at all, and it
 *      feeds COUNT(DISTINCT viewer_id) / GROUP BY viewer_id — so the body could forge who read a
 *      page."* Both are derived from the verified caller now; the SPA still sent both, and its
 *      `viewer_id` was read from `docs_member_id`, a localStorage key NOTHING IN THIS SPA WRITES.
 *
 * All four wires are removed in the same change. ⚠ AND ONE CONSEQUENCE IS NOT REMOVED AND MUST NOT
 * BE READ AS FIXED: `Sidebar.tsx` and `ApprovalInbox.tsx` still gate their queries on
 * `enabled: !!reviewerID` where `reviewerID` is that same never-written localStorage key, so the
 * pending-approvals badge and the whole "My approvals" screen are dark for every user while the
 * route behind them works. Removing a gate turns a dark surface ON, which is a product decision —
 * it is filed with its evidence rather than taken here.
 *
 * ── FLOORS, AND WHY EACH ONE EXISTS ───────────────────────────────────────────
 *
 * Both halves are source parses, so the failure mode is a parser that quietly matches less and
 * reports agreement over a question it never asked. Every floor below is a count this parse got
 * WRONG at some point while being written; the Go route walk alone was blind five separate ways.
 */

const MIN_WEB_SITES = 80; // 89 measured at d1bfd11 — and independently `grep -c apiRequest[<(]` = 89
const MIN_ROUTES = 95; // 103 measured
const MIN_JOINED = 80; // 89 of 89 join; floored under, not at
const MIN_QUERY_SITES = 10; // 12 measured
const MIN_BODY_JOINED = 25; // 32 measured

/**
 * Routes whose decode target is a map — every key decodes, so no field can be dropped. Named
 * rather than counted: "3 unconstrained" is a number, and which three is a fact.
 */
const unconstrainedRoutes = new Set([
  "PATCH /v1/spaces/{}/pages/{}",
  "PATCH /v1/spaces/{}",
  "PATCH /v1/databases/{}/views/{}",
]);

describe("what this SPA sends, against what the handler reads", () => {
  it("both parsers can see their own subject", () => {
    const sites = webSites();
    const routes = goRoutes();
    expect(
      sites.length,
      `${sites.length} apiRequest sites found; 89 were measured at d1bfd11 and an independent ` +
        "`grep -c 'apiRequest[<(]'` agreed. Do not lower this floor to make a red go green.",
    ).toBeGreaterThanOrEqual(MIN_WEB_SITES);
    expect(routes.length).toBeGreaterThanOrEqual(MIN_ROUTES);
    expect(
      routes.filter((r) => r.pattern === null),
      "a route whose path expression could not be resolved is an UNMEASURED route, and the join " +
        "silently omits it. Resolve it or teach the walk the new form — do not delete this.",
    ).toEqual([]);
  });

  it("every request this SPA makes joins to a route the server registers", () => {
    const facts = handlerFacts();
    const unjoined = webSites()
      .filter((s) => s.path === null || !facts.has(`${s.verb} ${s.path}`))
      .map((s) => `${s.verb} ${s.path ?? "<unrenderable>"} (${s.file}:${s.line})`)
      .sort();
    expect(
      unjoined,
      "an unjoined call site contributes NOTHING to either census below while looking like a site " +
        "that was checked. This is the assertion that stops the whole file agreeing with itself.",
    ).toEqual([]);
    expect(webSites().length - unjoined.length).toBeGreaterThanOrEqual(MIN_JOINED);
  });

  it("every query parameter the SPA attaches is read by the handler that serves it", () => {
    const facts = handlerFacts();
    const inert: string[] = [];
    let joined = 0;
    for (const s of webSites()) {
      if (!s.queryFields.length || s.queryUnbounded) continue;
      const h = facts.get(`${s.verb} ${s.path}`);
      if (!h) continue;
      joined += 1;
      const dead = s.queryFields.filter((f) => !h.queryRead.includes(f));
      if (dead.length)
        inert.push(
          `${s.verb} ${s.path} sends ${dead.join(", ")} — ${h.handler} reads ` +
            `${h.queryRead.join(", ") || "no query parameter at all"}  (${s.file}:${s.line} → ${h.file}:${h.line})`,
        );
    }
    expect(joined, "the query join matched nothing; a join that matches nothing reports no inert filters").toBeGreaterThanOrEqual(MIN_QUERY_SITES);
    expect(
      inert.sort(),
      "this server does not refuse unknown query parameters, so the request SUCCEEDS and returns " +
        "the UNFILTERED result. A control that changes nothing looks exactly like a control that " +
        "works — which is how ?reviewer_id= outlived the security fix that stopped honouring it.",
    ).toEqual([]);
  });

  it("every body field the SPA can send is one the handler decodes", () => {
    const facts = handlerFacts();
    const dropped: string[] = [];
    const unresolved: string[] = [];
    let joined = 0;
    for (const s of webSites()) {
      if (!s.hasBody || s.bodyUnbounded) continue;
      const key = `${s.verb} ${s.path}`;
      const h = facts.get(key);
      if (!h) continue;
      if (h.unconstrainedBody || unconstrainedRoutes.has(key)) continue;
      if (h.unresolvedTarget) {
        unresolved.push(`${key} — ${h.handler} decodes into ${h.unresolvedTarget}, which this parse could not resolve`);
        continue;
      }
      joined += 1;
      const accepts = h.bodyAccepts ?? [];
      const dead = s.bodyFields.filter((f) => !accepts.includes(f));
      if (dead.length)
        dropped.push(
          `${key} can send ${dead.join(", ")} — ${h.handler}` +
            `${h.decodeVia ? ` (via ${h.decodeVia})` : ""} accepts ${accepts.join(", ") || "no body field at all"}` +
            `  (${s.file}:${s.line} → ${h.file}:${h.line})`,
        );
    }
    expect(joined, "the body join matched nothing").toBeGreaterThanOrEqual(MIN_BODY_JOINED);
    expect(
      unresolved.sort(),
      "a decode whose target type this parse cannot read is an UNCHECKED request contract, not a " +
        "clean one. Teach the resolver, or the census is smaller than it looks.",
    ).toEqual([]);
    expect(
      dropped.sort(),
      "this server decodes without DisallowUnknownFields, so a field it does not declare is " +
        "dropped in SILENCE and the request answers 200. Three of these were parameters the " +
        "server RETIRED FOR A SECURITY REASON and the client never stopped sending.",
    ).toEqual([]);
  });

  it("the unconstrained list names only routes that really are unconstrained", () => {
    // ⚠ WITHOUT THIS THE EXEMPTION LIST IS A PLACE TO PUT ANYTHING. A route listed here whose
    // handler decodes into a fixed struct is not exempt — it is unchecked.
    const facts = handlerFacts();
    const wrong = [...unconstrainedRoutes].filter((k) => {
      const h = facts.get(k);
      return !h || !h.unconstrainedBody;
    });
    expect(
      wrong,
      "these routes are listed as taking any key, and they do not. Remove them from the list — " +
        "an exemption that outlives its reason is decoration.",
    ).toEqual([]);
  });
});

// Shared API types. Mirror the Go server's JSON shapes (the field
// names match the Go tags). When the schema changes on either side
// the TypeScript compiler is the canary.

export interface Space {
  id: string;
  workspace_id: string;
  name: string;
  slug: string;
  description: string;
  icon: string;
  color: string;
  private: boolean;
  created_by: string;
  created_at: string;
  updated_at: string;
}

export interface Page {
  id: string;
  space_id: string;
  workspace_id: string;
  parent_id?: string;
  title: string;
  slug: string;
  content: string;       // ProseMirror JSON (string-encoded)
  content_text: string;  // plain text, search-only
  icon: string;
  cover_url: string;
  position: number;
  depth: number;
  is_template: boolean;
  created_by: string;
  updated_by: string;
  linked_issues?: string[];
  // THREE COST FIELDS, AND THE API HAS SENT ALL THREE SINCE MIGRATION 0018. Only the first was
  // declared here, so the other two arrived on every page response and were dropped at this type
  // boundary — which is why a renderer showing one addend under a sentence naming the sum could
  // not be red anywhere in the frontend. `ai_cost_usd` is the Track half ALONE; prefer
  // `total_ai_cost_usd` for "what did this document cost".
  //
  // ⚠ OPTIONAL, AND NOT AS CAUTION — A `Page` IN THIS APP GENUINELY MAY LACK THEM. api/client.ts
  // persists successful page GETs to IndexedDB and serves them back when fetch REJECTS, so an
  // entry cached before these fields shipped is a real Page value this code will be handed. Typed
  // required, `page.total_ai_cost_usd.toFixed(2)` crashes the whole page view on the offline path
  // — measured, not feared: it is what broke PageView.editsession.test.tsx's fixture, which is
  // exactly the shape of a pre-0018 cached record.
  ai_cost_usd: number;
  own_ai_cost_usd?: number;
  total_ai_cost_usd?: number;
  view_count: number;
  last_viewed_at?: string;
  last_verified_at?: string;
  verified_by?: string;
  stale_after_days: number;
  doc_status?: "draft" | "in_review" | "approved" | "rejected" | "archived";
  locked: boolean;
  locked_by?: string;
  locked_at?: string;
  // "template" was never a page_type — templates are the separate `is_template` boolean, and
  // nothing on either side of the wire ever produced the value. Kept closed to what the API
  // now accepts (page.validatePageType).
  page_type?: "document" | "changelog";
  created_at: string;
  updated_at: string;
}

export interface PageVersion {
  id: string;
  page_id: string;
  workspace_id: string;
  version: number;
  title: string;
  content: string;
  created_by: string;
  created_at: string;
  // The AI spend attributed to THIS revision — the completions bound while it was being written.
  // ⚠ 0 means "no priced spend is attributed to this revision", NOT "this revision was free": a
  // completion bound after the newest save belongs to a revision that does not exist yet, and every
  // binding written before migration 0021 carries no revision at all. Both are real money, both are
  // reported by the server's VersionCostSplit — served since `GET .../version-cost` and rendered
  // by VersionHistory's reconciliation strip — and neither can appear here.
  ai_cost_usd: number;
}

/**
 * VersionCostSplit — how much of a page's AI spend version history can actually show.
 *
 * ⚠ IT IS ON THE WIRE BECAUSE THE FUNCTION BEHIND IT HAD NO CALLER. `Store.VersionCostSplit`
 * landed with #190 arguing that "a per-revision figure that does not add up is a lie about money
 * that looks like a feature", and then nothing served it: measured at merged main `f1ad4db`, zero
 * production callers, only its own unit test and two comments — one of which was this file's.
 * A reconciliation guaranteed in Go and unreachable from the product is, to a reader, absent.
 *
 * ⚠ THE THREE PARTS AND THE WHOLE ARE FOUR INDEPENDENT NUMBERS, DELIBERATELY. `page_total_usd` is
 * read from `pages.own_ai_cost_usd` rather than derived from the other three, so the sum can be
 * COMPARED against it instead of being true by construction — which is exactly the arithmetic
 * that would balance in the case where a bucket had lost money.
 */
export interface VersionCostSplit {
  /** Landed on a revision that exists. This is what the rows in version history add up to. */
  attributed_usd: number;
  /**
   * Bound after the newest save, so it names a revision that does not exist YET. Not lost — it
   * appears on its revision the moment the next save creates it.
   */
  pending_usd: number;
  /**
   * Written before migration 0021, when the revision was not captured at bind time. No query can
   * recover it, and no future save will ever land it on a row.
   *
   * ⚠ IT IS ITS OWN FIELD RATHER THAN FOLDED IN WITH `pending_usd`, and the difference is the
   * whole reason both exist: pending money is coming and this money is not. One combined
   * "unshown" figure would tell a reader that all of it is on its way.
   */
  unattributable_usd: number;
  /** `pages.own_ai_cost_usd` — the whole, read rather than recomputed. */
  page_total_usd: number;
}

// ⚠ THERE IS NO `Comment` HERE ANY MORE, AND THAT IS THE POINT. This module used to declare a
// second one — nine fields, mirroring internal/model/model.go's `Comment`, which no handler in
// the tree had ever written. The comment wire is internal/comment/store.go's fourteen-field
// shape and its ONE client type is api/comments.ts#Comment. A second declaration of a wire
// object is not an alias: it is a narrower claim about the same bytes, and the narrower claim
// wins wherever it is imported.

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
  // reported by the server's VersionCostSplit, and neither can appear here.
  ai_cost_usd: number;
}

// ⚠ THERE IS NO `Comment` HERE ANY MORE, AND THAT IS THE POINT. This module used to declare a
// second one — nine fields, mirroring internal/model/model.go's `Comment`, which no handler in
// the tree had ever written. The comment wire is internal/comment/store.go's fourteen-field
// shape and its ONE client type is api/comments.ts#Comment. A second declaration of a wire
// object is not an alias: it is a narrower claim about the same bytes, and the narrower claim
// wins wherever it is imported.

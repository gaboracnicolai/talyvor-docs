import { apiRequest, qs } from "./client";

export type SearchSource = "fulltext" | "semantic" | "both";
export type SearchType = "all" | "fulltext" | "semantic";

export interface SearchResult {
  page_id: string;
  page_title: string;
  space_name: string;
  headline: string;
  rank?: number;
  similarity?: number;
  source: SearchSource;
  url: string;

  // THE THREE COST FIELDS ARE THE WIRE'S THREE, NOT A CHOSEN ONE OF THEM. A page has two
  // costs and migration 0018 exists because conflating them was the defect: ai_cost_usd is
  // the cost of the Track ISSUES linked to the page, own_ai_cost_usd is the cost of AI
  // operations performed ON the document, total_ai_cost_usd is their sum, derived server-side
  // so a caller does not add the two itself and get it subtly wrong.
  //
  // ⚠ ALL THREE ARE OPTIONAL BECAUSE THE WIRE GENUINELY OMITS THEM, not to be lenient.
  // `internal/search/handler.go` declares them `*float64` with `omitempty` (`8b3e1be`): a
  // SEMANTIC-ONLY row carries none of the three, so all three keys are absent. undefined means
  // NOT REPORTED; 0 means measured and zero. A renderer that reads `?? 0` collapses those two
  // facts back together.
  //
  // ⚠ THE REASON THEY ARE ABSENT IS A DECISION, NOT A MISSING READ, AND THIS COMMENT HAD IT
  // WRONG. It said the row is "built from a vector hit with no `pages` row read". MEASURED:
  // `internal/search/semantic.go`'s query is `FROM page_embeddings pe JOIN pages p ... JOIN
  // spaces sp ...` — a pages row IS read, and this row's title and space name come from it. The
  // costs are omitted deliberately because starting to render a number on a money surface is a
  // product call. Guarded by `components/SearchModal.costreason.test.ts`, which reads the SQL
  // and fails in EITHER direction, so the correct sentence tracks the query rather than the
  // most recent edit.
  //
  // This type declared `ai_cost_usd` alone until `SearchModal.cost.test.tsx` was written: the
  // other two arrived on every full-text hit and were dropped HERE, at the type boundary,
  // which is why nothing in the frontend was red while the list showed the wrong half.
  ai_cost_usd?: number;
  own_ai_cost_usd?: number;
  total_ai_cost_usd?: number;
}

export interface SearchResponse {
  results: SearchResult[];
  total: number;
  query: string;
  took_ms: number;
}

export interface SearchOptions {
  type?: SearchType;
  spaceId?: string;
  limit?: number;
  offset?: number;
}

export const searchApi = {
  search(workspaceId: string, query: string, opts: SearchOptions = {}) {
    return apiRequest<SearchResponse>(
      `/v1/workspaces/${workspaceId}/search${qs({
        q: query,
        type: opts.type,
        space_id: opts.spaceId,
        limit: opts.limit,
        offset: opts.offset,
      })}`,
    );
  },
};

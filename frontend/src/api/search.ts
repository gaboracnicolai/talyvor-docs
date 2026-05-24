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
  ai_cost_usd?: number;
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

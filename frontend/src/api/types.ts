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
  ai_cost_usd: number;
  view_count: number;
  last_viewed_at?: string;
  last_verified_at?: string;
  verified_by?: string;
  stale_after_days: number;
  doc_status?: "draft" | "in_review" | "approved" | "rejected" | "archived";
  created_at: string;
  updated_at: string;
}

export interface PageVersion {
  id: string;
  page_id: string;
  version: number;
  title: string;
  content: string;
  created_by: string;
  created_at: string;
}

export interface Comment {
  id: string;
  page_id: string;
  block_id?: string;
  author_id: string;
  content: string;
  resolved: boolean;
  resolved_by?: string;
  created_at: string;
  updated_at: string;
}

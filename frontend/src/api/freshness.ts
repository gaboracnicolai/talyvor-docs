import { apiRequest } from "./client";

export type FreshnessStatus = "fresh" | "warning" | "stale" | "unknown";

export interface FreshnessReport {
  page_id: string;
  space_id: string;
  title: string;
  status: FreshnessStatus;
  days_since_edit: number;
  days_since_verify?: number;
  stale_after_days: number;
  verified_by?: string;
  linked_issues_closed: number;
  suggest_review: boolean;
  reason: string;
  updated_at: string;
}

export const freshnessApi = {
  forPage(spaceID: string, pageID: string) {
    return apiRequest<FreshnessReport>(
      `/v1/spaces/${spaceID}/pages/${pageID}/freshness`,
    );
  },
  forWorkspace(workspaceID: string) {
    return apiRequest<FreshnessReport[]>(
      `/v1/workspaces/${workspaceID}/freshness`,
    );
  },
};

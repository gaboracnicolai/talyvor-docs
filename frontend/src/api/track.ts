import { apiRequest, qs } from "./client";

// Wire shapes mirror internal/trackintegration/handler.go responses.

export interface TrackIssue {
  id: string;
  identifier: string;
  title: string;
  status: string;
  priority: number;
  assignee_id?: string;
  ai_cost_usd: number;
  url?: string;
}

interface GetIssueResp {
  configured: boolean;
  available?: boolean;
  issue?: TrackIssue;
  error?: string;
}

interface SearchResp {
  configured: boolean;
  issues: TrackIssue[];
}

export const trackApi = {
  getIssue(workspaceID: string, issueID: string) {
    return apiRequest<GetIssueResp>(
      `/v1/workspaces/${workspaceID}/track/issues/${issueID}`,
    );
  },
  search(workspaceID: string, q: string) {
    return apiRequest<SearchResp>(
      `/v1/workspaces/${workspaceID}/track/search${qs({ q })}`,
    );
  },
};

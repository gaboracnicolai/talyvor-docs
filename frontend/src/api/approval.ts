import { apiRequest, qs } from "./client";

export type DocStatus =
  | "draft"
  | "in_review"
  | "approved"
  | "rejected"
  | "archived";

export type ApprovalStatus = "pending" | "approved" | "rejected";

export interface ApprovalRequest {
  id: string;
  page_id: string;
  workspace_id: string;
  requested_by: string;
  reviewers: string[];
  message: string;
  due_date?: string | null;
  status: ApprovalStatus;
  created_at: string;
  updated_at: string;
}

export interface ReviewDecision {
  id: string;
  request_id: string;
  reviewer_id: string;
  decision: "pending" | "approved" | "rejected";
  comment: string;
  created_at: string;
}

export interface LatestApproval {
  request: ApprovalRequest | null;
  decisions: ReviewDecision[];
}

export const approvalApi = {
  request(
    spaceID: string,
    pageID: string,
    body: {
      reviewers: string[];
      message?: string;
      due_date?: string | null;
      workspace_id?: string;
    },
  ) {
    return apiRequest<ApprovalRequest>(
      `/v1/spaces/${spaceID}/pages/${pageID}/approval`,
      { method: "POST", body },
    );
  },
  latest(spaceID: string, pageID: string) {
    return apiRequest<LatestApproval>(
      `/v1/spaces/${spaceID}/pages/${pageID}/approval`,
    );
  },
  decide(
    spaceID: string,
    pageID: string,
    requestID: string,
    body: { decision: "approved" | "rejected"; comment?: string },
  ) {
    return apiRequest<{ ok: boolean }>(
      `/v1/spaces/${spaceID}/pages/${pageID}/approval/${requestID}/decide`,
      { method: "POST", body },
    );
  },
  publish(spaceID: string, pageID: string) {
    return apiRequest<{ ok: boolean }>(
      `/v1/spaces/${spaceID}/pages/${pageID}/publish`,
      { method: "POST" },
    );
  },
  pending(workspaceID: string, reviewerID?: string) {
    return apiRequest<ApprovalRequest[]>(
      `/v1/workspaces/${workspaceID}/approvals/pending${qs({ reviewer_id: reviewerID })}`,
    );
  },
};

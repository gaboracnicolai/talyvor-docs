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

// An inbox row is an ApprovalRequest plus the space its page lives in. The space is not a
// column on approval_requests — the server JOINs it from `pages` for this one query — so it
// is a distinct type rather than an optional field on ApprovalRequest, where every other
// producer would leave it undefined. Without it the inbox cannot build the page's address
// (`/spaces/{spaceID}/pages/{pageID}`) and its Open button lands on Not found.
export interface PendingApproval extends ApprovalRequest {
  space_id: string;
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
    return apiRequest<PendingApproval[]>(
      `/v1/workspaces/${workspaceID}/approvals/pending${qs({ reviewer_id: reviewerID })}`,
    );
  },
};

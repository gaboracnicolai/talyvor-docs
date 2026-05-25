import { apiRequest } from "./client";

export interface CustomDomain {
  id: string;
  workspace_id: string;
  domain: string;
  space_id?: string | null;
  verified: boolean;
  verify_token: string;
  ssl_status: "pending" | "active" | "failed" | string;
  created_by: string;
  created_at: string;
  updated_at: string;
}

export interface VerifyResponse {
  verified: boolean;
  message: string;
}

export const customdomainApi = {
  list(workspaceID: string) {
    return apiRequest<CustomDomain[]>(`/v1/workspaces/${workspaceID}/custom-domains`);
  },
  create(workspaceID: string, body: { domain: string; space_id?: string | null }) {
    return apiRequest<CustomDomain>(`/v1/workspaces/${workspaceID}/custom-domains`, {
      method: "POST",
      body,
    });
  },
  verify(workspaceID: string, id: string) {
    return apiRequest<VerifyResponse>(
      `/v1/workspaces/${workspaceID}/custom-domains/${id}/verify`,
      { method: "POST" },
    );
  },
  remove(workspaceID: string, id: string) {
    return apiRequest<{ ok: boolean }>(
      `/v1/workspaces/${workspaceID}/custom-domains/${id}`,
      { method: "DELETE" },
    );
  },
};

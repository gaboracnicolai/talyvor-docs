import { apiRequest } from "./client";
import type { AccessLevel } from "./permissions";

export interface ShareLink {
  id: string;
  page_id: string;
  workspace_id: string;
  token: string;
  access: AccessLevel;
  expires_at?: string | null;
  view_count: number;
  created_by: string;
  created_at: string;
  has_password: boolean;
}

export interface CreateShareResp {
  link: ShareLink;
  share_url: string;
}

export interface PublicSharePayload {
  page: {
    id: string;
    title: string;
    icon: string;
    content: string;
    content_text: string;
    updated_at: string;
  };
  access: AccessLevel;
  has_password: boolean;
  expires_at?: string | null;
  powered_by: string;
}

export const sharingApi = {
  create(
    spaceID: string,
    pageID: string,
    body: {
      access?: AccessLevel;
      expires_in_days?: number;
      password?: string;
      workspace_id?: string;
    },
  ) {
    return apiRequest<CreateShareResp>(
      `/v1/spaces/${spaceID}/pages/${pageID}/share`,
      { method: "POST", body },
    );
  },
  list(spaceID: string, pageID: string) {
    return apiRequest<ShareLink[]>(`/v1/spaces/${spaceID}/pages/${pageID}/share`);
  },
  revoke(spaceID: string, pageID: string, id: string) {
    return apiRequest<{ ok: boolean }>(
      `/v1/spaces/${spaceID}/pages/${pageID}/share/${id}`,
      { method: "DELETE" },
    );
  },
  // Public endpoint — no Authorization header is needed but
  // apiRequest will skip it gracefully when localStorage is empty.
  publicView(token: string, password?: string) {
    const q = password ? `?password=${encodeURIComponent(password)}` : "";
    return apiRequest<PublicSharePayload>(`/v1/public/s/${token}${q}`);
  },
};

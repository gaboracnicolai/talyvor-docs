import { apiRequest } from "./client";

export type PageLinkType = "embed" | "mention" | "spec";

export interface PageLink {
  id: string;
  page_id: string;
  workspace_id: string;
  issue_id: string;
  link_type: PageLinkType;
  created_by: string;
  created_at: string;
}

export const linksApi = {
  list(pageID: string) {
    return apiRequest<PageLink[]>(`/v1/pages/${pageID}/links`);
  },
  create(pageID: string, body: Partial<PageLink>) {
    return apiRequest<PageLink>(`/v1/pages/${pageID}/links`, {
      method: "POST",
      body,
    });
  },
  remove(pageID: string, issueID: string) {
    return apiRequest<{ ok: boolean }>(`/v1/pages/${pageID}/links/${issueID}`, {
      method: "DELETE",
    });
  },
};

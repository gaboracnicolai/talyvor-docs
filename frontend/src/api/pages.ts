import { apiRequest, qs } from "./client";
import type { Comment, Page, PageVersion } from "./types";

export const pagesApi = {
  list(spaceID: string) {
    return apiRequest<Page[]>(`/v1/spaces/${spaceID}/pages`);
  },
  get(spaceID: string, pageID: string) {
    return apiRequest<Page>(`/v1/spaces/${spaceID}/pages/${pageID}`);
  },
  create(spaceID: string, body: Partial<Page>) {
    return apiRequest<Page>(`/v1/spaces/${spaceID}/pages`, { method: "POST", body });
  },
  update(spaceID: string, pageID: string, body: Partial<Page> | Record<string, unknown>) {
    return apiRequest<Page>(`/v1/spaces/${spaceID}/pages/${pageID}`, {
      method: "PATCH",
      body,
    });
  },
  remove(spaceID: string, pageID: string) {
    return apiRequest<{ ok: boolean }>(`/v1/spaces/${spaceID}/pages/${pageID}`, {
      method: "DELETE",
    });
  },
  recordView(spaceID: string, pageID: string) {
    return apiRequest<{ ok: boolean }>(`/v1/spaces/${spaceID}/pages/${pageID}/view`, {
      method: "POST",
    });
  },
  verify(spaceID: string, pageID: string) {
    return apiRequest<{ ok: boolean }>(`/v1/spaces/${spaceID}/pages/${pageID}/verify`, {
      method: "POST",
    });
  },
  versions(spaceID: string, pageID: string) {
    return apiRequest<PageVersion[]>(
      `/v1/spaces/${spaceID}/pages/${pageID}/versions`,
    );
  },
  restore(spaceID: string, pageID: string, version: number) {
    return apiRequest<Page>(
      `/v1/spaces/${spaceID}/pages/${pageID}/versions/${version}/restore`,
      { method: "POST" },
    );
  },
  search(workspaceID: string, q: string, limit = 25) {
    return apiRequest<Page[]>(
      `/v1/workspaces/${workspaceID}/pages/search${qs({ q, limit })}`,
    );
  },
  stale(workspaceID: string) {
    return apiRequest<Page[]>(`/v1/workspaces/${workspaceID}/pages/stale`);
  },
  listComments(spaceID: string, pageID: string) {
    return apiRequest<Comment[]>(`/v1/spaces/${spaceID}/pages/${pageID}/comments`);
  },
  createComment(spaceID: string, pageID: string, body: Partial<Comment>) {
    return apiRequest<Comment>(`/v1/spaces/${spaceID}/pages/${pageID}/comments`, {
      method: "POST",
      body,
    });
  },
  resolveComment(spaceID: string, pageID: string, commentID: string) {
    return apiRequest<{ ok: boolean }>(
      `/v1/spaces/${spaceID}/pages/${pageID}/comments/${commentID}/resolve`,
      { method: "POST" },
    );
  },
};

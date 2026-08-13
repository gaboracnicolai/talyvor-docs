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
  // `recordView` lived here and is DELETED, not moved. It POSTed to
  // /v1/spaces/{s}/pages/{p}/view with no body, and that route's handler rejects an empty body
  // with 400 before it reaches the store — so the method could not record a view from any
  // caller, and its sole caller (PageView's mount effect) swallowed the failure. The recording
  // client is `analyticsApi.recordView` in ~/api/analytics, which sends the duration the
  // server requires. See PageView.viewrecord.wire.test.tsx.
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
  version(spaceID: string, pageID: string, version: number) {
    return apiRequest<PageVersion>(
      `/v1/spaces/${spaceID}/pages/${pageID}/versions/${version}`,
    );
  },
  diffVersions(spaceID: string, pageID: string, from: number, to: number) {
    return apiRequest<{ from: PageVersion; to: PageVersion }>(
      `/v1/spaces/${spaceID}/pages/${pageID}/versions/${from}/diff/${to}`,
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

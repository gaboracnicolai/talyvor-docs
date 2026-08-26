import { apiRequest, qs } from "./client";
import type { Page, PageVersion, VersionCostSplit } from "./types";

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
  // ⚠ NOT `/versions/cost`. The server registers `/{pageID}/versions/{version}` on the adjacent
  // line, so a sibling under that prefix would resolve only by chi's static-over-param
  // precedence — and would degrade to a 400 BAD_VERSION rather than a 404 the day it did not.
  versionCostSplit(spaceID: string, pageID: string) {
    return apiRequest<VersionCostSplit>(
      `/v1/spaces/${spaceID}/pages/${pageID}/version-cost`,
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
  // ⚠ THE COMMENT CALLS THAT USED TO SIT HERE ARE GONE ON PURPOSE — `commentsApi` IS THE CLIENT.
  // `listComments`/`createComment`/`resolveComment` duplicated commentsApi.list/create/resolve on
  // the same three routes, had zero callers, and typed the response as api/types.ts#Comment: a
  // NINE-field shape against a wire that carries fourteen. The five it did not name are
  // thread_id, parent_id, author_name, resolved_at and replies — the thread tree and the only
  // human-readable identity on a comment. TypeScript refuses those fields through the narrow
  // type, so the trap did not show a wrong number; it told the next reader the server does not
  // thread comments, while internal/comment/store.go assembles the tree and the handler ships it.
  // `createComment(body: Partial<Comment>)` could not express parent_id, so a reply was not
  // expressible through it either. route-response-type.census.test.ts is what keeps one route to
  // one declared response type from here on.
};

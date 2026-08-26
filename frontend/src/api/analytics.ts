import { apiRequest, qs } from "./client";

export interface DayCount {
  date: string;
  count: number;
}

export interface ViewerStat {
  viewer_id: string;
  viewer_name: string;
  view_count: number;
  last_viewed: string;
}

export interface ReadStats {
  page_id: string;
  title: string;
  total_views: number;
  unique_viewers: number;
  avg_duration_sec: number;
  last_viewed_at?: string;
  views_by_day: DayCount[];
  top_viewers: ViewerStat[];
}

export interface WorkspaceReadStats {
  total_views: number;
  unique_viewers: number;
  most_read_pages: ReadStats[];
  least_read_pages: ReadStats[];
  never_read_count: number;
}

export const analyticsApi = {
  recordView(
    spaceID: string,
    pageID: string,
    body: {
      viewer_id?: string;
      viewer_name?: string;
      duration_sec: number;
      workspace_id?: string;
    },
  ) {
    return apiRequest<{ ok: boolean }>(
      `/v1/spaces/${spaceID}/pages/${pageID}/view`,
      { method: "POST", body },
    );
  },
  pageStats(spaceID: string, pageID: string, days = 30) {
    return apiRequest<ReadStats>(
      `/v1/spaces/${spaceID}/pages/${pageID}/analytics${qs({ days })}`,
    );
  },
  workspaceStats(workspaceID: string, days = 30) {
    return apiRequest<WorkspaceReadStats>(
      `/v1/workspaces/${workspaceID}/analytics/pages${qs({ days })}`,
    );
  },
  /**
   * The SPACE roll-up — the third of the three scopes the product page sells, and the one that
   * had neither a route nor a surface.
   *
   * ⚠ IT RETURNS `WorkspaceReadStats`, THE SAME TYPE THE ORG SCOPE DOES, AND THAT IS DELIBERATE
   * RATHER THAN LAZY. Server-side the two roll-ups are ONE statement narrowed (see
   * analytics.getScopedStats), precisely so they cannot drift; declaring a second identical
   * interface here would reintroduce on the client the divergence that was designed out on the
   * server. The type's NAME is now wider than its subject, which is the honest cost of sharing
   * it — what these figures describe is a property of the CALL, and the screen says so in words.
   */
  spaceStats(spaceID: string, days = 30) {
    return apiRequest<WorkspaceReadStats>(
      `/v1/spaces/${spaceID}/analytics/pages${qs({ days })}`,
    );
  },
};

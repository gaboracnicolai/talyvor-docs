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
};

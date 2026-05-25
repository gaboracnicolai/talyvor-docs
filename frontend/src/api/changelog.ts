import { apiRequest, qs } from "./client";

export type EntryType =
  | "feature"
  | "bugfix"
  | "breaking"
  | "improvement"
  | "deprecated"
  | "security";

export interface ChangelogEntry {
  id: string;
  page_id: string;
  workspace_id: string;
  version: string;
  title: string;
  summary: string;
  type: EntryType;
  issue_ids: string[];
  content: string;
  published_at?: string | null;
  created_by: string;
  created_at: string;
  updated_at: string;
}

export const changelogApi = {
  list(spaceID: string, pageID: string, opts: { type?: EntryType; limit?: number; offset?: number } = {}) {
    return apiRequest<ChangelogEntry[]>(
      `/v1/spaces/${spaceID}/pages/${pageID}/changelog/entries${qs({
        type: opts.type,
        limit: opts.limit,
        offset: opts.offset,
      })}`,
    );
  },
  create(spaceID: string, pageID: string, body: Partial<ChangelogEntry>) {
    return apiRequest<ChangelogEntry>(
      `/v1/spaces/${spaceID}/pages/${pageID}/changelog/entries`,
      { method: "POST", body },
    );
  },
  update(spaceID: string, pageID: string, id: string, body: Partial<ChangelogEntry>) {
    return apiRequest<ChangelogEntry>(
      `/v1/spaces/${spaceID}/pages/${pageID}/changelog/entries/${id}`,
      { method: "PATCH", body },
    );
  },
  remove(spaceID: string, pageID: string, id: string) {
    return apiRequest<{ ok: boolean }>(
      `/v1/spaces/${spaceID}/pages/${pageID}/changelog/entries/${id}`,
      { method: "DELETE" },
    );
  },
  publish(spaceID: string, pageID: string, id: string) {
    return apiRequest<ChangelogEntry>(
      `/v1/spaces/${spaceID}/pages/${pageID}/changelog/entries/${id}/publish`,
      { method: "POST" },
    );
  },
  generate(spaceID: string, pageID: string, body: { version: string; issue_ids: string[]; workspace_id?: string }) {
    return apiRequest<ChangelogEntry>(
      `/v1/spaces/${spaceID}/pages/${pageID}/changelog/generate`,
      { method: "POST", body },
    );
  },
};

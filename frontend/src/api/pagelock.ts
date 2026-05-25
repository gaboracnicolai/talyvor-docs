import { apiRequest } from "./client";

export interface LockState {
  locked: boolean;
  locked_by?: string | null;
  locked_by_name?: string | null;
  locked_at?: string | null;
}

export const pagelockApi = {
  get(spaceID: string, pageID: string) {
    return apiRequest<LockState>(`/v1/spaces/${spaceID}/pages/${pageID}/lock`);
  },
  lock(spaceID: string, pageID: string, memberID?: string) {
    return apiRequest<LockState>(`/v1/spaces/${spaceID}/pages/${pageID}/lock`, {
      method: "POST",
      body: memberID ? { member_id: memberID } : {},
    });
  },
  unlock(spaceID: string, pageID: string, body: { member_id?: string; is_admin?: boolean } = {}) {
    return apiRequest<{ ok: boolean }>(
      `/v1/spaces/${spaceID}/pages/${pageID}/lock`,
      { method: "DELETE", body },
    );
  },
};

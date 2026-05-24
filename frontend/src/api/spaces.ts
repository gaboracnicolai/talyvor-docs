import { apiRequest } from "./client";
import type { Space } from "./types";

export const spacesApi = {
  list(workspaceID: string) {
    return apiRequest<Space[]>(`/v1/workspaces/${workspaceID}/spaces`);
  },
  get(id: string) {
    return apiRequest<Space>(`/v1/spaces/${id}`);
  },
  create(body: Partial<Space>) {
    return apiRequest<Space>(`/v1/spaces`, { method: "POST", body });
  },
  update(id: string, body: Partial<Space>) {
    return apiRequest<Space>(`/v1/spaces/${id}`, { method: "PATCH", body });
  },
  remove(id: string) {
    return apiRequest<{ ok: boolean }>(`/v1/spaces/${id}`, { method: "DELETE" });
  },
};

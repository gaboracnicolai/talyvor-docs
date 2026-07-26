import { apiRequest } from "./client";

export type AccessLevel = "none" | "view" | "comment" | "edit" | "admin";

// "team" is deliberately ABSENT. The backend rejects it at write time
// (permission.Store.Grant → validSubjectType) because Docs cannot resolve team membership —
// teams live upstream and never reach the rule engine, so a team grant would persist and
// authorize nobody. Including it in the type invited a future caller to send a value the
// server answers 400 to; the type now matches what the API accepts.
export type SubjectType = "member" | "everyone";

export interface Permission {
  id: string;
  resource_type: "space" | "page";
  resource_id: string;
  subject_type: SubjectType;
  subject_id: string;
  access: AccessLevel;
  workspace_id: string;
  granted_by: string;
  created_at: string;
}

export const permissionsApi = {
  listSpace(spaceID: string) {
    return apiRequest<Permission[]>(`/v1/spaces/${spaceID}/permissions`);
  },
  grantSpace(spaceID: string, body: Partial<Permission> & { access: AccessLevel }) {
    return apiRequest<Permission>(`/v1/spaces/${spaceID}/permissions`, {
      method: "POST",
      body,
    });
  },
  revokeSpace(spaceID: string, permID: string) {
    return apiRequest<{ ok: boolean }>(`/v1/spaces/${spaceID}/permissions/${permID}`, {
      method: "DELETE",
    });
  },
  listPage(spaceID: string, pageID: string) {
    return apiRequest<Permission[]>(`/v1/spaces/${spaceID}/pages/${pageID}/permissions`);
  },
  grantPage(spaceID: string, pageID: string, body: Partial<Permission> & { access: AccessLevel }) {
    return apiRequest<Permission>(`/v1/spaces/${spaceID}/pages/${pageID}/permissions`, {
      method: "POST",
      body,
    });
  },
  revokePage(spaceID: string, pageID: string, permID: string) {
    return apiRequest<{ ok: boolean }>(
      `/v1/spaces/${spaceID}/pages/${pageID}/permissions/${permID}`,
      { method: "DELETE" },
    );
  },
};

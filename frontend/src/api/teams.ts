import { apiRequest } from "./client";

/** One team in a workspace, as `internal/team` lists it. `can_manage` is true for the caller who
 *  created the team — only they may change who is on it. */
export interface Team {
  id: string;
  name: string;
  created_by: string;
  created_at: string;
  members: string[];
  can_manage: boolean;
}

export const teamsApi = {
  list(workspaceID: string) {
    return apiRequest<Team[]>(`/v1/workspaces/${workspaceID}/teams`);
  },
  create(workspaceID: string, name: string) {
    return apiRequest<Team>(`/v1/workspaces/${workspaceID}/teams`, {
      method: "POST",
      body: { name },
    });
  },
  addMember(workspaceID: string, teamID: string, memberID: string) {
    return apiRequest<{ member: boolean }>(
      `/v1/workspaces/${workspaceID}/teams/${teamID}/members/${encodeURIComponent(memberID)}`,
      { method: "PUT" },
    );
  },
  removeMember(workspaceID: string, teamID: string, memberID: string) {
    return apiRequest<{ member: boolean }>(
      `/v1/workspaces/${workspaceID}/teams/${teamID}/members/${encodeURIComponent(memberID)}`,
      { method: "DELETE" },
    );
  },
};

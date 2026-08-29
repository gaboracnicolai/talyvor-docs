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
  // ⚠ NEITHER OF THESE SENDS A BODY, AND THE SERVER'S OWN COMMENT IS WHY.
  // `internal/pagelock/handler.go`: *"Both member_id and is_admin used to live here and both were
  // forgeable — is_admin let any Edit-tier member bypass 'only the locker can unlock'; member_id
  // NAMED THE ACTOR whenever authz.ActorOrEmpty was empty… The request body is not read at all —
  // an endpoint that takes no authority from the client cannot be lied to."*
  // The SPA went on sending both. `is_admin` in particular reads like a privilege claim and has
  // never been one; a caller passing `true` got whatever the server's own IsAdminFromContext said.
  lock(spaceID: string, pageID: string) {
    return apiRequest<LockState>(`/v1/spaces/${spaceID}/pages/${pageID}/lock`, {
      method: "POST",
    });
  },
  unlock(spaceID: string, pageID: string) {
    return apiRequest<{ ok: boolean }>(
      `/v1/spaces/${spaceID}/pages/${pageID}/lock`,
      { method: "DELETE" },
    );
  },
};

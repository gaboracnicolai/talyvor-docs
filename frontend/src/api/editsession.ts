import { apiRequest } from "./client";

// EditSession is the single-writer slot for a page: who is editing right now, and whether that
// hold is still live (heartbeating). The backend derives `live` from last_heartbeat vs TTL.
export interface EditSession {
  page_id: string;
  workspace_id: string;
  holder: string;
  acquired_at: string;
  last_heartbeat: string;
  live: boolean;
}

// GetResult is the observe shape: `session` is null when nobody holds the slot.
export interface EditSessionGet {
  session: EditSession | null;
}

const base = (spaceID: string, pageID: string) =>
  `/v1/spaces/${spaceID}/pages/${pageID}/edit-session`;

export const editSessionApi = {
  // get returns the current holder (or {session:null}). The backend returns a bare EditSession
  // when held and {session:null} when free; normalize both to EditSession | null.
  async get(spaceID: string, pageID: string): Promise<EditSession | null> {
    const r = await apiRequest<EditSession | EditSessionGet>(base(spaceID, pageID));
    if (r && "session" in r) return r.session;
    return (r as EditSession) ?? null;
  },
  acquire(spaceID: string, pageID: string) {
    return apiRequest<EditSession>(base(spaceID, pageID), { method: "POST" });
  },
  heartbeat(spaceID: string, pageID: string) {
    return apiRequest<EditSession>(`${base(spaceID, pageID)}/heartbeat`, { method: "POST" });
  },
  takeover(spaceID: string, pageID: string) {
    return apiRequest<EditSession>(`${base(spaceID, pageID)}/takeover`, { method: "POST" });
  },
  release(spaceID: string, pageID: string) {
    return apiRequest<{ ok: boolean }>(base(spaceID, pageID), { method: "DELETE" });
  },
};

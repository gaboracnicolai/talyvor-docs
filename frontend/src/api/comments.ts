import { apiRequest, qs } from "./client";

export interface Comment {
  id: string;
  page_id: string;
  block_id?: string | null;
  thread_id?: string | null;
  parent_id?: string | null;
  author_id: string;
  author_name: string;
  content: string;
  resolved: boolean;
  resolved_by?: string | null;
  resolved_at?: string | null;
  replies?: Comment[];
  created_at: string;
  updated_at: string;
}

export interface CommentStats {
  total: number;
  open: number;
  resolved: number;
}

export const commentsApi = {
  list(spaceID: string, pageID: string, includeResolved = false) {
    return apiRequest<Comment[]>(
      `/v1/spaces/${spaceID}/pages/${pageID}/comments${qs({
        include_resolved: includeResolved ? "true" : undefined,
      })}`,
    );
  },
  // ⚠ NO author_id / resolved_by ON ANY OF THESE THREE. The server derives the acting member
  // from the gateway-verified caller and has ignored a body-supplied one since the
  // client-supplied-authority fix: createBody dropped the field, replyBody dropped it at
  // b98933b, and resolveBody has always been an empty struct. This client went on DECLARING and
  // SENDING all three, which is a client believing it chooses the author. author_name stays —
  // it is display text, not identity.
  create(
    spaceID: string,
    pageID: string,
    body: {
      content: string;
      block_id?: string;
      author_name?: string;
    },
  ) {
    return apiRequest<Comment>(`/v1/spaces/${spaceID}/pages/${pageID}/comments`, {
      method: "POST",
      body,
    });
  },
  reply(
    spaceID: string,
    pageID: string,
    commentID: string,
    body: { content: string; author_name?: string },
  ) {
    return apiRequest<Comment>(
      `/v1/spaces/${spaceID}/pages/${pageID}/comments/${commentID}/reply`,
      { method: "POST", body },
    );
  },
  resolve(spaceID: string, pageID: string, commentID: string) {
    return apiRequest<{ ok: boolean }>(
      `/v1/spaces/${spaceID}/pages/${pageID}/comments/${commentID}/resolve`,
      { method: "POST", body: {} },
    );
  },
  unresolve(spaceID: string, pageID: string, commentID: string) {
    return apiRequest<{ ok: boolean }>(
      `/v1/spaces/${spaceID}/pages/${pageID}/comments/${commentID}/resolve`,
      { method: "DELETE" },
    );
  },
  remove(spaceID: string, pageID: string, commentID: string) {
    return apiRequest<{ ok: boolean }>(
      `/v1/spaces/${spaceID}/pages/${pageID}/comments/${commentID}`,
      { method: "DELETE" },
    );
  },
  stats(spaceID: string, pageID: string) {
    return apiRequest<CommentStats>(
      `/v1/spaces/${spaceID}/pages/${pageID}/comments/stats`,
    );
  },
};

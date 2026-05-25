import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { commentsApi, type Comment } from "~/api/comments";

// useComments bundles the comments-list query (open or all), the
// stats query, and the CRUD mutations. Each mutation invalidates
// both the list + the stats so the page header counter stays in
// sync with the panel.
export function useComments(spaceID: string, pageID: string, includeResolved = false) {
  const qc = useQueryClient();
  const memberID =
    typeof window !== "undefined" ? localStorage.getItem("docs_member_id") || "" : "";
  const memberName =
    typeof window !== "undefined" ? localStorage.getItem("docs_member_name") || "" : "";

  const threads = useQuery({
    queryKey: ["comments", pageID, includeResolved],
    queryFn: () => commentsApi.list(spaceID, pageID, includeResolved),
  });
  const stats = useQuery({
    queryKey: ["comment-stats", pageID],
    queryFn: () => commentsApi.stats(spaceID, pageID),
  });

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ["comments", pageID] });
    qc.invalidateQueries({ queryKey: ["comment-stats", pageID] });
  };

  const create = useMutation({
    mutationFn: (body: { content: string; block_id?: string }) =>
      commentsApi.create(spaceID, pageID, {
        ...body,
        author_id: memberID,
        author_name: memberName || memberID,
      }),
    onSuccess: invalidate,
  });
  const reply = useMutation({
    mutationFn: ({ commentID, content }: { commentID: string; content: string }) =>
      commentsApi.reply(spaceID, pageID, commentID, {
        content,
        author_id: memberID,
        author_name: memberName || memberID,
      }),
    onSuccess: invalidate,
  });
  const resolve = useMutation({
    mutationFn: (commentID: string) =>
      commentsApi.resolve(spaceID, pageID, commentID, memberID),
    onSuccess: invalidate,
  });
  const unresolve = useMutation({
    mutationFn: (commentID: string) => commentsApi.unresolve(spaceID, pageID, commentID),
    onSuccess: invalidate,
  });
  const remove = useMutation({
    mutationFn: (commentID: string) => commentsApi.remove(spaceID, pageID, commentID),
    onSuccess: invalidate,
  });

  return {
    threads: (threads.data ?? []) as Comment[],
    isLoading: threads.isLoading,
    stats: stats.data,
    memberID,
    memberName,
    create,
    reply,
    resolve,
    unresolve,
    remove,
  };
}

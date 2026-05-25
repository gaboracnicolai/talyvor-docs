import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { changelogApi, type ChangelogEntry } from "~/api/changelog";

// useChangelog bundles the entry-list query with the mutations the
// ChangelogView surfaces. We invalidate on every mutation success
// so the timeline stays in sync with the draft + publish workflow.
export function useChangelog(spaceID: string, pageID: string) {
  const qc = useQueryClient();
  const entries = useQuery({
    queryKey: ["changelog", pageID],
    queryFn: () => changelogApi.list(spaceID, pageID, { limit: 50 }),
  });

  const invalidate = () =>
    qc.invalidateQueries({ queryKey: ["changelog", pageID] });

  const create = useMutation({
    mutationFn: (body: Partial<ChangelogEntry>) =>
      changelogApi.create(spaceID, pageID, body),
    onSuccess: invalidate,
  });
  const update = useMutation({
    mutationFn: ({ id, body }: { id: string; body: Partial<ChangelogEntry> }) =>
      changelogApi.update(spaceID, pageID, id, body),
    onSuccess: invalidate,
  });
  const remove = useMutation({
    mutationFn: (id: string) => changelogApi.remove(spaceID, pageID, id),
    onSuccess: invalidate,
  });
  const publish = useMutation({
    mutationFn: (id: string) => changelogApi.publish(spaceID, pageID, id),
    onSuccess: invalidate,
  });
  const generate = useMutation({
    mutationFn: (body: { version: string; issue_ids: string[]; workspace_id?: string }) =>
      changelogApi.generate(spaceID, pageID, body),
    onSuccess: invalidate,
  });

  return {
    entries: entries.data ?? [],
    isLoading: entries.isLoading,
    create,
    update,
    remove,
    publish,
    generate,
  };
}

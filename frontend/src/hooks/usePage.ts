import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { pagesApi } from "~/api/pages";
import type { Page } from "~/api/types";
import { workspaceID } from "./useSpaces";

export function usePages(spaceID: string | null) {
  return useQuery({
    queryKey: ["pages", spaceID],
    queryFn: () => pagesApi.list(spaceID!),
    enabled: !!spaceID,
  });
}

export function usePage(spaceID: string | null, pageID: string | null) {
  return useQuery({
    queryKey: ["page", spaceID, pageID],
    queryFn: () => pagesApi.get(spaceID!, pageID!),
    enabled: !!spaceID && !!pageID,
  });
}

export function useCreatePage(spaceID: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: Partial<Page>) =>
      pagesApi.create(spaceID, { ...body, workspace_id: workspaceID() }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["pages", spaceID] });
    },
  });
}

export function useUpdatePage(spaceID: string, pageID: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (updates: Record<string, unknown>) =>
      pagesApi.update(spaceID, pageID, updates),
    onSuccess: (updated) => {
      qc.setQueryData(["page", spaceID, pageID], updated);
      qc.invalidateQueries({ queryKey: ["pages", spaceID] });
    },
  });
}

export function useSearchPages(q: string) {
  return useQuery({
    queryKey: ["pages-search", workspaceID(), q],
    queryFn: () => pagesApi.search(workspaceID(), q),
    enabled: q.trim().length >= 2,
  });
}

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { spacesApi } from "~/api/spaces";
import type { Space } from "~/api/types";

// The workspace ID lives in localStorage until a real auth flow
// lands (Phase 4). Centralised here so every page reads the same
// source of truth.
export function workspaceID(): string {
  return localStorage.getItem("docs_workspace_id") || "default";
}

export function useSpaces() {
  return useQuery({
    queryKey: ["spaces", workspaceID()],
    queryFn: () => spacesApi.list(workspaceID()),
  });
}

export function useSpace(id: string | null) {
  return useQuery({
    queryKey: ["space", id],
    queryFn: () => spacesApi.get(id!),
    enabled: !!id,
  });
}

export function useCreateSpace() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: Partial<Space>) =>
      spacesApi.create({ ...body, workspace_id: workspaceID() }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["spaces"] }),
  });
}

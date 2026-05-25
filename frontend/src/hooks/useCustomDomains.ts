import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { customdomainApi, type CustomDomain } from "~/api/customdomain";

// useCustomDomains bundles the workspace's domain list query with
// create/verify/delete mutations. Verify polls on demand — there's
// no point auto-refetching because TXT records take minutes to
// propagate and the user knows when to retry.
export function useCustomDomains(workspaceID: string) {
  const qc = useQueryClient();
  const domains = useQuery({
    queryKey: ["custom-domains", workspaceID],
    queryFn: () => customdomainApi.list(workspaceID),
  });

  const invalidate = () =>
    qc.invalidateQueries({ queryKey: ["custom-domains", workspaceID] });

  const create = useMutation({
    mutationFn: (body: { domain: string; space_id?: string | null }) =>
      customdomainApi.create(workspaceID, body),
    onSuccess: invalidate,
  });
  const verify = useMutation({
    mutationFn: (id: string) => customdomainApi.verify(workspaceID, id),
    onSuccess: invalidate,
  });
  const remove = useMutation({
    mutationFn: (id: string) => customdomainApi.remove(workspaceID, id),
    onSuccess: invalidate,
  });

  return {
    domains: (domains.data ?? []) as CustomDomain[],
    isLoading: domains.isLoading,
    create,
    verify,
    remove,
  };
}

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { pagelockApi, type LockState } from "~/api/pagelock";

// usePageLock bundles the lock-state query + lock/unlock mutations.
// Returns the live LockState so the caller can render banners +
// gate the editor without re-deriving anything.
export function usePageLock(spaceID: string, pageID: string) {
  const qc = useQueryClient();
  const memberID =
    typeof window !== "undefined" ? localStorage.getItem("docs_member_id") || "" : "";

  const state = useQuery({
    queryKey: ["page-lock", pageID],
    queryFn: () => pagelockApi.get(spaceID, pageID),
    refetchInterval: 30_000,
  });

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ["page-lock", pageID] });
    qc.invalidateQueries({ queryKey: ["page", spaceID, pageID] });
  };

  const lock = useMutation({
    mutationFn: () => pagelockApi.lock(spaceID, pageID, memberID),
    onSuccess: invalidate,
  });
  const unlock = useMutation({
    mutationFn: ({ isAdmin }: { isAdmin?: boolean } = {}) =>
      pagelockApi.unlock(spaceID, pageID, { member_id: memberID, is_admin: isAdmin }),
    onSuccess: invalidate,
  });

  // lockedByMe surfaces a convenience flag the host components use
  // to decide between "Lock" + "Unlock" affordances.
  const lockedByMe = computeLockedByMe(state.data, memberID);

  return { state: state.data, isLoading: state.isLoading, lock, unlock, memberID, lockedByMe };
}

function computeLockedByMe(s: LockState | undefined, memberID: string): boolean {
  return !!s?.locked && !!memberID && s.locked_by === memberID;
}

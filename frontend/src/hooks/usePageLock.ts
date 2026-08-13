import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { pagelockApi, type LockState } from "~/api/pagelock";

// usePageLock bundles the lock-state query + lock/unlock mutations.
// Returns the live LockState so the caller can render banners +
// gate the editor without re-deriving anything.
export function usePageLock(spaceID: string, pageID: string) {
  const qc = useQueryClient();
  // WHO AM I, ASKED OF THE SERVER RATHER THAN OF localStorage — the same seam fixed for the
  // edit session in `ef7f935`, on the surface where the consequence is permanent.
  //
  // `locked_by` is a workspace_members row id the server resolves from the GATEWAY-VERIFIED
  // identity (pagelock/handler.go `actorFor`; "the request body is not read at all"). The only
  // thing it was compared against was `docs_member_id`, a localStorage key NOTHING IN THIS SPA
  // EVER WRITES — so it is "" in every un-seeded browser and `lockedByMe` is false for every
  // lock, INCLUDING THE ONE THIS BROWSER JUST TOOK. Every unlock affordance in the product sits
  // behind that flag (LockBadge's button, LockBanner's Unlock), and `pages.locked_by` has no
  // TTL, so the locker was left with no route back but an admin — while the server would have
  // granted their unlock, because the server knows they are the locker.
  //
  // `POST .../lock` RETURNS the LockState and a 200 means the caller now holds it, so
  // `locked_by` on that response IS this browser's verified member id, already on the wire. A
  // lock held by someone else is a 423 (apiRequest throws), so nothing is learned from a
  // stranger's row; the `get` query is deliberately NOT a source because it reports whoever
  // holds the lock, which is the question rather than the answer.
  //
  // localStorage stays as the fallback for the window before the first lock lands. What
  // identity the REST of the SPA should use is a product decision, not this hook's to make.
  const [learnedSelf, setLearnedSelf] = useState("");
  const storedMemberID =
    typeof window !== "undefined" ? localStorage.getItem("docs_member_id") || "" : "";
  const memberID = learnedSelf || storedMemberID;

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
    onSuccess: (s) => {
      // `s` can be undefined: on a network failure apiRequest queues the write and resolves
      // `undefined as T`. A 200 that carries a locked_by is the only thing worth learning from.
      if (s?.locked_by) setLearnedSelf(s.locked_by);
      invalidate();
    },
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

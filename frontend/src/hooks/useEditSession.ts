import { useEffect } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { editSessionApi, type EditSession } from "~/api/editsession";

// HEARTBEAT_MS keeps a held session alive. Well inside the backend's 30s TTL so a couple of
// missed beats (tab throttling, a blip) don't drop the slot, but a closed/crashed editor
// expires within ~TTL and the page becomes claimable via Takeover.
export const HEARTBEAT_MS = 10_000;

export interface SessionFlags {
  holder: string | null;
  live: boolean;
  heldByMe: boolean;
  heldByOther: boolean;
}

// sessionFlags is the PURE derivation the UI gates on. Only a LIVE session constrains anyone;
// an expired/absent session leaves the page open. Extracted so it is headless-testable.
export function sessionFlags(
  session: EditSession | null | undefined,
  memberID: string,
): SessionFlags {
  const holder = session?.holder ?? null;
  const live = !!session?.live;
  const heldByMe = live && !!memberID && holder === memberID;
  const heldByOther = live && !!holder && holder !== memberID;
  return { holder, live, heldByMe, heldByOther };
}

// useEditSession bundles the session-state query + acquire/heartbeat/release/takeover mutations
// and the derived flags, mirroring usePageLock. It heartbeats automatically WHILE the caller
// holds the slot; acquiring/releasing is the caller's decision (e.g. on editor open/close).
export interface UseEditSessionOptions {
  // autoAcquire takes the writer slot when the hook mounts (an editable page opened) and releases
  // it on unmount/navigation. Leave false to only observe (e.g. a read-only viewer). The host
  // passes `autoAcquire: canEdit` so approved/prop-read-only pages never grab the slot.
  autoAcquire?: boolean;
}

export function useEditSession(
  spaceID: string,
  pageID: string,
  opts: UseEditSessionOptions = {},
) {
  const qc = useQueryClient();
  const memberID =
    typeof window !== "undefined" ? localStorage.getItem("docs_member_id") || "" : "";

  const query = useQuery({
    queryKey: ["edit-session", pageID],
    queryFn: () => editSessionApi.get(spaceID, pageID),
    refetchInterval: 10_000, // observe other writers taking/dropping the slot
  });

  const invalidate = () => qc.invalidateQueries({ queryKey: ["edit-session", pageID] });
  const acquire = useMutation({
    mutationFn: () => editSessionApi.acquire(spaceID, pageID),
    onSuccess: invalidate,
  });
  const takeover = useMutation({
    mutationFn: () => editSessionApi.takeover(spaceID, pageID),
    onSuccess: invalidate,
  });
  const release = useMutation({
    mutationFn: () => editSessionApi.release(spaceID, pageID),
    onSuccess: invalidate,
  });

  const flags = sessionFlags(query.data, memberID);

  // Heartbeat only while I actually hold the slot — never a way to seize someone else's.
  useEffect(() => {
    if (!flags.heldByMe) return;
    const id = setInterval(() => {
      void editSessionApi.heartbeat(spaceID, pageID).catch(() => {});
    }, HEARTBEAT_MS);
    return () => clearInterval(id);
  }, [flags.heldByMe, spaceID, pageID]);

  // Auto-acquire lifecycle: take the slot on mount and release on unmount (or when the page /
  // editability changes). A live foreign session makes acquire 423 — harmless; the query then
  // reads heldByOther and the host renders read-only. mutate() is stable, so it's not a dep.
  const { autoAcquire = false } = opts;
  useEffect(() => {
    if (!autoAcquire) return;
    acquire.mutate();
    return () => {
      release.mutate();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [autoAcquire, spaceID, pageID]);

  return { session: query.data, ...flags, acquire, takeover, release, memberID };
}

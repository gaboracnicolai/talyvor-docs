import { useEffect, useState } from "react";
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
  // WHO AM I, ASKED OF THE SERVER RATHER THAN OF localStorage.
  //
  // `holder` is a workspace_members row id the server resolves from the GATEWAY-VERIFIED
  // identity (editsession/handler.go `actorFor`; the body is never read for authority).
  // `docs_member_id` was the only thing compared against it, and NOTHING IN THIS SPA EVER
  // WRITES THAT KEY — the sole localStorage.setItem in the tree is useSearch's recent-pages
  // list. So in every browser that has not been hand-seeded it is "", which makes heldByMe
  // permanently false and heldByOther permanently true: the screen locked the holder out of
  // the page it had just acquired, named that holder's own id in the banner, and never
  // started the heartbeat, so the slot died at the backend's 30s TTL mid-edit.
  //
  // Acquire / Heartbeat / Takeover each RETURN the Session, and a 2xx from any of them means
  // the slot is the CALLER's — a live foreign session is a 423, which apiRequest throws on, so
  // nothing is ever learned from someone else's row. That response's `holder` therefore IS this
  // browser's verified member id, already on the wire and needing no new endpoint. `get` is
  // deliberately NOT a source: it reports whoever holds the slot, which is the question, not
  // the answer.
  //
  // The localStorage value stays as the fallback for the window before the first claim lands.
  // What identity the REST of the SPA should use (usePageLock, useComments, Sidebar,
  // ApprovalInbox, the analytics viewer id, the presence id all read the same unset key) is a
  // product decision, not this hook's to make.
  const [learnedSelf, setLearnedSelf] = useState("");
  const storedMemberID =
    typeof window !== "undefined" ? localStorage.getItem("docs_member_id") || "" : "";
  const memberID = learnedSelf || storedMemberID;

  const query = useQuery({
    queryKey: ["edit-session", pageID],
    queryFn: () => editSessionApi.get(spaceID, pageID),
    refetchInterval: 10_000, // observe other writers taking/dropping the slot
  });

  const invalidate = () => qc.invalidateQueries({ queryKey: ["edit-session", pageID] });
  // learnSelf records the verified holder off a SUCCESSFUL claim. `s` can be undefined: on a
  // network failure apiRequest queues the write and resolves `undefined as T`, and an unheld
  // slot is not something a 2xx claim can return.
  const learnSelf = (s: EditSession | null | undefined) => {
    if (s?.holder) setLearnedSelf(s.holder);
  };
  const acquire = useMutation({
    mutationFn: () => editSessionApi.acquire(spaceID, pageID),
    onSuccess: (s) => {
      learnSelf(s);
      invalidate();
    },
  });
  const takeover = useMutation({
    mutationFn: () => editSessionApi.takeover(spaceID, pageID),
    onSuccess: (s) => {
      learnSelf(s);
      invalidate();
    },
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
      void editSessionApi.heartbeat(spaceID, pageID).then(learnSelf).catch(() => {});
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

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Check, X } from "lucide-react";
import { approvalApi, type PendingApproval } from "~/api/approval";

interface InboxProps {
  workspaceID: string;
  onOpenPage: (spaceID: string, pageID: string) => void;
}

// ApprovalInboxPage is the "My approvals" surface — every request
// where the current viewer still has a pending decision. The
// quick-action buttons let reviewers approve / reject without
// jumping into each page individually; for nuanced feedback they
// open the page and use ApprovalPanel directly.
export function ApprovalInboxPage({ workspaceID, onOpenPage }: InboxProps) {
  const reviewerID = localStorage.getItem("docs_member_id") || "";
  const qc = useQueryClient();
  const pending = useQuery({
    queryKey: ["approvals-pending", workspaceID, reviewerID],
    // ⚠ SEE Sidebar.tsx: `reviewerID` is no longer sent, the server derives it, and the
    // `enabled` gate below still keys on a localStorage value nothing writes — which is why this
    // screen renders its "set docs_member_id" instruction instead of the caller's queue. Left as
    // it is on purpose; turning it on is a product decision, not a wire cleanup.
    queryFn: () => approvalApi.pending(workspaceID),
    enabled: !!reviewerID,
  });

  return (
    <div className="mx-auto max-w-3xl space-y-3 p-8">
      <header>
        <h1 className="text-lg font-semibold">My approvals</h1>
        <p className="text-xs text-muted">
          Pages waiting for your decision. Approve or reject inline, or open
          the page for a closer look.
        </p>
      </header>
      {!reviewerID ? (
        <p className="text-xs text-muted">
          Set <code>docs_member_id</code> in local storage to use this view.
        </p>
      ) : pending.isLoading ? (
        <p className="text-xs text-muted">Loading…</p>
      ) : (pending.data ?? []).length === 0 ? (
        <p className="text-xs text-muted">Nothing waiting — you're caught up.</p>
      ) : (
        <ul className="space-y-2">
          {pending.data!.map((req) => (
            <PendingRow
              key={req.id}
              req={req}
              reviewerID={reviewerID}
              workspaceID={workspaceID}
              onOpen={() => onOpenPage(req.space_id, req.page_id)}
              onDecided={() =>
                qc.invalidateQueries({
                  queryKey: ["approvals-pending", workspaceID, reviewerID],
                })
              }
            />
          ))}
        </ul>
      )}
    </div>
  );
}

function PendingRow({
  req,
  workspaceID: _wsID,
  onOpen,
  onDecided,
}: {
  req: PendingApproval;
  reviewerID: string;
  workspaceID: string;
  onOpen: () => void;
  onDecided: () => void;
}) {
  // This was `const spaceID = ""`, justified by two claims. One was true and one was not,
  // and both were MEASURED rather than re-argued:
  //
  //   · "the decide endpoint doesn't actually need the spaceID (it's URL-decorative)" — TRUE.
  //     chi matches an empty {spaceID} segment and the route's enforcer resolves the page from
  //     {pageID}, so `POST /v1/spaces//pages/{p}/approval/{r}/decide` answered 200 and recorded
  //     the decision (internal/approval/inbox_space_realpg_test.go). Approve/Reject worked.
  //   · "the page route is reconstructed by PageView once the user navigates in" — FALSE.
  //     PageView never mounted: `paths.page("", id)` builds `/spaces//pages/{id}`, which
  //     react-router normalises to `/spaces/pages/{id}`, and the app's route table resolves
  //     BOTH to its catch-all. Open landed on Not found, every time.
  //
  // The response now carries the space (server-side JOIN to pages), so the URL is the real
  // one in both places — decide included, which stops it depending on a router's tolerance
  // for an empty path segment.
  const spaceID = req.space_id;

  const decide = useMutation({
    mutationFn: (decision: "approved" | "rejected") =>
      approvalApi.decide(spaceID, req.page_id, req.id, { decision }),
    onSuccess: onDecided,
  });

  return (
    <li className="rounded border border-border bg-surface px-3 py-2 text-xs">
      <div className="flex items-start justify-between gap-2">
        <button onClick={onOpen} className="flex-1 text-left">
          <div className="text-sm font-medium">Page {req.page_id.slice(0, 8)}</div>
          <div className="text-[10px] text-muted">
            requested by {req.requested_by}
            {req.due_date ? ` · due ${new Date(req.due_date).toLocaleDateString()}` : ""}
          </div>
          {req.message ? (
            <div className="mt-1 line-clamp-2 text-muted">"{req.message}"</div>
          ) : null}
        </button>
        <div className="flex items-center gap-1">
          <button
            onClick={() => decide.mutate("approved")}
            disabled={decide.isPending}
            className="rounded bg-callout-success px-1.5 py-1 text-bg hover:opacity-90 disabled:opacity-40"
            title="Approve"
          >
            <Check size={11} />
          </button>
          <button
            onClick={() => decide.mutate("rejected")}
            disabled={decide.isPending}
            className="rounded bg-callout-error px-1.5 py-1 text-bg hover:opacity-90 disabled:opacity-40"
            title="Reject"
          >
            <X size={11} />
          </button>
        </div>
      </div>
    </li>
  );
}

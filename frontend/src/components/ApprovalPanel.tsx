import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Check, Clock, X, Send, Upload } from "lucide-react";
import { approvalApi, type DocStatus, type ReviewDecision } from "~/api/approval";

interface PanelProps {
  spaceID: string;
  pageID: string;
  docStatus: DocStatus;
}

// ApprovalPanel surfaces the approval workflow in the PageView
// right rail. Render branches on the current doc_status:
//   draft     → "Request review" form
//   in_review → reviewers + their decision rows
//   approved  → green confirmation + Publish CTA
//   rejected  → rejection comments
//
// The host PageView passes the live doc_status (from the page
// record). The panel re-fetches the latest request to render
// per-reviewer state.
export function ApprovalPanel({ spaceID, pageID, docStatus }: PanelProps) {
  const qc = useQueryClient();
  const latest = useQuery({
    queryKey: ["approval", pageID],
    queryFn: () => approvalApi.latest(spaceID, pageID),
    enabled: docStatus !== "draft",
  });

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ["approval", pageID] });
    qc.invalidateQueries({ queryKey: ["page", spaceID, pageID] });
  };

  if (docStatus === "draft") {
    return <RequestForm spaceID={spaceID} pageID={pageID} onSubmitted={invalidate} />;
  }

  const decisions = latest.data?.decisions ?? [];

  if (docStatus === "in_review") {
    return (
      <div className="space-y-1 text-xs">
        <div className="text-[10px] uppercase tracking-wider text-muted">
          Reviewers
        </div>
        <DecisionList decisions={decisions} />
        {latest.data?.request?.message ? (
          <div className="rounded border border-border bg-bg px-2 py-1 text-muted">
            "{latest.data.request.message}"
          </div>
        ) : null}
      </div>
    );
  }

  if (docStatus === "approved") {
    return (
      <ApprovedSection
        spaceID={spaceID}
        pageID={pageID}
        decisions={decisions}
        onPublished={invalidate}
      />
    );
  }

  if (docStatus === "rejected") {
    const rejections = decisions.filter((d) => d.decision === "rejected");
    return (
      <div className="space-y-1 text-xs">
        <div className="text-callout-error">Changes requested.</div>
        {rejections.length > 0 ? (
          <ul className="space-y-1">
            {rejections.map((d) => (
              <li
                key={d.id}
                className="rounded border border-callout-error/40 bg-callout-error/10 px-2 py-1 text-text"
              >
                <div className="text-[10px] text-callout-error">
                  {d.reviewer_id}
                </div>
                {d.comment ? d.comment : <em className="text-muted">No comment.</em>}
              </li>
            ))}
          </ul>
        ) : null}
      </div>
    );
  }

  return null;
}

// ─── Sub-components ──────────────────────────────────

function RequestForm({
  spaceID,
  pageID,
  onSubmitted,
}: {
  spaceID: string;
  pageID: string;
  onSubmitted: () => void;
}) {
  const [reviewers, setReviewers] = useState("");
  const [message, setMessage] = useState("");
  const [error, setError] = useState<string | null>(null);
  const submit = useMutation({
    mutationFn: () =>
      approvalApi.request(spaceID, pageID, {
        reviewers: reviewers
          .split(",")
          .map((r) => r.trim())
          .filter(Boolean),
        message,
      }),
    onSuccess: () => {
      setReviewers("");
      setMessage("");
      onSubmitted();
    },
    onError: () => setError("Couldn't request review."),
  });
  return (
    <div className="space-y-1">
      <input
        value={reviewers}
        onChange={(e) => setReviewers(e.target.value)}
        placeholder="reviewer IDs (comma-separated)"
        className="w-full rounded border border-border bg-bg px-1 py-1 text-xs"
      />
      <textarea
        value={message}
        onChange={(e) => setMessage(e.target.value)}
        placeholder="Optional message"
        rows={2}
        className="w-full rounded border border-border bg-bg px-1 py-1 text-xs"
      />
      <button
        onClick={() => submit.mutate()}
        disabled={submit.isPending || !reviewers.trim()}
        className="flex w-full items-center justify-center gap-1 rounded bg-accent px-2 py-1 text-[10px] text-bg hover:opacity-90 disabled:opacity-40"
      >
        <Send size={10} /> {submit.isPending ? "Sending…" : "Request review"}
      </button>
      {error ? <div className="text-[10px] text-callout-error">{error}</div> : null}
    </div>
  );
}

function DecisionList({ decisions }: { decisions: ReviewDecision[] }) {
  if (decisions.length === 0) {
    return <span className="text-muted">No reviewers yet.</span>;
  }
  return (
    <ul className="space-y-0.5">
      {decisions.map((d) => (
        <li key={d.id} className="flex items-center gap-2">
          {d.decision === "approved" ? (
            <Check size={11} className="text-callout-success" />
          ) : d.decision === "rejected" ? (
            <X size={11} className="text-callout-error" />
          ) : (
            <Clock size={11} className="text-callout-warning" />
          )}
          <span className="font-mono text-[10px] text-muted">{d.reviewer_id}</span>
          {d.comment ? (
            <span className="ml-1 truncate text-muted">— {d.comment}</span>
          ) : null}
        </li>
      ))}
    </ul>
  );
}

function ApprovedSection({
  spaceID,
  pageID,
  decisions,
  onPublished,
}: {
  spaceID: string;
  pageID: string;
  decisions: ReviewDecision[];
  onPublished: () => void;
}) {
  const publish = useMutation({
    mutationFn: () => approvalApi.publish(spaceID, pageID),
    onSuccess: onPublished,
  });
  return (
    <div className="space-y-1 text-xs">
      <div className="flex items-center gap-1 text-callout-success">
        <Check size={11} /> Approved by every reviewer.
      </div>
      <DecisionList decisions={decisions.filter((d) => d.decision === "approved")} />
      <button
        onClick={() => publish.mutate()}
        disabled={publish.isPending}
        className="flex w-full items-center justify-center gap-1 rounded bg-callout-success px-2 py-1 text-[10px] text-bg hover:opacity-90 disabled:opacity-40"
      >
        <Upload size={10} /> {publish.isPending ? "Publishing…" : "Publish"}
      </button>
    </div>
  );
}

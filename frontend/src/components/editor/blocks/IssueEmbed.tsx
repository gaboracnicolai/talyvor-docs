import { useQuery } from "@tanstack/react-query";
import { Link2, Sparkles, AlertCircle } from "lucide-react";
import clsx from "clsx";
import { trackApi, type TrackIssue } from "~/api/track";
import { workspaceID } from "~/hooks/useSpaces";

interface IssueEmbedProps {
  // Stored on the ProseMirror node attributes — Track's UUID is the
  // load-bearing ID; identifier is a denormalised label so the chip
  // can render something before the network fetch resolves.
  issueID: string;
  identifier?: string;
  title?: string;
}

// statusTone maps Track's IssueStatus union to a Tailwind class
// pair. We don't import Track's StatusBadge directly because Docs
// has to keep working when Track is unconfigured.
const statusTone: Record<string, string> = {
  backlog: "border-border bg-bg text-muted",
  todo: "border-border bg-bg text-text",
  in_progress: "border-callout-info/40 bg-callout-info/10 text-callout-info",
  in_review: "border-callout-info/40 bg-callout-info/10 text-callout-info",
  done: "border-callout-success/40 bg-callout-success/10 text-callout-success",
  cancelled: "border-border bg-bg text-muted",
};

// Issue-embed chip. Live-fetched against /v1/track/issues/<id>; the
// 30-second server-side cache means a re-render is cheap, the per-
// page render count is bounded by the number of embeds, and a
// flaky Track never breaks the doc render.
export function IssueEmbed({ issueID, identifier, title }: IssueEmbedProps) {
  const wsID = workspaceID();
  const q = useQuery({
    queryKey: ["track-issue", wsID, issueID],
    queryFn: () => trackApi.getIssue(wsID, issueID),
    enabled: !!issueID,
    staleTime: 30_000,
  });

  // Skeleton while loading. The chip width matches the resolved
  // state so the document doesn't reflow when the fetch lands.
  if (q.isLoading) {
    return (
      <span className="inline-flex h-5 animate-pulse items-center gap-1 rounded border border-border bg-bg px-2 py-0.5 font-mono text-xs text-muted">
        <Link2 size={10} />
        {identifier || "loading…"}
      </span>
    );
  }

  // Track unconfigured → degrade to a static chip with whatever
  // identifier the editor captured at embed time.
  if (q.data && q.data.configured === false) {
    return (
      <span
        className="inline-flex items-center gap-1 rounded border border-border bg-bg px-2 py-0.5 font-mono text-xs text-muted"
        title="Track not configured"
      >
        <Link2 size={10} />
        {identifier || issueID.slice(0, 8)}
      </span>
    );
  }

  // Configured but the issue couldn't be fetched (deleted, network
  // blip). Surface the failure inline rather than hiding it — a
  // broken link in a spec should be visible.
  if (!q.data?.available || !q.data.issue) {
    return (
      <span
        className="inline-flex items-center gap-1 rounded border border-priority-urgent/40 bg-priority-urgent/10 px-2 py-0.5 font-mono text-xs text-callout-error"
        title={q.data?.error ?? "Issue not found"}
      >
        <AlertCircle size={10} />
        {identifier || issueID.slice(0, 8)}
      </span>
    );
  }

  const iss: TrackIssue = q.data.issue;
  const tone = statusTone[iss.status] ?? statusTone.backlog;
  const open = (e: React.MouseEvent) => {
    e.preventDefault();
    // Phase 4 doesn't assume Track lives on the same origin, so we
    // fall back to a synthesised path when no explicit issue URL is
    // attached. Phase 5 will get a smarter rule.
    const target = iss.url || `${window.location.origin}/track/issues/${iss.id}`;
    window.open(target, "_blank", "noopener,noreferrer");
  };

  return (
    <a
      href={iss.url || "#"}
      onClick={open}
      title={title || iss.title}
      className={clsx(
        "inline-flex items-center gap-1 rounded border px-2 py-0.5 font-mono text-xs",
        tone,
      )}
    >
      <Link2 size={10} />
      <span className="font-semibold">{iss.identifier}</span>
      <span className="font-sans text-muted">·</span>
      <span className="max-w-[12rem] truncate font-sans">{iss.title}</span>
      {iss.ai_cost_usd > 0 ? (
        <span className="ml-1 inline-flex items-center gap-0.5 rounded-full bg-accent/15 px-1 text-[10px] text-accent">
          <Sparkles size={9} />
          ${iss.ai_cost_usd.toFixed(2)}
        </span>
      ) : null}
    </a>
  );
}

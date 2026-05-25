import { useQuery } from "@tanstack/react-query";
import { MessageSquare } from "lucide-react";
import { commentsApi } from "~/api/comments";

interface BarProps {
  spaceID: string;
  pageID: string;
  onClick?: () => void;
}

// CommentStatsBar is the compact "💬 N open · M resolved" chip in
// the page header. Click → opens the right-rail comments panel.
// We poll lightly (60s) so the badge stays close to real-time even
// when other clients add or resolve threads.
export function CommentStatsBar({ spaceID, pageID, onClick }: BarProps) {
  const stats = useQuery({
    queryKey: ["comment-stats", pageID],
    queryFn: () => commentsApi.stats(spaceID, pageID),
    staleTime: 60_000,
  });
  const open = stats.data?.open ?? 0;
  const resolved = stats.data?.resolved ?? 0;
  if (open === 0 && resolved === 0) {
    return null;
  }
  return (
    <button
      onClick={onClick}
      className="inline-flex items-center gap-1 rounded border border-border bg-bg px-1.5 py-0.5 text-[10px] text-muted hover:border-accent hover:text-text"
      title="View comments"
    >
      <MessageSquare size={10} />
      <span>{open} open</span>
      <span>· {resolved} resolved</span>
    </button>
  );
}

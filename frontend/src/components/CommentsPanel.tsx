import { useState } from "react";
import { MessageSquare, Plus } from "lucide-react";
import { useComments } from "~/hooks/useComments";
import { CommentThread } from "./CommentThread";

interface PanelProps {
  spaceID: string;
  pageID: string;
}

// CommentsPanel is the right-rail companion to the page. Tabs split
// open vs resolved threads; the "New comment" composer lives at the
// top so reviewers can drop a top-level note without scrolling.
export function CommentsPanel({ spaceID, pageID }: PanelProps) {
  const [tab, setTab] = useState<"open" | "resolved">("open");
  const {
    threads,
    isLoading,
    stats,
    memberID,
    create,
    reply,
    resolve,
    unresolve,
    remove,
  } = useComments(spaceID, pageID, tab === "resolved");
  const [draft, setDraft] = useState("");

  const open = stats?.open ?? 0;
  const resolved = stats?.resolved ?? 0;

  return (
    <div className="space-y-2 text-xs">
      <nav className="flex items-center gap-1 border-b border-border pb-1">
        <button
          onClick={() => setTab("open")}
          className={`rounded px-1.5 py-0.5 text-[10px] uppercase tracking-wider ${
            tab === "open" ? "bg-accent text-bg" : "text-muted hover:text-text"
          }`}
        >
          Open ({open})
        </button>
        <button
          onClick={() => setTab("resolved")}
          className={`rounded px-1.5 py-0.5 text-[10px] uppercase tracking-wider ${
            tab === "resolved" ? "bg-accent text-bg" : "text-muted hover:text-text"
          }`}
        >
          Resolved ({resolved})
        </button>
      </nav>

      {/* New top-level comment */}
      <div className="flex items-center gap-1">
        <input
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          placeholder="Add a comment…"
          className="flex-1 rounded border border-border bg-bg px-1 py-1 text-xs"
          onKeyDown={(e) => {
            if (e.key === "Enter" && draft.trim()) {
              e.preventDefault();
              create.mutate({ content: draft.trim() });
              setDraft("");
            }
          }}
        />
        <button
          onClick={() => {
            if (draft.trim()) {
              create.mutate({ content: draft.trim() });
              setDraft("");
            }
          }}
          disabled={!draft.trim() || create.isPending}
          className="inline-flex items-center gap-1 rounded bg-accent px-2 py-1 text-[10px] text-bg hover:opacity-90 disabled:opacity-40"
        >
          <Plus size={10} /> Post
        </button>
      </div>

      {isLoading ? (
        <p className="text-muted">Loading…</p>
      ) : threads.length === 0 ? (
        <p className="flex items-center gap-1 text-muted">
          <MessageSquare size={11} />
          {tab === "open" ? "No open threads." : "No resolved threads."}
        </p>
      ) : (
        <div className="space-y-2">
          {threads.map((t) => (
            <CommentThread
              key={t.id}
              thread={t}
              currentMemberID={memberID}
              onReply={(commentID, content) => reply.mutate({ commentID, content })}
              onResolve={(commentID) => resolve.mutate(commentID)}
              onUnresolve={(commentID) => unresolve.mutate(commentID)}
              onDelete={(commentID) => remove.mutate(commentID)}
            />
          ))}
        </div>
      )}
    </div>
  );
}

import { useState } from "react";
import { Check, MessageSquare, Trash2, Undo } from "lucide-react";
import type { Comment } from "~/api/comments";

interface ThreadProps {
  thread: Comment;
  currentMemberID: string;
  onReply: (commentID: string, content: string) => void;
  onResolve: (commentID: string) => void;
  onUnresolve: (commentID: string) => void;
  onDelete: (commentID: string) => void;
}

// CommentThread renders one top-level comment plus its replies.
// Resolved threads pick up a grey tint + a resolver chip; the
// "Unresolve" affordance is available so reviewers can re-open a
// conversation that was closed prematurely.
export function CommentThread({
  thread,
  currentMemberID,
  onReply,
  onResolve,
  onUnresolve,
  onDelete,
}: ThreadProps) {
  const [replyDraft, setReplyDraft] = useState("");
  const showResolved = thread.resolved;

  return (
    <article
      className={`rounded border ${
        showResolved
          ? "border-border bg-bg/50 text-muted"
          : "border-border bg-surface text-text"
      } p-2 text-xs`}
    >
      <CommentRow comment={thread} currentMemberID={currentMemberID} onDelete={onDelete} />

      {(thread.replies ?? []).map((r) => (
        <div key={r.id} className="ml-6 mt-1 border-l border-border pl-2">
          <CommentRow comment={r} currentMemberID={currentMemberID} onDelete={onDelete} />
        </div>
      ))}

      {showResolved ? (
        <footer className="mt-2 flex items-center justify-between text-[10px] text-callout-success">
          <span>
            ✅ Resolved by {thread.resolved_by ?? "—"}
            {thread.resolved_at ? ` · ${relativeTime(thread.resolved_at)}` : ""}
          </span>
          <button
            onClick={() => onUnresolve(thread.id)}
            className="inline-flex items-center gap-1 text-muted hover:text-text"
          >
            <Undo size={10} /> Unresolve
          </button>
        </footer>
      ) : (
        <footer className="mt-2 space-y-1">
          <div className="flex items-center gap-1">
            <input
              value={replyDraft}
              onChange={(e) => setReplyDraft(e.target.value)}
              placeholder="Reply…"
              className="flex-1 rounded border border-border bg-bg px-1 py-1 text-xs"
              onKeyDown={(e) => {
                if (e.key === "Enter" && replyDraft.trim()) {
                  e.preventDefault();
                  onReply(thread.id, replyDraft.trim());
                  setReplyDraft("");
                }
              }}
            />
            <button
              onClick={() => {
                if (replyDraft.trim()) {
                  onReply(thread.id, replyDraft.trim());
                  setReplyDraft("");
                }
              }}
              disabled={!replyDraft.trim()}
              className="rounded bg-bg px-2 py-1 text-[10px] text-muted hover:text-text disabled:opacity-40"
            >
              Reply
            </button>
            <button
              onClick={() => onResolve(thread.id)}
              className="inline-flex items-center gap-1 rounded bg-callout-success px-2 py-1 text-[10px] text-bg hover:opacity-90"
              title="Resolve thread"
            >
              <Check size={10} /> Resolve
            </button>
          </div>
        </footer>
      )}
    </article>
  );
}

// CommentRow is one utterance — the top-level body or a reply.
// Markdown rendering is intentionally lightweight (bold/italic/code
// only) — anything richer would invite XSS without a full sanitiser.
function CommentRow({
  comment,
  currentMemberID,
  onDelete,
}: {
  comment: Comment;
  currentMemberID: string;
  onDelete: (id: string) => void;
}) {
  const isAuthor = comment.author_id === currentMemberID;
  return (
    <div>
      <header className="flex items-center gap-1 text-[10px]">
        <span className="inline-flex h-4 w-4 items-center justify-center rounded-full bg-accent/30 text-[9px] text-accent">
          {(comment.author_name || comment.author_id).slice(0, 2).toUpperCase()}
        </span>
        <span className="font-semibold">{comment.author_name || comment.author_id}</span>
        <span className="text-muted">· {relativeTime(comment.created_at)}</span>
        {isAuthor ? (
          <button
            onClick={() => onDelete(comment.id)}
            className="ml-auto text-muted hover:text-callout-error"
            title="Delete"
          >
            <Trash2 size={10} />
          </button>
        ) : null}
      </header>
      <div
        className="mt-0.5 whitespace-pre-wrap text-xs"
        dangerouslySetInnerHTML={{ __html: renderInlineMarkdown(comment.content) }}
      />
    </div>
  );
}

// renderInlineMarkdown is the tiny safe subset: **bold**, *italic*,
// `code`. Everything else is HTML-escaped so untrusted content can't
// break out of the markup.
function renderInlineMarkdown(text: string): string {
  const escaped = text
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;");
  return escaped
    .replace(/\*\*(.+?)\*\*/g, "<strong>$1</strong>")
    .replace(/\*(.+?)\*/g, "<em>$1</em>")
    .replace(/`([^`]+)`/g, "<code>$1</code>");
}

// relativeTime keeps the rendering self-contained — no date-fns
// dependency for what's effectively two minutes of code.
function relativeTime(iso: string): string {
  const then = new Date(iso).getTime();
  const diff = (Date.now() - then) / 1000;
  if (diff < 60) return "just now";
  if (diff < 3600) return `${Math.floor(diff / 60)}m ago`;
  if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`;
  if (diff < 86400 * 7) return `${Math.floor(diff / 86400)}d ago`;
  return new Date(iso).toLocaleDateString();
}

// Convenience re-export for the panel — the empty-state icon comes
// from lucide-react too.
export { MessageSquare };

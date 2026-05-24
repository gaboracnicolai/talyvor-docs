import { useEffect, useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Search, X } from "lucide-react";
import { trackApi, type TrackIssue } from "~/api/track";
import { workspaceID } from "~/hooks/useSpaces";
import { Input } from "~/components/ui/Input";

interface IssueSearchDialogProps {
  open: boolean;
  onPick: (issue: TrackIssue) => void;
  onClose: () => void;
}

// Modal picker for the slash-menu "Embed issue" + the right-panel
// "Link issue" affordances. Keyboard-first: type to filter, ↑/↓ to
// move, Enter to select, Esc to close.
export function IssueSearchDialog({ open, onPick, onClose }: IssueSearchDialogProps) {
  const [q, setQ] = useState("");
  const [idx, setIdx] = useState(0);

  useEffect(() => {
    if (open) {
      setQ("");
      setIdx(0);
    }
  }, [open]);

  const search = useQuery({
    queryKey: ["track-search", workspaceID(), q],
    queryFn: () => trackApi.search(workspaceID(), q),
    enabled: open && q.trim().length >= 2,
    staleTime: 5_000,
  });

  const items = useMemo(() => search.data?.issues ?? [], [search.data]);
  const configured = search.data?.configured !== false;

  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "ArrowDown") {
        e.preventDefault();
        setIdx((i) => Math.min(items.length - 1, i + 1));
      } else if (e.key === "ArrowUp") {
        e.preventDefault();
        setIdx((i) => Math.max(0, i - 1));
      } else if (e.key === "Enter") {
        e.preventDefault();
        const it = items[idx];
        if (it) onPick(it);
      } else if (e.key === "Escape") {
        e.preventDefault();
        onClose();
      }
    };
    window.addEventListener("keydown", onKey, true);
    return () => window.removeEventListener("keydown", onKey, true);
  }, [open, items, idx, onPick, onClose]);

  if (!open) return null;

  return (
    <div className="fixed inset-0 z-50 flex items-start justify-center bg-black/60 pt-32">
      <div className="w-full max-w-lg overflow-hidden rounded-md border border-border bg-surface shadow-2xl">
        <div className="flex items-center gap-2 border-b border-border px-3 py-2">
          <Search size={14} className="text-muted" />
          <Input
            autoFocus
            placeholder="Search Track issues…"
            value={q}
            onChange={(e) => setQ(e.target.value)}
            className="h-7 border-0 bg-transparent px-0 focus:ring-0"
          />
          <button onClick={onClose} className="text-muted hover:text-text">
            <X size={14} />
          </button>
        </div>
        <div className="max-h-80 overflow-y-auto p-1">
          {!configured ? (
            <Empty>Track is not configured. Set DOCS_TRACK_URL + DOCS_TRACK_API_KEY.</Empty>
          ) : q.trim().length < 2 ? (
            <Empty>Type at least 2 characters to search.</Empty>
          ) : search.isLoading ? (
            <Empty>Searching…</Empty>
          ) : items.length === 0 ? (
            <Empty>No matches for "{q}"</Empty>
          ) : (
            items.map((iss, i) => (
              <button
                key={iss.id}
                onClick={() => onPick(iss)}
                className={
                  "flex w-full items-center gap-2 rounded px-2 py-1.5 text-left text-xs " +
                  (i === idx ? "bg-bg text-text" : "text-muted hover:bg-bg/60 hover:text-text")
                }
              >
                <span className="w-20 shrink-0 font-mono">{iss.identifier}</span>
                <span className="flex-1 truncate text-text">{iss.title}</span>
                <span className="text-[10px] uppercase tracking-wider">{iss.status}</span>
                {iss.assignee_id ? (
                  <span className="text-[10px] text-muted">{iss.assignee_id.slice(0, 4)}</span>
                ) : null}
              </button>
            ))
          )}
        </div>
      </div>
    </div>
  );
}

function Empty({ children }: { children: React.ReactNode }) {
  return <div className="px-3 py-6 text-center text-xs text-muted">{children}</div>;
}

import { Link2 } from "lucide-react";

interface IssueEmbedProps {
  identifier: string;
  title?: string;
  // Optional click handler — the page chrome wires this to open the
  // issue in the host Track UI (or a popover).
  onClick?: () => void;
}

// Compact chip render of a Track issue link. Same visual as the
// editor's inline ".issue-embed" CSS so previews + the live editor
// stay consistent.
export function IssueEmbed({ identifier, title, onClick }: IssueEmbedProps) {
  return (
    <button
      type="button"
      onClick={onClick}
      className="inline-flex items-center gap-1 rounded border border-border bg-surface px-2 py-0.5 font-mono text-xs hover:border-accent"
      title={title || identifier}
    >
      <Link2 size={10} />
      {identifier}
    </button>
  );
}

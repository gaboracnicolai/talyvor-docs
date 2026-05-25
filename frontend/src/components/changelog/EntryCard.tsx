import { useState } from "react";
import { ChevronDown, ChevronRight, Send, Trash2 } from "lucide-react";
import type { ChangelogEntry } from "~/api/changelog";
import { TypeBadge } from "./TypeBadge";

// extractText walks a ProseMirror JSON doc and emits a markdown-ish
// plain text projection. Used for the expanded card body — full
// editor rendering would be overkill for a read-only timeline tile.
function extractText(pm: string): string {
  if (!pm || pm === "{}") return "";
  try {
    const doc = JSON.parse(pm);
    const lines: string[] = [];
    const walk = (node: unknown) => {
      if (!node || typeof node !== "object") return;
      const n = node as { type?: string; text?: string; attrs?: { level?: number }; content?: unknown[] };
      if (n.type === "heading") {
        const level = n.attrs?.level ?? 1;
        const text = (n.content ?? []).map(textOf).join("");
        lines.push(`${"#".repeat(level)} ${text}`);
        return;
      }
      if (n.type === "paragraph") {
        const text = (n.content ?? []).map(textOf).join("");
        if (text) lines.push(text);
        return;
      }
      if (n.type === "bullet_list" || n.type === "ordered_list") {
        for (const item of n.content ?? []) {
          walkListItem(item);
        }
        return;
      }
      for (const child of n.content ?? []) walk(child);
    };
    const walkListItem = (raw: unknown) => {
      if (!raw || typeof raw !== "object") return;
      const item = raw as { content?: unknown[] };
      for (const child of item.content ?? []) {
        if (!child || typeof child !== "object") continue;
        const c = child as { type?: string; content?: unknown[] };
        if (c.type === "paragraph") {
          lines.push(`- ${(c.content ?? []).map(textOf).join("")}`);
        }
      }
    };
    const textOf = (raw: unknown): string => {
      if (!raw || typeof raw !== "object") return "";
      const t = raw as { type?: string; text?: string };
      return t.type === "text" ? t.text ?? "" : "";
    };
    walk(doc);
    return lines.join("\n");
  } catch {
    return "";
  }
}

interface CardProps {
  entry: ChangelogEntry;
  onPublish: (id: string) => void;
  onDelete: (id: string) => void;
}

// EntryCard is a single timeline tile. Collapsed by default — the
// header is always visible (version + type + title); expanding
// reveals the rendered content + linked issues + publish action.
export function EntryCard({ entry, onPublish, onDelete }: CardProps) {
  const [open, setOpen] = useState(false);
  const isDraft = !entry.published_at;
  return (
    <article
      className={`rounded border ${
        isDraft ? "border-callout-warning/40" : "border-border"
      } bg-surface p-3 text-xs`}
    >
      <header className="flex items-center gap-2">
        <button
          onClick={() => setOpen((v) => !v)}
          className="text-muted hover:text-text"
        >
          {open ? <ChevronDown size={12} /> : <ChevronRight size={12} />}
        </button>
        <span className="font-mono text-sm font-semibold">{entry.version}</span>
        <TypeBadge type={entry.type} />
        <span className="flex-1 truncate text-text">{entry.title}</span>
        {isDraft ? (
          <span className="rounded bg-callout-warning/20 px-1.5 py-0.5 text-[10px] text-callout-warning">
            Draft
          </span>
        ) : (
          <span className="text-[10px] text-muted">
            {new Date(entry.published_at!).toLocaleDateString()}
          </span>
        )}
      </header>
      {entry.summary ? (
        <div className="mt-1 ml-5 text-muted">{entry.summary}</div>
      ) : null}
      {open ? (
        <div className="mt-2 ml-5 space-y-2">
          <pre className="whitespace-pre-wrap rounded border border-border bg-bg p-2 text-[11px] text-text">
            {extractText(entry.content)}
          </pre>
          {entry.issue_ids.length > 0 ? (
            <div className="text-[10px] text-muted">
              Linked issues: {entry.issue_ids.join(", ")}
            </div>
          ) : null}
          <div className="flex items-center gap-1">
            {isDraft ? (
              <button
                onClick={() => onPublish(entry.id)}
                className="inline-flex items-center gap-1 rounded bg-accent px-2 py-1 text-[10px] text-bg hover:opacity-90"
              >
                <Send size={10} /> Publish
              </button>
            ) : null}
            <button
              onClick={() => onDelete(entry.id)}
              className="ml-auto text-muted hover:text-callout-error"
              title="Delete"
            >
              <Trash2 size={10} />
            </button>
          </div>
        </div>
      ) : null}
    </article>
  );
}

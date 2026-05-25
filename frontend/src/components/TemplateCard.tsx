import { TrendingUp, Trash2 } from "lucide-react";
import type { LibraryTemplate } from "~/api/templates";

interface CardProps {
  template: LibraryTemplate;
  onUse: () => void;
  onDelete?: () => void;
  busy?: boolean;
}

// TemplateCard is one tile in the gallery grid: icon + name +
// description, the use-count + built-in badge, and a primary CTA
// that creates a page from the template. Custom workspace templates
// also expose a delete affordance; built-ins do not.
export function TemplateCard({ template, onUse, onDelete, busy }: CardProps) {
  return (
    <div className="flex h-full flex-col rounded-md border border-border bg-surface p-3">
      <header className="mb-1 flex items-center gap-2">
        <span className="text-2xl">{template.icon}</span>
        <div className="min-w-0 flex-1">
          <div className="truncate text-sm font-semibold">{template.name}</div>
          <div className="text-[10px] uppercase tracking-wider text-muted">
            {template.category}
          </div>
        </div>
        {template.is_built_in ? (
          <span className="rounded bg-bg px-1 py-px text-[10px] text-muted">
            Built-in
          </span>
        ) : null}
      </header>
      <p className="mb-2 line-clamp-3 text-xs text-muted">
        {template.description || "No description."}
      </p>
      <footer className="mt-auto flex items-center justify-between gap-2">
        <span className="flex items-center gap-1 text-[10px] text-muted">
          <TrendingUp size={10} /> {template.use_count} uses
        </span>
        <div className="flex items-center gap-1">
          {!template.is_built_in && onDelete ? (
            <button
              onClick={onDelete}
              className="text-muted hover:text-callout-error"
              title="Delete custom template"
            >
              <Trash2 size={11} />
            </button>
          ) : null}
          <button
            onClick={onUse}
            disabled={busy}
            className="rounded bg-accent px-2 py-1 text-[10px] text-bg hover:opacity-90 disabled:opacity-40"
          >
            {busy ? "Creating…" : "Use template"}
          </button>
        </div>
      </footer>
    </div>
  );
}

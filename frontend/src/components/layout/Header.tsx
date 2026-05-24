import { Search } from "lucide-react";

interface HeaderProps {
  // Breadcrumb segments — empty array on the home screen.
  breadcrumbs: { label: string; onClick?: () => void }[];
  // onOpenSearch opens the global SearchModal. The host owns the
  // open state so the modal can be triggered from anywhere (Cmd+K).
  onOpenSearch: () => void;
}

// Slim top bar. Breadcrumbs on the left, a SearchModal trigger on
// the right. Phase 6 replaces the inline filter input with a
// Cmd+K-driven modal so we can show ranked + semantic results.
export function Header({ breadcrumbs, onOpenSearch }: HeaderProps) {
  return (
    <header className="flex h-12 shrink-0 items-center justify-between border-b border-border bg-surface px-4">
      <nav className="flex items-center gap-2 text-sm">
        {breadcrumbs.length === 0 ? (
          <span className="text-muted">Home</span>
        ) : (
          breadcrumbs.map((b, i) => (
            <span key={i} className="flex items-center gap-2">
              {i > 0 ? <span className="text-muted">/</span> : null}
              {b.onClick ? (
                <button onClick={b.onClick} className="text-muted hover:text-text">
                  {b.label}
                </button>
              ) : (
                <span className="text-text">{b.label}</span>
              )}
            </span>
          ))
        )}
      </nav>
      <button
        onClick={onOpenSearch}
        className="flex h-8 items-center gap-2 rounded border border-border bg-bg px-2 text-xs text-muted hover:border-accent"
      >
        <Search size={12} />
        <span className="w-40 text-left">Search docs…</span>
        <kbd className="rounded border border-border bg-surface px-1 py-px text-[10px]">
          ⌘K
        </kbd>
      </button>
    </header>
  );
}

import { Search } from "lucide-react";
import { Input } from "~/components/ui/Input";

interface HeaderProps {
  // Breadcrumb segments — empty array on the home screen.
  breadcrumbs: { label: string; onClick?: () => void }[];
  onSearch: (q: string) => void;
}

// Slim top bar. Breadcrumb on the left, search on the right. The
// search box is a controlled input handled by the parent so the
// query can drive the results panel above the route content.
export function Header({ breadcrumbs, onSearch }: HeaderProps) {
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
      <div className="flex items-center gap-2">
        <Search size={12} className="text-muted" />
        <Input
          placeholder="Search docs…"
          onChange={(e) => onSearch(e.target.value)}
          className="h-8 w-64 text-xs"
        />
      </div>
    </header>
  );
}

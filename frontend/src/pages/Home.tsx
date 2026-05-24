import { useSpaces } from "~/hooks/useSpaces";
import { useSearchPages } from "~/hooks/usePage";
import type { Page, Space } from "~/api/types";

interface HomeProps {
  searchQuery: string;
  onOpenSpace: (space: Space) => void;
  onOpenPageById: (spaceID: string, pageID: string) => void;
}

// Workspace home — three sections:
//   1. live search results when the header search box has ≥ 2 chars
//   2. recent pages (Phase 2 stub: pulls the first space's pages)
//   3. all spaces grid
export function HomePage({ searchQuery, onOpenSpace, onOpenPageById }: HomeProps) {
  const spaces = useSpaces();
  const search = useSearchPages(searchQuery);

  return (
    <div className="max-w-4xl space-y-8 p-8">
      {searchQuery.trim().length >= 2 ? (
        <section>
          <h2 className="mb-3 text-sm font-semibold">Search results</h2>
          {search.isLoading ? (
            <p className="text-xs text-muted">Searching…</p>
          ) : (search.data ?? []).length === 0 ? (
            <p className="text-xs text-muted">No matches for "{searchQuery}".</p>
          ) : (
            <div className="space-y-1">
              {search.data!.map((p: Page) => (
                <button
                  key={p.id}
                  onClick={() => onOpenPageById(p.space_id, p.id)}
                  className="flex w-full items-center justify-between rounded-md border border-border bg-surface p-3 text-left hover:border-accent"
                >
                  <div>
                    <div className="text-sm font-medium">
                      {p.icon ? `${p.icon} ` : ""}
                      {p.title}
                    </div>
                    <div className="line-clamp-1 text-[10px] text-muted">
                      {p.content_text}
                    </div>
                  </div>
                  <span className="text-[10px] text-muted">
                    {new Date(p.updated_at).toLocaleDateString()}
                  </span>
                </button>
              ))}
            </div>
          )}
        </section>
      ) : null}

      <section>
        <h2 className="mb-3 text-sm font-semibold">Spaces</h2>
        {spaces.isLoading ? (
          <p className="text-xs text-muted">Loading…</p>
        ) : (spaces.data ?? []).length === 0 ? (
          <p className="text-xs text-muted">
            No spaces yet — create one from the sidebar.
          </p>
        ) : (
          <div className="grid grid-cols-1 gap-2 sm:grid-cols-2 lg:grid-cols-3">
            {spaces.data!.map((sp) => (
              <button
                key={sp.id}
                onClick={() => onOpenSpace(sp)}
                className="flex flex-col items-start gap-1 rounded-md border border-border bg-surface p-4 text-left hover:border-accent"
                style={{ borderLeft: `3px solid ${sp.color}` }}
              >
                <div className="text-xl">{sp.icon}</div>
                <div className="text-sm font-semibold">{sp.name}</div>
                <div className="line-clamp-2 text-[10px] text-muted">
                  {sp.description || "No description yet."}
                </div>
              </button>
            ))}
          </div>
        )}
      </section>
    </div>
  );
}

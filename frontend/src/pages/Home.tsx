import { useSpaces } from "~/hooks/useSpaces";
import type { Space } from "~/api/types";

interface HomeProps {
  onOpenSpace: (space: Space) => void;
}

// Workspace home — Phase 6 retires the inline search panel in favour
// of the global Cmd+K SearchModal. The home view now focuses on
// space navigation; search results live in the modal.
export function HomePage({ onOpenSpace }: HomeProps) {
  const spaces = useSpaces();

  return (
    <div className="max-w-4xl space-y-8 p-8">
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

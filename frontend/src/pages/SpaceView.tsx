import { Plus, FileText } from "lucide-react";
import type { Page, Space } from "~/api/types";
import { usePages, useCreatePage } from "~/hooks/usePage";
import { Button } from "~/components/ui/Button";

interface SpaceViewProps {
  space: Space;
  onOpenPage: (page: Page) => void;
}

// Space landing page: title + description + flat page list. Phase 2
// keeps this simple — Phase 3 will turn the tree view in the
// sidebar into a draggable, deeply-nested explorer.
export function SpaceViewPage({ space, onOpenPage }: SpaceViewProps) {
  const pages = usePages(space.id);
  const create = useCreatePage(space.id);

  return (
    <div className="max-w-4xl space-y-6 p-8">
      <header className="space-y-2 border-b border-border pb-4">
        <div className="flex items-center gap-3">
          <span className="text-3xl">{space.icon}</span>
          <h1 className="text-2xl font-semibold">{space.name}</h1>
        </div>
        {space.description ? (
          <p className="text-sm text-muted">{space.description}</p>
        ) : null}
      </header>

      <section>
        <div className="mb-3 flex items-center justify-between">
          <h2 className="text-sm font-semibold">Pages</h2>
          <Button
            size="sm"
            onClick={() =>
              create.mutate(
                { title: "Untitled" },
                {
                  onSuccess: (p) => onOpenPage(p),
                },
              )
            }
          >
            <Plus size={12} /> New page
          </Button>
        </div>
        {pages.isLoading ? (
          <p className="text-xs text-muted">Loading…</p>
        ) : (pages.data ?? []).length === 0 ? (
          <p className="text-xs text-muted">
            No pages yet — click "New page" to create the first one.
          </p>
        ) : (
          <div className="space-y-1">
            {pages.data!.map((p) => (
              <button
                key={p.id}
                onClick={() => onOpenPage(p)}
                className="flex w-full items-center gap-2 rounded-md border border-border bg-surface p-3 text-left hover:border-accent"
              >
                <FileText size={12} className="text-muted" />
                <span className="text-sm">
                  {p.icon ? `${p.icon} ` : ""}
                  {p.title}
                </span>
                <span className="ml-auto text-[10px] text-muted">
                  {new Date(p.updated_at).toLocaleDateString()}
                </span>
              </button>
            ))}
          </div>
        )}
      </section>
    </div>
  );
}

import { Plus, FileText, Eye, Users, AlertCircle } from "lucide-react";
import { useQuery } from "@tanstack/react-query";
import type { Page, Space } from "~/api/types";
import { analyticsApi } from "~/api/analytics";
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

      <SpaceReadership spaceID={space.id} />
    </div>
  );
}

/**
 * SpaceReadership — the SPACE roll-up, the third of the three scopes
 * talyvor.higgsfield.app/products/docs sells ("PAGE, SPACE AND ORG ROLLUPS").
 *
 * ⚠ IT IS HERE BECAUSE A ROUTE WITHOUT A SURFACE IS NOT A FEATURE. The org scope has had a screen
 * since Analytics.tsx existed and the page scope has one on the page itself; the space scope had
 * neither until this change. Serving it and stopping would repeat, in the same repo on the same
 * afternoon, the defect the previous merge fixed — a figure computed correctly in Go and reachable
 * by nobody.
 *
 * ⚠ THE HEADING SAYS "IN THIS SPACE", AND THAT WORDING IS LOAD-BEARING. The response type is
 * `WorkspaceReadStats`, shared with the org roll-up because server-side the two are one statement
 * narrowed; the same four figures under an unqualified heading would read as the WORKSPACE's,
 * which is exactly the confusion the server-side scope exists to prevent, reintroduced in words.
 *
 * ⚠ AND A FAILED READ IS NOT ZERO READERSHIP. A space nobody has opened is an ordinary answer; a
 * roll-up that could not be read is not an answer at all, and printing `0 views` for both states a
 * fact this screen does not have. The section withdraws rather than inventing a number — the same
 * distinction `search.Result` draws for its three cost fields, and the reason they carry
 * `omitempty`.
 */
function SpaceReadership({ spaceID }: { spaceID: string }) {
  const stats = useQuery({
    queryKey: ["space-analytics", spaceID],
    queryFn: () => analyticsApi.spaceStats(spaceID, 30),
  });
  if (!stats.data) return null;
  const d = stats.data;
  return (
    <section data-testid="space-rollup">
      <h2 className="mb-3 text-sm font-semibold">Readership in this space (30d)</h2>
      <div className="grid grid-cols-3 gap-2">
        <RollupStat icon={<Eye size={12} />} label="Views" value={d.total_views} />
        <RollupStat icon={<Users size={12} />} label="Unique visitors" value={d.unique_viewers} />
        <RollupStat
          icon={<AlertCircle size={12} />}
          label="Never read"
          value={d.never_read_count}
        />
      </div>
      {d.most_read_pages.length > 0 ? (
        <div className="mt-3">
          <h3 className="mb-1 text-xs font-semibold text-muted">Most read</h3>
          <ul className="space-y-1">
            {d.most_read_pages.map((p) => (
              <li
                key={p.page_id}
                className="flex items-center justify-between rounded-md border border-border bg-surface px-3 py-2 text-sm"
              >
                <span>{p.title}</span>
                <span className="text-[10px] text-muted">{p.total_views} views</span>
              </li>
            ))}
          </ul>
        </div>
      ) : (
        // Reached only when the read SUCCEEDED and found nothing — the failed case returned above.
        <p className="mt-3 text-xs text-muted">
          No pages in this space have been opened in the last 30 days.
        </p>
      )}
    </section>
  );
}

function RollupStat({
  icon,
  label,
  value,
}: {
  icon: React.ReactNode;
  label: string;
  value: number;
}) {
  return (
    <div className="rounded-md border border-border bg-surface p-3">
      <div className="flex items-center gap-1 text-[10px] text-muted">
        {icon} {label}
      </div>
      <div className="mt-1 text-lg font-semibold">{value}</div>
    </div>
  );
}

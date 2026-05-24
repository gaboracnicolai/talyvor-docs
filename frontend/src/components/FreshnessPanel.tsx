import { useMutation, useQueryClient } from "@tanstack/react-query";
import { CheckCircle2, X } from "lucide-react";
import { freshnessApi, type FreshnessReport } from "~/api/freshness";
import { pagesApi } from "~/api/pages";
import { useUpdatePage } from "~/hooks/usePage";

interface PanelProps {
  spaceID: string;
  pageID: string;
  report: FreshnessReport | null | undefined;
  isLoading: boolean;
  onClose: () => void;
}

const EXPIRY_PRESETS: { label: string; days: number }[] = [
  { label: "30 days", days: 30 },
  { label: "90 days", days: 90 },
  { label: "1 year", days: 365 },
  { label: "Never", days: 0 },
];

// FreshnessPanel is the popover that opens when the user clicks the
// FreshnessBadge. It shows the full report + lets owners change the
// TTL or mark the page as verified. Verifying creates a new page
// version server-side; the popover relies on the existing /verify
// endpoint via pagesApi.
export function FreshnessPanel({ spaceID, pageID, report, isLoading, onClose }: PanelProps) {
  const qc = useQueryClient();
  const updatePage = useUpdatePage(spaceID, pageID);

  const verify = useMutation({
    mutationFn: () => pagesApi.verify(spaceID, pageID),
    onSuccess: () => {
      // Both the freshness report AND the page object change (the
      // latter via last_verified_at), so invalidate both caches.
      qc.invalidateQueries({ queryKey: ["freshness", pageID] });
      qc.invalidateQueries({ queryKey: ["page", spaceID, pageID] });
    },
  });

  const setExpiry = (days: number) => {
    updatePage.mutate({ stale_after_days: days });
    // Mutation invalidates the page; freshness will refetch next render.
    qc.invalidateQueries({ queryKey: ["freshness", pageID] });
  };

  return (
    <div className="absolute right-0 top-7 z-30 w-72 rounded-md border border-border bg-surface p-3 shadow-lg">
      <header className="mb-2 flex items-center justify-between">
        <div className="text-xs font-semibold">Freshness</div>
        <button onClick={onClose} className="text-muted hover:text-text">
          <X size={12} />
        </button>
      </header>
      {isLoading || !report ? (
        <div className="text-xs text-muted">Loading…</div>
      ) : (
        <div className="space-y-2 text-xs">
          <div>
            Last edited{" "}
            <span className="text-text">
              {report.days_since_edit} day{report.days_since_edit === 1 ? "" : "s"} ago
            </span>
            .
          </div>
          {typeof report.days_since_verify === "number" && report.verified_by ? (
            <div className="text-muted">
              Verified by{" "}
              <span className="text-text">{report.verified_by}</span> ·{" "}
              {report.days_since_verify} day{report.days_since_verify === 1 ? "" : "s"} ago
            </div>
          ) : null}
          {report.linked_issues_closed > 0 ? (
            <div className="rounded border border-callout-warning/30 bg-callout-warning/10 px-2 py-1 text-callout-warning">
              {report.linked_issues_closed} linked issue
              {report.linked_issues_closed === 1 ? "" : "s"} completed since last edit
            </div>
          ) : null}
          {report.reason && report.linked_issues_closed === 0 ? (
            <div className="text-muted">{report.reason}</div>
          ) : null}
          <div className="pt-1">
            <div className="mb-1 text-[10px] uppercase tracking-wider text-muted">
              Set expiry
            </div>
            <div className="flex flex-wrap gap-1">
              {EXPIRY_PRESETS.map((p) => (
                <button
                  key={p.label}
                  onClick={() => setExpiry(p.days)}
                  className={`rounded border px-1.5 py-0.5 text-[10px] ${
                    report.stale_after_days === p.days
                      ? "border-accent text-accent"
                      : "border-border text-muted hover:border-accent"
                  }`}
                >
                  {p.label}
                </button>
              ))}
            </div>
          </div>
          <button
            onClick={() => verify.mutate()}
            disabled={verify.isPending}
            className="mt-1 flex w-full items-center justify-center gap-1 rounded bg-accent px-2 py-1 text-[10px] text-bg hover:opacity-90 disabled:opacity-40"
          >
            <CheckCircle2 size={10} />
            {verify.isPending ? "Verifying…" : "Mark as verified"}
          </button>
        </div>
      )}
    </div>
  );
}

// Caller signature parity with React Query — exposed so the host
// page can refetch freshness without re-declaring the query key.
export const FRESHNESS_QUERY_KEY = (pageID: string) => ["freshness", pageID];
export const freshnessFetcher = (spaceID: string, pageID: string) =>
  freshnessApi.forPage(spaceID, pageID);

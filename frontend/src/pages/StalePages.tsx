import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { CheckCircle2, AlertCircle, Clock } from "lucide-react";
import { freshnessApi, type FreshnessReport, type FreshnessStatus } from "~/api/freshness";
import { pagesApi } from "~/api/pages";

interface StalePagesProps {
  workspaceID: string;
  onOpenPage: (spaceID: string, pageID: string) => void;
}

const TONE: Record<
  FreshnessStatus,
  { label: string; bg: string; icon: React.ReactNode }
> = {
  stale: { label: "Stale", bg: "border-callout-error/40 bg-callout-error/10", icon: <AlertCircle size={11} className="text-callout-error" /> },
  warning: { label: "Review", bg: "border-callout-warning/40 bg-callout-warning/10", icon: <Clock size={11} className="text-callout-warning" /> },
  fresh: { label: "Fresh", bg: "border-callout-success/40 bg-callout-success/10", icon: <CheckCircle2 size={11} className="text-callout-success" /> },
  unknown: { label: "No expiry", bg: "border-border bg-surface", icon: <Clock size={11} className="text-muted" /> },
};

// StalePagesPage surfaces every page in the workspace that the
// freshness engine flagged as stale or warning. Each row has a
// quick "Verify" affordance so doc owners can knock these out
// without opening every page individually.
export function StalePagesPage({ workspaceID, onOpenPage }: StalePagesProps) {
  const qc = useQueryClient();
  const { data, isLoading } = useQuery({
    queryKey: ["workspace-freshness", workspaceID],
    queryFn: () => freshnessApi.forWorkspace(workspaceID),
  });

  return (
    <div className="mx-auto max-w-4xl space-y-4 p-8">
      <header>
        <h1 className="text-lg font-semibold">Needs review</h1>
        <p className="text-xs text-muted">
          Pages flagged by freshness rules — past their stale_after_days
          threshold, or with linked Track issues completed since last edit.
        </p>
      </header>
      {isLoading ? (
        <p className="text-xs text-muted">Loading…</p>
      ) : (data ?? []).length === 0 ? (
        <p className="text-xs text-muted">All clear — nothing needs review.</p>
      ) : (
        <ul className="space-y-2">
          {data!.map((r) => (
            <StaleRow
              key={r.page_id}
              report={r}
              onOpen={() => onOpenPage(r.space_id, r.page_id)}
              onVerified={() => {
                qc.invalidateQueries({ queryKey: ["workspace-freshness", workspaceID] });
                qc.invalidateQueries({ queryKey: ["freshness", r.page_id] });
              }}
            />
          ))}
        </ul>
      )}
    </div>
  );
}

function StaleRow({
  report,
  onOpen,
  onVerified,
}: {
  report: FreshnessReport;
  onOpen: () => void;
  onVerified: () => void;
}) {
  const verify = useMutation({
    mutationFn: () => pagesApi.verify(report.space_id, report.page_id),
    onSuccess: onVerified,
  });
  const t = TONE[report.status];
  return (
    <li className={`flex items-center justify-between rounded border ${t.bg} px-3 py-2`}>
      <button onClick={onOpen} className="flex flex-1 items-center gap-2 text-left">
        {t.icon}
        <div className="flex-1">
          <div className="text-sm">{report.title}</div>
          <div className="text-[10px] text-muted">
            {report.reason || `Last edit ${report.days_since_edit} days ago`}
          </div>
        </div>
      </button>
      <button
        onClick={() => verify.mutate()}
        disabled={verify.isPending}
        className="ml-2 flex items-center gap-1 rounded bg-accent px-2 py-1 text-[10px] text-bg hover:opacity-90 disabled:opacity-40"
      >
        <CheckCircle2 size={10} />
        {verify.isPending ? "Verifying…" : "Verify"}
      </button>
    </li>
  );
}

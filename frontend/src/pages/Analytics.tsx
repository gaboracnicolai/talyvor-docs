import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Eye, Users, Clock, TrendingUp, AlertCircle } from "lucide-react";
import { analyticsApi, type DayCount, type ReadStats } from "~/api/analytics";

interface AnalyticsProps {
  workspaceID: string;
  // Optional per-page context. When set, the Page Analytics tab is
  // pre-selected and shows that page's stats.
  page?: { spaceID: string; pageID: string; title: string };
}

type Tab = "page" | "workspace";

// AnalyticsPage hosts the two readership tabs:
//   1. Page Analytics — focused on the current page (line chart of
//      views/day, top viewers, avg dwell time).
//   2. Workspace Analytics — most read / least read / never read,
//      aggregated across the workspace.
export function AnalyticsPage({ workspaceID, page }: AnalyticsProps) {
  const [tab, setTab] = useState<Tab>(page ? "page" : "workspace");

  return (
    <div className="mx-auto max-w-5xl space-y-4 p-8">
      <h1 className="text-lg font-semibold">Analytics</h1>
      <nav className="flex gap-1 border-b border-border">
        {(page ? (["page", "workspace"] as Tab[]) : (["workspace", "page"] as Tab[])).map((t) => (
          <button
            key={t}
            onClick={() => setTab(t)}
            disabled={t === "page" && !page}
            className={`px-3 py-1.5 text-xs ${
              tab === t
                ? "border-b-2 border-accent text-accent"
                : "text-muted hover:text-text"
            } disabled:opacity-40`}
          >
            {t === "page" ? "This page" : "Workspace"}
          </button>
        ))}
      </nav>
      {tab === "page" && page ? (
        <PageAnalytics spaceID={page.spaceID} pageID={page.pageID} title={page.title} />
      ) : (
        <WorkspaceAnalytics workspaceID={workspaceID} />
      )}
    </div>
  );
}

function PageAnalytics({ spaceID, pageID, title }: { spaceID: string; pageID: string; title: string }) {
  const { data, isLoading } = useQuery({
    queryKey: ["page-analytics", pageID],
    queryFn: () => analyticsApi.pageStats(spaceID, pageID, 30),
  });
  if (isLoading || !data) return <p className="text-xs text-muted">Loading…</p>;
  return (
    <section className="space-y-4">
      <header className="text-sm">
        <div className="font-semibold">{title}</div>
        <div className="text-muted">Last 30 days</div>
      </header>
      <div className="grid grid-cols-4 gap-2">
        <Stat icon={<Eye size={12} />} label="Views" value={data.total_views} />
        <Stat icon={<Users size={12} />} label="Unique" value={data.unique_viewers} />
        <Stat icon={<Clock size={12} />} label="Avg" value={`${data.avg_duration_sec}s`} />
        <Stat
          icon={<TrendingUp size={12} />}
          label="Day peak"
          value={Math.max(0, ...data.views_by_day.map((d) => d.count))}
        />
      </div>
      <ViewsLineChart points={data.views_by_day} />
      <div>
        <h2 className="mb-1 text-xs font-semibold">Top viewers</h2>
        {data.top_viewers.length === 0 ? (
          <p className="text-xs text-muted">No views yet.</p>
        ) : (
          <ul className="space-y-1 text-xs">
            {data.top_viewers.map((v) => (
              <li key={v.viewer_id} className="flex items-center justify-between rounded border border-border px-2 py-1">
                <span className="flex items-center gap-2">
                  <span className="inline-flex h-5 w-5 items-center justify-center rounded-full bg-accent/30 text-[10px] text-accent">
                    {(v.viewer_name || v.viewer_id).slice(0, 2).toUpperCase()}
                  </span>
                  {v.viewer_name || v.viewer_id}
                </span>
                <span className="text-muted">{v.view_count} views</span>
              </li>
            ))}
          </ul>
        )}
      </div>
    </section>
  );
}

function WorkspaceAnalytics({ workspaceID }: { workspaceID: string }) {
  const { data, isLoading } = useQuery({
    queryKey: ["workspace-analytics", workspaceID],
    queryFn: () => analyticsApi.workspaceStats(workspaceID, 30),
  });
  if (isLoading || !data) return <p className="text-xs text-muted">Loading…</p>;
  return (
    <section className="space-y-4">
      <div className="grid grid-cols-3 gap-2">
        <Stat icon={<Eye size={12} />} label="Views (30d)" value={data.total_views} />
        <Stat icon={<Users size={12} />} label="Unique visitors" value={data.unique_viewers} />
        <Stat
          icon={<AlertCircle size={12} />}
          label="Never read"
          value={data.never_read_count}
          tone="warning"
        />
      </div>
      <PageList title="Most read" items={data.most_read_pages} empty="No views yet." />
      <PageList
        title="Needs attention (lowest read)"
        items={data.least_read_pages}
        empty="All pages have plenty of traffic."
      />
    </section>
  );
}

function Stat({
  icon,
  label,
  value,
  tone,
}: {
  icon: React.ReactNode;
  label: string;
  value: React.ReactNode;
  tone?: "warning";
}) {
  return (
    <div
      className={`rounded border border-border bg-surface px-3 py-2 ${
        tone === "warning" ? "border-callout-warning/40" : ""
      }`}
    >
      <div className="flex items-center gap-1 text-[10px] uppercase tracking-wider text-muted">
        {icon}
        {label}
      </div>
      <div className="mt-0.5 text-sm font-semibold">{value}</div>
    </div>
  );
}

function PageList({ title, items, empty }: { title: string; items: ReadStats[]; empty: string }) {
  return (
    <div>
      <h2 className="mb-1 text-xs font-semibold">{title}</h2>
      {items.length === 0 ? (
        <p className="text-xs text-muted">{empty}</p>
      ) : (
        <ul className="space-y-1 text-xs">
          {items.map((p) => (
            <li
              key={p.page_id}
              className="flex items-center justify-between rounded border border-border px-2 py-1"
            >
              <span className="truncate">{p.title || p.page_id.slice(0, 8)}</span>
              <span className="text-muted">{p.total_views} views</span>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

// ViewsLineChart renders the views-by-day series as a hand-rolled SVG.
// We deliberately avoid recharts to keep the bundle small — the
// chart is read-only and the data has at most 30 points.
function ViewsLineChart({ points }: { points: DayCount[] }) {
  if (points.length === 0) {
    return <div className="rounded border border-border bg-surface p-3 text-xs text-muted">No data yet.</div>;
  }
  const width = 600;
  const height = 120;
  const padX = 12;
  const padY = 12;
  const xs = points.map((_, i) => padX + (i * (width - padX * 2)) / Math.max(points.length - 1, 1));
  const max = Math.max(1, ...points.map((p) => p.count));
  const ys = points.map(
    (p) => padY + (height - padY * 2) * (1 - p.count / max),
  );
  const path = points
    .map((_, i) => `${i === 0 ? "M" : "L"}${xs[i].toFixed(1)},${ys[i].toFixed(1)}`)
    .join(" ");
  return (
    <div className="rounded border border-border bg-surface p-2">
      <svg viewBox={`0 0 ${width} ${height}`} className="h-32 w-full">
        <path d={path} fill="none" stroke="currentColor" className="text-accent" strokeWidth={1.5} />
        {points.map((p, i) => (
          <circle
            key={i}
            cx={xs[i]}
            cy={ys[i]}
            r={2}
            className="fill-accent"
          >
            <title>{`${new Date(p.date).toLocaleDateString()}: ${p.count} views`}</title>
          </circle>
        ))}
      </svg>
    </div>
  );
}

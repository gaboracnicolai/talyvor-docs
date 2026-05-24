import type { FreshnessReport, FreshnessStatus } from "~/api/freshness";

interface BadgeProps {
  status: FreshnessStatus;
  daysSinceEdit?: number;
  onClick?: () => void;
}

// Per-status colour + emoji. Tailwind classes pull from the design
// tokens so the badge matches callouts elsewhere in the editor.
const TONE: Record<
  FreshnessStatus,
  { emoji: string; label: string; bg: string; text: string }
> = {
  fresh: { emoji: "🟢", label: "Fresh", bg: "bg-callout-success/15", text: "text-callout-success" },
  warning: { emoji: "🟡", label: "Review suggested", bg: "bg-callout-warning/15", text: "text-callout-warning" },
  stale: { emoji: "🔴", label: "Stale", bg: "bg-callout-error/15", text: "text-callout-error" },
  unknown: { emoji: "⚪", label: "No expiry", bg: "bg-bg", text: "text-muted" },
};

export function FreshnessBadge({ status, daysSinceEdit, onClick }: BadgeProps) {
  const t = TONE[status];
  return (
    <button
      onClick={onClick}
      title={
        typeof daysSinceEdit === "number"
          ? `${t.label} · last updated ${daysSinceEdit} day${daysSinceEdit === 1 ? "" : "s"} ago`
          : t.label
      }
      className={`inline-flex items-center gap-1 rounded border border-border px-1.5 py-0.5 text-[10px] ${t.bg} ${t.text}`}
    >
      <span>{t.emoji}</span>
      <span>{t.label}</span>
    </button>
  );
}

// Helper for callers that already have a report.
export function statusFor(report: FreshnessReport | undefined | null): FreshnessStatus {
  return report?.status ?? "unknown";
}

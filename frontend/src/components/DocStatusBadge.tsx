import type { DocStatus } from "~/api/approval";

const TONE: Record<DocStatus, { label: string; classes: string }> = {
  draft: { label: "Draft", classes: "border-border bg-bg text-muted" },
  in_review: {
    label: "In review",
    classes: "border-callout-warning/40 bg-callout-warning/15 text-callout-warning",
  },
  approved: {
    label: "Approved",
    classes: "border-callout-success/40 bg-callout-success/15 text-callout-success",
  },
  rejected: {
    label: "Changes requested",
    classes: "border-callout-error/40 bg-callout-error/15 text-callout-error",
  },
  archived: { label: "Archived", classes: "border-border bg-bg text-muted" },
};

interface BadgeProps {
  status: DocStatus;
}

// DocStatusBadge is a tiny pill rendered next to the freshness +
// share badges in the page header. Tone-mapped to the existing
// callout palette so it reads as part of the same family.
export function DocStatusBadge({ status }: BadgeProps) {
  const t = TONE[status] ?? TONE.draft;
  return (
    <span
      className={`inline-flex items-center rounded border px-1.5 py-0.5 text-[10px] ${t.classes}`}
    >
      {t.label}
    </span>
  );
}

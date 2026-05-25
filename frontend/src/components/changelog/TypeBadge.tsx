import type { EntryType } from "~/api/changelog";

// TypeBadge maps the entry type to a colored chip + emoji. Matches
// the bucket emoji used by the server-side content generator so
// the badge and the heading inside the generated body agree.
const TONE: Record<EntryType, { emoji: string; label: string; classes: string }> = {
  feature: {
    emoji: "✨",
    label: "Feature",
    classes: "border-accent/40 bg-accent/15 text-accent",
  },
  bugfix: {
    emoji: "🐛",
    label: "Bug fix",
    classes: "border-callout-error/40 bg-callout-error/15 text-callout-error",
  },
  breaking: {
    emoji: "⚠️",
    label: "Breaking",
    classes: "border-callout-error/40 bg-callout-error/15 text-callout-error",
  },
  improvement: {
    emoji: "🔧",
    label: "Improvement",
    classes: "border-callout-warning/40 bg-callout-warning/15 text-callout-warning",
  },
  deprecated: {
    emoji: "🗄️",
    label: "Deprecated",
    classes: "border-border bg-bg text-muted",
  },
  security: {
    emoji: "🔒",
    label: "Security",
    classes: "border-purple-500/40 bg-purple-500/15 text-purple-300",
  },
};

interface BadgeProps {
  type: EntryType;
}

export function TypeBadge({ type }: BadgeProps) {
  const t = TONE[type] ?? TONE.improvement;
  return (
    <span
      className={`inline-flex items-center gap-1 rounded border px-1.5 py-0.5 text-[10px] ${t.classes}`}
    >
      <span>{t.emoji}</span>
      <span>{t.label}</span>
    </span>
  );
}

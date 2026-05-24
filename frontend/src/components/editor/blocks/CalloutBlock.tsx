import { Info, AlertTriangle, AlertOctagon, CheckCircle2 } from "lucide-react";
import type { ReactNode } from "react";

export type CalloutTone = "info" | "warning" | "error" | "success";

const config: Record<CalloutTone, { Icon: typeof Info; toneClass: string }> = {
  info: { Icon: Info, toneClass: "border-callout-info bg-callout-info/10 text-callout-info" },
  warning: { Icon: AlertTriangle, toneClass: "border-callout-warning bg-callout-warning/10 text-callout-warning" },
  error: { Icon: AlertOctagon, toneClass: "border-callout-error bg-callout-error/10 text-callout-error" },
  success: { Icon: CheckCircle2, toneClass: "border-callout-success bg-callout-success/10 text-callout-success" },
};

// Standalone callout for read-only renders. The editor styles
// callouts via CSS on the `.callout` class; this component is a
// React render path for previews / search hits.
export function CalloutBlock({ tone, children }: { tone: CalloutTone; children: ReactNode }) {
  const c = config[tone] ?? config.info;
  const Icon = c.Icon;
  return (
    <div className={`flex items-start gap-2 rounded-md border-l-2 p-3 text-sm ${c.toneClass}`}>
      <Icon size={14} className="mt-0.5 shrink-0" />
      <div className="text-text">{children}</div>
    </div>
  );
}

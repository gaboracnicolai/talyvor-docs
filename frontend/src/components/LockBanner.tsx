import { Lock } from "lucide-react";
import type { LockState } from "~/api/pagelock";

interface BannerProps {
  state: LockState | undefined;
  lockedByMe: boolean;
  onUnlock: () => void;
}

// LockBanner is the full-width strip above the editor that explains
// why the page is read-only. Two variants:
//   - you locked it    → green-tinged + "Unlock" CTA
//   - someone else did → amber + "Request edit access" CTA (today
//     this is a no-op anchor — Phase 2 wires a comment dialog).
//
// Returns null when the page isn't locked so callers can render
// it unconditionally inside the layout.
export function LockBanner({ state, lockedByMe, onUnlock }: BannerProps) {
  if (!state?.locked) return null;
  if (lockedByMe) {
    return (
      <div className="flex items-center justify-between rounded border border-accent/40 bg-accent/10 px-2 py-1 text-[10px] text-accent">
        <span className="flex items-center gap-1">
          <Lock size={11} /> You locked this page.
        </span>
        <button onClick={onUnlock} className="underline">
          Unlock
        </button>
      </div>
    );
  }
  const by = state.locked_by_name || state.locked_by || "another user";
  const at = state.locked_at
    ? new Date(state.locked_at).toLocaleString()
    : "";
  return (
    <div className="flex items-center justify-between rounded border border-callout-warning/40 bg-callout-warning/10 px-2 py-1 text-[10px] text-callout-warning">
      <span className="flex items-center gap-1">
        <Lock size={11} />
        Locked by {by}
        {at ? ` · ${at}` : ""}
      </span>
      <a
        href="#"
        onClick={(e) => e.preventDefault()}
        className="underline opacity-70"
        title="Phase 2: opens a comment dialog. For now, message the locker directly."
      >
        Request unlock
      </a>
    </div>
  );
}

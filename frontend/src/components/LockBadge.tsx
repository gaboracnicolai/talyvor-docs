import { Lock, Unlock } from "lucide-react";
import type { LockState } from "~/api/pagelock";

interface BadgeProps {
  state: LockState | undefined;
  lockedByMe: boolean;
  onLock: () => void;
  onUnlock: () => void;
  busy?: boolean;
}

// LockBadge sits in the page header next to the share/freshness
// chips. Click to lock when unlocked; click to unlock when you hold
// the lock; show "locked by X" when someone else holds it.
export function LockBadge({ state, lockedByMe, onLock, onUnlock, busy }: BadgeProps) {
  const locked = !!state?.locked;
  if (!locked) {
    return (
      <button
        onClick={onLock}
        disabled={busy}
        title="Lock this page"
        className="inline-flex items-center gap-1 rounded border border-border bg-bg px-1.5 py-0.5 text-[10px] text-muted hover:border-accent hover:text-text"
      >
        <Unlock size={10} /> Lock
      </button>
    );
  }
  if (lockedByMe) {
    return (
      <button
        onClick={onUnlock}
        disabled={busy}
        title="You locked this — click to unlock"
        className="inline-flex items-center gap-1 rounded border border-accent/40 bg-accent/15 px-1.5 py-0.5 text-[10px] text-accent"
      >
        <Lock size={10} /> Locked by you
      </button>
    );
  }
  const by = state?.locked_by_name || state?.locked_by || "another user";
  const at = state?.locked_at ? new Date(state.locked_at).toLocaleString() : "";
  return (
    <span
      className="inline-flex items-center gap-1 rounded border border-callout-warning/40 bg-callout-warning/15 px-1.5 py-0.5 text-[10px] text-callout-warning"
      title={`Locked by ${by}${at ? " at " + at : ""}`}
    >
      <Lock size={10} /> Locked
    </span>
  );
}

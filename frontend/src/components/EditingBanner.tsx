import { Pencil } from "lucide-react";
import type { SessionFlags } from "~/hooks/useEditSession";

interface EditingBannerProps {
  flags: SessionFlags;
  holderName?: string | null; // resolved display name, if the host has a member directory
  onTakeover: () => void;
  takingOver?: boolean;
}

// EditingBanner is the full-width strip above the editor for the SINGLE-WRITER edit session
// (distinct from LockBanner's manual lock). It renders only when someone ELSE holds a live
// session — the current editor never needs to be told they're editing. It offers Takeover,
// which the backend grants only once the holder's session has actually expired.
export function EditingBanner({ flags, holderName, onTakeover, takingOver }: EditingBannerProps) {
  if (!flags.heldByOther) return null;
  const who = holderName || flags.holder || "another user";
  return (
    <div className="flex items-center justify-between rounded border border-callout-warning/40 bg-callout-warning/10 px-2 py-1 text-[10px] text-callout-warning">
      <span className="flex items-center gap-1">
        <Pencil size={11} />
        {who} is editing this page.
      </span>
      <button
        onClick={onTakeover}
        disabled={takingOver}
        className="underline disabled:opacity-50"
        title="Take over once the current editor has gone idle. A live session cannot be stolen."
      >
        {takingOver ? "Taking over…" : "Take over"}
      </button>
    </div>
  );
}

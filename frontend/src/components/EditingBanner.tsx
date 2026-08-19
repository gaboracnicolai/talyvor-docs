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
// session — the current editor never needs to be told they're editing.
//
// ⚠ AND THAT IS EXACTLY THE STATE IN WHICH THE TAKEOVER BUTTON BELOW CANNOT SUCCEED. The backend
// grants a takeover only once the holder's session has EXPIRED — but an expired session makes
// `heldByOther` false, so this component has already returned null and the button is off screen.
// The two predicates are the same predicate (`live && holder !== me`), measured on both sides:
// internal/editsession/takeoverparity_realpg_test.go shows takeover and acquire answering
// identically (423 live, 200 expired), and EditingBanner.affordance.test.tsx pins the render rule.
// The previous version of this comment described a state this component cannot be in.
//
// Left as-is deliberately: whether "Take over" should steal a LIVE session is a product call. The
// tooltip is honest about today's behaviour, and the two guards mean neither side can change
// without the other reddening.
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

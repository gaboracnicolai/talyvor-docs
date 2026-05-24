import type { PresenceInfo } from "~/hooks/useCollab";
import { RemoteCursor } from "./RemoteCursor";

interface PresenceBarProps {
  presence: PresenceInfo[];
  selfClientID: string;
  // jumpTo is called when the user clicks a remote avatar; the
  // editor host implements scrollTo / setSelection.
  jumpTo?: (clientID: string) => void;
}

// Compact horizontal avatar strip shown above the page editor.
// Caps at 5 visible chips + a "+N" overflow indicator so wide pages
// don't get a fifty-avatar parade.
export function PresenceBar({ presence, selfClientID, jumpTo }: PresenceBarProps) {
  const others = presence.filter((p) => p.client_id !== selfClientID);
  if (others.length === 0) return null;

  const visible = others.slice(0, 5);
  const overflow = others.length - visible.length;

  return (
    <div className="flex items-center gap-1 text-[10px] text-muted">
      <span className="mr-1">Live</span>
      <div className="flex -space-x-1">
        {visible.map((p) => (
          <button
            key={p.client_id}
            onClick={() => jumpTo?.(p.client_id)}
            className="ring-1 ring-bg"
            title={p.member_name || "Guest"}
          >
            <RemoteCursor name={p.member_name || "Guest"} color={p.color} />
          </button>
        ))}
      </div>
      {overflow > 0 ? (
        <span className="ml-1 rounded-full border border-border bg-bg px-1.5 py-0.5">
          +{overflow}
        </span>
      ) : null}
    </div>
  );
}

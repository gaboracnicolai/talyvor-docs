// RemoteCursor is the React-side render for a single user marker.
// The in-editor cursor is drawn by the ProseMirror Decorations
// system (see extensions/remote-cursors.ts); this component is the
// avatar-style chip surfaced in the presence bar + tooltips.

interface RemoteCursorProps {
  name: string;
  color: string;
  initials?: string;
  size?: number;
}

export function RemoteCursor({ name, color, initials, size = 24 }: RemoteCursorProps) {
  const txt = (initials ?? name.split(" ").map((p) => p[0]).slice(0, 2).join("")).toUpperCase();
  return (
    <span
      title={name}
      className="inline-flex shrink-0 items-center justify-center rounded-full font-mono text-[10px] font-semibold text-bg"
      style={{ background: color, width: size, height: size }}
    >
      {txt || "?"}
    </span>
  );
}

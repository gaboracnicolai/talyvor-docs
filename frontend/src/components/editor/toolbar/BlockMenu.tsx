import { useEffect, useMemo, useRef, useState } from "react";
import type { EditorView } from "prosemirror-view";
import { slashKey, filterSlashCommands, type SlashCommand } from "../extensions/slash-commands";

interface BlockMenuProps {
  view: EditorView | null;
}

// BlockMenu renders the "/" slash menu when the slash plugin
// reports an open state. Keyboard navigation: ↑/↓ to move, Enter to
// apply, Escape to dismiss.
export function BlockMenu({ view }: BlockMenuProps) {
  // Force re-render on every transaction so the position + query
  // track the editor state. The parent Editor component triggers a
  // React update via dispatchTransaction; we read directly from
  // the plugin state here.
  const slashState = view ? slashKey.getState(view.state) : null;
  const open = slashState?.open ?? null;
  const items = useMemo<SlashCommand[]>(
    () => (open ? filterSlashCommands(open.query) : []),
    [open?.query],
  );
  const [index, setIndex] = useState(0);
  const listRef = useRef<HTMLDivElement | null>(null);

  // Clamp the selection index whenever the filtered list shrinks.
  useEffect(() => {
    if (index >= items.length) setIndex(0);
  }, [items.length, index]);

  // Keyboard handlers: bound to window because the editor's own
  // keymap intercepts ArrowDown/Up when the menu is open.
  useEffect(() => {
    if (!view || !open) return;
    const handler = (e: KeyboardEvent) => {
      if (!view) return;
      if (e.key === "ArrowDown") {
        e.preventDefault();
        setIndex((i) => Math.min(items.length - 1, i + 1));
      } else if (e.key === "ArrowUp") {
        e.preventDefault();
        setIndex((i) => Math.max(0, i - 1));
      } else if (e.key === "Enter") {
        e.preventDefault();
        const cmd = items[index];
        if (cmd) cmd.apply(view, open.from, open.to);
      } else if (e.key === "Escape") {
        e.preventDefault();
        // Close by deleting the trigger character (the plugin
        // resets when the regex no longer matches).
        view.dispatch(view.state.tr.delete(open.from, open.from + 1));
        view.focus();
      }
    };
    window.addEventListener("keydown", handler, true);
    return () => window.removeEventListener("keydown", handler, true);
  }, [view, open, items, index]);

  if (!view || !open) return null;

  // Position the menu just below the trigger character.
  const start = view.coordsAtPos(open.from);
  const style = {
    top: start.bottom + 4,
    left: start.left,
  } as const;

  // Group commands for display so the user sees the categorisation
  // from the spec (Text / Lists / Content / AI / Track).
  const groups: { name: string; cmds: SlashCommand[] }[] = [];
  for (const cmd of items) {
    const g = groups.find((x) => x.name === cmd.group);
    if (g) g.cmds.push(cmd);
    else groups.push({ name: cmd.group, cmds: [cmd] });
  }

  let flat = 0;
  return (
    <div
      ref={listRef}
      style={style}
      className="fixed z-50 max-h-80 w-72 overflow-y-auto rounded-md border border-border bg-surface p-1 shadow-2xl"
    >
      {items.length === 0 ? (
        <div className="px-3 py-2 text-xs text-muted">No commands match "{open.query}"</div>
      ) : (
        groups.map((group) => (
          <div key={group.name} className="mb-1 last:mb-0">
            <div className="px-2 pt-1 text-[10px] font-semibold uppercase tracking-wider text-muted">
              {group.name}
            </div>
            {group.cmds.map((cmd) => {
              const myIndex = flat++;
              return (
                <button
                  key={cmd.id}
                  onClick={() => cmd.apply(view, open.from, open.to)}
                  className={
                    "flex w-full items-center justify-between rounded px-2 py-1.5 text-left text-xs " +
                    (myIndex === index
                      ? "bg-bg text-text"
                      : "text-muted hover:bg-bg/60 hover:text-text")
                  }
                >
                  <span>{cmd.label}</span>
                  {cmd.hint ? (
                    <span className="text-[10px] font-mono text-muted">{cmd.hint}</span>
                  ) : null}
                </button>
              );
            })}
          </div>
        ))
      )}
    </div>
  );
}

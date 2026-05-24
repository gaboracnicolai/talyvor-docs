import { Plugin, PluginKey, EditorState } from "prosemirror-state";
import { Decoration, DecorationSet } from "prosemirror-view";

// remoteCursorPlugin renders per-user carets via inline Decorations.
// The plugin doesn't subscribe to any external source — useEditor
// owns the state and re-dispatches a meta transaction with the
// fresh presence list every time it changes; the plugin reads that
// meta and rebuilds its DecorationSet.

export interface RemoteCursor {
  clientID: string;
  memberName: string;
  color: string;
  from: number;
  to: number;
}

export const remoteCursorKey = new PluginKey<RemoteCursor[]>("remote-cursors");

interface SetCursorsMeta {
  setCursors: RemoteCursor[];
}

// SetCursors is the meta payload Editor.tsx pushes through a
// transaction every time presence updates. Encapsulated as a helper
// so callers don't reach into PluginKey directly.
export function setRemoteCursors(state: EditorState, cursors: RemoteCursor[]) {
  return state.tr.setMeta(remoteCursorKey, { setCursors: cursors } as SetCursorsMeta);
}

export function remoteCursorPlugin(): Plugin<RemoteCursor[]> {
  return new Plugin<RemoteCursor[]>({
    key: remoteCursorKey,
    state: {
      init: () => [],
      apply(tr, value) {
        const meta = tr.getMeta(remoteCursorKey) as SetCursorsMeta | undefined;
        if (meta?.setCursors) return meta.setCursors;
        // No meta this tick — map existing cursor positions through
        // the transaction so they stay glued to the right text as
        // the local user types.
        return value.map((c) => ({
          ...c,
          from: tr.mapping.map(c.from),
          to: tr.mapping.map(c.to),
        }));
      },
    },
    props: {
      decorations(state) {
        const cursors = remoteCursorKey.getState(state) ?? [];
        const decos: Decoration[] = [];
        for (const c of cursors) {
          // Skip cursors with no valid position (e.g. before the
          // remote client has dispatched its first cursor frame).
          if (c.from == null) continue;
          const caret = document.createElement("span");
          caret.className = "remote-cursor";
          caret.setAttribute("data-name", c.memberName);
          caret.style.borderLeft = `2px solid ${c.color}`;
          caret.style.marginLeft = "-1px";
          const label = document.createElement("span");
          label.className = "remote-cursor-label";
          label.textContent = c.memberName || "guest";
          label.style.background = c.color;
          caret.appendChild(label);
          decos.push(Decoration.widget(c.from, caret, { side: 1, key: `cursor-${c.clientID}` }));
          if (c.to !== c.from) {
            decos.push(
              Decoration.inline(c.from, c.to, {
                style: `background:${hexAlpha(c.color, 0.2)}`,
              }),
            );
          }
        }
        return DecorationSet.create(state.doc, decos);
      },
    },
  });
}

// hexAlpha converts a "#rrggbb" into a rgba() string. We don't need
// rigorous parsing — the colour palette is fixed and well-formed.
function hexAlpha(hex: string, a: number): string {
  const r = parseInt(hex.slice(1, 3), 16);
  const g = parseInt(hex.slice(3, 5), 16);
  const b = parseInt(hex.slice(5, 7), 16);
  return `rgba(${r}, ${g}, ${b}, ${a})`;
}

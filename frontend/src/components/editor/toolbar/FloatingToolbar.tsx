import { useEffect, useState } from "react";
import { Bold, Italic, Strikethrough, Code, Link2, Highlighter, MessageCircle } from "lucide-react";
import { toggleMark } from "prosemirror-commands";
import type { EditorView } from "prosemirror-view";
import { schema } from "../schema";

interface FloatingToolbarProps {
  view: EditorView | null;
}

// Selection-aware floating toolbar. Positions itself above the
// current non-empty selection using the EditorView's coordsAtPos
// helper, which returns screen coordinates pegged to the document.
// The toolbar re-renders on every editor transaction so the position
// stays glued to the live selection.
export function FloatingToolbar({ view }: FloatingToolbarProps) {
  const [rect, setRect] = useState<{ top: number; left: number } | null>(null);

  useEffect(() => {
    if (!view) {
      setRect(null);
      return;
    }
    const update = () => {
      const sel = view.state.selection;
      if (sel.empty) {
        setRect(null);
        return;
      }
      const start = view.coordsAtPos(sel.from);
      const end = view.coordsAtPos(sel.to);
      const top = Math.min(start.top, end.top) - 38;
      const left = (start.left + end.left) / 2;
      setRect({ top, left });
    };
    update();
    window.addEventListener("scroll", update, true);
    window.addEventListener("resize", update);
    // ProseMirror fires DOM events on every transaction; we read
    // them lazily via a MutationObserver on the editor DOM.
    const obs = new MutationObserver(update);
    obs.observe(view.dom, { childList: true, subtree: true, characterData: true });
    return () => {
      window.removeEventListener("scroll", update, true);
      window.removeEventListener("resize", update);
      obs.disconnect();
    };
  }, [view]);

  if (!view || !rect) return null;

  const run = (cmd: (...args: any[]) => any) => {
    cmd(view.state, view.dispatch);
    view.focus();
  };

  return (
    <div
      style={{ top: rect.top, left: rect.left, transform: "translateX(-50%)" }}
      className="fixed z-50 inline-flex items-center gap-0.5 rounded-md border border-border bg-surface px-1 py-1 text-text shadow-lg"
    >
      <ToolbarButton title="Bold (⌘B)" onClick={() => run(toggleMark(schema.marks.strong))}>
        <Bold size={12} />
      </ToolbarButton>
      <ToolbarButton title="Italic (⌘I)" onClick={() => run(toggleMark(schema.marks.em))}>
        <Italic size={12} />
      </ToolbarButton>
      <ToolbarButton title="Strike (⌘⇧X)" onClick={() => run(toggleMark(schema.marks.strike))}>
        <Strikethrough size={12} />
      </ToolbarButton>
      <ToolbarButton title="Code (⌘E)" onClick={() => run(toggleMark(schema.marks.code))}>
        <Code size={12} />
      </ToolbarButton>
      <ToolbarButton
        title="Link"
        onClick={() => {
          const href = window.prompt("URL:");
          if (!href) return;
          toggleMark(schema.marks.link, { href })(view.state, view.dispatch);
          view.focus();
        }}
      >
        <Link2 size={12} />
      </ToolbarButton>
      <ToolbarButton title="Highlight (⌘⇧H)" onClick={() => run(toggleMark(schema.marks.highlight))}>
        <Highlighter size={12} />
      </ToolbarButton>
      <ToolbarButton
        title="Inline comment"
        onClick={() => {
          // Phase 2: emit a custom event the page chrome can listen
          // to and open the comment composer.
          window.dispatchEvent(
            new CustomEvent("docs:inline-comment", {
              detail: { from: view.state.selection.from, to: view.state.selection.to },
            }),
          );
        }}
      >
        <MessageCircle size={12} />
      </ToolbarButton>
    </div>
  );
}

function ToolbarButton({
  children,
  onClick,
  title,
}: {
  children: React.ReactNode;
  onClick: () => void;
  title: string;
}) {
  return (
    <button
      title={title}
      onClick={onClick}
      className="flex h-7 w-7 items-center justify-center rounded text-muted hover:bg-bg hover:text-text"
    >
      {children}
    </button>
  );
}

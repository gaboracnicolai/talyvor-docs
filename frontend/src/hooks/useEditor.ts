import { useCallback, useEffect, useRef, useState } from "react";
import { EditorState, Transaction } from "prosemirror-state";
import { EditorView } from "prosemirror-view";
import { Node as PMNode } from "prosemirror-model";
import { schema } from "~/components/editor/schema";
import { buildPlugins } from "~/components/editor/extensions";
import { slashPlugin } from "~/components/editor/extensions/slash-commands";
import { codeHighlightPlugin } from "~/components/editor/extensions/code-highlight";

export interface UseEditorOptions {
  initialContent: string;
  readOnly?: boolean;
  onChange?: (jsonString: string, plainText: string) => void;
}

// useEditor owns the ProseMirror EditorView. We deliberately do NOT
// re-create the view on every React render — ProseMirror is a
// stateful DOM-mounted engine; remounting it would lose selection,
// undo history, and IME state. Instead the view is created once and
// kept in a ref; the React component just hosts the mount element.
export function useEditor({ initialContent, readOnly, onChange }: UseEditorOptions) {
  const mountRef = useRef<HTMLDivElement | null>(null);
  const viewRef = useRef<EditorView | null>(null);
  const [, force] = useState(0);

  // initialDocOnce: when the page reloads with a different doc we
  // rebuild the view; otherwise we leave the existing editor alone.
  // The hash is sufficient because content updates always flow
  // through the editor itself (auto-save handles persistence).
  const initialHash = useRef<string>("");

  useEffect(() => {
    if (!mountRef.current) return;
    if (viewRef.current && initialHash.current === initialContent) return;
    if (viewRef.current) {
      viewRef.current.destroy();
      viewRef.current = null;
    }
    initialHash.current = initialContent;

    const doc = parseDoc(initialContent);
    const state = EditorState.create({
      doc,
      schema,
      plugins: buildPlugins([slashPlugin(), codeHighlightPlugin()]),
    });
    const view = new EditorView(mountRef.current, {
      state,
      editable: () => !readOnly,
      dispatchTransaction(tr: Transaction) {
        const newState = view.state.apply(tr);
        view.updateState(newState);
        if (tr.docChanged && onChange) {
          const json = JSON.stringify(newState.doc.toJSON());
          onChange(json, newState.doc.textContent);
        }
        // Force a React re-render so the floating toolbar +
        // slash-menu portals can observe the new selection /
        // plugin state.
        force((n) => n + 1);
      },
    });
    viewRef.current = view;
    force((n) => n + 1);
    return () => {
      view.destroy();
      viewRef.current = null;
    };
    // initialContent is intentionally part of the deps: a different
    // page mounting through this hook reinitialises the view.
  }, [initialContent, readOnly, onChange]);

  // execute lets callers (slash menu, toolbar) run a command against
  // the current view without having to thread the ref through props.
  const execute = useCallback((fn: (view: EditorView) => void) => {
    if (viewRef.current) fn(viewRef.current);
  }, []);

  return { mountRef, view: viewRef.current, execute };
}

// parseDoc handles both empty and malformed content. ProseMirror
// throws on bad JSON, so we fall back to an empty doc rather than
// blow up the page render.
function parseDoc(json: string): PMNode | undefined {
  if (!json || json === "{}") return undefined;
  try {
    const parsed = JSON.parse(json);
    return schema.nodeFromJSON(parsed);
  } catch {
    return undefined;
  }
}

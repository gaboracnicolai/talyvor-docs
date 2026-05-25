import { useCallback, useEffect, useRef, useState } from "react";
import { EditorState, Transaction } from "prosemirror-state";
import { EditorView, NodeView } from "prosemirror-view";
import { Node as PMNode } from "prosemirror-model";
import { schema } from "~/components/editor/schema";
import { buildPlugins } from "~/components/editor/extensions";
import { slashPlugin } from "~/components/editor/extensions/slash-commands";
import { codeHighlightPlugin } from "~/components/editor/extensions/code-highlight";
import {
  remoteCursorPlugin,
  setRemoteCursors,
  type RemoteCursor,
} from "~/components/editor/extensions/remote-cursors";

export interface UseEditorOptions {
  initialContent: string;
  readOnly?: boolean;
  onChange?: (jsonString: string, plainText: string) => void;
  // nodeViews lets callers register custom React renderers for
  // specific PM node types. The database_block uses this.
  nodeViews?: Record<string, (node: PMNode, view: EditorView, getPos: () => number | undefined) => NodeView>;
}

// useEditor owns the ProseMirror EditorView. We deliberately do NOT
// re-create the view on every React render — ProseMirror is a
// stateful DOM-mounted engine; remounting it would lose selection,
// undo history, and IME state. Instead the view is created once and
// kept in a ref; the React component just hosts the mount element.
export function useEditor({ initialContent, readOnly, onChange, nodeViews }: UseEditorOptions) {
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
      plugins: buildPlugins([
        slashPlugin(),
        codeHighlightPlugin(),
        remoteCursorPlugin(),
      ]),
    });
    const view = new EditorView(mountRef.current, {
      state,
      editable: () => !readOnly,
      nodeViews: nodeViews ?? {},
      dispatchTransaction(tr: Transaction) {
        const newState = view.state.apply(tr);
        view.updateState(newState);
        // Skip the onChange callback for transactions tagged as
        // remote — they came in from the WebSocket, so re-emitting
        // would create an echo loop.
        const isRemote = tr.getMeta("remote") === true;
        if (tr.docChanged && onChange && !isRemote) {
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
  }, [initialContent, readOnly, onChange, nodeViews]);

  // execute lets callers (slash menu, toolbar) run a command against
  // the current view without having to thread the ref through props.
  const execute = useCallback((fn: (view: EditorView) => void) => {
    if (viewRef.current) fn(viewRef.current);
  }, []);

  // applyRemoteSnapshot replaces the entire document with a fresh
  // ProseMirror JSON payload. Phase 3 uses this as the "good
  // enough" merge path: when a remote change arrives we accept the
  // client's complete post-change snapshot and let ProseMirror
  // adapt the local selection through tr.mapping.
  const applyRemoteSnapshot = useCallback((json: string) => {
    const view = viewRef.current;
    if (!view) return;
    const doc = parseDoc(json);
    if (!doc) return;
    const tr = view.state.tr.replaceWith(0, view.state.doc.content.size, doc.content);
    // tag the transaction so the local onChange path doesn't echo
    // it back to the network — we just received it from there.
    tr.setMeta("remote", true);
    view.dispatch(tr);
  }, []);

  // updateRemoteCursors pushes the latest presence cursors into the
  // remote-cursor plugin's state via a meta-only transaction.
  const updateRemoteCursors = useCallback((cursors: RemoteCursor[]) => {
    const view = viewRef.current;
    if (!view) return;
    view.dispatch(setRemoteCursors(view.state, cursors));
  }, []);

  return {
    mountRef,
    view: viewRef.current,
    execute,
    applyRemoteSnapshot,
    updateRemoteCursors,
  };
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

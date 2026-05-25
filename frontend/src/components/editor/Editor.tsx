import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { createRoot, type Root } from "react-dom/client";
import { QueryClientProvider, useQueryClient } from "@tanstack/react-query";
import type { NodeView } from "prosemirror-view";
import { useEditor } from "~/hooks/useEditor";
import { useCollab, type Change, type PresenceInfo } from "~/hooks/useCollab";
import { FloatingToolbar } from "./toolbar/FloatingToolbar";
import { BlockMenu } from "./toolbar/BlockMenu";
import { IssueSearchDialog } from "./IssueSearchDialog";
import { schema } from "./schema";
import type { RemoteCursor } from "./extensions/remote-cursors";
import type { TrackIssue } from "~/api/track";
import { callAI, type AIAction } from "./extensions/ai-assist";
import { databaseApi } from "~/api/database";
import { DatabaseBlock } from "~/components/database/DatabaseBlock";
import { createTOCNodeView } from "./blocks/TOCBlock";

interface EditorProps {
  pageId: string;
  workspaceId: string;
  initialContent: string;
  readOnly?: boolean;
  // onSave fires after the 2-second debounce. Receives the
  // ProseMirror JSON (string-encoded) and the plain-text projection
  // for the server's content_text column.
  onSave?: (content: string, contentText: string) => void;
  onChange?: (content: string) => void;
  // onPresence surfaces the live presence list to a parent that
  // wants to render PresenceBar above the editor without booting
  // its own WebSocket connection.
  onPresence?: (presence: PresenceInfo[], selfClientID: string) => void;
}

// SaveState models the persistence indicator. We render "Saving…"
// during a flight, "Saved" briefly after, and nothing in the idle
// state to keep the chrome quiet.
type SaveState = "idle" | "dirty" | "saving" | "saved";

// clientID is stable for the lifetime of the browser tab. We persist
// it in sessionStorage so reconnects keep the same identity (and
// matching cursor colour) across refreshes within the session.
function getClientID(): string {
  let id = sessionStorage.getItem("docs_client_id");
  if (!id) {
    id = "c-" + Math.random().toString(36).slice(2, 12);
    sessionStorage.setItem("docs_client_id", id);
  }
  return id;
}

export function Editor({
  pageId,
  workspaceId,
  initialContent,
  readOnly,
  onSave,
  onChange,
  onPresence,
}: EditorProps) {
  const latest = useRef<{ json: string; text: string } | null>(null);
  const [saveState, setSaveState] = useState<SaveState>("idle");
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const sendChangeRef = useRef<((c: Omit<Change, "version" | "client_id" | "page_id">) => void) | null>(null);

  // Local change → debounce → onSave, AND immediately publish to
  // collab so peers see the edit before the auto-save round-trip.
  // We send the full snapshot rather than ops — Phase 3's server
  // treats the snapshot as the authoritative replicated state; the
  // ops array is a forward-compat hook for finer-grained sync.
  const handleChange = useCallback(
    (json: string, text: string) => {
      latest.current = { json, text };
      setSaveState("dirty");
      onChange?.(json);
      sendChangeRef.current?.({ ops: [], snapshot: json });
      if (timer.current) clearTimeout(timer.current);
      timer.current = setTimeout(async () => {
        if (!latest.current || !onSave) return;
        setSaveState("saving");
        try {
          await Promise.resolve(onSave(latest.current.json, latest.current.text));
          setSaveState("saved");
          setTimeout(() => setSaveState("idle"), 1500);
        } catch {
          setSaveState("dirty");
        }
      }, 2000);
    },
    [onChange, onSave],
  );

  // Flush pending saves on unmount so navigating away doesn't lose
  // the last few keystrokes.
  useEffect(() => {
    return () => {
      if (timer.current) {
        clearTimeout(timer.current);
        if (latest.current && onSave) {
          void onSave(latest.current.json, latest.current.text);
        }
      }
    };
  }, [onSave]);

  // database_block node view — renders the React DatabaseBlock
  // inside the ProseMirror tree. We grab the TanStack QueryClient at
  // call time so the embedded React tree shares the host's cache.
  const queryClient = useQueryClient();
  const nodeViews = useMemo(
    () => ({
      // toc_block paints a live TOC. Vanilla DOM (no React) — the
      // Editor's dispatchTransaction fires a window event the node
      // view listens for so it stays in sync with surrounding edits.
      toc_block: (node: import("prosemirror-model").Node, viewArg: import("prosemirror-view").EditorView) =>
        createTOCNodeView(node, viewArg),
      database_block: (node: import("prosemirror-model").Node): NodeView => {
        const dom = document.createElement("div");
        dom.className = "database-block-host";
        const root: Root = createRoot(dom);
        const render = (databaseID: string) => {
          root.render(
            <QueryClientProvider client={queryClient}>
              <DatabaseBlock databaseID={databaseID} />
            </QueryClientProvider>,
          );
        };
        render(node.attrs.database_id);
        return {
          dom,
          update(updated) {
            if (updated.type.name !== "database_block") return false;
            render(updated.attrs.database_id);
            return true;
          },
          destroy() {
            // Defer to next tick so React unmount doesn't race with
            // ProseMirror's own removal of the DOM node.
            setTimeout(() => root.unmount(), 0);
          },
          stopEvent: () => true,
          ignoreMutation: () => true,
        };
      },
    }),
    [queryClient],
  );

  const { mountRef, view, applyRemoteSnapshot, updateRemoteCursors } = useEditor({
    initialContent,
    readOnly,
    onChange: handleChange,
    nodeViews,
  });

  // useCollab needs onRemoteChange in its dep list, but Editor.tsx
  // depends on the editor view to apply it — the ref dance keeps
  // both hooks from oscillating each other's identity.
  const applyRef = useRef(applyRemoteSnapshot);
  applyRef.current = applyRemoteSnapshot;
  const onRemoteChange = useCallback((change: Change) => {
    if (change.snapshot) applyRef.current(change.snapshot);
  }, []);

  const clientID = useMemo(() => getClientID(), []);
  const memberID = localStorage.getItem("docs_member_id") || clientID;
  const memberName = localStorage.getItem("docs_member_name") || "Guest";

  const collab = useCollab({
    pageID: pageId,
    clientID,
    memberID,
    memberName,
    onRemoteChange,
  });
  sendChangeRef.current = collab.sendChange;

  // Surface presence + own client ID to the host (PageView) so it
  // can render PresenceBar above the editor without spinning up a
  // second WebSocket. Effect rather than a render-prop so the
  // collab data flow stays unidirectional.
  useEffect(() => {
    onPresence?.(collab.presence, clientID);
  }, [collab.presence, clientID, onPresence]);

  // Paint remote cursors via the editor plugin whenever the
  // presence list changes.
  useEffect(() => {
    const cursors: RemoteCursor[] = collab.presence
      .filter((p) => p.client_id !== clientID && p.cursor)
      .map((p) => ({
        clientID: p.client_id,
        memberName: p.member_name,
        color: p.color,
        from: p.cursor!.from,
        to: p.cursor!.to,
      }));
    updateRemoteCursors(cursors);
  }, [collab.presence, clientID, updateRemoteCursors]);

  // Selection broadcast — polled every 250ms so a fast-typing user
  // doesn't flood the network with cursor frames. The pool also
  // smooths out the noisy run of transactions during an IME compose.
  const lastCursorSent = useRef<{ from: number; to: number } | null>(null);
  useEffect(() => {
    if (!view) return;
    const tick = () => {
      const sel = view.state.selection;
      const next = { from: sel.from, to: sel.to };
      const prev = lastCursorSent.current;
      if (!prev || prev.from !== next.from || prev.to !== next.to) {
        lastCursorSent.current = next;
        collab.sendCursor(next);
      }
    };
    const id = setInterval(tick, 250);
    return () => clearInterval(id);
  }, [view, collab]);

  // Database creation. The slash command emits "docs:create-database";
  // we POST to /pages/:pageID/databases to mint a Database row,
  // then insert a database_block PM node carrying the returned ID.
  useEffect(() => {
    if (!view) return;
    const onCreate = async () => {
      try {
        const db = await databaseApi.create(pageId, { name: "Untitled Database" });
        const node = schema.nodes.database_block.create({ database_id: db.id });
        view.dispatch(view.state.tr.replaceSelectionWith(node));
        view.focus();
      } catch {
        // Silently ignore — the slash trigger range has already been
        // deleted, so the user just sees nothing happen. A toast
        // would be nice; Phase 2 polish.
      }
    };
    window.addEventListener("docs:create-database", onCreate);
    return () => window.removeEventListener("docs:create-database", onCreate);
  }, [view, pageId]);

  // Issue-embed picker. The slash command emits a
  // "docs:embed-issue" event with the trigger range; we open the
  // dialog and insert an issue_embed node when the user selects.
  const [embedOpen, setEmbedOpen] = useState(false);
  useEffect(() => {
    const onOpen = () => setEmbedOpen(true);
    window.addEventListener("docs:embed-issue", onOpen as EventListener);
    return () => window.removeEventListener("docs:embed-issue", onOpen as EventListener);
  }, []);

  const insertEmbed = useCallback(
    (issue: TrackIssue) => {
      if (!view) return;
      const node = schema.nodes.issue_embed.create({
        issue_id: issue.id,
        identifier: issue.identifier,
        title: issue.title,
      });
      view.dispatch(view.state.tr.replaceSelectionWith(node));
      setEmbedOpen(false);
      view.focus();
    },
    [view],
  );

  // AI loading state + toast surface. We show a small "✨ Generating…"
  // chip near the selection while the call is in flight; on error we
  // surface a friendly toast (no raw API messages).
  const [aiLoading, setAILoading] = useState(false);
  const [aiToast, setAIToast] = useState<string | null>(null);
  useEffect(() => {
    if (!aiToast) return;
    const id = setTimeout(() => setAIToast(null), 4000);
    return () => clearTimeout(id);
  }, [aiToast]);

  // Slash-menu AI commands all dispatch "docs:ai-command" with an
  // action ID. We capture the current selection text, ask the user
  // for any missing inputs (write prompt, target language), call the
  // engine, then replace the selection with the result.
  useEffect(() => {
    if (!view) return;
    const onCmd = async (ev: Event) => {
      const detail = (ev as CustomEvent<{ id: AIAction }>).detail;
      if (!detail) return;
      const action = detail.id;
      const state = view.state;
      const { from, to } = state.selection;
      const selectionText = from === to ? "" : state.doc.textBetween(from, to, "\n");
      // For ai-write the slash trigger doesn't carry a selection, so
      // we prompt for the user's instruction. For ai-translate we
      // prompt for the target language. UX polish for both lands in
      // Phase 6 as inline dialogs; window.prompt is the Phase-5
      // shipping shim.
      let prompt: string | undefined;
      let language: string | undefined;
      if (action === "ai-write") {
        prompt = window.prompt("What should I write?") ?? "";
        if (!prompt.trim()) return;
      }
      if (action === "ai-translate") {
        language = window.prompt("Translate to (language)?", "Spanish") ?? "";
        if (!language.trim()) return;
      }
      const context = state.doc.textBetween(
        Math.max(0, from - 500),
        Math.min(state.doc.content.size, to + 500),
        "\n",
      );
      setAILoading(true);
      try {
        const { text } = await callAI({
          action,
          text: selectionText,
          context,
          workspaceId,
          pageId,
          prompt,
          language,
        });
        const tr = view.state.tr;
        // Replace the current selection (if any) with the result;
        // for ai-write (empty selection) we insert at the cursor.
        if (from === to) {
          tr.insertText(text, from);
        } else {
          tr.replaceWith(from, to, view.state.schema.text(text));
        }
        view.dispatch(tr);
        view.focus();
      } catch {
        setAIToast("AI unavailable. Is Lens configured?");
      } finally {
        setAILoading(false);
      }
    };
    window.addEventListener("docs:ai-command", onCmd as EventListener);
    return () => window.removeEventListener("docs:ai-command", onCmd as EventListener);
  }, [view, workspaceId, pageId]);

  return (
    <div className="relative" data-page-id={pageId}>
      <SaveBadge state={saveState} connected={collab.connected} />
      <div
        ref={mountRef}
        className="prose-editor min-h-[200px] text-text focus:outline-none"
      />
      <FloatingToolbar view={view} />
      <BlockMenu view={view} />
      <IssueSearchDialog
        open={embedOpen}
        onPick={insertEmbed}
        onClose={() => setEmbedOpen(false)}
      />
      {aiLoading ? (
        <div className="pointer-events-none absolute right-0 top-4 rounded bg-surface px-2 py-0.5 text-[10px] text-muted shadow-sm">
          ✨ Generating…
        </div>
      ) : null}
      {aiToast ? (
        <div className="pointer-events-none fixed bottom-6 left-1/2 -translate-x-1/2 rounded bg-callout-warning px-3 py-1.5 text-xs text-text shadow-lg">
          {aiToast}
        </div>
      ) : null}
    </div>
  );
}

function SaveBadge({ state, connected }: { state: SaveState; connected: boolean }) {
  if (state === "idle" && connected) return null;
  const label = !connected
    ? "Offline — changes queued"
    : state === "saving"
      ? "Saving…"
      : state === "saved"
        ? "Saved"
        : state === "dirty"
          ? "Unsaved changes"
          : "";
  const tone = !connected
    ? "text-callout-warning"
    : state === "saved"
      ? "text-callout-success"
      : "text-muted";
  return (
    <div className={`absolute right-0 top-0 text-[10px] ${tone}`}>{label}</div>
  );
}

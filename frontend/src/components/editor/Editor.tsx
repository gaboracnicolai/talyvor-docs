import { useCallback, useEffect, useRef, useState } from "react";
import { useEditor } from "~/hooks/useEditor";
import { FloatingToolbar } from "./toolbar/FloatingToolbar";
import { BlockMenu } from "./toolbar/BlockMenu";

interface EditorProps {
  pageId: string;
  initialContent: string;
  readOnly?: boolean;
  // onSave fires after the 2-second debounce. Receives the
  // ProseMirror JSON (string-encoded) and the plain-text projection
  // for the server's content_text column.
  onSave?: (content: string, contentText: string) => void;
  onChange?: (content: string) => void;
}

// SaveState models the persistence indicator. We render "Saving…"
// during a flight, "Saved" briefly after, and nothing in the idle
// state to keep the chrome quiet.
type SaveState = "idle" | "dirty" | "saving" | "saved";

export function Editor({
  pageId,
  initialContent,
  readOnly,
  onSave,
  onChange,
}: EditorProps) {
  const latest = useRef<{ json: string; text: string } | null>(null);
  const [saveState, setSaveState] = useState<SaveState>("idle");
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);

  const handleChange = useCallback(
    (json: string, text: string) => {
      latest.current = { json, text };
      setSaveState("dirty");
      onChange?.(json);
      if (timer.current) clearTimeout(timer.current);
      timer.current = setTimeout(async () => {
        if (!latest.current || !onSave) return;
        setSaveState("saving");
        try {
          await Promise.resolve(onSave(latest.current.json, latest.current.text));
          setSaveState("saved");
          // Drop back to idle a second later so the badge isn't
          // permanently shouting "Saved".
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

  const { mountRef, view } = useEditor({
    initialContent,
    readOnly,
    onChange: handleChange,
  });

  return (
    <div className="relative" data-page-id={pageId}>
      <SaveBadge state={saveState} />
      <div
        ref={mountRef}
        className="prose-editor min-h-[200px] text-text focus:outline-none"
      />
      <FloatingToolbar view={view} />
      <BlockMenu view={view} />
    </div>
  );
}

function SaveBadge({ state }: { state: SaveState }) {
  if (state === "idle") return null;
  const label =
    state === "saving"
      ? "Saving…"
      : state === "saved"
        ? "Saved"
        : "Unsaved changes";
  const tone =
    state === "saved"
      ? "text-status-done text-callout-success"
      : state === "saving"
        ? "text-muted"
        : "text-muted";
  return (
    <div className={`absolute right-0 top-0 text-[10px] ${tone}`}>{label}</div>
  );
}

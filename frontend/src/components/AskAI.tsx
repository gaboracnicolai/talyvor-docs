import { useEffect, useRef, useState } from "react";
import { Sparkles, X, Loader2 } from "lucide-react";
import { aiApi, type AIAskResponse } from "~/api/ai";
import { APIError } from "~/api/client";

interface AskAIProps {
  workspaceId: string;
}

// AskAI is the floating Q&A surface — bottom-right, Cmd+Shift+A to
// open. The panel calls /v1/workspaces/:wsID/ai/ask which gathers
// top-3 full-text matches as context and asks Sonnet for an answer
// grounded in those pages.
export function AskAI({ workspaceId }: AskAIProps) {
  const [open, setOpen] = useState(false);
  const [question, setQuestion] = useState("");
  const [loading, setLoading] = useState(false);
  const [answer, setAnswer] = useState<AIAskResponse | null>(null);
  const [error, setError] = useState<string | null>(null);
  const inputRef = useRef<HTMLInputElement | null>(null);

  // Cmd+Shift+A toggle. Bound to window so the shortcut works
  // regardless of focus state.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const mod = e.metaKey || e.ctrlKey;
      if (mod && e.shiftKey && (e.key === "a" || e.key === "A")) {
        e.preventDefault();
        setOpen((v) => !v);
      }
      if (open && e.key === "Escape") setOpen(false);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [open]);

  useEffect(() => {
    if (open) inputRef.current?.focus();
  }, [open]);

  const ask = async () => {
    if (!question.trim() || loading) return;
    setLoading(true);
    setError(null);
    setAnswer(null);
    try {
      const res = await aiApi.ask(workspaceId, { question });
      setAnswer(res);
    } catch (e) {
      // Lens unavailable surfaces as a 503 from the server. We don't
      // expose the raw status to the user — the response body
      // already says "AI unavailable. Check Lens configuration." but
      // we re-message in case the call never made it that far
      // (offline, server down).
      if (e instanceof APIError && e.code === "AI_UNAVAILABLE") {
        setError("AI not configured. Set DOCS_LENS_URL + DOCS_LENS_API_KEY.");
      } else {
        setError("Something went wrong asking the AI.");
      }
    } finally {
      setLoading(false);
    }
  };

  const clear = () => {
    setAnswer(null);
    setError(null);
    setQuestion("");
    inputRef.current?.focus();
  };

  if (!open) {
    return (
      <button
        onClick={() => setOpen(true)}
        title="Ask AI (Cmd+Shift+A)"
        className="fixed bottom-4 right-4 flex h-10 w-10 items-center justify-center rounded-full bg-accent text-bg shadow-lg hover:opacity-90"
      >
        <Sparkles size={16} />
      </button>
    );
  }

  return (
    <div className="fixed bottom-4 right-4 z-50 flex w-96 flex-col rounded-md border border-border bg-surface shadow-xl">
      <header className="flex items-center justify-between border-b border-border px-3 py-2">
        <div className="flex items-center gap-1 text-xs font-semibold">
          <Sparkles size={12} className="text-accent" />
          Ask AI
        </div>
        <button onClick={() => setOpen(false)} className="text-muted hover:text-text">
          <X size={14} />
        </button>
      </header>

      <div className="flex items-center gap-1 px-3 py-2">
        <input
          ref={inputRef}
          value={question}
          onChange={(e) => setQuestion(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && !e.shiftKey) {
              e.preventDefault();
              void ask();
            }
          }}
          placeholder="Ask anything about your docs…"
          className="flex-1 rounded border border-border bg-bg px-2 py-1 text-xs focus:border-accent focus:outline-none"
        />
        <button
          onClick={() => void ask()}
          disabled={loading || !question.trim()}
          className="rounded bg-accent px-2 py-1 text-xs text-bg hover:opacity-90 disabled:opacity-40"
        >
          {loading ? <Loader2 size={12} className="animate-spin" /> : "Ask"}
        </button>
      </div>

      <div className="max-h-72 overflow-y-auto px-3 pb-3 text-xs">
        {error ? <div className="text-callout-error">{error}</div> : null}
        {loading && !answer ? (
          <div className="text-muted">Thinking…</div>
        ) : null}
        {answer ? (
          <div className="space-y-2">
            <div className="whitespace-pre-wrap text-text">{answer.answer}</div>
            {answer.sources.length > 0 ? (
              <div className="border-t border-border pt-2">
                <div className="mb-1 text-[10px] uppercase tracking-wider text-muted">
                  Sources
                </div>
                <ul className="space-y-0.5">
                  {answer.sources.map((s, i) => (
                    <li key={i}>
                      {s.url ? (
                        <a href={s.url} className="text-accent hover:underline">
                          {s.title}
                        </a>
                      ) : (
                        <span>{s.title}</span>
                      )}
                    </li>
                  ))}
                </ul>
              </div>
            ) : null}
            <button onClick={clear} className="text-[10px] text-muted underline">
              Clear
            </button>
          </div>
        ) : null}
      </div>
    </div>
  );
}

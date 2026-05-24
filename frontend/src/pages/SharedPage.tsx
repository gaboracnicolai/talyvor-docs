import { useEffect, useState } from "react";
import { ExternalLink, Lock } from "lucide-react";
import { APIError } from "~/api/client";
import { sharingApi, type PublicSharePayload } from "~/api/sharing";

interface SharedPageProps {
  token: string;
}

// SharedPage is the public reader for /s/:token. Stripped chrome —
// no editor, no sidebar — and a "Powered by Talyvor Docs" footer
// stamped on every render so the public surface is brand-visible.
// Password-protected links render a prompt first; the API surfaces
// the requires_pass flag via APIError.code.
export function SharedPage({ token }: SharedPageProps) {
  const [data, setData] = useState<PublicSharePayload | null>(null);
  const [needsPassword, setNeedsPassword] = useState(false);
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  const load = async (pw?: string) => {
    setLoading(true);
    setError(null);
    try {
      const res = await sharingApi.publicView(token, pw);
      setData(res);
      setNeedsPassword(false);
    } catch (e) {
      if (e instanceof APIError) {
        if (e.status === 401) {
          setNeedsPassword(true);
        } else if (e.status === 410) {
          setError("This link has expired.");
        } else if (e.status === 404) {
          setError("This link is no longer valid.");
        } else {
          setError("Something went wrong loading this page.");
        }
      } else {
        setError("Something went wrong loading this page.");
      }
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    void load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [token]);

  return (
    <div className="flex min-h-screen flex-col bg-bg text-text">
      <header className="flex items-center justify-between border-b border-border bg-surface px-4 py-2 text-xs">
        <div className="flex items-center gap-2">
          <span className="inline-flex h-5 w-5 items-center justify-center rounded bg-accent text-bg">
            <span className="font-mono text-[10px] font-bold">T</span>
          </span>
          <span className="font-semibold">Talyvor Docs</span>
        </div>
        <a
          href="https://talyvor.com"
          className="flex items-center gap-1 text-muted hover:text-text"
        >
          View in Talyvor Docs <ExternalLink size={10} />
        </a>
      </header>

      <main className="mx-auto w-full max-w-3xl flex-1 px-8 py-8">
        {loading ? (
          <p className="text-xs text-muted">Loading…</p>
        ) : needsPassword ? (
          <PasswordPrompt
            onSubmit={(pw) => {
              setPassword(pw);
              void load(pw);
            }}
          />
        ) : error ? (
          <div className="rounded border border-callout-error/40 bg-callout-error/10 px-3 py-2 text-xs text-callout-error">
            {error}
          </div>
        ) : data ? (
          <article>
            <h1 className="text-2xl font-semibold">
              {data.page.icon ? `${data.page.icon} ` : ""}
              {data.page.title}
            </h1>
            <div className="prose-editor mt-4 whitespace-pre-wrap text-sm">
              {data.page.content_text}
            </div>
          </article>
        ) : null}
      </main>

      <footer className="border-t border-border bg-surface px-4 py-2 text-center text-[10px] text-muted">
        Powered by Talyvor Docs
      </footer>
      {password ? <span className="hidden" /> : null}
    </div>
  );
}

function PasswordPrompt({ onSubmit }: { onSubmit: (pw: string) => void }) {
  const [pw, setPw] = useState("");
  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        if (!pw.trim()) return;
        onSubmit(pw);
      }}
      className="mx-auto flex max-w-sm flex-col gap-2 rounded border border-border bg-surface p-4"
    >
      <div className="flex items-center gap-1 text-xs font-semibold">
        <Lock size={12} /> Password required
      </div>
      <input
        type="password"
        value={pw}
        onChange={(e) => setPw(e.target.value)}
        autoFocus
        placeholder="Enter password"
        className="rounded border border-border bg-bg px-2 py-1 text-xs focus:border-accent focus:outline-none"
      />
      <button
        type="submit"
        className="rounded bg-accent px-2 py-1 text-xs text-bg hover:opacity-90"
      >
        View page
      </button>
    </form>
  );
}

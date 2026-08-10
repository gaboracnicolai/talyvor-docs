import { useEffect, useMemo, useRef, useState } from "react";
import { Search, Sparkles, Brain, Clock } from "lucide-react";
import type { SearchResult } from "~/api/search";
import {
  getRecentPages,
  pushRecentPage,
  useSearch,
  type RecentPage,
} from "~/hooks/useSearch";

interface SearchModalProps {
  workspaceId: string;
  open: boolean;
  onClose: () => void;
  // onOpenPage receives the chosen result so the host App can route
  // to it. The modal stays decoupled from the router.
  onOpenPage: (r: { pageID: string; spaceID?: string; title: string; spaceName: string; url: string }) => void;
}

// sanitiseHeadline keeps only <mark>…</mark> tags and strips everything
// else. The server already builds the headline with StartSel/StopSel
// pinned to <mark>; we belt-and-suspenders escape any other HTML
// from a malicious tsvector input. This is a fixed allowlist so
// dangerouslySetInnerHTML stays safe.
function sanitiseHeadline(html: string): string {
  // Escape the entire string, then re-allow <mark> / </mark> tokens.
  const escaped = html
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;");
  return escaped
    .replace(/&lt;mark&gt;/g, "<mark>")
    .replace(/&lt;\/mark&gt;/g, "</mark>");
}

// CostBadge renders what a search hit ACTUALLY cost, which is the TOTAL of a page's two
// independent costs — not either half.
//
// A page carries `ai_cost_usd` (the cost of the Track ISSUES linked to it) and
// `own_ai_cost_usd` (the cost of AI operations performed ON the document); `total_ai_cost_usd`
// is their sum, derived server-side. This row used to read `r.ai_cost_usd` — the Track half —
// for BOTH the gate and the number, which was measured on this component as:
//
//   own 12.34 / track 0     -> NO BADGE, byte-identical to a genuinely free document
//   own 12.34 / track 1.50  -> "$1.50", the SMALLER half, off by $12.34
//
// Two rows sit side by side in one list, so that is a wrong ORDERING between two documents on
// screen at once, not just a wrong absolute number.
//
// ⚠ THREE STATES, NOT TWO, AND THE THIRD IS WHY THIS IS A COMPONENT AND NOT AN EXPRESSION.
// `internal/search/handler.go` emits the costs as `*float64` with `omitempty`: a SEMANTIC-ONLY
// hit is built from a vector match with NO `pages` row read, so all three are absent. That is
// "not reported", which is a different fact from "measured and zero" — the pointers on the
// wire exist to keep them apart. `?? 0` here would render "$0.00" for a document nobody
// looked at: a fabricated zero, and the exact failure a renderer written by analogy to
// PageView produces. Absent renders as an em-dash marker instead.
//
// ⚠ NO FALLBACK TO `ai_cost_usd` WHEN THE TOTAL IS ABSENT, DELIBERATELY. The server sets all
// three or none (merge()), so a total-less-but-track-ful row is a shape it never produces —
// and a fallback to the Track half is precisely the defect above, reintroduced in a branch no
// test could reach. Absent total ⇒ not reported, one rule. (The page screen DOES degrade that
// way, because `api/client.ts` serves page GETs back out of IndexedDB and a record cached
// before 0018 really does have that shape. Search is NOT cached — readCached/writeCache match
// only `/v1/spaces/...` paths — so inheriting that fallback here would be inheriting evidence
// gathered for a different surface.)
function CostBadge({ r }: { r: SearchResult }) {
  const total = r.total_ai_cost_usd;
  if (total === undefined || total === null) {
    return (
      <span
        data-testid="search-cost-unknown"
        title="AI cost not reported for this match — it was found by meaning, and its page row was not read"
        aria-label="AI cost not reported"
        className="text-muted"
      >
        —
      </span>
    );
  }
  if (total <= 0) return null;
  const own = r.own_ai_cost_usd;
  const track = r.ai_cost_usd;
  // The number claims to be a sum. The breakdown makes that claim checkable on the row itself
  // rather than something the reader has to take on trust.
  const title =
    own !== undefined && own !== null && track !== undefined && track !== null
      ? `AI cost $${total.toFixed(2)} = $${own.toFixed(2)} writing this document + $${track.toFixed(2)} linked Track issues`
      : `AI cost $${total.toFixed(2)}`;
  return (
    <span data-testid="search-cost" title={title} className="flex items-center gap-0.5 text-accent">
      <Sparkles size={9} />${total.toFixed(2)}
    </span>
  );
}

export function SearchModal({ workspaceId, open, onClose, onOpenPage }: SearchModalProps) {
  const { query, setQuery, debounced, data, isLoading } = useSearch(workspaceId);
  const [selected, setSelected] = useState(0);
  const inputRef = useRef<HTMLInputElement | null>(null);
  const [recent, setRecent] = useState<RecentPage[]>([]);

  // Recompute recents when the modal opens so the list reflects any
  // navigation that happened since last render.
  useEffect(() => {
    if (open) {
      setRecent(getRecentPages());
      setQuery("");
      setSelected(0);
      // Defer focus to next paint so the input is actually mounted.
      setTimeout(() => inputRef.current?.focus(), 0);
    }
  }, [open, setQuery]);

  const results: SearchResult[] = data?.results ?? [];
  const lensFooter = useMemo(
    () => results.some((r) => r.source === "semantic" || r.source === "both"),
    [results],
  );

  // Keyboard navigation. Down/Up move the highlight, Enter opens.
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.preventDefault();
        onClose();
        return;
      }
      const list = debounced.length >= 2 ? results : recent;
      if (e.key === "ArrowDown") {
        e.preventDefault();
        setSelected((i) => Math.min(i + 1, Math.max(0, list.length - 1)));
      } else if (e.key === "ArrowUp") {
        e.preventDefault();
        setSelected((i) => Math.max(i - 1, 0));
      } else if (e.key === "Enter") {
        e.preventDefault();
        if (list.length === 0) return;
        const r = list[selected];
        const isSearch = "page_id" in r && "headline" in (r as SearchResult);
        if (isSearch) {
          openResult(r as SearchResult);
        } else {
          openRecent(r as RecentPage);
        }
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [open, results, recent, selected, debounced.length, onClose]);

  const openResult = (r: SearchResult) => {
    pushRecentPage({
      page_id: r.page_id,
      page_title: r.page_title,
      space_name: r.space_name,
      url: r.url,
    });
    onOpenPage({
      pageID: r.page_id,
      spaceID: spaceIDFromURL(r.url),
      title: r.page_title,
      spaceName: r.space_name,
      url: r.url,
    });
    onClose();
  };

  const openRecent = (r: RecentPage) => {
    onOpenPage({
      pageID: r.page_id,
      spaceID: spaceIDFromURL(r.url),
      title: r.page_title,
      spaceName: r.space_name,
      url: r.url,
    });
    onClose();
  };

  if (!open) return null;

  const showRecents = debounced.length < 2;

  return (
    <div
      className="fixed inset-0 z-40 flex items-start justify-center bg-black/40 pt-24"
      onClick={onClose}
    >
      <div
        className="w-full max-w-2xl rounded-lg border border-border bg-surface shadow-2xl"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-center gap-2 border-b border-border px-3 py-2">
          <Search size={14} className="text-muted" />
          <input
            ref={inputRef}
            value={query}
            onChange={(e) => {
              setQuery(e.target.value);
              setSelected(0);
            }}
            placeholder="Search pages…"
            className="flex-1 bg-transparent text-sm placeholder:text-muted focus:outline-none"
          />
          <kbd className="rounded border border-border bg-bg px-1.5 py-0.5 text-[10px] text-muted">
            esc
          </kbd>
        </div>

        <div className="max-h-96 overflow-y-auto">
          {showRecents ? (
            recent.length === 0 ? (
              <div className="px-3 py-4 text-xs text-muted">
                Start typing to search. Recently opened pages will appear here.
              </div>
            ) : (
              <div>
                <div className="px-3 pt-2 text-[10px] uppercase tracking-wider text-muted">
                  Recent
                </div>
                {recent.map((r, i) => (
                  <button
                    key={r.page_id}
                    onClick={() => openRecent(r)}
                    onMouseEnter={() => setSelected(i)}
                    className={`flex w-full items-center gap-2 px-3 py-2 text-left text-xs ${
                      i === selected ? "bg-bg" : "hover:bg-bg/60"
                    }`}
                  >
                    <Clock size={10} className="text-muted" />
                    <span className="flex-1 truncate">
                      <span className="text-muted">{r.space_name}</span>
                      <span className="mx-1 text-muted">·</span>
                      <span className="text-text">{r.page_title}</span>
                    </span>
                  </button>
                ))}
              </div>
            )
          ) : isLoading ? (
            <div className="px-3 py-4 text-xs text-muted">Searching…</div>
          ) : results.length === 0 ? (
            <div className="px-3 py-6 text-center text-xs text-muted">
              <div>No results for "{debounced}".</div>
              <div className="mt-1">Try fewer words or different terms.</div>
            </div>
          ) : (
            results.map((r, i) => (
              <button
                key={r.page_id}
                onClick={() => openResult(r)}
                onMouseEnter={() => setSelected(i)}
                className={`block w-full px-3 py-2 text-left text-xs ${
                  i === selected ? "bg-bg" : "hover:bg-bg/60"
                }`}
              >
                <div className="flex items-center gap-2">
                  {r.source === "semantic" ? (
                    <Brain size={11} className="text-accent" />
                  ) : (
                    <Search size={11} className="text-muted" />
                  )}
                  <span className="text-muted">{r.space_name}</span>
                  <span className="text-muted">·</span>
                  <span className="flex-1 truncate text-text">{r.page_title}</span>
                  <CostBadge r={r} />
                </div>
                <div
                  className="mt-1 line-clamp-2 text-muted [&_mark]:bg-accent/30 [&_mark]:text-text"
                  dangerouslySetInnerHTML={{
                    __html: sanitiseHeadline(r.headline || ""),
                  }}
                />
              </button>
            ))
          )}
        </div>

        <footer className="flex items-center justify-between border-t border-border px-3 py-1.5 text-[10px] text-muted">
          <span>
            <kbd className="rounded border border-border bg-bg px-1">↑↓</kbd>{" "}
            navigate ·{" "}
            <kbd className="rounded border border-border bg-bg px-1">↵</kbd> open
          </span>
          {lensFooter ? (
            <span>
              <Brain size={9} className="inline -translate-y-px" /> Semantic search
              powered by Talyvor Lens
            </span>
          ) : null}
        </footer>
      </div>
    </div>
  );
}

// spaceIDFromURL pulls the space slug out of /spaces/:spaceID/pages/:pageID.
// The server-built URLs are deterministic; if the shape ever changes
// this returns undefined and the caller routes to a bare page view.
function spaceIDFromURL(url: string): string | undefined {
  const m = url.match(/^\/spaces\/([^/]+)\/pages\//);
  return m?.[1];
}

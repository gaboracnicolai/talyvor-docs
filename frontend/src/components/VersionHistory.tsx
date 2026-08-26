import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { History, RotateCcw } from "lucide-react";
import { pagesApi } from "~/api/pages";

export interface DiffLine {
  type: "same" | "add" | "remove";
  text: string;
}

// lineDiff is a PURE Longest-Common-Subsequence line diff — the testable core of the diff view.
// Lines present in both are "same"; only in `from` are "remove"; only in `to` are "add".
export function lineDiff(from: string, to: string): DiffLine[] {
  const a = from.split("\n");
  const b = to.split("\n");
  const n = a.length;
  const m = b.length;
  // LCS length table.
  const dp: number[][] = Array.from({ length: n + 1 }, () => new Array(m + 1).fill(0));
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      dp[i][j] = a[i] === b[j] ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1]);
    }
  }
  const out: DiffLine[] = [];
  let i = 0;
  let j = 0;
  while (i < n && j < m) {
    if (a[i] === b[j]) {
      out.push({ type: "same", text: a[i] });
      i++;
      j++;
    } else if (dp[i + 1][j] >= dp[i][j + 1]) {
      out.push({ type: "remove", text: a[i] });
      i++;
    } else {
      out.push({ type: "add", text: b[j] });
      j++;
    }
  }
  while (i < n) out.push({ type: "remove", text: a[i++] });
  while (j < m) out.push({ type: "add", text: b[j++] });
  return out;
}

// prettyContent renders stored ProseMirror JSON as indented lines so the diff is legible;
// falls back to the raw string if it isn't JSON.
function prettyContent(content: string): string {
  try {
    return JSON.stringify(JSON.parse(content), null, 2);
  } catch {
    return content;
  }
}

// versionText is the text a snapshot is DIFFED as, and it is the whole snapshot rather than half
// of it. A `page_versions` row has exactly two content-bearing columns — title and content — and
// RestoreVersion writes BOTH back onto the live page, so a version IS the pair. Diffing content
// alone made a title-only save (which appends its own version row: see
// internal/page/versioning_title_only_save_test.go, "A RENAME IS A SAVE") render as an all-`same`
// panel: the screen answered "nothing changed" for the revision that renamed the document.
//
// The title is diffed as a leading line rather than shown in a separate strip, so a rename reads
// in the panel's existing +/- vocabulary and stays subject to the same LCS comparison — printing
// it outside the diff would report a rename on every ordinary body edit. The blank line keeps the
// title from anchoring against a body line that happens to match it.
function versionText(v: { title: string; content: string }): string {
  return `title: ${v.title}\n\n${prettyContent(v.content)}`;
}

interface VersionHistoryProps {
  spaceID: string;
  pageID: string;
  onRestored?: () => void;
}

// revisionCost renders what one revision's AI assistance cost.
//
// ⚠ A NON-ZERO COST MUST NEVER RENDER AS "$0.00". The rest of this SPA formats money with
// toFixed(2) because it prices whole documents and linked Track issues; a single revision routinely
// costs a fraction of a cent, and two decimals turn a real charge into a printed zero — the one
// rounding error a reader cannot detect, because "$0.00" and "free" are the same six characters.
// Small amounts widen to four decimals, and anything under that floor is reported as a bound rather
// than rounded away.
//
// ⚠ AND ZERO IS "—", NOT "$0.00". Zero here means no priced spend is ATTRIBUTED to this revision,
// which is not the same as the revision having been free: spend bound after the newest save belongs
// to a revision that does not exist yet, and bindings older than migration 0021 carry no revision at
// all. Printing $0.00 would state a fact about money the server never claimed.
export function revisionCost(usd: number): string {
  if (!(usd > 0)) return "—";
  if (usd < 0.0001) return "<$0.0001";
  if (usd < 0.01) return `$${usd.toFixed(4)}`;
  return `$${usd.toFixed(2)}`;
}

// VersionHistory lists the append-only version history and supports viewing a diff of any two
// versions and restoring one (a non-destructive restore — it writes a new current version).
export function VersionHistory({ spaceID, pageID, onRestored }: VersionHistoryProps) {
  const qc = useQueryClient();
  const [pick, setPick] = useState<number[]>([]);

  const versions = useQuery({
    queryKey: ["page-versions", pageID],
    queryFn: () => pagesApi.versions(spaceID, pageID),
  });

  // ⚠ HOW MUCH OF THE PAGE'S SPEND THESE ROWS DO NOT ACCOUNT FOR. Summing `ai_cost_usd` down the
  // list gives Attributed and nothing else, and until this read existed a reader had no way to
  // tell that from the page's own total — which `pages/PageView.tsx` renders directly BENEATH
  // this component. Two numbers on one screen that did not add up, with nothing saying why.
  //
  // ⚠ A SEPARATE QUERY RATHER THAN A FIELD ON THE LIST, deliberately: the split is a fact about
  // the PAGE, not about any revision, and hanging it off every row would invite a reader to add
  // it up. It is also allowed to fail on its own — see the render, where a failed split withdraws
  // the CLAIM without withdrawing the rows.
  const split = useQuery({
    queryKey: ["page-version-cost", pageID],
    queryFn: () => pagesApi.versionCostSplit(spaceID, pageID),
  });

  const [from, to] = [Math.min(...pick), Math.max(...pick)];
  const diff = useQuery({
    queryKey: ["page-diff", pageID, from, to],
    queryFn: () => pagesApi.diffVersions(spaceID, pageID, from, to),
    enabled: pick.length === 2,
  });

  const restore = useMutation({
    mutationFn: (version: number) => pagesApi.restore(spaceID, pageID, version),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["page", spaceID, pageID] });
      qc.invalidateQueries({ queryKey: ["page-versions", pageID] });
      onRestored?.();
    },
  });

  const diffLines = useMemo(() => {
    if (!diff.data) return [];
    return lineDiff(versionText(diff.data.from), versionText(diff.data.to));
  }, [diff.data]);

  const toggle = (v: number) =>
    setPick((p) => (p.includes(v) ? p.filter((x) => x !== v) : [...p, v].slice(-2)));

  const list = versions.data ?? [];
  return (
    <div className="flex flex-col gap-2 text-xs">
      <div className="flex items-center gap-1 font-medium text-fg-muted">
        <History size={13} /> Version history
      </div>
      {versions.isLoading && <div className="text-fg-muted">Loading…</div>}
      <ul className="flex flex-col gap-0.5">
        {list.map((v) => (
          <li
            key={v.id}
            className={`flex items-center justify-between rounded px-1.5 py-1 ${
              pick.includes(v.version) ? "bg-accent/10" : "hover:bg-surface-2"
            }`}
          >
            <button className="flex-1 text-left" onClick={() => toggle(v.version)}>
              <span className="font-mono">v{v.version}</span>{" "}
              <span className="text-fg-muted">
                {v.created_by || "—"} · {new Date(v.created_at).toLocaleString()}
              </span>{" "}
              <span
                className="font-mono text-fg-muted"
                title={v.ai_cost_usd > 0 ? "AI spend attributed to this revision" : "No AI spend is attributed to this revision"}
              >
                {revisionCost(v.ai_cost_usd)}
              </span>
            </button>
            <button
              className="flex items-center gap-0.5 text-fg-muted underline hover:text-fg disabled:opacity-50"
              onClick={() => restore.mutate(v.version)}
              disabled={restore.isPending}
              title="Restore this version (non-destructive — writes a new current version)"
            >
              <RotateCcw size={11} /> Restore
            </button>
          </li>
        ))}
      </ul>
      {/* ⚠ RENDERED ONLY WHEN THERE IS SOMETHING UNACCOUNTED FOR, and the silence is the design.
          A permanent "nothing is missing" strip is noise on every page that is fine, and it
          teaches a reader to skip the one region that carries the warning in the case that is
          not. `unshown > 0` is the whole condition — a page whose rows add up renders nothing.

          ⚠ AND A FAILED SPLIT RENDERS NOTHING EITHER, WHICH IS NOT THE SAME THING AND IS THE
          LESSER OF TWO WRONG ANSWERS. `split.data` being absent means the read failed, not that
          the rows are complete; the honest alternatives were to say so or to stay quiet, and
          what must never happen is the opposite — a claim of completeness the screen cannot
          support. The ROWS are unaffected either way: a failed side-read does not withdraw a
          feature that is still correct. */}
      {split.data && split.data.pending_usd + split.data.unattributable_usd > 0 && (
        <div
          data-testid="version-cost-reconcile"
          className="rounded border border-border px-1.5 py-1 text-fg-muted"
        >
          {/* ⚠ THE TWO BUCKETS ARE NAMED SEPARATELY BECAUSE ONLY ONE OF THEM WILL EVER LAND.
              Pending money appears on its revision the moment the next save creates it;
              pre-0021 money never will, because the revision was not recorded when it was spent.
              A single "unshown" figure would tell a reader that all of it is on its way. */}
          <div>
            {/* ⚠ EVERY FIGURE HERE GOES THROUGH `revisionCost`, INCLUDING THE PAGE TOTAL, and
                the first draft of this strip did not — it printed the total with `toFixed(2)`
                like the rest of the SPA. That is the defect this component's own header was
                written about, reintroduced two lines below the fix: a page whose whole AI spend
                is a fraction of a cent would have read "these rows show $0.0042 of $0.00", which
                is both absurd and, in the direction that matters, a claim that the page spent
                nothing. The AI-cost panel in pages/PageView.tsx still rounds this way and is
                left alone here — it is not this component's to change on the way past. */}
            These rows show {revisionCost(split.data.attributed_usd)} of{" "}
            <span className="font-mono">{revisionCost(split.data.page_total_usd)}</span> this page
            has spent on AI.
          </div>
          {split.data.pending_usd > 0 && (
            <div>
              <span className="font-mono">{revisionCost(split.data.pending_usd)}</span> was spent
              after the newest save, so it belongs to a revision that does not exist yet — it lands
              on the next save.
            </div>
          )}
          {split.data.unattributable_usd > 0 && (
            <div>
              <span className="font-mono">{revisionCost(split.data.unattributable_usd)}</span> was
              spent before this page recorded which revision it was for, so it can never be shown
              against one. The revision was never recorded, not lost.
            </div>
          )}
        </div>
      )}
      {pick.length === 2 && (
        <div className="rounded border border-border">
          <div className="border-b border-border px-1.5 py-1 text-fg-muted">
            Diff v{from} → v{to}
          </div>
          <pre className="max-h-64 overflow-auto p-1.5 font-mono text-[10px] leading-tight">
            {diff.isLoading && "Loading diff…"}
            {diffLines.map((l, idx) => (
              <div
                key={idx}
                className={
                  l.type === "add"
                    ? "bg-callout-success/15 text-callout-success"
                    : l.type === "remove"
                      ? "bg-callout-error/15 text-callout-error"
                      : "text-fg-muted"
                }
              >
                {l.type === "add" ? "+ " : l.type === "remove" ? "- " : "  "}
                {l.text}
              </div>
            ))}
          </pre>
        </div>
      )}
    </div>
  );
}

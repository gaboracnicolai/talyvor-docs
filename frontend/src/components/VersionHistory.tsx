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

interface VersionHistoryProps {
  spaceID: string;
  pageID: string;
  onRestored?: () => void;
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
    return lineDiff(prettyContent(diff.data.from.content), prettyContent(diff.data.to.content));
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

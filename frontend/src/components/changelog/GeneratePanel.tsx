import { useState } from "react";
import { Sparkles } from "lucide-react";

interface PanelProps {
  onGenerate: (input: { version: string; issueIDs: string[] }) => void;
  busy?: boolean;
}

// GeneratePanel collects a version + an issue-ID list and hands
// them to the parent, which calls the changelog generate endpoint.
// We deliberately accept comma-separated free text rather than
// building a Track-aware picker here — the Phase-9 polish wires the
// IssueSearchDialog as the multi-select.
export function GeneratePanel({ onGenerate, busy }: PanelProps) {
  const [version, setVersion] = useState("");
  const [ids, setIds] = useState("");
  return (
    <div className="rounded border border-border bg-surface p-3 text-xs">
      <header className="mb-2 flex items-center gap-1 text-sm font-semibold">
        <Sparkles size={12} className="text-accent" /> Generate from issues
      </header>
      <div className="space-y-1">
        <input
          value={version}
          onChange={(e) => setVersion(e.target.value)}
          placeholder="Version (e.g. v2.1.0)"
          className="w-full rounded border border-border bg-bg px-1 py-1 text-xs"
        />
        <input
          value={ids}
          onChange={(e) => setIds(e.target.value)}
          placeholder="Issue IDs (comma-separated)"
          className="w-full rounded border border-border bg-bg px-1 py-1 text-xs"
        />
        <button
          onClick={() =>
            onGenerate({
              version: version.trim(),
              issueIDs: ids
                .split(",")
                .map((s) => s.trim())
                .filter(Boolean),
            })
          }
          disabled={busy || !version.trim() || !ids.trim()}
          className="w-full rounded bg-accent px-2 py-1 text-[10px] text-bg hover:opacity-90 disabled:opacity-40"
        >
          {busy ? "Generating…" : "Generate"}
        </button>
      </div>
    </div>
  );
}

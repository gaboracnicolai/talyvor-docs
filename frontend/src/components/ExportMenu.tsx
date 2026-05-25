import { useEffect, useRef, useState } from "react";
import { Download, Loader2 } from "lucide-react";
import { useExport, type ExportFormat } from "~/hooks/useExport";

interface ExportMenuProps {
  spaceID: string;
  pageID: string;
}

const FORMATS: { value: ExportFormat; emoji: string; label: string }[] = [
  { value: "pdf", emoji: "📄", label: "PDF" },
  { value: "docx", emoji: "📝", label: "Word (.docx)" },
  { value: "html", emoji: "🌐", label: "HTML" },
  { value: "markdown", emoji: "⬇️", label: "Markdown" },
];

// ExportMenu is the page-header dropdown for downloading the
// current page as PDF / Word / HTML / Markdown. The user picks a
// format and toggles the TOC / children options; the actual
// download fires through useExport.
export function ExportMenu({ spaceID, pageID }: ExportMenuProps) {
  const [open, setOpen] = useState(false);
  const [format, setFormat] = useState<ExportFormat>("pdf");
  const [includeTOC, setIncludeTOC] = useState(false);
  const [includeChildren, setIncludeChildren] = useState(false);
  const ref = useRef<HTMLDivElement | null>(null);
  const { downloadExport, busy, error } = useExport();

  // Close the dropdown on outside click. We deliberately don't add
  // a global Escape handler — the dropdown is small + low-stakes.
  useEffect(() => {
    if (!open) return;
    const onClick = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) {
        setOpen(false);
      }
    };
    document.addEventListener("mousedown", onClick);
    return () => document.removeEventListener("mousedown", onClick);
  }, [open]);

  return (
    <div className="relative" ref={ref}>
      <button
        onClick={() => setOpen((v) => !v)}
        title="Export this page"
        className="inline-flex items-center gap-1 rounded border border-border bg-bg px-1.5 py-0.5 text-[10px] text-muted hover:border-accent hover:text-text"
      >
        <Download size={10} /> Export
      </button>
      {open ? (
        <div className="absolute right-0 top-7 z-30 w-56 rounded-md border border-border bg-surface p-2 shadow-lg">
          <div className="mb-1 text-[10px] uppercase tracking-wider text-muted">
            Format
          </div>
          <div className="mb-2 space-y-0.5">
            {FORMATS.map((f) => (
              <label
                key={f.value}
                className={`flex items-center gap-2 rounded px-1.5 py-0.5 text-xs ${
                  format === f.value ? "bg-bg text-text" : "text-muted hover:bg-bg/60"
                }`}
              >
                <input
                  type="radio"
                  name="export-format"
                  checked={format === f.value}
                  onChange={() => setFormat(f.value)}
                  className="accent-accent"
                />
                <span>{f.emoji}</span>
                <span>{f.label}</span>
              </label>
            ))}
          </div>
          <div className="mb-2 space-y-0.5">
            <label className="flex items-center gap-1 text-xs text-muted">
              <input
                type="checkbox"
                checked={includeTOC}
                onChange={(e) => setIncludeTOC(e.target.checked)}
                className="accent-accent"
              />
              Include table of contents
            </label>
            <label className="flex items-center gap-1 text-xs text-muted">
              <input
                type="checkbox"
                checked={includeChildren}
                onChange={(e) => setIncludeChildren(e.target.checked)}
                className="accent-accent"
              />
              Include child pages
            </label>
          </div>
          <button
            onClick={() => {
              void downloadExport(spaceID, pageID, format, {
                includeTOC,
                includeChildren,
              });
            }}
            disabled={busy}
            className="flex w-full items-center justify-center gap-1 rounded bg-accent px-2 py-1 text-xs text-bg hover:opacity-90 disabled:opacity-40"
          >
            {busy ? <Loader2 size={11} className="animate-spin" /> : <Download size={11} />}
            {busy ? "Exporting…" : "Export"}
          </button>
          {error ? (
            <div className="mt-1 text-[10px] text-callout-error">{error}</div>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}

import { useState } from "react";

export type ExportFormat = "pdf" | "docx" | "html" | "markdown";

export interface ExportOptions {
  includeTOC?: boolean;
  includeChildren?: boolean;
  watermark?: string;
}

// useExport returns a downloadExport function that hits the server
// export endpoint with the requested options, then triggers a
// browser download from the blob response. We deliberately avoid
// the apiRequest helper because it JSON-parses the response — we
// need the raw bytes plus the Content-Disposition filename.
export function useExport() {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const downloadExport = async (
    spaceID: string,
    pageID: string,
    format: ExportFormat,
    opts: ExportOptions = {},
  ) => {
    setBusy(true);
    setError(null);
    try {
      const params = new URLSearchParams({ format });
      if (opts.includeTOC) params.set("include_toc", "true");
      if (opts.includeChildren) params.set("include_children", "true");
      if (opts.watermark) params.set("watermark", opts.watermark);
      const token = localStorage.getItem("docs_api_key") ?? "";
      const headers: Record<string, string> = {};
      if (token) headers.Authorization = `Bearer ${token}`;

      const base = import.meta.env.VITE_API_URL ?? "";
      const res = await fetch(
        `${base}/v1/spaces/${spaceID}/pages/${pageID}/export?${params.toString()}`,
        { headers },
      );
      if (!res.ok) {
        if (res.status === 413) {
          throw new Error("Export exceeds 50 MB. Try a smaller scope.");
        }
        throw new Error("Export failed.");
      }
      // Derive filename from the Content-Disposition header so it
      // matches the server-side slug. Falls back to a sane default
      // when the header is missing.
      const disposition = res.headers.get("Content-Disposition") ?? "";
      const match = disposition.match(/filename="([^"]+)"/);
      const filename = match?.[1] ?? `export.${extFor(format)}`;

      const blob = await res.blob();
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = filename;
      document.body.appendChild(a);
      a.click();
      a.remove();
      URL.revokeObjectURL(url);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Export failed.");
    } finally {
      setBusy(false);
    }
  };

  return { downloadExport, busy, error };
}

function extFor(f: ExportFormat): string {
  if (f === "markdown") return "md";
  return f;
}

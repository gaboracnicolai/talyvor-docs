import { CheckSquare, Link as LinkIcon } from "lucide-react";
import type { ColumnDef, Row } from "~/api/database";

interface GalleryProps {
  schema: ColumnDef[];
  rows: Row[];
}

// GalleryView renders each row as a card with a primary value and
// up to three properties. We use the first text/URL/checkbox column
// as the headline so the grid reads cleanly without per-row
// configuration.
export function GalleryView({ schema, rows }: GalleryProps) {
  const headline = schema.find(
    (c) => c.type === "text" || c.type === "url" || c.type === "checkbox",
  ) ?? schema[0];
  const properties = schema.filter((c) => c.id !== headline?.id).slice(0, 3);

  if (rows.length === 0) {
    return (
      <div className="rounded border border-dashed border-border p-4 text-center text-xs text-muted">
        No rows yet.
      </div>
    );
  }

  return (
    <div className="grid grid-cols-1 gap-2 sm:grid-cols-2 lg:grid-cols-3">
      {rows.map((r) => {
        const head = headline ? r.values[headline.id] : undefined;
        return (
          <div
            key={r.id}
            className="rounded border border-border bg-surface p-3 text-xs"
          >
            <div className="mb-1 flex items-center gap-1 text-sm font-semibold">
              {headline?.type === "checkbox" ? (
                <CheckSquare
                  size={11}
                  className={head ? "text-callout-success" : "text-muted"}
                />
              ) : headline?.type === "url" ? (
                <LinkIcon size={11} className="text-accent" />
              ) : null}
              {String(head ?? "(untitled)")}
            </div>
            <dl className="space-y-0.5 text-[10px] text-muted">
              {properties.map((c) => {
                const v = r.values[c.id];
                if (v === undefined || v === null || v === "") return null;
                return (
                  <div key={c.id} className="flex gap-1">
                    <dt className="text-[10px] uppercase tracking-wider">
                      {c.name}
                    </dt>
                    <dd className="text-text">
                      {Array.isArray(v) ? v.join(", ") : String(v)}
                    </dd>
                  </div>
                );
              })}
            </dl>
          </div>
        );
      })}
    </div>
  );
}

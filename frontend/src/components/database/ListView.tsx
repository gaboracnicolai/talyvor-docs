import type { ColumnDef, Row } from "~/api/database";

interface ListProps {
  schema: ColumnDef[];
  rows: Row[];
  onClickRow?: (rowID: string) => void;
}

// ListView shows each row as a single-line summary: title-ish first
// column, then up to three secondary fields. The compact format is
// what users reach for when the table feels too dense.
export function ListView({ schema, rows, onClickRow }: ListProps) {
  const title = schema.find((c) => c.type === "text") ?? schema[0];
  const secondary = schema.filter((c) => c.id !== title?.id).slice(0, 3);

  if (rows.length === 0) {
    return (
      <div className="rounded border border-dashed border-border p-4 text-center text-xs text-muted">
        No rows yet.
      </div>
    );
  }

  return (
    <ul className="divide-y divide-border rounded border border-border">
      {rows.map((r) => (
        <li
          key={r.id}
          onClick={() => onClickRow?.(r.id)}
          className="flex items-center justify-between gap-2 px-3 py-1.5 text-xs hover:bg-bg/60"
        >
          <span className="font-medium">
            {title ? String(r.values[title.id] ?? "(untitled)") : r.id}
          </span>
          <span className="flex flex-wrap items-center gap-2 text-muted">
            {secondary.map((c) => {
              const v = r.values[c.id];
              if (v === undefined || v === null || v === "") return null;
              return (
                <span key={c.id} className="rounded bg-bg px-1.5 py-px text-[10px]">
                  {c.name}: {Array.isArray(v) ? v.join(", ") : String(v)}
                </span>
              );
            })}
          </span>
        </li>
      ))}
    </ul>
  );
}

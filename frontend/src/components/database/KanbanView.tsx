import { useMemo } from "react";
import { Plus } from "lucide-react";
import type { ColumnDef, Row } from "~/api/database";

interface KanbanProps {
  schema: ColumnDef[];
  rows: Row[];
  groupBy: string;
  onUpdateRow: (rowID: string, values: Record<string, unknown>) => void;
  onAddRow: (column: string) => void;
}

// KanbanView buckets rows by the GroupBy column (must be a select).
// Drag uses native HTML5 — we don't need react-dnd for a single-
// page-load lifecycle and a bounded row count.
export function KanbanView({
  schema,
  rows,
  groupBy,
  onUpdateRow,
  onAddRow,
}: KanbanProps) {
  const groupCol = schema.find((c) => c.id === groupBy);
  const columns = useMemo(() => {
    if (!groupCol || (groupCol.type !== "select" && groupCol.type !== "multi_select")) {
      return ["(no group)"];
    }
    return groupCol.options ?? [];
  }, [groupCol]);

  if (!groupCol) {
    return (
      <div className="rounded border border-dashed border-border p-4 text-center text-xs text-muted">
        Choose a select column to group by.
      </div>
    );
  }

  const titleCol = schema.find((c) => c.type === "text") ?? schema[0];

  return (
    <div className="flex gap-2 overflow-x-auto pb-2">
      {columns.map((bucket) => {
        const bucketRows = rows.filter((r) => {
          const v = r.values[groupBy];
          if (Array.isArray(v)) return v.includes(bucket);
          return v === bucket;
        });
        return (
          <div
            key={bucket}
            onDragOver={(e) => e.preventDefault()}
            onDrop={(e) => {
              const rowID = e.dataTransfer.getData("text/row-id");
              if (rowID) onUpdateRow(rowID, { [groupBy]: bucket });
            }}
            className="flex w-56 shrink-0 flex-col rounded border border-border bg-surface p-1"
          >
            <header className="flex items-center justify-between px-1 pb-1 text-[10px] uppercase tracking-wider text-muted">
              <span>{bucket || "—"}</span>
              <span>{bucketRows.length}</span>
            </header>
            <div className="space-y-1">
              {bucketRows.map((r) => (
                <div
                  key={r.id}
                  draggable
                  onDragStart={(e) => e.dataTransfer.setData("text/row-id", r.id)}
                  className="cursor-grab rounded border border-border bg-bg px-2 py-1 text-xs"
                >
                  {titleCol ? String(r.values[titleCol.id] ?? "(untitled)") : r.id.slice(0, 6)}
                </div>
              ))}
            </div>
            <button
              onClick={() => onAddRow(bucket)}
              className="mt-1 flex items-center justify-center gap-1 rounded border border-dashed border-border py-1 text-[10px] text-muted hover:border-accent"
            >
              <Plus size={10} /> Add
            </button>
          </div>
        );
      })}
    </div>
  );
}
